package storage

import (
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/craigmccaskill/posthorn/transport"
)

var idemNow = time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)

const idemTTL = 24 * time.Hour

// --- atomic reservation ------------------------------------------------

func TestReserveIdempotent_FirstCallerWins(t *testing.T) {
	s := memStore(t)
	rec, err := s.ReserveIdempotent("k1", "project:novel", "fp1", "msg-1", idemNow, idemTTL)
	if err != nil {
		t.Fatalf("ReserveIdempotent: %v", err)
	}
	if rec.State != IdemReserved {
		t.Fatalf("state = %v, want reserved", rec.State)
	}

	// A second caller with the same payload sees the reservation.
	rec, err = s.ReserveIdempotent("k1", "project:novel", "fp1", "msg-1", idemNow, idemTTL)
	if err != nil {
		t.Fatal(err)
	}
	if rec.State != IdemInFlight {
		t.Errorf("state = %v, want in-flight", rec.State)
	}
}

func TestReserveIdempotent_ReplayAfterCompletion(t *testing.T) {
	s := memStore(t)
	if _, err := s.ReserveIdempotent("k1", "project:novel", "fp1", "msg-1", idemNow, idemTTL); err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"status":"ok"}`)
	if err := s.CompleteIdempotent("k1", "project:novel", "msg-1", 200, "application/json", body, idemNow, idemTTL); err != nil {
		t.Fatal(err)
	}

	rec, err := s.ReserveIdempotent("k1", "project:novel", "fp1", "msg-1", idemNow, idemTTL)
	if err != nil {
		t.Fatal(err)
	}
	if rec.State != IdemReplay {
		t.Fatalf("state = %v, want replay", rec.State)
	}
	if rec.Status != 200 || string(rec.Body) != string(body) || rec.MessageID != "msg-1" {
		t.Errorf("replayed record = %+v", rec)
	}
}

func TestReserveIdempotent_PayloadConflict(t *testing.T) {
	s := memStore(t)
	if _, err := s.ReserveIdempotent("k1", "project:novel", "fp1", "msg-1", idemNow, idemTTL); err != nil {
		t.Fatal(err)
	}
	if err := s.CompleteIdempotent("k1", "project:novel", "msg-1", 200, "application/json", []byte("{}"), idemNow, idemTTL); err != nil {
		t.Fatal(err)
	}

	rec, err := s.ReserveIdempotent("k1", "project:novel", "DIFFERENT", "msg-1", idemNow, idemTTL)
	if err != nil {
		t.Fatal(err)
	}
	if rec.State != IdemConflict {
		t.Errorf("state = %v, want conflict", rec.State)
	}
}

// Two projects generating the same UUID must not collide.
func TestReserveIdempotent_ScopedPerProject(t *testing.T) {
	s := memStore(t)
	a, err := s.ReserveIdempotent("shared", "project:novel", "fp", "msg-1", idemNow, idemTTL)
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.ReserveIdempotent("shared", "project:account", "fp", "msg-1", idemNow, idemTTL)
	if err != nil {
		t.Fatal(err)
	}
	if a.State != IdemReserved || b.State != IdemReserved {
		t.Errorf("states = %v, %v — the same key in two scopes must both reserve", a.State, b.State)
	}
}

// The property the whole design exists for: exactly one of N concurrent
// reservations may win.
func TestReserveIdempotent_ExactlyOneWinnerUnderConcurrency(t *testing.T) {
	s := memStore(t)
	const callers = 64
	var wg sync.WaitGroup
	results := make(chan IdemState, callers)

	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec, err := s.ReserveIdempotent("burst", "project:novel", "fp", "msg-1", idemNow, idemTTL)
			if err != nil {
				results <- IdemConflict
				return
			}
			results <- rec.State
		}()
	}
	wg.Wait()
	close(results)

	won := 0
	for st := range results {
		if st == IdemReserved {
			won++
		}
	}
	if won != 1 {
		t.Errorf("%d callers reserved the key, want exactly 1", won)
	}
}

func TestReleaseIdempotent_FreesTheKey(t *testing.T) {
	s := memStore(t)
	if _, err := s.ReserveIdempotent("k1", "s", "fp", "msg-1", idemNow, idemTTL); err != nil {
		t.Fatal(err)
	}
	if err := s.ReleaseIdempotent("k1", "s"); err != nil {
		t.Fatal(err)
	}
	rec, err := s.ReserveIdempotent("k1", "s", "fp", "msg-1", idemNow, idemTTL)
	if err != nil {
		t.Fatal(err)
	}
	if rec.State != IdemReserved {
		t.Errorf("state = %v, want reserved after release", rec.State)
	}
}

// A process that died mid-send must not hold a key for its full TTL.
func TestReserveIdempotent_StalePendingIsTakenOver(t *testing.T) {
	s := memStore(t)
	if _, err := s.ReserveIdempotent("k1", "s", "fp", "msg-1", idemNow, idemTTL); err != nil {
		t.Fatal(err)
	}
	// Still owned inside the window.
	rec, _ := s.ReserveIdempotent("k1", "s", "fp", "msg-1", idemNow.Add(PendingTTL/2), idemTTL)
	if rec.State != IdemInFlight {
		t.Fatalf("state = %v, want in-flight inside the window", rec.State)
	}
	// Reclaimable past it.
	rec, _ = s.ReserveIdempotent("k1", "s", "fp", "msg-1", idemNow.Add(PendingTTL+time.Second), idemTTL)
	if rec.State != IdemReserved {
		t.Errorf("state = %v, want reserved after the pending window", rec.State)
	}
}

func TestReserveIdempotent_ExpiredEntryIsReclaimed(t *testing.T) {
	s := memStore(t)
	if _, err := s.ReserveIdempotent("k1", "s", "fp", "msg-1", idemNow, time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := s.CompleteIdempotent("k1", "s", "m", 200, "application/json", []byte("{}"), idemNow, time.Hour); err != nil {
		t.Fatal(err)
	}
	rec, _ := s.ReserveIdempotent("k1", "s", "fp", "msg-1", idemNow.Add(2*time.Hour), idemTTL)
	if rec.State != IdemReserved {
		t.Errorf("state = %v, want reserved after expiry", rec.State)
	}
}

// --- attempts ----------------------------------------------------------

func TestAttempts_RecordsTheFailoverStory(t *testing.T) {
	s := memStore(t)
	t0 := idemNow
	for _, a := range []Attempt{
		{MessageID: "m1", Attempt: 1, Provider: "resend", StartedAt: t0, FinishedAt: t0.Add(time.Second),
			Result: AttemptFailed, ErrorClass: "transient"},
		{MessageID: "m1", Attempt: 2, Provider: "ses", StartedAt: t0.Add(time.Second), FinishedAt: t0.Add(2 * time.Second),
			Result: AttemptSent, ProviderMessageID: "ses-1"},
		{MessageID: "m2", Attempt: 1, Provider: "resend", StartedAt: t0, FinishedAt: t0,
			Result: AttemptSent, ProviderMessageID: "re-1"},
	} {
		if err := s.RecordAttempt(a); err != nil {
			t.Fatalf("RecordAttempt: %v", err)
		}
	}

	got, err := s.AttemptsFor("m1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("attempts = %d, want 2", len(got))
	}
	if got[0].Provider != "resend" || got[0].Result != AttemptFailed || got[0].ErrorClass != "transient" {
		t.Errorf("attempt 1 = %+v", got[0])
	}
	if got[1].Provider != "ses" || got[1].Result != AttemptSent || got[1].ProviderMessageID != "ses-1" {
		t.Errorf("attempt 2 = %+v", got[1])
	}
	// One message's trail must not leak into another's.
	other, _ := s.AttemptsFor("m2")
	if len(other) != 1 {
		t.Errorf("m2 attempts = %d, want 1", len(other))
	}
}

// NFR44: the trail answers "who took this and what happened". It has
// no column that could hold a body, recipient or subject.
func TestAttempts_SchemaHoldsNoMessageContent(t *testing.T) {
	s := memStore(t)
	rows, err := s.db.Query(`SELECT name FROM pragma_table_info('message_attempts')`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	forbidden := []string{"body", "subject", "recipient", "to_addrs", "from"}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		for _, bad := range forbidden {
			if strings.Contains(name, bad) {
				t.Errorf("message_attempts has a content column %q", name)
			}
		}
	}
}

// --- delivery events ---------------------------------------------------

func TestDeliveryEvents_IdempotentByEventID(t *testing.T) {
	s := memStore(t)
	row := DeliveryEventRow{
		EventID: "ev_1", MessageID: "m1", Project: "novel", Provider: "resend",
		ProviderMessageID: "re-1", Type: "message.delivered", OccurredAt: idemNow,
		Metadata: map[string]string{"recipient_domain": "example.com"},
	}
	fresh, err := s.RecordDeliveryEvent(row)
	if err != nil || !fresh {
		t.Fatalf("first insert: fresh=%v err=%v", fresh, err)
	}
	fresh, err = s.RecordDeliveryEvent(row)
	if err != nil {
		t.Fatal(err)
	}
	if fresh {
		t.Error("a provider webhook retry double-counted the event")
	}

	got, err := s.EventsFor("m1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("events = %d, want 1", len(got))
	}
	if got[0].Project != "novel" || got[0].Metadata["recipient_domain"] != "example.com" {
		t.Errorf("event = %+v", got[0])
	}
}

// --- project usage -----------------------------------------------------

func TestProjectUsage_CountsAndPeriods(t *testing.T) {
	s := memStore(t)
	if err := s.AddProjectUsage("novel", "2026-09-07", "2026-09", 3, 1); err != nil {
		t.Fatal(err)
	}
	if err := s.AddProjectUsage("novel", "2026-09-08", "2026-09", 2, 0); err != nil {
		t.Fatal(err)
	}

	day1, month, err := s.ProjectUsageAt("novel", "2026-09-07", "2026-09")
	if err != nil {
		t.Fatal(err)
	}
	if day1.Success != 3 || day1.Failure != 1 {
		t.Errorf("day 1 = %+v", day1)
	}
	if month.Success != 5 || month.Failure != 1 {
		t.Errorf("month = %+v", month)
	}
	day2, _, _ := s.ProjectUsageAt("novel", "2026-09-08", "2026-09")
	if day2.Success != 2 {
		t.Errorf("day 2 = %+v — a new day starts fresh", day2)
	}
}

// Project usage and provider quota are separate dimensions and must
// stay in separate tables with no shared state (ADR-35).
func TestProjectUsage_IndependentFromProviderQuota(t *testing.T) {
	s := memStore(t)
	if err := s.AddProjectUsage("novel", "2026-09-07", "2026-09", 5, 0); err != nil {
		t.Fatal(err)
	}
	if err := s.AddProviderUsage("resend", "2026-09-07", "2026-09", 5); err != nil {
		t.Fatal(err)
	}

	proj, _, _ := s.ProjectUsageAt("novel", "2026-09-07", "2026-09")
	prov, _ := s.ProviderUsageAt("resend", "2026-09-07", "2026-09")
	if proj.Success != 5 || prov.UsedToday != 5 {
		t.Fatalf("project=%+v provider=%+v", proj, prov)
	}
	// Neither name resolves in the other's table.
	empty, _, _ := s.ProjectUsageAt("resend", "2026-09-07", "2026-09")
	if empty.Success != 0 {
		t.Error("a provider name resolved in the project usage table")
	}
	var _ transport.ProviderUsage = prov
}

// --- migration ---------------------------------------------------------

func TestMigrationV3_AddsTenantSurface(t *testing.T) {
	s := memStore(t)
	var version int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != schemaVersion {
		t.Errorf("user_version = %d, want %d", version, schemaVersion)
	}
	for _, probe := range []func() error{
		func() error { _, err := s.ReserveIdempotent("k", "s", "f", "msg-1", idemNow, idemTTL); return err },
		func() error { return s.RecordAttempt(Attempt{MessageID: "m", Provider: "p", Result: AttemptSent}) },
		func() error {
			_, err := s.RecordDeliveryEvent(DeliveryEventRow{EventID: "e", Type: "message.sent", OccurredAt: idemNow})
			return err
		},
		func() error { return s.AddProjectUsage("p", "2026-09-07", "2026-09", 1, 0) },
		func() error {
			return s.RecordSubmission(Submission{ID: "s1", Endpoint: "/x", Status: StatusSending,
				CreatedAt: idemNow, Project: "novel", MessageKind: "verification"})
		},
	} {
		if err := probe(); err != nil {
			t.Errorf("v3 surface unusable: %v", err)
		}
	}

	sub, ok, err := s.GetSubmission("s1")
	if err != nil || !ok {
		t.Fatalf("GetSubmission: ok=%v err=%v", ok, err)
	}
	if sub.Project != "novel" || sub.MessageKind != "verification" {
		t.Errorf("row = %+v, want project ownership recorded", sub)
	}
}

func TestPrune_ClearsTenantTables(t *testing.T) {
	s := memStore(t)
	old := idemNow.Add(-90 * 24 * time.Hour)
	if err := s.RecordAttempt(Attempt{MessageID: "m", Provider: "p", StartedAt: old, FinishedAt: old, Result: AttemptSent}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordDeliveryEvent(DeliveryEventRow{EventID: "e", MessageID: "m", Type: "message.sent", OccurredAt: old}); err != nil {
		t.Fatal(err)
	}

	// Prune is the whole retention pass: an audit trail must not
	// outlive the message it describes.
	if _, err := s.Prune(idemNow.Add(-30 * 24 * time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.AttemptsFor("m"); len(got) != 0 {
		t.Errorf("%d attempt rows survived pruning", len(got))
	}
	if evs, _ := s.EventsFor("m"); len(evs) != 0 {
		t.Errorf("%d event rows survived pruning", len(evs))
	}
}

func TestPruneAttempts_KeepsRecent(t *testing.T) {
	s := memStore(t)
	if err := s.RecordAttempt(Attempt{MessageID: "m", Provider: "p",
		StartedAt: idemNow, FinishedAt: idemNow, Result: AttemptSent}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PruneAttempts(idemNow.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	got, _ := s.AttemptsFor("m")
	if len(got) != 1 {
		t.Errorf("recent attempt was pruned: %v", got)
	}
	_ = fmt.Sprint()
}

// --- crash recovery of orphaned reservations (ADR-45) ------------------

// A process killed mid-send leaves a pending reservation. Recovery must
// settle it with the queued answer, not leave the caller staring at
// 409 in_flight for the whole pending window.
func TestRecoverPendingReservations_SettlesQueuedSubmissions(t *testing.T) {
	s := memStore(t)
	if err := s.RecordSubmission(Submission{
		ID: "m1", Endpoint: "/api/send", Status: StatusSending, CreatedAt: idemNow,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReserveIdempotent("k1", "project:novel", "fp", "m1", idemNow, idemTTL); err != nil {
		t.Fatal(err)
	}
	// The crash: the submission is recovered into the queue.
	if _, err := s.db.Exec(`UPDATE submissions SET status = ? WHERE id = ?`, StatusQueued, "m1"); err != nil {
		t.Fatal(err)
	}

	n, err := s.RecoverPendingReservations(func(id string) (int, string, []byte) {
		return 202, "application/json", []byte(`{"status":"queued","message_id":"` + id + `"}`)
	})
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("settled %d reservations, want 1", n)
	}

	// The caller's retry now replays "queued" instead of stalling.
	rec, err := s.ReserveIdempotent("k1", "project:novel", "fp", "m1", idemNow, idemTTL)
	if err != nil {
		t.Fatal(err)
	}
	if rec.State != IdemReplay {
		t.Fatalf("state = %v, want replay after recovery", rec.State)
	}
	if rec.Status != 202 || !strings.Contains(string(rec.Body), "queued") {
		t.Errorf("replayed response = %d %s", rec.Status, rec.Body)
	}
}

// A reservation whose request died before the submission was persisted
// has nothing to promise, and must stay pending rather than be settled
// with a queued answer that is not true.
func TestRecoverPendingReservations_LeavesUnpersistedAlone(t *testing.T) {
	s := memStore(t)
	if _, err := s.ReserveIdempotent("k1", "project:novel", "fp", "m-missing", idemNow, idemTTL); err != nil {
		t.Fatal(err)
	}
	n, err := s.RecoverPendingReservations(func(string) (int, string, []byte) {
		return 202, "application/json", []byte("{}")
	})
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("settled %d reservations with no persisted submission, want 0", n)
	}
	rec, _ := s.ReserveIdempotent("k1", "project:novel", "fp", "m-missing", idemNow, idemTTL)
	if rec.State != IdemInFlight {
		t.Errorf("state = %v, want the reservation left pending", rec.State)
	}
}

// A submission that completed normally must not be re-settled.
func TestRecoverPendingReservations_IgnoresSentSubmissions(t *testing.T) {
	s := memStore(t)
	if err := s.RecordSubmission(Submission{
		ID: "m1", Endpoint: "/api/send", Status: StatusSending, CreatedAt: idemNow,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReserveIdempotent("k1", "s", "fp", "m1", idemNow, idemTTL); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkSent("m1", "prov-1", idemNow); err != nil {
		t.Fatal(err)
	}
	n, err := s.RecoverPendingReservations(func(string) (int, string, []byte) {
		return 202, "application/json", []byte("{}")
	})
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("settled %d reservations for a sent submission, want 0", n)
	}
}

// The health metric must move with the gate, not with the canary probe:
// /healthz degrades the instant a write fails, and an operator alerts
// on the metric.
func TestGate_HealthTransitionIsObservedImmediately(t *testing.T) {
	s := memStore(t)
	var seen []bool
	g := NewGate(s, slog.New(slog.NewTextHandler(io.Discard, nil)))
	g.OnHealthChange(func(h bool) { seen = append(seen, h) })

	g.ReportError(fmt.Errorf("disk full"))
	if !g.Healthy() == false {
		t.Fatal("gate did not degrade")
	}
	if len(seen) != 1 || seen[0] {
		t.Fatalf("health callbacks = %v, want one false", seen)
	}
	// Repeated failures do not re-notify.
	g.ReportError(fmt.Errorf("disk full again"))
	if len(seen) != 1 {
		t.Errorf("health callbacks = %v, want no duplicate notification", seen)
	}
}
