package voyager

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
	return p.FirstName + " " + p.LastName
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
	SharedSecret  string `json:"shared_secret"`
	FromName      string `json:"from_name"`
	FromPublicID  string `json:"from_public_id"`
	Message       string `json:"message,omitempty"`
	Incoming      bool   `json:"incoming"`
}

// Conversation is a messaging thread.
type Conversation struct {
	URN          string   `json:"urn"`
	Participants []string `json:"participants"`
	LastMessage  string   `json:"last_message"`
	UpdatedAt    int64    `json:"updated_at"` // epoch millis
	Unread       bool     `json:"unread"`
}

// Message is a single message within a conversation.
type Message struct {
	URN    string `json:"urn"`
	From   string `json:"from"`
	Text   string `json:"text"`
	SentAt int64  `json:"sent_at"` // epoch millis
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
