// Modified for the cryotx/posthorn distribution.

package lifecycle

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/craigmccaskill/posthorn/storage"
)

// SignatureHeader carries the HMAC-SHA256 of the request body under the
// endpoint's webhook_secret, hex-encoded with a scheme prefix:
// "sha256=<hex>". Receivers recompute and constant-time compare (FR84).
const SignatureHeader = "X-Posthorn-Signature"

// EventIDHeader and TimestampHeader let a receiver deduplicate and
// age-check without parsing the body first (FR118).
//
// They are conveniences, not the security boundary: both values are
// ALSO inside the signed body, which is what makes replay protection
// real. A header alone is unauthenticated and an attacker could rewrite
// it; the body cannot be touched without invalidating the signature.
// So the body-only signing scheme is unchanged from v2.0, and existing
// receivers keep verifying exactly as they do today.
const (
	EventIDHeader   = "X-Posthorn-Event-Id"
	TimestampHeader = "X-Posthorn-Timestamp"
)

// ForwardTimeout bounds one delivery attempt. Callback receivers are
// operator-controlled servers; 5s matches the transport-side posture.
// Exported so the composition root can build a matching guarded client.
const ForwardTimeout = 5 * time.Second

// Webhook is one endpoint's callback target (from config).
type Webhook struct {
	URL    string
	Secret string
}

// Sign returns the SignatureHeader value for body under secret.
func Sign(body []byte, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// ForwardHooks observe delivery outcomes for metrics. outcome is a
// fixed enum: "sent", "queued", "dropped".
type ForwardHooks func(endpoint, outcome string)

// Forwarder delivers normalized events to per-endpoint webhooks:
// inline first, riding the storage forward-queue on transient failure
// (FR84, same sync-first posture as mail).
type Forwarder struct {
	Webhooks map[string]Webhook // endpoint path → target
	Gate     *storage.Gate
	Logger   *slog.Logger
	OnResult ForwardHooks // nil-safe

	// Client is injectable for tests. Nil means a 5s-timeout default.
	Client *http.Client

	// Now is injectable for tests. Nil means time.Now.
	Now func() time.Time
}

func (f *Forwarder) client() *http.Client {
	if f.Client != nil {
		return f.Client
	}
	return &http.Client{Timeout: ForwardTimeout}
}

func (f *Forwarder) now() time.Time {
	if f.Now != nil {
		return f.Now()
	}
	return time.Now()
}

func (f *Forwarder) result(endpoint, outcome string) {
	if f.OnResult != nil {
		f.OnResult(endpoint, outcome)
	}
}

// DeliverEvent is Deliver with the event's identity attached, for the
// unified-event path (FR118). scope is an endpoint path or
// "project:<name>".
func (f *Forwarder) DeliverEvent(ctx context.Context, scope, eventID string, ts time.Time, payload []byte) {
	f.deliver(ctx, scope, eventID, ts, payload)
}

// Deliver attempts one inline delivery for endpoint; transient failures
// enqueue for background retry. A no-webhook endpoint is a silent no-op
// (the caller filters, but defense in depth).
func (f *Forwarder) Deliver(ctx context.Context, endpoint string, payload []byte) {
	f.deliver(ctx, endpoint, "", time.Time{}, payload)
}

func (f *Forwarder) deliver(ctx context.Context, endpoint, eventID string, ts time.Time, payload []byte) {
	wh, ok := f.Webhooks[endpoint]
	if !ok {
		return
	}
	logger := f.Logger.With(slog.String("endpoint", endpoint))

	retryable, err := f.attempt(ctx, wh, eventID, ts, payload)
	if err == nil {
		logger.Info("lifecycle_forward_sent")
		f.result(endpoint, "sent")
		return
	}
	if retryable && f.Gate.Healthy() {
		if qerr := f.Gate.Store().EnqueueForward(endpoint, payload, f.now()); qerr != nil {
			f.Gate.ReportError(qerr)
		} else {
			logger.Warn("lifecycle_forward_queued", slog.String("error", err.Error()))
			f.result(endpoint, "queued")
			return
		}
	}
	logger.Error("lifecycle_forward_dropped", slog.String("error", err.Error()))
	f.result(endpoint, "dropped")
}

// attempt POSTs payload to the webhook. Classification mirrors FR19-22:
// 2xx success; 429/5xx/network retryable; other 4xx terminal.
//
// NFR29: wh.Secret is used for signing only; it must never reach a log
// or an error value.
func (f *Forwarder) attempt(ctx context.Context, wh Webhook, eventID string, ts time.Time, payload []byte) (retryable bool, err error) {
	ctx, cancel := context.WithTimeout(ctx, ForwardTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, wh.URL, bytes.NewReader(payload))
	if err != nil {
		return false, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(SignatureHeader, Sign(payload, wh.Secret))
	if eventID != "" {
		req.Header.Set(EventIDHeader, eventID)
	}
	if !ts.IsZero() {
		req.Header.Set(TimestampHeader, ts.UTC().Format(time.RFC3339))
	}

	resp, err := f.client().Do(req)
	if err != nil {
		return true, err
	}
	defer func() { _ = resp.Body.Close() }()
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return false, nil
	case resp.StatusCode == http.StatusTooManyRequests, resp.StatusCode >= 500:
		return true, statusError(resp.StatusCode)
	default:
		return false, statusError(resp.StatusCode)
	}
}

// eventIdentity reads the event id and timestamp back out of a stored
// payload. A payload from the pre-unified path has neither, and the
// zero values simply omit the headers.
func eventIdentity(payload []byte) (string, time.Time) {
	var probe struct {
		EventID   string    `json:"event_id"`
		Timestamp time.Time `json:"timestamp"`
	}
	if err := json.Unmarshal(payload, &probe); err != nil {
		return "", time.Time{}
	}
	return probe.EventID, probe.Timestamp
}

func statusError(code int) error {
	return fmt.Errorf("webhook receiver returned status %d", code)
}

// RunQueue drains the forward queue until ctx is canceled — the
// lifecycle counterpart of the mail retry worker, same jittered-poll
// shape, one goroutine (NFR26).
func (f *Forwarder) RunQueue(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 30 * time.Second
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
		f.ProcessDue(ctx)
	}
}

