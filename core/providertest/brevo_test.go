package providertest

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"

	"github.com/craigmccaskill/posthorn/transport"
)

// brevoWire mirrors the /v3/smtp/email body the transport builds.
// Brevo takes STRUCTURED addresses, so the decode reassembles the
// display-name form to compare against the same canonical expectation
// every other provider is checked against.
type brevoWire struct {
	Sender struct {
		Email string `json:"email"`
		Name  string `json:"name"`
	} `json:"sender"`
	To []struct {
		Email string `json:"email"`
		Name  string `json:"name"`
	} `json:"to"`
	Subject     string `json:"subject"`
	TextContent string `json:"textContent"`
	HTMLContent string `json:"htmlContent"`
}

var brevoTUT = TransportUnderTest{
	Name:        "brevo",
	Response:    `{"messageId":"<202609081200.1@smtp-relay.brevo.com>"}`,
	NewHermetic: func(baseURL string) transport.Transport { return transport.NewBrevoTransport("test-key", baseURL) },
	Decode: func(t *testing.T, req CapturedRequest) DeliveredMail {
		t.Helper()
		var w brevoWire
		if err := json.Unmarshal(req.Body, &w); err != nil {
			t.Fatalf("brevo body not valid JSON: %v\n%s", err, req.Body)
		}
		from := w.Sender.Email
		if w.Sender.Name != "" {
			from = w.Sender.Name + " <" + w.Sender.Email + ">"
		}
		to := make([]string, 0, len(w.To))
		for _, r := range w.To {
			to = append(to, r.Email)
		}
		return DeliveredMail{From: from, To: to, Subject: w.Subject, BodyText: w.TextContent}
	},
	SafetyCheck: AssertNoRawCRLF,
	CheckAuth: func(t *testing.T, req CapturedRequest) {
		t.Helper()
		// Brevo authenticates with its own header, not Bearer.
		if got := req.Header.Get("api-key"); got != "test-key" {
			t.Errorf("api-key = %q, want %q", got, "test-key")
		}
		if got := req.Header.Get("Authorization"); got != "" {
			t.Errorf("Authorization = %q, want unset", got)
		}
	},
}

func TestE2E_Brevo_FormToWire(t *testing.T) {
	RunFormScenario(t, brevoTUT, Scenario{
		Name:     "contact",
		Endpoint: contactEndpoint(),
		Form:     url.Values{"name": {"Alice"}, "message": {"Hello there"}},
		Want: DeliveredMail{
			From:     "Posthorn <noreply@example.com>",
			To:       []string{"ops@example.com"},
			Subject:  "Contact from Alice",
			BodyText: "Hello there",
		},
		InjectField: "name",
	})
}

// TestE2E_Brevo_SynthesizedHTMLIsInert covers the one place Posthorn
// generates markup: Brevo rejects a send with no htmlContent, so a
// text-only submission gets an escaped HTML part. An injection payload
// must land there as escaped text, never as live markup.
func TestE2E_Brevo_SynthesizedHTMLIsInert(t *testing.T) {
	form := url.Values{
		"name":    {"Alice"},
		"message": {`<img src=x onerror="alert(1)">`},
	}
	req := driveForm(t, brevoTUT, contactEndpoint(), form)

	var w brevoWire
	if err := json.Unmarshal(req.Body, &w); err != nil {
		t.Fatalf("brevo body not valid JSON: %v", err)
	}
	if w.HTMLContent == "" {
		t.Fatal("no htmlContent synthesized; Brevo would reject this send")
	}
	// Strip the only tags Posthorn itself adds; anything angle-bracketed
	// left over came from the submitter and would be live markup.
	stripped := strings.NewReplacer("<p>", "", "</p>", "", "<br>", "").Replace(w.HTMLContent)
	if strings.ContainsAny(stripped, "<>") {
		t.Errorf("htmlContent carries submitter markup: %q", w.HTMLContent)
	}
	if !strings.Contains(w.HTMLContent, "&lt;img") {
		t.Errorf("htmlContent = %q, want the payload escaped", w.HTMLContent)
	}
	if w.TextContent != `<img src=x onerror="alert(1)">` {
		t.Errorf("textContent = %q, want the submitter's text verbatim", w.TextContent)
	}
}
