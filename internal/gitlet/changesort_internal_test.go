package gitlet

import (
	"context"
	"database/sql"
	"maps"
	"slices"
	"testing"
)

// A CONVERSATION WRITE REWRITES THE MOVED INDEXES ONLY WHEN SOMEBODY SPOKE: the home's touch
// names no instant, so it rewrites neither the company's nor a repository's index of when
// somebody last spoke; a record somebody wrote rewrites both, one entry each.
//
// Mutation: name updated_at in the home's touch, and every target move dates every live change.
func TestAConversationWriteRewritesTheMovedIndexesOnlyWhenSomebodySpoke(t *testing.T) {
	t.Parallel()
	e := openApplyEstate(t)
	const repo = "repo-a"
	e.apply(create(t, repo, "web"))
	e.seedLiveChanges(repo, 1)
	change := repo + "-change-000000"
	e.exec(`INSERT INTO code_conversations (change_id, repo_id, comments, text_bytes, version, position,
		updated_at) VALUES (?, ?, 1, 10, 1, 1, 5)`, change, repo)
	ctx := t.Context()
	both := []string{conversationsMovedKey, conversationsRepoMovedKey}
	slices.Sort(both)
	if err := e.db.Tx(ctx, func(tx *sql.Tx) error {
		roots := conversationIndexRoots(ctx, t, tx)
		for _, tc := range []struct {
			name, stmt string
			want       []string
		}{
			{"the home's touch", touchConversationSQL, nil},
			{"a record somebody wrote", touchSpokenConversationSQL, both},
		} {
			got := rewrites(t, program(ctx, t, tx, tc.stmt, []any{change, repo, 0, 0, 0, 0, 0, 0, 1, 2, 9}), roots)
			slices.Sort(got)
			if !slices.Equal(got, tc.want) {
				t.Errorf("%s rewrites %q of code_conversations' indexes, want %q", tc.name, got, tc.want)
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("plan the writes: %v", err)
	}
}

// conversationIndexRoots is every index of code_conversations by its root page.
func conversationIndexRoots(ctx context.Context, t *testing.T, tx *sql.Tx) map[int64]string {
	t.Helper()
	rows, err := tx.QueryContext(ctx, `SELECT name, rootpage FROM sqlite_master
		WHERE type = 'index' AND tbl_name = 'code_conversations'`)
	if err != nil {
		t.Fatalf("read code_conversations' indexes: %v", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[int64]string{}
	for rows.Next() {
		var name string
		var root int64
		if err = rows.Scan(&name, &root); err != nil {
			t.Fatalf("read code_conversations' indexes: %v", err)
		}
		out[root] = name
	}
	if err = rows.Err(); err != nil {
		t.Fatalf("read code_conversations' indexes: %v", err)
	}
	if !slices.Contains(slices.Collect(maps.Values(out)), conversationsRepoMovedKey) {
		t.Fatalf("code_conversations has no %s", conversationsRepoMovedKey)
	}
	return out
}
