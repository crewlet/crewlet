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
	if want := []code.MergedVia{"push", "direct"}; !slices.Equal(code.MergedVias(), want) {
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

// EVERY CHANGE TABLE IS CLASSIFIED, AND HELD TO THE CLASS ITS MIGRATION
// STATES. The rows a change is kept in — code_changes, and every code table
// that names a change by a change_id — are found from the schema rather than
// listed, so a change table a later migration adds is held here too: the
// domain's table map gives each a class, and one sentence of the `-- Class:`
// paragraph of the migration that last creates it names the table and that
// class, as every replicated migration since the code host's states its
// tables'. Each is held to its own class, never to one class for all: a table
// whose rows each node ages out by a horizon of its own is divergent — carried
// in a snapshot, outside the identity claim — and its migration says so.
//
// THE REPOSITORY'S OWN ARE REPRODUCIBLE. What the records on a repository's
// subject write of a change — its row, its body, its authors and its
// revisions — is rebuilt by a replay from zero and compared by the identity
// claim, never a node's own: two data nodes that disagreed about a change's
// number or its head would answer web!42 differently. A replicated table that
// a change's own subject writes joins the list with the commit that lands it.
//
// Mutation: leave a change table out of the domain's inventory; state another
// class for it in its migration, even in a paragraph whose next sentence
// states this class of another table; or name a repository table the
// inventory does not reproduce — and this fails.
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
	repository := []string{"code_change_authors", "code_change_bodies", "code_changes", "code_revisions"}
	// AND THE CHANGE'S OWN SUBJECT'S, its conversation: rebuilt from the
	// records on the change's subject and compared on every node like the
	// repository's.
	conversation := []string{"code_comments", "code_conversations", "code_participants", "code_reviewers"}
	for _, table := range slices.Concat(repository, conversation) {
		if !slices.Contains(tables, table) {
			t.Fatalf("the schema's change tables are %v, and %s is not among them — the walk below is "+
				"not looking at the tables a change is kept in", tables, table)
		}
	}
	classes := code.Domain{}.Tables()
	for _, table := range tables {
		// THE COMMA-OK READ, because a table the map lacks reads as its zero
		// class, which is statelog.Replicated.
		class, classed := classes[table]
		if !classed {
			t.Errorf("%s names a change and the domain gives it no class: the scrub list, the identity "+
				"claim and the local sweep are all derived from that map", table)
			continue
		}
		if stated := classOf(t, table); !statesClass(stated, table, classWord(class)) {
			t.Errorf("%s is classed %s, and no sentence of the `-- Class:` paragraph of the migration "+
				"that last creates it says it is %s:\n\t%s", table, class, classWord(class), stated)
		}
	}
	for _, table := range slices.Concat(repository, conversation) {
		if class := classes[table]; !code.Reproducible(table) || class != statelog.Replicated {
			t.Errorf("%s is classed %s (reproducible: %v): a change's rows, on its repository's subject and "+
				"its own, are the log's, rebuilt and compared on every node", table, class, code.Reproducible(table))
		}
	}
}

// classWord is how a migration's `-- Class:` paragraph names a class: a
// replicated table as reproducible, the word [code.ReproducibleTables] is
// named by, and any other by the class's own name.
func classWord(class statelog.TableClass) string {
	if class == statelog.Replicated {
		return "reproducible"
	}
	return class.String()
}

// statesClass reports whether one sentence of a `-- Class:` paragraph names
// table and says word — ONE SENTENCE rather than the paragraph, because a
// paragraph that classes several tables states each table's class beside its
// name: 0081's says its three change tables are reproducible and, in the next
// sentence, that code_repos stays so, and a paragraph-wide match would still
// pass with the first sentence calling them anything else.
func statesClass(paragraph, table, word string) bool {
	named := regexp.MustCompile(`\b` + regexp.QuoteMeta(table) + `\b`)
	for _, sentence := range strings.Split(paragraph, ". ") {
		if named.MatchString(sentence) && strings.Contains(strings.ToLower(sentence), word) {
			return true
		}
	}
	return false
}

