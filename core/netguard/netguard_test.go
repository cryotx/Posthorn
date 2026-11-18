package netguard

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"
)

func TestValidateURL_BlocksLiteralAddresses(t *testing.T) {
	var strict Policy
	for _, raw := range []string{
		"http://127.0.0.1/hook",
		"http://127.1.2.3/hook",
		"http://[::1]/hook",
		// IPv4-mapped IPv6: the classic way past a v6-unaware check.
		"http://[::ffff:127.0.0.1]/hook",
		"http://localhost/hook",
		"http://LOCALHOST./hook",
		"http://app.localhost/hook",
		"http://10.0.0.5/hook",
		"http://172.16.0.1/hook",
		"http://192.168.1.10/hook",
		"http://[fd00::1]/hook",
		// Cloud metadata, the reason this package exists.
		"http://169.254.169.254/latest/meta-data/",
		"http://[fe80::1]/hook",
		"http://100.100.100.100/hook",
		"http://100.64.0.1/hook",
		"http://0.0.0.0/hook",
		"http://255.255.255.255/hook",
	} {
		if err := strict.ValidateURL(raw); err == nil {
			t.Errorf("%s accepted, want blocked", raw)
		} else if !errors.Is(err, ErrBlocked) {
			t.Errorf("%s: error = %v, want ErrBlocked", raw, err)
		}
	}
}

func TestValidateURL_AllowsPublicDestinations(t *testing.T) {
	var strict Policy
	for _, raw := range []string{
		"https://hooks.example.com/posthorn",
		"http://93.184.216.34/hook",
		"https://[2606:2800:220:1:248:1893:25c8:1946]/hook",
		"https://example.com:8443/hook",
		"https://user:pass@example.com/hook",
	} {
		if err := strict.ValidateURL(raw); err != nil {
			t.Errorf("%s rejected: %v", raw, err)
		}
	}
}

func TestValidateURL_RejectsNonHTTPSchemes(t *testing.T) {
	var strict Policy
	for _, raw := range []string{
		"file:///etc/passwd",
		"gopher://example.com/",
		"ftp://example.com/x",
		"redis://example.com:6379",
		"/relative/path",
		"example.com/hook", // no scheme
	} {
		if err := strict.ValidateURL(raw); err == nil {
			t.Errorf("%s accepted, want rejected", raw)
		}
	}
}

// TestAllowPrivate_IsScopedToPrivateNetworks: the opt-in exists for the
// operator running the receiver on their own host. It must not become a
// blanket "allow everything" that hands over cloud metadata.
func TestAllowPrivate_IsScopedToPrivateNetworks(t *testing.T) {
	relaxed := Policy{AllowPrivate: true}

	for _, raw := range []string{
		"http://127.0.0.1:9000/hook",
		"http://localhost:9000/hook",
		"http://10.1.2.3/hook",
		"http://192.168.0.4/hook",
		"http://[::1]:9000/hook",
	} {
		if err := relaxed.ValidateURL(raw); err != nil {
			t.Errorf("AllowPrivate rejected %s: %v", raw, err)
		}
	}

	for _, raw := range []string{
		"http://169.254.169.254/latest/meta-data/",
		"http://[fe80::1]/hook",
		"http://0.0.0.0/hook",
		"http://255.255.255.255/hook",
	} {
		if err := relaxed.ValidateURL(raw); err == nil {
			t.Errorf("AllowPrivate accepted %s; link-local and unroutable stay blocked", raw)
		}
	}
}

func TestControl_BlocksResolvedAddresses(t *testing.T) {
	var strict Policy
	for _, addr := range []string{
		"127.0.0.1:80", "[::1]:80", "10.0.0.1:443", "169.254.169.254:80",
		"[::ffff:169.254.169.254]:80", "100.100.100.100:80",
	} {
		if err := strict.Control("tcp", addr, nil); err == nil {
			t.Errorf("Control allowed %s", addr)
		}
	}
	if err := strict.Control("tcp", "93.184.216.34:443", nil); err != nil {
		t.Errorf("Control blocked a public address: %v", err)
	}
	// Non-TCP networks have no business here.
	if err := strict.Control("udp", "93.184.216.34:53", nil); err == nil {
		t.Error("Control allowed a udp dial")
	}
}

