// Package cli builds lion's command tree. Commands are thin: they parse flags,
// call into the voyager client, and render results via the output package.
package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/jodok/lion/internal/auth"
	"github.com/jodok/lion/internal/config"
	"github.com/jodok/lion/internal/output"
	"github.com/jodok/lion/internal/ratelimit"
	"github.com/jodok/lion/internal/voyager"
	"github.com/spf13/cobra"
)

// Exit codes — stable contract, documented in DESIGN.md and `lion schema`.
const (
	ExitOK          = 0
	ExitError       = 1
	ExitUsage       = 2
	ExitAuth        = 3
	ExitRateLimited = 4
	ExitNotFound    = 5
	ExitPermission  = 6
	ExitChallenge   = 7
)

// App holds shared state resolved from global flags, threaded to subcommands
// through cobra's context.
type App struct {
	Cfg *config.Config
}

type ctxKey struct{}

func appFrom(cmd *cobra.Command) *App {
	return cmd.Context().Value(ctxKey{}).(*App)
}

// commandFactories holds constructors for feature-vertical commands. Verticals
// register themselves from an init() in their own file so new verticals never
// have to edit this file — keeping parallel work conflict-free.
var commandFactories []func() *cobra.Command

// registerCommand adds a top-level command constructor. Call from an init().
func registerCommand(f func() *cobra.Command) {
	commandFactories = append(commandFactories, f)
}

// Renderer builds an output.Renderer from the resolved config.
func (a *App) Renderer() *output.Renderer {
	f := output.FormatTable
	switch {
	case a.Cfg.JSON:
		f = output.FormatJSON
	case a.Cfg.Plain:
		f = output.FormatPlain
	}
	return output.New(os.Stdout, f, a.Cfg.WrapUntrusted)
}

// Client constructs an authenticated voyager client for the active account,
// wired to the Chrome-impersonating transport (DESIGN.md §3.3 — stdlib
// net/http's TLS fingerprint gets blocked by LinkedIn's Cloudflare bot
// management), and applying dry-run from global flags. It returns
// ErrAuth-mappable errors.
func (a *App) Client(opts ...voyager.Option) (*voyager.Client, error) {
	cred, err := auth.Get(a.Cfg.Account)
	if err != nil {
		return nil, err
	}
	// cred.Cookies is normalized on load, so this fallback only matters for
	// callers that build a Credential by hand rather than through auth.Get.
	cookies := cred.Cookies
	if len(cookies) == 0 {
		cookies = map[string]string{"li_at": cred.LiAt, "JSESSIONID": cred.JSessionID}
	}
	chromeT, err := voyager.NewChromeTransport(cookies)
	if err != nil {
		return nil, err
	}
	base := []voyager.Option{
		voyager.WithCookies(cookies),
		voyager.WithTransport(chromeT),
		voyager.WithDryRun(a.Cfg.DryRun),
	}
	return voyager.New(cred.LiAt, cred.JSessionID, append(base, opts...)...), nil
}

// requireWritable blocks a mutating command under --readonly.
func (a *App) requireWritable() error {
	if a.Cfg.ReadOnly {
		return fmt.Errorf("blocked by --readonly")
	}
	return nil
}

// confirm prompts on stderr before a live (non-dry-run, non-readonly)
// mutation and reads the answer from stdin (DESIGN.md §2.2 — writes need
// confirmation). It returns proceed=true only when the answer is
// "y"/"yes", or when --yes/--no-input is set (which skip the prompt
// entirely).
//
// Declining the prompt is deliberately not an error: callers should print
// their own "aborted" note to stderr and return nil so the command exits 0
// without having mutated anything (F15 — an abort is a normal, successful
// outcome, not a failure).
//
// Running without --yes on a non-interactive stdin IS an error (a usageError,
// exit 2): DESIGN.md requires explicit confirmation for writes, and a non-TTY
// stdin can't supply one, so there is nothing safe to assume — silently
// proceeding would be the one thing this check exists to prevent, and silently
// declining would surprise a script that expected the write to happen.
func (a *App) confirm(prompt string) (bool, error) {
	// Only --yes is consent. --no-input promises lion will not *ask* a
	// question; that is not the same as answering yes to one. Treating them as
	// equivalent let any non-interactive script send invites, messages, and
	// posts to real people with nothing on the command line that says "go
	// ahead". So --no-input suppresses the prompt and then declines, naming the
	// flag that does authorize the write.
	if a.Cfg.Yes {
		return true, nil
	}
	if a.Cfg.NoInput {
		return false, usageErr("writes need confirmation and --no-input suppresses the prompt; pass --yes to authorize this mutation")
	}
	if !isInteractive() {
		return false, usageErr("writes need confirmation but stdin is not a terminal; pass --yes")
	}
	fmt.Fprintf(os.Stderr, "%s [y/N] ", prompt)
	s := bufio.NewScanner(os.Stdin)
	if !s.Scan() {
		return false, nil
	}
	ans := strings.ToLower(strings.TrimSpace(s.Text()))
	return ans == "y" || ans == "yes", nil
}

// isInteractive reports whether stdin looks like an interactive terminal
// rather than a pipe or redirected file, i.e. whether it's meaningful to
// prompt on it. It's a package variable (rather than a plain function call)
// so tests can simulate a TTY without needing a real one — see
// isTerminal below for the real, stdlib-only heuristic used in production.
var isInteractive = func() bool { return isTerminal(os.Stdin) }

