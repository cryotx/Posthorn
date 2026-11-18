package gateway_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/craigmccaskill/posthorn/config"
	"github.com/craigmccaskill/posthorn/gateway"
	"github.com/craigmccaskill/posthorn/metrics"
	"github.com/craigmccaskill/posthorn/project"
	"github.com/craigmccaskill/posthorn/storage"
	"github.com/craigmccaskill/posthorn/transport"
)

const (
	novelKey   = "novel-key-0123456789abcdef"
	accountKey = "account-key-0123456789abcdef"
)

func tenantRegistry(t *testing.T) *project.Registry {
	t.Helper()
	reg, err := project.Build([]project.Config{
		{
			Name: "novel", Enabled: true, APIKey: novelKey,
			DefaultFrom:        "noreply@novel.example.com",
			AllowedFromDomains: []string{"novel.example.com"},
		},
		{
			Name: "account", Enabled: true, APIKey: accountKey,
			DefaultFrom:        "account@example.com",
			AllowedFromDomains: []string{"example.com"},
		},
	})
	if err != nil {
		t.Fatalf("project.Build: %v", err)
	}
	return reg
}

// tenantHandler wires an api-mode endpoint with the project registry
// and storage, which is the shape a real project deployment runs.
func tenantHandler(t *testing.T, tr transport.Transport, rec *metrics.Recorder) (*gateway.Handler, *storage.Gate) {
	t.Helper()
	cfg := apiModeConfig("legacy-endpoint-key")
	opts := []gateway.Option{}
	if rec != nil {
		opts = append(opts, gateway.WithRecorder(rec))
	}
	h, err := gateway.New(cfg, tr, opts...)
	if err != nil {
		t.Fatalf("gateway.New: %v", err)
	}
	gate := newGate(t)
	h.AttachStorage(gate)
	h.AttachProjects(tenantRegistry(t), gateway.NewProjectUsage(gate))
	return h, gate
}

