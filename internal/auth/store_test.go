package auth

import (
	"os"
	"path/filepath"
	"testing"
)

// isolate points LION_HOME at a fresh temp directory so tests never touch a
// real credentials.json.
func isolate(t *testing.T) {
	t.Helper()
	t.Setenv("LION_HOME", t.TempDir())
}

func TestSaveGetRoundTripsCookies(t *testing.T) {
	isolate(t)
	cred := &Credential{
		Alias: "default",
		Cookies: map[string]string{
			"li_at":      "abc",
			"JSESSIONID": `"ajax:1"`,
			"bcookie":    "b1",
		},
	}
	if err := Save(cred); err != nil {
		t.Fatal(err)
	}
	got, err := Get("default")
	if err != nil {
		t.Fatal(err)
	}
	// normalize() must have refreshed the legacy fields from Cookies.
	if got.LiAt != "abc" {
		t.Errorf("LiAt = %q, want abc", got.LiAt)
	}
	if got.JSessionID != `"ajax:1"` {
		t.Errorf("JSessionID = %q, want quoted ajax:1", got.JSessionID)
	}
	if got.Cookies["bcookie"] != "b1" {
		t.Errorf("Cookies[bcookie] = %q, want b1", got.Cookies["bcookie"])
	}
}

func TestNormalizeSynthesizesCookiesFromLegacyFields(t *testing.T) {
	c := &Credential{LiAt: "abc", JSessionID: `"ajax:1"`}
	c.normalize()
	want := map[string]string{"li_at": "abc", "JSESSIONID": `"ajax:1"`}
	if len(c.Cookies) != len(want) || c.Cookies["li_at"] != want["li_at"] || c.Cookies["JSESSIONID"] != want["JSESSIONID"] {
		t.Errorf("Cookies = %#v, want %#v", c.Cookies, want)
	}
}

// TestLoadSynthesizesCookiesForOldCredentialsFile simulates a
// credentials.json written before the Cookies field existed (no "cookies"
// key at all) and checks that loading it populates Cookies from the legacy
// li_at/jsessionid fields, so old `auth login` sessions keep working with
// the new cookie-map-based Client wiring.
func TestLoadSynthesizesCookiesForOldCredentialsFile(t *testing.T) {
	isolate(t)
	home := os.Getenv("LION_HOME")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	old := `{
		"default": "default",
		"accounts": {
			"default": {
				"alias": "default",
				"li_at": "old-li-at",
				"jsessionid": "\"old-jsession\"",
				"saved_at": "2024-01-01T00:00:00Z"
			}
		}
	}`
	if err := os.WriteFile(filepath.Join(home, "credentials.json"), []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := Get("default")
	if err != nil {
		t.Fatal(err)
	}
	if got.Cookies["li_at"] != "old-li-at" {
		t.Errorf("Cookies[li_at] = %q, want old-li-at", got.Cookies["li_at"])
	}
	if got.Cookies["JSESSIONID"] != `"old-jsession"` {
		t.Errorf("Cookies[JSESSIONID] = %q, want quoted old-jsession", got.Cookies["JSESSIONID"])
	}
}

func TestGetNoAccount(t *testing.T) {
	isolate(t)
	if _, err := Get("default"); err != ErrNoAccount {
		t.Errorf("Get on empty store = %v, want ErrNoAccount", err)
	}
}
