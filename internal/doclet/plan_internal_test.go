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
			plan := storetest.PlanTree(t, db, statement, args)
			details := storetest.Details(plan)
			shown := strings.Join(details, "\n")
			kinds := len(slices.Compact(slices.Sorted(slices.Values(c.q.ActorKinds))))
			sought := 0
			for _, line := range plan {
				switch {
				case strings.HasPrefix(line.Detail, "SCAN pages_history") &&
					!strings.Contains(line.Detail, "INDEX"):
					t.Errorf("this read reads every change from the heap:\n%s", shown)
				case strings.HasPrefix(line.Detail, "SEARCH h USING") &&
					strings.Contains(line.Detail, "INDEX "+writers+" "):
					sought++
				case strings.Contains(line.Detail, "FOR ORDER BY") && c.unsorted:
					t.Errorf("this read sorts what it read, where an index is "+
						"in the feed's order:\n%s", shown)
				case strings.Contains(line.Detail, "FOR ORDER BY") && line.Parent != 0:
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
		if shape.drivenBy != "" {
			// NOT IN THE ORDER'S INDEX: a listing narrowed by a filter's
			// own rows reads those rows and sorts them, which
			// [TestAFilterIsReadFromItsOwnRows] holds instead.
			continue
		}
		t.Run(name, func(t *testing.T) {
			reads := map[string]listSpec{"the first window": {}}
			for i, arm := range listResumes(cursor, shape.pinned) {
				reads[fmt.Sprintf("arm %d of a later window", i)] = arm
			}
			for read, from := range reads {
				statement, args := listRowsStatement(shape.spec, from, DefaultLimit+1)
				plan := storetest.PlanTree(t, db, statement, args)
				details := storetest.Details(plan)
				shown := strings.Join(details, "\n")
				for _, line := range plan {
					switch {
					case strings.HasPrefix(line.Detail, "SCAN pages_heads AS p") &&
						!strings.Contains(line.Detail, "INDEX"):
						t.Errorf("%s reads every page from the heap:\n%s", read, shown)
					case strings.Contains(line.Detail, "FOR ORDER BY") && line.Parent == 0:
						t.Errorf("%s sorts what it read, where an index is in the "+
							"listing's order:\n%s", read, shown)
					case strings.Contains(line.Detail, "FOR ORDER BY"):
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
				plan := storetest.PlanTree(t, db, statement.sql, statement.args)
				details := storetest.Details(plan)
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
// predicate its rows' children are counted under, whether it is pinned to one
// container ([listResumes]), and the index of the filter whose own rows drive
// it, where one does.
type listingShape struct {
	spec, child listSpec
	pinned      bool
	drivenBy    string
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
		"a label's pages":               {Label: "runbook"},
		"a label's pages in a container": {Container: "ENG", Label: "runbook",
			Status: live},
		"one person's watched pages": {Watcher: "jane", Status: live},
	} {
		spec, child := compileListing(f)
		shape := listingShape{spec: spec, child: child, pinned: f.Container != ""}
		switch {
		case f.Label != "":
			shape.drivenBy = "pages_labels_label_idx"
		case f.Watcher != "":
			shape.drivenBy = "pages_watchers_handle_idx"
		}
		shapes[name] = shape
	}
	shapes["a page's own children"] = listingShape{spec: childListing("p-0001")}
	return shapes
}

// A FILTER BY A PAGE'S OWN ROWS IS READ FROM THOSE ROWS.
//
// A label and a watcher narrow a listing to the pages one label or one person
// names, and each has an index for exactly that: `pages_labels_label_idx` and
// `pages_watchers_handle_idx`. Written as a probe of each listed page's own
// rows, the filter walked the listing's order asking every page in it, and the
// listing's total counted the same way — one person's watched pages read, and
// counted, every page in the company to find the few they follow. So each such
// listing's total, first window and every arm of a later one are held to: a
// seek of the filter's own index; `pages_heads` read by nothing but its key;
// and no walk of the heads, the labels or the watchers. Each window sorts the
// filter's matches, which is what it costs instead of the knowledge base.
//
// Mutation: probe the listed pages' own rows again, and every read walks the
// heads; drop the filter's index, and the filter's rows are read whole.
func TestAFilterIsReadFromItsOwnRows(t *testing.T) {
	t.Parallel()
	db := pagePlanStore(t)
	cursor := listCursor{Container: "ENG", Title: "Page 5", ID: "p-0005"}
	held := 0
	for name, shape := range listingShapes() {
		if shape.drivenBy == "" {
			continue
		}
		held++
		t.Run(name, func(t *testing.T) {
			total, totalArgs := listTotalStatement(shape.spec)
			reads := map[string]struct {
				sql  string
				args []any
			}{"the total": {total, totalArgs}}
			for i, from := range append(slices.Clone(listFromStart),
				listResumes(cursor, shape.pinned)...) {
				statement, args := listRowsStatement(shape.spec, from, DefaultLimit+1)
				reads[fmt.Sprintf("window read %d", i)] = struct {
					sql  string
					args []any
				}{statement, args}
			}
			for read, statement := range reads {
				details := storetest.Details(storetest.PlanTree(t, db, statement.sql,
					statement.args))
				shown := strings.Join(details, "\n")
				if !storetest.Seeks(details, shape.drivenBy) {
					t.Errorf("%s does not seek %s, the filter's own index:\n%s",
						read, shape.drivenBy, shown)
				}
				for _, line := range details {
					switch {
					case strings.HasPrefix(line, "SCAN pages_heads"),
						strings.HasPrefix(line, "SCAN pages_labels"),
						strings.HasPrefix(line, "SCAN pages_watchers"):
						t.Errorf("%s walks a table the filter names its rows of:\n%s",
							read, shown)
					case strings.HasPrefix(line, "SEARCH p ") &&
						!strings.Contains(line, "sqlite_autoindex_pages_heads_1 (id=?)"):
						t.Errorf("%s reads the heads other than by the key of a page "+
							"the filter named:\n%s", read, shown)
					}
				}
			}
		})
	}
	if held == 0 {
		t.Fatal("no listing shape is narrowed by a filter's own rows, so this " +
			"test holds nothing")
	}
}

// A PAGE'S REVISIONS ARE LISTED IN THEIR KEY'S ORDER.
//
// A page's detail lists its newest revisions, and `pages_revisions`' own key
// (page_id, edit_version) holds them in exactly that order, newest last. So the
// list is one walk of the key backwards from the page's newest revision that
// its LIMIT stops; ordered by the log position instead, which no index holds,
// it sought the page's revisions and sorted them all first.
//
// Mutation: order the list by the position column again, and it sorts.
func TestAPagesRevisionsAreListedInTheirKeysOrder(t *testing.T) {
	t.Parallel()
	db := pagePlanStore(t)
	plan := storetest.Plan(t, db, revisionListStatement, []any{"p-0001", RevisionsKept})
	shown := strings.Join(plan, "\n")
	if !storetest.Seeks(plan, "sqlite_autoindex_pages_revisions_1") {
		t.Errorf("the revision list does not seek the revisions' key:\n%s", shown)
	}
	if storetest.Sorts(plan) {
		t.Errorf("the revision list sorts a page's revisions, where their key "+
			"holds them in order:\n%s", shown)
	}
}

// THE TRASH IS LISTED IN ITS OWN ORDER.
//
// The trash reaper lists the pages trashed before the term's cutoff oldest
// first and then by id, a pass's cap at a time ([Store.TrashedBefore]). The
// partial index it reads is on (trashed_at, id) since replicated 0066, which is
// that order, so the listing is one seek of it up to the cutoff that its LIMIT
// stops. On `trashed_at` alone, as 0005 declared it, the id ties sorted: the
// seek read every page in the trash past the cutoff and sorted them all before
// the first was returned, on exactly the pass that runs when a trash has piled
// up.
//
// Mutation: rebuild the index on `trashed_at` alone, and the listing sorts.
func TestTheTrashIsListedInItsOrder(t *testing.T) {
	t.Parallel()
	db := pagePlanStore(t)
	plan := storetest.Plan(t, db, trashedBeforeStatement,
		[]any{int64(1), string(StatusTrashed), 100})
	shown := strings.Join(plan, "\n")
	if !storetest.Seeks(plan, "pages_heads_trashed_idx") {
		t.Errorf("the trash listing does not seek the trash's index:\n%s", shown)
	}
	if storetest.Sorts(plan) {
		t.Errorf("the trash listing sorts the trash it read, where its index "+
			"holds it in order:\n%s", shown)
	}
}
