package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/pflag"
)

// newTestFlags returns a FlagSet mirroring the global flags Apply cares
// about, so tests can control Changed() the same way cobra would after
// parsing real command-line args.
func newTestFlags() (*pflag.FlagSet, *Config) {
	cfg := &Config{}
	fs := pflag.NewFlagSet("test", pflag.ContinueOnError)
	fs.StringVar(&cfg.Account, "account", "", "")
	fs.BoolVar(&cfg.ReadOnly, "readonly", false, "")
	fs.BoolVar(&cfg.JSON, "json", false, "")
	fs.BoolVar(&cfg.Plain, "plain", false, "")
	return fs, cfg
}

func writeConfigFile(t *testing.T, dir string, contents string) string {
	t.Helper()
	p := filepath.Join(dir, "config.json")
	if err := os.WriteFile(p, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestApplyDefaultsWhenNothingSet(t *testing.T) {
	fs, cfg := newTestFlags()
	if err := fs.Parse(nil); err != nil {
		t.Fatal(err)
	}
	cfg.ConfigPath = filepath.Join(t.TempDir(), "does-not-exist.json")
	if err := Apply(cfg, fs); err != nil {
		t.Fatal(err)
	}
	if cfg.Account != "" || cfg.ReadOnly || cfg.JSON || cfg.Plain {
		t.Errorf("Apply with nothing set changed cfg: %+v", cfg)
	}
}

func TestApplyFileSettings(t *testing.T) {
	fs, cfg := newTestFlags()
	if err := fs.Parse(nil); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	cfg.ConfigPath = writeConfigFile(t, dir, `{"account":"work","readonly":true,"output":"json"}`)
	if err := Apply(cfg, fs); err != nil {
		t.Fatal(err)
	}
	if cfg.Account != "work" {
		t.Errorf("Account = %q, want work", cfg.Account)
	}
	if !cfg.ReadOnly {
		t.Error("ReadOnly = false, want true from file")
	}
	if !cfg.JSON || cfg.Plain {
		t.Errorf("JSON/Plain = %v/%v, want json from file", cfg.JSON, cfg.Plain)
	}
}

func TestApplyEnvOverridesFile(t *testing.T) {
	fs, cfg := newTestFlags()
	if err := fs.Parse(nil); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	cfg.ConfigPath = writeConfigFile(t, dir, `{"account":"from-file","readonly":false,"output":"plain"}`)
	t.Setenv("LION_ACCOUNT", "from-env")
	t.Setenv("LION_READONLY", "true")
	t.Setenv("LION_OUTPUT", "json")
	if err := Apply(cfg, fs); err != nil {
		t.Fatal(err)
	}
	if cfg.Account != "from-env" {
		t.Errorf("Account = %q, want from-env (env beats file)", cfg.Account)
	}
	if !cfg.ReadOnly {
		t.Error("ReadOnly = false, want true from env (env beats file)")
	}
	if !cfg.JSON || cfg.Plain {
		t.Errorf("JSON/Plain = %v/%v, want json from env", cfg.JSON, cfg.Plain)
	}
}

func TestApplyFlagOverridesEnvAndFile(t *testing.T) {
	fs, cfg := newTestFlags()
	if err := fs.Parse([]string{"--account=from-flag", "--readonly=false", "--plain"}); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	cfg.ConfigPath = writeConfigFile(t, dir, `{"account":"from-file","readonly":true,"output":"json"}`)
	t.Setenv("LION_ACCOUNT", "from-env")
	t.Setenv("LION_READONLY", "true")
	t.Setenv("LION_OUTPUT", "json")
	if err := Apply(cfg, fs); err != nil {
		t.Fatal(err)
	}
	if cfg.Account != "from-flag" {
		t.Errorf("Account = %q, want from-flag (flag beats env/file)", cfg.Account)
	}
	if cfg.ReadOnly {
		t.Error("ReadOnly = true, want false — explicit flag must beat env/file")
	}
	if cfg.JSON || !cfg.Plain {
		t.Errorf("JSON/Plain = %v/%v, want plain (explicit flag beats env/file json)", cfg.JSON, cfg.Plain)
	}
}

func TestApplyMissingFileIsNotAnError(t *testing.T) {
	fs, cfg := newTestFlags()
	if err := fs.Parse(nil); err != nil {
		t.Fatal(err)
	}
	cfg.ConfigPath = filepath.Join(t.TempDir(), "nope.json")
	if err := Apply(cfg, fs); err != nil {
		t.Fatalf("missing config file should not error: %v", err)
	}
}

func TestApplyMalformedFileErrors(t *testing.T) {
	fs, cfg := newTestFlags()
	if err := fs.Parse(nil); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	cfg.ConfigPath = writeConfigFile(t, dir, `{not json`)
	if err := Apply(cfg, fs); err == nil {
		t.Fatal("malformed config file should error")
	}
}

// TestEnsureHomeRepairsLoosePermissions is the F24 regression test: a
// pre-existing home directory with permissive perms (e.g. left over from an
// older lion version) must be tightened back to 0700, since credentials.json
// (0600) is only as private as the directory containing it.
func TestEnsureHomeRepairsLoosePermissions(t *testing.T) {
	dir := t.TempDir()
	home := filepath.Join(dir, "lion-home")
	if err := os.MkdirAll(home, 0o777); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LION_HOME", home)
	got, err := EnsureHome()
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(got)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o700 {
		t.Errorf("home dir perm = %o, want 0700 (should have been repaired)", perm)
	}
}
