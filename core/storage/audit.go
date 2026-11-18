package storage

import (
	"encoding/json"
	"fmt"
	"time"
)

// Administrative audit log (FR147).
//
// Every privileged action Posthorn offers writes one row here before it
// takes effect. Two rules govern what goes in:
//
//	No secrets, ever. There is no column for one, and Details is
//	restricted to describing WHAT changed — "the api key for project
//	acme was rotated", never the key, the old key, or a fingerprint
//	that could confirm a guess.
//
//	No expiry. Audit rows are exempt from retention pruning: a trail
//	that ages out is a trail an attacker only has to outwait.

// Audit actions. A fixed enum, not free text: these values are read by
// operators filtering the log, and an action nobody can spell is an
// action nobody can find.
const (
	AuditProviderEnabled    = "provider.enabled"
	AuditProviderDisabled   = "provider.disabled"
	AuditBreakerReset       = "provider.breaker_reset"
	AuditProjectDisabled    = "project.disabled"
	AuditProjectEnabled     = "project.enabled"
	AuditAPIKeyRotated      = "project.api_key_rotated"
	AuditDeadLetterRetry    = "message.dead_letter_retried"
	AuditSuppressionAdded   = "suppression.added"
	AuditSuppressionRemoved = "suppression.removed"
	AuditAdminLogin         = "admin.login"
	AuditAdminLoginFailed   = "admin.login_failed"
)

// AuditEntry is one recorded action.
type AuditEntry struct {
	ID      int64             `json:"id"`
	At      time.Time         `json:"at"`
	Actor   string            `json:"actor"`
	Action  string            `json:"action"`
	Target  string            `json:"target,omitempty"`
	Details map[string]string `json:"details,omitempty"`

	// SourceIP is the admin request's peer address. Recorded because
	// "from where" is half of what an operator reviewing the log needs;
	// empty for actions taken from the CLI on the host itself.
	SourceIP string `json:"source_ip,omitempty"`
}

// Actor values.
const (
	ActorAdmin = "admin"
	ActorCLI   = "cli"
)

// RecordAudit appends one entry.
//
// A failure to write the audit row is returned to the caller rather
// than swallowed: the calling convention is that an action which cannot
// be recorded does not proceed.
func (s *Store) RecordAudit(e AuditEntry) error {
	if e.At.IsZero() {
		e.At = time.Now()
	}
	details := "{}"
	if len(e.Details) > 0 {
		buf, err := json.Marshal(e.Details)
		if err != nil {
			return fmt.Errorf("storage: encode audit details: %w", err)
		}
		details = string(buf)
	}
	_, err := s.db.Exec(`
		INSERT INTO audit_log (at, actor, action, target, details, source_ip)
		VALUES (?, ?, ?, ?, ?, ?)`,
		e.At.Unix(), e.Actor, e.Action, e.Target, details, e.SourceIP)
	if err != nil {
		return fmt.Errorf("storage: record audit: %w", err)
	}
	return nil
}

// ListAudit returns the most recent entries, newest first.
func (s *Store) ListAudit(limit int) ([]AuditEntry, error) {
	rows, err := s.db.Query(`
		SELECT id, at, actor, action, target, details, source_ip
		FROM audit_log ORDER BY at DESC, id DESC LIMIT ?`, clampLimit(limit))
	if err != nil {
		return nil, fmt.Errorf("storage: list audit: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []AuditEntry
	for rows.Next() {
		var e AuditEntry
		var at int64
		var details string
		if err := rows.Scan(&e.ID, &at, &e.Actor, &e.Action, &e.Target, &details, &e.SourceIP); err != nil {
			return nil, fmt.Errorf("storage: list audit: %w", err)
		}
		e.At = time.Unix(at, 0).UTC()
		if details != "" && details != "{}" {
			_ = json.Unmarshal([]byte(details), &e.Details)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("storage: list audit: %w", err)
	}
	return out, nil
}

// CountAudit returns the number of recorded entries.
func (s *Store) CountAudit() (int, error) {
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM audit_log`).Scan(&n); err != nil {
		return 0, fmt.Errorf("storage: count audit: %w", err)
	}
	return n, nil
}
