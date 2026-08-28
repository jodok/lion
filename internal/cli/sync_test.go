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
	// errOnce marks a route whose injected failure applies to that one call
	// only, for scenarios where the code is expected to recover and retry.
	errOnce map[string]bool
	err     error
	lastURL map[string]string
	// badStatusOnce makes a route's Nth call answer with an HTTP status
	// instead of a fixture. Distinct from errOnCall: voyager retries a GET
	// once on a transport error, so a transport-level failure is masked by
	// that retry, while a 4xx comes straight back — which is what a rejected
	// sync token actually looks like.
	badStatusOnce map[string]int
}

func newRouteFixtureTransport() *routeFixtureTransport {
	return &routeFixtureTransport{routes: map[string][]string{}, calls: map[string]int{},
		errOnCall: map[string]int{}, errOnce: map[string]bool{}, lastURL: map[string]string{},
		badStatusOnce: map[string]int{}}
}

func (r *routeFixtureTransport) on(path string, bodies ...string) *routeFixtureTransport {
	r.routes[path] = bodies
	return r
}

// failOnCallOnce injects a failure for a single call, for paths the code is
// expected to recover from rather than give up on.
func (r *routeFixtureTransport) failOnCallOnce(path string, call int, err error) *routeFixtureTransport {
	r.errOnCall[path] = call
	r.errOnce[path] = true
	r.err = err
	return r
}

// badRequestOnce makes a route's next call answer 400, the way LinkedIn
// rejects a stale sync token.
func (r *routeFixtureTransport) badRequestOnce(path string) *routeFixtureTransport {
	r.badStatusOnce[path] = 400
	return r
}

