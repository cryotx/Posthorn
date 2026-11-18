package lifecycle

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"time"
)

// The unified delivery-event model (FR114).
//
// Every provider dialect normalizes into this one shape before it
// reaches any business logic, so a project integrating with Posthorn
// never learns whether its mail went through Resend, SES or Mailgun —
// which is the entire point of a gateway. Provider payloads may be
// summarised into Metadata; nothing downstream may branch on a
// provider's own field names.
type EventType string

// Unified event types. The set is closed and provider-agnostic: an
// adapter that meets an event it cannot place maps it to TypeOther
// rather than inventing a type, so a provider adding events can never
// break ingestion or surprise a receiver.
const (
	// TypeAccepted is Posthorn accepting a submission, before any
	// provider has seen it. Emitted by the gateway, not by a webhook.
	TypeAccepted EventType = "message.accepted"

	// TypeSent is a provider accepting the message for delivery.
	TypeSent EventType = "message.sent"

	// TypeDelivered is confirmed delivery to the recipient's server.
	TypeDelivered EventType = "message.delivered"

	// TypeDeferred is a temporary failure the provider will retry.
	TypeDeferred EventType = "message.deferred"

	// TypeBounced is a permanent delivery failure.
	TypeBounced EventType = "message.bounced"

	// TypeComplained is a recipient spam complaint.
	TypeComplained EventType = "message.complained"

	// TypeFailed is Posthorn failing to hand the message to any
	// provider. Emitted by the gateway.
	TypeFailed EventType = "message.failed"

	// TypeOther is a provider event outside the set above. Forwarded
	// so a receiver can see it happened, never acted on.
	TypeOther EventType = "message.other"
)

// KnownTypes lists the unified types, for docs and validation.
func KnownTypes() []EventType {
	return []EventType{
		TypeAccepted, TypeSent, TypeDelivered, TypeDeferred,
		TypeBounced, TypeComplained, TypeFailed, TypeOther,
	}
}

// DeliveryEvent is the unified event — what Posthorn stores, counts,
// and POSTs to a project's webhook.
type DeliveryEvent struct {
	// EventID is unique per event and stable across delivery retries,
	// so a receiver can deduplicate and reject replays. It is inside
	// the signed body, which is what makes that guarantee usable.
	EventID string `json:"event_id"`

	// MessageID is the Posthorn message id — one logical message, one
	// id, whichever provider ended up carrying it (ADR-34).
	MessageID string `json:"message_id"`

	// Project owns the message. Empty for events on messages sent
	// outside project mode.
	Project string `json:"project,omitempty"`

	// Provider is the pool provider that handled the message.
	Provider string `json:"provider,omitempty"`

	// ProviderMessageID is the upstream's own id, for operators who
	// need to open the provider's dashboard.
	ProviderMessageID string `json:"provider_message_id,omitempty"`

	// Recipient is the single address this event concerns, for
	// providers that scope events per recipient (Postmark). Empty for
	// message-scoped providers (Resend), where the event applies to
	// every recipient of the message.
	//
	// Not serialised to receivers: the outbound event carries only a
	// recipient DOMAIN (NFR44). This field feeds the recipient-scoped
	// projection, and the address it holds is already in the
	// submission row, so it exposes nothing new.
	Recipient string `json:"-"`

	// ProviderEventKey is the provider's own identifier for THIS
	// EVENT, stable across the provider's delivery retries (FR122).
	// It is the replay key: a redelivery carries the same value and is
	// collapsed rather than reprocessed.
	//
	// Providers that mint one supply it directly (Resend's svix-id);
	// providers that do not get a key derived from the event's
	// identifying content. Not serialised to receivers — it is
	// Posthorn's deduplication state, not part of the event contract,
	// and EventID is what a receiver deduplicates on.
	ProviderEventKey string `json:"-"`

	// Type is one of the unified constants above.
	Type EventType `json:"type"`

	// Timestamp is when the event occurred, best-effort from the
	// provider; ingestion time when it omits or mangles one.
	Timestamp time.Time `json:"timestamp"`

	// Metadata carries a small, normalized summary — recipient domain,
	// bounce class, provider status text. Never the message body, and
	// never the provider's raw payload: a receiver that could couple to
	// a provider's field names would defeat the abstraction.
	Metadata map[string]string `json:"metadata,omitempty"`
}

// NewEventID mints a unique event id. Hex rather than UUID so it is
// visibly a Posthorn identifier and needs no extra dependency; 128 bits
// of randomness, so collision is not a real concern.
func NewEventID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failing is not recoverable and not maskable;
		// fall back to a time-based id so the event still ships.
		return "ev_" + hex.EncodeToString([]byte(time.Now().UTC().Format(time.RFC3339Nano)))
	}
	return "ev_" + hex.EncodeToString(b[:])
}

// Encode renders the event as the signed webhook body.
func (e DeliveryEvent) Encode() ([]byte, error) {
	return json.Marshal(e)
}
