package doclet

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
)

// THE CHANGE FEED IS READ IN ITS OWN ORDER, AND THE AUDIT ONE PAGE PER KIND.
//
// `pages_history` is one row per change since the domain landed and nothing
// sweeps it, so a feed read with no index for its order reads every change the
// company has made, sorted, on every load — which is what the audit did: its
// `actor_kind IN (…)` sought each kind of `(actor_kind, version DESC)` and then
// sorted everything the seeks found, while the comment above it said the order
// came out of the index. Each author kind is now its own arm
// ([pageActivityStatement]): one kind seeks the index in the feed's order and
// needs no sort, and several are merged at the top of the statement over at
// most one page per kind.
//
// So the statement the reader issues ([pageActivityStatement] over
// [compilePageActivity]) is planned for every shape the feed is asked in, and
// held to: no heap scan of the history; the audit to one seek of the author
// index per DISTINCT kind and to no sort below the top; one kind, the
// unfiltered digest and one page's own feed to no sort at all — the last
// because `pages_history_page_idx` is (page_id, version DESC) since replicated
// 0063, where it was in the broker instant's order and one page's feed sorted
// its whole history.
//
// PLANNED AS A DEPLOYMENT PLANS IT — with no statistics, since nothing in the
// engine runs ANALYZE — over a history with the shape of a company's
// ([seedPageHistory]).
func TestTheChangeFeedIsReadInItsOwnOrder(t *testing.T) {
	t.Parallel()
	db := pagePlanStore(t)
	const writers = "pages_history_actor_kind_version_idx"
	people := []AuthorKind{AuthorOperator, AuthorHuman}
	for name, c := range map[string]struct {
		q        PageActivityQuery
		unsorted bool
	}{
		"the company's digest":        {PageActivityQuery{}, true},
		"a later page of the digest":  {PageActivityQuery{Cursor: 9_000}, true},
		"the audit screen":            {PageActivityQuery{ActorKinds: people}, false},
		"a later page of the audit":   {PageActivityQuery{ActorKinds: people, Cursor: 9_000}, false},
		"every author kind":           {PageActivityQuery{ActorKinds: AuthorKinds()}, false},
		"one author kind":             {PageActivityQuery{ActorKinds: []AuthorKind{AuthorAgent}}, true},
		"one kind named twice":        {PageActivityQuery{ActorKinds: []AuthorKind{AuthorHuman, AuthorHuman}}, true},
		"one writer's changes":        {PageActivityQuery{Actor: "h-7"}, true},
		"the audit of one space":      {PageActivityQuery{Container: "ENG", ActorKinds: people}, false},
		"one kind of change":          {PageActivityQuery{Kinds: []ChangeKind{ChangeSaved}}, true},
		"the digest since a position": {PageActivityQuery{Since: 9_000}, true},
		"one page's changes":          {PageActivityQuery{Page: "p-0001"}, true},
		"a later page of one page's":  {PageActivityQuery{Page: "p-0001", Cursor: 9_000}, true},
	} {
		t.Run(name, func(t *testing.T) {
			statement, args := pageActivityStatement(compilePageActivity(c.q),
				MaxPageChanges+1)
			plan := explainPlan(t, db, statement, args)
			details := make([]string, len(plan))
			for i, line := range plan {
				details[i] = line.detail
			}
			shown := strings.Join(details, "\n")
			kinds := len(slices.Compact(slices.Sorted(slices.Values(c.q.ActorKinds))))
			sought := 0
			for _, line := range plan {
				switch {
				case strings.HasPrefix(line.detail, "SCAN pages_history") &&
					!strings.Contains(line.detail, "INDEX"):
					t.Errorf("this read reads every change from the heap:\n%s", shown)
				case strings.HasPrefix(line.detail, "SEARCH h USING") &&
					strings.Contains(line.detail, "INDEX "+writers+" "):
					sought++
				case strings.Contains(line.detail, "FOR ORDER BY") && c.unsorted:
					t.Errorf("this read sorts what it read, where an index is "+
						"in the feed's order:\n%s", shown)
				case strings.Contains(line.detail, "FOR ORDER BY") && line.parent != 0:
					t.Errorf("an arm sorts what it read rather than reading one "+
						"kind's range in its order:\n%s", shown)
				}
			}
			if kinds > 0 && sought != kinds {
				t.Errorf("the audit of %d author kind(s) seeks %s %d times, want "+
					"once per kind:\n%s", kinds, writers, sought, shown)
			}
			// AND EVERY ARM STOPS AT A PAGE, which no plan says: this
			// engine's plan names no LIMIT, so an arm that lost its own is
			// the same seek in the same order, read to the end of its kind
			// before the merge keeps a page — every change of those kinds
			// again, and every check above still passes. Measured. So the
			// statement carries a LIMIT for each arm beside the merge's.
			if limits := strings.Count(statement, "LIMIT ?"); kinds > 1 && limits != kinds+1 {
				t.Errorf("the audit of %d author kinds carries %d LIMITs, want one "+
					"per arm and the merge's:\n%s", kinds, limits, statement)
			}
		})
	}
}

