package voyager

import (
	"context"
	"testing"
)

func TestFeed(t *testing.T) {
	c, _ := newTestClient(t, map[string]string{"/feed/updatesV2": "feed.json"})
	items, err := c.Feed(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	// feed.json lists elements in the order [Katherine's update, Grace's
	// update] and also carries an "aux" UpdateV2 (activity ...099) in
	// included[] that data.elements never references — it must never be
	// returned.
	if len(items) != 2 {
		t.Fatalf("got %d items, want 2: %+v", len(items), items)
	}
	if items[0].AuthorName != "Katherine Johnson" {
		t.Errorf("author = %q, want Katherine Johnson (data.elements order)", items[0].AuthorName)
	}
	if items[0].Text != "Excited to share our latest trajectory analysis." {
		t.Errorf("text = %q", items[0].Text)
	}
	if items[0].Likes != 108 || items[0].Comments != 15 {
		t.Errorf("likes/comments = %d/%d, want 108/15", items[0].Likes, items[0].Comments)
	}
	if items[0].PostedAt != 1717100000000 {
		t.Errorf("postedAt = %d", items[0].PostedAt)
	}
	if items[1].AuthorName != "Grace Hopper" {
		t.Errorf("second author = %q, want Grace Hopper", items[1].AuthorName)
	}
	for _, it := range items {
		if it.AuthorName == "Aux Cached" {
			t.Errorf("aux update (not in data.elements) was returned: %+v", it)
		}
	}
}

func TestFeedRespectsMax(t *testing.T) {
	c, _ := newTestClient(t, map[string]string{"/feed/updatesV2": "feed.json"})
	items, err := c.Feed(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("got %d, want 1", len(items))
	}
	// max should cap the server's own order (data.elements), not whatever
	// order included[] happens to store entities in.
	if items[0].AuthorName != "Katherine Johnson" {
		t.Errorf("author = %q, want Katherine Johnson (first in data.elements)", items[0].AuthorName)
	}
}

// TestCreatePostDryRun asserts the safety-critical property of every mutating
// call: under --dry-run, no HTTP request is made and the call still reports
// success. The fixture transport has no route for normShares, so any real
// request would 404 and this test would fail if dry-run were broken.
func TestCreatePostDryRun(t *testing.T) {
	c, ft := newTestClient(t, map[string]string{})
	c.dryRun = true
	if err := c.CreatePost(context.Background(), "hello world", "connections"); err != nil {
		t.Fatalf("dry-run CreatePost returned error: %v", err)
	}
	if ft.lastReq != nil {
		t.Errorf("dry-run CreatePost made an HTTP request: %s", ft.lastReq.URL)
	}
}

func TestCreatePostRejectsInvalidVisibility(t *testing.T) {
	c, ft := newTestClient(t, map[string]string{})
	// A typo must error before any request, never silently publish as public.
	if err := c.CreatePost(context.Background(), "hello", "conectons"); err == nil {
		t.Fatal("expected error for invalid visibility, got nil")
	}
	if ft.lastReq != nil {
		t.Errorf("invalid visibility should not make a request: %s", ft.lastReq.URL)
	}
}
