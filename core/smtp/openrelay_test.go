package smtp

import (
	"strings"
	"testing"
)

// Open-relay coverage (FR155).
//
// An open relay is a mail server that accepts mail from anyone, for
// anyone, and sends it on. It is the single worst way to misconfigure
// something that speaks SMTP: within hours the host is in every
// blocklist, the operator's sending domain is burned, and the abuse is
// attributed to them.
//
// Posthorn cannot be one, and this file is the automated proof rather
// than an assurance in a doc. Four independent controls have to fail
// simultaneously for relaying to happen, and each is tested here:
//
//	1. AUTH is required before MAIL FROM (unless deliberately disabled
//	   for a trusted network, which is itself gated).
//	2. allowed_senders is REQUIRED and non-empty — a listener with no
//	   sender allowlist does not start.
//	3. allowed_recipients, when set, bounds who mail can be sent TO.
//	4. Recipients come from the SMTP envelope, never from the MIME
//	   headers a submitter wrote (NFR22).

// relaySession drives a session up to the point a relay attempt would
// succeed, and reports the code the server answered with.
func relayAttempt(t *testing.T, f *smtpFixture, authenticate bool, from, to string) int {
	t.Helper()
	tp := f.dial()
	_ = tp.PrintfLine("EHLO relay-test")
	expectMultiline(t, tp, 250)
	if authenticate {
		_ = tp.PrintfLine("AUTH PLAIN %s", authPlainCreds("user", "pass"))
		expect(t, tp, 235)
	}
	_ = tp.PrintfLine("MAIL FROM:<%s>", from)
	code, _ := expectCode(t, tp)
	if code != 250 {
		return code
	}
	_ = tp.PrintfLine("RCPT TO:<%s>", to)
	code, _ = expectCode(t, tp)
	return code
}

// TestOpenRelay_UnauthenticatedStrangerIsRefused is the headline case:
// a stranger connects and tries to send from their domain to a third
// party. Every step must refuse.
func TestOpenRelay_UnauthenticatedStrangerIsRefused(t *testing.T) {
	f := startListener(t, baseTestConfig())

	code := relayAttempt(t, f, false, "spammer@evil.example", "victim@elsewhere.example")
	if code == 250 {
		t.Fatal("OPEN RELAY: an unauthenticated stranger was accepted")
	}
	if code != 530 {
		t.Errorf("code = %d, want 530 (authentication required)", code)
	}
}

// Authenticating does not turn the gateway into a relay: the sender
// still has to be one this listener is allowed to send as. Otherwise a
// single leaked SMTP password would be a relay for the whole internet.
func TestOpenRelay_AuthenticatedStrangerSenderIsRefused(t *testing.T) {
	f := startListener(t, baseTestConfig()) // allowed_senders = *@example.com

	code := relayAttempt(t, f, true, "spammer@evil.example", "victim@elsewhere.example")
	if code == 250 {
		t.Fatal("OPEN RELAY: an authenticated session sent as an unrelated domain")
	}
	if code != 550 {
		t.Errorf("code = %d, want 550 (sender not permitted)", code)
	}
}

// With a recipient allowlist configured, an allowed sender still cannot
// mail arbitrary third parties — the "compromised app" case.
func TestOpenRelay_RecipientAllowlistBoundsTheBlastRadius(t *testing.T) {
	cfg := baseTestConfig()
	cfg.AllowedRecipients = []string{"*@yourdomain.com"}
	f := startListener(t, cfg)

	if code := relayAttempt(t, f, true, "noreply@example.com", "victim@elsewhere.example"); code == 250 {
		t.Fatal("OPEN RELAY: an allowed sender mailed an unlisted third party")
	}
	if code := relayAttempt(t, f, true, "noreply@example.com", "ops@yourdomain.com"); code != 250 {
		t.Errorf("a permitted recipient was refused: %d", code)
	}
}

// A listener with no sender allowlist must not start. This is the
// misconfiguration that produces an open relay by omission, so it is
// refused at construction rather than warned about.
func TestOpenRelay_ListenerWithoutSenderAllowlistRefusesToStart(t *testing.T) {
	cfg := baseTestConfig()
	cfg.AllowedSenders = nil
	if _, err := New(cfg, &mockTransport{}, 1<<20, nil, nil); err == nil {
		t.Fatal("a listener with no allowed_senders was constructed")
	}

	cfg.AllowedSenders = []string{}
	if _, err := New(cfg, &mockTransport{}, 1<<20, nil, nil); err == nil {
		t.Fatal("a listener with an empty allowed_senders was constructed")
	}
}

