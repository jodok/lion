package voyager

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// Saved posts ("My Items → Saved posts") come from the same GraphQL query as
// people search — voyagerSearchDashClusters — with a different search intent:
//
//	variables=(start:0,count:N,query:(flagshipSearchIntent:SEARCH_MY_ITEMS_SAVED_POSTS))
//
// Observed by loading linkedin.com/my-items/saved-posts in the browser lion
// drives and reading the request the page issued, which is also why no new
// queryId had to be pinned: LinkedIn reuses the one already in graphql.go.
//
// The response is the normalized form. Cluster items hold only a URN under
// item."*entityResult", and the posts themselves are flattened into
// included[] as EntityResultViewModel entities.

// savedPostsIntent selects the saved-posts view of the search cluster query.
const savedPostsIntent = "SEARCH_MY_ITEMS_SAVED_POSTS"

// SavedPost is one bookmarked post.
type SavedPost struct {
	// URN identifies the post itself (urn:li:activity:<id>), stable across
	// runs and the natural key for a local archive.
	URN string `json:"urn"`
	// URL is the post's permalink.
	URL string `json:"url"`
	// Author is who posted it, and AuthorURL their profile or company page.
	Author    string `json:"author"`
	AuthorURL string `json:"author_url,omitempty"`
	// Subtitle is LinkedIn's own one-line context for the post (the author's
	// headline, or the posting company's tagline).
	Subtitle string `json:"subtitle,omitempty"`
	// Summary is the post's text as LinkedIn renders it in the saved list —
	// an excerpt, not necessarily the whole body.
	Summary string `json:"summary,omitempty"`
	// LinkedTitle and LinkedSubtitle describe content the post embeds: an
	// article, a document, a video. Empty for a plain text post. Fetching
	// what LinkedTitle points at is deliberately not done here — see
	// `lion bookmark fetch`.
	LinkedTitle    string `json:"linked_title,omitempty"`
	LinkedSubtitle string `json:"linked_subtitle,omitempty"`
}

// SavedPosts returns the member's saved posts, most recently saved first.
// max caps the result (0 = LinkedIn's default page).
func (c *Client) SavedPosts(ctx context.Context, max int) ([]SavedPost, error) {
	count := max
	if count <= 0 {
		count = 10
	}
	vars := fmt.Sprintf("(start:0,count:%d,query:(flagshipSearchIntent:%s))", count, savedPostsIntent)
	body, err := c.getGraphQL(ctx, queryIDSearchClusters, vars)
	if err != nil {
		return nil, err
	}
	return decodeSavedPosts(body, max)
}

// savedPostsEnvelope is the shape the cluster items arrive in. Only the
// entityResult references are needed; everything else in a cluster describes
// how LinkedIn would render it.
type savedPostsEnvelope struct {
	Data struct {
		Data struct {
			Clusters struct {
				Elements []struct {
					Items []struct {
						Item struct {
							EntityResult string `json:"*entityResult"`
						} `json:"item"`
					} `json:"items"`
				} `json:"elements"`
			} `json:"searchDashClustersByAll"`
		} `json:"data"`
	} `json:"data"`
}

// decodeSavedPosts resolves each cluster item's entityResult reference
// through included[].
//
// Membership comes from the items, not from sweeping included[] for anything
// of the right type: included[] is the flat index of every object the
// response touches, so a post referenced for context rather than saved would
// be indistinguishable from a real hit. (decodePeopleSearch does sweep, and
// gets away with it because it filters on the navigation URL looking like a
// member profile; there is no equivalent tell here.)
func decodeSavedPosts(body []byte, max int) ([]SavedPost, error) {
	_, idx, err := parseNormalized(body)
	if err != nil {
		return nil, err
	}
	var env savedPostsEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, fmt.Errorf("decode saved posts: %w", err)
	}

	var out []SavedPost
	seen := map[string]bool{}
	for _, cluster := range env.Data.Data.Clusters.Elements {
		for _, item := range cluster.Items {
			ref := item.Item.EntityResult
			if ref == "" {
				continue
			}
			raw, ok := idx.get(ref)
			if !ok {
				continue
			}
			var e struct {
				TrackingUrn        string    `json:"trackingUrn"`
				NavigationURL      string    `json:"navigationUrl"`
				ActorNavigationURL string    `json:"actorNavigationUrl"`
				Title              textField `json:"title"`
				PrimarySub         textField `json:"primarySubtitle"`
				Summary            textField `json:"summary"`
				Embedded           *struct {
					Title      textField `json:"title"`
					PrimarySub textField `json:"primarySubtitle"`
				} `json:"entityEmbeddedObject"`
			}
			if err := decodeInto(raw, &e); err != nil {
				continue
			}
			// trackingUrn is the post's own activity URN and the only stable
			// key here: entityUrn wraps it together with the search context
			// it was returned in, so it differs between queries for the same
			// post.
			if e.TrackingUrn == "" || seen[e.TrackingUrn] {
				continue
			}
			seen[e.TrackingUrn] = true
			p := SavedPost{
				URN:       e.TrackingUrn,
				URL:       e.NavigationURL,
				Author:    strings.TrimSpace(e.Title.Text),
				AuthorURL: e.ActorNavigationURL,
				Subtitle:  strings.TrimSpace(e.PrimarySub.Text),
				Summary:   strings.TrimSpace(e.Summary.Text),
			}
			if e.Embedded != nil {
				p.LinkedTitle = strings.TrimSpace(e.Embedded.Title.Text)
				p.LinkedSubtitle = strings.TrimSpace(e.Embedded.PrimarySub.Text)
			}
			out = append(out, p)
			if max > 0 && len(out) >= max {
				return out, nil
			}
		}
	}
	return out, nil
}
