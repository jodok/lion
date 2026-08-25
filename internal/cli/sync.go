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
		backfill         bool
		after            string
		maxConversations int
		maxMessages      int
		maxDBSize        string
		storePath        string
		once             bool
		follow           bool
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
				backfill:         backfill,
				afterMs:          afterMs,
				maxConversations: maxConversations,
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

	cmd.Flags().BoolVar(&backfill, "backfill", false, "after catching up, page backwards from the oldest stored message until history is exhausted")
	cmd.Flags().StringVar(&after, "after", "", "only sync conversations/messages at or after this time (RFC3339 or YYYY-MM-DD)")
	cmd.Flags().IntVar(&maxConversations, "max-conversations", 0, "cap the number of conversations processed this run (0 = no cap)")
	cmd.Flags().IntVar(&maxMessages, "max-messages", 0, "cap the number of messages fetched this run (0 = no cap)")
	cmd.Flags().StringVar(&maxDBSize, "max-db-size", "", "stop cleanly before the store would exceed this size (e.g. 500MB, 2GB)")
	cmd.Flags().StringVar(&storePath, "store", "", "path to the store database (default $LION_HOME/store.db)")
	cmd.Flags().BoolVar(&once, "once", false, "sync a single pass and exit (the default; accepted explicitly to match wacli)")
	cmd.Flags().BoolVar(&follow, "follow", false, "keep syncing every --interval instead of exiting after one pass (opt-in — see command help)")
	cmd.Flags().DurationVar(&interval, "interval", 60*time.Second, "how often to sync again under --follow")
	cmd.Flags().BoolVar(&events, "events", false, "emit NDJSON progress events on stderr instead of a status line")
	cmd.Flags().DurationVar(&lockWait, "lock-wait", 0, "wait up to this long for another `lion sync` to release the store lock instead of failing immediately")
	return cmd
}

