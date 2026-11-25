package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/executorkeep"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/provenance"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/redact"
)

// ErrResumeUnavailable reports that this node cannot resume the run a
// completion refers to: a failure of routing, not of the run. The caller hands
// the completion back to reach the seat's node rather than settling the run
// done, which would tear the box down and discard the turn.
// [ErrResumeElsewhere] is the one another node or build can get past. See the
// package doc, "Resume routing".
var ErrResumeUnavailable = errors.New("sandbox: this node cannot resume the run")

// ErrResumeElsewhere is an [ErrResumeUnavailable] another node or build can get
// past: a seat this node does not hold, a parked state of a version or a reply
// kind this build does not read, a seat or company not this node's. An error
// wrapping it matches both. It is the node's failure on the seat's control
// topic, neither the run's nor the message's, and counts toward the resume
// give-up only once no live node may resume the run
// ([CoordinatorOptions.ResumableElsewhere]). See the package doc, "Resume
// routing".
var ErrResumeElsewhere = fmt.Errorf("%w; another node or build may", ErrResumeUnavailable)

// ErrResumeAbandoned reports a resumed turn that broke and must not be resumed
// again from the same completion.
//
// The counterpart of the dispatcher's abandon decision: a redelivery re-runs
// every call the resumed segment made, and a coding run most likely pushed
// already. A PANICKED turn or a resume REFUSED for a reason every retry meets
// (turn.ErrPermanent) is abandoned too. A resumer wraps its error in this only
// when the turn must not run again: a turn.Abandon, a panic outside the turn
// loop, a person's stop, a failed relaunch, an [Unresumable]. The coordinator
// never reverts the claim: it settles the delivery and run, reclaiming the box.
// Every other failure reverts and comes back.
var ErrResumeAbandoned = errors.New("sandbox: the resumed turn broke and must not be resumed again")

// Unresumable is a run no build will ever resume, as the engine judges it. It
// matches [ErrResumeAbandoned], so a resume that returns it ends the run before
// its turn begins, announced under Reason, with a person's reply handed back.
type Unresumable struct{ Reason, Detail string }

func (u *Unresumable) Error() string {
	return fmt.Sprintf("sandbox: no build will resume this run (%s): %s", u.Reason, u.Detail)
}

// Is matches [ErrResumeAbandoned]: no build will resume the run, so it ends.
func (u *Unresumable) Is(target error) bool { return target == ErrResumeAbandoned }

// note is what the run's ending announces.
func (u *Unresumable) note() *failureNote { return &failureNote{reason: u.Reason, detail: u.Detail} }

// abandonedNote is what a resume abandoned before its turn began announces.
func abandonedNote(err error) *failureNote {
	if u, ok := errors.AsType[*Unresumable](err); ok {
		return u.note()
	}
	return &failureNote{reason: types.ExecutorFailureResumeBroken, detail: resumeBrokenDetail}
}

// ErrResumeHeld reports a resumed turn its seat's conditions refused — the
// token budget its review or its relaunch was refused under, today — returned
// wrapped in a [HeldError] that says what the refusal waits for.
//
// NOT A FAILURE, AND NOT A RETRY. The coordinator hands the claim back holding
// the resume until the refusal lifts ([PendingRun.HeldFor]) and spends the
// completion that drove it: the job stays collected in its paused box, the seat
// stays held, and the poll announces the job again once the hold lifts. An
// ordinary retry would re-collect the job and re-run its review into the same
// refusal.
var ErrResumeHeld = errors.New("sandbox: the resume waits for its seat's conditions")

// HeldError is a resume refused by its seat's conditions — see [ErrResumeHeld]
// — and the [Refusal] it waits on.
type HeldError struct {
	Refusal Refusal
	Err     error
}

func (e *HeldError) Error() string {
	return "sandbox: the resume is held (" + e.Refusal.Reason + "): " + e.Err.Error()
}

// Unwrap reports the refusal as both [ErrResumeHeld] and its own cause.
func (e *HeldError) Unwrap() []error { return []error{ErrResumeHeld, e.Err} }

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

// ErrEndOwed is a store's refusal to delete a run whose row still owes the
// executor log its turn's end ([PendingRun.OwedEnd]): the row is the end's only
// record until the log takes it. The answer is to record it
// ([Coordinator.publishEnd]), clear it ([PendingStore.ClearEnd]) and ask again.
var ErrEndOwed = errors.New("sandbox: the run still owes the executor log its turn's end")

// ErrAnswerTaken is a store's refusal to let go of a recorded answer a resumed
// turn already took ([PendingStore.OweHandBack]): the reply has been used, and
// a copy handed back now would answer the person twice.
var ErrAnswerTaken = errors.New("sandbox: a resumed turn already took the reply")

// ErrSeatNotHeld is a write on behalf of a seat this node does not hold: the
// lease the coordinator asks ([CoordinatorOptions.Lease]) says the seat is not
// held here, or the store finds a newer lease than the writer's own on the run
// ([PendingStore.RecordAnswer]). Either way the write is the seat's holder's to
// make, and the delivery that asked for it goes back to reach that holder.
//
// NOT THE ZERO FENCE: zero means UNFENCED (a node with no seat host), which
// constrains nothing and would let a node that lost the seat record, claim and
// take an answer past its successor's fence.
var ErrSeatNotHeld = errors.New("sandbox: this node does not hold the seat")

// ErrRunEnding is a store's refusal of a write to a run whose ending is decided
// ([PendingRun.Ending]): from then on the row takes only the ending's own steps
// (no claim, take, release, relaunch, new answer or park), so the ending can be
// announced before its record is deleted ([RecordedEnding]). It keeps the take
// and the let-go exclusive: a take that lands first is seen by the decision, a
// later one is refused.
var ErrRunEnding = errors.New("sandbox: the run's ending is decided, so it takes no other write")

// Resumer re-enters a turn parked on its executor's run: the one seam between
// the sandbox and agent layers, handed the run's opaque state and the answer
// text, so this package imports nothing of the agent. It returns an error
// wrapping [ErrResumeAbandoned] when a retry must not repeat the break,
// [ErrResumeUnavailable] when this node cannot resume the run, and
// [ErrResumeElsewhere] where another node or build can. See the package doc,
// "Resume routing".
type Resumer interface {
	Resume(ctx context.Context, req ResumeRequest) error
}

// ResumeRequest is one re-entry into a suspended turn.
type ResumeRequest struct {
	Run PendingRun

	// Bridged is the run's bridged tool log as its job left it
	// ([PendingStore.BridgeLog]), plus the call a person approved that the log
	// is still owed ([BridgeLog.withHeld]): the calls the resumed phase is
	// judged on. Read once the job is collected, beside the row.
	Bridged BridgeLog

	// BridgedRead is the read Bridged was made from ([BridgeLog.Read]), taken
	// BEFORE the held call is added: that call is numbered after any call that
	// landed in between, so a count including it would leave the first of those
	// unmarked. Every relaunch hands it to its launch ([LaunchRequest.BriefRead])
	// to mark late the calls no brief told, or carries it into a new round
	// ([PendingRun.BriefRead]). For a log on another job it is the empty read
	// the store answers ([PendingStore.BridgeLog]), never applied.
	BridgedRead LogRead

	// Leads is the turn's leads kept beside the run's row ([PendingStore.Leads]):
	// oldest first, plus how many earlier ones are not kept. A part the store
	// could not read is said unread, never "no leads" ([Coordinator.leadsAt],
	// [TurnLeads.Unread]).
	Leads TurnLeads

	// Answer is what the resumed turn files as its executor's report: the
	// collected run's account of itself ([runReport]), already redacted because
	// it is published on the phase record. Empty on a person's answer
	// ([ResumeRequest.Asked]).
	Answer string

	// Asked is set when a PERSON'S ANSWER drives the resume of a run that
	// parked on a question: what was asked, the reply, and what the run left
	// behind. Nil for a completion.
	//
	// THE TURN CARRIES THE RUN ON, and never files the answer as the run's
	// report: the round is not over, so the turn relaunches its executor
	// (continuing the harness's session in the box the run asked from, else a
	// fresh run told the question, answer and branch; [Continuation]) with the
	// answer as its next prompt. Filed as the report, the run was rescued as
	// incomplete and never saw the reply.
	Asked *AskedRun

	// Lost is set when the job a completion announced was LOST with its host
	// rather than ended ([PendingRun.Lost]). Nil for every other resume.
	//
	// THE TURN CARRIES THE ROUND ON, as for a question: what the cut-off job
	// left is no account of the round, so the turn relaunches for the same round
	// (in the lost job's box where this node may continue in it, else a fresh
	// one), told where to look. Except where the round's calls already hold its
	// submission: then the round is collected and Answer is the job's account,
	// marked cut off. Past [LostRun.Limit] losses the round ends, saying why.
	Lost *LostRun

	// Success is whether the launch itself succeeded, for the resumed
	// phase's own record.
	Success bool

	// Trigger is the event that caused the resume, carried so the resumed
	// turn's telemetry names what woke it.
	Trigger *events.Event

	// DeliveredRefs are what the run reported producing, for the resumed phase's
	// event to display; nothing judges delivery on them ([Result.DeliveredRefs]).
	// Empty on a person's answer: no new run finished.
	//
	// The run's COST is deliberately not here: it rides each launch's own record
	// (`executor_collected`, [Coordinator.publishCollected]), which an answer's
	// resume, carrying no run, could not report.
	DeliveredRefs []string

	// DeliveredRefsElided is how many more refs the run reported than
	// DeliveredRefs lists ([Result.DeliveredRefsElided]).
	DeliveredRefsElided int

	// Pushed, PushedElided and PushedUnread are what the push record says the
	// run moved on Gitlet ([Result.Pushed]). Empty, like the refs, on a person's
	// answer.
	Pushed       []PushedRef
	PushedElided int
	PushedUnread string

	// InputTokens and OutputTokens are what the collected job cost, charged to
	// the turn's work item by the resumed segment (ADR-0022).
	//
	// UNLIKE the refs, a person's answer carries them too: the tokens of the job
	// that asked, recorded when it parked ([PendingRun.ParkedInputTokens]). The
	// answer's resume is the ONLY segment that job gets, so it pays.
	InputTokens  int
	OutputTokens int

	// CacheReadTokens and CacheWriteTokens are the share of InputTokens the
	// prompt cache served and stored ([Result.CacheReadTokens]), a breakdown and
	// never an addition, carried the same two ways.
	CacheReadTokens  int
	CacheWriteTokens int

	// Begin is what the resumer calls ONCE, when the segment is certain to run
	// and before anything of it does, after every check that can still send the
	// resume back as a retry. An error means it must not run. Nil commits nothing.
	//
	// IT IS WHERE A RESUME BECOMES A TURN. For a recorded answer it writes that
	// the turn took it ([PendingStore.TakeAnswer]): a claim stopping before this
	// still owes the reply to the seat, one after has used it. It records the
	// delivery as worked ([CoordinatorOptions.Spent]) and tells a resume that
	// broke before its turn from one that broke running it
	// ([ErrResumeAbandoned]). The round's carry is recorded on a collected job's
	// claim ([LaunchRecord.ResumeCarry]), best effort.
	Begin func(ctx context.Context, carry ResumeCarry) error

	// Engine is what the ENGINE spent on the job outside every segment of the
	// turn (its bridged calls, [LaunchRecord.Bridged], and the condensation,
	// [LaunchRecord.Condensed]), charged here because no segment tallied it.
	//
	// A COLLECTION RETRIED after a failed resume condenses again and only the
	// attempt the landing resume runs from reaches the item, so the item errs
	// short of the turn, never past it.
	Engine EngineSpend

	// Ended is how the harness said the run ended (answer, error, success)
	// VERBATIM, beside Answer's account. A spent plan or rate limit is in there
	// and only the resuming node can act on it: it holds the entry the run
	// launched on, and its next launch (a reviewer's send-back) must not walk
	// into the same limit. Zero on a person's answer.
	Ended RunEnding
}

// RunEnding is how a collected run said it ended — see [ResumeRequest.Ended].
type RunEnding struct {
	Text  string
	Error string
	// Event is the harness's own terminal event — see [Result.Event].
	Event string
	// Limits is the run's last limit report — see [Result.Limits].
	Limits string
	// NoLimitExit says the run's exit names no limit — see [Result.NoLimitExit].
	NoLimitExit bool
	Success     bool
	// Refused says the provider refused the request its harness made as built
	// ([requestRefused]): the resume carries its round on once in a fresh
	// session, or ends the turn on the provider's message.
	Refused bool
	// At is when the ending was FIRST read back from its box
	// ([LaunchRecord.CollectedAt]): the instant a vendor reset named as a time of
	// day ("resets 4:30pm") is the next occurrence after. The first, because a
	// resume held for hours reads the same ending, and against the later instant
	// a 4:30pm already past would be read as the next day's.
	At time.Time
}

// Accountant post-charges a collected run's tokens.
//
// The charge comes AFTER the spend, so it stops nothing, and is RECORDED
// whatever the caps say: a dropped spend would under-state the company when its
// cap bound. The boolean says the run took a counter PAST its cap; both counters
// hold it either way. companyCharged says an earlier attempt charged the
// COMPANY's counter and failed on the seat's, so this records the seat's share
// alone; a [coord.SeatUncountedError] is that partial, reached now. Any other
// error records nothing the caller can count on.
type Accountant interface {
	Charge(ctx context.Context, agentID, handle string, tokens int, companyCharged bool) (refused bool, err error)
}

// AudienceResolver resolves the audience a harness named for its question
// — "requester", "manager", "team", or a name it typed — to the seats it means,
// for the run that asked.
//
// Implemented by the engine, which holds the chart; this package knows the label
// and the run, not who leads whom. A PURE ANSWER with no error: a walk over a
// chart in memory, and a label resolving to nobody is part of the answer
// ([Audience.Fallback]), never a failure of the park.
type AudienceResolver interface {
	ResolveAudience(run PendingRun, label string) Audience

	// MayAnswer reports whether whoever holds seat may answer a parked question
	// NOW, read when the reply arrives and never at the park: an agent seat the
	// chart holds, or a human seat whose person may act. A suspended or removed
	// person, or an unheld human seat, answers nothing and the reply runs as an
	// ordinary message. An error is UNKNOWN, which keeps the reply's place.
	MayAnswer(ctx context.Context, seat string) (bool, error)
}

// CoordinatorOptions configures a [Coordinator].
type CoordinatorOptions struct {
	Queue   Publisher
	Pending PendingStore
	Manager *Manager

	// FailureKind is the classifier's word for a failed resume, carried by the
	// run's lost announcement. It sits above this package; nil leaves it unsaid.
	FailureKind func(error) string

	// NodesFailure reports whether a resume's failure is the node's or its
	// infrastructure's rather than the run's (a coordination store, state log,
	// store or disk that did not answer, a dead queue, this node's stop). It is
	// handed back like any other but not counted toward the give-up that ends
	// the run ([Coordinator.resumeGivenUp]): an outage says nothing about whether
	// the turn can be re-entered. Classified above this package (internal/engine's
	// nodesFailure); nil counts every failure as the run's.
	NodesFailure func(context.Context, error) bool

	// ResumableElsewhere reports whether a live node other than this one may
	// resume run: one whose build reads the run's parked-state version, or, for
	// a reply kind this build does not read, one on another build. A failure on
	// [ErrResumeElsewhere] is not counted toward the give-up while one may, nor
	// while the answer cannot be read; once none may, it is, so a rollback cannot
	// hold the seat for ever ([Coordinator.runsOwnFailure]). Read above this
	// package (internal/engine's resumableElsewhere); nil counts every such
	// failure as the run's.
	ResumableElsewhere func(ctx context.Context, run PendingRun) (bool, error)

	// OperatorsCondition reports the kind of a resume failure only an operator
	// clears (a disk full, read-only or failing, open files at their limit, nodes
	// holding different keys or builds), false for any other. The give-up counts
	// no such failure, so the run waits for as long as the condition lasts and is
	// counted in [Coordinator.ResumesBlocked], from which data nodes raise the
	// executor_resume_blocked alarm. Classified above this package
	// (internal/agent/builtin's OperatorKind); nil says none.
	OperatorsCondition func(error) (string, bool)

	// Resume re-enters a suspended turn. Required: [NewCoordinator] refuses a
	// coordinator without it.
	//
	// "This node cannot resume this run" is the resumer's answer to give, by
	// wrapping [ErrResumeUnavailable]: only the engine knows which seats it holds
	// and which conversation formats it reads. It gives the claim back so the
	// node that holds the seat can win it.
	Resume Resumer

	// Account post-charges collected tokens. Nil skips accounting.
	Account Accountant

	// Audience resolves who a parked question may be answered by, at the park.
	// Required, like Resume: only the engine holds the chart, and without it
	// every question would be recorded as waiting on nobody
	// ([PendingRun.AudienceHandles]).
	Audience AudienceResolver

	// Answers seals a person's answer onto the run and opens it before a resume
	// reads it ([NewAnswerSeal], over the fleet keyring). Required: the words
	// live on the row only inside the seal. See answerseal.go.
	Answers *AnswerSeal

	// Leads seals the leads a launch's brief opened with into the run's leads,
	// for every later brief to a session that may lack that brief
	// ([NewLeadSeal], over the fleet keyring; leads.go). Nil keeps no lead (a
	// test's coordinator).
	Leads *LeadSeal

	// Ended is called for every run this node finishes with, however, and before
	// every collection's wait for calls in flight (drain.go), so more than once
	// per run: it is idempotent. It exists for credentials a run HOLDS: a bridge
	// token carries no expiry, so an open session would let a box that outlived
	// its run keep a working key to a seat's surface (internal/api/mcpbridge). A
	// parked run's session ends at its park's collection ([Coordinator.drainCalls]);
	// waiting notes stay on the record ([CoordinatorOptions.SteersEnded]). IT
	// WAITS FOR NO CALL STILL RUNNING (its update may land later,
	// [BridgeLog.InFlight]): collections wait for those on the run's log, never
	// the bridge (drain.go).
	Ended func(runID string)

	// Recovered is told of each run this node takes over still RUNNING in its
	// box, as it recovers the seat ([Coordinator.RecoverSeat]): before the seat's
	// mailbox opens, once the run is fenced to this lease or could not be written
	// either way. A run a newer lease owns is not reported, nor a parked run (its
	// box calls nothing) or a reaped tail. Like Ended, it exists for what a box
	// REACHES: its bridge session lived in the launching process, so a node that
	// restarted or took the seat over serves it again (internal/api/mcpbridge).
	// Nil serves nothing again.
	Recovered func(ctx context.Context, run PendingRun)

	// Unresumable judges each run parked on a question as its seat is recovered:
	// non-nil is a run no build will ever resume, ended there rather than left
	// holding its seat for an answer it cannot take. Nil judges none.
	Unresumable func(ctx context.Context, run PendingRun) (*Unresumable, error)

	// Stopped is called once for every way a run's engine-side life ENDS, and for
	// the one way it stops without ending, a park: nothing is working behind this
	// seat and turn any more. A turn suspended into the run returned its frame,
	// so only this call lets the working indicator drop. It fires on ordinary
	// endings too: a gate on "had a suspended turn" is a second opinion. Reported
	// WHERE THE TRANSITION IS MADE, NOT WHERE A LIST SAYS (enumerated stops missed
	// one each time): every ending goes through [Coordinator.endRecord],
	// [Coordinator.park] reports itself, each only where the transition was THIS
	// call's. Over-reporting is harmless, under-reporting the defect. After the
	// durable write (a retry could still resume the turn and find its hold gone),
	// before the announcement (a publish can block). NOT Ended, keyed on a credential.
	Stopped func(ctx context.Context, handle, turnID string)

	// Parked is told every run whose question just landed on its row as
	// awaiting: what a held call put to delegates wakes them from (ADR-0125).
	// After the durable write, so a woken delegate finds the call waiting; nil
	// tells nobody.
	Parked func(ctx context.Context, run PendingRun)

	// WaitedFor is how the run a run parked by `wait_for_run` waits for left
	// its seat's slots, read now, and false while it holds one: what the run is
	// carried on with ([Coordinator.OnWaitOver]). Nil carries no wait on.
	WaitedFor func(ctx context.Context, run PendingRun) (types.RunWaited, bool)

	// CloseStopped ends the turn of a run a person stopped ([Coordinator.StopSeat])
	// and answers how it ended for the executor log ([Coordinator.publishEnd]):
	// by whom and why, whether its reply reached anybody ([RunCalls]), and the
	// answer to the colleague whose ask the turn took up ([PendingRun.ColleagueAsk]).
	// A turn PARKED on the run is also closed on the record
	// ([types.AgentTurnStopped], [RunStop.Collected]). The engine implements it:
	// only the agent layer reads [PendingRun.ExecuteState]. EVERY STOP, PARKED OR
	// NOT: the END is this step's, since recorded by the launching frame its
	// tombstone refused the job's session. A STEP OF THE ENDING, before the record
	// is deleted, decided ONCE ([PendingRun.OwedEnd]), under identities from the
	// run and ending ([RecordedEnding.ID]) so a repeat is the same records. An
	// error keeps the ending for retry. Nil closes nothing.
	CloseStopped func(ctx context.Context, run PendingRun, calls RunCalls) (executorkeep.Ending, error)

	// TurnLost is handed every run whose decided ending is a LOSS
	// ([PendingRun.lost]), to settle what the parked turn still owed outside the
	// run: the colleague whose a2a_ask woke it ([PendingRun.ColleagueAsk]) is told
	// it was lost, and how the turn ended is recorded ([Coordinator.publishEnd],
	// [RunCalls]). THE ONE TERMINAL OF A PARKED TURN NO TURN FRAME ENDS (a resume
	// ends a collected run's, [CoordinatorOptions.CloseStopped] a stop's); without
	// it the ask waited for the idle sweep. A STEP OF THE ENDING, after the
	// announcement and before the record is deleted, decided ONCE
	// ([PendingRun.OwedEnd]); BEST EFFORT toward the asker, whose refusal must not
	// hold the seat's inbox. Nil does nothing.
	TurnLost func(ctx context.Context, run PendingRun, calls RunCalls) executorkeep.Ending

	// SteersEnded publishes what became of every note a person sent the run's
	// turn that no step reported reading ([PendingRun.EndedNotes]): DELIVERED
	// (told again by a later launch's brief), UNCONFIRMED (handed to a harness,
	// never confirmed), MISSED (a take whose answer was lost recorded them read,
	// no later job told them), EXPIRED (still WAITING, [RunSteer], or first read
	// by a job that never started). The engine implements it: only it opens a
	// note's seal. A STEP OF THE ENDING like CloseStopped, repeated after a crash
	// under identities from the run, ending and note ([RecordedEnding.ID],
	// [RecordedEnding.At]); an error keeps the ending for retry. A note filed once
	// the ending is decided is refused ([PendingStore.FileNote]). Nil publishes
	// nothing.
	SteersEnded func(ctx context.Context, run PendingRun) error

	// SteersConfirmed publishes, as delivered, every note and wake item a hook's
	// serve to the run's job is recorded as having handed over ([readByServeOf]),
	// the engine claiming each wake. Called on every Claude Code box read, asked
	// or not ([Coordinator.settleReceipts]), so a verdict whose answer was
	// lost is published by the next read. Ids derive from the turn and each note,
	// so a repeat is the same records; nil publishes nothing.
	SteersConfirmed func(ctx context.Context, run PendingRun, notes []RunNote)

	// WakesEnded reports the wake items of a job a relaunch replaced that no
	// step read, which its write removed ([PriorBox.Wakes]): each unconfirmed
	// or unread, its delivery left to a turn of its own. Called once the write
	// landed, never on the launch's path to fail it: the engine publishes off it,
	// under an id from the turn and the wake. Nil reports nothing.
	WakesEnded func(ctx context.Context, run PendingRun, wakes []RunNote)

	// Served is every other conversation run's turn served, each one whose
	// wake a step of the run read, as its end names them on the executor log
	// ([TurnEnd.Served]): the engine's to say, since only it opens a wake's
	// seal. Nil names none.
	Served func(ctx context.Context, run PendingRun) []string

	// SeatCap is the most runs a seat holds at once, at most one per
	// conversation (seatcap.go): the engine's, from the seat's
	// sandbox.max_concurrent_runs, read at every check so an apply moves the
	// next one. Nil, or an answer below one, is one.
	SeatCap func(handle string) int

	// Busy is told a seat's runs holding it went from none to some on this
	// node or back, or that this node let the seat go: the engine's router of
	// the seat's held inbox follows it, reading [Coordinator.SeatRuns]. Not an
	// owed answer's hold, which leaves no run to hand a wake to. Called outside
	// the coordinator's lock, possibly out of order, hence a read rather than a
	// value. Nil tells nobody.
	Busy func(handle string)

	// FoldInAnswered is told a seat a person answered a fold-in of one of its
	// runs (foldin.go): the answer is on the run's row already, and the
	// engine's router of the seat reads it at once rather than at its
	// backstop. Nil tells nobody.
	FoldInAnswered func(handle string)

	// Hold holds a seat's inbox while one of its runs holds the seat or an
	// answer it owes awaits its resume ([SeatHold]). Nil holds nothing (a test's
	// coordinator).
	Hold SeatHold

	// Admit says whether a seat may run a retried resume now ([Admission]). Nil
	// admits every retry.
	Admit Admission

	// Gate is the node's admission, which every resume passes before it claims
	// its run, on every route and told which ([TurnGate]). Nil admits every turn.
	//
	// NOT Admit, the screening a delivery applies (lease, pause, model, posture),
	// which a retried answer asks because it runs outside the screened delivery.
	// The gate is whether this process may start the turn at all or would meet a
	// known condition: its models' limit, a park this node holds the seat in, and
	// for an answer (first call a launch) the launch's budget. A completion is
	// asked no budget: its job is collected and charged first, and a refusing
	// budget holds it then ([ErrResumeHeld]).
	Gate TurnGate

	// Collections is how many reads of its runs' boxes this node makes itself
	// at once ([Coordinator.collectionSlot]): its turns, node.max_concurrent as
	// its gate resolves it, so the reads the gate does not admit (a completion
	// ended at its turn's deadline, a stop's, a loss's) stay within them. Zero
	// bounds none, as a nil Gate admits every turn; below zero is refused. See
	// the package doc, "A node's collections".
	Collections int

	// Lease is the seat lease this node holds a seat under, stamped on the row by
	// a launch ([PendingStore.BeginLaunch]) and carried back by every later
	// write. It lets the seat's next holder FENCE this node out: a write under
	// the outranked lease is refused (a release that revives a reaped claim, a
	// resumed turn taking an answer the holder is handing back).
	//
	// ASKED AT EVERY WRITE ON A SEAT'S BEHALF rather than carried on each request:
	// a path that forgot would write unfenced unnoticed. THREE ANSWERS: the
	// seat's fence and true (held here); the ZERO fence and true (unfenced, a node
	// with no seat lease); false (a seat host that does not hold the seat), which
	// every write refuses with [ErrSeatNotHeld] (the zero fence there constrains
	// nothing). Nil answers the zero fence, held.
	Lease func(handle string) (fence Fence, held bool)

	// Spent records a recorded answer's deliveries as WORKED in the completion
	// ledger, so a later copy of the delivery is dropped, WHATEVER ITS TYPE (an
	// answer by turn runs no turn, so a filter on the ledger's turn set missed
	// it). CALLED THE MOMENT THE ANSWER LEAVES THE RUN: when a turn TAKES it
	// ([ResumeRequest.Begin]) and before an ending or decline hands it back (the
	// next holder's reap also spends a taken answer). Not when the turn returns:
	// the node can stop mid-turn after its reply reached the person. A spent
	// reply not yet gone is not lost: the row, never the delivery, resumes it or
	// hands it back ([declinedCopyID]). A REFUSAL IS SPENT ONLY ONCE A TURN THAT
	// TOOK IT KEEPS IT ([Coordinator.spendRefused]): the row carries none of its
	// reply ([AnswerRefused]). FAILS OPEN: a missed write costs a recoverable
	// duplicate turn, where refusing to move the answer would lose it.
	Spent func(ctx context.Context, handle string, evs []*events.Event)

	// WasSpent reports whether a delivery is recorded as worked in the ledger
	// [CoordinatorOptions.Spent] writes, for an answer by turn coming round after
	// it was spent ([Coordinator.OnAnswerGiven]) and a refused reply's delivery
	// whose refusal another attempt kept ([Coordinator.refusedReply]). FAILS
	// OPEN: unreadable, the answer reaches [Coordinator.AnswerByTurn], which asks
	// the run, and the refused reply is decided by its row ([PendingRun.RoundRefusals]),
	// running as a duplicate only where another attempt kept the refusal and the
	// run has since ended. NEVER THE WORD on a delivery whose refusal this node's
	// own attempt kept. Nil reads every delivery as unspent.
	WasSpent func(ctx context.Context, handle string, ev *events.Event) bool

	// After schedules f to run after d in its own goroutine and returns a cancel
	// reporting whether f had not started (time.AfterFunc's contract). Nil is
	// time.AfterFunc; a test replaces it to fire a retry when it chooses. It must
	// never call f before it returns.
	After func(d time.Duration, f func()) (stop func() bool)

	// Condense rewrites a collected run's report or failure detail past
	// [MaxRunTextBytes] ([Condenser]). Nil condenses nothing: such a piece keeps
	// its whole lines up to the bound and says how many it left out.
	Condense Condenser

	// Node is this node's id: written on every run it launches as the node whose
	// host its box is on ([PendingRun.LaunchNode]) and compared with every run it
	// reads, since a box is touched only on its host (boxhost.go). Empty
	// only in a test's coordinator, whose launched runs are all its own.
	Node string

	// Incarnation is this PROCESS's incarnation, `<node>:<uuid>`, which its seat
	// leases and presence are held under; Takers holds its TAKER LEASE under it
	// ([coord.ClassTaker], taker.go): claimed at build, renewed every
	// TakerTTL / [coord.KeptRenewals], released by [Coordinator.Stop] after the
	// last keep. Every capture names the lease ([SessionCapture.Taker],
	// [SessionCapture.TakerEpoch]); a wait for another process's capture settles
	// from the executor log once that lease lapsed (session.go). Held to
	// [maxIncarnationBytes]. BOTH OR NEITHER ([NewCoordinator] checks): a holder
	// with no lease would read as a taker gone the moment its capture began;
	// neither is a test's coordinator, waited for by stamp age.
	Incarnation string
	Takers      TakerLeases

	// TakerTTL is the taker lease's TTL: the deployment's seat lease TTL, so a
	// stopped taker reads as gone as fast as its seats move ([takerLease]). Zero
	// takes [seat.SeatLeaseTTL]; nothing past [coord.MaxDutyTTL], the longest
	// lease the store keeps outside seats and presence, is asked for.
	TakerTTL time.Duration

	// Boxes reads a run's box on the node whose host it is on, for a run this
	// node holds the seat of but not the box ([BoxReader]). Nil reads no peer's
	// box: a collection of one fails.
	Boxes BoxReader

	// Nudge tells the waiter driving a run to look at it now ([Nudge]); sent
	// where this coordinator leaves a box to its host ([Coordinator.tellHost]).
	// Nil tells nobody and the host's next pass finds it.
	Nudge func(ctx context.Context, n Nudge)

	// Refresh is handed the box a job CONTINUES IN (kept box reattached,
	// [PriorBox.ContinuesIn]) before the job starts, to renew what expires
	// ([RefreshFunc]), the hook the waiters run on every poll. AT THE START TOO:
	// a kept box runs no setup ([Coordinator.acquire]) and no poll reaches a
	// paused one, so a token can have expired and a job continued after an answer
	// ("go ahead and push") would act on it first. A fresh box needs none.
	// WHAT IT WRITES IS SEALED WITH THE JOB: the job's set does not exist yet, so
	// the launch's recorder keeps each token for it ([Coordinator.sealHanded]); a
	// launch failing before reclaims the box. A failing refresh is logged by the
	// implementation, the job starting with what the box holds. Nil refreshes none.
	Refresh RefreshFunc

	// Pushes reads what a collected run pushed to Gitlet ([RunPushes]). Required,
	// like Resume: without it a run given a repository would reach its turn
	// saying nothing of its pushes, which its reviewer reads as "nothing pushed".
	// On a node not serving Gitlet it answers an error, which the collection SAYS
	// ([Result.PushedUnread]); asked only for a run that may push ([Pushable]).
	Pushes RunPushes

	// Sessions is the executor log's writes (every turn's first read, every
	// capture of a harness session, every turn's end): THE ESTATE ROUTER'S
	// (internal/estate's ExecutorWrites), never a data node's own writer, since a
	// seat's box is wherever its seat runs, a seats-only node with no log
	// included. See session.go. Keep is the engine's part: the company's policy,
	// the scrub's secrets, the instant a turn's operations are minted at, and the
	// sealed upload. Both or neither: either nil keeps no session and records no
	// end (a test's coordinator).
	Sessions SessionWrites
	Keep     SessionKeep

	// Restore is what a relaunch continues its turn's kept session from: the
	// executor log's row read through THE ESTATE ROUTER'S reads, as Sessions
	// writes through its writes, and the sealed object (restore.go). Nil
	// continues no kept session: a relaunch continues at most its own box's.
	Restore SessionRestore

	// Holders is whether a session id is any turn's in the executor log, which a
	// minted session's bind asks of a fresh id (minted.go). Nil refuses every
	// fresh id, so a minting harness's job binds only the session it continued.
	Holders SessionHolders

	// Saves is the fleet's ledger of each seat's saves, which a save of a running
	// job is charged against before its stamp (saveledger.go): the engine's
	// coordination store. Nil takes no save.
	Saves SaveLedgers

	// SpoolDir is where this node spools a capture's sealed session between its
	// take and keep ([Coordinator.spool]), in a directory of the process's own
	// under it ([Coordinator.spoolHere]). Empty is os.TempDir; a test names its
	// own directory so what is left is its own.
	SpoolDir string

	// Spools is told each final take's wait for spool room and the room held
	// ([SpoolMeter]): the engine's meter records them in the node's metrics. Nil
	// measures nothing.
	Spools SpoolMeter

	// Carries is told each collected launch's verdict on its session, once
	// per launch ([SessionMeter]). Nil measures nothing.
	Carries SessionMeter

	// Conversations is told, once the executor log has a turn's end, the
	// verdict its conversation is told ([PendingRun.ConversationNotice],
	// [ConversationTeller]). Nil tells nobody.
	Conversations ConversationTeller

	// AnswerBudget is the node's answer budget every collection of a run's
	// box holds its weight of while it reads ([Runner.Collect]), its peers'
	// payload answers beside. Nil is a budget of this coordinator's own.
	AnswerBudget *queue.AnswerBudget

	// Now is the clock, injectable for tests.
	Now func() time.Time
}

