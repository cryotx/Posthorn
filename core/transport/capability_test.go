package transport

import "testing"

// TestCapabilityCoverage: every registered transport type must declare
// its capabilities. A provider that ships without an entry is invisible
// in the matrix operators plan against.
func TestCapabilityCoverage(t *testing.T) {
	if err := VerifyCapabilityCoverage(); err != nil {
		t.Fatal(err)
	}
}

// TestCapabilityValuesAreUnambiguous enforces the rule the matrix exists
// for: exactly three values, no blanks, no invented middle ground like
// "partial" or "planned". An operator must never have to guess whether a
// cell means "not yet" or "never".
func TestCapabilityValuesAreUnambiguous(t *testing.T) {
	valid := map[Capability]bool{CapYes: true, CapNo: true, CapUnsupported: true}
	for _, c := range CapabilityMatrix() {
		fields := map[string]Capability{
			"Send":                c.Send,
			"Webhook":             c.Webhook,
			"WebhookVerification": c.WebhookVerification,
			"RemoteQuota":         c.RemoteQuota,
		}
		for name, v := range fields {
			if !valid[v] {
				t.Errorf("%s.%s = %q, not one of yes/no/unsupported", c.Type, name, v)
			}
		}
	}
}

// TestCapabilityUnsupportedIsExplained: "unsupported" is a claim about
// the PROVIDER, not about Posthorn, so it needs a reason an operator can
// check. Without one, an operator can't tell a real provider limitation
// from an undocumented shortcut.
func TestCapabilityUnsupportedIsExplained(t *testing.T) {
	for _, c := range CapabilityMatrix() {
		unsupported := c.Send == CapUnsupported ||
			c.Webhook == CapUnsupported ||
			c.WebhookVerification == CapUnsupported ||
			c.RemoteQuota == CapUnsupported
		if unsupported && c.Notes == "" {
			t.Errorf("%s declares an unsupported capability with no Notes", c.Type)
		}
	}
}

// TestCapabilityVerificationImpliesWebhook: verifying a callback that is
// never accepted is meaningless, and claiming verification without
// ingestion would read as stronger coverage than exists.
func TestCapabilityVerificationImpliesWebhook(t *testing.T) {
	for _, c := range CapabilityMatrix() {
		if c.WebhookVerification == CapYes && c.Webhook != CapYes {
			t.Errorf("%s claims webhook verification but webhook = %q", c.Type, c.Webhook)
		}
	}
}

// TestCapabilityEverySenderSends: every provider in the matrix is a send
// path — Posthorn has no receive-only provider (design principle 2).
func TestCapabilityEverySenderSends(t *testing.T) {
	for _, c := range CapabilityMatrix() {
		if c.Send != CapYes {
			t.Errorf("%s.Send = %q; a registered transport must be able to send", c.Type, c.Send)
		}
	}
}

// TestCapabilityMatrixIsSorted keeps the docs/admin rendering stable
// across runs — the matrix is copied into documentation.
func TestCapabilityMatrixIsSorted(t *testing.T) {
	m := CapabilityMatrix()
	for i := 1; i < len(m); i++ {
		if m[i-1].Type >= m[i].Type {
			t.Fatalf("matrix not sorted by type: %q before %q", m[i-1].Type, m[i].Type)
		}
	}
}

// TestCapabilityNativeAdaptersEarnTheirKeep pins the v1.0 rule: a native
// adapter must offer something the generic SMTP relay cannot. Cloudflare
// is the borderline case — no webhook, no quota, no message id — and it
// qualifies on synchronous per-recipient delivery status, which is
// asserted in cloudflare_test.go rather than here.
func TestCapabilityNativeAdaptersEarnTheirKeep(t *testing.T) {
	for _, c := range CapabilityMatrix() {
		if c.GenericSMTP || c.Type == "webhook" {
			continue
		}
		if c.Webhook == CapYes || c.RemoteQuota == CapYes {
			continue
		}
		// No webhook and no quota: the justification must at least be
		// written down for an operator choosing between this and SMTP.
		if c.Notes == "" {
			t.Errorf("%s is a native adapter with no webhook or quota and no Notes "+
				"explaining what it offers over the generic SMTP relay", c.Type)
		}
	}
}
