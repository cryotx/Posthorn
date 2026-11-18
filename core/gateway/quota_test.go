package gateway_test

import (
	"encoding/json"
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

// quotaRouterOver builds a router with a local tracker on a fixed clock.
func quotaRouterOver(t *testing.T, name string, clock *time.Time, provs ...*transport.Provider) *transport.Router {
	t.Helper()
	route, err := transport.NewRoute(nil, transport.StrategyFailover, provs)
	if err != nil {
		t.Fatalf("NewRoute: %v", err)
	}
	r, err := transport.NewRouter(name, route, nil)
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	r.Quota = transport.NewLocalQuota(nil, nil)
	r.Now = func() time.Time { return *clock }
	return r
}

func budgeted(name string, daily int64, tp transport.Transport) *transport.Provider {
	return &transport.Provider{
		Name: name, Weight: 1, Transport: tp,
		Breaker: transport.NewBreaker(transport.BreakerConfig{Disabled: true}),
		Quota:   transport.Quota{Daily: daily},
	}
}

// A gateway out of budget is not a broken gateway: nothing failed, the
// same message will send after the window rolls over, and the caller
// should be told to come back rather than to give up.
func TestHandler_QuotaExhaustedReturns503(t *testing.T) {
	clock := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	only := &recordingTransport{sendResult: transport.SendResult{MessageID: "1"}}
	router := quotaRouterOver(t, "/test", &clock, budgeted("free", 1, only))

	h, err := gateway.New(pooledCfg("/test", "free"), router)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	w := httptest.NewRecorder()
	h.ServeHTTP(w, urlencodedRequest("name=x"))
	if w.Code != http.StatusOK {
		t.Fatalf("first send = %d, want 200", w.Code)
	}

	w = httptest.NewRecorder()
	h.ServeHTTP(w, urlencodedRequest("name=x"))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("second send = %d, want 503", w.Code)
	}
	if !strings.Contains(w.Body.String(), "quota") {
		t.Errorf("body = %q, want it to name the quota", w.Body.String())
	}
	if len(only.sent) != 1 {
		t.Errorf("transport saw %d sends, want 1", len(only.sent))
	}
}

// A terminal delivery failure keeps its 502: only the new quota path
// produces 503, so no pre-quota deployment sees a changed status code.
func TestHandler_TerminalFailureStill502(t *testing.T) {
	tr := &recordingTransport{
		sendErr: &transport.TransportError{Class: transport.ErrTerminal, Status: 422, Message: "rejected"},
	}
	h := newTestHandler(t, tr)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, urlencodedRequest("name=x"))
	if w.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", w.Code)
	}
}

func TestHandler_QuotaExhaustionRecordsItsOwnMetric(t *testing.T) {
	clock := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	reg := metrics.New()
	rec := metrics.NewRecorder(reg)

	free := budgeted("free", 1, &recordingTransport{sendResult: transport.SendResult{MessageID: "1"}})
	paid := &transport.Provider{
		Name: "paid", Weight: 1,
		Transport: &recordingTransport{sendResult: transport.SendResult{MessageID: "2"}},
		Breaker:   transport.NewBreaker(transport.BreakerConfig{Disabled: true}),
	}
	router := quotaRouterOver(t, "/test", &clock, free, paid)
	router.Hooks = transport.RouterHooks{
		OnQuotaBlocked:        rec.QuotaExhausted,
		OnSkipped:             rec.ProviderSkipped,
		OnRouteQuotaExhausted: rec.RouteQuotaExhausted,
	}

	h, err := gateway.New(pooledCfg("/test", "free", "paid"), router, gateway.WithRecorder(rec))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for i := 0; i < 3; i++ {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, urlencodedRequest("name=x"))
		if w.Code != http.StatusOK {
			t.Fatalf("send %d = %d", i+1, w.Code)
		}
	}

	var sb strings.Builder
	if err := reg.Emit(&sb); err != nil {
		t.Fatal(err)
	}
	out := sb.String()
	if !strings.Contains(out, `posthorn_provider_quota_exhausted_total{provider="free",window="daily"} 2`) {
		t.Errorf("quota metric missing or wrong:\n%s", out)
	}
	// "Out of allowance" must not be reported as "provider skipped for
	// health" — an operator has to be able to tell them apart.
	if strings.Contains(out, `posthorn_provider_skipped_total{provider="free"}`) {
		t.Errorf("quota skips leaked into the circuit-skip series:\n%s", out)
	}
}

// --- mail kind ---------------------------------------------------------

func TestHandler_EndpointMailKindReachesTheMessage(t *testing.T) {
	tr := &recordingTransport{}
	cfg := config.EndpointConfig{
		Path: "/test", To: []string{"to@example.com"}, From: "from@example.com",
		Subject: "S", Body: "B", MailKind: "verification",
	}
	h, err := gateway.New(cfg, tr)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, urlencodedRequest("name=x"))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if len(tr.sent) != 1 || tr.sent[0].Kind != transport.KindVerification {
		t.Errorf("Kind = %q, want verification", tr.sent[0].Kind)
	}
}