// Coordinator is the engine's hands on its executors' detached runs.
//
// Ordering makes each transition safe. SUSPEND persists the parked state and holds
// the seat. COMPLETION is claimed AT MOST ONCE, for its own job (collect,
// post-account, publish `executor_collected` once, re-enter the turn); the seat
// stays held until just before the resume, else a queued event takes the slot and
// the redelivery finds the claim flipped, the turn lost. PARK gives the seat BACK
// (answers take days); deliveries are offered to [Coordinator.TryResumeFromAnswer]
// first, the park is DURABLE FIRST ([Coordinator.unclaim] on failure), the first
// reply RECORDED before the resume. RECOVERY re-marks running jobs, inherits
// questions, reaps abandoned tails. STOP ends the seat's runs ([Coordinator.StopSeat]).
type Coordinator struct {
	// failureKind is [CoordinatorOptions.FailureKind].
	failureKind func(error) string
	// nodesFailure is [CoordinatorOptions.NodesFailure].
	nodesFailure func(context.Context, error) bool
	// resumableElsewhere is [CoordinatorOptions.ResumableElsewhere].
	resumableElsewhere func(ctx context.Context, run PendingRun) (bool, error)
	// operatorsCondition is [CoordinatorOptions.OperatorsCondition].
	operatorsCondition func(error) (string, bool)

	queue   Publisher
	pending PendingStore
	manager *Manager
	resume  Resumer
	account Accountant
	// audience is [CoordinatorOptions.Audience].
	audience AudienceResolver
	// answers is [CoordinatorOptions.Answers], leads [CoordinatorOptions.Leads].
	answers  *AnswerSeal
	leads    *LeadSeal
	condense Condenser
	// pushes is [CoordinatorOptions.Pushes].
	pushes RunPushes
	ended  func(runID string)
	// recovered is [CoordinatorOptions.Recovered].
	recovered func(ctx context.Context, run PendingRun)
	stopped   func(ctx context.Context, handle, turnID string)
	parked    func(ctx context.Context, run PendingRun)
	// waitedFor is [CoordinatorOptions.WaitedFor].
	waitedFor func(ctx context.Context, run PendingRun) (types.RunWaited, bool)
	// unresumable is [CoordinatorOptions.Unresumable].
	unresumable func(ctx context.Context, run PendingRun) (*Unresumable, error)
	// closeStopped is [CoordinatorOptions.CloseStopped].
	closeStopped func(ctx context.Context, run PendingRun, calls RunCalls) (executorkeep.Ending, error)
	// turnLost is [CoordinatorOptions.TurnLost].
	turnLost func(ctx context.Context, run PendingRun, calls RunCalls) executorkeep.Ending
	// steersEnded is [CoordinatorOptions.SteersEnded], steersConfirmed
	// [CoordinatorOptions.SteersConfirmed] and wakesEnded
	// [CoordinatorOptions.WakesEnded].
	steersEnded     func(ctx context.Context, run PendingRun) error
	steersConfirmed func(ctx context.Context, run PendingRun, notes []RunNote)
	wakesEnded      func(ctx context.Context, run PendingRun, wakes []RunNote)
	// served is [CoordinatorOptions.Served].
	served func(ctx context.Context, run PendingRun) []string
	// seatCap is [CoordinatorOptions.SeatCap].
	seatCap func(handle string) int
	// busy is [CoordinatorOptions.Busy].
	busy func(handle string)
	// foldInAnswered is [CoordinatorOptions.FoldInAnswered].
	foldInAnswered func(handle string)
	// node, boxes and nudge are [CoordinatorOptions.Node], Boxes and Nudge.
	node  string
	boxes BoxReader
	nudge func(ctx context.Context, n Nudge)
	// taker is this process's taker lease ([CoordinatorOptions.Takers]), nil
	// where none is wired.
	taker *takerLease
	// refresh is [CoordinatorOptions.Refresh].
	refresh RefreshFunc
	// sessions and keep are [CoordinatorOptions.Sessions] and Keep. Every wait of
	// the coordinator is a field so a case sets its own: sessionRetry is the
	// first wait between attempts at a record ([sessionRetryBase]), recordBudget
	// all of a record's attempts ([sessionRecordBudget]), beginBudget all of a
	// begin's ([beginRecordBudget]), captureTick how often an ending waiting on a
	// capture reads the row ([captureWaitTick]).
	sessions     SessionWrites
	keep         SessionKeep
	sessionRetry time.Duration
	recordBudget time.Duration
	beginBudget  time.Duration
	captureTick  time.Duration
	// restore is [CoordinatorOptions.Restore]; relaunchWait bounds a
	// relaunch's asks ([relaunchAskWait]), and captureRest how long it waits
	// for a capture of an archive of a size still being kept
	// ([captureRestFor]); holdingsWait bounds reading what a cut-off session
	// holds ([holdingsReadWait]).
	restore      SessionRestore
	holders      SessionHolders // [CoordinatorOptions.Holders]
	relaunchWait time.Duration
	captureRest  func(archive int64) time.Duration
	holdingsWait time.Duration
	// endWait is how long a read waits for a job its script was asked to end
	// ([JobEndWait]); endPoll how often it asks ([jobEndPoll]).
	endWait time.Duration
	endPoll time.Duration
	now     func() time.Time

	// collections is [CoordinatorOptions.Collections] as a semaphore, nil for
	// none, and collectionsWaiting how many reads wait for one
	// ([Coordinator.collectionSlot]).
	collections        chan struct{}
	collectionsWaiting atomic.Int32

	// saves is this node's part in saving its runs' sessions as they work
	// (save.go).
	saves saveState
	// sharedPack is the node's ONE pack slot, taken while a pack runs in a box
	// sharing the node's memory ([Placement.OwnsMemory]), a save's or a take's
	// ([Coordinator.packSlot]). Capacity 1: such a pack is charged to the
	// engine's memory (an OpenCode export holds up to ten times its session),
	// and two read at once could each pass for the other's room. The cost is a
	// drain one pack at a time: 0.4 to 5.2 s an OpenCode export of up to 64 MiB,
	// 0.04 to 0.46 s a Claude Code pack of 1 to 20 MiB (package doc, "A pack in a
	// node's own memory, one at a time").
	sharedPack chan struct{}
	// spoolRoom is the room every spool of this process shares, in bytes
	// ([spoolRoomBytes], spoolroom.go).
	spoolRoom *spoolRoom
	// sessionMeter is [CoordinatorOptions.Carries].
	sessionMeter SessionMeter
	// conversations is [CoordinatorOptions.Conversations].
	conversations ConversationTeller
	// answerBudget is the node's answer budget ([CoordinatorOptions.AnswerBudget]).
	answerBudget *queue.AnswerBudget

	// mainThreadWait bounds reading a job's main thread at its collection
	// ([MainThreadReadWait], [Coordinator.readMainThread]); verdictWait and
	// verdictRetry are a hook verdict's reason and spacing ([hookVerdictWait],
	// [hookVerdictRetry]).
	mainThreadWait time.Duration
	verdictWait    time.Duration
	verdictRetry   time.Duration

	// keepLife is the lifetime every keep runs under, off the read that took its
	// capture ([Coordinator.keepLater]); keeping counts them: [Coordinator.Stop]
	// lets those under way finish for [sessionKeepDrain], then ends them.
	// spoolDir is [CoordinatorOptions.SpoolDir], resolved, and spools the
	// coordinator's own directory under it (spooldir.go).
	keepLife context.Context
	endKeeps context.CancelFunc
	keeping  sync.WaitGroup
	spoolDir string
	spools   ownSpools

	// mu guards runs, the two seat-level answers the inbox screening reads on
	// every delivery, and attempts beside them. In memory rather than a store
	// read per delivery: the seat's owner is the only node running its turns,
	// and RecoverSeat seeds it on takeover.
	mu   sync.Mutex
	runs map[string]seatRuns
	// reserved counts each seat's slots taken by a write out that moves a run
	// into [Holding], and heldRuns is each run counted there, by turn, with
	// the conversations it works: together the seat's cap and its busy
	// conversations (seatcap.go). Under mu.
	reserved map[string]int
	heldRuns map[string]heldRun
	// waitingRuns is each run parked by `wait_for_run` this node counts, by
	// turn: out of [Holding], its keys stay held while it waits, so its mail
	// waits aside behind it (ADR-0176's amendment). Under mu.
	waitingRuns map[string]heldRun
	// freed is each seat's keys its runs leaving [Holding] freed, readmitted
	// on its inbox where no run still holds them ([Coordinator.readmitFreed]),
	// and asideFull each seat whose inbox holds as many deliveries aside as it
	// keeps ([Coordinator.HoldForAside]). Under mu.
	freed     map[string][]freedKey
	asideFull map[string]bool

	// moves counts what this node has done to each seat's runs that a recount
	// from the store must not be read across (every [Coordinator.adjustLocked]
	// transition, deleted ending record and recount written), so a recount can
	// tell one landed while it read. See [Coordinator.syncSeat].
	moves map[string]uint64

	// attempts counts the failed handoffs of one delivery to one parked run, so a
	// resume failing every time stops circling the seat's inbox: a BUDGET PER
	// DELIVERY keyed by the run it is owed to. The bounds, and why per process,
	// are at [MaxAnswerAttempts] and [maxAnswerDeliveries]; the dead-letter
	// margin is [AnswerDeliveryReserve], measured on the delivery.
	attempts map[answerKey]map[string]answerBudget

	// retries are the owed answers this node is retrying the resume (or the
	// hand-back) of, by turn id; owed is which each seat owes, which with its
	// holding count is what its inbox hold is taken for; held whether the hold is
	// applied, inboxes each seat's inbox attachment ([Coordinator.RecoverSeat]),
	// and holdBusy, holdDirty and holdGen the serialisation
	// [Coordinator.reconcileHold] runs under. All under mu.
	retries map[string]*answerRetry
	// releases are the claim hand-backs this node is making or owes, by turn id,
	// recorded before each is sent, each with its write in its run's entry
	// ([Coordinator.writing]); claiming counts the claims of a job's completion
	// asked of the store and not yet answered. Together no claim of a job is
	// taken while its last claim's release is out (claimrelease.go). Under mu.
	releases map[string]*claimRelease
	claiming map[string]int
	// writing are the writes this node has out that move a run between its seat's
	// sets, by turn, recorded before sending and dropped, with the move made, in
	// the step that takes its answer (or that of an earlier write on the run
	// still out): open of a row, recovery takeover, park's question, answer's
	// record/claim/decline, claim release, delete of an ending that counts its
	// run out (countwrite.go). A recount counts every run with a write out where
	// the moves so far leave it, row listed or not ([Coordinator.syncSeat]).
	// Under mu.
	writing map[string]*runWrites
	// unsettled is the clock on each job's host that answered this node's
	// collections when its own coordination store did not, by turn id, until the
	// run settles ([hostUnsettled]). Under mu.
	unsettled map[string]hostUnsettled
	// republish is each re-fire's announcement the broker did not take, by turn
	// id, retried on this node's timer until one lands or this node settles the
	// run ([refirePublish]). Under mu.
	republish map[string]*refirePublish
	// refused are the completions the node's gate refused on a condition, by turn
	// id, kept so the condition clearing re-fires each at once
	// ([Coordinator.Readmit]), not a poll later. Under mu.
	refused map[string]refusedCompletion
	// refiring counts each run's job's re-fires on their way, by turn id
	// ([Coordinator.refireLocked], [Coordinator.liftAndRefireLocked]): a signal's
	// lift of the hold and a seat's recovery start none beside them
	// ([Coordinator.carriedLocked]). Under mu.
	refiring map[string]int
	// liftedDuring is the hold a signal's lift cleared while a re-fire was on its
	// way, by turn id, for the last re-fire to take up ([Coordinator.refired]).
	// Under mu.
	liftedDuring map[string]rowHold
	// blocked are the completions whose last resume failed on a condition only an
	// operator clears, by turn id ([blockedResume]). Under mu.
	blocked   map[string]blockedResume
	owed      map[string]map[string]struct{}
	held      map[string]bool
	inboxes   map[string]queue.Attachment
	holdBusy  map[string]bool
	holdDirty map[string]bool
	holdGen   map[string]uint64

	// signals counts every [Coordinator.Readmit], by condition and seat, so a
	// retry refused while one landed does not wait through it. Under mu; see
	// [Coordinator.waitOwed].
	signals map[signal]uint64

	// launchStops are the stops decided on this node's runs while still
	// LAUNCHING, by turn id, for the launching frame to take
	// ([Coordinator.LaunchStopped]). Under mu.
	launchStops map[string]launchStop

	// finishing are the turns an attempt here is finishing the ending of
	// ([Coordinator.finishOnce]); frames counts the turns whose resumed frame
	// runs here ([Coordinator.framing]). Under mu.
	finishing map[string]bool
	frames    map[string]int

	// recovering are the seats a recovery pass is under way for, by handle, with
	// the claims of their runs the pass carries once done; live the claims of
	// runs this node's attempts hold, by turn id, counted from the gate that let
	// them through until the attempt returns. Between them a seat's recovery pass
	// and its runs' claims here exclude each other (recoverygate.go). Under mu.
	recovering map[string]*seatRecovery
	live       map[string]*liveAttempt

	hold  SeatHold
	admit Admission
	// gate is [CoordinatorOptions.Gate]; see [Coordinator.admitTurn].
	gate  TurnGate
	after func(time.Duration, func()) func() bool
	// spent is [CoordinatorOptions.Spent]; see [Coordinator.spend].
	spent func(ctx context.Context, handle string, evs []*events.Event)
	// wasSpent is [CoordinatorOptions.WasSpent]; see [Coordinator.OnAnswerGiven].
	wasSpent func(ctx context.Context, handle string, ev *events.Event) bool
	// lease is [CoordinatorOptions.Lease]; see [Coordinator.leaseOf].
	lease func(handle string) (Fence, bool)

	// life is the coordinator's own lifetime, which a scheduled retry runs under
	// (it outlives every delivery) and ends with [Coordinator.Stop], which waits
	// for inflight, every scheduled attempt not yet cancelled. Its end also holds
	// every store call on a run's row still to come to its reason alone
	// ([Coordinator.rowContext]).
	life     context.Context
	end      context.CancelFunc
	inflight sync.WaitGroup
	closed   bool

	// lastCalls ends one teardown's grace after the stop ends life, cutting
	// endLastCalls: life CARRIES it ([stopBoundKey]), so every call work on life
	// makes after the stop (a teardown's write, a stamp, a box call, each on its
	// own context that keeps none of life's cancellation) ends by then
	// ([detach]).
	lastCalls    context.Context
	endLastCalls context.CancelCauseFunc
}

// seatRuns is this node's count of one seat's detached runs, by the question
// each set answers.
//
// TWO COUNTS, NOT ONE: "is the seat held" and "is a run waiting for somebody's
// reply" are DISJOINT by construction ([Holding] and [Awaiting] share no status,
// since a run parked on a question must leave the seat free to receive the
// answer). Conflating them left the one delivery the answer match exists for
// arriving at a seat called free, consumed as an ordinary turn.
//
// COUNTED, not boolean: a resumed turn can launch a SECOND run before the first
// settles, so the seat is free only when the last is, and one seat can drive a
// job while another of its runs waits for a person.
type seatRuns struct {
	// holding counts the runs in [Holding], the ones the engine is driving, during
	// which the seat starts no new turn and its inbox is held (seathold.go).
	holding int

	// awaiting counts the runs in [Awaiting], stopped on a question. They hold
	// nothing, and this tells the screening to offer a delivery to the match
	// before anything else consumes it.
	//
	// ERRING HIGH IS SAFE AND ERRING LOW IS NOT, which decides every transition
	// that cannot prove a run left the set: one count too many costs a store
	// lookup that finds nothing, one too few is a person's answer run as an
	// unrelated turn and a box left to expire.
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
		{"Pushes", opts.Pushes == nil},
		{"Audience", opts.Audience == nil},
		{"Answers", opts.Answers == nil},
	} {
		if field.absent {
			missing = append(missing, "CoordinatorOptions."+field.name)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("sandbox: a coordinator needs %s: a detached run is "+
			"published, recorded, reconnected to, resumed, put to its "+
			"answerers and told what it pushed through them, and its answers "+
			"sealed and opened",
			strings.Join(missing, ", "))
	}
	switch {
	case (opts.Incarnation == "") != (opts.Takers == nil):
		return nil, fmt.Errorf("sandbox: CoordinatorOptions.Incarnation and CoordinatorOptions.Takers come " +
			"together: a taker lease is held under the process's incarnation, and an incarnation named on a " +
			"capture with no lease behind it reads as a taker gone")
	case len(opts.Incarnation) > maxIncarnationBytes:
		// REFUSED HERE, not at every stamp, where every capture would be refused.
		return nil, fmt.Errorf("sandbox: CoordinatorOptions.Incarnation is %d bytes, past the %d a run's "+
			"row carries of one: an incarnation is `<node>:<uuid>`, its node id at most 64 bytes",
			len(opts.Incarnation), maxIncarnationBytes)
	case opts.Collections < 0:
		return nil, fmt.Errorf("sandbox: CoordinatorOptions.Collections is %d: it is how many of its runs' "+
			"boxes the node reads at once, its turns (node.TurnsAtOnce), or zero for no bound",
			opts.Collections)
	}
	c := &Coordinator{
		queue: opts.Queue, pending: opts.Pending, manager: opts.Manager, failureKind: opts.FailureKind,
		resume: opts.Resume, pushes: opts.Pushes, account: opts.Account, audience: opts.Audience,
		nodesFailure:       opts.NodesFailure,
		resumableElsewhere: opts.ResumableElsewhere,
		operatorsCondition: opts.OperatorsCondition,
		answers:            opts.Answers,
		leads:              opts.Leads,
		condense:           opts.Condense,
		ended:              opts.Ended,
		recovered:          opts.Recovered,
		unresumable:        opts.Unresumable,
		stopped:            opts.Stopped,
		parked:             opts.Parked,
		waitedFor:          opts.WaitedFor,
		closeStopped:       opts.CloseStopped,
		turnLost:           opts.TurnLost,
		steersEnded:        opts.SteersEnded,
		steersConfirmed:    opts.SteersConfirmed,
		wakesEnded:         opts.WakesEnded,
		served:             opts.Served,
		seatCap:            opts.SeatCap,
		busy:               opts.Busy,
		foldInAnswered:     opts.FoldInAnswered,
		node:               opts.Node,
		boxes:              opts.Boxes,
		nudge:              opts.Nudge,
		refresh:            opts.Refresh,
		sessions:           opts.Sessions,
		keep:               opts.Keep,
		restore:            opts.Restore,
		holders:            opts.Holders,
		saves: saveState{ledger: opts.Saves, slots: make(chan struct{}, maxConcurrentSaves),
			memos: map[string]*saveMemo{}},
		sharedPack:    make(chan struct{}, 1),
		spoolRoom:     newSpoolRoom(opts.Spools),
		sessionMeter:  opts.Carries,
		conversations: opts.Conversations,
		answerBudget:  opts.AnswerBudget,
		relaunchWait:  relaunchAskWait,
		captureRest:   captureRestFor,
		holdingsWait:  holdingsReadWait,
		endWait:       JobEndWait,
		endPoll:       jobEndPoll,

		// THE ONE WAIT every reading of a job's main thread shares.
		mainThreadWait: MainThreadReadWait,
		verdictWait:    hookVerdictWait,
		verdictRetry:   hookVerdictRetry,

		spoolDir:     opts.SpoolDir,
		sessionRetry: sessionRetryBase,
		recordBudget: sessionRecordBudget,
		beginBudget:  beginRecordBudget,
		captureTick:  captureWaitTick,
		now:          opts.Now,
		runs:         map[string]seatRuns{},
		reserved:     map[string]int{},
		heldRuns:     map[string]heldRun{},
		waitingRuns:  map[string]heldRun{},
		freed:        map[string][]freedKey{},
		asideFull:    map[string]bool{},
		moves:        map[string]uint64{},
		attempts:     map[answerKey]map[string]answerBudget{},
		retries:      map[string]*answerRetry{},
		releases:     map[string]*claimRelease{},
		claiming:     map[string]int{},
		writing:      map[string]*runWrites{},
		unsettled:    map[string]hostUnsettled{},
		republish:    map[string]*refirePublish{},
		refused:      map[string]refusedCompletion{},
		refiring:     map[string]int{},
		liftedDuring: map[string]rowHold{},
		blocked:      map[string]blockedResume{},
		owed:         map[string]map[string]struct{}{},
		held:         map[string]bool{},
		inboxes:      map[string]queue.Attachment{},
		holdBusy:     map[string]bool{},
		holdDirty:    map[string]bool{},
		holdGen:      map[string]uint64{},
		signals:      map[signal]uint64{},
		launchStops:  map[string]launchStop{},
		finishing:    map[string]bool{},
		frames:       map[string]int{},
		recovering:   map[string]*seatRecovery{},
		live:         map[string]*liveAttempt{},
		hold:         opts.Hold,
		admit:        opts.Admit,
		gate:         opts.Gate,
		after:        opts.After,
		spent:        opts.Spent,
		wasSpent:     opts.WasSpent,
		lease:        opts.Lease,
	}
	if c.now == nil {
		c.now = time.Now
	}
	if opts.Collections > 0 {
		c.collections = make(chan struct{}, opts.Collections)
	}
	if c.after == nil {
		c.after = func(d time.Duration, f func()) func() bool { return time.AfterFunc(d, f).Stop }
	}
	c.lastCalls, c.endLastCalls = context.WithCancelCause(context.Background())
	c.life, c.end = context.WithCancel(context.WithValue(context.Background(), stopBoundKey{}, c.lastCalls))
	c.keepLife, c.endKeeps = context.WithCancel(context.Background())
	// THE TAKER LEASE LAST, once nothing can refuse the coordinator; held until
	// [Coordinator.Stop] has seen the last keep end.
	if opts.Takers != nil {
		c.taker = newTakerLease(opts.Takers, opts.Incarnation, opts.TakerTTL)
	}
	c.sweepSpools()
	if c.answerBudget == nil {
		c.answerBudget = queue.NewAnswerBudget(nil)
	}
	return c, nil
}

// Stop ends every scheduled retry and waits for any running. An owed answer's
// resume stays recorded on its run for the next holder ([Coordinator.RecoverSeat]).
// WHAT IS RUNNING GETS ONE TEARDOWN'S GRACE FROM THE STOP, ALL OF IT
// ([discardGrace]): later calls end by then, each on a context keeping the stop's
// one deadline ([detach]) (per-call graces held the stop ninety seconds where a
// pod's share is thirty). A KEEP UNDER WAY gets [sessionKeepDrain], then is ended
// and stamped refused. THE TAKER LEASE IS RELEASED ONLY AFTER THE LAST KEEP ENDS,
// else a peer reads a capture whose lease is gone as never-recording and settles it
// refused, tombstoning the turn early; released, not lapsed, so waits settle at once
// ([takerLease.release]).
func (c *Coordinator) Stop() {
	c.mu.Lock()
	c.closed = true
	for _, r := range c.retries {
		if r.stop != nil && r.stop() {
			c.inflight.Done()
		}
		r.stop = nil
	}
	for turnID := range c.republish {
		c.dropRepublishLocked(turnID)
	}
	// Kept completions' re-fires too: nothing waits for them
	// ([Coordinator.keepLocked]) and the next holder owes the announcement.
	for turnID := range c.refused {
		c.dropKeptLocked(turnID)
	}
	c.mu.Unlock()
	c.end()
	grace := time.AfterFunc(discardGrace, func() { c.endLastCalls(errStopGrace) })
	c.inflight.Wait()
	// Nothing is left to wait for, so the grace ends.
	grace.Stop()
	c.endLastCalls(errStopGrace)
	kept := make(chan struct{})
	go func() {
		c.keeping.Wait()
		close(kept)
	}()
	drain := time.NewTimer(sessionKeepDrain)
	defer drain.Stop()
	select {
	case <-kept:
	case <-drain.C:
	}
	c.endKeeps()
	<-kept
	c.closeSpools()
	if c.taker != nil {
		c.taker.stop()
	}
}

// leaseOf is the lease this node writes on a seat's behalf under —
// [CoordinatorOptions.Lease] — and whether it holds the seat at all; the zero,
// unfenced lease, held, where none is wired.
func (c *Coordinator) leaseOf(handle string) (Fence, bool) {
	if c.lease == nil {
		return Fence{}, true
	}
	return c.lease(handle)
}

// notHeld logs a write refused because this node does not hold the seat, and
// returns the refusal for the caller to report.
func notHeld(ctx context.Context, handle, turnID, write string) error {
	log.InfoContext(ctx, "sandbox_seat_not_held",
		"agent", handle, "turn_id", turnID, "write", write,
		"detail", "this node does not hold the seat, so the write is left to the seat's "+
			"holder, whose recovery pass drives the run")
	return fmt.Errorf("sandbox: %s for run %s on seat %q: %w", write, turnID, handle, ErrSeatNotHeld)
}

// SetManager swaps the sandbox manager, for a live reload of providers.sandbox.
//
// RUNS IN FLIGHT KEEP THEIR BOXES REACHABLE: a cell the new catalogue drops is
// carried as a retired backend ([Manager.carrying]), so its jobs are polled,
// collected, reclaimed and reaped through the backend that made them, while new
// runs provision from the new catalogue alone. A NIL MANAGER CHANGES NOTHING:
// it is the removal of providers.sandbox, which validates no launching seat, so
// the last manager stays to finish the runs in flight.
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

// packSlot waits on ctx for the node's one pack slot ([Coordinator.sharedPack])
// where run's box shares the node's memory, and returns what gives it back, to
// be called as the pack returns; a box that owns its memory takes none.
func (c *Coordinator) packSlot(ctx context.Context, run PendingRun) (give func(), err error) {
	cell := Placement(run.Placement)
	if manager := c.mgr(); manager != nil {
		cell = manager.CellOf(run)
	}
	if cell.OwnsMemory() {
		return func() {}, nil
	}
	select {
	case c.sharedPack <- struct{}{}:
		return func() { <-c.sharedPack }, nil
	case <-ctx.Done():
		return nil, fmt.Errorf("waiting for the node's one pack in a box that shares its memory: %w", ctx.Err())
	}
}

// collectionSlot waits on ctx for one of this node's collections
// ([CoordinatorOptions.Collections]) and returns what gives it back, once
// however often it is called. Held around a read of a run's box made on this
// node and nothing else ([Coordinator.boxResult], [Coordinator.readLostHere]):
// internal/sandbox/executor's base tier counts those reads at the node's turns,
// and a read asked of another node's host is bounded by that host's slots. A
// wait its context ends reads nothing ([errReadUnstarted]); a read that cannot
// start at once says so (`sandbox_collection_waits`).
func (c *Coordinator) collectionSlot(ctx context.Context, turnID string) (func(), error) {
	if c.collections == nil {
		return func() {}, nil
	}
	give := sync.OnceFunc(func() { <-c.collections })
	select {
	case c.collections <- struct{}{}:
		return give, nil
	default:
	}
	waiting := c.collectionsWaiting.Add(1)
	defer c.collectionsWaiting.Add(-1)
	log.InfoContext(ctx, "sandbox_collection_waits", "turn_id", turnID,
		"collections", cap(c.collections), "waiting", waiting,
		"detail", "this node is making as many reads of its runs' boxes as it runs turns, so this "+
			"read waits for one of them to end")
	select {
	case c.collections <- struct{}{}:
		return give, nil
	case <-ctx.Done():
		log.InfoContext(ctx, "sandbox_collection_unstarted", "turn_id", turnID,
			"collections", cap(c.collections), "error", ctx.Err().Error(),
			"detail", "this read's wait for one of the node's collections ended before it started; the "+
				"box was not read, and the read is asked again")
		return nil, fmt.Errorf("%w: waiting for one of this node's %d collections to read %s: %w",
			errReadUnstarted, cap(c.collections), turnID, ctx.Err())
	}
}

// errReadUnstarted is a read of a run's box on this node whose wait for one of
// the node's collections its context ended ([Coordinator.collectionSlot]). The
// box was not asked, so it says nothing of the box: a collection hands its
// claim back uncounted ([Coordinator.collectFailedAt]), and a stop or a loss
// keeps its ending for the retry.
var errReadUnstarted = errors.New("sandbox: the read of the run's box did not start")

// SeatRuns is what this node knows about one seat's detached runs: whether one
// HOLDS the seat, and whether one is waiting for a person's answer. How many,
// against the seat's cap, is [Coordinator.SeatCount].
//
// The inbox screening reads BOTH values together because they change together:
// a run parking stops holding and starts awaiting in one moment, so two calls
// could land between the halves and see an idle seat with no question open, the
// state in which a person's reply is eaten as an ordinary turn.
func (c *Coordinator) SeatRuns(handle string) (held, awaitsAnswer bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	runs := c.runs[handle]
	return runs.holding > 0, runs.awaiting > 0
}

// SeatHeldBySandbox reports whether a detached run is HOLDING a seat, so it
// takes no new turn until the run settles: a job can run for hours, past any
// broker ack window, so the seat's inbox is HELD (nothing fetched) rather than
// its mail consumed and held (seathold.go). NOT "is a run waiting for an
// answer", which is [Coordinator.SeatRuns]'s second value; see [seatRuns].
func (c *Coordinator) SeatHeldBySandbox(handle string) bool {
	held, _ := c.SeatRuns(handle)
	return held
}

// setOf says which count a status belongs to. A status that owns no seat-level
// state (a run that has ended, or the empty string a write passes for no row
// at all, countwrite.go) belongs to neither.
func setOf(status string) (holding, awaiting int) {
	switch {
	case slices.Contains(Holding, status):
		return 1, 0
	case slices.Contains(Awaiting, status):
		return 0, 1
	}
	return 0, 0
}

// adjustLocked applies the deltas to handle's seat under c.mu in one step for
// both counts (a park is a decrement and an increment [Coordinator.SeatRuns]
// must never see apart) and reports the counts before and after, for the caller
// to free the seat outside the lock ([Coordinator.seatFreed]). Clamped at zero:
// a decrement for a run this node never counted is ordinary, and a negative
// count would swallow the next increment. See the package doc, "Seat counts".
func (c *Coordinator) adjustLocked(handle string, holding, awaiting int) (was, runs seatRuns) {
	c.moves[handle]++
	was = c.runs[handle]
	runs = was
	runs.holding = max(runs.holding+holding, 0)
	runs.awaiting = max(runs.awaiting+awaiting, 0)
	if runs == (seatRuns{}) {
		delete(c.runs, handle)
	} else {
		c.runs[handle] = runs
	}
	return was, runs
}

// moved says that a seat's runs moved in the store under this node's hand (an
// ending deleted a run's record), so a recount whose listing was read before is
// read again ([Coordinator.syncSeat]) rather than written after the recount
// that follows the delete. Said for every delete of this node's.
func (c *Coordinator) moved(handle string) {
	if handle == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.moves[handle]++
}

// seatFreed readmits on the seat's inbox what its runs leaving [Holding] freed
// ([Coordinator.readmitFreed]), signals [ConversationBusy] for a seat one of
// whose runs has just left, and [SeatFull] where that freed a slot below its
// cap, so an answer owed on it is resumed now rather than at its backstop, and
// tells [CoordinatorOptions.Busy] of a seat that became busy or free. Called
// outside c.mu, which [Coordinator.Readmit] takes.
func (c *Coordinator) seatFreed(ctx context.Context, handle string, was, now seatRuns) {
	c.readmitFreed(ctx, handle)
	// A RUN PARKED ON A WAIT ENDING frees the conversations it kept busy too.
	if now.holding < was.holding || now.awaiting < was.awaiting {
		c.Readmit(ConversationBusy, handle)
	}
	if now.holding < was.holding && now.holding < c.capOf(handle) {
		c.Readmit(SeatFull, handle)
	}
	if (was.holding > 0) != (now.holding > 0) && c.busy != nil {
		c.busy(handle)
	}
}

// OnEvent routes a seat's control-topic delivery. A refusal for a seat this node
// no longer holds names [SeatLost], handing the delivery to its holder. While
// this node keeps a clock on the box host's store ([Coordinator.unannouncedJob])
// a completion is acknowledged only where its run settled; every other answer
// hands it back marked [ErrUnannounced] and it is renewed (renew.go). A refusal
// the topic acknowledges is first recorded on the job's record
// ([Coordinator.oweRefused]). See the package doc, "Routing a control-topic
// delivery".
func (c *Coordinator) OnEvent(ctx context.Context, ev *events.Event) error {
	if ev == nil {
		return nil
	}
	switch payload := ev.Data.(type) {
	case *types.ExecutorLaunched:
		return c.OnStarted(ctx, *payload)
	case *types.ExecutorFinished:
		c.hearAnnouncement(ctx, *payload, ev)
		ctx = delivering(ctx, ev)
		answer := c.renewing(ctx, *payload, ev, c.OnCompleted(ctx, *payload, ev))
		return c.lost(payload.AgentHandle, c.oweRefused(ctx, *payload, answer))
	case *types.ExecutorAnswerGiven:
		if payload.FoldIn != "" {
			// A FOLD-IN'S ANSWER IS RECORDED ALREADY (AnswerDesk.AnswerFoldIn):
			// this delivery only hands back the wake it decides, should it wait
			// aside ([Coordinator.readmitFoldIn]), and tells the seat's router
			// to read it.
			c.readmitFoldIn(ctx, *payload)
			if c.foldInAnswered != nil {
				c.foldInAnswered(payload.AgentHandle)
			}
			return nil
		}
		return c.lost(payload.AgentHandle, c.OnAnswerGiven(ctx, *payload, ev))
	case *types.ExecutorWaitOver:
		return c.lost(payload.AgentHandle, c.OnWaitOver(ctx, *payload, ev))
	}
	return nil
}

// lost names a refusal of a seat this node no longer holds as [SeatLost],
// asking the node's own lease rather than taking the refusal's word: see
// [SeatLost] for the refusal that names no loss.
func (c *Coordinator) lost(handle string, err error) error {
	if err == nil || !errors.Is(err, ErrSeatNotHeld) {
		return err
	}
	if _, held := c.leaseOf(handle); held {
		return err
	}
	return &TurnRefused{Refusal: Refusal{Condition: SeatLost, Reason: err.Error()}, Cause: err}
}

// OnStarted recounts the seat from the store.
//
// NOT WHAT HOLDS THE SEAT: the launch did that as it wrote the row
// ([Coordinator.Launch]); this event arrives on a subscription the launching
// turn never waits for, and holding from here left a window for the seat's next
// mail to run beside the job. It puts the store's answer under the counts, on
// whichever node holds the seat. Idempotent on redelivery because it consults
// the store rather than incrementing: a double increment would hold the seat
// forever.
func (c *Coordinator) OnStarted(ctx context.Context, ev types.ExecutorLaunched) error {
	if ev.AgentHandle == "" {
		return nil
	}
	c.syncSeat(ctx, ev.AgentHandle)
	log.InfoContext(ctx, "sandbox_agent_busy",
		"agent", ev.AgentHandle, "turn_id", ev.TurnID, "sandbox_id", ev.SandboxID)
	return nil
}

