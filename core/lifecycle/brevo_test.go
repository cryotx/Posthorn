package lifecycle

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func brevoTestAdapter(t *testing.T, cfg ProviderAuthConfig) ProviderWebhookAdapter {
	t.Helper()
	a, known, err := BuildAdapter("brevo", cfg)
	if err != nil || !known {
		t.Fatalf("BuildAdapter(brevo): known=%v err=%v", known, err)
	}
	return a
}

func TestBrevoAdapter_UnifiedTypeMapping(t *testing.T) {
	a := brevoTestAdapter(t, ProviderAuthConfig{})
	for _, tc := range []struct {
		event string
		want  EventType
	}{
		// "request" is Brevo accepting the message for sending — the
		// sent step, not a delivery.
		{"request", TypeSent},
		{"delivered", TypeDelivered},
		{"deferred", TypeDeferred},
		{"soft_bounce", TypeDeferred},
		{"hard_bounce", TypeBounced},
		{"invalid_email", TypeBounced},
		{"blocked", TypeBounced},
		{"spam", TypeComplained},
		{"error", TypeFailed},
		// Engagement is deliberately not delivery state.
		{"opened", TypeOther},
		{"unique_opened", TypeOther},
		{"click", TypeOther},
		{"proxy_open", TypeOther},
		{"unsubscribed", TypeOther},
		{"something_brevo_adds_later", TypeOther},
	} {
		raw, err := a.Parse([]byte(`{"event":"` + tc.event + `","message-id":"<m@brevo>","email":"a@example.com"}`))
		if err != nil {
			t.Fatal(err)
		}
		ev, err := a.Normalize(raw[0], evNow)
		if err != nil {
			t.Fatalf("%s: %v", tc.event, err)
		}
		if ev.Type != tc.want {
			t.Errorf("%s → %q, want %q", tc.event, ev.Type, tc.want)
		}
		if ev.Provider != "brevo" {
			t.Errorf("provider = %q", ev.Provider)
		}
		if ev.ProviderMessageID != "<m@brevo>" {
			t.Errorf("provider message id = %q", ev.ProviderMessageID)
		}
	}
}

// TestBrevoAdapter_RecipientScoped: Brevo sends one event per recipient
// address, so the event must be scoped to it. A message-scoped reading
// would let one recipient's bounce overwrite another's delivery.
func TestBrevoAdapter_RecipientScoped(t *testing.T) {
	a := brevoTestAdapter(t, ProviderAuthConfig{})
	raw, _ := a.Parse([]byte(`{"event":"hard_bounce","message-id":"m","email":"one@example.com"}`))
	ev, err := a.Normalize(raw[0], evNow)
	if err != nil {
		t.Fatal(err)
	}
	if ev.Recipient != "one@example.com" {
		t.Errorf("Recipient = %q, want the event's own address", ev.Recipient)
	}
}

func TestBrevoAdapter_Timestamps(t *testing.T) {
	a := brevoTestAdapter(t, ProviderAuthConfig{})
	const unixSec = 1757332800 // 2025-09-08T12:00:00Z

	for _, tc := range []struct {
		name    string
		payload string
		want    time.Time
	}{
		{"ts_event_preferred", `{"event":"delivered","ts_event":1757332800,"ts":1,"message-id":"m"}`,
			time.Unix(unixSec, 0).UTC()},
		{"ts_fallback", `{"event":"delivered","ts":1757332800,"message-id":"m"}`,
			time.Unix(unixSec, 0).UTC()},
		{"millisecond_epoch", `{"event":"delivered","ts_epoch":1757332800000,"message-id":"m"}`,
			time.Unix(unixSec, 0).UTC()},
		// No usable timestamp: ingestion time, rather than a zero value
		// that would sort before every real event.
		{"none", `{"event":"delivered","message-id":"m"}`, evNow.UTC()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, _ := a.Parse([]byte(tc.payload))
			ev, err := a.Normalize(raw[0], evNow)
			if err != nil {
				t.Fatal(err)
			}
			if !ev.Timestamp.Equal(tc.want) {
				t.Errorf("Timestamp = %v, want %v", ev.Timestamp, tc.want)
			}
		})
	}
}

// TestBrevoAdapter_DerivedEventKeyIsStable: Brevo mints no per-callback
// event id, so redeliveries must collapse on a key derived from the
// event's identifying content.
func TestBrevoAdapter_DerivedEventKeyIsStable(t *testing.T) {
	a := brevoTestAdapter(t, ProviderAuthConfig{})
	payload := []byte(`{"event":"delivered","ts_event":1757332800,"message-id":"m-1","email":"a@example.com"}`)

	raw1, _ := a.Parse(payload)
	ev1, _ := a.Normalize(raw1[0], evNow)
	raw2, _ := a.Parse(payload)
	ev2, _ := a.Normalize(raw2[0], evNow.Add(time.Hour))

	if ev1.ProviderEventKey == "" {
		t.Fatal("no replay key derived")
	}
	if ev1.ProviderEventKey != ev2.ProviderEventKey {
		t.Errorf("replay key not stable across redelivery: %q vs %q",
			ev1.ProviderEventKey, ev2.ProviderEventKey)
	}

	// A different recipient is a different event, not a redelivery.
	rawOther, _ := a.Parse([]byte(`{"event":"delivered","ts_event":1757332800,"message-id":"m-1","email":"b@example.com"}`))
	evOther, _ := a.Normalize(rawOther[0], evNow)
	if evOther.ProviderEventKey == ev1.ProviderEventKey {
		t.Error("two recipients collapsed onto one replay key")
	}
}

