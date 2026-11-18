// Package netguard decides which destinations Posthorn's own outbound
// HTTP requests may reach (NFR45).
//
// Three config values point Posthorn at a URL an operator supplies:
// an endpoint's webhook_url, a project's webhook_url, and the remote
// quota endpoint. Each turns "a string in a config file" into "a
// request Posthorn makes from inside the operator's network", which is
// the shape of an SSRF: a URL naming 169.254.169.254 makes Posthorn
// fetch cloud instance credentials and POST them to nowhere useful —
// but a URL naming an internal admin service makes Posthorn act as a
// confused deputy against it.
//
// So the destination is checked TWICE, and the second check is the one
// that matters:
//
//	ValidateURL  at config-parse time. Catches the obvious cases early,
//	             with a message naming the offending host.
//	Control      at CONNECT time, on the address the resolver actually
//	             returned. A hostname resolving to 127.0.0.1 — whether
//	             by an attacker's DNS record, a rebinding attack, or an
//	             /etc/hosts line — is blocked here even though the URL
//	             looked public. Parse-time validation alone cannot see
//	             this, which is why it is not the security boundary.
//
// Redirects are re-checked because every hop dials again through the
// same Control.
//
// The default is deny. But Posthorn is a SINGLE-INSTANCE, self-hosted
// gateway, and forwarding events to an application on the same host is
// an ordinary deployment, not an attack — so AllowPrivate is the
// documented opt-in for exactly that. It never unblocks link-local:
// no legitimate webhook receiver lives on 169.254.0.0/16, and that is
// where every cloud metadata service does.
package netguard

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"syscall"
	"time"
)

// Policy is one destination policy. The zero value is the strict
// default: public destinations only.
type Policy struct {
	// AllowPrivate permits loopback, private and CGNAT destinations —
	// the opt-in an operator sets when the receiver is their own app on
	// 127.0.0.1 or a service on the LAN. Link-local stays blocked.
	AllowPrivate bool
}

// ErrBlocked is the sentinel for a destination the policy refuses.
var ErrBlocked = errors.New("destination blocked by SSRF policy")

// blockReason describes why an address is refused, in operator terms.
type blockReason string

const (
	reasonLoopback   blockReason = "a loopback address"
	reasonPrivate    blockReason = "a private-network address"
	reasonLinkLocal  blockReason = "a link-local address (this range holds cloud metadata services)"
	reasonCGNAT      blockReason = "a carrier-grade NAT address"
	reasonUnroutable blockReason = "an unspecified, multicast or broadcast address"
)

// alibabaMetadata is reachable at a fixed public-looking address rather
// than a link-local one, so it needs naming explicitly.
var alibabaMetadata = netip.MustParseAddr("100.100.100.100")

// classify returns the reason an address is refused, or "" if it is a
// destination the policy allows.
func (p Policy) classify(addr netip.Addr) blockReason {
	// An IPv4-mapped IPv6 address (::ffff:127.0.0.1) is the classic way
	// to smuggle a blocked v4 address past a v6-unaware check.
	addr = addr.Unmap()

	switch {
	case !addr.IsValid(), addr.IsUnspecified(), addr.IsMulticast(),
		addr.IsInterfaceLocalMulticast(), addr.IsLinkLocalMulticast():
		return reasonUnroutable
	case addr.Is4() && addr.As4() == [4]byte{255, 255, 255, 255}:
		return reasonUnroutable
	case addr.IsLinkLocalUnicast():
		// Never unblocked, not even by AllowPrivate: this is where AWS,
		// GCP, Azure and Oracle all serve instance credentials.
		return reasonLinkLocal
	}

	if p.AllowPrivate {
		return ""
	}

	switch {
	case addr.IsLoopback():
		return reasonLoopback
	case addr.IsPrivate():
		return reasonPrivate
	case addr == alibabaMetadata:
		return reasonLinkLocal
	case addr.Is4() && addr.As4()[0] == 100 && addr.As4()[1]&0xc0 == 64:
		// 100.64.0.0/10 — shared address space, not routable to the
		// public internet, so a webhook there is an internal target.
		return reasonCGNAT
	case addr.Is4() && addr.As4()[0] == 0:
		return reasonUnroutable
	}
	return ""
}

