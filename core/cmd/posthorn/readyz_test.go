package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/craigmccaskill/posthorn/config"
	"github.com/craigmccaskill/posthorn/gateway"
	"github.com/craigmccaskill/posthorn/metrics"
	"github.com/craigmccaskill/posthorn/project"
	"github.com/craigmccaskill/posthorn/storage"
	"github.com/craigmccaskill/posthorn/transport"
)

// /healthz and /readyz answer different questions, and the difference
// decides what a supervisor does: restart me, versus stop sending me
// traffic. These tests pin that they are not wired to the same signals.

func readyMux(t *testing.T, withStorage bool, ready *workerReadiness) *http.ServeMux {
	t.Helper()
	cfg := &config.Config{
		Endpoints: []config.EndpointConfig{{
			Path: "/api/contact", To: []string{"to@example.com"},
			From: "from@example.com", Subject: "S", Body: "B",
			Transport: config.TransportConfig{
				Type: "postmark", Settings: map[string]any{"api_key": "k"},
			},
		}},
	}
	var gate *storage.Gate
	if withStorage {
		cfg.Storage = &config.StorageConfig{InMemory: true}
		store, err := storage.Open(storage.Config{InMemory: true})
		if err != nil {
			t.Fatalf("storage.Open: %v", err)
		}
		t.Cleanup(func() { _ = store.Close() })
		gate = storage.NewGate(store, buildLogger(config.LoggingConfig{}))
	}
	if ready == nil {
		ready = newWorkerReadiness(cfg)
	}
	reg := metrics.New()
	rec := metrics.NewRecorder(reg)
	pool, err := buildPool(cfg.Providers, rec)
	if err != nil {
		t.Fatalf("buildPool: %v", err)
	}
	projects, _ := project.Build(nil)
	mux, _, err := buildMux(cfg, pool, transport.QuotaSource(nil), projects,
		gateway.NewProjectUsage(nil), buildLogger(config.LoggingConfig{}), reg, rec, gate, ready)
	if err != nil {
		t.Fatalf("buildMux: %v", err)
	}
	return mux
}

func probe(t *testing.T, mux *http.ServeMux, path string) (int, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	return rec.Code, body
}

// A deployment with no workers to wait for is ready immediately.
func TestReadyz_ReadyWithoutStorage(t *testing.T) {
	mux := readyMux(t, false, nil)
	code, body := probe(t, mux, "/readyz")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %+v", code, body)
	}
	if body["status"] != "ready" {
		t.Errorf("status = %v", body["status"])
	}
}

// TestReadyz_NotReadyUntilWorkersStart is the gap the probe exists for:
// between the listener coming up and the queue drainer starting,
// Posthorn will accept work nothing is draining.
func TestReadyz_NotReadyUntilWorkersStart(t *testing.T) {
	cfg := &config.Config{Storage: &config.StorageConfig{InMemory: true}}
	ready := newWorkerReadiness(cfg)
	mux := readyMux(t, true, ready)

	code, body := probe(t, mux, "/readyz")
	if code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 before workers start: %+v", code, body)
	}
	checks, _ := body["checks"].(map[string]any)
	if checks["workers"] != "starting" {
		t.Errorf("workers check = %v, want it to name the reason", checks["workers"])
	}

	// A probe that gets 503 must be told to back off rather than spin.
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Header().Get("Retry-After") == "" {
		t.Error("no Retry-After on a not-ready response")
	}

	ready.markStarted()
	ready.markStarted()
	if code, body = probe(t, mux, "/readyz"); code != http.StatusOK {
		t.Errorf("status after workers started = %d: %+v", code, body)
	}
}

