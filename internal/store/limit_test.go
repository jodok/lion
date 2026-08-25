package store

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// TestSetMaxSizeRejectsAndRollsBackOversizedTransaction is the required
// hard-bound regression test: --max-db-size's pre-checks (SizeBytes,
// checked before a page is fetched) are necessarily advisory, since they
// can't know a not-yet-fetched page's own footprint in advance. SetMaxSize
// derives PRAGMA max_page_count from the limit so SQLite itself refuses,
// and rolls back, any single transaction that would grow the database past
// it — the backstop that makes the promise hold even when a page turns out
// bigger than the pre-check anticipated.
func TestSetMaxSizeRejectsAndRollsBackOversizedTransaction(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	var pageSize int64
	if err := s.db.QueryRow("PRAGMA page_size").Scan(&pageSize); err != nil {
		t.Fatal(err)
	}
	// A handful of pages above whatever the freshly-migrated schema already
	// occupies: enough headroom that Open/migrate themselves never trip it,
	// but nowhere near enough for the bulk insert below.
	if err := s.SetMaxSize(20 * pageSize); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	longBody := strings.Repeat("x", 20000)
	err = s.WithTx(ctx, func(tx *Tx) error {
		if err := tx.UpsertConversation(ctx, Conversation{ID: "c1", URN: "urn:li:fs_conversation:c1", UpdatedAt: 1}, 1); err != nil {
			return err
		}
		msgs := make([]Message, 50)
		for i := range msgs {
			msgs[i] = Message{URN: fmt.Sprintf("m%d", i), ConversationID: "c1", SentAt: int64(i), Body: longBody}
		}
		_, err := tx.RecordMessagePage(ctx, "c1", msgs, 1)
		return err
	})
	if !errors.Is(err, ErrDatabaseFull) {
		t.Fatalf("err = %v, want ErrDatabaseFull", err)
	}

	// Rolled back whole, not partially applied: the conversation row the
	// same transaction inserted first must be gone too, not just the
	// messages that actually overflowed the limit.
	if _, ok, cErr := s.Conversation(ctx, "c1"); cErr != nil {
		t.Fatal(cErr)
	} else if ok {
		t.Error("conversation c1 present after a transaction that should have been rolled back whole")
	}
	msgs, err := s.Messages(ctx, MessageFilter{ConversationID: "c1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 0 {
		t.Errorf("messages for c1 = %d, want 0: nothing from the oversized transaction should have landed", len(msgs))
	}
}

// TestSetMaxSizeZeroIsNoLimit pins SetMaxSize's own contract for the
// "--max-db-size not set" case: maxBytes<=0 must leave the store able to
// grow normally, not accidentally install some tiny effective ceiling.
func TestSetMaxSizeZeroIsNoLimit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if err := s.SetMaxSize(0); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	longBody := strings.Repeat("x", 20000)
	err = s.WithTx(ctx, func(tx *Tx) error {
		if err := tx.UpsertConversation(ctx, Conversation{ID: "c1", URN: "urn:li:fs_conversation:c1", UpdatedAt: 1}, 1); err != nil {
			return err
		}
		msgs := make([]Message, 50)
		for i := range msgs {
			msgs[i] = Message{URN: fmt.Sprintf("m%d", i), ConversationID: "c1", SentAt: int64(i), Body: longBody}
		}
		_, err := tx.RecordMessagePage(ctx, "c1", msgs, 1)
		return err
	})
	if err != nil {
		t.Fatalf("WithTx with no size limit: %v", err)
	}
}

