package transport

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The provider failure matrix (RC1 item 8).
//
// Every fault a real provider can present, mapped to the class the
// router keys its behaviour off. The point is not that each individual
// mapping is right in isolation — it is that the whole table matches
// what ADR-27 and ADR-28 promise, and that 401/403 in particular do
// NOT quietly fail over to a second provider.

// statusTransport is a Postmark client pointed at a server that returns
// a chosen status, so the real classification code runs.
func statusTransport(t *testing.T, status int, body string) (Transport, func()) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	return NewPostmarkTransport("k", srv.URL), srv.Close
}

func classOf(t *testing.T, err error) ErrorClass {
	t.Helper()
	var te *TransportError
	if !errors.As(err, &te) {
		t.Fatalf("error is %T, not a *TransportError — the Transport contract requires one: %v", err, err)
	}
	return te.Class
}

func TestFailureMatrix_HTTPStatusClassification(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   ErrorClass
	}{
		{"400 bad request", 400, `{"ErrorCode":300,"Message":"invalid"}`, ErrTerminal},
		{"401 unauthorized", 401, `{"ErrorCode":10,"Message":"bad token"}`, ErrTerminal},
		{"403 forbidden", 403, `{"ErrorCode":11,"Message":"forbidden"}`, ErrTerminal},
		{"422 invalid sender", 422, `{"ErrorCode":400,"Message":"sender signature not confirmed"}`, ErrTerminal},
		{"429 rate limited", 429, `{"ErrorCode":0,"Message":"slow down"}`, ErrRateLimited},
		{"500 server error", 500, `oops`, ErrTransient},
		{"502 bad gateway", 502, `oops`, ErrTransient},
		{"503 unavailable", 503, `oops`, ErrTransient},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tp, done := statusTransport(t, tc.status, tc.body)
			defer done()
			_, err := tp.Send(context.Background(), simpleMsg())
			if err == nil {
				t.Fatal("Send succeeded on an error status")
			}
			if got := classOf(t, err); got != tc.want {
				t.Errorf("class = %v, want %v", got, tc.want)
			}
		})
	}
}

// Network-level faults are all transient: nothing about them says the
// message is unacceptable.
func TestFailureMatrix_NetworkFaults(t *testing.T) {
	t.Run("connection refused", func(t *testing.T) {
		// Bind and immediately close to get a port nothing listens on.
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addr := ln.Addr().String()
		_ = ln.Close()

		tp := NewPostmarkTransport("k", "http://"+addr)
		_, serr := tp.Send(context.Background(), simpleMsg())
		if serr == nil {
			t.Fatal("Send succeeded against a closed port")
		}
		if got := classOf(t, serr); got != ErrTransient {
			t.Errorf("class = %v, want transient", got)
		}
	})

	t.Run("dns failure", func(t *testing.T) {
		tp := NewPostmarkTransport("k", "http://no-such-host.invalid")
		_, serr := tp.Send(context.Background(), simpleMsg())
		if serr == nil {
			t.Fatal("Send succeeded against an unresolvable host")
		}
		if got := classOf(t, serr); got != ErrTransient {
			t.Errorf("class = %v, want transient", got)
		}
	})

	t.Run("tls failure", func(t *testing.T) {
		// An HTTPS client against a plain-HTTP server: a handshake
		// failure, which is a connection problem, not a message one.
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
		defer srv.Close()
		https := strings.Replace(srv.URL, "http://", "https://", 1)

		tp := NewPostmarkTransport("k", https)
		_, serr := tp.Send(context.Background(), simpleMsg())
		if serr == nil {
			t.Fatal("Send succeeded across a broken TLS handshake")
		}
		if got := classOf(t, serr); got != ErrTransient {
			t.Errorf("class = %v, want transient", got)
		}
	})

	t.Run("timeout", func(t *testing.T) {
		// The handler blocks on a channel the test controls, not on the
		// request context: httptest.Server.Close waits for in-flight
		// handlers, and a handler waiting on a client-cancelled context
		// is not guaranteed to be released promptly.
		release := make(chan struct{})
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			<-release
		}))
		defer func() { close(release); srv.Close() }()
		tp := NewPostmarkTransport("k", srv.URL)
		ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
		defer cancel()
		_, serr := tp.Send(ctx, simpleMsg())
		if serr == nil {
			t.Fatal("Send succeeded against a hanging server")
		}
		if got := classOf(t, serr); got != ErrTransient {
			t.Errorf("class = %v, want transient", got)
		}
	})

	t.Run("tls handshake against a self-signed cert", func(t *testing.T) {
		srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
		defer srv.Close()
		tp := NewPostmarkTransport("k", srv.URL)
		_, serr := tp.Send(context.Background(), simpleMsg())
		if serr == nil {
			t.Fatal("Send trusted an untrusted certificate")
		}
		if got := classOf(t, serr); got != ErrTransient {
			t.Errorf("class = %v, want transient", got)
		}
		var ce *tls.CertificateVerificationError
		_ = ce // the class is what the router keys off; the cause is informational
	})
}

