package sandbox

import (
	"context"
	"fmt"
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
	Limits     Limits

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
	// rather than nothing. Create is idempotent: a redelivered kick-off
	// presents the same turn id, and the row already there is the correct
	// one — possibly with a box attached that a second create would erase.
	if err := store.Create(ctx, PendingRun{
		TurnID: req.Turn.TurnID, AgentHandle: req.Turn.AgentHandle,
		AgentID: req.Turn.AgentID, Role: req.Turn.Role,
		CodingAgent:          req.Spec.CodingAgent,
		TaskDescription:      req.Task,
		SuccessCriteria:      req.Criteria,
		ConversationKey:      req.Turn.ConversationKey,
		NotificationMetadata: req.Turn.Metadata,
		TraceID:              req.Turn.TraceID, SpanID: req.Turn.SpanID,
		DelegationDepth: req.Turn.Depth, DelegationChain: req.Turn.Chain,
		Status: StatusRunning, CreatedAt: now(),
	}); err != nil {
		return LaunchResult{}, fmt.Errorf("sandbox: recording the run: %w", err)
	}

	box, runner, reused, err := acquire(ctx, m, req)
	if err != nil {
		return LaunchResult{}, err
	}

	// Attached before the job starts, for the reason above.
	if err = store.AttachSandbox(ctx, req.Turn.TurnID, BoxRef{
		SandboxID: box.ID(), CodingAgent: req.Spec.CodingAgent,
		PauseTTLSec: req.Spec.PauseTTLSec,
	}, req.Fence); err != nil {
		reclaim(ctx, m, box.ID())
		return LaunchResult{}, fmt.Errorf("sandbox: attaching the box: %w", err)
	}

	handle, err := runner.Start(ctx, box, RunRequest{
		Brief:      buildBrief(req),
		Env:        req.Spec.Env,
		Limits:     req.Limits,
		LLM:        req.LLM,
		MCPServers: req.MCPServers,
	})
	if err != nil {
		reclaim(ctx, m, box.ID())
		if relErr := store.ReleaseBox(ctx, req.Turn.TurnID); relErr != nil {
			log.Warn("sandbox_launch_release_failed",
				"turn_id", req.Turn.TurnID, "error", relErr.Error())
		}
		if setErr := store.SetStatus(ctx, req.Turn.TurnID, StatusFailed, req.Fence); setErr != nil {
			log.Warn("sandbox_launch_mark_failed",
				"turn_id", req.Turn.TurnID, "error", setErr.Error())
		}
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
		reclaim(ctx, m, box.ID())
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
		log.Warn("sandbox_started_publish_failed",
			"turn_id", req.Turn.TurnID, "error", err.Error())
	}
	if control := topics.AgentControl(req.Turn.AgentHandle); control != "" {
		if err := q.Publish(ctx, control, ev); err != nil {
			log.Warn("sandbox_started_control_failed",
				"turn_id", req.Turn.TurnID, "error", err.Error())
		}
	}

	log.Info("sandbox_run_started",
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
		log.Warn("sandbox_reuse_failed",
			"turn_id", req.Turn.TurnID, "sandbox_id", req.ReuseBox, "error", err.Error())
	}
	box, runner, err := m.Acquire(ctx, req.Spec, req.Setup)
	if err != nil {
		return nil, nil, false, err
	}
	return box, runner, false, nil
}

// reclaim kills a box the launch could not finish wiring up.
//
// A context of its own, because the failure that got us here is often the
// caller's context expiring — and a teardown skipped for that reason leaves a
// box running to its TTL with nobody to collect it.
func reclaim(ctx context.Context, m *Manager, sandboxID string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), discardGrace)
	defer cancel()
	if err := m.Provider().Kill(ctx, sandboxID); err != nil {
		log.Warn("sandbox_launch_reclaim_failed", "sandbox_id", sandboxID, "error", err.Error())
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
	return strings.TrimSpace(line[:briefSummaryLimit]) + "…"
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
