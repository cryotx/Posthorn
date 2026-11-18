package gateway

import (
	"fmt"
	"sync"
	"time"

	"github.com/craigmccaskill/posthorn/project"
	"github.com/craigmccaskill/posthorn/storage"
	"github.com/craigmccaskill/posthorn/transport"
)

// ProjectUsage records per-project send counters (FR115).
//
// Deliberately a plain counter, not a budget. It is period-keyed the
// same way provider usage is — a new day is a new key, so nothing has
// to run at midnight — but it enforces nothing and shares no code with
// the provider quota. A project budget bounds one tenant's own volume;
// a provider quota bounds an upstream account's allowance. Wiring them
// together would make "who ran out?" unanswerable (ADR-35).
//
// Writes are buffered and flushed on a timer so a busy gateway does not
// pay a transaction per message for a counter nobody reads in real
// time. A lost buffer costs at most one flush interval of counting,
// which is the same NFR27 trade the rest of the storage layer makes.
type ProjectUsage struct {
	gate *storage.Gate

	// now is injectable for tests. Nil means time.Now.
	now func() time.Time

	mu      sync.Mutex
	pending map[string]*usageDelta

	// counts is the authoritative in-memory view of each project's
	// spent budget for the CURRENT periods, restored from storage at
	// startup. Separate from pending, which is only the not-yet-flushed
	// delta: a flush empties pending without touching what has been
	// spent.
	counts map[string]*projectCount
}

type usageDelta struct {
	day, month       string
	success, failure int64
}

// projectCount is one project's spent budget plus its in-flight
// reservations, period-keyed so rollover needs no scheduled job — the
// same mechanism provider quota uses (ADR-30), deliberately a separate
// implementation because the two are separate dimensions (ADR-35).
type projectCount struct {
	day, month           string
	usedToday, usedMonth int64
	reserved             int64
}

// NewProjectUsage builds a recorder. gate may be nil, in which case
// counting is a no-op — there is nowhere durable to put it, and an
// in-memory counter nobody can read has no purpose.
func NewProjectUsage(gate *storage.Gate) *ProjectUsage {
	return &ProjectUsage{
		gate:    gate,
		pending: map[string]*usageDelta{},
		counts:  map[string]*projectCount{},
	}
}

// Restore loads each project's spent budget for the periods current at
// now. Call once at startup, before serving: a project whose daily
// allowance is already spent must not get a fresh one because the
// process restarted.
func (u *ProjectUsage) Restore(projects []*project.Project, now time.Time) error {
	if u == nil || u.gate == nil {
		return nil
	}
	day, month := transport.DayKey(now), transport.MonthKey(now)
	u.mu.Lock()
	defer u.mu.Unlock()
	for _, p := range projects {
		if p.DailyLimit <= 0 && p.MonthlyLimit <= 0 {
			continue
		}
		today, monthTotal, err := u.gate.Store().ProjectUsageAt(p.Name, day, month)
		if err != nil {
			return fmt.Errorf("restore project usage for %q: %w", p.Name, err)
		}
		u.counts[p.Name] = &projectCount{
			day: day, month: month,
			usedToday: today.Success, usedMonth: monthTotal.Success,
		}
	}
	return nil
}

// currentLocked returns a project's counters, rolling over any period
// whose key no longer matches the clock. Caller holds u.mu.
func (u *ProjectUsage) currentLocked(name string, now time.Time) *projectCount {
	day, month := transport.DayKey(now), transport.MonthKey(now)
	c, ok := u.counts[name]
	if !ok {
		c = &projectCount{day: day, month: month}
		u.counts[name] = c
		return c
	}
	if c.day != day {
		c.day, c.usedToday = day, 0
	}
	if c.month != month {
		c.month, c.usedMonth = month, 0
	}
	return c
}

