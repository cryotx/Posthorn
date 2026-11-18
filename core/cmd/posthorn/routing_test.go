package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/craigmccaskill/posthorn/config"
	"github.com/craigmccaskill/posthorn/metrics"
	"github.com/craigmccaskill/posthorn/transport"
)

// twoProviders is the pool used across the wiring tests. Postmark twice
// rather than two different types keeps the assertions about routing,
// not about provider clients.
func twoProviders() []config.ProviderConfig {
	return []config.ProviderConfig{
		{Name: "postmark-prod", Type: "postmark", Settings: map[string]any{"api_key": "k1"}},
		{Name: "postmark-backup", Type: "postmark", Settings: map[string]any{"api_key": "k2"}},
	}
}

func pooledEndpoint(path string, providers ...string) config.EndpointConfig {
	return config.EndpointConfig{
		Path:      path,
		To:        []string{"to@example.com"},
		From:      "from@example.com",
		Subject:   "S",
		Body:      "B",
		Transport: config.TransportConfig{Providers: providers},
	}
}

// --- buildPool ---------------------------------------------------------

func TestBuildPool_BuildsEveryEntry(t *testing.T) {
	pool, err := buildPool(twoProviders(), nil)
	if err != nil {
		t.Fatalf("buildPool: %v", err)
	}
	if pool.Len() != 2 {
		t.Fatalf("pool.Len() = %d, want 2", pool.Len())
	}
	for _, name := range []string{"postmark-prod", "postmark-backup"} {
		p, ok := pool.Get(name)
		if !ok {
			t.Fatalf("provider %q missing", name)
		}
		if p.Transport == nil || p.Breaker == nil {
			t.Errorf("provider %q incompletely built: %+v", name, p)
		}
	}
}

func TestBuildPool_EmptyConfigYieldsEmptyPool(t *testing.T) {
	pool, err := buildPool(nil, nil)
	if err != nil {
		t.Fatalf("buildPool: %v", err)
	}
	if pool.Len() != 0 {
		t.Errorf("pool.Len() = %d, want 0", pool.Len())
	}
}

func TestBuildPool_SurfacesSettingsErrors(t *testing.T) {
	_, err := buildPool([]config.ProviderConfig{
		{Name: "broken", Type: "postmark", Settings: map[string]any{}},
	}, nil)
	if err == nil {
		t.Fatal("buildPool = nil error for a postmark provider with no api_key")
	}
	if !strings.Contains(err.Error(), "broken") {
		t.Errorf("error %q should name the offending provider", err)
	}
}

func TestBuildPool_AppliesBreakerConfig(t *testing.T) {
	cd := config.Duration(0)
	if err := (&cd).UnmarshalText([]byte("7s")); err != nil {
		t.Fatal(err)
	}
	pool, err := buildPool([]config.ProviderConfig{{
		Name: "p", Type: "postmark", Settings: map[string]any{"api_key": "k"},
		CircuitBreaker: &config.CircuitBreakerConfig{FailureThreshold: 2, Cooldown: cd},
	}}, nil)
	if err != nil {
		t.Fatalf("buildPool: %v", err)
	}
	p, _ := pool.Get("p")

	p.Breaker.Failure(&transport.TransportError{Class: transport.ErrTransient})
	if !p.Breaker.Allow() {
		t.Fatal("circuit opened after 1 failure, want threshold 2")
	}
	p.Breaker.Failure(&transport.TransportError{Class: transport.ErrTransient})
	if p.Breaker.Allow() {
		t.Error("circuit still closed after 2 failures, want open")
	}
}

// The pool exists so health is observed once per upstream rather than
// once per endpoint that happens to notice.
func TestBuildPool_ProvidersAreSharedAcrossEndpoints(t *testing.T) {
	cfg := &config.Config{
		Providers: twoProviders(),
		Endpoints: []config.EndpointConfig{
			pooledEndpoint("/a", "postmark-prod", "postmark-backup"),
			pooledEndpoint("/b", "postmark-prod"),
		},
	}
	pool, err := buildPool(cfg.Providers, nil)
	if err != nil {
		t.Fatalf("buildPool: %v", err)
	}

	ta, err := buildRoutedTransport("/a", cfg.Endpoints[0].Transport, pool, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("buildRoutedTransport(/a): %v", err)
	}
	tb, err := buildRoutedTransport("/b", cfg.Endpoints[1].Transport, pool, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("buildRoutedTransport(/b): %v", err)
	}

	first := ta.(*transport.Router).Providers()[0]
	second := tb.(*transport.Router).Providers()[0]
	if first != second {
		t.Error("two endpoints naming one provider got separate instances — the circuit breaker would not be shared")
	}

	// Trip it through one router; the other must already know.
	first.Breaker.Failure(&transport.TransportError{Class: transport.ErrTransient})
	for i := 0; i < 10; i++ {
		second.Breaker.Failure(&transport.TransportError{Class: transport.ErrTransient})
	}
	if first.Breaker.Allow() {
		t.Error("failures recorded via /b did not reach the provider /a routes to")
	}
}

// --- buildRoutedTransport ----------------------------------------------

func TestBuildRoutedTransport_InlineStaysUnwrapped(t *testing.T) {
	tp, err := buildRoutedTransport("/x", config.TransportConfig{
		Type: "postmark", Settings: map[string]any{"api_key": "k"},
	}, nil, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("buildRoutedTransport: %v", err)
	}
	if _, isRouter := tp.(*transport.Router); isRouter {
		t.Error("an inline transport was wrapped in a Router — v1.x configs must keep their exact send path")
	}
}

