package cli

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/jodok/lion/internal/output"
	"github.com/jodok/lion/internal/store"
	"github.com/spf13/cobra"
)

// newMessageSearchCmd builds `lion message search`, added as a subcommand
// from newMessageCmd (message.go) rather than self-registered — same
// wiring as newMessageExportCmd, since search is part of the message
// vertical, not its own top-level command.
func newMessageSearchCmd() *cobra.Command {
	var (
		conversationID string
		from           string
		limit          int
		after, before  string
		asc            bool
		storePath      string
	)
	cmd := &cobra.Command{
		Use:   "search <query>",
		Short: "Search synced messages in the local store",
		Long: "Search reads lion's local message store ($LION_HOME/store.db, " +
			"populated by `lion sync`) and never touches the network — it " +
			"works offline, and under any rate-limit state, because it makes " +
			"no LinkedIn request at all. Run `lion sync` first to populate " +
			"the store.\n\n" +
			"Matches are found via a SQLite FTS5 index over message bodies " +
			"and sender names, returned newest-first by default (--asc for " +
			"oldest-first) — search is a way to jump into conversation " +
			"history at a point in time, not a relevance-ranked lookup.",
		Args: usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			query := args[0]
			if strings.TrimSpace(query) == "" {
				return usageErr("search query must not be empty")
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

			// The distinction this guard exists for: "no matches" (a normal,
			// successful outcome — see below) must never be confused with
			// "you never synced" (a store that's empty or doesn't exist yet).
			// Conflating the two is what makes a search tool untrustworthy —
			// a user seeing no results has no way to tell whether their
			// query really has no matches or whether there's simply nothing
			// to search yet.
			empty, err := st.Empty(ctx)
			if err != nil {
				return err
			}
			if empty {
				fmt.Fprintln(os.Stderr, "no local message store yet; run `lion sync` first")
				return fmt.Errorf("no local message store yet: run `lion sync` first")
			}

			if conversationID != "" {
				if _, ok, err := st.Conversation(ctx, conversationID); err != nil {
					return err
				} else if !ok {
					return fmt.Errorf("conversation %q not found in the store", conversationID)
				}
			}

			msgs, err := st.Search(ctx, store.SearchFilter{
				Query:          query,
				ConversationID: conversationID,
				From:           from,
				After:          afterMs,
				Before:         beforeMs,
				Limit:          limit,
				Asc:            asc,
			})
			if err != nil {
				return err
			}

			// An empty result set is a normal outcome (see the store-empty
			// guard above for the case this is NOT confused with): nothing
			// on stdout, a count on stderr, exit 0.
			fmt.Fprintf(os.Stderr, "%d match(es) for %q\n", len(msgs), query)
			if len(msgs) == 0 {
				return nil
			}

			return renderSearchResults(app.Renderer(), app.Cfg.JSON, msgs)
		},
	}

	cmd.Flags().StringVar(&conversationID, "conversation", "", "restrict the search to one conversation id")
	cmd.Flags().StringVar(&from, "from", "", "restrict to messages from this sender (matches a display-name substring or an exact profile URN)")
	cmd.Flags().IntVar(&limit, "limit", 0, "cap the number of results (0 = no cap)")
	cmd.Flags().StringVar(&after, "after", "", "only match messages at or after this time (RFC3339 or YYYY-MM-DD)")
	cmd.Flags().StringVar(&before, "before", "", "only match messages at or before this time (RFC3339 or YYYY-MM-DD)")
	cmd.Flags().BoolVar(&asc, "asc", false, "return oldest-first instead of the default newest-first")
	cmd.Flags().StringVar(&storePath, "store", "", "path to the store database (default $LION_HOME/store.db)")
	return cmd
}

// renderSearchResults wraps every LinkedIn-controlled free-text field (via
// wrapStoreMessage — see untrusted.go) once and renders it identically for
// every output format, matching renderMessages'/renderConversations' F17
// convention: --wrap-untrusted applies to --json, --plain, and table output
// alike, not just the human table.
func renderSearchResults(r *output.Renderer, jsonOut bool, msgs []store.Message) error {
	wrapped := make([]store.Message, len(msgs))
	for i, m := range msgs {
		wrapped[i] = wrapStoreMessage(r, m)
	}
	if jsonOut {
		hits := make([]searchHit, len(wrapped))
		for i, m := range wrapped {
			hits[i] = toSearchHit(m)
		}
		return r.Emit(hits)
	}
	t := &output.Table{Cols: []string{"CONVERSATION", "SENDER", "SENT_AT", "BODY"}}
	for _, m := range wrapped {
		t.Rows = append(t.Rows, []string{
			m.ConversationID,
			m.SenderName,
			strconv.FormatInt(m.SentAt, 10),
			m.Body,
		})
	}
	return r.Emit(t)
}

// searchHit is one message search result's --json wire shape. A distinct
// type from exportedMessage (export.go) rather than a shared one: export's
// type deliberately skips --wrap-untrusted (an archive must round-trip),
// while search must apply it, and folding both concerns into one type would
// make that difference a runtime flag instead of something visible at the
// call site.
type searchHit struct {
	ConversationID string `json:"conversation_id"`
	URN            string `json:"urn"`
	SenderName     string `json:"sender_name"`
	SenderURN      string `json:"sender_urn,omitempty"`
	SentAt         int64  `json:"sent_at"`
	Body           string `json:"body"`
}

func toSearchHit(m store.Message) searchHit {
	return searchHit{
		ConversationID: m.ConversationID,
		URN:            m.URN,
		SenderName:     m.SenderName,
		SenderURN:      m.SenderURN,
		SentAt:         m.SentAt,
		Body:           m.Body,
	}
}
