package transport

import (
	"fmt"
	"sort"
	"sync"
)

// Provider capability registry (FR135).
//
// "Supports Resend" is three different claims — can Posthorn SEND
// through it, does it accept that provider's delivery WEBHOOKS, and can
// it VERIFY those webhooks are genuine — and conflating them is how an
// operator ends up trusting a callback nobody authenticated.
//
// So each provider declares each capability separately, and every entry
// is one of three explicit values. There is deliberately no "partial"
// or "planned": a capability is available, absent, or the provider does
// not offer it at all.
type Capability string

const (
	// CapYes: implemented and usable.
	CapYes Capability = "yes"

	// CapNo: the provider offers it, Posthorn has not implemented it.
	// A gap Posthorn could close.
	CapNo Capability = "no"

	// CapUnsupported: the PROVIDER does not offer it. Not a gap —
	// nothing to implement. Postmark's payload signature is the
	// canonical example.
	CapUnsupported Capability = "unsupported"
)

// Capabilities describes one provider type's real surface.
type Capabilities struct {
	// Type is the registered transport type ("resend", "smtp", ...).
	Type string

	// Send: can Posthorn deliver mail through this provider.
	Send Capability

	// Webhook: does Posthorn accept this provider's delivery-event
	// callbacks (an adapter exists and is mounted).
	Webhook Capability

	// WebhookVerification: can Posthorn prove a callback genuinely came
	// from this provider. CapUnsupported where the provider publishes
	// no verifiable scheme — the honest answer, not a gap.
	WebhookVerification Capability

	// RemoteQuota: can Posthorn read this provider's own usage/quota
	// API for reconciliation.
	RemoteQuota Capability

	// GenericSMTP marks the catch-all relay rather than a vendor API.
	GenericSMTP bool

	// Notes is one operator-facing sentence explaining any
	// CapUnsupported entry. Required where a value is unsupported: an
	// operator reading a matrix needs to know whether to go looking for
	// a workaround.
	Notes string
}

var (
	capMu   sync.RWMutex
	capReg  = map[string]Capabilities{}
	capSeen = map[string]bool{}
)

// RegisterCapabilities records one provider type's surface. Called from
// the same init() that registers the transport, so a new provider
// cannot ship without declaring what it actually does.
func RegisterCapabilities(c Capabilities) {
	if c.Type == "" {
		panic("transport.RegisterCapabilities: Type is required")
	}
	capMu.Lock()
	defer capMu.Unlock()
	if capSeen[c.Type] {
		panic("transport.RegisterCapabilities: duplicate entry for " + c.Type)
	}
	capSeen[c.Type] = true
	capReg[c.Type] = c
}

// CapabilitiesFor returns one provider type's declared surface.
func CapabilitiesFor(typ string) (Capabilities, bool) {
	capMu.RLock()
	defer capMu.RUnlock()
	c, ok := capReg[typ]
	return c, ok
}

// CapabilityMatrix returns every provider's surface, sorted by type —
// the table that goes in the docs, the admin API and the UI.
func CapabilityMatrix() []Capabilities {
	capMu.RLock()
	defer capMu.RUnlock()
	types := make([]string, 0, len(capReg))
	for t := range capReg {
		types = append(types, t)
	}
	sort.Strings(types)
	out := make([]Capabilities, 0, len(types))
	for _, t := range types {
		out = append(out, capReg[t])
	}
	return out
}

// VerifyCapabilityCoverage reports provider types that registered a
// transport but no capability declaration. Called from the doctor
// command: an undeclared provider is a documentation hole, and the
// matrix is what operators plan against.
func VerifyCapabilityCoverage() error {
	var missing []string
	for _, t := range KnownTypes() {
		if _, ok := CapabilitiesFor(t); !ok {
			missing = append(missing, t)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("provider types with no capability declaration: %v", missing)
	}
	return nil
}

func init() {
	// The matrix, as it actually stands. Every "unsupported" carries
	// the reason, because an operator seeing a blank cell will
	// otherwise assume it is a Posthorn gap they can wait out.
	RegisterCapabilities(Capabilities{
		Type: "postmark", Send: CapYes, Webhook: CapYes,
		WebhookVerification: CapUnsupported, RemoteQuota: CapUnsupported,
		Notes: "Postmark does not sign webhook payloads; its documented mechanisms are basic auth and IP allowlisting, both implemented. It publishes no usage API.",
	})
	RegisterCapabilities(Capabilities{
		Type: "resend", Send: CapYes, Webhook: CapYes,
		WebhookVerification: CapYes, RemoteQuota: CapUnsupported,
		Notes: "Signs with Standard Webhooks (Svix), fully verified. No public usage/quota API.",
	})
	RegisterCapabilities(Capabilities{
		Type: "brevo", Send: CapYes, Webhook: CapYes,
		WebhookVerification: CapUnsupported, RemoteQuota: CapUnsupported,
		Notes: "Brevo documents IP allowlisting for webhooks, not a payload signature; the allowlist is implemented. No usage API documented for transactional sending.",
	})
	RegisterCapabilities(Capabilities{
		Type: "cloudflare", Send: CapYes, Webhook: CapUnsupported,
		WebhookVerification: CapUnsupported, RemoteQuota: CapUnsupported,
		Notes: "Cloudflare Email Service has no per-message delivery webhook (its Notifications are account-level alerts) and no queryable send-quota API. The REST send returns per-recipient status synchronously instead.",
	})
	RegisterCapabilities(Capabilities{
		Type: "ses", Send: CapYes, Webhook: CapNo,
		WebhookVerification: CapNo, RemoteQuota: CapNo,
		Notes: "SES delivers events via SNS, which Posthorn has no adapter for. Implementable; not implemented.",
	})
	RegisterCapabilities(Capabilities{
		Type: "mailgun", Send: CapYes, Webhook: CapNo,
		WebhookVerification: CapNo, RemoteQuota: CapNo,
		Notes: "Mailgun signs webhooks (timestamp/token/signature), so verification is implementable; Posthorn has no adapter yet.",
	})
	RegisterCapabilities(Capabilities{
		Type: "smtp", Send: CapYes, Webhook: CapUnsupported,
		WebhookVerification: CapUnsupported, RemoteQuota: CapUnsupported,
		GenericSMTP: true,
		Notes:       "The generic relay. SMTP has no callback channel by construction — delivery state comes only from what the relay accepts at hand-off.",
	})
	RegisterCapabilities(Capabilities{
		Type: "webhook", Send: CapYes, Webhook: CapUnsupported,
		WebhookVerification: CapUnsupported, RemoteQuota: CapUnsupported,
		Notes: "A non-mail sink: Posthorn POSTs the submission to a receiver the operator runs. There is no upstream provider to call back.",
	})
	RegisterCapabilities(Capabilities{
		Type: "sendernet", Send: CapYes, Webhook: CapNo,
		WebhookVerification: CapNo, RemoteQuota: CapNo,
		Notes: "Sender.net's docs reference account-level webhooks on paid plans, but no payload-signature scheme or send-event shape was confirmed — Posthorn has no adapter for it yet. No documented rate-limit or usage-query API either.",
	})
}
