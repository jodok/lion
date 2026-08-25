package store

import (
	"errors"
	"fmt"
	"path/filepath"

	sqlite "modernc.org/sqlite"
)

// ErrDatabaseFull is what WithTx returns when a transaction is rolled back
// because it would have grown the database past the ceiling SetMaxSize
// installed. It translates the sqlite driver's SQLITE_FULL result into a
// sentinel internal/cli can recognize with errors.Is, so cli never needs to
// import or type-assert the sqlite driver's own error type itself — this
// package is the one that owns SQL (see the package doc).
var ErrDatabaseFull = errors.New("store: database size limit reached")

// sqliteFullCode is SQLITE_FULL from sqlite3.h. SQLite returns it when a
// write would grow the database past PRAGMA max_page_count — the ceiling
// SetMaxSize installs — but it uses the same code for a genuinely full
// filesystem or exhausted temp storage (https://sqlite.org/rescode.html#full).
const sqliteFullCode = 13

// asDatabaseFull reports err back unchanged unless it is SQLITE_FULL *and*
// the ceiling this package installed is the demonstrable cause.
//
// The distinction matters because SQLite uses the same result code for two
// conditions that want opposite handling (sqlite.org/rescode.html#full):
// hitting the configured --max-db-size is a clean truncation that sync stops
// on and reports complete:false, while a genuinely full filesystem is a
// storage failure that must surface as an error — mapping it onto the size
// sentinel would let sync suppress it as an ordinary early stop, exiting
// successfully with a stale archive and no signal to automation, even when
// --max-db-size was never passed at all.
//
// Attribution can't come from PRAGMA page_count: by the time WithTx sees the
// error the transaction has rolled back, so the count reflects the
// pre-transaction size, below the ceiling. What can be compared instead is
// which constraint would actually have bound: capHeadroom is how many more
// bytes the failed transaction was allowed to write before hitting the
// --max-db-size ceiling (maxBytes minus the store's SizeBytes right now,
// which — since the transaction already rolled back — is exactly the
// pre-transaction footprint the failed write was working from). diskFree is
// how many bytes the filesystem itself had free. If the disk could have
// accommodated everything the cap would have permitted (diskFree >=
// capHeadroom), the cap is what actually stopped the write, so this is
// ErrDatabaseFull. Otherwise the disk had less room than the cap would have
// allowed, so the disk itself is the binding constraint and this is a
// genuine storage failure. Whenever either quantity can't be established —
// SizeBytes errors, or diskFree is unknown on this platform (see
// diskFreeBytes) — the honest outcome is the same: preserve the original
// error rather than claim the cap without being able to show it.
func (s *Store) asDatabaseFull(err error) error {
	var sqliteErr *sqlite.Error
	if !errors.As(err, &sqliteErr) || sqliteErr.Code() != sqliteFullCode {
		return err
	}
	if s.maxPages <= 0 {
		return err // no ceiling of ours to have hit
	}
	size, sizeErr := s.SizeBytes()
	if sizeErr != nil {
		return err // can't establish the pre-transaction footprint; don't guess
	}
	capHeadroom := s.maxBytes - size
	diskFree, ok := s.diskFree(filepath.Dir(s.path))
	if !ok {
		return err // disk free space unknown here; don't guess
	}
	if diskFree >= capHeadroom {
		return ErrDatabaseFull
	}
	return err // the disk had less room than the cap allowed: a real storage failure
}

// SetMaxSize caps how large the store's on-disk footprint (SizeBytes: the
// main database file plus its WAL/shm sidecars, see Open's WAL mode) is
// allowed to grow, deriving PRAGMA max_page_count and journal_size_limit
// from maxBytes so SQLite itself refuses — and rolls back, atomically — any
// transaction that would push the store past that footprint (surfaced
// through WithTx as ErrDatabaseFull).
//
// This exists because `lion sync --max-db-size` previously enforced its
// budget only by checking SizeBytes() before each page was fetched: a
// pre-check like that is necessarily advisory, since it can't know a
// not-yet-fetched page's own footprint in advance, so a single page larger
// than expected could still push the store past the advertised limit. The
// pragmas below are the backstop that makes the bound actually hard: the
// pre-check is what stops a sync cleanly and early in the common case, and
// this is what guarantees the promise holds even when it doesn't.
//
// maxBytes is split between the two mechanisms because they cover different
// files: max_page_count bounds only the main database file's logical page
// count, while the WAL grows separately between checkpoints and isn't
// subject to it at all. A quarter of the budget goes to journal_size_limit
// (which truncates the WAL back to at most that many bytes at
// commit/checkpoint boundaries — see also Store.Close's TRUNCATE
// checkpoint), and the rest becomes the main file's page ceiling. What
// remains soft: a single in-flight transaction can transiently push the WAL
// past its share before the next checkpoint truncates it back down, but
// WithTx scopes one fetched page per transaction, so that transient is
// bounded by one page's own footprint, not by anything unbounded. The
// pre-checks in sync (which read SizeBytes and therefore do count the
// sidecars) remain the mechanism that stops a sync early and cleanly in the
// common case; these pragmas are the backstop for when a single page turns
// out bigger than the pre-check anticipated, not a substitute for it.
//
// maxBytes <= 0 means "no limit" and is a no-op, leaving SQLite's own
// defaults (effectively unbounded for any store this command will ever
// produce) in place.
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

	walBudget := maxBytes / 4
	mainBudget := maxBytes - walBudget
	maxPages := mainBudget / pageSize
	if maxPages < 1 {
		// A limit smaller than one page still has to mean "as tight a bound
		// as SQLite can enforce", not "no limit" — 0 is what means unlimited
		// to both PRAGMA max_page_count and this method's own maxBytes<=0
		// check above, so this can't be left at 0 without silently
		// reopening the no-limit case for a very small --max-db-size.
		maxPages = 1
	}
	// PRAGMA statements don't take bound parameters in SQLite's own
	// grammar; maxPages and walBudget are derived entirely from validated
	// int64 arithmetic above, never from a raw caller string, so formatting
	// them directly into the statements carries no injection risk.
	if _, err := s.db.Exec(fmt.Sprintf("PRAGMA max_page_count = %d", maxPages)); err != nil {
		return fmt.Errorf("store: set max_page_count: %w", err)
	}
	if _, err := s.db.Exec(fmt.Sprintf("PRAGMA journal_size_limit = %d", walBudget)); err != nil {
		return fmt.Errorf("store: set journal_size_limit: %w", err)
	}
	// Remembered so asDatabaseFull can tell our ceiling apart from a full
	// disk, which SQLite reports with the same result code.
	s.maxPages = maxPages
	s.maxBytes = maxBytes
	return nil
}
