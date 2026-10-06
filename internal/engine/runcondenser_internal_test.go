package engine

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/auxspend"
	"github.com/crewlet/crewlet/internal/compact"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/workkey"
)

// A COLLECTED RUN'S CONDENSATION IS THE TURN'S, AND IT FITS THE RECORD.
//
// The coordinator condenses a run's report, failure or question past what the
// run's record carries, and trusts two things of the engine's condenser: that
// the call is filed as part of the turn the run belongs to — the turn stage,
// the run's turn id and its unit of work, with the purpose naming what was
// rewritten — and that the text it hands back, its label included, is within
// the budget it was given. Filed anywhere else, the condensation is missing
// from the turn's cost on every screen; past the budget, the sandbox refuses
// the rewrite and every long report falls back to whole lines.
//
// A row an older build parked carries no work key, so its unit of work is the
// derived turn id, as every other record of that run files it. And what the
// rewrite cost is answered with it, exactly as its record states it, for the
// segment that resumes from the collection to pay.
//
// Driven through the real compactor and the real ledger, with a model that
// answers exactly as long as it is asked to be.
//
// Mutations: file the call under the background stage, drop the turn id, read
// the raw work key, hand the compactor the whole budget rather than the budget
// less the label, or answer no cost — each turns this red.
func TestARunsCondensationIsFiledUnderItsTurnAndFitsItsBudget(t *testing.T) {
	t.Parallel()
	derived := workkey.Derive([]string{"evt-1"})
	for _, tc := range []struct {
		name string
		run  sandbox.PendingRun
		unit string
	}{
		{name: "a run with a work key",
			run:  sandbox.PendingRun{TurnID: "run-7", WorkKey: "wk-7", AgentHandle: "swe"},
			unit: "wk-7"},
		{name: "a row parked with no work key",
			run:  sandbox.PendingRun{TurnID: derived, AgentHandle: "swe"},
			unit: derived},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e, _ := spendingEngine(t)
			q := e.backends.Queue.(*memory.Queue)
			e.auxSpend = auxspend.NewLedger(q)
			swapModel(t, e, fullBudgetModel{})

			const budget = 4096
			text := strings.Repeat("the coding agent changed the parser and its tests\n", 400)
			cost := map[sandbox.RunPart]sandbox.AuxTokens{}
			for _, part := range sandbox.RunParts {
				got, spent, err := runCondenser{engine: e}.Condense(t.Context(), tc.run, part, text, budget)
				cost[part] = spent
				if err != nil {
					t.Fatalf("%s: condense: %v", part, err)
				}
				if len(got) > budget {
					t.Fatalf("%s: the condensed text is %d bytes with its label, past the "+
						"budget of %d it was given", part, len(got), budget)
				}
				if !strings.HasPrefix(got, compact.Result{Compacted: true, From: len(text)}.Note()) {
					t.Fatalf("%s: the condensed text does not open with its label: %.80q", part, got)
				}
			}
			e.auxSpend.FlushTurn(t.Context(), tc.run.TurnID)

			filed := map[types.AuxPurpose]*types.AuxiliarySpend{}
			for _, ev := range q.History() {
				if ev.Type != (types.AuxiliarySpend{}).EventType() {
					continue
				}
				rec, ok := events.DataAs[*types.AuxiliarySpend](ev)
				if !ok {
					t.Fatalf("an auxiliary_spend event that does not decode: %+v", ev)
				}
				filed[rec.Purpose] = rec
			}
			for _, part := range sandbox.RunParts {
				purpose := types.AuxCondense(string(runPartKind(part)))
				rec := filed[purpose]
				if rec == nil {
					t.Fatalf("%s: no %s record was published; filed %v", part, purpose, filed)
				}
				if rec.Stage != types.AuxStageTurn || rec.TurnID != tc.run.TurnID ||
					rec.WorkKey != tc.unit || rec.AgentHandle != "swe" {
					t.Fatalf("%s: filed as stage=%q turn=%q work_key=%q seat=%q, want the "+
						"turn stage under turn %q, unit %q and seat swe", part, rec.Stage,
						rec.TurnID, rec.WorkKey, rec.AgentHandle, tc.run.TurnID, tc.unit)
				}
				if rec.InputTokens == 0 || rec.OutputTokens == 0 {
					t.Fatalf("%s: the record states no tokens: %+v", part, rec)
				}
				// WHAT IT COST IS HANDED BACK, as the record states it:
				// the coordinator carries it to the segment that resumes
				// from the collection, the only one that can charge the
				// turn's work item for it.
				want := sandbox.AuxTokens{Input: rec.InputTokens, Output: rec.OutputTokens,
					CacheRead: rec.CacheReadTokens, CacheWrite: rec.CacheWriteTokens}
				if cost[part] != want {
					t.Fatalf("%s: the condenser answered a cost of %+v and its record states %+v",
						part, cost[part], want)
				}
			}
		})
	}
}

// fullBudgetModel answers a rewrite with exactly as many bytes as its prompt
// allows — the most a compliant model may — so a caller that asked for more
// than it can carry finds out.
type fullBudgetModel struct{}

func (fullBudgetModel) Model() string { return "rewriter" }

func (fullBudgetModel) Complete(_ context.Context, req llm.Request) (*llm.Completion, error) {
	limit := 0
	for _, m := range req.Messages {
		if m.Role != llm.RoleSystem {
			continue
		}
		at := strings.LastIndex(m.Content, "at most ")
		if at < 0 {
			continue
		}
		if _, err := fmt.Sscanf(m.Content[at:], "at most %d characters", &limit); err != nil {
			return nil, fmt.Errorf("no limit in the rewrite prompt: %w", err)
		}
	}
	if limit <= 0 {
		return nil, fmt.Errorf("the request states no limit")
	}
	return &llm.Completion{Model: "rewriter", Content: strings.Repeat("r", limit),
		InputTokens: 900, OutputTokens: limit / 4}, nil
}
