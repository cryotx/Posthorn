package transport

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/textproto"
	"testing"
	"time"
)

func TestSMTPOut_StalledServerHonorsContext(t *testing.T) {
	for _, implicit := range []bool{false, true} {
		shape := "plaintext"
		if implicit {
			shape = "implicit_tls"
		}
		for _, stage := range []string{"greeting", "ehlo"} {
			for _, mode := range []string{"deadline", "cancel"} {
				t.Run(shape+"/"+stage+"/"+mode, func(t *testing.T) {
					listener, err := net.Listen("tcp", "127.0.0.1:0")
					if err != nil {
						t.Fatal(err)
					}
					if implicit {
						listener = tls.NewListener(listener, &tls.Config{
							Certificates: []tls.Certificate{selfSignedCert(t)},
						})
					}
					t.Cleanup(func() { _ = listener.Close() })
					release := make(chan struct{})
					t.Cleanup(func() { close(release) })
					stalled := make(chan error, 1)
					go func() {
						conn, err := listener.Accept()
						if err != nil {
							stalled <- err
							return
						}
						defer func() { _ = conn.Close() }()
						_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
						if tlsConn, ok := conn.(*tls.Conn); ok {
							if err := tlsConn.Handshake(); err != nil {
								stalled <- err
								return
							}
						}
						if stage == "ehlo" {
							wire := textproto.NewConn(conn)
							if err := wire.PrintfLine("220 local SMTP test"); err != nil {
								stalled <- err
								return
							}
							if _, err := wire.ReadLine(); err != nil {
								stalled <- err
								return
							}
						}
						stalled <- nil
						<-release // leave the greeting or EHLO unanswered
					}()

					host, port, err := net.SplitHostPort(listener.Addr().String())
					if err != nil {
						t.Fatal(err)
					}
					addr, err := net.ResolveTCPAddr("tcp", net.JoinHostPort(host, port))
					if err != nil {
						t.Fatal(err)
					}
					sender := NewSMTPOutTransport(host, addr.Port, "", "", false)
					sender.ImplicitTLS = implicit
					sender.TLSInsecureSkipVerify = true
					ctx, cancel := context.WithCancel(context.Background())
					if mode == "deadline" {
						cancel()
						ctx, cancel = context.WithTimeout(context.Background(), 300*time.Millisecond)
					}
					defer cancel()
					done := make(chan error, 1)
					go func() {
						_, err := sender.Send(ctx, goodSMTPMessage())
						done <- err
					}()
					select {
					case err := <-stalled:
						if err != nil {
							t.Fatalf("test server: %v", err)
						}
					case <-time.After(2 * time.Second):
						t.Fatal("SMTP did not reach the intended stall")
					}
					if mode == "cancel" {
						cancel()
					}
					select {
					case err := <-done:
						var transportErr *TransportError
						if !errors.As(err, &transportErr) || transportErr.Class != ErrTransient {
							t.Fatalf("stalled send error = %v, want transient", err)
						}
					case <-time.After(2 * time.Second):
						t.Fatal("SMTP send ignored context cancellation or deadline")
					}
				})
			}
		}
	}
}
