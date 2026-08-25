package voyager

import (
	"encoding/json"
	"strings"
	"testing"
)

// F11: SharedSecret is needed internally to call AcceptInvitation but must
// never leak into `--json` output.
func TestInvitationJSONOmitsSharedSecret(t *testing.T) {
	inv := Invitation{
		InvitationURN: "urn:li:fs_invitation:1111",
		SharedSecret:  "shared-secret-1111",
		FromName:      "Alan Turing",
		FromPublicID:  "alan-turing",
		Incoming:      true,
	}
	// The Go field must still be readable in-process (AcceptInvitation
	// reads it directly); only the JSON encoding must drop it.
	if inv.SharedSecret != "shared-secret-1111" {
		t.Fatalf("SharedSecret field = %q, want it to remain readable in Go", inv.SharedSecret)
	}
	b, err := json.Marshal(inv)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "shared-secret-1111") || strings.Contains(string(b), "shared_secret") {
		t.Errorf("marshaled invitation leaked the shared secret: %s", b)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if _, ok := m["shared_secret"]; ok {
		t.Errorf("marshaled invitation has a shared_secret key: %s", b)
	}
	if _, ok := m["-"]; ok {
		t.Errorf(`marshaled invitation has a literal "-" key: %s`, b)
	}
}
