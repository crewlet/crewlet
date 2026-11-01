package collect

import (
	"context"
	"database/sql"
	"maps"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
)

// liveRefs, retiredRefs and derivedRefs are one domain's declared tables, one
// of each standing — what a domain that retires the objects it replaces, and
// keeps objects the engine can build again, declares (ADR-0074) — in a
// database of the test's own, read through [Sources] exactly as the engine
// builds the collector's: the fake source the other cases use stands in for
// the declarations, and so cannot say what a declaration's standing does.
var (
	liveRefs = objstore.ReferenceTable{Domain: "files", Table: "live_refs", Key: "object",
		Hash: "hash", Size: "size", Owner: []string{"owner"}, Standing: objstore.Required}
	retiredRefs = objstore.ReferenceTable{Domain: "files", Table: "retired_refs", Key: "object",
		Hash: "hash", Size: "size", Owner: []string{"owner"}, Standing: objstore.Retired}
	derivedRefs = objstore.ReferenceTable{Domain: "files", Table: "derived_refs", Key: "object",
		Hash: "hash", Size: "size", Owner: []string{"owner"}, Standing: objstore.Derived}
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

// standingHarness is [harness] with its collector built over liveRefs,
// retiredRefs and derivedRefs rather than the fake source.
type standingHarness struct {
	*harness
	db *store.DB
}

// newStandingHarness is the standing tables over [newHarness]'s store, which
// keeps no order, so the audit reads its listing a window at a time.
func newStandingHarness(t *testing.T) *standingHarness {
	t.Helper()
	return standingOver(t, newHarness(t))
}

// standingOver is h with its collector rebuilt over the standing tables, on
// h's own store — a [newRangedHarness]'s, for a case auditing a store that
// lists ranges in order.
func standingOver(t *testing.T, h *harness) *standingHarness {
	t.Helper()
	declared := []objstore.ReferenceTable{liveRefs, retiredRefs, derivedRefs}
	db := storetest.OpenNode(t, filepath.Join(t.TempDir(), "node.db"), store.Options{})
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Tx(t.Context(), func(tx *sql.Tx) error {
		for _, ref := range declared {
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
	refs, err := Sources(declared, sqlEstate{db: db})
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
// it once its grace has passed; for a derived one, its domain replacing what
// the row named.
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
// stands, and no longer (ADR-0074). The collector's grace dates an object's
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

// THE AUDIT ASKS ONLY AFTER REQUIRED REFERENCES (ADR-0074). An object a
// retired row keeps is one the company has let go of, and one a derived row
// names is one the engine builds again: a store that lost either, or holds it
// wrong, has lost nothing anybody can miss, so it is never asked after, never
// counted and never raises objects_missing — while a required table's lost
// object still is.
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
	rebuilt := h.putObject("an index the store lost")
	h.name(derivedRefs, rebuilt, "rebuilt")
	warped := h.putObject("an index the store holds wrong")
	h.name(derivedRefs, warped, "warped")
	for _, k := range []objstore.Key{lost.Key, gone.Key, rebuilt.Key} {
		if err := h.store.Delete(t.Context(), k); err != nil {
			t.Fatal(err)
		}
	}
	h.backend.Corrupt(bent.Key.Name(), []byte("other length"))
	h.backend.Corrupt(warped.Key.Name(), []byte("another length"))

	r, err := h.c.Audit(t.Context())
	if err != nil || !r.Completed || r.Referenced != 2 || r.Missing != 1 || r.Damaged != 0 {
		t.Fatalf("Audit = %+v, %v; want the two required references asked after and "+
			"one of them missing, and no retired or derived one asked after", r, err)
	}
	want := []MissingFile{{Object: lost.Key, NamedBy: "lost.md", Finding: FindingMissing}}
	if r.Found == nil || !slices.Equal(r.Found.MissingFiles, want) {
		t.Fatalf("the findings = %+v, want %+v", r.Found, want)
	}
}

// A STORE MOVE WALKS THE REQUIRED AND THE DERIVED TABLES, AND NEVER A RETIRED
// ONE (ADR-0074), while the audit walks the Required tables alone: a derived
// object left behind by a move is one every node of the moved fleet builds
// again, and a retired one is one no reader of the moved fleet is left to
// want. Every reference a move's walk hands on carries its table's standing,
// which is how the move tells an object it owes the target from one it copies
// where the source has it.
func TestAStoreMoveWalksTheRequiredAndDerivedTablesAlone(t *testing.T) {
	t.Parallel()
	h := newStandingHarness(t)
	live := h.putObject("a file's bytes")
	h.name(liveRefs, live, "file.md")
	replaced := h.putObject("the bytes a rewrite replaced")
	h.name(retiredRefs, replaced, "replaced.md")
	index := h.putObject("an index the engine builds again")
	h.name(derivedRefs, index, "index")

	walked := func(walks []objstore.Walk) map[objstore.Key]objstore.Standing {
		t.Helper()
		out := map[objstore.Key]objstore.Standing{}
		for _, w := range walks {
			for {
				page, whole, err := w.Next(t.Context())
				if err != nil || !whole {
					t.Fatalf("a walk's page = %v (whole %v)", err, whole)
				}
				if len(page) == 0 {
					break
				}
				for _, ref := range page {
					out[ref.Object.Key] = ref.Standing
				}
			}
		}
		return out
	}
	src := h.c.opts.References[0]
	if got, want := walked(src.Moved(statelog.Position{})), map[objstore.Key]objstore.Standing{
		live.Key: objstore.Required, index.Key: objstore.Derived,
	}; !maps.Equal(got, want) {
		t.Errorf("a store move walked %v; want the live object and the derived one, each with "+
			"its table's standing", got)
	}
	if got, want := walked(src.Required(statelog.Position{})), map[objstore.Key]objstore.Standing{
		live.Key: objstore.Required,
	}; !maps.Equal(got, want) {
		t.Errorf("the audit walked %v; want the live object alone", got)
	}
}

// A DERIVED REFERENCE IS KEPT, AND NEVER AUDITED (ADR-0074). A derived row
// names an object the engine can build again from what live rows name, so its
// object is kept from collection for as long as the row stands, exactly as a
// live row's is, and goes at the next pass once the row does, with no grace of
// its own. But the audit never asks after it: a store that lost it has lost a
// rebuild, not a file, and objects_missing would page somebody about a loss
// nobody has to repair. In BOTH of the audit's modes, since each merges the
// same walks: a store that keeps no order, whose whole listing is read a
// window at a time (`nats`), and one that lists ranges in order (S3).
func TestADerivedReferenceIsKeptAndNeverAudited(t *testing.T) {
	t.Parallel()
	for _, store := range []struct {
		name string
		// open is the case's harness, and how many listings of its audit's
		// mode the store has answered — whole listings, or ranges.
		open func(*testing.T) (*standingHarness, func() int)
	}{
		{"a store that keeps no order", func(t *testing.T) (*standingHarness, func() int) {
			h := newStandingHarness(t)
			return h, h.backend.listings
		}},
		{"a store that lists ranges in order", func(t *testing.T) (*standingHarness, func() int) {
			base, r := newRangedHarness(t)
			return standingOver(t, base), r.listings
		}},
	} {
		t.Run(store.name, func(t *testing.T) {
			t.Parallel()
			h, listings := store.open(t)
			live := h.putObject("a file's bytes")
			h.name(liveRefs, live, "file.md")
			kept := h.putObject("an index the engine builds again")
			h.name(derivedRefs, kept, "index")
			lost := h.putObject("an index the store has lost")
			h.name(derivedRefs, lost, "lost-index")

			// TWO DAYS ON every object is past the grace, so only a row
			// keeps it.
			h.clock.advance(2 * PendingGrace)
			r, err := h.c.Collect(t.Context())
			if err != nil || !r.Completed || r.Aged != 3 || r.Deleted != 0 || r.Referenced != 3 {
				t.Fatalf("a pass past the grace = %+v, %v; want the live object and both "+
					"derived ones kept", r, err)
			}

			// THE AUDIT ASKS AFTER THE LIVE ONE ALONE, and the derived object
			// the store has lost raises nothing.
			if err := h.store.Delete(t.Context(), lost.Key); err != nil {
				t.Fatal(err)
			}
			before := listings()
			a, err := h.c.Audit(t.Context())
			if err != nil || !a.Completed || a.Referenced != 1 || a.Missing != 0 ||
				a.Damaged != 0 || a.Unanswered != 0 {
				t.Fatalf("Audit = %+v, %v; want the one live reference asked after and found, "+
					"and neither derived one asked after", a, err)
			}
			if a.Found == nil || a.Found.Lost() != 0 || len(a.Found.MissingFiles) != 0 {
				t.Fatalf("the findings = %+v; want nothing lost", a.Found)
			}
			if listings() == before {
				t.Fatal("the audit made no listing of the kind this store answers, so this " +
					"arm did not audit the way its store is audited")
			}

			// ONCE ITS ROW GOES, its object goes at the next pass: a derived
			// row keeps its object for no grace.
			h.unname(derivedRefs, kept.Key)
			h.clock.advance(CollectInterval)
			if r, err = h.c.Collect(t.Context()); err != nil || !r.Completed || r.Deleted != 1 {
				t.Fatalf("the first pass after the derived row went = %+v, %v; want its "+
					"object deleted", r, err)
			}
			if h.held(kept.Key) {
				t.Fatal("an object only a removed derived row named outlived it")
			}
			if !h.held(live.Key) {
				t.Fatal("the live file's object was deleted")
			}
		})
	}
}
