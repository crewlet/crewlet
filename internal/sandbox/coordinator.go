package sandbox

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/redact"
)

// ErrResumeUnavailable reports that this node cannot resume the run a
// completion refers to.
//
// NOT a failure of the run — a failure of ROUTING. The suspended Execute
// conversation can only be resumed where the seat is, so the completion has to
// go back and reach that node instead of being settled here. Returning instead
// of raising would let the caller fall through to settling the run done and
// tearing the box down, discarding the agent's whole in-progress turn.
//
// The per-seat control topic makes this rare rather than routine — a
// completion reaches its seat's owner by construction — but "rare" is not
// "never": a lease can move between the publish and the receive.
var ErrResumeUnavailable = errors.New("sandbox: this node cannot resume the run")

// ErrResumeAbandoned reports a resumed turn that broke and must not be resumed
// again from the same completion.
//
// The counterpart of the dispatcher's own abandon decision, on the other path
// a turn can arrive by, and it exists for the same reasons. A resumed turn
// re-enters the executor's suspended conversation, so a redelivery re-runs
// every call the resumed round made, and a run that came back from a coding box
// is the turn most likely to have pushed a branch or opened a pull request
// already. A turn that PANICKED is abandoned too: the suspended conversation is
// the same bytes on every delivery, so the retry reaches the same defect.
//
// A resumer wraps its error in this when, and only when, the turn's own answer
// says so (in the engine, turn.Abandon). The coordinator then never reverts the
// claim, which is what stops a retry winning the flip: it settles the delivery
// and the run, reclaiming the box and deleting the record, so no redelivery
// finds anything to claim. Every other resume failure still reverts and comes
// back, because the suspended conversation is the expensive thing here and a
// resume that proved nothing has lost nothing by trying again.
var ErrResumeAbandoned = errors.New("sandbox: the resumed turn broke and must not be resumed again")

// Resumer re-enters a suspended Execute loop.
//
// The one seam between the sandbox layer and the agent layer, and it is
// deliberately narrow: the coordinator hands back the run's OPAQUE state blob
// and the text to answer the pending call with, and knows nothing about the
// conversation inside. That is what keeps this package free of agent imports —
// the state's shape lives in the agent layer, which is the only side that
// understands it.
//
// It must return an error wrapping [ErrResumeAbandoned] when the turn broke in a
// way a retry must not repeat; see that sentinel for which ways those are.
//
// It must return an error wrapping [ErrResumeUnavailable] when this node has
// no seat to resume into, so the caller reverts the claim instead of settling
// the run.
type Resumer interface {
	Resume(ctx context.Context, req ResumeRequest) error
}

// ResumeRequest is one re-entry into a suspended turn.
type ResumeRequest struct {
	Run PendingRun

	// Answer is what the pending run_sandbox call is answered with: the
	// coding agent's findings, or a person's reply to its question. Already
	// redacted — it becomes a tool message published as a phase record.
	Answer string

	// Success is whether the sandbox run itself succeeded, for the resumed
	// phase's own record.
	Success bool

	// Trigger is the event that caused the resume, carried so the resumed
	// turn's telemetry names what woke it.
	Trigger *events.Event

	// CostUSD and DeliveredRefs are what the run reported, for the resumed
	// phase's own event. The coding agents produce both and nothing carried
	// them: the phase record's `cost_usd` and `delivered_refs` had no
	// producer at all, so a subscription CLI's spend — which never passes
	// through the engine's token meter — was reported nowhere.
	//
	// Both are zero when a PERSON's answer resumes a parked clarification:
	// no new run finished, and claiming a cost for one would double-count
	// the run that is still going.
	CostUSD       float64
	DeliveredRefs []string
}

// Accountant post-charges a collected run's tokens.
//
// The charge happens AFTER the spend, so it cannot stop anything: the tokens
// are spent and the turn continues whatever the answer. It is RECORDED
// whatever the caps say, because a refusal cannot un-spend a run that already
// ran and a counter that dropped it would under-state the company by the whole
// run at exactly the moment its cap bound.
//
// So the boolean is not "nothing was recorded". It says the run took a counter
// PAST its cap, for the caller to report; the spend is on both counters either
// way, and the next round the seat or the company attempts is refused against
// the figure that includes it.
type Accountant interface {
	Charge(ctx context.Context, agentID, handle string, tokens int) (refused bool, err error)
}

// CoordinatorOptions configures a [Coordinator].
type CoordinatorOptions struct {
	Queue   Publisher
	Pending PendingStore
	Manager *Manager

	// Resume re-enters a suspended turn. Required: every node that builds a
	// coordinator runs the engine whose seats a completion resumes into,
	// and [NewCoordinator] refuses one without it.
	//
	// "This node cannot resume this run" is still an answer, and it is the
	// resumer's to give, by wrapping [ErrResumeUnavailable]: only the engine
	// knows which seats it holds and which conversation formats it reads.
	// That answer takes the path every failed resume takes, which gives the
	// claim back, so the node that does hold the seat can win it.
	Resume Resumer

	// Account post-charges collected tokens. Nil skips accounting.
	Account Accountant

	// Ended is called once for every run this node finishes with, whatever
	// finished it: collected, failed, torn down or reaped.
	//
	// It exists for credentials a run HOLDS rather than for its own state.
	// An agent-mode run's box dials the engine's tool bridge with a
	// per-run token, and a session left open is a box that outlived its
	// run keeping a working key to a live seat's whole surface. The token
	// expires on its own clock, so this is the difference between a
	// credential that dies with the job and one that dies in four hours.
	//
	// A PARKED run is deliberately not ended: it is waiting on a person,
	// not finished, and its box will resume into the same session.
	Ended func(runID string)

	// Now is the clock, injectable for tests.
	Now func() time.Time
}

// Coordinator is the engine's hands on the detached run_sandbox flow.
//
// Three transitions, and the ordering of each is what makes the flow safe:
//
//   - SUSPEND → BUSY. The suspending turn persists its conversation and the
//     seat is marked busy, so no queued event slips a turn in beside a run
//     that is still going.
//   - COMPLETION → RESUME. A completion is claimed AT MOST ONCE, and only
//     for the job it reports, the result collected with the box paused for
//     reuse, tokens post-accounted once per launch however often the tail is
//     retried, and the suspended loop re-entered with the result spliced in.
//     The seat stays busy through all of it and is freed only at the last
//     moment before the resume, because freeing it earlier lets a queued
//     event take the slot, the resume fail, and the redelivery find the claim
//     already flipped, with the suspended conversation permanently lost.
//   - RESTART RECOVERY. A node claiming a seat re-marks its running jobs busy
//     and reaps any tail the previous owner abandoned mid-resume.
type Coordinator struct {
	queue   Publisher
	pending PendingStore
	manager *Manager
	resume  Resumer
	account Accountant
	ended   func(runID string)
	now     func() time.Time

	// mu guards busy, the seat-level "is a detached run in flight?" answer
	// the inbox screening reads on every delivery.
	//
	// In memory rather than a store read per delivery: the seat's owner is
	// the only node that runs its turns, so its own memory is authoritative
	// for it, and RecoverSeat seeds it from the store when a node takes the
	// seat over. A store read on the hot path of every message would pay a
	// round trip to answer a question this process already knows.
	mu   sync.Mutex
	busy map[string]int
}

