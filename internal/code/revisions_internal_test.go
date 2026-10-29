package code

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/store"
)

// THE QUALIFIED CHANGE COLUMNS ARE THE CHANGE COLUMNS: a push's reads of the
// live changes it moves name every column of a change's row qualified — the
// table function they are driven from has an id of its own — and scanChange
// reads each answer in changeColumns' order, so the two lists name the same
// columns in the same order, or a push's book would read one column into
// another's field on every data node alike, and no other test would see a
// change whose title is its state.
//
// Mutation: swap two columns of qualifiedChangeColumns, or drop one, and this
// fails naming where.
func TestTheQualifiedChangeColumnsAreTheChangeColumns(t *testing.T) {
	t.Parallel()
	columns := func(list string) []string {
		var out []string
		for _, column := range strings.Split(list, ",") {
			out = append(out, strings.TrimSpace(column))
		}
		return out
	}
	plain, qualified := columns(changeColumns), columns(qualifiedChangeColumns)
	if len(plain) != len(qualified) {
		t.Fatalf("changeColumns names %d columns and qualifiedChangeColumns %d", len(plain), len(qualified))
	}
	for i, column := range plain {
		if qualified[i] != "c."+column {
			t.Errorf("column %d is %s in changeColumns and %s in qualifiedChangeColumns, want c.%s", i+1,
				column, qualified[i], column)
		}
	}
}

// execCounter counts the statements a write issues through it.
type execCounter struct {
	tx *sql.Tx
	n  int
}

func (s *execCounter) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	s.n++
	return s.tx.ExecContext(ctx, query, args...)
}

// A PUSH RECORD WRITES THE CHANGES IT MOVES IN BATCHES: its apply holds the
// replicated estate's one write lane on every data node, and one record moves
// up to some 1 300 changes, so what it writes of them — each moved change's
// row, at its next head, merged or closed, and each revision — goes in as few
// statements as the engine's parameters allow ([store.Chunks]), as its refs
// do, never a statement a change: a record moving 999 changes issues the
// statements one moving three does. And each change's row says what its move
// did, its head and revision, a merge's state and a close's — so a batch that
// sets less than a push moves fails here too.
//
// Mutation: write each moved change with its own UPDATE (writeChange), and the
// record of 999 issues a statement a change; leave state out of
// changesMovedSQL, and the merged and closed changes stay open.
func TestAPushRecordWritesTheChangesItMovesInBatches(t *testing.T) {
	t.Parallel()
	for _, n := range []int{3, 999} {
		t.Run(fmt.Sprint(n, " changes"), func(t *testing.T) {
			t.Parallel()
			e := openApplyEstate(t)
			const repo = "repo-a"
			e.apply(create(t, repo, "web"))
			e.seedLiveChanges(repo, n)
			// EVERY MOVE A PUSH MAKES: the heads of all but two, the source of
			// one deleted, and main — the target of every one — set to the
			// last one's head.
			var cmds []Command
			for c := range n - 2 {
				cmds = append(cmds, move(liveSource(c), liveHead(c), oid(fmt.Sprint("next", c))))
			}
			cmds = append(cmds, move(liveSource(n-2), liveHead(n-2), ZeroOID),
				move("refs/heads/main", ZeroOID, liveHead(n-1)))
			entries := []PushEntry{pushEntry("moving", cmds...)}

			ctx := t.Context()
			vars := e.db.Caps().MaxVariables
			var issued, written, counted int
			if err := e.db.Tx(ctx, func(tx *sql.Tx) error {
				book, err := readChangeBook(ctx, tx, repo, entries, 7, store.EncodeTime(applyClock))
				if err != nil {
					return err
				}
				for _, entry := range entries {
					book.move(entry)
				}
				through := &execCounter{tx: tx}
				written, err = book.write(ctx, through, vars)
				issued, counted = through.n, book.rowCount()
				return err
			}); err != nil {
				t.Fatalf("write the record's changes: %v", err)
			}
			chunks := func(rows int, columns string) int {
				n := 0
				for range store.Chunks(rows, vars, len(strings.Split(columns, ","))) {
					n++
				}
				return n
			}
			// THE MOVED CHANGES' ROWS AND THE REVISIONS: the pusher is an
			// author of each already, and the record opens none.
			want := chunks(n, changeColumns) +
				chunks(n-2, "change_id, n, head_oid, pusher, pusher_kind, push_id, position, at")
			if issued != want {
				t.Errorf("the record's changes took %d statements, want %d: a statement a chunk of each table",
					issued, want)
			}
			if written != counted || written != n+n-2 {
				t.Errorf("the write answered %d rows, its book counted %d, want %d: every change and every "+
					"revision", written, counted, n+n-2)
			}
			for state, count := range map[string]int{"open": n - 2, "closed": 1, "merged": 1} {
				if got := e.num(`SELECT COUNT(*) FROM code_changes WHERE repo_id = ? AND state = ?`, repo,
					state); got != int64(count) {
					t.Errorf("%d changes are %s, want %d", got, state, count)
				}
			}
			if got := e.num(`SELECT COUNT(*) FROM code_changes AS c JOIN code_revisions AS r
				ON r.change_id = c.id AND r.n = 2
				WHERE c.repo_id = ? AND c.head_oid = r.head_oid AND c.revision = 2 AND c.version = 2
					AND c.position = 7`, repo); got != int64(n-2) {
				t.Errorf("%d changes are at the head of their second revision, at the record's version and "+
					"position, want %d", got, n-2)
			}
		})
	}
}