// syncSeat sets both of a seat's counts from one listing of the store, the
// arbiter, so redeliveries, restarts and takeovers converge; an unreadable store
// leaves them alone. A recount during which this node moved the seat, or
// another recount was written, reads again, up to [seatRecountRounds]; a run
// with a write of this node's out is counted where those writes leave it
// ([Coordinator.countedLocked]), so a write's answer moves the count once. See
// the package doc, "Recounting a seat".
func (c *Coordinator) syncSeat(ctx context.Context, handle string) {
	for range seatRecountRounds {
		c.mu.Lock()
		moved := c.moves[handle]
		c.mu.Unlock()
		runs, err := c.pending.ListActiveForSeat(ctx, handle)
		if err != nil {
			log.WarnContext(ctx, "sandbox_busy_sync_failed", "agent", handle, "error", err.Error())
			return
		}
		served := c.servedOf(ctx, runs)
		c.mu.Lock()
		if c.moves[handle] != moved {
			c.mu.Unlock()
			continue
		}
		// COUNTED UNDER THE LOCK writes this node has out move the count once
		// their answers are taken, so such a run is counted where the moves so
		// far leave it, row listed or not.
		counts := c.countedLocked(handle, runs)
		c.heldFromListingLocked(handle, runs, served)
		// THIS RECOUNT MOVES THE SEAT TOO, for an older recount still on its way.
		c.moves[handle]++
		was := c.runs[handle]
		if counts == (seatRuns{}) {
			delete(c.runs, handle)
		} else {
			c.runs[handle] = counts
		}
		c.mu.Unlock()
		c.seatFreed(ctx, handle, was, counts)
		// The hold follows the recount, as it follows every transition.
		c.reconcileHold(ctx, handle)
		return
	}
	log.WarnContext(ctx, "sandbox_busy_sync_unsettled", "agent", handle,
		"detail", "the seat's runs kept moving under the recount, so the counts this node "+
			"already kept stand")
}

// countedLocked is handle's seat's counts from a listing of its runs: each run
// with no write out in the set its status names, and EVERY RUN WITH A WRITE OUT
// where the moves so far leave it ([Coordinator.writing]), whatever set the
// listing found it in or whether it has its row: a create landed after the
// listing, or a delete before it, has its move still to come (countwrite.go).
// See [Coordinator.syncSeat]. Under mu.
func (c *Coordinator) countedLocked(handle string, runs []PendingRun) seatRuns {
	var counts seatRuns
	for _, run := range runs {
		if c.writing[run.TurnID] != nil {
			continue
		}
		part := countOf(run.Status)
		counts.holding += part.holding
		counts.awaiting += part.awaiting
	}
	for _, writes := range c.writing {
		if writes.handle == handle {
			counts.holding += writes.counted.holding
			counts.awaiting += writes.counted.awaiting
		}
	}
	return counts
}

// seatRecountRounds bounds [Coordinator.syncSeat]'s listings.
//
// A recount is read again only when THIS NODE moved the seat while the listing
// ran, so it races only the seat's own transitions, which come in bursts no
// longer than an answered question's: answer recorded, run claimed, claim given
// back, next launch, four store writes at a listing's pace. Eight rounds is that
// burst twice over, so a recount starting at a burst's head still settles; a
// recount giving up may leave counts uncorrected a while (a park and a recorded
// answer trigger no recount). A bound rather than more, because every round
// lists every run the fleet holds.
const seatRecountRounds = 8

// OnCompleted claims the run, collects, accounts, then resumes the loop.
func (c *Coordinator) OnCompleted(ctx context.Context, ev types.ExecutorFinished, trigger *events.Event) error {
	fence, held := c.leaseOf(ev.AgentHandle)
	if !held {
		// NOT THIS NODE'S TO CLAIM: a ROUTING failure, handed back to reach the
		// holder; the poll fires it again while the job's row runs.
		return fmt.Errorf("%w: %w", ErrResumeElsewhere,
			notHeld(ctx, ev.AgentHandle, ev.TurnID, "claiming a completion"))
	}
	// ADMITTED BEFORE THE CLAIM, held through the resume ([TurnGate]). A refusal
	// leaves the run unclaimed and running, and the waiter announces again
	// (reannounce.go); its condition decides what the caller does with this
	// delivery, unless no waiter can announce the job (the clock on its box's
	// host running), when it is handed back whatever the condition (renew.go).
	// An acknowledged one is first written on the job's record
	// ([Coordinator.oweRefused]). What was signalled is read first
	// ([Coordinator.keepRefused]) and handed to the resume, whose seat's budget
	// may hold it ([Coordinator.resumeAndSettle]).
	seen := c.signalsSeen()
	// HELD BY THIS NODE before its gate is asked: the last resume here met a
	// post-claim refusal that lifts at an instant the gate cannot see
	// ([Coordinator.keepRefused]). Refused as the gate refuses; quietly, since
	// the hold was said when taken and announcements repeat it.
	if refusal, held := c.stillRefused(ev); held {
		if c.pastDeadline(ctx, ev) {
			return c.completeAtDeadline(ctx, ev, fence, refusal.Reason)
		}
		log.DebugContext(ctx, "sandbox_completion_held_here",
			"turn_id", ev.TurnID, "agent", ev.AgentHandle, "condition", string(refusal.Condition),
			"until", refusal.Until.UTC().Format(time.RFC3339))
		return fmt.Errorf("%w: %w", ErrResumeUnavailable, &TurnRefused{Refusal: refusal})
	}
	// NO CONVERSATION FOR A COMPLETION: its run holds the conversation's one
	// turn, which this resume continues (ADR-0169).
	adm, err := c.admitTurn(ctx, ev.AgentHandle, "", ResumeCompletion)
	if err != nil {
		if c.pastDeadline(ctx, ev) {
			// PAST ITS TURN'S DEADLINE A REFUSED RESUME IS NOT WAITED FOR
			// ([Coordinator.completeAtDeadline]).
			return c.completeAtDeadline(ctx, ev, fence, err.Error())
		}
		log.InfoContext(ctx, "sandbox_completion_not_admitted",
			"turn_id", ev.TurnID, "agent", ev.AgentHandle, "reason", err.Error(),
			"detail", "this node may not start the turn the completion resumes, so the "+
				"run is left unclaimed for the waiter to announce again, or for this delivery "+
				"where the box's host cannot read the coordination store, here once the "+
				"condition clears or on the seat's next holder")
		c.keepRefused(ev, trigger, refusalOf(err), false, seen)
		return fmt.Errorf("%w: %w", ErrResumeUnavailable, err)
	}
	defer adm.release()
	c.forgetAdmitted(ev)
	err = c.completeAdmitted(adm, ev, trigger, fence, seen)
	if errors.Is(err, errRunMovedOn) {
		// THE RUN MOVED ON BEFORE THIS ATTEMPT COULD END IT and this delivery is
		// SETTLED: the next holder fenced the claim and its recovery reaps and
		// announces the run, or somebody ended it first; a redelivery finds no
		// running job to claim. Handed back it would be a message failure on the
		// control topic (internal/engine's controlOutcome), where this is a
		// routing failure that could send a healthy completion to the poison.
		// The answer route reads it as not this node's to say
		// ([Coordinator.resumeOwedAdmitted]).
		log.InfoContext(ctx, "sandbox_completion_moved_on",
			"turn_id", ev.TurnID, "launch_id", ev.LaunchID, "agent", ev.AgentHandle,
			"detail", "the run moved on to the seat's next holder, or was ended by another "+
				"party, before this node could end it; that party drives what becomes of it, "+
				"and this delivery is settled")
		return nil
	}
	if refusal, refused := waitingRefusal(err); refused {
		// REVERTED ON A WAIT: the refusal is kept for the signal that ends it and
		// handed back by its condition, as the gate's own is, against what was
		// signalled before the gate was asked, so a signal landing during the
		// resume re-fires at once. HELD HERE until its instant, since the gate
		// admitted it and would again: this node's own, never the run's row
		// ([Coordinator.keepRefused]).
		held := !refusal.Until.IsZero()
		if held {
			log.InfoContext(ctx, "sandbox_completion_held_here",
				"turn_id", ev.TurnID, "agent", ev.AgentHandle, "condition", string(refusal.Condition),
				"reason", refusal.Reason, "until", refusal.Until.UTC().Format(time.RFC3339),
				"detail", "the resume met a condition its gate could not see; this node refuses "+
					"the job's announcements until the condition's instant or its signal, and a "+
					"restart or the seat's next holder asks its own models")
		}
		c.keepRefused(ev, trigger, refusal, held, seen)
	}
	return err
}

// completeAdmitted is a completion's claim, collection and resume, under the
// admission [Coordinator.OnCompleted] took for it.
//
// IT TAKES THE ADMISSION, NOT A CONTEXT ([Coordinator.resumeOwedAdmitted]):
// everything from the claim on belongs to the admitted turn. seen is what was
// signalled before the admission was asked, for a claim the hold on the run's
// row refuses ([Coordinator.keepHeldClaim]) and a resume its seat's budget holds
// ([Coordinator.resumeAndSettle]).
func (c *Coordinator) completeAdmitted(adm admitted, ev types.ExecutorFinished,
	trigger *events.Event, fence Fence, seen map[signal]uint64,
) error {
	ctx := adm.ctx
	// NOTHING OF THE JOB IS CLAIMED WHILE THIS NODE GIVES BACK, OR OWES, THE
	// RELEASE OF ITS LAST CLAIM ([Coordinator.claimCompletion]): that release,
	// made again, would give back a claim taken now. The completion comes back on
	// the queue's backoff. NOR WHILE THE SEAT'S RECOVERY PASS RUNS HERE: the
	// claim's gate refuses it (recoverygate.go). Let through, the claim is this
	// node's live attempt until this frame returns.
	run, won, done, err := c.claimCompletion(ctx, ev, fence)
	if done != nil {
		defer done()
	}
	if err != nil {
		return fmt.Errorf("sandbox: claiming %s: %w", ev.TurnID, err)
	}
	if !won {
		// Not this signal's to run: already claimed by a duplicate (poll ticks
		// firing before the claim landed, a redelivery, a failed resume's retry),
		// or the row no longer holds its job running (parked, replaced by the next
		// launch, over), or a newer lease owns it, or its resume is held (a
		// duplicate queued behind the resume that held it, announced again once
		// the hold lifts; kept for the hold here, [Coordinator.keepHeldClaim]).
		// Where no poll can announce it, the delivery is handed back (renew.go).
		log.InfoContext(ctx, "sandbox_completion_already_claimed",
			"turn_id", ev.TurnID, "launch_id", ev.LaunchID)
		c.keepHeldClaim(ctx, ev, trigger, fence, seen)
		return nil
	}
	// THE CLAIM SETTLES THE JOB from here, so what is kept for its run goes,
	// including a keep for the row's hold ([Coordinator.forgetAdmitted]).
	c.forgetRefused(ev.TurnID)

	// THE JOB WAS LOST WITH ITS HOST ([PendingRun.Lost]) and this completion is
	// the waiter's announcement of that, or the collection below finds it lost
	// with its box's host ([Coordinator.loseWithHost]): what it left is no result.
	// Its round is carried on, resumed HERE ALONE under this frame's admission: a
	// helper between this and resumeOwedAdmitted would be a road to a turn
	// internal/node's admission guard refuses (ADR-0063).
	lost := run.Lost != nil
	var result Result
	if !lost {
		var collected bool
		run, result, collected, err = c.collectClaimed(ctx, run)
		switch {
		case !collected && run.Lost != nil:
			lost = true
		case !collected:
			return err
		}
	}
	if lost {
		var carry *lostCarry
		carry, err = c.carryOnLost(ctx, run)
		if err != nil || carry == nil {
			return err
		}
		_, err = c.resumeAndSettle(adm, carry.run, carry.report, false, trigger, carry.outcome, seen)
		return err
	}

	if run.asked() {
		return c.park(ctx, run, result)
	}

	// THE ERROR IS THIS ROUTE'S WHOLE ANSWER. A completion is handed back by
	// NAKing, which spends one of the broker's own 25 deliveries, so the retry is
	// bounded where it stands; an answer needs a bound that stops SHORT of the
	// dead-letter boundary because it spends a person's reply
	// ([AnswerDeliveryReserve], off the message's own count, which a per-process
	// ceiling cannot match across handoffs). Every disposition returning an error
	// is the retry, every other an ending.
	_, err = c.resumeAndSettle(adm, run, runReport(run, result), result.Success, trigger, runOutcome{
		DeliveredRefs: result.DeliveredRefs, DeliveredRefsElided: result.DeliveredRefsElided,
		Pushed: result.Pushed, PushedElided: result.PushedElided, PushedUnread: result.PushedUnread,
		InputTokens: result.InputTokens, OutputTokens: result.OutputTokens,
		CacheReadTokens: result.CacheReadTokens, CacheWriteTokens: result.CacheWriteTokens,
		Engine: run.Launch.Bridged.Plus(EngineSpend{Aux: result.Condensed}),
		Ended: RunEnding{Text: result.Text, Error: result.Error, Event: result.Event,
			Limits: result.Limits, NoLimitExit: result.NoLimitExit, Success: result.Success, At: run.Launch.CollectedAt, Refused: requestRefused(run, result)},
	}, seen)
	return err
}

// collectClaimed reads a claimed run's job back and records its result, charge
// and launch record on the claimed row, so a handed-back claim carries them.
// collected is false where the box could not be read (err is
// [Coordinator.collectFailed]'s answer), or where only the box's host kept it
// unread for the host grace, when the run comes back lost with that host
// ([Coordinator.loseWithHost]). One collection serves a resume and an ending at
// the turn's deadline ([Coordinator.completeAtDeadline]), so an unresumed run is
// still charged and recorded for what its job did.
func (c *Coordinator) collectClaimed(ctx context.Context, run PendingRun) (PendingRun, Result, bool, error) {
	// THE CLAIM AS THE READ LEAVES IT, marked once its box answered
	// ([Coordinator.collect]): every later hand-back carries it.
	run, result, ended, err := c.collect(ctx, run)
	if err != nil {
		// ONE VERDICT ON THE READ'S FAILURE ([collectVerdict]), acted on as it is,
		// never re-judged at a later clock reading. A read only the box's host
		// kept from it for the host grace is a job lost with that host, unless
		// this node itself cut the collection short.
		verdict := c.collectGivenUp(run, err, c.now())
		if verdict.lostWithHost() && !errors.Is(ctx.Err(), context.Canceled) {
			lost, lostErr := c.loseWithHost(ctx, run, err, verdict)
			return lost, Result{}, false, lostErr
		}
		return run, Result{}, false, c.collectFailedAt(ctx, run, err, verdict)
	}
	// THE JOB'S CALLS STILL IN FLIGHT are waited for on the run's log before
	// anything reads what the job did ([Coordinator.drainCalls]): its session
	// ends to new calls first, and a call its harness left running is recorded,
	// so the resume reads what it committed. A wait this node's stop cut hands
	// the claim back uncounted ([Coordinator.collectFailed]).
	drained, err := c.drainCalls(ctx, run)
	if err != nil {
		return run, Result{}, false, c.collectFailed(ctx, run, err)
	}
	// WHAT THE JOB'S CALLS COST ONCE IT IS OVER is read again where the claim's
	// read cannot be the whole: a run ENDED AT ITS TURN'S CAP made bridge calls
	// until the end, their spend on the record, not the claim, and a drained
	// call wrote its own. The resume reads the calls themselves from the log
	// ([Coordinator.resumeAndSettle]), judging and charging all of them.
	if ended || drained {
		run = c.rereadSpend(ctx, run)
	}
	if ended {
		// NO QUESTION, whatever it asked on its way: a run being stopped is not
		// one a person's answer could continue ([endedAtDeadline]), so it is
		// collected and resumed like any ending, nothing parked.
		run = withoutQuestion(run)
	}

	// WRITTEN BEFORE THE TURN RESUMES, since its reviewer's send-back relaunches
	// on that frame and reads the row: a store that does not take it is a
	// collection to ask again, uncounted, since its silence is no failure of the
	// box's ([ErrStoreSilent]).
	if spent := sessionSpent(run, result); spent != "" {
		sessions := sessionsRanOutOfContext(run, result)
		if err = c.pending.RetireSessions(ctx, run.TurnID, spent, sessions); err != nil {
			return run, Result{}, false, c.collectFailed(ctx, run, storeSilent(ctx, err))
		}
		run.SpentSessions = withSpent(run.SpentSessions, spent, sessions)
	}

	// Carried on the claimed row from here, so a handed-back claim carries every
	// record. First that the box answered, which ends any run of failed
	// collections behind this one ([Release.Collected]).
	run.Collected = true
	// AND WHEN, the first time: the instant a later collection reads the job's
	// ending against ([LaunchRecord.CollectedAt]).
	if run.Launch.CollectedAt.IsZero() {
		run.Launch.CollectedAt = c.now().UTC()
	}
	// AND HOW ITS HARNESS ENDED, as the read just recorded it ([LaunchRecord.HarnessEnd]):
	// the claim was read before that write, and the resume on this claim (a round
	// a replaced sign-in carries on in the box) reads it here. The same for the
	// newest request's size ([LaunchRecord.Context]), the harness's count, its
	// provider and release ([LaunchRecord.counted]: what the copy may have
	// compacted away) and its next-request estimate ([LaunchRecord.nextRequested]),
	// each by the rule its record follows.
	run.Launch.HarnessEnd = run.Launch.HarnessEnd.after(result.HarnessEnd)
	run.Launch.Context = run.Launch.measured(result.LastRequest, run.LaunchID)
	run.Launch.ResumedCount, run.Launch.CountProvider, run.Launch.CountRelease = run.Launch.counted(
		result.ResumedCount, result.CountProvider, result.HarnessVersion)
	run.Launch.setNextRequest(run.Launch.nextRequested(result.NextRequest))
	run.Charged, run.CompanyCharged = c.charge(ctx, run, result)
	run.Launch = c.publishCollected(ctx, run, result)
	return run, result, true, nil
}

// rereadSpend is run with what its job's bridged calls cost the engine read
// again off its row ([LaunchRecord.Bridged]): the run as claimed where the row
// cannot be read or no longer holds the job.
//
// Every append, a session's carry of what it owed included, writes the running
// total onto the job's record ([PendingStore.AppendBridgeCall]), and the resume
// charges the engine spend off this run ([ResumeRequest.Engine]). Not read
// again, a carried call's work (a `delegate`'s workers above all) is in the log
// and charged nowhere, since the next launch resets the record.
func (c *Coordinator) rereadSpend(ctx context.Context, run PendingRun) PendingRun {
	latest, found, err := c.pending.Get(ctx, run.TurnID)
	switch {
	case err != nil:
		// The claim's spend stands; the line says a late call's cost may be missing.
		log.WarnContext(ctx, "sandbox_run_spend_reread_failed",
			"turn_id", run.TurnID, "launch_id", run.LaunchID, "error", err.Error())
	case found && latest.LaunchID == run.LaunchID:
		// THE NEWER TOTAL ([EngineSpend.Newest]): each figure only grows within
		// one session's meter.
		run.Launch.Bridged = run.Launch.Bridged.Newest(latest.Launch.Bridged)
	}
	return run
}

// collectFailed answers a collection that could not read its box back: ask
// again within a bound, then give the run up. Safe because nothing the tail
// writes precedes a successful collection, so the run stays as found and the
// completion is NAK'd. The bound is the waiter's ([MinConnectFailures]
// consecutive attempts spanning [ConnectGiveUp]), counted on the job's record; a
// read that answered ends the run of failures. A gone box ([ErrBoxGone]) is
// settled at once; a collection this node cancelled or whose read never
// started ([errReadUnstarted]) and a step the store did not answer
// ([ErrStoreSilent]) are handed back uncounted; a hand-back the store
// does not answer is kept, never an ending. One verdict per failure
// ([collectVerdict]). See the package doc, "Collection failures".
func (c *Coordinator) collectFailed(ctx context.Context, run PendingRun, cause error) error {
	return c.collectFailedAt(ctx, run, cause, c.collectGivenUp(run, cause, c.now()))
}

// collectFailedAt is [Coordinator.collectFailed] on the verdict its caller
// judged cause by ([Coordinator.carryOnLost]), so the hand-back never decides
// apart from it at a later clock reading.
func (c *Coordinator) collectFailedAt(ctx context.Context, run PendingRun, cause error,
	verdict collectVerdict,
) error {
	var answered *boxAnsweredError
	if errors.As(cause, &answered) {
		// THE BOX ANSWERED before a later step failed ([boxAnsweredError]): it
		// ends the run of failed collections, and the hand-back's own write
		// records how the job's harness ended ([Release.HarnessEnd]).
		run.BoxAnswered, run.BoxHarnessEnd = true, answered.end
	}
	if errors.Is(ctx.Err(), context.Canceled) || errors.Is(cause, errReadUnstarted) {
		log.InfoContext(ctx, "sandbox_collect_interrupted", "turn_id", run.TurnID,
			"error", cause.Error(),
			"detail", "the collection was stopped by this node, not by its box; the claim is "+
				"handed back uncounted and the completion is retried")
		if err := c.unclaimAt(ctx, run, collectUnrevertedDetail, failure{}, nil,
			BudgetWindow{}); err != nil {
			//nolint:nilerr // As below: the run has been ended and announced.
			return nil
		}
		return fmt.Errorf("sandbox: collecting %s was interrupted: %w", run.TurnID, cause)
	}
	if errors.Is(cause, ErrBoxHostSilent) {
		// The box's host did not answer ([ErrBoxHostSilent]), which says nothing
		// about the box: handed back uncounted and acknowledged, the host's waiter
		// announcing again (reannounce.go), unless this node keeps a clock on that
		// host's store ([Coordinator.hostClocked]): its waiter announces nothing
		// through a silent store, so the completion is handed back for the queue's
		// backoff and renewed before its last delivery (renew.go).
		clocked := c.hostClocked(run.TurnID, run.LaunchID)
		log.WarnContext(ctx, "sandbox_collect_host_silent", "turn_id", run.TurnID,
			"node", run.LaunchNode, "store_silent", clocked, "error", cause.Error(),
			"detail", "the node whose host the run's box is on did not answer the collection; "+
				"the claim is handed back uncounted and that node announces the completion again, "+
				"or, where it last found its coordination store silent, the completion is asked "+
				"again on the queue's backoff")
		// A hand-back the store refused ended and announced the run itself; one it
		// did not answer is this node's to repeat ([Coordinator.unclaimAt]).
		if err := c.unclaimAt(ctx, run, collectUnrevertedDetail, failure{}, nil,
			BudgetWindow{}); err != nil || !clocked {
			//nolint:nilerr // Ended and announced where the hand-back was refused, as below; acknowledged
			// otherwise, no clock running on that host's store.
			return nil
		}
		return fmt.Errorf("sandbox: collecting %s: %w", run.TurnID, cause)
	}
	now := verdict.at
	if errors.Is(cause, ErrStoreSilent) {
		// The coordination store did not answer a step the collection needs
		// ([ErrStoreSilent]): handed back uncounted and NAK'd. For this node's own
		// store the NAK suffices: the waiter announces a running unheld row within
		// [reannounceCeilingPolls] polls, inside the delivery budget. Where only
		// the box host's store is silent no waiter announces, so the completion is
		// renewed before its last delivery (renew.go) until the store answers or
		// the bound records the job lost with that host.
		log.WarnContext(ctx, "sandbox_collect_unsettled", "turn_id", run.TurnID,
			"error", cause.Error(),
			"detail", "the coordination store did not answer a step the collection cannot go on "+
				"without; the claim is handed back uncounted and the collection is asked again "+
				"until the store answers, or until the host grace where only the box's host "+
				"finds it silent")
		if err := c.unclaimAt(ctx, run, collectUnrevertedDetail, failure{}, nil,
			BudgetWindow{}); err != nil {
			//nolint:nilerr // As below: the run has been ended and announced.
			return nil
		}
		return fmt.Errorf("sandbox: collecting %s: %w", run.TurnID, cause)
	}
	facts := run.Launch
	attempts := facts.CollectFailures + 1
	since := facts.CollectFailingSince
	if since.IsZero() {
		since = now
	}
	gone := errors.Is(cause, ErrBoxGone)
	if verdict.givenUp {
		log.ErrorContext(ctx, "sandbox_collect_failed", "turn_id", run.TurnID,
			"attempts", attempts, "failing_for_s", now.Sub(since).Seconds(),
			"box_gone", gone, "error", cause.Error())
		c.runSettled(run.TurnID)
		// The job is OVER though collection failed: free the seat and settle the
		// row whatever the network cleanup manages.
		c.settleFailed(ctx, run, types.ExecutorFailureCollect,
			"the executor's launch finished but its box could not be read back, so its "+
				"result is lost; the work it pushed, if any, is on its branch")
		return nil
	}
	log.WarnContext(ctx, "sandbox_collect_retried", "turn_id", run.TurnID,
		"attempt", attempts, "failing_for_s", now.Sub(since).Seconds(), "error", cause.Error(),
		"detail", "the box could not be read back; the claim is handed back and the "+
			"collection is retried until it has failed for the poll's give-up window")
	// COUNTED: the claim did not move the seat's holding count (running and
	// resumed both hold it). A release the store does not answer counts the
	// failure once, when it lands ([Coordinator.unclaimAt]). AND COUNTED ONTO THE
	// RUN OF FAILURES BEHIND IT even where the box answered this read: a step
	// after it was cut by the delivery's deadline, and a collection always
	// outlasting its delivery would otherwise start a new run each time and never
	// be given up ([Release.BoxAnswered]).
	run.BoxAnswered = false
	if err := c.unclaimAt(ctx, run, collectUnrevertedDetail, failure{collectFailed: now}, nil,
		BudgetWindow{}); err != nil {
		//nolint:nilerr // Deliberate, as at every other hand-back: the claim
		// did not go back, so the run has been ended and announced, and a
		// redelivered completion would find nothing to claim.
		return nil
	}
	return fmt.Errorf("sandbox: collecting %s (attempt %d): %w", run.TurnID, attempts, cause)
}

// runOutcome is what a finished run reported about itself, for the resumed
// phase's record and segment charge. The refs are empty where no run finished (a
// person's answer collects nothing); the tokens are the job's either way
// ([ResumeRequest.InputTokens]). Fields mirror [ResumeRequest]'s.
type runOutcome struct {
	DeliveredRefs       []string
	DeliveredRefsElided int
	Pushed              []PushedRef
	PushedElided        int
	PushedUnread        string
	InputTokens         int
	OutputTokens        int
	CacheReadTokens     int
	CacheWriteTokens    int
	Engine              EngineSpend
	Ended               RunEnding
	Asked               *AskedRun
	Lost                *LostRun
}

// parkedEngineSpend is what the engine spent on a parked job outside every
// segment, for the resume its answer drives (bridged calls and condensing the
// parking collection), off the job's own record since nothing was collected.
func parkedEngineSpend(run PendingRun) EngineSpend {
	facts := run.Launch
	return facts.Bridged.Plus(EngineSpend{Aux: facts.Condensed})
}

// collect reconnects, reads the result, and pauses the box rather than tearing
// it down: a relaunch continues in it, a parked run waits in it, a retried
// resume reads it again; the pause reaper bounds it ([PendingRun.HeldSince]).
// ended reports a run its turn's wall-clock cap ended ([PendingRun.Deadline]):
// the read ends the job first, its bridge session closed before
// ([CoordinatorOptions.Ended]), and the result says it was cut off. The claim is
// marked the moment the box answered ([PendingRun.BoxAnswered]). See the
// package doc, "Collecting a box".
func (c *Coordinator) collect(ctx context.Context, run PendingRun) (PendingRun, Result, bool, error) {
	if c.ended != nil && run.PastDeadline(c.now()) {
		c.ended(run.TurnID)
	}
	// THE BOX IS READ WHERE IT IS ([Coordinator.boxResult]); the rest is this
	// node's work under the turn the result is fitted for.
	result, ended, err := c.boxResult(ctx, run, BoxReadCollect)
	if err != nil {
		return run, Result{}, false, err
	}
	// THE BOX ANSWERED, ending the run of failed collections behind this one
	// whatever a later step does ([Release.BoxAnswered], [Release.HarnessEnd]).
	// Marked here and not once this read returns: reading the pushes follows, and
	// a read cut short there is one of those steps. The delivery knows it, so
	// it reads no row for what the host's store says (renew.go).
	run.BoxAnswered, run.BoxHarnessEnd = true, result.HarnessEnd
	readTheBox(ctx)
	if ended {
		result = endedAtDeadline(result, run.Deadline, run.Question)
	}
	c.warnNotesUnasked(ctx, run, result)
	// WHAT IT PUSHED, read here, where the result is used: the push record is
	// Gitlet's, which the seat's node reaches ([RunPushes]).
	if result, err = c.readPushes(ctx, run, result); err != nil {
		return run, Result{}, false, err
	}
	// HELD TO THE RECORD'S BOUND here, where every collected result enters,
	// so the launch's record, the park and the run's report carry one text.
	return run, c.fitText(ctx, run, result), ended, nil
}

// noteReader is a [Runner] whose harness takes a person's notes to its run's turn
// between its own steps (internal/sandbox/executor's Runner). Optional: a runner
// that reads none says nothing about notes.
type noteReader interface{ ReadsNotes() bool }

// warnNotesUnasked says, as steer_reader_never_ran, that a run which succeeded
// never asked for its turn's notes ([RunSteer.Pulled]), telling a run whose
// harness's reading was switched off from one with nothing to read.
//
// RELIABLE for a harness that reads notes: its reader asks before the run can
// succeed. A run that succeeded without asking had its reading disabled, as
// its runner says what can do that ([notesOffWhy]), and every note waited for
// its review. Only for a run whose record names its seat
// ([LaunchRequest.SteerSeat]); logged on the collecting node.
func (c *Coordinator) warnNotesUnasked(ctx context.Context, run PendingRun, result Result) {
	if !result.Success || len(run.Steer.Seat) == 0 || run.Steer.Pulled == run.LaunchID {
		return
	}
	manager := c.mgr()
	if manager == nil {
		return
	}
	runner, err := manager.RunnerFor(run.Harness)
	if err != nil {
		return
	}
	if reader, ok := runner.(noteReader); !ok || !reader.ReadsNotes() {
		return
	}
	detail := "the run succeeded and its harness never asked for its turn's notes, so a note sent " +
		"while it worked waited for its review instead of reaching it between its steps. Something in " +
		"the box switched the harness's reading off"
	if why := notesOffWhy(runner); why != "" {
		detail += ": " + why
	}
	log.WarnContext(ctx, "steer_reader_never_ran", "turn_id", run.TurnID,
		"launch_id", run.LaunchID, "agent", run.Harness, "detail", detail)
}

// readBox is the part of a collection touching the box, made on its host's node:
// reconnect, end the job where its cap passed and it still works, read and floor
// the result, bound transcript and refs ([boundBox]), take the session
// ([Coordinator.takeCapture]), and for a completion record how the harness ended
// ([LaunchRecord.HarnessEnd]) and pause the box. ENDED is the run its cap ended.
// A STOPPED RUN'S READ ([BoxReadStop]) ENDS ITS JOB FIRST ([Coordinator.endJob]):
// its script packs the session as its own exit does, so it is final, and PAUSES
// NOTHING (the ending reclaims the box). So does a cap-passed job's collection:
// packed beside a writing harness the session was never final. That the cap ended
// the job is recorded before it is asked or killed ([LaunchRecord.EndedAtDeadline]).
func (c *Coordinator) readBox(ctx context.Context, run PendingRun, read BoxRead) (Result, bool, error) {
	manager := c.mgr()
	box, runner, err := manager.Reconnect(ctx, Placement(run.Placement), run.SandboxID, run.Harness)
	if err != nil {
		return Result{}, false, err
	}
	// WHETHER THE JOB IS OVER: a stop's is ended first, and a collection's that
	// still works or whose poll is silent once its cap passed (by this node's clock
	// or an earlier recorded end, [LaunchRecord.EndedAtDeadline]). Asked first, so a
	// job that finished by itself is collected normally, not swept or called cut
	// off; only one still working after its end is packed non-final ([readCapture]).
	// ENDED marks the result cut off at the cap: a job SEEN working past it (poll
	// said running, or its script heard the end request, [Coordinator.endJob]) and
	// RECORDED when seen, before it is asked or killed, since the kill can take the
	// script before it records the request. A silent poll shows nothing, and never
	// read it off whether the job is over after its end (a stopped job is over too).
	capped := read == BoxReadCollect && (run.Launch.EndedAtDeadline || run.PastDeadline(c.now()))
	ended, over := capped && run.Launch.EndedAtDeadline, true
	sawWorking := func() {
		if !ended {
			ended = true
			c.markEndedAtDeadline(ctx, run)
		}
	}
	switch {
	case read == BoxReadStop:
		over = c.endJob(ctx, run, box, runner, nil)
	case capped:
		state, pollErr := runner.Poll(ctx, box, run.Handle())
		if pollErr == nil && state == JobRunning {
			sawWorking()
		}
		if pollErr != nil || !state.Over() {
			over = c.endJob(ctx, run, box, runner, sawWorking)
		}
	}
	result, err := runner.Collect(ctx, box, run.Handle(), c.answerBudget)
	if err != nil {
		return Result{}, false, err
	}
	// A MINTING JOB'S SESSION IS BOUND AS IT IS COLLECTED (minted.go).
	if run, result, err = c.bindCollected(ctx, run, runner, result); err != nil {
		return Result{}, false, err
	}
	// THE JOB'S OWN RECORD THAT A REQUEST ENDED IT ([Result.JobEnd]) is the same
	// sight where the job's record did not take this read's: only the cap asks a
	// collected job to end (stops and losses are endings no completion collects),
	// so the script that heard it was still working. It covers a request whose
	// answer was lost and a repeat after a record that did not land; otherwise
	// the run would be parked on a question asked on its way out
	// ([Coordinator.collectClaimed]).
	if capped && result.JobEnd != "" {
		ended = true
	}
	// WHAT THE RUN SAYS IT SPENT IS NEVER A REFUND — see [Result.usageFloored].
	var floored bool
	if result, floored = result.usageFloored(); floored {
		log.WarnContext(ctx, "sandbox_usage_negative",
			"turn_id", run.TurnID, "sandbox_id", run.SandboxID,
			"detail", "the executor reported a negative token count or cost; it "+
				"is read as nothing spent rather than subtracted from the seat's budget")
	}
	result = boundBox(result)
	// THE JOB'S SESSION, TAKEN HERE on the box's host before the box is paused or
	// reclaimed (a paused box cannot be read), whichever node asked, spooled
	// sealed on this node and KEPT off the read from the moment its take is
	// spooled (session.go), never behind what the read does after. Recording the
	// harness end, pausing and recording the pause each wait on a store or box
	// backend, and a read of a box this node hosts has no deadline: a keep started
	// after the read waited on all of it, no capture budget could bound it, and
	// a turn's end could read a capture still to be kept as abandoned. The keep
	// reads the spool and the claim's copy of the run only. A stop's read is the
	// last anything makes of the box.
	at := readCapture(read, result, over)
	// WHETHER IT RAN OUT OF CONTEXT is judged on the run as its collection
	// records it ([asCollected]): a run its cap ended drops its parting
	// question, so judged on the claimed row its session would be kept and then
	// retired by the same collection.
	at.exhausted = ranOutOfContext(asCollected(run, result, ended))
	at.refused = requestRefused(asCollected(run, result, ended))
	// THE JOB'S MAIN THREAD, read once on the box's host before the take
	// (mainthread.go): the harness's count of the session and its provider (an
	// OpenCode count's reply), and its next-request estimate, kept on the result
	// and written with how the harness ended after the take; and the receipts of
	// its hooks' serves, settled BEFORE the take, a verdict the store does not
	// answer handing the read back box-answered ([boxAnsweredError]).
	asked := c.asksSummary(runner)
	reading := c.readMainThread(ctx, run, box, runner, read, at, result, asked)
	result.ResumedCount, result.CountProvider, result.NextRequest = reading.Count, reading.Provider, reading.Next
	if err := c.settleReceipts(ctx, run, reading.Receipts, read); err != nil {
		return Result{}, false, &boxAnsweredError{err: err, end: at.harnessEnd()}
	}
	// ITS NEWEST SUMMARY, which the take keeps beside the session (summary.go).
	at.summary = c.foundSummary(ctx, run, runner, asked, reading.Summary)
	spooled, kept := c.takeCapture(ctx, run, box, runner, at)
	result.Kept = kept
	c.keepLater(ctx, spooled)
	// HOW THE JOB'S HARNESS ENDED, drawn by the line the take was: carried back
	// with the result, and recorded by a collection's read whether or not the
	// take kept a capture ([LaunchRecord.HarnessEnd], harnessend.go), after the
	// take so a slow store costs neither the take nor the pause their time. A
	// stop's and a loss's read record nothing: nothing carries the round on.
	result.HarnessEnd = at.harnessEnd()
	if read != BoxReadCollect {
		return result, ended, nil
	}
	// AND the copy's newest request size, the harness's count with its provider
	// and release, and its next-request estimate, in the same write.
	c.markHarnessEnd(ctx, run, EndRead{End: result.HarnessEnd, Request: result.LastRequest,
		Count: result.ResumedCount, CountProvider: result.CountProvider, CountRelease: result.HarnessVersion,
		Next: result.NextRequest})
	if err := box.Pause(ctx); err != nil {
		log.WarnContext(ctx, "sandbox_pause_failed", "turn_id", run.TurnID, "error", err.Error())
	} else if err := c.markBoxPaused(ctx, run); err != nil {
		// WARN, NOT FAIL: refusing a collected result over a timestamp would
		// throw the run away. A row that parks names a held box whatever this
		// write did, and [PendingRun.HeldSince] dates the wait from the park.
		log.WarnContext(ctx, "sandbox_pause_record_failed", "turn_id", run.TurnID, "error", err.Error(),
			"detail", "the pause instant was not recorded; a run that parks on a question is "+
				"expired from its park instead, and one that resumes settles its own box")
	}
	return result, ended, nil
}

