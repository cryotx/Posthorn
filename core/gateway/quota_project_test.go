package gateway_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/craigmccaskill/posthorn/gateway"
	"github.com/craigmccaskill/posthorn/metrics"
	"github.com/craigmccaskill/posthorn/project"
	"github.com/craigmccaskill/posthorn/storage"
	"github.com/craigmccaskill/posthorn/transport"
)

// budgetedRegistry builds a registry with one limited project and one
// unlimited one, so every test can check that the limit applies to the
// project that has it and to nothing else.
func budgetedRegistry(t *testing.T, daily, monthly int64) *project.Registry {
	t.Helper()
	reg, err := project.Build([]project.Config{
		{
			Name: "novel", Enabled: true, APIKey: novelKey,
			DefaultFrom:        "noreply@novel.example.com",
			AllowedFromDomains: []string{"novel.example.com"},
			DailyLimit:         daily, MonthlyLimit: monthly,
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

// budgetedHandler wires an api-mode endpoint with a budgeted registry.
func budgetedHandler(t *testing.T, tr transport.Transport, daily, monthly int64, rec *metrics.Recorder) (*gateway.Handler, *gateway.ProjectUsage, *storage.Gate) {
	t.Helper()
	opts := []gateway.Option{}
	if rec != nil {
		opts = append(opts, gateway.WithRecorder(rec))
	}
	h, err := gateway.New(apiModeConfig("legacy-endpoint-key"), tr, opts...)
	if err != nil {
		t.Fatalf("gateway.New: %v", err)
	}
	gate := newGate(t)
	h.AttachStorage(gate)
	usage := gateway.NewProjectUsage(gate)
	h.AttachProjects(budgetedRegistry(t, daily, monthly), usage)
	return h, usage, gate
}

// --- enforcement -------------------------------------------------------

func TestProjectQuota_DailyLimitRefuses(t *testing.T) {
	tr := &recordingTransport{sendResult: transport.SendResult{MessageID: "pm-1"}}
	h, _, _ := budgetedHandler(t, tr, 2, 0, nil)

	for i := 1; i <= 2; i++ {
		if w := post(t, h, `{"message":"x"}`, novelKey, nil); w.Code != http.StatusOK {
			t.Fatalf("send %d = %d: %s", i, w.Code, w.Body.String())
		}
	}
	w := post(t, h, `{"message":"x"}`, novelKey, nil)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("send 3 = %d, want 429: %s", w.Code, w.Body.String())
	}

	var body struct {
		Code   string            `json:"code"`
		Fields map[string]string `json:"fields"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Code != "project_quota_exhausted" {
		t.Errorf("code = %q, want project_quota_exhausted", body.Code)
	}
	if body.Fields["window"] != "daily" {
		t.Errorf("window = %q, want daily", body.Fields["window"])
	}
	if len(tr.sent) != 2 {
		t.Errorf("provider saw %d sends, want 2", len(tr.sent))
	}
}

func TestProjectQuota_MonthlyLimitRefuses(t *testing.T) {
	tr := &recordingTransport{sendResult: transport.SendResult{MessageID: "pm-1"}}
	h, _, _ := budgetedHandler(t, tr, 0, 2, nil)

	post(t, h, `{"message":"a"}`, novelKey, nil)
	post(t, h, `{"message":"b"}`, novelKey, nil)
	w := post(t, h, `{"message":"c"}`, novelKey, nil)

	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", w.Code)
	}
	var body struct {
		Fields map[string]string `json:"fields"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if body.Fields["window"] != "monthly" {
		t.Errorf("window = %q, want monthly", body.Fields["window"])
	}
}

// One project's exhausted budget must not touch another's.
func TestProjectQuota_IsolatedPerProject(t *testing.T) {
	tr := &recordingTransport{sendResult: transport.SendResult{MessageID: "pm-1"}}
	h, _, _ := budgetedHandler(t, tr, 1, 0, nil)

	post(t, h, `{"message":"a"}`, novelKey, nil)
	if w := post(t, h, `{"message":"b"}`, novelKey, nil); w.Code != http.StatusTooManyRequests {
		t.Fatalf("novel = %d, want 429", w.Code)
	}
	// account has no limit and must be unaffected.
	for i := 0; i < 5; i++ {
		if w := post(t, h, `{"message":"c"}`, accountKey, nil); w.Code != http.StatusOK {
			t.Fatalf("account send %d = %d, want 200", i, w.Code)
		}
	}
}

// A project running out of its own budget is not a provider problem:
// no other provider is tried, no circuit is touched, nothing is queued.
func TestProjectQuota_DoesNotTouchProviderMachinery(t *testing.T) {
	primary := &recordingTransport{sendResult: transport.SendResult{MessageID: "a"}}
	backup := &recordingTransport{sendResult: transport.SendResult{MessageID: "b"}}
	router := routerOver(t, "/api/transactional", entry{"primary", primary}, entry{"backup", backup})

	h, err := gateway.New(apiModeConfig("legacy-endpoint-key"), router)
	if err != nil {
		t.Fatal(err)
	}
	gate := newGate(t)
	h.AttachStorage(gate)
	h.AttachProjects(budgetedRegistry(t, 1, 0), gateway.NewProjectUsage(gate))

	post(t, h, `{"message":"a"}`, novelKey, nil)
	w := post(t, h, `{"message":"b"}`, novelKey, nil)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", w.Code)
	}

	if len(primary.sent) != 1 {
		t.Errorf("primary saw %d sends, want 1", len(primary.sent))
	}
	if len(backup.sent) != 0 {
		t.Error("a project quota refusal failed over to another provider")
	}
	for _, p := range router.Providers() {
		if p.Breaker.State() != transport.CircuitClosed {
			t.Errorf("provider %s circuit = %v, want closed — a project budget is not a provider health event",
				p.Name, p.Breaker.State())
		}
	}
	if n, _ := gate.Store().QueueDepth(); n != 0 {
		t.Errorf("queue depth = %d, want 0 — a project quota refusal must not queue", n)
	}
}

// --- rollover ----------------------------------------------------------

func TestProjectQuota_RollsOverByPeriodKey(t *testing.T) {
	usage := gateway.NewProjectUsage(nil)
	reg := budgetedRegistry(t, 2, 0)
	p, _ := reg.ByName("novel")

	day1 := time.Date(2026, 9, 8, 23, 0, 0, 0, time.UTC)
	for i := 0; i < 2; i++ {
		commit, ok, _ := usage.Reserve(p, day1)
		if !ok {
			t.Fatalf("reservation %d refused", i)
		}
		commit(true)
	}
	if _, ok, w := usage.Reserve(p, day1); ok {
		t.Fatal("a third send was allowed against a limit of 2")
	} else if w != "daily" {
		t.Errorf("window = %q, want daily", w)
	}

	// One hour later is a new UTC day. Nothing ran at midnight.
	day2 := time.Date(2026, 9, 9, 0, 0, 0, 0, time.UTC)
	if _, ok, _ := usage.Reserve(p, day2); !ok {
		t.Error("the daily budget did not roll over")
	}
}

func TestProjectQuota_MonthlyRollsOverIndependently(t *testing.T) {
	usage := gateway.NewProjectUsage(nil)
	reg := budgetedRegistry(t, 0, 2)
	p, _ := reg.ByName("novel")

	sep := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 2; i++ {
		commit, _, _ := usage.Reserve(p, sep)
		commit(true)
	}
	if _, ok, _ := usage.Reserve(p, sep); ok {
		t.Fatal("september budget not exhausted")
	}
	oct := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	if _, ok, _ := usage.Reserve(p, oct); !ok {
		t.Error("the monthly budget did not roll over")
	}
}

// --- strict reservation ------------------------------------------------

// The scenario from the brief: limit 100, 99 spent, 100 goroutines.
// At most one may enter.
func TestProjectQuota_StrictUnderConcurrency(t *testing.T) {
	usage := gateway.NewProjectUsage(nil)
	reg := budgetedRegistry(t, 100, 0)
	p, _ := reg.ByName("novel")
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)

	for i := 0; i < 99; i++ {
		commit, ok, _ := usage.Reserve(p, now)
		if !ok {
			t.Fatalf("priming reservation %d refused", i)
		}
		commit(true)
	}

	const callers = 100
	var wg sync.WaitGroup
	granted := make(chan bool, callers)
	release := make(chan struct{})
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			commit, ok, _ := usage.Reserve(p, now)
			granted <- ok
			if !ok {
				return
			}
			<-release
			commit(true)
		}()
	}
	// Everyone has decided before any of them commits.
	for i := 0; i < callers; i++ {
		<-granted
	}
	close(release)
	wg.Wait()

	daily, _, _, _ := usage.Remaining(p, now)
	if daily != 0 {
		t.Errorf("remaining = %d, want 0", daily)
	}
	// Re-run the count: exactly one caller may have been granted.
	if _, ok, _ := usage.Reserve(p, now); ok {
		t.Error("budget remained after the limit was reached — more than one caller was granted")
	}
}

