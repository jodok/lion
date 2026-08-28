package voyager

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/jodok/lion/internal/ratelimit"
)

// ErrPaginationStalled is returned by ConversationsPage and MessagesPage,
// alongside the page they just fetched, when the server's cursor failed to
// strictly decrease — the loop guard described on each function's doc
// comment. The page is real and safe to keep: it's whatever the server
// actually sent back. What the error means is narrower than "this call
// failed" — it's "there may be more data, but this endpoint isn't giving
// pagination a cursor that can reach it," so a caller can't tell the
// difference between "ignored createdBefore" and "genuinely exhausted" any
// other way. A caller that only wants a single page (see Conversations and
// Messages below, which never call these Page variants at all) is
// unaffected by this error by construction.
var ErrPaginationStalled = errors.New("voyager: pagination cursor did not decrease; more data may exist but is not reachable through this endpoint")

// NOTE: the messaging paths below (/messaging/conversations,
// /messaging/conversations/{id}/events) and body shapes are best-known from
// public reverse-engineering write-ups, not a live-verified capture. They are
// likely to have drifted (LinkedIn has migrated parts of messaging to
// GraphQL). Treat path/body construction here as the first place to look if
// `lion message` starts failing against a real account.

// conversationURNPrefix is the fixed prefix on a conversation entity URN
// (urn:li:fs_conversation:<id>). conversationIDFromURN strips it to recover
// the raw id the events endpoint path and a filesystem-safe filename both
// need.
const conversationURNPrefix = "urn:li:fs_conversation:"

// conversationIDFromURN extracts the raw id segment from a conversation
// entity URN. It returns "" when urn doesn't have the expected prefix,
// rather than guessing at an id from an unfamiliar shape — an empty ID is an
// obvious "not derivable" signal to a caller, not a plausible-looking wrong
// one.
func conversationIDFromURN(urn string) string {
	if !strings.HasPrefix(urn, conversationURNPrefix) {
		return ""
	}
	return strings.TrimPrefix(urn, conversationURNPrefix)
}

// Conversations returns the member's messaging threads, most recent first.
// When unreadOnly is true, threads without an unread flag are filtered out
// client-side (the endpoint does not appear to support a server-side filter
// reliably). max caps the number of returned conversations (0 = server
// default / no client-side cap).
func (c *Client) Conversations(ctx context.Context, unreadOnly bool, max int) ([]Conversation, error) {
	mailbox, err := c.mailbox(ctx)
	if err != nil {
		return nil, err
	}
	// The messaging GraphQL query takes no count: it returns the mailbox and
	// the cap is applied below, as it already was for unreadOnly.
	body, err := c.getMessagingGraphQL(ctx, queryIDMessengerConversations,
		"(mailboxUrn:"+mailbox+")")
	if err != nil {
		return nil, err
	}
	convs, err := decodeMessengerConversations(body)
	if err != nil {
		return nil, err
	}
	if !unreadOnly {
		if max > 0 && len(convs) > max {
			convs = convs[:max]
		}
		return convs, nil
	}
	var out []Conversation
	for _, cv := range convs {
		if cv.Unread {
			out = append(out, cv)
		}
		if max > 0 && len(out) >= max {
			break
		}
	}
	return out, nil
}

