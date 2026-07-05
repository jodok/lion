package voyager

import (
	"context"
	"testing"
)

func TestConnections(t *testing.T) {
	c, _ := newTestClient(t, map[string]string{"/relationships/connections": "connections.json"})
	conns, err := c.Connections(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(conns) != 2 {
		t.Fatalf("got %d connections, want 2", len(conns))
	}
	if conns[0].Name != "Grace Hopper" || conns[0].PublicID != "grace-hopper" {
		t.Errorf("first connection = %+v", conns[0])
	}
	if conns[1].Name != "Katherine Johnson" || conns[1].PublicID != "katherine-johnson" {
		t.Errorf("second connection = %+v", conns[1])
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
}

func TestInvitations(t *testing.T) {
	c, _ := newTestClient(t, map[string]string{"/relationships/invitationViews": "invitations.json"})
	invs, err := c.Invitations(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	if len(invs) != 1 {
		t.Fatalf("got %d invitations, want 1", len(invs))
	}
	inv := invs[0]
	if inv.InvitationURN != "urn:li:fs_invitation:1111" {
		t.Errorf("invitation urn = %q", inv.InvitationURN)
	}
	if inv.SharedSecret != "shared-secret-1111" {
		t.Errorf("shared secret = %q", inv.SharedSecret)
	}
	if inv.FromName != "Alan Turing" || inv.FromPublicID != "alan-turing" {
		t.Errorf("from = %+v", inv)
	}
	if !inv.Incoming {
		t.Errorf("expected incoming = true")
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
