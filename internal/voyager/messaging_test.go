package voyager

import (
	"context"
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
