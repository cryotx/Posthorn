// Modified for the cryotx/posthorn distribution.

package metrics

import (
	"encoding/json"
	"net/http"
)

// HealthzHandler returns an HTTP handler that responds with `200 OK` and
// body `ok` for any GET request. The endpoint is auth-free and
// rate-limit-free; operators concerned about exposure firewall the path
// at the reverse proxy (FR54).
func HealthzHandler() http.Handler {
	return HealthzHandlerWithStorage(nil)
}

// StorageState reports the storage layer's current health for /healthz
// (FR80). Wired by cmd/posthorn when a [storage] block is configured.
type StorageState func() (healthy bool)

// HealthzHandlerWithStorage extends /healthz with the storage canary
// state. With a nil state func the response is the v1.x plain-text "ok",
// byte-identical. With storage configured the body becomes JSON:
//
//	{"status":"ok","storage":"ok"}      — canary passing
//	{"status":"ok","storage":"degraded"} — canary failing
//
// The HTTP status stays 200 in both cases: a degraded disk does not
// stop mail flow (NFR27), so it must not fail liveness probes and cause
// restarts that wouldn't help.
func HealthzHandlerWithStorage(state StorageState) http.Handler {
	return HealthzHandlerWith(state, nil)
}

// ProviderState reports each pool provider's circuit-breaker state for
// /healthz (FR97), as name → "closed" / "half_open" / "open". Wired by
// cmd/posthorn when [[providers]] blocks are configured; nil otherwise.
type ProviderState func() map[string]string

// HealthzHandlerWith is HealthzHandlerWithStorage plus the provider
// circuit report. With both funcs nil the response stays the v1.x
// plain-text "ok"; with either configured the body is JSON:
//
//	{"status":"ok","storage":"ok",
//	 "providers":{"postmark-prod":"closed","ses-backup":"open"}}
//
// The HTTP status stays 200 even with every circuit open. /healthz is a
// liveness probe, and restarting the process does not fix an upstream
// outage — it just drops the in-flight mail that the retry queue would
// otherwise have held. Alert on posthorn_provider_circuit_state, not
// on this endpoint's status code.
func HealthzHandlerWith(state StorageState, providers ProviderState) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if state == nil && providers == nil {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ok"))
			return
		}
		body := map[string]any{"status": "ok"}
		if state != nil {
			storage := "ok"
			if !state() {
				storage = "degraded"
			}
			body["storage"] = storage
		}
		if providers != nil {
			body["providers"] = providers()
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(body)
	})
}

// Handler returns an HTTP handler that serves the registry's metrics in
// Prometheus text exposition format (FR55). Like /healthz, auth-free
// and rate-limit-free; firewall at the reverse proxy if needed.
//
// The Content-Type is the Prometheus exposition 0.0.4 format, recognized
// by Prometheus servers and promtool.
func (r *Registry) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_ = r.Emit(w)
	})
}

// --- readiness (FR149) --------------------------------------------------

// Readiness reports whether one startup dependency is ready. Each check
// answers for exactly one thing so the response can say WHICH one is
// not ready — "not ready" without a reason is an alert nobody can act
// on at 3am.
type Readiness struct {
	// Name is the dependency, e.g. "storage", "router", "workers".
	Name string

	// Ready reports the current state. Called on every probe: a check
	// that cached its answer at startup would keep reporting ready
	// after the thing it describes stopped being so.
	Ready func() (ok bool, detail string)
}

// ReadyzHandler serves /readyz: is this process able to SERVE, as
// opposed to /healthz's is it alive (FR149).
//
// The distinction is not academic — it decides what a supervisor does.
// /healthz failing means restart me. /readyz failing means stop sending
// me traffic, but restarting will not help. So the two endpoints must
// never be wired to the same signals.
//
// What makes Posthorn ready: storage is open and writable, the router
// is built, and the background workers have started. What deliberately
// does NOT: a provider being down. An upstream outage is exactly the
// case the retry queue, the circuit breaker and failover exist to
// absorb — reporting unready would take Posthorn out of rotation, so
// mail that would have been queued and delivered on recovery is instead
// refused at the door. Alert on posthorn_provider_circuit_state.
func ReadyzHandler(checks ...Readiness) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		body := map[string]any{"status": "ready"}
		details := make(map[string]string, len(checks))
		ready := true
		for _, c := range checks {
			ok, detail := true, "ok"
			if c.Ready != nil {
				ok, detail = c.Ready()
			}
			if !ok {
				ready = false
			}
			details[c.Name] = detail
		}
		body["checks"] = details
		status := http.StatusOK
		if !ready {
			body["status"] = "not ready"
			// 503, and Retry-After so a probe backs off rather than
			// hammering a process that is still opening its database.
			status = http.StatusServiceUnavailable
			w.Header().Set("Retry-After", "5")
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(body)
	})
}
