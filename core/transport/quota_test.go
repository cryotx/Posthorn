package transport

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// --- test doubles ------------------------------------------------------

// memUsageStore is an in-process UsageStore. It records every write so
// tests can assert that persistence actually happened, and can be made
// to fail on demand.
type memUsageStore struct {
	mu     sync.Mutex
	rows   map[string]int64 // "provider|window|period" → used
	writes int
	failOn error
}

func newMemStore() *memUsageStore {
	return &memUsageStore{rows: map[string]int64{}}
}

func (m *memUsageStore) LoadProviderUsage(day, month string) ([]ProviderUsage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failOn != nil {
		return nil, m.failOn
	}
	byProvider := map[string]*ProviderUsage{}
	for k, v := range m.rows {
		parts := strings.Split(k, "|")
		provider, window, period := parts[0], parts[1], parts[2]
		if (window == "day" && period != day) || (window == "month" && period != month) {
			continue
		}
		u, ok := byProvider[provider]
		if !ok {
			u = &ProviderUsage{Provider: provider}
			byProvider[provider] = u
		}
		if window == "day" {
			u.UsedToday = v
		} else {
			u.UsedMonth = v
		}
	}
	out := make([]ProviderUsage, 0, len(byProvider))
	for _, u := range byProvider {
		out = append(out, *u)
	}
	return out, nil
}

func (m *memUsageStore) AddProviderUsage(provider, day, month string, n int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failOn != nil {
		return m.failOn
	}
	m.writes++
	m.rows[provider+"|day|"+day] += n
	m.rows[provider+"|month|"+month] += n
	return nil
}

func (m *memUsageStore) get(provider, window, period string) int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.rows[provider+"|"+window+"|"+period]
}

func quotaProvider(name string, daily, monthly int64) *Provider {
	return &Provider{
		Name: name, Type: "mock", Weight: 1,
		Transport: succeeding(name),
		Breaker:   NewBreaker(BreakerConfig{Disabled: true}),
		Quota:     Quota{Daily: daily, Monthly: monthly},
	}
}

func at(y int, m time.Month, d, h int) time.Time {
	return time.Date(y, m, d, h, 0, 0, 0, time.UTC)
}

// --- period keys -------------------------------------------------------

func TestPeriodKeysAreUTC(t *testing.T) {
	// 2026-09-07 23:30 in +08:00 is 15:30 UTC the same day; 00:30 the
	// next day locally is still 16:30 UTC on the 7th. Whichever the
	// operator expects, the answer must be stable and documented.
	tz := time.FixedZone("test", 8*3600)
	local := time.Date(2026, 9, 8, 0, 30, 0, 0, tz)
	if got := DayKey(local); got != "2026-09-07" {
		t.Errorf("DayKey = %q, want the UTC day 2026-09-07", got)
	}
	if got := MonthKey(local); got != "2026-09" {
		t.Errorf("MonthKey = %q, want 2026-09", got)
	}
}

// --- limits ------------------------------------------------------------

func TestLocalQuota_UnlimitedAlwaysAllowed(t *testing.T) {
	q := NewLocalQuota(nil, nil)
	p := quotaProvider("free", 0, 0)
	now := at(2026, 9, 7, 12)
	for i := 0; i < 1000; i++ {
		if v := q.Check(p, now); !v.Allowed {
			t.Fatalf("unlimited provider blocked after %d sends", i)
		}
		q.Record(p, now)
	}
}

