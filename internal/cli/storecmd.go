// Package cli's storecmd.go implements `lion store stats` and `lion store
// cleanup` — inspection and maintenance of $LION_HOME/store.db itself,
// distinct from the message/history verticals that read *through* it.
// Named storecmd.go (not store.go) only to avoid inviting confusion with
// the internal/store package this file is a thin cobra wrapper around.
package cli

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/jodok/lion/internal/output"
	"github.com/jodok/lion/internal/store"
	"github.com/spf13/cobra"
)

func init() { registerCommand(newStoreCmd) }

func newStoreCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "store",
		Short: "Inspect and maintain lion's local message store",
	}
	cmd.AddCommand(newStoreStatsCmd(), newStoreCleanupCmd())
	return cmd
}

func newStoreStatsCmd() *cobra.Command {
	var storePath string
	cmd := &cobra.Command{
		Use:   "stats",
		Short: "Show summary statistics for the local message store",
		Long: "Stats reads lion's local message store ($LION_HOME/store.db) " +
			"and never touches the network. On a store nothing has synced " +
			"into yet, it reports all-zero counts rather than an error — " +
			"that's a legitimate answer here (unlike `message search`/" +
			"`message export`, where an empty result could be mistaken for " +
			"a real one), since the counts themselves already say \"you " +
			"haven't synced yet\".",
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
			stats, err := st.Stats(ctx)
			if err != nil {
				return err
			}
			size, err := st.SizeBytes()
			if err != nil {
				return err
			}

			r := app.Renderer()
			if app.Cfg.JSON {
				return r.Emit(storeStatsResult{
					Conversations: stats.Conversations,
					Messages:      stats.Messages,
					OldestMessage: stats.OldestMessage,
					NewestMessage: stats.NewestMessage,
					SchemaVersion: stats.SchemaVersion,
					LastSyncedAt:  stats.LastSyncedAt,
					SizeBytes:     size,
					Path:          st.Path(),
				})
			}
			return r.Emit(&output.Table{
				Cols: []string{"CONVERSATIONS", "MESSAGES", "OLDEST", "NEWEST", "SIZE_BYTES", "SCHEMA_VERSION", "LAST_SYNCED_AT", "PATH"},
				Rows: [][]string{{
					fmt.Sprintf("%d", stats.Conversations),
					fmt.Sprintf("%d", stats.Messages),
					formatOptionalEpochMs(stats.OldestMessage),
					formatOptionalEpochMs(stats.NewestMessage),
					fmt.Sprintf("%d", size),
					fmt.Sprintf("%d", stats.SchemaVersion),
					formatOptionalEpochMs(stats.LastSyncedAt),
					st.Path(),
				}},
			})
		},
	}
	cmd.Flags().StringVar(&storePath, "store", "", "path to the store database (default $LION_HOME/store.db)")
	return cmd
}

// storeStatsResult is `store stats`' --json wire shape.
type storeStatsResult struct {
	Conversations int    `json:"conversations"`
	Messages      int    `json:"messages"`
	OldestMessage *int64 `json:"oldest_message,omitempty"`
	NewestMessage *int64 `json:"newest_message,omitempty"`
	SchemaVersion int    `json:"schema_version"`
	LastSyncedAt  *int64 `json:"last_synced_at,omitempty"`
	SizeBytes     int64  `json:"size_bytes"`
	Path          string `json:"path"`
}

// cleanupNote is printed on stderr and embedded in the --json output alike
// (DESIGN.md §2.3 doesn't require duplicating a message across streams, but
// this particular one is safety-critical: a user who believes `store
// cleanup` deletes anything on LinkedIn itself, in either direction, would
// be badly surprised — see the spec's explicit callout).
const cleanupNote = "local cache only: this removes lion's cached copy from store.db; nothing on LinkedIn is changed, and a later `lion sync` can refetch these conversations"

// cleanupEntry is one conversation `store cleanup` removed (or, under
// --dry-run, would remove).
type cleanupEntry struct {
	ID           string   `json:"id"`
	Participants []string `json:"participants,omitempty"`
	UpdatedAt    int64    `json:"updated_at"`
}

// cleanupResult is `store cleanup`'s --json wire shape.
type cleanupResult struct {
	Status        string         `json:"status"` // "deleted" or "would_delete" (--dry-run)
	Conversations []cleanupEntry `json:"conversations"`
	Note          string         `json:"note"`
}