// NewCoordinator validates the options and returns the coordinator, or refuses
// a missing collaborator by name.
func NewCoordinator(opts CoordinatorOptions) (*Coordinator, error) {
	var missing []string
	for _, field := range []struct {
		name   string
		absent bool
	}{
		{"Queue", opts.Queue == nil},
		{"Pending", opts.Pending == nil},
		{"Manager", opts.Manager == nil},
		{"Resume", opts.Resume == nil},
	} {
		if field.absent {
			missing = append(missing, "CoordinatorOptions."+field.name)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("sandbox: a coordinator needs %s: a detached run is "+
			"published, recorded, reconnected to and resumed through them",
			strings.Join(missing, ", "))
	}
	c := &Coordinator{
		queue: opts.Queue, pending: opts.Pending, manager: opts.Manager,
		resume: opts.Resume, account: opts.Account, ended: opts.Ended,
		now:  opts.Now,
		busy: map[string]int{},
	}
	if c.now == nil {
		c.now = time.Now
	}
	return c, nil
}

// SetManager swaps the sandbox manager, for a live reload of providers.sandbox.
func (c *Coordinator) SetManager(m *Manager) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.manager = m
}

// Manager is the coordinator's current manager, for a caller that needs to
// mint a box: the manager is swapped on an apply, so a caller holding its own
// reference would provision against a provider the company has replaced.
func (c *Coordinator) Manager() *Manager { return c.mgr() }

func (c *Coordinator) mgr() *Manager {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.manager
}

// AwaitingSandbox reports whether a seat is parked on a detached coding run.
//
// The engine's inbox screening reads this on every delivery: a job can run for
// hours, far past any broker ack window, so its seat's mail is PARKED —
// requeued — rather than consumed and held.
func (c *Coordinator) AwaitingSandbox(handle string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.busy[handle] > 0
}

// markBusy and clearBusy are counted rather than boolean, because a resumed
// Execute can launch a SECOND run before the first is settled: the seat is
// free only when the last of them is.
func (c *Coordinator) markBusy(handle string) {
	if handle == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.busy[handle]++
}

func (c *Coordinator) clearBusy(handle string) {
	if handle == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.busy[handle] <= 1 {
		delete(c.busy, handle)
		return
	}
	c.busy[handle]--
}

// OnEvent routes a seat's control-topic delivery.
func (c *Coordinator) OnEvent(ctx context.Context, ev *events.Event) error {
	if ev == nil {
		return nil
	}
	switch payload := ev.Data.(type) {
	case *types.SandboxRunStarted:
		return c.OnStarted(ctx, *payload)
	case *types.SandboxRunCompleted:
		return c.OnCompleted(ctx, *payload, ev)
	}
	return nil
}

// OnStarted marks the seat busy.
//
// Idempotent on a redelivery, which is why it consults the store rather than
// blindly incrementing: at-least-once means this can arrive twice, and a
// double increment would leave the seat parked forever after the run settled.
func (c *Coordinator) OnStarted(ctx context.Context, ev types.SandboxRunStarted) error {
	if ev.AgentHandle == "" {
		return nil
	}
	c.syncBusy(ctx, ev.AgentHandle)
	log.InfoContext(ctx, "sandbox_agent_busy",
		"agent", ev.AgentHandle, "turn_id", ev.TurnID, "sandbox_id", ev.SandboxID)
	return nil
}

