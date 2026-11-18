package transport

import (
	"fmt"
	"sync"
	"time"
)

// QuotaWindow names one accounting period.
type QuotaWindow string

const (
	// WindowDaily is the calendar-day budget.
	WindowDaily QuotaWindow = "daily"

	// WindowMonthly is the calendar-month budget.
	WindowMonthly QuotaWindow = "monthly"
)

// Quota is a provider's send budget. A zero limit means unlimited, which
// is what every provider gets when the operator says nothing — so a
// config with no quota fields behaves exactly as it did before (NFR32).
type Quota struct {
	// Daily bounds successful sends per UTC calendar day.
	Daily int64

	// Monthly bounds successful sends per UTC calendar month.
	Monthly int64
}

// Unlimited reports whether neither window is bounded.
func (q Quota) Unlimited() bool { return q.Daily <= 0 && q.Monthly <= 0 }

// Usage is one provider's counters for the periods they were recorded
// in. The period keys are carried alongside the counts because that is
// what makes rollover self-correcting: a counter is only meaningful
// together with the period it belongs to, so a stale key reads as zero
// rather than as a wrong number.
type Usage struct {
	// Day is the UTC day key, "2006-01-02".
	Day string

	// UsedToday is successful sends recorded against Day.
	UsedToday int64

	// Month is the UTC month key, "2006-01".
	Month string

	// UsedMonth is successful sends recorded against Month.
	UsedMonth int64
}

// DayKey and MonthKey derive the period keys for an instant.
//
// UTC, deliberately. A provider's own quota window is in *its* timezone
// and no two agree, so any local-time choice would be wrong for some
// provider while looking authoritative. UTC is wrong for all of them in
// the same documented, predictable way, and an operator sizing a limit
// can leave headroom for the offset.
func DayKey(t time.Time) string   { return t.UTC().Format("2006-01-02") }
func MonthKey(t time.Time) string { return t.UTC().Format("2006-01") }

// QuotaVerdict is the answer to "may this provider take another
// message right now".
type QuotaVerdict struct {
	// Allowed is false when a configured limit is already reached.
	Allowed bool

	// Window names the exhausted budget when Allowed is false. It is a
	// closed enum, safe as a metric label.
	Window QuotaWindow

	// Limit and Used describe the exhausted window, for the log line an
	// operator will actually read.
	Limit int64
	Used  int64
}

// QuotaSource decides whether a provider has budget left and records
// consumption.
//
// The interface exists so `quota_source = "remote"` can be added later
// without touching the router: a remote source would poll a provider's
// own usage API and answer the same two questions. Nothing beyond the
// local implementation is built yet, and the interface is deliberately
// this small — anything richer would be guessing at the shape of APIs
// we have not read.
//
// Implementations must be safe for concurrent use.
type QuotaSource interface {
	// Check reports whether p may take another message at time now.
	// Providers with no configured limits must always be allowed.
	Check(p *Provider, now time.Time) QuotaVerdict

	// Record accounts for exactly one SUCCESSFUL send. Failed attempts
	// must never reach here — a provider that rejected a message did
	// not spend the operator's allowance on it.
	Record(p *Provider, now time.Time)

	// Snapshot returns a provider's current counters, or ok=false when
	// the source knows nothing about that name. Used for the remaining
	// gauges and for tests.
	Snapshot(name string, now time.Time) (Usage, bool)
}

// Reserver is the optional strict half of quota accounting (ADR-36).
//
// Check-then-Record is a time-of-check/time-of-use gap: with a limit of
// 100 and 99 used, a hundred concurrent sends can all pass Check before
// any of them Records, and all hundred go out. That is *soft* quota,
// and calling it strict because the final count is arithmetically
// correct would be a lie — the allowance was still exceeded.
//
// Reserve closes the gap by taking the unit up front, under the same
// lock that reads it. The returned commit MUST be called exactly once:
// commit(true) keeps the reservation and persists it, commit(false)
// hands it back — so a provider that failed still spends nothing.
//
// A source is free not to implement this. A remote, eventually
// consistent view of someone else's usage cannot honestly promise
// strictness, and the router falls back to advisory Check/Record for
// those.
type Reserver interface {
	Reserve(p *Provider, now time.Time) (commit func(kept bool), allowed bool, window string)
}

