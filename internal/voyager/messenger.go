package voyager

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"
)

// LinkedIn retired the REST messaging surface. /voyager/api/messaging/
// conversations answers 500 for every request, with or without parameters —
// not 404 or 410, so it reads like a server fault rather than a removal, and
// callers had no way to tell. Messaging now lives on its own GraphQL
// endpoint, which is what the web app itself calls:
//
//	/voyager/api/voyagerMessagingGraphQL/graphql
//	  ?queryId=messengerConversations.<hash>
//	  &variables=(mailboxUrn:urn%3Ali%3Afsd_profile%3A<memberId>)
//
// Note the endpoint is NOT the /voyager/api/graphql the rest of the modern
// surface uses, and it takes no includeWebMetadata parameter.
const messagingGraphQLPath = "/voyagerMessagingGraphQL/graphql"

// Messaging queryId hashes, pinned to a LinkedIn web-app build. Like the
// ones in graphql.go these ROTATE, and a stale hash comes back as a 400.
// Observed 2026-08-28 by loading linkedin.com/messaging in the browser lion
// drives and reading the requests the page issued — which is now the
// cheapest way to refresh them, and a good argument for teaching lion to
// harvest them itself rather than pinning them here.
const (
	// queryIDMessengerConversations lists the mailbox's conversations.
	queryIDMessengerConversations = "messengerConversations.0d5e6781bbee71c3e51c8843c6519f48"
	// queryIDMessengerMessages lists one conversation's messages.
	queryIDMessengerMessages = "messengerMessages.5846eeb71c981f11e0134cb6626cc314"
)

// getMessagingGraphQL issues a GET against the messaging GraphQL endpoint.
// variables is the pre-assembled Rest.li-encoded string including its
// surrounding parentheses; see graphql.go for why it is not form-encoded.
func (c *Client) getMessagingGraphQL(ctx context.Context, queryID, variables string) ([]byte, error) {
	return c.getRawQuery(ctx, messagingGraphQLPath,
		"queryId="+queryID+"&variables="+variables)
}

// mailboxURN builds the mailboxUrn variable from the member URN Me returns.
//
// Me reports a urn:li:fs_miniProfile:<id> (or bare id) while messaging wants
// urn:li:fsd_profile:<id>, so this takes the trailing id segment and rebuilds
// it rather than assuming either spelling.
func mailboxURN(memberURN string) string {
	id := memberURN
	if i := strings.LastIndex(id, ":"); i >= 0 {
		id = id[i+1:]
	}
	return url.QueryEscape("urn:li:fsd_profile:" + id)
}

// messengerEntity is one entry of the response's included[] index. The
// messaging GraphQL response is the normalized+json form: the query result
// holds URN references, and every actual object — conversations, messages,
// participants — is flattened into included[], keyed by entityUrn and tagged
// with $type. One struct covers all three types because the fields are
// disjoint; $type decides which half is meaningful.
type messengerEntity struct {
	Type      string `json:"$type"`
	EntityURN string `json:"entityUrn"`

	// Conversation
	LastActivityAt int64    `json:"lastActivityAt"`
	CreatedAt      int64    `json:"createdAt"`
	Read           *bool    `json:"read"`
	UnreadCount    int      `json:"unreadCount"`
	Participants   []string `json:"*conversationParticipants"`
	Messages       struct {
		Elements []string `json:"*elements"`
	} `json:"messages"`

	// Message
	Body struct {
		Text string `json:"text"`
	} `json:"body"`
	DeliveredAt int64  `json:"deliveredAt"`
	Sender      string `json:"*sender"`

	// MessagingParticipant
	HostIdentityURN string `json:"hostIdentityUrn"`
	ParticipantType struct {
		Member struct {
			FirstName struct {
				Text string `json:"text"`
			} `json:"firstName"`
			LastName struct {
				Text string `json:"text"`
			} `json:"lastName"`
		} `json:"member"`
	} `json:"participantType"`
}

const (
	typeConversation = "com.linkedin.messenger.Conversation"
	typeMessage      = "com.linkedin.messenger.Message"
	typeParticipant  = "com.linkedin.messenger.MessagingParticipant"
)

