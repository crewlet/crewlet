package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

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

// ErrHandBackOwed is a store's refusal to delete a run, or to record more
// copies on it, while it still owes the seat's inbox the copies it carries
// ([PendingRun.HandBack]). The answer is to publish and clear them first
// ([Coordinator.deliverHandBack]) and then ask again.
var ErrHandBackOwed = errors.New("sandbox: the run still owes the seat a reply it has not handed back")

// ErrAnswerOwed is a store's refusal to delete a run whose row still holds a
// person's reply no turn took ([PendingRun.Answer]). The reply's delivery was
// spent when it was recorded, so the row is the only thing still carrying it.
// The answer is to let it go back to the seat first ([PendingStore.OweHandBack],
// [Coordinator.letGoAnswer]) and then ask again.
var ErrAnswerOwed = errors.New("sandbox: the run still holds a person's reply that no turn took")

// ErrAnswerTaken is a store's refusal to let go of a recorded answer a resumed
// turn already took ([PendingStore.OweHandBack]): the reply has been used, and
// a copy handed back now would answer the person twice.
var ErrAnswerTaken = errors.New("sandbox: a resumed turn already took the reply")

// ErrRunEnding is a store's refusal of a write to a run whose ending is decided
// ([PendingRun.Ending]): from the decision on, the row takes nothing but the
// ending's own steps — no claim, no take, no release, no relaunch, no new
// answer, no park — which is what lets the ending be announced before its record
// is deleted (see [RecordedEnding]). It is what keeps the take and the let-go
// exclusive: a take that lands first is seen by the decision, and one that comes
// after it is refused.
var ErrRunEnding = errors.New("sandbox: the run's ending is decided, so it takes no other write")

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

	// DeliveredRefs are what the run reported producing, for the resumed
	// phase's own event — the executor's delivery is judged on them. Empty
	// when a PERSON's answer resumes a parked clarification: no new run
	// finished.
	//
	// The run's COST is deliberately not here. It is the run's own fact and
	// rides the run's own record ([Coordinator.OnCompleted] publishes it as
	// the `sandbox` phase), which every collected run gets — a run that
	// parked on a question included, whose resume is an answer and carries
	// no run at all, so on this request its cost was reported nowhere.
	DeliveredRefs []string

	// InputTokens and OutputTokens are what the job this resume collected
	// cost, for the resumed segment's charge to the turn's work item
	// (ADR-0022): that segment is the job's, so it pays for it.
	//
	// UNLIKE the refs above, a person's answer carries them too — the
	// tokens of the job that asked, recorded on the row when it parked
	// ([PendingRun.ParkedInputTokens]). The answer's resume is the ONLY
	// segment that job ever gets, so it is the one that pays; the
	// completion that parked resumed nothing.
	InputTokens  int
	OutputTokens int

	// Begin is what the resumer calls ONCE, at the moment the resumed
	// segment is certain to run and before anything of it does — after
	// every check that can still send the resume back as a retry. An error
	// means the segment must not run: the resumer returns it, and it takes
	// the path every failed resume takes. Nil commits nothing.
	//
	// IT IS WHERE A RESUME BECOMES A TURN, and the coordinator records it
	// there because nothing else can tell the two apart afterwards. For a
	// recorded answer it writes on the run that the turn took the answer
	// ([PendingStore.TakeAnswer]): a claim whose process stops before this
	// still owes the person's reply to the seat, and one that stops after
	// it has used the reply — and the seat's next holder, reaping the claim,
	// can read only the row. And it records the reply's delivery as worked
	// ([CoordinatorOptions.Spent]), on every route, because from this
	// moment a copy of it reaching the seat is a reply a turn already has.
	// In this process it is also what tells a resume that broke before its
	// turn from one that broke while running it ([ErrResumeAbandoned]): the
	// first destroyed nothing a person sent.
	Begin func(ctx context.Context) error
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