// ConversationsPage returns one page of conversations, plus the cursor to
// pass as createdBefore for the next, older page. createdBefore is the
// server's own pagination cursor — an epoch-milliseconds bound — and 0 means
// "most recent" (no bound). count caps the page size (0 = server default).
//
// next is the oldest item's UpdatedAt in this page, i.e. what a caller loops
// with to walk further back. It is 0, with a nil error, when there is
// nothing more to fetch because the page came back empty — that's genuine
// exhaustion. If instead the computed cursor fails to strictly decrease
// versus createdBefore, this returns the page it fetched (non-nil) alongside
// ErrPaginationStalled instead of silently reporting next as 0: those two
// situations used to be indistinguishable, which meant a caller looping on
// "next != 0" kept only the first page and reported success. The guard only
// applies once createdBefore is actually bounding the request (> 0): the
// very first call passes createdBefore=0, and "no bound" always counts as
// bigger than any real timestamp the server returns, so that call must not
// itself be flagged as non-decreasing.
// NOTE: this still calls the retired REST endpoint, which answers 500 for
// every request (see messenger.go). Conversations has moved to the messaging
// GraphQL surface, but that query takes no pagination variables — count and
// lastUpdatedBefore are both accepted and both ignored, verified by probing
// four combinations against the live API and getting byte-identical
// responses. LinkedIn's web app pages this surface by sync token, a
// different model from the timestamp cursor this signature is built around.
//
// Rewriting it to return a single page would quietly cost `lion sync` its
// paging, backfill, and resume behaviour, so the migration is left to its own
// change rather than smuggled in here. Until then sync fails against the dead
// endpoint, which is the status quo, and README records it.
func (c *Client) ConversationsPage(ctx context.Context, createdBefore int64, count int) ([]Conversation, int64, error) {
	q := url.Values{}
	if createdBefore > 0 {
		q.Set("createdBefore", fmt.Sprintf("%d", createdBefore))
	}
	if count > 0 {
		q.Set("count", fmt.Sprintf("%d", count))
	}
	body, err := c.get(ctx, "/messaging/conversations", q)
	if err != nil {
		return nil, 0, err
	}
	convs, err := decodeConversations(body)
	if err != nil {
		return nil, 0, err
	}
	if len(convs) == 0 {
		return nil, 0, nil
	}
	next := oldestConversationTime(convs)
	if createdBefore > 0 && next >= createdBefore {
		return convs, 0, ErrPaginationStalled
	}
	return convs, next, nil
}

// oldestConversationTime returns the minimum UpdatedAt across a page of
// conversations. The server is expected to return pages already ordered
// most-recent-first, but computing the minimum rather than trusting the last
// element's position means a cursor bug can't silently skip or repeat
// conversations if that ordering assumption ever turns out to be wrong.
func oldestConversationTime(convs []Conversation) int64 {
	oldest := convs[0].UpdatedAt
	for _, cv := range convs[1:] {
		if cv.UpdatedAt < oldest {
			oldest = cv.UpdatedAt
		}
	}
	return oldest
}

// conversationsRaw mirrors the shape of a /messaging/conversations response:
// a normalized envelope whose data.elements list conversation summaries that
// reference MiniProfile participants in the included[] array.
type conversationsRaw struct {
	Data struct {
		Elements []struct {
			EntityUrn      string `json:"entityUrn"`
			Unread         bool   `json:"unread"`
			LastActivityAt int64  `json:"lastActivityAt"`
			Participants   []struct {
				MiniProfile struct {
					EntityUrn string `json:"entityUrn"`
				} `json:"miniProfile"`
			} `json:"participants"`
			Events []struct {
				EventContent struct {
					MessageEvent struct {
						Body string `json:"body"`
					} `json:"com.linkedin.voyager.messaging.event.MessageEvent"`
				} `json:"eventContent"`
			} `json:"events"`
		} `json:"elements"`
	} `json:"data"`
}

