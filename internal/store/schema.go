package store

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
)

// currentSchemaVersion is the schema this build knows how to create/migrate
// to. Bumping it and adding a case to migrate is how a later version adds
// its own migration step.
const currentSchemaVersion = 2

// schemaV1 creates the store from nothing. Each statement runs individually
// (rather than as one multi-statement Exec) so this doesn't depend on a
// driver's multi-statement support, which varies.
//
// FTS5 uses external content (content='messages', content_rowid='rowid') so
// message bodies aren't duplicated between `messages` and `messages_fts` —
// the index stores only the inverted index, not a second copy of the text.
// The trade-off is that nothing keeps the two in sync automatically; the
// three triggers below do that by hand, mirroring SQLite's own documented
// pattern for external-content tables.
var schemaV1 = []string{
	`CREATE TABLE meta (
		key   TEXT PRIMARY KEY,
		value TEXT NOT NULL
	)`,

	`CREATE TABLE conversations (
		id             TEXT PRIMARY KEY,          -- raw id from urn:li:fs_conversation:<id>
		urn            TEXT NOT NULL,
		participants   TEXT NOT NULL,             -- JSON array of {name,urn}
		updated_at     INTEGER NOT NULL,          -- epoch ms
		unread         INTEGER NOT NULL DEFAULT 0,
		newest_synced  INTEGER,                   -- epoch ms of newest message stored
		oldest_synced  INTEGER,                   -- epoch ms of oldest message stored
		backfill_done  INTEGER NOT NULL DEFAULT 0,-- 1 once paging reached the start
		first_seen_at  INTEGER NOT NULL,
		last_synced_at INTEGER NOT NULL
	)`,

	`CREATE TABLE messages (
		urn             TEXT PRIMARY KEY,
		conversation_id TEXT NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
		sender_name     TEXT,
		sender_urn      TEXT,
		sent_at         INTEGER NOT NULL,
		body            TEXT
	)`,

	`CREATE INDEX messages_conv_time ON messages(conversation_id, sent_at)`,

	`CREATE VIRTUAL TABLE messages_fts USING fts5(
		body, sender_name, content='messages', content_rowid='rowid'
	)`,

	// AFTER INSERT: mirror the new row into the index.
	`CREATE TRIGGER messages_ai AFTER INSERT ON messages BEGIN
		INSERT INTO messages_fts(rowid, body, sender_name)
		VALUES (new.rowid, new.body, new.sender_name);
	END`,

	// AFTER DELETE: the 'delete' form of an external-content insert is
	// FTS5's documented way to remove one row from the index without a full
	// rebuild — a plain DELETE against messages_fts isn't supported for
	// external-content tables.
	`CREATE TRIGGER messages_ad AFTER DELETE ON messages BEGIN
		INSERT INTO messages_fts(messages_fts, rowid, body, sender_name)
		VALUES('delete', old.rowid, old.body, old.sender_name);
	END`,

	// AFTER UPDATE: an upsert's ON CONFLICT ... DO UPDATE fires this (not
	// the INSERT trigger above), so re-syncing an edited/re-fetched message
	// must delete-then-reinsert the index entry, matching FTS5's documented
	// pattern for updating an external-content row.
	`CREATE TRIGGER messages_au AFTER UPDATE ON messages BEGIN
		INSERT INTO messages_fts(messages_fts, rowid, body, sender_name)
		VALUES('delete', old.rowid, old.body, old.sender_name);
		INSERT INTO messages_fts(rowid, body, sender_name)
		VALUES (new.rowid, new.body, new.sender_name);
	END`,

	`INSERT INTO meta (key, value) VALUES ('schema_version', '1')`,
}

// migrations maps a target schema version to the statements that create or
// upgrade to it. v1 creates from nothing; a v2 would append its own ALTER
// TABLE / CREATE statements here rather than rewriting v1's.
var migrations = map[int][]string{
	1: schemaV1,
	2: schemaV2,
}

// schemaV2 adds per-conversation message sync tokens.
//
// LinkedIn's messaging surface is a sync-token protocol: a response carries a
// token, and passing it back asks only for what changed since. Without
// somewhere to keep the token, every run had to take a full snapshot of every
// conversation. The column lives on conversations rather than in meta so it
// is scoped to the row it describes and disappears with it on delete —
// keeping a token for a conversation that no longer exists would be a way to
// resume a stream that has nothing to resume.
var schemaV2 = []string{
	`ALTER TABLE conversations ADD COLUMN messages_sync_token TEXT`,
}

// migrate brings the store up to currentSchemaVersion, running whichever
// migrations haven't applied yet inside one transaction so a crash mid-
// migration can't leave the schema half-created.
func (s *Store) migrate(ctx context.Context) error {
	version, err := s.schemaVersion(ctx)
	if err != nil {
		return err
	}
	if version >= currentSchemaVersion {
		return nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // no-op once Commit succeeds

	for v := version + 1; v <= currentSchemaVersion; v++ {
		stmts, ok := migrations[v]
		if !ok {
			return errUnknownSchemaVersion(v)
		}
		for _, stmt := range stmts {
			if _, err := tx.ExecContext(ctx, stmt); err != nil {
				return err
			}
		}
		// schemaV1 already seeds meta.schema_version via its own INSERT; a
		// later migration that doesn't create meta from scratch needs an
		// explicit upsert instead.
		if v > 1 {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO meta (key, value) VALUES ('schema_version', ?)
				 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
				strconv.Itoa(v)); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

func errUnknownSchemaVersion(v int) error {
	return &unknownSchemaVersionError{v: v}
}

type unknownSchemaVersionError struct{ v int }

func (e *unknownSchemaVersionError) Error() string {
	return "store: no migration registered for schema version " + strconv.Itoa(e.v)
}

// schemaVersion reads meta.schema_version, returning 0 for a brand-new
// database (no meta table yet) rather than an error — that's the normal
// starting point migrate is meant to handle, not a failure.
func (s *Store) schemaVersion(ctx context.Context) (int, error) {
	var name string
	err := s.db.QueryRowContext(ctx,
		`SELECT name FROM sqlite_master WHERE type='table' AND name='meta'`).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}

	var v string
	err = s.db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key='schema_version'`).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, err
	}
	return n, nil
}
