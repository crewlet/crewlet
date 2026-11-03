package engine

import (
	"fmt"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/maintenance"
)

// retentionDoc is a company on Worklet stating its inbox's sweep horizon.
func retentionDoc(inboxDays int) string {
	return fmt.Sprintf(`
name: Nimbus
inbox:
  retention_days: %d
roles:
  - name: SWE
    handle: swe
`, inboxDays)
}

// jobNamed is the sweep job of that name, failing the case when there is none.
func jobNamed(t *testing.T, jobs []maintenance.Job, name string) maintenance.Job {
	t.Helper()
	for _, j := range jobs {
		if j.Name == name {
			return j
		}
	}
	t.Fatalf("the sweep has no %s job", name)
	return maintenance.Job{}
}

// A REVISION THAT MOVES A SWEEP HORIZON IS HONOURED BY THE SWEEP ALREADY
// RUNNING, with no restart.
//
// The inbox's horizon is the company's, and it was read ONCE, when the sweep
// was built at boot: a revision shortening it was honoured everywhere else at
// once and by the sweep only after the process restarted, so rows the company
// had asked to forget were kept for as long as the node stayed up — and a
// lengthened one kept deleting rows the company had just asked to keep. The
// jobs built at boot are asked here after the apply, which is what the running
// worker does at its next sweep.
//
// Mutation: have [Engine.maintenanceJobs] hand the inbox job a horizon read
// once ([maintenance.Fixed] of the boot value) and the second half goes red.
func TestTheSweepHonoursAHorizonARevisionMoves(t *testing.T) {
	t.Parallel()
	e := newSandboxNode(t, parseCompany(t, retentionDoc(30)))
	inbox := jobNamed(t, e.maintenanceJobs(), "tracker_notifications")
	day := 24 * time.Hour
	if got := inbox.Horizon(); got != 30*day {
		t.Fatalf("the inbox horizon is %v, want the configured 30 days", got)
	}

	applyOK(t, e, retentionDoc(90))

	if got := inbox.Horizon(); got != 90*day {
		t.Errorf("after a revision set 90 days the running sweep keeps the inbox "+
			"for %v — the horizon it was built with", got)
	}
}
