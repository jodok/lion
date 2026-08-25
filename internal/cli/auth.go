package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/jodok/lion/internal/auth"
	"github.com/jodok/lion/internal/output"
	"github.com/jodok/lion/internal/voyager"
	"github.com/spf13/cobra"
)

func newAuthCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "auth",
		Short: "Manage LinkedIn session credentials",
	}
	cmd.AddCommand(newAuthLoginCmd(), newAuthStatusCmd(), newAuthLogoutCmd())
	return cmd
}

func newAuthLoginCmd() *cobra.Command {
	var liAt, jsession, alias, cookiesFlag, cookiesFile string
	var cookiesStdin bool
	cmd := &cobra.Command{
		Use:   "login",
		Short: "Store a LinkedIn session (full browser cookie jar)",
		Long: "Store the browser session cookies lion uses to call LinkedIn's " +
			"Voyager API.\n\nGraphQL endpoints (search, modern profile) reject a " +
			"bare li_at+JSESSIONID pair and want the full linkedin.com cookie " +
			"jar (bcookie, bscookie, lidc, li_gc, ...) — see DESIGN.md §3.3. " +
			"Recommended: pipe the Cookie header in on stdin so it never touches " +
			"shell history or a process listing, e.g. " +
			"`pbpaste | lion auth login --cookies-stdin`, or use --cookies-file " +
			"PATH (a saved Cookie header line, or a Netscape cookies.txt " +
			"export). --cookies '<value>', --li-at, and --jsessionid (or the " +
			"interactive prompt) still work for compatibility, but any " +
			"credential passed directly as a command-line flag is visible in " +
			"shell history and in the process listing of any other user on the " +
			"same machine — lion prints a warning when you do this.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			app := appFrom(cmd)
			warnArgvCredentials(liAt, jsession, cookiesFlag)
			cookies, err := resolveLoginCookies(cookiesFlag, cookiesFile, cookiesStdin, liAt, jsession, app.Cfg.NoInput, cmd.InOrStdin())
			if err != nil {
				return err
			}
			if cookies["li_at"] == "" || cookies["JSESSIONID"] == "" {
				return usageErr("both li_at and JSESSIONID cookies are required")
			}

			// Validate the session before saving so we never store dead
			// cookies, going through the same Chrome-impersonating transport
			// production traffic uses (DESIGN.md §3.3) so this exercises the
			// real path rather than the stdlib fallback.
			transport, err := voyager.NewChromeTransport(cookies)
			if err != nil {
				return fmt.Errorf("build transport: %w", err)
			}
			cl := voyager.New(cookies["li_at"], cookies["JSESSIONID"],
				voyager.WithCookies(cookies), voyager.WithTransport(transport))
			me, err := cl.Me(context.Background())
			if err != nil {
				return fmt.Errorf("validate session: %w", err)
			}
			cred := &auth.Credential{
				Alias:    firstNonEmpty(alias, "default"),
				Cookies:  cookies,
				MemberID: me.URN,
				Name:     me.Name(),
				SavedAt:  time.Now(),
			}
			if err := auth.Save(cred); err != nil {
				return err
			}
			fmt.Fprintf(os.Stderr, "saved session for %s (alias %q)\n", me.Name(), cred.Alias)
			return nil
		},
	}
	cmd.Flags().StringVar(&liAt, "li-at", "", "li_at cookie value (argv — visible in shell history/process listing; prefer --cookies-stdin)")
	cmd.Flags().StringVar(&jsession, "jsessionid", "", "JSESSIONID cookie value (argv — visible in shell history/process listing; prefer --cookies-stdin)")
	cmd.Flags().StringVar(&cookiesFlag, "cookies", "", "full Cookie: header string, or '-' to read it from stdin (argv is visible in shell history/process listing; prefer --cookies-stdin/--cookies-file)")
	cmd.Flags().BoolVar(&cookiesStdin, "cookies-stdin", false, "read the full Cookie: header from stdin (recommended: keeps credentials out of shell history and process listings)")
	cmd.Flags().StringVar(&cookiesFile, "cookies-file", "", "path to a file with a Cookie header line or a Netscape cookies.txt export")
	cmd.Flags().StringVar(&alias, "alias", "default", "account alias")
	return cmd
}

// warnArgvCredentials prints a one-line stderr warning when session
// credentials were passed directly as command-line flag values, which are
// visible in shell history and in the process listing of any other user on
// the same machine (F9). --cookies-file and --cookies-stdin (or `--cookies
// -`) don't trigger this since the credential value itself never appears in
// argv.
func warnArgvCredentials(liAt, jsession, cookiesFlag string) {
	if liAt != "" || jsession != "" || (cookiesFlag != "" && cookiesFlag != "-") {
		fmt.Fprintln(os.Stderr, "warning: passing credentials as a command-line flag exposes them in shell history and process listings; prefer --cookies-stdin or --cookies-file")
	}
}

