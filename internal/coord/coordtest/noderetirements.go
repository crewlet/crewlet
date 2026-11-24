package coordtest

import (
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/iam"
)

// retirementCases hold every backend to a node's retirement as the capacity
// window reads it: every node reads it back with who asserted it and the
// instant the store took it, by the clock that stamps a node's row, so a row
// written before it stays retired; a second assertion replaces the first; and
// an id no node could run under is refused before anything is written.
var retirementCases = []fleetCase{
	{"a retirement is read back with its author and the store's instant, a later one replacing it", func(h *fleetHarness) {
		if err := h.f.PutPositions(h.ctx, coord.NodePositions{NodeID: "node-9",
			Domains: map[string]coord.DomainPosition{}}); err != nil {
			h.t.Fatal(err)
		}
		for _, by := range []string{"ana.admin", "ben.admin"} {
			if err := h.f.RetireNode(h.ctx, coord.NodeRetirement{NodeID: "node-9", By: by,
				ByKind: iam.ActorOperator, OperatorID: "session:" + by, At: time.Now().UTC()}); err != nil {
				h.t.Fatalf("retire node-9 as %s: %v", by, err)
			}
		}
		rows, err := h.node().Positions(h.ctx)
		if err != nil || len(rows) != 1 {
			h.t.Fatalf("the positions read %v, %v", rows, err)
		}
		got, err := h.node().NodeRetirements(h.ctx)
		if err != nil || len(got) != 1 || got[0].NodeID != "node-9" || got[0].By != "ben.admin" ||
			got[0].OperatorID != "session:ben.admin" {
			h.t.Fatalf("the retirements read %+v, %v, want node-9's by ben.admin alone", got, err)
		}
		if !got[0].Stands(rows[0].StoredAt) {
			h.t.Errorf("node-9's retirement, stored at %v, does not stand against the row it wrote "+
				"before, stored at %v", got[0].StoredAt, rows[0].StoredAt)
		}
		if err := h.f.RetireNode(h.ctx, coord.NodeRetirement{By: "ana.admin", ByKind: iam.ActorOperator,
			At: time.Now().UTC()}); err == nil {
			h.t.Error("a retirement naming no node was written")
		}
	}},
}
