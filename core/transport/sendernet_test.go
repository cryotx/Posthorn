package transport

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func goodSenderNetMessage() Message {
	return Message{
		From:     "noreply@example.com",
		To:       []string{"craig@example.com"},
		Subject:  "Hello",
		BodyText: "Body text.",
	}
}

func TestSenderNet_Success_200(t *testing.T) {
	cs := newCaptureServer(t, http.StatusOK, `{"success":true,"message":"Email sent","emailId":"ep2W4y-7pn8o21-YPpLY9PR5Jy9-x7GYQ"}`)
	tp := NewSenderNetTransport("test-token", cs.URL)

	result, err := tp.Send(context.Background(), goodSenderNetMessage())
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if cs.hits != 1 {
		t.Errorf("hits = %d, want 1", cs.hits)
	}
	if result.MessageID != "ep2W4y-7pn8o21-YPpLY9PR5Jy9-x7GYQ" {
		t.Errorf("MessageID = %q", result.MessageID)
	}
}

// TestSenderNet_Success_NoEmailID: a missing or unparseable id only
// degrades correlation — the message was already accepted.
func TestSenderNet_Success_NoEmailID(t *testing.T) {
	for _, body := range []string{`{"success":true}`, `{}`, `not-json`} {
		cs := newCaptureServer(t, http.StatusOK, body)
		tp := NewSenderNetTransport("test-token", cs.URL)
		result, err := tp.Send(context.Background(), goodSenderNetMessage())
		if err != nil {
			t.Fatalf("Send(%s): %v", body, err)
		}
		if result.MessageID != "" {
			t.Errorf("MessageID = %q, want empty", result.MessageID)
		}
	}
}

func TestSenderNet_RequestShape(t *testing.T) {
	cs := newCaptureServer(t, http.StatusOK, `{"success":true,"emailId":"m"}`)
	tp := NewSenderNetTransport("sender-token-abc", cs.URL)

	msg := Message{
		From:     "Posthorn <a@example.com>",
		To:       []string{"Cee <c@example.com>"},
		Subject:  "Subj",
		BodyText: "Hello",
	}
	if _, err := tp.Send(context.Background(), msg); err != nil {
		t.Fatalf("Send: %v", err)
	}

	if cs.method != http.MethodPost {
		t.Errorf("method = %q, want POST", cs.method)
	}
	if cs.path != senderNetSendPath {
		t.Errorf("path = %q, want %q", cs.path, senderNetSendPath)
	}
	if got := cs.headers.Get("Authorization"); got != "Bearer sender-token-abc" {
		t.Errorf("Authorization = %q", got)
	}

	var body senderNetRequest
	if err := json.Unmarshal(cs.body, &body); err != nil {
		t.Fatalf("body is not JSON: %v\n%s", err, cs.body)
	}
	if body.From.Email != "a@example.com" || body.From.Name != "Posthorn" {
		t.Errorf("from = %+v", body.From)
	}
	if body.To.Email != "c@example.com" || body.To.Name != "Cee" {
		t.Errorf("to = %+v", body.To)
	}
	if body.Subject != "Subj" || body.Text != "Hello" {
		t.Errorf("subject/text = %q/%q", body.Subject, body.Text)
	}
}

// TestSenderNet_TextOnlyGetsEscapedHTML: when the endpoint carries no
// HTML body, the synthesized part must be ESCAPED submitter text, never
// live markup (ADR-19's guarantee, applied here).
func TestSenderNet_TextOnlyGetsEscapedHTML(t *testing.T) {
	cs := newCaptureServer(t, http.StatusOK, `{"success":true}`)
	tp := NewSenderNetTransport("k", cs.URL)

	msg := goodSenderNetMessage()
	msg.BodyText = "Hi <script>alert(1)</script>\nsecond line"
	if _, err := tp.Send(context.Background(), msg); err != nil {
		t.Fatalf("Send: %v", err)
	}
	var body senderNetRequest
	_ = json.Unmarshal(cs.body, &body)
	if body.Text != msg.BodyText {
		t.Errorf("text = %q, want verbatim submitter text", body.Text)
	}
	if strings.Contains(body.HTML, "<script>") {
		t.Errorf("html carries live markup: %q", body.HTML)
	}
	if !strings.Contains(body.HTML, "&lt;script&gt;") {
		t.Errorf("html = %q, want the script tag escaped", body.HTML)
	}
}

