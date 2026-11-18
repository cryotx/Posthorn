package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/craigmccaskill/posthorn/config"
	"github.com/craigmccaskill/posthorn/metrics"
	"github.com/craigmccaskill/posthorn/transport"
)

func budgetedProviders() []config.ProviderConfig {
	free := config.ProviderConfig{
		Name: "resend-free", Type: "resend",
		Settings: map[string]any{"api_key": "re-key"},
		Quota:    &config.QuotaConfig{DailyLimit: 2, MonthlyLimit: 50},
	}
	paid := config.ProviderConfig{
		Name: "postmark-paid", Type: "postmark",
		Settings:    map[string]any{"api_key": "pm-key"},
		CostPer1000: config.Cost{V: 100_000},
	}
	return []config.ProviderConfig{free, paid}
}

func TestBuildPool_CarriesQuotaAndCost(t *testing.T) {
	pool, err := buildPool(budgetedProviders(), nil)
	if err != nil {
		t.Fatalf("buildPool: %v", err)
	}
	free, _ := pool.Get("resend-free")
	if free.Quota.Daily != 2 || free.Quota.Monthly != 50 {
		t.Errorf("quota = %+v", free.Quota)
	}
	if free.Cost != 0 {
		t.Errorf("free cost = %d, want 0", free.Cost)
	}
	paid, _ := pool.Get("postmark-paid")
	if !paid.Quota.Unlimited() {
		t.Errorf("paid quota = %+v, want unlimited", paid.Quota)
	}
	if paid.Cost != 100_000 {
		t.Errorf("paid cost = %d, want 100000", paid.Cost)
	}
}

// A config with no limits gets a nil quota source, so the send path is
// exactly what it was before quotas existed (NFR32).
func TestBuildQuota_NilWithoutLimits(t *testing.T) {
	cfg := &config.Config{Providers: twoProviders()}
	if q := buildQuota(cfg, nil); q != nil {
		t.Error("buildQuota returned a tracker for a config with no limits")
	}
}

func TestBuildQuota_BuiltWhenAnyProviderHasALimit(t *testing.T) {
	cfg := &config.Config{Providers: budgetedProviders()}
	if q := buildQuota(cfg, nil); q == nil {
		t.Error("buildQuota returned nil for a config with a daily limit")
	}
}

// Zero limits mean unlimited, so they must not switch quota tracking on.
func TestBuildQuota_ZeroLimitsDoNotEnable(t *testing.T) {
	cfg := &config.Config{Providers: []config.ProviderConfig{{
		Name: "p", Type: "postmark", Settings: map[string]any{"api_key": "k"},
		Quota: &config.QuotaConfig{},
	}}}
	if q := buildQuota(cfg, nil); q != nil {
		t.Error("buildQuota enabled tracking for zero (unlimited) limits")
	}
}

func TestBuildRoutedTransport_AttachesQuotaSource(t *testing.T) {
	pool, err := buildPool(budgetedProviders(), nil)
	if err != nil {
		t.Fatal(err)
	}
	quota := transport.NewLocalQuota(nil, nil)
	tp, err := buildRoutedTransport("/x", config.TransportConfig{
		Strategy:  "free_first",
		Providers: []string{"resend-free", "postmark-paid"},
	}, pool, nil, quota, nil, nil)
	if err != nil {
		t.Fatalf("buildRoutedTransport: %v", err)
	}
	router := tp.(*transport.Router)
	if router.Quota == nil {
		t.Error("router has no quota source")
	}
	if router.Default.Strategy != transport.StrategyFreeFirst {
		t.Errorf("strategy = %q", router.Default.Strategy)
	}
}

// --- provider groups ---------------------------------------------------

func TestBuildRoutedTransport_ResolvesGroup(t *testing.T) {
	pool, err := buildPool(budgetedProviders(), nil)
	if err != nil {
		t.Fatal(err)
	}
	groups := map[string]config.ProviderGroupConfig{
		"free_pool": {Name: "free_pool", Providers: []string{"resend-free", "postmark-paid"}, Strategy: "free_first"},
	}
	tp, err := buildRoutedTransport("/x", config.TransportConfig{Group: "free_pool"}, pool, groups, nil, nil, nil)
	if err != nil {
		t.Fatalf("buildRoutedTransport: %v", err)
	}
	router := tp.(*transport.Router)
	if router.Default.Strategy != transport.StrategyFreeFirst {
		t.Errorf("strategy = %q, want the group's free_first", router.Default.Strategy)
	}
	if len(router.Providers()) != 2 {
		t.Errorf("providers = %d, want 2", len(router.Providers()))
	}
}

// A referencing block may deviate from the group's default strategy.
func TestBuildRoutedTransport_BlockStrategyOverridesGroup(t *testing.T) {
	pool, _ := buildPool(budgetedProviders(), nil)
	groups := map[string]config.ProviderGroupConfig{
		"free_pool": {Name: "free_pool", Providers: []string{"resend-free"}, Strategy: "free_first"},
	}
	tp, err := buildRoutedTransport("/x",
		config.TransportConfig{Group: "free_pool", Strategy: "round_robin"}, pool, groups, nil, nil, nil)
	if err != nil {
		t.Fatalf("buildRoutedTransport: %v", err)
	}
	if got := tp.(*transport.Router).Default.Strategy; got != transport.StrategyRoundRobin {
		t.Errorf("strategy = %q, want round_robin", got)
	}
}

