package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jodok/lion/internal/output"
	"github.com/jodok/lion/internal/store"
	"github.com/spf13/cobra"
)

// newMessageExportCmd builds `lion message export`, added as a subcommand
// from newMessageCmd (message.go) rather than self-registered — it's part
// of the message vertical, not its own top-level command.
func newMessageExportCmd() *cobra.Command {
	var (
		conversationID string
		after, before  string
		limit          int
		format         string
		outputPath     string
		storePath      string
	)
	cmd := &cobra.Command{
		Use:   "export",
		Short: "Export synced messages from the local store",
		Long: "Export reads lion's local message store ($LION_HOME/store.db, " +
			"populated by `lion sync`) and never touches the network — it " +
			"works offline, and under any rate-limit state, because it makes " +
			"no LinkedIn request at all. Run `lion sync` first to populate " +
			"the store.\n\n" +
			"With no --output, the export streams to stdout so it pipes. A " +
			"--output naming a directory writes conversations.jsonl plus " +
			"one messages/<conversation-id>.jsonl per conversation " +
			"(--format is ignored in this mode: a directory of JSON Lines " +
			"files is the layout, not a choice between two of them). A " +
			"--output naming a file writes a single document there in " +
			"--format.",
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			if format != "json" && format != "jsonl" {
				return usageErr("--format must be json or jsonl, got %q", format)
			}

			var afterMs, beforeMs *int64
			if after != "" {
				ms, err := parseTimeFlag("after", after)
				if err != nil {
					return err
				}
				afterMs = &ms
			}
			if before != "" {
				ms, err := parseTimeFlag("before", before)
				if err != nil {
					return err
				}
				beforeMs = &ms
			}
			if limit < 0 {
				return usageErr("--limit must not be negative")
			}

			app := appFrom(cmd)

			path := storePath
			if path == "" {
				var err error
				path, err = store.DefaultPath()
				if err != nil {
					return err
				}
			}
			st, err := store.Open(path)
			if err != nil {
				return err
			}
			defer st.Close()

			ctx := context.Background()

			// An empty result must never look like success (it reads
			// identically to "you have no messages" otherwise): refuse
			// before writing anything, on both of the two ways an export
			// can come back with nothing.
			empty, err := st.Empty(ctx)
			if err != nil {
				return err
			}
			if empty {
				fmt.Fprintln(os.Stderr, "the local message store is empty; run `lion sync` first")
				return fmt.Errorf("nothing to export: the local store is empty")
			}

			if conversationID != "" {
				if _, ok, err := st.Conversation(ctx, conversationID); err != nil {
					return err
				} else if !ok {
					return fmt.Errorf("conversation %q not found in the store", conversationID)
				}
			}

			msgs, err := st.Messages(ctx, store.MessageFilter{
				ConversationID: conversationID,
				After:          afterMs,
				Before:         beforeMs,
				Limit:          limit,
			})
			if err != nil {
				return err
			}
			if len(msgs) == 0 {
				fmt.Fprintln(os.Stderr, "no messages match the given export filters (--conversation/--after/--before)")
				return fmt.Errorf("nothing to export: no messages match the given filters")
			}

			groups := groupMessagesByConversation(msgs)
			for _, g := range groups {
				conv, ok, err := st.Conversation(ctx, g.id)
				if err != nil {
					return err
				}
				if ok {
					g.meta, g.hasMeta = conv, true
				}
			}

			if outputPath == "" {
				// The stream itself is stdout's data (DESIGN.md §2.3) — no
				// separate summary is printed here, unlike the --output
				// cases below, since printing one would corrupt the piped
				// stream.
				return writeExportStream(os.Stdout, format, groups)
			}

			if isDirTarget(outputPath) {
				if err := writeExportDirectory(outputPath, groups); err != nil {
					return err
				}
			} else {
				if err := writeExportFile(outputPath, format, groups); err != nil {
					return err
				}
			}

			// Bare JSON, no wacli-style {"success":...,"data":...} envelope
			// — see sync.go's identical note on emitSyncSummary for why.
			r := app.Renderer()
			if app.Cfg.JSON {
				return r.Emit(map[string]any{
					"status":        "exported",
					"conversations": len(groups),
					"messages":      len(msgs),
					"path":          outputPath,
				})
			}
			return r.Emit(&output.Table{
				Cols: []string{"STATUS", "CONVERSATIONS", "MESSAGES", "PATH"},
				Rows: [][]string{{"exported", fmt.Sprintf("%d", len(groups)), fmt.Sprintf("%d", len(msgs)), outputPath}},
			})
		},
	}

	cmd.Flags().StringVar(&conversationID, "conversation", "", "restrict the export to one conversation id")
	cmd.Flags().StringVar(&after, "after", "", "only export messages at or after this time (RFC3339 or YYYY-MM-DD)")
	cmd.Flags().StringVar(&before, "before", "", "only export messages at or before this time (RFC3339 or YYYY-MM-DD)")
	cmd.Flags().IntVar(&limit, "limit", 0, "cap the export to the most recent N messages matching the other filters (0 = no cap)")
	cmd.Flags().StringVar(&format, "format", "json", "json (one document, for small exports) or jsonl (one message per line, for large/streaming exports)")
	cmd.Flags().StringVar(&outputPath, "output", "", "write the export here (a file, or a directory for the per-conversation layout); omit to stream to stdout")
	cmd.Flags().StringVar(&storePath, "store", "", "path to the store database (default $LION_HOME/store.db)")
	return cmd
}