func TestSenderNet_HTMLBodyPassesThrough(t *testing.T) {
	cs := newCaptureServer(t, http.StatusOK, `{"success":true}`)
	tp := NewSenderNetTransport("k", cs.URL)

	msg := goodSenderNetMessage()
	msg.BodyHTML = "<h1>Rendered</h1>"
	msg.BodyText = "Rendered"
	if _, err := tp.Send(context.Background(), msg); err != nil {
		t.Fatalf("Send: %v", err)
	}
	var body senderNetRequest
	_ = json.Unmarshal(cs.body, &body)
	if body.HTML != "<h1>Rendered</h1>" {
		t.Errorf("html = %q", body.HTML)
	}
}

// TestSenderNet_MultipleRecipients_Terminal: the documented schema
// takes a single `to` object. Fanning one submission into N provider
// calls would produce N outcomes for one message id — deliberately out
// of scope — so the adapter refuses rather than silently sending to the
// first recipient only.
func TestSenderNet_MultipleRecipients_Terminal(t *testing.T) {
	cs := newCaptureServer(t, http.StatusOK, `{"success":true}`)
	tp := NewSenderNetTransport("k", cs.URL)

	msg := goodSenderNetMessage()
	msg.To = []string{"a@example.com", "b@example.com"}
	_, err := tp.Send(context.Background(), msg)
	var te *TransportError
	if !errors.As(err, &te) {
		t.Fatalf("not a *TransportError: %v", err)
	}
	if te.Class != ErrTerminal {
		t.Errorf("Class = %v, want ErrTerminal", te.Class)
	}
	if cs.hits != 0 {
		t.Errorf("hits = %d; the request must not reach sender.net at all", cs.hits)
	}
}

func TestSenderNet_NoRecipients_Terminal(t *testing.T) {
	tp := NewSenderNetTransport("k", "http://127.0.0.1:1")
	msg := goodSenderNetMessage()
	msg.To = nil
	_, err := tp.Send(context.Background(), msg)
	var te *TransportError
	if !errors.As(err, &te) || te.Class != ErrTerminal {
		t.Fatalf("want terminal error, got %v", err)
	}
}

func TestSenderNet_401_Terminal(t *testing.T) {
	cs := newCaptureServer(t, http.StatusUnauthorized, `{"message":"Unauthenticated"}`)
	tp := NewSenderNetTransport("wrong-token", cs.URL)

	_, err := tp.Send(context.Background(), goodSenderNetMessage())
	var te *TransportError
	if !errors.As(err, &te) {
		t.Fatalf("not a *TransportError: %v", err)
	}
	if te.Class != ErrTerminal {
		t.Errorf("Class = %v, want ErrTerminal", te.Class)
	}
	if IsRetryable(err) || IsDeferrable(err) {
		t.Error("a bad credential must not be retried or queued")
	}
}

func TestSenderNet_429_RateLimited(t *testing.T) {
	cs := newCaptureServer(t, http.StatusTooManyRequests, `{}`)
	tp := NewSenderNetTransport("k", cs.URL)

	_, err := tp.Send(context.Background(), goodSenderNetMessage())
	var te *TransportError
	if !errors.As(err, &te) {
		t.Fatalf("not a *TransportError: %v", err)
	}
	if te.Class != ErrRateLimited {
		t.Errorf("Class = %v, want ErrRateLimited", te.Class)
	}
}

func TestSenderNet_422_Terminal(t *testing.T) {
	cs := newCaptureServer(t, http.StatusUnprocessableEntity,
		`{"message":"The given data was invalid.","errors":{"to.email":["Required value, email"]}}`)
	tp := NewSenderNetTransport("k", cs.URL)

	_, err := tp.Send(context.Background(), goodSenderNetMessage())
	var te *TransportError
	if !errors.As(err, &te) {
		t.Fatalf("not a *TransportError: %v", err)
	}
	if te.Class != ErrTerminal {
		t.Errorf("Class = %v, want ErrTerminal", te.Class)
	}
	if !strings.Contains(te.Message, "to.email") {
		t.Errorf("Message = %q, want it to name the invalid field", te.Message)
	}
}

// TestSenderNet_ValidationMessageNotEchoed: the documented 422 message
// text is Sender.net's own validation prose and could echo submitted
// content back. Only field NAMES are safe to surface.
func TestSenderNet_ValidationMessageNotEchoed(t *testing.T) {
	cs := newCaptureServer(t, http.StatusUnprocessableEntity,
		`{"message":"subject contains secret-token-xyz","errors":{"subject":["subject contains secret-token-xyz"]}}`)
	tp := NewSenderNetTransport("k", cs.URL)

	_, err := tp.Send(context.Background(), goodSenderNetMessage())
	if err == nil {
		t.Fatal("want error")
	}
	if strings.Contains(err.Error(), "secret-token-xyz") {
		t.Errorf("provider message echoed into the error: %q", err.Error())
	}
}

