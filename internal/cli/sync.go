// Package cli's sync.go implements `lion sync`: the network pass that
// populates internal/store from LinkedIn, following the same
// sync-then-read-locally architecture as wacli (https://wacli.sh) rather
// than lion's other commands' one-shot API-to-stdout shape. See
// internal/store's package doc for why, and export.go for the read side.
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

func init() { registerCommand(newSyncCmd) }

// defaultConversationPageSize/defaultMessagePageSize bound how much a
// single fetched page (and therefore a single store transaction — see
// store.WithTx) covers. Smaller than LinkedIn's own server-side default so
// an interrupted sync loses at most a small page's worth of in-flight work,
// not a huge one.
const (
	defaultConversationPageSize = 25
	defaultMessagePageSize      = 50
)

// errMaxDBSizeReached stops a sync cleanly once the store would exceed
// --max-db-size, the same way a rate-limit budget stops it: reported on
// stderr, folded into complete:false, not treated as a crash.
var errMaxDBSizeReached = errors.New("store size limit reached (--max-db-size)")

func newSyncCmd() *cobra.Command {
	var (
		after            string
		maxConversations int
		maxMessages      int
		maxDBSize        string
		storePath        string
		once             bool
		follow           bool
		full             bool
		interval         time.Duration
		events           bool
		lockWait         time.Duration
	)
	cmd := &cobra.Command{
		Use:   "sync",
		Short: "Sync messages into lion's local store",
		Long: "Sync fetches conversations and messages from LinkedIn into a local " +
			"SQLite store ($LION_HOME/store.db by default), so `lion message " +
			"export` can read them offline without touching the network. It is " +
			"read-only with respect to LinkedIn — it never posts, sends, or " +
			"mutates anything there — so it works under --readonly and never " +
			"prompts.\n\n" +
			"A full first sync WILL be slow: every request goes through the same " +
			"rate limiter as every other lion command, with the same conservative " +
			"pacing. That's the account-safety feature this command exists to keep, " +
			"not a bug — a sync that hammered LinkedIn as fast as the network " +
			"allowed is exactly the bot-shaped traffic the rate limiter is here to " +
			"avoid. Re-running sync is cheap: catch-up mode stops as soon as it " +
			"reaches a message already in the store.\n\n" +
			"Unlike wacli, which defaults to following (it holds a live WhatsApp " +
			"Web socket and receives pushes for free), lion has no push channel: " +
			"following here means polling LinkedIn on a timer, which is precisely " +
			"the bot-shaped traffic the rate limiter exists to avoid. So sync " +
			"defaults to --once, and --follow is opt-in.",
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			if once && follow {
				return usageErr("--once and --follow are mutually exclusive")
			}

			var afterMs *int64
			if after != "" {
				ms, err := parseTimeFlag("after", after)
				if err != nil {
					return err
				}
				afterMs = &ms
			}
			var maxDBSizeBytes int64
			if maxDBSize != "" {
				b, err := parseSize(maxDBSize)
				if err != nil {
					return err
				}
				maxDBSizeBytes = b
			}
			if follow && interval <= 0 {
				return usageErr("--interval must be positive")
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

			// The hard backstop for --max-db-size: SizeBytes() pre-checks
			// scattered through discovery/catch-up/backfill are what stop a
			// pass cleanly and early in the common case, but they can't
			// predict a not-yet-fetched page's own footprint, so a single
			// large page could still push the store past the advertised
			// limit. This makes SQLite itself refuse and roll back any
			// transaction that would exceed it — see store.SetMaxSize.
			if err := st.SetMaxSize(maxDBSizeBytes); err != nil {
				return err
			}

			// Ctrl-C during a long sync must still leave the store consistent
			// and report complete:false honestly, rather than a hard kill that
			// leaves the caller unsure what landed. Each fetched page already
			// commits in its own transaction (store.WithTx), so cancellation
			// between pages loses at most the in-flight one.
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

			opts := syncOptions{
				afterMs:          afterMs,
				maxConversations: maxConversations,
				full:             full,
				maxMessages:      maxMessages,
				maxDBSizeBytes:   maxDBSizeBytes,
			}

			r := app.Renderer()
			for {
				summary, passErr := runSyncPass(ctx, cl, st, opts, progress)
				// Bare JSON objects on stdout, deliberately NOT wrapped in a
				// wacli-style {"success":...,"data":...} envelope: lion's
				// other commands already ship bare JSON as their public
				// --json contract, and a reviewer already rejected one
				// silent field rename here for breaking automation.
				// Introducing a second convention alongside that one would
				// be worse than picking either consistently, so sync and
				// export follow the rest of lion instead of the reference.
				if emitErr := emitSyncSummary(r, app.Cfg.JSON, summary); emitErr != nil {
					if passErr != nil {
						return errors.Join(passErr, emitErr)
					}
					return emitErr
				}
				if passErr != nil {
					return passErr
				}
				if !follow {
					return nil
				}
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(interval):
				}
			}
		},
	}

	cmd.Flags().StringVar(&after, "after", "", "only sync conversations/messages at or after this time (RFC3339 or YYYY-MM-DD)")
	cmd.Flags().IntVar(&maxConversations, "max-conversations", 0, "cap the number of conversations processed this run (0 = no cap)")
	cmd.Flags().IntVar(&maxMessages, "max-messages", 0, "cap the number of messages fetched this run (0 = no cap)")
	cmd.Flags().StringVar(&maxDBSize, "max-db-size", "", "stop cleanly before the store would exceed this size (e.g. 500MB, 2GB)")
	cmd.Flags().StringVar(&storePath, "store", "", "path to the store database (default $LION_HOME/store.db)")
	cmd.Flags().BoolVar(&once, "once", false, "sync a single pass and exit (the default; accepted explicitly to match wacli)")
	cmd.Flags().BoolVar(&full, "full", false, "ignore stored sync tokens and take a complete snapshot")
	cmd.Flags().BoolVar(&follow, "follow", false, "keep syncing every --interval instead of exiting after one pass (opt-in — see command help)")
	cmd.Flags().DurationVar(&interval, "interval", 60*time.Second, "how often to sync again under --follow")
	cmd.Flags().BoolVar(&events, "events", false, "emit NDJSON progress events on stderr instead of a status line")
	cmd.Flags().DurationVar(&lockWait, "lock-wait", 0, "wait up to this long for another `lion sync` to release the store lock instead of failing immediately")
	return cmd
}

