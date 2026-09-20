package sandbox

import (
	"context"
	"errors"
	"fmt"
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

	// Stopped is called once for every way a run's engine-side life ENDS,
	// and for the one way it stops without ending — a park. It is the ONE
	// report above this package of a fact this package alone can see: that
	// nothing is working behind this seat and turn any more.
	//
	// THE CASES IT EXISTS FOR are the ones where a turn was still SUSPENDED
	// into the run — it parks on a question and waits for a person, it is
	// destroyed and never resumed, or a claim this node took cannot be given
	// back, which is the second wearing the first's clothes. In every one of
	// them the frame that raised whatever the engine holds up "while the
	// agent works" has already returned — it returned when the turn
	// suspended — so nothing above this package can learn that the agent
	// stopped unless this call makes it. The working indicator is the caller
	// it was added for: an "is thinking…" that outlives the thinking tells
	// the one person who could move the work that nobody is waiting on them.
	//
	// IT FIRES ON THE ORDINARY ENDINGS TOO, where a turn came back and took
	// its own hold down before the run was settled. The sentence is true
	// there as well, the report finds nothing to drop, and the alternative —
	// a gate asking whether this particular ending had a suspended turn
	// behind it — is a second opinion about what the store has already
	// answered and a fifth chance to get the enumeration wrong. See
	// [Coordinator.endRecord].
	//
	// ONE OPTION RATHER THAN ONE PER REASON, because three rounds of
	// hand-enumerating the ways a run stops each missed one, and a second
	// field is a second thing a wiring can declare and never pass. A caller
	// that needs to tell a wait from an ending reads the events that already
	// say which — [types.SandboxClarificationRequested] and
	// [types.SandboxRunFailed] — rather than a parameter nothing else needs.
	//
	// IT IS REPORTED WHERE THE TRANSITION IS MADE, NOT WHERE A LIST SAYS,
	// which is what this contract no longer has to enumerate. Every ending
	// deletes the run's record, and every deletion goes through
	// [Coordinator.endRecord], which reports; the one stop that is not an
	// ending, [Coordinator.park], reports for itself. So there are two
	// reporting sites for two kinds of transition, however many callers
	// reach them — a settle, a reap, a retirement, a lost claim, and
	// whatever ends a run next. This doc said "exactly two places" and then
	// named the two CALLERS it was thinking of, which was already three by
	// the time it was written: a retirement ends runs too, and it reported
	// separately because it cannot use [Coordinator.finish]. Counting
	// callers is the same hand-enumeration that missed four endings in a
	// row; keying the report to the deletion is what ends it.
	//
	// Both report only where the transition was THIS call's — the same gate
	// the failure announcement takes, since a run a newer lease owns or one
	// somebody else ended first is that party's to settle, to explain and to
	// drop the holds of. Over-reporting is harmless and under-reporting is
	// the defect, so a path that cannot tell reports: a delete that errored
	// may have landed, and it reports rather than staying quiet.
	//
	// After the durable write and before the announcement, never the other
	// way round: until the park is on the row the completion can still be
	// retried, and a retry that resumed the turn would find the hold already
	// dropped — while the announcement is a publish that can block, and what
	// comes down here is a claim on somebody's screen.
	//
	// NOT Ended, which fires for every finish including a collected run's:
	// there the turn is resumed immediately and the agent never stopped.
	// Ended is about a credential the run HOLDS and is keyed on the run; this
	// is about the turn, and is keyed on the seat and turn a caller addresses
	// its holds by.
	Stopped func(ctx context.Context, handle, turnID string)

	// Now is the clock, injectable for tests.
	Now func() time.Time
}

// Coordinator is the engine's hands on the detached run_sandbox flow.
//
// Four transitions, and the ordering of each is what makes the flow safe:
//
//   - SUSPEND → HELD. The suspending turn persists its conversation and the
//     seat is marked held, so no queued event slips a turn in beside a run
//     that is still going.
//   - COMPLETION → RESUME. A completion is claimed AT MOST ONCE, and only
//     for the job it reports, the result collected with the box paused for
//     reuse, tokens post-accounted once per launch however often the tail is
//     retried, and the suspended loop re-entered with the result spliced in.
//     The seat stays held through all of it and is freed only at the last
//     moment before the resume, because freeing it earlier lets a queued
//     event take the slot, the resume fail, and the redelivery find the claim
//     already flipped, with the suspended conversation permanently lost.
//   - PARK → ANSWER. A run that stops to ask a person something gives the
//     seat BACK — the answer arrives on that seat's own inbox, and a person
//     can take days — and leaves a question open on it instead. The seat then
//     works as usual, with one difference: every delivery is offered to
//     [Coordinator.TryResumeFromAnswer] before anything else consumes it,
//     because the reply that resumes an hours-old coding run is an ordinary
//     chat message and nothing about it says so. The wait is DURABLE FIRST
//     and everything else follows it: a park whose write does not land gives
//     the claim it holds back instead, and ends the run where even that
//     cannot be written — because a row left in the claim is picked up by
//     nothing at all ([Coordinator.revertClaim]).
//   - RESTART RECOVERY. A node claiming a seat re-marks its running jobs held,
//     inherits its open questions, and reaps any tail the previous owner
//     abandoned mid-resume.
type Coordinator struct {
	queue   Publisher
	pending PendingStore
	manager *Manager
	resume  Resumer
	account Accountant
	ended   func(runID string)
	stopped func(ctx context.Context, handle, turnID string)
	now     func() time.Time

	// mu guards runs, the two seat-level answers the inbox screening reads
	// on every delivery, and attempts beside them.
	//
	// In memory rather than a store read per delivery: the seat's owner is
	// the only node that runs its turns, so its own memory is authoritative
	// for it, and RecoverSeat seeds it from the store when a node takes the
	// seat over. A store read on the hot path of every message would pay a
	// round trip to answer a question this process already knows.
	mu   sync.Mutex
	runs map[string]seatRuns

	// attempts counts the failed handoffs of one delivery to one parked
	// run, so a resume that fails the same way every time stops circling
	// the seat's inbox. One BUDGET PER DELIVERY, under a key naming the
	// run it is owed to, and both bounds are stated at [MaxAnswerAttempts]
	// and [maxAnswerDeliveries] — which is also where it says why this is
	// per process.
	attempts map[answerKey]map[string]answerBudget
}

