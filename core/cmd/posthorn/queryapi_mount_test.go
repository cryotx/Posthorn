package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/craigmccaskill/posthorn/config"
	"github.com/craigmccaskill/posthorn/gateway"
	"github.com/craigmccaskill/posthorn/metrics"
	"github.com/craigmccaskill/posthorn/project"
	"github.com/craigmccaskill/posthorn/storage"
	"github.com/craigmccaskill/posthorn/transport"
)

// The query API is only useful mounted, and mounting is where a route
// can quietly go missing or, worse, be served without a scope. These
// tests exercise it through the real mux.

const mountAcmeKey = "ph_live_acme_0123456789abcdef0123456789"

func queryMux(t *testing.T, withProjects, withStorage bool) *http.ServeMux {
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
	var projects *project.Registry
	var err error
	if withProjects {
		projects, err = project.Build([]project.Config{{
			Name: "acme", Enabled: true, APIKey: mountAcmeKey,
			AllowedFromDomains: []string{"example.com"},
		}})
	} else {
		projects, err = project.Build(nil)
	}
	if err != nil {
		t.Fatalf("project.Build: %v", err)
	}

	var gate *storage.Gate
	if withStorage {
		store, serr := storage.Open(storage.Config{InMemory: true})
		if serr != nil {
			t.Fatalf("storage.Open: %v", serr)
		}
		t.Cleanup(func() { _ = store.Close() })
		gate = storage.NewGate(store, buildLogger(config.LoggingConfig{}))
		if err := store.RecordSubmission(storage.Submission{
			ID: "a-1", Project: "acme", Endpoint: "/api/contact",
			Transport: "postmark", From: "noreply@example.com",
			ToAddrs: []string{"user@example.com"}, Subject: "Hello",
			BodyText: "SECRET BODY", Status: storage.StatusSent,
			CreatedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	reg := metrics.New()
	rec := metrics.NewRecorder(reg)
	pool, err := buildPool(cfg.Providers, rec)
	if err != nil {
		t.Fatalf("buildPool: %v", err)
	}
	mux, _, err := buildMux(cfg, pool, transport.QuotaSource(nil), projects,
		gateway.NewProjectUsage(nil), buildLogger(config.LoggingConfig{}), reg, rec, gate, newWorkerReadiness(cfg))
	if err != nil {
		t.Fatalf("buildMux: %v", err)
	}
	return mux
}

func TestQueryAPI_MountedWithProjectsAndStorage(t *testing.T) {
	mux := queryMux(t, true, true)

	r := httptest.NewRequest(http.MethodGet, "/v1/messages", nil)
	r.Header.Set("Authorization", "Bearer "+mountAcmeKey)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, r)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"a-1"`) {
		t.Errorf("body = %s, want the seeded message", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "SECRET BODY") {
		t.Errorf("message content served through the mux: %s", rec.Body.String())
	}
}

// Without a project there is no scope to answer in, and without storage
// nothing to answer from. In both cases the route must be ABSENT rather
// than served unscoped.
func TestQueryAPI_NotMountedWithoutScopeOrStorage(t *testing.T) {
	for _, tc := range []struct {
		name              string
		projects, storage bool
	}{
		{"no_projects", false, true},
		{"no_storage", true, false},
		{"neither", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mux := queryMux(t, tc.projects, tc.storage)
			r := httptest.NewRequest(http.MethodGet, "/v1/messages", nil)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, r)
			if rec.Code != http.StatusNotFound {
				t.Errorf("status = %d, want 404 (route must not exist)", rec.Code)
			}
		})
	}
}

func TestQueryAPI_MountedRoutesRequireAuth(t *testing.T) {
	mux := queryMux(t, true, true)
	for _, path := range []string{
		"/v1/messages", "/v1/messages/a-1", "/v1/messages/a-1/attempts",
		"/v1/messages/a-1/events", "/v1/usage",
	} {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, r)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s without a key = %d, want 401", path, rec.Code)
		}
	}
}
