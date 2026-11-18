// Modified for the cryotx/posthorn distribution.

package transport

import (
	"context"
	"fmt"
)

// Message is the canonical form of an email passed to a Transport.
//
// All fields are structured data. Transport implementations MUST pass these
// fields to their underlying email API as separate, structured values — never
// concatenate them into raw protocol headers. This is the architectural
// enforcement of NFR1 (no header injection): if every transport handles
// header construction through library APIs that escape headers properly,
// submitter-controlled content cannot smuggle headers into outbound mail.
//
// BodyHTML is set only for endpoints configured with body_format = "html"
// (FR71). When non-empty, BodyText carries the plain-text alternative —
// transports send both as a multipart pair. BodyText is never empty on an
// HTML send: the renderer auto-derives it when no explicit text_body
// template is configured (FR72).
type Message struct {
	From     string
	To       []string
	ReplyTo  string
	Subject  string
	BodyText string
	BodyHTML string

	// SubmissionID and Fields cross the boundary for structured sinks
	// (ADR-24 amending ADR-12; FR89). SubmissionID is Posthorn's
	// submission UUID — set by both ingresses and the retry worker so
	// at-least-once replays carry a stable dedup key. Fields is the raw
	// submitted key/values, populated by HTTP ingresses and nil from the
	// SMTP listener. Mail transports MUST ignore both, and neither may
	// ever be interpolated into any header at any layer (NFR1).
	SubmissionID string
	Fields       map[string][]string

	// Kind labels what this message is for (FR104) — verification,
	// password_reset, notification, important, marketing, or the
	// unspecified zero value. It is a ROUTING input only: transports
	// must ignore it, and like SubmissionID and Fields it may never be
	// interpolated into a header (NFR1).
	//
	// Set from the endpoint's `mail_kind`, overridable per request in
	// api-mode against the closed enum.
	Kind MailKind

	// Attachments carries files accepted by an opt-in
	// [endpoints.attachments] endpoint (FR90-FR92, ADR-25). ContentType
	// is the SNIFFED type of Data — enforcement and outbound declaration
	// both use it; the client-declared type never crosses this boundary.
	// Filenames are structured API/MIME parameter values only, never
	// header-line material (NFR1).
	Attachments []Attachment
}

// Attachment is one file crossing to a transport.
type Attachment struct {
	Filename    string
	ContentType string
	Data        []byte
}

// SendResult is the per-call metadata returned from a successful Send.
//
// MessageID is the upstream provider's identifier for the message — e.g.,
// Postmark's response.MessageID, SES's MessageId, Mailgun's id. Posthorn
// surfaces it in the submission_sent log so an operator triaging a missing
// email can grep posthorn logs for the submission UUID and jump straight
// to the provider's UI to inspect delivery state.
//
// Empty when the transport doesn't expose a message ID, when parsing it
// failed (a non-fatal degradation), or on transports that haven't grown
// the support yet.
type SendResult struct {
	MessageID string

	// Provider is the pool name of the entry that actually delivered
	// the message ("postmark-prod"). Set only by the multi-provider
	// Router; an endpoint configured with a single inline transport
	// leaves it empty and every downstream label falls back to the
	// transport type, so v1.x deployments see no change.
	Provider string
}

// Transport sends a Message. v1.0 ships one implementation (Postmark);
// the interface exists so Resend, Mailgun, SES, and outbound SMTP can
// be added in v1.1+ without touching handler logic or config schema (FR4).
//
// Implementations MUST return a *TransportError on failure so the handler
// can classify retries (FR18-20). Returning a bare error is a contract bug.
// On success, implementations SHOULD populate SendResult.MessageID when the
// upstream provider exposes one; an empty MessageID is acceptable.
type Transport interface {
	Send(ctx context.Context, msg Message) (SendResult, error)
}

// ErrorClass classifies transport errors for the retry policy.
//
// The handler maps each class to a retry decision:
//
//	ErrTransient      → retry once after 1s   (network errors, 5xx)
//	ErrRateLimited    → retry once after 5s   (429 from upstream)
//	ErrTerminal       → no retry, log + 502   (4xx other than 429)
//	ErrQuotaExhausted → no retry, log + 503   (every provider out of budget)
//	ErrUnknown        → treated as terminal   (defensive default)
type ErrorClass int

const (
	// ErrUnknown is the zero value. A TransportError with this class is a
	// contract bug — implementations should always set a specific class.
	ErrUnknown ErrorClass = iota

	// ErrTransient is for failures that may succeed on retry: network errors,
	// connection timeouts, upstream 5xx responses.
	ErrTransient

	// ErrRateLimited is for 429 responses from the upstream provider. Retried
	// after a longer backoff to give the provider time to recover.
	ErrRateLimited

	// ErrTerminal is for failures that won't succeed on retry: 4xx responses
	// (other than 429), malformed config, authentication errors.
	ErrTerminal

	// ErrQuotaExhausted means every candidate provider has spent its
	// configured send budget (FR99). It is deliberately its own class
	// rather than a flavour of transient: nothing is wrong with the
	// providers, so no circuit breaker may trip, and no amount of
	// inline retrying inside a 10s request will help — the budget
	// resets on a calendar boundary, not a backoff.
	//
	// It is still deferrable: the message is fine and will send once
	// the window rolls over, so the FR78 queue holds it.
	ErrQuotaExhausted
)

// String returns a stable label for log fields (NFR7 error_class).
func (c ErrorClass) String() string {
	switch c {
	case ErrTransient:
		return "transient"
	case ErrRateLimited:
		return "rate_limited"
	case ErrTerminal:
		return "terminal"
	case ErrQuotaExhausted:
		return "quota_exhausted"
	default:
		return "unknown"
	}
}

// TransportError is the error type all Transport implementations must return
// on failure. Wraps an underlying cause and carries metadata the handler
// uses for retry classification and structured logging.
type TransportError struct {
	// Class is the retry classification. Required.
	Class ErrorClass

	// Status is the upstream HTTP status code if applicable, 0 otherwise.
	Status int

	// Cause is the underlying error (network failure, JSON decode error, etc.).
	// May be nil when the error originates from a status-code mapping with no
	// underlying Go error.
	Cause error

	// Message is a short operator-facing description. Should not contain
	// secrets (API keys, request bodies). NFR3.
	Message string

	// Provider is the pool name of the provider that produced this
	// error, stamped by the Router. Empty for a single inline
	// transport. Operator-configured content only (NFR24).
	Provider string
}

// Error implements the error interface. Format is intentionally compact and
// safe to log: "transport: <message> (class=<class> status=<status>): <cause>".
func (e *TransportError) Error() string {
	if e == nil {
		return "<nil>"
	}
	base := fmt.Sprintf("transport: %s (class=%s", e.Message, e.Class)
	if e.Provider != "" {
		base += fmt.Sprintf(" provider=%s", e.Provider)
	}
	if e.Status != 0 {
		base += fmt.Sprintf(" status=%d", e.Status)
	}
	base += ")"
	if e.Cause != nil {
		base += ": " + e.Cause.Error()
	}
	return base
}

// Unwrap supports errors.Is / errors.As traversal to the underlying cause.
func (e *TransportError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}
