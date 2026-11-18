package transport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const cfAccount = "acct-123"

func goodCloudflareMessage() Message {
	return Message{
		From:     "noreply@example.com",
		To:       []string{"craig@example.com"},
		Subject:  "Hello",
		BodyText: "Body text.",
	}
}

func cfResult(delivered, bounced, queued []string) string {
	body := map[string]any{
		"success": true,
		"errors":  []any{},
		"result": map[string]any{
			"delivered":         delivered,
			"permanent_bounces": bounced,
			"queued":            queued,
		},
	}
	buf, _ := json.Marshal(body)
	return string(buf)
}

func TestCloudflare_Success_Delivered(t *testing.T) {
	cs := newCaptureServer(t, http.StatusOK, cfResult([]string{"craig@example.com"}, nil, nil))
	tp := NewCloudflareTransport(cfAccount, "token", cs.URL)

	result, err := tp.Send(context.Background(), goodCloudflareMessage())
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if cs.hits != 1 {
		t.Errorf("hits = %d, want 1", cs.hits)
	}
	// Cloudflare's REST send returns no message id. Reporting an empty
	// one is the honest answer; inventing a synthetic id would put a
	// value in the attempt trail that no callback can ever join to.
	if result.MessageID != "" {
		t.Errorf("MessageID = %q, want empty (Cloudflare returns none)", result.MessageID)
	}
}

func TestCloudflare_Success_Queued(t *testing.T) {
	cs := newCaptureServer(t, http.StatusOK, cfResult(nil, nil, []string{"craig@example.com"}))
	tp := NewCloudflareTransport(cfAccount, "token", cs.URL)

	if _, err := tp.Send(context.Background(), goodCloudflareMessage()); err != nil {
		t.Fatalf("queued recipient treated as failure: %v", err)
	}
}

// TestCloudflare_PermanentBounce_Terminal is the reason this adapter is
// native rather than generic SMTP: Cloudflare reports a permanent bounce
// synchronously, in the same response that carries HTTP 200. Recording
// that as a success would log a delivery that never happened.
func TestCloudflare_PermanentBounce_Terminal(t *testing.T) {
	cs := newCaptureServer(t, http.StatusOK, cfResult(nil, []string{"gone@example.com"}, nil))
	tp := NewCloudflareTransport(cfAccount, "token", cs.URL)

	_, err := tp.Send(context.Background(), goodCloudflareMessage())
	var te *TransportError
	if !errors.As(err, &te) {
		t.Fatalf("permanent bounce reported as success: %v", err)
	}
	if te.Class != ErrTerminal {
		t.Errorf("Class = %v, want ErrTerminal", te.Class)
	}
	if IsRetryable(err) || IsDeferrable(err) {
		t.Error("a permanent bounce must be neither retried nor queued")
	}
}

// TestCloudflare_AcceptedButNothingHappened: success:true with every
// recipient bucket empty is not an outcome we can call a send.
func TestCloudflare_AcceptedButNothingHappened(t *testing.T) {
	cs := newCaptureServer(t, http.StatusOK, cfResult(nil, nil, nil))
	tp := NewCloudflareTransport(cfAccount, "token", cs.URL)

	_, err := tp.Send(context.Background(), goodCloudflareMessage())
	var te *TransportError
	if !errors.As(err, &te) {
		t.Fatalf("empty result reported as success: %v", err)
	}
	if te.Class != ErrTransient {
		t.Errorf("Class = %v, want ErrTransient", te.Class)
	}
}

// TestCloudflare_SuccessFalse_Terminal: a 200 carrying success:false is
// a rejection, not a send.
func TestCloudflare_SuccessFalse_Terminal(t *testing.T) {
	cs := newCaptureServer(t, http.StatusOK,
		`{"success":false,"errors":[{"code":10200,"message":"invalid sender"}],"result":null}`)
	tp := NewCloudflareTransport(cfAccount, "token", cs.URL)

	_, err := tp.Send(context.Background(), goodCloudflareMessage())
	var te *TransportError
	if !errors.As(err, &te) {
		t.Fatalf("not a *TransportError: %v", err)
	}
	if te.Class != ErrTerminal {
		t.Errorf("Class = %v, want ErrTerminal", te.Class)
	}
	if !strings.Contains(te.Message, "10200") {
		t.Errorf("Message = %q, want the provider error code", te.Message)
	}
}

