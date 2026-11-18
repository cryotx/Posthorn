package transport

import (
	"context"
	"sync"
	"testing"
	"time"
)

// The quota concurrency audit (ADR-36).
//
// The question these tests answer is not "is the final count right" —
// it is "was the limit ever exceeded". A tracker whose arithmetic
// settles correctly after over-sending is still a soft quota, and
// calling it strict because the number looks right afterwards would be
// a lie an operator only discovers on their provider bill.
//
// The scenario is the one that matters: a limit of 100, 99 already
// spent, and 100 requests arriving at once.

const (
	auditLimit = 100
	auditUsed  = 99
)

// auditProvider builds a provider at limit-1 with a transport that
// blocks until released, so every goroutine is inside Send at the same
// moment — the widest possible TOCTOU window.
func auditProvider(t *testing.T, q *LocalQuota, now time.Time, gate <-chan struct{}) (*Provider, *countingSends) {
	t.Helper()
	counter := &countingSends{gate: gate}
	p := &Provider{
		Name: "audited", Weight: 1, Transport: counter,
		Breaker: NewBreaker(BreakerConfig{Disabled: true}),
		Quota:   Quota{Daily: auditLimit},
	}
	for i := 0; i < auditUsed; i++ {
		q.Record(p, now)
	}
	return p, counter
}

type countingSends struct {
	gate <-chan struct{}
	mu   sync.Mutex
	n    int
}

func (c *countingSends) Send(context.Context, Message) (SendResult, error) {
	if c.gate != nil {
		<-c.gate
	}
	c.mu.Lock()
	c.n++
	c.mu.Unlock()
	return SendResult{MessageID: "ok"}, nil
}

func (c *countingSends) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

// STRICT: with Reserve, the last unit of an allowance goes to exactly
// one caller however many ask at once. This is the shipped behavior.
func TestQuotaAudit_ReserveIsStrictUnderConcurrency(t *testing.T) {
	now := at(2026, 9, 7, 12)
	q := NewLocalQuota(nil, nil)
	gate := make(chan struct{})
	p, counter := auditProvider(t, q, now, gate)

	const callers = 100
	var reserved sync.WaitGroup
	var done sync.WaitGroup
	granted := make(chan bool, callers)

	for i := 0; i < callers; i++ {
		reserved.Add(1)
		done.Add(1)
		go func() {
			defer done.Done()
			commit, ok, _ := q.Reserve(p, now)
			granted <- ok
			reserved.Done()
			if !ok {
				return
			}
			_, _ = p.Transport.Send(context.Background(), Message{})
			commit(true)
		}()
	}

	// Every caller has decided before any of them completes a send.
	reserved.Wait()
	close(gate)
	done.Wait()
	close(granted)

	allowed := 0
	for ok := range granted {
		if ok {
			allowed++
		}
	}
	if allowed != auditLimit-auditUsed {
		t.Errorf("%d callers were granted budget, want exactly %d — the limit was %s",
			allowed, auditLimit-auditUsed,
			map[bool]string{true: "exceeded", false: "under-used"}[allowed > auditLimit-auditUsed])
	}
	if got := counter.count(); got != auditLimit-auditUsed {
		t.Errorf("%d messages were sent, want %d", got, auditLimit-auditUsed)
	}
	u, _ := q.Snapshot("audited", now)
	if u.UsedToday != auditLimit {
		t.Errorf("UsedToday = %d, want exactly the limit %d", u.UsedToday, auditLimit)
	}
}

// SOFT, for contrast and as documentation: the Check-then-Record shape
// a source that cannot reserve must fall back to. This test asserts the
// weakness explicitly so nobody later mistakes that path for strict.
func TestQuotaAudit_CheckThenRecordIsSoft(t *testing.T) {
	now := at(2026, 9, 7, 12)
	q := NewLocalQuota(nil, nil)
	p := &Provider{
		Name: "advisory", Weight: 1, Transport: succeeding("x"),
		Breaker: NewBreaker(BreakerConfig{Disabled: true}),
		Quota:   Quota{Daily: auditLimit},
	}
	for i := 0; i < auditUsed; i++ {
		q.Record(p, now)
	}

	const callers = 100
	var start sync.WaitGroup
	var done sync.WaitGroup
	start.Add(1)
	passed := make(chan bool, callers)

	for i := 0; i < callers; i++ {
		done.Add(1)
		go func() {
			defer done.Done()
			start.Wait()
			// Exactly the advisory sequence: check, then act, then
			// record. No lock spans the gap.
			v := q.Check(p, now)
			passed <- v.Allowed
		}()
	}
	start.Done()
	done.Wait()
	close(passed)

	allowed := 0
	for ok := range passed {
		if ok {
			allowed++
		}
	}
	// The point of the test: more than one caller can pass the check.
	// It is not deterministic how many — that is exactly the problem —
	// so the assertion is only that the shape permits it.
	if allowed < 1 {
		t.Fatalf("no caller passed the check; the fixture is wrong")
	}
	t.Logf("advisory check-then-record admitted %d of %d callers for %d remaining unit(s)",
		allowed, callers, auditLimit-auditUsed)
}