// syncOptions bundles one pass's parameters so runSyncPass doesn't carry a
// long, error-prone positional-argument list.
type syncOptions struct {
	afterMs          *int64
	maxConversations int
	// full ignores stored sync tokens and takes a complete snapshot — the
	// escape hatch for a delta stream that has drifted from what is stored.
	full           bool
	maxMessages    int
	maxDBSizeBytes int64
}

// syncSummary is sync's stdout contract: conversations seen/updated,
// messages added, elapsed time, and complete:true|false. complete is false
// whenever anything cut the pass short — a budget, the store size cap, an
// error, or cancellation — so a caller can never mistake a partial sync for
// a finished one.
type syncSummary struct {
	ConversationsSeen    int    `json:"conversations_seen"`
	ConversationsUpdated int    `json:"conversations_updated"`
	MessagesAdded        int    `json:"messages_added"`
	Elapsed              string `json:"elapsed"`
	Complete             bool   `json:"complete"`
}

func emitSyncSummary(r *output.Renderer, jsonOut bool, s syncSummary) error {
	if jsonOut {
		return r.Emit(s)
	}
	return r.Emit(&output.Table{
		Cols: []string{"CONVERSATIONS_SEEN", "CONVERSATIONS_UPDATED", "MESSAGES_ADDED", "ELAPSED", "COMPLETE"},
		Rows: [][]string{{
			fmt.Sprintf("%d", s.ConversationsSeen),
			fmt.Sprintf("%d", s.ConversationsUpdated),
			fmt.Sprintf("%d", s.MessagesAdded),
			s.Elapsed,
			fmt.Sprintf("%t", s.Complete),
		}},
	})
}