// syncOptions bundles one pass's parameters so runSyncPass doesn't carry a
// long, error-prone positional-argument list.
type syncOptions struct {
	backfill         bool
	afterMs          *int64
	maxConversations int
	maxMessages      int
	maxDBSizeBytes   int64
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

		if opts.backfill {
			bAdded, backfillComplete, err := backfillMessages(ctx, cl, st, conv.ID, opts, budget, progress)
			summary.MessagesAdded += bAdded
			if bAdded > 0 && added == 0 {
				summary.ConversationsUpdated++
			}
			if !backfillComplete {
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
		}
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

// discoverConversations pages newest-to-older through ConversationsPage,
// upserting each page (one store transaction per page) and collecting the
// conversations this pass will sync messages for. It stops when
// --max-conversations is reached, when a page's oldest conversation is
// already older than --after (pages are newest-first, so nothing further
// back can pass the filter either), or when there are no more pages.
//
// The returned bool is true only when discovery ran to genuine exhaustion
// (an empty page, or the --after cutoff) — it is false when
// --max-conversations cut the walk short or when ConversationsPage reported
// ErrPaginationStalled, since in both cases older conversations may exist
// that this pass never saw. That's a truncation, not a failure: the error
// return stays nil so a caller doesn't turn it into a non-zero exit, but the
// caller must fold the bool into the pass's complete:false summary field.
func discoverConversations(ctx context.Context, cl *voyager.Client, st *store.Store, opts syncOptions, progress *progressReporter) ([]voyager.Conversation, bool, error) {
	var toProcess []voyager.Conversation
	var createdBefore int64

	for {
		if ctx.Err() != nil {
			return toProcess, false, ctx.Err()
		}
		if opts.maxDBSizeBytes > 0 {
			// Same ordering rule as catchUpMessages/backfillMessages: checked
			// before this page is even fetched, never after it's already been
			// committed via UpsertConversation. Discovery used to have no
			// --max-db-size check at all, so a store already at the limit
			// still got mutated, and kept growing through the whole
			// discovery walk regardless of the bound.
			if reached, sErr := storeSizeReached(st, opts.maxDBSizeBytes); sErr != nil {
				return toProcess, false, sErr
			} else if reached {
				return toProcess, false, errMaxDBSizeReached
			}
		}
		convs, next, err := cl.ConversationsPage(ctx, createdBefore, defaultConversationPageSize)
		stalled := errors.Is(err, voyager.ErrPaginationStalled)
		if err != nil && !stalled {
			return toProcess, false, err
		}
		if len(convs) == 0 {
			return toProcess, true, nil
		}

		now := time.Now().UnixMilli()
		txErr := st.WithTx(ctx, func(tx *store.Tx) error {
			for _, c := range convs {
				if c.ID == "" {
					// conversationIDFromURN couldn't parse this one (an
					// unexpected URN shape) — nothing to key a row on, so
					// skip it rather than guess an id (see voyager.Conversation.ID).
					progress.Warn("skipping a conversation with no parseable id (urn=%q)", c.URN)
					continue
				}
				if err := tx.UpsertConversation(ctx, toStoreConversation(c), now); err != nil {
					return err
				}
			}
			return nil
		})
		if txErr != nil {
			// SizeBytes() is a pre-check, advisory by nature (it can't predict
			// a not-yet-fetched page's own footprint), so the hard backstop —
			// SQLite itself refusing a transaction that would exceed
			// PRAGMA max_page_count, see store.SetMaxSize — is what actually
			// guarantees the bound when a single page is bigger than the
			// pre-check anticipated. translateStoreFull folds that into the
			// same errMaxDBSizeReached truncation every other --max-db-size
			// stop already reports.
			return toProcess, false, translateStoreFull(txErr)
		}

		oldestInPage := convs[0].UpdatedAt
		for _, c := range convs {
			if c.UpdatedAt < oldestInPage {
				oldestInPage = c.UpdatedAt
			}
			if c.ID == "" {
				continue
			}
			if opts.afterMs != nil && c.UpdatedAt < *opts.afterMs {
				continue
			}
			toProcess = append(toProcess, c)
			progress.Event("conversation_discovered", map[string]any{"id": c.ID, "urn": c.URN})
			if opts.maxConversations > 0 && len(toProcess) >= opts.maxConversations {
				progress.Warn("--max-conversations reached; older conversations were not discovered")
				return toProcess, false, nil
			}
		}

		if stalled {
			progress.Warn("conversations pagination cursor did not advance; older conversations may not have been discovered")
			return toProcess, false, nil
		}
		if next == 0 {
			return toProcess, true, nil
		}
		if opts.afterMs != nil && oldestInPage < *opts.afterMs {
			return toProcess, true, nil
		}
		createdBefore = next
	}
}

// catchUpMessages walks a conversation's messages newest-to-older, one page
// per store transaction, stopping as soon as a fetched page contains a
// message already in the store (RecordMessagePage's added count comes back
// short of the page size) — that's the "already caught up" signal, per
// sync's catch-up contract. It also stops at --after, at a --max-messages
// budget, or at --max-db-size.
//
// The returned bool is true only when the walk reached a genuine stopping
// point (an empty page, or --after) — it is false when a --max-messages
// budget ran out before that, when MessagesPage reported
// ErrPaginationStalled, or when it stopped on an already-known message
// without the conversation's history having actually reached its true start
// (see the pageAdded < len(msgs) branch below for why hitting a known
// message alone isn't proof of that). A caller must fold that into the
// pass's complete:false summary field; it is not itself a returned error,
// matching --max-db-size's existing "truncated, not failed" treatment via
// errMaxDBSizeReached below.
func catchUpMessages(ctx context.Context, cl *voyager.Client, st *store.Store, conv voyager.Conversation, opts syncOptions, budget *int, progress *progressReporter) (added int, complete bool, err error) {
	var createdBefore int64
	for {
		if ctx.Err() != nil {
			return added, false, ctx.Err()
		}
		if budget != nil && *budget <= 0 {
			return added, false, nil
		}
		if opts.maxDBSizeBytes > 0 {
			// Checked before this page is even fetched, never after it's
			// already committed — the defect this guards against was
			// exactly that ordering: a store already at --max-db-size kept
			// growing because the check only ran once a page had already
			// landed. SizeBytes() only reflects what's actually durable on
			// disk, so this can't predict a not-yet-fetched page's own
			// footprint in advance and stop that specific page from
			// landing — but nothing further is ever attempted once the
			// bound is reached, and a store already at the limit is never
			// touched at all.
			if reached, sErr := storeSizeReached(st, opts.maxDBSizeBytes); sErr != nil {
				return added, false, sErr
			} else if reached {
				return added, false, errMaxDBSizeReached
			}
		}
		pageSize := defaultMessagePageSize
		if budget != nil && *budget < pageSize {
			pageSize = *budget
		}

		msgs, next, err := cl.MessagesPage(ctx, conv.ID, createdBefore, pageSize)
		stalled := errors.Is(err, voyager.ErrPaginationStalled)
		if err != nil && !stalled {
			return added, false, err
		}
		if len(msgs) == 0 {
			// A genuine end-of-history signal, same as backfillMessages'
			// own empty-page case: catch-up can reach it directly for a
			// conversation whose entire history fits before ever hitting a
			// duplicate (e.g. a brand new conversation, or one short enough
			// that a single pass covers it). Marking BackfillDone here too
			// means a later plain sync never needs --backfill to know
			// there's nothing older left for this conversation.
			if err := markBackfillDone(ctx, st, conv.ID); err != nil {
				return added, false, err
			}
			return added, true, nil
		}

		// toStore, not msgs, is what actually gets persisted below — every
		// pagination/completeness decision from here on (the stalled check,
		// the duplicate-page check, next, the --after cutoff itself) keeps
		// reading msgs, the page the server actually returned, never
		// toStore. Filtering only decides what applyMessagePage is allowed
		// to write; conflating the two would make a --after-trimmed page
		// look like a short or duplicate page to that cursor logic, which
		// has nothing to do with why it was trimmed.
		toStore := msgs
		if opts.afterMs != nil {
			toStore = messagesAtOrAfter(msgs, *opts.afterMs)
		}
		pageAdded, aErr := applyMessagePage(ctx, st, conv.ID, toStore)
		if aErr != nil {
			return added, false, aErr
		}
		added += pageAdded
		if budget != nil {
			*budget -= len(msgs)
		}
		progress.Status("syncing %s: +%d messages (%d so far)", conv.ID, pageAdded, added)
		progress.Event("messages_stored", map[string]any{"conversation_id": conv.ID, "added": pageAdded, "phase": "catch_up"})

		// stalled must be checked before the duplicate-page branch below: a
		// server that ignores createdBefore keeps re-serving the same page,
		// which — once this loop has already stored it once — looks
		// identical to a legitimate "caught up" duplicate (pageAdded <
		// len(toStore), or even pageAdded == 0). Checking the dup case first
		// would silently treat a stalled cursor as a successful catch-up,
		// bypassing ErrPaginationStalled's guard in exactly the failure
		// mode it exists to catch.
		if stalled {
			progress.Warn("messages pagination cursor did not advance for conversation %s; older messages may not have been fetched", conv.ID)
			return added, false, nil
		}
		if pageAdded < len(toStore) {
			// This page reconnected with a message already in the store —
			// proof catch-up has caught this conversation up to whatever
			// newest range was previously synced. It is NOT proof the
			// conversation's full history (back to its true start) is held:
			// that range could itself be the product of an earlier
			// interrupted or --max-messages-capped run that never reached
			// an empty page. Trust "caught up" as "conversation fully
			// synced" only when a prior pass already walked all the way
			// back (BackfillDone); otherwise stop here — a --backfill pass
			// is what actually resumes from OldestSynced (see
			// backfillMessages) — but report the pass incomplete so the
			// summary doesn't claim a full archive that isn't one.
			//
			// Comparing against len(toStore) rather than len(msgs) matters
			// once --after is set: toStore can be shorter than msgs simply
			// because older messages were trimmed, which is not evidence of
			// a duplicate and must not be misread as one — the cutoff check
			// below is what correctly recognizes that case.
			done, dErr := conversationBackfillDone(ctx, st, conv.ID)
			if dErr != nil {
				return added, false, dErr
			}
			return added, done, nil
		}
		if next == 0 {
			if err := markBackfillDone(ctx, st, conv.ID); err != nil {
				return added, false, err
			}
			return added, true, nil
		}
		if opts.afterMs != nil && oldestSentAt(msgs) < *opts.afterMs {
			return added, true, nil
		}
		createdBefore = next
	}
}

// conversationBackfillDone reports whether a previous pass already walked a
// conversation's history all the way back to an empty page (see
// MarkBackfillDone). catchUpMessages consults this before trusting a
// duplicate-page stop as genuine completion — see that function's doc
// comment.
func conversationBackfillDone(ctx context.Context, st *store.Store, conversationID string) (bool, error) {
	conv, ok, err := st.Conversation(ctx, conversationID)
	if err != nil {
		return false, err
	}
	return ok && conv.BackfillDone, nil
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

// backfillMessages continues paging a conversation's messages backwards
// from where it's already synced to (OldestSynced) until a page comes back
// empty — the signal that paging reached the true start of the
// conversation — at which point it marks BackfillDone so a later plain sync
// knows there's nothing older to fetch. Stopping early (--after, a budget,
// --max-db-size, or MessagesPage reporting ErrPaginationStalled) does NOT
// set BackfillDone: only actually reaching the start does.
//
// The returned bool mirrors catchUpMessages': true only when this walk
// reached a genuine stopping point (the true start), false when a
// --max-messages budget or a stalled cursor cut it short and older messages
// may remain. A caller must fold that into the pass's complete:false
// summary field rather than only checking the error, which stays nil for
// this kind of truncation (see catchUpMessages' doc comment for why).
func backfillMessages(ctx context.Context, cl *voyager.Client, st *store.Store, conversationID string, opts syncOptions, budget *int, progress *progressReporter) (added int, complete bool, err error) {
	current, ok, err := st.Conversation(ctx, conversationID)
	if err != nil {
		return 0, false, err
	}
	if !ok || current.BackfillDone {
		return 0, true, nil
	}
	var createdBefore int64
	if current.OldestSynced != nil {
		createdBefore = *current.OldestSynced
	}

	for {
		if ctx.Err() != nil {
			return added, false, ctx.Err()
		}
		if budget != nil && *budget <= 0 {
			return added, false, nil
		}
		if opts.maxDBSizeBytes > 0 {
			// See catchUpMessages' identical guard for why this runs before
			// the page is fetched rather than after it's already committed.
			if reached, sErr := storeSizeReached(st, opts.maxDBSizeBytes); sErr != nil {
				return added, false, sErr
			} else if reached {
				return added, false, errMaxDBSizeReached
			}
		}
		pageSize := defaultMessagePageSize
		if budget != nil && *budget < pageSize {
			pageSize = *budget
		}

		msgs, next, err := cl.MessagesPage(ctx, conversationID, createdBefore, pageSize)
		stalled := errors.Is(err, voyager.ErrPaginationStalled)
		if err != nil && !stalled {
			return added, false, err
		}
		if len(msgs) == 0 {
			if err := markBackfillDone(ctx, st, conversationID); err != nil {
				return added, false, err
			}
			progress.Event("backfill_done", map[string]any{"conversation_id": conversationID})
			return added, true, nil
		}

		// See catchUpMessages' identical split: toStore is what actually
		// gets persisted, but next/stalled/the --after check below all keep
		// reading the unfiltered msgs the server returned.
		toStore := msgs
		if opts.afterMs != nil {
			toStore = messagesAtOrAfter(msgs, *opts.afterMs)
		}
		pageAdded, aErr := applyMessagePage(ctx, st, conversationID, toStore)
		if aErr != nil {
			return added, false, aErr
		}
		added += pageAdded
		if budget != nil {
			*budget -= len(msgs)
		}
		progress.Status("backfilling %s: +%d messages (%d so far)", conversationID, pageAdded, added)
		progress.Event("messages_stored", map[string]any{"conversation_id": conversationID, "added": pageAdded, "phase": "backfill"})

		// Ordering audit (see catchUpMessages' equivalent comment, added for
		// the same class of bug): unlike catchUpMessages, none of the
		// branches below ever report success (added, true, nil) — every one
		// returns added, false, nil (or an error) — so there's no "treat a
		// stalled page as a duplicate-triggered success" hazard here for
		// stalled to be checked ahead of. The order among them only changes
		// which stderr warning fires, not the returned completeness.
		if opts.afterMs != nil && oldestSentAt(msgs) < *opts.afterMs {
			return added, false, nil // hit the user's --after bound, not the true start
		}
		if stalled {
			progress.Warn("messages pagination cursor did not advance while backfilling conversation %s; older messages may not have been fetched", conversationID)
			return added, false, nil
		}
		if next == 0 {
			return added, false, nil // shouldn't happen without stalled being set; treated conservatively as incomplete
		}
		createdBefore = next
	}
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
