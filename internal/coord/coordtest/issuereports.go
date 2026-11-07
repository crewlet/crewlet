package coordtest

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
)

// ---- issue reports ------------------------------------------------------- //

var issueReportFingerprint = strings.Repeat("ab", 32)

func (h *fleetHarness) issueReport(status coord.IssueReportStatus, at time.Time) coord.IssueReport {
	return coord.IssueReport{Fingerprint: issueReportFingerprint, Class: "engine.panicked",
		Status: status, FirstSent: at, ClaimedAt: at}
}

func closedToEverybody(coord.IssueReport) bool { return false }
func openToEverybody(coord.IssueReport) bool   { return true }

var issueReportCases = []fleetCase{{
	// THE PROMISE THE WHOLE RECORD EXISTS FOR: every node that detects one
	// defect asks at once, and exactly one of them sends.
	name: "of any number of claims on one fingerprint, one wins",
	fn: func(h *fleetHarness) {
		var wins atomic.Int32
		var wg sync.WaitGroup
		for range 8 {
			wg.Go(func() {
				_, won, err := h.f.ClaimIssueReport(h.ctx, h.issueReport(coord.IssueReportSending, h.now()), closedToEverybody)
				if err != nil {
					h.t.Errorf("ClaimIssueReport: %v", err)
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
	// A SHUT WINDOW HANDS BACK WHAT SHUT IT, and an open one replaces it, so
	// what decides the seven days is the caller's rule and not the store's.
	name: "a claim asks the held record and is handed it when refused",
	fn: func(h *fleetHarness) {
		first, won, err := h.f.ClaimIssueReport(h.ctx, h.issueReport(coord.IssueReportSending, h.now()), openToEverybody)
		if err != nil || !won || first.Version == 0 {
			h.t.Fatalf("first claim = %+v, won %v, %v", first, won, err)
		}
		held, won, err := h.f.ClaimIssueReport(h.ctx, h.issueReport(coord.IssueReportSending, h.now().Add(time.Hour)), closedToEverybody)
		if err != nil || won || held.Version != first.Version {
			h.t.Fatalf("refused claim = %+v, won %v, %v; want the first record", held, won, err)
		}
		next, won, err := h.f.ClaimIssueReport(h.ctx, h.issueReport(coord.IssueReportSending, h.now().Add(time.Hour)), openToEverybody)
		if err != nil || !won || next.Version == first.Version {
			h.t.Fatalf("admitted claim = %+v, won %v, %v; want a new version", next, won, err)
		}
	},
}, {
	// A NODE WHOSE LEASE RAN OUT SETTLES NOTHING over the node that took it
	// over, and its version is how it can tell.
	name: "a settle holds only at the version it read",
	fn: func(h *fleetHarness) {
		mine, _, err := h.f.ClaimIssueReport(h.ctx, h.issueReport(coord.IssueReportSending, h.now()), openToEverybody)
		if err != nil {
			h.t.Fatalf("claim: %v", err)
		}
		filed := mine
		filed.Status, filed.Issue = coord.IssueReportFiled, "https://github.com/crewlet/crewlet/issues/1"
		filed.Payload = []byte(`{"schema":1}`)
		stored, ok, err := h.f.SettleIssueReport(h.ctx, filed)
		if err != nil || !ok || stored.Status != coord.IssueReportFiled || string(stored.Payload) != `{"schema":1}` {
			h.t.Fatalf("settle = %+v, %v, %v; want it filed, whole", stored, ok, err)
		}
		if _, stale, staleErr := h.f.SettleIssueReport(h.ctx, filed); staleErr != nil || stale {
			h.t.Errorf("a settle at a stale version = %v, %v; want it refused", stale, staleErr)
		}
		read, found, readErr := h.f.IssueReport(h.ctx, issueReportFingerprint)
		if readErr != nil || !found || read.Version != stored.Version || read.Status != coord.IssueReportFiled {
			h.t.Errorf("IssueReport = %+v (found %v, %v), want the settled record", read, found, readErr)
		}
		all, listErr := h.f.ListIssueReports(h.ctx)
		if listErr != nil || len(all) != 1 || all[0].Fingerprint != issueReportFingerprint {
			h.t.Errorf("ListIssueReports = %+v, %v; want the one record", all, listErr)
		}
	},
}, {
	// THE SHAPES ARE THE GUARD: no sentence reaches the fleet's store through
	// a field that was meant to hold an id.
	name: "a record that is not the promised shape is refused and leaves nothing",
	fn: func(h *fleetHarness) {
		good := h.issueReport(coord.IssueReportSending, h.now())
		mutate := func(change func(*coord.IssueReport)) coord.IssueReport {
			r := good
			change(&r)
			return r
		}
		for name, bad := range map[string]coord.IssueReport{
			"a short fingerprint": mutate(func(r *coord.IssueReport) { r.Fingerprint = "abc" }),
			"words as a class":    mutate(func(r *coord.IssueReport) { r.Class = "Acme failed in /srv/acme" }),
			"an unknown status":   mutate(func(r *coord.IssueReport) { r.Status = "gone" }),
			"no instant":          mutate(func(r *coord.IssueReport) { r.ClaimedAt = time.Time{} }),
			"a long address": mutate(func(r *coord.IssueReport) {
				r.Issue = strings.Repeat("x", coord.MaxIssueReportAddressBytes+1)
			}),
			"a long payload": mutate(func(r *coord.IssueReport) {
				r.Payload = []byte(strings.Repeat("x", coord.MaxIssueReportPayloadBytes+1))
			}),
		} {
			if _, _, err := h.f.ClaimIssueReport(h.ctx, bad, openToEverybody); err == nil {
				h.t.Errorf("ClaimIssueReport with %s was accepted", name)
			}
		}
		if all, _ := h.f.ListIssueReports(context.WithoutCancel(h.ctx)); len(all) != 0 {
			h.t.Errorf("a refused record left %+v behind", all)
		}
	},
}}
