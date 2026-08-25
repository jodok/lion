package auth

import (
	"os"
	"path/filepath"
	"testing"
)

// TestSaveWritesRestrictivePermissions is the F12 regression test: the
// credentials file must end up 0600 regardless of any ambient umask or a
// permissive leftover at the temp path.
func TestSaveWritesRestrictivePermissions(t *testing.T) {
	isolate(t)
	if err := Save(&Credential{Alias: "default", Cookies: map[string]string{"li_at": "x"}}); err != nil {
		t.Fatal(err)
	}
	home := os.Getenv("LION_HOME")
	fi, err := os.Stat(filepath.Join(home, "credentials.json"))
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("credentials.json perm = %o, want 0600", perm)
	}
}

// TestSaveUsesUniqueTempFiles pins the F12/F24 fix: two sequential saves
// must not reuse (or collide on) a fixed "credentials.json.tmp" path, and
// no temp file should be left behind afterward.
func TestSaveUsesUniqueTempFiles(t *testing.T) {
	isolate(t)
	if err := Save(&Credential{Alias: "a", Cookies: map[string]string{"li_at": "1"}}); err != nil {
		t.Fatal(err)
	}
	if err := Save(&Credential{Alias: "b", Cookies: map[string]string{"li_at": "2"}}); err != nil {
		t.Fatal(err)
	}
	home := os.Getenv("LION_HOME")
	entries, err := os.ReadDir(home)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() == "credentials.json.tmp" {
			t.Errorf("found leftover fixed-name temp file %q; expected unique names cleaned up after rename", e.Name())
		}
		if filepath.Ext(e.Name()) == ".tmp" {
			t.Errorf("found leftover temp file %q after save completed", e.Name())
		}
	}
	// Both accounts must have survived two sequential saves without
	// corrupting the store.
	got, _, err := List()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("List() after two sequential saves = %d accounts, want 2", len(got))
	}
}

// TestListSortedByAlias is the F18 regression test.
func TestListSortedByAlias(t *testing.T) {
	isolate(t)
	for _, alias := range []string{"zebra", "alpha", "mid"} {
		if err := Save(&Credential{Alias: alias, Cookies: map[string]string{"li_at": "x"}}); err != nil {
			t.Fatal(err)
		}
	}
	got, _, err := List()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"alpha", "mid", "zebra"}
	if len(got) != len(want) {
		t.Fatalf("List() = %d accounts, want %d", len(got), len(want))
	}
	for i, w := range want {
		if got[i].Alias != w {
			t.Errorf("List()[%d].Alias = %q, want %q (not sorted)", i, got[i].Alias, w)
		}
	}
}

// TestDefaultAlias covers the F2 accessor logout relies on.
func TestDefaultAlias(t *testing.T) {
	isolate(t)
	if got, err := DefaultAlias(); err != nil || got != "" {
		t.Errorf("DefaultAlias() on empty store = (%q, %v), want (\"\", nil)", got, err)
	}
	if err := Save(&Credential{Alias: "work", Cookies: map[string]string{"li_at": "x"}}); err != nil {
		t.Fatal(err)
	}
	if got, err := DefaultAlias(); err != nil || got != "work" {
		t.Errorf("DefaultAlias() = (%q, %v), want (\"work\", nil)", got, err)
	}
}

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

