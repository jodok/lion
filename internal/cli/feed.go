package cli

import (
	"context"
	"fmt"
	"os"

	"github.com/jodok/lion/internal/output"
	"github.com/jodok/lion/internal/voyager"
	"github.com/spf13/cobra"
)

// Verticals self-register so adding one never requires editing root.go.
func init() { registerCommand(newFeedCmd) }

func newFeedCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "feed",
		Short: "Read and interact with the LinkedIn feed",
	}
	cmd.AddCommand(
		newFeedReadCmd(),
		newFeedPostCmd(),
		newFeedCommentCmd(),
		newFeedReactCmd(),
		newFeedEngagementCmd(),
	)
	return cmd
}

func newFeedReadCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "read",
		Short: "Read the chronological feed",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			app := appFrom(cmd)
			cl, err := app.Client()
			if err != nil {
				return err
			}
			items, err := cl.Feed(context.Background(), app.Cfg.Max)
			if err != nil {
				return err
			}
			return renderFeedItems(app.Renderer(), app.Cfg.JSON, items)
		},
	}
}

// renderFeedItems wraps free text captured from LinkedIn once and renders
// it identically for every output format — JSON included — so
// --wrap-untrusted is honored consistently (F17).
func renderFeedItems(r *output.Renderer, jsonOut bool, items []voyager.FeedItem) error {
	for i := range items {
		items[i].Text = r.Untrusted(items[i].Text)
	}
	if jsonOut {
		return r.Emit(items)
	}
	t := &output.Table{Cols: []string{"URN", "AUTHOR", "TEXT", "LIKES", "COMMENTS"}}
	for _, it := range items {
		t.Rows = append(t.Rows, []string{
			it.URN,
			it.AuthorName,
			it.Text,
			fmt.Sprintf("%d", it.Likes),
			fmt.Sprintf("%d", it.Comments),
		})
	}
	return r.Emit(t)
}

func newFeedPostCmd() *cobra.Command {
	var visibility string
	cmd := &cobra.Command{
		Use:   "post <text...>",
		Short: "Publish a text post to the feed",
		Args:  usageArgs(cobra.MinimumNArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			app := appFrom(cmd)
			if err := app.requireWritable(); err != nil {
				return err
			}
			cl, err := app.Client()
			if err != nil {
				return err
			}
			text := joinArgs(args)
			r := app.Renderer()

			if cl.DryRun() {
				if app.Cfg.JSON {
					return r.Emit(map[string]string{"status": "dry-run", "action": "feed.post", "visibility": visibility, "text": text})
				}
				return r.Emit(&output.Table{Cols: []string{"STATUS", "VISIBILITY", "TEXT"}, Rows: [][]string{{"dry-run", visibility, text}}})
			}

			ok, err := app.confirm(fmt.Sprintf("About to publish a %s post. Proceed?", visibility))
			if err != nil {
				return err
			}
			if !ok {
				fmt.Fprintln(os.Stderr, "aborted: post not published")
				return nil
			}

			if err := cl.CreatePost(context.Background(), text, visibility); err != nil {
				return err
			}
			if app.Cfg.JSON {
				return r.Emit(map[string]string{"status": "posted", "visibility": visibility})
			}
			t := &output.Table{Cols: []string{"STATUS", "VISIBILITY"}}
			t.Rows = [][]string{{"posted", visibility}}
			return r.Emit(t)
		},
	}
	cmd.Flags().StringVar(&visibility, "visibility", "connections", "post visibility: connections|public")
	return cmd
}

func newFeedCommentCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "comment <urn> <text...>",
		Short: "Comment on a feed object",
		Args:  usageArgs(cobra.MinimumNArgs(2)),
		RunE: func(cmd *cobra.Command, args []string) error {
			app := appFrom(cmd)
			if err := app.requireWritable(); err != nil {
				return err
			}
			cl, err := app.Client()
			if err != nil {
				return err
			}
			urn := args[0]
			text := joinArgs(args[1:])
			r := app.Renderer()

			if cl.DryRun() {
				if app.Cfg.JSON {
					return r.Emit(map[string]string{"status": "dry-run", "action": "feed.comment", "urn": urn, "text": text})
				}
				return r.Emit(&output.Table{Cols: []string{"STATUS", "URN", "TEXT"}, Rows: [][]string{{"dry-run", urn, text}}})
			}

			ok, err := app.confirm(fmt.Sprintf("About to comment on %s. Proceed?", urn))
			if err != nil {
				return err
			}
			if !ok {
				fmt.Fprintln(os.Stderr, "aborted: comment not posted")
				return nil
			}

			if err := cl.Comment(context.Background(), urn, text); err != nil {
				return err
			}
			if app.Cfg.JSON {
				return r.Emit(map[string]string{"status": "commented", "urn": urn})
			}
			t := &output.Table{Cols: []string{"STATUS", "URN"}}
			t.Rows = [][]string{{"commented", urn}}
			return r.Emit(t)
		},
	}
}

func newFeedReactCmd() *cobra.Command {
	var reactionType string
	cmd := &cobra.Command{
		Use:   "react <urn>",
		Short: "React to a feed object",
		Args:  usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			app := appFrom(cmd)
			if err := app.requireWritable(); err != nil {
				return err
			}
			cl, err := app.Client()
			if err != nil {
				return err
			}
			urn := args[0]
			r := app.Renderer()

			if cl.DryRun() {
				if app.Cfg.JSON {
					return r.Emit(map[string]string{"status": "dry-run", "action": "feed.react", "urn": urn, "type": reactionType})
				}
				return r.Emit(&output.Table{Cols: []string{"STATUS", "URN", "TYPE"}, Rows: [][]string{{"dry-run", urn, reactionType}}})
			}

			ok, err := app.confirm(fmt.Sprintf("About to react (%s) to %s. Proceed?", reactionType, urn))
			if err != nil {
				return err
			}
			if !ok {
				fmt.Fprintln(os.Stderr, "aborted: reaction not sent")
				return nil
			}

			if err := cl.React(context.Background(), urn, reactionType); err != nil {
				return err
			}
			if app.Cfg.JSON {
				return r.Emit(map[string]string{"status": "reacted", "urn": urn, "type": reactionType})
			}
			t := &output.Table{Cols: []string{"STATUS", "URN", "TYPE"}}
			t.Rows = [][]string{{"reacted", urn, reactionType}}
			return r.Emit(t)
		},
	}
	cmd.Flags().StringVar(&reactionType, "type", "like", "reaction type: like|celebrate|support|love|insightful|funny")
	return cmd
}

func newFeedEngagementCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "engagement <urn>",
		Short: "Show like/comment counts for a feed object",
		Args:  usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			app := appFrom(cmd)
			cl, err := app.Client()
			if err != nil {
				return err
			}
			urn := args[0]
			likes, comments, err := cl.Engagement(context.Background(), urn)
			if err != nil {
				return err
			}
			r := app.Renderer()
			if app.Cfg.JSON {
				return r.Emit(map[string]int{"likes": likes, "comments": comments})
			}
			t := &output.Table{Cols: []string{"LIKES", "COMMENTS"}}
			t.Rows = [][]string{{fmt.Sprintf("%d", likes), fmt.Sprintf("%d", comments)}}
			return r.Emit(t)
		},
	}
}