func TestCloudflare_RequestShape(t *testing.T) {
	cs := newCaptureServer(t, http.StatusOK, cfResult([]string{"b@example.com"}, nil, nil))
	tp := NewCloudflareTransport(cfAccount, "cf-token-abc", cs.URL)

	msg := Message{
		From:     "a@example.com",
		To:       []string{"b@example.com"},
		Subject:  "Subj",
		BodyText: "Hello",
		BodyHTML: "<p>Hello</p>",
	}
	if _, err := tp.Send(context.Background(), msg); err != nil {
		t.Fatalf("Send: %v", err)
	}

	if cs.method != http.MethodPost {
		t.Errorf("method = %q, want POST", cs.method)
	}
	wantPath := "/client/v4/accounts/" + cfAccount + "/email/sending/send"
	if cs.path != wantPath {
		t.Errorf("path = %q, want %q", cs.path, wantPath)
	}
	if got := cs.headers.Get("Authorization"); got != "Bearer cf-token-abc" {
		t.Errorf("Authorization = %q", got)
	}

	var body cloudflareRequest
	if err := json.Unmarshal(cs.body, &body); err != nil {
		t.Fatalf("body is not JSON: %v\n%s", err, cs.body)
	}
	if body.From != "a@example.com" || body.To != "b@example.com" {
		t.Errorf("from/to = %q/%q", body.From, body.To)
	}
	if body.Subject != "Subj" || body.Text != "Hello" || body.HTML != "<p>Hello</p>" {
		t.Errorf("subject/text/html = %q/%q/%q", body.Subject, body.Text, body.HTML)
	}
}

// TestCloudflare_MultipleRecipients_Terminal: the documented schema takes
// a single `to`. Fanning one submission into N provider calls would
// produce N outcomes for one message id — deliberately out of scope — so
// the adapter refuses rather than silently sending to the first only.
func TestCloudflare_MultipleRecipients_Terminal(t *testing.T) {
	cs := newCaptureServer(t, http.StatusOK, cfResult(nil, nil, nil))
	tp := NewCloudflareTransport(cfAccount, "token", cs.URL)

	msg := goodCloudflareMessage()
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
		t.Errorf("hits = %d; the request must not reach Cloudflare at all", cs.hits)
	}
}

func TestCloudflare_NoRecipients_Terminal(t *testing.T) {
	tp := NewCloudflareTransport(cfAccount, "token", "http://127.0.0.1:1")
	msg := goodCloudflareMessage()
	msg.To = nil
	_, err := tp.Send(context.Background(), msg)
	var te *TransportError
	if !errors.As(err, &te) || te.Class != ErrTerminal {
		t.Fatalf("want terminal error, got %v", err)
	}
}

// TestCloudflare_OversizeMessage_Terminal checks the documented 5 MiB
// cap locally: an oversize message can only ever be rejected, so it
// fails fast instead of burning a round trip and a retry budget.
func TestCloudflare_OversizeMessage_Terminal(t *testing.T) {
	cs := newCaptureServer(t, http.StatusOK, cfResult(nil, nil, nil))
	tp := NewCloudflareTransport(cfAccount, "token", cs.URL)

	msg := goodCloudflareMessage()
	msg.Attachments = []Attachment{{
		Filename: "big.bin", ContentType: "application/octet-stream",
		Data: make([]byte, cloudflareMaxMessageBytes),
	}}
	_, err := tp.Send(context.Background(), msg)
	var te *TransportError
	if !errors.As(err, &te) {
		t.Fatalf("not a *TransportError: %v", err)
	}
	if te.Class != ErrTerminal {
		t.Errorf("Class = %v, want ErrTerminal", te.Class)
	}
	if cs.hits != 0 {
		t.Errorf("hits = %d; oversize must be rejected before the round trip", cs.hits)
	}
}

