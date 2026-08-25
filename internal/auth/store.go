// Package auth persists LinkedIn session credentials on disk.
//
// Credentials are the browser session cookies required to call the Voyager
// API. GraphQL endpoints want the full linkedin.com cookie jar (li_at,
// JSESSIONID, bcookie, bscookie, lidc, li_gc, ...), not just li_at +
// JSESSIONID (which doubles as the CSRF token) — see DESIGN.md §3.3. They
// are stored in a 0600 JSON file under the lion home directory. Multiple
// accounts are supported via aliases.
package auth

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jodok/lion/internal/config"
)

// ErrNoAccount is returned when a requested account does not exist.
var ErrNoAccount = errors.New("no such account; run `lion auth login`")

// Credential is a single stored LinkedIn session.
type Credential struct {
	Alias      string    `json:"alias"`
	LiAt       string    `json:"li_at"`
	JSessionID string    `json:"jsessionid"`
	MemberID   string    `json:"member_id,omitempty"`
	Name       string    `json:"name,omitempty"`
	SavedAt    time.Time `json:"saved_at"`

	// Cookies is the full linkedin.com browser cookie jar (li_at,
	// JSESSIONID, bcookie, bscookie, lidc, li_gc, ...). GraphQL endpoints
	// reject a bare li_at+JSESSIONID pair (DESIGN.md §3.3), so this is what
	// the CLI passes to voyager.WithCookies / NewChromeTransport. LiAt and
	// JSessionID above are kept in sync with Cookies["li_at"] and
	// Cookies["JSESSIONID"] for back-compat with anything reading those
	// fields directly.
	Cookies map[string]string `json:"cookies,omitempty"`
}

// NormalizeCookies canonicalizes cookie values in place so they match what
// LinkedIn expects on the wire. This is the single boundary where cookie
// values are corrected, regardless of how they were supplied (pasted Cookie
// header, cookies.txt import, individual flags, or legacy credentials).
//
// Currently it only touches JSESSIONID, which LinkedIn wraps in exactly one
// pair of double quotes on the wire; users paste it with zero, one, or
// (rarely) doubled quotes, so we strip every surrounding quote and re-wrap
// in exactly one pair. The csrf-token header is derived from this value by
// stripping the quotes again (see voyager.Client.New), so this stays
// consistent. Structured as a switch on cookie name so future per-cookie
// normalizations slot in.
func NormalizeCookies(cookies map[string]string) {
	for name, value := range cookies {
		switch name {
		case "JSESSIONID":
			if value != "" {
				cookies[name] = `"` + strings.Trim(value, `"`) + `"`
			}
		}
	}
}

// normalize keeps Cookies and the legacy LiAt/JSessionID fields in sync and
// canonicalizes the cookie values. When Cookies is empty (credentials saved
// before this field existed), it is synthesized from LiAt/JSessionID;
// NormalizeCookies then corrects the values, and the legacy fields are
// refreshed from Cookies so both views of the same session agree.
func (c *Credential) normalize() {
	if len(c.Cookies) == 0 {
		c.Cookies = map[string]string{}
		if c.LiAt != "" {
			c.Cookies["li_at"] = c.LiAt
		}
		if c.JSessionID != "" {
			c.Cookies["JSESSIONID"] = c.JSessionID
		}
	}
	NormalizeCookies(c.Cookies)
	if v, ok := c.Cookies["li_at"]; ok {
		c.LiAt = v
	}
	if v, ok := c.Cookies["JSESSIONID"]; ok {
		c.JSessionID = v
	}
}

// store is the on-disk representation.
type store struct {
	Default  string                 `json:"default"`
	Accounts map[string]*Credential `json:"accounts"`
}

func credPath() (string, error) {
	home, err := config.EnsureHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "credentials.json"), nil
}

func load() (*store, error) {
	p, err := credPath()
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return &store{Accounts: map[string]*Credential{}}, nil
	}
	if err != nil {
		return nil, err
	}
	var s store
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("parse credentials: %w", err)
	}
	if s.Accounts == nil {
		s.Accounts = map[string]*Credential{}
	}
	// Synthesize Cookies for credentials saved before it existed.
	for _, c := range s.Accounts {
		c.normalize()
	}
	return &s, nil
}

