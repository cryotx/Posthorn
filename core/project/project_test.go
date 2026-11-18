package project

import (
	"strings"
	"testing"
)

const (
	novelKey   = "novel-key-0123456789abcdef"
	accountKey = "account-key-0123456789abcdef"
)

func novelCfg() Config {
	return Config{
		Name:               "novel",
		Enabled:            true,
		APIKey:             novelKey,
		DefaultFrom:        "noreply@novel.example.com",
		AllowedFromDomains: []string{"novel.example.com"},
	}
}

func accountCfg() Config {
	return Config{
		Name:               "account",
		Enabled:            true,
		APIKey:             accountKey,
		DefaultFrom:        "account@example.com",
		AllowedFromDomains: []string{"example.com"},
	}
}

func mustBuild(t *testing.T, cfgs ...Config) *Registry {
	t.Helper()
	r, err := Build(cfgs)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	return r
}

// --- construction ------------------------------------------------------

func TestBuild_Rejects(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  Config
		want string
	}{
		{"no name", Config{APIKey: novelKey, AllowedFromDomains: []string{"a.com"}}, "name is required"},
		{"short key", Config{Name: "n", APIKey: "short", AllowedFromDomains: []string{"a.com"}}, "at least 24 characters"},
		{"no domains", Config{Name: "n", APIKey: novelKey}, "allowed_from_domains"},
		{"address not domain", Config{Name: "n", APIKey: novelKey,
			AllowedFromDomains: []string{"a@b.com"}}, "not an address"},
		{"bare label", Config{Name: "n", APIKey: novelKey,
			AllowedFromDomains: []string{"localhost"}}, "not a domain name"},
		{"double wildcard", Config{Name: "n", APIKey: novelKey,
			AllowedFromDomains: []string{"*.*.com"}}, "leading"},
		{"smtp password only", Config{Name: "n", APIKey: novelKey,
			AllowedFromDomains: []string{"a.com"}, SMTPPassword: "x"}, "must be set together"},
		{"weak webhook secret", Config{Name: "n", APIKey: novelKey,
			AllowedFromDomains: []string{"a.com"}, WebhookURL: "https://x", WebhookSecret: "short"}, "webhook_secret"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Build([]Config{tc.cfg})
			if err == nil {
				t.Fatalf("Build = nil error, want %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

// A default_from outside the project's own allowlist would be a back
// door around the exact policy the block exists to enforce.
func TestBuild_RejectsDefaultFromOutsideAllowlist(t *testing.T) {
	cfg := novelCfg()
	cfg.DefaultFrom = "noreply@other-project.com"
	_, err := Build([]Config{cfg})
	if err == nil || !strings.Contains(err.Error(), "default_from") {
		t.Errorf("error = %v, want a default_from policy error", err)
	}
}

func TestBuild_RejectsDuplicateNameAndKey(t *testing.T) {
	if _, err := Build([]Config{novelCfg(), novelCfg()}); err == nil {
		t.Error("duplicate project name accepted")
	}
	second := accountCfg()
	second.APIKey = novelKey
	_, err := Build([]Config{novelCfg(), second})
	if err == nil || !strings.Contains(err.Error(), "identical to one used by project") {
		t.Errorf("error = %v, want a shared-key error", err)
	}
}

// --- authentication ----------------------------------------------------

func TestAuthenticate(t *testing.T) {
	r := mustBuild(t, novelCfg(), accountCfg())

	p, err := r.Authenticate(novelKey)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if p.Name != "novel" {
		t.Errorf("project = %q, want novel", p.Name)
	}

	if _, err := r.Authenticate(accountKey); err != nil {
		t.Errorf("second project key rejected: %v", err)
	}
}

func TestAuthenticate_WrongKey(t *testing.T) {
	r := mustBuild(t, novelCfg())
	for _, bad := range []string{"", "wrong", novelKey + "x", novelKey[:len(novelKey)-1]} {
		if _, err := r.Authenticate(bad); err == nil {
			t.Errorf("Authenticate(%q) = nil error, want rejection", bad)
		}
	}
}

func TestAuthenticate_DisabledProject(t *testing.T) {
	cfg := novelCfg()
	cfg.Enabled = false
	r := mustBuild(t, cfg)

	_, err := r.Authenticate(novelKey)
	if err == nil {
		t.Fatal("disabled project authenticated")
	}
	ae, ok := err.(*AuthError)
	if !ok || ae.Reason != "disabled" {
		t.Fatalf("error = %v, want a disabled AuthError", err)
	}
	// Naming the project is safe here: the credential was valid, and
	// the operator needs to know which one is suspended.
	if ae.Project != "novel" {
		t.Errorf("AuthError.Project = %q, want novel", ae.Project)
	}
}

func TestAuthenticate_EmptyRegistry(t *testing.T) {
	r := mustBuild(t)
	if _, err := r.Authenticate(novelKey); err != ErrNoProjects {
		t.Errorf("error = %v, want ErrNoProjects", err)
	}
}

// NFR44: the plaintext key must not survive construction anywhere a
// dump, a trace, or a future admin surface could reach it.
func TestAuthenticate_NeverRetainsPlaintextKey(t *testing.T) {
	r := mustBuild(t, novelCfg())
	p, _ := r.Authenticate(novelKey)

	// Nothing on the struct's exported surface carries the key.
	for _, field := range []string{p.Name, p.DefaultFrom, p.WebhookSecret, p.WebhookURL} {
		if strings.Contains(field, novelKey) {
			t.Errorf("API key leaked into an exported field: %q", field)
		}
	}
	// And an auth error never echoes the credential.
	_, err := r.Authenticate("some-attacker-supplied-value-here")
	if err != nil && strings.Contains(err.Error(), "some-attacker-supplied-value-here") {
		t.Errorf("auth error echoed the presented credential: %v", err)
	}
}

func TestAuthenticateSMTP(t *testing.T) {
	cfg := novelCfg()
	cfg.SMTPUsername = "novel"
	cfg.SMTPPassword = "novel-smtp-secret-0123456789"
	r := mustBuild(t, cfg)

	if !r.HasSMTPIdentities() {
		t.Fatal("HasSMTPIdentities = false")
	}
	p, err := r.AuthenticateSMTP("novel", "novel-smtp-secret-0123456789")
	if err != nil {
		t.Fatalf("AuthenticateSMTP: %v", err)
	}
	if p.Name != "novel" {
		t.Errorf("project = %q", p.Name)
	}
	if _, err := r.AuthenticateSMTP("novel", "wrong"); err == nil {
		t.Error("wrong SMTP password accepted")
	}
	if _, err := r.AuthenticateSMTP("nobody", "novel-smtp-secret-0123456789"); err == nil {
		t.Error("unknown SMTP username accepted")
	}
}

// HTTP and SMTP must resolve to the SAME project object, not two
// parallel identity systems that could drift.
func TestAuthenticate_HTTPAndSMTPAgree(t *testing.T) {
	cfg := novelCfg()
	cfg.SMTPUsername = "novel"
	cfg.SMTPPassword = "novel-smtp-secret-0123456789"
	r := mustBuild(t, cfg)

	viaHTTP, _ := r.Authenticate(novelKey)
	viaSMTP, _ := r.AuthenticateSMTP("novel", "novel-smtp-secret-0123456789")
	if viaHTTP != viaSMTP {
		t.Error("HTTP and SMTP auth produced different project instances")
	}
}

// --- sender policy (P0) ------------------------------------------------

func TestResolveFrom(t *testing.T) {
	r := mustBuild(t, novelCfg())
	p, _ := r.ByName("novel")

	for _, tc := range []struct {
		name      string
		requested string
		fallback  string
		want      string
		wantErr   bool
	}{
		{"empty uses default", "", "", "noreply@novel.example.com", false},
		{"in-domain allowed", "support@novel.example.com", "", "support@novel.example.com", false},
		{"display name form", "Support <support@novel.example.com>", "", "Support <support@novel.example.com>", false},
		{"case insensitive domain", "a@NOVEL.example.COM", "", "a@NOVEL.example.COM", false},
		{"other project's domain rejected", "admin@other-project.com", "", "", true},
		{"bare TLD-ish rejected", "admin@example.com", "", "", true},
		{"not an address", "not-an-address", "", "", true},
		{"suffix trickery rejected", "a@evilnovel.example.com.attacker.test", "", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := p.ResolveFrom(tc.requested, tc.fallback)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ResolveFrom(%q) = %q, want rejection", tc.requested, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ResolveFrom(%q): %v", tc.requested, err)
			}
			if got != tc.want {
				t.Errorf("ResolveFrom(%q) = %q, want %q", tc.requested, got, tc.want)
			}
		})
	}
}

// The cross-project spoofing case, stated directly: novel must not be
// able to send as account's domain, in either direction.
func TestResolveFrom_CrossProjectSpoofingRejected(t *testing.T) {
	r := mustBuild(t, novelCfg(), accountCfg())
	novel, _ := r.ByName("novel")
	account, _ := r.ByName("account")

	if _, err := novel.ResolveFrom("account@example.com", ""); err == nil {
		t.Error("novel was allowed to send as account's domain")
	}
	if _, err := account.ResolveFrom("noreply@novel.example.com", ""); err == nil {
		t.Error("account was allowed to send as novel's domain")
	}
}

// The endpoint's own `from` is shared infrastructure and must not be a
// way around the allowlist.
func TestResolveFrom_FallbackStillChecked(t *testing.T) {
	cfg := novelCfg()
	cfg.DefaultFrom = ""
	r := mustBuild(t, cfg)
	p, _ := r.ByName("novel")

	if _, err := p.ResolveFrom("", "shared@other-project.com"); err == nil {
		t.Error("an endpoint's from bypassed the project allowlist")
	}
	got, err := p.ResolveFrom("", "ok@novel.example.com")
	if err != nil {
		t.Fatalf("in-domain fallback rejected: %v", err)
	}
	if got != "ok@novel.example.com" {
		t.Errorf("got %q", got)
	}
}

func TestResolveFrom_NoSenderAnywhere(t *testing.T) {
	cfg := novelCfg()
	cfg.DefaultFrom = ""
	r := mustBuild(t, cfg)
	p, _ := r.ByName("novel")

	_, err := p.ResolveFrom("", "")
	if err == nil || !strings.Contains(err.Error(), "no sender address") {
		t.Errorf("error = %v, want a no-sender error", err)
	}
}

func TestAllowsFrom_Wildcard(t *testing.T) {
	cfg := novelCfg()
	cfg.AllowedFromDomains = []string{"*.novel.example.com"}
	cfg.DefaultFrom = "a@mail.novel.example.com"
	r := mustBuild(t, cfg)
	p, _ := r.ByName("novel")

	for _, tc := range []struct {
		addr string
		want bool
	}{
		{"a@mail.novel.example.com", true},
		{"a@eu.mail.novel.example.com", true},
		{"a@novel.example.com", true},
		{"a@novel.example.com.evil.test", false},
		{"a@example.com", false},
	} {
		if got := p.AllowsFrom(tc.addr); got != tc.want {
			t.Errorf("AllowsFrom(%q) = %v, want %v", tc.addr, got, tc.want)
		}
	}
}

func TestRegistry_Accessors(t *testing.T) {
	disabled := accountCfg()
	disabled.Enabled = false
	r := mustBuild(t, novelCfg(), disabled)

	if r.Len() != 2 || r.Enabled() != 1 {
		t.Errorf("Len=%d Enabled=%d, want 2 and 1", r.Len(), r.Enabled())
	}
	names := r.Names()
	if len(names) != 2 || names[0] != "account" || names[1] != "novel" {
		t.Errorf("Names = %v, want sorted", names)
	}
	if _, ok := r.ByName("nope"); ok {
		t.Error("ByName found a project that does not exist")
	}
}

// A nil registry is the non-project mode every existing deployment
// runs, so every accessor must tolerate it.
func TestRegistry_NilIsSafe(t *testing.T) {
	var r *Registry
	if r.Len() != 0 || r.Enabled() != 0 || r.Names() != nil || r.All() != nil {
		t.Error("nil registry accessors misbehaved")
	}
	if r.HasSMTPIdentities() {
		t.Error("nil registry claims SMTP identities")
	}
	if _, ok := r.ByName("x"); ok {
		t.Error("nil registry resolved a name")
	}
	if _, err := r.Authenticate("x"); err != ErrNoProjects {
		t.Errorf("error = %v, want ErrNoProjects", err)
	}
	if _, err := r.AuthenticateSMTP("x", "y"); err != ErrNoProjects {
		t.Errorf("error = %v, want ErrNoProjects", err)
	}
}

// --- admin kill switch (FR144) ------------------------------------------

// TestAdminDisable_StopsBothIngresses: suspending a project must stop
// its HTTP key and its SMTP identity at the same instant, or a tenant
// "suspended" in the UI keeps sending through the other door.
func TestAdminDisable_StopsBothIngresses(t *testing.T) {
	const key = "ph_live_0123456789abcdef0123456789"
	const smtpPass = "smtp-password-0123456789"
	reg, err := Build([]Config{{
		Name: "acme", Enabled: true, APIKey: key,
		AllowedFromDomains: []string{"example.com"},
		SMTPUsername:       "acme", SMTPPassword: smtpPass,
	}})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	p, ok := reg.ByName("acme")
	if !ok {
		t.Fatal("project not found")
	}

	if _, err := reg.Authenticate(key); err != nil {
		t.Fatalf("baseline HTTP auth failed: %v", err)
	}
	if _, err := reg.AuthenticateSMTP("acme", smtpPass); err != nil {
		t.Fatalf("baseline SMTP auth failed: %v", err)
	}

	p.SetAdminDisabled(true)
	if _, err := reg.Authenticate(key); err == nil {
		t.Error("HTTP key still works for a suspended project")
	}
	if _, err := reg.AuthenticateSMTP("acme", smtpPass); err == nil {
		t.Error("SMTP identity still works for a suspended project")
	}

	p.SetAdminDisabled(false)
	if _, err := reg.Authenticate(key); err != nil {
		t.Errorf("restoring the project did not restore its key: %v", err)
	}
}

// The kill switch can only SUBTRACT: clearing it restores the config's
// answer, it does not enable a project the config disabled.
func TestAdminDisable_CannotEnableAConfigDisabledProject(t *testing.T) {
	const key = "ph_live_0123456789abcdef0123456789"
	reg, err := Build([]Config{{
		Name: "acme", Enabled: false, APIKey: key,
		AllowedFromDomains: []string{"example.com"},
	}})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	p, _ := reg.ByName("acme")

	p.SetAdminDisabled(false)
	if _, err := reg.Authenticate(key); err == nil {
		t.Error("clearing the kill switch enabled a project the config disabled")
	}
}

// --- key rotation (FR141) -----------------------------------------------

// TestRotation_BothKeysAuthenticate is the whole mechanism: during a
// rotation the old and new keys are both in the config, so every caller
// keeps working while they migrate. No downtime window, and no runtime
// mutation surface.
func TestRotation_BothKeysAuthenticate(t *testing.T) {
	const oldKey = "ph_live_old_0123456789abcdef0123"
	const newKey = "ph_live_new_0123456789abcdef0123"
	reg, err := Build([]Config{{
		Name: "acme", Enabled: true, APIKey: oldKey, APIKeys: []string{newKey},
		AllowedFromDomains: []string{"example.com"},
	}})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	for _, key := range []string{oldKey, newKey} {
		p, err := reg.Authenticate(key)
		if err != nil {
			t.Fatalf("key %q did not authenticate: %v", key, err)
		}
		if p.Name != "acme" {
			t.Errorf("key resolved to %q", p.Name)
		}
	}
	if p, _ := reg.ByName("acme"); p.KeyCount() != 2 {
		t.Errorf("KeyCount = %d, want 2 during a rotation", p.KeyCount())
	}

	// Retiring the old key: it stops working, the new one continues.
	after, err := Build([]Config{{
		Name: "acme", Enabled: true, APIKey: newKey,
		AllowedFromDomains: []string{"example.com"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := after.Authenticate(oldKey); err == nil {
		t.Error("the retired key still authenticates")
	}
	if _, err := after.Authenticate(newKey); err != nil {
		t.Errorf("the surviving key stopped working: %v", err)
	}
}

// A rotation must change the key-set digest, which is what makes the
// startup audit entry possible.
func TestRotation_ChangesTheKeySetDigest(t *testing.T) {
	const oldKey = "ph_live_old_0123456789abcdef0123"
	const newKey = "ph_live_new_0123456789abcdef0123"
	base := Config{Name: "acme", Enabled: true, APIKey: oldKey,
		AllowedFromDomains: []string{"example.com"}}

	before, _ := Build([]Config{base})
	rotating := base
	rotating.APIKeys = []string{newKey}
	during, _ := Build([]Config{rotating})
	after := base
	after.APIKey = newKey
	done, _ := Build([]Config{after})

	b, _ := before.ByName("acme")
	d, _ := during.ByName("acme")
	f, _ := done.ByName("acme")
	if b.KeySetDigest() == d.KeySetDigest() {
		t.Error("adding a second key did not change the key-set digest")
	}
	if d.KeySetDigest() == f.KeySetDigest() {
		t.Error("retiring a key did not change the key-set digest")
	}
	// And the digest is not the key.
	for _, digest := range []string{b.KeySetDigest(), d.KeySetDigest(), f.KeySetDigest()} {
		if strings.Contains(digest, oldKey) || strings.Contains(digest, newKey) {
			t.Error("the key-set digest embeds a key")
		}
	}
}

// The same value twice is a half-finished rotation, not two keys.
func TestRotation_RejectsDuplicateKeyWithinAProject(t *testing.T) {
	const key = "ph_live_0123456789abcdef0123456789"
	_, err := Build([]Config{{
		Name: "acme", Enabled: true, APIKey: key, APIKeys: []string{key},
		AllowedFromDomains: []string{"example.com"},
	}})
	if err == nil {
		t.Fatal("the same key twice was accepted as a rotation")
	}
	if !strings.Contains(err.Error(), "same value") {
		t.Errorf("error = %v, want it to explain the duplicate", err)
	}
}

// Every key in the set must meet the length floor: a rotation is not a
// way to smuggle a weak key in beside a strong one.
func TestRotation_EveryKeyMeetsTheLengthFloor(t *testing.T) {
	_, err := Build([]Config{{
		Name: "acme", Enabled: true,
		APIKey:             "ph_live_0123456789abcdef0123456789",
		APIKeys:            []string{"short"},
		AllowedFromDomains: []string{"example.com"},
	}})
	if err == nil {
		t.Fatal("a short key was accepted alongside a valid one")
	}
}

// Two PROJECTS may never share a key, including through a rotation set.
func TestRotation_KeysStayUniqueAcrossProjects(t *testing.T) {
	const shared = "ph_live_shared_0123456789abcdef01"
	_, err := Build([]Config{
		{Name: "acme", Enabled: true, APIKey: "ph_live_acme_0123456789abcdef012",
			APIKeys: []string{shared}, AllowedFromDomains: []string{"example.com"}},
		{Name: "globex", Enabled: true, APIKey: shared,
			AllowedFromDomains: []string{"globex.test"}},
	})
	if err == nil {
		t.Fatal("two projects shared a key through a rotation set")
	}
}