// planLine is one line of a plan and the line it sits under — 0 for the
// statement's own top level — which is what tells a sort of a whole statement
// from a sort inside one of its subqueries.
type planLine struct {
	parent int
	detail string
}

// explainPlan is the planner's account of one statement.
func explainPlan(t *testing.T, db store.ReplicatedHandle, statement string, args []any) []planLine {
	t.Helper()
	var plan []planLine
	if err := db.Read(t.Context(), func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+statement, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id, notUsed int
			var line planLine
			if err := rows.Scan(&id, &line.parent, &notUsed, &line.detail); err != nil {
				return err
			}
			plan = append(plan, line)
		}
		return rows.Err()
	}); err != nil {
		t.Fatalf("EXPLAIN QUERY PLAN:\n%s\n%v", statement, err)
	}
	if len(plan) == 0 {
		t.Fatalf("the planner said nothing about:\n%s", statement)
	}
	return plan
}

// The history the feed is planned against: changes across pages in a few
// spaces, by every kind of writer.
//
// It decides no plan today — with no statistics this engine's planner plans
// from its built-in guesses whatever the tables hold — and it is a company's
// shape anyway, for the reason the tracker's plan corpus gives: the day an
// engine release starts weighing a table's contents, the dependency update that
// brings it plans the feed over what a deployment holds rather than an empty
// file.
const (
	pageHistoryRows = 4_000
	pagePlanPages   = 200
	pagePlanSpaces  = 4
)

// pagePlanStore is a replicated estate holding [seedPageHistory].
func pagePlanStore(t *testing.T) store.ReplicatedHandle {
	t.Helper()
	dbNode, db := storetest.OpenEstate(t, filepath.Join(t.TempDir(), "node.db"), store.Options{}, 1)
	t.Cleanup(func() {
		if err := dbNode.Close(); err != nil {
			t.Errorf("close the store: %v", err)
		}
	})
	w, err := db.Writer(t.Context())
	if err != nil {
		t.Fatalf("take the writer: %v", err)
	}
	defer w.Close()
	if err := w.Tx(t.Context(), func(tx *sql.Tx) error {
		return seedPageHistory(t.Context(), tx, db.Caps().MaxVariables)
	}); err != nil {
		t.Fatalf("seed the fixture: %v", err)
	}
	return db
}

// seedPageHistory writes the heads and the history the feed is planned over.
func seedPageHistory(ctx context.Context, tx *sql.Tx, maxVariables int) error {
	spaces := []string{"ENG", "OPS", "DOCS", "HR"}
	if _, err := store.InsertRows(ctx, tx, maxVariables, `
		INSERT INTO pages_heads (id, container, title, title_norm, status,
			edit_version, created_at, updated_at, version, document) VALUES`,
		`(?,?,?,?,?,?,?,?,?,?)`, "", pagePlanPages, func(i int) []any {
			title := fmt.Sprintf("Page %d", i)
			return []any{fmt.Sprintf("p-%04d", i), spaces[i%pagePlanSpaces], title,
				strings.ToLower(title), "current", 1, int64(i), int64(i), int64(i),
				[]byte(`{}`)}
		}); err != nil {
		return fmt.Errorf("the heads: %w", err)
	}
	writers := AuthorKinds()
	_, err := store.InsertRows(ctx, tx, maxVariables, `
		INSERT INTO pages_history (id, page_id, kind, actor, actor_kind,
			created_at, version, document) VALUES`,
		`(?,?,?,?,?,?,?,?)`, "", pageHistoryRows, func(i int) []any {
			return []any{fmt.Sprintf("h-%05d", i), fmt.Sprintf("p-%04d", i%pagePlanPages),
				string(ChangeSaved), fmt.Sprintf("h-%d", i%50),
				string(writers[i%len(writers)]), int64(i), int64(i + 1), []byte(`{}`)}
		})
	if err != nil {
		return fmt.Errorf("the history: %w", err)
	}
	return nil
}

