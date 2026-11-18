package storage

import (
	"strings"
	"testing"
	"time"
)

var auditNow = time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)

func TestAudit_RecordAndList(t *testing.T) {
	s := memStore(t)
	entries := []AuditEntry{
		{At: auditNow, Actor: ActorAdmin, Action: AuditProviderDisabled, Target: "postmark-main",
			Details: map[string]string{"reason": "maintenance"}, SourceIP: "127.0.0.1"},
		{At: auditNow.Add(time.Minute), Actor: ActorCLI, Action: AuditSuppressionRemoved,
			Target: "user@example.com"},
	}
	for _, e := range entries {
		if err := s.RecordAudit(e); err != nil {
			t.Fatalf("RecordAudit: %v", err)
		}
	}

	got, err := s.ListAudit(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d entries, want 2", len(got))
	}
	// Newest first: an operator reading an audit log is looking for
	// what just happened.
	if got[0].Action != AuditSuppressionRemoved {
		t.Errorf("first entry = %q, want the newest", got[0].Action)
	}
	if got[1].Details["reason"] != "maintenance" {
		t.Errorf("details = %+v", got[1].Details)
	}
	if got[1].SourceIP != "127.0.0.1" {
		t.Errorf("source_ip = %q", got[1].SourceIP)
	}
}

// TestAudit_SurvivesRetentionPruning: a trail that ages out is a trail
// an attacker only has to outwait.
func TestAudit_SurvivesRetentionPruning(t *testing.T) {
	s := memStore(t)
	old := auditNow.Add(-365 * 24 * time.Hour)
	if err := s.RecordAudit(AuditEntry{
		At: old, Actor: ActorAdmin, Action: AuditProviderDisabled, Target: "postmark",
	}); err != nil {
		t.Fatal(err)
	}
	// Prune everything a retention sweep would touch.
	if _, err := s.Prune(auditNow); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PruneAttempts(auditNow); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PruneDeliveryEvents(auditNow); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PruneIdempotency(auditNow); err != nil {
		t.Fatal(err)
	}

	n, err := s.CountAudit()
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("audit entries after pruning = %d, want 1", n)
	}
}

// The audit log records WHAT changed. There is no column for a secret,
// and this pins that a caller passing one in Details is the caller's
// bug rather than a schema that invites it.
func TestAudit_HasNoSecretColumn(t *testing.T) {
	s := memStore(t)
	rows, err := s.db.Query(`SELECT name FROM pragma_table_info('audit_log')`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		lower := strings.ToLower(name)
		for _, forbidden := range []string{"secret", "password", "key", "token", "credential"} {
			if strings.Contains(lower, forbidden) {
				t.Errorf("audit_log has a column named %q", name)
			}
		}
	}
}

// --- dead letters ------------------------------------------------------

func TestDeadLetters_ListExcludesQueuedFailures(t *testing.T) {
	s := memStore(t)
	seedMessage(t, s, "dead", "acme", auditNow, func(sub *Submission) {
		sub.Status = StatusFailed
	})
	seedMessage(t, s, "still-queued", "acme", auditNow, func(sub *Submission) {
		sub.Status = StatusFailed
	})
	if err := s.Enqueue("still-queued", auditNow); err != nil {
		t.Fatal(err)
	}

	dead, err := s.ListDeadLetters(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(dead) != 1 || dead[0].ID != "dead" {
		t.Fatalf("dead letters = %+v, want only the unqueued failure", dead)
	}
	if n, err := s.CountDeadLetters(); err != nil || n != 1 {
		t.Errorf("CountDeadLetters = %d, %v", n, err)
	}
	// The dead-letter listing is a message listing: no content in it.
	if strings.Contains(sprintAny(dead), "SECRET BODY CONTENT") {
		t.Error("dead-letter listing carried message content")
	}
}

func TestDeadLetters_RequeuePutsItBackOnTheQueue(t *testing.T) {
	s := memStore(t)
	seedMessage(t, s, "dead", "acme", auditNow, func(sub *Submission) {
		sub.Status = StatusFailed
	})

	if err := s.RequeueDeadLetter("dead", auditNow); err != nil {
		t.Fatalf("RequeueDeadLetter: %v", err)
	}
	sub, ok, err := s.GetSubmission("dead")
	if err != nil || !ok {
		t.Fatalf("GetSubmission: %v", err)
	}
	if sub.Status != StatusQueued {
		t.Errorf("status = %q, want %q", sub.Status, StatusQueued)
	}
	due, err := s.ClaimDue(auditNow, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 1 || due[0].Submission.ID != "dead" {
		t.Errorf("claimed %+v, want the requeued message due immediately", due)
	}
}

// A retry that would deliver an EMPTY message is worse than no retry:
// the content is gone and cannot be reconstructed.
func TestDeadLetters_RefusesContentlessRow(t *testing.T) {
	s := memStore(t)
	seedMessage(t, s, "metadata-only", "acme", auditNow, func(sub *Submission) {
		sub.Status = StatusFailed
		sub.BodyText = ""
		sub.BodyHTML = ""
	})

	err := s.RequeueDeadLetter("metadata-only", auditNow)
	if err == nil {
		t.Fatal("a content-less submission was requeued")
	}
	if !strings.Contains(err.Error(), "empty message") {
		t.Errorf("error = %v, want it to explain why", err)
	}
}

func TestDeadLetters_RefusesNonDeadLetters(t *testing.T) {
	s := memStore(t)
	seedMessage(t, s, "sent", "acme", auditNow, nil)
	seedMessage(t, s, "queued", "acme", auditNow, func(sub *Submission) {
		sub.Status = StatusFailed
	})
	if err := s.Enqueue("queued", auditNow); err != nil {
		t.Fatal(err)
	}

	if err := s.RequeueDeadLetter("sent", auditNow); err == nil {
		t.Error("a sent message was requeued")
	}
	if err := s.RequeueDeadLetter("queued", auditNow); err == nil {
		t.Error("an already-queued message was queued twice")
	}
	if err := s.RequeueDeadLetter("nope", auditNow); err == nil {
		t.Error("a missing message was requeued")
	}
}

func TestListRetryQueue_ReportsDueOrder(t *testing.T) {
	s := memStore(t)
	seedMessage(t, s, "later", "acme", auditNow, func(sub *Submission) { sub.Status = StatusQueued })
	seedMessage(t, s, "sooner", "acme", auditNow, func(sub *Submission) { sub.Status = StatusQueued })
	if err := s.Enqueue("later", auditNow.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := s.Enqueue("sooner", auditNow); err != nil {
		t.Fatal(err)
	}

	queue, err := s.ListRetryQueue(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(queue) != 2 || queue[0].MessageID != "sooner" {
		t.Errorf("queue = %+v, want due order", queue)
	}
	if queue[0].Project != "acme" {
		t.Errorf("project = %q", queue[0].Project)
	}
}
