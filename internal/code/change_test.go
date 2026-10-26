package code_test

import (
	"database/sql"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// A CHANGE'S STATES AND HOW IT MERGED ARE CLOSED SETS, in the order the lists a
// validator and the dashboard's contract read answer them: each value valid, a
// value this build never wrote not, and the live states the first two — the
// ones a push to the source still revises.
func TestAChangesStatesAreAClosedSet(t *testing.T) {
	t.Parallel()
	if want := []code.ChangeState{"draft", "open", "closed", "merged"}; !slices.Equal(code.ChangeStates(), want) {
		t.Errorf("ChangeStates = %v, want %v", code.ChangeStates(), want)
	}
	for _, s := range code.ChangeStates() {
		if !s.Valid() {
			t.Errorf("%s is in the set and Valid says it is not", s)
		}
		if live := s == code.ChangeStateDraft || s == code.ChangeStateOpen; s.Live() != live {
			t.Errorf("%s reports Live=%v, want %v", s, s.Live(), live)
		}
	}
	if want := []code.MergedVia{"push"}; !slices.Equal(code.MergedVias(), want) {
		t.Errorf("MergedVias = %v, want %v", code.MergedVias(), want)
	}
	for _, m := range code.MergedVias() {
		if !m.Valid() {
			t.Errorf("%s is in the set and Valid says it is not", m)
		}
	}
	if code.ChangeState("queued").Valid() || code.ChangeState("").Valid() || code.MergedVia("rebase").Valid() ||
		code.MergedVia("").Valid() {
		t.Error("a value no build wrote reads as valid")
	}
}

// EVERY CHANGE TABLE IS CLASSIFIED REPRODUCIBLE, AND ITS MIGRATION SAYS SO. The
// rows a change is kept in — code_changes, and every code table that names a
// change by a change_id — are rebuilt by a replay from zero and compared by the
// identity claim, never a node's own: two data nodes that disagreed about a
// change's number or its head would answer web!42 differently. Found from the
// schema rather than listed, so a change table a later migration adds is held
// to it too; and the migration that creates each states its class, as every
// replicated migration since the code host's has.
func TestEveryChangeTableIsClassified(t *testing.T) {
	t.Parallel()
	namesAChange := regexp.MustCompile(`[(,]\s*change_id\s`)
	var tables []string
	for name, statement := range ddl(t) {
		table, ok := strings.CutPrefix(name, "table:")
		if ok && (table == "code_changes" || namesAChange.MatchString(statement)) {
			tables = append(tables, table)
		}
	}
	slices.Sort(tables)
	for _, table := range []string{"code_change_authors", "code_changes", "code_revisions"} {
		if !slices.Contains(tables, table) {
			t.Fatalf("the schema's change tables are %v, and %s is not among them — the walk below is "+
				"not looking at the tables a change is kept in", tables, table)
		}
	}
	classes := code.Domain{}.Tables()
	for _, table := range tables {
		if !code.Reproducible(table) || classes[table] != statelog.Replicated {
			t.Errorf("%s is classed %q (reproducible: %v): a change's rows are the log's, rebuilt and "+
				"compared on every node", table, classes[table], code.Reproducible(table))
		}
		if class := classOf(t, table); !strings.Contains(class, table) {
			t.Errorf("the migration that creates %s states no class for it:\n\t%s", table, class)
		}
	}
}

// classOf is the `-- Class:` paragraph of the replicated migration that creates
// table, or "" when that migration has none.
func classOf(t *testing.T, table string) string {
	t.Helper()
	for _, version := range store.SchemaVersions(store.EstateReplicated) {
		body, err := store.SchemaFile(store.EstateReplicated, version)
		if err != nil {
			t.Fatalf("read %s: %v", version, err)
		}
		if !strings.Contains(string(body), "CREATE TABLE "+table+" (") {
			continue
		}
		var paragraph []string
		for _, line := range strings.Split(string(body), "\n") {
			switch {
			case strings.HasPrefix(line, "-- Class:"):
				paragraph = append(paragraph, line)
			case len(paragraph) > 0 && strings.HasPrefix(line, "--"):
				paragraph = append(paragraph, line)
			case len(paragraph) > 0:
				return strings.Join(paragraph, " ")
			}
		}
		return strings.Join(paragraph, " ")
	}
	t.Fatalf("no replicated migration creates %s", table)
	return ""
}

// changeShape is one change a case seeds: how many revisions and authors it
// holds.
type changeShape struct{ revisions, authors int }

// changesOf is count changes of shape.
func changesOf(count int, shape changeShape) []changeShape {
	out := make([]changeShape, count)
	for i := range out {
		out[i] = shape
	}
	return out
}

// seedChanges writes a repository's changes straight into a node's estate,
// numbered from 1 in the order given, each with the revisions and authors its
// shape names — the bulk a purge has to clear, which opening change by change
// would spend minutes on and assert nothing more by. It answers the changes'
// ids.
func (n *node) seedChanges(repoID string, shapes []changeShape) []string {
	n.t.Helper()
	ctx := n.t.Context()
	vars := n.db.Caps().MaxVariables
	ids := make([]string, len(shapes))
	var revisions, authors [][]any
	for i, s := range shapes {
		ids[i] = fmt.Sprintf("%s-change-%06d", repoID, i+1)
		for r := 1; r <= s.revisions; r++ {
			revisions = append(revisions, []any{ids[i], r, oidOf(fmt.Sprint(ids[i], r))})
		}
		for a := range s.authors {
			authors = append(authors, []any{ids[i], fmt.Sprintf("seat-%05d", a)})
		}
	}
	if err := n.db.Tx(ctx, func(tx *sql.Tx) error {
		if _, err := store.InsertRows(ctx, tx, vars,
			`INSERT INTO code_changes (id, repo_id, number, title, state, source_ref, target_ref, head_oid,
				revision, owner, owner_kind, opened_at, opened_position, version, position) VALUES `,
			`(?, ?, ?, 'a change', 'open', ?, 'refs/heads/main', ?, ?, 'seat-00000', 'agent', 0, ?, 1, ?)`, ``,
			len(shapes), func(i int) []any {
				return []any{ids[i], repoID, i + 1, fmt.Sprintf("refs/heads/change-%06d", i+1),
					oidOf(fmt.Sprint(ids[i], shapes[i].revisions)), shapes[i].revisions, i + 1, i + 1}
			}); err != nil {
			return err
		}
		if _, err := store.InsertRows(ctx, tx, vars,
			`INSERT INTO code_revisions (change_id, n, head_oid, pusher, pusher_kind, position, at) VALUES `,
			`(?, ?, ?, 'seat-00000', 'agent', 1, 0)`, ``, len(revisions),
			func(i int) []any { return revisions[i] }); err != nil {
			return err
		}
		_, err := store.InsertRows(ctx, tx, vars,
			`INSERT INTO code_change_authors (change_id, author, author_kind, first_revision, position) VALUES `,
			`(?, ?, 'agent', 1, 1)`, ``, len(authors), func(i int) []any { return authors[i] })
		return err
	}); err != nil {
		n.t.Fatalf("seed %s's changes: %v", repoID, err)
	}
	return ids
}

// changeRows is every row of a repository's changes this node holds — the
// changes by their repository, and their revisions and authors by the ids
// seedChanges gave them, so a row the purge orphaned still counts.
func (n *node) changeRows(repoID string) (changes, revisions, authors int) {
	n.t.Helper()
	prefix := repoID + "-change-%"
	return n.count(`SELECT COUNT(*) FROM code_changes WHERE repo_id = ?`, repoID),
		n.count(`SELECT COUNT(*) FROM code_revisions WHERE change_id LIKE ?`, prefix),
		n.count(`SELECT COUNT(*) FROM code_change_authors WHERE change_id LIKE ?`, prefix)
}

// A PURGE REMOVES A REPOSITORY'S CHANGES, each whole and its row last, counted
// toward its parts: a part takes the changes that fit it whole and cuts only
// the one it stops at — oldest revisions first — never more than PurgePartRows
// rows, and the purge removes the rest, leaving no revision or author of a
// change whose row is gone, and nothing of a neighbour's.
//
// Mutation: leave the revisions, the authors or the changes out of the bounded
// count, and that case's purge is one record deleting more than a part may;
// drop a part's delete of the whole changes' revisions, or the final purge's
// of the revisions or the authors, and rows are left behind; delete the
// changes before their dependents in the final purge, and the revisions and
// authors are orphaned; cut a change newest revision first, and the oldest
// revision outlives the part.
func TestAPurgeRemovesARepositorysChanges(t *testing.T) {
	t.Parallel()
	// TEN CHANGES OF ONE REVISION AND ONE AUTHOR fit the first part whole, 30
	// rows; the change after them needs a part and two rows more, so it is
	// cut, PurgePartRows less 30 of its oldest revisions, and the purge takes
	// its last 30, its author and its row.
	small := changeShape{revisions: 1, authors: 1}
	whole := 3 * 10
	for name, tc := range map[string]struct {
		shapes []changeShape
		steps  []int
		// oldest, when set, is the lowest revision the last change keeps
		// after the first part.
		oldest int
	}{
		"whole changes, then the one a part stops at": {
			shapes: append(changesOf(10, small), changeShape{revisions: code.PurgePartRows, authors: 1}),
			steps:  []int{code.PurgePartRows, 32}, oldest: code.PurgePartRows - whole + 1,
		},
		"changes alone": {
			shapes: changesOf(code.PurgePartRows+1, changeShape{}),
			steps:  []int{code.PurgePartRows, 1},
		},
		"one change's authors": {
			shapes: []changeShape{{revisions: 1, authors: code.PurgePartRows}},
			steps:  []int{code.PurgePartRows, 2},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			n := newNode(t)
			n.ensure("app", described("one"), t1)
			n.ensure("other", described("two"), t1)
			id, other := n.mustID("app"), n.mustID("other")
			n.deleteAt(id, harnessAt.Add(-2*keep))
			ids := n.seedChanges(id, tc.shapes)
			n.seedChanges(other, changesOf(3, small))
			left := func() int {
				changes, revisions, authors := n.changeRows(id)
				return changes + revisions + authors
			}
			var steps []int
			for range len(tc.steps) + 1 {
				before := left()
				res, err := n.store.PurgeStep(t.Context(), id, keep)
				if err != nil {
					t.Fatalf("PurgeStep: %v", err)
				}
				steps = append(steps, before-left())
				if len(steps) == 1 && tc.oldest > 0 {
					last := ids[len(ids)-1]
					if got := n.count(`SELECT MIN(n) FROM code_revisions WHERE change_id = ?`,
						last); got != tc.oldest {
						t.Errorf("after the first part the cut change's oldest revision is %d, want %d: "+
							"a cut takes a change's oldest revisions first", got, tc.oldest)
					}
				}
				if res.Done {
					break
				}
			}
			if !slices.Equal(steps, tc.steps) {
				t.Fatalf("the purge removed %v rows of the changes a record, want %v: parts of at most "+
					"PurgePartRows (%d), then the purge", steps, tc.steps, code.PurgePartRows)
			}
			if changes, revisions, authors := n.changeRows(id); changes+revisions+authors != 0 {
				t.Fatalf("the purge left %d changes, %d revisions and %d authors of the repository",
					changes, revisions, authors)
			}
			if _, found := n.repo("app"); found {
				t.Fatal("the repository still has a row after its purge")
			}
			if changes, revisions, authors := n.changeRows(other); changes != 3 || revisions != 3 ||
				authors != 3 {
				t.Fatalf("the neighbour holds %d changes, %d revisions and %d authors after the purge, "+
					"want the 3 of each it was given", changes, revisions, authors)
			}
		})
	}
}