// runSyncPass runs one full sync pass: discover conversations, then catch
// up (and optionally backfill) each one's messages. It always returns a
// summary, even on error — the caller emits it either way (see the
// "partial runs must be honest" rule) — and err is non-nil whenever the
// pass didn't finish everything it set out to do, in which case
// summary.Complete is always false.
func runSyncPass(ctx context.Context, cl *voyager.Client, st *store.Store, opts syncOptions, progress *progressReporter) (syncSummary, error) {
	start := time.Now()
	summary := syncSummary{Complete: true}

	toProcess, discComplete, err := discoverConversations(ctx, cl, st, opts, progress)
	if err != nil {
		if !errors.Is(err, errMaxDBSizeReached) {
			summary.Complete = false
			summary.ConversationsSeen = len(toProcess)
			summary.Elapsed = formatDuration(time.Since(start))
			return summary, err
		}
		// A store already at --max-db-size, or SQLite's own max_page_count
		// backstop rejecting a page mid-discovery — the same "truncated, not
		// failed" treatment as the identical check inside the
		// per-conversation loop below: warn, fall through with whatever
		// discovery already committed, and let the pass's own
		// complete:false summary say so rather than a non-zero exit.
		progress.Warn("%s", err)
	}
	summary.ConversationsSeen = len(toProcess)
	if !discComplete {
		// --max-conversations cut discovery short, or the conversations
		// cursor stalled (ErrPaginationStalled) — either way there may be
		// older conversations this pass never saw, so the summary must say
		// so even though nothing here actually failed.
		summary.Complete = false
	}

	var budget *int
	if opts.maxMessages > 0 {
		b := opts.maxMessages
		budget = &b
	}

	for _, conv := range toProcess {
		if ctx.Err() != nil {
			summary.Complete = false
			summary.Elapsed = formatDuration(time.Since(start))
			return summary, ctx.Err()
		}
		if budget != nil && *budget <= 0 {
			progress.Warn("--max-messages reached; stopping before conversation %s", conv.ID)
			summary.Complete = false
			break
		}

		progress.Status("syncing %s (%s)", conv.ID, firstParticipantName(conv.Participants))
		progress.Event("conversation_start", map[string]any{"id": conv.ID, "urn": conv.URN})

		added, caughtUp, err := catchUpMessages(ctx, cl, st, conv, opts, budget, progress)
		summary.MessagesAdded += added
		if !caughtUp {
			// catchUpMessages stopped without reaching a known message, an
			// empty page, or --after — i.e. --max-messages ran out mid-walk,
			// or the messages cursor stalled (ErrPaginationStalled). Either
			// way this conversation may have older messages left unfetched,
			// which the outer loop's own budget check (above) won't
			// necessarily catch if this was the last conversation.
			summary.Complete = false
		}
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
		if added > 0 {
			summary.ConversationsUpdated++
		}

		// No --backfill branch: a drain returns a conversation's whole
		// history, so catch-up above has already fetched everything a
		// separate backwards walk used to find. `lion history backfill` —
		// which wacli also ships as a first-class command — remains the way
		// to fill a conversation an earlier --after or budget-capped run
		// left partial.
	}

	progress.Event("sync_complete", map[string]any{
		"conversations_seen":    summary.ConversationsSeen,
		"conversations_updated": summary.ConversationsUpdated,
		"messages_added":        summary.MessagesAdded,
		"complete":              summary.Complete,
	})
	summary.Elapsed = formatDuration(time.Since(start))
	return summary, nil
}

// conversationsSyncTokenKey is where the mailbox stream's resume point lives.
// Keyed by nothing else because the store itself is per-LION_HOME and holds
// one account's conversations; switching --account against the same store
// would want a per-mailbox key, which is a change to make when the store
// grows an account column rather than guessed at here.
const conversationsSyncTokenKey = "sync_token:conversations"

// applyDeletedConversations removes conversations the sync stream reported as
// deleted.
//
// This only starts mattering once tokens are persisted: a full snapshot every
// run made a removed conversation simply stop appearing, whereas a delta
// stream names it once and never mentions it again. Ignoring that would leave
// it in the store forever.
func applyDeletedConversations(ctx context.Context, st *store.Store, urns []string, progress *progressReporter) error {
	for _, urn := range urns {
		id := conversationIDFromSyncURN(urn)
		if id == "" {
			continue
		}
		if err := st.DeleteConversation(ctx, id); err != nil {
			return err
		}
		progress.Event("conversation_deleted", map[string]any{"id": id, "urn": urn})
	}
	return nil
}

