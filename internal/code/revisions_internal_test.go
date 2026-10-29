package code

import (
	"context"
	"database/sql"
	"fmt"
	"maps"
	"slices"
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

// bookState is everything a change book holds, by value, whole.
type bookState struct {
	next                      int64
	rows                      map[string]changeRow
	bySource                  map[string]string
	byTarget                  map[string][]string
	authored                  map[[2]string]bool
	opened, isTouched         map[string]bool
	touched                   []string
	bodies                    []bodyRow
	revisions                 []revisionRow
	authors                   []authorRow
	merges                    int
	onTrial                   bool
	moves, opens, rowsCounted int
}

// stateOf is b's state, copied.
func stateOf(b *changeBook) bookState {
	rows := make(map[string]changeRow, len(b.rows))
	for id, row := range b.rows {
		rows[id] = *row
	}
	targets := make(map[string][]string, len(b.byTarget))
	for target, ids := range b.byTarget {
		targets[target] = slices.Clone(ids)
	}
	w := b.writes
	return bookState{next: b.next, rows: rows, bySource: maps.Clone(b.bySource), byTarget: targets,
		authored: maps.Clone(b.authored), opened: maps.Clone(w.opened), isTouched: maps.Clone(w.isTouched),
		touched: slices.Clone(w.touched), bodies: slices.Clone(w.bodies), revisions: slices.Clone(w.revisions),
		authors: slices.Clone(w.authors), merges: w.merges, onTrial: b.trial != nil, moves: b.moved(),
		opens: len(w.opened), rowsCounted: b.rowCount()}
}

// differs names the first part of s that o does not hold alike, "" for none.
func (s bookState) differs(o bookState) string {
	switch {
	case s.next != o.next:
		return "the next number"
	case !maps.Equal(s.rows, o.rows):
		return "the changes' rows"
	case !maps.Equal(s.bySource, o.bySource):
		return "the changes the branches head"
	case !maps.EqualFunc(s.byTarget, o.byTarget, func(a, b []string) bool { return slices.Equal(a, b) }):
		return "the changes into each target"
	case !maps.Equal(s.authored, o.authored):
		return "the authors"
	case !maps.Equal(s.opened, o.opened), s.opens != o.opens:
		return "the changes opened"
	case !maps.Equal(s.isTouched, o.isTouched), !slices.Equal(s.touched, o.touched), s.moves != o.moves:
		return "the changes moved"
	case !slices.Equal(s.bodies, o.bodies):
		return "the descriptions written"
	case !slices.Equal(s.revisions, o.revisions):
		return "the revisions written"
	case !slices.Equal(s.authors, o.authors):
		return "the authors written"
	case s.merges != o.merges:
		return "the merges counted"
	case s.rowsCounted != o.rowsCounted:
		return "the rows counted"
	case s.onTrial != o.onTrial:
		return "whether an entry is on trial"
	}
	return ""
}

// AN ENTRY TAKEN BACK LEAVES THE BOOK AS IT FOUND IT, AND COSTS WHAT IT REACHED:
// the push decide tries each entry on the round's one change book and takes
// back one the record cannot hold, so after taking back an entry that moved
// every way an entry moves a change — a head the record already moved and
// one it had not, a new author, a merge, a close — or that opened a change,
// on a branch nothing named or on the source of a change the record closed,
// into a target the book holds changes into or one it holds none into, the
// book holds exactly what it held before; what the entry would write as the
// first of a record of its own is counted on the book and leaves it as it
// was; and an entry kept leaves the book as moving it untried does. The
// journal holds only the changes the entry reached — four, of a book of a
// hundred and fifty-odd — never the book.
//
// Mutation: forget to restore a moved change's row, an author, a source a
// change was opened on, a target's changes or the record's writes, and the
// book differs; save every change of the book in try, and the journal holds
// more than the entry reached.
func TestAnEntryTakenBackLeavesTheBookAsItFoundIt(t *testing.T) {
	t.Parallel()
	e := openApplyEstate(t)
	const repo = "repo-a"
	e.apply(create(t, repo, "web"))
	const bulk = 150
	e.seedLiveChanges(repo, bulk+5)
	grace := Principal{Kind: PrincipalOperator, OperatorID: "pat:grace", Seat: "grace"}
	by := func(who Principal, id string, cmds ...Command) PushEntry {
		entry := pushEntry(id, cmds...)
		entry.Principal = who
		return entry
	}
	opening := func(id string, c Command, target string) PushEntry {
		entry := by(grace, id, c)
		entry.Change = &ChangeOption{Target: target, Title: "Opened on trial", Body: "about it"}
		return entry
	}
	// THE RECORD SO FAR: a bulk of heads moved, one of them again by a new
	// author, and a change closed.
	var heads []Command
	for c := range bulk {
		heads = append(heads, move(liveSource(c), liveHead(c), oid(fmt.Sprint("bulk", c))))
	}
	kept := []PushEntry{
		pushEntry("bulk", heads...),
		by(grace, "again", move(liveSource(1), oid("bulk1"), oid("again1"))),
		by(grace, "closing", move(liveSource(bulk+4), liveHead(bulk+4), ZeroOID)),
	}
	tried := []PushEntry{
		// EVERY MOVE: a head the record moved and one it did not, each by a
		// pusher no author of it, a merge and a close.
		by(grace, "every", move(liveSource(0), oid("bulk0"), oid("every0")),
			move(liveSource(bulk), liveHead(bulk), oid("every")),
			move("refs/heads/main", ZeroOID, liveHead(bulk+1)),
			move(liveSource(bulk+2), liveHead(bulk+2), ZeroOID)),
		// AN OPEN on a branch nothing named, into the target every change
		// shares; and one on the source of the change the record closed,
		// into a branch no change targets.
		opening("fresh", move("refs/heads/topic", ZeroOID, oid("topic")), "refs/heads/main"),
		opening("reopened", move(liveSource(bulk+4), ZeroOID, oid("back")), "refs/heads/release"),
	}

	ctx := t.Context()
	if err := e.db.Read(ctx, func(tx *sql.Tx) error {
		read := func() *changeBook {
			book, err := readChangeBook(ctx, tx, repo, append(slices.Clone(kept), tried...), 0, 0)
			if err != nil {
				t.Fatalf("read the book: %v", err)
			}
			return book
		}
		book, untried := read(), read()
		for _, entry := range kept {
			book.try()
			book.move(entry)
			book.keep()
			untried.move(entry)
		}
		if diff := stateOf(book).differs(stateOf(untried)); diff != "" {
			t.Fatalf("the entries kept left the book's %s other than moving them untried did", diff)
		}
		before := stateOf(book)
		for _, entry := range tried {
			book.try()
			book.move(entry)
			if entry.Change != nil {
				if _, why := book.open(entry); why != "" {
					t.Fatalf("entry %s opened nothing: %s", entry.PushID, why)
				}
			}
			reached := len(book.trial.moved) + len(book.trial.added)
			book.takeBack()
			if diff := stateOf(book).differs(before); diff != "" {
				t.Errorf("entry %s taken back left the book's %s other than it found them", entry.PushID, diff)
			}
			if entry.PushID == "every" && reached != 4 {
				t.Errorf("the trial of entry %s journaled %d changes, want the 4 it moved of the book's %d",
					entry.PushID, reached, len(book.rows))
			}
		}
		// ALONE: the record's moves landed first, so the head it already
		// moved is a change's row of this record again — eight rows where
		// the record counts the same entry seven — and none of the record's
		// own writes are its.
		rows, moved := book.alone(tried[0])
		if rows != 8 || moved != 4 {
			t.Errorf("entry %s alone writes %d rows of %d changes, want 8 of 4: four changes, two revisions "+
				"and two authors", tried[0].PushID, rows, moved)
		}
		if diff := stateOf(book).differs(before); diff != "" {
			t.Errorf("weighing entry %s alone left the book's %s other than it found them", tried[0].PushID, diff)
		}
		return nil
	}); err != nil {
		t.Fatalf("read: %v", err)
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
