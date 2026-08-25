package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
)

// Readers (this file) never take the sync lock (see Lock) — only a writer
// needs exclusivity, and letting export/search block on a long sync would
// defeat the point of separating the network pass from the read pass.

const conversationColumns = `id, urn, participants, updated_at, unread, newest_synced, oldest_synced, backfill_done, first_seen_at, last_synced_at`

// rowScanner is satisfied by both *sql.Row and *sql.Rows, so
// scanConversation works for a single-row lookup and a list query alike.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanConversation(rs rowScanner) (Conversation, error) {
	var (
		c                     Conversation
		participantsJSON      string
		unreadInt, backfillN  int
		newestSync, oldestSyn sql.NullInt64
	)
	if err := rs.Scan(&c.ID, &c.URN, &participantsJSON, &c.UpdatedAt, &unreadInt,
		&newestSync, &oldestSyn, &backfillN, &c.FirstSeenAt, &c.LastSyncedAt); err != nil {
		return Conversation{}, err
	}
	c.Unread = unreadInt != 0
	c.BackfillDone = backfillN != 0
	if newestSync.Valid {
		v := newestSync.Int64
		c.NewestSynced = &v
	}
	if oldestSyn.Valid {
		v := oldestSyn.Int64
		c.OldestSynced = &v
	}
	if err := json.Unmarshal([]byte(participantsJSON), &c.Participants); err != nil {
		return Conversation{}, fmt.Errorf("store: decode participants for %s: %w", c.ID, err)
	}
	return c, nil
}

// Conversation returns one conversation by id, and false if it isn't in the
// store.
func (s *Store) Conversation(ctx context.Context, id string) (Conversation, bool, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+conversationColumns+` FROM conversations WHERE id = ?`, id)
	c, err := scanConversation(row)
	if err == sql.ErrNoRows {
		return Conversation{}, false, nil
	}
	if err != nil {
		return Conversation{}, false, err
	}
	return c, true, nil
}

// Conversations returns every conversation in the store, most recently
// updated first — the order sync's discovery pass itself walks in, and a
// sensible default for anything enumerating "all conversations" (e.g.
// export with no --conversation filter).
func (s *Store) Conversations(ctx context.Context) ([]Conversation, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+conversationColumns+` FROM conversations ORDER BY updated_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Conversation
	for rows.Next() {
		c, err := scanConversation(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// MessageFilter narrows Messages. The zero value matches every message in
// the store.
type MessageFilter struct {
	// ConversationID restricts to one conversation; "" matches all.
	ConversationID string
	// After/Before bound SentAt (epoch ms), inclusive; nil is unbounded on
	// that side.
	After, Before *int64
	// Limit, when > 0, caps the result to the Limit most recent messages
	// matching the other filters — not the first Limit encountered, so a
	// caller asking for "the last 50 messages since March" gets the 50
	// closest to now rather than the 50 earliest after the --after cutoff.
	// The result is still returned oldest-first (see Messages), matching
	// the store's normal ordering; only the *selection* is newest-first.
	Limit int
}

// Messages returns messages matching f, oldest first. See MessageFilter.Limit
// for how a limited query selects which messages to include.
func (s *Store) Messages(ctx context.Context, f MessageFilter) ([]Message, error) {
	var where []string
	var args []any
	if f.ConversationID != "" {
		where = append(where, "conversation_id = ?")
		args = append(args, f.ConversationID)
	}
	if f.After != nil {
		where = append(where, "sent_at >= ?")
		args = append(args, *f.After)
	}
	if f.Before != nil {
		where = append(where, "sent_at <= ?")
		args = append(args, *f.Before)
	}
	whereClause := ""
	if len(where) > 0 {
		whereClause = "WHERE " + strings.Join(where, " AND ")
	}

	order := "ASC"
	limitClause := ""
	if f.Limit > 0 {
		// Select the most recent Limit rows first (DESC + LIMIT), then flip
		// to oldest-first below, rather than "ORDER BY sent_at ASC LIMIT N"
		// which would instead return the *oldest* N — the opposite of what
		// a size-capped export wants.
		order = "DESC"
		limitClause = fmt.Sprintf(" LIMIT %d", f.Limit)
	}

	query := fmt.Sprintf(
		`SELECT urn, conversation_id, sender_name, sender_urn, sent_at, body FROM messages %s ORDER BY sent_at %s%s`,
		whereClause, order, limitClause)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Message
	for rows.Next() {
		var m Message
		if err := rows.Scan(&m.URN, &m.ConversationID, &m.SenderName, &m.SenderURN, &m.SentAt, &m.Body); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	if order == "DESC" {
		for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
			out[i], out[j] = out[j], out[i]
		}
	}
	return out, nil
}

// Search returns messages whose body or sender name matches an FTS5 query,
// most relevant first, capped at limit (0 = FTS5's default, effectively
// unbounded for this store's size).
func (s *Store) Search(ctx context.Context, query string, limit int) ([]Message, error) {
	sqlQuery := `
		SELECT m.urn, m.conversation_id, m.sender_name, m.sender_urn, m.sent_at, m.body
		FROM messages_fts
		JOIN messages m ON m.rowid = messages_fts.rowid
		WHERE messages_fts MATCH ?
		ORDER BY rank`
	args := []any{query}
	if limit > 0 {
		sqlQuery += " LIMIT ?"
		args = append(args, limit)
	}

	rows, err := s.db.QueryContext(ctx, sqlQuery, args...)
	if err != nil {
		return nil, fmt.Errorf("store: search: %w", err)
	}
	defer rows.Close()

	var out []Message
	for rows.Next() {
		var m Message
		if err := rows.Scan(&m.URN, &m.ConversationID, &m.SenderName, &m.SenderURN, &m.SentAt, &m.Body); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// Empty reports whether the store holds no data at all — no conversations
// and no messages. Callers like `lion message export` use this to refuse an
// export before it happens, rather than silently writing an archive that
// looks like "you have no messages" (empty and successful look identical on
// disk otherwise).
func (s *Store) Empty(ctx context.Context) (bool, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM conversations`).Scan(&n); err != nil {
		return false, err
	}
	if n > 0 {
		return false, nil
	}
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM messages`).Scan(&n); err != nil {
		return false, err
	}
	return n == 0, nil
}

// DeleteConversation removes a conversation and, via ON DELETE CASCADE, all
// of its messages. Not currently wired to any CLI command — it exists so
// the cascade behavior itself is directly testable — but is a reasonable
// building block for a future `lion sync --forget` or similar.
func (s *Store) DeleteConversation(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM conversations WHERE id = ?`, id)
	return err
}
