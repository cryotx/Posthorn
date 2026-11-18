package lifecycle

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Brevo delivery-event adapter.
//
// Built against Brevo's published webhook documentation on 2026-09-08,
// not from memory:
//
//   - one event object per request (not a batch);
//   - fields "event", "email", "id", "date", "ts", "ts_event",
//     "ts_epoch", "message-id", "subject", "tag"/"tags", "reason";
//   - email event names: request, delivered, deferred, soft_bounce,
//     hard_bounce, blocked, spam, invalid_email, error, unsubscribed,
//     opened, unique_opened, click, proxy_open, unique_proxy_open.
//
// AUTHENTICATION, stated plainly: Brevo publishes NO cryptographic
// payload signature. Its documented options are basic auth embedded in
// the notify URL, a bearer token or custom headers it will send, and
// restricting the endpoint to its published IP ranges. So this adapter
// implements no signature check — a receiver cannot verify a signature
// the sender never produces — and relies on:
//
//   - the endpoint's MANDATORY basic auth, enforced by the handler
//     before Verify is reached and fail-closed since ADR-22;
//   - an optional source-IP allowlist the operator configures.
//
// The resulting boundary is the same as Postmark's and is recorded the
// same way: anyone holding the basic-auth credential can forge Brevo
// events, and an IP allowlist narrows the path without authenticating
// the payload. The capability matrix says webhook_verification =
// unsupported for exactly this reason.
//
// No IP preset ships for Brevo. Its ranges live in a help-centre
// article rather than the API reference, and pinning values Posthorn
// cannot re-verify at build time would be worse than making the
// operator copy the current list into allowed_ips themselves.

// brevoEvent is the subset of Brevo's webhook payload Posthorn reads.
//
// Timestamps: "ts_event" and "ts" are unix seconds, "date" is a
// human-formatted local string. The numeric fields are preferred
// because "date" carries a timezone offset format that varies.
type brevoEvent struct {
	Event     string `json:"event"`
	Email     string `json:"email"`
	MessageID string `json:"message-id"`
	TsEvent   int64  `json:"ts_event"`
	Ts        int64  `json:"ts"`
	TsEpoch   int64  `json:"ts_epoch"`
	Date      string `json:"date"`
	Reason    string `json:"reason"`
	ID        int64  `json:"id"`
}

type brevoAdapter struct {
	allowedIPs []netAllow
	trusted    []netAllow
}

func (brevoAdapter) Name() string { return "brevo" }

// Verify checks only what Brevo actually offers: the request's source
// address, when the operator configured an allowlist. See the file
// comment for why there is no signature check here.
func (a brevoAdapter) Verify(r *http.Request, _ []byte) error {
	return verifySourceIP(a.allowedIPs, a.trusted, r)
}

// Parse: Brevo posts one event per request.
func (brevoAdapter) Parse(body []byte) ([]RawEvent, error) {
	if len(body) == 0 {
		return nil, fmt.Errorf("lifecycle: empty brevo payload")
	}
	return []RawEvent{{Payload: body}}, nil
}

func (brevoAdapter) Normalize(raw RawEvent, now time.Time) (DeliveryEvent, error) {
	var be brevoEvent
	if err := json.Unmarshal(raw.Payload, &be); err != nil {
		return DeliveryEvent{}, fmt.Errorf("lifecycle: parse brevo payload: %w", err)
	}
	if be.Event == "" {
		return DeliveryEvent{}, errors.New("lifecycle: brevo payload has no event")
	}

	when := now
	for _, sec := range []int64{be.TsEvent, be.Ts, be.TsEpoch} {
		if sec > 0 {
			// ts_epoch is milliseconds in Brevo's marketing events;
			// a value far beyond any plausible second-precision unix
			// time is therefore read as milliseconds.
			if sec > 1e12 {
				when = time.Unix(sec/1000, (sec%1000)*int64(time.Millisecond))
			} else {
				when = time.Unix(sec, 0)
			}
			break
		}
	}

	meta := map[string]string{"provider_event": be.Event}
	if at := strings.LastIndex(be.Email, "@"); at >= 0 {
		// Domain only, never the address (NFR44).
		meta["recipient_domain"] = strings.ToLower(be.Email[at+1:])
	}
	if be.Reason != "" {
		meta["provider_reason"] = truncateReason(be.Reason)
	}

	out := DeliveryEvent{
		Type:              unifyBrevo(be.Event),
		Timestamp:         when.UTC(),
		Provider:          "brevo",
		ProviderMessageID: be.MessageID,
		// Brevo events are RECIPIENT-SCOPED: each carries the single
		// "email" the outcome concerns, so a bounce for one recipient
		// must not overwrite another recipient's delivery.
		Recipient:        be.Email,
		ProviderEventKey: raw.EventKey,
		Metadata:         meta,
	}
	if out.ProviderEventKey == "" {
		// Brevo sends no stable per-delivery event id ("id" identifies
		// the message, not this callback), so the replay key is derived
		// from the event's identifying content — two Brevo callbacks
		// with the same type, message, recipient and timestamp are
		// indistinguishable to any receiver.
		out.ProviderEventKey = derivedEventKey("brevo", be.MessageID, string(out.Type), out.Timestamp, be.Email)
	}
	return out, nil
}

// truncateReason bounds a provider-supplied string before it is stored
// and forwarded. Brevo's reason text is provider prose, but it can
// quote the recipient or the subject, and metadata crosses to project
// webhooks.
func truncateReason(s string) string {
	const max = 200
	s = strings.TrimSpace(s)
	if len(s) > max {
		return s[:max]
	}
	return s
}

// unifyBrevo maps Brevo's event names onto the unified vocabulary.
//
// Engagement events (opens, clicks) map to TypeOther deliberately:
// Posthorn projects DELIVERY state, and treating an open as a delivery
// signal would put marketing analytics in a transactional gateway
// (design principle 3).
func unifyBrevo(kind string) EventType {
	switch kind {
	case "request":
		// Brevo has accepted the message for sending; the mail has not
		// left yet. That is the "sent" step, not delivery.
		return TypeSent
	case "delivered":
		return TypeDelivered
	case "deferred", "soft_bounce":
		return TypeDeferred
	case "hard_bounce", "invalid_email", "blocked":
		return TypeBounced
	case "spam":
		return TypeComplained
	case "error":
		return TypeFailed
	default:
		// opened, unique_opened, click, proxy_open, unique_proxy_open,
		// unsubscribed, and anything Brevo adds later.
		return TypeOther
	}
}

// brevoUnsupportedSigning is the message an operator sees when they
// configure a signing secret Brevo will never use. Refusing rather than
// ignoring is the ADR-10 posture: a defense you believe is active but
// isn't is worse than no defense.
const brevoUnsupportedSigning = "signing_secret is set, but Brevo does not sign webhook payloads — " +
	"secure this endpoint with the mandatory basic auth and, optionally, allowed_ips " +
	"(Brevo publishes its webhook IP ranges in its help centre)"

func init() {
	RegisterAdapter("brevo", func(cfg ProviderAuthConfig) (ProviderWebhookAdapter, error) {
		allowed, err := parseNetAllow(cfg.AllowedIPs)
		if err != nil {
			return nil, fmt.Errorf("allowed_ips: %w", err)
		}
		trusted, err := parseNetAllow(cfg.TrustedProxies)
		if err != nil {
			return nil, fmt.Errorf("trusted_proxies: %w", err)
		}
		if cfg.SigningSecret != "" {
			return nil, errors.New(brevoUnsupportedSigning)
		}
		return brevoAdapter{allowedIPs: allowed, trusted: trusted}, nil
	})
}
