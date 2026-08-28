// Package cli's bookmark.go implements `lion bookmark list`: the posts saved
// under LinkedIn's "My Items → Saved posts".
//
// Read-only with respect to LinkedIn — it never saves, unsaves, or mutates
// anything there — so it works under --readonly and never prompts.
package cli

import (
	"context"

	"github.com/jodok/lion/internal/output"
	"github.com/spf13/cobra"
)

func init() { registerCommand(newBookmarkCmd) }

func newBookmarkCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "bookmark",
		Short: "Work with posts you've saved on LinkedIn",
	}
	cmd.AddCommand(newBookmarkListCmd())
	return cmd
}

func newBookmarkListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List saved posts, most recently saved first",
		Long: "List the posts saved under LinkedIn's My Items → Saved posts.\n\n" +
			"Read-only with respect to LinkedIn: it never saves, unsaves, or " +
			"mutates anything there, so it works under --readonly and never " +
			"prompts.\n\n" +
			"A post that embeds an article, document, or video also reports what " +
			"it links to. Fetching that linked content is a separate step — this " +
			"command talks only to LinkedIn.",
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			app := appFrom(cmd)
			cl, err := app.Client()
			if err != nil {
				return err
			}
			posts, err := cl.SavedPosts(context.Background(), app.Cfg.Max)
			if err != nil {
				return err
			}
			r := app.Renderer()
			if app.Cfg.JSON {
				return r.Emit(posts)
			}
			t := &output.Table{Cols: []string{"URN", "AUTHOR", "LINKED", "SUMMARY"}}
			for _, p := range posts {
				// Free text from LinkedIn goes through the untrusted wrapper
				// like every other rendered field (see untrusted.go).
				t.Rows = append(t.Rows, []string{
					p.URN,
					r.Untrusted(p.Author),
					r.Untrusted(p.LinkedTitle),
					r.Untrusted(p.Summary),
				})
			}
			return r.Emit(t)
		},
	}
}