func TestProjectQuota_FailedSendReleasesReservation(t *testing.T) {
	tr := &recordingTransport{sendErr: terminalErr()}
	h, usage, _ := budgetedHandler(t, tr, 2, 0, nil)
	reg := budgetedRegistry(t, 2, 0)
	p, _ := reg.ByName("novel")

	for i := 0; i < 5; i++ {
		if w := post(t, h, `{"message":"x"}`, novelKey, nil); w.Code != http.StatusBadGateway {
			t.Fatalf("send %d = %d, want 502", i, w.Code)
		}
	}
	daily, _, _, _ := usage.Remaining(p, time.Now())
	if daily != 2 {
		t.Errorf("remaining = %d, want 2 — failed sends must not spend a tenant's allowance", daily)
	}
}

func TestProjectQuota_CommitIsIdempotent(t *testing.T) {
	usage := gateway.NewProjectUsage(nil)
	reg := budgetedRegistry(t, 10, 0)
	p, _ := reg.ByName("novel")
	now := time.Now()

	commit, ok, _ := usage.Reserve(p, now)
	if !ok {
		t.Fatal("reservation refused")
	}
	commit(true)
	commit(true)
	commit(false)

	daily, _, _, _ := usage.Remaining(p, now)
	if daily != 9 {
		t.Errorf("remaining = %d, want 9 after repeated commits", daily)
	}
}

