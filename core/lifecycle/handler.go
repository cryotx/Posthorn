// Modified for the cryotx/posthorn distribution.

package lifecycle

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/craigmccaskill/posthorn/metrics"
	"github.com/craigmccaskill/posthorn/ratelimit"
	"github.com/craigmccaskill/posthorn/storage"
)

// maxEventBody bounds an inbound event payload. Postmark events are
// single-digit KB; 256KB is generous headroom, not an invitation.
const maxEventBody = 256 << 10

// Threat posture for the ingestion endpoint (threat→defense table,
// architecture doc): forged events poisoning the suppression list are
// blocked by mandatory fail-closed basic auth; retry storms and
// brute-force are blunted by the per-IP rate limit; oversized bodies by
// MaxBytesReader; unknown message IDs are answered 200 so a
// misconfigured (or probing) sender gets no retry loop and no oracle.
type Handler struct {
	username  string
	password  string
	gate      *storage.Gate
	forwarder *Forwarder
	logger    *slog.Logger
	recorder  *metrics.Recorder
	limiter   *ratelimit.Limiter

	// adapter is the provider dialect this handler ingests (FR117).
	// Nil means the pre-adapter Postmark path, kept so an existing
	// deployment's /events/postmark route behaves identically.
	adapter ProviderWebhookAdapter

	// now is injectable for tests. Production: time.Now.
	now func() time.Time
}

// WithAdapter returns a copy of h that ingests one provider dialect
// through the adapter interface, storing unified events and forwarding
// to the owning project's webhook (FR114, FR117).
func (h *Handler) WithAdapter(a ProviderWebhookAdapter) *Handler {
	clone := *h
	clone.adapter = a
	return &clone
}

// NewHandler builds the /events/postmark handler (FR82). username and
// password are the mandatory basic-auth pair; forwarder carries the
// per-endpoint webhook map and may deliver inline or via the queue.
func NewHandler(username, password string, gate *storage.Gate, forwarder *Forwarder, logger *slog.Logger, recorder *metrics.Recorder) (*Handler, error) {
	// 60 posts/min per source IP: far above any legitimate Postmark
	// event rate for a self-hosted sender, low enough to blunt
	// brute-force against the basic-auth pair.
	limiter, err := ratelimit.New(60, time.Minute, 0)
	if err != nil {
		return nil, err
	}
	return &Handler{
		username:  username,
		password:  password,
		gate:      gate,
		forwarder: forwarder,
		logger:    logger,
		recorder:  recorder,
		limiter:   limiter,
		now:       time.Now,
	}, nil
}

