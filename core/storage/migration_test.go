package storage

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Migration must fail CLOSED (FR151).
//
// The failure mode this guards against is the worst one available to a
// mail gateway: a binary that cannot understand its database starts
// anyway, accepts mail, and writes rows nobody can read back — or
// silently reinterprets columns whose meaning changed. Refusing to
// start is loud, recoverable, and costs one alert. Starting is not
// recoverable.

// TestMigration_RefusesANewerSchema: a database written by a NEWER
// Posthorn must not be opened by an older binary. The newer binary may
// have changed what a column MEANS, and there is no way to detect that
// from the outside.
func TestMigration_RefusesANewerSchema(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "posthorn.db")

	s, err := Open(Config{Path: path})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := s.db.Exec("PRAGMA user_version = 9999"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	_, err = Open(Config{Path: path})
	if err == nil {
		t.Fatal("a future-schema database was opened")
	}
	if !strings.Contains(err.Error(), "newer than this binary supports") {
		t.Errorf("error = %v, want it to name the version mismatch", err)
	}
	if !strings.Contains(err.Error(), "refusing to open") {
		t.Errorf("error = %v, want it to say it refused", err)
	}
}

// TestMigration_RunsForwardFromEveryVersion: a database stamped at an
// older version migrates all the way up, and lands exactly on the
// version this binary writes.
func TestMigration_RunsForwardFromEveryVersion(t *testing.T) {
	for version := 0; version < schemaVersion; version++ {
		t.Run("from_v"+itoa(version), func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "posthorn.db")

			// Build a current database, then stamp it back to an older
			// version so the migration path runs over real tables.
			s, err := Open(Config{Path: path})
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			seedMessage(t, s, "m-1", "acme", qNow, nil)
			if _, err := s.db.Exec("PRAGMA user_version = " + itoa(version)); err != nil {
				t.Fatal(err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}

			reopened, err := Open(Config{Path: path})
			if err != nil {
				t.Fatalf("migration from v%d failed: %v", version, err)
			}
			defer func() { _ = reopened.Close() }()

			var got int
			if err := reopened.db.QueryRow("PRAGMA user_version").Scan(&got); err != nil {
				t.Fatal(err)
			}
			if got != schemaVersion {
				t.Errorf("after migrating from v%d, version = %d, want %d", version, got, schemaVersion)
			}
			// The data survived.
			if _, ok, err := reopened.GetSubmission("m-1"); err != nil || !ok {
				t.Errorf("migration lost a row: ok=%v err=%v", ok, err)
			}
		})
	}
}

// TestMigration_IsIdempotent: re-running migrations over an
// already-current database must be a no-op, not an error. This is what
// a restart does, and a restart is not an event.
func TestMigration_IsIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "posthorn.db")
	for i := 0; i < 3; i++ {
		s, err := Open(Config{Path: path})
		if err != nil {
			t.Fatalf("open %d: %v", i, err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if err := VerifyFile(path); err != nil {
		t.Errorf("database did not verify after repeated opens: %v", err)
	}
}

// TestMigration_RefusesACorruptDatabase: a file that is not a valid
// database must stop startup, not produce a half-working process.
func TestMigration_RefusesACorruptDatabase(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "posthorn.db")
	// A plausible-looking SQLite header followed by garbage: enough to
	// get past a naive "is this a database" check.
	content := append([]byte("SQLite format 3\x00"), make([]byte, 4096)...)
	copy(content[100:], []byte("this is not a real b-tree page and never was"))
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}

	s, err := Open(Config{Path: path})
	if err == nil {
		_ = s.Close()
		t.Fatal("a corrupt database file was opened")
	}
}

// The canary is what proves the database is WRITABLE, not merely
// openable — a read-only mount opens fine and fails on the first
// submission, which is far too late to find out.
func TestMigration_WriteCanaryRunsAtOpen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "posthorn.db")
	s, err := Open(Config{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()

	var n int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM canary").Scan(&n); err != nil {
		t.Fatalf("canary table missing: %v", err)
	}
	if n == 0 {
		t.Error("no canary row written at open; a read-only mount would not be noticed until the first send")
	}
}