// exportGroup collects one conversation's exported messages, in the order
// they first appear in the overall oldest-first result — see
// groupMessagesByConversation.
type exportGroup struct {
	id       string
	meta     store.Conversation
	hasMeta  bool
	messages []store.Message
}

// groupMessagesByConversation splits a flat, oldest-first message slice
// (as returned by store.Messages) into per-conversation groups, preserving
// each message's relative order within its group and each group's order of
// first appearance in the flat slice. This is what lets a single
// store.Messages query — which already implements --limit's "most recent N
// overall" selection — serve both the flat jsonl stream and the grouped
// json-envelope/directory layouts without querying twice.
func groupMessagesByConversation(msgs []store.Message) []*exportGroup {
	var order []string
	byID := map[string]*exportGroup{}
	for _, m := range msgs {
		g, ok := byID[m.ConversationID]
		if !ok {
			g = &exportGroup{id: m.ConversationID}
			byID[m.ConversationID] = g
			order = append(order, m.ConversationID)
		}
		g.messages = append(g.messages, m)
	}
	out := make([]*exportGroup, len(order))
	for i, id := range order {
		out[i] = byID[id]
	}
	return out
}

// exportedMessage is the on-disk/on-wire shape of one exported message.
//
// Deliberately NOT run through output.Renderer.Untrusted / --wrap-untrusted
// (contrast wrapMessage in untrusted.go, used by every other command that
// renders a message body): an export is an archive meant to round-trip back
// into something that reads like the original conversation, and
// --wrap-untrusted's nonce-delimited boundaries are a presentation wrapper,
// not part of the data. Applying it here would corrupt every exported body
// with delimiters no consumer of the archive asked for or expects to strip.
type exportedMessage struct {
	ConversationID string `json:"conversation_id"`
	URN            string `json:"urn"`
	SenderName     string `json:"sender_name"`
	SenderURN      string `json:"sender_urn,omitempty"`
	SentAt         int64  `json:"sent_at"`
	Body           string `json:"body"`
}

func toExportedMessage(m store.Message) exportedMessage {
	return exportedMessage{
		ConversationID: m.ConversationID,
		URN:            m.URN,
		SenderName:     m.SenderName,
		SenderURN:      m.SenderURN,
		SentAt:         m.SentAt,
		Body:           m.Body,
	}
}

// exportedConversationMeta is a conversation's identity/metadata in an
// export, without its messages (used standalone in conversations.jsonl, and
// nested with Messages in the json-envelope format below).
type exportedConversationMeta struct {
	ID           string              `json:"id"`
	URN          string              `json:"urn,omitempty"`
	Participants []store.Participant `json:"participants,omitempty"`
	UpdatedAt    int64               `json:"updated_at,omitempty"`
}

func toExportedConversationMeta(g *exportGroup) exportedConversationMeta {
	meta := exportedConversationMeta{ID: g.id}
	if g.hasMeta {
		meta.URN = g.meta.URN
		meta.Participants = g.meta.Participants
		meta.UpdatedAt = g.meta.UpdatedAt
	}
	return meta
}

// exportEnvelope is the --format json document: everything in one value,
// meant for small exports someone wants to read, diff, or pass whole to
// another tool. exportedAt records when the export ran (distinct from any
// message or sync timestamp) since the archive itself has no other record
// of that.
type exportEnvelope struct {
	ExportedAt    string                       `json:"exported_at"`
	Conversations []exportEnvelopeConversation `json:"conversations"`
}

