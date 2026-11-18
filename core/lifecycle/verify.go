package lifecycle

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Provider webhook authentication (FR120).
//
// Verify is no longer a no-op. Each adapter implements the scheme its
// provider actually publishes — and where a provider publishes none,
// the adapter says so plainly rather than inventing one. A signature
// check reimplemented from memory is worse than an absent one, because
// it looks like protection.

// VerifyError classifies an authentication failure. The Reason values
// are a closed, low-cardinality enum so they can be metric labels and
// log fields without carrying any payload content (NFR48).
type VerifyError struct {
	// Reason is one of the Reason* constants below.
	Reason string

	// Detail is operator-facing context. It must never contain the
	// secret, the signature, the Authorization header, or payload
	// content — only structural facts ("no svix-id header").
	Detail string
}

func (e *VerifyError) Error() string {
	if e.Detail == "" {
		return "webhook verification failed: " + e.Reason
	}
	return "webhook verification failed: " + e.Reason + " (" + e.Detail + ")"
}

// Verification failure reasons. Closed enum — safe as a metric label.
const (
	ReasonMissingHeaders   = "missing_headers"
	ReasonBadTimestamp     = "bad_timestamp"
	ReasonTimestampWindow  = "timestamp_outside_window"
	ReasonBadSignature     = "invalid_signature"
	ReasonSourceNotAllowed = "source_not_allowed"
	ReasonNotConfigured    = "not_configured"
)

// SvixTolerance is how far a webhook timestamp may be from now, in
// either direction, before it is rejected as a replay.
//
// Five minutes, matching the tolerance Svix itself enforces on the
// sending side. A wider window would accept captured requests for
// longer; a narrower one would reject legitimate deliveries whenever a
// receiver's clock drifts or a provider retries after a brief queue.
const SvixTolerance = 5 * time.Minute

// ProviderAuthConfig is one provider's webhook authentication
// configuration, supplied by the operator.
type ProviderAuthConfig struct {
	// SigningSecret is the provider-issued webhook secret, for
	// providers that sign. Resend issues these with a "whsec_" prefix.
	SigningSecret string

	// AllowedIPs are source addresses or CIDRs permitted to post, for
	// providers that publish a fixed egress set instead of signing.
	AllowedIPs []string

	// TrustedProxies are the CIDRs of reverse proxies in front of
	// Posthorn, so the source-IP check can read X-Forwarded-For safely.
	// Without these, only the direct peer address is trusted.
	TrustedProxies []string

	// Tolerance overrides the timestamp window. Zero means the default.
	Tolerance time.Duration
}

// svixHeaders reads the three Standard Webhooks headers, accepting both
// the "svix-" names Resend sends and the vendor-neutral "webhook-"
// aliases the Standard Webhooks spec defines for the same fields.
func svixHeaders(r *http.Request) (id, timestamp, signature string) {
	pick := func(a, b string) string {
		if v := r.Header.Get(a); v != "" {
			return v
		}
		return r.Header.Get(b)
	}
	return pick("svix-id", "webhook-id"),
		pick("svix-timestamp", "webhook-timestamp"),
		pick("svix-signature", "webhook-signature")
}

// parseSvixSecret turns an operator-configured secret into the raw HMAC
// key.
//
// Per the Svix spec the secret is issued as "whsec_" + base64. The
// prefix is stripped and the remainder base64-decoded; both padded and
// unpadded encodings are accepted because secrets are copied by hand
// and trailing "=" is easy to lose.
func parseSvixSecret(secret string) ([]byte, error) {
	raw := strings.TrimSpace(secret)
	if raw == "" {
		return nil, errors.New("signing secret is empty")
	}
	raw = strings.TrimPrefix(raw, "whsec_")
	if key, err := base64.StdEncoding.DecodeString(raw); err == nil {
		return key, nil
	}
	key, err := base64.RawStdEncoding.DecodeString(raw)
	if err != nil {
		return nil, errors.New("signing secret is not valid base64 after the whsec_ prefix")
	}
	return key, nil
}