// syncBusy sets the seat's busy count from the store's own answer.
//
// The store is the arbiter rather than an increment, so a redelivered start, a
// restart, and a seat takeover all converge on the same number instead of
// drifting apart. A store that cannot be read leaves the count ALONE: the
// alternatives are parking a free seat forever or freeing a busy one into
// overlapping turns, and keeping what we already believed is the only answer
// that makes neither mistake on its own.
func (c *Coordinator) syncBusy(ctx context.Context, handle string) {
	runs, err := c.pending.ListActiveForSeat(ctx, handle)
	if err != nil {
		log.WarnContext(ctx, "sandbox_busy_sync_failed", "agent", handle, "error", err.Error())
		return
	}
	n := 0
	for _, run := range runs {
		// A run parked on a question does NOT hold the seat: a person can
		// take days to answer, and the seat has to be able to receive that
		// answer — which arrives on its inbox. [Holding] is the set that
		// does, and it is a named list rather than a disjunction here
		// because the same question is asked in three places.
		if slices.Contains(Holding, run.Status) {
			n++
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if n == 0 {
		delete(c.busy, handle)
		return
	}
	c.busy[handle] = n
}

// OnCompleted claims the run, collects, accounts, then resumes the loop.
func (c *Coordinator) OnCompleted(ctx context.Context, ev types.SandboxRunCompleted, trigger *events.Event) error {
	run, won, err := c.pending.ClaimForResume(ctx, ev.TurnID, CompletionTail(ev.LaunchID))
	if err != nil {
		return fmt.Errorf("sandbox: claiming %s: %w", ev.TurnID, err)
	}
	if !won {
		// Not this signal's to run. Already claimed by a duplicate of it
		// (successive poll ticks both firing before the claim landed, an
		// at-least-once redelivery, the retry a failed resume opened), or
		// the row no longer holds its job running: parked on the question
		// that job asked, replaced by the next run_sandbox call, or over.
		log.InfoContext(ctx, "sandbox_completion_already_claimed",
			"turn_id", ev.TurnID, "launch_id", ev.LaunchID)
		return nil
	}

	result, err := c.collect(ctx, run)
	if err != nil {
		log.ErrorContext(ctx, "sandbox_collect_failed", "turn_id", run.TurnID, "error", err.Error())
		// The job is OVER even though collection failed. Free the seat and
		// settle the row whatever the cleanup manages: both are network
		// calls that can fail on their own, and neither failing is a reason
		// to leave a seat parked on a run that is finished.
		c.settleFailed(ctx, run, types.SandboxFailureCollect,
			"the coding job finished but its box could not be read back, so its "+
				"result is lost; the work it pushed, if any, is on its branch")
		return nil
	}

	// Carried on the claimed row from here, so that handing the claim back
	// hands the record back with it.
	run.Charged = c.charge(ctx, run, result)

	if result.NeedsInput {
		return c.park(ctx, run, result)
	}

	return c.resumeAndSettle(ctx, run, resumeText(result), result.Success, trigger, runOutcome{
		CostUSD: result.CostUSD, DeliveredRefs: result.DeliveredRefs,
	})
}

// runOutcome is what a finished run reported about itself, for the resumed
// phase's own record. Zero where no run finished — a person answering a parked
// clarification resumes the turn without collecting anything.
type runOutcome struct {
	CostUSD       float64
	DeliveredRefs []string
}

// collect reconnects, reads the result, and PAUSES the box rather than tearing
// it down: the resumed Execute may call run_sandbox again to continue in the
// same checkout, and re-provisioning would throw away the working tree.
func (c *Coordinator) collect(ctx context.Context, run PendingRun) (Result, error) {
	manager := c.mgr()
	box, runner, err := manager.Reconnect(ctx, Placement(run.Placement), run.SandboxID, run.CodingAgent)
	if err != nil {
		return Result{}, err
	}
	result, err := runner.Collect(ctx, box, RunHandle{
		CommandID: run.CommandID, SessionID: run.SessionID,
	})
	if err != nil {
		return Result{}, err
	}
	if err := box.Pause(ctx); err != nil {
		log.WarnContext(ctx, "sandbox_pause_failed", "turn_id", run.TurnID, "error", err.Error())
	} else if err := c.pending.MarkBoxPaused(ctx, run.TurnID, c.now()); err != nil {
		log.WarnContext(ctx, "sandbox_pause_record_failed", "turn_id", run.TurnID, "error", err.Error())
	}
	return result, nil
}

// charge post-accounts a collected run's tokens, ONCE per launch, and reports
// whether the run's spend is on the counter.
//
// The charge sits inside the part of the tail that is retried: a resume that
// fails hands the claim back, the completion comes back to this node or to the
// seat's next owner, and the retry collects the same finished job again. So
// the run's own row records the charge, and a retry that finds it there
// charges nothing. In memory it would be lost to exactly the two retries that
// matter, the one on another node and the one after a restart.
//
// THE RECORD RIDES ON THE RELEASE. The only write that lets a retry reach this
// charge again is the one that hands the claim back, so that write carries it
// (see [PendingRun.Charged]): the run is reopened with its record or not at
// all, whatever else the store refuses in between. A process that dies holding
// the claim leaves the run resumed, which no signal claims and the seat's next
// owner reaps.
//
// A CHARGE THE COUNTER NEVER ANSWERED IS NOT RECORDED: it may or may not have
// landed, and offering it again can only over-state the counter, which trips a
// cap early rather than late — the direction the counter itself takes when a
// node dies mid-charge.
//
// A charge that went OVER A CAP is recorded like any other, because it landed
// like any other: the post-charge records the spend whatever the caps say (see
// [Accountant]). Reading that answer as "unrecorded" and offering it again is
// how one over-cap run is charged once per completion retry, which is the
// double-charge this record exists to stop.
func (c *Coordinator) charge(ctx context.Context, run PendingRun, result Result) bool {
	tokens := result.InputTokens + result.OutputTokens
	if run.Charged {
		log.InfoContext(ctx, "sandbox_charge_already_recorded",
			"agent_id", run.AgentID, "turn_id", run.TurnID, "tokens", tokens)
		return true
	}
	if c.account == nil || tokens == 0 {
		return false
	}
	over, err := c.account.Charge(ctx, run.AgentID, run.AgentHandle, tokens)
	if err != nil {
		log.WarnContext(ctx, "sandbox_accounting_failed", "turn_id", run.TurnID, "error", err.Error())
		return false
	}
	if over {
		log.WarnContext(ctx, "sandbox_spend_over_budget",
			"agent_id", run.AgentID, "turn_id", run.TurnID, "tokens", tokens)
	}
	// RECORDED EITHER WAY, because the counters moved either way.
	return true
}

// park announces the question, records it, and settles the box per the pause
// policy.
//
// EVERY STEP THAT CAN FAIL RUNS UNDER THE CLAIM, and a failure hands the claim
// back. A completion claims only its own job while that job is running, so
// once the row says awaiting, no redelivery of the completion can reach the
// question again. A question recorded but never announced would then wait for
// an answer nobody was asked for, and a question that could not be recorded
// left the row resumed, its paused box held until the seat changed hands. So
// the announcement goes first and the record second, and either failing
// reverts the claim for the completion's retry to ask again.
//
// The seat is freed only once the question is on the row. A clarification wait
// FREES it, because a person can take days and the answer arrives on the
// seat's own inbox; freed any earlier, a reply that arrived in between would
// be taken as a new turn instead of being held until it has a run to answer.
//
// This is the wait PauseTTL exists for, and the only place the knob applies:
// every other pause in the lifecycle is settled by the tail that made it, but
// this one is open-ended. The box stays paused with its TTL now ticking for
// the waiter's reaper, unless the deployment set a zero TTL, which means
// "never hold a blocked box": tear it down now and park straight into reseed
// for zero holding cost. The answer resumes the work either way; only the
// starting point differs, a live checkout against the pushed branch.
func (c *Coordinator) park(ctx context.Context, run PendingRun, result Result) error {
	announcement := types.SandboxClarificationRequested{
		Agent: run.AgentID, AgentHandle: run.AgentHandle, RoleName: run.Role,
		// UnitOfWork, never the raw field: see [PendingRun.UnitOfWork].
		TurnID: run.TurnID, WorkKey: run.UnitOfWork(), SandboxID: run.SandboxID,
		Question: redact.Secrets(result.Question), Audience: result.AskTo,
		ConversationKey: run.ConversationKey,
	}
	ev := events.New(announcement, events.TraceContext{
		TraceID: run.TraceID, ParentSpanID: run.SpanID,
	})
	ev.Source = run.Role
	if err := c.queue.Publish(ctx, topics.Event(announcement.EventType()), ev); err != nil {
		c.unclaim(ctx, run, true)
		return fmt.Errorf("sandbox: announcing the question %s asked: %w", run.TurnID, err)
	}

	if err := c.pending.MarkAwaiting(ctx, run.TurnID, Clarification{
		Question: result.Question, Audience: result.AskTo,
		Branch: firstRef(result.DeliveredRefs), SessionID: result.SessionID,
	}); err != nil {
		c.unclaim(ctx, run, true)
		return fmt.Errorf("sandbox: parking %s: %w", run.TurnID, err)
	}
	c.clearBusy(run.AgentHandle)

	if run.PauseTTLSeconds == 0 {
		c.teardown(ctx, run)
		if err := c.pending.SetStatus(ctx, run.TurnID, StatusReseed, fenceOf(run)); err != nil {
			log.WarnContext(ctx, "sandbox_reseed_mark_failed", "turn_id", run.TurnID, "error", err.Error())
		}
	}
	log.InfoContext(ctx, "sandbox_run_awaiting_clarification",
		"turn_id", run.TurnID, "audience", result.AskTo)
	return nil
}

// TryResumeFromAnswer resumes a parked run if this event answers its question.
//
// Reports whether it handled the event, so the caller skips normal handling.
// The disambiguation is positional: the next inbound on the question's
// conversation while a clarification is pending IS the answer.
func (c *Coordinator) TryResumeFromAnswer(ctx context.Context, handle, conversation, answer string, trigger *events.Event) (bool, error) {
	if conversation == "" {
		return false, nil
	}
	run, found, err := c.pending.FindAwaitingByConversation(ctx, handle, conversation)
	if err != nil {
		// FAIL OPEN. An unreadable store must not swallow an ordinary
		// message: handling it as a normal inbound is recoverable, dropping
		// it is not.
		log.WarnContext(ctx, "sandbox_answer_lookup_failed",
			"agent", handle, "conversation", conversation, "error", err.Error())
		return false, nil
	}
	if !found {
		return false, nil
	}
	// THE JOB THAT ASKED, still waiting. The lookup is a snapshot, and a
	// claim that took whatever the row held by now would hand this answer
	// to the next job, which asked nothing.
	claimed, won, err := c.pending.ClaimForResume(ctx, run.TurnID, AnswerTail(run.LaunchID))
	if err != nil {
		return false, err
	}
	if !won {
		// Another inbound already claimed it. Report handled so this one is
		// not ALSO run as an unrelated message.
		return true, nil
	}
	log.InfoContext(ctx, "sandbox_clarification_answered",
		"turn_id", claimed.TurnID, "conversation_key", conversation)
	// The seat goes busy again for the duration of the resume: the parked
	// run freed it, and re-entering the Execute loop is work like any other.
	c.markBusy(claimed.AgentHandle)
	// NO OUTCOME: this resume collects no run. The box is still parked and
	// its cost is charged where it is collected, so reporting one here would
	// bill the same run twice.
	return true, c.resumeAndSettle(ctx, claimed, answerText(claimed, answer), true, trigger, runOutcome{})
}

// resumeAndSettle re-enters the suspended loop, then settles the box.
//
// After the resumed Execute returns: if the executor called run_sandbox AGAIN
// the row holds a new job that owns the paused box, so it is left for that
// job's own tail. Otherwise the phase is done with the box, so the box is
// torn down and the run finished.
func (c *Coordinator) resumeAndSettle(ctx context.Context, run PendingRun,
	answer string, success bool, trigger *events.Event, outcome runOutcome,
) error {
	if len(run.ExecuteState) == 0 {
		// No suspended conversation to resume, and the turn cannot
		// continue without one.
		//
		// This is now unreachable through any live path — a run holds
		// [StatusLaunching] until its conversation is written, and a
		// launching run is not claimable — which is exactly why it stays:
		// it is the assertion that the launching state is doing its job.
		// What can still land here is a row a build predating that state
		// wrote, read by this one across a rolling upgrade.
		log.WarnContext(ctx, "sandbox_resume_no_execute_state",
			"turn_id", run.TurnID, "claimed_from", run.ClaimedFrom,
			"detail", "the row carried no suspended conversation; the turn "+
				"cannot be resumed and the run is failed")
		c.settleFailed(ctx, run, types.SandboxFailureNoConversation,
			"the run record carried no suspended conversation, so the turn that "+
				"started it cannot be continued")
		return nil
	}
	// Freed only NOW, immediately before the resume, so no queued event can
	// take the slot first.
	c.clearBusy(run.AgentHandle)

	// THE CONVERSATION WHOLE: a row that could not hold its suspension holds
	// a reference to the parts it is kept in, and the resume re-enters what
	// they hold. A reference is never empty, so the check above has already
	// asked the right question of it.
	state, err := c.pending.Suspension(ctx, run)
	switch {
	case errors.Is(err, ErrSuspensionUnreadable):
		// Its parts are what the row names, and a retry reads the same ones:
		// the conversation is lost, like a row that never carried one.
		log.ErrorContext(ctx, "sandbox_resume_suspension_unreadable",
			"turn_id", run.TurnID, "launch_id", run.LaunchID, "error", err.Error(),
			"detail", "the run's suspended conversation was kept in parts that do not make the "+
				"whole its record names; the turn cannot be resumed and the run is failed")
		c.settleFailed(ctx, run, types.SandboxFailureNoConversation,
			"the run's suspended conversation was kept in parts that could not be read back "+
				"whole, so the turn that started it cannot be continued")
		return nil
	case err != nil:
		// UN-CLAIMED, as a resume that failed is: the store could not be
		// read, and the retry the signal's redelivery brings may.
		log.ErrorContext(ctx, "sandbox_resume_failed",
			"turn_id", run.TurnID, "revert_to", claimedFrom(run), "error", err.Error())
		c.unclaim(ctx, run, false)
		return err
	}
	run.ExecuteState = state

	// STRAIGHT TO THE RESUMER, which [NewCoordinator] refuses to be built
	// without: "this node cannot resume this run" is the resumer's own
	// answer, wrapping [ErrResumeUnavailable], and it takes the failure
	// path below like every other failed resume.
	if err := c.resume.Resume(ctx, ResumeRequest{
		Run: run, Answer: answer, Success: success, Trigger: trigger,
		CostUSD: outcome.CostUSD, DeliveredRefs: outcome.DeliveredRefs,
	}); err != nil {
		if errors.Is(err, ErrResumeAbandoned) {
			// THE CLAIM IS NEVER GIVEN BACK. Reverting it here would hand
			// the completion to a retry, and that retry would re-enter the
			// same suspended conversation and either repeat the writes this
			// turn has already made or reach the same panic, which are the
			// two things a resume must not do twice. The turn is lost
			// either way, and neither is recoverable by trying again.
			//
			// So the run is SETTLED, which keeps that promise for good: a
			// finished run has no record for a redelivery to claim. Left
			// claimed instead, it was settled by nothing (recovery runs
			// only when the seat changes hands), and re-marking the seat
			// busy parked every message it received for as long as this
			// node kept it, beside a paused box nobody would collect.
			//
			// The seat's busy count is RECOUNTED from the store rather than
			// re-marked: the resumed turn is over, but it may have called
			// run_sandbox again before it broke, and that relaunch's start
			// event counted the seat busy on a job this settle has just
			// ended. And nothing is announced, because the turn did resume
			// and has already published its own failed completion.
			log.ErrorContext(ctx, "sandbox_resume_abandoned",
				"turn_id", run.TurnID, "error", err.Error(),
				"detail", "the run is settled rather than un-claimed, so the completion is "+
					"not redelivered into a conversation a retry must not re-enter")
			// Its bridged calls outlive this finish only in what the broken
			// turn published before it broke: an executor pass that finished
			// published them in its record, and a turn that broke before its
			// pass did loses them to the purge, lost with the turn.
			if settle, ok := c.current(ctx, run); ok {
				c.finish(ctx, settle, fenceOf(run))
			}
			c.syncBusy(ctx, run.AgentHandle)
			return nil
		}
		// UN-CLAIM so a retry can win the flip again. Without this the NAK'd
		// completion redelivers, the claim refuses, and the suspended
		// conversation is permanently lost with the row stranded in resumed.
		log.ErrorContext(ctx, "sandbox_resume_failed",
			"turn_id", run.TurnID, "revert_to", claimedFrom(run), "error", err.Error())
		c.unclaim(ctx, run, false)
		return err
	}

	latest, ok := c.current(ctx, run)
	if !ok {
		return nil
	}
	if latest.LaunchID != run.LaunchID && slices.Contains(Active, latest.Status) {
		// The resumed executor called run_sandbox AGAIN: a new detached job
		// owns the box and the suspending turn re-marked the seat busy.
		//
		// TOLD BY ITS LAUNCH, because its status can be any live one by
		// now: launching until the resumed turn writes its conversation,
		// running while it works, and claimed by its own completion, or
		// parked on a question of its own, when it finished before this
		// read. Reading for running alone once tore a second run_sandbox
		// call's box down underneath it, and reading for running or
		// launching tore down the checkout of a question the next job had
		// just asked and ended the run under it. A relaunch that is
		// already over (one that could not start) is settled like the
		// rest: the phase is done with whatever box it still names.
		log.InfoContext(ctx, "sandbox_reused_in_turn",
			"turn_id", run.TurnID, "status", latest.Status, "launch_id", latest.LaunchID)
		return nil
	}
	// The resume has returned, and it read every bridged call of this
	// launch whole before it re-entered the turn: the resumed phase's
	// published record is where those calls outlive the purge this finish
	// makes of them.
	c.finish(ctx, latest, fenceOf(run))
	// RECOUNTED, not assumed free. The count was cleared before the resume,
	// but a start event redelivered while the turn ran recomputes it from
	// the store, where this run, claimed, still held the seat; with no
	// recount here that seat stayed parked on a run that no longer exists
	// until the seat changed hands. The store now has no record of this run,
	// so the recount keeps only the seat's other live runs.
	c.syncBusy(ctx, run.AgentHandle)
	return nil
}

// current is a claimed run as its record stands after the resumed turn
// returned, false when the record could not be read.
//
// The LATEST record, because the box to settle is the one it names now: a
// re-seeded run provisioned a fresh box, so the claimed snapshot's id is stale,
// and a resumed executor that called run_sandbox again has a new job in it. A
// record that is already gone is answered with the snapshot, whose settle then
// reclaims a box that may still be there and finds nothing to delete. A read
// that fails settles nothing: the record is still active, so the seat's next
// recovery reaps it, where acting on the snapshot could kill a relaunched job.
func (c *Coordinator) current(ctx context.Context, run PendingRun) (PendingRun, bool) {
	latest, found, err := c.pending.Get(ctx, run.TurnID)
	if err != nil {
		log.WarnContext(ctx, "sandbox_settle_read_failed", "turn_id", run.TurnID, "error", err.Error())
		return PendingRun{}, false
	}
	if !found {
		return run, true
	}
	return latest, true
}

// unclaim hands a claimed tail back, so the signal's retry can win the flip
// again, and leaves the seat's busy count saying what the run holds after it.
//
// To the EXACT status the claim took it from, which the claim snapshotted: a
// run answered out of a clarification goes back to waiting for its answer,
// and one collected from a running job goes back to running for its
// completion. A tail left in resumed is refused by every retry and looked at
// by nothing but the seat's next owner.
//
// ONLY THE CLAIM THIS CALL TOOK (see [Release]). A run that has moved on from
// it, relaunched by the resumed turn or reaped by the seat's next owner, is
// left as it stands, and what holds the seat is then the store's answer rather
// than this claim's.
//
// THE SEAT FOLLOWS THE STATUS THE RUN GOES BACK TO. counted is whether the
// busy count still includes this run, which it does until the resume frees
// the seat, and only a park, whose claim is always out of running, hands one
// back before that. A run back in running holds the seat again; one back
// waiting on a person does not, and marking the seat busy for it parked every
// later delivery on a run no poll completes, with nothing left to take the
// mark back once its answer's retry had settled it.
//
// A CONTEXT OF ITS OWN, like [Coordinator.teardown], because this is a
// rollback and the failure it undoes is often the cancellation itself: a drain
// cancels the delivery's context, the resume breaks on it, and a release that
// inherited it wrote nothing. The run then stayed resumed, and the seat's next
// owner, the node the drain was handing it to, reaped it as abandoned instead
// of resuming it.
func (c *Coordinator) unclaim(ctx context.Context, run PendingRun, counted bool) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), discardGrace)
	defer cancel()
	to := claimedFrom(run)
	released, err := c.pending.ReleaseClaim(ctx, run.TurnID, Release{
		Launch: run.LaunchID, To: to, Charged: run.Charged, Fence: fenceOf(run),
	})
	switch {
	case err != nil:
		log.ErrorContext(ctx, "sandbox_claim_revert_failed",
			"turn_id", run.TurnID, "revert_to", to, "error", err.Error())
		c.syncBusy(ctx, run.AgentHandle)
	case !released:
		log.WarnContext(ctx, "sandbox_claim_moved_on",
			"turn_id", run.TurnID, "launch_id", run.LaunchID,
			"detail", "the run no longer holds this claim, so nothing was handed back: "+
				"the resumed turn launched another job, or the seat's next owner reaped it")
		c.syncBusy(ctx, run.AgentHandle)
	case !counted && slices.Contains(Holding, to):
		c.markBusy(run.AgentHandle)
	}
}