// conversationIDFromSyncURN extracts the thread segment from a deleted
// conversation's URN, matching how Conversation.ID is derived.
func conversationIDFromSyncURN(urn string) string {
	open := strings.Index(urn, "(")
	if open < 0 || !strings.HasSuffix(urn, ")") {
		return ""
	}
	inner := urn[open+1 : len(urn)-1]
	if i := strings.LastIndex(inner, ","); i >= 0 {
		return inner[i+1:]
	}
	return ""
}

// discoverConversations drains the mailbox sync stream, upserts every
// conversation it yields, and collects the ones this pass will sync messages
// for. It stops early at --max-conversations or --max-db-size.
//
// This replaced a newest-to-older page walk when LinkedIn retired the REST
// messaging endpoints. The sync-token surface has no cursor — its metadata
// carries a token, deleted urns, and a clear-cache flag, and nothing else —
// so there is no createdBefore to advance, no page to re-serve, and so no
// stalled cursor to detect. voyager.AllConversations drains until a response
// brings nothing new, which is what makes this correct whether or not
// LinkedIn chunks a large mailbox — a property that could not be verified
// directly, since the only account available to test against holds two
// conversations (see voyager/sync.go).
//
// The returned bool is true only when discovery ran to genuine exhaustion —
// false when --max-conversations cut it short, or when the drain hit its own
// request limit. Both are truncations, not failures: err stays nil so a
// caller doesn't turn them into a non-zero exit, but the caller must fold the
// bool into the pass's complete:false summary field.
func discoverConversations(ctx context.Context, cl *voyager.Client, st *store.Store, opts syncOptions, progress *progressReporter) ([]voyager.Conversation, bool, error) {
	if ctx.Err() != nil {
		return nil, false, ctx.Err()
	}
	if opts.maxDBSizeBytes > 0 {
		// Checked before anything is fetched, never after it has been
		// committed — the same ordering rule the message walk follows, so a
		// store already at the limit is never touched at all.
		if reached, sErr := storeSizeReached(st, opts.maxDBSizeBytes); sErr != nil {
			return nil, false, sErr
		} else if reached {
			return nil, false, errMaxDBSizeReached
		}
	}

	// Resume from where the last run left off, unless --full was asked for.
	// An empty token means a full snapshot, which is also what a first run,
	// an account switch, or a rejected token gets.
	var token string
	if !opts.full {
		if v, ok, mErr := st.Meta(ctx, conversationsSyncTokenKey); mErr != nil {
			return nil, false, mErr
		} else if ok {
			token = v
		}
	}

	// Drained without a cap so --max-conversations counts conversations that
	// actually pass the --after filter, the way the page walk did. The drain
	// bounds its own request count.
	convs, drained, next, deleted, err := cl.ConversationsFrom(ctx, token, 0)
	if err != nil && token != "" {
		// A stored token the server will not accept must not wedge sync
		// forever. Drop it and take a full snapshot; the next run then has a
		// fresh resume point.
		progress.Warn("stored sync token was rejected (%v); taking a full snapshot", err)
		if cErr := st.WithTx(ctx, func(tx *store.Tx) error {
			return tx.SetMeta(ctx, conversationsSyncTokenKey, "")
		}); cErr != nil {
			return nil, false, cErr
		}
		token = ""
		convs, drained, next, deleted, err = cl.ConversationsFrom(ctx, "", 0)
	}
	if err != nil {
		return nil, false, err
	}
	if len(deleted) > 0 {
		if dErr := applyDeletedConversations(ctx, st, deleted, progress); dErr != nil {
			return nil, false, dErr
		}
	}
	if len(convs) == 0 {
		return nil, drained, nil
	}
	if !drained {
		progress.Warn("conversation sync stream did not run out; older conversations may not have been discovered")
	}

	now := time.Now().UnixMilli()
	txErr := st.WithTx(ctx, func(tx *store.Tx) error {
		for _, c := range convs {
			if c.ID == "" {
				// The URN didn't parse into an id (an unexpected shape) —
				// nothing to key a row on, so skip rather than guess (see
				// voyager.Conversation.ID).
				progress.Warn("skipping a conversation with no parseable id (urn=%q)", c.URN)
				continue
			}
			if err := tx.UpsertConversation(ctx, toStoreConversation(c), now); err != nil {
				return err
			}
		}
		// Written in the same transaction as the rows it describes: a token
		// saved without its conversations would skip them forever, and
		// conversations saved without their token would refetch them.
		if next != "" {
			return tx.SetMeta(ctx, conversationsSyncTokenKey, next)
		}
		return nil
	})
	if txErr != nil {
		// SizeBytes() is advisory (it can't predict a not-yet-fetched
		// response's footprint), so SQLite refusing a transaction past
		// PRAGMA max_page_count is the hard backstop. translateStoreFull
		// folds that into the same errMaxDBSizeReached truncation every other
		// --max-db-size stop reports.
		return nil, false, translateStoreFull(txErr)
	}

	var toProcess []voyager.Conversation
	seen := map[string]bool{}
	for _, c := range convs {
		if c.ID == "" || seen[c.ID] {
			continue
		}
		if opts.afterMs != nil && c.UpdatedAt < *opts.afterMs {
			continue
		}
		seen[c.ID] = true
		toProcess = append(toProcess, c)
		progress.Event("conversation_discovered", map[string]any{"id": c.ID, "urn": c.URN})
		if opts.maxConversations > 0 && len(toProcess) >= opts.maxConversations {
			progress.Warn("--max-conversations reached; older conversations were not discovered")
			return toProcess, false, nil
		}
	}
	return toProcess, drained, nil
}