type exportEnvelopeConversation struct {
	exportedConversationMeta
	Messages []exportedMessage `json:"messages"`
}

func buildEnvelope(groups []*exportGroup) exportEnvelope {
	env := exportEnvelope{
		ExportedAt:    time.Now().UTC().Format(time.RFC3339),
		Conversations: make([]exportEnvelopeConversation, len(groups)),
	}
	for i, g := range groups {
		msgs := make([]exportedMessage, len(g.messages))
		for j, m := range g.messages {
			msgs[j] = toExportedMessage(m)
		}
		env.Conversations[i] = exportEnvelopeConversation{
			exportedConversationMeta: toExportedConversationMeta(g),
			Messages:                 msgs,
		}
	}
	return env
}

// writeExportStream writes groups to w in format: one message-per-line for
// jsonl (messages ordered oldest-first overall, matching store.Messages),
// or one exportEnvelope document for json.
func writeExportStream(w io.Writer, format string, groups []*exportGroup) error {
	if format == "jsonl" {
		enc := json.NewEncoder(w)
		for _, g := range groups {
			for _, m := range g.messages {
				if err := enc.Encode(toExportedMessage(m)); err != nil {
					return err
				}
			}
		}
		return nil
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(buildEnvelope(groups))
}

// writeExportFile writes a single-document export to path (a file, not a
// directory), creating its parent directories 0700 and the file itself
// 0600 — this is a complete copy of someone's private messages.
func writeExportFile(path, format string, groups []*exportGroup) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("export: create %s: %w", filepath.Dir(path), err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("export: open %s: %w", path, err)
	}
	// os.OpenFile's mode is subject to umask, so an explicit Chmod is what
	// actually guarantees 0600 rather than something looser.
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	if err := writeExportStream(f, format, groups); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// writeExportDirectory writes the per-conversation layout: conversations.jsonl
// (one line of metadata per conversation) plus messages/<id>.jsonl (one line
// per message) for each conversation present in groups. Always JSON Lines
// regardless of --format — see newMessageExportCmd's Long help for why a
// directory of files doesn't have a "json envelope" equivalent worth
// building.
func writeExportDirectory(dir string, groups []*exportGroup) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("export: create %s: %w", dir, err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	messagesDir := filepath.Join(dir, "messages")
	if err := os.MkdirAll(messagesDir, 0o700); err != nil {
		return fmt.Errorf("export: create %s: %w", messagesDir, err)
	}
	if err := os.Chmod(messagesDir, 0o700); err != nil {
		return err
	}

	cf, err := os.OpenFile(filepath.Join(dir, "conversations.jsonl"), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if err := cf.Chmod(0o600); err != nil {
		cf.Close()
		return err
	}
	convEnc := json.NewEncoder(cf)

	for _, g := range groups {
		if err := convEnc.Encode(toExportedConversationMeta(g)); err != nil {
			cf.Close()
			return err
		}

		msgPath := filepath.Join(messagesDir, sanitizeConversationFilename(g.id)+".jsonl")
		mf, err := os.OpenFile(msgPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
		if err != nil {
			cf.Close()
			return err
		}
		if err := mf.Chmod(0o600); err != nil {
			mf.Close()
			cf.Close()
			return err
		}
		menc := json.NewEncoder(mf)
		for _, m := range g.messages {
			if err := menc.Encode(toExportedMessage(m)); err != nil {
				mf.Close()
				cf.Close()
				return err
			}
		}
		if err := mf.Close(); err != nil {
			cf.Close()
			return err
		}
	}
	return cf.Close()
}

// isDirTarget reports whether path should be treated as a directory
// (existing directory, or a path ending in a separator so an intent to
// create one is explicit) rather than a single output file.
func isDirTarget(path string) bool {
	if strings.HasSuffix(path, string(os.PathSeparator)) {
		return true
	}
	fi, err := os.Stat(path)
	return err == nil && fi.IsDir()
}

// sanitizeConversationFilename neutralizes path separators in a
// conversation id before it becomes part of a filename. Conversation ids
// are opaque strings lion parsed out of a LinkedIn URN (see
// voyager.Conversation.ID) rather than user input, but treating anything
// that ends up on a filesystem path as untrusted by default is cheap
// insurance against a malformed or unexpected id being read as a path
// traversal instead of a filename.
func sanitizeConversationFilename(id string) string {
	r := strings.NewReplacer("/", "_", "\\", "_", "..", "_")
	return r.Replace(id)
}
