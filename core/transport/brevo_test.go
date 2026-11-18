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

func goodBrevoMessage() Message {
	return Message{
		From:     "noreply@example.com",
		To:       []string{"craig@example.com"},
		Subject:  "Hello",
		BodyText: "Body text.",
	}
}

func TestBrevo_Success_201(t *testing.T) {
	cs := newCaptureServer(t, http.StatusCreated, `{"messageId":"<202609081200.1234@smtp-relay.mailin.fr>"}`)
	tp := NewBrevoTransport("test-key", cs.URL)

	result, err := tp.Send(context.Background(), goodBrevoMessage())
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if cs.hits != 1 {
		t.Errorf("hits = %d, want 1", cs.hits)
	}
	if result.MessageID != "<202609081200.1234@smtp-relay.mailin.fr>" {
		t.Errorf("MessageID = %q", result.MessageID)
	}
}

// TestBrevo_Success_OtherAccepted covers the 200/202 variants Brevo may
// answer with; the message is accepted either way.
func TestBrevo_Success_OtherAccepted(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusAccepted} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			cs := newCaptureServer(t, status, `{"messageId":"m-1"}`)
			tp := NewBrevoTransport("k", cs.URL)
			result, err := tp.Send(context.Background(), goodBrevoMessage())
			if err != nil {
				t.Fatalf("Send: %v", err)
			}
			if result.MessageID != "m-1" {
				t.Errorf("MessageID = %q, want m-1", result.MessageID)
			}
		})
	}
}

// TestBrevo_Success_NoMessageID: a missing or unparseable id only
// degrades correlation — Brevo already accepted the mail.
func TestBrevo_Success_NoMessageID(t *testing.T) {
	for _, body := range []string{`{}`, `{"other":"x"}`, `not-json`} {
		cs := newCaptureServer(t, http.StatusCreated, body)
		tp := NewBrevoTransport("k", cs.URL)
		result, err := tp.Send(context.Background(), goodBrevoMessage())
		if err != nil {
			t.Fatalf("Send(%s): %v", body, err)
		}
		if result.MessageID != "" {
			t.Errorf("MessageID = %q, want empty", result.MessageID)
		}
	}
}

func TestBrevo_RequestShape(t *testing.T) {
	cs := newCaptureServer(t, http.StatusCreated, `{"messageId":"m"}`)
	tp := NewBrevoTransport("xkeysib-token-abc", cs.URL)

	msg := Message{
		From:     "Posthorn <a@example.com>",
		To:       []string{"b@example.com", "Cee <c@example.com>"},
		ReplyTo:  "reply@example.com",
		Subject:  "Subj",
		BodyText: "Hello",
	}
	if _, err := tp.Send(context.Background(), msg); err != nil {
		t.Fatalf("Send: %v", err)
	}

	if cs.method != http.MethodPost {
		t.Errorf("method = %q, want POST", cs.method)
	}
	if cs.path != brevoSendPath {
		t.Errorf("path = %q, want %q", cs.path, brevoSendPath)
	}
	// Brevo authenticates with its own header, not Authorization: Bearer.
	if got := cs.headers.Get("api-key"); got != "xkeysib-token-abc" {
		t.Errorf("api-key = %q", got)
	}
	if got := cs.headers.Get("Authorization"); got != "" {
		t.Errorf("Authorization = %q, want unset (Brevo uses api-key)", got)
	}

	var body brevoRequest
	if err := json.Unmarshal(cs.body, &body); err != nil {
		t.Fatalf("body is not JSON: %v\n%s", err, cs.body)
	}
	if body.Sender.Email != "a@example.com" || body.Sender.Name != "Posthorn" {
		t.Errorf("sender = %+v, want {a@example.com Posthorn}", body.Sender)
	}
	if len(body.To) != 2 {
		t.Fatalf("to = %+v, want 2 recipients", body.To)
	}
	if body.To[0].Email != "b@example.com" || body.To[0].Name != "" {
		t.Errorf("to[0] = %+v", body.To[0])
	}
	if body.To[1].Email != "c@example.com" || body.To[1].Name != "Cee" {
		t.Errorf("to[1] = %+v", body.To[1])
	}
	if body.ReplyTo == nil || body.ReplyTo.Email != "reply@example.com" {
		t.Errorf("replyTo = %+v", body.ReplyTo)
	}
	if body.Subject != "Subj" || body.TextContent != "Hello" {
		t.Errorf("subject/text = %q/%q", body.Subject, body.TextContent)
	}
}