// drainConversationMessages fetches a conversation's messages through the
// sync-token stream and stores the ones not already held, honouring --after,
// a --max-messages budget, and --max-db-size. phase only labels the progress
// event.
//
// The page walk this replaced went with the REST endpoints. The sync-token
// surface returns a conversation's history without a cursor, so there is no
// createdBefore to advance, no short page to read as "caught up", and no
// stalled cursor to guard against. Re-storing a message already held is a
// no-op, which is what the old duplicate-page detection was approximating.
//
// complete is true only when the drain genuinely ran out — false when the
// budget, --max-db-size, or the drain's own request limit stopped it short,
// since older messages may then exist that this pass never saw.
func drainConversationMessages(ctx context.Context, cl *voyager.Client, st *store.Store, conversationID string, opts syncOptions, budget *int, progress *progressReporter, phase string) (added int, complete bool, err error) {
	if ctx.Err() != nil {
		return 0, false, ctx.Err()
	}
	if budget != nil && *budget <= 0 {
		return 0, false, nil
	}
	if opts.maxDBSizeBytes > 0 {
		// Before fetching, never after committing: a store already at
		// --max-db-size must not be touched.
		if reached, sErr := storeSizeReached(st, opts.maxDBSizeBytes); sErr != nil {
			return 0, false, sErr
		} else if reached {
			return 0, false, errMaxDBSizeReached
		}
	}

	limit := 0
	if budget != nil {
		limit = *budget
	}

	// Resume this conversation's stream where the last run left it.
	var token string
	if !opts.full {
		if conv, ok, cErr := st.Conversation(ctx, conversationID); cErr != nil {
			return 0, false, cErr
		} else if ok {
			token = conv.MessagesSyncToken
		}
	}
	msgs, drained, next, deleted, err := cl.MessagesFrom(ctx, conversationID, token, limit)
	if err != nil && token != "" {
		// A rejected token must not wedge this conversation forever: drop it
		// and take a full snapshot of the thread.
		progress.Warn("stored sync token for %s was rejected (%v); refetching the conversation", conversationID, err)
		if cErr := st.WithTx(ctx, func(tx *store.Tx) error {
			return tx.SetMessagesSyncToken(ctx, conversationID, "")
		}); cErr != nil {
			return 0, false, cErr
		}
		token = ""
		msgs, drained, next, deleted, err = cl.MessagesFrom(ctx, conversationID, "", limit)
	}
	if err != nil {
		return 0, false, err
	}
	if len(deleted) > 0 {
		if dErr := st.WithTx(ctx, func(tx *store.Tx) error {
			return tx.DeleteMessages(ctx, deleted)
		}); dErr != nil {
			return 0, false, dErr
		}
		progress.Event("messages_deleted", map[string]any{"conversation_id": conversationID, "count": len(deleted)})
	}

	// toStore is what gets persisted; the completeness decision below reads
	// drained — the drain's own verdict — never the filtered set, so an
	// --after-trimmed result is not mistaken for a short history.
	toStore := msgs
	if opts.afterMs != nil {
		toStore = messagesAtOrAfter(msgs, *opts.afterMs)
	}
	added, aErr := applyMessagePage(ctx, st, conversationID, toStore)
	if aErr != nil {
		return 0, false, translateStoreFull(aErr)
	}
	if budget != nil {
		*budget -= len(msgs)
	}
	progress.Status("syncing %s: +%d messages", conversationID, added)
	progress.Event("messages_stored", map[string]any{"conversation_id": conversationID, "added": added, "phase": phase})

	if next != "" {
		if tErr := st.WithTx(ctx, func(tx *store.Tx) error {
			return tx.SetMessagesSyncToken(ctx, conversationID, next)
		}); tErr != nil {
			return added, false, tErr
		}
	}

	if !drained {
		progress.Warn("message sync stream did not run out for conversation %s; older messages may not have been fetched", conversationID)
		return added, false, nil
	}
	// A drain that resumed from a token running out means "nothing changed
	// since last time" — it says nothing about whether the whole history is
	// held, because the delta never replays what came before. Only a full
	// snapshot proves that, so only a full snapshot may claim it. Without
	// this, the first incremental run would mark every conversation fully
	// archived regardless of how little of it the store actually has.
	if token != "" {
		return added, true, nil
	}
	// BackfillDone means the store holds this conversation's full history, so
	// it can only be claimed when what the drain fetched is also what was
	// stored. Under --after the trimmed messages were seen and deliberately
	// discarded; marking done would drop the conversation from every future
	// `history backfill` target list and report backfill_done=true for an
	// archive that has a hole in it.
	if len(toStore) == len(msgs) {
		if err := markBackfillDone(ctx, st, conversationID); err != nil {
			return added, false, err
		}
	}
	return added, true, nil
}