// --- routing behaviour under each class --------------------------------

// The headline invariant, restated as a matrix: an authentication or
// validation rejection must NOT be retried on another provider. Sending
// a rejected message to every provider in the pool spends N sender
// reputations on one bad submission.
func TestFailureMatrix_TerminalNeverFailsOver(t *testing.T) {
	for _, status := range []int{400, 401, 403, 422} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			primary, done := statusTransport(t, status, `{"ErrorCode":10,"Message":"no"}`)
			defer done()
			backup := succeeding("backup-1")

			r := mustRouter(t, "/x", mustRoute(t, nil, StrategyFailover,
				realProvider("primary", primary),
				testProvider("backup", backup)))

			_, err := r.Send(context.Background(), simpleMsg())
			if err == nil {
				t.Fatal("a terminal rejection was reported as success")
			}
			if len(backup.Sent) != 0 {
				t.Errorf("status %d failed over to a second provider", status)
			}
		})
	}
}

func TestFailureMatrix_TransientAndRateLimitedFailOver(t *testing.T) {
	for _, status := range []int{429, 500, 502, 503} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			primary, done := statusTransport(t, status, `{"ErrorCode":0,"Message":"later"}`)
			defer done()
			backup := succeeding("backup-1")

			r := mustRouter(t, "/x", mustRoute(t, nil, StrategyFailover,
				realProvider("primary", primary),
				testProvider("backup", backup)))

			res, err := r.Send(context.Background(), simpleMsg())
			if err != nil {
				t.Fatalf("status %d did not fail over: %v", status, err)
			}
			if res.Provider != "backup" {
				t.Errorf("delivered by %q, want backup", res.Provider)
			}
		})
	}
}

// A terminal rejection must not trip the breaker either: the provider
// is healthy, the message was not.
func TestFailureMatrix_TerminalDoesNotTripBreaker(t *testing.T) {
	tp, done := statusTransport(t, 422, `{"ErrorCode":400,"Message":"unverified sender"}`)
	defer done()

	breaker := NewBreaker(BreakerConfig{FailureThreshold: 2, Cooldown: time.Hour})
	p := &Provider{Name: "p", Weight: 1, Transport: tp, Breaker: breaker}
	r := mustRouter(t, "/x", mustRoute(t, nil, StrategyFailover, p))

	for i := 0; i < 10; i++ {
		_, _ = r.Send(context.Background(), simpleMsg())
	}
	if breaker.State() != CircuitClosed {
		t.Errorf("breaker = %v after 10 terminal rejections, want closed", breaker.State())
	}
}

// Transient failures DO trip it, which is the other half of the same
// promise.
func TestFailureMatrix_TransientTripsBreaker(t *testing.T) {
	tp, done := statusTransport(t, 503, `down`)
	defer done()

	breaker := NewBreaker(BreakerConfig{FailureThreshold: 3, Cooldown: time.Hour})
	p := &Provider{Name: "p", Weight: 1, Transport: tp, Breaker: breaker}
	r := mustRouter(t, "/x", mustRoute(t, nil, StrategyFailover, p))

	for i := 0; i < 5; i++ {
		_, _ = r.Send(context.Background(), simpleMsg())
	}
	if breaker.State() != CircuitOpen {
		t.Errorf("breaker = %v after 5 transient failures, want open", breaker.State())
	}
}

// A quota-blocked provider is skipped without any of that machinery
// engaging — the fourth row of the matrix.
func TestFailureMatrix_QuotaIsNotAHealthEvent(t *testing.T) {
	now := at(2026, 9, 8, 12)
	q := NewLocalQuota(nil, nil)
	breaker := NewBreaker(BreakerConfig{FailureThreshold: 1, Cooldown: time.Hour})
	spent := &Provider{
		Name: "spent", Weight: 1, Transport: succeeding("s"),
		Breaker: breaker, Quota: Quota{Daily: 1},
	}
	backup := succeeding("b")
	r := mustRouter(t, "/x", mustRoute(t, nil, StrategyFailover,
		spent, testProvider("backup", backup)))
	r.Quota = q
	r.Now = func() time.Time { return now }

	for i := 0; i < 5; i++ {
		if _, err := r.Send(context.Background(), simpleMsg()); err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
	}
	if breaker.State() != CircuitClosed {
		t.Errorf("breaker = %v, want closed — a spent budget is not ill health", breaker.State())
	}
	if len(backup.Sent) != 4 {
		t.Errorf("backup took %d, want 4 after the free provider's single unit", len(backup.Sent))
	}
}

// realProvider wraps a concrete transport (not the package's mock) in a
// pool entry, so the matrix exercises the actual classification code.
func realProvider(name string, tp Transport) *Provider {
	return &Provider{
		Name: name, Type: "postmark", Weight: 1, Transport: tp,
		Breaker: NewBreaker(BreakerConfig{FailureThreshold: 1 << 30, Cooldown: time.Hour}),
	}
}
