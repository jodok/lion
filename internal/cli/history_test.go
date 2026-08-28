package cli

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/jodok/lion/internal/store"
)

// seedHistoryStore writes two conversations with different sync progress —
// c1 fully backfilled, c2 not — so coverage/backfill tests have something
// to distinguish.
func seedHistoryStore(t *testing.T) *store.Store {
	t.Helper()
	st := openSyncTestStore(t)
	ctx := context.Background()

	err := st.WithTx(ctx, func(tx *store.Tx) error {
		if err := tx.UpsertConversation(ctx, store.Conversation{
			ID: "c1", URN: "urn:li:fs_conversation:c1",
			Participants: []store.Participant{{Name: "Ada Lovelace", URN: "urn:li:fs_miniProfile:ada"}},
			UpdatedAt:    500,
		}, 1000); err != nil {
			return err
		}
		if _, err := tx.RecordMessagePage(ctx, "c1", []store.Message{
			{URN: "m1", ConversationID: "c1", SentAt: 100, Body: "hi"},
			{URN: "m2", ConversationID: "c1", SentAt: 200, Body: "hi again"},
		}, 1001); err != nil {
			return err
		}
		if err := tx.MarkBackfillDone(ctx, "c1", 1001); err != nil {
			return err
		}

		if err := tx.UpsertConversation(ctx, store.Conversation{
			ID: "c2", URN: "urn:li:fs_conversation:c2",
			Participants: []store.Participant{{Name: "Grace Hopper", URN: "urn:li:fs_miniProfile:grace"}},
			UpdatedAt:    900,
		}, 1002); err != nil {
			return err
		}
		_, err := tx.RecordMessagePage(ctx, "c2", []store.Message{
			{URN: "m3", ConversationID: "c2", SentAt: 300, Body: "hey"},
		}, 1003)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// TestHistoryCoverageReportsCorrectly is the required coverage test: correct
// oldest/newest/count and backfill_done per conversation, sorted
// newest-activity first.
func TestHistoryCoverageReportsCorrectly(t *testing.T) {
	seedHistoryStore(t)

	var runErr error
	out := captureStdout(t, func() {
		runErr = runRoot(t, "history", "coverage", "--json")
	})
	if runErr != nil {
		t.Fatalf("history coverage: %v", runErr)
	}

	var entries []coverageEntry
	if err := json.Unmarshal([]byte(out), &entries); err != nil {
		t.Fatalf("output not valid JSON: %v\noutput: %s", err, out)
	}
	if len(entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(entries))
	}
	// c2 (UpdatedAt=900) must sort before c1 (UpdatedAt=500).
	if entries[0].ID != "c2" || entries[1].ID != "c1" {
		t.Fatalf("order = [%s, %s], want [c2, c1] (newest activity first)", entries[0].ID, entries[1].ID)
	}

	c1 := entries[1]
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

	c2 := entries[0]
	if c2.MessageCount != 1 {
		t.Errorf("c2.MessageCount = %d, want 1", c2.MessageCount)
	}
	if c2.BackfillDone {
		t.Error("c2.BackfillDone = true, want false")
	}
}

// TestHistoryCoverageMissingStoreErrors mirrors message search's identical
// guard: coverage against an empty/never-synced store must error rather
// than report an empty (and misleading) coverage list.
func TestHistoryCoverageMissingStoreErrors(t *testing.T) {
	isolateHome(t)
	path, err := store.DefaultPath()
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	st.Close()

	stderr := captureStderr(t, func() {
		err = runRoot(t, "history", "coverage")
	})
	if err == nil {
		t.Fatal("expected an error for a missing/empty store")
	}
	if !strings.Contains(stderr, "sync") {
		t.Errorf("stderr = %q, want it to point at `lion sync`", stderr)
	}
}

// TestHistoryCoverageUnknownConversationErrors pins that an unknown
// --conversation is reported as an error.
func TestHistoryCoverageUnknownConversationErrors(t *testing.T) {
	seedHistoryStore(t)
	err := runRoot(t, "history", "coverage", "--conversation", "does-not-exist")
	if err == nil {
		t.Fatal("expected an error for an unknown --conversation")
	}
}

// TestResolveBackfillTargetsDefaultsToIncomplete pins the target-selection
// logic: with no --conversation, only conversations not yet BackfillDone
// are selected.
func TestResolveBackfillTargetsDefaultsToIncomplete(t *testing.T) {
	st := seedHistoryStore(t)
	targets, err := resolveBackfillTargets(context.Background(), st, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 1 || targets[0] != "c2" {
		t.Errorf("targets = %v, want exactly [c2] (c1 is already backfill_done)", targets)
	}
}

// TestResolveBackfillTargetsUnknownConversationErrors pins that an unknown
// --conversation is rejected rather than silently backfilling nothing.
func TestResolveBackfillTargetsUnknownConversationErrors(t *testing.T) {
	st := seedHistoryStore(t)
	if _, err := resolveBackfillTargets(context.Background(), st, "does-not-exist"); err == nil {
		t.Fatal("expected an error for an unknown conversation id")
	}
}

// TestHistoryBackfillResumesFromStoredCursor is the required backfill test:
// backfill must continue paging backwards from the conversation's already-
// recorded OldestSynced, not restart from scratch, and mark BackfillDone
// once it reaches an empty page — using the exact same backfillMessages
// code path `lion sync --backfill` uses.
func TestHistoryBackfillResumesFromStoredCursor(t *testing.T) {
	st := openSyncTestStore(t)
	ctx := context.Background()

	// Simulate a prior sync that only caught up to message m2 (sent_at=200),
	// leaving OldestSynced=200 and BackfillDone=false.
	if err := st.WithTx(ctx, func(tx *store.Tx) error {
		if err := tx.UpsertConversation(ctx, store.Conversation{ID: "c1", URN: "urn:li:fs_conversation:c1", UpdatedAt: 5000}, 1000); err != nil {
			return err
		}
		_, err := tx.RecordMessagePage(ctx, "c1", []store.Message{{URN: "m2", ConversationID: "c1", SentAt: 200}}, 1001)
		return err
	}); err != nil {
		t.Fatal(err)
	}

	rt := newRouteFixtureTransport().
		on("/me", meJSON).
		on(routeMessages("c1"),
			// Backfill must page from createdBefore=200 (the stored
			// OldestSynced), not from "now" — a fixture with only these two
			// pages queued would return the wrong/stale page indefinitely
			// if resumption didn't work, and the test would see 0 added.
			messagesSyncJSON([][2]any{{"m1", int64(100)}}),
			messagesSyncJSON(nil)) // terminal empty page -> BackfillDone
	cl := newFixtureClient(rt)

	targets, err := resolveBackfillTargets(ctx, st, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 1 || targets[0] != "c1" {
		t.Fatalf("targets = %v, want exactly [c1]", targets)
	}

	summary, err := runHistoryBackfill(ctx, cl, st, targets, nil, 0, discardProgress(t))
	if err != nil {
		t.Fatalf("runHistoryBackfill: %v", err)
	}
	if !summary.Complete {
		t.Error("Complete = false, want true")
	}
	if summary.MessagesAdded != 1 || summary.ConversationsProcessed != 1 {
		t.Errorf("summary = %+v, want 1 message added, 1 conversation processed", summary)
	}

	conv, ok, err := st.Conversation(ctx, "c1")
	if err != nil || !ok {
		t.Fatalf("Conversation: ok=%v err=%v", ok, err)
	}
	if !conv.BackfillDone {
		t.Error("BackfillDone = false, want true once backfill reaches an empty page")
	}
	if conv.OldestSynced == nil || *conv.OldestSynced != 100 {
		t.Errorf("OldestSynced = %v, want 100", conv.OldestSynced)
	}
}

// TestHistoryBackfillPartialRunIsHonest is the required partial-run test:
// an error partway through must report complete:false and return the error
// (so the caller's exit code is right), while whatever committed before the
// failure stays committed.
func TestHistoryBackfillPartialRunIsHonest(t *testing.T) {
	st := openSyncTestStore(t)
	ctx := context.Background()

	if err := st.WithTx(ctx, func(tx *store.Tx) error {
		if err := tx.UpsertConversation(ctx, store.Conversation{ID: "c1", URN: "urn:c1", UpdatedAt: 5000}, 1); err != nil {
			return err
		}
		if err := tx.UpsertConversation(ctx, store.Conversation{ID: "c2", URN: "urn:c2", UpdatedAt: 4000}, 1); err != nil {
			return err
		}
		_, err := tx.RecordMessagePage(ctx, "c1", []store.Message{{URN: "m1", ConversationID: "c1", SentAt: 100}}, 1)
		return err
	}); err != nil {
		t.Fatal(err)
	}

	boom := errors.New("simulated rate limit")
	rt := newRouteFixtureTransport().
		on("/me", meJSON).
		on(routeMessages("c1"), messagesSyncJSON(nil)). // c1 reaches an empty page: backfill_done
		on(routeMessages("c2"), messagesSyncJSON([][2]any{{"m2", int64(50)}}))
	rt.failOnCall("/messaging/conversations/c2/events", 0, boom)
	cl := newFixtureClient(rt)

	targets, err := resolveBackfillTargets(ctx, st, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 2 {
		t.Fatalf("targets = %v, want both conversations (neither is backfill_done yet)", targets)
	}

	summary, err := runHistoryBackfill(ctx, cl, st, targets, nil, 0, discardProgress(t))
	if err == nil {
		t.Fatal("expected an error from the failing conversation")
	}
	if summary.Complete {
		t.Error("Complete = true, want false after a mid-run failure")
	}

	// c1 must have finished (it processed before c2 failed) and been marked
	// backfill_done.
	c1, ok, err := st.Conversation(ctx, "c1")
	if err != nil || !ok {
		t.Fatalf("Conversation c1: ok=%v err=%v", ok, err)
	}
	if !c1.BackfillDone {
		t.Error("c1.BackfillDone = false, want true (it completed before c2 failed)")
	}
}
