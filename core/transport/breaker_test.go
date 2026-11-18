package transport

import (
	"sync"
	"testing"
	"time"
)

// fakeClock drives a breaker's cooldown deterministically. Real sleeps
// in a breaker test would make the suite slow and flaky for no gain —
// the thing under test is the state machine, not the wall clock.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// newTestBreaker builds a breaker on a fake clock.
func newTestBreaker(t *testing.T, cfg BreakerConfig) (*Breaker, *fakeClock) {
	t.Helper()
	b := NewBreaker(cfg)
	c := newClock()
	b.now = c.Now
	return b, c
}

func transientErr() error {
	return &TransportError{Class: ErrTransient, Message: "upstream 503"}
}

func terminalErr() error {
	return &TransportError{Class: ErrTerminal, Status: 422, Message: "unverified from domain"}
}

func TestBreaker_DefaultsApplied(t *testing.T) {
	b := NewBreaker(BreakerConfig{})
	if b.cfg.FailureThreshold != DefaultBreakerFailureThreshold {
		t.Errorf("FailureThreshold = %d, want %d", b.cfg.FailureThreshold, DefaultBreakerFailureThreshold)
	}
	if b.cfg.Cooldown != DefaultBreakerCooldown {
		t.Errorf("Cooldown = %v, want %v", b.cfg.Cooldown, DefaultBreakerCooldown)
	}
	if b.cfg.HalfOpenProbes != DefaultBreakerHalfOpenProbes {
		t.Errorf("HalfOpenProbes = %d, want %d", b.cfg.HalfOpenProbes, DefaultBreakerHalfOpenProbes)
	}
}

func TestBreaker_OpensAtThreshold(t *testing.T) {
	b, _ := newTestBreaker(t, BreakerConfig{FailureThreshold: 3, Cooldown: time.Minute})

	for i := 0; i < 2; i++ {
		b.Failure(transientErr())
		if !b.Allow() {
			t.Fatalf("Allow() = false after %d failures, want true (threshold 3)", i+1)
		}
	}
	b.Failure(transientErr())
	if b.Allow() {
		t.Error("Allow() = true after 3 failures, want false (circuit should be open)")
	}
	if got := b.State(); got != CircuitOpen {
		t.Errorf("State() = %v, want open", got)
	}
}

// A terminal error describes the message, not the provider. If it
// counted, one endpoint with a bad From domain would take every
// provider in the pool out of rotation, one submission at a time.
func TestBreaker_TerminalErrorsNeverTrip(t *testing.T) {
	b, _ := newTestBreaker(t, BreakerConfig{FailureThreshold: 2, Cooldown: time.Minute})

	for i := 0; i < 20; i++ {
		b.Failure(terminalErr())
	}
	if !b.Allow() {
		t.Error("Allow() = false after 20 terminal errors, want true")
	}
	if got := b.State(); got != CircuitClosed {
		t.Errorf("State() = %v, want closed", got)
	}
}

func TestBreaker_RateLimitedCountsAsHealthClass(t *testing.T) {
	b, _ := newTestBreaker(t, BreakerConfig{FailureThreshold: 2, Cooldown: time.Minute})
	b.Failure(&TransportError{Class: ErrRateLimited, Status: 429})
	b.Failure(&TransportError{Class: ErrRateLimited, Status: 429})
	if b.Allow() {
		t.Error("Allow() = true, want false — 429s are a provider-health signal")
	}
}

func TestBreaker_SuccessResetsConsecutiveCount(t *testing.T) {
	b, _ := newTestBreaker(t, BreakerConfig{FailureThreshold: 3, Cooldown: time.Minute})
	b.Failure(transientErr())
	b.Failure(transientErr())
	b.Success()
	b.Failure(transientErr())
	b.Failure(transientErr())
	if !b.Allow() {
		t.Error("Allow() = false, want true — Success should have reset the run")
	}
}

func TestBreaker_CooldownAdmitsOneProbe(t *testing.T) {
	b, clock := newTestBreaker(t, BreakerConfig{FailureThreshold: 1, Cooldown: 30 * time.Second})
	b.Failure(transientErr())

	clock.Advance(29 * time.Second)
	if b.Allow() {
		t.Fatal("Allow() = true before cooldown expiry")
	}

	clock.Advance(2 * time.Second)
	if !b.Allow() {
		t.Fatal("Allow() = false after cooldown expiry, want a probe")
	}
	// Only HalfOpenProbes (1) may be in flight.
	if b.Allow() {
		t.Error("Allow() = true for a second concurrent probe, want false")
	}
}

func TestBreaker_ProbeSuccessCloses(t *testing.T) {
	b, clock := newTestBreaker(t, BreakerConfig{FailureThreshold: 1, Cooldown: time.Second})
	b.Failure(transientErr())
	clock.Advance(2 * time.Second)

	if !b.Allow() {
		t.Fatal("no probe admitted")
	}
	b.Success()

	if got := b.State(); got != CircuitClosed {
		t.Errorf("State() = %v, want closed after probe success", got)
	}
	if !b.Allow() || !b.Allow() {
		t.Error("closed circuit should admit unlimited sends")
	}
}

