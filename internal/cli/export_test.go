package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/jodok/lion/internal/store"
)

// seedExportStore isolates LION_HOME, opens the default store, and writes
// two conversations' worth of messages into it directly through
// internal/store (rather than via `lion sync`, which needs a network
// fixture — see sync_test.go for that side), so export tests can focus on
// the read path.
func seedExportStore(t *testing.T) {
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
	err = st.WithTx(ctx, func(tx *store.Tx) error {
		if err := tx.UpsertConversation(ctx, store.Conversation{
			ID: "c1", URN: "urn:li:fs_conversation:c1",
			Participants: []store.Participant{{Name: "Ada Lovelace", URN: "urn:li:fs_miniProfile:ada"}},
			UpdatedAt:    200,
		}, 1000); err != nil {
			return err
		}
		if _, err := tx.RecordMessagePage(ctx, "c1", []store.Message{
			{URN: "m1", ConversationID: "c1", SenderName: "Ada Lovelace", SentAt: 100, Body: "hello there"},
			{URN: "m2", ConversationID: "c1", SenderName: "Me", SentAt: 200, Body: "hi Ada"},
		}, 1001); err != nil {
			return err
		}
		if err := tx.UpsertConversation(ctx, store.Conversation{
			ID: "c2", URN: "urn:li:fs_conversation:c2",
			Participants: []store.Participant{{Name: "Grace Hopper", URN: "urn:li:fs_miniProfile:grace"}},
			UpdatedAt:    300,
		}, 1002); err != nil {
			return err
		}
		_, err := tx.RecordMessagePage(ctx, "c2", []store.Message{
			{URN: "m3", ConversationID: "c2", SenderName: "Grace Hopper", SentAt: 300, Body: "coffee tomorrow?"},
		}, 1003)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestExportRoundTripsSyncedMessages is the required round-trip test: every
// message written to the store must come back out of a default (--format
// json) export.
func TestExportRoundTripsSyncedMessages(t *testing.T) {
	seedExportStore(t)

	var runErr error
	out := captureStdout(t, func() {
		runErr = runRoot(t, "message", "export", "--json")
	})
	if runErr != nil {
		t.Fatalf("export: %v", runErr)
	}

	var env exportEnvelope
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("output not valid JSON: %v\noutput: %s", err, out)
	}
	if len(env.Conversations) != 2 {
		t.Fatalf("conversations = %d, want 2", len(env.Conversations))
	}
	total := 0
	for _, c := range env.Conversations {
		total += len(c.Messages)
	}
	if total != 3 {
		t.Errorf("total messages = %d, want 3", total)
	}

	// Find c1 and check its messages came back oldest-first with the right
	// bodies (round-trip fidelity, not just count).
	for _, c := range env.Conversations {
		if c.ID != "c1" {
			continue
		}
		if len(c.Messages) != 2 || c.Messages[0].Body != "hello there" || c.Messages[1].Body != "hi Ada" {
			t.Errorf("c1 messages = %+v, want [hello there, hi Ada] oldest-first", c.Messages)
		}
	}
}

// TestExportSingleFileOutputRefusesToFollowSymlink is the required
// regression test for finding #2: a single-file `--output <file>` export (as
// opposed to the directory layout) used to open the destination path
// directly with O_TRUNC, which follows a symlink. Pre-placing a symlink at
// a predictable export path in a shared directory would let it truncate and
// overwrite whatever the symlink pointed at with the exporting user's
// private message data. writeExportFile must instead publish through the
// same temp-file-plus-rename helper the directory layout uses (safeWriteFile),
// so the export succeeds — there is no ownership/marker concept for a bare
// --output file, only the symlink-safety requirement — but the symlink's
// target is never opened, and the destination ends up a regular file with
// the export's own content, not the symlink dereferenced and truncated.
func TestExportSingleFileOutputRefusesToFollowSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need elevated privileges to create on windows")
	}
	seedExportStore(t)

	target := filepath.Join(t.TempDir(), "victim.txt")
	victimContents := "this file must survive the export untouched"
	if err := os.WriteFile(target, []byte(victimContents), 0o600); err != nil {
		t.Fatal(err)
	}

	outDir := t.TempDir()
	outPath := filepath.Join(outDir, "export.json")
	if err := os.Symlink(target, outPath); err != nil {
		t.Fatal(err)
	}

	if err := runRoot(t, "message", "export", "--output", outPath); err != nil {
		t.Fatalf("export to a symlinked --output path: %v", err)
	}

	b, statErr := os.ReadFile(target)
	if statErr != nil {
		t.Fatalf("symlink target destroyed or removed by the export: %v", statErr)
	}
	if string(b) != victimContents {
		t.Errorf("symlink target contents = %q, want unchanged %q", b, victimContents)
	}

	fi, lstatErr := os.Lstat(outPath)
	if lstatErr != nil {
		t.Fatalf("lstat %s: %v", outPath, lstatErr)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		t.Error("--output path is still a symlink; want it replaced with a regular file by the publish rename")
	}
	var env exportEnvelope
	got, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read %s: %v", outPath, err)
	}
	if err := json.Unmarshal(got, &env); err != nil {
		t.Fatalf("--output content not valid JSON: %v\ncontent: %s", err, got)
	}
	if len(env.Conversations) != 2 {
		t.Errorf("conversations in --output file = %d, want 2", len(env.Conversations))
	}
}

