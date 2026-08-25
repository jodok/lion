package cli

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/jodok/lion/internal/ratelimit"
	"github.com/jodok/lion/internal/store"
	"github.com/jodok/lion/internal/voyager"
)

// routeFixtureTransport serves canned JSON responses per URL path (the
// query string, which carries the pagination cursor, is ignored for
// routing purposes), one response per call to that path, repeating the
// last response once a route's queue is exhausted. This mirrors
// internal/voyager's own sequenceTransport test helper, but keyed by path
// rather than call order, since a sync pass interleaves conversation
// discovery calls with several different conversations' message calls.
type routeFixtureTransport struct {
	routes map[string][]string
	calls  map[string]int
	// errOnCall, if set for a path, makes that path's Nth call (0-indexed)
	// return err instead of a fixture body — used to simulate a mid-run
	// failure (e.g. rate limiting) partway through a sync.
	errOnCall map[string]int
	err       error
}

func newRouteFixtureTransport() *routeFixtureTransport {
	return &routeFixtureTransport{routes: map[string][]string{}, calls: map[string]int{}, errOnCall: map[string]int{}}
}

func (r *routeFixtureTransport) on(path string, bodies ...string) *routeFixtureTransport {
	r.routes[path] = bodies
	return r
}

func (r *routeFixtureTransport) failOnCall(path string, call int, err error) *routeFixtureTransport {
	r.errOnCall[path] = call
	r.err = err
	return r
}

func (r *routeFixtureTransport) Do(_ context.Context, req *voyager.Request) (*voyager.Response, error) {
	u, err := url.Parse(req.URL)
	if err != nil {
		return nil, err
	}
	// Requests go to https://www.linkedin.com/voyager/api<path>?<query>;
	// routes are registered without that host/prefix (matching the path
	// argument voyager.Client's own get()/getRawQuery use), so strip it
	// before matching.
	path := strings.TrimPrefix(u.Path, "/voyager/api")

	i := r.calls[path]
	r.calls[path]++

	// >= (not ==): a real failure mode this simulates (rate limiting,
	// session expiry) doesn't clear itself on GET's built-in one-shot
	// retry (see voyager.Client.do), so the fixture shouldn't either — a
	// one-call-only failure would let that retry silently mask the
	// scenario this test exists to cover.
	if failAt, ok := r.errOnCall[path]; ok && i >= failAt {
		return nil, r.err
	}

	bodies, ok := r.routes[path]
	if !ok || len(bodies) == 0 {
		return &voyager.Response{StatusCode: 404, Body: []byte("no fixture for " + path)}, nil
	}
	if i >= len(bodies) {
		i = len(bodies) - 1
	}
	return &voyager.Response{StatusCode: 200, Body: []byte(bodies[i])}, nil
}

func (r *routeFixtureTransport) callCount(path string) int {
	return r.calls[path]
}

// newFixtureClient builds a voyager.Client wired to rt with rate-limiting
// disabled: an empty budgets map makes every class a no-op in
// ratelimit.Limiter.Wait, so a test paging through several fixture pages
// doesn't pay real inter-action jitter.
func newFixtureClient(rt *routeFixtureTransport) *voyager.Client {
	return voyager.New("li_at_test", `"jsession_test"`,
		voyager.WithTransport(rt),
		voyager.WithLimiter(ratelimit.New(map[ratelimit.Class]ratelimit.Budget{})))
}

// discardProgress returns a progressReporter writing to /dev/null, for
// tests that exercise sync's logic directly and don't care about its
// stderr narration.
func discardProgress(t *testing.T) *progressReporter {
	t.Helper()
	f, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return newProgressReporter(f, false)
}

// openSyncTestStore isolates LION_HOME and opens the default store path
// under it, for tests that call runSyncPass/discoverConversations/etc.
// directly rather than through the full command dispatch.
func openSyncTestStore(t *testing.T) *store.Store {
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
	t.Cleanup(func() { st.Close() })
	return st
}