// classOf is the `-- Class:` paragraph of the replicated migration that LAST
// creates table — the shape the table has, where a later migration created it
// again — each line without its comment marker, or "" when that migration has
// none.
func classOf(t *testing.T, table string) string {
	t.Helper()
	stated, found := "", false
	for _, version := range store.SchemaVersions(store.EstateReplicated) {
		body, err := store.SchemaFile(store.EstateReplicated, version)
		if err != nil {
			t.Fatalf("read %s: %v", version, err)
		}
		if !strings.Contains(string(body), "CREATE TABLE "+table+" (") {
			continue
		}
		found = true
		var paragraph []string
		for _, line := range strings.Split(string(body), "\n") {
			text, comment := strings.CutPrefix(line, "--")
			if !comment && len(paragraph) > 0 {
				break
			}
			if comment && (len(paragraph) > 0 || strings.HasPrefix(text, " Class:")) {
				paragraph = append(paragraph, strings.TrimSpace(text))
			}
		}
		stated = strings.Join(paragraph, " ")
	}
	if !found {
		t.Fatalf("no replicated migration creates %s", table)
	}
	return stated
}

// changeShape is one change a case seeds: how many revisions and authors it
// holds, and what its body weighs — 0 for none, and so no body row.
type changeShape struct{ revisions, authors, body int }

// changesOf is count changes of shape.
func changesOf(count int, shape changeShape) []changeShape {
	out := make([]changeShape, count)
	for i := range out {
		out[i] = shape
	}
	return out
}