// catchUpMessages brings a conversation up to date. See
// drainConversationMessages for the contract.
func catchUpMessages(ctx context.Context, cl *voyager.Client, st *store.Store, conv voyager.Conversation, opts syncOptions, budget *int, progress *progressReporter) (added int, complete bool, err error) {
	return drainConversationMessages(ctx, cl, st, conv.ID, opts, budget, progress, "catch_up")
}

// markBackfillDone records that paging a conversation's messages backwards
// reached a genuine empty page, in its own transaction. It's shared by
// catchUpMessages (which can reach this directly for a short conversation)
// and, indirectly, mirrors what backfillMessages does inline for the same
// reason further down.
func markBackfillDone(ctx context.Context, st *store.Store, conversationID string) error {
	return translateStoreFull(st.WithTx(ctx, func(tx *store.Tx) error {
		return tx.MarkBackfillDone(ctx, conversationID, time.Now().UnixMilli())
	}))
}

// translateStoreFull turns store.ErrDatabaseFull — what WithTx returns when
// SQLite's own PRAGMA max_page_count ceiling (see store.SetMaxSize) rejects
// and rolls back a transaction that would have exceeded --max-db-size —
// into errMaxDBSizeReached, so every caller downstream only ever has to
// recognize the one sentinel already wired into "truncated, not failed"
// handling, regardless of whether lion's own SizeBytes pre-check or
// SQLite's hard backstop is what actually stopped this pass. A nil err
// passes through unchanged.
func translateStoreFull(err error) error {
	if errors.Is(err, store.ErrDatabaseFull) {
		return errMaxDBSizeReached
	}
	return err
}