// A LISTING IS READ IN ITS ORDER, AND A LATER WINDOW SEEKS IT.
//
// Every listing is ordered by container, title and id ([listOrder]), and a
// window is meant to be a walk of an index in that order which its LIMIT
// stops: `pages_heads_container_idx` for the workspace and a container,
// `pages_heads_parent_idx` for a tree level. Without one, every window read
// every page the listing matched and sorted them all to keep fifty; and a
// later window resumed through one nested disjunction no index could seek, so
// it did the same from the top. So each window a listing is asked in is
// planned — the first, and every arm of a later one ([listResumes]) — over the
// shapes the surfaces ask for, and held to: no read of `pages_heads` from the
// heap, no sort of the window, and each arm of a resume one seek of the
// order's index bounded where the cursor stopped. And no sort INSIDE it
// either: each row's last save ([summarySaved]) is the newest of its page's
// saves, which `pages_history_page_idx` holds in log order, where ordered by
// anything else it sorted that page's whole history once per row.
//
// Mutation: resume through the nested disjunction again, or order a listing by
// a column its indexes do not lead with, and the plans sort; resume a listing
// pinned to one container on its container as well, and the last arm reads
// the whole container to find nothing; order a page's saves by their broker
// instant, and every row sorts its history.
func TestAListingIsReadInItsOrder(t *testing.T) {
	t.Parallel()
	db := pagePlanStore(t)
	cursor := listCursor{Container: "ENG", Title: "Page 5", ID: "p-0005"}
	shapes := listingShapes()
	for name, shape := range shapes {
		t.Run(name, func(t *testing.T) {
			reads := map[string]listSpec{"the first window": {}}
			for i, arm := range listResumes(cursor, shape.pinned) {
				reads[fmt.Sprintf("arm %d of a later window", i)] = arm
			}
			for read, from := range reads {
				statement, args := listRowsStatement(shape.spec, from, DefaultLimit+1)
				plan := explainPlan(t, db, statement, args)
				details := make([]string, len(plan))
				for i, line := range plan {
					details[i] = line.detail
				}
				shown := strings.Join(details, "\n")
				for _, line := range plan {
					switch {
					case strings.HasPrefix(line.detail, "SCAN pages_heads AS p") &&
						!strings.Contains(line.detail, "INDEX"):
						t.Errorf("%s reads every page from the heap:\n%s", read, shown)
					case strings.Contains(line.detail, "FOR ORDER BY") && line.parent == 0:
						t.Errorf("%s sorts what it read, where an index is in the "+
							"listing's order:\n%s", read, shown)
					case strings.Contains(line.detail, "FOR ORDER BY"):
						t.Errorf("%s sorts a listed page's history for its last "+
							"save, where pages_history_page_idx holds it in log "+
							"order:\n%s", read, shown)
					}
				}
				if len(from.where) == 0 {
					continue
				}
				// THE ARM'S OWN BOUND, in the plan's spelling: the last
				// conjunct of the arm, `p.title > ?` read as `title>?`.
				conjuncts := strings.Split(strings.Trim(from.where[0], "()"), " AND ")
				bound := strings.ReplaceAll(strings.TrimPrefix(
					conjuncts[len(conjuncts)-1], "p."), " ", "")
				sought := false
				for _, line := range details {
					sought = sought || (strings.HasPrefix(line, "SEARCH p USING ") &&
						(strings.Contains(line, "INDEX pages_heads_container_idx (") ||
							strings.Contains(line, "INDEX pages_heads_parent_idx (")) &&
						strings.Contains(line, bound+")"))
				}
				if !sought {
					t.Errorf("%s (%s) is no seek of the listing's index bounded "+
						"%s, so it reads from the top of the listing to find where "+
						"the last window stopped:\n%s", read, from.where[0], bound, shown)
				}
			}
		})
	}
}

