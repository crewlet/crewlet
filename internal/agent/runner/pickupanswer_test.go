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
	"github.com/crewlet/crewlet/internal/events/types"
)

// A PICKUP'S COLLECTED ROUND CARRIES THE ENGINE'S READ OF ITS ANSWER
// (ADR-0122): the round's Work says what the tracker's rows say, which the
// loop's checks then judge; an item that left the seat is carried as such and
// accepted; a read that fails, or answers what this build does not know,
// answers unread, so the turn falls back to the delivery every turn owes rather
// than loop on a store it cannot reach; and an assignment's turn reads nothing.
//
// Mutation: collect a pickup's round without reading its answer, and its
// comment reads as no answer whatever its run put on the item.
func TestAPickupsCollectedRoundCarriesItsTypedAnswer(t *testing.T) {
	t.Parallel()
	launched := time.Now().Add(-time.Hour).UTC()
	collect := func(reply turn.Reply, read func(context.Context) (types.PickupAnswer, error)) turn.Work {
		t.Helper()
		r, _ := buildWith(t, []phase.Entry{{Key: "default", Provider: &scriptedProvider{}}}, buildOpts{
			reply: reply, answer: read,
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
	answer := func(a types.PickupAnswer, err error) func(context.Context) (types.PickupAnswer, error) {
		return func(context.Context) (types.PickupAnswer, error) { reads++; return a, err }
	}
	// A RUN THAT SUBMITTED NOTHING is a round the engine rescued, which the
	// reviewer sees; its `done` is overturned for the same reason.
	if w := collect(pickup, answer(types.PickupUnanswered, nil)); w.Answer != types.PickupUnanswered {
		t.Errorf("a pickup's round with nothing typed on its item collected as %q", w.Answer)
	} else if override, correction := turn.OverrideDone(w, nil, pickup, turn.Surface{
		Deliveries: map[string]string{"slack_post": "slack"}}); !override || correction != turn.PickupUntyped {
		t.Errorf("the reviewer's done on that round stood: %v, %q", override, correction)
	}
	for _, want := range []types.PickupAnswer{types.PickupTyped, types.PickupRemoved,
		types.PickupFinished, types.PickupReassigned} {
		if w := collect(pickup, answer(want, nil)); w.Answer != want {
			t.Errorf("a pickup's round read %q collected as %q", want, w.Answer)
		}
	}
	if w := collect(pickup, answer("", errors.New("the estate did not answer"))); w.Answer != types.PickupUnread {
		t.Errorf("a pickup's round whose answer could not be read collected as %q", w.Answer)
	}
	if w := collect(pickup, answer("pondered", nil)); w.Answer != types.PickupUnread {
		t.Errorf("a pickup's round read as an answer this build does not know collected as %q", w.Answer)
	}
	before := reads
	if w := collect(turn.ToolReply(""), answer(types.PickupUnanswered, nil)); w.Answer != "" || reads != before {
		t.Errorf("an assignment's round read a pickup's answer %d times (%q)", reads-before, w.Answer)
	}
}
