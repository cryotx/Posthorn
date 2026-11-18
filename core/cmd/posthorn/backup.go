package main

import (
	"flag"
	"fmt"
	"path/filepath"
	"time"

	"github.com/craigmccaskill/posthorn/config"
	"github.com/craigmccaskill/posthorn/storage"
)

// `posthorn backup` and `posthorn restore` (FR150, FR151).
//
// The operator-facing promise is narrow and worth stating: taking a
// backup does not require stopping Posthorn, and does not require
// knowing that SQLite keeps recent writes in a separate -wal file. Both
// of those are how backups of this shape usually go wrong — a `cp` of
// the .db taken at 3am that silently omits the last hour of mail.

func runBackup(args []string) error {
	fs := flag.NewFlagSet("backup", flag.ContinueOnError)
	configPath := fs.String("config", defaultConfigPath, "path to TOML config file")
	out := fs.String("out", "", "backup file to write (default: <db>.backup-<timestamp>)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	if cfg.Storage == nil {
		return fmt.Errorf("no [storage] block in %s — there is no database to back up", *configPath)
	}
	if cfg.Storage.InMemory || cfg.Storage.Path == ":memory:" {
		return fmt.Errorf("storage is in-memory — there is nothing on disk to back up")
	}

	target := *out
	if target == "" {
		target = cfg.Storage.Path + ".backup-" + time.Now().UTC().Format("20060102T150405Z")
	}

	// Opening the live database read-write is what allows VACUUM INTO
	// while the service runs. SQLite handles the concurrency; this
	// process is simply another connection.
	store, err := storage.Open(storage.Config{Path: cfg.Storage.Path})
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer func() { _ = store.Close() }()

	if err := store.BackupTo(target); err != nil {
		return err
	}
	info, err := storage.InspectFile(target)
	if err != nil {
		return err
	}
	fmt.Printf("backup written: %s\n", info.Path)
	fmt.Printf("  size:        %s\n", humanBytes(info.Bytes))
	fmt.Printf("  schema:      v%d\n", info.SchemaVersion)
	fmt.Printf("  submissions: %d\n", info.Submissions)
	fmt.Printf("  verified:    integrity_check ok\n")
	fmt.Printf("\nThe database snapshot needs no -wal or -shm alongside it.\n")
	fmt.Printf("Managed provider credentials also require the separate encryption key %s; back it up securely with this snapshot. It is not included in the database backup.\n", filepath.Join(filepath.Dir(cfg.Storage.Path), "provider-secrets.key"))
	return nil
}

func runRestore(args []string) error {
	fs := flag.NewFlagSet("restore", flag.ContinueOnError)
	configPath := fs.String("config", defaultConfigPath, "path to TOML config file")
	from := fs.String("from", "", "backup file to restore (required)")
	to := fs.String("to", "", "database path to write (default: the configured storage.path)")
	confirm := fs.Bool("yes", false, "proceed without the interactive confirmation")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *from == "" {
		return fmt.Errorf("--from is required")
	}

	target := *to
	if target == "" {
		cfg, err := config.Load(*configPath)
		if err != nil {
			return err
		}
		if cfg.Storage == nil {
			return fmt.Errorf("no [storage] block in %s — pass --to to name the target explicitly", *configPath)
		}
		target = cfg.Storage.Path
	}

	info, err := storage.InspectFile(*from)
	if err != nil {
		return err
	}

	// A restore over a database a running Posthorn holds open would
	// leave that process writing through a file descriptor to a
	// database nobody can see any more. There is no way to make that
	// safe, so the check is a hard stop rather than a warning.
	if running, detail := storage.InUse(target); running {
		return fmt.Errorf("%s is in use (%s) — stop Posthorn before restoring, "+
			"or the running process will keep writing to the database this replaces", target, detail)
	}

	fmt.Printf("restore %s → %s\n", *from, target)
	fmt.Printf("  size:        %s\n", humanBytes(info.Bytes))
	fmt.Printf("  schema:      v%d\n", info.SchemaVersion)
	fmt.Printf("  submissions: %d\n", info.Submissions)
	fmt.Printf("  taken:       %s\n", info.Taken.UTC().Format(time.RFC3339))
	fmt.Printf("  Provider credentials require the matching provider-secrets.key from the same backup, placed in %s with owner-only permissions. This command does not restore the key.\n", filepath.Dir(target))
	if !*confirm {
		return fmt.Errorf("this replaces the database at %s — re-run with --yes to proceed "+
			"(the existing file is kept aside, not deleted)", target)
	}

	superseded, err := storage.RestoreFile(*from, target)
	if err != nil {
		return err
	}
	fmt.Printf("\nrestored: %s (integrity_check ok)\n", target)
	if superseded != "" {
		fmt.Printf("the previous database was kept at: %s\n", superseded)
		fmt.Printf("delete it once the restore is confirmed good.\n")
	}
	return nil
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGT"[exp])
}

// defaultConfigPath is shared by every subcommand.
const defaultConfigPath = "/etc/posthorn/config.toml"

// backupDir is used by doctor to check that the storage directory is
// writable — a backup that cannot be written is a backup that does not
// exist.
func backupDir(dbPath string) string { return filepath.Dir(dbPath) }
