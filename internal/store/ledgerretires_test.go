package store

import (
	"path/filepath"
	"slices"
	"testing"
)

// THE CONVERSATION LEDGER'S TABLE LEAVES A NODE FILE THAT HELD IT, its entries
// and its indexes with it (node migration 0060). A conversation's next turn
// continues its last turn's session and is told how the conversation's earlier
// turns ended from the executor log, and a table of rendered accounts kept
// beside that would be a second history of one conversation for a reader to
// take as authoritative.
//
// The file is built as a node that ran every migration before 0060 left it —
// the table holding an entry, under one of its indexes — by recording 0060 as
// not yet applied and putting the table back; reopening the file runs it.
//
// Mutation: empty 0060, or have it delete the table's rows and keep the table,
// and the table or its index is still in the file.
func TestTheConversationLedgerLeavesANodeFileThatHeldIt(t *testing.T) {
	t.Parallel()
	const version = "0060_the_conversation_ledger_retires.sql"
	path := filepath.Join(t.TempDir(), "node.db")
	db, err := OpenNode(t.Context(), path, Options{})
	if err != nil {
		t.Fatalf("OpenNode: %v", err)
	}
	for _, statement := range []string{
		`DELETE FROM schema_migrations WHERE version = '` + version + `'`,
		`CREATE TABLE conversation_sessions (
			id               INTEGER PRIMARY KEY AUTOINCREMENT,
			agent_handle     TEXT    NOT NULL,
			conversation_key TEXT    NOT NULL,
			entry            TEXT    NOT NULL,
			created_at       INTEGER NOT NULL
		)`,
		`CREATE INDEX conversation_sessions_lookup_idx
			ON conversation_sessions (agent_handle, conversation_key, created_at DESC)`,
		`INSERT INTO conversation_sessions (agent_handle, conversation_key, entry, created_at)
			VALUES ('ceo', 'slack:C1', '{}', 1)`,
	} {
		if _, err := db.SQL().ExecContext(t.Context(), statement); err != nil {
			t.Fatalf("put the ledger back: %v\n%s", err, statement)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	again, err := OpenNode(t.Context(), path, Options{})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = again.Close() }()
	var left int
	if err := again.SQL().QueryRowContext(t.Context(),
		`SELECT count(*) FROM sqlite_master WHERE name IN
			('conversation_sessions', 'conversation_sessions_lookup_idx')`).Scan(&left); err != nil {
		t.Fatalf("read the schema: %v", err)
	}
	if left != 0 {
		t.Errorf("%d of the ledger's table and its index are still in a node file 0060 ran on", left)
	}
	applied, err := again.AppliedMigrations(t.Context())
	if err != nil {
		t.Fatalf("AppliedMigrations: %v", err)
	}
	if !slices.Contains(applied, version) {
		t.Errorf("the reopened file records %v, without %s", applied, version)
	}
}
