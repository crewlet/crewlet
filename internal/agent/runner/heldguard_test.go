package runner_test

import (
	"context"
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
// makes the call, and its result is recorded once.
type firstCommit struct {
	mu     sync.Mutex
	marked bool
	result *sandbox.HeldResult
}

func (c *firstCommit) Begin(context.Context) (runner.HeldPrior, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.marked {
		c.marked = true
		return runner.HeldPrior{First: true}, nil
	}
	if c.result != nil {
		return runner.HeldPrior{Made: true, Result: *c.result}, nil
	}
	return runner.HeldPrior{}, nil
}

func (c *firstCommit) Record(_ context.Context, result sandbox.HeldResult, _ toolloop.ToolResult) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.result == nil {
		c.result = &result
	}
	return nil
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
	registry := skills.NewRegistry()
	if _, err := registry.PutPage(skills.Skill{
		Key: "tracker-rules", Title: "Tracker rules", Summary: "how this company closes an issue",
		Body:    "Close an issue with the reason it is done.",
		Trigger: skills.Trigger{Tool: "tr_close_issue"}, Required: true,
		SourcePageID: "tracker-rules-page",
	}); err != nil {
		t.Fatalf("PutPage: %v", err)
	}
	models, err := phase.NewRegistry([]phase.Entry{{Key: "default", Provider: &scriptedProvider{}}})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	reg := tools.NewRegistry()
	tool := &approvalTool{}
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
			State: execstate.State{Version: execstate.Version, Round: 1,
				ActiveTools: []string{"tr_close_issue", toolname.LoadToolSkill}},
			// THE ASKING JOB'S OWN LOAD, made over the bridge before it called
			// the tool the person was asked about.
			Bridged: []runner.BridgedCall{{Call: ledger.Call{Name: toolname.LoadToolSkill,
				Args: map[string]any{"key": "tracker-rules"}, Result: "Close an issue with the reason it is done."},
				Seq: 1}},
			Asked: &runner.Asked{Question: "close #7?", Answer: "yes", By: "founder",
				SessionID: "sess-ask", BoxKept: true,
				Held: &runner.HeldCall{Tool: "tr_close_issue", Server: "tracker", Digest: "sha256:close",
					Args: map[string]any{"id": "7"}, Choice: sandbox.HeldOnce, Commit: &firstCommit{}}},
		},
	})
	if err != nil {
		t.Fatalf("runner.New: %v", err)
	}
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
