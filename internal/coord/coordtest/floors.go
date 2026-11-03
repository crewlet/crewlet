package coordtest

import (
	"github.com/crewlet/crewlet/internal/coord"
)

// ---- the published trim floors ------------------------------------------ //

// putFloor publishes one domain's floor as a trim tick would.
func (h *fleetHarness) putFloor(domain string, generation uint32, floor uint64) coord.TrimFloor {
	h.t.Helper()
	row := coord.TrimFloor{
		Domain: domain, Generation: generation, TrimTo: floor, Floor: floor,
		Terms: []coord.TrimTerm{{Name: "applied", Seq: floor, Known: true}},
		At:    h.now(), By: "node-a",
	}
	if err := h.f.PutFloor(h.ctx, row); err != nil {
		h.t.Fatalf("PutFloor(%s): %v", domain, err)
	}
	return row
}

// floorOf reads one domain's floor, failing the case on a store that did not
// answer: every case here runs against a healthy one.
func (h *fleetHarness) floorOf(domain string) (coord.TrimFloor, bool) {
	h.t.Helper()
	row, found, err := h.f.Floor(h.ctx, domain)
	if err != nil {
		h.t.Fatalf("Floor(%s): %v", domain, err)
	}
	if !found && (row.Domain != "" || row.Floor != 0 || len(row.Terms) != 0) {
		h.t.Fatalf("Floor(%s) reported no floor beside a row %+v: an absent floor "+
			"that carries a value is read as one by any caller that checks the "+
			"value before the flag", domain, row)
	}
	return row, found
}

var floorCases = []fleetCase{{
	// THE WRITE FENCE ASKS ABOUT ONE DOMAIN, so a floor is read by the domain
	// that names it and answers for nobody else: a cursor on one log compared
	// against another log's floor compares two number spaces. The domain
	// whose name is the first one's with a segment added is the prefix
	// confusion a key read could only commit if the grammar stopped escaping
	// the separator.
	name: "a floor reads back under its own domain and no other",
	fn: func(h *fleetHarness) {
		want := h.putFloor("tracker", 3, 900)
		h.putFloor("tracker.archive", 1, 5)
		got, found := h.floorOf("tracker")
		if !found {
			h.t.Fatal("a published floor read back as never written: the write " +
				"fence clears every node against a log the trim has moved past")
		}
		if got.Domain != want.Domain || got.Generation != want.Generation ||
			got.TrimTo != want.TrimTo || got.Floor != want.Floor ||
			got.By != want.By || !got.At.Equal(want.At) ||
			len(got.Terms) != 1 || got.Terms[0] != want.Terms[0] {
			h.t.Fatalf("Floor(tracker) = %+v, want %+v", got, want)
		}
		if _, pages := h.floorOf("pages"); pages {
			h.t.Fatal("a domain the trim never wrote for read back a floor")
		}
		archive, archived := h.floorOf("tracker.archive")
		if !archived || archive.Floor != 5 || archive.Domain != "tracker.archive" {
			h.t.Fatalf("Floor(tracker.archive) = (%+v, %v), want its own row "+
				"at 5", archive, archived)
		}
	},
}, {
	// THE REGISTER IS SHARED: positions, holds and backup points are filed
	// in the same bucket, keyed by an id a node, an owner or a domain may
	// share a spelling with. A floor read that matched any of them would
	// hand the fence a row with an empty floor, which clears every cursor.
	name: "no other key class of the register reads as a floor",
	fn: func(h *fleetHarness) {
		if err := h.f.PutPositions(h.ctx, coord.NodePositions{
			NodeID: "tracker", At: h.now(),
			Domains: map[string]coord.DomainPosition{"tracker": {Generation: 1, Seq: 7}},
		}); err != nil {
			h.t.Fatalf("PutPositions: %v", err)
		}
		h.putHold("tracker")
		if err := h.f.PutBackupPoint(h.ctx, coord.BackupPoint{
			Owner: "tracker", At: h.now(), Verified: true,
			Streams: map[string]coord.Position{"LOG": {Stream: "LOG", Generation: 1, Seq: 5}},
		}); err != nil {
			h.t.Fatalf("PutBackupPoint: %v", err)
		}
		if row, found := h.floorOf("tracker"); found {
			h.t.Fatalf("a positions row, a hold and a backup point keyed "+
				"tracker read back as tracker's floor: %+v", row)
		}
	},
}, {
	// A LATER TICK REPLACES THE ROW WHOLESALE, and the read answers the
	// newest: the floor never moves down within a generation, so a read that
	// returned an older row would clear a node the newer one counts below
	// the log.
	name: "a floor read answers the newest row written",
	fn: func(h *fleetHarness) {
		h.putFloor("tracker", 1, 40)
		h.putFloor("tracker", 1, 90)
		if row, found := h.floorOf("tracker"); !found || row.Floor != 90 {
			h.t.Fatalf("Floor(tracker) = (%+v, %v) after the floor rose to 90",
				row, found)
		}
	},
}, {
	// WHAT A READ HANDS BACK IS THE CALLER'S: the alarm reading and the
	// report each hold the terms they read, and a twin that handed out its
	// own slice would let one caller rewrite what the next one reads — the
	// aliasing bug only an in-memory store can have.
	name: "a floor read hands back a copy",
	fn: func(h *fleetHarness) {
		h.putFloor("tracker", 1, 40)
		first, _ := h.floorOf("tracker")
		first.Terms[0].Name = "rewritten"
		again, found := h.floorOf("tracker")
		if !found || again.Terms[0].Name != "applied" {
			h.t.Fatalf("a caller's edit of the terms it read reached the store: %+v",
				again.Terms)
		}
	},
}}
