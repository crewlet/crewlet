package runner_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/execstate"
	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/prompts"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/agent/skills"
	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/toolname"
	"github.com/crewlet/crewlet/internal/tools"
)

// approvalTool is an MCP server's tool at Needs approval: it names the
// definition a person approved and counts the calls made to it.
type approvalTool struct {
	mu    sync.Mutex
	calls int
}

func (a *approvalTool) Name() string               { return "tr_close_issue" }
func (a *approvalTool) Description() string        { return "Close an issue." }
func (a *approvalTool) Parameters() map[string]any { return map[string]any{"type": "object"} }
func (a *approvalTool) Digest() string             { return "sha256:close" }
func (a *approvalTool) Call(context.Context, map[string]any) (tools.Result, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls++
	return tools.Result{Output: "closed #7"}, nil
}

func (a *approvalTool) made() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.calls
}

// firstCommit marks a held call on its row as the store does: the first mark
// makes the call, and its result is recorded once. Every attempt at the answer
// shares it, and each attempt's claim reads the row as it stands then.
type firstCommit struct {
	mu     sync.Mutex
	marked bool
	result *sandbox.HeldResult
	// kept is the result as the row seals it, which the answer's lead keeps.
	kept *sandbox.HeldKept
}

// prior is how the row holds the call once some attempt marked it.
func (c *firstCommit) prior() runner.HeldPrior {
	if c.result != nil {
		return runner.HeldPrior{Made: true, Result: *c.result, Kept: c.kept}
	}
	return runner.HeldPrior{}
}

func (c *firstCommit) Marked(context.Context) (runner.HeldPrior, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.marked {
		return runner.HeldPrior{}, false
	}
	return c.prior(), true
}

func (c *firstCommit) Begin(context.Context) (runner.HeldPrior, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.marked {
		c.marked = true
		return runner.HeldPrior{First: true}, nil
	}
	return c.prior(), nil
}

func (c *firstCommit) Record(_ context.Context, result sandbox.HeldResult,
	_ toolloop.ToolResult) (sandbox.HeldKept, error) {

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.result == nil {
		c.result = &result
		c.kept = &sandbox.HeldKept{Result: "sealed: " + result.Output, ID: "held-1"}
	}
	return *c.kept, nil
}

// heldCarry is a runner carrying on a run that parked on a call to
// `tr_close_issue`, a tool at Needs approval a person allowed once, from the
// asking job's record — state, and the calls the round made over the bridge —
// where the required skill `tracker-rules` covers that tool; and the tool, the
// commit marking the call on the run's row, the launcher the run is relaunched
// through and what the turn published.
func heldCarry(t *testing.T, state execstate.State, bridged []runner.BridgedCall) (*runner.Runner,
	*approvalTool, *firstCommit, *playLauncher, *capture) {

	t.Helper()
	tool, commit := &approvalTool{}, &firstCommit{}
	r, launcher, pub := heldAttempt(t, state, bridged, true, tool, commit)
	return r, tool, commit, launcher, pub
}

