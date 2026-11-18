// Modified for the cryotx/posthorn distribution.

package storage

import (
	"database/sql"
	"fmt"
	"time"
)

// Idempotency reservation states.
const (
	// IdemPending means a request holds the key and is in flight.
	IdemPending = "pending"

	// IdemDone means a response is recorded and replayable.
	IdemDone = "done"
)

// PendingTTL bounds how long a reservation may sit in "pending" before
// another request may take it over.
//
// A process that dies mid-send leaves a pending row behind. Without a
// takeover window that key would be unusable until its full 24h TTL,
// which turns one crash into a day of refused retries for that caller.
// Two minutes is comfortably longer than the 10s request timeout plus
// every retry the FR19-22 policy can add, so a live request is never
// stolen from.
const PendingTTL = 2 * time.Minute

// IdemState is the outcome of a reservation attempt.
type IdemState int

const (
	// IdemReserved means this caller now owns the key and must send.
	IdemReserved IdemState = iota

	// IdemReplay means a completed response exists for this exact
	// payload; return it and send nothing.
	IdemReplay

	// IdemInFlight means another request holds the key right now.
	IdemInFlight

	// IdemConflict means the key exists with a DIFFERENT payload —
	// the caller reused an idempotency key for a different message.
	IdemConflict
)

// IdemRecord is a stored reservation.
type IdemRecord struct {
	State       IdemState
	Status      int
	ContentType string
	Body        []byte
	MessageID   string
}

// ReserveIdempotent atomically claims (key, scope) for the caller, or
// reports why it could not (FR113).
//
// This is the whole point of doing idempotency in the database rather
// than in a map: two concurrent requests carrying the same key must not
// both discover "no entry" and both send. The INSERT ... ON CONFLICT DO
// NOTHING inside a transaction is the atom — exactly one of them
// affects a row, and the loser reads back what the winner wrote.
//
// fingerprint identifies the request payload. A second request with the
// same key and a different fingerprint is a caller bug (an idempotency
// key reused for a different message) and is reported as a conflict
// rather than silently replaying the wrong response.
func (s *Store) ReserveIdempotent(key, scope, fingerprint, messageID string, now time.Time, ttl time.Duration) (IdemRecord, error) {
	var out IdemRecord
	err := s.inTx(func(tx *sql.Tx) error {
		// Clear anything that has aged out: an expired entry, or a
		// reservation whose owner died.
		if _, err := tx.Exec(`
			DELETE FROM idempotency
			WHERE key = ? AND scope = ?
			  AND (expires_at <= ?
			       OR (state = ? AND created_at <= ?))`,
			key, scope, now.Unix(), IdemPending, now.Add(-PendingTTL).Unix()); err != nil {
			return fmt.Errorf("reap idempotency: %w", err)
		}

		// The message id is stored AT RESERVE, not only on completion.
		// A process that dies mid-send leaves the reservation pending,
		// and recovery needs the link back to the submission to answer
		// the caller's retry correctly instead of stalling it for the
		// whole pending window (ADR-45).
		res, err := tx.Exec(`
			INSERT INTO idempotency (key, scope, fingerprint, state, message_id, created_at, expires_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(key, scope) DO NOTHING`,
			key, scope, fingerprint, IdemPending, messageID, now.Unix(), now.Add(ttl).Unix())
		if err != nil {
			return fmt.Errorf("reserve idempotency: %w", err)
		}
		if n, _ := res.RowsAffected(); n == 1 {
			out = IdemRecord{State: IdemReserved}
			return nil
		}

		// Someone else owns it. Read back what they have.
		var state, storedFP, contentType, messageID string
		var status int
		var body []byte
		row := tx.QueryRow(`
			SELECT state, fingerprint, status, content_type, response, message_id
			FROM idempotency WHERE key = ? AND scope = ?`, key, scope)
		if err := row.Scan(&state, &storedFP, &status, &contentType, &body, &messageID); err != nil {
			if err == sql.ErrNoRows {
				// Raced with the reaper; the caller retries and wins.
				out = IdemRecord{State: IdemInFlight}
				return nil
			}
			return fmt.Errorf("read idempotency: %w", err)
		}
		if storedFP != fingerprint {
			out = IdemRecord{State: IdemConflict, MessageID: messageID}
			return nil
		}
		if state == IdemPending {
			out = IdemRecord{State: IdemInFlight, MessageID: messageID}
			return nil
		}
		out = IdemRecord{
			State: IdemReplay, Status: status, ContentType: contentType,
			Body: body, MessageID: messageID,
		}
		return nil
	})
	return out, err
}

// CompleteIdempotent records the response for a reservation this caller
// owns, making it replayable.
func (s *Store) CompleteIdempotent(key, scope, messageID string, status int, contentType string, body []byte, now time.Time, ttl time.Duration) error {
	_, err := s.db.Exec(`
		UPDATE idempotency
		SET state = ?, status = ?, content_type = ?, response = ?,
		    message_id = ?, expires_at = ?
		WHERE key = ? AND scope = ?`,
		IdemDone, status, contentType, body, messageID, now.Add(ttl).Unix(), key, scope)
	if err != nil {
		return fmt.Errorf("storage: complete idempotent: %w", err)
	}
	return nil
}

