package transport

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/craigmccaskill/posthorn/netguard"
)

// Remote quota (FR119, ADR-37).
//
// The v2.2 QuotaSource interface was audited against this and needed no
// change: Check / Record / Snapshot are exactly what a remote source
// can answer, and Reserve stayed optional precisely so a source that
// cannot promise atomicity is not forced to pretend.
//
// The relationship between the two sources is the whole design, and it
// is deliberately asymmetric:
//
//	LOCAL usage is authoritative for blocking. Posthorn counts its own
//	successful sends, synchronously, and that count is exact.
//
//	REMOTE usage is advisory — a reconciliation signal, never a gate.
//	Provider usage APIs lag, round, count differently (some count
//	recipients, some messages), and include sends Posthorn never made
//	(a colleague using the same account from a script). They are useful
//	for exactly one thing: telling Posthorn that MORE of the allowance
//	is gone than it thought.
//
// So reconciliation only ever raises the local counter, never lowers
// it. Lowering on a lagging read would hand back allowance that is
// already spent and blow the budget — the failure mode quota exists to
// prevent. Raising on a stale read costs at most a premature failover
// to the next provider, which is the cheap direction.

// RemoteUsage is a provider's own report of what has been spent.
type RemoteUsage struct {
	UsedToday  int64
	HasDaily   bool
	UsedMonth  int64
	HasMonthly bool
}

// RemoteUsageFetcher reads one provider's usage from its own API.
//
// Deliberately this small: a vendor adapter's whole job is to turn one
// HTTP response into these four fields. Nothing about rate limits,
// pagination or auth leaks into the quota layer.
type RemoteUsageFetcher interface {
	// Name identifies the fetcher for logs.
	Name() string

	// FetchUsage returns the provider's reported usage for the current
	// day and month. A provider that reports only one window sets only
	// that window's Has flag.
	FetchUsage(ctx context.Context, provider string) (RemoteUsage, error)
}

// RemoteQuota wraps a LocalQuota with periodic reconciliation against
// provider-reported usage.
//
// Every QuotaSource and Reserver call delegates straight to the local
// tracker: the send path is unchanged, and a provider API being slow or
// down cannot delay a single message. Reconciliation happens out of
// band, on its own goroutine.
type RemoteQuota struct {
	local    *LocalQuota
	fetchers map[string]RemoteUsageFetcher

	// OnReconcile observes each reconciliation for logs/metrics:
	// provider, the local count before, and the remote count applied.
	OnReconcile func(provider string, localUsed, remoteUsed int64, window QuotaWindow)

	// OnError observes fetch failures. A failing provider API is a
	// degradation, never an outage: local counting continues.
	OnError func(provider string, err error)

	mu sync.Mutex
}

// NewRemoteQuota wraps local with reconciliation. fetchers maps a pool
// provider name to the fetcher that can read its usage; providers with
// no fetcher are simply never reconciled.
func NewRemoteQuota(local *LocalQuota, fetchers map[string]RemoteUsageFetcher) *RemoteQuota {
	return &RemoteQuota{local: local, fetchers: fetchers}
}

// Check delegates to the local tracker.
func (q *RemoteQuota) Check(p *Provider, now time.Time) QuotaVerdict { return q.local.Check(p, now) }

// Record delegates to the local tracker.
func (q *RemoteQuota) Record(p *Provider, now time.Time) { q.local.Record(p, now) }

// Snapshot delegates to the local tracker.
func (q *RemoteQuota) Snapshot(name string, now time.Time) (Usage, bool) {
	return q.local.Snapshot(name, now)
}

// Reserve delegates to the local tracker, so strict reservation is
// preserved: the authoritative count is still the exact local one.
func (q *RemoteQuota) Reserve(p *Provider, now time.Time) (func(bool), bool, string) {
	return q.local.Reserve(p, now)
}

// Reconcile fetches provider-reported usage and raises the local
// counters where the provider says more has been spent.
//
// Never lowers. See the package comment above for why that asymmetry is
// the safe direction.
func (q *RemoteQuota) Reconcile(ctx context.Context, pool *Pool, now time.Time) {
	if pool == nil || len(q.fetchers) == 0 {
		return
	}
	for _, p := range pool.All() {
		fetcher, ok := q.fetchers[p.Name]
		if !ok || p.Quota.Unlimited() {
			continue
		}
		remote, err := fetcher.FetchUsage(ctx, p.Name)
		if err != nil {
			if q.OnError != nil {
				q.OnError(p.Name, err)
			}
			continue
		}
		q.apply(p.Name, remote, now)
	}
}

