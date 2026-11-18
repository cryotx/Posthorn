package storage

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Dead-letter inspection and replay (FR145).
//
// A "dead letter" is a submission that failed and is no longer queued:
// either it exhausted its attempts or a provider called it terminal.
// The distinction from a queued failure matters operationally — a
// queued row will be tried again on its own, a dead letter will not.

// ListDeadLetters returns failed submissions that no longer have a
// queue row, newest first.
func (s *Store) ListDeadLetters(limit int) ([]MessageSummary, error) {
	rows, err := s.db.Query(`
		SELECT id, project, endpoint, transport, from_addr, to_addrs, subject,
		       status, message_kind, delivery_state, transport_message_id,
		       last_error, created_at, sent_at, delivery_state_at, attachments
		FROM submissions
		WHERE status = ?
		  AND id NOT IN (SELECT submission_id FROM retry_queue)
		ORDER BY created_at DESC, id DESC
		LIMIT ?`, StatusFailed, clampLimit(limit))
	if err != nil {
		return nil, fmt.Errorf("storage: list dead letters: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return scanSummaries(rows)
}

// CountDeadLetters reports how many dead letters exist.
func (s *Store) CountDeadLetters() (int, error) {
	var n int
	err := s.db.QueryRow(`
		SELECT COUNT(*) FROM submissions
		WHERE status = ? AND id NOT IN (SELECT submission_id FROM retry_queue)`,
		StatusFailed).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("storage: count dead letters: %w", err)
	}
	return n, nil
}

// ErrNotReplayable is returned when a dead letter cannot be retried.
var ErrNotReplayable = errors.New("storage: submission cannot be replayed")

// RequeueDeadLetter puts a dead letter back on the retry queue with a
// fresh attempt count, due immediately.
//
// Refused for a row with no body: an endpoint with
// log_failed_submissions = false records metadata only, and replaying
// that would send a real recipient an EMPTY message. Failing loudly is
// the only honest option — the content is gone and cannot be
// reconstructed.
//
// Also refused for a row that is not actually a dead letter, so an
// operator cannot double-queue something the worker already owns.
func (s *Store) RequeueDeadLetter(id string, now time.Time) error {
	return s.inTx(func(tx *sql.Tx) error {
		var status, bodyText, bodyHTML string
		err := tx.QueryRow(
			`SELECT status, body_text, body_html FROM submissions WHERE id = ?`, id).
			Scan(&status, &bodyText, &bodyHTML)
		if err == sql.ErrNoRows {
			return fmt.Errorf("%w: no such submission", ErrNotReplayable)
		}
		if err != nil {
			return err
		}
		if status != StatusFailed {
			return fmt.Errorf("%w: status is %q, not %q", ErrNotReplayable, status, StatusFailed)
		}
		var queued int
		err = tx.QueryRow(`SELECT COUNT(*) FROM retry_queue WHERE submission_id = ?`, id).Scan(&queued)
		if err != nil {
			return err
		}
		if queued > 0 {
			return fmt.Errorf("%w: already queued for retry", ErrNotReplayable)
		}
		if bodyText == "" && bodyHTML == "" {
			return fmt.Errorf("%w: this submission was logged without its content "+
				"(log_failed_submissions = false), so replaying it would deliver an empty message",
				ErrNotReplayable)
		}
		if _, err := tx.Exec(
			`INSERT INTO retry_queue (submission_id, attempt, next_attempt_at) VALUES (?, 0, ?)`,
			id, now.Unix()); err != nil {
			return err
		}
		_, err = tx.Exec(`UPDATE submissions SET status = ? WHERE id = ?`, StatusQueued, id)
		return err
	})
}

// PendingRetry is one row of the retry queue, for admin inspection.
type PendingRetry struct {
	MessageID     string    `json:"message_id"`
	Project       string    `json:"project,omitempty"`
	Attempt       int       `json:"attempt"`
	NextAttemptAt time.Time `json:"next_attempt_at"`
	LastError     string    `json:"last_error,omitempty"`
}

// ListRetryQueue returns the queue in due order — what will be tried
// next, and when.
func (s *Store) ListRetryQueue(limit int) ([]PendingRetry, error) {
	rows, err := s.db.Query(`
		SELECT q.submission_id, s.project, q.attempt, q.next_attempt_at, s.last_error
		FROM retry_queue q JOIN submissions s ON s.id = q.submission_id
		ORDER BY q.next_attempt_at
		LIMIT ?`, clampLimit(limit))
	if err != nil {
		return nil, fmt.Errorf("storage: list retry queue: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []PendingRetry
	for rows.Next() {
		var p PendingRetry
		var next int64
		if err := rows.Scan(&p.MessageID, &p.Project, &p.Attempt, &next, &p.LastError); err != nil {
			return nil, fmt.Errorf("storage: list retry queue: %w", err)
		}
		p.NextAttemptAt = time.Unix(next, 0).UTC()
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("storage: list retry queue: %w", err)
	}
	return out, nil
}
