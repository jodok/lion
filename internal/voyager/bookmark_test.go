package voyager

import (
	"context"
	"strings"
	"testing"
)

func TestSavedPosts(t *testing.T) {
	c, ft := newTestClient(t, map[string]string{"/graphql": "saved_posts.json"})
	posts, err := c.SavedPosts(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(posts) != 2 {
		t.Fatalf("got %d saved posts, want 2", len(posts))
	}

	// The request must carry the saved-posts intent, not a plain search.
	if got := ft.lastReq.URL; !strings.Contains(got, "SEARCH_MY_ITEMS_SAVED_POSTS") {
		t.Errorf("request URL = %q, want the saved-posts intent", got)
	}

	first := posts[0]
	// trackingUrn, not entityUrn: entityUrn wraps the activity together with
	// the search context it came back in, so it differs between queries for
	// the same post and is useless as an archive key.
	if first.URN != "urn:li:activity:111" {
		t.Errorf("URN = %q, want the activity urn", first.URN)
	}
	if first.URL != "https://www.linkedin.com/feed/update/urn:li:activity:111" {
		t.Errorf("URL = %q", first.URL)
	}
	if first.Author != "Acme Corp" || first.AuthorURL != "https://www.linkedin.com/company/acme/" {
		t.Errorf("author = %q / %q", first.Author, first.AuthorURL)
	}
	if first.Summary != "A plain text post with no embedded link." {
		t.Errorf("Summary = %q", first.Summary)
	}
	// A post with nothing embedded must not invent a link.
	if first.LinkedTitle != "" || first.LinkedSubtitle != "" {
		t.Errorf("plain post reported linked content: %q / %q", first.LinkedTitle, first.LinkedSubtitle)
	}

	// The second embeds an article, which is what `bookmark fetch` will need.
	second := posts[1]
	if second.LinkedTitle != "On the Analytical Engine" {
		t.Errorf("LinkedTitle = %q", second.LinkedTitle)
	}
	if second.LinkedSubtitle != "example.com • 8 min read" {
		t.Errorf("LinkedSubtitle = %q", second.LinkedSubtitle)
	}
}

// TestSavedPostsIgnoresUnsavedEntities is the membership guarantee: included[]
// is the flat index of every object the response touches, so a post
// referenced for context but absent from items[] must not be reported as
// saved. Sweeping included[] by type — which decodePeopleSearch does, getting
// away with it via a navigation-URL filter — would list it.
func TestSavedPostsIgnoresUnsavedEntities(t *testing.T) {
	c, _ := newTestClient(t, map[string]string{"/graphql": "saved_posts.json"})
	posts, err := c.SavedPosts(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range posts {
		if p.URN == "urn:li:activity:999" {
			t.Error("a post present only in included[] was reported as saved")
		}
	}
}

// TestSavedPostsToleratesGraphQLErrors: the live response carries a
// NullValueInNonNullableField error about a metadata field while returning
// perfectly good items. Treating data.errors as fatal would make the command
// fail on every real call.
func TestSavedPostsToleratesGraphQLErrors(t *testing.T) {
	c, _ := newTestClient(t, map[string]string{"/graphql": "saved_posts.json"})
	posts, err := c.SavedPosts(context.Background(), 0)
	if err != nil {
		t.Fatalf("a metadata-level GraphQL error must not fail the call: %v", err)
	}
	if len(posts) == 0 {
		t.Error("no posts decoded from a response that carried both items and an error")
	}
}

func TestSavedPostsRespectsMax(t *testing.T) {
	c, _ := newTestClient(t, map[string]string{"/graphql": "saved_posts.json"})
	posts, err := c.SavedPosts(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(posts) != 1 {
		t.Fatalf("got %d, want 1", len(posts))
	}
}