// claimedFrom is the status a claimed run held before its claim. A claim
// always records it, and running is the only status a completion takes a tail
// out of, so it is the one a row without the record goes back to.
func claimedFrom(run PendingRun) string {
	if run.ClaimedFrom == "" {
		return StatusRunning
	}
	return run.ClaimedFrom
}

// settleFailed ends a run that failed: it reaps the box, finishes the run,
// frees the seat, and SAYS SO.
//
// Every step is attempted regardless of the ones before it: each is a network
// or store call that can fail on its own, and none of them failing is a reason
// to leave a seat parked on a run that is over.
//
// The announcement is the step that was missing. This path destroys a turn:
// the record is deleted, the box is gone, and the completion that would have
// explained it has already been acked. It published nothing, so every way of
// reaching here presented to the seat, the dashboard and the requester as an
// identical silence. The first symptom was a wait that
// never ended. `park` announces a QUESTION; a lost turn cannot be quieter than
// that.
//
// Announced only when this call ended the run. One that a newer lease owns,
// or that somebody else ended first, is that party's to settle and to explain,
// and a second announcement would name a reason the run did not end for.
//
// A FAILED RUN'S BRIDGED CALLS ARE LOST WITH IT. No resume collected them, so
// no phase record carries them, and the finish purges its log unread: what
// the run did survives only as the effects its calls had where they reached,
// and the announcement is the record that it ran and was lost.
func (c *Coordinator) settleFailed(ctx context.Context, run PendingRun, reason, detail string) {
	ended := c.finish(ctx, run, fenceOf(run))
	c.clearBusy(run.AgentHandle)
	if ended {
		c.announceFailure(ctx, run, reason, detail)
	}
}

