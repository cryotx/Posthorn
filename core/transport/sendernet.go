package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Sender.net transactional-email transport.
//
// Implemented against Sender.net's published API reference on
// 2026-09-11, not from memory: POST https://api.sender.net/v2/message/send,
// Authorization: Bearer <token>, structured {from:{email,name},
// to:{email,name}, subject, html, text}, and a 200 response carrying
// {"success": true, "emailId": "..."}.
//
// Why a native adapter rather than the generic SMTP relay Sender.net
// also offers, per the v1.0 rule that a native provider must earn its
// keep: the REST API returns a real per-message id (`emailId`) in the
// same response that confirms acceptance, and typed field-level
// validation errors (422 with a per-field errors object) rather than an
// SMTP server's free-text reply. The credential Posthorn is given here
// is an API token, not an SMTP username/password, so the REST path is
// also what the held credential actually authenticates.
//
// What it does NOT offer, recorded honestly rather than assumed: no
// documented webhook payload signature, no documented rate-limit
// number, and `to` is a single object rather than an array — the
// endpoint sends to exactly one recipient per call, same constraint as
// Cloudflare Email.
//
// Per ADR-1: bespoke client, no vendor SDK.
//
// NFR1 enforcement: every submitter-controlled value reaches
// Sender.net through json.Marshal of a typed struct. Nothing in this
// file builds a header or an address by string concatenation.

const (
	defaultSenderNetBaseURL = "https://api.sender.net"
	senderNetSendPath       = "/v2/message/send"
	senderNetRequestTimeout = 5 * time.Second

	// senderNetResponseSizeLimit caps what we read into memory. Success
	// bodies are a few dozen bytes; this stops a hostile or broken
	// upstream forcing an allocation.
	senderNetResponseSizeLimit = 64 * 1024
)

var senderNetHTTPClient = &http.Client{Timeout: senderNetRequestTimeout}

// SenderNetTransport implements Transport against Sender.net's JSON API.
type SenderNetTransport struct {
	// APIToken is sent as Authorization: Bearer. Never logged (NFR3).
	APIToken string

	// BaseURL is the API base. Empty means the production default;
	// only tests set it.
	BaseURL string
}

// NewSenderNetTransport constructs a transport. baseURL may be empty.
func NewSenderNetTransport(apiToken, baseURL string) *SenderNetTransport {
	if baseURL == "" {
		baseURL = defaultSenderNetBaseURL
	}
	return &SenderNetTransport{APIToken: apiToken, BaseURL: baseURL}
}

// senderNetAddress is Sender.net's {email, name} address shape.
type senderNetAddress struct {
	Email string `json:"email"`
	Name  string `json:"name,omitempty"`
}

// senderNetRequest is the payload for /v2/message/send.
//
// SECURITY: adding a field here changes what submitter-controlled data
// crosses to Sender.net. Review NFR1 before doing so. `headers` is
// deliberately NOT populated from anything submitter-controlled — see
// the file comment.
type senderNetRequest struct {
	From    senderNetAddress `json:"from"`
	To      senderNetAddress `json:"to"`
	Subject string           `json:"subject"`
	HTML    string           `json:"html,omitempty"`
	Text    string           `json:"text,omitempty"`
}

type senderNetResponse struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
	EmailID string `json:"emailId"`
}

// senderNetValidationError is the documented 422 shape:
// {"message": "...", "errors": {"email": ["Required value, email"]}}.
type senderNetValidationError struct {
	Errors map[string][]string `json:"errors"`
}

