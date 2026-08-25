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

// ForEachMessage streams messages matching f to fn, oldest first, without
// ever materializing the matching set into a slice — unlike Messages, which
// this otherwise mirrors exactly (see MessageFilter.Limit for what "most
// recent N, emitted oldest-first" means). It exists for internal/cli's jsonl
// export path: an archive large enough to matter is exactly the one that
// can't be safely loaded into memory before the first byte is written.
//
// Limit's selection still can't be done by scanning forward and stopping
// early — the most recent N rows are the ones nearest the *end* of
// oldest-first order — so it's expressed in SQL instead of in Go: an inner
// query does ORDER BY sent_at DESC LIMIT N to pick the right rows, and an
// outer query re-sorts just that (Limit-sized, not table-sized) result back
// to oldest-first. That keeps the selection itself from ever buffering more
// than Limit rows, matching the no-buffering promise for the common case
// where Limit is unset (0) too.
//
// fn is called once per matching row in final order; an error from fn stops
// iteration immediately and is returned as-is, so a caller streaming into an
// io.Writer can propagate a write failure straight out of ForEachMessage.
func (s *Store) ForEachMessage(ctx context.Context, f MessageFilter, fn func(Message) error) error {
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

	const cols = "urn, conversation_id, sender_name, sender_urn, sent_at, body"
	query := fmt.Sprintf(`SELECT %s FROM messages %s ORDER BY sent_at ASC`, cols, whereClause)
	if f.Limit > 0 {
		// f.Limit is formatted directly for the same reason Messages' own
		// LIMIT clause is: it's a validated Go int (the CLI rejects
		// --limit < 0 before this is ever called), never a raw caller
		// string, so there's no injection surface to bind a parameter
		// against.
		query = fmt.Sprintf(
			`SELECT %s FROM (SELECT %s FROM messages %s ORDER BY sent_at DESC LIMIT %d) newest_first ORDER BY sent_at ASC`,
			cols, cols, whereClause, f.Limit)
	}

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var m Message
		if err := rows.Scan(&m.URN, &m.ConversationID, &m.SenderName, &m.SenderURN, &m.SentAt, &m.Body); err != nil {
			return err
		}
		if err := fn(m); err != nil {
			return err
		}
	}
	return rows.Err()
}

// SearchFilter narrows Search. Query is the only required field; the rest
// mirror MessageFilter so `lion message search` can layer the same
// --conversation/--after/--before narrowing on top of the FTS match, plus
// --from (matched against sender name, substring, and sender URN, exact).
type SearchFilter struct {
	// Query is the FTS5 match expression, checked against message bodies
	// and sender names (see schemaV1's messages_fts columns).
	Query string
	// ConversationID restricts to one conversation; "" matches all.
	ConversationID string
	// From narrows by sender: a case-insensitive substring match against
	// the stored sender name, OR an exact match against the sender URN —
	// accepting either is what lets a caller pass either half of `--from
	// NAME-or-URN` without first resolving one to the other.
	From string
	// After/Before bound SentAt (epoch ms), inclusive; nil is unbounded on
	// that side. Same semantics as MessageFilter.After/Before.
	After, Before *int64
	// Limit, when > 0, caps the number of results returned.
	Limit int
	// Asc returns oldest-first instead of the default newest-first — the
	// ordering search results actually get sorted by (see below), not
	// FTS5's relevance rank: a message search is a way to jump into
	// conversation history at a point in time, so "most recent match
	// first" is the useful default, matching wacli's own `messages search`.
	Asc bool
}

