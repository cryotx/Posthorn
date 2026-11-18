package queryapi

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/craigmccaskill/posthorn/project"
	"github.com/craigmccaskill/posthorn/storage"
)

var qNow = time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)

const (
	acmeKey   = "ph_live_acme_0123456789abcdef0123456789"
	globexKey = "ph_live_globex_0123456789abcdef012345"
)

// testHandler builds a handler over an in-memory store with two
// projects — one tenant is never enough to prove isolation.
func testHandler(t *testing.T) (*Handler, *storage.Store) {
	t.Helper()
	store, err := storage.Open(storage.Config{InMemory: true})
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	reg, err := project.Build([]project.Config{
		{Name: "acme", Enabled: true, APIKey: acmeKey, AllowedFromDomains: []string{"example.com"}},
		{Name: "globex", Enabled: true, APIKey: globexKey, AllowedFromDomains: []string{"globex.test"}},
	})
	if err != nil {
		t.Fatalf("project.Build: %v", err)
	}

	h := New(Deps{
		Store:    store,
		Projects: reg,
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:      func() time.Time { return qNow },
	})
	return h, store
}

func seed(t *testing.T, s *storage.Store, id, projectName string, at time.Time) {
	t.Helper()
	if err := s.RecordSubmission(storage.Submission{
		ID: id, Project: projectName, Endpoint: "/api/contact", Transport: "postmark",
		From: "noreply@example.com", ToAddrs: []string{"user@example.com"},
		Subject: "Subject " + id, BodyText: "SECRET BODY " + id,
		BodyHTML: "<p>SECRET BODY " + id + "</p>",
		Status:   storage.StatusSent, MessageKind: "notification", CreatedAt: at,
	}); err != nil {
		t.Fatalf("RecordSubmission: %v", err)
	}
}

func get(t *testing.T, h *Handler, path, key string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, path, nil)
	if key != "" {
		r.Header.Set("Authorization", "Bearer "+key)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

// --- authentication ----------------------------------------------------

func TestAuth_RequiredOnEveryRoute(t *testing.T) {
	h, store := testHandler(t)
	seed(t, store, "a-1", "acme", qNow)

	for _, path := range []string{
		"/v1/messages",
		"/v1/messages/a-1",
		"/v1/messages/a-1/attempts",
		"/v1/messages/a-1/events",
		"/v1/usage",
	} {
		if rec := get(t, h, path, ""); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s without a key = %d, want 401", path, rec.Code)
		}
		if rec := get(t, h, path, "ph_live_not_a_real_key_000000000000"); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s with a bad key = %d, want 401", path, rec.Code)
		}
	}
}