// FailRun settles a run the turn that launched it cannot suspend into.
//
// The engine calls it when the conversation a resume would re-enter never
// reached the run's record: the runner recorded none, it would not serialize,
// or the record could not be written. The job is already executing in its box
// by then, and marking the record alone left that box running to its
// provider's TTL with nothing to reclaim it, because a record that is not
// active is read by no recovery pass. So the run is settled here like any
// other lost turn, while this node still owns the seat: the box reclaimed, the
// run finished, the seat freed and the loss announced.
//
// ONLY A RUN STILL LAUNCHING is settled, because that is the one state the
// failed suspension proves nothing else will act on. A run whose record is
// gone was ended by somebody else. A run already running carries a
// conversation after all: a write reported as failed can have landed, and that
// run is an ordinary suspended one the completion poll resumes, so settling it
// here would destroy a turn that is fine. Any other state belongs to a party
// that has moved the run on. A record that cannot be read is returned, because
// the box it names is then unknown.
func (c *Coordinator) FailRun(ctx context.Context, turnID, reason, detail string) error {
	run, found, err := c.pending.Get(ctx, turnID)
	if err != nil {
		return fmt.Errorf("sandbox: reading run %s to settle it: %w", turnID, err)
	}
	if !found || run.Status != StatusLaunching {
		return nil
	}
	c.settleFailed(ctx, run, reason, detail)
	return nil
}

