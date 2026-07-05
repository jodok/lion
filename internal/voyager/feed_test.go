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
	if len(items) != 2 {
		t.Fatalf("got %d items, want 2", len(items))
	}
	if items[0].AuthorName != "Grace Hopper" {
		t.Errorf("author = %q, want Grace Hopper", items[0].AuthorName)
	}
	if items[0].Text != "Debugging is twice as hard as writing the code in the first place." {
		t.Errorf("text = %q", items[0].Text)
	}
	if items[0].Likes != 42 || items[0].Comments != 7 {
		t.Errorf("likes/comments = %d/%d, want 42/7", items[0].Likes, items[0].Comments)
	}
	if items[0].PostedAt != 1717000000000 {
		t.Errorf("postedAt = %d", items[0].PostedAt)
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