// --- ordering with idempotency -----------------------------------------

// The ordering guarantee (ADR-42): a replay returns the recorded
// response BEFORE the quota reserve is reached, so it costs nothing.
func TestProjectQuota_ReplayDoesNotConsumeQuota(t *testing.T) {
	tr := &recordingTransport{sendResult: transport.SendResult{MessageID: "pm-1"}}
	h, usage, _ := budgetedHandler(t, tr, 5, 0, nil)
	reg := budgetedRegistry(t, 5, 0)
	p, _ := reg.ByName("novel")
	hdr := map[string]string{"Idempotency-Key": "verify:user-1"}

	if w := post(t, h, `{"message":"code"}`, novelKey, hdr); w.Code != http.StatusOK {
		t.Fatalf("first = %d", w.Code)
	}
	afterFirst, _, _, _ := usage.Remaining(p, time.Now())
	if afterFirst != 4 {
		t.Fatalf("remaining after first send = %d, want 4", afterFirst)
	}

	for i := 0; i < 5; i++ {
		if w := post(t, h, `{"message":"code"}`, novelKey, hdr); w.Code != http.StatusOK {
			t.Fatalf("replay %d = %d", i, w.Code)
		}
	}
	afterReplays, _, _, _ := usage.Remaining(p, time.Now())
	if afterReplays != 4 {
		t.Errorf("remaining after 5 replays = %d, want 4 — a replay must not spend quota", afterReplays)
	}
	if len(tr.sent) != 1 {
		t.Errorf("provider saw %d sends, want 1", len(tr.sent))
	}
}

// Concurrent duplicates must not over-spend either: the idempotency
// reservation runs first, so only the winner reaches the quota.
func TestProjectQuota_ConcurrentDuplicatesSpendOneUnit(t *testing.T) {
	tr := &recordingTransport{sendResult: transport.SendResult{MessageID: "pm-1"}}
	h, usage, _ := budgetedHandler(t, tr, 50, 0, nil)
	reg := budgetedRegistry(t, 50, 0)
	p, _ := reg.ByName("novel")

	const callers = 40
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			post(t, h, `{"message":"code"}`, novelKey,
				map[string]string{"Idempotency-Key": "burst"})
		}()
	}
	wg.Wait()

	daily, _, _, _ := usage.Remaining(p, time.Now())
	if daily != 49 {
		t.Errorf("remaining = %d, want 49 — %d concurrent duplicates spent more than one unit", daily, callers)
	}
	if len(tr.sent) != 1 {
		t.Errorf("provider saw %d sends, want 1", len(tr.sent))
	}
}