// An unlimited provider must not generate storage writes — a row per
// send for something nobody enforces is pure overhead.
func TestLocalQuota_UnlimitedDoesNotPersist(t *testing.T) {
	store := newMemStore()
	q := NewLocalQuota(store, nil)
	p := quotaProvider("free", 0, 0)
	now := at(2026, 9, 7, 12)
	for i := 0; i < 5; i++ {
		q.Record(p, now)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.writes != 0 {
		t.Errorf("writes = %d, want 0 for an unlimited provider", store.writes)
	}
}

func TestLocalQuota_DailyLimitBlocks(t *testing.T) {
	q := NewLocalQuota(nil, nil)
	p := quotaProvider("resend", 3, 0)
	now := at(2026, 9, 7, 12)

	for i := 0; i < 3; i++ {
		if v := q.Check(p, now); !v.Allowed {
			t.Fatalf("blocked at send %d, want the first 3 allowed", i+1)
		}
		q.Record(p, now)
	}
	v := q.Check(p, now)
	if v.Allowed {
		t.Fatal("send 4 allowed, want blocked on the daily limit")
	}
	if v.Window != WindowDaily {
		t.Errorf("Window = %q, want daily", v.Window)
	}
	if v.Limit != 3 || v.Used != 3 {
		t.Errorf("verdict = %+v, want limit 3 used 3", v)
	}
}

func TestLocalQuota_MonthlyLimitBlocks(t *testing.T) {
	q := NewLocalQuota(nil, nil)
	p := quotaProvider("brevo", 0, 4)

	// Spread across days so the daily window can't be the one blocking.
	for i := 0; i < 4; i++ {
		day := at(2026, 9, 1+i, 12)
		if v := q.Check(p, day); !v.Allowed {
			t.Fatalf("blocked at send %d", i+1)
		}
		q.Record(p, day)
	}
	v := q.Check(p, at(2026, 9, 5, 12))
	if v.Allowed {
		t.Fatal("send 5 allowed, want blocked on the monthly limit")
	}
	if v.Window != WindowMonthly {
		t.Errorf("Window = %q, want monthly", v.Window)
	}
}

// Both windows configured: the daily one is the tighter budget in every
// realistic config, so it is the one the verdict should name.
func TestLocalQuota_DailyReportedBeforeMonthly(t *testing.T) {
	q := NewLocalQuota(nil, nil)
	p := quotaProvider("both", 2, 100)
	now := at(2026, 9, 7, 12)
	q.Record(p, now)
	q.Record(p, now)
	if v := q.Check(p, now); v.Window != WindowDaily {
		t.Errorf("Window = %q, want daily", v.Window)
	}
}

// --- rollover ----------------------------------------------------------

func TestLocalQuota_DailyRollover(t *testing.T) {
	q := NewLocalQuota(nil, nil)
	p := quotaProvider("resend", 2, 0)

	day1 := at(2026, 9, 7, 23)
	q.Record(p, day1)
	q.Record(p, day1)
	if q.Check(p, day1).Allowed {
		t.Fatal("day 1 quota not exhausted")
	}

	// One hour later is a new UTC day. No cron ran; the period key
	// simply changed.
	day2 := at(2026, 9, 8, 0)
	if !q.Check(p, day2).Allowed {
		t.Fatal("quota did not roll over into the new day")
	}
	u, _ := q.Snapshot("resend", day2)
	if u.UsedToday != 0 {
		t.Errorf("UsedToday = %d after rollover, want 0", u.UsedToday)
	}
}

func TestLocalQuota_MonthlyRollover(t *testing.T) {
	q := NewLocalQuota(nil, nil)
	p := quotaProvider("brevo", 0, 2)

	sep := at(2026, 9, 30, 12)
	q.Record(p, sep)
	q.Record(p, sep)
	if q.Check(p, sep).Allowed {
		t.Fatal("september quota not exhausted")
	}

	oct := at(2026, 10, 1, 0)
	if !q.Check(p, oct).Allowed {
		t.Fatal("quota did not roll over into the new month")
	}
	u, _ := q.Snapshot("brevo", oct)
	if u.UsedMonth != 0 || u.UsedToday != 0 {
		t.Errorf("usage = %+v after month rollover, want zeroed", u)
	}
}

// The rollover must be a property of the data, not of a timer: a
// process that was asleep across midnight — or several midnights — must
// come back with a correct count on its first call, not a stale one.
func TestLocalQuota_RolloverAfterLongGap(t *testing.T) {
	q := NewLocalQuota(nil, nil)
	p := quotaProvider("resend", 2, 0)

	q.Record(p, at(2026, 9, 7, 12))
	q.Record(p, at(2026, 9, 7, 12))

	// Wake up three weeks later. Nothing ran in between.
	later := at(2026, 9, 28, 9)
	if !q.Check(p, later).Allowed {
		t.Fatal("a three-week gap did not roll the daily window over")
	}
	u, _ := q.Snapshot("resend", later)
	if u.Day != "2026-09-28" || u.UsedToday != 0 {
		t.Errorf("snapshot = %+v, want the current day at zero", u)
	}
	// Same month, so the monthly counter must NOT have been reset.
	if u.UsedMonth != 2 {
		t.Errorf("UsedMonth = %d, want 2 — the month did not change", u.UsedMonth)
	}
}

// --- persistence -------------------------------------------------------

func TestLocalQuota_WritesThroughToStore(t *testing.T) {
	store := newMemStore()
	q := NewLocalQuota(store, nil)
	p := quotaProvider("resend", 10, 100)
	now := at(2026, 9, 7, 12)

	q.Record(p, now)
	q.Record(p, now)

	if got := store.get("resend", "day", "2026-09-07"); got != 2 {
		t.Errorf("persisted day count = %d, want 2", got)
	}
	if got := store.get("resend", "month", "2026-09"); got != 2 {
		t.Errorf("persisted month count = %d, want 2", got)
	}
}

// The restart case: a new tracker over the same store must see the
// counters the old one wrote, or every restart would hand the operator
// a fresh free tier.
func TestLocalQuota_RestoreAfterRestart(t *testing.T) {
	store := newMemStore()
	now := at(2026, 9, 7, 12)
	p := quotaProvider("resend", 3, 0)

	first := NewLocalQuota(store, nil)
	first.Record(p, now)
	first.Record(p, now)

	restarted := NewLocalQuota(store, nil)
	if err := restarted.Restore(now); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	u, ok := restarted.Snapshot("resend", now)
	if !ok || u.UsedToday != 2 || u.UsedMonth != 2 {
		t.Fatalf("restored usage = %+v ok=%v, want 2/2", u, ok)
	}
	// One send left, then blocked — the budget continued, it did not restart.
	if !restarted.Check(p, now).Allowed {
		t.Fatal("third send blocked after restore, want allowed")
	}
	restarted.Record(p, now)
	if restarted.Check(p, now).Allowed {
		t.Fatal("fourth send allowed after restore, want blocked")
	}
}

// Restoring only reads the CURRENT periods: yesterday's exhausted
// counter must not follow the process into today.
func TestLocalQuota_RestoreIgnoresStalePeriods(t *testing.T) {
	store := newMemStore()
	p := quotaProvider("resend", 2, 0)

	yesterday := at(2026, 9, 7, 12)
	first := NewLocalQuota(store, nil)
	first.Record(p, yesterday)
	first.Record(p, yesterday)

	today := at(2026, 9, 8, 9)
	restarted := NewLocalQuota(store, nil)
	if err := restarted.Restore(today); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if !restarted.Check(p, today).Allowed {
		t.Error("yesterday's exhausted daily counter leaked into today")
	}
	u, _ := restarted.Snapshot("resend", today)
	if u.UsedMonth != 2 {
		t.Errorf("UsedMonth = %d, want 2 — the month row is still current", u.UsedMonth)
	}
}

func TestLocalQuota_RestoreErrorSurfaces(t *testing.T) {
	store := newMemStore()
	store.failOn = fmt.Errorf("disk gone")
	q := NewLocalQuota(store, nil)
	if err := q.Restore(at(2026, 9, 7, 12)); err == nil {
		t.Error("Restore = nil error on a failing store")
	}
}

// A failing write must not stop the send path: the message went out,
// and refusing to count it (or panicking) would be worse than an
// under-count the operator is told about.
func TestLocalQuota_PersistFailureIsReportedNotFatal(t *testing.T) {
	store := newMemStore()
	store.failOn = fmt.Errorf("disk full")
	var reported []error
	q := NewLocalQuota(store, func(err error) { reported = append(reported, err) })
	p := quotaProvider("resend", 5, 0)
	now := at(2026, 9, 7, 12)

	q.Record(p, now)

	if len(reported) != 1 {
		t.Fatalf("reported %d errors, want 1", len(reported))
	}
	// In-memory accounting continues regardless.
	u, _ := q.Snapshot("resend", now)
	if u.UsedToday != 1 {
		t.Errorf("UsedToday = %d, want 1 — the in-memory count must survive a write failure", u.UsedToday)
	}
}

func TestLocalQuota_NilStoreIsInMemoryOnly(t *testing.T) {
	q := NewLocalQuota(nil, nil)
	if err := q.Restore(at(2026, 9, 7, 12)); err != nil {
		t.Fatalf("Restore with nil store: %v", err)
	}
	p := quotaProvider("resend", 1, 0)
	now := at(2026, 9, 7, 12)
	q.Record(p, now)
	if q.Check(p, now).Allowed {
		t.Error("in-memory counting did not take effect")
	}
}

// --- concurrency -------------------------------------------------------

// A pooled provider is shared across endpoints, so its counters are hit
// concurrently by definition. The count must be exact — an over-count
// wastes allowance, an under-count overspends it.
func TestLocalQuota_ConcurrentRecordIsExact(t *testing.T) {
	store := newMemStore()
	q := NewLocalQuota(store, nil)
	p := quotaProvider("resend", 0, 100000)
	now := at(2026, 9, 7, 12)

	const goroutines, each = 16, 100
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < each; j++ {
				q.Record(p, now)
				q.Check(p, now)
				q.Snapshot(p.Name, now)
			}
		}()
	}
	wg.Wait()

	want := int64(goroutines * each)
	u, _ := q.Snapshot("resend", now)
	if u.UsedMonth != want {
		t.Errorf("UsedMonth = %d, want %d", u.UsedMonth, want)
	}
	if got := store.get("resend", "month", "2026-09"); got != want {
		t.Errorf("persisted = %d, want %d", got, want)
	}
}

