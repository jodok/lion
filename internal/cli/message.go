package cli

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/jodok/lion/internal/output"
	"github.com/jodok/lion/internal/voyager"
	"github.com/spf13/cobra"
)

// Verticals self-register so adding one never requires editing root.go.
func init() { registerCommand(newMessageCmd) }

func newMessageCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "message",
		Short: "Read and send LinkedIn messages",
	}
	cmd.AddCommand(newMessageListCmd(), newMessageReadCmd(), newMessageSendCmd(), newMessageExportCmd(), newMessageSearchCmd())
	return cmd
}

func newMessageListCmd() *cobra.Command {
	var unread bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List messaging conversations",
		Args:  usageArgs(cobra.NoArgs),
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
			return renderConversations(app.Renderer(), app.Cfg.JSON, convs)
		},
	}
	cmd.Flags().BoolVar(&unread, "unread", false, "only show conversations with unread messages")
	return cmd
}

// conversationOutput is the exact JSON shape `lion message list` has shipped
// since v1.0.0: participants as a plain array of display-name strings.
// voyager.Conversation now pairs each participant with its MiniProfile URN
// internally (see Participant's doc comment), and the newer store/sync/
// export/history-coverage surfaces render that richer pair directly because
// they have no shipped contract to protect — but serializing
// voyager.Conversation straight to this command's JSON output would leak
// that internal shape change into a published contract and break every
// consumer reading .participants[0] as a string. Rendering through this
// explicit DTO instead means a future change to voyager.Conversation can't
// silently do that again. Do not add fields here: this type's shape is
// exactly v1.0.0's, byte for byte.
type conversationOutput struct {
	URN          string   `json:"urn"`
	Participants []string `json:"participants"`
	LastMessage  string   `json:"last_message"`
	UpdatedAt    int64    `json:"updated_at"`
	Unread       bool     `json:"unread"`
}

// renderConversations wraps every LinkedIn-controlled free-text field (via
// wrapConversation — see untrusted.go) once and renders it identically for
// every output format — JSON included — so --wrap-untrusted is honored
// consistently (F17), covering participant names as well as the last
// message preview.
func renderConversations(r *output.Renderer, jsonOut bool, convs []voyager.Conversation) error {
	for i := range convs {
		convs[i] = wrapConversation(r, convs[i])
	}
	if jsonOut {
		out := make([]conversationOutput, len(convs))
		for i, cv := range convs {
			out[i] = conversationOutput{
				URN:          cv.URN,
				Participants: participantNames(cv.Participants),
				LastMessage:  cv.LastMessage,
				UpdatedAt:    cv.UpdatedAt,
				Unread:       cv.Unread,
			}
		}
		return r.Emit(out)
	}
	t := &output.Table{Cols: []string{"URN", "PARTICIPANTS", "UNREAD", "LAST_MESSAGE"}}
	for _, cv := range convs {
		t.Rows = append(t.Rows, []string{
			cv.URN,
			strings.Join(participantNames(cv.Participants), ", "),
			strconv.FormatBool(cv.Unread),
			cv.LastMessage,
		})
	}
	return r.Emit(t)
}

// participantNames extracts the display names for both the table path and
// conversationOutput's JSON.
//
// Participants whose MiniProfile didn't resolve carry an empty Name, and
// v1.0.0 never emitted those: it only appended a name once it had one, so the
// published shape is "the names we know", not "one slot per participant, some
// blank". Passing the blanks through would be a second silent change to the
// very contract conversationOutput exists to protect, from the same root
// cause — so they're skipped here, at the one place both paths share.
func participantNames(participants []voyager.Participant) []string {
	names := make([]string, 0, len(participants))
	for _, p := range participants {
		if p.Name == "" {
			continue
		}
		names = append(names, p.Name)
	}
	return names
}

func newMessageReadCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "read <conversation-id>",
		Short: "Show messages in a conversation",
		Args:  usageArgs(cobra.ExactArgs(1)),
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
			return renderMessages(app.Renderer(), app.Cfg.JSON, msgs)
		},
	}
}