// lastURLFor returns the most recent request URL seen on a route, for tests
// asserting on how a query was built.
func (r *routeFixtureTransport) lastURLFor(path string) string { return r.lastURL[path] }

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
	// Messaging moved onto one GraphQL path, so the path alone no longer
	// identifies a call: conversations and messages differ only by queryId,
	// and one conversation's messages from another's only by the
	// conversationUrn variable. Fold both into the routing key.
	if qid := u.Query().Get("queryId"); qid != "" {
		name := qid
		if i := strings.Index(name, "."); i > 0 {
			name = name[:i]
		}
		path += ":" + name
		if vars := u.Query().Get("variables"); strings.Contains(vars, "conversationUrn") {
			path += ":" + conversationIDFromVars(vars)
		}
	}

	i := r.calls[path]
	r.calls[path]++
	r.lastURL[path] = req.URL

	// >= (not ==): a real failure mode this simulates (rate limiting,
	// session expiry) doesn't clear itself on GET's built-in one-shot
	// retry (see voyager.Client.do), so the fixture shouldn't either — a
	// one-call-only failure would let that retry silently mask the
	// scenario this test exists to cover.
	if failAt, ok := r.errOnCall[path]; ok && i >= failAt {
		if r.errOnce[path] {
			delete(r.errOnCall, path)
		}
		return nil, r.err
	}

	if status, ok := r.badStatusOnce[path]; ok {
		delete(r.badStatusOnce, path)
		return &voyager.Response{StatusCode: status, Body: []byte(`{"status":400}`)}, nil
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

// conversationIDFromVars pulls the thread segment out of a conversationUrn
// variable, so one conversation's message route can be told from another's.
func conversationIDFromVars(vars string) string {
	dec, err := url.QueryUnescape(vars)
	if err != nil {
		dec = vars
	}
	// Take the conversationUrn value specifically, not "whatever follows the
	// last comma": once a syncToken is appended the variables read
	// (conversationUrn:urn:li:msg_conversation:(mailbox,thread),syncToken:…)
	// and the last comma belongs to the token.
	const key = "conversationUrn:"
	i := strings.Index(dec, key)
	if i < 0 {
		return ""
	}
	rest := dec[i+len(key):]
	// The URN carries exactly one paren group, so it ends at the first ")".
	end := strings.Index(rest, ")")
	if end < 0 {
		return ""
	}
	urn := rest[:end]
	if j := strings.LastIndex(urn, ","); j >= 0 {
		return urn[j+1:]
	}
	return urn
}

const (
	routeConversations = "/voyagerMessagingGraphQL/graphql:messengerConversations"
	routeMessagesBase  = "/voyagerMessagingGraphQL/graphql:messengerMessages"
)

// routeMessages is the routing key for one conversation's message stream.
func routeMessages(conversationID string) string {
	return routeMessagesBase + ":" + conversationID
}

// meJSON is the /me response the client resolves its mailbox URN from. The
// result is memoized per Client, so one is enough for a whole sync pass.
const meJSON = `{"data":{"*miniProfile":"urn:li:fs_miniProfile:me"},` +
	`"included":[{"$type":"com.linkedin.voyager.identity.shared.MiniProfile",` +
	`"entityUrn":"urn:li:fs_miniProfile:me","firstName":"Test","lastName":"User",` +
	`"publicIdentifier":"test-user"}]}`

// conversationsSyncJSON builds a conversations response in the messaging
// sync-token shape: *elements naming the result set, included[] carrying the
// entities, and the SyncMetadata block the drain reads.
func conversationsSyncJSON(entries [][2]any) string {
	els := make([]string, 0, len(entries))
	inc := make([]string, 0, len(entries))
	for _, e := range entries {
		id, last := e[0].(string), e[1].(int64)
		urn := fmt.Sprintf("urn:li:msg_conversation:(urn:li:fsd_profile:me,%s)", id)
		els = append(els, fmt.Sprintf("%q", urn))
		inc = append(inc, fmt.Sprintf(
			`{"$type":"com.linkedin.messenger.Conversation","entityUrn":%q,`+
				`"lastActivityAt":%d,"read":true,`+
				`"*conversationParticipants":["urn:li:msg_messagingParticipant:p1"]}`, urn, last))
	}
	inc = append(inc, `{"$type":"com.linkedin.messenger.MessagingParticipant",`+
		`"entityUrn":"urn:li:msg_messagingParticipant:p1",`+
		`"hostIdentityUrn":"urn:li:fsd_profile:p1",`+
		`"participantType":{"member":{"firstName":{"text":"Pat"},"lastName":{"text":"Lee"}}}}`)
	return fmt.Sprintf(`{"data":{"data":{"messengerConversationsBySyncToken":{`+
		`"*elements":[%s],"metadata":{"newSyncToken":"tok-%d","deletedUrns":[],`+
		`"shouldClearCache":true}}}},"included":[%s]}`,
		strings.Join(els, ","), len(entries), strings.Join(inc, ","))
}

// messagesSyncJSON builds a messages response in the same sync-token shape.
func messagesSyncJSON(entries [][2]any) string {
	els := make([]string, 0, len(entries))
	inc := make([]string, 0, len(entries))
	for _, e := range entries {
		urn, sent := e[0].(string), e[1].(int64)
		els = append(els, fmt.Sprintf("%q", urn))
		inc = append(inc, fmt.Sprintf(
			`{"$type":"com.linkedin.messenger.Message","entityUrn":%q,"deliveredAt":%d,`+
				`"body":{"text":"hi"},"*sender":"urn:li:msg_messagingParticipant:p1"}`, urn, sent))
	}
	inc = append(inc, `{"$type":"com.linkedin.messenger.MessagingParticipant",`+
		`"entityUrn":"urn:li:msg_messagingParticipant:p1",`+
		`"hostIdentityUrn":"urn:li:fsd_profile:p1",`+
		`"participantType":{"member":{"firstName":{"text":"Pat"},"lastName":{"text":"Lee"}}}}`)
	return fmt.Sprintf(`{"data":{"data":{"messengerMessagesBySyncToken":{`+
		`"*elements":[%s],"metadata":{"newSyncToken":"mtok-%d","deletedUrns":[],`+
		`"shouldClearCache":true}}}},"included":[%s]}`,
		strings.Join(els, ","), len(entries), strings.Join(inc, ","))
}

// TestSyncPopulatesStore is the required "populates the store" test: a
// fixture transport serving a multi-page conversation-with-messages fixture
// must leave the store holding every conversation and message, and report a
// complete, accurate summary.
func TestSyncPopulatesStore(t *testing.T) {
	st := openSyncTestStore(t)
	rt := newRouteFixtureTransport().
		on("/me", meJSON).
		on(routeConversations,
			conversationsSyncJSON([][2]any{{"c1", int64(5000)}}),
			conversationsSyncJSON(nil)).
		on(routeMessages("c1"),
			messagesSyncJSON([][2]any{{"m2", int64(200)}, {"m1", int64(100)}}),
			messagesSyncJSON(nil))
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
			on("/me", meJSON).
			on(routeConversations,
				conversationsSyncJSON([][2]any{{"c1", int64(5000)}}),
				conversationsSyncJSON(nil)).
			on(routeMessages("c1"),
				messagesSyncJSON([][2]any{{"m1", int64(100)}}),
				messagesSyncJSON(nil))
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

// TestSyncCatchUpDoesNotRestoreKnownMessages replaces
// TestSyncCatchUpStopsAtKnownMessage.
//
// The old contract was a request-count optimisation: catch-up stopped
// paging the moment a page contained a message already stored, so a
// conversation cost one call. The sync-token stream has no cursor to stop —
// it is drained until a response brings nothing new — so that assertion
// described a mechanism that no longer exists.
//
// What still matters, and is what the test was really protecting, is that
// re-seeing a known message must not duplicate it in the store or inflate
// the summary. That contract survives the migration intact.
func TestSyncCatchUpDoesNotRestoreKnownMessages(t *testing.T) {
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

	// The stream returns one genuinely new message (m2) alongside the
	// already-known m1, then runs out.
	rt := newRouteFixtureTransport().
		on("/me", meJSON).
		on(routeConversations,
			conversationsSyncJSON([][2]any{{"c1", int64(5000)}}),
			conversationsSyncJSON(nil)).
		on(routeMessages("c1"),
			messagesSyncJSON([][2]any{{"m2", int64(200)}, {"m1", int64(100)}}),
			messagesSyncJSON(nil))
	cl := newFixtureClient(rt)

	summary, err := runSyncPass(context.Background(), cl, st, syncOptions{}, discardProgress(t))
	if err != nil {
		t.Fatalf("runSyncPass: %v", err)
	}
	if summary.MessagesAdded != 1 {
		t.Errorf("messages_added = %d, want 1 (only m2 is new)", summary.MessagesAdded)
	}
	if !summary.Complete {
		t.Error("Complete = false, want true (the stream ran out)")
	}
	msgs, err := st.Messages(context.Background(), store.MessageFilter{ConversationID: "c1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 {
		t.Errorf("store holds %d messages, want 2 (m1 kept, m2 added, no duplicate)", len(msgs))
	}
}

// TestSyncPlainRunReachesEndOfHistory replaces
// TestSyncBackfillReachesEndAndSetsFlag.
//
// That test drove `syncOptions{backfill: true}`, and the flag is gone: a
// drain returns a conversation's whole history, so catch-up already fetches
// everything the separate backwards walk used to find. What replaces the
// assertion is the reason the flag became unnecessary — a plain sync, with
// nothing opted into, must drain across responses to the end of history and
// set BackfillDone itself.
func TestSyncPlainRunReachesEndOfHistory(t *testing.T) {
	st := openSyncTestStore(t)
	rt := newRouteFixtureTransport().
		on("/me", meJSON).
		on(routeConversations,
			conversationsSyncJSON([][2]any{{"c1", int64(5000)}}),
			conversationsSyncJSON(nil)).
		on(routeMessages("c1"),
			messagesSyncJSON([][2]any{{"m2", int64(200)}}),
			messagesSyncJSON([][2]any{{"m1", int64(100)}}),
			messagesSyncJSON(nil))
	cl := newFixtureClient(rt)

	summary, err := runSyncPass(context.Background(), cl, st, syncOptions{}, discardProgress(t))
	if err != nil {
		t.Fatal(err)
	}
	if summary.MessagesAdded != 2 {
		t.Fatalf("messages_added = %d, want 2 (both drained without --backfill)", summary.MessagesAdded)
	}
	if !summary.Complete {
		t.Error("Complete = false, want true")
	}
	conv, ok, err := st.Conversation(context.Background(), "c1")
	if err != nil {
		t.Fatal(err)
	}
	if !ok || !conv.BackfillDone {
		t.Error("BackfillDone = false after a plain run reached the end of history, want true")
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
		on("/me", meJSON).
		on(routeConversations,
			conversationsSyncJSON([][2]any{{"c1", int64(5000)}, {"c2", int64(4000)}}),
			conversationsSyncJSON(nil)).
		on(routeMessages("c1"),
			messagesSyncJSON([][2]any{{"m1", int64(100)}}),
			messagesSyncJSON(nil)).
		on(routeMessages("c2"),
			messagesSyncJSON([][2]any{{"m2", int64(100)}}))
	// c2's very first page fails outright, simulating a rate limit hit
	// partway through the run (c1 must have already been fully processed).
	rt.failOnCall(routeMessages("c2"), 0, boom)
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
		on("/me", meJSON).
		on(routeConversations,
			conversationsSyncJSON([][2]any{{"c1", int64(5000)}, {"c2", int64(4000)}})).
		on(routeMessages("c1"), messagesSyncJSON(nil))
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

// TestSyncMaxMessagesTruncatesAndReportsIncomplete: --max-messages stopping
// a conversation mid-history is a truncation, and recording that pass as a
// complete sync would strand everything past the cap as permanently unseen.
//
// The mechanism changed with the migration — the budget now caps a drain
// rather than cutting a page walk short — but the contract is the same one
// the original test pinned.
func TestSyncMaxMessagesTruncatesAndReportsIncomplete(t *testing.T) {
	st := openSyncTestStore(t)
	rt := newRouteFixtureTransport().
		on("/me", meJSON).
		on(routeConversations,
			conversationsSyncJSON([][2]any{{"c1", int64(5000)}}),
			conversationsSyncJSON(nil)).
		on(routeMessages("c1"),
			messagesSyncJSON([][2]any{{"m3", int64(300)}, {"m2", int64(200)}, {"m1", int64(100)}}),
			messagesSyncJSON(nil))
	cl := newFixtureClient(rt)

	summary, err := runSyncPass(context.Background(), cl, st, syncOptions{maxMessages: 2}, discardProgress(t))
	if err != nil {
		t.Fatalf("runSyncPass: %v", err)
	}
	if summary.Complete {
		t.Error("Complete = true, want false: --max-messages cut the conversation short")
	}
	if summary.MessagesAdded > 2 {
		t.Errorf("messages_added = %d, want at most the budget of 2", summary.MessagesAdded)
	}
	// A capped pass must not claim the conversation is fully archived.
	conv, ok, err := st.Conversation(context.Background(), "c1")
	if err != nil {
		t.Fatal(err)
	}
	if ok && conv.BackfillDone {
		t.Error("BackfillDone = true after a capped run, want false")
	}
}

// Removed with the sync-token migration (see voyager/sync.go):
//
//   - TestSyncStalledMessagesCursorReportsIncomplete
//   - TestSyncStalledDuplicatePageIsNotTreatedAsCaughtUp
//   - TestSyncPlainRerunAfterInterruptedInitialSyncReportsIncomplete
//   - TestSyncBackfillResumesFromOldestSyncedAfterInterruptedInitialSync
//
// All four pinned behaviour of a timestamp cursor that no longer exists.
// LinkedIn's messaging surface returns a token, deleted urns, and a
// clear-cache flag — no cursor, no pages — so there is nothing to advance,
// nothing to re-serve, and no stall to detect. A conversation is drained
// until a response brings nothing new, and resuming from a stored
// OldestSynced has no counterpart: the drain simply returns the history
// again, and re-storing a known message is a no-op.
//
// They are deleted rather than converted because a converted version would
// assert something the code cannot do wrong any more, which is worse than no
// test: it reads as coverage while pinning nothing. What they were really
// protecting — that a truncated pass is never recorded as complete — is now
// covered by TestSyncMaxMessagesTruncatesAndReportsIncomplete and
// TestSyncMaxConversationsTruncatesAndReportsIncomplete here, and by the
// drain's own limit and cap tests in voyager/sync_test.go.

// TestSyncAfterFiltersStoredMessages replaces
// TestSyncAfterFiltersStoredMessagesButPaginationStillAdvances.
//
// The dropped half asserted that paging stopped once a page's oldest message
// fell before --after — a cursor optimisation with nothing left to optimise,
// since the sync stream is drained rather than walked. The surviving half is
// the one that matters: --after decides what gets stored, and must not be
// confused with the stream running out.
func TestSyncAfterFiltersStoredMessages(t *testing.T) {
	st := openSyncTestStore(t)
	rt := newRouteFixtureTransport().
		on("/me", meJSON).
		on(routeConversations,
			conversationsSyncJSON([][2]any{{"c1", int64(5000)}}),
			conversationsSyncJSON(nil)).
		on(routeMessages("c1"),
			messagesSyncJSON([][2]any{{"m3", int64(300)}, {"m2", int64(200)}, {"m1", int64(100)}}),
			messagesSyncJSON(nil))
	cl := newFixtureClient(rt)

	after := int64(250)
	summary, err := runSyncPass(context.Background(), cl, st, syncOptions{afterMs: &after}, discardProgress(t))
	if err != nil {
		t.Fatalf("runSyncPass: %v", err)
	}
	if summary.MessagesAdded != 1 {
		t.Errorf("messages_added = %d, want 1 (only m3 is at or after the cutoff)", summary.MessagesAdded)
	}
	msgs, err := st.Messages(context.Background(), store.MessageFilter{ConversationID: "c1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || msgs[0].URN != "m3" {
		t.Errorf("store holds %+v, want only m3", msgs)
	}
	// Filtering is not truncation: the stream still ran out.
	if !summary.Complete {
		t.Error("Complete = false, want true (--after trimmed the result, it did not cut the stream short)")
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
		on("/me", meJSON).
		on(routeConversations,
			conversationsSyncJSON([][2]any{{"c1", int64(5000)}}),
			conversationsSyncJSON(nil)).
		on(routeMessages("c1"),
			messagesSyncJSON([][2]any{{"m1", int64(100)}}))
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

// TestSyncMaxDBSizeAlreadyAtLimitBlocksDiscovery is a regression test for
// discoverConversations having no --max-db-size check of its own: it
// committed every fetched conversation page via UpsertConversation before
// --max-db-size was consulted anywhere, so a store already at the
// configured limit still had discovery mutate it — the previous sibling
// test only proved the message side stayed untouched, not that "c1" itself
// was never even written to the conversations table by discovery.
func TestSyncMaxDBSizeAlreadyAtLimitBlocksDiscovery(t *testing.T) {
	st := openSyncTestStore(t)
	ctx := context.Background()
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
		on("/me", meJSON).
		on(routeConversations,
			conversationsSyncJSON([][2]any{{"c1", int64(5000)}}),
			conversationsSyncJSON(nil))
	cl := newFixtureClient(rt)

	summary, err := runSyncPass(ctx, cl, st, syncOptions{maxDBSizeBytes: limit}, discardProgress(t))
	if err != nil {
		t.Fatalf("runSyncPass: %v", err)
	}
	if summary.Complete {
		t.Error("Complete = true, want false: --max-db-size was already at the limit before discovery even ran")
	}
	if summary.ConversationsSeen != 0 {
		t.Errorf("ConversationsSeen = %d, want 0: discovery must not have processed c1 at all", summary.ConversationsSeen)
	}
	if _, ok, cErr := st.Conversation(ctx, "c1"); cErr != nil {
		t.Fatal(cErr)
	} else if ok {
		t.Error("conversation c1 was written to the store by discovery despite the store already being at --max-db-size")
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

// TestSyncDeduplicatesRepeatedConversationPages covers the discovery dedup: an
// inclusive createdBefore re-serves the boundary conversation at the head of
// the next page, and a stalled cursor re-serves a whole page. Either way a
// conversation must be walked (a rate-limited MessagesPage) and counted only
// once. Here c2 is the boundary and appears on both pages.
func TestSyncDeduplicatesRepeatedConversationPages(t *testing.T) {
	st := openSyncTestStore(t)
	rt := newRouteFixtureTransport().
		on("/me", meJSON).
		on(routeConversations,
			conversationsSyncJSON([][2]any{{"c1", int64(5000)}, {"c2", int64(4000)}}),
			// Inclusive boundary: c2 (UpdatedAt == the cursor just sent)
			// re-served at the head, then a genuinely older c3.
			conversationsSyncJSON([][2]any{{"c2", int64(4000)}, {"c3", int64(3000)}}),
			conversationsSyncJSON(nil)).
		on(routeMessages("c1"), messagesSyncJSON([][2]any{{"m1", int64(10)}}), messagesSyncJSON(nil)).
		on(routeMessages("c2"), messagesSyncJSON([][2]any{{"m2", int64(20)}}), messagesSyncJSON(nil)).
		on(routeMessages("c3"), messagesSyncJSON([][2]any{{"m3", int64(30)}}), messagesSyncJSON(nil))
	cl := newFixtureClient(rt)

	summary, err := runSyncPass(context.Background(), cl, st, syncOptions{}, discardProgress(t))
	if err != nil {
		t.Fatalf("runSyncPass: %v", err)
	}
	if summary.ConversationsSeen != 3 {
		t.Errorf("ConversationsSeen = %d, want 3 (c2 must not be counted twice)", summary.ConversationsSeen)
	}
	// c2 must have been walked exactly once, like c1 and c3: each
	// conversation fetches one data page plus one terminating empty page.
	// A duplicate discovery entry would double that to four.
	if got, want := rt.callCount(routeMessages("c2")), rt.callCount(routeMessages("c1")); got != want {
		t.Errorf("c2 events fetched %d times, c1 %d — c2 was re-walked by a duplicate discovery entry", got, want)
	}
}

// TestSyncAfterDoesNotClaimFullBackfill: BackfillDone means the store holds a
// conversation's whole history. Under --after the drain sees every message
// and deliberately discards the old ones, so claiming a full archive would
// drop the conversation from future `history backfill` targets and report
// backfill_done=true for an archive with a hole in it.
func TestSyncAfterDoesNotClaimFullBackfill(t *testing.T) {
	st := openSyncTestStore(t)
	rt := newRouteFixtureTransport().
		on("/me", meJSON).
		on(routeConversations,
			conversationsSyncJSON([][2]any{{"c1", int64(5000)}}),
			conversationsSyncJSON(nil)).
		on(routeMessages("c1"),
			messagesSyncJSON([][2]any{{"m2", int64(300)}, {"m1", int64(100)}}),
			messagesSyncJSON(nil))
	cl := newFixtureClient(rt)

	after := int64(250)
	if _, err := runSyncPass(context.Background(), cl, st, syncOptions{afterMs: &after}, discardProgress(t)); err != nil {
		t.Fatalf("runSyncPass: %v", err)
	}
	conv, ok, err := st.Conversation(context.Background(), "c1")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("conversation not stored")
	}
	if conv.BackfillDone {
		t.Error("BackfillDone = true after --after trimmed older messages, want false")
	}
}

// TestSyncWithoutAfterClaimsFullBackfill is the other half: an unfiltered
// drain that ran out really does hold the whole conversation, and must say so
// or every later run pays to rediscover history it already has.
func TestSyncWithoutAfterClaimsFullBackfill(t *testing.T) {
	st := openSyncTestStore(t)
	rt := newRouteFixtureTransport().
		on("/me", meJSON).
		on(routeConversations,
			conversationsSyncJSON([][2]any{{"c1", int64(5000)}}),
			conversationsSyncJSON(nil)).
		on(routeMessages("c1"),
			messagesSyncJSON([][2]any{{"m2", int64(300)}, {"m1", int64(100)}}),
			messagesSyncJSON(nil))
	cl := newFixtureClient(rt)

	if _, err := runSyncPass(context.Background(), cl, st, syncOptions{}, discardProgress(t)); err != nil {
		t.Fatalf("runSyncPass: %v", err)
	}
	conv, ok, err := st.Conversation(context.Background(), "c1")
	if err != nil {
		t.Fatal(err)
	}
	if !ok || !conv.BackfillDone {
		t.Error("BackfillDone = false after a complete unfiltered drain, want true")
	}
}

// TestSyncResumesFromStoredToken is the point of persisting tokens: a second
// run must ask the server to continue from where the first stopped rather
// than replaying the whole mailbox.
func TestSyncResumesFromStoredToken(t *testing.T) {
	st := openSyncTestStore(t)
	rt := newRouteFixtureTransport().
		on("/me", meJSON).
		on(routeConversations,
			conversationsSyncJSON([][2]any{{"c1", int64(5000)}}),
			conversationsSyncJSON(nil)).
		on(routeMessages("c1"),
			messagesSyncJSON([][2]any{{"m1", int64(100)}}),
			messagesSyncJSON(nil))
	cl := newFixtureClient(rt)

	if _, err := runSyncPass(context.Background(), cl, st, syncOptions{}, discardProgress(t)); err != nil {
		t.Fatalf("first pass: %v", err)
	}

	// The mailbox resume point is persisted...
	tok, ok, err := st.Meta(context.Background(), conversationsSyncTokenKey)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || tok == "" {
		t.Fatal("no mailbox sync token stored after a pass")
	}
	// ...and so is the conversation's.
	conv, ok, err := st.Conversation(context.Background(), "c1")
	if err != nil {
		t.Fatal(err)
	}
	if !ok || conv.MessagesSyncToken == "" {
		t.Fatal("no message sync token stored for c1")
	}

	// A second pass must send the stored token back.
	if _, err := runSyncPass(context.Background(), cl, st, syncOptions{}, discardProgress(t)); err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if !strings.Contains(rt.lastURLFor(routeConversations), "syncToken") {
		t.Errorf("second pass did not resume: %s", rt.lastURLFor(routeConversations))
	}
}

// TestSyncFullIgnoresStoredToken: --full is the escape hatch for a delta
// stream that has drifted from what is stored, so it must not send the token.
func TestSyncFullIgnoresStoredToken(t *testing.T) {
	st := openSyncTestStore(t)
	rt := newRouteFixtureTransport().
		on("/me", meJSON).
		on(routeConversations,
			conversationsSyncJSON([][2]any{{"c1", int64(5000)}}),
			conversationsSyncJSON(nil)).
		on(routeMessages("c1"),
			messagesSyncJSON([][2]any{{"m1", int64(100)}}),
			messagesSyncJSON(nil))
	cl := newFixtureClient(rt)

	if _, err := runSyncPass(context.Background(), cl, st, syncOptions{}, discardProgress(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := runSyncPass(context.Background(), cl, st, syncOptions{full: true}, discardProgress(t)); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(rt.lastURLFor(routeConversations), "syncToken") {
		t.Errorf("--full sent a stored token: %s", rt.lastURLFor(routeConversations))
	}
}

// TestIncrementalRunDoesNotClaimFullBackfill pins the guard that a
// resumed-from-token drain running dry means "nothing changed since last
// time", not "the whole history is held" — a delta never replays what came
// before, so marking BackfillDone on it would tell `history coverage` a
// conversation was fully archived on evidence that says no such thing.
//
// The state is seeded directly rather than produced by a capped run. Since
// the token now only advances when a drain both completed and stored
// everything it fetched, a normal pass can no longer leave a token behind
// with the history still partial — but a store written by an earlier build,
// or one whose backfill flag was cleared, still can, and the guard is what
// protects those.
func TestIncrementalRunDoesNotClaimFullBackfill(t *testing.T) {
	ctx := context.Background()
	st := openSyncTestStore(t)
	if err := st.WithTx(ctx, func(tx *store.Tx) error {
		if err := tx.UpsertConversation(ctx, store.Conversation{
			ID: "c1", URN: "urn:li:msg_conversation:(urn:li:fsd_profile:me,c1)", UpdatedAt: 5000,
		}, 1); err != nil {
			return err
		}
		// A resume point with the history still incomplete: BackfillDone is
		// deliberately left false.
		return tx.SetMessagesSyncToken(ctx, "c1", "stored-token")
	}); err != nil {
		t.Fatal(err)
	}

	rt := newRouteFixtureTransport().
		on("/me", meJSON).
		on(routeConversations,
			conversationsSyncJSON([][2]any{{"c1", int64(5000)}}),
			conversationsSyncJSON(nil)).
		// The delta runs dry immediately: nothing changed.
		on(routeMessages("c1"), messagesSyncJSON(nil))
	cl := newFixtureClient(rt)

	if _, err := runSyncPass(ctx, cl, st, syncOptions{}, discardProgress(t)); err != nil {
		t.Fatal(err)
	}
	// The pass must actually have resumed, or the guard is never reached.
	if !strings.Contains(rt.lastURLFor(routeMessages("c1")), "syncToken") {
		t.Fatalf("pass did not resume the message stream: %s", rt.lastURLFor(routeMessages("c1")))
	}
	conv, ok, err := st.Conversation(ctx, "c1")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("c1 missing from the store")
	}
	if conv.BackfillDone {
		t.Error("BackfillDone = true after an incremental drain ran dry; a delta never replays older history")
	}
}

// TestCappedRunDoesNotAdvanceTheResumePoint: a run that fetches more than it
// keeps must not move the token past what it discarded.
//
// --max-messages caps through capNewest, which keeps the newest N of a
// response and throws away the older ones from that same response. Advancing
// the token past them would mean no later run — not even `history backfill`,
// which resumes from the same token — ever asks for them again.
func TestCappedRunDoesNotAdvanceTheResumePoint(t *testing.T) {
	ctx := context.Background()
	st := openSyncTestStore(t)
	rt := newRouteFixtureTransport().
		on("/me", meJSON).
		on(routeConversations,
			conversationsSyncJSON([][2]any{{"c1", int64(5000)}}),
			conversationsSyncJSON(nil)).
		on(routeMessages("c1"),
			messagesSyncJSON([][2]any{{"m3", int64(300)}, {"m2", int64(200)}, {"m1", int64(100)}}),
			messagesSyncJSON(nil))
	cl := newFixtureClient(rt)

	if _, err := runSyncPass(ctx, cl, st, syncOptions{maxMessages: 1}, discardProgress(t)); err != nil {
		t.Fatal(err)
	}
	conv, _, err := st.Conversation(ctx, "c1")
	if err != nil {
		t.Fatal(err)
	}
	if conv.MessagesSyncToken != "" {
		t.Errorf("MessagesSyncToken = %q after a capped run; resuming from it would skip the messages "+
			"the cap discarded", conv.MessagesSyncToken)
	}
}

// TestAfterFilteredRunDoesNotAdvanceTheResumePoint is the same guarantee for
// --after: messages below the cutoff are fetched and dropped, and a later run
// without --after must still be able to reach them.
func TestAfterFilteredRunDoesNotAdvanceTheResumePoint(t *testing.T) {
	ctx := context.Background()
	st := openSyncTestStore(t)
	rt := newRouteFixtureTransport().
		on("/me", meJSON).
		on(routeConversations,
			conversationsSyncJSON([][2]any{{"c1", int64(5000)}}),
			conversationsSyncJSON(nil)).
		on(routeMessages("c1"),
			messagesSyncJSON([][2]any{{"m2", int64(300)}, {"m1", int64(100)}}),
			messagesSyncJSON(nil))
	cl := newFixtureClient(rt)

	after := int64(250)
	if _, err := runSyncPass(ctx, cl, st, syncOptions{afterMs: &after}, discardProgress(t)); err != nil {
		t.Fatal(err)
	}
	conv, _, err := st.Conversation(ctx, "c1")
	if err != nil {
		t.Fatal(err)
	}
	if conv.MessagesSyncToken != "" {
		t.Errorf("MessagesSyncToken = %q after --after trimmed older messages; resuming from it would "+
			"skip them permanently", conv.MessagesSyncToken)
	}
}

// TestSteadyStateAdvancesTheMailboxToken: once caught up, a delta run
// legitimately returns no conversations. That is the common case for a
// periodic sync, and the resume point still has to move — otherwise the
// stored token ages until LinkedIn rejects it and the recovery path pays for
// a full snapshot of the whole mailbox.
func TestSteadyStateAdvancesTheMailboxToken(t *testing.T) {
	ctx := context.Background()
	st := openSyncTestStore(t)
	if err := st.WithTx(ctx, func(tx *store.Tx) error {
		return tx.SetMeta(ctx, conversationsSyncTokenKey, "old-token")
	}); err != nil {
		t.Fatal(err)
	}

	// Nothing changed: no conversations, but the server still offers a token.
	body := `{"data":{"data":{"messengerConversationsBySyncToken":{"*elements":[],
		"metadata":{"newSyncToken":"fresh-token","deletedUrns":[],"shouldClearCache":false}}}},
		"included":[]}`
	rt := newRouteFixtureTransport().on("/me", meJSON).on(routeConversations, body)
	cl := newFixtureClient(rt)

	if _, err := runSyncPass(ctx, cl, st, syncOptions{}, discardProgress(t)); err != nil {
		t.Fatal(err)
	}
	tok, ok, err := st.Meta(ctx, conversationsSyncTokenKey)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || tok != "fresh-token" {
		t.Errorf("mailbox token = %q, want it advanced to fresh-token even though nothing changed", tok)
	}
}

// TestSyncDropsRejectedToken: a stored token the server refuses must not
// wedge sync. It is discarded and the pass falls back to a full snapshot.
func TestSyncDropsRejectedToken(t *testing.T) {
	st := openSyncTestStore(t)
	if err := st.WithTx(context.Background(), func(tx *store.Tx) error {
		return tx.SetMeta(context.Background(), conversationsSyncTokenKey, "stale-token")
	}); err != nil {
		t.Fatal(err)
	}

	rt := newRouteFixtureTransport().
		on("/me", meJSON).
		// The rejected call consumes the first slot, so the full-snapshot
		// retry lands on the second — which has to carry the mailbox.
		on(routeConversations,
			conversationsSyncJSON(nil),
			conversationsSyncJSON([][2]any{{"c1", int64(5000)}}),
			conversationsSyncJSON(nil)).
		on(routeMessages("c1"),
			messagesSyncJSON([][2]any{{"m1", int64(100)}}),
			messagesSyncJSON(nil))
	// The call carrying the stale token is rejected the way LinkedIn does it.
	rt.badRequestOnce(routeConversations)
	cl := newFixtureClient(rt)

	summary, err := runSyncPass(context.Background(), cl, st, syncOptions{}, discardProgress(t))
	if err != nil {
		t.Fatalf("a rejected token must not fail the pass: %v", err)
	}
	if summary.ConversationsSeen != 1 {
		t.Errorf("conversations_seen = %d, want 1 (the full-snapshot retry)", summary.ConversationsSeen)
	}
}

// TestSyncAppliesDeletedConversations: a delta stream names a removed
// conversation once and never mentions it again, so ignoring deletedUrns
// would leave it in the store forever.
func TestSyncAppliesDeletedConversations(t *testing.T) {
	ctx := context.Background()
	st := openSyncTestStore(t)
	if err := st.WithTx(ctx, func(tx *store.Tx) error {
		return tx.UpsertConversation(ctx, store.Conversation{ID: "gone", URN: "urn:gone", UpdatedAt: 1}, 1)
	}); err != nil {
		t.Fatal(err)
	}

	body := `{"data":{"data":{"messengerConversationsBySyncToken":{"*elements":[],
		"metadata":{"newSyncToken":"t2","deletedUrns":["urn:li:msg_conversation:(urn:li:fsd_profile:me,gone)"],
		"shouldClearCache":false}}}},"included":[]}`
	rt := newRouteFixtureTransport().on("/me", meJSON).on(routeConversations, body)
	cl := newFixtureClient(rt)

	if _, err := runSyncPass(ctx, cl, st, syncOptions{}, discardProgress(t)); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := st.Conversation(ctx, "gone"); err != nil {
		t.Fatal(err)
	} else if ok {
		t.Error("a conversation the server reported deleted is still in the store")
	}
}
