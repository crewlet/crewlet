package pages_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/statelog"
)

// AN EVICTION ON THIS LOG DROPS THE NODE'S RECORDS HERE, AND A READMISSION
// TAKES IT BACK.
//
// The applier reads the gate from this domain's own table, so an eviction
// recorded on another log drops nothing here: until this log carried its own,
// an evicted node's knowledge-base writes went on applying on every peer, and
// the trim went on counting it against this log's floor. The row names who ran
// the gesture, because the trim's tombstone and the status report both do.
//
// Mutation: publish the readmission as another eviction and the node is never
// taken back; publish nothing from EvictNode and no row names the node.
func TestAnEvictionOnThisLogGatesTheNodeHere(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	ops := pages.Actor{Handle: "ops", Kind: pages.AuthorOperator, OperatorID: "ops"}

	evicted, err := r.store.EvictNode(t.Context(), ops, "op-evict", "node-b")
	if err != nil {
		t.Fatalf("EvictNode: %v", err)
	}
	r.drain()
	rows, err := pages.Evictions(t.Context(), r.db.Replicated())
	if err != nil {
		t.Fatalf("Evictions: %v", err)
	}
	if len(rows) != 1 || rows[0].NodeID != "node-b" || rows[0].IsBack ||
		rows[0].By != "ops" || rows[0].Stream != (pages.Domain{}).Stream().Name {
		t.Fatalf("the evictions on this log are %+v, want node-b evicted by ops", rows)
	}
	gates := pages.NewGates(r.db)
	above := statelog.Position{Stream: evicted.Position.Stream,
		Generation: evicted.Position.Generation, Seq: evicted.Position.Seq + 1}
	reason, gated, err := gates.GatedAt(t.Context(), statelog.Subject{
		Kind: string(pages.KindPage), ID: "p-1"}, "node-b", "op-late", above)
	if err != nil || !gated || reason != statelog.ReasonEvicted {
		t.Fatalf("a record node-b wrote above its eviction resolves (%s, %v, %v), "+
			"want it gated as evicted", reason, gated, err)
	}
	if fenced, err := pages.NewFence(r.db, "node-b").Evicted(t.Context()); err != nil || !fenced {
		t.Fatalf("node-b's own fence reads evicted=%v (%v), want true", fenced, err)
	}

	back, err := r.store.ReadmitNode(t.Context(), ops, "op-readmit", "node-b")
	if err != nil {
		t.Fatalf("ReadmitNode: %v", err)
	}
	r.drain()
	rows, err = pages.Evictions(t.Context(), r.db.Replicated())
	if err != nil {
		t.Fatalf("Evictions: %v", err)
	}
	if len(rows) != 1 || !rows[0].IsBack {
		t.Fatalf("after the readmission the evictions read %+v, want node-b back", rows)
	}
	later := statelog.Position{Stream: back.Position.Stream,
		Generation: back.Position.Generation, Seq: back.Position.Seq + 1}
	if _, gated, err := gates.GatedAt(t.Context(), statelog.Subject{
		Kind: string(pages.KindPage), ID: "p-1"}, "node-b", "op-later", later); err != nil || gated {
		t.Fatalf("a record node-b wrote after its readmission is gated=%v (%v)", gated, err)
	}
	if fenced, err := pages.NewFence(r.db, "node-b").Evicted(t.Context()); err != nil || fenced {
		t.Fatalf("node-b's own fence reads evicted=%v (%v) after its readmission",
			fenced, err)
	}
}
