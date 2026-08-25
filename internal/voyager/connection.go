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

// connectionsEnvelope captures the ordered element references from a
// connections list response. Each element carries a "*miniProfile" URN
// reference — Voyager's convention for a not-yet-resolved reference field
// (the same convention /me uses for its own "*miniProfile", see
// DESIGN.md §3.2) — rather than an inline object; the actual MiniProfile
// lives in included[] and is resolved via the entity index.
type connectionsEnvelope struct {
	Data struct {
		Elements []struct {
			MiniProfileRef string `json:"*miniProfile"`
		} `json:"elements"`
	} `json:"data"`
}

// decodeConnections resolves the connections list's ordered `data.elements`
// references against the `included[]` entity index, returning connections in
// the server's own order (most-recently-added first per the sortType this
// package requests) and capped at max.
//
// Earlier versions of this decoder scanned every MiniProfile in `included[]`
// instead of following `data.elements`, which could surface unrelated cached
// profiles (anything else the response happened to embed) and lost the
// server's ordering, making `max` cap an arbitrary subset. Following the
// ordered references fixes both.
func decodeConnections(body []byte, max int) ([]Connection, error) {
	_, idx, err := parseNormalized(body)
	if err != nil {
		return nil, err
	}
	var env connectionsEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, fmt.Errorf("decode connections: %w", err)
	}
	var out []Connection
	seen := make(map[string]bool)
	for _, el := range env.Data.Elements {
		raw, ok := idx.get(el.MiniProfileRef)
		if !ok {
			continue
		}
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
		if mp.PublicIdentifier == "" || seen[mp.PublicIdentifier] {
			continue
		}
		seen[mp.PublicIdentifier] = true
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
// Verified against the live API (2026-07-06): /relationships/invitationViews
// returns HTTP 400 (see DESIGN.md §3.2) — the REST-li surface is gone and
// connection requests now live behind a GraphQL surface whose queryId has
// not been captured from a live browser session. Rather than send a request
// that is known to always fail, this returns a clear, typed error so
// callers fail honestly instead of surfacing a generic 400 API error.
// decodeInvitations is kept (and still exercised directly by tests) so the
// ordered-decode fix is ready to wire up once that GraphQL surface is
// modeled.
func (c *Client) Invitations(_ context.Context, _ bool) ([]Invitation, error) {
	return nil, fmt.Errorf("connection requests/accept require LinkedIn's GraphQL invitations surface, not yet supported in this build: %w", ErrNotFound)
}

// invitationsEnvelope mirrors connectionsEnvelope's pattern: ordered
// "*invitationView" reference elements resolved against included[].
type invitationsEnvelope struct {
	Data struct {
		Elements []struct {
			InvitationViewRef string `json:"*invitationView"`
		} `json:"elements"`
	} `json:"data"`
}

// decodeInvitations resolves the invitationViews response's ordered
// `data.elements` references against the `included[]` entity index. Each
// resolved invitation view entity carries the invitation URN, shared
// secret, and a reference to the sending member's MiniProfile.
//
// Earlier versions of this decoder scanned every InvitationView in
// `included[]` instead of following `data.elements`. Because `connection
// accept --all` acts on this list, surfacing an unrelated cached invitation
// could accept something outside the pending set; following the ordered
// references fixes that. Invitations with an empty shared secret are still
// dropped so a downstream accept can never send an incomplete mutation.
func decodeInvitations(body []byte, incoming bool) ([]Invitation, error) {
	_, idx, err := parseNormalized(body)
	if err != nil {
		return nil, err
	}
	var env invitationsEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, fmt.Errorf("decode invitations: %w", err)
	}
	var out []Invitation
	seen := make(map[string]bool)
	for _, el := range env.Data.Elements {
		raw, ok := idx.get(el.InvitationViewRef)
		if !ok {
			continue
		}
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
		if iv.Invitation.EntityUrn == "" || iv.Invitation.SharedSecret == "" || seen[iv.Invitation.EntityUrn] {
			continue
		}
		seen[iv.Invitation.EntityUrn] = true
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
