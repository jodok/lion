package store

import (
	"errors"
	"fmt"

	sqlite "modernc.org/sqlite"
)

// ErrDatabaseFull is what WithTx returns when a transaction is rolled back
// because it would have grown the database past the ceiling SetMaxSize
// installed. It translates the sqlite driver's SQLITE_FULL result into a
// sentinel internal/cli can recognize with errors.Is, so cli never needs to
// import or type-assert the sqlite driver's own error type itself — this
// package is the one that owns SQL (see the package doc).
var ErrDatabaseFull = errors.New("store: database size limit reached")

// sqliteFullCode is SQLITE_FULL from sqlite3.h: the result code SQLite
// returns when a write would grow the database past PRAGMA max_page_count.
// That's exactly the ceiling SetMaxSize installs, so this is the one code
// asDatabaseFull needs to recognize.
const sqliteFullCode = 13

// asDatabaseFull reports err back unchanged unless it's the sqlite driver's
// SQLITE_FULL, in which case it returns ErrDatabaseFull instead. err may be
// a wrapped error (every caller in this package wraps driver errors with
// %w), which errors.As follows.
func asDatabaseFull(err error) error {
	var sqliteErr *sqlite.Error
	if errors.As(err, &sqliteErr) && sqliteErr.Code() == sqliteFullCode {
		return ErrDatabaseFull
	}
	return err
}

// SetMaxSize caps how large the store's main database file is allowed to
// grow by deriving PRAGMA max_page_count from maxBytes and the database's
// own page_size, so SQLite itself refuses — and rolls back, atomically —
// any transaction that would push the file past that many pages
// (surfaced through WithTx as ErrDatabaseFull).
//
// This exists because `lion sync --max-db-size` previously enforced its
// budget only by checking SizeBytes() before each page was fetched: a
// pre-check like that is necessarily advisory, since it can't know a
// not-yet-fetched page's own footprint in advance, so a single page larger
// than expected could still push the store past the advertised limit. The
// pragma is the backstop that makes the bound actually hard: the pre-check
// is what stops a sync cleanly and early in the common case, and this is
// what guarantees the promise holds even when it doesn't.
//
// maxBytes <= 0 means "no limit" and is a no-op, leaving SQLite's own
// default max_page_count (effectively unbounded for any store this command
// will ever produce) in place.
func (s *Store) SetMaxSize(maxBytes int64) error {
	if maxBytes <= 0 {
		return nil
	}
	var pageSize int64
	if err := s.db.QueryRow("PRAGMA page_size").Scan(&pageSize); err != nil {
		return fmt.Errorf("store: read page_size: %w", err)
	}
	if pageSize <= 0 {
		return fmt.Errorf("store: page_size reported as %d", pageSize)
	}
	maxPages := maxBytes / pageSize
	if maxPages < 1 {
		// A limit smaller than one page still has to mean "as tight a bound
		// as SQLite can enforce", not "no limit" — 0 is what means unlimited
		// to both PRAGMA max_page_count and this method's own maxBytes<=0
		// check above, so this can't be left at 0 without silently
		// reopening the no-limit case for a very small --max-db-size.
		maxPages = 1
	}
	// PRAGMA statements don't take bound parameters in SQLite's own
	// grammar; maxPages is derived entirely from validated int64 arithmetic
	// above, never from a raw caller string, so formatting it directly into
	// the statement carries no injection risk.
	if _, err := s.db.Exec(fmt.Sprintf("PRAGMA max_page_count = %d", maxPages)); err != nil {
		return fmt.Errorf("store: set max_page_count: %w", err)
	}
	return nil
}
