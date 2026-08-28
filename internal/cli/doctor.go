// Package cli's doctor.go implements `lion doctor`: the self-checks that
// answer "is this setup actually working?" in one place, matching the command
// wacli ships for the same purpose.
//
// It exists because the answer was previously spread across four commands and
// a filesystem poke — `auth status` for the session, `store stats` for the
// archive, the absence of an error from anything else for the browser. When a
// periodic sync stops producing data at 3am, the useful thing is one command
// that says which layer broke.
//
// Every check is read-only with respect to LinkedIn: doctor never posts,
// sends, or mutates anything there, so it works under --readonly and never
// prompts.
package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/jodok/lion/internal/browser"
	"github.com/jodok/lion/internal/config"
	"github.com/jodok/lion/internal/output"
	"github.com/jodok/lion/internal/store"
	"github.com/spf13/cobra"
)

func init() { registerCommand(newDoctorCmd) }

// checkStatus is one check's verdict. Three levels rather than pass/fail
// because "your archive is empty" and "your session is dead" are different
// kinds of news: the first is a note, the second is the reason nothing works.
const (
	statusOK   = "ok"
	statusWarn = "warn"
	statusFail = "fail"
)

// check is one diagnostic line. Detail carries the actionable part — a path,
// a count, or what to run next — since a bare "fail" tells nobody anything.
type check struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail"`
}

func newDoctorCmd() *cobra.Command {
	var skipNetwork bool
	var storePath string
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Check that lion's session, browser, and store are working",
		Long: "Run lion's self-checks and report what is and isn't working.\n\n" +
			"Checks the lion home directory, the browser lion drives, whether the " +
			"stored session still authenticates, and the state of the local " +
			"archive. Read-only with respect to LinkedIn — it never posts, sends, " +
			"or mutates anything there — so it works under --readonly and never " +
			"prompts. Locally it opens the store to read its schema version, " +
			"which applies any pending migration, the same one every other " +
			"command would.\n\n" +
			"Exits non-zero if any check fails, so a script or timer can gate on " +
			"it. A warning alone does not fail the command: an empty archive is " +
			"worth saying, not worth failing over.\n\n" +
			"--offline skips the check that talks to LinkedIn, which is the slow " +
			"one (it starts a browser) and the only one that needs the network.",
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			app := appFrom(cmd)
			checks := runDoctor(cmd.Context(), app, storePath, skipNetwork)

			r := app.Renderer()
			if app.Cfg.JSON {
				if err := r.Emit(checks); err != nil {
					return err
				}
			} else {
				t := &output.Table{Cols: []string{"CHECK", "STATUS", "DETAIL"}}
				for _, c := range checks {
					t.Rows = append(t.Rows, []string{c.Name, c.Status, c.Detail})
				}
				if err := r.Emit(t); err != nil {
					return err
				}
			}
			for _, c := range checks {
				if c.Status == statusFail {
					// Returned rather than printed so exitCode maps it the
					// same way every other failure is mapped, and so the
					// table above is still the report.
					return errDoctorFailed
				}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&skipNetwork, "offline", false, "skip the check that talks to LinkedIn")
	// Every other store-touching command takes this; without it doctor would
	// confidently report "no archive yet" at anyone using a non-default
	// database, which is the one thing a diagnostic must not do.
	cmd.Flags().StringVar(&storePath, "store", "", "path to the store database (default $LION_HOME/store.db)")
	return cmd
}

// errDoctorFailed marks a run in which at least one check failed. The report
// is the table already written to stdout; this only drives the exit code.
var errDoctorFailed = errors.New("one or more checks failed")

// runDoctor performs every check and returns them in a fixed order, outermost
// layer first: a broken home directory explains a broken store, which
// explains an empty archive, and reading them in that order is how someone
// finds the actual cause rather than the first symptom.
func runDoctor(ctx context.Context, app *App, storePath string, skipNetwork bool) []check {
	if ctx == nil {
		ctx = context.Background()
	}
	checks := []check{
		checkHome(),
		checkTransport(app),
		checkBrowserBinary(app),
		checkStore(ctx, storePath),
	}
	if skipNetwork {
		checks = append(checks, check{
			Name: "session", Status: statusWarn,
			Detail: "skipped (--offline); run without it to verify the session still authenticates",
		})
		return checks
	}
	return append(checks, checkSession(ctx, app))
}

