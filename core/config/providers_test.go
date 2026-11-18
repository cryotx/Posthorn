package config_test

import (
	"strings"
	"testing"

	"github.com/craigmccaskill/posthorn/config"
)

// pooledTOML is the multi-provider baseline: two named providers, one
// endpoint failing over between them.
const pooledTOML = `
[[providers]]
name = "postmark-prod"
type = "postmark"
[providers.settings]
api_key = "pm-key"

[[providers]]
name = "ses-backup"
type = "ses"
weight = 2
[providers.settings]
region = "us-east-1"
access_key_id = "AKIA"
secret_access_key = "secret"

[[endpoints]]
path = "/api/contact"
to = ["craig@example.com"]
from = "noreply@example.com"
subject = "Contact"
body = "Body"

[endpoints.transport]
strategy = "failover"
providers = ["postmark-prod", "ses-backup"]
`

func TestLoad_PooledConfig(t *testing.T) {
	cfg, err := loadString(t, pooledTOML)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Providers) != 2 {
		t.Fatalf("Providers = %d, want 2", len(cfg.Providers))
	}
	if cfg.Providers[1].Weight != 2 {
		t.Errorf("weight = %d, want 2", cfg.Providers[1].Weight)
	}
	tc := cfg.Endpoints[0].Transport
	if !tc.Pooled() {
		t.Error("Pooled() = false on a providers block")
	}
	if got := tc.Label(); got != "pool" {
		t.Errorf("Label() = %q, want pool", got)
	}
	if got := tc.EffectiveStrategy(); got != "failover" {
		t.Errorf("EffectiveStrategy() = %q, want failover", got)
	}
}

// The v1.x inline shape must keep working byte-for-byte — an existing
// deployment upgrades with an unchanged config file.
func TestLoad_InlineTransportStillWorks(t *testing.T) {
	cfg, err := loadString(t, minimalTOML)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	tc := cfg.Endpoints[0].Transport
	if tc.Pooled() {
		t.Error("Pooled() = true on an inline block")
	}
	if got := tc.Label(); got != "postmark" {
		t.Errorf("Label() = %q, want postmark", got)
	}
	if got := tc.EffectiveStrategy(); got != "" {
		t.Errorf("EffectiveStrategy() = %q, want empty for an inline block", got)
	}
	if tc.ProviderRefs() != nil {
		t.Errorf("ProviderRefs() = %v, want nil", tc.ProviderRefs())
	}
}

func TestLoad_StrategyDefaultsToFailover(t *testing.T) {
	toml := strings.Replace(pooledTOML, `strategy = "failover"`, "", 1)
	cfg, err := loadString(t, toml)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Endpoints[0].Transport.EffectiveStrategy(); got != "failover" {
		t.Errorf("EffectiveStrategy() = %q, want failover", got)
	}
}

// --- mode mutex (the ADR-10 footgun rule applied to routing) -----------

func TestValidate_InlineAndPooledAreMutuallyExclusive(t *testing.T) {
	toml := strings.Replace(pooledTOML,
		`[endpoints.transport]
strategy = "failover"
providers = ["postmark-prod", "ses-backup"]`,
		`[endpoints.transport]
type = "postmark"
providers = ["postmark-prod", "ses-backup"]
[endpoints.transport.settings]
api_key = "inline"`, 1)

	_, err := loadString(t, toml)
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("error = %v, want a mutual-exclusion error", err)
	}
}

func TestValidate_StrategyRejectedOnInlineTransport(t *testing.T) {
	toml := strings.Replace(minimalTOML, `type = "postmark"`,
		"type = \"postmark\"\nstrategy = \"weighted\"", 1)
	_, err := loadString(t, toml)
	if err == nil || !strings.Contains(err.Error(), "strategy applies to a provider pool") {
		t.Errorf("error = %v, want a strategy-on-inline error", err)
	}
}

// --- reference resolution ----------------------------------------------

func TestValidate_UnknownProviderReference(t *testing.T) {
	toml := strings.Replace(pooledTOML, `"postmark-prod", "ses-backup"`, `"postmark-typo", "ses-backup"`, 1)
	_, err := loadString(t, toml)
	if err == nil {
		t.Fatal("Load = nil error for an unknown provider reference")
	}
	for _, want := range []string{"unknown provider", "postmark-typo", "postmark-prod"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err, want)
		}
	}
}

func TestValidate_ProvidersReferencedWithNoPool(t *testing.T) {
	toml := strings.Replace(minimalTOML,
		`[endpoints.transport]
type = "postmark"

[endpoints.transport.settings]
api_key = "test-key"`,
		`[endpoints.transport]
providers = ["nowhere"]`, 1)
	_, err := loadString(t, toml)
	if err == nil || !strings.Contains(err.Error(), "no [[providers]] blocks are defined") {
		t.Errorf("error = %v, want a no-pool error", err)
	}
}

