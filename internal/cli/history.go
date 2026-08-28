// Package cli's history.go implements `lion history coverage` and `lion
// history backfill`. wacli ships both as first-class commands; lion's own
// `sync --backfill` came first (sync.go), so backfill here calls straight
// into sync.go's backfillMessages rather than re-implementing the paging
// logic — see newHistoryBackfillCmd.
package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/jodok/lion/internal/output"
	"github.com/jodok/lion/internal/store"
	"github.com/jodok/lion/internal/voyager"
	"github.com/spf13/cobra"
)

func init() { registerCommand(newHistoryCmd) }

func newHistoryCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "history",
		Short: "Inspect and extend how much message history is synced locally",
	}
	cmd.AddCommand(newHistoryCoverageCmd(), newHistoryBackfillCmd())
	return cmd
}

// coverageEntry is one conversation's --json/table shape for `history
// coverage`.
type coverageEntry struct {
	ID           string   `json:"id"`
	URN          string   `json:"urn,omitempty"`
	Participants []string `json:"participants,omitempty"`
	MessageCount int      `json:"message_count"`
	OldestSynced *int64   `json:"oldest_synced,omitempty"`
	NewestSynced *int64   `json:"newest_synced,omitempty"`
	BackfillDone bool     `json:"backfill_done"`
	UpdatedAt    int64    `json:"updated_at"`
}

func newHistoryCoverageCmd() *cobra.Command {
	var (
		conversationID string
		storePath      string
	)
	cmd := &cobra.Command{
		Use:   "coverage",
		Short: "Show how much message history the local store holds, per conversation",
		Long: "Coverage reads lion's local message store ($LION_HOME/store.db, " +
			"populated by `lion sync`) and never touches the network. For " +
			"each conversation it reports the oldest and newest stored " +
			"message time, how many messages are stored, and whether " +
			"--backfill has reached all the way back to the start of the " +
			"thread (backfill_done) — this is how you answer \"do I " +
			"actually have everything?\", which for a backup tool is the " +
			"whole point. Sorted newest-activity first.",
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
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

			cov, err := st.Coverage(ctx, conversationID)
			if err != nil {
				return err
			}
			return renderCoverage(app.Renderer(), app.Cfg.JSON, cov)
		},
	}
	cmd.Flags().StringVar(&conversationID, "conversation", "", "restrict to one conversation id")
	cmd.Flags().StringVar(&storePath, "store", "", "path to the store database (default $LION_HOME/store.db)")
	return cmd
}

// renderCoverage wraps every LinkedIn-controlled free-text field
// (participant display names, via wrapStoreParticipants — see untrusted.go)
// once and renders it identically for every output format (F17).
func renderCoverage(r *output.Renderer, jsonOut bool, cov []store.ConversationCoverage) error {
	entries := make([]coverageEntry, len(cov))
	for i, c := range cov {
		entries[i] = coverageEntry{
			ID:           c.ID,
			URN:          c.URN,
			Participants: wrapStoreParticipants(r, c.Participants),
			MessageCount: c.MessageCount,
			OldestSynced: c.OldestSynced,
			NewestSynced: c.NewestSynced,
			BackfillDone: c.BackfillDone,
			UpdatedAt:    c.UpdatedAt,
		}
	}
	if jsonOut {
		return r.Emit(entries)
	}
	t := &output.Table{Cols: []string{"CONVERSATION", "PARTICIPANTS", "MESSAGES", "OLDEST", "NEWEST", "BACKFILL_DONE"}}
	for _, e := range entries {
		t.Rows = append(t.Rows, []string{
			e.ID,
			strings.Join(e.Participants, ", "),
			fmt.Sprintf("%d", e.MessageCount),
			formatOptionalEpochMs(e.OldestSynced),
			formatOptionalEpochMs(e.NewestSynced),
			fmt.Sprintf("%t", e.BackfillDone),
		})
	}
	return r.Emit(t)
}

