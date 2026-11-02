package engine

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/auxspend"
	"github.com/crewlet/crewlet/internal/compact"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/llmtest"
	"github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/worklet"
)

// A TURN'S END FOLLOWS ITS COST: every in-turn auxiliary record, its card's
// rewrite the last of them, is on the stream before agent_turn_completed.
//
// The Turn screen asks for the turn again when the seat leaves it, which the
// completion is what moves; a record the ledger published after the completion
// was missing from that answer, nothing asked again, and the page's tokens
// stood below the turn list's for the same turn. The card is the case that
// shows it: its rewrite used to be made after the completion, inside the
// charge's write, so it could not be on the stream before it whatever the
// ledger did. Driven through a real runTurn on a Worklet task, whose account is
// past what a card holds.
func TestATurnsEndFollowsEveryInTurnRecord(t *testing.T) {
	t.Parallel()
	e, served := spendingEngine(t)
	q := e.backends.Queue.(*memory.Queue)
	e.auxSpend = auxspend.NewLedger(q)
	model := longAccountModel()
	swapModel(t, e, model)
	rig := rigRuns(t, e, modelPlayer(t, model.pick))

	runToItsEnd(t, e, rig, Request{
		Handle: "swe", WorkKey: "wk-1", RunID: "run-1",
		WorkSince: time.Now().UTC(),
		Events:    []*events.Event{taskWake("task-9", "ENG-9")},
	})

	card, ended := -1, -1
	for i, ev := range q.History() {
		switch ev.Type {
		case types.AuxiliarySpend{}.EventType():
			rec, _ := events.DataAs[*types.AuxiliarySpend](ev)
			if rec.TurnID == "run-1" && rec.Purpose == types.AuxCondense(string(compact.KindOutcome)) {
				card = i
			}
		case types.AgentTurnCompleted{}.EventType():
			ended = i
		}
	}
	if card < 0 {
		t.Fatal("no record of the card's rewrite was published — the case exercises nothing")
	}
	if ended < 0 || card > ended {
		t.Fatalf("the card's rewrite was published at %d and the turn's end at %d: the end "+
			"must follow every in-turn record", card, ended)
	}
	if ended := endingCharge(t, served); !strings.HasPrefix(ended.Summary, condensedCard) {
		t.Fatalf("the segment that ended the turn charged %+v, want it carrying the rewritten card",
			ended)
	}
}

// endingCharge is the charge of the segment that ended a turn which parked
// once on its executor's run: the second of the two the task was handed.
func endingCharge(t *testing.T, served *servedWriter) worklet.TurnRecord {
	t.Helper()
	turns, _ := served.recorded()
	if len(turns) != 2 {
		t.Fatalf("the task was charged %+v, want the two segments of a turn that parked on its run",
			turns)
	}
	return turns[1]
}

// A TURN'S END IS WHERE ITS SEGMENT ENDED, NOT WHERE ITS END WAS PUBLISHED.
//
// The card is rewritten before the completion goes out, so the publish can
// come seconds after the segment's work did — and a completion that read its
// own clock there measured the rewrite into the turn's duration, which then
// disagreed with the task's turn row, stamped a moment earlier, for the same
// segment. Both are measured to the one instant the segment ended. Driven
// through a real runTurn on a Worklet task whose card needs a rewrite, with a
// rewrite slow enough to show in either figure.
//
// Mutation: let the completion read its own clock again, and duration_ms
// exceeds the task's wall_ms by the rewrite.
func TestATurnsEndIsMeasuredWhereItsSegmentEnded(t *testing.T) {
	t.Parallel()
	e, served := spendingEngine(t)
	q := e.backends.Queue.(*memory.Queue)
	e.auxSpend = auxspend.NewLedger(q)
	model := slowRewriteModel(200 * time.Millisecond)
	swapModel(t, e, model)
	rig := rigRuns(t, e, modelPlayer(t, model.pick))

	runToItsEnd(t, e, rig, Request{
		Handle: "swe", WorkKey: "wk-1", RunID: "run-1",
		WorkSince: time.Now().UTC(),
		Events:    []*events.Event{taskWake("task-9", "ENG-9")},
	})

	var completed *types.TurnCompleted
	for _, ev := range q.History() {
		if ev.Type == (types.TurnCompleted{}).EventType() {
			completed, _ = events.DataAs[*types.TurnCompleted](ev)
		}
	}
	if completed == nil {
		t.Fatal("no turn_completed was published")
	}
	ended := endingCharge(t, served)
	if !strings.HasPrefix(ended.Summary, condensedCard) {
		t.Fatalf("the segment that ended the turn charged %+v, want it carrying the rewritten "+
			"card — without a rewrite the case exercises nothing", ended)
	}
	if got, want := completed.DurationMS, ended.Spend.WallMs; got != want {
		t.Fatalf("turn_completed says the turn took %d ms and the task's turn row %d ms: "+
			"the card's rewrite was measured into one of them", got, want)
	}
	if got := completed.EndedAt.Sub(completed.StartedAt); got.Milliseconds() != int64(completed.DurationMS) {
		t.Errorf("the completion's end and start are %v apart, and its duration says %d ms",
			got, completed.DurationMS)
	}
}

// slowRewriteModel is [longAccountModel] whose rewrites — the calls offering no
// tools — take a while to answer.
func slowRewriteModel(delay time.Duration) phasedCLI {
	m := longAccountModel()
	text := m.text
	m.text = func(ctx context.Context, req llm.Request) (*llm.Completion, error) {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return text(ctx, req)
	}
	return m
}

// swapModel puts one provider behind every chain the spending engine's seat
// resolves — its phases and its auxiliary model alike — ahead of the entry its
// executor launches on, which it keeps.
func swapModel(t *testing.T, e *Engine, p llm.Provider) {
	t.Helper()
	c := *e.Company()
	models, err := phase.NewRegistry([]phase.Entry{{Key: "only", Provider: p}, runnerEntry(t)})
	if err != nil {
		t.Fatalf("phase registry: %v", err)
	}
	c.Models = models
	e.epoch.current.Store(&c)
}

// longAccountModel answers each phase as billingModel does, with a reviewer's
// account past what a task's card holds, and a rewrite with a short one.
func longAccountModel() phasedCLI {
	return phasedCLI{
		model: "billing",
		pick: func(offered []string) llmtest.Run {
			if offers(offered, runner.SubmitReviewTool) {
				return calling(runner.SubmitReviewTool, map[string]any{
					"decision":       "done",
					"completed_work": strings.Repeat("The seat read the task and found nothing to do. ", 20),
				}, llm.Usage{Input: billedInput, Output: billedOutput})
			}
			return billingRun(offered)
		},
		text: func(context.Context, llm.Request) (*llm.Completion, error) {
			return &llm.Completion{Model: "billing", Content: "Looked; nothing to do.",
				InputTokens: 400, OutputTokens: 8}, nil
		},
	}
}
