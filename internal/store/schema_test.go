package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

// TestMigrateV1StoreGainsSyncTokenColumn covers the upgrade path: a store
// created by an older lion must gain the column without losing its rows.
// Dropping and recreating would discard a synced archive that can take a long
// time to rebuild under lion's deliberately conservative rate limiting.
func TestMigrateV1StoreGainsSyncTokenColumn(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "store.db")

	// Build a v1 store by hand, exactly as an older lion would have left it.
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range schemaV1 {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("v1 schema: %v", err)
		}
	}
	if _, err := db.Exec(`INSERT INTO conversations
		(id, urn, participants, updated_at, unread, first_seen_at, last_synced_at)
		VALUES ('c1', 'urn:c1', '[]', 5000, 0, 1, 1)`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	st, err := Open(path)
	if err != nil {
		t.Fatalf("opening a v1 store: %v", err)
	}
	defer st.Close()

	conv, ok, err := st.Conversation(context.Background(), "c1")
	if err != nil {
		t.Fatalf("reading a migrated conversation: %v", err)
	}
	if !ok {
		t.Fatal("the v1 conversation did not survive migration")
	}
	if conv.MessagesSyncToken != "" {
		t.Errorf("MessagesSyncToken = %q on a migrated row, want empty (no resume point yet)", conv.MessagesSyncToken)
	}
	if conv.UpdatedAt != 5000 {
		t.Errorf("UpdatedAt = %d, want the stored 5000", conv.UpdatedAt)
	}
}

// TestSyncTokenRoundTrip covers both halves of the resume point: the
// per-conversation token and the store-wide meta key.
func TestSyncTokenRoundTrip(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	if err := st.WithTx(ctx, func(tx *Tx) error {
		if err := tx.UpsertConversation(ctx, Conversation{ID: "c1", URN: "urn:c1", UpdatedAt: 1}, 1); err != nil {
			return err
		}
		if err := tx.SetMessagesSyncToken(ctx, "c1", "tok-1"); err != nil {
			return err
		}
		return tx.SetMeta(ctx, "sync_token:conversations", "mailbox-tok")
	}); err != nil {
		t.Fatal(err)
	}

	conv, _, err := st.Conversation(ctx, "c1")
	if err != nil {
		t.Fatal(err)
	}
	if conv.MessagesSyncToken != "tok-1" {
		t.Errorf("MessagesSyncToken = %q, want tok-1", conv.MessagesSyncToken)
	}
	v, ok, err := st.Meta(ctx, "sync_token:conversations")
	if err != nil || !ok || v != "mailbox-tok" {
		t.Errorf("Meta = (%q, %v, %v), want (mailbox-tok, true, nil)", v, ok, err)
	}
	if _, ok, err := st.Meta(ctx, "never-set"); err != nil || ok {
		t.Errorf("Meta(absent) = (_, %v, %v), want (false, nil)", ok, err)
	}

	// An upsert must not wipe the resume point: sync re-upserts every
	// conversation it sees on every run.
	if err := st.WithTx(ctx, func(tx *Tx) error {
		return tx.UpsertConversation(ctx, Conversation{ID: "c1", URN: "urn:c1", UpdatedAt: 2}, 1)
	}); err != nil {
		t.Fatal(err)
	}
	conv, _, err = st.Conversation(ctx, "c1")
	if err != nil {
		t.Fatal(err)
	}
	if conv.MessagesSyncToken != "tok-1" {
		t.Errorf("MessagesSyncToken = %q after a routine upsert, want it preserved", conv.MessagesSyncToken)
	}

	// Clearing is how a caller says the stream must start over.
	if err := st.WithTx(ctx, func(tx *Tx) error {
		return tx.SetMessagesSyncToken(ctx, "c1", "")
	}); err != nil {
		t.Fatal(err)
	}
	conv, _, err = st.Conversation(ctx, "c1")
	if err != nil {
		t.Fatal(err)
	}
	if conv.MessagesSyncToken != "" {
		t.Errorf("MessagesSyncToken = %q after clearing, want empty", conv.MessagesSyncToken)
	}
}
