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

// TestSyncMaxConversationsTruncatesAndReportsIncomplete is the required
// --max-conversations completeness test: capping discovery below the number
// of conversations actually available must leave older ones undiscovered,
// so the pass must report complete:false even though nothing errored.
func TestSyncMaxConversationsTruncatesAndReportsIncomplete(t *testing.T) {
	st := openSyncTestStore(t)
	rt := newRouteFixtureTransport().
		on("/messaging/conversations",
			conversationsPageJSON([][2]any{{"c1", int64(5000)}, {"c2", int64(4000)}})).
		on("/messaging/conversations/c1/events", messagesPageJSON(nil))
	cl := newFixtureClient(rt)

	summary, err := runSyncPass(context.Background(), cl, st, syncOptions{maxConversations: 1}, discardProgress(t))
	if err != nil {
		t.Fatalf("runSyncPass: %v", err)
	}
	if summary.Complete {
		t.Error("Complete = true, want false: --max-conversations left c2 undiscovered")
	}
	if summary.ConversationsSeen != 1 {
		t.Errorf("ConversationsSeen = %d, want 1", summary.ConversationsSeen)
	}
}

// TestSyncMaxMessagesTruncatesAndReportsIncomplete is the required
// --max-messages completeness test, specifically the case where the budget
// is exhausted by the last (here, only) conversation's page: the outer
// per-conversation budget check never gets a "next" conversation to run
// against, so catchUpMessages itself must report the truncation.
func TestSyncMaxMessagesTruncatesAndReportsIncomplete(t *testing.T) {
	st := openSyncTestStore(t)
	rt := newRouteFixtureTransport().
		on("/messaging/conversations",
			conversationsPageJSON([][2]any{{"c1", int64(5000)}}),
			conversationsPageJSON(nil)).
		// The page exactly consumes the --max-messages=2 budget, and (unlike
		// a genuine "caught up" page) both messages are new, so nothing
		// signals a stopping point other than the budget itself.
		on("/messaging/conversations/c1/events",
			messagesPageJSON([][2]any{{"m2", int64(200)}, {"m1", int64(100)}}))
	cl := newFixtureClient(rt)

	summary, err := runSyncPass(context.Background(), cl, st, syncOptions{maxMessages: 2}, discardProgress(t))
	if err != nil {
		t.Fatalf("runSyncPass: %v", err)
	}
	if summary.Complete {
		t.Error("Complete = true, want false: --max-messages was exhausted mid-conversation with no further conversation to trip the outer budget check")
	}
	if summary.MessagesAdded != 2 {
		t.Errorf("MessagesAdded = %d, want 2", summary.MessagesAdded)
	}
}

// TestSyncStalledMessagesCursorReportsIncomplete is the required
// stalled-cursor completeness test: a conversation whose events endpoint
// stops honoring createdBefore partway through paging must not be reported
// as a complete pass, even though nothing returned an error.
func TestSyncStalledMessagesCursorReportsIncomplete(t *testing.T) {
	st := openSyncTestStore(t)
	rt := newRouteFixtureTransport().
		on("/messaging/conversations",
			conversationsPageJSON([][2]any{{"c1", int64(5000)}}),
			conversationsPageJSON(nil)).
		on("/messaging/conversations/c1/events",
			// First page: genuinely new, decreasing cursor (oldest=200).
			messagesPageJSON([][2]any{{"m1", int64(300)}, {"m2", int64(200)}}),
			// Second page: also new messages, but its oldest (220) does not
			// decrease below the createdBefore (200) just sent — the server
			// ignored the cursor, which is exactly the condition
			// MessagesPage's ErrPaginationStalled guard exists to catch.
			messagesPageJSON([][2]any{{"m3", int64(250)}, {"m4", int64(220)}}))
	cl := newFixtureClient(rt)

	summary, err := runSyncPass(context.Background(), cl, st, syncOptions{}, discardProgress(t))
	if err != nil {
		t.Fatalf("runSyncPass: %v", err)
	}
	if summary.Complete {
		t.Error("Complete = true, want false: the messages cursor stalled before the conversation's true start was reached")
	}
	if summary.MessagesAdded != 4 {
		t.Errorf("MessagesAdded = %d, want 4 (both pages' messages are genuinely new)", summary.MessagesAdded)
	}
}

