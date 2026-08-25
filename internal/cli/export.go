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
	"runtime"
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
// O_CREATE|O_TRUNC: the previous round of this fix closed exactly this hole
// at one call site (conversations.jsonl) and left two more open
// (writeExportFile's --output file, and the per-conversation files below) —
// a single helper is what makes "every file lion writes goes through the
// safe path" true by construction instead of by remembering to copy it.
//
// It returns the size and SHA-256 of what was written so callers that need
// to verify a file later (writeMessagesDir's manifest) don't have to trust a
// self-asserted flag — they can check what's actually on disk against what
// was actually written here.
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

// writeExportDirectory writes the per-conversation layout: conversations.jsonl
// (one line of metadata per conversation) plus messages/<id>.jsonl (one line
// per message) for each conversation present in groups. Always JSON Lines
// regardless of --format — see newMessageExportCmd's Long help for why a
// directory of files doesn't have a "json envelope" equivalent worth
// building.
func writeExportDirectory(dir string, groups []*exportGroup) error {
	if err := ensureOutputDir(dir); err != nil {
		return err
	}
	// Must run before anything below touches the directory's contents: an
	// --output directory that already holds an unrelated conversations.jsonl
	// or messages/ tree has to be refused untouched, not partially
	// overwritten (conversations.jsonl written, then the refusal on
	// messages/) — see checkOutputDirOwnership and writeMessagesDir. The
	// returned marker (nil for a fresh --output directory) is what lets
	// writeMessagesDir delete only the files it can prove a prior lion
	// export actually created, instead of trusting the marker's mere
	// presence to authorize wiping the directory.
	prior, err := checkOutputDirOwnership(dir)
	if err != nil {
		return err
	}

	if err := writeConversationsFile(dir, groups); err != nil {
		return err
	}

	return writeMessagesDir(dir, groups, prior)
}

// writeConversationsFile publishes conversations.jsonl via safeWriteFile
// rather than opening the destination path directly. dir is a
// caller-supplied --output path, and opening "dir/conversations.jsonl"
// directly with O_TRUNC would mean anyone able to pre-place a symlink at
// that name (a shared output directory, say) redirects the truncating write
// to whatever file the exporting user can write, anywhere on disk.
func writeConversationsFile(dir string, groups []*exportGroup) error {
	_, _, err := safeWriteFile(dir, ".conversations-*.jsonl.tmp", "conversations.jsonl", 0o600, func(w io.Writer) error {
		enc := json.NewEncoder(w)
		for _, g := range groups {
			if err := enc.Encode(toExportedConversationMeta(g)); err != nil {
				return err
			}
		}
		return nil
	})
	return err
}

// exportMarkerFilename is written inside the messages/ directory itself
// (not at the --output directory's root — see checkOutputDirOwnership's
// doc comment for why that distinction is the whole point) the first time
// lion writes the export layout there. It's what checkOutputDirOwnership
// consults before either conversations.jsonl or the messages/ tree is ever
// allowed to be replaced — it's the one notion of "lion owns this archive"
// the whole directory layout shares, not a rule per file.
const exportMarkerFilename = ".lion-export.json"

// exportMarkerFormat identifies exportMarker's contents as lion's own, as
// opposed to some other tool's file that happened to land at the same path.
const exportMarkerFormat = "lion-message-export"

// exportManifestEntry identifies one file lion wrote into a messages/
// directory it created, well enough that a later export can tell "this is
// still exactly what I wrote" from "something else is here now" without
// trusting either the filename or the marker's mere presence: size catches
// a truncated or appended-to file cheaply, and the SHA-256 catches anything
// else, including a same-size substitution.
type exportManifestEntry struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// exportMarker is the on-disk shape of exportMarkerFilename. Format,
// Version, and UpdatedAt identify the marker itself as lion's; Files is the
// manifest of every other file lion wrote alongside it into the same
// messages/ directory. Neither half authorizes anything by itself — see
// checkOutputDirOwnership (which validates Format/Version before trusting
// the marker exists at all) and reconcileMessagesDir (which validates every
// entry in Files against what's actually on disk before deleting anything).
type exportMarker struct {
	Format    string                `json:"format"`
	Version   int                   `json:"version"`
	UpdatedAt string                `json:"updated_at"`
	Files     []exportManifestEntry `json:"files"`
}

