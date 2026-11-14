package turn_test

import (
	"context"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/events/types"
)

// A ROUND ITS PROVIDER REFUSED ENDS THE TURN ON THE PROVIDER'S MESSAGE. The
// model read nothing, so there is no work to check, review or send back: sent
// back, the next launch is refused word for word and the stall guard names the
// failure "the same artifact twice", which hides what the provider said.
//
// Mutation: drop the branch on Work.RequestRefused in Run, and the round is
// reviewed, sent back and ended as a stall.
func TestARoundItsProviderRefusedEndsTheTurnWithItsMessage(t *testing.T) {
	t.Parallel()
	const said = "API Error: 400 tools.88.custom.name: String should match pattern '^[a-zA-Z0-9_-]{1,128}$'"
	refused := turn.Work{Outcome: turn.OutcomeIncomplete, Rescued: true,
		Text: "The executor's run did NOT fully succeed.\n\nError: " + said, RequestRefused: said}
	f := &fake{
		works:    []turn.Work{refused},
		surfaces: []turn.Surface{slackSurface()},
		reviews:  []turn.Review{{Decision: phase.SelfIterate, Notes: "again"}},
	}
	res, err := turn.Run(context.Background(), f, settings(), turn.Input{
		RunID: "t1", Reply: turn.ToolReply(""), Resume: true, Round: 1,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Decision != phase.Failed || res.Breach == nil || res.Breach.Kind != types.GuardRequestRefused ||
		!strings.Contains(res.Breach.Detail, said) {
		t.Fatalf("the turn ended %s with breach %+v, want it failed as request_refused with the "+
			"provider's message", res.Decision, res.Breach)
	}
	if f.revRounds != 0 || f.workRounds != 0 {
		t.Errorf("the refused round cost %d reviews and %d more launches, want none", f.revRounds, f.workRounds)
	}
	if len(res.Iterations) != 0 {
		t.Errorf("the refused round was closed as work: %+v", res.Iterations)
	}
}
