// Package auth persists LinkedIn session credentials on disk.
//
// Credentials are the browser session cookies required to call the Voyager
// API: li_at (the auth session) and JSESSIONID (which doubles as the CSRF
// token). They are stored in a 0600 JSON file under the lion home directory.
// Multiple accounts are supported via aliases.
package auth

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