func TestSenderNet_5xx_Transient(t *testing.T) {
	for _, status := range []int{500, 502, 503} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			cs := newCaptureServer(t, status, `{}`)
			tp := NewSenderNetTransport("k", cs.URL)
			_, err := tp.Send(context.Background(), goodSenderNetMessage())
			var te *TransportError
			if !errors.As(err, &te) {
				t.Fatalf("not a *TransportError: %v", err)
			}
			if te.Class != ErrTransient {
				t.Errorf("Class = %v, want ErrTransient", te.Class)
			}
			if te.Status != status {
				t.Errorf("Status = %d, want %d", te.Status, status)
			}
		})
	}
}

func TestSenderNet_NetworkError_Transient(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	url := srv.URL
	srv.Close()

	tp := NewSenderNetTransport("k", url)
	_, err := tp.Send(context.Background(), goodSenderNetMessage())
	var te *TransportError
	if !errors.As(err, &te) {
		t.Fatalf("not a *TransportError: %v", err)
	}
	if te.Class != ErrTransient {
		t.Errorf("Class = %v, want ErrTransient", te.Class)
	}
}

// TestSenderNet_NoTokenInError is the NFR3 acceptance test.
func TestSenderNet_NoTokenInError(t *testing.T) {
	const sentinel = "SENTINEL-token-must-not-leak-abcdef"
	cs := newCaptureServer(t, http.StatusUnauthorized, `{"message":"Unauthenticated"}`)
	tp := NewSenderNetTransport(sentinel, cs.URL)

	_, err := tp.Send(context.Background(), goodSenderNetMessage())
	if err == nil {
		t.Fatal("want error")
	}
	if strings.Contains(err.Error(), sentinel) {
		t.Errorf("API token leaked into error: %q", err.Error())
	}
}

// TestSenderNet_MalformedAddress_Terminal: a From or To that net/mail
// cannot parse is the message's problem, not the provider's.
func TestSenderNet_MalformedAddress_Terminal(t *testing.T) {
	cs := newCaptureServer(t, http.StatusOK, `{"success":true}`)
	for _, tt := range []struct {
		name string
		msg  Message
	}{
		{"from", Message{From: "not an address", To: []string{"r@example.com"}}},
		{"to", Message{From: "f@example.com", To: []string{"not an address"}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			tp := NewSenderNetTransport("k", cs.URL)
			_, err := tp.Send(context.Background(), tt.msg)
			var te *TransportError
			if !errors.As(err, &te) {
				t.Fatalf("not a *TransportError: %v", err)
			}
			if te.Class != ErrTerminal {
				t.Errorf("Class = %v, want ErrTerminal", te.Class)
			}
		})
	}
}

