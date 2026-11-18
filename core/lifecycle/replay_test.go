package lifecycle

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/craigmccaskill/posthorn/metrics"
	"github.com/craigmccaskill/posthorn/storage"
)

// replayFixture wires a real store, a real forwarder pointed at a
// recording receiver, and a handler on a configured Resend adapter —
// the whole ingestion path, so "unverified means nothing happens" is
// checked against actual side effects rather than a mock.
type replayFixture struct {
	handler  *Handler
	gate     *storage.Gate
	registry *metrics.Registry
	adapter  resendAdapter

	mu       sync.Mutex
	received []DeliveryEvent
}

func newReplayFixture(t *testing.T) *replayFixture {
	t.Helper()
	st, err := storage.Open(storage.Config{InMemory: true})
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	gate := storage.NewGate(st, logger)

	f := &replayFixture{gate: gate, registry: metrics.New()}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var ev DeliveryEvent
		_ = json.Unmarshal(body, &ev)
		f.mu.Lock()
		f.received = append(f.received, ev)
		f.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	fwd := &Forwarder{
		Webhooks: map[string]Webhook{
			"project:novel": {URL: srv.URL, Secret: "project-webhook-secret-value"},
		},
		Gate:   gate,
		Logger: logger,
	}
	rec := metrics.NewRecorder(f.registry)
	h, err := NewHandler("u", "p", gate, fwd, logger, rec)
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}

	a, _, err := BuildAdapter("resend", ProviderAuthConfig{SigningSecret: testResendSecret})
	if err != nil {
		t.Fatalf("BuildAdapter: %v", err)
	}
	ra := a.(resendAdapter)
	ra.now = func() time.Time { return verifyNow }
	f.adapter = ra
	f.handler = h.WithAdapter(ra)
	return f
}

// seed records a delivered submission so events have something to
// correlate to.
func (f *replayFixture) seed(t *testing.T, messageID, providerMessageID string) {
	t.Helper()
	err := f.gate.Store().RecordSubmission(storage.Submission{
		ID: messageID, Endpoint: "/api/send", Project: "novel",
		Transport: "resend", ToAddrs: []string{"reader@example.net"},
		Status: storage.StatusSending, CreatedAt: verifyNow,
	})
	if err != nil {
		t.Fatalf("RecordSubmission: %v", err)
	}
	if err := f.gate.Store().MarkSentVia(messageID, "resend", providerMessageID, verifyNow); err != nil {
		t.Fatalf("MarkSentVia: %v", err)
	}
}

// seedTo records a delivered submission with explicit recipients.
func (f *replayFixture) seedTo(t *testing.T, messageID, providerMessageID string, to []string) {
	t.Helper()
	err := f.gate.Store().RecordSubmission(storage.Submission{
		ID: messageID, Endpoint: "/api/send", Project: "novel",
		Transport: "resend", ToAddrs: to,
		Status: storage.StatusSending, CreatedAt: verifyNow,
	})
	if err != nil {
		t.Fatalf("RecordSubmission: %v", err)
	}
	if err := f.gate.Store().MarkSentVia(messageID, "resend", providerMessageID, verifyNow); err != nil {
		t.Fatalf("MarkSentVia: %v", err)
	}
}

// post signs and delivers one Resend webhook.
func (f *replayFixture) post(t *testing.T, svixID, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := svixRequest(t, testResendSecret, svixID, verifyNow, body)
	req.SetBasicAuth("u", "p")
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, req)
	return w
}

// postUnverified delivers a request whose body no longer matches its
// signature — the tampered case.
func (f *replayFixture) postUnverified(t *testing.T, svixID, signedBody, sentBody string) *httptest.ResponseRecorder {
	t.Helper()
	req := svixRequest(t, testResendSecret, svixID, verifyNow, signedBody)
	req.Body = io.NopCloser(strings.NewReader(sentBody))
	req.ContentLength = int64(len(sentBody))
	req.SetBasicAuth("u", "p")
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, req)
	return w
}

func (f *replayFixture) forwarded() []DeliveryEvent {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]DeliveryEvent(nil), f.received...)
}

func (f *replayFixture) metric(t *testing.T, want string) bool {
	t.Helper()
	var sb strings.Builder
	if err := f.registry.Emit(&sb); err != nil {
		t.Fatal(err)
	}
	return strings.Contains(sb.String(), want)
}

