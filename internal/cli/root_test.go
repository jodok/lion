package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jodok/lion/internal/auth"
	"github.com/jodok/lion/internal/config"
	"github.com/jodok/lion/internal/ratelimit"
	"github.com/jodok/lion/internal/voyager"
)

// isolateHome points LION_HOME at a fresh temp directory so tests never read
// or write a real credentials.json/config.json.
func isolateHome(t *testing.T) {
	t.Helper()
	t.Setenv("LION_HOME", t.TempDir())
}

// execRoot isolates LION_HOME, builds the real command tree, and runs it
// with args, discarding stdout/stderr, returning the error ExecuteContext
// produced (nil on success). This exercises the full
// flag-parsing/arg-validation/dispatch path exactly as Execute() does,
// without needing a subprocess. Tests that need to seed state (e.g. a saved
// credential) before running should call isolateHome themselves and use
// runRoot directly instead, so the seeded state lands in the same LION_HOME
// the command runs against.
func execRoot(t *testing.T, args ...string) error {
	t.Helper()
	isolateHome(t)
	return runRoot(t, args...)
}

// runRoot runs the command tree with args against whatever LION_HOME is
// already set to (the caller isolates it), discarding cobra's own
// stdout/stderr (help/usage text), and returns the resulting error. Command
// output that goes through App.Renderer() writes to the real os.Stdout, not
// cmd.OutOrStdout(), so use captureStdout around this to inspect it.
func runRoot(t *testing.T, args ...string) error {
	t.Helper()
	cfg := &config.Config{}
	root, app := newRootCmd(cfg)
	root.SetArgs(args)
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	ctx := context.WithValue(context.Background(), ctxKey{}, app)
	return root.ExecuteContext(ctx)
}

// captureStdout redirects os.Stdout for the duration of fn and returns what
// was written, restoring the original afterward. Needed because
// App.Renderer() writes straight to os.Stdout (by design — stdout is data,
// per DESIGN.md §2.3), not through cobra's cmd.OutOrStdout().
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stdout
	os.Stdout = w
	fn()
	os.Stdout = orig
	w.Close()
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// saveFakeAccount stores a syntactically-valid but fake credential under
// the currently-isolated LION_HOME, so app.Client() can build a real
// (offline-safe) voyager.Client — NewChromeTransport only constructs a TLS
// client locally and never hits the network by itself, so this is safe to
// use even for non-dry-run tests as long as the test path stops short of
// actually issuing a request (e.g. a declined confirmation, or a dry-run
// mutation, which voyager.Client.post() short-circuits before any network
// call).
func saveFakeAccount(t *testing.T) {
	t.Helper()
	if err := auth.Save(&auth.Credential{
		Alias:   "default",
		Cookies: map[string]string{"li_at": "test-li-at", "JSESSIONID": `"test-jsession"`},
	}); err != nil {
		t.Fatal(err)
	}
}

// TestExtraPositionalArgsIsUsageError is the F20 regression test: a command
// invoked with more args than it accepts (cobra's own arg-count validator)
// must map to exit 2, not the generic exit-1 bucket.
func TestExtraPositionalArgsIsUsageError(t *testing.T) {
	err := execRoot(t, "feed", "read", "extra")
	if err == nil {
		t.Fatal("expected an error for an unexpected extra argument")
	}
	if got := exitCode(err); got != ExitUsage {
		t.Errorf("exitCode(%v) = %d, want ExitUsage (%d)", err, got, ExitUsage)
	}
}

// TestUnknownFlagIsUsageError is the other half of the F20 regression test.
func TestUnknownFlagIsUsageError(t *testing.T) {
	err := execRoot(t, "feed", "read", "--this-flag-does-not-exist")
	if err == nil {
		t.Fatal("expected an error for an unknown flag")
	}
	if got := exitCode(err); got != ExitUsage {
		t.Errorf("exitCode(%v) = %d, want ExitUsage (%d)", err, got, ExitUsage)
	}
}

// TestUnknownCommandIsUsageError covers cobra's command-resolution failure
// path (isCobraUsageError), the one case not caught by SetFlagErrorFunc or
// usageArgs.
func TestUnknownCommandIsUsageError(t *testing.T) {
	err := execRoot(t, "not-a-real-command")
	if err == nil {
		t.Fatal("expected an error for an unknown command")
	}
	if got := exitCode(err); got != ExitUsage {
		t.Errorf("exitCode(%v) = %d, want ExitUsage (%d)", err, got, ExitUsage)
	}
}

