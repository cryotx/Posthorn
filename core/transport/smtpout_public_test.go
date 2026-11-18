package transport

import (
	"context"
	"errors"
	"github.com/craigmccaskill/posthorn/netguard"
	"testing"
)

func TestManagedSMTPRejectsPrivateDestinations(t *testing.T) {
	s := NewSMTPOutTransport("localhost", 587, "u", "p", true)
	s.RestrictToPublicDestinations()
	if _, err := s.dial(context.Background(), "127.0.0.1:587"); !errors.Is(err, netguard.ErrBlocked) {
		t.Fatalf("STARTTLS private destination: %v", err)
	}
	if _, err := s.dialTLS(context.Background(), "127.0.0.1:465"); !errors.Is(err, netguard.ErrBlocked) {
		t.Fatalf("TLS private destination: %v", err)
	}
}
