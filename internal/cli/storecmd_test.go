package cli

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jodok/lion/internal/store"
)

// TestStoreStatsMatchesSyncedData is the required stats test: counts must
// match what sync (here, a direct store seed standing in for it) actually
// wrote.
func TestStoreStatsMatchesSyncedData(t *testing.T) {
	seedExportStore(t) // 2 conversations, 3 messages total

	var runErr error
	out := captureStdout(t, func() {
		runErr = runRoot(t, "store", "stats", "--json")
	})
	if runErr != nil {
		t.Fatalf("store stats: %v", runErr)
	}

	var got storeStatsResult
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output not valid JSON: %v\noutput: %s", err, out)
	}
	if got.Conversations != 2 {
		t.Errorf("Conversations = %d, want 2", got.Conversations)
	}
	if got.Messages != 3 {
		t.Errorf("Messages = %d, want 3", got.Messages)
	}
	if got.OldestMessage == nil || *got.OldestMessage != 100 {
		t.Errorf("OldestMessage = %v, want 100", got.OldestMessage)
	}
	if got.NewestMessage == nil || *got.NewestMessage != 300 {
		t.Errorf("NewestMessage = %v, want 300", got.NewestMessage)
	}
	if got.SizeBytes <= 0 {
		t.Errorf("SizeBytes = %d, want > 0", got.SizeBytes)
	}
	if got.Path == "" {
		t.Error("Path = \"\", want the store's file path")
	}
}

// TestStoreStatsOnEmptyStoreIsNotAnError pins that stats on a never-synced
// store reports zero counts rather than erroring — unlike search/export,
// zero here is itself an honest, useful answer.
func TestStoreStatsOnEmptyStoreIsNotAnError(t *testing.T) {
	isolateHome(t)
	err := runRoot(t, "store", "stats")
	if err != nil {
		t.Fatalf("store stats on an empty store should not error, got %v", err)
	}
}