// hookVerdictWait is the reason one verdict on a job's hooks' serves is given
// ([Coordinator.settleReceipts]): a store's slow moment, ten seconds, as
// every row write's reason; [Coordinator.rowContext] adds the row's crossing
// ([BoxReadBudget] counts both for a read that asks receipts).
const hookVerdictWait = 10 * time.Second

// hookVerdictRetry spaces the attempts at a verdict within its room: a tenth of
// the reason, so a store's brief refusal is retried several times before the
// read is handed back.
const hookVerdictRetry = time.Second

// settleReceipts settles run's job's serves by what the job's record showed of
// them ([PendingStore.SettleHook]) and publishes every note and wake a serve
// handed over ([CoordinatorOptions.SteersConfirmed]) on every read of a job
// whose runner confirms off that record ([ThreadTraits.Receipts]).
// A verdict the store does not answer within its room is [ErrStoreSilent],
// returned before the read's take; the package doc's "A HOOK'S NOTE" says why.
func (c *Coordinator) settleReceipts(ctx context.Context, run PendingRun, receipts TakeReceipts,
	read BoxRead) error {
	if !c.threadTraits(run.Harness).Receipts {
		return nil
	}
	notes, asked := run.Steer.Notes, askedTakes(run)
	switch {
	case len(asked) == 0:
	case !receipts.decided():
		log.InfoContext(ctx, "steer_receipt_unread", "turn_id", run.TurnID, "launch_id", run.LaunchID,
			"point", string(read), "takes", len(asked), "detail", receipts.Why,
			"outcome", "the job's session decided none of its hooks' serves, which stay unconfirmed")
	default:
		answered, err := c.settleVerdict(ctx, run, receipts)
		if err != nil {
			log.WarnContext(ctx, "steer_receipt_unsettled", "turn_id", run.TurnID, "launch_id", run.LaunchID,
				"point", string(read), "error", err.Error(),
				"detail", "the coordination store did not answer the verdict on the job's served notes "+
					"within its room; the read is handed back before its take, and the read made again "+
					"finds the verdict wherever it landed")
			return storeSilent(ctx, fmt.Errorf("sandbox: settling run %s's hooks' serves by its job's "+
				"session: %w", run.TurnID, err))
		}
		notes = withAnswered(notes, answered)
		warnCancelUnheard(ctx, run, receipts.Absent)
	}
	if c.steersConfirmed == nil {
		return nil
	}
	var confirmed []RunNote
	for _, n := range notes {
		if (n.For == "" || n.IsWake()) && readByServeOf(n, run.LaunchID) {
			confirmed = append(confirmed, n)
		}
	}
	c.steersConfirmed(ctx, run, confirmed)
	return nil
}

// settleVerdict asks the store for the verdict on run's job's serves, again
// [Coordinator.verdictRetry] after each error, until its room (the reason and the
// row's crossing, [Coordinator.rowContext]) is spent. Each attempt decides on the
// record as it stands, so one finding an earlier write landed answers the same.
func (c *Coordinator) settleVerdict(ctx context.Context, run PendingRun, receipts TakeReceipts) ([]RunNote,
	error) {
	row, cancel := c.rowContext(ctx, c.verdictWait)
	defer cancel()
	settle := HookSettle{Launch: run.LaunchID, Held: receipts.Held, Restarted: receipts.Restarted,
		Absent: receipts.Absent}
	for {
		answered, err := c.pending.SettleHook(row, run.TurnID, settle)
		if err == nil {
			return answered, nil
		}
		retry := time.NewTimer(c.verdictRetry)
		select {
		case <-row.Done():
			retry.Stop()
			return nil, err
		case <-retry.C:
		}
	}
}

// withAnswered is notes with each note a verdict answered in its place, by id.
func withAnswered(notes, answered []RunNote) []RunNote {
	out := slices.Clone(notes)
	for _, a := range answered {
		if i := slices.IndexFunc(out, func(n RunNote) bool { return n.ID == a.ID }); i >= 0 {
			out[i] = a
		}
	}
	return out
}

// warnCancelUnheard says, as steer_cancel_unheard, each serve to run's job no
// cancel lost that its session proved absent: the harness gave an answer up
// and the bridge heard no cancel of it.
func warnCancelUnheard(ctx context.Context, run PendingRun, absent []string) {
	for _, n := range run.Steer.Notes {
		for _, h := range n.Serves {
			if h.Launch == run.LaunchID && h.Lost == "" && slices.Contains(absent, h.Take) {
				log.WarnContext(ctx, "steer_cancel_unheard", "turn_id", run.TurnID, "launch_id", run.LaunchID,
					"take", h.Take, "note_id", n.ID,
					"detail", "the job's session never took the note its hook was served, and the bridge "+
						"heard no cancel of the call: a release that stops sending cancels shows so")
			}
		}
	}
}

// markBoxPaused records on run's row that its box is paused
// ([PendingStore.MarkBoxPaused]) in the room a read leaves it
// ([Coordinator.pauseRoom]): a teardown's grace plus the row's crossing
// ([Coordinator.rowContext]). HELD TO ITS ROOM, NEVER TO THE READ'S CONTEXT
// ALONE: a read of a box this node hosts runs under its admission, which has no
// deadline, and an unanswering store would hold the read and the turn's resume
// indefinitely. A record so cut is the missing stamp the read already warns of.
func (c *Coordinator) markBoxPaused(ctx context.Context, run PendingRun) error {
	row, cancel := c.rowContext(ctx, discardGrace)
	defer cancel()
	return c.pending.MarkBoxPaused(row, run.TurnID, c.now())
}

// markEndedAtDeadline records on run's job's record that its collection saw it
// working past its turn's cap and is ending it ([PendingStore.MarkEndedAtDeadline]):
// every later collection reads it as the cap's verdict ([Coordinator.readBox]).
//
// UNDER A CONTEXT OF ITS OWN ([Coordinator.detachedRecord]), like the kill that
// may follow: the read can be cut short as it ends the job, and this record is
// what tells a later collection the job was one the cap stopped. A record that
// does not land is said and costs only that later sight: the job's own record
// of the request ([Result.JobEnd]) says so where its script left one.
func (c *Coordinator) markEndedAtDeadline(ctx context.Context, run PendingRun) {
	markCtx, cancel := c.detachedRecord(ctx)
	defer cancel()
	if _, err := c.pending.MarkEndedAtDeadline(markCtx, run.TurnID, run.LaunchID); err != nil {
		log.WarnContext(ctx, "sandbox_deadline_end_unrecorded", "turn_id", run.TurnID,
			"launch_id", run.LaunchID, "error", err.Error(),
			"detail", "the run's record does not say that its turn's cap ended the job this collection "+
				"saw working; this collection says so, and one made again after it reads it off the "+
				"job's own record of the request, where its script left one")
	}
}

// endedAtDeadline is what a run its turn's cap ended reports: what it said up to
// then, marked as cut off. Never a success, since it did not get to say it was
// done, and parked on no question ([withoutQuestion]). BUT THE QUESTION IT
// ASKED IS SAID: `asked` (redacted and bounded where recorded) is named in the
// note, so the judging turn and the seat's next turn know a person's answer was
// what it waited for; dropped with the park it would leave no trace.
func endedAtDeadline(result Result, deadline time.Time, asked string) Result {
	why := fmt.Sprintf("the run was ended at its turn's wall-clock cap, %s, before it "+
		"finished; what it had done by then is collected", deadline.UTC().Format(time.RFC3339))
	if asked != "" {
		why += ". It had asked: " + asked
	}
	result.Success = false
	if result.Error != "" {
		why += ". It had reported: " + result.Error
	}
	result.Error = why
	return result
}

// readPushes sets what a collected run pushed to Gitlet on its result, from the
// push record ([RunPushes]), for a run given a repository to push to (a read-only
// run's grants say it pushed none). THE FIELDS ARE THE COORDINATOR'S, cleared
// first (a runner's would be its own account). AN UNREADABLE RECORD DOES NOT FAIL
// THE COLLECTION (retrying would hold a finished run's seat and box while Gitlet
// is down): it is SAID ([Result.PushedUnread]), not "none"; except a read this node
// cancelled, the collection interrupted ([Coordinator.collectFailed]). AND THE CHANGES
// ([RunPushes.Changes]), read at the record's position ([PushRecord.At]) for one
// barrier: refs are named by their live change, and changes the run asked to open
// are opened only for a run that FINISHED (succeeded, asked nothing); a question's
// run is carried on by another job (ADR-0068), a failed one the seat decides
// (code_open_change). A failed read says each ask was not opened, and why.
func (c *Coordinator) readPushes(ctx context.Context, run PendingRun, result Result) (Result, error) {
	result.Pushed, result.PushedElided, result.PushedUnread, result.Opened = nil, 0, "", nil
	if len(Pushable(run.Repos)) == 0 {
		return result, nil
	}
	record, err := c.pushes.Pushed(ctx, run)
	switch {
	case err != nil && errors.Is(ctx.Err(), context.Canceled):
		return Result{}, fmt.Errorf("sandbox: reading what run %s pushed: %w", run.TurnID, err)
	case err != nil:
		// Redacted and bounded where every collected result is ([Coordinator.fitResult]).
		result.PushedUnread = err.Error()
		result.Opened = asked(run, unopenedUnread(err.Error()))
		log.WarnContext(ctx, "sandbox_pushes_unread", "turn_id", run.TurnID, "launch_id", run.LaunchID,
			"error", err.Error(),
			"detail", "what the run pushed to Gitlet could not be read; the run resumes "+
				"saying its pushes are unknown, never that it pushed nothing")
		return result, nil
	}
	result.Pushed, result.PushedElided = record.Refs, record.More
	finished := result.Success && !run.asked()
	if len(record.Refs) == 0 && (!finished || len(asked(run, "")) == 0) {
		// NOTHING TO NAME AND NOTHING TO OPEN: no read.
		return withUnopened(run, result, finished), nil
	}
	named, opened, err := c.pushes.Changes(ctx, run, record, finished)
	switch {
	case err != nil && errors.Is(ctx.Err(), context.Canceled):
		return Result{}, fmt.Errorf("sandbox: reading the changes run %s delivered to: %w", run.TurnID, err)
	case err != nil:
		log.WarnContext(ctx, "sandbox_changes_unread", "turn_id", run.TurnID, "launch_id", run.LaunchID,
			"error", err.Error(),
			"detail", "the changes the run's branches head could not be read; its pushed refs name none, "+
				"and no change it asked to open was opened")
		if finished {
			result.Opened = asked(run, unopenedNoChanges(err.Error()))
			return result, nil
		}
		return withUnopened(run, result, finished), nil
	}
	result.Pushed = named
	if finished {
		result.Opened = opened
		return result, nil
	}
	return withUnopened(run, result, finished), nil
}

// withUnopened is result with every change run asked to open said unopened (why,
// for a run that did not finish), or as is for one that finished and opened none.
func withUnopened(run PendingRun, result Result, finished bool) Result {
	switch {
	case finished:
	case run.asked():
		result.Opened = asked(run, unopenedAsked)
	default:
		result.Opened = asked(run, unopenedFailed)
	}
	return result
}

// charge post-accounts a collected run's tokens, ONCE per launch, and reports
// whether the run's spend is on the counter.
//
// The charge sits in the retried tail (a failed resume hands the claim back and the
// retry collects the same job again), so the run's row records it and a retry finding
// it charges nothing (memory would be lost to a retry elsewhere or after a restart).
// THE RECORD RIDES ON THE RELEASE, the only write letting a retry reach this charge
// ([PendingRun.Charged]). A CHARGE THE COUNTER NEVER ANSWERED IS NOT
// RECORDED (re-offering can only trip a cap early). One that reached the COMPANY and
// failed on the seat is recorded as that ([PendingRun.CompanyCharged]), the retry
// finishing the seat's alone. One OVER A CAP is recorded like any other
// ([Accountant]): "unrecorded" would charge an over-cap run once per retry.
func (c *Coordinator) charge(ctx context.Context, run PendingRun, result Result) (charged, companyCharged bool) {
	tokens := result.InputTokens + result.OutputTokens
	if run.Charged {
		log.InfoContext(ctx, "sandbox_charge_already_recorded",
			"agent_id", run.AgentID, "turn_id", run.TurnID, "tokens", tokens)
		return true, run.CompanyCharged
	}
	if c.account == nil || tokens == 0 {
		return false, run.CompanyCharged
	}
	over, err := c.account.Charge(ctx, run.AgentID, run.AgentHandle, tokens, run.CompanyCharged)
	var partial *coord.SeatUncountedError
	switch {
	case errors.As(err, &partial):
		log.WarnContext(ctx, "sandbox_accounting_partial", "turn_id", run.TurnID,
			"agent_id", run.AgentID, "tokens", tokens, "error", err.Error(),
			"detail", "the company's counter holds this run and the seat's does not; a "+
				"retry of the completion records the seat's share alone")
		return false, true
	case err != nil:
		log.WarnContext(ctx, "sandbox_accounting_failed", "turn_id", run.TurnID, "error", err.Error())
		return false, run.CompanyCharged
	}
	if over {
		log.WarnContext(ctx, "sandbox_spend_over_budget",
			"agent_id", run.AgentID, "turn_id", run.TurnID, "tokens", tokens)
	}
	// RECORDED EITHER WAY, because the counters moved either way.
	return true, true
}

// publishCollected publishes the collected launch's own record
// (`executor_collected`), ONCE per launch, and returns the job's record as it now
// stands. THE LAUNCH IS THE EXECUTOR'S SPEND: the executor publishes nothing when
// it suspends and its resumed phase has no tokens of its own, so here every token
// reaches the spend rollup and daily usage and the activity transcript is kept, for
// every collected launch (finished or asking). A RECORD OF ITS OWN, NOT A PHASE
// ([types.ExecutorCollected]). ONCE, like the charge: it rides the release
// ([LaunchRecord.Published]), a refused publish is NOT recorded so a retry re-offers
// it, and the one remaining duplicate (reported failed, landed) errs like the
// charge. Telemetry never fails the tail: a failure is logged.
func (c *Coordinator) publishCollected(ctx context.Context, run PendingRun, result Result) LaunchRecord {
	facts := run.Launch
	if facts.Published {
		log.InfoContext(ctx, "sandbox_collected_already_published",
			"turn_id", run.TurnID, "launch_id", run.LaunchID)
		return run.Launch
	}
	rec := collectedRecord(run, facts, result, c.pinnedWindow(run.Harness, facts.Model), c.now())
	ev := events.New(rec, events.TraceContext{TraceID: run.TraceID, ParentSpanID: run.SpanID})
	ev.Source = run.Role
	if err := c.queue.Publish(ctx, topics.Event(ev.Type), ev); err != nil {
		log.WarnContext(ctx, "sandbox_collected_publish_failed",
			"turn_id", run.TurnID, "launch_id", run.LaunchID, "error", err.Error())
		return run.Launch
	}
	// MEASURED WITH THE RECORD, under its guard: once per launch.
	c.meterCarry(facts, result, rec.SessionNotContinued)
	facts.Published = true
	return facts
}

// collectedRecord is a collected launch's own record.
//
// Everything on it is the LAUNCH's (tokens, cache share, cost, report,
// deliveries, transcript, why it did not finish); nothing is the executor's,
// which is on the resumed phase record, so the two never sum past the turn's
// spend. Its clock is the store's instant for the launch and this node's for the
// collection, so DURATION includes the waiter's poll interval.
func collectedRecord(run PendingRun, facts LaunchRecord, result Result, pinned int,
	collected time.Time) types.ExecutorCollected {
	// THE SESSION AND RELEASE THE BOX NAMED are held to the shapes a harness gives
	// them, whatever the runner or host made of them: each is the box's word, and
	// one of any other shape is none, never cut (a shortened id names another).
	session, release := result.SessionID, result.HarnessVersion
	if !ValidSessionID(session) {
		session = ""
	}
	if !ValidRelease(release) {
		release = ""
	}
	rec := types.ExecutorCollected{
		Agent: run.AgentID, RoleName: run.Role,
		TurnID: run.TurnID, WorkKey: run.WorkKey,
		// The executor round that launched it (a round can launch more than once).
		Iteration: facts.Iteration, LaunchID: run.LaunchID,
		// The entry and model the job launched on: the chain member that took the
		// run, which a fallback makes other than the head.
		Model: facts.Model, ProviderKey: facts.ProviderKey,
		// Redacted again at the publish, the boundary the record leaves by: a
		// runner that forgot would put a box's credentials in the event store.
		Response:           redact.Secrets(result.Text),
		ActivityTranscript: redact.Secrets(result.Transcript),
		// What the bound left out of the transcript's middle, so a screen says it
		// is not the whole log ([boundTranscript]).
		ActivityTranscriptElidedLines: result.TranscriptElidedLines,
		ActivityTranscriptElidedBytes: result.TranscriptElidedBytes,
		InputTokens:                   result.InputTokens,
		OutputTokens:                  result.OutputTokens,
		TotalTokens:                   result.InputTokens + result.OutputTokens,
		CacheReadTokens:               result.CacheReadTokens,
		CacheWriteTokens:              result.CacheWriteTokens,
		// The launch's model turns as its harness reported them: the calls every
		// spend figure counts this record as (tokens.LaunchCalls).
		HarnessTurns:  result.Turns,
		WorkItem:      run.WorkItem,
		Harness:       run.Harness,
		SandboxID:     run.SandboxID,
		CostUSD:       result.CostUSD,
		DeliveredRefs: result.DeliveredRefs,
		// How many more it reported than listed ([Result.DeliveredRefsElided]).
		DeliveredRefsElided: result.DeliveredRefsElided,
		// AND ITS PUSHES, on the launch's own record: an answer's resume collects
		// nothing, so a parked job's pushes are here or nowhere ([Result.Pushed]).
		PushedRefs:       PushedRecords(result.Pushed),
		PushedRefsElided: result.PushedElided,
		PushedRefsUnread: result.PushedUnread,
		ConversationKey:  run.ConversationKey,
		// THE SESSION THE LAUNCH RAN, as its script recorded it (not the one it was
		// asked to continue, when it fell back to a new one), the harness's
		// release, and what the read took of it to keep or why none, so a later
		// launch knows whether it may continue it. The upload and record are made
		// off the read, from the moment its take spooled the session.
		HarnessSession: session,
		HarnessVersion: release,
		// AND WHICH BRIEF the launch handed it, whether the run continued the
		// session it was asked to and, where not, why its script started its own
		// with the fresh brief (the started phase record carries the fresh brief,
		// which a continuing run never read). THE REASON IS HELD TO ITS BOUND
		// HERE, redacted then cut ([BoundFellBack]): it is a line of a box file the
		// run's own agent can write as long as the engine reads whole.
		Brief:                   string(facts.Brief),
		HarnessSessionContinued: result.Continued,
		SessionNotContinued:     finalNotContinued(run, facts, result),
	}
	// The window the run was pinned to, as its runner pins the model its launch
	// recorded ([windowPinner]), none for a runner that pins none.
	rec.ContextWindowTokens = pinned
	if kept := result.Kept; kept != nil {
		rec.SessionTakenBytes = kept.Bytes
		// AND WHY IT WAS NOT KEPT, at the executor log's bound for the reason: it
		// quotes the take's refusal, which can quote the box's archive.
		rec.SessionNotKept = boundReason(kept.NotKept, executorkeep.UnkeptReasonMaxBytes)
		rec.SessionNotKeptLoss = kept.Loss
	}
	rec.StartedAt = facts.StartedAt.UTC()
	if took := collected.Sub(facts.StartedAt); took > 0 {
		rec.DurationMS = int(took / time.Millisecond)
	}
	switch {
	case run.Stop != nil:
		// ENDED BY A PERSON, NOT FAILED ([Coordinator.StopSeat]): marked failed
		// it would list with the runs that broke. Who stopped it is the note.
		rec.Notes = "stopped by " + run.Stop.By + " before it finished"
	case run.Lost != nil && !run.asked():
		// LOST, NOT FAILED: its host cut it off mid-step ([JobLoss]), saying
		// nothing about the run, and its round is carried on.
		rec.Notes = lostNote(*run.Lost)
	case run.asked():
		// STOPPED, NOT FAILED: parked, to be resumed by its answer. The question
		// is the note, redacted where the ask was recorded.
		rec.Notes = "stopped to ask: " + run.Question
	case !result.Success:
		rec.Failed = true
		rec.Error = redact.Secrets(result.Error)
		if rec.Error == "" {
			rec.Error = "the executor's launch did not succeed and gave no reason"
		}
		rec.ErrorKind = types.ExecutorErrorLaunchFailed
		switch {
		case ranOutOfContext(run, result):
			rec.ErrorKind = types.ExecutorErrorContextExhausted
		case requestRefused(run, result):
			rec.ErrorKind = types.ExecutorErrorRequestRefused
		case result.OutOfMemory && Placement(run.Placement).OwnsMemory():
			// A direct box's kills are the engine host's domain's, recorded
			// below and never the run's ending.
			rec.ErrorKind = types.ExecutorErrorOutOfMemory
		case result.RoundCapped:
			rec.ErrorKind = types.ExecutorErrorMaxTurns
		}
	}
	// THE KERNEL'S KILLS FOR MEMORY, on any ending, so a run that survived a
	// killed command is evidence of its box's size too.
	rec.MemoryKills, rec.MemoryBytes = result.MemoryKills, result.MemoryBytes
	return rec
}

// park announces the question, records it, and settles the box per the pause policy.
// EVERY STEP THAT CAN FAIL RUNS UNDER THE CLAIM, a failure handing it back: once
// the row says awaiting no redelivery reaches the question again, so announce first,
// record second, and either failing gives the claim back for the retry or, where it
// cannot, ENDS the run (a row nothing picks up is a turn destroyed in silence,
// [Coordinator.unclaim]). THE ONE STOP THAT IS NOT AN ENDING, so reported here as
// well as in [Coordinator.endRecord]; a park whose write failed reports nothing.
// The seat is freed only once the question is on the row (a person can take days;
// earlier, a reply would run as a new turn). This is the wait PauseTTL exists for:
// the box stays paused, TTL ticking, unless zero, when it is torn down and the run
// parks straight into reseed (the answer resumes either way).
func (c *Coordinator) park(ctx context.Context, run PendingRun, result Result) error {
	// THE ANCHOR, taken BEFORE the announcement: nobody can answer before this
	// instant, so an earlier reply is not its answer ([PendingRun.AskedAt]).
	asked := c.now()
	announcement := types.ExecutorAsked{
		Agent: run.AgentID, AgentHandle: run.AgentHandle, RoleName: run.Role,
		TurnID: run.TurnID, WorkKey: run.WorkKey, SandboxID: run.SandboxID,
		Question: run.Question, QuestionContext: run.QuestionContext, Decision: run.Decision,
		Audience: run.Audience,
		// THE IDENTITY, like the launch announcement: this event is display and
		// the durable thread is what a feed reader means by the conversation.
		ConversationKey: run.ConversationKey,
		// The item recorded at launch, so the question shows against its work.
		WorkItem: run.WorkItem,
		// THE QUESTION'S OWN JOB and any call it holds, so a live surface offers
		// the answer the engine takes for it.
		LaunchID: run.LaunchID,
		HeldCall: run.HeldCall.Ref(),
	}
	ev := events.New(announcement, events.TraceContext{
		TraceID: run.TraceID, ParentSpanID: run.SpanID,
	})
	ev.Source = run.Role
	if err := c.queue.Publish(ctx, topics.Event(announcement.EventType()), ev); err != nil {
		if unclaimErr := c.unclaim(ctx, run, parkUnannouncedDetail); unclaimErr != nil {
			//nolint:nilerr // Deliberate, as at the record below: the
			// claim did not go back, so the run has been ended and a
			// redelivered completion would find no record to claim.
			return nil
		}
		return fmt.Errorf("sandbox: announcing the question %s asked: %w", run.TurnID, err)
	}

	// BEING WRITTEN FROM HERE: a recount counts the run as the claim it still is
	// until the write's answer moves it (countwrite.go).
	write := c.writingCount(run, StatusResumed, StatusAwaiting)
	defer c.settleWrite(write)
	err := c.pending.MarkAwaiting(ctx, run.TurnID, Clarification{
		InputTokens: result.InputTokens, OutputTokens: result.OutputTokens,
		CacheReadTokens: result.CacheReadTokens, CacheWriteTokens: result.CacheWriteTokens,
		// What condensing this collection cost, paid by the same resume.
		Condensed: result.Condensed,
		// WHO IT IS PUT TO, resolved now and written with the question: the label
		// is the harness's words, and "what is waiting on me" was unanswerable
		// from the row ([PendingRun.AudienceHandles]).
		Answerers: c.audience.ResolveAudience(run, run.Audience),
		AskedAt:   asked,
	})
	// Freed and opened as one move: between two writes the seat would read idle
	// and the answer would run as an unrelated turn. Made in the step ending the
	// write's counting as the claim, so a recount cannot count the question
	// twice. A failed write records its claim's hand-back in this step too, so the
	// release sits behind it in the run's entry (countwrite.go), with what the
	// claim learned of the box host's store ([Coordinator.unclaimAt]). See the
	// package doc, "Parking a question".
	var (
		silent   time.Time
		answered bool
	)
	if err != nil {
		silent, answered = c.hostStoreSeen(ctx, run, failure{})
	}
	c.mu.Lock()
	var handBack *claimRelease
	movedBack := func() {}
	if err != nil {
		handBack, movedBack = c.handingBackLocked(run, parkUnrecordedDetail, failure{}, nil)
		handBack.release.HostStoreSilentSince, handBack.release.HostStoreAnswered = silent, answered
	}
	moved := c.wroteCountLocked(write, err == nil)
	c.mu.Unlock()
	movedBack()
	moved()
	if err != nil {
		// THE WAIT DID NOT LAND: the run is not parked and the row is still in
		// the claim. Handed back, the poll fires again, the paused box is
		// collected a second time and the park retried; where it cannot be, the
		// run is ended, since nothing picks up a row left claimed.
		log.ErrorContext(ctx, "sandbox_park_write_failed",
			"turn_id", run.TurnID, "error", err.Error(),
			"detail", "the question could not be recorded, so the run is not parked; its "+
				"claim is given back for the completion to be retried, or the run is "+
				"ended where it cannot be")
		if unclaimErr := c.unclaimOwing(ctx, handBack); unclaimErr != nil {
			//nolint:nilerr // Deliberate: the claim was not handed back,
			// so the run has been SETTLED and a redelivered completion
			// would retry against no record. Both failures are logged and
			// the loss is announced.
			return nil
		}
		// THE STORE'S SILENCE ([ErrStoreSilent]), like every step a collection
		// cannot go on without: the park follows a read the box's host answered,
		// so no clock runs on it and nothing renews its delivery, which is handed
		// back as this node's failure, never the message's (internal/engine's
		// controlOutcome), or an outage would send it to the poison.
		return fmt.Errorf("sandbox: parking %s: %w", run.TurnID, storeSilent(ctx, err))
	}

	// THE AGENT HAS STOPPED, durably: what the engine holds up while a turn works
	// comes down here ([CoordinatorOptions.Stopped]).
	c.reportStopped(ctx, run)
	if c.parked != nil {
		c.parked(ctx, run)
	}

	if run.PauseTTLSeconds == 0 {
		c.teardown(ctx, run)
		if err := c.pending.SetStatus(ctx, run.TurnID, StatusReseed, fenceOf(run)); err != nil {
			log.WarnContext(ctx, "sandbox_reseed_mark_failed", "turn_id", run.TurnID, "error", err.Error())
		}
	}
	log.InfoContext(ctx, "sandbox_run_awaiting_clarification",
		"turn_id", run.TurnID, "audience", run.Audience)
	return nil
}