// TestExportStreamsJSONLToStdout is the required stdout-streaming test.
func TestExportStreamsJSONLToStdout(t *testing.T) {
	seedExportStore(t)

	var runErr error
	out := captureStdout(t, func() {
		runErr = runRoot(t, "message", "export", "--format", "jsonl")
	})
	if runErr != nil {
		t.Fatalf("export: %v", runErr)
	}

	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("jsonl lines = %d, want 3", len(lines))
	}
	for _, l := range lines {
		var m exportedMessage
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Errorf("line not valid JSON: %v (%q)", err, l)
		}
	}
}

// TestExportEmptyStoreErrors is the required empty-store test: an export
// against an empty store must fail (non-nil error, non-zero exit) with a
// clear stderr message, rather than silently writing an empty archive that
// reads as "you have no messages".
func TestExportEmptyStoreErrors(t *testing.T) {
	isolateHome(t)
	// Open (and immediately close) the store so it exists but is empty,
	// exercising the "empty" check itself rather than a missing-file error.
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
		err = runRoot(t, "message", "export")
	})
	if err == nil {
		t.Fatal("expected an error for an empty store")
	}
	if !strings.Contains(stderr, "empty") {
		t.Errorf("stderr = %q, want it to mention the store is empty", stderr)
	}
}

// TestExportOutputFileNotWrittenWhenNoMessagesMatchFilter covers the other
// "empty result must not look like success" case: filters that leave zero
// messages, even though the store itself isn't empty, must error and must
// not create the requested output file.
func TestExportOutputFileNotWrittenWhenNoMessagesMatchFilter(t *testing.T) {
	seedExportStore(t)
	out := filepath.Join(t.TempDir(), "nothing.json")

	err := runRoot(t, "message", "export", "--conversation", "c1", "--after", "9999-01-01", "--output", out)
	if err == nil {
		t.Fatal("expected an error when no messages match the filters")
	}
	if _, statErr := os.Stat(out); statErr == nil {
		t.Error("an output file was written despite no messages matching the filters")
	}
}

// TestExportJSONLOutputFileNotWrittenWhenNoMessagesMatchFilter is the jsonl
// counterpart of TestExportOutputFileNotWrittenWhenNoMessagesMatchFilter:
// writeExportFileJSONL can't know "zero rows" until store.Store.ForEachMessage
// has already finished (unlike --format json, which knows from a
// materialized slice before writing anything — see that test), so it must
// remove the file safeWriteFile already published rather than leave an
// empty archive that reads as "you have no messages".
func TestExportJSONLOutputFileNotWrittenWhenNoMessagesMatchFilter(t *testing.T) {
	seedExportStore(t)
	out := filepath.Join(t.TempDir(), "nothing.jsonl")

	err := runRoot(t, "message", "export", "--format", "jsonl", "--conversation", "c1", "--after", "9999-01-01", "--output", out)
	if err == nil {
		t.Fatal("expected an error when no messages match the filters")
	}
	if _, statErr := os.Stat(out); statErr == nil {
		t.Error("a jsonl output file was left behind despite no messages matching the filters")
	}
}