// Reserve takes one unit of a project's own budget (FR126).
//
// Strict, by the same reasoning as the provider quota reservation
// (ADR-36): the check and the take happen under one lock, so with a
// limit of 100 and 99 spent, a hundred simultaneous callers see exactly
// one grant. The returned commit MUST be called: commit(true) keeps the
// unit and counts a success, commit(false) hands it back and counts a
// failure — a send that never left must not spend a tenant's allowance.
//
// A project with no configured limit is always allowed and costs
// nothing beyond the usage counter it already had.
func (u *ProjectUsage) Reserve(p *project.Project, now time.Time) (commit func(bool), allowed bool, window string) {
	noop := func(success bool) {
		if u != nil && p != nil {
			u.record(p.Name, success, now)
		}
	}
	if u == nil || p == nil {
		return func(bool) {}, true, ""
	}
	if p.DailyLimit <= 0 && p.MonthlyLimit <= 0 {
		return noop, true, ""
	}

	u.mu.Lock()
	c := u.currentLocked(p.Name, now)
	if p.DailyLimit > 0 && c.usedToday+c.reserved >= p.DailyLimit {
		u.mu.Unlock()
		return func(bool) {}, false, WindowDaily
	}
	if p.MonthlyLimit > 0 && c.usedMonth+c.reserved >= p.MonthlyLimit {
		u.mu.Unlock()
		return func(bool) {}, false, WindowMonthly
	}
	c.reserved++
	u.mu.Unlock()

	var once sync.Once
	return func(success bool) {
		once.Do(func() {
			u.mu.Lock()
			c := u.currentLocked(p.Name, now)
			if c.reserved > 0 {
				c.reserved--
			}
			if success {
				c.usedToday++
				c.usedMonth++
			}
			u.mu.Unlock()
			u.record(p.Name, success, now)
			// A LIMITED project's counter is not just a statistic, it
			// is the budget. Buffering it means a crash refunds every
			// send since the last flush — a silent quota leak that a
			// restart converts into extra allowance. So a limited
			// project writes through immediately; an unlimited one
			// keeps the buffer, because nothing enforces on it.
			u.Flush()
		})
	}, true, ""
}

// Window names for project quota. Deliberately the same strings the
// provider quota uses, because they mean the same calendar periods —
// but they label a different metric family, so an operator can always
// tell which budget ran out.
const (
	WindowDaily   = "daily"
	WindowMonthly = "monthly"
)

// Remaining reports a project's remaining budget per window, and
// whether each window is bounded at all.
func (u *ProjectUsage) Remaining(p *project.Project, now time.Time) (daily int64, hasDaily bool, monthly int64, hasMonthly bool) {
	if u == nil || p == nil {
		return 0, false, 0, false
	}
	u.mu.Lock()
	c := u.currentLocked(p.Name, now)
	usedToday, usedMonth := c.usedToday, c.usedMonth
	u.mu.Unlock()

	if p.DailyLimit > 0 {
		hasDaily = true
		if daily = p.DailyLimit - usedToday; daily < 0 {
			daily = 0
		}
	}
	if p.MonthlyLimit > 0 {
		hasMonthly = true
		if monthly = p.MonthlyLimit - usedMonth; monthly < 0 {
			monthly = 0
		}
	}
	return daily, hasDaily, monthly, hasMonthly
}

func (u *ProjectUsage) clock() time.Time {
	if u == nil {
		return time.Now()
	}
	if u.now != nil {
		return u.now()
	}
	return time.Now()
}

// Record buffers one outcome for a project. Used by paths that do not
// hold a reservation (a suppressed send, a sender rejection): those
// still happened and belong in the usage counters, but they never took
// budget so there is nothing to release.
func (u *ProjectUsage) Record(name string, success bool) {
	u.record(name, success, u.clock())
}

func (u *ProjectUsage) record(name string, success bool, now time.Time) {
	if u == nil || u.gate == nil || name == "" {
		return
	}
	day, month := transport.DayKey(now), transport.MonthKey(now)

	u.mu.Lock()
	defer u.mu.Unlock()
	d, ok := u.pending[name]
	if !ok || d.day != day || d.month != month {
		// A period boundary crossed while a delta was buffered: flush
		// what we have under its own keys before starting the new one,
		// or the counts would land in the wrong day.
		if ok {
			u.flushOneLocked(name, d)
		}
		d = &usageDelta{day: day, month: month}
		u.pending[name] = d
	}
	if success {
		d.success++
	} else {
		d.failure++
	}
}

// Flush writes every buffered delta. Called on a timer and at shutdown.
func (u *ProjectUsage) Flush() {
	if u == nil || u.gate == nil {
		return
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	for name, d := range u.pending {
		u.flushOneLocked(name, d)
		delete(u.pending, name)
	}
}

// flushOneLocked persists one delta. Caller holds u.mu.
func (u *ProjectUsage) flushOneLocked(name string, d *usageDelta) {
	if d.success == 0 && d.failure == 0 {
		return
	}
	if !u.gate.Healthy() {
		return
	}
	if err := u.gate.Store().AddProjectUsage(name, d.day, d.month, d.success, d.failure); err != nil {
		u.gate.ReportError(err)
		return
	}
	d.success, d.failure = 0, 0
}

// Run flushes on an interval until ctx-like stop channel closes.
// Interval zero means the 10s default.
func (u *ProjectUsage) Run(stop <-chan struct{}, interval time.Duration) {
	if u == nil || u.gate == nil {
		return
	}
	if interval <= 0 {
		interval = 10 * time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-stop:
			u.Flush()
			return
		case <-t.C:
			u.Flush()
		}
	}
}