// resolveLoginCookies builds the cookie map for `auth login` from whichever
// input source was given, in priority order: --cookies-stdin (or `--cookies
// -`), --cookies-file, --cookies, then --li-at/--jsessionid (prompting for
// any still missing unless --no-input is set). Every path funnels through
// auth.NormalizeCookies so cookie values (notably JSESSIONID's wire
// quoting) are corrected in one place, matching what auth.Save persists.
func resolveLoginCookies(cookiesFlag, cookiesFile string, cookiesStdin bool, liAt, jsession string, noInput bool, stdin io.Reader) (map[string]string, error) {
	var cookies map[string]string
	switch {
	case cookiesStdin, cookiesFlag == "-":
		b, err := io.ReadAll(stdin)
		if err != nil {
			return nil, fmt.Errorf("read cookies from stdin: %w", err)
		}
		cookies = parseCookiesFile(string(b))
	case cookiesFile != "":
		b, err := os.ReadFile(cookiesFile)
		if err != nil {
			return nil, fmt.Errorf("read cookies file: %w", err)
		}
		cookies = parseCookiesFile(string(b))
	case cookiesFlag != "":
		cookies = parseCookieHeader(cookiesFlag)
	default:
		if liAt == "" {
			liAt = promptSecret("li_at cookie: ", noInput)
		}
		if jsession == "" {
			jsession = promptSecret("JSESSIONID cookie: ", noInput)
		}
		cookies = map[string]string{}
		if liAt = strings.TrimSpace(liAt); liAt != "" {
			cookies["li_at"] = liAt
		}
		// JSESSIONID's quoting is fixed up by auth.NormalizeCookies below, so
		// the user can paste it with or without the surrounding quotes.
		if jsession = strings.TrimSpace(jsession); jsession != "" {
			cookies["JSESSIONID"] = jsession
		}
	}
	auth.NormalizeCookies(cookies)
	return cookies, nil
}

// parseCookieHeader parses a `Cookie:` header value (e.g. `li_at=..; ` +
// `JSESSIONID="ajax:..."; bcookie=..; lidc=..`) into a name->value map. An
// optional literal `Cookie:` prefix (as copied straight from DevTools) is
// stripped first. Values are kept verbatim, including any quotes — the
// JSESSIONID cookie is quoted on the wire and normalized centrally by
// auth.NormalizeCookies; the csrf-token header strips the quotes again (see
// voyager.Client.New).
func parseCookieHeader(header string) map[string]string {
	header = strings.TrimSpace(header)
	if len(header) >= len("cookie:") && strings.EqualFold(header[:len("cookie:")], "cookie:") {
		header = strings.TrimSpace(header[len("cookie:"):])
	}
	cookies := map[string]string{}
	for _, part := range strings.Split(header, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		name, value, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		cookies[name] = strings.TrimSpace(value)
	}
	return cookies
}

// parseCookiesFile parses the contents of a --cookies-file: either a single
// pasted `Cookie:` header line, or a Netscape cookies.txt export (as
// produced by browser cookie-export extensions) — one cookie per line, 7
// tab-separated fields: domain, includeSubdomains, path, secure,
// expiration, name, value.
func parseCookiesFile(data string) map[string]string {
	if looksLikeNetscapeCookies(data) {
		return parseNetscapeCookies(data)
	}
	return parseCookieHeader(strings.TrimSpace(data))
}

// looksLikeNetscapeCookies reports whether data looks like a Netscape
// cookies.txt export rather than a pasted Cookie header: the format is
// tab-separated (a Cookie header is semicolon-separated name=value pairs on
// one line), so the first non-comment, non-blank line is the tell.
func looksLikeNetscapeCookies(data string) bool {
	for _, line := range strings.Split(data, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#") && !strings.HasPrefix(line, "#HttpOnly_") {
			continue
		}
		return strings.Count(line, "\t") >= 6
	}
	return false
}

// parseNetscapeCookies parses a Netscape cookies.txt export into a
// name->value map. Only linkedin.com cookies are kept: cookiesToFHTTP later
// rewrites every cookie's domain to .linkedin.com, so importing a general
// cookies.txt export unfiltered would disclose other sites' cookies to
// LinkedIn (and cause name collisions). Records for any other domain are
// dropped.
func parseNetscapeCookies(data string) map[string]string {
	cookies := map[string]string{}
	for _, line := range strings.Split(data, "\n") {
		line = strings.TrimRight(line, "\r")
		line = strings.TrimPrefix(line, "#HttpOnly_")
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) < 7 {
			continue
		}
		host := strings.TrimPrefix(strings.TrimSpace(fields[0]), ".")
		if host != "linkedin.com" && !strings.HasSuffix(host, ".linkedin.com") {
			continue
		}
		name := strings.TrimSpace(fields[5])
		if name == "" {
			continue
		}
		cookies[name] = strings.TrimSpace(fields[6])
	}
	return cookies
}

// accountStatus is one stored account's validated session state.
type accountStatus struct {
	Alias   string
	Name    string
	Default bool
	// State is one of: "valid", "expired", "challenge", "unknown". "unknown"
	// covers both network failures (offline, timeout) and any other
	// unexpected error — validation must never crash the command, it should
	// just report the account as unverifiable.
	State string
}