func TestBuildRoutedTransport_PooledBuildsRouter(t *testing.T) {
	pool, err := buildPool(twoProviders(), nil)
	if err != nil {
		t.Fatal(err)
	}
	tp, err := buildRoutedTransport("/api/contact", config.TransportConfig{
		Strategy:  "weighted",
		Providers: []string{"postmark-prod", "postmark-backup"},
	}, pool, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("buildRoutedTransport: %v", err)
	}
	router, ok := tp.(*transport.Router)
	if !ok {
		t.Fatalf("built %T, want *transport.Router", tp)
	}
	if router.Name != "/api/contact" {
		t.Errorf("Name = %q, want /api/contact", router.Name)
	}
	if router.Default.Strategy != transport.StrategyWeighted {
		t.Errorf("Strategy = %q, want weighted", router.Default.Strategy)
	}
	if len(router.Providers()) != 2 {
		t.Errorf("Providers = %d, want 2", len(router.Providers()))
	}
}

func TestBuildRoutedTransport_BuildsRules(t *testing.T) {
	pool, err := buildPool(twoProviders(), nil)
	if err != nil {
		t.Fatal(err)
	}
	tp, err := buildRoutedTransport("/x", config.TransportConfig{
		Providers: []string{"postmark-prod"},
		Rules: []config.RouteRuleConfig{{
			MatchRecipientDomain: []string{"gmail.com"},
			Providers:            []string{"postmark-backup"},
		}},
	}, pool, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("buildRoutedTransport: %v", err)
	}
	router := tp.(*transport.Router)

	ex := router.Explain(transport.Message{To: []string{"a@gmail.com"}})
	if ex.RuleIndex != 0 || len(ex.Providers) != 1 || ex.Providers[0] != "postmark-backup" {
		t.Errorf("Explain(gmail) = %+v, want rule 0 → postmark-backup", ex)
	}
	ex = router.Explain(transport.Message{To: []string{"a@example.com"}})
	if ex.RuleIndex != -1 || ex.Providers[0] != "postmark-prod" {
		t.Errorf("Explain(other) = %+v, want the default route", ex)
	}
}

func TestBuildRoutedTransport_UnknownProviderIsAnError(t *testing.T) {
	pool, err := buildPool(twoProviders(), nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = buildRoutedTransport("/x", config.TransportConfig{Providers: []string{"nope"}}, pool, nil, nil, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "unknown provider") {
		t.Errorf("error = %v, want an unknown-provider error", err)
	}
}

// --- /healthz + /metrics wiring ----------------------------------------

func TestBuildMux_HealthzReportsProviderCircuits(t *testing.T) {
	cfg := &config.Config{
		Providers: twoProviders(),
		Endpoints: []config.EndpointConfig{pooledEndpoint("/a", "postmark-prod", "postmark-backup")},
	}
	mux, _, _, err := muxRegRec(cfg)
	if err != nil {
		t.Fatalf("buildMux: %v", err)
	}

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var body struct {
		Status    string            `json:"status"`
		Providers map[string]string `json:"providers"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	if body.Status != "ok" {
		t.Errorf("status = %q, want ok", body.Status)
	}
	if body.Providers["postmark-prod"] != "closed" || body.Providers["postmark-backup"] != "closed" {
		t.Errorf("providers = %v, want both closed", body.Providers)
	}
}

// Without a pool, /healthz keeps its v1.x plain-text body — an upgrade
// must not break an operator's existing probe.
func TestBuildMux_HealthzUnchangedWithoutPool(t *testing.T) {
	cfg := &config.Config{Endpoints: []config.EndpointConfig{{
		Path: "/a", To: []string{"to@example.com"}, From: "from@example.com",
		Subject: "S", Body: "B",
		Transport: config.TransportConfig{Type: "postmark", Settings: map[string]any{"api_key": "k"}},
	}}}
	mux, _, _, err := muxRegRec(cfg)
	if err != nil {
		t.Fatalf("buildMux: %v", err)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if got := rec.Body.String(); got != "ok" {
		t.Errorf("body = %q, want %q", got, "ok")
	}
}

func TestBuildMux_ProviderMetricsAreRegisteredUpFront(t *testing.T) {
	cfg := &config.Config{
		Providers: twoProviders(),
		Endpoints: []config.EndpointConfig{pooledEndpoint("/a", "postmark-prod", "postmark-backup")},
	}
	mux, _, _, err := muxRegRec(cfg)
	if err != nil {
		t.Fatalf("buildMux: %v", err)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := rec.Body.String()

	for _, want := range []string{
		"posthorn_provider_attempts_total",
		"posthorn_provider_circuit_state",
		"posthorn_route_failover_total",
		// The healthy series must exist before the first incident:
		// an absent series and a closed circuit look identical to an
		// alerting rule.
		`posthorn_provider_circuit_state{provider="postmark-prod"} 0`,
		`posthorn_provider_circuit_state{provider="postmark-backup"} 0`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/metrics missing %q", want)
		}
	}
}

func TestBuildMux_CircuitGaugeTracksBreakerTransitions(t *testing.T) {
	reg := metrics.New()
	rec := metrics.NewRecorder(reg)
	pool, err := buildPool(twoProviders(), rec)
	if err != nil {
		t.Fatalf("buildPool: %v", err)
	}
	p, _ := pool.Get("postmark-prod")
	for i := 0; i < transport.DefaultBreakerFailureThreshold; i++ {
		p.Breaker.Failure(&transport.TransportError{Class: transport.ErrTransient})
	}

	var sb strings.Builder
	if err := reg.Emit(&sb); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if !strings.Contains(sb.String(), `posthorn_provider_circuit_state{provider="postmark-prod"} 2`) {
		t.Errorf("gauge did not follow the breaker to open:\n%s", sb.String())
	}
}