// TestJSONAndPlainTogetherIsUsageError pins that our own usage checks still
// work alongside the new cobra-error classification.
func TestJSONAndPlainTogetherIsUsageError(t *testing.T) {
	err := execRoot(t, "version", "--json", "--plain")
	if err == nil {
		t.Fatal("expected an error for --json and --plain together")
	}
	if got := exitCode(err); got != ExitUsage {
		t.Errorf("exitCode(%v) = %d, want ExitUsage (%d)", err, got, ExitUsage)
	}
}

// TestExitCodeDailyBudget is the F21 regression test: a local rate-limit
// budget exhaustion must map to exit 4, the same as a live 429 from
// LinkedIn, not fall through to the generic exit-1 bucket.
func TestExitCodeDailyBudget(t *testing.T) {
	if got := exitCode(ratelimit.ErrDailyBudget); got != ExitRateLimited {
		t.Errorf("exitCode(ratelimit.ErrDailyBudget) = %d, want ExitRateLimited (%d)", got, ExitRateLimited)
	}
	// Also via errors.Is through a wrapped error, matching how it will
	// actually surface from deep inside the ratelimit/voyager call chain.
	wrapped := fmt.Errorf("invite: %w", ratelimit.ErrDailyBudget)
	if got := exitCode(wrapped); got != ExitRateLimited {
		t.Errorf("exitCode(wrapped ErrDailyBudget) = %d, want ExitRateLimited (%d)", got, ExitRateLimited)
	}
	if got := exitCode(voyager.ErrRateLimited); got != ExitRateLimited {
		t.Errorf("exitCode(voyager.ErrRateLimited) = %d, want ExitRateLimited (%d) (must still work)", got, ExitRateLimited)
	}
}

// TestConfirmYesSkipsPrompt covers F15: --yes must skip the prompt/stdin
// read entirely and proceed.
func TestConfirmYesSkipsPrompt(t *testing.T) {
	app := &App{Cfg: &config.Config{Yes: true}}
	ok, err := app.confirm("proceed?")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Error("confirm() with --yes = false, want true")
	}
}

// TestConfirmNoInputIsNotConsent pins the distinction between "don't ask me"
// and "yes". --no-input suppressing the prompt must not authorize the write,
// or any non-interactive script would mutate real people's inboxes without
// anything on the command line approving it.
func TestConfirmNoInputIsNotConsent(t *testing.T) {
	app := &App{Cfg: &config.Config{NoInput: true}}
	ok, err := app.confirm("proceed?")
	if ok {
		t.Error("confirm() with --no-input = true, want false: --no-input is not consent")
	}
	var ue usageError
	if !errors.As(err, &ue) {
		t.Errorf("confirm() with --no-input err = %v, want a usageError naming --yes", err)
	}
}

// TestConfirmNonTTYWithoutYesErrors covers F15's non-interactive-stdin case:
// without --yes/--no-input and without a TTY, confirm must refuse rather
// than guess, with a clear (usage) error.
func TestConfirmNonTTYWithoutYesErrors(t *testing.T) {
	restore := forceInteractive(false)
	defer restore()
	app := &App{Cfg: &config.Config{}}
	ok, err := app.confirm("proceed?")
	if ok {
		t.Error("confirm() on non-TTY without --yes should not proceed")
	}
	if err == nil {
		t.Fatal("expected an error")
	}
	if exitCode(err) != ExitUsage {
		t.Errorf("exitCode(%v) = %d, want ExitUsage (%d)", err, exitCode(err), ExitUsage)
	}
}

// TestConfirmDeclineAborts covers F15's "n" path: a TTY that answers
// anything but y/yes must abort without error (exit 0, no mutation is the
// caller's responsibility once ok=false).
func TestConfirmDeclineAborts(t *testing.T) {
	restore := forceInteractive(true)
	defer restore()
	withStdin(t, "n\n")
	app := &App{Cfg: &config.Config{}}
	ok, err := app.confirm("proceed?")
	if err != nil {
		t.Fatalf("decline should not be an error, got %v", err)
	}
	if ok {
		t.Error("confirm() with 'n' answer = true, want false")
	}
}