// backfillMessages reaches older history for a conversation an earlier pass
// left partially synced.
//
// Under the sync-token surface a drain returns a conversation's whole
// history, so there is no separate older range to walk: backfilling is
// draining. What survives from the page-walk era is the part that still
// matters — the BackfillDone short-circuit, so a conversation already known
// to be fully held costs no requests at all.
func backfillMessages(ctx context.Context, cl *voyager.Client, st *store.Store, conversationID string, opts syncOptions, budget *int, progress *progressReporter) (added int, complete bool, err error) {
	current, ok, err := st.Conversation(ctx, conversationID)
	if err != nil {
		return 0, false, err
	}
	if !ok || current.BackfillDone {
		return 0, true, nil
	}
	added, complete, err = drainConversationMessages(ctx, cl, st, conversationID, opts, budget, progress, "backfill")
	if err == nil && complete {
		progress.Event("backfill_done", map[string]any{"conversation_id": conversationID})
	}
	return added, complete, err
}

// applyMessagePage upserts one fetched page inside its own transaction, so
// an interruption right after this call leaves the store holding exactly
// that page — never half of it.
func applyMessagePage(ctx context.Context, st *store.Store, conversationID string, msgs []voyager.Message) (added int, err error) {
	err = st.WithTx(ctx, func(tx *store.Tx) error {
		var e error
		added, e = tx.RecordMessagePage(ctx, conversationID, toStoreMessages(msgs, conversationID), time.Now().UnixMilli())
		return e
	})
	return added, translateStoreFull(err)
}

func storeSizeReached(st *store.Store, maxBytes int64) (bool, error) {
	size, err := st.SizeBytes()
	if err != nil {
		return false, err
	}
	return size >= maxBytes, nil
}

// messagesAtOrAfter filters msgs down to those at or after cutoff,
// preserving order. It exists solely to decide what applyMessagePage is
// allowed to persist — see catchUpMessages/backfillMessages, which
// deliberately keep using the unfiltered page for their own pagination and
// completeness decisions. Without this filter, any page whose messages
// straddle --after got stored in full: the cutoff was only ever checked
// afterward, against the page's oldest timestamp, to decide whether to
// fetch another page — never against each message to decide whether it
// should have been written at all.
func messagesAtOrAfter(msgs []voyager.Message, cutoff int64) []voyager.Message {
	out := make([]voyager.Message, 0, len(msgs))
	for _, m := range msgs {
		if m.SentAt >= cutoff {
			out = append(out, m)
		}
	}
	return out
}

func oldestSentAt(msgs []voyager.Message) int64 {
	oldest := msgs[0].SentAt
	for _, m := range msgs[1:] {
		if m.SentAt < oldest {
			oldest = m.SentAt
		}
	}
	return oldest
}

// firstParticipantName returns the first resolved display name among a
// conversation's participants, for a one-line progress status. A
// participant whose MiniProfile never resolved (empty Name — see
// voyager.Conversation.Participants) is skipped rather than shown as blank.
func firstParticipantName(participants []voyager.Participant) string {
	for _, p := range participants {
		if p.Name != "" {
			return p.Name
		}
	}
	return ""
}

// toStoreConversation translates the voyager API type to the store's
// persisted type. These are deliberately distinct types (see
// store.Conversation's doc comment) — this is the one place that bridges
// them, so a field added to either side has one obvious spot to wire up.
func toStoreConversation(c voyager.Conversation) store.Conversation {
	// A direct pairwise copy, not an index-zip: voyager.Conversation
	// already pairs each participant's name with its URN in a single
	// Participant value (see that type's doc comment for why parallel
	// slices were the defect this replaced), so there's no alignment to
	// get wrong here anymore.
	participants := make([]store.Participant, len(c.Participants))
	for i, p := range c.Participants {
		participants[i] = store.Participant{Name: p.Name, URN: p.URN}
	}
	return store.Conversation{
		ID:           c.ID,
		URN:          c.URN,
		Participants: participants,
		UpdatedAt:    c.UpdatedAt,
		Unread:       c.Unread,
	}
}

// toStoreMessages translates a page of voyager messages to the store's
// persisted type, stamping in the conversation id (the events endpoint
// response doesn't carry it — see voyager.MessagesPage).
func toStoreMessages(msgs []voyager.Message, conversationID string) []store.Message {
	out := make([]store.Message, len(msgs))
	for i, m := range msgs {
		out[i] = store.Message{
			URN:            m.URN,
			ConversationID: conversationID,
			SenderName:     m.From,
			SenderURN:      m.FromURN,
			SentAt:         m.SentAt,
			Body:           m.Text,
		}
	}
	return out
}
