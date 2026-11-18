// Modified for the cryotx/posthorn distribution.

package transport

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/textproto"
	"net/url"
	"sync/atomic"

	"github.com/craigmccaskill/posthorn/netguard"
	"time"
)

// Webhook transport (FR88, v2.0): the sixth transport delivers a
// submission as a signed JSON POST instead of an email — the
// integration seam generalized to a non-mail sink (Slack bridges,
// ticketing systems, analytics pipelines).
//
// This is the transport ADR-24 widened the Message boundary for: the
// payload carries the raw submitted Fields and the SubmissionID, which
// rendered bodies alone cannot provide.
//
// NFR1 enforcement: every submitter-controlled value reaches the
// receiver inside json.Marshal of a typed struct — body data, never
// headers. Operator-defined static headers are validated at parse time
// and are config, not submitter input.

const (
	// WebhookSignatureHeader carries "sha256=<hex>" — the HMAC-SHA256 of
	// the exact request body under the configured secret. The same
	// scheme as lifecycle-event forwarding, so receivers implement one
	// verifier.
	WebhookSignatureHeader = "X-Posthorn-Signature"

	webhookRequestTimeout    = 5 * time.Second
	webhookResponseSizeLimit = 64 * 1024
)

var webhookHTTPClient = &http.Client{
	Timeout: webhookRequestTimeout,
}

// outboundPolicy is the SSRF policy applied to transports that call an
// operator-supplied URL — today just the webhook transport, whose whole
// purpose is to POST somewhere of the operator's choosing (NFR45).
//
// Process-wide rather than per-transport because it answers a
// deployment question ("may Posthorn call things inside this
// network?"), and Posthorn is one process serving one deployment.
// Vendor transports are deliberately NOT subject to it: their
// destination is a fixed provider API, and their base_url override
// exists so tests can point them at a local server.
var outboundPolicy atomic.Pointer[netguard.Policy]

// SetOutboundPolicy installs the deployment's policy. Called once from
// the composition root, before any transport is built.
func SetOutboundPolicy(p netguard.Policy) { outboundPolicy.Store(&p) }

// OutboundPolicy returns the installed policy. The default — nothing
// installed — is the strict one: a caller that forgot to configure a
// policy gets the safe behavior, not an open one.
func OutboundPolicy() netguard.Policy {
	if p := outboundPolicy.Load(); p != nil {
		return *p
	}
	return netguard.Policy{}
}

// WebhookTransport implements Transport against an operator-configured
// HTTP receiver.
type WebhookTransport struct {
	// URL is the receiver endpoint.
	URL string

	// Secret signs every request body. Never logged (NFR3/NFR29).
	Secret string

	// Headers are operator-defined static headers added to every
	// request (auth tokens for the receiver, routing labels). Reserved
	// headers are rejected at config-parse time.
	Headers map[string]string

	// Client overrides the HTTP client. The registry sets a
	// policy-guarded one here (NFR45); nil keeps the package default,
	// which is what direct construction in tests gets.
	Client *http.Client
}

// NewWebhookTransport constructs a transport.
func NewWebhookTransport(rawURL, secret string, headers map[string]string) *WebhookTransport {
	return &WebhookTransport{URL: rawURL, Secret: secret, Headers: headers}
}

func (t *WebhookTransport) client() *http.Client {
	if t.Client != nil {
		return t.Client
	}
	return webhookHTTPClient
}

// webhookPayload is the JSON body POSTed to the receiver (FR88).
//
// SECURITY: adding a field here is a security-relevant change — the set
// of fields defines what submitter-controlled data leaves Posthorn.
type webhookPayload struct {
	SubmissionID string              `json:"submission_id"`
	From         string              `json:"from"`
	To           []string            `json:"to"`
	ReplyTo      string              `json:"reply_to,omitempty"`
	Subject      string              `json:"subject"`
	BodyText     string              `json:"body_text"`
	BodyHTML     string              `json:"body_html,omitempty"`
	Fields       map[string][]string `json:"fields,omitempty"`
}

