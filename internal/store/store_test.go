package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "store.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// TestOpenCreatesSchema is the migration test: a fresh Open must create the
// v1 schema and record schema_version, and running it again must be a no-op
// rather than erroring on "table already exists".
func TestOpenCreatesSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	v, err := s.schemaVersion(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if v != currentSchemaVersion {
		t.Errorf("schemaVersion = %d, want %d", v, currentSchemaVersion)
	}
	s.Close()

	// Re-opening an existing store must not fail (idempotent migration).
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("second Open on an existing store: %v", err)
	}
	s2.Close()
}

// TestOpenModes is the 0600/0700 modes test: the store file and its parent
// directory must end up with restrictive permissions, since the store holds
// a complete copy of someone's private messages.
func TestOpenModes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix permission bits don't apply on windows")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "store.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("store.db mode = %o, want 0600", perm)
	}

	di, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if perm := di.Mode().Perm(); perm != 0o700 {
		t.Errorf("store dir mode = %o, want 0700", perm)
	}
}

// TestOpenPreservesPreexistingDirMode is the Theme A regression test: a
// --store path inside a directory that already existed (the way "--store
// /tmp/store.db" or "--store ./store.db" name a directory lion had no hand
// in) must not have its mode touched, even though the db file inside it
// still gets the usual 0600 treatment.
func TestOpenPreservesPreexistingDirMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix permission bits don't apply on windows")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "store.db")

	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	di, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if perm := di.Mode().Perm(); perm != 0o755 {
		t.Errorf("pre-existing dir mode = %o, want unchanged 0755", perm)
	}

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("store.db mode = %o, want 0600 regardless of the directory's mode", perm)
	}
}