func newStoreCleanupCmd() *cobra.Command {
	var (
		days      int
		storePath string
		lockWait  time.Duration
	)
	cmd := &cobra.Command{
		Use:   "cleanup",
		Short: "Delete conversations older than N days from the local cache",
		Long: "Cleanup deletes conversations (and their messages, via cascade) " +
			"from lion's LOCAL cache ($LION_HOME/store.db) whose last " +
			"activity is older than --days. This only prunes what lion has " +
			"cached on this machine: it deletes NOTHING on LinkedIn — the " +
			"conversation and its messages are untouched in your LinkedIn " +
			"account, and a later `lion sync` can refetch them (subject to " +
			"whatever history LinkedIn itself still retains).\n\n" +
			"Deleting local data is still a mutation: it's blocked by " +
			"--readonly, and (outside --dry-run) needs confirmation — only " +
			"--yes authorizes it, --no-input alone does not. --dry-run " +
			"prints exactly what would be removed and deletes nothing.",
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			if days <= 0 {
				return usageErr("--days must be a positive number of days")
			}
			app := appFrom(cmd)
			// Checked unconditionally, even under --dry-run, matching every
			// other mutating command in this codebase (e.g. feed post):
			// --readonly means "block all mutating actions", not "block
			// only the ones that would actually run".
			if err := app.requireWritable(); err != nil {
				return err
			}

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
			cutoffMs := time.Now().AddDate(0, 0, -days).UnixMilli()
			targets, err := st.ConversationsOlderThan(ctx, cutoffMs)
			if err != nil {
				return err
			}

			fmt.Fprintln(os.Stderr, "note:", cleanupNote)

			r := app.Renderer()
			dryRun := app.Cfg.DryRun

			// Nothing to confirm when there's nothing to delete — matching
			// `connection accept --all`'s identical guard — so an empty
			// cleanup run never trips the --no-input-without---yes usage
			// error over a mutation that wouldn't have touched anything.
			if !dryRun && len(targets) > 0 {
				ok, err := app.confirm(fmt.Sprintf(
					"About to delete %d conversation(s) — and all their messages — from lion's LOCAL cache. Nothing on LinkedIn is affected. Proceed?",
					len(targets)))
				if err != nil {
					return err
				}
				if !ok {
					fmt.Fprintln(os.Stderr, "aborted: nothing deleted")
					return nil
				}
			}

			if !dryRun {
				// Take the store lock for the delete pass, the same lock
				// `lion sync` holds — cleanup is a store writer too, and a
				// concurrent sync must not interleave. The lock is acquired
				// here, after the confirmation prompt, rather than around the
				// whole command, so a human deliberating at the prompt never
				// blocks a scheduled sync.
				if st.LockSupported() {
					release, err := st.Lock(ctx, lockWait)
					if err != nil {
						return err
					}
					defer release()
				}
				// Each delete re-checks the cutoff inside its own statement,
				// so a conversation a sync refreshed between the select above
				// and here (updated_at bumped to now) is spared rather than
				// destroyed. deleted tracks what was actually removed, which
				// can be fewer than targets for exactly that reason.
				deleted := targets[:0:0]
				for _, c := range targets {
					gone, err := st.DeleteConversationIfOlderThan(ctx, c.ID, cutoffMs)
					if err != nil {
						return err
					}
					if gone {
						deleted = append(deleted, c)
					}
				}
				if len(deleted) < len(targets) {
					fmt.Fprintf(os.Stderr, "note: %d of %d target(s) were refreshed by a concurrent sync and left in place\n",
						len(targets)-len(deleted), len(targets))
				}
				targets = deleted
			}

			status := "deleted"
			if dryRun {
				status = "would_delete"
			}
			entries := make([]cleanupEntry, len(targets))
			for i, c := range targets {
				entries[i] = cleanupEntry{
					ID:           c.ID,
					Participants: wrapStoreParticipants(r, c.Participants),
					UpdatedAt:    c.UpdatedAt,
				}
			}

			if app.Cfg.JSON {
				return r.Emit(cleanupResult{Status: status, Conversations: entries, Note: cleanupNote})
			}
			t := &output.Table{Cols: []string{"CONVERSATION", "PARTICIPANTS", "LAST_ACTIVITY"}}
			for _, e := range entries {
				t.Rows = append(t.Rows, []string{e.ID, strings.Join(e.Participants, ", "), fmt.Sprintf("%d", e.UpdatedAt)})
			}
			return r.Emit(t)
		},
	}
	cmd.Flags().IntVar(&days, "days", 0, "delete conversations whose last activity is older than this many days (required)")
	cmd.Flags().DurationVar(&lockWait, "lock-wait", 0, "wait up to this long for a running lion sync to release the store lock instead of failing immediately")
	cmd.Flags().StringVar(&storePath, "store", "", "path to the store database (default $LION_HOME/store.db)")
	return cmd
}
