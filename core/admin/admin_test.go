package admin

import (
	"context"
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
	"github.com/craigmccaskill/posthorn/transport"
)

const (
	adminToken   = "admin-token-0123456789abcdef"
	projectKey   = "ph_live_0123456789abcdef0123456789"
	otherProject = "ph_live_globex_0123456789abcdef012"
)

var adminNow = time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)

type harness struct {
	h        *Handler
	store    *storage.Store
	pool     *transport.Pool
	projects *project.Registry
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	store, err := storage.Open(storage.Config{InMemory: true})
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	projects, err := project.Build([]project.Config{
		{Name: "acme", Enabled: true, APIKey: projectKey, AllowedFromDomains: []string{"example.com"}},
		{Name: "globex", Enabled: true, APIKey: otherProject, AllowedFromDomains: []string{"globex.test"}},
	})
	if err != nil {
		t.Fatalf("project.Build: %v", err)
	}

	pool, err := transport.NewPool([]*transport.Provider{{
		Name: "postmark-main", Type: "postmark", Weight: 1,
		Transport: stubTransport{}, Breaker: transport.NewBreaker(transport.BreakerConfig{}),
	}})
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}

	auth, err := NewAuthenticator(adminToken, false)
	if err != nil {
		t.Fatalf("NewAuthenticator: %v", err)
	}
	auth.now = func() time.Time { return adminNow }

	h := New(Deps{
		Auth: auth, Store: store, Pool: pool, Projects: projects,
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		Version: "v1.0.0-test", Now: func() time.Time { return adminNow },
	})
	return &harness{h: h, store: store, pool: pool, projects: projects}
}

// stubTransport satisfies transport.Transport; the admin surface never
// sends, it only reports on providers.
type stubTransport struct{}

func (stubTransport) Send(context.Context, transport.Message) (transport.SendResult, error) {
	return transport.SendResult{}, nil
}

func (x *harness) seed(t *testing.T, id, projectName, status string) {
	t.Helper()
	if err := x.store.RecordSubmission(storage.Submission{
		ID: id, Project: projectName, Endpoint: "/api/contact", Transport: "postmark-main",
		From: "noreply@example.com", ToAddrs: []string{"very.specific.person@example.com"},
		Subject: "Subject " + id, BodyText: "SECRET BODY " + id,
		BodyHTML: "<p>SECRET BODY</p>", Status: status, CreatedAt: adminNow,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
}

// bearer performs a request with the admin bearer token.
func (x *harness) bearer(t *testing.T, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, nil)
	r.Header.Set("Authorization", "Bearer "+adminToken)
	rec := httptest.NewRecorder()
	x.h.ServeHTTP(rec, r)
	return rec
}

func (x *harness) anon(t *testing.T, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, nil)
	rec := httptest.NewRecorder()
	x.h.ServeHTTP(rec, r)
	return rec
}

// login exchanges the token for a session and returns cookie + CSRF token.
func (x *harness) login(t *testing.T) (*http.Cookie, string) {
	t.Helper()
	form := url.Values{"token": {adminToken}}
	r := httptest.NewRequest(http.MethodPost, "/admin/login", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	x.h.ServeHTTP(rec, r)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("login status = %d: %s", rec.Code, rec.Body.String())
	}
	cookies := rec.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatal("login set no cookie")
	}
	c := cookies[0]
	csrfToken, ok := x.h.deps.Auth.ValidSession(c.Value)
	if !ok {
		t.Fatal("session not valid after login")
	}
	return c, csrfToken
}

// --- authentication -----------------------------------------------------

func TestAdmin_EveryAPIRouteRequiresAuth(t *testing.T) {
	x := newHarness(t)
	x.seed(t, "m-1", "acme", storage.StatusSent)

	for _, path := range []string{
		"/admin/api/overview", "/admin/api/providers", "/admin/api/projects",
		"/admin/api/messages", "/admin/api/messages/m-1",
		"/admin/api/messages/m-1/attempts", "/admin/api/messages/m-1/events",
		"/admin/api/queue", "/admin/api/dead-letters", "/admin/api/suppressions",
		"/admin/api/audit", "/admin/api/usage",
	} {
		if rec := x.anon(t, http.MethodGet, path); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s unauthenticated = %d, want 401", path, rec.Code)
		}
	}
}

// A PROJECT key is not an admin credential. This is the separation the
// whole surface depends on.
func TestAdmin_ProjectKeyIsNotAdminCredential(t *testing.T) {
	x := newHarness(t)
	r := httptest.NewRequest(http.MethodGet, "/admin/api/overview", nil)
	r.Header.Set("Authorization", "Bearer "+projectKey)
	rec := httptest.NewRecorder()
	x.h.ServeHTTP(rec, r)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("a project key reached the admin API: %d", rec.Code)
	}
}