func conversationsPageJSON(entries [][2]any) string {
	els := make([]string, 0, len(entries))
	for _, e := range entries {
		id, last := e[0].(string), e[1].(int64)
		els = append(els, fmt.Sprintf(
			`{"entityUrn":"urn:li:fs_conversation:%s","unread":false,"lastActivityAt":%d,"participants":[{"miniProfile":{"entityUrn":"urn:li:fs_miniProfile:p1"}}],"events":[]}`,
			id, last))
	}
	return fmt.Sprintf(`{"data":{"elements":[%s]}}`, strings.Join(els, ","))
}

func messagesPageJSON(entries [][2]any) string {
	els := make([]string, 0, len(entries))
	for _, e := range entries {
		urn, sent := e[0].(string), e[1].(int64)
		els = append(els, fmt.Sprintf(
			`{"entityUrn":"%s","createdAt":%d,"from":{"miniProfile":{"entityUrn":"urn:li:fs_miniProfile:p1"}},"eventContent":{"com.linkedin.voyager.messaging.event.MessageEvent":{"body":"hi"}}}`,
			urn, sent))
	}
	return fmt.Sprintf(`{"data":{"elements":[%s]}}`, strings.Join(els, ","))
}

// TestSyncPopulatesStore is the required "populates the store" test: a
// fixture transport serving a multi-page conversation-with-messages fixture
// must leave the store holding every conversation and message, and report a
// complete, accurate summary.
func TestSyncPopulatesStore(t *testing.T) {
	st := openSyncTestStore(t)
	rt := newRouteFixtureTransport().
		on("/messaging/conversations",
			conversationsPageJSON([][2]any{{"c1", int64(5000)}}),
			conversationsPageJSON(nil)).
		on("/messaging/conversations/c1/events",
			messagesPageJSON([][2]any{{"m2", int64(200)}, {"m1", int64(100)}}),
			messagesPageJSON(nil))
	cl := newFixtureClient(rt)

	summary, err := runSyncPass(context.Background(), cl, st, syncOptions{}, discardProgress(t))
	if err != nil {
		t.Fatalf("runSyncPass: %v", err)
	}
	if !summary.Complete {
		t.Error("Complete = false, want true")
	}
	if summary.ConversationsSeen != 1 || summary.ConversationsUpdated != 1 || summary.MessagesAdded != 2 {
		t.Errorf("summary = %+v, want 1 seen, 1 updated, 2 added", summary)
	}

	conv, ok, err := st.Conversation(context.Background(), "c1")
	if err != nil || !ok {
		t.Fatalf("Conversation: ok=%v err=%v", ok, err)
	}
	if conv.NewestSynced == nil || *conv.NewestSynced != 200 {
		t.Errorf("NewestSynced = %v, want 200", conv.NewestSynced)
	}
	msgs, err := st.Messages(context.Background(), store.MessageFilter{ConversationID: "c1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 {
		t.Fatalf("stored messages = %d, want 2", len(msgs))
	}
}

// TestSyncSecondRunIsIdempotent is the required idempotence test: running
// the exact same fixture through sync twice must add nothing the second
// time — the property that makes resumption and --follow safe.
func TestSyncSecondRunIsIdempotent(t *testing.T) {
	st := openSyncTestStore(t)
	newRT := func() *voyager.Client {
		rt := newRouteFixtureTransport().
			on("/messaging/conversations",
				conversationsPageJSON([][2]any{{"c1", int64(5000)}}),
				conversationsPageJSON(nil)).
			on("/messaging/conversations/c1/events",
				messagesPageJSON([][2]any{{"m1", int64(100)}}),
				messagesPageJSON(nil))
		return newFixtureClient(rt)
	}

	first, err := runSyncPass(context.Background(), newRT(), st, syncOptions{}, discardProgress(t))
	if err != nil {
		t.Fatal(err)
	}
	if first.MessagesAdded != 1 {
		t.Fatalf("first run messages_added = %d, want 1", first.MessagesAdded)
	}

	second, err := runSyncPass(context.Background(), newRT(), st, syncOptions{}, discardProgress(t))
	if err != nil {
		t.Fatal(err)
	}
	if second.MessagesAdded != 0 {
		t.Errorf("second run messages_added = %d, want 0 (idempotent re-sync)", second.MessagesAdded)
	}
	if second.ConversationsUpdated != 0 {
		t.Errorf("second run conversations_updated = %d, want 0", second.ConversationsUpdated)
	}
}

// TestSyncCatchUpStopsAtKnownMessage is the required catch-up test: once a
// fetched page comes back with a message already in the store, catch-up
// must stop rather than keep paging further back.
func TestSyncCatchUpStopsAtKnownMessage(t *testing.T) {
	st := openSyncTestStore(t)

	// Pre-seed the store as if a prior sync already recorded m1.
	err := st.WithTx(context.Background(), func(tx *store.Tx) error {
		if err := tx.UpsertConversation(context.Background(), store.Conversation{ID: "c1", URN: "urn:li:fs_conversation:c1", UpdatedAt: 100}, 1); err != nil {
			return err
		}
		_, err := tx.RecordMessagePage(context.Background(), "c1", []store.Message{{URN: "m1", ConversationID: "c1", SentAt: 100}}, 1)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}

	// The newest page mixes one genuinely new message (m2) with the
	// already-known m1 — catch-up must stop after this one page.
	rt := newRouteFixtureTransport().
		on("/messaging/conversations",
			conversationsPageJSON([][2]any{{"c1", int64(5000)}}),
			conversationsPageJSON(nil)).
		on("/messaging/conversations/c1/events",
			messagesPageJSON([][2]any{{"m2", int64(200)}, {"m1", int64(100)}}),
			// A second page would only be fetched if catch-up incorrectly
			// kept paging; make it look like more new history so the test
			// fails loudly (extra messages_added) rather than silently.
			messagesPageJSON([][2]any{{"m0", int64(50)}}))
	cl := newFixtureClient(rt)

	summary, err := runSyncPass(context.Background(), cl, st, syncOptions{}, discardProgress(t))
	if err != nil {
		t.Fatal(err)
	}
	if summary.MessagesAdded != 1 {
		t.Errorf("messages_added = %d, want 1 (only m2 is new)", summary.MessagesAdded)
	}
	if got := rt.callCount("/messaging/conversations/c1/events"); got != 1 {
		t.Errorf("events endpoint called %d times, want 1 (catch-up must stop at the known message)", got)
	}
}

// TestSyncBackfillReachesEndAndSetsFlag is the required --backfill test:
// paging backwards must continue until an empty page, at which point
// BackfillDone must be set.
func TestSyncBackfillReachesEndAndSetsFlag(t *testing.T) {
	st := openSyncTestStore(t)
	rt := newRouteFixtureTransport().
		on("/messaging/conversations",
			conversationsPageJSON([][2]any{{"c1", int64(5000)}}),
			conversationsPageJSON(nil)).
		on("/messaging/conversations/c1/events",
			// Catch-up's first (and only) page...
			messagesPageJSON([][2]any{{"m2", int64(200)}}),
			// ...then backfill continues from OldestSynced=200 downward:
			messagesPageJSON([][2]any{{"m1", int64(100)}}),
			messagesPageJSON(nil)) // the terminal empty page
	cl := newFixtureClient(rt)

	summary, err := runSyncPass(context.Background(), cl, st, syncOptions{backfill: true}, discardProgress(t))
	if err != nil {
		t.Fatal(err)
	}
	if summary.MessagesAdded != 2 {
		t.Fatalf("messages_added = %d, want 2 (m2 from catch-up, m1 from backfill)", summary.MessagesAdded)
	}
	if !summary.Complete {
		t.Error("Complete = false, want true")
	}

	conv, ok, err := st.Conversation(context.Background(), "c1")
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

// TestSyncMidRunErrorLeavesConsistentStoreAndReportsIncomplete is the
// required partial-run test: an error partway through a sync must (a)
// leave the store holding exactly what was durably committed before the
// failure — no more, no less — (b) report complete:false, and (c) return
// the error so the caller's exit code is right.
func TestSyncMidRunErrorLeavesConsistentStoreAndReportsIncomplete(t *testing.T) {
	st := openSyncTestStore(t)
	boom := errors.New("simulated rate limit")
	rt := newRouteFixtureTransport().
		on("/messaging/conversations",
			conversationsPageJSON([][2]any{{"c1", int64(5000)}, {"c2", int64(4000)}}),
			conversationsPageJSON(nil)).
		on("/messaging/conversations/c1/events",
			messagesPageJSON([][2]any{{"m1", int64(100)}}),
			messagesPageJSON(nil)).
		on("/messaging/conversations/c2/events",
			messagesPageJSON([][2]any{{"m2", int64(100)}}))
	// c2's very first page fails outright, simulating a rate limit hit
	// partway through the run (c1 must have already been fully processed).
	rt.failOnCall("/messaging/conversations/c2/events", 0, boom)
	cl := newFixtureClient(rt)

	summary, err := runSyncPass(context.Background(), cl, st, syncOptions{}, discardProgress(t))
	if err == nil {
		t.Fatal("expected an error from the failing conversation")
	}
	if !errors.Is(err, boom) {
		t.Errorf("err = %v, want it to wrap the simulated failure", err)
	}
	if summary.Complete {
		t.Error("Complete = true, want false after a mid-run failure")
	}

	// c1's page committed before the failure must still be there.
	c1msgs, err := st.Messages(context.Background(), store.MessageFilter{ConversationID: "c1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(c1msgs) != 1 {
		t.Errorf("c1 messages = %d, want 1 (committed before the failure)", len(c1msgs))
	}
	// c2 never got a chance to commit anything.
	c2msgs, err := st.Messages(context.Background(), store.MessageFilter{ConversationID: "c2"})
	if err != nil {
		t.Fatal(err)
	}
	if len(c2msgs) != 0 {
		t.Errorf("c2 messages = %d, want 0 (the failing page must not have partially committed)", len(c2msgs))
	}
}

// TestSyncOnceAndFollowAreMutuallyExclusive covers the flag-validation half
// of the --once/--follow wacli-compat flags, at the full command-dispatch
// level (no network needed since this fails before building a client).
func TestSyncOnceAndFollowAreMutuallyExclusive(t *testing.T) {
	err := execRoot(t, "sync", "--once", "--follow")
	if err == nil {
		t.Fatal("expected a usage error")
	}
	if exitCode(err) != ExitUsage {
		t.Errorf("exitCode(%v) = %d, want ExitUsage", err, exitCode(err))
	}
}

// TestParseSizeUnits pins --max-db-size's grammar.
func TestParseSizeUnits(t *testing.T) {
	cases := map[string]int64{
		"0":     0,
		"512":   512,
		"1KB":   1024,
		"500MB": 500 * (1 << 20),
		"2GB":   2 * (1 << 30),
		"1.5MB": int64(1.5 * (1 << 20)),
		"100kb": 100 * (1 << 10), // case-insensitive
	}
	for in, want := range cases {
		got, err := parseSize(in)
		if err != nil {
			t.Errorf("parseSize(%q) error: %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("parseSize(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestParseSizeRejectsGarbage(t *testing.T) {
	if _, err := parseSize("not-a-size"); err == nil {
		t.Error("expected an error for a garbage size")
	}
}

// TestParseTimeFlagAcceptsBothForms pins --after/--before's grammar.
func TestParseTimeFlagAcceptsBothForms(t *testing.T) {
	if _, err := parseTimeFlag("after", "2026-01-02T15:04:05Z"); err != nil {
		t.Errorf("RFC3339: %v", err)
	}
	ms, err := parseTimeFlag("after", "2026-01-02")
	if err != nil {
		t.Fatalf("YYYY-MM-DD: %v", err)
	}
	if ms <= 0 {
		t.Errorf("date-only parse = %d, want a positive epoch-ms value", ms)
	}
	if _, err := parseTimeFlag("after", "not-a-date"); err == nil {
		t.Error("expected an error for a garbage date")
	}
}
