package storage

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var backupNow = time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)

// fileStore opens a real on-disk store, since backup is exactly about
// what is in the FILE.
func fileStore(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "posthorn.db")
	s, err := Open(Config{Path: path})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, path
}

// TestBackup_CapturesUncheckpointedWrites is the reason VACUUM INTO
// exists here: in WAL mode the most recent writes live in the -wal
// file, so a naive copy of the .db alone loses them silently.
func TestBackup_CapturesUncheckpointedWrites(t *testing.T) {
	s, path := fileStore(t)
	for i := 0; i < 5; i++ {
		seedMessage(t, s, "m-"+string(rune('1'+i)), "acme", backupNow, nil)
	}

	backup := filepath.Join(t.TempDir(), "snapshot.db")
	if err := s.BackupTo(backup); err != nil {
		t.Fatalf("BackupTo: %v", err)
	}

	info, err := InspectFile(backup)
	if err != nil {
		t.Fatalf("InspectFile: %v", err)
	}
	if info.Submissions != 5 {
		t.Errorf("backup holds %d submissions, want 5 — WAL contents were lost", info.Submissions)
	}
	if info.SchemaVersion != schemaVersion {
		t.Errorf("schema version = %d, want %d", info.SchemaVersion, schemaVersion)
	}

	// The snapshot stands alone: no -wal or -shm is required beside it.
	for _, sidecar := range []string{backup + "-wal", backup + "-shm"} {
		if _, err := os.Stat(sidecar); err == nil {
			t.Errorf("%s exists; the snapshot should be a single self-contained file", sidecar)
		}
	}
	_ = path
}

// The service keeps running: a backup must not require stopping mail.
func TestBackup_WorksOnALiveDatabase(t *testing.T) {
	s, _ := fileStore(t)
	seedMessage(t, s, "before", "acme", backupNow, nil)

	backup := filepath.Join(t.TempDir(), "live.db")
	if err := s.BackupTo(backup); err != nil {
		t.Fatalf("BackupTo on a live store: %v", err)
	}

	// Writes after the snapshot land in the running database, not in
	// the snapshot — a snapshot is a point in time, and saying so
	// beats pretending otherwise.
	seedMessage(t, s, "after", "acme", backupNow.Add(time.Minute), nil)
	info, _ := InspectFile(backup)
	if info.Submissions != 1 {
		t.Errorf("snapshot holds %d rows, want the 1 present when it was taken", info.Submissions)
	}
	if _, ok, _ := s.GetSubmission("after"); !ok {
		t.Error("the live database lost a write taken after the backup")
	}
}

// A backup command one bad argument away from destroying the only good
// copy is not a backup command.
func TestBackup_RefusesToOverwrite(t *testing.T) {
	s, _ := fileStore(t)
	backup := filepath.Join(t.TempDir(), "snapshot.db")
	if err := s.BackupTo(backup); err != nil {
		t.Fatal(err)
	}
	err := s.BackupTo(backup)
	if err == nil {
		t.Fatal("a second backup overwrote the first")
	}
	if !strings.Contains(err.Error(), "refusing to overwrite") {
		t.Errorf("error = %v", err)
	}
}