// decodeConversations flattens a conversations list response, resolving
// participant MiniProfiles from the included[] index for display names.
func decodeConversations(body []byte) ([]Conversation, error) {
	_, idx, err := parseNormalized(body)
	if err != nil {
		return nil, err
	}
	var raw conversationsRaw
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("decode conversations: %w", err)
	}

	out := make([]Conversation, 0, len(raw.Data.Elements))
	for _, el := range raw.Data.Elements {
		// Built as one paired slice, not two parallel ones: each
		// participant reference always has a URN (captured directly off the
		// reference, not through included[] resolution), but its display
		// name only resolves when included[] happens to carry that
		// participant's MiniProfile. Appending both fields onto the same
		// Participant value — even when the name lookup below misses and
		// leaves Name empty — is what keeps a name from ever landing next
		// to the wrong participant's URN (see Participant's doc comment).
		var participants []Participant
		for _, p := range el.Participants {
			urn := p.MiniProfile.EntityUrn
			if urn == "" {
				continue
			}
			var name string
			if ent, ok := idx.get(urn); ok {
				var mp struct {
					FirstName string `json:"firstName"`
					LastName  string `json:"lastName"`
				}
				if err := decodeInto(ent, &mp); err == nil {
					name = strings.TrimSpace(mp.FirstName + " " + mp.LastName)
				}
			}
			participants = append(participants, Participant{Name: name, URN: urn})
		}
		var lastMsg string
		if n := len(el.Events); n > 0 {
			lastMsg = el.Events[n-1].EventContent.MessageEvent.Body
		}
		out = append(out, Conversation{
			URN:          el.EntityUrn,
			ID:           conversationIDFromURN(el.EntityUrn),
			Participants: participants,
			LastMessage:  lastMsg,
			UpdatedAt:    el.LastActivityAt,
			Unread:       el.Unread,
		})
	}
	// Shape-drift guard, mirroring decodeConnections/decodeFeed. Unlike those
	// decoders, this one reads data.elements directly rather than following
	// ordered URN references into included[] (LinkedIn nests the conversation
	// list inline), so the drift signal is different: included[] only holds
	// participant MiniProfiles at all because some conversation embedded
	// them. If that's true but data.elements produced zero conversations,
	// the top-level list shape has changed under this decoder — and without
	// this check, that looks identical to "this account has no
	// conversations" (exit 0), which for an export tool is the worst
	// possible failure mode: it looks like success.
	if len(out) == 0 && len(idx.ofType(typeMiniProfile)) > 0 {
		return nil, fmt.Errorf("conversations response shape not recognized: %d profile(s) present in included[] but no conversation decoded from data.elements; the decoder likely needs updating for a changed Voyager response shape: %w", len(idx.ofType(typeMiniProfile)), ErrNotFound)
	}
	return out, nil
}

// Messages returns up to max messages (0 = server default) in a conversation,
// oldest first, as returned by the events endpoint.
func (c *Client) Messages(ctx context.Context, conversationID string, max int) ([]Message, error) {
	if conversationID == "" {
		return nil, fmt.Errorf("empty conversation id")
	}
	urn, err := c.conversationURN(ctx, conversationID)
	if err != nil {
		return nil, err
	}
	// The messenger query takes no count; max is applied when decoding.
	body, err := c.getMessagingGraphQL(ctx, queryIDMessengerMessages,
		"(conversationUrn:"+url.QueryEscape(urn)+")")
	if err != nil {
		return nil, err
	}
	return decodeMessengerMessages(body, max)
}

// MessagesPage returns one page of a conversation's events, plus the cursor
// to pass as createdBefore for the next, older page. It follows the same
// contract as ConversationsPage — see that doc comment for createdBefore/next
// semantics, the ErrPaginationStalled case, and the loop-guard rationale —
// using each message's SentAt as the cursor instead of a conversation's
// UpdatedAt.
func (c *Client) MessagesPage(ctx context.Context, conversationID string, createdBefore int64, count int) ([]Message, int64, error) {
	if conversationID == "" {
		return nil, 0, fmt.Errorf("empty conversation id")
	}
	q := url.Values{}
	if createdBefore > 0 {
		q.Set("createdBefore", fmt.Sprintf("%d", createdBefore))
	}
	if count > 0 {
		q.Set("count", fmt.Sprintf("%d", count))
	}
	path := fmt.Sprintf("/messaging/conversations/%s/events", url.PathEscape(conversationID))
	body, err := c.get(ctx, path, q)
	if err != nil {
		return nil, 0, err
	}
	// max=0: a page's size is already bounded by the count query param sent
	// above, so decodeMessages should return everything the server sent
	// rather than applying a second, client-side cap.
	msgs, err := decodeMessages(body, 0)
	if err != nil {
		return nil, 0, err
	}
	if len(msgs) == 0 {
		return nil, 0, nil
	}
	next := oldestMessageTime(msgs)
	if createdBefore > 0 && next >= createdBefore {
		return msgs, 0, ErrPaginationStalled
	}
	return msgs, next, nil
}

// oldestMessageTime returns the minimum SentAt across a page of messages —
// see oldestConversationTime for why the minimum is computed rather than
// trusting element order.
func oldestMessageTime(msgs []Message) int64 {
	oldest := msgs[0].SentAt
	for _, m := range msgs[1:] {
		if m.SentAt < oldest {
			oldest = m.SentAt
		}
	}
	return oldest
}

