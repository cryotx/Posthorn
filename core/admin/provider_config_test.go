package admin

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/craigmccaskill/posthorn/managedproviders"
)

type mockProviderManager struct {
	entries      []managedproviders.View
	saved        []managedproviders.Input
	enabledCalls int
}

func (m *mockProviderManager) List() ([]managedproviders.View, error) { return m.entries, nil }
func (m *mockProviderManager) Save(v managedproviders.Input) error {
	m.saved = append(m.saved, v)
	return nil
}
func (m *mockProviderManager) SetEnabled(_ string, _ bool) error { m.enabledCalls++; return nil }

func TestProviderConfigurationAuthCSRFAndSecretRedaction(t *testing.T) {
	x := newHarness(t)
	m := &mockProviderManager{entries: []managedproviders.View{{Name: "mine", Type: "resend", Enabled: true, Settings: map[string]string{"from": "sender@example.com", "api_key": "secret-should-not-appear"}}}}
	x.h.deps.ProviderManager = m
	if got := x.anon(t, http.MethodGet, "/admin/providers/configure?edit=mine"); got.Code != http.StatusSeeOther {
		t.Fatalf("unauthenticated status %d", got.Code)
	}
	cookie, csrf := x.login(t)
	r := httptest.NewRequest(http.MethodGet, "/admin/providers/configure?edit=mine", nil)
	r.AddCookie(cookie)
	rec := httptest.NewRecorder()
	x.h.ServeHTTP(rec, r)
	if rec.Code != 200 || strings.Contains(rec.Body.String(), "secret-should-not-appear") || !strings.Contains(rec.Body.String(), "sender@example.com") {
		t.Fatalf("unsafe or incomplete edit page: %d", rec.Code)
	}
	values := url.Values{"name": {"mine"}, "type": {"resend"}, "from": {"sender@example.com"}, "api_key": {"new-secret"}, "daily_limit": {"100"}, "monthly_limit": {"3000"}}
	post := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/admin/providers/configure", strings.NewReader(values.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.AddCookie(cookie)
		out := httptest.NewRecorder()
		x.h.ServeHTTP(out, req)
		return out
	}
	if got := post(); got.Code != http.StatusForbidden || len(m.saved) != 0 {
		t.Fatalf("CSRF allowed save: %d", got.Code)
	}
	values.Set("csrf_token", csrf)
	if got := post(); got.Code != http.StatusSeeOther || len(m.saved) != 1 {
		t.Fatalf("save failed: %d", got.Code)
	}
	if !m.saved[0].Enabled || m.saved[0].Settings["api_key"] != "new-secret" {
		t.Fatal("save did not preserve state or pass credential")
	}
}

func TestProviderSMTPUsernameIsNotRendered(t *testing.T) {
	x := newHarness(t)
	x.h.deps.ProviderManager = &mockProviderManager{entries: []managedproviders.View{{Name: "smtp", Type: "smtp", Settings: map[string]string{"from": "sender@example.com", "host": "mail.example.com", "port": "587", "username": "secret-username", "password": "secret-password"}}}}
	cookie, _ := x.login(t)
	r := httptest.NewRequest(http.MethodGet, "/admin/providers/configure?edit=smtp", nil)
	r.AddCookie(cookie)
	rec := httptest.NewRecorder()
	x.h.ServeHTTP(rec, r)
	body := rec.Body.String()
	if rec.Code != http.StatusOK || strings.Contains(body, "secret-username") || strings.Contains(body, "secret-password") {
		t.Fatalf("SMTP credentials exposed or page failed: %d", rec.Code)
	}
	if !strings.Contains(body, `type="password" name="username"`) {
		t.Fatal("SMTP username is not a secret field")
	}
}

func TestMailgunRegionSelectionAndSave(t *testing.T) {
	x := newHarness(t)
	m := &mockProviderManager{entries: []managedproviders.View{{Name: "mailgun-eu", Type: "mailgun", Settings: map[string]string{"from": "sender@example.com", "domain": "example.com", "mailgun_region": "eu"}}}}
	x.h.deps.ProviderManager = m
	cookie, csrf := x.login(t)
	for _, tc := range []struct{ path, selected string }{
		{"/admin/providers/configure?type=mailgun", `value="us" selected`},
		{"/admin/providers/configure?edit=mailgun-eu", `value="eu" selected`},
	} {
		r := httptest.NewRequest(http.MethodGet, tc.path, nil)
		r.AddCookie(cookie)
		rec := httptest.NewRecorder()
		x.h.ServeHTTP(rec, r)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), tc.selected) {
			t.Fatalf("region not selected for %s: %d", tc.path, rec.Code)
		}
	}
	values := url.Values{"csrf_token": {csrf}, "name": {"mailgun-eu"}, "type": {"mailgun"}, "from": {"sender@example.com"}, "domain": {"example.com"}, "mailgun_region": {"eu"}, "api_key": {""}, "daily_limit": {"0"}, "monthly_limit": {"0"}}
	r := httptest.NewRequest(http.MethodPost, "/admin/providers/configure", strings.NewReader(values.Encode()))
	r.AddCookie(cookie)
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	x.h.ServeHTTP(rec, r)
	if rec.Code != http.StatusSeeOther || len(m.saved) != 1 || m.saved[0].Settings["mailgun_region"] != "eu" {
		t.Fatal("region was not passed on save")
	}
}
