// Modified for the cryotx/posthorn distribution.

// Package storage is the v2.0 SQLite spine (FR76-FR81, ADR-20, ADR-21).
//
// It is entirely optional: without a `[storage]` config block nothing in
// this package is constructed and every v1.x code path runs unchanged.
// When configured, it provides the submission log, the retry queue, the
// durable idempotency backend, and the suppression list — one file, one
// process, one writer (NFR26).
//
// Failure posture (NFR27): callers treat any error from this package as
// a degrade signal, never a reason to block mail. The gateway keeps
// sending synchronously; it just stops persisting until the canary
// probe reports recovery.
//
// The driver is modernc.org/sqlite — pure Go, preserving the
// CGO_ENABLED=0 static multi-arch build. This is deliberately the
// project's largest dependency; the reasoning and named costs live in
// ADR-20. Everything SQLite touches stays behind this package.
package storage

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sync"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" database/sql driver
)

// schemaVersion is stamped into SQLite's user_version pragma.
// Migrations are forward-only: Open applies every step from the file's
// current version up to this constant.
const schemaVersion = 6

// Submission statuses. The visible lifecycle is
// sending → sent | queued → sent | failed, plus suppressed for sends
// refused at the suppression check. "sending" rows found at Open are
// the crash window: they re-enter the queue, which is the documented
// at-least-once duplicate risk (NFR28).
const (
	StatusSending    = "sending"
	StatusSent       = "sent"
	StatusQueued     = "queued"
	StatusFailed     = "failed"
	StatusSuppressed = "suppressed"
)

// Config mirrors the `[storage]` TOML block after parsing.
type Config struct {
	// Path is the SQLite file location. Required unless InMemory.
	Path string

	// InMemory uses a private in-memory database — tests only.
	InMemory bool

	// Retention bounds how long non-pending submission rows live.
	// Zero means the config default (30d) was not applied by the caller;
	// Store itself does not interpret zero specially — Prune receives
	// an explicit cutoff.
	Retention time.Duration

	// MaxSize caps the database file size via SQLite's max_page_count
	// (FR79) so Posthorn can never fill the disk on its own. Zero means
	// no cap (the config layer applies the 1GB default).
	MaxSize int64
}

// Store wraps the SQLite handle. All methods are safe for concurrent
// use within one process; the pool is capped at a single connection,
// which both serializes writers (SQLite wants that) and makes the
// single-writer assumption structural rather than conventional.
type Store struct {
	db                *sql.DB
	providerKeyPath   string
	providerKeyMu     sync.Mutex
	providerMemoryKey []byte
}

// Open opens (creating if needed) the database, applies pragmas and
// forward-only migrations, and recovers crash-window rows: submissions
// stuck in "sending" from a previous process re-enter the retry queue
// with an immediate next attempt (NFR28).
func Open(cfg Config) (*Store, error) {
	dsn := cfg.Path
	if cfg.InMemory {
		dsn = ":memory:"
	}
	if dsn == "" {
		return nil, fmt.Errorf("storage: path is required unless in_memory = true")
	}

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("storage: open %q: %w", dsn, err)
	}
	// One connection: serialized access, no SQLITE_BUSY dances, and the
	// NFR26 single-writer stance enforced by construction. Throughput is
	// not a concern at Posthorn volumes.
	db.SetMaxOpenConns(1)

	s := &Store{db: db}
	if !isMemory(cfg) {
		s.providerKeyPath = filepath.Join(filepath.Dir(cfg.Path), "provider-secrets.key")
	}
	if err := s.init(cfg); err != nil {
		_ = db.Close()
		return nil, err
	}
	// One canary write before reporting success. Opening proves the
	// file is READABLE; a database whose volume is mounted read-only,
	// or whose disk is full, opens perfectly and fails on the first
	// submission — which is far too late to find out. An already-current
	// database runs no migration, so without this there is a startup
	// path that writes nothing at all.
	if err := s.Probe(time.Now()); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("storage: opened %q but cannot write to it: %w", dsn, err)
	}
	return s, nil
}