// announceFailure publishes the lost run, to the board and to its seat.
//
// TWO PUBLISHES, as for a start and a completion: the events copy is what a
// dashboard and the event store read, and the per-seat control copy reaches
// the node that was running the turn. Best effort — the run is already
// settled, and a failed publish must not turn one lost turn into a stuck seat.
func (c *Coordinator) announceFailure(ctx context.Context, run PendingRun, reason, detail string) {
	failed := types.SandboxRunFailed{
		Agent: run.AgentID, AgentHandle: run.AgentHandle, RoleName: run.Role,
		// UnitOfWork, never the raw field: see [PendingRun.UnitOfWork].
		TurnID: run.TurnID, WorkKey: run.UnitOfWork(), SandboxID: run.SandboxID,
		CodingAgent: run.CodingAgent,
		Reason:      reason, Detail: redact.Secrets(detail),
	}
	ev := events.New(failed, events.TraceContext{
		TraceID: run.TraceID, ParentSpanID: run.SpanID,
	})
	ev.Source = run.Role
	if err := c.queue.Publish(ctx, topics.Event(failed.EventType()), ev); err != nil {
		log.WarnContext(ctx, "sandbox_failure_publish_failed",
			"turn_id", run.TurnID, "reason", reason, "error", err.Error())
	}
	if control := topics.AgentControl(run.AgentHandle); control != "" {
		if err := c.queue.Publish(ctx, control, ev); err != nil {
			log.WarnContext(ctx, "sandbox_failure_control_failed",
				"turn_id", run.TurnID, "reason", reason, "error", err.Error())
		}
	}
}

// teardown reclaims a run's box and clears the record's reference to it, for
// a run that goes on without its box: one parked straight into reseed.
func (c *Coordinator) teardown(ctx context.Context, run PendingRun) {
	killCtx, cancel := detached(ctx)
	defer cancel()
	_ = c.reclaimBox(ctx, killCtx, run)
	if run.SandboxID == "" {
		return
	}
	if err := c.pending.ReleaseBox(killCtx, run.TurnID); err != nil {
		log.WarnContext(ctx, "sandbox_release_failed", "turn_id", run.TurnID, "error", err.Error())
	}
}

// finish ends a run: its box is reclaimed and then its record deleted.
// Reports whether the ending is this call's, which is what licenses the caller
// to announce it: false when a newer lease owns the run or somebody else had
// already ended it.
//
// IN THAT ORDER, for the reason [PendingStore.Finish] gives: a record that
// outlives its box is reaped by the next recovery pass, while a box that
// outlives its record is named by nothing. A record that cannot be deleted is
// logged rather than retried here: it is still an active record of its seat,
// so the seat's next recovery pass reaps it, and the ending still counts as
// this call's, because the box is reclaimed and the turn is over.
//
// A kill that fails does not keep the record, unlike in [Coordinator.RetireSeat]:
// nothing retries a settle, and a record left claimed or launching would park
// its seat on the next busy count for as long as this node keeps the seat. The
// failure is logged, and a remote box runs out its TTL.
func (c *Coordinator) finish(ctx context.Context, run PendingRun, fence Fence) bool {
	if outranked(run, fence) {
		// A newer lease owns the run; its box is that owner's to reclaim.
		log.WarnContext(ctx, "sandbox_finish_outranked", "turn_id", run.TurnID,
			"owner_epoch", run.OwnerEpoch, "epoch", fence.Epoch)
		return false
	}
	killCtx, cancel := detached(ctx)
	defer cancel()
	_ = c.reclaimBox(ctx, killCtx, run)
	ended, err := c.pending.Finish(killCtx, run.TurnID, fence)
	if err != nil {
		log.WarnContext(ctx, "sandbox_finish_failed", "turn_id", run.TurnID, "error", err.Error(),
			"detail", "the run's box is reclaimed but its record was not deleted; the seat's "+
				"next recovery pass reaps it")
		return true
	}
	return ended
}

