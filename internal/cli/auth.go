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
	var liAt, jsession, alias string
	cmd := &cobra.Command{
		Use:   "login",
		Short: "Store a LinkedIn session (li_at + JSESSIONID cookies)",
		Long: "Store the browser session cookies lion uses to call LinkedIn's " +
			"Voyager API.\n\nGet them from a logged-in browser: DevTools → " +
			"Application → Cookies → linkedin.com → copy the `li_at` and " +
			"`JSESSIONID` values. Provide via --li-at/--jsessionid or paste when prompted.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			app := appFrom(cmd)
			if liAt == "" {
				liAt = promptSecret("li_at cookie: ", app.Cfg.NoInput)
			}
			if jsession == "" {
				jsession = promptSecret("JSESSIONID cookie: ", app.Cfg.NoInput)
			}
			liAt, jsession = strings.TrimSpace(liAt), strings.Trim(strings.TrimSpace(jsession), `"`)
			if liAt == "" || jsession == "" {
				return usageErr("both li_at and JSESSIONID are required")
			}

			// Validate the session before saving so we never store dead cookies.
			cl := voyager.New(liAt, jsession)
			me, err := cl.Me(context.Background())
			if err != nil {
				return fmt.Errorf("validate session: %w", err)
			}
			cred := &auth.Credential{
				Alias:      firstNonEmpty(alias, "default"),
				LiAt:       liAt,
				JSessionID: jsession,
				MemberID:   me.URN,
				Name:       me.Name(),
				SavedAt:    time.Now(),
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
	cmd.Flags().StringVar(&alias, "alias", "default", "account alias")
	return cmd
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