// Send implements Transport.
func (t *SenderNetTransport) Send(ctx context.Context, msg Message) (SendResult, error) {
	if len(msg.To) == 0 {
		return SendResult{}, &TransportError{Class: ErrTerminal, Message: "no recipients"}
	}
	// The documented schema takes a single `to` object, not an array.
	// Posthorn does not fan one submission into several provider calls
	// — that would produce several provider outcomes for one message
	// id, which is deliberately out of scope (same rule as Cloudflare).
	if len(msg.To) > 1 {
		return SendResult{}, &TransportError{
			Class: ErrTerminal,
			Message: fmt.Sprintf("sender.net accepts one recipient per send, got %d — "+
				"use a provider with multi-recipient support for this endpoint", len(msg.To)),
		}
	}

	from, err := splitAddress(msg.From)
	if err != nil {
		return SendResult{}, &TransportError{
			Class: ErrTerminal, Message: "invalid from address", Cause: err,
		}
	}
	to, err := splitAddress(msg.To[0])
	if err != nil {
		return SendResult{}, &TransportError{
			Class: ErrTerminal, Message: "invalid recipient address", Cause: err,
		}
	}

	req := senderNetRequest{
		From:    senderNetAddress{Email: from.address, Name: from.name},
		To:      senderNetAddress{Email: to.address, Name: to.name},
		Subject: msg.Subject,
		Text:    msg.BodyText,
		HTML:    msg.BodyHTML,
	}
	// Sender.net documents neither field as required, but an empty
	// html leaves a text-only message with nothing in that slot on
	// clients that prefer HTML. Escaping first, same guarantee ADR-19
	// gives the HTML renderer: submitter content is inert markup by
	// construction.
	if req.HTML == "" {
		req.HTML = htmlEscapedParagraph(msg.BodyText)
	}

	buf, err := json.Marshal(req)
	if err != nil {
		return SendResult{}, &TransportError{
			Class: ErrTerminal, Message: "encode sender.net request", Cause: err,
		}
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		t.BaseURL+senderNetSendPath, bytes.NewReader(buf))
	if err != nil {
		return SendResult{}, &TransportError{
			Class: ErrTerminal, Message: "build sender.net request", Cause: err,
		}
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+t.APIToken)

	resp, err := senderNetHTTPClient.Do(httpReq)
	if err != nil {
		return SendResult{}, &TransportError{
			Class: ErrTransient, Message: "sender.net request failed", Cause: err,
		}
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, senderNetResponseSizeLimit))

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		var decoded senderNetResponse
		// A parse failure or success:false is a non-fatal degradation
		// only when the status line already said 2xx — Sender.net's own
		// docs show success as part of a 200, so treat a malformed body
		// as an accepted-but-uncorrelated send rather than a failure.
		_ = json.Unmarshal(body, &decoded)
		return SendResult{MessageID: decoded.EmailID}, nil

	case resp.StatusCode == http.StatusUnauthorized:
		return SendResult{}, &TransportError{
			Class: ErrTerminal, Status: resp.StatusCode,
			Message: "sender.net rejected the credential",
		}

	case resp.StatusCode == http.StatusTooManyRequests:
		return SendResult{}, &TransportError{
			Class: ErrRateLimited, Status: resp.StatusCode,
			Message: "sender.net rate limited",
		}

	case resp.StatusCode == http.StatusUnprocessableEntity:
		// 422: the message itself is the problem. Surface the invalid
		// FIELD NAMES only — the documented message strings are
		// Sender.net's own validation prose and may echo submitted
		// content (e.g. "invalid email: attacker@evil.com\r\n..."),
		// and a transport error is logged (NFR3-adjacent: never log
		// submitter content as if it were operator-safe).
		var verr senderNetValidationError
		_ = json.Unmarshal(body, &verr)
		detail := "sender.net rejected the message"
		if len(verr.Errors) > 0 {
			fields := make([]string, 0, len(verr.Errors))
			for f := range verr.Errors {
				fields = append(fields, f)
			}
			detail += " (invalid: " + strings.Join(fields, ", ") + ")"
		}
		return SendResult{}, &TransportError{
			Class: ErrTerminal, Status: resp.StatusCode, Message: detail,
		}

	case resp.StatusCode >= 500:
		return SendResult{}, &TransportError{
			Class: ErrTransient, Status: resp.StatusCode,
			Message: "sender.net server error",
		}

	default:
		// Every response shape Sender.net documents carries a top-level
		// "message" field, and unlike the per-field 422 detail (which
		// can echo submitted content back), this one is consistently a
		// fixed, account-state classification string — "Sending domain
		// not found" being the case that motivated surfacing it: an
		// operator debugging a 404 needs that sentence, and it never
		// contains anything the submitter wrote.
		var generic senderNetResponse
		_ = json.Unmarshal(body, &generic)
		detail := "sender.net rejected the request"
		if generic.Message != "" {
			detail = "sender.net: " + generic.Message
		}
		return SendResult{}, &TransportError{
			Class: ErrTerminal, Status: resp.StatusCode, Message: detail,
		}
	}
}

func init() {
	Register(Registration{
		Type: "sendernet",
		Validate: func(settings map[string]any) error {
			token, _ := settings["api_token"].(string)
			if token == "" {
				return fmt.Errorf("sendernet transport requires settings.api_token")
			}
			return nil
		},
		Build: func(settings map[string]any) (Transport, error) {
			token, _ := settings["api_token"].(string)
			baseURL, _ := settings["base_url"].(string)
			return NewSenderNetTransport(token, baseURL), nil
		},
	})
}
