package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
)

// Tx is a transaction-scoped handle for the mutations `lion sync` needs.
// Every method upserts (INSERT ... ON CONFLICT DO UPDATE) rather than
// inserting blindly, so re-running a sync — or resuming one that was
// interrupted — never duplicates a row. That property is what makes
// resumption and --follow safe: a page can be re-fetched and re-applied any
// number of times with the same result.
type Tx struct {
	tx *sql.Tx
}

// WithTx runs fn inside one transaction, committing on success and rolling
// back on any error (including a panic recovered by fn itself, which is
// still an error return). Callers should scope one call to one fetched page
// — one conversation-discovery page, or one conversation's message page —
// so an interruption between pages leaves the store consistent rather than
// with half a page applied.
func (s *Store) WithTx(ctx context.Context, fn func(*Tx) error) error {
	sqlTx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	t := &Tx{tx: sqlTx}
	if err := fn(t); err != nil {
		_ = sqlTx.Rollback() // best-effort; the original err is what matters
		return err
	}
	return sqlTx.Commit()
}

// UpsertConversation records (or refreshes) a conversation's discovery-time
// metadata: identity, participants, LinkedIn's own updated_at, and unread
// state. It deliberately does not touch the sync-progress fields
// (NewestSynced, OldestSynced, BackfillDone) or LastSyncedAt — those are
// only ever advanced by RecordMessagePage/MarkBackfillDone, which run after
// messages, not conversation summaries, were actually fetched. On first
// insert, FirstSeenAt and LastSyncedAt are both set to firstSeenAt (the
// caller's clock reading) since there is nothing more specific to record yet.
func (t *Tx) UpsertConversation(ctx context.Context, c Conversation, firstSeenAt int64) error {
	participantsJSON, err := json.Marshal(c.Participants)
	if err != nil {
		return fmt.Errorf("store: encode participants: %w", err)
	}
	_, err = t.tx.ExecContext(ctx, `
		INSERT INTO conversations (id, urn, participants, updated_at, unread, first_seen_at, last_synced_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			urn          = excluded.urn,
			-- Same reasoning as the message upsert: participants are resolved
			-- from the page's included[], so a later page can return this
			-- conversation with nobody resolved. Keeping the empty list would
			-- turn a named thread into an anonymous one on re-sync, so an empty
			-- incoming list leaves the stored one alone.
			participants = CASE
				WHEN excluded.participants IN ('[]', 'null', '') THEN conversations.participants
				ELSE excluded.participants
			END,
			updated_at   = excluded.updated_at,
			unread       = excluded.unread
	`, c.ID, c.URN, string(participantsJSON), c.UpdatedAt, boolToInt(c.Unread), firstSeenAt, firstSeenAt)
	if err != nil {
		return fmt.Errorf("store: upsert conversation %s: %w", c.ID, err)
	}
	return nil
}

// ErrConversationNotFound is returned by RecordMessagePage/MarkBackfillDone
// when the conversation hasn't been upserted yet. Sync always discovers a
// conversation (UpsertConversation) before paging its messages, so this
// signals a caller ordering bug rather than a data condition to recover
// from.
var ErrConversationNotFound = fmt.Errorf("store: conversation not upserted yet")