func TestNormalizeCookies(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"unquoted", "ajax:1", `"ajax:1"`},
		{"already quoted (idempotent)", `"ajax:1"`, `"ajax:1"`},
		{"doubled quotes", `""ajax:1""`, `"ajax:1"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cookies := map[string]string{"JSESSIONID": tc.in, "li_at": "keep"}
			NormalizeCookies(cookies)
			if cookies["JSESSIONID"] != tc.want {
				t.Errorf("JSESSIONID = %q, want %q", cookies["JSESSIONID"], tc.want)
			}
			if cookies["li_at"] != "keep" {
				t.Errorf("li_at = %q, want it left untouched", cookies["li_at"])
			}
		})
	}
	// Empty or absent JSESSIONID is left as-is.
	empty := map[string]string{"JSESSIONID": ""}
	NormalizeCookies(empty)
	if empty["JSESSIONID"] != "" {
		t.Errorf("empty JSESSIONID = %q, want empty", empty["JSESSIONID"])
	}
}

// TestNormalizeQuotesLegacyUnquotedJSession covers the migration case Fix 1
// closed: an old credential whose JSESSIONID was stored WITHOUT the wire
// quotes must come out wrapped in exactly one pair after normalize(), so
// GraphQL requests carry the value LinkedIn expects.
func TestNormalizeQuotesLegacyUnquotedJSession(t *testing.T) {
	c := &Credential{LiAt: "abc", JSessionID: "ajax:1"}
	c.normalize()
	if c.Cookies["JSESSIONID"] != `"ajax:1"` {
		t.Errorf("Cookies[JSESSIONID] = %q, want quoted", c.Cookies["JSESSIONID"])
	}
	if c.JSessionID != `"ajax:1"` {
		t.Errorf("JSessionID field = %q, want refreshed to quoted value", c.JSessionID)
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

// TestUpdateCookiesRoundTripsAndRenormalizes covers the cookie-writeback
// happy path: a rotated value overlays the stored credential, is persisted,
// and JSESSIONID's wire quoting is corrected by the same normalize() path
// every other write goes through, even when the rotated value arrived
// unquoted.
func TestUpdateCookiesRoundTripsAndRenormalizes(t *testing.T) {
	isolate(t)
	if err := Save(&Credential{Alias: "default", Cookies: map[string]string{
		"li_at":      "orig-li-at",
		"JSESSIONID": `"orig-jsession"`,
		"bcookie":    "b1",
	}}); err != nil {
		t.Fatal(err)
	}
	changed, err := UpdateCookies("default", map[string]string{
		"li_at":      "rotated-li-at",
		"JSESSIONID": "rotated-jsession", // unquoted on purpose
	})
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("UpdateCookies() changed = false, want true")
	}
	got, err := Get("default")
	if err != nil {
		t.Fatal(err)
	}
	if got.Cookies["li_at"] != "rotated-li-at" {
		t.Errorf("Cookies[li_at] = %q, want rotated-li-at", got.Cookies["li_at"])
	}
	if got.Cookies["JSESSIONID"] != `"rotated-jsession"` {
		t.Errorf("Cookies[JSESSIONID] = %q, want quoted rotated-jsession", got.Cookies["JSESSIONID"])
	}
	// A cookie UpdateCookies never mentioned must survive untouched.
	if got.Cookies["bcookie"] != "b1" {
		t.Errorf("Cookies[bcookie] = %q, want untouched b1", got.Cookies["bcookie"])
	}
}

// TestUpdateCookiesNoopWhenNothingChanged pins the "must not write" half of
// the contract: a rotated map that already matches what's stored must
// report changed=false and must not touch the file on disk (checked via
// mtime, since a rewrite would bump it even with identical content).
func TestUpdateCookiesNoopWhenNothingChanged(t *testing.T) {
	isolate(t)
	if err := Save(&Credential{Alias: "default", Cookies: map[string]string{
		"li_at":      "same",
		"JSESSIONID": `"same-jsession"`,
	}}); err != nil {
		t.Fatal(err)
	}
	home := os.Getenv("LION_HOME")
	path := filepath.Join(home, "credentials.json")
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	changed, err := UpdateCookies("default", map[string]string{
		"li_at":      "same",
		"JSESSIONID": `"same-jsession"`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Error("UpdateCookies() changed = true, want false when nothing actually changed")
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !before.ModTime().Equal(after.ModTime()) {
		t.Errorf("credentials.json was rewritten (mtime %v -> %v) despite no change", before.ModTime(), after.ModTime())
	}
}

// TestUpdateCookiesUnknownAliasReturnsFalseNotError is the "must never break
// a command" requirement: a writeback for an account that doesn't exist (or
// an empty store) must not surface as an error.
func TestUpdateCookiesUnknownAliasReturnsFalseNotError(t *testing.T) {
	isolate(t)
	changed, err := UpdateCookies("no-such-account", map[string]string{"li_at": "x"})
	if err != nil {
		t.Errorf("UpdateCookies(unknown alias) err = %v, want nil", err)
	}
	if changed {
		t.Error("UpdateCookies(unknown alias) changed = true, want false")
	}

	// Also covers the empty-store, empty-alias (resolve-to-default) case.
	changed, err = UpdateCookies("", map[string]string{"li_at": "x"})
	if err != nil {
		t.Errorf("UpdateCookies(\"\") on empty store err = %v, want nil", err)
	}
	if changed {
		t.Error("UpdateCookies(\"\") on empty store changed = true, want false")
	}
}

func TestGetNoAccount(t *testing.T) {
	isolate(t)
	if _, err := Get("default"); err != ErrNoAccount {
		t.Errorf("Get on empty store = %v, want ErrNoAccount", err)
	}
}
