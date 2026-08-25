package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
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
	if _, err := os.Stat(filepath.Join(dir, "messages", exportMarkerFilename)); err != nil {
		t.Fatalf("export marker missing from messages/ after the first export: %v", err)
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

// TestExportRefusesCorruptOrForeignMarker is a required regression test for
// the stale/copied-marker defect: a messages/ directory that does carry a
// file at the marker path, but one that's unparseable or doesn't identify
// itself as lion's own (wrong format/version), must be refused exactly like
// a directory with no marker at all — the marker's mere presence must never
// be enough to trust it, since a copied or hand-crafted file at that path
// costs an attacker (or a stale leftover) nothing to produce.
func TestExportRefusesCorruptOrForeignMarker(t *testing.T) {
	for name, contents := range map[string]string{
		"corrupt JSON":   `{"format": "lion-message-export", "version": 1,`, // truncated
		"foreign format": `{"format": "some-other-tool", "version": 1, "updated_at": "2026-01-01T00:00:00Z"}`,
		"wrong version":  fmt.Sprintf(`{"format": %q, "version": 99, "updated_at": "2026-01-01T00:00:00Z"}`, exportMarkerFormat),
	} {
		t.Run(name, func(t *testing.T) {
			seedExportStore(t)
			dir := t.TempDir()
			messagesDir := filepath.Join(dir, "messages")
			if err := os.MkdirAll(messagesDir, 0o700); err != nil {
				t.Fatal(err)
			}
			sentinel := filepath.Join(messagesDir, "not-lions.txt")
			if err := os.WriteFile(sentinel, []byte("someone else's file"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(messagesDir, exportMarkerFilename), []byte(contents), 0o600); err != nil {
				t.Fatal(err)
			}

			err := runRoot(t, "message", "export", "--output", dir)
			if err == nil {
				t.Fatal("expected export to refuse a messages/ directory with a corrupt/foreign marker")
			}
			if exitCode(err) != ExitUsage {
				t.Errorf("exitCode(%v) = %d, want ExitUsage", err, exitCode(err))
			}
			if _, statErr := os.Stat(sentinel); statErr != nil {
				t.Errorf("sentinel file destroyed by the refused export: %v", statErr)
			}
		})
	}
}

// TestExportRefusesAfterMessagesDirReplacedWithUnrelatedData is the required
// regression test for the stale-root-marker defect: a marker that lived at
// the --output directory's root (rather than inside messages/ itself)
// survived the user deleting messages/ and dropping unrelated data in its
// place, so the next export would RemoveAll that unrelated data on the
// strength of a marker that no longer described what was actually there.
// Moving the marker inside messages/ means deleting messages/ deletes the
// marker with it, so a later export into the same --output directory must
// refuse rather than destroy whatever now occupies that path.
func TestExportRefusesAfterMessagesDirReplacedWithUnrelatedData(t *testing.T) {
	seedExportStore(t)
	dir := t.TempDir()

	if err := runRoot(t, "message", "export", "--output", dir); err != nil {
		t.Fatalf("first export: %v", err)
	}
	messagesDir := filepath.Join(dir, "messages")
	if err := os.RemoveAll(messagesDir); err != nil {
		t.Fatalf("remove messages/: %v", err)
	}
	if err := os.MkdirAll(messagesDir, 0o700); err != nil {
		t.Fatal(err)
	}
	unrelated := filepath.Join(messagesDir, "not-lions-either.txt")
	if err := os.WriteFile(unrelated, []byte("unrelated data the user put here"), 0o600); err != nil {
		t.Fatal(err)
	}

	err := runRoot(t, "message", "export", "--output", dir)
	if err == nil {
		t.Fatal("expected the second export to refuse: messages/ was replaced with data lion never marked")
	}
	if exitCode(err) != ExitUsage {
		t.Errorf("exitCode(%v) = %d, want ExitUsage", err, exitCode(err))
	}
	b, statErr := os.ReadFile(unrelated)
	if statErr != nil {
		t.Fatalf("unrelated data destroyed by the refused export: %v", statErr)
	}
	if string(b) != "unrelated data the user put here" {
		t.Errorf("unrelated file contents = %q, want unchanged", b)
	}
}

// TestExportRefusesUnownedConversationsFile is a regression test for the
// unchecked-conversations.jsonl defect: the ownership check used to cover
// only messages/, so an --output directory holding an unrelated, pre-
// existing conversations.jsonl (no lion marker anywhere) had it silently
// truncated and overwritten. Ownership must be judged on the whole archive
// layout, not just the messages/ subtree, so this must be refused with the
// pre-existing file left byte-for-byte untouched.
func TestExportRefusesUnownedConversationsFile(t *testing.T) {
	seedExportStore(t)
	dir := t.TempDir()
	convFile := filepath.Join(dir, "conversations.jsonl")
	original := "someone else's conversations.jsonl, nothing to do with lion"
	if err := os.WriteFile(convFile, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}

	err := runRoot(t, "message", "export", "--output", dir)
	if err == nil {
		t.Fatal("expected export to refuse a directory with a pre-existing, unmarked conversations.jsonl")
	}
	if exitCode(err) != ExitUsage {
		t.Errorf("exitCode(%v) = %d, want ExitUsage", err, exitCode(err))
	}

	b, statErr := os.ReadFile(convFile)
	if statErr != nil {
		t.Fatalf("conversations.jsonl destroyed by the refused export: %v", statErr)
	}
	if string(b) != original {
		t.Errorf("conversations.jsonl contents = %q, want unchanged %q", b, original)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "messages")); !os.IsNotExist(statErr) {
		t.Error("messages/ was written despite the refusal — nothing should be written on refusal")
	}
}

// TestExportRefusesSymlinkConversationsFile is a regression test for the
// symlink-follows-through-OpenFile defect: OpenFile(O_TRUNC) follows
// symlinks, so pointing conversations.jsonl at a symlink to an arbitrary
// file the exporting user can write let a shared --output directory be
// used to destroy that file's contents. The export must refuse outright
// (no marker vouches for this directory) and must never even attempt to
// open the symlink's target, let alone truncate it.
func TestExportRefusesSymlinkConversationsFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need elevated privileges to create on windows")
	}
	seedExportStore(t)
	dir := t.TempDir()

	target := filepath.Join(t.TempDir(), "victim.txt")
	victimContents := "this file must survive the export untouched"
	if err := os.WriteFile(target, []byte(victimContents), 0o600); err != nil {
		t.Fatal(err)
	}

	convFile := filepath.Join(dir, "conversations.jsonl")
	if err := os.Symlink(target, convFile); err != nil {
		t.Fatal(err)
	}

	err := runRoot(t, "message", "export", "--output", dir)
	if err == nil {
		t.Fatal("expected export to refuse a directory whose conversations.jsonl is a symlink to an unowned file")
	}
	if exitCode(err) != ExitUsage {
		t.Errorf("exitCode(%v) = %d, want ExitUsage", err, exitCode(err))
	}

	b, statErr := os.ReadFile(target)
	if statErr != nil {
		t.Fatalf("symlink target destroyed by the refused export: %v", statErr)
	}
	if string(b) != victimContents {
		t.Errorf("symlink target contents = %q, want unchanged %q", b, victimContents)
	}
	fi, lstatErr := os.Lstat(convFile)
	if lstatErr != nil {
		t.Fatalf("symlink itself was removed by the refused export: %v", lstatErr)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Error("conversations.jsonl was replaced with a regular file instead of being left as the original symlink")
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
