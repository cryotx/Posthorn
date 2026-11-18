package transport

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// costProvider is a pool entry with a price and an optional budget.
func costProvider(name string, cost Cost, daily int64, mt *mockTransport) *Provider {
	return &Provider{
		Name: name, Type: "mock", Weight: 1,
		Transport: mt,
		Breaker:   NewBreaker(BreakerConfig{FailureThreshold: 1 << 30, Cooldown: time.Hour}),
		Cost:      cost,
		Quota:     Quota{Daily: daily},
	}
}

// quotaRouter wires a router to a fresh local tracker on a fixed clock.
func quotaRouter(t *testing.T, name string, strategy Strategy, provs ...*Provider) (*Router, *LocalQuota, *time.Time) {
	t.Helper()
	clock := at(2026, 9, 7, 12)
	q := NewLocalQuota(nil, nil)
	r := mustRouter(t, name, mustRoute(t, nil, strategy, provs...))
	r.Quota = q
	r.Now = func() time.Time { return clock }
	return r, q, &clock
}

// --- quota gating ------------------------------------------------------

func TestRouter_SkipsQuotaExhaustedProvider(t *testing.T) {
	freeT, paidT := succeeding("free-1"), succeeding("paid-1")
	free := costProvider("free", 0, 2, freeT)
	paid := costProvider("paid", 100_000, 0, paidT)

	var blocked []string
	r, _, _ := quotaRouter(t, "/api/send", StrategyFailover, free, paid)
	r.Hooks.OnQuotaBlocked = func(p, window string) { blocked = append(blocked, p+":"+window) }

	// The free provider's two-message budget, then the overflow.
	for i := 1; i <= 3; i++ {
		res, err := r.Send(context.Background(), simpleMsg())
		if err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
		want := "free"
		if i == 3 {
			want = "paid"
		}
		if res.Provider != want {
			t.Errorf("send %d went to %q, want %q", i, res.Provider, want)
		}
	}
	if len(freeT.Sent) != 2 || len(paidT.Sent) != 1 {
		t.Errorf("split = free:%d paid:%d, want 2 and 1", len(freeT.Sent), len(paidT.Sent))
	}
	if len(blocked) != 1 || blocked[0] != "free:daily" {
		t.Errorf("OnQuotaBlocked = %v, want [free:daily]", blocked)
	}
}

// Only a delivered message spends allowance. A provider that fails must
// not burn budget the operator paid for.
func TestRouter_FailedSendDoesNotConsumeQuota(t *testing.T) {
	failing := &mockTransport{SendErr: &TransportError{Class: ErrTransient, Message: "down"}}
	primary := costProvider("primary", 0, 5, failing)
	backup := costProvider("backup", 0, 5, succeeding("b"))

	r, q, clock := quotaRouter(t, "/x", StrategyFailover, primary, backup)

	for i := 0; i < 3; i++ {
		if _, err := r.Send(context.Background(), simpleMsg()); err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
	}
	u, _ := q.Snapshot("primary", *clock)
	if u.UsedToday != 0 {
		t.Errorf("primary used %d of its quota on failed attempts, want 0", u.UsedToday)
	}
	u, _ = q.Snapshot("backup", *clock)
	if u.UsedToday != 3 {
		t.Errorf("backup UsedToday = %d, want 3", u.UsedToday)
	}
}

// Quota exhaustion is not a health signal. Letting it trip a breaker
// would take a perfectly healthy provider out of rotation for a
// cooldown it does not need, and would make the metrics lie about why.
func TestRouter_QuotaExhaustionDoesNotTripBreaker(t *testing.T) {
	breaker := NewBreaker(BreakerConfig{FailureThreshold: 2, Cooldown: time.Hour})
	free := &Provider{
		Name: "free", Transport: succeeding("f"), Weight: 1,
		Breaker: breaker, Quota: Quota{Daily: 1},
	}
	paid := costProvider("paid", 100_000, 0, succeeding("p"))

	r, _, _ := quotaRouter(t, "/x", StrategyFailover, free, paid)

	for i := 0; i < 10; i++ {
		if _, err := r.Send(context.Background(), simpleMsg()); err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
	}
	if got := breaker.State(); got != CircuitClosed {
		t.Errorf("breaker state = %v after 9 quota skips, want closed", got)
	}
	if !breaker.Allow() {
		t.Error("breaker refused a send after quota skips — quota was treated as a health failure")
	}
}