// save writes the store atomically: marshal, write to a fresh unique temp
// file in the same directory (so the final rename is same-filesystem and
// atomic), then rename over the real path.
//
// The temp file is unique per call (os.CreateTemp's random suffix) rather
// than a fixed "credentials.json.tmp" name for two reasons: (1) a
// pre-existing file at a fixed tmp path could have been left with permissive
// mode by something else and os.WriteFile does not change an existing
// file's mode, so the "restrictive permissions" the old code claimed weren't
// actually guaranteed; (2) two concurrent lion processes writing the same
// fixed tmp path could interleave their writes and rename over a corrupted
// file. A unique name avoids both. os.CreateTemp already creates the file
// 0600, but we chmod explicitly so that guarantee doesn't silently depend on
// the stdlib's current default.
func (s *store) save() error {
	p, err := credPath()
	if err != nil {
		return err
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(p)
	f, err := os.CreateTemp(dir, "credentials.*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	// Best-effort cleanup if we bail before the rename below; a no-op once
	// the rename has succeeded (nothing left at tmp to remove).
	defer os.Remove(tmp)

	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// withLock runs fn while holding a best-effort advisory lock on the
// credential store, serializing concurrent load-modify-save cycles (e.g. two
// `lion auth login`/`logout` invocations racing) so one doesn't clobber the
// other's write with a load taken before the other's save.
//
// This is a plain O_CREATE|O_EXCL lock file rather than flock/syscall locking
// so it stays portable with no cgo/build-tag split. It is advisory only —
// nothing stops a process that doesn't call withLock from writing directly —
// but it closes the common race between well-behaved lion invocations, which
// is the case that actually happens in practice (a user or script running
// two lion commands close together). A lock older than staleLockAge is
// assumed to be left over from a crashed process and is stolen rather than
// wedging the store forever; a process that can't acquire the lock within
// maxWait proceeds without it rather than hanging or failing a command that
// would otherwise succeed — the unique-temp-file fix in save() above is the
// remaining backstop if two writers do overlap.
func withLock(fn func() error) error {
	lp, err := lockPath()
	if err != nil {
		return err
	}
	const (
		retryDelay   = 25 * time.Millisecond
		maxWait      = 5 * time.Second
		staleLockAge = 30 * time.Second
	)
	deadline := time.Now().Add(maxWait)
	for {
		f, err := os.OpenFile(lp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			f.Close()
			break
		}
		if !errors.Is(err, os.ErrExist) {
			return err
		}
		if fi, statErr := os.Stat(lp); statErr == nil && time.Since(fi.ModTime()) > staleLockAge {
			os.Remove(lp) // steal a stale lock left by a crashed process
			continue
		}
		if time.Now().After(deadline) {
			break // proceed unlocked rather than hang or fail outright
		}
		time.Sleep(retryDelay)
	}
	defer os.Remove(lp)
	return fn()
}

func lockPath() (string, error) {
	home, err := config.EnsureHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "credentials.json.lock"), nil
}

// Save stores (or replaces) a credential and, if it is the first account,
// makes it the default.
func Save(c *Credential) error {
	if c.Alias == "" {
		c.Alias = "default"
	}
	if c.SavedAt.IsZero() {
		c.SavedAt = time.Now()
	}
	c.normalize()
	return withLock(func() error {
		s, err := load()
		if err != nil {
			return err
		}
		s.Accounts[c.Alias] = c
		if s.Default == "" {
			s.Default = c.Alias
		}
		return s.save()
	})
}

// Get returns the credential for alias, or the default account when alias is
// empty.
func Get(alias string) (*Credential, error) {
	s, err := load()
	if err != nil {
		return nil, err
	}
	if alias == "" {
		alias = s.Default
	}
	if alias == "" {
		return nil, ErrNoAccount
	}
	c, ok := s.Accounts[alias]
	if !ok {
		return nil, ErrNoAccount
	}
	return c, nil
}

// List returns all stored credentials and the default alias, sorted by
// alias so callers (e.g. `auth status`) get stable, deterministic output
// instead of Go's randomized map iteration order.
func List() ([]*Credential, string, error) {
	s, err := load()
	if err != nil {
		return nil, "", err
	}
	out := make([]*Credential, 0, len(s.Accounts))
	for _, c := range s.Accounts {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Alias < out[j].Alias })
	return out, s.Default, nil
}

// DefaultAlias returns the store's recorded default account alias, or "" if
// no accounts have been saved yet.
func DefaultAlias() (string, error) {
	s, err := load()
	if err != nil {
		return "", err
	}
	return s.Default, nil
}

// Delete removes an account. If it was the default, another (arbitrary)
// account becomes the default.
func Delete(alias string) error {
	return withLock(func() error {
		s, err := load()
		if err != nil {
			return err
		}
		if _, ok := s.Accounts[alias]; !ok {
			return ErrNoAccount
		}
		delete(s.Accounts, alias)
		if s.Default == alias {
			s.Default = ""
			for a := range s.Accounts {
				s.Default = a
				break
			}
		}
		return s.save()
	})
}
