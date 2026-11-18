package admin

import (
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
)

func TestCatalog_CoversEveryAdminLabel(t *testing.T) {
	keyUsage := regexp.MustCompile(`t \$?\.Lang "([^"]+)"`)
	titleKey := regexp.MustCompile(`TitleKey:\s*"([^"]+)"`)
	fieldKey := regexp.MustCompile(`"(field\.[a-z_]+)"`)
	flashKey := regexp.MustCompile(`\b(flash\.[a-z_]+)\b`)

	used := make(map[string]bool)
	collect := func(src string, pattern *regexp.Regexp) {
		for _, match := range pattern.FindAllStringSubmatch(src, -1) {
			used[match[1]] = true
		}
	}
	collect(uiHTML, keyUsage)
	for _, name := range []string{"ui.go", "provider_config.go"} {
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		collect(string(src), titleKey)
		collect(string(src), fieldKey)
		collect(string(src), flashKey)
	}

	if len(catalog) != len(uiLanguages) {
		t.Errorf("language picker has %d entries for %d catalogs", len(uiLanguages), len(catalog))
	}
	for _, option := range uiLanguages {
		stringsByKey := catalog[option.Code]
		for key := range used {
			if strings.TrimSpace(stringsByKey[key]) == "" {
				t.Errorf("%s: empty or missing translation for %q", option.Code, key)
			}
		}
		for key := range stringsByKey {
			if !used[key] {
				t.Errorf("%s: unused translation key %q", option.Code, key)
			}
		}
	}
}

// --- t() lookup behaviour -------------------------------------------------

func TestT_ResolvesEveryLanguage(t *testing.T) {
	for lang, want := range map[uiLang]string{
		langEN: "Dashboard",
		langZH: "仪表盘",
		langJA: "ダッシュボード",
		langKO: "대시보드",
		langES: "Panel",
	} {
		if got := translate(lang, "nav.dashboard"); got != want {
			t.Errorf("translate(%s, nav.dashboard) = %q, want %q", lang, got, want)
		}
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
		{"ja-JP", langJA},
		{"ko-KR", langKO},
		{"es-MX", langES},
		{"fr-CA, es-ES;q=0.7, ja;q=0.9", langJA},
		{"zh;q=0, en;q=0.1, ko;q=0.8", langKO},
		{"ja;q=2, es;q=0.4", langES},
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

func TestAdmin_LanguageToggleKeepsQueryAndMarksCurrentLanguage(t *testing.T) {
	x := newHarness(t)
	c, _ := x.login(t)

	r := httptest.NewRequest(http.MethodGet, "/admin/messages?lang=ko&project=demo", nil)
	r.AddCookie(c)
	rec := httptest.NewRecorder()
	x.h.ServeHTTP(rec, r)
	body := rec.Body.String()

	if got := rec.Header().Get("Content-Language"); got != "ko" {
		t.Errorf("Content-Language = %q, want ko", got)
	}
	if !strings.Contains(body, `<html lang="ko">`) {
		t.Error("HTML document has no Korean language declaration")
	}
	for _, option := range uiLanguages {
		href := `href="?lang=` + string(option.Code) + `&amp;project=demo"`
		start := strings.Index(body, href)
		if start < 0 {
			t.Fatalf("language toggle missing or dropped the project filter: %s", option.Code)
		}
		link := strings.SplitN(body[start:], ">", 2)[0]
		if strings.Contains(link, `aria-current="true"`) != (option.Code == langKO) {
			t.Errorf("incorrect selection indicator for %s: %s", option.Code, link)
		}
	}
}

func TestAdmin_LoginMessagesUseSelectedLanguage(t *testing.T) {
	x := newHarness(t)
	for _, option := range uiLanguages {
		r := httptest.NewRequest(http.MethodGet, "/admin/login?error=flash.invalid_token&lang="+string(option.Code), nil)
		rec := httptest.NewRecorder()
		x.h.ServeHTTP(rec, r)
		if !strings.Contains(rec.Body.String(), translate(option.Code, "flash.invalid_token")) {
			t.Errorf("login error was not translated for %s", option.Code)
		}
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
