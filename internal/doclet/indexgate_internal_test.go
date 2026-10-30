package doclet

import (
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/search"
	"github.com/crewlet/crewlet/internal/store/storetest"
)

// EVERY INDEX ON A PAGE TABLE SERVES A REGISTERED QUERY, AND EVERY STATEMENT
// THAT READS ONE IS REGISTERED.
//
// # Why this is asserted rather than reviewed
//
// An index is a write on every apply of every page record on every data node,
// for ever, and a statement with no index behind it reads every page, revision
// or change the company has ever made and shows up as a slow screen rather than
// a failure. The schema's trailing comments name a reader for every index, and
// a comment is a CLAIM: three indexes were maintained for no reader until
// replicated 0064 dropped them — one for a digest ordered by an instant nothing
// orders by, a second copy of the revisions' own key, and one for the skills
// registry's re-read, which is driven from the heads. And the embedding duty's
// selection had no index in its order at all until replicated 0065, nor the
// trash reaper's listing until replicated 0066. The planner is the only thing
// that knows whether a statement reaches an index, so this runs EXPLAIN QUERY
// PLAN over every statement that reads a page table and reads the answer out —
// planned as a deployment plans it, with no statistics, over [pagePlanStore]'s
// company.
//
// # What it holds
//
//   - every statement in the module's source that reads one of the covered
//     tables is registered here, matched whole ([storetest.StatementLiterals]):
//     this package's reader, applier and writer, and search's statements over
//     the pages (its lexical walk, its fetches and the embedding duty's page
//     corpus). A reader nobody registered is one whose plan nobody checked;
//   - a registered READ — a point read, a probe, a batch by key, the reads an
//     apply makes — seeks the index it names, or walks it in its own order
//     with no sort when the read is about that order ([pageReads]);
//   - a CENSUS — a listing's total and its window in every shape the surfaces
//     ask for, the change feed in every shape, the rail's per-container counts
//     — reads every row its question names by definition, and is held to no
//     heap scan ([pageCensuses]);
//   - a PASS reads the whole live corpus by definition, so it may read the
//     heads' rows, and is held to no sort ([pagePasses]);
//   - no read and no census reads a covered table from the heap, and none
//     answers a per-row aggregate from its whole child table
//     ([storetest.Decorrelated]);
//   - and the inverse: every index the schema declares on a covered table is
//     in some registered plan.
//
// The listing's own order, its resumed windows, its children's counts, its
// filters and the feed's arms are held in their own tests, which say what each
// costs when it goes wrong; this test is what makes the set they are drawn
// from complete.
//
// # The tables it speaks for
//
// A page's own rows — its head, its address, its revisions, its comments, its
// labels, its watchers, its derived marks — and the history. The containers and
// the deletion markers are not here: each is read by its primary key alone and
// declares no other index, so there is no inventory to keep and no second
// index for a read to lose its way to. The log's own tables are the state-log
// framework's.
//
// Mutation: recreate any of the three indexes 0064 dropped, and the inventory
// names it; drop `pages_heads_live_updated_idx` (replicated 0065), and the
// embedding duty's selection reads every page from the heap; read a page table
// in a new literal anywhere in the module without registering it, and the walk
// names the file; drop `pages_comments_page_idx`, and a thread and a purge's
// clear of the comments read every comment from the heap.
func TestEveryIndexServesARegisteredQuery(t *testing.T) {
	t.Parallel()
	db := pagePlanStore(t)
	covered := []string{"pages_heads", "pages_titles", "pages_revisions",
		"pages_comments", "pages_labels", "pages_watchers", "pages_skills",
		"pages_history"}

	used := map[string]bool{}
	var planned []string
	plan := func(t *testing.T, statement string, args []any) []string {
		t.Helper()
		steps := storetest.Plan(t, db, statement, args)
		for _, index := range storetest.IndexesIn(steps) {
			used[index] = true
		}
		return steps
	}
	refuse := func(t *testing.T, steps []string) {
		t.Helper()
		shown := strings.Join(steps, "\n")
		for _, table := range covered {
			if storetest.ScansHeap(steps, table) {
				t.Errorf("this read reads %s with no index:\n%s", table, shown)
			}
		}
		if line, unnested := storetest.Decorrelated(steps); unnested {
			t.Errorf("this read answers a per-row aggregate from its whole child "+
				"table (%q):\n%s", line, shown)
		}
	}

	for name, read := range pageReads(t) {
		planned = append(planned, read.sql)
		t.Run(name, func(t *testing.T) {
			steps := plan(t, read.sql, read.args)
			refuse(t, steps)
			shown := strings.Join(steps, "\n")
			switch {
			case !read.walk && !storetest.Seeks(steps, read.index):
				t.Errorf("this read does not SEEK %s:\n%s", read.index, shown)
			case read.walk && !storetest.Walks(steps, read.index):
				t.Errorf("this read does not walk %s in its order:\n%s", read.index, shown)
			case read.walk && storetest.Sorts(steps):
				t.Errorf("this read walks %s and then sorts what it read:\n%s",
					read.index, shown)
			}
		})
	}
	for name, census := range pageCensuses() {
		planned = append(planned, census.sql)
		t.Run(name, func(t *testing.T) {
			refuse(t, plan(t, census.sql, census.args))
		})
	}
	for name, pass := range pagePasses() {
		planned = append(planned, pass.sql)
		t.Run(name, func(t *testing.T) {
			if steps := plan(t, pass.sql, pass.args); storetest.Sorts(steps) {
				t.Errorf("this pass sorts the corpus it reads:\n%s",
					strings.Join(steps, "\n"))
			}
		})
	}

	// EVERY READER IN THE SOURCE IS ONE OF THEM.
	for _, literal := range storetest.StatementLiterals(t, "internal/doclet", covered...) {
		if !slices.ContainsFunc(planned, func(statement string) bool {
			return strings.Contains(statement, literal.Text)
		}) {
			t.Errorf("%s reads a page table in a statement this test does not "+
				"plan — register it with the reads, the censuses or the "+
				"passes:\n%s", literal.File, literal.Text)
		}
	}

	// AND EVERY INDEX IS IN SOME PLAN.
	for _, index := range storetest.IndexesOn(t, db, covered) {
		if !used[index] {
			t.Errorf("no registered statement reaches %s: an index no plan "+
				"reaches is a write on every apply for no reader", index)
		}
	}
}

