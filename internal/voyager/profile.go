package voyager

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

// Me returns the authenticated member's basic identity. It doubles as a cheap
// session-validity check (used by `lion auth status`). It hits the /me
// endpoint, which returns the viewer's mini profile.
func (c *Client) Me(ctx context.Context) (*Profile, error) {
	body, err := c.get(ctx, "/me", nil)
	if err != nil {
		return nil, err
	}
	// /me returns a miniProfile under data; parse leniently.
	var env struct {
		Data struct {
			MiniProfile struct {
				PublicIdentifier string `json:"publicIdentifier"`
				EntityUrn        string `json:"entityUrn"`
				FirstName        string `json:"firstName"`
				LastName         string `json:"lastName"`
				Occupation       string `json:"occupation"`
			} `json:"miniProfile"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, fmt.Errorf("decode /me: %w", err)
	}
	mp := env.Data.MiniProfile
	return &Profile{
		PublicID:  mp.PublicIdentifier,
		URN:       mp.EntityUrn,
		FirstName: mp.FirstName,
		LastName:  mp.LastName,
		Headline:  mp.Occupation,
	}, nil
}

// Profile fetches a full profile by public identifier (the vanity slug in a
// profile URL, e.g. "john-doe-123"). It uses the REST-li profileView endpoint.
func (c *Client) Profile(ctx context.Context, publicID string) (*Profile, error) {
	if publicID == "" {
		return nil, fmt.Errorf("empty profile id")
	}
	path := fmt.Sprintf("/identity/profiles/%s/profileView", url.PathEscape(publicID))
	body, err := c.get(ctx, path, nil)
	if err != nil {
		return nil, err
	}
	return decodeProfileView(body, publicID)
}

// profileViewRaw captures the fields lion surfaces from a profileView payload.
// profileView nests the core profile under data.profile.
type profileViewRaw struct {
	Data struct {
		Profile struct {
			FirstName    string `json:"firstName"`
			LastName     string `json:"lastName"`
			Headline     string `json:"headline"`
			Summary      string `json:"summary"`
			Industry     string `json:"industryName"`
			LocationName string `json:"locationName"`
			MiniProfile  struct {
				PublicIdentifier string `json:"publicIdentifier"`
				EntityUrn        string `json:"entityUrn"`
			} `json:"miniProfile"`
		} `json:"profile"`
	} `json:"data"`
}

func decodeProfileView(body []byte, publicID string) (*Profile, error) {
	var raw profileViewRaw
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("decode profileView: %w", err)
	}
	p := raw.Data.Profile
	pub := p.MiniProfile.PublicIdentifier
	if pub == "" {
		pub = publicID
	}
	return &Profile{
		PublicID:  pub,
		URN:       p.MiniProfile.EntityUrn,
		FirstName: p.FirstName,
		LastName:  p.LastName,
		Headline:  p.Headline,
		Location:  p.LocationName,
		Industry:  p.Industry,
		Summary:   p.Summary,
	}, nil
}

// SearchPeople runs a people search and returns up to max results (0 = server
// default). It uses the blended search REST-li endpoint with a people filter.
func (c *Client) SearchPeople(ctx context.Context, query string, max int) ([]SearchResult, error) {
	if strings.TrimSpace(query) == "" {
		return nil, fmt.Errorf("empty search query")
	}
	q := url.Values{}
	q.Set("keywords", query)
	q.Set("origin", "GLOBAL_SEARCH_HEADER")
	q.Set("q", "all")
	if max > 0 {
		q.Set("count", fmt.Sprintf("%d", max))
	}
	body, err := c.get(ctx, "/search/blended", q)
	if err != nil {
		return nil, err
	}
	return decodePeopleSearch(body, max)
}

// decodePeopleSearch flattens blended search results, keeping only people hits.
//
// NOTE (known limitation, flagged in review): this returns every MiniProfile in
// the normalized `included` cache rather than resolving the ordered result URNs
// under `data`. `included` is a shared entity cache, so unrelated profiles may
// appear and result ordering is not guaranteed. Resolving `data.elements` result
// references first requires a live-captured blended-search payload to model the
// exact shape; do that once a throwaway-account session is available. For now we
// de-duplicate by public id and cap at max.
func decodePeopleSearch(body []byte, max int) ([]SearchResult, error) {
	_, idx, err := parseNormalized(body)
	if err != nil {
		return nil, err
	}
	var out []SearchResult
	seen := make(map[string]bool)
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
		if mp.PublicIdentifier == "" || seen[mp.PublicIdentifier] {
			continue
		}
		seen[mp.PublicIdentifier] = true
		out = append(out, SearchResult{
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