// A disabled project's valid credential must not read anything, and
// must not be distinguishable from an unknown one.
func TestAuth_DisabledProjectIsRejected(t *testing.T) {
	store, err := storage.Open(storage.Config{InMemory: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	reg, err := project.Build([]project.Config{
		{Name: "acme", Enabled: false, APIKey: acmeKey, AllowedFromDomains: []string{"example.com"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	h := New(Deps{Store: store, Projects: reg})

	rec := get(t, h, "/v1/messages", acmeKey)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("disabled project = %d, want 401", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "disabled") {
		t.Errorf("the response distinguished disabled from unknown: %s", rec.Body.String())
	}
}

// --- isolation ---------------------------------------------------------

// TestIsolation_ListIsScopedToTheCredential is the headline requirement:
// project A must never see project B.
func TestIsolation_ListIsScopedToTheCredential(t *testing.T) {
	h, store := testHandler(t)
	seed(t, store, "a-1", "acme", qNow)
	seed(t, store, "a-2", "acme", qNow.Add(time.Minute))
	seed(t, store, "b-1", "globex", qNow.Add(2*time.Minute))

	rec := get(t, h, "/v1/messages", acmeKey)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var page storage.MessagePage
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode: %v\n%s", err, rec.Body.String())
	}
	if len(page.Messages) != 2 {
		t.Fatalf("acme saw %d messages, want 2", len(page.Messages))
	}
	for _, m := range page.Messages {
		if m.Project != "acme" {
			t.Errorf("acme saw a message owned by %q", m.Project)
		}
	}
	if strings.Contains(rec.Body.String(), "b-1") {
		t.Errorf("globex's message id appeared in acme's listing: %s", rec.Body.String())
	}
}

// TestIsolation_ClientCannotChooseItsScope: a `project` parameter is
// not a fact about who is calling. Supplying one must change nothing.
func TestIsolation_ClientCannotChooseItsScope(t *testing.T) {
	h, store := testHandler(t)
	seed(t, store, "b-1", "globex", qNow)

	for _, path := range []string{
		"/v1/messages?project=globex",
		"/v1/messages?project=",
		"/v1/messages?scope=globex",
	} {
		rec := get(t, h, path, acmeKey)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status = %d", path, rec.Code)
		}
		if strings.Contains(rec.Body.String(), "b-1") {
			t.Errorf("%s let acme read globex's rows: %s", path, rec.Body.String())
		}
	}
}

func TestIsolation_ForeignMessageIsNotFound(t *testing.T) {
	h, store := testHandler(t)
	seed(t, store, "b-1", "globex", qNow)

	foreign := get(t, h, "/v1/messages/b-1", acmeKey)
	missing := get(t, h, "/v1/messages/does-not-exist", acmeKey)
	if foreign.Code != http.StatusNotFound {
		t.Errorf("foreign message = %d, want 404", foreign.Code)
	}
	if foreign.Code != missing.Code || foreign.Body.String() != missing.Body.String() {
		t.Error("a foreign message and a missing one answered differently — that is an id oracle")
	}
}

func TestIsolation_SubResources(t *testing.T) {
	h, store := testHandler(t)
	seed(t, store, "b-1", "globex", qNow)
	if err := store.RecordAttempt(storage.Attempt{
		MessageID: "b-1", Attempt: 1, Provider: "postmark",
		StartedAt: qNow, FinishedAt: qNow, Result: storage.AttemptSent,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RecordDeliveryEvent(storage.DeliveryEventRow{
		EventID: "ev-1", MessageID: "b-1", Project: "globex",
		Provider: "postmark", Type: "message.delivered", OccurredAt: qNow,
	}); err != nil {
		t.Fatal(err)
	}

	for _, path := range []string{"/v1/messages/b-1/attempts", "/v1/messages/b-1/events"} {
		if rec := get(t, h, path, acmeKey); rec.Code != http.StatusNotFound {
			t.Errorf("acme read %s: %d %s", path, rec.Code, rec.Body.String())
		}
		if rec := get(t, h, path, globexKey); rec.Code != http.StatusOK {
			t.Errorf("owner could not read %s: %d %s", path, rec.Code, rec.Body.String())
		}
	}
}

func TestUsage_IsScopedToTheCredential(t *testing.T) {
	h, store := testHandler(t)
	if err := store.AddProjectUsage("acme", "2026-09-07", "2026-09", 4, 1); err != nil {
		t.Fatal(err)
	}
	if err := store.AddProjectUsage("globex", "2026-09-07", "2026-09", 99, 99); err != nil {
		t.Fatal(err)
	}

	rec := get(t, h, "/v1/usage", acmeKey)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var report storage.UsageReport
	if err := json.Unmarshal(rec.Body.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.Project != "acme" || report.Day.Success != 4 || report.Day.Failure != 1 {
		t.Errorf("usage = %+v, want acme's own numbers", report)
	}
	if strings.Contains(rec.Body.String(), "99") {
		t.Errorf("globex's numbers appeared in acme's usage: %s", rec.Body.String())
	}
}

// --- content -----------------------------------------------------------

// TestNoBodyInAnyResponse: the API exposes the envelope and the state,
// never what the user wrote.
func TestNoBodyInAnyResponse(t *testing.T) {
	h, store := testHandler(t)
	seed(t, store, "a-1", "acme", qNow)

	for _, path := range []string{
		"/v1/messages",
		"/v1/messages/a-1",
		"/v1/messages/a-1/attempts",
		"/v1/messages/a-1/events",
	} {
		rec := get(t, h, path, acmeKey)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", path, rec.Code, rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), "SECRET BODY") {
			t.Errorf("%s returned message content: %s", path, rec.Body.String())
		}
	}
}

// --- method and shape --------------------------------------------------

// The API is read-only, and that is enforced by the routing table
// rather than by a check each handler has to remember.
func TestReadOnly_NonGETIsRejected(t *testing.T) {
	h, store := testHandler(t)
	seed(t, store, "a-1", "acme", qNow)

	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		r := httptest.NewRequest(method, "/v1/messages", nil)
		r.Header.Set("Authorization", "Bearer "+acmeKey)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s /v1/messages = %d, want 405", method, rec.Code)
		}
	}
}

func TestListMessages_FiltersAndPagination(t *testing.T) {
	h, store := testHandler(t)
	for i := 0; i < 5; i++ {
		seed(t, store, "a-"+string(rune('1'+i)), "acme", qNow.Add(time.Duration(i)*time.Minute))
	}

	rec := get(t, h, "/v1/messages?limit=2", acmeKey)
	var page storage.MessagePage
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Messages) != 2 || page.NextCursor == "" {
		t.Fatalf("page = %+v, want 2 messages and a cursor", page)
	}

	next := get(t, h, "/v1/messages?limit=2&cursor="+page.NextCursor, acmeKey)
	var page2 storage.MessagePage
	if err := json.Unmarshal(next.Body.Bytes(), &page2); err != nil {
		t.Fatal(err)
	}
	if len(page2.Messages) != 2 {
		t.Errorf("second page = %+v", page2.Messages)
	}
	if page2.Messages[0].ID == page.Messages[0].ID {
		t.Error("the second page repeated the first")
	}

	// Filters that produce nothing return an empty list, not an error.
	empty := get(t, h, "/v1/messages?status=failed", acmeKey)
	if empty.Code != http.StatusOK || !strings.Contains(empty.Body.String(), `"messages":[]`) {
		t.Errorf("empty filter result = %d %s", empty.Code, empty.Body.String())
	}
}

func TestListMessages_RejectsBadInput(t *testing.T) {
	h, _ := testHandler(t)
	for _, path := range []string{
		"/v1/messages?limit=abc",
		"/v1/messages?limit=0",
		"/v1/messages?since=yesterday",
		"/v1/messages?until=nope",
		"/v1/messages?cursor=%21%21not-base64",
	} {
		if rec := get(t, h, path, acmeKey); rec.Code != http.StatusBadRequest {
			t.Errorf("%s = %d, want 400 (%s)", path, rec.Code, rec.Body.String())
		}
	}
}

// SQL injection through every filter: the values are parameterised, so
// a quote is data. The assertion is that nothing errors and nothing
// extra comes back.
func TestFilters_AreParameterised(t *testing.T) {
	h, store := testHandler(t)
	seed(t, store, "a-1", "acme", qNow)
	seed(t, store, "b-1", "globex", qNow)

	for _, injection := range []string{
		"' OR 1=1 --",
		"'; DROP TABLE submissions; --",
		"\" OR \"\"=\"",
		"acme' UNION SELECT * FROM submissions --",
	} {
		for _, param := range []string{"status", "provider", "mail_kind", "recipient"} {
			rec := get(t, h, "/v1/messages?"+param+"="+urlEscape(injection), acmeKey)
			if rec.Code != http.StatusOK {
				t.Fatalf("%s=%q → %d %s", param, injection, rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), `"messages":[]`) {
				t.Errorf("%s=%q returned rows: %s", param, injection, rec.Body.String())
			}
		}
	}

	// The tables are still there afterwards.
	if _, err := store.QueryMessages(storage.AdminScope(), storage.MessageFilter{}); err != nil {
		t.Fatalf("submissions table damaged: %v", err)
	}
}

// Responses must not be cached by anything in between: they are
// per-credential views.
func TestResponses_AreNotCacheable(t *testing.T) {
	h, store := testHandler(t)
	seed(t, store, "a-1", "acme", qNow)
	rec := get(t, h, "/v1/messages", acmeKey)
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
}

func urlEscape(s string) string { return url.QueryEscape(s) }
