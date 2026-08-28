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
	"github.com/jodok/lion/internal/browser"
	"github.com/jodok/lion/internal/config"
	"github.com/jodok/lion/internal/output"
	"github.com/jodok/lion/internal/ratelimit"
	"github.com/jodok/lion/internal/voyager"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"golang.org/x/term"
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

	// clients and clientAlias record every voyager.Client this App built via
	// Client(), and the account alias they were built for, so
	// persistRotatedCookies can write back whatever cookies LinkedIn rotated
	// during the command's run (DESIGN.md §3.3). A single App only ever
	// resolves one alias per invocation (a.Cfg.Account doesn't change
	// mid-command), so tracking one alias alongside however many clients
	// were built is enough — every command file that calls Client() does so
	// at most once per RunE today, but nothing stops that from changing.
	clients     []*voyager.Client
	clientAlias string
	// clientLiAt is the li_at the clients were built from, handed to
	// auth.UpdateCookies as a compare-and-swap on session identity so a
	// concurrent `auth login` that replaced this alias mid-command doesn't get
	// the old session's cookies spliced onto its record.
	clientLiAt string

	// browser is the Chromium session backing --browser, launched lazily on
	// the first Client() call and shared by every later one in the same
	// invocation. Starting a browser costs seconds and a profile lock, so
	// one per command is the most that can be afforded — and the second
	// launch would fail anyway, since Chromium refuses to open a profile
	// another process already holds.
	browser *browser.Browser
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
	if a.Cfg.Browser {
		return a.browserClient(opts...)
	}
	// Warned here rather than for every invocation: `lion version` and `lion
	// store stats` never open a connection, and annotating purely local work
	// with a transport notice trains people to ignore it.
	warnCookieTransport()
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
	cl := voyager.New(cred.LiAt, cred.JSessionID, append(base, opts...)...)
	// cred.Alias is the *resolved* alias: auth.Get("") resolves an empty
	// a.Cfg.Account to the store's default, so this must come off the
	// returned credential rather than a.Cfg.Account directly, or the
	// writeback below would try to save under alias "" instead of the
	// account it actually authenticated as.
	a.clients = append(a.clients, cl)
	a.clientAlias = cred.Alias
	a.clientLiAt = cred.LiAt
	return cl, nil
}

// browserClient builds a client whose requests are issued by a real Chromium
// lion drives, rather than by replaying stored cookies (see internal/browser).
//
// There are no cookies to pass: the session lives in the browser profile, and
// the transport's fetch() runs inside a linkedin.com page that already
// carries it. The one thing the Client still needs is the csrf-token, which
// Voyager derives from JSESSIONID — read out of the page the same way
// LinkedIn's own web app reads it.
func (a *App) browserClient(opts ...voyager.Option) (*voyager.Client, error) {
	ctx := context.Background()
	if a.browser == nil {
		b, err := browser.Launch(ctx, browser.Options{
			Alias:      a.Cfg.Account,
			Headed:     a.Cfg.Headed,
			ChromePath: a.Cfg.ChromePath,
			Verbose:    a.Cfg.Verbose,
		})
		if err != nil {
			return nil, err
		}
		if err := b.Open(ctx); err != nil {
			b.Close()
			// Someone upgrading arrives here with a working cookie
			// credential and an empty browser profile, and "not signed in"
			// alone would look like lion had lost their session. Name both
			// ways forward instead. Deliberately not an automatic fallback:
			// the cookie transport is what gets a session revoked
			// account-wide, so choosing it has to be explicit.
			if errors.Is(err, browser.ErrLoggedOut) {
				if _, cErr := auth.Get(a.Cfg.Account); cErr == nil {
					return nil, fmt.Errorf("%w\n\nThis account has stored cookies from an older lion, "+
						"but lion now drives a real browser by default — replaying cookies gets the "+
						"session revoked account-wide. Either run `lion auth login` to sign in once "+
						"through a browser, or pass --cookie-transport to keep using the stored "+
						"cookies (deprecated)", err)
				}
			}
			return nil, err
		}
		a.browser = b
	}
	csrf, err := a.browser.CSRFToken(ctx)
	if err != nil {
		return nil, err
	}
	// csrf is passed where a JSESSIONID would go: voyager.New derives the
	// header by stripping that value's quotes, and this one is already
	// stripped, so the two agree.
	base := []voyager.Option{
		voyager.WithTransport(a.browser.Transport()),
		voyager.WithDryRun(a.Cfg.DryRun),
	}
	return voyager.New("", csrf, append(base, opts...)...), nil
}

// closeBrowser shuts down the browser this invocation started, if any.
func (a *App) closeBrowser() {
	if a.browser != nil {
		a.browser.Close()
		a.browser = nil
	}
}

