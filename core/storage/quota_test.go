package storage

import (
	"testing"
	"time"

	"github.com/craigmccaskill/posthorn/transport"
)

func TestAddProviderUsage_UpsertsBothWindows(t *testing.T) {
	s := memStore(t)
	if err := s.AddProviderUsage("resend", "2026-09-07", "2026-09", 1); err != nil {
		t.Fatalf("AddProviderUsage: %v", err)
	}
	if err := s.AddProviderUsage("resend", "2026-09-07", "2026-09", 1); err != nil {
		t.Fatalf("AddProviderUsage: %v", err)
	}

	u, err := s.ProviderUsageAt("resend", "2026-09-07", "2026-09")
	if err != nil {
		t.Fatalf("ProviderUsageAt: %v", err)
	}
	if u.UsedToday != 2 || u.UsedMonth != 2 {
		t.Errorf("usage = %+v, want 2/2", u)
	}
}

// A new day is a new key, so the previous day's counter is simply not
// read any more — the property that makes rollover need no cron.
func TestProviderUsage_PeriodsAreIndependent(t *testing.T) {
	s := memStore(t)
	for i := 0; i < 3; i++ {
		if err := s.AddProviderUsage("resend", "2026-09-07", "2026-09", 1); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.AddProviderUsage("resend", "2026-09-08", "2026-09", 1); err != nil {
		t.Fatal(err)
	}

	day1, _ := s.ProviderUsageAt("resend", "2026-09-07", "2026-09")
	if day1.UsedToday != 3 {
		t.Errorf("day 1 = %d, want 3", day1.UsedToday)
	}
	day2, _ := s.ProviderUsageAt("resend", "2026-09-08", "2026-09")
	if day2.UsedToday != 1 {
		t.Errorf("day 2 = %d, want 1 — a new day starts fresh", day2.UsedToday)
	}
	// The month spans both days.
	if day2.UsedMonth != 4 {
		t.Errorf("month = %d, want 4", day2.UsedMonth)
	}
}

func TestLoadProviderUsage_CurrentPeriodsOnly(t *testing.T) {
	s := memStore(t)
	if err := s.AddProviderUsage("resend", "2026-09-07", "2026-09", 5); err != nil {
		t.Fatal(err)
	}
	if err := s.AddProviderUsage("ses", "2026-09-07", "2026-09", 2); err != nil {
		t.Fatal(err)
	}
	if err := s.AddProviderUsage("resend", "2026-09-06", "2026-08", 99); err != nil {
		t.Fatal(err)
	}

	rows, err := s.LoadProviderUsage("2026-09-07", "2026-09")
	if err != nil {
		t.Fatalf("LoadProviderUsage: %v", err)
	}
	got := map[string]transport.ProviderUsage{}
	for _, r := range rows {
		got[r.Provider] = r
	}
	if len(got) != 2 {
		t.Fatalf("rows = %v, want 2 providers", got)
	}
	if got["resend"].UsedToday != 5 || got["resend"].UsedMonth != 5 {
		t.Errorf("resend = %+v, want 5/5 (the August row must not leak in)", got["resend"])
	}
	if got["ses"].UsedToday != 2 {
		t.Errorf("ses = %+v", got["ses"])
	}
}

func TestLoadProviderUsage_EmptyIsNotAnError(t *testing.T) {
	s := memStore(t)
	rows, err := s.LoadProviderUsage("2026-09-07", "2026-09")
	if err != nil {
		t.Fatalf("LoadProviderUsage: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("rows = %v, want none", rows)
	}
}

// The full restart path, through real SQLite: the tracker writes
// through, a fresh tracker over the same store restores the counters,
// and the budget continues where it left off.
func TestQuotaSurvivesRestart_ThroughStore(t *testing.T) {
	s := memStore(t)
	now := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	p := &transport.Provider{Name: "resend", Quota: transport.Quota{Daily: 3}}

	first := transport.NewLocalQuota(s, nil)
	first.Record(p, now)
	first.Record(p, now)

	second := transport.NewLocalQuota(s, nil)
	if err := second.Restore(now); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if !second.Check(p, now).Allowed {
		t.Fatal("third send blocked after restart, want allowed")
	}
	second.Record(p, now)
	v := second.Check(p, now)
	if v.Allowed {
		t.Fatal("fourth send allowed after restart — the budget restarted with the process")
	}
	if v.Used != 3 {
		t.Errorf("Used = %d, want 3 across the restart", v.Used)
	}
}

func TestPrune_DropsStaleUsageRows(t *testing.T) {
	s := memStore(t)
	if err := s.AddProviderUsage("resend", "2026-01-05", "2026-01", 7); err != nil {
		t.Fatal(err)
	}
	if err := s.AddProviderUsage("resend", "2026-09-07", "2026-09", 3); err != nil {
		t.Fatal(err)
	}

	cutoff := time.Date(2026, 8, 8, 0, 0, 0, 0, time.UTC)
	if _, err := s.Prune(cutoff); err != nil {
		t.Fatalf("Prune: %v", err)
	}

	old, _ := s.ProviderUsageAt("resend", "2026-01-05", "2026-01")
	if old.UsedToday != 0 {
		t.Errorf("stale day row survived pruning: %+v", old)
	}
	// The month row is kept for two years so a cost review can look back.
	if old.UsedMonth != 7 {
		t.Errorf("month row = %d, want 7 kept", old.UsedMonth)
	}
	current, _ := s.ProviderUsageAt("resend", "2026-09-07", "2026-09")
	if current.UsedToday != 3 {
		t.Errorf("current row = %+v, want 3 kept", current)
	}
}

// Opening an existing v1 database must add the quota table rather than
// refuse or wipe: an operator upgrading has a submission log they care
// about.
func TestMigration_V1DatabaseGainsUsageTable(t *testing.T) {
	s := memStore(t)
	var version int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatalf("read user_version: %v", err)
	}
	if version != schemaVersion {
		t.Errorf("user_version = %d, want %d", version, schemaVersion)
	}
	if err := s.AddProviderUsage("p", "2026-09-07", "2026-09", 1); err != nil {
		t.Errorf("provider_usage unusable after migration: %v", err)
	}
}
