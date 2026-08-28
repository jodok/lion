package store

// Participant is one member of a conversation, stored as JSON in
// conversations.participants. A display name alone isn't a stable identity
// (see voyager.Conversation.ParticipantURNs), so both travel together.
type Participant struct {
	Name string `json:"name"`
	URN  string `json:"urn"`
}

// Conversation is a messaging thread as recorded in the store. It is a
// distinct type from voyager.Conversation: the store tracks sync-progress
// fields (NewestSynced, OldestSynced, BackfillDone, ...) the API type has no
// concept of, and internal/store must not import internal/voyager — the
// dependency runs the other way (internal/cli translates between the two).
type Conversation struct {
	ID           string
	URN          string
	Participants []Participant
	UpdatedAt    int64 // epoch ms, LinkedIn's own "last activity" timestamp
	Unread       bool

	// NewestSynced/OldestSynced bound the contiguous range of messages this
	// store actually holds for the conversation, in epoch ms. Both are nil
	// until at least one message page has been recorded. Catch-up sync walks
	// back from "now" until it hits a stored URN (using NewestSynced as a
	// hint of where "known" begins); --backfill walks back from
	// OldestSynced until a page comes back empty.
	NewestSynced *int64
	OldestSynced *int64
	// MessagesSyncToken is where this conversation's message stream was
	// left off, so a later run can ask only for what changed. Empty means
	// no resume point: the next drain starts from a full snapshot.
	MessagesSyncToken string
	// BackfillDone is true once paging backwards reached the start of the
	// conversation (an empty page), so a later plain sync knows there is
	// nothing older left to fetch even without --backfill.
	BackfillDone bool

	FirstSeenAt  int64 // epoch ms; when this store first learned of the conversation
	LastSyncedAt int64 // epoch ms; last time any page of this conversation was recorded
}

// Message is a single message as recorded in the store.
type Message struct {
	URN            string
	ConversationID string
	SenderName     string
	SenderURN      string
	SentAt         int64 // epoch ms
	Body           string
}