// seedOldConversation isolates LION_HOME and writes one conversation whose
// UpdatedAt is old enough to be picked up by `store cleanup --days N` for a
// small N, plus one recent conversation that must never be — so cleanup
// tests can assert on exactly which one is touched.
func seedOldConversation(t *testing.T) {
	t.Helper()
	isolateHome(t)
	path, err := store.DefaultPath()
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	ctx := context.Background()
	// UpdatedAt is epoch ms; 400 days ago is comfortably past any --days
	// threshold used below, and "now" is comfortably within all of them.
	oldMs := nowMillisForTest() - 400*24*60*60*1000
	nowMs := nowMillisForTest()
	if err := st.WithTx(ctx, func(tx *store.Tx) error {
		if err := tx.UpsertConversation(ctx, store.Conversation{
			ID: "stale", URN: "urn:li:fs_conversation:stale",
			Participants: []store.Participant{{Name: "Ada Lovelace", URN: "urn:li:fs_miniProfile:ada"}},
			UpdatedAt:    oldMs,
		}, oldMs); err != nil {
			return err
		}
		if _, err := tx.RecordMessagePage(ctx, "stale", []store.Message{
			{URN: "m1", ConversationID: "stale", SentAt: oldMs, Body: "ancient history"},
		}, oldMs); err != nil {
			return err
		}
		if err := tx.UpsertConversation(ctx, store.Conversation{
			ID: "fresh", URN: "urn:li:fs_conversation:fresh",
			Participants: []store.Participant{{Name: "Grace Hopper", URN: "urn:li:fs_miniProfile:grace"}},
			UpdatedAt:    nowMs,
		}, nowMs); err != nil {
			return err
		}
		_, err := tx.RecordMessagePage(ctx, "fresh", []store.Message{
			{URN: "m2", ConversationID: "fresh", SentAt: nowMs, Body: "still relevant"},
		}, nowMs)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func nowMillisForTest() int64 {
	return time.Now().UnixMilli()
}

// TestStoreCleanupDryRunChangesNothing is the required --dry-run test: it
// must print what would be removed and delete nothing.
func TestStoreCleanupDryRunChangesNothing(t *testing.T) {
	seedOldConversation(t)

	var runErr error
	out := captureStdout(t, func() {
		runErr = runRoot(t, "store", "cleanup", "--days", "30", "--dry-run", "--json")
	})
	if runErr != nil {
		t.Fatalf("store cleanup --dry-run: %v", runErr)
	}

	var res cleanupResult
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("output not valid JSON: %v\noutput: %s", err, out)
	}
	if res.Status != "would_delete" {
		t.Errorf("Status = %q, want would_delete", res.Status)
	}
	if len(res.Conversations) != 1 || res.Conversations[0].ID != "stale" {
		t.Errorf("Conversations = %+v, want exactly [stale]", res.Conversations)
	}

	// Nothing was actually deleted.
	path, err := store.DefaultPath()
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, ok, err := st.Conversation(context.Background(), "stale"); err != nil || !ok {
		t.Errorf("stale conversation was removed by --dry-run: ok=%v err=%v", ok, err)
	}
}

// TestStoreCleanupYesDeletesOnlyPastThreshold is the required --yes test:
// only the conversation past the threshold is deleted, and its messages
// cascade with it; the fresh conversation survives untouched.
func TestStoreCleanupYesDeletesOnlyPastThreshold(t *testing.T) {
	seedOldConversation(t)

	err := runRoot(t, "store", "cleanup", "--days", "30", "--yes")
	if err != nil {
		t.Fatalf("store cleanup --yes: %v", err)
	}

	path, err := store.DefaultPath()
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()

	if _, ok, err := st.Conversation(ctx, "stale"); err != nil {
		t.Fatal(err)
	} else if ok {
		t.Error("stale conversation still present after cleanup --yes")
	}
	msgs, err := st.Messages(ctx, store.MessageFilter{ConversationID: "stale"})
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 0 {
		t.Errorf("stale conversation's messages = %d, want 0 (cascade delete)", len(msgs))
	}

	if _, ok, err := st.Conversation(ctx, "fresh"); err != nil || !ok {
		t.Errorf("fresh conversation removed: ok=%v err=%v, want it untouched", ok, err)
	}
}

// TestStoreCleanupReadonlyBlocked is the required --readonly test.
func TestStoreCleanupReadonlyBlocked(t *testing.T) {
	seedOldConversation(t)
	err := runRoot(t, "store", "cleanup", "--days", "30", "--yes", "--readonly")
	if err == nil {
		t.Fatal("expected --readonly to block store cleanup")
	}
	if exitCode(err) != ExitPermission {
		t.Errorf("exitCode(%v) = %d, want ExitPermission", err, exitCode(err))
	}

	// Nothing was deleted.
	path, err := store.DefaultPath()
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, ok, err := st.Conversation(context.Background(), "stale"); err != nil || !ok {
		t.Errorf("stale conversation removed despite --readonly: ok=%v err=%v", ok, err)
	}
}

// TestStoreCleanupNoInputWithoutYesRefuses is the required --no-input test:
// without --yes, --no-input must suppress the prompt and then decline
// (exit 2), matching every other mutation in this codebase (F15) — it must
// not silently proceed or silently do nothing.
func TestStoreCleanupNoInputWithoutYesRefuses(t *testing.T) {
	seedOldConversation(t)
	err := runRoot(t, "store", "cleanup", "--days", "30", "--no-input")
	if err == nil {
		t.Fatal("expected an error: --no-input alone is not consent")
	}
	if exitCode(err) != ExitUsage {
		t.Errorf("exitCode(%v) = %d, want ExitUsage", err, exitCode(err))
	}

	path, err := store.DefaultPath()
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, ok, err := st.Conversation(context.Background(), "stale"); err != nil || !ok {
		t.Errorf("stale conversation removed despite the refused confirmation: ok=%v err=%v", ok, err)
	}
}

// TestStoreCleanupHelpMentionsLocalCacheOnly pins the spec's explicit safety
// requirement: the help text must make clear this only prunes the local
// cache and deletes nothing on LinkedIn, so a user can't come away thinking
// otherwise in either direction.
func TestStoreCleanupHelpMentionsLocalCacheOnly(t *testing.T) {
	cmd := newStoreCleanupCmd()
	long := cmd.Long
	if !strings.Contains(strings.ToLower(long), "local cache") {
		t.Errorf("Long help = %q, want it to say this only touches the local cache", long)
	}
	if !strings.Contains(strings.ToLower(long), "nothing on linkedin") {
		t.Errorf("Long help = %q, want it to explicitly say nothing on LinkedIn is deleted", long)
	}
}

// TestStoreCleanupOutputNotesLocalCacheOnly pins the same requirement for
// the command's actual output (stderr), not just --help.
func TestStoreCleanupOutputNotesLocalCacheOnly(t *testing.T) {
	seedOldConversation(t)
	stderr := captureStderr(t, func() {
		if err := runRoot(t, "store", "cleanup", "--days", "30", "--dry-run"); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(stderr, "nothing on LinkedIn") {
		t.Errorf("stderr = %q, want it to note this only prunes the local cache", stderr)
	}
}

// TestStoreCleanupNoTargetsSkipsConfirmPrompt pins that a run with nothing
// past the threshold doesn't trip the non-interactive confirmation guard —
// there's nothing to confirm, matching `connection accept --all`'s
// identical zero-target guard.
func TestStoreCleanupNoTargetsSkipsConfirmPrompt(t *testing.T) {
	isolateHome(t) // no conversations at all
	err := runRoot(t, "store", "cleanup", "--days", "30")
	if err != nil {
		t.Errorf("cleanup with nothing to remove should not error, got %v", err)
	}
}
