package store

import (
	"context"
	"testing"
)

// seedTwoConversations writes a small fixed dataset used by several tests
// below: two conversations with different senders and timestamps, one of
// which is marked BackfillDone.
func seedTwoConversations(t *testing.T, s *Store) {
	t.Helper()
	ctx := context.Background()

	if err := s.WithTx(ctx, func(tx *Tx) error {
		if err := tx.UpsertConversation(ctx, Conversation{
			ID: "c1", URN: "urn:c1",
			Participants: []Participant{{Name: "Ada Lovelace", URN: "urn:li:fs_miniProfile:ada"}},
			UpdatedAt:    500,
		}, 1000); err != nil {
			return err
		}
		if _, err := tx.RecordMessagePage(ctx, "c1", []Message{
			{URN: "m1", ConversationID: "c1", SenderName: "Ada Lovelace", SenderURN: "urn:li:fs_miniProfile:ada", SentAt: 100, Body: "let's grab coffee tomorrow"},
			{URN: "m2", ConversationID: "c1", SenderName: "Me", SentAt: 200, Body: "sounds good, coffee it is"},
		}, 1001); err != nil {
			return err
		}
		if err := tx.MarkBackfillDone(ctx, "c1", 1001); err != nil {
			return err
		}

		if err := tx.UpsertConversation(ctx, Conversation{
			ID: "c2", URN: "urn:c2",
			Participants: []Participant{{Name: "Grace Hopper", URN: "urn:li:fs_miniProfile:grace"}},
			UpdatedAt:    900,
		}, 1002); err != nil {
			return err
		}
		_, err := tx.RecordMessagePage(ctx, "c2", []Message{
			{URN: "m3", ConversationID: "c2", SenderName: "Grace Hopper", SenderURN: "urn:li:fs_miniProfile:grace", SentAt: 300, Body: "unrelated message about golf"},
		}, 1003)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

// TestSearchFiltersNarrowResults is the required filter test: --conversation,
// --from, --after/--before, and --limit each narrow a search independently.
func TestSearchFiltersNarrowResults(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedTwoConversations(t, s)

	// A body word shared across both messages in c1 but absent from c2.
	all, err := s.Search(ctx, SearchFilter{Query: "coffee"})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("unfiltered Search(coffee) = %d hits, want 2", len(all))
	}

	// --conversation narrows to one thread.
	byConv, err := s.Search(ctx, SearchFilter{Query: "coffee", ConversationID: "c1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(byConv) != 2 {
		t.Errorf("Search with ConversationID=c1 = %d hits, want 2", len(byConv))
	}
	byConv2, err := s.Search(ctx, SearchFilter{Query: "coffee", ConversationID: "c2"})
	if err != nil {
		t.Fatal(err)
	}
	if len(byConv2) != 0 {
		t.Errorf("Search with ConversationID=c2 = %d hits, want 0 (no match there)", len(byConv2))
	}

	// --from matches sender name (substring, case-insensitive) or exact URN.
	byFromName, err := s.Search(ctx, SearchFilter{Query: "coffee", From: "ada"})
	if err != nil {
		t.Fatal(err)
	}
	if len(byFromName) != 1 || byFromName[0].URN != "m1" {
		t.Errorf("Search with From=ada = %+v, want exactly m1", byFromName)
	}
	byFromURN, err := s.Search(ctx, SearchFilter{Query: "coffee", From: "urn:li:fs_miniProfile:ada"})
	if err != nil {
		t.Fatal(err)
	}
	if len(byFromURN) != 1 || byFromURN[0].URN != "m1" {
		t.Errorf("Search with From=<exact URN> = %+v, want exactly m1", byFromURN)
	}

	// --after/--before bound SentAt.
	after := int64(150)
	byAfter, err := s.Search(ctx, SearchFilter{Query: "coffee", After: &after})
	if err != nil {
		t.Fatal(err)
	}
	if len(byAfter) != 1 || byAfter[0].URN != "m2" {
		t.Errorf("Search with After=150 = %+v, want exactly m2", byAfter)
	}
	before := int64(150)
	byBefore, err := s.Search(ctx, SearchFilter{Query: "coffee", Before: &before})
	if err != nil {
		t.Fatal(err)
	}
	if len(byBefore) != 1 || byBefore[0].URN != "m1" {
		t.Errorf("Search with Before=150 = %+v, want exactly m1", byBefore)
	}

	// --limit caps the result count.
	limited, err := s.Search(ctx, SearchFilter{Query: "coffee", Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(limited) != 1 {
		t.Errorf("Search with Limit=1 = %d hits, want 1", len(limited))
	}
}

// TestSearchOrdering covers the newest-first default and --asc reversal —
// deliberately by SentAt, not FTS5 relevance rank (see SearchFilter.Asc).
func TestSearchOrdering(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedTwoConversations(t, s)

	desc, err := s.Search(ctx, SearchFilter{Query: "coffee"})
	if err != nil {
		t.Fatal(err)
	}
	if len(desc) != 2 || desc[0].URN != "m2" || desc[1].URN != "m1" {
		t.Errorf("default Search order = %+v, want [m2, m1] (newest first)", desc)
	}

	asc, err := s.Search(ctx, SearchFilter{Query: "coffee", Asc: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(asc) != 2 || asc[0].URN != "m1" || asc[1].URN != "m2" {
		t.Errorf("Asc Search order = %+v, want [m1, m2] (oldest first)", asc)
	}
}

// TestSearchNoMatchReturnsEmptyNotError pins the "no matches is not an
// error" contract that `lion message search` relies on to distinguish "zero
// results" from "no store" (see cli/search.go).
func TestSearchNoMatchReturnsEmptyNotError(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedTwoConversations(t, s)

	hits, err := s.Search(ctx, SearchFilter{Query: "spaceship"})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Errorf("Search(spaceship) = %+v, want no hits", hits)
	}
}

// TestCoverageReportsCountsAndBackfillDone is the required coverage test:
// oldest/newest/count and BackfillDone must be correct per conversation, and
// the result must be sorted newest-activity-first.
func TestCoverageReportsCountsAndBackfillDone(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedTwoConversations(t, s)

	cov, err := s.Coverage(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(cov) != 2 {
		t.Fatalf("Coverage(\"\") = %d entries, want 2", len(cov))
	}
	// c2 (UpdatedAt=900) must sort before c1 (UpdatedAt=500): newest-activity
	// first.
	if cov[0].ID != "c2" || cov[1].ID != "c1" {
		t.Fatalf("Coverage order = [%s, %s], want [c2, c1] (newest activity first)", cov[0].ID, cov[1].ID)
	}

	c1 := cov[1]
	if c1.MessageCount != 2 {
		t.Errorf("c1.MessageCount = %d, want 2", c1.MessageCount)
	}
	if c1.OldestSynced == nil || *c1.OldestSynced != 100 {
		t.Errorf("c1.OldestSynced = %v, want 100", c1.OldestSynced)
	}
	if c1.NewestSynced == nil || *c1.NewestSynced != 200 {
		t.Errorf("c1.NewestSynced = %v, want 200", c1.NewestSynced)
	}
	if !c1.BackfillDone {
		t.Error("c1.BackfillDone = false, want true")
	}

	c2 := cov[0]
	if c2.MessageCount != 1 {
		t.Errorf("c2.MessageCount = %d, want 1", c2.MessageCount)
	}
	if c2.BackfillDone {
		t.Error("c2.BackfillDone = true, want false (never marked)")
	}

	// Filtering to one conversation id returns exactly that one.
	only, err := s.Coverage(ctx, "c1")
	if err != nil {
		t.Fatal(err)
	}
	if len(only) != 1 || only[0].ID != "c1" {
		t.Fatalf("Coverage(\"c1\") = %+v, want exactly c1", only)
	}

	// An unknown id returns an empty slice, not an error.
	none, err := s.Coverage(ctx, "does-not-exist")
	if err != nil {
		t.Fatal(err)
	}
	if len(none) != 0 {
		t.Errorf("Coverage(unknown) = %+v, want empty", none)
	}
}

// TestCoverageConversationWithNoMessages covers the LEFT JOIN edge case: a
// conversation discovered but never paged for messages must report
// MessageCount 0, not be silently dropped from the result (an INNER JOIN
// would have done the latter).
func TestCoverageConversationWithNoMessages(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.WithTx(ctx, func(tx *Tx) error {
		return tx.UpsertConversation(ctx, Conversation{ID: "empty", URN: "urn:empty", UpdatedAt: 1}, 1)
	}); err != nil {
		t.Fatal(err)
	}

	cov, err := s.Coverage(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(cov) != 1 || cov[0].MessageCount != 0 {
		t.Errorf("Coverage for a message-less conversation = %+v, want one entry with MessageCount 0", cov)
	}
}

// TestConversationsOlderThan covers `lion store cleanup`'s candidate query:
// only conversations whose UpdatedAt is strictly before the cutoff come
// back, oldest-activity first.
func TestConversationsOlderThan(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedTwoConversations(t, s) // c1 UpdatedAt=500, c2 UpdatedAt=900

	old, err := s.ConversationsOlderThan(ctx, 600)
	if err != nil {
		t.Fatal(err)
	}
	if len(old) != 1 || old[0].ID != "c1" {
		t.Errorf("ConversationsOlderThan(600) = %+v, want exactly c1", old)
	}

	none, err := s.ConversationsOlderThan(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(none) != 0 {
		t.Errorf("ConversationsOlderThan(100) = %+v, want none (both newer)", none)
	}

	all, err := s.ConversationsOlderThan(ctx, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 || all[0].ID != "c1" || all[1].ID != "c2" {
		t.Errorf("ConversationsOlderThan(1000) = %+v, want [c1, c2] oldest-activity first", all)
	}
}

// TestStatsMatchesSyncedData is the required stats test: counts must match
// what was actually written, oldest/newest bound the stored messages, and
// LastSyncedAt reflects the most recently synced conversation.
func TestStatsMatchesSyncedData(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedTwoConversations(t, s)

	st, err := s.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Conversations != 2 {
		t.Errorf("Conversations = %d, want 2", st.Conversations)
	}
	if st.Messages != 3 {
		t.Errorf("Messages = %d, want 3", st.Messages)
	}
	if st.OldestMessage == nil || *st.OldestMessage != 100 {
		t.Errorf("OldestMessage = %v, want 100", st.OldestMessage)
	}
	if st.NewestMessage == nil || *st.NewestMessage != 300 {
		t.Errorf("NewestMessage = %v, want 300", st.NewestMessage)
	}
	if st.SchemaVersion != currentSchemaVersion {
		t.Errorf("SchemaVersion = %d, want %d", st.SchemaVersion, currentSchemaVersion)
	}
	// c2 was recorded last (1003), so that's the store-wide LastSyncedAt.
	if st.LastSyncedAt == nil || *st.LastSyncedAt != 1003 {
		t.Errorf("LastSyncedAt = %v, want 1003 (the most recently synced conversation)", st.LastSyncedAt)
	}
}

// TestStatsOnEmptyStore pins the zero-value shape `lion store stats` reports
// before any sync has run: zero counts and nil bounds, not an error — a
// stats command reporting "you have nothing yet" is itself useful output,
// unlike an empty export/search which would look like false success.
func TestStatsOnEmptyStore(t *testing.T) {
	s := newTestStore(t)
	st, err := s.Stats(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.Conversations != 0 || st.Messages != 0 {
		t.Errorf("Stats on a fresh store = %+v, want all zero", st)
	}
	if st.OldestMessage != nil || st.NewestMessage != nil || st.LastSyncedAt != nil {
		t.Errorf("Stats on a fresh store = %+v, want nil bounds", st)
	}
}

// TestDeleteConversationIfOlderThanRespectsCutoff is the store cleanup race
// guard: a conversation a concurrent sync refreshed (updated_at bumped past
// the cutoff) between cleanup's target selection and the delete must survive,
// while a still-stale one is removed. Folding the cutoff into the DELETE's own
// WHERE is what closes that window.
func TestDeleteConversationIfOlderThanRespectsCutoff(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	const cutoff = int64(1000)

	mk := func(id string, updated int64) {
		if err := s.WithTx(ctx, func(tx *Tx) error {
			return tx.UpsertConversation(ctx, Conversation{
				ID: id, URN: "urn:li:fs_conversation:" + id, UpdatedAt: updated,
			}, updated)
		}); err != nil {
			t.Fatal(err)
		}
	}
	mk("stale", 500)  // below cutoff → deletable
	mk("fresh", 5000) // a sync bumped it above cutoff → must be spared

	gone, err := s.DeleteConversationIfOlderThan(ctx, "fresh", cutoff)
	if err != nil {
		t.Fatal(err)
	}
	if gone {
		t.Error("a conversation newer than the cutoff was deleted; the race guard failed")
	}
	if _, ok, _ := s.Conversation(ctx, "fresh"); !ok {
		t.Error("fresh conversation is gone from the store")
	}

	gone, err = s.DeleteConversationIfOlderThan(ctx, "stale", cutoff)
	if err != nil {
		t.Fatal(err)
	}
	if !gone {
		t.Error("a still-stale conversation was not deleted")
	}
	if _, ok, _ := s.Conversation(ctx, "stale"); ok {
		t.Error("stale conversation should have been removed")
	}
}
