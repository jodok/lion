package voyager

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"

	"github.com/jodok/lion/internal/ratelimit"
)

// Voyager endpoints for feed/social actions drift more than most (LinkedIn
// reshuffles the feed and social-actions surface often), so every path below
// is best-known-good rather than guaranteed. They need live verification
// against a real session before this vertical is trusted in production; the
// paths and request bodies are isolated in small helpers so they're easy to
// swap without touching the decode logic.

// Feed returns up to max items from the chronological feed (0 = server
// default). It hits the updatesV2 REST-li endpoint with the chronFeed query.
func (c *Client) Feed(ctx context.Context, max int) ([]FeedItem, error) {
	q := url.Values{}
	q.Set("q", "chronFeed")
	if max > 0 {
		q.Set("count", strconv.Itoa(max))
	}
	// NEEDS VERIFICATION: /feed/updatesV2?q=chronFeed is the last-known path
	// for the chronological home feed; LinkedIn has migrated feed reads to
	// GraphQL before and may again.
	body, err := c.get(ctx, "/feed/updatesV2", q)
	if err != nil {
		return nil, err
	}
	return decodeFeed(body, max)
}

// feedUpdateRaw captures the fields lion surfaces from one feed update. The
// update itself is an UpdateV2 entity living in included[] (rather than a
// top-level "elements" array), so it is walked the same way profile/search
// decoders walk included[].
type feedUpdateRaw struct {
	EntityUrn string `json:"entityUrn"`
	Actor     struct {
		Name string `json:"name"`
	} `json:"actor"`
	Commentary struct {
		Text struct {
			Text string `json:"text"`
		} `json:"text"`
	} `json:"commentary"`
	SocialDetail struct {
		TotalSocialActivityCounts struct {
			NumLikes    int `json:"numLikes"`
			NumComments int `json:"numComments"`
		} `json:"totalSocialActivityCounts"`
	} `json:"socialDetail"`
	CreatedAt int64 `json:"createdAt"`
}

// feedEnvelope captures the ordered element references from a feed
// response. Each element carries a "*feedUpdate" URN reference — Voyager's
// convention for a not-yet-resolved reference field, matching the pattern
// connectionsEnvelope and /me use for "*miniProfile" — rather than an
// inline update; the actual UpdateV2 lives in included[] and is resolved
// via the entity index.
type feedEnvelope struct {
	Data struct {
		Elements []struct {
			UpdateRef string `json:"*feedUpdate"`
		} `json:"elements"`
	} `json:"data"`
}

// decodeFeed resolves the feed's ordered `data.elements` references against
// the `included[]` entity index, returning items in the server's own
// (reverse-chronological) order and capped at max.
//
// Earlier versions of this decoder returned every UpdateV2 found in
// `included[]` instead of following `data.elements`. `included` can hold
// extra entities for rendering, so that scanned in unrelated updates and
// lost the server's ordering; following the ordered references fixes both.
func decodeFeed(body []byte, max int) ([]FeedItem, error) {
	_, idx, err := parseNormalized(body)
	if err != nil {
		return nil, err
	}
	var env feedEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, fmt.Errorf("decode feed: %w", err)
	}
	var out []FeedItem
	seen := make(map[string]bool)
	for _, el := range env.Data.Elements {
		raw, ok := idx.get(el.UpdateRef)
		if !ok {
			continue
		}
		var u feedUpdateRaw
		if err := decodeInto(raw, &u); err != nil {
			continue
		}
		if u.EntityUrn == "" || seen[u.EntityUrn] {
			continue
		}
		seen[u.EntityUrn] = true
		out = append(out, FeedItem{
			URN:        u.EntityUrn,
			AuthorName: u.Actor.Name,
			Text:       u.Commentary.Text.Text,
			Likes:      u.SocialDetail.TotalSocialActivityCounts.NumLikes,
			Comments:   u.SocialDetail.TotalSocialActivityCounts.NumComments,
			PostedAt:   u.CreatedAt,
		})
		if max > 0 && len(out) >= max {
			break
		}
	}
	return out, nil
}

