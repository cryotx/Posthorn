package gateway_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/craigmccaskill/posthorn/config"
	"github.com/craigmccaskill/posthorn/gateway"
	"github.com/craigmccaskill/posthorn/metrics"
	"github.com/craigmccaskill/posthorn/transport"
)

// pooledCfg is an endpoint whose transport block references the pool, so
// Handler.transportLabel resolves to "pool" rather than a type name.
func pooledCfg(path string, providers ...string) config.EndpointConfig {
	return config.EndpointConfig{
		Path:      path,
		To:        []string{"to@example.com"},
		From:      "from@example.com",
		Subject:   "Subject text",
		Body:      "Body text",
		Transport: config.TransportConfig{Providers: providers},
	}
}

// routerOver builds a real transport.Router over recordingTransports so
// the handler exercises the same send path production uses.
func routerOver(t *testing.T, name string, entries ...struct {
	name string
	tp   transport.Transport
}) *transport.Router {
	t.Helper()
	provs := make([]*transport.Provider, 0, len(entries))
	for _, e := range entries {
		provs = append(provs, &transport.Provider{
			Name: e.name, Type: "mock", Weight: 1, Transport: e.tp,
			Breaker: transport.NewBreaker(transport.BreakerConfig{Disabled: true}),
		})
	}
	route, err := transport.NewRoute(nil, transport.StrategyFailover, provs)
	if err != nil {
		t.Fatalf("NewRoute: %v", err)
	}
	r, err := transport.NewRouter(name, route, nil)
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	return r
}

type entry = struct {
	name string
	tp   transport.Transport
}

// --- metric labels -----------------------------------------------------

// A pooled endpoint's success metric must name the provider that
// actually delivered, not the static "pool" label — otherwise the whole
// point of per-provider observability is lost.
func TestHandler_SentMetricUsesDeliveringProvider(t *testing.T) {
	primary := &recordingTransport{
		sendErr: &transport.TransportError{Class: transport.ErrTransient, Message: "down"},
	}
	backup := &recordingTransport{sendResult: transport.SendResult{MessageID: "ses-1"}}
	router := routerOver(t, "/test", entry{"postmark-prod", primary}, entry{"ses-backup", backup})

	reg := metrics.New()
	rec := metrics.NewRecorder(reg)
	h, err := gateway.New(pooledCfg("/test", "postmark-prod", "ses-backup"), router,
		gateway.WithRecorder(rec))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	w := httptest.NewRecorder()
	h.ServeHTTP(w, urlencodedRequest("name=x"))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}

	var sb strings.Builder
	if err := reg.Emit(&sb); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sb.String(), `posthorn_submissions_sent_total{endpoint="/test",transport="ses-backup"} 1`) {
		t.Errorf("sent metric not labelled with the delivering provider:\n%s", sb.String())
	}
}

// On failure the Router stamps the provider onto the TransportError, so
// failure metrics stay per-provider too.
func TestHandler_FailedMetricUsesFailingProvider(t *testing.T) {
	down := &recordingTransport{
		sendErr: &transport.TransportError{Class: transport.ErrTerminal, Status: 422, Message: "rejected"},
	}
	router := routerOver(t, "/test", entry{"postmark-prod", down})

	reg := metrics.New()
	rec := metrics.NewRecorder(reg)
	h, err := gateway.New(pooledCfg("/test", "postmark-prod"), router, gateway.WithRecorder(rec))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	w := httptest.NewRecorder()
	h.ServeHTTP(w, urlencodedRequest("name=x"))
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", w.Code)
	}

	var sb strings.Builder
	if err := reg.Emit(&sb); err != nil {
		t.Fatal(err)
	}
	want := `posthorn_submissions_failed_total{endpoint="/test",transport="postmark-prod",error_class="terminal"} 1`
	if !strings.Contains(sb.String(), want) {
		t.Errorf("failure metric missing %q:\n%s", want, sb.String())
	}
}