// persistRotatedCookies writes back any cookies LinkedIn rotated during this
// command's run — JSESSIONID, li_at, and lidc rotate continuously, and the
// Cloudflare __cf_bm cookie expires (DESIGN.md §3.3) — so the next
// invocation starts from a fresh jar instead of the stale snapshot `auth
// login` stored, which otherwise stops working within minutes. Later
// clients win when merging (matching how a later Set-Cookie would actually
// supersede an earlier one within the same run).
//
// A writeback failure is deliberately non-fatal: this runs after the
// command already did what it was asked to do, so a store-lock timeout or a
// disk error here must never turn a successful command into a failing one.
// It's reported on stderr only under --verbose, since otherwise it would be
// noise on every single command that happens to hit it.
func (a *App) persistRotatedCookies() {
	// Under --browser the session belongs to the Chromium profile and the
	// credential store holds nothing for this account. Writing back here
	// would create a second, diverging copy of a session lion does not own.
	if a.Cfg.Browser {
		return
	}
	if len(a.clients) == 0 {
		return
	}
	merged := map[string]string{}
	for _, cl := range a.clients {
		for k, v := range cl.Cookies() {
			merged[k] = v
		}
	}
	if _, err := auth.UpdateCookies(a.clientAlias, a.clientLiAt, merged); err != nil && a.Cfg.Verbose {
		fmt.Fprintf(os.Stderr, "warning: could not persist rotated session cookies: %v\n", err)
	}
}

