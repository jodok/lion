package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/jodok/lion/internal/store"
)

// TestSearchFindsMatch is the required FTS-match-found test.
func TestSearchFindsMatch(t *testing.T) {
	seedExportStore(t) // c1: "hello there"/"hi Ada"; c2: "coffee tomorrow?"

	var runErr error
	var stderr string
	out := captureStdout(t, func() {
		stderr = captureStderr(t, func() {
			runErr = runRoot(t, "message", "search", "coffee", "--json")
		})
	})
	if runErr != nil {
		t.Fatalf("search: %v", runErr)
	}
	if !strings.Contains(stderr, "1 match") {
		t.Errorf("stderr = %q, want it to report 1 match", stderr)
	}

	var hits []searchHit
	if err := json.Unmarshal([]byte(out), &hits); err != nil {
		t.Fatalf("output not valid JSON: %v\noutput: %s", err, out)
	}
	if len(hits) != 1 || hits[0].Body != "coffee tomorrow?" {
		t.Errorf("hits = %+v, want exactly the coffee message", hits)
	}
}

// TestSearchNoMatchIsNotError is the required "not found" case: a query
// with zero matches must not be an error and must print nothing on stdout.
func TestSearchNoMatchIsNotError(t *testing.T) {
	seedExportStore(t)

	var runErr error
	var stderr string
	out := captureStdout(t, func() {
		stderr = captureStderr(t, func() {
			runErr = runRoot(t, "message", "search", "spaceship")
		})
	})
	if runErr != nil {
		t.Fatalf("a zero-match search must not be an error, got %v", runErr)
	}
	if out != "" {
		t.Errorf("stdout = %q, want empty for zero matches", out)
	}
	if !strings.Contains(stderr, "0 match") {
		t.Errorf("stderr = %q, want it to report 0 matches", stderr)
	}
}

// TestSearchMissingStoreErrors is the required missing-store test: search
// against an empty/never-synced store must error, distinctly from a
// zero-match search (see TestSearchNoMatchIsNotError) — the whole point of
// keeping these two outcomes apart.
func TestSearchMissingStoreErrors(t *testing.T) {
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
		err = runRoot(t, "message", "search", "coffee")
	})
	if err == nil {
		t.Fatal("expected an error for a missing/empty store")
	}
	if !strings.Contains(stderr, "sync") {
		t.Errorf("stderr = %q, want it to point at `lion sync`", stderr)
	}
}

