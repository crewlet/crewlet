package runner_test

import (
	"context"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/fleet"
)

func (c *capture) moves() []*types.LLMFleetMoved {
	c.mu <- struct{}{}
	defer func() { <-c.mu }()
	var out []*types.LLMFleetMoved
	for _, ev := range c.events {
		if got, ok := events.DataAs[*types.LLMFleetMoved](ev); ok {
			out = append(out, got)
		}
	}
	return out
}

func (c *capture) completed(ph types.Phase) *types.AgentPhaseCompleted {
	c.mu <- struct{}{}
	defer func() { <-c.mu }()
	for _, ev := range c.events {
		if got, ok := events.DataAs[*types.AgentPhaseCompleted](ev); ok && got.Phase == ph {
			return got
		}
	}
	return nil
}

// planUsing reads as a plan with a fresh session window at used.
func planUsing(used float64, now time.Time) fleet.Signals {
	return fleet.Signals{Kind: fleet.KindPlan, Readings: []fleet.Reading{{
		LimitReading: llm.LimitReading{Window: llm.LimitSession, State: llm.LimitOK, Used: &used},
		Seen:         now}}}
}

// A PHASE ON AN LLM FLEET SAYS WHERE IT RAN AND WHY: its record carries the
// decision of the member that served it, a pick away from the seat job's home
// is a move between turns, and a member that met its limit mid-call hands the
// call on as a move, never as a provider_fallback, which stays for pinned lists.
func TestAPhaseOnAFleetRecordsItsDecisionAndItsMoves(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 24, 9, 0, 0, 0, time.UTC)
	ledger := fleet.NewLedger(func() time.Time { return now })
	// The seat job's home is executor, 20 points above benched: past the
	// threshold, so the next pick is benched.
	job := fleet.Job{Fleet: "opus", Subject: "cto", Job: "review"}
	ledger.Walk(fleet.Fleet{Key: "opus", Strategy: fleet.Balanced, Threshold: 5, Members: []fleet.Member{
		{Key: "executor", Plan: true, State: fleet.Available, Known: true}}}, job)[0].Take()
	signals := map[string]fleet.Signals{"benched": planUsing(0.10, now), "executor": planUsing(0.30, now)}

	pub := newCapture()
	r, _ := buildWith(t, []phase.Entry{
		{Key: "benched", Provider: benched{model: "benched-model"}},
		{Key: "executor", Provider: reviewing(t)},
	}, buildOpts{pub: pub, reviewChain: org.ProviderKeys{"fleet:opus"},
		fleets: map[string]phase.Fleet{"opus": {Strategy: fleet.Balanced, Threshold: 5,
			Members: []string{"benched", "executor"}}},
		balance: phase.Balance{Ledger: ledger, Signals: func(k string) fleet.Signals { return signals[k] }}})

	if _, err := r.Review(context.Background(), 1, workFor("posted"), nil); err != nil {
		t.Fatalf("Review: %v", err)
	}
	if got := pub.fallbacks(); len(got) != 0 {
		t.Errorf("published %d provider_fallback events on a fleet, want moves only", len(got))
	}
	moves := pub.moves()
	if len(moves) < 2 {
		t.Fatalf("published %d moves, want the pick away from home and the mid-call hand-off", len(moves))
	}
	between, midCall := moves[0], moves[1]
	if between.From != "executor" || between.To == nil || *between.To != "benched" ||
		between.Work != "Between turns: its next turn started on the new member" ||
		between.Why != "Balanced to the least-used member" || between.TurnID != "t-1" || between.Job != "review" {
		t.Errorf("first move = %+v, want executor to benched between turns, balanced", between)
	}
	if midCall.From != "benched" || midCall.To == nil || *midCall.To != "executor" ||
		midCall.Why != "Reached its usage limit" {
		t.Errorf("second move = %+v, want benched to executor for its limit", midCall)
	}
	done := pub.completed(types.PhaseReview)
	if done == nil {
		t.Fatal("no review record")
	}
	if done.ProviderKey != "executor" || done.Fleet != "opus" || done.Strategy != "balanced" ||
		done.FleetReason != string(fleet.ReasonLimitHandover) || done.ShareAtPick == nil || *done.ShareAtPick != 30 {
		t.Errorf("review record decision = %s on %q (%s, %s, share %v), want executor on opus by limit_handover at 30",
			done.ProviderKey, done.Fleet, done.Strategy, done.FleetReason, done.ShareAtPick)
	}
	for _, inv := range done.Invocations {
		if inv.ProviderKey == "executor" && inv.Fleet != "opus" {
			t.Errorf("an invocation served by executor carries no decision: %+v", inv)
		}
	}
	if home := ledger.Home(job); home != "executor" {
		t.Errorf("home after the hand-off = %q, want executor, the member that answered", home)
	}
}
