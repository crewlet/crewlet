package search_test

import (
	"errors"
	"reflect"
	"testing"

	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/search"
)

// Page ids in the uuid shape the link grammar reads.
const (
	runbookID    = "0b6f5a4e-6a41-4b6e-9d8c-3f1e2d4c5b6a"
	oncallID     = "1c7e6b5f-7b52-4c7f-8e9d-4a2f3e5d6c7b"
	postmortemID = "2d8f7c6a-8c63-4d8a-9fae-5b3a4f6e7d8c"
)

func linkedFrom(t *testing.T, x *search.Indexer, id string) search.Backlinks {
	t.Helper()
	got, err := x.LinkedFrom(t.Context(), id)
	if err != nil {
		t.Fatalf("linked from %s: %v", id, err)
	}
	return got
}

// "LINKED FROM" LISTS BOTH SOURCES, named from their rows: a page whose body
// carries the address and a task whose description does, each under the name
// a reader recognises it by — a page by its title, a task by its key — and
// neither the page linking to itself nor a link inside a code example.
func TestLinkedFromListsThePagesAndTasksThatCarryTheAddress(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	x := search.NewIndexer(db).WithLinks(pages.Links)

	page(t, db, runbookID, "ENG", "Provisioner runbook",
		"Permalink: "+pages.AddressPrefix+runbookID, 1)
	page(t, db, oncallID, "ENG", "Scheduler on-call",
		"When a host fails, follow [the runbook]("+pages.AddressPrefix+runbookID+").", 1)
	page(t, db, postmortemID, "ENG", "Postmortem",
		"An example link: `/pages/"+runbookID+"`.", 1)
	item(t, db, "i.retry", "ENG", "Retry PXE boot",
		"See https://crewlet.example.com/pages/"+runbookID, 1)
	indexAll(t, x)

	got := linkedFrom(t, x, runbookID)
	want := search.Backlinks{
		Pages: []search.PageLink{{ID: oncallID, Container: "ENG", Title: "Scheduler on-call"}},
		Tasks: []search.TaskLink{{ID: "i.retry", Key: "i.retry", Title: "Retry PXE boot", Status: "todo",
			Via: []string{search.TaskCitesPage}}},
		PagesTotal: 1, TasksTotal: 1,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("linked from = %+v\nwant          %+v", got, want)
	}
}

// AN INDEX STILL ON ITS FIRST LAP CANNOT SAY NOTHING LINKS HERE. Before the
// lap over both corpora finishes, the linking body may simply be unread, so
// the answer is [search.ErrIndexBuilding] — never empty lists, which a rail
// would draw as "no page or task links here" about a page that is linked.
func TestAnIndexStillBuildingRefusesRatherThanAnsweringEmpty(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	x := search.NewIndexer(db).WithLinks(pages.Links)

	page(t, db, runbookID, "ENG", "Provisioner runbook", "The runbook.", 1)
	page(t, db, oncallID, "ENG", "Scheduler on-call", "See /pages/"+runbookID, 1)
	got, err := x.LinkedFrom(t.Context(), runbookID)
	if !errors.Is(err, search.ErrIndexBuilding) {
		t.Fatalf("before the first lap: %+v, %v — want ErrIndexBuilding", got, err)
	}
	indexAll(t, x)
	if got := linkedFrom(t, x, runbookID); got.PagesTotal != 1 {
		t.Errorf("after the first lap: %+v, want the on-call page", got)
	}
}

// A LINK SOMEBODY REMOVED STOPS BEING ONE, and a source that leaves takes its
// links with it: the rows are REPLACED on every re-index, and the orphan pass's
// delete cascades — so an edit that drops the address and a page that is
// trashed both vanish from the list.
func TestABacklinkLeavesWithTheEditOrTheSourceThatCarriedIt(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	x := search.NewIndexer(db).WithLinks(pages.Links)

	page(t, db, runbookID, "ENG", "Provisioner runbook", "The runbook.", 1)
	page(t, db, oncallID, "ENG", "Scheduler on-call", "See /pages/"+runbookID, 1)
	page(t, db, postmortemID, "ENG", "Postmortem", "See /pages/"+runbookID, 1)
	indexAll(t, x)
	if got := linkedFrom(t, x, runbookID); got.PagesTotal != 2 {
		t.Fatalf("before: %+v, want both pages", got)
	}

	page(t, db, oncallID, "ENG", "Scheduler on-call", "No longer links anywhere.", 2)
	if _, err := db.Replicated().SQL().ExecContext(t.Context(),
		`UPDATE pages_heads SET status = 'trashed' WHERE id = ?`, postmortemID); err != nil {
		t.Fatal(err)
	}
	indexAll(t, x)
	if got := linkedFrom(t, x, runbookID); got.PagesTotal != 0 || len(got.Pages) != 0 {
		t.Errorf("after the edit and the trash: %+v, want nothing", got)
	}
}