// decodeMessengerConversations turns a messengerConversations response into
// lion's Conversation values, resolving participant and message references
// through included[].
//
// Conversations come back in whatever order included[] happens to hold them,
// so they are sorted newest-first here — the order every caller documents and
// the one `message list` promises.
func decodeMessengerConversations(body []byte) ([]Conversation, error) {
	var root struct {
		Included []messengerEntity `json:"included"`
	}
	if err := json.Unmarshal(body, &root); err != nil {
		return nil, fmt.Errorf("decode conversations: %w", err)
	}

	byURN := make(map[string]*messengerEntity, len(root.Included))
	var convs []*messengerEntity
	for i := range root.Included {
		e := &root.Included[i]
		if e.EntityURN != "" {
			byURN[e.EntityURN] = e
		}
		if e.Type == typeConversation {
			convs = append(convs, e)
		}
	}

	out := make([]Conversation, 0, len(convs))
	for _, e := range convs {
		c := Conversation{
			URN:       e.EntityURN,
			ID:        conversationID(e.EntityURN),
			UpdatedAt: e.LastActivityAt,
			// read is a pointer so an absent field is not silently "unread":
			// LinkedIn omits it on some conversations, and defaulting a
			// missing flag to unread would light up `message list --unread`
			// with threads nobody has touched.
			Unread: e.Read != nil && !*e.Read,
		}
		if c.UpdatedAt == 0 {
			c.UpdatedAt = e.CreatedAt
		}
		// Paired name+URN, matching decodeConversations: a participant's URN
		// is always known from the reference, while the display name only
		// resolves if included[] carried that participant. Appending both
		// together keeps a name from ever landing beside the wrong URN.
		for _, ref := range e.Participants {
			p := byURN[ref]
			if p == nil || p.Type != typeParticipant {
				continue
			}
			urn := p.HostIdentityURN
			if urn == "" {
				urn = p.EntityURN
			}
			name := strings.TrimSpace(
				p.ParticipantType.Member.FirstName.Text + " " +
					p.ParticipantType.Member.LastName.Text)
			c.Participants = append(c.Participants, Participant{Name: name, URN: urn})
		}
		// The conversation carries its most recent message inline; later
		// ones need the messengerMessages query.
		for _, ref := range e.Messages.Elements {
			m := byURN[ref]
			if m == nil || m.Type != typeMessage {
				continue
			}
			c.LastMessage = m.Body.Text
			break
		}
		out = append(out, c)
	}

	sort.SliceStable(out, func(i, j int) bool { return out[i].UpdatedAt > out[j].UpdatedAt })
	return out, nil
}

// conversationID extracts the thread segment from a messaging URN.
//
// The new URNs look like
//
//	urn:li:msg_conversation:(urn:li:fsd_profile:<me>,2-<thread>==)
//
// where the useful, stable identifier is the trailing "2-…" segment: the
// leading profile URN is just the viewer's own mailbox and is identical for
// every conversation. Returns "" when the URN does not match, rather than
// guessing — Conversation.ID documents empty as "unknown" and callers that
// need it (event paths, filenames) check.
func conversationID(urn string) string {
	open := strings.Index(urn, "(")
	if open < 0 || !strings.HasSuffix(urn, ")") {
		return ""
	}
	inner := urn[open+1 : len(urn)-1]
	comma := strings.LastIndex(inner, ",")
	if comma < 0 {
		return ""
	}
	return inner[comma+1:]
}

// conversationURN turns whatever `message read` was handed into the full
// conversation URN the messenger query needs.
//
// Callers pass the thread segment (what Conversation.ID carries, and what the
// command has taken since v1.0.0), but the GraphQL variable wants the whole
// urn:li:msg_conversation:(<mailbox>,<thread>) form. A value that is already
// a full URN is passed through, so a URN copied straight out of `message
// list` also works.
func (c *Client) conversationURN(ctx context.Context, conversationID string) (string, error) {
	if strings.HasPrefix(conversationID, "urn:li:msg_conversation:") {
		return conversationID, nil
	}
	me, err := c.Me(ctx)
	if err != nil {
		return "", err
	}
	id := me.URN
	if i := strings.LastIndex(id, ":"); i >= 0 {
		id = id[i+1:]
	}
	return "urn:li:msg_conversation:(urn:li:fsd_profile:" + id + "," + conversationID + ")", nil
}

// decodeMessengerMessages turns a messengerMessages response into lion's
// Message values, resolving each sender through included[].
//
// Ordered oldest-first, which is both how a conversation reads and what the
// retired endpoint produced, so `message read` output does not change shape
// under the migration. max caps from the newest end: asking for 20 messages
// of a long thread means the last 20, not the first.
func decodeMessengerMessages(body []byte, max int) ([]Message, error) {
	var root struct {
		Included []messengerEntity `json:"included"`
	}
	if err := json.Unmarshal(body, &root); err != nil {
		return nil, fmt.Errorf("decode messages: %w", err)
	}
	byURN := make(map[string]*messengerEntity, len(root.Included))
	var msgs []*messengerEntity
	for i := range root.Included {
		e := &root.Included[i]
		if e.EntityURN != "" {
			byURN[e.EntityURN] = e
		}
		if e.Type == typeMessage {
			msgs = append(msgs, e)
		}
	}

	out := make([]Message, 0, len(msgs))
	for _, e := range msgs {
		m := Message{
			URN:    e.EntityURN,
			Text:   e.Body.Text,
			SentAt: e.DeliveredAt,
		}
		// Name and URN are taken from the same resolved participant, so a
		// sender's name can never end up attached to another's identity —
		// the pairing rule Participant's doc comment sets out.
		if p := byURN[e.Sender]; p != nil && p.Type == typeParticipant {
			m.FromURN = p.HostIdentityURN
			if m.FromURN == "" {
				m.FromURN = p.EntityURN
			}
			m.From = strings.TrimSpace(
				p.ParticipantType.Member.FirstName.Text + " " +
					p.ParticipantType.Member.LastName.Text)
		}
		out = append(out, m)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].SentAt < out[j].SentAt })
	if max > 0 && len(out) > max {
		out = out[len(out)-max:]
	}
	return out, nil
}
