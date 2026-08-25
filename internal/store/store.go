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
// file is always created (or repaired) 0600, and its parent directory 0700
// whenever lion is the one creating it (see Open).
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
	// maxPages is the PRAGMA max_page_count ceiling SetMaxSize installed
	// (0 = none). Kept because SQLite reports hitting that ceiling and
	// running out of disk with the same result code, and only one of those
	// is a clean truncation — see asDatabaseFull.
	maxPages int64
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
// created 0700 when Open is the one creating it. A directory that already
// existed is left exactly as found: --store accepts an arbitrary path (a
// bare "--store /tmp/store.db" or a relative "--store ./store.db" both name
// a directory lion had no hand in), so re-permissioning it out from under
// whatever else uses it would be a surprising, unrelated side effect of
// opening a database. The database file itself is created, or repaired, to
// 0600 before SQLite ever touches it regardless of the directory's mode, so
// there is no window where a partially-initialized store is world- or
// group-readable.
func Open(path string) (*Store, error) {
	dir := filepath.Dir(path)
	if err := ensureStoreDir(dir); err != nil {
		return nil, err
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

// ensureStoreDir makes sure dir exists, creating it 0700 only when this call
// is the one that creates it — never chmodding a directory that was already
// there (see Open's doc comment for why: --store names an arbitrary path,
// not something lion is guaranteed to own).
func ensureStoreDir(dir string) error {
	if fi, err := os.Stat(dir); err == nil {
		if !fi.IsDir() {
			return fmt.Errorf("store: %s exists and is not a directory", dir)
		}
		// Refusing a directory others can write to is the only real defence
		// against the swap race: ensureFileMode necessarily closes its
		// descriptor before SQLite reopens the path by name, and no amount of
		// checking in that gap helps if another user can replace the entry
		// with a symlink mid-flight. Where nobody else can create entries,
		// there is no one to lose the race to. Checked, not warned, because a
		// warning doesn't stop the attack.
		if perm := fi.Mode().Perm(); perm&0o022 != 0 {
			return fmt.Errorf("store: %s is writable by other users (mode %04o); "+
				"the store holds your private messages and its path must not be "+
				"replaceable by someone else — use a private directory such as $LION_HOME", dir, perm)
		}
		return nil
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("store: stat %s: %w", dir, err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("store: create %s: %w", dir, err)
	}
	// os.MkdirAll's mode is subject to umask, so an explicit Chmod is what
	// actually guarantees 0700 — safe here because we just created dir
	// ourselves in the branch above.
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("store: chmod %s: %w", dir, err)
	}
	return nil
}

// ensureFileMode creates path (empty) with mode if it doesn't exist, or
// chmods it to mode if it does — so a store.db that predates a stricter
// lion version, or one created under a permissive umask, gets its
// permissions repaired rather than silently trusted.
// A symlink at the store path is refused rather than followed, and the mode
// is applied through the open descriptor instead of by name. Both a plain
// open and os.Chmod dereference a symlink, so a --store path in a shared
// directory could be pre-placed as a link to someone else's file and lion
// would open that file and re-permission it to 0600. The store is always a
// regular file lion manages, so a link there is never legitimate.
//
// The Lstat leaves a small window before the open; closing it entirely needs
// O_NOFOLLOW, which is unix-only (see internal/lockfile for the build-tagged
// version). Refusing the symlink and never chmod'ing by name removes the
// damaging half — lion can no longer be made to change another file's
// permissions.
func ensureFileMode(path string, mode os.FileMode) error {
	if fi, err := os.Lstat(path); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("store: %s is a symlink; refusing to open it as the store", path)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, mode)
	if err != nil {
		return err
	}
	if err := f.Chmod(mode); err != nil {
		f.Close()
		return err
	}
	return f.Close()
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
