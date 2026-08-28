package voyager

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// syncBody builds a conversations sync response: one conversation per id,
// plus the metadata block that drives the drain.
func syncBody(token string, ids ...string) string {
	var elems, inc []string
	for i, id := range ids {
		urn := fmt.Sprintf("urn:li:msg_conversation:(urn:li:fsd_profile:me,%s)", id)
		elems = append(elems, fmt.Sprintf("%q", urn))
		inc = append(inc, fmt.Sprintf(`{"$type":"com.linkedin.messenger.Conversation",
			"entityUrn":%q,"lastActivityAt":%d,"read":true}`, urn, 9000-i))
	}
	return fmt.Sprintf(`{
	  "data": {"data": {"messengerConversationsBySyncToken": {
	    "*elements": [%s],
	    "metadata": {"newSyncToken": %q, "deletedUrns": [], "shouldClearCache": true}
	  }}},
	  "included": [%s]
	}`, strings.Join(elems, ","), token, strings.Join(inc, ","))
}

func syncClient(t *testing.T, bodies ...string) (*Client, *sequenceTransport) {
	t.Helper()
	st := &sequenceTransport{}
	for _, b := range bodies {
		st.responses = append(st.responses, &Response{StatusCode: 200, Body: []byte(b)})
	}
	c := New("li_at", `"ajax:1"`, WithTransport(st), WithLimiter(noopLimiter()))
	// Pre-seed the memoized mailbox so the drain does not need a /me fixture
	// interleaved into the response sequence.
	c.mailboxOnce.Do(func() { c.mailboxURN = "urn%3Ali%3Afsd_profile%3Ame" })
	return c, st
}

