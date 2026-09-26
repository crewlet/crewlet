package sandbox

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/queue/topics"
)

// TurnRef identifies the turn a detached run belongs to.
//
// Carried on the row rather than looked up later, because the resume happens
// in a different process from the launch as a matter of routine: a restart, a
// seat handoff, or simply days passing while a person answers a question.
type TurnRef struct {
	// TurnID is the RUN this job belongs to — one execution of a turn, and
	// the key the whole pending-run record is stored under. It has to be
	// unique per execution: a redelivered trigger re-runs, and two runs
	// sharing this value would share one row and one box. See ADR-0017.
	TurnID string

	// WorkKey is the unit of work behind that run, carried so the row can
	// say which trigger it came from after the run outlives its process.
	WorkKey string

	AgentID         string
	AgentHandle     string
	Role            string
	ConversationKey string
	TraceID         string
	SpanID          string

	// Reply is who is waiting for this turn, persisted so the resumed turn
	// inherits the same delivery obligation. See [PendingRun.Reply].
	Reply string

	// Depth and Chain are the delegation state a resumed turn inherits.
	Depth int
	Chain []string

	// TriggeredAt is the TriggeredAt of the launching turn's context
	// (internal/agent/turnctx): the earliest instant an operation id the
	// turn derives can have been minted. Carried onto the row
	// ([PendingRun.TriggeredAt]) because the turn a resume re-enters derives
	// the same ids and has no trigger left to re-derive the instant from.
	TriggeredAt time.Time
}

// LaunchRequest is everything one detached coding run needs.
type LaunchRequest struct {
	Turn TurnRef

	// Brief is the executor's own description of the code task. It is
	// FRAMED, not passed through: the coding agent also gets the turn's
	// task and what its environment provides (see [buildBrief]).
	Brief string

	// Task is the ask the suspended turn was working on, carried onto the
	// row so a resume has it when the trigger is long gone.
	//
	// It is also what the started event names the run by, WHOLE: see the
	// announcement in [Launch].
	Task string

	Spec       Spec
	Setup      []SetupStep
	MCPServers map[string]MCPServer
	LLM        *AgentLLM

	// ReuseBox is a box this turn already has, paused from an earlier
	// run_sandbox call. Reattaching keeps the checkout; an empty value
	// provisions a fresh box, and the work re-seeds from the pushed branch.
	ReuseBox string

	// Fence is the ownership token every mutation on the row carries.
	Fence Fence

	// MaxLaunches is the most launches the run may open across its resumes
	// ([PendingRun.Launches]), zero for no bound. A launch past it is
	// refused with a [LaunchCapError] before anything is recorded or
	// provisioned.
	MaxLaunches int

	// Now is the clock. Nil takes time.Now.
	Now func() time.Time
}

// LaunchResult reports what a launch produced.
type LaunchResult struct {
	SandboxID   string
	CommandID   string
	CodingAgent string
	Reused      bool
}

