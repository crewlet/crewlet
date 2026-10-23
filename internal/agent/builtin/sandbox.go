package builtin

import (
	"context"
	"errors"
	"strings"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/toolname"
	"github.com/crewlet/crewlet/internal/tools"
)

// RunSandboxTool is the tool's wire name.
const RunSandboxTool = toolname.RunSandbox

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
	return refused(tools.RefusalForbidden, RunSandboxTool+" can only be called "+
		"from an Execute phase that can suspend, because the coding job "+
		"outlives the turn that starts it."), nil
}

func (t *runSandbox) CallDetached(ctx context.Context, turn *turnctx.Turn, args map[string]any) (tools.DetachedResult, error) {
	brief := strings.TrimSpace(argString(args, "brief"))
	if brief == "" {
		return tools.DetachedResult{Result: failed(RunSandboxTool + " needs a " +
			"non-empty `brief` describing the code task.")}, nil
	}
	if turn == nil || turn.RunID == "" {
		return tools.DetachedResult{Result: refused(tools.RefusalForbidden,
			RunSandboxTool+" needs an active turn; it can only be called from "+
				"within an Execute phase.")}, nil
	}

	res, err := t.launcher.Launch(ctx, turn, brief)
	if err != nil {
		// A launch that failed is a TOOL FAILURE, not an engine one: the
		// model asked for something the engine could not do, and telling it
		// so lets it fall back to its own tools rather than losing the turn.
		return tools.DetachedResult{Result: notStarted(ctx, turn, err)}, nil
	}

	log.InfoContext(ctx, "run_sandbox_suspended",
		"turn_id", turn.RunID, "sandbox_id", res.SandboxID,
		"coding_agent", res.CodingAgent, "reused", res.Reused)

	return tools.DetachedResult{
		Result: tools.Result{
			Output: "(the sandbox coding job is running; its findings will come " +
				"back as this call's result)",
		},
		Suspend: true,
		Payload: map[string]any{
			"turn_id":    turn.RunID,
			"sandbox_id": res.SandboxID,
		},
	}, nil
}