func TestCloudflare_AttachmentEncoding(t *testing.T) {
	cs := newCaptureServer(t, http.StatusOK, cfResult([]string{"craig@example.com"}, nil, nil))
	tp := NewCloudflareTransport(cfAccount, "token", cs.URL)

	msg := goodCloudflareMessage()
	msg.Attachments = []Attachment{{
		Filename: "note.txt", ContentType: "text/plain", Data: []byte("hi"),
	}}
	if _, err := tp.Send(context.Background(), msg); err != nil {
		t.Fatalf("Send: %v", err)
	}
	var body cloudflareRequest
	_ = json.Unmarshal(cs.body, &body)
	if len(body.Attachments) != 1 {
		t.Fatalf("attachments = %+v", body.Attachments)
	}
	a := body.Attachments[0]
	if a.Content != "aGk=" || a.Filename != "note.txt" || a.Type != "text/plain" {
		t.Errorf("attachment = %+v", a)
	}
}

func TestCloudflare_429_RateLimited(t *testing.T) {
	cs := newCaptureServer(t, http.StatusTooManyRequests,
		`{"success":false,"errors":[{"code":10004,"message":"rate limited"}]}`)
	tp := NewCloudflareTransport(cfAccount, "token", cs.URL)

	_, err := tp.Send(context.Background(), goodCloudflareMessage())
	var te *TransportError
	if !errors.As(err, &te) {
		t.Fatalf("not a *TransportError: %v", err)
	}
	if te.Class != ErrRateLimited {
		t.Errorf("Class = %v, want ErrRateLimited", te.Class)
	}
}

func TestCloudflare_5xx_Transient(t *testing.T) {
	for _, status := range []int{500, 502, 503} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			cs := newCaptureServer(t, status,
				`{"success":false,"errors":[{"code":10002,"message":"internal"}]}`)
			tp := NewCloudflareTransport(cfAccount, "token", cs.URL)
			_, err := tp.Send(context.Background(), goodCloudflareMessage())
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

// TestCloudflare_AuthFailure_Terminal pins the failover invariant: a 401
// or 403 is a credential problem, so it must not be retried and must not
// be laundered into a failover to another provider.
func TestCloudflare_AuthFailure_Terminal(t *testing.T) {
	for _, status := range []int{400, 401, 403, 404} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			cs := newCaptureServer(t, status,
				`{"success":false,"errors":[{"code":10101,"message":"invalid token"}]}`)
			tp := NewCloudflareTransport(cfAccount, "token", cs.URL)
			_, err := tp.Send(context.Background(), goodCloudflareMessage())
			var te *TransportError
			if !errors.As(err, &te) {
				t.Fatalf("not a *TransportError: %v", err)
			}
			if te.Class != ErrTerminal {
				t.Errorf("Class = %v, want ErrTerminal", te.Class)
			}
			if IsRetryable(err) || IsDeferrable(err) {
				t.Error("credential failure must not be retried or queued")
			}
		})
	}
}

func TestCloudflare_NetworkError_Transient(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	url := srv.URL
	srv.Close()

	tp := NewCloudflareTransport(cfAccount, "token", url)
	_, err := tp.Send(context.Background(), goodCloudflareMessage())
	var te *TransportError
	if !errors.As(err, &te) {
		t.Fatalf("not a *TransportError: %v", err)
	}
	if te.Class != ErrTransient {
		t.Errorf("Class = %v, want ErrTransient", te.Class)
	}
}

