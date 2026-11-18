package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// doctorConfig writes a config whose every credential is a sentinel, so
// any leak into doctor's output is unmistakable.
const (
	sentinelProviderKey = "SENTINEL-PROVIDER-KEY-must-not-print"
	sentinelProjectKey  = "ph_live_SENTINEL_PROJECT_0123456789"
	sentinelSMTPPass    = "SENTINEL-SMTP-PASSWORD-0123456789"
	sentinelAdminToken  = "SENTINEL-ADMIN-TOKEN-0123456789"
)

func doctorConfig(t *testing.T, dbPath string) string {
	t.Helper()
	return `
[storage]
path = "` + dbPath + `"

[[providers]]
name = "postmark-prod"
type = "postmark"
[providers.settings]
api_key = "` + sentinelProviderKey + `"

[[projects]]
name = "acme"
api_key = "` + sentinelProjectKey + `"
allowed_from_domains = ["example.com"]
default_from = "noreply@example.com"
smtp_username = "acme"
smtp_password = "` + sentinelSMTPPass + `"

[[endpoints]]
path = "/api/contact"
to = ["ops@example.com"]
from = "noreply@example.com"
subject = "Contact"
body = "Body"
[endpoints.transport]
providers = ["postmark-prod"]
`
}

// capture runs fn with stdout redirected and returns what it printed.
func capture(t *testing.T, fn func() error) (string, error) {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	runErr := fn()
	_ = w.Close()
	os.Stdout = old

	var sb strings.Builder
	buf := make([]byte, 4096)
	for {
		n, rerr := r.Read(buf)
		sb.Write(buf[:n])
		if rerr != nil {
			break
		}
	}
	return sb.String(), runErr
}

// TestDoctor_NeverPrintsASecret is the requirement that matters most:
// doctor output is what operators paste into issues.
func TestDoctor_NeverPrintsASecret(t *testing.T) {
	dir := t.TempDir()
	path := writeConfig(t, doctorConfig(t, filepath.Join(dir, "posthorn.db")))
	t.Setenv("POSTHORN_ADMIN_TOKEN", sentinelAdminToken)

	out, err := capture(t, func() error { return runDoctor([]string{"--config", path}) })
	if err != nil {
		t.Fatalf("doctor failed on a valid config: %v\n%s", err, out)
	}
	for _, secret := range []string{
		sentinelProviderKey, sentinelProjectKey, sentinelSMTPPass, sentinelAdminToken,
	} {
		if strings.Contains(out, secret) {
			t.Errorf("doctor printed a credential:\n%s", out)
		}
	}
	// It should still say something useful about each of them.
	for _, want := range []string{"providers", "projects", "admin surface", "storage volume"} {
		if !strings.Contains(out, want) {
			t.Errorf("doctor output missing the %q check:\n%s", want, out)
		}
	}
}

// A doctor that reports "all good" on an unusable config is worse than
// no doctor.
func TestDoctor_FailsOnAnUnloadableConfig(t *testing.T) {
	path := writeConfig(t, "this is not valid TOML {{{")
	out, err := capture(t, func() error { return runDoctor([]string{"--config", path}) })
	if err == nil {
		t.Fatalf("doctor passed an unparseable config:\n%s", out)
	}
	if !strings.Contains(out, "FAIL") {
		t.Errorf("output did not mark a failure:\n%s", out)
	}
}

// A storage directory that cannot be written is a fail, not a warning:
// it means neither the database nor a backup can be written.
//
// The fixture makes the "directory" a regular FILE rather than using
// permission bits, because this process may be running as root — and
// root bypasses mode bits entirely, so a chmod-based fixture would
// silently test nothing.
func TestDoctor_FailsOnUnwritableStorageVolume(t *testing.T) {
	dir := t.TempDir()
	notADir := filepath.Join(dir, "notadir")
	if err := os.WriteFile(notADir, []byte("I am a file"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := writeConfig(t, doctorConfig(t, filepath.Join(notADir, "posthorn.db")))

	out, err := capture(t, func() error { return runDoctor([]string{"--config", path}) })
	if err == nil {
		t.Fatalf("doctor passed with an unwritable storage volume:\n%s", out)
	}
	if !strings.Contains(out, "storage volume") {
		t.Errorf("output did not name the volume check:\n%s", out)
	}
}

// The connectivity probe is opt-in and must not send mail.
func TestDoctor_ProviderProbeIsOptInAndNamed(t *testing.T) {
	dir := t.TempDir()
	path := writeConfig(t, doctorConfig(t, filepath.Join(dir, "posthorn.db")))

	plain, _ := capture(t, func() error { return runDoctor([]string{"--config", path}) })
	if strings.Contains(plain, "provider probe") {
		t.Error("the provider probe ran without --provider")
	}

	probed, err := capture(t, func() error {
		return runDoctor([]string{"--config", path, "--provider", "postmark-prod"})
	})
	if err != nil {
		t.Fatalf("probe failed: %v\n%s", err, probed)
	}
	if !strings.Contains(probed, "provider probe") {
		t.Errorf("--provider produced no probe result:\n%s", probed)
	}
	if !strings.Contains(probed, "live send is deliberately not attempted") {
		t.Errorf("the probe did not state that it sends nothing:\n%s", probed)
	}
	if strings.Contains(probed, sentinelProviderKey) {
		t.Error("the probe printed the provider credential")
	}

	unknown, err := capture(t, func() error {
		return runDoctor([]string{"--config", path, "--provider", "no-such-provider"})
	})
	if err == nil {
		t.Errorf("probing an unknown provider passed:\n%s", unknown)
	}
}