// seatRuns is this node's count of one seat's detached runs, by the question
// each set answers.
//
// TWO COUNTS, NOT ONE, and conflating them is what made the answer match
// unreachable for the whole life of the feature: the screening asked "is the
// seat held" and acted on the answer as though it meant "is a run waiting for
// somebody's reply", which are DISJOINT by construction — [Holding] and
// [Awaiting] share no status, deliberately, because a run parked on a question
// has to leave the seat free to receive the answer. So the one delivery the
// match exists for arrived at a seat the screening called free, and was
// consumed as an ordinary turn while the box waited out its pause TTL.
//
// COUNTED rather than boolean, because a resumed Execute can launch a SECOND
// run before the first is settled: the seat is free only when the last of them
// is, and one seat can drive a job while another of its runs waits for a
// person.
type seatRuns struct {
	// holding counts the runs in [Holding] — the ones the engine is
	// driving, during which the seat starts no new turn and its mail is
	// parked.
	holding int

	// awaiting counts the runs in [Awaiting] — the ones stopped on a
	// question. They hold nothing, which is the point, and this is what
	// tells the screening to offer a delivery to the match before anything
	// else consumes it.
	//
	// ERRING HIGH IS SAFE AND ERRING LOW IS NOT, which is what decides
	// every transition below that cannot prove a run left the set: one
	// count too many costs a single store lookup that finds nothing and
	// falls through to ordinary handling, while one too few is a person's
	// answer run as an unrelated turn and a box left to expire.
	awaiting int
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
		stopped:  opts.Stopped,
		now:      opts.Now,
		runs:     map[string]seatRuns{},
		attempts: map[answerKey]map[string]answerBudget{},
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

// SeatRuns is what this node knows about one seat's detached runs: whether one
// HOLDS the seat, and whether one is waiting for a person's answer.
//
// The engine's inbox screening reads it on every delivery, and reads BOTH
// values together because they change together. A run that parks on a question
// stops holding its seat and starts awaiting an answer in the same moment, so
// a caller that asked the two questions in two calls could land between the
// halves and be told neither is true — a seat that looks idle with no question
// open, which is precisely the state in which a person's reply is eaten as an
// ordinary turn.
func (c *Coordinator) SeatRuns(handle string) (held, awaitsAnswer bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	runs := c.runs[handle]
	return runs.holding > 0, runs.awaiting > 0
}

// SeatHeldBySandbox reports whether a detached run is HOLDING a seat, so it
// takes no new turn until the run settles: a job can run for hours, far past
// any broker ack window, so its seat's mail is PARKED — requeued — rather than
// consumed and held.
//
// NOT "is a run waiting for an answer", which is [Coordinator.SeatRuns]'s
// second value. This was called AwaitingSandbox, which reads as that other
// question and was acted on as though it were it; see [seatRuns].
func (c *Coordinator) SeatHeldBySandbox(handle string) bool {
	held, _ := c.SeatRuns(handle)
	return held
}

// countRun and uncountRun take a run into and out of the set its status
// belongs to, and moveRun carries one from one set to the other.
//
// A STATUS RATHER THAN A NUMBER at every call site, so a caller cannot count a
// run into one set while the store has it in the other — the two sets are what
// the screening branches on, and a run counted in the wrong one is either a
// seat parked on nothing or an open question nothing offers a reply to.
func (c *Coordinator) countRun(handle, status string)   { c.moveRun(handle, "", status) }
func (c *Coordinator) uncountRun(handle, status string) { c.moveRun(handle, status, "") }

// moveRun applies one transition under ONE lock, which is what
// [Coordinator.SeatRuns] needs: a park is a decrement and an increment, and a
// delivery screened between two separate writes would see a seat with no run
// of either kind.
func (c *Coordinator) moveRun(handle, from, to string) {
	if handle == "" {
		return
	}
	fromHolding, fromAwaiting := setOf(from)
	toHolding, toAwaiting := setOf(to)
	c.adjust(handle, toHolding-fromHolding, toAwaiting-fromAwaiting)
}

// setOf says which count a status belongs to. A status that owns no seat-level
// state — a run that has ended, or the empty string moveRun passes for "no set
// at all" — belongs to neither.
func setOf(status string) (holding, awaiting int) {
	switch {
	case slices.Contains(Holding, status):
		return 1, 0
	case slices.Contains(Awaiting, status):
		return 0, 1
	}
	return 0, 0
}

// adjust applies the deltas, CLAMPED AT ZERO.
//
// A decrement for a run this node never counted is ordinary rather than a bug
// — a seat taken over mid-run, a start event that never arrived, a settle for
// a run recovery already reaped — and left negative the count would then
// swallow the next real increment, which is a seat that runs a turn while a
// job holds it.
func (c *Coordinator) adjust(handle string, holding, awaiting int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	runs := c.runs[handle]
	runs.holding = max(runs.holding+holding, 0)
	runs.awaiting = max(runs.awaiting+awaiting, 0)
	if runs == (seatRuns{}) {
		delete(c.runs, handle)
		return
	}
	c.runs[handle] = runs
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

// OnStarted marks the seat held.
//
// Idempotent on a redelivery, which is why it consults the store rather than
// blindly incrementing: at-least-once means this can arrive twice, and a
// double increment would leave the seat parked forever after the run settled.
func (c *Coordinator) OnStarted(ctx context.Context, ev types.SandboxRunStarted) error {
	if ev.AgentHandle == "" {
		return nil
	}
	c.syncSeat(ctx, ev.AgentHandle)
	log.InfoContext(ctx, "sandbox_agent_busy",
		"agent", ev.AgentHandle, "turn_id", ev.TurnID, "sandbox_id", ev.SandboxID)
	return nil
}

// syncSeat sets both of a seat's counts from the store's own answer.
//
// The store is the arbiter rather than an increment, so a redelivered start, a
// restart, and a seat takeover all converge on the same numbers instead of
// drifting apart. A store that cannot be read leaves them ALONE: the
// alternatives are parking a free seat forever or freeing a busy one into
// overlapping turns, and keeping what we already believed is the only answer
// that makes neither mistake on its own.
//
// BOTH FROM ONE LISTING. A run parked on a question does NOT hold the seat —
// a person can take days to answer, and the seat has to be able to receive
// that answer, which arrives on its inbox — so each row lands in exactly one
// of the two counts. Recounting them from two listings would let a run that
// moved between the reads be counted in both or in neither.
func (c *Coordinator) syncSeat(ctx context.Context, handle string) {
	runs, err := c.pending.ListActiveForSeat(ctx, handle)
	if err != nil {
		log.WarnContext(ctx, "sandbox_busy_sync_failed", "agent", handle, "error", err.Error())
		return
	}
	var counts seatRuns
	for _, run := range runs {
		holding, awaiting := setOf(run.Status)
		counts.holding += holding
		counts.awaiting += awaiting
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if counts == (seatRuns{}) {
		delete(c.runs, handle)
		return
	}
	c.runs[handle] = counts
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

	// THE ERROR IS THIS ROUTE'S WHOLE ANSWER, and the disposition beside it
	// says nothing this caller can act on differently. A completion is
	// handed back by NAKing it, which spends one of the broker's own 25
	// deliveries — so the retry is bounded where it stands, and the budget
	// an answer needs ([MaxAnswerAttempts]) has no counterpart here: both
	// routes now hand a failed resume back the same way and take the same
	// backoff, and what an answer needs on top is a bound that stops SHORT
	// of the dead-letter boundary, because the delivery it is spending is a
	// person's reply rather than an engine-generated completion. Every
	// disposition that returns an error is the retry, and every one that
	// does not is an ending.
	_, err = c.resumeAndSettle(ctx, run, resumeText(result), result.Success, trigger, runOutcome{
		CostUSD: result.CostUSD, DeliveredRefs: result.DeliveredRefs,
	})
	return err
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
		// WARN RATHER THAN FAIL, because the job is over and refusing a
		// collected result over a timestamp would throw the whole run
		// away. What the missing stamp does NOT cost any more is the box:
		// a row that goes on to park names a held box whatever this
		// write did, and [PendingRun.HeldSince] dates the wait from the
		// park instead.
		log.WarnContext(ctx, "sandbox_pause_record_failed", "turn_id", run.TurnID, "error", err.Error(),
			"detail", "the pause instant was not recorded; a run that parks on a question is "+
				"expired from its park instead, and one that resumes settles its own box")
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
// the announcement goes first and the record second, and either failing gives
// the claim back for the completion's retry to ask again — or, where it cannot
// be given back, ENDS the run, because a row nothing will pick up is a turn
// destroyed in silence rather than one left for next time
// ([Coordinator.unclaim]).
//
// THE ONE STOP THAT IS NOT AN ENDING, which is why the report is made here and
// not only in [Coordinator.endRecord]: the run is alive and a person can move
// it, but the turn that suspended into it has stopped and will not come back
// to say so. A park whose write did not land is neither a stop nor a park, and
// reports nothing here — the settle that may follow makes its own report.
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
		// THE IDENTITY, like the launch announcement: this event is
		// display, and the durable thread is what a person reading the
		// feed means by the run's conversation.
		ConversationKey: run.Conversation(),
	}
	ev := events.New(announcement, events.TraceContext{
		TraceID: run.TraceID, ParentSpanID: run.SpanID,
	})
	ev.Source = run.Role
	if err := c.queue.Publish(ctx, topics.Event(announcement.EventType()), ev); err != nil {
		if unclaimErr := c.unclaim(ctx, run, true, parkUnannouncedDetail); unclaimErr != nil {
			//nolint:nilerr // Deliberate, as at the record below: the
			// claim did not go back, so the run has been ended and a
			// redelivered completion would find no record to claim.
			return nil
		}
		return fmt.Errorf("sandbox: announcing the question %s asked: %w", run.TurnID, err)
	}

	if err := c.pending.MarkAwaiting(ctx, run.TurnID, Clarification{
		Question: result.Question, Audience: result.AskTo,
		Branch: firstRef(result.DeliveredRefs), SessionID: result.SessionID,
	}); err != nil {
		// THE WAIT DID NOT LAND, so this run is not parked and this turn
		// is not waiting for anybody: the row is still in the claim that
		// brought us here. Handed back, the completion poll fires again,
		// the paused box is collected a second time and the park is
		// retried. Where it cannot be handed back the run is ended
		// instead, because a row left in the claim is picked up by
		// nothing at all.
		log.ErrorContext(ctx, "sandbox_park_write_failed",
			"turn_id", run.TurnID, "error", err.Error(),
			"detail", "the question could not be recorded, so the run is not parked; its "+
				"claim is given back for the completion to be retried, or the run is "+
				"ended where it cannot be")
		if unclaimErr := c.unclaim(ctx, run, true, parkUnrecordedDetail); unclaimErr != nil {
			//nolint:nilerr // Deliberate: the claim was not handed back,
			// so the run has been SETTLED and there is nothing left for
			// a redelivered completion to claim. Sending it back would
			// retry against no record. Both failures are logged and the
			// loss is announced.
			return nil
		}
		return fmt.Errorf("sandbox: parking %s: %w", run.TurnID, err)
	}
	// FREED AND OPENED AS ONE MOVE, not two: a person can take days, the
	// answer arrives on the seat's own inbox, and between two separate
	// writes the seat would read as idle with nothing open — the state in
	// which that answer is run as an unrelated turn.
	//
	// The claim took the row through [StatusResumed], so it is the holding
	// side it leaves.
	c.moveRun(run.AgentHandle, StatusResumed, StatusAwaiting)

	// THE AGENT HAS STOPPED, and this is the moment that becomes durable.
	// Whatever the engine holds up while a turn works comes down here — see
	// [CoordinatorOptions.Stopped].
	c.reportStopped(ctx, run)

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
// Reports what the CALLER must do with the delivery — see [AnswerDisposition],
// which is the whole contract and states what each answer costs when it is
// wrong. The error beside it is the explanation and never the decision: a
// caller logs it and acts on the disposition.
//
// The disambiguation is positional WITHIN A CONVERSATION: the next inbound on
// the conversation the question was asked in, while a clarification is
// pending, IS the answer. Hence the whole [ConversationRef] rather than one
// key — the identity is what the match turns on, and the partition rides
// along for the rows parked before an identity was written. Matching on the
// partition alone lost every answer the engine's own prompt pushed into a
// thread; see [ConversationRef.Answers].
//
// # Every failure, classified
//
// Nothing here reports an error and leaves the caller to guess what it meant,
// which is precisely what the bool beside it cost: the dispatcher read every
// error as "not handled" and spent the person's answer on an unrelated turn
// while the run that asked was still waiting for it.
//
//   - No conversation keys at all, and no run awaiting this one:
//     [AnswerNotMine]. Nothing was matched, so nothing is owed.
//   - The lookup itself failed: [AnswerNotMine], and deliberately — an
//     unreadable store must not swallow an ordinary message, and nothing was
//     matched, so nothing is owed. The one error resolved rather than
//     reported.
//   - The claim could not be written: [AnswerDeferred]. The claim MAY have
//     landed, so this is the ambiguous case, and the ambiguity is resolved
//     towards the run — an answer arriving twice is recoverable and an answer
//     spent is not. If it did land, the redelivery finds no awaiting row,
//     answers [AnswerNotMine] and the message becomes the turn it looks like,
//     while the claimed row is reaped by the seat's next recovery pass.
//   - The claim was lost to another inbound: [AnswerConsumed]. That delivery
//     is resuming the run, so this one must not ALSO be run as an unrelated
//     message.
//   - The resume ran, whatever it concluded: [AnswerConsumed]. Including a
//     resumed turn that broke after writing outside the engine
//     ([ErrResumeActed]), whose claim is deliberately never given back.
//   - The resume could not be made and the claim went back:
//     [AnswerDeferred] — the run is awaiting this same answer again, so the
//     delivery has to come back. [ErrResumeUnavailable] is the case this
//     exists for.
//   - The run is terminally gone — no suspended conversation to resume into,
//     or a claim that could not be given back and was settled instead:
//     [AnswerNotMine]. Requeueing for a run that no longer exists is a loop
//     with no end, and the message is an ordinary one now.
//
// See [Coordinator.resumeAndSettle], which makes the last three of those calls,
// and [MaxAnswerAttempts] for what bounds a requeue that keeps failing.
func (c *Coordinator) TryResumeFromAnswer(ctx context.Context, handle string, conv ConversationRef, answer string, trigger *events.Event) (AnswerDisposition, error) {
	if conv.Identity == "" && conv.Partition == "" {
		return AnswerNotMine, nil
	}
	run, found, err := c.pending.FindAwaitingByConversation(ctx, handle, conv)
	if err != nil {
		// FAIL OPEN. An unreadable store must not swallow an ordinary
		// message: handling it as a normal inbound is recoverable, dropping
		// it is not. NOT a defer, and the reasoning is the same one the
		// line below states — nothing was MATCHED, so no run is owed this
		// delivery and there is nothing for a requeue to come back to.
		// The error is resolved here rather than reported, because the
		// caller has nothing left to decide about it.
		// BOTH KEYS. The match turns on the identity and falls back to
		// the partition for a row parked before an identity was
		// written, so a line naming one of them cannot say which read
		// was attempted against what — and on a direct message the two
		// are different values.
		log.WarnContext(ctx, "sandbox_answer_lookup_failed",
			"agent", handle, "conversation", conv.Identity,
			"partition", conv.Partition, "error", err.Error())
		return AnswerNotMine, nil
	}
	if !found {
		return AnswerNotMine, nil
	}
	// THE JOB THAT ASKED, still waiting. The lookup is a snapshot, and a
	// claim that took whatever the row held by now would hand this answer
	// to the next job, which asked nothing.
	claimed, won, err := c.pending.ClaimForResume(ctx, run.TurnID, AnswerTail(run.LaunchID))
	if err != nil {
		// THE CLAIM MAY HAVE LANDED. A store that could not say is not a
		// store that said no, and the two would be told apart by nothing
		// on the delivery's side — so the run keeps its answer and the
		// message comes back. See the classification above.
		return c.deferAnswer(ctx, run, trigger,
			fmt.Errorf("sandbox: claiming %s for the answer it is waiting on: %w", run.TurnID, err))
	}
	if !won {
		// Another inbound already claimed it: that delivery is resuming
		// the run, so this one is spent rather than run as an unrelated
		// message.
		c.clearAnswerAttempts(run.AgentHandle, run.TurnID)
		return AnswerConsumed, nil
	}
	// FOUR VALUES, BECAUSE THE MATCH HAS TWO ENDS. The delivery's pair and
	// the row's pair together are what say WHICH row won and WHY: a row
	// whose conversation equals the delivery's was admitted on the
	// identity, a row with no conversation at all was matched on the
	// partition fallback because it predates the split, and two questions
	// parked on one direct-message line are told apart by the partitions
	// alone — [ConversationRef.Best] prefers the row whose batch the reply
	// arrived in. Logging the identity by itself left every one of those
	// indistinguishable from the others.
	log.InfoContext(ctx, "sandbox_clarification_answered",
		"turn_id", claimed.TurnID,
		"conversation", conv.Identity, "partition", conv.Partition,
		"run_conversation", claimed.ConversationKey,
		"run_partition", claimed.PartitionKey)
	// The claim CLOSED THE QUESTION and took the seat: the parked run freed
	// it, and re-entering the Execute loop is work like any other. One move,
	// so no delivery sees the seat between the two halves.
	c.moveRun(claimed.AgentHandle, StatusAwaiting, StatusResumed)
	// NO OUTCOME: this resume collects no run. The box is still parked and
	// its cost is charged where it is collected, so reporting one here would
	// bill the same run twice.
	disposition, err := c.resumeAndSettle(ctx, claimed, answerText(claimed, answer), true, trigger, runOutcome{})
	if disposition == AnswerDeferred {
		// The claim went back and the run is awaiting this same answer
		// again — so the delivery has to come back, up to the budget
		// [MaxAnswerAttempts] gives it.
		return c.deferAnswer(ctx, claimed, trigger, err)
	}
	// SPENT OR HANDED ON, either way not a failure in a series, so the next
	// one starts its own.
	c.clearAnswerAttempts(claimed.AgentHandle, claimed.TurnID)
	return disposition, err
}

// resumeAndSettle re-enters the suspended loop, then settles the box.
//
// After the resumed Execute returns: if the executor called run_sandbox AGAIN
// the row is back in running and a new job owns the paused box, so it is left
// for the next completion. Otherwise the phase is done with the box, so the box
// is torn down and the run finished.
//
// IT REPORTS WHAT IT LEFT THE DELIVERY, not merely whether it failed, because
// "the resume did not happen" is three different facts and only one of them is
// a retry: the run can be back where the claim found it (the delivery is still
// owed to it), or terminally gone (nothing is owed it any more), or the turn
// can have run and concluded something (the delivery is spent). Its two
// callers arrive by different routes and read the pair differently — a
// completion NAKs on the error and lets the broker's own budget bound the
// retry, while a person's answer is handed back on [AnswerDeferred] — the same
// NAK, under this package's own shorter bound — and falls through to an
// ordinary turn on [AnswerNotMine]. The classification is stated
// once, at [Coordinator.TryResumeFromAnswer].
func (c *Coordinator) resumeAndSettle(ctx context.Context, run PendingRun,
	answer string, success bool, trigger *events.Event, outcome runOutcome,
) (AnswerDisposition, error) {
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
		// TERMINALLY GONE, so nothing is owed the delivery that got here:
		// requeueing it would circle a run this settle has just deleted,
		// and acking it would swallow a person's message on behalf of a
		// turn that no longer exists.
		return AnswerNotMine, nil
	}
	// Freed only NOW, immediately before the resume, so no queued event can
	// take the slot first. The claim holds the row in [StatusResumed]
	// whichever set it was claimed from, so that is the count to give back.
	c.uncountRun(run.AgentHandle, StatusResumed)

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
			// AND SETTLED EVEN WHEN THE RECORD CANNOT BE READ. A read
			// failure here used to settle nothing, which left the row in
			// the claim this branch exists to keep — the one state
			// nothing recovers from. See [Coordinator.settleClaimed].
			settle, readErr := c.current(ctx, run)
			if readErr != nil {
				c.settleClaimed(ctx, run, readErr)
			} else {
				c.finish(ctx, settle, fenceOf(run))
			}
			c.syncSeat(ctx, run.AgentHandle)
			// THE TURN RAN AND WROTE OUTSIDE THE ENGINE, so whatever drove
			// it is SPENT: this is the one branch that deliberately keeps
			// the claim, and handing the delivery back to the ordinary
			// route would run a second turn on a message the resumed one
			// has already acted on.
			return AnswerConsumed, nil
		}
		// UN-CLAIM so a retry can win the flip again. Without this the NAK'd
		// completion redelivers, the claim refuses, and the suspended
		// conversation is permanently lost with the row stranded in resumed.
		log.ErrorContext(ctx, "sandbox_resume_failed",
			"turn_id", run.TurnID, "revert_to", claimedFrom(run), "error", err.Error())
		if unclaimErr := c.unclaim(ctx, run, false, resumeUnrevertedDetail); unclaimErr != nil {
			//nolint:nilerr // Deliberate, and the same answer the
			// acted-and-broke branch above gives for the same reason:
			// the run has been settled, so the completion is not sent
			// back for a retry that would find nothing to claim.
			//
			// And nothing is owed the delivery either, for that same
			// reason: the run it would come back to has been ended and
			// announced.
			return AnswerNotMine, nil
		}
		// THE RUN IS BACK WHERE THE CLAIM FOUND IT, which on the answer
		// route is awaiting this very reply. The delivery is still owed to
		// it, so it has to come back rather than be worked as an ordinary
		// message.
		return AnswerDeferred, err
	}

	latest, err := c.current(ctx, run)
	if err != nil {
		// THE SAME ANSWER THE ACTED BRANCH GIVES, and for the same
		// reason: the resume is over, so a row left in the claim is
		// picked up by nothing. The store decides whether this run is
		// still the one that was claimed — see
		// [Coordinator.settleClaimed].
		c.settleClaimed(ctx, run, err)
		c.syncSeat(ctx, run.AgentHandle)
		// THE TURN RAN: the resume returned, and only the settle that
		// follows it could not be decided. Whatever drove it is spent.
		return AnswerConsumed, nil
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
			"turn_id", run.TurnID, "status", latest.Status)
		return AnswerConsumed, nil
	}
	c.finish(ctx, latest, fenceOf(run))
	// RECOUNTED, not assumed free. The count was cleared before the resume,
	// but a start event redelivered while the turn ran recomputes it from
	// the store, where this run, claimed, still held the seat; with no
	// recount here that seat stayed parked on a run that no longer exists
	// until the seat changed hands. The store now has no record of this run,
	// so the recount keeps only the seat's other live runs.
	c.syncSeat(ctx, run.AgentHandle)
	// The ordinary ending: the turn came back and the run is finished, so
	// whatever drove the resume did its whole job.
	return AnswerConsumed, nil
}

