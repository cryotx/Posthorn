package storage

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Multi-tenant persistence (FR112, FR114, FR115): per-message provider
// attempts, normalized delivery events, and per-project usage counters.
//
// None of these tables holds message bodies, recipients, or subjects.
// An attempt row answers "which provider took this, and what happened",
// and an event row answers "what did the provider say afterwards" —
// neither needs content, and storing it would put verification codes on
// disk for the benefit of a debugging view nobody asked for (NFR44).

// Attempt is one provider attempt for one logical message (FR112).
type Attempt struct {
	MessageID         string
	Attempt           int
	Provider          string
	StartedAt         time.Time
	FinishedAt        time.Time
	Result            string // AttemptSent | AttemptFailed
	ErrorClass        string
	ProviderMessageID string
}

// Attempt results.
const (
	AttemptSent   = "sent"
	AttemptFailed = "failed"
)

// RecordAttempt appends one provider attempt.
func (s *Store) RecordAttempt(a Attempt) error {
	_, err := s.db.Exec(`
		INSERT INTO message_attempts
		  (message_id, attempt, provider, started_at, finished_at, result, error_class, provider_message_id)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		a.MessageID, a.Attempt, a.Provider,
		a.StartedAt.Unix(), a.FinishedAt.Unix(),
		a.Result, a.ErrorClass, a.ProviderMessageID)
	if err != nil {
		return fmt.Errorf("storage: record attempt: %w", err)
	}
	return nil
}

// AttemptsFor returns every attempt for one message, oldest first —
// the "Resend timed out, SES delivered" story for a single message id.
func (s *Store) AttemptsFor(messageID string) ([]Attempt, error) {
	rows, err := s.db.Query(`
		SELECT message_id, attempt, provider, started_at, finished_at,
		       result, error_class, provider_message_id
		FROM message_attempts WHERE message_id = ? ORDER BY id ASC`, messageID)
	if err != nil {
		return nil, fmt.Errorf("storage: list attempts: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []Attempt
	for rows.Next() {
		var a Attempt
		var started, finished int64
		if err := rows.Scan(&a.MessageID, &a.Attempt, &a.Provider, &started, &finished,
			&a.Result, &a.ErrorClass, &a.ProviderMessageID); err != nil {
			return nil, fmt.Errorf("storage: scan attempt: %w", err)
		}
		a.StartedAt = time.Unix(started, 0).UTC()
		a.FinishedAt = time.Unix(finished, 0).UTC()
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("storage: read attempts: %w", err)
	}
	return out, nil
}

// PruneAttempts drops attempt rows older than cutoff. Called from the
// same maintenance pass as submission pruning, on the same clock.
func (s *Store) PruneAttempts(cutoff time.Time) (int64, error) {
	res, err := s.db.Exec(`DELETE FROM message_attempts WHERE started_at < ?`, cutoff.Unix())
	if err != nil {
		return 0, fmt.Errorf("storage: prune attempts: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// DeliveryEventRow is a normalized event as persisted (FR114).
type DeliveryEventRow struct {
	EventID           string
	MessageID         string
	Project           string
	Provider          string
	ProviderMessageID string
	Type              string
	OccurredAt        time.Time
	Metadata          map[string]string

	// ProviderEventKey is the provider's own identifier for this event,
	// stable across the provider's delivery retries (FR122). It carries
	// a UNIQUE index, so a redelivery collapses into the existing row
	// instead of producing a second one.
	ProviderEventKey string
}

// RecordDeliveryEvent stores one normalized event. The event_id primary
// key makes this idempotent: a provider that retries its webhook, or
// two providers reporting the same thing, cannot double-count.
// Returns whether the row was new.
func (s *Store) RecordDeliveryEvent(e DeliveryEventRow) (bool, error) {
	meta := []byte("{}")
	if len(e.Metadata) > 0 {
		encoded, err := json.Marshal(e.Metadata)
		if err != nil {
			return false, fmt.Errorf("storage: marshal event metadata: %w", err)
		}
		meta = encoded
	}
	// Two conflict targets, one statement: event_id is Posthorn's own
	// id, provider_event_key is the provider's. A redelivery reuses the
	// provider's key while Posthorn mints a fresh event_id, so it is the
	// second index that actually catches replays.
	res, err := s.db.Exec(`
		INSERT INTO delivery_events
		  (event_id, message_id, project, provider, provider_message_id,
		   provider_event_key, type, occurred_at, metadata)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(event_id) DO NOTHING`,
		e.EventID, e.MessageID, e.Project, e.Provider, e.ProviderMessageID,
		e.ProviderEventKey, e.Type, e.OccurredAt.Unix(), meta)
	if err != nil {
		// A UNIQUE violation on provider_event_key is the replay case,
		// not a failure: the provider sent an event we already have.
		if isUniqueViolation(err) {
			return false, nil
		}
		return false, fmt.Errorf("storage: record delivery event: %w", err)
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// isUniqueViolation reports whether err is SQLite's UNIQUE constraint
// error. Matched on the message because the pure-Go driver does not
// surface a typed constraint error, and a partial-index conflict is a
// normal outcome here rather than an exception.
func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

// EventsFor returns every recorded event for one message, oldest first.
func (s *Store) EventsFor(messageID string) ([]DeliveryEventRow, error) {
	rows, err := s.db.Query(`
		SELECT event_id, message_id, project, provider, provider_message_id,
		       type, occurred_at, metadata
		FROM delivery_events WHERE message_id = ? ORDER BY occurred_at ASC, event_id ASC`,
		messageID)
	if err != nil {
		return nil, fmt.Errorf("storage: list events: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []DeliveryEventRow
	for rows.Next() {
		var e DeliveryEventRow
		var occurred int64
		var meta []byte
		if err := rows.Scan(&e.EventID, &e.MessageID, &e.Project, &e.Provider,
			&e.ProviderMessageID, &e.Type, &occurred, &meta); err != nil {
			return nil, fmt.Errorf("storage: scan event: %w", err)
		}
		e.OccurredAt = time.Unix(occurred, 0).UTC()
		if len(meta) > 0 {
			_ = json.Unmarshal(meta, &e.Metadata)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("storage: read events: %w", err)
	}
	return out, nil
}

// PruneDeliveryEvents drops event rows older than cutoff.
func (s *Store) PruneDeliveryEvents(cutoff time.Time) (int64, error) {
	res, err := s.db.Exec(`DELETE FROM delivery_events WHERE occurred_at < ?`, cutoff.Unix())
	if err != nil {
		return 0, fmt.Errorf("storage: prune delivery events: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// ProjectUsage is one project's counters for a period (FR115).
type ProjectUsage struct {
	Project string
	Success int64
	Failure int64
}

// AddProjectUsage increments a project's counters for both the day and
// month periods in one transaction.
//
// Period-keyed exactly like provider usage, and for the same reason: a
// new day is a new row, so nothing has to run at midnight. But it is a
// SEPARATE table with no limit enforcement — a project budget and a
// provider quota answer different questions, and sharing the machinery
// would make "who ran out" ambiguous (ADR-35).
func (s *Store) AddProjectUsage(name, day, month string, success, failure int64) error {
	return s.inTx(func(tx *sql.Tx) error {
		const upsert = `
			INSERT INTO project_usage (project, window, period, success, failure)
			VALUES (?, ?, ?, ?, ?)
			ON CONFLICT(project, window, period) DO UPDATE SET
			  success = success + excluded.success,
			  failure = failure + excluded.failure`
		if _, err := tx.Exec(upsert, name, "day", day, success, failure); err != nil {
			return fmt.Errorf("add project day usage: %w", err)
		}
		if _, err := tx.Exec(upsert, name, "month", month, success, failure); err != nil {
			return fmt.Errorf("add project month usage: %w", err)
		}
		return nil
	})
}

// ProjectUsageAt reads one project's counters for the given periods.
func (s *Store) ProjectUsageAt(name, day, month string) (today, monthTotal ProjectUsage, err error) {
	today.Project, monthTotal.Project = name, name
	row := s.db.QueryRow(`
		SELECT COALESCE((SELECT success FROM project_usage
		                 WHERE project = ? AND window = 'day' AND period = ?), 0),
		       COALESCE((SELECT failure FROM project_usage
		                 WHERE project = ? AND window = 'day' AND period = ?), 0),
		       COALESCE((SELECT success FROM project_usage
		                 WHERE project = ? AND window = 'month' AND period = ?), 0),
		       COALESCE((SELECT failure FROM project_usage
		                 WHERE project = ? AND window = 'month' AND period = ?), 0)`,
		name, day, name, day, name, month, name, month)
	if err := row.Scan(&today.Success, &today.Failure, &monthTotal.Success, &monthTotal.Failure); err != nil {
		return today, monthTotal, fmt.Errorf("storage: read project usage: %w", err)
	}
	return today, monthTotal, nil
}

// PruneProjectUsage drops stale day rows on the retention clock; month
// rows are kept two years so a usage review can look back.
func (s *Store) PruneProjectUsage(cutoff time.Time) error {
	if _, err := s.db.Exec(
		`DELETE FROM project_usage WHERE window = 'day' AND period < ?`,
		cutoff.UTC().Format("2006-01-02")); err != nil {
		return fmt.Errorf("storage: prune project_usage: %w", err)
	}
	if _, err := s.db.Exec(
		`DELETE FROM project_usage WHERE window = 'month' AND period < ?`,
		cutoff.UTC().AddDate(-2, 0, 0).Format("2006-01")); err != nil {
		return fmt.Errorf("storage: prune project_usage: %w", err)
	}
	return nil
}
