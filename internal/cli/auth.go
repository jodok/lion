package cli

import (
	"bufio"
	"context"
	"fmt"
	"os"
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
	cmd := &cobra.Command{
		Use:   "login",
		Short: "Store a LinkedIn session (full browser cookie jar)",
		Long: "Store the browser session cookies lion uses to call LinkedIn's " +
			"Voyager API.\n\nGraphQL endpoints (search, modern profile) reject a " +
			"bare li_at+JSESSIONID pair and want the full linkedin.com cookie " +
			"jar (bcookie, bscookie, lidc, li_gc, ...) — see DESIGN.md §3.3. " +
			"Preferred: --cookies '<paste the Cookie: header from DevTools → " +
			"Network → any linkedin.com request>' or --cookies-file PATH (a " +
			"saved Cookie header line, or a Netscape cookies.txt export). " +
			"--li-at/--jsessionid (or the interactive prompt) still work but " +
			"only cover li_at + JSESSIONID.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			app := appFrom(cmd)
			cookies, err := resolveLoginCookies(cookiesFlag, cookiesFile, liAt, jsession, app.Cfg.NoInput)
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
	cmd.Flags().StringVar(&liAt, "li-at", "", "li_at cookie value")
	cmd.Flags().StringVar(&jsession, "jsessionid", "", "JSESSIONID cookie value")
	cmd.Flags().StringVar(&cookiesFlag, "cookies", "", "full Cookie: header string (preferred — GraphQL needs the whole jar)")
	cmd.Flags().StringVar(&cookiesFile, "cookies-file", "", "path to a file with a Cookie header line or a Netscape cookies.txt export")
	cmd.Flags().StringVar(&alias, "alias", "default", "account alias")
	return cmd
}

// resolveLoginCookies builds the cookie map for `auth login` from whichever
// input source was given, in priority order: --cookies-file, --cookies,
// then --li-at/--jsessionid (prompting for any still missing unless
// --no-input is set).
func resolveLoginCookies(cookiesFlag, cookiesFile, liAt, jsession string, noInput bool) (map[string]string, error) {
	switch {
	case cookiesFile != "":
		b, err := os.ReadFile(cookiesFile)
		if err != nil {
			return nil, fmt.Errorf("read cookies file: %w", err)
		}
		return parseCookiesFile(string(b)), nil
	case cookiesFlag != "":
		return parseCookieHeader(cookiesFlag), nil
	}

	if liAt == "" {
		liAt = promptSecret("li_at cookie: ", noInput)
	}
	if jsession == "" {
		jsession = promptSecret("JSESSIONID cookie: ", noInput)
	}
	liAt = strings.TrimSpace(liAt)
	// JSESSIONID's cookie value carries literal surrounding quotes on the
	// wire (transport.go's cookieHeader relies on this); re-wrap after
	// trimming whatever the user pasted, so --jsessionid works whether or
	// not they included the quotes themselves.
	jsession = strings.Trim(strings.TrimSpace(jsession), `"`)
	cookies := map[string]string{"li_at": liAt}
	if jsession != "" {
		cookies["JSESSIONID"] = `"` + jsession + `"`
	}
	return cookies, nil
}

// parseCookieHeader parses a `Cookie:` header value (e.g. `li_at=..; ` +
// `JSESSIONID="ajax:..."; bcookie=..; lidc=..`) into a name->value map.
// Values are kept verbatim, including any quotes — LinkedIn's JSESSIONID
// cookie is quoted on the wire and the csrf-token header is derived from it
// by stripping them (see voyager.Client.New).
func parseCookieHeader(header string) map[string]string {
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
// name->value map.
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
		name := strings.TrimSpace(fields[5])
		if name == "" {
			continue
		}
		cookies[name] = strings.TrimSpace(fields[6])
	}
	return cookies
}

func newAuthStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show the active account and validate the session",
		RunE: func(cmd *cobra.Command, _ []string) error {
			app := appFrom(cmd)
			creds, def, err := auth.List()
			if err != nil {
				return err
			}
			if len(creds) == 0 {
				return auth.ErrNoAccount
			}
			r := app.Renderer()
			if app.Cfg.JSON {
				type row struct {
					Alias   string `json:"alias"`
					Name    string `json:"name"`
					Default bool   `json:"default"`
				}
				rows := make([]row, 0, len(creds))
				for _, c := range creds {
					rows = append(rows, row{c.Alias, c.Name, c.Alias == def})
				}
				return r.Emit(rows)
			}
			t := &output.Table{Cols: []string{"ALIAS", "NAME", "DEFAULT"}}
			for _, c := range creds {
				d := ""
				if c.Alias == def {
					d = "*"
				}
				t.Rows = append(t.Rows, []string{c.Alias, c.Name, d})
			}
			return r.Emit(t)
		},
	}
}

func newAuthLogoutCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "logout [alias]",
		Short: "Remove a stored session",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			app := appFrom(cmd)
			alias := app.Cfg.Account
			if len(args) == 1 {
				alias = args[0]
			}
			if alias == "" {
				alias = "default"
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