// seedChanges writes a repository's changes straight into a node's estate,
// numbered from 1 in the order given, each with the revisions, the authors and
// the body its shape names — the bulk a purge has to clear, which opening
// change by change would spend minutes on and assert nothing more by. It
// answers the changes' ids.
//
// A BODY ROW STANDS FOR THE WIDTH ITS CHANGE STATES, as seedWeighed's push
// rows do: a purge weighs a body by its change's body_bytes and never reads
// the body, so a case weighs mebibytes without writing them — a body row
// carries the width itself only up to a kilobyte.
func (n *node) seedChanges(repoID string, shapes []changeShape) []string {
	n.t.Helper()
	ctx := n.t.Context()
	vars := n.db.Caps().MaxVariables
	ids := make([]string, len(shapes))
	var revisions, authors, bodies [][]any
	for i, s := range shapes {
		ids[i] = fmt.Sprintf("%s-change-%06d", repoID, i+1)
		for r := 1; r <= s.revisions; r++ {
			revisions = append(revisions, []any{ids[i], r, oidOf(fmt.Sprint(ids[i], r))})
		}
		for a := range s.authors {
			authors = append(authors, []any{ids[i], fmt.Sprintf("seat-%05d", a)})
		}
		if s.body > 0 {
			bodies = append(bodies, []any{ids[i], strings.Repeat("b", min(s.body, 1<<10))})
		}
	}
	if err := n.db.Tx(ctx, func(tx *sql.Tx) error {
		if _, err := store.InsertRows(ctx, tx, vars,
			`INSERT INTO code_changes (id, repo_id, number, title, body_bytes, state, source_ref, target_ref,
				head_oid, revision, owner, owner_kind, opened_at, opened_position, version, position) VALUES `,
			`(?, ?, ?, 'a change', ?, 'open', ?, 'refs/heads/main', ?, ?, 'seat-00000', 'agent', 0, ?, 1, ?)`,
			``, len(shapes), func(i int) []any {
				return []any{ids[i], repoID, i + 1, shapes[i].body, fmt.Sprintf("refs/heads/change-%06d", i+1),
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
		if _, err := store.InsertRows(ctx, tx, vars,
			`INSERT INTO code_change_bodies (change_id, body) VALUES `, `(?, ?)`, ``, len(bodies),
			func(i int) []any { return bodies[i] }); err != nil {
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
// changes by their repository, and their revisions, authors and bodies by the
// ids seedChanges gave them, so a row the purge orphaned still counts.
func (n *node) changeRows(repoID string) (changes, revisions, authors, bodies int) {
	n.t.Helper()
	prefix := repoID + "-change-%"
	return n.count(`SELECT COUNT(*) FROM code_changes WHERE repo_id = ?`, repoID),
		n.count(`SELECT COUNT(*) FROM code_revisions WHERE change_id LIKE ?`, prefix),
		n.count(`SELECT COUNT(*) FROM code_change_authors WHERE change_id LIKE ?`, prefix),
		n.count(`SELECT COUNT(*) FROM code_change_bodies WHERE change_id LIKE ?`, prefix)
}

// A PURGE REMOVES A REPOSITORY'S CHANGES, each whole and its row last, counted
// toward its parts: a part takes the changes that fit it whole and cuts only
// the one it stops at — oldest revisions first, its body and row left for
// later — never more than PurgePartRows rows, and the purge removes the rest,
// leaving no revision, author or body of a change whose row is gone, and
// nothing of a neighbour's.
//
// Mutation: leave the revisions, the authors, the bodies or the changes out of
// the bounded count, and that case's purge is one record deleting more than a
// part may; leave the bodies out of the walk's count, and a part of bodied
// changes deletes past PurgePartRows; drop a part's delete of the whole
// changes' revisions or bodies, or the final purge's of the revisions, the
// authors or the bodies, and rows are left behind; delete the changes before
// their dependents in the final purge, and the dependents are orphaned; cut a
// change newest revision first, and the oldest revision outlives the part.
func TestAPurgeRemovesARepositorysChanges(t *testing.T) {
	t.Parallel()
	// TEN CHANGES OF ONE REVISION, ONE AUTHOR AND A BODY fit the first part
	// whole, 40 rows; the change after them needs a part and three rows more,
	// so it is cut, PurgePartRows less 40 of its oldest revisions, and the
	// purge takes its last 40, its author, its body and its row.
	small := changeShape{revisions: 1, authors: 1, body: 64}
	whole := 4 * 10
	for name, tc := range map[string]struct {
		shapes []changeShape
		steps  []int
		// oldest, when set, is the lowest revision the last change keeps
		// after the first part.
		oldest int
	}{
		"whole changes, then the one a part stops at": {
			shapes: append(changesOf(10, small), changeShape{revisions: code.PurgePartRows, authors: 1, body: 64}),
			steps:  []int{code.PurgePartRows, 43}, oldest: code.PurgePartRows - whole + 1,
		},
		"changes alone": {
			shapes: changesOf(code.PurgePartRows+1, changeShape{}),
			steps:  []int{code.PurgePartRows, 1},
		},
		// A BODY IS A ROW: a change of a body alone is two, so a part takes
		// half as many of them as of bare changes.
		"bodies past a part": {
			shapes: changesOf(code.PurgePartRows/2+1, changeShape{body: 64}),
			steps:  []int{code.PurgePartRows, 2},
		},
		"one change's authors": {
			shapes: []changeShape{{revisions: 1, authors: code.PurgePartRows, body: 64}},
			steps:  []int{code.PurgePartRows, 3},
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
				changes, revisions, authors, bodies := n.changeRows(id)
				return changes + revisions + authors + bodies
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
			if changes, revisions, authors, bodies := n.changeRows(id); changes+revisions+authors+bodies != 0 {
				t.Fatalf("the purge left %d changes, %d revisions, %d authors and %d bodies of the repository",
					changes, revisions, authors, bodies)
			}
			if _, found := n.repo("app"); found {
				t.Fatal("the repository still has a row after its purge")
			}
			if changes, revisions, authors, bodies := n.changeRows(other); changes != 3 || revisions != 3 ||
				authors != 3 || bodies != 3 {
				t.Fatalf("the neighbour holds %d changes, %d revisions, %d authors and %d bodies after the "+
					"purge, want the 3 of each it was given", changes, revisions, authors, bodies)
			}
		})
	}
}

// A PURGE IS BOUNDED BY WHAT ITS CHANGES' BODIES WEIGH, in one budget with its
// pushes: a repository whose few changes carry wide bodies is purged in parts,
// each taking whole changes only while its wide rows — the push rows first,
// then the bodies — weigh under PurgePartBytes, and the final purge, whose
// statements are unbounded, is formed only once what is left weighs no more
// than that. A change's body is the one wide value it keeps, and deleting it
// frees overflow pages a count of rows does not see.
//
// Mutation: leave the bodies' weight out of the bounded count, and the purge
// is one record over every body; take a part's changes by their rows alone,
// and its first part deletes every change; weigh them apart from the pushes,
// and the part after the pushes takes four.
func TestAPurgeIsBoundedByWhatItsChangesWeigh(t *testing.T) {
	t.Parallel()
	// TEN CHANGES OF A REVISION, AN AUTHOR AND AN 8 MiB BODY: 80 MiB in forty
	// rows, far under PurgePartRows. A part takes changes until their bodies
	// reach PurgePartBytes — four — and the purge takes what is left once it
	// weighs no more than that.
	const width = 8 << 20
	for name, tc := range map[string]struct {
		// pushes is how many push rows of width bytes the repository holds,
		// which a part takes before any change.
		pushes int
		// steps is how many changes each record removed.
		steps []int
	}{
		"bodies alone": {steps: []int{4, 4, 2}},
		// THREE PUSHES leave a quarter of the first part's bytes: it takes
		// them and the one change whose body reaches the bound.
		"after the pushes": {pushes: 3, steps: []int{1, 4, 4, 1}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			n := newNode(t)
			n.ensure("app", described("one"), t1)
			id := n.mustID("app")
			n.deleteAt(id, harnessAt.Add(-2*keep))
			n.seedChanges(id, changesOf(10, changeShape{revisions: 1, authors: 1, body: width}))
			if tc.pushes > 0 {
				n.exec(`WITH RECURSIVE k(i) AS (SELECT 0 UNION ALL SELECT i + 1 FROM k WHERE i + 1 < ?)
					INSERT INTO code_pushes (push_id, repo_id, position, at, actor, actor_kind, via, commands,
						received_at, row_bytes)
					SELECT printf('wide-%02d', i), ?, i + 1, 0, 'seed', 'operator', 'git', '[]', 0, ? FROM k`,
					tc.pushes, id, width)
			}
			changes := func() int {
				return n.count(`SELECT COUNT(*) FROM code_changes WHERE repo_id = ?`, id)
			}
			var steps []int
			for range len(tc.steps) + 1 {
				before := changes()
				res, err := n.store.PurgeStep(t.Context(), id, keep)
				if err != nil {
					t.Fatalf("PurgeStep: %v", err)
				}
				steps = append(steps, before-changes())
				if len(steps) == 1 {
					if pushes := n.count(`SELECT COUNT(*) FROM code_pushes WHERE repo_id = ?`, id); pushes != 0 {
						t.Errorf("the first part left %d of the %d pushes, which fit its bytes and go "+
							"before any change", pushes, tc.pushes)
					}
				}
				if res.Done {
					break
				}
			}
			if !slices.Equal(steps, tc.steps) {
				t.Fatalf("the purge removed %v changes a record, want %v: whole changes until their "+
					"bodies, after the pushes, reach PurgePartBytes (%d), then the purge once what is "+
					"left fits", steps, tc.steps, code.PurgePartBytes)
			}
			if changes, revisions, authors, bodies := n.changeRows(id); changes+revisions+authors+bodies != 0 {
				t.Fatalf("the purge left %d changes, %d revisions, %d authors and %d bodies",
					changes, revisions, authors, bodies)
			}
		})
	}
}
