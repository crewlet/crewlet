package host

import (
	"testing"

	"github.com/crewlet/crewlet/internal/code/gitproc"
)

// THE HOST'S STOP WAITS OUT A KILLED GIT: its bound is longer than the SIGKILL
// that follows a cancelled process's SIGTERM and the release of a pipe a
// straggler holds.
func TestTheStopBudgetOutlastsAKilledGit(t *testing.T) {
	t.Parallel()
	if StopBudget <= 2*gitproc.KillGrace {
		t.Fatalf("StopBudget %v ends before a killed git's SIGKILL and pipe release (%v)",
			StopBudget, 2*gitproc.KillGrace)
	}
}