// detached is the context a teardown runs under.
//
// A CONTEXT OF ITS OWN, like [abandon] and [Manager.discard], and for the same
// reason: a teardown is reached right after a failure (settleFailed,
// resumeAndSettle) and from the queue handler a drain cancels, so the context
// that got us here is very often already dead. Inheriting it makes the kill and
// the store write no-ops, and the two failures are different and both bad. On
// a remote provider the box is left to run out its TTL, billed, with nothing
// left to collect it. On the local one Kill's wait for the process group is
// what stops removeBox racing the dying wrapper's writes, which is exactly
// what Kill's own comment says the wait prevents.
//
// WithoutCancel rather than Background, so the warnings still carry the turn's
// values.
func detached(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), discardGrace)
}

// reclaimBox kills a run's box under killCtx and closes the run's credentials,
// logging under ctx.
//
// The error is a kill that failed, the one outcome a retry can change. A run
// with no box, and a box whose placement this company no longer configures,
// report nil: there is nothing any later attempt could reach either.
func (c *Coordinator) reclaimBox(ctx, killCtx context.Context, run PendingRun) error {
	// BEFORE THE BOX CHECK, because a run that never got one still ended —
	// a launch that failed at create is exactly the case where a bridge
	// session was opened and nothing else will ever close it.
	if c.ended != nil {
		c.ended(run.TurnID)
	}
	if run.SandboxID == "" {
		return nil
	}
	provider, err := c.mgr().Provider(Placement(run.Placement))
	if err != nil {
		// The record names a cell this company no longer configures. The
		// box is unreachable and cannot be reclaimed here, so the caller
		// still settles the record: leaving it open would hold the seat's
		// busy count forever over a box that will expire on its own TTL.
		log.WarnContext(ctx, "sandbox_teardown_no_backend",
			"turn_id", run.TurnID, "placement", run.Placement, "error", err.Error())
		return nil
	}
	if err := provider.Kill(killCtx, run.SandboxID); err != nil {
		log.WarnContext(ctx, "sandbox_teardown_failed",
			"turn_id", run.TurnID, "sandbox_id", run.SandboxID, "error", err.Error())
		return fmt.Errorf("sandbox: reclaiming box %s of run %s: %w", run.SandboxID, run.TurnID, err)
	}
	return nil
}

// RecoverSeat re-attaches to a seat's still-active runs as this node claims it.
//
// PER-SEAT, and inside the acquire hook, because a fleet-wide boot scan is
// wrong twice over: every node would re-mark and reap runs belonging to seats
// its peers own, and a node that claims a seat LATER — a takeover, not a boot
// — would never recover it at all.
//
// Running jobs re-mark this seat busy; the waiter then drives them to
// completion. Clarification and reseed runs are left for their answer.
//
// A RESUMED row means the engine that owned this seat died between claiming a
// completion and settling it. Nothing will ever pick it up — the at-most-once
// claim already flipped, so a redelivered completion is refused — and its box
// sits paused. A LAUNCHING row is the same fact one step earlier: that engine
// died between starting the job and writing the conversation a resume would
// re-enter, so there is nothing to resume into and never will be. Both are
// unresumable tails holding a box, and both are reaped.
//
// Reaping is safe HERE and only here: taking the seat's lease is what proves
// no live process holds the row — and for a launching row that proof is the
// whole argument, because a launching row on a seat this node already owns is
// a launch happening right now, microseconds from becoming resumable. A
// boot-time scan proves nothing of the sort, because a peer could be
// mid-resume on a seat this node never owned.
func (c *Coordinator) RecoverSeat(ctx context.Context, handle, owner string, epoch int64) error {
	active, err := c.pending.ListActiveForSeat(ctx, handle)
	if err != nil {
		return fmt.Errorf("sandbox: recovering %s: %w", handle, err)
	}
	if len(active) == 0 {
		return nil
	}
	recovered, abandoned := 0, 0
	for _, run := range active {
		switch run.Status {
		case StatusLaunching, StatusResumed:
			log.WarnContext(ctx, "sandbox_abandoned_tail_reaped",
				"turn_id", run.TurnID, "agent", run.AgentHandle,
				"sandbox_id", run.SandboxID, "status", run.Status)
			// Fenced on the lease this node just took, so a record a
			// newer owner has already claimed is left to that owner, and
			// neither ended nor announced here. Its bridged calls go with
			// it: a launching run was never resumed, and a resumed one's
			// turn died with the previous owner, whose phase record — if
			// it published one before dying — is the only place they are.
			if !c.finish(ctx, run, Fence{Owner: owner, Epoch: epoch}) {
				continue
			}
			// Announced like the other ways a run is lost: the seat's new
			// owner is about to open its mailbox, and a turn that died
			// with the previous owner has to be visible rather than
			// inferred from a record that quietly left the board.
			c.announceFailure(ctx, run, types.SandboxFailureAbandoned,
				"the node that owned this seat stopped mid-run, so its turn "+
					"cannot be continued by the seat's new owner")
			abandoned++
		case StatusRunning:
			if _, err := c.pending.ClaimOwnership(ctx, run.TurnID, owner, epoch); err != nil {
				log.WarnContext(ctx, "sandbox_ownership_claim_failed",
					"turn_id", run.TurnID, "error", err.Error())
			}
			c.markBusy(run.AgentHandle)
			recovered++
		}
	}
	log.InfoContext(ctx, "sandbox_seat_recovered",
		"seat", handle, "epoch", epoch, "running", recovered,
		"abandoned", abandoned, "active", len(active))
	return nil
}