// pagePlanned is one registered statement and the arguments it is planned
// with.
type pagePlanned struct {
	sql  string
	args []any
}

// pageIndexed is a registered read and the index it is held to: sought, or
// walked in its own order with no sort.
type pageIndexed struct {
	pagePlanned
	index string
	walk  bool
}

// pageReads is every read of one page, one link, one comment or one batch of
// pages by their keys — the reader's, the applier's and the writer's, and
// search's — each with the index it seeks.
func pageReads(t *testing.T) map[string]pageIndexed {
	t.Helper()
	const (
		heads    = "sqlite_autoindex_pages_heads_1"
		skills   = "sqlite_autoindex_pages_skills_1"
		labels   = "sqlite_autoindex_pages_labels_1"
		watchers = "sqlite_autoindex_pages_watchers_1"
		titles   = "sqlite_autoindex_pages_titles_1"
		revs     = "sqlite_autoindex_pages_revisions_1"
		comments = "sqlite_autoindex_pages_comments_1"
		history  = "sqlite_autoindex_pages_history_1"

		container = "pages_heads_container_idx"
		address   = "pages_heads_address_idx"
		parent    = "pages_heads_parent_idx"
		thread    = "pages_comments_page_idx"
		trashed   = "pages_heads_trashed_idx"
	)
	ids := []string{"p-0001", "p-0002"}
	seek := func(index, statement string, args ...any) pageIndexed {
		return pageIndexed{pagePlanned{statement, args}, index, false}
	}
	of := func(statement string, args []any) pagePlanned { return pagePlanned{statement, args} }
	at := func(index string, read pagePlanned) pageIndexed { return pageIndexed{read, index, false} }
	out := map[string]pageIndexed{
		// THE APPLIER'S, by the page's key or its container's.
		"a rename's head":                 seek(heads, retitleHeadStatement, "t", "t", "ENG", 1, 1, "{}", "p-0001", 1, 1),
		"a rename's release of its title": seek(titles, releaseTitleStatement, "ENG", "t", "p-0001", 1),
		"a purge's head":                  seek(heads, purgedHeadStatement, "p-0001"),
		"a purge's skill flag":            seek(skills, skillFlagStatement, "p-0001"),
		"a purge's removal of its head":   seek(heads, purgeHeadStatement, "p-0001"),
		"a page's children":               seek(parent, childrenOfStatement, "p-0001"),
		"a child re-filed":                seek(heads, refileChildStatement, "p", 1, "{}", "p-0001", 1, 1),
		"a head at a version":             seek(heads, headAtStatement, "p-0001"),
		"a held comment":                  seek(comments, heldCommentStatement, "c"),
		"a comment removed":               seek(comments, removeCommentStatement, "c", 1),
		"a page's watchers cleared":       seek(watchers, clearWatchersStatement, "p-0001"),
		"a revision retired":              seek(revs, retireRevisionStatement, "p-0001", 1),
		"a container's pages":             seek(container, containerHoldsStatement, "ENG"),

		// THE TRASH REAPER'S LISTING, of the pages trashed before the
		// term's cutoff: a seek of the trash's own index, which holds the
		// listing's order ([TestTheTrashIsListedInItsOrder]).
		"the trash past its term": seek(trashed, trashedBeforeStatement, 1, string(StatusTrashed), 100),

		// THE READER'S AND THE WRITER'S.
		"what a page was derived as": seek(skills, derivedAsStatement, "p-0001"),
		"a scope by id":              seek(heads, scopeByIDStatement, "p-0001"),
		"a scope by address":         seek(address, scopeByAddressStatement, "ENG", "t"),
		"a page by id":               seek(heads, locateByIDStatement, "p-0001"),
		"a page by address":          seek(address, locateByAddressStatement, "ENG", "t"),
		"a page's thread":            seek(thread, threadStatement, "p-0001"),
		"a page's revisions":         seek(revs, revisionListStatement, "p-0001", RevisionsKept),
		"one revision":               seek(revs, revisionStatement, "p-0001", 1),
		"an ancestor":                seek(heads, ancestorStatement, "p-0001"),
		"a container's skills":       seek(container, skillPagesStatement, "TS", string(StatusTrashed)),
		"a window's last saves":      seek(history, lastSavesStatement(len(ids)), "h-1", "h-2"),
		"a window's labels":          seek(labels, pageLabelsStatement(len(ids)), "p-0001", "p-0002"),
		"a window's children":        at(parent, of(childCountStatement(ids, listSpec{}))),
		"a comment in a write":       seek(comments, commentTxStatement, "c", "p-0001"),
		"a page's container":         seek(heads, containerOfStatement, "p-0001"),
		"a head in a write":          seek(heads, headTxStatement, "p-0001"),
		"a page's parent":            seek(heads, parentOfStatement, "p-0001"),
		"a write's guards":           seek(heads, pageGuardsStatement, "p-0001", "p-0001"),
		"a title's guard":            seek(titles, titleGuardStatement, "ENG", "t"),

		// SEARCH'S, which reads the pages for the lexical index and the
		// embedding duty: a batch of the live pages past a cursor, one batch
		// by its keys, and one page's opening.
		"the lexical walk":          at(heads, of(search.PageVersionsRead("p-0010", 500))),
		"the lexical fetch":         at(heads, of(search.PageFetchRead(ids))),
		"the lexical liveness":      at(heads, of(search.PageLiveRead(ids))),
		"a search's linked pages":   at(heads, of(search.LinkedPagesRead(ids))),
		"a selected page's opening": at(heads, of(search.PageOpeningRead("p-0001"))),
		"the withdrawn vectors":     at(heads, of(search.PageWithdrawals(100))),
	}
	// THE EMBEDDING DUTY'S SELECTION walks the live pages oldest change first
	// through the index on exactly its population (replicated 0065), so its
	// LIMIT stops it at a batch of stale pages rather than after a sort of
	// every page in the company.
	statement, args := search.PageSelection("m", 8, 100)
	out["the embedding duty's selection"] = pageIndexed{pagePlanned{statement, args},
		"pages_heads_live_updated_idx", true}
	// A PURGE'S CLEAR OF EVERY CHILD TABLE, and a save's of its labels, each
	// by the page: the table is chosen at run time, so these are registered
	// from the list the applier walks.
	clears := map[string]string{
		"pages_labels":    labels,
		"pages_watchers":  watchers,
		"pages_revisions": revs,
		"pages_comments":  thread,
		"pages_history":   "pages_history_page_idx",
		"pages_titles":    "pages_titles_page_idx",
		"pages_skills":    skills,
	}
	for _, child := range append(slices.Clone(purgedChildren), labelsTable) {
		index, ok := clears[child.table]
		if !ok {
			t.Fatalf("a purge clears %s, and this test names no index it seeks "+
				"there", child.table)
		}
		out["a page's "+child.table+" cleared"] = seek(index, childClearStatement(child), "p-0001")
	}
	return out
}