func (s *Store) init(cfg Config) error {
	pragmas := []string{
		"PRAGMA busy_timeout = 5000",
		"PRAGMA foreign_keys = ON",
		// FULL over NORMAL: a power loss must not lose a committed
		// queue entry — the queue's entire promise is surviving exactly
		// that. Commit latency is irrelevant at our volumes.
		"PRAGMA synchronous = FULL",
		// Temp tables and sorting spills stay in memory. The production
		// container runs with a read-only root filesystem, and SQLite's
		// default is to write temp files into /tmp — which would turn a
		// large query into an I/O error at exactly the wrong moment.
		// Posthorn's working sets are small enough that memory is the
		// right place for them anyway.
		"PRAGMA temp_store = MEMORY",
	}
	if !isMemory(cfg) {
		pragmas = append(pragmas, "PRAGMA journal_mode = WAL")
	}
	for _, p := range pragmas {
		if _, err := s.db.Exec(p); err != nil {
			return fmt.Errorf("storage: %s: %w", p, err)
		}
	}

	if cfg.MaxSize > 0 {
		var pageSize int64
		if err := s.db.QueryRow("PRAGMA page_size").Scan(&pageSize); err != nil || pageSize <= 0 {
			return fmt.Errorf("storage: read page_size: %w", err)
		}
		pages := cfg.MaxSize / pageSize
		if pages < 16 {
			pages = 16 // floor: room for the schema itself
		}
		if _, err := s.db.Exec(fmt.Sprintf("PRAGMA max_page_count = %d", pages)); err != nil {
			return fmt.Errorf("storage: set max_page_count: %w", err)
		}
	}

	if err := s.migrate(); err != nil {
		return err
	}
	return s.recoverInFlight()
}

func isMemory(cfg Config) bool {
	return cfg.InMemory || cfg.Path == ":memory:"
}

func (s *Store) migrate() error {
	var version int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("storage: read user_version: %w", err)
	}
	if version > schemaVersion {
		return fmt.Errorf("storage: database schema version %d is newer than this binary supports (%d) — refusing to open", version, schemaVersion)
	}
	if version < 1 {
		if _, err := s.db.Exec(schemaV1); err != nil {
			return fmt.Errorf("storage: apply schema v1: %w", err)
		}
	}
	if version < 2 {
		if _, err := s.db.Exec(schemaV2); err != nil {
			return fmt.Errorf("storage: apply schema v2: %w", err)
		}
	}
	if version < 3 {
		if err := s.migrateV3(); err != nil {
			return err
		}
	}
	if version < 4 {
		if err := s.migrateV4(); err != nil {
			return err
		}
	}
	if version < 5 {
		if err := s.migrateV5(); err != nil {
			return err
		}
	}
	if version < 6 {
		if err := s.migrateV6(); err != nil {
			return err
		}
	}
	// Additive feature table: older v6 binaries can still open this database.
	// They do not authenticate managed credentials, so rollback requires
	// temporarily switching callers to legacy credentials.
	if _, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS managed_projects (name TEXT PRIMARY KEY, default_from TEXT NOT NULL, key_digest BLOB NOT NULL)`); err != nil {
		return fmt.Errorf("storage: create managed projects table: %w", err)
	}
	if _, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS managed_providers (name TEXT PRIMARY KEY, type TEXT NOT NULL, sender TEXT NOT NULL, settings BLOB NOT NULL, daily_limit INTEGER NOT NULL, monthly_limit INTEGER NOT NULL, enabled INTEGER NOT NULL)`); err != nil {
		return fmt.Errorf("storage: create managed providers table: %w", err)
	}
	if version != schemaVersion {
		if _, err := s.db.Exec(fmt.Sprintf("PRAGMA user_version = %d", schemaVersion)); err != nil {
			return fmt.Errorf("storage: stamp user_version: %w", err)
		}
	}
	return nil
}

