package voyager

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/jodok/lion/internal/ratelimit"
)

// NOTE: the messaging paths below (/messaging/conversations,
// /messaging/conversations/{id}/events) and body shapes are best-known from
// public reverse-engineering write-ups, not a live-verified capture. They are
// likely to have drifted (LinkedIn has migrated parts of messaging to
// GraphQL). Treat path/body construction here as the first place to look if
// `lion message` starts failing against a real account.

// Conversations returns the member's messaging threads, most recent first.
// When unreadOnly is true, threads without an unread flag are filtered out
// client-side (the endpoint does not appear to support a server-side filter
// reliably). max caps the number of returned conversations (0 = server
// default / no client-side cap).
func (c *Client) Conversations(ctx context.Context, unreadOnly bool, max int) ([]Conversation, error) {
	q := url.Values{}
	if max > 0 {
		q.Set("count", fmt.Sprintf("%d", max))
	}
	body, err := c.get(ctx, "/messaging/conversations", q)
	if err != nil {
		return nil, err
	}
	convs, err := decodeConversations(body)
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
		var names []string
		for _, p := range el.Participants {
			if ent, ok := idx.get(p.MiniProfile.EntityUrn); ok {
				var mp struct {
					FirstName string `json:"firstName"`
					LastName  string `json:"lastName"`
				}
				if err := decodeInto(ent, &mp); err == nil {
					if name := strings.TrimSpace(mp.FirstName + " " + mp.LastName); name != "" {
						names = append(names, name)
					}
				}
			}
		}
		var lastMsg string
		if n := len(el.Events); n > 0 {
			lastMsg = el.Events[n-1].EventContent.MessageEvent.Body
		}
		out = append(out, Conversation{
			URN:          el.EntityUrn,
			Participants: names,
			LastMessage:  lastMsg,
			UpdatedAt:    el.LastActivityAt,
			Unread:       el.Unread,
		})
	}
	return out, nil
}

// Messages returns up to max messages (0 = server default) in a conversation,
// oldest first, as returned by the events endpoint.
func (c *Client) Messages(ctx context.Context, conversationID string, max int) ([]Message, error) {
	if conversationID == "" {
		return nil, fmt.Errorf("empty conversation id")
	}
	q := url.Values{}
	if max > 0 {
		q.Set("count", fmt.Sprintf("%d", max))
	}
	path := fmt.Sprintf("/messaging/conversations/%s/events", url.PathEscape(conversationID))
	body, err := c.get(ctx, path, q)
	if err != nil {
		return nil, err
	}
	return decodeMessages(body, max)
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
			URN:    el.EntityUrn,
			From:   from,
			Text:   el.EventContent.MessageEvent.Body,
			SentAt: el.CreatedAt,
		})
		if max > 0 && len(out) >= max {
			break
		}
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
