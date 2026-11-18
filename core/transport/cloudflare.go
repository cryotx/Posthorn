package transport

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Cloudflare Email Service transport.
//
// Implemented against Cloudflare's published REST reference on
// 2026-09-08, not from memory:
//
//	POST https://api.cloudflare.com/client/v4/accounts/{account_id}/email/sending/send
//	Authorization: Bearer <API token>
//	{to, from, subject, html, text, attachments[], headers{}}
//
// Why a native adapter rather than Cloudflare's SMTP endpoint, per the
// v1.0 rule that a native provider must earn its keep: the REST send
// returns PER-RECIPIENT DELIVERY STATUS SYNCHRONOUSLY —
// result.{delivered, permanent_bounces, queued} — so a permanent bounce
// is known at hand-off instead of waiting for a callback that
// Cloudflare does not send. Combined with its numeric error codes,
// that is delivery metadata and classification precision an SMTP relay
// cannot offer.
//
// What it does NOT offer, recorded honestly in the capability matrix:
// no per-message id (the response is grouped by recipient, not by
// message), no per-message delivery webhook (Cloudflare Notifications
// are account-level alerts), and no queryable send-quota API.
//
// Per ADR-1: bespoke client, no vendor SDK.

const (
	defaultCloudflareBaseURL = "https://api.cloudflare.com"
	cloudflareRequestTimeout = 5 * time.Second
	cloudflareResponseLimit  = 64 * 1024

	// cloudflareMaxMessageBytes is the documented 5 MiB total message
	// cap, attachments included. Checked locally so an oversize message
	// fails fast and terminally rather than after a wasted round trip.
	cloudflareMaxMessageBytes = 5 << 20
)

var cloudflareHTTPClient = &http.Client{Timeout: cloudflareRequestTimeout}

// CloudflareTransport implements Transport against Cloudflare Email
// Service's REST API.
type CloudflareTransport struct {
	// AccountID is the Cloudflare account the sending domain lives in.
	AccountID string

	// APIToken is sent as Authorization: Bearer. Never logged (NFR3).
	APIToken string

	// BaseURL is the API base. Empty means production; only tests set it.
	BaseURL string
}

// NewCloudflareTransport constructs a transport. baseURL may be empty.
func NewCloudflareTransport(accountID, apiToken, baseURL string) *CloudflareTransport {
	if baseURL == "" {
		baseURL = defaultCloudflareBaseURL
	}
	return &CloudflareTransport{AccountID: accountID, APIToken: apiToken, BaseURL: baseURL}
}

func (c *CloudflareTransport) sendURL() string {
	return c.BaseURL + "/client/v4/accounts/" + c.AccountID + "/email/sending/send"
}

// cloudflareAttachment mirrors the documented attachment shape.
type cloudflareAttachment struct {
	Content     string `json:"content"`
	Filename    string `json:"filename"`
	Type        string `json:"type,omitempty"`
	Disposition string `json:"disposition,omitempty"`
}

// cloudflareRequest is the documented send payload.
//
// SECURITY: adding a field here changes what submitter-controlled data
// crosses to Cloudflare. In particular `headers` is deliberately NOT
// populated from anything a submitter can influence — it is the one
// field in this API that would reopen NFR1.
type cloudflareRequest struct {
	To          string                 `json:"to"`
	From        string                 `json:"from"`
	Subject     string                 `json:"subject"`
	HTML        string                 `json:"html,omitempty"`
	Text        string                 `json:"text,omitempty"`
	Attachments []cloudflareAttachment `json:"attachments,omitempty"`
}

