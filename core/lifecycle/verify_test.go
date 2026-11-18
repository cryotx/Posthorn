package lifecycle

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// testResendSecret is a Svix-format webhook secret: "whsec_" followed
// by base64. The value is a fixture, not a real credential.
const testResendSecret = "whsec_cG9zdGhvcm4tbG9jYWwtdGVzdC1zZWNyZXQ="

var verifyNow = time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)

// svixRequest builds a request signed exactly as the Standard Webhooks
// spec describes, independently of the implementation under test:
// content is "{id}.{timestamp}.{body}", HMAC-SHA256 under the
// base64-decoded secret, base64 signature, "v1," version prefix.
func svixRequest(t *testing.T, secret, id string, ts time.Time, body string) *http.Request {
	t.Helper()
	key, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(secret, "whsec_"))
	if err != nil {
		t.Fatalf("decode fixture secret: %v", err)
	}
	stamp := strconv.FormatInt(ts.Unix(), 10)
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(id + "." + stamp + "." + body))
	sig := base64.StdEncoding.EncodeToString(mac.Sum(nil))

	req := httptest.NewRequest(http.MethodPost, "/events/resend/unified", strings.NewReader(body))
	req.Header.Set("svix-id", id)
	req.Header.Set("svix-timestamp", stamp)
	req.Header.Set("svix-signature", "v1,"+sig)
	return req
}

func resendVerifier(t *testing.T) resendAdapter {
	t.Helper()
	a, known, err := BuildAdapter("resend", ProviderAuthConfig{SigningSecret: testResendSecret})
	if err != nil || !known {
		t.Fatalf("BuildAdapter: known=%v err=%v", known, err)
	}
	ra := a.(resendAdapter)
	ra.now = func() time.Time { return verifyNow }
	return ra
}

func reason(t *testing.T, err error) string {
	t.Helper()
	var ve *VerifyError
	if !errors.As(err, &ve) {
		t.Fatalf("error is %T, want *VerifyError: %v", err, err)
	}
	return ve.Reason
}

// --- Resend / Svix -----------------------------------------------------

func TestResendVerify_ValidSignature(t *testing.T) {
	a := resendVerifier(t)
	body := `{"type":"email.delivered","data":{"email_id":"re-1"}}`
	req := svixRequest(t, testResendSecret, "msg_2abc", verifyNow, body)

	if err := a.Verify(req, []byte(body)); err != nil {
		t.Fatalf("Verify: %v", err)
	}
}

func TestResendVerify_RejectsForgedSignature(t *testing.T) {
	a := resendVerifier(t)
	body := `{"type":"email.delivered","data":{"email_id":"re-1"}}`
	req := svixRequest(t, testResendSecret, "msg_2abc", verifyNow, body)
	req.Header.Set("svix-signature", "v1,"+base64.StdEncoding.EncodeToString([]byte("not-the-real-signature-32bytes!!")))

	err := a.Verify(req, []byte(body))
	if err == nil {
		t.Fatal("a forged signature verified")
	}
	if got := reason(t, err); got != ReasonBadSignature {
		t.Errorf("reason = %q, want %q", got, ReasonBadSignature)
	}
}

// A signature signed with a different secret must not verify — the
// check must actually use the configured key.
func TestResendVerify_RejectsWrongSecret(t *testing.T) {
	a := resendVerifier(t)
	body := `{"type":"email.delivered"}`
	req := svixRequest(t, "whsec_"+base64.StdEncoding.EncodeToString([]byte("a-completely-different-key!!")), "msg_1", verifyNow, body)

	if err := a.Verify(req, []byte(body)); err == nil {
		t.Fatal("a signature from another secret verified")
	}
}

// The headline property: the signature covers the body, so a body
// modified in flight cannot survive with the original signature.
func TestResendVerify_RejectsTamperedBody(t *testing.T) {
	a := resendVerifier(t)
	original := `{"type":"email.delivered","data":{"email_id":"re-1"}}`
	req := svixRequest(t, testResendSecret, "msg_2abc", verifyNow, original)

	tampered := `{"type":"email.bounced","data":{"email_id":"re-1"}}`
	err := a.Verify(req, []byte(tampered))
	if err == nil {
		t.Fatal("a tampered body verified under the original signature")
	}
	if got := reason(t, err); got != ReasonBadSignature {
		t.Errorf("reason = %q, want %q", got, ReasonBadSignature)
	}
}

