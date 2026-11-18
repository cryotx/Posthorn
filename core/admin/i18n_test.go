package admin

import (
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
)

// --- catalog completeness ------------------------------------------------

// keyUsageRE extracts every catalog key referenced from html.go, in
// either `{{t .Lang "key"}}` or `{{t $.Lang "key"}}` form (the two forms
// templates need depending on whether "." is already the page root or
// has been shadowed by a {{range}}).
var keyUsageRE = regexp.MustCompile(`t \$?\.Lang "([^"]+)"`)

// titleKeyRE catches the OTHER way a key reaches a template: ui.go sets
// pageData.TitleKey to a literal string, and html.go's <title> resolves
// it dynamically via {{t .Lang .TitleKey}} — a key name that is itself
// a template variable, not a literal, so keyUsageRE cannot see it.
var titleKeyRE = regexp.MustCompile(`TitleKey:\s*"([^"]+)"`)

// usedKeys returns every catalog key reachable from the templates,
// combining both discovery methods above. Both orphan and coverage
// checks call this so neither drifts from the other.
func usedKeys(t *testing.T) map[string]bool {
	t.Helper()
	used := map[string]bool{}
	for _, m := range keyUsageRE.FindAllStringSubmatch(uiHTML, -1) {
		used[m[1]] = true
	}
	uiGoSrc, err := os.ReadFile("ui.go")
	if err != nil {
		t.Fatalf("read ui.go: %v", err)
	}
	for _, m := range titleKeyRE.FindAllStringSubmatch(string(uiGoSrc), -1) {
		used[m[1]] = true
	}
	return used
}

// TestCatalog_EveryUsedKeyExistsInBothLanguages is the test that matters
// most for a hand-written table this size: every key the templates
// actually call must resolve in EN and in ZH. Without this, a typo in
// either the template or the catalog falls back to English (or to the
// deliberately-ugly "[key]") silently — this test turns that into a
// build-time-adjacent failure instead of something only noticed by
// eyeballing a rendered page in the other language.
func TestCatalog_EveryUsedKeyExistsInBothLanguages(t *testing.T) {
	used := usedKeys(t)
	if len(used) < 50 {
		t.Fatalf("only found %d key usages; the extraction regexes are probably broken", len(used))
	}
	for key := range used {
		if _, ok := catalog[langEN][key]; !ok {
			t.Errorf("template uses %q, missing from the English catalog", key)
		}
		if _, ok := catalog[langZH][key]; !ok {
			t.Errorf("template uses %q, missing from the Chinese catalog", key)
		}
	}
	t.Logf("checked %d distinct translation keys", len(used))
}

// TestCatalog_NoOrphanedKeys is the mirror check: a key sitting in the
// catalog that no template references is either a leftover from a
// rename (silently stale) or evidence the extraction regex missed a
// real usage (which would make the test above pass falsely). Catalog
// keys not meant to be template-referenced directly (there are none
// today) would need an allowlist here if that ever changes.
func TestCatalog_NoOrphanedKeys(t *testing.T) {
	used := usedKeys(t)
	for key := range catalog[langEN] {
		if !used[key] {
			t.Errorf("catalog key %q (en) is not referenced by any template", key)
		}
	}
	for key := range catalog[langZH] {
		if !used[key] {
			t.Errorf("catalog key %q (zh) is not referenced by any template", key)
		}
	}
}

// Every English and Chinese entry for the SAME key should exist —
// asymmetric coverage (a key present in one language's map but absent
// from the other) is exactly the case that silently triggers the
// English fallback the file comment warns about.
func TestCatalog_LanguagesHaveMatchingKeySets(t *testing.T) {
	for key := range catalog[langEN] {
		if _, ok := catalog[langZH][key]; !ok {
			t.Errorf("key %q has an English entry but no Chinese one", key)
		}
	}
	for key := range catalog[langZH] {
		if _, ok := catalog[langEN][key]; !ok {
			t.Errorf("key %q has a Chinese entry but no English one", key)
		}
	}
}

// --- t() lookup behaviour -------------------------------------------------

func TestT_ResolvesBothLanguages(t *testing.T) {
	if got := translate(langEN, "nav.dashboard"); got != "Dashboard" {
		t.Errorf("t(en, nav.dashboard) = %q", got)
	}
	if got := translate(langZH, "nav.dashboard"); got != "仪表盘" {
		t.Errorf("t(zh, nav.dashboard) = %q", got)
	}
}

// A missing key must render as something UNMISTAKABLY not real copy —
// silently returning "" would be indistinguishable from an intentionally
// blank label.
func TestT_MissingKeyIsObviouslyWrong(t *testing.T) {
	got := translate(langEN, "this.key.does.not.exist")
	if got != "[this.key.does.not.exist]" {
		t.Errorf("t(missing) = %q, want the bracketed key", got)
	}
}

// --- language resolution ---------------------------------------------------

func TestResolveLang_QueryParamWins(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/admin?lang=zh", nil)
	r.Header.Set("Accept-Language", "en-US,en;q=0.9")
	r.AddCookie(&http.Cookie{Name: langCookieName, Value: "en"})

	lang, fromQuery := resolveLang(r)
	if lang != langZH || !fromQuery {
		t.Errorf("lang=%q fromQuery=%v, want zh/true", lang, fromQuery)
	}
}

func TestResolveLang_CookieBeatsAcceptLanguage(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/admin", nil)
	r.Header.Set("Accept-Language", "zh-CN,zh;q=0.9")
	r.AddCookie(&http.Cookie{Name: langCookieName, Value: "en"})

	lang, fromQuery := resolveLang(r)
	if lang != langEN || fromQuery {
		t.Errorf("lang=%q fromQuery=%v, want en/false (cookie should win over Accept-Language)", lang, fromQuery)
	}
}