func TestBrevo_OmitsEmptyReplyTo(t *testing.T) {
	cs := newCaptureServer(t, http.StatusCreated, `{}`)
	tp := NewBrevoTransport("k", cs.URL)

	msg := goodBrevoMessage()
	msg.ReplyTo = ""
	if _, err := tp.Send(context.Background(), msg); err != nil {
		t.Fatalf("Send: %v", err)
	}
	var body map[string]any
	_ = json.Unmarshal(cs.body, &body)
	if _, present := body["replyTo"]; present {
		t.Errorf("replyTo present when message field empty: %s", cs.body)
	}
}

// TestBrevo_TextOnlyGetsEscapedHTML documents the one place Posthorn
// synthesizes markup: Brevo rejects a send carrying neither htmlContent
// nor a templateId. The synthesized part must be ESCAPED submitter text,
// never live markup (ADR-19's guarantee, applied here).
func TestBrevo_TextOnlyGetsEscapedHTML(t *testing.T) {
	cs := newCaptureServer(t, http.StatusCreated, `{}`)
	tp := NewBrevoTransport("k", cs.URL)

	msg := goodBrevoMessage()
	msg.BodyText = "Hi <script>alert(1)</script>\nsecond line"
	if _, err := tp.Send(context.Background(), msg); err != nil {
		t.Fatalf("Send: %v", err)
	}
	var body brevoRequest
	_ = json.Unmarshal(cs.body, &body)
	if body.TextContent != msg.BodyText {
		t.Errorf("textContent = %q, want verbatim submitter text", body.TextContent)
	}
	if strings.Contains(body.HTMLContent, "<script>") {
		t.Errorf("htmlContent carries live markup: %q", body.HTMLContent)
	}
	if !strings.Contains(body.HTMLContent, "&lt;script&gt;") {
		t.Errorf("htmlContent = %q, want the script tag escaped", body.HTMLContent)
	}
	if !strings.Contains(body.HTMLContent, "<br>") {
		t.Errorf("htmlContent = %q, want the newline rendered as <br>", body.HTMLContent)
	}
}

// TestBrevo_HTMLBodyPassesThrough: when the endpoint is configured for
// HTML, the already-rendered (ADR-19 auto-escaped) body is what ships.
func TestBrevo_HTMLBodyPassesThrough(t *testing.T) {
	cs := newCaptureServer(t, http.StatusCreated, `{}`)
	tp := NewBrevoTransport("k", cs.URL)

	msg := goodBrevoMessage()
	msg.BodyHTML = "<h1>Rendered</h1>"
	msg.BodyText = "Rendered"
	if _, err := tp.Send(context.Background(), msg); err != nil {
		t.Fatalf("Send: %v", err)
	}
	var body brevoRequest
	_ = json.Unmarshal(cs.body, &body)
	if body.HTMLContent != "<h1>Rendered</h1>" {
		t.Errorf("htmlContent = %q", body.HTMLContent)
	}
	if body.TextContent != "Rendered" {
		t.Errorf("textContent = %q", body.TextContent)
	}
}

func TestBrevo_429_RateLimited(t *testing.T) {
	cs := newCaptureServer(t, http.StatusTooManyRequests, `{"code":"too_many_requests","message":"slow down"}`)
	tp := NewBrevoTransport("k", cs.URL)

	_, err := tp.Send(context.Background(), goodBrevoMessage())
	var te *TransportError
	if !errors.As(err, &te) {
		t.Fatalf("not a *TransportError: %v", err)
	}
	if te.Class != ErrRateLimited {
		t.Errorf("Class = %v, want ErrRateLimited", te.Class)
	}
}

func TestBrevo_5xx_Transient(t *testing.T) {
	for _, status := range []int{500, 502, 503} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			cs := newCaptureServer(t, status, `{}`)
			tp := NewBrevoTransport("k", cs.URL)
			_, err := tp.Send(context.Background(), goodBrevoMessage())
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

func TestBrevo_4xx_Terminal(t *testing.T) {
	for _, status := range []int{400, 401, 403, 404} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			cs := newCaptureServer(t, status, `{"code":"invalid_parameter","message":"bad"}`)
			tp := NewBrevoTransport("k", cs.URL)
			_, err := tp.Send(context.Background(), goodBrevoMessage())
			var te *TransportError
			if !errors.As(err, &te) {
				t.Fatalf("not a *TransportError: %v", err)
			}
			if te.Class != ErrTerminal {
				t.Errorf("Class = %v, want ErrTerminal", te.Class)
			}
			if !strings.Contains(te.Message, "invalid_parameter") {
				t.Errorf("Message = %q, want the provider error code", te.Message)
			}
		})
	}
}

