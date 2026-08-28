package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jodok/lion/internal/config"
	"github.com/jodok/lion/internal/store"
)

// statusOf finds one check's verdict by name.
func statusOf(checks []check, name string) (check, bool) {
	for _, c := range checks {
		if c.Name == name {
			return c, true
		}
	}
	return check{}, false
}

// TestDoctorReportsAFreshSetup: on a home with nothing in it, doctor must
// name the two things that are actually missing rather than reporting a
// generic failure — the point of the command is saying which layer broke.
func TestDoctorReportsAFreshSetup(t *testing.T) {
	isolateHome(t)
	cfg := &config.Config{Browser: true}
	app := &App{Cfg: cfg}

	checks := runDoctor(context.Background(), app, "", true)

	if c, ok := statusOf(checks, "home"); !ok || c.Status != statusOK {
		t.Errorf("home = %+v, want ok", c)
	}
	if c, ok := statusOf(checks, "store"); !ok || c.Status != statusWarn {
		t.Errorf("store = %+v, want a warning naming the missing archive", c)
	} else if !strings.Contains(c.Detail, "lion sync") {
		t.Errorf("store detail = %q, want it to name the command that fixes it", c.Detail)
	}
	// --offline must not silently pass the check it skipped.
	if c, ok := statusOf(checks, "session"); !ok || c.Status == statusOK {
		t.Errorf("session = %+v, want it reported as skipped rather than ok", c)
	}
}

// TestDoctorDoesNotCreateAStore: a diagnostic that creates the database it is
// reporting on would make "no archive yet" impossible to observe twice, and
// would write to disk on a read-only check.
func TestDoctorDoesNotCreateAStore(t *testing.T) {
	isolateHome(t)
	app := &App{Cfg: &config.Config{Browser: true}}

	runDoctor(context.Background(), app, "", true)

	path := filepath.Join(config.Home(), "store.db")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("doctor created %s; a read-only check must not write a database", path)
	}
}

// TestDoctorFlagsTheCookieTransport: the transport in use is the setting most
// likely to explain surprising behaviour, and the deprecated one has to say
// so rather than reporting a clean bill of health.
func TestDoctorFlagsTheCookieTransport(t *testing.T) {
	isolateHome(t)
	app := &App{Cfg: &config.Config{Browser: false}}

	checks := runDoctor(context.Background(), app, "", true)

	c, ok := statusOf(checks, "transport")
	if !ok {
		t.Fatal("no transport check")
	}
	if c.Status != statusWarn {
		t.Errorf("transport = %+v, want a warning for the deprecated path", c)
	}
	if !strings.Contains(c.Detail, "revoke") {
		t.Errorf("transport detail = %q, want it to say what the risk is", c.Detail)
	}
	// The browser check must not fail just because a cookie-transport setup
	// has no Chromium: it isn't used.
	if b, ok := statusOf(checks, "browser"); !ok || b.Status == statusFail {
		t.Errorf("browser = %+v, want it not to fail when the transport doesn't use one", b)
	}
}

// TestDoctorFailsOnAMissingBrowser: a browser lion cannot start is a failure,
// not a warning — every command would break on it.
func TestDoctorFailsOnAMissingBrowser(t *testing.T) {
	isolateHome(t)
	app := &App{Cfg: &config.Config{Browser: true, ChromePath: filepath.Join(t.TempDir(), "nope")}}

	checks := runDoctor(context.Background(), app, "", true)

	c, ok := statusOf(checks, "browser")
	if !ok {
		t.Fatal("no browser check")
	}
	if c.Status != statusFail {
		t.Errorf("browser = %+v, want fail for a Chromium that isn't there", c)
	}
}

// TestDoctorExitCodeGatesOnFailures: a timer or script runs doctor to decide
// whether the setup is healthy, so a failing check has to be visible in the
// exit status and a warning must not be.
func TestDoctorExitCodeGatesOnFailures(t *testing.T) {
	if got := exitCode(errDoctorFailed); got != ExitError {
		t.Errorf("exitCode(errDoctorFailed) = %d, want %d", got, ExitError)
	}

	isolateHome(t)
	// A fresh home warns (no archive) but nothing fails, once the network
	// check is skipped — that must still be exit 0.
	app := &App{Cfg: &config.Config{Browser: true}}
	for _, c := range runDoctor(context.Background(), app, "", true) {
		if c.Status == statusFail {
			t.Fatalf("unexpected failure on a fresh home: %+v", c)
		}
	}
}

// TestDoctorHonoursStoreFlag: every other store-touching command takes
// --store, and a diagnostic that ignores it would tell anyone using a
// non-default database that they have no archive — confidently, and wrongly.
func TestDoctorHonoursStoreFlag(t *testing.T) {
	isolateHome(t)
	app := &App{Cfg: &config.Config{Browser: true}}

	// A store somewhere other than the default location.
	custom := filepath.Join(t.TempDir(), "elsewhere.db")
	st, err := store.Open(custom)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.WithTx(context.Background(), func(tx *store.Tx) error {
		return tx.UpsertConversation(context.Background(),
			store.Conversation{ID: "c1", URN: "urn:c1", UpdatedAt: 1}, 1)
	}); err != nil {
		t.Fatal(err)
	}
	st.Close()

	// Without the flag, doctor looks at the default path and finds nothing.
	def, _ := statusOf(runDoctor(context.Background(), app, "", true), "store")
	if def.Status != statusWarn {
		t.Errorf("default path = %+v, want a warning (nothing there)", def)
	}

	// With it, it must report the archive that actually exists.
	got, ok := statusOf(runDoctor(context.Background(), app, custom, true), "store")
	if !ok {
		t.Fatal("no store check")
	}
	if got.Status != statusOK {
		t.Errorf("store = %+v, want ok for a populated --store database", got)
	}
	if !strings.Contains(got.Detail, "1 conversations") {
		t.Errorf("store detail = %q, want it to describe the --store database", got.Detail)
	}
}