func TestBreaker_ProbeFailureReopensForFullCooldown(t *testing.T) {
	b, clock := newTestBreaker(t, BreakerConfig{FailureThreshold: 1, Cooldown: 10 * time.Second})
	b.Failure(transientErr())
	clock.Advance(11 * time.Second)

	if !b.Allow() {
		t.Fatal("no probe admitted")
	}
	b.Failure(transientErr())

	if got := b.State(); got != CircuitOpen {
		t.Fatalf("State() = %v, want open after probe failure", got)
	}
	clock.Advance(9 * time.Second)
	if b.Allow() {
		t.Error("Allow() = true 9s into a fresh 10s cooldown")
	}
	clock.Advance(2 * time.Second)
	if !b.Allow() {
		t.Error("Allow() = false after the fresh cooldown expired")
	}
}

func TestBreaker_DisabledNeverSkips(t *testing.T) {
	b, _ := newTestBreaker(t, BreakerConfig{FailureThreshold: 1, Disabled: true})
	for i := 0; i < 10; i++ {
		b.Failure(transientErr())
	}
	if !b.Allow() {
		t.Error("Allow() = false on a disabled breaker")
	}
	if got := b.State(); got != CircuitClosed {
		t.Errorf("State() = %v, want closed on a disabled breaker", got)
	}
}

// State() is the gauge/healthz read path and must not consume a probe
// slot — otherwise a Prometheus scrape would starve the half-open
// recovery it is there to observe.
func TestBreaker_StateDoesNotConsumeProbe(t *testing.T) {
	b, clock := newTestBreaker(t, BreakerConfig{FailureThreshold: 1, Cooldown: time.Second})
	b.Failure(transientErr())
	clock.Advance(2 * time.Second)

	for i := 0; i < 5; i++ {
		if got := b.State(); got != CircuitHalfOpen {
			t.Fatalf("State() = %v, want half_open", got)
		}
	}
	if !b.Allow() {
		t.Error("Allow() = false — repeated State() reads consumed the probe slot")
	}
}

func TestBreaker_TransitionCallbackFires(t *testing.T) {
	b, clock := newTestBreaker(t, BreakerConfig{FailureThreshold: 1, Cooldown: time.Second})
	var seen []CircuitState
	b.OnTransition(func(s CircuitState) { seen = append(seen, s) })

	b.Failure(transientErr()) // → open
	clock.Advance(2 * time.Second)
	b.Allow()   // → half-open
	b.Success() // → closed

	want := []CircuitState{CircuitOpen, CircuitHalfOpen, CircuitClosed}
	if len(seen) != len(want) {
		t.Fatalf("transitions = %v, want %v", seen, want)
	}
	for i := range want {
		if seen[i] != want[i] {
			t.Errorf("transition[%d] = %v, want %v", i, seen[i], want[i])
		}
	}
}

// A pooled provider is shared by every endpoint that names it, so its
// breaker is hit concurrently by definition. -race guards the invariant.
func TestBreaker_ConcurrentUse(t *testing.T) {
	b := NewBreaker(BreakerConfig{FailureThreshold: 50, Cooldown: time.Millisecond})

	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				if b.Allow() {
					if (i+j)%3 == 0 {
						b.Failure(transientErr())
					} else {
						b.Success()
					}
				}
				_ = b.State()
			}
		}(i)
	}
	wg.Wait()
}

func TestCircuitState_String(t *testing.T) {
	for _, tc := range []struct {
		state CircuitState
		want  string
	}{
		{CircuitClosed, "closed"},
		{CircuitOpen, "open"},
		{CircuitHalfOpen, "half_open"},
		{CircuitState(99), "closed"},
	} {
		if got := tc.state.String(); got != tc.want {
			t.Errorf("CircuitState(%d).String() = %q, want %q", tc.state, got, tc.want)
		}
	}
}

// TestBreaker_Reset closes an open breaker on demand (FR144) — the
// operator who just fixed the credential should not have to wait out a
// cooldown for information they already have.
func TestBreaker_Reset(t *testing.T) {
	b := NewBreaker(BreakerConfig{FailureThreshold: 2, Cooldown: time.Hour})
	for i := 0; i < 2; i++ {
		b.Failure(&TransportError{Class: ErrTransient})
	}
	if b.State() != CircuitOpen {
		t.Fatalf("state = %v, want open", b.State())
	}

	b.Reset()
	if b.State() != CircuitClosed {
		t.Errorf("state after Reset = %v, want closed", b.State())
	}
	if !b.Allow() {
		t.Error("Allow() = false after Reset")
	}

	// A reset is not a promise: the next failures trip it again.
	for i := 0; i < 2; i++ {
		b.Failure(&TransportError{Class: ErrTransient})
	}
	if b.State() != CircuitOpen {
		t.Errorf("state = %v, want open again after fresh failures", b.State())
	}
}
