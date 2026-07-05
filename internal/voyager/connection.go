package voyager

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/jodok/lion/internal/ratelimit"
)

// Connections returns up to max first-degree connections, most recently added
// first. It uses the REST-li relationships endpoint.
//
// NOTE: the exact path/query for this endpoint has drifted across LinkedIn
// web app releases in the past (it has lived under both
// /relationships/connections and /relationships/dash/connections). This is
// the best-known current shape; verify against a live session before relying
// on it and adjust the path/query here if LinkedIn has moved it again.
func (c *Client) Connections(ctx context.Context, max int) ([]Connection, error) {
	count := max
	if count <= 0 {
		count = 40
	}
	q := url.Values{}
	q.Set("start", "0")
	q.Set("count", fmt.Sprintf("%d", count))
	q.Set("sortType", "RECENTLY_ADDED")
	body, err := c.get(ctx, "/relationships/connections", q)
	if err != nil {
		return nil, err
	}
	return decodeConnections(body, max)
}

// decodeConnections flattens the normalized connections payload, keeping the
// MiniProfile entities that represent each connection.
func decodeConnections(body []byte, max int) ([]Connection, error) {
	_, idx, err := parseNormalized(body)
	if err != nil {
		return nil, err
	}
	var out []Connection
	for _, raw := range idx.ofType("com.linkedin.voyager.identity.shared.MiniProfile") {
		var mp struct {
			PublicIdentifier string `json:"publicIdentifier"`
			EntityUrn        string `json:"entityUrn"`
			FirstName        string `json:"firstName"`
			LastName         string `json:"lastName"`
			Occupation       string `json:"occupation"`
		}
		if err := decodeInto(raw, &mp); err != nil {
			continue
		}
		if mp.PublicIdentifier == "" {
			continue
		}
		out = append(out, Connection{
			PublicID: mp.PublicIdentifier,
			URN:      mp.EntityUrn,
			Name:     strings.TrimSpace(mp.FirstName + " " + mp.LastName),
			Headline: mp.Occupation,
		})
		if max > 0 && len(out) >= max {
			break
		}
	}
	return out, nil
}

// Invitations returns pending connection invitations. incoming selects
// received invites (invitations sent to the authenticated member); outgoing
// invites are not yet supported by this endpoint shape.
//
// NOTE: /relationships/invitationViews is the legacy REST-li surface for
// invitations; LinkedIn has been migrating pieces of this to GraphQL
// (voyagerRelationshipsDashInvitations). Verify against a live session and
// adjust if the response shape or path has moved.
func (c *Client) Invitations(ctx context.Context, incoming bool) ([]Invitation, error) {
	q := url.Values{}
	if incoming {
		q.Set("q", "invitationType")
		q.Set("invitationType", "CONNECTION")
	}
	body, err := c.get(ctx, "/relationships/invitationViews", q)
	if err != nil {
		return nil, err
	}
	return decodeInvitations(body, incoming)
}

// decodeInvitations flattens the normalized invitationViews payload. Each
// invitation view entity carries the invitation URN, shared secret, and a
// reference to the sending member's MiniProfile.
func decodeInvitations(body []byte, incoming bool) ([]Invitation, error) {
	_, idx, err := parseNormalized(body)
	if err != nil {
		return nil, err
	}
	var out []Invitation
	for _, raw := range idx.ofType("com.linkedin.voyager.relationships.invitation.InvitationView") {
		var iv struct {
			Invitation struct {
				EntityUrn    string `json:"entityUrn"`
				SharedSecret string `json:"sharedSecret"`
				Message      string `json:"message"`
			} `json:"invitation"`
			FromMember string `json:"fromMember"`
		}
		if err := decodeInto(raw, &iv); err != nil {
			continue
		}
		inv := Invitation{
			InvitationURN: iv.Invitation.EntityUrn,
			SharedSecret:  iv.Invitation.SharedSecret,
			Message:       iv.Invitation.Message,
			Incoming:      incoming,
		}
		if raw, ok := idx.get(iv.FromMember); ok {
			var mp struct {
				PublicIdentifier string `json:"publicIdentifier"`
				FirstName        string `json:"firstName"`
				LastName         string `json:"lastName"`
			}
			if err := decodeInto(raw, &mp); err == nil {
				inv.FromName = strings.TrimSpace(mp.FirstName + " " + mp.LastName)
				inv.FromPublicID = mp.PublicIdentifier
			}
		}
		out = append(out, inv)
	}
	return out, nil
}

// Invite sends a connection invitation to profileID (public identifier),
// optionally including a note. Under --dry-run, client.post returns (nil,
// nil) without sending and Invite treats that as success.
//
// NOTE: /growth/normInvitations is the best-known current endpoint for
// sending invites from the web app; LinkedIn has changed this path before
// (it previously lived under /relationships/invitations). Verify against a
// live session before relying on it.
func (c *Client) Invite(ctx context.Context, profileID, note string) error {
	if profileID == "" {
		return fmt.Errorf("empty profile id")
	}
	payload := struct {
		Invitee struct {
			InviteeProfile struct {
				ProfileID string `json:"profileId"`
			} `json:"com.linkedin.voyager.growth.invitation.InviteeProfile"`
		} `json:"invitee"`
		TrackingID string `json:"trackingId"`
		Message    string `json:"message,omitempty"`
	}{}
	payload.Invitee.InviteeProfile.ProfileID = profileID
	payload.TrackingID = randomTrackingID()
	payload.Message = note

	buf, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode invite payload: %w", err)
	}
	_, err = c.post(ctx, "/growth/normInvitations", strings.NewReader(string(buf)), ratelimit.Invite)
	return err
}

// AcceptInvitation accepts a pending invitation identified by its URN
// (invitationID) and shared secret, both obtained from Invitations.
//
// NOTE: best-known path is /relationships/invitations/{id}?action=accept;
// verify against a live session — LinkedIn sometimes expects the numeric
// invitation id rather than the full URN in the path segment.
func (c *Client) AcceptInvitation(ctx context.Context, invitationURN, sharedSecret string) error {
	if invitationURN == "" {
		return fmt.Errorf("empty invitation urn")
	}
	payload := struct {
		InvitationID           string `json:"invitationId"`
		InvitationSharedSecret string `json:"invitationSharedSecret"`
		IsGenericInvitation    bool   `json:"isGenericInvitation"`
	}{
		InvitationID:           invitationURN,
		InvitationSharedSecret: sharedSecret,
		IsGenericInvitation:    false,
	}
	buf, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode accept payload: %w", err)
	}
	path := fmt.Sprintf("/relationships/invitations/%s?action=accept", url.PathEscape(invitationURN))
	_, err = c.post(ctx, path, strings.NewReader(string(buf)), ratelimit.Write)
	return err
}

// RemoveConnection disconnects from an existing first-degree connection.
//
// NOTE: best-known path is /identity/profiles/{id}/profileActions?action=disconnect;
// verify against a live session before relying on it.
func (c *Client) RemoveConnection(ctx context.Context, profileID string) error {
	if profileID == "" {
		return fmt.Errorf("empty profile id")
	}
	path := fmt.Sprintf("/identity/profiles/%s/profileActions?action=disconnect", url.PathEscape(profileID))
	_, err := c.post(ctx, path, strings.NewReader("{}"), ratelimit.Write)
	return err
}

// randomTrackingID generates a short random tracking id for invite requests,
// mimicking the opaque client-generated ids the web app sends.
func randomTrackingID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "lion-invite"
	}
	return fmt.Sprintf("%x", b)
}