// VerifyDistinct refuses a deployment where the two credentials collide.
func TestAdmin_VerifyDistinctRefusesSharedCredential(t *testing.T) {
	projects, err := project.Build([]project.Config{{
		Name: "acme", Enabled: true, APIKey: adminToken,
		AllowedFromDomains: []string{"example.com"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	auth, err := NewAuthenticator(adminToken, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyDistinct(auth, projects); err == nil {
		t.Fatal("a shared admin/project credential was accepted")
	}

	// The distinct case must still start.
	distinct, err := project.Build([]project.Config{{
		Name: "acme", Enabled: true, APIKey: projectKey,
		AllowedFromDomains: []string{"example.com"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyDistinct(auth, distinct); err != nil {
		t.Errorf("distinct credentials rejected: %v", err)
	}
}

func TestAdmin_ShortTokenRefused(t *testing.T) {
	if _, err := NewAuthenticator("short", false); err == nil {
		t.Error("a short admin token was accepted")
	}
	if _, err := NewAuthenticator("", false); err == nil {
		t.Error("an empty admin token was accepted")
	}
}

func TestAdmin_LoginThrottlesGuessing(t *testing.T) {
	x := newHarness(t)
	for i := 0; i < loginFailureBudget; i++ {
		if _, err := x.h.deps.Auth.Login("10.0.0.1", "wrong-token-guess-000000"); err == nil {
			t.Fatal("a wrong token logged in")
		}
	}
	// Even the CORRECT token is refused while locked out — otherwise
	// the limit only slows down the last guess.
	if _, err := x.h.deps.Auth.Login("10.0.0.1", adminToken); err != ErrLockedOut {
		t.Errorf("err = %v, want ErrLockedOut", err)
	}
	// A different source is unaffected.
	if _, err := x.h.deps.Auth.Login("10.0.0.2", adminToken); err != nil {
		t.Errorf("an unrelated source was locked out: %v", err)
	}
}

// --- sessions and CSRF ---------------------------------------------------

func TestAdmin_SessionCookieAttributes(t *testing.T) {
	x := newHarness(t)
	c, _ := x.login(t)

	if !c.HttpOnly {
		t.Error("session cookie is readable by script")
	}
	if c.SameSite != http.SameSiteStrictMode {
		t.Errorf("SameSite = %v, want Strict", c.SameSite)
	}
	if c.Path != "/" {
		t.Errorf("Path = %q", c.Path)
	}
	if c.Value == adminToken {
		t.Fatal("the session cookie IS the admin token")
	}
	if strings.Contains(c.Value, adminToken) {
		t.Fatal("the session id embeds the admin token")
	}
}

// Over TLS the cookie takes the __Host- prefix, which browsers refuse
// to accept unless it is Secure, host-only and path=/.
func TestAdmin_SecureDeploymentUsesHostPrefix(t *testing.T) {
	auth, err := NewAuthenticator(adminToken, true)
	if err != nil {
		t.Fatal(err)
	}
	if auth.cookieName() != SessionCookie || !strings.HasPrefix(auth.cookieName(), "__Host-") {
		t.Errorf("secure cookie name = %q", auth.cookieName())
	}
	rec := httptest.NewRecorder()
	auth.setSessionCookie(rec, "session-id")
	c := rec.Result().Cookies()[0]
	if !c.Secure {
		t.Error("secure deployment did not set Secure")
	}
}

// TestAdmin_StateChangingRequestNeedsCSRF: a session cookie travels
// automatically, so a form on another site could otherwise disable a
// provider on the operator's behalf.
func TestAdmin_StateChangingRequestNeedsCSRF(t *testing.T) {
	x := newHarness(t)
	c, csrfToken := x.login(t)

	post := func(withToken bool, token string) int {
		form := url.Values{
			"action": {"provider_disable"}, "target": {"postmark-main"}, "back": {"/admin/providers"},
		}
		if withToken {
			form.Set("csrf_token", token)
		}
		r := httptest.NewRequest(http.MethodPost, "/admin/action", strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.AddCookie(c)
		rec := httptest.NewRecorder()
		x.h.ServeHTTP(rec, r)
		return rec.Code
	}

	if code := post(false, ""); code != http.StatusForbidden {
		t.Errorf("no CSRF token = %d, want 403", code)
	}
	p, _ := x.pool.Get("postmark-main")
	if p.AdminDisabled() {
		t.Fatal("a CSRF-less request disabled a provider")
	}
	if code := post(true, "wrong-token"); code != http.StatusForbidden {
		t.Errorf("wrong CSRF token = %d, want 403", code)
	}
	if code := post(true, csrfToken); code != http.StatusSeeOther {
		t.Errorf("valid CSRF token = %d, want 303", code)
	}
	if !p.AdminDisabled() {
		t.Error("the authorised request did not disable the provider")
	}
}

// A bearer-authenticated request needs no CSRF token: nothing attaches
// an Authorization header automatically.
func TestAdmin_BearerOpsSkipCSRF(t *testing.T) {
	x := newHarness(t)
	rec := x.bearer(t, http.MethodPost, "/admin/api/providers/postmark-main/disable")
	if rec.Code != http.StatusOK {
		t.Fatalf("bearer op = %d: %s", rec.Code, rec.Body.String())
	}
	p, _ := x.pool.Get("postmark-main")
	if !p.AdminDisabled() {
		t.Error("the provider was not disabled")
	}
}

func TestAdmin_LogoutInvalidatesSession(t *testing.T) {
	x := newHarness(t)
	c, csrfToken := x.login(t)

	form := url.Values{"csrf_token": {csrfToken}}
	r := httptest.NewRequest(http.MethodPost, "/admin/logout", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.AddCookie(c)
	rec := httptest.NewRecorder()
	x.h.ServeHTTP(rec, r)

	if _, ok := x.h.deps.Auth.ValidSession(c.Value); ok {
		t.Error("session survived logout")
	}
}

func TestAdmin_SessionExpires(t *testing.T) {
	x := newHarness(t)
	c, _ := x.login(t)
	if _, ok := x.h.deps.Auth.ValidSession(c.Value); !ok {
		t.Fatal("session invalid immediately after login")
	}
	x.h.deps.Auth.now = func() time.Time { return adminNow.Add(SessionTTL + time.Minute) }
	if _, ok := x.h.deps.Auth.ValidSession(c.Value); ok {
		t.Error("an expired session still authenticates")
	}
}

// --- what the surface shows ---------------------------------------------

// TestAdmin_NeverShowsMessageBodies: the operator running the gateway
// is not the owner of the mail passing through it.
func TestAdmin_NeverShowsMessageBodies(t *testing.T) {
	x := newHarness(t)
	x.seed(t, "m-1", "acme", storage.StatusSent)
	c, _ := x.login(t)

	for _, path := range []string{
		"/admin/api/messages", "/admin/api/messages/m-1",
		"/admin", "/admin/messages", "/admin/messages/m-1",
	} {
		rec := x.bearer(t, http.MethodGet, path)
		if rec.Code == http.StatusSeeOther {
			// HTML pages want a session rather than a bearer token.
			r := httptest.NewRequest(http.MethodGet, path, nil)
			r.AddCookie(c)
			rec = httptest.NewRecorder()
			x.h.ServeHTTP(rec, r)
		}
		if rec.Code != http.StatusOK {
			t.Fatalf("%s = %d: %s", path, rec.Code, rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), "SECRET BODY") {
			t.Errorf("%s showed message content", path)
		}
	}
}

// TestAdmin_MasksRecipients: an admin surface listing every recipient
// is a harvestable directory of every user of every tenant.
func TestAdmin_MasksRecipients(t *testing.T) {
	x := newHarness(t)
	x.seed(t, "m-1", "acme", storage.StatusSent)
	c, _ := x.login(t)

	check := func(path, body string) {
		if strings.Contains(body, "very.specific.person@example.com") {
			t.Errorf("%s showed a full recipient address", path)
		}
		if !strings.Contains(body, "v***@example.com") {
			t.Errorf("%s did not show the masked address: %s", path, body)
		}
	}
	rec := x.bearer(t, http.MethodGet, "/admin/api/messages")
	check("/admin/api/messages", rec.Body.String())

	r := httptest.NewRequest(http.MethodGet, "/admin/messages", nil)
	r.AddCookie(c)
	page := httptest.NewRecorder()
	x.h.ServeHTTP(page, r)
	check("/admin/messages", page.Body.String())
}

// TestAdmin_NeverReturnsSecrets: no credential, in any form, on any
// route. The project view carries a fingerprint of a DIGEST, which is
// not the key and cannot become it.
func TestAdmin_NeverReturnsSecrets(t *testing.T) {
	x := newHarness(t)
	x.seed(t, "m-1", "acme", storage.StatusSent)

	for _, path := range []string{
		"/admin/api/overview", "/admin/api/providers", "/admin/api/projects",
		"/admin/api/messages", "/admin/api/audit",
	} {
		body := x.bearer(t, http.MethodGet, path).Body.String()
		for _, secret := range []string{adminToken, projectKey, otherProject} {
			if strings.Contains(body, secret) {
				t.Errorf("%s leaked a credential", path)
			}
		}
	}

	// And the project view specifically: a fingerprint, never a key.
	var payload struct {
		Projects []projectView `json:"projects"`
	}
	if err := json.Unmarshal(x.bearer(t, http.MethodGet, "/admin/api/projects").Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Projects) != 2 {
		t.Fatalf("projects = %+v", payload.Projects)
	}
	for _, p := range payload.Projects {
		if p.APIKeyFingerprint == "" {
			t.Error("no key fingerprint shown; an operator cannot tell keys apart")
		}
		if len(p.APIKeyFingerprint) > 16 {
			t.Errorf("fingerprint %q is long enough to be a credential", p.APIKeyFingerprint)
		}
	}
}

// --- ops and audit -------------------------------------------------------

// TestAdmin_ActionsAreAudited: an action that leaves no trace is
// indistinguishable from one that never happened.
func TestAdmin_ActionsAreAudited(t *testing.T) {
	x := newHarness(t)
	x.seed(t, "dead-1", "acme", storage.StatusFailed)

	for _, tc := range []struct {
		path   string
		action string
		target string
	}{
		{"/admin/api/providers/postmark-main/disable", storage.AuditProviderDisabled, "postmark-main"},
		{"/admin/api/providers/postmark-main/enable", storage.AuditProviderEnabled, "postmark-main"},
		{"/admin/api/providers/postmark-main/reset-breaker", storage.AuditBreakerReset, "postmark-main"},
		{"/admin/api/projects/acme/disable", storage.AuditProjectDisabled, "acme"},
		{"/admin/api/projects/acme/enable", storage.AuditProjectEnabled, "acme"},
		{"/admin/api/dead-letters/dead-1/retry", storage.AuditDeadLetterRetry, "dead-1"},
	} {
		rec := x.bearer(t, http.MethodPost, tc.path)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s = %d: %s", tc.path, rec.Code, rec.Body.String())
		}
		entries, err := x.store.ListAudit(50)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, e := range entries {
			if e.Action == tc.action && e.Target == tc.target {
				found = true
				if e.Actor != storage.ActorAdmin {
					t.Errorf("%s recorded actor %q", tc.action, e.Actor)
				}
			}
		}
		if !found {
			t.Errorf("%s wrote no audit row", tc.path)
		}
	}
}

// The audit row must carry no credential, including for the action
// whose target IS an address.
func TestAdmin_AuditCarriesNoSecretsOrAddresses(t *testing.T) {
	x := newHarness(t)
	if err := x.store.AddSuppression("very.specific.person@example.com", "bounce", "", adminNow); err != nil {
		t.Fatal(err)
	}
	rec := x.bearer(t, http.MethodPost,
		"/admin/api/suppressions/very.specific.person@example.com/remove")
	if rec.Code != http.StatusOK {
		t.Fatalf("unsuppress = %d: %s", rec.Code, rec.Body.String())
	}
	entries, err := x.store.ListAudit(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 {
		t.Fatal("no audit row for the unsuppress")
	}
	for _, e := range entries {
		if strings.Contains(e.Target, "very.specific.person") {
			t.Errorf("audit target carried the full address: %q", e.Target)
		}
	}
	if entries[0].Target != "v***@example.com" {
		t.Errorf("audit target = %q, want the masked address", entries[0].Target)
	}

	// And the suppression is actually gone.
	if _, found, err := x.store.SuppressionFor("very.specific.person@example.com"); err != nil || found {
		t.Errorf("suppression still present: found=%v err=%v", found, err)
	}
}

func TestAdmin_DisableSurvivesAsAnOverrideRow(t *testing.T) {
	x := newHarness(t)
	if rec := x.bearer(t, http.MethodPost, "/admin/api/providers/postmark-main/disable"); rec.Code != http.StatusOK {
		t.Fatalf("disable = %d", rec.Code)
	}
	overrides, err := x.store.ListOverrides()
	if err != nil {
		t.Fatal(err)
	}
	if len(overrides) != 1 || overrides[0].Name != "postmark-main" {
		t.Fatalf("overrides = %+v, want the disable persisted", overrides)
	}

	if rec := x.bearer(t, http.MethodPost, "/admin/api/providers/postmark-main/enable"); rec.Code != http.StatusOK {
		t.Fatalf("enable = %d", rec.Code)
	}
	overrides, err = x.store.ListOverrides()
	if err != nil {
		t.Fatal(err)
	}
	if len(overrides) != 0 {
		t.Errorf("overrides = %+v, want the disable cleared", overrides)
	}
}

func TestAdmin_UnknownTargetIsNotFound(t *testing.T) {
	x := newHarness(t)
	for _, path := range []string{
		"/admin/api/providers/no-such-provider/disable",
		"/admin/api/projects/no-such-project/disable",
		"/admin/api/dead-letters/no-such-message/retry",
	} {
		rec := x.bearer(t, http.MethodPost, path)
		if rec.Code == http.StatusOK {
			t.Errorf("%s succeeded against a missing target", path)
		}
	}
}

// --- redirects and headers ----------------------------------------------

// An open redirect on an authenticated surface is a phishing primitive.
func TestAdmin_ActionRedirectStaysInAdmin(t *testing.T) {
	x := newHarness(t)
	c, csrfToken := x.login(t)

	form := url.Values{
		"csrf_token": {csrfToken}, "action": {"breaker_reset"},
		"target": {"postmark-main"}, "back": {"https://evil.example.com/phish"},
	}
	r := httptest.NewRequest(http.MethodPost, "/admin/action", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.AddCookie(c)
	rec := httptest.NewRecorder()
	x.h.ServeHTTP(rec, r)

	loc := rec.Header().Get("Location")
	if !strings.HasPrefix(loc, "/admin") {
		t.Errorf("Location = %q, want an /admin path", loc)
	}
}

func TestAdmin_SecurityHeaders(t *testing.T) {
	x := newHarness(t)
	rec := x.bearer(t, http.MethodGet, "/admin/api/overview")
	for header, want := range map[string]string{
		"Cache-Control":          "no-store",
		"X-Frame-Options":        "DENY",
		"X-Content-Type-Options": "nosniff",
		"Referrer-Policy":        "no-referrer",
	} {
		if got := rec.Header().Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}
}

// The UI loads no script of any origin, so a stored value cannot become
// executable content.
func TestAdmin_PagesSetCSPAndCarryNoScript(t *testing.T) {
	x := newHarness(t)
	c, _ := x.login(t)
	for _, path := range []string{"/admin", "/admin/providers", "/admin/projects", "/admin/messages", "/admin/audit"} {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.AddCookie(c)
		rec := httptest.NewRecorder()
		x.h.ServeHTTP(rec, r)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s = %d: %s", path, rec.Code, rec.Body.String())
		}
		if csp := rec.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'none'") {
			t.Errorf("%s CSP = %q", path, csp)
		}
		if strings.Contains(strings.ToLower(rec.Body.String()), "<script") {
			t.Errorf("%s contains a script tag", path)
		}
	}
}

// A value that arrived from a provider payload must render as text.
func TestAdmin_HTMLEscapesStoredValues(t *testing.T) {
	x := newHarness(t)
	if err := x.store.RecordSubmission(storage.Submission{
		ID: "xss-1", Project: "acme", Endpoint: "/api/contact", Transport: "postmark-main",
		From: "noreply@example.com", ToAddrs: []string{"user@example.com"},
		Subject:  `<script>alert(1)</script>`,
		Status:   storage.StatusFailed,
		BodyText: "b", CreatedAt: adminNow,
		LastError: `<img src=x onerror=alert(1)>`,
	}); err != nil {
		t.Fatal(err)
	}
	c, _ := x.login(t)
	r := httptest.NewRequest(http.MethodGet, "/admin/messages/xss-1", nil)
	r.AddCookie(c)
	rec := httptest.NewRecorder()
	x.h.ServeHTTP(rec, r)

	body := rec.Body.String()
	if strings.Contains(body, "<script>alert(1)</script>") {
		t.Error("a stored subject rendered as live markup")
	}
	if strings.Contains(body, "<img src=x") {
		t.Error("a stored error string rendered as live markup")
	}
	if !strings.Contains(body, "&lt;script&gt;") {
		t.Errorf("the subject was not escaped into the page: %s", body)
	}
}

// --- pages require a session --------------------------------------------

func TestAdmin_PagesRedirectToLogin(t *testing.T) {
	x := newHarness(t)
	for _, path := range []string{"/admin", "/admin/providers", "/admin/projects", "/admin/messages", "/admin/audit"} {
		rec := x.anon(t, http.MethodGet, path)
		if rec.Code != http.StatusSeeOther {
			t.Errorf("%s unauthenticated = %d, want a redirect to login", path, rec.Code)
		}
		if loc := rec.Header().Get("Location"); loc != "/admin/login" {
			t.Errorf("%s redirected to %q", path, loc)
		}
	}
}