// A half-applied rename leaves a provider nobody routes to, and the
// failover the operator configured silently never fires.
func TestValidate_OrphanProviderRejected(t *testing.T) {
	toml := strings.Replace(pooledTOML, `"postmark-prod", "ses-backup"`, `"postmark-prod"`, 1)
	_, err := loadString(t, toml)
	if err == nil || !strings.Contains(err.Error(), "referenced by no endpoint") {
		t.Errorf("error = %v, want an orphan-provider error", err)
	}
	if err != nil && !strings.Contains(err.Error(), "ses-backup") {
		t.Errorf("error %q should name the orphan", err)
	}
}

func TestValidate_SMTPListenerCountsAsAReference(t *testing.T) {
	toml := strings.Replace(pooledTOML, `providers = ["postmark-prod", "ses-backup"]`,
		`providers = ["postmark-prod"]`, 1) + `
[smtp_listener]
listen = "127.0.0.1:2525"
allowed_senders = ["*@example.com"]
require_tls = false
auth_required = "none"

[smtp_listener.smtp_users]
username = "ghost"
password = "pw"

[smtp_listener.transport]
providers = ["ses-backup"]
`
	// smtp_users is a table array in the real schema; use the array form.
	toml = strings.Replace(toml, "[smtp_listener.smtp_users]", "[[smtp_listener.smtp_users]]", 1)
	if _, err := loadString(t, toml); err != nil {
		t.Fatalf("Load: %v", err)
	}
}

// --- provider block validation -----------------------------------------

func TestValidate_ProviderRequiresName(t *testing.T) {
	toml := strings.Replace(pooledTOML, `name = "postmark-prod"`, "", 1)
	_, err := loadString(t, toml)
	if err == nil || !strings.Contains(err.Error(), "name is required") {
		t.Errorf("error = %v, want a missing-name error", err)
	}
}

func TestValidate_ProviderUnknownType(t *testing.T) {
	toml := strings.Replace(pooledTOML, `type = "postmark"`, `type = "carrier-pigeon"`, 1)
	_, err := loadString(t, toml)
	if err == nil || !strings.Contains(err.Error(), "unknown transport type") {
		t.Errorf("error = %v, want an unknown-type error", err)
	}
}

func TestValidate_ProviderSettingsValidatedByRegistry(t *testing.T) {
	toml := strings.Replace(pooledTOML, `api_key = "pm-key"`, `api_key = ""`, 1)
	_, err := loadString(t, toml)
	if err == nil {
		t.Error("Load = nil error for an empty postmark api_key")
	}
}

func TestValidate_DuplicateProviderName(t *testing.T) {
	toml := strings.Replace(pooledTOML, `name = "ses-backup"`, `name = "postmark-prod"`, 1)
	_, err := loadString(t, toml)
	if err == nil || !strings.Contains(err.Error(), "duplicate provider name") {
		t.Errorf("error = %v, want a duplicate-name error", err)
	}
}

func TestValidate_NegativeWeightRejected(t *testing.T) {
	toml := strings.Replace(pooledTOML, "weight = 2", "weight = -1", 1)
	_, err := loadString(t, toml)
	if err == nil || !strings.Contains(err.Error(), "weight") {
		t.Errorf("error = %v, want a weight error", err)
	}
}

func TestValidate_UnknownStrategyRejected(t *testing.T) {
	toml := strings.Replace(pooledTOML, `strategy = "failover"`, `strategy = "sideways"`, 1)
	_, err := loadString(t, toml)
	if err == nil || !strings.Contains(err.Error(), "unknown strategy") {
		t.Errorf("error = %v, want an unknown-strategy error", err)
	}
}

// Listing a provider twice reads like "give it two chances", which is
// not what it does — it just tries the same open circuit again.
func TestValidate_DuplicateProviderReferenceRejected(t *testing.T) {
	toml := strings.Replace(pooledTOML, `"postmark-prod", "ses-backup"`,
		`"postmark-prod", "postmark-prod", "ses-backup"`, 1)
	_, err := loadString(t, toml)
	if err == nil || !strings.Contains(err.Error(), "duplicate provider") {
		t.Errorf("error = %v, want a duplicate-reference error", err)
	}
}

// --- circuit breaker block ---------------------------------------------

func TestLoad_CircuitBreakerBlock(t *testing.T) {
	toml := strings.Replace(pooledTOML, `[providers.settings]
api_key = "pm-key"`, `[providers.settings]
api_key = "pm-key"
[providers.circuit_breaker]
failure_threshold = 3
cooldown = "10s"
half_open_probes = 2`, 1)

	cfg, err := loadString(t, toml)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	cb := cfg.Providers[0].CircuitBreaker
	if cb == nil {
		t.Fatal("CircuitBreaker not parsed")
	}
	if cb.FailureThreshold != 3 || cb.Cooldown.Std().String() != "10s" || cb.HalfOpenProbes != 2 {
		t.Errorf("CircuitBreaker = %+v", cb)
	}
}

func TestValidate_CircuitBreakerNegativeValues(t *testing.T) {
	toml := strings.Replace(pooledTOML, `[providers.settings]
api_key = "pm-key"`, `[providers.settings]
api_key = "pm-key"
[providers.circuit_breaker]
failure_threshold = -1`, 1)
	_, err := loadString(t, toml)
	if err == nil || !strings.Contains(err.Error(), "failure_threshold") {
		t.Errorf("error = %v, want a failure_threshold error", err)
	}
}

