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

// TestExportOutputDirectoryLayoutAndModes is the required --output
// directory-layout-and-modes test.
func TestExportOutputDirectoryLayoutAndModes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix permission bits don't apply on windows")
	}
	seedExportStore(t)
	// A trailing separator is what tells isDirTarget to create dir as a
	// directory rather than write it as a single output file — the same
	// disambiguation rule documented in `message export --help`, needed
	// here because the path doesn't exist yet for os.Stat to identify as a
	// directory on its own.
	dir := filepath.Join(t.TempDir(), "archive") + string(filepath.Separator)

	if err := runRoot(t, "message", "export", "--output", dir); err != nil {
		t.Fatalf("export: %v", err)
	}

	di, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if perm := di.Mode().Perm(); perm != 0o700 {
		t.Errorf("archive dir mode = %o, want 0700", perm)
	}

	convFile := filepath.Join(dir, "conversations.jsonl")
	fi, err := os.Stat(convFile)
	if err != nil {
		t.Fatalf("conversations.jsonl missing: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("conversations.jsonl mode = %o, want 0600", perm)
	}

	lines := readJSONLines(t, convFile)
	if len(lines) != 2 {
		t.Fatalf("conversations.jsonl lines = %d, want 2", len(lines))
	}

	msgFile := filepath.Join(dir, "messages", "c1.jsonl")
	mi, err := os.Stat(msgFile)
	if err != nil {
		t.Fatalf("messages/c1.jsonl missing: %v", err)
	}
	if perm := mi.Mode().Perm(); perm != 0o600 {
		t.Errorf("messages/c1.jsonl mode = %o, want 0600", perm)
	}
	msgLines := readJSONLines(t, msgFile)
	if len(msgLines) != 2 {
		t.Errorf("messages/c1.jsonl lines = %d, want 2", len(msgLines))
	}
}

// TestExportOutputDirectoryPreservesPreexistingMode is the Theme A
// regression test: an --output directory that already existed (e.g. a
// shared directory the caller reused) must keep its own mode — export must
// only chmod a directory it actually created.
func TestExportOutputDirectoryPreservesPreexistingMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix permission bits don't apply on windows")
	}
	seedExportStore(t)
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := runRoot(t, "message", "export", "--output", dir); err != nil {
		t.Fatalf("export: %v", err)
	}

	di, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if perm := di.Mode().Perm(); perm != 0o755 {
		t.Errorf("pre-existing --output dir mode = %o, want unchanged 0755", perm)
	}
}

// TestExportOutputDirectoryDropsStaleFiles is the required Theme B test: a
// full export followed by a filtered export into the same --output
// directory must not leave the excluded conversation's file behind —
// copying or sharing the archive afterward would otherwise leak exactly the
// messages the second export's filter was meant to exclude.
func TestExportOutputDirectoryDropsStaleFiles(t *testing.T) {
	seedExportStore(t)
	dir := filepath.Join(t.TempDir(), "archive") + string(filepath.Separator)

	if err := runRoot(t, "message", "export", "--output", dir); err != nil {
		t.Fatalf("full export: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "messages", "c2.jsonl")); err != nil {
		t.Fatalf("messages/c2.jsonl missing after the full export: %v", err)
	}

	if err := runRoot(t, "message", "export", "--conversation", "c1", "--output", dir); err != nil {
		t.Fatalf("filtered export: %v", err)
	}

	if _, err := os.Stat(filepath.Join(dir, "messages", "c2.jsonl")); !os.IsNotExist(err) {
		t.Errorf("messages/c2.jsonl still present after a filtered re-export excluded c2 (err = %v)", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "messages", "c1.jsonl")); err != nil {
		t.Errorf("messages/c1.jsonl missing after the filtered export: %v", err)
	}

	lines := readJSONLines(t, filepath.Join(dir, "conversations.jsonl"))
	if len(lines) != 1 {
		t.Errorf("conversations.jsonl lines = %d, want 1 (only c1 survives the filter)", len(lines))
	}
}

// TestExportRefusesUnownedMessagesDirectory is the required regression test
// for the RemoveAll-destroys-unowned-directory defect: --output accepts any
// existing directory, and export used to unconditionally RemoveAll
// dir/messages before staging its own copy in. Pointing --output at a
// directory that already has an unrelated messages/ folder (never created
// by lion — no export marker at the directory root) must refuse the export
// outright, leaving that folder and everything in it untouched.
func TestExportRefusesUnownedMessagesDirectory(t *testing.T) {
	seedExportStore(t)
	dir := t.TempDir()
	unrelatedMessages := filepath.Join(dir, "messages")
	if err := os.MkdirAll(unrelatedMessages, 0o700); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(unrelatedMessages, "not-lions.txt")
	if err := os.WriteFile(sentinel, []byte("someone else's file"), 0o600); err != nil {
		t.Fatal(err)
	}

	err := runRoot(t, "message", "export", "--output", dir)
	if err == nil {
		t.Fatal("expected export to refuse a directory with a pre-existing, unmarked messages/ folder")
	}
	if exitCode(err) != ExitUsage {
		t.Errorf("exitCode(%v) = %d, want ExitUsage", err, exitCode(err))
	}

	// Nothing about the unrelated directory must have been touched.
	b, statErr := os.ReadFile(sentinel)
	if statErr != nil {
		t.Fatalf("sentinel file destroyed by the refused export: %v", statErr)
	}
	if string(b) != "someone else's file" {
		t.Errorf("sentinel file contents = %q, want unchanged", b)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "conversations.jsonl")); !os.IsNotExist(statErr) {
		t.Error("conversations.jsonl was written despite the refusal — nothing should be written on refusal")
	}
}

// TestExportReplacesOwnedMessagesDirectoryCleanly is the required companion
// test: a directory lion itself previously exported into (the marker is
// present) must still be replaced cleanly on a later export, with no stale
// files left over — the marker gates destruction of an UNOWNED directory,
// it must not block lion from re-exporting into its own.
func TestExportReplacesOwnedMessagesDirectoryCleanly(t *testing.T) {
	seedExportStore(t)
	dir := t.TempDir()

	if err := runRoot(t, "message", "export", "--output", dir); err != nil {
		t.Fatalf("first export: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, exportMarkerFilename)); err != nil {
		t.Fatalf("export marker missing after the first export: %v", err)
	}
	staleFile := filepath.Join(dir, "messages", "leftover-from-a-prior-lion-run.jsonl")
	if err := os.WriteFile(staleFile, []byte(`{"stale":true}`), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := runRoot(t, "message", "export", "--output", dir); err != nil {
		t.Fatalf("second export into the same, lion-owned directory: %v", err)
	}

	if _, err := os.Stat(staleFile); !os.IsNotExist(err) {
		t.Errorf("stale file survived a re-export into a lion-owned directory (err = %v)", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "messages", "c1.jsonl")); err != nil {
		t.Errorf("messages/c1.jsonl missing after the re-export: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "messages", "c2.jsonl")); err != nil {
		t.Errorf("messages/c2.jsonl missing after the re-export: %v", err)
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
