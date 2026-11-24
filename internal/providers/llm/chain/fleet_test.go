package chain

import (
	"context"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/fleet"
)

// fleetPicks is a balanced fleet's walk over plans a and b for one seat job,
// on ledger.
func fleetPicks(ledger *fleet.Ledger, job fleet.Job) []*fleet.Pick {
	f := fleet.Fleet{Key: "opus", Strategy: fleet.Balanced, Threshold: 5, Members: []fleet.Member{
		{Key: "a", Plan: true, State: fleet.Available, Share: 10, Known: true},
		{Key: "b", Plan: true, State: fleet.Available, Share: 30, Known: true},
	}}
	return ledger.Walk(f, job)
}

// A FLEET'S CHAIN RECORDS WHO TOOK THE WORK: each member's pick is taken as it
// takes the call, so the seat job's home is the member that answered, and a
// hand-off carries both picks, so a fleet's move is told from a fallback.
func TestAFleetChainTakesThePickOfTheMemberThatTakesTheCall(t *testing.T) {
	t.Parallel()
	ledger := fleet.NewLedger(func() time.Time { return time.Date(2026, 11, 24, 9, 0, 0, 0, time.UTC) })
	job := fleet.Job{Fleet: "opus", Subject: "engineer", Job: "review"}
	picks := fleetPicks(ledger, job)
	var handoffs []Fallback
	c := build(t, Options{Fleet: picks, OnFallback: func(f Fallback) { handoffs = append(handoffs, f) }},
		member("a", failing("opus", llm.KindRateLimit)), member("b", answering("opus", "done")))
	got, err := c.Complete(context.Background(), llm.Request{})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if got.ProviderKey != "b" || ledger.Home(job) != "b" {
		t.Errorf("answered by %q, home %q; want b for both", got.ProviderKey, ledger.Home(job))
	}
	if len(handoffs) != 1 || handoffs[0].FromFleet != picks[0] || handoffs[0].ToFleet != picks[1] {
		t.Fatalf("hand-offs = %+v, want one from a's pick to b's", handoffs)
	}
	if !ledger.LastServed("a").Equal(ledger.LastServed("b")) || ledger.LastServed("a").IsZero() {
		t.Error("both members took the call, so both carry its serve")
	}
}

// A pinned chain has no picks, and its hand-offs carry none.
func TestAPinnedChainHandsOffWithNoPicks(t *testing.T) {
	t.Parallel()
	var handoffs []Fallback
	c := build(t, Options{OnFallback: func(f Fallback) { handoffs = append(handoffs, f) }},
		member("a", failing("m", llm.KindTimeout)), member("b", answering("m", "x")))
	if _, err := c.Complete(context.Background(), llm.Request{}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if len(handoffs) != 1 || handoffs[0].FromFleet != nil || handoffs[0].ToFleet != nil {
		t.Errorf("hand-offs = %+v, want one with no fleet picks", handoffs)
	}
}

// Picks name members index for index: a count that differs is a wiring fault.
func TestNewRefusesPicksThatDoNotMatchTheMembers(t *testing.T) {
	t.Parallel()
	picks := fleetPicks(fleet.NewLedger(nil), fleet.Job{Fleet: "opus"})
	if _, err := New([]Member{member("a", answering("m", "x"))}, Options{Fleet: picks}); err == nil {
		t.Error("New with two picks for one member = nil error")
	}
}