// TestDiskFullIsNotReportedAsTheSizeCap covers the ambiguity in SQLITE_FULL:
// SQLite returns result code 13 both for the max_page_count ceiling
// SetMaxSize installs and for a genuinely full filesystem. Only the first is
// a clean truncation. Mapping the second onto ErrDatabaseFull would let sync
// suppress a storage failure as an ordinary early stop — exiting successfully
// with a stale archive — even when --max-db-size was never passed.
//
// A real disk-full can't be produced in a unit test, so this stands in the
// equivalent position: a genuine driver SQLITE_FULL arising from a ceiling
// this package did not install (SetMaxSize is never called, so maxPages
// stays 0).
func TestDiskFullIsNotReportedAsTheSizeCap(t *testing.T) {
	s := newTestStore(t)
	if s.maxPages != 0 {
		t.Fatalf("precondition: maxPages = %d, want 0 (SetMaxSize not called)", s.maxPages)
	}
	// A ceiling imposed by something other than SetMaxSize, standing in for
	// the filesystem running out of room.
	if _, err := s.db.Exec("PRAGMA max_page_count = 1"); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	err := s.WithTx(ctx, func(tx *Tx) error {
		c := Conversation{ID: "2-full", URN: "urn:li:fs_conversation:2-full", UpdatedAt: 1}
		if err := tx.UpsertConversation(ctx, c, 1); err != nil {
			return err
		}
		big := strings.Repeat("x", 4096)
		msgs := make([]Message, 0, 500)
		for i := 0; i < 500; i++ {
			msgs = append(msgs, Message{
				URN: fmt.Sprintf("m%d", i), ConversationID: "2-full", SentAt: int64(i), Body: big,
			})
		}
		_, err := tx.RecordMessagePage(ctx, "2-full", msgs, 1)
		return err
	})
	if err == nil {
		t.Fatal("expected the write to fail against a 1-page ceiling")
	}
	if errors.Is(err, ErrDatabaseFull) {
		t.Errorf("a SQLITE_FULL we did not cause was reported as the --max-db-size cap: %v", err)
	}
}

// TestSetMaxSizeSplitsBudgetBetweenMainFileAndWAL is the required regression
// for the WAL sidecars: --max-db-size's user-facing definition of size is
// SizeBytes() (main file plus WAL/shm, see SizeBytes), but PRAGMA
// max_page_count only ever bounded the main file's logical pages, leaving
// the WAL free to carry the total well past the advertised bound. SetMaxSize
// must reserve part of maxBytes for journal_size_limit too, so the pragmas
// read back the derived split, and SizeBytes() after a rejection stays close
// to maxBytes rather than drifting past it by however large the WAL grew.
func TestSetMaxSizeSplitsBudgetBetweenMainFileAndWAL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	var pageSize int64
	if err := s.db.QueryRow("PRAGMA page_size").Scan(&pageSize); err != nil {
		t.Fatal(err)
	}
	maxBytes := 400 * pageSize
	if err := s.SetMaxSize(maxBytes); err != nil {
		t.Fatal(err)
	}

	wantWALBudget := maxBytes / 4
	wantMainBudget := maxBytes - wantWALBudget
	wantMaxPages := wantMainBudget / pageSize

	var gotJournalLimit, gotMaxPages int64
	if err := s.db.QueryRow("PRAGMA journal_size_limit").Scan(&gotJournalLimit); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow("PRAGMA max_page_count").Scan(&gotMaxPages); err != nil {
		t.Fatal(err)
	}
	if gotJournalLimit != wantWALBudget {
		t.Errorf("journal_size_limit = %d, want %d (maxBytes/4)", gotJournalLimit, wantWALBudget)
	}
	if gotMaxPages != wantMaxPages {
		t.Errorf("max_page_count = %d, want %d (the remaining 3/4 of maxBytes, in pages)", gotMaxPages, wantMaxPages)
	}

	// Fill past the cap and confirm SizeBytes (main file + sidecars) still
	// lands close to maxBytes, not wherever the WAL happened to grow to
	// before the page ceiling caught the write.
	ctx := context.Background()
	longBody := strings.Repeat("x", 20000)
	err = s.WithTx(ctx, func(tx *Tx) error {
		if err := tx.UpsertConversation(ctx, Conversation{ID: "c1", URN: "urn:li:fs_conversation:c1", UpdatedAt: 1}, 1); err != nil {
			return err
		}
		msgs := make([]Message, 200)
		for i := range msgs {
			msgs[i] = Message{URN: fmt.Sprintf("m%d", i), ConversationID: "c1", SentAt: int64(i), Body: longBody}
		}
		_, err := tx.RecordMessagePage(ctx, "c1", msgs, 1)
		return err
	})
	if !errors.Is(err, ErrDatabaseFull) {
		t.Fatalf("err = %v, want ErrDatabaseFull", err)
	}

	size, err := s.SizeBytes()
	if err != nil {
		t.Fatal(err)
	}
	// A single in-flight transaction can transiently push things a bit past
	// budget before the rejection rolls it back and the next checkpoint
	// truncates the WAL (see SetMaxSize's doc comment on what remains
	// soft) — a few pages of slack over maxBytes, not an unbounded amount.
	if slack := size - maxBytes; slack > 8*pageSize {
		t.Errorf("SizeBytes() after rejection = %d, maxBytes = %d (slack %d > 8 pages)", size, maxBytes, slack)
	}
}