// ProcessDue performs one poll of the forward queue. Exposed for
// deterministic tests.
func (f *Forwarder) ProcessDue(ctx context.Context) {
	if !f.Gate.Healthy() {
		return
	}
	due, err := f.Gate.Store().ClaimDueForwards(f.now(), 10)
	if err != nil {
		f.Logger.Error("lifecycle_queue_claim_failed", slog.String("error", err.Error()))
		return
	}
	for _, p := range due {
		if ctx.Err() != nil {
			return
		}
		logger := f.Logger.With(slog.String("endpoint", p.Endpoint), slog.Int("attempt", p.Attempt))
		wh, ok := f.Webhooks[p.Endpoint]
		if !ok {
			// Endpoint's webhook removed between restarts: drop.
			_, _ = f.Gate.Store().RecordForwardOutcome(p.ID, false, f.now())
			logger.Warn("lifecycle_forward_dropped", slog.String("error", "webhook no longer configured"))
			f.result(p.Endpoint, "dropped")
			continue
		}
		// A queued retry re-derives the event identity from the stored
		// payload, so a redelivery carries the SAME event id as the
		// first attempt — which is what lets the receiver's dedupe
		// recognise it as a retry rather than a new event.
		eventID, ts := eventIdentity(p.Payload)
		retryable, err := f.attempt(ctx, wh, eventID, ts, p.Payload)
		if err == nil {
			_, _ = f.Gate.Store().RecordForwardOutcome(p.ID, false, f.now())
			logger.Info("lifecycle_forward_sent")
			f.result(p.Endpoint, "sent")
			continue
		}
		dropped, oerr := f.Gate.Store().RecordForwardOutcome(p.ID, retryable, f.now())
		if oerr != nil {
			f.Gate.ReportError(oerr)
			continue
		}
		if dropped || !retryable {
			logger.Error("lifecycle_forward_dropped", slog.String("error", err.Error()))
			f.result(p.Endpoint, "dropped")
		} else {
			logger.Warn("lifecycle_forward_retry_scheduled", slog.String("error", err.Error()))
		}
	}
}
