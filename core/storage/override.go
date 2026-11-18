package storage

import (
	"database/sql"
	"fmt"
	"time"
)

// Admin disable overrides (FR144).
//
// The rule that makes this safe to have alongside a config file: an
// override can only DISABLE. Clearing one restores whatever the config
// says rather than forcing an enable, so the config file remains the
// source of truth for what is permitted and the override is purely a
// kill switch — reachable during an incident without an edit, a
// restart, and a deploy.
//
// Overrides are persisted so a restart does not silently re-enable
// something an operator turned off at 3am.

// Override kinds.
const (
	OverrideProvider = "provider"
	OverrideProject  = "project"
)

// Override is one disabled entity.
type Override struct {
	Kind   string    `json:"kind"`
	Name   string    `json:"name"`
	Reason string    `json:"reason,omitempty"`
	At     time.Time `json:"at"`
}

// SetOverride disables kind/name. Idempotent: re-disabling something
// already disabled updates the reason and timestamp.
func (s *Store) SetOverride(kind, name, reason string, at time.Time) error {
	_, err := s.db.Exec(`
		INSERT INTO admin_overrides (kind, name, reason, at) VALUES (?, ?, ?, ?)
		ON CONFLICT(kind, name) DO UPDATE SET reason = excluded.reason, at = excluded.at`,
		kind, name, reason, at.Unix())
	if err != nil {
		return fmt.Errorf("storage: set override: %w", err)
	}
	return nil
}

// ClearOverride removes a disable, restoring the configured state.
// Reports whether an override was actually present.
func (s *Store) ClearOverride(kind, name string) (bool, error) {
	res, err := s.db.Exec(`DELETE FROM admin_overrides WHERE kind = ? AND name = ?`, kind, name)
	if err != nil {
		return false, fmt.Errorf("storage: clear override: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("storage: clear override: %w", err)
	}
	return n > 0, nil
}

// IsOverridden reports whether kind/name is disabled by an override.
func (s *Store) IsOverridden(kind, name string) (bool, error) {
	var one int
	err := s.db.QueryRow(
		`SELECT 1 FROM admin_overrides WHERE kind = ? AND name = ?`, kind, name).Scan(&one)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("storage: read override: %w", err)
	}
	return true, nil
}

// ListOverrides returns every active override.
func (s *Store) ListOverrides() ([]Override, error) {
	rows, err := s.db.Query(
		`SELECT kind, name, reason, at FROM admin_overrides ORDER BY kind, name`)
	if err != nil {
		return nil, fmt.Errorf("storage: list overrides: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []Override
	for rows.Next() {
		var o Override
		var at int64
		if err := rows.Scan(&o.Kind, &o.Name, &o.Reason, &at); err != nil {
			return nil, fmt.Errorf("storage: list overrides: %w", err)
		}
		o.At = time.Unix(at, 0).UTC()
		out = append(out, o)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("storage: list overrides: %w", err)
	}
	return out, nil
}

// RecordKeySet stores a digest of a project's CURRENT key set and
// reports whether it differs from what was stored before.
//
// This is how key rotation gets an audit entry without a runtime
// mutation surface: keys live in the config file, and Posthorn notices
// at startup that the set changed. The stored value is a digest of
// digests — it can confirm "the set changed", and cannot be worked
// backwards into a key.
func (s *Store) RecordKeySet(project, keysetDigest string, at time.Time) (changed bool, err error) {
	var previous string
	qerr := s.db.QueryRow(
		`SELECT keyset FROM project_key_state WHERE project = ?`, project).Scan(&previous)
	switch {
	case qerr == sql.ErrNoRows:
		// First sighting is not a rotation.
		changed = false
	case qerr != nil:
		return false, fmt.Errorf("storage: read key state: %w", qerr)
	default:
		changed = previous != keysetDigest
	}
	if _, err := s.db.Exec(`
		INSERT INTO project_key_state (project, keyset, updated_at) VALUES (?, ?, ?)
		ON CONFLICT(project) DO UPDATE SET keyset = excluded.keyset, updated_at = excluded.updated_at`,
		project, keysetDigest, at.Unix()); err != nil {
		return false, fmt.Errorf("storage: record key state: %w", err)
	}
	return changed, nil
}
