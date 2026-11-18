package providertest

import (
	"encoding/json"
	"net/url"
	"testing"

	"github.com/craigmccaskill/posthorn/transport"
)

// cloudflareWire mirrors the Email Service send body.
type cloudflareWire struct {
	From    string `json:"from"`
	To      string `json:"to"`
	Subject string `json:"subject"`
	Text    string `json:"text"`
}

var cloudflareTUT = TransportUnderTest{
	Name: "cloudflare",
	// The canned success mirrors the documented envelope: per-recipient
	// status rather than a message id.
	Response: `{"success":true,"errors":[],"result":{"delivered":["ops@example.com"],"permanent_bounces":[],"queued":[]}}`,
	NewHermetic: func(baseURL string) transport.Transport {
		return transport.NewCloudflareTransport("acct-1", "test-token", baseURL)
	},
	Decode: func(t *testing.T, req CapturedRequest) DeliveredMail {
		t.Helper()
		var w cloudflareWire
		if err := json.Unmarshal(req.Body, &w); err != nil {
			t.Fatalf("cloudflare body not valid JSON: %v\n%s", err, req.Body)
		}
		return DeliveredMail{From: w.From, To: []string{w.To}, Subject: w.Subject, BodyText: w.Text}
	},
	SafetyCheck: AssertNoRawCRLF,
	CheckAuth: func(t *testing.T, req CapturedRequest) {
		t.Helper()
		if got := req.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Errorf("Authorization = %q, want %q", got, "Bearer test-token")
		}
	},
}

func TestE2E_Cloudflare_FormToWire(t *testing.T) {
	RunFormScenario(t, cloudflareTUT, Scenario{
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
