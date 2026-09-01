package sandbox

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

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
	TurnID          string
	AgentID         string
	AgentHandle     string
	Role            string
	ConversationKey string
	TraceID         string
	SpanID          string

	// Metadata is the notification routing the resumed turn's reply tools
	// need, so a report lands on the conversation the task came from.
	Metadata map[string]any

	// Depth and Chain are the delegation state a resumed turn inherits.
	Depth int
	Chain []string
}

// LaunchRequest is everything one detached coding run needs.
type LaunchRequest struct {
	Turn TurnRef

	// Brief is the executor's own description of the code task. It is
	// FRAMED, not passed through: the coding agent also gets the turn's
	// task, its success criteria, and what its environment provides.
	Brief string

	// Task and Criteria come from the plan the turn already made.
	Task     string
	Criteria []string

	Spec       Spec
	Setup      []SetupStep
	MCPServers map[string]map[string]any
	LLM        *AgentLLM

	// ReuseBox is a box this turn already has, paused from an earlier
	// run_sandbox call. Reattaching keeps the checkout; an empty value
	// provisions a fresh box, and the work re-seeds from the pushed branch.
	ReuseBox string

	// Fence is the ownership token every mutation on the row carries.
	Fence Fence

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

	// The row FIRST, so a crash between here and the box leaves a record
	// rather than nothing. It opens in [StatusLaunching] and stays there
	// until the turn writes the conversation a resume re-enters: nothing
	// polls or claims a run in that window, which is what stops a job that
	// finishes before the turn unwinds from being collected into nothing.
	if err := store.BeginLaunch(ctx, PendingRun{
		TurnID: req.Turn.TurnID, AgentHandle: req.Turn.AgentHandle,
		AgentID: req.Turn.AgentID, Role: req.Turn.Role,
		CodingAgent:          req.Spec.CodingAgent,
		TaskDescription:      req.Task,
		SuccessCriteria:      req.Criteria,
		ConversationKey:      req.Turn.ConversationKey,
		NotificationMetadata: req.Turn.Metadata,
		TraceID:              req.Turn.TraceID, SpanID: req.Turn.SpanID,
		DelegationDepth: req.Turn.Depth, DelegationChain: req.Turn.Chain,
		CreatedAt: now(),
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
		RoleName: req.Turn.Role, TurnID: req.Turn.TurnID,
		SandboxID: box.ID(), CodingAgent: req.Spec.CodingAgent,
		ConversationKey: req.Turn.ConversationKey,
		Task:            summarise(req.Brief),
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
		"sandbox_id", box.ID(), "coding_agent", req.Spec.CodingAgent, "reused", reused)

	return LaunchResult{
		SandboxID: box.ID(), CommandID: handle.CommandID,
		CodingAgent: req.Spec.CodingAgent, Reused: reused,
	}, nil
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
		box, runner, err := m.Reconnect(ctx, req.ReuseBox, req.Spec.CodingAgent)
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

// abandon closes out a launch that could not finish: the box is reclaimed, the
// row stops naming it, and the run is marked failed.
//
// ALL THREE, EVERY TIME, and each attempted regardless of the ones before it —
// they are separate calls that fail separately, and none of them failing is a
// reason to leave the rest undone. Three of the four failure paths used to do
// none of it and simply return, which left the row OPEN: a launching run is
// polled by nothing and claimed by nothing, so it held its seat's busy count
// and, on two of those paths, a box, until the seat happened to move to
// another node and recovery reaped it.
//
// A context of its own, because the failure that got us here is often the
// caller's context expiring — and a teardown skipped for that reason leaves a
// box running to its TTL with nobody to collect it, and a row nobody settles.
func abandon(ctx context.Context, m *Manager, store PendingStore, req LaunchRequest, sandboxID string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), discardGrace)
	defer cancel()
	if sandboxID != "" {
		if err := m.Provider().Kill(ctx, sandboxID); err != nil {
			log.WarnContext(ctx, "sandbox_launch_reclaim_failed",
				"sandbox_id", sandboxID, "error", err.Error())
		}
		if err := store.ReleaseBox(ctx, req.Turn.TurnID); err != nil {
			log.WarnContext(ctx, "sandbox_launch_release_failed",
				"turn_id", req.Turn.TurnID, "error", err.Error())
		}
	}
	if err := store.SetStatus(ctx, req.Turn.TurnID, StatusFailed, req.Fence); err != nil {
		log.WarnContext(ctx, "sandbox_launch_mark_failed",
			"turn_id", req.Turn.TurnID, "error", err.Error())
	}
}

// briefSummaryLimit bounds the one-line task summary on the started event.
//
// The panel it feeds shows one row per running box, so this is a label rather
// than a description; the full brief lives on the pending row and is never on
// the wire. Sized to a readable row on a narrow column.
const briefSummaryLimit = 120

func summarise(brief string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(brief), "\n")
	if len(line) <= briefSummaryLimit {
		return line
	}
	// Never through a rune: a byte slice splits whatever multi-byte
	// character straddles the cut, and this label reaches the event store
	// and a dashboard row as JSON.
	cut := briefSummaryLimit
	for cut > 0 && !utf8.RuneStart(line[cut]) {
		cut--
	}
	return strings.TrimSpace(line[:cut]) + "…"
}

// buildBrief assembles what the coding agent is actually told.
//
// FOUR PARTS, in the order an engineer reads them: the concrete task the
// executor asked for, the wider goal it serves, what "done" means, and what
// the environment already provides. The last is the setup steps' own briefs —
// the mechanism and the hint together, so the agent does not spend rounds
// rediscovering that git auth is already wired.
func buildBrief(req LaunchRequest) string {
	var b strings.Builder
	b.WriteString(req.Brief)
	if req.Task != "" && req.Task != req.Brief {
		b.WriteString("\n\n## The wider task\n")
		b.WriteString(req.Task)
	}
	if len(req.Criteria) > 0 {
		b.WriteString("\n\n## Success criteria\n")
		for _, c := range req.Criteria {
			b.WriteString("- " + c + "\n")
		}
	}
	names := make([]string, 0, len(req.MCPServers))
	for name := range req.MCPServers {
		names = append(names, name)
	}
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
