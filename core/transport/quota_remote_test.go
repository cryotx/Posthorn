package transport

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/craigmccaskill/posthorn/netguard"
)

// fakeFetcher returns a scripted remote reading.
type fakeFetcher struct {
	usage RemoteUsage
	err   error
	calls int
}

func (f *fakeFetcher) Name() string { return "fake" }

func (f *fakeFetcher) FetchUsage(context.Context, string) (RemoteUsage, error) {
	f.calls++
	return f.usage, f.err
}

func remotePool(t *testing.T, daily int64) (*Pool, *Provider) {
	t.Helper()
	p := &Provider{
		Name: "resend", Weight: 1, Transport: succeeding("x"),
		Breaker: NewBreaker(BreakerConfig{Disabled: true}),
		Quota:   Quota{Daily: daily},
	}
	pool, err := NewPool([]*Provider{p})
	if err != nil {
		t.Fatal(err)
	}
	return pool, p
}

// The audit's conclusion in one test: the v2.2 QuotaSource interface
// already fits a remote source, so RemoteQuota satisfies it by
// delegation without the interface changing.
func TestRemoteQuota_SatisfiesTheExistingInterfaces(t *testing.T) {
	local := NewLocalQuota(nil, nil)
	rq := NewRemoteQuota(local, nil)
	var _ QuotaSource = rq
	var _ Reserver = rq

	now := at(2026, 9, 7, 12)
	p := quotaProvider("resend", 2, 0)
	if v := rq.Check(p, now); !v.Allowed {
		t.Error("Check delegated wrongly")
	}
	commit, ok, _ := rq.Reserve(p, now)
	if !ok {
		t.Fatal("Reserve delegated wrongly")
	}
	commit(true)
	if u, _ := rq.Snapshot("resend", now); u.UsedToday != 1 {
		t.Errorf("Snapshot = %+v", u)
	}
}

// Reconciliation may only RAISE the local count. Lowering on a lagging
// read would hand back allowance that is already spent.
func TestRemoteQuota_ReconcileRaisesNeverLowers(t *testing.T) {
	now := at(2026, 9, 7, 12)
	local := NewLocalQuota(nil, nil)
	pool, p := remotePool(t, 100)
	for i := 0; i < 10; i++ {
		local.Record(p, now)
	}

	fetcher := &fakeFetcher{usage: RemoteUsage{UsedToday: 40, HasDaily: true}}
	rq := NewRemoteQuota(local, map[string]RemoteUsageFetcher{"resend": fetcher})
	rq.Reconcile(context.Background(), pool, now)

	if u, _ := local.Snapshot("resend", now); u.UsedToday != 40 {
		t.Errorf("UsedToday = %d, want 40 — the provider says more is spent", u.UsedToday)
	}

	// A lagging read reporting LESS must be ignored.
	fetcher.usage = RemoteUsage{UsedToday: 5, HasDaily: true}
	rq.Reconcile(context.Background(), pool, now)
	if u, _ := local.Snapshot("resend", now); u.UsedToday != 40 {
		t.Errorf("UsedToday = %d, want 40 — a stale lower reading must not refund allowance", u.UsedToday)
	}
}

func TestRemoteQuota_ReconcileCanExhaustAProvider(t *testing.T) {
	now := at(2026, 9, 7, 12)
	local := NewLocalQuota(nil, nil)
	pool, p := remotePool(t, 100)

	if !local.Check(p, now).Allowed {
		t.Fatal("provider should start with budget")
	}
	rq := NewRemoteQuota(local, map[string]RemoteUsageFetcher{
		"resend": &fakeFetcher{usage: RemoteUsage{UsedToday: 100, HasDaily: true}},
	})
	rq.Reconcile(context.Background(), pool, now)

	if local.Check(p, now).Allowed {
		t.Error("provider still has budget after the provider reported the allowance spent")
	}
}

// A failing provider API is a degradation, never an outage: local
// counting continues and mail keeps flowing.
func TestRemoteQuota_FetchFailureDegrades(t *testing.T) {
	now := at(2026, 9, 7, 12)
	local := NewLocalQuota(nil, nil)
	pool, p := remotePool(t, 10)

	var reported []string
	rq := NewRemoteQuota(local, map[string]RemoteUsageFetcher{
		"resend": &fakeFetcher{err: fmt.Errorf("429 rate limited")},
	})
	rq.OnError = func(provider string, err error) { reported = append(reported, provider) }
	rq.Reconcile(context.Background(), pool, now)

	if len(reported) != 1 || reported[0] != "resend" {
		t.Errorf("OnError = %v, want [resend]", reported)
	}
	if !local.Check(p, now).Allowed {
		t.Error("a failed remote read blocked sending")
	}
}