// --- exhaustion classification -----------------------------------------

func TestRouter_AllQuotaExhaustedIsItsOwnClass(t *testing.T) {
	a := costProvider("a", 0, 1, succeeding("a"))
	b := costProvider("b", 0, 1, succeeding("b"))

	var routeExhausted []string
	r, _, _ := quotaRouter(t, "/api/alerts", StrategyFailover, a, b)
	r.Hooks.OnRouteQuotaExhausted = func(n string) { routeExhausted = append(routeExhausted, n) }

	// Spend both budgets.
	for i := 0; i < 2; i++ {
		if _, err := r.Send(context.Background(), simpleMsg()); err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
	}

	_, err := r.Send(context.Background(), simpleMsg())
	if err == nil {
		t.Fatal("send 3 succeeded, want a quota error")
	}
	var te *TransportError
	if !errors.As(err, &te) {
		t.Fatalf("error is %T, want *TransportError", err)
	}
	if te.Class != ErrQuotaExhausted {
		t.Errorf("Class = %v, want quota_exhausted", te.Class)
	}
	if !strings.Contains(err.Error(), "spent its send quota") {
		t.Errorf("error = %v, want it to say the quota is spent", err)
	}
	if len(routeExhausted) != 1 || routeExhausted[0] != "/api/alerts" {
		t.Errorf("OnRouteQuotaExhausted = %v, want [/api/alerts]", routeExhausted)
	}
}

// The message is fine and will send after the window rolls over, so the
// queue must hold it — but no inline retry inside a 10s request could
// possibly help, so it must not be retryable.
func TestQuotaExhausted_DeferrableButNotRetryable(t *testing.T) {
	err := &TransportError{Class: ErrQuotaExhausted, Message: "out of budget"}
	if IsRetryable(err) {
		t.Error("IsRetryable = true — a daily budget does not return within a 5s backoff")
	}
	if !IsDeferrable(err) {
		t.Error("IsDeferrable = false — the message will send once the window rolls over")
	}
}

func TestIsDeferrable(t *testing.T) {
	for _, tc := range []struct {
		class ErrorClass
		want  bool
	}{
		{ErrTransient, true},
		{ErrRateLimited, true},
		{ErrQuotaExhausted, true},
		{ErrTerminal, false},
		{ErrUnknown, false},
	} {
		if got := IsDeferrable(&TransportError{Class: tc.class}); got != tc.want {
			t.Errorf("IsDeferrable(%v) = %v, want %v", tc.class, got, tc.want)
		}
	}
	if IsDeferrable(errors.New("bare")) {
		t.Error("IsDeferrable(bare error) = true, want false")
	}
}

// A broken provider and a spent budget call for different operator
// responses, so the mixed case must report the more urgent one — and an
// open circuit is both more urgent and the shorter wait.
func TestRouter_OpenCircuitOutranksQuotaInMixedExhaustion(t *testing.T) {
	tripped := NewBreaker(BreakerConfig{FailureThreshold: 1, Cooldown: time.Hour})
	tripped.Failure(&TransportError{Class: ErrTransient})

	broken := &Provider{Name: "broken", Transport: succeeding("x"), Weight: 1, Breaker: tripped}
	spent := costProvider("spent", 0, 1, succeeding("y"))

	r, _, _ := quotaRouter(t, "/x", StrategyFailover, spent, broken)
	if _, err := r.Send(context.Background(), simpleMsg()); err != nil {
		t.Fatalf("first send: %v", err)
	}

	_, err := r.Send(context.Background(), simpleMsg())
	var te *TransportError
	if !errors.As(err, &te) {
		t.Fatalf("error is %T", err)
	}
	if te.Class != ErrTransient {
		t.Errorf("Class = %v, want transient (an open circuit outranks a spent budget)", te.Class)
	}
	if !strings.Contains(err.Error(), "1 out of quota") {
		t.Errorf("error = %v, want it to account for both reasons", err)
	}
}