const schemaV1 = `
CREATE TABLE IF NOT EXISTS submissions (
  id                   TEXT PRIMARY KEY,
  endpoint             TEXT NOT NULL,
  transport            TEXT NOT NULL DEFAULT '',
  from_addr            TEXT NOT NULL DEFAULT '',
  to_addrs             TEXT NOT NULL DEFAULT '[]',
  reply_to             TEXT NOT NULL DEFAULT '',
  subject              TEXT NOT NULL DEFAULT '',
  body_text            TEXT NOT NULL DEFAULT '',
  body_html            TEXT NOT NULL DEFAULT '',
  fields               TEXT NOT NULL DEFAULT '{}',
  attachments          TEXT NOT NULL DEFAULT '[]',
  client_ip            TEXT NOT NULL DEFAULT '',
  status               TEXT NOT NULL,
  transport_message_id TEXT NOT NULL DEFAULT '',
  last_error           TEXT NOT NULL DEFAULT '',
  created_at           INTEGER NOT NULL,
  sent_at              INTEGER
);
CREATE INDEX IF NOT EXISTS idx_submissions_created ON submissions(created_at);
CREATE INDEX IF NOT EXISTS idx_submissions_msgid   ON submissions(transport_message_id);

CREATE TABLE IF NOT EXISTS retry_queue (
  submission_id   TEXT PRIMARY KEY REFERENCES submissions(id) ON DELETE CASCADE,
  attempt         INTEGER NOT NULL DEFAULT 0,
  next_attempt_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_retry_due ON retry_queue(next_attempt_at);

CREATE TABLE IF NOT EXISTS idempotency (
  key        TEXT NOT NULL,
  endpoint   TEXT NOT NULL,
  response   BLOB NOT NULL,
  expires_at INTEGER NOT NULL,
  PRIMARY KEY (key, endpoint)
);
CREATE INDEX IF NOT EXISTS idx_idem_expiry ON idempotency(expires_at);

CREATE TABLE IF NOT EXISTS suppressions (
  email           TEXT NOT NULL,
  reason          TEXT NOT NULL,
  source_endpoint TEXT NOT NULL DEFAULT '',
  created_at      INTEGER NOT NULL,
  PRIMARY KEY (email, reason)
);

CREATE TABLE IF NOT EXISTS canary (
  id INTEGER PRIMARY KEY CHECK (id = 1),
  at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS lifecycle_queue (
  id              INTEGER PRIMARY KEY AUTOINCREMENT,
  endpoint        TEXT NOT NULL,
  payload         BLOB NOT NULL,
  attempt         INTEGER NOT NULL DEFAULT 0,
  next_attempt_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_lifecycle_due ON lifecycle_queue(next_attempt_at);
`

// schemaV2 adds provider quota accounting (FR100).
//
// One row per (provider, window, period). Keying by the period string
// rather than storing a "current count" plus a reset timestamp is what
// makes rollover free: a new day is a new key, so the old count is
// simply not read any more. Nothing has to run at midnight, and a
// process that was down across the boundary reads the right row on its
// next query rather than a stale counter it would have to reason about.
const schemaV2 = `
CREATE TABLE IF NOT EXISTS provider_usage (
  provider TEXT NOT NULL,
  window   TEXT NOT NULL,           -- 'day' | 'month'
  period   TEXT NOT NULL,           -- '2026-09-07' | '2026-09'
  used     INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (provider, window, period)
);
CREATE INDEX IF NOT EXISTS idx_provider_usage_period ON provider_usage(window, period);
`

