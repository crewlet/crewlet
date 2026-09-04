package work_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/projection"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/work"
)

// projected runs a real projector over a real store, so what these tests read
// is what a node would serve.
func projected(t *testing.T) (*work.Store, *store.DB) {
	t.Helper()
	docs := memory.NewFleet()
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "w.db"), store.Options{})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	s, err := work.NewStore(work.Options{Documents: docs})
	if err != nil {
		t.Fatal(err)
	}
	p, err := projection.New(projection.Options{
		Documents: docs, DB: db, Applier: work.NewApplier(),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- p.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("the projector did not stop")
		}
	})
	settle(t, func() bool { return p.Hydrated() }, "the projector never hydrated")
	t.Cleanup(func() {})
	return s, db
}

func settle(t *testing.T, want func() bool, why string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if want() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal(why)
}

func rowCount(t *testing.T, db *store.DB, query string, args ...any) int {
	t.Helper()
	var n int
	if err := db.SQL().QueryRowContext(t.Context(), query, args...).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

// A WRITE REACHES THE BOARD. The whole reason the projection exists is that a
// board is a local query, so this is the end-to-end claim the design rests on.
func TestAWrittenItemBecomesAQueryableRow(t *testing.T) {
	t.Parallel()
	s, db := projected(t)
	got := file(t, s, human("jane"), work.NewItem{
		Assignee: "eng", Title: "ship the thing", Labels: []string{"backend", "q1"},
	})

	settle(t, func() bool {
		return rowCount(t, db, `SELECT COUNT(*) FROM work_items WHERE item_key = ?`, got.Item.Key) == 1
	}, "the item never reached the projection")

	var (
		status, assignee, title string
		revision                int64
	)
	if err := db.SQL().QueryRowContext(t.Context(),
		`SELECT status, assignee, title, revision FROM work_items WHERE item_key = ?`,
		got.Item.Key).Scan(&status, &assignee, &title, &revision); err != nil {
		t.Fatal(err)
	}
	if status != string(work.StatusTodo) || assignee != "eng" || title != "ship the thing" {
		t.Errorf("row = %q %q %q", status, assignee, title)
	}
	if uint64(revision) != got.Revision {
		t.Errorf("the row carries revision %d, the write reported %d — a caller "+
			"conditioning its next write on the ETag would be refused",
			revision, got.Revision)
	}
	if n := rowCount(t, db, `SELECT COUNT(*) FROM work_labels WHERE item_id = ?`, got.Item.ID); n != 2 {
		t.Errorf("labels projected = %d, want 2", n)
	}
	if n := rowCount(t, db,
		`SELECT COUNT(*) FROM work_watchers WHERE item_id = ? AND muted = 0`, got.Item.ID); n != 2 {
		t.Errorf("watchers projected = %d, want the reporter and the assignee", n)
	}
	if n := rowCount(t, db, `SELECT last FROM work_counters WHERE project = 'ENG'`); n != 1 {
		t.Errorf("the counter projected as %d", n)
	}
}

// BOTH DIRECTIONS OF A LINK ARE DERIVED HERE, so a board renders "blocked by"
// without a second authored record that could disagree with the first.
func TestALinkIsProjectedInBothDirections(t *testing.T) {
	t.Parallel()
	s, db := projected(t)
	blocker := file(t, s, human("jane"), work.NewItem{Title: "the blocker"})
	blocked := file(t, s, human("jane"), work.NewItem{Title: "the blocked"})

	if _, err := s.Update(t.Context(), human("jane"), blocker.Item.ID, 0, work.Edit{
		AddLinks: []work.Link{{Kind: work.LinkBlocks, To: blocked.Item.ID}},
	}); err != nil {
		t.Fatal(err)
	}
	settle(t, func() bool {
		return rowCount(t, db, `SELECT COUNT(*) FROM work_links WHERE item_id = ?`, blocker.Item.ID) == 1
	}, "the authored link never projected")
	settle(t, func() bool {
		return rowCount(t, db,
			`SELECT COUNT(*) FROM work_links WHERE item_id = ? AND derived = 1 AND kind = 'blocked_by'`,
			blocked.Item.ID) == 1
	}, "the inverse link was never derived")

	// REMOVING THE AUTHORED END REMOVES BOTH, or the board keeps showing a
	// dependency nobody holds.
	if _, err := s.Update(t.Context(), human("jane"), blocker.Item.ID, 0, work.Edit{
		RemoveLinks: []work.Link{{Kind: work.LinkBlocks, To: blocked.Item.ID}},
	}); err != nil {
		t.Fatal(err)
	}
	settle(t, func() bool {
		return rowCount(t, db, `SELECT COUNT(*) FROM work_links`) == 0
	}, "a removed link left its derived half behind")
}

// A COMMENT AND ITS CHANGE BOTH PROJECT, and the history is append-only —
// which is what an item's activity panel renders and what an audit answers
// from.
func TestTheThreadAndTheHistoryProject(t *testing.T) {
	t.Parallel()
	s, db := projected(t)
	got := file(t, s, human("jane"), work.NewItem{Assignee: "eng"})
	if _, _, err := s.Comment(t.Context(), agent("eng"), got.Item.ID,
		work.NewComment{Body: "on it"}); err != nil {
		t.Fatal(err)
	}
	settle(t, func() bool {
		return rowCount(t, db, `SELECT COUNT(*) FROM work_comments WHERE item_id = ?`, got.Item.ID) == 1 &&
			rowCount(t, db, `SELECT COUNT(*) FROM work_history WHERE item_id = ?`, got.Item.ID) == 2
	}, "the comment or its change never projected")

	var kind, actor, excerpt string
	if err := db.SQL().QueryRowContext(t.Context(),
		`SELECT kind, actor, excerpt FROM work_history WHERE item_id = ? ORDER BY created_at DESC LIMIT 1`,
		got.Item.ID).Scan(&kind, &actor, &excerpt); err != nil {
		t.Fatal(err)
	}
	if kind != string(work.ChangeComment) || actor != "eng" || excerpt != "on it" {
		t.Errorf("the history row = %q %q %q", kind, actor, excerpt)
	}
}

// A REMOVAL TRAVELS AND TAKES EVERYTHING WITH IT. Nothing re-converges a
// deleted item — unlike a seat's diary — so a removal that left rows behind
// would leave the item on somebody's board until a person deleted it twice.
func TestRemovingAnItemClearsEveryDerivedRow(t *testing.T) {
	t.Parallel()
	s, db := projected(t)
	got := file(t, s, human("jane"), work.NewItem{Assignee: "eng", Labels: []string{"x"}})
	if _, _, err := s.Comment(t.Context(), human("jane"), got.Item.ID,
		work.NewComment{Body: "a remark"}); err != nil {
		t.Fatal(err)
	}
	settle(t, func() bool {
		return rowCount(t, db, `SELECT COUNT(*) FROM work_comments`) == 1
	}, "the comment never projected")

	if err := s.Remove(t.Context(), human("jane"), got.Item.ID); err != nil {
		t.Fatal(err)
	}
	settle(t, func() bool {
		return rowCount(t, db, `SELECT COUNT(*) FROM work_items`) == 0
	}, "the item survived removal in the projection")

	for _, table := range []string{"work_comments", "work_labels", "work_watchers", "work_history"} {
		if n := rowCount(t, db, `SELECT COUNT(*) FROM `+table); n != 0 {
			t.Errorf("%s kept %d rows for a removed item", table, n)
		}
	}
}

// AN APPLY IS IDEMPOTENT BY REVISION, which is what makes a crash between a
// batch commit and the cursor write free: the replay reaches the same rows.
func TestReplayingAChangeChangesNothing(t *testing.T) {
	t.Parallel()
	s, db := projected(t)
	got := file(t, s, human("jane"), work.NewItem{Title: "once"})
	settle(t, func() bool {
		return rowCount(t, db, `SELECT COUNT(*) FROM work_items`) == 1
	}, "the item never projected")

	if _, err := s.Update(t.Context(), human("jane"), got.Item.ID, 0,
		work.Edit{Title: ptr("twice")}); err != nil {
		t.Fatal(err)
	}
	settle(t, func() bool {
		var title string
		_ = db.SQL().QueryRowContext(t.Context(),
			`SELECT title FROM work_items WHERE id = ?`, got.Item.ID).Scan(&title)
		return title == "twice"
	}, "the edit never projected")

	if n := rowCount(t, db, `SELECT COUNT(*) FROM work_items`); n != 1 {
		t.Errorf("an edit produced %d rows", n)
	}
	if n := rowCount(t, db, `SELECT COUNT(*) FROM work_history WHERE item_id = ?`, got.Item.ID); n != 2 {
		t.Errorf("history = %d rows, want created + the edit", n)
	}
}

// A MUTED WATCHER IS A ROW, not an absence: "this person unwatched" is a fact
// a re-add has to consult, and a projection that dropped it would let the
// next assignment silently re-subscribe somebody who opted out.
func TestAMuteIsProjectedAsItsOwnFact(t *testing.T) {
	t.Parallel()
	s, db := projected(t)
	got := file(t, s, human("jane"), work.NewItem{Assignee: "eng"})
	if _, err := s.Update(t.Context(), human("eng"), got.Item.ID, 0,
		work.Edit{Watch: ptr(false)}); err != nil {
		t.Fatal(err)
	}
	settle(t, func() bool {
		return rowCount(t, db,
			`SELECT COUNT(*) FROM work_watchers WHERE item_id = ? AND handle = 'eng' AND muted = 1`,
			got.Item.ID) == 1
	}, "the mute never projected as its own row")
	if n := rowCount(t, db,
		`SELECT COUNT(*) FROM work_watchers WHERE item_id = ? AND muted = 0`, got.Item.ID); n != 1 {
		t.Errorf("active watchers = %d, want only the reporter", n)
	}
}

// A KEY CLASS THIS BUILD HAS NO RULE FOR IS IGNORED, not fatal. A newer node
// writes classes this one does not know, and a rolling upgrade must not wedge
// the older half's projector on a record it was never meant to understand.
func TestAnUnknownKeyClassDoesNotWedgeTheProjector(t *testing.T) {
	t.Parallel()
	docs := memory.NewFleet()
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "w.db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	// A class from a build that ships something this one does not.
	if _, err := docs.CreateDocument(t.Context(), work.NewApplier().Family(),
		"z.something.new", []byte(`{"v":1}`)); err != nil {
		t.Fatal(err)
	}
	s, err := work.NewStore(work.Options{Documents: docs})
	if err != nil {
		t.Fatal(err)
	}
	p, err := projection.New(projection.Options{Documents: docs, DB: db, Applier: work.NewApplier()})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() { _ = p.Run(ctx) }()
	settle(t, func() bool { return p.Hydrated() },
		"an unknown key class stopped the projector hydrating")

	got := file(t, s, human("jane"), work.NewItem{Title: "still works"})
	settle(t, func() bool {
		return rowCount(t, db, `SELECT COUNT(*) FROM work_items WHERE id = ?`, got.Item.ID) == 1
	}, "ordinary items stopped projecting after an unknown class")
}

// EVERY SCREEN'S QUERY MUST SEEK. A partial index the planner cannot use is
// worse than no index: it looks present in the schema, and the failure is a
// board that gets slower every month with nothing saying why. SQLite's
// partial-index prover is SYNTACTIC — it does not derive `assignee <> ”`
// from `assignee = 'eng'` — so this asserts the plans rather than the DDL.
func TestEveryBoardQuerySeeksRatherThanScans(t *testing.T) {
	t.Parallel()
	s, db := projected(t)
	for i := range 6 {
		in := work.NewItem{Title: "item"}
		if i%2 == 0 {
			in.Assignee = "eng"
		}
		file(t, s, human("jane"), in)
	}
	settle(t, func() bool { return rowCount(t, db, `SELECT COUNT(*) FROM work_items`) == 6 },
		"the items never projected")

	for _, tc := range []struct{ name, query string }{
		{"a project board", `SELECT * FROM work_items WHERE project = 'ENG' AND status = 'todo' ORDER BY updated_at DESC`},
		{"a seat's own work", `SELECT * FROM work_items WHERE assignee = 'eng' AND status = 'todo' ORDER BY updated_at DESC`},
		{"the triage lane", `SELECT * FROM work_items WHERE assignee = '' AND status = 'triage'`},
		{"a parent's children", `SELECT * FROM work_items WHERE parent_id = 'i0'`},
		{"an item's thread", `SELECT * FROM work_comments WHERE item_id = 'i0' ORDER BY created_at`},
		{"an item's history", `SELECT * FROM work_history WHERE item_id = 'i0' ORDER BY created_at DESC`},
		{"what a seat watches", `SELECT * FROM work_watchers WHERE handle = 'eng'`},
		{"a label filter", `SELECT * FROM work_labels WHERE label = 'backend'`},
		{"the links pointing at an item", `SELECT * FROM work_links WHERE other_id = 'i0' AND kind = 'blocks'`},
		// The two PARTIAL indexes: their queries must restate the
		// predicate, and this is what says so if a sweep forgets.
		{"the closed-item sweep", `SELECT * FROM work_items WHERE closed_at IS NOT NULL AND closed_at < 100`},
		{"the change sweep", `SELECT * FROM work_history WHERE created_at < 100`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			for _, step := range queryPlan(t, db, tc.query) {
				if strings.HasPrefix(step, "SCAN ") {
					t.Errorf("scans rather than seeks:\n  %s\n  %s", tc.query, step)
				}
			}
		})
	}
}

