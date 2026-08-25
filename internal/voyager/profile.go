package voyager

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

// miniProfile is the MiniProfile projection LinkedIn returns for a member. It
// appears in `included` across many endpoints (/me, search, invitations).
type miniProfile struct {
	PublicIdentifier string `json:"publicIdentifier"`
	EntityUrn        string `json:"entityUrn"`
	FirstName        string `json:"firstName"`
	LastName         string `json:"lastName"`
	Occupation       string `json:"occupation"`
}

// Me returns the authenticated member's basic identity. It doubles as a cheap
// session-validity check (used by `lion auth status`).
//
// Verified against the live API (2026-07-06): /me returns normalized JSON where
// data holds a "*miniProfile" URN *reference* and the actual MiniProfile object
// lives in `included` — so we resolve the reference through the entity index.
func (c *Client) Me(ctx context.Context) (*Profile, error) {
	body, err := c.get(ctx, "/me", nil)
	if err != nil {
		return nil, err
	}
	var env struct {
		Data struct {
			MiniProfileURN string `json:"*miniProfile"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, fmt.Errorf("decode /me: %w", err)
	}
	_, idx, err := parseNormalized(body)
	if err != nil {
		return nil, err
	}
	raw, ok := idx.get(env.Data.MiniProfileURN)
	if !ok {
		return nil, fmt.Errorf("/me: miniProfile %q not found in response", env.Data.MiniProfileURN)
	}
	var mp miniProfile
	if err := decodeInto(raw, &mp); err != nil {
		return nil, fmt.Errorf("decode miniProfile: %w", err)
	}
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
	// Verified 2026-07-06: the REST /identity/profiles/{id}/profileView endpoint
	// is gone (HTTP 410). The modern profile is decomposed into many card-scoped
	// GraphQL queries (voyagerIdentityDashProfiles + card queries) keyed by the
	// member id, not the public slug. Modeling the full card set is follow-up
	// work; until then, surface a clear error instead of hitting the dead path.
	// `profile view me` works via Me(); `profile search` returns member basics.
	return nil, fmt.Errorf("profile-by-id is not yet supported on LinkedIn's new GraphQL profile API (the legacy endpoint returns HTTP 410); use `lion profile search` or `lion profile view me`: %w", ErrNotFound)
}

// SearchPeople runs a people search and returns up to max results (0 = default).
//
// Verified against the live API (2026-07-06): the legacy REST /search/blended is
// gone (404). People search is now GraphQL (voyagerSearchDashClusters), and
// results arrive as EntityResultViewModel entities in `included`.
func (c *Client) SearchPeople(ctx context.Context, query string, max int) ([]SearchResult, error) {
	if strings.TrimSpace(query) == "" {
		return nil, fmt.Errorf("empty search query")
	}
	count := max
	if count <= 0 {
		count = 10
	}
	// Rest.li variables string; only the keywords value is percent-encoded.
	vars := fmt.Sprintf(
		"(start:0,count:%d,origin:GLOBAL_SEARCH_HEADER,query:(keywords:%s,flagshipSearchIntent:SEARCH_SRP,queryParameters:List((key:resultType,value:List(PEOPLE)))))",
		count, rlEncode(query),
	)
	body, err := c.getGraphQL(ctx, queryIDSearchClusters, vars)
	if err != nil {
		return nil, err
	}
	return decodePeopleSearch(body, max)
}

// decodePeopleSearch extracts people hits from a search-clusters GraphQL
// response. Each hit is an EntityResultViewModel whose title/primarySubtitle/
// secondarySubtitle carry the name, headline and location, and whose
// navigationUrl embeds the public identifier. Only profile results are kept.
func decodePeopleSearch(body []byte, max int) ([]SearchResult, error) {
	_, idx, err := parseNormalized(body)
	if err != nil {
		return nil, err
	}
	var out []SearchResult
	seen := make(map[string]bool)
	for _, raw := range idx.ofType("com.linkedin.voyager.dash.search.EntityResultViewModel") {
		var e struct {
			EntityUrn     string    `json:"entityUrn"`
			TrackingUrn   string    `json:"trackingUrn"`
			NavigationURL string    `json:"navigationUrl"`
			Title         textField `json:"title"`
			PrimarySub    textField `json:"primarySubtitle"`
			SecondarySub  textField `json:"secondarySubtitle"`
		}
		if err := decodeInto(raw, &e); err != nil {
			continue
		}
		// Only member profiles (skip companies, schools, upsell cards, etc.).
		pub := publicIDFromNavURL(e.NavigationURL)
		if pub == "" || seen[pub] {
			continue
		}
		seen[pub] = true
		out = append(out, SearchResult{
			PublicID: pub,
			URN:      profileURNFromNavURL(e.NavigationURL),
			Name:     strings.TrimSpace(e.Title.Text),
			Headline: strings.TrimSpace(e.PrimarySub.Text),
			Location: strings.TrimSpace(e.SecondarySub.Text),
		})
		if max > 0 && len(out) >= max {
			break
		}
	}
	return out, nil
}

// textField matches LinkedIn's { "text": "...", ... } attributed-string shape.
type textField struct {
	Text string `json:"text"`
}

// publicIDFromNavURL pulls the vanity slug out of a profile navigation URL like
// "https://www.linkedin.com/in/jane-doe?miniProfileUrn=...". Returns "" for
// non-profile URLs (company/school results).
func publicIDFromNavURL(nav string) string {
	i := strings.Index(nav, "/in/")
	if i < 0 {
		return ""
	}
	slug := nav[i+len("/in/"):]
	if j := strings.IndexAny(slug, "?/#"); j >= 0 {
		slug = slug[:j]
	}
	return slug
}

// profileURNFromNavURL extracts the miniProfile URN a search result carries in
// its navigationUrl query string (miniProfileUrn=urn%3Ali%3Afs_miniProfile%3A…).
//
// This, not trackingUrn, is the identifier the rest of lion needs. trackingUrn
// is a urn:li:member:<numeric> analytics handle, while messaging addresses
// people by urn:li:fs_miniProfile:<opaque>. Exporting the tracking one made the
// obvious search-then-message workflow hand SendMessageToProfile a recipient it
// cannot address.
//
// Returns "" when the parameter is missing rather than synthesizing a URN from
// another field: an empty URN fails loudly at the next step, whereas a guessed
// one would be sent to LinkedIn as though it were real.
func profileURNFromNavURL(nav string) string {
	u, err := url.Parse(nav)
	if err != nil {
		return ""
	}
	urn := u.Query().Get("miniProfileUrn")
	if !strings.HasPrefix(urn, "urn:li:") {
		return ""
	}
	return urn
}