// RetireSeat ends every run of a seat that has left the company.
//
// Called by the seat's mailbox retirement, which is the one moment a removed
// seat's runs are known to be nobody's: the seat has been absent from the
// active revision for the whole retirement grace, and the caller HOLDS ITS
// LEASE under owner and epoch, so no node can claim the seat and recover these
// runs while they are ended. Until then they are kept on purpose, for the
// reason the mailbox is: a seat restored within the grace comes back to its
// parked questions and its running jobs rather than to lost turns.
//
// Every run is ended whatever its status, because none can continue: a resume
// needs the seat in the company, a parked question's answer arrives on an
// inbox about to be deleted, and a job's completion is routed to a control
// topic that goes with it. Each box is reclaimed, each loss announced, and
// each record finished under the retirement's fence.
//
// Everything runs under ctx, NOT detached like the settle paths: the lease is
// what keeps a returning seat's owner off these records, and it is only held
// for as long as the caller's budget. A run this call could not end is
// returned as an error, and the caller retries the retirement on its next
// tick; ending the same run twice reclaims a box that is already gone and
// finds a record that is already deleted.
func (c *Coordinator) RetireSeat(ctx context.Context, handle, owner string, epoch int64) error {
	runs, err := c.pending.ListActiveForSeat(ctx, handle)
	if err != nil {
		return fmt.Errorf("sandbox: listing the runs of retired seat %q: %w", handle, err)
	}
	fence := Fence{Owner: owner, Epoch: epoch}
	var errs []error
	for _, run := range runs {
		if err := ctx.Err(); err != nil {
			errs = append(errs, fmt.Errorf("sandbox: run %s of retired seat %q was not ended: %w",
				run.TurnID, handle, err))
			break
		}
		if outranked(run, fence) {
			// Claimed under a newer lease than the one this retirement
			// holds, which a held lease rules out; left to that owner
			// rather than killed out from under it.
			errs = append(errs, fmt.Errorf("sandbox: run %s of retired seat %q is owned by a newer "+
				"lease (epoch %d) than the retirement's (%d)", run.TurnID, handle, run.OwnerEpoch, epoch))
			continue
		}
		// A box that could not be reclaimed keeps its record, so the
		// retry the caller makes on its next tick still knows the box
		// exists. Deleting the record anyway would leave a billed box
		// named by nothing, which a settle accepts only because nothing
		// retries it.
		if err := c.reclaimBox(ctx, ctx, run); err != nil {
			errs = append(errs, fmt.Errorf("sandbox: run %s of retired seat %q was not ended: %w",
				run.TurnID, handle, err))
			continue
		}
		// The run's bridged calls go with it, unread: its seat has left the
		// company, so no resume will ever collect them, and the loss is
		// announced below.
		ended, err := c.pending.Finish(ctx, run.TurnID, fence)
		if err != nil {
			errs = append(errs, fmt.Errorf("sandbox: finishing run %s of retired seat %q: %w",
				run.TurnID, handle, err))
			continue
		}
		if !ended {
			// Settled by somebody else since the listing, who announced
			// it if it was lost.
			continue
		}
		c.announceFailure(ctx, run, types.SandboxFailureSeatRemoved,
			"the seat was removed from the company and not restored within the retirement "+
				"grace, so its run was ended; any work it pushed is on its branch")
		log.InfoContext(ctx, "sandbox_retired_seat_run_ended",
			"turn_id", run.TurnID, "agent", handle, "status", run.Status, "sandbox_id", run.SandboxID)
	}
	c.ReleaseSeat(handle)
	return errors.Join(errs...)
}

// ReleaseSeat stops tracking a seat's runs.
//
// NOTHING IS TORN DOWN. A detached run belongs to its row, not to this
// process, and the seat's next owner recovers it through RecoverSeat. Reaping
// the box here would destroy work the successor is about to resume.
func (c *Coordinator) ReleaseSeat(handle string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.busy, handle)
	log.Debug("sandbox_seat_released", "seat", handle)
}

// Busy is the seats this node believes hold a run, for the operator surface.
func (c *Coordinator) Busy() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Sorted(maps.Keys(c.busy))
}

func fenceOf(run PendingRun) Fence {
	return Fence{Owner: run.Owner, Epoch: run.OwnerEpoch}
}

func firstRef(refs []string) string {
	if len(refs) == 0 {
		return ""
	}
	return refs[0]
}

// resumeText is the run_sandbox reply spliced into the resumed Execute loop.
//
// It carries the coding agent's findings — reported text plus delivered refs,
// or the error — framed so the executor CONTINUES the task rather than redoing
// it. Secret-redacted, because it becomes a tool message published as a phase
// record.
func resumeText(result Result) string {
	status := "did NOT fully succeed"
	if result.Success {
		status = "succeeded"
	}
	lines := []string{"The sandbox coding run " + status + "."}
	if len(result.DeliveredRefs) > 0 {
		lines = append(lines, "Delivered: "+strings.Join(result.DeliveredRefs, ", "))
	}
	switch {
	case result.Text != "":
		lines = append(lines, "\n"+result.Text)
	case result.Error != "":
		lines = append(lines, "\nError: "+result.Error)
	}
	lines = append(lines, "\nThe sandbox did the code work above — do NOT redo it. Continue the "+
		"task: report the outcome to the requester on the channel it came from "+
		"(on success share the result / PR; on failure explain what blocked it "+
		"and what's needed), and take any remaining action with your tools.")
	return redact.Secrets(strings.Join(lines, "\n"))
}

// answerText is the run_sandbox reply spliced in when a parked clarification
// is answered.
//
// What it must NOT do is promise a box that no longer exists. A run whose
// sandbox id is empty had its box reclaimed — reaped past the pause TTL, or
// torn down the moment it blocked under a zero TTL — so the next call
// provisions a fresh one: git is the durable state, and the brief has to say
// so or the coding agent starts by looking for a working tree that is gone.
func answerText(run PendingRun, answer string) string {
	lines := []string{
		"The sandbox coding run paused to ask a person a question before it could finish.",
	}
	if run.Question != "" {
		lines = append(lines, "\nQuestion it asked: "+run.Question)
	}
	lines = append(lines, "Their answer: "+answer)
	if run.Branch != "" {
		lines = append(lines, "\nWork-in-progress is on git branch: "+run.Branch)
	}
	if run.SandboxID != "" {
		lines = append(lines, "\nThe sandbox is still up with that work-in-progress. Continue the "+
			"task: call run_sandbox again with a brief that incorporates this "+
			"answer so the coding agent finishes the work (it reuses the same "+
			"checkout). When it's done, report the outcome to the requester on "+
			"the channel this came from.")
	} else {
		branch := "check out the work-in-progress branch it pushed earlier"
		if run.Branch != "" {
			branch = "check out the existing branch `" + run.Branch + "`"
		}
		lines = append(lines, "\nThat sandbox has since been reclaimed, so the next run starts on "+
			"a FRESH machine — the earlier working tree and the coding agent's "+
			"memory of it are gone, but the pushed branch has the work. "+
			"Continue the task: call run_sandbox again with a brief that tells "+
			"it to "+branch+", re-read the code there, and finish the work with "+
			"this answer incorporated. When it's done, report the outcome to "+
			"the requester on the channel this came from.")
	}
	return redact.Secrets(strings.Join(lines, "\n"))
}
