package voyager

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestConnections(t *testing.T) {
	c, _ := newTestClient(t, map[string]string{"/relationships/connections": "connections.json"})
	conns, err := c.Connections(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	// connections.json lists elements in the order [Katherine, Grace] and
	// also carries an "aux-cached-profile" MiniProfile in included[] that
	// is not referenced by data.elements — it must never be returned.
	if len(conns) != 2 {
		t.Fatalf("got %d connections, want 2: %+v", len(conns), conns)
	}
	if conns[0].Name != "Katherine Johnson" || conns[0].PublicID != "katherine-johnson" {
		t.Errorf("first connection = %+v, want Katherine Johnson (data.elements order)", conns[0])
	}
	if conns[1].Name != "Grace Hopper" || conns[1].PublicID != "grace-hopper" {
		t.Errorf("second connection = %+v, want Grace Hopper", conns[1])
	}
	for _, c := range conns {
		if c.PublicID == "aux-cached-profile" {
			t.Errorf("aux cached profile (not in data.elements) was returned: %+v", c)
		}
	}
}

func TestConnectionsRespectsMax(t *testing.T) {
	c, _ := newTestClient(t, map[string]string{"/relationships/connections": "connections.json"})
	conns, err := c.Connections(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(conns) != 1 {
		t.Fatalf("got %d, want 1", len(conns))
	}
	// max should cap the server's own order (data.elements), not whatever
	// order included[] happens to store entities in.
	if conns[0].PublicID != "katherine-johnson" {
		t.Errorf("first connection = %+v, want katherine-johnson (first in data.elements)", conns[0])
	}
}

// TestInvitations asserts the live call fails honestly (F4): LinkedIn's
// /relationships/invitationViews returns 400 in practice (DESIGN.md §3.2),
// so Invitations must never hit it — it returns a typed, wrapped
// ErrNotFound instead, without making any HTTP request.
func TestInvitations(t *testing.T) {
	ft := &fixtureTransport{routes: map[string]string{"/relationships/invitationViews": "invitations.json"}}
	c := New("li_at_test", `"jsession_test"`, WithTransport(ft), WithLimiter(noopLimiter()))
	_, err := c.Invitations(context.Background(), true)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	if ft.lastReq != nil {
		t.Errorf("Invitations made an HTTP request (endpoint is known-broken): %s", ft.lastReq.URL)
	}
}

// TestDecodeInvitations exercises decodeInvitations directly (it's no
// longer reachable through Invitations, see TestInvitations above) to prove
// the ordered-decode fix: results follow data.elements order, and entries
// in included[] that data.elements doesn't reference are dropped.
func TestDecodeInvitations(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "..", "testdata", "fixtures", "invitations.json"))
	if err != nil {
		t.Fatal(err)
	}
	invs, err := decodeInvitations(body, true)
	if err != nil {
		t.Fatal(err)
	}
	// invitations.json lists elements in the order [2222, 1111] and also
	// carries a 9999 InvitationView in included[] that data.elements never
	// references (and, separately, whose sharedSecret marks it as not
	// pending) — it must never be returned.
	if len(invs) != 2 {
		t.Fatalf("got %d invitations, want 2: %+v", len(invs), invs)
	}
	if invs[0].InvitationURN != "urn:li:fs_invitation:2222" || invs[0].FromName != "Katherine Johnson" {
		t.Errorf("first invitation = %+v, want invitation 2222 from Katherine Johnson (data.elements order)", invs[0])
	}
	if invs[1].InvitationURN != "urn:li:fs_invitation:1111" {
		t.Errorf("second invitation = %+v, want invitation 1111", invs[1])
	}
	if invs[1].SharedSecret != "shared-secret-1111" {
		t.Errorf("shared secret = %q", invs[1].SharedSecret)
	}
	if invs[1].FromName != "Alan Turing" || invs[1].FromPublicID != "alan-turing" {
		t.Errorf("from = %+v", invs[1])
	}
	if !invs[1].Incoming {
		t.Errorf("expected incoming = true")
	}
	for _, inv := range invs {
		if inv.InvitationURN == "urn:li:fs_invitation:9999" {
			t.Errorf("aux invitation (not in data.elements) was returned: %+v", inv)
		}
	}
}

// TestInviteDryRun asserts that Invite under --dry-run never performs an HTTP
// call and reports success. The fixtureTransport has no route for the invite
// path, so any real request would surface as a decode/404 error.
func TestInviteDryRun(t *testing.T) {
	ft := &fixtureTransport{routes: map[string]string{}}
	c := New("li_at_test", `"jsession_test"`, WithTransport(ft), WithDryRun(true))
	if err := c.Invite(context.Background(), "grace-hopper", "hello"); err != nil {
		t.Fatalf("Invite under dry-run returned error: %v", err)
	}
	if ft.lastReq != nil {
		t.Errorf("Invite under dry-run made an HTTP request: %s", ft.lastReq.URL)
	}
}