// Launch provisions a box, starts the coding agent detached, and persists the
// row that outlives the turn.
//
// THE ORDER IS THE CONTRACT. The row is created and the box attached to it
// BEFORE the job starts: a crash in that window leaves a row naming a box
// nobody is using, which recovery can kill, while the reverse ordering leaves
// a box that nothing names and nothing can ever reclaim.
//
// On any failure after the box exists, the box is reclaimed before the error
// propagates — an unreferenced box is billed for until its TTL and collected
// by nobody.
func Launch(ctx context.Context, m *Manager, store PendingStore, q Publisher, req LaunchRequest) (LaunchResult, error) {
	now := req.Now
	if now == nil {
		now = time.Now
	}
	if strings.TrimSpace(req.Turn.TurnID) == "" {
		return LaunchResult{}, fmt.Errorf("sandbox: a launch needs a turn id")
	}
	if strings.TrimSpace(req.Brief) == "" {
		return LaunchResult{}, fmt.Errorf("sandbox: a launch needs a brief")
	}
	if err := withinLaunches(ctx, store, req); err != nil {
		return LaunchResult{}, err
	}

	// The row FIRST, so a crash between here and the box leaves a record
	// rather than nothing. It opens in [StatusLaunching] and stays there
	// until the turn writes the conversation a resume re-enters: nothing
	// polls or claims a run in that window, which is what stops a job that
	// finishes before the turn unwinds from being collected into nothing.
	if err := store.BeginLaunch(ctx, PendingRun{
		TurnID: req.Turn.TurnID, WorkKey: req.Turn.WorkKey,
		AgentHandle: req.Turn.AgentHandle,
		AgentID:     req.Turn.AgentID, Role: req.Turn.Role,
		// WRITTEN WITH THE ROW, before the box exists, because every
		// failure path from here on reclaims through it — and because the
		// process that collects this run may not be this one, nor reading
		// the configuration that chose the cell.
		Placement:       string(req.Spec.Placement),
		CodingAgent:     req.Spec.CodingAgent,
		TaskDescription: req.Task,
		ConversationKey: req.Turn.ConversationKey,
		Reply:           req.Turn.Reply,
		TraceID:         req.Turn.TraceID, SpanID: req.Turn.SpanID,
		DelegationDepth: req.Turn.Depth, DelegationChain: req.Turn.Chain,
		TriggeredAt: req.Turn.TriggeredAt,
		CreatedAt:   now(),
	}, req.Fence); err != nil {
		return LaunchResult{}, fmt.Errorf("sandbox: recording the run: %w", err)
	}

	box, runner, reused, err := acquire(ctx, m, req)
	if err != nil {
		// The row is open and this launch is over. Every failure past this
		// point closes it: a run left LAUNCHING is polled by nothing and
		// claimed by nothing, so it sits on its seat's busy count and its
		// box until the seat happens to move to another node.
		abandon(ctx, m, store, req, "")
		return LaunchResult{}, err
	}

	// Attached before the job starts, for the reason above.
	if err = store.AttachSandbox(ctx, req.Turn.TurnID, BoxRef{
		SandboxID: box.ID(), CodingAgent: req.Spec.CodingAgent,
		PauseTTLSec: req.Spec.PauseTTLSec,
	}, req.Fence); err != nil {
		abandon(ctx, m, store, req, box.ID())
		return LaunchResult{}, fmt.Errorf("sandbox: attaching the box: %w", err)
	}

	handle, err := runner.Start(ctx, box, RunRequest{
		Brief: buildBrief(req),
		Env:   withTelemetry(m, req),
		// FROM THE SPEC, which is where the provider default and the
		// seat's override have already been reconciled. A Limits the
		// caller passed alongside would be a second answer to the same
		// question, and it was the one nobody filled in: every coding run
		// went out uncapped while the field looked wired.
		Limits:     Limits{MaxTurns: req.Spec.MaxTurns},
		LLM:        req.LLM,
		MCPServers: req.MCPServers,
	})
	if err != nil {
		abandon(ctx, m, store, req, box.ID())
		return LaunchResult{}, fmt.Errorf("sandbox: starting the coding agent: %w", err)
	}

	// The command id is written SECOND, once the job exists: a row naming a
	// command that was never started would have the waiter poll for a
	// completion marker nothing is ever going to write.
	if err := store.AttachSandbox(ctx, req.Turn.TurnID, BoxRef{
		SandboxID: box.ID(), CommandID: handle.CommandID,
		CodingAgent: req.Spec.CodingAgent, SessionID: handle.SessionID,
		PauseTTLSec: req.Spec.PauseTTLSec,
	}, req.Fence); err != nil {
		abandon(ctx, m, store, req, box.ID())
		return LaunchResult{}, fmt.Errorf("sandbox: recording the job: %w", err)
	}

	started := types.SandboxRunStarted{
		Agent: req.Turn.AgentID, AgentHandle: req.Turn.AgentHandle,
		RoleName: req.Turn.Role, TurnID: req.Turn.TurnID, WorkKey: req.Turn.WorkKey,
		SandboxID: box.ID(), CodingAgent: req.Spec.CodingAgent,
		ConversationKey: req.Turn.ConversationKey,
		// THE ROW'S OWN VALUE, WHOLE. The dashboard names a run by this
		// field until its poll of the run records returns the row, and by
		// the row's task_description from then on — req.Task, as written
		// above — so any other value here is a label that changes under the
		// reader mid-run. And nothing is cut: the same text is on the row
		// and served whole by the sandbox-runs query, so a cut here would
		// buy nothing but a second, shorter spelling of it.
		Task: req.Task,
	}
	ev := events.New(started, events.TraceContext{
		TraceID: req.Turn.TraceID, ParentSpanID: req.Turn.SpanID,
	})
	ev.Source = req.Turn.Role

	// TWO PUBLISHES, as for a completion. The events copy is the
	// announcement the dashboard's running-sandboxes panel reads; the
	// per-seat control copy is what marks the seat busy on the node that
	// owns it — which is this one, but the event is what makes that true
	// after a restart as well.
	if err := q.Publish(ctx, topics.Event(started.EventType()), ev); err != nil {
		log.WarnContext(ctx, "sandbox_started_publish_failed",
			"turn_id", req.Turn.TurnID, "error", err.Error())
	}
	if control := topics.AgentControl(req.Turn.AgentHandle); control != "" {
		if err := q.Publish(ctx, control, ev); err != nil {
			log.WarnContext(ctx, "sandbox_started_control_failed",
				"turn_id", req.Turn.TurnID, "error", err.Error())
		}
	}

	log.InfoContext(ctx, "sandbox_run_started",
		"turn_id", req.Turn.TurnID, "agent", req.Turn.AgentHandle,
		"sandbox_id", box.ID(), "placement", string(req.Spec.Placement),
		"coding_agent", req.Spec.CodingAgent, "reused", reused)

	return LaunchResult{
		SandboxID: box.ID(), CommandID: handle.CommandID,
		CodingAgent: req.Spec.CodingAgent, Reused: reused,
	}, nil
}

