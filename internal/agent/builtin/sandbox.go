package builtin

import (
	"context"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/tools"
)

// RunSandboxTool is the tool's wire name.
const RunSandboxTool = "run_sandbox"

// SandboxLauncher is what this tool needs to start a detached coding run.
//
// An interface so the tool can be built and tested without a provider, and so
// the launch's ordering rules stay in one place rather than being re-derived
// here.
type SandboxLauncher interface {
	// Launch starts the run and returns once the job is going. It does NOT
	// wait for the job.
	Launch(ctx context.Context, turn *turnctx.Turn, brief string) (sandbox.LaunchResult, error)
}

// runSandbox hands a concrete code task to a coding agent in an isolated box.
//
// THE SANDBOX IS A TOOL, not a phase backend that replaces Execute. The
// executor calls it with a code task, the engine starts the coding agent
// detached and SUSPENDS this Execute loop, and when the job finishes the loop
// is resumed with the agent's findings spliced in as this call's result. The
// same turn then continues — reporting, fixing, or calling the sandbox again —
// with its ordinary tools and full context.
//
// That is why it suspends rather than blocking: a coding job runs for minutes
// to hours, far past any broker ack window, and a turn that blocked on one
// would hold its seat and its node for the duration and lose everything to a
// restart.
type runSandbox struct{ launcher SandboxLauncher }

var _ tools.Detached = (*runSandbox)(nil)

func (t *runSandbox) Name() string { return RunSandboxTool }

func (t *runSandbox) Description() string {
	return "Hand a concrete code task to a coding agent running in an " +
		"isolated sandbox with its own shell, git checkout and developer " +
		"toolchain. Name the repository and the exact change or " +
		"investigation. The agent works on its own and ends by reporting " +
		"what it found or delivered, which comes back to you as this " +
		"call's result. You are NOT done when you call this — when it " +
		"returns you still report to, or act for, the requester."
}

func (t *runSandbox) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"brief": map[string]any{
				"type": "string",
				"description": "The concrete code task: which repository, and the " +
					"exact change or investigation. For example \"Clone " +
					"example.com/acme/api, run the test suite, and report which " +
					"tests fail and why\" or \"Fix the failing CI in acme/api and " +
					"open a pull request\".",
			},
		},
		"required": []any{"brief"},
	}
}

// Call is the non-detached path, and it REFUSES.
//
// A surface that invoked this tool without the detached seam would run the
// coding job and then answer the call normally — the turn would end believing
// the work was done while the job was still running, and nothing would ever
// collect its result. Refusing is the only safe answer, and it says why.
func (t *runSandbox) Call(context.Context, map[string]any) (tools.Result, error) {
	return tools.Result{
		Output: "run_sandbox can only be called from an Execute phase that can " +
			"suspend, because the coding job outlives the turn that starts it.",
		Failed: true,
	}, nil
}

func (t *runSandbox) CallDetached(ctx context.Context, turn *turnctx.Turn, args map[string]any) (tools.DetachedResult, error) {
	brief := strings.TrimSpace(argString(args, "brief"))
	if brief == "" {
		return failedDetached("run_sandbox needs a non-empty `brief` describing the code task."), nil
	}
	if turn == nil || turn.ID == "" {
		return failedDetached("run_sandbox needs an active turn; it can only be " +
			"called from within an Execute phase."), nil
	}

	res, err := t.launcher.Launch(ctx, turn, brief)
	if err != nil {
		// A launch that failed is a TOOL FAILURE, not an engine one: the
		// model asked for something the engine could not do, and telling it
		// so lets it fall back to its own tools rather than losing the turn.
		return failedDetached(fmt.Sprintf("the sandbox could not be started: %v", err)), nil
	}

	log.InfoContext(ctx, "run_sandbox_suspended",
		"turn_id", turn.ID, "sandbox_id", res.SandboxID,
		"coding_agent", res.CodingAgent, "reused", res.Reused)

	return tools.DetachedResult{
		Result: tools.Result{
			Output: "(the sandbox coding job is running; its findings will come " +
				"back as this call's result)",
		},
		Suspend: true,
		Payload: map[string]any{
			"turn_id":    turn.ID,
			"sandbox_id": res.SandboxID,
		},
	}, nil
}

func failedDetached(msg string) tools.DetachedResult {
	return tools.DetachedResult{Result: tools.Result{Output: msg, Failed: true}}
}
