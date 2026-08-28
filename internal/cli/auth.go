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
			"export). With no flags at all, lion prompts for that same Cookie " +
			"header. --cookies '<value>', --li-at, and --jsessionid still work " +
			"for compatibility, but any credential passed directly as a " +
			"command-line flag is visible in shell history and in the process " +
			"listing of any other user on the same machine — lion prints a " +
			"warning when you do this.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			app := appFrom(cmd)
			warnArgvCredentials(liAt, jsession, cookiesFlag)
			cookies, err := resolveLoginCookies(cookiesFlag, cookiesFile, cookiesStdin, liAt, jsession, app.Cfg.NoInput, cmd.InOrStdin())
			if err != nil {
				return err
			}
			if cookies["li_at"] == "" || cookies["JSESSIONID"] == "" {
				return usageErr("%s", missingCookiesMsg(cookies))
			}
			warnThinCookieJar(cookies)

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
			// Save cl.Cookies() — the post-validation jar — rather than the
			// cookies map built above: LinkedIn can rotate JSESSIONID/li_at
			// or refresh __cf_bm during the very /me request that validates
			// the session, and saving the pre-validation snapshot would
			// store an already-stale credential. Without this, a login can
			// become unusable within minutes of being saved even though it
			// worked at the moment it was validated.
			cred := &auth.Credential{
				Alias:    firstNonEmpty(alias, "default"),
				Cookies:  cl.Cookies(),
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
// -`), --cookies-file, --cookies, then --li-at/--jsessionid. With none of
// those, it prompts for the full Cookie header (unless --no-input is set).
// Every path funnels through auth.NormalizeCookies so cookie values (notably
// JSESSIONID's wire quoting) are corrected in one place, matching what
// auth.Save persists.
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
		// Nothing on the command line: ask for the whole Cookie header, the
		// same input --cookies-stdin takes. This prompt used to ask for li_at
		// and JSESSIONID separately, which quietly steered every interactive
		// login into the two-cookie compatibility path — a jar that
		// authenticates /me (so `auth login` reports success) but is rejected
		// by the GraphQL surface `message list` and `profile search` use, and
		// that is missing the browser-identity cookies LinkedIn expects to
		// see alongside a session (DESIGN.md §3.3). --li-at/--jsessionid
		// still select the old path, prompting for whichever one they omit.
		// One buffered reader shared by every prompt below (see
		// promptSecret), constructed here rather than beside `cookies`
		// above: this is the only branch that prompts, and the
		// --cookies-stdin branch reads the same stdin directly with
		// io.ReadAll. Two readers over one source is safe only as long as
		// the buffered one is never read from first, so it is scoped to
		// where it is actually used instead of leaving that invariant for a
		// later edit to trip over.
		prompts := bufio.NewReader(stdin)
		if liAt == "" && jsession == "" {
			cookies = parseCookieHeader(promptSecret(cookieHeaderPrompt, noInput, prompts))
			break
		}
		if liAt == "" {
			liAt = promptSecret("li_at cookie: ", noInput, prompts)
		}
		if jsession == "" {
			jsession = promptSecret("JSESSIONID cookie: ", noInput, prompts)
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

// cookieHeaderPrompt is the interactive login prompt. It spells out where
// the value comes from because "paste your cookies" is the step people get
// wrong — the header lives in the Network tab's request headers, not in the
// Application tab's cookie table (whose tab-separated copy output is not the
// Netscape format --cookies-file reads).
const cookieHeaderPrompt = "Paste the full Cookie: header for linkedin.com.\n" +
	"DevTools -> Network -> any linkedin.com request -> Request Headers -> Cookie\n" +
	"(li_at and JSESSIONID alone are not enough — see `lion auth login --help`)\n\n" +
	"Cookie: "

// identityCookies are the cookies that identify the browser instance a
// session belongs to. LinkedIn issues them alongside li_at and expects to
// keep seeing them: a session presented without them looks like it moved to
// a different device, which is the shape of a stolen cookie. Requests can
// still succeed for a while, so their absence is a warning rather than an
// error — but it is the difference between a login that keeps working and
// one that dies minutes later.
var identityCookies = []string{"bcookie", "bscookie"}

// warnThinCookieJar warns on stderr when the cookies supplied to `auth
// login` omit the browser-identity ones. It runs after the li_at/JSESSIONID
// check, so by here the session itself is well-formed and the only thing
// worth saying is that this input is thinner than a browser's jar.
//
// The message deliberately talks about what was *supplied*, not about the
// credential that gets saved. Those are different jars: the credential
// stores the post-validation snapshot (see the Cookies field in login's
// RunE), and LinkedIn issues bcookie/bscookie during the very /me request
// that validates the session — so a warned-about login still lands on disk
// with both cookies present. Wording this as "the saved jar is missing
// them" would be provably false the moment anyone looked at the store, and
// the warning would read as a bug rather than as the one notice explaining
// why the session is about to stop working. Server-minted identity cookies
// are exactly the problem being described: they identify a jar that is not
// the browser the session was issued to.
func warnThinCookieJar(cookies map[string]string) {
	var missing []string
	for _, name := range identityCookies {
		if cookies[name] == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) == 0 {
		return
	}
	fmt.Fprintf(os.Stderr,
		"warning: the cookies you supplied omit %s, so this session is not being "+
			"presented as the browser it was issued to. LinkedIn hands out its own "+
			"and then typically drops the session within minutes, and GraphQL "+
			"endpoints (message list, profile search) can reject it outright. Paste "+
			"the whole Cookie: header instead: "+
			"`pbpaste | lion auth login --cookies-stdin`\n",
		strings.Join(missing, " and "))
}

// missingCookiesMsg builds the error for login input that didn't yield both
// required cookies. It reports how many cookies were parsed, because the
// usual cause is input that is not a Cookie header at all — an empty or
// overwritten clipboard, a copied terminal line, or DevTools' cookie table
// rather than the request header. A bare "both are required" leaves the user
// re-pasting the same wrong thing; a count of 0 says lion never saw a cookie.
// Only the count is reported, never parsed names or values, so a mistaken
// paste of unrelated content can't be echoed back to the terminal.
func missingCookiesMsg(cookies map[string]string) string {
	var missing []string
	for _, name := range []string{"li_at", "JSESSIONID"} {
		if cookies[name] == "" {
			missing = append(missing, name)
		}
	}
	noun := "cookie"
	if len(missing) > 1 {
		noun = "cookies"
	}
	return fmt.Sprintf("missing required %s %s (parsed %d cookie(s) from the input). "+
		"Copy the whole Cookie: header for linkedin.com — DevTools -> Network -> any "+
		"linkedin.com request -> Request Headers -> Cookie — and pipe it in with "+
		"`pbpaste | lion auth login --cookies-stdin`",
		noun, strings.Join(missing, " and "), len(cookies))
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

// promptSecret prints label to stderr and reads one line from r. It is
// intentionally simple (no echo suppression) since cookies are pasted, not
// typed; NoInput short-circuits before anything is printed or read.
//
// The reader is passed in (rather than read from os.Stdin here) so the
// caller can share one buffered reader across consecutive prompts. A fresh
// bufio.Reader per prompt would read ahead past its own line and discard
// what it buffered, so a second prompt could miss input the user had already
// typed. It also lets tests drive the prompt.
//
// bufio.Reader.ReadString rather than bufio.Scanner: a full Cookie header is
// a few KB today and Scanner's 64KB line cap would silently truncate rather
// than error if one ever grew past it.
func promptSecret(label string, noInput bool, r *bufio.Reader) string {
	if noInput {
		return ""
	}
	fmt.Fprint(os.Stderr, label)
	line, err := r.ReadString('\n')
	if line == "" && err != nil {
		return ""
	}
	return strings.TrimRight(line, "\r\n")
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
