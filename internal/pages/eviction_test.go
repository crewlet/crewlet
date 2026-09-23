package pages_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/statelog"
)

// AN EVICTION IS WRITTEN TO THIS LOG BY ITS OWN STORE, AND READ BACK FROM THIS
// LOG'S OWN ROWS — install, lift, and install again.
//
// For as long as only the tracker's writer could publish an eviction, this
// domain had an applier, a fence and a table for one and no way to write it:
// nothing in production ever filled `pages_evictions`, the pages applier never
// dropped an evicted node's records, and the trim — reading the tracker's
// table on this log's behalf — never stopped counting the node here. The store
// writes it now, and [pages.Domain.Evictions] is what the trim reads, so what
// that answers after each step is the contract.
func TestAnEvictionIsWrittenToThisLogAndReadBackFromIt(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	operator := pages.Actor{Handle: "founder", Kind: pages.AuthorOperator,
		OperatorID: "founder"}
	standing := func(node string) (row statelog.EvictionRow, held bool) {
		t.Helper()
		rows, err := pages.Domain{}.Evictions(t.Context(), r.db)
		if err != nil {
			t.Fatalf("read the evictions: %v", err)
		}
		for _, row := range rows {
			if row.NodeID == node {
				return row, true
			}
		}
		return statelog.EvictionRow{}, false
	}
	// AND THE SAME STANDING READ OFF THE LOG ITSELF ([statelog.EvictedOnLog]),
	// which is how a node a peer re-anchored past sees an eviction its
	// stopped applier never reaches.
	onLog := func(want bool) {
		t.Helper()
		evicted, found, err := statelog.EvictedOnLog(t.Context(), pages.Domain{}, r.log, "node-b")
		if err != nil || !found || evicted != want {
			t.Fatalf("node-b's standing read off the log = evicted %v, found %v (%v), "+
				"want evicted %v", evicted, found, err, want)
		}
	}

	res, err := r.store.EvictNode(t.Context(), operator, "op-evict", "node-b")
	if err != nil {
		t.Fatalf("EvictNode: %v", err)
	}
	if res.Position.Seq == 0 || res.OpID != "op-evict" {
		t.Fatalf("the eviction answered %+v — a gate write reports where it "+
			"landed and under which operation, or a partial gesture could not "+
			"be retried", res)
	}
	r.drain()
	row, held := standing("node-b")
	switch {
	case !held:
		t.Fatal("the eviction landed and this log's rows hold no row for the node")
	case row.Back:
		t.Fatalf("node-b reads as back straight after its eviction: %+v", row)
	case row.From != uint64(res.Position.Packed()):
		t.Fatalf("the eviction takes effect above %d, want its own position %d — "+
			"the gate drops records by comparing against exactly this",
			row.From, res.Position.Packed())
	case row.By != operator.Name():
		t.Fatalf("the eviction was run by %q, want %q", row.By, operator.Name())
	case row.At.IsZero():
		t.Fatal("the eviction carries no instant, and the fence window is " +
			"measured from it")
	}

	onLog(true)
	back, err := r.store.ReadmitNode(t.Context(), operator, "op-back", "node-b")
	if err != nil {
		t.Fatalf("ReadmitNode: %v", err)
	}
	r.drain()
	if row, held = standing("node-b"); !held || !row.Back ||
		row.Readmitted != uint64(back.Position.Packed()) {
		t.Fatalf("node-b after its readmission reads %+v (held %v), want its row "+
			"kept and back at %d — a readmission is an inverse commit, not a "+
			"delete", row, held, back.Position.Packed())
	}

	onLog(false)
	if _, err := r.store.EvictNode(t.Context(), operator, "op-again", "node-b"); err != nil {
		t.Fatalf("EvictNode again: %v", err)
	}
	r.drain()
	if row, held = standing("node-b"); !held || row.Back {
		t.Fatalf("node-b evicted a second time reads %+v (held %v) — a "+
			"re-eviction clears the readmission", row, held)
	}
	onLog(true)
}

// A GATE WRITE REFUSES WHAT IT CANNOT MEAN, before it forms a record.
func TestAnEvictionNamingNothingIsRefused(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	operator := pages.Actor{Kind: pages.AuthorOperator, OperatorID: "founder"}
	if _, err := r.store.EvictNode(t.Context(), operator, "op", ""); err == nil {
		t.Fatal("an eviction naming no node was written")
	}
	if _, err := r.store.EvictNode(t.Context(), operator, "", "node-b"); err == nil {
		t.Fatal("an eviction with no operation id was written — a retry of it " +
			"could never be told from a second eviction")
	}
	if last, err := r.log.End(t.Context()); err != nil || last != 0 {
		t.Fatalf("the log's end is %d (err %v) after two refused gate writes", last, err)
	}
}
