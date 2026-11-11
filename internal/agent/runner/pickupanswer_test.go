package runner_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/execstate"
	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/agent/turn"
)

// A PICKUP'S COLLECTED ROUND CARRIES THE ENGINE'S READ OF ITS TYPED ANSWER
// (ADR-0122): the round's Work says what the tracker's record of the turn's
// writes says, which the loop's checks then judge; a read that fails answers
// typed, so the turn falls back to the delivery every turn owes rather than
// loop on a store it cannot reach; and an assignment's turn reads nothing.
//
// Mutation: collect a pickup's round without reading its typed answer, and its
// comment reads as no answer whatever its run put on the item.
func TestAPickupsCollectedRoundCarriesItsTypedAnswer(t *testing.T) {
	t.Parallel()
	launched := time.Now().Add(-time.Hour).UTC()
	collect := func(reply turn.Reply, typed func(context.Context) (bool, error)) turn.Work {
		t.Helper()
		r, _ := buildWith(t, []phase.Entry{{Key: "default", Provider: &scriptedProvider{}}}, buildOpts{
			reply: reply, typed: typed,
			resume: &runner.Resume{
				State: execstate.State{Version: execstate.Version, Round: 1, LaunchedAt: launched,
					ActiveTools: []string{"slack_post"}},
				Answer: "noted that it still waits",
				Bridged: []runner.BridgedCall{{Call: ledger.Call{Name: "slack_post", Result: "posted"},
					Seq: 1, StartedAt: launched.Add(time.Minute)}},
				Run: runner.RunRecord{Harness: "claude-code", SandboxID: "box-1"},
			},
		})
		w, _, err := r.Resume(context.Background(), nil)
		if err != nil {
			t.Fatalf("Resume: %v", err)
		}
		return w
	}
	pickup := turn.ToolReply("")
	pickup.Item = "w-1"
	reads := 0
	answer := func(typed bool, err error) func(context.Context) (bool, error) {
		return func(context.Context) (bool, error) { reads++; return typed, err }
	}
	// A RUN THAT SUBMITTED NOTHING is a round the engine rescued, which the
	// reviewer sees; its `done` is overturned for the same reason.
	if w := collect(pickup, answer(false, nil)); w.Typed {
		t.Error("a pickup's round with nothing typed on its item collected as typed")
	} else if override, correction := turn.OverrideDone(w, nil, pickup, turn.Surface{
		Deliveries: map[string]string{"slack_post": "slack"}}); !override || correction != turn.PickupUntyped {
		t.Errorf("the reviewer's done on that round stood: %v, %q", override, correction)
	}
	if w := collect(pickup, answer(true, nil)); !w.Typed {
		t.Error("a pickup's round with a typed answer on its item collected as words")
	}
	if w := collect(pickup, answer(false, errors.New("the estate did not answer"))); !w.Typed {
		t.Error("a pickup's round whose answer could not be read was held to it anyway")
	}
	before := reads
	if w := collect(turn.ToolReply(""), answer(false, nil)); w.Typed || reads != before {
		t.Errorf("an assignment's round read a typed answer %d times", reads-before)
	}
}