// TestClient_BlocksDNSRebinding is the test that matters: a name that
// passes ValidateURL because it is a hostname, but resolves to
// loopback. Parse-time validation cannot catch this; the dial-time
// check must.
func TestClient_BlocksDNSRebinding(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	_, port, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))

	var strict Policy
	// The URL is an ordinary public-looking hostname and passes
	// parse-time validation.
	raw := "http://hooks.example.com:" + port + "/hook"
	if err := strict.ValidateURL(raw); err != nil {
		t.Fatalf("hostname URL rejected at parse time: %v", err)
	}

	// ...but resolves to 127.0.0.1. Rather than depend on a public
	// resolver, point the client's resolver at loopback directly.
	client := strict.Client(2 * time.Second)
	tr := client.Transport.(*http.Transport)
	base := tr.DialContext
	tr.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		// Stand in for a resolver that answers 127.0.0.1.
		_, p, _ := net.SplitHostPort(address)
		return base(ctx, network, net.JoinHostPort("127.0.0.1", p))
	}

	req, _ := http.NewRequest(http.MethodGet, raw, nil)
	resp, err := client.Do(req)
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("request to a loopback-resolving host succeeded; SSRF guard did not fire")
	}
	if !strings.Contains(err.Error(), "blocked by SSRF policy") {
		t.Errorf("error = %v, want the SSRF policy message", err)
	}
}

// TestClient_AllowsPrivateWhenOptedIn: the same request succeeds under
// the documented opt-in, so an operator forwarding to their own app on
// 127.0.0.1 is not locked out.
func TestClient_AllowsPrivateWhenOptedIn(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client := Policy{AllowPrivate: true}.Client(2 * time.Second)
	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatalf("opted-in private request failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d", resp.StatusCode)
	}
}

// TestClient_BlocksRedirectToBlockedAddress: a receiver Posthorn is
// allowed to reach must not be able to walk it somewhere it is not. The
// policy here permits the first hop (private, opted in) and the
// redirect points at the cloud metadata address, which no policy
// unblocks — so the guard has to fire on the SECOND dial, not the first.
func TestClient_BlocksRedirectToBlockedAddress(t *testing.T) {
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://169.254.169.254/latest/meta-data/", http.StatusFound)
	}))
	defer redirector.Close()

	client := Policy{AllowPrivate: true}.Client(2 * time.Second)
	resp, err := client.Get(redirector.URL)
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("metadata address reached through a redirect")
	}
	if !strings.Contains(err.Error(), "blocked by SSRF policy") {
		t.Errorf("error = %v, want the SSRF policy message", err)
	}
}

func TestClient_CapsRedirectChain(t *testing.T) {
	var hops int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hops++
		http.Redirect(w, r, "/again", http.StatusFound)
	}))
	defer srv.Close()

	client := Policy{AllowPrivate: true}.Client(2 * time.Second)
	resp, err := client.Get(srv.URL)
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("infinite redirect chain followed to completion")
	}
	if hops > 4 {
		t.Errorf("followed %d redirect hops, want the chain capped low", hops)
	}
}

func TestCheckAddr(t *testing.T) {
	var strict Policy
	if err := strict.CheckAddr(netip.MustParseAddr("169.254.169.254")); err == nil {
		t.Error("metadata address allowed")
	}
	if err := strict.CheckAddr(netip.MustParseAddr("93.184.216.34")); err != nil {
		t.Errorf("public address blocked: %v", err)
	}
}

func TestResolve_ReportsLiteralAndPolicy(t *testing.T) {
	var strict Policy
	addrs, err := strict.Resolve(context.Background(), "http://127.0.0.1/hook")
	if err == nil {
		t.Fatal("Resolve reported a loopback literal as permitted")
	}
	if len(addrs) != 1 || addrs[0].String() != "127.0.0.1" {
		t.Errorf("addrs = %v, want the literal reported back for the operator", addrs)
	}
}