// current is a claimed run as its record stands after the resumed turn
// returned.
//
// The LATEST record, because the box to settle is the one it names now: a
// re-seeded run provisioned a fresh box, so the claimed snapshot's id is stale,
// and a resumed executor that called run_sandbox again has a new job in it. A
// record that is already gone is answered with the snapshot, whose settle then
// reclaims a box that may still be there and finds nothing to delete.
//
// THREE ANSWERS RATHER THAN TWO. "Here is the record", "the record is gone"
// and "the store could not be read" are three different facts, and the last is
// the one a caller must not read as either of the others. It was a bool, and
// both callers read the false as "settle nothing" — which leaves the row in
// [StatusResumed], the one state nothing recovers from (see
// [Coordinator.revertClaim]). What a caller does with the error instead is
// [Coordinator.settleClaimed].
func (c *Coordinator) current(ctx context.Context, run PendingRun) (PendingRun, error) {
	latest, found, err := c.pending.Get(ctx, run.TurnID)
	if err != nil {
		return PendingRun{}, fmt.Errorf("sandbox: reading run %s to settle it: %w", run.TurnID, err)
	}
	if !found {
		return run, nil
	}
	return latest, nil
}

// claimedOnly is the license a settle takes when it could not read the record:
// the run is ended only while it is still the one the claim flipped.
var claimedOnly = []string{StatusResumed}

