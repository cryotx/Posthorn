package lifecycle

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// ProviderWebhookAdapter turns one provider's callback dialect into
// unified DeliveryEvents (FR117).
//
// Three responsibilities, deliberately separated so each can fail
// distinctly and be tested alone:
//
//	Verify    — is this request actually from the provider?
//	Parse     — split the payload into individual raw events
//	Normalize — map one raw event onto the unified shape
//
// Adapters are the ONLY place a provider's field names may appear.
// Everything downstream — storage, metrics, project webhooks — sees
// DeliveryEvent and nothing else, which is what lets an operator swap
// providers without their business systems noticing.
type ProviderWebhookAdapter interface {
	// Name is the provider dialect, and the URL segment this adapter
	// is mounted at (/events/<name>).
	Name() string

	// Verify authenticates the request beyond the endpoint's mandatory
	// basic auth. Adapters for providers with a documented signature
	// scheme check it here; adapters for providers without one return
	// nil and rely on basic auth (ADR-22's fail-closed posture is
	// unchanged either way).
	Verify(r *http.Request, body []byte) error

	// Parse splits a payload into raw per-event units. Providers that
	// post one event per request return a single-element slice; those
	// that batch return several.
	//
	// Implementations that carry an event id in a header rather than
	// the body also implement RequestParser, which the handler prefers.
	Parse(body []byte) ([]RawEvent, error)

	// Normalize maps one raw unit onto the unified shape. now is the
	// fallback timestamp when the provider supplies none.
	//
	// Implementations must fill Type, Timestamp, Provider and
	// ProviderMessageID. EventID, MessageID and Project are filled by
	// the ingestion handler, which owns correlation.
	Normalize(raw RawEvent, now time.Time) (DeliveryEvent, error)
}

// RequestParser is the optional richer Parse for adapters whose event
// identity lives in the request headers rather than the body — Resend
// carries it in svix-id. The handler prefers this when an adapter
// implements it and falls back to Parse otherwise, so an adapter that
// needs nothing from the request stays a two-line implementation.
type RequestParser interface {
	ParseRequest(r *http.Request, body []byte) ([]RawEvent, error)
}

// RawEvent is one provider event before normalization.
type RawEvent struct {
	// Payload is the provider's own JSON for this single event.
	Payload json.RawMessage

	// EventKey is the provider's own event identifier, when it sends
	// one and it lives outside the payload (Resend puts it in the
	// svix-id header). Parse fills it; Normalize copies it onto the
	// DeliveryEvent as the replay key.
	EventKey string
}

// AdapterBuilder constructs a configured adapter. Adapters carry their
// authentication configuration, so Verify is a real check rather than a
// stateless no-op — which is why the registry holds builders now and
// not instances.
type AdapterBuilder func(ProviderAuthConfig) (ProviderWebhookAdapter, error)

var (
	adapterMu sync.RWMutex
	adapters  = map[string]AdapterBuilder{}
)

// RegisterAdapter adds an adapter builder. Called from init(),
// mirroring the transport registry: a new provider dialect is a new
// file, not an edit to the ingestion handler.
func RegisterAdapter(name string, b AdapterBuilder) {
	if name == "" || b == nil {
		panic("lifecycle.RegisterAdapter: name and builder are required")
	}
	adapterMu.Lock()
	defer adapterMu.Unlock()
	if _, dup := adapters[name]; dup {
		panic("lifecycle.RegisterAdapter: duplicate adapter " + name)
	}
	adapters[name] = b
}

// BuildAdapter constructs the adapter for a dialect with the operator's
// authentication configuration. Returns ok=false for an unknown
// dialect, and an error when the configuration is unusable — an
// unusable webhook credential must fail at startup, not silently accept
// forged events at runtime.
func BuildAdapter(name string, cfg ProviderAuthConfig) (ProviderWebhookAdapter, bool, error) {
	adapterMu.RLock()
	b, ok := adapters[name]
	adapterMu.RUnlock()
	if !ok {
		return nil, false, nil
	}
	a, err := b(cfg)
	if err != nil {
		return nil, true, fmt.Errorf("lifecycle: %s webhook: %w", name, err)
	}
	return a, true, nil
}

