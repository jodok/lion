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

func (s *store) save() error {
	p, err := credPath()
	if err != nil {
		return err
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	// Write atomically with restrictive permissions.
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
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
	s, err := load()
	if err != nil {
		return err
	}
	s.Accounts[c.Alias] = c
	if s.Default == "" {
		s.Default = c.Alias
	}
	return s.save()
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

// List returns all stored credentials and the default alias.
func List() ([]*Credential, string, error) {
	s, err := load()
	if err != nil {
		return nil, "", err
	}
	out := make([]*Credential, 0, len(s.Accounts))
	for _, c := range s.Accounts {
		out = append(out, c)
	}
	return out, s.Default, nil
}

// Delete removes an account. If it was the default, another (arbitrary)
// account becomes the default.
func Delete(alias string) error {
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
}