// Search returns messages matching an FTS5 query and f's other filters,
// newest-first by default (Asc for oldest-first), capped at f.Limit (0 = no
// cap).
func (s *Store) Search(ctx context.Context, f SearchFilter) ([]Message, error) {
	where := []string{"messages_fts MATCH ?"}
	args := []any{f.Query}
	if f.ConversationID != "" {
		where = append(where, "m.conversation_id = ?")
		args = append(args, f.ConversationID)
	}
	if f.From != "" {
		where = append(where, "(m.sender_name LIKE ? OR m.sender_urn = ?)")
		args = append(args, "%"+f.From+"%", f.From)
	}
	if f.After != nil {
		where = append(where, "m.sent_at >= ?")
		args = append(args, *f.After)
	}
	if f.Before != nil {
		where = append(where, "m.sent_at <= ?")
		args = append(args, *f.Before)
	}

	order := "DESC"
	if f.Asc {
		order = "ASC"
	}

	sqlQuery := fmt.Sprintf(`
		SELECT m.urn, m.conversation_id, m.sender_name, m.sender_urn, m.sent_at, m.body
		FROM messages_fts
		JOIN messages m ON m.rowid = messages_fts.rowid
		WHERE %s
		ORDER BY m.sent_at %s`, strings.Join(where, " AND "), order)
	if f.Limit > 0 {
		sqlQuery += " LIMIT ?"
		args = append(args, f.Limit)
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
// of its messages. Used by `lion store cleanup`; also exists independently
// of that command so the cascade behavior itself is directly testable.
func (s *Store) DeleteConversation(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM conversations WHERE id = ?`, id)
	return err
}

// ConversationCoverage answers "how much of this conversation's history does
// the store actually hold?" — the question `lion history coverage` exists
// to answer, since for a backup tool that's the whole point.
type ConversationCoverage struct {
	Conversation
	// MessageCount is how many messages this store holds for the
	// conversation — distinct from "how many exist on LinkedIn", which
	// lion has no way to ask for directly; BackfillDone is the closer proxy
	// for completeness.
	MessageCount int
}

// Coverage returns per-conversation coverage, newest-activity first
// (matching Conversations' ordering). id, when non-empty, restricts the
// result to one conversation; an unknown id returns an empty slice rather
// than an error, mirroring Conversation's not-found-is-a-bool convention —
// callers that need to distinguish "unknown id" from "no messages yet" use
// Conversation's ok return for that check first.
func (s *Store) Coverage(ctx context.Context, id string) ([]ConversationCoverage, error) {
	// conversationColumns is unqualified (fine for Conversation/Conversations,
	// which never join); joined against messages here — which has its own
	// urn column — "urn" alone would be an ambiguous-column error, so every
	// column is qualified to the conversations side explicitly.
	query := `
		SELECT c.` + strings.ReplaceAll(conversationColumns, ", ", ", c.") + `, count(m.urn)
		FROM conversations c
		LEFT JOIN messages m ON m.conversation_id = c.id`
	var args []any
	if id != "" {
		query += " WHERE c.id = ?"
		args = append(args, id)
	}
	query += " GROUP BY c.id ORDER BY c.updated_at DESC"

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: coverage: %w", err)
	}
	defer rows.Close()

	var out []ConversationCoverage
	for rows.Next() {
		// scanConversation reads exactly conversationColumns' worth of
		// dest, so the trailing count needs its own Scan target chained
		// onto the same *sql.Rows — coverageRowScanner below bridges that.
		var count int
		c, err := scanConversation(&coverageRowScanner{rows: rows, count: &count})
		if err != nil {
			return nil, err
		}
		out = append(out, ConversationCoverage{Conversation: c, MessageCount: count})
	}
	return out, rows.Err()
}

// coverageRowScanner adapts one *sql.Rows call to scanConversation's
// rowScanner interface when the query has one extra trailing column
// (count(m.urn)) beyond conversationColumns — it appends its own dest to
// whatever scanConversation passes in, rather than duplicating
// scanConversation's field-by-field decode logic for a query that differs
// from it by exactly one column.
type coverageRowScanner struct {
	rows  *sql.Rows
	count *int
}

func (c *coverageRowScanner) Scan(dest ...any) error {
	return c.rows.Scan(append(dest, c.count)...)
}

// ConversationsOlderThan returns conversations whose UpdatedAt (LinkedIn's
// own last-activity timestamp) is strictly before cutoffMs, oldest-activity
// first — the candidate set `lion store cleanup --days N` considers for
// removal, listed oldest-first so a --dry-run reads as "these have been
// stale longest" rather than an arbitrary order.
func (s *Store) ConversationsOlderThan(ctx context.Context, cutoffMs int64) ([]Conversation, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+conversationColumns+` FROM conversations WHERE updated_at < ? ORDER BY updated_at ASC`, cutoffMs)
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

// Stats is the store-wide summary `lion store stats` reports.
type Stats struct {
	Conversations int
	Messages      int
	// OldestMessage/NewestMessage are nil when the store holds no messages.
	OldestMessage, NewestMessage *int64
	SchemaVersion                int
	// LastSyncedAt is the most recent LastSyncedAt across every
	// conversation (i.e. the last time any `lion sync` actually wrote
	// something), nil when the store holds no conversations yet.
	LastSyncedAt *int64
}

// Stats computes store-wide counts and bounds. Unlike SizeBytes (a plain
// stat call, not a query), this belongs in query.go since it reads table
// contents.
func (s *Store) Stats(ctx context.Context) (Stats, error) {
	var st Stats

	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM conversations`).Scan(&st.Conversations); err != nil {
		return Stats{}, err
	}

	var msgCount sql.NullInt64
	var oldest, newest, lastSynced sql.NullInt64
	if err := s.db.QueryRowContext(ctx,
		`SELECT count(*), min(sent_at), max(sent_at) FROM messages`,
	).Scan(&msgCount, &oldest, &newest); err != nil {
		return Stats{}, err
	}
	st.Messages = int(msgCount.Int64)
	if oldest.Valid {
		v := oldest.Int64
		st.OldestMessage = &v
	}
	if newest.Valid {
		v := newest.Int64
		st.NewestMessage = &v
	}

	if err := s.db.QueryRowContext(ctx,
		`SELECT max(last_synced_at) FROM conversations`,
	).Scan(&lastSynced); err != nil {
		return Stats{}, err
	}
	if lastSynced.Valid {
		v := lastSynced.Int64
		st.LastSyncedAt = &v
	}

	version, err := s.schemaVersion(ctx)
	if err != nil {
		return Stats{}, err
	}
	st.SchemaVersion = version

	return st, nil
}