// settleClaimed ends a run whose record could not be read, WHILE IT IS STILL
// CLAIMED.
//
// SILENCE IS NOT AN ANSWER HERE. Both callers reach it having just taken a
// suspended turn out of the engine's hands, and the row they would leave
// behind is the claim itself: the completion poll does not read a
// [StatusResumed] row, a redelivery cannot re-claim one, no answer matches one
// and no reaper expires one. So "leave it for the next pass" is a turn
// destroyed in silence beside a paused box billed until the seat happens to
// change hands — exactly what [Coordinator.revertClaim] settles a run rather
// than accept.
//
// THE CONDITION MOVES TO THE STORE, which is what makes acting without the
// read safe. The only hazard the read ever guarded was killing a job the
// resumed turn RELAUNCHED — a relaunch reuses this very box — and a relaunch
// takes the row back through [StatusLaunching], so a delete licensed for
// [StatusResumed] alone declines it. What that leaves is a launching row,
// which is the shape [Coordinator.FailRun] and the seat's next recovery pass
// already end, rather than a box killed under a live job, which nothing undoes.
//
// DELETED FIRST AND RECLAIMED SECOND, the one inversion of
// [Coordinator.finish]'s order, and for the reason that order exists: here the
// delete IS the decision, so reclaiming first would destroy the box before
// anything had established the row was still this call's to end. The window it
// opens is a crash between the two, which leaves a box named by nothing until
// its provider's TTL or the local orphan reaper takes it; the window the other
// order opens is a live checkout killed out from under a turn.
func (c *Coordinator) settleClaimed(ctx context.Context, run PendingRun, cause error) {
	// DETACHED, like every other teardown: this is reached from a failure
	// path and from a queue handler a drain cancels, so the context that
	// got here is very often already dead — and a settle that no-ops is the
	// stranded row all over again.
	settleCtx, cancel := detached(ctx)
	defer cancel()
	settled, ended, err := c.endRecord(settleCtx, run, fenceOf(run), claimedOnly)
	if err != nil {
		log.ErrorContext(ctx, "sandbox_claimed_settle_failed",
			"turn_id", run.TurnID, "error", err.Error(), "cause", cause.Error(),
			"detail", "the run's record could neither be read nor deleted, so it is left in "+
				"the claim with its box paused; the seat's next recovery pass reaps it")
		return
	}
	if !ended {
		// Not ours: the run either moved on under us — a relaunch the
		// turn made before it broke — or somebody else ended it. Either
		// way that party owns the box and the record.
		log.InfoContext(ctx, "sandbox_claimed_settle_declined",
			"turn_id", run.TurnID, "cause", cause.Error(),
			"detail", "the run is no longer the claim this settle took, so it is left to "+
				"whoever moved it")
		return
	}
	log.WarnContext(ctx, "sandbox_claimed_settled_unread",
		"turn_id", run.TurnID, "sandbox_id", settled.SandboxID, "error", cause.Error(),
		"detail", "the run's record could not be read after the resume, so it was ended on "+
			"the status the store still held rather than left in the claim")
	// The stop went with the deletion, inside [Coordinator.endRecord]; what
	// is left is the box that record named.
	_ = c.reclaimBox(ctx, settleCtx, settled)
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
// THE RUN IS COUNTED BACK INTO THE SET ITS REVERTED STATUS BELONGS TO, which
// is not always the seat. counted is whether this node's count still includes
// this run, which it does until the resume gives the seat back, and only a
// park, whose claim is always out of running, hands one back before that. A
// run back in running holds the seat again; one back waiting on a person does
// not — it is an OPEN QUESTION instead. Re-marking the seat held for it
// parked every later delivery on a run no poll completes, the person's own
// answer included; counting it into neither set is the opposite mistake, and
// leaves that answer to be run as an unrelated turn (see [seatRuns]).
//
// A CONTEXT OF ITS OWN, like [Coordinator.teardown], because this is a
// rollback and the failure it undoes is often the cancellation itself: a drain
// cancels the delivery's context, the resume breaks on it, and a release that
// inherited it wrote nothing. The run then stayed resumed, and the seat's next
// owner, the node the drain was handing it to, reaped it as abandoned instead
// of resuming it.
//
// # A HAND-BACK THAT CANNOT BE WRITTEN IS NOT A RETRY, so it ENDS the run
//
// Giving the claim back is the whole of what makes every failure after it a
// retry, and a row left in [StatusResumed] is picked up by nothing: the
// completion poll reads only running rows, a redelivered completion is refused
// by the very claim it would retake, no answer matches a row that is not
// awaiting, and the pause reaper expires only one that is. So what looks like
// "leave it for next time" is a turn destroyed in silence, its box paused and
// billed until the seat happens to change hands. It is settled instead — box
// reclaimed, record deleted, loss announced, stop reported — which is what
// every other destroyed turn gets, and detail is the sentence that reaches an
// operator's board.
//
// # NO STOP IS REPORTED ON THE SUCCESSFUL PATH, on either route
//
// Which is a decision rather than an omission — see
// [CoordinatorOptions.Stopped] for what a stop takes down. A completion's
// hand-back puts the run back to [StatusRunning], where the box IS still
// working and the indicator the suspension kept alive must stay up. An
// answer's puts it back to [StatusAwaiting], where nothing is working — but
// the indicator over that wait was already taken down twice over: once at the
// park, which reports its own stop, and again by the resume frame itself,
// which raises a fresh indicator off the answer's trigger and clears it on
// exactly this failure. A stop reported from here would have to know which of
// the two it was in, which is the same distinction the reverted status already
// makes — and getting it wrong on the completion route takes down an indicator
// over work that is still running. The SETTLE is the exception, and it makes
// its own report through the ending it performs.
//
// Reports whether the run is back in the hands of whatever will retry it: nil
// where the claim went back (or had already moved on), and the release's own
// error where the run was ended in its place. The caller's own error is not
// this answer — a retry keeps it, so the delivery comes back, and an ending
// drops it, because there is nothing left to come back to.
func (c *Coordinator) unclaim(ctx context.Context, run PendingRun, counted bool, detail string) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), discardGrace)
	defer cancel()
	to := claimedFrom(run)
	released, err := c.pending.ReleaseClaim(ctx, run.TurnID, Release{
		Launch: run.LaunchID, To: to, Charged: run.Charged, Fence: fenceOf(run),
	})
	switch {
	case err != nil:
		log.ErrorContext(ctx, "sandbox_claim_revert_failed",
			"turn_id", run.TurnID, "revert_to", to, "error", err.Error(),
			"detail", "the claim could not be handed back, so nothing would ever pick "+
				"this run up; it is ended and announced instead")
		c.settleFailed(ctx, run, types.SandboxFailureClaimStranded, detail)
		// AUTHORITATIVE, not assumed: the settle moved the row and this
		// call moved the counts, and neither knows what the other found.
		c.syncSeat(ctx, run.AgentHandle)
		return fmt.Errorf("sandbox: handing back the claim on %s (to %s): %w", run.TurnID, to, err)
	case !released:
		log.WarnContext(ctx, "sandbox_claim_moved_on",
			"turn_id", run.TurnID, "launch_id", run.LaunchID,
			"detail", "the run no longer holds this claim, so nothing was handed back: "+
				"the resumed turn launched another job, or the seat's next owner reaped it")
		c.syncSeat(ctx, run.AgentHandle)
	case !counted:
		c.countRun(run.AgentHandle, to)
	}
	return nil
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
// and a second announcement would name a reason the run did not end for. The
// engine is told on the same gate, by the deletion inside
// [Coordinator.endRecord] — see [CoordinatorOptions.Stopped].
func (c *Coordinator) settleFailed(ctx context.Context, run PendingRun, reason, detail string) {
	ended := c.finish(ctx, run, fenceOf(run))
	// Out of whichever set the record was in: this path settles a claimed
	// run ([StatusResumed]) and a launch that never suspended
	// ([StatusLaunching]) alike, and a status names its own set.
	c.uncountRun(run.AgentHandle, run.Status)
	if !ended {
		return
	}
	c.announceFailure(ctx, run, reason, detail)
}

