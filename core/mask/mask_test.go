package mask

import "testing"

func TestSecret(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"", ""},
		{"short", "********"},
		{"0123456789a", "********"}, // 11 chars: still too short to show a tail
		{"0123456789ab", "********89ab"},
		{"ph_live_0123456789abcdef", "********cdef"},
		{"whsec_dGVzdC1zZWNyZXQ=", "********ZXQ="},
	} {
		if got := Secret(tc.in); got != tc.want {
			t.Errorf("Secret(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// The masked form must never contain enough of the secret to be worth
// having: at most four characters, and never the prefix that identifies
// the key type plus more.
func TestSecret_RevealsAtMostFourCharacters(t *testing.T) {
	const secret = "ph_live_SENSITIVE_MIDDLE_PART_abcd"
	got := Secret(secret)
	if len(got) > len("********")+4 {
		t.Errorf("Secret = %q, longer than mask+4", got)
	}
	for _, fragment := range []string{"SENSITIVE", "MIDDLE", "ph_live"} {
		if contains(got, fragment) {
			t.Errorf("Secret = %q, leaked %q", got, fragment)
		}
	}
}

func TestEmail(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"", ""},
		{"user@example.com", "u***@example.com"},
		{"u@example.com", "u***@example.com"},
		{"first.last+tag@mail.example.co.uk", "f***@mail.example.co.uk"},
		{"not-an-address", "***"},
		{"@example.com", "***"},
		{"user@", "***"},
	} {
		if got := Email(tc.in); got != tc.want {
			t.Errorf("Email(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestEmail_LocalPartIsNotRecoverable is the property that matters: an
// admin surface showing masked recipients must not be a way to read the
// address list.
func TestEmail_LocalPartIsNotRecoverable(t *testing.T) {
	const addr = "very.specific.person@example.com"
	got := Email(addr)
	for _, fragment := range []string{"very", "specific", "person", "."} {
		if fragment == "." {
			continue
		}
		if contains(got, fragment) {
			t.Errorf("Email = %q, leaked %q", got, fragment)
		}
	}
	if got != "v***@example.com" {
		t.Errorf("Email = %q", got)
	}
}

func TestEmails_PreservesOrderAndNil(t *testing.T) {
	if Emails(nil) != nil {
		t.Error("Emails(nil) should stay nil")
	}
	got := Emails([]string{"a@x.com", "b@y.com"})
	if len(got) != 2 || got[0] != "a***@x.com" || got[1] != "b***@y.com" {
		t.Errorf("Emails = %v", got)
	}
}

func contains(h, n string) bool {
	for i := 0; i+len(n) <= len(h); i++ {
		if h[i:i+len(n)] == n {
			return true
		}
	}
	return false
}