// Rollover restores capacity with no restart and no scheduled job.
func TestRouter_QuotaRecoversAfterRollover(t *testing.T) {
	freeT := succeeding("f")
	free := costProvider("free", 0, 1, freeT)
	paid := costProvider("paid", 100_000, 0, succeeding("p"))

	r, _, clock := quotaRouter(t, "/x", StrategyFailover, free, paid)

	res, _ := r.Send(context.Background(), simpleMsg())
	if res.Provider != "free" {
		t.Fatalf("send 1 went to %q", res.Provider)
	}
	res, _ = r.Send(context.Background(), simpleMsg())
	if res.Provider != "paid" {
		t.Fatalf("send 2 went to %q, want paid", res.Provider)
	}

	*clock = at(2026, 9, 8, 0) // next UTC day
	res, _ = r.Send(context.Background(), simpleMsg())
	if res.Provider != "free" {
		t.Errorf("send 3 went to %q, want free after rollover", res.Provider)
	}
	if len(freeT.Sent) != 2 {
		t.Errorf("free took %d messages, want 2", len(freeT.Sent))
	}
}

// A router with no quota source must behave exactly as it did before
// quotas existed (NFR32).
func TestRouter_NilQuotaSourceIgnoresLimits(t *testing.T) {
	mt := succeeding("a")
	p := costProvider("a", 0, 1, mt) // a limit that will not be enforced
	r := mustRouter(t, "/x", mustRoute(t, nil, StrategyFailover, p))

	for i := 0; i < 5; i++ {
		if _, err := r.Send(context.Background(), simpleMsg()); err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
	}
	if len(mt.Sent) != 5 {
		t.Errorf("Sent = %d, want 5 — a nil quota source must enforce nothing", len(mt.Sent))
	}
}

// --- cost strategies ---------------------------------------------------

func TestRouter_FreeFirstPrefersZeroCost(t *testing.T) {
	paidT, freeT := succeeding("paid"), succeeding("free")
	// Declared paid-first on purpose: the strategy must reorder.
	paid := costProvider("paid", 100_000, 0, paidT)
	free := costProvider("free", 0, 0, freeT)

	r, _, _ := quotaRouter(t, "/x", StrategyFreeFirst, paid, free)
	for i := 0; i < 5; i++ {
		if _, err := r.Send(context.Background(), simpleMsg()); err != nil {
			t.Fatal(err)
		}
	}
	if len(freeT.Sent) != 5 || len(paidT.Sent) != 0 {
		t.Errorf("split = free:%d paid:%d, want all on free", len(freeT.Sent), len(paidT.Sent))
	}
}

func TestRouter_FreeFirstFallsToPaidWhenFreeQuotaSpent(t *testing.T) {
	freeAT, freeBT, paidT := succeeding("fa"), succeeding("fb"), succeeding("p")
	freeA := costProvider("free-a", 0, 2, freeAT)
	freeB := costProvider("free-b", 0, 1, freeBT)
	paid := costProvider("paid", 50_000, 0, paidT)

	r, _, _ := quotaRouter(t, "/x", StrategyFreeFirst, paid, freeA, freeB)

	got := make([]string, 0, 5)
	for i := 0; i < 5; i++ {
		res, err := r.Send(context.Background(), simpleMsg())
		if err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
		got = append(got, res.Provider)
	}
	want := []string{"free-a", "free-a", "free-b", "paid", "paid"}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("send %d went to %q, want %q (order: %v)", i+1, got[i], want[i], got)
			break
		}
	}
}