// isTerminal reports whether f looks like an interactive terminal rather
// than a pipe or redirected file. This is a stdlib-only heuristic (no
// golang.org/x/term dependency): a character device is the standard signal
// for "someone is typing here interactively".
func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// Execute is the entrypoint called by main. It builds the command tree, wires
// global flags, runs, and maps errors to stable exit codes.
func Execute() int {
	cfg := &config.Config{}
	root, app := newRootCmd(cfg)
	ctx := context.WithValue(context.Background(), ctxKey{}, app)

	if err := root.ExecuteContext(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return exitCode(err)
	}
	return ExitOK
}

// newRootCmd builds the command tree and wires global flags, without
// running it — split out from Execute so tests can drive it directly (via
// SetArgs/ExecuteContext) and inspect the resulting error, rather than only
// being able to exercise the process-exit-code level of Execute.
func newRootCmd(cfg *config.Config) (*cobra.Command, *App) {
	root := &cobra.Command{
		Use:           "lion",
		Short:         "A task-first LinkedIn CLI",
		Long:          "lion — one binary for LinkedIn from the terminal.\n\nData goes to stdout; prompts, progress, and warnings go to stderr.",
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			cfg.Home = config.Home()
			if cfg.JSON && cfg.Plain {
				return usageErr("choose either --json or --plain, not both")
			}
			// Layer config-file/env settings under whatever flags were
			// actually passed (flag > env > file > default — see
			// internal/config's package doc and F13).
			if err := config.Apply(cfg, cmd.Flags()); err != nil {
				return err
			}
			return nil
		},
	}
	// cobra's own flag-parse errors (unknown flag, bad value, ...) aren't our
	// usageError type by default, which would otherwise fall through to the
	// generic exit-1 bucket instead of exit 2 (F20).
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error {
		return usageErr("%s", err)
	})

	pf := root.PersistentFlags()
	pf.BoolVar(&cfg.JSON, "json", false, "emit JSON")
	pf.BoolVar(&cfg.Plain, "plain", false, "emit tab-separated values")
	pf.BoolVar(&cfg.ReadOnly, "readonly", false, "block all mutating actions")
	pf.BoolVar(&cfg.DryRun, "dry-run", false, "print intended mutations without sending")
	pf.BoolVar(&cfg.Yes, "yes", false, "assume yes for write confirmations")
	pf.BoolVar(&cfg.NoInput, "no-input", false, "never prompt (for CI); also skips write confirmations like --yes")
	pf.BoolVar(&cfg.WrapUntrusted, "wrap-untrusted", false, "wrap LinkedIn free text as untrusted data")
	pf.BoolVar(&cfg.Verbose, "verbose", false, "verbose logging on stderr")
	pf.IntVar(&cfg.Max, "max", 0, "cap number of results (0 = command default)")
	pf.StringVar(&cfg.Account, "account", "", "account alias (default: primary)")
	pf.StringVar(&cfg.ConfigPath, "config", "", "path to config file (default: $LION_HOME/config.json)")

	app := &App{Cfg: cfg}

	root.AddCommand(
		newVersionCmd(),
		newAuthCmd(),
	)
	// Feature verticals (profile, connection, message, feed, ...) self-register.
	for _, f := range commandFactories {
		root.AddCommand(f())
	}

	return root, app
}

// usageError marks an error as a usage problem (exit 2).
type usageError struct{ msg string }

func (e usageError) Error() string { return e.msg }
func usageErr(f string, a ...any) error {
	return usageError{msg: fmt.Sprintf(f, a...)}
}

// usageArgs wraps a cobra positional-args validator (cobra.ExactArgs,
// MinimumNArgs, MaximumNArgs, NoArgs, ...) so a validation failure is
// classified as a usage error (exit 2) rather than falling through to the
// generic exit-1 bucket — cobra's arg-count errors aren't our usageError
// type on their own (F20). Every command's Args field should go through
// this.
func usageArgs(v cobra.PositionalArgs) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if err := v(cmd, args); err != nil {
			return usageErr("%s", err)
		}
		return nil
	}
}

// isCobraUsageError reports whether err looks like one of cobra's own
// usage-shaped errors that our own code never returns itself — chiefly
// "unknown command". Flag-parsing errors are already converted to our
// usageError type via root.SetFlagErrorFunc, and arg-count errors via
// usageArgs, so both of those are covered before this is ever consulted;
// this is specifically the fallback for command-resolution failures (e.g.
// `lion feed reed`), which cobra doesn't expose a typed error or hook for.
// Cobra's message format for this ("unknown command %q for %q...") is part
// of its stable public behavior, so matching the fixed prefix is the
// standard way other cobra-based CLIs handle this case too.
func isCobraUsageError(err error) bool {
	return strings.HasPrefix(err.Error(), "unknown command ")
}

// exitCode maps errors (including voyager and ratelimit sentinels) to
// stable exit codes.
func exitCode(err error) int {
	switch {
	case err == nil:
		return ExitOK
	case errors.As(err, new(usageError)), isCobraUsageError(err):
		return ExitUsage
	case errors.Is(err, auth.ErrNoAccount), errors.Is(err, voyager.ErrUnauthorized):
		return ExitAuth
	case errors.Is(err, voyager.ErrRateLimited), errors.Is(err, ratelimit.ErrDailyBudget):
		return ExitRateLimited
	case errors.Is(err, voyager.ErrNotFound):
		return ExitNotFound
	case errors.Is(err, voyager.ErrChallenge):
		return ExitChallenge
	default:
		if isPermission(err) {
			return ExitPermission
		}
		return ExitError
	}
}

func isPermission(err error) bool {
	return err != nil && err.Error() == "blocked by --readonly"
}