// pageCensuses is every statement whose question names a set of rows it reads
// whole: a listing's total and first window in every shape the surfaces ask for
// ([listingShapes]), the change feed in every shape, and the per-container
// counts the rail and the search report draw.
func pageCensuses() map[string]pagePlanned {
	out := map[string]pagePlanned{
		"the rail's counts": {pageCountsStatement, []any{string(StatusTrashed)}},
	}
	statement, args := search.PageCountRead()
	out["search's count of the pages"] = pagePlanned{statement, args}
	for name, shape := range listingShapes() {
		statement, args := listTotalStatement(shape.spec)
		out[name+": its total"] = pagePlanned{statement, args}
		statement, args = listRowsStatement(shape.spec, listSpec{}, DefaultLimit+1)
		out[name+": its window"] = pagePlanned{statement, args}
	}
	for name, q := range map[string]PageActivityQuery{
		"the digest":      {},
		"the audit":       {ActorKinds: []AuthorKind{AuthorOperator, AuthorHuman}},
		"one page's feed": {Page: "p-0001"},
		"one writer's":    {Actor: "h-7"},
		"one space's":     {Container: "ENG"},
	} {
		statement, args := pageActivityStatement(compilePageActivity(q), MaxPageChanges+1)
		out["the change feed, "+name] = pagePlanned{statement, args}
	}
	return out
}

// pagePasses is every statement that reads the whole live corpus by
// definition: the window report's text, and the embedding duty's coverage.
func pagePasses() map[string]pagePlanned {
	out := map[string]pagePlanned{}
	statement, args := search.PageTextRead()
	out["the window report's text"] = pagePlanned{statement, args}
	statement, args = search.PageCoverageCount("m", 8)
	out["the embedding duty's coverage"] = pagePlanned{statement, args}
	return out
}
