package engine

import (
	"context"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// THE INSTANT A UNIT OF WORK BEGAN TRAVELS THE WHOLE WAY ITS KEY DOES: from the
// dispatch into the turn's tools, onto a detached run's row, and back out into
// the turn that resumes it — days later, possibly on another node.
//
// Every operation id a turn derives from its work key carries this instant as
// its mint time, and the state log refuses to decide again an operation
// minted before its node adopted a donated snapshot. A hop that dropped it
// would hand the tools a zero instant (every write refused `unknown` on a node
// that ever adopted); a hop that re-derived it from its own clock would read a
// re-run after an adoption as a new operation; and a resume that lost it would
// derive different ids for the second half of a turn than the first half used.
func TestWhenTheWorkBeganTravelsWithTheWorkKey(t *testing.T) {
	t.Parallel()
	began := time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)
	seat := &org.Role{Name: "Engineer", DeclaredHandle: "swe"}
	company := &Company{Org: &org.Organization{Name: "Acme", Roles: []*org.Role{seat}}}

	// THE DISPATCH'S OWN TURN, handed to every tool.
	dispatched := (&Engine{}).describeTurn(context.Background(), company, Request{
		Handle: "swe", RunID: "run-1", WorkKey: "wk-1", WorkSince: began,
	})
	live := dispatched.runnerTurn(company, 0, nil, "the task", turn.ToolReply(""))
	if live.Context == nil || !live.Context.WorkSince.Equal(began) {
		t.Fatalf("the turn's tools see the work begin at %+v, want %s", live.Context, began)
	}

	// ONTO THE ROW a detached run is resumed from.
	l := &agentLauncher{seat: seat, turn: &turnctx.Turn{
		RunID: "run-1", WorkKey: "wk-1", WorkSince: began, Seat: seat,
	}}
	ref := l.runTurnRef(context.Background())
	if !ref.WorkSince.Equal(began) {
		t.Fatalf("the detached run's row records %s, want %s", ref.WorkSince, began)
	}

	// AND BACK OUT, into the turn that resumes it.
	resumedTel := (&Engine{}).describeResume(context.Background(), company, resumeInput{
		Run: sandbox.PendingRun{
			TurnID: "run-1", WorkKey: "wk-1", WorkSince: ref.WorkSince,
			AgentHandle: "swe",
		},
		Turn: &turnctx.Turn{RunID: "run-1", WorkKey: "wk-1", WorkSince: began, Seat: seat},
	})
	resumedTurn := resumedTel.runnerTurn(company, 0, nil, "the task", turn.ToolReply(""))
	if resumedTurn.Context == nil || !resumedTurn.Context.WorkSince.Equal(began) {
		t.Fatalf("the resumed turn's tools see the work begin at %+v, want %s — "+
			"the second half of the turn would derive different operation ids "+
			"from the first", resumedTurn.Context, began)
	}
}

// A RUN AN OLDER BUILD PARKED RESUMES WITH A REAL INSTANT, on both the turn its
// tools run under and the telemetry that describes it.
//
// Its row has a unit of work and no `work_since`, and a zero instant reads as
// older than every loss the operation ledger has recorded: on any node whose
// ledger had swept once, every write the resumed turn made answered `unknown`.
// The row's own CreatedAt is the fixed instant it answers instead.
func TestARunAnOlderBuildParkedResumesWithAnInstant(t *testing.T) {
	t.Parallel()
	seat := &org.Role{Name: "Engineer", DeclaredHandle: "swe"}
	company := &Company{Org: &org.Organization{Name: "Acme", Roles: []*org.Role{seat}}}
	launched := time.Date(2026, 9, 1, 8, 10, 0, 0, time.UTC)
	run := sandbox.PendingRun{
		TurnID: "run-1", WorkKey: "wk-1", AgentHandle: "swe", CreatedAt: launched,
	}

	resumed := resumedTurn(run, seat, company.Org)
	if !resumed.WorkSince.Equal(launched) || resumed.WorkKey != "wk-1" {
		t.Fatalf("the resumed turn's tools see work %q begin at %s, want wk-1 at "+
			"%s — a zero instant answers every write unknown", resumed.WorkKey,
			resumed.WorkSince, launched)
	}
	described := (&Engine{}).describeResume(context.Background(), company, resumeInput{
		Run: run, Turn: resumed,
	}).runnerTurn(company, 0, nil, "the task", turn.ToolReply(""))
	if described.Context == nil || !described.Context.WorkSince.Equal(launched) {
		t.Fatalf("the resumed turn's runner sees the work begin at %+v, want %s",
			described.Context, launched)
	}
}
