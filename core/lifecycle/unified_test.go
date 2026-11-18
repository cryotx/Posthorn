package lifecycle

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

var evNow = time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)

// --- event identity ----------------------------------------------------

func TestNewEventID_UniqueAndPrefixed(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 1000; i++ {
		id := NewEventID()
		if !strings.HasPrefix(id, "ev_") {
			t.Fatalf("id %q has no ev_ prefix", id)
		}
		if seen[id] {
			t.Fatalf("duplicate event id %q", id)
		}
		seen[id] = true
	}
}

// The event id and timestamp live INSIDE the signed body, which is what
// makes a receiver's replay check meaningful — a header alone could be
// rewritten without invalidating the signature.
func TestDeliveryEvent_IdentityIsInsideTheSignedBody(t *testing.T) {
	ev := DeliveryEvent{
		EventID: "ev_abc", MessageID: "m1", Project: "novel",
		Type: TypeDelivered, Timestamp: evNow,
	}
	body, err := ev.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"event_id":"ev_abc"`) {
		t.Errorf("event_id missing from the signed body: %s", body)
	}
	if !strings.Contains(string(body), `"timestamp"`) {
		t.Errorf("timestamp missing from the signed body: %s", body)
	}
}

// --- adapters ----------------------------------------------------------

// testAdapter builds a configured adapter for tests. Adapters now carry
// their authentication configuration, so there is no stateless instance
// to look up.
func testAdapter(t *testing.T, name string) ProviderWebhookAdapter {
	t.Helper()
	cfg := ProviderAuthConfig{}
	if name == "resend" {
		cfg.SigningSecret = testResendSecret
	}
	a, known, err := BuildAdapter(name, cfg)
	if err != nil || !known {
		t.Fatalf("BuildAdapter(%q): known=%v err=%v", name, known, err)
	}
	return a
}

func TestAdapterRegistry(t *testing.T) {
	names := AdapterNames()
	if len(names) < 3 {
		t.Fatalf("adapters = %v, want at least brevo, postmark and resend", names)
	}
	for _, want := range []string{"brevo", "postmark", "resend"} {
		if a := testAdapter(t, want); a.Name() != want {
			t.Errorf("adapter name = %q, want %q", a.Name(), want)
		}
	}
	if _, known, _ := BuildAdapter("nonexistent", ProviderAuthConfig{}); known {
		t.Error("BuildAdapter reported an unregistered dialect as known")
	}
}

// Two providers, two payload shapes, one unified event type. That is
// the whole point of the adapter layer.
func TestAdapters_NormalizeToTheSameUnifiedType(t *testing.T) {
	postmark := testAdapter(t, "postmark")
	resend := testAdapter(t, "resend")

	pmRaw, err := postmark.Parse([]byte(`{"RecordType":"Delivery","MessageID":"pm-1","Recipient":"a@example.com","DeliveredAt":"2026-09-07T12:00:00Z"}`))
	if err != nil {
		t.Fatal(err)
	}
	pmEv, err := postmark.Normalize(pmRaw[0], evNow)
	if err != nil {
		t.Fatal(err)
	}

	reRaw, err := resend.Parse([]byte(`{"type":"email.delivered","created_at":"2026-09-07T12:00:00Z","data":{"email_id":"re-1","to":["a@example.com"]}}`))
	if err != nil {
		t.Fatal(err)
	}
	reEv, err := resend.Normalize(reRaw[0], evNow)
	if err != nil {
		t.Fatal(err)
	}

	if pmEv.Type != TypeDelivered || reEv.Type != TypeDelivered {
		t.Fatalf("types = %q and %q, want both message.delivered", pmEv.Type, reEv.Type)
	}
	if pmEv.ProviderMessageID != "pm-1" || reEv.ProviderMessageID != "re-1" {
		t.Errorf("provider ids = %q, %q", pmEv.ProviderMessageID, reEv.ProviderMessageID)
	}
	if pmEv.Provider != "postmark" || reEv.Provider != "resend" {
		t.Errorf("providers = %q, %q", pmEv.Provider, reEv.Provider)
	}
}

func TestAdapters_UnifiedTypeMapping(t *testing.T) {
	resend := testAdapter(t, "resend")
	for _, tc := range []struct {
		providerType string
		want         EventType
	}{
		{"email.sent", TypeSent},
		{"email.delivered", TypeDelivered},
		{"email.delivery_delayed", TypeDeferred},
		{"email.bounced", TypeBounced},
		{"email.complained", TypeComplained},
		{"email.opened", TypeOther},
		{"something.brand.new", TypeOther},
	} {
		raw, err := resend.Parse([]byte(`{"type":"` + tc.providerType + `","data":{"email_id":"x"}}`))
		if err != nil {
			t.Fatal(err)
		}
		ev, err := resend.Normalize(raw[0], evNow)
		if err != nil {
			t.Fatalf("%s: %v", tc.providerType, err)
		}
		if ev.Type != tc.want {
			t.Errorf("%s → %q, want %q", tc.providerType, ev.Type, tc.want)
		}
	}

	postmark := testAdapter(t, "postmark")
	for _, tc := range []struct {
		payload string
		want    EventType
	}{
		{`{"RecordType":"Delivery","MessageID":"1"}`, TypeDelivered},
		{`{"RecordType":"Bounce","Type":"HardBounce","MessageID":"1"}`, TypeBounced},
		{`{"RecordType":"Bounce","Type":"SoftBounce","MessageID":"1"}`, TypeDeferred},
		{`{"RecordType":"SpamComplaint","MessageID":"1"}`, TypeComplained},
		{`{"RecordType":"Open","MessageID":"1"}`, TypeOther},
	} {
		raw, _ := postmark.Parse([]byte(tc.payload))
		ev, err := postmark.Normalize(raw[0], evNow)
		if err != nil {
			t.Fatalf("%s: %v", tc.payload, err)
		}
		if ev.Type != tc.want {
			t.Errorf("%s → %q, want %q", tc.payload, ev.Type, tc.want)
		}
	}
}

// A normalized event carries a recipient DOMAIN, never the address: an
// event leaving Posthorn should let a receiver route it, not hand them
// a harvestable recipient list (NFR44).
func TestAdapters_MetadataCarriesDomainNotAddress(t *testing.T) {
	for _, tc := range []struct{ adapter, payload string }{
		{"postmark", `{"RecordType":"Delivery","MessageID":"1","Recipient":"secret.user@example.com"}`},
		{"resend", `{"type":"email.delivered","data":{"email_id":"1","to":["secret.user@example.com"]}}`},
		{"brevo", `{"event":"delivered","message-id":"1","email":"secret.user@example.com"}`},
	} {
		a := testAdapter(t, tc.adapter)
		raw, _ := a.Parse([]byte(tc.payload))
		ev, err := a.Normalize(raw[0], evNow)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := ev.Encode()
		if strings.Contains(string(body), "secret.user@") {
			t.Errorf("%s: the recipient address reached the event body: %s", tc.adapter, body)
		}
		if ev.Metadata["recipient_domain"] != "example.com" {
			t.Errorf("%s: recipient_domain = %q", tc.adapter, ev.Metadata["recipient_domain"])
		}
	}
}

func TestAdapters_MalformedPayloadIsAnError(t *testing.T) {
	for _, name := range []string{"brevo", "postmark", "resend"} {
		a := testAdapter(t, name)
		if _, err := a.Parse(nil); err == nil {
			t.Errorf("%s: Parse(nil) = nil error", name)
		}
		if _, err := a.Normalize(RawEvent{Payload: []byte("not json")}, evNow); err == nil {
			t.Errorf("%s: Normalize of garbage = nil error", name)
		}
	}
}

// --- outbound signature ------------------------------------------------

func TestSign_ReceiverCanVerify(t *testing.T) {
	body := []byte(`{"event_id":"ev_1","type":"message.delivered"}`)
	const secret = "project-webhook-secret-value"

	got := Sign(body, secret)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	want := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	if got != want {
		t.Errorf("Sign = %q, want %q", got, want)
	}
	// A tampered body must not verify.
	if Sign([]byte(`{"event_id":"ev_1","type":"message.bounced"}`), secret) == got {
		t.Error("signature did not change with the body")
	}
	// A different secret must not verify.
	if Sign(body, "other-secret") == got {
		t.Error("signature did not change with the secret")
	}
}

func TestForwarder_SignsAndStampsEventIdentity(t *testing.T) {
	var gotSig, gotID, gotTS string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotSig = r.Header.Get(SignatureHeader)
		gotID = r.Header.Get(EventIDHeader)
		gotTS = r.Header.Get(TimestampHeader)
		buf := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(buf)
		gotBody = buf
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	f := &Forwarder{
		Webhooks: map[string]Webhook{
			"project:novel": {URL: srv.URL, Secret: "project-webhook-secret-value"},
		},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	ev := DeliveryEvent{EventID: "ev_abc", MessageID: "m1", Project: "novel",
		Type: TypeDelivered, Timestamp: evNow}
	payload, _ := ev.Encode()
	f.DeliverEvent(t.Context(), "project:novel", ev.EventID, ev.Timestamp, payload)

	if gotID != "ev_abc" {
		t.Errorf("%s = %q", EventIDHeader, gotID)
	}
	if gotTS != evNow.Format(time.RFC3339) {
		t.Errorf("%s = %q", TimestampHeader, gotTS)
	}
	if gotSig != Sign(gotBody, "project-webhook-secret-value") {
		t.Errorf("signature does not verify over the delivered body")
	}
	var decoded DeliveryEvent
	if err := json.Unmarshal(gotBody, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Project != "novel" || decoded.Type != TypeDelivered {
		t.Errorf("delivered event = %+v", decoded)
	}
}

// A queued redelivery must carry the SAME event id, or a receiver's
// dedupe would treat a retry as a new event.
func TestEventIdentity_SurvivesTheQueue(t *testing.T) {
	ev := DeliveryEvent{EventID: "ev_stable", Type: TypeBounced, Timestamp: evNow}
	payload, _ := ev.Encode()

	id, ts := eventIdentity(payload)
	if id != "ev_stable" {
		t.Errorf("recovered id = %q", id)
	}
	if !ts.Equal(evNow) {
		t.Errorf("recovered timestamp = %v, want %v", ts, evNow)
	}
	// A pre-unified payload simply has no identity, and that is fine.
	id, ts = eventIdentity([]byte(`{"event":"delivered"}`))
	if id != "" || !ts.IsZero() {
		t.Errorf("legacy payload produced identity %q / %v", id, ts)
	}
}
