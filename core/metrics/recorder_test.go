package metrics

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

// emit renders a registry to text for substring assertions.
func emit(t *testing.T, reg *Registry) string {
	t.Helper()
	var buf bytes.Buffer
	if err := reg.Emit(&buf); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	return buf.String()
}

// --- v2.1 multi-provider routing surface (FR97) ---

func TestRecorder_ProviderAttemptCountsAndTimes(t *testing.T) {
	reg := New()
	r := NewRecorder(reg)

	r.ProviderAttempt("postmark-prod", "transient", 120*time.Millisecond)
	r.ProviderAttempt("ses-backup", "sent", 80*time.Millisecond)
	r.ProviderAttempt("ses-backup", "sent", 90*time.Millisecond)

	out := emit(t, reg)
	for _, want := range []string{
		`posthorn_provider_attempts_total{provider="postmark-prod",outcome="transient"} 1`,
		`posthorn_provider_attempts_total{provider="ses-backup",outcome="sent"} 2`,
		`posthorn_provider_latency_seconds_count{provider="ses-backup"} 2`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestRecorder_CircuitStateGauge(t *testing.T) {
	reg := New()
	r := NewRecorder(reg)

	r.SetCircuitState("a", "closed")
	r.SetCircuitState("b", "half_open")
	r.SetCircuitState("c", "open")

	out := emit(t, reg)
	for _, want := range []string{
		`posthorn_provider_circuit_state{provider="a"} 0`,
		`posthorn_provider_circuit_state{provider="b"} 1`,
		`posthorn_provider_circuit_state{provider="c"} 2`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

// An unrecognised state must read as closed rather than as a missing
// series: a gauge that silently disappears is worse than one that is
// merely optimistic.
func TestRecorder_CircuitStateUnknownReadsClosed(t *testing.T) {
	reg := New()
	r := NewRecorder(reg)
	r.SetCircuitState("a", "wat")
	if out := emit(t, reg); !strings.Contains(out, `posthorn_provider_circuit_state{provider="a"} 0`) {
		t.Errorf("unknown state did not fall back to 0:\n%s", out)
	}
}

func TestRecorder_RoutingCounters(t *testing.T) {
	reg := New()
	r := NewRecorder(reg)

	r.ProviderSkipped("postmark-prod")
	r.Failover("/api/contact")
	r.Failover("/api/contact")
	r.RouteExhausted("/api/alerts")

	out := emit(t, reg)
	for _, want := range []string{
		`posthorn_provider_skipped_total{provider="postmark-prod"} 1`,
		`posthorn_route_failover_total{router="/api/contact"} 2`,
		`posthorn_route_exhausted_total{router="/api/alerts"} 1`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

// NFR24 for the routing surface: the Recorder's routing methods accept
// only provider names, router names and the outcome enum. There is no
// parameter a request could flow into — this test pins that the API
// shape stays that way.
func TestRecorder_RoutingMethodsTakeNoRequestContent(t *testing.T) {
	reg := New()
	r := NewRecorder(reg)

	// Everything below is operator-configured or enum-shaped by
	// construction; a recipient address or subject has no parameter to
	// travel through.
	r.ProviderAttempt("ses-backup", "terminal", time.Millisecond)
	r.ProviderSkipped("ses-backup")
	r.SetCircuitState("ses-backup", "open")
	r.Failover("/api/contact")
	r.RouteExhausted("/api/contact")

	out := emit(t, reg)
	if strings.Contains(out, "@") {
		t.Errorf("an address-shaped value reached the label space:\n%s", out)
	}
}

func TestRecorder_NilRecorderRoutingMethodsAreSafe(t *testing.T) {
	var r *Recorder
	r.ProviderAttempt("a", "sent", time.Second)
	r.ProviderSkipped("a")
	r.SetCircuitState("a", "open")
	r.Failover("/x")
	r.RouteExhausted("/x")
}
