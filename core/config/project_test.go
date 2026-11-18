package config_test

import (
	"strings"
	"testing"
)

// projectTOML is the shape from the brief: two business projects
// sharing one gateway, each with its own key and sender policy.
const projectTOML = `
[[providers]]
name = "postmark-prod"
type = "postmark"
[providers.settings]
api_key = "pm-key"

[[projects]]
name = "novel"
api_key = "novel-key-0123456789abcdef"
default_from = "noreply@novel.example.com"
allowed_from_domains = ["novel.example.com"]

[[projects]]
name = "account"
api_key = "account-key-0123456789abcdef"
default_from = "account@example.com"
allowed_from_domains = ["example.com"]

[[endpoints]]
path = "/api/send"
to = ["fallback@example.com"]
from = "noreply@example.com"
subject = "S"
body = "B"
auth = "api-key"
api_keys = ["legacy-endpoint-key"]

[endpoints.transport]
providers = ["postmark-prod"]
`

func TestLoad_Projects(t *testing.T) {
	cfg, err := loadString(t, projectTOML)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Projects) != 2 {
		t.Fatalf("projects = %d, want 2", len(cfg.Projects))
	}
	novel := cfg.Projects[0]
	if novel.Name != "novel" || novel.DefaultFrom != "noreply@novel.example.com" {
		t.Errorf("project = %+v", novel)
	}
	if !novel.EffectiveEnabled() {
		t.Error("a project with no enabled key should default to enabled")
	}
	if len(cfg.ProjectConfigs()) != 2 {
		t.Errorf("ProjectConfigs = %d", len(cfg.ProjectConfigs()))
	}
}

func TestLoad_ProjectDisabled(t *testing.T) {
	toml := strings.Replace(projectTOML, `name = "novel"`, "name = \"novel\"\nenabled = false", 1)
	cfg, err := loadString(t, toml)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Projects[0].EffectiveEnabled() {
		t.Error("enabled = false was not honoured")
	}
}

func TestValidate_ProjectRules(t *testing.T) {
	for _, tc := range []struct{ mutate, want string }{
		{`api_key = "novel-key-0123456789abcdef"`, ""},
		{`api_key = "short"`, "at least 24 characters"},
		{`allowed_from_domains = ["novel.example.com"]`, ""},
	} {
		if tc.want == "" {
			continue
		}
		toml := strings.Replace(projectTOML, `api_key = "novel-key-0123456789abcdef"`, tc.mutate, 1)
		_, err := loadString(t, toml)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error = %v, want %q", tc.mutate, err, tc.want)
		}
	}
}

// The P0 control must be mandatory: a project that may send as any
// domain can impersonate every other project on the gateway.
func TestValidate_AllowedFromDomainsRequired(t *testing.T) {
	toml := strings.Replace(projectTOML,
		`allowed_from_domains = ["novel.example.com"]`, "", 1)
	_, err := loadString(t, toml)
	if err == nil || !strings.Contains(err.Error(), "allowed_from_domains") {
		t.Errorf("error = %v, want a required-allowlist error", err)
	}
}

func TestValidate_DefaultFromMustSatisfyAllowlist(t *testing.T) {
	toml := strings.Replace(projectTOML,
		`default_from = "noreply@novel.example.com"`,
		`default_from = "noreply@other-project.com"`, 1)
	_, err := loadString(t, toml)
	if err == nil || !strings.Contains(err.Error(), "default_from") {
		t.Errorf("error = %v, want a default_from policy error", err)
	}
}

func TestValidate_DuplicateProjectName(t *testing.T) {
	toml := strings.Replace(projectTOML, `name = "account"`, `name = "novel"`, 1)
	_, err := loadString(t, toml)
	if err == nil || !strings.Contains(err.Error(), "duplicate project name") {
		t.Errorf("error = %v, want a duplicate-name error", err)
	}
}

func TestValidate_SharedAPIKeyRejected(t *testing.T) {
	toml := strings.Replace(projectTOML,
		`api_key = "account-key-0123456789abcdef"`,
		`api_key = "novel-key-0123456789abcdef"`, 1)
	_, err := loadString(t, toml)
	if err == nil || !strings.Contains(err.Error(), "identical to one used by project") {
		t.Errorf("error = %v, want a shared-key error", err)
	}
}

// A webhook with nowhere to queue would drop events on the first
// receiver outage, which is the failure the queue exists to prevent.
func TestValidate_ProjectWebhookRequiresLifecycleAndStorage(t *testing.T) {
	toml := strings.Replace(projectTOML, `name = "novel"`,
		"name = \"novel\"\nwebhook_url = \"https://novel.example.com/hooks\"\nwebhook_secret = \"project-webhook-secret\"", 1)
	_, err := loadString(t, toml)
	if err == nil || !strings.Contains(err.Error(), "[lifecycle]") {
		t.Errorf("error = %v, want a lifecycle-required error", err)
	}

	withLifecycle := toml + `
[lifecycle]
basic_auth_username = "u"
basic_auth_password = "p"
`
	_, err = loadString(t, withLifecycle)
	if err == nil || !strings.Contains(err.Error(), "[storage]") {
		t.Errorf("error = %v, want a storage-required error", err)
	}

	full := withLifecycle + `
[storage]
in_memory = true
`
	if _, err := loadString(t, full); err != nil {
		t.Errorf("Load with lifecycle + storage: %v", err)
	}
}

func TestValidate_SMTPCredentialsMustPair(t *testing.T) {
	toml := strings.Replace(projectTOML, `name = "novel"`,
		"name = \"novel\"\nsmtp_username = \"novel\"", 1)
	_, err := loadString(t, toml)
	if err == nil || !strings.Contains(err.Error(), "must be set together") {
		t.Errorf("error = %v, want a paired-credential error", err)
	}
}

func TestLoad_ProjectSMTPIdentity(t *testing.T) {
	toml := strings.Replace(projectTOML, `name = "novel"`,
		"name = \"novel\"\nsmtp_username = \"novel\"\nsmtp_password = \"novel-smtp-secret-0123456789\"", 1)
	cfg, err := loadString(t, toml)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Projects[0].SMTPUsername != "novel" {
		t.Errorf("smtp_username = %q", cfg.Projects[0].SMTPUsername)
	}
}

func TestLoad_ProjectReservedLimits(t *testing.T) {
	toml := strings.Replace(projectTOML, `name = "novel"`,
		"name = \"novel\"\ndaily_limit = 5000\nmonthly_limit = 100000", 1)
	cfg, err := loadString(t, toml)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Projects[0].DailyLimit != 5000 || cfg.Projects[0].MonthlyLimit != 100000 {
		t.Errorf("limits = %d/%d", cfg.Projects[0].DailyLimit, cfg.Projects[0].MonthlyLimit)
	}
}

// NFR42: a config with no [[projects]] parses to exactly what it did
// before this release.
func TestValidate_NoProjectsIsUnchanged(t *testing.T) {
	cfg, err := loadString(t, pooledTOML)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Projects) != 0 || cfg.ProjectConfigs() != nil {
		t.Error("a project-free config grew projects")
	}
}