// ErrLaunchCap reports a launch refused because the run has already opened as
// many launches as it may. See [LaunchCapError].
var ErrLaunchCap = errors.New("sandbox: the run has opened as many launches as it may")

// LaunchCapError is a launch refused at [LaunchRequest.MaxLaunches], naming
// how many the run has opened and the bound.
type LaunchCapError struct {
	Launched, Max int
}

func (e *LaunchCapError) Error() string {
	return fmt.Sprintf("sandbox: this turn has already started %d coding runs, and a turn may "+
		"start at most %d", e.Launched, e.Max)
}

// Unwrap is [ErrLaunchCap], which every refusal at the bound is.
func (e *LaunchCapError) Unwrap() error { return ErrLaunchCap }

// withinLaunches refuses a launch past [LaunchRequest.MaxLaunches].
//
// READ APART FROM THE LAUNCH THAT COUNTS IT, which is sound because a run's
// launches are serial: each one suspends the loop that asked for it, and the
// next can only be asked for by the turn that loop resumes into. Nothing else
// opens a launch under a run's id, so the count cannot move between this read
// and [PendingStore.BeginLaunch].
func withinLaunches(ctx context.Context, store PendingStore, req LaunchRequest) error {
	if req.MaxLaunches <= 0 {
		return nil
	}
	existing, found, err := store.Get(ctx, req.Turn.TurnID)
	if err != nil {
		return fmt.Errorf("sandbox: reading how many runs this turn has started: %w", err)
	}
	if found && existing.Launches >= req.MaxLaunches {
		return &LaunchCapError{Launched: existing.Launches, Max: req.MaxLaunches}
	}
	return nil
}

// acquire reattaches to this turn's existing box, or provisions a new one.
//
// Reuse keeps the CHECKOUT, which is the expensive half of a coding run: a
// second run_sandbox call in one turn continues where the first stopped rather
// than re-cloning. A reattach that fails falls through to a fresh box rather
// than failing the launch — the box is gone, which is exactly the case the
// pushed branch exists for.
func acquire(ctx context.Context, m *Manager, req LaunchRequest) (Sandbox, Runner, bool, error) {
	if req.ReuseBox != "" {
		box, runner, err := m.Reconnect(ctx, req.Spec.Placement, req.ReuseBox, req.Spec.CodingAgent)
		if err == nil {
			return box, runner, true, nil
		}
		log.WarnContext(ctx, "sandbox_reuse_failed",
			"turn_id", req.Turn.TurnID, "sandbox_id", req.ReuseBox, "error", err.Error())
	}
	box, runner, err := m.Acquire(ctx, req.Spec, req.Setup)
	if err != nil {
		return nil, nil, false, err
	}
	return box, runner, false, nil
}