// exportMarkerVersion is bumped if the marker's own shape (not the export
// layout) ever needs to change in a way a reader must distinguish.
const exportMarkerVersion = 1

// checkOutputDirOwnership refuses to let writeExportDirectory touch dir's
// conversations.jsonl file or its messages/ subdirectory unless a prior
// lion export already created THAT EXACT archive layout
// (exportMarkerFilename present *inside* messages/ itself, not merely
// somewhere at dir's root). On success it returns the marker found there —
// nil if dir has neither conversations.jsonl nor messages/ yet, non-nil
// (with its Files manifest) otherwise — for writeMessagesDir to reconcile
// against before it deletes anything.
//
// The marker used to live at dir's root, next to messages/ rather than
// inside it, and its mere existence was trusted without reading its
// contents. Both of those were the bug: a root-level marker survives a
// caller later deleting messages/ and replacing it with unrelated data
// (nothing then re-examines whether messages/ itself was ever lion's), and
// an unparsed marker trusts a copied, foreign, or corrupt file just as much
// as a genuine one. Keying ownership on a marker that lives inside the
// directory it vouches for — and actually validating its contents — ties
// the check to the exact tree about to be removed, so it can only ever be
// stale in a way that's still safe: gone along with whatever it protected.
// A parsed, correctly-formatted marker STILL isn't enough on its own to
// authorize deleting anything, though: it is exactly as forgeable as any
// other file lion didn't just write, since nothing stops another process
// with write access to dir from copying or hand-writing one. That is what
// Files exists to close — see reconcileMessagesDir, which is the only place
// deletion actually happens and which trusts the manifest only file-by-file,
// never the marker's format/version match alone.
//
// --output happily accepts any existing directory a caller names, including
// a normal one that already contains an unrelated messages/ folder, or an
// unrelated conversations.jsonl (possibly a symlink someone else placed
// there), for its own reasons. This is the guard that turns writing into
// either of those into a refusal instead: nothing gets deleted or written
// past this point unless neither path exists yet, or the messages/ tree
// that does exist carries a marker lion itself is known to have written
// into it. A pre-existing conversations.jsonl is covered by the very same
// check — lion never had a separate notion of "owns conversations.jsonl"
// and one file's ownership can't be judged any more strongly than the
// marker that vouches for the whole layout allows.
func checkOutputDirOwnership(dir string) (*exportMarker, error) {
	messagesDir := filepath.Join(dir, "messages")
	convPath := filepath.Join(dir, "conversations.jsonl")

	// Lstat, not Stat: a symlink at either path counts as "something is
	// already there" even if it's broken or points elsewhere entirely —
	// ownership is decided on what occupies the path, never on whatever a
	// symlink might resolve to.
	messagesPresent, err := lexists(messagesDir)
	if err != nil {
		return nil, fmt.Errorf("export: stat %s: %w", messagesDir, err)
	}
	convPresent, err := lexists(convPath)
	if err != nil {
		return nil, fmt.Errorf("export: stat %s: %w", convPath, err)
	}
	if !messagesPresent && !convPresent {
		return nil, nil
	}

	if messagesPresent {
		// The marker read below (os.ReadFile, which follows symlinks) is
		// only safe to trust once messagesDir itself is confirmed to be a
		// real directory: if "messages" were a symlink, reading through it
		// would read whatever the attacker's target contains, and every
		// later operation that walks messagesDir (reconcileMessagesDir)
		// would then be walking and deleting inside that target instead of
		// the archive directory the caller actually asked for.
		fi, err := os.Lstat(messagesDir)
		if err != nil {
			return nil, fmt.Errorf("export: stat %s: %w", messagesDir, err)
		}
		if fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir() {
			return nil, unownedOutputDirErr(dir)
		}
	}

	markerPath := filepath.Join(messagesDir, exportMarkerFilename)
	buf, err := os.ReadFile(markerPath)
	if err != nil {
		// No messages/ tree at all, or one with no marker in it: either way
		// there is nothing here proving lion wrote whatever currently
		// occupies conversations.jsonl or messages/.
		return nil, unownedOutputDirErr(dir)
	}
	var m exportMarker
	// An unparseable, foreign (wrong Format), or version-mismatched marker
	// means "not ours": a stale, copied, or corrupt file must never be
	// trusted to authorize replacing whatever actually occupies
	// conversations.jsonl or messages/ right now.
	if err := json.Unmarshal(buf, &m); err != nil || m.Format != exportMarkerFormat || m.Version != exportMarkerVersion {
		return nil, unownedOutputDirErr(dir)
	}
	return &m, nil
}

