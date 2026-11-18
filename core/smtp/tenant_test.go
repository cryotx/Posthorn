package smtp

import (
	"log/slog"
	"net/textproto"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/craigmccaskill/posthorn/metrics"
	"github.com/craigmccaskill/posthorn/project"
	"github.com/craigmccaskill/posthorn/transport"
)

const novelSMTPPassword = "novel-smtp-secret-0123456789"

func tenantRegistry(t *testing.T) *project.Registry {
	t.Helper()
	reg, err := project.Build([]project.Config{{
		Name: "novel", Enabled: true,
		APIKey:             "novel-key-0123456789abcdef",
		DefaultFrom:        "noreply@novel.example.com",
		AllowedFromDomains: []string{"novel.example.com"},
		SMTPUsername:       "novel",
		SMTPPassword:       novelSMTPPassword,
	}})
	if err != nil {
		t.Fatalf("project.Build: %v", err)
	}
	return reg
}

// tenantListener starts a listener whose sender allowlist is wide open,
// so the only thing that can reject a sender is the project policy.
func tenantListener(t *testing.T, tp transport.Transport, rec ...*metrics.Recorder) *smtpFixture {
	t.Helper()
	cfg := baseTestConfig()
	cfg.AllowedSenders = []string{"*"}
	f := startListenerWith(t, cfg, tp, rec...)
	f.listener.AttachProjects(tenantRegistry(t), nil)
	return f
}

func smtpAuth(t *testing.T, tp *textproto.Conn, user, pass string) int {
	t.Helper()
	_ = tp.PrintfLine("EHLO client.test")
	expectMultiline(t, tp, 250)
	_ = tp.PrintfLine("AUTH PLAIN %s", authPlainCreds(user, pass))
	code, _, err := tp.ReadCodeLine(-1)
	if err != nil && code == 0 {
		t.Fatalf("read AUTH reply: %v", err)
	}
	return code
}

// The same registry authenticates both doors, so an SMTP submission
// carries the same project identity an HTTP request does.
func TestSMTPTenant_ProjectCredentialAuthenticates(t *testing.T) {
	f := tenantListener(t, nil)
	if code := smtpAuth(t, f.dial(), "novel", novelSMTPPassword); code != 235 {
		t.Errorf("AUTH = %d, want 235", code)
	}
}

func TestSMTPTenant_WrongProjectPasswordRejected(t *testing.T) {
	f := tenantListener(t, nil)
	if code := smtpAuth(t, f.dial(), "novel", "wrong-password-000000000000"); code != 535 {
		t.Errorf("AUTH = %d, want 535", code)
	}
}

// The listener's own smtp_users must keep working alongside projects.
func TestSMTPTenant_ListenerUserStillWorks(t *testing.T) {
	f := tenantListener(t, nil)
	if code := smtpAuth(t, f.dial(), "user", "pass"); code != 235 {
		t.Errorf("AUTH = %d, want 235 for the listener's own user", code)
	}
}

// FR109 on the SMTP path: the From here comes from inbound MIME, so
// without the policy an SMTP-authenticated project could send as any
// domain — exactly the spoofing the policy exists to stop.
func TestSMTPTenant_SenderPolicyEnforced(t *testing.T) {
	tp := &stubTransport{res: transport.SendResult{MessageID: "ok"}}
	router := testRouter(t, poolProvider("p", tp))
	f := tenantListener(t, router)

	conn := f.dial()
	if code := smtpAuth(t, conn, "novel", novelSMTPPassword); code != 235 {
		t.Fatalf("AUTH = %d", code)
	}
	_ = conn.PrintfLine("MAIL FROM:<admin@other-project.com>")
	expect(t, conn, 250)
	_ = conn.PrintfLine("RCPT TO:<alice@somewhere.com>")
	expect(t, conn, 250)
	_ = conn.PrintfLine("DATA")
	expect(t, conn, 354)
	_ = conn.PrintfLine("From: admin@other-project.com\r\nSubject: x\r\n\r\nbody\r\n.")

	code, msg, _ := conn.ReadCodeLine(-1)
	if code != 550 {
		t.Fatalf("DATA reply = %d %q, want 550", code, msg)
	}
	if !strings.Contains(msg, "Sender not permitted") {
		t.Errorf("reply = %q", msg)
	}
	if tp.seen != 0 {
		t.Error("a spoofed sender reached the transport")
	}
}

