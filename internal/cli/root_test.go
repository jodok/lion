package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
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
