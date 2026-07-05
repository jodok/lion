// Package config resolves configuration from (in order of precedence):
// command-line flags > environment variables > config file > defaults.
package config

import (
	"os"
	"path/filepath"
	"runtime"
)

// Config holds runtime configuration shared across commands.
type Config struct {
	// Account is the active account alias (for multi-account setups).
	Account string
	// Home is the lion config/state directory.
	Home string

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

// EnsureHome creates the home directory (0700) and returns its path.
func EnsureHome() (string, error) {
	h := Home()
	if err := os.MkdirAll(h, 0o700); err != nil {
		return "", err
	}
	return h, nil
}
