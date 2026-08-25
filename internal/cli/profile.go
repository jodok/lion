package cli

import (
	"context"

	"github.com/jodok/lion/internal/output"
	"github.com/spf13/cobra"
)

// Verticals self-register so adding one never requires editing root.go.
func init() { registerCommand(newProfileCmd) }

func newProfileCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "profile",
		Short: "View and search LinkedIn profiles",
	}
	cmd.AddCommand(newProfileViewCmd(), newProfileSearchCmd())
	return cmd
}

func newProfileViewCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "view [me]",
		Short: "View your own profile",
		Long: "View your own profile.\n\nViewing someone else's profile by " +
			"public id is not supported in this build: LinkedIn retired the " +
			"REST profileView endpoint (HTTP 410) and its modern replacement " +
			"is a set of GraphQL cards this version doesn't model yet — see " +
			"DESIGN.md §3.2. Use `lion profile search` to look people up in " +
			"the meantime.",
		Args: usageArgs(cobra.MaximumNArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			app := appFrom(cmd)
			cl, err := app.Client()
			if err != nil {
				return err
			}
			ctx := context.Background()
			id := "me"
			if len(args) == 1 {
				id = args[0]
			}

			r := app.Renderer()
			if id == "me" {
				me, err := cl.Me(ctx)
				if err != nil {
					return err
				}
				return renderProfile(r, app, me.PublicID, me.Name(), me.Headline, me.Location, me.Industry, me.Summary)
			}
			pr, err := cl.Profile(ctx, id)
			if err != nil {
				return err
			}
			return renderProfile(r, app, pr.PublicID, pr.Name(), pr.Headline, pr.Location, pr.Industry, pr.Summary)
		},
	}
}

func newProfileSearchCmd() *cobra.Command {
	var title, company, location string
	cmd := &cobra.Command{
		Use:   "search <query>",
		Short: "Search people",
		Args:  usageArgs(cobra.MinimumNArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			app := appFrom(cmd)
			cl, err := app.Client()
			if err != nil {
				return err
			}
			query := joinArgs(args)
			// Simple filter hints appended to the keyword query.
			for _, f := range []string{title, company, location} {
				if f != "" {
					query += " " + f
				}
			}
			results, err := cl.SearchPeople(context.Background(), query, app.Cfg.Max)
			if err != nil {
				return err
			}
			r := app.Renderer()
			// Wrap every LinkedIn-controlled free-text field (via
			// wrapSearchResult — see untrusted.go) once, here, so both the
			// JSON and table/plain branches below see the same (possibly
			// wrapped) values — this is what keeps --wrap-untrusted honored
			// consistently across output formats (F17) instead of only in
			// the table path, and covers the person's name and location as
			// well as their headline.
			for i := range results {
				results[i] = wrapSearchResult(r, results[i])
			}
			if app.Cfg.JSON {
				return r.Emit(results)
			}
			t := &output.Table{Cols: []string{"PUBLIC_ID", "NAME", "HEADLINE"}}
			for _, res := range results {
				t.Rows = append(t.Rows, []string{res.PublicID, res.Name, res.Headline})
			}
			return r.Emit(t)
		},
	}
	cmd.Flags().StringVar(&title, "title", "", "appended to the search keywords as a hint (not a strict filter — LinkedIn's structured search filters aren't modeled in v1)")
	cmd.Flags().StringVar(&company, "company", "", "appended to the search keywords as a hint (not a strict filter — LinkedIn's structured search filters aren't modeled in v1)")
	cmd.Flags().StringVar(&location, "location", "", "appended to the search keywords as a hint (not a strict filter — LinkedIn's structured search filters aren't modeled in v1)")
	return cmd
}

func renderProfile(r *output.Renderer, app *App, publicID, name, headline, location, industry, summary string) error {
	// Wrap every LinkedIn-controlled free-text field once, here, so it's
	// applied identically whichever branch below renders it (F17 —
	// --wrap-untrusted must not be JSON/table-format-specific, and must not
	// stop at headline/summary while leaving name/location/industry — the
	// member's own free-text description of themselves — unwrapped). Only
	// public_id is left alone: it's the machine identifier a script needs
	// verbatim. hadSummary is captured before wrapping since Untrusted("")
	// is no longer empty once wrapped.
	hadSummary := summary != ""
	name = r.Untrusted(name)
	headline = r.Untrusted(headline)
	location = r.Untrusted(location)
	industry = r.Untrusted(industry)
	summary = r.Untrusted(summary)
	if app.Cfg.JSON {
		return r.Emit(map[string]string{
			"public_id": publicID,
			"name":      name,
			"headline":  headline,
			"location":  location,
			"industry":  industry,
			"summary":   summary,
		})
	}
	t := &output.Table{Cols: []string{"FIELD", "VALUE"}}
	t.Rows = [][]string{
		{"public_id", publicID},
		{"name", name},
		{"headline", headline},
		{"location", location},
		{"industry", industry},
	}
	if hadSummary {
		t.Rows = append(t.Rows, []string{"summary", summary})
	}
	return r.Emit(t)
}

func joinArgs(args []string) string {
	out := ""
	for i, a := range args {
		if i > 0 {
			out += " "
		}
		out += a
	}
	return out
}
