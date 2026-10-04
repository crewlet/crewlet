package engine

import (
	"fmt"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/maintenance"
)

// retentionDoc is a native company stating both of its own sweep horizons.
func retentionDoc(conversationDays, inboxDays int) string {
	return fmt.Sprintf(`
name: Nimbus
tracker:
  native:
    inbox_retention_days: %d
turn_engine:
  conversation_session:
    retention_days: %d
roles:
  - name: SWE
    handle: swe
`, inboxDays, conversationDays)
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
// The conversation ledger's and the inbox's horizons are the company's, and
// they were read ONCE, when the sweep was built at boot: a revision shortening
// either was honoured everywhere else at once and by the sweep only after the
// process restarted, so rows the company had asked to forget were kept for as
// long as the node stayed up — and a lengthened one kept deleting rows the
// company had just asked to keep. The jobs built at boot are asked here after
// the apply, which is what the running worker does at its next sweep.
func TestTheSweepHonoursAHorizonARevisionMoves(t *testing.T) {
	t.Parallel()
	e := newSandboxNode(t, parseCompany(t, retentionDoc(3, 30)))
	jobs := e.maintenanceJobs()
	conversations := jobNamed(t, jobs, "conversation_sessions")
	inbox := jobNamed(t, jobs, "tracker_notifications")
	day := 24 * time.Hour
	if got := conversations.Horizon(); got != 3*day {
		t.Fatalf("the conversation horizon is %v, want the configured 3 days", got)
	}
	if got := inbox.Horizon(); got != 30*day {
		t.Fatalf("the inbox horizon is %v, want the configured 30 days", got)
	}

	applyOK(t, e, retentionDoc(7, 90))

	if got := conversations.Horizon(); got != 7*day {
		t.Errorf("after a revision set 7 days the running sweep keeps conversations "+
			"for %v — the horizon it was built with", got)
	}
	if got := inbox.Horizon(); got != 90*day {
		t.Errorf("after a revision set 90 days the running sweep keeps the inbox "+
			"for %v — the horizon it was built with", got)
	}
}