// messagesRaw mirrors the shape of a conversation events response: a
// normalized envelope with data.elements holding individual message events.
// Each event references its sender via a MiniProfile in included[].
type messagesRaw struct {
	Data struct {
		Elements []struct {
			EntityUrn string `json:"entityUrn"`
			CreatedAt int64  `json:"createdAt"`
			From      struct {
				MiniProfile struct {
					EntityUrn string `json:"entityUrn"`
				} `json:"miniProfile"`
			} `json:"from"`
			EventContent struct {
				MessageEvent struct {
					Body string `json:"body"`
				} `json:"com.linkedin.voyager.messaging.event.MessageEvent"`
			} `json:"eventContent"`
		} `json:"elements"`
	} `json:"data"`
}

// decodeMessages flattens a conversation events response, resolving the
// sender's display name from the included[] MiniProfile index.
func decodeMessages(body []byte, max int) ([]Message, error) {
	_, idx, err := parseNormalized(body)
	if err != nil {
		return nil, err
	}
	var raw messagesRaw
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("decode messages: %w", err)
	}

	var out []Message
	for _, el := range raw.Data.Elements {
		from := el.From.MiniProfile.EntityUrn
		if ent, ok := idx.get(from); ok {
			var mp struct {
				FirstName string `json:"firstName"`
				LastName  string `json:"lastName"`
			}
			if err := decodeInto(ent, &mp); err == nil {
				if name := strings.TrimSpace(mp.FirstName + " " + mp.LastName); name != "" {
					from = name
				}
			}
		}
		out = append(out, Message{
			URN:     el.EntityUrn,
			From:    from,
			FromURN: el.From.MiniProfile.EntityUrn,
			Text:    el.EventContent.MessageEvent.Body,
			SentAt:  el.CreatedAt,
		})
		if max > 0 && len(out) >= max {
			break
		}
	}
	// Shape-drift guard — see decodeConversations. included[] only carries
	// sender MiniProfiles because some event referenced one, so zero decoded
	// messages alongside profiles present means data.elements itself no
	// longer matches this decoder, not that the conversation is empty.
	if len(out) == 0 && len(idx.ofType(typeMiniProfile)) > 0 {
		return nil, fmt.Errorf("messages response shape not recognized: %d profile(s) present in included[] but no message decoded from data.elements; the decoder likely needs updating for a changed Voyager response shape: %w", len(idx.ofType(typeMiniProfile)), ErrNotFound)
	}
	return out, nil
}

// SendMessage posts a new message into an existing conversation. Under
// dry-run it returns nil without making a request.
func (c *Client) SendMessage(ctx context.Context, conversationID, text string) error {
	if conversationID == "" {
		return fmt.Errorf("empty conversation id")
	}
	if strings.TrimSpace(text) == "" {
		return fmt.Errorf("empty message text")
	}
	path := fmt.Sprintf("/messaging/conversations/%s/events?action=create", url.PathEscape(conversationID))
	payload := map[string]any{
		"eventCreate": map[string]any{
			"value": map[string]any{
				"com.linkedin.voyager.messaging.create.MessageCreate": map[string]any{
					"body": text,
				},
			},
		},
	}
	buf, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = c.post(ctx, path, bytes.NewReader(buf), ratelimit.Write)
	return err
}

// SendMessageToProfile creates a new conversation thread with a single
// recipient (identified by their profile URN, e.g.
// "urn:li:fs_miniProfile:ACoAAA...") and sends the given text as the first
// message. Under dry-run it returns nil without making a request.
func (c *Client) SendMessageToProfile(ctx context.Context, profileURN, text string) error {
	if profileURN == "" {
		return fmt.Errorf("empty profile urn")
	}
	if strings.TrimSpace(text) == "" {
		return fmt.Errorf("empty message text")
	}
	payload := map[string]any{
		"conversationCreate": map[string]any{
			"recipients": []string{profileURN},
			"eventCreate": map[string]any{
				"value": map[string]any{
					"com.linkedin.voyager.messaging.create.MessageCreate": map[string]any{
						"body": text,
					},
				},
			},
		},
	}
	buf, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = c.post(ctx, "/messaging/conversations?action=create", bytes.NewReader(buf), ratelimit.Write)
	return err
}