// heldAttempt is one attempt at the answer [heldCarry]'s runner carries the run
// on with, over the tool and the commit every attempt at that answer shares,
// where `tracker-rules` is required — or, where required is false, a skill the
// seat may load and is not held to.
func heldAttempt(t *testing.T, state execstate.State, bridged []runner.BridgedCall, required bool,
	tool *approvalTool, commit *firstCommit) (*runner.Runner, *playLauncher, *capture) {

	t.Helper()
	registry := skills.NewRegistry()
	if _, err := registry.PutPage(skills.Skill{
		Key: "tracker-rules", Title: "Tracker rules", Summary: "how this company closes an issue",
		Body:    "Close an issue with the reason it is done.",
		Trigger: skills.Trigger{Tool: "tr_close_issue"}, Required: required,
		SourcePageID: "tracker-rules-page",
	}); err != nil {
		t.Fatalf("PutPage: %v", err)
	}
	models, err := phase.NewRegistry([]phase.Entry{{Key: "default", Provider: &scriptedProvider{}}})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	reg := tools.NewRegistry()
	if err := reg.RegisterWith(tool, tools.Origin("tracker"), tools.Annotations{},
		func(e *tools.Entry) { e.NeedsApproval = true }); err != nil {
		t.Fatalf("RegisterWith: %v", err)
	}
	// THE REAL LOADER, whose successful call is what a rebuilt guard reads a
	// load off ([guardedRunner] says why a stub would prove nothing).
	if _, err := builtin.Register(reg, builtin.Deps{ToolSkills: registry,
		Authorize: builtin.Decide(authz.NoChart{})}); err != nil {
		t.Fatalf("builtin.Register: %v", err)
	}
	role := &org.Role{Name: "CTO", DeclaredHandle: "cto"}
	chart := &org.Organization{Name: "Acme", Roles: []*org.Role{role}}
	pub := newCapture()
	launcher := &playLauncher{}
	r, err := runner.New(runner.Config{
		Seat:     prompts.Seat{Org: chart, Role: role},
		Registry: reg, Models: models, Skills: registry,
		Launcher:  launcher,
		Task:      "close the issue",
		Publisher: pub,
		Turn: runner.Turn{RunID: "t-held", WorkKey: "wk-t-held", AgentID: "agent-1",
			Context: &turnctx.Turn{RunID: "t-held", WorkKey: "wk-t-held", Seat: role, Org: chart}},
		Sessions: sessions,
		Resume: &runner.Resume{
			State: state, Bridged: bridged,
			Asked: &runner.Asked{Question: "close #7?", Answer: "yes", By: "founder",
				SessionID: "sess-ask", BoxKept: true,
				Held: &runner.HeldCall{Tool: "tr_close_issue", Server: "tracker", Digest: "sha256:close",
					Args: map[string]any{"id": "7"}, Choice: sandbox.HeldOnce, Commit: commit}},
		},
	})
	if err != nil {
		t.Fatalf("runner.New: %v", err)
	}
	return r, launcher, pub
}

// heldState is the asking job's parked state: the round, and the tools its
// launch had active.
func heldState() execstate.State {
	return execstate.State{Version: execstate.Version, Round: 1,
		ActiveTools: []string{"tr_close_issue", toolname.LoadToolSkill}}
}

// skillLoad is the call loading `tracker-rules` over the bridge, the round's
// first.
func skillLoad() runner.BridgedCall {
	return runner.BridgedCall{Call: ledger.Call{Name: toolname.LoadToolSkill,
		Args: map[string]any{"key": "tracker-rules"}, Result: "Close an issue with the reason it is done."},
		Seq: 1}
}

// AN APPROVED CALL IS MADE UNDER THE GUARD OF THE SESSION THAT ASKED IT. A run
// whose required skill covers a tool at Needs approval loads the skill over the
// bridge, calls the tool, and parks on the person's approval; the answer's
// resume makes the call once, through the surface the run carries on with, as
// the asking session's guard holds it — the skill its own job loaded — before
// the relaunch empties the guard for whichever session its launch decides. The
// call runs, its result reaches the relaunched run, and no refusal is published.
//
// Mutation: empty the guard for the relaunch before the held call is made, and
// the approved call is refused as covered by a skill never loaded, recorded as
// made, and never run.
func TestAnApprovedCallIsMadeUnderTheGuardOfTheSessionThatAskedIt(t *testing.T) {
	t.Parallel()
	// THE ASKING JOB'S OWN LOAD, made over the bridge before it called the
	// tool the person was asked about.
	r, tool, _, launcher, pub := heldCarry(t, heldState(), []runner.BridgedCall{skillLoad()})
	if _, _, err := r.Resume(context.Background(), nil); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if made := tool.made(); made != 1 {
		t.Fatalf("the approved call ran %d time(s), want once", made)
	}
	again := launcher.last(t)
	if again.Continue == nil || !strings.Contains(again.Continue.Brief, "returned:") ||
		!strings.Contains(again.Continue.Brief, "closed #7") ||
		strings.Contains(again.Continue.Brief, "reported a failure") {
		t.Fatalf("the relaunched run is told %+v, want what the approved call returned", again.Continue)
	}
	if blocked := pub.blocked(); len(blocked) != 0 {
		t.Fatalf("the guard refused %d call(s) of the resume, want none: %+v", len(blocked), blocked[0])
	}
}

