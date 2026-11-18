package storage

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"
)

var qNow = time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)

// seedMessage writes one submission owned by project.
func seedMessage(t *testing.T, s *Store, id, project string, at time.Time, mut func(*Submission)) Submission {
	t.Helper()
	sub := Submission{
		ID:          id,
		Project:     project,
		Endpoint:    "/api/contact",
		Transport:   "postmark",
		From:        "noreply@example.com",
		ToAddrs:     []string{"user@example.com"},
		Subject:     "Subject for " + id,
		BodyText:    "SECRET BODY CONTENT " + id,
		BodyHTML:    "<p>SECRET BODY CONTENT " + id + "</p>",
		Status:      StatusSent,
		MessageKind: "notification",
		CreatedAt:   at,
	}
	if mut != nil {
		mut(&sub)
	}
	if err := s.RecordSubmission(sub); err != nil {
		t.Fatalf("RecordSubmission(%s): %v", id, err)
	}
	return sub
}

func projectScope(t *testing.T, name string) Scope {
	t.Helper()
	sc, err := ProjectScope(name)
	if err != nil {
		t.Fatalf("ProjectScope(%q): %v", name, err)
	}
	return sc
}

// --- isolation ---------------------------------------------------------

// TestQueryMessages_ProjectIsolation is the requirement this whole file
// exists to satisfy: project A must never see project B's rows.
func TestQueryMessages_ProjectIsolation(t *testing.T) {
	s := memStore(t)
	seedMessage(t, s, "a-1", "acme", qNow, nil)
	seedMessage(t, s, "a-2", "acme", qNow.Add(time.Minute), nil)
	seedMessage(t, s, "b-1", "globex", qNow.Add(2*time.Minute), nil)
	seedMessage(t, s, "legacy", "", qNow.Add(3*time.Minute), nil)

	page, err := s.QueryMessages(projectScope(t, "acme"), MessageFilter{})
	if err != nil {
		t.Fatalf("QueryMessages: %v", err)
	}
	if len(page.Messages) != 2 {
		t.Fatalf("acme saw %d messages, want 2: %+v", len(page.Messages), page.Messages)
	}
	for _, m := range page.Messages {
		if m.Project != "acme" {
			t.Errorf("acme saw a message owned by %q", m.Project)
		}
	}
}

// A pre-project row (empty project) belongs to nobody, and an empty
// scope must not become a wildcard over it.
func TestProjectScope_RefusesEmptyName(t *testing.T) {
	if _, err := ProjectScope(""); err == nil {
		t.Error("empty project name accepted as a scope")
	}
	if _, err := ProjectScope("   "); err == nil {
		t.Error("whitespace project name accepted as a scope")
	}
}

// TestQueryMessage_ForeignIDIsNotFound: a foreign message must be
// indistinguishable from a nonexistent one, or the endpoint becomes an
// oracle for guessing other tenants' ids.
func TestQueryMessage_ForeignIDIsNotFound(t *testing.T) {
	s := memStore(t)
	seedMessage(t, s, "b-1", "globex", qNow, nil)

	acme := projectScope(t, "acme")
	_, foreignOK, err := s.QueryMessage(acme, "b-1")
	if err != nil {
		t.Fatal(err)
	}
	_, missingOK, err := s.QueryMessage(acme, "no-such-id")
	if err != nil {
		t.Fatal(err)
	}
	if foreignOK {
		t.Error("acme read globex's message")
	}
	if foreignOK != missingOK {
		t.Error("a foreign id and a missing id answered differently")
	}
}