// TestConfirmAcceptProceeds is the positive case alongside
// TestConfirmDeclineAborts.
func TestConfirmAcceptProceeds(t *testing.T) {
	restore := forceInteractive(true)
	defer restore()
	withStdin(t, "y\n")
	app := &App{Cfg: &config.Config{}}
	ok, err := app.confirm("proceed?")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Error("confirm() with 'y' answer = false, want true")
	}
}

// forceInteractive overrides isInteractive for the duration of a test and
// returns a func to restore it.
func forceInteractive(v bool) func() {
	orig := isInteractive
	isInteractive = func() bool { return v }
	return func() { isInteractive = orig }
}

// withStdin temporarily replaces os.Stdin with a pipe pre-loaded with
// content, restoring the original at test cleanup.
func withStdin(t *testing.T, content string) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.WriteString(content); err != nil {
		t.Fatal(err)
	}
	w.Close()
	orig := os.Stdin
	os.Stdin = r
	t.Cleanup(func() {
		os.Stdin = orig
		r.Close()
	})
}

// TestIsTerminalRejectsDevNull pins the /dev/null case that os.ModeCharDevice
// got wrong: cron jobs and daemons run with stdin on /dev/null, which IS a
// character device. Treating it as a TTY meant the prompt went to a stream
// nobody reads, the scanner hit EOF, and the command exited 0 having mutated
// nothing while automation recorded a success.
func TestIsTerminalRejectsDevNull(t *testing.T) {
	f, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if isTerminal(f) {
		t.Error("isTerminal(/dev/null) = true, want false: a cron stdin is not a terminal")
	}
}

// TestIsTerminalRejectsPipe covers the ordinary redirected-stdin case.
func TestIsTerminalRejectsPipe(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	if isTerminal(r) {
		t.Error("isTerminal(pipe) = true, want false")
	}
}

// rotatingTransport is a fixture voyager.Transport that also implements
// voyager.CookieSnapshotter, standing in for the real Chrome-TLS transport's
// live jar (which only rotates cookies via a real network round-trip) so
// cookie writeback can be tested without hitting the network.
type rotatingTransport struct {
	snapshot map[string]string
}

func (r *rotatingTransport) Do(_ context.Context, _ *voyager.Request) (*voyager.Response, error) {
	return &voyager.Response{StatusCode: 200, Body: []byte(`{}`)}, nil
}

func (r *rotatingTransport) Snapshot() map[string]string { return r.snapshot }

// TestPersistRotatedCookiesWritesBackToStore is the cookie-writeback
// regression test (DESIGN.md §3.3): after a command's client(s) ran,
// persistRotatedCookies must save whatever the transport's jar rotated in,
// so the next invocation starts from a fresh session instead of the stale
// snapshot `auth login` stored — the bug that made a session stop working
// within minutes of a successful login.
func TestPersistRotatedCookiesWritesBackToStore(t *testing.T) {
	isolateHome(t)
	saveFakeAccount(t)
	// A live jar always carries li_at — it was seeded with it and it hasn't
	// expired — so the snapshot is the full session, not just the deltas.
	cl := voyager.New("test-li-at", `"test-jsession"`,
		voyager.WithTransport(&rotatingTransport{snapshot: map[string]string{
			"li_at":      "test-li-at",
			"JSESSIONID": `"rotated:9"`,
			"lidc":       "dc-3",
		}}))
	app := &App{Cfg: &config.Config{}, clients: []*voyager.Client{cl}, clientAlias: "default"}
	app.persistRotatedCookies()

	got, err := auth.Get("default")
	if err != nil {
		t.Fatal(err)
	}
	if got.Cookies["JSESSIONID"] != `"rotated:9"` {
		t.Errorf("Cookies[JSESSIONID] = %q, want quoted rotated:9", got.Cookies["JSESSIONID"])
	}
	if got.Cookies["lidc"] != "dc-3" {
		t.Errorf("Cookies[lidc] = %q, want dc-3", got.Cookies["lidc"])
	}
	if got.Cookies["li_at"] != "test-li-at" {
		t.Errorf("Cookies[li_at] = %q, want test-li-at", got.Cookies["li_at"])
	}
}