// An inline endpoint's labels must be byte-identical to v1.x.
func TestHandler_InlineEndpointLabelsUnchanged(t *testing.T) {
	tr := &recordingTransport{sendResult: transport.SendResult{MessageID: "pm-1"}}
	cfg := config.EndpointConfig{
		Path: "/test", To: []string{"to@example.com"}, From: "from@example.com",
		Subject: "S", Body: "B",
		Transport: config.TransportConfig{Type: "postmark", Settings: map[string]any{"api_key": "k"}},
	}
	reg := metrics.New()
	rec := metrics.NewRecorder(reg)
	h, err := gateway.New(cfg, tr, gateway.WithRecorder(rec))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	w := httptest.NewRecorder()
	h.ServeHTTP(w, urlencodedRequest("name=x"))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	var sb strings.Builder
	if err := reg.Emit(&sb); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sb.String(), `posthorn_submissions_sent_total{endpoint="/test",transport="postmark"} 1`) {
		t.Errorf("inline label changed:\n%s", sb.String())
	}
}

// NFR24: nothing the submitter typed may reach the label space. The
// routing work added a provider dimension, so re-assert it here.
func TestHandler_NoSubmitterContentInProviderLabels(t *testing.T) {
	backup := &recordingTransport{sendResult: transport.SendResult{MessageID: "id"}}
	router := routerOver(t, "/test", entry{"ses-backup", backup})

	reg := metrics.New()
	rec := metrics.NewRecorder(reg)
	h, err := gateway.New(pooledCfg("/test", "ses-backup"), router, gateway.WithRecorder(rec))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	const sentinel = "cardinality-bomb-9f3a"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, urlencodedRequest("name="+sentinel+"&email="+sentinel+"%40evil.example"))

	var sb strings.Builder
	if err := reg.Emit(&sb); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(sb.String(), sentinel) {
		t.Errorf("submitter content reached /metrics:\n%s", sb.String())
	}
}

// --- logging -----------------------------------------------------------

func TestHandler_LogsDeliveringProvider(t *testing.T) {
	var buf strings.Builder
	logger := slog.New(slog.NewJSONHandler(&buf, nil))

	primary := &recordingTransport{
		sendErr: &transport.TransportError{Class: transport.ErrTransient, Message: "down"},
	}
	backup := &recordingTransport{sendResult: transport.SendResult{MessageID: "ses-1"}}
	router := routerOver(t, "/test", entry{"postmark-prod", primary}, entry{"ses-backup", backup})

	h, err := gateway.New(pooledCfg("/test", "postmark-prod", "ses-backup"), router,
		gateway.WithLogger(logger))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	w := httptest.NewRecorder()
	h.ServeHTTP(w, urlencodedRequest("name=x"))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	out := buf.String()
	if !strings.Contains(out, `"provider":"ses-backup"`) {
		t.Errorf("submission_sent has no provider field:\n%s", out)
	}
	if !strings.Contains(out, `"transport":"pool"`) {
		t.Errorf("request logger should carry the static pool label:\n%s", out)
	}
}

// --- dry run -----------------------------------------------------------

