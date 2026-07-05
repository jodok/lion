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
		Use:   "view [id|me]",
		Short: "View a profile by public id (default: your own)",
		Args:  cobra.MaximumNArgs(1),
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
		Args:  cobra.MinimumNArgs(1),
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
			if app.Cfg.JSON {
				return r.Emit(results)
			}
			t := &output.Table{Cols: []string{"PUBLIC_ID", "NAME", "HEADLINE"}}
			for _, res := range results {
				t.Rows = append(t.Rows, []string{res.PublicID, res.Name, r.Untrusted(res.Headline)})
			}
			return r.Emit(t)
		},
	}
	cmd.Flags().StringVar(&title, "title", "", "filter by title/keyword")
	cmd.Flags().StringVar(&company, "company", "", "filter by company")
	cmd.Flags().StringVar(&location, "location", "", "filter by location")
	return cmd
}

func renderProfile(r *output.Renderer, app *App, publicID, name, headline, location, industry, summary string) error {
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
		{"headline", r.Untrusted(headline)},
		{"location", location},
		{"industry", industry},
	}
	if summary != "" {
		t.Rows = append(t.Rows, []string{"summary", r.Untrusted(summary)})
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
