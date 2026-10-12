package code_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/statelog"
)

// AN EVICTION IS WRITTEN TO THIS LOG BY ITS OWN STORE, AND READ BACK FROM THIS
// LOG'S OWN ROWS — install, lift, and install again.
//
// The code log claims identity, so the trim counts its nodes on it, and an
// eviction the node gate wrote only to the other two identity logs would
// leave the node pinning this log's floor for ever. [code.Domain.Evictions]
// is what the trim reads, and [statelog.EvictedOnLog] what a node a peer
// re-anchored past reads, so what both answer after each step is the
// contract.
func TestAnEvictionIsWrittenToThisLogAndReadBackFromIt(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	operator := code.Actor{ID: "founder", Kind: code.ActorOperator, OperatorID: "founder"}
	standing := func(node string) (statelog.EvictionRow, bool) {
		t.Helper()
		rows, err := code.Domain{}.Evictions(t.Context(), n.db.Reader())
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
	onLog := func(want bool) {
		t.Helper()
		evicted, found, err := statelog.EvictedOnLog(t.Context(), code.Domain{},
			code.Domain{}.Stream(), n.log, "node-b")
		if err != nil || !found || evicted != want {
			t.Fatalf("node-b's standing read off the log = evicted %v, found %v (%v), "+
				"want evicted %v", evicted, found, err, want)
		}
	}

	res, err := n.store.EvictNode(t.Context(), operator, "op-evict", "node-b")
	if err != nil {
		t.Fatalf("EvictNode: %v", err)
	}
	if res.Position.Seq == 0 || res.OpID != "op-evict" {
		t.Fatalf("the eviction answered %+v — a gate write reports where it landed "+
			"and under which operation, or a partial gesture could not be retried", res)
	}
	n.drain()
	row, held := standing("node-b")
	switch {
	case !held:
		t.Fatal("the eviction landed and this log's rows hold no row for the node")
	case row.Back:
		t.Fatalf("node-b reads as back straight after its eviction: %+v", row)
	case row.From != uint64(res.Position.Packed()):
		t.Fatalf("the eviction takes effect above %d, want its own position %d",
			row.From, res.Position.Packed())
	case row.By != operator.ID:
		t.Fatalf("the eviction was run by %q, want %q", row.By, operator.ID)
	case row.At.IsZero():
		t.Fatal("the eviction carries no instant, and the fence window is measured from it")
	}
	onLog(true)

	back, err := n.store.ReadmitNode(t.Context(), operator, "op-back", "node-b")
	if err != nil {
		t.Fatalf("ReadmitNode: %v", err)
	}
	n.drain()
	if row, held = standing("node-b"); !held || !row.Back ||
		row.Readmitted != uint64(back.Position.Packed()) {
		t.Fatalf("node-b after its readmission reads %+v (held %v), want its row kept "+
			"and back at %d — a readmission is an inverse commit, not a delete",
			row, held, back.Position.Packed())
	}
	onLog(false)

	if _, err := n.store.EvictNode(t.Context(), operator, "op-again", "node-b"); err != nil {
		t.Fatalf("EvictNode again: %v", err)
	}
	n.drain()
	if row, held = standing("node-b"); !held || row.Back {
		t.Fatalf("node-b evicted a second time reads %+v (held %v) — a re-eviction "+
			"clears the readmission", row, held)
	}
	onLog(true)
}

// A GATE WRITE REFUSES WHAT IT CANNOT MEAN, before it forms a record.
func TestAnEvictionNamingNothingIsRefused(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	operator := code.Actor{ID: "founder", Kind: code.ActorOperator, OperatorID: "founder"}
	if _, err := n.store.EvictNode(t.Context(), operator, "op", ""); err == nil {
		t.Fatal("an eviction naming no node was written")
	}
	if _, err := n.store.EvictNode(t.Context(), operator, "", "node-b"); err == nil {
		t.Fatal("an eviction with no operation id was written — a retry of it could " +
			"never be told from a second eviction")
	}
	if _, err := n.store.EvictNode(t.Context(), code.Actor{ID: "founder", Kind: code.ActorOperator},
		"op", "node-b"); err == nil {
		t.Fatal("an operator's eviction naming no operator was written")
	}
	if last := n.end(); last != 0 {
		t.Fatalf("the log's end is %d after three refused gate writes", last)
	}
}
