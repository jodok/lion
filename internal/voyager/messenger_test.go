package voyager

import (
	"context"
	"strings"
	"testing"
)

// TestConversationsIgnoresUnlistedConversations pins that membership comes
// from the collection's *elements rather than from sweeping included[].
//
// included[] is the flat index of every object a response touches, so a
// thread referenced for context but not part of the result would otherwise be
// indistinguishable from a hit and would appear in `message list`.
func TestConversationsIgnoresUnlistedConversations(t *testing.T) {
	body := []byte(`{
	  "data": {"data": {"messengerConversationsBySyncToken": {
	    "*elements": ["urn:li:msg_conversation:(urn:li:fsd_profile:me,2-listed)"]
	  }}},
	  "included": [
	    {"$type":"com.linkedin.messenger.Conversation",
	     "entityUrn":"urn:li:msg_conversation:(urn:li:fsd_profile:me,2-listed)",
	     "lastActivityAt":2000,"read":true},
	    {"$type":"com.linkedin.messenger.Conversation",
	     "entityUrn":"urn:li:msg_conversation:(urn:li:fsd_profile:me,2-unlisted)",
	     "lastActivityAt":3000,"read":true}
	  ]
	}`)
	convs, err := decodeMessengerConversations(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(convs) != 1 {
		t.Fatalf("got %d conversations, want only the one in *elements", len(convs))
	}
	if convs[0].ID != "2-listed" {
		t.Errorf("ID = %q, want 2-listed", convs[0].ID)
	}
}

// TestConversationsFallBackWhenCollectionKeyMoves covers the defensive path:
// the wrapper key is LinkedIn's to rename, and returning nothing because it
// moved would be indistinguishable from an empty mailbox.
func TestConversationsFallBackWhenCollectionKeyMoves(t *testing.T) {
	body := []byte(`{
	  "data": {"data": {"someRenamedWrapper": {"$type": "x"}}},
	  "included": [
	    {"$type":"com.linkedin.messenger.Conversation",
	     "entityUrn":"urn:li:msg_conversation:(urn:li:fsd_profile:me,2-abc)",
	     "lastActivityAt":2000,"read":true}
	  ]
	}`)
	convs, err := decodeMessengerConversations(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(convs) != 1 {
		t.Fatalf("got %d conversations, want the fallback to find 1", len(convs))
	}
}

// TestUnreadFallsBackToUnreadCount: LinkedIn omits `read` on some
// conversations, and a thread reporting unread messages must not be filtered
// out of `message list --unread` just because that flag was absent.
func TestUnreadFallsBackToUnreadCount(t *testing.T) {
	body := []byte(`{
	  "data": {"data": {"c": {"*elements": [
	    "urn:li:msg_conversation:(urn:li:fsd_profile:me,2-a)",
	    "urn:li:msg_conversation:(urn:li:fsd_profile:me,2-b)",
	    "urn:li:msg_conversation:(urn:li:fsd_profile:me,2-c)"
	  ]}}},
	  "included": [
	    {"$type":"com.linkedin.messenger.Conversation",
	     "entityUrn":"urn:li:msg_conversation:(urn:li:fsd_profile:me,2-a)",
	     "lastActivityAt":3000,"unreadCount":3},
	    {"$type":"com.linkedin.messenger.Conversation",
	     "entityUrn":"urn:li:msg_conversation:(urn:li:fsd_profile:me,2-b)",
	     "lastActivityAt":2000,"unreadCount":0},
	    {"$type":"com.linkedin.messenger.Conversation",
	     "entityUrn":"urn:li:msg_conversation:(urn:li:fsd_profile:me,2-c)",
	     "lastActivityAt":1000,"read":true,"unreadCount":5}
	  ]
	}`)
	convs, err := decodeMessengerConversations(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(convs) != 3 {
		t.Fatalf("got %d conversations, want 3", len(convs))
	}
	// read absent, unreadCount > 0 -> unread.
	if !convs[0].Unread {
		t.Error("conversation with unreadCount=3 and no read flag reported as read")
	}
	// read absent, unreadCount 0 -> read (the conservative default).
	if convs[1].Unread {
		t.Error("conversation with unreadCount=0 and no read flag reported as unread")
	}
	// An explicit read flag wins over unreadCount.
	if convs[2].Unread {
		t.Error("explicit read:true was overridden by unreadCount")
	}
}

// TestConversationURNRejectsLegacyURN: pre-migration stores hold
// urn:li:fs_conversation:… values, and splicing one in whole would build a
// nested URN that LinkedIn rejects with an opaque error.
func TestConversationURNRejectsLegacyURN(t *testing.T) {
	c, _ := newTestClient(t, map[string]string{"/me": "me.json"})
	_, err := c.conversationURN(context.Background(), "urn:li:fs_conversation:2-YWJjMTIz")
	if err == nil {
		t.Fatal("legacy conversation URN was accepted, want a clear rejection")
	}
	if !strings.Contains(err.Error(), "not a conversation URN") {
		t.Errorf("error = %q, want it to name the problem", err)
	}

	// A full new-style URN passes through untouched, and a bare thread id is
	// wrapped with the mailbox.
	got, err := c.conversationURN(context.Background(),
		"urn:li:msg_conversation:(urn:li:fsd_profile:me,2-abc)")
	if err != nil {
		t.Fatal(err)
	}
	if got != "urn:li:msg_conversation:(urn:li:fsd_profile:me,2-abc)" {
		t.Errorf("full URN was rewritten: %q", got)
	}
	got, err = c.conversationURN(context.Background(), "2-abc")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got, "urn:li:msg_conversation:(urn:li:fsd_profile:") ||
		!strings.HasSuffix(got, ",2-abc)") {
		t.Errorf("thread id built the wrong URN: %q", got)
	}
}

// TestMailboxResolvedOnce is the cost guard: the mailbox URN is derived from
// an id that cannot change while a Client lives, so repeated messaging calls
// must not each spend a Voyager request re-fetching it. The rate limiter
// meters reads so a real account does not look automated.
func TestMailboxResolvedOnce(t *testing.T) {
	c, ft := newTestClient(t, map[string]string{
		"/me":                              "me.json",
		"/voyagerMessagingGraphQL/graphql": "messenger_conversations.json",
	})
	for i := 0; i < 3; i++ {
		if _, err := c.Conversations(context.Background(), false, 0); err != nil {
			t.Fatal(err)
		}
	}
	var meCalls int
	for _, u := range ft.requested {
		if strings.HasSuffix(u, "/me") {
			meCalls++
		}
	}
	if meCalls != 1 {
		t.Errorf("/me requested %d times across 3 calls, want 1 (memoized)", meCalls)
	}
}