// --- Remaining ---------------------------------------------------------

func TestRemaining(t *testing.T) {
	q := NewLocalQuota(nil, nil)
	p := quotaProvider("resend", 10, 100)
	now := at(2026, 9, 7, 12)
	for i := 0; i < 3; i++ {
		q.Record(p, now)
	}

	daily, hasDaily, monthly, hasMonthly := Remaining(q, p, now)
	if !hasDaily || daily != 7 {
		t.Errorf("daily remaining = %d (has=%v), want 7", daily, hasDaily)
	}
	if !hasMonthly || monthly != 97 {
		t.Errorf("monthly remaining = %d (has=%v), want 97", monthly, hasMonthly)
	}
}

// An unlimited window has no meaningful "remaining", and inventing a
// large number would break any alert written against the gauge.
func TestRemaining_UnlimitedReportsNothing(t *testing.T) {
	q := NewLocalQuota(nil, nil)
	p := quotaProvider("free", 0, 0)
	_, hasDaily, _, hasMonthly := Remaining(q, p, at(2026, 9, 7, 12))
	if hasDaily || hasMonthly {
		t.Error("Remaining reported a bound for an unlimited provider")
	}
}

func TestRemaining_NeverNegative(t *testing.T) {
	q := NewLocalQuota(nil, nil)
	p := quotaProvider("resend", 2, 0)
	now := at(2026, 9, 7, 12)
	// Force an over-count, as a lost-write recovery or a limit lowered
	// mid-period would produce.
	for i := 0; i < 5; i++ {
		q.Record(p, now)
	}
	daily, _, _, _ := Remaining(q, p, now)
	if daily != 0 {
		t.Errorf("daily remaining = %d, want 0 (clamped)", daily)
	}
}

func TestRemaining_NilSourceIsSafe(t *testing.T) {
	_, hasDaily, _, hasMonthly := Remaining(nil, quotaProvider("x", 1, 1), time.Now())
	if hasDaily || hasMonthly {
		t.Error("Remaining with a nil source reported bounds")
	}
}

func TestLocalQuota_SnapshotOfUnknownProvider(t *testing.T) {
	q := NewLocalQuota(nil, nil)
	now := at(2026, 9, 7, 12)
	u, ok := q.Snapshot("never-used", now)
	if ok {
		t.Error("Snapshot reported ok for a provider with no usage")
	}
	if u.Day != "2026-09-07" || u.UsedToday != 0 {
		t.Errorf("snapshot = %+v, want a zeroed snapshot keyed to today", u)
	}
}