// lexists reports whether path already has something at it, following the
// same Lstat-not-Stat reasoning as checkOutputDirOwnership: a symlink
// counts as present without dereferencing it.
func lexists(path string) (bool, error) {
	if _, err := os.Lstat(path); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func unownedOutputDirErr(dir string) error {
	return usageErr("%s already contains a conversations.jsonl or messages/ directory that lion did not create; pick an empty directory or a different --output path", dir)
}

// writeExportMarker writes exportMarkerFilename inside messagesDir (the
// staging directory that writeMessagesDir is about to swap in as dir's
// messages/) so the marker and the tree it vouches for are always written
// and replaced together, atomically, by the same rename that publishes the
// rest of the export — see checkOutputDirOwnership for why the marker
// must live inside the directory it protects rather than beside it.
//
// files is the manifest of every other file this run wrote into messagesDir
// (see exportManifestEntry) — recording it here is what lets a later
// export's reconcileMessagesDir delete only files it can prove this run
// created, rather than trusting the marker's format/version alone.
func writeExportMarker(messagesDir string, files []exportManifestEntry) error {
	m := exportMarker{
		Format:    exportMarkerFormat,
		Version:   exportMarkerVersion,
		UpdatedAt: time.Now().UTC().Format(time.RFC3339),
		Files:     files,
	}
	buf, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	_, _, err = safeWriteFile(messagesDir, ".marker-*.tmp", exportMarkerFilename, 0o600, func(w io.Writer) error {
		_, err := w.Write(buf)
		return err
	})
	return err
}

// ensureOutputDir makes sure dir exists, creating it 0700 only when this
// call is the one that creates it. --output can point at a directory a user
// already has for other purposes, so an existing directory's permissions
// are left alone — chmodding it out from under whatever else uses it would
// be a surprising side effect of running an export. A pre-existing
// directory that's group/world-accessible is still flagged on stderr, so
// leaving it unrepaired is a visible choice rather than a silent one: it's
// about to gain a full copy of someone's private messages.
func ensureOutputDir(dir string) error {
	if fi, err := os.Stat(dir); err == nil {
		if !fi.IsDir() {
			return fmt.Errorf("export: %s exists and is not a directory", dir)
		}
		if runtime.GOOS != "windows" && fi.Mode().Perm()&0o077 != 0 {
			fmt.Fprintf(os.Stderr, "warning: %s already exists and is group/world-accessible (mode %o); lion will not change an existing directory's permissions, but it is about to hold exported private messages\n", dir, fi.Mode().Perm())
		}
		return nil
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("export: stat %s: %w", dir, err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("export: create %s: %w", dir, err)
	}
	// os.MkdirAll's mode is subject to umask, so an explicit Chmod is what
	// actually guarantees 0700 — safe here because we just created dir
	// ourselves in the branch above.
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("export: chmod %s: %w", dir, err)
	}
	return nil
}

// writeMessagesDir stages every conversation's messages/<id>.jsonl file into
// a fresh temporary directory inside dir, then swaps it into place as
// dir/messages, rather than reusing whatever was already at dir/messages and
// only writing this run's files into it. A second export into the same
// --output directory with a narrower filter (e.g. --after, or
// --conversation) legitimately writes fewer files than a prior, broader
// export did — reusing the directory in place would leave the excluded
// conversations' files sitting there, and an archive that silently retains
// messages the caller just asked to exclude is a privacy bug, not
// untidiness. Swapping the whole directory in guarantees the archive's
// contents match exactly this export, every time.
//
// prior is whatever checkOutputDirOwnership found (nil for a fresh --output
// directory). It is passed through to reconcileMessagesDir, which is the
// only place any pre-existing messages/ tree is ever deleted — file by file,
// and only files prior's manifest actually accounts for. This function never
// calls RemoveAll on messagesDir itself: an export marker only proves lion
// wrote *some* archive here at some point, not that everything sitting in
// the directory today is still that archive's, and treating it as blanket
// deletion authority is exactly the bug this whole fix closes (a copied or
// hand-written marker would otherwise make an unrelated messages/ tree look
// lion-owned and wipeable).
func writeMessagesDir(dir string, groups []*exportGroup, prior *exportMarker) error {
	staging, err := os.MkdirTemp(dir, ".messages-")
	if err != nil {
		return fmt.Errorf("export: stage messages dir: %w", err)
	}
	// os.MkdirTemp's mode is subject to umask like any other create; we made
	// this directory ourselves, so an explicit Chmod to guarantee 0700 is
	// always safe (contrast ensureOutputDir, which never chmods a
	// pre-existing --output directory).
	if err := os.Chmod(staging, 0o700); err != nil {
		os.RemoveAll(staging)
		return err
	}

	manifest := make([]exportManifestEntry, 0, len(groups))
	for _, g := range groups {
		name := sanitizeConversationFilename(g.id) + ".jsonl"
		size, sum, err := safeWriteFile(staging, ".msg-*.jsonl.tmp", name, 0o600, func(w io.Writer) error {
			menc := json.NewEncoder(w)
			for _, m := range g.messages {
				if err := menc.Encode(toExportedMessage(m)); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			os.RemoveAll(staging)
			return err
		}
		manifest = append(manifest, exportManifestEntry{Name: name, Size: size, SHA256: sum})
	}

	// The marker (with this run's manifest) is staged alongside the message
	// files it describes, not written separately afterward, so the rename
	// below publishes both together — there's never a window where
	// messages/ exists without the marker that vouches for it, or vice
	// versa.
	if err := writeExportMarker(staging, manifest); err != nil {
		os.RemoveAll(staging)
		return err
	}

	messagesDir := filepath.Join(dir, "messages")
	// Validated, file-by-file removal of exactly what prior's manifest
	// accounts for — see reconcileMessagesDir. staging is left untouched by
	// this call (it only ever touches messagesDir), so a refusal here still
	// leaves a full, valid staged export behind for the caller to inspect or
	// retry against a different --output, rather than losing the work.
	if err := reconcileMessagesDir(messagesDir, prior); err != nil {
		os.RemoveAll(staging)
		return err
	}
	if err := os.Rename(staging, messagesDir); err != nil {
		os.RemoveAll(staging)
		return fmt.Errorf("export: swap in %s: %w", messagesDir, err)
	}
	return nil
}

// reconcileMessagesDir is the only place this package ever deletes a
// pre-existing messages/ directory, and it never uses RemoveAll: every entry
// present in messagesDir must either be the marker file itself or a name
// prior's manifest lists, with a size and SHA-256 that still match what the
// manifest recorded — otherwise this refuses and deletes nothing at all,
// leaving messagesDir exactly as it was. Only once every entry has been
// verified does it remove them, one by one, and finally the (now provably
// empty) directory itself.
//
// This is deliberately two passes over the directory rather than
// delete-as-you-verify: a mismatch discovered halfway through must never
// leave the directory partially destroyed, since a caller who gets a
// refusal error is relying on "nothing happened" to decide what to do next.
func reconcileMessagesDir(messagesDir string, prior *exportMarker) error {
	present, err := lexists(messagesDir)
	if err != nil {
		return fmt.Errorf("export: stat %s: %w", messagesDir, err)
	}
	if !present {
		return nil // first export into this --output directory: nothing to remove
	}
	if prior == nil {
		// Unreachable in the current call graph: writeExportDirectory always
		// runs checkOutputDirOwnership first, which refuses before this
		// point whenever messagesDir exists without a valid marker. Kept as
		// a hard stop rather than assumed dead code — silently falling
		// through to delete an unaccounted-for directory here, on some
		// future refactor that skips the ownership check, is exactly the
		// bug this fix exists to close.
		return unownedOutputDirErr(filepath.Dir(messagesDir))
	}

	// checkOutputDirOwnership already confirmed messagesDir is a real
	// directory (not a symlink) as of that call; re-confirming here costs
	// nothing and means this function is safe to call on its own, not only
	// as writeMessagesDir's second step after that check.
	fi, err := os.Lstat(messagesDir)
	if err != nil {
		return fmt.Errorf("export: stat %s: %w", messagesDir, err)
	}
	if fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir() {
		return unownedOutputDirErr(filepath.Dir(messagesDir))
	}

	entries, err := os.ReadDir(messagesDir)
	if err != nil {
		return fmt.Errorf("export: read %s: %w", messagesDir, err)
	}
	byName := make(map[string]exportManifestEntry, len(prior.Files))
	for _, f := range prior.Files {
		byName[f.Name] = f
	}

	// Pass 1: verify every entry before deleting anything.
	for _, e := range entries {
		if e.Name() == exportMarkerFilename {
			continue
		}
		want, ok := byName[e.Name()]
		if !ok {
			return usageErr("%s contains %q, which lion's last export into this directory did not create; refusing to delete anything there — remove it manually, or export to an empty directory", messagesDir, e.Name())
		}
		if e.Type()&os.ModeSymlink != 0 || !e.Type().IsRegular() {
			return usageErr("%s in %s is not a regular file; refusing to delete anything there", e.Name(), messagesDir)
		}
		full := filepath.Join(messagesDir, e.Name())
		size, sum, err := hashFile(full)
		if err != nil {
			return fmt.Errorf("export: verify %s: %w", full, err)
		}
		if size != want.Size || sum != want.SHA256 {
			return usageErr("%s does not match what lion's last export recorded for it; refusing to delete anything in %s", full, messagesDir)
		}
	}

	// Pass 2: every entry checked out above — delete them, then the
	// directory itself. os.Remove rather than os.RemoveAll on the directory
	// is a deliberate fail-safe: if this loop somehow missed an entry (it
	// shouldn't — entries is exactly what ReadDir returned above), Remove
	// refuses on a non-empty directory instead of recursing through
	// whatever that entry turned out to be.
	for _, e := range entries {
		full := filepath.Join(messagesDir, e.Name())
		if err := os.Remove(full); err != nil {
			return fmt.Errorf("export: remove %s: %w", full, err)
		}
	}
	if err := os.Remove(messagesDir); err != nil {
		return fmt.Errorf("export: remove %s: %w", messagesDir, err)
	}
	return nil
}

// hashFile reads path (already confirmed a regular, non-symlink directory
// entry by reconcileMessagesDir's caller) and returns its size and
// hex-encoded SHA-256, for comparison against an exportManifestEntry.
func hashFile(path string) (size int64, sha256Hex string, err error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, "", err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return 0, "", err
	}
	return n, hex.EncodeToString(h.Sum(nil)), nil
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
