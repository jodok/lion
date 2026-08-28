package voyager

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
)

// The messaging GraphQL surface is a sync-token protocol, not a paginated
// one. Its collection metadata carries no paging block and no cursor — only
// a newSyncToken, plus deletedUrns and shouldClearCache on the message
// stream (type com.linkedin.messenger.SyncMetadata). Probing confirmed the
// rest: count, lastUpdatedBefore, and nextCursor are all accepted and all
// ignored on the conversations query.
//
// That is a different shape from the timestamp cursor lion's sync was built
// around, and it is why the naive migration would have quietly cost `lion
// sync` its reach. The protocol is:
//
//	no token   -> a full snapshot, shouldClearCache=true, and a token
//	with token -> whatever changed since it, and a fresh token
//
// Draining it means calling with the newest token until a response brings
// nothing new. That matters beyond tidiness: it is correct whether or not
// the server chunks large mailboxes, which is the one property that could
// not be verified directly — the only account available to test against
// holds two conversations, far too few to make LinkedIn chunk anything. A
// loop that stops when a response adds nothing needs no answer to that
// question, where a hardcoded "it all arrives at once" would silently
// truncate the first person with a big mailbox.
//
// Nothing here persists a token yet, so each run takes a fresh snapshot.
// True incremental sync — storing the token and asking only for deltas — is
// the follow-up this makes possible; sync's own "stop at a message already
// stored" check is what keeps the current cost down.

// syncDrainLimit bounds how many requests one drain may make. A server that
// keeps handing back fresh tokens and fresh elements would otherwise loop
// forever; this turns that into a bounded, reportable truncation. Sized well
// above any plausible mailbox at the page sizes LinkedIn returns.
const syncDrainLimit = 50

// ConversationSync is one response from the conversation sync stream.
type ConversationSync struct {
	Conversations []Conversation
	// ClearCache is LinkedIn's shouldClearCache: this response is a full
	// snapshot and anything previously held for this mailbox is stale.
	ClearCache bool
	// DeletedURNs are conversations removed since the token was issued.
	DeletedURNs []string
	// NextToken is the token to pass to the following call. Empty means the
	// server offered none, which ends a drain.
	NextToken string
}

// MessageSync is one response from a conversation's message sync stream.
type MessageSync struct {
	Messages    []Message
	ClearCache  bool
	DeletedURNs []string
	NextToken   string
}

// syncMetadata is the SyncMetadata block both streams carry.
type syncMetadata struct {
	NewSyncToken     string   `json:"newSyncToken"`
	DeletedURNs      []string `json:"deletedUrns"`
	ShouldClearCache bool     `json:"shouldClearCache"`
}

