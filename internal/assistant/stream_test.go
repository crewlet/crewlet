package assistant_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/assistant/assistanttest"
	"github.com/crewlet/crewlet/internal/assistant/evals"
	"github.com/crewlet/crewlet/internal/assistant/wire"
	"github.com/crewlet/crewlet/internal/providers/llm"
)

// drawn is the answer as the drawer draws it from delta frames: appended, and
// replaced whole by a restart (dashboard/src/app/assistant/chunk/turn.ts).
func drawn(frames []wire.Frame) (text string, restarts int) {
	for _, f := range frames {
		if f.Kind != wire.EventDelta {
			continue
		}
		if f.Delta.Restart {
			restarts++
			text = f.Delta.Text
			continue
		}
		text += f.Delta.Text
	}
	return text, restarts
}

// whileWriting reads what a turn's stream holds while its round is still being
// written: until the read times out, since the turn has not ended.
func whileWriting(t *testing.T, f *assistanttest.Rig, turn wire.TurnAccepted) []wire.Frame {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	c := &evals.Collector{}
	// The stream ends on the deadline; whatever it says about that is not
	// what is under test, the frames it delivered before it are.
	_ = f.Svc.Stream(ctx, f.Person.ID, f.Conversation, turn.TurnID, 0, c)
	return c.Frames()
}

// A STREAMED ROUND SHOWS ITS PROSE BEFORE THE ROUND ENDS, a settled line at a
// time. The round is held after its fragments are sent: the drawer already
// holds the first line, nothing of the line still being written (a credential
// is never shown before its end arrives), and no done frame; once the round
// commits, the answer it holds is exactly the committed prose, with nothing
// redrawn.
//
// Mutation: drop StreamPartials from the loop's config and nothing is drawn
// while the round is held.
func TestAStreamedRoundShowsItsProseBeforeTheRoundEnds(t *testing.T) {
	t.Parallel()
	streamed, hold := make(chan struct{}), make(chan struct{})
	answer := "The keys rotate weekly.\nThe next rotation is on Monday."
	f := assistanttest.NewRig(t, evals.RigOptions{Rounds: []evals.Round{{
		Text: answer, Input: 100, Output: 20, Streamed: streamed, Hold: hold,
		Deltas: []llm.Delta{{Content: "The keys rotate weekly.\nThe next rot"}},
	}}})
	accepted, err := f.Ask("stream", "When do the keys rotate?", nil)
	if err != nil {
		t.Fatal(err)
	}
	<-streamed
	early := whileWriting(t, f, accepted)
	text, _ := drawn(early)
	if text != "The keys rotate weekly." {
		t.Errorf("while the round was written the drawer held %q, want its first settled line", text)
	}
	if evals.Last(early, wire.EventDone) != nil {
		t.Fatal("the turn ended before the round was released")
	}
	close(hold)
	frames := f.Follow(accepted.TurnID, 0)
	text, restarts := drawn(frames)
	if text != answer || restarts != 0 {
		t.Errorf("the drawer ended on %q after %d restarts, want %q with none", text, restarts, answer)
	}
}

// A RETRACTED PARTIAL IS ERASED WITH A RESTART. The backend withdrew the view
// it had streamed; the drawer is told to discard it, and what it holds in the
// end is the committed answer alone, never the retracted draft beside it.
func TestARetractedPartialIsErasedWithARestart(t *testing.T) {
	t.Parallel()
	f := assistanttest.NewRig(t, evals.RigOptions{Rounds: []evals.Round{{
		Text: "The keys rotate weekly.", Input: 100, Output: 20,
		Deltas: []llm.Delta{
			{Content: "The keys rotate daily.\n"},
			{Retract: true},
			{Content: "The keys rotate weekly."},
		},
	}}})
	accepted, err := f.Ask("retract", "When do the keys rotate?", nil)
	if err != nil {
		t.Fatal(err)
	}
	frames := f.Follow(accepted.TurnID, 0)
	text, restarts := drawn(frames)
	if restarts == 0 {
		t.Errorf("no restart was sent for the retracted view: %v", kinds(frames))
	}
	if text != "The keys rotate weekly." || strings.Contains(text, "daily") {
		t.Errorf("the drawer ended on %q, want the committed answer alone", text)
	}
}

// A CLI THAT CANNOT STREAM STILL SENDS ONE DELTA, AT THE ROUND'S END. Streaming
// is asked for on every turn, and a backend with no streaming mode never sends
// a fragment: its round is drawn once, whole, when it commits.
func TestANonStreamingCLISendsOneDeltaAtTheRoundsEnd(t *testing.T) {
	t.Parallel()
	f := assistanttest.NewRig(t, evals.RigOptions{Rounds: []evals.Round{
		evals.Say("The keys rotate weekly."),
	}})
	accepted, err := f.Ask("whole", "When do the keys rotate?", nil)
	if err != nil {
		t.Fatal(err)
	}
	frames := f.Follow(accepted.TurnID, 0)
	var deltas []wire.DeltaEvent
	for _, fr := range frames {
		if fr.Kind == wire.EventDelta {
			deltas = append(deltas, *fr.Delta)
		}
	}
	if len(deltas) != 1 || deltas[0].Text != "The keys rotate weekly." || deltas[0].Restart {
		t.Errorf("deltas = %+v, want the round once, whole", deltas)
	}
	if req := f.Model.Requests()[0]; !req.Streaming() || !req.StreamTokens {
		t.Errorf("streaming %v, token stream %v: the turn did not ask its model for its token stream",
			req.Streaming(), req.StreamTokens)
	}
}