// AN APPROVED CALL THE GUARD WOULD REFUSE IS NOT MADE, AND ITS APPROVAL IS NOT
// USED UP. The guard the call is made under is the one the rebuild armed for
// the asking session, which need not be the guard that held it: a skill the
// asking session loaded in an earlier job is the launch's armed skill, held
// only where the job's box said it took that session up — and a box whose
// word never reached the job's record is rebuilt without it — and a skill made
// required while the call waited covers it now. Either way the guard is asked
// before the call is marked on the run's row: the call is not made, the mark
// is never written, and the relaunched run is told why — it can load the skill
// and call again, which asks the person afresh.
//
// Mutation: ask the guard only as the call runs, after the mark, and the call
// is marked, its refusal recorded as its result, and the run told the call
// was made.
func TestAnApprovedCallTheGuardWouldRefuseIsNotMade(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		state   func() execstate.State
		bridged []runner.BridgedCall
	}{
		// THE SKILL LOADED BY AN EARLIER JOB OF THE SESSION, which the asking
		// job's launch armed its guard with: its load is a call before the
		// asking job opened (BridgeFrom), and the job's record says nothing
		// of which session its box started.
		"an earlier job's load, its box's word never recorded": {
			state: func() execstate.State {
				s := heldState()
				s.LoadedSkills, s.BridgeFrom = []string{"tracker-rules"}, 1
				return s
			},
			bridged: []runner.BridgedCall{skillLoad()},
		},
		// THE SKILL MADE REQUIRED WHILE THE CALL WAITED: the asking session
		// never loaded it, and nothing held the call back for it then.
		"a skill required since the call was held": {state: heldState},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r, tool, commit, launcher, _ := heldCarry(t, tc.state(), tc.bridged)
			if _, _, err := r.Resume(t.Context(), nil); err != nil {
				t.Fatalf("Resume: %v", err)
			}
			if made := tool.made(); made != 0 {
				t.Fatalf("the approved call ran %d time(s) under a guard that refuses it, want none", made)
			}
			commit.mu.Lock()
			marked := commit.marked
			commit.mu.Unlock()
			if marked {
				t.Fatal("the call was marked made on the run's row, using the approval up on a call that " +
					"never ran")
			}
			again := launcher.last(t)
			if again.Continue == nil || !strings.Contains(again.Continue.Brief, "The call was not made") ||
				!strings.Contains(again.Continue.Brief, "tracker-rules") ||
				strings.Contains(again.Continue.Brief, "The call was made") {
				t.Fatalf("the relaunched run is told %+v, want the call not made, naming the skill", again.Continue)
			}
		})
	}
}

// AN APPROVED CALL AN EARLIER ATTEMPT MADE IS ANSWERED FROM ITS MARK, WHATEVER
// THE GUARD SAYS NOW. The first attempt at the answer makes the call under a
// guard that admits it, and its relaunch is refused after, as the floor of the
// session it continues refuses one, so the answer is attempted again. Meanwhile
// the skill covering the tool is made required, and the second attempt's
// rebuilt guard would refuse the call. The call is not made again, and the run
// that carries the work on is handed what it returned: never told the call was
// not made, which would send it to ask the person again for a call that was
// made, and approved a second time, make it twice.
//
// Mutation: ask the guard before the mark answers the call, and the second
// attempt tells the run the call was not made, its result lost.
func TestAnApprovedCallAnEarlierAttemptMadeIsAnsweredFromItsMark(t *testing.T) {
	t.Parallel()
	tool, commit := &approvalTool{}, &firstCommit{}
	first, refused, _ := heldAttempt(t, heldState(), nil, false, tool, commit)
	refused.err = fmt.Errorf("the launch is below its continued session's floor: %w", toolloop.ErrBudgetExhausted)
	if _, _, err := first.Resume(t.Context(), nil); err == nil {
		t.Fatal("the first attempt's relaunch was refused, and its resume reported nothing")
	}
	if made := tool.made(); made != 1 {
		t.Fatalf("the first attempt made the approved call %d time(s), want once", made)
	}

	// THE SKILL MADE REQUIRED before the answer is attempted again: nothing in
	// the round loaded it, so the rebuilt guard holds the tool back.
	again, launcher, _ := heldAttempt(t, heldState(), nil, true, tool, commit)
	if _, _, err := again.Resume(t.Context(), nil); err != nil {
		t.Fatalf("the second attempt's Resume: %v", err)
	}
	if made := tool.made(); made != 1 {
		t.Fatalf("the approved call ran %d time(s) over both attempts, want once", made)
	}
	relaunched := launcher.last(t)
	if relaunched.Continue == nil || !strings.Contains(relaunched.Continue.Brief, "The call was made, and returned:") ||
		!strings.Contains(relaunched.Continue.Brief, "closed #7") ||
		strings.Contains(relaunched.Continue.Brief, "The call was not made") {
		t.Fatalf("the relaunched run is told %+v, want what the first attempt's call returned", relaunched.Continue)
	}
}
