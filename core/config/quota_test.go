package config_test

import (
	"strings"
	"testing"

	"github.com/craigmccaskill/posthorn/config"
	"github.com/craigmccaskill/posthorn/transport"
)

// quotaTOML is the shape the feature exists for: a free provider with a
// daily allowance, a paid one without.
const quotaTOML = `
[[providers]]
name = "resend-free"
type = "resend"
cost_per_1000 = 0
[providers.settings]
api_key = "re-key"
[providers.quota]
daily_limit = 100
monthly_limit = 3000

[[providers]]
name = "ses-paid"
type = "ses"
cost_per_1000 = "0.10"
[providers.settings]
region = "us-east-1"
access_key_id = "AKIA"
secret_access_key = "secret"

[[endpoints]]
path = "/api/send"
to = ["ops@example.com"]
from = "noreply@example.com"
subject = "S"
body = "B"

[endpoints.transport]
strategy = "free_first"
providers = ["resend-free", "ses-paid"]
`

func TestLoad_QuotaAndCost(t *testing.T) {
	cfg, err := loadString(t, quotaTOML)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	free := cfg.Providers[0]
	if free.Quota == nil || free.Quota.DailyLimit != 100 || free.Quota.MonthlyLimit != 3000 {
		t.Errorf("quota = %+v", free.Quota)
	}
	if free.CostPer1000.V != 0 {
		t.Errorf("free cost = %d, want 0", free.CostPer1000.V)
	}
	paid := cfg.Providers[1]
	if paid.Quota != nil {
		t.Errorf("paid quota = %+v, want nil (unlimited)", paid.Quota)
	}
	if paid.CostPer1000.V != 100_000 {
		t.Errorf("paid cost = %d, want 100000 micros", paid.CostPer1000.V)
	}
	if got := cfg.Endpoints[0].Transport.EffectiveStrategy(); got != "free_first" {
		t.Errorf("strategy = %q", got)
	}
}