// RecordMessagePage upserts one fetched page of messages and extends the
// conversation's synced range (NewestSynced/OldestSynced) to cover it,
// merging with whatever range was already recorded rather than overwriting
// it — a catch-up page (extending the newest edge) and a --backfill page
// (extending the oldest edge) both call this, in either order, any number
// of times. It returns how many of the messages were new to the store
// (rather than an update to an already-stored one), for sync's summary.
//
// msgs must be non-empty and all belong to conversationID, which must
// already have a row (see UpsertConversation) — see ErrConversationNotFound.
func (t *Tx) RecordMessagePage(ctx context.Context, conversationID string, msgs []Message, now int64) (added int, err error) {
	if len(msgs) == 0 {
		return 0, nil
	}

	urns := make([]string, len(msgs))
	pageNewest, pageOldest := msgs[0].SentAt, msgs[0].SentAt
	for i, m := range msgs {
		urns[i] = m.URN
		if m.SentAt > pageNewest {
			pageNewest = m.SentAt
		}
		if m.SentAt < pageOldest {
			pageOldest = m.SentAt
		}
	}

	existing, err := t.existingMessageURNs(ctx, urns)
	if err != nil {
		return 0, fmt.Errorf("store: check existing messages: %w", err)
	}

	for _, m := range msgs {
		if _, err := t.tx.ExecContext(ctx, `
			INSERT INTO messages (urn, conversation_id, sender_name, sender_urn, sent_at, body)
			VALUES (?, ?, ?, ?, ?, ?)
			ON CONFLICT(urn) DO UPDATE SET
				conversation_id = excluded.conversation_id,
				-- Never let a blank re-fetch erase detail already held.
				-- LinkedIn's normalized responses carry included[] per page, so
				-- the same message seen again from a different page can fail to
				-- resolve its sender's MiniProfile and come back with an empty
				-- name/URN. A plain overwrite would quietly downgrade a complete
				-- archive on every re-sync, and the loss is permanent once the
				-- page that had the detail is out of reach.
				sender_name     = COALESCE(NULLIF(excluded.sender_name, ''), messages.sender_name),
				sender_urn      = COALESCE(NULLIF(excluded.sender_urn, ''),  messages.sender_urn),
				sent_at         = excluded.sent_at,
				body            = COALESCE(NULLIF(excluded.body, ''),        messages.body)
		`, m.URN, m.ConversationID, m.SenderName, m.SenderURN, m.SentAt, m.Body); err != nil {
			return 0, fmt.Errorf("store: upsert message %s: %w", m.URN, err)
		}
		if !existing[m.URN] {
			added++
		}
	}

	var curNewest, curOldest sql.NullInt64
	err = t.tx.QueryRowContext(ctx,
		`SELECT newest_synced, oldest_synced FROM conversations WHERE id = ?`, conversationID,
	).Scan(&curNewest, &curOldest)
	if err == sql.ErrNoRows {
		return 0, fmt.Errorf("%w: %s", ErrConversationNotFound, conversationID)
	}
	if err != nil {
		return 0, fmt.Errorf("store: read conversation %s bounds: %w", conversationID, err)
	}

	mergedNewest := pageNewest
	if curNewest.Valid && curNewest.Int64 > mergedNewest {
		mergedNewest = curNewest.Int64
	}
	mergedOldest := pageOldest
	if curOldest.Valid && curOldest.Int64 < mergedOldest {
		mergedOldest = curOldest.Int64
	}

	if _, err := t.tx.ExecContext(ctx, `
		UPDATE conversations
		SET newest_synced = ?, oldest_synced = ?, last_synced_at = ?
		WHERE id = ?
	`, mergedNewest, mergedOldest, now, conversationID); err != nil {
		return 0, fmt.Errorf("store: update conversation %s bounds: %w", conversationID, err)
	}

	return added, nil
}

// MarkBackfillDone flips a conversation's BackfillDone flag once --backfill
// paging has walked all the way back to an empty page — i.e. there is
// nothing older left to fetch. It's a separate call from RecordMessagePage
// (rather than a parameter on it) because the terminal page carries no
// messages and therefore no bounds to merge; it only has this one bit of
// news to record.
func (t *Tx) MarkBackfillDone(ctx context.Context, conversationID string, now int64) error {
	res, err := t.tx.ExecContext(ctx, `
		UPDATE conversations SET backfill_done = 1, last_synced_at = ? WHERE id = ?
	`, now, conversationID)
	if err != nil {
		return fmt.Errorf("store: mark backfill done for %s: %w", conversationID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("%w: %s", ErrConversationNotFound, conversationID)
	}
	return nil
}

// inClauseChunk bounds how many placeholders existingMessageURNs puts in a
// single IN (...) clause. SQLite's default compiled-in limit on bound
// parameters (SQLITE_MAX_VARIABLE_NUMBER) is comfortably above this for any
// build lion ships against, so this is about keeping one query's argument
// list a sane size rather than working around a real ceiling — a page's
// message count is normally far below it anyway.
const inClauseChunk = 400

// existingMessageURNs returns the subset of urns already present in the
// messages table, so RecordMessagePage can report how many of a page's
// messages were actually new rather than a re-fetched duplicate.
func (t *Tx) existingMessageURNs(ctx context.Context, urns []string) (map[string]bool, error) {
	existing := make(map[string]bool, len(urns))
	for i := 0; i < len(urns); i += inClauseChunk {
		end := min(i+inClauseChunk, len(urns))
		chunk := urns[i:end]

		placeholders := strings.TrimSuffix(strings.Repeat("?,", len(chunk)), ",")
		args := make([]any, len(chunk))
		for j, u := range chunk {
			args[j] = u
		}

		rows, err := t.tx.QueryContext(ctx, `SELECT urn FROM messages WHERE urn IN (`+placeholders+`)`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var u string
			if err := rows.Scan(&u); err != nil {
				rows.Close()
				return nil, err
			}
			existing[u] = true
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
	}
	return existing, nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