// A LISTING COUNTS ITS ROWS' CHILDREN BY SEEKING THEIR PARENTS.
//
// Each row of a listing says how many children it has under the listing's own
// narrowing, which a tree draws its expander off. Written as a correlated
// subquery in the listing's statement, the count was one the planner unnests by
// cost into a grouped read of every page head in the company — measured, it
// did on the workspace's listing, on every window of it. So the count is read
// for the window's rows in a statement of its own ([childCountStatement]),
// whose IN list of bound ids leaves nothing to unnest. Each shape a listing is
// asked in is held to: no decorrelated subquery in its total, its window or its
// child count ([storetest.Decorrelated]); no read of `pages_heads` from the
// heap in any of them; and the child count one seek of
// `pages_heads_parent_idx` by the parent.
//
// Mutation: put the correlated count back into the window's statement, and the
// workspace's window is decorrelated; count the children by the child's own id
// rather than its parent's, and the count seeks nothing.
func TestAListingCountsChildrenBySeekingTheirParents(t *testing.T) {
	t.Parallel()
	db := pagePlanStore(t)
	window := []string{"p-0001", "p-0002", "p-0003"}
	for name, shape := range listingShapes() {
		t.Run(name, func(t *testing.T) {
			total, totalArgs := listTotalStatement(shape.spec)
			rows, rowsArgs := listRowsStatement(shape.spec, listSpec{}, DefaultLimit+1)
			children, childArgs := childCountStatement(window, shape.child)
			for read, statement := range map[string]struct {
				sql  string
				args []any
			}{
				"the total":       {total, totalArgs},
				"the window":      {rows, rowsArgs},
				"the child count": {children, childArgs},
			} {
				plan := explainPlan(t, db, statement.sql, statement.args)
				details := make([]string, len(plan))
				for i, line := range plan {
					details[i] = line.detail
				}
				shown := strings.Join(details, "\n")
				if line, unnested := storetest.Decorrelated(details); unnested {
					t.Errorf("%s answers a per-row aggregate by reading its whole "+
						"child table (%q):\n%s", read, line, shown)
				}
				for _, line := range details {
					if strings.HasPrefix(line, "SCAN pages_heads") &&
						!strings.Contains(line, "INDEX") {
						t.Errorf("%s reads every page from the heap:\n%s", read, shown)
					}
				}
				if read != "the child count" {
					continue
				}
				sought := slices.ContainsFunc(details, func(line string) bool {
					return strings.HasPrefix(line, "SEARCH c USING ") &&
						strings.Contains(line, "INDEX pages_heads_parent_idx (parent_id=?)")
				})
				if !sought {
					t.Errorf("the child count is no seek of pages_heads_parent_idx by "+
						"the parent, so it reads pages it does not count:\n%s", shown)
				}
			}
		})
	}
}

// listingShape is one listing as the surfaces ask for it: its predicate, the
// predicate its rows' children are counted under, and whether it is pinned to
// one container ([listResumes]).
type listingShape struct {
	spec, child listSpec
	pinned      bool
}

// listingShapes is every shape of listing the plan tests hold, by name: the
// workspace, a container, a tree level, a narrowing by status, by skill and by
// title, and a page's own children.
func listingShapes() map[string]listingShape {
	skills, ordinary := true, false
	live := []Status{StatusPublished, StatusDraft}
	shapes := map[string]listingShape{}
	for name, f := range map[string]Filter{
		"the workspace":                 {},
		"the published pages":           {Status: []Status{StatusPublished}},
		"one container":                 {Container: "ENG"},
		"one container's live pages":    {Container: "ENG", Status: live},
		"the top of a container's tree": {Container: "ENG", Roots: true, Status: live, Skills: &ordinary},
		"a tree level under one page":   {ParentID: "p-0001", Status: live, Skills: &ordinary},
		"the tool skills":               {Skills: &skills, Status: []Status{StatusPublished}},
		"a title in one container":      {Container: "ENG", Title: "page"},
	} {
		spec, child := compileListing(f)
		shapes[name] = listingShape{spec, child, f.Container != ""}
	}
	shapes["a page's own children"] = listingShape{spec: childListing("p-0001")}
	return shapes
}
