package storage

import (
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Scoped query surface (FR136-FR140).
//
// One rule governs this whole file: a project must never see another
// project's rows. That is enforced structurally rather than by
// remembering to add a WHERE clause — every query takes a Scope, the
// Scope's fields are unexported, and the only way to get one that reads
// across projects is to call AdminScope() by name. A missed filter is
// then a compile error or a visible call to AdminScope, not a silent
// leak, and `grep AdminScope` lists every place cross-tenant reads
// happen.
//
// The second rule is that this surface never returns message CONTENT.
// body_text and body_html are not in any SELECT here — not masked, not
// truncated, absent. An operator debugging a delivery needs the
// envelope and the attempt trail; nobody browsing an API needs the
// message their user wrote.

// Scope is the tenancy boundary for a query.
type Scope struct {
	project string
	admin   bool
}

// ProjectScope restricts a query to one project's rows. An empty name
// is refused rather than treated as "all": rows written outside project
// mode carry an empty project, so an empty scope would quietly become a
// wildcard over exactly the rows nobody owns.
func ProjectScope(name string) (Scope, error) {
	if strings.TrimSpace(name) == "" {
		return Scope{}, errors.New("storage: project scope requires a project name")
	}
	return Scope{project: name}, nil
}

// AdminScope reads across every project. Named, exported and greppable
// on purpose: this is the one bypass, and it should be obvious in a
// diff when a new call site starts using it.
func AdminScope() Scope { return Scope{admin: true} }

// Project returns the scope's project, empty for an admin scope.
func (s Scope) Project() string { return s.project }

// IsAdmin reports whether the scope crosses projects.
func (s Scope) IsAdmin() bool { return s.admin }

// where returns the scope's SQL predicate and argument.
func (s Scope) where(column string) (string, []any) {
	if s.admin {
		return "", nil
	}
	return " AND " + column + " = ?", []any{s.project}
}

// MessageFilter narrows a message listing. Every field is optional;
// the zero value lists the scope's most recent messages.
//
// There is deliberately no body or subject search. Full-text search
// over message content would make the query API a reason to keep
// content queryable, and content is exactly what this surface does not
// expose.
type MessageFilter struct {
	Status    string    // submissions.status
	Provider  string    // the provider that carried it
	MailKind  string    // routing label
	Since     time.Time // created_at >= Since
	Until     time.Time // created_at < Until
	Limit     int       // page size; clamped to [1, MaxPageSize]
	Cursor    string    // opaque, from a previous page
	Recipient string    // exact-match recipient address
}

// Page-size bounds. The cap is a memory bound as much as a politeness
// one: a page is materialised in full before it is written.
const (
	DefaultPageSize = 50
	MaxPageSize     = 200
)

// MessageSummary is one row of a listing. It carries the envelope and
// the state, never the content.
type MessageSummary struct {
	ID                string     `json:"id"`
	Project           string     `json:"project,omitempty"`
	Endpoint          string     `json:"endpoint"`
	Provider          string     `json:"provider,omitempty"`
	From              string     `json:"from"`
	To                []string   `json:"to"`
	Subject           string     `json:"subject"`
	Status            string     `json:"status"`
	MailKind          string     `json:"mail_kind,omitempty"`
	DeliveryState     string     `json:"delivery_state,omitempty"`
	ProviderMessageID string     `json:"provider_message_id,omitempty"`
	LastError         string     `json:"last_error,omitempty"`
	CreatedAt         time.Time  `json:"created_at"`
	SentAt            *time.Time `json:"sent_at,omitempty"`
	DeliveryStateAt   *time.Time `json:"delivery_state_at,omitempty"`
	AttachmentCount   int        `json:"attachment_count,omitempty"`
}

// MessagePage is one page of results plus the cursor for the next.
type MessagePage struct {
	Messages   []MessageSummary `json:"messages"`
	NextCursor string           `json:"next_cursor,omitempty"`
}

// cursor encodes the position after the last row of a page. Ordering is
// (created_at DESC, id DESC), so the cursor must carry both: created_at
// alone is not unique, and paging on it drops or repeats rows that
// share a second.
type cursor struct {
	CreatedAt int64  `json:"t"`
	ID        string `json:"i"`
}

func encodeCursor(c cursor) string {
	buf, err := json.Marshal(c)
	if err != nil {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(buf)
}

func decodeCursor(s string) (cursor, error) {
	if s == "" {
		return cursor{}, nil
	}
	buf, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return cursor{}, errors.New("cursor is not valid")
	}
	var c cursor
	if err := json.Unmarshal(buf, &c); err != nil {
		return cursor{}, errors.New("cursor is not valid")
	}
	return c, nil
}

func clampLimit(n int) int {
	switch {
	case n <= 0:
		return DefaultPageSize
	case n > MaxPageSize:
		return MaxPageSize
	default:
		return n
	}
}

// QueryMessages lists messages visible to scope, newest first.
func (s *Store) QueryMessages(scope Scope, f MessageFilter) (MessagePage, error) {
	limit := clampLimit(f.Limit)
	cur, err := decodeCursor(f.Cursor)
	if err != nil {
		return MessagePage{}, err
	}

	var sb strings.Builder
	args := []any{}
	sb.WriteString(`
		SELECT id, project, endpoint, transport, from_addr, to_addrs, subject,
		       status, message_kind, delivery_state, transport_message_id,
		       last_error, created_at, sent_at, delivery_state_at, attachments
		FROM submissions
		WHERE 1=1`)

	scopeSQL, scopeArgs := scope.where("project")
	sb.WriteString(scopeSQL)
	args = append(args, scopeArgs...)

	// Filters are parameterised without exception. Nothing from a
	// request is ever concatenated into this string.
	if f.Status != "" {
		sb.WriteString(" AND status = ?")
		args = append(args, f.Status)
	}
	if f.Provider != "" {
		sb.WriteString(" AND transport = ?")
		args = append(args, f.Provider)
	}
	if f.MailKind != "" {
		sb.WriteString(" AND message_kind = ?")
		args = append(args, f.MailKind)
	}
	if !f.Since.IsZero() {
		sb.WriteString(" AND created_at >= ?")
		args = append(args, f.Since.Unix())
	}
	if !f.Until.IsZero() {
		sb.WriteString(" AND created_at < ?")
		args = append(args, f.Until.Unix())
	}
	if f.Recipient != "" {
		// to_addrs is a JSON array; the recipient index is the delivery
		// projection, so this is an exact membership test rather than a
		// substring match — "a@b.com" must not match "xa@b.com".
		sb.WriteString(` AND EXISTS (
			SELECT 1 FROM json_each(submissions.to_addrs)
			WHERE json_each.value = ?)`)
		args = append(args, f.Recipient)
	}
	if cur.ID != "" {
		sb.WriteString(" AND (created_at < ? OR (created_at = ? AND id < ?))")
		args = append(args, cur.CreatedAt, cur.CreatedAt, cur.ID)
	}
	// One extra row tells us whether a next page exists without a
	// second COUNT query over the same predicate.
	sb.WriteString(" ORDER BY created_at DESC, id DESC LIMIT ?")
	args = append(args, limit+1)

	rows, err := s.db.Query(sb.String(), args...)
	if err != nil {
		return MessagePage{}, fmt.Errorf("storage: query messages: %w", err)
	}
	defer func() { _ = rows.Close() }()

	messages, err := scanSummaries(rows)
	if err != nil {
		return MessagePage{}, err
	}
	page := MessagePage{Messages: messages}
	if len(page.Messages) > limit {
		last := page.Messages[limit-1]
		page.Messages = page.Messages[:limit]
		page.NextCursor = encodeCursor(cursor{CreatedAt: last.CreatedAt.Unix(), ID: last.ID})
	}
	return page, nil
}

// scanSummaries reads MessageSummary rows from a query that selected
// the summary column list. The column list is fixed and body columns
// are not in it — that is the guarantee, so it lives in one place.
func scanSummaries(rows *sql.Rows) ([]MessageSummary, error) {
	out := make([]MessageSummary, 0, 8)
	for rows.Next() {
		var m MessageSummary
		var toJSON, attJSON string
		var createdAt, deliveryStateAt int64
		var sentAt sql.NullInt64
		if err := rows.Scan(&m.ID, &m.Project, &m.Endpoint, &m.Provider, &m.From,
			&toJSON, &m.Subject, &m.Status, &m.MailKind, &m.DeliveryState,
			&m.ProviderMessageID, &m.LastError, &createdAt, &sentAt,
			&deliveryStateAt, &attJSON); err != nil {
			return nil, fmt.Errorf("storage: scan message: %w", err)
		}
		if err := json.Unmarshal([]byte(toJSON), &m.To); err != nil {
			return nil, fmt.Errorf("storage: decode to_addrs: %w", err)
		}
		var atts []Attachment
		if err := json.Unmarshal([]byte(attJSON), &atts); err == nil {
			m.AttachmentCount = len(atts)
		}
		m.CreatedAt = time.Unix(createdAt, 0).UTC()
		if sentAt.Valid {
			t := time.Unix(sentAt.Int64, 0).UTC()
			m.SentAt = &t
		}
		if deliveryStateAt > 0 {
			t := time.Unix(deliveryStateAt, 0).UTC()
			m.DeliveryStateAt = &t
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("storage: scan messages: %w", err)
	}
	return out, nil
}

// QueryMessage returns one message visible to scope.
//
// A message owned by another project reports ok=false — the SAME answer
// as a message that does not exist. Distinguishing the two would turn
// this endpoint into an oracle for guessing other tenants' message ids.
func (s *Store) QueryMessage(scope Scope, id string) (MessageSummary, bool, error) {
	scopeSQL, scopeArgs := scope.where("project")
	args := append([]any{id}, scopeArgs...)
	row := s.db.QueryRow(`
		SELECT id, project, endpoint, transport, from_addr, to_addrs, subject,
		       status, message_kind, delivery_state, transport_message_id,
		       last_error, created_at, sent_at, delivery_state_at, attachments
		FROM submissions WHERE id = ?`+scopeSQL, args...)

	var m MessageSummary
	var toJSON, attJSON string
	var createdAt, deliveryStateAt int64
	var sentAt sql.NullInt64
	err := row.Scan(&m.ID, &m.Project, &m.Endpoint, &m.Provider, &m.From,
		&toJSON, &m.Subject, &m.Status, &m.MailKind, &m.DeliveryState,
		&m.ProviderMessageID, &m.LastError, &createdAt, &sentAt,
		&deliveryStateAt, &attJSON)
	if err == sql.ErrNoRows {
		return MessageSummary{}, false, nil
	}
	if err != nil {
		return MessageSummary{}, false, fmt.Errorf("storage: query message: %w", err)
	}
	if err := json.Unmarshal([]byte(toJSON), &m.To); err != nil {
		return MessageSummary{}, false, fmt.Errorf("storage: decode to_addrs: %w", err)
	}
	var atts []Attachment
	if err := json.Unmarshal([]byte(attJSON), &atts); err == nil {
		m.AttachmentCount = len(atts)
	}
	m.CreatedAt = time.Unix(createdAt, 0).UTC()
	if sentAt.Valid {
		t := time.Unix(sentAt.Int64, 0).UTC()
		m.SentAt = &t
	}
	if deliveryStateAt > 0 {
		t := time.Unix(deliveryStateAt, 0).UTC()
		m.DeliveryStateAt = &t
	}
	return m, true, nil
}

// visible reports whether scope may see a message id. Sub-resources
// check this FIRST and answer "not found" when it fails, so attempts
// and events cannot be read for a message the caller cannot see —
// message_attempts has no project column of its own, and joining is
// exactly the kind of easy-to-forget filter this guards.
func (s *Store) visible(scope Scope, id string) (bool, error) {
	if scope.admin {
		var one int
		err := s.db.QueryRow(`SELECT 1 FROM submissions WHERE id = ?`, id).Scan(&one)
		if err == sql.ErrNoRows {
			return false, nil
		}
		return err == nil, err
	}
	var one int
	err := s.db.QueryRow(`SELECT 1 FROM submissions WHERE id = ? AND project = ?`,
		id, scope.project).Scan(&one)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("storage: check message visibility: %w", err)
	}
	return true, nil
}

// QueryAttempts returns one message's provider attempts, oldest first.
// ok=false means the message is not visible to scope.
func (s *Store) QueryAttempts(scope Scope, id string) ([]Attempt, bool, error) {
	ok, err := s.visible(scope, id)
	if err != nil || !ok {
		return nil, false, err
	}
	attempts, err := s.AttemptsFor(id)
	if err != nil {
		return nil, false, err
	}
	return attempts, true, nil
}

// QueryEvents returns one message's delivery events, oldest first.
// ok=false means the message is not visible to scope.
func (s *Store) QueryEvents(scope Scope, id string) ([]DeliveryEventRow, bool, error) {
	ok, err := s.visible(scope, id)
	if err != nil || !ok {
		return nil, false, err
	}
	events, err := s.EventsFor(id)
	if err != nil {
		return nil, false, err
	}
	return events, true, nil
}

// UsageWindow is one accounting window's totals.
type UsageWindow struct {
	Period  string `json:"period"`
	Success int64  `json:"success"`
	Failure int64  `json:"failure"`
}

// UsageReport is a project's own usage — never another project's, and
// never the provider-level pool numbers, which describe the operator's
// account rather than this tenant's activity.
type UsageReport struct {
	Project string      `json:"project"`
	Day     UsageWindow `json:"day"`
	Month   UsageWindow `json:"month"`
}

// QueryUsage reports one project's usage for the given periods.
func (s *Store) QueryUsage(scope Scope, day, month string) (UsageReport, error) {
	if scope.admin {
		return UsageReport{}, errors.New("storage: usage is per-project; use ProjectUsageAt for a named project")
	}
	today, monthTotal, err := s.ProjectUsageAt(scope.project, day, month)
	if err != nil {
		return UsageReport{}, err
	}
	return UsageReport{
		Project: scope.project,
		Day:     UsageWindow{Period: day, Success: today.Success, Failure: today.Failure},
		Month:   UsageWindow{Period: month, Success: monthTotal.Success, Failure: monthTotal.Failure},
	}, nil
}

// ParseLimit reads a limit query parameter. Exposed so the HTTP layer
// and the CLI agree on the bounds rather than each inventing them.
func ParseLimit(raw string) (int, error) {
	if raw == "" {
		return DefaultPageSize, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, errors.New("limit must be an integer")
	}
	if n < 1 {
		return 0, errors.New("limit must be at least 1")
	}
	return clampLimit(n), nil
}
