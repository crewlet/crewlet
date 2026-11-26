package coordtest

import (
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/iam"
)

// captureStampCases hold every backend to a node's capture stamp record as the
// media gate reads it: every node reads it back with the instant the store
// took it, by the clock a retirement is stored by, so a record written before
// a retirement stays retired; a node's later write replaces its earlier one;
// a record is filed apart from the rows and retirements beside it; and a
// record naming no node or a version below zero is refused before anything
// is written.
var captureStampCases = []fleetCase{
	{"a record is read back at the store's instant, a later one replacing it", func(h *fleetHarness) {
		for _, version := range []int{0, 1} {
			if err := h.f.PutCaptureStamp(h.ctx, coord.CaptureStampRecord{NodeID: "node-9",
				CaptureStampVersion: version}); err != nil {
				h.t.Fatalf("record node-9 at %d: %v", version, err)
			}
		}
		if err := h.f.RetireNode(h.ctx, coord.NodeRetirement{NodeID: "node-9", By: "ana.admin",
			ByKind: iam.ActorOperator, At: time.Now().UTC()}); err != nil {
			h.t.Fatal(err)
		}
		got, err := h.node().CaptureStamps(h.ctx)
		if err != nil || len(got) != 1 || got[0].NodeID != "node-9" || got[0].CaptureStampVersion != 1 ||
			got[0].At.IsZero() || got[0].StoredAt.IsZero() {
			h.t.Fatalf("the records read %+v, %v, want node-9's at 1 alone, its instants filled", got, err)
		}
		retired, err := h.node().NodeRetirements(h.ctx)
		if err != nil || len(retired) != 1 {
			h.t.Fatalf("the retirements read %+v, %v", retired, err)
		}
		if !retired[0].Stands(got[0].StoredAt) {
			h.t.Errorf("node-9's retirement, stored at %v, does not stand against the record it wrote "+
				"before, stored at %v", retired[0].StoredAt, got[0].StoredAt)
		}
	}},
	{"a record is filed apart from the rows and retirements beside it", func(h *fleetHarness) {
		if err := h.f.PutPositions(h.ctx, coord.NodePositions{NodeID: "node-1",
			Domains: map[string]coord.DomainPosition{}}); err != nil {
			h.t.Fatal(err)
		}
		if err := h.f.RetireNode(h.ctx, coord.NodeRetirement{NodeID: "node-2", By: "ana.admin",
			ByKind: iam.ActorOperator, At: time.Now().UTC()}); err != nil {
			h.t.Fatal(err)
		}
		if err := h.f.PutCaptureStamp(h.ctx, coord.CaptureStampRecord{NodeID: "node-3",
			CaptureStampVersion: 1}); err != nil {
			h.t.Fatal(err)
		}
		peer := h.node()
		stamps, err := peer.CaptureStamps(h.ctx)
		if err != nil || len(stamps) != 1 || stamps[0].NodeID != "node-3" {
			h.t.Errorf("the records read %+v, %v, want node-3's alone", stamps, err)
		}
		rows, err := peer.Positions(h.ctx)
		if err != nil || !slices.EqualFunc(rows, []string{"node-1"},
			func(r coord.NodePositions, id string) bool { return r.NodeID == id }) {
			h.t.Errorf("the positions read %+v, %v, want node-1's row alone", rows, err)
		}
	}},
	{"a record naming no node or a version below zero is refused", func(h *fleetHarness) {
		for _, bad := range []coord.CaptureStampRecord{{CaptureStampVersion: 1},
			{NodeID: "node-4", CaptureStampVersion: -1}} {
			if err := h.f.PutCaptureStamp(h.ctx, bad); err == nil {
				h.t.Errorf("the record %+v was written", bad)
			}
		}
		if got, err := h.node().CaptureStamps(h.ctx); err != nil || len(got) != 0 {
			h.t.Errorf("the records read %+v, %v after two refusals, want none", got, err)
		}
	}},
}
