package projection_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/projection"
	"github.com/crewlet/crewlet/internal/store"
)

// ---- the fixture ------------------------------------------------------ //

func openStore(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "p.db"), store.Options{})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// rowApplier is a minimal applier: it writes one row per key into a scratch
// table, so a test can assert what a projector applied without dragging in
// the tracker's or the wiki's own schema.
//
// It exercises the two rules the contract binds an applier to — idempotent by
// key, and a pure function of (row, change) — so a projector defect shows up
// here as a wrong row rather than as a domain quirk somewhere else.
type rowApplier struct {
	family projection.Family

	mu      sync.Mutex
	applies int
	commits int
	fail    error
}

func newRowApplier(t *testing.T, db *store.DB, family projection.Family) *rowApplier {
	t.Helper()
	if _, err := db.SQL().ExecContext(t.Context(), `
		CREATE TABLE IF NOT EXISTS projection_probe (
			key      TEXT NOT NULL PRIMARY KEY,
			value    TEXT NOT NULL,
			revision INTEGER NOT NULL
		)`); err != nil {
		t.Fatalf("probe table: %v", err)
	}
	return &rowApplier{family: family}
}

func (a *rowApplier) Family() projection.Family { return a.family }

func (a *rowApplier) Apply(ctx context.Context, tx *sql.Tx, c coord.Change) error {
	a.mu.Lock()
	a.applies++
	err := a.fail
	a.mu.Unlock()
	if err != nil {
		return err
	}
	if c.Op == coord.OpPurge {
		_, derr := tx.ExecContext(ctx, `DELETE FROM projection_probe WHERE key = ?`, c.Key)
		return derr
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO projection_probe (key, value, revision) VALUES (?, ?, ?)
		ON CONFLICT (key) DO UPDATE SET value = excluded.value, revision = excluded.revision`,
		c.Key, string(c.Value), int64(c.Revision))
	return err
}

// Order is constant: this probe's records have no parents, which is the case
// the contract explicitly allows — an applier with no hierarchy returns one
// rank and loses nothing.
func (a *rowApplier) Order(string) int { return 0 }

func (a *rowApplier) Reset(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `DELETE FROM projection_probe`)
	return err
}

// Committed counts the post-commit hook, so the "a failed batch fires no side
// effect" case can assert on it.
func (a *rowApplier) Committed(context.Context) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.commits++
}

func (a *rowApplier) committed() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.commits
}

func (a *rowApplier) breakWith(err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.fail = err
}

func (a *rowApplier) count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.applies
}

// rows is what the probe table holds, key -> value.
func rows(t *testing.T, db *store.DB) map[string]string {
	t.Helper()
	out := map[string]string{}
	r, err := db.SQL().QueryContext(t.Context(), `SELECT key, value FROM projection_probe`)
	if err != nil {
		t.Fatalf("read probe: %v", err)
	}
	defer r.Close()
	for r.Next() {
		var k, v string
		if err := r.Scan(&k, &v); err != nil {
			t.Fatalf("scan probe: %v", err)
		}
		out[k] = v
	}
	if err := r.Err(); err != nil {
		t.Fatalf("read probe: %v", err)
	}
	return out
}

// put writes a document and returns its revision.
func put(t *testing.T, docs coord.Documents, family coord.Family, key, value string) uint64 {
	t.Helper()
	ctx := t.Context()
	if created, err := docs.CreateDocument(ctx, family, key, []byte(value)); err != nil {
		t.Fatalf("create %s: %v", key, err)
	} else if created {
		rec, _, err := docs.Document(ctx, family, key)
		if err != nil {
			t.Fatalf("read back %s: %v", key, err)
		}
		return rec.Version
	}
	rec, _, err := docs.Document(ctx, family, key)
	if err != nil {
		t.Fatalf("read %s: %v", key, err)
	}
	ok, err := docs.UpdateDocument(ctx, family, key, []byte(value), rec.Version)
	if err != nil || !ok {
		t.Fatalf("update %s: ok=%v err=%v", key, ok, err)
	}
	rec, _, err = docs.Document(ctx, family, key)
	if err != nil {
		t.Fatalf("read back %s: %v", key, err)
	}
	return rec.Version
}

// start runs a projector and stops it with the test.
func start(t *testing.T, p *projection.Projector) {
	t.Helper()
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
}

// awaitHydrated blocks until the projector reports hydrated.
func awaitHydrated(t *testing.T, p *projection.Projector) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if p.Hydrated() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("the projector never hydrated: %+v", p.Status())
}

func newProjector(t *testing.T, docs coord.Documents, db *store.DB, a projection.Applier) *projection.Projector {
	t.Helper()
	p, err := projection.New(projection.Options{Documents: docs, DB: db, Applier: a})
	if err != nil {
		t.Fatalf("new projector: %v", err)
	}
	return p
}

// ---- the cases -------------------------------------------------------- //

// A BOOT ENUMERATES THE BUCKET, it does not resume a number. Everything the
// bucket held before this node existed has to land, or the node serves a
// board that says the company has no work.
func TestABootProjectsEverythingTheBucketAlreadyHeld(t *testing.T) {
	t.Parallel()
	docs := memory.NewFleet()
	db := openStore(t)
	for i := range 5 {
		put(t, docs, projection.FamilyWork, fmt.Sprintf("i.%d", i), fmt.Sprintf("item %d", i))
	}
	p := newProjector(t, docs, db, newRowApplier(t, db, projection.FamilyWork))
	start(t, p)
	awaitHydrated(t, p)

	got := rows(t, db)
	if len(got) != 5 {
		t.Fatalf("projected %d rows, want the 5 the bucket held: %v", len(got), got)
	}
	for i := range 5 {
		if want := fmt.Sprintf("item %d", i); got[fmt.Sprintf("i.%d", i)] != want {
			t.Errorf("i.%d = %q, want %q", i, got[fmt.Sprintf("i.%d", i)], want)
		}
	}
}

// HYDRATION IS THE FACT A MAILBOX WAITS ON, and a read taken before it is
// refused rather than answered empty: "this company has no work" is an answer
// a seat acts on.
func TestAReadBeforeHydrationIsRefusedRatherThanEmpty(t *testing.T) {
	t.Parallel()
	docs := memory.NewFleet()
	db := openStore(t)
	p := newProjector(t, docs, db, newRowApplier(t, db, projection.FamilyWork))
	if p.Hydrated() {
		t.Fatal("a projector that has not run reports hydrated")
	}
	if got := p.Status(); got.Hydrated || got.Revision != 0 {
		t.Errorf("status before Run = %+v", got)
	}
	// The sentinel exists so a caller can tell this state from absence.
	if !errors.Is(projection.ErrNotHydrated, projection.ErrNotHydrated) {
		t.Error("the sentinel is not comparable")
	}
}

// A LIVE WRITE REACHES EVERY NODE, which is what makes this a projection of a
// shared record rather than a per-node table.
func TestALiveWriteLandsOnBothNodes(t *testing.T) {
	t.Parallel()
	docs := memory.NewFleet()
	dbA, dbB := openStore(t), openStore(t)
	a := newProjector(t, docs, dbA, newRowApplier(t, dbA, projection.FamilyWork))
	b := newProjector(t, docs, dbB, newRowApplier(t, dbB, projection.FamilyWork))
	start(t, a)
	start(t, b)
	awaitHydrated(t, a)
	awaitHydrated(t, b)

	rev := put(t, docs, projection.FamilyWork, "i.new", "filed on A")
	for _, tc := range []struct {
		name string
		p    *projection.Projector
		db   *store.DB
	}{{"A", a, dbA}, {"B", b, dbB}} {
		if err := tc.p.WaitApplied(t.Context(), rev); err != nil {
			t.Fatalf("node %s never applied revision %d: %v", tc.name, rev, err)
		}
		if got := rows(t, tc.db)["i.new"]; got != "filed on A" {
			t.Errorf("node %s has %q", tc.name, got)
		}
	}
}

// A PURGE TRAVELS. Nothing re-converges a deleted item — unlike a seat's
// diary, which the lifecycle pass drops again — so a removal that did not
// reach a peer would leave the item on somebody's board until a person
// deleted it a second time.
func TestAPurgeRemovesTheRowAndStaysRemoved(t *testing.T) {
	t.Parallel()
	docs := memory.NewFleet()
	db := openStore(t)
	put(t, docs, projection.FamilyWork, "i.gone", "here")
	p := newProjector(t, docs, db, newRowApplier(t, db, projection.FamilyWork))
	start(t, p)
	awaitHydrated(t, p)
	if rows(t, db)["i.gone"] != "here" {
		t.Fatal("the row did not project")
	}

	rec, _, err := docs.Document(t.Context(), projection.FamilyWork, "i.gone")
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := docs.PurgeDocument(t.Context(), projection.FamilyWork, "i.gone", rec.Version); err != nil || !ok {
		t.Fatalf("purge: ok=%v err=%v", ok, err)
	}
	waitFor(t, func() bool { _, still := rows(t, db)["i.gone"]; return !still },
		"the purge never reached the projection")

	// AND A RESTART DOES NOT RESURRECT IT. The key set records that the
	// removal was applied, so the next reconcile neither re-fetches it nor
	// re-applies the purge.
	p.Stop()
	applier := newRowApplier(t, db, projection.FamilyWork)
	p2 := newProjector(t, docs, db, applier)
	start(t, p2)
	awaitHydrated(t, p2)
	if _, back := rows(t, db)["i.gone"]; back {
		t.Error("a restart resurrected a purged item")
	}
	if applier.count() != 0 {
		t.Errorf("the restart re-applied %d changes for an unchanged bucket", applier.count())
	}
}

// A RESTART RE-APPLIES NOTHING it has already applied. The cursor and the key
// set together are what make that true, and without it every boot would
// rewrite every row in the company.
func TestARestartAppliesOnlyWhatChanged(t *testing.T) {
	t.Parallel()
	docs := memory.NewFleet()
	db := openStore(t)
	for i := range 3 {
		put(t, docs, projection.FamilyWork, fmt.Sprintf("i.%d", i), "v1")
	}
	first := newRowApplier(t, db, projection.FamilyWork)
	p := newProjector(t, docs, db, first)
	start(t, p)
	awaitHydrated(t, p)
	if first.count() != 3 {
		t.Fatalf("the first boot applied %d, want 3", first.count())
	}
	p.Stop()

	put(t, docs, projection.FamilyWork, "i.1", "v2")

	second := newRowApplier(t, db, projection.FamilyWork)
	p2 := newProjector(t, docs, db, second)
	start(t, p2)
	awaitHydrated(t, p2)
	if second.count() != 1 {
		t.Errorf("the second boot applied %d changes, want only the one that moved", second.count())
	}
	if got := rows(t, db)["i.1"]; got != "v2" {
		t.Errorf("i.1 = %q after the change", got)
	}
}

// THE CASE A CURSOR COMPARISON CANNOT SEE: this node's stored cursor is above
// anything the bucket holds, which is what a cold restore from an older
// backup, an in-memory bucket recreated at sequence 1, and a cloned data
// directory all produce. A watch resumed from there delivers nothing and
// reports caught-up, so a cursor-only boot would sit over an empty projection
// for ever.
func TestABootConvergesFromACursorAheadOfTheBucket(t *testing.T) {
	t.Parallel()
	docs := memory.NewFleet()
	db := openStore(t)
	newRowApplier(t, db, projection.FamilyWork)

	// A cursor from a deployment whose bucket held far more than this one.
	if _, err := db.SQL().ExecContext(t.Context(), `
		INSERT INTO projection_cursor (family, revision, hydrated, updated_at)
		VALUES (?, 999999, 1, 0)`, string(projection.FamilyWork)); err != nil {
		t.Fatal(err)
	}
	put(t, docs, projection.FamilyWork, "i.restored", "from the backup")

	p := newProjector(t, docs, db, newRowApplier(t, db, projection.FamilyWork))
	start(t, p)
	awaitHydrated(t, p)
	if got := rows(t, db)["i.restored"]; got != "from the backup" {
		t.Fatalf("the projection stayed empty behind a stale cursor: %v", rows(t, db))
	}
}

// A ROW THE BUCKET NO LONGER HOLDS IS DELETED AT BOOT, even though this node
// never saw the purge — a node that was down when an item was removed would
// otherwise carry it for ever.
func TestABootDropsWhatTheBucketNoLongerHolds(t *testing.T) {
	t.Parallel()
	docs := memory.NewFleet()
	db := openStore(t)
	newRowApplier(t, db, projection.FamilyWork)
	if _, err := db.SQL().ExecContext(t.Context(), `
		INSERT INTO projection_probe (key, value, revision) VALUES ('i.stale', 'left over', 4)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL().ExecContext(t.Context(), `
		INSERT INTO projection_keys (family, key, revision, purged) VALUES (?, 'i.stale', 4, 0)`,
		string(projection.FamilyWork)); err != nil {
		t.Fatal(err)
	}
	put(t, docs, projection.FamilyWork, "i.live", "current")

	p := newProjector(t, docs, db, newRowApplier(t, db, projection.FamilyWork))
	start(t, p)
	awaitHydrated(t, p)
	got := rows(t, db)
	if _, still := got["i.stale"]; still {
		t.Error("a row the bucket no longer holds survived the boot reconcile")
	}
	if got["i.live"] != "current" {
		t.Errorf("the live row is %q", got["i.live"])
	}
}

// AN APPLY FAILURE DROPS HYDRATION rather than leaving a stale projection
// answering reads: a projector that has stopped following is one whose rows
// are going stale, and a mailbox that attached on an earlier hydration would
// keep answering from them.
func TestAFailingApplyDropsHydration(t *testing.T) {
	t.Parallel()
	docs := memory.NewFleet()
	db := openStore(t)
	applier := newRowApplier(t, db, projection.FamilyWork)
	p := newProjector(t, docs, db, applier)
	start(t, p)
	awaitHydrated(t, p)

	applier.breakWith(errors.New("the store said no"))
	put(t, docs, projection.FamilyWork, "i.doomed", "never lands")
	waitFor(t, func() bool { return !p.Hydrated() },
		"a failing apply left the projector reporting hydrated")
	if got := p.Status(); !strings.Contains(got.Err, "the store said no") {
		t.Errorf("the status does not name the failure: %+v", got)
	}

	// AND IT RECOVERS. The cycle re-enters through the reconcile, so a
	// transient store failure is not a projection that stays dead.
	applier.breakWith(nil)
	waitFor(t, func() bool { return p.Hydrated() && rows(t, db)["i.doomed"] == "never lands" },
		"the projector never recovered after the store came back")
}

// WaitApplied IS THE READ-YOUR-WRITES PRIMITIVE, and its failure is "not here
// yet", never "the write failed" — the write was acknowledged by coordination
// before anyone waited on it.
func TestWaitAppliedReportsNotYetRatherThanFailure(t *testing.T) {
	t.Parallel()
	docs := memory.NewFleet()
	db := openStore(t)
	p := newProjector(t, docs, db, newRowApplier(t, db, projection.FamilyWork))
	start(t, p)
	awaitHydrated(t, p)

	// Revision zero is "no wait", so a caller with nothing to wait for
	// never pays the budget.
	if err := p.WaitApplied(t.Context(), 0); err != nil {
		t.Errorf("waiting for revision 0 blocked: %v", err)
	}

	rev := put(t, docs, projection.FamilyWork, "i.mine", "just filed")
	if err := p.WaitApplied(t.Context(), rev); err != nil {
		t.Fatalf("the projector never applied its own write: %v", err)
	}

	// A revision nothing will ever produce times out with the sentinel a
	// REST handler renders as applied: false.
	ctx, cancel := context.WithTimeout(t.Context(), 150*time.Millisecond)
	defer cancel()
	err := p.WaitApplied(ctx, rev+10_000)
	if !errors.Is(err, projection.ErrRevisionTooNew) {
		t.Errorf("waiting past the head gave %v, want ErrRevisionTooNew", err)
	}
}

// THE VECTOR FAMILY IS NOT PROJECTED. It is written by the indexer against
// its own source versions and read by the searcher, so a projector following
// it would burn a watch on records it has no apply rule for.
func TestOnlyTheTwoDocumentFamiliesAreProjected(t *testing.T) {
	t.Parallel()
	got := projection.Projected()
	if !slices.Equal(got, []projection.Family{coord.FamilyWork, coord.FamilyPages}) {
		t.Errorf("Projected() = %v", got)
	}
	if slices.Contains(got, coord.FamilyKBVectors) {
		t.Error("the derived vector family is being projected as a change feed")
	}
}

// A projector refuses what it cannot serve rather than starting and failing
// later, which on this path means a node claiming seats it cannot answer for.
func TestAProjectorRefusesAnIncompleteWiring(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	a := newRowApplier(t, db, projection.FamilyWork)
	for _, tc := range []struct {
		name string
		opts projection.Options
	}{
		{"no documents", projection.Options{DB: db, Applier: a}},
		{"no store", projection.Options{Documents: memory.NewFleet(), Applier: a}},
		{"no applier", projection.Options{Documents: memory.NewFleet(), DB: db}},
		{"an unknown family", projection.Options{
			Documents: memory.NewFleet(), DB: db,
			Applier: &rowApplier{family: "ledger"},
		}},
	} {
		if _, err := projection.New(tc.opts); err == nil {
			t.Errorf("%s was accepted", tc.name)
		}
	}
}

// waitFor polls until want holds, or fails with why.
func waitFor(t *testing.T, want func() bool, why string) {
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

// THE POST-COMMIT HOOK FIRES ONLY FOR A BATCH THAT LANDED.
//
// It is what tells the tool-skill registry to re-read its container, and the
// registry's replace is WHOLESALE — so a hook fired for rows that rolled back
// would have it re-read a page that was never applied, and on a rebuild that
// is the difference between the company's skills and none of them.
func TestTheCommitHookFiresOnlyForABatchThatLanded(t *testing.T) {
	t.Parallel()
	docs := memory.NewFleet()
	db := openStore(t)
	applier := newRowApplier(t, db, projection.FamilyWork)
	p := newProjector(t, docs, db, applier)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() { _ = p.Run(ctx) }()
	waitFor(t, p.Hydrated, "the projector never hydrated")

	put(t, docs, projection.FamilyWork, "i.one", `{"n":1}`)
	waitFor(t, func() bool { return applier.committed() > 0 },
		"a committed batch fired no post-commit hook")
	landed := applier.committed()

	// A batch that FAILS must fire nothing. The projector re-enters
	// through its reconcile after a failure, so the count is allowed to
	// grow again once the applier is healthy — what must not happen is a
	// hook for the batch that broke.
	applier.breakWith(errors.New("apply refused"))
	put(t, docs, projection.FamilyWork, "i.two", `{"n":2}`)
	time.Sleep(200 * time.Millisecond)
	if got := applier.committed(); got > landed {
		t.Errorf("a failed batch fired %d post-commit hooks", got-landed)
	}
}
