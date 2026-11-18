package transport

import (
	"strings"
	"testing"
	"time"
)

func TestNewPool_BuildsAndDefaults(t *testing.T) {
	p, err := NewPool([]*Provider{
		{Name: "a", Type: "mock", Transport: succeeding("a")},
		{Name: "b", Type: "mock", Transport: succeeding("b"), Weight: 5},
	})
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	if p.Len() != 2 {
		t.Errorf("Len = %d, want 2", p.Len())
	}
	a, ok := p.Get("a")
	if !ok {
		t.Fatal("Get(a) missing")
	}
	if a.Weight != 1 {
		t.Errorf("default Weight = %d, want 1", a.Weight)
	}
	if a.Breaker == nil {
		t.Error("Breaker not defaulted")
	}
	b, _ := p.Get("b")
	if b.Weight != 5 {
		t.Errorf("Weight = %d, want 5 (explicit weights survive)", b.Weight)
	}
}

func TestNewPool_RejectsBadEntries(t *testing.T) {
	for _, tc := range []struct {
		name    string
		entries []*Provider
		want    string
	}{
		{"nil entry", []*Provider{nil}, "nil entry"},
		{"empty name", []*Provider{{Type: "mock", Transport: succeeding("x")}}, "name is required"},
		{"no transport", []*Provider{{Name: "a", Type: "mock"}}, "transport is required"},
		{"duplicate name", []*Provider{
			{Name: "a", Transport: succeeding("1")},
			{Name: "a", Transport: succeeding("2")},
		}, "duplicate name"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewPool(tc.entries)
			if err == nil {
				t.Fatalf("NewPool = nil error, want %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

func TestPool_ResolvePreservesOrder(t *testing.T) {
	p, err := NewPool([]*Provider{
		{Name: "a", Transport: succeeding("a")},
		{Name: "b", Transport: succeeding("b")},
		{Name: "c", Transport: succeeding("c")},
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := p.Resolve([]string{"c", "a"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(got) != 2 || got[0].Name != "c" || got[1].Name != "a" {
		t.Errorf("Resolve = %v, want [c a] — priority order is the config's order", got)
	}
}

// The unknown-provider message is what an operator sees for a typo, so
// it must list what is actually defined.
func TestPool_ResolveUnknownListsDefined(t *testing.T) {
	p, _ := NewPool([]*Provider{
		{Name: "postmark-prod", Transport: succeeding("a")},
		{Name: "ses-backup", Transport: succeeding("b")},
	})
	_, err := p.Resolve([]string{"postmark-prd"})
	if err == nil {
		t.Fatal("Resolve = nil error for an unknown name")
	}
	for _, want := range []string{"postmark-prd", "postmark-prod", "ses-backup"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err, want)
		}
	}
}

func TestPool_NamesAndAllAreSorted(t *testing.T) {
	p, _ := NewPool([]*Provider{
		{Name: "zeta", Transport: succeeding("z")},
		{Name: "alpha", Transport: succeeding("a")},
	})
	names := p.Names()
	if len(names) != 2 || names[0] != "alpha" || names[1] != "zeta" {
		t.Errorf("Names = %v, want [alpha zeta]", names)
	}
	all := p.All()
	if len(all) != 2 || all[0].Name != "alpha" {
		t.Errorf("All out of order: %v", all)
	}
}

func TestPool_StatesReportsEveryProvider(t *testing.T) {
	tripped := NewBreaker(BreakerConfig{FailureThreshold: 1, Cooldown: time.Hour})
	tripped.Failure(&TransportError{Class: ErrTransient})

	p, _ := NewPool([]*Provider{
		{Name: "healthy", Transport: succeeding("h")},
		{Name: "down", Transport: succeeding("d"), Breaker: tripped},
	})

	states := p.States()
	if len(states) != 2 {
		t.Fatalf("States = %v, want 2 entries", states)
	}
	if states["healthy"] != CircuitClosed {
		t.Errorf("healthy = %v, want closed", states["healthy"])
	}
	if states["down"] != CircuitOpen {
		t.Errorf("down = %v, want open", states["down"])
	}
}

// A nil *Pool is the "no [[providers]] configured" case and every
// accessor must tolerate it — buildRoutedTransport calls Len on it
// before knowing whether a pool exists.
func TestPool_NilIsSafe(t *testing.T) {
	var p *Pool
	if p.Len() != 0 {
		t.Error("Len on a nil pool")
	}
	if p.Names() != nil {
		t.Error("Names on a nil pool")
	}
	if p.All() != nil {
		t.Error("All on a nil pool")
	}
	if p.States() != nil {
		t.Error("States on a nil pool")
	}
	if _, ok := p.Get("x"); ok {
		t.Error("Get on a nil pool returned ok")
	}
}
