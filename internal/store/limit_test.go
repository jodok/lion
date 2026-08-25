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
