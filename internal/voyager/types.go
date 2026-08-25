package voyager

import "strings"

// Domain types returned by the client. These are deliberately flat and
// presentation-agnostic; the CLI layer decides how to render them.

// Profile is a LinkedIn member profile.
type Profile struct {
	PublicID    string `json:"public_id"`
	URN         string `json:"urn"`
	FirstName   string `json:"first_name"`
	LastName    string `json:"last_name"`
	Headline    string `json:"headline"`
	Location    string `json:"location"`
	Industry    string `json:"industry"`
	Summary     string `json:"summary,omitempty"`
	Connections int    `json:"connections,omitempty"`
}

// Name returns the member's display name.
func (p Profile) Name() string {
	if p.FirstName == "" && p.LastName == "" {
		return p.PublicID
	}
	return strings.TrimSpace(p.FirstName + " " + p.LastName)
}

// SearchResult is a lightweight person hit from people search.
type SearchResult struct {
	PublicID string `json:"public_id"`
	URN      string `json:"urn"`
	Name     string `json:"name"`
	Headline string `json:"headline"`
	Location string `json:"location"`
}

// Connection is a first-degree connection.
type Connection struct {
	PublicID    string `json:"public_id"`
	URN         string `json:"urn"`
	Name        string `json:"name"`
	Headline    string `json:"headline"`
	ConnectedAt int64  `json:"connected_at,omitempty"` // epoch millis
}

// Invitation is an incoming or outgoing connection request.
type Invitation struct {
	InvitationURN string `json:"invitation_urn"`
	// SharedSecret is only needed internally to call AcceptInvitation; it
	// must never leak into `--json` output, so it is excluded from
	// marshaling rather than merely renamed.
	SharedSecret string `json:"-"`
	FromName     string `json:"from_name"`
	FromPublicID string `json:"from_public_id"`
	Message      string `json:"message,omitempty"`
	Incoming     bool   `json:"incoming"`
}

// Participant is one participant of a Conversation, carrying a display name
// paired with the MiniProfile URN it came from. This exists as a single
// struct rather than two parallel slices (a prior shape this replaced)
// because a participant's URN is always known — it's read directly off the
// participant reference — while the display name only resolves when
// included[] happens to carry that participant's MiniProfile (see
// decodeConversations). Pairing them means an unresolved name is simply an
// empty Name on THAT participant's own entry, not a missing slice element
// that silently shifts every later index — which is exactly how a name
// could end up attached to the wrong participant's URN under the old shape.
type Participant struct {
	Name string `json:"name"`
	URN  string `json:"urn"`
}

// Conversation is a messaging thread.
type Conversation struct {
	URN string `json:"urn"`
	// ID is the raw id segment parsed out of URN (a
	// urn:li:fs_conversation:<id> URN) — the identifier the events endpoint
	// path and a filesystem-safe filename both need. Empty when URN doesn't
	// match the expected prefix, rather than guessing.
	ID string `json:"id,omitempty"`
	// Participants pairs each participant's display name with their
	// MiniProfile URN — see the Participant doc comment.
	Participants []Participant `json:"participants"`
	LastMessage  string        `json:"last_message"`
	UpdatedAt    int64         `json:"updated_at"` // epoch millis
	Unread       bool          `json:"unread"`
}

// Message is a single message within a conversation.
type Message struct {
	URN  string `json:"urn"`
	From string `json:"from"`
	// FromURN is the sender's MiniProfile URN alongside the display name in
	// From — a display name alone isn't a stable identity.
	FromURN string `json:"from_urn,omitempty"`
	Text    string `json:"text"`
	SentAt  int64  `json:"sent_at"` // epoch millis
}

// FeedItem is a single post in the feed.
type FeedItem struct {
	URN        string `json:"urn"`
	AuthorName string `json:"author_name"`
	Text       string `json:"text"`
	Likes      int    `json:"likes"`
	Comments   int    `json:"comments"`
	PostedAt   int64  `json:"posted_at"` // epoch millis
}
