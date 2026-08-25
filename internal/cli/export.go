package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
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
			"With no --output, the export streams to stdout so it pipes. " +
			"--output writes the same document to a file instead, created " +
			"fresh and renamed into place so an existing entry at that path " +
			"is replaced rather than opened and truncated.\n\n" +
			"--format json emits one document (messages oldest-first within " +
			"each conversation); --format jsonl emits one message per line, " +
			"for large exports and streaming.",
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

			if err := writeExportFile(outputPath, format, groups); err != nil {
				return err
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
	cmd.Flags().StringVar(&outputPath, "output", "", "write the export to this file; omit to stream to stdout")
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
//
// It publishes through safeWriteFile rather than opening path directly: path
// is exactly the caller-supplied --output value, so anyone able to pre-place
// a symlink there (a shared output directory, say) could otherwise redirect
// an O_TRUNC open onto a file the exporting user can write but shouldn't —
// the same attack writeConversationsFile's doc comment describes for
// conversations.jsonl, just at a --output path instead of a fixed filename
// inside one. See safeWriteFile for why this is the one way this package
// writes any file, rather than a third hand-rolled O_TRUNC.
func writeExportFile(path, format string, groups []*exportGroup) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("export: create %s: %w", dir, err)
	}
	_, _, err := safeWriteFile(dir, ".export-*.tmp", filepath.Base(path), 0o600, func(w io.Writer) error {
		return writeExportStream(w, format, groups)
	})
	return err
}

// safeWriteFile is the one way this package creates or replaces a file at a
// path it did not itself choose (--output, or a name derived from a
// caller-controlled conversation id): it writes to a fresh, randomly-named
// temp file in dir — os.CreateTemp opens with O_CREATE|O_EXCL, so there is
// nothing at that name yet for an attacker to have pre-placed — then
// os.Rename's it onto dir/destName. rename(2) replaces whatever directory
// entry currently occupies that name, file or symlink, without ever
// dereferencing it, so an existing symlink there is swapped out rather than
// followed and truncated.
//
// Every export write site funnels through this rather than hand-rolling
// O_CREATE|O_TRUNC. An earlier version closed that hole at one call site and
// left another open, so a single helper is what makes "every file lion writes
// goes through the safe path" true by construction rather than by remembering
// to copy it.
//
// It returns the size and SHA-256 of what was written, which a caller can use
// to verify on disk later what was actually produced here.
func safeWriteFile(dir, tmpPattern, destName string, mode os.FileMode, write func(io.Writer) error) (size int64, sha256Hex string, err error) {
	tmp, err := os.CreateTemp(dir, tmpPattern)
	if err != nil {
		return 0, "", fmt.Errorf("export: create temp file in %s: %w", dir, err)
	}
	tmpPath := tmp.Name()
	cleanup := func() {
		tmp.Close()
		os.Remove(tmpPath)
	}
	// os.CreateTemp already requests 0600, which umask can't loosen (0600
	// has no group/other bits to strip) — the explicit Chmod guarantees the
	// caller-requested mode regardless, matching every other file this
	// package writes into an export archive.
	if err := tmp.Chmod(mode); err != nil {
		cleanup()
		return 0, "", err
	}
	h := sha256.New()
	n, err := countingWrite(io.MultiWriter(tmp, h), write)
	if err != nil {
		cleanup()
		return 0, "", err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return 0, "", err
	}
	dest := filepath.Join(dir, destName)
	if err := os.Rename(tmpPath, dest); err != nil {
		os.Remove(tmpPath)
		return 0, "", fmt.Errorf("export: publish %s: %w", dest, err)
	}
	return n, hex.EncodeToString(h.Sum(nil)), nil
}

// countingWrite runs write against w wrapped to count the bytes that pass
// through it, so safeWriteFile can report a file's size without a second
// pass (e.g. os.Stat after the fact, which would itself be a path-based
// re-check of exactly the kind this whole fix moves away from).
func countingWrite(w io.Writer, write func(io.Writer) error) (int64, error) {
	cw := &countingWriter{w: w}
	if err := write(cw); err != nil {
		return cw.n, err
	}
	return cw.n, nil
}

type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}