// ReleaseIdempotent drops a reservation without recording a response,
// so a failed send does not hold the key hostage for its full TTL.
func (s *Store) ReleaseIdempotent(key, scope string) error {
	_, err := s.db.Exec(
		`DELETE FROM idempotency WHERE key = ? AND scope = ? AND state = ?`,
		key, scope, IdemPending)
	if err != nil {
		return fmt.Errorf("storage: release idempotent: %w", err)
	}
	return nil
}

// RecoverPendingReservations settles idempotency reservations orphaned
// by a crash (ADR-45).
//
// A process killed mid-send leaves a `pending` row whose owner is gone.
// Without this, the caller's retry gets 409 in_flight for the whole
// PendingTTL — a two-minute dead zone in which a user asking for a new
// verification code is told "already in flight" while the queue is in
// fact about to deliver the first one.
//
// Recovery has better information than the dead request did: the
// submission has just been moved into the retry queue, so the honest
// answer to a retry is the 202 "queued" the original request would
// have returned. Writing that closes the reservation and makes the
// retry a replay.
//
// Only reservations whose submission actually reached the queue are
// settled. One whose request died before the submission was persisted
// has nothing to promise, and correctly stays pending until its window
// expires and the key becomes reusable.
func (s *Store) RecoverPendingReservations(queuedBody func(messageID string) (int, string, []byte)) (int, error) {
	rows, err := s.db.Query(`
		SELECT i.key, i.scope, i.message_id
		FROM idempotency i
		JOIN submissions sub ON sub.id = i.message_id
		WHERE i.state = ? AND i.message_id != '' AND sub.status = ?`,
		IdemPending, StatusQueued)
	if err != nil {
		return 0, fmt.Errorf("storage: find orphaned reservations: %w", err)
	}
	type orphan struct{ key, scope, messageID string }
	var orphans []orphan
	for rows.Next() {
		var o orphan
		if err := rows.Scan(&o.key, &o.scope, &o.messageID); err != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("storage: scan orphaned reservation: %w", err)
		}
		orphans = append(orphans, o)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return 0, fmt.Errorf("storage: read orphaned reservations: %w", err)
	}
	_ = rows.Close()

	now := time.Now()
	for _, o := range orphans {
		status, contentType, body := queuedBody(o.messageID)
		if err := s.CompleteIdempotent(o.key, o.scope, o.messageID,
			status, contentType, body, now, 24*time.Hour); err != nil {
			return 0, err
		}
	}
	return len(orphans), nil
}

// GetIdempotent fetches a TTL-valid completed response. Pending
// reservations and expired rows are misses.
func (s *Store) GetIdempotent(key, scope string, now time.Time) (status int, contentType string, body []byte, ok bool, err error) {
	var state string
	var expiresAt int64
	row := s.db.QueryRow(
		`SELECT state, status, content_type, response, expires_at
		 FROM idempotency WHERE key = ? AND scope = ?`, key, scope)
	if err := row.Scan(&state, &status, &contentType, &body, &expiresAt); err != nil {
		if err == sql.ErrNoRows {
			return 0, "", nil, false, nil
		}
		return 0, "", nil, false, fmt.Errorf("storage: get idempotent: %w", err)
	}
	if state != IdemDone || now.Unix() >= expiresAt {
		return 0, "", nil, false, nil
	}
	return status, contentType, body, true, nil
}

// PutIdempotent stores a completed response directly. Retained for the
// v2.x in-flight-in-memory path (endpoint-scoped mode), which claims
// its key in a map rather than in the database.
func (s *Store) PutIdempotent(key, scope string, status int, contentType string, body []byte, expiresAt time.Time) error {
	_, err := s.db.Exec(`
		INSERT INTO idempotency (key, scope, state, status, content_type, response, created_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(key, scope) DO UPDATE SET
		  state = excluded.state, status = excluded.status,
		  content_type = excluded.content_type, response = excluded.response,
		  expires_at = excluded.expires_at`,
		// created_at is only read when reaping stale *pending* rows, and
		// this row is already done, so zero is correct rather than lazy.
		key, scope, IdemDone, status, contentType, body, 0, expiresAt.Unix())
	if err != nil {
		return fmt.Errorf("storage: put idempotent: %w", err)
	}
	return nil
}

// PruneIdempotency deletes expired rows (FR81 background cleanup).
func (s *Store) PruneIdempotency(now time.Time) (int64, error) {
	res, err := s.db.Exec(`DELETE FROM idempotency WHERE expires_at <= ?`, now.Unix())
	if err != nil {
		return 0, fmt.Errorf("storage: prune idempotency: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}