// cloudflareResponse is the documented envelope. `result` carries the
// per-recipient outcome rather than a message id.
type cloudflareResponse struct {
	Success bool `json:"success"`
	Errors  []struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"errors"`
	Result *struct {
		Delivered        []string `json:"delivered"`
		PermanentBounces []string `json:"permanent_bounces"`
		Queued           []string `json:"queued"`
	} `json:"result"`
}

// Send implements Transport.
func (c *CloudflareTransport) Send(ctx context.Context, msg Message) (SendResult, error) {
	if len(msg.To) == 0 {
		return SendResult{}, &TransportError{Class: ErrTerminal, Message: "no recipients"}
	}
	// The documented schema takes a single `to`. Posthorn does not fan
	// one submission into several provider calls — that would produce
	// several provider outcomes for one message id, which is exactly
	// the fan-out that is deliberately out of scope.
	if len(msg.To) > 1 {
		return SendResult{}, &TransportError{
			Class: ErrTerminal,
			Message: fmt.Sprintf("cloudflare accepts one recipient per send, got %d — "+
				"use a provider with multi-recipient support for this endpoint", len(msg.To)),
		}
	}

	req := cloudflareRequest{
		To:      msg.To[0],
		From:    msg.From,
		Subject: msg.Subject,
		Text:    msg.BodyText,
		HTML:    msg.BodyHTML,
	}
	for _, a := range msg.Attachments {
		req.Attachments = append(req.Attachments, cloudflareAttachment{
			Content:     base64.StdEncoding.EncodeToString(a.Data),
			Filename:    a.Filename,
			Type:        a.ContentType,
			Disposition: "attachment",
		})
	}

	buf, err := json.Marshal(req)
	if err != nil {
		return SendResult{}, &TransportError{
			Class: ErrTerminal, Message: "encode cloudflare request", Cause: err,
		}
	}
	if len(buf) > cloudflareMaxMessageBytes {
		return SendResult{}, &TransportError{
			Class:   ErrTerminal,
			Message: fmt.Sprintf("message is %d bytes, over Cloudflare's 5 MiB limit", len(buf)),
		}
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.sendURL(), bytes.NewReader(buf))
	if err != nil {
		return SendResult{}, &TransportError{
			Class: ErrTerminal, Message: "build cloudflare request", Cause: err,
		}
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+c.APIToken)

	resp, err := cloudflareHTTPClient.Do(httpReq)
	if err != nil {
		return SendResult{}, &TransportError{
			Class: ErrTransient, Message: "cloudflare request failed", Cause: err,
		}
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, cloudflareResponseLimit))

	var decoded cloudflareResponse
	_ = json.Unmarshal(body, &decoded)

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300 && decoded.Success:
		// Cloudflare reports the outcome per recipient in the same
		// response. A permanent bounce here is a TERMINAL failure that
		// other providers only surface hours later by webhook —
		// treating it as success because the HTTP call returned 200
		// would record a delivery that never happened.
		if decoded.Result != nil && len(decoded.Result.PermanentBounces) > 0 {
			return SendResult{}, &TransportError{
				Class:   ErrTerminal,
				Status:  resp.StatusCode,
				Message: "cloudflare reported a permanent bounce at hand-off",
			}
		}
		if decoded.Result != nil &&
			len(decoded.Result.Delivered) == 0 && len(decoded.Result.Queued) == 0 {
			// Accepted with nothing delivered or queued: not an
			// outcome we can call success.
			return SendResult{}, &TransportError{
				Class: ErrTransient, Status: resp.StatusCode,
				Message: "cloudflare accepted the request but reported no delivered or queued recipient",
			}
		}
		// No message id exists to return — see the type comment.
		return SendResult{}, nil

	case resp.StatusCode == http.StatusTooManyRequests:
		return SendResult{}, &TransportError{
			Class: ErrRateLimited, Status: resp.StatusCode,
			Message: "cloudflare rate limited",
		}

	case resp.StatusCode >= 500:
		return SendResult{}, &TransportError{
			Class: ErrTransient, Status: resp.StatusCode,
			Message: "cloudflare server error",
		}

	default:
		return SendResult{}, &TransportError{
			Class: ErrTerminal, Status: resp.StatusCode,
			Message: "cloudflare rejected the message" + cloudflareErrorCodes(decoded),
		}
	}
}

// cloudflareErrorCodes renders the provider's numeric error codes.
//
// Codes only, never the messages: Cloudflare's error text can echo the
// submitted payload, and a provider error is logged (NFR3).
func cloudflareErrorCodes(r cloudflareResponse) string {
	if len(r.Errors) == 0 {
		return ""
	}
	codes := make([]string, 0, len(r.Errors))
	for _, e := range r.Errors {
		codes = append(codes, fmt.Sprintf("%d", e.Code))
	}
	return " (codes " + strings.Join(codes, ",") + ")"
}

func init() {
	Register(Registration{
		Type: "cloudflare",
		Validate: func(settings map[string]any) error {
			account, _ := settings["account_id"].(string)
			if account == "" {
				return fmt.Errorf("cloudflare transport requires settings.account_id")
			}
			token, _ := settings["api_token"].(string)
			if token == "" {
				return fmt.Errorf("cloudflare transport requires settings.api_token")
			}
			return nil
		},
		Build: func(settings map[string]any) (Transport, error) {
			account, _ := settings["account_id"].(string)
			token, _ := settings["api_token"].(string)
			baseURL, _ := settings["base_url"].(string)
			return NewCloudflareTransport(account, token, baseURL), nil
		},
	})
}
