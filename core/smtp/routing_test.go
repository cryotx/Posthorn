package smtp

import (
	"context"
	"net/textproto"
	"testing"
	"time"

	"github.com/craigmccaskill/posthorn/config"
	"github.com/craigmccaskill/posthorn/metrics"
	"github.com/craigmccaskill/posthorn/transport"
)

// stubTransport is a minimal Transport for pool entries in these tests.
type stubTransport struct {
	res  transport.SendResult
	err  error
	seen int
}

func (s *stubTransport) Send(context.Context, transport.Message) (transport.SendResult, error) {
	s.seen++
	return s.res, s.err
}

func poolProvider(name string, tp transport.Transport) *transport.Provider {
	return &transport.Provider{
		Name: name, Type: "mock", Weight: 1, Transport: tp,
		Breaker: transport.NewBreaker(transport.BreakerConfig{Disabled: true}),
	}
}

func testRouter(t *testing.T, provs ...*transport.Provider) *transport.Router {
	t.Helper()
	route, err := transport.NewRoute(nil, transport.StrategyFailover, provs)
	if err != nil {
		t.Fatalf("NewRoute: %v", err)
	}
	r, err := transport.NewRouter("smtp_listener", route, nil)
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	return r
}

// pooledSMTPConfig is baseTestConfig with a pooled transport block, so
// the listener's labels resolve through TransportConfig.Label().
func pooledSMTPConfig() ListenerConfig {
	cfg := baseTestConfig()
	cfg.Transport = config.TransportConfig{Providers: []string{"postmark-prod", "ses-backup"}}
	return cfg
}

// submit drives one full SMTP transaction and returns the DATA reply code.
func submit(t *testing.T, tp *textproto.Conn) int {
	t.Helper()
	_ = tp.PrintfLine("EHLO client.test")
	expectMultiline(t, tp, 250)
	_ = tp.PrintfLine("AUTH PLAIN %s", authPlainCreds("user", "pass"))
	expect(t, tp, 235)
	_ = tp.PrintfLine("MAIL FROM:<noreply@example.com>")
	expect(t, tp, 250)
	_ = tp.PrintfLine("RCPT TO:<alice@somewhere.com>")
	expect(t, tp, 250)
	_ = tp.PrintfLine("DATA")
	expect(t, tp, 354)
	_ = tp.PrintfLine("Subject: hello\r\n\r\nbody\r\n.")
	code, _, err := tp.ReadCodeLine(-1)
	if err != nil && code == 0 {
		t.Fatalf("read DATA reply: %v", err)
	}
	return code
}

// ADR-13 said one listener has one transport. It now has one route —
// the listener still hands a message to a single Transport, which may
// itself be a pool.
func TestSMTP_ListenerFailsOverBetweenProviders(t *testing.T) {
	primary := &stubTransport{err: &transport.TransportError{Class: transport.ErrTransient, Message: "down"}}
	backup := &stubTransport{res: transport.SendResult{MessageID: "ses-1"}}
	router := testRouter(t, poolProvider("postmark-prod", primary), poolProvider("ses-backup", backup))

	f := startListenerWith(t, pooledSMTPConfig(), router)
	if code := submit(t, f.dial()); code != 250 {
		t.Fatalf("DATA reply = %d, want 250", code)
	}
	if primary.seen != 1 || backup.seen != 1 {
		t.Errorf("attempts: primary=%d backup=%d, want 1 and 1", primary.seen, backup.seen)
	}
}

func TestSMTP_MetricsLabelTheDeliveringProvider(t *testing.T) {
	reg := metrics.New()
	rec := metrics.NewRecorder(reg)

	primary := &stubTransport{err: &transport.TransportError{Class: transport.ErrTransient, Message: "down"}}
	backup := &stubTransport{res: transport.SendResult{MessageID: "ses-1"}}
	router := testRouter(t, poolProvider("postmark-prod", primary), poolProvider("ses-backup", backup))

	f := startListenerWith(t, pooledSMTPConfig(), router, rec)
	if code := submit(t, f.dial()); code != 250 {
		t.Fatalf("DATA reply = %d, want 250", code)
	}

	waitForMetric(t, reg,
		`posthorn_submissions_sent_total{endpoint="smtp_listener",transport="ses-backup"} 1`,
		2*time.Second)
}

func TestSMTP_FailureMetricNamesTheFailingProvider(t *testing.T) {
	reg := metrics.New()
	rec := metrics.NewRecorder(reg)

	down := &stubTransport{err: &transport.TransportError{
		Class: transport.ErrTerminal, Status: 422, Message: "rejected"}}
	router := testRouter(t, poolProvider("postmark-prod", down))

	f := startListenerWith(t, pooledSMTPConfig(), router, rec)
	if code := submit(t, f.dial()); code != 451 {
		t.Fatalf("DATA reply = %d, want 451", code)
	}

	waitForMetric(t, reg,
		`posthorn_submissions_failed_total{endpoint="smtp_listener",transport="postmark-prod",error_class="terminal"} 1`,
		2*time.Second)
}

// An inline listener config keeps the v1.x transport-type label.
func TestSMTP_InlineListenerLabelUnchanged(t *testing.T) {
	reg := metrics.New()
	rec := metrics.NewRecorder(reg)

	f := startListener(t, baseTestConfig(), rec)
	if code := submit(t, f.dial()); code != 250 {
		t.Fatalf("DATA reply = %d, want 250", code)
	}

	waitForMetric(t, reg,
		`posthorn_submissions_sent_total{endpoint="smtp_listener",transport="postmark"} 1`,
		2*time.Second)
}

// NFR22 is unchanged by routing: outbound recipients come from the SMTP
// envelope, never from the inbound MIME headers, whichever provider the
// route lands on.
func TestSMTP_RoutingDoesNotWeakenEnvelopeOnlyRecipients(t *testing.T) {
	backup := &stubTransport{res: transport.SendResult{MessageID: "ok"}}
	primary := &stubTransport{err: &transport.TransportError{Class: transport.ErrTransient}}
	var captured transport.Message
	capturing := &captureTransport{inner: backup, onSend: func(m transport.Message) { captured = m }}
	router := testRouter(t, poolProvider("postmark-prod", primary), poolProvider("ses-backup", capturing))

	f := startListenerWith(t, pooledSMTPConfig(), router)
	tp := f.dial()
	_ = tp.PrintfLine("EHLO client.test")
	expectMultiline(t, tp, 250)
	_ = tp.PrintfLine("AUTH PLAIN %s", authPlainCreds("user", "pass"))
	expect(t, tp, 235)
	_ = tp.PrintfLine("MAIL FROM:<noreply@example.com>")
	expect(t, tp, 250)
	_ = tp.PrintfLine("RCPT TO:<envelope@somewhere.com>")
	expect(t, tp, 250)
	_ = tp.PrintfLine("DATA")
	expect(t, tp, 354)
	_ = tp.PrintfLine("To: header-victim@evil.example\r\nBcc: smuggled@evil.example\r\nSubject: x\r\n\r\nbody\r\n.")
	if code, _, _ := tp.ReadCodeLine(-1); code != 250 {
		t.Fatalf("DATA reply = %d, want 250", code)
	}

	if len(captured.To) != 1 || captured.To[0] != "envelope@somewhere.com" {
		t.Errorf("To = %v, want only the envelope recipient", captured.To)
	}
}

type captureTransport struct {
	inner  transport.Transport
	onSend func(transport.Message)
}

func (c *captureTransport) Send(ctx context.Context, m transport.Message) (transport.SendResult, error) {
	c.onSend(m)
	return c.inner.Send(ctx, m)
}
