package sandbox

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/queue/topics"
)

// controlSubject is a seat's sandbox-control subject from the id a run's row
// carries, or empty when the row names no routable seat.
//
// ONE PARSE, in one place. The id rides these rows as TEXT — the record is a
// JSON blob on the node estate and the framework that carries it never
// decodes it — so the three publishers that need this subject would otherwise
// each have their own idea of what an unparseable value means. Here it means
// exactly what a nil id means: not routable, say so, and do not publish to a
// subject named after nothing.
func controlSubject(agentID string) string {
	return topics.AgentControl(seatOf(agentID))
}

// inboxSubject is a seat's inbox from the id a run's row carries, or empty when
// the row names no routable seat: where an answer by turn is delivered
// ([AnswerDesk.Deliver]) and a reply the run did not take is handed back
// ([Coordinator.deliverHandBack]). The same one parse as [controlSubject], so
// the two subjects a run is reached on can never disagree about whether its
// row names a seat.
func inboxSubject(agentID string) string {
	return topics.AgentInbox(seatOf(agentID))
}

// seatOf is the one parse both subjects take: a row's text id as a seat id,
// with an unparseable value read as [uuid.Nil], which every topics builder
// answers with an empty subject.
func seatOf(agentID string) uuid.UUID {
	id, err := uuid.Parse(agentID)
	if err != nil {
		return uuid.Nil
	}
	return id
}

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

	// WorkSince is when that unit of work began, carried beside the key
	// for the same reason — see [PendingRun.WorkSince].
	WorkSince time.Time

	AgentID     string
	AgentHandle string
	Role        string

	// PartitionKey is the inbox batch the kick-off trigger arrived in and
	// ConversationKey the durable conversation it belongs to. BOTH travel,
	// and both are written onto the row: the conversation is what matches a
	// person's answer back to this run and where the resumed turn reports,
	// the partition tells two runs parked on one direct message apart
	// ([ConversationRef.Best]). See [PendingRun].
	//
	// NAMED AS [turnctx.Turn] NAMES THEM, field for field, because that is
	// where both values come from and the two launch sites copy them across
	// one after the other. They were ConversationKey and
	// ConversationIdentity here, holding the partition and the conversation
	// respectively — so each assignment read as its own opposite and a
	// swapped pair looked exactly like a correct one. [PendingRun] spells
	// them the same way, on the row and on the wire.
	//
	// Two fields where there was one, and the miss this guards against is
	// now available twice: this struct's partition was empty at its only
	// construction site for the whole life of the feature, so every resumed
	// turn recorded nothing at all.
	PartitionKey    string
	ConversationKey string

	TraceID string
	SpanID  string

	// Reply is who is waiting for this turn, persisted so the resumed turn
	// inherits the same delivery obligation. See [PendingRun.Reply].
	Reply string

	// Depth and Chain are the delegation state a resumed turn inherits.
	Depth int
	Chain []string

	// WorkItem is the item the launching turn is charged to, nil when it is
	// on nothing. Written onto the row, because the resumed turn has no
	// trigger to resolve it from — see [PendingRun.WorkItem].
	WorkItem *types.WorkItem

	// Requester is the seat whose wake started the turn, by its handle,
	// "" for none — what a question addressed to
	// "requester" is put to. See [PendingRun.Requester].
	Requester string
}

