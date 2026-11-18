package config_test

import (
	"strings"
	"testing"

	"github.com/craigmccaskill/posthorn/config"
)

// projectTOML builds a config with the given project key block.
func rotationTOML(keys string) string {
	return `
[[projects]]
name = "acme"
` + keys + `
allowed_from_domains = ["example.com"]

[[endpoints]]
path = "/api/contact"
to = ["craig@example.com"]
from = "noreply@example.com"
subject = "Contact"
body = "Body"

[endpoints.transport]
type = "postmark"

[endpoints.transport.settings]
api_key = "test-key"
`
}

// TestRotation_ConfigAcceptsAnOverlap is the acceptance test for FR141:
// two keys in the file, both live, no downtime while callers migrate.
func TestRotation_ConfigAcceptsAnOverlap(t *testing.T) {
	cfg, err := loadString(t, rotationTOML(`
api_key = "ph_live_old_0123456789abcdef0123"
api_keys = ["ph_live_new_0123456789abcdef0123"]`))
	if err != nil {
		t.Fatalf("rotation config rejected: %v", err)
	}
	if len(cfg.Projects) != 1 || len(cfg.Projects[0].APIKeys) != 1 {
		t.Fatalf("projects = %+v", cfg.Projects)
	}
}

func TestRotation_ConfigRejectsBadRotations(t *testing.T) {
	for _, tc := range []struct{ name, keys, wantIn string }{
		{
			name: "same_key_twice",
			keys: `
api_key = "ph_live_0123456789abcdef0123456789"
api_keys = ["ph_live_0123456789abcdef0123456789"]`,
			wantIn: "same value",
		},
		{
			name: "short_second_key",
			keys: `
api_key = "ph_live_0123456789abcdef0123456789"
api_keys = ["short"]`,
			wantIn: "at least",
		},
		{
			name:   "no_key_at_all",
			keys:   ``,
			wantIn: "api_key",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadString(t, rotationTOML(tc.keys))
			if err == nil {
				t.Fatal("accepted")
			}
			if !strings.Contains(err.Error(), tc.wantIn) {
				t.Errorf("error = %v, want it to mention %q", err, tc.wantIn)
			}
		})
	}
}

// A key must never appear in an error message, however the config is
// wrong: config errors are printed at startup and pasted into issues.
func TestRotation_ErrorsNeverEchoAKey(t *testing.T) {
	const sentinel = "ph_live_SENTINEL_0123456789abcdef"
	_, err := loadString(t, rotationTOML(`
api_key = "`+sentinel+`"
api_keys = ["`+sentinel+`"]`))
	if err == nil {
		t.Fatal("duplicate keys accepted")
	}
	if strings.Contains(err.Error(), sentinel) {
		t.Errorf("the config error echoed a key: %v", err)
	}
}

var _ = config.Config{}