// TestSyncPlainRerunAfterInterruptedInitialSyncReportsIncomplete is the
// required interrupted-initial-sync test: a first sync capped by
// --max-messages leaves older history unfetched (BackfillDone stays
// false). A later plain `sync` (no --backfill) re-fetches the same newest
// page — now entirely duplicates — and must not claim the pass complete
// just because that page contained no new messages: the older history
// below OldestSynced was never walked, plain sync never asked to walk it
// (--backfill is opt-in), and it remains permanently missing from any
// export unless the summary is honest about that.
func TestSyncPlainRerunAfterInterruptedInitialSyncReportsIncomplete(t *testing.T) {
	st := openSyncTestStore(t)

	// Run 1: capped mid-conversation, exactly like a real interrupted or
	// --max-messages-limited initial sync.
	rt1 := newRouteFixtureTransport().
		on("/messaging/conversations",
			conversationsPageJSON([][2]any{{"c1", int64(5000)}}),
			conversationsPageJSON(nil)).
		on("/messaging/conversations/c1/events",
			messagesPageJSON([][2]any{{"m4", int64(400)}, {"m3", int64(300)}}))
	first, err := runSyncPass(context.Background(), newFixtureClient(rt1), st, syncOptions{maxMessages: 2}, discardProgress(t))
	if err != nil {
		t.Fatalf("first (capped) run: %v", err)
	}
	if first.Complete {
		t.Error("first run Complete = true, want false: --max-messages cut it short")
	}
	if first.MessagesAdded != 2 {
		t.Fatalf("first run MessagesAdded = %d, want 2", first.MessagesAdded)
	}

	// Run 2: plain sync, no --max-messages, no --backfill. Nothing changed
	// on the server, so the newest page it fetches is exactly what run 1
	// already stored — entirely duplicates.
	rt2 := newRouteFixtureTransport().
		on("/messaging/conversations",
			conversationsPageJSON([][2]any{{"c1", int64(5000)}}),
			conversationsPageJSON(nil)).
		on("/messaging/conversations/c1/events",
			messagesPageJSON([][2]any{{"m4", int64(400)}, {"m3", int64(300)}}))
	second, err := runSyncPass(context.Background(), newFixtureClient(rt2), st, syncOptions{}, discardProgress(t))
	if err != nil {
		t.Fatalf("second (plain) run: %v", err)
	}
	if second.Complete {
		t.Error("second run Complete = true, want false: older history below OldestSynced was never fetched (BackfillDone is still false)")
	}
	if second.MessagesAdded != 0 {
		t.Errorf("second run MessagesAdded = %d, want 0 (the fetched page was entirely duplicates)", second.MessagesAdded)
	}
	if got := rt2.callCount("/messaging/conversations/c1/events"); got != 1 {
		t.Errorf("events endpoint called %d times, want 1 (plain sync must still stop at the duplicate page, not loop)", got)
	}

	conv, ok, err := st.Conversation(context.Background(), "c1")
	if err != nil || !ok {
		t.Fatalf("Conversation: ok=%v err=%v", ok, err)
	}
	if conv.BackfillDone {
		t.Error("BackfillDone = true, want false: paging never actually reached an empty page")
	}
}