func TestSMTPTenant_InDomainSenderAccepted(t *testing.T) {
	tp := &stubTransport{res: transport.SendResult{MessageID: "ok"}}
	router := testRouter(t, poolProvider("p", tp))
	f := tenantListener(t, router)

	conn := f.dial()
	if code := smtpAuth(t, conn, "novel", novelSMTPPassword); code != 235 {
		t.Fatalf("AUTH = %d", code)
	}
	_ = conn.PrintfLine("MAIL FROM:<support@novel.example.com>")
	expect(t, conn, 250)
	_ = conn.PrintfLine("RCPT TO:<alice@somewhere.com>")
	expect(t, conn, 250)
	_ = conn.PrintfLine("DATA")
	expect(t, conn, 354)
	_ = conn.PrintfLine("From: support@novel.example.com\r\nSubject: x\r\n\r\nbody\r\n.")
	if code, _, _ := conn.ReadCodeLine(-1); code != 250 {
		t.Fatalf("DATA reply = %d, want 250", code)
	}
	if tp.seen != 1 {
		t.Errorf("transport saw %d sends, want 1", tp.seen)
	}
}

func TestSMTPTenant_ProjectMetrics(t *testing.T) {
	reg := metrics.New()
	rec := metrics.NewRecorder(reg)
	tp := &stubTransport{res: transport.SendResult{MessageID: "ok"}}
	router := testRouter(t, poolProvider("p", tp))
	f := tenantListener(t, router, rec)

	conn := f.dial()
	if code := smtpAuth(t, conn, "novel", novelSMTPPassword); code != 235 {
		t.Fatalf("AUTH = %d", code)
	}
	_ = conn.PrintfLine("MAIL FROM:<support@novel.example.com>")
	expect(t, conn, 250)
	_ = conn.PrintfLine("RCPT TO:<alice@somewhere.com>")
	expect(t, conn, 250)
	_ = conn.PrintfLine("DATA")
	expect(t, conn, 354)
	_ = conn.PrintfLine("Subject: x\r\n\r\nbody\r\n.")
	if code, _, _ := conn.ReadCodeLine(-1); code != 250 {
		t.Fatal("DATA rejected")
	}

	waitForMetric(t, reg, `posthorn_project_send_total{project="novel",status="sent"} 1`, 2*time.Second)
}

// NFR44: an SMTP credential must never reach a log line.
func TestSMTPTenant_CredentialNeverLogged(t *testing.T) {
	var buf safeBuffer
	cfg := baseTestConfig()
	cfg.AllowedSenders = []string{"*"}
	f := startListenerFull(t, cfg, nil, slog.New(slog.NewTextHandler(&buf, nil)))
	f.listener.AttachProjects(tenantRegistry(t), nil)

	conn := f.dial()
	_ = smtpAuth(t, conn, "novel", novelSMTPPassword)
	_ = smtpAuth(t, f.dial(), "novel", "wrong-password-000000000000")

	if strings.Contains(buf.String(), novelSMTPPassword) {
		t.Errorf("SMTP password reached the log:\n%s", buf.String())
	}
}

// safeBuffer is a mutex-guarded log sink: the listener writes from its
// session goroutines while the test reads.
type safeBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *safeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *safeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// --- project quota on the SMTP path (FR125) ----------------------------

// stubQuota is a ProjectQuota that allows a fixed number of sends, so
// the SMTP reply for an exhausted project can be checked without
// wiring the whole gateway usage tracker.
type stubQuota struct {
	mu        sync.Mutex
	allowance int
	released  int
}

func (q *stubQuota) Reserve(_ *project.Project, _ time.Time) (func(bool), bool, string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.allowance <= 0 {
		return func(bool) {}, false, "daily"
	}
	q.allowance--
	return func(success bool) {
		if !success {
			q.mu.Lock()
			q.allowance++
			q.released++
			q.mu.Unlock()
		}
	}, true, ""
}

// quotaListener takes the interface, not the concrete type: passing a
// typed nil pointer as an interface produces a non-nil interface, and
// the listener would then call a method on a nil receiver. Production
// never does that (gateway.ProjectUsage is always constructed), but a
// test that models "no quota" must model it as an actually-nil
// interface.
func quotaListener(t *testing.T, tp transport.Transport, q ProjectQuota, rec ...*metrics.Recorder) *smtpFixture {
	t.Helper()
	cfg := baseTestConfig()
	cfg.AllowedSenders = []string{"*"}
	f := startListenerWith(t, cfg, tp, rec...)
	f.listener.AttachProjects(tenantRegistry(t), q)
	return f
}