// Send implements Transport.
//
// Status code mapping (FR19-22 parallel to the mail transports):
//
//	2xx        → success
//	429        → ErrRateLimited
//	5xx        → ErrTransient
//	4xx (other)→ ErrTerminal
//	network/timeout/ctx → ErrTransient (caller will retry once)
func (t *WebhookTransport) Send(ctx context.Context, msg Message) (SendResult, error) {
	payload := webhookPayload{
		SubmissionID: msg.SubmissionID,
		From:         msg.From,
		To:           msg.To,
		ReplyTo:      msg.ReplyTo,
		Subject:      msg.Subject,
		BodyText:     msg.BodyText,
		BodyHTML:     msg.BodyHTML,
		Fields:       msg.Fields,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return SendResult{}, &TransportError{Class: ErrTerminal, Cause: err, Message: "encode webhook payload"}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.URL, bytes.NewReader(body))
	if err != nil {
		return SendResult{}, &TransportError{Class: ErrTerminal, Cause: err, Message: "build webhook request"}
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range t.Headers {
		req.Header.Set(k, v)
	}
	// Signature last so a stray custom header can never displace it.
	mac := hmac.New(sha256.New, []byte(t.Secret))
	mac.Write(body)
	req.Header.Set(WebhookSignatureHeader, "sha256="+hex.EncodeToString(mac.Sum(nil)))

	resp, err := t.client().Do(req)
	if err != nil {
		return SendResult{}, &TransportError{
			Class:   ErrTransient,
			Cause:   err,
			Message: "webhook request failed",
		}
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, webhookResponseSizeLimit))

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		// Receivers have no message-ID convention; the submission ID
		// already identifies the delivery.
		return SendResult{}, nil
	case resp.StatusCode == http.StatusTooManyRequests:
		return SendResult{}, &TransportError{
			Class:   ErrRateLimited,
			Status:  resp.StatusCode,
			Message: "webhook receiver rate limit",
		}
	case resp.StatusCode >= 500:
		return SendResult{}, &TransportError{
			Class:   ErrTransient,
			Status:  resp.StatusCode,
			Message: "webhook receiver server error",
		}
	default:
		return SendResult{}, &TransportError{
			Class:   ErrTerminal,
			Status:  resp.StatusCode,
			Message: "webhook receiver rejected request",
		}
	}
}

var _ Transport = (*WebhookTransport)(nil)

// reservedWebhookHeaders cannot be overridden by operator config: the
// signature is the integrity guarantee and the content type is the
// payload contract.
var reservedWebhookHeaders = map[string]bool{
	textproto.CanonicalMIMEHeaderKey(WebhookSignatureHeader): true,
	"Content-Type": true,
}

// Registry registration.
func init() {
	Register(Registration{
		Type:     "webhook",
		Validate: validateWebhookSettings,
		Build:    buildWebhookFromSettings,
	})
}

func validateWebhookSettings(settings map[string]any) error {
	rawURL, ok := settings["url"].(string)
	if !ok || rawURL == "" {
		return fmt.Errorf("webhook transport requires settings.url")
	}
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("webhook transport url must be an absolute http(s) URL, got %q", rawURL)
	}
	secret, ok := settings["secret"].(string)
	if !ok || len(secret) < 16 {
		return fmt.Errorf("webhook transport requires settings.secret of at least 16 bytes")
	}
	if raw, present := settings["headers"]; present {
		headers, ok := raw.(map[string]any)
		if !ok {
			return fmt.Errorf("webhook transport settings.headers must be a table of string values")
		}
		for k, v := range headers {
			if _, ok := v.(string); !ok {
				return fmt.Errorf("webhook transport settings.headers[%q] must be a string", k)
			}
			if reservedWebhookHeaders[textproto.CanonicalMIMEHeaderKey(k)] {
				return fmt.Errorf("webhook transport settings.headers[%q] is reserved", k)
			}
		}
	}
	return nil
}

func buildWebhookFromSettings(settings map[string]any) (Transport, error) {
	if err := validateWebhookSettings(settings); err != nil {
		return nil, err
	}
	rawURL := settings["url"].(string)
	secret := settings["secret"].(string)
	var headers map[string]string
	if raw, present := settings["headers"]; present {
		headers = map[string]string{}
		for k, v := range raw.(map[string]any) {
			headers[k] = v.(string)
		}
	}
	wt := NewWebhookTransport(rawURL, secret, headers)
	// Built from config, so this is a real deployment destination: it
	// gets the SSRF-guarded client, which re-checks every connection
	// (and every redirect hop) against the resolved address.
	wt.Client = OutboundPolicy().Client(webhookRequestTimeout)
	return wt, nil
}
