package transport

import (
	"fmt"
	"sort"
	"strings"
)

// MailKind labels what a message is *for*, so routing can send a
// password reset down a different path than a weekly digest.
//
// The set is closed and small on purpose. It is a routing input, not a
// campaign taxonomy — Posthorn does not schedule, segment, or track
// opens, and a free-form label would invite exactly that. An operator
// who needs a finer distinction already has `match_field`.
//
// The zero value (KindUnspecified) is what every v2.1 config produces
// and what every rule that does not mention kinds ignores.
type MailKind string

const (
	// KindUnspecified is the default: the operator said nothing about
	// what this message is.
	KindUnspecified MailKind = ""

	// KindVerification is a signup / email-confirmation message. Time
	// critical: the user is staring at a "check your inbox" screen.
	KindVerification MailKind = "verification"

	// KindPasswordReset is a credential-recovery message. Time critical
	// and security sensitive.
	KindPasswordReset MailKind = "password_reset"

	// KindNotification is routine app traffic — a comment reply, a build
	// result. Latency-tolerant; the natural fit for a free tier.
	KindNotification MailKind = "notification"

	// KindImportant is business-critical mail: receipts, invoices, legal
	// notices. Deliverability matters more than cost.
	KindImportant MailKind = "important"

	// KindMarketing is opt-in bulk mail. Named so it can be routed away
	// from the domains and providers that carry transactional traffic —
	// Posthorn provides no campaign machinery for it and never will
	// (that is Listmonk's lane).
	KindMarketing MailKind = "marketing"
)

var mailKinds = map[MailKind]bool{
	KindVerification:  true,
	KindPasswordReset: true,
	KindNotification:  true,
	KindImportant:     true,
	KindMarketing:     true,
}

// ParseMailKind validates an operator- or caller-supplied kind. The
// empty string is valid and means unspecified.
func ParseMailKind(s string) (MailKind, error) {
	k := MailKind(strings.TrimSpace(s))
	if k == KindUnspecified || mailKinds[k] {
		return k, nil
	}
	return "", fmt.Errorf("unknown mail kind %q (known: %s)", s, strings.Join(KnownMailKinds(), ", "))
}

// KnownMailKinds returns the accepted values, sorted for stable errors.
func KnownMailKinds() []string {
	out := make([]string, 0, len(mailKinds))
	for k := range mailKinds {
		out = append(out, string(k))
	}
	sort.Strings(out)
	return out
}

// String makes MailKind printable in logs and metric labels. Every value
// is operator- or enum-controlled, so it is NFR24-safe.
func (k MailKind) String() string {
	if k == KindUnspecified {
		return "unspecified"
	}
	return string(k)
}