// Routing rules are the one part of the config whose effect cannot be
// read off the TOML, so dry-run has to answer "where would this go".
func TestHandler_DryRunExplainsTheRoute(t *testing.T) {
	bulk := &recordingTransport{}
	std := &recordingTransport{}

	stdProv := &transport.Provider{Name: "standard", Weight: 1, Transport: std,
		Breaker: transport.NewBreaker(transport.BreakerConfig{Disabled: true})}
	bulkProv := &transport.Provider{Name: "bulk", Weight: 1, Transport: bulk,
		Breaker: transport.NewBreaker(transport.BreakerConfig{Disabled: true})}

	def, err := transport.NewRoute(nil, transport.StrategyFailover, []*transport.Provider{stdProv})
	if err != nil {
		t.Fatal(err)
	}
	rule, err := transport.NewRoute(
		&transport.RuleMatch{RecipientDomains: []string{"gmail.com"}},
		transport.StrategyRoundRobin, []*transport.Provider{bulkProv})
	if err != nil {
		t.Fatal(err)
	}
	router, err := transport.NewRouter("/test", def, []*transport.Route{rule})
	if err != nil {
		t.Fatal(err)
	}

	cfg := pooledCfg("/test", "standard")
	cfg.To = []string{"someone@gmail.com"}
	cfg.DryRun = true
	h, err := gateway.New(cfg, router)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	w := httptest.NewRecorder()
	h.ServeHTTP(w, urlencodedRequest("name=x"))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	var body struct {
		Status string `json:"status"`
		Route  *struct {
			MatchedRule int      `json:"matched_rule"`
			Strategy    string   `json:"strategy"`
			Providers   []string `json:"providers"`
		} `json:"route"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %q: %v", w.Body.String(), err)
	}
	if body.Status != "dry_run" {
		t.Fatalf("status = %q", body.Status)
	}
	if body.Route == nil {
		t.Fatal("dry-run response has no route block")
	}
	if body.Route.MatchedRule != 0 {
		t.Errorf("matched_rule = %d, want 0", body.Route.MatchedRule)
	}
	if body.Route.Strategy != "round_robin" {
		t.Errorf("strategy = %q, want round_robin", body.Route.Strategy)
	}
	if len(body.Route.Providers) != 1 || body.Route.Providers[0] != "bulk" {
		t.Errorf("providers = %v, want [bulk]", body.Route.Providers)
	}
	// A dry run sends nothing, whichever route it names.
	if len(bulk.sent)+len(std.sent) != 0 {
		t.Error("dry run reached a transport")
	}
}

// An inline endpoint has nothing to decide, so the block stays absent
// and the v1.x dry-run body is unchanged.
func TestHandler_DryRunOmitsRouteForInlineTransport(t *testing.T) {
	cfg := config.EndpointConfig{
		Path: "/test", To: []string{"to@example.com"}, From: "from@example.com",
		Subject: "S", Body: "B", DryRun: true,
		Transport: config.TransportConfig{Type: "postmark", Settings: map[string]any{"api_key": "k"}},
	}
	h, err := gateway.New(cfg, &recordingTransport{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	w := httptest.NewRecorder()
	h.ServeHTTP(w, urlencodedRequest("name=x"))
	if strings.Contains(w.Body.String(), `"route"`) {
		t.Errorf("inline dry-run grew a route block: %s", w.Body.String())
	}
}

// --- retry policy composition ------------------------------------------

// The handler still calls transport.SendWithRetry, which now wraps a
// Router. A pool that exhausts on transient errors gets one more full
// pass inside the request's hard timeout.
func TestHandler_RetryPolicyStillWrapsTheRouter(t *testing.T) {
	restore := gateway.SetRetryDelaysForTest(time.Millisecond, time.Millisecond, 10*time.Second)
	defer restore()

	calls := 0
	flaky := &countingTransport{fn: func() (transport.SendResult, error) {
		calls++
		if calls <= 2 {
			return transport.SendResult{}, &transport.TransportError{
				Class: transport.ErrTransient, Message: "down"}
		}
		return transport.SendResult{MessageID: "ok"}, nil
	}}
	router := routerOver(t, "/test", entry{"a", flaky}, entry{"b", flaky})

	h, err := gateway.New(pooledCfg("/test", "a", "b"), router)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, urlencodedRequest("name=x"))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 — the retry pass should have delivered", w.Code)
	}
	if calls != 3 {
		t.Errorf("attempts = %d, want 3 (two providers, then the first again after backoff)", calls)
	}
}

type countingTransport struct {
	fn func() (transport.SendResult, error)
}

func (c *countingTransport) Send(context.Context, transport.Message) (transport.SendResult, error) {
	return c.fn()
}