// ServeHTTP implements the Postmark event sink (FR82, FR83, FR85).
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	ip := remoteIP(r)
	if !h.limiter.Allow(ip) {
		http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
		return
	}

	// Fail-closed basic auth, constant-time on both fields (NFR29).
	user, pass, ok := r.BasicAuth()
	userOK := subtle.ConstantTimeCompare([]byte(user), []byte(h.username))
	passOK := subtle.ConstantTimeCompare([]byte(pass), []byte(h.password))
	if !ok || userOK&passOK != 1 {
		w.Header().Set("WWW-Authenticate", `Basic realm="posthorn-events"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxEventBody))
	if err != nil {
		http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
		return
	}

	// FR121: provider authenticity is checked BEFORE anything is
	// written back. A request that fails verification is not an
	// authenticated sender, so the "always 200" posture below does not
	// apply to it — it gets a clear rejection, and nothing downstream
	// runs.
	if h.adapter != nil {
		if !h.verified(w, r, body) {
			return
		}
	}

	// From here on the answer is 200 regardless of processing outcome:
	// the sender is authenticated, and non-2xx answers only buy
	// provider retry storms for events we will never process
	// differently (FR83). Problems surface in logs and metrics.
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))

	if h.adapter != nil {
		h.serveAdapter(r, body)
		return
	}

	ev, messageID, err := NormalizePostmark(body, h.now())
	if err != nil {
		h.logger.Warn("lifecycle_event_malformed", slog.String("error", err.Error()))
		h.recorder.LifecycleDropped("malformed")
		return
	}

	if !h.gate.Healthy() {
		// Degraded storage: no correlation possible. Drop loudly.
		h.logger.Warn("lifecycle_event_dropped_storage_degraded", slog.String("event", ev.Event))
		h.recorder.LifecycleDropped("unmatched")
		return
	}
	sub, found, err := h.gate.Store().FindByMessageID(messageID)
	if err != nil {
		h.gate.ReportError(err)
		h.recorder.LifecycleDropped("unmatched")
		return
	}
	if !found {
		h.logger.Info("lifecycle_event_unmatched",
			slog.String("event", ev.Event),
			slog.String("transport_message_id", messageID),
		)
		h.recorder.LifecycleDropped("unmatched")
		return
	}

	ev.SubmissionID = sub.ID
	ev.Endpoint = sub.Endpoint
	h.recorder.LifecycleEvent(ev.Event)
	h.logger.Info("lifecycle_event",
		slog.String("event", ev.Event),
		slog.String("submission_id", sub.ID),
		slog.String("endpoint", sub.Endpoint),
	)

	// FR85: hard bounces and spam complaints auto-suppress globally.
	if (ev.Event == EventHardBounce || ev.Event == EventSpamComplaint) && ev.Recipient != "" {
		if err := h.gate.Store().AddSuppression(ev.Recipient, suppressionReason(ev.Event), sub.Endpoint, h.now()); err != nil {
			h.gate.ReportError(err)
		} else {
			h.logger.Info("suppression_added",
				slog.String("reason", suppressionReason(ev.Event)),
				slog.String("endpoint", sub.Endpoint),
			)
		}
	}

	// FR84: forward to the originating endpoint's webhook, if any.
	payload, err := json.Marshal(ev)
	if err != nil {
		h.logger.Error("lifecycle_event_encode_failed", slog.String("error", err.Error()))
		return
	}
	h.forwarder.Deliver(r.Context(), sub.Endpoint, payload)
}

// verified authenticates the request against the provider's own
// scheme, answering the caller directly on failure.
//
// Nothing past this point runs on an unverified request: no normalize,
// no DeliveryEvent, no message-state change, no project webhook — a
// forged callback must not be able to mark a message bounced, suppress
// a recipient, or fire a customer's callback (FR121).
func (h *Handler) verified(w http.ResponseWriter, r *http.Request, body []byte) bool {
	err := h.adapter.Verify(r, body)
	if err == nil {
		return true
	}
	reason := ReasonBadSignature
	var ve *VerifyError
	if errors.As(err, &ve) {
		reason = ve.Reason
	}
	// The reason and a request id are logged. The signature, the
	// secret, the Authorization header and the payload are not (NFR48).
	h.logger.Warn("lifecycle_event_unverified",
		slog.String("provider", h.adapter.Name()),
		slog.String("reason", reason),
		slog.String("request_id", requestID(r)),
	)
	h.recorder.ProviderWebhook(h.adapter.Name(), metrics.WebhookInvalidSignature)
	h.recorder.LifecycleDropped("unverified")

	// 401 rather than a silent 200: this is an authentication failure,
	// and a provider retrying it will keep failing until the operator
	// fixes the secret — which a 4xx makes visible in their dashboard,
	// and a 200 would hide forever.
	http.Error(w, "webhook signature verification failed", http.StatusUnauthorized)
	return false
}

// serveAdapter ingests one verified provider payload: parse, normalize,
// correlate, store, forward.
//
// The response has already been written 200 by the caller — an
// authenticated sender gets no retry storm for events Posthorn would
// never process differently.
func (h *Handler) serveAdapter(r *http.Request, body []byte) {
	raws, err := h.parseEvents(r, body)
	if err != nil {
		h.logger.Warn("lifecycle_event_malformed",
			slog.String("provider", h.adapter.Name()),
			slog.String("error", err.Error()),
			slog.String("request_id", requestID(r)),
		)
		h.recorder.ProviderWebhook(h.adapter.Name(), metrics.WebhookInvalidPayload)
		h.recorder.LifecycleDropped("malformed")
		return
	}
	for _, raw := range raws {
		h.ingestOne(r, raw)
	}
}

// parseEvents prefers the request-aware parser when the adapter offers
// one, so an event id carried in a header reaches the replay check.
func (h *Handler) parseEvents(r *http.Request, body []byte) ([]RawEvent, error) {
	if rp, ok := h.adapter.(RequestParser); ok {
		return rp.ParseRequest(r, body)
	}
	return h.adapter.Parse(body)
}

// requestID is a short correlation handle for a rejected webhook. It is
// derived from the connection and the clock, never from the payload, so
// it can be logged unconditionally.
func requestID(r *http.Request) string {
	if v := r.Header.Get("X-Request-Id"); v != "" && len(v) <= 64 {
		return v
	}
	return fmt.Sprintf("%s/%d", remoteIP(r), time.Now().UnixNano()%1e9)
}

func (h *Handler) ingestOne(r *http.Request, raw RawEvent) {
	ev, err := h.adapter.Normalize(raw, h.now())
	if err != nil {
		// A correctly signed request whose body is not a usable event:
		// the sender is proven, the payload is not. Counted as a
		// payload problem, never as a forgery — conflating the two
		// would make a provider's schema change look like an attack.
		h.logger.Warn("lifecycle_event_malformed",
			slog.String("provider", h.adapter.Name()),
			slog.String("error", err.Error()))
		h.recorder.ProviderWebhook(h.adapter.Name(), metrics.WebhookInvalidPayload)
		h.recorder.LifecycleDropped("malformed")
		return
	}
	if ev.ProviderMessageID == "" {
		// Nothing to correlate on. Not a forgery and not a real
		// unknown message either — the payload simply did not carry an
		// id, which is a payload defect.
		h.logger.Warn("lifecycle_event_no_provider_id",
			slog.String("provider", h.adapter.Name()),
			slog.String("type", string(ev.Type)))
		h.recorder.ProviderWebhook(h.adapter.Name(), metrics.WebhookInvalidPayload)
		h.recorder.LifecycleDropped("malformed")
		return
	}
	ev.EventID = NewEventID()

	if !h.gate.Healthy() {
		h.logger.Warn("lifecycle_event_dropped_storage_degraded",
			slog.String("type", string(ev.Type)))
		h.recorder.LifecycleDropped("unmatched")
		return
	}

	// Correlate the provider's own id back to the Posthorn message id.
	// This is what makes one logical message trackable across the
	// provider that actually carried it (ADR-34).
	sub, found, err := h.gate.Store().FindByMessageID(ev.ProviderMessageID)
	if err != nil {
		h.gate.ReportError(err)
		h.recorder.LifecycleDropped("unmatched")
		return
	}
	if !found {
		h.logger.Info("lifecycle_event_unmatched",
			slog.String("provider", h.adapter.Name()),
			slog.String("type", string(ev.Type)))
		h.recorder.ProviderWebhook(h.adapter.Name(), metrics.WebhookUnknownMessage)
		h.recorder.LifecycleDropped("unmatched")
		return
	}
	ev.MessageID = sub.ID
	ev.Project = sub.Project
	if ev.Provider == "" {
		ev.Provider = sub.Transport
	}

	fresh, err := h.gate.Store().RecordDeliveryEvent(storage.DeliveryEventRow{
		EventID:           ev.EventID,
		MessageID:         ev.MessageID,
		Project:           ev.Project,
		Provider:          ev.Provider,
		ProviderMessageID: ev.ProviderMessageID,
		ProviderEventKey:  ev.ProviderEventKey,
		Type:              string(ev.Type),
		OccurredAt:        ev.Timestamp,
		Metadata:          ev.Metadata,
	})
	if err != nil {
		h.gate.ReportError(err)
		h.recorder.ProviderWebhook(h.adapter.Name(), metrics.WebhookInternalError)
		return
	}
	if !fresh {
		// FR122: the provider redelivered an event we already have.
		// Stop here — recording it twice would double-count, and
		// forwarding it twice would make the project's callback fire
		// again for something it already handled.
		h.logger.Info("lifecycle_event_duplicate",
			slog.String("provider", ev.Provider),
			slog.String("message_id", ev.MessageID),
			slog.String("type", string(ev.Type)),
		)
		h.recorder.ProviderWebhook(h.adapter.Name(), metrics.WebhookDuplicate)
		h.recorder.LifecycleDropped("duplicate")
		return
	}
	h.recorder.ProviderWebhook(h.adapter.Name(), metrics.WebhookAccepted)

	// FR128: project the event onto the message's RECIPIENTS, then the
	// submission aggregate. A recipient-scoped provider (Postmark)
	// names one address; a message-scoped one (Resend) names none and
	// applies to all. Events stay the record of fact; state is a
	// projection, and no event is ever removed to maintain it.
	matched, serr := h.gate.Store().ProjectRecipientEvent(
		ev.MessageID, ev.Recipient, string(ev.Type), ev.Timestamp)
	if serr != nil {
		h.gate.ReportError(serr)
	} else if !matched && ev.Recipient != "" {
		// The provider named a recipient this submission does not
		// have. The event is already recorded; inventing a projection
		// row for it would corrupt the aggregate.
		h.logger.Warn("lifecycle_event_recipient_unmatched",
			slog.String("provider", ev.Provider),
			slog.String("message_id", ev.MessageID),
			slog.String("type", string(ev.Type)),
		)
	}

	h.recorder.LifecycleEvent(string(ev.Type))
	h.recorder.ProjectDelivery(ev.Project, string(ev.Type))
	h.logger.Info("lifecycle_event",
		slog.String("type", string(ev.Type)),
		slog.String("message_id", ev.MessageID),
		slog.String("project", ev.Project),
		slog.String("provider", ev.Provider),
	)

	// FR85: hard bounces and complaints auto-suppress globally. The
	// recipient is read from the submission log, not from the event —
	// the event carries only a domain by design (NFR44).
	if ev.Type == TypeBounced || ev.Type == TypeComplained {
		reason := storage.ReasonHardBounce
		if ev.Type == TypeComplained {
			reason = storage.ReasonSpamComplaint
		}
		// Suppress only the recipient the event actually names. A
		// message-scoped provider names none, and then the whole
		// recipient set is what bounced. Suppressing every recipient
		// of a multi-recipient message on one recipient-scoped bounce
		// would block addresses that are perfectly deliverable.
		victims := sub.ToAddrs
		if ev.Recipient != "" {
			victims = []string{ev.Recipient}
		}
		for _, addr := range victims {
			if err := h.gate.Store().AddSuppression(addr, reason, sub.Endpoint, h.now()); err != nil {
				h.gate.ReportError(err)
			}
		}
	}

	payload, err := ev.Encode()
	if err != nil {
		h.logger.Error("lifecycle_event_encode_failed", slog.String("error", err.Error()))
		return
	}
	// A project's webhook wins over the endpoint's: the project is the
	// business owner of the message, and is who asked to be told.
	scope := sub.Endpoint
	if ev.Project != "" {
		scope = "project:" + ev.Project
	}
	h.forwarder.DeliverEvent(r.Context(), scope, ev.EventID, ev.Timestamp, payload)
}

func suppressionReason(event string) string {
	if event == EventSpamComplaint {
		return storage.ReasonSpamComplaint
	}
	return storage.ReasonHardBounce
}

func remoteIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