// notStarted is a launch that failed, as the model is told it: what kept the
// run from starting, and whether waiting or a person's change clears it — and
// the error itself to the node's log, never to the model.
//
// THE ERROR WAS THE ANSWER, and it was the wrong reader's. Every failure came
// back as "the sandbox could not be started: " and the Go error after it — an
// E2B status and its body, a container runtime's stderr, a coordination
// store's transport error naming its host — and marked as one a wait clears,
// so a model told its seat's sandbox setup could not start a run was invited
// to try again for as long as its turn lasted. The backends class their
// failures where they meet them ([sandbox.WhyOf]); this reads the class.
//
// IN ORDER: a call whose own context ended is told that, whatever the
// provider's request it ended said; a seat this node no longer holds is no
// run anybody here can start; a run whose ending is decided takes no new job
// in this turn; then the class; and what nothing classed is a condition of
// this node or a fault of it ([nodeFailure]), never filed under the nearest
// class.
func notStarted(ctx context.Context, turn *turnctx.Turn, err error) tools.Result {
	const stopped = "No coding run was started"
	why, classed := sandbox.WhyOf(err)
	switch {
	case ctx.Err() != nil:
		logNotStarted(ctx, turn, "", err)
		return nodeFailure(ctx, RunSandboxTool, ctx.Err(), stopped, "")
	case errors.Is(err, sandbox.ErrSeatNotHeld):
		logNotStarted(ctx, turn, "", err)
		return refusedBy(tools.RefusalUnavailable, waitless(), stopped+": this node no "+
			"longer holds your seat, so it can't start a run for it. Trying again won't "+
			"help in this turn, so finish without the coding run.")
	case errors.Is(err, sandbox.ErrRunEnding):
		// THE TURN'S RUN IS BEING CLOSED, which is a state of the run and
		// not a broken node: its ending is decided, and the row takes no
		// new job until it is gone. Told as a fault, the model read that
		// the node itself had failed.
		logNotStarted(ctx, turn, "", err)
		return refusedBy(tools.RefusalUnavailable, waitless(), stopped+": this turn's "+
			"coding run is being closed, so no new one starts in this turn. Trying again "+
			"won't help, so finish without the coding run.")
	case !classed:
		if _, condition := Condition(err); condition {
			logNotStarted(ctx, turn, "", err)
		}
		return nodeFailure(ctx, RunSandboxTool, err, stopped, "")
	}
	logNotStarted(ctx, turn, why, err)
	switch why {
	case sandbox.WhyConfiguration:
		return refusedBy(tools.RefusalUnavailable, waitless(), stopped+": this seat's "+
			"sandbox isn't set up so it can start one, and waiting won't change that. Do "+
			"the work with your own tools, or tell whoever asked that an administrator "+
			"needs to fix the seat's sandbox setup; the node's log says what to change.")
	case sandbox.WhySetupUnavailable:
		return refusedBy(tools.RefusalUnavailable, waits(), stopped+": a download this "+
			"seat's sandbox setup makes failed for now. Try again shortly, or do the work "+
			"with your own tools.")
	case sandbox.WhyOwnSession:
		return refusedBy(tools.RefusalUnavailable, waitless(), stopped+": you do your "+
			"code work in your own session, which already has a shell, an editor and a "+
			"checkout, and a second box would hold work you couldn't see. Use the shell "+
			"and editor you already have.")
	case sandbox.WhyBudget:
		return refused(tools.RefusalBudgetExhausted, stopped+": this seat has too little "+
			"of its token budget left for one, and a coding run uses a box, a clone and a "+
			"toolchain before it produces anything. Trying again won't help until the "+
			"budget's window turns over or its ceiling is raised, so do the work with your "+
			"own tools, or report that the budget is running low.")
	case sandbox.WhyAwaitingCredential:
		return refusedBy(tools.RefusalUnavailable, waits(), stopped+": this seat's run "+
			"environment is waiting for a credential one of the company's integrations is "+
			"setting up. Try again in a few minutes, or do the work with your own tools.")
	case sandbox.WhyCredentialUnavailable:
		return refusedBy(tools.RefusalUnavailable, waits(), stopped+": the service that "+
			"issues this seat's credential for the run didn't issue it just now. Try again "+
			"shortly, or do the work with your own tools.")
	case sandbox.WhyProviderRefused:
		return refusedBy(tools.RefusalUnavailable, waitless(), stopped+": the sandbox "+
			"provider refused the company's credentials, and waiting won't change that. Do "+
			"the work with your own tools, or tell whoever asked that an administrator needs "+
			"to fix the sandbox provider's credentials.")
	case sandbox.WhyProviderBilling:
		return refusedBy(tools.RefusalUnavailable, waitless(), stopped+": the sandbox "+
			"provider's account for the company needs paying for, and waiting won't change "+
			"that. Do the work with your own tools, or tell whoever asked that an "+
			"administrator needs to check the sandbox provider's account.")
	case sandbox.WhyProviderLimit:
		return refusedBy(tools.RefusalUnavailable, waits(), stopped+": the sandbox "+
			"provider is at a limit of the company's account, with too many boxes running "+
			"or too many requests. Try again later, or do the work with your own tools.")
	case sandbox.WhyProviderUnavailable:
		return refusedBy(tools.RefusalUnavailable, waits(), stopped+": the sandbox "+
			"provider didn't answer, or failed on its side. Try again shortly, or do the "+
			"work with your own tools.")
	}
	// A CLASS THIS BUILD HAS NO WORDS FOR is a fault of this build:
	// TestEveryLaunchClassIsWorded holds every class to its words.
	return faulted(ctx, RunSandboxTool, err, stopped+": "+faultSaid+".")
}

// logNotStarted writes a launch that failed, with its class where one was
// given — the error the model is not told goes here. A fault is not written
// here: [faulted] writes it, as every tool's.
func logNotStarted(ctx context.Context, turn *turnctx.Turn, why sandbox.Why, err error) {
	attrs := []any{"turn_id", turn.RunID, "error", err.Error()}
	if why != "" {
		attrs = append(attrs, "why", string(why))
	}
	log.WarnContext(ctx, "run_sandbox_not_started", attrs...)
}