// TestBrevoAdapter_EventKeyCarriesNoAddress: the key is stored and
// indexed, so it must not turn the dedup index into a recipient list.
func TestBrevoAdapter_EventKeyCarriesNoAddress(t *testing.T) {
	a := brevoTestAdapter(t, ProviderAuthConfig{})
	raw, _ := a.Parse([]byte(`{"event":"delivered","message-id":"m","email":"secret.user@example.com"}`))
	ev, _ := a.Normalize(raw[0], evNow)
	if strings.Contains(ev.ProviderEventKey, "secret.user") {
		t.Errorf("replay key contains the recipient address: %q", ev.ProviderEventKey)
	}
}

// TestBrevoAdapter_ReasonIsBounded: provider prose crosses to project
// webhooks as metadata, so it cannot be unbounded.
func TestBrevoAdapter_ReasonIsBounded(t *testing.T) {
	a := brevoTestAdapter(t, ProviderAuthConfig{})
	long := strings.Repeat("x", 5000)
	raw, _ := a.Parse([]byte(`{"event":"hard_bounce","message-id":"m","reason":"` + long + `"}`))
	ev, _ := a.Normalize(raw[0], evNow)
	if got := len(ev.Metadata["provider_reason"]); got > 200 {
		t.Errorf("provider_reason length = %d, want it bounded", got)
	}
}

func TestBrevoAdapter_MissingEventIsAnError(t *testing.T) {
	a := brevoTestAdapter(t, ProviderAuthConfig{})
	if _, err := a.Normalize(RawEvent{Payload: []byte(`{"message-id":"m"}`)}, evNow); err == nil {
		t.Error("payload with no event accepted")
	}
}

// TestBrevoAdapter_SigningSecretRefused is the honesty check: Brevo
// publishes no payload signature, so accepting a signing_secret would
// leave the operator believing payloads are verified when nothing
// verifies them.
func TestBrevoAdapter_SigningSecretRefused(t *testing.T) {
	_, known, err := BuildAdapter("brevo", ProviderAuthConfig{SigningSecret: "whsec_something"})
	if !known {
		t.Fatal("brevo adapter not registered")
	}
	if err == nil {
		t.Fatal("signing_secret accepted; Brevo does not sign webhooks")
	}
	if !strings.Contains(err.Error(), "does not sign") {
		t.Errorf("error = %q, want it to say Brevo does not sign", err)
	}
}

// TestBrevoAdapter_SourceIPAllowlist covers the one check Brevo's
// documented mechanisms actually support.
func TestBrevoAdapter_SourceIPAllowlist(t *testing.T) {
	a := brevoTestAdapter(t, ProviderAuthConfig{AllowedIPs: []string{"1.179.112.0/20"}})

	for _, tc := range []struct {
		from    string
		allowed bool
	}{
		{"1.179.112.5", true},
		{"1.179.127.255", true},
		{"1.179.128.1", false},
		{"203.0.113.7", false},
	} {
		req := httptest.NewRequest(http.MethodPost, "/events/brevo", nil)
		req.RemoteAddr = tc.from + ":54321"
		err := a.Verify(req, nil)
		if tc.allowed && err != nil {
			t.Errorf("%s rejected: %v", tc.from, err)
		}
		if !tc.allowed && err == nil {
			t.Errorf("%s accepted, want rejection", tc.from)
		}
	}
}

// TestBrevoAdapter_NoAllowlistRelesOnBasicAuth documents the boundary
// rather than hiding it: with no allowlist configured, Verify passes and
// the endpoint's mandatory basic auth — enforced by the handler before
// Verify runs — is the only authentication.
func TestBrevoAdapter_NoAllowlistReliesOnBasicAuth(t *testing.T) {
	a := brevoTestAdapter(t, ProviderAuthConfig{})
	req := httptest.NewRequest(http.MethodPost, "/events/brevo", nil)
	req.RemoteAddr = "203.0.113.7:54321"
	if err := a.Verify(req, nil); err != nil {
		t.Errorf("Verify with no allowlist = %v, want nil", err)
	}
}

// TestBrevoAdapter_NoInventedSignatureHeader: a signature header an
// attacker supplies must not be treated as authentication, because
// Brevo never sends one.
func TestBrevoAdapter_NoInventedSignatureHeader(t *testing.T) {
	a := brevoTestAdapter(t, ProviderAuthConfig{AllowedIPs: []string{"1.179.112.0/20"}})
	req := httptest.NewRequest(http.MethodPost, "/events/brevo", nil)
	req.RemoteAddr = "203.0.113.7:54321"
	req.Header.Set("X-Brevo-Signature", "anything")
	req.Header.Set("svix-signature", "v1,anything")
	if err := a.Verify(req, nil); err == nil {
		t.Error("a supplied signature header bypassed the source-IP check")
	}
}
