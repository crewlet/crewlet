package engine

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// EVERY RUN'S TOOLS SEE A CALL LOG OF THEIR OWN, a resumed run's included.
//
// A derived operation id carries how many different calls to its tool the run
// made before it, read off the turn's log. A turn built without one counts
// nothing, so every call is its run's first of its kind and a call made again
// after a different one takes its first copy's id — the bug the log exists to
// end, silently back for every turn. And a log SHARED between runs would count
// another run's calls, so a re-run would not reproduce its own counts.
func TestEveryRunsToolsSeeACallLogOfTheirOwn(t *testing.T) {
	t.Parallel()
	seat := &org.Role{Name: "Engineer", DeclaredHandle: "swe"}
	company := &Company{Org: &org.Organization{Name: "Acme", Roles: []*org.Role{seat}}}

	// BEGUN AN HOUR AGO, so neither half is past the ledger's horizon and
	// nothing needs a coordination store to record where it mints.
	began := time.Now().UTC().Add(-time.Hour)
	dispatched := mustDescribeTurn(t, &Engine{}, company, Request{
		Handle: "swe", RunID: "run-1", WorkKey: "wk-1", WorkSince: began,
	})
	first := dispatched.runnerTurn(company, 0, nil, "the task", turn.ToolReply(""))
	rerun := dispatched.runnerTurn(company, 0, nil, "the task", turn.ToolReply(""))
	resumed := mustDescribeResume(t, &Engine{}, company, resumeInput{
		Run: sandbox.PendingRun{TurnID: "run-1", WorkKey: "wk-1", WorkSince: began,
			AgentHandle: "swe"},
		Turn: &turnctx.Turn{RunID: "run-1", WorkKey: "wk-1", WorkSince: began, Seat: seat},
	}).runnerTurn(company, 0, nil, "the task", turn.ToolReply(""))

	for name, got := range map[string]*turnctx.Turn{
		"a dispatched run": first.Context, "a resumed run": resumed.Context,
	} {
		if got.CallLog() == nil {
			t.Errorf("%s's tools see no call log — every repeat count is zero", name)
		}
	}
	if first.Context.CallLog() == rerun.Context.CallLog() {
		t.Error("two runs share one call log — a re-run would count the first run's calls")
	}
}