func TestVerifyFile_RejectsGarbageAndForeignFiles(t *testing.T) {
	dir := t.TempDir()

	garbage := filepath.Join(dir, "garbage.db")
	if err := os.WriteFile(garbage, []byte("this is not a database"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := VerifyFile(garbage); err == nil {
		t.Error("a non-database file verified")
	}

	if err := VerifyFile(filepath.Join(dir, "missing.db")); err == nil {
		t.Error("a missing file verified")
	}
}

// A backup written by a NEWER Posthorn must not be restored by an older
// binary: it would be opened, migrated-downward-by-accident, or simply
// misread.
func TestVerifyFile_RejectsNewerSchema(t *testing.T) {
	s, path := fileStore(t)
	if _, err := s.db.Exec("PRAGMA user_version = 999"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	err := VerifyFile(path)
	if err == nil {
		t.Fatal("a future-schema database verified")
	}
	if !strings.Contains(err.Error(), "newer than this binary") {
		t.Errorf("error = %v", err)
	}
}

// --- restore -------------------------------------------------------------

func TestRestore_ReplacesTargetAndKeepsTheOldOne(t *testing.T) {
	source, _ := fileStore(t)
	seedMessage(t, source, "restored-1", "acme", backupNow, nil)
	backup := filepath.Join(t.TempDir(), "snapshot.db")
	if err := source.BackupTo(backup); err != nil {
		t.Fatal(err)
	}

	// A target with DIFFERENT contents, to prove the restore replaced it.
	targetStore, targetPath := fileStore(t)
	seedMessage(t, targetStore, "will-be-superseded", "acme", backupNow, nil)
	if err := targetStore.Close(); err != nil {
		t.Fatal(err)
	}

	superseded, err := RestoreFile(backup, targetPath)
	if err != nil {
		t.Fatalf("RestoreFile: %v", err)
	}
	if superseded == "" {
		t.Fatal("the previous database was deleted rather than kept aside")
	}
	if _, err := os.Stat(superseded); err != nil {
		t.Errorf("superseded database missing: %v", err)
	}

	restored, err := Open(Config{Path: targetPath})
	if err != nil {
		t.Fatalf("open restored: %v", err)
	}
	defer func() { _ = restored.Close() }()
	if _, ok, _ := restored.GetSubmission("restored-1"); !ok {
		t.Error("the restored database is missing the backup's content")
	}
	if _, ok, _ := restored.GetSubmission("will-be-superseded"); ok {
		t.Error("the restored database still holds the superseded content")
	}
}

// A stale -wal beside the target belongs to the database that was moved
// aside; applied on top of the restored file it would corrupt it.
func TestRestore_ClearsStaleSidecars(t *testing.T) {
	source, _ := fileStore(t)
	seedMessage(t, source, "m-1", "acme", backupNow, nil)
	backup := filepath.Join(t.TempDir(), "snapshot.db")
	if err := source.BackupTo(backup); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	target := filepath.Join(dir, "posthorn.db")
	for _, name := range []string{target, target + "-wal", target + "-shm"} {
		if err := os.WriteFile(name, []byte("stale"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := RestoreFile(backup, target); err != nil {
		t.Fatalf("RestoreFile: %v", err)
	}
	for _, sidecar := range []string{target + "-wal", target + "-shm"} {
		if _, err := os.Stat(sidecar); err == nil {
			t.Errorf("%s survived the restore", sidecar)
		}
	}
}

func TestRestore_RefusesAnUnverifiableSource(t *testing.T) {
	dir := t.TempDir()
	garbage := filepath.Join(dir, "garbage.db")
	if err := os.WriteFile(garbage, []byte("not a database"), 0o600); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "posthorn.db")
	if err := os.WriteFile(target, []byte("the good database"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := RestoreFile(garbage, target); err == nil {
		t.Fatal("a corrupt backup was restored")
	}
	// And critically: the target was not touched.
	content, err := os.ReadFile(target)
	if err != nil || string(content) != "the good database" {
		t.Error("a failed restore damaged the existing database")
	}
}

// The restored file must not become world-readable because a restore
// ran with a permissive umask.
func TestRestore_TightPermissions(t *testing.T) {
	source, _ := fileStore(t)
	backup := filepath.Join(t.TempDir(), "snapshot.db")
	if err := source.BackupTo(backup); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "posthorn.db")
	if _, err := RestoreFile(backup, target); err != nil {
		t.Fatal(err)
	}
	stat, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if perm := stat.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("restored database mode = %o, want owner-only", perm)
	}
}

// --- in-use detection ----------------------------------------------------

// TestInUse_StaleSidecarIsNotInUse is the case the previous heuristic
// got wrong. SQLite can leave a -shm behind after a clean shutdown, so
// "is there a -shm?" reports a STOPPED database as running and blocks a
// restore at exactly the moment an operator needs one.
func TestInUse_StaleSidecarIsNotInUse(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "posthorn.db")
	s, err := Open(Config{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	seedMessage(t, s, "m-1", "acme", backupNow, nil)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// Leave the sidecars behind, as a real shutdown sometimes does.
	for _, sidecar := range []string{path + "-shm", path + "-wal"} {
		if err := os.WriteFile(sidecar, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	if inUse, detail := InUse(path); inUse {
		t.Errorf("a closed database with stale sidecars reported in use: %s", detail)
	}
}

// TestInUse_OpenDatabaseIsInUse: the check has to actually detect the
// case it exists for, or it is worse than nothing.
func TestInUse_OpenDatabaseIsInUse(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "posthorn.db")
	s, err := Open(Config{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()

	// Hold a write transaction, the way a live Posthorn does while it
	// records a submission.
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`INSERT INTO canary (id, at) VALUES (1, 1) ON CONFLICT(id) DO UPDATE SET at = 2`); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()

	if inUse, _ := InUse(path); !inUse {
		t.Error("a database with a live write transaction reported as free")
	}
}

func TestInUse_MissingFileIsNotInUse(t *testing.T) {
	if inUse, _ := InUse(filepath.Join(t.TempDir(), "absent.db")); inUse {
		t.Error("a nonexistent database reported in use")
	}
}

// The check must change nothing: it runs against a database an operator
// is about to restore over, and a probe that wrote would be a probe
// that could corrupt.
func TestInUse_ChangesNothing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "posthorn.db")
	s, err := Open(Config{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	seedMessage(t, s, "m-1", "acme", backupNow, nil)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	before, err := InspectFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if inUse, _ := InUse(path); inUse {
		t.Fatal("closed database reported in use")
	}
	after, err := InspectFile(path)
	if err != nil {
		t.Fatalf("database unreadable after the in-use check: %v", err)
	}
	if before.Submissions != after.Submissions || before.SchemaVersion != after.SchemaVersion {
		t.Errorf("the in-use check changed the database: %+v → %+v", before, after)
	}
}
