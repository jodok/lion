package store

import (
	"context"
	"testing"
)

// TestUpsertDoesNotEraseDetailWithBlanks covers a data-loss path specific to
// how LinkedIn paginates. Its normalized responses carry included[] per page,
// so the same message or conversation seen again from a different page can
// fail to resolve its sender/participants and come back blank. Overwriting
// with those blanks would quietly downgrade a complete archive on every
// re-sync, and the loss is permanent once the page that had the detail is out
// of reach.
func TestUpsertDoesNotEraseDetailWithBlanks(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	const convID = "2-blank"

	// First pass: everything resolved.
	if err := s.WithTx(ctx, func(tx *Tx) error {
		c := Conversation{
			ID: convID, URN: "urn:li:fs_conversation:" + convID,
			Participants: []Participant{{Name: "Grace Hopper", URN: "urn:li:fs_miniProfile:ACoAAA1"}},
			UpdatedAt:    2000,
		}
		if err := tx.UpsertConversation(ctx, c, 1000); err != nil {
			return err
		}
		_, err := tx.RecordMessagePage(ctx, convID, []Message{{
			URN: "urn:li:msg:blank", ConversationID: convID,
			SenderName: "Grace Hopper", SenderURN: "urn:li:fs_miniProfile:ACoAAA1",
			SentAt: 1500, Body: "the original body",
		}}, 2000)
		return err
	}); err != nil {
		t.Fatal(err)
	}

	// Second pass: same entities, nothing resolved.
	if err := s.WithTx(ctx, func(tx *Tx) error {
		c := Conversation{ID: convID, URN: "urn:li:fs_conversation:" + convID, UpdatedAt: 2000}
		if err := tx.UpsertConversation(ctx, c, 1000); err != nil {
			return err
		}
		_, err := tx.RecordMessagePage(ctx, convID, []Message{{
			URN: "urn:li:msg:blank", ConversationID: convID, SentAt: 1500,
		}}, 2000)
		return err
	}); err != nil {
		t.Fatal(err)
	}

	msgs, err := s.Messages(ctx, MessageFilter{ConversationID: convID})
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 {
		t.Fatalf("got %d messages, want 1", len(msgs))
	}
	if msgs[0].SenderURN != "urn:li:fs_miniProfile:ACoAAA1" {
		t.Errorf("SenderURN = %q, want it preserved through a blank re-fetch", msgs[0].SenderURN)
	}
	if msgs[0].SenderName != "Grace Hopper" {
		t.Errorf("SenderName = %q, want it preserved", msgs[0].SenderName)
	}
	if msgs[0].Body != "the original body" {
		t.Errorf("Body = %q, want it preserved", msgs[0].Body)
	}

	conv, ok, err := s.Conversation(ctx, convID)
	if err != nil || !ok {
		t.Fatalf("Conversation(%q) = (_, %v, %v)", convID, ok, err)
	}
	if len(conv.Participants) != 1 || conv.Participants[0].Name != "Grace Hopper" {
		t.Errorf("Participants = %#v, want the resolved list preserved", conv.Participants)
	}
}

// TestUpsertPreservesResolvedNameOnPartialResync covers the per-participant
// version of the blank-erasure bug: decodeConversations emits
// Participant{Name:"", URN} for a member whose MiniProfile wasn't in a given
// page, so a routine partially-resolved re-sync must not blank a name an
// earlier page resolved.
func TestUpsertPreservesResolvedNameOnPartialResync(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	const id = "2-partial"

	if err := s.WithTx(ctx, func(tx *Tx) error {
		return tx.UpsertConversation(ctx, Conversation{
			ID: id, URN: "urn:li:fs_conversation:" + id, UpdatedAt: 1,
			Participants: []Participant{
				{Name: "Grace Hopper", URN: "urn:li:fs_miniProfile:A1"},
				{Name: "Ada Lovelace", URN: "urn:li:fs_miniProfile:A2"},
			},
		}, 1)
	}); err != nil {
		t.Fatal(err)
	}

	// A later page resolves A2 but not A1.
	if err := s.WithTx(ctx, func(tx *Tx) error {
		return tx.UpsertConversation(ctx, Conversation{
			ID: id, URN: "urn:li:fs_conversation:" + id, UpdatedAt: 2,
			Participants: []Participant{
				{Name: "", URN: "urn:li:fs_miniProfile:A1"},
				{Name: "Ada Lovelace", URN: "urn:li:fs_miniProfile:A2"},
			},
		}, 1)
	}); err != nil {
		t.Fatal(err)
	}

	conv, ok, err := s.Conversation(ctx, id)
	if err != nil || !ok {
		t.Fatalf("Conversation = (_, %v, %v)", ok, err)
	}
	byURN := map[string]string{}
	for _, p := range conv.Participants {
		byURN[p.URN] = p.Name
	}
	if byURN["urn:li:fs_miniProfile:A1"] != "Grace Hopper" {
		t.Errorf("A1 name = %q, want it preserved through the blank re-sync", byURN["urn:li:fs_miniProfile:A1"])
	}
	if byURN["urn:li:fs_miniProfile:A2"] != "Ada Lovelace" {
		t.Errorf("A2 name = %q, want Ada Lovelace", byURN["urn:li:fs_miniProfile:A2"])
	}
}