// LaunchRequest is everything one detached coding run needs.
//
// TWO TEXTS, KEPT APART to the end: what the coding agent is asked to do
// (Brief) and how it is to work (System). The runner hands the first to its
// CLI as the prompt and the second on the CLI's channel for adding to its own
// system prompt ([RunRequest.System]), so the vendor's harness prompt stays in
// force and the standing instructions sit beneath it rather than in front of
// the task as a message the model reads as part of the request.
type LaunchRequest struct {
	Turn TurnRef

	// System is the run's own standing instructions, empty for none. An
	// agent-mode executor's is the seat's system prompt; a run_sandbox job
	// has none of its own, its executor having written everything it means
	// into the Brief. It is FRAMED as the brief is: what the box provides,
	// and how to ask a person, follow it ([buildSystem]).
	System string

	// Brief is the task: an agent-mode executor's turn's ask, or the
	// executor's own description of the code task.
	Brief string

	// Wider is the goal the Brief serves, framed after it ([buildBrief]): a
	// run_sandbox job's turn task, of which its executor's brief is one
	// step. Empty for an agent-mode executor, whose Brief IS the turn's ask
	// — its task again under a second heading was paid for on every launch,
	// and read as a goal beyond the one it was given.
	Wider string

	// Task is the ask the suspended turn was working on, carried onto the
	// row so a resume has the brief when the trigger is long gone.
	Task string

	// Label is what a person sees the run named by — on the running-runs
	// panel, live and after a restart alike ([LaunchRequest.label]). Empty
	// takes the brief's first line, which for a run_sandbox job is the
	// concrete code task its executor wrote. An agent-mode executor names
	// its turn's task: its brief is the turn's whole message, which opens
	// with a heading rather than with what the run is doing.
	Label string

	// Ask is the addendum telling the coding agent how to stop and ask a
	// person — the ask shim's usage, with the names it may address. The
	// ENGINE composes it, because the names are the chart's and this
	// package holds none; empty adds nothing. A standing instruction, so it
	// rides System's channel ([buildSystem]).
	Ask string

	Spec       Spec
	Setup      []SetupStep
	MCPServers map[string]MCPServer

	// Model is the model the run works under — see [RunRequest.Model].
	// Empty names none, and the CLI chooses its own.
	Model string

	// ReuseBox is a box this turn already has, paused from an earlier
	// run_sandbox call. Reattaching keeps the checkout; an empty value
	// provisions a fresh box, and the work re-seeds from the pushed branch.
	ReuseBox string

	// Opened is told the job's name the moment the store has opened it
	// ([PendingStore.BeginLaunch]) and BEFORE ANY BOX EXISTS, for what must
	// name the job before its box can act: an agent-mode run's bridge
	// session, which records every call the box makes under its own job
	// ([BridgeAppend.Launch]). Learned any later — from the row, say, on the
	// session's first append — the name would be whichever job the row held
	// by then, which for a call that outlived its job is the next one. Nil
	// tells nobody.
	Opened func(launchID string)

	// CodeHolds is the turn's code-host hold set as the launch read it
	// (internal/agent/turnctx's Holds.Seal): every scratch repository the turn
	// has named, which the row carries from here on ([PendingRun.CodeHolds]) —
	// written by [PendingStore.BeginLaunch], unioned with what the row
	// already holds. The launch's caller keeps the turn's set SEALED from
	// reading it until Opened, so no hold is taken in between that the row
	// would not carry.
	CodeHolds []string

	// Clone are the engine's own steps giving the box the repositories it is
	// granted on the engine's code host ([Spec.Repos]): [GitVersionStep],
	// then a [CloneRecipe] for each. Applied after Setup on a fresh box, and
	// ALONE on a reused one — where each step's own guard skips a repository
	// already checked out — so a second run_sandbox call in one turn is given
	// a repository the first did not name, and keeps the first's checkout and
	// the work in it. Kept apart from Setup, which is the operator's and is
	// never applied twice to one box.
	Clone []SetupStep

	// CodeFloor is the run's first code floor — the furthest position its
	// repositories were resolved at, as statelog.Position's token — written on
	// the row with Spec.Repos ([PendingRun.CodeFloor]). Set exactly when
	// Spec.Repos is.
	CodeFloor string

	// RunEnv is the engine's environment for THIS LAUNCH: what a box needs
	// that is minted for the job itself, such as its access to the engine's
	// own code host (ADR-0063). Asked ONCE, AFTER the store has minted the
	// launch id the job's credentials name and before any box exists,
	// because a credential minted before the store names the job could only
	// name a job the row does not hold. Its variables win over every other
	// source of the run
	// environment for the names they set, and an error abandons the launch
	// like every failure past the row. Nil adds nothing.
	RunEnv func(launchID string) (map[string]string, error)

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
// row that outlives the turn — and HOLDS THE SEAT from the moment that row is
// written.
//
// THE ORDER IS THE CONTRACT. The row is created and the box attached to it
// BEFORE the job starts: a crash in that window leaves a row naming a box
// nobody is using, which recovery can kill, while the reverse ordering leaves
// a box that nothing names and nothing can ever reclaim.
//
// On any failure after the box exists, the box is reclaimed before the error
// propagates — an unreferenced box is billed for until its TTL and collected
// by nobody.
//
// # The seat is held from the row, not from the announcement
//
// A run is counted into the seat's [Holding] set, and the seat's inbox hold
// taken ([SeatHold]), in the step that writes its row — before the box is
// provisioned, and so before the launching turn can return and its seat's
// consumer go on to the next thing it has. It used to be counted when its
// [types.SandboxRunStarted] was processed off the seat's control topic, which
// is a separate subscription the launching turn does not wait for: mail
// already drained beside the launching delivery (the next partition of the
// same batch) and mail fetched after the turn returned both reached a seat the
// screening called free, and ran a turn beside the job. The announcement still
// RECOUNTS the seat from the store wherever it is processed — that is what
// carries a held seat across a restart or a handoff — but nothing waits for it.
//
// A launch that fails after the row is written gives the count back as it
// abandons the row ([Coordinator.abandon]); one that fails to suspend into the
// row is settled by [Coordinator.FailRun], whose ending gives it back the same
// way.
//
// m is the manager the request's [Spec] was built with ([Manager.BuildSpec]),
// passed rather than read off the coordinator here: an apply landing between
// the two swaps the coordinator's, and a spec resolved against one catalogue
// is launched on that catalogue.
func (c *Coordinator) Launch(ctx context.Context, m *Manager, req LaunchRequest) (LaunchResult, error) {
	store, q := c.pending, c.queue
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
	// THE GRANTS ARE HELD TO THEIR GRAMMAR BEFORE THE ROW IS WRITTEN: the row
	// is what a box's request to the code host is admitted against, so a
	// repository it could not carry, or one it names twice, is never on it.
	if err := checkRepos(req); err != nil {
		return LaunchResult{}, err
	}
	// THE SEAT'S LEASE, which the row is stamped with and every write below
	// carries — see [CoordinatorOptions.Lease]. A seat this node does not
	// hold launches nothing: the turn asking is one its lease no longer
	// stands behind.
	fence, held := c.leaseOf(req.Turn.AgentHandle)
	if !held {
		return LaunchResult{}, notHeld(ctx, req.Turn.AgentHandle, req.Turn.TurnID, "launching a run")
	}

	// The row FIRST, so a crash between here and the box leaves a record
	// rather than nothing. It opens in [StatusLaunching] and stays there
	// until the turn writes the conversation a resume re-enters: nothing
	// polls or claims a run in that window, which is what stops a job that
	// finishes before the turn unwinds from being collected into nothing.
	opened, err := store.BeginLaunch(ctx, PendingRun{
		TurnID: req.Turn.TurnID, WorkKey: req.Turn.WorkKey,
		WorkSince:   req.Turn.WorkSince,
		AgentHandle: req.Turn.AgentHandle,
		AgentID:     req.Turn.AgentID, Role: req.Turn.Role,
		// WRITTEN WITH THE ROW, before the box exists, because every
		// failure path from here on reclaims through it — and because the
		// process that collects this run may not be this one, nor reading
		// the configuration that chose the cell.
		Placement:       string(req.Spec.Placement),
		CodingAgent:     req.Spec.CodingAgent,
		TaskDescription: req.Task,
		Label:           req.label(),
		PartitionKey:    req.Turn.PartitionKey,
		// The conversation an answer is matched on and the resume
		// reports back to, because a resume days later has neither the
		// trigger nor this frame.
		ConversationKey: req.Turn.ConversationKey,
		Reply:           req.Turn.Reply,
		TraceID:         req.Turn.TraceID, SpanID: req.Turn.SpanID,
		DelegationDepth: req.Turn.Depth, DelegationChain: req.Turn.Chain,
		WorkItem: req.Turn.WorkItem,
		// Who asked, for the park that resolves a question's audience:
		// that frame sees no trigger.
		Requester: req.Turn.Requester,
		// The model the run's phase record is filed under; the store keys
		// the rest of that record on the launch it mints.
		Launch: LaunchRecord{Model: req.Model},
		// WHAT THE TURN HOLDS FROM THE CODE HOST'S EXPIRY, on the row before
		// any box exists: once the turn parks, the row is the only place
		// the expiry can read it from.
		CodeHolds: req.CodeHolds,
		// THE JOB'S GRANTS AND ITS FIRST FLOOR, on the row before any box
		// exists for the same reason: a box's first request to the code
		// host is admitted against the row, and it can come before this
		// launch returns.
		Repos: req.Spec.Repos, CodeFloor: req.CodeFloor,
		CreatedAt: now(),
	}, fence)
	if err != nil {
		return LaunchResult{}, fmt.Errorf("sandbox: recording the run: %w", err)
	}
	// HELD FROM HERE, before anything below can fail or the turn can
	// return — see [Coordinator.Launch]. Every failure past this point
	// gives it back through [Coordinator.abandon].
	c.countRun(req.Turn.AgentHandle, StatusLaunching)
	// THE JOB IS NAMED BEFORE ITS BOX EXISTS, so nothing the box does can be
	// filed under a job that is not its own — see [LaunchRequest.Opened].
	if req.Opened != nil {
		req.Opened(opened.LaunchID)
	}

	// THE LAUNCH'S OWN ENVIRONMENT, now that the store has named the job
	// its credentials are minted for ([LaunchRequest.RunEnv]). It is what
	// the box is made with, what its setup and clone steps run with — the
	// clones need the job's credential — and, with the run's telemetry, what
	// the coding agent runs with; and it lives in this frame alone: the spec
	// is never persisted, and a resume hours later launches anew with
	// credentials of its own.
	env, err := launchEnv(req, opened.LaunchID)
	if err != nil {
		c.abandon(ctx, m, req, fence, "")
		return LaunchResult{}, fmt.Errorf("sandbox: the run's environment: %w", err)
	}
	launched := req
	launched.Spec.Env = env

	box, runner, reused, err := acquire(ctx, m, launched)
	if err != nil {
		// The row is open and this launch is over. Every failure past this
		// point closes it: a run left LAUNCHING is polled by nothing and
		// claimed by nothing, so it sits on its seat's busy count and its
		// box until the seat happens to move to another node.
		c.abandon(ctx, m, req, fence, "")
		return LaunchResult{}, err
	}
	// A REUSED BOX IS GIVEN WHAT IT LACKS: its own provisioning survives with
	// its disk state, but this job's repositories may not all be there, so
	// the clone steps run on it alone, each skipping a repository already
	// checked out. One that fails is this launch's failure, as a failed step
	// on a fresh box is: a brief promising a checkout the box does not hold
	// must never reach a run.
	if reused && len(req.Clone) > 0 {
		if err := ApplySetup(ctx, box, req.Clone, env); err != nil {
			log.ErrorContext(ctx, "sandbox_setup_failed", "sandbox_id", box.ID(), "reused", true,
				"error", err.Error())
			c.abandon(ctx, m, req, fence, box.ID())
			return LaunchResult{}, err
		}
	}

	// Attached before the job starts, for the reason above.
	if err = store.AttachSandbox(ctx, req.Turn.TurnID, BoxRef{
		SandboxID: box.ID(), CodingAgent: req.Spec.CodingAgent,
		PauseTTLSec: req.Spec.PauseTTLSec,
	}, fence); err != nil {
		c.abandon(ctx, m, req, fence, box.ID())
		return LaunchResult{}, fmt.Errorf("sandbox: attaching the box: %w", err)
	}

	handle, err := runner.Start(ctx, box, RunRequest{
		System: buildSystem(req),
		Brief:  buildBrief(req),
		// THE TELEMETRY ENDPOINT IS MINTED HERE, at the job's start, as it
		// always was rather than with the launch's environment above: its
		// token lives a fixed span from its minting ([otelTokenTTL]), and
		// whatever the box's provisioning took would come off the job's own.
		Env: withTelemetry(m, launched),
		// FROM THE SPEC, which is where the provider default and the
		// seat's override have already been reconciled. A Limits the
		// caller passed alongside would be a second answer to the same
		// question, and it was the one nobody filled in: every coding run
		// went out uncapped while the field looked wired.
		Limits:     Limits{MaxTurns: req.Spec.MaxTurns},
		Model:      req.Model,
		MCPServers: req.MCPServers,
	})
	if err != nil {
		c.abandon(ctx, m, req, fence, box.ID())
		return LaunchResult{}, fmt.Errorf("sandbox: starting the coding agent: %w", err)
	}

	// The command id is written SECOND, once the job exists: a row naming a
	// command that was never started would have the waiter poll for a
	// completion marker nothing is ever going to write.
	if err := store.AttachSandbox(ctx, req.Turn.TurnID, BoxRef{
		SandboxID: box.ID(), CommandID: handle.CommandID,
		CodingAgent: req.Spec.CodingAgent, SessionID: handle.SessionID,
		PauseTTLSec: req.Spec.PauseTTLSec,
	}, fence); err != nil {
		c.abandon(ctx, m, req, fence, box.ID())
		return LaunchResult{}, fmt.Errorf("sandbox: recording the job: %w", err)
	}

	// WHICH JOB THIS IS, as the store opened it: the announcement is what a
	// watcher pairs a start with its job's record by, and what a request for
	// its live output names. The store's own answer rather than a read of the
	// row back, which could fail and cost the pairing — and which could, in
	// principle, find a later job than this one.
	started := types.SandboxRunStarted{
		LaunchID: opened.LaunchID, StartedAt: opened.Launch.StartedAt,
		Agent: req.Turn.AgentID, AgentHandle: req.Turn.AgentHandle,
		RoleName: req.Turn.Role, TurnID: req.Turn.TurnID, WorkKey: req.Turn.WorkKey,
		SandboxID: box.ID(), CodingAgent: req.Spec.CodingAgent,
		// THE IDENTITY on the announcement: this event is read for
		// display, and what a person means by "which conversation is
		// this run for" is the durable thread rather than the batch the
		// trigger arrived in.
		ConversationKey: req.Turn.ConversationKey,
		WorkItem:        req.Turn.WorkItem,
		Task:            req.label(),
	}
	ev := events.New(started, events.TraceContext{
		TraceID: req.Turn.TraceID, ParentSpanID: req.Turn.SpanID,
	})
	ev.Source = req.Turn.Role

	// TWO PUBLISHES, as for a completion. The events copy is the
	// announcement the dashboard's running-sandboxes panel reads; the
	// per-seat control copy RECOUNTS the seat from the store on whichever
	// node holds it when the copy is processed. The seat is already held
	// here — the row's write counted it — so nothing waits for this; what
	// the copy adds is the store's own answer, after a restart or a handoff
	// as much as now.
	if err := q.Publish(ctx, topics.Event(started.EventType()), ev); err != nil {
		log.WarnContext(ctx, "sandbox_started_publish_failed",
			"turn_id", req.Turn.TurnID, "error", err.Error())
	}
	if control := controlSubject(req.Turn.AgentID); control != "" {
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

// acquire reattaches to this turn's existing box, or provisions a new one.
//
// Reuse keeps the CHECKOUT, which is the expensive half of a coding run: a
// second run_sandbox call in one turn continues where the first stopped rather
// than re-cloning. A reattach that fails falls through to a fresh box rather
// than failing the launch — the box is gone, which is exactly the case the
// pushed branch exists for.
//
// A fresh box is given the operator's setup and then the engine's clone
// steps, in that order, so a clone can rely on whatever the setup installed
// (a proxy, a CA bundle in the box's git configuration). A reused box is
// given its clone steps by the launch itself, which reclaims it if one fails.
func acquire(ctx context.Context, m *Manager, req LaunchRequest) (Sandbox, Runner, bool, error) {
	if req.ReuseBox != "" {
		box, runner, err := m.Reconnect(ctx, req.Spec.Placement, req.ReuseBox, req.Spec.CodingAgent)
		if err == nil {
			return box, runner, true, nil
		}
		log.WarnContext(ctx, "sandbox_reuse_failed",
			"turn_id", req.Turn.TurnID, "sandbox_id", req.ReuseBox, "error", err.Error())
	}
	box, runner, err := m.Acquire(ctx, req.Spec, append(slices.Clone(req.Setup), req.Clone...))
	if err != nil {
		return nil, nil, false, err
	}
	return box, runner, false, nil
}

// checkRepos refuses a launch whose repositories its row could not carry:
// more than [MaxRunRepos], one that is not a [RunRepo] ([RunRepo.Validate]),
// one named twice — by its name or by its id, since either would give one
// repository two grants and one checkout directory two clones — or a floor
// that does not go with them, set exactly when repositories are.
func checkRepos(req LaunchRequest) error {
	repos := req.Spec.Repos
	if len(repos) > MaxRunRepos {
		return fmt.Errorf("sandbox: a run is given at most %d repositories, not %d", MaxRunRepos, len(repos))
	}
	names := make(map[string]bool, len(repos))
	ids := make(map[string]bool, len(repos))
	for _, r := range repos {
		if err := r.Validate(); err != nil {
			return err
		}
		if names[r.Repo] || ids[r.RepoID] {
			return fmt.Errorf("sandbox: repository %s is given twice: one run works on one branch of a "+
				"repository", r.Repo)
		}
		names[r.Repo], ids[r.RepoID] = true, true
	}
	switch {
	case len(repos) > 0 && req.CodeFloor == "":
		return fmt.Errorf("sandbox: a run given repositories needs the code floor they were resolved at")
	case len(repos) == 0 && req.CodeFloor != "":
		return fmt.Errorf("sandbox: a run given no repositories has no code floor")
	}
	return nil
}

// launchEnv is the environment of THIS LAUNCH's box: the spec's run
// environment, and then the engine's own variables for the job
// ([LaunchRequest.RunEnv]), which win over every other source for the names
// they set — an operator's value for one of those would break what the engine
// minted it for, and configuration refuses the names where an operator writes
// the run environment. The box is made with it and every setup and clone step
// runs with it; the coding agent runs with it and the run's telemetry
// ([withTelemetry]).
//
// A COPY, never the spec's own map, which the caller's [Spec] goes on holding:
// what a box, its setup or its runner does with the map it is handed never
// reaches the configuration the next launch is built from.
func launchEnv(req LaunchRequest, launchID string) (map[string]string, error) {
	var added map[string]string
	if req.RunEnv != nil {
		var err error
		if added, err = req.RunEnv(launchID); err != nil {
			return nil, err
		}
	}
	out := make(map[string]string, len(req.Spec.Env)+len(added))
	maps.Copy(out, req.Spec.Env)
	maps.Copy(out, added)
	return out, nil
}

// abandon closes out a launch that could not finish: the box is reclaimed, then
// the run is ended, which deletes its record, and the seat the launch held is
// given back.
//
// ALL OF IT, EVERY TIME, and each step attempted whether or not the one before
// it succeeded: they are separate calls that fail separately, and none failing
// is a reason to leave the others undone. Three of the four failure paths used
// to do none of it and simply return, which left the row OPEN: a launching run
// is polled by nothing and claimed by nothing, so it held its seat and, on two
// of those paths, a box, until the seat happened to move to another node and
// recovery reaped it. The box goes first, so a record that cannot be deleted
// names a box that is already gone rather than the reverse.
//
// THE ENDING EVERY OTHER PATH MAKES ([Coordinator.endRecord]), not a delete of
// its own: that is the one place a run's record is deleted, so it is the one
// place a stop is reported and the one place what the row still owes the seat
// goes out before the row does (see [PendingRun.HandBack]) — a relaunch is
// opened on a row that has carried a whole run's history.
//
// A context of its own, because the failure that got us here is often the
// caller's context expiring — and a teardown skipped for that reason leaves a
// box running to its TTL with nobody to collect it, and a row nobody settles.
func (c *Coordinator) abandon(ctx context.Context, m *Manager, req LaunchRequest, fence Fence, sandboxID string) {
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
	// Every live status and every job: this launch is abandoned whatever the
	// row reached. TWO BOXES CAN BE OWED A RECLAIM, and each is reclaimed by
	// the one party that can name it. The launch's own is killed above,
	// because the launch may never have attached it to the row. The box the
	// ROW names is reclaimed by the ending, after its decision: a relaunch
	// that never got a box of its own leaves the row naming the previous
	// job's paused one, and the ending deletes the only record of it — so a
	// box left to the turn's own settle was named by nothing once the turn
	// came back, and was killed only off that settle's stale snapshot, which
	// a settle may no longer act on ([Coordinator.finish]). Where the two are
	// one box — the attach landed — it is killed twice, which every provider
	// answers as the reclaim it wanted: a kill of a box already gone succeeds.
	run := PendingRun{TurnID: req.Turn.TurnID, AgentHandle: req.Turn.AgentHandle}
	if _, err := c.endRecord(ctx, run, ending{
		fence: fence, whileIn: Active, launch: EveryLaunch, reclaim: true,
	}); err != nil {
		log.WarnContext(ctx, "sandbox_launch_finish_kept",
			"turn_id", req.Turn.TurnID, "error", err.Error(),
			"detail", "the abandoned launch's record is not deleted yet; this node finishes "+
				"the ending, and the seat's next recovery pass reaps the row if the seat "+
				"moves first")
	}
	// AND THE SEAT GOES BACK whether or not the record could be deleted:
	// the launch is over and its box reclaimed, so nothing is driving a job
	// the seat's mail must wait behind. A record that survived the delete is
	// a launching row whose ending this node keeps and retries.
	c.uncountRun(req.Turn.AgentHandle, StatusLaunching)
}

// label is what the run is named by, cut by [summarise]: the caller's own
// label where it gave one, else the brief's first line. ONE ANSWER for the
// announcement and the record, so a run a process learns of from the record
// after a restart is named exactly as the one it saw announced.
func (r LaunchRequest) label() string {
	if strings.TrimSpace(r.Label) != "" {
		return summarise(r.Label)
	}
	return summarise(r.Brief)
}

// summarise is a brief's first line, WHOLE: the label a running-runs panel
// draws for a run. The record keeps the label it cut ([PendingRun.Label]), so
// a process that learns of the run from the record after a restart names it as
// the announcement did, with no second rule of its own.
//
// The first line because the brief opens with the concrete task the executor
// asked for (see [buildBrief]) and the rest is the goal it serves. Never cut
// to a width: it used to be held to 120 bytes and marked, which put a
// sentence's first half on the run's page as its title — and a row too narrow
// for a label is the renderer's to elide, which the dashboard's rows already
// do, with the whole line still there to read.
func summarise(brief string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(brief), "\n")
	return strings.TrimSpace(line)
}

// buildBrief assembles what the coding agent is asked to do: the prompt its
// CLI runs.
//
// TWO PARTS, in the order an engineer reads them: the concrete task the
// executor asked for, then the wider goal it serves, where it has one
// ([LaunchRequest.Wider]). Nothing standing is here
// — what the box provides and how to ask a person are instructions for the
// whole run rather than part of this request, and they ride the system
// prompt's channel ([buildSystem]).
//
// There is no success-criteria section any more. It came from the planning
// phase's declared criteria, and with one phase deciding and acting there is
// no separate plan to declare them: what "done" means is the executor's own
// brief, written by the frame that will read the answer.
func buildBrief(req LaunchRequest) string {
	var b strings.Builder
	b.WriteString(req.Brief)
	if req.Wider != "" && req.Wider != req.Brief {
		b.WriteString("\n\n## The wider task\n")
		b.WriteString(req.Wider)
	}
	return b.String()
}

// buildSystem assembles the run's standing instructions: what its CLI adds to
// its own system prompt ([RunRequest.System]).
//
// THREE PARTS, each present only where it says something: the run's own
// instructions (an agent-mode executor's seat prompt), what the environment
// already provides, and how to stop and ask a person. The environment is the
// setup steps' own briefs — the mechanism and the hint together, so the agent
// does not spend rounds rediscovering that git auth is already wired — and,
// like the ask, it holds for every round of the run, which is what a system
// prompt is for. The runner adds the last standing instruction, the report
// it must write, because only it knows the box's path for it.
//
// THE RUN'S OWN TEXT FIRST, the framing after it: for an agent-mode executor
// that text is who the model is and what it answers for, and the box and the
// ask shim are details of where it is working.
//
// The environment lists the operator's steps and then the engine's clone
// steps ([LaunchRequest.Clone]), in the order a fresh box was given them, on a
// reused box too: the checkouts are there either way, and the run is told
// where each is and how it pushes.
func buildSystem(req LaunchRequest) string {
	names := slices.Collect(maps.Keys(req.MCPServers))
	own := strings.TrimSpace(req.System)
	if own != "" {
		own = agentRunNote + "\n\n" + own
	}
	parts := []string{
		own,
		strings.TrimSpace(EnvironmentBrief(append(slices.Clone(req.Setup), req.Clone...), names)),
		strings.TrimSpace(req.Ask),
	}
	return strings.Join(slices.DeleteFunc(parts, func(p string) bool { return p == "" }), "\n\n")
}

// agentRunNote opens a run's own instructions — an agent-mode executor's seat
// prompt — where they follow the coding CLI's system prompt.
//
// The vendor's prompt says what the model is, and says it first: Claude Code
// opens with "You are Claude Code, Anthropic's official CLI for Claude", and
// no flag removes it. A seat prompt that only implies who it is loses to that,
// so this says outright which of the two prompts is the role. The text-mode
// backend opens its calls with the same note
// (internal/providers/llm/cliagent harnessNote), less the sentence about
// Crewlet's JSON envelope: here the run's tools are its own and the seat's,
// called the way its CLI calls any tool. Constant, so it stays part of the
// prefix a vendor caches.
const agentRunNote = "You are running inside a command-line coding tool as one seat of a " +
	"Crewlet company, and Crewlet — not a person at a terminal — reads what you " +
	"report. The tool's own instructions describe the program you run in, not your " +
	"role: who you are and what you do are the instructions that follow, and they " +
	"win wherever the two disagree."

// withTelemetry adds the run's OTel environment to the spec's.
//
// MERGED PER LAUNCH, never into the configuration a spec is built from,
// because the endpoint carries a token scoped to THIS run and minted with a
// TTL. The spec itself is not persisted — the row keeps what a resume needs,
// and a resume hours later launches anew — so the launch environment
// ([launchEnv]) is per-launch for the reason its tokens are, and a resume is
// never handed a token that expired while the run was parked. And it is
// merged at the job's START, after the box is provisioned, never with the
// rest of the launch's environment before it: the token's span runs from its
// minting, so one minted before the setup steps would spend on them what the
// job's own export needs, and no setup step reads it.
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