// The three sentences a stranded claim reaches an operator's board with.
//
// One per caller rather than one shared line, because what an operator does
// about them differs: a question that was never announced is one nobody saw, a
// question announced but not recorded is one somebody may be composing an
// answer to that will never be matched, and a resume that could not be given
// back is work a box had already finished. All three name the coordination
// store, because a claim that could not be handed back is what brought every
// one of them here.
const (
	parkUnannouncedDetail = "the coding run stopped to ask a person a question, but neither the " +
		"question could be announced nor the run's own claim given back to the " +
		"coordination store, so nobody was asked and the turn cannot be continued; the " +
		"work it pushed, if any, is on its branch"

	parkUnrecordedDetail = "the coding run stopped to ask a person a question, but neither the " +
		"question nor the run's own claim could be written to the coordination store, so " +
		"the turn cannot be continued and nobody can answer it; the work it pushed, if " +
		"any, is on its branch"

	resumeUnrevertedDetail = "the coding job finished but the turn it belongs to could not be " +
		"re-entered, and the run's claim could not be given back to the coordination " +
		"store for another attempt, so the turn cannot be continued; the work it pushed, " +
		"if any, is on its branch"
)

// reportStopped tells the engine one suspended turn has stopped.
//
// One helper rather than a nil check at each site, so "a stop is reported" is
// one statement rather than several that have to keep agreeing. A caller that
// wired nothing gets a no-op, which is a valid build: nothing above this
// package need hold anything up while a turn works.
//
// Its callers are [Coordinator.park] and [Coordinator.endRecord], and nothing
// else may become one: an ending reports by deleting the record, which is what
// stops the next ending from having to remember. See
// [CoordinatorOptions.Stopped].
func (c *Coordinator) reportStopped(ctx context.Context, run PendingRun) {
	if c.stopped == nil {
		return
	}
	c.stopped(ctx, run.AgentHandle, run.TurnID)
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

// endRecord deletes a run's record — while its status is one this ending is
// licensed for — and REPORTS THE STOP where the deletion was this call's.
//
// THE ONE PLACE A RUN'S RECORD IS DELETED, which is what makes it the one place
// that can say a suspended turn is over, and why the report is made here rather
// than by each ending. Three rounds of hand-enumerating those endings each
// missed one, and the count in [CoordinatorOptions.Stopped] was wrong again the
// moment a fourth arrived; a report keyed to the deletion cannot be missed by a
// path that deletes, and needs no count. It fires on exactly the gate a
// caller's own announcement takes, because it IS that gate — the ending being
// this call's — so a losing racer neither announces a reason the run did not
// end for nor drops a hold belonging to whoever did end it. And before any
// caller's announcement, which is a publish that can block: what comes down is
// a claim on somebody's screen.
//
// It reports for a turn that came back too — a collected run whose resumed
// executor finished, which every ordinary completion is. That is deliberate:
// the hold is ended by the resume's own frame before this call is reached, so
// the report finds nothing to drop, and a report gated on "was this turn
// suspended when we got here" would be a second opinion about a question the
// store has already answered. Over-reporting costs a map lookup;
// under-reporting is the defect. See [CoordinatorOptions.Stopped].
//
// A DELETE THAT COULD NOT BE WRITTEN REPORTS TOO, for the same asymmetry: the
// write may have landed, and no caller can tell. None has to, because of the
// PROPERTY EVERY CALLER SATISFIES rather than anything particular each one does
// next: IT HAS ALREADY FINISHED WITH THE RUN BEFORE IT ASKS. Nothing a caller
// leaves behind resumes the turn whether the delete landed or not — a record
// that survives one is an unresumable tail, ended by some later pass whose own
// report finds nothing left to drop. So the stop is true at the moment it is
// reported, on a branch that cannot establish what the record says. The
// property is what is written down here because the alternative was not: this
// paragraph reasoned from what each caller does next, and was a caller short
// the moment a third one reached it.
//
// Reports the record it deleted, whether the ending was this call's, and the
// delete's own error — which is what a caller that can RETRY needs and a
// settle has no use for.
func (c *Coordinator) endRecord(ctx context.Context, run PendingRun, fence Fence, whileIn []string,
) (PendingRun, bool, error) {
	settled, ended, err := c.pending.Finish(ctx, run.TurnID, fence, whileIn)
	if err != nil {
		c.reportStopped(ctx, run)
		// FORGOTTEN ON THE SAME GATE THE STOP TAKES, and for the same
		// asymmetry: the delete may well have landed, and a count kept
		// for a run nothing will ever offer a delivery to again is a
		// map entry this process never drops. See [answerBudget].
		c.clearAnswerAttempts(run.AgentHandle, run.TurnID)
		return PendingRun{}, false, err
	}
	if !ended {
		return PendingRun{}, false, nil
	}
	c.reportStopped(ctx, settled)
	c.clearAnswerAttempts(settled.AgentHandle, settled.TurnID)
	return settled, true, nil
}

// finish ends a run: its box is reclaimed, its record deleted, and — where the
// ending was this call's — the stop REPORTED.
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
	// LICENSED FOR EVERY LIVE STATUS, because the box is already reclaimed:
	// whatever the row says now, this run is over and its record must not
	// outlive it. The narrow license belongs to the one settle that has not
	// touched the box yet — see [Coordinator.settleClaimed].
	_, ended, err := c.endRecord(killCtx, run, fence, Active)
	if err != nil {
		log.WarnContext(ctx, "sandbox_finish_failed", "turn_id", run.TurnID, "error", err.Error(),
			"detail", "the run's box is reclaimed but its record was not deleted; the seat's "+
				"next recovery pass reaps it")
		// The ending is this call's whatever the record says, for the
		// reason above: the box is gone and the turn is over.
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
// Running jobs re-mark this seat held; the waiter then drives them to
// completion. Clarification and reseed runs are left for their answer — the
// run untouched, but COUNTED, because the seat's new owner is the one that
// will be handed that answer and has to recognise it as one.
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
	recovered, parked, abandoned := 0, 0, 0
	for _, run := range active {
		switch run.Status {
		case StatusLaunching, StatusResumed:
			log.WarnContext(ctx, "sandbox_abandoned_tail_reaped",
				"turn_id", run.TurnID, "agent", run.AgentHandle,
				"sandbox_id", run.SandboxID, "status", run.Status)
			// Fenced on the lease this node just took, so a record a
			// newer owner has already claimed is left to that owner, and
			// neither ended nor announced here.
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
			c.countRun(run.AgentHandle, run.Status)
			recovered++
		case StatusAwaiting, StatusReseed:
			// NOTHING IS DONE TO THE RUN — its answer is what moves it —
			// but the new owner has to know the question is open, or the
			// answer arrives at a seat this node believes has nothing
			// waiting and is run as an unrelated turn. The old owner's
			// count went with the old owner; this is where the new one
			// gets it.
			c.countRun(run.AgentHandle, run.Status)
			parked++
		}
	}
	log.InfoContext(ctx, "sandbox_seat_recovered",
		"seat", handle, "epoch", epoch, "running", recovered,
		"parked", parked, "abandoned", abandoned, "active", len(active))
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
		// THE SAME ENDING EVERY OTHER PATH MAKES, which is what keeps
		// the stop report out of this function: a retirement does not
		// use [Coordinator.finish] — it keeps the record of a box it
		// could not reclaim, and it runs under the caller's context
		// rather than a detached one — but the delete and the report
		// are one helper both reach, so the seat that is gone from the
		// company tells whoever is holding something up for its turn
		// without this path having to remember to.
		_, ended, err := c.endRecord(ctx, run, fence, Active)
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
	// The seat's answer-attempt counts go with it, for the reason they are
	// per node at all: the successor gets a clean set of attempts at every
	// run it inherits, and this node keeps no count for a seat it will
	// never be offered a delivery for again. See [MaxAnswerAttempts].
	c.releaseAnswerAttempts(handle)
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.runs, handle)
	log.Debug("sandbox_seat_released", "seat", handle)
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
