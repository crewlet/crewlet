package builtin_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/coord"
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
	s.turns = append(s.turns, turn.RunID)
	s.briefs = append(s.briefs, brief)
	return sandbox.LaunchResult{SandboxID: "box-1", CommandID: "cmd-1", CodingAgent: "claude-code"}, nil
}

// sandboxSurface builds an Execute surface offering run_sandbox, bound to a
// turn — which is how the runner builds one.
func sandboxSurface(t *testing.T, launcher builtin.SandboxLauncher) *tools.Surface {
	t.Helper()
	reg := tools.NewRegistry()
	if _, err := builtin.Register(reg, gated(builtin.Deps{Sandbox: launcher})); err != nil {
		t.Fatalf("Register: %v", err)
	}
	surface := tools.NewSurface("execute", reg.Snapshot(), []string{builtin.RunSandboxTool})
	return surface.ForTurn(turnFor(t, "agent-ceo"))
}

func TestRunSandboxIsOmittedWithoutALauncher(t *testing.T) {
	reg := tools.NewRegistry()
	names, err := builtin.Register(reg, gated(builtin.Deps{}))
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
	names, err := builtin.Register(reg, gated(builtin.Deps{Sandbox: &launchSpy{}}))
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
	if _, err := builtin.Register(reg, gated(builtin.Deps{Sandbox: &launchSpy{}})); err != nil {
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
	//
	// And it is the RUN, because the pending row is keyed on it: two runs
	// of one redelivered trigger would otherwise share one row and one box.
	// See ADR-0017.
	if len(spy.turns) != 1 || spy.turns[0] != "run-1" {
		t.Fatalf("turns = %v, want the calling turn", spy.turns)
	}
}

// A launch the engine could not do is a tool failure the model can react to,
// not an engine error that loses the turn — and one nothing classed is told as
// the fault it is, never in the error's own words.
func TestAFailedLaunchIsAToolFailureAndDoesNotSuspend(t *testing.T) {
	surface := sandboxSurface(t, &launchSpy{err: errors.New("sandbox: recording the run: driver said no")})

	res, err := surface.Execute(t.Context(), llm.ToolCall{ID: "c1", Name: builtin.RunSandboxTool, Arguments: map[string]any{
		"brief": "fix the flake",
	}})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Suspend {
		t.Fatal("a failed launch suspended the loop — nothing would ever resume it")
	}
	if !res.Failed || res.Refusal() != tools.RefusalInternalError {
		t.Fatalf("result = %+v (%s), want a fault of this node", res, res.Refusal())
	}
	if strings.Contains(res.Output, "driver said no") || strings.Contains(res.Output, "sandbox:") {
		t.Fatalf("the model was handed the error's own text: %q", res.Output)
	}
	if !strings.Contains(res.Output, "No coding run was started") {
		t.Fatalf("the failure does not say no run started: %q", res.Output)
	}
}

// EVERY CLASS A LAUNCH CAN FAIL WITH IS TOLD IN THIS TOOL'S WORDS, with a wait
// offered exactly where one helps. The launcher's error is the log's: it names
// hosts, statuses, a runtime's stderr and configuration paths, none of which a
// model can act on.
//
// It used to be "the sandbox could not be started: " and the error, marked as
// one a wait clears whatever it was, so a seat whose setup a person has to fix
// was told to try again for as long as its turn lasted.
func TestEveryLaunchClassIsWorded(t *testing.T) {
	const detail = "e2b: POST /sandboxes: 401: key sk-not-real rejected at api.example.com"
	cases := map[sandbox.Why]struct {
		class tools.Refusal
		says  string
	}{
		sandbox.WhyConfiguration:      {tools.RefusalUnavailable, "an administrator needs to fix"},
		sandbox.WhySetupUnavailable:   {tools.RefusalUnavailable, "sandbox setup makes failed for now"},
		sandbox.WhyOwnSession:         {tools.RefusalUnavailable, "shell and editor you already have"},
		sandbox.WhyBudget:             {tools.RefusalBudgetExhausted, "token budget"},
		sandbox.WhyAwaitingCredential: {tools.RefusalUnavailable, "Try again in a few minutes"},
		sandbox.WhyCredentialUnavailable: {tools.RefusalUnavailable,
			"didn't issue it just now. Try again shortly"},
		sandbox.WhyProviderRefused:     {tools.RefusalUnavailable, "refused the company's credentials"},
		sandbox.WhyProviderBilling:     {tools.RefusalUnavailable, "account for the company needs paying for"},
		sandbox.WhyProviderLimit:       {tools.RefusalUnavailable, "Try again later"},
		sandbox.WhyProviderUnavailable: {tools.RefusalUnavailable, "Try again shortly"},
	}
	for _, why := range sandbox.Whys {
		want, ok := cases[why]
		if !ok {
			t.Errorf("class %q has no case here: give it one, and words in run_sandbox", why)
			continue
		}
		surface := sandboxSurface(t, &launchSpy{err: sandbox.Because(why, errors.New(detail))})
		res, err := surface.Execute(t.Context(), llm.ToolCall{ID: "c1", Name: builtin.RunSandboxTool,
			Arguments: map[string]any{"brief": "fix the flake"}})
		if err != nil {
			t.Fatalf("%s: Execute: %v", why, err)
		}
		switch {
		case !res.Failed || res.Suspend:
			t.Errorf("%s: result = %+v, want a failure that does not suspend", why, res)
		case res.Refusal() != want.class:
			t.Errorf("%s: classed %s, want %s", why, res.Refusal(), want.class)
		case strings.Contains(res.Output, "sk-not-real") || strings.Contains(res.Output, "401") ||
			strings.Contains(res.Output, "e2b"):
			t.Errorf("%s: the model was handed the error's own text: %q", why, res.Output)
		case !strings.Contains(res.Output, want.says):
			t.Errorf("%s: %q does not say %q", why, res.Output, want.says)
		}
		// A WAIT OFFERED EXACTLY WHERE ONE HELPS: the hint every surface
		// answering in status codes takes, and the words a model reads.
		if want.class != tools.RefusalUnavailable {
			continue
		}
		waits := builtin.RetryAfter(res.Cause, time.Second) > 0
		if waits != why.Waits() {
			t.Errorf("%s: a wait is offered %t, want %t", why, waits, why.Waits())
		}
		// AND A PERSON'S CHANGE NAMED where one is what clears it: a seat
		// that runs its own session needs none, and is told what to use.
		if !why.Waits() && why != sandbox.WhyOwnSession && !strings.Contains(res.Output, "won't") {
			t.Errorf("%s: %q does not say waiting won't clear it", why, res.Output)
		}
	}
}

// What no backend classed is told as what it is: a seat this node no longer
// holds, a condition of the node, or the turn's own context ending — each in
// its own words, and none as a sandbox class.
func TestAnUnclassedLaunchIsToldAsWhatItIs(t *testing.T) {
	call := llm.ToolCall{ID: "c1", Name: builtin.RunSandboxTool, Arguments: map[string]any{"brief": "x"}}

	held := sandboxSurface(t, &launchSpy{err: fmt.Errorf("sandbox: launching a run: %w",
		sandbox.ErrSeatNotHeld)})
	res, err := held.Execute(t.Context(), call)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !res.Failed || !strings.Contains(res.Output, "no longer holds your seat") ||
		builtin.RetryAfter(res.Cause, time.Second) != 0 {
		t.Errorf("seat not held: %q (wait %v), want a waitless refusal saying so",
			res.Output, builtin.RetryAfter(res.Cause, time.Second))
	}

	// A RUN WHOSE ENDING IS DECIDED is a state of the run, never a fault of
	// the node: told as one, the model read that the node had failed.
	ending := sandboxSurface(t, &launchSpy{err: fmt.Errorf("sandbox: recording the run: %w",
		sandbox.ErrRunEnding)})
	res, err = ending.Execute(t.Context(), call)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Refusal() != tools.RefusalUnavailable ||
		!strings.Contains(res.Output, "coding run is being closed") ||
		strings.Contains(res.Output, "fault") ||
		builtin.RetryAfter(res.Cause, time.Second) != 0 {
		t.Errorf("a run whose ending is decided: %q (%s), want a waitless refusal "+
			"saying the run is being closed", res.Output, res.Refusal())
	}

	store := sandboxSurface(t, &launchSpy{err: fmt.Errorf("sandbox: recording the run: %w",
		coord.ErrUnavailable)})
	res, err = store.Execute(t.Context(), call)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Refusal() != tools.RefusalUnavailable ||
		!strings.Contains(res.Output, "coordination store did not answer") ||
		strings.Contains(res.Output, "recording the run") {
		t.Errorf("a store that did not answer: %q (%s), want the condition's words",
			res.Output, res.Refusal())
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	ended := sandboxSurface(t, &launchSpy{err: sandbox.Because(sandbox.WhyProviderUnavailable,
		fmt.Errorf("e2b: /sandboxes: %w", context.Canceled))})
	res, err = ended.Execute(ctx, call)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(res.Output, "abandoned") || strings.Contains(res.Output, "provider") {
		t.Errorf("a call abandoned by its own context: %q, want it told as abandoned", res.Output)
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
	if _, err := builtin.Register(reg, gated(builtin.Deps{Sandbox: &launchSpy{}})); err != nil {
		t.Fatalf("Register: %v", err)
	}
	entry, _ := reg.Snapshot().Lookup(builtin.RunSandboxTool)
	res, err := entry.Tool.Call(everyGrant(), map[string]any{"brief": "fix it"})
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