// formatOptionalEpochMs renders a possibly-nil epoch-ms bound as the raw
// integer, or "-" when there's nothing stored yet (a conversation that's
// been discovered but never had a message page recorded).
func formatOptionalEpochMs(v *int64) string {
	if v == nil {
		return "-"
	}
	return fmt.Sprintf("%d", *v)
}

// backfillSummary is `history backfill`'s stdout contract, deliberately
// shaped like syncSummary (sync.go) minus the discovery-specific fields
// backfill doesn't have (it never discovers new conversations, only pages
// further back on ones already known — see newHistoryBackfillCmd).
type backfillSummary struct {
	ConversationsProcessed int    `json:"conversations_processed"`
	MessagesAdded          int    `json:"messages_added"`
	Elapsed                string `json:"elapsed"`
	Complete               bool   `json:"complete"`
}

func emitBackfillSummary(r *output.Renderer, jsonOut bool, s backfillSummary) error {
	if jsonOut {
		// Bare JSON object, not a wacli-style {"success":...,"data":...}
		// envelope — see sync.go's emitSyncSummary for why lion's other
		// commands already committed to this and backfill follows suit.
		return r.Emit(s)
	}
	return r.Emit(&output.Table{
		Cols: []string{"CONVERSATIONS_PROCESSED", "MESSAGES_ADDED", "ELAPSED", "COMPLETE"},
		Rows: [][]string{{
			fmt.Sprintf("%d", s.ConversationsProcessed),
			fmt.Sprintf("%d", s.MessagesAdded),
			s.Elapsed,
			fmt.Sprintf("%t", s.Complete),
		}},
	})
}

func newHistoryBackfillCmd() *cobra.Command {
	var (
		conversationID string
		limit          int
		after          string
		storePath      string
		events         bool
		lockWait       time.Duration
	)
	cmd := &cobra.Command{
		Use:   "backfill",
		Short: "Fill in history for conversations not yet fully synced",
		Long: "Backfill drains a conversation's message history until it " +
			"reaches the true start of the thread (recorded as " +
			"backfill_done — see `lion history coverage`) or this run's " +
			"limits are hit.\n\n" +
			"A plain `lion sync` already reaches the end of history on its " +
			"own, which is why the old `sync --backfill` flag was dropped. " +
			"This is what fills a conversation an --after or budget-capped " +
			"run left partial, and unlike sync it skips re-discovering " +
			"conversations and catching up to the newest messages first.\n\n" +
			"With no --conversation, every stored conversation not yet " +
			"backfill_done is processed. Like `lion sync`, this is " +
			"read-only with respect to LinkedIn — it never posts, sends, or " +
			"mutates anything there, only extends the local store — so it " +
			"works under --readonly and never prompts.",
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			var afterMs *int64
			if after != "" {
				ms, err := parseTimeFlag("after", after)
				if err != nil {
					return err
				}
				afterMs = &ms
			}
			if limit < 0 {
				return usageErr("--limit must not be negative")
			}

			app := appFrom(cmd)
			cl, err := app.Client()
			if err != nil {
				return err
			}

			path := storePath
			if path == "" {
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

			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
			defer stop()

			progress := newProgressReporter(os.Stderr, events)
			defer progress.Close()

			if !st.LockSupported() {
				progress.Warn("no inter-process lock on this platform; a concurrent `lion sync` could interleave writes")
			}
			release, err := st.Lock(ctx, lockWait)
			if err != nil {
				return err
			}
			defer release()

			targets, err := resolveBackfillTargets(ctx, st, conversationID)
			if err != nil {
				return err
			}

			summary, runErr := runHistoryBackfill(ctx, cl, st, targets, afterMs, limit, progress)

			r := app.Renderer()
			if emitErr := emitBackfillSummary(r, app.Cfg.JSON, summary); emitErr != nil {
				if runErr != nil {
					return errors.Join(runErr, emitErr)
				}
				return emitErr
			}
			return runErr
		},
	}
	cmd.Flags().StringVar(&conversationID, "conversation", "", "restrict backfill to one conversation id (default: every conversation not yet backfill_done)")
	cmd.Flags().IntVar(&limit, "limit", 0, "cap the number of messages fetched this run (0 = no cap)")
	cmd.Flags().StringVar(&after, "after", "", "stop backfilling once messages reach this time (RFC3339 or YYYY-MM-DD) rather than paging to the true start")
	cmd.Flags().StringVar(&storePath, "store", "", "path to the store database (default $LION_HOME/store.db)")
	cmd.Flags().BoolVar(&events, "events", false, "emit NDJSON progress events on stderr instead of a status line")
	cmd.Flags().DurationVar(&lockWait, "lock-wait", 0, "wait up to this long for a running `lion sync` to release the store lock instead of failing immediately")
	return cmd
}