// ValidateURL checks a configured URL at parse time: the scheme must be
// http or https, a host must be present, and a literal IP host must be
// one the policy allows.
//
// A HOSTNAME is deliberately not resolved here. Resolution at parse
// time proves nothing about what the name will resolve to at request
// time, and a config that fails to load because a DNS server was
// briefly unreachable would be worse than useless. Control is what
// enforces the policy for hostnames.
func (p Policy) ValidateURL(raw string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return fmt.Errorf("not a valid URL: %w", err)
	}
	switch u.Scheme {
	case "http", "https":
	default:
		return fmt.Errorf("must be an http(s) URL, got scheme %q", u.Scheme)
	}
	host := u.Hostname()
	if host == "" {
		return errors.New("must include a host")
	}
	// "localhost" and friends resolve to loopback but are not literal
	// IPs, so name them explicitly for a clearer config-time error than
	// a connection refusal at the first delivery.
	if !p.AllowPrivate && isLoopbackName(host) {
		return fmt.Errorf("%w: %q is %s (set allow_private_targets to permit it)",
			ErrBlocked, host, reasonLoopback)
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		// A hostname. Checked at connect time instead.
		return nil
	}
	if reason := p.classify(addr); reason != "" {
		return fmt.Errorf("%w: %s is %s", ErrBlocked, host, reason)
	}
	return nil
}

func isLoopbackName(host string) bool {
	h := strings.ToLower(strings.TrimSuffix(host, "."))
	return h == "localhost" || strings.HasSuffix(h, ".localhost")
}

// Control is a net.Dialer Control function that refuses a connection to
// a blocked address. This is the real enforcement point: it sees the
// address the resolver returned, for the original request and for every
// redirect hop, so a name that resolves to a blocked address is stopped
// even though its URL passed ValidateURL.
func (p Policy) Control(network, address string, _ syscall.RawConn) error {
	switch network {
	case "tcp", "tcp4", "tcp6":
	default:
		return fmt.Errorf("%w: network %q is not permitted", ErrBlocked, network)
	}
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("%w: unparseable destination %q", ErrBlocked, address)
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		// Control always receives a resolved literal; anything else is
		// unexpected and is refused rather than guessed at.
		return fmt.Errorf("%w: unresolved destination %q", ErrBlocked, host)
	}
	if reason := p.classify(addr); reason != "" {
		// The ADDRESS is named, not the hostname: an operator debugging
		// this needs to know what the name resolved to.
		return fmt.Errorf("%w: %s is %s", ErrBlocked, addr, reason)
	}
	return nil
}

// Dialer returns a dialer that enforces the policy.
func (p Policy) Dialer(timeout time.Duration) *net.Dialer {
	return &net.Dialer{Timeout: timeout, KeepAlive: 30 * time.Second, Control: p.Control}
}

// Client returns an http.Client whose every connection — including
// redirect hops — is checked against the policy.
//
// Redirects are additionally capped low: a chain of redirects is a
// standard way to walk a fetcher somewhere it would not go directly,
// and no legitimate webhook receiver needs more than a couple.
func (p Policy) Client(timeout time.Duration) *http.Client {
	d := p.Dialer(timeout)
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			DialContext:           d.DialContext,
			Proxy:                 http.ProxyFromEnvironment,
			MaxIdleConns:          10,
			IdleConnTimeout:       30 * time.Second,
			TLSHandshakeTimeout:   timeout,
			ExpectContinueTimeout: time.Second,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 3 {
				return fmt.Errorf("%w: too many redirects", ErrBlocked)
			}
			switch req.URL.Scheme {
			case "http", "https":
				return nil
			default:
				return fmt.Errorf("%w: redirect to scheme %q", ErrBlocked, req.URL.Scheme)
			}
		},
	}
}

// CheckAddr reports whether a resolved address is permitted. Exposed
// for the doctor command and for tests.
func (p Policy) CheckAddr(addr netip.Addr) error {
	if reason := p.classify(addr); reason != "" {
		return fmt.Errorf("%w: %s is %s", ErrBlocked, addr, reason)
	}
	return nil
}

// Resolve reports what a configured URL's host currently resolves to
// and whether each address is permitted. Used by `posthorn doctor` to
// tell an operator why a webhook will be refused BEFORE an event needs
// delivering, rather than at 3am in a retry log.
func (p Policy) Resolve(ctx context.Context, raw string) ([]netip.Addr, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	host := u.Hostname()
	if addr, perr := netip.ParseAddr(host); perr == nil {
		return []netip.Addr{addr}, p.CheckAddr(addr)
	}
	addrs, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil, err
	}
	for _, a := range addrs {
		if cerr := p.CheckAddr(a); cerr != nil {
			return addrs, cerr
		}
	}
	return addrs, nil
}
