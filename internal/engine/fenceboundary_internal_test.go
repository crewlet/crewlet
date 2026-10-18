package engine

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// EVERY SEGMENT FENCES WITH A TOKEN OF ITS OWN, and its tools fence with the
// one its prompt's thread was fenced with.
//
// The token reaches the tools on the turn and the thread through the prefetch,
// and both read it off the telemetry, so a turn built without one would fence
// each read with a fresh token: still unguessable, but no longer the segment's
// one token. A resumed segment mints its own, so a token a model echoed into
// what it wrote before it parked is not the token its next reads carry.
func TestEverySegmentFencesWithATokenOfItsOwn(t *testing.T) {
	t.Parallel()
	seat := &org.Role{Name: "Engineer", DeclaredHandle: "swe"}
	company := &Company{Org: &org.Organization{Name: "Acme", Roles: []*org.Role{seat}}}
	began := time.Now().UTC().Add(-time.Hour)

	dispatched := mustDescribeTurn(t, &Engine{}, company, Request{
		Handle: "swe", RunID: "run-1", WorkKey: "wk-1", WorkSince: began,
	})
	resumed := mustDescribeResume(t, &Engine{}, company, resumeInput{
		Run: sandbox.PendingRun{TurnID: "run-1", WorkKey: "wk-1", WorkSince: began,
			AgentHandle: "swe"},
		Turn: &turnctx.Turn{RunID: "run-1", WorkKey: "wk-1", WorkSince: began, Seat: seat},
	})
	for name, tel := range map[string]turnTelemetry{"a dispatched segment": dispatched, "a resumed one": resumed} {
		if !tel.boundary.Valid() {
			t.Fatalf("%s minted no fence token: %q", name, tel.boundary)
		}
		got := tel.runnerTurn(company, 0, nil, "the task", turn.ToolReply(""), nil).Context
		if got.Boundary != tel.boundary || got.FenceBoundary() != tel.boundary {
			t.Fatalf("%s's tools fence with %q, its prompt with %q", name, got.Boundary, tel.boundary)
		}
	}
	if dispatched.boundary == resumed.boundary {
		t.Fatal("a resumed segment reused the token of the segment before it")
	}
}