// resolveBackfillTargets decides which conversations a backfill run should
// process: just conversationID if given (erroring if it isn't a known
// conversation — backfill extends history sync already discovered, it
// doesn't discover new conversations itself), or every stored conversation
// not yet BackfillDone otherwise.
func resolveBackfillTargets(ctx context.Context, st *store.Store, conversationID string) ([]string, error) {
	if conversationID != "" {
		if _, ok, err := st.Conversation(ctx, conversationID); err != nil {
			return nil, err
		} else if !ok {
			return nil, fmt.Errorf("conversation %q not found in the store; run `lion sync` first so it's discovered", conversationID)
		}
		return []string{conversationID}, nil
	}
	convs, err := st.Conversations(ctx)
	if err != nil {
		return nil, err
	}
	var targets []string
	for _, c := range convs {
		if !c.BackfillDone {
			targets = append(targets, c.ID)
		}
	}
	return targets, nil
}

// runHistoryBackfill pages further back on each of targets in turn, calling
// backfillMessages (sync.go) — the exact same function `lion sync
// --backfill` uses — so backfill's paging/resumption/termination logic
// lives in exactly one place. It follows the same partial-run honesty rules
// as runSyncPass (sync.go): it always returns a summary, and err is
// non-nil whenever the pass didn't finish everything it set out to do, in
// which case summary.Complete is always false.
func runHistoryBackfill(ctx context.Context, cl *voyager.Client, st *store.Store, targets []string, afterMs *int64, limit int, progress *progressReporter) (backfillSummary, error) {
	start := time.Now()
	summary := backfillSummary{Complete: true}
	opts := syncOptions{afterMs: afterMs}

	var budget *int
	if limit > 0 {
		b := limit
		budget = &b
	}

	for _, id := range targets {
		if ctx.Err() != nil {
			summary.Complete = false
			summary.Elapsed = formatDuration(time.Since(start))
			return summary, ctx.Err()
		}
		if budget != nil && *budget <= 0 {
			progress.Warn("--limit reached; stopping before conversation %s", id)
			summary.Complete = false
			break
		}

		progress.Status("backfilling %s", id)
		added, complete, err := backfillMessages(ctx, cl, st, id, opts, budget, progress)
		summary.MessagesAdded += added
		if err != nil {
			if errors.Is(err, errMaxDBSizeReached) {
				progress.Warn("%s", err)
				summary.Complete = false
				break
			}
			summary.Complete = false
			summary.Elapsed = formatDuration(time.Since(start))
			return summary, err
		}
		// backfillMessages reports complete=false when it stopped short of a
		// conversation's true start (a stalled cursor, or a budget cap landing
		// mid-history) — the same honesty sync carries, so a coverage-driven
		// backfill that didn't finish a thread must not claim it did.
		if !complete {
			summary.Complete = false
		}
		summary.ConversationsProcessed++
	}

	summary.Elapsed = formatDuration(time.Since(start))
	return summary, nil
}
