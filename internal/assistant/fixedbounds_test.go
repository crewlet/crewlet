package assistant_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/assistant"
	"github.com/crewlet/crewlet/internal/assistant/assistanttest"
	"github.com/crewlet/crewlet/internal/assistant/catalog"
	"github.com/crewlet/crewlet/internal/assistant/evals"
	"github.com/crewlet/crewlet/internal/assistant/exec"
	"github.com/crewlet/crewlet/internal/assistant/proposal"
	"github.com/crewlet/crewlet/internal/assistant/wire"
)

// TestATurnRunsUnderTheFixedBounds: no setting bounds a turn. It takes at most
// forty model rounds, runs at most twenty minutes (its read grant's life), and
// a change it proposes waits fifteen minutes for its person. Mutation: move
// MaxRounds, TurnTimeout or proposal.TTL, or let the loop take another round.
func TestATurnRunsUnderTheFixedBounds(t *testing.T) {
	t.Parallel()
	if assistant.MaxRounds != 40 || assistant.TurnTimeout != 20*time.Minute ||
		exec.ReadLifetime != assistant.TurnTimeout || proposal.TTL != 15*time.Minute {
		t.Fatalf("the bounds are %d rounds, %v, a %v grant and a %v wait", assistant.MaxRounds,
			assistant.TurnTimeout, exec.ReadLifetime, proposal.TTL)
	}

	// A MODEL THAT NEVER STOPS READING is stopped at the bound.
	rounds := make([]evals.Round, 0, assistant.MaxRounds+10)
	for i := range assistant.MaxRounds + 10 {
		rounds = append(rounds, evals.Call(fmt.Sprintf("r%d", i), catalog.ReadAPITool,
			taskArgs(fmt.Sprintf("ENG-%d", i))))
	}
	f := assistanttest.NewRig(t, evals.RigOptions{Tools: []catalog.Tool{getTask(t)},
		Handler: task, Rounds: rounds})
	accepted, err := f.Ask("bounded", "Read every task.", nil)
	if err != nil {
		t.Fatal(err)
	}
	done := evals.Last(f.Follow(accepted.TurnID, 0), wire.EventDone)
	if done == nil || done.Done.Steps < assistant.MaxRounds || done.Done.Steps > assistant.MaxRounds+1 {
		t.Errorf("a turn that kept reading ended as %+v, want it stopped at the %d-round bound",
			done, assistant.MaxRounds)
	}

	// A PROPOSAL WAITS FIFTEEN MINUTES from when it is made.
	comment := assistanttest.Tool(t, "comment_on_task")
	f = assistanttest.NewRig(t, evals.RigOptions{Tools: []catalog.Tool{comment},
		Acts: []string{comment.Act}, Handler: task, Rounds: []evals.Round{
			evals.Call("c", "comment_on_task", map[string]any{"item": "ENG-7", "body": "Noted."}),
			evals.Say("Proposed.")}})
	before := f.Clock.Now()
	accepted, err = f.Ask("wait", "Comment on ENG-7.", nil)
	if err != nil {
		t.Fatal(err)
	}
	f.Follow(accepted.TurnID, 0)
	proposals := f.Store.Proposals()
	if len(proposals) != 1 || proposals[0].Card.Proposal == nil {
		t.Fatalf("the turn proposed %+v", proposals)
	}
	wait := proposals[0].Card.Proposal.ExpiresAt.Sub(before) //fixedwait:stated both are the rig's clock, fixed until a test moves it
	if wait < proposal.TTL-time.Minute || wait > proposal.TTL+time.Minute {
		t.Errorf("the proposal waits %v, want %v", wait, proposal.TTL)
	}
}
