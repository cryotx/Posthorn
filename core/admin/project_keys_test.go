package admin

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

type fakeProjectKeys struct{ created, rotated string }

func (f *fakeProjectKeys) Create(name string) (string, error) {
	f.created = name
	return "generated-secret-for-test", nil
}
func (f *fakeProjectKeys) Rotate(name string) (string, error) {
	f.rotated = name
	return "new-secret-for-test", nil
}
func (f *fakeProjectKeys) IsManaged(name string) bool { return name == "acme" }

func TestProjectKeyFormsRequireAuthAndCSRFAndShowSecretOnce(t *testing.T) {
	x := newHarness(t)
	manager := &fakeProjectKeys{}
	x.h.deps.ProjectKeys = manager
	if got := x.anon(t, http.MethodPost, "/admin/projects/create").Code; got != http.StatusUnauthorized {
		t.Fatalf("anonymous status = %d", got)
	}
	cookie, csrf := x.login(t)
	post := func(path string, vals url.Values) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(vals.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.AddCookie(cookie)
		rec := httptest.NewRecorder()
		x.h.ServeHTTP(rec, r)
		return rec
	}
	if got := post("/admin/projects/create", url.Values{"name": {"app"}}).Code; got != http.StatusForbidden {
		t.Fatalf("missing CSRF status = %d", got)
	}
	rec := post("/admin/projects/create", url.Values{"name": {"app"}, "csrf_token": {csrf}})
	if rec.Code != http.StatusOK || manager.created != "app" || !strings.Contains(rec.Body.String(), "generated-secret-for-test") {
		t.Fatalf("create failed: %d %s", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "" {
		t.Fatalf("secret could leak in redirect %q", loc)
	}
	r := httptest.NewRequest(http.MethodGet, "/admin/projects", nil)
	r.AddCookie(cookie)
	list := httptest.NewRecorder()
	x.h.ServeHTTP(list, r)
	if strings.Contains(list.Body.String(), "generated-secret-for-test") {
		t.Fatal("secret persisted in project page")
	}
	if got := post("/admin/projects/acme/rotate", url.Values{"csrf_token": {csrf}}).Code; got != http.StatusBadRequest {
		t.Fatalf("unconfirmed rotation status = %d", got)
	}
	rec = post("/admin/projects/acme/rotate", url.Values{"csrf_token": {csrf}, "confirm": {"yes"}})
	if rec.Code != http.StatusOK || manager.rotated != "acme" || !strings.Contains(rec.Body.String(), "new-secret-for-test") {
		t.Fatalf("rotation failed: %d %s", rec.Code, rec.Body.String())
	}
}