func submitAs(t *testing.T, f *smtpFixture, from string) int {
	t.Helper()
	conn := f.dial()
	if code := smtpAuth(t, conn, "novel", novelSMTPPassword); code != 235 {
		t.Fatalf("AUTH = %d", code)
	}
	_ = conn.PrintfLine("MAIL FROM:<%s>", from)
	expect(t, conn, 250)
	_ = conn.PrintfLine("RCPT TO:<alice@somewhere.com>")
	expect(t, conn, 250)
	_ = conn.PrintfLine("DATA")
	expect(t, conn, 354)
	_ = conn.PrintfLine("Subject: x\r\n\r\nbody\r\n.")
	code, _, _ := conn.ReadCodeLine(-1)
	return code
}

// A project out of budget gets a TEMPORARY failure: the allowance comes
// back on a calendar boundary, so a sending MTA that queues and retries
// will succeed — and a 5xx would tell it to give up permanently.
func TestSMTPTenant_ProjectQuotaExhaustedIsTemporary(t *testing.T) {
	tp := &stubTransport{res: transport.SendResult{MessageID: "ok"}}
	q := &stubQuota{allowance: 2}
	f := quotaListener(t, testRouter(t, poolProvider("p", tp)), q)

	for i := 1; i <= 2; i++ {
		if code := submitAs(t, f, "support@novel.example.com"); code != 250 {
			t.Fatalf("send %d = %d, want 250", i, code)
		}
	}
	code := submitAs(t, f, "support@novel.example.com")
	if code != 452 {
		t.Fatalf("exhausted send = %d, want 452 (temporary)", code)
	}
	if tp.seen != 2 {
		t.Errorf("transport saw %d sends, want 2", tp.seen)
	}
}

// A rejected sender must not spend the project's allowance — the check
// runs before the reservation.
func TestSMTPTenant_SenderRejectionDoesNotSpendQuota(t *testing.T) {
	tp := &stubTransport{res: transport.SendResult{MessageID: "ok"}}
	q := &stubQuota{allowance: 1}
	f := quotaListener(t, testRouter(t, poolProvider("p", tp)), q)

	if code := submitAs(t, f, "admin@other-project.com"); code != 550 {
		t.Fatalf("spoofed sender = %d, want 550", code)
	}
	if code := submitAs(t, f, "support@novel.example.com"); code != 250 {
		t.Errorf("legitimate send = %d, want 250 — the rejection spent the allowance", code)
	}
}

// A failed send hands the reservation back.
func TestSMTPTenant_FailedSendReleasesQuota(t *testing.T) {
	tp := &stubTransport{err: &transport.TransportError{
		Class: transport.ErrTerminal, Status: 422, Message: "rejected"}}
	q := &stubQuota{allowance: 1}
	f := quotaListener(t, testRouter(t, poolProvider("p", tp)), q)

	if code := submitAs(t, f, "support@novel.example.com"); code != 451 {
		t.Fatalf("failed send = %d, want 451", code)
	}
	// The reservation is released on the way out of the DATA handler,
	// which happens after the 451 is written — so the assertion has to
	// wait for it rather than race it.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		q.mu.Lock()
		released := q.released
		q.mu.Unlock()
		if released == 1 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Error("a failed send kept the tenant's allowance")
}

func TestSMTPTenant_QuotaMetric(t *testing.T) {
	reg := metrics.New()
	rec := metrics.NewRecorder(reg)
	tp := &stubTransport{res: transport.SendResult{MessageID: "ok"}}
	f := quotaListener(t, testRouter(t, poolProvider("p", tp)), &stubQuota{allowance: 0}, rec)

	if code := submitAs(t, f, "support@novel.example.com"); code != 452 {
		t.Fatalf("code = %d", code)
	}
	waitForMetric(t, reg,
		`posthorn_project_quota_exhausted_total{project="novel",window="daily"} 1`, 2*time.Second)
}

// A listener with no quota attached behaves exactly as before.
func TestSMTPTenant_NoQuotaIsUnchanged(t *testing.T) {
	tp := &stubTransport{res: transport.SendResult{MessageID: "ok"}}
	var noQuota ProjectQuota // an actually-nil interface, not a typed nil
	f := quotaListener(t, testRouter(t, poolProvider("p", tp)), noQuota)
	for i := 0; i < 5; i++ {
		if code := submitAs(t, f, "support@novel.example.com"); code != 250 {
			t.Fatalf("send %d = %d", i, code)
		}
	}
}