func TestBuildRoutedTransport_UnknownGroupIsAnError(t *testing.T) {
	pool, _ := buildPool(budgetedProviders(), nil)
	_, err := buildRoutedTransport("/x", config.TransportConfig{Group: "nope"}, pool, nil, nil, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "unknown provider group") {
		t.Errorf("error = %v, want an unknown-group error", err)
	}
}

func TestBuildRoutedTransport_RuleGroupAndKind(t *testing.T) {
	pool, _ := buildPool(budgetedProviders(), nil)
	groups := map[string]config.ProviderGroupConfig{
		"free_pool": {Name: "free_pool", Providers: []string{"resend-free"}},
	}
	tp, err := buildRoutedTransport("/x", config.TransportConfig{
		Providers: []string{"postmark-paid"},
		Rules: []config.RouteRuleConfig{{
			MatchKind: []string{"notification"},
			Group:     "free_pool",
		}},
	}, pool, groups, nil, nil, nil)
	if err != nil {
		t.Fatalf("buildRoutedTransport: %v", err)
	}
	router := tp.(*transport.Router)

	ex := router.Explain(transport.Message{To: []string{"a@example.com"}, Kind: transport.KindNotification})
	if ex.RuleIndex != 0 || ex.Providers[0] != "resend-free" {
		t.Errorf("notification routed to %+v, want the free pool", ex)
	}
	ex = router.Explain(transport.Message{To: []string{"a@example.com"}, Kind: transport.KindImportant})
	if ex.RuleIndex != -1 || ex.Providers[0] != "postmark-paid" {
		t.Errorf("important routed to %+v, want the default route", ex)
	}
}

// --- metrics -----------------------------------------------------------

func TestBuildPool_PublishesQuotaAndCostGauges(t *testing.T) {
	reg := metrics.New()
	rec := metrics.NewRecorder(reg)
	if _, err := buildPool(budgetedProviders(), rec); err != nil {
		t.Fatal(err)
	}

	var sb strings.Builder
	if err := reg.Emit(&sb); err != nil {
		t.Fatal(err)
	}
	out := sb.String()
	for _, want := range []string{
		`posthorn_provider_quota_limit{provider="resend-free",window="daily"} 2`,
		`posthorn_provider_quota_limit{provider="resend-free",window="monthly"} 50`,
		`posthorn_provider_cost_per_1000_micros{provider="postmark-paid"} 100000`,
		`posthorn_provider_cost_per_1000_micros{provider="resend-free"} 0`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	// An unlimited provider must not get a fabricated ceiling — an
	// alert written against the gauge would break on it.
	if strings.Contains(out, `posthorn_provider_quota_limit{provider="postmark-paid"`) {
		t.Errorf("unlimited provider got a quota_limit series:\n%s", out)
	}
}

func TestPublishQuotaGauges_ReportsRemaining(t *testing.T) {
	reg := metrics.New()
	rec := metrics.NewRecorder(reg)
	pool, err := buildPool(budgetedProviders(), rec)
	if err != nil {
		t.Fatal(err)
	}
	quota := transport.NewLocalQuota(nil, nil)
	now := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	free, _ := pool.Get("resend-free")
	quota.Record(free, now)

	publishQuotaGauges(pool, quota, rec, now)

	var sb strings.Builder
	if err := reg.Emit(&sb); err != nil {
		t.Fatal(err)
	}
	out := sb.String()
	if !strings.Contains(out, `posthorn_provider_quota_remaining{provider="resend-free",window="daily"} 1`) {
		t.Errorf("daily remaining missing:\n%s", out)
	}
	if !strings.Contains(out, `posthorn_provider_quota_remaining{provider="resend-free",window="monthly"} 49`) {
		t.Errorf("monthly remaining missing:\n%s", out)
	}
	if strings.Contains(out, `posthorn_provider_quota_remaining{provider="postmark-paid"`) {
		t.Errorf("unlimited provider got a remaining series:\n%s", out)
	}
}

func TestPublishQuotaGauges_NilArgsAreSafe(t *testing.T) {
	publishQuotaGauges(nil, nil, nil, time.Now())
}

// --- end-to-end through the mux ----------------------------------------

func TestBuildMux_QuotaMetricsRegistered(t *testing.T) {
	cfg := &config.Config{
		Providers: budgetedProviders(),
		Endpoints: []config.EndpointConfig{
			pooledEndpoint("/a", "resend-free", "postmark-paid"),
		},
	}
	mux, _, _, err := muxRegRec(cfg)
	if err != nil {
		t.Fatalf("buildMux: %v", err)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	for _, want := range []string{
		"posthorn_provider_quota_exhausted_total",
		"posthorn_provider_quota_remaining",
		"posthorn_provider_quota_limit",
		"posthorn_route_quota_exhausted_total",
		"posthorn_provider_cost_per_1000_micros",
	} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("/metrics missing %q", want)
		}
	}
}
