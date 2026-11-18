// Package mask renders sensitive values in a form that is useful to
// look at and useless to steal (FR142, FR148).
//
// Two different jobs, deliberately in one small package so the rules
// are read together:
//
//	Secret  — a credential. Shown as ********abcd so an operator can
//	          tell WHICH key a config refers to without the value.
//	Email   — a recipient. Shown as u***@example.com so an operator can
//	          recognise a delivery without the admin surface becoming a
//	          harvestable address list.
//
// Neither is reversible, and neither is a substitute for not having the
// value: the masking exists for surfaces that legitimately display a
// row, not as permission to pass secrets around and mask them later.
package mask

import "strings"

// Secret renders a credential as a fixed prefix plus its last four
// characters: "********abcd".
//
// The tail rather than the head, because provider keys are prefixed by
// convention ("whsec_", "xkeysib-", "ph_live_") and a masked head would
// show only the part every key of that type shares. Four characters is
// enough to tell two keys apart and far too few to guess one.
//
// A short secret shows no tail at all: revealing four of six characters
// is a meaningful fraction of the value.
func Secret(s string) string {
	const shown = 4
	if s == "" {
		return ""
	}
	if len(s) < 12 {
		return "********"
	}
	return "********" + s[len(s)-shown:]
}

// Email renders an address as "u***@example.com".
//
// The DOMAIN survives because it is what an operator actually reasons
// about — which provider, which customer, which bounce pattern — and it
// is not personal data in the way the local part is. The first
// character of the local part survives so two recipients at the same
// domain are visibly different rows.
func Email(addr string) string {
	at := strings.LastIndex(addr, "@")
	if at <= 0 {
		// Not an address shape: mask it entirely rather than guess.
		if addr == "" {
			return ""
		}
		return "***"
	}
	local, domain := addr[:at], addr[at+1:]
	if domain == "" {
		return "***"
	}
	if len(local) == 1 {
		return local + "***@" + domain
	}
	return local[:1] + "***@" + domain
}

// Emails masks a list, preserving order.
func Emails(addrs []string) []string {
	if addrs == nil {
		return nil
	}
	out := make([]string, len(addrs))
	for i, a := range addrs {
		out[i] = Email(a)
	}
	return out
}