// decodeSyncMetadata pulls the metadata out of whichever collection wrapper
// the response used. The wrapper key is query-specific and LinkedIn's to
// rename, so this looks for the member that actually carries metadata rather
// than hardcoding messengerConversationsBySyncToken.
func decodeSyncMetadata(body []byte) syncMetadata {
	var root struct {
		Data struct {
			Data map[string]json.RawMessage `json:"data"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &root); err != nil {
		return syncMetadata{}
	}
	for _, raw := range root.Data.Data {
		var probe struct {
			Metadata *syncMetadata `json:"metadata"`
		}
		if err := json.Unmarshal(raw, &probe); err != nil {
			continue // not an object (e.g. the $type string)
		}
		if probe.Metadata != nil && probe.Metadata.NewSyncToken != "" {
			return *probe.Metadata
		}
	}
	return syncMetadata{}
}

// ConversationsSync fetches one response from the mailbox sync stream. An
// empty token asks for a full snapshot.
func (c *Client) ConversationsSync(ctx context.Context, token string) (*ConversationSync, error) {
	mailbox, err := c.mailbox(ctx)
	if err != nil {
		return nil, err
	}
	vars := "(mailboxUrn:" + mailbox
	if token != "" {
		vars += ",syncToken:" + url.QueryEscape(token)
	}
	vars += ")"

	body, err := c.getMessagingGraphQL(ctx, queryIDMessengerConversations, vars)
	if err != nil {
		return nil, err
	}
	convs, err := decodeMessengerConversations(body)
	if err != nil {
		return nil, err
	}
	md := decodeSyncMetadata(body)
	return &ConversationSync{
		Conversations: convs,
		ClearCache:    md.ShouldClearCache,
		DeletedURNs:   md.DeletedURNs,
		NextToken:     md.NewSyncToken,
	}, nil
}

// MessagesSync fetches one response from a conversation's message sync
// stream. conversationID accepts a thread segment or a full conversation URN,
// like Messages does.
func (c *Client) MessagesSync(ctx context.Context, conversationID, token string) (*MessageSync, error) {
	if conversationID == "" {
		return nil, fmt.Errorf("empty conversation id")
	}
	urn, err := c.conversationURN(ctx, conversationID)
	if err != nil {
		return nil, err
	}
	vars := "(conversationUrn:" + url.QueryEscape(urn)
	if token != "" {
		vars += ",syncToken:" + url.QueryEscape(token)
	}
	vars += ")"

	body, err := c.getMessagingGraphQL(ctx, queryIDMessengerMessages, vars)
	if err != nil {
		return nil, err
	}
	// max=0: a drain caps its own total, and capping each response would
	// discard messages the next token has already moved past.
	msgs, err := decodeMessengerMessages(body, 0)
	if err != nil {
		return nil, err
	}
	md := decodeSyncMetadata(body)
	return &MessageSync{
		Messages:    msgs,
		ClearCache:  md.ShouldClearCache,
		DeletedURNs: md.DeletedURNs,
		NextToken:   md.NewSyncToken,
	}, nil
}

// AllConversations drains the mailbox sync stream and returns every
// conversation it yielded, newest first.
//
// max caps the result (0 = no cap). complete reports whether the drain
// genuinely ran out: false means the cap or the request limit stopped it
// early and older conversations may exist that this pass never saw — the
// caller must not record such a pass as a full sync.
func (c *Client) AllConversations(ctx context.Context, max int) (convs []Conversation, complete bool, err error) {
	seen := map[string]bool{}
	var token string
	for i := 0; i < syncDrainLimit; i++ {
		page, err := c.ConversationsSync(ctx, token)
		if err != nil {
			return convs, false, err
		}
		added := 0
		for _, cv := range page.Conversations {
			key := cv.URN
			if key == "" {
				key = cv.ID
			}
			if key == "" || seen[key] {
				continue
			}
			seen[key] = true
			convs = append(convs, cv)
			added++
			if max > 0 && len(convs) >= max {
				return convs, false, nil
			}
		}
		// A response that adds nothing is the end of the stream. Checked
		// before the token, because a server that keeps issuing fresh tokens
		// for an unchanged mailbox — which this one does — would otherwise
		// never terminate.
		if added == 0 {
			return convs, true, nil
		}
		if page.NextToken == "" || page.NextToken == token {
			return convs, true, nil
		}
		token = page.NextToken
	}
	return convs, false, nil
}

// AllMessages drains one conversation's message sync stream, oldest first.
//
// complete carries the same meaning as AllConversations': false means the cap
// or the request limit stopped the walk, not that the conversation ended.
func (c *Client) AllMessages(ctx context.Context, conversationID string, max int) (msgs []Message, complete bool, err error) {
	seen := map[string]bool{}
	var token string
	for i := 0; i < syncDrainLimit; i++ {
		page, err := c.MessagesSync(ctx, conversationID, token)
		if err != nil {
			return msgs, false, err
		}
		added := 0
		for _, m := range page.Messages {
			if m.URN == "" || seen[m.URN] {
				continue
			}
			seen[m.URN] = true
			msgs = append(msgs, m)
			added++
		}
		if added == 0 || page.NextToken == "" || page.NextToken == token {
			sortMessagesOldestFirst(msgs)
			return capNewest(msgs, max), true, nil
		}
		token = page.NextToken
	}
	sortMessagesOldestFirst(msgs)
	return capNewest(msgs, max), false, nil
}
