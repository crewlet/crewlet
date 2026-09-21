package turn_test

import (
	"context"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/turn"
)

// THE TURN THIS WHOLE RULE EXISTS FOR: a seat that could not finish without a
// person, asked them where they had asked IT, and ended there.
//
// `blocked` + `done` is how that is spelled — the EXECUTOR's word for what
// happened beside the REVIEWER's for whether it was good enough — and the loop
// has to let it end. It used to end `self_iterate`, and the next round asked
// the same question again: a CEO seat put a clarifying question to its founder,
// was sent back, re-posted it two minutes later, and terminated on the
// max-iterations guard.
//
// Nothing in the loop needed changing for this, which is the point — the
// contradiction was in the prompt. This is what says so, and what would catch
// a guard added later that quietly took the case back.
func TestATurnBlockedOnSomebodyAlreadyAskedEndsDone(t *testing.T) {
	t.Parallel()
	f := &fake{
		works: []turn.Work{{
			Outcome:  turn.OutcomeBlocked,
			Summary:  "asked the founder which repo this belongs in",
			Evidence: "asked @founder which repo to file against; waiting on them",
			Text:     "Which repo should this go in?",
			// THE QUESTION WENT WHERE IT WAS ASKED. That is what makes it a
			// handoff rather than silence, and it is the only difference
			// between this test and the one below it.
			Calls: []ledger.Call{{Name: "slack_post"}},
		}},
		surfaces: []turn.Surface{slackSurface()},
		reviews:  []turn.Review{{Decision: phase.Done}},
	}
	res, err := turn.Run(context.Background(), f, turn.Settings{MaxIterations: 3},
		turn.Input{RunID: "t1", Reply: turn.ToolReply("test")})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Decision != phase.Done {
		t.Errorf("decision = %s, want done: a turn that asked the right person was not allowed to end",
			res.Decision)
	}
	if res.Rounds != 1 {
		t.Errorf("rounds = %d, want 1: the turn looped rather than ending", res.Rounds)
	}
	// The executor's account has to survive the loop, because two readers
	// downstream have nothing else: the conversation entry the seat's next
	// turn opens on, and the A2A answer on a turn that produced no prose.
	if res.LastWork == nil {
		t.Fatal("the executor's round did not survive the turn")
	}
	if res.LastWork.Outcome != turn.OutcomeBlocked {
		t.Errorf("outcome = %s, want blocked", res.LastWork.Outcome)
	}
	if !strings.Contains(res.LastWork.Evidence, "waiting on them") {
		t.Errorf("the evidence did not survive: %q", res.LastWork.Evidence)
	}
}

// ASKING A COLLEAGUE IS NOT ANSWERING THE REQUESTER, and this is the branch
// that says so.
//
// It is the one case the flat delivery check used to pass, and the reason the
// rule keeps its last clause: whoever triggered the turn is owed a reply where
// THEY asked, even one that only names who the seat is waiting on. A turn that
// consults its manager on one surface while the requester watches a thread
// that never moves has not handed off — it has gone quiet.
//
// The correction NAMES the surface, because a turn that did write somewhere
// reads "no tool was called" as false against its own record and argues with
// the correction instead of acting on it.
func TestAskingAColleagueDoesNotEndATurnTheRequesterIsWaitingOn(t *testing.T) {
	t.Parallel()
	f := &fake{
		works: []turn.Work{{
			Outcome:  turn.OutcomeBlocked,
			Summary:  "asked the CTO",
			Evidence: "needs a deploy key I do not hold; asked the CTO",
			Text:     "I've asked the CTO.",
			// Reached somebody — just not the person waiting.
			Calls: []ledger.Call{{Name: "a2a_ask"}},
		}},
		surfaces: []turn.Surface{{
			Catalogue:  []string{"slack_post", "a2a_ask"},
			Deliveries: map[string]string{"slack_post": "test", "a2a_ask": "a2a"},
		}},
		reviews: []turn.Review{{Decision: phase.Done, Notes: "sensible"}},
	}
	res, _ := turn.Run(context.Background(), f, turn.Settings{MaxIterations: 2},
		turn.Input{RunID: "t1", Reply: turn.ToolReply("test")})

	if res.Decision == phase.Done {
		t.Error("the requester was left in silence while a colleague was consulted")
	}
	if len(f.notesSeen) < 2 {
		t.Fatalf("the turn did not loop back: %q", f.notesSeen)
	}
	// NAMED, and the reviewer's own note survives in front of it.
	if !strings.Contains(f.notesSeen[1], "not the person waiting") {
		t.Errorf("the correction did not say who was missed: %q", f.notesSeen[1])
	}
	if !strings.Contains(f.notesSeen[1], "test") {
		t.Errorf("the correction did not name the surface: %q", f.notesSeen[1])
	}
	if !strings.Contains(f.notesSeen[1], "sensible") {
		t.Errorf("the reviewer's own note was dropped: %q", f.notesSeen[1])
	}
}