// TestUpsertMessageIsIdempotent pins the load-bearing property: applying
// the same message page twice must never duplicate a row, which is what
// makes resumption and --follow safe.
func TestUpsertMessageIsIdempotent(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	conv := Conversation{ID: "c1", URN: "urn:li:fs_conversation:c1", UpdatedAt: 100}
	if err := s.WithTx(ctx, func(tx *Tx) error {
		return tx.UpsertConversation(ctx, conv, 1000)
	}); err != nil {
		t.Fatal(err)
	}

	msgs := []Message{{URN: "m1", ConversationID: "c1", SentAt: 100, Body: "hello"}}

	var added int
	apply := func() {
		t.Helper()
		if err := s.WithTx(ctx, func(tx *Tx) error {
			var err error
			added, err = tx.RecordMessagePage(ctx, "c1", msgs, 2000)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}

	apply()
	if added != 1 {
		t.Errorf("first apply: added = %d, want 1", added)
	}
	apply()
	if added != 0 {
		t.Errorf("second apply (same message): added = %d, want 0 (already stored)", added)
	}

	got, err := s.Messages(ctx, MessageFilter{ConversationID: "c1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Errorf("len(Messages) = %d, want 1 (upsert must not duplicate)", len(got))
	}
}

// TestRecordMessagePageMergesBounds covers the catch-up/backfill interplay:
// a newer page extends NewestSynced, a later older page extends
// OldestSynced, and neither call clobbers the bound the other one set.
func TestRecordMessagePageMergesBounds(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	conv := Conversation{ID: "c1", URN: "urn:c1", UpdatedAt: 500}
	if err := s.WithTx(ctx, func(tx *Tx) error {
		return tx.UpsertConversation(ctx, conv, 1000)
	}); err != nil {
		t.Fatal(err)
	}

	// Catch-up: newest page first.
	if err := s.WithTx(ctx, func(tx *Tx) error {
		_, err := tx.RecordMessagePage(ctx, "c1", []Message{
			{URN: "m3", ConversationID: "c1", SentAt: 300},
			{URN: "m4", ConversationID: "c1", SentAt: 400},
		}, 1001)
		return err
	}); err != nil {
		t.Fatal(err)
	}

	// Backfill: an older page, fetched later.
	if err := s.WithTx(ctx, func(tx *Tx) error {
		_, err := tx.RecordMessagePage(ctx, "c1", []Message{
			{URN: "m1", ConversationID: "c1", SentAt: 100},
			{URN: "m2", ConversationID: "c1", SentAt: 200},
		}, 1002)
		return err
	}); err != nil {
		t.Fatal(err)
	}

	got, ok, err := s.Conversation(ctx, "c1")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("conversation not found")
	}
	if got.NewestSynced == nil || *got.NewestSynced != 400 {
		t.Errorf("NewestSynced = %v, want 400", got.NewestSynced)
	}
	if got.OldestSynced == nil || *got.OldestSynced != 100 {
		t.Errorf("OldestSynced = %v, want 100", got.OldestSynced)
	}
	if got.LastSyncedAt != 1002 {
		t.Errorf("LastSyncedAt = %d, want 1002 (the most recent page)", got.LastSyncedAt)
	}
}

// TestMarkBackfillDone covers the terminal-page case and the ordering
// invariant (conversation must exist first).
func TestMarkBackfillDone(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if err := s.WithTx(ctx, func(tx *Tx) error {
		return tx.MarkBackfillDone(ctx, "does-not-exist", 1)
	}); err == nil {
		t.Fatal("expected ErrConversationNotFound for an unknown conversation")
	}

	if err := s.WithTx(ctx, func(tx *Tx) error {
		return tx.UpsertConversation(ctx, Conversation{ID: "c1", URN: "urn:c1", UpdatedAt: 1}, 1000)
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.WithTx(ctx, func(tx *Tx) error {
		return tx.MarkBackfillDone(ctx, "c1", 2000)
	}); err != nil {
		t.Fatal(err)
	}

	got, ok, err := s.Conversation(ctx, "c1")
	if err != nil || !ok {
		t.Fatalf("Conversation: ok=%v err=%v", ok, err)
	}
	if !got.BackfillDone {
		t.Error("BackfillDone = false, want true")
	}
}

// TestUpsertConversationPreservesFirstSeenAndProgress pins that re-running
// conversation discovery (a plain metadata refresh) must not reset
// FirstSeenAt or the sync-progress fields set by RecordMessagePage.
func TestUpsertConversationPreservesFirstSeenAndProgress(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	conv := Conversation{ID: "c1", URN: "urn:c1", UpdatedAt: 100, Unread: true}
	if err := s.WithTx(ctx, func(tx *Tx) error {
		return tx.UpsertConversation(ctx, conv, 1000)
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.WithTx(ctx, func(tx *Tx) error {
		_, err := tx.RecordMessagePage(ctx, "c1", []Message{{URN: "m1", ConversationID: "c1", SentAt: 50}}, 1500)
		return err
	}); err != nil {
		t.Fatal(err)
	}

	// Re-discover the same conversation later, with fresher metadata.
	conv.UpdatedAt = 999
	conv.Unread = false
	if err := s.WithTx(ctx, func(tx *Tx) error {
		return tx.UpsertConversation(ctx, conv, 9999)
	}); err != nil {
		t.Fatal(err)
	}

	got, ok, err := s.Conversation(ctx, "c1")
	if err != nil || !ok {
		t.Fatalf("Conversation: ok=%v err=%v", ok, err)
	}
	if got.FirstSeenAt != 1000 {
		t.Errorf("FirstSeenAt = %d, want 1000 (must not be reset by a later discovery pass)", got.FirstSeenAt)
	}
	if got.NewestSynced == nil || *got.NewestSynced != 50 {
		t.Errorf("NewestSynced = %v, want 50 (must survive a metadata-only refresh)", got.NewestSynced)
	}
	if got.UpdatedAt != 999 || got.Unread != false {
		t.Errorf("UpdatedAt/Unread not refreshed: got %d/%v", got.UpdatedAt, got.Unread)
	}
}

// TestSearchReturnsMatch is the FTS test: a body word must be findable via
// Search after being stored through the normal upsert path.
func TestSearchReturnsMatch(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if err := s.WithTx(ctx, func(tx *Tx) error {
		return tx.UpsertConversation(ctx, Conversation{ID: "c1", URN: "urn:c1", UpdatedAt: 1}, 1)
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.WithTx(ctx, func(tx *Tx) error {
		_, err := tx.RecordMessagePage(ctx, "c1", []Message{
			{URN: "m1", ConversationID: "c1", SentAt: 1, Body: "let's grab coffee tomorrow"},
			{URN: "m2", ConversationID: "c1", SentAt: 2, Body: "unrelated message about golf"},
		}, 1)
		return err
	}); err != nil {
		t.Fatal(err)
	}

	hits, err := s.Search(ctx, "coffee", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].URN != "m1" {
		t.Errorf("Search(coffee) = %+v, want exactly m1", hits)
	}
}

// TestCascadeDeleteRemovesMessages is the cascade-delete test: removing a
// conversation must remove its messages too (ON DELETE CASCADE), and must
// also clear it out of the FTS index rather than leaving an orphaned entry.
func TestCascadeDeleteRemovesMessages(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if err := s.WithTx(ctx, func(tx *Tx) error {
		return tx.UpsertConversation(ctx, Conversation{ID: "c1", URN: "urn:c1", UpdatedAt: 1}, 1)
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.WithTx(ctx, func(tx *Tx) error {
		_, err := tx.RecordMessagePage(ctx, "c1", []Message{{URN: "m1", ConversationID: "c1", SentAt: 1, Body: "hi there"}}, 1)
		return err
	}); err != nil {
		t.Fatal(err)
	}

	if err := s.DeleteConversation(ctx, "c1"); err != nil {
		t.Fatal(err)
	}

	msgs, err := s.Messages(ctx, MessageFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 0 {
		t.Errorf("Messages after DeleteConversation = %v, want none (cascade)", msgs)
	}
	hits, err := s.Search(ctx, "hi", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Errorf("Search after cascade delete = %v, want no hits", hits)
	}
}

// TestMessagesFilterAndLimit covers the export-facing query shape:
// conversation/after/before filtering, oldest-first ordering, and Limit
// selecting the most recent N rather than the first N.
func TestMessagesFilterAndLimit(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if err := s.WithTx(ctx, func(tx *Tx) error {
		return tx.UpsertConversation(ctx, Conversation{ID: "c1", URN: "urn:c1", UpdatedAt: 1}, 1)
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.WithTx(ctx, func(tx *Tx) error {
		_, err := tx.RecordMessagePage(ctx, "c1", []Message{
			{URN: "m1", ConversationID: "c1", SentAt: 100},
			{URN: "m2", ConversationID: "c1", SentAt: 200},
			{URN: "m3", ConversationID: "c1", SentAt: 300},
			{URN: "m4", ConversationID: "c1", SentAt: 400},
		}, 1)
		return err
	}); err != nil {
		t.Fatal(err)
	}

	after := int64(150)
	got, err := s.Messages(ctx, MessageFilter{After: &after})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].URN != "m2" || got[2].URN != "m4" {
		t.Errorf("After=150 = %+v, want m2,m3,m4 oldest-first", got)
	}

	limited, err := s.Messages(ctx, MessageFilter{Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(limited) != 2 || limited[0].URN != "m3" || limited[1].URN != "m4" {
		t.Errorf("Limit=2 = %+v, want the 2 most recent (m3,m4), oldest-first", limited)
	}
}

// TestForEachMessageMatchesMessagesFilterAndLimit is ForEachMessage's
// required regression: it must honor the same filter semantics as Messages,
// including Limit's "most recent N, emitted oldest-first" selection — done
// here via a SQL subquery rather than by buffering and reversing in Go (see
// ForEachMessage's doc comment), so this pins that the two selection
// strategies actually agree.
func TestForEachMessageMatchesMessagesFilterAndLimit(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if err := s.WithTx(ctx, func(tx *Tx) error {
		return tx.UpsertConversation(ctx, Conversation{ID: "c1", URN: "urn:c1", UpdatedAt: 1}, 1)
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.WithTx(ctx, func(tx *Tx) error {
		_, err := tx.RecordMessagePage(ctx, "c1", []Message{
			{URN: "m1", ConversationID: "c1", SentAt: 100},
			{URN: "m2", ConversationID: "c1", SentAt: 200},
			{URN: "m3", ConversationID: "c1", SentAt: 300},
			{URN: "m4", ConversationID: "c1", SentAt: 400},
		}, 1)
		return err
	}); err != nil {
		t.Fatal(err)
	}

	collect := func(f MessageFilter) []Message {
		t.Helper()
		var got []Message
		if err := s.ForEachMessage(ctx, f, func(m Message) error {
			got = append(got, m)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		return got
	}

	after := int64(150)
	got := collect(MessageFilter{After: &after})
	if len(got) != 3 || got[0].URN != "m2" || got[2].URN != "m4" {
		t.Errorf("After=150 = %+v, want m2,m3,m4 oldest-first", got)
	}

	limited := collect(MessageFilter{Limit: 2})
	if len(limited) != 2 || limited[0].URN != "m3" || limited[1].URN != "m4" {
		t.Errorf("Limit=2 = %+v, want the 2 most recent (m3,m4), oldest-first", limited)
	}

	// fn's error must stop iteration immediately and come back unchanged, so
	// a caller streaming into an io.Writer can propagate a write failure.
	stopErr := errors.New("stop")
	seen := 0
	err := s.ForEachMessage(ctx, MessageFilter{}, func(Message) error {
		seen++
		return stopErr
	})
	if !errors.Is(err, stopErr) {
		t.Errorf("err = %v, want stopErr", err)
	}
	if seen != 1 {
		t.Errorf("fn invoked %d times before the error stopped iteration, want 1", seen)
	}
}

// TestEmpty covers export's "store is empty" guard.
func TestEmpty(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	empty, err := s.Empty(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !empty {
		t.Error("Empty() on a fresh store = false, want true")
	}

	if err := s.WithTx(ctx, func(tx *Tx) error {
		return tx.UpsertConversation(ctx, Conversation{ID: "c1", URN: "urn:c1", UpdatedAt: 1}, 1)
	}); err != nil {
		t.Fatal(err)
	}
	empty, err = s.Empty(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if empty {
		t.Error("Empty() after a conversation was stored = true, want false")
	}
}

// TestLockExclusiveWithHolderInfo covers the sync-lock UX: a second Lock
// with --lock-wait 0 must fail fast and name the current holder's pid, and
// releasing must let a waiting acquirer through within its --lock-wait.
func TestLockExclusiveWithHolderInfo(t *testing.T) {
	s := newTestStore(t)
	if !s.LockSupported() {
		t.Skip("no inter-process lock on this platform")
	}
	ctx := context.Background()

	release1, err := s.Lock(ctx, 0)
	if err != nil {
		t.Fatalf("first Lock: %v", err)
	}

	_, err = s.Lock(ctx, 0)
	if err == nil {
		t.Fatal("second Lock with --lock-wait 0 succeeded while the first still held it")
	}
	wantPID := os.Getpid()
	if !strings.Contains(err.Error(), "pid "+strconv.Itoa(wantPID)) {
		t.Errorf("error = %q, want it to name the holder's pid (%d)", err.Error(), wantPID)
	}

	// A waiting acquirer should get in once the first releases, well within
	// its wait budget.
	done := make(chan error, 1)
	go func() {
		release2, err := s.Lock(context.Background(), 2*time.Second)
		if err == nil {
			release2()
		}
		done <- err
	}()

	time.Sleep(100 * time.Millisecond)
	release1()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("waiting Lock failed: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("waiting Lock never succeeded after the holder released")
	}
}

// TestSizeBytesGrowsWithData is a light sanity check that SizeBytes reports
// something plausible rather than 0/an error, backing --max-db-size.
func TestSizeBytesGrowsWithData(t *testing.T) {
	s := newTestStore(t)
	before, err := s.SizeBytes()
	if err != nil {
		t.Fatal(err)
	}
	if before <= 0 {
		t.Errorf("SizeBytes() on a freshly-migrated store = %d, want > 0", before)
	}
}

// TestOpenRefusesSymlinkedStorePath covers the permission-tampering half of
// the symlink class fixed elsewhere in this branch. A plain open and os.Chmod
// both dereference a symlink, so a --store path in a shared directory could be
// pre-placed as a link to another file and lion would open it and re-permission
// it to 0600. The store is always a regular file lion manages.
func TestOpenRefusesSymlinkedStorePath(t *testing.T) {
	dir := t.TempDir()
	victim := filepath.Join(dir, "victim.txt")
	if err := os.WriteFile(victim, []byte("keep me"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "store.db")
	if err := os.Symlink(victim, link); err != nil {
		t.Fatal(err)
	}

	if _, err := Open(link); err == nil {
		t.Fatal("Open on a symlinked store path should be refused")
	}

	fi, err := os.Lstat(victim)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o644 {
		t.Errorf("victim mode = %o, want 0644 unchanged", got)
	}
	b, err := os.ReadFile(victim)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "keep me" {
		t.Errorf("victim contents = %q, want them untouched", b)
	}
}

// TestOpenRefusesWorldWritableStoreDir covers the swap race that a symlink
// check alone cannot close: ensureFileMode must release its descriptor before
// SQLite reopens the path by name, so where another user can create entries
// they can substitute a symlink in that gap. Refusing such a directory removes
// the party that would win the race.
func TestOpenRefusesWorldWritableStoreDir(t *testing.T) {
	parent := t.TempDir()
	shared := filepath.Join(parent, "shared")
	if err := os.Mkdir(shared, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(shared, 0o777); err != nil { // defeat umask
		t.Fatal(err)
	}
	if _, err := Open(filepath.Join(shared, "store.db")); err == nil {
		t.Fatal("Open in a world-writable directory should be refused")
	}

	// A private directory must still work, or the check is too broad to ship.
	private := filepath.Join(parent, "private")
	if err := os.Mkdir(private, 0o700); err != nil {
		t.Fatal(err)
	}
	s, err := Open(filepath.Join(private, "store.db"))
	if err != nil {
		t.Fatalf("Open in a private directory = %v, want success", err)
	}
	s.Close()
}
