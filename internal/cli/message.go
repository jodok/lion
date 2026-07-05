package cli

import (
	"context"
	"strconv"
	"strings"

	"github.com/jodok/lion/internal/output"
	"github.com/spf13/cobra"
)

// Verticals self-register so adding one never requires editing root.go.
func init() { registerCommand(newMessageCmd) }

func newMessageCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "message",
		Short: "Read and send LinkedIn messages",
	}
	cmd.AddCommand(newMessageListCmd(), newMessageReadCmd(), newMessageSendCmd())
	return cmd
}

func newMessageListCmd() *cobra.Command {
	var unread bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List messaging conversations",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			app := appFrom(cmd)
			cl, err := app.Client()
			if err != nil {
				return err
			}
			convs, err := cl.Conversations(context.Background(), unread, app.Cfg.Max)
			if err != nil {
				return err
			}
			r := app.Renderer()
			if app.Cfg.JSON {
				return r.Emit(convs)
			}
			t := &output.Table{Cols: []string{"URN", "PARTICIPANTS", "UNREAD", "LAST_MESSAGE"}}
			for _, cv := range convs {
				t.Rows = append(t.Rows, []string{
					cv.URN,
					strings.Join(cv.Participants, ", "),
					strconv.FormatBool(cv.Unread),
					r.Untrusted(cv.LastMessage),
				})
			}
			return r.Emit(t)
		},
	}
	cmd.Flags().BoolVar(&unread, "unread", false, "only show conversations with unread messages")
	return cmd
}

func newMessageReadCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "read <conversation-id>",
		Short: "Show messages in a conversation",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			app := appFrom(cmd)
			cl, err := app.Client()
			if err != nil {
				return err
			}
			msgs, err := cl.Messages(context.Background(), args[0], app.Cfg.Max)
			if err != nil {
				return err
			}
			r := app.Renderer()
			if app.Cfg.JSON {
				return r.Emit(msgs)
			}
			t := &output.Table{Cols: []string{"FROM", "SENT_AT", "TEXT"}}
			for _, m := range msgs {
				t.Rows = append(t.Rows, []string{
					m.From,
					strconv.FormatInt(m.SentAt, 10),
					r.Untrusted(m.Text),
				})
			}
			return r.Emit(t)
		},
	}
}

func newMessageSendCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "send <id|conversation> <text...>",
		Short: "Send a message to a person or an existing conversation",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			app := appFrom(cmd)
			if err := app.requireWritable(); err != nil {
				return err
			}
			cl, err := app.Client()
			if err != nil {
				return err
			}
			target := args[0]
			text := joinArgs(args[1:])
			ctx := context.Background()

			// A conversation id/URN identifies an existing thread. Anything
			// else is treated as a recipient and starts (or reuses) a new
			// thread: a bare public id (e.g. "ada-lovelace") is resolved to
			// its profile URN first, since SendMessageToProfile needs a URN.
			if isConversationID(target) {
				err = cl.SendMessage(ctx, target, text)
			} else {
				recipient := target
				if !strings.HasPrefix(recipient, "urn:") {
					pr, perr := cl.Profile(ctx, recipient)
					if perr != nil {
						return perr
					}
					recipient = pr.URN
				}
				err = cl.SendMessageToProfile(ctx, recipient, text)
			}
			if err != nil {
				return err
			}

			r := app.Renderer()
			if cl.DryRun() {
				if app.Cfg.JSON {
					return r.Emit(map[string]string{"status": "dry-run", "target": target})
				}
				return r.Emit(&output.Table{Cols: []string{"STATUS", "TARGET"}, Rows: [][]string{{"dry-run", target}}})
			}
			if app.Cfg.JSON {
				return r.Emit(map[string]string{"status": "sent", "target": target})
			}
			return r.Emit(&output.Table{Cols: []string{"STATUS", "TARGET"}, Rows: [][]string{{"sent", target}}})
		},
	}
}

// isConversationID reports whether id looks like an existing conversation
// identifier (a fs_conversation URN, or the raw id copied from `message
// list` output) rather than a person's public id or profile URN.
func isConversationID(id string) bool {
	return strings.Contains(id, "fs_conversation") || strings.HasPrefix(id, "2-")
}
