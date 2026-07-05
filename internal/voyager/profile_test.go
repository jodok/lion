package voyager

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fixtureTransport serves recorded Voyager responses based on the request path.
// This is the canonical test pattern for lion's Voyager client: no live account
// is needed, and every resource vertical should mirror it.
type fixtureTransport struct {
	// routes maps a path substring to a fixture filename under testdata/fixtures.
	routes map[string]string
	// lastReq captures the most recent request for header assertions.
	lastReq *http.Request
}

func (f *fixtureTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	f.lastReq = req
	for sub, file := range f.routes {
		if strings.Contains(req.URL.Path, sub) {
			b, err := os.ReadFile(filepath.Join("..", "..", "testdata", "fixtures", file))
			if err != nil {
				return nil, err
			}
			return &http.Response{
				StatusCode: 200,
				Body:       io.NopCloser(strings.NewReader(string(b))),
				Header:     make(http.Header),
				Request:    req,
			}, nil
		}
	}
	return &http.Response{StatusCode: 404, Body: io.NopCloser(strings.NewReader("{}")), Header: make(http.Header)}, nil
}

func newTestClient(t *testing.T, routes map[string]string) (*Client, *fixtureTransport) {
	t.Helper()
	ft := &fixtureTransport{routes: routes}
	c := New("li_at_test", `"jsession_test"`, WithTransport(ft))
	return c, ft
}

func TestMe(t *testing.T) {
	c, ft := newTestClient(t, map[string]string{"/me": "me.json"})
	me, err := c.Me(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// /me returns a *miniProfile reference resolved from included.
	if got := me.Name(); got != "Ada Lovelace" {
		t.Errorf("name = %q, want Ada Lovelace", got)
	}
	if me.PublicID != "ada-lovelace" {
		t.Errorf("public id = %q", me.PublicID)
	}
	// The CSRF header must be the JSESSIONID with quotes stripped.
	if got := ft.lastReq.Header.Get("Csrf-Token"); got != "jsession_test" {
		t.Errorf("csrf-token = %q, want jsession_test", got)
	}
}

func TestProfileByIDUnsupported(t *testing.T) {
	c, _ := newTestClient(t, map[string]string{})
	// The legacy profileView endpoint is gone (410); we fail fast with ErrNotFound.
	if _, err := c.Profile(context.Background(), "ada-lovelace"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestSearchPeople(t *testing.T) {
	c, ft := newTestClient(t, map[string]string{"/graphql": "people_search.json"})
	res, err := c.SearchPeople(context.Background(), "compiler engineer", 0)
	if err != nil {
		t.Fatal(err)
	}
	// Two people results; the premium upsell entity must be skipped.
	if len(res) != 2 {
		t.Fatalf("got %d results, want 2", len(res))
	}
	if res[0].Name != "Grace Hopper" || res[0].PublicID != "grace-hopper" {
		t.Errorf("first result = %+v", res[0])
	}
	if res[0].Headline != "Rear Admiral · Compiler pioneer" || res[0].Location != "Arlington, Virginia" {
		t.Errorf("headline/location = %q / %q", res[0].Headline, res[0].Location)
	}
	// Keywords with a space must be percent-encoded in the GraphQL variables.
	if q := ft.lastReq.URL.RawQuery; !strings.Contains(q, "compiler%20engineer") {
		t.Errorf("expected encoded keywords in query, got %q", q)
	}
}

func TestSearchPeopleRespectsMax(t *testing.T) {
	c, _ := newTestClient(t, map[string]string{"/graphql": "people_search.json"})
	res, err := c.SearchPeople(context.Background(), "x", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 {
		t.Fatalf("got %d, want 1", len(res))
	}
}