// Even a semantically identical re-serialisation breaks the signature,
// which is precisely why verification must use the raw bytes and never
// decode-then-re-encode.
func TestResendVerify_RawBytesMatter(t *testing.T) {
	a := resendVerifier(t)
	raw := `{"type":"email.delivered","data":{"email_id":"re-1"}}`
	req := svixRequest(t, testResendSecret, "msg_2abc", verifyNow, raw)

	if err := a.Verify(req, []byte(raw)); err != nil {
		t.Fatalf("raw bytes failed to verify: %v", err)
	}
	// The same JSON with whitespace — what a decode/re-encode round trip
	// would produce — must fail.
	reencoded := `{"type": "email.delivered", "data": {"email_id": "re-1"}}`
	if err := a.Verify(req, []byte(reencoded)); err == nil {
		t.Fatal("a re-serialised body verified; verification is not using raw bytes")
	}
}

func TestResendVerify_TimestampWindow(t *testing.T) {
	a := resendVerifier(t)
	body := `{"type":"email.delivered"}`

	for _, tc := range []struct {
		name    string
		offset  time.Duration
		wantErr bool
	}{
		{"now", 0, false},
		{"4 minutes old", -4 * time.Minute, false},
		{"4 minutes ahead (clock skew)", 4 * time.Minute, false},
		{"6 minutes old — replay", -6 * time.Minute, true},
		{"an hour old — replay", -time.Hour, true},
		{"6 minutes ahead", 6 * time.Minute, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := svixRequest(t, testResendSecret, "msg_1", verifyNow.Add(tc.offset), body)
			err := a.Verify(req, []byte(body))
			if tc.wantErr {
				if err == nil {
					t.Fatal("an out-of-window timestamp verified")
				}
				if got := reason(t, err); got != ReasonTimestampWindow {
					t.Errorf("reason = %q, want %q", got, ReasonTimestampWindow)
				}
				return
			}
			if err != nil {
				t.Fatalf("Verify: %v", err)
			}
		})
	}
}

func TestResendVerify_MissingHeaders(t *testing.T) {
	a := resendVerifier(t)
	body := `{"type":"email.delivered"}`

	for _, drop := range []string{"svix-id", "svix-timestamp", "svix-signature"} {
		req := svixRequest(t, testResendSecret, "msg_1", verifyNow, body)
		req.Header.Del(drop)
		err := a.Verify(req, []byte(body))
		if err == nil {
			t.Fatalf("verified with %s missing", drop)
		}
		if got := reason(t, err); got != ReasonMissingHeaders {
			t.Errorf("dropping %s gave reason %q, want %q", drop, got, ReasonMissingHeaders)
		}
	}
}

func TestResendVerify_BadTimestampFormat(t *testing.T) {
	a := resendVerifier(t)
	body := `{"type":"email.delivered"}`
	req := svixRequest(t, testResendSecret, "msg_1", verifyNow, body)
	req.Header.Set("svix-timestamp", "2026-09-08T12:00:00Z")

	err := a.Verify(req, []byte(body))
	if got := reason(t, err); got != ReasonBadTimestamp {
		t.Errorf("reason = %q, want %q", got, ReasonBadTimestamp)
	}
}

// The spec allows several signatures during a key rotation. One valid
// entry among others must verify, and unknown version prefixes must be
// skipped rather than rejected.
func TestResendVerify_MultipleSignaturesAndVersions(t *testing.T) {
	a := resendVerifier(t)
	body := `{"type":"email.delivered"}`
	req := svixRequest(t, testResendSecret, "msg_1", verifyNow, body)
	valid := req.Header.Get("svix-signature")

	req.Header.Set("svix-signature",
		"v1,"+base64.StdEncoding.EncodeToString([]byte("a-wrong-signature-value-here!!!!"))+
			" v1a,Zm9yd2FyZC1jb21wYXRpYmxlLXNjaGVtZQ== "+valid)
	if err := a.Verify(req, []byte(body)); err != nil {
		t.Fatalf("a valid signature among others failed: %v", err)
	}

	// ...but ONLY an unknown version is still a rejection.
	req.Header.Set("svix-signature", "v1a,Zm9yd2FyZC1jb21wYXRpYmxlLXNjaGVtZQ==")
	if err := a.Verify(req, []byte(body)); err == nil {
		t.Fatal("a payload with no v1 signature verified")
	}
}

