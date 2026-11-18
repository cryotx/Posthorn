package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The container healthcheck's whole job is to notice a process that is
// up but not serving. These tests pin that it actually does.

func TestProbe_ReadyIsSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ready","checks":{"storage":"ok"}}`))
	}))
	defer srv.Close()

	out, err := capture(t, func() error {
		return runProbe([]string{"--url", srv.URL + "/readyz"})
	})
	if err != nil {
		t.Fatalf("probe of a ready server failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "ready") {
		t.Errorf("output = %q", out)
	}
}

func TestProbe_NotReadyIsFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"status":"not ready","checks":{"storage":"degraded","workers":"ok"}}`))
	}))
	defer srv.Close()

	if _, err := capture(t, func() error {
		return runProbe([]string{"--url", srv.URL + "/readyz", "--quiet"})
	}); err == nil {
		t.Fatal("probe of a not-ready server succeeded")
	}
}

// A dead listener is the failure this replaced config-validation to
// catch: validating a file would have passed here.
func TestProbe_DeadListenerIsFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	url := srv.URL
	srv.Close()

	if _, err := capture(t, func() error {
		return runProbe([]string{"--url", url + "/readyz", "--quiet"})
	}); err == nil {
		t.Fatal("probe of a closed listener succeeded")
	}
}

// A 3xx from the readiness endpoint means something is in front of it
// that should not be — following it could report a foreign server ready.
func TestProbe_DoesNotFollowRedirects(t *testing.T) {
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ready"}`))
	}))
	defer elsewhere.Close()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL, http.StatusFound)
	}))
	defer srv.Close()

	if _, err := capture(t, func() error {
		return runProbe([]string{"--url", srv.URL + "/readyz", "--quiet"})
	}); err == nil {
		t.Fatal("probe followed a redirect and reported ready")
	}
}

// The failing check should be named, so `docker inspect` says WHICH
// dependency is not ready rather than only that something is not.
func TestProbe_NamesTheFailingCheck(t *testing.T) {
	body := `{"status":"not ready","checks":{"storage":"degraded — the write canary is failing","workers":"ok"}}`
	got := summarise([]byte(body))
	if !strings.Contains(got, "storage") || !strings.Contains(got, "degraded") {
		t.Errorf("summary = %q, want it to name the failing check", got)
	}
	if strings.Contains(got, "workers") {
		t.Errorf("summary = %q, want passing checks omitted", got)
	}
}
