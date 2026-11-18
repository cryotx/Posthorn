package transport

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// --- helpers -----------------------------------------------------------

// testProvider wraps a mockTransport in a pool entry with a breaker that
// is effectively off unless a test asks for one, so router tests
// exercise routing rather than health.
func testProvider(name string, mt *mockTransport, opts ...func(*Provider)) *Provider {
	p := &Provider{
		Name:      name,
		Type:      "mock",
		Weight:    1,
		Transport: mt,
		Breaker:   NewBreaker(BreakerConfig{FailureThreshold: 1 << 30, Cooldown: time.Hour}),
	}
	for _, o := range opts {
		o(p)
	}
	return p
}

func withWeight(w int) func(*Provider) {
	return func(p *Provider) { p.Weight = w }
}

func withBreaker(b *Breaker) func(*Provider) {
	return func(p *Provider) { p.Breaker = b }
}

func failing(err error) *mockTransport { return &mockTransport{SendErr: err} }

func succeeding(id string) *mockTransport {
	return &mockTransport{SendResult: SendResult{MessageID: id}}
}

func mustRoute(t *testing.T, match *RuleMatch, s Strategy, provs ...*Provider) *Route {
	t.Helper()
	r, err := NewRoute(match, s, provs)
	if err != nil {
		t.Fatalf("NewRoute: %v", err)
	}
	return r
}

func mustRouter(t *testing.T, name string, def *Route, rules ...*Route) *Router {
	t.Helper()
	r, err := NewRouter(name, def, rules)
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	return r
}

func simpleMsg() Message {
	return Message{From: "noreply@example.com", To: []string{"ops@example.com"}, Subject: "s", BodyText: "b"}
}

// --- strategy parsing --------------------------------------------------