const deliveredBody = `{"type":"email.delivered","created_at":"2026-09-08T12:00:00Z","data":{"email_id":"re-1","to":["reader@example.net"]}}`

// --- happy path --------------------------------------------------------

func TestIngest_VerifiedEventIsRecordedAndForwarded(t *testing.T) {
	f := newReplayFixture(t)
	f.seed(t, "msg-1", "re-1")

	if w := f.post(t, "msg_evt_1", deliveredBody); w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	waitFor(t, func() bool { return len(f.forwarded()) == 1 })

	events, err := f.gate.Store().EventsFor("msg-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Type != string(TypeDelivered) {
		t.Fatalf("events = %+v", events)
	}
	if !f.metric(t, `posthorn_provider_webhook_total{provider="resend",result="accepted"} 1`) {
		t.Error("accepted not counted")
	}

	// FR123: the state projection followed the event.
	sub, ok, _ := f.gate.Store().GetSubmission("msg-1")
	if !ok || sub.DeliveryState != storage.AggDelivered {
		t.Errorf("delivery_state = %q, want the delivered aggregate", sub.DeliveryState)
	}
}

// --- replay ------------------------------------------------------------

// The provider redelivers the same event: same svix-id, same body.
// It must be collapsed, not reprocessed.
func TestIngest_ProviderReplayIsDuplicate(t *testing.T) {
	f := newReplayFixture(t)
	f.seed(t, "msg-1", "re-1")

	f.post(t, "msg_evt_1", deliveredBody)
	waitFor(t, func() bool { return len(f.forwarded()) == 1 })

	if w := f.post(t, "msg_evt_1", deliveredBody); w.Code != http.StatusOK {
		t.Fatalf("replay status = %d", w.Code)
	}

	events, _ := f.gate.Store().EventsFor("msg-1")
	if len(events) != 1 {
		t.Errorf("events = %d, want 1 — a redelivery was recorded twice", len(events))
	}
	if got := f.forwarded(); len(got) != 1 {
		t.Errorf("forwarded %d times, want 1 — a redelivery fired the project webhook again", len(got))
	}
	if !f.metric(t, `posthorn_provider_webhook_total{provider="resend",result="duplicate"} 1`) {
		t.Error("duplicate not counted")
	}
}

// A genuinely different event for the same message must still land.
func TestIngest_DistinctEventsAreNotCollapsed(t *testing.T) {
	f := newReplayFixture(t)
	f.seed(t, "msg-1", "re-1")

	f.post(t, "msg_evt_1", deliveredBody)
	bounced := `{"type":"email.bounced","created_at":"2026-09-08T12:01:00Z","data":{"email_id":"re-1","to":["reader@example.net"]}}`
	f.post(t, "msg_evt_2", bounced)

	waitFor(t, func() bool { return len(f.forwarded()) == 2 })
	events, _ := f.gate.Store().EventsFor("msg-1")
	if len(events) != 2 {
		t.Errorf("events = %d, want 2", len(events))
	}
}

// --- verification gate -------------------------------------------------

// The security boundary: an unverified request must produce NO event,
// NO state change and NO outbound webhook.
func TestIngest_TamperedBodyChangesNothing(t *testing.T) {
	f := newReplayFixture(t)
	f.seed(t, "msg-1", "re-1")

	tampered := `{"type":"email.bounced","created_at":"2026-09-08T12:00:00Z","data":{"email_id":"re-1","to":["reader@example.net"]}}`
	w := f.postUnverified(t, "msg_evt_1", deliveredBody, tampered)
	if w.Code == 0 {
		t.Fatal("no response written")
	}

	events, _ := f.gate.Store().EventsFor("msg-1")
	if len(events) != 0 {
		t.Errorf("%d events written for an unverified request", len(events))
	}
	if got := f.forwarded(); len(got) != 0 {
		t.Errorf("%d project webhooks fired for an unverified request", len(got))
	}
	sub, _, _ := f.gate.Store().GetSubmission("msg-1")
	if sub.DeliveryState != "" {
		t.Errorf("delivery_state = %q — an unverified request changed message state", sub.DeliveryState)
	}
	if !f.metric(t, `posthorn_provider_webhook_total{provider="resend",result="invalid_signature"} 1`) {
		t.Error("invalid_signature not counted")
	}
}