// The Standard Webhooks spec defines vendor-neutral header aliases.
func TestResendVerify_AcceptsWebhookHeaderAliases(t *testing.T) {
	a := resendVerifier(t)
	body := `{"type":"email.delivered"}`
	req := svixRequest(t, testResendSecret, "msg_1", verifyNow, body)

	for _, pair := range [][2]string{
		{"svix-id", "webhook-id"},
		{"svix-timestamp", "webhook-timestamp"},
		{"svix-signature", "webhook-signature"},
	} {
		req.Header.Set(pair[1], req.Header.Get(pair[0]))
		req.Header.Del(pair[0])
	}
	if err := a.Verify(req, []byte(body)); err != nil {
		t.Fatalf("webhook-* aliases rejected: %v", err)
	}
}

// An adapter with no secret must refuse, not pass. Accepting would be
// the looks-configured-but-isn't footgun.
func TestResendVerify_UnconfiguredRefuses(t *testing.T) {
	a := resendAdapter{tolerance: SvixTolerance, now: func() time.Time { return verifyNow }}
	req := httptest.NewRequest(http.MethodPost, "/events/resend/unified", nil)
	err := a.Verify(req, []byte("{}"))
	if got := reason(t, err); got != ReasonNotConfigured {
		t.Errorf("reason = %q, want %q", got, ReasonNotConfigured)
	}
}

func TestBuildAdapter_ResendRequiresUsableSecret(t *testing.T) {
	for _, secret := range []string{"", "whsec_not-base64!!!", "not-base64-either!!!"} {
		if _, _, err := BuildAdapter("resend", ProviderAuthConfig{SigningSecret: secret}); err == nil {
			t.Errorf("BuildAdapter accepted secret %q", secret)
		}
	}
	// Both padded and unpadded base64 are accepted: secrets get copied
	// by hand and a trailing "=" is easy to lose.
	for _, secret := range []string{testResendSecret, strings.TrimRight(testResendSecret, "=")} {
		if _, _, err := BuildAdapter("resend", ProviderAuthConfig{SigningSecret: secret}); err != nil {
			t.Errorf("BuildAdapter(%q): %v", secret, err)
		}
	}
}

// --- Postmark ----------------------------------------------------------

func postmarkVerifier(t *testing.T, cfg ProviderAuthConfig) ProviderWebhookAdapter {
	t.Helper()
	a, known, err := BuildAdapter("postmark", cfg)
	if err != nil || !known {
		t.Fatalf("BuildAdapter: known=%v err=%v", known, err)
	}
	return a
}

func postmarkRequest(from string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/events/postmark/unified",
		strings.NewReader(`{"RecordType":"Delivery","MessageID":"pm-1"}`))
	req.RemoteAddr = from + ":54321"
	return req
}

// Postmark does not sign its payloads (confirmed against its developer
// documentation). With no allowlist configured, the endpoint's
// mandatory basic auth is the whole gate, and Verify says so by
// passing rather than by inventing a signature check.
func TestPostmarkVerify_NoAllowlistRelliesOnBasicAuth(t *testing.T) {
	a := postmarkVerifier(t, ProviderAuthConfig{})
	if err := a.Verify(postmarkRequest("203.0.113.9"), []byte("{}")); err != nil {
		t.Errorf("Verify = %v, want nil when no allowlist is configured", err)
	}
}

func TestPostmarkVerify_IPAllowlist(t *testing.T) {
	a := postmarkVerifier(t, ProviderAuthConfig{AllowedIPs: PostmarkWebhookIPs})

	for _, ip := range PostmarkWebhookIPs {
		if err := a.Verify(postmarkRequest(ip), []byte("{}")); err != nil {
			t.Errorf("published Postmark address %s rejected: %v", ip, err)
		}
	}
	err := a.Verify(postmarkRequest("203.0.113.9"), []byte("{}"))
	if err == nil {
		t.Fatal("an address outside the allowlist was accepted")
	}
	if got := reason(t, err); got != ReasonSourceNotAllowed {
		t.Errorf("reason = %q, want %q", got, ReasonSourceNotAllowed)
	}
}