func TestRouter_CheapestOrdersByPrice(t *testing.T) {
	expensiveT, midT, cheapT := succeeding("e"), succeeding("m"), succeeding("c")
	expensive := costProvider("expensive", 800_000, 0, expensiveT)
	mid := costProvider("mid", 300_000, 0, midT)
	cheap := costProvider("cheap", 100_000, 0, cheapT)

	r, _, _ := quotaRouter(t, "/x", StrategyCheapest, expensive, mid, cheap)
	for i := 0; i < 4; i++ {
		res, err := r.Send(context.Background(), simpleMsg())
		if err != nil {
			t.Fatal(err)
		}
		if res.Provider != "cheap" {
			t.Fatalf("send %d went to %q, want cheap", i+1, res.Provider)
		}
	}
	if len(midT.Sent)+len(expensiveT.Sent) != 0 {
		t.Error("a pricier provider took traffic while the cheapest was healthy")
	}
}

func TestRouter_CheapestClimbsAsQuotaRunsOut(t *testing.T) {
	cheap := costProvider("cheap", 100_000, 1, succeeding("c"))
	mid := costProvider("mid", 300_000, 1, succeeding("m"))
	expensive := costProvider("expensive", 800_000, 0, succeeding("e"))

	r, _, _ := quotaRouter(t, "/x", StrategyCheapest, expensive, mid, cheap)

	want := []string{"cheap", "mid", "expensive", "expensive"}
	for i, w := range want {
		res, err := r.Send(context.Background(), simpleMsg())
		if err != nil {
			t.Fatalf("send %d: %v", i+1, err)
		}
		if res.Provider != w {
			t.Fatalf("send %d went to %q, want %q", i+1, res.Provider, w)
		}
	}
}

// Equal prices must resolve to declared order, so the same config
// always produces the same route.
func TestRouter_CheapestTieBreaksOnDeclaredOrder(t *testing.T) {
	firstT, secondT := succeeding("1"), succeeding("2")
	first := costProvider("first", 200_000, 0, firstT)
	second := costProvider("second", 200_000, 0, secondT)

	for run := 0; run < 3; run++ {
		r, _, _ := quotaRouter(t, "/x", StrategyCheapest, first, second)
		res, err := r.Send(context.Background(), simpleMsg())
		if err != nil {
			t.Fatal(err)
		}
		if res.Provider != "first" {
			t.Fatalf("run %d chose %q, want the first-declared of two equal prices", run, res.Provider)
		}
	}
}

func TestRouter_CostStrategiesStillFailOver(t *testing.T) {
	downT := &mockTransport{SendErr: &TransportError{Class: ErrTransient, Message: "down"}}
	cheap := costProvider("cheap", 100_000, 0, downT)
	pricier := costProvider("pricier", 500_000, 0, succeeding("p"))

	r, _, _ := quotaRouter(t, "/x", StrategyCheapest, pricier, cheap)
	res, err := r.Send(context.Background(), simpleMsg())
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if res.Provider != "pricier" {
		t.Errorf("Provider = %q, want pricier — cheapest must still fail over", res.Provider)
	}
}

// The cost orderings are static, so they must not consume the rotation
// state the weighted/round-robin strategies use.
func TestRouter_CostStrategyOrderIsStable(t *testing.T) {
	a := costProvider("a", 0, 0, succeeding("a"))
	b := costProvider("b", 100_000, 0, succeeding("b"))
	route := mustRoute(t, nil, StrategyFreeFirst, b, a)

	for i := 0; i < 10; i++ {
		order := route.order()
		if order[0].Name != "a" || order[1].Name != "b" {
			t.Fatalf("iteration %d order = [%s %s], want [a b]", i, order[0].Name, order[1].Name)
		}
	}
}

func TestParseStrategy_CostStrategies(t *testing.T) {
	for _, s := range []string{"free_first", "cheapest"} {
		got, err := ParseStrategy(s)
		if err != nil {
			t.Errorf("ParseStrategy(%q): %v", s, err)
		}
		if string(got) != s {
			t.Errorf("ParseStrategy(%q) = %q", s, got)
		}
	}
	if _, err := ParseStrategy("free"); err == nil {
		t.Error("ParseStrategy(\"free\") = nil error, want one")
	} else if !strings.Contains(err.Error(), "free_first") {
		t.Errorf("error = %v, want it to list the known strategies", err)
	}
}