func queryPlan(t *testing.T, db *store.DB, query string) []string {
	t.Helper()
	rows, err := db.SQL().QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+query)
	if err != nil {
		t.Fatalf("explain %q: %v", query, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var a, b, c int
		var detail string
		if err := rows.Scan(&a, &b, &c, &detail); err != nil {
			t.Fatalf("scan plan: %v", err)
		}
		out = append(out, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("explain %q: %v", query, err)
	}
	return out
}

// A BOOT RECONCILE ENUMERATES KEYS IN MAP ORDER, so a comment can be reached
// before its item. Skipping it there is PERMANENT: the projection key set
// records the child as applied at that revision, so no later reconcile
// re-fetches it and nothing anywhere says a thread is short.
//
// Measured before [work.Applier.Order] existed: a fresh node projected twelve
// of a twenty-comment thread, differently on every run.
func TestABootReconcileNeverDropsAThread(t *testing.T) {
	t.Parallel()
	docs := memory.NewFleet()
	s, err := work.NewStore(work.Options{Documents: docs})
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.Create(t.Context(), human("jane"),
		work.NewItem{Project: "ENG", Type: work.TypeTask, Title: "t"})
	if err != nil {
		t.Fatal(err)
	}
	const comments = 20
	for i := range comments {
		if _, _, err := s.Comment(t.Context(), human("jane"), got.Item.ID,
			work.NewComment{Body: "remark"}); err != nil {
			t.Fatalf("comment %d: %v", i, err)
		}
	}

	// A FRESH node, so its whole projection is built by the reconcile
	// rather than by the live watch — which is the path that has an order.
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "fresh.db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	p, err := projection.New(projection.Options{
		Documents: docs, DB: db, Applier: work.NewApplier(),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() { _ = p.Run(ctx) }()
	settle(t, func() bool { return p.Hydrated() }, "the fresh node never hydrated")

	if n := rowCount(t, db, `SELECT COUNT(*) FROM work_comments`); n != comments {
		t.Errorf("the reconcile projected %d of %d comments", n, comments)
	}
	if n := rowCount(t, db, `SELECT COUNT(*) FROM work_history`); n != comments+1 {
		t.Errorf("the reconcile projected %d of %d history rows", n, comments+1)
	}
}

// The ranks put a parent before its children, and an unknown class LAST —
// ranking it early would make the skip depend on the order of a map.
func TestTheApplierRanksParentsBeforeChildren(t *testing.T) {
	t.Parallel()
	a := work.NewApplier()
	item := a.Order(work.ItemKey("i1"))
	for _, child := range []string{work.CommentKey("i1", "m1"), work.ChangeKey("i1", "c1")} {
		if a.Order(child) <= item {
			t.Errorf("%q ranks at or before its item", child)
		}
	}
	if a.Order(work.CounterKey("ENG")) >= item {
		t.Error("the counter ranks after the item it numbers")
	}
	for _, foreign := range []string{"z.unknown.class", "not a key"} {
		if a.Order(foreign) <= a.Order(work.ChangeKey("i1", "c1")) {
			t.Errorf("a class this build has no rule for (%q) ranks before a known child", foreign)
		}
	}
}