// TryResumeFromAnswer records a delivery as the answer to the question a parked run
// of this seat waits on, if it is one, and resumes the run. It reports what the
// CALLER does with the delivery ([AnswerDisposition], the whole contract). A reply
// answers only if written after the question ([Reply.qualifies]); the first one is
// RECORDED (compare-and-set) and spent ([AnswerConsumed]) whatever the resume
// concludes, retried with the inbox held (answerowed.go). No run to answer, or none
// the delivery qualifies for, is [AnswerNotMine]; unreadable runs answer by this
// node's count ([Coordinator.answerLookupFailed]). An unrecordable answer is
// [AnswerDeferred] (the write MAY have landed), a failure of this node or store,
// handed back to keep its place at the inbox head, not NAKed behind newer mail
// ([MaxAnswerAttempts]). A reply past [MaxAnswerBytes] is [AnswerRefused], its
// REFUSAL the outcome (ADR-0114); a run ended under it returns it THROUGH THE ROW.
func (c *Coordinator) TryResumeFromAnswer(ctx context.Context, handle string, reply Reply) (AnswerDisposition, error) {
	if reply.Conv.Identity == "" && reply.Conv.Partition == "" {
		return AnswerNotMine, nil
	}
	trigger := reply.trigger()
	runs, err := c.pending.ListActiveForSeat(ctx, handle)
	if err != nil {
		// BOTH KEYS: the identity admits a run and the partition picks between
		// them ([ConversationRef.Best]), and on a direct message they differ.
		log.WarnContext(ctx, "sandbox_answer_lookup_failed",
			"agent", handle, "conversation", reply.Conv.Identity,
			"partition", reply.Conv.Partition, "error", err.Error())
		return c.answerLookupFailed(ctx, handle, trigger, err)
	}
	// THE LOOKUP ANSWERED, so what this seat counted against an unreadable store
	// is spent.
	c.clearLookupAttempts(handle)
	// Bounded by the seat's runs: each lost race is a run another reply or
	// transition took. mayAnswer is whether the writer's holder may act now,
	// asked once and only after a run would take the reply
	// ([AudienceResolver.MayAnswer]), so a chat message no question was put to
	// its writer reads nothing of the identity directory.
	mayAnswer := false
	for range len(runs) + 1 {
		if recorded, ok := reply.recordedOn(runs); ok {
			// A COPY OF AN ANSWER ALREADY RECORDED: spent as the answer it is, and
			// if this node is not driving its resume it starts (the recording node
			// may have stopped). A copy of a REFUSAL'S delivery is kept again until
			// the refusal leaves the run ([Coordinator.refusedReply]), spent at once
			// where the attempt this copy starts kept it.
			c.clearAnswerAttempts(recorded.AgentHandle, recorded.TurnID)
			kept := false
			if recorded.Status == StatusAnswered && !c.owes(recorded.TurnID) {
				kept = c.resumeOwed(ctx, recorded)
			}
			if _, refused := recorded.Refused(); refused {
				// AND ITS RECORD PUBLISHED AGAIN under the identity the run, question
				// and delivery derive: the recording node may have stopped before
				// publishing, and a landed record is the same row.
				c.announceRefusal(ctx, recorded, trigger)
				return c.refusedReply(ctx, recorded, reply, kept), nil
			}
			return AnswerConsumed, nil
		}
		run, found := reply.Best(runs)
		if !found {
			// NOTHING MATCHED: an ordinary message.
			return AnswerNotMine, nil
		}
		if !mayAnswer {
			// THE WRITER'S HOLDER, NOW: a person suspended or removed since, or an
			// unheld human seat, answers nothing. A directory that cannot say
			// keeps the reply's place, bounded per run like every deferral, never
			// spent on the run nor on an ordinary turn.
			may, err := c.audience.MayAnswer(ctx, reply.Writer)
			if err != nil {
				return c.deferAnswer(ctx, run, trigger,
					fmt.Errorf("sandbox: whether %s may answer %s now: %w", reply.Writer, run.TurnID, err))
			}
			if !may {
				log.InfoContext(ctx, "sandbox_answer_writer_may_not_act", "turn_id", run.TurnID,
					"agent", handle, "writer", reply.Writer,
					"detail", "the question was put to this seat, and whoever holds it may not act in "+
						"the company now, so the reply runs as the ordinary message it is and the "+
						"run waits on its question")
				return AnswerNotMine, nil
			}
			mayAnswer = true
		}
		recorded, won, err := c.recordAnswer(ctx, run, reply)
		refused := false
		if tooLarge, ok := errors.AsType[*AnswerTooLargeError](err); ok {
			// NO ANSWER THE ROW CAN CARRY ([MaxAnswerBytes]), refused by the store
			// against the run still waiting: ITS REFUSAL IS THE QUESTION'S OUTCOME
			// (ADR-0114), recorded in the answer's place (figures, no words) and
			// resumed like an answer. Recorded on the same terms, so a run answered
			// another way or moved on is the reply's ordinary loss, read again
			// below. Left waiting, the question took the next short message ("any
			// luck?") as its answer.
			recorded, won, err = c.recordRefusal(ctx, run, reply, tooLarge)
			refused = true
		}
		if errors.Is(err, ErrSeatNotHeld) {
			// THE SEAT'S HOLDER'S TO RECORD: nothing written; the delivery goes
			// back to reach the holder, uncounted (the budget is this node's, for a
			// seat it does not hold).
			return AnswerDeferred, err
		}
		if err != nil {
			// THE RECORD MAY HAVE LANDED: a store that could not say did not say
			// no, so the delivery comes back, and a copy finding the record is
			// spent above.
			return c.deferAnswer(ctx, run, trigger,
				fmt.Errorf("sandbox: recording the answer %s is waiting on: %w", run.TurnID, err))
		}
		if won && refused {
			// THE DELIVERY IN HAND IS DECIDED BY WHAT THE INLINE ATTEMPT DID with
			// the refusal: kept by its turn, spent here as an answer's is at its
			// record; otherwise off the row ([Coordinator.refusedReply]).
			c.clearAnswerAttempts(recorded.AgentHandle, recorded.TurnID)
			c.announceRefusal(ctx, recorded, trigger)
			kept := c.resumeOwed(ctx, recorded)
			return c.refusedReply(ctx, recorded, reply, kept), nil
		}
		if won {
			c.clearAnswerAttempts(recorded.AgentHandle, recorded.TurnID)
			c.resumeOwed(ctx, recorded)
			return AnswerConsumed, nil
		}
		// LOST: another reply was recorded first or the run moved on; read again
		// so the decision is made against what the store holds now.
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

// AnswerByTurn records the answer a person gave a parked run BY NAMING IT and
// resumes the run. It travels the seat's CONTROL TOPIC ([AnswerDesk.Deliver],
// [Coordinator.OnAnswerGiven]), never its mail, where a backlog starved it until
// the pause TTL. RECORDED first like a chat reply ([PendingStore.RecordAnswer]),
// then the RESUME waits for a free seat ([Coordinator.queueOwed]): resumed under the
// claim alone, a node stopping before its turn lost the answer. The delivery carries
// its question ([types.ExecutorAnswerGiven.LaunchID], new per question) and records
// only while the run waits on THAT one, else `not_awaiting`; none is refused
// ([errAnswerNamesNoQuestion]). NEVER A TURN: [AnswerConsumed] and [AnswerNotMine]
// are spent, only [AnswerDeferred] is NAKed, with no attempt ceiling on the record
// (dropping would lose the answer silently); the recorded resume is bounded like a
// chat reply's ([MaxAnswerAttempts], [answerWindow]). Publishes [types.ExecutorAnswered].
func (c *Coordinator) AnswerByTurn(ctx context.Context, given types.ExecutorAnswerGiven, trigger *events.Event) (AnswerDisposition, error) {
	answered := func(run PendingRun, outcome types.AnswerOutcome) {
		c.announceAnswered(ctx, run, types.AnswerViaOperator, outcome, answerer{
			by: given.AnsweredBy, kind: given.AnsweredByKind, operatorID: given.OperatorID,
			choice: given.Choice, said: redact.Secrets(strings.TrimSpace(given.Answer)),
			delegatedRule: given.DelegatedRule,
		})
	}
	if given.LaunchID == "" {
		// NO QUESTION NAMED, so none to pair with: spent and said why.
		log.ErrorContext(ctx, "sandbox_answer_names_no_question",
			"turn_id", given.TurnID, "agent", given.AgentHandle,
			"answered_by", given.AnsweredBy, "answered_by_kind", given.AnsweredByKind,
			"operator_id", given.OperatorID,
			"detail", "an answer by turn arrived naming no question (launch_id), so which "+
				"question it answers cannot be known; it is spent without resuming the run, "+
				"and the person has to answer the run again")
		return AnswerNotMine, fmt.Errorf("%w: run %s", errAnswerNamesNoQuestion, given.TurnID)
	}
	delivery := deliveryOf(trigger)
	if delivery == "" {
		// NOTHING TO RECORD IT UNDER: a record names its deliveries, by which a
		// copy is recognised.
		return AnswerNotMine, fmt.Errorf("sandbox: the answer to run %s arrived with no event to "+
			"record it under", given.TurnID)
	}
	for range answerRecordRounds {
		run, found, err := c.pending.Get(ctx, given.TurnID)
		if err != nil {
			// A STORE THAT COULD NOT BE READ IS NOT A RUN THAT IS GONE: handed
			// back, nothing announced.
			return AnswerDeferred, fmt.Errorf("sandbox: reading run %s for the answer it was given: %w",
				given.TurnID, err)
		}
		if !found {
			// SETTLED, or never there; all known is what the answer named.
			answered(PendingRun{
				TurnID: given.TurnID, AgentID: given.Agent, AgentHandle: given.AgentHandle,
			}, types.AnswerGone)
			return AnswerNotMine, nil
		}
		if run.AgentID != given.Agent {
			// THE ANSWER REACHED ANOTHER SEAT'S INBOX ([AnswerDesk.Deliver]
			// addresses it off the accepted row): resuming it here would re-enter
			// another seat's conversation as this one. Compared by agent id, which
			// names the inbox.
			log.ErrorContext(ctx, "sandbox_answer_misrouted",
				"turn_id", given.TurnID, "addressed_to", given.Agent,
				"run_seat", run.AgentID,
				"detail", "an answer by turn arrived on a seat that does not hold the run it "+
					"names; it is dropped rather than resumed under the wrong seat")
			return AnswerNotMine, nil
		}
		if run.Answer != nil && slices.Contains(run.Answer.EventIDs, delivery) {
			// THIS VERY ANSWER, ALREADY RECORDED (a redelivery whose ack was lost):
			// spent as the answer it is, and if this node is not driving its resume
			// it starts, unless the ending is decided, which takes no resume and
			// lets the answer go back to the seat.
			if run.Status == StatusAnswered && run.Ending == nil && !c.owes(run.TurnID) {
				c.queueOwed(ctx, run)
			}
			return AnswerConsumed, nil
		}
		if given.AnsweredByKind == string(iam.ActorSystem) && run.EngineAnswers() {
			// THE ENGINE'S ANSWER BY TURN, AS AN OLDER BUILD'S WATCH SENDS IT, even
			// past losing the seat: this build carries such a run on from its row
			// (waitover.go), and the keys saying how its wait went are gone off
			// the wire, so recorded it tells the run a person answered. Spent
			// unrecorded and untold: nobody answered.
			log.WarnContext(ctx, "sandbox_engine_answer_dropped",
				"turn_id", given.TurnID, "launch_id", given.LaunchID, "agent", given.AgentHandle,
				"detail", "an answer by turn from the engine reached a run the engine carries on from its "+
					"row; it is spent without being recorded, and the seat's watch carries the run on")
			return AnswerNotMine, nil
		}
		if slices.Contains(run.DeclinedAnswers, delivery) {
			// THE QUESTION LET THIS ANSWER GO (every resume attempt failed): this
			// is the decline's copy or the original round again. It has no
			// ordinary form, so it is spent, the person told by the COPY, which
			// the decline publishes once through the row's outbox.
			log.WarnContext(ctx, "sandbox_answer_declined_spent",
				"turn_id", run.TurnID, "agent", run.AgentHandle, "delivery", delivery,
				"detail", "an answer by turn its run could not be resumed with was let go of; "+
					"the run waits on its question again and has to be answered again")
			if declinedCopy(run.DeclinedAnswers, delivery) {
				answered(run, types.AnswerDeclined)
			}
			return AnswerNotMine, nil
		}
		if run.Ending != nil {
			// THE RUN IS OVER though its record is not gone: a decided ending takes
			// no answer ([ErrRunEnding]) and the record waits for the ending's last
			// steps (the end's write and the delete). The copy of an answer the
			// ending let go of arrives in that window; read as a live run not
			// waiting it was announced `not_awaiting` and the person never learned
			// the run ended without their answer.
			answered(run, types.AnswerGone)
			return AnswerNotMine, nil
		}
		if !slices.Contains(Awaiting, run.Status) || given.LaunchID != run.LaunchID {
			// NOT WAITING ON THE QUESTION THIS ANSWERS: answered another way or
			// moved on since ("The question it answers" above).
			answered(run, types.AnswerNotAwaiting)
			return AnswerNotMine, nil
		}
		if heldMismatch(run, given) {
			// A HELD CALL IS ANSWERED BY ITS APPROVAL ALONE, and only a held call
			// is: the route deciding who may approve stamps its id, every other
			// route stamps none. Spent, said, never recorded: recording would make
			// or refuse a call nobody entitled decided.
			log.ErrorContext(ctx, "sandbox_answer_approval_mismatched",
				"turn_id", given.TurnID, "launch_id", given.LaunchID, "approval", given.Approval,
				"held", run.HeldCall != nil, "answered_by", given.AnsweredBy,
				"detail", "an answer by turn names a held call the run's question is not, or "+
					"answers a held call without naming it; it is spent without resuming the run")
			answered(run, types.AnswerUnoffered)
			return AnswerNotMine, nil
		}
		if run.HeldCall != nil && run.HeldCall.Lapsed(c.now()) && given.Choice != HeldDeny {
			// A LAPSED CALL TAKES DENY ALONE, the engine's own or a person's:
			// an allow arriving between the lapse and the sweep would make a
			// call on figures a day old (ADR-0171).
			log.WarnContext(ctx, "sandbox_answer_lapsed",
				"turn_id", given.TurnID, "approval", given.Approval, "choice", given.Choice,
				"answered_by", given.AnsweredBy, "lapsed_at", run.HeldCall.LapsesAt)
			answered(run, types.AnswerNotAwaiting)
			return AnswerNotMine, nil
		}
		// WHAT IT SAYS, MEASURED AS `answer_executor` MEASURED IT: as authored,
		// trimmed, before redaction, the one string both ends hold to
		// [MaxAnswerTextBytes], so a tool-accepted answer is never refused here
		// for a redaction it did not write. Before the choice is read, in the
		// tool's order.
		if said := len(strings.TrimSpace(given.Answer)); said > MaxAnswerTextBytes {
			return c.answerByTurnTooLarge(ctx, run, given, "words", said, MaxAnswerTextBytes)
		}
		words, answer, err := answerGivenOf(run, given, trigger, c.now())
		if err != nil {
			// A CHOICE THE QUESTION DOES NOT OFFER (`answer_executor` checked it
			// before delivering): spent under an outcome of its own, never handed
			// to the run as an unoffered option nor told as not waiting.
			log.ErrorContext(ctx, "sandbox_answer_choice_unoffered",
				"turn_id", given.TurnID, "launch_id", given.LaunchID, "choice", given.Choice,
				"answered_by", given.AnsweredBy, "error", err.Error())
			answered(run, types.AnswerUnoffered)
			return AnswerNotMine, nil
		}
		recorded, won, err := c.record(ctx, run, words, answer)
		if tooLarge, ok := errors.AsType[*AnswerTooLargeError](err); ok {
			// NO ANSWER THE ROW CAN CARRY ([MaxAnswerBytes]), refused before any
			// write, which no attempt changes: the row's backstop, unreachable by
			// an answer within [MaxAnswerTextBytes] even at its worst
			// (TestAnAnswerByTurnAtItsSourcesBoundFitsARecordedAnswersShare).
			return c.answerByTurnTooLarge(ctx, run, given, "record", tooLarge.Weighs, tooLarge.May)
		}
		if err != nil {
			// THE RECORD MAY HAVE LANDED, resolved towards the run as on the chat
			// route: a landed record is found on redelivery and spent.
			return AnswerDeferred, fmt.Errorf("sandbox: recording the answer run %s was given: %w",
				given.TurnID, err)
		}
		if !won {
			// LOST between read and record: read again against what the store
			// holds now.
			continue
		}
		log.InfoContext(ctx, "sandbox_clarification_answered",
			"turn_id", recorded.TurnID, "via", string(types.AnswerViaOperator),
			"answered_by", given.AnsweredBy, "answered_by_kind", given.AnsweredByKind,
			"operator_id", given.OperatorID)
		c.clearAnswerAttempts(recorded.AgentHandle, recorded.TurnID)
		c.queueOwed(ctx, recorded)
		return AnswerConsumed, nil
	}
	return AnswerDeferred, fmt.Errorf("sandbox: run %s kept changing under the answer it was given",
		given.TurnID)
}

// answerRecordRounds bounds [Coordinator.AnswerByTurn]'s record loop.
//
// A round repeats only when the record was REFUSED (answered another way or
// moved on between read and record; a lost race on the row is the store's to
// retry). The read after a refusal lands on the branch that explains it, so one
// round past the first covers it and a third covers a question answered and let
// go again in between. Past that the answer is handed back (AnswerDeferred) and
// the queue's backoff paces it, not back-to-back reads.
const answerRecordRounds = 3

// answerByTurnTooLarge spends an answer by turn past what its run may take,
// refused before any write (past [MaxAnswerTextBytes], measured "words", or past
// [MaxAnswerBytes] on the row, "record"), recording `executor_answered`
// too_large with the figures and none of the words, the question still waiting.
// It has no ordinary form and no conversation to be told in, so the record says
// it was not taken; never `declined`, which names an answer the run held and
// let go.
func (c *Coordinator) answerByTurnTooLarge(ctx context.Context, run PendingRun,
	given types.ExecutorAnswerGiven, measured string, weighs, may int,
) (AnswerDisposition, error) {
	log.ErrorContext(ctx, "sandbox_answer_too_large",
		"turn_id", given.TurnID, "launch_id", given.LaunchID, "answered_by", given.AnsweredBy,
		"measured", measured, "bytes", weighs, "max_bytes", may,
		"detail", "an answer by turn past what its run may take as an answer is spent "+
			"unrecorded, and the run waits on its question")
	c.announceAnswered(ctx, run, types.AnswerViaOperator, types.AnswerTooLarge, answerer{
		by: given.AnsweredBy, kind: given.AnsweredByKind, operatorID: given.OperatorID,
		choice: given.Choice, answerBytes: weighs, answerMaxBytes: may,
	})
	return AnswerNotMine, nil
}

// OnAnswerGiven settles one answer BY TURN off the seat's control topic: an error
// hands the delivery back, nil spends it. SPENT BEFORE IS SPENT NOW: a delivery
// recorded as worked ([CoordinatorOptions.WasSpent]) is acknowledged without
// reaching its run (it would announce `gone` or `not_awaiting` about an answer that
// resumed it), read BEFORE the coordinator is asked since the run then says nothing.
// EVERY DISPOSITION BUT [AnswerDeferred] SPENDS IT and records it as worked
// ([CoordinatorOptions.Spent]); the deferred goes back to the broker, whose backoff
// spaces attempts and whose dead-letter subject is the loud end.
func (c *Coordinator) OnAnswerGiven(ctx context.Context, given types.ExecutorAnswerGiven, ev *events.Event) error {
	handle := given.AgentHandle
	if c.wasSpent != nil && ev != nil && c.wasSpent(ctx, handle, ev) {
		log.InfoContext(ctx, "sandbox_answer_by_turn_already_spent",
			"agent_handle", handle, "turn_id", given.TurnID, "event_id", ev.ID.String(),
			"detail", "this answer by turn was spent before (taken by a resumed turn, let go "+
				"of, or settled), so its redelivery is acknowledged without reaching its run "+
				"or being announced again")
		return nil
	}
	disposition, err := c.AnswerByTurn(ctx, given, ev)
	switch {
	case disposition == AnswerDeferred:
		if err == nil {
			err = fmt.Errorf("sandbox: the answer to run %s could not be recorded", given.TurnID)
		}
		return fmt.Errorf("sandbox: %s is still owed this answer: %w", handle, err)
	case !disposition.Valid():
		log.ErrorContext(ctx, "sandbox_answer_disposition_unknown",
			"agent_handle", handle, "turn_id", given.TurnID, "disposition", disposition.String(),
			"detail", "an answer by turn was given no disposition this build knows; it is "+
				"spent, because an answer addressed to a run is never a turn")
	case err != nil:
		log.WarnContext(ctx, "sandbox_answer_by_turn_failed",
			"agent_handle", handle, "turn_id", given.TurnID,
			"disposition", disposition.String(), "error", err)
	}
	if c.spent != nil && ev != nil {
		c.spent(ctx, handle, []*events.Event{ev})
	}
	return nil
}

// errRunMovedOn is a resume's report that the run moved on to somebody else
// first (a newer lease owns it, or another party ended it), so the beside
// disposition ([AnswerNotMine]) is not a run that is gone: whoever holds it now
// drives and announces it. Neither route hands back the delivery as its own
// failure: a completion's is settled ([Coordinator.OnCompleted]), marked
// [ErrUnannounced] only while no waiter can announce its job (renew.go).
var errRunMovedOn = errors.New("sandbox: the run moved on to another holder before this " +
	"attempt could end it")

// errAnswerNamesNoQuestion is an answer by turn that carries no
// [types.ExecutorAnswerGiven.LaunchID]: it is spent rather than resumed with,
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

// answerGivenOf is what recording an answer by turn writes: the sealed words
// (the answer as the run reads it, [AnswerText], and who gave it per
// iam.ActorFor) and the record they are sealed onto ([Coordinator.record]), with
// the one delivery it arrived in, the [types.ExecutorAnswerGiven] event, carried
// so a copy can be handed back if the answer is let go. An error is a choice the
// run's question does not offer.
func answerGivenOf(run PendingRun, given types.ExecutorAnswerGiven, trigger *events.Event,
	now time.Time,
) (AnswerWords, RecordedAnswer, error) {
	if _, err := AnswerText(run.Decision, given.Choice, given.Answer); err != nil {
		return AnswerWords{}, RecordedAnswer{}, err
	}
	words := AnswerWords{
		// Redacted HERE, as a chat reply is: it is sealed on the row and handed
		// as is to the harness, whose box and session keep it.
		Text: redact.Secrets(strings.TrimSpace(given.Answer)), Choice: given.Choice,
		Via: types.AnswerViaOperator,
		By:  given.AnsweredBy, ByKind: given.AnsweredByKind, OperatorID: given.OperatorID,
	}
	answer := RecordedAnswer{
		EventIDs: []string{trigger.ID.String()}, PostedAt: trigger.Timestamp, RecordedAt: now,
	}
	raw, err := json.Marshal(trigger)
	if err != nil {
		// Kept without it, as a chat reply's is: it costs only a let-go's copy.
		log.Warn("sandbox_answer_event_unencodable", "event_id", trigger.ID.String(), "error", err.Error())
		return words, answer, nil
	}
	answer.Events = []json.RawMessage{raw}
	return words, answer, nil
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

// answerer is who gave an answer, in iam.ActorFor's three halves
// ([types.ExecutorAnswered.AnsweredBy]), and what they answered: the option
// chosen by id and their words, announced beside the question.
type answerer struct {
	by, kind, operatorID string
	choice, said         string
	// delegatedRule is the rule a delegate answered under, 0 for a person.
	delegatedRule int
	// answerBytes and answerMaxBytes are an answer refused as too large and what
	// one may be, on the refusal's measure ([types.AnswerTooLarge]); else zero.
	answerBytes, answerMaxBytes int
}

// chatAnswerer is who a chat reply came from: the sender as the transport's
// envelope names them, a person, no credential (the chat surface vouched).
func chatAnswerer(trigger *events.Event) answerer {
	if trigger == nil {
		return answerer{}
	}
	return answerer{by: trigger.Actor(), kind: string(iam.ActorHuman)}
}

// announceAnswered publishes what an answer to a parked run became.
//
// BEST EFFORT, like every after-the-fact announcement: a feed row that could not
// be written is a log line, not a reason to hand the delivery back and resume
// the run twice.
func (c *Coordinator) announceAnswered(ctx context.Context, run PendingRun,
	via types.AnswerVia, outcome types.AnswerOutcome, by answerer,
) {
	c.announceAnsweredAs(ctx, run, via, outcome, by, recordIdentity{})
}

// announceAnsweredAs is [Coordinator.announceAnswered] under identity as, fresh
// where zero: a record the same fact republishes carries its id and instant, which
// the event store keeps as one row ([recordIdentity]).
func (c *Coordinator) announceAnsweredAs(ctx context.Context, run PendingRun,
	via types.AnswerVia, outcome types.AnswerOutcome, by answerer, as recordIdentity,
) {
	payload := types.ExecutorAnswered{
		Agent: run.AgentID, AgentHandle: run.AgentHandle, RoleName: run.Role,
		TurnID: run.TurnID, WorkKey: run.WorkKey, WorkItem: run.WorkItem,
		Via: via, Outcome: outcome,
		AnsweredBy: by.by, AnsweredByKind: by.kind, OperatorID: by.operatorID,
		DelegatedRule: by.delegatedRule,
		// THE QUESTION AND ITS ANSWER, which the row forgets once the run moves
		// on: the one account outliving the run, read from the event log for a
		// person's answered decisions (ADR-0068).
		LaunchID: run.LaunchID, Question: run.Question, QuestionContext: run.QuestionContext,
		Decision: run.Decision, AudienceHandles: slices.Clone(run.AudienceHandles),
		Choice: by.choice, Answer: by.said,
	}
	if by.answerBytes > 0 {
		payload.AnswerBytes, payload.AnswerMaxBytes = by.answerBytes, by.answerMaxBytes
	}
	if !run.AskedAt.IsZero() {
		payload.AskedAt = run.AskedAt.UTC()
	}
	ev := events.New(payload, events.TraceContext{TraceID: run.TraceID, ParentSpanID: run.SpanID})
	if as.id != uuid.Nil {
		ev.ID = as.id
	}
	if !as.at.IsZero() {
		ev.Timestamp = as.at.UTC()
	}
	ev.Source = run.Role
	if err := c.queue.Publish(ctx, topics.Event(payload.EventType()), ev); err != nil {
		log.WarnContext(ctx, "sandbox_answer_announce_failed",
			"turn_id", run.TurnID, "via", string(via), "outcome", string(outcome),
			"error", err.Error())
	}
}

// resumeAndSettle re-enters the parked turn, then settles the box; seen is what
// was signalled before the turn's admission was asked
// ([Coordinator.signalsSeen]). A turn that relaunched leaves the box to its new
// job; otherwise the ending is decided and the box torn down, the record going
// with the ending's last step, here or by this node's retry
// ([Coordinator.finishEnding]). It reports what it left the delivery (still
// owed, gone, or spent), since only one of those is a retry. A recorded answer
// the run ends without goes back to the seat through the row
// ([Coordinator.letGoAnswer]); a reply a turn took ([ResumeRequest.Begin]) is
// spent. See the package doc, "Resuming and settling".
func (c *Coordinator) resumeAndSettle(adm admitted, run PendingRun,
	answer string, success bool, trigger *events.Event, outcome runOutcome, seen map[signal]uint64,
) (AnswerDisposition, error) {
	// UNDER THE ADMISSION, the only way here ([admitted]).
	ctx := adm.ctx
	// WHAT THIS ATTEMPT SAYS OF A WAIT FOR THE OPERATOR: a failure only an
	// operator clears counts the run in [Coordinator.ResumesBlocked] once its
	// hand-back lands; every other outcome takes it out as the attempt returns.
	blockedOn := ""
	defer func() {
		if blockedOn == "" {
			c.unblock(run.TurnID)
		}
	}()
	if len(run.ExecuteState) == 0 {
		// No parked turn to resume, so the run cannot be judged. Unreachable
		// through any live path ([StatusLaunching] until the parked state is
		// written, and launching runs are not claimable), and kept as the
		// assertion that the state does its job: a claimed run without one is a
		// row this build did not write, failed as no_execute_state, not resumed
		// into nothing.
		log.WarnContext(ctx, "sandbox_resume_no_execute_state",
			"turn_id", run.TurnID, "claimed_from", run.ClaimedFrom,
			"detail", "the row carried no parked turn state; the turn "+
				"cannot be resumed and the run is failed")
		if !c.settleFailed(ctx, run, types.ExecutorFailureNoExecuteState,
			"the run record carried none of its turn's parked state, so the turn that "+
				"started it cannot be continued") {
			return AnswerNotMine, errRunMovedOn
		}
		// TERMINALLY GONE, so nothing is owed the delivery: requeueing would circle
		// a deleted run, acking would swallow a person's message for a turn that no
		// longer exists. A recorded reply is not lost: the ending handed it back
		// through the row.
		return AnswerNotMine, nil
	}
	// THE SEAT STAYS HELD THROUGH THE RESUMED TURN. The claim holds the row in
	// [StatusResumed], a [Holding] status, counted so while the row stands; every
	// branch below recounts from the store after the turn, and a row outliving the
	// turn by the end it owes the executor log is counted until its retry deletes
	// it, the seat's mail held behind the end too. Giving the count back before the
	// resumer ran lifted the inbox hold for the whole turn and ran the next mail as
	// a second turn beside it (seathold.go).
	//
	// WHETHER THE TURN BEGAN, reported by the resumer calling Begin
	// ([ResumeRequest.Begin]); atomic since it need not call on this goroutine.
	var began atomic.Bool

	// A PANIC ENDS THE CLAIM, as an abandoned resume does, before going up. The
	// queue's handler recovers it, so a left claim kept the row `resumed`, holding
	// the seat with no mail until a restart or move. Ended on [ErrResumeAbandoned]'s
	// terms: announced as lost where the turn never began (a recorded reply goes
	// back through the row), settled silently where it did, since it may have
	// acted and a retry must not re-enter it.
	//
	// AND THE TURN'S FRAME RUNS until the resumer returns, which an ending decided
	// meanwhile waits on before deciding the turn's end ([Coordinator.framing]);
	// done once whichever way it returns.
	frameDone := sync.OnceFunc(func() { c.frameEnds(run.TurnID) })
	defer func(ctx context.Context) {
		if p := recover(); p != nil {
			frameDone()
			var note *failureNote
			if !began.Load() {
				note = &failureNote{reason: types.ExecutorFailureResumeBroken, detail: resumeBrokenDetail}
			}
			// DETACHED, bounded: the panic may be the cancellation's own and a
			// dead context writes nothing.
			settle, cancel := c.detachedRecord(ctx)
			defer cancel()
			c.settleAbandoned(settle, run, note)
			panic(p)
		}
	}(ctx)
	begin := func(ctx context.Context, carry ResumeCarry) error {
		switch {
		case claimedAnswer(run):
			// THE TURN TAKES THE ANSWER on the row, or does not run, under the
			// CLAIMANT's lease ([PendingStore.ClaimForResume]).
			taken, takeErr := c.pending.TakeAnswer(ctx, run.TurnID, run.LaunchID, fenceOf(run))
			if takeErr != nil {
				return fmt.Errorf("sandbox: recording that run %s's turn took its answer: %w",
					run.TurnID, takeErr)
			}
			if !taken {
				return fmt.Errorf("sandbox: run %s is no longer the claim that holds its answer: "+
					"the answer was let go of, or the seat's next holder fenced the row", run.TurnID)
			}
			// AND ITS DELIVERY IS SPENT AT THE TAKE, on every route ([CoordinatorOptions.Spent]):
			// the inline attempt holds it unacknowledged through the whole turn, and a
			// node stopping mid-turn leaves it for the next holder, who finds the run
			// reaped and nothing to recognise it by; spent only after the turn, it ran
			// as a second turn. NOT A REFUSAL'S, which carries no copy of its reply: the
			// inbox's delivery is the only one, and a turn that broke before reaching
			// anybody gives the take back, so it is spent once the turn that took it
			// keeps it ([Coordinator.spendRefused]).
			c.spend(ctx, run.AgentHandle, *run.Answer)
		case claimedWaitOver(run):
			// THE TURN TAKES THE ENGINE'S CARRYING ON, as an answer's: a claim
			// stopping before it is revived to waiting, one after it is used.
			taken, takeErr := c.pending.TakeWaitOver(ctx, run.TurnID, run.LaunchID, fenceOf(run))
			if takeErr != nil {
				return fmt.Errorf("sandbox: recording that run %s's turn carried it on: %w",
					run.TurnID, takeErr)
			}
			if !taken {
				return fmt.Errorf("sandbox: run %s is no longer the claim that carries it on: the seat's "+
					"next holder fenced the row", run.TurnID)
			}
		default:
			c.markResumeBegun(ctx, run, carry)
		}
		began.Store(true)
		return nil
	}

	// STRAIGHT TO THE RESUMER ([NewCoordinator] refuses to build without it): "this
	// node cannot resume this run" is its own answer ([ErrResumeUnavailable]), down
	// the failure path. AND THE RUN'S CALLS, read from its log here, after the box
	// was stopped or read, missing only a call still running when the bridge session
	// ended (internal/api/mcpbridge, "An opening ends without waiting for its
	// calls"). Never "no calls" for an unreadable log: that is a failed resume. Read
	// INSIDE the frame so a concurrent ending waits ([Coordinator.framing]). WITH THE
	// CALL A PERSON APPROVED that the log is still owed ([BridgeLog.withHeld]), which
	// a later attempt finds on the row alone.
	c.frameBegins(run.TurnID)
	calls, err := c.pending.BridgeLog(ctx, run)
	if err == nil {
		// WHAT THE READ SAW, before the held call is added
		// ([ResumeRequest.BridgedRead]).
		read := calls.Read()
		calls = calls.withHeld(run)
		// AND THE TURN'S LEADS, same frame ([Coordinator.leadsAt]), never "none"
		// for a part it could not read.
		leads := c.leadsAt(ctx, run)
		err = c.resume.Resume(ctx, ResumeRequest{
			Run: run, Bridged: calls, BridgedRead: read, Leads: leads, Answer: answer, Success: success,
			Trigger:       trigger,
			DeliveredRefs: outcome.DeliveredRefs, DeliveredRefsElided: outcome.DeliveredRefsElided,
			Pushed: outcome.Pushed, PushedElided: outcome.PushedElided, PushedUnread: outcome.PushedUnread,
			InputTokens: outcome.InputTokens, OutputTokens: outcome.OutputTokens,
			CacheReadTokens: outcome.CacheReadTokens, CacheWriteTokens: outcome.CacheWriteTokens,
			Engine: outcome.Engine, Ended: outcome.Ended, Asked: outcome.Asked, Lost: outcome.Lost,
			Begin: begin,
		})
	}
	frameDone()
	if err != nil {
		if errors.Is(err, ErrResumeAbandoned) && !began.Load() {
			// BROKEN BEFORE ITS TURN BEGAN (a panic re-entering the conversation,
			// say): not retried, as every abandoned resume (same bytes, same
			// defect), but nothing ran, so a recorded reply goes back through the
			// row. AND ANNOUNCED, like every run lost without its turn: no turn
			// published a completion, so the ending is the only account (else the
			// turn parked on it read as parked for good).
			note := abandonedNote(err)
			log.ErrorContext(ctx, "sandbox_resume_abandoned_before_turn",
				"turn_id", run.TurnID, "reason", note.reason, "error", err.Error(),
				"detail", "the resume could not begin the turn it would continue; the run "+
					"is ended and announced rather than retried, and a person's reply that "+
					"drove the resume goes back to the seat rather than being spent")
			if !c.settleAbandoned(ctx, run, note) {
				// NOT THIS NODE'S TO END after all: the run moved on (the next
				// holder fenced and revived it), and its fate, and a reply's, is
				// that holder's.
				return AnswerNotMine, errRunMovedOn
			}
			return AnswerNotMine, nil
		}
		if errors.Is(err, ErrResumeAbandoned) {
			// THE CLAIM IS NEVER GIVEN BACK: a retry could not cure any way a resumer
			// marks a turn so ([ErrResumeAbandoned]); it would re-enter the same
			// parked turn and repeat its writes, hit the same panic or refusal, run
			// past a person's stop, or find no run to collect. The run is SETTLED
			// instead, so a redelivery finds no record to claim (left claimed,
			// nothing settles it, since recovery runs only on a seat handoff, and the
			// busy seat would park every message beside a box nobody collects). The
			// busy count is RECOUNTED, not re-marked: the turn may have relaunched
			// before breaking, counting a job this settle ended. Nothing is
			// announced: the turn did resume and published its own failed
			// completion, and the reply it took was spent at the take.
			log.ErrorContext(ctx, "sandbox_resume_abandoned",
				"turn_id", run.TurnID, "error", err.Error(),
				"detail", "the run is settled rather than un-claimed, so the completion is "+
					"not redelivered into a conversation a retry must not re-enter")
			c.settleAbandoned(ctx, run, nil)
			// THE TURN RAN and must not run again, so what drove it is SPENT (the one
			// branch deliberately keeping the claim): the ordinary route would run
			// a second turn on a message already taken up, a refusal it took
			// included ([Coordinator.spendRefused]).
			c.spendRefused(ctx, run)
			return AnswerConsumed, nil
		}
		// PAST THE TURN'S DEADLINE NOTHING WAITS AND NOTHING IS RETRIED: a
		// completion's resume that failed or met a waiting condition after the
		// wall-clock cap is the turn's last, the run ended and the seat freed
		// ([Coordinator.endAtDeadline]). Handed back, a health check whose
		// ten-minute cap passed during a model's weekly limit was re-announced on
		// every poll for twenty-seven hours with the seat's next three fires queued
		// behind it. An answer's resume is bounded by its own series
		// ([MaxAnswerAttempts]).
		if !resumedFromWait(run) && run.PastDeadline(c.now()) {
			c.endAtDeadline(ctx, run, err.Error(), outcome.DeliveredRefs, outcome.DeliveredRefsElided)
			return AnswerConsumed, nil
		}
		// A RESUME ITS SEAT'S CONDITIONS REFUSED IS HELD, not retried
		// ([ErrResumeHeld]), A COMPLETION'S only: an answer's resume is its own
		// series, asking the seat's conditions before every attempt
		// ([Admission]), reverted below as the wait it is ([waitingRefusal]),
		// uncharged.
		var held *HeldError
		if errors.As(err, &held) && !resumedFromWait(run) {
			// The claim's count moves with the hand-back once its release lands
			// ([Coordinator.unclaimOwing]). seen is what was signalled before
			// admission, not at return: a signal after the refusal found no hold to
			// lift and would leave the job waiting for the hold's instant. See the
			// package doc, "Holding a refused resume".
			hold, unclaimErr := c.unclaimHeld(ctx, run, held.Refusal)
			if unclaimErr != nil {
				//nolint:nilerr // The run was ended in its place, as on
				// the retry's path below: nothing is left to come back to.
				return AnswerNotMine, nil
			}
			// THE COMPLETION IS SPENT: the hold is on the row and the poll announces
			// again when it lifts; where no poll can (the clock on its box's host
			// running) the caller hands the delivery back (renew.go). AND KEPT HERE,
			// unheld, for a re-fire at the hold's instant and the lifting signal,
			// since the poll reads through the host's store, whose silence is
			// unbounded. The release numbered its hold next after the claimed row's
			// ([CoordStore.ReleaseClaim]).
			c.keepHeldOnRow(run, trigger, hold, run.Holds+1, seen, false)
			return AnswerConsumed, nil
		}
		// Un-claim so a retry can win the flip again, else the redelivered
		// completion is refused and the conversation lost in resumed. A resume
		// refused on a named condition is a wait, said as one, handed back unheld
		// too: only this node knows its models are at their limit, so it is held
		// here until the refusal's instant ([Coordinator.keepRefused]), never on
		// the row, which would silence every waiter of the next holder. See the
		// package doc, "Holding a refused resume".
		refusal, refused := waitingRefusal(err)
		var failed failure
		if refused {
			log.InfoContext(ctx, "sandbox_resume_refused",
				"turn_id", run.TurnID, "revert_to", claimedFrom(run),
				"condition", string(refusal.Condition), "reason", refusal.Reason,
				"detail", "the resumed turn met a condition that ends by waiting; the run is "+
					"reverted and resumed once the condition clears")
		} else {
			log.ErrorContext(ctx, "sandbox_resume_failed",
				"turn_id", run.TurnID, "revert_to", claimedFrom(run),
				"attempt", run.Launch.ResumeFailures+1, "error", err.Error())
			// COUNTED ONCE THE HAND-BACK BELOW LANDS, not as the failure is met or
			// the attempt returns: a hand-back that did not land ended the run in
			// its place or found it moved on, and counted either side of that, an
			// over run held the alarm until its seat moved ([blockedResume]).
			blockedOn = c.blockedBy(run, err)
			failed.blocked = blockedOn
		}
		// A FAILURE IS COUNTED AND BOUNDED; a wait is not, nor a node's own
		// failure or one another node can get past ([Coordinator.resumeGivenUp]).
		// A completion's only, for the deadline's reason.
		if !refused && !claimedAnswer(run) && c.runsOwnFailure(ctx, run, err) {
			now := c.now()
			if c.resumeGivenUp(ctx, run, now, err) {
				return AnswerConsumed, nil
			}
			failed.resumeFailed = now
		}
		// AND THE WINDOW AN ANSWER'S RELAUNCH WAS REFUSED BELOW ITS FLOOR IN, onto
		// the job's record ([Release.RelaunchRefusedIn]): the answer's wait is its
		// series' in this node's memory, but its next attempt's relaunch (here or
		// on the next holder) reads the row and waits for its brief's room no
		// longer than that window.
		if unclaimErr := c.unclaimAt(ctx, run, resumeUnrevertedDetail, failed, nil,
			refusal.Window); unclaimErr != nil {
			//nolint:nilerr // Deliberate, and the same answer the
			// acted-and-broke branch above gives: the run has been settled, so
			// nothing is sent back for a retry to claim and nothing is owed the
			// delivery (ended and announced, a recorded reply gone back by its row).
			return AnswerNotMine, nil
		}
		// THE RUN IS BACK WHERE THE CLAIM FOUND IT (on the answer route, awaiting
		// this reply), so the delivery is still owed and comes back, not worked as
		// an ordinary message.
		return AnswerDeferred, err
	}
	// THE TURN RAN, so a refusal it took is kept and the inbox's delivery is spent
	// now ([Coordinator.spendRefused]): the run was told, and a copy is a reply it
	// already has.
	c.spendRefused(ctx, run)

	latest, err := c.current(ctx, run)
	if err != nil {
		// THE ACTED BRANCH'S ANSWER, for its reason: the resume is over, so a row
		// left claimed is picked up by nothing; the store decides whether this is
		// still the claimed run ([Coordinator.settleClaimed]).
		c.settleClaimed(ctx, run, err, nil)
		c.syncSeat(ctx, run.AgentHandle)
		// THE TURN RAN; only the settle after it could not be decided. What drove
		// it is spent.
		return AnswerConsumed, nil
	}
	if latest.LaunchID != run.LaunchID && slices.Contains(Active, latest.Status) {
		// The resumed turn RELAUNCHED its executor: a new job owns the row and
		// held the seat as it opened. The box this run was collected from is the
		// new job's too or already gone (the relaunch continued in it, or reclaimed
		// it through its own cell, [Coordinator.Launch]), so nothing of this run's
		// is left to reclaim.
		//
		// TOLD BY ITS LAUNCH, since its status can be any live one by now
		// (launching, running, claimed by its own completion, parked on its own
		// question). Reading for running alone tore a second launch's box down
		// under it; running or launching tore down a next job's question checkout
		// and ended the run under it. An already-over relaunch (one that could not
		// start) settles like the rest.
		log.InfoContext(ctx, "sandbox_relaunched_in_turn",
			"turn_id", run.TurnID, "status", latest.Status,
			"sandbox_id", latest.SandboxID, "continued", latest.SandboxID == run.SandboxID)
		// RECOUNTED, the store under the counts: the claim and relaunch are one
		// row, counted once as the claim the relaunch opened on (countwrite.go), so
		// the recount moves nothing unless the job moved since.
		c.syncSeat(ctx, run.AgentHandle)
		return AnswerConsumed, nil
	}
	c.finish(ctx, latest, ending{fence: fenceOf(run)})
	// RECOUNTED, not assumed free. Where the ending owed nothing more the store has
	// no record of the run, so the recount keeps only the seat's other live runs and
	// the inbox hold lifts once the resumed turn is over. Where the record OWES THE
	// TURN'S END, the ending is decided here and finished by this node's retry,
	// armed at once on its own goroutine ([errAskAway]): a capture still being kept
	// and the log's write after it are not this frame's to wait on. The record
	// outlives the resume, its ending keeping the inbox held
	// ([Coordinator.oweEndingAfter]), until the retry has deleted it, recounted and
	// settled ([Coordinator.retryOwed]). That retry may be under way or done as
	// this recount reads: a delete and every recount written move the seat
	// ([Coordinator.moved]), so an older listing is read again.
	c.syncSeat(ctx, run.AgentHandle)
	// The ordinary ending: the turn came back and the run is finished.
	return AnswerConsumed, nil
}

// leadsAt is the turn's leads beside run's row as its resume reads them
// ([PendingStore.Leads]): where the store cannot give them, every lead the row
// counts is said as not kept, never none ([PendingRun.LeadsAppended]), and the
// read marked unread ([TurnLeads.Unread]), so the relaunch's job hands the
// marked leads on by number for a later read to tell. A resume is not refused for
// it, unlike an unreadable log: a lead told again is one the session may already
// show, and the brief says how many it cannot tell.
func (c *Coordinator) leadsAt(ctx context.Context, run PendingRun) TurnLeads {
	leads, err := c.pending.Leads(ctx, run)
	if err != nil {
		log.WarnContext(ctx, "sandbox_leads_part_unread", "turn_id", run.TurnID, "launch_id", run.LaunchID,
			"not_kept", run.LeadsAppended, "error", err.Error(),
			"detail", "every lead the run's record counts is told as not kept by the relaunch this resume "+
				"builds, rather than told again; the job that relaunch opens marks the ones this job's "+
				"record marks, so a later resume that reads them tells them")
		return TurnLeads{Cut: run.LeadsAppended, Unread: true}
	}
	return leads
}

// markResumeBegun records on a claim of a collected job what the resuming turn
// does with the job's round ([LaunchRecord.ResumeCarry]), as it begins
// ([ResumeRequest.Begin]), under the claimant's lease. An unsaid carry is none.
//
// BEST EFFORT, AND BOUNDED ([resumeCarryRecordBudget] plus the row's crossing,
// [Coordinator.rowContext]): nothing the turn does reads it, only readers of
// whether a relaunch would continue the job's session, so an untaken record is
// logged and read as not said, undecided either way.
func (c *Coordinator) markResumeBegun(ctx context.Context, run PendingRun, carry ResumeCarry) {
	if !carry.Valid() {
		return
	}
	ctx, cancel := c.rowContext(ctx, resumeCarryRecordBudget)
	defer cancel()
	if _, err := c.pending.MarkResumeBegun(ctx, run.TurnID, run.LaunchID, fenceOf(run), carry); err != nil {
		log.WarnContext(ctx, "sandbox_resume_carry_unrecorded", "turn_id", run.TurnID,
			"carry", string(carry), "error", err.Error(),
			"detail", "what the resumed turn does with its job's round was not recorded on the run; while "+
				"the turn runs, whether a relaunch continues the job's session reads as not said")
	}
}

// resumeCarryRecordBudget bounds the write recording what a resumed turn does
// with its job's round ([Coordinator.markResumeBegun]), beside the row's crossing
// time ([RowBudget]): FIVE SECONDS, like the session-start write
// (internal/engine's sessionStartRecordBudget): one compare-and-swap of one row
// a healthy store answers in milliseconds, and a store silent for five is in an
// outage this write waits out no better. On the resume's path before its turn
// runs, so a slow store holds the turn back by no more than that plus the row's
// bytes crossing (none in the store's process; up to thirteen minutes across the
// slowest allowed link for a row at its bound). The five alone cut a megabyte
// row while its bytes still crossed and the carry went unrecorded
// (crewlet/crewlet#1076).
const resumeCarryRecordBudget = 5 * time.Second

// claimedAnswer reports whether a claimed run carries a RECORDED answer its
// claim was taken for: the chat route's, which lives on the row, not a delivery.
func claimedAnswer(run PendingRun) bool {
	return run.ClaimedFrom == StatusAnswered && run.Answer != nil
}

// resumedFromWait reports whether a claimed run is carried on from waiting, by
// its recorded answer or by the engine ([claimedWaitOver]), never a completion:
// a deadline or a budget's hold is not a completion's to end or hold for it.
func resumedFromWait(run PendingRun) bool { return claimedAnswer(run) || claimedWaitOver(run) }

// settleAbandoned ends a run whose resume was abandoned, from its record, or
// while it is still the claim this resume took where that cannot be read
// ([Coordinator.settleClaimed]), and recounts the seat. A recorded reply no turn
// took goes back to the seat ([ErrAnswerOwed]); one the turn took is spent.
// SETTLED EVEN WHEN THE RECORD CANNOT BE READ: settling nothing left the row in
// the claim, the one state nothing recovers from.
//
// note is what the ending announces: nil for a resume whose turn ran (it
// published its own failed completion), the loss for one whose turn never began
// ([resumeBrokenDetail]). Reports whether the ending is this node's
// ([Coordinator.finish]); false is a run that moved on (the next holder fenced
// and revived it), whose fate this node must not announce.
func (c *Coordinator) settleAbandoned(ctx context.Context, run PendingRun, note *failureNote) bool {
	defer c.syncSeat(ctx, run.AgentHandle)
	settle, readErr := c.current(ctx, run)
	if readErr != nil {
		return c.settleClaimed(ctx, run, readErr, note)
	}
	_, ours := c.finish(ctx, settle, ending{fence: fenceOf(run), note: note})
	return ours
}

// spend is [CoordinatorOptions.Spent] for one recorded answer: its deliveries,
// as the answer carries them.
//
// BEFORE AN ANSWER IS LET GO OF, this makes its copies the reply: the original
// coming round after is a reply already handed back. A REFUSAL CARRIES NO
// DELIVERY, so letting one go spends nothing, which sends its reply back the way
// it came: the inbox's delivery comes round as the ordinary message
// ([AnswerRefused]). Spent by id once a turn keeps it ([Coordinator.spendRefused]).
func (c *Coordinator) spend(ctx context.Context, handle string, answer RecordedAnswer) {
	if c.spent == nil {
		return
	}
	if evs := answer.decodedEvents(); len(evs) > 0 {
		c.spent(ctx, handle, evs)
	}
}

// spendRefused is [CoordinatorOptions.Spent] for the refusal run holds, once a turn
// that took it KEEPS it: the deliveries its reply was made of, by the ids it names
// ([AnswerRefusal.EventIDs]), so the inbox's delivery is dropped when it comes
// round instead of running as a turn. KEPT means the turn returned having run,
// broke after it began (never retried), or died holding it ([Coordinator.reapTail]);
// never the take alone, which a turn breaking before reaching anybody gives back
// (the refusal may then be let go, sending the delivery back as an ordinary
// message; spent at the take it was lost). BY ID ALONE, all a refusal keeps; an
// unparseable id is skipped.
func (c *Coordinator) spendRefused(ctx context.Context, run PendingRun) {
	refusal, refused := run.Refused()
	if !refused || c.spent == nil {
		return
	}
	evs := make([]*events.Event, 0, len(refusal.EventIDs))
	for _, id := range refusal.EventIDs {
		if parsed, err := uuid.Parse(id); err == nil {
			evs = append(evs, &events.Event{ID: parsed})
		}
	}
	if len(evs) > 0 {
		c.spent(ctx, run.AgentHandle, evs)
	}
}

// spendTaken is what a turn that TOOK run's answer and died holding it spends:
// the answer's deliveries, or the refusal's ([Coordinator.spendRefused]).
func (c *Coordinator) spendTaken(ctx context.Context, run PendingRun) {
	if run.Answer == nil {
		return
	}
	c.spend(ctx, run.AgentHandle, *run.Answer)
	c.spendRefused(ctx, run)
}

// current is a claimed run as its record stands after the resumed turn returned.
// The LATEST, since the box to settle is the one it names now (a re-seeded run
// provisioned a fresh box; a relaunch has a new job). A gone record is answered
// with the snapshot, whose settle reclaims a box that may remain.
//
// THREE ANSWERS, NOT TWO: "here", "gone" and "the store could not be read" are
// three facts and the last must not read as either other. As a bool, callers read
// false as "settle nothing", leaving the row in [StatusResumed], which nothing
// recovers ([Coordinator.unclaim]); the error goes to [Coordinator.settleClaimed].
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

// endClaim ends a run while it is still the claim this call took, for an ending
// decided without reading the row (a failed settle read, [Coordinator.settleClaimed];
// a refused release, [Coordinator.unclaim]); it reports whether it ended the run
// and the error where the ending is kept ([errEndingOwed]). The condition moves
// to the store, so a relaunched run or a landed release is declined. Decided
// first, reclaimed second, the reclaim riding the recorded ending
// ([RecordedEnding.Reclaim]). See the package doc, "Ending a claim".
func (c *Coordinator) endClaim(ctx context.Context, run PendingRun, e ending) (bool, error) {
	// DETACHED, like every teardown: reached from failure paths and a drain-cancelled
	// handler, so the context is often dead, and a no-op settle is the stranded
	// row again. Each write gets the row's crossing time besides ([detachedEnding]).
	e.fence, e.whileIn, e.launch, e.reclaim = fenceOf(run), claimedOnly, run.LaunchID, true
	endCtx, done := detachedEnding(ctx)
	defer done()
	decided, err := c.endRecord(endCtx, run, e)
	return decided != nil, err
}

// settleClaimed ends a run whose record could not be read, WHILE IT IS STILL
// CLAIMED ([Coordinator.endClaim]). SILENCE IS NOT AN ANSWER HERE: both callers
// just took a suspended turn from the engine, and the row left is the claim itself
// (no poll reads a [StatusResumed] row, no redelivery re-claims it, no answer matches
// it, no reaper expires it), a turn destroyed in silence beside a held paused box,
// which [Coordinator.unclaim] ends rather than accept. The one hazard the read
// guarded (killing a job the turn RELAUNCHED in this box) the claim's license
// declines. note is what the ending announces, nil where the turn announced itself.
// Reports whether the ending is this node's, as [Coordinator.finish] does.
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
		// Not ours: the run moved on (a relaunch before the turn broke) or
		// somebody ended it; that party owns the box and the record.
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

// unclaim hands a claimed tail back to the exact status the claim took it from,
// only while it is still this call's claim ([Release]), counting the run into the
// set that status belongs to. It takes a context of its own, since the failure it
// undoes is often the cancellation. A hand-back the store refused ends the run
// while still the claim ([Coordinator.endClaim]); one it did not answer is kept
// owed (claimrelease.go). No stop is reported on success. Returns nil where the
// run is still in a retry's hands, the release's error where it was ended. See
// the package doc, "Handing a claim back".
func (c *Coordinator) unclaim(ctx context.Context, run PendingRun, detail string) error {
	return c.unclaimAt(ctx, run, detail, failure{}, nil, BudgetWindow{})
}

// unclaimHeld hands back a claim whose resume the seat's conditions refused,
// holding its resume until the refusal lifts ([Release.Hold]) and counting the
// window its relaunch was refused in ([Release.RelaunchRefusedIn]). A refusal
// only an event lifts is held until [answerWaitBackstop], or a turn's deadline if
// no later ([PendingRun.Deadline]), since nothing reaches a held row to end it.
// Returns the hold the completion is kept for ([Coordinator.keepHeldOnRow]). See
// the package doc, "Holding a refused resume".
func (c *Coordinator) unclaimHeld(ctx context.Context, run PendingRun, refusal Refusal) (Refusal, error) {
	if refusal.Until.IsZero() {
		refusal.Until = c.now().Add(answerWaitBackstop)
	}
	if !run.Deadline.IsZero() && run.Deadline.Before(refusal.Until) {
		refusal.Until = run.Deadline
	}
	log.InfoContext(ctx, "sandbox_resume_held",
		"turn_id", run.TurnID, "agent", run.AgentHandle, "condition", string(refusal.Condition),
		"reason", refusal.Reason, "until", refusal.Until.UTC().Format(time.RFC3339),
		"detail", "the job stays collected in its paused box and its seat held behind it; "+
			"its completion is announced again when the refusal lifts")
	return refusal, c.unclaimAt(ctx, run, resumeUnrevertedDetail, failure{}, &refusal, refusal.Window)
}

// unclaimAt is [Coordinator.unclaim] for a claim whose collection or following
// resume failed: failed is what the release counts onto the job's record; detail
// is the sentence an ending reaches an operator's board with if the store refuses
// the release; hold holds the resume back ([Release.Hold]); refusedIn is the
// window the relaunch was refused below its floor in ([Release.RelaunchRefusedIn]).
// Every hand-back is recorded as out first by [Coordinator.handingBackLocked], so
// no claim of the job starts meanwhile ([Coordinator.claimCompletion]), and made
// by [Coordinator.unclaimOwing]; an unanswered one is kept owed (claimrelease.go).
func (c *Coordinator) unclaimAt(ctx context.Context, run PendingRun, detail string, failed failure,
	hold *Refusal, refusedIn BudgetWindow,
) error {
	// AND WHAT THE CLAIM LEARNED OF THE BOX HOST'S STORE goes onto the job's
	// record, so a restart or next holder reads it before acknowledging a
	// delivery no clock of its own covers (renew.go).
	silent, answered := c.hostStoreSeen(ctx, run, failed)
	c.mu.Lock()
	cr, moved := c.handingBackLocked(run, detail, failed, hold)
	cr.release.HostStoreSilentSince, cr.release.HostStoreAnswered = silent, answered
	cr.release.RelaunchRefusedIn = refusedIn
	c.mu.Unlock()
	moved()
	return c.unclaimOwing(ctx, cr)
}

// handingBackLocked records the release that hands run's claim back among the
// releases out, and its write in the run's entry, in one step under mu, returning
// it for [Coordinator.unclaimOwing] to make after unlock. A recount counts the run
// as the claim while the release is out; a caller whose own write failed records
// the release in the step taking that write's answer ([Coordinator.park]). The
// release it replaces (an earlier job's, still owed) is answered first as not
// landed, or it would hold every later move behind it. See the package doc,
// "Handing a claim back".
func (c *Coordinator) handingBackLocked(run PendingRun, detail string, failed failure, hold *Refusal,
) (*claimRelease, func()) {
	cr := &claimRelease{run: run, release: releaseFor(run, failed, hold), detail: detail}
	// THE RELEASE WHOSE LANDING COUNTS THE RUN AS WAITING FOR THE OPERATOR
	// ([blockedResume]) is recorded before the one it replaces is answered, so an
	// earlier-counted run keeps its instant.
	if failed.blocked != "" {
		c.blockedHandBackLocked(cr, failed.blocked)
	}
	moved := func() {}
	if earlier := c.releases[run.TurnID]; earlier != nil {
		moved = c.wroteCountLocked(earlier.write, false)
		c.blockedAnsweredLocked(earlier, false)
	}
	cr.write = c.writingCountLocked(run, StatusResumed, cr.release.To)
	c.releases[run.TurnID] = cr
	return cr, moved
}

// releaseAnswered acts on the store's answer to cr's release as
// [Coordinator.unclaimAt] reports it: a failed release ends the run while still
// the claim, one that moved on is left to what moved it, and a landed one has
// already moved the seat's count in the step taking its answer
// ([Coordinator.unclaimOwing]).
func (c *Coordinator) releaseAnswered(ctx context.Context, cr claimRelease, released bool, err error) error {
	run, to := cr.run, cr.release.To
	switch {
	case err != nil:
		log.ErrorContext(ctx, "sandbox_claim_revert_failed",
			"turn_id", run.TurnID, "revert_to", to, "error", err.Error(),
			"detail", "the claim could not be handed back, so nothing would ever pick "+
				"this run up; it is ended and announced instead, unless the hand-back "+
				"landed after all")
		ended, endErr := c.endClaim(ctx, run, ending{
			unused: claimedAnswer(run),
			note:   &failureNote{reason: types.ExecutorFailureClaimStranded, detail: cr.detail},
		})
		// AUTHORITATIVE, not assumed: the ending and this call each moved things
		// without knowing what the other found.
		c.syncSeat(ctx, run.AgentHandle)
		switch {
		case ended:
			return fmt.Errorf("sandbox: handing back the claim on %s (to %s): %w", run.TurnID, to, err)
		case endErr != nil:
			// THE ENDING IS KEPT, and whether the hand-back landed is open: this
			// node finishes it or finds the run handed back and leaves it to its
			// retry. The run is not known gone, so the caller takes the retry's path.
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
	}
	// LANDED: the claim's count moved with it in the step that settled it
	// ([Coordinator.unclaimOwing]).
	return nil
}

// claimedFrom is the status a claimed run held before its claim. A claim always
// records it; running is the only status a completion takes a tail out of, so a
// row without the record goes back to it.
func claimedFrom(run PendingRun) string {
	if run.ClaimedFrom == "" {
		return StatusRunning
	}
	return run.ClaimedFrom
}

// settleFailed ends a run that failed: reaps the box, finishes the run, frees the
// seat, and SAYS SO. Every step is attempted whatever the earlier ones did, since
// none failing may leave a seat parked on an over run. This path destroys a turn
// (record deleted, box gone, completion acked) and once published nothing, an
// identical silence to the seat, dashboard and requester. Announced by the ending
// itself ([Coordinator.endRecord]), only for the ending decided on the run: a newer
// lease's, or one somebody else ended or DECIDED first, is that party's to explain
// (a second announcement would name a reason the run did not end for). The engine
// is told on the same gate ([CoordinatorOptions.Stopped]); a recorded reply no turn
// took goes back to the seat ([ErrAnswerOwed]). Reports whether the ending is this
// node's ([Coordinator.finish]).
func (c *Coordinator) settleFailed(ctx context.Context, run PendingRun, reason, detail string) bool {
	return c.settleNoted(ctx, run, &failureNote{reason: reason, detail: detail})
}

// settleNoted is [Coordinator.settleFailed] over a whole announcement. The run
// leaves the set it was counted in (resumed or launching) as its delete lands, the
// answer moving it once ([ending.counted]); counted out afterwards, a recount
// could free a seat whose other run still worked. A run no longer this node's has
// no delete to move it, so the seat is recounted from the store. See the package
// doc, "Seat counts".
func (c *Coordinator) settleNoted(ctx context.Context, run PendingRun, note *failureNote) bool {
	_, ours := c.finish(ctx, run, ending{fence: fenceOf(run), note: note, counted: true})
	if !ours {
		c.syncSeat(ctx, run.AgentHandle)
	}
	return ours
}

// The four sentences a stranded claim reaches an operator's board with, one per
// caller because what an operator does differs: an unannounced question was seen
// by nobody; an announced but unrecorded one may be answered and never matched;
// an unrevertable resume or collection is finished work in a box. All name the
// coordination store, since an unhandable claim brought each here.
const (
	parkUnannouncedDetail = "the executor stopped to ask a person a question, but neither the " +
		"question could be announced nor its own claim given back to the " +
		"coordination store, so nobody was asked and the turn cannot be continued; the " +
		"work it pushed, if any, is on its branch"

	parkUnrecordedDetail = "the executor stopped to ask a person a question, but neither the " +
		"question nor its own claim could be written to the coordination store, so " +
		"the turn cannot be continued and nobody can answer it; the work it pushed, if " +
		"any, is on its branch"

	resumeUnrevertedDetail = "the executor's launch finished but the turn it belongs to could " +
		"not be re-entered, and the executor's claim could not be given back to the coordination " +
		"store for another attempt, so the turn cannot be continued; the work it pushed, " +
		"if any, is on its branch"

	collectUnrevertedDetail = "the executor's launch finished but its box could not be read " +
		"back, and the executor's claim could not be given back to the coordination store for another " +
		"attempt, so its result is lost; the work it pushed, if any, is on its branch"
)

// resumeBrokenDetail is the sentence a run whose resume broke before its turn
// began reaches an operator's board with: nothing ran and a retry would reach the
// same defect, so the run is ended.
const resumeBrokenDetail = "the turn this executor's launch belongs to could not be re-entered — " +
	"resuming it broke before the turn began, and another attempt would reach the same " +
	"defect — so the turn cannot be continued; the work it pushed, if any, is on its branch"

// reportStopped tells the engine one suspended turn has stopped: one helper, so
// "a stop is reported" is one statement, a no-op where nothing is wired. Its
// callers are [Coordinator.park] and [Coordinator.endRecord] and no other may
// become one: an ending reports by deleting the record. See
// [CoordinatorOptions.Stopped].
func (c *Coordinator) reportStopped(ctx context.Context, run PendingRun) {
	if c.stopped == nil {
		return
	}
	c.stopped(ctx, run.AgentHandle, run.TurnID)
}

// FailRun settles a run the turn that launched it cannot suspend into. The engine
// calls it when the conversation a resume would re-enter never reached the record
// (none recorded, not serializable, write failed): the job already executes in its
// box, and marking the record alone would leave it running for nobody, since an
// inactive record is read by no recovery pass. So it is settled like any lost turn while
// this node owns the seat. ONLY A RUN STILL LAUNCHING is settled, the one state the
// failed suspension proves nothing else will act on: a gone record was ended by
// somebody else, a running run carries a conversation after all (a write reported
// failed can have landed) and the poll resumes it, any other state belongs to a
// party that moved it on. An unreadable record is returned (the box is unknown).
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

// announceEnding publishes a decided ending's announcement (the lost run, to the
// board and its seat) under the identity the decision recorded: TWO PUBLISHES, the
// events copy (dashboard, event store) and the per-seat control copy reaching the
// node that ran the turn. THE SAME EVENT ON EVERY ATTEMPT: its id derives from the
// run and ending ([endingEventID]) and its instant is the decision's, so a repeat
// after a crash is one event to the event store (key (event_time, event_id)). Made
// BEFORE the record is deleted (after it, a crash between lost it). The events copy
// must land, being the only account of how the run ended: a refused publish keeps
// the ending for retry ([Coordinator.oweEnding]); the control copy is best effort. A
// STOP IS ANNOUNCED AS ONE ([types.ExecutorStopped]), never a loss ([PendingRun.Stop]).
func (c *Coordinator) announceEnding(ctx context.Context, run PendingRun) error {
	ending := run.Ending
	trace := events.TraceContext{TraceID: run.TraceID, ParentSpanID: run.SpanID}
	var ev *events.Event
	what := ending.Reason
	if stop := run.Stop; stop != nil {
		what = "stopped by " + stop.By
		ev = events.New(types.ExecutorStopped{
			Agent: run.AgentID, AgentHandle: run.AgentHandle, RoleName: run.Role,
			TurnID: run.TurnID, WorkKey: run.WorkKey, SandboxID: run.SandboxID,
			Harness: run.Harness, LaunchID: run.LaunchID,
			StoppedBy: stop.By, StoppedByKind: stop.ByKind,
			OperatorID: stop.OperatorID, Reason: stop.Reason,
		}, trace)
	} else {
		detail := ending.Detail
		switch {
		case run.EndingReturnsRefusal:
			detail += refusedReturnedDetail
		case ending.Returned:
			detail += replyReturnedDetail
		}
		ev = events.New(types.ExecutorFailed{
			Agent: run.AgentID, AgentHandle: run.AgentHandle, RoleName: run.Role,
			TurnID: run.TurnID, WorkKey: run.WorkKey, SandboxID: run.SandboxID,
			Harness: run.Harness,
			Reason:  ending.Reason, Detail: redact.Secrets(detail), ErrorKind: ending.ErrorKind,
		}, trace)
	}
	ev.ID, ev.Timestamp = endingEventID(run), ending.At.UTC()
	ev.Source = run.Role
	if err := c.queue.Publish(ctx, topics.Event(ev.Type), ev); err != nil {
		return fmt.Errorf("sandbox: announcing the end of run %s (%s): %w", run.TurnID, what, err)
	}
	if control := controlSubject(run.AgentID); control != "" {
		if err := c.queue.Publish(ctx, control, ev); err != nil {
			log.WarnContext(ctx, "sandbox_failure_control_failed",
				"turn_id", run.TurnID, "reason", what, "error", err.Error())
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
// a run that goes on without its box: one parked straight into reseed. A box
// whose kill failed is its host's sweep's then, the row naming it no more.
func (c *Coordinator) teardown(ctx context.Context, run PendingRun) {
	killCtx, cancel := detached(ctx)
	_ = c.reclaimBox(ctx, killCtx, run)
	cancel()
	if run.SandboxID == "" {
		return
	}
	// THE RECORD'S OWN CONTEXT, not the kill's: that grace is a box call's and the
	// release writes the row whole.
	releaseCtx, release := c.detachedRecord(ctx)
	defer release()
	if err := c.pending.ReleaseBox(releaseCtx, run.TurnID); err != nil {
		log.WarnContext(ctx, "sandbox_release_failed", "turn_id", run.TurnID, "error", err.Error())
	}
}

// ending is one decision to end a run as its caller makes it: the license it is
// taken under, whether a reply its turn took went unused, whether the box is
// reclaimed, and what it announces. ONE VALUE, because a decision can outlive its
// call: one the store could not confirm is kept and made again by this node's
// retry ([Coordinator.oweEnding]) on these terms; once RECORDED on the row
// ([RecordedEnding]) the recorded terms count, whoever finishes it.
type ending struct {
	// fence, whileIn and launch are the license: see [License].
	fence   Fence
	whileIn []string
	launch  string

	// unused is whether the ending lets the recorded answer go back to the seat
	// though a turn TOOK it: the claim's own ending after its turn gave the claim
	// back as a retry (so nothing it did reached anybody) and the release could
	// not land, so no retry will hand the reply to a turn again. Every other
	// ending lets go only a reply no turn took, which the store refuses to delete
	// with the row ([ErrAnswerOwed]), and spends one a turn took.
	unused bool

	// reclaim is whether the box is reclaimed after the decision, by whichever
	// attempt finishes it: every ending not having reclaimed its own box, the
	// store's fence check deciding whether this call may kill it
	// ([Coordinator.finish]).
	reclaim bool

	// note is what the ending announces, nil for none ([Coordinator.announceEnding]).
	note *failureNote

	// stop is the person whose pause ended the run, nil except for a stop's
	// ([Coordinator.StopSeat]); announced in place of a note.
	stop *RunStop

	// counted is whether this node counts the run in the set its status names, so
	// the delete moves it out of the seat's counts as its answer is taken, once
	// (countwrite.go): a run this node launched or claimed and settles itself
	// ([Coordinator.settleNoted], [Coordinator.StopSeat], [Coordinator.abandon]).
	// Kept with the ending so the retry moves the count as its own delete lands.
	// Every other ending moves nothing at its delete: its run is one this node does
	// not count yet (a recovery's) or its caller recounts afterwards.
	counted bool
}

// decision is the ending as the store records it.
func (e ending) decision() Decision {
	d := Decision{
		License: License{Fence: e.fence, WhileIn: e.whileIn, Launch: e.launch, Unused: e.unused},
		Reclaim: e.reclaim,
		Stop:    e.stop,
	}
	if e.note != nil {
		d.Reason, d.Detail, d.ErrorKind = e.note.reason, e.note.detail, e.note.errorKind
	}
	return d
}

// failureNote is a lost run's announcement: its reason and the sentence that
// reaches an operator's board.
type failureNote struct{ reason, detail, errorKind string }

// replyReturnedDetail is what an announcement adds when the ending hands a
// person's answer back to the seat ([RecordedEnding.Returned]), and
// refusedReturnedDetail when what goes back is a reply too large for the run,
// whose refusal it held untold ([PendingRun.EndingReturnsRefusal]).
const (
	replyReturnedDetail   = "; the answer a person gave it goes back to the seat"
	refusedReturnedDetail = "; a reply too large for it to take, which it was never told of, goes back " +
		"to the seat as the message it was"
)

// errEndingOwed marks an ending KEPT for this node's retry
// ([Coordinator.oweEnding]): its decision could not be confirmed or a step (the
// reply handed back, the announcement, the delete) did not land.
var errEndingOwed = errors.New("sandbox: the run's ending is kept until it can be finished")

// endRecord ends a run: it DECIDES the ending ([PendingStore.DecideEnding]) or finds
// one decided, which it finishes in place of its own, and FINISHES it
// ([Coordinator.finishEnding]): reply handed back, box reclaimed, announced, deleted.
// THE ONE PLACE A RUN IS ENDED, so the STOP is reported here, keyed to the decision
// (enumerated endings missed one each round) and only where it is this call's, so a
// racer neither announces a wrong reason nor drops another's hold; also for a turn
// that came back (nothing to drop). Over-reporting is a map lookup, under-reporting
// the defect ([CoordinatorOptions.Stopped]). EXACTLY ONE ANNOUNCEMENT, whoever
// finishes ([Coordinator.announceEnding]). AN UNFINISHED ENDING IS KEPT, retried here
// with the inbox held ([Coordinator.oweEnding]) or by the next holder. Returns the
// decided ending (maybe another's) or nil, and [errEndingOwed] for retrying callers.
func (c *Coordinator) endRecord(ctx context.Context, run PendingRun, e ending) (*RecordedEnding, error) {
	return c.endRun(ctx, run, e, false)
}

// endRun is [Coordinator.endRecord], saying whether its finish may ASK another
// node's host to read a box ([Coordinator.finishEnding]): only this node's own
// retry of a kept ending may ([Coordinator.retryOwed], on the coordinator's own
// context). Every other caller is a frame somebody waits on (pause watch, seat
// acquisition, completion handler), so an ending needing such a read is decided
// there and finished by that retry at once, off the frame.
func (c *Coordinator) endRun(ctx context.Context, run PendingRun, e ending, mayAsk bool,
) (*RecordedEnding, error) {
	decideCtx, decideDone := c.endingCall(ctx)
	decided, ok, err := c.pending.DecideEnding(decideCtx, run.TurnID, e.decision())
	decideDone()
	if err != nil {
		c.reportStopped(ctx, run)
		// FORGOTTEN ON THE STOP'S GATE, for its asymmetry: the decision may have
		// landed, and a count for a run nothing will offer a delivery again is a map
		// entry never dropped ([answerBudget]).
		c.clearAnswerAttempts(run.AgentHandle, run.TurnID)
		return nil, c.keepEnding(ctx, run, e, fmt.Errorf("sandbox: deciding the end of run %s: %w",
			run.TurnID, err))
	}
	if !ok {
		return nil, nil
	}
	// THE RUN IS SETTLED HERE, whatever finishes it: its end is decided, so no
	// collection of its job is made again and the clock on its box host's store
	// goes now ([hostUnsettled]), a lost job's carried round included; not at the
	// delete, which an end the executor log is owed keeps minutes behind. So does a
	// re-fire's announcement still owed ([refirePublish]).
	c.runSettled(decided.TurnID)
	c.reportStopped(ctx, decided)
	c.clearAnswerAttempts(decided.AgentHandle, decided.TurnID)
	c.noteLaunchStop(decided)
	ending := *decided.Ending
	finished, err := c.finishOnce(ctx, decided, mayAsk, e.counted)
	switch {
	case !finished && !mayAsk:
		// ANOTHER ATTEMPT ON THIS NODE IS FINISHING IT, and this node's retry keeps
		// it too, holding the seat's mail until it lands and asked again when that
		// attempt returns ([Coordinator.finishOnce]): finished, the retry finds the
		// record gone; kept, it finishes it. Left to that attempt alone, the ending
		// held nothing whenever its series was gone (a retry under way while the
		// seat was released and retaken, waiting on a keep or the log) and the
		// seat's mail ran while the record still owed the turn's end. Not this
		// frame's failure. Kept ON THE DECISION'S TERMS, licensed for every status
		// and job: the retry finishes the decision recorded.
		kept := e
		kept.whileIn, kept.launch = Active, EveryLaunch
		c.oweEndingAfter(ctx, decided, kept, answerRetryCeiling)
		return &ending, nil
	case !finished:
		return &ending, c.keepEnding(ctx, decided, e, errEndingBusy)
	case errors.Is(err, errAskAway):
		// DECIDED, its session closed, FINISHED BY THIS NODE'S OWN RETRY now: not a
		// failure.
		c.oweEndingAfter(ctx, decided, e, 0)
		return &ending, nil
	case errors.Is(err, errAwaitsFrame) && !mayAsk:
		// THE TURN'S FRAME IS STILL RUNNING HERE (this frame's own, a relaunch it
		// made being abandoned) and ends the turn: its settle finishes the ending
		// when it returns, and this node's retry if not ([Coordinator.framing]).
		c.oweEndingAfter(ctx, decided, e, answerRetryCeiling)
		return &ending, nil
	case err != nil:
		return &ending, c.keepEnding(ctx, decided, e, err)
	}
	return &ending, nil
}

// errEndingBusy is a decided ending another attempt on this node is finishing
// ([Coordinator.finishOnce]): left to it, and asked again by this node's retry
// the moment it returns.
var errEndingBusy = errors.New("sandbox: another attempt on this node is finishing the run's ending")

// errAwaitsFrame is a decided ending whose turn's end is still its resumed
// frame's to decide, that frame running on this node ([Coordinator.framing]).
var errAwaitsFrame = errors.New("sandbox: the run's turn is still running on this node, and decides its own end")

// finishOnce is [Coordinator.finishEnding] made by ONE attempt on this node at a
// time per turn, answering false and doing nothing where another is finishing it.
// An ending's steps are harmless to REPEAT, not to make TWICE AT ONCE: two attempts
// reading the row before either owed the turn's end both answered the colleague
// whose ask the turn took up ([CoordinatorOptions.CloseStopped],
// [CoordinatorOptions.TurnLost]) (a stop decided on the pause watch while its job
// launched was finished there and by the launch, the asker waking to two answers).
// The other attempt keeps the ending for the retry ([Coordinator.endRun]), asked
// again the moment the first returns, not on the hand-back's spacing.
func (c *Coordinator) finishOnce(ctx context.Context, run PendingRun, mayAsk, counted bool) (bool, error) {
	c.mu.Lock()
	if c.finishing[run.TurnID] {
		c.mu.Unlock()
		return false, nil
	}
	if c.finishing == nil {
		c.finishing = map[string]bool{}
	}
	c.finishing[run.TurnID] = true
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		delete(c.finishing, run.TurnID)
		if r := c.retries[run.TurnID]; r != nil && r.ending != nil && r.stop != nil {
			c.scheduleOwedLocked(run.TurnID, 0)
		}
	}()
	return true, c.finishEnding(ctx, run, mayAsk, counted)
}

// frameBegins and frameEnds bracket a resumed turn's frame on this node
// ([Coordinator.resumeAndSettle]); framing reports whether one of turnID's runs
// here. An ending decided while the frame still runs (its relaunch abandoned under
// it, [Coordinator.abandon]) leaves the turn's end to that frame, which owes its
// own account on the row ([Coordinator.OweEnd]); decided by the ending instead,
// the end was a second account (the colleague answered twice) or none (the row and
// capture stamps deleted first). In memory, since another node's frame is one the
// seat moved off.
func (c *Coordinator) frameBegins(turnID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.frames == nil {
		c.frames = map[string]int{}
	}
	c.frames[turnID]++
}

func (c *Coordinator) frameEnds(turnID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.frames[turnID] > 1 {
		c.frames[turnID]--
		return
	}
	delete(c.frames, turnID)
	// AN ENDING THAT WAITED ON THE FRAME is asked again when it returns.
	if r := c.retries[turnID]; r != nil && r.ending != nil {
		c.scheduleOwedLocked(turnID, 0)
	}
}

func (c *Coordinator) framing(turnID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.frames[turnID] > 0
}

// errAskAway is a decided ending whose next step may not be made on the frame
// that reached it ([Coordinator.endRun]): a read of a box on another node's host,
// a job's session take, or the turn's end on the executor log, each waiting on
// something not this frame's to wait on. Handed to this node's retry, which runs
// on the coordinator's own lifetime and may.
var errAskAway = errors.New("sandbox: the ending's next step is made off this frame, by its retry")

// finishEnding does what a decided ending ([PendingRun.Ending]) still has to do,
// in a fixed order, deleting the record last: let a held reply go, publish owed
// copies, end and read a stopped or lost job before its box goes (a lost one's
// served notes settled, then the row read again), reclaim the box, announce, hand
// a loss to the engine or close a stopped turn, publish unreported notes, record
// the turn's end on the executor log, delete. Waits not the frame's own run only
// on the ending's retry (mayAsk); elsewhere it is handed over ([errAskAway]).
// Every step is harmless repeated, so a crash resumes where it stopped; a refused
// delete ([ErrHandBackOwed], [ErrAnswerOwed], [ErrEndOwed]) reruns the steps. See
// the package doc, "Finishing an ending".
func (c *Coordinator) finishEnding(ctx context.Context, run PendingRun, mayAsk, counted bool) error {
	// THE ROW AS IT STANDS, not as the caller's decision read it: an earlier
	// attempt may have owed or recorded the turn's end, and a step off a stale copy
	// is made twice. A gone row or another ending was finished by whoever got
	// there first. AND WHETHER THIS NODE'S STORE ANSWERED, by which a stop's read of
	// a box whose host's store is silent is judged ([Coordinator.readStopped]): the
	// silence is the host's alone only where this node's is not.
	rowRead := false
	readCtx, readDone := c.endingRead(ctx)
	current, found, err := c.pending.Get(readCtx, run.TurnID)
	readDone()
	switch {
	case err != nil:
		// Read as handed: each step is harmless repeated, the delete decides.
	case !found || current.Ending == nil || current.Ending.ID != run.Ending.ID:
		return nil
	default:
		run, rowRead = current, true
	}
	var handed []string
	reclaimed, announced, ended, named := false, false, false, false
	for range endingRounds {
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
				// THE SAME COPIES AGAIN: they are out and clearing them did not land;
				// republishing changes nothing, so the ending waits for its retry.
				return fmt.Errorf("sandbox: ending run %s: the reply it owed its seat is handed "+
					"back but could not be cleared from it", run.TurnID)
			}
			if err := c.deliverHandBack(ctx, run); err != nil {
				return fmt.Errorf("sandbox: ending run %s, which owes its seat a reply it could not "+
					"hand back: %w", run.TurnID, err)
			}
			handed = owed
		}
		if run.Stop != nil && !run.Stop.Collected {
			// THE SESSION FIRST: see [Coordinator.collectStopped].
			if c.ended != nil {
				c.ended(run.TurnID)
			}
			// A JOB THAT STARTED IN A BOX IS ENDED BY ITS READ, which waits for
			// it ([Coordinator.endJob]), never on this frame.
			if !mayAsk && run.SandboxID != "" && run.CommandID != "" {
				return errAskAway
			}
			collected, err := c.collectStopped(ctx, run, rowRead)
			if err != nil {
				return err
			}
			run = collected
		}
		if run.Ending.Reclaim && !reclaimed && c.capturesLoss(run) {
			// THE SESSION FIRST, as a stop's: a lost job may still be working, and
			// nothing it does after this acts as the seat (it kept acting through
			// its take when the reclaim was first to close it).
			if c.ended != nil {
				c.ended(run.TurnID)
			}
			// THE LAST READ OF THE BOX, before the reclaim takes it (session.go).
			if !mayAsk {
				return errAskAway
			}
			if err := c.keepLostSession(ctx, run, rowRead); err != nil {
				return fmt.Errorf("sandbox: ending run %s, whose lost job's box is not read yet: %w",
					run.TurnID, err)
			}
			if run.asksReceipts() {
				// THE ROW AGAIN, holding the verdict the read wrote, which step 8
				// reports from.
				readCtx, readDone := c.endingRead(ctx)
				current, found, err := c.pending.Get(readCtx, run.TurnID)
				readDone()
				switch {
				case err != nil:
					return fmt.Errorf("sandbox: ending run %s, whose row could not be read again after its "+
						"lost job's box: %w", run.TurnID, err)
				case !found || current.Ending == nil || current.Ending.ID != run.Ending.ID:
					return nil
				}
				run, rowRead = current, true
			}
		}
		if run.Ending.Reclaim && !reclaimed {
			// THE BOX THE ROW NAMES, on a context of its own (often dead on failure
			// paths). One whose kill failed keeps its own record, and its host's
			// sweep tries again once the row is gone.
			killCtx, cancel := detached(ctx)
			_ = c.reclaimBox(ctx, killCtx, run)
			cancel()
			reclaimed = true
		}
		if run.announces() && !announced {
			if err := c.announceEnding(ctx, run); err != nil {
				return err
			}
			announced = true
		}
		if len(run.OwedEnd) == 0 && !run.EndRecorded {
			// AFTER THE ANNOUNCEMENT, the record of the loss or stop the end and the
			// colleague's answer tell of, and ONCE: owed on the row and recorded
			// there once the log has it ([PendingRun.EndRecorded]), found by every
			// later attempt. NOT WHILE THE TURN'S OWN FRAME RUNS HERE, which ends the
			// turn itself ([Coordinator.framing]).
			if c.framing(run.TurnID) {
				return errAwaitsFrame
			}
			// THE RUN'S CALLS STILL IN FLIGHT are waited for before the turn's end
			// is decided on its log ([Coordinator.drainEnding]), only by the
			// ending's retry off any frame; other attempts hand such a run to it
			// ([errAskAway]).
			if err := c.drainEnding(ctx, run, mayAsk); err != nil {
				return err
			}
			ending, owes, err := c.endingOfRun(ctx, run)
			if err != nil {
				return fmt.Errorf("sandbox: ending run %s, whose stopped turn could not be closed "+
					"on the record: %w", run.TurnID, err)
			}
			if owes {
				owing, err := c.oweEnd(ctx, run, ending)
				if err != nil {
					return err
				}
				run = owing
			}
		}
		if !ended && c.steersEnded != nil && run.endsNotes() {
			if err := c.steersEnded(ctx, run); err != nil {
				return fmt.Errorf("sandbox: ending run %s, whose notes' ends could not be "+
					"recorded: %w", run.TurnID, err)
			}
			ended = true
		}
		if len(run.OwedEnd) > 0 {
			if !mayAsk && c.keepsSessions() {
				return errAskAway
			}
			waited, found, err := c.awaitEndCaptures(ctx, run)
			switch {
			case err != nil:
				return fmt.Errorf("sandbox: ending run %s, whose turn's end waits for its capture: %w",
					run.TurnID, err)
			case !found:
				// GONE WHILE IT WAITED: another attempt recorded the end and
				// deleted the record.
				return nil
			}
			published, err := c.publishEnd(ctx, waited)
			if err != nil {
				return fmt.Errorf("sandbox: ending run %s, whose turn's end the executor log has not "+
					"taken yet: %w", run.TurnID, err)
			}
			run = published
		}
		// AND WHAT IS STILL IN FLIGHT AS THE RECORD GOES, named: its update will
		// find no record, so this line is what is left.
		if !named {
			c.nameDeletedInFlight(ctx, run)
			named = true
		}
		settled, deleted, err := c.deleteRecord(ctx, run, counted)
		switch {
		case err == nil:
			// GONE, by this delete or whoever finished first. A delete of this
			// call's moves the seat for a recount already reading; the box host's
			// store clock went with the decision ([Coordinator.endRun]).
			if deleted {
				c.moved(run.AgentHandle)
			}
			return nil
		case errors.Is(err, ErrHandBackOwed), errors.Is(err, ErrAnswerOwed), errors.Is(err, ErrEndOwed):
			run = settled
		default:
			return fmt.Errorf("sandbox: ending run %s, whose record could not be deleted: %w",
				run.TurnID, err)
		}
	}
	return fmt.Errorf("sandbox: ending run %s: it kept owing its seat a reply, or the log its turn's "+
		"end, after recording one", run.TurnID)
}

// deleteRecord is an ending's delete of run's record ([PendingStore.Finish]) as
// an ending's store call ([Coordinator.endingCall]). For a run this node counts
// ([ending.counted]) it moves the run out of the seat's sets as its answer is
// taken: recorded before sending so a recount meanwhile counts the run where it
// is, row listed or not, and settled on every way out (a panic included) so an
// untaken answer moves nothing (countwrite.go).
func (c *Coordinator) deleteRecord(ctx context.Context, run PendingRun, counted bool,
) (PendingRun, bool, error) {
	if !counted {
		finishCtx, finishDone := c.endingCall(ctx)
		defer finishDone()
		return c.pending.Finish(finishCtx, run.TurnID, run.Ending.ID)
	}
	write := c.writingCount(run, run.Status, "")
	defer c.settleWrite(write)
	finishCtx, finishDone := c.endingCall(ctx)
	settled, deleted, err := c.pending.Finish(finishCtx, run.TurnID, run.Ending.ID)
	finishDone()
	c.mu.Lock()
	moved := c.wroteCountLocked(write, err == nil && deleted)
	c.mu.Unlock()
	moved()
	return settled, deleted, err
}

// endingOfRun is how the turn of run (a loss or a stop) ended, from the engine,
// and whether it owes the executor log an end: a loss's account
// ([CoordinatorOptions.TurnLost]) or a stop's, its parked turn closed first
// ([CoordinatorOptions.CloseStopped]). A turn that came back owes none here: its
// own frame decided its end.
func (c *Coordinator) endingOfRun(ctx context.Context, run PendingRun) (executorkeep.Ending, bool, error) {
	switch {
	case run.lost() && c.turnLost != nil:
		return c.turnLost(ctx, run, c.runCalls(ctx, run)), true, nil
	case run.Stop != nil && c.closeStopped != nil:
		ending, err := c.closeStopped(ctx, run, c.runCalls(ctx, run))
		if err != nil {
			return executorkeep.Ending{}, false, err
		}
		return ending, true, nil
	}
	return executorkeep.Ending{}, false, nil
}

// RunCalls is a run's bridged log as its ending read it, for the engine's account
// of whether the turn's reply reached anybody ([CoordinatorOptions.TurnLost],
// [CoordinatorOptions.CloseStopped]): Log, the run's job's calls and the fold of
// earlier rounds ([BridgeLog.PriorCalls]); Unread, why the store could not
// answer, nil where it did. AN UNREAD LOG IS A DELIVERY NOT KNOWN, never "no
// calls": read as empty, a turn that did reply would be recorded as one that did
// not, the false account the turn's end exists to prevent.
type RunCalls struct {
	Log    BridgeLog
	Unread error
}

// runCalls reads run's log for its ending ([RunCalls]): ONE READ PER ENDING, at
// the step deciding the turn's end, as a resume reads its own
// ([ResumeRequest.Bridged]), but the turn's calls whatever job the log is on
// ([PendingStore.TurnLog]): an ending reaps a row whose relaunch never opened the
// log to its job as readily as any, and the rounds' calls are the turn's all the
// same.
func (c *Coordinator) runCalls(ctx context.Context, run PendingRun) RunCalls {
	readCtx, readDone := c.endingRead(ctx)
	defer readDone()
	calls, err := c.pending.TurnLog(readCtx, run)
	if err != nil {
		return RunCalls{Unread: err}
	}
	return RunCalls{Log: calls}
}

// endingRounds bounds [Coordinator.finishEnding]'s rounds. A round repeats only
// when the delete is refused for what the row still owes (copies, an answer or the
// turn's end another attempt left after this round read it); a decided ending
// takes no new answer ([ErrRunEnding]), so a refusal names one of a finite set: an
// answer to let go, an earlier decline's copies, the let-go's copies, an end. Five
// rounds is one each plus the delete. The same-copies guard stops sooner, so the
// bound is reached only by another node finishing in step, the ending then kept
// for this node's retry.
const endingRounds = 5

// keepEnding keeps an ending that could not be finished for this node's retry
// ([Coordinator.oweEnding]) and returns cause as the kept ending's error.
func (c *Coordinator) keepEnding(ctx context.Context, run PendingRun, e ending, cause error) error {
	c.oweEnding(ctx, run, e)
	return fmt.Errorf("%w: %w", errEndingOwed, cause)
}

// letGoAnswer lets go of the recorded answer a run with a decided ending still owes
// the seat ([PendingStore.OweHandBack]): its copies are recorded on the row as owed
// in the write taking the answer off it, for the ending to hand back before the
// delete. Returns the row as left. SPENT BEFORE THE WRITE
// ([CoordinatorOptions.Spent]): the carrying delivery may still come round, and
// spent but not let go the reply stays on the row (the next reader lets go), let go
// but not spent original and copy would both reach the seat. A ROW ALREADY OWING
// COPIES (an earlier decline's) hands them back first, keeping its debt to one
// decline ([PendingRun.HandBack]). THE DECISION SETTLED WHETHER THE REPLY GOES BACK:
// one no turn took is let go, one a turn took is the run's unless the ending says it
// went unused; an unmade let-go (another attempt's) leaves the delete to refuse.
func (c *Coordinator) letGoAnswer(ctx context.Context, run PendingRun) (PendingRun, error) {
	answer := *run.Answer
	c.spend(ctx, run.AgentHandle, answer)
	letGo := LetGo{Ending: run.Ending.ID, Answer: answer.EventIDs, HandBack: handBackOf(answer)}
	var handed []string
	for range letGoRounds {
		owing, won, err := c.oweHandBack(ctx, run.TurnID, letGo)
		switch {
		case err == nil && won:
			return owing, nil
		case err == nil, errors.Is(err, ErrAnswerTaken):
			// NOT THIS ATTEMPT'S TO LET GO: the delete refuses for anything owed.
			run.Answer, run.AnswerRefusal = nil, nil
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

// oweHandBack is the let-go's write ([PendingStore.OweHandBack]) as an ending's
// store call ([Coordinator.endingCall]).
func (c *Coordinator) oweHandBack(ctx context.Context, turnID string, letGo LetGo) (PendingRun, bool, error) {
	writeCtx, done := c.endingCall(ctx)
	defer done()
	return c.pending.OweHandBack(writeCtx, turnID, letGo)
}

// endingCall is [Coordinator.rowContext] for one of an ending's changes of its
// run's row (decision, step write, delete) within ctx, the ending's lifetime
// ([detachedEnding], or the coordinator's for its retry): the teardown's grace
// its reason, each call its own crossing.
func (c *Coordinator) endingCall(ctx context.Context) (context.Context, context.CancelFunc) {
	return c.rowContext(ctx, discardGrace)
}

// endingRead is [Coordinator.endingCall] for one of an ending's READS of its
// run's row or of its log: the teardown's grace and the reply's crossing alone
// ([Coordinator.rowReadContext]), never a change's, since a read sends no write.
func (c *Coordinator) endingRead(ctx context.Context) (context.Context, context.CancelFunc) {
	return c.rowReadContext(ctx, discardGrace)
}

// letGoRounds bounds [Coordinator.letGoAnswer]'s rounds: one repeats only to hand
// back copies an earlier decline owes (one decline's worth at a time,
// [PendingRun.HandBack], and a decided ending takes no new one), so the second
// records the let-go or finds another attempt did, and a third is a margin. An
// unlanded clear is stopped sooner by the same-copies guard.
const letGoRounds = 3

// handBackIDs are the ids of what a row owes the seat, in order.
func handBackIDs(owed []HandedBack) []string {
	ids := make([]string, 0, len(owed))
	for _, copied := range owed {
		ids = append(ids, copied.ID)
	}
	return ids
}

// finish ends a run the caller has read and decided is over: the ending is DECIDED
// and finished ([Coordinator.endRecord]), the stop REPORTED where it was this call's.
// Reports the decided ending or nil ([RecordedEnding.Returned] says whether an answer
// goes back) and whether the ending is THIS NODE'S; false is somebody else's run, of
// which the caller says nothing. THE BOX GOES AFTER THE DECISION, never on the
// caller's snapshot: a lapsed lease lets the next holder REVIVE the claim, owed a
// resume into the box left paused, and a kill before the decision destroyed it. The
// attempt that FINISHES reclaims it ([Decision.Reclaim]) once the fence check
// allows, before the record, via the recorded ending so a next holder does too. A
// failed kill does not keep the record (it would park the seat busy); its host's
// sweep reclaims a box no run names. LICENSED FOR EVERY LIVE STATUS AND JOB; the FENCE guards kill and
// delete, and an untaken reply still goes back first ([ErrAnswerOwed]).
func (c *Coordinator) finish(ctx context.Context, run PendingRun, e ending) (*RecordedEnding, bool) {
	if outranked(run, e.fence) {
		// A newer lease owns the run, as the snapshot says; the store would refuse
		// the decision too.
		log.WarnContext(ctx, "sandbox_finish_outranked", "turn_id", run.TurnID,
			"owner_epoch", run.OwnerEpoch, "epoch", e.fence.Epoch)
		return nil, false
	}
	e.whileIn, e.launch, e.reclaim = Active, EveryLaunch, true
	endCtx, done := detachedEnding(ctx)
	defer done()
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

// detached is the context a teardown runs under: A CONTEXT OF ITS OWN, like
// [abandon] and [Manager.discard], since a teardown follows a failure
// (settleFailed, resumeAndSettle) or a drain-cancelled handler, so the inherited
// context is often dead, making the kill and store write no-ops: a box held with
// nothing to collect it, and Kill's wait for the process group is what stops
// removeBox racing the dying wrapper's writes. WithoutCancel, not Background, so warnings keep the turn's values, and
// through [detach] a teardown by the coordinator's stopped work still ends within
// the stop's grace.
func detached(ctx context.Context) (context.Context, context.CancelFunc) {
	free, release := detach(ctx)
	bounded, cancel := context.WithTimeout(free, discardGrace)
	return bounded, func() {
		cancel()
		release()
	}
}

// detach is ctx with its values and NONE OF ITS CANCELLATION, the context a
// teardown, a failure path's record, or any call outliving its asking frame runs
// under ([detached], [Coordinator.detachedRecord], [detachedEnding],
// [Coordinator.settleContext]): the frame's context is often dead. EXCEPT THE STOP'S
// ONE DEADLINE, which ctx carries for the coordinator's own work ([stopBoundKey],
// [Coordinator.Stop]): such a call ends with that grace whenever it began, cut with
// [errStopGrace]. THE ONE DETACHMENT A STEP OF THE PACKAGE MAKES: context.WithoutCancel
// dropped the stop's deadline, and a stopped retry's teardowns held the stop a grace
// apiece. What keeps its own WithoutCancel is no such step: a box backend's rollback
// inside its call (a refreshed login's write-back, a failed mount proof's container),
// the taker lease's give-back after the stop's waits ([takerLease.release]), a
// waiter's row write with no lifetime ([Waiter.rowContext]).
func detach(ctx context.Context) (context.Context, context.CancelFunc) {
	free := context.WithoutCancel(ctx)
	last, ok := ctx.Value(stopBoundKey{}).(context.Context)
	if !ok {
		return free, func() {}
	}
	held, cut := context.WithCancelCause(free)
	if last.Err() != nil {
		cut(context.Cause(last))
		return held, func() { cut(context.Canceled) }
	}
	unhook := context.AfterFunc(last, func() { cut(context.Cause(last)) })
	return held, func() {
		unhook()
		cut(context.Canceled)
	}
}

// stopBoundKey is the key under which a coordinator's lifetime ([Coordinator.life])
// carries the context ending one teardown's grace after its stop
// ([Coordinator.lastCalls]), which every call its work makes once stopped is held
// to, detached or not ([detach]).
type stopBoundKey struct{}

// detachedRecord is the context a teardown's write of a run's row runs under: its
// own, for [detached]'s reason, within the teardown's grace plus the row's
// crossing time ([Coordinator.rowContext]). The grace alone is a box call's, and
// a megabyte row on a slow link outlasts it mid-crossing, leaving the claim
// unreleased, the box's release unwritten and the turn's end unowed
// (crewlet/crewlet#1076). Box calls keep [detached]; an ending's steps take a
// context each ([detachedEnding]).
func (c *Coordinator) detachedRecord(ctx context.Context) (context.Context, context.CancelFunc) {
	free, release := detach(ctx)
	record, cancel := c.rowContext(free, discardGrace)
	return record, func() {
		cancel()
		release()
	}
}

// detachedEnding is the context an ending makes ([Coordinator.endRecord]) runs
// under: ctx's values, none of its cancellation, NO DEADLINE OF ITS OWN. Each store
// call on the row has its own ([Coordinator.rowContext]: grace plus crossing, afresh
// per call), a box call [detached], other steps (a publish, the engine's account)
// what makes them ([Coordinator.retryOwed]). ONE BOUND FOR THE WHOLE ENDING gave the
// row its crossing once, yet an ending writes it several times (decision, let-go,
// cleared copies, owed end, delete) reading and sending it whole: at the slowest link
// a row at its bound takes four minutes a read and three a send, so the second write
// outlasted the bound and the ending was cut between two. An ending by the
// coordinator's stopped work keeps the stop's one deadline ([detach]).
func detachedEnding(ctx context.Context) (context.Context, context.CancelFunc) {
	return detach(ctx)
}

// rowContext is the context ONE store call on a run's row runs under, within ctx:
// its reason (a store's slow moment, an outlasted election, a teardown's grace) plus
// the row's crossing time at its bound ([RowBudget]). HELD TO ITS REASON ALONE ONCE
// THIS COORDINATOR STOPS ([Coordinator.Stop]): the stop waits for cut keeps and
// running retries, and a crossing in each write held a node's stop minutes past its
// orchestrator's grace (sized counting each write at its reason). A call by the
// coordinator's OWN work is also held to the stop's one grace wherever it began
// ([detach]). A write so cut is left undone as a keep is: a begun stamp is settled by
// a peer once this process's taker lease is gone, an ending or claim finished by the
// next holder.
func (c *Coordinator) rowContext(ctx context.Context, reason time.Duration) (context.Context, context.CancelFunc) {
	return c.stopHeld(ctx, reason, RowBudget(c.pending, reason))
}

// rowReadContext is [Coordinator.rowContext] for one READ of a run's row: its
// reason plus the store's reply's crossing alone ([RowReadBudget]), never a
// change's.
func (c *Coordinator) rowReadContext(ctx context.Context, reason time.Duration) (context.Context, context.CancelFunc) {
	return c.stopHeld(ctx, reason, RowReadBudget(c.pending, reason))
}

// stopHeld is the context of one store call on a run's row given budget within
// ctx, and held to reason alone once this coordinator stops
// ([Coordinator.rowContext]).
func (c *Coordinator) stopHeld(ctx context.Context, reason, budget time.Duration) (context.Context,
	context.CancelFunc) {

	row, cancel := context.WithTimeout(ctx, budget)
	held, cut := context.WithCancelCause(row)
	var mu sync.Mutex
	var grace *time.Timer
	ended := false
	stopping := context.AfterFunc(c.life, func() {
		mu.Lock()
		defer mu.Unlock()
		if !ended {
			grace = time.AfterFunc(reason, func() { cut(errStopGrace) })
		}
	})
	return held, func() {
		stopping()
		mu.Lock()
		ended = true
		if grace != nil {
			grace.Stop()
		}
		mu.Unlock()
		cut(context.Canceled)
		cancel()
	}
}

// errStopGrace is a store call on a run's row a stopping coordinator cut at its
// reason ([Coordinator.rowContext]).
var errStopGrace = errors.New("sandbox: the node stopped, and the write was held to its reason alone")

// reclaimBox kills a run's box under killCtx and closes the run's credentials,
// logging under ctx. The error is a failed kill, the one outcome a retry can
// change; a run with no box, or a box whose placement is no longer configured,
// reports nil (nothing later could reach it).
func (c *Coordinator) reclaimBox(ctx, killCtx context.Context, run PendingRun) error {
	// BEFORE THE BOX CHECK: a launch that failed at create still opened a bridge
	// session nothing else closes.
	if c.ended != nil {
		c.ended(run.TurnID)
	}
	if run.SandboxID == "" {
		return nil
	}
	if !c.hosts(run) {
		// ANOTHER NODE'S HOST: killed by id here, the local backend would find no
		// such box and report done, leaving the job (a stopped one too) running.
		// Its host's waiter reclaims it once the row no longer names it, and is
		// told to look now (boxhost.go).
		log.InfoContext(ctx, "sandbox_reclaim_left_to_host", "turn_id", run.TurnID,
			"sandbox_id", run.SandboxID, "node", run.LaunchNode,
			"detail", "the box is on the host of the node that launched it, which reclaims it")
		c.tellHost(ctx, run)
		return nil
	}
	provider, err := c.mgr().Provider(Placement(run.Placement))
	if err != nil {
		// The record names a cell no longer configured: the box is unreachable, so
		// the caller still settles the record (open, it would hold the seat busy
		// forever over a box no configured cell reaches).
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
// inbox is the inbox attachment every hold from here is taken for
// (seathold.go); attach, where not nil, attaches the seat's control topic once
// the recovery gate is closed (recoverygate.go). While the pass runs, every
// claim of the seat's runs here is refused ([ErrSeatRecovering]). Running jobs
// re-mark the seat held and questions are counted, each fenced to this lease
// ([Coordinator.fenceRun]); resumed and launching tails are reaped, which is
// safe only here ([Coordinator.reapTail]), except a dead claim of an untaken
// answer, which is revived ([Coordinator.revivable]). Each run is counted once
// ([Coordinator.takingOver]). See the package doc, "Recovering a seat".
func (c *Coordinator) RecoverSeat(ctx context.Context, handle, owner string, epoch int64,
	inbox queue.Attachment, attach func(context.Context) error) error {
	// THE SEAT'S RECOVERY GATE CLOSED FIRST, before anything can deliver: no claim
	// of its runs on this node until the pass is done, claims this node's attempts
	// already hold left to them (recoverygate.go).
	c.beginRecovery(handle)
	var (
		takeovers []*countWrite
		tally     recovery
		seen      map[signal]uint64
	)
	// EVERY TAKEOVER IS SETTLED ON EVERY WAY OUT, a panic included: one left out
	// holds every later move on its run (countwrite.go). THEN THE GATE OPENS and
	// what it refused is carried (completions re-fired, answers' next attempts
	// armed, [Coordinator.endRecovery]). ONLY THEN ARE OWED REFUSALS RE-ARMED
	// ([Coordinator.rearmOwed]) and held resumes kept ([Coordinator.keepHeldRows]),
	// so a re-fire's claim moves a finished-with run and neither starts a second
	// re-fire of a job the gate's carry re-fired ([Coordinator.carriedLocked]).
	defer func() {
		for _, w := range takeovers {
			c.settleWrite(w)
		}
		c.endRecovery(ctx, handle)
		c.rearmOwed(ctx, tally.owed)
		c.keepHeldRows(ctx, tally.held, seen)
	}()
	// THE CONTROL TOPIC INSIDE THE GATE: a completion published while the seat was
	// not consumed here is held by the subscription, delivered from now, and
	// refused by the gate until the pass is done.
	if attach != nil {
		if err := attach(ctx); err != nil {
			return fmt.Errorf("sandbox: recovering %s: %w", handle, err)
		}
	}
	// THE ATTACHMENT FIRST, before anything counted here can ask for the seat's
	// hold: every hold is taken for it.
	c.attachInbox(handle, inbox)
	// WHAT WAS SIGNALLED BEFORE THE RUNS ARE READ, for the keep of each held resume
	// ([Coordinator.keepHeldRows]).
	seen = c.signalsSeen()
	active, err := c.pending.ListActiveForSeat(ctx, handle)
	if err != nil {
		return fmt.Errorf("sandbox: recovering %s: %w", handle, err)
	}
	takeovers = c.takingOver(ctx, handle, active)
	if len(active) == 0 {
		return nil
	}
	for i, run := range active {
		c.takeoverAnswered(takeovers[i], c.recoverRun(ctx, run, owner, epoch, &tally, true))
	}
	log.InfoContext(ctx, "sandbox_seat_recovered",
		"seat", handle, "epoch", epoch, "running", tally.running,
		"parked", tally.parked, "abandoned", tally.abandoned, "finished", tally.finished,
		"revived", tally.revived, "owed", len(tally.owed), "held", len(tally.held), "active", len(active))
	return nil
}

// recovery counts what a seat's recovery pass found, for its one log line:
// finished is the endings the last holder decided and this one finished, revived
// the dead claims whose answer went back to its run, owed the running jobs whose
// record owes a refused completion its re-fire ([LaunchRecord.RefusalOwed]), and
// held the running jobs whose resume their row holds ([PendingRun.HeldFor]); the
// pass re-arms and keeps those once done.
type recovery struct {
	running, parked, abandoned, finished, revived int
	owed, held                                    []PendingRun
}

// rearmOwed owes again here the completion of each of runs' jobs whose record
// says a holder acknowledged a refused delivery ([LaunchRecord.RefusalOwed]): read
// off the row since that holder's memory did not survive a restart or move, and
// offered to this node's gate at once, never kept on the recorded condition alone
// (this holder may never see its signal). Refused, it is kept for this holder's
// condition ([Coordinator.keepRefused]) with a backstop re-fire
// ([Coordinator.keepLocked]). A job already carried is left
// ([Coordinator.carriedLocked]). See the package doc, "Re-arming owed refusals and
// holds".
func (c *Coordinator) rearmOwed(ctx context.Context, runs []PendingRun) {
	if len(runs) == 0 {
		return
	}
	var rearmed []PendingRun
	c.mu.Lock()
	if !c.closed {
		for _, run := range runs {
			if c.carriedLocked(run.TurnID) {
				continue
			}
			ev, trigger := announcementOf(run)
			kept := refusedCompletion{cond: run.Launch.RefusalOwed, ev: ev, trigger: trigger}
			c.keepLocked(kept)
			c.refireLocked(kept)
			rearmed = append(rearmed, run)
		}
	}
	c.mu.Unlock()
	for _, run := range rearmed {
		log.InfoContext(ctx, "sandbox_completion_refusal_rearmed",
			"turn_id", run.TurnID, "launch_id", run.LaunchID, "agent", run.AgentHandle,
			"condition", string(run.Launch.RefusalOwed),
			"since", run.Launch.RefusalOwedSince.UTC().Format(time.RFC3339),
			"detail", "a holder of the seat acknowledged a refused delivery of the job's completion and owed it "+
				"its re-fire; this holder owes it now, asks its own gate at once, and keeps it for its "+
				"condition's signal")
	}
}

// keepHeldRows keeps here the completion of each of runs' jobs whose resume a
// holder handed back held on the row ([PendingRun.HeldFor]), as that holder kept
// it ([Coordinator.keepHeldOnRow]): re-fired at the hold's instant (at once if
// past) or its condition's signal, never before, since the claim refuses a held
// row. A signal seen since `seen` lifts the hold and re-fires at once. A job
// already carried is left, asked under the keep's lock ([Coordinator.carriedLocked]).
// See the package doc, "Re-arming owed refusals and holds".
func (c *Coordinator) keepHeldRows(ctx context.Context, runs []PendingRun, seen map[signal]uint64) {
	var kept []PendingRun
	for _, run := range runs {
		if c.keepHeldOnRow(run, nil, Refusal{Condition: run.HeldFor, Until: run.HeldUntil}, run.Holds, seen,
			true) {
			kept = append(kept, run)
		}
	}
	for _, run := range kept {
		log.InfoContext(ctx, "sandbox_completion_hold_kept",
			"turn_id", run.TurnID, "launch_id", run.LaunchID, "agent", run.AgentHandle,
			"condition", string(run.HeldFor), "until", run.HeldUntil.UTC().Format(time.RFC3339),
			"detail", "a holder of the seat handed the job's resume back held on its row; this holder keeps "+
				"its completion for the hold's instant, and lifts the hold and re-fires it on its condition's signal")
	}
}

// reaped is what [Coordinator.reapTail] did with a tail the seat's last holder
// left.
type reaped int

const (
	// reapLeft: nothing, a newer lease owns the row or it is gone.
	reapLeft reaped = iota
	// reapMoved: not abandoned after all (given back before this node's fence
	// landed); the row is reported as it stands.
	reapMoved
	// reapRevived: a dead claim's answer was given back to its run, the answered
	// row reported, owed its resume.
	reapRevived
	// reapEnded: the tail was ended, its ending this call's.
	reapEnded
	// reapOwed: a revival that did not land, kept for this node's retry with the
	// seat's mail held ([Coordinator.oweRevival]).
	reapOwed
	// reapLive: refused, the claim being this node's own live attempt's
	// ([errReapLive]): left to it and counted as the claim holding the seat.
	reapLive
)

// errReapLive is the invariant a reap refuses on: a claim this node's own live
// attempt holds is never an abandoned tail. The recovery gate keeps them apart
// (recoverygate.go), so meeting one means the gate failed; the reap still refuses,
// since reaping it ended a turn under the review still running it.
var errReapLive = errors.New("sandbox: invariant broken: a reap reached a claim this node's own " +
	"live attempt holds, which the seat's recovery gate keeps from every pass")

// recoverRun takes over one run of a seat this node just acquired and reports the
// status the seat counts it in ([Coordinator.takeoverAnswered]): none for a run
// ended, reaped, left to a newer lease or gone, or a dead claim whose revival is
// kept owed. again is whether a run that moved under this recovery's fence
// ([Coordinator.fenceClaim], [Coordinator.fenceRun]) is taken over once more as
// what it moved to (once, the fence then holds the row). See the package doc,
// "Recovering a seat".
func (c *Coordinator) recoverRun(ctx context.Context, run PendingRun, owner string, epoch int64,
	tally *recovery, again bool,
) (counted string) {
	if run.Ending != nil {
		// AN ENDING THE LAST HOLDER DECIDED AND DID NOT FINISH is this holder's to
		// finish on the recorded terms (reply, box, one announcement under its own
		// identity), never a second ending of its own.
		if decided, err := c.endRecord(ctx, run, ending{
			fence: Fence{Owner: owner, Epoch: epoch}, whileIn: Active, launch: EveryLaunch,
		}); err != nil {
			log.WarnContext(ctx, "sandbox_ending_kept", "turn_id", run.TurnID, "error", err.Error(),
				"detail", "the ending the seat's last holder decided could not be finished yet; "+
					"this node finishes it, and the seat's mail waits behind it")
		} else if decided != nil {
			tally.finished++
		}
		return ""
	}
	switch run.Status {
	case StatusLaunching, StatusResumed:
		moved, did := c.reapTail(ctx, run, owner, epoch)
		switch did {
		case reapRevived:
			// THE ANSWER IS THE RUN'S AGAIN, owed its resume: taken over as the
			// answered run it now is, like one the last holder never resumed.
			tally.revived++
			return c.recoverRun(ctx, *moved, owner, epoch, tally, false)
		case reapMoved:
			if again {
				return c.recoverRun(ctx, *moved, owner, epoch, tally, false)
			}
		case reapEnded:
			tally.abandoned++
		case reapLeft, reapOwed:
			// Another lease's, gone, or a revival this node retries.
		case reapLive:
			// THIS NODE'S OWN LIVE CLAIM, left to its attempt and counted as the
			// claim holding the seat.
			return run.Status
		}
		return ""
	}
	// FENCED FIRST, TAKEN OVER AS THE FENCE FOUND IT ([Coordinator.fenceRun]): an
	// ending decided after the listing is finished here, a moved run taken over as
	// what it moved to.
	latest, served, gone := c.fenceRun(ctx, run, owner, epoch)
	switch {
	case gone:
		return ""
	case latest.Ending != nil || (again && latest.Status != run.Status):
		return c.recoverRun(ctx, latest, owner, epoch, tally, false)
	}
	run = latest
	switch run.Status {
	case StatusRunning:
		counted = run.Status
		tally.running++
		// A RESUME ITS ROW HOLDS is kept for the hold's instant once the pass is
		// done ([Coordinator.keepHeldRows]), whatever else is owed (an earlier
		// re-fire claims nothing). Else an acknowledged owed refusal is re-armed
		// ([Coordinator.rearmOwed]), a since-lifted hold included, which owed its
		// completion in the release that wrote it ([Release.Hold]).
		switch {
		case run.HeldFor != "":
			tally.held = append(tally.held, run)
		case run.Launch.RefusalOwed != "":
			tally.owed = append(tally.owed, run)
		}
		// WHAT THE BOX CALLS IS SERVED HERE NOW ([CoordinatorOptions.Recovered])
		// unless a newer lease owns the run; an unanswered fence is no such
		// evidence, and what is served acts only while this node holds the seat,
		// just granted.
		if c.recovered != nil && served {
			c.recovered(ctx, run)
		}
	case StatusAnswered:
		// AN ANSWER THE PREVIOUS OWNER RECORDED AND NEVER RESUMED WITH (it
		// stopped or the seat moved between record and resume): on the row, so
		// this node's to drive, the person not asked to resend, the seat's mail
		// waiting behind it. Scheduled, not run: this is the seat's preparation
		// and the mailbox is not open yet.
		counted = run.Status
		c.queueOwed(ctx, run)
		tally.parked++
	case StatusAwaiting, StatusReseed:
		if c.settledUnresumable(ctx, run, Fence{Owner: owner, Epoch: epoch}) {
			tally.abandoned++
			return ""
		}
		// NOTHING IS DONE TO THE RUN (its answer moves it) but the new owner must
		// know the question is open, or the answer arrives at a seat believing
		// nothing waits and runs as an unrelated turn. AND IT IS FENCED TO THIS
		// NODE'S LEASE like every run the seat holds, covering both old-holder cases:
		// one that has NOT noticed writes under its old lease, outranked
		// ([PendingStore.RecordAnswer], [PendingStore.ClaimForResume]); one that HAS
		// writes nothing ([ErrSeatNotHeld]) and the store refuses a record or claim
		// under no lease on an owned row. Left at the old holder's, a node with no seat
		// could answer and claim the run and a parked run stayed at the zero epoch.
		counted = run.Status
		tally.parked++
	}
	// AND A REPLY A DECLINE OWES THE SEAT, whatever the run did since: the
	// decline's write landed and its node stopped before the copies were
	// published (or confirmed). Owed to the seat, so this holder publishes before
	// the mailbox opens, the copies waiting with the seat's mail. A run reaped
	// above publishes what it carries before its record goes
	// ([Coordinator.endRecord]); an answered run's owed resume publishes first
	// ([Coordinator.retryOwed]).
	if len(run.HandBack) > 0 &&
		slices.Contains([]string{StatusRunning, StatusAwaiting, StatusReseed}, run.Status) {
		c.queueOwed(ctx, run)
	}
	return counted
}

// SettleParked ends the seat's parked runs no build will resume
// ([CoordinatorOptions.Unresumable]) and reports whether one still holds its box
// paused for its answer: the one parked state that keeps the seat from other work.
func (c *Coordinator) SettleParked(ctx context.Context, handle string) (pausing bool, err error) {
	fence, held := c.leaseOf(handle)
	if !held {
		return false, notHeld(ctx, handle, "", "settling its parked runs")
	}
	runs, err := c.pending.ListActiveForSeat(ctx, handle)
	if err != nil {
		return false, fmt.Errorf("sandbox: listing seat %s's parked runs: %w", handle, err)
	}
	settled := false
	for _, run := range runs {
		if !run.WaitsOn(run.LaunchID) {
			continue
		}
		if c.settledUnresumable(ctx, run, fence) {
			settled = true
			continue
		}
		pausing = pausing || run.Status == StatusAwaiting
	}
	if settled {
		c.syncSeat(ctx, handle)
	}
	return pausing, nil
}

// settledUnresumable ends a parked run no build will resume
// ([CoordinatorOptions.Unresumable]) while it is still parked, and reports
// whether it is over: ended here, kept for this node's retry, or ended by another.
func (c *Coordinator) settledUnresumable(ctx context.Context, run PendingRun, fence Fence) bool {
	if c.unresumable == nil {
		return false
	}
	u, err := c.unresumable(ctx, run)
	if err != nil {
		log.WarnContext(ctx, "sandbox_parked_run_unjudged", "turn_id", run.TurnID,
			"agent", run.AgentHandle, "error", err.Error(),
			"detail", "whether any build can resume this parked run could not be read, so it "+
				"waits for its answer as before and is judged again at its next resume")
		return false
	}
	if u == nil {
		return false
	}
	decided, err := c.endRecord(ctx, run, ending{
		fence: fence, whileIn: Awaiting, launch: run.LaunchID, reclaim: true, note: u.note(),
	})
	if err != nil {
		log.WarnContext(ctx, "sandbox_unresumable_run_kept", "turn_id", run.TurnID,
			"reason", u.Reason, "error", err.Error(),
			"detail", "the parked run no build can resume is not ended yet; this node "+
				"finishes the ending, and the seat's mail waits behind it")
		return true
	}
	if decided != nil {
		log.InfoContext(ctx, "sandbox_unresumable_run_ended", "turn_id", run.TurnID,
			"agent", run.AgentHandle, "reason", u.Reason, "status", run.Status)
		return true
	}
	// NOT ENDED UNDER THIS LICENSE: the run moved on since the listing (an answer
	// recorded, say), whose resume judges it again.
	return false
}

// fenceRun moves a run this recovery takes over (running or parked) to this node's
// lease and reads it again, for [Coordinator.recoverRun]. It reports the run as the
// fence found it, whether its box's calls may be served here (false for a newer
// lease's), and whether it is gone. READ AGAIN, BECAUSE THE LISTING IS NOT THE ROW THE
// FENCE FENCED: an ending the last holder decides between listing and fence (a
// person's stop) lands under its old lease and, after the fence, its retry is
// outranked ([PendingStore.DecideEnding]), so the fenced row alone tells it; taken
// over as listed, a stopped job ran on and the seat stayed held. A RUN GONE BEFORE
// THE FENCE is not taken over either (as listed it was counted as holding with
// nothing to recount), so a fence that does not land reads too. A FENCE OR READ THAT
// FAILS LEAVES THE RUN AS LISTED, as the reap does ([Coordinator.fenceClaim]): an
// ending decided then waits for the next holder.
func (c *Coordinator) fenceRun(ctx context.Context, run PendingRun, owner string, epoch int64,
) (latest PendingRun, served, gone bool) {
	won, err := c.pending.ClaimOwnership(ctx, run.TurnID, owner, epoch)
	switch {
	case err != nil:
		log.WarnContext(ctx, "sandbox_ownership_claim_failed",
			"turn_id", run.TurnID, "error", err.Error(),
			"detail", "the run could not be fenced to this node's lease; it is taken over as listed")
		return run, true, false
	case !won:
		// NOT FENCED: gone since the listing (not taken over), or a NEWER LEASE owns
		// it (taken over as listed, its box's calls not served here, as is a run
		// this read cannot tell about).
		if _, found, readErr := c.pending.Get(ctx, run.TurnID); readErr == nil && !found {
			return PendingRun{}, false, true
		}
		return run, false, false
	}
	latest, found, err := c.pending.Get(ctx, run.TurnID)
	switch {
	case err != nil:
		log.WarnContext(ctx, "sandbox_recovery_read_failed",
			"turn_id", run.TurnID, "error", err.Error(),
			"detail", "the fenced run could not be read again; it is taken over as listed")
		run.Owner, run.OwnerEpoch = owner, epoch
		return run, true, false
	case !found:
		// FINISHED SINCE THE LISTING, by whoever ended it.
		return PendingRun{}, false, true
	}
	return latest, true, false
}

// reapTail ends a tail the last holder abandoned (a launching row or a claim) and
// announces it; or, for a claim whose node stopped before its turn took the person's
// answer, REVIVES it ([PendingStore.ReviveAnswer], fenced to this lease in one write)
// and reports the row for takeover as the answered run. A recorded answer's delivery
// was spent at the record, so only the row brings it to the run: a claim that never
// took it ([RecordedAnswer.TakenAt]) never used it; reaping lost the run it was for.
// ONLY WHILE IT CAN BE RESUMED ([Coordinator.revivable]); past that the run is reaped
// and the answer let go to the seat. A turn that took it and died used it: reaped as
// spent ([CoordinatorOptions.Spent]). TAKE AND REVIVAL ARE EXCLUSIVE IN THE STORE
// ([ErrAnswerTaken]). An unconfirmed revival is RETRIED ([Coordinator.oweRevival]),
// never a reason to end the run. FENCED FIRST and READ AGAIN
// ([PendingStore.ClaimOwnership]); an unfenceable row is reaped or revived as listed.
func (c *Coordinator) reapTail(ctx context.Context, run PendingRun, owner string, epoch int64,
) (*PendingRun, reaped) {
	fence := Fence{Owner: owner, Epoch: epoch}
	if run.Status == StatusResumed && c.liveHere(run.TurnID) {
		// NOT A TAIL ANYBODY ABANDONED: this node's own attempt holds the claim
		// ([errReapLive]).
		log.ErrorContext(ctx, "sandbox_reap_refused_live_claim",
			"turn_id", run.TurnID, "agent", run.AgentHandle, "launch_id", run.LaunchID,
			"error", errReapLive.Error(),
			"detail", "the claim is left to this node's attempt that holds it; the seat's recovery gate "+
				"should have kept the pass and the claim apart, and did not")
		return nil, reapLive
	}
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
	if moved, did, handled := c.reviveWaitOver(ctx, run, fence); handled {
		return moved, did
	}
	if run.Status == StatusResumed && run.Answer != nil && !run.Answer.Taken() {
		why := c.revivable(run)
		if why == "" {
			answered, took, err := c.reviveAnswer(ctx, run, fence)
			switch {
			case err == nil && answered != nil:
				return answered, reapRevived
			case err == nil:
				// THE CLAIM MOVED between read and revival (given back or ended):
				// taken over as what it is now.
				return c.rereadClaim(ctx, run, fence)
			case took != nil:
				// A TURN TOOK THE ANSWER FIRST: used, so the run is reaped as spent.
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
		// THE TURN TOOK THE REPLY AND DIED WITH IT: spent, so the carrying delivery
		// (unacknowledged if taken inline, or kept for a refusal) is dropped when it
		// comes round to this holder.
		c.spendTaken(ctx, run)
	}
	// Fenced on the lease this node just took, so a record a newer owner claimed
	// is left to it, neither ended nor announced here. ANNOUNCED like the other
	// ways a run is lost, by the ending itself: a turn that died with the previous
	// owner must be visible, not inferred from a record quietly leaving the board.
	decided, ours := c.finish(ctx, run, ending{
		fence: fence,
		note:  &failureNote{reason: types.ExecutorFailureAbandoned, detail: detail},
	})
	if !ours {
		return nil, reapLeft
	}
	// LOGGED ONCE THE ENDING IS DECIDED: what an operator searches for (did the
	// answer go back to the seat?) is the decision's to settle
	// ([RecordedEnding.Returned]); before it a line could only say what the row held
	// (answer_held, answer_taken: a held untaken answer returns, a taken one is held
	// too, and one an earlier decline owes goes back as well). answer_handed_back is
	// the field to search; ending_decided false is an unconfirmed decision whose
	// retry decides it and whose announcement says whether it went back.
	log.WarnContext(ctx, "sandbox_abandoned_tail_reaped",
		"turn_id", run.TurnID, "agent", run.AgentHandle,
		"sandbox_id", run.SandboxID, "status", run.Status,
		"answer_held", run.Answer != nil, "answer_taken", taken,
		"answer_handed_back", decided != nil && decided.Returned,
		"ending_decided", decided != nil)
	return nil, reapEnded
}

// MaxAnswerRevivals is how many claims of one recorded answer may die before their
// turn took it and still be given back to the run ([Coordinator.reapTail]); the
// next is reaped and the answer handed to the seat.
//
// A DYING CLAIM IS A NODE THAT STOPPED between claim and turn (deploy, crash, moved
// lease), a sub-second window: one loss is bad luck, three is a resume killing its
// node before its turn (an allocation the panic guard cannot catch), each revival
// handing it to the next holder with the seat's mail held. Three bounds that to
// three handoffs (minutes at the 45 s lease TTL) while a run meeting two deploys
// resumes. Counted on the row ([RecordedAnswer.LostClaims]) since each node's own
// attempt count ([MaxAnswerAttempts]) resets with the event counted.
const MaxAnswerRevivals = 3

// revivable reports why a dead claim's recorded answer is NOT given back to its
// run, or "" where it is.
//
// TWO BOUNDS, kept on the row since a stopping node resets its counts: claims died
// ([MaxAnswerRevivals]), and time since the first against the run's awaiting window
// ([answerWindow], `pause_ttl_seconds`), past which a resume dying with its node is
// not worth waiting out. NOT THE BOX (the claim never touched it; a gone box
// re-seeds from the pushed branch as any answered run does), NOR THE PARKED STATE
// (a claim is only taken on a run having it; the resume's own refusal,
// [types.ExecutorFailureNoExecuteState], hands the answer back anyway).
func (c *Coordinator) revivable(run PendingRun) string {
	return c.revivableAfter(run, "this answer", run.Answer.LostClaims, run.Answer.FirstLostAt)
}

// revivableAfter is [Coordinator.revivable] on a claim's own counts: lost claims
// of what (the claim, said as the reason says it) died before their turn took
// it, the first at firstLost.
func (c *Coordinator) revivableAfter(run PendingRun, what string, lost int, firstLost time.Time) string {
	if lost >= MaxAnswerRevivals {
		return fmt.Sprintf("%d claims of %s have now died before their turn took it", lost+1, what)
	}
	if window := answerWindow(run); window > 0 && !firstLost.IsZero() && c.now().Sub(firstLost) >= window {
		return fmt.Sprintf("%s has been owed its resume for %s since the first claim of it "+
			"died, past the run's pause_ttl_seconds", what, c.now().Sub(firstLost).Round(time.Second))
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
		// A CLAIM WHOSE NODE STOPPED, so counted ([MaxAnswerRevivals]).
		Lost: true,
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

// rereadClaim is a dead claim's row read again after its revival was refused (it
// moved under the reap) and reported as it is now: given back or ended, to be taken
// over as that; still the claim or unreadable, for the revival to be retried
// ([Coordinator.oweRevival]); or nothing where a newer lease owns it or it is gone.
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
// [Coordinator.reapTail]. It reports the row to reap (still; nil where it is no
// longer a claim, fenced then being the row as it stands) and whether the reap
// should go on (false for a row a newer lease owns or that is gone). A FENCE OR
// READ THAT FAILS LEAVES THE ROW AS LISTED and the reap goes on: the ending's own
// reads decide what the row holds, so an untaken reply is handed back anyway.
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
		// A NEWER LEASE owns the run: its reap, not ours.
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

// RetireSeat ends every run of a seat that left the company, called by the mailbox
// retirement, the one moment such runs are known to be nobody's (absent the whole
// grace; the caller HOLDS ITS LEASE). Until then they are kept (a seat made an
// agent's again returns to its questions and jobs). Runs under ctx, NOT detached (the
// lease lasts the caller's budget); an unended run is an error and the next tick
// retries. Each ending is FINISHED BY THIS CALL ([Coordinator.endRun], mayAsk): the
// host's retry needs the seat's lease, which [Coordinator.ReleaseSeat] drops. A run
// whose log holds a call a collection would wait for (drain.go) is not ended: its
// session is ended to new calls and it is answered as waiting ([SeatRunsWaiting])
// beside any error, keeping the mailbox (waiting in the loop held every later
// ending). Runs are found by seat id ([PendingStore.ListActiveForSeatID]), never
// handle (a broker-found mailbox has none).
func (c *Coordinator) RetireSeat(ctx context.Context, seat uuid.UUID, handle, owner string, epoch int64,
) (SeatRunsWaiting, error) {
	runs, err := c.pending.ListActiveForSeatID(ctx, seat)
	if err != nil {
		return SeatRunsWaiting{}, fmt.Errorf("sandbox: listing the runs of retired seat %s: %w",
			seatLabel(seat, handle), err)
	}
	if handle == "" && len(runs) > 0 {
		// ONE HANDLE PER ID (the id derives from it).
		handle = runs[0].AgentHandle
	}
	label := seatLabel(seat, handle)
	fence := Fence{Owner: owner, Epoch: epoch}
	var (
		errs []error
		wait seatWait
	)
	for _, run := range runs {
		if err := ctx.Err(); err != nil {
			errs = append(errs, fmt.Errorf("sandbox: run %s of retired seat %s was not ended: %w",
				run.TurnID, label, err))
			break
		}
		if outranked(run, fence) {
			// Claimed under a newer lease than the retirement holds (which a held
			// lease rules out): left to that owner, not killed under it.
			errs = append(errs, fmt.Errorf("sandbox: run %s of retired seat %s is owned by a newer "+
				"lease (epoch %d) than the retirement's (%d)", run.TurnID, label, run.OwnerEpoch, epoch))
			continue
		}
		// ITS CALLS STILL RUNNING FIRST, its session ended to new ones: such a run
		// waits for the next tick, one with an unreadable log was not ended.
		waiting, _, logErr := c.callsStillRunning(ctx, run)
		if logErr != nil {
			errs = append(errs, fmt.Errorf("sandbox: run %s of retired seat %s was not ended: %w",
				run.TurnID, label, logErr))
			continue
		}
		if len(waiting) > 0 {
			wait.add(run, waiting)
			continue
		}
		// A box that could not be reclaimed keeps its run's record, and the
		// seat its mailbox: the next tick retries, each within the
		// retirement's budget, until the box is gone. Nothing else waits on
		// it (a settle leaves such a box to its host's sweep, off its own
		// record).
		if err := c.reclaimBox(ctx, ctx, run); err != nil {
			errs = append(errs, fmt.Errorf("sandbox: run %s of retired seat %s was not ended: %w",
				run.TurnID, label, err))
			continue
		}
		// THE SAME ENDING EVERY OTHER PATH MAKES, keeping the stop report out of
		// this function: a retirement does not use [Coordinator.finish] (it keeps
		// the record of an unreclaimed box and runs under the caller's context) but
		// the delete and report are one helper both reach, so a gone seat tells
		// whoever holds something up for its turn without this path remembering.
		// AND THE SAME ANNOUNCEMENT through the ending's note: a held reply goes
		// back to the seat's inbox before the record, like every ending's.
		decided, err := c.endRun(ctx, run, ending{
			fence: fence, whileIn: Active, launch: EveryLaunch,
			note: &failureNote{reason: types.ExecutorFailureSeatRemoved,
				detail: "the seat left the company's agent seats — removed from the chart, or " +
					"made a person's and not an agent's again within the retirement grace — so " +
					"its run was ended; any work it pushed is on its branch"},
		}, true)
		if err != nil {
			errs = append(errs, fmt.Errorf("sandbox: finishing run %s of retired seat %s: %w",
				run.TurnID, label, err))
			continue
		}
		if decided == nil {
			// Settled by somebody else since the listing, who announced a loss.
			continue
		}
		log.InfoContext(ctx, "sandbox_retired_seat_run_ended",
			"turn_id", run.TurnID, "agent", run.AgentHandle, "seat", seat.String(),
			"status", run.Status, "sandbox_id", run.SandboxID)
	}
	if handle != "" {
		// Nothing to release for a seat no handle names: this node counted no run
		// of it.
		c.ReleaseSeat(handle)
	}
	return wait.answer(), errors.Join(errs...)
}

// seatLabel is what a retirement's errors call a seat: its handle, beside the
// id it is retired by, or the id alone for a seat nothing names any more.
func seatLabel(seat uuid.UUID, handle string) string {
	if handle == "" {
		return seat.String()
	}
	return fmt.Sprintf("%q (%s)", handle, seat)
}

// ReleaseSeat stops tracking a seat's runs.
//
// NOTHING IS TORN DOWN: a detached run belongs to its row, not this process, and
// the next owner recovers it through RecoverSeat; reaping the box would destroy work
// the successor is about to resume. AND NOTHING OUT MOVES THE SEAT'S COUNTS AFTER
// IT: the writes out on its runs go with its counts, so a later answer moves
// nothing and this node's recovery counts each run once where it finds it
// (countwrite.go).
func (c *Coordinator) ReleaseSeat(handle string) {
	// The seat's answer-attempt counts go with it: the successor gets a clean set
	// per inherited run, and this node will never be offered a delivery for the
	// seat again ([MaxAnswerAttempts]).
	c.releaseAnswerAttempts(handle)
	// AND ANY ANSWER IT WAS STILL RESUMING: it stays recorded on the run, driven by
	// the successor's recovery.
	c.releaseOwed(handle)
	c.mu.Lock()
	delete(c.runs, handle)
	// AND THE WRITES OUT ON THE SEAT'S RUNS, same step: a later answer moves
	// nothing and a recovery here counts each run afresh (countwrite.go).
	maps.DeleteFunc(c.writing, func(_ string, w *runWrites) bool { return w.handle == handle })
	// AND ITS SLOTS AND HELD RUNS with them: a recovery here counts afresh.
	delete(c.reserved, handle)
	delete(c.freed, handle)
	delete(c.asideFull, handle)
	maps.DeleteFunc(c.heldRuns, func(_ string, r heldRun) bool { return r.handle == handle })
	maps.DeleteFunc(c.waitingRuns, func(_ string, r heldRun) bool { return r.handle == handle })
	// AND A STOP OF ONE OF ITS LAUNCHES NO FRAME TOOK: its turns run on the next
	// holder.
	maps.DeleteFunc(c.launchStops, func(_ string, s launchStop) bool { return s.handle == handle })
	// AND ANY COMPLETION IT WAITED TO RE-FIRE: the run is unclaimed, and the box's
	// waiter announces it to the successor, or, while that waiter cannot, the delivery
	// handed back does (renew.go). Every refusal this node acknowledged is on the
	// job's record ([LaunchRecord.RefusalOwed]), which the successor's recovery
	// re-arms ([Coordinator.rearmOwed]), including a job no collection had read since
	// its host's store went silent, which no waiter announces while it is silent.
	for turnID, kept := range c.refused {
		if kept.ev.AgentHandle == handle {
			c.dropKeptLocked(turnID)
		}
	}
	// AND EVERY RESUME OF ITS RUNS WAITING HERE FOR THE OPERATOR: the successor's
	// attempts say whether one still does ([Coordinator.ResumesBlocked]).
	maps.DeleteFunc(c.blocked, func(_ string, b blockedResume) bool { return b.agent == handle })
	// AND THE CLOCK ON ANY HOST WHOSE STORE ALONE WAS SILENT: the successor's
	// collections start their own. NOT A RE-FIRE'S ANNOUNCEMENT STILL OWED
	// ([refirePublish]): it needs no lease and reaches the successor on the control
	// topic, nothing else telling it of the job (the delivery was acknowledged and
	// the host's waiter announces nothing while its store is silent); made until one
	// lands.
	maps.DeleteFunc(c.unsettled, func(_ string, u hostUnsettled) bool { return u.handle == handle })
	c.mu.Unlock()
	// AND THE SEAT'S INBOX HOLD, already dropped with the attachment by the
	// release's detach: forgotten, never lifted (its mail is the successor's to
	// order). See [Coordinator.forgetHold].
	c.forgetHold(handle)
	// AND THE ROUTER OF ITS HELD INBOX stops: the seat's mail is the successor's.
	if c.busy != nil {
		c.busy(handle)
	}
	log.Debug("sandbox_seat_released", "seat", handle)
}

func fenceOf(run PendingRun) Fence {
	return Fence{Owner: run.Owner, Epoch: run.OwnerEpoch}
}

// runReport is a collected run's own account of itself, filed by the resumed turn
// as its executor's report: the reviewer judges the run on it beside its bridge
// calls, and the turn's record carries it.
//
// THE RUN'S WORDS AND WHY IT ENDED, nothing addressed to anybody: the run IS the
// executor (it worked, told whoever waited through its tools, ended), so no later
// executor is there to instruct, and an instruction here would be read by the
// reviewer as the executor's and filed as what the turn did. Secret-redacted, as it
// is published on the phase record. It carries the findings (reported text,
// delivered refs, the push record's pushes to Gitlet, or the error) and TAKES THE
// RUN, since whether it says "nothing" was pushed turns on what the run was given
// ([pushedText]): its repositories are the row's, never the result's.
func runReport(run PendingRun, result Result) string {
	status := "did NOT fully succeed"
	if result.Success {
		status = "succeeded"
	}
	lines := []string{"The executor's run " + status + "."}
	switch {
	case len(result.DeliveredRefs) > 0:
		delivered := "Delivered: " + strings.Join(result.DeliveredRefs, ", ")
		if result.DeliveredRefsElided > 0 {
			// SAID, so a short list is not read as all of it.
			delivered += fmt.Sprintf(" (and %d more its report names, not listed here)",
				result.DeliveredRefsElided)
		}
		lines = append(lines, delivered)
	case result.DeliveredRefsElided > 0:
		// SAID WHERE NONE IS LISTED TOO: a ref past [MaxDeliveredRefBytes], or not
		// UTF-8, is never listed ([boundRefs]) yet the record and dashboard count it;
		// dropped unsaid, the report looked like a run that delivered nothing.
		lines = append(lines, fmt.Sprintf("Delivered: %d ref(s) its report names, not listed here",
			result.DeliveredRefsElided))
	}
	if pushed := pushedText(run, result); pushed != "" {
		lines = append(lines, pushed)
	}
	if opened := openedText(result); opened != "" {
		lines = append(lines, opened)
	}
	switch {
	case result.Text != "":
		lines = append(lines, "\n"+result.Text)
		// WHY, BESIDE WHAT IT SAID: a run that wrote a report and still failed
		// (crashed after, unreadable result, harness error after an answer) would
		// otherwise be filed "did NOT fully succeed" with no reason.
		if !result.Success && result.Error != "" && result.Error != result.Text {
			lines = append(lines, "\nWhy it did not succeed: "+result.Error)
		}
	case result.Error != "":
		lines = append(lines, "\nError: "+result.Error)
	}
	return redact.Secrets(strings.Join(lines, "\n"))
}

// AskedRun is what a person's answer resumes a parked run with
// ([ResumeRequest.Asked]). Redacted, like everything a resume hands the turn,
// which hands it as is to the run it carries on, whose box and session keep it.
type AskedRun struct {
	// Question is what the run asked and Answer the reply; By is who gave it
	// where the route knows (an answer by turn names the person; a chat reply's
	// sender is already in the conversation), else "".
	Question, Answer, By string

	// Branch is the work-in-progress branch the run pushed before asking, "" if its
	// ask named none: the durable half of the work, which a run on a fresh machine
	// starts from.
	Branch string

	// SessionID is the asking job's harness session, continued by the relaunch where
	// it runs in the asking job's box ([Continuation]): the one the box ran
	// ([PendingRun.SessionRan]), "" where the row cannot say, continuing none.
	SessionID string

	// BoxKept is whether that box is still there. False once reclaimed while the
	// question waited (reaped past the pause TTL, or torn down at the block under a
	// zero TTL): the work carries on on a fresh machine from the branch, and the
	// run must be told or it looks for a working tree that is gone.
	BoxKept bool

	// Label is who wrote Answer as the fence around the reply's message labelled it
	// ([AnswerWords.Label]), for a run reading a chat reply inside a fence of its
	// turn's token; nil for an answer by turn, the person's own instruction, read
	// defused.
	Label *provenance.Label

	// Held is the call to a tool at Needs approval the question was about, with the
	// answer's choice and the writes making it at most once; nil otherwise
	// (ADR-0077).
	Held *HeldResume

	// Withdrawn is whether the run's own seat gave the answer: it withdrew the
	// question, or the held call (as Deny), Answer its reason (ADR-0111). Derived
	// from who answered, never set beside Refused: a refused reply is no answer.
	Withdrawn bool

	// Waited is how the run a wait was for left its seat's slots, where the
	// engine carried a run parked by `wait_for_run` on (ADR-0176, waitover.go);
	// nil otherwise.
	Waited *types.RunWaited

	// Called is every bridged call the job parked on ([PendingRun.ParkedCalls]),
	// whatever answered its ask: each told with what it returned. CallsReturned
	// is whether the engine carried the job on because they returned, the job
	// having asked nothing else (waitover.go), never a person's answer.
	Called        []types.CallReturned
	CallsReturned bool

	// Refused is set where the question's outcome is no answer but the REFUSAL of a
	// reply too large to be one ([PendingRun.AnswerRefusal]): what the run is told
	// (how many messages, the figures) in place of an answer, Answer and By empty.
	// NEVER AN EMPTY ANSWER: every reader handing the run its answer tells it the
	// refusal instead where this is set (ADR-0114).
	Refused *AnswerRefusal
}

// WithdrawnBy reports an answer given by the run's own seat: an agent seat
// answers no question but its own run's, and only to withdraw it.
func WithdrawnBy(run PendingRun, by, byKind string) bool {
	return by != "" && by == run.AgentHandle && byKind == string(iam.ActorAgent)
}

// askedRun is what run's recorded answer resumes it with: the words its seal
// opened to ([AnswerSeal.Open]), nothing else on the row about them; for a
// refusal, the refusal they carry in place of an answer.
func askedRun(run PendingRun, answer AnswerWords) *AskedRun {
	if answer.Refusal != nil {
		refusal := *answer.Refusal
		return &AskedRun{
			Question: redact.Secrets(run.Question), Branch: run.Branch, SessionID: run.SessionRan(),
			BoxKept: run.SandboxID != "", Refused: &refusal, Called: run.CallsReturned(),
		}
	}
	text, err := AnswerText(run.Decision, answer.Choice, answer.Text)
	if err != nil {
		// UNREACHABLE: a choice is checked against its question before recording,
		// and a new question comes only under a new job. Said, and the run is handed
		// the words given, not a choice nobody offered.
		log.Error("sandbox_answer_choice_unread", "turn_id", run.TurnID,
			"launch_id", run.LaunchID, "choice", answer.Choice, "error", err.Error())
		text = answer.Text
	}
	return &AskedRun{
		Question: redact.Secrets(run.Question), Answer: redact.Secrets(text),
		By: answer.attribution(), Branch: run.Branch, SessionID: run.SessionRan(),
		BoxKept:   run.SandboxID != "",
		Label:     answer.Label,
		Withdrawn: WithdrawnBy(run, answer.By, answer.ByKind),
		Called:    run.CallsReturned(),
	}
}
