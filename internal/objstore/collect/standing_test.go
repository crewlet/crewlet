package collect

import (
	"context"
	"database/sql"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
)

// liveRefs and retiredRefs are one domain's two declared tables — what a
// domain that retires the objects it replaces declares (ADR-0033) — in a
// database of the test's own, read through [Sources] exactly as the engine
// builds the collector's: the fake source the other cases use stands in for
// the declarations, and so cannot say what a declaration's standing does.
var (
	liveRefs = objstore.ReferenceTable{Domain: "files", Table: "live_refs", Key: "object",
		Hash: "hash", Size: "size", Owner: []string{"owner"}, Standing: objstore.Required}
	retiredRefs = objstore.ReferenceTable{Domain: "files", Table: "retired_refs", Key: "object",
		Hash: "hash", Size: "size", Owner: []string{"owner"}, Standing: objstore.Retired}
)

// sqlEstate is that domain: always current, always complete.
type sqlEstate struct{ db *store.DB }

func (sqlEstate) Name() string { return "files" }

func (sqlEstate) Barrier(context.Context) (statelog.Position, error) {
	return statelog.Position{}, nil
}

func (e sqlEstate) Read(ctx context.Context, _ statelog.Position, fn func(*sql.Tx) error) (bool, error) {
	return true, e.db.Read(ctx, fn)
}

// standingHarness is [harness] with its collector built over liveRefs and
// retiredRefs rather than the fake source.
type standingHarness struct {
	*harness
	db *store.DB
}

func newStandingHarness(t *testing.T) *standingHarness {
	t.Helper()
	h := newHarness(t)
	db := storetest.OpenNode(t, filepath.Join(t.TempDir(), "node.db"), store.Options{})
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Tx(t.Context(), func(tx *sql.Tx) error {
		for _, ref := range []objstore.ReferenceTable{liveRefs, retiredRefs} {
			for _, stmt := range []string{
				`CREATE TABLE ` + ref.Table + ` (object TEXT, hash TEXT NOT NULL,
					size INTEGER NOT NULL, owner TEXT NOT NULL)`,
				`CREATE INDEX ` + ref.Table + `_object_idx ON ` + ref.Table + ` (object)`,
			} {
				if _, err := tx.ExecContext(t.Context(), stmt); err != nil {
					return err
				}
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	refs, err := Sources([]objstore.ReferenceTable{liveRefs, retiredRefs}, sqlEstate{db: db})
	if err != nil {
		t.Fatal(err)
	}
	if h.c, err = New(Options{Store: h.store, References: refs, Now: h.clock.Now}); err != nil {
		t.Fatal(err)
	}
	return &standingHarness{harness: h, db: db}
}

// name writes a row of ref naming o, owned by owner.
func (h *standingHarness) name(ref objstore.ReferenceTable, o objstore.Object, owner string) {
	h.t.Helper()
	if err := h.db.Tx(h.t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(h.t.Context(), `INSERT INTO `+ref.Table+
			` (object, hash, size, owner) VALUES (?, ?, ?, ?)`,
			o.Key.String(), string(o.Hash), o.Size, owner)
		return err
	}); err != nil {
		h.t.Fatal(err)
	}
}

// unname deletes ref's row naming k — for a retired row, its domain sweeping
// it once its grace has passed.
func (h *standingHarness) unname(ref objstore.ReferenceTable, k objstore.Key) {
	h.t.Helper()
	if err := h.db.Tx(h.t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(h.t.Context(), `DELETE FROM `+ref.Table+` WHERE object = ?`, k.String())
		return err
	}); err != nil {
		h.t.Fatal(err)
	}
}

// A RETIRED REFERENCE KEEPS ITS OBJECT FROM COLLECTION, for as long as its row
// stands, and no longer (ADR-0033). The collector's grace dates an object's
// UPLOAD, so a replaced object older than a day would go at the first pass
// after its live row stopped naming it; a retired row is what keeps it for a
// reader that has not caught up — and once its domain sweeps the row, the
// object goes at the next pass like any other nothing names.
func TestARetiredReferenceKeepsItsObjectFromCollection(t *testing.T) {
	t.Parallel()
	h := newStandingHarness(t)
	live := h.putObject("a live file's bytes")
	h.name(liveRefs, live, "live.md")
	replaced := h.putObject("the bytes a rewrite replaced")
	h.name(retiredRefs, replaced, "replaced.md")
	orphan := h.put("an upload whose record never came")

	h.clock.advance(PendingGrace + time.Hour)
	r, err := h.c.Collect(t.Context())
	if err != nil || !r.Completed || r.Aged != 3 || r.Deleted != 1 || r.Referenced != 2 {
		t.Fatalf("a pass past the grace = %+v, %v; want the orphan deleted and the "+
			"live and the retired objects both kept", r, err)
	}
	if h.held(orphan) {
		t.Fatal("an object nothing names was kept")
	}
	if !h.held(live.Key) || !h.held(replaced.Key) {
		t.Fatalf("a named object was deleted: live held %v, retired held %v",
			h.held(live.Key), h.held(replaced.Key))
	}

	h.unname(retiredRefs, replaced.Key)
	h.clock.advance(CollectInterval)
	if r, err = h.c.Collect(t.Context()); err != nil || !r.Completed || r.Deleted != 1 {
		t.Fatalf("the first pass after the retired row was swept = %+v, %v; want its object deleted", r, err)
	}
	if h.held(replaced.Key) {
		t.Fatal("an object kept only by a swept retired row outlived it")
	}
	if !h.held(live.Key) {
		t.Fatal("the live file's object was deleted")
	}
}

// THE AUDIT ASKS ONLY AFTER REQUIRED REFERENCES (ADR-0033). An object a
// retired row keeps is one the company has let go of: a store that lost it,
// or holds it wrong, has lost nothing anybody can miss, so it is never asked
// after, never counted and never raises objects_missing — while a required
// table's lost object still is.
func TestTheAuditAsksOnlyAfterRequiredReferences(t *testing.T) {
	t.Parallel()
	h := newStandingHarness(t)
	h.name(liveRefs, h.putObject("kept"), "kept.md")
	lost := h.putObject("lost")
	h.name(liveRefs, lost, "lost.md")
	gone := h.putObject("a replaced object the store lost")
	h.name(retiredRefs, gone, "gone.md")
	bent := h.putObject("a replaced object the store holds wrong")
	h.name(retiredRefs, bent, "bent.md")
	for _, k := range []objstore.Key{lost.Key, gone.Key} {
		if err := h.store.Delete(t.Context(), k); err != nil {
			t.Fatal(err)
		}
	}
	h.backend.Corrupt(bent.Key.Name(), []byte("other length"))

	r, err := h.c.Audit(t.Context())
	if err != nil || !r.Completed || r.Referenced != 2 || r.Missing != 1 || r.Damaged != 0 {
		t.Fatalf("Audit = %+v, %v; want the two required references asked after and "+
			"one of them missing, and neither retired one asked after", r, err)
	}
	want := []MissingFile{{Object: lost.Key, NamedBy: "lost.md"}}
	if r.Found == nil || !slices.Equal(r.Found.MissingFiles, want) {
		t.Fatalf("the findings = %+v, want %+v", r.Found, want)
	}
}