// TestAllConversationsStopsWhenAResponseAddsNothing is the termination case
// that actually occurs against LinkedIn: the server issues a *fresh* token on
// every call even when nothing has changed, so a drain keyed on "the token
// moved" would never stop. It has to key on whether anything new arrived.
func TestAllConversationsStopsWhenAResponseAddsNothing(t *testing.T) {
	c, st := syncClient(t,
		syncBody("token-1", "2-a", "2-b"),
		syncBody("token-2", "2-a", "2-b"), // same content, new token
	)
	convs, complete, err := c.AllConversations(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if !complete {
		t.Error("complete = false, want true (the stream ran out)")
	}
	if len(convs) != 2 {
		t.Fatalf("got %d conversations, want 2", len(convs))
	}
	if st.calls != 2 {
		t.Errorf("made %d requests, want 2 (one to fetch, one to find nothing new)", st.calls)
	}
}

// TestAllConversationsDrainsAcrossResponses is the property the drain exists
// for: whether or not LinkedIn chunks a large mailbox — unverifiable on a
// two-conversation account — following the token until nothing new arrives
// collects everything either way.
func TestAllConversationsDrainsAcrossResponses(t *testing.T) {
	c, _ := syncClient(t,
		syncBody("token-1", "2-a", "2-b"),
		syncBody("token-2", "2-c"),
		syncBody("token-3", "2-d"),
		syncBody("token-4"), // empty: end of stream
	)
	convs, complete, err := c.AllConversations(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if !complete {
		t.Error("complete = false, want true")
	}
	if len(convs) != 4 {
		t.Fatalf("got %d conversations, want all 4 across the responses", len(convs))
	}
	// Newest-first, by lastActivityAt.
	if convs[0].ID != "2-a" {
		t.Errorf("first conversation = %q, want the newest", convs[0].ID)
	}
}

// TestAllConversationsDedupesAcrossResponses: a sync stream may re-serve a
// conversation it already sent, and walking one twice would cost a
// rate-limited message fetch and double-count it against --max-conversations.
func TestAllConversationsDedupesAcrossResponses(t *testing.T) {
	c, _ := syncClient(t,
		syncBody("token-1", "2-a", "2-b"),
		syncBody("token-2", "2-b", "2-c"), // 2-b repeats
		syncBody("token-3"),
	)
	convs, _, err := c.AllConversations(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(convs) != 3 {
		t.Fatalf("got %d conversations, want 3 distinct", len(convs))
	}
	seen := map[string]int{}
	for _, cv := range convs {
		seen[cv.ID]++
	}
	for id, n := range seen {
		if n != 1 {
			t.Errorf("conversation %s appeared %d times", id, n)
		}
	}
}

// TestAllConversationsCapReportsIncomplete: hitting the cap is a truncation,
// and recording that pass as a complete sync would strand every conversation
// past the cap as permanently unseen.
func TestAllConversationsCapReportsIncomplete(t *testing.T) {
	c, _ := syncClient(t,
		syncBody("token-1", "2-a", "2-b", "2-c"),
		syncBody("token-2"),
	)
	convs, complete, err := c.AllConversations(context.Background(), 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(convs) != 2 {
		t.Fatalf("got %d conversations, want the cap of 2", len(convs))
	}
	if complete {
		t.Error("complete = true after the cap truncated the walk, want false")
	}
}

// TestAllConversationsRequestLimitReportsIncomplete: a server that keeps
// returning fresh elements forever must produce a bounded, honest truncation
// rather than an endless loop.
func TestAllConversationsRequestLimitReportsIncomplete(t *testing.T) {
	// Every response carries a distinct conversation and a distinct token, so
	// the drain never sees "nothing new".
	var bodies []string
	for i := 0; i < syncDrainLimit+5; i++ {
		bodies = append(bodies, syncBody(fmt.Sprintf("token-%d", i), fmt.Sprintf("2-%d", i)))
	}
	c, st := syncClient(t, bodies...)
	convs, complete, err := c.AllConversations(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if complete {
		t.Error("complete = true after the request limit stopped the drain, want false")
	}
	if st.calls != syncDrainLimit {
		t.Errorf("made %d requests, want the limit of %d", st.calls, syncDrainLimit)
	}
	if len(convs) != syncDrainLimit {
		t.Errorf("collected %d conversations, want %d", len(convs), syncDrainLimit)
	}
}

// TestAllConversationsStopsOnRepeatedToken guards the other non-advancing
// case: a server that returns the same token back.
func TestAllConversationsStopsOnRepeatedToken(t *testing.T) {
	c, st := syncClient(t,
		syncBody("token-1", "2-a"),
		syncBody("token-1", "2-b"), // new content, same token
		syncBody("token-1", "2-c"),
	)
	convs, complete, err := c.AllConversations(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if !complete {
		t.Error("complete = false, want true (a stalled token ends the stream)")
	}
	if st.calls != 2 {
		t.Errorf("made %d requests, want 2 (stop once the token stops advancing)", st.calls)
	}
	if len(convs) != 2 {
		t.Errorf("got %d conversations, want the 2 fetched before the stall", len(convs))
	}
}

// TestConversationsSyncSurfacesMetadata: deletedUrns and shouldClearCache are
// how the protocol reports removals and full snapshots, and a caller that
// cannot see them cannot keep a local store honest.
func TestConversationsSyncSurfacesMetadata(t *testing.T) {
	body := `{
	  "data": {"data": {"messengerConversationsBySyncToken": {
	    "*elements": [],
	    "metadata": {"newSyncToken": "tok", "deletedUrns": ["urn:li:msg_conversation:(x,2-gone)"],
	                 "shouldClearCache": true}
	  }}},
	  "included": []
	}`
	c, _ := syncClient(t, body)
	page, err := c.ConversationsSync(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if page.NextToken != "tok" {
		t.Errorf("NextToken = %q", page.NextToken)
	}
	if !page.ClearCache {
		t.Error("ClearCache = false, want true")
	}
	if len(page.DeletedURNs) != 1 {
		t.Errorf("DeletedURNs = %v, want the one removal", page.DeletedURNs)
	}
}

// TestConversationsSyncSendsTokenOnlyWhenSet keeps the first call a genuine
// full-snapshot request rather than one carrying an empty syncToken variable.
func TestConversationsSyncSendsTokenOnlyWhenSet(t *testing.T) {
	c, st := syncClient(t, syncBody("t1", "2-a"), syncBody("t2"))
	if _, err := c.ConversationsSync(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	first := st.lastURL
	if strings.Contains(first, "syncToken") {
		t.Errorf("first request carried a syncToken: %s", first)
	}
	if _, err := c.ConversationsSync(context.Background(), "abc"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(st.lastURL, "syncToken:abc") {
		t.Errorf("second request did not carry the token: %s", st.lastURL)
	}
}

// TestAllMessagesCapReportsIncomplete: capping is a truncation, and a
// --max-messages run recorded as a full sync would strand everything past the
// cap as permanently unseen. AllConversations already reported false here;
// messages must match.
func TestAllMessagesCapReportsIncomplete(t *testing.T) {
	body := func(token string, ids ...string) string {
		var els, inc []string
		for i, id := range ids {
			urn := "urn:li:msg_message:" + id
			els = append(els, fmt.Sprintf("%q", urn))
			inc = append(inc, fmt.Sprintf(`{"$type":"com.linkedin.messenger.Message",
				"entityUrn":%q,"deliveredAt":%d,"body":{"text":"hi"}}`, urn, 100+i))
		}
		return fmt.Sprintf(`{"data":{"data":{"messengerMessagesBySyncToken":{
			"*elements":[%s],"metadata":{"newSyncToken":%q}}}},"included":[%s]}`,
			strings.Join(els, ","), token, strings.Join(inc, ","))
	}
	c, _ := syncClient(t, body("t1", "m1", "m2", "m3"), body("t2"))
	msgs, complete, err := c.AllMessages(context.Background(), "2-abc", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 {
		t.Fatalf("got %d messages, want the cap of 2", len(msgs))
	}
	if complete {
		t.Error("complete = true after the cap truncated the walk, want false")
	}

	// Uncapped, the same stream is complete.
	c2, _ := syncClient(t, body("t1", "m1", "m2", "m3"), body("t2"))
	msgs, complete, err = c2.AllMessages(context.Background(), "2-abc", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 3 || !complete {
		t.Errorf("uncapped drain = %d messages complete=%v, want 3 and true", len(msgs), complete)
	}
}
