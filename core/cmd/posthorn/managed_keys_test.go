package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/craigmccaskill/posthorn/config"
	"github.com/craigmccaskill/posthorn/gateway"
	"github.com/craigmccaskill/posthorn/metrics"
	"github.com/craigmccaskill/posthorn/project"
	"github.com/craigmccaskill/posthorn/storage"
)

func TestManagedProjectCreatedAfterMuxStarts(t *testing.T) {
	store, err := storage.Open(storage.Config{InMemory: true})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	logger := buildLogger(config.LoggingConfig{})
	gate := storage.NewGate(store, logger)
	projects, err := project.Build(nil)
	if err != nil {
		t.Fatal(err)
	}
	manager, err := project.NewManager(projects, store, "sender@example.com")
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Endpoints: []config.EndpointConfig{{
		Path: "/api/send", To: []string{"fallback@example.com"}, From: "sender@example.com",
		Auth: config.AuthAPIKey, APIKeys: []string{"legacy-key-for-testing-only"}, DryRun: true,
		Subject: "{{.subject}}", Body: "{{.message}}",
		Transport: config.TransportConfig{Type: "postmark", Settings: map[string]any{"api_key": "test"}},
	}}}
	reg := metrics.New()
	recorder := metrics.NewRecorder(reg)
	pool, err := buildPool(cfg.Providers, recorder)
	if err != nil {
		t.Fatal(err)
	}
	mux, _, err := buildMux(cfg, pool, nil, projects, gateway.NewProjectUsage(gate), logger, reg, recorder, gate, newWorkerReadiness(cfg))
	if err != nil {
		t.Fatal(err)
	}
	// The registry is empty when the mux is constructed; the admin adds a project later.
	key, err := manager.Create("app-one")
	if err != nil {
		t.Fatal(err)
	}
	request := func(key string) int {
		r := httptest.NewRequest(http.MethodPost, "/api/send", strings.NewReader(`{"to_override":"recipient@example.com","subject":"test","message":"test"}`))
		r.Header.Set("Authorization", "Bearer "+key)
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		return w.Code
	}
	if status := request(key); status != http.StatusOK {
		t.Fatalf("new project key: %d", status)
	}
	if status := request("legacy-key-for-testing-only"); status != http.StatusOK {
		t.Fatalf("legacy key: %d", status)
	}
	p, _ := projects.ByName("app-one")
	p.SetAdminDisabled(true)
	if status := request(key); status != http.StatusUnauthorized {
		t.Fatalf("disabled project key: %d", status)
	}
}