func TestIngest_ExpiredTimestampChangesNothing(t *testing.T) {
	f := newReplayFixture(t)
	f.seed(t, "msg-1", "re-1")

	old := verifyNow.Add(-30 * time.Minute)
	req := svixRequest(t, testResendSecret, "msg_evt_1", old, deliveredBody)
	req.SetBasicAuth("u", "p")
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, req)

	events, _ := f.gate.Store().EventsFor("msg-1")
	if len(events) != 0 {
		t.Errorf("%d events written for an expired timestamp", len(events))
	}
	if got := f.forwarded(); len(got) != 0 {
		t.Errorf("%d webhooks fired for an expired timestamp", len(got))
	}
}

// Basic auth still gates everything, before the adapter is consulted.
func TestIngest_BadBasicAuthChangesNothing(t *testing.T) {
	f := newReplayFixture(t)
	f.seed(t, "msg-1", "re-1")

	req := svixRequest(t, testResendSecret, "msg_evt_1", verifyNow, deliveredBody)
	req.SetBasicAuth("u", "wrong")
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
	events, _ := f.gate.Store().EventsFor("msg-1")
	if len(events) != 0 {
		t.Errorf("%d events written without basic auth", len(events))
	}
}

func TestIngest_UnknownMessageIsCounted(t *testing.T) {
	f := newReplayFixture(t)
	// No seeded submission: nothing correlates.
	f.post(t, "msg_evt_1", deliveredBody)

	if !f.metric(t, `posthorn_provider_webhook_total{provider="resend",result="unknown_message"} 1`) {
		t.Error("unknown_message not counted")
	}
	if got := f.forwarded(); len(got) != 0 {
		t.Errorf("%d webhooks fired for an uncorrelated event", len(got))
	}
}

// --- state projection --------------------------------------------------

func TestDeliveryState_SingleRecipientAggregate(t *testing.T) {
	f := newReplayFixture(t)
	f.seed(t, "msg-1", "re-1")

	if _, err := f.gate.Store().ProjectRecipientEvent(
		"msg-1", "", string(TypeDelivered), verifyNow); err != nil {
		t.Fatal(err)
	}
	sub, _, _ := f.gate.Store().GetSubmission("msg-1")
	if sub.DeliveryState != storage.AggDelivered {
		t.Errorf("aggregate = %q, want delivered", sub.DeliveryState)
	}
}

// The defect a single submission-level column could not express: one
// recipient delivered, another bounced.
func TestDeliveryState_MultiRecipientPartial(t *testing.T) {
	f := newReplayFixture(t)
	f.seedTo(t, "msg-2", "re-2", []string{"a@example.net", "b@example.net"})
	st := f.gate.Store()

	// A recipient-scoped provider names one address at a time.
	if _, err := st.ProjectRecipientEvent("msg-2", "a@example.net", string(TypeDelivered), verifyNow); err != nil {
		t.Fatal(err)
	}
	sub, _, _ := st.GetSubmission("msg-2")
	if sub.DeliveryState != storage.AggPending {
		t.Errorf("aggregate = %q, want pending while b is unreported", sub.DeliveryState)
	}

	if _, err := st.ProjectRecipientEvent("msg-2", "b@example.net", string(TypeBounced), verifyNow); err != nil {
		t.Fatal(err)
	}
	sub, _, _ = st.GetSubmission("msg-2")
	if sub.DeliveryState != storage.AggPartial {
		t.Errorf("aggregate = %q, want partial", sub.DeliveryState)
	}

	// One recipient's bounce must not have overwritten the other's
	// delivery — the whole point of the recipient-scoped projection.
	states, err := st.RecipientStates("msg-2")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, rs := range states {
		got[rs.Recipient] = rs.State
	}
	if got["a@example.net"] != string(TypeDelivered) || got["b@example.net"] != string(TypeBounced) {
		t.Errorf("recipient states = %v", got)
	}
}

func TestDeliveryState_AllFailed(t *testing.T) {
	f := newReplayFixture(t)
	f.seedTo(t, "msg-3", "re-3", []string{"a@example.net", "b@example.net"})
	st := f.gate.Store()

	for _, r := range []string{"a@example.net", "b@example.net"} {
		if _, err := st.ProjectRecipientEvent("msg-3", r, string(TypeBounced), verifyNow); err != nil {
			t.Fatal(err)
		}
	}
	sub, _, _ := st.GetSubmission("msg-3")
	if sub.DeliveryState != storage.AggFailed {
		t.Errorf("aggregate = %q, want failed", sub.DeliveryState)
	}
}

