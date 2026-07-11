// Package cli builds lion's command tree. Commands are thin: they parse flags,
// call into the voyager client, and render results via the output package.
package cli

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/jodok/lion/internal/auth"
	"github.com/jodok/lion/internal/config"
	"github.com/jodok/lion/internal/output"
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

// Execute is the entrypoint called by main. It builds the command tree, wires
// global flags, runs, and maps errors to stable exit codes.
func Execute() int {
	cfg := &config.Config{}
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
			return nil
		},
	}

	pf := root.PersistentFlags()
	pf.BoolVar(&cfg.JSON, "json", false, "emit JSON")
	pf.BoolVar(&cfg.Plain, "plain", false, "emit tab-separated values")
	pf.BoolVar(&cfg.ReadOnly, "readonly", false, "block all mutating actions")
	pf.BoolVar(&cfg.DryRun, "dry-run", false, "print intended mutations without sending")
	pf.BoolVar(&cfg.Yes, "yes", false, "assume yes for confirmations")
	pf.BoolVar(&cfg.NoInput, "no-input", false, "never prompt (for CI); implies --yes for reads")
	pf.BoolVar(&cfg.WrapUntrusted, "wrap-untrusted", false, "wrap LinkedIn free text as untrusted data")
	pf.BoolVar(&cfg.Verbose, "verbose", false, "verbose logging on stderr")
	pf.IntVar(&cfg.Max, "max", 0, "cap number of results (0 = command default)")
	pf.StringVar(&cfg.Account, "account", "", "account alias (default: primary)")

	app := &App{Cfg: cfg}
	ctx := context.WithValue(context.Background(), ctxKey{}, app)

	root.AddCommand(
		newVersionCmd(),
		newAuthCmd(),
	)
	// Feature verticals (profile, connection, message, feed, ...) self-register.
	for _, f := range commandFactories {
		root.AddCommand(f())
	}

	if err := root.ExecuteContext(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return exitCode(err)
	}
	return ExitOK
}

// usageError marks an error as a usage problem (exit 2).
type usageError struct{ msg string }

func (e usageError) Error() string { return e.msg }
func usageErr(f string, a ...any) error {
	return usageError{msg: fmt.Sprintf(f, a...)}
}

// exitCode maps errors (including voyager sentinels) to stable exit codes.
func exitCode(err error) int {
	switch {
	case err == nil:
		return ExitOK
	case errors.As(err, new(usageError)):
		return ExitUsage
	case errors.Is(err, auth.ErrNoAccount), errors.Is(err, voyager.ErrUnauthorized):
		return ExitAuth
	case errors.Is(err, voyager.ErrRateLimited):
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