// TestSenderNet_NoHeaderInjection is the NFR1/NFR2 acceptance test.
//
// Sender.net takes structured {email, name} addresses, so the boundary
// has two halves: address fields are parsed by net/mail and a
// CRLF-bearing address is REJECTED outright; free-text fields cross as
// JSON string values where CRLF cannot synthesize a sibling key or an
// HTTP header.
func TestSenderNet_NoHeaderInjection(t *testing.T) {
	t.Run("crlf_addresses_rejected", func(t *testing.T) {
		cs := newCaptureServer(t, http.StatusOK, `{"success":true}`)
		for _, tt := range []struct {
			name string
			msg  Message
		}{
			{"from", Message{
				From: "attacker@evil.com\r\nBcc: target@victim.com",
				To:   []string{"r@example.com"}, Subject: "s", BodyText: "b",
			}},
			{"to", Message{
				From: "f@example.com",
				To:   []string{"r@example.com\r\nBcc: target@victim.com"}, Subject: "s", BodyText: "b",
			}},
		} {
			t.Run(tt.name, func(t *testing.T) {
				tp := NewSenderNetTransport("k", cs.URL)
				_, err := tp.Send(context.Background(), tt.msg)
				var te *TransportError
				if !errors.As(err, &te) || te.Class != ErrTerminal {
					t.Fatalf("CRLF address accepted or misclassified: %v", err)
				}
			})
		}
	})

	allowedJSONKeys := map[string]bool{
		"from": true, "to": true, "subject": true, "html": true, "text": true,
	}
	expectedHTTPHeaders := []string{"Content-Type", "Accept", "Authorization"}

	for _, tt := range []struct {
		name        string
		msg         Message
		injectField string
		payload     string
	}{
		{
			name: "crlf_bcc_in_subject",
			msg: Message{From: "f@example.com", To: []string{"r@example.com"},
				Subject: "Hello\r\nBcc: target@victim.com", BodyText: "b"},
			injectField: "subject",
			payload:     "Hello\r\nBcc: target@victim.com",
		},
		{
			name: "lf_only_in_subject",
			msg: Message{From: "f@example.com", To: []string{"r@example.com"},
				Subject: "Hello\nBcc: target@victim.com", BodyText: "b"},
			injectField: "subject",
			payload:     "Hello\nBcc: target@victim.com",
		},
		{
			name: "smuggled_xspoof_in_bodytext",
			msg: Message{From: "f@example.com", To: []string{"r@example.com"},
				Subject: "s", BodyText: "Jane\r\nX-Spoof: yes"},
			injectField: "text",
			payload:     "Jane\r\nX-Spoof: yes",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cs := newCaptureServer(t, http.StatusOK, `{"success":true}`)
			tp := NewSenderNetTransport("k", cs.URL)
			if _, err := tp.Send(context.Background(), tt.msg); err != nil {
				t.Fatalf("Send: %v", err)
			}

			var body map[string]any
			if err := json.Unmarshal(cs.body, &body); err != nil {
				t.Fatalf("body not valid JSON: %v\n%s", err, cs.body)
			}
			for key := range body {
				if !allowedJSONKeys[key] {
					t.Errorf("smuggled JSON key %q in request body: %s", key, cs.body)
				}
			}
			got, ok := body[tt.injectField].(string)
			if !ok {
				t.Fatalf("field %q missing or not a string: %v", tt.injectField, body[tt.injectField])
			}
			if got != tt.payload {
				t.Errorf("%s = %q, want %q (verbatim, unsanitized)", tt.injectField, got, tt.payload)
			}

			for headerName := range cs.headers {
				switch headerName {
				case "Accept-Encoding", "User-Agent", "Content-Length", "Host":
					continue
				}
				ok := false
				for _, expected := range expectedHTTPHeaders {
					if headerName == expected {
						ok = true
						break
					}
				}
				if !ok {
					t.Errorf("unexpected outbound HTTP header %q (value %q)",
						headerName, cs.headers.Get(headerName))
				}
			}
		})
	}
}

func TestSenderNet_Registration(t *testing.T) {
	reg, ok := Lookup("sendernet")
	if !ok {
		t.Fatal("sendernet not registered")
	}
	if err := reg.Validate(map[string]any{}); err == nil {
		t.Error("empty settings accepted; api_token must be required")
	}
	if err := reg.Validate(map[string]any{"api_token": "t"}); err != nil {
		t.Errorf("valid settings rejected: %v", err)
	}
	tp, err := reg.Build(map[string]any{"api_token": "t"})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	sn, ok := tp.(*SenderNetTransport)
	if !ok {
		t.Fatalf("Build returned %T, want *SenderNetTransport", tp)
	}
	if sn.BaseURL != defaultSenderNetBaseURL {
		t.Errorf("BaseURL = %q, want the production default", sn.BaseURL)
	}
}

// TestSenderNet_404_SurfacesTheAccountStateMessage: an unrecognised
// status still gets the provider's own "message" field — unlike the
// 422 per-field detail, this one is consistently a fixed account-state
// string (e.g. "Sending domain not found") and never echoes submitted
// content, so it is safe and genuinely useful to surface.
func TestSenderNet_404_SurfacesTheAccountStateMessage(t *testing.T) {
	cs := newCaptureServer(t, http.StatusNotFound, `{"message":"Sending domain not found","success":false}`)
	tp := NewSenderNetTransport("k", cs.URL)

	_, err := tp.Send(context.Background(), goodSenderNetMessage())
	var te *TransportError
	if !errors.As(err, &te) {
		t.Fatalf("not a *TransportError: %v", err)
	}
	if te.Class != ErrTerminal {
		t.Errorf("Class = %v, want ErrTerminal", te.Class)
	}
	if !strings.Contains(te.Message, "Sending domain not found") {
		t.Errorf("Message = %q, want it to carry the provider's classification", te.Message)
	}
}