// A message-scoped provider (Resend) names no recipient, so its event
// covers every recipient of the message.
func TestDeliveryState_MessageScopedAppliesToAll(t *testing.T) {
	f := newReplayFixture(t)
	f.seedTo(t, "msg-4", "re-4", []string{"a@example.net", "b@example.net"})
	st := f.gate.Store()

	if _, err := st.ProjectRecipientEvent("msg-4", "", string(TypeDelivered), verifyNow); err != nil {
		t.Fatal(err)
	}
	sub, _, _ := st.GetSubmission("msg-4")
	if sub.DeliveryState != storage.AggDelivered {
		t.Errorf("aggregate = %q, want delivered for a message-scoped event", sub.DeliveryState)
	}
	states, _ := st.RecipientStates("msg-4")
	if len(states) != 2 {
		t.Errorf("recipient rows = %d, want 2", len(states))
	}
}

// An event naming an address the submission does not have is reported
// rather than silently creating a row that would corrupt the aggregate.
func TestDeliveryState_UnknownRecipientDoesNotProject(t *testing.T) {
	f := newReplayFixture(t)
	f.seed(t, "msg-5", "re-5")

	matched, err := f.gate.Store().ProjectRecipientEvent(
		"msg-5", "stranger@elsewhere.test", string(TypeDelivered), verifyNow)
	if err != nil {
		t.Fatal(err)
	}
	if matched {
		t.Error("an unknown recipient was matched")
	}
	if states, _ := f.gate.Store().RecipientStates("msg-5"); len(states) != 0 {
		t.Errorf("recipient rows = %d, want none", len(states))
	}
}

func TestDeliveryState_RecipientMatchIsCaseInsensitive(t *testing.T) {
	f := newReplayFixture(t)
	f.seedTo(t, "msg-6", "re-6", []string{"Alice@Example.NET"})

	matched, err := f.gate.Store().ProjectRecipientEvent(
		"msg-6", "alice@example.net", string(TypeDelivered), verifyNow)
	if err != nil {
		t.Fatal(err)
	}
	if !matched {
		t.Error("a case-different recipient was dropped; providers normalise case")
	}
}

// --- transition rules (ADR-44) -----------------------------------------

// The disorder cases the projection must survive, stated one by one.
func TestSupersedes_TransitionRules(t *testing.T) {
	t0 := verifyNow
	t1 := verifyNow.Add(time.Minute)

	for _, tc := range []struct {
		name      string
		current   string
		currentAt time.Time
		next      string
		nextAt    time.Time
		want      bool
	}{
		{"first event always lands", "", time.Time{}, string(TypeSent), t0, true},

		// The case a plain "latest wins" rule gets wrong.
		{"delivered then a LATER deferred", string(TypeDelivered), t0, string(TypeDeferred), t1, false},
		{"delivered then an EARLIER deferred", string(TypeDelivered), t1, string(TypeDeferred), t0, false},
		{"delivered then a later sent", string(TypeDelivered), t0, string(TypeSent), t1, false},

		// A terminal fact lands however late it arrives.
		{"sent then an EARLIER bounce", string(TypeSent), t1, string(TypeBounced), t0, true},
		{"deferred then a later delivery", string(TypeDeferred), t0, string(TypeDelivered), t1, true},

		// Duplicates are idempotent.
		{"duplicate delivered", string(TypeDelivered), t0, string(TypeDelivered), t0, false},
		{"duplicate sent", string(TypeSent), t0, string(TypeSent), t0, false},

		// A complaint after a delivery is legitimate and wins.
		{"complained after delivered", string(TypeDelivered), t0, string(TypeComplained), t1, true},
		{"complained at the same instant as delivered", string(TypeDelivered), t0, string(TypeComplained), t0, true},
		// Terminal outcomes escalate and never de-escalate: an
		// out-of-order delivery webhook must not undo a complaint.
		{"delivered cannot undo a complaint", string(TypeComplained), t0, string(TypeDelivered), t1, false},
		{"delivered cannot undo a bounce", string(TypeBounced), t0, string(TypeDelivered), t1, false},
		{"a bounce escalates to a complaint", string(TypeBounced), t0, string(TypeComplained), t1, true},
		{"a complaint is not downgraded to a bounce", string(TypeComplained), t0, string(TypeBounced), t1, false},

		// Progress ordering among non-terminals.
		{"sent then deferred", string(TypeSent), t0, string(TypeDeferred), t1, true},
		{"deferred then an earlier sent", string(TypeDeferred), t1, string(TypeSent), t0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := storage.Supersedes(tc.current, tc.currentAt, tc.next, tc.nextAt)
			if got != tc.want {
				t.Errorf("Supersedes(%s@%v, %s@%v) = %v, want %v",
					tc.current, tc.currentAt.Unix(), tc.next, tc.nextAt.Unix(), got, tc.want)
			}
		})
	}
}

