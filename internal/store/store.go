// Package store is lion's local message store: a SQLite database that
// `lion sync` populates and `lion message export` (and, later, search)
// reads from. It exists so lion follows the same architecture as wacli
// (https://wacli.sh) rather than doing one-shot API dumps: the network pass
// (sync) and the read pass (export) are separate, so export never touches
// LinkedIn and works offline, under any rate-limit state, and repeatably.
//
// This package never imports cobra and never writes to stdout/stderr — it
// is a plain data layer that internal/cli calls into, matching the same
// cli/voyager split DESIGN.md §2 already draws for the network client.
//
// The database holds a complete copy of someone's private messages, so the
// file is created 0600 and its parent directory 0700 (see Open).
package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/jodok/lion/internal/config"
	_ "modernc.org/sqlite" // registers the "sqlite" database/sql driver; pure Go, no CGO (DESIGN.md's no-CGO rule)
)

// dbFileName is the store's filename under the lion home directory.
const dbFileName = "store.db"

// Store is a handle on the local message database. Construct with Open.
type Store struct {
	db   *sql.DB
	path string
	// now is injectable so tests can pin first_seen_at/last_synced_at rather
	// than asserting against a moving clock, mirroring ratelimit.Limiter's
	// same pattern.
	now func() time.Time
}

// DefaultPath returns the default store location, $LION_HOME/store.db,
// creating the lion home directory (0700) if needed.
func DefaultPath() (string, error) {
	home, err := config.EnsureHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, dbFileName), nil
}

// Open opens (creating if needed) the SQLite store at path, applying
// pragmas and running any pending schema migration. The parent directory is
// created 0700 and repaired to 0700 if it already existed looser (mirroring
// config.EnsureHome, for the same reason: a 0600 file guarantee is only as
// good as the directory it lives in). The database file itself is created,
// or repaired, to 0600 before SQLite ever touches it, so there is no window
// where a partially-initialized store is world- or group-readable.
func Open(path string) (*Store, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("store: create %s: %w", dir, err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, fmt.Errorf("store: chmod %s: %w", dir, err)
	}
	if err := ensureFileMode(path, 0o600); err != nil {
		return nil, fmt.Errorf("store: prepare %s: %w", path, err)
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	// A pure-Go sqlite driver still multiplexes goroutines across several
	// database/sql connections by default; each is a distinct sqlite
	// connection contending for the same file lock. Capping the pool at one
	// connection avoids SQLITE_BUSY between goroutines *within this
	// process* — cross-process contention (two `lion sync` runs) is what
	// the separate store.db.lock (see Lock) exists to prevent instead.
	db.SetMaxOpenConns(1)

	for _, pragma := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA foreign_keys=ON",
		// A short busy_timeout absorbs the brief window where WAL
		// checkpointing or a reader briefly holds the file, rather than
		// surfacing a spurious SQLITE_BUSY from ordinary contention.
		"PRAGMA busy_timeout=5000",
	} {
		if _, err := db.Exec(pragma); err != nil {
			db.Close()
			return nil, fmt.Errorf("store: %s: %w", pragma, err)
		}
	}

	s := &Store{db: db, path: path, now: time.Now}
	if err := s.migrate(context.Background()); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: migrate: %w", err)
	}
	return s, nil
}

// ensureFileMode creates path (empty) with mode if it doesn't exist, or
// chmods it to mode if it does — so a store.db that predates a stricter
// lion version, or one created under a permissive umask, gets its
// permissions repaired rather than silently trusted.
func ensureFileMode(path string, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, mode)
	if err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Chmod(path, mode)
}

// Close releases the underlying database handle.
func (s *Store) Close() error {
	return s.db.Close()
}

// Path returns the store's on-disk file path.
func (s *Store) Path() string {
	return s.path
}

// LockPath returns the path of the advisory lock `lion sync` takes around a
// sync run (see Lock), so two concurrent syncs can't interleave writes.
// Readers (export, search) never take this lock — see Lock's doc comment.
func (s *Store) LockPath() string {
	return s.path + ".lock"
}
