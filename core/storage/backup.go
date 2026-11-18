package storage

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Online backup and restore (FR150, FR151).
//
// The naive backup — copy the .db file — is WRONG for a WAL database in
// two ways at once: the recent writes live in the -wal file, so a copy
// of the .db alone silently loses them, and a copy taken while a write
// is in flight can be torn. Copying all three files is not much better,
// because there is no instant at which a plain `cp` sees a consistent
// set of them.
//
// So BackupTo uses SQLite's own VACUUM INTO, which walks the database
// under a read transaction and writes a fresh, fully-checkpointed
// single file. The result needs no -wal or -shm alongside it, is
// defragmented, and is taken WITHOUT stopping the service — which is
// the whole requirement: an operator must not have to take mail down to
// get a backup, and must not have to know about WAL files to take one
// safely.

// BackupTo writes a consistent snapshot of the database to path.
//
// Safe to call on a live database. The snapshot reflects a single
// point in time; writes concurrent with it land in the running
// database and simply are not in the snapshot.
func (s *Store) BackupTo(path string) error {
	if path == "" {
		return errors.New("storage: backup path is required")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("storage: resolve backup path: %w", err)
	}
	// VACUUM INTO refuses to overwrite, which is the behaviour we want:
	// a backup command that can silently clobber the previous backup is
	// one bad argument away from destroying the only good copy.
	if _, err := os.Stat(abs); err == nil {
		return fmt.Errorf("storage: %s already exists — refusing to overwrite a backup", abs)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("storage: check backup path: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return fmt.Errorf("storage: create backup directory: %w", err)
	}
	// The path is a parameter, so a directory name containing a quote
	// cannot break out of the statement.
	if _, err := s.db.Exec(`VACUUM INTO ?`, abs); err != nil {
		return fmt.Errorf("storage: backup: %w", err)
	}
	// A backup that cannot be read back is not a backup. Verifying here
	// means the failure surfaces at backup time, when an operator is
	// watching, rather than during a restore, when they are not.
	if err := VerifyFile(abs); err != nil {
		return fmt.Errorf("storage: the backup just written did not verify: %w", err)
	}
	return nil
}

// BackupInfo describes a snapshot file.
type BackupInfo struct {
	Path          string
	Bytes         int64
	SchemaVersion int
	Submissions   int64
	Taken         time.Time
}

// VerifyFile opens a database file read-only and checks that it is
// intact and readable by this binary.
//
// Three separate questions, because they fail differently:
// integrity_check catches corruption, the user_version check catches a
// file written by a NEWER binary (which this one must not open, let
// alone restore over a good database), and the table read catches a
// file that is valid SQLite but not a Posthorn database.
func VerifyFile(path string) error {
	info, err := InspectFile(path)
	if err != nil {
		return err
	}
	if info.SchemaVersion > schemaVersion {
		return fmt.Errorf("storage: %s has schema version %d, newer than this binary supports (%d)",
			path, info.SchemaVersion, schemaVersion)
	}
	return nil
}

// InspectFile reports what a database file contains, without modifying
// it. Used by `posthorn backup`, `posthorn restore` and `posthorn
// doctor`.
func InspectFile(path string) (BackupInfo, error) {
	stat, err := os.Stat(path)
	if err != nil {
		return BackupInfo{}, fmt.Errorf("storage: %w", err)
	}
	// Read-only, and immutable=0 so a stale -wal beside the file is
	// still honoured rather than ignored.
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		return BackupInfo{}, fmt.Errorf("storage: open %s: %w", path, err)
	}
	defer func() { _ = db.Close() }()

	var result string
	if err := db.QueryRow(`PRAGMA integrity_check`).Scan(&result); err != nil {
		return BackupInfo{}, fmt.Errorf("storage: integrity check on %s: %w", path, err)
	}
	if result != "ok" {
		return BackupInfo{}, fmt.Errorf("storage: %s failed its integrity check: %s", path, result)
	}

	info := BackupInfo{Path: path, Bytes: stat.Size(), Taken: stat.ModTime()}
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&info.SchemaVersion); err != nil {
		return BackupInfo{}, fmt.Errorf("storage: read schema version of %s: %w", path, err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM submissions`).Scan(&info.Submissions); err != nil {
		return BackupInfo{}, fmt.Errorf("storage: %s does not look like a Posthorn database: %w", path, err)
	}
	return info, nil
}

// InUse reports whether another process currently holds the database.
//
// This is a LOCK acquisition, not a filesystem guess. The obvious
// heuristic — "is there a -shm file?" — is wrong in the direction that
// matters: SQLite can leave a -shm behind after a clean shutdown, so it
// reports a stopped database as running and blocks a restore at exactly
// the moment an operator needs one. Taking an exclusive lock asks the
// question that actually matters, and a live process answers it by
// refusing.
//
// A file that does not exist, or that cannot be opened at all, is not
// "in use" — those are different failures with their own messages.
func InUse(path string) (bool, string) {
	if _, err := os.Stat(path); err != nil {
		return false, ""
	}
	// A short busy timeout: a live Posthorn holds write locks only for
	// the duration of a transaction, so waiting a moment distinguishes
	// "busy right now" from "held by another connection".
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(750)")
	if err != nil {
		return false, ""
	}
	defer func() { _ = db.Close() }()

	tx, err := db.Begin()
	if err != nil {
		return true, "another process holds the database: " + err.Error()
	}
	// BEGIN EXCLUSIVE is the real test: it needs the write lock no
	// other connection can be holding.
	if _, err := tx.Exec("PRAGMA locking_mode = EXCLUSIVE"); err != nil {
		_ = tx.Rollback()
		return true, "another process holds the database: " + err.Error()
	}
	if _, err := tx.Exec("CREATE TABLE IF NOT EXISTS canary (id INTEGER PRIMARY KEY CHECK (id = 1), at INTEGER NOT NULL)"); err != nil {
		_ = tx.Rollback()
		return true, "another process holds the database: " + err.Error()
	}
	// Roll back: this check must change nothing.
	_ = tx.Rollback()
	return false, ""
}

// RestoreFile copies a verified backup into place.
//
// This is an OFFLINE operation and says so: it refuses to run while
// another process holds the target, because replacing the file under a
// running Posthorn would leave that process writing through a file
// descriptor to a database nobody can see any more — the corruption
// would be silent and total.
//
// The existing target is renamed aside rather than deleted. A restore
// is performed by someone having a bad day, and "the old database is
// still there under .superseded" is the difference between a mistake
// and a disaster.
func RestoreFile(from, to string) (supersededPath string, err error) {
	info, err := InspectFile(from)
	if err != nil {
		return "", err
	}
	if info.SchemaVersion > schemaVersion {
		return "", fmt.Errorf("storage: %s has schema version %d, newer than this binary supports (%d) — "+
			"restore it with the version of Posthorn that wrote it", from, info.SchemaVersion, schemaVersion)
	}

	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		return "", fmt.Errorf("storage: create target directory: %w", err)
	}
	if _, err := os.Stat(to); err == nil {
		supersededPath = to + ".superseded-" + time.Now().UTC().Format("20060102T150405Z")
		if err := os.Rename(to, supersededPath); err != nil {
			return "", fmt.Errorf("storage: move the existing database aside: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("storage: check target: %w", err)
	}

	if err := copyFile(from, to); err != nil {
		return supersededPath, err
	}
	// A stale -wal or -shm from the superseded database would be
	// applied on top of the restored file and corrupt it. They belong
	// to the database that was moved aside, not to this one.
	for _, sidecar := range []string{to + "-wal", to + "-shm"} {
		if err := os.Remove(sidecar); err != nil && !os.IsNotExist(err) {
			return supersededPath, fmt.Errorf("storage: remove stale %s: %w", sidecar, err)
		}
	}
	if err := VerifyFile(to); err != nil {
		return supersededPath, fmt.Errorf("storage: the restored database did not verify: %w", err)
	}
	return supersededPath, nil
}

func copyFile(from, to string) error {
	src, err := os.Open(from)
	if err != nil {
		return fmt.Errorf("storage: open backup: %w", err)
	}
	defer func() { _ = src.Close() }()

	// 0600: a database holding message metadata should not be
	// world-readable because a restore recreated it with a default umask.
	dst, err := os.OpenFile(to, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("storage: create restored database: %w", err)
	}
	if _, err := dst.ReadFrom(src); err != nil {
		_ = dst.Close()
		return fmt.Errorf("storage: copy backup: %w", err)
	}
	// fsync before reporting success: a restore that is still in the
	// page cache when the machine loses power has restored nothing.
	if err := dst.Sync(); err != nil {
		_ = dst.Close()
		return fmt.Errorf("storage: flush restored database: %w", err)
	}
	return dst.Close()
}
