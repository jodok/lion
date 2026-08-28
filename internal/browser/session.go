package browser

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/go-rod/rod/lib/proto"
	"github.com/jodok/lion/internal/config"
)

// Session persistence does not rely on Chromium writing its own cookie
// store, because in practice it does not reliably do so before lion exits.
//
// Chromium holds cookies in memory and commits them lazily; a sign-in
// followed within seconds by a close is exactly the shape that loses the
// write. This was not theoretical — a real sign-in showed all twelve
// cookies present in the live browser at close, and the profile on disk
// afterwards held only the anonymous set a later run had fetched for
// itself. Every attempt to reproduce it locally persisted correctly,
// including a harness that mimicked production down to exiting the process
// immediately after Close, which is what makes it untrustworthy: a
// mechanism that works on every machine except the one that matters is not
// a mechanism to build on.
//
// So lion exports the jar itself at the end of sign-in and injects it at the
// start of every run. That turns "did Chromium happen to flush?" into a file
// lion writes and reads, which is deterministic, inspectable, and testable
// in the same shape production runs in. The browser profile is still used
// and still accumulates its own state; this just stops the session depending
// on it.

// storedCookie is one cookie, with the attributes needed to put it back
// exactly as LinkedIn issued it. Storing only name and value would drop
// Secure, HttpOnly, and SameSite=None, which LinkedIn's cookies all carry
// and which decide whether the browser sends them at all.
type storedCookie struct {
	Name     string  `json:"name"`
	Value    string  `json:"value"`
	Domain   string  `json:"domain"`
	Path     string  `json:"path"`
	Secure   bool    `json:"secure"`
	HTTPOnly bool    `json:"http_only"`
	SameSite string  `json:"same_site,omitempty"`
	Expires  float64 `json:"expires,omitempty"`
}

// sessionPath returns the cookie file backing alias. It sits beside the
// profile directory rather than inside it: Chromium owns that directory and
// rewrites it freely, and ListProfiles only counts directories, so a sibling
// file cannot be mistaken for an account.
func sessionPath(alias string) (string, error) {
	if alias == "" {
		alias = "default"
	}
	if alias != filepath.Base(alias) || alias == "." || alias == ".." {
		return "", fmt.Errorf("invalid account alias %q", alias)
	}
	home, err := config.EnsureHome()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, "profiles")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return filepath.Join(dir, alias+".cookies.json"), nil
}

// SaveSession exports the browser's LinkedIn cookies for alias.
//
// Written 0600 through a temp file and a rename, so a crash mid-write cannot
// leave a half-parsed session behind — the same discipline the credential
// store uses, and for the same reason: this file is the session.
func (b *Browser) SaveSession(ctx context.Context, alias string) error {
	cookies, err := b.page.Context(ctx).Cookies(nil)
	if err != nil {
		return fmt.Errorf("read cookies: %w", err)
	}
	out := make([]storedCookie, 0, len(cookies))
	var haveSession bool
	for _, c := range cookies {
		if c.Value == "" {
			continue
		}
		if c.Name == sessionCookieName {
			haveSession = true
		}
		out = append(out, storedCookie{
			Name: c.Name, Value: c.Value, Domain: c.Domain, Path: c.Path,
			Secure: c.Secure, HTTPOnly: c.HTTPOnly,
			SameSite: string(c.SameSite), Expires: float64(c.Expires),
		})
	}
	// Refuse to write a jar with no session in it. Overwriting a good file
	// with an anonymous one would turn a recoverable "sign in again" into a
	// stored session that looks present and never works.
	if !haveSession {
		return ErrLoggedOut
	}

	path, err := sessionPath(alias)
	if err != nil {
		return err
	}
	blob, err := json.Marshal(out)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".cookies.*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(blob); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// RestoreSession injects alias's saved cookies into the browser.
//
// A missing file is not an error: it means this alias has never signed in,
// which Open reports as ErrLoggedOut once it sees where the page lands.
func (b *Browser) RestoreSession(ctx context.Context, alias string) error {
	path, err := sessionPath(alias)
	if err != nil {
		return err
	}
	blob, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var stored []storedCookie
	if err := json.Unmarshal(blob, &stored); err != nil {
		// Treated as "no session" rather than an error on purpose. Launch
		// restores before returning, and `auth login` goes through Launch
		// too — so failing here would fail the very command that exists to
		// replace the unreadable file, leaving no way out short of deleting
		// it by hand. Dropping it lets sign-in proceed and overwrite it.
		fmt.Fprintf(os.Stderr, "warning: ignoring unreadable saved session %s (%v); sign in again to replace it\n", path, err)
		return nil
	}
	params := make([]*proto.NetworkCookieParam, 0, len(stored))
	for _, c := range stored {
		p := &proto.NetworkCookieParam{
			Name: c.Name, Value: c.Value, Domain: c.Domain, Path: c.Path,
			Secure: c.Secure, HTTPOnly: c.HTTPOnly,
		}
		if c.SameSite != "" {
			p.SameSite = proto.NetworkCookieSameSite(c.SameSite)
		}
		if c.Expires > 0 {
			p.Expires = proto.TimeSinceEpoch(c.Expires)
		}
		params = append(params, p)
	}
	if len(params) == 0 {
		return nil
	}
	if err := b.browser.SetCookies(params); err != nil {
		return fmt.Errorf("restore session: %w", err)
	}
	return nil
}

// DeleteSession removes alias's saved cookies. Part of logout: the profile
// directory is not the only place the session lives any more.
func DeleteSession(alias string) error {
	path, err := sessionPath(alias)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