// validateAccountsTimeout bounds each account's session check so an
// unreachable network can't hang `auth status` indefinitely.
const validateAccountsTimeout = 10 * time.Second

// validateAccounts checks every credential's session validity by calling a
// lightweight authenticated endpoint (Me, i.e. GET /me) through the client
// newClient builds for it (F1). It never returns an error for an
// individual account's check failing — network/timeout/unexpected errors
// are reported as "unknown" so one flaky account doesn't blow up the whole
// command — and the result is sorted by alias for stable output (F18).
func validateAccounts(ctx context.Context, creds []*auth.Credential, def string, newClient func(*auth.Credential) (*voyager.Client, error)) []accountStatus {
	out := make([]accountStatus, 0, len(creds))
	for _, c := range creds {
		st := accountStatus{Alias: c.Alias, Name: c.Name, Default: c.Alias == def}
		cl, err := newClient(c)
		if err != nil {
			st.State = "unknown"
			out = append(out, st)
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, validateAccountsTimeout)
		_, meErr := cl.Me(cctx)
		cancel()
		switch {
		case meErr == nil:
			st.State = "valid"
		case errors.Is(meErr, voyager.ErrUnauthorized):
			st.State = "expired"
		case errors.Is(meErr, voyager.ErrChallenge):
			st.State = "challenge"
		default:
			// Includes context deadline/network errors: reachability
			// problems shouldn't be reported as "expired" (a different,
			// actionable condition — it means re-login is needed).
			st.State = "unknown"
		}
		out = append(out, st)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Alias < out[j].Alias })
	return out
}

// newStatusValidationClient builds a real voyager client for validating one
// stored account, using the same Chrome-impersonating transport production
// traffic uses (DESIGN.md §3.3) — the bot wall blocks stdlib net/http, so a
// plain client would misreport every account as unreachable.
func newStatusValidationClient(c *auth.Credential) (*voyager.Client, error) {
	cookies := c.Cookies
	if len(cookies) == 0 {
		cookies = map[string]string{"li_at": c.LiAt, "JSESSIONID": c.JSessionID}
	}
	t, err := voyager.NewChromeTransport(cookies)
	if err != nil {
		return nil, err
	}
	return voyager.New(c.LiAt, c.JSessionID, voyager.WithCookies(cookies), voyager.WithTransport(t)), nil
}

func newAuthStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show every stored account and validate its session against LinkedIn",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			app := appFrom(cmd)
			creds, def, err := auth.List()
			if err != nil {
				return err
			}
			if len(creds) == 0 {
				return auth.ErrNoAccount
			}
			// Progress goes to stderr; stdout stays data-only.
			fmt.Fprintf(os.Stderr, "validating %d session(s)...\n", len(creds))
			statuses := validateAccounts(context.Background(), creds, def, newStatusValidationClient)

			r := app.Renderer()
			if app.Cfg.JSON {
				type row struct {
					Alias   string `json:"alias"`
					Name    string `json:"name"`
					Status  string `json:"status"`
					Default bool   `json:"default"`
				}
				rows := make([]row, 0, len(statuses))
				for _, s := range statuses {
					rows = append(rows, row{s.Alias, s.Name, s.State, s.Default})
				}
				return r.Emit(rows)
			}
			t := &output.Table{Cols: []string{"ALIAS", "NAME", "STATUS", "DEFAULT"}}
			for _, s := range statuses {
				d := ""
				if s.Default {
					d = "*"
				}
				t.Rows = append(t.Rows, []string{s.Alias, s.Name, s.State, d})
			}
			return r.Emit(t)
		},
	}
}

func newAuthLogoutCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "logout [alias]",
		Short: "Remove a stored session",
		Args:  usageArgs(cobra.MaximumNArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			app := appFrom(cmd)
			alias := app.Cfg.Account
			if len(args) == 1 {
				alias = args[0]
			}
			if alias == "" {
				// F2: resolve the store's actual recorded default rather
				// than assuming the literal alias "default" — an account
				// logged in under a different --alias (e.g. the first
				// account, "work") becomes the real default, and the
				// literal string "default" may not exist at all.
				def, err := auth.DefaultAlias()
				if err != nil {
					return err
				}
				if def == "" {
					return auth.ErrNoAccount
				}
				alias = def
			}
			if err := auth.Delete(alias); err != nil {
				return err
			}
			fmt.Fprintf(os.Stderr, "removed account %q\n", alias)
			return nil
		},
	}
}

// promptSecret reads a line from stdin. It is intentionally simple (no echo
// suppression) since cookies are pasted, not typed; NoInput short-circuits.
func promptSecret(label string, noInput bool) string {
	if noInput {
		return ""
	}
	fmt.Fprint(os.Stderr, label)
	s := bufio.NewScanner(os.Stdin)
	if s.Scan() {
		return s.Text()
	}
	return ""
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