func TestPostmarkVerify_CIDRAllowlist(t *testing.T) {
	a := postmarkVerifier(t, ProviderAuthConfig{AllowedIPs: []string{"50.31.156.0/24"}})
	if err := a.Verify(postmarkRequest("50.31.156.77"), []byte("{}")); err != nil {
		t.Errorf("in-CIDR address rejected: %v", err)
	}
	if err := a.Verify(postmarkRequest("50.31.157.1"), []byte("{}")); err == nil {
		t.Error("an out-of-CIDR address was accepted")
	}
}

// Trusting X-Forwarded-For unconditionally would make the allowlist
// forgeable by anyone who can set a header.
func TestPostmarkVerify_ForwardedForOnlyFromTrustedProxy(t *testing.T) {
	a := postmarkVerifier(t, ProviderAuthConfig{
		AllowedIPs:     []string{"3.134.147.250"},
		TrustedProxies: []string{"10.0.0.0/8"},
	})

	// From an untrusted peer, the header is ignored and the peer loses.
	forged := postmarkRequest("203.0.113.9")
	forged.Header.Set("X-Forwarded-For", "3.134.147.250")
	if err := a.Verify(forged, []byte("{}")); err == nil {
		t.Error("a forged X-Forwarded-For was trusted from an untrusted peer")
	}

	// From a trusted proxy, the header is read.
	proxied := postmarkRequest("10.1.2.3")
	proxied.Header.Set("X-Forwarded-For", "3.134.147.250")
	if err := a.Verify(proxied, []byte("{}")); err != nil {
		t.Errorf("a trusted proxy's X-Forwarded-For was ignored: %v", err)
	}
}

// An operator who sets signing_secret for Postmark believes payloads
// are being verified. Postmark does not sign them, so accepting the
// setting would leave exactly that false confidence.
func TestBuildAdapter_PostmarkRefusesSigningSecret(t *testing.T) {
	_, _, err := BuildAdapter("postmark", ProviderAuthConfig{SigningSecret: "whsec_anything"})
	if err == nil {
		t.Fatal("Postmark accepted a signing_secret it cannot use")
	}
	if !strings.Contains(err.Error(), "does not sign") {
		t.Errorf("error = %v, want it to explain that Postmark does not sign", err)
	}
}

func TestBuildAdapter_RejectsBadAllowlist(t *testing.T) {
	for _, entry := range []string{"not-an-ip", "999.1.1.1", "10.0.0.0/99"} {
		if _, _, err := BuildAdapter("postmark", ProviderAuthConfig{AllowedIPs: []string{entry}}); err == nil {
			t.Errorf("BuildAdapter accepted allowlist entry %q", entry)
		}
	}
}

// NFR48: a verification failure may name the reason, never the secret,
// the signature or the payload.
func TestVerifyError_CarriesNoSecrets(t *testing.T) {
	a := resendVerifier(t)
	body := `{"type":"email.delivered","data":{"email_id":"SENSITIVE-PAYLOAD-VALUE"}}`
	req := svixRequest(t, testResendSecret, "msg_1", verifyNow, body)
	req.Header.Set("svix-signature", "v1,"+base64.StdEncoding.EncodeToString([]byte("wrong-signature-here-32-bytes!!!")))

	err := a.Verify(req, []byte(body))
	msg := err.Error()
	for _, forbidden := range []string{testResendSecret, "cG9zdGhvcm4tbG9jYWwtdGVzdC1zZWNyZXQ=", "SENSITIVE-PAYLOAD-VALUE", "wrong-signature"} {
		if strings.Contains(msg, forbidden) {
			t.Errorf("verification error leaked %q: %s", forbidden, msg)
		}
	}
}

// --- Postmark replay identity (FR129) ----------------------------------

const postmarkDelivery = `{"RecordType":"Delivery","MessageID":"pm-1","Recipient":"a@example.net","DeliveredAt":"2026-09-08T12:00:00Z"}`

func postmarkParse(t *testing.T, a ProviderWebhookAdapter, traceID, body string) DeliveryEvent {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/events/postmark/unified", strings.NewReader(body))
	if traceID != "" {
		req.Header.Set(PostmarkTraceHeader, traceID)
	}
	rp, ok := a.(RequestParser)
	if !ok {
		t.Fatal("postmark adapter does not implement RequestParser")
	}
	raws, err := rp.ParseRequest(req, []byte(body))
	if err != nil {
		t.Fatalf("ParseRequest: %v", err)
	}
	ev, err := a.Normalize(raws[0], verifyNow)
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	return ev
}