// UsageStore is the persistence seam for LocalQuota.
//
// Defined here rather than taken as *storage.Store because storage
// already imports transport (the retry worker replays Messages), so the
// dependency has to point this way. cmd/posthorn supplies the adapter,
// exactly as it does for the metrics hooks.
type UsageStore interface {
	// LoadProviderUsage returns the counters recorded against the given
	// period keys, for every provider that has any.
	LoadProviderUsage(day, month string) ([]ProviderUsage, error)

	// AddProviderUsage adds n to both period counters for one provider,
	// creating the rows if absent.
	AddProviderUsage(provider, day, month string, n int64) error
}

// ProviderUsage is one row of persisted counters.
type ProviderUsage struct {
	Provider  string
	UsedToday int64
	UsedMonth int64
}

// LocalQuota is the `quota_source = "local"` implementation: Posthorn
// counts its own successful sends.
//
// Counters live in memory for the read path — a quota check happens on
// every send and must not become a database round-trip — and are
// written through to the UsageStore so a restart does not hand every
// provider a fresh allowance. On startup the current day's and month's
// rows are read back.
//
// Rollover needs no scheduled job. Every Check and Record derives the
// period keys from the clock and compares them to the keys the cached
// counters were recorded under; a mismatch resets that counter. A
// process that is asleep, stopped, or crashed at midnight therefore
// still rolls over correctly on its next call — the boundary is a
// property of the data, not of a timer that has to fire.
type LocalQuota struct {
	store UsageStore

	// onPersistError reports write-through failures to the caller's
	// storage gate. Nil is fine; a nil store is fine too (usage is then
	// in-memory only and a restart resets it, which is the documented
	// no-[storage] behavior).
	onPersistError func(error)

	mu    sync.Mutex
	usage map[string]*Usage

	// reserved counts units taken but not yet committed, per provider.
	// Held separately from usage so a released reservation cannot
	// corrupt the committed count, and so a snapshot can report what is
	// actually spent rather than what is merely in flight.
	reserved map[string]int64
}

// NewLocalQuota builds a tracker. store may be nil, in which case
// counters are in-memory only.
func NewLocalQuota(store UsageStore, onPersistError func(error)) *LocalQuota {
	return &LocalQuota{
		store:          store,
		onPersistError: onPersistError,
		usage:          map[string]*Usage{},
		reserved:       map[string]int64{},
	}
}

// Reserve implements Reserver: strict, atomic quota accounting.
//
// The check and the take happen under one lock, so the last unit of an
// allowance can be handed to exactly one caller however many ask at
// once. Persistence still happens only on commit(true), which keeps
// FR99's "only successful sends count" true on disk.
func (q *LocalQuota) Reserve(p *Provider, now time.Time) (func(bool), bool, string) {
	noop := func(bool) {}
	if p == nil || p.Quota.Unlimited() {
		return noop, true, ""
	}

	q.mu.Lock()
	u := q.currentLocked(p.Name, now)
	inFlight := q.reserved[p.Name]
	if p.Quota.Daily > 0 && u.UsedToday+inFlight >= p.Quota.Daily {
		q.mu.Unlock()
		return noop, false, string(WindowDaily)
	}
	if p.Quota.Monthly > 0 && u.UsedMonth+inFlight >= p.Quota.Monthly {
		q.mu.Unlock()
		return noop, false, string(WindowMonthly)
	}
	q.reserved[p.Name]++
	q.mu.Unlock()

	day, month := DayKey(now), MonthKey(now)
	var once sync.Once
	return func(kept bool) {
		once.Do(func() {
			q.mu.Lock()
			if q.reserved[p.Name] > 0 {
				q.reserved[p.Name]--
			}
			if kept {
				u := q.currentLocked(p.Name, now)
				u.UsedToday++
				u.UsedMonth++
			}
			q.mu.Unlock()

			if !kept || q.store == nil {
				return
			}
			if err := q.store.AddProviderUsage(p.Name, day, month, 1); err != nil && q.onPersistError != nil {
				q.onPersistError(err)
			}
		})
	}, true, ""
}

