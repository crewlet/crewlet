package engine

import (
	"testing"

	"github.com/crewlet/crewlet/internal/coord"
)

// EVERY LOG'S HEARTBEAT SAYS WHICH RECORDS THIS BUILD READS ON IT, and the
// positions a writer reads carry it through.
//
// Deferral rests on a peer decoding a record's ENVELOPE; a kind an older
// build's envelope refuses stops that build instead. The heartbeat is the only
// place a node says which records it reads, and [reportedPositions] the only
// road from the register to the counted set a writer asks — so a heartbeat
// that leaves it out, or a read that drops it, is a fleet where every node
// reads as a build that predates the question and a new kind is never
// published.
func TestEveryHeartbeatAdvertisesTheRecordsItsBuildReads(t *testing.T) {
	t.Parallel()
	e, _ := aRunningNode(t)
	s := e.native.Load().log
	s.publishPositions(t.Context())

	rows, err := e.backends.Fleet.Positions(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var mine *coord.NodePositions
	for i := range rows {
		if rows[i].NodeID == e.id {
			mine = &rows[i]
		}
	}
	if mine == nil {
		t.Fatalf("this node published no row: %+v", rows)
	}
	for _, name := range s.order {
		want := s.domains[name].domain.RecordVersion()
		if got := mine.Domains[name].RecordVersion; got != want {
			t.Fatalf("the heartbeat advertises reading version %d of %s, and this "+
				"build reads %d", got, name, want)
		}
		var read bool
		for _, at := range reportedPositions(rows, name) {
			if at.NodeID != e.id {
				continue
			}
			read = true
			if at.RecordVersion != want {
				t.Fatalf("the %s log's positions read this node as reading "+
					"version %d, and its heartbeat said %d", name,
					at.RecordVersion, want)
			}
		}
		if !read {
			t.Fatalf("the %s log's positions do not count this node", name)
		}
	}
}