func TestHandler_APIModeMailKindOverride(t *testing.T) {
	tr := &recordingTransport{}
	cfg := config.EndpointConfig{
		Path: "/test", To: []string{"to@example.com"}, From: "from@example.com",
		Subject: "S", Body: "B", MailKind: "notification",
		Auth: config.AuthAPIKey, APIKeys: []string{"k"},
	}
	h, err := gateway.New(cfg, tr)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/test",
		strings.NewReader(`{"mail_kind":"password_reset","name":"x"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer k")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	if tr.sent[0].Kind != transport.KindPasswordReset {
		t.Errorf("Kind = %q, want password_reset", tr.sent[0].Kind)
	}
}

// The override is bounded by the closed enum: a caller can pick among
// the kinds the operator's rules already mention and nothing else.
func TestHandler_APIModeMailKindRejectsUnknown(t *testing.T) {
	tr := &recordingTransport{}
	cfg := config.EndpointConfig{
		Path: "/test", To: []string{"to@example.com"}, From: "from@example.com",
		Subject: "S", Body: "B", Auth: config.AuthAPIKey, APIKeys: []string{"k"},
	}
	h, err := gateway.New(cfg, tr)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/test",
		strings.NewReader(`{"mail_kind":"../../admin","name":"x"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer k")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422: %s", w.Code, w.Body.String())
	}
	if len(tr.sent) != 0 {
		t.Error("a message was sent despite an invalid mail_kind")
	}
}

// Form mode ignores a submitted mail_kind: a browser form's purpose is
// a property of the endpoint the operator configured, not of what the
// page posted.
func TestHandler_FormModeIgnoresSubmittedMailKind(t *testing.T) {
	tr := &recordingTransport{}
	cfg := config.EndpointConfig{
		Path: "/test", To: []string{"to@example.com"}, From: "from@example.com",
		Subject: "S", Body: "B", MailKind: "notification",
	}
	h, err := gateway.New(cfg, tr)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, urlencodedRequest("name=x&mail_kind=important"))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if tr.sent[0].Kind != transport.KindNotification {
		t.Errorf("Kind = %q, want the endpoint's notification", tr.sent[0].Kind)
	}
}

func TestHandler_DryRunReportsMailKind(t *testing.T) {
	clock := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	router := quotaRouterOver(t, "/test", &clock,
		budgeted("free", 0, &recordingTransport{}))

	cfg := pooledCfg("/test", "free")
	cfg.MailKind = "marketing"
	cfg.DryRun = true
	h, err := gateway.New(cfg, router)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, urlencodedRequest("name=x"))

	var body struct {
		Route *struct {
			Kind string `json:"kind"`
		} `json:"route"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %q: %v", w.Body.String(), err)
	}
	if body.Route == nil || body.Route.Kind != "marketing" {
		t.Errorf("dry-run route = %+v, want kind marketing", body.Route)
	}
}

func TestNew_RejectsUnknownMailKind(t *testing.T) {
	cfg := config.EndpointConfig{
		Path: "/test", To: []string{"to@example.com"}, From: "from@example.com",
		Subject: "S", Body: "B", MailKind: "newsletter",
	}
	if _, err := gateway.New(cfg, &recordingTransport{}); err == nil {
		t.Error("New = nil error for an unknown mail_kind")
	}
}

// A message blocked on a spent budget must be HELD, not dropped: it
// will send perfectly well once the window rolls over. This is the
// distinction IsDeferrable exists to draw.
func TestStorage_QuotaExhaustion_QueuesRatherThanFails(t *testing.T) {
	clock := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	only := &recordingTransport{sendResult: transport.SendResult{MessageID: "1"}}
	router := quotaRouterOver(t, "/test", &clock, budgeted("free", 1, only))

	cfg := apiModeConfig("valid-key")
	cfg.Transport = config.TransportConfig{Providers: []string{"free"}}
	h, err := gateway.New(cfg, router)
	if err != nil {
		t.Fatal(err)
	}
	h.AttachStorage(newGate(t))

	// Spend the budget.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, apiRequest(`{"email":"a@b.com","message":"one"}`, "valid-key"))
	if rec.Code != http.StatusOK {
		t.Fatalf("first send = %d: %s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, apiRequest(`{"email":"a@b.com","message":"two"}`, "valid-key"))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("quota-blocked send = %d, want 202 queued: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Status       string `json:"status"`
		SubmissionID string `json:"submission_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Status != "queued" {
		t.Errorf("status = %q, want queued", resp.Status)
	}
	if len(only.sent) != 1 {
		t.Errorf("transport saw %d sends, want 1", len(only.sent))
	}
}