// TestReadyz_ProviderOutageDoesNotUnready is the requirement that
// matters most here: an upstream outage is what the retry queue, the
// breaker and failover exist to absorb. Reporting unready would turn
// mail that would have been queued into mail refused at the door.
func TestReadyz_ProviderOutageDoesNotUnready(t *testing.T) {
	cfg := &config.Config{
		Providers: []config.ProviderConfig{{
			Name: "postmark-main", Type: "postmark",
			Settings: map[string]any{"api_key": "k"},
		}},
		Endpoints: []config.EndpointConfig{{
			Path: "/api/contact", To: []string{"to@example.com"},
			From: "from@example.com", Subject: "S", Body: "B",
			Transport: config.TransportConfig{Providers: []string{"postmark-main"}},
		}},
	}
	reg := metrics.New()
	rec := metrics.NewRecorder(reg)
	pool, err := buildPool(cfg.Providers, rec)
	if err != nil {
		t.Fatalf("buildPool: %v", err)
	}
	projects, _ := project.Build(nil)
	ready := newWorkerReadiness(cfg)
	mux, _, err := buildMux(cfg, pool, transport.QuotaSource(nil), projects,
		gateway.NewProjectUsage(nil), buildLogger(config.LoggingConfig{}), reg, rec, nil, ready)
	if err != nil {
		t.Fatalf("buildMux: %v", err)
	}

	// Trip the breaker open and disable the provider outright: the
	// worst upstream state there is.
	p, ok := pool.Get("postmark-main")
	if !ok {
		t.Fatal("provider not in pool")
	}
	for i := 0; i < 20; i++ {
		p.Breaker.Failure(&transport.TransportError{Class: transport.ErrTransient})
	}
	p.SetAdminDisabled(true)
	if p.Breaker.State() == transport.CircuitClosed {
		t.Fatal("breaker did not open; the test is not exercising an outage")
	}

	code, body := probe(t, mux, "/readyz")
	if code != http.StatusOK {
		t.Errorf("/readyz = %d with every provider down, want 200: %+v", code, body)
	}
	// And /healthz stays 200 too — restarting does not fix an upstream.
	if code, _ = probe(t, mux, "/healthz"); code != http.StatusOK {
		t.Errorf("/healthz = %d with every provider down, want 200", code)
	}
}

// Degraded storage means Posthorn should stop taking traffic, but a
// restart will not fix a failing disk — so /readyz fails and /healthz
// does not.
func TestReadyz_DegradedStorageIsNotReadyButStaysAlive(t *testing.T) {
	cfg := &config.Config{
		Storage: &config.StorageConfig{InMemory: true},
		Endpoints: []config.EndpointConfig{{
			Path: "/api/contact", To: []string{"to@example.com"},
			From: "from@example.com", Subject: "S", Body: "B",
			Transport: config.TransportConfig{
				Type: "postmark", Settings: map[string]any{"api_key": "k"},
			},
		}},
	}
	store, err := storage.Open(storage.Config{InMemory: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	gate := storage.NewGate(store, buildLogger(config.LoggingConfig{}))

	reg := metrics.New()
	rec := metrics.NewRecorder(reg)
	pool, _ := buildPool(cfg.Providers, rec)
	projects, _ := project.Build(nil)
	ready := newWorkerReadiness(cfg)
	ready.markStarted()
	ready.markStarted()
	mux, _, err := buildMux(cfg, pool, transport.QuotaSource(nil), projects,
		gateway.NewProjectUsage(nil), buildLogger(config.LoggingConfig{}), reg, rec, gate, ready)
	if err != nil {
		t.Fatal(err)
	}

	if code, body := probe(t, mux, "/readyz"); code != http.StatusOK {
		t.Fatalf("healthy storage = %d: %+v", code, body)
	}

	gate.ReportError(errFakeDisk{})
	code, body := probe(t, mux, "/readyz")
	if code != http.StatusServiceUnavailable {
		t.Errorf("degraded storage /readyz = %d, want 503: %+v", code, body)
	}
	if code, _ = probe(t, mux, "/healthz"); code != http.StatusOK {
		t.Errorf("/healthz = %d with degraded storage, want 200 (a restart does not fix a disk)", code)
	}
}

type errFakeDisk struct{}

func (errFakeDisk) Error() string { return "disk I/O error" }