// THE DERIVATION BUMP IS THE BACKFILL. A row indexed before the backlinks
// existed matches its source's version, so the version walk alone would never
// look at it again and no page written before the upgrade would ever list a
// link. The row's derivation is what brings it back into the walk.
func TestARowIndexedBeforeTheBacklinksIsRederived(t *testing.T) {
	t.Parallel()
	db := openStore(t)

	// AN INDEX FROM BEFORE: built with no link grammar, and stamped with the
	// derivation that predates it — what migration 0036 leaves every
	// existing row at.
	page(t, db, runbookID, "ENG", "Provisioner runbook", "The runbook.", 1)
	page(t, db, oncallID, "ENG", "Scheduler on-call", "See /pages/"+runbookID, 1)
	indexAll(t, search.NewIndexer(db))
	if _, err := db.SQL().ExecContext(t.Context(), `UPDATE kb_docs SET derivation = 0`); err != nil {
		t.Fatal(err)
	}

	x := search.NewIndexer(db).WithLinks(pages.Links)
	indexAll(t, x)
	if got := linkedFrom(t, x, runbookID); got.PagesTotal != 1 {
		t.Errorf("after an upgrade's first laps: %+v, want the on-call page — the "+
			"row matched its source's version and was never re-derived", got)
	}
}

// A TASK LINKS A PAGE TWO WAYS, and both are "linked from": naming it as one of
// its linked pages (a tracker relation) and citing its address in the
// description. A task that does both is listed once, with both ways named.
func TestATaskLinkedByRelationOrByItsDescriptionIsLinkedFrom(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	x := search.NewIndexer(db).WithLinks(pages.Links)

	page(t, db, runbookID, "ENG", "Provisioner runbook", "The runbook.", 1)
	item(t, db, "ENG-9", "ENG", "Flaky e2e", "No address here.", 1)
	item(t, db, "ENG-10", "ENG", "Retry PXE boot", "See /pages/"+runbookID, 1)
	for _, id := range []string{"ENG-9", "ENG-10"} {
		if _, err := db.Replicated().SQL().ExecContext(t.Context(), `
			INSERT INTO tracker_relations (task_id, other_id, kind) VALUES (?, ?, 'page')`,
			id, runbookID); err != nil {
			t.Fatal(err)
		}
	}
	indexAll(t, x)

	got := linkedFrom(t, x, runbookID)
	want := []search.TaskLink{
		{ID: "ENG-9", Key: "ENG-9", Title: "Flaky e2e", Status: "todo",
			Via: []string{search.TaskLinkedPage}},
		{ID: "ENG-10", Key: "ENG-10", Title: "Retry PXE boot", Status: "todo",
			Via: []string{search.TaskLinkedPage, search.TaskCitesPage}},
	}
	if !reflect.DeepEqual(got.Tasks, want) || got.TasksTotal != 2 {
		t.Errorf("tasks (%d) = %+v\nwant %+v — ENG-9 before ENG-10, as a person reads keys",
			got.TasksTotal, got.Tasks, want)
	}
}

// A BACKLINKED TASK OPENS THE TASK, NOT THE KEY'S CLAIMANT. Two tasks can hold
// one key — a restored counter mints a number a task already holds — and the
// tracker answers a key with the task that claimed it first, so a "linked
// from" row that carried only the duplicate's key would open somebody else's
// task. The row says so with the tracker's own `key_collision`, which the
// applier derives and every tracker reader reads as a column; the fixture
// writes the rows the applier leaves (the directory naming the claimant, the
// flag on the duplicate) rather than running a second copy of that rule.
//
// The claimant is listed first, then the duplicate: one key, so the key alone
// orders nothing, and a tie left to the batched read's row order drew the two
// either way round. The duplicate's id sorts BEFORE the claimant's here, so an
// order by id alone is caught too.
//
// Mutation: drop the flag from the read (or read the bare key) and the
// duplicate's row opens the claimant; drop the claimant-first tie-break and
// the order flips.
func TestABacklinkedTaskWhoseKeyAnotherClaimedSaysSo(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	x := search.NewIndexer(db).WithLinks(pages.Links)

	page(t, db, runbookID, "ENG", "Provisioner runbook", "The runbook.", 1)
	item(t, db, "t-claimant", "ENG", "The claimant", "See /pages/"+runbookID, 1)
	item(t, db, "t-a-duplicate", "ENG", "The duplicate", "See /pages/"+runbookID, 1)
	for _, stmt := range []struct {
		sql  string
		args []any
	}{
		{`UPDATE tracker_tasks SET key = 'ENG-7', key_collision = 0 WHERE id = ?`,
			[]any{"t-claimant"}},
		{`UPDATE tracker_tasks SET key = 'ENG-7', key_collision = 1 WHERE id = ?`,
			[]any{"t-a-duplicate"}},
		{`INSERT INTO tracker_task_keys (key, task_id, current) VALUES ('ENG-7', ?, 1)`,
			[]any{"t-claimant"}},
	} {
		if _, err := db.Replicated().SQL().ExecContext(t.Context(), stmt.sql,
			stmt.args...); err != nil {
			t.Fatalf("%s: %v", stmt.sql, err)
		}
	}
	indexAll(t, x)

	got := linkedFrom(t, x, runbookID)
	want := []search.TaskLink{
		{ID: "t-claimant", Key: "ENG-7", Title: "The claimant", Status: "todo",
			Via: []string{search.TaskCitesPage}},
		{ID: "t-a-duplicate", Key: "ENG-7", KeyCollision: true, Title: "The duplicate",
			Status: "todo", Via: []string{search.TaskCitesPage}},
	}
	if !reflect.DeepEqual(got.Tasks, want) || got.TasksTotal != 2 {
		t.Errorf("tasks (%d) = %+v\nwant %+v — the duplicate's row must say its key "+
			"opens the claimant, and the claimant's must not", got.TasksTotal, got.Tasks, want)
	}
}