func TestResolveLang_AcceptLanguageSniffing(t *testing.T) {
	for _, tc := range []struct {
		accept string
		want   uiLang
	}{
		{"zh-CN,zh;q=0.9,en;q=0.8", langZH},
		{"zh", langZH},
		{"ZH-tw", langZH}, // case-insensitive
		{"en-US,en;q=0.9", langEN},
		{"", langEN},
		{"fr-FR", langEN}, // unsupported language falls back to English, not blank
	} {
		r := httptest.NewRequest(http.MethodGet, "/admin", nil)
		if tc.accept != "" {
			r.Header.Set("Accept-Language", tc.accept)
		}
		lang, fromQuery := resolveLang(r)
		if lang != tc.want || fromQuery {
			t.Errorf("Accept-Language=%q → lang=%q fromQuery=%v, want %q/false", tc.accept, lang, fromQuery, tc.want)
		}
	}
}

// An invalid/forged cookie value must not crash or silently pick a
// language other than the documented fallback chain.
func TestResolveLang_InvalidCookieFallsThrough(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/admin", nil)
	r.AddCookie(&http.Cookie{Name: langCookieName, Value: "fr"})
	r.Header.Set("Accept-Language", "zh")

	lang, _ := resolveLang(r)
	if lang != langZH {
		t.Errorf("an invalid cookie value should fall through to Accept-Language sniffing, got %q", lang)
	}
}

// --- end-to-end through the handler ---------------------------------------

func TestAdmin_LanguageSwitchEndToEnd(t *testing.T) {
	x := newHarness(t)
	c, _ := x.login(t)

	// Default (no lang cookie, no Accept-Language): English.
	r := httptest.NewRequest(http.MethodGet, "/admin", nil)
	r.AddCookie(c)
	rec := httptest.NewRecorder()
	x.h.ServeHTTP(rec, r)
	if !strings.Contains(rec.Body.String(), ">Dashboard<") {
		t.Errorf("default render is not English:\n%s", rec.Body.String())
	}

	// Switch to Chinese via the query param.
	r2 := httptest.NewRequest(http.MethodGet, "/admin?lang=zh", nil)
	r2.AddCookie(c)
	rec2 := httptest.NewRecorder()
	x.h.ServeHTTP(rec2, r2)
	if !strings.Contains(rec2.Body.String(), "仪表盘") {
		t.Errorf("?lang=zh did not render Chinese:\n%s", rec2.Body.String())
	}
	var langCookie *http.Cookie
	for _, ck := range rec2.Result().Cookies() {
		if ck.Name == langCookieName {
			langCookie = ck
		}
	}
	if langCookie == nil || langCookie.Value != "zh" {
		t.Fatal("switching language did not set the persistence cookie")
	}

	// The NEXT request, with no ?lang= but carrying the cookie the
	// previous response set, must stay in Chinese.
	r3 := httptest.NewRequest(http.MethodGet, "/admin/providers", nil)
	r3.AddCookie(c)
	r3.AddCookie(langCookie)
	rec3 := httptest.NewRecorder()
	x.h.ServeHTTP(rec3, r3)
	if !strings.Contains(rec3.Body.String(), "服务商") {
		t.Errorf("language did not persist via cookie on a later request:\n%s", rec3.Body.String())
	}
}

// The two toggle links must exist and correctly mark whichever
// language is currently active — an operator needs to see which one
// they're on, not just have two identical-looking links.
func TestAdmin_LanguageToggleLinksMarkCurrentLanguage(t *testing.T) {
	x := newHarness(t)
	c, _ := x.login(t)

	r := httptest.NewRequest(http.MethodGet, "/admin?lang=zh", nil)
	r.AddCookie(c)
	rec := httptest.NewRecorder()
	x.h.ServeHTTP(rec, r)
	body := rec.Body.String()

	if !strings.Contains(body, `href="?lang=en"`) || !strings.Contains(body, `href="?lang=zh"`) {
		t.Fatal("both language toggle links must be present regardless of current language")
	}
	// The zh link should carry class="current" somewhere near it; a
	// crude but sufficient check given both plain strings appear once
	// each in the chrome.
	zhIdx := strings.Index(body, `href="?lang=zh"`)
	enIdx := strings.Index(body, `href="?lang=en"`)
	zhSnippet := body[zhIdx : zhIdx+40]
	enSnippet := body[enIdx : enIdx+40]
	if !strings.Contains(zhSnippet, "current") {
		t.Errorf("zh toggle not marked current: %q", zhSnippet)
	}
	if strings.Contains(enSnippet, "current") {
		t.Errorf("en toggle incorrectly marked current: %q", enSnippet)
	}
}

// Raw operator/provider content must never be routed through t() by
// accident — it stays literal regardless of language. This guards the
// scope boundary the file comment states: chrome is translated,
// content is not.
func TestAdmin_RawContentUnaffectedByLanguage(t *testing.T) {
	x := newHarness(t)
	x.seed(t, "m-1", "acme", "sent")
	c, _ := x.login(t)

	for _, lang := range []string{"en", "zh"} {
		r := httptest.NewRequest(http.MethodGet, "/admin/messages?lang="+lang, nil)
		r.AddCookie(c)
		rec := httptest.NewRecorder()
		x.h.ServeHTTP(rec, r)
		if !strings.Contains(rec.Body.String(), "postmark-main") {
			t.Errorf("lang=%s: provider name (raw content) missing or altered", lang)
		}
	}
}
