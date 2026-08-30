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

// Resumer re-enters a suspended Execute loop.
//
// The one seam between the sandbox layer and the agent layer, and it is
// deliberately narrow: the coordinator hands back the run's OPAQUE state blob
// and the text to answer the pending call with, and knows nothing about the
// conversation inside. That is what keeps this package free of agent imports —
// the state's shape lives in the agent layer, which is the only side that
// understands it.
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
}

// Accountant post-charges a collected run's tokens.
//
// The charge happens AFTER the spend, which is why it cannot refuse: a
// refusal cannot un-spend a collected run, and recording it anyway is the only
// way the meter stays true when the cap is binding. It reports whether the
// charge went over so the caller can say so.
type Accountant interface {
	Charge(ctx context.Context, agentID, handle string, tokens int) (over bool, err error)
}

// CoordinatorOptions configures a [Coordinator].
type CoordinatorOptions struct {
	Queue   Publisher
	Pending PendingStore
	Manager *Manager

	// Resume re-enters a suspended turn. Nil means this node cannot resume
	// anything, which is a valid build (an API-only process) and is
	// reported as [ErrResumeUnavailable] rather than silently settling runs.
	Resume Resumer

	// Account post-charges collected tokens. Nil skips accounting.
	Account Accountant

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
//   - COMPLETION → RESUME. A completion is claimed AT MOST ONCE, the result
//     collected with the box paused for reuse, tokens post-accounted, and the
//     suspended loop re-entered with the result spliced in. The seat stays
//     busy through all of it and is freed only at the last moment before the
//     resume, because freeing it earlier lets a queued event take the slot,
//     the resume fail, and the redelivery find the claim already flipped —
//     the suspended conversation permanently lost.
//   - RESTART RECOVERY. A node claiming a seat re-marks its running jobs busy
//     and reaps any tail the previous owner abandoned mid-resume.
type Coordinator struct {
	queue   Publisher
	pending PendingStore
	manager *Manager
	resume  Resumer
	account Accountant
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

// NewCoordinator validates the options and returns the coordinator.
func NewCoordinator(opts CoordinatorOptions) (*Coordinator, error) {
	if opts.Queue == nil || opts.Pending == nil || opts.Manager == nil {
		return nil, errors.New("sandbox: a coordinator needs a queue, a pending store and a manager")
	}
	c := &Coordinator{
		queue: opts.Queue, pending: opts.Pending, manager: opts.Manager,
		resume: opts.Resume, account: opts.Account, now: opts.Now,
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
		// answer — which arrives on its inbox.
		if run.Status == StatusRunning || run.Status == StatusResumed {
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
	run, won, err := c.pending.ClaimForResume(ctx, ev.TurnID)
	if err != nil {
		return fmt.Errorf("sandbox: claiming %s: %w", ev.TurnID, err)
	}
	if !won {
		// Already claimed by a duplicate signal — successive poll ticks both
		// firing before this claim landed, or an at-least-once redelivery —
		// or terminal.
		log.InfoContext(ctx, "sandbox_completion_already_claimed", "turn_id", ev.TurnID)
		return nil
	}

	result, err := c.collect(ctx, run)
	if err != nil {
		log.ErrorContext(ctx, "sandbox_collect_failed", "turn_id", run.TurnID, "error", err.Error())
		// The job is OVER even though collection failed. Free the seat and
		// settle the row whatever the cleanup manages: both are network
		// calls that can fail on their own, and neither failing is a reason
		// to leave a seat parked on a run that is finished.
		c.settleFailed(ctx, run)
		return nil
	}

	c.charge(ctx, run, result)

	if result.NeedsInput {
		// A clarification wait FREES the seat: a person can take days, and
		// the answer arrives on the seat's own inbox.
		c.clearBusy(run.AgentHandle)
		return c.park(ctx, run, result)
	}

	return c.resumeAndSettle(ctx, run, resumeText(result), result.Success, trigger)
}

// collect reconnects, reads the result, and PAUSES the box rather than tearing
// it down: the resumed Execute may call run_sandbox again to continue in the
// same checkout, and re-provisioning would throw away the working tree.
func (c *Coordinator) collect(ctx context.Context, run PendingRun) (Result, error) {
	manager := c.mgr()
	box, runner, err := manager.Reconnect(ctx, run.SandboxID, run.CodingAgent)
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

// charge post-accounts a collected run's tokens.
func (c *Coordinator) charge(ctx context.Context, run PendingRun, result Result) {
	tokens := result.InputTokens + result.OutputTokens
	if c.account == nil || tokens == 0 {
		return
	}
	over, err := c.account.Charge(ctx, run.AgentID, run.AgentHandle, tokens)
	if err != nil {
		log.WarnContext(ctx, "sandbox_accounting_failed", "turn_id", run.TurnID, "error", err.Error())
		return
	}
	if over {
		log.WarnContext(ctx, "sandbox_spend_over_budget",
			"agent_id", run.AgentID, "turn_id", run.TurnID, "tokens", tokens)
	}
}

// park records the question, settles the box per the pause policy, and
// announces the clarification.
//
// This is the wait PauseTTL exists for, and the only place the knob applies:
// every other pause in the lifecycle is settled by the tail that made it, but
// this one is open-ended. The box stays paused with its TTL now ticking for
// the waiter's reaper — unless the deployment set a zero TTL, which means
// "never hold a blocked box": tear it down now and park straight into reseed
// for zero holding cost. The answer resumes the work either way; only the
// starting point differs, a live checkout against the pushed branch.
func (c *Coordinator) park(ctx context.Context, run PendingRun, result Result) error {
	if err := c.pending.MarkAwaiting(ctx, run.TurnID, Clarification{
		Question: result.Question, Audience: result.AskTo,
		Branch: firstRef(result.DeliveredRefs), SessionID: result.SessionID,
	}); err != nil {
		return fmt.Errorf("sandbox: parking %s: %w", run.TurnID, err)
	}
	if run.PauseTTLSeconds == 0 {
		c.teardown(ctx, run)
		if err := c.pending.SetStatus(ctx, run.TurnID, StatusReseed, fenceOf(run)); err != nil {
			log.WarnContext(ctx, "sandbox_reseed_mark_failed", "turn_id", run.TurnID, "error", err.Error())
		}
	}
	log.InfoContext(ctx, "sandbox_run_awaiting_clarification",
		"turn_id", run.TurnID, "audience", result.AskTo)

	announcement := types.SandboxClarificationRequested{
		Agent: run.AgentID, AgentHandle: run.AgentHandle, RoleName: run.Role,
		TurnID: run.TurnID, SandboxID: run.SandboxID,
		Question: redact.Secrets(result.Question), Audience: result.AskTo,
		ConversationKey: run.ConversationKey,
	}
	ev := events.New(announcement, events.TraceContext{
		TraceID: run.TraceID, ParentSpanID: run.SpanID,
	})
	ev.Source = run.Role
	return c.queue.Publish(ctx, topics.Event(announcement.EventType()), ev)
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
	claimed, won, err := c.pending.ClaimForResume(ctx, run.TurnID)
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
	return true, c.resumeAndSettle(ctx, claimed, answerText(claimed, answer), true, trigger)
}

// resumeAndSettle re-enters the suspended loop, then settles the box.
//
// After the resumed Execute returns: if the executor called run_sandbox AGAIN
// the row is back in running and a new job owns the paused box — leave it for
// the next completion. Otherwise the phase is done with the box, so tear it
// down and mark the run done.
func (c *Coordinator) resumeAndSettle(ctx context.Context, run PendingRun,
	answer string, success bool, trigger *events.Event,
) error {
	if len(run.ExecuteState) == 0 {
		// No suspended conversation to resume — a crash between launching
		// the job and persisting the suspend. The turn cannot continue.
		log.WarnContext(ctx, "sandbox_resume_no_execute_state", "turn_id", run.TurnID)
		c.settleFailed(ctx, run)
		return nil
	}
	if c.resume == nil {
		return fmt.Errorf("%w: seat %q has no resumer on this node", ErrResumeUnavailable, run.AgentHandle)
	}

	// Freed only NOW, immediately before the resume, so no queued event can
	// take the slot first.
	c.clearBusy(run.AgentHandle)

	if err := c.resume.Resume(ctx, ResumeRequest{
		Run: run, Answer: answer, Success: success, Trigger: trigger,
	}); err != nil {
		// UN-CLAIM so a retry can win the flip again. Without this the NAK'd
		// completion redelivers, the claim refuses, and the suspended
		// conversation is permanently lost with the row stranded in resumed.
		// Reverted to the EXACT pre-claim status the claim snapshotted.
		revert := run.ClaimedFrom
		if revert == "" {
			revert = StatusRunning
		}
		log.ErrorContext(ctx, "sandbox_resume_failed",
			"turn_id", run.TurnID, "revert_to", revert, "error", err.Error())
		if setErr := c.pending.SetStatus(ctx, run.TurnID, revert, fenceOf(run)); setErr != nil {
			log.ErrorContext(ctx, "sandbox_resume_revert_failed",
				"turn_id", run.TurnID, "error", setErr.Error())
		}
		c.markBusy(run.AgentHandle)
		return err
	}

	latest, found, err := c.pending.Get(ctx, run.TurnID)
	if err != nil {
		log.WarnContext(ctx, "sandbox_settle_read_failed", "turn_id", run.TurnID, "error", err.Error())
		return nil
	}
	if found && latest.Status == StatusRunning {
		// The resumed executor called run_sandbox AGAIN: a new detached job
		// owns the box and the suspending turn re-marked the seat busy.
		log.InfoContext(ctx, "sandbox_reused_in_turn", "turn_id", run.TurnID)
		return nil
	}
	// Tear down the box the LATEST row points at: a re-seeded run
	// provisioned a fresh one, so the claimed row's id is stale.
	settle := run
	if found {
		settle = latest
	}
	c.teardown(ctx, settle)
	if err := c.pending.SetStatus(ctx, run.TurnID, StatusDone, fenceOf(run)); err != nil {
		log.WarnContext(ctx, "sandbox_done_mark_failed", "turn_id", run.TurnID, "error", err.Error())
	}
	return nil
}

// settleFailed marks a run failed, reaps its box, and frees the seat.
//
// Every step is attempted regardless of the ones before it: each is a network
// or store call that can fail on its own, and none of them failing is a reason
// to leave a seat parked on a run that is over.
func (c *Coordinator) settleFailed(ctx context.Context, run PendingRun) {
	if err := c.pending.SetStatus(ctx, run.TurnID, StatusFailed, fenceOf(run)); err != nil {
		log.WarnContext(ctx, "sandbox_failed_mark_failed", "turn_id", run.TurnID, "error", err.Error())
	}
	c.teardown(ctx, run)
	c.clearBusy(run.AgentHandle)
}

// teardown reclaims a run's box and clears the row's record of it.
func (c *Coordinator) teardown(ctx context.Context, run PendingRun) {
	if run.SandboxID == "" {
		return
	}
	if err := c.mgr().Provider().Kill(ctx, run.SandboxID); err != nil {
		log.WarnContext(ctx, "sandbox_teardown_failed",
			"turn_id", run.TurnID, "sandbox_id", run.SandboxID, "error", err.Error())
	}
	if err := c.pending.ReleaseBox(ctx, run.TurnID); err != nil {
		log.WarnContext(ctx, "sandbox_release_failed", "turn_id", run.TurnID, "error", err.Error())
	}
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
// sits paused. Reaping it is safe HERE and only here: taking the seat's lease
// is what proves no live process holds the row. A boot-time scan proves
// nothing of the sort, because a peer could be mid-resume on a seat this node
// never owned.
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
		case StatusResumed:
			log.WarnContext(ctx, "sandbox_abandoned_tail_reaped",
				"turn_id", run.TurnID, "agent", run.AgentHandle, "sandbox_id", run.SandboxID)
			c.teardown(ctx, run)
			if err := c.pending.SetStatus(ctx, run.TurnID, StatusFailed, Fence{}); err != nil {
				log.WarnContext(ctx, "sandbox_abandoned_mark_failed",
					"turn_id", run.TurnID, "error", err.Error())
			}
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