// TestBrevo_ErrorMessageNotEchoed: the provider's error CODE is safe to
// surface; its human message can quote the submitted payload, and a
// transport error is logged.
func TestBrevo_ErrorMessageNotEchoed(t *testing.T) {
	cs := newCaptureServer(t, http.StatusBadRequest,
		`{"code":"invalid_parameter","message":"subject contains secret-token-xyz"}`)
	tp := NewBrevoTransport("k", cs.URL)

	_, err := tp.Send(context.Background(), goodBrevoMessage())
	if err == nil {
		t.Fatal("want error")
	}
	if strings.Contains(err.Error(), "secret-token-xyz") {
		t.Errorf("provider message echoed into the error: %q", err.Error())
	}
}

func TestBrevo_NetworkError_Transient(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	url := srv.URL
	srv.Close()

	tp := NewBrevoTransport("k", url)
	_, err := tp.Send(context.Background(), goodBrevoMessage())
	var te *TransportError
	if !errors.As(err, &te) {
		t.Fatalf("not a *TransportError: %v", err)
	}
	if te.Class != ErrTransient {
		t.Errorf("Class = %v, want ErrTransient", te.Class)
	}
}

// TestBrevo_NoAPIKeyInError is the NFR3 acceptance test.
func TestBrevo_NoAPIKeyInError(t *testing.T) {
	const sentinel = "xkeysib-SENTINEL-must-not-leak"
	cs := newCaptureServer(t, http.StatusUnauthorized, `{"code":"unauthorized","message":"Key not found"}`)
	tp := NewBrevoTransport(sentinel, cs.URL)

	_, err := tp.Send(context.Background(), goodBrevoMessage())
	if err == nil {
		t.Fatal("want error")
	}
	if strings.Contains(err.Error(), sentinel) {
		t.Errorf("API key leaked into error: %q", err.Error())
	}
}

// TestBrevo_MalformedAddress_Terminal: a From or To that net/mail cannot
// parse is the message's problem, not the provider's — retrying or
// failing over to another provider would fail identically.
func TestBrevo_MalformedAddress_Terminal(t *testing.T) {
	cs := newCaptureServer(t, http.StatusCreated, `{}`)
	for _, tt := range []struct {
		name string
		msg  Message
	}{
		{"from", Message{From: "not an address", To: []string{"r@example.com"}}},
		{"to", Message{From: "f@example.com", To: []string{"not an address"}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			tp := NewBrevoTransport("k", cs.URL)
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

// TestBrevo_NoHeaderInjection is the NFR1/NFR2 acceptance test.
//
// Brevo takes structured {email, name} addresses, so the boundary has
// two halves: address fields are parsed by net/mail and a CRLF-bearing
// address is REJECTED outright; free-text fields cross as JSON string
// values where CRLF cannot synthesize a sibling key or an HTTP header.
func TestBrevo_NoHeaderInjection(t *testing.T) {
	t.Run("crlf_addresses_rejected", func(t *testing.T) {
		cs := newCaptureServer(t, http.StatusCreated, `{}`)
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
				tp := NewBrevoTransport("k", cs.URL)
				_, err := tp.Send(context.Background(), tt.msg)
				var te *TransportError
				if !errors.As(err, &te) || te.Class != ErrTerminal {
					t.Fatalf("CRLF address accepted or misclassified: %v", err)
				}
			})
		}
	})

	allowedJSONKeys := map[string]bool{
		"sender": true, "to": true, "replyTo": true,
		"subject": true, "textContent": true, "htmlContent": true,
		"attachment": true,
	}
	expectedHTTPHeaders := []string{"Content-Type", "Accept", "Api-Key"}

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
			injectField: "textContent",
			payload:     "Jane\r\nX-Spoof: yes",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cs := newCaptureServer(t, http.StatusCreated, `{}`)
			tp := NewBrevoTransport("k", cs.URL)
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

func TestBrevo_Registration(t *testing.T) {
	reg, ok := Lookup("brevo")
	if !ok {
		t.Fatal("brevo not registered")
	}
	if err := reg.Validate(map[string]any{}); err == nil {
		t.Error("empty settings accepted; api_key must be required")
	}
	if err := reg.Validate(map[string]any{"api_key": "k"}); err != nil {
		t.Errorf("valid settings rejected: %v", err)
	}
	tp, err := reg.Build(map[string]any{"api_key": "k"})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	bt, ok := tp.(*BrevoTransport)
	if !ok {
		t.Fatalf("Build returned %T, want *BrevoTransport", tp)
	}
	if bt.BaseURL != defaultBrevoBaseURL {
		t.Errorf("BaseURL = %q, want the production default", bt.BaseURL)
	}
}