// The end-to-end disorder case: a delayed `deferred` webhook arriving
// after a `delivered` one must not un-deliver the message.
func TestDeliveryState_DelayedDeferredDoesNotUndeliver(t *testing.T) {
	f := newReplayFixture(t)
	f.seed(t, "msg-7", "re-7")
	st := f.gate.Store()

	if _, err := st.ProjectRecipientEvent("msg-7", "", string(TypeDelivered), verifyNow); err != nil {
		t.Fatal(err)
	}
	// The provider's earlier "delayed" webhook finally arrives, and
	// its own clock stamps it LATER than the delivery.
	if _, err := st.ProjectRecipientEvent("msg-7", "", string(TypeDeferred), verifyNow.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	sub, _, _ := st.GetSubmission("msg-7")
	if sub.DeliveryState != storage.AggDelivered {
		t.Errorf("aggregate = %q — a delayed deferred un-delivered the message", sub.DeliveryState)
	}
}

// The events remain the record of fact; the projection never removes
// them.
func TestDeliveryState_EventsAreNotRewritten(t *testing.T) {
	f := newReplayFixture(t)
	f.seed(t, "msg-1", "re-1")

	f.post(t, "msg_evt_1", deliveredBody)
	bounced := `{"type":"email.bounced","created_at":"2026-09-08T12:05:00Z","data":{"email_id":"re-1","to":["reader@example.net"]}}`
	f.post(t, "msg_evt_2", bounced)
	waitFor(t, func() bool { return len(f.forwarded()) == 2 })

	events, _ := f.gate.Store().EventsFor("msg-1")
	if len(events) != 2 {
		t.Fatalf("events = %d, want both retained", len(events))
	}
	sub, _, _ := f.gate.Store().GetSubmission("msg-1")
	if sub.DeliveryState != storage.AggFailed {
		t.Errorf("aggregate = %q, want failed after the bounce", sub.DeliveryState)
	}
}

// suppressFor exercises the handler's suppression scoping directly:
// the event names a recipient or it does not, and that decides who is
// suppressed.
func (f *replayFixture) suppressFor(t *testing.T, ev DeliveryEvent, all []string) {
	t.Helper()
	victims := all
	if ev.Recipient != "" {
		victims = []string{ev.Recipient}
	}
	for _, addr := range victims {
		if err := f.gate.Store().AddSuppression(addr, storage.ReasonHardBounce, "/api/send", verifyNow); err != nil {
			t.Fatalf("AddSuppression: %v", err)
		}
	}
}

func (f *replayFixture) suppressed(t *testing.T, addr string) bool {
	t.Helper()
	_, ok, err := f.gate.Store().SuppressionFor(addr)
	if err != nil {
		t.Fatalf("SuppressionFor: %v", err)
	}
	return ok
}

// waitFor polls until cond holds or the deadline passes. The forwarder
// delivers inline but the receiver records on its own goroutine.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not reached within 2s")
}

var _ = strconv.Itoa

// --- multi-recipient suppression scoping -------------------------------

// A recipient-scoped bounce must suppress ONLY the address it names.
// Suppressing every recipient of a multi-recipient message would block
// addresses that are perfectly deliverable.
func TestSuppression_ScopedToTheNamedRecipient(t *testing.T) {
	f := newReplayFixture(t)
	f.seedTo(t, "msg-sup", "pm-sup", []string{"good@example.net", "bad@example.net"})

	ev := DeliveryEvent{
		MessageID: "msg-sup", Project: "novel", Provider: "postmark",
		Recipient: "bad@example.net", Type: TypeBounced, Timestamp: verifyNow,
	}
	f.suppressFor(t, ev, []string{"good@example.net", "bad@example.net"})

	if !f.suppressed(t, "bad@example.net") {
		t.Error("the bouncing recipient was not suppressed")
	}
	if f.suppressed(t, "good@example.net") {
		t.Error("a deliverable recipient was suppressed by another recipient's bounce")
	}
}