// apply raises the local counters to the remote floor.
func (q *RemoteQuota) apply(name string, remote RemoteUsage, now time.Time) {
	q.mu.Lock()
	defer q.mu.Unlock()

	current, _ := q.local.Snapshot(name, now)
	if remote.HasDaily && remote.UsedToday > current.UsedToday {
		q.local.raise(name, now, remote.UsedToday-current.UsedToday, 0)
		if q.OnReconcile != nil {
			q.OnReconcile(name, current.UsedToday, remote.UsedToday, WindowDaily)
		}
	}
	if remote.HasMonthly && remote.UsedMonth > current.UsedMonth {
		q.local.raise(name, now, 0, remote.UsedMonth-current.UsedMonth)
		if q.OnReconcile != nil {
			q.OnReconcile(name, current.UsedMonth, remote.UsedMonth, WindowMonthly)
		}
	}
}

// RunReconciler reconciles on an interval until ctx is cancelled.
// Interval zero means five minutes — provider usage APIs are rate
// limited and their numbers lag anyway, so polling harder buys nothing.
func (q *RemoteQuota) RunReconciler(ctx context.Context, pool *Pool, interval time.Duration) {
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	q.Reconcile(ctx, pool, time.Now())
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			q.Reconcile(ctx, pool, now)
		}
	}
}

// raise adds to a provider's counters WITHOUT persisting, because a
// reconciliation is not a send: the sends it represents were either
// already persisted by Posthorn or were made by something else
// entirely, and writing them to the usage table would double-count on
// the next restore.
func (q *LocalQuota) raise(name string, now time.Time, day, month int64) {
	q.mu.Lock()
	defer q.mu.Unlock()
	u := q.currentLocked(name, now)
	u.UsedToday += day
	u.UsedMonth += month
}

// --- reference fetcher -------------------------------------------------

// HTTPUsageFetcher reads usage from an HTTP endpoint returning JSON:
//
//	{"used_today": 42, "used_month": 900}
//
// This is the REFERENCE implementation, and it is deliberately generic
// rather than named after a vendor. Posthorn ships no Resend / SES /
// Mailgun / Brevo usage adapter yet: each needs its live API read
// carefully (they differ on whether a "send" is a message or a
// recipient, on window boundaries, and on auth), and an adapter written
// from memory would report confident, wrong numbers into a budget.
//
// An operator can point this at a small endpoint of their own today,
// and a vendor adapter is a new type implementing the same interface.
type HTTPUsageFetcher struct {
	// URL is the usage endpoint. A "{provider}" placeholder is
	// substituted with the pool provider name.
	URL string

	// Header is an optional static auth header (name → value). Values
	// are credentials and are never logged (NFR3).
	Header map[string]string

	// Client is injectable for tests. Nil means a 5s-timeout client
	// guarded by Policy.
	Client *http.Client

	// Policy is the SSRF policy for the usage endpoint (NFR45). The
	// zero value is the strict default, so a fetcher constructed
	// without thinking about it cannot be walked into the metadata
	// service by a URL in a config file.
	Policy netguard.Policy
}

// Name implements RemoteUsageFetcher.
func (f *HTTPUsageFetcher) Name() string { return "http" }

// FetchUsage implements RemoteUsageFetcher.
func (f *HTTPUsageFetcher) FetchUsage(ctx context.Context, provider string) (RemoteUsage, error) {
	client := f.Client
	if client == nil {
		client = f.Policy.Client(5 * time.Second)
	}
	url := f.URL
	for i := 0; i+len("{provider}") <= len(url); i++ {
		if url[i:i+len("{provider}")] == "{provider}" {
			url = url[:i] + provider + url[i+len("{provider}"):]
			break
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return RemoteUsage{}, err
	}
	for k, v := range f.Header {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return RemoteUsage{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return RemoteUsage{}, fmt.Errorf("usage endpoint returned %d", resp.StatusCode)
	}
	var payload struct {
		UsedToday *int64 `json:"used_today"`
		UsedMonth *int64 `json:"used_month"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return RemoteUsage{}, fmt.Errorf("decode usage payload: %w", err)
	}
	out := RemoteUsage{}
	if payload.UsedToday != nil {
		out.UsedToday, out.HasDaily = *payload.UsedToday, true
	}
	if payload.UsedMonth != nil {
		out.UsedMonth, out.HasMonthly = *payload.UsedMonth, true
	}
	return out, nil
}

// Compile-time checks.
var (
	_ QuotaSource        = (*RemoteQuota)(nil)
	_ Reserver           = (*RemoteQuota)(nil)
	_ RemoteUsageFetcher = (*HTTPUsageFetcher)(nil)
)
