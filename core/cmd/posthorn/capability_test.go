package main

import (
	"testing"

	"github.com/craigmccaskill/posthorn/lifecycle"
	"github.com/craigmccaskill/posthorn/transport"
)

// The capability matrix is a promise made to operators, and this is the
// composition root — the one package that sees both the transport
// registry and the webhook adapter registry. So the matrix is checked
// against reality here rather than being trusted as documentation.

// TestCapabilityMatrix_WebhookClaimsMatchRegisteredAdapters: a provider
// whose matrix row says webhook = yes must actually have an adapter an
// operator can mount, and one that says no or unsupported must not have
// one sitting unmounted.
func TestCapabilityMatrix_WebhookClaimsMatchRegisteredAdapters(t *testing.T) {
	registered := map[string]bool{}
	for _, name := range lifecycle.AdapterNames() {
		registered[name] = true
	}

	for _, c := range transport.CapabilityMatrix() {
		claims := c.Webhook == transport.CapYes
		switch {
		case claims && !registered[c.Type]:
			t.Errorf("%s: matrix claims webhook = yes, but no adapter is registered "+
				"(adapters: %v)", c.Type, lifecycle.AdapterNames())
		case !claims && registered[c.Type]:
			t.Errorf("%s: an adapter is registered, but the matrix says webhook = %q — "+
				"an operator reading the matrix would not know the route exists",
				c.Type, c.Webhook)
		}
	}
}

// TestCapabilityMatrix_VerificationClaimsAreReal: only providers that
// publish a payload-signature scheme may claim verification = yes. The
// negative half is the one that matters — Postmark and Brevo publish no
// signature, and their adapters REFUSE a signing_secret rather than
// accepting one that nothing checks.
func TestCapabilityMatrix_VerificationClaimsAreReal(t *testing.T) {
	for _, c := range transport.CapabilityMatrix() {
		if c.Webhook != transport.CapYes {
			continue
		}
		_, known, err := lifecycle.BuildAdapter(c.Type, lifecycle.ProviderAuthConfig{
			SigningSecret: "whsec_" + "dGVzdC1zZWNyZXQtdmFsdWU=",
		})
		if !known {
			t.Fatalf("%s: adapter not registered", c.Type)
		}
		if c.WebhookVerification == transport.CapYes {
			if err != nil {
				t.Errorf("%s claims verification = yes but rejects a signing secret: %v", c.Type, err)
			}
			continue
		}
		// verification is no/unsupported: configuring a secret must be
		// a startup error, never a silently ignored setting.
		if err == nil {
			t.Errorf("%s: verification = %q, yet a signing_secret was accepted — "+
				"the operator would believe payloads are verified when nothing verifies them",
				c.Type, c.WebhookVerification)
		}
	}
}

// TestCapabilityMatrix_CoversEveryBuildableTransport: buildTransport is
// what turns config into a provider, so anything it can build must
// appear in the matrix.
func TestCapabilityMatrix_CoversEveryBuildableTransport(t *testing.T) {
	if err := transport.VerifyCapabilityCoverage(); err != nil {
		t.Fatal(err)
	}
}