// renderMessages wraps every LinkedIn-controlled free-text field (via
// wrapMessage) once and renders it identically for every output format
// (F17 — see renderConversations), covering the sender name as well as the
// message body.
func renderMessages(r *output.Renderer, jsonOut bool, msgs []voyager.Message) error {
	for i := range msgs {
		msgs[i] = wrapMessage(r, msgs[i])
	}
	if jsonOut {
		return r.Emit(msgs)
	}
	t := &output.Table{Cols: []string{"FROM", "SENT_AT", "TEXT"}}
	for _, m := range msgs {
		t.Rows = append(t.Rows, []string{
			m.From,
			strconv.FormatInt(m.SentAt, 10),
			m.Text,
		})
	}
	return r.Emit(t)
}

func newMessageSendCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "send <conversation-id|profile-urn> <text...>",
		Short: "Send a message to a conversation or a profile URN",
		Long: "Send a message to an existing conversation, or to a person by " +
			"their profile URN.\n\nv1 does not accept a bare person id " +
			"(e.g. \"ada-lovelace\"): resolving one to a profile URN needs " +
			"profile-by-id, which LinkedIn's modern API doesn't support in " +
			"this build (the legacy endpoint returns HTTP 410 — see " +
			"DESIGN.md §3.2). Copy a conversation id from `lion message " +
			"list`, or pass the person's urn:li:fs_miniProfile:... URN, " +
			"which is sent to directly.",
		Args: usageArgs(cobra.MinimumNArgs(2)),
		RunE: func(cmd *cobra.Command, args []string) error {
			app := appFrom(cmd)
			if err := app.requireWritable(); err != nil {
				return err
			}
			target := args[0]
			text := joinArgs(args[1:])

			// F3: person-id targeting depends on Client.Profile, which is
			// unsupported (LinkedIn 410s the legacy profileView endpoint;
			// the modern replacement isn't modeled in v1 — see
			// DESIGN.md §3.2). Rather than attempt it and fail with a
			// confusing downstream error, reject it here with a clear,
			// actionable message before even building a client.
			//
			// Only a *bare* person id needs that lookup, though. A profile URN
			// is already what SendMessageToProfile wants, so it goes straight
			// through — rejecting it too would break a path that works.
			isConversation := isConversationID(target)
			isProfileURN := !isConversation && strings.HasPrefix(target, "urn:")
			if !isConversation && !isProfileURN {
				return usageErr("message send needs a conversation id or a profile URN in this build (got %q, a bare person id); resolving a person id to its profile URN needs profile-by-id, which LinkedIn's modern API doesn't support (see DESIGN.md §3.2) — copy a conversation id from `lion message list`, or pass the person's urn:li:fs_miniProfile:... URN", target)
			}

			cl, err := app.Client()
			if err != nil {
				return err
			}
			r := app.Renderer()

			if cl.DryRun() {
				// Validate through the real mutation first — see feed post.
				// It rejects empty text and an empty target, then returns
				// before any network call, so a preview only appears for a
				// request the live run would actually attempt.
				var vErr error
				if isProfileURN {
					vErr = cl.SendMessageToProfile(context.Background(), target, text)
				} else {
					vErr = cl.SendMessage(context.Background(), target, text)
				}
				if vErr != nil {
					return vErr
				}
				if app.Cfg.JSON {
					return r.Emit(map[string]string{"status": "dry-run", "action": "message.send", "target": target, "body": text})
				}
				return r.Emit(&output.Table{Cols: []string{"STATUS", "TARGET", "BODY"}, Rows: [][]string{{"dry-run", target, text}}})
			}

			noun := "conversation"
			if isProfileURN {
				noun = "profile"
			}
			ok, err := app.confirm(fmt.Sprintf("About to send a message to %s %s. Proceed?", noun, target))
			if err != nil {
				return err
			}
			if !ok {
				fmt.Fprintln(os.Stderr, "aborted: message not sent")
				return nil
			}

			ctx := context.Background()
			var sendErr error
			if isProfileURN {
				sendErr = cl.SendMessageToProfile(ctx, target, text)
			} else {
				sendErr = cl.SendMessage(ctx, target, text)
			}
			if sendErr != nil {
				return sendErr
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
