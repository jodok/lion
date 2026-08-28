// Package config resolves configuration from (in order of precedence):
// command-line flags > environment variables > config file > defaults.
//
// The config file is a small optional JSON document at $LION_HOME/config.json
// (override with --config PATH). It only carries the handful of settings
// worth persisting for v1: readonly, account, output format. Everything else
// is flag-only. Recognized environment variables: LION_ACCOUNT, LION_READONLY,
// LION_OUTPUT (json|plain|table).
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/spf13/pflag"
)

// Config holds runtime configuration shared across commands.
type Config struct {
	// Account is the active account alias (for multi-account setups).
	Account string
	// Home is the lion config/state directory.
	Home string
	// ConfigPath is the config file path (--config). Empty means the
	// default, DefaultConfigPath().
	ConfigPath string

	// Output controls.
	JSON          bool
	Plain         bool
	WrapUntrusted bool
	Verbose       bool

	// Safety controls.
	ReadOnly bool
	DryRun   bool
	Yes      bool
	NoInput  bool

	// Max caps result counts for list/search commands (0 = command default).
	Max int

	// Browser routes LinkedIn traffic through a real Chromium instance lion
	// drives, instead of replaying stored cookies over a synthesized HTTP
	// client. Opt-in for now; see internal/browser's package doc.
	Browser bool
	// CookieTransport opts back into the retired cookie path. It is the
	// explicit inverse of Browser, which now defaults on.
	CookieTransport bool
	// Headed shows that browser's window. Sign-in forces it on regardless.
	Headed bool
	// ChromePath overrides the Chromium binary the browser transport uses.
	ChromePath string
}

// Home returns the lion home directory, honoring LION_HOME then XDG/OS
// conventions. It does not create the directory.
func Home() string {
	if h := os.Getenv("LION_HOME"); h != "" {
		return h
	}
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "lion")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".lion"
	}
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(home, "Library", "Application Support", "lion")
	case "windows":
		if appdata := os.Getenv("APPDATA"); appdata != "" {
			return filepath.Join(appdata, "lion")
		}
		return filepath.Join(home, "lion")
	default:
		return filepath.Join(home, ".config", "lion")
	}
}

// EnsureHome creates the home directory (0700) and returns its path. It also
// repairs the permissions of a pre-existing home directory that has looser
// perms (e.g. created by an older lion version, a different umask, or by
// hand) — os.MkdirAll leaves an already-existing directory's mode untouched,
// so the credential store's 0600-file guarantee is only as good as the
// directory it lives in.
func EnsureHome() (string, error) {
	h := Home()
	if err := os.MkdirAll(h, 0o700); err != nil {
		return "", err
	}
	if err := os.Chmod(h, 0o700); err != nil {
		return "", err
	}
	return h, nil
}

// DefaultConfigPath returns the default config file location, $LION_HOME/config.json.
func DefaultConfigPath() string {
	return filepath.Join(Home(), "config.json")
}

// fileConfig is the on-disk config file shape. Fields are pointers/omitempty
// where the zero value is a meaningful setting (readonly) so an absent key
// means "not set" rather than "set to false".
type fileConfig struct {
	Account  string `json:"account,omitempty"`
	ReadOnly *bool  `json:"readonly,omitempty"`
	// Output selects the default render format: "json", "plain", or "table"
	// (or empty/omitted for the built-in default).
	Output string `json:"output,omitempty"`
	// Browser opts into the browser transport from the config file, so an
	// unattended `lion sync` on a timer does not need the flag on every
	// invocation. Pointer so an absent key means "not set" rather than
	// "explicitly false" (see this struct's doc).
	Browser    *bool  `json:"browser,omitempty"`
	ChromePath string `json:"chrome_path,omitempty"`
}

// loadFile reads and parses a config file. A missing file is not an error —
// the config file is entirely optional — but a present-and-malformed one is.
func loadFile(path string) (fileConfig, error) {
	var fc fileConfig
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return fc, nil
	}
	if err != nil {
		return fc, err
	}
	if err := json.Unmarshal(b, &fc); err != nil {
		return fc, fmt.Errorf("parse config %s: %w", path, err)
	}
	return fc, nil
}

// Apply layers environment and config-file settings onto cfg for any setting
// the user did not pass explicitly as a flag, implementing the package's
// documented precedence: flag > env > file > default. It must run after
// cobra has parsed flags into cfg (so cfg already holds any flag values),
// with flags giving Changed() so Apply can tell "flag passed" from "flag
// left at its zero-value default".
func Apply(cfg *Config, flags *pflag.FlagSet) error {
	path := cfg.ConfigPath
	if path == "" {
		path = DefaultConfigPath()
	}
	fc, err := loadFile(path)
	if err != nil {
		return err
	}

	if !flags.Changed("account") {
		if v := os.Getenv("LION_ACCOUNT"); v != "" {
			cfg.Account = v
		} else if fc.Account != "" {
			cfg.Account = fc.Account
		}
	}

	if !flags.Changed("readonly") {
		// LION_READONLY gates every mutation, so it must never fail open. An
		// unset or empty value falls through to the config file; a value we
		// cannot understand is an error rather than a silent false, because
		// silently ignoring `LION_READONLY=yes` would enable writes for
		// someone who believes they are protected.
		if v, ok := os.LookupEnv("LION_READONLY"); ok && v != "" {
			b, err := parseBoolStrict(v)
			if err != nil {
				return fmt.Errorf("LION_READONLY=%q: %w", v, err)
			}
			cfg.ReadOnly = b
		} else if fc.ReadOnly != nil {
			cfg.ReadOnly = *fc.ReadOnly
		}
	}

	if !flags.Changed("browser") {
		// Unlike LION_READONLY this may fail open: the browser transport is
		// a routing choice, not a safety gate, and a value we cannot parse
		// should not stop an otherwise valid command.
		if v, ok := os.LookupEnv("LION_BROWSER"); ok && v != "" {
			if b, err := parseBoolStrict(v); err == nil {
				cfg.Browser = b
			}
		} else if fc.Browser != nil {
			cfg.Browser = *fc.Browser
		}
	}

	if !flags.Changed("chrome-path") && cfg.ChromePath == "" {
		if v := os.Getenv("LION_CHROME_PATH"); v != "" {
			cfg.ChromePath = v
		} else if fc.ChromePath != "" {
			cfg.ChromePath = fc.ChromePath
		}
	}

	if !flags.Changed("json") && !flags.Changed("plain") {
		out := os.Getenv("LION_OUTPUT")
		if out == "" {
			out = fc.Output
		}
		switch out {
		case "json":
			cfg.JSON = true
		case "plain":
			cfg.Plain = true
		}
	}

	return nil
}

// parseBoolStrict parses a boolean environment value, accepting the spellings
// people actually type (strconv's 1/t/true/0/f/false, plus yes/no/on/off, any
// case) and rejecting everything else. Used for safety-gating variables where
// an unrecognized value must be reported rather than quietly treated as off.
func parseBoolStrict(s string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "yes", "y", "on":
		return true, nil
	case "no", "n", "off":
		return false, nil
	}
	b, err := strconv.ParseBool(s)
	if err != nil {
		return false, errors.New("want a boolean (true/false, 1/0, yes/no, on/off)")
	}
	return b, nil
}
