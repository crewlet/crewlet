package builtin_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/mcp"
	llm "github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/tools"
)

// launchSpy records what the tool asked for.
type launchSpy struct {
	turns  []string
	briefs []string
	err    error
}

func (s *launchSpy) Launch(_ context.Context, turn *turnctx.Turn, brief string) (sandbox.LaunchResult, error) {
	if s.err != nil {
		return sandbox.LaunchResult{}, s.err
	}
	s.turns = append(s.turns, turn.ID)
	s.briefs = append(s.briefs, brief)
	return sandbox.LaunchResult{SandboxID: "box-1", CommandID: "cmd-1", CodingAgent: "claude-code"}, nil
}

// sandboxSurface builds an Execute surface offering run_sandbox, bound to a
// turn — which is how the runner builds one.
func sandboxSurface(t *testing.T, launcher builtin.SandboxLauncher) *tools.Surface {
	t.Helper()
	reg := tools.NewRegistry()
	if _, err := builtin.Register(reg, builtin.Deps{Sandbox: launcher}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	surface := tools.NewSurface("execute", reg.Snapshot(), []string{builtin.RunSandboxTool})
	return surface.ForTurn(turnFor(t, "agent-ceo"))
}

func TestRunSandboxIsOmittedWithoutALauncher(t *testing.T) {
	reg := tools.NewRegistry()
	names, err := builtin.Register(reg, builtin.Deps{})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	for _, name := range names {
		if name == builtin.RunSandboxTool {
			t.Fatal("run_sandbox was registered with no launcher — a seat would plan around a box it cannot start")
		}
	}
}

func TestRunSandboxIsRegisteredWithALauncher(t *testing.T) {
	reg := tools.NewRegistry()
	names, err := builtin.Register(reg, builtin.Deps{Sandbox: &launchSpy{}})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if !slices.Contains(names, builtin.RunSandboxTool) {
		t.Fatalf("run_sandbox was not registered; got %v", names)
	}
}

// A coding run pushes branches and opens pull requests, so a sub-agent acting
// under its parent's identity must not reach it.
func TestRunSandboxIsClassifiedAsWritingToASharedSurface(t *testing.T) {
	reg := tools.NewRegistry()
	if _, err := builtin.Register(reg, builtin.Deps{Sandbox: &launchSpy{}}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	entry, ok := reg.Snapshot().Lookup(builtin.RunSandboxTool)
	if !ok {
		t.Fatal("run_sandbox is not in the registry")
	}
	if !mcp.WritesToSharedSurface(entry.Annotations) {
		t.Fatalf("run_sandbox reads as private: %+v", entry.Annotations)
	}
}

func TestALaunchedRunSuspendsTheLoop(t *testing.T) {
	spy := &launchSpy{}
	surface := sandboxSurface(t, spy)

	res, err := surface.Execute(t.Context(), llm.ToolCall{ID: "c1", Name: builtin.RunSandboxTool, Arguments: map[string]any{
		"brief": "Clone example.com/acme/api and fix the failing test",
	}})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !res.Suspend {
		t.Fatal("a launched coding job did not suspend the loop — the turn would end believing the work was done")
	}
	if res.Failed {
		t.Fatalf("a successful launch reported failure: %q", res.Output)
	}
	if res.SuspendPayload["sandbox_id"] != "box-1" {
		t.Fatalf("payload = %v, want the box it started", res.SuspendPayload)
	}
	if len(spy.briefs) != 1 || !strings.Contains(spy.briefs[0], "acme/api") {
		t.Fatalf("briefs = %v", spy.briefs)
	}
	// THE TURN COMES FROM THE SURFACE, never from the arguments: a model
	// that spelled a different id could otherwise start a box against
	// somebody else's suspended conversation.
	if len(spy.turns) != 1 || spy.turns[0] != "wk-1" {
		t.Fatalf("turns = %v, want the calling turn", spy.turns)
	}
}

// A launch the engine could not do is a tool failure the model can react to,
// not an engine error that loses the turn.
func TestAFailedLaunchIsAToolFailureAndDoesNotSuspend(t *testing.T) {
	surface := sandboxSurface(t, &launchSpy{err: errors.New("no sandbox capacity")})

	res, err := surface.Execute(t.Context(), llm.ToolCall{ID: "c1", Name: builtin.RunSandboxTool, Arguments: map[string]any{
		"brief": "fix the flake",
	}})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Suspend {
		t.Fatal("a failed launch suspended the loop — nothing would ever resume it")
	}
	if !res.Failed || !strings.Contains(res.Output, "no sandbox capacity") {
		t.Fatalf("result = %+v, want a failure naming the reason", res)
	}
}

func TestAnEmptyBriefIsRefusedWithoutLaunching(t *testing.T) {
	spy := &launchSpy{}
	surface := sandboxSurface(t, spy)

	res, err := surface.Execute(t.Context(), llm.ToolCall{ID: "c1", Name: builtin.RunSandboxTool, Arguments: map[string]any{"brief": "   "}})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Suspend || !res.Failed {
		t.Fatalf("result = %+v, want a refusal", res)
	}
	if len(spy.briefs) != 0 {
		t.Fatal("an empty brief still started a box")
	}
}

// A surface that ran this tool without the detached seam would have the turn
// end believing the work was done while the job was still running.
func TestTheNonDetachedPathRefuses(t *testing.T) {
	reg := tools.NewRegistry()
	if _, err := builtin.Register(reg, builtin.Deps{Sandbox: &launchSpy{}}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	entry, _ := reg.Snapshot().Lookup(builtin.RunSandboxTool)
	res, err := entry.Tool.Call(t.Context(), map[string]any{"brief": "fix it"})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !res.Failed {
		t.Fatal("the non-detached path ran the job and answered normally")
	}
	if !strings.Contains(res.Output, "suspend") {
		t.Fatalf("the refusal does not say why: %q", res.Output)
	}
}