// TestCloudflare_NoTokenInError is the NFR3 acceptance test.
func TestCloudflare_NoTokenInError(t *testing.T) {
	const sentinel = "cf-SENTINEL-must-not-leak"
	cs := newCaptureServer(t, http.StatusForbidden,
		`{"success":false,"errors":[{"code":10101,"message":"denied"}]}`)
	tp := NewCloudflareTransport(cfAccount, sentinel, cs.URL)

	_, err := tp.Send(context.Background(), goodCloudflareMessage())
	if err == nil {
		t.Fatal("want error")
	}
	if strings.Contains(err.Error(), sentinel) {
		t.Errorf("API token leaked into error: %q", err.Error())
	}
}

// TestCloudflare_ErrorMessageNotEchoed: codes are surfaced, provider
// prose is not — it can quote the submitted payload, and errors are
// logged (NFR3).
func TestCloudflare_ErrorMessageNotEchoed(t *testing.T) {
	cs := newCaptureServer(t, http.StatusBadRequest,
		`{"success":false,"errors":[{"code":10200,"message":"subject contains secret-token-xyz"}]}`)
	tp := NewCloudflareTransport(cfAccount, "token", cs.URL)

	_, err := tp.Send(context.Background(), goodCloudflareMessage())
	if err == nil {
		t.Fatal("want error")
	}
	if strings.Contains(err.Error(), "secret-token-xyz") {
		t.Errorf("provider message echoed into the error: %q", err.Error())
	}
}

// TestCloudflare_NoHeaderInjection is the NFR1/NFR2 acceptance test.
// Every submitter value crosses as a JSON string field; the payload
// survives verbatim, creates no sibling key, and creates no HTTP header.
func TestCloudflare_NoHeaderInjection(t *testing.T) {
	allowedJSONKeys := map[string]bool{
		"to": true, "from": true, "subject": true,
		"html": true, "text": true, "attachments": true,
	}
	expectedHTTPHeaders := []string{"Content-Type", "Authorization"}

	for _, tt := range []struct {
		name        string
		msg         Message
		injectField string
		payload     string
	}{
		{
			name: "crlf_bcc_in_from",
			msg: Message{From: "attacker@evil.com\r\nBcc: target@victim.com",
				To: []string{"r@example.com"}, Subject: "s", BodyText: "b"},
			injectField: "from",
			payload:     "attacker@evil.com\r\nBcc: target@victim.com",
		},
		{
			name: "crlf_bcc_in_to",
			msg: Message{From: "f@example.com",
				To: []string{"r@example.com\r\nBcc: target@victim.com"}, Subject: "s", BodyText: "b"},
			injectField: "to",
			payload:     "r@example.com\r\nBcc: target@victim.com",
		},
		{
			name: "crlf_bcc_in_subject",
			msg: Message{From: "f@example.com", To: []string{"r@example.com"},
				Subject: "Hello\r\nBcc: target@victim.com", BodyText: "b"},
			injectField: "subject",
			payload:     "Hello\r\nBcc: target@victim.com",
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
			cs := newCaptureServer(t, http.StatusOK, cfResult([]string{"x@example.com"}, nil, nil))
			tp := NewCloudflareTransport(cfAccount, "token", cs.URL)
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

func TestCloudflare_Registration(t *testing.T) {
	reg, ok := Lookup("cloudflare")
	if !ok {
		t.Fatal("cloudflare not registered")
	}
	for _, settings := range []map[string]any{
		{},
		{"account_id": "a"},
		{"api_token": "t"},
	} {
		if err := reg.Validate(settings); err == nil {
			t.Errorf("incomplete settings %v accepted", settings)
		}
	}
	valid := map[string]any{"account_id": "a", "api_token": "t"}
	if err := reg.Validate(valid); err != nil {
		t.Errorf("valid settings rejected: %v", err)
	}
	tp, err := reg.Build(valid)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	ct, ok := tp.(*CloudflareTransport)
	if !ok {
		t.Fatalf("Build returned %T, want *CloudflareTransport", tp)
	}
	if ct.BaseURL != defaultCloudflareBaseURL {
		t.Errorf("BaseURL = %q, want the production default", ct.BaseURL)
	}
}