// AudienceResolver resolves the audience a coding agent named for its question
// — "requester", "manager", "team", or a name it typed — to the seats it means,
// for the run that asked.
//
// DECLARED HERE, by the one caller, and implemented by the engine, which holds
// the chart: this package knows the label and the run, and nothing about who
// leads whom. A PURE ANSWER with no error, because it is a walk over a chart
// already in memory — and because what a label that resolves to nobody means
// is part of the answer ([Audience.Fallback]), never a failure of the park.
type AudienceResolver interface {
	ResolveAudience(run PendingRun, label string) Audience
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

	// Audience resolves who a parked question may be answered by, at the
	// park. Required, for the reason Resume is: only the engine holds the
	// chart the label is resolved against, and a coordinator that parked
	// questions without it would record every one of them as waiting on
	// nobody — which is the defect [PendingRun.AudienceHandles] exists to
	// end.
	Audience AudienceResolver

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
	// is decided on the run's record, and every decision goes through
	// [Coordinator.endRecord], which reports; the one stop that is not an
	// ending, [Coordinator.park], reports for itself. So there are two
	// reporting sites for two kinds of transition, however many callers
	// reach them — a settle, a reap, a retirement, a lost claim, and
	// whatever ends a run next. This doc said "exactly two places" and then
	// named the two CALLERS it was thinking of, which was already three by
	// the time it was written: a retirement ends runs too, and it reported
	// separately because it cannot use [Coordinator.finish]. Counting
	// callers is the same hand-enumeration that missed four endings in a
	// row; keying the report to the decision is what ends it.
	//
	// Both report only where the transition was THIS call's — the same gate
	// the failure announcement takes, since a run a newer lease owns or one
	// somebody else ended first is that party's to settle, to explain and to
	// drop the holds of. Over-reporting is harmless and under-reporting is
	// the defect, so a path that cannot tell reports: a decision that
	// errored may have landed, and it reports rather than staying quiet.
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

	// Hold holds a seat's inbox while one of its runs holds the seat or an
	// answer it owes is waiting on its resume — see [SeatHold]. Nil holds
	// nothing, which a coordinator built without an inbox (a test's) is
	// entitled to: the runs are still driven and the answers still resumed,
	// and only what the seat's mail does meanwhile is left to the screening.
	Hold SeatHold

	// Admit says whether a seat may run a retried resume now — see
	// [Admission]. Nil admits every retry.
	Admit Admission

	// Lease is the seat lease this node holds a seat under, which a launch
	// stamps on the run's row ([PendingStore.BeginLaunch]) and every later
	// write this node makes on the row carries back off it. It is what lets
	// the seat's next holder FENCE this node out: the holder recovers the
	// row under its own, newer lease, and a write under the lease it
	// outranks is refused rather than landing — a release that revives a
	// claim the holder has reaped, a resumed turn taking an answer the holder
	// is handing back.
	//
	// ASKED BY THE COORDINATOR AT EVERY LAUNCH rather than carried on each
	// launch request, because every launch path must stamp it and one that
	// forgot would launch an unfenced row nothing in its own frame notices.
	// The zero fence is an unfenced launch: what a node with no seat lease
	// — and so no next holder to be fenced out by — answers. Nil answers it
	// for every seat.
	Lease func(handle string) Fence

	// Spent records a recorded answer's deliveries as WORKED, in the fleet's
	// completion ledger, so a copy of the original delivery that reaches the
	// seat afterwards is dropped rather than run as an ordinary message.
	//
	// CALLED THE MOMENT THE ANSWER LEAVES THE RUN, whichever way it leaves:
	// when a turn TAKES it ([ResumeRequest.Begin]), on the inline attempt
	// and on a retry alike, and before an ending or a decline lets it go
	// back to the seat — the seat's next holder's reap included, which also
	// spends an answer it finds taken, for a take whose own spend did not
	// land. The delivery that carried it can still come round in each of
	// those: a node that recorded the answer — or took it inline, holding
	// the delivery unacknowledged through the whole resumed turn — and
	// stopped before acknowledging it leaves it to be redelivered, and by
	// then the run may have used it, or handed a copy back, or be gone, so
	// nothing on the run would recognise it as the answer it was. Spent at
	// the take rather than once the turn returns, because the node can stop
	// in the middle of the turn, after its reply reached the person.
	//
	// A REPLY SPENT AND NOT YET GONE IS NOT LOST: its row still holds it,
	// and the row is what resumes it or hands it back — never the delivery.
	// A take whose claim is given back leaves the answer on the row; a
	// copy handed back goes under an id of its own ([declinedCopyID]).
	//
	// FAILS OPEN, like every completion write: the answer goes where it was
	// going either way, and the cost of a write that did not land is a copy
	// that may run as a turn of its own — a duplicate, which is recoverable,
	// where a refusal to move the answer would lose it. Nil records nothing.
	Spent func(ctx context.Context, handle string, evs []*events.Event)

	// After schedules f to run after d, in its own goroutine, and returns a
	// function that cancels it and reports whether it had not started —
	// time.AfterFunc's contract. Nil is time.AfterFunc; a test replaces it
	// to fire a retry when it chooses. It must never call f before it
	// returns.
	After func(d time.Duration, f func()) (stop func() bool)

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
//     reuse, tokens post-accounted and the run published as a phase of its
//     own once per launch however often the tail is retried, and the
//     suspended loop re-entered with the result spliced in.
//     The seat stays held through all of it and is freed only at the last
//     moment before the resume, because freeing it earlier lets a queued
//     event take the slot, the resume fail, and the redelivery find the claim
//     already flipped, with the suspended conversation permanently lost.
//   - PARK → ANSWER. A run that stops to ask a person something gives the
//     seat BACK — the answer arrives on that seat's own inbox, and a person
//     can take days — and leaves a question open on it instead, stamped with
//     the instant it was asked. The seat then works as usual, with one
//     difference: every delivery is offered to
//     [Coordinator.TryResumeFromAnswer] before anything else consumes it,
//     because the reply that resumes an hours-old coding run is an ordinary
//     chat message and nothing about it says so. The wait is DURABLE FIRST
//     and everything else follows it: a park whose write does not land gives
//     the claim it holds back instead, and ends the run where even that
//     cannot be written — because a row left in the claim is picked up by
//     nothing at all ([Coordinator.unclaim]). And so is the ANSWER: the
//     first reply posted after the question is RECORDED on the run before
//     the resume is attempted, so a resume that fails is this coordinator's
//     to retry and never a reason to hand the person's message back.
//   - RESTART RECOVERY. A node claiming a seat re-marks its running jobs held,
//     inherits its open questions, and reaps any tail the previous owner
//     abandoned mid-resume — handing a person's answer the tail was claimed
//     for back to the seat, where no turn had taken it yet.
type Coordinator struct {
	queue   Publisher
	pending PendingStore
	manager *Manager
	resume  Resumer
	account Accountant
	// audience is [CoordinatorOptions.Audience].
	audience AudienceResolver
	ended    func(runID string)
	stopped  func(ctx context.Context, handle, turnID string)
	now      func() time.Time

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

	// moves counts the transitions [Coordinator.adjust] has applied to each
	// seat, so a recount from the store can tell that one landed while it
	// was reading — see [Coordinator.syncSeat].
	moves map[string]uint64

	// attempts counts the failed handoffs of one delivery to one parked
	// run, so a resume that fails the same way every time stops circling
	// the seat's inbox. One BUDGET PER DELIVERY, under a key naming the
	// run it is owed to, and both bounds are stated at [MaxAnswerAttempts]
	// and [maxAnswerDeliveries] — which is also where it says why this is
	// per process, and why a count that resets with the process cannot be
	// the thing keeping a reply clear of the broker's dead-letter budget.
	// That clause is [AnswerDeliveryReserve], and it is measured on the
	// delivery rather than remembered here.
	attempts map[answerKey]map[string]answerBudget

	// retries are the owed answers this node is retrying the resume (or
	// the hand-back) of, by turn id; owed is which of them each seat owes,
	// which with the seat's holding count is what its inbox hold is taken
	// for; held whether that hold is applied, and holdBusy, holdDirty and
	// holdGen the serialisation [Coordinator.reconcileHold] runs under. All
	// under mu.
	retries   map[string]*answerRetry
	owed      map[string]map[string]struct{}
	held      map[string]bool
	holdBusy  map[string]bool
	holdDirty map[string]bool
	holdGen   map[string]uint64

	// signals counts every [Coordinator.Readmit], by condition and seat,
	// so a retry refused while one landed does not wait through it. Under
	// mu; see [Coordinator.waitOwed].
	signals map[signal]uint64

	hold  SeatHold
	admit Admission
	after func(time.Duration, func()) func() bool
	// spent is [CoordinatorOptions.Spent]; see [Coordinator.spend].
	spent func(ctx context.Context, handle string, evs []*events.Event)
	// lease is [CoordinatorOptions.Lease]; see [Coordinator.leaseOf].
	lease func(handle string) Fence

	// life is the coordinator's own lifetime, which a scheduled retry runs
	// under — it outlives every delivery — and ends with [Coordinator.Stop],
	// which waits for inflight: every scheduled attempt not yet cancelled.
	life     context.Context
	end      context.CancelFunc
	inflight sync.WaitGroup
	closed   bool
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
	// driving, during which the seat starts no new turn and its inbox is
	// held (see seathold.go).
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
		{"Audience", opts.Audience == nil},
	} {
		if field.absent {
			missing = append(missing, "CoordinatorOptions."+field.name)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("sandbox: a coordinator needs %s: a detached run is "+
			"published, recorded, reconnected to, resumed and put to its "+
			"answerers through them",
			strings.Join(missing, ", "))
	}
	c := &Coordinator{
		queue: opts.Queue, pending: opts.Pending, manager: opts.Manager,
		resume: opts.Resume, account: opts.Account, audience: opts.Audience,
		ended:     opts.Ended,
		stopped:   opts.Stopped,
		now:       opts.Now,
		runs:      map[string]seatRuns{},
		moves:     map[string]uint64{},
		attempts:  map[answerKey]map[string]answerBudget{},
		retries:   map[string]*answerRetry{},
		owed:      map[string]map[string]struct{}{},
		held:      map[string]bool{},
		holdBusy:  map[string]bool{},
		holdDirty: map[string]bool{},
		holdGen:   map[string]uint64{},
		signals:   map[signal]uint64{},
		hold:      opts.Hold,
		admit:     opts.Admit,
		after:     opts.After,
		spent:     opts.Spent,
		lease:     opts.Lease,
	}
	if c.now == nil {
		c.now = time.Now
	}
	if c.after == nil {
		c.after = func(d time.Duration, f func()) func() bool { return time.AfterFunc(d, f).Stop }
	}
	c.life, c.end = context.WithCancel(context.Background())
	return c, nil
}

// Stop ends every retry this coordinator has scheduled and waits for any that
// is running. An answer whose resume is still owed stays recorded on its run,
// and whichever node holds the seat next drives it ([Coordinator.RecoverSeat]).
func (c *Coordinator) Stop() {
	c.mu.Lock()
	c.closed = true
	for _, r := range c.retries {
		if r.stop != nil && r.stop() {
			c.inflight.Done()
		}
		r.stop = nil
	}
	c.mu.Unlock()
	c.end()
	c.inflight.Wait()
}

// leaseOf is the lease a launch on a seat is stamped with —
// [CoordinatorOptions.Lease] — and the zero, unfenced lease where none is
// wired.
func (c *Coordinator) leaseOf(handle string) Fence {
	if c.lease == nil {
		return Fence{}
	}
	return c.lease(handle)
}

// SetManager swaps the sandbox manager, for a live reload of providers.sandbox.
//
// THE RUNS ALREADY IN FLIGHT KEEP THEIR BOXES REACHABLE. A cell the new
// catalogue no longer configures is carried as a retired backend (see
// [Manager.carrying]), so a job still running there is polled, collected and
// reclaimed, and a paused one reaped, through the backend that made it — while
// every new run provisions from the new catalogue alone.
//
// A NIL MANAGER CHANGES NOTHING, and that is the removal of providers.sandbox:
// a revision with no catalogue can validate no seat that launches a run, so
// the last manager stays for the runs already in flight, which still have to
// be finished through it.
func (c *Coordinator) SetManager(m *Manager) {
	if m == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.manager = m.carrying(c.manager)
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
// any broker ack window, so its seat's inbox is HELD — nothing is fetched —
// rather than its mail consumed and held (see seathold.go).
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
//
// AND THE SEAT'S INBOX HOLD FOLLOWS IT, outside the lock: a run entering
// [Holding] holds the seat's inbox and the last one leaving lifts it (see
// seathold.go), at the transition rather than at the next delivery.
func (c *Coordinator) moveRun(handle, from, to string) {
	if handle == "" {
		return
	}
	fromHolding, fromAwaiting := setOf(from)
	toHolding, toAwaiting := setOf(to)
	c.adjust(handle, toHolding-fromHolding, toAwaiting-fromAwaiting)
	c.reconcileHold(c.life, handle)
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
	c.moves[handle]++
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

// OnStarted recounts the seat from the store.
//
// NOT WHAT HOLDS THE SEAT. The launch did that as it wrote the run's row
// ([Coordinator.Launch]); this event is processed on a separate subscription
// the launching turn never waits for, and holding the seat from here left a
// window in which the seat's next mail ran beside the job. What it still does
// is put the store's own answer under the counts — on whichever node holds the
// seat when it is processed, after a restart or a handoff as much as now.
//
// Idempotent on a redelivery, which is why it consults the store rather than
// blindly incrementing: at-least-once means this can arrive twice, and a
// double increment would leave the seat held forever after the run settled.
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
//
// AND NEVER OVER A TRANSITION IT DID NOT SEE. A listing taken before a launch
// wrote its row, written back after the launch counted it, would drop the run
// from the seat's count and lift the hold the launch just took — a seat freed
// beside a job that is starting. So a recount during which this node moved the
// seat ([Coordinator.adjust]) is read again, and one that cannot settle within
// [casRetries] reads leaves the counts alone, which is the same answer a
// listing that failed gets.
func (c *Coordinator) syncSeat(ctx context.Context, handle string) {
	for range casRetries {
		c.mu.Lock()
		moved := c.moves[handle]
		c.mu.Unlock()
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
		if c.moves[handle] != moved {
			c.mu.Unlock()
			continue
		}
		if counts == (seatRuns{}) {
			delete(c.runs, handle)
		} else {
			c.runs[handle] = counts
		}
		c.mu.Unlock()
		// The hold follows the recount, as it follows every transition.
		c.reconcileHold(ctx, handle)
		return
	}
	log.WarnContext(ctx, "sandbox_busy_sync_unsettled", "agent", handle,
		"detail", "the seat's runs kept moving under the recount, so the counts this node "+
			"already kept stand")
}

// OnCompleted claims the run, collects, accounts, then resumes the loop.
func (c *Coordinator) OnCompleted(ctx context.Context, ev types.SandboxRunCompleted, trigger *events.Event) error {
	run, won, err := c.pending.ClaimForResume(ctx, ev.TurnID, CompletionTail(ev.LaunchID),
		c.leaseOf(ev.AgentHandle))
	if err != nil {
		return fmt.Errorf("sandbox: claiming %s: %w", ev.TurnID, err)
	}
	if !won {
		// Not this signal's to run. Already claimed by a duplicate of it
		// (successive poll ticks both firing before the claim landed, an
		// at-least-once redelivery, the retry a failed resume opened), or
		// the row no longer holds its job running: parked on the question
		// that job asked, replaced by the next run_sandbox call, or over —
		// or a newer lease than this node's owns it.
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
	// hands the record back with it — both of them.
	run.Charged = c.charge(ctx, run, result)
	run.Launch = c.publishPhase(ctx, run, result)

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
	// person's reply rather than an engine-generated completion. That bound
	// is [AnswerDeliveryReserve], off the count the message carries — a
	// per-process attempt ceiling cannot make a claim about a budget that
	// survives the handoffs which reset it. Every
	// disposition that returns an error is the retry, and every one that
	// does not is an ending.
	_, err = c.resumeAndSettle(ctx, run, resumeText(result), result.Success, trigger, runOutcome{
		DeliveredRefs: result.DeliveredRefs,
		InputTokens:   result.InputTokens, OutputTokens: result.OutputTokens,
	})
	return err
}

// runOutcome is what a finished run reported about itself, for the resumed
// phase's own record and the resumed segment's charge. The refs are empty
// where no run finished — a person answering a parked clarification resumes
// the turn without collecting anything — while the tokens are the job's
// either way: see [ResumeRequest.InputTokens].
type runOutcome struct {
	DeliveredRefs []string
	InputTokens   int
	OutputTokens  int
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
	// WHAT THE RUN SAYS IT SPENT IS NEVER A REFUND — see [Result.usageFloored].
	var floored bool
	if result, floored = result.usageFloored(); floored {
		log.WarnContext(ctx, "sandbox_usage_negative",
			"turn_id", run.TurnID, "sandbox_id", run.SandboxID,
			"detail", "the coding run reported a negative token count or cost; it "+
				"is read as nothing spent rather than subtracted from the seat's budget")
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

// publishPhase publishes the collected run as a phase of its turn, ONCE per
// launch, and returns the job's record as it now stands.
//
// THE RUN IS A PHASE OF ITS OWN. Until this existed a detached coding run left
// no phase record at all: the executor that launched it publishes nothing when
// it suspends, and its resumed record is the EXECUTOR's — its own rounds and
// its own tokens. So every token a coding run spent was missing from the spend
// rollup and from each node's daily usage (the budget counter was charged, and
// nothing a person reads agreed with it), and the activity transcript the
// runner reconstructs — the only account of what an agent with no telemetry
// did — was collected, redacted and thrown away. It is published here, where
// the result is in hand, for every collected run: one that finished and one
// that stopped to ask a question alike, because both spent what they spent.
//
// ONCE, for the reason the charge is once, and recorded the same way: the
// publish sits inside the part of the tail that is retried, every reader of
// the record counts it as spend, and a second copy is a second charge on
// every surface that shows one. The record rides the release that hands the
// claim back ([LaunchRecord.Published]). A publish the queue refused is NOT
// recorded, so a retry, if one comes, offers it again — the one duplicate that
// can remain is a publish reported failed that had in fact landed, which is
// the direction the charge also errs in.
//
// Telemetry never fails the tail: a record that could not be published is
// logged, and the run is resumed exactly as it would have been.
func (c *Coordinator) publishPhase(ctx context.Context, run PendingRun, result Result) LaunchRecord {
	facts := run.LaunchFacts()
	if facts.Published {
		log.InfoContext(ctx, "sandbox_phase_already_published",
			"turn_id", run.TurnID, "launch_id", run.LaunchID)
		return run.Launch
	}
	ev := events.New(runPhase(run, facts, result, c.now()), events.TraceContext{
		TraceID: run.TraceID, ParentSpanID: run.SpanID,
	})
	ev.Source = run.Role
	if err := c.queue.Publish(ctx, topics.Event(ev.Type), ev); err != nil {
		log.WarnContext(ctx, "sandbox_phase_publish_failed",
			"turn_id", run.TurnID, "launch_id", run.LaunchID, "error", err.Error())
		return run.Launch
	}
	facts.ID, facts.Published = run.LaunchID, true
	return facts
}

// runPhase is a collected run's own phase record.
//
// Everything on it is the RUN's: its tokens and cache share, what its CLI said
// it cost, the report it wrote, what it delivered, its transcript and, when it
// did not finish, why. Nothing on it is the executor's — that is on the
// resumed executor's own record, which is why the two never sum to more than
// the turn spent.
//
// Its clock is the store's instant for the launch and this node's for the
// collection, so DURATION covers launch to collection: the waiter's poll
// interval is inside it, and the coding itself is the rest. A row an older
// build launched carries no start, and the record then states none rather
// than a length measured from nothing.
func runPhase(run PendingRun, facts LaunchRecord, result Result, collected time.Time) types.AgentPhaseCompleted {
	rec := types.AgentPhaseCompleted{
		Agent: run.AgentID, RoleName: run.Role,
		TurnID: run.TurnID, WorkKey: run.UnitOfWork(),
		Iteration: facts.Iteration, Phase: types.PhaseSandbox,
		Model: facts.Model,
		// Redacted again, at the publish, although the runner redacts at
		// collection: this is the boundary the record leaves by, and a
		// runner that forgot would otherwise put a box's credentials in
		// the event store.
		Response:           redact.Secrets(result.Text),
		ActivityTranscript: redact.Secrets(result.Transcript),
		InputTokens:        result.InputTokens,
		OutputTokens:       result.OutputTokens,
		TotalTokens:        result.InputTokens + result.OutputTokens,
		CacheReadTokens:    result.CacheReadTokens,
		CacheWriteTokens:   result.CacheWriteTokens,
		WorkItem:           run.WorkItem,
		LaunchID:           run.LaunchID,
		Backend:            types.BackendSandbox,
		CodingAgent:        run.CodingAgent,
		SandboxID:          run.SandboxID,
		CostUSD:            result.CostUSD,
		DeliveredRefs:      result.DeliveredRefs,
		ConversationKey:    run.Conversation(),
	}
	if !facts.StartedAt.IsZero() {
		rec.StartedAt = facts.StartedAt.UTC()
		if took := collected.Sub(facts.StartedAt); took > 0 {
			rec.DurationMS = int(took / time.Millisecond)
		}
	}
	switch {
	case result.NeedsInput:
		// STOPPED, NOT FAILED: the run is parked on a question and will
		// be resumed by its answer. The question is the note a reader
		// needs beside the record.
		rec.Notes = "stopped to ask: " + redact.Secrets(result.Question)
	case !result.Success:
		rec.Failed = true
		rec.Error = redact.Secrets(result.Error)
		if rec.Error == "" {
			rec.Error = "the coding run did not succeed and gave no reason"
		}
		rec.ErrorKind = "coding_run_failed"
	}
	return rec
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
	// THE ANCHOR, taken BEFORE the question is announced: nobody can read
	// the question before the announcement, so nobody can answer it before
	// this instant, and a reply posted earlier is not its answer. See
	// [PendingRun.AskedAt].
	asked := c.now()
	announcement := types.SandboxClarificationRequested{
		Agent: run.AgentID, AgentHandle: run.AgentHandle, RoleName: run.Role,
		// UnitOfWork, never the raw field: see [PendingRun.UnitOfWork].
		TurnID: run.TurnID, WorkKey: run.UnitOfWork(), SandboxID: run.SandboxID,
		Question: redact.Secrets(result.Question), Audience: result.AskTo,
		// THE IDENTITY, like the launch announcement: this event is
		// display, and the durable thread is what a person reading the
		// feed means by the run's conversation.
		ConversationKey: run.Conversation(),
		// The item the run recorded at launch, so the question is shown
		// against the work it is about.
		WorkItem: run.WorkItem,
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
		InputTokens: result.InputTokens, OutputTokens: result.OutputTokens,
		// WHO IT IS PUT TO, resolved now and written with the question:
		// the label is the coding agent's own words, and "what is waiting
		// on me" is a question nobody could answer while it was all the
		// row said. See [PendingRun.AudienceHandles].
		Answerers: c.audience.ResolveAudience(run, result.AskTo),
		AskedAt:   asked,
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

// TryResumeFromAnswer records a delivery as the answer to the question a
// parked run of this seat is waiting on, if it is one, and resumes the run
// with it.
//
// Reports what the CALLER must do with the delivery — see [AnswerDisposition],
// which is the whole contract and states what each answer costs when it is
// wrong. The error beside it is the explanation and never the decision: a
// caller logs it and acts on the disposition.
//
// # Which question a delivery answers
//
// The CONVERSATION it arrived on admits the runs it may answer, and its batch
// picks between them ([ConversationRef.Best]). THE QUESTION'S OWN ASKING
// decides whether it can answer at all: a delivery written before the
// question was asked is not its answer ([Reply.qualifies], against
// [PendingRun.AskedAt]), and neither is a reply this question already let go
// of. Only a run still waiting is a candidate, so the first qualifying reply
// is the answer and every later one is the ordinary message it looks like.
//
// # Recorded first, resumed second
//
// The answer is RECORDED on the run by a compare-and-set before anything is
// done with it ([PendingStore.RecordAnswer]), and from that moment the
// delivery is spent: [AnswerConsumed], whatever the resume then concludes. The
// resume's first attempt runs inline; a failure is retried by this
// coordinator on its own schedule, with the seat's inbox held so its later
// mail waits behind it. See answerowed.go for why receiving an answer and
// resuming with it were split, and for what ends an answer that cannot be
// resumed.
//
// # Every failure, classified
//
//   - No conversation keys at all, no run awaiting this one, or none the
//     delivery qualifies for: [AnswerNotMine]. Nothing is owed it.
//   - The delivery is already the recorded answer of one of the seat's runs:
//     [AnswerConsumed]. It is a copy of an answer, never a second one.
//   - The seat's runs could not be read: whichever answer THIS NODE'S OWN
//     COUNT supports — see [Coordinator.answerLookupFailed]. No awaiting run
//     on the seat: [AnswerNotMine]. An awaiting run: [AnswerDeferred].
//   - The answer could not be recorded: [AnswerDeferred]. The write MAY have
//     landed, so this is the ambiguous case, resolved towards the run — the
//     delivery comes back and, if the record did land, is recognised as the
//     answer it already is.
//
// Every outcome of the resume is [AnswerConsumed]: the answer is the run's now.
// It ran, or it failed and will be retried — or the run ended under it before
// any turn took the reply (no conversation to resume into, a claim that could
// not be given back, a resume that broke before its turn), and then the reply
// goes back to the seat THROUGH THE RUN'S ROW, as it does on every route
// ([Coordinator.letGoAnswer]): the ending lets it go before it deletes the row,
// with the seat's inbox held until the copy is out — the way a decline's copy
// reaches the seat. This attempt used to hand its own delivery on instead, as
// the ordinary message — which needed the store to delete a row still holding
// the reply on this caller's word, and an ending that could not land left the
// reply both on the row and handed on.
//
// [AnswerDeferred] is a failure of this NODE or its store, never of the
// message, and the caller hands the delivery back so it keeps its place at the
// head of the seat's inbox — a deferral, not a Nak, which would return it
// behind the conversation's newer mail and hand the question to the wrong
// reply. [MaxAnswerAttempts] bounds how often one delivery is handed back.
func (c *Coordinator) TryResumeFromAnswer(ctx context.Context, handle string, reply Reply) (AnswerDisposition, error) {
	if reply.Conv.Identity == "" && reply.Conv.Partition == "" {
		return AnswerNotMine, nil
	}
	trigger := reply.trigger()
	runs, err := c.pending.ListActiveForSeat(ctx, handle)
	if err != nil {
		// BOTH KEYS. The match turns on the identity and falls back to the
		// partition for a row parked before an identity was written, so a
		// line naming one of them cannot say which read was attempted
		// against what — and on a direct message the two are different
		// values.
		log.WarnContext(ctx, "sandbox_answer_lookup_failed",
			"agent", handle, "conversation", reply.Conv.Identity,
			"partition", reply.Conv.Partition, "error", err.Error())
		return c.answerLookupFailed(ctx, handle, trigger, err)
	}
	// THE LOOKUP ANSWERED, so whatever this seat was counting against a
	// store that would not read is spent.
	c.clearLookupAttempts(handle)
	// Bounded by the seat's runs: every lost race is a run some other reply
	// or transition took, and a delivery cannot lose one more often than
	// there are runs for it to lose.
	for range len(runs) + 1 {
		if recorded, ok := reply.recordedOn(runs); ok {
			// A COPY OF AN ANSWER ALREADY RECORDED. Spent as the answer
			// it is — and if this node is not already driving its
			// resume, it starts: the node that recorded it may have
			// stopped before it could.
			c.clearAnswerAttempts(recorded.AgentHandle, recorded.TurnID)
			if recorded.Status == StatusAnswered && !c.owes(recorded.TurnID) {
				c.resumeOwed(ctx, recorded)
			}
			return AnswerConsumed, nil
		}
		run, found := reply.Best(runs)
		if !found {
			// NOTHING WAS MATCHED, so nothing is owed the delivery: it
			// is an ordinary message and is handled as one.
			return AnswerNotMine, nil
		}
		recorded, won, err := c.recordAnswer(ctx, run, reply)
		if err != nil {
			// THE RECORD MAY HAVE LANDED. A store that could not say is
			// not a store that said no, so the delivery comes back — and
			// a copy of it that finds the record is spent above.
			return c.deferAnswer(ctx, run, trigger,
				fmt.Errorf("sandbox: recording the answer %s is waiting on: %w", run.TurnID, err))
		}
		if won {
			c.clearAnswerAttempts(recorded.AgentHandle, recorded.TurnID)
			c.resumeOwed(ctx, recorded)
			return AnswerConsumed, nil
		}
		// LOST: another reply was recorded first, or the run moved on.
		// Read again, so the decision is made against what the store holds
		// now rather than against the snapshot that lost.
		if runs, err = c.pending.ListActiveForSeat(ctx, handle); err != nil {
			return c.answerLookupFailed(ctx, handle, trigger, err)
		}
	}
	return c.answerLookupFailed(ctx, handle, trigger,
		errors.New("sandbox: the seat's runs kept changing under the answer's record"))
}

// owes reports whether this node is already driving a run's owed answer.
func (c *Coordinator) owes(turnID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.retries[turnID]; ok {
		return true
	}
	for _, owed := range c.owed {
		if _, ok := owed[turnID]; ok {
			return true
		}
	}
	return false
}

// AnswerByTurn records the answer a person gave a parked run BY NAMING IT,
// rather than by replying on the conversation it was asked in, and resumes the
// run with it.
//
// # Recorded first, like a chat reply
//
// The answer is RECORDED on the run ([PendingStore.RecordAnswer], as
// [types.AnswerViaOperator]) before anything is done with it, exactly as the
// chat route records a reply ([Coordinator.TryResumeFromAnswer]), and from
// that moment the run owns it: the first attempt at the resume runs inline,
// every retry is this coordinator's, the seat's inbox waits behind it, and
// everything that keeps a recorded reply exactly-once keeps this one —
// the take a resumed turn makes ([ResumeRequest.Begin]), the let-go an ending
// makes, and the seat's next holder reviving a claim whose node stopped before
// its turn took the answer ([Coordinator.RecoverSeat]). It was resumed with
// DIRECTLY instead, held only by the claim: a node that stopped between that
// claim and the turn left a claim its successor reaped as an abandoned tail,
// and the answer — whose delivery came back to a run that was gone — was lost.
//
// # The question it answers, and no other
//
// An answer is given against the question the person READ, and it reaches
// the seat's inbox some time later: behind a pause, behind a seat busy on
// another of its coding runs, after a NAK's backoff, or as a second copy of a
// retried `answer_run`. By then the run may have been answered by somebody
// else, resumed, called `run_sandbox` again, and parked on a NEW question —
// and an answer that claimed whatever the run is waiting on now resumed it a
// second time with the first question's answer presented as the second's.
// So the delivery carries the question it was given against
// ([types.SandboxAnswerGiven.LaunchID], stamped off the row the answer was
// accepted against), and is recorded only while the run is still waiting on
// THAT question: the launch identifies it, because every new question comes
// with a new launch ([PendingStore.BeginLaunch] mints one) and nothing but a
// launch opens a question — a completion parks only a running job, and a job
// that has parked never runs again. The record itself names the launch, so a
// launch that moves between the read and the write loses it. An answer whose
// question is gone is spent as `not_awaiting`, exactly like one that reached a
// run nobody was waiting on. An answer carrying NO question is refused before
// the run is read and spent with that reason ([errAnswerNamesNoQuestion]):
// which question it answers cannot be known, and recording it would be the
// pairing this rule exists to stop.
//
// # What the caller does with each answer
//
// The delivery is a [types.SandboxAnswerGiven] on the seat's inbox, and it is
// NEVER A TURN, so two of the three dispositions mean the same thing to the
// dispatcher — spend it — and only one hands it back:
//
//   - [AnswerConsumed] — the answer is the run's: recorded now, or already —
//     a redelivery of the very event a record holds — and its resume is the
//     coordinator's from here.
//   - [AnswerNotMine] — the run is not waiting for this answer (another
//     answer was recorded first, it is running a job, or it has moved on to a
//     question of its own), the question let this very answer go
//     (`declined`, below), or the run is gone. Spent: announced as such, and
//     there is nothing else for the delivery to become.
//   - [AnswerDeferred] — the run IS waiting and the answer could not be
//     recorded against it: the row could not be read, or the record could not
//     be confirmed. Handed back with a NAK, so the queue's own backoff spaces
//     the attempts; if the record did land, the redelivery finds it and is
//     spent as the answer it already is.
//
// # Why no attempt ceiling on the record
//
// The chat route stops offering a reply after [MaxAnswerAttempts] because the
// reply has somewhere else to go — it is an ordinary message, and being
// worked as one is better than circling a run that cannot take it. This
// delivery has nowhere else to go, so giving up early would DROP a person's
// answer in silence. The bound is the broker's own delivery budget instead,
// whose end is loud: the message lands on the dead-letter subject with the
// cause under it. Once RECORDED, the resume is bounded like a chat reply's
// ([MaxAnswerAttempts], [answerWindow]), and an answer every attempt failed to
// resume with is let go of: the run waits on its question again, and the copy
// handed back to the seat's inbox is spent here as `declined`, so the person
// is told to answer again rather than left waiting on a run that is not using
// their answer.
//
// # Who is recorded
//
// Every answer that reached a run publishes [types.SandboxRunAnswered], with
// the route `operator` and the credential and person the answer names —
// which is the whole of the audit this route needed and the chat route
// lacked. An answer recorded on its run publishes it once its resume has
// become something ([Coordinator.attemptOwed]); one that was not recorded,
// here.
func (c *Coordinator) AnswerByTurn(ctx context.Context, given types.SandboxAnswerGiven, trigger *events.Event) (AnswerDisposition, error) {
	answered := func(run PendingRun, outcome types.AnswerOutcome) {
		c.announceAnswered(ctx, run, types.AnswerViaOperator, outcome,
			given.AnsweredBy, given.AnsweredBySeat)
	}
	if given.LaunchID == "" {
		// NO QUESTION NAMED, so none this answer can be paired with: spent,
		// and said why, rather than recorded against whatever the run waits on.
		log.ErrorContext(ctx, "sandbox_answer_names_no_question",
			"turn_id", given.TurnID, "agent", given.AgentHandle,
			"answered_by", given.AnsweredBy, "answered_by_seat", given.AnsweredBySeat,
			"detail", "an answer by turn arrived naming no question (launch_id), so which "+
				"question it answers cannot be known; it is spent without resuming the run, "+
				"and the person has to answer the run again")
		return AnswerNotMine, fmt.Errorf("%w: run %s", errAnswerNamesNoQuestion, given.TurnID)
	}
	delivery := deliveryOf(trigger)
	if delivery == "" {
		// NOTHING TO RECORD IT UNDER: a record names the deliveries it was
		// made of, and a copy coming back is recognised by them.
		return AnswerNotMine, fmt.Errorf("sandbox: the answer to run %s arrived with no event to "+
			"record it under", given.TurnID)
	}
	for range casRetries {
		run, found, err := c.pending.Get(ctx, given.TurnID)
		if err != nil {
			// A STORE THAT COULD NOT BE READ IS NOT A RUN THAT IS GONE.
			// Handed back, and nothing is announced: the answer has not
			// become anything yet.
			return AnswerDeferred, fmt.Errorf("sandbox: reading run %s for the answer it was given: %w",
				given.TurnID, err)
		}
		if !found {
			// SETTLED, or never there: a run that is over has no record. What
			// is known about it is what the answer itself named.
			answered(PendingRun{TurnID: given.TurnID, AgentHandle: given.AgentHandle}, types.AnswerGone)
			return AnswerNotMine, nil
		}
		if run.AgentHandle != given.AgentHandle {
			// THE ANSWER REACHED ANOTHER SEAT'S INBOX. It is addressed off
			// the row it was accepted against, so this is a delivery this
			// seat must not act on — resuming it here would re-enter
			// another seat's conversation as this one.
			log.ErrorContext(ctx, "sandbox_answer_misrouted",
				"turn_id", given.TurnID, "addressed_to", given.AgentHandle,
				"run_seat", run.AgentHandle,
				"detail", "an answer by turn arrived on a seat that does not hold the run it "+
					"names; it is dropped rather than resumed under the wrong seat")
			return AnswerNotMine, nil
		}
		if run.Answer != nil && slices.Contains(run.Answer.EventIDs, delivery) {
			// THIS VERY ANSWER, ALREADY RECORDED: a redelivery whose
			// acknowledgement was lost, or the node that recorded it
			// stopping before it acked. Spent as the answer it is — and if
			// this node is not already driving its resume, it starts.
			if run.Status == StatusAnswered && !c.owes(run.TurnID) {
				c.resumeOwed(ctx, run)
			}
			return AnswerConsumed, nil
		}
		if slices.Contains(run.DeclinedAnswers, delivery) {
			// THE QUESTION LET THIS ANSWER GO — every attempt to resume
			// with it failed — and this is the copy its decline handed
			// back to the seat, or the original come round again. It has
			// no ordinary form to become, so it is spent, and the person
			// told their answer was not used: by the COPY, which the
			// decline publishes once through the row's outbox, so the
			// original coming round after it says nothing a second time.
			log.WarnContext(ctx, "sandbox_answer_declined_spent",
				"turn_id", run.TurnID, "agent", run.AgentHandle, "delivery", delivery,
				"detail", "an answer by turn its run could not be resumed with was let go of; "+
					"the run waits on its question again and has to be answered again")
			if declinedCopy(run.DeclinedAnswers, delivery) {
				answered(run, types.AnswerDeclined)
			}
			return AnswerNotMine, nil
		}
		if !slices.Contains(Awaiting, run.Status) || given.LaunchID != run.LaunchID {
			// NOT WAITING, OR NOT WAITING ON THE QUESTION THIS ANSWERS: the
			// run was answered another way, or resumed and moved on to a
			// question of its own, since the answer was given. See "The
			// question it answers" above.
			answered(run, types.AnswerNotAwaiting)
			return AnswerNotMine, nil
		}
		recorded, won, err := c.record(ctx, run, answerGivenOf(given, trigger, c.now()))
		if err != nil {
			// THE RECORD MAY HAVE LANDED — the chat route's ambiguous case,
			// resolved the same way: towards the run. If it did land, the
			// redelivery finds this very answer on the row and is spent as
			// it.
			return AnswerDeferred, fmt.Errorf("sandbox: recording the answer run %s was given: %w",
				given.TurnID, err)
		}
		if !won {
			// LOST: another answer was recorded first, or the run moved on,
			// between the read and the record. Read again, so the outcome
			// is decided against what the store holds now.
			continue
		}
		log.InfoContext(ctx, "sandbox_clarification_answered",
			"turn_id", recorded.TurnID, "via", string(types.AnswerViaOperator),
			"answered_by", given.AnsweredBy, "answered_by_seat", given.AnsweredBySeat)
		c.clearAnswerAttempts(recorded.AgentHandle, recorded.TurnID)
		c.resumeOwed(ctx, recorded)
		return AnswerConsumed, nil
	}
	return AnswerDeferred, fmt.Errorf("sandbox: run %s kept changing under the answer it was given",
		given.TurnID)
}

// errRunMovedOn is a resume's report that the run it would have ended moved on
// to somebody else first — a newer lease owns it, or it was ended by another
// party — so the disposition beside it ([AnswerNotMine]) is not a run that is
// gone: whoever holds the run now drives what becomes of it, and announces it.
var errRunMovedOn = errors.New("sandbox: the run moved on to another holder before this " +
	"attempt could end it")

// errAnswerNamesNoQuestion is an answer by turn that carries no
// [types.SandboxAnswerGiven.LaunchID]: it is spent rather than resumed with,
// because the question it answers cannot be known.
var errAnswerNamesNoQuestion = errors.New("sandbox: the answer names no question it answers " +
	"(no launch_id), so it is spent without resuming the run")

// declinedCopy reports whether delivery is the COPY a decline handed back for
// one of the deliveries declined beside it ([declinedCopyID]), rather than one
// of those deliveries itself.
func declinedCopy(declined []string, delivery string) bool {
	for _, id := range declined {
		original, err := uuid.Parse(id)
		if err == nil && declinedCopyID(original).String() == delivery {
			return true
		}
	}
	return false
}

// answerGivenOf is what recording an answer by turn writes: the answer as the
// person gave it, the credential and person it names, and the one delivery it
// arrived in — the [types.SandboxAnswerGiven] event itself, carried so a copy
// of it can be handed back to the seat should the answer be let go of.
func answerGivenOf(given types.SandboxAnswerGiven, trigger *events.Event, now time.Time) RecordedAnswer {
	answer := RecordedAnswer{
		// Redacted HERE, as a chat reply is: it is spliced into a phase
		// record verbatim.
		Text: redact.Secrets(given.Answer), Via: types.AnswerViaOperator,
		By: given.AnsweredBy, BySeat: given.AnsweredBySeat,
		EventIDs: []string{trigger.ID.String()}, PostedAt: trigger.Timestamp, RecordedAt: now,
	}
	raw, err := json.Marshal(trigger)
	if err != nil {
		// Kept without it, as a chat reply's event is: the answer is the
		// text, and an event that cannot be carried costs only the copy a
		// let-go would hand back.
		log.Warn("sandbox_answer_event_unencodable", "event_id", trigger.ID.String(), "error", err.Error())
		return answer
	}
	answer.Events = []json.RawMessage{raw}
	return answer
}

// answeredAs is what a settled answer became, from what the resume left the
// delivery: a turn that ran consumed it, and anything else is a run that is
// gone. Never called for a deferral, which has not become anything.
func answeredAs(disposition AnswerDisposition) types.AnswerOutcome {
	if disposition == AnswerConsumed {
		return types.AnswerResumed
	}
	return types.AnswerGone
}

// answererOf is who a chat reply came from, as its envelope names them.
func answererOf(trigger *events.Event) string {
	if trigger == nil {
		return ""
	}
	return trigger.Actor()
}

// announceAnswered publishes what an answer to a parked run became.
//
// BEST EFFORT, like every other announcement this coordinator makes after the
// fact: the answer already did whatever it did, and a feed row that could not
// be written is a log line rather than a reason to hand the delivery back and
// resume the run a second time.
func (c *Coordinator) announceAnswered(ctx context.Context, run PendingRun,
	via types.AnswerVia, outcome types.AnswerOutcome, by, bySeat string,
) {
	payload := types.SandboxRunAnswered{
		Agent: run.AgentID, AgentHandle: run.AgentHandle, RoleName: run.Role,
		TurnID: run.TurnID, WorkKey: run.UnitOfWork(), WorkItem: run.WorkItem,
		Via: via, Outcome: outcome, AnsweredBy: by, AnsweredBySeat: bySeat,
	}
	ev := events.New(payload, events.TraceContext{TraceID: run.TraceID, ParentSpanID: run.SpanID})
	ev.Source = run.Role
	if err := c.queue.Publish(ctx, topics.Event(payload.EventType()), ev); err != nil {
		log.WarnContext(ctx, "sandbox_answer_announce_failed",
			"turn_id", run.TurnID, "via", string(via), "outcome", string(outcome),
			"error", err.Error())
	}
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
// can have run and concluded something (the delivery is spent). Its callers
// arrive by different routes and read the pair differently — a completion
// NAKs on the error and lets the broker's own budget bound the retry (the
// message names its run, so where it comes back does not matter); and a
// RECORDED answer — a chat reply or an answer by turn — is retried by this
// coordinator on [AnswerDeferred], bounded by [MaxAnswerAttempts]
// ([Coordinator.attemptOwed]).
//
// # A recorded answer the run ends without
//
// A run claimed out of [StatusAnswered] carries the person's reply on its row,
// and a resume can end the run before any turn took that reply: no
// conversation to re-enter, a claim that cannot be given back, a resume that
// broke before its turn began. ON EVERY ROUTE the reply then goes back to the
// seat through the run's row: the store will not delete a row still holding a
// reply no turn took ([ErrAnswerOwed]), so the ending lets it go first,
// recording its copies as owed before the record is deleted
// ([Coordinator.letGoAnswer]), and a crash anywhere in between leaves them for
// whoever reads the row next. The inline attempt used to hand the delivery it
// still held on instead, which needed the store to delete a reply on the
// caller's word — and an ending that could not land then left the reply both
// on the row and handed on.
//
// A REPLY A TURN TOOK IS SPENT, whatever becomes of the turn, and the line
// between the two is drawn by the turn's own start ([ResumeRequest.Begin]) —
// which is where its delivery is recorded as worked, too.
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
		// A claimed run without one is a row this build's launch path did
		// not write, and it is failed as no_execute_state rather than
		// resumed into nothing.
		log.WarnContext(ctx, "sandbox_resume_no_execute_state",
			"turn_id", run.TurnID, "claimed_from", run.ClaimedFrom,
			"detail", "the row carried no suspended conversation; the turn "+
				"cannot be resumed and the run is failed")
		if !c.settleFailed(ctx, run, types.SandboxFailureNoConversation,
			"the run record carried no suspended conversation, so the turn that "+
				"started it cannot be continued") {
			return AnswerNotMine, errRunMovedOn
		}
		// TERMINALLY GONE, so nothing is owed the delivery that got here:
		// requeueing it would circle a run this settle has just deleted,
		// and acking it would swallow a person's message on behalf of a
		// turn that no longer exists. A recorded reply is not lost with it:
		// the ending handed it back through the row.
		return AnswerNotMine, nil
	}
	// Freed only NOW, immediately before the resume, so no queued event can
	// take the slot first. The claim holds the row in [StatusResumed]
	// whichever set it was claimed from, so that is the count to give back.
	c.uncountRun(run.AgentHandle, StatusResumed)

	// WHETHER THE TURN BEGAN, which the resumer reports by calling Begin —
	// see [ResumeRequest.Begin]. Atomic because nothing here promises the
	// resumer calls it on this goroutine.
	var began atomic.Bool
	begin := func(ctx context.Context) error {
		if claimedAnswer(run) {
			// THE TURN TAKES THE ANSWER, on the row, or it does not run —
			// under the CLAIMANT's lease, which the claim stamped on the
			// row ([PendingStore.ClaimForResume]).
			taken, err := c.pending.TakeAnswer(ctx, run.TurnID, run.LaunchID, fenceOf(run))
			if err != nil {
				return fmt.Errorf("sandbox: recording that run %s's turn took its answer: %w",
					run.TurnID, err)
			}
			if !taken {
				return fmt.Errorf("sandbox: run %s is no longer the claim that holds its answer: "+
					"the answer was let go of, or the seat's next holder fenced the row", run.TurnID)
			}
			// AND ITS DELIVERY IS SPENT AT THE TAKE, on every route: from
			// here a copy of it reaching the seat is a reply a turn already
			// has. The inline attempt holds that delivery unacknowledged
			// through the whole turn, and a node that stops in the middle
			// of it — its turn may already have answered the person — leaves
			// the delivery to come round to the seat's next holder, which
			// finds the run reaped and nothing on it to recognise the copy
			// by. Spent only once the turn returned, it ran as a second
			// turn. See [CoordinatorOptions.Spent].
			c.spend(ctx, run.AgentHandle, *run.Answer)
		}
		began.Store(true)
		return nil
	}

	// STRAIGHT TO THE RESUMER, which [NewCoordinator] refuses to be built
	// without: "this node cannot resume this run" is the resumer's own
	// answer, wrapping [ErrResumeUnavailable], and it takes the failure
	// path below like every other failed resume.
	if err := c.resume.Resume(ctx, ResumeRequest{
		Run: run, Answer: answer, Success: success, Trigger: trigger,
		DeliveredRefs: outcome.DeliveredRefs,
		InputTokens:   outcome.InputTokens, OutputTokens: outcome.OutputTokens,
		Begin: begin,
	}); err != nil {
		if errors.Is(err, ErrResumeAbandoned) && !began.Load() {
			// BROKEN BEFORE ITS TURN BEGAN — a panic re-entering the
			// conversation, say. It is not retried, for the reason every
			// abandoned resume is not: the same bytes reach the same
			// defect. But nothing ran, so nothing a person sent was used:
			// a recorded reply goes back to the seat through the row.
			//
			// AND IT IS ANNOUNCED, like every other run lost without its
			// turn: no turn ran to publish a completion of its own, so the
			// run's ending is the only account of what became of it. It
			// used to leave a guard breach and nothing else, so the turn
			// parked on this run read as parked for good.
			log.ErrorContext(ctx, "sandbox_resume_abandoned_before_turn",
				"turn_id", run.TurnID, "error", err.Error(),
				"detail", "the resume broke before the turn it would continue began; the run "+
					"is ended and announced rather than retried, and a person's reply that "+
					"drove the resume goes back to the seat rather than being spent")
			if !c.settleAbandoned(ctx, run, &failureNote{
				reason: types.SandboxFailureResumeBroken, detail: resumeBrokenDetail,
			}) {
				// NOT THIS NODE'S TO END after all: the run moved on —
				// the seat's next holder fenced the claim and revived it,
				// say — and what becomes of it, and of a reply on it, is
				// that holder's to say.
				return AnswerNotMine, errRunMovedOn
			}
			return AnswerNotMine, nil
		}
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
			// run_sandbox again before it broke, and that relaunch counted
			// the seat busy on a job this settle has just ended. And
			// nothing is announced, because the turn did resume and has
			// already published its own failed completion. The reply it
			// took was spent when it took it.
			log.ErrorContext(ctx, "sandbox_resume_abandoned",
				"turn_id", run.TurnID, "error", err.Error(),
				"detail", "the run is settled rather than un-claimed, so the completion is "+
					"not redelivered into a conversation a retry must not re-enter")
			c.settleAbandoned(ctx, run, nil)
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
			// announced, and a recorded reply went back through its row.
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
		c.settleClaimed(ctx, run, err, nil)
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
	c.finish(ctx, latest, ending{fence: fenceOf(run)})
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

// claimedAnswer reports whether a claimed run carries a RECORDED answer its
// claim was taken for — the chat route's, which lives on the row rather than
// in a delivery.
func claimedAnswer(run PendingRun) bool {
	return run.ClaimedFrom == StatusAnswered && run.Answer != nil
}

// settleAbandoned ends a run whose resume was abandoned, from its record as it
// stands — or, where that cannot be read, while it is still the claim this
// resume took ([Coordinator.settleClaimed]) — and recounts the seat. A
// recorded reply no turn took goes back to the seat as it ends, as it does
// from every ending ([ErrAnswerOwed]); one the turn took is spent.
//
// AND SETTLED EVEN WHEN THE RECORD CANNOT BE READ. A read failure here used to
// settle nothing, which left the row in the claim an abandoned resume exists
// to keep — the one state nothing recovers from.
//
// note is what the ending announces: nil for a resume whose turn ran, which
// published its own failed completion, and the loss for one whose turn never
// began — see [resumeBrokenDetail].
//
// Reports whether the run's ending is this node's ([Coordinator.finish]): false
// is a run that moved on to somebody else before it could be ended here — the
// seat's next holder fenced the claim and revived it, say — whose fate this
// node must not announce.
func (c *Coordinator) settleAbandoned(ctx context.Context, run PendingRun, note *failureNote) bool {
	defer c.syncSeat(ctx, run.AgentHandle)
	settle, readErr := c.current(ctx, run)
	if readErr != nil {
		return c.settleClaimed(ctx, run, readErr, note)
	}
	_, ours := c.finish(ctx, settle, ending{fence: fenceOf(run), note: note})
	return ours
}

// spend is [CoordinatorOptions.Spent] for one recorded answer.
func (c *Coordinator) spend(ctx context.Context, handle string, answer RecordedAnswer) {
	if c.spent == nil {
		return
	}
	if evs := answer.decodedEvents(); len(evs) > 0 {
		c.spent(ctx, handle, evs)
	}
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
// [Coordinator.unclaim]). What a caller does with the error instead is
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

// claimedOnly is the status a claim's own ending is licensed for: the run is
// ended only while it is still the claim — see [Coordinator.endClaim].
var claimedOnly = []string{StatusResumed}

// endClaim ends a run WHILE IT IS STILL THE CLAIM THIS CALL TOOK — its status,
// its job and its lease — for an ending decided WITHOUT knowing what the row
// holds: a settle whose read of the row failed ([Coordinator.settleClaimed]),
// and a claim whose release could not be confirmed ([Coordinator.unclaim]).
// Reports whether it ended the run, and the ending's error where it was kept
// for its retry ([errEndingOwed]).
//
// THE CONDITION MOVES TO THE STORE, which is what makes acting without the
// read safe. An ending licensed for this claim alone is not decided on a run
// that moved off it: a relaunch the resumed turn made takes the row through
// [StatusLaunching] and a new job, and the claim that job's own completion
// takes is the same status on another job — which a license on the status
// alone ended, box and all. A release that LANDED although it reported a
// failure puts the run back where the claim found it, and an ending licensed
// for every status then ended a run that was fine, with the person's reply on
// it. Declined, either is left to whatever moved it.
//
// DECIDED FIRST AND RECLAIMED SECOND, as every ending is ([Coordinator.finish]),
// and here the reason is sharpest: the decision is what establishes the row was
// still this call's to end, so reclaiming first would destroy the box before it
// — the box of a relaunch, or of a run handed back for its retry. The reclaim
// rides the recorded ending ([RecordedEnding.Reclaim]), so whichever attempt
// finishes the ending — this node's retry, or the seat's next holder — reclaims
// the box before it deletes the record, rather than leaving the box billed
// behind a record that is gone.
func (c *Coordinator) endClaim(ctx context.Context, run PendingRun, e ending) (bool, error) {
	// DETACHED, like every other teardown: this is reached from a failure
	// path and from a queue handler a drain cancels, so the context that
	// got here is very often already dead — and a settle that no-ops is the
	// stranded row all over again.
	endCtx, cancel := detached(ctx)
	defer cancel()
	e.fence, e.whileIn, e.launch, e.reclaim = fenceOf(run), claimedOnly, run.LaunchID, true
	decided, err := c.endRecord(endCtx, run, e)
	return decided != nil, err
}

// settleClaimed ends a run whose record could not be read, WHILE IT IS STILL
// CLAIMED ([Coordinator.endClaim]).
//
// SILENCE IS NOT AN ANSWER HERE. Both callers reach it having just taken a
// suspended turn out of the engine's hands, and the row they would leave
// behind is the claim itself: the completion poll does not read a
// [StatusResumed] row, a redelivery cannot re-claim one, no answer matches one
// and no reaper expires one. So "leave it for the next pass" is a turn
// destroyed in silence beside a paused box billed until the seat happens to
// change hands — exactly what [Coordinator.unclaim] ends a run rather than
// accept. The only hazard the read ever guarded was killing a job the resumed
// turn RELAUNCHED — a relaunch reuses this very box — and the claim's own
// license declines that.
//
// note is what the ending announces, nil for one whose turn ran and announced
// itself. Reports whether the run's ending is this node's — ended here, or kept
// for this node's retry — as [Coordinator.finish] does.
func (c *Coordinator) settleClaimed(ctx context.Context, run PendingRun, cause error, note *failureNote) bool {
	ended, err := c.endClaim(ctx, run, ending{note: note})
	switch {
	case err != nil:
		log.ErrorContext(ctx, "sandbox_claimed_settle_kept",
			"turn_id", run.TurnID, "error", err.Error(), "cause", cause.Error(),
			"detail", "the run's record could neither be read nor ended; its ending is kept and "+
				"this node finishes it, its box included, once the store answers — or the seat's "+
				"next recovery pass reaps it, if the seat moves first")
	case !ended:
		// Not ours: the run either moved on under us — a relaunch the
		// turn made before it broke — or somebody else ended it. Either
		// way that party owns the box and the record.
		log.InfoContext(ctx, "sandbox_claimed_settle_declined",
			"turn_id", run.TurnID, "cause", cause.Error(),
			"detail", "the run is no longer the claim this settle took, so it is left to "+
				"whoever moved it")
	default:
		log.WarnContext(ctx, "sandbox_claimed_settled_unread",
			"turn_id", run.TurnID, "error", cause.Error(),
			"detail", "the run's record could not be read after the resume, so it was ended "+
				"while it was still the claim rather than left in it, and its box reclaimed")
	}
	return ended || err != nil
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
// billed until the seat happens to change hands. It is settled instead — record
// deleted, box reclaimed, loss announced, stop reported — which is what every
// other destroyed turn gets, and detail is the sentence that reaches an
// operator's board. A person's reply the claim holds goes back to the seat as
// it ends, EVEN ONE ITS TURN TOOK: a turn that gave its claim back has said
// nothing it did reached anybody, and the retry that would have handed the
// reply to a turn of its own will never come ([RecordedEnding.Unused]).
//
// BUT A RELEASE THAT REPORTED A FAILURE MAY HAVE LANDED, and then the run is
// not stranded at all: it is back where the claim found it, owed its retry, and
// for a recorded answer carrying the person's reply. So it is ended ONLY WHILE
// IT IS STILL THE CLAIM ([Coordinator.endClaim]), which the store decides: one
// the release did reach is declined and left to its retry, exactly as if the
// release had answered. Ended on every status instead, the run was killed
// under its own retry — box reclaimed, record deleted — and, when the ending
// then refused to let the reply go from a run that no longer read as the
// claim, the reply was deleted with it.
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
// Reports whether the run may still be in the hands of whatever will retry it:
// nil where the claim went back (or had already moved on), or where the ending
// could not be finished and is kept — whether the hand-back landed is then
// still open, and a signal that comes back finds the outcome — and the
// release's own error where the run was ended in its place. The caller's own
// error is not this answer — a retry keeps it, so the delivery comes back, and
// an ending drops it, because there is nothing left to come back to.
func (c *Coordinator) unclaim(ctx context.Context, run PendingRun, counted bool, detail string) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), discardGrace)
	defer cancel()
	to := claimedFrom(run)
	released, err := c.pending.ReleaseClaim(ctx, run.TurnID, Release{
		Launch: run.LaunchID, To: to, Charged: run.Charged,
		Published: run.LaunchFacts().Published, Fence: fenceOf(run),
	})
	switch {
	case err != nil:
		log.ErrorContext(ctx, "sandbox_claim_revert_failed",
			"turn_id", run.TurnID, "revert_to", to, "error", err.Error(),
			"detail", "the claim could not be handed back, so nothing would ever pick "+
				"this run up; it is ended and announced instead, unless the hand-back "+
				"landed after all")
		ended, endErr := c.endClaim(ctx, run, ending{
			unused: claimedAnswer(run),
			note:   &failureNote{reason: types.SandboxFailureClaimStranded, detail: detail},
		})
		// AUTHORITATIVE, not assumed: the ending moved the row, or found
		// it moved, and this call moved the counts, and neither knows
		// what the other found.
		c.syncSeat(ctx, run.AgentHandle)
		switch {
		case ended:
			return fmt.Errorf("sandbox: handing back the claim on %s (to %s): %w", run.TurnID, to, err)
		case endErr != nil:
			// THE ENDING IS KEPT, and with it the question of whether the
			// hand-back landed: this node finishes the ending, or finds the
			// run handed back after all and leaves it to its retry. Until
			// then the run is not known to be gone, so the caller takes the
			// retry's path — a signal that comes back finds the outcome.
			log.WarnContext(ctx, "sandbox_claim_revert_undecided",
				"turn_id", run.TurnID, "revert_to", to, "error", endErr.Error(),
				"detail", "the claim could not be handed back and the run could not be ended "+
					"either; this node keeps the ending and finishes it, or leaves the run to "+
					"its retry if the hand-back landed")
		default:
			log.WarnContext(ctx, "sandbox_claim_revert_landed",
				"turn_id", run.TurnID, "revert_to", to,
				"detail", "the run is no longer the claim: the hand-back that reported a "+
					"failure landed, or the run moved on, so it is left to its retry")
		}
		return nil
	case !released:
		log.WarnContext(ctx, "sandbox_claim_moved_on",
			"turn_id", run.TurnID, "launch_id", run.LaunchID,
			"detail", "the run no longer holds this claim, so nothing was handed back: "+
				"the resumed turn launched another job, the seat's next owner reaped it, "+
				"or its ending let its answer go")
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
// Announced by the ending itself ([Coordinator.endRecord]), and only for the
// ending decided on the run: one that a newer lease owns, or that somebody else
// ended first, is that party's to settle and to explain, and one whose ending
// somebody else DECIDED first is finished for that party's reason — a second
// announcement would name a reason the run did not end for. The engine is told
// on the same gate, by the same decision — see [CoordinatorOptions.Stopped].
// A recorded reply no turn took goes back to the seat as the run ends, as it
// does from every ending ([ErrAnswerOwed]). Reports whether the run's ending is
// this node's ([Coordinator.finish]).
func (c *Coordinator) settleFailed(ctx context.Context, run PendingRun, reason, detail string) bool {
	_, ours := c.finish(ctx, run, ending{
		fence: fenceOf(run),
		note:  &failureNote{reason: reason, detail: detail},
	})
	// Out of whichever set the record was in: this path settles a claimed
	// run ([StatusResumed]) and a launch that never suspended
	// ([StatusLaunching]) alike, and a status names its own set.
	c.uncountRun(run.AgentHandle, run.Status)
	return ours
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

// resumeBrokenDetail is the sentence a run whose resume broke before its turn
// began reaches an operator's board with: nothing of the turn ran, and trying
// again would re-enter the same suspended conversation and reach the same
// defect, so the run is ended rather than retried.
const resumeBrokenDetail = "the turn this coding run belongs to could not be re-entered — " +
	"resuming it broke before the turn began, and another attempt would reach the same " +
	"defect — so the turn cannot be continued; the work it pushed, if any, is on its branch"

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

// announceEnding publishes a decided ending's announcement — the lost run, to the
// board and to its seat — under the identity the decision recorded.
//
// TWO PUBLISHES, as for a start and a completion: the events copy is what a
// dashboard and the event store read, and the per-seat control copy reaches the
// node that was running the turn.
//
// THE SAME EVENT ON EVERY ATTEMPT. Its id is derived from the run and the
// ending ([endingEventID]) and its instant is the decision's, so an
// announcement made again by the attempt that finishes the ending after a crash
// — this node's retry, or the seat's next holder — is one event to the event
// store, whose key is (event_time, event_id), and to the fleet's reads, which
// merge rows on that same pair. It is made BEFORE the record is deleted, and
// the announcement an ending used to make after its delete was lost to a crash
// between the two — or never made, when a delete that landed reported a
// failure and the attempt that would have announced found no row.
//
// The events copy must land, because it is the only account of how the run
// ended: a publish the broker refused keeps the ending for its retry
// ([Coordinator.oweEnding]). The control copy is a courtesy to the node running
// the turn and stays best effort.
func (c *Coordinator) announceEnding(ctx context.Context, run PendingRun) error {
	ending := run.Ending
	detail := ending.Detail
	if ending.Returned {
		detail += replyReturnedDetail
	}
	failed := types.SandboxRunFailed{
		Agent: run.AgentID, AgentHandle: run.AgentHandle, RoleName: run.Role,
		// UnitOfWork, never the raw field: see [PendingRun.UnitOfWork].
		TurnID: run.TurnID, WorkKey: run.UnitOfWork(), SandboxID: run.SandboxID,
		CodingAgent: run.CodingAgent,
		Reason:      ending.Reason, Detail: redact.Secrets(detail),
	}
	ev := events.New(failed, events.TraceContext{
		TraceID: run.TraceID, ParentSpanID: run.SpanID,
	})
	ev.ID, ev.Timestamp = endingEventID(run), ending.At.UTC()
	ev.Source = run.Role
	if err := c.queue.Publish(ctx, topics.Event(failed.EventType()), ev); err != nil {
		return fmt.Errorf("sandbox: announcing the end of run %s (%s): %w", run.TurnID, ending.Reason, err)
	}
	if control := topics.AgentControl(run.AgentHandle); control != "" {
		if err := c.queue.Publish(ctx, control, ev); err != nil {
			log.WarnContext(ctx, "sandbox_failure_control_failed",
				"turn_id", run.TurnID, "reason", ending.Reason, "error", err.Error())
		}
	}
	return nil
}

// endingNamespace derives the ids decided endings are announced under.
var endingNamespace = uuid.MustParse("3c1f5a52-9d0e-5b8a-8f47-2a6d1e9b7c40")

// endingEventID is the id a decided ending's announcement is published under:
// derived from the run and the ending rather than minted per publish, so every
// attempt that finishes the ending publishes the same event.
func endingEventID(run PendingRun) uuid.UUID {
	return uuid.NewSHA1(endingNamespace, []byte(run.TurnID+"/"+run.Ending.ID))
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

// ending is one decision to end a run, as its caller makes it: the license it
// is taken under, whether a person's reply its turn took went unused, whether
// the box is reclaimed after the decision, and what it announces.
//
// ONE VALUE RATHER THAN SIX ARGUMENTS, because a decision can outlive the call
// that made it: one the store could not confirm is kept and made again by this
// node's retry ([Coordinator.oweEnding]) on exactly these terms. Once the
// decision is RECORDED on the row ([RecordedEnding]) the terms that count are
// the recorded ones, whoever finishes it.
type ending struct {
	// fence, whileIn and launch are the license: see [License].
	fence   Fence
	whileIn []string
	launch  string

	// unused is whether the ending lets the recorded answer go back to the
	// seat even though a turn TOOK it: the claim's own ending, after its turn
	// gave the claim back as a retry — which says nothing it did reached
	// anybody — and the release could not land, so no retry will hand the
	// reply to a turn again. Every other ending lets go only of a reply no
	// turn took, which the store refuses to delete with the row
	// ([ErrAnswerOwed]), and spends one a turn took.
	unused bool

	// reclaim is whether the box is reclaimed after the decision, by whichever
	// attempt finishes the ending — every ending that has not reclaimed a box
	// of its own, so that the store's fence check decides whether this call
	// may kill the box at all ([Coordinator.finish]).
	reclaim bool

	// note is what the ending announces, nil for an ending that announces
	// nothing ([Coordinator.announceEnding]).
	note *failureNote
}

// decision is the ending as the store records it.
func (e ending) decision() Decision {
	d := Decision{
		License: License{Fence: e.fence, WhileIn: e.whileIn, Launch: e.launch, Unused: e.unused},
		Reclaim: e.reclaim,
	}
	if e.note != nil {
		d.Reason, d.Detail = e.note.reason, e.note.detail
	}
	return d
}

// failureNote is a lost run's announcement: its reason and the sentence that
// reaches an operator's board.
type failureNote struct{ reason, detail string }

// replyReturnedDetail is what an announcement adds when the ending hands a
// person's answer back to the seat ([RecordedEnding.Returned]).
const replyReturnedDetail = "; the answer a person gave it goes back to the seat"

// errEndingOwed marks an ending that is KEPT for this node's retry
// ([Coordinator.oweEnding]): its decision could not be confirmed, or a step of
// it — the reply it hands back, its announcement, the delete — did not land.
var errEndingOwed = errors.New("sandbox: the run's ending is kept until it can be finished")

// endRecord ends a run: it DECIDES the ending on the run's row
// ([PendingStore.DecideEnding]) — or finds one already decided there, which it
// finishes in place of its own — and FINISHES it ([Coordinator.finishEnding]):
// the reply it lets go of handed back, its box reclaimed where it says so, its
// announcement published and the record deleted.
//
// THE ONE PLACE A RUN IS ENDED, which is what makes it the one place that can
// say a suspended turn is over, and why the STOP is reported here rather than
// by each ending. Three rounds of hand-enumerating those endings each missed
// one, and the count in [CoordinatorOptions.Stopped] was wrong again the moment
// a fourth arrived; a report keyed to the decision cannot be missed by a path
// that ends a run, and needs no count. It is reported where the decision is this
// call's to finish, so a racer whose license the run moved off neither announces
// a reason the run did not end for nor drops a hold belonging to whoever did end
// it.
//
// It reports for a turn that came back too — a collected run whose resumed
// executor finished, which every ordinary completion is. That is deliberate:
// the hold is ended by the resume's own frame before this call is reached, so
// the report finds nothing to drop, and a report gated on "was this turn
// suspended when we got here" would be a second opinion about a question the
// store has already answered. Over-reporting costs a map lookup;
// under-reporting is the defect. See [CoordinatorOptions.Stopped].
//
// EXACTLY ONE ANNOUNCEMENT, whichever node finishes it and however many times:
// the decision is recorded once, and every attempt announces it under the
// identity the decision recorded ([Coordinator.announceEnding]).
//
// AN ENDING THAT COULD NOT BE FINISHED IS KEPT — a decision the store could not
// confirm, a reply that could not be handed back, an announcement the broker
// refused, a delete the store did not take — and this node retries it, holding
// the seat's inbox until it lands ([Coordinator.oweEnding]); the seat's next
// holder finishes a decided one if the seat moves first. A decision that could
// not be confirmed reports the stop too, for the old asymmetry: it may have
// landed, and nothing a caller leaves behind resumes the turn either way.
//
// Reports the ending decided under this call's license — the run is over, and
// this is the decision every attempt finishes, which may be one somebody else
// decided first — or nil where none is; and the kept ending's error
// ([errEndingOwed]), which is what a caller that can RETRY needs and a settle
// has no use for.
func (c *Coordinator) endRecord(ctx context.Context, run PendingRun, e ending) (*RecordedEnding, error) {
	decided, ok, err := c.pending.DecideEnding(ctx, run.TurnID, e.decision())
	if err != nil {
		c.reportStopped(ctx, run)
		// FORGOTTEN ON THE SAME GATE THE STOP TAKES, and for the same
		// asymmetry: the decision may well have landed, and a count kept
		// for a run nothing will ever offer a delivery to again is a map
		// entry this process never drops. See [answerBudget].
		c.clearAnswerAttempts(run.AgentHandle, run.TurnID)
		return nil, c.keepEnding(ctx, run, e, fmt.Errorf("sandbox: deciding the end of run %s: %w",
			run.TurnID, err))
	}
	if !ok {
		return nil, nil
	}
	c.reportStopped(ctx, decided)
	c.clearAnswerAttempts(decided.AgentHandle, decided.TurnID)
	ending := *decided.Ending
	if err := c.finishEnding(ctx, decided); err != nil {
		return &ending, c.keepEnding(ctx, decided, e, err)
	}
	return &ending, nil
}

// finishEnding does what a decided ending ([PendingRun.Ending]) still has to
// do, IN THIS ORDER, and deletes the record last:
//
//  1. a person's reply the run still holds and owes the seat is LET GO
//     ([Coordinator.letGoAnswer]), its copies recorded on the row as owed;
//  2. every copy the row owes ([PendingRun.HandBack]) is published and cleared;
//  3. the box is reclaimed, where the ending reclaims it after its decision;
//  4. the announcement is published, under the ending's own identity;
//  5. the record is deleted ([PendingStore.Finish]).
//
// # Across a crash at each step
//
// Nothing reads a deleted row again, so everything the ending owes is done
// before the delete, and every step is one a repeat makes harmless: the let-go
// finds nothing left to let go of, the copies go out again under the same
// derived ids (the inbox's same-id dedupe and the completion ledger collapse
// them), a box already reclaimed is a kill of nothing, and the announcement is
// the same event again. Stopped anywhere before the delete, the row still
// carries the ending, and whoever reads it next — this node's retry, or the
// seat's next holder ([Coordinator.RecoverSeat]) — finishes it from the step it
// stopped at.
//
// # A delete the store refuses
//
// The store will not delete a row that still owes the seat copies or holds a
// reply the ending must hand back ([ErrHandBackOwed], [ErrAnswerOwed]), and
// hands back the row as it stands; the steps run again against it. A step that
// does not land is returned, and the caller keeps the ending.
func (c *Coordinator) finishEnding(ctx context.Context, run PendingRun) error {
	var handed []string
	reclaimed, announced := false, false
	for range casRetries {
		if run.answerOwed() {
			owing, err := c.letGoAnswer(ctx, run)
			if err != nil {
				return fmt.Errorf("sandbox: ending run %s, which holds a reply it could not let go "+
					"of: %w", run.TurnID, err)
			}
			run = owing
		}
		if len(run.HandBack) > 0 {
			owed := handBackIDs(run.HandBack)
			if slices.Equal(owed, handed) {
				// THE SAME COPIES AGAIN: they are out, and clearing them is
				// what did not land. Publishing them once more would change
				// nothing, so the ending waits for its retry rather than
				// going round here.
				return fmt.Errorf("sandbox: ending run %s: the reply it owed its seat is handed "+
					"back but could not be cleared from it", run.TurnID)
			}
			if err := c.deliverHandBack(ctx, run); err != nil {
				return fmt.Errorf("sandbox: ending run %s, which owes its seat a reply it could not "+
					"hand back: %w", run.TurnID, err)
			}
			handed = owed
		}
		if run.Ending.Reclaim && !reclaimed {
			// THE BOX THE ROW NAMES, under a context of its own: a teardown
			// is reached from failure paths whose context is often dead.
			killCtx, cancel := detached(ctx)
			_ = c.reclaimBox(ctx, killCtx, run)
			cancel()
			reclaimed = true
		}
		if run.Ending.Reason != "" && !announced {
			if err := c.announceEnding(ctx, run); err != nil {
				return err
			}
			announced = true
		}
		settled, _, err := c.pending.Finish(ctx, run.TurnID, run.Ending.ID)
		switch {
		case err == nil:
			// GONE, by this delete or by whoever finished the same ending
			// first: either way there is nothing left of it to do.
			return nil
		case errors.Is(err, ErrHandBackOwed), errors.Is(err, ErrAnswerOwed):
			run = settled
		default:
			return fmt.Errorf("sandbox: ending run %s, whose record could not be deleted: %w",
				run.TurnID, err)
		}
	}
	return fmt.Errorf("sandbox: ending run %s: it kept owing its seat a reply after handing one back",
		run.TurnID)
}

// keepEnding keeps an ending that could not be finished for this node's retry
// ([Coordinator.oweEnding]) and returns cause as the kept ending's error.
func (c *Coordinator) keepEnding(ctx context.Context, run PendingRun, e ending, cause error) error {
	c.oweEnding(ctx, run, e)
	return fmt.Errorf("%w: %w", errEndingOwed, cause)
}

// letGoAnswer lets go of the recorded answer a run whose ending is decided still
// owes the seat ([PendingStore.OweHandBack]): its copies are recorded on the row
// as owed, in the write that takes the answer off it, for the ending to hand
// back before it deletes the row. Returns the row as the let-go left it.
//
// SPENT BEFORE THE WRITE ([CoordinatorOptions.Spent]): the delivery that carried
// the reply may still come round — a node that recorded it and stopped before
// acknowledging it left it unacknowledged — and once a copy is owed it must not
// be a second message. Spent and not let go, the reply is still on the row,
// which the store will not delete with it, and the next reader lets it go; let
// go and not spent, the original and the copy would both reach the seat, and
// the next holder, finding the answer gone, would have nothing to spend.
//
// A ROW THAT ALREADY OWES COPIES — an earlier decline's, still unpublished — has
// them handed back first, which keeps what a row owes to one decline's worth
// ([PendingRun.HandBack]) and keeps the replies in the order they were let go
// of.
//
// THE DECISION SETTLED WHETHER THE REPLY GOES BACK: a turn cannot take the
// answer once the ending is decided, so one no turn took by then is let go of
// here, and one a turn took is the run's to keep — spent at the take — unless
// the ending says it went unused. A let-go the store does not make (another
// attempt at the same ending made it first) leaves the delete to decide, which
// refuses for whatever the row still holds.
func (c *Coordinator) letGoAnswer(ctx context.Context, run PendingRun) (PendingRun, error) {
	answer := *run.Answer
	c.spend(ctx, run.AgentHandle, answer)
	letGo := LetGo{Ending: run.Ending.ID, Answer: answer.EventIDs, HandBack: handBackOf(answer)}
	var handed []string
	for range casRetries {
		owing, won, err := c.pending.OweHandBack(ctx, run.TurnID, letGo)
		switch {
		case err == nil && won:
			return owing, nil
		case err == nil, errors.Is(err, ErrAnswerTaken):
			// NOT THIS ATTEMPT'S TO LET GO: the delete reads the row and
			// refuses for anything it still owes.
			run.Answer = nil
			return run, nil
		case !errors.Is(err, ErrHandBackOwed):
			return PendingRun{}, err
		}
		owed := handBackIDs(owing.HandBack)
		if slices.Equal(owed, handed) {
			return PendingRun{}, fmt.Errorf("sandbox: letting go of run %s's answer: the reply it "+
				"already owed its seat is handed back but could not be cleared from it", run.TurnID)
		}
		if err := c.deliverHandBack(ctx, owing); err != nil {
			return PendingRun{}, err
		}
		handed = owed
	}
	return PendingRun{}, fmt.Errorf("sandbox: letting go of run %s's answer: it kept owing its seat "+
		"an earlier reply", run.TurnID)
}

// handBackIDs are the ids of what a row owes the seat, in order.
func handBackIDs(owed []HandedBack) []string {
	ids := make([]string, 0, len(owed))
	for _, copied := range owed {
		ids = append(ids, copied.ID)
	}
	return ids
}

// finish ends a run the caller has read and decided is over: the ending is
// DECIDED on the run's row and finished ([Coordinator.endRecord]) — the reply
// it lets go of handed back, its box reclaimed, its note announced and its
// record deleted, the stop REPORTED where the ending was this call's.
//
// Reports the ending decided under this call's license, or nil where none is
// — which a caller reads for what the decision settled, such as whether a
// person's answer goes back to the seat ([RecordedEnding.Returned]); and
// whether the run's ending is THIS NODE'S — decided under this call, or kept
// for this node's retry ([Coordinator.oweEnding]) where the decision could not
// be confirmed, which decides it again on the same terms, its reclaim included.
// False is a run that is somebody else's: a newer lease owns it, or somebody
// else had already ended it — and a caller must then say nothing about what
// became of it, because whoever holds it will.
//
// # The box goes after the decision, never on the caller's snapshot
//
// The run the caller read can be stale by the time it is ended. A slow holder
// reads its own claim to settle it, its lease lapses meanwhile, and the seat's
// next holder fences the row to its own lease and REVIVES the claim — the run
// answered again, owed a resume into the very box the claim left paused. A
// kill made before the decision destroyed that box on the snapshot's word, and
// only then did the store refuse the ending under the newer lease: the revived
// run was resumed into a box that was gone, with its row still naming it. So
// the box is reclaimed by the attempt that FINISHES the ending
// ([Decision.Reclaim]), once the store's own fence check has said this call may
// end the run at all — the order [Coordinator.endClaim] already kept, and the
// one [PendingStore.Finish] still needs: the box goes before the record, since a
// record that outlives its box is reaped by the next recovery pass while a box
// that outlives its record is named by nothing. And it rides the recorded
// ending, so the seat's next holder finishing an ending this node decided
// reclaims the box too.
//
// A kill that fails does not keep the record, unlike in [Coordinator.RetireSeat]:
// a record left claimed or launching would park its seat on the next busy
// count for as long as this node keeps the seat. The failure is logged, and a
// remote box runs out its TTL.
//
// LICENSED FOR EVERY LIVE STATUS AND EVERY JOB, whatever e says: the caller read
// the run and decided it is over, and its record must not outlive it whatever
// the row has moved to since. The FENCE is the part of the license the store
// checks against the row as it stands, and it now guards the kill as well as the
// delete: a newer lease owns the run, and its box is that owner's. The narrow
// license belongs to the ending decided WITHOUT having read the row — see
// [Coordinator.endClaim]. What the widest license still refuses is a person's
// reply no turn took, which goes back to the seat before the row does
// ([ErrAnswerOwed]).
func (c *Coordinator) finish(ctx context.Context, run PendingRun, e ending) (*RecordedEnding, bool) {
	if outranked(run, e.fence) {
		// A newer lease owns the run, and the snapshot already says so;
		// the store would refuse the decision for the same reason.
		log.WarnContext(ctx, "sandbox_finish_outranked", "turn_id", run.TurnID,
			"owner_epoch", run.OwnerEpoch, "epoch", e.fence.Epoch)
		return nil, false
	}
	endCtx, cancel := detached(ctx)
	defer cancel()
	e.whileIn, e.launch, e.reclaim = Active, EveryLaunch, true
	decided, err := c.endRecord(endCtx, run, e)
	if err != nil {
		log.WarnContext(ctx, "sandbox_finish_kept", "turn_id", run.TurnID, "error", err.Error(),
			"detail", "the run's ending could not be finished yet; this node finishes it — its "+
				"box included, once the decision is confirmed — and the seat's next recovery "+
				"pass reaps the run if the seat moves first")
		return decided, true
	}
	if decided == nil {
		log.InfoContext(ctx, "sandbox_finish_declined", "turn_id", run.TurnID,
			"detail", "the run is no longer this node's to end — a newer lease owns it, or "+
				"somebody else ended it first — so its box and its record are left to them")
	}
	return decided, decided != nil
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
// run's tail and settling it. Nothing will ever pick it up — the at-most-once
// claim already flipped, so a redelivered completion is refused — and its box
// sits paused. A LAUNCHING row is the same fact one step earlier: that engine
// died between starting the job and writing the conversation a resume would
// re-enter, so there is nothing to resume into and never will be. Both are
// tails nobody drives, and both are reaped ([Coordinator.reapTail]) — except a
// claim of a person's answer that no turn took yet, which is not unresumable at
// all: it is REVIVED, the answer given back to the run and the run resumed with
// it, and reaped, its reply handed back to the seat, only past the revival's
// bounds ([Coordinator.revivable]).
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
	var tally recovery
	for _, run := range active {
		c.recoverRun(ctx, run, owner, epoch, &tally, true)
	}
	log.InfoContext(ctx, "sandbox_seat_recovered",
		"seat", handle, "epoch", epoch, "running", tally.running,
		"parked", tally.parked, "abandoned", tally.abandoned, "finished", tally.finished,
		"revived", tally.revived, "active", len(active))
	return nil
}

// recovery counts what a seat's recovery pass found, for its one log line:
// finished is the endings the last holder decided and this one finished, and
// revived the dead claims whose answer it gave back to the run it answers.
type recovery struct{ running, parked, abandoned, finished, revived int }

// reaped is what [Coordinator.reapTail] did with a tail the seat's last holder
// left.
type reaped int

const (
	// reapLeft: nothing — a newer lease owns the row, or it is gone.
	reapLeft reaped = iota
	// reapMoved: the tail was not abandoned after all — given back before
	// this node's fence landed — and is the row reported, as it stands.
	reapMoved
	// reapRevived: a dead claim's answer was given back to its run, which is
	// the answered row reported, owed its resume.
	reapRevived
	// reapEnded: the tail was ended, its ending this call's.
	reapEnded
	// reapOwed: a revival that did not land, kept for this node's retry with
	// the seat's mail held behind it ([Coordinator.oweRevival]).
	reapOwed
)

// recoverRun takes over one run of a seat this node has just acquired.
//
// again is whether a run that turns out to have moved under the reap's fence
// is taken over once more as what it moved to: the reap reads the row again
// after fencing it, and the one write a process that lost the seat can still
// have landed before the fence is the release that hands its claim back — a
// row that is then owed its resume, not reaped. Once, because the fence is in
// place by then and nothing else can move the row.
func (c *Coordinator) recoverRun(ctx context.Context, run PendingRun, owner string, epoch int64,
	tally *recovery, again bool,
) {
	if run.Ending != nil {
		// AN ENDING THE LAST HOLDER DECIDED AND DID NOT FINISH: it is this
		// holder's to finish, on the terms recorded — the reply it hands
		// back, its box, and the announcement it makes once under its own
		// identity — never a second ending of this holder's own.
		if decided, err := c.endRecord(ctx, run, ending{
			fence: Fence{Owner: owner, Epoch: epoch}, whileIn: Active, launch: EveryLaunch,
		}); err != nil {
			log.WarnContext(ctx, "sandbox_ending_kept", "turn_id", run.TurnID, "error", err.Error(),
				"detail", "the ending the seat's last holder decided could not be finished yet; "+
					"this node finishes it, and the seat's mail waits behind it")
		} else if decided != nil {
			tally.finished++
		}
		return
	}
	switch run.Status {
	case StatusLaunching, StatusResumed:
		moved, did := c.reapTail(ctx, run, owner, epoch)
		switch did {
		case reapRevived:
			// THE ANSWER IS THE RUN'S AGAIN, owed its resume: taken over
			// as the answered run it now is, exactly as an answer the last
			// holder recorded and never resumed with.
			tally.revived++
			c.recoverRun(ctx, *moved, owner, epoch, tally, false)
		case reapMoved:
			if again {
				c.recoverRun(ctx, *moved, owner, epoch, tally, false)
			}
		case reapEnded:
			tally.abandoned++
		case reapLeft, reapOwed:
			// Another lease's to reap, gone, or a revival this node
			// retries with the seat's mail held behind it.
		}
		return
	case StatusRunning:
		if _, err := c.pending.ClaimOwnership(ctx, run.TurnID, owner, epoch); err != nil {
			log.WarnContext(ctx, "sandbox_ownership_claim_failed",
				"turn_id", run.TurnID, "error", err.Error())
		}
		c.countRun(run.AgentHandle, run.Status)
		tally.running++
	case StatusAnswered:
		// AN ANSWER THE PREVIOUS OWNER RECORDED AND NEVER RESUMED
		// WITH: it stopped, or the seat moved, between the record and
		// the resume that settles it. The answer is on the row, so it
		// is this node's to drive — the person is not asked to send
		// it again — and the seat's mail waits behind it here exactly
		// as it did there. Scheduled rather than run, because this is
		// the seat's preparation and the mailbox is not open yet.
		if _, err := c.pending.ClaimOwnership(ctx, run.TurnID, owner, epoch); err != nil {
			log.WarnContext(ctx, "sandbox_ownership_claim_failed",
				"turn_id", run.TurnID, "error", err.Error())
		}
		c.countRun(run.AgentHandle, run.Status)
		c.recoverOwed(ctx, run)
		tally.parked++
	case StatusAwaiting, StatusReseed:
		// NOTHING IS DONE TO THE RUN — its answer is what moves it —
		// but the new owner has to know the question is open, or the
		// answer arrives at a seat this node believes has nothing
		// waiting and is run as an unrelated turn. The old owner's
		// count went with the old owner; this is where the new one
		// gets it.
		//
		// AND IT IS FENCED TO THIS NODE'S LEASE, like every other run
		// the seat holds: left at the old holder's, that holder — lost
		// the seat and not noticed yet — could still claim the answer
		// that arrives for it under a lease nothing outranked
		// ([PendingStore.ClaimForResume]), and a run parked under no
		// lease stayed at the zero epoch across every seat move.
		if _, err := c.pending.ClaimOwnership(ctx, run.TurnID, owner, epoch); err != nil {
			log.WarnContext(ctx, "sandbox_ownership_claim_failed",
				"turn_id", run.TurnID, "error", err.Error())
		}
		c.countRun(run.AgentHandle, run.Status)
		tally.parked++
	}
	// AND A REPLY A DECLINE OWES THE SEAT, whatever the run has done
	// since: the decline's write landed and the node that wrote it
	// stopped before its copies were published (or before it could
	// say they were). Owed to the seat rather than to the run, so it
	// is this holder's to publish — before the mailbox opens, so the
	// copies are waiting with the rest of the seat's mail when it
	// does. A run reaped above publishes what it still carries before
	// its record goes ([Coordinator.endRecord]); an answered run's owed
	// resume publishes it first ([Coordinator.retryOwed]).
	if len(run.HandBack) > 0 &&
		slices.Contains([]string{StatusRunning, StatusAwaiting, StatusReseed}, run.Status) {
		c.recoverOwed(ctx, run)
	}
}

// reapTail ends a tail the seat's last holder abandoned — a launching row, or a
// claim — and announces it; or, for a claim whose node stopped before its turn
// took the person's answer it was claimed for, REVIVES it — gives the answer
// back to the run — and reports the row as it now stands, to be taken over as
// the answered run it is. It reports what it did ([reaped]) and, where the
// claim was revived or turned out not to be abandoned after all — given back
// before this node's fence landed — the row as it now stands.
//
// # A person's answer the claim died holding: revived, not handed back
//
// A claim taken for a RECORDED ANSWER — a chat reply or an answer by turn —
// carries the answer on its row, and the delivery that brought it was spent
// when it was recorded: nothing but this row will ever bring it to the run it
// answered. Whether a turn took it is the one thing the row has to say, and it
// says it ([RecordedAnswer.TakenAt]). A claim whose process stopped before its
// turn took the answer never used it, and the answer is still the run's: the
// run has its suspended conversation and the answer on its row, and resuming
// it with that answer is exactly what the dead claim was doing. So the claim is
// given back to the answer ([PendingStore.ReviveAnswer]) — the run answered
// again, fenced to this node's lease in the same write — and this node drives
// its resume as it drives any answer its last holder recorded and never resumed
// with ([Coordinator.recoverOwed]). The box needs nothing of its own: the claim
// never reached the turn, so it never touched the box, which is held exactly as
// an answered run's is — reclaimed by the pause reaper past its pause TTL, and
// the run then re-seeded from its branch, as it would have been had the claim
// never been taken. It used to be reaped, the run ended and the answer handed to
// the seat as an ordinary message: the person's answer reached the seat, but
// never the run it answered, which was lost.
//
// ONLY WHILE THE RUN CAN STILL BE RESUMED WITH IT — see [Coordinator.revivable]
// for the two bounds — and handed back past them: the run is reaped, its
// answer let go back to the seat as the ordinary message it is, and the
// announcement says why. A turn that took the answer and died mid-round has
// used it, and a copy handed back now would answer the person twice: the run is
// reaped as spent, and the answer's delivery recorded as worked here too, for a
// node that stopped between its take and the spend that goes with it
// ([CoordinatorOptions.Spent]).
//
// THE TAKE AND THE REVIVAL ARE EXCLUSIVE IN THE STORE, as the take and an
// ending's let-go are, and that — not this reap's reading of the row — is what
// decides between them: a revival refuses an answer a turn took
// ([ErrAnswerTaken]), and the reap then ends the run as spent; a take after the
// revival finds no claim to take it under. So a node still on its way to its
// turn when the reap revives the answer cannot also answer the person with it.
//
// A REVIVAL THE STORE DOES NOT CONFIRM IS RETRIED, never read as a reason to
// end the run: a write that failed, or a claim that moved under it and could
// not be read again, says nothing about whether the run can be resumed. The
// claim stays on the row, fenced to this node where the fence landed, and this
// node tries the revival again on the hand-back's spacing with the seat's mail
// held behind it
// ([Coordinator.oweRevival]) — finding it landed after all, or the claim given
// back, it goes on as the answered run it is — and if the seat moves first its
// next holder reaps the claim afresh.
//
// FENCED FIRST, and READ AGAIN. The process that took the claim lost the seat,
// but may not have noticed: it can still give the claim back, or reach its
// turn. So the reap moves the row to this node's lease
// ([PendingStore.ClaimOwnership]) — a take or a release under the old lease is
// then refused ([PendingStore.TakeAnswer], [PendingStore.ReleaseClaim]) — and
// reads it again: a release that landed first left a run owed its retry, which
// is taken over as what it is rather than reaped. That holds for every claim,
// a completion's included, because a released claim is a live run whatever
// signal it was taken for. A row this node can neither fence nor read again is
// reaped as listed — and revived as listed, the revival being a compare-and-set
// that stamps this node's lease itself: the store decides from what it holds,
// so an answer on it no turn took still reaches the run, or the seat.
func (c *Coordinator) reapTail(ctx context.Context, run PendingRun, owner string, epoch int64,
) (*PendingRun, reaped) {
	fence := Fence{Owner: owner, Epoch: epoch}
	if run.Status == StatusResumed {
		fenced, still, decided := c.fenceClaim(ctx, run, owner, epoch)
		if !decided {
			return nil, reapLeft
		}
		if still == nil {
			return &fenced, reapMoved
		}
		run = *still
	}
	detail := "the node that owned this seat stopped mid-run, so its turn cannot be continued by the " +
		"seat's new owner"
	if run.Status == StatusResumed && run.Answer != nil && !run.Answer.Taken() {
		why := c.revivable(run)
		if why == "" {
			answered, took, err := c.reviveAnswer(ctx, run, fence)
			switch {
			case err == nil && answered != nil:
				return answered, reapRevived
			case err == nil:
				// THE CLAIM MOVED between the read and the revival — given
				// back, or ended — and is taken over as what it is now.
				return c.rereadClaim(ctx, run, fence)
			case took != nil:
				// A TURN TOOK THE ANSWER FIRST: the reply has been used, and
				// the run is reaped as spent.
				run = *took
			default:
				c.oweRevival(ctx, run, fence, err)
				return nil, reapOwed
			}
		} else {
			log.WarnContext(ctx, "sandbox_answer_not_revived",
				"turn_id", run.TurnID, "agent", run.AgentHandle, "reason", why,
				"lost_claims", run.Answer.LostClaims)
			detail = "the node holding this seat stopped while resuming the run with a person's " +
				"answer, and " + why + ", so the run is not resumed again"
		}
	}
	taken := run.Answer != nil && run.Answer.Taken()
	if taken {
		// THE TURN TOOK THE REPLY AND DIED WITH IT: spent, so the
		// delivery that carried it — unacknowledged, if the node took it
		// inline — is dropped when it comes round to this holder.
		c.spend(ctx, run.AgentHandle, *run.Answer)
	}
	// Fenced on the lease this node just took, so a record a newer owner has
	// already claimed is left to that owner, and neither ended nor announced
	// here. ANNOUNCED like the other ways a run is lost, by the ending itself:
	// the seat's new owner is about to open its mailbox, and a turn that died
	// with the previous owner has to be visible rather than inferred from a
	// record that quietly left the board.
	decided, ours := c.finish(ctx, run, ending{
		fence: fence,
		note:  &failureNote{reason: types.SandboxFailureAbandoned, detail: detail},
	})
	if !ours {
		return nil, reapLeft
	}
	// LOGGED ONCE THE ENDING IS DECIDED, because what an operator searches
	// this line for — did the person's answer go back to the seat? — is the
	// decision's to settle ([RecordedEnding.Returned]), and a line written
	// before it could only say what the row held. answer_held and
	// answer_taken are that: an answer held and not taken is one the ending
	// returns, but a taken one is held too, and a held one an earlier decline
	// already owes goes back as well. answer_handed_back is the one field to
	// search; ending_decided false is a decision the store did not confirm,
	// whose retry decides it and whose announcement says whether it went back.
	log.WarnContext(ctx, "sandbox_abandoned_tail_reaped",
		"turn_id", run.TurnID, "agent", run.AgentHandle,
		"sandbox_id", run.SandboxID, "status", run.Status,
		"answer_held", run.Answer != nil, "answer_taken", taken,
		"answer_handed_back", decided != nil && decided.Returned,
		"ending_decided", decided != nil)
	return nil, reapEnded
}

// MaxAnswerRevivals is how many claims of one recorded answer may die before
// their turn took it and still be given back to the run it answers
// ([Coordinator.reapTail]); the next is reaped, and the answer handed back to
// the seat.
//
// A CLAIM THAT DIES IS A NODE THAT STOPPED between a claim and the turn it
// drives — a deploy, a crash, a lease that moved — and that window is the
// fraction of a second a resume takes to reach its turn, so one such loss is
// bad luck and a second in a row is rare. Three in a row is not luck: it is a
// resume that takes its node down before its turn begins — a defect the
// panic guard cannot catch, an allocation that kills the process — and each
// revival hands it to the next holder to fall over the same way, holding the
// seat's mail behind it while it does. Three bounds that loop to three seat
// handoffs, a few minutes at the seat lease's 45 s TTL, while leaving a run
// that merely met two deploys in a row resumed rather than abandoned. It is
// counted on the row ([RecordedAnswer.LostClaims]) because the count each node
// keeps of its own attempts ([MaxAnswerAttempts]) resets with the very event
// this counts.
const MaxAnswerRevivals = 3

// revivable reports why a dead claim's recorded answer is NOT given back to the
// run it answers, or "" where it is.
//
// TWO BOUNDS, the same two every series of attempts at a recorded answer's
// resume is held to, kept on the row because a node that stops resets every
// count it kept: how many claims of the answer have died
// ([MaxAnswerRevivals]), and how long since the first of them did, against the
// run's own awaiting window ([answerWindow], `pause_ttl_seconds`) — the
// tolerance the run itself declared for a reply, past which a resume that keeps
// dying with its node is not a transient worth waiting out.
//
// NOT THE BOX. The claim never reached its turn, so it never touched the box,
// and a run whose box is gone — reaped past its pause TTL, or never held, under
// a zero one — re-seeds from its pushed branch when it resumes, as any answered
// run does; reviving a run with no box, or no branch, resumes the same turn
// with the same answer the person gave, which is what the person asked for. NOR
// THE CONVERSATION: a claim is only ever taken on a run that has one, and the
// resume's own refusal of one that does not ([types.SandboxFailureNoConversation])
// hands the answer back through the run's ending in any case.
func (c *Coordinator) revivable(run PendingRun) string {
	answer := run.Answer
	if answer.LostClaims >= MaxAnswerRevivals {
		return fmt.Sprintf("%d claims of this answer have now died before their turn took it",
			answer.LostClaims+1)
	}
	if window := answerWindow(run); window > 0 && !answer.FirstLostAt.IsZero() &&
		c.now().Sub(answer.FirstLostAt) >= window {
		return fmt.Sprintf("the answer has been owed its resume for %s since the first claim of it "+
			"died, past the run's pause_ttl_seconds", c.now().Sub(answer.FirstLostAt).Round(time.Second))
	}
	return ""
}

// reviveAnswer gives a dead claim back to its recorded answer under this node's
// lease ([PendingStore.ReviveAnswer]). It reports the run as revived; or, where a
// turn took the answer first, the row as it stands, to be reaped as spent; or
// neither, for a claim that moved under it; or the store's failure.
func (c *Coordinator) reviveAnswer(ctx context.Context, run PendingRun, fence Fence,
) (revived, took *PendingRun, err error) {
	written, won, err := c.pending.ReviveAnswer(ctx, run.TurnID, Revival{
		Launch: run.LaunchID, Answer: run.Answer.EventIDs, Fence: fence,
	})
	switch {
	case errors.Is(err, ErrAnswerTaken):
		return nil, &written, err
	case err != nil:
		return nil, nil, err
	case !won:
		return nil, nil, nil
	}
	log.InfoContext(ctx, "sandbox_answer_revived",
		"turn_id", written.TurnID, "agent", written.AgentHandle,
		"lost_claims", written.Answer.LostClaims, "first_lost_at", written.Answer.FirstLostAt,
		"detail", "the node holding this seat stopped before the resumed turn took the person's "+
			"answer; the answer is given back to the run, and this node resumes it")
	return &written, nil, nil
}

// rereadClaim is a dead claim's row read again after its revival was refused —
// it moved under the reap — and reported as what it is now: given back, or
// ended, to be taken over as that; still the claim, or unreadable, for the
// revival to be tried again ([Coordinator.oweRevival]); or nothing, where a
// newer lease owns it or it is gone.
func (c *Coordinator) rereadClaim(ctx context.Context, run PendingRun, fence Fence) (*PendingRun, reaped) {
	latest, found, err := c.pending.Get(ctx, run.TurnID)
	switch {
	case err != nil:
		c.oweRevival(ctx, run, fence, fmt.Errorf("sandbox: reading run %s again after its "+
			"revival was refused: %w", run.TurnID, err))
		return nil, reapOwed
	case !found, outranked(latest, fence):
		return nil, reapLeft
	case latest.Status == StatusResumed && latest.Ending == nil:
		c.oweRevival(ctx, latest, fence, fmt.Errorf("sandbox: run %s's claim changed under its "+
			"revival", run.TurnID))
		return nil, reapOwed
	}
	return &latest, reapMoved
}

// fenceClaim moves a claimed row to this node's lease and reads it again, for
// [Coordinator.reapTail]. It reports the row to reap (still, nil where the row
// is no longer a claim to reap, and fenced is then the row as it stands), and
// whether the reap should go on at all — false for a row a newer lease owns, or
// one that is gone.
//
// A FENCE OR A READ THAT FAILS LEAVES THE ROW AS LISTED, and the reap goes on:
// the ending's store reads decide what it holds when it is asked, so a reply
// on it no turn took is handed back however the reap got there.
func (c *Coordinator) fenceClaim(ctx context.Context, run PendingRun, owner string, epoch int64,
) (fenced PendingRun, still *PendingRun, decided bool) {
	won, err := c.pending.ClaimOwnership(ctx, run.TurnID, owner, epoch)
	if err != nil {
		log.WarnContext(ctx, "sandbox_ownership_claim_failed",
			"turn_id", run.TurnID, "error", err.Error(),
			"detail", "the claim could not be fenced before its reap; it is reaped as listed, "+
				"and a reply on it no turn took is handed back")
		return PendingRun{}, &run, true
	}
	if !won {
		// A NEWER LEASE than this node's owns the run: its reap, not ours.
		return PendingRun{}, nil, false
	}
	latest, found, err := c.pending.Get(ctx, run.TurnID)
	switch {
	case err != nil:
		log.WarnContext(ctx, "sandbox_reap_read_failed",
			"turn_id", run.TurnID, "error", err.Error(),
			"detail", "the fenced claim could not be read again; it is reaped as listed, and "+
				"a reply on it no turn took is handed back")
		run.Owner, run.OwnerEpoch = owner, epoch
		return PendingRun{}, &run, true
	case !found:
		return PendingRun{}, nil, false
	case latest.Status != StatusResumed && latest.Status != StatusLaunching:
		return latest, nil, true
	}
	return PendingRun{}, &latest, true
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
		//
		// AND THE SAME ANNOUNCEMENT, through the ending's own note: a
		// person's reply the run still holds goes back to the seat's
		// inbox before the record does, like every ending's, and the
		// announcement says so where it did.
		decided, err := c.endRecord(ctx, run, ending{
			fence: fence, whileIn: Active, launch: EveryLaunch,
			note: &failureNote{reason: types.SandboxFailureSeatRemoved,
				detail: "the seat was removed from the company and not restored within the " +
					"retirement grace, so its run was ended; any work it pushed is on its branch"},
		})
		if err != nil {
			errs = append(errs, fmt.Errorf("sandbox: finishing run %s of retired seat %q: %w",
				run.TurnID, handle, err))
			continue
		}
		if decided == nil {
			// Settled by somebody else since the listing, who announced
			// it if it was lost.
			continue
		}
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
	// AND ANY ANSWER IT WAS STILL RESUMING: it stays recorded on the run, and
	// the successor's recovery pass drives it.
	c.releaseOwed(handle)
	c.mu.Lock()
	delete(c.runs, handle)
	c.mu.Unlock()
	// AND THE SEAT'S INBOX HOLD, which the release's detach has already
	// dropped with the attachment: forgotten, never lifted — the seat's mail
	// is the successor's to order now. See [Coordinator.forgetHold].
	c.forgetHold(handle)
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
//
// BY names who answered where the route knows it — an answer by turn carries
// the person, while a chat reply's sender is already in the conversation the
// resumed turn reports back to — and "" leaves the answer unattributed.
func answerText(run PendingRun, answer, by string) string {
	lines := []string{
		"The sandbox coding run paused to ask a person a question before it could finish.",
	}
	if run.Question != "" {
		lines = append(lines, "\nQuestion it asked: "+run.Question)
	}
	if by != "" {
		lines = append(lines, "Answer from "+by+": "+answer)
	} else {
		lines = append(lines, "Their answer: "+answer)
	}
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