// checkHome verifies the directory everything else lives in.
//
// It deliberately does not warn about loose permissions. An earlier version
// did, and the branch was unreachable: config.EnsureHome chmods the directory
// to 0700 on every call precisely so the credential store's 0600 guarantee is
// not undermined by the directory holding it, so by the time this could look,
// the thing it would report has already been repaired. A check that cannot
// fire is worse than no check — it reads as coverage.
func checkHome() check {
	home, err := config.EnsureHome()
	if err != nil {
		return check{"home", statusFail, fmt.Sprintf("cannot create %s: %v", config.Home(), err)}
	}
	return check{"home", statusOK, home}
}

// checkTransport reports which transport a command would use, since that is
// the single setting most likely to explain surprising behaviour.
func checkTransport(app *App) check {
	if app.Cfg.Browser {
		return check{"transport", statusOK, "browser (a real Chromium lion drives)"}
	}
	return check{"transport", statusWarn,
		"cookie transport (deprecated) — LinkedIn can revoke a session used this way " +
			"account-wide; run `lion auth login` to switch to a browser session"}
}

// checkBrowserBinary resolves the Chromium lion would drive without starting
// it, so a missing browser is reported as itself rather than as a failure of
// whatever command happened to need one first.
func checkBrowserBinary(app *App) check {
	if !app.Cfg.Browser {
		return check{"browser", statusOK, "not used by the cookie transport"}
	}
	path, err := browser.ChromePath(app.Cfg.ChromePath)
	if err != nil {
		return check{"browser", statusFail, fmt.Sprintf("no Chromium available: %v", err)}
	}
	return check{"browser", statusOK, path}
}

// checkStore reports the archive without creating one. Opening the store
// would create an empty database as a side effect of a diagnostic, which is
// the kind of thing a check has no business doing — so a missing file is
// reported by stat, before store.Open is ever reached.
//
// It does open an archive that already exists, which runs any pending schema
// migration. That is deliberate and is what the help text says: reporting the
// schema version means reading it through the store, and a store lion has
// opened is a store lion has migrated. It is the same upgrade every other
// command would apply, and it is additive.
func checkStore(ctx context.Context, storePath string) check {
	path := storePath
	if path == "" {
		var err error
		path, err = store.DefaultPath()
		if err != nil {
			return check{"store", statusFail, fmt.Sprintf("cannot resolve the store path: %v", err)}
		}
	}
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return check{"store", statusWarn,
			fmt.Sprintf("no archive at %s yet; run `lion sync` to create one", path)}
	} else if err != nil {
		return check{"store", statusFail, fmt.Sprintf("cannot stat %s: %v", path, err)}
	}

	st, err := store.Open(path)
	if err != nil {
		return check{"store", statusFail, fmt.Sprintf("cannot open %s: %v", path, err)}
	}
	defer st.Close()

	stats, err := st.Stats(ctx)
	if err != nil {
		return check{"store", statusFail, fmt.Sprintf("cannot read %s: %v", path, err)}
	}
	detail := fmt.Sprintf("schema v%d, %d conversations, %d messages",
		stats.SchemaVersion, stats.Conversations, stats.Messages)
	if stats.LastSyncedAt != nil {
		detail += fmt.Sprintf(", last synced %s ago",
			formatDuration(time.Since(time.UnixMilli(*stats.LastSyncedAt))))
	}
	if stats.Conversations == 0 {
		return check{"store", statusWarn, detail + " — run `lion sync` to populate it"}
	}
	return check{"store", statusOK, detail}
}

// checkSession is the one check that talks to LinkedIn, and the one that
// usually holds the answer: an expired session is why a timer stopped
// producing data. It goes through the same client every other command builds,
// so it fails the same way they would rather than testing a different path.
func checkSession(ctx context.Context, app *App) check {
	cl, err := app.Client()
	if err != nil {
		if errors.Is(err, browser.ErrLoggedOut) {
			return check{"session", statusFail, "not signed in; run `lion auth login`"}
		}
		return check{"session", statusFail, err.Error()}
	}
	defer app.closeBrowser()

	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	me, err := cl.Me(cctx)
	if err != nil {
		return check{"session", statusFail, fmt.Sprintf("session did not authenticate: %v", err)}
	}
	return check{"session", statusOK, fmt.Sprintf("signed in as %s", me.Name())}
}
