package engine

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	"github.com/crewlet/crewlet/internal/providers/llm/chain"
	"github.com/crewlet/crewlet/internal/providers/llm/cliagent"
	"github.com/crewlet/crewlet/internal/runtoken"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// A RUN THAT ENDED ON ITS VENDOR'S SPENT PLAN MOVES THE TURN'S NEXT LAUNCH ON.
// The run is its CLI's own, so nothing a text call reads saw the limit; the
// segment that collects it reads the run's own ending, sits the entry out on
// this node, and the turn's next launch — a reviewer's send-back — takes the
// chain's next member, announced as the hand-off it is. A run that ended any
// other way leaves the entry in place.
//
// THE ENDING AS COLLECTED, its terminal event included: Claude Code 2.1.312
// says a spent plan in words no sentinel matches, and as a status on its
// result line, which is all that reads it.
//
// Mutation: leave the event out of what noteRunEnded hands the entry, and the
// next launch walks into the spent plan.
func TestARunThatSpentItsPlanMovesTheNextLaunchOn(t *testing.T) {
	t.Parallel()
	c, seat := chainCompany(t,
		chainMember{key: "plan", agent: "claude-code", model: "opus"},
		chainMember{key: "spare", agent: "claude-code", model: "haiku"},
	)
	e, jobs := launchRig(t, c, bridgeFor(t, mcpbridge.Options{
		Material: runtoken.OneKey("k", "test-key"), BaseURL: "https://engine.example.com",
	}))
	run := sandbox.PendingRun{TurnID: "t0", AgentHandle: "swe",
		Launch: sandbox.LaunchRecord{ProviderKey: "plan"}}

	e.noteRunEnded(t.Context(), c, run, sandbox.RunEnding{Success: true, Text: "fixed the flake"})
	if err := planOf(t, c).SittingOut(); err != nil {
		t.Fatalf("a run that ended on its answer sat its entry out: %v", err)
	}

	const said = "You've hit your 5-hour limit"
	e.noteRunEnded(t.Context(), c, run, sandbox.RunEnding{Error: said, Text: said,
		Event: `{"type":"result","is_error":true,"api_error_status":429,"result":"` + said + `"}`,
		At:    time.Now()})
	l := launcherOn(e, c, seat, "t1")
	var handoffs []chain.Fallback
	if _, err := l.LaunchExecutor(t.Context(), launchRequest(l, &handoffs)); err != nil {
		t.Fatalf("LaunchExecutor: %v", err)
	}
	if started := jobs.Started(); len(started) != 1 || started[0].Model != "haiku" {
		t.Fatalf("started %+v, want the next launch on the spare entry", started)
	}
	if len(handoffs) != 1 || handoffs[0].From != "plan" || handoffs[0].To != "spare" {
		t.Errorf("hand-offs = %+v, want one from the spent entry to the spare", handoffs)
	}
}

// AN APPLY KEEPS A SPENT PLAN SPENT. Every apply rebuilds every entry, so an
// edit to anything else handed the node's seats the spent entry back, and each
// relearned the window with a call or a launch. An entry whose spec is
// unchanged keeps its predecessor's sit-out; one whose spec moved starts clean.
func TestAnApplyKeepsASpentPlanSpent(t *testing.T) {
	t.Parallel()
	before, _ := chainCompany(t, chainMember{key: "plan", agent: "claude-code", model: "opus"})
	if !planOf(t, before).RunEnded(t.Context(), cliagent.RunEnd{Answer: "Usage limit reached · resets 5pm"}) {
		t.Fatal("the premise: the old entry sits out")
	}
	same, _ := chainCompany(t, chainMember{key: "plan", agent: "claude-code", model: "opus"})
	carrySitOuts(before, same)
	if planOf(t, same).SittingOut() == nil {
		t.Error("the rebuilt entry with an unchanged spec was handed back the spent plan")
	}
	moved, _ := chainCompany(t, chainMember{key: "plan", agent: "claude-code", model: "haiku"})
	carrySitOuts(before, moved)
	if err := planOf(t, moved).SittingOut(); err != nil {
		t.Errorf("an entry whose spec moved inherited the old one's sit-out: %v", err)
	}
}

// planOf is c's "plan" entry, as the coding CLI it is.
func planOf(t *testing.T, c *Company) *cliagent.Provider {
	t.Helper()
	p, ok := c.Models.Provider("plan")
	if !ok {
		t.Fatal("no plan entry")
	}
	return p.(*cliagent.Provider)
}