// auth_required = "none" exists for a trusted internal network. It
// still is not a relay: the sender allowlist is the control that keeps
// standing, and it does.
func TestOpenRelay_AuthNoneStillEnforcesSenderPolicy(t *testing.T) {
	cfg := baseTestConfig()
	cfg.AuthRequired = AuthNone
	cfg.SMTPUsers = nil
	f := startListener(t, cfg)

	if code := relayAttempt(t, f, false, "spammer@evil.example", "victim@elsewhere.example"); code == 250 {
		t.Fatal("OPEN RELAY: auth_required = none accepted an arbitrary sender")
	}
	if code := relayAttempt(t, f, false, "noreply@example.com", "ops@example.com"); code != 250 {
		t.Errorf("a permitted sender was refused on a trusted-network listener: %d", code)
	}
}

// The one configuration that IS an open relay by definition — anyone
// may connect, and anyone may claim any sender — is refused at parse
// time. Each half is legitimate alone; together they are a mail server
// that accepts mail from anyone for anyone.
func TestOpenRelay_AuthNoneWithWildcardSenderIsRefused(t *testing.T) {
	cfg := baseTestConfig()
	cfg.AuthRequired = AuthNone
	cfg.SMTPUsers = nil
	cfg.AllowedSenders = []string{"*"}

	err := cfg.Validate()
	if err == nil {
		t.Fatal("auth_required = none with a wildcard sender allowlist was accepted")
	}
	if !strings.Contains(err.Error(), "open relay") {
		t.Errorf("error = %v, want it to name the problem", err)
	}

	// Either half alone stays permitted: a trusted-network listener
	// with named senders, and a wildcard listener that authenticates.
	authed := baseTestConfig()
	authed.AllowedSenders = []string{"*"}
	if err := authed.Validate(); err != nil {
		t.Errorf("a wildcard allowlist behind authentication was refused: %v", err)
	}
	trusted := baseTestConfig()
	trusted.AuthRequired = AuthNone
	trusted.SMTPUsers = nil
	if err := trusted.Validate(); err != nil {
		t.Errorf("a trusted-network listener with named senders was refused: %v", err)
	}
}

// An empty allowlist fails CLOSED rather than open: nothing matches, so
// every sender is refused. Worth pinning, because the opposite reading
// of "no allowlist" is exactly how open relays happen.
func TestOpenRelay_EmptyAllowlistMatchesNothing(t *testing.T) {
	for _, addr := range []string{"anyone@anywhere.example", "noreply@example.com", ""} {
		if matchesAllowlist(addr, nil) {
			t.Errorf("an empty allowlist matched %q", addr)
		}
		if matchesAllowlist(addr, []string{}) {
			t.Errorf("an empty allowlist matched %q", addr)
		}
	}
}

// NFR22: the outbound recipient comes from RCPT TO, never from the MIME
// To/Cc/Bcc a submitter wrote. Otherwise an authorised sender could
// smuggle extra recipients past the envelope checks entirely.
func TestOpenRelay_MIMEHeadersCannotAddRecipients(t *testing.T) {
	cfg := baseTestConfig()
	cfg.AllowedRecipients = []string{"*@yourdomain.com"}
	f := startListener(t, cfg)

	tp := f.dial()
	_ = tp.PrintfLine("EHLO relay-test")
	expectMultiline(t, tp, 250)
	_ = tp.PrintfLine("AUTH PLAIN %s", authPlainCreds("user", "pass"))
	expect(t, tp, 235)
	_ = tp.PrintfLine("MAIL FROM:<noreply@example.com>")
	expect(t, tp, 250)
	_ = tp.PrintfLine("RCPT TO:<ops@yourdomain.com>")
	expect(t, tp, 250)
	_ = tp.PrintfLine("DATA")
	expect(t, tp, 354)

	// The MIME headers name recipients the envelope never authorised.
	for _, line := range []string{
		"From: noreply@example.com",
		"To: ops@yourdomain.com",
		"Cc: smuggled-cc@elsewhere.example",
		"Bcc: smuggled-bcc@elsewhere.example",
		"Subject: relay probe",
		"",
		"body",
		".",
	} {
		_ = tp.PrintfLine("%s", line)
	}
	expect(t, tp, 250)

	sent := f.mt.Sent()
	if len(sent) != 1 {
		t.Fatalf("sent %d messages, want 1", len(sent))
	}
	got := strings.Join(sent[0].To, ",")
	if got != "ops@yourdomain.com" {
		t.Errorf("outbound recipients = %q, want only the envelope recipient", got)
	}
	for _, smuggled := range []string{"smuggled-cc@elsewhere.example", "smuggled-bcc@elsewhere.example"} {
		if strings.Contains(got, smuggled) {
			t.Errorf("OPEN RELAY: a MIME header added recipient %q", smuggled)
		}
	}
}