// Restore loads persisted counters for the periods current at now. Call
// once at startup, before serving. A read failure is returned rather
// than swallowed: starting with counters the operator believes are
// persisted, when they are not, is worse than a loud startup error the
// caller can choose to downgrade.
func (q *LocalQuota) Restore(now time.Time) error {
	if q.store == nil {
		return nil
	}
	day, month := DayKey(now), MonthKey(now)
	rows, err := q.store.LoadProviderUsage(day, month)
	if err != nil {
		return fmt.Errorf("restore provider usage: %w", err)
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, row := range rows {
		q.usage[row.Provider] = &Usage{
			Day: day, UsedToday: row.UsedToday,
			Month: month, UsedMonth: row.UsedMonth,
		}
	}
	return nil
}

// Check implements QuotaSource.
func (q *LocalQuota) Check(p *Provider, now time.Time) QuotaVerdict {
	if p == nil || p.Quota.Unlimited() {
		return QuotaVerdict{Allowed: true}
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	u := q.currentLocked(p.Name, now)

	// Daily is checked first: it is the tighter window in every
	// real configuration, so it is the one an operator wants named.
	if p.Quota.Daily > 0 && u.UsedToday >= p.Quota.Daily {
		return QuotaVerdict{Window: WindowDaily, Limit: p.Quota.Daily, Used: u.UsedToday}
	}
	if p.Quota.Monthly > 0 && u.UsedMonth >= p.Quota.Monthly {
		return QuotaVerdict{Window: WindowMonthly, Limit: p.Quota.Monthly, Used: u.UsedMonth}
	}
	return QuotaVerdict{Allowed: true}
}

// Record implements QuotaSource. Called only after a provider accepted
// a message.
func (q *LocalQuota) Record(p *Provider, now time.Time) {
	if p == nil || p.Quota.Unlimited() {
		// Nothing to enforce, so nothing to store. An unlimited
		// provider writing a row per send would be pure overhead.
		return
	}
	day, month := DayKey(now), MonthKey(now)

	q.mu.Lock()
	u := q.currentLocked(p.Name, now)
	u.UsedToday++
	u.UsedMonth++
	q.mu.Unlock()

	if q.store == nil {
		return
	}
	// Write-through outside the lock: a slow disk must not serialise
	// every other provider's quota check behind it. A lost write costs
	// at most this one message's accounting after a restart, which is
	// the same NFR27 trade the submission log already makes — mail
	// first, bookkeeping second.
	if err := q.store.AddProviderUsage(p.Name, day, month, 1); err != nil && q.onPersistError != nil {
		q.onPersistError(err)
	}
}

// Snapshot implements QuotaSource.
func (q *LocalQuota) Snapshot(name string, now time.Time) (Usage, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if _, ok := q.usage[name]; !ok {
		// Report a zeroed, correctly-keyed snapshot rather than
		// "unknown": a provider that has sent nothing today has used
		// nothing today, and the gauge should say so.
		return Usage{Day: DayKey(now), Month: MonthKey(now)}, false
	}
	return *q.currentLocked(name, now), true
}

// currentLocked returns the cached counters for name, rolling over any
// period that no longer matches the clock. Caller holds q.mu.
func (q *LocalQuota) currentLocked(name string, now time.Time) *Usage {
	day, month := DayKey(now), MonthKey(now)
	u, ok := q.usage[name]
	if !ok {
		u = &Usage{Day: day, Month: month}
		q.usage[name] = u
		return u
	}
	if u.Day != day {
		u.Day, u.UsedToday = day, 0
	}
	if u.Month != month {
		u.Month, u.UsedMonth = month, 0
	}
	return u
}

// Remaining reports how many sends are left in each window for p, and
// whether that window is bounded at all. Used for the gauges.
func Remaining(q QuotaSource, p *Provider, now time.Time) (daily int64, hasDaily bool, monthly int64, hasMonthly bool) {
	if q == nil || p == nil {
		return 0, false, 0, false
	}
	u, _ := q.Snapshot(p.Name, now)
	if p.Quota.Daily > 0 {
		hasDaily = true
		daily = max64(p.Quota.Daily-u.UsedToday, 0)
	}
	if p.Quota.Monthly > 0 {
		hasMonthly = true
		monthly = max64(p.Quota.Monthly-u.UsedMonth, 0)
	}
	return daily, hasDaily, monthly, hasMonthly
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