// TestPersistRotatedCookiesRefusesSessionlessJar pins the safety guard at the
// CLI level: if the jar somehow comes back without li_at, nothing is written
// and the stored credential survives for the user to retry with.
func TestPersistRotatedCookiesRefusesSessionlessJar(t *testing.T) {
	isolateHome(t)
	saveFakeAccount(t)
	cl := voyager.New("test-li-at", `"test-jsession"`,
		voyager.WithTransport(&rotatingTransport{snapshot: map[string]string{"lidc": "dc-3"}}))
	app := &App{Cfg: &config.Config{}, clients: []*voyager.Client{cl}, clientAlias: "default"}
	app.persistRotatedCookies()

	got, err := auth.Get("default")
	if err != nil {
		t.Fatal(err)
	}
	if got.Cookies["li_at"] != "test-li-at" {
		t.Errorf("Cookies[li_at] = %q, want the stored credential untouched", got.Cookies["li_at"])
	}
	if _, ok := got.Cookies["lidc"]; ok {
		t.Error("a session-less jar was written to the store")
	}
}

// TestPersistRotatedCookiesNoClientsIsNoop covers the early return: a
// command that never called App.Client() (e.g. `lion version`) must not
// touch the credential store at all.
func TestPersistRotatedCookiesNoClientsIsNoop(t *testing.T) {
	isolateHome(t)
	saveFakeAccount(t)
	before, err := auth.Get("default")
	if err != nil {
		t.Fatal(err)
	}
	app := &App{Cfg: &config.Config{}}
	app.persistRotatedCookies() // no clients built; must be a no-op
	after, err := auth.Get("default")
	if err != nil {
		t.Fatal(err)
	}
	if !after.SavedAt.Equal(before.SavedAt) {
		t.Error("persistRotatedCookies() with no clients modified the stored credential")
	}
}