func post(t *testing.T, h *gateway.Handler, body, key string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := apiRequest(body, key)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

// --- authentication ----------------------------------------------------

func TestTenant_ValidProjectKeyAuthenticates(t *testing.T) {
	tr := &recordingTransport{sendResult: transport.SendResult{MessageID: "pm-1"}}
	h, _ := tenantHandler(t, tr, nil)

	w := post(t, h, `{"message":"hi"}`, novelKey, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if len(tr.sent) != 1 {
		t.Fatalf("sent %d messages, want 1", len(tr.sent))
	}
	if tr.sent[0].From != "noreply@novel.example.com" {
		t.Errorf("From = %q, want the project default", tr.sent[0].From)
	}
}

func TestTenant_WrongProjectKeyRejected(t *testing.T) {
	tr := &recordingTransport{}
	h, _ := tenantHandler(t, tr, nil)

	w := post(t, h, `{"message":"hi"}`, "novel-key-WRONG-000000000000", nil)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
	if len(tr.sent) != 0 {
		t.Error("a message was sent on a bad key")
	}
}

// A valid credential for a suspended project must NOT fall through to
// the endpoint's own api_keys — otherwise disabling a project would
// depend on it not sharing a key by accident.
func TestTenant_DisabledProjectRejected(t *testing.T) {
	reg, err := project.Build([]project.Config{{
		Name: "novel", Enabled: false, APIKey: novelKey,
		DefaultFrom:        "noreply@novel.example.com",
		AllowedFromDomains: []string{"novel.example.com"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	tr := &recordingTransport{}
	h, herr := gateway.New(apiModeConfig("legacy-endpoint-key"), tr)
	if herr != nil {
		t.Fatal(herr)
	}
	h.AttachProjects(reg, nil)

	w := post(t, h, `{"message":"hi"}`, novelKey, nil)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 for a disabled project", w.Code)
	}
	if len(tr.sent) != 0 {
		t.Error("a disabled project sent a message")
	}
}

// An endpoint's own api_keys keep working alongside projects, so an
// operator can adopt project auth one endpoint at a time.
func TestTenant_EndpointKeyStillWorksAlongsideProjects(t *testing.T) {
	tr := &recordingTransport{sendResult: transport.SendResult{MessageID: "pm-1"}}
	h, _ := tenantHandler(t, tr, nil)

	w := post(t, h, `{"message":"hi"}`, "legacy-endpoint-key", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	// No project context, so the endpoint's configured from applies and
	// the response carries no message_id.
	if tr.sent[0].From != "from@example.com" {
		t.Errorf("From = %q, want the endpoint default", tr.sent[0].From)
	}
	if strings.Contains(w.Body.String(), "message_id") {
		t.Errorf("non-project response grew a message_id: %s", w.Body.String())
	}
}

// --- sender policy (P0) ------------------------------------------------

func TestTenant_InDomainFromAccepted(t *testing.T) {
	tr := &recordingTransport{sendResult: transport.SendResult{MessageID: "pm-1"}}
	h, _ := tenantHandler(t, tr, nil)

	w := post(t, h, `{"message":"hi","from":"support@novel.example.com"}`, novelKey, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	if tr.sent[0].From != "support@novel.example.com" {
		t.Errorf("From = %q", tr.sent[0].From)
	}
}

// The headline requirement: one project must not be able to send as
// another's domain.
func TestTenant_CrossProjectFromSpoofingRejected(t *testing.T) {
	tr := &recordingTransport{}
	reg := metrics.New()
	rec := metrics.NewRecorder(reg)
	h, _ := tenantHandler(t, tr, rec)

	w := post(t, h, `{"message":"hi","from":"admin@example.com"}`, novelKey, nil)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403: %s", w.Code, w.Body.String())
	}
	if len(tr.sent) != 0 {
		t.Fatal("a spoofed sender reached the transport")
	}

	var body struct{ Code string }
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Code != "sender_not_allowed" {
		t.Errorf("code = %q, want sender_not_allowed", body.Code)
	}

	var sb strings.Builder
	if err := reg.Emit(&sb); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sb.String(), `posthorn_project_send_total{project="novel",status="sender_rejected"} 1`) {
		t.Errorf("rejection not counted:\n%s", sb.String())
	}
}

// A `project` field in the body must never influence identity — that
// comes from the authenticated credential only.
func TestTenant_BodyProjectFieldIsIgnored(t *testing.T) {
	tr := &recordingTransport{}
	h, _ := tenantHandler(t, tr, nil)

	w := post(t, h, `{"message":"hi","project":"account","from":"account@example.com"}`, novelKey, nil)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 — a body field claimed another project", w.Code)
	}
	if len(tr.sent) != 0 {
		t.Error("the spoofed project field took effect")
	}
}

func TestTenant_EachProjectUsesItsOwnDefault(t *testing.T) {
	tr := &recordingTransport{sendResult: transport.SendResult{MessageID: "x"}}
	h, _ := tenantHandler(t, tr, nil)

	if w := post(t, h, `{"message":"a"}`, novelKey, nil); w.Code != http.StatusOK {
		t.Fatalf("novel: %d", w.Code)
	}
	if w := post(t, h, `{"message":"b"}`, accountKey, nil); w.Code != http.StatusOK {
		t.Fatalf("account: %d", w.Code)
	}
	if tr.sent[0].From != "noreply@novel.example.com" || tr.sent[1].From != "account@example.com" {
		t.Errorf("froms = %q, %q", tr.sent[0].From, tr.sent[1].From)
	}
}

// --- idempotency -------------------------------------------------------

func TestTenant_IdenticalRequestReplaysWithoutResending(t *testing.T) {
	tr := &recordingTransport{sendResult: transport.SendResult{MessageID: "pm-1"}}
	h, _ := tenantHandler(t, tr, nil)
	hdr := map[string]string{"Idempotency-Key": "verify:user-1"}

	first := post(t, h, `{"message":"code 123456"}`, novelKey, hdr)
	if first.Code != http.StatusOK {
		t.Fatalf("first = %d: %s", first.Code, first.Body.String())
	}
	second := post(t, h, `{"message":"code 123456"}`, novelKey, hdr)
	if second.Code != http.StatusOK {
		t.Fatalf("second = %d: %s", second.Code, second.Body.String())
	}

	if len(tr.sent) != 1 {
		t.Errorf("provider saw %d sends, want 1", len(tr.sent))
	}
	if first.Body.String() != second.Body.String() {
		t.Errorf("replay body differs:\n%s\n%s", first.Body.String(), second.Body.String())
	}
}

// Re-serialising the same request with different key order or spacing
// is the same request and must replay, not conflict.
func TestTenant_ReorderedPayloadStillReplays(t *testing.T) {
	tr := &recordingTransport{sendResult: transport.SendResult{MessageID: "pm-1"}}
	h, _ := tenantHandler(t, tr, nil)
	hdr := map[string]string{"Idempotency-Key": "k1"}

	post(t, h, `{"message":"hi","subject_line":"s"}`, novelKey, hdr)
	w := post(t, h, `{"subject_line":"s",  "message":"hi"}`, novelKey, hdr)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want a replay: %s", w.Code, w.Body.String())
	}
	if len(tr.sent) != 1 {
		t.Errorf("provider saw %d sends, want 1", len(tr.sent))
	}
}

func TestTenant_DifferentPayloadSameKeyConflicts(t *testing.T) {
	tr := &recordingTransport{sendResult: transport.SendResult{MessageID: "pm-1"}}
	h, _ := tenantHandler(t, tr, nil)
	hdr := map[string]string{"Idempotency-Key": "k1"}

	post(t, h, `{"message":"first"}`, novelKey, hdr)
	w := post(t, h, `{"message":"DIFFERENT"}`, novelKey, hdr)

	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", w.Code, w.Body.String())
	}
	var body struct{ Code string }
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if body.Code != "idempotency_key_reused" {
		t.Errorf("code = %q, want idempotency_key_reused", body.Code)
	}
	if len(tr.sent) != 1 {
		t.Errorf("provider saw %d sends, want 1 — the conflicting request must not send", len(tr.sent))
	}
}

// Two projects using the same UUID must not collide: the key is scoped
// to the project, so each gets its own message.
func TestTenant_IdempotencyKeysAreScopedPerProject(t *testing.T) {
	tr := &recordingTransport{sendResult: transport.SendResult{MessageID: "pm-1"}}
	h, _ := tenantHandler(t, tr, nil)
	hdr := map[string]string{"Idempotency-Key": "shared-uuid"}

	if w := post(t, h, `{"message":"novel"}`, novelKey, hdr); w.Code != http.StatusOK {
		t.Fatalf("novel = %d: %s", w.Code, w.Body.String())
	}
	if w := post(t, h, `{"message":"account"}`, accountKey, hdr); w.Code != http.StatusOK {
		t.Fatalf("account = %d: %s", w.Code, w.Body.String())
	}
	if len(tr.sent) != 2 {
		t.Errorf("provider saw %d sends, want 2 — one per project", len(tr.sent))
	}
}

// The requirement that motivates database-level reservation: 100
// concurrent requests carrying one key must produce exactly one email.
func TestTenant_ConcurrentSameKeySendsOnce(t *testing.T) {
	tr := &recordingTransport{sendResult: transport.SendResult{MessageID: "pm-1"}}
	h, _ := tenantHandler(t, tr, nil)

	const callers = 100
	var wg sync.WaitGroup
	codes := make(chan int, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := post(t, h, `{"message":"code 123456"}`, novelKey,
				map[string]string{"Idempotency-Key": "burst"})
			codes <- w.Code
		}()
	}
	wg.Wait()
	close(codes)

	var ok, conflict, other int
	for c := range codes {
		switch c {
		case http.StatusOK:
			ok++
		case http.StatusConflict:
			conflict++
		default:
			other++
		}
	}
	if len(tr.sent) != 1 {
		t.Errorf("provider saw %d sends, want exactly 1 — a duplicate verification code went out", len(tr.sent))
	}
	if other != 0 {
		t.Errorf("%d callers got an unexpected status", other)
	}
	if ok < 1 {
		t.Error("no caller received the successful response")
	}
	t.Logf("%d concurrent callers: %d ok, %d conflict, 1 provider send", callers, ok, conflict)
}

// A failed send must hand the key back, so the caller's retry can
// actually reach a provider.
func TestTenant_FailedSendReleasesTheKey(t *testing.T) {
	tr := &recordingTransport{sendErr: terminalErr()}
	h, _ := tenantHandler(t, tr, nil)
	hdr := map[string]string{"Idempotency-Key": "k1"}

	if w := post(t, h, `{"message":"hi"}`, novelKey, hdr); w.Code != http.StatusBadGateway {
		t.Fatalf("first = %d, want 502", w.Code)
	}
	tr.sendErr = nil
	tr.sendResult = transport.SendResult{MessageID: "pm-1"}
	if w := post(t, h, `{"message":"hi"}`, novelKey, hdr); w.Code != http.StatusOK {
		t.Fatalf("retry = %d, want 200 — a failed send must not hold the key", w.Code)
	}
	if len(tr.sent) != 2 {
		t.Errorf("provider saw %d sends, want 2", len(tr.sent))
	}
}

// --- message id --------------------------------------------------------

// One logical message keeps one id across a provider failover, while
// the provider's own id is whatever the provider that took it returned.
func TestTenant_MessageIDStableAcrossFailover(t *testing.T) {
	primary := &recordingTransport{sendErr: transientErr()}
	backup := &recordingTransport{sendResult: transport.SendResult{MessageID: "ses-999"}}
	router := routerOver(t, "/api/transactional",
		entry{"primary", primary}, entry{"backup", backup})

	cfg := apiModeConfig("legacy-endpoint-key")
	cfg.Transport = config.TransportConfig{Providers: []string{"primary", "backup"}}
	h, err := gateway.New(cfg, router)
	if err != nil {
		t.Fatal(err)
	}
	gate := newGate(t)
	h.AttachStorage(gate)
	h.AttachProjects(tenantRegistry(t), gateway.NewProjectUsage(gate))

	w := post(t, h, `{"message":"hi"}`, novelKey, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}

	var body struct {
		SubmissionID string `json:"submission_id"`
		MessageID    string `json:"message_id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.MessageID == "" {
		t.Fatal("project response carries no message_id")
	}
	// One logical message, one id — not a second identifier bolted on.
	if body.MessageID != body.SubmissionID {
		t.Errorf("message_id %q != submission_id %q", body.MessageID, body.SubmissionID)
	}

	sub, ok, _ := gate.Store().GetSubmission(body.MessageID)
	if !ok {
		t.Fatal("submission row missing")
	}
	if sub.Project != "novel" {
		t.Errorf("row project = %q, want novel", sub.Project)
	}
	if sub.TransportMessageID != "ses-999" {
		t.Errorf("provider message id = %q, want the backup's", sub.TransportMessageID)
	}
}

// --- metrics + usage ---------------------------------------------------

func TestTenant_ProjectMetrics(t *testing.T) {
	tr := &recordingTransport{sendResult: transport.SendResult{MessageID: "pm-1"}}
	reg := metrics.New()
	rec := metrics.NewRecorder(reg)
	h, _ := tenantHandler(t, tr, rec)

	post(t, h, `{"message":"a"}`, novelKey, nil)
	post(t, h, `{"message":"b"}`, novelKey, nil)
	post(t, h, `{"message":"c"}`, accountKey, nil)

	var sb strings.Builder
	if err := reg.Emit(&sb); err != nil {
		t.Fatal(err)
	}
	out := sb.String()
	for _, want := range []string{
		`posthorn_project_send_total{project="novel",status="sent"} 2`,
		`posthorn_project_send_total{project="account",status="sent"} 1`,
		`posthorn_project_send_duration_seconds_count{project="novel"} 2`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

// NFR47: nothing a caller supplies may become a label. Re-asserted for
// the tenant surface, which added a whole new label dimension.
func TestTenant_NoRequestContentInProjectLabels(t *testing.T) {
	tr := &recordingTransport{sendResult: transport.SendResult{MessageID: "pm-1"}}
	reg := metrics.New()
	rec := metrics.NewRecorder(reg)
	h, _ := tenantHandler(t, tr, rec)

	const sentinel = "tenant-cardinality-bomb-4f2a"
	post(t, h, `{"message":"`+sentinel+`","to_override":"`+sentinel+`@evil.test"}`, novelKey,
		map[string]string{"Idempotency-Key": sentinel})

	var sb strings.Builder
	if err := reg.Emit(&sb); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(sb.String(), sentinel) {
		t.Errorf("request content reached /metrics:\n%s", sb.String())
	}
	// And the API key itself must never appear.
	if strings.Contains(sb.String(), novelKey) {
		t.Error("an API key reached /metrics")
	}
}

func TestTenant_UsageCountersPersist(t *testing.T) {
	tr := &recordingTransport{sendResult: transport.SendResult{MessageID: "pm-1"}}
	h, gate := tenantHandler(t, tr, nil)

	usage := gateway.NewProjectUsage(gate)
	h.AttachProjects(tenantRegistry(t), usage)
	for i := 0; i < 3; i++ {
		post(t, h, `{"message":"x"}`, novelKey, nil)
	}
	usage.Flush()

	now := time.Now()
	day, month := transport.DayKey(now), transport.MonthKey(now)
	today, _, err := gate.Store().ProjectUsageAt("novel", day, month)
	if err != nil {
		t.Fatal(err)
	}
	if today.Success != 3 {
		t.Errorf("success = %d, want 3", today.Success)
	}
}

// --- backwards compatibility -------------------------------------------

// NFR42: an endpoint with no project registry behaves exactly as it did
// before this release.
func TestTenant_NoProjectsConfiguredIsUnchanged(t *testing.T) {
	tr := &recordingTransport{sendResult: transport.SendResult{MessageID: "pm-1"}}
	h, err := gateway.New(apiModeConfig("valid-key"), tr)
	if err != nil {
		t.Fatal(err)
	}

	w := post(t, h, `{"message":"hi","from":"anything@anywhere.test"}`, "valid-key", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	// The endpoint's from still wins and the request `from` is inert,
	// exactly as ADR-11 requires outside project mode.
	if tr.sent[0].From != "from@example.com" {
		t.Errorf("From = %q, want the endpoint's configured sender", tr.sent[0].From)
	}
	var body map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if _, present := body["message_id"]; present {
		t.Error("non-project response grew a message_id field")
	}
}

// The legacy in-memory idempotency path must still work for endpoints
// with no project.
func TestTenant_LegacyIdempotencyStillWorks(t *testing.T) {
	tr := &recordingTransport{sendResult: transport.SendResult{MessageID: "pm-1"}}
	h, err := gateway.New(apiModeConfig("valid-key"), tr)
	if err != nil {
		t.Fatal(err)
	}
	hdr := map[string]string{"Idempotency-Key": "legacy"}
	post(t, h, `{"message":"hi"}`, "valid-key", hdr)
	w := post(t, h, `{"message":"hi"}`, "valid-key", hdr)
	if w.Code != http.StatusOK {
		t.Fatalf("replay = %d", w.Code)
	}
	if len(tr.sent) != 1 {
		t.Errorf("provider saw %d sends, want 1", len(tr.sent))
	}
}