// migrateV3 adds the multi-tenant surface (FR107-FR115).
//
// The idempotency table is rebuilt rather than altered: its scope
// changes from "endpoint path" to "the authenticated identity that owns
// this key", and it grows the fingerprint and state columns that make
// atomic reservation and payload-conflict detection possible. A rebuild
// is the honest way to change a primary key's meaning, and existing
// rows are dropped deliberately — a v2.2 row has no fingerprint, so
// replaying it could not prove the retried payload matched.
func (s *Store) migrateV3() error {
	if _, err := s.db.Exec(schemaV3); err != nil {
		return fmt.Errorf("storage: apply schema v3: %w", err)
	}
	// The submission log gains project ownership so a future query
	// surface can scope rows to their tenant (NFR46). Rows written
	// before project mode carry an empty project, which reads as "not
	// owned by any project" rather than as a wildcard.
	//
	// Added by ALTER rather than in schemaV1 so a database created by
	// an older binary and one created fresh converge on the same shape.
	if err := s.addColumnIfMissing("submissions", "project", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	return s.addColumnIfMissing("submissions", "message_kind", "TEXT NOT NULL DEFAULT ''")
}

// addColumnIfMissing is an idempotent ALTER: SQLite has no
// ADD COLUMN IF NOT EXISTS, and re-running a migration must not fail.
func (s *Store) addColumnIfMissing(table, column, decl string) error {
	rows, err := s.db.Query(`SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return fmt.Errorf("storage: inspect %s: %w", table, err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return fmt.Errorf("storage: inspect %s: %w", table, err)
		}
		if name == column {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("storage: inspect %s: %w", table, err)
	}
	if _, err := s.db.Exec(fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, column, decl)); err != nil {
		return fmt.Errorf("storage: add %s.%s: %w", table, column, err)
	}
	return nil
}

// migrateV4 adds webhook replay protection and the message-state
// projection (FR122, FR123).
//
// The provider_event_key index is PARTIAL — it only covers non-empty
// keys — so rows written before this release, and any future adapter
// that genuinely cannot produce a stable per-event key, do not all
// collide on the empty string.
func (s *Store) migrateV4() error {
	for _, col := range []struct{ table, name, decl string }{
		{"delivery_events", "provider_event_key", "TEXT NOT NULL DEFAULT ''"},
		{"submissions", "delivery_state", "TEXT NOT NULL DEFAULT ''"},
		{"submissions", "delivery_state_at", "INTEGER NOT NULL DEFAULT 0"},
		// delivery_state_rank was v2.4's single linear ordering. ADR-44
		// replaced it with explicit transition rules, which a rank
		// cannot express; the column stays for databases that already
		// have it and is no longer read or written.
		{"submissions", "delivery_state_rank", "INTEGER NOT NULL DEFAULT 0"},
	} {
		if err := s.addColumnIfMissing(col.table, col.name, col.decl); err != nil {
			return err
		}
	}
	if _, err := s.db.Exec(`
		CREATE UNIQUE INDEX IF NOT EXISTS idx_events_provider_key
		ON delivery_events(provider_event_key)
		WHERE provider_event_key != ''`); err != nil {
		return fmt.Errorf("storage: apply schema v4: %w", err)
	}
	return nil
}

// migrateV5 adds the recipient-scoped delivery projection (FR128).
//
// submissions.delivery_state changes meaning here — from "the latest
// event type" to an AGGREGATE over recipients — so the old column is
// cleared rather than reinterpreted. The events it was derived from are
// untouched, and the projection rebuilds from the next event.
func (s *Store) migrateV5() error {
	if _, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS recipient_delivery (
		  message_id TEXT NOT NULL,
		  recipient  TEXT NOT NULL,
		  state      TEXT NOT NULL,
		  state_at   INTEGER NOT NULL DEFAULT 0,
		  PRIMARY KEY (message_id, recipient)
		);
		CREATE INDEX IF NOT EXISTS idx_recipient_delivery_msg ON recipient_delivery(message_id);
		UPDATE submissions SET delivery_state = '', delivery_state_at = 0;`); err != nil {
		return fmt.Errorf("storage: apply schema v5: %w", err)
	}
	return nil
}

// migrateV6 adds the administrative surface's state (FR143-FR147):
// the audit log, the admin disable overrides, and the record of each
// project's key set used to detect a rotation across restarts.
//
// admin_overrides deserves its own note, because it is the one place
// Posthorn keeps state that config also describes. It can only ever
// SUBTRACT: an override disables a provider or a project, and clearing
// it restores whatever the config says — it can never enable something
// the config disabled. So the config file stays the source of truth for
// what is permitted, and the override is a kill switch an operator can
// reach without an edit-and-restart during an incident.
//
// A privileged action that leaves no trace is indistinguishable from
// one that never happened, and "who disabled the provider at 3am" is
// not a question a log line in a rotated file reliably answers. So the
// record lives in the same database as the thing it describes, and is
// exempt from retention pruning — an audit trail that expires is an
// audit trail an attacker can wait out.
//
// The table has no column for a secret, by construction. Details is a
// short JSON object describing WHAT changed, never a credential.
func (s *Store) migrateV6() error {
	if _, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS audit_log (
		  id         INTEGER PRIMARY KEY AUTOINCREMENT,
		  at         INTEGER NOT NULL,
		  actor      TEXT NOT NULL,             -- 'admin' | 'cli'
		  action     TEXT NOT NULL,             -- fixed enum, see storage/audit.go
		  target     TEXT NOT NULL DEFAULT '',  -- provider/project/message name
		  details    TEXT NOT NULL DEFAULT '{}',
		  source_ip  TEXT NOT NULL DEFAULT ''
		);
		CREATE INDEX IF NOT EXISTS idx_audit_at ON audit_log(at);
		CREATE INDEX IF NOT EXISTS idx_audit_action ON audit_log(action);

		CREATE TABLE IF NOT EXISTS admin_overrides (
		  kind    TEXT NOT NULL,             -- 'provider' | 'project'
		  name    TEXT NOT NULL,
		  reason  TEXT NOT NULL DEFAULT '',
		  at      INTEGER NOT NULL,
		  PRIMARY KEY (kind, name)
		);

		CREATE TABLE IF NOT EXISTS project_key_state (
		  project    TEXT PRIMARY KEY,
		  keyset     TEXT NOT NULL,
		  updated_at INTEGER NOT NULL
		)`); err != nil {
		return fmt.Errorf("storage: apply schema v6: %w", err)
	}
	return nil
}

const schemaV3 = `
DROP TABLE IF EXISTS idempotency;
CREATE TABLE idempotency (
  key         TEXT NOT NULL,
  scope       TEXT NOT NULL,   -- "project:<name>" or the endpoint path
  fingerprint TEXT NOT NULL DEFAULT '',
  state       TEXT NOT NULL DEFAULT 'done',   -- 'pending' | 'done'
  message_id  TEXT NOT NULL DEFAULT '',
  status      INTEGER NOT NULL DEFAULT 0,
  content_type TEXT NOT NULL DEFAULT '',
  response    BLOB NOT NULL DEFAULT x'',
  created_at  INTEGER NOT NULL DEFAULT 0,
  expires_at  INTEGER NOT NULL,
  PRIMARY KEY (key, scope)
);
CREATE INDEX IF NOT EXISTS idx_idem_expiry ON idempotency(expires_at);

CREATE TABLE IF NOT EXISTS message_attempts (
  id                  INTEGER PRIMARY KEY AUTOINCREMENT,
  message_id          TEXT NOT NULL,
  attempt             INTEGER NOT NULL,
  provider            TEXT NOT NULL,
  started_at          INTEGER NOT NULL,
  finished_at         INTEGER NOT NULL,
  result              TEXT NOT NULL,          -- 'sent' | 'failed'
  error_class         TEXT NOT NULL DEFAULT '',
  provider_message_id TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_attempts_message ON message_attempts(message_id);
CREATE INDEX IF NOT EXISTS idx_attempts_started ON message_attempts(started_at);

CREATE TABLE IF NOT EXISTS delivery_events (
  event_id            TEXT PRIMARY KEY,
  message_id          TEXT NOT NULL DEFAULT '',
  project             TEXT NOT NULL DEFAULT '',
  provider            TEXT NOT NULL DEFAULT '',
  provider_message_id TEXT NOT NULL DEFAULT '',
  type                TEXT NOT NULL,
  occurred_at         INTEGER NOT NULL,
  metadata            TEXT NOT NULL DEFAULT '{}'
);
CREATE INDEX IF NOT EXISTS idx_events_message ON delivery_events(message_id);
CREATE INDEX IF NOT EXISTS idx_events_occurred ON delivery_events(occurred_at);

CREATE TABLE IF NOT EXISTS project_usage (
  project TEXT NOT NULL,
  window  TEXT NOT NULL,          -- 'day' | 'month'
  period  TEXT NOT NULL,
  success INTEGER NOT NULL DEFAULT 0,
  failure INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (project, window, period)
);
CREATE INDEX IF NOT EXISTS idx_project_usage_period ON project_usage(window, period);
`

// recoverInFlight moves crash-window rows ("sending" at open) into the
// retry queue with an immediate due time. This is the deliberate
// at-least-once choice from ADR-21: the provider may have accepted the
// send before the crash, and a duplicate beats silent loss.
func (s *Store) recoverInFlight() error {
	_, err := s.db.Exec(`
		INSERT OR IGNORE INTO retry_queue (submission_id, attempt, next_attempt_at)
		SELECT id, 0, 0 FROM submissions WHERE status = ?`, StatusSending)
	if err != nil {
		return fmt.Errorf("storage: recover in-flight submissions: %w", err)
	}
	_, err = s.db.Exec(
		`UPDATE submissions SET status = ? WHERE status = ?`, StatusQueued, StatusSending)
	if err != nil {
		return fmt.Errorf("storage: recover in-flight submissions: %w", err)
	}
	return nil
}

// Submission is one row of the submission log (FR77). ToAddrs and
// Fields marshal to JSON columns. For endpoints with
// log_failed_submissions = false, the caller records metadata only:
// content fields stay empty and the row is queue-ineligible.
type Submission struct {
	ID                 string
	Endpoint           string
	Transport          string
	From               string
	ToAddrs            []string
	ReplyTo            string
	Subject            string
	BodyText           string
	BodyHTML           string
	Fields             map[string][]string
	Attachments        []Attachment
	ClientIP           string
	Status             string
	TransportMessageID string
	LastError          string
	CreatedAt          time.Time
	SentAt             time.Time // zero when unsent

	// Project owns this submission (FR107). Empty for messages sent
	// outside project mode. Recorded so a future query surface can
	// scope rows to their tenant rather than bolting ownership on
	// later (NFR46).
	Project string

	// MessageKind is the routing label the message carried (FR104).
	MessageKind string

	// DeliveryState is the latest delivery outcome projected from the
	// event log (FR123), or empty when no provider event has arrived.
	// A projection, never a substitute for the events themselves.
	DeliveryState   string
	DeliveryStateAt time.Time
}

// Attachment mirrors transport.Attachment for persistence (FR92 —
// queued sends must replay with their files). Data JSON-encodes as
// base64.
type Attachment struct {
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	Data        []byte `json:"data"`
}

// RecordSubmission inserts a new row (FR77).
func (s *Store) RecordSubmission(sub Submission) error {
	toJSON, err := json.Marshal(orEmptySlice(sub.ToAddrs))
	if err != nil {
		return fmt.Errorf("storage: marshal to_addrs: %w", err)
	}
	fieldsJSON, err := json.Marshal(orEmptyMap(sub.Fields))
	if err != nil {
		return fmt.Errorf("storage: marshal fields: %w", err)
	}
	atts := sub.Attachments
	if atts == nil {
		atts = []Attachment{}
	}
	attJSON, err := json.Marshal(atts)
	if err != nil {
		return fmt.Errorf("storage: marshal attachments: %w", err)
	}
	var sentAt any
	if !sub.SentAt.IsZero() {
		sentAt = sub.SentAt.Unix()
	}
	_, err = s.db.Exec(`
		INSERT INTO submissions
		  (id, endpoint, transport, from_addr, to_addrs, reply_to, subject,
		   body_text, body_html, fields, attachments, client_ip, status,
		   transport_message_id, last_error, created_at, sent_at,
		   project, message_kind)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		sub.ID, sub.Endpoint, sub.Transport, sub.From, string(toJSON), sub.ReplyTo,
		sub.Subject, sub.BodyText, sub.BodyHTML, string(fieldsJSON), string(attJSON),
		sub.ClientIP, sub.Status, sub.TransportMessageID, sub.LastError,
		sub.CreatedAt.Unix(), sentAt, sub.Project, sub.MessageKind)
	if err != nil {
		return fmt.Errorf("storage: record submission: %w", err)
	}
	return nil
}

// MarkSent finalizes a row after provider acceptance and removes any
// queue entry.
func (s *Store) MarkSent(id, transportMessageID string, at time.Time) error {
	return s.MarkSentVia(id, "", transportMessageID, at)
}

// MarkSentVia is MarkSent plus the pool provider that actually
// delivered the message (FR97). The pre-send row records the endpoint's
// configured transport, which for a pooled endpoint is only "pool" —
// the useful answer ("ses-backup took it after postmark-prod 500'd")
// exists only once the send returns, so it is written back here.
//
// An empty provider leaves the column as recorded, which is the v1.x
// path: a single inline transport already wrote its own type and has
// nothing to correct.
func (s *Store) MarkSentVia(id, provider, transportMessageID string, at time.Time) error {
	return s.inTx(func(tx *sql.Tx) error {
		if _, err := tx.Exec(`
			UPDATE submissions
			SET status = ?, transport_message_id = ?, sent_at = ?, last_error = ''
			WHERE id = ?`, StatusSent, transportMessageID, at.Unix(), id); err != nil {
			return err
		}
		if provider != "" {
			if _, err := tx.Exec(
				`UPDATE submissions SET transport = ? WHERE id = ?`, provider, id); err != nil {
				return err
			}
		}
		_, err := tx.Exec(`DELETE FROM retry_queue WHERE submission_id = ?`, id)
		return err
	})
}

// MarkStatus updates status + last_error without touching sent_at.
func (s *Store) MarkStatus(id, status, lastError string) error {
	_, err := s.db.Exec(
		`UPDATE submissions SET status = ?, last_error = ? WHERE id = ?`,
		status, lastError, id)
	if err != nil {
		return fmt.Errorf("storage: mark %s: %w", status, err)
	}
	return nil
}

// GetSubmission fetches one row by ID.
func (s *Store) GetSubmission(id string) (Submission, bool, error) {
	return s.scanOne(`WHERE id = ?`, id)
}

// FindByMessageID resolves a provider message ID back to the
// originating submission — the lifecycle-event correlation step (FR83).
func (s *Store) FindByMessageID(transportMessageID string) (Submission, bool, error) {
	if transportMessageID == "" {
		return Submission{}, false, nil
	}
	return s.scanOne(`WHERE transport_message_id = ? ORDER BY created_at DESC LIMIT 1`, transportMessageID)
}

func (s *Store) scanOne(where string, args ...any) (Submission, bool, error) {
	row := s.db.QueryRow(`
		SELECT id, endpoint, transport, from_addr, to_addrs, reply_to, subject,
		       body_text, body_html, fields, attachments, client_ip, status,
		       transport_message_id, last_error, created_at, sent_at,
		       project, message_kind, delivery_state, delivery_state_at
		FROM submissions `+where, args...)

	var sub Submission
	var toJSON, fieldsJSON, attJSON string
	var createdAt, deliveryStateAt int64
	var sentAt sql.NullInt64
	err := row.Scan(&sub.ID, &sub.Endpoint, &sub.Transport, &sub.From, &toJSON,
		&sub.ReplyTo, &sub.Subject, &sub.BodyText, &sub.BodyHTML, &fieldsJSON,
		&attJSON, &sub.ClientIP, &sub.Status, &sub.TransportMessageID, &sub.LastError,
		&createdAt, &sentAt, &sub.Project, &sub.MessageKind,
		&sub.DeliveryState, &deliveryStateAt)
	if err == sql.ErrNoRows {
		return Submission{}, false, nil
	}
	if err != nil {
		return Submission{}, false, fmt.Errorf("storage: get submission: %w", err)
	}
	if err := json.Unmarshal([]byte(toJSON), &sub.ToAddrs); err != nil {
		return Submission{}, false, fmt.Errorf("storage: decode to_addrs: %w", err)
	}
	if err := json.Unmarshal([]byte(fieldsJSON), &sub.Fields); err != nil {
		return Submission{}, false, fmt.Errorf("storage: decode fields: %w", err)
	}
	if err := json.Unmarshal([]byte(attJSON), &sub.Attachments); err != nil {
		return Submission{}, false, fmt.Errorf("storage: decode attachments: %w", err)
	}
	if len(sub.Attachments) == 0 {
		sub.Attachments = nil
	}
	sub.CreatedAt = time.Unix(createdAt, 0).UTC()
	if deliveryStateAt > 0 {
		sub.DeliveryStateAt = time.Unix(deliveryStateAt, 0).UTC()
	}
	if sentAt.Valid {
		sub.SentAt = time.Unix(sentAt.Int64, 0).UTC()
	}
	return sub, true, nil
}

// ListSubmissions returns up to limit rows, newest first — FR77's
// "queryable" acceptance. Serves tests today and the CLI/admin surface
// later; operators can always query the file directly with sqlite3.
func (s *Store) ListSubmissions(limit int) ([]Submission, error) {
	rows, err := s.db.Query(
		`SELECT id FROM submissions ORDER BY created_at DESC, id LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("storage: list submissions: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("storage: list submissions: %w", err)
		}
		ids = append(ids, id)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("storage: list submissions: %w", err)
	}
	subs := make([]Submission, 0, len(ids))
	for _, id := range ids {
		sub, ok, err := s.GetSubmission(id)
		if err != nil {
			return nil, err
		}
		if ok {
			subs = append(subs, sub)
		}
	}
	return subs, nil
}

// Prune deletes submission rows older than cutoff (FR79). Pending work
// ("sending"/"queued") survives regardless of age; suppressions are a
// different table and deliberately never pruned (ADR-23); idempotency
// expiry is time-based via PruneIdempotency.
func (s *Store) Prune(cutoff time.Time) (int64, error) {
	res, err := s.db.Exec(`
		DELETE FROM submissions
		WHERE created_at < ? AND status NOT IN (?, ?)`,
		cutoff.Unix(), StatusSending, StatusQueued)
	if err != nil {
		return 0, fmt.Errorf("storage: prune: %w", err)
	}
	n, _ := res.RowsAffected()

	// Prune is the whole retention pass, not just the submission
	// table: an audit trail that outlives the message it describes is
	// an orphan nobody can join back to anything, and usage rows for
	// periods long past are dead weight. One method so a caller cannot
	// apply retention to half the database.
	//
	// Quota and usage rows are keyed by period string, which sorts
	// chronologically, so the cutoff comparison is a plain string
	// compare. Only day rows go on the submission retention clock;
	// month rows are two per key per year and are kept for a fixed two
	// years so a cost or volume review can look back.
	if _, err := s.db.Exec(
		`DELETE FROM provider_usage WHERE window = 'day' AND period < ?`,
		cutoff.UTC().Format("2006-01-02")); err != nil {
		return n, fmt.Errorf("storage: prune provider_usage: %w", err)
	}
	if _, err := s.db.Exec(
		`DELETE FROM provider_usage WHERE window = 'month' AND period < ?`,
		cutoff.UTC().AddDate(-2, 0, 0).Format("2006-01")); err != nil {
		return n, fmt.Errorf("storage: prune provider_usage: %w", err)
	}
	na, err := s.PruneAttempts(cutoff)
	if err != nil {
		return n, err
	}
	ne, err := s.PruneDeliveryEvents(cutoff)
	if err != nil {
		return n, err
	}
	if err := s.PruneProjectUsage(cutoff); err != nil {
		return n, err
	}
	if err := s.PruneRecipientStates(); err != nil {
		return n, err
	}
	return n + na + ne, nil
}

// Probe is the canary write (FR80): one upsert through the full write
// path — SQLite, journal, filesystem. An error means the storage layer
// is degraded; the caller flips healthz/metrics and stops persisting
// until a later probe succeeds.
func (s *Store) Probe(now time.Time) error {
	_, err := s.db.Exec(
		`INSERT INTO canary (id, at) VALUES (1, ?)
		 ON CONFLICT(id) DO UPDATE SET at = excluded.at`, now.Unix())
	if err != nil {
		return fmt.Errorf("storage: canary probe: %w", err)
	}
	return nil
}

// Close closes the underlying handle.
func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) inTx(fn func(*sql.Tx) error) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("storage: begin tx: %w", err)
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("storage: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("storage: commit: %w", err)
	}
	return nil
}

func orEmptySlice(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func orEmptyMap(m map[string][]string) map[string][]string {
	if m == nil {
		return map[string][]string{}
	}
	return m
}

// SchemaVersion reports the schema version this binary writes. Exposed
// for `posthorn doctor`, which compares it against a database file's
// stamp to tell an operator whether the next start will migrate, do
// nothing, or refuse.
func SchemaVersion() int { return schemaVersion }