// warnCookieTransport is printed once, where the deprecated transport is
// actually constructed.
func warnCookieTransport() {
	fmt.Fprintln(os.Stderr, "warning: the cookie transport is deprecated. It replays stored "+
		"cookies over a synthesized TLS fingerprint, and LinkedIn can revoke the session "+
		"account-wide within minutes — signing you out of your own browser. Run `lion auth "+
		"login` without --cookie-transport to sign in through a real browser instead.")
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
// confirmation). It returns proceed=true only when the answer is "y"/"yes",
// or when --yes is set. --yes is the only flag that authorizes a write:
// --no-input suppresses the prompt but does not answer it (see below).
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

// isInteractive reports whether stdin is an interactive terminal rather than
// a pipe, a redirected file, or /dev/null, i.e. whether it's meaningful to
// prompt on it. It's a package variable (rather than a plain function call)
// so tests can simulate a TTY without needing a real one — see isTerminal
// below for the check used in production.
var isInteractive = func() bool { return isTerminal(os.Stdin) }

// isTerminal reports whether f is a real interactive terminal.
//
// This asks the OS (via an ioctl) rather than checking os.ModeCharDevice,
// which was the earlier stdlib-only heuristic and was wrong in the case that
// matters most: /dev/null is a character device, so cron jobs and daemons —
// which conventionally run with stdin on /dev/null — looked interactive. The
// prompt was then written to a stream nobody reads, the scanner hit EOF
// immediately, and the mutation was silently skipped while the command exited
// 0, so automation recorded an invite or message as sent that never was.
// Failing that check now routes those callers to the "pass --yes" usage error
// instead.
func isTerminal(f *os.File) bool {
	return term.IsTerminal(int(f.Fd()))
}

// Execute is the entrypoint called by main. It builds the command tree, wires
// global flags, runs, and maps errors to stable exit codes.
func Execute() int {
	cfg := &config.Config{}
	root, app := newRootCmd(cfg)
	ctx := context.WithValue(context.Background(), ctxKey{}, app)

	err := root.ExecuteContext(ctx)
	defer app.closeBrowser()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		// A failed command still rotated cookies on its way to failing, and
		// most failures say nothing about the session: a 404, a local budget
		// stop, or a usage error leaves the jar every bit as valid as a
		// success would. Discarding those rotations would reintroduce exactly
		// the slow decay this writeback exists to stop, just at a lower rate.
		//
		// The exception is a failure that means the session itself is gone.
		// LinkedIn answers a dead session by clearing the cookie
		// (Set-Cookie: li_at=delete me, DESIGN.md §3.3), so persisting that
		// jar would overwrite a good stored credential with the wipe — and a
		// challenge response is no more trustworthy. Keep what was stored and
		// let the user re-run `auth login`.
		if !errors.Is(err, voyager.ErrUnauthorized) && !errors.Is(err, voyager.ErrChallenge) {
			app.persistRotatedCookies()
		}
		return exitCode(err)
	}
	// Persisting cookie rotation is wired here rather than as a
	// PersistentPostRun(E) on root: cobra only runs the *nearest*
	// PersistentPostRun in the command tree for a given invocation — a
	// child command that sets its own overrides the root's rather than
	// running alongside it (short of opting into
	// cobra.EnableTraverseRunHooks, which nothing here does). Feature
	// verticals in this codebase self-register from independent files
	// specifically so they don't have to coordinate with each other (see
	// commandFactories' doc comment); a future vertical adding its own
	// PersistentPostRunE for unrelated cleanup would silently disable
	// cookie writeback for its whole subtree, with no compiler error and no
	// obvious symptom beyond sessions quietly going stale again. Calling
	// persistRotatedCookies directly here instead makes it run exactly
	// once, whenever the command succeeded, regardless of what any
	// subcommand does with its own hooks. It also runs on the failure path
	// above — a command that failed for an unrelated reason still rotated
	// cookies worth keeping — with the auth failures carved out there.
	app.persistRotatedCookies()
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
			// --cookie-transport is the explicit opt-out and wins over both
			// the --browser default and anything the config file said, so a
			// person reaching for the old path gets it without having to
			// know which setting takes precedence.
			if cmd.Flags().Changed("cookie-transport") {
				// Explicit either way: --cookie-transport selects the old
				// path, and --cookie-transport=false asks for the browser
				// back, which is otherwise unreachable without the
				// now-deprecated --browser once a config file has opted out.
				cfg.Browser = !cfg.CookieTransport
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
	pf.BoolVar(&cfg.ReadOnly, "readonly", false, "block all mutating actions (--read-only also accepted, matching wacli)")
	// wacli spells this --read-only. Accepting both costs a normalizer and
	// spares anyone driving both tools from remembering which is which;
	// aliasing rather than renaming keeps every existing lion script working.
	root.SetGlobalNormalizationFunc(func(f *pflag.FlagSet, name string) pflag.NormalizedName {
		if name == "read-only" {
			name = "readonly"
		}
		return pflag.NormalizedName(name)
	})
	pf.BoolVar(&cfg.DryRun, "dry-run", false, "print intended mutations without sending")
	pf.BoolVar(&cfg.Yes, "yes", false, "assume yes for write confirmations")
	pf.BoolVar(&cfg.NoInput, "no-input", false, "never prompt (for CI); writes still require --yes")
	pf.BoolVar(&cfg.WrapUntrusted, "wrap-untrusted", false, "wrap LinkedIn free text as untrusted data")
	pf.BoolVar(&cfg.Verbose, "verbose", false, "verbose logging on stderr")
	pf.IntVar(&cfg.Max, "max", 0, "cap number of results (0 = command default)")
	pf.StringVar(&cfg.Account, "account", "", "account alias (default: primary)")
	pf.StringVar(&cfg.ConfigPath, "config", "", "path to config file (default: $LION_HOME/config.json)")
	// Default true: the cookie transport's synthesized fingerprint gets a
	// session revoked account-wide within minutes (see internal/browser), so
	// it cannot be what an unqualified `lion` command uses.
	pf.BoolVar(&cfg.Browser, "browser", true, "route LinkedIn traffic through a real Chromium lion drives (the default)")
	_ = pf.MarkDeprecated("browser", "it is the default; pass --cookie-transport for the old behaviour")
	pf.BoolVar(&cfg.CookieTransport, "cookie-transport", false, "replay stored cookies instead of driving a browser (deprecated; LinkedIn can revoke the session account-wide)")
	pf.BoolVar(&cfg.Headed, "headed", false, "show the browser window (sign-in always shows it regardless)")
	pf.StringVar(&cfg.ChromePath, "chrome-path", "", "Chromium binary to drive (default: system Chrome, else a downloaded build)")

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
	// browser.ErrLoggedOut is the --browser spelling of the same condition
	// as ErrUnauthorized: there is no usable session and the fix is to sign
	// in again. Scripts branching on exit 3 should not have to care which
	// transport produced it.
	case errors.Is(err, auth.ErrNoAccount), errors.Is(err, voyager.ErrUnauthorized),
		errors.Is(err, browser.ErrLoggedOut):
		return ExitAuth
	// ErrBudgetLock/ErrBudgetPersist are the limiter refusing to act because it
	// could not account for the action (lock unavailable, state unwritable).
	// They belong with the other "the budget stopped you" outcomes: a caller
	// retrying on exit 4 is doing the right thing, whereas exit 1 would read as
	// an unrelated failure.
	case errors.Is(err, voyager.ErrRateLimited), errors.Is(err, ratelimit.ErrDailyBudget),
		errors.Is(err, ratelimit.ErrBudgetLock), errors.Is(err, ratelimit.ErrBudgetPersist):
		return ExitRateLimited
	case errors.Is(err, voyager.ErrNotFound):
		return ExitNotFound
	case errors.Is(err, voyager.ErrChallenge):
		return ExitChallenge
	case errors.Is(err, errDoctorFailed):
		// `doctor` reporting a broken setup is a normal, successful run of
		// the command: the table it printed is the answer. The non-zero exit
		// exists so a timer or script can gate on it, and ExitError is the
		// generic bucket rather than a new code, since the report — not the
		// number — carries which layer broke.
		return ExitError
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