func TestLoad_CostAcceptsAllThreeForms(t *testing.T) {
	for _, tc := range []struct {
		literal string
		want    transport.Cost
	}{
		{`cost_per_1000 = "0.10"`, 100_000},
		{`cost_per_1000 = 0.10`, 100_000},
		{`cost_per_1000 = 2`, 2_000_000},
	} {
		t.Run(tc.literal, func(t *testing.T) {
			toml := strings.Replace(quotaTOML, `cost_per_1000 = "0.10"`, tc.literal, 1)
			cfg, err := loadString(t, toml)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if got := cfg.Providers[1].CostPer1000.V; got != tc.want {
				t.Errorf("cost = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestValidate_RejectsBadCost(t *testing.T) {
	for _, tc := range []struct{ literal, want string }{
		{`cost_per_1000 = "cheap"`, "not a decimal number"},
		{`cost_per_1000 = "0.1234567"`, "more than 6 decimal places"},
		{`cost_per_1000 = "-1"`, "non-negative"},
	} {
		t.Run(tc.literal, func(t *testing.T) {
			toml := strings.Replace(quotaTOML, `cost_per_1000 = "0.10"`, tc.literal, 1)
			_, err := loadString(t, toml)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestValidate_QuotaLimits(t *testing.T) {
	for _, tc := range []struct{ block, want string }{
		{"daily_limit = -1", "daily_limit"},
		{"monthly_limit = -5", "monthly_limit"},
		// A daily budget larger than the monthly one could never be
		// spent in full; it is always a typo.
		{"daily_limit = 5000\nmonthly_limit = 100", "exceeds monthly_limit"},
	} {
		t.Run(tc.block, func(t *testing.T) {
			toml := strings.Replace(quotaTOML,
				"daily_limit = 100\nmonthly_limit = 3000", tc.block, 1)
			_, err := loadString(t, toml)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestValidate_ZeroLimitsMeanUnlimited(t *testing.T) {
	toml := strings.Replace(quotaTOML,
		"daily_limit = 100\nmonthly_limit = 3000",
		"daily_limit = 0\nmonthly_limit = 0", 1)
	if _, err := loadString(t, toml); err != nil {
		t.Errorf("Load: %v", err)
	}
}

// A reserved-but-unimplemented value must be refused, not ignored. A
// silently-dropped quota_source is exactly the looks-configured-but-
// isn't footgun ADR-10 exists to prevent.
func TestValidate_RemoteQuotaSourceIsRefused(t *testing.T) {
	toml := strings.Replace(quotaTOML, `name = "resend-free"`,
		"name = \"resend-free\"\nquota_source = \"remote\"", 1)
	_, err := loadString(t, toml)
	if err == nil || !strings.Contains(err.Error(), "reserved but not implemented") {
		t.Errorf("error = %v, want a reserved-value error", err)
	}
}

func TestValidate_LocalQuotaSourceAccepted(t *testing.T) {
	toml := strings.Replace(quotaTOML, `name = "resend-free"`,
		"name = \"resend-free\"\nquota_source = \"local\"", 1)
	if _, err := loadString(t, toml); err != nil {
		t.Errorf("Load: %v", err)
	}
}

func TestValidate_UnknownQuotaSource(t *testing.T) {
	toml := strings.Replace(quotaTOML, `name = "resend-free"`,
		"name = \"resend-free\"\nquota_source = \"magic\"", 1)
	_, err := loadString(t, toml)
	if err == nil || !strings.Contains(err.Error(), "quota_source") {
		t.Errorf("error = %v, want a quota_source error", err)
	}
}

// --- mail kind ---------------------------------------------------------

func TestLoad_MailKind(t *testing.T) {
	toml := strings.Replace(quotaTOML, `path = "/api/send"`,
		"path = \"/api/send\"\nmail_kind = \"verification\"", 1)
	cfg, err := loadString(t, toml)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Endpoints[0].MailKind != "verification" {
		t.Errorf("mail_kind = %q", cfg.Endpoints[0].MailKind)
	}
}

func TestValidate_UnknownMailKind(t *testing.T) {
	toml := strings.Replace(quotaTOML, `path = "/api/send"`,
		"path = \"/api/send\"\nmail_kind = \"newsletter\"", 1)
	_, err := loadString(t, toml)
	if err == nil || !strings.Contains(err.Error(), "unknown mail kind") {
		t.Errorf("error = %v, want an unknown-kind error", err)
	}
}

func TestValidate_MatchKindRule(t *testing.T) {
	toml := quotaTOML + `
[[endpoints.transport.rules]]
match_kind = ["verification", "password_reset"]
providers = ["ses-paid"]
`
	cfg, err := loadString(t, toml)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	rule := cfg.Endpoints[0].Transport.Rules[0]
	if len(rule.MatchKind) != 2 || rule.MatchKind[0] != "verification" {
		t.Errorf("match_kind = %v", rule.MatchKind)
	}
}

func TestValidate_MatchKindRejectsUnknown(t *testing.T) {
	toml := quotaTOML + `
[[endpoints.transport.rules]]
match_kind = ["newsletter"]
providers = ["ses-paid"]
`
	_, err := loadString(t, toml)
	if err == nil || !strings.Contains(err.Error(), "unknown mail kind") {
		t.Errorf("error = %v, want an unknown-kind error", err)
	}
}

// match_kind alone is a complete rule — it is the whole point of the
// feature, so it must satisfy the "needs a criterion" check.
func TestValidate_MatchKindAloneIsAValidRule(t *testing.T) {
	toml := quotaTOML + `
[[endpoints.transport.rules]]
match_kind = ["marketing"]
providers = ["ses-paid"]
`
	if _, err := loadString(t, toml); err != nil {
		t.Errorf("Load: %v", err)
	}
}

// --- provider groups ---------------------------------------------------

const groupTOML = `
[[providers]]
name = "resend-free"
type = "resend"
[providers.settings]
api_key = "re-key"
[providers.quota]
daily_limit = 100

[[providers]]
name = "postmark-paid"
type = "postmark"
cost_per_1000 = "0.50"
[providers.settings]
api_key = "pm-key"

[[provider_groups]]
name = "free_pool"
providers = ["resend-free", "postmark-paid"]
strategy = "free_first"

[[endpoints]]
path = "/api/send"
to = ["ops@example.com"]
from = "noreply@example.com"
subject = "S"
body = "B"

[endpoints.transport]
group = "free_pool"
`

func TestLoad_ProviderGroup(t *testing.T) {
	cfg, err := loadString(t, groupTOML)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.ProviderGroups) != 1 || cfg.ProviderGroups[0].Name != "free_pool" {
		t.Fatalf("groups = %+v", cfg.ProviderGroups)
	}
	tc := cfg.Endpoints[0].Transport
	if !tc.Pooled() {
		t.Error("a group reference should count as pooled")
	}
	if refs := tc.GroupRefs(); len(refs) != 1 || refs[0] != "free_pool" {
		t.Errorf("GroupRefs = %v", refs)
	}
	// A group name is not a provider name.
	if refs := tc.ProviderRefs(); len(refs) != 0 {
		t.Errorf("ProviderRefs = %v, want none", refs)
	}
}

func TestValidate_GroupAndProvidersAreExclusive(t *testing.T) {
	toml := strings.Replace(groupTOML, `group = "free_pool"`,
		"group = \"free_pool\"\nproviders = [\"resend-free\"]", 1)
	_, err := loadString(t, toml)
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("error = %v, want a mutual-exclusion error", err)
	}
}

func TestValidate_UnknownGroupReference(t *testing.T) {
	toml := strings.Replace(groupTOML, `group = "free_pool"`, `group = "premium_pool"`, 1)
	_, err := loadString(t, toml)
	if err == nil || !strings.Contains(err.Error(), "unknown provider group") {
		t.Errorf("error = %v, want an unknown-group error", err)
	}
}

func TestValidate_GroupWithUnknownProvider(t *testing.T) {
	toml := strings.Replace(groupTOML, `providers = ["resend-free", "postmark-paid"]`,
		`providers = ["resend-free", "ghost"]`, 1)
	_, err := loadString(t, toml)
	if err == nil || !strings.Contains(err.Error(), "unknown provider") {
		t.Errorf("error = %v, want an unknown-provider error", err)
	}
}

// One name space: `group = "x"` and `providers = ["x"]` must never be
// able to mean two different routes.
func TestValidate_GroupNameCannotShadowProvider(t *testing.T) {
	toml := strings.Replace(groupTOML, `name = "free_pool"`, `name = "resend-free"`, 1)
	toml = strings.Replace(toml, `group = "free_pool"`, `group = "resend-free"`, 1)
	_, err := loadString(t, toml)
	if err == nil || !strings.Contains(err.Error(), "already a provider name") {
		t.Errorf("error = %v, want a name-collision error", err)
	}
}

func TestValidate_DuplicateGroupName(t *testing.T) {
	toml := groupTOML + `
[[provider_groups]]
name = "free_pool"
providers = ["postmark-paid"]
`
	_, err := loadString(t, toml)
	if err == nil || !strings.Contains(err.Error(), "duplicate group name") {
		t.Errorf("error = %v, want a duplicate-name error", err)
	}
}

// Providers reached only through a group are referenced, so the
// orphan check must not fire on them.
func TestValidate_GroupMembersCountAsReferenced(t *testing.T) {
	if _, err := loadString(t, groupTOML); err != nil {
		t.Errorf("Load: %v — group members should count as referenced", err)
	}
}

func TestValidate_RuleCanReferenceGroup(t *testing.T) {
	toml := strings.Replace(groupTOML, `[endpoints.transport]
group = "free_pool"`, `[endpoints.transport]
providers = ["postmark-paid"]

[[endpoints.transport.rules]]
match_kind = ["notification"]
group = "free_pool"`, 1)
	cfg, err := loadString(t, toml)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Endpoints[0].Transport.Rules[0].Group != "free_pool" {
		t.Errorf("rule group = %q", cfg.Endpoints[0].Transport.Rules[0].Group)
	}
}

// --- backwards compatibility -------------------------------------------

// NFR32 restated for this phase: a config with no quota, cost, kind or
// group fields must parse to exactly what it did before.
func TestValidate_PreQuotaConfigUnchanged(t *testing.T) {
	cfg, err := loadString(t, pooledTOML)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, p := range cfg.Providers {
		if p.Quota != nil {
			t.Errorf("provider %s gained a quota block", p.Name)
		}
		if p.CostPer1000.V != 0 {
			t.Errorf("provider %s gained a cost", p.Name)
		}
		if p.QuotaSource != "" {
			t.Errorf("provider %s gained a quota_source", p.Name)
		}
	}
	if cfg.Endpoints[0].MailKind != "" {
		t.Error("endpoint gained a mail_kind")
	}
	if len(cfg.ProviderGroups) != 0 {
		t.Error("config gained provider groups")
	}
}

func TestTransportConfig_GroupRefsNilForInline(t *testing.T) {
	tc := config.TransportConfig{Type: "postmark", Settings: map[string]any{"api_key": "k"}}
	if tc.GroupRefs() != nil {
		t.Errorf("GroupRefs = %v, want nil for an inline block", tc.GroupRefs())
	}
}