// abandon closes out a launch that could not finish: the box is reclaimed and
// then the run is finished, which deletes its record.
//
// BOTH, EVERY TIME, and the second attempted whether or not the first
// succeeded: they are separate calls that fail separately, and neither failing
// is a reason to leave the other undone. Three of the four failure paths used
// to do none of it and simply return, which left the row OPEN: a launching run
// is polled by nothing and claimed by nothing, so it held its seat's busy
// count and, on two of those paths, a box, until the seat happened to move to
// another node and recovery reaped it. The box goes first, so a record that
// cannot be deleted names a box that is already gone rather than the reverse.
//
// A context of its own, because the failure that got us here is often the
// caller's context expiring — and a teardown skipped for that reason leaves a
// box running to its TTL with nobody to collect it, and a row nobody settles.
func abandon(ctx context.Context, m *Manager, store PendingStore, req LaunchRequest, sandboxID string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), discardGrace)
	defer cancel()
	if sandboxID != "" {
		provider, err := m.Provider(req.Spec.Placement)
		if err != nil {
			log.WarnContext(ctx, "sandbox_launch_reclaim_no_backend",
				"sandbox_id", sandboxID, "placement", string(req.Spec.Placement),
				"error", err.Error())
		} else if err := provider.Kill(ctx, sandboxID); err != nil {
			log.WarnContext(ctx, "sandbox_launch_reclaim_failed",
				"sandbox_id", sandboxID, "error", err.Error())
		}
	}
	// An agent-mode job that had started before the launch failed can have
	// called the bridge already, and those calls go with this finish, unread:
	// the launch's error fails the executor pass that asked for it, so no
	// resume will collect them.
	if _, err := store.Finish(ctx, req.Turn.TurnID, req.Fence); err != nil {
		log.WarnContext(ctx, "sandbox_launch_finish_failed",
			"turn_id", req.Turn.TurnID, "error", err.Error())
	}
}

// buildBrief assembles what the coding agent is actually told.
//
// THREE PARTS, in the order an engineer reads them: the concrete task the
// executor asked for, the wider goal it serves, and what the environment
// already provides. The last is the setup steps' own briefs — the mechanism
// and the hint together, so the agent does not spend rounds rediscovering
// that git auth is already wired.
//
// There is no success-criteria section any more. It came from the planning
// phase's declared criteria, and with one phase deciding and acting there is
// no separate plan to declare them: what "done" means is the executor's own
// brief, written by the frame that will read the answer.
func buildBrief(req LaunchRequest) string {
	var b strings.Builder
	b.WriteString(req.Brief)
	if req.Task != "" && req.Task != req.Brief {
		b.WriteString("\n\n## The wider task\n")
		b.WriteString(req.Task)
	}
	names := slices.Collect(maps.Keys(req.MCPServers))
	b.WriteString("\n")
	b.WriteString(EnvironmentBrief(req.Setup, names))
	return b.String()
}

// withTelemetry adds the run's OTel environment to the spec's.
//
// MERGED HERE rather than in the spec, because the endpoint carries a token
// scoped to THIS run and minted with a TTL: putting it in the spec would have
// it persisted with the pending row and re-used by a resume hours later,
// against a token that expired while the run was parked.
//
// THE OPERATOR'S OWN VALUES WIN. role.sandbox.env is where an operator
// declares what a box gets, and a deployment that points its coding agents at
// its own collector directly has said so deliberately — silently overriding
// it would be the engine choosing where somebody else's telemetry goes.
func withTelemetry(m *Manager, req LaunchRequest) map[string]string {
	if m.telemetry == nil {
		return req.Spec.Env
	}
	added := m.telemetry.RunEnv(req.Turn.TraceID, req.Turn.SpanID,
		req.Turn.TurnID, req.Turn.AgentHandle, otelTokenTTL(req.Spec))
	if len(added) == 0 {
		return req.Spec.Env
	}
	env := make(map[string]string, len(req.Spec.Env)+len(added))
	for k, v := range added {
		env[k] = v
	}
	for k, v := range req.Spec.Env {
		env[k] = v
	}
	return env
}

// otelTokenTTL is how long a run's export endpoint stays valid.
//
// TIED TO THE BOX'S OWN LIFETIME rather than picked: a token that expires
// while its box is still running loses the tail of the run's telemetry —
// exactly the part that explains a failure — and one that outlives the box is
// a credential nothing needs. The pause TTL is added because a run parked on
// a human's answer resumes into the same box and keeps exporting, and the
// doubling covers the keepalive refreshing the box's own timer.
func otelTokenTTL(spec Spec) time.Duration {
	ttl := time.Duration(spec.TimeoutSec) * time.Second
	if ttl <= 0 {
		ttl = DefaultBoxTimeout
	}
	ttl *= 2
	if pause := time.Duration(spec.PauseTTLSec) * time.Second; pause > 0 {
		ttl += pause
	}
	return ttl
}

// RunEnvFor is [withTelemetry] under a name a test can reach.
//
// EXPORTED FOR THE ONE PROPERTY THAT IS OTHERWISE UNOBSERVABLE: the merge
// order between the engine's telemetry variables and the operator's own. Both
// end up inside a box that a test cannot look into, and getting the order
// wrong sends somebody else's spans to the engine — silently, because they do
// arrive somewhere.
func RunEnvFor(m *Manager, req LaunchRequest) map[string]string {
	return withTelemetry(m, req)
}