// TestSyncBackfillResumesFromOldestSyncedAfterInterruptedInitialSync is the
// companion resumption test: once the caller does pass --backfill on a
// later run, sync must actually walk backward from the conversation's
// OldestSynced (not just flip a flag) and fetch the older history the
// interrupted first run never reached — and must still report the pass
// incomplete when that walk itself never reaches a genuine empty page.
func TestSyncBackfillResumesFromOldestSyncedAfterInterruptedInitialSync(t *testing.T) {
	st := openSyncTestStore(t)

	rt1 := newRouteFixtureTransport().
		on("/messaging/conversations",
			conversationsPageJSON([][2]any{{"c1", int64(5000)}}),
			conversationsPageJSON(nil)).
		on("/messaging/conversations/c1/events",
			messagesPageJSON([][2]any{{"m4", int64(400)}, {"m3", int64(300)}}))
	if _, err := runSyncPass(context.Background(), newFixtureClient(rt1), st, syncOptions{maxMessages: 2}, discardProgress(t)); err != nil {
		t.Fatalf("first (capped) run: %v", err)
	}

	rt2 := newRouteFixtureTransport().
		on("/messaging/conversations",
			conversationsPageJSON([][2]any{{"c1", int64(5000)}}),
			conversationsPageJSON(nil)).
		on("/messaging/conversations/c1/events",
			// Call 1 (catch-up, createdBefore=0): the same newest page run 1
			// already stored — entirely duplicates, so catch-up stops here
			// (see the plain-rerun test above) without ever calling this
			// route again itself.
			messagesPageJSON([][2]any{{"m4", int64(400)}, {"m3", int64(300)}}),
			// Call 2 (backfill, resuming from OldestSynced=300): genuinely
			// older, new-to-the-store messages.
			messagesPageJSON([][2]any{{"m2", int64(200)}, {"m1", int64(100)}}),
			// Call 3 (backfill continues from 100): the server re-serves the
			// same page instead of a true empty one — modeling a
			// conversation whose paging never actually reaches its true
			// start. MessagesPage's own stalled-cursor guard trips here
			// (oldest=100 no longer decreases below createdBefore=100),
			// which is what must end the walk, not a fabricated empty page.
			messagesPageJSON([][2]any{{"m2", int64(200)}, {"m1", int64(100)}}))
	second, err := runSyncPass(context.Background(), newFixtureClient(rt2), st, syncOptions{backfill: true}, discardProgress(t))
	if err != nil {
		t.Fatalf("second (backfill) run: %v", err)
	}
	if second.MessagesAdded != 2 {
		t.Errorf("second run MessagesAdded = %d, want 2 (m2 and m1, fetched by resuming from OldestSynced)", second.MessagesAdded)
	}
	if second.Complete {
		t.Error("second run Complete = true, want false: the backfill walk stalled before ever reaching a genuine empty page")
	}

	msgs, err := st.Messages(context.Background(), store.MessageFilter{ConversationID: "c1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 4 {
		t.Fatalf("stored messages for c1 = %d, want 4 (m1..m4): the second run must continue from the oldest stored message, not just re-confirm what run 1 already had", len(msgs))
	}

	conv, ok, err := st.Conversation(context.Background(), "c1")
	if err != nil || !ok {
		t.Fatalf("Conversation: ok=%v err=%v", ok, err)
	}
	if conv.OldestSynced == nil || *conv.OldestSynced != 100 {
		t.Errorf("OldestSynced = %v, want 100 (the walk reached m1 before stalling)", conv.OldestSynced)
	}
	if conv.BackfillDone {
		t.Error("BackfillDone = true, want false: paging never actually reached an empty page")
	}
}

// TestSyncStalledDuplicatePageIsNotTreatedAsCaughtUp is the required
// stalled-vs-duplicate-ordering test: a page that looks like a legitimate
// "caught up" duplicate (pageAdded < len(msgs)) but was ALSO served because
// the server ignored createdBefore (ErrPaginationStalled) must be reported
// incomplete. Checking the duplicate condition before stalled would treat
// the stall as a successful catch-up and silently stop looking for older
// history — exactly the failure mode the stalled guard exists to catch.
func TestSyncStalledDuplicatePageIsNotTreatedAsCaughtUp(t *testing.T) {
	st := openSyncTestStore(t)
	rt := newRouteFixtureTransport().
		on("/messaging/conversations",
			conversationsPageJSON([][2]any{{"c1", int64(5000)}}),
			conversationsPageJSON(nil)).
		on("/messaging/conversations/c1/events",
			// Call 1 (createdBefore=0, no stall guard active yet): both
			// messages are genuinely new.
			messagesPageJSON([][2]any{{"m1", int64(300)}, {"m2", int64(200)}}),
			// Call 2 (createdBefore=200): re-serves m2 alone. m2 is already
			// stored (from call 1), so this page is entirely duplicates
			// (pageAdded=0 < len=1) -- AND its oldest (200) does not
			// decrease below createdBefore (200), which is simultaneously
			// the exact ErrPaginationStalled condition.
			messagesPageJSON([][2]any{{"m2", int64(200)}}))
	cl := newFixtureClient(rt)

	summary, err := runSyncPass(context.Background(), cl, st, syncOptions{}, discardProgress(t))
	if err != nil {
		t.Fatalf("runSyncPass: %v", err)
	}
	if summary.Complete {
		t.Error("Complete = true, want false: the duplicate page was also a stalled cursor, not a genuine catch-up")
	}
	if summary.MessagesAdded != 2 {
		t.Errorf("MessagesAdded = %d, want 2 (m1 and m2, both added from call 1)", summary.MessagesAdded)
	}
	if got := rt.callCount("/messaging/conversations/c1/events"); got != 2 {
		t.Errorf("events endpoint called %d times, want 2 (must stop once the stall is detected, not loop)", got)
	}
}

// TestSyncAfterFiltersStoredMessagesButPaginationStillAdvances is the
// required --after correctness test: a single fetched page whose messages
// straddle the --after cutoff must store only the messages at or after it,
// while the pagination decisions (the cursor, whether the walk reached a
// genuine stopping point) still key off the page the server actually
// returned. Before this fix, the whole page was committed and --after was
// only ever consulted afterward to decide whether to fetch another page —
// so every older message on a straddling page silently landed in the store
// (and, from there, in any export).
func TestSyncAfterFiltersStoredMessagesButPaginationStillAdvances(t *testing.T) {
	st := openSyncTestStore(t)
	rt := newRouteFixtureTransport().
		on("/messaging/conversations",
			conversationsPageJSON([][2]any{{"c1", int64(5000)}}),
			conversationsPageJSON(nil)).
		// One page straddling the cutoff (--after=150): m2 (200) is at or
		// after it, m1 (100) is strictly older. A second page would only be
		// fetched if the straddling page didn't correctly signal "reached
		// the cutoff" — make it look like more new history so the test
		// fails loudly (extra messages_added, extra call) rather than
		// silently if pagination doesn't stop here.
		on("/messaging/conversations/c1/events",
			messagesPageJSON([][2]any{{"m2", int64(200)}, {"m1", int64(100)}}),
			messagesPageJSON([][2]any{{"m0", int64(50)}}))
	cl := newFixtureClient(rt)

	afterMs := int64(150)
	summary, err := runSyncPass(context.Background(), cl, st, syncOptions{afterMs: &afterMs}, discardProgress(t))
	if err != nil {
		t.Fatalf("runSyncPass: %v", err)
	}
	if summary.MessagesAdded != 1 {
		t.Errorf("MessagesAdded = %d, want 1 (only m2 is at or after --after)", summary.MessagesAdded)
	}
	if got := rt.callCount("/messaging/conversations/c1/events"); got != 1 {
		t.Errorf("events endpoint called %d times, want 1 (pagination must stop once the page's oldest message is before --after)", got)
	}

	msgs, err := st.Messages(context.Background(), store.MessageFilter{ConversationID: "c1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || msgs[0].URN != "m2" {
		t.Errorf("stored messages = %+v, want only m2 — m1 predates --after and must never have been stored", msgs)
	}
}

// TestSyncMaxDBSizeAlreadyAtLimitDoesNotMutate is the required --max-db-size
// regression test: a store already at (or past) the configured limit must
// not be mutated at all — no page may be applied, however small — and the
// pass must report complete:false, rather than the size only being checked
// after a page had already landed (the defect: checking post-commit means
// the very first page of a run always lands even when the store started
// over the limit, since "after this page" has no counterpart before the
// first page exists to check after).
func TestSyncMaxDBSizeAlreadyAtLimitDoesNotMutate(t *testing.T) {
	st := openSyncTestStore(t)
	ctx := context.Background()
	// Seed something for the store to already hold, so this isn't simply an
	// empty-database edge case.
	err := st.WithTx(ctx, func(tx *store.Tx) error {
		if err := tx.UpsertConversation(ctx, store.Conversation{ID: "c0", URN: "urn:li:fs_conversation:c0", UpdatedAt: 1}, 1); err != nil {
			return err
		}
		_, err := tx.RecordMessagePage(ctx, "c0", []store.Message{{URN: "seed", ConversationID: "c0", SentAt: 1}}, 1)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	limit, err := st.SizeBytes()
	if err != nil {
		t.Fatal(err)
	}

	rt := newRouteFixtureTransport().
		on("/messaging/conversations",
			conversationsPageJSON([][2]any{{"c1", int64(5000)}}),
			conversationsPageJSON(nil)).
		on("/messaging/conversations/c1/events",
			messagesPageJSON([][2]any{{"m1", int64(100)}}))
	cl := newFixtureClient(rt)

	summary, err := runSyncPass(ctx, cl, st, syncOptions{maxDBSizeBytes: limit}, discardProgress(t))
	if err != nil {
		t.Fatalf("runSyncPass: %v", err)
	}
	if summary.Complete {
		t.Error("Complete = true, want false: --max-db-size was already at the limit before this run started")
	}
	if summary.MessagesAdded != 0 {
		t.Errorf("MessagesAdded = %d, want 0: a store already at --max-db-size must not be mutated at all", summary.MessagesAdded)
	}
	msgs, err := st.Messages(ctx, store.MessageFilter{ConversationID: "c1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 0 {
		t.Errorf("c1 messages = %d, want 0: the page must never have been applied once the store was already at the limit", len(msgs))
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