// CreatePost publishes a text share to the member's feed. visibility is
// "connections" (connections-only) or "public". Respects dry-run: under
// --dry-run the client's post() returns (nil, nil) without sending.
func (c *Client) CreatePost(ctx context.Context, text, visibility string) error {
	if text == "" {
		return fmt.Errorf("empty post text")
	}
	// Validate visibility as an enum so a typo never silently broadens reach.
	switch visibility {
	case "connections", "public":
	default:
		return fmt.Errorf("invalid visibility %q: want \"connections\" or \"public\"", visibility)
	}
	payload := map[string]any{
		"visibleToConnectionsOnly": visibility == "connections",
		"commentary": map[string]string{
			"text": text,
		},
		// LinkedIn's normShares payload also expects a distribution/origin
		// block in practice; kept minimal here since the exact required
		// fields need live verification.
		"origin": "FEED",
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	// NEEDS VERIFICATION: /contentcreation/normShares is the last-known path
	// for creating a text share; body shape (visibility, commentary nesting)
	// may have drifted.
	_, err = c.post(ctx, "/contentcreation/normShares", bytes.NewReader(body), ratelimit.Write)
	return err
}

// Comment posts a top-level comment on a feed object (a post, article, etc.)
// identified by its URN.
func (c *Client) Comment(ctx context.Context, objectURN, text string) error {
	if objectURN == "" {
		return fmt.Errorf("empty object urn")
	}
	if text == "" {
		return fmt.Errorf("empty comment text")
	}
	payload := map[string]any{
		"commentary": map[string]string{
			"text": text,
		},
		"parentUrn": objectURN,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	// NEEDS VERIFICATION: /feed/comments is the last-known path for creating
	// a comment; the alternative form nests it as a collection under
	// /socialActions/{urn}/comments, which some LinkedIn API versions use
	// instead. Swap here if live traffic shows otherwise.
	_, err = c.post(ctx, "/feed/comments", bytes.NewReader(body), ratelimit.Write)
	return err
}

// React adds a reaction (like, celebrate, support, love, insightful, funny)
// to a feed object identified by its URN.
func (c *Client) React(ctx context.Context, objectURN, reactionType string) error {
	if objectURN == "" {
		return fmt.Errorf("empty object urn")
	}
	if reactionType == "" {
		reactionType = "like"
	}
	payload := map[string]any{
		"root":         objectURN,
		"reactionType": reactionType,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	// NEEDS VERIFICATION: /feed/reactions is the last-known path (note that
	// c.post already prefixes baseURL, which itself already includes
	// /voyager/api, so this hits {baseURL}/feed/reactions). The legacy
	// alternative is /socialActions/{urn}/likes; swap if this 404s in
	// practice.
	_, err = c.post(ctx, "/feed/reactions", bytes.NewReader(body), ratelimit.Write)
	return err
}

// Engagement returns the like and comment counts for a feed object URN, using
// the social detail endpoint (the same counts embedded in feed updates, but
// fetchable standalone for a URN the caller already has, e.g. from `feed
// read`).
func (c *Client) Engagement(ctx context.Context, objectURN string) (likes int, comments int, err error) {
	if objectURN == "" {
		return 0, 0, fmt.Errorf("empty object urn")
	}
	// NEEDS VERIFICATION: /feed/socialDetail/{urn} is the last-known path for
	// a standalone social-counts lookup; the encoded URN is appended as a
	// path segment per REST-li convention.
	path := "/feed/socialDetail/" + url.PathEscape(objectURN)
	body, err := c.get(ctx, path, nil)
	if err != nil {
		return 0, 0, err
	}
	var detail struct {
		TotalSocialActivityCounts struct {
			NumLikes    int `json:"numLikes"`
			NumComments int `json:"numComments"`
		} `json:"totalSocialActivityCounts"`
	}
	if err := json.Unmarshal(body, &detail); err != nil {
		return 0, 0, fmt.Errorf("decode socialDetail: %w", err)
	}
	return detail.TotalSocialActivityCounts.NumLikes, detail.TotalSocialActivityCounts.NumComments, nil
}