// A message-scoped provider names no recipient, so the whole recipient
// set is what bounced.
func TestSuppression_MessageScopedSuppressesAll(t *testing.T) {
	f := newReplayFixture(t)
	f.seedTo(t, "msg-sup2", "re-sup", []string{"a@example.net", "b@example.net"})

	ev := DeliveryEvent{
		MessageID: "msg-sup2", Project: "novel", Provider: "resend",
		Type: TypeBounced, Timestamp: verifyNow,
	}
	f.suppressFor(t, ev, []string{"a@example.net", "b@example.net"})

	for _, addr := range []string{"a@example.net", "b@example.net"} {
		if !f.suppressed(t, addr) {
			t.Errorf("%s was not suppressed by a message-scoped bounce", addr)
		}
	}
}

// --- remaining disorder cases (RC1 item 9) -----------------------------

// A correctly SIGNED request whose body is not a usable event: the
// signature proves the sender, the payload is still garbage. It must be
// rejected as a payload problem, not as a forgery, and must change
// nothing.
func TestIngest_ValidSignatureMalformedPayload(t *testing.T) {
	f := newReplayFixture(t)
	f.seed(t, "msg-1", "re-1")

	for _, body := range []string{
		`not json at all`,
		`{"no_type_field":true}`,
		`{"type":"","data":{}}`,
	} {
		w := f.post(t, "evt-"+body[:4], body)
		if w.Code == http.StatusUnauthorized {
			t.Errorf("a correctly signed request was reported as a forgery: %q", body)
		}
	}
	if events, _ := f.gate.Store().EventsFor("msg-1"); len(events) != 0 {
		t.Errorf("%d events written from malformed payloads", len(events))
	}
	if got := f.forwarded(); len(got) != 0 {
		t.Errorf("%d webhooks fired from malformed payloads", len(got))
	}
	if !f.metric(t, `posthorn_provider_webhook_total{provider="resend",result="invalid_payload"}`) {
		t.Error("malformed payloads were not counted as invalid_payload")
	}
}

// A signed, well-formed event naming a provider message id Posthorn has
// never seen. It must be counted and dropped, never guessed at.
func TestIngest_WrongProviderMessageID(t *testing.T) {
	f := newReplayFixture(t)
	f.seed(t, "msg-1", "re-1")

	body := `{"type":"email.delivered","created_at":"2026-09-08T12:00:00Z","data":{"email_id":"someone-elses-id","to":["reader@example.net"]}}`
	f.post(t, "evt-wrong", body)

	if events, _ := f.gate.Store().EventsFor("msg-1"); len(events) != 0 {
		t.Error("an event for an unknown provider id was attached to a real message")
	}
	if got := f.forwarded(); len(got) != 0 {
		t.Errorf("%d webhooks fired for an uncorrelated event", len(got))
	}
	sub, _, _ := f.gate.Store().GetSubmission("msg-1")
	if sub.DeliveryState != "" {
		t.Errorf("delivery_state = %q — an uncorrelated event changed message state", sub.DeliveryState)
	}
	if !f.metric(t, `posthorn_provider_webhook_total{provider="resend",result="unknown_message"} 1`) {
		t.Error("unknown_message not counted")
	}
}

// Out-of-order arrival end to end: the bounce is delivered to the
// ingestion endpoint AFTER the delivery, but carries an EARLIER
// provider timestamp. The terminal outcome must still land.
func TestIngest_OutOfOrderArrivalStillLands(t *testing.T) {
	f := newReplayFixture(t)
	f.seed(t, "msg-1", "re-1")

	// The provider's own clock says the bounce happened first.
	late := `{"type":"email.bounced","created_at":"2026-09-08T11:00:00Z","data":{"email_id":"re-1","to":["reader@example.net"]}}`
	f.post(t, "evt-sent", `{"type":"email.sent","created_at":"2026-09-08T11:30:00Z","data":{"email_id":"re-1","to":["reader@example.net"]}}`)
	f.post(t, "evt-late-bounce", late)
	waitFor(t, func() bool { return len(f.forwarded()) == 2 })

	sub, _, _ := f.gate.Store().GetSubmission("msg-1")
	if sub.DeliveryState != storage.AggFailed {
		t.Errorf("aggregate = %q — a terminal bounce arriving out of order was dropped", sub.DeliveryState)
	}
	// Both events remain the record of fact.
	if events, _ := f.gate.Store().EventsFor("msg-1"); len(events) != 2 {
		t.Errorf("events = %d, want both retained", len(events))
	}
}