// Sub-resources are the easy place to forget the filter: message_attempts
// and delivery_events have no project column of their own.
func TestQuerySubResources_RespectScope(t *testing.T) {
	s := memStore(t)
	seedMessage(t, s, "b-1", "globex", qNow, nil)
	if err := s.RecordAttempt(Attempt{
		MessageID: "b-1", Attempt: 1, Provider: "postmark",
		StartedAt: qNow, FinishedAt: qNow, Result: AttemptSent,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordDeliveryEvent(DeliveryEventRow{
		EventID: "ev-1", MessageID: "b-1", Project: "globex",
		Provider: "postmark", Type: "message.delivered", OccurredAt: qNow,
	}); err != nil {
		t.Fatal(err)
	}

	acme := projectScope(t, "acme")
	if _, ok, err := s.QueryAttempts(acme, "b-1"); err != nil || ok {
		t.Errorf("acme read globex's attempts (ok=%v, err=%v)", ok, err)
	}
	if _, ok, err := s.QueryEvents(acme, "b-1"); err != nil || ok {
		t.Errorf("acme read globex's events (ok=%v, err=%v)", ok, err)
	}

	globex := projectScope(t, "globex")
	attempts, ok, err := s.QueryAttempts(globex, "b-1")
	if err != nil || !ok || len(attempts) != 1 {
		t.Errorf("owner could not read its own attempts: ok=%v err=%v n=%d", ok, err, len(attempts))
	}
	events, ok, err := s.QueryEvents(globex, "b-1")
	if err != nil || !ok || len(events) != 1 {
		t.Errorf("owner could not read its own events: ok=%v err=%v n=%d", ok, err, len(events))
	}
}

// TestAdminScope_CrossesProjects: the one deliberate bypass works, and
// is reachable only by naming it.
func TestAdminScope_CrossesProjects(t *testing.T) {
	s := memStore(t)
	seedMessage(t, s, "a-1", "acme", qNow, nil)
	seedMessage(t, s, "b-1", "globex", qNow.Add(time.Minute), nil)

	page, err := s.QueryMessages(AdminScope(), MessageFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Messages) != 2 {
		t.Errorf("admin saw %d messages, want 2", len(page.Messages))
	}
	if _, ok, _ := s.QueryMessage(AdminScope(), "b-1"); !ok {
		t.Error("admin could not read a message")
	}
}

// --- content exposure --------------------------------------------------

// TestQuery_NeverReturnsBody is structural, not cosmetic: the body
// columns are absent from every SELECT in this surface, so there is no
// masking to get wrong.
func TestQuery_NeverReturnsBody(t *testing.T) {
	s := memStore(t)
	seedMessage(t, s, "a-1", "acme", qNow, nil)
	acme := projectScope(t, "acme")

	page, err := s.QueryMessages(acme, MessageFilter{})
	if err != nil {
		t.Fatal(err)
	}
	one, _, err := s.QueryMessage(acme, "a-1")
	if err != nil {
		t.Fatal(err)
	}
	for _, blob := range []string{sprintAny(page), sprintAny(one)} {
		if strings.Contains(blob, "SECRET BODY CONTENT") {
			t.Errorf("message body reached the query surface: %s", blob)
		}
	}
}

// --- filters and pagination --------------------------------------------

func TestQueryMessages_Filters(t *testing.T) {
	s := memStore(t)
	seedMessage(t, s, "m-sent", "acme", qNow, nil)
	seedMessage(t, s, "m-failed", "acme", qNow.Add(time.Minute), func(sub *Submission) {
		sub.Status = StatusFailed
		sub.Transport = "resend"
		sub.MessageKind = "marketing"
		sub.ToAddrs = []string{"other@example.com"}
	})

	acme := projectScope(t, "acme")
	for _, tc := range []struct {
		name   string
		filter MessageFilter
		wantID string
	}{
		{"status", MessageFilter{Status: StatusFailed}, "m-failed"},
		{"provider", MessageFilter{Provider: "resend"}, "m-failed"},
		{"mail_kind", MessageFilter{MailKind: "marketing"}, "m-failed"},
		{"recipient", MessageFilter{Recipient: "other@example.com"}, "m-failed"},
		{"since", MessageFilter{Since: qNow.Add(30 * time.Second)}, "m-failed"},
		{"until", MessageFilter{Until: qNow.Add(30 * time.Second)}, "m-sent"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			page, err := s.QueryMessages(acme, tc.filter)
			if err != nil {
				t.Fatal(err)
			}
			if len(page.Messages) != 1 || page.Messages[0].ID != tc.wantID {
				t.Errorf("got %+v, want exactly %s", page.Messages, tc.wantID)
			}
		})
	}
}

// A recipient filter must match the whole address, not a substring:
// "user@example.com" must not match "other-user@example.com".
func TestQueryMessages_RecipientIsExact(t *testing.T) {
	s := memStore(t)
	seedMessage(t, s, "m-1", "acme", qNow, func(sub *Submission) {
		sub.ToAddrs = []string{"other-user@example.com"}
	})
	page, err := s.QueryMessages(projectScope(t, "acme"), MessageFilter{Recipient: "user@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Messages) != 0 {
		t.Errorf("substring match leaked a message: %+v", page.Messages)
	}
}

func TestQueryMessages_CursorPagination(t *testing.T) {
	s := memStore(t)
	const total = 25
	for i := 0; i < total; i++ {
		seedMessage(t, s, "m-"+itoa(i), "acme", qNow.Add(time.Duration(i)*time.Second), nil)
	}

	acme := projectScope(t, "acme")
	seen := map[string]bool{}
	cursor := ""
	pages := 0
	for {
		page, err := s.QueryMessages(acme, MessageFilter{Limit: 10, Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		pages++
		for _, m := range page.Messages {
			if seen[m.ID] {
				t.Errorf("%s returned on two pages", m.ID)
			}
			seen[m.ID] = true
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
		if pages > 10 {
			t.Fatal("pagination did not terminate")
		}
	}
	if len(seen) != total {
		t.Errorf("saw %d of %d messages across %d pages", len(seen), total, pages)
	}
}

// Rows sharing a timestamp are the case a created_at-only cursor gets
// wrong — it either repeats them or skips them.
func TestQueryMessages_CursorHandlesTimestampTies(t *testing.T) {
	s := memStore(t)
	for i := 0; i < 6; i++ {
		seedMessage(t, s, "tie-"+itoa(i), "acme", qNow, nil)
	}
	acme := projectScope(t, "acme")
	seen := map[string]bool{}
	cursor := ""
	for {
		page, err := s.QueryMessages(acme, MessageFilter{Limit: 2, Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range page.Messages {
			if seen[m.ID] {
				t.Fatalf("%s repeated across pages of tied timestamps", m.ID)
			}
			seen[m.ID] = true
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	if len(seen) != 6 {
		t.Errorf("saw %d of 6 messages with identical timestamps", len(seen))
	}
}

func TestQueryMessages_CursorIsOpaqueAndValidated(t *testing.T) {
	s := memStore(t)
	seedMessage(t, s, "m-1", "acme", qNow, nil)
	acme := projectScope(t, "acme")

	// A cursor is not a place to smuggle SQL or another project's name.
	for _, bad := range []string{"not-base64!!", "YWJj", "' OR 1=1 --"} {
		if _, err := s.QueryMessages(acme, MessageFilter{Cursor: bad}); err == nil {
			t.Errorf("cursor %q accepted", bad)
		}
	}
}

func TestQueryMessages_LimitIsClamped(t *testing.T) {
	s := memStore(t)
	for i := 0; i < 5; i++ {
		seedMessage(t, s, "m-"+itoa(i), "acme", qNow.Add(time.Duration(i)*time.Second), nil)
	}
	acme := projectScope(t, "acme")

	page, err := s.QueryMessages(acme, MessageFilter{Limit: 100000})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Messages) != 5 {
		t.Errorf("got %d messages", len(page.Messages))
	}
	if n, err := ParseLimit("100000"); err != nil || n != MaxPageSize {
		t.Errorf("ParseLimit(100000) = %d, %v; want it clamped to %d", n, err, MaxPageSize)
	}
	if _, err := ParseLimit("0"); err == nil {
		t.Error("ParseLimit(0) accepted")
	}
	if _, err := ParseLimit("abc"); err == nil {
		t.Error("ParseLimit(abc) accepted")
	}
}

// --- usage -------------------------------------------------------------

func TestQueryUsage_IsProjectScoped(t *testing.T) {
	s := memStore(t)
	if err := s.AddProjectUsage("acme", "2026-09-07", "2026-09", 3, 1); err != nil {
		t.Fatal(err)
	}
	if err := s.AddProjectUsage("globex", "2026-09-07", "2026-09", 99, 99); err != nil {
		t.Fatal(err)
	}

	rep, err := s.QueryUsage(projectScope(t, "acme"), "2026-09-07", "2026-09")
	if err != nil {
		t.Fatal(err)
	}
	if rep.Project != "acme" || rep.Day.Success != 3 || rep.Day.Failure != 1 {
		t.Errorf("usage = %+v, want acme's own numbers", rep)
	}
	if rep.Month.Success != 3 {
		t.Errorf("month usage = %+v", rep.Month)
	}

	// Usage has no cross-project meaning, so the admin scope is refused
	// rather than silently summing every tenant together.
	if _, err := s.QueryUsage(AdminScope(), "2026-09-07", "2026-09"); err == nil {
		t.Error("admin scope accepted for a per-project usage report")
	}
}

func itoa(i int) string { return strconv.Itoa(i) }

// sprintAny renders a value the way the HTTP layer will: if a body
// cannot appear in the JSON, it cannot leak through the API either.
func sprintAny(v any) string {
	buf, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%+v", v)
	}
	return string(buf)
}