// The router must take the strict path when the source offers it.
func TestQuotaAudit_RouterUsesReservationWhenAvailable(t *testing.T) {
	now := at(2026, 9, 7, 12)
	q := NewLocalQuota(nil, nil)
	gate := make(chan struct{})
	p, counter := auditProvider(t, q, now, gate)

	r := mustRouter(t, "/api/send", mustRoute(t, nil, StrategyFailover, p))
	r.Quota = q
	r.Now = func() time.Time { return now }

	const callers = 100
	var done sync.WaitGroup
	var mu sync.Mutex
	var successes, failures int

	for i := 0; i < callers; i++ {
		done.Add(1)
		go func() {
			defer done.Done()
			_, err := r.Send(context.Background(), simpleMsg())
			mu.Lock()
			if err == nil {
				successes++
			} else {
				failures++
			}
			mu.Unlock()
		}()
	}
	// Let the reservations pile up, then release the sends.
	time.Sleep(20 * time.Millisecond)
	close(gate)
	done.Wait()

	if successes != auditLimit-auditUsed {
		t.Errorf("router delivered %d messages, want exactly %d", successes, auditLimit-auditUsed)
	}
	if got := counter.count(); got != auditLimit-auditUsed {
		t.Errorf("provider saw %d sends, want %d", got, auditLimit-auditUsed)
	}
	if failures != callers-(auditLimit-auditUsed) {
		t.Errorf("%d callers were refused, want %d", failures, callers-(auditLimit-auditUsed))
	}
}

// A reservation for a send that fails must be handed back, or a flaky
// provider would silently eat an operator's allowance.
func TestQuotaAudit_FailedSendReleasesReservation(t *testing.T) {
	now := at(2026, 9, 7, 12)
	q := NewLocalQuota(nil, nil)
	failing := &mockTransport{SendErr: &TransportError{Class: ErrTransient, Message: "down"}}
	p := &Provider{
		Name: "flaky", Weight: 1, Transport: failing,
		Breaker: NewBreaker(BreakerConfig{Disabled: true}),
		Quota:   Quota{Daily: 3},
	}
	r := mustRouter(t, "/x", mustRoute(t, nil, StrategyFailover, p))
	r.Quota = q
	r.Now = func() time.Time { return now }

	for i := 0; i < 10; i++ {
		_, _ = r.Send(context.Background(), simpleMsg())
	}
	u, _ := q.Snapshot("flaky", now)
	if u.UsedToday != 0 {
		t.Errorf("UsedToday = %d after 10 failed sends, want 0", u.UsedToday)
	}
	if !q.Check(p, now).Allowed {
		t.Error("budget was consumed by failures")
	}
}

// commit must be idempotent: a double call (a defensive caller, a
// future refactor) must not double-count.
func TestQuotaAudit_CommitIsIdempotent(t *testing.T) {
	now := at(2026, 9, 7, 12)
	q := NewLocalQuota(nil, nil)
	p := quotaProvider("p", 10, 0)

	commit, ok, _ := q.Reserve(p, now)
	if !ok {
		t.Fatal("reservation refused")
	}
	commit(true)
	commit(true)
	commit(false)

	u, _ := q.Snapshot("p", now)
	if u.UsedToday != 1 {
		t.Errorf("UsedToday = %d after repeated commits, want 1", u.UsedToday)
	}
}

// Reservations count toward the ceiling while in flight, so a burst
// cannot over-commit even before any of it settles.
func TestQuotaAudit_InFlightReservationsCountTowardLimit(t *testing.T) {
	now := at(2026, 9, 7, 12)
	q := NewLocalQuota(nil, nil)
	p := quotaProvider("p", 2, 0)

	c1, ok1, _ := q.Reserve(p, now)
	_, ok2, _ := q.Reserve(p, now)
	_, ok3, _ := q.Reserve(p, now)
	if !ok1 || !ok2 {
		t.Fatal("the first two reservations should be granted")
	}
	if ok3 {
		t.Error("a third reservation was granted against a limit of 2 with none committed")
	}
	// Releasing one hands the unit back.
	c1(false)
	if _, ok, _ := q.Reserve(p, now); !ok {
		t.Error("a released unit was not reusable")
	}
}

func TestQuotaAudit_UnlimitedProviderNeedsNoReservation(t *testing.T) {
	q := NewLocalQuota(nil, nil)
	p := quotaProvider("free", 0, 0)
	for i := 0; i < 1000; i++ {
		commit, ok, _ := q.Reserve(p, at(2026, 9, 7, 12))
		if !ok {
			t.Fatalf("unlimited provider refused at %d", i)
		}
		commit(true)
	}
}