// --- routing rules -----------------------------------------------------

const rulesTOML = pooledTOML + `
[[endpoints.transport.rules]]
match_recipient_domain = ["gmail.com", "*.edu"]
strategy = "weighted"
providers = ["ses-backup"]

[[endpoints.transport.rules]]
providers = ["postmark-prod"]
[endpoints.transport.rules.match_field]
plan = ["enterprise", "pro"]
`

func TestLoad_RoutingRules(t *testing.T) {
	cfg, err := loadString(t, rulesTOML)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	rules := cfg.Endpoints[0].Transport.Rules
	if len(rules) != 2 {
		t.Fatalf("rules = %d, want 2", len(rules))
	}
	if len(rules[0].MatchRecipientDomain) != 2 || rules[0].Strategy != "weighted" {
		t.Errorf("rule[0] = %+v", rules[0])
	}
	if got := rules[1].MatchField["plan"]; len(got) != 2 || got[0] != "enterprise" {
		t.Errorf("rule[1].MatchField = %v", rules[1].MatchField)
	}
	refs := cfg.Endpoints[0].Transport.ProviderRefs()
	if len(refs) != 4 {
		t.Errorf("ProviderRefs = %v, want 4 (2 default + 1 per rule)", refs)
	}
}

func TestValidate_RuleNeedsAMatchCriterion(t *testing.T) {
	toml := strings.Replace(rulesTOML, `match_recipient_domain = ["gmail.com", "*.edu"]`, "", 1)
	_, err := loadString(t, toml)
	if err == nil || !strings.Contains(err.Error(), "at least one of match_recipient_domain") {
		t.Errorf("error = %v, want a missing-criterion error", err)
	}
}

func TestValidate_RuleNeedsProviders(t *testing.T) {
	toml := strings.Replace(rulesTOML, `strategy = "weighted"
providers = ["ses-backup"]`, `strategy = "weighted"`, 1)
	_, err := loadString(t, toml)
	if err == nil || !strings.Contains(err.Error(), "at least one provider name") {
		t.Errorf("error = %v, want a missing-providers error", err)
	}
}

func TestValidate_RuleProviderMustExist(t *testing.T) {
	toml := strings.Replace(rulesTOML, `providers = ["ses-backup"]`, `providers = ["ghost-provider"]`, 1)
	_, err := loadString(t, toml)
	if err == nil || !strings.Contains(err.Error(), "unknown provider") {
		t.Errorf("error = %v, want an unknown-provider error", err)
	}
}

func TestValidate_RulesWithoutDefaultProvidersRejected(t *testing.T) {
	toml := strings.Replace(rulesTOML, `strategy = "failover"
providers = ["postmark-prod", "ses-backup"]`, "", 1)
	_, err := loadString(t, toml)
	if err == nil || !strings.Contains(err.Error(), "nowhere to go") {
		t.Errorf("error = %v, want a missing-default-route error", err)
	}
}

func TestValidate_DomainPatterns(t *testing.T) {
	for _, tc := range []struct {
		pattern string
		wantErr string
	}{
		{"gmail.com", ""},
		{"*.edu", ""},
		{"GMAIL.com", "lowercase"},
		{"a@gmail.com", "not an address"},
		{"*.*.com", "only a leading"},
		{"mail.*", "only a leading"},
		{"", "empty pattern"},
	} {
		t.Run(tc.pattern, func(t *testing.T) {
			toml := strings.Replace(rulesTOML, `["gmail.com", "*.edu"]`, `["`+tc.pattern+`"]`, 1)
			_, err := loadString(t, toml)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Load: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %v, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

func TestValidate_RuleFieldNeedsValues(t *testing.T) {
	toml := strings.Replace(rulesTOML, `plan = ["enterprise", "pro"]`, `plan = []`, 1)
	_, err := loadString(t, toml)
	if err == nil || !strings.Contains(err.Error(), "at least one value required") {
		t.Errorf("error = %v, want an empty-values error", err)
	}
}

// A pooled endpoint reaches Validate through the same path as an
// inline one, so a config that is otherwise broken still reports the
// endpoint-level problem rather than being masked by routing checks.
func TestValidate_EndpointErrorsStillSurfaceUnderAPool(t *testing.T) {
	toml := strings.Replace(pooledTOML, `subject = "Contact"`, "", 1)
	_, err := loadString(t, toml)
	if err == nil || !strings.Contains(err.Error(), "subject") {
		t.Errorf("error = %v, want the subject error", err)
	}
}

func TestTransportConfig_ProviderRefsOrder(t *testing.T) {
	tc := config.TransportConfig{
		Providers: []string{"a", "b"},
		Rules: []config.RouteRuleConfig{
			{MatchRecipientDomain: []string{"x.com"}, Providers: []string{"c"}},
		},
	}
	got := tc.ProviderRefs()
	want := []string{"a", "b", "c"}
	if len(got) != len(want) {
		t.Fatalf("ProviderRefs = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("ProviderRefs[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}