// A dry run sends nothing and must cost nothing.
func TestProjectQuota_DryRunDoesNotConsume(t *testing.T) {
	tr := &recordingTransport{}
	cfg := apiModeConfig("legacy-endpoint-key")
	cfg.DryRun = true
	h, err := gateway.New(cfg, tr)
	if err != nil {
		t.Fatal(err)
	}
	gate := newGate(t)
	h.AttachStorage(gate)
	usage := gateway.NewProjectUsage(gate)
	h.AttachProjects(budgetedRegistry(t, 1, 0), usage)
	reg := budgetedRegistry(t, 1, 0)
	p, _ := reg.ByName("novel")

	for i := 0; i < 3; i++ {
		if w := post(t, h, `{"message":"x"}`, novelKey, nil); w.Code != http.StatusOK {
			t.Fatalf("dry run %d = %d", i, w.Code)
		}
	}
	daily, _, _, _ := usage.Remaining(p, time.Now())
	if daily != 1 {
		t.Errorf("remaining = %d, want 1 — a dry run must not spend quota", daily)
	}
}

// --- persistence + metrics ---------------------------------------------

func TestProjectQuota_SurvivesRestart(t *testing.T) {
	tr := &recordingTransport{sendResult: transport.SendResult{MessageID: "pm-1"}}
	h, usage, gate := budgetedHandler(t, tr, 2, 0, nil)

	post(t, h, `{"message":"a"}`, novelKey, nil)
	post(t, h, `{"message":"b"}`, novelKey, nil)
	usage.Flush()

	// A fresh tracker over the same store, as a restart produces.
	reg := budgetedRegistry(t, 2, 0)
	restarted := gateway.NewProjectUsage(gate)
	if err := restarted.Restore(reg.All(), time.Now()); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	p, _ := reg.ByName("novel")
	if _, ok, _ := restarted.Reserve(p, time.Now()); ok {
		t.Error("the budget restarted with the process")
	}
}

func TestProjectQuota_Metrics(t *testing.T) {
	tr := &recordingTransport{sendResult: transport.SendResult{MessageID: "pm-1"}}
	reg := metrics.New()
	rec := metrics.NewRecorder(reg)
	h, _, _ := budgetedHandler(t, tr, 1, 0, rec)

	post(t, h, `{"message":"a"}`, novelKey, nil)
	post(t, h, `{"message":"b"}`, novelKey, nil)

	var sb strings.Builder
	if err := reg.Emit(&sb); err != nil {
		t.Fatal(err)
	}
	out := sb.String()
	for _, want := range []string{
		`posthorn_project_quota_exhausted_total{project="novel",window="daily"} 1`,
		`posthorn_project_send_total{project="novel",status="quota_exhausted"} 1`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	// NFR47 again: nothing from the request may become a label.
	if strings.Contains(out, novelKey) {
		t.Error("an API key reached /metrics")
	}
}

// NFR42 restated: a project with no limits behaves exactly as before.
func TestProjectQuota_UnlimitedProjectUnaffected(t *testing.T) {
	tr := &recordingTransport{sendResult: transport.SendResult{MessageID: "pm-1"}}
	h, _, _ := budgetedHandler(t, tr, 0, 0, nil)

	for i := 0; i < 20; i++ {
		if w := post(t, h, `{"message":"x"}`, novelKey, nil); w.Code != http.StatusOK {
			t.Fatalf("send %d = %d, want 200", i, w.Code)
		}
	}
	if len(tr.sent) != 20 {
		t.Errorf("provider saw %d sends, want 20", len(tr.sent))
	}
}

// And a deployment with no projects at all is untouched.
func TestProjectQuota_NoProjectsIsUnchanged(t *testing.T) {
	tr := &recordingTransport{sendResult: transport.SendResult{MessageID: "pm-1"}}
	h, err := gateway.New(apiModeConfig("valid-key"), tr)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, apiRequest(`{"message":"x"}`, "valid-key"))
		if w.Code != http.StatusOK {
			t.Fatalf("send %d = %d", i, w.Code)
		}
	}
}

// A crash must not refund a limited project's spent budget. The
// counter for a project with a limit is the budget itself, so it is
// written through rather than buffered — a buffered one would hand the
// tenant extra allowance on restart.
func TestProjectQuota_UsageIsDurableImmediately(t *testing.T) {
	tr := &recordingTransport{sendResult: transport.SendResult{MessageID: "pm-1"}}
	h, _, gate := budgetedHandler(t, tr, 5, 0, nil)

	post(t, h, `{"message":"a"}`, novelKey, nil)
	post(t, h, `{"message":"b"}`, novelKey, nil)

	// No Flush() call: the durable row must already be there, because
	// a crash here would otherwise lose the count.
	now := time.Now()
	today, _, err := gate.Store().ProjectUsageAt("novel",
		transport.DayKey(now), transport.MonthKey(now))
	if err != nil {
		t.Fatal(err)
	}
	if today.Success != 2 {
		t.Errorf("durable success = %d, want 2 without an explicit flush", today.Success)
	}
}
