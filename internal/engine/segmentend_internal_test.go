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
	"github.com/crewlet/crewlet/internal/queue/memory"
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
// ledger did. Driven through a real runTurn on a native task, whose account is
// past what a card holds.
func TestATurnsEndFollowsEveryInTurnRecord(t *testing.T) {
	t.Parallel()
	e, served := spendingEngine(t)
	q := e.backends.Queue.(*memory.Queue)
	e.auxSpend = auxspend.NewLedger(q)
	swapModel(t, e, longAccountModel{})

	if _, err := e.runTurn(t.Context(), Request{
		Handle: "swe", WorkKey: "wk-1", RunID: "run-1",
		WorkSince: time.Now().UTC(),
		Events:    []*events.Event{taskWake("task-9", "ENG-9")},
	}); err != nil {
		t.Fatalf("runTurn: %v", err)
	}

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
	turns, _ := served.recorded()
	if len(turns) != 1 || !strings.HasPrefix(turns[0].Summary, condensedCard) {
		t.Fatalf("the task was charged %+v, want one turn carrying the rewritten card", turns)
	}
}

// swapModel puts one provider behind every chain the spending engine's seat
// resolves — its phases and its auxiliary model alike.
func swapModel(t *testing.T, e *Engine, p llm.Provider) {
	t.Helper()
	c := *e.Company()
	models, err := phase.NewRegistry([]phase.Entry{{Key: "only", Provider: p}})
	if err != nil {
		t.Fatalf("phase registry: %v", err)
	}
	c.Models = models
	e.epoch.current.Store(&c)
}

// longAccountModel answers each phase as billingModel does, with a reviewer's
// account past what a task's card holds, and a rewrite with a short one.
type longAccountModel struct{}

func (longAccountModel) Model() string { return "billing" }

func (longAccountModel) Complete(ctx context.Context, req llm.Request) (*llm.Completion, error) {
	if len(req.Tools) == 0 {
		return &llm.Completion{Model: "billing", Content: "Looked; nothing to do.",
			InputTokens: 400, OutputTokens: 8}, nil
	}
	for _, tool := range req.Tools {
		if tool.Name == runner.SubmitReviewTool {
			return &llm.Completion{Model: "billing", ToolCalls: []llm.ToolCall{{
				ID: "c1", Name: runner.SubmitReviewTool, Arguments: map[string]any{
					"decision":       "done",
					"completed_work": strings.Repeat("The seat read the task and found nothing to do. ", 20),
				},
			}}, InputTokens: billedInput, OutputTokens: billedOutput}, nil
		}
	}
	return billingModel{}.Complete(ctx, req)
}