// TestPersistRotatedCookiesWritebackFailureIsNonFatal is the "must never
// break a command" requirement: a store the writeback can't touch (here, a
// directory sitting where credentials.json should be, forcing auth.load()
// to fail) must not surface an error or panic. persistRotatedCookies has no
// return value specifically so a broken writeback can never fail an
// otherwise-successful command; the failure is only ever visible on stderr,
// and only under --verbose.
func TestPersistRotatedCookiesWritebackFailureIsNonFatal(t *testing.T) {
	isolateHome(t)
	saveFakeAccount(t)
	home := os.Getenv("LION_HOME")
	// Replace credentials.json with a directory so the next load() errors.
	if err := os.Remove(filepath.Join(home, "credentials.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(home, "credentials.json"), 0o700); err != nil {
		t.Fatal(err)
	}

	cl := voyager.New("test-li-at", `"test-jsession"`,
		voyager.WithTransport(&rotatingTransport{snapshot: map[string]string{"JSESSIONID": `"rotated:9"`}}))
	app := &App{Cfg: &config.Config{}, clients: []*voyager.Client{cl}, clientAlias: "default"}

	out := captureStderr(t, func() {
		app.persistRotatedCookies() // must not panic or otherwise fail
	})
	if out != "" {
		t.Errorf("persistRotatedCookies() without --verbose wrote to stderr: %q", out)
	}

	app.Cfg.Verbose = true
	out = captureStderr(t, func() {
		app.persistRotatedCookies()
	})
	if out == "" {
		t.Error("persistRotatedCookies() with --verbose should report the failure on stderr")
	}
}

// TestClientRecordsResolvedAlias pins the resolved-alias requirement: when
// --account is empty, auth.Get("") resolves to the store's default account,
// so App.Client() must record that resolved alias (off the returned
// credential) rather than the empty a.Cfg.Account — otherwise
// persistRotatedCookies would try to save under alias "" instead of the
// account that was actually authenticated.
func TestClientRecordsResolvedAlias(t *testing.T) {
	isolateHome(t)
	saveFakeAccount(t) // saved under alias "default", which becomes the store default
	app := &App{Cfg: &config.Config{}}
	if _, err := app.Client(); err != nil {
		t.Fatal(err)
	}
	if app.clientAlias != "default" {
		t.Errorf("clientAlias = %q, want the resolved alias %q", app.clientAlias, "default")
	}
	if len(app.clients) != 1 {
		t.Errorf("len(clients) = %d, want 1", len(app.clients))
	}
}

// TestExitCodeBudgetLockAndPersist keeps the limiter's fail-closed refusals on
// the rate-limit exit code. Both mean "the action was not performed because it
// could not be accounted for", which a caller should treat like any other
// budget stop rather than an unrelated generic failure.
func TestExitCodeBudgetLockAndPersist(t *testing.T) {
	for _, err := range []error{ratelimit.ErrBudgetLock, ratelimit.ErrBudgetPersist} {
		if got := exitCode(err); got != ExitRateLimited {
			t.Errorf("exitCode(%v) = %d, want ExitRateLimited (%d)", err, got, ExitRateLimited)
		}
		wrapped := fmt.Errorf("invite: %w", err)
		if got := exitCode(wrapped); got != ExitRateLimited {
			t.Errorf("exitCode(wrapped %v) = %d, want ExitRateLimited (%d)", err, got, ExitRateLimited)
		}
	}
}

// TestBrowserIsTheDefaultTransport pins the flip. The cookie transport gets a
// session revoked account-wide within minutes (see internal/browser), so it
// cannot be what an unqualified `lion` command reaches for.
func TestBrowserIsTheDefaultTransport(t *testing.T) {
	isolateHome(t)
	cfg := &config.Config{}
	root, _ := newRootCmd(cfg)
	root.SetArgs([]string{"version"})
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	if err := root.ExecuteContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !cfg.Browser {
		t.Error("Browser = false with no flags, want true (the browser transport is the default)")
	}
}

// TestCookieTransportOptsOut: --cookie-transport is the explicit escape
// hatch, and must win over the default without the person having to know
// which setting takes precedence.
func TestCookieTransportOptsOut(t *testing.T) {
	isolateHome(t)
	cfg := &config.Config{}
	root, _ := newRootCmd(cfg)
	root.SetArgs([]string{"--cookie-transport", "version"})
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	if err := root.ExecuteContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if cfg.Browser {
		t.Error("Browser = true with --cookie-transport, want false")
	}
}

// TestSuppliedCookiesSelectTheCookiePath: `pbpaste | lion auth login
// --cookies-stdin` must keep working without also naming --cookie-transport.
// Otherwise the browser default would open a window and ignore a jar the
// person deliberately piped in.
//
// The tell is which error comes back: the cookie path rejects an incomplete
// jar as a usage error, where the browser path would have tried to launch
// Chromium and reported a session problem instead.
func TestSuppliedCookiesSelectTheCookiePath(t *testing.T) {
	isolateHome(t)
	cfg := &config.Config{}
	root, app := newRootCmd(cfg)
	root.SetArgs([]string{"auth", "login", "--cookies-stdin", "--no-input"})
	root.SetIn(strings.NewReader("li_at=only-this-one\n"))
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	ctx := context.WithValue(context.Background(), ctxKey{}, app)

	err := root.ExecuteContext(ctx)
	if err == nil {
		t.Fatal("expected an error for a jar missing JSESSIONID")
	}
	if !strings.Contains(err.Error(), "JSESSIONID") {
		t.Errorf("error = %q, want the cookie path's complaint about the missing cookie "+
			"(a browser-path error would mean the piped jar was ignored)", err)
	}
	if exitCode(err) != ExitUsage {
		t.Errorf("exit code = %d, want %d (usage)", exitCode(err), ExitUsage)
	}
}

// TestReadOnlyAliasMatchesWacli: wacli spells this --read-only, and lion
// --readonly. Both are accepted so anyone driving the two tools does not have
// to remember which is which; aliasing rather than renaming keeps existing
// lion scripts working.
func TestReadOnlyAliasMatchesWacli(t *testing.T) {
	for _, spelling := range []string{"--readonly", "--read-only"} {
		isolateHome(t)
		cfg := &config.Config{}
		root, _ := newRootCmd(cfg)
		root.SetArgs([]string{spelling, "version"})
		root.SetOut(io.Discard)
		root.SetErr(io.Discard)
		if err := root.ExecuteContext(context.Background()); err != nil {
			t.Fatalf("%s: %v", spelling, err)
		}
		if !cfg.ReadOnly {
			t.Errorf("%s did not set ReadOnly", spelling)
		}
	}
}