// Unlimited providers are never polled: there is no budget to reconcile.
func TestRemoteQuota_SkipsUnlimitedProviders(t *testing.T) {
	local := NewLocalQuota(nil, nil)
	pool, _ := remotePool(t, 0)
	fetcher := &fakeFetcher{usage: RemoteUsage{UsedToday: 5, HasDaily: true}}
	rq := NewRemoteQuota(local, map[string]RemoteUsageFetcher{"resend": fetcher})
	rq.Reconcile(context.Background(), pool, at(2026, 9, 7, 12))

	if fetcher.calls != 0 {
		t.Errorf("fetcher called %d times for an unlimited provider", fetcher.calls)
	}
}

// Reconciliation must not persist: those sends were either already
// recorded by Posthorn or were made by something else entirely, and
// writing them would double-count on the next restore.
func TestRemoteQuota_ReconcileDoesNotPersist(t *testing.T) {
	now := at(2026, 9, 7, 12)
	store := newMemStore()
	local := NewLocalQuota(store, nil)
	pool, _ := remotePool(t, 100)

	rq := NewRemoteQuota(local, map[string]RemoteUsageFetcher{
		"resend": &fakeFetcher{usage: RemoteUsage{UsedToday: 40, HasDaily: true}},
	})
	rq.Reconcile(context.Background(), pool, now)

	if got := store.get("resend", "day", "2026-09-07"); got != 0 {
		t.Errorf("persisted %d from reconciliation, want 0", got)
	}
	if u, _ := local.Snapshot("resend", now); u.UsedToday != 40 {
		t.Errorf("in-memory count = %d, want 40", u.UsedToday)
	}
}

// --- reference HTTP fetcher --------------------------------------------

func TestHTTPUsageFetcher(t *testing.T) {
	var gotPath, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewEncoder(w).Encode(map[string]int64{"used_today": 12, "used_month": 340})
	}))
	defer srv.Close()

	f := &HTTPUsageFetcher{
		URL:    srv.URL + "/usage/{provider}",
		Header: map[string]string{"Authorization": "Bearer secret"},
		// The test server is on loopback, which the strict default
		// refuses on purpose (NFR45) — so the test states the same
		// opt-in an operator would set for a local usage endpoint.
		Policy: netguard.Policy{AllowPrivate: true},
	}
	got, err := f.FetchUsage(context.Background(), "resend")
	if err != nil {
		t.Fatalf("FetchUsage: %v", err)
	}
	if gotPath != "/usage/resend" {
		t.Errorf("path = %q, want the provider substituted", gotPath)
	}
	if gotAuth != "Bearer secret" {
		t.Errorf("auth header = %q", gotAuth)
	}
	if got.UsedToday != 12 || !got.HasDaily || got.UsedMonth != 340 || !got.HasMonthly {
		t.Errorf("usage = %+v", got)
	}
}

// A provider reporting only one window must not have the other
// invented for it.
func TestHTTPUsageFetcher_PartialReport(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"used_month": 900}`))
	}))
	defer srv.Close()

	got, err := (&HTTPUsageFetcher{URL: srv.URL, Policy: netguard.Policy{AllowPrivate: true}}).
		FetchUsage(context.Background(), "x")
	if err != nil {
		t.Fatal(err)
	}
	if got.HasDaily {
		t.Error("a daily window was invented from a month-only report")
	}
	if !got.HasMonthly || got.UsedMonth != 900 {
		t.Errorf("usage = %+v", got)
	}
}

func TestHTTPUsageFetcher_ErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	f := &HTTPUsageFetcher{URL: srv.URL, Policy: netguard.Policy{AllowPrivate: true}}
	if _, err := f.FetchUsage(context.Background(), "x"); err == nil {
		t.Error("a 429 from the usage endpoint was not reported")
	}
}

// TestHTTPUsageFetcher_SSRFGuarded: the usage endpoint is a URL from a
// config file, so it gets the same treatment as a webhook target. With
// the strict default it cannot reach loopback at all — and a fetcher
// built without thinking about the policy gets that default.
func TestHTTPUsageFetcher_SSRFGuarded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"used_today": 1}`))
	}))
	defer srv.Close()

	_, err := (&HTTPUsageFetcher{URL: srv.URL}).FetchUsage(context.Background(), "x")
	if err == nil {
		t.Fatal("strict fetcher reached a loopback usage endpoint")
	}
	if !strings.Contains(err.Error(), "blocked by SSRF policy") {
		t.Errorf("error = %v, want the SSRF policy message", err)
	}
}

func TestRemoteQuota_RunReconcilerStops(t *testing.T) {
	local := NewLocalQuota(nil, nil)
	pool, _ := remotePool(t, 10)
	rq := NewRemoteQuota(local, map[string]RemoteUsageFetcher{
		"resend": &fakeFetcher{usage: RemoteUsage{UsedToday: 1, HasDaily: true}},
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { rq.RunReconciler(ctx, pool, 10*time.Millisecond); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("RunReconciler did not stop on context cancel")
	}
}
