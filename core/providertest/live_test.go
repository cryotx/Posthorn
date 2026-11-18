//go:build integration

// Modified for the cryotx/posthorn distribution.

// Live tier (issue #76, S4/S6). Runs only under `-tags integration`, so
// its heavier expectations and any future live-only deps stay out of the
// default `go test ./...` that every PR runs.
//
// Live tests verify a DIFFERENT thing from the hermetic tier: hermetic
// can read the outbound bytes and assert their shape; live cannot (they
// went to the real provider), so it asserts the OUTCOME — the real API
// accepted the send. They share the Scenario/expected-outcome definitions
// with the hermetic tier.
//
// Credentials come from the environment (sourced from the maintainer's
// `pass` store locally, or a protected GitHub Environment in CI). A test
// whose credentials are absent SKIPS rather than fails — except Postmark,
// which uses the provider's PUBLIC, non-secret test token and so always
// runs, proving the live harness itself works with zero secrets.
package providertest

import (
	"context"
	"os"
	"testing"

	"github.com/craigmccaskill/posthorn/transport"
)

// liveMessage is the canonical send used across live checks.
func liveMessage() transport.Message {
	return transport.Message{
		From:     envOr("POSTHORN_TEST_FROM", "posthorn-test@example.com"),
		To:       []string{envOr("POSTHORN_TEST_TO", "test@example.com")},
		Subject:  "Posthorn live harness",
		BodyText: "Sent by the Posthorn integration harness.",
	}
}

// TestLive_Postmark uses POSTMARK_API_TEST — Postmark's documented,
// PUBLIC test token. The real API validates the request and returns a
// success with a test MessageID but delivers nothing. Zero secret; runs
// everywhere the integration tag is set.
func TestLive_Postmark(t *testing.T) {
	tp := transport.NewPostmarkTransport("POSTMARK_API_TEST", "")
	res, err := tp.Send(context.Background(), liveMessage())
	if err != nil {
		t.Fatalf("live Postmark send failed: %v", err)
	}
	if res.MessageID == "" {
		t.Error("no MessageID returned from the real Postmark API")
	}
}

// TestLive_Resend runs only when RESEND_API_KEY is present (a real,
// restricted key). Delivers to Resend's simulator address so nothing
// reaches a real inbox.
func TestLive_Resend(t *testing.T) {
	key := os.Getenv("RESEND_API_KEY")
	if key == "" {
		t.Skip("RESEND_API_KEY not set — skipping live Resend")
	}
	msg := liveMessage()
	msg.To = []string{"delivered@resend.dev"} // Resend's non-delivering simulator
	tp := transport.NewResendTransport(key, "")
	if _, err := tp.Send(context.Background(), msg); err != nil {
		t.Fatalf("live Resend send failed: %v", err)
	}
}

// TestLive_Mailgun runs only when MAILGUN_API_KEY + MAILGUN_DOMAIN are
// present (a sandbox domain). testmode is requested via the recipient
// being an authorized sandbox address; the request is accepted, not
// delivered.
func TestLive_Mailgun(t *testing.T) {
	key := os.Getenv("MAILGUN_API_KEY")
	domain := os.Getenv("MAILGUN_DOMAIN")
	if key == "" || domain == "" {
		t.Skip("MAILGUN_API_KEY / MAILGUN_DOMAIN not set — skipping live Mailgun")
	}
	tp := transport.NewMailgunTransport(key, domain, "")
	if _, err := tp.Send(context.Background(), liveMessage()); err != nil {
		t.Fatalf("live Mailgun send failed: %v", err)
	}
}

// TestLive_SES runs when AWS credentials are present in the environment
// (in CI these come from the OIDC → STS role, so nothing long-lived is
// stored). Delivers to the SES simulator success address.
func TestLive_SES(t *testing.T) {
	akid := os.Getenv("AWS_ACCESS_KEY_ID")
	secret := os.Getenv("AWS_SECRET_ACCESS_KEY")
	region := envOr("AWS_REGION", "us-east-1")
	if akid == "" || secret == "" {
		t.Skip("AWS credentials not set — skipping live SES")
	}
	msg := liveMessage()
	msg.To = []string{"success@simulator.amazonses.com"} // SES simulator, no delivery
	tp := transport.NewSESTransport(akid, secret, region, "")
	if _, err := tp.Send(context.Background(), msg); err != nil {
		t.Fatalf("live SES send failed: %v", err)
	}
}

// TestLive_Brevo runs only when BREVO_API_KEY is present. Brevo has no
// simulator address, so POSTHORN_TEST_TO must be a mailbox the operator
// owns — which is why this one both requires an explicit recipient and
// really delivers.
func TestLive_Brevo(t *testing.T) {
	key := os.Getenv("BREVO_API_KEY")
	to := os.Getenv("POSTHORN_TEST_TO")
	if key == "" || to == "" {
		t.Skip("BREVO_API_KEY / POSTHORN_TEST_TO not set — skipping live Brevo")
	}
	msg := liveMessage()
	msg.To = []string{to}
	tp := transport.NewBrevoTransport(key, "")
	res, err := tp.Send(context.Background(), msg)
	if err != nil {
		t.Fatalf("live Brevo send failed: %v", err)
	}
	// The reliable message id is the reason this adapter is native
	// rather than the generic relay; a live run that returns none means
	// the justification no longer holds.
	if res.MessageID == "" {
		t.Error("no messageId returned from the real Brevo API")
	}
}

// TestLive_Cloudflare runs only when CLOUDFLARE_ACCOUNT_ID +
// CLOUDFLARE_API_TOKEN are present. Cloudflare publishes no simulator
// address either, so POSTHORN_TEST_TO must be a real mailbox.
//
// The assertion is deliberately NOT about a message id — the REST send
// returns none — but about the send being accepted, which is what the
// per-recipient result reports.
func TestLive_Cloudflare(t *testing.T) {
	account := os.Getenv("CLOUDFLARE_ACCOUNT_ID")
	token := os.Getenv("CLOUDFLARE_API_TOKEN")
	to := os.Getenv("POSTHORN_TEST_TO")
	if account == "" || token == "" || to == "" {
		t.Skip("CLOUDFLARE_ACCOUNT_ID / CLOUDFLARE_API_TOKEN / POSTHORN_TEST_TO not set — skipping live Cloudflare")
	}
	msg := liveMessage()
	msg.To = []string{to}
	tp := transport.NewCloudflareTransport(account, token, "")
	if _, err := tp.Send(context.Background(), msg); err != nil {
		t.Fatalf("live Cloudflare send failed: %v", err)
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