func TestParseStrategy(t *testing.T) {
	for _, tc := range []struct {
		in      string
		want    Strategy
		wantErr bool
	}{
		{"", StrategyFailover, false},
		{"failover", StrategyFailover, false},
		{"round_robin", StrategyRoundRobin, false},
		{"weighted", StrategyWeighted, false},
		{"roundrobin", "", true},
		{"random", "", true},
	} {
		got, err := ParseStrategy(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("ParseStrategy(%q) = %v, want error", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseStrategy(%q): %v", tc.in, err)
		}
		if got != tc.want {
			t.Errorf("ParseStrategy(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// --- failover ----------------------------------------------------------

func TestRouter_FirstProviderWinsWhenHealthy(t *testing.T) {
	primary, backup := succeeding("pm-1"), succeeding("ses-1")
	r := mustRouter(t, "/api/contact", mustRoute(t, nil, StrategyFailover,
		testProvider("postmark", primary), testProvider("ses", backup)))

	res, err := r.Send(context.Background(), simpleMsg())
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if res.MessageID != "pm-1" {
		t.Errorf("MessageID = %q, want pm-1", res.MessageID)
	}
	if res.Provider != "postmark" {
		t.Errorf("Provider = %q, want postmark", res.Provider)
	}
	if len(backup.Sent) != 0 {
		t.Errorf("backup received %d messages, want 0", len(backup.Sent))
	}
}

func TestRouter_FailsOverOnTransient(t *testing.T) {
	primary := failing(&TransportError{Class: ErrTransient, Status: 503, Message: "gateway down"})
	backup := succeeding("ses-1")
	r := mustRouter(t, "/api/contact", mustRoute(t, nil, StrategyFailover,
		testProvider("postmark", primary), testProvider("ses", backup)))

	res, err := r.Send(context.Background(), simpleMsg())
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if res.Provider != "ses" {
		t.Errorf("Provider = %q, want ses", res.Provider)
	}
	if len(primary.Sent) != 1 || len(backup.Sent) != 1 {
		t.Errorf("attempts: primary=%d backup=%d, want 1 and 1", len(primary.Sent), len(backup.Sent))
	}
}

func TestRouter_FailsOverOnRateLimited(t *testing.T) {
	primary := failing(&TransportError{Class: ErrRateLimited, Status: 429})
	backup := succeeding("ses-1")
	r := mustRouter(t, "/x", mustRoute(t, nil, StrategyFailover,
		testProvider("postmark", primary), testProvider("ses", backup)))

	res, err := r.Send(context.Background(), simpleMsg())
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if res.Provider != "ses" {
		t.Errorf("Provider = %q, want ses — a 429 is a this-provider-right-now signal", res.Provider)
	}
}

// A terminal error means the provider rejected THIS MESSAGE. Shopping it
// around the pool turns one rejection into N rejections against N sender
// reputations, so the walk stops.
func TestRouter_TerminalErrorDoesNotFailOver(t *testing.T) {
	primary := failing(&TransportError{Class: ErrTerminal, Status: 422, Message: "from domain not verified"})
	backup := succeeding("ses-1")
	r := mustRouter(t, "/x", mustRoute(t, nil, StrategyFailover,
		testProvider("postmark", primary), testProvider("ses", backup)))

	_, err := r.Send(context.Background(), simpleMsg())
	if err == nil {
		t.Fatal("Send = nil error, want the terminal error surfaced")
	}
	if len(backup.Sent) != 0 {
		t.Errorf("backup received %d messages, want 0", len(backup.Sent))
	}
	var te *TransportError
	if !errors.As(err, &te) {
		t.Fatalf("error is %T, want *TransportError", err)
	}
	if te.Class != ErrTerminal {
		t.Errorf("Class = %v, want terminal", te.Class)
	}
	if te.Provider != "postmark" {
		t.Errorf("Provider = %q, want postmark", te.Provider)
	}
}

func TestRouter_ExhaustionReturnsLastError(t *testing.T) {
	a := failing(&TransportError{Class: ErrTransient, Message: "a down"})
	b := failing(&TransportError{Class: ErrTransient, Message: "b down"})
	r := mustRouter(t, "/x", mustRoute(t, nil, StrategyFailover,
		testProvider("a", a), testProvider("b", b)))

	_, err := r.Send(context.Background(), simpleMsg())
	if err == nil {
		t.Fatal("Send = nil, want error")
	}
	if !strings.Contains(err.Error(), "b down") {
		t.Errorf("error = %v, want the last provider's failure", err)
	}
	var te *TransportError
	if errors.As(err, &te) && te.Provider != "b" {
		t.Errorf("Provider = %q, want b", te.Provider)
	}
	// Exhaustion must stay retryable so the v2 queue holds the message
	// instead of dead-lettering it (ADR-21).
	if !IsRetryable(err) {
		t.Error("IsRetryable = false on transient exhaustion, want true")
	}
}

// The router only annotates a copy: the transport's own error value is
// shared across calls in some implementations and must not be mutated.
func TestRouter_DoesNotMutateTransportError(t *testing.T) {
	original := &TransportError{Class: ErrTerminal, Message: "nope"}
	r := mustRouter(t, "/x", mustRoute(t, nil, StrategyFailover,
		testProvider("a", failing(original))))

	if _, err := r.Send(context.Background(), simpleMsg()); err == nil {
		t.Fatal("want error")
	}
	if original.Provider != "" {
		t.Errorf("original.Provider = %q, want empty — the router mutated the caller's error", original.Provider)
	}
}

// --- circuit interaction -----------------------------------------------

func TestRouter_SkipsOpenCircuit(t *testing.T) {
	openBreaker := NewBreaker(BreakerConfig{FailureThreshold: 1, Cooldown: time.Hour})
	openBreaker.Failure(&TransportError{Class: ErrTransient})

	primary, backup := succeeding("pm-1"), succeeding("ses-1")
	var skipped []string
	r := mustRouter(t, "/x", mustRoute(t, nil, StrategyFailover,
		testProvider("postmark", primary, withBreaker(openBreaker)),
		testProvider("ses", backup)))
	r.Hooks.OnSkipped = func(p string) { skipped = append(skipped, p) }

	res, err := r.Send(context.Background(), simpleMsg())
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if res.Provider != "ses" {
		t.Errorf("Provider = %q, want ses", res.Provider)
	}
	if len(primary.Sent) != 0 {
		t.Error("open-circuit provider was dialled")
	}
	if len(skipped) != 1 || skipped[0] != "postmark" {
		t.Errorf("skipped = %v, want [postmark]", skipped)
	}
}

func TestRouter_AllCircuitsOpenIsTransient(t *testing.T) {
	mk := func() *Breaker {
		b := NewBreaker(BreakerConfig{FailureThreshold: 1, Cooldown: time.Hour})
		b.Failure(&TransportError{Class: ErrTransient})
		return b
	}
	a, b := succeeding("a"), succeeding("b")
	var exhausted []string
	r := mustRouter(t, "/api/alerts", mustRoute(t, nil, StrategyFailover,
		testProvider("a", a, withBreaker(mk())),
		testProvider("b", b, withBreaker(mk()))))
	r.Hooks.OnExhausted = func(n string) { exhausted = append(exhausted, n) }

	_, err := r.Send(context.Background(), simpleMsg())
	if err == nil {
		t.Fatal("Send = nil, want error")
	}
	if !IsRetryable(err) {
		t.Error("IsRetryable = false, want true — the pool is down, the message is fine")
	}
	if !strings.Contains(err.Error(), "circuit(s) open") {
		t.Errorf("error = %v, want it to name the open circuits", err)
	}
	if len(exhausted) != 1 || exhausted[0] != "/api/alerts" {
		t.Errorf("OnExhausted = %v, want [/api/alerts]", exhausted)
	}
	if len(a.Sent)+len(b.Sent) != 0 {
		t.Error("a provider was dialled with its circuit open")
	}
}

func TestRouter_SuccessClosesCircuitAfterProbe(t *testing.T) {
	clock := newClock()
	br := NewBreaker(BreakerConfig{FailureThreshold: 1, Cooldown: time.Second})
	br.now = clock.Now
	br.Failure(&TransportError{Class: ErrTransient})

	mt := succeeding("pm-1")
	r := mustRouter(t, "/x", mustRoute(t, nil, StrategyFailover,
		testProvider("postmark", mt, withBreaker(br))))

	clock.Advance(2 * time.Second)
	if _, err := r.Send(context.Background(), simpleMsg()); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if got := br.State(); got != CircuitClosed {
		t.Errorf("State() = %v, want closed after the probe delivered", got)
	}
}

// --- strategies --------------------------------------------------------

func TestRouter_RoundRobinSpreadsEvenly(t *testing.T) {
	a, b, c := succeeding("a"), succeeding("b"), succeeding("c")
	r := mustRouter(t, "/x", mustRoute(t, nil, StrategyRoundRobin,
		testProvider("a", a), testProvider("b", b), testProvider("c", c)))

	for i := 0; i < 30; i++ {
		if _, err := r.Send(context.Background(), simpleMsg()); err != nil {
			t.Fatalf("Send %d: %v", i, err)
		}
	}
	for name, mt := range map[string]*mockTransport{"a": a, "b": b, "c": c} {
		if len(mt.Sent) != 10 {
			t.Errorf("provider %s took %d of 30, want 10", name, len(mt.Sent))
		}
	}
}

// Smooth weighted round-robin is exact over a full cycle, not merely
// exact in expectation — which is why it was chosen over weighted-random.
func TestRouter_WeightedSplitsByWeight(t *testing.T) {
	a, b := succeeding("a"), succeeding("b")
	r := mustRouter(t, "/x", mustRoute(t, nil, StrategyWeighted,
		testProvider("a", a, withWeight(3)), testProvider("b", b, withWeight(1))))

	for i := 0; i < 40; i++ {
		if _, err := r.Send(context.Background(), simpleMsg()); err != nil {
			t.Fatalf("Send %d: %v", i, err)
		}
	}
	if len(a.Sent) != 30 || len(b.Sent) != 10 {
		t.Errorf("split = a:%d b:%d, want a:30 b:10", len(a.Sent), len(b.Sent))
	}
}

func TestRouter_WeightedStillFailsOver(t *testing.T) {
	a := failing(&TransportError{Class: ErrTransient, Message: "a down"})
	b := succeeding("b-1")
	r := mustRouter(t, "/x", mustRoute(t, nil, StrategyWeighted,
		testProvider("a", a, withWeight(9)), testProvider("b", b, withWeight(1))))

	for i := 0; i < 5; i++ {
		res, err := r.Send(context.Background(), simpleMsg())
		if err != nil {
			t.Fatalf("Send %d: %v", i, err)
		}
		if res.Provider != "b" {
			t.Fatalf("Send %d landed on %q, want b", i, res.Provider)
		}
	}
}

func TestRouter_FailoverIgnoresWeightForFirstPick(t *testing.T) {
	a, b := succeeding("a"), succeeding("b")
	r := mustRouter(t, "/x", mustRoute(t, nil, StrategyFailover,
		testProvider("a", a, withWeight(1)), testProvider("b", b, withWeight(99))))

	for i := 0; i < 10; i++ {
		if _, err := r.Send(context.Background(), simpleMsg()); err != nil {
			t.Fatal(err)
		}
	}
	if len(a.Sent) != 10 || len(b.Sent) != 0 {
		t.Errorf("split = a:%d b:%d, want all on a — failover is ordered, not weighted", len(a.Sent), len(b.Sent))
	}
}

// --- hooks -------------------------------------------------------------

func TestRouter_HooksReportAttemptsAndFailover(t *testing.T) {
	type attempt struct{ provider, outcome string }
	var attempts []attempt
	var failovers []string

	primary := failing(&TransportError{Class: ErrTransient})
	r := mustRouter(t, "/api/contact", mustRoute(t, nil, StrategyFailover,
		testProvider("postmark", primary), testProvider("ses", succeeding("ses-1"))))
	r.Hooks.OnAttempt = func(p, o string, _ time.Duration) {
		attempts = append(attempts, attempt{p, o})
	}
	r.Hooks.OnFailover = func(n string) { failovers = append(failovers, n) }

	if _, err := r.Send(context.Background(), simpleMsg()); err != nil {
		t.Fatalf("Send: %v", err)
	}

	want := []attempt{{"postmark", "transient"}, {"ses", "sent"}}
	if len(attempts) != len(want) {
		t.Fatalf("attempts = %v, want %v", attempts, want)
	}
	for i := range want {
		if attempts[i] != want[i] {
			t.Errorf("attempt[%d] = %v, want %v", i, attempts[i], want[i])
		}
	}
	if len(failovers) != 1 || failovers[0] != "/api/contact" {
		t.Errorf("failovers = %v, want [/api/contact]", failovers)
	}
}

func TestRouter_NoFailoverHookOnFirstPickSuccess(t *testing.T) {
	fired := false
	r := mustRouter(t, "/x", mustRoute(t, nil, StrategyFailover,
		testProvider("a", succeeding("a")), testProvider("b", succeeding("b"))))
	r.Hooks.OnFailover = func(string) { fired = true }

	if _, err := r.Send(context.Background(), simpleMsg()); err != nil {
		t.Fatal(err)
	}
	if fired {
		t.Error("OnFailover fired when the first pick delivered")
	}
}

func TestRouter_NilHooksAreSafe(t *testing.T) {
	r := mustRouter(t, "/x", mustRoute(t, nil, StrategyFailover,
		testProvider("a", failing(&TransportError{Class: ErrTransient})),
		testProvider("b", succeeding("b"))))
	if _, err := r.Send(context.Background(), simpleMsg()); err != nil {
		t.Fatalf("Send with nil hooks: %v", err)
	}
}

// --- context -----------------------------------------------------------

// FR22's hard timeout bounds the whole send. A router that kept walking
// a ten-provider pool after the deadline would blow past it tenfold.
func TestRouter_StopsOnCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	a := &mockTransport{SendFn: func(context.Context, Message) (SendResult, error) {
		cancel()
		return SendResult{}, &TransportError{Class: ErrTransient, Message: "a down"}
	}}
	b := succeeding("b")
	r := mustRouter(t, "/x", mustRoute(t, nil, StrategyFailover,
		testProvider("a", a), testProvider("b", b)))

	if _, err := r.Send(ctx, simpleMsg()); err == nil {
		t.Fatal("Send = nil, want the transient error")
	}
	if len(b.Sent) != 0 {
		t.Error("router kept walking the pool after the context was canceled")
	}
}

// --- composition with the FR19-22 retry policy -------------------------

// The router is a Transport, so SendWithRetry wraps it unchanged: a pool
// that exhausts on transient errors gets one more full pass after the
// backoff. This is the property that let the retry policy stay
// untouched by the multi-provider work.
func TestRouter_ComposesWithSendWithRetry(t *testing.T) {
	restore := TransientRetryDelay
	TransientRetryDelay = time.Millisecond
	defer func() { TransientRetryDelay = restore }()

	var mu sync.Mutex
	calls := 0
	flaky := &mockTransport{SendFn: func(context.Context, Message) (SendResult, error) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		if calls <= 2 { // both providers fail on the first pass
			return SendResult{}, &TransportError{Class: ErrTransient, Message: "flaky"}
		}
		return SendResult{MessageID: "ok"}, nil
	}}
	r := mustRouter(t, "/x", mustRoute(t, nil, StrategyFailover,
		testProvider("a", flaky), testProvider("b", flaky)))

	res, err := SendWithRetry(context.Background(), r, simpleMsg(), discardLogger())
	if err != nil {
		t.Fatalf("SendWithRetry: %v", err)
	}
	if res.MessageID != "ok" {
		t.Errorf("MessageID = %q, want ok", res.MessageID)
	}
	if res.Provider != "a" {
		t.Errorf("Provider = %q, want a (the retry pass restarts at the first pick)", res.Provider)
	}
}

// --- construction ------------------------------------------------------

func TestNewRoute_RejectsEmptyProviders(t *testing.T) {
	if _, err := NewRoute(nil, StrategyFailover, nil); err == nil {
		t.Error("NewRoute with no providers = nil error, want one")
	}
}

func TestNewRoute_RejectsUnknownStrategy(t *testing.T) {
	if _, err := NewRoute(nil, "sideways", []*Provider{testProvider("a", succeeding("a"))}); err == nil {
		t.Error("NewRoute with an unknown strategy = nil error, want one")
	}
}

func TestNewRouter_RequiresDefaultRoute(t *testing.T) {
	if _, err := NewRouter("/x", nil, nil); err == nil {
		t.Error("NewRouter with no default route = nil error, want one")
	}
}

func TestRouter_ProvidersDeduplicatesAcrossRoutes(t *testing.T) {
	shared := testProvider("shared", succeeding("s"))
	only := testProvider("only", succeeding("o"))
	r := mustRouter(t, "/x",
		mustRoute(t, nil, StrategyFailover, shared),
		mustRoute(t, &RuleMatch{RecipientDomains: []string{"example.org"}}, StrategyFailover, shared, only))

	got := r.Providers()
	if len(got) != 2 || got[0].Name != "shared" || got[1].Name != "only" {
		names := make([]string, len(got))
		for i, p := range got {
			names[i] = p.Name
		}
		t.Errorf("Providers() = %v, want [shared only]", names)
	}
}

// A single-provider pool must behave exactly like the v1.x inline
// transport it replaces, minus the health bookkeeping.
func TestRouter_SingleProviderIsPassThrough(t *testing.T) {
	mt := succeeding("pm-1")
	r := mustRouter(t, "/x", mustRoute(t, nil, StrategyRoundRobin, testProvider("only", mt)))

	for i := 0; i < 5; i++ {
		res, err := r.Send(context.Background(), simpleMsg())
		if err != nil {
			t.Fatalf("Send %d: %v", i, err)
		}
		if res.MessageID != "pm-1" || res.Provider != "only" {
			t.Fatalf("res = %+v", res)
		}
	}
	if len(mt.Sent) != 5 {
		t.Errorf("Sent = %d, want 5", len(mt.Sent))
	}
}

// --- admin disable (FR144) ---------------------------------------------

// TestRouter_SkipsAdminDisabledProvider: disabling one provider in a
// failover pool must route around it, not fail the send. That is what
// makes the kill switch usable during an incident.
func TestRouter_SkipsAdminDisabledProvider(t *testing.T) {
	primary := succeeding("primary-1")
	secondary := succeeding("secondary-1")
	p1 := testProvider("primary", primary)
	p2 := testProvider("secondary", secondary)
	r := mustRouter(t, "test", mustRoute(t, nil, StrategyFailover, p1, p2))

	p1.SetAdminDisabled(true)
	res, err := r.Send(context.Background(), simpleMsg())
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if len(primary.Sent) != 0 {
		t.Errorf("disabled provider was used %d times", len(primary.Sent))
	}
	if len(secondary.Sent) != 1 || res.Provider != "secondary" {
		t.Errorf("secondary sends = %d, provider = %q", len(secondary.Sent), res.Provider)
	}

	// Re-enabling restores it — the flag only ever subtracts.
	p1.SetAdminDisabled(false)
	res, err = r.Send(context.Background(), simpleMsg())
	if err != nil {
		t.Fatalf("Send after re-enable: %v", err)
	}
	if len(primary.Sent) != 1 || res.Provider != "primary" {
		t.Errorf("primary sends after re-enable = %d, provider = %q", len(primary.Sent), res.Provider)
	}
}

// With every provider disabled the error must say so: the fix is to
// re-enable, not to wait out a cooldown.
func TestRouter_AllProvidersDisabled(t *testing.T) {
	p := testProvider("only", succeeding("x"))
	p.SetAdminDisabled(true)
	r := mustRouter(t, "test", mustRoute(t, nil, StrategyFailover, p))

	_, err := r.Send(context.Background(), simpleMsg())
	if err == nil {
		t.Fatal("send succeeded with every provider disabled")
	}
	if !strings.Contains(err.Error(), "disabled by an operator") {
		t.Errorf("error = %v, want it to name the operator disable", err)
	}
	var te *TransportError
	if errors.As(err, &te) && te.Class != ErrTransient {
		t.Errorf("Class = %v, want ErrTransient (re-enabling fixes it)", te.Class)
	}
}
