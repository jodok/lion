package voyager

import (
	"context"
	"fmt"
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

// TestSavedPostsShapeDriftIsAnError is the guard the other decoders in this
// package already have: a response whose shape has moved must fail loudly
// rather than read as "you have nothing saved". An archiving job built on the
// silent version would stop capturing and still look healthy.
func TestSavedPostsShapeDriftIsAnError(t *testing.T) {
	// included[] carries saved posts, but the cluster items reference none —
	// exactly what a renamed key or restructured cluster would produce.
	body := []byte(`{
	  "data": {"data": {"searchDashClustersByAll": {
	    "elements": [{"items": []}],
	    "metadata": {"totalResultCount": 2}
	  }}},
	  "included": [
	    {"$type": "com.linkedin.voyager.dash.search.EntityResultViewModel",
	     "entityUrn": "urn:li:fsd_entityResultViewModel:(urn:li:activity:1,SEARCH,DEFAULT)",
	     "trackingUrn": "urn:li:activity:1"}
	  ]
	}`)
	_, _, err := decodeSavedPostsPage(body)
	if err == nil {
		t.Fatal("shape drift decoded as an empty result, want an error")
	}
	if !strings.Contains(err.Error(), "shape not recognized") {
		t.Errorf("error = %q, want it to name the shape change", err)
	}
}

// TestSavedPostsEmptyIsNotAnError: someone with nothing saved must get an
// empty list, not the shape-drift error above.
func TestSavedPostsEmptyIsNotAnError(t *testing.T) {
	body := []byte(`{
	  "data": {"data": {"searchDashClustersByAll": {
	    "elements": [], "metadata": {"totalResultCount": 0}
	  }}},
	  "included": []
	}`)
	posts, total, err := decodeSavedPostsPage(body)
	if err != nil {
		t.Fatalf("an empty saved list must not error: %v", err)
	}
	if len(posts) != 0 || total != 0 {
		t.Errorf("got %d posts / total %d, want both zero", len(posts), total)
	}
}

// TestSavedPostsPagesUntilTheTotalIsReached: bookmarks exist to be archived
// completely, so stopping after one page and letting the caller believe that
// is everything would defeat the point.
func TestSavedPostsPagesUntilTheTotalIsReached(t *testing.T) {
	page := func(total int, urns ...string) string {
		var items, inc []string
		for _, u := range urns {
			ref := "urn:li:fsd_entityResultViewModel:(" + u + ",SEARCH,DEFAULT)"
			items = append(items, `{"item":{"*entityResult":"`+ref+`"}}`)
			inc = append(inc, `{"$type":"com.linkedin.voyager.dash.search.EntityResultViewModel",
				"entityUrn":"`+ref+`","trackingUrn":"`+u+`","title":{"text":"x"}}`)
		}
		return `{"data":{"data":{"searchDashClustersByAll":{
			"elements":[{"items":[` + strings.Join(items, ",") + `]}],
			"metadata":{"totalResultCount":` + fmt.Sprint(total) + `}}}},
			"included":[` + strings.Join(inc, ",") + `]}`
	}
	st := &sequenceTransport{responses: []*Response{
		{StatusCode: 200, Body: []byte(page(3, "urn:li:activity:1", "urn:li:activity:2"))},
		{StatusCode: 200, Body: []byte(page(3, "urn:li:activity:3"))},
		{StatusCode: 200, Body: []byte(page(3))},
	}}
	c := New("li_at", `"ajax:1"`, WithTransport(st), WithLimiter(noopLimiter()))

	posts, err := c.SavedPosts(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(posts) != 3 {
		t.Fatalf("got %d saved posts, want all 3 across pages", len(posts))
	}
	if st.calls < 2 {
		t.Errorf("made %d requests, want it to page past the first", st.calls)
	}
	// The walk must stop once the reported total is reached rather than
	// fetching until the page limit.
	if st.calls > 3 {
		t.Errorf("made %d requests for a 3-post mailbox; the total should have ended the walk", st.calls)
	}
}

// TestSavedPostsPagingStopsOnMaxAndOnEmptyPage covers the two other exits:
// the caller's cap, and a page that adds nothing.
func TestSavedPostsPagingStopsOnMaxAndOnEmptyPage(t *testing.T) {
	body := `{"data":{"data":{"searchDashClustersByAll":{
		"elements":[{"items":[
			{"item":{"*entityResult":"r1"}},{"item":{"*entityResult":"r2"}}]}],
		"metadata":{"totalResultCount":99}}}},
		"included":[
		 {"$type":"com.linkedin.voyager.dash.search.EntityResultViewModel","entityUrn":"r1","trackingUrn":"urn:li:activity:1","title":{"text":"a"}},
		 {"$type":"com.linkedin.voyager.dash.search.EntityResultViewModel","entityUrn":"r2","trackingUrn":"urn:li:activity:2","title":{"text":"b"}}]}`

	// max stops it.
	st := &sequenceTransport{responses: []*Response{{StatusCode: 200, Body: []byte(body)}}}
	c := New("li_at", `"ajax:1"`, WithTransport(st), WithLimiter(noopLimiter()))
	posts, err := c.SavedPosts(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(posts) != 1 {
		t.Errorf("got %d, want the cap of 1", len(posts))
	}

	// The same body repeating adds nothing after the first page, and the walk
	// must end there rather than run to the page limit chasing a total of 99.
	st2 := &sequenceTransport{responses: []*Response{{StatusCode: 200, Body: []byte(body)}}}
	c2 := New("li_at", `"ajax:1"`, WithTransport(st2), WithLimiter(noopLimiter()))
	posts, err = c2.SavedPosts(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(posts) != 2 {
		t.Errorf("got %d, want the 2 distinct posts", len(posts))
	}
	if st2.calls > 3 {
		t.Errorf("made %d requests against a repeating page; a page adding nothing must end the walk", st2.calls)
	}
}
