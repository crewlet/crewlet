package coordtest

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/crewlet/crewlet/internal/coord"
)

// ---- insight records ----------------------------------------------------- //

func insightRecord(class string, status coord.InsightStatus) coord.InsightRecord {
	return coord.InsightRecord{Class: class, Week: "2030-W02", Status: status, Day: "2030-01-07"}
}

func openInsight(coord.InsightRecord) bool { return true }
func shutInsight(coord.InsightRecord) bool { return false }

var insightRecordCases = []fleetCase{{
	// THE PROMISE THE RECORD EXISTS FOR: every node that evaluates one class's
	// week asks at once, and exactly one of them moves it.
	name: "of any number of claims on one class's week, one wins",
	fn: func(h *fleetHarness) {
		var wins atomic.Int32
		var wg sync.WaitGroup
		for range 8 {
			wg.Go(func() {
				_, won, err := h.f.ClaimInsightRecord(h.ctx, insightRecord("wake.noop_share", coord.InsightObserved), shutInsight)
				if err != nil {
					h.t.Errorf("ClaimInsightRecord: %v", err)
				}
				if won {
					wins.Add(1)
				}
			})
		}
		wg.Wait()
		if wins.Load() != 1 {
			h.t.Errorf("%d claims won, want exactly one", wins.Load())
		}
	},
}, {
	// A MOVE HOLDS ONLY OVER THE VERSION IT READ, so a node that read a stale
	// record moves nothing, and a delete takes only the record it read.
	name: "a claim and a delete hold only at the version read",
	fn: func(h *fleetHarness) {
		first, won, err := h.f.ClaimInsightRecord(h.ctx, insightRecord("wake.noop_share", coord.InsightObserved), openInsight)
		if err != nil || !won || first.Version == 0 {
			h.t.Fatalf("first claim = %+v, won %v, %v", first, won, err)
		}
		at := func(v uint64) func(coord.InsightRecord) bool {
			return func(held coord.InsightRecord) bool { return held.Version == v }
		}
		queued, won, err := h.f.ClaimInsightRecord(h.ctx, insightRecord("wake.noop_share", coord.InsightQueued), at(first.Version))
		if err != nil || !won || queued.Status != coord.InsightQueued {
			h.t.Fatalf("a claim at the version read = %+v, won %v, %v", queued, won, err)
		}
		if held, won, err := h.f.ClaimInsightRecord(h.ctx, insightRecord("wake.noop_share", coord.InsightSent),
			at(first.Version)); err != nil || won || held.Status != coord.InsightQueued {
			h.t.Errorf("a claim at a stale version = %+v, won %v, %v; want the queued record", held, won, err)
		}
		if gone, err := h.f.DeleteInsightRecord(h.ctx, first); err != nil || gone {
			h.t.Errorf("a delete at a stale version = %v, %v; want it refused", gone, err)
		}
		if gone, err := h.f.DeleteInsightRecord(h.ctx, queued); err != nil || !gone {
			h.t.Errorf("a delete at the version read = %v, %v; want it taken", gone, err)
		}
		if _, found, err := h.f.InsightRecord(h.ctx, "wake.noop_share", "2030-W02"); err != nil || found {
			h.t.Errorf("a deleted record still reads (found %v, %v)", found, err)
		}
	},
}, {
	name: "every class's week is its own record, listed",
	fn: func(h *fleetHarness) {
		for _, class := range []string{"wake.noop_share", "review.idle"} {
			if _, _, err := h.f.ClaimInsightRecord(h.ctx, insightRecord(class, coord.InsightObserved), openInsight); err != nil {
				h.t.Fatalf("claim %s: %v", class, err)
			}
		}
		all, err := h.f.ListInsightRecords(h.ctx)
		if err != nil || len(all) != 2 {
			h.t.Fatalf("ListInsightRecords = %+v, %v; want two records", all, err)
		}
		read, found, err := h.f.InsightRecord(h.ctx, "review.idle", "2030-W02")
		if err != nil || !found || read.Class != "review.idle" || read.Week != "2030-W02" {
			h.t.Errorf("InsightRecord = %+v (found %v, %v)", read, found, err)
		}
	},
}, {
	// THE SHAPES ARE THE GUARD: no sentence reaches the fleet's store through a
	// field that was meant to hold an id.
	name: "an insight record that is not the promised shape is refused and leaves nothing",
	fn: func(h *fleetHarness) {
		mutate := func(change func(*coord.InsightRecord)) coord.InsightRecord {
			r := insightRecord("wake.noop_share", coord.InsightObserved)
			change(&r)
			return r
		}
		for name, bad := range map[string]coord.InsightRecord{
			"words as a class":  mutate(func(r *coord.InsightRecord) { r.Class = "Acme payroll" }),
			"a day as a week":   mutate(func(r *coord.InsightRecord) { r.Week = "2030-01-07" }),
			"an unknown status": mutate(func(r *coord.InsightRecord) { r.Status = "gone" }),
			"words as a day":    mutate(func(r *coord.InsightRecord) { r.Day = "Monday" }),
			"a long payload": mutate(func(r *coord.InsightRecord) {
				r.Payload = []byte(strings.Repeat("x", coord.MaxInsightPayloadBytes+1))
			}),
		} {
			if _, _, err := h.f.ClaimInsightRecord(h.ctx, bad, openInsight); err == nil {
				h.t.Errorf("ClaimInsightRecord with %s was accepted", name)
			}
		}
		if all, _ := h.f.ListInsightRecords(context.WithoutCancel(h.ctx)); len(all) != 0 {
			h.t.Errorf("a refused record left %+v behind", all)
		}
	},
}}
