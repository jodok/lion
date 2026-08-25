package voyager

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestConversations(t *testing.T) {
	c, _ := newTestClient(t, map[string]string{"/messaging/conversations": "conversations.json"})
	convs, err := c.Conversations(context.Background(), false, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(convs) != 2 {
		t.Fatalf("got %d conversations, want 2", len(convs))
	}
	first := convs[0]
	if first.URN != "urn:li:fs_conversation:2-YWJjMTIz" {
		t.Errorf("urn = %q", first.URN)
	}
	if len(first.Participants) != 1 || first.Participants[0] != "Grace Hopper" {
		t.Errorf("participants = %+v", first.Participants)
	}
	if first.LastMessage != "Are you free for a quick call this week?" {
		t.Errorf("last message = %q", first.LastMessage)
	}
	if !first.Unread {
		t.Error("expected first conversation to be unread")
	}
}

func TestConversationsUnreadOnly(t *testing.T) {
	c, _ := newTestClient(t, map[string]string{"/messaging/conversations": "conversations.json"})
	convs, err := c.Conversations(context.Background(), true, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(convs) != 1 {
		t.Fatalf("got %d unread conversations, want 1", len(convs))
	}
	if convs[0].Participants[0] != "Grace Hopper" {
		t.Errorf("unread conversation = %+v", convs[0])
	}
}

func TestConversationsRespectsMax(t *testing.T) {
	c, _ := newTestClient(t, map[string]string{"/messaging/conversations": "conversations.json"})
	convs, err := c.Conversations(context.Background(), false, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(convs) != 1 {
		t.Fatalf("got %d, want 1", len(convs))
	}
}

func TestMessages(t *testing.T) {
	c, _ := newTestClient(t, map[string]string{"/messaging/conversations/2-YWJjMTIz/events": "messages.json"})
	msgs, err := c.Messages(context.Background(), "2-YWJjMTIz", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 {
		t.Fatalf("got %d messages, want 2", len(msgs))
	}
	if msgs[0].From != "Grace Hopper" {
		t.Errorf("first message from = %q", msgs[0].From)
	}
	if msgs[1].From != "Ada Lovelace" {
		t.Errorf("second message from = %q", msgs[1].From)
	}
	if msgs[1].Text != "Are you free for a quick call this week?" {
		t.Errorf("second message text = %q", msgs[1].Text)
	}
}

func TestMessagesRespectsMax(t *testing.T) {
	c, _ := newTestClient(t, map[string]string{"/messaging/conversations/2-YWJjMTIz/events": "messages.json"})
	msgs, err := c.Messages(context.Background(), "2-YWJjMTIz", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 {
		t.Fatalf("got %d, want 1", len(msgs))
	}
}

// TestSendMessageDryRun verifies that SendMessage under --dry-run makes no
// HTTP call and returns nil, mirroring the client-wide dry-run contract.
func TestSendMessageDryRun(t *testing.T) {
	ft := &fixtureTransport{routes: map[string]string{}}
	c := New("li_at_test", `"jsession_test"`, WithTransport(ft), WithDryRun(true))
	if err := c.SendMessage(context.Background(), "2-YWJjMTIz", "hello"); err != nil {
		t.Fatal(err)
	}
	if ft.lastReq != nil {
		t.Error("expected no HTTP request under dry-run")
	}
}

func TestSendMessageToProfileDryRun(t *testing.T) {
	ft := &fixtureTransport{routes: map[string]string{}}
	c := New("li_at_test", `"jsession_test"`, WithTransport(ft), WithDryRun(true))
	if err := c.SendMessageToProfile(context.Background(), "urn:li:fs_miniProfile:ACoAAA1", "hello"); err != nil {
		t.Fatal(err)
	}
	if ft.lastReq != nil {
		t.Error("expected no HTTP request under dry-run")
	}
}

// TestConversationsIncludesIDAndParticipantURNs pins the richer-identity
// fields added alongside the existing display-name-only Conversation type: an
// archive/export consumer needs a stable id and participant identity, not
// just a label.
func TestConversationsIncludesIDAndParticipantURNs(t *testing.T) {
	c, _ := newTestClient(t, map[string]string{"/messaging/conversations": "conversations.json"})
	convs, err := c.Conversations(context.Background(), false, 0)
	if err != nil {
		t.Fatal(err)
	}
	first := convs[0]
	if first.ID != "2-YWJjMTIz" {
		t.Errorf("ID = %q, want 2-YWJjMTIz (parsed from the fs_conversation URN)", first.ID)
	}
	if len(first.ParticipantURNs) != 1 || first.ParticipantURNs[0] != "urn:li:fs_miniProfile:ACoAAA1" {
		t.Errorf("ParticipantURNs = %+v, want [urn:li:fs_miniProfile:ACoAAA1]", first.ParticipantURNs)
	}
}

// TestConversationIDFromURNMissingPrefix pins the deliberate empty return: an
// unfamiliar URN shape must not produce a guessed id that looks plausible.
func TestConversationIDFromURNMissingPrefix(t *testing.T) {
	if got := conversationIDFromURN("not-a-urn"); got != "" {
		t.Errorf("conversationIDFromURN(%q) = %q, want empty", "not-a-urn", got)
	}
}

// TestMessagesIncludesFromURN is the Message-side counterpart of
// TestConversationsIncludesIDAndParticipantURNs.
func TestMessagesIncludesFromURN(t *testing.T) {
	c, _ := newTestClient(t, map[string]string{"/messaging/conversations/2-YWJjMTIz/events": "messages.json"})
	msgs, err := c.Messages(context.Background(), "2-YWJjMTIz", 0)
	if err != nil {
		t.Fatal(err)
	}
	if msgs[0].FromURN != "urn:li:fs_miniProfile:ACoAAA1" {
		t.Errorf("msgs[0].FromURN = %q, want urn:li:fs_miniProfile:ACoAAA1", msgs[0].FromURN)
	}
	if msgs[1].FromURN != "urn:li:fs_miniProfile:ACoAAAme" {
		t.Errorf("msgs[1].FromURN = %q, want urn:li:fs_miniProfile:ACoAAAme", msgs[1].FromURN)
	}
}

// jsonResponses wraps a sequence of raw JSON bodies as 200 responses, for use
// with the package's sequenceTransport (client_test.go) — which serves them
// in order and repeats the last one forever once exhausted. That repeat
// behavior is exactly what's needed to simulate a server that ignores the
// pagination cursor entirely (every call keeps getting the same page), as
// well as a normal paginating endpoint (each call gets the next page).
func jsonResponses(bodies ...string) []*Response {
	out := make([]*Response, len(bodies))
	for i, b := range bodies {
		out[i] = &Response{StatusCode: 200, Body: []byte(b)}
	}
	return out
}

// convEntry is a minimal conversation fixture entry: just enough to drive
// ConversationsPage's cursor arithmetic and ID parsing.
type convEntry struct {
	id   string
	last int64
}

// conversationsPageJSON builds a minimal /messaging/conversations response
// body from entries, in the given order. No participants/events/included are
// needed for the pagination tests, since they only exercise ID and
// UpdatedAt.
func conversationsPageJSON(entries []convEntry) string {
	els := make([]string, 0, len(entries))
	for _, e := range entries {
		els = append(els, fmt.Sprintf(
			`{"entityUrn":"urn:li:fs_conversation:%s","unread":false,"lastActivityAt":%d,"participants":[],"events":[]}`,
			e.id, e.last))
	}
	return fmt.Sprintf(`{"data":{"elements":[%s]}}`, strings.Join(els, ","))
}

// TestConversationsPagePaginatesAcrossPages is the required paged-fetch
// test: a fixture transport serving two pages then an empty one must return
// every item across pages, in order, and the loop must terminate on the
// empty page (next == 0).
func TestConversationsPagePaginatesAcrossPages(t *testing.T) {
	page1 := conversationsPageJSON([]convEntry{{"2-aaa", 5000}, {"2-bbb", 4000}})
	page2 := conversationsPageJSON([]convEntry{{"2-ccc", 3000}, {"2-ddd", 2000}})
	page3 := conversationsPageJSON(nil)
	st := &sequenceTransport{responses: jsonResponses(page1, page2, page3)}
	c := New("li_at_test", `"jsession_test"`, WithTransport(st), WithLimiter(noopLimiter()))

	var gotIDs []string
	cursor := int64(0)
	pages := 0
	for {
		convs, next, err := c.ConversationsPage(context.Background(), cursor, 2)
		if err != nil {
			t.Fatal(err)
		}
		pages++
		for _, cv := range convs {
			gotIDs = append(gotIDs, cv.ID)
		}
		if next == 0 {
			break
		}
		cursor = next
		if pages > 10 {
			t.Fatal("loop did not terminate")
		}
	}
	if pages != 3 {
		t.Fatalf("pages fetched = %d, want 3 (two data pages plus the terminating empty page)", pages)
	}
	want := []string{"2-aaa", "2-bbb", "2-ccc", "2-ddd"}
	if len(gotIDs) != len(want) {
		t.Fatalf("got %v, want %v", gotIDs, want)
	}
	for i := range want {
		if gotIDs[i] != want[i] {
			t.Errorf("item %d = %q, want %q (must preserve order across pages)", i, gotIDs[i], want[i])
		}
	}
}

// TestConversationsPageLoopGuardTerminates is the required loop-guard test: a
// transport that ignores createdBefore and keeps returning the same page
// must not spin forever — the cursor stops strictly decreasing on the
// second call, which must force next to 0.
func TestConversationsPageLoopGuardTerminates(t *testing.T) {
	page := conversationsPageJSON([]convEntry{{"2-aaa", 5000}, {"2-bbb", 4000}})
	st := &sequenceTransport{responses: jsonResponses(page)} // same body every call
	c := New("li_at_test", `"jsession_test"`, WithTransport(st), WithLimiter(noopLimiter()))

	cursor := int64(0)
	pages := 0
	for {
		convs, next, err := c.ConversationsPage(context.Background(), cursor, 2)
		if err != nil {
			t.Fatal(err)
		}
		pages++
		if len(convs) == 0 {
			t.Fatal("the ignored-cursor page unexpectedly came back empty")
		}
		if next == 0 {
			break
		}
		cursor = next
		if pages > 10 {
			t.Fatal("loop did not terminate against a server ignoring createdBefore")
		}
	}
	if pages != 2 {
		t.Errorf("pages fetched = %d, want 2 (the real page, then the guard trips on the repeat)", pages)
	}
}

// TestDecodeConversationsShapeDrift is the required shape-drift test: a
// payload whose included[] clearly holds a conversation participant's
// MiniProfile (which only happens in a real response because some
// conversation referenced it), but whose top-level list shape no longer
// matches data.elements, must be reported as an error rather than silently
// decoded as "zero conversations" — the latter is indistinguishable from a
// genuinely empty account and, for an export tool, the worst possible wrong
// answer.
func TestDecodeConversationsShapeDrift(t *testing.T) {
	body := []byte(`{
		"data": {"somethingElse": []},
		"included": [
			{"$type": "com.linkedin.voyager.identity.shared.MiniProfile", "entityUrn": "urn:li:fs_miniProfile:ACoAAA1", "firstName": "Grace", "lastName": "Hopper"}
		]
	}`)
	_, err := decodeConversations(body)
	if err == nil {
		t.Fatal("expected a shape-drift error, got nil")
	}
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("error = %v, want it to wrap ErrNotFound", err)
	}
}

// msgEntry is a minimal message fixture entry for MessagesPage's cursor
// arithmetic.
type msgEntry struct {
	urn  string
	sent int64
}

// messagesPageJSON builds a minimal conversation-events response body from
// entries, in the given order.
func messagesPageJSON(entries []msgEntry) string {
	els := make([]string, 0, len(entries))
	for _, e := range entries {
		els = append(els, fmt.Sprintf(
			`{"entityUrn":"%s","createdAt":%d,"from":{"miniProfile":{"entityUrn":"urn:li:fs_miniProfile:ACoAAA1"}},"eventContent":{"com.linkedin.voyager.messaging.event.MessageEvent":{"body":"hi"}}}`,
			e.urn, e.sent))
	}
	return fmt.Sprintf(`{"data":{"elements":[%s]}}`, strings.Join(els, ","))
}

// TestMessagesPagePaginatesAcrossPages is the Message-side counterpart of
// TestConversationsPagePaginatesAcrossPages.
func TestMessagesPagePaginatesAcrossPages(t *testing.T) {
	page1 := messagesPageJSON([]msgEntry{{"urn:li:fs_event:(2-abc,1)", 5000}, {"urn:li:fs_event:(2-abc,2)", 4000}})
	page2 := messagesPageJSON([]msgEntry{{"urn:li:fs_event:(2-abc,3)", 3000}, {"urn:li:fs_event:(2-abc,4)", 2000}})
	page3 := messagesPageJSON(nil)
	st := &sequenceTransport{responses: jsonResponses(page1, page2, page3)}
	c := New("li_at_test", `"jsession_test"`, WithTransport(st), WithLimiter(noopLimiter()))

	var gotURNs []string
	cursor := int64(0)
	pages := 0
	for {
		msgs, next, err := c.MessagesPage(context.Background(), "2-abc", cursor, 2)
		if err != nil {
			t.Fatal(err)
		}
		pages++
		for _, m := range msgs {
			gotURNs = append(gotURNs, m.URN)
		}
		if next == 0 {
			break
		}
		cursor = next
		if pages > 10 {
			t.Fatal("loop did not terminate")
		}
	}
	if pages != 3 {
		t.Fatalf("pages fetched = %d, want 3", pages)
	}
	want := []string{
		"urn:li:fs_event:(2-abc,1)", "urn:li:fs_event:(2-abc,2)",
		"urn:li:fs_event:(2-abc,3)", "urn:li:fs_event:(2-abc,4)",
	}
	if len(gotURNs) != len(want) {
		t.Fatalf("got %v, want %v", gotURNs, want)
	}
	for i := range want {
		if gotURNs[i] != want[i] {
			t.Errorf("item %d = %q, want %q (must preserve order across pages)", i, gotURNs[i], want[i])
		}
	}
}

// TestMessagesPageLoopGuardTerminates is the Message-side counterpart of
// TestConversationsPageLoopGuardTerminates.
func TestMessagesPageLoopGuardTerminates(t *testing.T) {
	page := messagesPageJSON([]msgEntry{{"urn:li:fs_event:(2-abc,1)", 5000}, {"urn:li:fs_event:(2-abc,2)", 4000}})
	st := &sequenceTransport{responses: jsonResponses(page)}
	c := New("li_at_test", `"jsession_test"`, WithTransport(st), WithLimiter(noopLimiter()))

	cursor := int64(0)
	pages := 0
	for {
		msgs, next, err := c.MessagesPage(context.Background(), "2-abc", cursor, 2)
		if err != nil {
			t.Fatal(err)
		}
		pages++
		if len(msgs) == 0 {
			t.Fatal("the ignored-cursor page unexpectedly came back empty")
		}
		if next == 0 {
			break
		}
		cursor = next
		if pages > 10 {
			t.Fatal("loop did not terminate against a server ignoring createdBefore")
		}
	}
	if pages != 2 {
		t.Errorf("pages fetched = %d, want 2", pages)
	}
}

// TestDecodeMessagesShapeDrift is the Message-side counterpart of
// TestDecodeConversationsShapeDrift.
func TestDecodeMessagesShapeDrift(t *testing.T) {
	body := []byte(`{
		"data": {"somethingElse": []},
		"included": [
			{"$type": "com.linkedin.voyager.identity.shared.MiniProfile", "entityUrn": "urn:li:fs_miniProfile:ACoAAA1", "firstName": "Grace", "lastName": "Hopper"}
		]
	}`)
	_, err := decodeMessages(body, 0)
	if err == nil {
		t.Fatal("expected a shape-drift error, got nil")
	}
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("error = %v, want it to wrap ErrNotFound", err)
	}
}
