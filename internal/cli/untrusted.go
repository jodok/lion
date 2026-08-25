package cli

import (
	"github.com/jodok/lion/internal/output"
	"github.com/jodok/lion/internal/voyager"
)

// This file is the single place that decides, per domain type, which fields
// are LinkedIn-controlled free text (i.e. written by other people) and must
// be wrapped when --wrap-untrusted is set. A vertical's render function
// calls the matching wrap* helper below instead of re-deciding, field by
// field, which strings are attacker-controlled — that scattered approach is
// exactly how a defect shipped: Connection.Headline was wrapped but
// Connection.Name was not, in every output mode, letting an attacker plant
// prompt-injection text in a display name instead of a headline. Centralizing
// per type here means a newly added free-text field is an obvious omission
// (it's missing from the one function that lists this type's fields) rather
// than something that can slip through a call site nobody remembered to
// update.
//
// Left untouched deliberately: URNs, public ids, conversation ids, shared
// secrets, counts, timestamps, and enum-ish values (visibility, reaction
// type, invitation state). Those are machine identifiers or lion's own
// structured data, not free text — wrapping them would break scripts that
// pipe them verbatim.

// wrapConnection returns c with its free-text display fields (Name,
// Headline) wrapped.
func wrapConnection(r *output.Renderer, c voyager.Connection) voyager.Connection {
	c.Name = r.Untrusted(c.Name)
	c.Headline = r.Untrusted(c.Headline)
	return c
}

// wrapInvitation returns inv with its free-text display fields (FromName,
// Message) wrapped. InvitationURN, SharedSecret, and FromPublicID are
// identifiers a script needs verbatim, so they're left alone.
func wrapInvitation(r *output.Renderer, inv voyager.Invitation) voyager.Invitation {
	inv.FromName = r.Untrusted(inv.FromName)
	inv.Message = r.Untrusted(inv.Message)
	return inv
}

// wrapConversation returns c with its free-text display fields
// (Participants, LastMessage) wrapped. Each participant name is a display
// name written by someone else, same as Connection.Name, so it's wrapped
// element-by-element rather than as a whole joined string.
func wrapConversation(r *output.Renderer, c voyager.Conversation) voyager.Conversation {
	if len(c.Participants) > 0 {
		wrapped := make([]string, len(c.Participants))
		for i, p := range c.Participants {
			wrapped[i] = r.Untrusted(p)
		}
		c.Participants = wrapped
	}
	c.LastMessage = r.Untrusted(c.LastMessage)
	return c
}

// wrapMessage returns m with its free-text display fields (From, Text)
// wrapped.
func wrapMessage(r *output.Renderer, m voyager.Message) voyager.Message {
	m.From = r.Untrusted(m.From)
	m.Text = r.Untrusted(m.Text)
	return m
}

// wrapFeedItem returns it with its free-text display fields (AuthorName,
// Text) wrapped.
func wrapFeedItem(r *output.Renderer, it voyager.FeedItem) voyager.FeedItem {
	it.AuthorName = r.Untrusted(it.AuthorName)
	it.Text = r.Untrusted(it.Text)
	return it
}

// wrapSearchResult returns sr with its free-text display fields (Name,
// Headline, Location) wrapped.
func wrapSearchResult(r *output.Renderer, sr voyager.SearchResult) voyager.SearchResult {
	sr.Name = r.Untrusted(sr.Name)
	sr.Headline = r.Untrusted(sr.Headline)
	sr.Location = r.Untrusted(sr.Location)
	return sr
}
