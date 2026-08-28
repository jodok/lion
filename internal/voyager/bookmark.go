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

// savedPostsPageSize is how many saved posts one request asks for. LinkedIn
// decides what it actually returns; the walk below follows the reported total
// rather than trusting this.
const savedPostsPageSize = 20

// savedPostsPageLimit bounds the walk. A server that keeps reporting a total
// it never reaches would otherwise loop forever; this turns that into a
// bounded, reportable stop.
const savedPostsPageLimit = 50

// SavedPosts returns the member's saved posts, most recently saved first.
// max caps the result (0 = every saved post).
//
// It pages rather than taking the first response: bookmarks exist to be
// archived completely, so silently returning one page's worth and letting the
// caller believe that is everything would defeat the point. profile search
// shares this query and does not page, which is right for a browse command
// where the first page is the answer.
func (c *Client) SavedPosts(ctx context.Context, max int) ([]SavedPost, error) {
	var out []SavedPost
	seen := map[string]bool{}
	start := 0
	for page := 0; page < savedPostsPageLimit; page++ {
		vars := fmt.Sprintf("(start:%d,count:%d,query:(flagshipSearchIntent:%s))",
			start, savedPostsPageSize, savedPostsIntent)
		body, err := c.getGraphQL(ctx, queryIDSearchClusters, vars)
		if err != nil {
			return nil, err
		}
		posts, total, err := decodeSavedPostsPage(body)
		if err != nil {
			return nil, err
		}
		added := 0
		for _, p := range posts {
			if seen[p.URN] {
				continue
			}
			seen[p.URN] = true
			out = append(out, p)
			added++
			if max > 0 && len(out) >= max {
				return out, nil
			}
		}
		// A page that adds nothing ends the walk, checked before the total:
		// a server that keeps reporting a count it never delivers must not
		// spin here.
		if added == 0 {
			return out, nil
		}
		if total > 0 && len(out) >= total {
			return out, nil
		}
		start += len(posts)
	}
	return out, nil
}

// decodeSavedPosts is the single-response decoder, kept for tests and for
// callers that already hold a body.
func decodeSavedPosts(body []byte, max int) ([]SavedPost, error) {
	posts, _, err := decodeSavedPostsPage(body)
	if err != nil {
		return nil, err
	}
	if max > 0 && len(posts) > max {
		posts = posts[:max]
	}
	return posts, nil
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
				Metadata struct {
					TotalResultCount int `json:"totalResultCount"`
				} `json:"metadata"`
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
func decodeSavedPostsPage(body []byte) ([]SavedPost, int, error) {
	_, idx, err := parseNormalized(body)
	if err != nil {
		return nil, 0, err
	}
	var env savedPostsEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, 0, fmt.Errorf("decode saved posts: %w", err)
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
		}
	}

	// Shape-drift guard, matching connection.go, feed.go and messaging.go: if
	// included[] carries saved posts but the items referenced none of them,
	// the decoder has fallen behind a Voyager change. Reporting that as "you
	// have no saved posts" would let an archiving job stop capturing anything
	// and still look healthy.
	if len(out) == 0 {
		if n := len(idx.ofType(typeEntityResultViewModel)); n > 0 {
			return nil, 0, fmt.Errorf("saved posts response shape not recognized: %d entity result(s) "+
				"present in included[] but none referenced by the cluster items; the decoder likely "+
				"needs updating for a changed Voyager response shape: %w", n, ErrNotFound)
		}
	}
	return out, env.Data.Data.Clusters.Metadata.TotalResultCount, nil
}

// typeEntityResultViewModel is the included[] $type saved posts arrive as.
const typeEntityResultViewModel = "com.linkedin.voyager.dash.search.EntityResultViewModel"
