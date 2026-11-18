package transport

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Brevo transactional-email transport.
//
// Implemented against Brevo's published API reference on 2026-09-08,
// not from memory: POST https://api.brevo.com/v3/smtp/email, the API
// key in an `api-key` header (NOT Authorization: Bearer), and a 201
// response carrying {"messageId": "..."}.
//
// Why a native adapter rather than the generic SMTP relay, per the
// v1.0 rule that a native provider must earn its keep: Brevo's API
// returns a RELIABLE per-message id, and its webhooks carry that same
// id for correlation. Over SMTP, the message id is whatever the relay
// chooses to report at hand-off, which cannot be joined to a delivery
// callback. That join is the whole basis of Posthorn's attempt trail
// and delivery projection.
//
// Per ADR-1: bespoke client, no vendor SDK.
//
// NFR1 enforcement: every submitter-controlled value reaches Brevo
// through json.Marshal of a typed struct. Nothing in this file builds a
// header or an address by string concatenation.

const (
	defaultBrevoBaseURL = "https://api.brevo.com"
	brevoSendPath       = "/v3/smtp/email"
	brevoRequestTimeout = 5 * time.Second

	// brevoResponseSizeLimit caps what we read into memory. Success
	// bodies are a few dozen bytes; this stops a hostile or broken
	// upstream forcing an allocation.
	brevoResponseSizeLimit = 64 * 1024
)

var brevoHTTPClient = &http.Client{Timeout: brevoRequestTimeout}

// BrevoTransport implements Transport against Brevo's JSON API.
type BrevoTransport struct {
	// APIKey is sent in the `api-key` header. Never logged (NFR3).
	APIKey string

	// BaseURL is the API base. Empty means the production default;
	// only tests set it.
	BaseURL string
}

// NewBrevoTransport constructs a transport. baseURL may be empty.
func NewBrevoTransport(apiKey, baseURL string) *BrevoTransport {
	if baseURL == "" {
		baseURL = defaultBrevoBaseURL
	}
	return &BrevoTransport{APIKey: apiKey, BaseURL: baseURL}
}

// brevoAddress is Brevo's {email, name} address shape.
type brevoAddress struct {
	Email string `json:"email"`
	Name  string `json:"name,omitempty"`
}

// brevoRequest is the payload for /v3/smtp/email.
//
// SECURITY: adding a field here changes what submitter-controlled data
// crosses to Brevo. Review NFR1 before doing so.
type brevoRequest struct {
	Sender      brevoAddress   `json:"sender"`
	To          []brevoAddress `json:"to"`
	Subject     string         `json:"subject"`
	HTMLContent string         `json:"htmlContent,omitempty"`
	TextContent string         `json:"textContent,omitempty"`
	ReplyTo     *brevoAddress  `json:"replyTo,omitempty"`
	Attachment  []brevoAttach  `json:"attachment,omitempty"`
}

// brevoAttach is Brevo's base64 attachment shape.
type brevoAttach struct {
	Content string `json:"content"`
	Name    string `json:"name"`
}

type brevoResponse struct {
	MessageID string `json:"messageId"`
}

type brevoError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Send implements Transport.
func (b *BrevoTransport) Send(ctx context.Context, msg Message) (SendResult, error) {
	sender, err := splitAddress(msg.From)
	if err != nil {
		return SendResult{}, &TransportError{
			Class: ErrTerminal, Message: "invalid from address", Cause: err,
		}
	}
	req := brevoRequest{
		Sender:  brevoAddress{Email: sender.address, Name: sender.name},
		Subject: msg.Subject,
	}
	for _, to := range msg.To {
		parsed, perr := splitAddress(to)
		if perr != nil {
			return SendResult{}, &TransportError{
				Class: ErrTerminal, Message: "invalid recipient address", Cause: perr,
			}
		}
		req.To = append(req.To, brevoAddress{Email: parsed.address, Name: parsed.name})
	}
	// Brevo requires htmlContent when no templateId is used. A
	// text-only Posthorn message therefore has to carry something in
	// the HTML slot; sending the text as both parts keeps the message
	// faithful without inventing markup around submitter content.
	req.TextContent = msg.BodyText
	if msg.BodyHTML != "" {
		req.HTMLContent = msg.BodyHTML
	} else {
		req.HTMLContent = htmlEscapedParagraph(msg.BodyText)
	}
	if msg.ReplyTo != "" {
		if parsed, perr := splitAddress(msg.ReplyTo); perr == nil {
			req.ReplyTo = &brevoAddress{Email: parsed.address, Name: parsed.name}
		}
	}
	for _, a := range msg.Attachments {
		req.Attachment = append(req.Attachment, brevoAttach{
			Content: base64.StdEncoding.EncodeToString(a.Data), Name: a.Filename,
		})
	}

	buf, err := json.Marshal(req)
	if err != nil {
		return SendResult{}, &TransportError{
			Class: ErrTerminal, Message: "encode brevo request", Cause: err,
		}
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, b.BaseURL+brevoSendPath, bytes.NewReader(buf))
	if err != nil {
		return SendResult{}, &TransportError{
			Class: ErrTerminal, Message: "build brevo request", Cause: err,
		}
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	// Brevo authenticates with its own header name, not Bearer.
	httpReq.Header.Set("api-key", b.APIKey)

	resp, err := brevoHTTPClient.Do(httpReq)
	if err != nil {
		return SendResult{}, &TransportError{
			Class: ErrTransient, Message: "brevo request failed", Cause: err,
		}
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, brevoResponseSizeLimit))

	switch {
	case resp.StatusCode == http.StatusCreated || resp.StatusCode == http.StatusAccepted || resp.StatusCode == http.StatusOK:
		var decoded brevoResponse
		// A parse failure is a non-fatal degradation: the mail was
		// accepted, we just lose the id for correlation.
		_ = json.Unmarshal(body, &decoded)
		return SendResult{MessageID: decoded.MessageID}, nil

	case resp.StatusCode == http.StatusTooManyRequests:
		return SendResult{}, &TransportError{
			Class: ErrRateLimited, Status: resp.StatusCode,
			Message: "brevo rate limited",
		}

	case resp.StatusCode >= 500:
		return SendResult{}, &TransportError{
			Class: ErrTransient, Status: resp.StatusCode,
			Message: "brevo server error",
		}

	default:
		// 4xx other than 429: the message or the credential is the
		// problem, and neither is fixed by trying again or by trying
		// somewhere else (ADR-27).
		var be brevoError
		_ = json.Unmarshal(body, &be)
		detail := "brevo rejected the message"
		if be.Code != "" {
			// The provider's own error CODE is safe to surface; its
			// message may echo submitted content, so it does not go in.
			detail = "brevo rejected the message (" + be.Code + ")"
		}
		return SendResult{}, &TransportError{
			Class: ErrTerminal, Status: resp.StatusCode, Message: detail,
		}
	}
}

func init() {
	Register(Registration{
		Type: "brevo",
		Validate: func(settings map[string]any) error {
			key, _ := settings["api_key"].(string)
			if key == "" {
				return fmt.Errorf("brevo transport requires settings.api_key")
			}
			return nil
		},
		Build: func(settings map[string]any) (Transport, error) {
			key, _ := settings["api_key"].(string)
			baseURL, _ := settings["base_url"].(string)
			return NewBrevoTransport(key, baseURL), nil
		},
	})
}