// Postmark's own trace id is stable across retries of one event, which
// makes it the right dedup identity when present.
func TestPostmark_TraceIDIsTheReplayKey(t *testing.T) {
	a := postmarkVerifier(t, ProviderAuthConfig{})

	first := postmarkParse(t, a, "trace-abc", postmarkDelivery)
	retry := postmarkParse(t, a, "trace-abc", postmarkDelivery)
	if first.ProviderEventKey != retry.ProviderEventKey {
		t.Errorf("a retry of one event produced different keys: %q vs %q",
			first.ProviderEventKey, retry.ProviderEventKey)
	}
	if !strings.HasPrefix(first.ProviderEventKey, "pm_") {
		t.Errorf("key = %q, want the trace id to be used", first.ProviderEventKey)
	}
}

// Two genuinely distinct events carry different trace ids even when
// their payloads are identical — a message delivered to the same
// recipient twice is two facts, not one.
func TestPostmark_DifferentTraceIDsAreDistinct(t *testing.T) {
	a := postmarkVerifier(t, ProviderAuthConfig{})

	one := postmarkParse(t, a, "trace-1", postmarkDelivery)
	two := postmarkParse(t, a, "trace-2", postmarkDelivery)
	if one.ProviderEventKey == two.ProviderEventKey {
		t.Error("two distinct trace ids collapsed to one key")
	}
}

// Without a trace id the derived key still collapses redeliveries.
func TestPostmark_FallbackKeyWithoutTraceID(t *testing.T) {
	a := postmarkVerifier(t, ProviderAuthConfig{})

	first := postmarkParse(t, a, "", postmarkDelivery)
	retry := postmarkParse(t, a, "", postmarkDelivery)
	if first.ProviderEventKey == "" {
		t.Fatal("no fallback key was derived")
	}
	if !strings.HasPrefix(first.ProviderEventKey, "d_") {
		t.Errorf("key = %q, want a derived fallback", first.ProviderEventKey)
	}
	if first.ProviderEventKey != retry.ProviderEventKey {
		t.Error("the derived key is not stable across an identical redelivery")
	}

	// A different recipient is a different event.
	other := postmarkParse(t, a, "",
		strings.Replace(postmarkDelivery, "a@example.net", "b@example.net", 1))
	if other.ProviderEventKey == first.ProviderEventKey {
		t.Error("events for different recipients collapsed to one key")
	}
}

// The trace id is unauthenticated, so it must not be usable to write
// an unbounded value into the dedup index.
func TestPostmark_OversizeTraceIDIsIgnored(t *testing.T) {
	a := postmarkVerifier(t, ProviderAuthConfig{})
	ev := postmarkParse(t, a, strings.Repeat("x", 500), postmarkDelivery)
	if strings.HasPrefix(ev.ProviderEventKey, "pm_") {
		t.Error("an oversize trace id was accepted as the dedup key")
	}
}

// Postmark events are recipient-scoped; Resend events are not. The
// adapters must reflect what each provider actually sends.
func TestAdapters_RecipientScoping(t *testing.T) {
	pm := postmarkVerifier(t, ProviderAuthConfig{})
	ev := postmarkParse(t, pm, "trace-1", postmarkDelivery)
	if ev.Recipient != "a@example.net" {
		t.Errorf("postmark Recipient = %q, want the single named recipient", ev.Recipient)
	}

	rs := testAdapter(t, "resend")
	raws, err := rs.Parse([]byte(`{"type":"email.delivered","data":{"email_id":"re-1","to":["a@example.net","b@example.net"]}}`))
	if err != nil {
		t.Fatal(err)
	}
	rev, err := rs.Normalize(raws[0], verifyNow)
	if err != nil {
		t.Fatal(err)
	}
	if rev.Recipient != "" {
		t.Errorf("resend Recipient = %q, want empty — its events are message-scoped", rev.Recipient)
	}
}

// The recipient address must not leave Posthorn in an event payload.
func TestAdapters_RecipientNotSerialised(t *testing.T) {
	a := postmarkVerifier(t, ProviderAuthConfig{})
	ev := postmarkParse(t, a, "trace-1", postmarkDelivery)
	body, err := ev.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "a@example.net") {
		t.Errorf("the recipient address reached the outbound event: %s", body)
	}
	if ev.Metadata["recipient_domain"] != "example.net" {
		t.Errorf("recipient_domain = %q", ev.Metadata["recipient_domain"])
	}
}