// TestSearchFiltersNarrowResults is the required filter test at the CLI
// level: --conversation, --from, --after/--before, and --limit each narrow
// the result set (store-level filter correctness is covered directly in
// internal/store's query_extra_test.go; this pins that the CLI actually
// wires the flags through).
func TestSearchFiltersNarrowResults(t *testing.T) {
	seedExportStore(t)

	get := func(args ...string) []searchHit {
		t.Helper()
		var runErr error
		out := captureStdout(t, func() {
			captureStderr(t, func() {
				runErr = runRoot(t, append([]string{"message", "search"}, args...)...)
			})
		})
		if runErr != nil {
			t.Fatalf("search %v: %v", args, runErr)
		}
		if out == "" {
			return nil
		}
		var hits []searchHit
		if err := json.Unmarshal([]byte(out), &hits); err != nil {
			t.Fatalf("output not valid JSON: %v\noutput: %s", err, out)
		}
		return hits
	}

	// --conversation restricts to c1 only ("hi Ada" lives there; "hi" isn't
	// present in c2's body).
	if hits := get("hi", "--conversation", "c1", "--json"); len(hits) != 1 || hits[0].Body != "hi Ada" {
		t.Errorf("--conversation c1: hits = %+v, want exactly [hi Ada]", hits)
	}
	if hits := get("hi", "--conversation", "c2", "--json"); len(hits) != 0 {
		t.Errorf("--conversation c2: hits = %+v, want none", hits)
	}

	// --from narrows by sender display name.
	if hits := get("hello", "--from", "Ada", "--json"); len(hits) != 1 || hits[0].Body != "hello there" {
		t.Errorf("--from Ada: hits = %+v, want exactly [hello there]", hits)
	}
	if hits := get("hello", "--from", "Grace", "--json"); len(hits) != 0 {
		t.Errorf("--from Grace: hits = %+v, want none (Grace never said hello)", hits)
	}

	// --after/--before bound sent_at. c2's "coffee tomorrow?" is at 300ms.
	if hits := get("coffee", "--after", "9999-01-01", "--json"); len(hits) != 0 {
		t.Errorf("--after in the far future: hits = %+v, want none", hits)
	}
	// RFC3339 epoch (0ms) is before c2's message (300ms), so --before that
	// instant must exclude it.
	if hits := get("coffee", "--before", "1970-01-01T00:00:00Z", "--json"); len(hits) != 0 {
		t.Errorf("--before the epoch: hits = %+v, want none (c2's message is at 300ms, after it)", hits)
	}

	// --limit caps the result count. "hello OR hi" (FTS5 boolean OR) hits
	// both of c1's messages ("hello there" and "hi Ada") via their bodies,
	// and neither sender name ("Ada Lovelace", "Me") contains either token,
	// so this is 2 hits unlimited, capped to 1 with --limit 1.
	if hits := get("hello OR hi", "--json"); len(hits) != 2 {
		t.Fatalf("unlimited 'hello OR hi': hits = %+v, want 2 (both c1 messages)", hits)
	}
	if hits := get("hello OR hi", "--limit", "1", "--json"); len(hits) != 1 {
		t.Errorf("--limit 1: got %d hits, want exactly 1", len(hits))
	}
}

// TestSearchWrapUntrustedWrapsBodyAndSender is the required
// --wrap-untrusted test: both the message body and the sender name must be
// wrapped, matching the codebase's established F17 convention (see
// untrusted.go) — a prior review caught exactly this class of gap
// elsewhere (Connection.Headline wrapped but not Connection.Name).
func TestSearchWrapUntrustedWrapsBodyAndSender(t *testing.T) {
	seedExportStore(t)

	var runErr error
	out := captureStdout(t, func() {
		captureStderr(t, func() {
			runErr = runRoot(t, "message", "search", "coffee", "--json", "--wrap-untrusted")
		})
	})
	if runErr != nil {
		t.Fatalf("search: %v", runErr)
	}
	var hits []searchHit
	if err := json.Unmarshal([]byte(out), &hits); err != nil {
		t.Fatalf("output not valid JSON: %v\noutput: %s", err, out)
	}
	if len(hits) != 1 {
		t.Fatalf("hits = %+v, want exactly 1", hits)
	}
	if !strings.Contains(hits[0].Body, "<untrusted nonce=") {
		t.Errorf("Body = %q, want it wrapped in an <untrusted> boundary", hits[0].Body)
	}
	if !strings.Contains(hits[0].SenderName, "<untrusted nonce=") {
		t.Errorf("SenderName = %q, want it wrapped in an <untrusted> boundary too", hits[0].SenderName)
	}
}

// TestSearchUnknownConversationErrors pins that an unknown --conversation is
// reported as an error, not silently treated as "no matches" (matching
// message export's identical guard).
func TestSearchUnknownConversationErrors(t *testing.T) {
	seedExportStore(t)
	err := runRoot(t, "message", "search", "coffee", "--conversation", "does-not-exist")
	if err == nil {
		t.Fatal("expected an error for an unknown --conversation")
	}
}

// TestSearchEmptyQueryIsUsageError pins that a blank query is rejected as a
// usage error (exit 2) rather than reaching the store at all.
func TestSearchEmptyQueryIsUsageError(t *testing.T) {
	seedExportStore(t)
	err := runRoot(t, "message", "search", "   ")
	if err == nil {
		t.Fatal("expected a usage error for a blank query")
	}
	if exitCode(err) != ExitUsage {
		t.Errorf("exitCode(%v) = %d, want ExitUsage", err, exitCode(err))
	}
}