// oldStyleJSONLOutput reconstructs what the pre-streaming implementation
// would have written for --format jsonl: st.Messages (flat, oldest-first)
// grouped by conversation via groupMessagesByConversation, then each
// group's messages encoded in turn (see that function's doc comment). It's
// built directly from the store rather than pinned as a literal string, so
// TestExportJSONLStreamingMatchesOldMaterializedOutput below is checking
// "the new streaming path and the old materializing path agree", not just
// "the output looks like valid JSON".
func oldStyleJSONLOutput(t *testing.T, filter store.MessageFilter) string {
	t.Helper()
	path, err := store.DefaultPath()
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	msgs, err := st.Messages(context.Background(), filter)
	if err != nil {
		t.Fatal(err)
	}
	groups := groupMessagesByConversation(msgs)

	var buf strings.Builder
	enc := json.NewEncoder(&buf)
	for _, g := range groups {
		for _, m := range g.messages {
			if err := enc.Encode(toExportedMessage(m)); err != nil {
				t.Fatal(err)
			}
		}
	}
	return buf.String()
}

// TestExportJSONLStreamingMatchesOldMaterializedOutput is the required
// byte-for-byte regression: switching --format jsonl's write path from a
// materialize-then-group slice (store.Messages + groupMessagesByConversation)
// to store.Store.ForEachMessage streaming must not change what gets written,
// for a fixture small enough that the two orderings coincide (see
// oldStyleJSONLOutput) — even though the underlying mechanism changed
// completely, from buffering everything to writing row by row.
func TestExportJSONLStreamingMatchesOldMaterializedOutput(t *testing.T) {
	seedExportStore(t)
	want := oldStyleJSONLOutput(t, store.MessageFilter{})

	var runErr error
	got := captureStdout(t, func() {
		runErr = runRoot(t, "message", "export", "--format", "jsonl")
	})
	if runErr != nil {
		t.Fatalf("export: %v", runErr)
	}

	if got != want {
		t.Errorf("streamed jsonl stdout differs from the old materialized-then-grouped output:\ngot:  %q\nwant: %q", got, want)
	}
}

// TestExportJSONLOutputFileStreamingMatchesOldMaterializedOutput is the
// --output counterpart: writeExportFileJSONL must publish the exact same
// bytes writeJSONExportFile-via-groups would have for --format jsonl,
// despite streaming through store.Store.ForEachMessage instead.
func TestExportJSONLOutputFileStreamingMatchesOldMaterializedOutput(t *testing.T) {
	seedExportStore(t)
	want := oldStyleJSONLOutput(t, store.MessageFilter{})

	out := filepath.Join(t.TempDir(), "export.jsonl")
	if err := runRoot(t, "message", "export", "--format", "jsonl", "--output", out); err != nil {
		t.Fatalf("export: %v", err)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Errorf("streamed jsonl file differs from the old materialized-then-grouped output:\ngot:  %q\nwant: %q", got, want)
	}
}

func readJSONLines(t *testing.T, path string) []string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var lines []string
	s := bufio.NewScanner(f)
	for s.Scan() {
		if s.Text() != "" {
			lines = append(lines, s.Text())
		}
	}
	return lines
}

// TestExportJSONLZeroMatchPreservesExistingArchive covers a data-loss
// ordering bug: the empty temp file must never be renamed over an existing
// archive before the match count is known. Publishing first and removing
// after meant a filtered re-export matching nothing destroyed the previous
// export and left only an error behind.
func TestExportJSONLZeroMatchPreservesExistingArchive(t *testing.T) {
	seedExportStore(t)
	out := filepath.Join(t.TempDir(), "archive.jsonl")

	if err := runRoot(t, "message", "export", "--format", "jsonl", "--output", out); err != nil {
		t.Fatalf("initial export: %v", err)
	}
	before, err := os.ReadFile(out)
	if err != nil || len(before) == 0 {
		t.Fatalf("initial archive missing: %v", err)
	}

	// A filter that matches nothing must leave the archive untouched.
	err = runRoot(t, "message", "export", "--format", "jsonl", "--output", out, "--conversation", "no-such-conversation")
	if err == nil {
		t.Fatal("zero-match export should error")
	}
	after, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("previous archive was destroyed: %v", err)
	}
	if string(after) != string(before) {
		t.Error("previous archive was modified by a zero-match export")
	}
}