// AdapterNames returns the registered dialects, sorted.
func AdapterNames() []string {
	adapterMu.RLock()
	defer adapterMu.RUnlock()
	out := make([]string, 0, len(adapters))
	for n := range adapters {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// --- Postmark ---------------------------------------------------------

// PostmarkWebhookIPs are the source addresses Postmark publishes for
// outbound webhook requests, offered as a named preset so an operator
// does not have to transcribe them.
//
// Verified against Postmark's firewall documentation on 2026-09-08.
// They are Postmark's to change: an operator who pins them takes on
// the job of noticing when they do, which is why the allowlist is
// opt-in and empty by default.
var PostmarkWebhookIPs = []string{
	"3.134.147.250",
	"50.31.156.6",
	"50.31.156.77",
	"18.217.206.57",
}

type postmarkAdapter struct {
	allowedIPs []netAllow
	trusted    []netAllow
}

func (postmarkAdapter) Name() string { return "postmark" }

// Verify implements the authentication Postmark actually publishes.
//
// Postmark DOES NOT SIGN its webhook payloads — confirmed against their
// developer documentation on 2026-09-08, which states plainly that HMAC
// webhook signature verification is not supported and recommends HTTP
// basic auth on the webhook URL plus IP allowlisting instead.
//
// So this adapter deliberately implements no signature check. Inventing
// one would be security theatre: a receiver cannot verify a signature a
// sender never produces. What it does implement:
//
//   - the endpoint's MANDATORY basic auth, enforced by the handler
//     before this is reached and fail-closed since ADR-22;
//   - an optional source-IP allowlist against Postmark's published
//     egress addresses (PostmarkWebhookIPs);
//   - payload-shape validation, in Parse and Normalize.
//
// The security boundary that follows is stated in ADR-40 rather than
// papered over: anyone who learns the basic-auth credential can forge
// Postmark events, and the IP allowlist narrows but does not close
// that, because it authenticates the network path and not the payload.
func (a postmarkAdapter) Verify(r *http.Request, _ []byte) error {
	return verifySourceIP(a.allowedIPs, a.trusted, r)
}

// Parse: Postmark posts one event per request.
func (postmarkAdapter) Parse(body []byte) ([]RawEvent, error) {
	if len(body) == 0 {
		return nil, fmt.Errorf("lifecycle: empty postmark payload")
	}
	return []RawEvent{{Payload: body}}, nil
}

func (postmarkAdapter) Normalize(raw RawEvent, now time.Time) (DeliveryEvent, error) {
	ev, providerMessageID, err := NormalizePostmark(raw.Payload, now)
	if err != nil {
		return DeliveryEvent{}, err
	}
	meta := map[string]string{}
	if ev.Recipient != "" {
		// The domain, not the address: an event payload leaving
		// Posthorn should carry what a receiver needs to route it, not
		// a recipient list they can harvest (NFR44).
		if at := strings.LastIndex(ev.Recipient, "@"); at >= 0 {
			meta["recipient_domain"] = strings.ToLower(ev.Recipient[at+1:])
		}
	}
	meta["provider_event"] = ev.Event
	out := DeliveryEvent{
		Type:              unifyPostmark(ev.Event),
		Timestamp:         ev.Timestamp,
		Provider:          "postmark",
		ProviderMessageID: providerMessageID,
		// Postmark events are RECIPIENT-SCOPED: its documentation
		// states each event "is specific to a single recipient — a
		// message sent to several people produces several events". So
		// the address scopes the projection to that recipient alone.
		Recipient:        ev.Recipient,
		ProviderEventKey: raw.EventKey,
		Metadata:         meta,
	}
	if out.ProviderEventKey == "" {
		// No trace id on this delivery, so fall back to a key derived
		// from the event's identifying content. Two Postmark events
		// with the same type, message, recipient and timestamp are
		// indistinguishable to any receiver, so collapsing them is the
		// correct reading of a redelivery.
		out.ProviderEventKey = derivedEventKey("postmark", providerMessageID, string(out.Type), out.Timestamp, ev.Recipient)
	}
	return out, nil
}

// PostmarkTraceHeader is the per-delivery trace id Postmark sends,
// stable across retries of the same event — which makes it the right
// replay key when present.
//
// It is NOT an authenticity claim: the header is unauthenticated, and
// it is only consulted AFTER the request has passed the endpoint's
// mandatory basic auth and the adapter's IP allowlist. A forged trace
// id from an attacker who already holds the credential buys nothing a
// forged payload would not.
const PostmarkTraceHeader = "X-PM-Webhook-Trace-Id"

// ParseRequest implements RequestParser for Postmark: it lifts the
// trace id out of the request headers so redeliveries collapse on the
// provider's own identity rather than on a derived hash.
func (a postmarkAdapter) ParseRequest(r *http.Request, body []byte) ([]RawEvent, error) {
	events, err := a.Parse(body)
	if err != nil {
		return nil, err
	}
	trace := strings.TrimSpace(r.Header.Get(PostmarkTraceHeader))
	if len(trace) > 200 {
		// A hostile or broken sender must not be able to write an
		// unbounded value into the dedup index.
		trace = ""
	}
	if trace != "" {
		for i := range events {
			events[i].EventKey = "pm_" + trace
		}
	}
	return events, nil
}

func unifyPostmark(kind string) EventType {
	switch kind {
	case EventDelivered:
		return TypeDelivered
	case EventHardBounce:
		return TypeBounced
	case EventSoftBounce:
		return TypeDeferred
	case EventSpamComplaint:
		return TypeComplained
	default:
		return TypeOther
	}
}

// --- Resend -----------------------------------------------------------

// resendEvent is the subset of Resend's webhook payload Posthorn reads.
type resendEvent struct {
	Type      string `json:"type"`
	CreatedAt string `json:"created_at"`
	Data      struct {
		EmailID string   `json:"email_id"`
		To      []string `json:"to"`
	} `json:"data"`
}

type resendAdapter struct {
	key       []byte
	tolerance time.Duration
	now       func() time.Time
}

func (resendAdapter) Name() string { return "resend" }

// Verify implements the Standard Webhooks / Svix scheme Resend uses,
// confirmed against Resend's and Svix's documentation on 2026-09-08:
//
//	headers        svix-id, svix-timestamp, svix-signature
//	               (webhook-* aliases accepted per the spec)
//	signed content "{svix-id}.{svix-timestamp}.{raw body}"
//	key            base64-decode(secret without its "whsec_" prefix)
//	signature      base64(HMAC-SHA256), sent as a space-delimited list
//	               of "v1,<signature>" entries
//	timestamp      unix seconds, rejected beyond a 5-minute window
//
// The body is the raw request bytes. Decoding and re-encoding the JSON
// would reorder keys and normalise whitespace, and every signature
// would then fail — which is exactly the mistake the spec warns about.
func (a resendAdapter) Verify(r *http.Request, body []byte) error {
	if len(a.key) == 0 {
		// Refuse rather than pass: an adapter with no secret cannot
		// authenticate anything, and silently accepting would be the
		// looks-configured-but-isn't footgun.
		return &VerifyError{Reason: ReasonNotConfigured,
			Detail: "no signing_secret configured for the resend webhook"}
	}
	return verifySvix(a.key, r, body, a.tolerance, a.clock())
}

func (a resendAdapter) clock() time.Time {
	if a.now != nil {
		return a.now()
	}
	return time.Now()
}

func (resendAdapter) Parse(body []byte) ([]RawEvent, error) {
	if len(body) == 0 {
		return nil, fmt.Errorf("lifecycle: empty resend payload")
	}
	return []RawEvent{{Payload: body}}, nil
}

// ParseRequest implements RequestParser: the svix-id header is Resend's
// own event id, and the Standard Webhooks spec guarantees it is the
// same across every retry of one event — which makes it exactly the
// right replay key.
func (a resendAdapter) ParseRequest(r *http.Request, body []byte) ([]RawEvent, error) {
	events, err := a.Parse(body)
	if err != nil {
		return nil, err
	}
	id, _, _ := svixHeaders(r)
	for i := range events {
		events[i].EventKey = id
	}
	return events, nil
}

func (resendAdapter) Normalize(raw RawEvent, now time.Time) (DeliveryEvent, error) {
	var re resendEvent
	if err := json.Unmarshal(raw.Payload, &re); err != nil {
		return DeliveryEvent{}, fmt.Errorf("lifecycle: parse resend payload: %w", err)
	}
	if re.Type == "" {
		return DeliveryEvent{}, fmt.Errorf("lifecycle: resend payload has no type")
	}

	when := now
	if re.CreatedAt != "" {
		if parsed, err := time.Parse(time.RFC3339, re.CreatedAt); err == nil {
			when = parsed
		}
	}

	meta := map[string]string{"provider_event": re.Type}
	if len(re.Data.To) > 0 {
		if at := strings.LastIndex(re.Data.To[0], "@"); at >= 0 {
			meta["recipient_domain"] = strings.ToLower(re.Data.To[0][at+1:])
		}
	}
	return DeliveryEvent{
		Type:              unifyResend(re.Type),
		Timestamp:         when.UTC(),
		Provider:          "resend",
		ProviderMessageID: re.Data.EmailID,
		ProviderEventKey:  raw.EventKey,
		// Recipient is deliberately EMPTY: Resend events are
		// MESSAGE-scoped. One event carries the whole `to` array for
		// one email_id, so the outcome applies to every recipient of
		// the message rather than to one of them.
		Metadata: meta,
	}, nil
}

func unifyResend(kind string) EventType {
	switch kind {
	case "email.sent":
		return TypeSent
	case "email.delivered":
		return TypeDelivered
	case "email.delivery_delayed":
		return TypeDeferred
	case "email.bounced":
		return TypeBounced
	case "email.complained":
		return TypeComplained
	default:
		return TypeOther
	}
}

// derivedEventKey builds a stable replay key for providers that send no
// event id of their own. It is a hash so the key is fixed-width and
// carries no address: the recipient contributes to uniqueness without
// being stored (NFR44).
func derivedEventKey(parts ...any) string {
	h := sha256.New()
	for _, p := range parts {
		fmt.Fprintf(h, "%v\x00", p)
	}
	return "d_" + hex.EncodeToString(h.Sum(nil)[:16])
}

func init() {
	RegisterAdapter("postmark", func(cfg ProviderAuthConfig) (ProviderWebhookAdapter, error) {
		allowed, err := parseNetAllow(cfg.AllowedIPs)
		if err != nil {
			return nil, fmt.Errorf("allowed_ips: %w", err)
		}
		trusted, err := parseNetAllow(cfg.TrustedProxies)
		if err != nil {
			return nil, fmt.Errorf("trusted_proxies: %w", err)
		}
		if cfg.SigningSecret != "" {
			// Refuse rather than ignore: an operator who set a signing
			// secret believes payloads are being verified, and Postmark
			// does not sign them. Silently accepting the setting would
			// leave them with exactly the false confidence ADR-10
			// exists to prevent.
			return nil, errors.New("signing_secret is set, but Postmark does not sign webhook payloads — secure this endpoint with the mandatory basic auth and, optionally, allowed_ips (see the docs)")
		}
		return postmarkAdapter{allowedIPs: allowed, trusted: trusted}, nil
	})

	RegisterAdapter("resend", func(cfg ProviderAuthConfig) (ProviderWebhookAdapter, error) {
		key, err := parseSvixSecret(cfg.SigningSecret)
		if err != nil {
			return nil, fmt.Errorf("signing_secret: %w", err)
		}
		tolerance := cfg.Tolerance
		if tolerance <= 0 {
			tolerance = SvixTolerance
		}
		return resendAdapter{key: key, tolerance: tolerance}, nil
	})
}
