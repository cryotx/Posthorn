package config_test

import (
	"strings"
	"testing"
)

// The SSRF surface is every URL an operator can point Posthorn AT:
// endpoint and project webhook_url, and the webhook transport's own
// destination. Parse-time rejection is the legible half of the defense
// (netguard's connect-time check is the half that holds); these tests
// pin that a config naming an internal address does not load at all.

// webhookConfig builds a config whose endpoint forwards events to url.
func webhookConfig(url, security string) string {
	return security + `
[storage]
path = ":memory:"

[lifecycle]
basic_auth_username = "hook"
basic_auth_password = "hook-password-value"

[[endpoints]]
path = "/api/contact"
to = ["craig@example.com"]
from = "noreply@example.com"
subject = "Contact"
body = "Body"
webhook_url = "` + url + `"
webhook_secret = "0123456789abcdef0123"

[endpoints.transport]
type = "postmark"

[endpoints.transport.settings]
api_key = "test-key"
`
}

func TestSSRF_WebhookURLRejectsInternalTargets(t *testing.T) {
	for _, url := range []string{
		"http://127.0.0.1:9000/hook",
		"http://localhost:9000/hook",
		"http://10.0.0.5/hook",
		"http://192.168.1.10/hook",
		"http://[::1]:9000/hook",
		// The one that matters most: cloud instance credentials.
		"http://169.254.169.254/latest/meta-data/iam/security-credentials/",
	} {
		_, err := loadString(t, webhookConfig(url, ""))
		if err == nil {
			t.Errorf("%s accepted as a webhook_url", url)
			continue
		}
		if !strings.Contains(err.Error(), "SSRF") {
			t.Errorf("%s: error = %v, want it to name the SSRF policy", url, err)
		}
	}
}

func TestSSRF_WebhookURLAcceptsPublicTargets(t *testing.T) {
	if _, err := loadString(t, webhookConfig("https://hooks.example.com/posthorn", "")); err != nil {
		t.Errorf("public webhook_url rejected: %v", err)
	}
}

// The opt-in exists because forwarding to the operator's own app on
// 127.0.0.1 is an ordinary single-instance deployment, not an attack.
func TestSSRF_AllowPrivateOptIn(t *testing.T) {
	const security = `
[security]
allow_private_webhook_targets = true
`
	if _, err := loadString(t, webhookConfig("http://127.0.0.1:9000/hook", security)); err != nil {
		t.Errorf("opted-in loopback webhook_url rejected: %v", err)
	}

	// ...but the opt-in is not a master key: link-local stays blocked,
	// because no webhook receiver lives there and every cloud metadata
	// service does.
	_, err := loadString(t, webhookConfig("http://169.254.169.254/latest/meta-data/", security))
	if err == nil {
		t.Error("allow_private_webhook_targets unblocked the metadata address")
	}
}

func TestSSRF_WebhookTransportDestination(t *testing.T) {
	tmpl := `
[[endpoints]]
path = "/api/contact"
to = ["craig@example.com"]
from = "noreply@example.com"
subject = "Contact"
body = "Body"

[endpoints.transport]
type = "webhook"

[endpoints.transport.settings]
url = "%s"
secret = "0123456789abcdef0123"
`
	if _, err := loadString(t, strings.Replace(tmpl, "%s", "http://169.254.169.254/", 1)); err == nil {
		t.Error("webhook transport accepted the metadata address")
	}
	if _, err := loadString(t, strings.Replace(tmpl, "%s", "https://sink.example.com/in", 1)); err != nil {
		t.Errorf("public webhook transport destination rejected: %v", err)
	}
}

func TestSSRF_ProjectWebhookURL(t *testing.T) {
	tmpl := `
[storage]
path = ":memory:"

[lifecycle]
basic_auth_username = "hook"
basic_auth_password = "hook-password-value"

[[projects]]
name = "acme"
api_key = "ph_live_0123456789abcdef0123456789abcdef"
allowed_from_domains = ["example.com"]
webhook_url = "%s"
webhook_secret = "0123456789abcdef0123"

[[endpoints]]
path = "/api/contact"
to = ["craig@example.com"]
from = "noreply@example.com"
subject = "Contact"
body = "Body"

[endpoints.transport]
type = "postmark"

[endpoints.transport.settings]
api_key = "test-key"
`
	_, err := loadString(t, strings.Replace(tmpl, "%s", "http://192.168.1.50/events", 1))
	if err == nil {
		t.Fatal("project webhook_url accepted a private address")
	}
	if !strings.Contains(err.Error(), "SSRF") {
		t.Errorf("error = %v, want it to name the SSRF policy", err)
	}
	if _, err := loadString(t, strings.Replace(tmpl, "%s", "https://acme.example.com/events", 1)); err != nil {
		t.Errorf("public project webhook_url rejected: %v", err)
	}
}