// verifySvix implements the Standard Webhooks / Svix scheme that Resend
// uses:
//
//	signed content = "{id}.{timestamp}.{raw body}"
//	signature      = base64(HMAC-SHA256(key, signed content))
//	header         = space-delimited list of "<version>,<signature>"
//
// body MUST be the exact bytes received. Decoding and re-encoding JSON
// changes key order and whitespace and would break every signature.
//
// Unknown version prefixes are skipped rather than rejected: the spec
// allows a provider to send several signatures during a key rotation,
// and refusing an unrecognised one would break a rotation we could
// otherwise ride through.
func verifySvix(key []byte, r *http.Request, body []byte, tolerance time.Duration, now time.Time) error {
	id, ts, sigHeader := svixHeaders(r)
	if id == "" || ts == "" || sigHeader == "" {
		return &VerifyError{Reason: ReasonMissingHeaders,
			Detail: "expected svix-id, svix-timestamp and svix-signature"}
	}

	seconds, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return &VerifyError{Reason: ReasonBadTimestamp, Detail: "timestamp is not a unix second count"}
	}
	skew := now.Sub(time.Unix(seconds, 0))
	if skew < 0 {
		skew = -skew
	}
	if skew > tolerance {
		return &VerifyError{Reason: ReasonTimestampWindow,
			Detail: fmt.Sprintf("timestamp is %s from now, tolerance is %s", skew.Round(time.Second), tolerance)}
	}

	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(id))
	mac.Write([]byte("."))
	mac.Write([]byte(ts))
	mac.Write([]byte("."))
	mac.Write(body)
	expected := mac.Sum(nil)

	for _, entry := range strings.Fields(sigHeader) {
		version, value, ok := strings.Cut(entry, ",")
		if !ok || version != "v1" {
			continue
		}
		got, derr := base64.StdEncoding.DecodeString(value)
		if derr != nil {
			continue
		}
		// Constant-time: a byte-by-byte comparison would leak how much
		// of a forged signature was correct.
		if hmac.Equal(got, expected) {
			return nil
		}
	}
	return &VerifyError{Reason: ReasonBadSignature}
}

// verifySourceIP checks the request's source against an allowlist, for
// providers that publish a fixed egress set instead of signing.
//
// This is materially weaker than a signature and is documented as such
// (ADR-40): it authenticates the network path, not the payload, and it
// is only as good as the operator's proxy configuration. It is a
// defence-in-depth layer on top of the endpoint's mandatory basic auth,
// never a replacement for it.
func verifySourceIP(allowed []netAllow, trusted []netAllow, r *http.Request) error {
	if len(allowed) == 0 {
		return nil
	}
	ip := sourceIP(r, trusted)
	if ip == nil {
		return &VerifyError{Reason: ReasonSourceNotAllowed, Detail: "source address could not be determined"}
	}
	for _, a := range allowed {
		if a.contains(ip) {
			return nil
		}
	}
	return &VerifyError{Reason: ReasonSourceNotAllowed}
}

// netAllow is one parsed allowlist entry: a single address or a CIDR.
type netAllow struct {
	ip  net.IP
	net *net.IPNet
}

func (a netAllow) contains(ip net.IP) bool {
	if a.net != nil {
		return a.net.Contains(ip)
	}
	return a.ip != nil && a.ip.Equal(ip)
}

// parseNetAllow parses addresses and CIDRs into matchers.
func parseNetAllow(entries []string) ([]netAllow, error) {
	out := make([]netAllow, 0, len(entries))
	for _, raw := range entries {
		e := strings.TrimSpace(raw)
		if e == "" {
			continue
		}
		if strings.Contains(e, "/") {
			_, cidr, err := net.ParseCIDR(e)
			if err != nil {
				return nil, fmt.Errorf("%q is not a valid CIDR: %w", raw, err)
			}
			out = append(out, netAllow{net: cidr})
			continue
		}
		ip := net.ParseIP(e)
		if ip == nil {
			return nil, fmt.Errorf("%q is not a valid IP address or CIDR", raw)
		}
		out = append(out, netAllow{ip: ip})
	}
	return out, nil
}

// sourceIP resolves the client address, consulting X-Forwarded-For only
// when the direct peer is a configured trusted proxy.
//
// Trusting the header unconditionally would make the allowlist useless:
// anyone could claim a Postmark address by setting a request header.
func sourceIP(r *http.Request, trusted []netAllow) net.IP {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	peer := net.ParseIP(strings.TrimSpace(host))
	if peer == nil {
		return nil
	}
	if len(trusted) == 0 {
		return peer
	}
	isTrusted := false
	for _, t := range trusted {
		if t.contains(peer) {
			isTrusted = true
			break
		}
	}
	if !isTrusted {
		return peer
	}
	xff := r.Header.Get("X-Forwarded-For")
	if xff == "" {
		return peer
	}
	// Right-most entry that is not itself a trusted proxy is the
	// closest address the proxy chain actually observed.
	parts := strings.Split(xff, ",")
	for i := len(parts) - 1; i >= 0; i-- {
		candidate := net.ParseIP(strings.TrimSpace(parts[i]))
		if candidate == nil {
			continue
		}
		trustedHop := false
		for _, t := range trusted {
			if t.contains(candidate) {
				trustedHop = true
				break
			}
		}
		if !trustedHop {
			return candidate
		}
	}
	return peer
}
