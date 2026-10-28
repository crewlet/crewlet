package engine

import (
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	"github.com/crewlet/crewlet/internal/org"
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

// A RECOLLECTED ENDING BENCHES NOTHING ITS FIRST READING DID NOT. The segment
// that collects a job reads the job's ending every time, and a completion held
// on a limit is collected again when its hold lifts: the ending the entry is
// handed is dated by the job's first collection, so a report whose window is
// over sits nothing out, and the review the hold waited for reaches the model.
// Dated by the reading, every collection benched the entry afresh, before any
// call could find out whether the quota was back, and held the completion
// again for longer.
//
// Mutation: leave At out of what noteRunEnded hands the entry, and the
// hour-old report benches the entry again.
func TestARecollectedEndingBenchesItsEntryNoMore(t *testing.T) {
	t.Parallel()
	c, _ := chainCompany(t, chainMember{key: "plan", agent: "opencode", model: "anthropic/claude-sonnet-5"})
	run := sandbox.PendingRun{TurnID: "t0", AgentHandle: "swe",
		Launch: sandbox.LaunchRecord{ProviderKey: "plan"}}
	quota := sandbox.RunEnding{Error: `{"type":"provider.quota","message":"You have exhausted your quota"}`,
		At: time.Now().Add(-time.Hour)}
	e := &Engine{}

	e.noteRunEnded(t.Context(), c, run, quota)
	if err := planOf(t, c).SittingOut(); err != nil {
		t.Fatalf("an ending first collected an hour ago sat its entry out again: %v", err)
	}
	quota.At = time.Now()
	e.noteRunEnded(t.Context(), c, run, quota)
	if planOf(t, c).SittingOut() == nil {
		t.Error("the same report, first collected now, sits nothing out: the case proves nothing")
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

// A RESUME WAITS ON THE CHAIN ITS FIRST CALL TAKES. A completion's first model
// call is its reviewer's, and an answer's is the relaunch that carries its run
// on, so a seat whose executor sits out a spent plan while its own reviewer
// answers has a finished run to review and deliver: its completion is resumed
// at once, and an answer, whose relaunch would find every runner sitting out,
// is held at the gate until the first is tried again. Spend the reviewer
// instead and the two swap. Each is asked where it is asked: an answer at the
// turn gate, before its claim, and a completion once its job is collected
// (collectedRefusal), the gate admitting it to be collected first.
//
// Mutation: ask the executor chain for both routes, and the completion of the
// seat whose reviewer answers waits on its executor's limit.
func TestAResumeWaitsOnTheChainItsFirstCallTakes(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		spent              string
		completion, answer bool
	}{
		"its executor spent": {spent: "plan", answer: true},
		"its reviewer spent": {spent: "review", completion: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			c, seat := chainCompany(t,
				chainMember{key: "plan", agent: "claude-code", model: "opus", spent: tc.spent == "plan"},
				chainMember{key: "review", agent: "claude-code", model: "haiku", spent: tc.spent == "review"},
			)
			seat.LLM, seat.LLMReview = org.ProviderKeys{"plan"}, org.ProviderKeys{"review"}
			e := launchReadyEngine(t, c)
			// THE ANSWER FIRST: a completion refused once collected parks the
			// seat, and a park refuses every resume at the gate.
			for _, route := range []sandbox.ResumeRoute{sandbox.ResumeAnswer, sandbox.ResumeCompletion} {
				want := map[sandbox.ResumeRoute]bool{
					sandbox.ResumeCompletion: tc.completion, sandbox.ResumeAnswer: tc.answer,
				}[route]
				refusal, refused := e.seatCannotRun(t.Context(), "swe", route)
				if route == sandbox.ResumeCompletion {
					if refused {
						t.Errorf("a completion's gate on a seat with %s refused it before its "+
							"collection: %+v", name, refusal)
					}
					refusal, refused = e.collectedRefusal(t.Context(), e.Company(), "swe")
				}
				if refused != want {
					t.Errorf("a %s's resume on a seat with %s: refused = %v (%+v), want %v",
						route, name, refused, refusal, want)
					continue
				}
				if refused && (refusal.Condition != waitProvider || refusal.Until.IsZero() ||
					!strings.Contains(refusal.Reason, tc.spent)) {
					t.Errorf("a %s's resume was refused as %+v, want it waiting on %q for %s's retry",
						route, refusal, waitProvider, tc.spent)
				}
			}
		})
	}
}
