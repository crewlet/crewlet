package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/decision"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/executorkeep"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/toolname"
)

// The durable state of an executor's detached run.
//
// A run OUTLIVES its kick-off turn, its process, and sometimes its node. What
// survives is this row: the completion turn rebuilds everything it needs from
// it, and a startup pass re-attaches to still-live boxes after a restart —
// without which a restart orphans a box that holds its host's resources and
// hands its result to nobody.

// The run's state machine. Every state is one a RECOVERY pass can act on; an
// unrecoverable state leaks a box.
//
// A RUN THAT IS OVER HAS NO RECORD. Settling a run, done or failed, deletes its
// row ([PendingStore.Finish]) once its box is reclaimed. The bucket is ageless
// (a parked run can wait days, and a reaped row would hide a held box), and
// every poll and recovery reads all of it, so a kept terminal status would be
// read and skipped forever. What a run ended as is on the phase events of the
// resumed turn or its executor_failed or executor_stopped announcement.
const (
	// StatusLaunching — the job has started but the turn has not yet written
	// the conversation a resume re-enters. The seat is BUSY and a box exists;
	// there is nothing to resume INTO yet.
	//
	// It exists because the launch is two writes: [Launch] starts a detached
	// process and returns, and the parked turn's state reaches the row only
	// when the turn's frame unwinds (milliseconds idle, hundreds loaded). A job
	// finishing in ~100 ms could be claimed against a row with no ExecuteState,
	// and the coordinator, finding nothing to resume, FAILED the run and lost
	// the agent's turn with no retry. [PendingStore.MarkSuspended] is one write
	// that makes the run pollable and claimable at the same instant.
	StatusLaunching = "launching"

	// StatusRunning — the job is executing, the parked turn's state is on the
	// row, and the seat is BUSY.
	StatusRunning = "running"

	// StatusAwaiting — the agent asked a person and stopped. The seat is FREE:
	// a clarification can wait days.
	StatusAwaiting = "awaiting_clarification"

	// StatusAnswered — a person's reply is RECORDED on the row and the resume it
	// drives is owed. Not [Awaiting], since nobody is waited on; not holding the
	// seat, since nothing runs. The seat's later mail waits behind the resume on
	// an inbox hold the coordinator keeps while the answer is owed
	// ([Coordinator.TryResumeFromAnswer]).
	//
	// ITS OWN STATUS rather than a field on an awaiting row: the answer match
	// lists [Awaiting] rows only, so a later reply can never be taken as the
	// answer to a question that already has one, with no reader having to
	// remember a side field.
	StatusAnswered = "answered"

	// StatusResumed — the tail has been claimed. THE AT-MOST-ONCE GATE.
	//
	// A claim is not a resume: the row reads resumed from claim to settle, and a
	// turn may or may not have run between. For a claimed ANSWER
	// [RecordedAnswer.TakenAt] says which, deciding whether the reply is still
	// owed when the claim dies with its process.
	StatusResumed = "resumed"

	// StatusReseed — a paused box was reaped past its pause TTL. The run is NOT
	// over: the answer can still arrive, and the work re-seeds from the pushed
	// branch.
	StatusReseed = "reseed"
)

// Claimable are the statuses whose tail has not run yet.
//
// Reseed is a state rather than a flag because reaping the box does not end the
// run. LAUNCHING IS ABSENT: a claim promises a resume can follow, and a
// launching row has no conversation. ANSWERED IS HERE: recording an answer is
// not resuming with it, and a failed resume hands the claim back there for the
// coordinator's own retry, not a redelivery of the person's message.
var Claimable = []string{StatusRunning, StatusAwaiting, StatusReseed, StatusAnswered}

// Tail is what a claim expects to find on a run: the job the signal is about,
// and the statuses that signal may take the tail out of.
//
// A CLAIM NAMES WHAT IT CLAIMS. A completion is only true of a RUNNING row
// still holding that job; an answer only of an [Awaiting] row holding the job
// that asked. A claim taking any claimable status would let a duplicate
// completion re-collect a run parked on its question, or one outliving its job
// claim the next job mid-run: the turn would resume on a half-written result
// and the settle would tear the box down under the job.
type Tail struct {
	// Launch is the [PendingRun.LaunchID] the signal was raised for,
	// matched exactly, the empty value included.
	Launch string

	// From is the statuses the signal may claim out of. A status outside
	// [Claimable] is never claimed, whatever this says.
	From []string
}

// CompletionTail is what a completion of one job claims: that job, while it
// is running. A completion finding any other status is a duplicate.
func CompletionTail(launch string) Tail {
	return Tail{Launch: launch, From: []string{StatusRunning}}
}

// RecordedAnswerTail is what the resume of a RECORDED answer claims: the job
// that asked, once its reply is on the row ([StatusAnswered]). Both routes, a
// chat reply and an answer by turn, record before anything else.
func RecordedAnswerTail(launch string) Tail {
	return Tail{Launch: launch, From: []string{StatusAnswered}}
}

// Release is how a claimed tail is handed back for its signal's retry: the
// claim it hands back, where to, and what the claim already did.
//
// A RELEASE NAMES WHAT IT RELEASES, like a claim. The resumed turn may move the
// row on (a relaunch opens a new launch; a seat's next owner reaps an abandoned
// claim), so putting back whatever the row held by then would revert the new
// launch to running with no box or conversation, or revive a run already
// announced lost.
type Release struct {
	// Launch is the [PendingRun.LaunchID] the claim took, matched exactly.
	Launch string

	// To is the status the claim took the run out of, one of [Claimable].
	To string

	// Charged is whether the run's spend is on the token counter, recorded by
	// the release itself ([PendingRun.Charged]).
	Charged bool

	// CompanyCharged is the same for the company's share where the seat's is
	// not ([PendingRun.CompanyCharged]).
	CompanyCharged bool

	// Published is the same for the launch's `executor_collected` record
	// ([LaunchRecord.Published]).
	Published bool

	// CollectFailedAt, when set, is when a collection could not read the box
	// back: the release counts it onto [LaunchRecord.CollectFailures], dating
	// the run of failures from the first. Zero otherwise.
	CollectFailedAt time.Time

	// ResumeFailedAt, when set, is when the job's resume failed: counted onto
	// [LaunchRecord.ResumeFailures] the same way. Zero for any other release,
	// a resume held on a condition included, which is a wait.
	ResumeFailedAt time.Time

	// Hold, where set, holds the resume back until the refusal lifts
	// ([PendingRun.HeldFor]). Nil hands back a claim to be retried as the
	// completion comes round and clears any earlier hold. A hold also owes the
	// completion on the record ([LaunchRecord.RefusalOwed]) where none is owed
	// yet: the hold spent the delivery, and a lift could leave recovery nothing
	// to re-arm ([Coordinator.rearmOwed]). See the package doc, "Holding a
	// refused resume".
	Hold *Refusal

	// RelaunchRefusedIn, where it names a window, is the budget window the
	// resume's relaunch was refused below its floor in ([Refusal.Window]),
	// counted onto [LaunchRecord.RelaunchRefusedIn] for the next relaunch. A
	// held completion names one beside its Hold; a recorded answer turned back
	// by its seat's budget names one with no Hold. The zero window counts
	// nothing and keeps an earlier one.
	RelaunchRefusedIn BudgetWindow

	// Collected is whether the claim READ THE JOB'S BOX BACK (a collection that
	// succeeded, whose park or resume then failed). It ENDS the run of failed
	// collections, clearing [LaunchRecord.CollectFailures] and
	// [LaunchRecord.CollectFailingSince].
	//
	// The bound is on CONSECUTIVE failures: otherwise a box that failed once,
	// was collected, and was handed back kept the count and the first instant,
	// and its next single failure was measured from before the success and gave
	// up a box that had just answered. A release cannot say both Collected and
	// a failed collection; one that does is refused.
	Collected bool

	// CollectedAt is when the box was first read back ([LaunchRecord.CollectedAt]),
	// written with Collected onto a record holding none.
	CollectedAt time.Time

	// BoxAnswered is whether the collection read the box back and a later step
	// handed the claim back uncounted: the coordination store did not answer
	// ([ErrStoreSilent]), or this node stopped. It ends the run of failed
	// collections as Collected does and records nothing more, so the job stays
	// stoppable. A release counting a failed collection cannot say it too; one
	// that does is refused. See the package doc, "Collection failures".
	BoxAnswered bool

	// HarnessEnd, where set, is how the harness ended as the claim's read of its
	// box found it, where a later step handed the claim back
	// ([PendingRun.BoxHarnessEnd]). Recorded by the rule
	// [PendingStore.MarkHarnessEnd] follows ([HarnessEnd.after]) IN THE RELEASE'S
	// OWN WRITE, so a row that reads running again says the harness ended.
	HarnessEnd HarnessEnd

	// HostStoreSilentSince, where set, is when this node's clock on the store of
	// the box's host started, which the collection left running
	// ([Coordinator.hostClocked]): that host last answered that its store did
	// not answer its read of the run's row. Recorded on
	// [LaunchRecord.HostStoreSilentSince], so a restarted node or the seat's
	// next holder reads it before acknowledging the completion's delivery.
	HostStoreSilentSince time.Time

	// HostStoreAnswered is whether the claim learned that the box host's store
	// answers: its collection read the box or found it failing to be read
	// (which that host could say only with its store answering), or the
	// delivery is a waiter's announcement made from a read of the row through
	// that store, asked after the recorded silence ([announcedAfter]). The
	// release clears that record; a claim that learned nothing of the store
	// leaves it. A release cannot say both; one that does is refused.
	HostStoreAnswered bool

	// Fence is the lease the claim was taken under.
	Fence Fence
}

// OwedRefusal is a refusal of a job's completion the seat's holder
// acknowledged, as [PendingStore.OweRefusal] records it on the job's record
// ([LaunchRecord.RefusalOwed]).
type OwedRefusal struct {
	// Condition is what the refusal waits on ([Refusal.Condition]).
	Condition Condition

	// Since is when the holder acknowledged it.
	Since time.Time
}

// MaxOwedConditionBytes is the longest condition an owed refusal records
// ([PendingStore.OweRefusal]) and a release's [Release.Hold] names. Gate
// conditions are short words ("ownership", "turn_engine"); the written bound
// gives the field its share of the row (rowbytes.go). Every [Condition]
// constant is held to it (TestEveryConditionFitsWhatAnOwedRefusalRecords): one
// past it would be refused at its first record, its deliveries never
// acknowledged, and a hold on it refused with its release, ending the run as a
// claim that could not be handed back.
const MaxOwedConditionBytes = 32

// completionUnsettled reports whether the row's job has a completion nothing
// has settled, which an acknowledged refusal leaves owed
// ([PendingStore.OweRefusal]): running, or claimed for its completion
// ([CompletionTail]). The caller matches the job and writes live, which refuses
// a run whose ending is decided ([ErrRunEnding]). A claim settles nothing by
// itself (a re-fire's claim can land between refusal and record, and a handed
// back claim restores the completion), but a recorded answer's claim is not
// one: its job parked on its question, which settled the completion.
func (r PendingRun) completionUnsettled() bool {
	return r.Status == StatusRunning || (r.Status == StatusResumed && r.Answer == nil)
}

// License is what an ending may be DECIDED under ([PendingStore.DecideEnding]):
// the lease, the statuses and the job it was decided on.
//
// A LICENSE, NOT A FILTER: what this ending is entitled to end. A run that has
// moved off it (newer lease, another status or job) is somebody else's. An
// ending decided on a row its caller READ takes the widest statuses and jobs
// ([Coordinator.finish]); the narrow one belongs to an ending decided without
// reading the row, a claim's own ([Coordinator.endClaim]). All keep the fence,
// which the store checks against the row as it stands; since the box is
// reclaimed only after the decision ([Decision.Reclaim]), that guards the kill
// as well as the delete.
type License struct {
	// Fence is the lease the ending is made under; a newer one owns the run.
	Fence Fence

	// WhileIn is the statuses the run may be ended from; [Active] (every status
	// a record can hold) is the widest. An empty set licenses nothing, the safe
	// zero value.
	WhileIn []string

	// Launch is the job the ending was decided on; the run ends only while it
	// still holds that job, or any job for [EveryLaunch]. A claim's ending names
	// its job because status alone cannot tell it from the next claim: the
	// resumed turn can relaunch on the row and that job's completion claims it
	// in the same status. The zero value names no job (every launch mints one),
	// so it ends nothing.
	Launch string

	// Unused says the recorded answer went unused even where a turn took it: the
	// claim's own ending, after its turn gave the claim back as a retry and the
	// release could not land. Recorded on [RecordedEnding.Unused]: the run is
	// then not deleted while it holds a recorded answer ([ErrAnswerOwed]), and
	// every other ending deletes one a turn took, since it has been used.
	Unused bool
}

// EveryLaunch licenses an ending whatever job its run holds — see
// [License.Launch]. Never a launch id: the store mints those as UUIDs.
const EveryLaunch = "*"

// LetGo is how a decided ending lets go of the recorded answer its run still
// holds ([PendingStore.OweHandBack]): which ending, which answer, and the copies
// it owes the seat for it.
//
// NO LEASE AND NO STATUSES: it is a step of an ending already DECIDED
// ([PendingRun.Ending]), whose license was the decision's, and the row takes no
// other write from then on.
type LetGo struct {
	// Ending is the [RecordedEnding.ID] the let-go is a step of.
	Ending string

	// Answer is the deliveries the answer was made of
	// ([RecordedAnswer.EventIDs]); the run must hold exactly that answer.
	Answer []string

	// HandBack is the copies the seat is owed for them ([handBackOf]).
	HandBack []HandedBack
}

// Decision is an ending, as [PendingStore.DecideEnding] records it: the license
// it is decided under, what it announces, and whether the box is reclaimed by
// whichever attempt finishes it.
type Decision struct {
	License License

	// Reason and Detail are what the ending announces
	// ([types.ExecutorFailed]); an empty Reason announces nothing — unless
	// the ending is a stop ([Decision.Stop]), which announces itself.
	Reason, Detail string

	// ErrorKind is the classifier's word for the failure behind the ending,
	// where one has it: [types.ExecutorFailed.ErrorKind].
	ErrorKind string

	// Reclaim is whether the attempt finishing the ending reclaims the box the
	// row names before deleting the row: every ending that has not reclaimed a
	// box itself, since a kill before the decision runs on the caller's
	// snapshot, which a newer lease may have overtaken ([Coordinator.finish]).
	Reclaim bool

	// Stop, where set, is the person whose pause ENDED the run
	// ([Coordinator.StopSeat]), recorded in the write that decides the ending
	// ([PendingRun.Stop]). It makes the ending a stop: it announces
	// [types.ExecutorStopped] rather than a loss and closes the parked turn as
	// stopped. Nil otherwise.
	Stop *RunStop
}

// RecordedEnding is an ending DECIDED on a run and not yet finished: what it
// announces and under which identity, what it hands back, and what it reclaims.
//
// THE DECISION IS THE COMMITMENT: an ending spans several systems (reply handed
// back, box reclaimed, announcement, delete) and a process can stop between any
// two. Recorded first, every later step is the SAME ending's, finished by whoever
// reads the row next, announcing BEFORE the delete under the recorded identity
// ([RecordedEnding.ID], [RecordedEnding.At]) so a crash repeat is the same event.
// AND IT FREEZES THE RUN: the store refuses every write but the ending's own
// steps ([ErrRunEnding]), so a run is never announced lost and then resumed.
type RecordedEnding struct {
	// ID names the ending and, with the run's turn, derives the id its
	// announcement is published under ([endingEventID]).
	ID string `json:"id"`

	// At is when the ending was decided, on the deciding node's clock, and the
	// instant the announcement carries. Only that node reads it as an age on one
	// clock: another's may differ by up to twice objstore.AssumedClockSkew,
	// which a stop's give-up of a silent host allows ([Coordinator.hostGivenUp]).
	At time.Time `json:"at"`

	// Reason and Detail are what it announces; an empty Reason announces
	// nothing (an ordinary ending, whose turn came back).
	Reason string `json:"reason,omitempty"`
	Detail string `json:"detail,omitempty"`

	// ErrorKind is the decision's ([Decision.ErrorKind]), kept for whichever
	// attempt announces it.
	ErrorKind string `json:"error_kind,omitempty"`

	// Returned is whether the ending hands a person's answer back to the seat
	// (one no turn took, one [Unused], or copies already owed), decided with the
	// ending so every attempt's announcement agrees.
	Returned bool `json:"returned,omitempty"`

	// Unused is the license's ([License.Unused]).
	Unused bool `json:"unused,omitempty"`

	// Reclaim is the decision's ([Decision.Reclaim]).
	Reclaim bool `json:"reclaim,omitempty"`
}

// ColleagueAsk is the A2A question a colleague put to the seat whose turn
// launched a run: what the engine replies to, on the channel the ask opened,
// once that turn ends ([PendingRun.ColleagueAsk]).
//
// ON THE ROW because the frame ending a parked turn never saw the ask: it resumes
// off the control topic on any node, days later, or closes by stop or loss
// ([CoordinatorOptions.TurnLost]), with no trigger left to read it from.
// ONLY WHAT THE REPLY NEEDS: channel, echoed question, chain and trace; not the
// depth ([PendingRun.DelegationDepth]). No Extra, so a later field is a top-level
// key (wirekeys.go).
type ColleagueAsk struct {
	ChannelID string `json:"channel_id"`
	// question is the colleague's words as asked, `question` on the wire: read
	// through [ColleagueAsk.Question], redacted, and by the answer alone
	// [ColleagueAsk.RawQuestion]. Its two encodings are in colleagueask.go.
	question     string
	Chain        []string `json:"chain,omitempty"`
	TraceID      string   `json:"trace_id,omitempty"`
	SpanID       string   `json:"span_id,omitempty"`
	ParentSpanID string   `json:"parent_span_id,omitempty"`
	// WorkingFor is who the answering turn works for
	// ([types.A2AMessage.WorkingFor]): the ask's carrier, or the asking seat
	// where the ask was its own work.
	WorkingFor *types.WorkingFor `json:"working_for,omitempty"`
}

// RunStop is who ENDED a run by pausing its seat with a stop
// ([Coordinator.StopSeat]), and what the ending read of the run before it
// reclaimed the box, recorded on the row ([PendingRun.Stop]).
//
// WHO is the pause's three halves: the author iam.ActorFor names, its kind and
// the credential used, plus the reason, so any node finishing the ending says
// the same in the announcement and the stopped turn's record.
type RunStop struct {
	By         string `json:"by"`
	ByKind     string `json:"by_kind,omitempty"`
	OperatorID string `json:"operator_id,omitempty"`
	Reason     string `json:"reason,omitempty"`

	// Collected is whether the ending has read the box back
	// ([Coordinator.collectStopped]); the figures beside it are what the run had
	// spent: tokens, their cached share, and the seat's auxiliary model cost of
	// condensing its report. Written by [PendingStore.CollectStop] before the box
	// is reclaimed and read by whichever attempt closes the turn, which charges
	// the work item ([CoordinatorOptions.CloseStopped]).
	//
	// FALSE UNTIL THE STEP LANDS. A box that could not be read leaves it true
	// with nothing beside it: a gone box has nothing to read, and an ending
	// waiting for it would never finish.
	Collected        bool      `json:"collected,omitempty"`
	InputTokens      int       `json:"input_tokens,omitempty"`
	OutputTokens     int       `json:"output_tokens,omitempty"`
	CacheReadTokens  int       `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens int       `json:"cache_write_tokens,omitempty"`
	Condensed        AuxTokens `json:"condensed,omitzero"`
}

// Stoppable are the statuses a stop ENDS: a run whose job is out in a box with
// the engine driving it, launching or running.
//
// NOT THE OTHERS. A RESUMED run's turn is running or about to: its own fence
// stops it at the next round ([Coordinator.StopSeat]). A running run whose job a
// collection read ([LaunchRecord.Collected]) is the same turn one step later,
// and the store refuses a stop ([PendingStore.DecideEnding]). A PARKED run does
// no work, and its answer waits behind the pause like any delivery.
var Stoppable = []string{StatusLaunching, StatusRunning}

// StopCollection is what a stop's ending read of its run's box before
// reclaiming it ([PendingStore.CollectStop]).
type StopCollection struct {
	// Ending is the [RecordedEnding.ID] this is a step of.
	Ending string

	// InputTokens, OutputTokens, CacheReadTokens, CacheWriteTokens and
	// Condensed are what the read found ([RunStop.Collected]); zero for an
	// unreadable box.
	InputTokens      int
	OutputTokens     int
	CacheReadTokens  int
	CacheWriteTokens int
	Condensed        AuxTokens

	// Charged, CompanyCharged and Published are [PendingRun.Charged],
	// [PendingRun.CompanyCharged] and [LaunchRecord.Published], recorded in the
	// same write so a retry after a crash neither charges nor publishes twice.
	Charged        bool
	CompanyCharged bool
	Published      bool
}

// announces reports whether a run's decided ending makes an announcement: a
// loss names its reason, and a stop names who stopped it.
func (r PendingRun) announces() bool {
	return r.Ending != nil && (r.Ending.Reason != "" || r.Stop != nil)
}

// lost reports whether a run's decided ending is a LOSS: it names the reason,
// so the turn suspended into the run has no frame left to end it
// ([CoordinatorOptions.TurnLost]). An ordinary ending names none (its turn came
// back) and a stop names who stopped it.
func (r PendingRun) lost() bool {
	return r.Ending != nil && r.Ending.Reason != "" && r.Stop == nil
}

// endDecided reports whether the turn's end is decided: its run's ending decided
// ([PendingRun.Ending]), that end owed on the row ([PendingRun.OwedEnd]), or
// already on the executor log ([PendingRun.EndRecorded]). Such a turn is ending
// and no relaunch follows, whatever the row holds: EVERY LAUNCH REFUSES A ROW
// WHOSE ENDING IS DECIDED ([ErrRunEnding]), from the decision on, however long
// a stop's read, announcement or a store's bad moment keeps it for retry. The
// decision itself ends the turn, not the owing after it.
func (r PendingRun) endDecided() bool { return r.Ending != nil || len(r.OwedEnd) > 0 || r.EndRecorded }

// answerOwed reports whether a run whose ending is decided still holds a
// person's answer that has to go back to the seat before its record is deleted:
// one no turn took, or one the ending says went unused.
func (r PendingRun) answerOwed() bool {
	if r.Answer == nil {
		return false
	}
	return !r.Answer.Taken() || (r.Ending != nil && r.Ending.Unused)
}

// Revival is how a claim of a recorded answer that no resume holds is given
// back to the run it answers ([PendingStore.ReviveAnswer]): which job, which
// answer, the lease the reviving node holds the seat under, and whether the
// claim was LOST.
type Revival struct {
	// Launch is the job the claim took, matched exactly.
	Launch string

	// Answer is the deliveries the answer was made of
	// ([RecordedAnswer.EventIDs]); the run must hold exactly that answer.
	Answer []string

	// Fence is the reviving node's lease, stamped on the row by the revival.
	Fence Fence

	// Lost says the claim DIED: its node stopped, or the seat moved, between the
	// claim and the turn that would take the answer, and the seat's next holder
	// revives it ([Coordinator.reapTail]). The revival COUNTS it on the answer
	// ([RecordedAnswer.LostClaims], [RecordedAnswer.FirstLostAt]), which bounds
	// a resume that takes its node down.
	//
	// FALSE IS A CLAIM NOBODY LOST: a node's own claim whose write reported
	// failure and landed, given back by its series under the lease it was taken
	// under ([Coordinator.suspectClaim]). Nothing is counted: counted, a store
	// answering a few of a healthy node's claims with errors ended the run as an
	// abandoned tail. That series is bounded by [MaxAnswerAttempts] and
	// [answerWindow], ending in a decline rather than an ending.
	Lost bool
}

// Holding are the statuses in which a run holds its seat, so the seat takes no
// new turn. The seat is held while the engine drives the run and freed while a
// person does: launching is a fraction of a second, a parked run can wait days.
var Holding = []string{StatusLaunching, StatusRunning, StatusResumed}

// Awaiting are the statuses still waiting on a person's answer, matched back
// by conversation.
var Awaiting = []string{StatusAwaiting, StatusReseed}

// Active are the statuses that still own engine-side state (a seat, a box, a
// pending tail) and so must survive a restart.
//
// It is every status a record can hold, the closed set [PendingStore.SetStatus]
// accepts (a typo is refused, not a row no recovery matches) and listings match,
// so a status this build does not know is never mistaken for live work.
// RESUMED is here so boot recovery SEES a tail that died mid-flight; LAUNCHING
// because a node dying mid-launch left a box; ANSWERED so the resume it owes is
// found again by the seat's next holder.
var Active = []string{
	StatusLaunching, StatusRunning, StatusAwaiting, StatusReseed, StatusAnswered, StatusResumed,
}

// BridgeCall is one tool call a bridged run made.
//
// Its own shape rather than [tools.Call]: it crosses the wire into the fleet's
// coordination store, where another build reads it, so the json tags are a wire
// format and the type takes no dependency the store layer should not have. Same
// rule as [BridgeLog] and [PendingRun]: add fields, never rename; unknown keys
// ride [BridgeCall.Extra] and survive this build's writes.
type BridgeCall struct {
	Name string `json:"name"`
	// Args is what the caller passed, as JSON text rather than a decoded map,
	// which would pass the store's encoder twice and corrupt a large id.
	//
	// AS THE LOG KEEPS IT, not always whole: past the call's share of the log
	// ([MaxBridgeArgsBytes]) its largest values are replaced by a marker naming
	// size and digest, and ArgsBytes is the whole's size. Output keeps its
	// opening past [MaxBridgeOutputBytes], ending in a marker, with OutputBytes
	// the whole's size. Zero means kept whole, so a reader decides by the sizes,
	// never by parsing a marker.
	Args   string `json:"args,omitempty"`
	Output string `json:"output,omitempty"`
	Failed bool   `json:"failed,omitempty"`

	ArgsBytes   int `json:"args_bytes,omitempty"`
	OutputBytes int `json:"output_bytes,omitempty"`

	// ArgsUnparsed marks a call whose arguments DID NOT PARSE: the bridge
	// answered it unrun, so Args is empty as for a call made with none. A reader
	// tells them apart by this alone and reads it as arguments not known
	// (internal/agent/ledger's Call.ArgsUnread), never as a different call with
	// none. ADDITIVE: an older build drops the key and reads it as made with none.
	ArgsUnparsed bool `json:"args_unparsed,omitempty"`

	// Digest is the call's IDENTITY: the digest its run's call log admitted it
	// under (internal/agent/turnctx's ArgsDigest of its whole canonical
	// arguments), taken as the bridge admitted it, before anything was recorded
	// or cut. A resumed segment seeds its call log by it, so neither a cut
	// argument nor a call dropped from the middle ([BridgeLog.ElidedCalls])
	// moves a derived operation id. Empty for a call nothing admitted (refused
	// after the tools closed, or unparsed), for one an older build recorded,
	// seeded from its whole arguments, and for one withheld
	// ([BridgeCall.Withheld]), counted as a call whose arguments are not known.
	Digest string `json:"digest,omitempty"`

	// CallID is the HARNESS'S OWN ID FOR THE CALL, as the box sent it under the
	// `_meta` key the runner declares ([CallIdentifier], [OpenedJob.CallIDMeta]);
	// for Claude Code the tool_use id the session transcript files the call under.
	// Taken by the bridge on arrival (internal/api/mcpbridge's Call.CallID).
	//
	// THE BOX'S WORD, NO PART OF THE CALL'S IDENTITY: any value can arrive and a
	// repeated call may reuse one, so nothing finds a call by it (the update finds
	// its entry by place, instant and tool, [BridgeLog.inFlightAt]); two calls under
	// one id are two calls. WHOLE OR NOT AT ALL ([ValidCallID]): past
	// [MaxBridgeCallIDBytes] or outside letters, digits, `-` and `_` it is kept as
	// none, never cut. EMPTY for OpenCode, older builds, hook-reported skill loads
	// and person-approved calls ([approvedCall]). ADDITIVE ON THE WIRE (wirekeys.go).
	CallID string `json:"call_id,omitempty"`

	// At is when the log recorded the call, on the writing node's clock, set by
	// its store handle; the Executors board lists a run's calls by it. It is
	// the order calls FINISHED in, which for concurrent calls is whatever order
	// their tools take.
	At time.Time `json:"at"`

	// Seq is the call's place in the order the run's sessions admitted calls,
	// from 1: the harness's order, which a resume replays. One order for the
	// round: a job reopened after recovery, or continuing one that asked a
	// person, numbers after the calls the log holds, in-flight ones included
	// ([BridgeLog.LastSeq]). ZERO on a late call of an earlier round, whose
	// place was that round's ([takeEarlier]).
	Seq int `json:"seq,omitempty"`

	// Appended is the call's place in the order its ROUND'S LOG TOOK calls, from 1:
	// the append count once it took this one ([BridgeLog.Appends]), stamped by
	// [BridgeLog.appendCall], the one writer, so it rises by one per append through
	// every dropped middle ([MaxBridgeCalls]). Numbered as the call FINISHES, since a
	// save point counts calls whose outcome a session holds.
	//
	// A SAVE POINT IS READ AGAINST IT ([SessionCapture.Through], savepoint.go): a call
	// is after it exactly where Appended is past it, whatever its Seq or dropped
	// places. LATE and ABANDONED calls are held by no save point. ZERO on an older
	// build's call: read as after every save point (internal/agent/runner's heldAt);
	// its append still counts, so the dropped-call count may over-count, never under.
	Appended int `json:"appended,omitempty"`

	// Late marks a call its round's log took AFTER ITS JOB WAS REPLACED: still
	// running when a relaunch carrying the round on was readied, or when the round's
	// collection read the log, finishing only after its harness died or was replaced.
	// Its effect happened: listed and numbered like any call, but its spend is never
	// the later job's. THE LOG'S TO GIVE, in the append taking it once the log is open
	// to a later job or round ([BridgeLog.take], [takeEarlier]) and the opening
	// ([PendingRun.BriefRead]); never the person-approved call ([approvedCall]).
	//
	// HELD BY NO SAVE POINT: continuations built after it landed are told it, except
	// one the record says took it ([RunSteer.Tellings]); no skill guard counts a load
	// it made, nor is it the round's submission ([MaxSubmissionArgsBytes]). A dropped
	// one is counted ([BridgeLog.LateElided]). ADDITIVE ON THE WIRE (wirekeys.go).
	Late bool `json:"late,omitempty"`

	// Abandoned marks a call NOBODY WAITED FOR BY THE TIME IT RETURNED: its harness
	// gave up (cancelled at its tool timeout, hung up), or the job's bridge session
	// ended while it ran (internal/api/mcpbridge's Call.Abandoned). The bridge's to
	// give, kept as handed: listed, numbered and paid as its job's own, but its
	// session does not hold the outcome though the call ran.
	//
	// HELD BY NO SAVE POINT, as a late call ([BridgeCall.Late], savepoint.go): told to
	// continuations built after it landed unless a session took it; no skill guard
	// counts its load; the dropped middle keeps it as a late one
	// ([BridgeLog.LateElided]). Unlike a late call it may be the round's submission.
	Abandoned bool `json:"abandoned,omitempty"`

	// StartedAt is when the bridge took the call, UTC, before it wrote the
	// in-flight entry ([BridgeLog.InFlight]) so the span includes that write;
	// DurationMS is the time to its answer, the bridge's own measurement
	// (mcpbridge.Call) that the executor's record places the call by. With Seq
	// and Name it is the IDENTITY a call's one update finds its entry by
	// ([AppendRecord]): two openings of a job can reuse a place, never an
	// instant.
	//
	// BESIDE At, NOT INSTEAD: At is the store's and always there, a measurement
	// only when somebody made one. A zero StartedAt (a call answered without
	// running) was not measured, never an instant call.
	StartedAt  time.Time `json:"started_at,omitzero"`
	DurationMS int64     `json:"duration_ms,omitempty"`

	// Origin and Server are who answered the call, `builtin` or `mcp:<server>`
	// and the bare server name, as the surface resolved it. Empty on a call
	// nothing answered (a refused one).
	Origin string `json:"origin,omitempty"`
	Server string `json:"server,omitempty"`

	// Source is whose tool the call named, in the origin grammar, answered or
	// not: a call refused before any tool ran is still filed under its server
	// (mcpbridge.Call). Refusal is the engine's class for a failed call
	// (mcp.Refusal), empty on a success and on a third-party server's failure.
	Source  string `json:"source,omitempty"`
	Refusal string `json:"refusal,omitempty"`

	// Held marks a call to a tool at Needs approval that was not made but
	// recorded as the job's question ([PendingRun.HeldCall], ADR-0077): it ended
	// the job, as a submission or an ask does ([BridgeCall.Ends]).
	Held bool `json:"held,omitempty"`

	// Approved marks the call a person then let be made: by the run's resume on
	// the engine host, not the job over its bridge, appended after its result
	// was recorded on the row ([PendingStore.RecordHeldResult], [OwedCall]). A
	// call of its round like any other, which readers of whether the turn
	// replied need; its arguments, output and their sizes are never here and
	// ride the row sealed on the held call until the next job opens
	// ([approvedCall]).
	Approved bool `json:"approved,omitempty"`

	// HeldID names the held call an Approved call was made for ([HeldCall.ID]),
	// making its append to the log ONCE. The result is recorded on the row
	// first and the call appended after ([PendingStore.RecordHeldResult]), two
	// keys the store cannot write in one step, so every appender (that one, a
	// retry, the next job's opening carrying a call whose append never landed,
	// [OwedCall]) appends only where the log does not hold this id. Empty on
	// every bridge call. Additive on the wire.
	HeldID string `json:"held_id,omitempty"`

	// Committed is the kinds of change the call committed to its turn
	// (types.CommittedChange), which the run's collection settles the turn's
	// outcome by. The bridge's to give on every call it records as the tool
	// returns (internal/api/mcpbridge's Call.Committed), late
	// ([BridgeCall.Late]) and abandoned ([BridgeCall.Abandoned]) included: each
	// ran. Each kind at most once, which [MaxBridgeCallBytes] leaves room for.
	// Additive on the wire.
	Committed []string `json:"committed,omitempty"`

	// Runner is the process running a call whose entry is IN FLIGHT
	// ([BridgeLog.InFlight]): the incarnation its node holds its taker lease
	// under (coord.TakerResource), by which a collection on ANY node reads
	// whether that process still runs it; a call whose process stopped will
	// never be recorded and is not waited for. Empty on a finished call
	// ([keptCall]) and on an entry whose process holds no taker lease, waited
	// for by its bound alone ([BridgeCall.Until]). Additive on the wire.
	Runner string `json:"runner,omitempty"`

	// Until is the instant past which no collection waits for an in-flight
	// call: the bridge's stamp of its tool's own bound plus the bridge's margin
	// (internal/api/mcpbridge's wait.go), so a node not holding the call reads
	// it. Zero on a finished call ([keptCall]). Additive on the wire.
	Until time.Time `json:"until,omitzero"`

	// Extra is every key of the call this build does not know, kept verbatim and
	// written back by every write of the record (wirekeys.go). Filled by the
	// decode, never by a caller.
	Extra map[string]json.RawMessage `json:"-"`
}

// BridgeAppend is one step of a bridged call as the bridge hands it to the run's
// log ([PendingStore.AppendBridgeCall]): the entry written ahead of the run, its
// one update as it finishes, or the withdrawal of an entry whose call never ran
// ([AppendStep]).
type BridgeAppend struct {
	// Step is which of the three this is; zero is the record of a finished call
	// ([AppendRecord]).
	Step AppendStep

	// Launch is the job the session serves, named by [PendingStore.BeginLaunch]
	// and handed over through [LaunchRequest.Opened] before the box existed, so
	// no box call can name another. REQUIRED: an append naming no job is
	// recorded under none.
	Launch string

	// Round is the round the job works in ([PendingRun.Round]), as the launch
	// named it ([OpenedJob.Round]): what an append that outlived its job is listed
	// by, marked ([BridgeCall.Late]), in the round's log after a relaunch carried
	// the round on, and, a record, in a later round's log where a send-back opened
	// one ([takeEarlier]). EMPTY is an unknown round, dropped once its job is
	// replaced.
	Round string

	// Call is the call this append is a step of: for a begin its identity and
	// the harness's id ([BridgeLog.InFlight], [BridgeCall.CallID]), for a
	// withdrawal its identity alone, for a record the whole finished call. An
	// append with an empty [BridgeCall.Name] writes nothing.
	Call BridgeCall

	// Absorbed are the calls the call log took in from Call's own work (a
	// delegated worker's under `delegate`), by identity, Failed unset
	// ([BridgeLog.AbsorbedCalls]): listed nowhere, yet a resumed call log must
	// count them.
	Absorbed []ElidedCall

	// Wrote is the records Call's committed writes created or wrote, folded
	// into the log's set ([BridgeLog.Wrote]). A record's alone.
	Wrote []types.WrittenRecord

	// Spent is the session's running total of what its bridged calls cost the
	// engine. A record's alone.
	Spent EngineSpend
}

// AppendStep is which step of a bridged call one append is
// ([BridgeAppend.Step]). Never crosses a wire; its ZERO VALUE IS THE RECORD OF A
// FINISHED CALL, what every append was before entries were written ahead.
type AppendStep string

const (
	// AppendRecord is a call's ONE UPDATE as it finishes: its in-flight entry,
	// found by identity (place, the instant the bridge took it, tool,
	// [BridgeLog.InFlight]), is replaced by the finished call in one write, so the
	// log never holds two records of a call. A call with no entry (answered
	// without running, unparsed, or arriving after the tools closed) is listed as
	// it is.
	AppendRecord AppendStep = ""
	// AppendBegin writes a call's entry IN FLIGHT before its tool runs: the call
	// runs only once the log has taken it.
	AppendBegin AppendStep = "begin"
	// AppendWithdraw takes back the entry of a call that was then not run
	// (refused, or answered by its opening's end). Its one update.
	AppendWithdraw AppendStep = "withdraw"
)

// Valid reports whether s is one of the three steps.
func (s AppendStep) Valid() bool {
	return s == AppendRecord || s == AppendBegin || s == AppendWithdraw
}

// LogView is one state of a run's bridged tool log as a watch delivers it
// ([PendingStore.WatchLog]): the log, its version in the store, and whether
// there is one.
type LogView struct {
	Log     BridgeLog
	Version uint64
	Found   bool
}

// ErrCallsInFlight is what [PendingStore.AppendBridgeCall] answers for a begin
// with no room: [MaxCallsInFlight] calls of the round are in flight. Nothing is
// written, so the call is not run and is answered as unrecorded.
var ErrCallsInFlight = errors.New("sandbox: the run's log holds as many calls in flight as it keeps")

// ErrSessionBindRefused is a minting job's reported session the record will not
// bind ([PendingStore.BindSession]): the run fails closed, its session no id the
// engine can trust.
var ErrSessionBindRefused = errors.New("sandbox: the session the job reported is refused")

// SessionBind is a minting job's report to bind ([PendingStore.BindSession]): the
// job, the id its box reported, and whether its box continued the session its
// launch named (the box's record of its session) rather than starting its own.
type SessionBind struct {
	Launch    string
	ID        string
	Continued bool

	// Fresh, where set, is asked whether ID may be bound as a new session,
	// AFTER the claim is taken and before the record binds it (the executor
	// log's seek, [Coordinator.freshSession]): its error refuses the bind, as
	// it says, and releases the claim this bind took.
	Fresh func(ctx context.Context, id string) error
}

// ErrJobReplaced is what [PendingStore.AppendBridgeCall] answers BESIDE TRUE for
// an append of a job a relaunch replaced while carrying its round on: its calls
// are in the round's log (marked late, [BridgeCall.Late]) and what they cost is
// on no record, since the next job's resume would pay it as its own. Asking
// again writes nothing more. NO FAILURE OF THE APPEND; a caller retrying after
// an attempt that landed without its spend still holds that attempt's error,
// which this answer does not clear (internal/engine's
// bridgeLedger.appendRetrying).
var ErrJobReplaced = errors.New("sandbox: the append's job was replaced, its round carried on: its calls " +
	"are in the round's log, and what they cost is on no record")

// MaxBridgeCalls bounds the durable log of a bridged run ([BridgeLog]) in
// COUNT; [MaxBridgeCallBytes] bounds each call's share in bytes, together the
// calls' share of the log ([BridgeLogBytes], rowbytes.go).
//
// The log is ONE VALUE in the coordination store, written whole on every append,
// so it must be bounded. Two hundred is past what a reviewer reads and measures
// about half a megabyte full. The MIDDLE is dropped, never the start, kept as
// IDENTITIES ([BridgeLog.ElidedCalls]): costing the reviewer evidence, never a
// resumed segment an operation id.
const MaxBridgeCalls = 200

// The tools whose calls END an executor's run as the bridge records them:
// submitting its work, asking a person and parking on the answer (ADR-0068),
// or parking until another run leaves its seat (ADR-0176). Named here because
// the log's bound keeps them and the bridge's nudge reads them; each is
// declared once in internal/toolname.
const (
	SubmitWorkTool = toolname.SubmitWork
	AskAndWaitTool = toolname.AskAndWait
	WaitForRunTool = toolname.WaitForRun
)

// EndsRun reports whether a call to the tool named is one of the ways an
// executor's run ends ([SubmitWorkTool], [AskAndWaitTool], [WaitForRunTool]).
func EndsRun(tool string) bool {
	return tool == SubmitWorkTool || tool == AskAndWaitTool || tool == WaitForRunTool
}

// Ends reports whether the call is one that ended its job: a call to
// [EndsRun]'s two tools, or a call to a tool at Needs approval the bridge
// held ([BridgeCall.Held]), which parks the run as an ask does.
func (c BridgeCall) Ends() bool { return EndsRun(c.Name) || c.Held }

// Withheld reports whether the call's arguments, and so its identity, are
// withheld from the log: a call held for approval ([BridgeCall.Held]) and the
// call approved ([BridgeCall.Approved]), whose arguments ride the row sealed on
// the held call (ADR-0077). Neither arguments nor digest are ever in the clear
// (a digest beside the tool name would confirm a guess at what the seal hides),
// so a resumed call log counts it as a call of its tool with unknown arguments:
// an over-count no operation id reads, since no builtin is held and only a
// builtin derives one.
func (c BridgeCall) Withheld() bool { return c.Held || c.Approved }

// MaxCodeHolds bounds [PendingRun.CodeHolds]: internal/agent/turnctx's
// MaxHolds, so a turn's whole set always fits its row. This package imports
// neither the turn context nor anything that does; the engine's
// TestATurnHoldsWhatItsRowMay holds the two equal. A key is a scratch
// repository's id (39 bytes on the row) or a seat branch's key (72, Gitlet's
// RefHoldKey), so the field is at most about 72 KiB of the row's rest
// (rowbytes.go).
const MaxCodeHolds = 1024

// ErrCodeHoldsFull refuses a write that would take a row's
// [PendingRun.CodeHolds] past [MaxCodeHolds], writing nothing: the request that
// wanted the hold is refused naming the bound, never sent unheld.
var ErrCodeHoldsFull = errors.New("sandbox: the run already holds the most repositories and branches one run " +
	"may (1 024)")

// PendingRun is one detached job's durable state, keyed by its kick-off turn.
//
// The json tags are a WIRE FORMAT: the record lives in the fleet's coordination
// store, where another build reads what this one wrote, and a key the reader
// does not know decodes to zero (an emptied box reference is a leaked sandbox).
// Add fields; never rename or repurpose one.
type PendingRun struct {
	// TurnID is the RUN this record belongs to (one execution of a turn) and its
	// own key. See ADR-0017.
	TurnID string `json:"turn_id"`

	// WorkKey is the unit of work the run was dispatched for. It rides the row
	// so a resumed turn, re-entering mid-round with no trigger, keeps the
	// identity its writes are idempotent against. Empty on a turn with no
	// ledgerable trigger ("nothing to collapse").
	WorkKey string `json:"work_key,omitempty"`

	// WorkSince is when that unit of work began. Every operation id a turn derives
	// from the key carries it as mint time, so a resume unable to reproduce it would
	// derive DIFFERENT ids for the same writes, and the state log refuses deciding
	// again an operation minted before the operation ledger may have swept it.
	//
	// THE START, NOT NECESSARILY WHERE THE RESUMED HALF MINTS: a resume so late that
	// the ledger may have swept it mints at the instant the engine rebases onto. That
	// rule is the engine's, judged per attempt and kept in the coordination store,
	// not on this row. Zero where there is no key or timestamp; rebased like any
	// start past the horizon, identically in both halves.
	WorkSince time.Time `json:"work_since,omitzero"`

	AgentHandle string `json:"agent_handle"`
	AgentID     string `json:"agent_id"`
	Role        string `json:"role"`

	SandboxID string `json:"sandbox_id"`
	Harness   string `json:"harness"`

	// BoxSince is when the box this row names was first attached to it, on the
	// attaching node's clock: the age a relaunch reads off the box before
	// continuing in it ([KeptBoxMaxAge], [PriorBox.ContinuesIn]). Zero where the
	// row names no box.
	//
	// WRITTEN BY THE ATTACH OF A BOX THE ROW DOES NOT ALREADY NAME, kept by
	// every other: a job continued in the turn's kept box attaches the same box
	// again, and re-dating it each time would never age it, which the bound
	// exists to stop. Dated in the attach's own write so no attach can name a
	// box undated; the attach follows setup and precedes the first job, so every
	// checkout in it is younger. Cleared wherever the row stops naming a box.
	BoxSince time.Time `json:"box_since,omitzero"`

	// Placement is which configured backend holds this run's box.
	//
	// PERSISTED, not re-derived from config: the completion turn may run on
	// another node, days later, after the config was applied again, and
	// reconnecting to a container box through the direct cell reports a
	// vanished box and abandons a live run. Always resolved at launch
	// ([Manager.BuildSpec] fills the default).
	Placement string `json:"placement,omitempty"`
	CommandID string `json:"command_id"`
	Status    string `json:"status"`

	// LaunchID names the job this row currently holds.
	//
	// The row is the TURN's and a turn can run several jobs: a resumed turn that
	// relaunches reuses it, and [PendingStore.BeginLaunch] names each launch
	// anew. A completion carries the name of the job it saw finish, so a claim
	// can tell the job it was raised for from its replacement. See [Tail].
	//
	// Minted by the store, never the caller (a chosen id could be reused), in the
	// write that creates the row and every relaunch; never empty on a row.
	LaunchID string `json:"launch_id,omitempty"`

	// LaunchSeq is the launch's ORDINAL in its turn, from 1, minted with LaunchID
	// by [PendingStore.BeginLaunch], one past the launch before. Every capture of
	// the turn's session thus carries a (launch, capture) pair that only grows,
	// which the executor log orders captures by and refuses a late one with
	// ([SessionCapture]); LaunchID, random, cannot. A job launched under 0 is
	// never captured, since nothing could order it.
	LaunchSeq int64 `json:"launch_seq,omitempty"`

	// LeadsRun is the run its turn's leads part is of ([leadsPart.Run],
	// [PendingStore.Leads]): the job whose launch created the row. Written once,
	// so a part an earlier run left beside the row, which the store may leave by
	// design (coord's removeRunPart leaves a part beside a record created
	// again), is told from this run's own.
	LeadsRun string `json:"leads_run,omitempty"`

	// LeadsAppended is how many leads the leads part had taken as the newest
	// attach that marked any left it ([leadsPart.Appends]): what a reader holds
	// the part to, so a part that is gone, short, unreadable or another run's
	// reports every lead it cannot give back as not kept, never as none. Only
	// grows.
	LeadsAppended int `json:"leads_appended,omitempty"`

	// Capture is the newest capture of the turn's harness session the row
	// records (begun, landed, refused or unresolved) and Kept the newest that
	// LANDED, the one the executor log's row names; both nil until the turn's
	// first ([Coordinator.takeCapture]). The TURN'S, kept by every relaunch: the
	// turn's end reads them to say whether its newest attempt was kept, naming
	// the one not ([unkeptOf]), whether the turn went on past the logged session
	// ([wentOnPast]) and, where that attempt was its last word, its place
	// ([lastWordOf]). Each is a section with its own Extra (wirekeys.go).
	Capture *SessionCapture `json:"session_capture,omitempty"`
	Kept    *SessionCapture `json:"session_kept,omitempty"`

	// Saving is the turn's SAVE UNDER WAY, a capture of a job still working, from
	// its reservation ([PendingStore.ReserveSave]) until it can no longer become
	// the kept capture; Capture alone loses sight of it once a final take of the
	// job replaces it. A READING, NOT A ROW INVARIANT: a build without the field
	// writes it back from Extra, so it is read only through one rule
	// ([PendingRun.savingTake], [PendingRun.saving]), which a source walk holds
	// every reader to, beside the stamps that keep it ([stampOnto], [savingAfter],
	// [holdsStamp], [holdsSave], [stillBegun]). The TURN'S, kept by every
	// relaunch, with an Extra of its own (wirekeys.go). The package doc's "A save
	// under way" has the argument.
	Saving *SessionCapture `json:"session_saving,omitempty"`

	// Base is the read the turn's FIRST launch made of its conversation: where
	// its head was read, the turns handed out that the brief told, and the head,
	// which the turn's launches continue while it has kept no session
	// (conversationbase.go). Written only by [PendingStore.BeginLaunch] on the
	// write creating the record ([LaunchRequest.Base]), so a relaunch reads the
	// first read and never makes one; deleted with the record. Nil for a turn
	// that read no conversation. A section with an Extra (wirekeys.go).
	Base *ConversationBase `json:"conversation_base,omitempty"`

	// SpentSessions are the sessions a job of this turn ran out of its
	// model's context in, or had its request refused by its provider in, each
	// with which ([executorkeep.SpentSession]): no later launch continues them, a
	// launch that passes one over says which ([PendingRun.spentBy]), and the
	// turn's end names them with it on the executor log, which retires them.
	// The TURN'S, kept by every relaunch.
	// A KEY OF ITS OWN for this shape: one key in two shapes would leave a
	// build unable to decode a row of the other, and under two each carries the
	// other's in Extra.
	SpentSessions []executorkeep.SpentSession `json:"spent_sessions,omitempty"`

	// Summary is the newest compaction summary a box read of the turn's jobs
	// kept ([SummaryStamp], summary.go), which the turn's end names on the
	// executor log. The TURN'S, kept by every relaunch; moved only to a newer
	// one ([PendingStore.StampSummary]).
	Summary *SummaryStamp `json:"summary,omitempty"`

	// NotContinued is the verdicts of this turn's earlier jobs on the sessions
	// they did not continue, at most [executorkeep.NotContinuedMaxEntries]: the
	// first, and the newest, folded in as the next launch replaces a job's
	// record ([PendingRun.launchVerdict]). The TURN'S, kept by every relaunch;
	// the turn's end carries them with its current job's ([PendingRun.turnVerdicts]).
	NotContinued []executorkeep.NotContinued `json:"not_continued,omitempty"`

	// OwedEnd is the turn's END, decided and not yet on the executor log
	// ([Coordinator.publishEnd]): the [executorkeep.Ending] as JSON, carried whole by
	// every build. Written by whatever ended the turn ([PendingStore.OweEnd]) and
	// published by the run's ending before the row is deleted, which refuses until
	// then ([ErrEndOwed]), so the conversation's next turn is never dispatched before
	// the end it reads is recorded. DECIDED ONCE: a second path ending the turn
	// records this one and answers no colleague twice. AN END THIS BUILD CANNOT READ
	// came from a newer build and stays owed for a node that reads it, never
	// re-encoded without what it could not read.
	OwedEnd json.RawMessage `json:"owed_end,omitempty"`

	// EndRecorded says the executor log has the turn's end: the run's ending
	// published the owed end and cleared it in the same write that set this
	// ([PendingStore.ClearEnd]). The DURABLE HALF OF "DECIDED ONCE": an end
	// cleared from OwedEnd would read as never decided, and an attempt after a
	// crash between the clear and the delete (or a delete refused for a reply
	// still owed) would decide the end again and answer its colleague twice.
	EndRecorded bool `json:"end_recorded,omitempty"`

	// Launch is what the launch's own record needs about the job named by
	// LaunchID ([LaunchRecord]). Written whole with LaunchID on every launch.
	Launch LaunchRecord `json:"launch_record,omitzero"`

	// Owner is the process INCARNATION that owns this run's seat, and OwnerEpoch
	// the seat lease's epoch at the claim.
	//
	// The epoch is the FENCING TOKEN: every mutation on a live run carries it, so
	// a node whose lease moved cannot write even before it notices. The ownership
	// check is an optimisation; the fence is the guarantee. Empty owner means
	// unclaimed, as an in-flight run is just before its seat's new owner
	// recovers it.
	Owner      string `json:"owner"`
	OwnerEpoch int64  `json:"owner_epoch"`

	// TaskDescription is what a person reads the run by, in the launching turn's
	// own words, redacted and unfenced: what the turn was asked, or the line a
	// feed shows its trigger by ([LaunchRequest.Task]). A resume reads its brief
	// off the turn's park, never this.
	//
	// HELD TO [MaxRunTitleBytes] by the write creating the row, keeping its
	// opening with the cut said: a trigger's words are bounded by nothing below
	// the broker's payload, and the turn's own record keeps the whole.
	TaskDescription string `json:"task_description"`

	// Label is what the run is named by on the running-runs panel
	// ([LaunchRequest.Label]), so a process learning of the run from this row
	// after a restart names it the same way. Held as TaskDescription is.
	Label string `json:"label,omitempty"`

	// Reply is who is waiting for the suspended turn: [turn.Reply]'s wire value,
	// a plain string because the row crosses builds.
	//
	// Persisted rather than re-derived: the resumed turn does not see the
	// trigger, so without it a turn somebody awaited could come back free to end
	// in silence. Every launch writes a kind (`none` when nobody waits), so a
	// resume refuses a row naming none rather than guessing; see [turn.Reply].
	Reply string `json:"reply,omitempty"`

	// PartitionKey is the inbox PARTITION key the run was launched under: the batch
	// its kick-off trigger arrived in.
	//
	// NOT WHAT ADMITS AN ANSWER (the chat prompt tells a seat to reply AS A THREAD,
	// so an answer to a top-level DM question arrives in a finer partition and
	// equality never met), but what tells two admitted runs apart: a DM's identity is
	// the whole channel, so a reply in either of two threads admits both and this
	// alone says which thread each was asked in ([ConversationRef.Best]). Empty for
	// wakes naming no conversation (A2A ask, task assignment); a scheduled fire names
	// its schedule's conversation here and as ConversationKey
	// (internal/notify's ScheduleConversation).
	PartitionKey string `json:"partition_key,omitempty"`

	// ConversationKey is the durable conversation this run belongs to: what the
	// turn's executor log rows (captures, and the end its conversation's next turn
	// reads) are keyed by and, since a person answers on the conversation rather than
	// into the batch, what admits an arriving delivery as its answer. Not the
	// partition: for a DM that is the batch a delivery arrives in, this the line a
	// person talks on, and one value for both failed each way (a DM's work hidden from
	// the next turn, or the answer lost). Spelled `conversation_key` as on every event
	// carrying the identity; empty where a wake named no conversation.
	ConversationKey string `json:"conversation_key,omitempty"`

	// Branch is the pushed WIP branch: the durable half of the work, and what a
	// re-seeded run starts from when its paused box is gone. It is the branch the
	// run's ask named ([Ask.Branch]), held to the engine's grammar
	// ([gitref.ValidBranch]: git-accepted, valid UTF-8, at most
	// [gitref.MaxBranchBytes]), empty when the ask named none, and never one of
	// the delivered refs, which are as often a pull request's URL.
	Branch string `json:"branch"`

	// SessionID is the harness session the job's launch named before its harness
	// started (the one it continues, or one minted), written by the attach that
	// names the box ([PendingStore.AttachSandbox]) and by nothing a box says.
	// Which session the box ran is [PendingRun.SessionRan], read off this, the
	// launch's other session and the box's word on its token.
	SessionID string `json:"session_id"`

	// Question is what the job asked a person, in one sentence, and Audience who
	// it asked, as the run wrote them; both empty on a job that asked nothing.
	// QuestionContext is the ask's Markdown context, Decision the choice it asks
	// for (nil for an open question). Written together with [PendingRun.Branch]
	// by [PendingStore.RecordAsk] and cleared together when the next job opens;
	// the branch stays for the job carrying the work on. See [Ask].
	Question        string             `json:"question"`
	QuestionContext string             `json:"question_context,omitempty"`
	Decision        *decision.Decision `json:"decision,omitempty"`
	Audience        string             `json:"audience"`

	// HeldCall is the call to a tool at Needs approval the question asks about,
	// nil for a question the run asked itself (ADR-0077). Written with the
	// question by [PendingStore.RecordAsk], pinned to the job that made it
	// ([HeldCall.LaunchID]), marked and given its result by the resume that makes
	// it ([PendingStore.MarkHeldCall], [PendingStore.RecordHeldResult]), and
	// cleared when the next job opens, never by a claim given back: that keeps
	// the call from being made twice. A KEY OF ITS OWN AT THE TOP OF THE ROW,
	// carrying later keys inside it ([HeldCall.Extra]).
	HeldCall *HeldCall `json:"held_call,omitempty"`

	// WaitsFor is the run the job parked to wait for (`wait_for_run`), nil for
	// any other question: written with the question by [PendingStore.RecordAsk],
	// answered by the engine once that run leaves [Holding] (ADR-0176), and
	// cleared when the next job opens. A KEY OF ITS OWN AT THE TOP OF THE ROW,
	// carrying later keys inside it ([RunWait.Extra]).
	WaitsFor *RunWait `json:"waits_for,omitempty"`

	// ParkedCalls is every bridged call of the job answered still running
	// before its harness's ceiling, its job parked on it ([Ask.Call],
	// callwait.go), in the order they parked: each told with what it returned
	// by the resume that carries the run on, whatever answers its ask.
	// Written by [PendingStore.RecordAsk] and cleared with the question.
	// ON THE ROW, NOT IN [RunWait]: a call parks beside a person's question
	// too, and a wait on that row would read as one put to nobody wherever
	// WaitsFor is taken for that (the audience, replies, conversation mail).
	ParkedCalls []CallWait `json:"parked_calls,omitempty"`

	// WaitOver is the claim of the run the engine carried on, its wait over
	// ([PendingStore.ClaimWaitOver]): never an answer (waitover.go). Written by
	// the claim, taken by its turn, revived by the seat's next holder, and
	// cleared with the question and by an answer recorded.
	WaitOver *WaitOver `json:"wait_over,omitempty"`

	// RunSendsFor and RunSends are how many messages the job RunSendsFor names
	// sent other runs ([PendingStore.CountRunSend]), within [MaxJobRunSends]: a
	// count for an older job counts nothing.
	RunSendsFor string `json:"run_sends_for,omitempty"`
	RunSends    int    `json:"run_sends,omitempty"`

	// OwedCall is the person-approved call the round's log is owed ([OwedCall]):
	// set by the reset that clears a held call whose result is recorded
	// ([PendingStore.BeginLaunch]), appended by the opening that follows, and
	// kept until a job of the round has launched past it.
	OwedCall *OwedCall `json:"owed_call,omitempty"`

	// ParkedInputTokens and ParkedOutputTokens are what the job that asked the
	// question cost, recorded with it.
	//
	// ON THE ROW because the segment paying for them did not collect them: a job
	// parking on a question is collected, charged and parked, and the turn
	// resumes only when a person answers, maybe days later on another node with
	// nothing collected. That resume is the job's segment (same launch) and its
	// charge to the work item includes these as a completion's resume does
	// (ADR-0022); without them the task was charged for the collection and never
	// for the launch that asked.
	ParkedInputTokens  int `json:"parked_input_tokens,omitempty"`
	ParkedOutputTokens int `json:"parked_output_tokens,omitempty"`
	// ParkedCacheReadTokens and ParkedCacheWriteTokens are the cached share of
	// ParkedInputTokens for the same charge ([ResumeRequest.CacheReadTokens]).
	ParkedCacheReadTokens  int `json:"parked_cache_read_tokens,omitempty"`
	ParkedCacheWriteTokens int `json:"parked_cache_write_tokens,omitempty"`

	// AskedAt is the instant the question was put, taken before it was
	// announced, so before anybody could have read it.
	//
	// THE ANCHOR EVERY ANSWER IS MEASURED AGAINST: a reply qualifies only if
	// posted at or after it ([Reply.Posted]), however it is threaded. Matching
	// purely by position ("the next inbound on the conversation") spliced a reply
	// to an EARLIER question, redelivered behind newer mail after a failed
	// resume, into the run as the answer to its next ask, and took a message sent
	// while the job ran as the answer the moment it parked.
	//
	// REQUIRED on every park ([PendingStore.MarkAwaiting]): without an anchor no
	// reply could be shown to answer.
	AskedAt time.Time `json:"asked_at,omitzero"`

	// Answer is the reply recorded as this question's answer, set exactly while
	// the run is [StatusAnswered] and while that answer's resume is claimed,
	// until a turn takes it or the ending lets it go back to the seat
	// ([PendingStore.OweHandBack]). A row holding one no turn took is never
	// deleted ([ErrAnswerOwed]). See [RecordedAnswer].
	Answer *RecordedAnswer `json:"answer,omitempty"`

	// AnswerRefusal is set where the recorded answer is a REFUSAL: the reply was past
	// what the row carries ([MaxAnswerBytes]), so the row records its figures beside
	// [PendingRun.Answer] and none of its words ([PendingStore.RecordRefusal],
	// ADR-0114). Read through [PendingRun.Refused]; nil on every other row.
	//
	// A KEY OF ITS OWN AT THE TOP OF THE ROW, not a field of [RecordedAnswer], as
	// [PendingRun.Stop] is: every node rewrites the row whole and an unknown key
	// survives only at the top ([PendingRun.Extra]) or in a section with its own
	// Extra (wirekeys.go), which [RecordedAnswer] lacks; inside it an older build
	// would drop the refusal on its next write. Cleared by every other write that
	// records, lets go of or clears the answer.
	AnswerRefusal *AnswerRefusal `json:"answer_refusal,omitempty"`

	// EndingReturnsRefusal is set with the ending ([PendingRun.Ending]) where
	// what it hands back is a reply too large for the run, whose refusal the run
	// held untold ([PendingRun.AnswerRefusal]): the delivery goes back to the
	// seat's inbox as the message it came in, and the announcement says so
	// rather than "the answer a person gave it", since the run got none of it.
	// Decided in the ending's write, as [RecordedEnding.Returned] is, because the
	// let-go clears the refusal before the announcement.
	//
	// A KEY OF ITS OWN AT THE TOP OF THE ROW, like [PendingRun.Stop]:
	// [RecordedEnding] has no Extra, so a field of it would be dropped by an
	// older build's next write (wirekeys.go).
	EndingReturnsRefusal bool `json:"ending_returns_refusal,omitempty"`

	// DeclinedAnswers are the event ids of deliveries this question was recorded
	// with and let go of (every resume with them failed, so they took the seat's
	// ordinary route, [Coordinator.declineAnswer]), and the ids of the copies
	// handed back. Neither is recorded as this question's answer again, which
	// stops a copy circling the run it failed to reach. Bounded by
	// [maxDeclinedAnswers]; cleared with the question.
	DeclinedAnswers []string `json:"declined_answers,omitempty"`

	// HandBack is what an answer let go of still owes the seat's inbox: copies of its
	// deliveries, written IN THE SAME WRITE that lets the answer go
	// ([PendingStore.DeclineAnswer], [PendingStore.OweHandBack]), removed once published
	// ([PendingStore.ClearHandBack]). AN OUTBOX ON THE ROW: the decline is a compare-and-set plus a broker publish and
	// neither order survives a crash alone, so the write carries the copies, published
	// by whichever node reads the row next under derived ids (deduped by the inbox). A
	// run that ENDS publishes before its record is deleted, which refuses while anything
	// is owed ([PendingStore.Finish]). BOUNDED BY ONE DECLINE: copies are added only to
	// a row owing none ([ErrHandBackOwed]), at most one inbox batch
	// ([queue.DefaultBatchOptions]); the retry publishes first ([Coordinator.retryOwed]).
	HandBack []HandedBack `json:"hand_back,omitempty"`

	// Ending is the run's ending once DECIDED, nil while live. From the decision
	// on the row takes only the ending's own steps, and whoever reads it next
	// finishes it on the recorded terms ([RecordedEnding]).
	Ending *RecordedEnding `json:"ending,omitempty"`

	// Stop is who ended the run by pausing its seat with a stop, recorded in the
	// write that DECIDES the ending ([Decision.Stop]), nil otherwise; its
	// presence on a decided row makes that ending a stop ([RunStop]).
	//
	// RECORDED WITH THE DECISION, not after: if the stopping node died between
	// deciding and announcing, the seat's next holder has only the row to say a
	// person ended the run and who. A KEY OF ITS OWN AT THE TOP OF THE ROW, not a
	// field of [RecordedEnding], which lacks an Extra (wirekeys.go) and would lose an
	// unknown key on any node's whole-row rewrite ([PendingRun.Extra]).
	Stop *RunStop `json:"stop,omitempty"`

	// WorkItem is the one work item the launching turn was charged to, nil when
	// it was on nothing.
	//
	// ON THE ROW because the resumed turn has no trigger to resolve it from: the
	// dispatch is gone and a person's answer is an ordinary chat message naming
	// nothing. The resume reads it as its own item under [types.BasisResume].
	WorkItem *types.WorkItem `json:"work_item,omitempty"`

	// WroteKeys is every record the turn's calls created or wrote
	// ([BridgeLog.Wrote]), each as its key ([writtenKey]): what one listing of
	// the fleet's runs relates another seat's run by ([RunsOn]), since a listing
	// reads no log. Folded in by the write that records a call's spend, or for
	// a late call one of its own ([CoordStore.AppendBridgeCall]), from the log's
	// whole set, so a write lost is made good by the next. Never dropped, held
	// to [types.MaxWrittenRecords] as the log's set is.
	WroteKeys []string `json:"wrote_keys,omitempty"`

	// Answered are the comments answering the seat's questions whose deliveries
	// woke the launching turn, ON THE ROW as WorkItem is, so a call a later job
	// of the turn holds still names the decision it carries out.
	Answered []string `json:"answered,omitempty"`

	// AudienceHandles are the seats a parked question may be answered by, and
	// AudienceFallback whether that set fell back to a default rather than what
	// the run named.
	//
	// RESOLVED ONCE, AT THE PARK, by [CoordinatorOptions.Audience] against the
	// parking node's chart, in the question's write: [PendingRun.Audience] is a free
	// label ("requester", "team", "manager") never resolved, and who the manager WAS
	// at the moment asked is who was asked. Empty until the run parks, cleared when a
	// new job opens ([PendingStore.BeginLaunch]). Held to [MaxAudienceHandles], first
	// kept in resolution order (a `team`'s lead then its people, the fallback's lead
	// chain nearest first).
	AudienceHandles  []string `json:"audience_handles,omitempty"`
	AudienceFallback bool     `json:"audience_fallback,omitempty"`

	// Requester is the seat whose message, notice or ask woke the launching turn,
	// whom a question to "requester" means; empty when nothing a seat said woke
	// it (a schedule, a sender the org does not know). By the immutable handle
	// (ADR-0013), so the row names the same seat while the run is parked.
	//
	// ON THE ROW because the park that resolves the audience runs when the job
	// finishes, possibly days later on another node, with the dispatch gone. A
	// row without it resolves "requester" to the fallback.
	Requester string `json:"requester,omitempty"`

	// RequesterLogin is the credential acting as no seat (a Tier A token, a
	// service account) whose write woke the launching turn, by login, never
	// value, and RequesterApps the apps its writes carried as the taking node
	// resolved them. Empty wherever [PendingRun.Requester] is set and for a turn
	// no credential woke.
	//
	// ON THE ROW because every resuming segment, and every bridge call after a
	// takeover, still works for that credential: a call may use the narrower of
	// these apps and the running node's own resolution of the login (ADR-0109).
	// Without them the turn reads as a stranger's. Nobody is asked anything for a
	// credential: "requester" resolves to the fallback, as with no requester.
	RequesterLogin string      `json:"requester_login,omitempty"`
	RequesterApps  []iam.Grant `json:"requester_apps,omitempty"`

	// WorkingFor is who the turn works for when a colleague's ask, answer, or
	// write on Worklet, Doclet, Chatlet or Gitlet handed it on
	// ([types.WorkingFor]); its resumed turn works for them too.
	WorkingFor *types.WorkingFor `json:"working_for,omitempty"`

	// ColleagueAsk is the colleague's A2A question the turn answers, nil for a
	// turn no ask woke ([ColleagueAsk]). THE TURN'S, kept by every relaunch's
	// reset like the identity fields: a reviewer's send-back is the same turn
	// answering the same ask.
	ColleagueAsk *ColleagueAsk `json:"colleague_ask,omitempty"`

	// TraceID and SpanID are the trace the run started under, so the follow-up
	// turn nests beneath it.
	TraceID string `json:"trace_id"`
	SpanID  string `json:"span_id"`

	DelegationDepth int      `json:"delegation_depth"`
	DelegationChain []string `json:"delegation_chain"`

	// ExecuteState is the PARKED TURN's serialized state (tool surface, rounds so
	// far, frozen context, what it wrote and spent), opaque to this package.
	//
	// Empty exactly while the run is [StatusLaunching] (the turn writes it when its
	// frame unwinds; nothing polls or claims in that window). A claimed run WITHOUT
	// one is a row the launch path did not write, which the coordinator fails rather
	// than resume into nothing. HELD TO [ExecuteStateBytes], its share of the row:
	// the park condenses (internal/agent/execstate's EncodeWithin) and
	// [PendingStore.MarkSuspended] refuses past it ([ErrStateTooLarge]). RAW BYTES:
	// as a decoded map it was decoded twice per suspension and numbers read as
	// float64 corrupted ids past 2^53. A JSON null reads back as none (decodeRun).
	ExecuteState json.RawMessage `json:"execute_state"`

	// CodeHolds are the hold KEYS of what the run's turn holds (internal/agent/
	// turnctx's Holds), carried never decoded: the seat's scratch repositories
	// it named, by id, held from Gitlet's expiry, and the seats' branches it
	// read, wrote, launched on or pushed from its box, by Gitlet's ref key. Every
	// key the turn names while the row exists is written BEFORE the request that
	// named it ([PendingStore.AddCodeHolds]).
	//
	// UNIONED, NEVER REPLACED, unlike everything else a launch resets: the turn may
	// still depend on what an earlier job touched, and the expiry reads only the row
	// once parked. Deleted with the row, the whole release: Gitlet's retention pass
	// lists every row whatever its status ([PendingStore.ListRuns]). Bounded by
	// [MaxCodeHolds].
	CodeHolds []string `json:"code_holds,omitempty"`

	// Repos are the run's grants on Gitlet: none at a fresh launch, one more with
	// every `code_checkout` the job's box makes ([PendingStore.GrantRepo],
	// ADR-0093). Written by [PendingStore.BeginLaunch] from [Spec.Repos] and
	// REPLACED by every relaunch, since a job's repositories are that job's: a
	// job carrying on a round ([LaunchRequest.Continue]) gets the grants of the
	// job it continues, any other none. A box's request to Gitlet is admitted
	// against them for the job's life: the row, never the box, says what it may
	// clone and push.
	Repos []RunRepo `json:"repos,omitempty"`

	// CodeFloor is the run's SESSION on the code log: a position (a
	// statelog.Position token, `<stream>@<generation>:<sequence>`) at or above
	// everything its box was shown or pushed, so a read served at it never shows
	// a branch older than one already seen.
	//
	// CARRIED, NEVER READ HERE, like ExecuteState: this package holds no log.
	// Written by the launch carrying a job's grants on and by the first checkout
	// of one with none (the furthest position its resolution read), raised only
	// through [PendingStore.RaiseCodeFloor] and [PendingStore.GrantRepo]; empty
	// for a run with no repositories.
	CodeFloor string `json:"code_floor,omitempty"`

	// CodeLaunch is the code floor the launch wrote, NEVER RAISED: every box push
	// applies above it, so what the run pushed is read from its repositories'
	// pushes above this position, not through a code-log index every box push
	// would pay for on every data node. Written by [PendingStore.BeginLaunch]
	// beside CodeFloor, on create and every relaunch, and by a job's first
	// checkout where the launch gave none ([PendingStore.GrantRepo]): no box
	// pushes before it.
	CodeLaunch string `json:"code_launch,omitempty"`

	// CodeCeiling is the handle of the colleague SEAT whose ask woke the launching
	// turn, the requester ceiling the repositories were resolved under: that seat's
	// own grant on each repository bounds what the box may do there as it bounds the
	// turn's code tools (Gitlet's box admission, gitlet.Grant.Under). Empty where no
	// colleague bounds the job (nobody, the seat itself, or a person, who maintains
	// every repository). CARRIED, NEVER READ HERE, like CodeFloor; JOB-SCOPED like
	// Repos: set with them, replaced by every relaunch.
	CodeCeiling string `json:"code_ceiling,omitempty"`

	// CodeWorkingFor is whom the launching turn works for, as the job's first checkout
	// composed it: the carrier every code write of that turn states
	// (internal/agent/builtin's RunRepos.WorkingFor, ADR-0109); nil for a job working
	// for its own seat. The box's pushes and the change its collection opens are the
	// turn's writes made late, so each carries it. A credential's holds the box to its
	// own grant (which pushes nothing) and the open is asked under the ceiling of
	// nobody (ADR-0110). CARRIED, NEVER READ HERE; job-scoped like CodeCeiling. ONE
	// REQUESTER A JOB: a checkout for anybody else is refused ([GrantConflict]); one
	// for the same credential with fewer apps narrows, never widens
	// ([PendingStore.GrantRepo]).
	CodeWorkingFor *types.WorkingFor `json:"code_working_for,omitempty"`

	// Charged is whether this launch's collected tokens are on the fleet's token
	// counter.
	//
	// ON THE ROW, not in memory: the charge sits in the RETRIED part of the tail (a
	// failed resume hands the claim back and the retry, here or on the seat's next
	// owner, collects the finished job again), so without a record every retry
	// charged the seat's and company's budgets again. WRITTEN BY THE RELEASE handing
	// the claim back ([Release]), the only write a retry passes through; recorded
	// separately, a refused write then accepted release charged twice. Launch-scoped:
	// [PendingStore.BeginLaunch] clears it, nothing else.
	Charged bool `json:"charged,omitempty"`

	// CompanyCharged is whether this launch's collected tokens are on the
	// COMPANY's counter while the seat's write failed ([coord.SeatUncountedError]);
	// meaningful only while Charged is not.
	//
	// The partial is KEPT, never undone: an undo left the spend on neither counter
	// whenever the resume then succeeded, since only a failed resume brings a
	// collected run back to be charged. Recorded here, the retry finishes the
	// seat's share alone rather than counting the company twice; a successful
	// resume leaves only the seat short by this run.
	//
	// Written and cleared exactly as Charged is.
	CompanyCharged bool `json:"company_charged,omitempty"`

	PauseTTLSeconds float64 `json:"pause_ttl_seconds"`

	// PausedAt is when the run's box was paused, zero when no pause was recorded.
	// With SandboxID it is the engine's record of the box, letting the reaper
	// reclaim a paused box nothing else would free.
	//
	// ZERO IS NOT "NOT PAUSED", the reading that leaked boxes: the stamp is a
	// SECOND, warn-only write after the box is paused, so a parked run can hold a
	// paused box this field does not show. Every caller acting on a held box reads
	// [PendingRun.HeldSince].
	PausedAt time.Time `json:"paused_at"`

	// ClaimedFrom is TRANSIENT, never persisted (`json:"-"`): the status the row
	// held immediately BEFORE a claim flipped it to resumed. A failed resume
	// dispatch reverts to exactly this so a NAK'd trigger can re-claim on
	// redelivery; inferring it from other fields is unsound, since a reused run
	// keeps its old question.
	ClaimedFrom string `json:"-"`

	// Collected is TRANSIENT: set on a claimed row by the completion tail once
	// THIS claim's collection read the box back, so whichever hand-back follows
	// (a failed park or resume) ends the run of failed collections
	// ([Release.Collected]). A fact about one claim, never persisted.
	Collected bool `json:"-"`

	// BoxAnswered is TRANSIENT, set earlier: the moment THIS claim's read of the
	// box answered, so a hand-back a later step makes uncounted (store silent, or
	// this node stopped) still ends the run of failed collections
	// ([Release.BoxAnswered]). Never persisted, like Collected.
	BoxAnswered bool `json:"-"`

	// BoxHarnessEnd is TRANSIENT: how the harness ended as THIS claim's read of the
	// box found it, set with BoxAnswered, so every later hand-back records it in
	// its own write ([Release.HarnessEnd]). A lost job's is empty: its loss record
	// already says the job is over ([PendingRun.Lost]).
	BoxHarnessEnd HarnessEnd `json:"-"`

	// Deadline is when the launching turn stops being worked on: its wall-clock
	// cap (a scheduled fire's) from when the turn began, UTC. Zero for any other
	// trigger.
	//
	// ON THE ROW because a detached run can outlast the cap by hours, the turn's
	// own segments refuse rounds past it, and a run is no round of theirs. The
	// poll announces a job passing it as if finished ([Waiter]) and the
	// collection ENDS the job as a stop's read does (harness told to stop,
	// session packed as its own exit packs it, [Coordinator.readBox]) and says
	// why, so the turn resumes to judge what was done instead of being cut off
	// unrecorded. Written by every launch of the turn.
	Deadline time.Time `json:"deadline,omitzero"`

	// LaunchNode is the node that launched the job the row's box was attached
	// for: the node whose host the box is on.
	//
	// ON THE ROW because every box is a process tree or a container on that
	// node's host, reachable only by it. Written by every launch and WITH THE BOX
	// ([BoxRef.Node]): a relaunch still naming the previous box names its host until it
	// attaches its own, so every reclaim goes to the right host. IT NAMES WHO DRIVES
	// THE BOX: that node's waiter polls it, keeps it alive, reaps its pause and
	// reclaims it ([Waiter]); others ask it for a collection ([BoxReader]).
	LaunchNode string `json:"launch_node,omitempty"`

	// Lost is the verdict that the job was lost with its host ([JobLoss]): killed
	// with its engine by a restart, box gone, or launch node gone for good; or a
	// job whose box only its node's host can read, on a node that could not read
	// the run's row for the host grace ([LossLaunchNodeStoreSilent]). Nil
	// otherwise. Recorded because the node reaching it is not always the one
	// acting on it ([Coordinator.carryOnLost], [Coordinator.loseWithHost]), and on
	// the row so a completion anybody could publish cannot restart a round.
	// Cleared by a relaunch. See the package doc, "Lost jobs".
	Lost *JobLoss `json:"lost,omitempty"`

	// RoundLosses is how many jobs of the round were lost before this one
	// ([PendingRun.Lost]), each carried on by the next. Kept by a relaunch
	// carrying the round on ([PendingRun.Continues]), counting the job it follows
	// if lost; zero on a new round. Bounds restarts of one round
	// ([MaxRoundLosses]).
	RoundLosses int `json:"round_losses,omitempty"`

	// Round names the executor round the row's job belongs to: the job that
	// opened it. Set to the new job by a launch starting a round (create, and
	// every relaunch not continuing one) and carried by one that does
	// ([PendingRun.Continues]). The log beside the record keeps the round's calls
	// under the same name ([BridgeLog.Round]), and a launch keeps a log of its
	// own round whichever of its jobs the log was last opened to.
	//
	// ON THE RECORD, in the reset's own write, because the log's opening is a
	// second write that can fail or never come if the node stops: a relaunch
	// judging by the job the row held just before it found the log on the job
	// before THAT, started a carried-on round afresh, and dropped the calls made
	// before its question and the submission its delivery check reads.
	Round string `json:"round,omitempty"`

	// AnsweredJob names the last job of the round whose question a person's
	// answer carried the round on from: set by a relaunch continuing a job that
	// asked, carried by one continuing any other, cleared by a new round. The
	// log's cut ([BridgeLog.AnsweredThrough]) is taken once, by the first opening
	// finding the log not yet cut for it ([BridgeLog.CutFor]), so an answer whose
	// opening failed is still cut by the next relaunch and a cut taken is never
	// moved.
	AnsweredJob string `json:"answered_job,omitempty"`

	// RoundRefusals are the replies refused as too large for the round's questions
	// ([AnswerRefusal]), oldest first, for the round's review: job, message count and
	// figures, never words (ADR-0114). Appended by the relaunch carrying the round on, in
	// the write clearing the refusal ([CoordStore.resetLaunch]); cleared by a new round;
	// held to [maxRoundRefusals]. THE REVIEWER'S, which judges from calls alone, where a
	// re-asked question reads as a duplicate. A top-level key like
	// [PendingRun.AnswerRefusal]. AND THE ROW'S WORD THAT A TURN KEPT THE REFUSAL: an
	// entry naming the job says the reply's delivery is spent where the ledger fails open
	// ([PendingRun.carriedOnFromRefusal], [Coordinator.refusedReply]).
	RoundRefusals []RoundRefusal `json:"round_refusals,omitempty"`

	// HeldFor and HeldUntil hold back the resume of a job whose seat's conditions refused
	// it (today the token budget). The resume waits until HeldUntil (the refusal lifting
	// on the clock) or until the condition HeldFor names is signalled
	// ([Coordinator.Readmit]). Written by the release handing the claim back
	// ([Release.Hold]); cleared by the next release, a relaunch or a lifted hold. THE ROW
	// STAYS RUNNING, so its seat stays held; the poll announces no completion while held
	// ([PendingRun.Held]) and no claim takes it ([PendingStore.ClaimForResume]); retried
	// at once, every retry re-collected the job and reran its review. ONLY WHAT EVERY NODE
	// READS ALIKE: a model's limit is the resuming node's and is held in its memory
	// ([Coordinator.keepRefused]), else it would silence the job for every holder.
	HeldFor   Condition `json:"held_for,omitempty"`
	HeldUntil time.Time `json:"held_until,omitzero"`
	// Holds counts the holds releases have put on the run, the current one last:
	// a lift names the hold it read, so one read before a newer hold clears
	// nothing ([PendingStore.LiftHold]).
	Holds uint64 `json:"holds,omitempty"`

	// Continues is TRANSIENT, an instruction to [PendingStore.BeginLaunch]: the
	// job it opens CONTINUES the one before, which parked on a question or was
	// lost with its host ([LaunchRequest.Continue]), so the reset keeps that
	// job's bridged calls. They are the same round's, and the collection closing
	// the round rebuilds it from that log, submission and deliveries included.
	// Every other relaunch is a new round whose log starts with only what its
	// collection did not read.
	Continues bool `json:"-"`

	// BriefRead is TRANSIENT, an instruction to [PendingStore.BeginLaunch]: the
	// read of the round's log the relaunch's briefs were built from
	// ([LaunchRequest.BriefRead]), nil for none, applied only to a log still open
	// to the read's job. To a job carrying the round on it marks LATE
	// ([BridgeCall.Late]) every call the log took after the read, counting late
	// any dropped from the middle since ([BridgeLog.LateElided]); into a new round
	// it carries them, late and placeless ([BridgeLog.carried]); a person's
	// approved call apart, which the relaunch's lead tells. Same mark as a call
	// taken after the opening, so no save point holds either. Never persisted: a
	// read is the relaunch's, and an opening that never landed is made by the
	// next relaunch after its own read.
	BriefRead *LogRead `json:"-"`

	// Steer is the notes a person sent the run's turn from launch to the turn's
	// end, sealed, with the seat anchor authority to send one is decided on
	// ([RunSteer], notes.go). Kept by every reset: the TURN's, and a relaunch is
	// the same turn.
	Steer RunSteer `json:"steer,omitzero"`

	// FoldIns are the questions the job's running work asked of the people who
	// may let another writer's request join it ([FoldIn], foldin.go), at most
	// [MaxJobFoldIns]. The JOB's: cleared when the next job opens
	// ([PendingStore.BeginLaunch]), so no answer reaches work it was not
	// asked for.
	FoldIns []FoldIn `json:"fold_ins,omitempty"`

	// BriefNotes is TRANSIENT, an instruction to [PendingStore.BeginLaunch] like
	// [PendingRun.Continues]: the notes the job's brief carries, which the write
	// opening the job records read by it. A note the record holds, or never held
	// (the turn's first segment's own), is added already read; one a lost take's
	// settle handed the brief, naming that take, is read by the job in its place
	// among the reads ([RunNote.LostTake]). Never persisted as itself.
	BriefNotes []RunNote `json:"-"`

	// RetoldNotes is TRANSIENT: notes the turn has read that the brief tells
	// again, by ID, each with the read to record. They are ones the record marked
	// unanswered as the brief was built, a take having recorded them read and its
	// answer never reaching its reader. The opening records each read by the job,
	// told again ([NoteRead.Retold]), at that take's own place among the reads,
	// where the record still marks it. Never persisted as itself.
	RetoldNotes []RunNote `json:"-"`

	// HandedNotes is TRANSIENT: notes the first segment's box still held unread
	// when the launch began ([LaunchRequest.HandedNotes]), filed WAITING by the
	// opening after the brief's notes and ahead of any a person files once the
	// record exists. A note already held is left as is. Never persisted as itself.
	HandedNotes []RunNote `json:"-"`

	// Prior is TRANSIENT: on the row a relaunch's [PendingStore.BeginLaunch]
	// answers, the box the row named before the reset moved it to the new job (id,
	// cell, harness installed for, row status). Zero on a created row or one whose
	// previous job never got a box.
	//
	// HANDED TO THE LAUNCH, not kept on the row, because only the launch knows what
	// becomes of that box: continued in, when the new job runs the same harness in
	// the same cell, or reclaimed through its own cell before a fresh one is
	// acquired ([Coordinator.Launch]). Until the launch attaches its own box the
	// row keeps naming this one with its cell, so a node dying between leaves a
	// box recovery can reclaim.
	Prior PriorBox `json:"-"`

	// OpenedAfter is TRANSIENT: on the row [PendingStore.BeginLaunch] answers, the
	// place of the last bridged call the log kept from before this job
	// ([BridgeLog.LastSeq]), zero where the round starts its log afresh.
	OpenedAfter int `json:"-"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`

	// Extra is every key on the stored record THIS BUILD DOES NOT KNOW, kept
	// verbatim and written back on every re-encode.
	//
	// THE ROW IS READ, MODIFIED AND WRITTEN WHOLE by every node touching it (each
	// status flip, claim and release is a compare-and-swap of the whole value),
	// and a mixed fleet has two builds doing so. Decoded into this struct alone,
	// an older build's first claim or release silently deleted what a newer build
	// added (the work item, the audience). Filled by the decode in [CoordStore]
	// and re-emitted by its encode, never by a caller; a known field wins over a
	// same-named key here.
	Extra map[string]json.RawMessage `json:"-"`
}

// Paused reports whether a pause was RECORDED for this run's box: the record's
// fact, not "is a box being held" ([PendingRun.HeldSince], which callers acting
// on a held box read). They differ on one row: a parked run whose pause reached
// the box and whose stamp never reached the row.
func (r PendingRun) Paused() bool { return !r.PausedAt.IsZero() }

// HasBox reports whether a box exists for this run at all.
func (r PendingRun) HasBox() bool { return r.SandboxID != "" }

// BridgeLog is what a run made through the MCP bridge: its bridged tool log,
// kept in the coordination store BESIDE the run's record ([coord.SandboxRuns]).
//
// DURABLE, as it has nowhere else to live: without it a resumed run's reviewer sees
// "it called nothing". BESIDE THE RECORD: two hundred calls weigh about half a
// megabyte, which every flip and box git request would move. THE LOG HOLDS THE
// CONDITION ITS APPENDS ARE TAKEN ON ([BridgeLog.Launch]), since the store writes no
// two keys at once: an append decides on the log in one compare-and-swap and the
// launch opening a job moves it ([PendingStore.BeginLaunch]). A WIRE FORMAT; unknown
// keys ride [BridgeLog.Extra].
type BridgeLog struct {
	// Launch is the job whose calls the log takes: the job the row held when its
	// launch opened the log. An append naming another job is listed, marked late,
	// only where that job worked in the log's round or, a record, in an earlier
	// one, and is dropped otherwise ([PendingStore.AppendBridgeCall],
	// [BridgeCall.Late]); a reader holding another job's row reads none of its
	// calls here ([PendingStore.BridgeLog]).
	Launch string `json:"launch"`

	// Round is the executor round the log holds the calls of ([PendingRun.Round]):
	// opening the log to a job of the same round keeps them, another round starts
	// it with only the calls taken past the previous round's collection read.
	Round string `json:"round"`

	// CutFor names the job whose answered question the log's cut was taken for
	// ([PendingRun.AnsweredJob]); empty for a round no answer has cut.
	CutFor string `json:"cut_for,omitempty"`

	// Calls are the round's calls in the order they finished, bounded by
	// [MaxBridgeCalls]. Empty before any call.
	Calls []BridgeCall `json:"calls,omitempty"`

	// Elided counts the round's calls the list does not hold (dropped from the
	// middle by the count bound), so a reader can tell a short run from a long
	// one whose middle was cut: a silently skipping log lies about what the run
	// did. A call in flight is not one: it is [BridgeLog.InFlight]'s until its
	// update lists it.
	Elided int `json:"elided,omitempty"`

	// LateElided is how many dropped calls were LATE ([BridgeCall.Late]) or ones
	// their harness stopped waiting for ([BridgeCall.Abandoned]). A dropped call
	// keeps only its identity ([BridgeLog.ElidedCalls]), not its mark, and no save
	// point may hold such a call, so every continuation of the round is told this
	// count under the late calls' words, never as calls its session made after
	// the save (internal/agent/runner's calledAfter, internal/agent/prompts'
	// AfterSave.LateDropped). The middle gives up such a call only once no
	// ordinary call is left there ([appendBounded]), so it is zero unless the
	// middle holds nothing else. THE ROUND'S: kept by every opening carrying the
	// round on, absent from a new round's. ADDITIVE ON THE WIRE: an older build
	// carries it in Extra (wirekeys.go) and counts no late call it drops.
	LateElided int `json:"late_elided,omitempty"`

	// ElidedCalls is WHAT those calls were, by identity: each tool and argument
	// digest with its count and failures ([ElidedCall]), held to
	// [MaxElidedIdentityBytes]. A resumed segment seeds its call log with it so later
	// calls derive the operation id the run's own process would, and a reader counts a
	// dropped reply by it ([BridgeLog.ElisionKnown]). READ THROUGH
	// [BridgeLog.Unlisted] (an uncovered count means calls of unknown tool). THE
	// ROUND'S: kept by openings carrying the round on, folded into
	// [BridgeLog.PriorCalls] when a new round starts.
	ElidedCalls []ElidedCall `json:"elided_calls,omitempty"`

	// AbsorbedCalls is the calls the run's call log took in from its bridged calls'
	// OWN WORK, by identity ([ElidedCall], Failed unset): a delegated worker's calls
	// under `delegate` (internal/agent/turnctx's CallLog), none in this log. Held to
	// [MaxAbsorbedIdentityBytes]. WHAT A RESUMED SEGMENT SEEDS ITS CALL LOG WITH,
	// since a call after a worker's differing one must count it or derive an earlier
	// call's operation id; not a call of the round, so no replied-reader counts it.
	// Read through [BridgeLog.Absorbed]; a new round's is the previous less its read
	// ([BridgeLog.carried]).
	AbsorbedCalls []ElidedCall `json:"absorbed_calls,omitempty"`

	// AnsweredThrough is the place ([BridgeCall.Seq]) of the last call the round made
	// before a person's answer carried it on, zero where none has. Written by the first
	// opening after an answer ([PendingRun.AnsweredJob], [BridgeLog.CutFor]) in the
	// same write that keeps those calls and opens the log to the new job, so no
	// asking-job call lands after the cut; kept by a relaunch carrying the round on
	// after a loss, zeroed by a new round. A PLACE, NOT A SEARCH: the collection
	// reports only a submission made after it, and "after the last `ask_and_wait`"
	// would move with whatever the log held (an ask dropped from the middle,
	// [MaxBridgeCalls], would leave an earlier submission standing).
	AnsweredThrough int `json:"answered_through,omitempty"`

	// PriorCalls is every tool the turn's EARLIER rounds called through the bridge and
	// that succeeded, by name once ([BridgeLog.ElidedCalls] included), and
	// PriorCallsElided that some are unaccounted for by name (no identity kept, tool
	// not kept, or past [MaxPriorCalls]). Folded in by the SAME compare-and-swap that
	// opens the log to a new round ([CoordStore.openLog]), so no crash loses a round's
	// calls, letting the run's ending, for a loss or stop no turn frame reaches, know
	// whether an earlier round delivered the reply ([turn.Answered]). ON THE LOG,
	// NEVER THE RECORD, which every reader reads whole; its one reader is the ending
	// ([RunCalls]).
	PriorCalls       []string `json:"prior_calls,omitempty"`
	PriorCallsElided bool     `json:"prior_calls_elided,omitempty"`

	// Committed is every kind of change the TURN'S calls committed up to this
	// round's, each once, in the order the logs took the calls: its jobs', late
	// ([BridgeCall.Late]) and abandoned ([BridgeCall.Abandoned]) alike, folded in
	// by the one append taking every call ([BridgeLog.appendCall]) before the
	// bound can drop it, and kept whole when calls are condensed, so the
	// collection settles the outcome by every kind the log holds
	// ([BridgeLog.Changes]). KEPT BY EVERY OPENING, a new round's whole
	// ([BridgeLog.carried]): the collection between rounds folded only what it
	// read, and kinds are a set. A late call of an earlier round folds its kinds
	// in too ([takeEarlier]), so only a kind folded in after the turn's last
	// collection read is in no outcome.
	Committed []string `json:"committed,omitempty"`

	// Wrote is every record the TURN'S calls created or wrote, each once, in
	// the order the logs took them, named as a wake about it is
	// ([types.WrittenRecord]): what a busy seat's router hands a wake about to
	// the running run (internal/engine, ADR-0167). Folded in by the append
	// taking a call's record ([BridgeAppend.Wrote]; late, abandoned and an
	// earlier round's alike, as each ran), and KEPT BY EVERY OPENING as
	// [BridgeLog.Committed] is, so it lives as long as the log through a park,
	// a relaunch and a takeover. HERE, the record keeping only their keys
	// ([PendingRun.WroteKeys]): the append alone knows what a call wrote. Held to
	// [types.MaxWrittenRecords]; WroteMore says a record was refused for want of
	// room, which the append that first refused one says
	// ([CoordStore.AppendBridgeCall]). Read through [PendingStore.TurnLog].
	Wrote     []types.WrittenRecord `json:"wrote,omitempty"`
	WroteMore bool                  `json:"wrote_more,omitempty"`

	// InFlight are the round's calls that have BEGUN AND NOT FINISHED: each entry,
	// written by the bridge before its tool runs ([AppendBegin]), carries the call's
	// identity beside the harness's id (no part of it). The one update takes it out and
	// lists the finished call in the same write ([AppendRecord]); withdrawal removes it.
	// SO NO CALL RUNS OFF THE RECORD: a call whose entry was not taken is not run, and one
	// whose update the store refuses keeps its entry, outcome unknown. Numbering and
	// seeding readers count them ([BridgeLog.LastSeq], internal/agent/runner's
	// seedCalls). Held to [MaxCallsInFlight] within [MaxCallInFlightBytes]. THE ROUND'S:
	// a remaining entry makes a new round's fold incomplete ([foldPriorCalls]).
	InFlight []BridgeCall `json:"in_flight,omitempty"`

	// Extra is every key of the log this build does not know, kept verbatim and
	// written back by every write of it (wirekeys.go). Filled by the decode,
	// never by a caller.
	Extra map[string]json.RawMessage `json:"-"`
}

// priorFor is what an opening of the log to a job of round keeps of the turn's
// earlier rounds ([BridgeLog.PriorCalls]): the fold as it stands for the same
// round, and for another the fold with this log's own calls taken in.
func (l BridgeLog) priorFor(round string) ([]string, bool) {
	if l.Round == round {
		return l.PriorCalls, l.PriorCallsElided
	}
	return foldPriorCalls(l)
}

// OwedCall is a call a person approved, as its round's log keeps it
// ([HeldCall.Call]), which that round's log is owed ([PendingRun.OwedCall]).
//
// ON THE ROW, BECAUSE THE LOG IS NOT: a held call's result is recorded on the row
// and the call appended to the log after, two keys the store cannot write in one
// step ([PendingStore.RecordHeldResult]). Where the append never landed, the reset
// opening the next job carries the call here for the following opening to append,
// and a failed opening is retried by the next relaunch's reset. Dropped by a reset
// replacing a job that launched. The json tags are a WIRE FORMAT.
type OwedCall struct {
	// Round is the round the call was made in ([PendingRun.Round]); only that
	// round's log is owed it.
	Round string `json:"round"`

	// Call is the call as the log keeps it, named by its held call
	// ([BridgeCall.HeldID]), which keeps it in the log once.
	Call BridgeCall `json:"call"`

	// Extra is every key of the section this build does not know (wirekeys.go).
	Extra map[string]json.RawMessage `json:"-"`
}

// withHeld is l with the person-approved call in run's job that the log is still
// owed: made and kept on the row with its result ([HeldCall.Call]) where the
// append to the log has not landed. It is a call of the round all the same, read
// by every reader of the job's calls: an ending's account
// ([PendingStore.TurnLog]) and a resume's ([ResumeRequest.Bridged]). Never
// another job's: the relaunching reset carries the held call off the row
// ([PendingRun.OwedCall]).
func (l BridgeLog) withHeld(run PendingRun) BridgeLog {
	if h := run.HeldCall; h != nil && h.Made() && h.Call != nil {
		l, _ = l.withApproved(run.Round, *h.Call)
	}
	return l
}

// withApproved is l with call, the call a person approved in round
// ([BridgeCall.Approved]), appended where l is that round's log and does not
// hold it yet ([BridgeCall.HeldID]), and whether it was: placed after every call
// l holds, as a bridge append is, held to the log's bounds as every call is
// ([BridgeLog.appendCall]), arguments and digest withheld.
//
// AND NUMBERED AS THE LOG WILL NUMBER IT ([BridgeCall.Appended]): every reader copy
// ([BridgeLog.withHeld], [PendingStore.TurnLog]'s) and both appending writes go
// through this one append, so each places it after every call the log has taken. A
// SAVE POINT PLACED OFF SUCH A COPY COUNTS THE CALL though the box's session may never
// have been answered it; no continuation is told less (a stop ends its turn, and a
// loss's round is read off the job's own log, [PendingStore.BridgeLog]).
func (l BridgeLog) withApproved(round string, call BridgeCall) (BridgeLog, bool) {
	if l.Round != round || slices.ContainsFunc(l.Calls, func(c BridgeCall) bool {
		return c.HeldID == call.HeldID
	}) {
		return l, false
	}
	call.Seq = l.LastSeq() + 1
	// A LIST OF ITS OWN, since l is a copy whose list its caller still holds.
	l.Calls = slices.Clone(l.Calls)
	l.appendCall(call, nil)
	return l, true
}

// LastSeq is the place of the last bridged call the log records
// ([BridgeCall.Seq]), zero for none: where the next opening of the run's job
// numbers from (the job's own after a recovery, or a job continuing one that
// asked a person, whose calls the log keeps, [PendingRun.Continues]).
//
// THE HIGHEST, not the last listed: calls are listed as they FINISH. AND A CALL IN
// FLIGHT ([BridgeLog.InFlight]) IS COUNTED wherever it runs, so a node taking the
// seat over while the old holder's call runs numbers after it, as do an answered
// round's cut and the person-approved call.
func (l BridgeLog) LastSeq() int {
	last := 0
	for _, call := range l.Calls {
		last = max(last, call.Seq)
	}
	for _, call := range l.InFlight {
		last = max(last, call.Seq)
	}
	return last
}

// Appends is how many calls the round's log has taken: listed plus counted
// without listing ([BridgeLog.Elided]). One writer adds to either, a call at a
// time ([BridgeLog.appendCall]), and a new round's log starts from none, taking
// carried calls the same way ([BridgeLog.carried]); so this is the place of the
// last call taken ([BridgeCall.Appended]) and the save point of a session placed
// by this read ([SessionCapture.Through]).
//
// A COUNT, NEVER A SEARCH: unlike [BridgeLog.LastSeq] it counts calls dropped
// from the middle ([MaxBridgeCalls]) and calls an older build appended without
// numbering.
func (l BridgeLog) Appends() int { return len(l.Calls) + l.Elided }

// LogRead is one read of a round's log by the collection closing the round's job
// ([ResumeRequest.BridgedRead]): the job the log was open to ([BridgeLog.Launch]),
// calls taken ([BridgeLog.Appends]) and calls absorbed ([BridgeLog.Absorbed]).
// Every brief of a launch built from it can tell the calls through Took, and
// the collection seeded Absorbed, so the launch's opening marks late, or carries
// into a new round, only what the log took after it, while the log is still open
// to that job ([PendingRun.BriefRead]). In memory only.
type LogRead struct {
	Launch   string
	Took     int
	Absorbed []ElidedCall
}

// Read is what a read of l as it stands saw ([LogRead]).
func (l BridgeLog) Read() LogRead {
	return LogRead{Launch: l.Launch, Took: l.Appends(), Absorbed: l.Absorbed()}
}

// Changes is every kind of change the turn's calls committed up to this round's,
// each once, whether or not the log still lists the committing call (late and
// abandoned included): the log's own record ([BridgeLog.Committed]) plus any
// listed call carries that it lacks (an older build's append).
func (l BridgeLog) Changes() []string {
	out := slices.Clone(l.Committed)
	for _, call := range l.Calls {
		out = committedOnce(out, call.Committed)
	}
	return out
}

// Told is the tellings among tellings ([RunSteer.Tellings]) of the log's own
// round ONLY: a telling names a place in its round's log alone, and one an older
// build's new-round reset left on the row names no call of this log. None for a
// log naming no round.
func (l BridgeLog) Told(tellings []CallTelling) []CallTelling {
	var out []CallTelling
	for _, t := range tellings {
		if l.Round != "" && t.Round == l.Round {
			out = append(out, t)
		}
	}
	return out
}

// committedOnce is have with every kind of more it lacks, in first-seen order.
func committedOnce(have, more []string) []string {
	for _, c := range more {
		if c != "" && !slices.Contains(have, c) {
			have = append(have, c)
		}
	}
	return have
}

// HeldSince is when this run's box started being held paused with nothing
// driving it, and whether it is held at all.
//
// THE ROW DESCRIBES THE BOX; THE STAMP ONLY DATES IT. [Coordinator.collect] pauses the
// box THEN records the instant, a second warn-only write that can fail alone, so a
// run PARKED ON A QUESTION can hold a paused box with no stamp, and reading that as
// "not paused" made such a box invisible to the reaper ([pauseExpired]) for good.
// THE FALLBACK IS THE ROW'S OWN LAST WRITE, on a parked row the park itself
// ([Coordinator.park]), landing just after the pause and so never EARLIER than it:
// held a moment longer rather than reclaimed from under a person typing. AN ANSWERED
// RUN TAKES THE SAME FALLBACK. A RUN THE ENGINE IS DRIVING TAKES NONE: its pauses last
// one dispatch, so a missing stamp means a live box, not a held one.
func (r PendingRun) HeldSince() (time.Time, bool) {
	if !r.HasBox() {
		return time.Time{}, false
	}
	if r.Paused() {
		return r.PausedAt, true
	}
	if (!slices.Contains(Awaiting, r.Status) && r.Status != StatusAnswered) || r.UpdatedAt.IsZero() {
		return time.Time{}, false
	}
	return r.UpdatedAt, true
}

// PastDeadline reports whether the run's turn has passed its wall-clock cap at
// now ([PendingRun.Deadline]); never for a turn with none.
func (r PendingRun) PastDeadline(now time.Time) bool {
	return !r.Deadline.IsZero() && !now.Before(r.Deadline)
}

// PendingStore is the persistence surface for detached runs.
//
// ONE IMPLEMENTATION, [CoordStore], on the fleet's coordination store, whose
// record operations are certified on both coordination backends. It is an
// interface because the coordinator's hardest properties (at-most-once claim,
// scoped release, epoch fencing) belong to the store's conditional writes, which
// the sandboxtest suite certifies apart from any caller, and because a
// coordinator case can stage a store refusing one write only by wrapping it.
type PendingStore interface {
	// BeginLaunch opens a launch on this turn's row: it creates the row or RESETS an
	// existing one to launching (clearing the previous job's parked state, question and
	// charge record, keeping identity and box), minting a new [PendingRun.LaunchID] and
	// answering the row as it opened the job: the bridge session records calls under
	// that name, so it must exist before the box acts. CREATE-OR-RESET because a turn's
	// second launch has the same turn id but is another job; left alone the row kept the
	// tail's `resumed` status and the settle tore down the new job's box. OWNED BY THE
	// LEASE THAT LAUNCHED IT (the fence stamps Owner and OwnerEpoch); a refused reset is
	// an error ([ErrSeatNotHeld] first, then [ErrRunEnding]), a failure past a write that
	// may have named the job wraps [UnopenedLaunchError]. Code holds are UNIONED; grants
	// are the job's own; notes, base and leads are KEPT, the brief's notes recorded READ BY THE JOB in this write.
	BeginLaunch(ctx context.Context, run PendingRun, fence Fence) (PendingRun, error)

	Get(ctx context.Context, turnID string) (PendingRun, bool, error)

	// RowCrossing is how long a change of a run's row or a part beside it, at its
	// bound ([RowBytes]), takes to cross this node's connection to the store (the
	// read its decision rests on, and the write, for every time the store may
	// send it): the store's figure ([coord.SandboxRuns.ValueCrossing]), thirteen
	// minutes at the slowest pace the engine allows, none in the store's own
	// process. A caller's budget for such a change adds its own reason
	// ([RowBudget]).
	RowCrossing() time.Duration

	// RowReadCrossing is how long a READ of a run's row at its bound takes to
	// cross this node's connection to the store, the store's reply alone
	// ([coord.SandboxRuns.ValueReadCrossing]): four minutes at the slowest pace
	// the engine allows, none in the store's own process. A caller's budget for
	// such a read adds its own reason ([RowReadBudget]).
	RowReadCrossing() time.Duration

	// StampCapture records one capture of the turn's harness session on its row
	// ([PendingRun.Capture], [PendingRun.Kept]), answering whether it was taken,
	// by [stampOnto]: THE PLACE ONLY GROWS, which reserves it, so two reads of one
	// run never take the same; a capture at the newest's place only as the same
	// take moving on; a LANDED one newer than the kept one is kept. A stamp the row
	// already holds in any capture section ([holdsStamp]) is answered taken with
	// nothing written, so an asker whose answer was lost learns the place is its
	// own. On a row in any state, its ending decided included, since a stop's
	// capture is taken after the decision; a gone row takes nothing. A stamp past
	// its bounds is refused before any write ([ErrCaptureUnbounded]): a row that
	// could not carry it would take no later write adding a byte.
	StampCapture(ctx context.Context, turnID string, capture SessionCapture) (bool, error)

	// SettleBegun is [PendingStore.StampCapture] held to one more condition,
	// decided on the row as it stands: the newest capture, or the save under way
	// ([PendingRun.Saving]), is the settled capture's own take, still BEGUN. It is
	// the write of a node settling another's capture from the executor log
	// ([Coordinator.settleGone]) from a copy where the take stood begun; where the
	// row has moved on (the taker's outcome, an unresolved record, a newer take)
	// nothing is written and false answered. Otherwise as StampCapture.
	SettleBegun(ctx context.Context, turnID string, capture SessionCapture) (bool, error)

	// ReserveSave reserves the place of save, the capture of a job still working,
	// on its row as the newest capture and save under way ([PendingRun.Saving])
	// at once, answering whether it was taken: ONLY WHILE ITS JOB RUNS UNDECIDED
	// ([licensesSave]), decided on the row as it stands with this node's clock, at
	// the next place after the newest. Every final take writes its decision before
	// reading for its place, so A FINAL TAKE NEVER LOSES ITS PLACE TO A SAVE.
	// Refused, nothing is written. A reservation the row holds is answered taken;
	// a save that is not one, or past its bounds, is an error before any write
	// ([reservable]); a gone row takes nothing. The package doc's "A save's
	// license" says why.
	ReserveSave(ctx context.Context, turnID string, save SessionCapture) (bool, error)

	// StandBy records capture's take (a read whose place another read of the same
	// job holds, begun at that place) as the read STANDING BY for the newest
	// capture ([SessionCapture.StandbyAt]), answering whether it stands: written
	// only where that capture is at capture's place, of the same job
	// ([SessionCapture.Launch]), another take's, NOT LANDED (begun, refused or
	// unresolved), with no standby yet. Decided on the row as it stands, so of two
	// reads finding one place taken one stands by, and none stands by for a capture
	// that landed, a newer one or another job's: false, nothing written, and the
	// read stands down, the session the other's to keep. A standby the row holds
	// (this one, asked again) is answered standing; a gone row takes nothing, no
	// error; a stamp past its bounds is refused as [PendingStore.StampCapture]
	// refuses.
	StandBy(ctx context.Context, turnID string, capture SessionCapture) (bool, error)

	// TakeOver is [PendingStore.StampCapture] of capture (the take of the read
	// standing by as standby names it, begun, at a place after the capture it
	// stood by for) held to one more condition on the row as it stands: the
	// newest capture is the one standby stands by for, and it did NOT LAND. Where
	// it landed the standby has nothing to keep, and where the row moved on
	// (standby withdrawn, newer capture, another job's) it keeps nothing: false,
	// nothing written. So one read's upload of a job's session is ever the one
	// kept, and a landed capture is never followed by another of the same session.
	// A stamp the row holds is answered taken; a gone row takes nothing, no error;
	// a stamp past its bounds is refused as there.
	TakeOver(ctx context.Context, turnID string, standby, capture SessionCapture) (bool, error)

	// StandDown withdraws standby (the read standing by for the newest capture, as
	// it stood by, [SessionCapture.standbyOf]), answering whether it was there:
	// written only where the newest capture is at standby's place, of its job, with
	// standby standing by for it, so no read withdraws another's and a moved-on
	// capture is left as it stands. What a standby writes when it cannot keep what
	// it took, and a wait when its taker is gone, so no turn's end waits for a read
	// that keeps nothing. A gone row takes nothing, no error.
	StandDown(ctx context.Context, turnID string, standby SessionCapture) (bool, error)

	// StampSummary names on the row the summary a box read of its job kept
	// ([PendingRun.Summary]), on a row in any state, where it is newer than the
	// one the row names ([SummaryStamp.newerThan]), and answers whether the row
	// names it now. A gone row takes nothing, no error.
	StampSummary(ctx context.Context, turnID string, stamp SummaryStamp) (bool, error)

	// RetireSessions adds sessions, spent by loss, to [PendingRun.SpentSessions],
	// on a row in any state: a fact about what the box did. Asking again changes
	// nothing; a gone row is no error.
	RetireSessions(ctx context.Context, turnID string, loss executorkeep.Loss, sessions []string) error

	// OweEnd records on the row the end its turn owes the executor log
	// ([PendingRun.OwedEnd]), answering the row as it stands after the write and
	// whether this call wrote it.
	//
	// DECIDED ONCE: a row already owing an end keeps and answers it, so a second
	// path ending the turn records the first one's account; a row whose end is
	// recorded ([PendingRun.EndRecorded]) takes none. ON A ROW IN ANY STATE, its
	// ending decided included: a loss's or stop's end is decided by that ending's
	// own steps. A gone row takes nothing, no error.
	OweEnd(ctx context.Context, turnID string, ending json.RawMessage) (PendingRun, bool, error)

	// ClearEnd removes the end a row owes once the executor log has taken it
	// ([PendingRun.OwedEnd]), only where the row still owes exactly owed, so a
	// clear never takes an end the log has not seen, and marks the end recorded in
	// the same write ([PendingRun.EndRecorded]), which no later OweEnd overwrites.
	// A gone row, or one owing another, is no error and answers false.
	ClearEnd(ctx context.Context, turnID string, owed json.RawMessage) (bool, error)

	// ClaimForResume atomically flips a run to resumed, when it holds the tail's launch in
	// one of the tail's statuses; reports the row IFF THIS CALL WON (the at-most-once guard,
	// [Tail]), carrying ClaimedFrom so a failed dispatch can put it back. TAKEN UNDER THE
	// CLAIMANT'S LEASE: refused where a newer lease owns the run, and a fenced claim stamps
	// [PendingRun.Owner] and [PendingRun.OwnerEpoch], so the seat's next holder fences the
	// CLAIMANT out. SUPERSEDED, NOT OUTRANKED, as a take ([PendingStore.TakeAnswer]): a ZERO
	// fence claims only a row no lease ever owned.
	//
	// NOT WHILE ITS RESUME IS HELD ([PendingRun.Held], at the store's instant): the claim
	// IS the resume, and an announcement from before the hold can still sit on the control
	// topic; claimed, it re-collected the job and woke the box the hold keeps asleep. A
	// lifted or expired hold ([PendingStore.LiftHold]) leaves the row claimable.
	ClaimForResume(ctx context.Context, turnID string, tail Tail, fence Fence) (PendingRun, bool, error)

	// ReleaseClaim hands a claimed run back to the status it was claimed from,
	// reporting whether this call did; only while the claim still stands
	// ([Release]). Recorded in the same write: the claim's charge, which no
	// release clears ([PendingRun.Charged]), and the harness's end
	// ([Release.HarnessEnd]), a recorded cut-off standing as
	// [PendingStore.MarkHarnessEnd] keeps it. A recorded answer the claim's turn
	// took is the run's again ([RecordedAnswer.TakenAt] cleared); a claim whose
	// answer an ending let go is not released to [StatusAnswered]. A release
	// holding the resume ([Release.Hold]) records the completion as owed in the
	// same write ([LaunchRecord.RefusalOwed]). False is not an error; a release to
	// a status outside [Claimable], or a hold whose condition exceeds
	// [MaxOwedConditionBytes], is. See the package doc, "Releasing a claim".
	ReleaseClaim(ctx context.Context, turnID string, release Release) (bool, error)

	// LiftHold clears the hold a release put on a run's resume
	// ([PendingRun.HeldFor]) where it is held for cond and the hold is still
	// number hold, the one the caller read ([PendingRun.Holds]), reporting whether
	// it was: the condition was signalled and the waiter announces the completion
	// again on its schedule. FALSE IS NOT AN ERROR: held for something else, by a
	// newer hold, not at all, or gone.
	LiftHold(ctx context.Context, turnID string, cond Condition, hold uint64) (bool, error)

	// OweRefusal records on job launch's record that the seat's holder
	// acknowledged a refused delivery of its completion and owes its re-fire
	// ([LaunchRecord.RefusalOwed]), reporting whether this call wrote it: one
	// write, only where the row holds that job with its completion unsettled, no
	// ending decided and none owed yet, so the first refusal's condition and
	// instant stand. Under the holder's fence ([ErrSeatNotHeld]). A refusal with
	// no condition, one past [MaxOwedConditionBytes], or no instant is an error
	// writing nothing. False is not an error: nothing is owed.
	OweRefusal(ctx context.Context, turnID, launch string, owed OwedRefusal, fence Fence) (bool, error)

	// MarkAwaiting parks a run on a question, freeing the seat. A question with
	// no [Clarification.AskedAt] is refused: it anchors every answer
	// ([PendingRun.AskedAt]). The seats it is put to are held to
	// [MaxAudienceHandles], first kept in resolution order: a `team` audience's
	// lead then its people in chart order, the fallback's lead chain nearest
	// first. The park SETTLES the job's completion: the refusal its record owed
	// ([LaunchRecord.RefusalOwed]) is cleared in the same write, since the
	// question's answer, never that completion, carries the run on.
	MarkAwaiting(ctx context.Context, turnID string, q Clarification) error

	// ClaimOwnership takes the run for a node, reporting whether it won. A run
	// whose epoch is already higher is not stolen.
	ClaimOwnership(ctx context.Context, turnID, owner string, epoch int64) (bool, error)

	// SetStatus moves a run between the live states, fenced on the epoch. Ending a
	// run is not a status; see DecideEnding.
	SetStatus(ctx context.Context, turnID, status string, fence Fence) error

	// DecideEnding records that a run is ENDING on the terms of d, while its license
	// holds ([License]), and returns the row with its [PendingRun.Ending], this call's or
	// the one already there ([RecordedEnding] says why). ONE DECISION PER RUN: a second
	// decider is handed the first to finish, and from then on the row refuses every write
	// but the ending's own steps ([ErrRunEnding]). A STOP ENDS ONLY A JOB STILL OUT IN A
	// BOX: a stop ([Decision.Stop]) on a job a collection already read
	// ([LaunchRecord.Collected]) is refused in the deciding write, since a listing cannot
	// see a claim that collected it and gave it back in between. FALSE IS NOT AN ERROR (run
	// gone, newer lease, license not entitling). An error may or may not have landed; the
	// caller decides again and finds it recorded.
	DecideEnding(ctx context.Context, turnID string, d Decision) (PendingRun, bool, error)

	// CollectStop records, as a STEP of the stop ending named
	// ([StopCollection.Ending]), what that ending read of the run's box before
	// reclaiming it ([RunStop.Collected]) and whether the spend is charged and the
	// launch's record published, in ONE WRITE, returning the row IFF THIS CALL
	// WROTE IT.
	//
	// ONE WRITE because the three are one fact about one read: an attempt finishing
	// after a crash reads the row to learn whether the box was read, and "read" without
	// "charged" would leave the charge to nobody. The charge and publish still precede the
	// write ([Release.Charged]), so a crash can only charge twice, never lose it. ONLY ON
	// A ROW WHOSE DECIDED ENDING IS THE ONE NAMED with a stop not yet collected; a second
	// collection is refused. FALSE IS NOT AN ERROR.
	CollectStop(ctx context.Context, turnID string, c StopCollection) (PendingRun, bool, error)

	// Finish deletes the record of a run whose ending is decided (the ending named,
	// and only once it owes the seat nothing) and hands back the deleted record.
	//
	// The ending's other steps come FIRST ([Coordinator.finishEnding]: box reclaimed, reply
	// handed back, announcement), since nothing reads a deleted row. FALSE IS NOT AN ERROR
	// (gone, or not that ending's). NOT DELETED while the row owes the seat copies
	// ([ErrHandBackOwed]), nor while it holds a PERSON'S REPLY NO TURN TOOK
	// ([ErrAnswerOwed]; for an unused reply, any reply), whose delivery was spent when
	// recorded, so the caller lets it go ([PendingStore.OweHandBack]) and finishes again;
	// NOR while it owes the executor log its turn's END ([ErrEndOwed]), the end's only
	// record until [PendingStore.ClearEnd].
	Finish(ctx context.Context, turnID, ending string) (PendingRun, bool, error)

	// ExpirePause flips a run parked on a clarification to reseed AND clears its
	// box record, reporting whether THIS call won.
	//
	// The pause reaper's authority, a conditional flip, not a plain SetStatus: the reaper
	// decides from a snapshot seconds old and the answer may have arrived since, so
	// killing the box before this returns true would destroy it under a resume. Only from
	// StatusAwaiting or [StatusAnswered]. AN ANSWERED RUN EXPIRES TOO (its box is held for
	// as open-ended a wait): it keeps status and answer, owed its resume, which re-seeds
	// from the branch, and the answer's From becomes [StatusReseed]. It clears the box IN
	// THE SAME WRITE: between two writes a `reseed` row naming its box tells an arriving
	// answer the checkout is live moments before it is destroyed.
	ExpirePause(ctx context.Context, turnID string) (bool, error)

	// MarkLost records on a RUNNING job the verdict that it was lost with its host
	// ([PendingRun.Lost]), reporting whether the row carries a verdict for the job
	// now, from this call or an earlier one, which stands.
	//
	// PINNED TO THE JOB the caller judged (launch) and to the node whose host its
	// box is on (loss.Node, the row's [PendingRun.LaunchNode]): after a relaunch,
	// claim of its completion, park or ending moved the row on, a verdict about a
	// job the row no longer holds, or a box on another node, is about nothing. False with no error is
	// that, or a gone row. A zero loss.At is stamped by the writing store handle's
	// clock.
	MarkLost(ctx context.Context, turnID, launch string, loss JobLoss) (bool, error)

	// MarkClaimLost records on a job claimed for its collection the verdict that it
	// was lost with the host its box is on ([PendingRun.Lost],
	// [LossLaunchNodeStoreSilent]), so the claim carries its round on and the
	// relaunch counts the loss ([PendingRun.RoundLosses]). Reports whether the row
	// carries a verdict for the job now. Pinned to the claim the caller holds: the
	// job, [StatusResumed] under fence, and the node whose host its box is on. A
	// running job is the waiter's to judge ([PendingStore.MarkLost]); any
	// other claim, or a gone row, is false with no error. A zero loss.At is the
	// store's clock.
	MarkClaimLost(ctx context.Context, turnID, launch string, loss JobLoss, fence Fence) (bool, error)

	// AttachSandbox records the box and command a run is using. An attach naming a command
	// records the job STARTED ([LaunchRecord.Started]) in the same write, which nothing
	// unsays. What the box was handed ([BoxRef.Handed]) is written beside it afterwards
	// ([PendingStore.Handed]); failing that leaves the job with no set, so captures are
	// refused. The brief's own leads ([BoxRef.Leads]) are taken into the leads part FIRST
	// ([PendingStore.Leads]), then marked ([LaunchRecord.LeadMarks]); a part of another run is
	// replaced only where the record names this run, else the attach is refused.
	//
	// A REFUSED ATTACH IS AN ERROR, as a refused relaunch ([PendingStore.BeginLaunch]): a
	// newer lease owns the run ([ErrSeatNotHeld]) or its ending is decided ([ErrRunEnding]).
	// The launch must abandon the box rather than run a job the row does not name; a seat
	// that moved during setup ([Coordinator.RecoverSeat]) reaches this as an ending.
	AttachSandbox(ctx context.Context, turnID string, box BoxRef, fence Fence) error

	// MarkBoxPaused and ReleaseBox are the two halves of the box record: paused_at
	// set means a paused box is held, cleared means the box is gone.
	MarkBoxPaused(ctx context.Context, turnID string, at time.Time) error
	ReleaseBox(ctx context.Context, turnID string) error

	// MarkSuspended writes the parked turn's state AND flips launching to running in ONE
	// compare-and-swap: resumable and pollable at the same instant, since two writes expose
	// a `running` row with no conversation on which the poll fires ([StatusLaunching]).
	// FALSE IS NOT AN ERROR: the run is no longer launching and is left alone (re-arming a
	// claimed tail would hand a redelivered completion a second resume), so the caller,
	// holding a conversation with nowhere to put it, must fail the run rather than keep a
	// box. A RUN WHOSE ENDING IS DECIDED IS REFUSED WITH [ErrRunEnding], since a stop
	// ended the launch under it ([Coordinator.StopSeat]). A STATE PAST ITS SHARE OF THE
	// ROW ([ExecuteStateBytes]) IS REFUSED with [ErrStateTooLarge] before any write: the
	// park condenses to fit (internal/agent/execstate's EncodeWithin).
	MarkSuspended(ctx context.Context, turnID string, s Suspension) (bool, error)

	// AppendBridgeCall makes one step of a bridged call onto the run's log
	// ([BridgeLog], [BridgeCall]): the entry IN FLIGHT before the tool runs
	// ([AppendBegin], the call running only once this answered true), its ONE UPDATE as it
	// finishes ([AppendRecord]), or a withdrawal ([AppendWithdraw]). NO FENCE: a log append
	// is no ownership decision, and refusing a call that ran would lose true evidence.
	// PINNED TO ITS JOB AND LISTED BY ITS ROUND: once a launch replaced the job, an
	// in-flight call is a LATE CALL (listed, marked, spend written nowhere, answered beside
	// [ErrJobReplaced], [BridgeLog.take], [takeEarlier]). Held to [BridgeLogBytes]; ONCE
	// however often asked (a re-asked record dropped from the middle over-counts, safely).
	// A call finishing after its job's claim is logged but NOT PAID ([ResumeRequest.Engine]):
	// the task ends short of the turn's cost, never past it. FALSE IS NOT AN ERROR (row
	// gone, round over); TRUE BESIDE AN ERROR is the spend's ([BridgeAppend.Spent]).
	AppendBridgeCall(ctx context.Context, turnID string, a BridgeAppend) (bool, error)

	// WatchLog follows the bridged tool log beside run turnID's record, whatever job
	// it is on ([BridgeLog]): its state as the watch begins, READ FROM THE STORE'S
	// NEWEST WRITE so a restarted watch misses nothing, then each state it reaches
	// (one a later write replaced before the reader took it is passed over, the
	// newest always delivered), and none ([LogView.Found] false) once the run's
	// record and log are gone. A state this build cannot read is passed over. The
	// channel closes when ctx ends or the watch fails, which says nothing of the log:
	// its reader watches again. What a collection waits on its calls in flight by
	// ([Coordinator.drainCalls]).
	WatchLog(ctx context.Context, turnID string) (<-chan LogView, error)

	// BridgeLog reads the bridged tool log of run's job: the calls of its round as
	// the log beside the record now holds them ([BridgeLog]). run is the row the
	// caller read and the log is read AFTER it, so it holds every call the job had
	// recorded by then.
	//
	// A LOG THAT IS NOT THE JOB'S READS AS ONE WITH NO CALLS, Launch naming the job:
	// no log, one the launch had not yet opened to the job when the row was read, or
	// one a later launch moved on; never another job's calls, which a resume would
	// replay as its own.
	//
	// AN ERROR IS THE STORE'S, never "no calls": a resumed turn judged on an empty
	// log did nothing, which the delivery check fails.
	BridgeLog(ctx context.Context, run PendingRun) (BridgeLog, error)

	// TurnLog reads the calls of run's turn as run's job stands to hold them, whatever job
	// the log is on: what an ending reads ([RunCalls]) and a capture places its session by
	// ([SessionCapture.Through]). NOT [PendingStore.BridgeLog], WHICH READS A LOG NOT YET OPENED TO THE JOB AS EMPTY:
	// right for a resume, a false account for an ending. A relaunch writes the row, then
	// opens the log in a second write, and a stop between leaves a row whose job the log
	// never opened to; read as no calls, a turn whose earlier round posted its reply would
	// be recorded as one that did not. So a log on an earlier job is answered as the
	// opening would leave it ([PendingRun.Round], [PendingRun.AnsweredJob]), earlier
	// rounds folded into [BridgeLog.PriorCalls], and the person-approved call still owed
	// ([HeldCall.Call], [PendingRun.OwedCall]) is read as its round's. AN ERROR IS THE
	// STORE'S, never "no calls".
	TurnLog(ctx context.Context, run PendingRun) (BridgeLog, error)

	// Handed reads every secret run's job's box was handed, SEALED under the fleet keyring
	// ([HandedSecrets], [SessionKeep.SealHanded]): what a capture of the box's session
	// takes out by value. Kept BESIDE the record (up to [MaxHandedBytes]), written by the
	// attach naming the command ([PendingStore.AttachSandbox]), GROWN by every renewed
	// token ([PendingStore.AddHanded]), and replaced by the next job's attach, which
	// carries it forward where that job continues in the same box ([PriorBox.Handed]).
	// "" WHERE THE JOB HAS NONE (unsealed launch, unlanded attach, a later job's set),
	// never another job's set, which would scrub values the box was not handed: every
	// capture is refused rather than keep a session scrubbed of less than its box was
	// given. [ErrHandedIncomplete] where the set was marked as holding less than the box.
	Handed(ctx context.Context, run PendingRun) (string, error)

	// AddHanded grows run's job's sealed set ([PendingStore.Handed]) by a token a refresh
	// is about to write into the box, BEFORE it is written ([Coordinator.RecordHanded]):
	// add gets the set and answers the set to keep, or "" and why it can no longer hold
	// every value the box does, which MARKS IT INCOMPLETE (every later capture refused).
	// TRUE IS A TOKEN THE REFRESH MAY WRITE (landed, needed no write, or the set already
	// refuses captures; a job with NO SET is marked so). FALSE AND NO ERROR where the row
	// does not hold run's job running with its ending undecided, or a later job's set is
	// beside it: a stale poll. An error is the store's; the next poll asks again. ONE
	// COMPARE-AND-SWAP OF THE PART decides the set, so an opening or capture never misses a
	// recorded token. Unfenced, like a code hold: what a box holds is true whoever holds
	// the seat.
	AddHanded(ctx context.Context, run PendingRun, add func(sealed string) (next, incomplete string)) (bool, error)

	// Leads reads the turn's leads kept beside run's row as the row expects: every lead the
	// part keeps, oldest first, and how many earlier ones it does not (its own cut plus
	// every lead the row counts, [PendingRun.LeadsAppended], that it cannot give back). A
	// part of another run, or none, is none of this run's. An error is an unreadable part,
	// never "none": the caller tells every counted lead as not kept and hands the marked
	// ones on for a later read ([TurnLeads.Unread]). Kept BESIDE the record like the log
	// (up to [MaxKeptLeadBytes]), taken in only by the attach ([PendingStore.AttachSandbox])
	// and kept by every relaunch ([Coordinator.leadsAt]).
	Leads(ctx context.Context, run PendingRun) (TurnLeads, error)

	// RecordAsk records the question job `launch` asked a person through its bridge
	// (ADR-0068) onto the fields a parked question is read from
	// ([PendingRun.Question], [PendingRun.QuestionContext], [PendingRun.Decision],
	// [PendingRun.Audience], [PendingRun.Branch]), as [Ask.Redacted] has it. The run
	// parks on it when its harness ends and the job is collected.
	//
	// PINNED TO THE JOB AND ITS RUNNING, like a bridged call: recorded only while the row
	// holds that job launching or running ([AskNotRunning]), so a call outliving its job
	// never parks the next. ONE QUESTION PER JOB ([AskAlreadyAsked]); the first stands. An
	// invalid ask ([Ask.Validate]) is an error, as is a held call whose sealed arguments
	// exceed their share of the row ([ErrHeldCallTooLarge], [HeldArgsFit]).
	RecordAsk(ctx context.Context, turnID, launch string, ask Ask) (AskOutcome, error)

	// AddCodeHolds adds keys to the run's [PendingRun.CodeHolds], the turn's holds
	// from Gitlet's expiry, written BEFORE the request each was taken for, through
	// the read-decide-write every flip takes, so racing adds both land, each key once
	// in first-taken order. FALSE AND NO ERROR where the row is gone or its ending
	// is decided: the run holds nothing new and its turn is told before sending the
	// request. [ErrCodeHoldsFull], writing nothing, where the keys would pass
	// [MaxCodeHolds]. Unfenced: a hold is a fact about what the turn named, true
	// whoever holds the seat, and a turn whose lease moved writes nothing else here.
	AddCodeHolds(ctx context.Context, turnID string, keys []string) (bool, error)

	// RaiseCodeFloor raises a run's [PendingRun.CodeFloor], only while the row holds
	// launch: raise gets the floor as it stands (token, "" for none) and answers the
	// next, or false for no write. Reports whether this call wrote. A gone row, a
	// decided ending, or another launch is no write and no error: the job whose box
	// was shown something is over, and its floor with it.
	//
	// THE STORE NEVER COMPARES POSITIONS (it reads no log), so raise is the caller's
	// judgement of the higher floor, asked against the row as the write reads it: the
	// read-decide-write reruns it on every lost race, so racing raises land the
	// higher and none overwrites what the coordinator wrote meanwhile. No floor
	// writes nothing: a floor is raised, never cleared. Unfenced, like a hold: what a
	// box was shown is true whoever holds the seat.
	RaiseCodeFloor(ctx context.Context, turnID, launchID string,
		raise func(current string) (next string, ok bool)) (bool, error)

	// GrantRepo adds g's repository to [PendingRun.Repos] while the row holds job launchID
	// launching or running, raising [PendingRun.CodeFloor] and setting
	// [PendingRun.CodeLaunch] where unset: a `code_checkout` the box made (ADR-0093).
	// Answers [GrantAdded]; [GrantHeld] for the same branch and access; [GrantConflict]
	// for one given otherwise, another ceiling, or another requester than
	// [PendingRun.CodeWorkingFor] (nobody included: a job granted for nobody is never used
	// for somebody); [GrantFull] past [MaxRunRepos]; [GrantNotRunning]. An invalid grant
	// ([RepoGrant.Validate]) is an error. The first grant sets whom the turn works for; a
	// later one for the same credential with fewer apps narrows it, never widens. THE ROW
	// IS THE GRANT: Gitlet's box admission reads it on every request. Unfenced, like a
	// floor raise: what the box was given is true whoever holds the seat.
	GrantRepo(ctx context.Context, turnID, launchID string, g RepoGrant) (RunRepo, GrantOutcome, error)

	// ListRuns returns EVERY run the store holds, whatever its status (a newer
	// build's included), oldest first, raising rather than answering short. A run is
	// unended exactly while it has a record ([PendingStore.Finish] deletes it), so a
	// reader asking "which runs are unended" asks this, never
	// [PendingStore.ListActive], whose filter is this build's status list: a newer
	// peer's run would drop out mid-upgrade, with whatever its turn holds.
	ListRuns(ctx context.Context) ([]PendingRun, error)

	// ListActive returns every run that still owns engine-side state.
	ListActive(ctx context.Context) ([]PendingRun, error)

	// ListActiveForSeat is the "is this seat busy?" read, by the HANDLE the seat
	// runs under: the key this node's per-seat bookkeeping counts by, which its
	// callers (running, recovering, reply-matching) already hold. It and
	// [PendingStore.ListActiveForSeatID] select on the runs' standing
	// (internal/coord's RunStanding), reading whole only that seat's records
	// and any record stating none, so a seat's read never moves every row.
	ListActiveForSeat(ctx context.Context, handle string) ([]PendingRun, error)

	// ListActiveForSeatID is the same seat's unfinished runs selected by its ID
	// ([PendingRun.AgentID], read through the one parse its subjects take), for the
	// reader holding nothing else: a mailbox retirement.
	//
	// A mailbox the broker's listing alone found carries no handle, and nothing the
	// fleet holds says what the departed seat was called. Selected by a handle it
	// lacks, every run of such a seat was missed and its subscriptions deleted with
	// boxes still held and nothing able to reach them. The id is also the only
	// exact answer: the same handle under a renamed organisation is another seat
	// (ADR-0013), whose runs a handle would hand to this one's retirement.
	// [uuid.Nil] names no seat and is an error, not the runs whose row names none.
	ListActiveForSeatID(ctx context.Context, seat uuid.UUID) ([]PendingRun, error)

	// RecordAnswer records a person's reply as the answer to the question a run is parked
	// on, flipping it to [StatusAnswered], and returns the row IFF THIS CALL WON, a
	// COMPARE-AND-SET: the run must still be [Awaiting] on the launch matched, with no answer
	// and none of the deliveries among [PendingRun.DeclinedAnswers]; the loser reads false
	// (not an error). UNDER THE RECORDING NODE'S LEASE, superseded as a claim is
	// ([PendingStore.ClaimForResume]): a newer lease (the zero fence included) is refused with
	// [ErrSeatNotHeld] (the answer is the holder's to record and drive), the lease not stamped. An answer lacking a delivery, route or seal
	// ([RecordedAnswer.Sealed]) is an error; the store opens nothing ([AnswerSeal]). AN ANSWER
	// PAST ITS SHARE OF THE ROW ([MaxAnswerBytes]) is refused with [ErrAnswerTooLarge] ONLY
	// WHERE THE ROW WOULD HAVE TAKEN IT ([PendingRun.takesAnswer]), else false (an ordinary
	// message); the coordinator records it as a REFUSAL ([PendingStore.RecordRefusal]).
	RecordAnswer(ctx context.Context, turnID, launch string, answer RecordedAnswer, fence Fence,
	) (PendingRun, bool, error)

	// RecordRefusal records a reply too large to be the answer ([ErrAnswerTooLarge]) as the
	// OUTCOME of the question: the run flips to [StatusAnswered] holding answer (delivery ids,
	// post time, a seal of who gave it, none of its words, [AnswerWords.Refusal]) with refusal
	// beside it ([PendingRun.AnswerRefusal]), in one write (ADR-0114).
	// ON [PendingStore.RecordAnswer]'S TERMS EXACTLY (compare-and-set, lease rule, FALSE IS
	// NOT AN ERROR). From the write on, every other write treats the refusal as the
	// question's answer (claimed, taken, revived, declined, let go of), the run being owed
	// a resume with it; declined or let go of, the question waits again. IT CARRIES NONE OF
	// THE REPLY: an answer carrying deliveries ([RecordedAnswer.Events]) is an error, as is
	// one [PendingStore.RecordAnswer] would refuse, a refusal naming other deliveries than
	// the answer, or figures that are no reply past a share; what returns to the seat if
	// the refusal is let go of is the delivery itself, kept on its inbox ([AnswerRefused]).
	RecordRefusal(ctx context.Context, turnID, launch string, answer RecordedAnswer, refusal AnswerRefusal,
		fence Fence) (PendingRun, bool, error)

	// DeclineAnswer lets go of a recorded answer the run could not be resumed with,
	// IN ONE WRITE: the run returns to the status the record took it out of, the
	// answer is cleared, its deliveries and the ids of the copies handed back join
	// [PendingRun.DeclinedAnswers], and the copies join [PendingRun.HandBack] for the
	// caller to publish. Only while the run is still [StatusAnswered] on that launch,
	// holding exactly the answer made of the deliveries named, no newer lease
	// outranking the fence. Returns the row IFF THIS CALL WROTE IT. FALSE IS NOT AN
	// ERROR.
	//
	// AND ONLY ON A ROW THAT OWES THE SEAT NOTHING YET: one still carrying an earlier
	// decline's unpublished copies refuses with [ErrHandBackOwed], which bounds
	// [PendingRun.HandBack].
	DeclineAnswer(ctx context.Context, turnID, launch string, answer []string,
		handBack []HandedBack, fence Fence) (PendingRun, bool, error)

	// TakeAnswer records that the resumed turn a claim drives TOOK the recorded answer
	// ([RecordedAnswer.TakenAt]), reporting whether the run is still that claim. The write a
	// resume makes just before its turn runs ([ResumeRequest.Begin]); one unable to make it
	// does not run, since a turn running without it would be reaped after a crash
	// as never having got the answer and the reply handed back twice. Only while the run is
	// [StatusResumed] on that launch, carrying the answer, ending undecided
	// ([PendingStore.DecideEnding]), and the fence is the row's own lease or newer, so a
	// process that lost the seat never takes an answer the holder is about to return.
	// UNLIKE EVERY OTHER WRITE HERE, A ZERO FENCE IS NOT EXEMPT: a take under no lease is
	// refused on a row any lease owns (a stalled leaseless claimant once took the answer on
	// its successor's fenced row and the person was answered twice). FALSE IS NOT AN ERROR;
	// an answer already taken answers true and is not rewritten.
	TakeAnswer(ctx context.Context, turnID, launch string, fence Fence) (bool, error)

	// ClaimWaitOver claims a run waiting on what the engine answers
	// ([PendingRun.EngineAnswers]) on that launch, with no answer and its resume
	// not held, straight to [StatusResumed], recording the claim ([WaitOver],
	// its counts kept), as [PendingStore.ClaimForResume] does a tail: the row
	// IFF THIS CALL WON, [PendingRun.ClaimedFrom] set. Never an answer recorded.
	ClaimWaitOver(ctx context.Context, turnID, launch string, fence Fence) (PendingRun, bool, error)

	// TakeWaitOver records that the resumed turn that claim drives took it
	// ([WaitOver.TakenAt]), on [PendingStore.TakeAnswer]'s terms, a zero fence
	// not exempt: false where the run is no longer that claim, true and nothing
	// rewritten where it was taken already.
	TakeWaitOver(ctx context.Context, turnID, launch string, fence Fence) (bool, error)

	// ReviveWaitOver gives that claim, untaken, back to the status it was
	// claimed out of, fenced to the reviving lease, counted where lost (as
	// [Revival.Lost]); the row IFF THIS CALL WON. One a turn took answers
	// [ErrWaitOverTaken] with the row: take and revival are exclusive.
	ReviveWaitOver(ctx context.Context, turnID, launch string, fence Fence, lost bool) (PendingRun, bool, error)

	// MarkHeldCall records that the resume a claim drives is making the held call its
	// run's job made ([PendingRun.HeldCall]), and answers the held call as the row
	// then holds it and whether this write marked it: false for a call some resume
	// marked before, never made again (ADR-0077).
	//
	// ON THE CLAIM'S TERMS, as a take ([PendingStore.TakeAnswer], zero fence not exempt);
	// any other row answers [ErrHeldCallUnclaimed]. NOTHING CLEARS THE MARK but the next
	// job's opening: a claim given back or revived keeps it, so every later attempt finds it
	// on the row its claim returns ([PendingStore.ClaimForResume]), with the result once
	// recorded, answered before asking whether the call may be made ([HeldResume.Call]).
	MarkHeldCall(ctx context.Context, turnID, launch string, fence Fence) (HeldCall, bool, error)

	// RecordHeldResult records what the held call a resume marked came to
	// ([HeldMade]), on the terms of [PendingStore.MarkHeldCall], once: a call whose
	// result is recorded is not written again and answers true.
	//
	// THE RESULT, THEN THE CALL: the result, sealed ([CallSeal.SealResult]), on the held
	// call under the claim's fence, with the call as the log keeps it ([HeldCall.Call],
	// [BridgeCall.Approved], [BridgeCall.HeldID], no arguments or output); then that call
	// appended to the round's log ([BridgeLog.Calls]). Two keys the store cannot write at
	// once, the result first. A crash between, or a failed append, leaves the call on the
	// row alone and nothing loses it: a retry appends where the log lacks its id, an ending
	// reads it off the row ([PendingStore.TurnLog]), and the next opening appends the call
	// the reset carried ([PendingRun.OwedCall]). Never twice.
	RecordHeldResult(ctx context.Context, turnID, launch string, made HeldMade, fence Fence) (bool, error)

	// GiveDelegateVerdict records one delegate's answer to a held call put to
	// delegates (ADR-0125) and answers the held call as written. ONLY WHILE THE CALL
	// WAITS ON ITS DELEGATES: run awaiting on launch, ending undecided, held call
	// heldID not escalated, v.By one of its delegates with no answer yet. Otherwise
	// false and nothing written; an answer past its bounds is an error. Its
	// reason and findings are recorded redacted ([HeldVerdict.Redacted]).
	GiveDelegateVerdict(ctx context.Context, turnID, launch, heldID string, v HeldVerdict) (HeldCall, bool, error)

	// EscalateHeld puts a held call waiting on its delegates to people, on the terms
	// of [PendingStore.GiveDelegateVerdict]: it is then put to the run's audience.
	// False for a call answered, escalated or let go of meanwhile.
	EscalateHeld(ctx context.Context, turnID, launch, heldID string, e HeldEscalation) (bool, error)

	// ReviveAnswer gives a claim no resume holds back to the recorded answer it was taken
	// for: the run returns to [StatusAnswered], owed the resume, the row FENCED to the
	// reviving node's lease and, for a claim whose node stopped before its turn took the
	// answer ([Revival.Lost]), the loss COUNTED ([RecordedAnswer.LostClaims]); a claim its
	// own node could not confirm is given back uncounted. Returns the row IFF THIS CALL
	// WROTE IT, only while the run is still that claim ([StatusResumed], that answer,
	// ending undecided, no newer lease). FALSE IS NOT AN ERROR. EXCLUSIVE WITH THE TAKE: an
	// answer a turn already took is used and refused with [ErrAnswerTaken] (the caller
	// reaps the claim as spent), and a take after the revival finds no claim.
	ReviveAnswer(ctx context.Context, turnID string, revival Revival) (PendingRun, bool, error)

	// OweHandBack LETS GO of the recorded answer a run whose ending is decided still
	// holds: in one write the answer leaves the row and copies of its deliveries are
	// recorded as owed to the seat's inbox ([PendingRun.HandBack]), for the ending to
	// publish before deleting the row ([PendingStore.Finish]). Returns the row IFF THIS
	// CALL WROTE IT.
	//
	// The decline's outbox for the other ways an answer is let go of: all have nowhere to
	// send the reply but the seat's inbox, its delivery having been spent when recorded.
	// ONLY A STEP OF THE ENDING NAMED ([LetGo.Ending]); else FALSE IS NOT AN ERROR. The
	// decision, which fences off any TAKE, settles whether it goes back: an untaken answer
	// does, a taken one only if unused ([RecordedEnding.Unused]), else [ErrAnswerTaken]. A
	// row already owing copies refuses with [ErrHandBackOwed].
	OweHandBack(ctx context.Context, turnID string, letGo LetGo) (PendingRun, bool, error)

	// ClearHandBack removes the copies a decline owed the seat's inbox that are now
	// published, by id, reporting whether it removed any. Unconditioned on status,
	// launch or lease: it records a fact about the broker, true whoever published.
	// FALSE IS NOT AN ERROR: a gone run, or copies a peer already cleared.
	ClearHandBack(ctx context.Context, turnID string, ids []string) (bool, error)

	// FileNote files a person's note on a run's record ([RunSteer]), waiting, in every
	// live status with no ending decided, and answers what became of it:
	// [NoteDuplicate], [NoteSpent] past [NoteFiling.MaxNotes], [NoteFull] past
	// [NoteFiling.MaxWaiting], or [NoteClosed], none an error. The bounds count a
	// person's notes alone, and a note a hook's live serve handed over
	// ([RunNote.LiveServe]) no longer waits. A filing naming a job or call
	// ([RunNote.For], [RunNote.LateCall]) is refused with [ErrNoteKind]. No fence: a
	// note is an append, not an ownership decision. The seal must be within
	// [MaxNoteSealedBytes] ([ErrNoteTooLarge]). See the package doc, "Filing and
	// serving notes".
	FileNote(ctx context.Context, turnID string, filing NoteFiling) (NoteFiled, PendingRun, error)

	// FileWake files a wake for the job a filing names as a wake item
	// ([RunNote.Wake]), the store's alone to file, and answers the item holding
	// it: only while the record names that job, launching or running, its
	// ending undecided, else [ErrWakeClosed]. It joins the job's newest item
	// while that waits served to nobody, within [MaxItemWakes] and
	// [MaxWakeItemBytes], else opens one, past [MaxJobWakes] for the job or
	// [MaxTurnWakes] for the turn [ErrWakesSpent]; a seal past
	// [MaxWakeItemBytes] is [ErrWakeTooLarge]. A retry of a wake held answers
	// its item. No fence. See the package doc, "A wake joins a running run".
	FileWake(ctx context.Context, turnID string, filing WakeFiling) (RunNote, error)

	// CountRunSend counts delta messages the job names sent other runs
	// ([PendingRun.RunSends]): a positive delta only while the record names that
	// job, launching or running, its ending undecided, else [ErrWakeClosed], and
	// within [MaxJobRunSends], else [ErrRunSendsSpent]; a negative one gives a
	// send back whose message no run took, never below none. No fence.
	CountRunSend(ctx context.Context, turnID, job string, delta int) error

	// ListActiveOn returns the unfinished runs of seats other than the one on
	// excepts that are on its work: charged to an item it names, or in its
	// conversation; on what its caller created or wrote; or that created or
	// wrote its item or conversation ([PendingRun.WroteKeys]). Oldest first, at
	// most [MaxRunsAround]; none for work that names nothing. One read, never
	// one per seat nor per run.
	ListActiveOn(ctx context.Context, on RunsOn) ([]PendingRun, error)

	// AskFoldIn records fold-in f on the run's row for its job ([FoldIn]):
	// only while the row holds that job in [ItemStatuses] and no ending is
	// decided, else [ErrFoldInClosed]; once per job and wake, a repeat
	// answering the one recorded; within [MaxJobFoldIns] for the job, else
	// [ErrFoldInsSpent]; and [ErrFoldInTooLarge] past [MaxFoldInBytes]. The
	// store stamps its AskedAt.
	AskFoldIn(ctx context.Context, turnID string, f FoldIn) (FoldIn, error)

	// AnswerFoldIn records the answer to fold-in id: only while the row holds
	// its job in [ItemStatuses], else [ErrFoldInClosed]; the first answer
	// alone, a later one refused [ErrFoldInAnswered] with the fold-in naming
	// the first; never its asker's ([ErrFoldInAsker]). The store stamps its
	// AnsweredAt.
	AnswerFoldIn(ctx context.Context, turnID, id string, a FoldInAnswered) (FoldIn, error)

	// TakeNotes records the notes a take names read, where its Read says, and answers
	// the ones THIS CALL recorded, each with its read. Pinned ([NoteTake]): a failed
	// pin, a note no longer waiting, an item for a job its Read does not name
	// ([RunNote.IsFor]) and a gone record take nothing, never an error. Each read is at
	// the take's own place among the reads, but a note a hook's serve placed is at its
	// own ([RunNote.Place]). A take confirming delivery ([NoteTake.Confirms]) records a
	// telling of each late-call item it reads at its own place ([RunSteer.Tellings]).
	//
	// THE READ IS THE REVIEW'S HANDOVER, never delivered twice by racing takers nor
	// without a record; a hook's is a serve ([PendingStore.ServeHook]).
	TakeNotes(ctx context.Context, turnID string, take NoteTake) ([]RunNote, error)

	// ServeNotes records the notes a serve names served to its job ([RunNote.Served]),
	// pinned to the job, a status the serve names and an undecided ending
	// ([NoteServe]), and answers every waiting one served to that job, in record
	// order. A failed pin, a note no longer waiting, an item for another job
	// ([RunNote.IsFor]), and a gone or ending record serve nothing, never an error; a
	// serve naming no job or status is one. The mark precedes the handover, so a note a
	// harness may have delivered is never one the record is silent on
	// ([PendingStore.TakeNotes]); a wake item a wake joined since the poll read it is
	// served nothing ([NoteServe.Wakes]).
	ServeNotes(ctx context.Context, turnID string, serve NoteServe) ([]RunNote, error)

	// AgainNotes grants a job's harness the second admission of each note the ask
	// names that waits, a poll served that job, and is a person's note or a late
	// call's item, pinned as a serve is ([NoteAgain]): granted spends one of the
	// note's poll serves ([RunNote.PollServes]) unless that job was granted it
	// already; spent, at its budget, it is given up and waits for the review.
	// Any other note is neither: a wake item, served once, is never admitted
	// again. A gone or ending record grants nothing, never an error.
	AgainNotes(ctx context.Context, turnID string, ask NoteAgain) (granted, spent []string, err error)

	// SettleTake settles a take whose TakeNotes answered an error, by the identity its
	// read named ([NoteRead.Take]): its write may have landed with the answer lost, or
	// may land yet. It answers every note whose read that take recorded, each first
	// marked unanswered where the settle says its reader never had them
	// ([NoteSettle.Unanswered]), and leaves the take nothing more to record: where the
	// record holds no read of it, the record is written all the same so the take's
	// in-flight write, conditioned on an earlier version, never lands after it. A
	// record gone before or under the settle answers nothing, never an error; a settle
	// naming no take is an error.
	//
	// NO PIN AND NO FENCE: what a take recorded is a fact about the take, true
	// whoever holds the record and whatever its status or ending.
	SettleTake(ctx context.Context, turnID string, settle NoteSettle) ([]RunNote, error)

	// ServeHook records the notes a hook's serve names served to its job's hook
	// ([HookServed]) in one write and answers the ones this call served, each with its
	// serves; pinned to the serve's job, statuses and an undecided ending ([HookServe]).
	// A live serve that answered a cancelled call is lost to the cancel
	// ([ServeCancelled]), a serve of an unanswered take is removed, then each eligible
	// note is served, up to [RunNote.MaxServes] serves, and a wake item a wake joined
	// since the hook read it not at all ([HookServe.Wakes]). A serve, never a read: the
	// job's session says whether the hook took it ([PendingStore.SettleHook]). See the
	// package doc, "Filing and serving notes".
	ServeHook(ctx context.Context, turnID string, serve HookServe) ([]RunNote, error)

	// SettleHook settles the serves of a waiting note to the job named ([HookSettle]):
	// an unanswered take's serve is removed, a note served by a take named held (or
	// restarted) is read off its earliest such serve, and on a note not read so a
	// cancelled live serve is lost to the cancel and an absent take's serve lost absent
	// ([ServeAbsent]). It answers every note it changed and every note read off a serve
	// to the job where it names a take held or restarted, so a repeated settle answers
	// what it read. A late-call item read off a held take records a telling at the
	// write's own place, which the record's count moves to, the read staying at the
	// item's ([RunSteer.Tellings]); one restarted records none. No pin, no fence: a
	// fact about the job. See the package doc, "Filing and serving notes".
	SettleHook(ctx context.Context, turnID string, settle HookSettle) ([]RunNote, error)

	// MarkPulled records that the job named has asked for its notes at least once
	// ([RunSteer.Pulled]), reporting whether this call recorded it. FALSE IS NOT AN
	// ERROR: the record holds another job, says so already, or is gone.
	MarkPulled(ctx context.Context, turnID, launch string) (bool, error)

	// MarkSessionStart records which session the job named said its box started
	// ([LaunchRecord.SessionStart]), reporting whether this call recorded it. ONCE PER
	// JOB: a box starts one session and the first record stands. FALSE IS NOT AN ERROR:
	// the record holds another job, has said already, or is gone. NO FENCE, like
	// MarkPulled: a fact about the job, not an ownership decision. A start that is not
	// [SessionStart.Valid] is an error.
	MarkSessionStart(ctx context.Context, turnID, launch string, start SessionStart) (bool, error)

	// BindSession binds the session a minting harness's job reported to the job's record
	// ([LaunchRecord.Minted]) and answers the record holding it. ONCE PER JOB: the same id
	// again answers as bound; another, a job that continued its session reporting any id
	// but that one, one that started its own reporting the one it was asked to continue,
	// another job's record, a gone one, and an id another live run claims are
	// [ErrSessionBindRefused]. The claim, fleet-wide, is taken first, then [SessionBind.Fresh]
	// asked, then the record written; a relaunch's job takes its turn's claim over, and the
	// claim goes on a failed bind, a landed capture ([PendingStore.ReleaseSessionClaim]) or
	// the record's end. NO FENCE: a fact about the job, as a settle is.
	BindSession(ctx context.Context, turnID string, bind SessionBind) (PendingRun, error)

	// ReleaseSessionClaim removes turnID's claim of a session it bound, once the
	// executor log names the session (its capture landed) and so guards the id
	// itself; a claim another run holds, or none, is left alone.
	ReleaseSessionClaim(ctx context.Context, turnID, sessionID string) error

	// MarkEndedAtDeadline records that the collection of the job named saw it still
	// working past its turn's wall-clock cap and is ending it
	// ([LaunchRecord.EndedAtDeadline]), reporting whether this call recorded it. ONCE
	// PER JOB; the next launch's job starts with nothing said. FALSE IS NOT AN ERROR:
	// another job, already said, being ended, or gone. NO FENCE, like MarkSessionStart:
	// the box's host makes it as it reads the box, and it is a fact about the job.
	MarkEndedAtDeadline(ctx context.Context, turnID, launch string) (bool, error)

	// MarkHarnessEnd records how the job's harness ended and what the newest request of the
	// session its box ran carried, as a read collecting the job found them
	// ([LaunchRecord.HarnessEnd], [LaunchRecord.Context]), in ONE WRITE, reporting whether
	// the record changed. CUT OFF ONCE, CUT OFF FOR THE JOB ([HarnessEnd.after]): a cut-off
	// end is recorded over an own-run end, never the reverse, and nothing over a word this
	// build does not know. THE MEASURE BY ITS LINEAGE ([LaunchRecord.measured]) and THE
	// COUNT AS READ ([LaunchRecord.counted]; a mark past its bound is [CountUnread]), neither
	// carried to the next job. FALSE IS NOT AN ERROR (another job, already said, being
	// ended, gone). NO FENCE: a fact about the job. An end not [HarnessEnd.Valid] is an error.
	MarkHarnessEnd(ctx context.Context, turnID, launch string, read EndRead) (bool, error)

	// MarkResumeBegun records on a claimed run what the turn resuming from the job named
	// does with the job's round, as it begins ([LaunchRecord.ResumeCarry]), reporting
	// whether this call recorded it. Only while the run is [StatusResumed] on that job,
	// ending undecided, under a fence no newer lease outranks. FALSE IS NOT AN ERROR:
	// moved on, already said, or gone. A carry that is not [ResumeCarry.Valid] is an
	// error.
	MarkResumeBegun(ctx context.Context, turnID, launch string, fence Fence, carry ResumeCarry) (bool, error)

	// WatchNotes follows one run's notes: the record's state when the watch began
	// ([NotesView.Found] false where it has none), then each state it reaches as
	// [coord.SandboxRuns.WatchSandboxRun] delivers them, decoded only as far as a
	// [NotesView] reads. A reader slower than the record gets the newest state, never a
	// queue of older ones.
	//
	// A CLOSED CHANNEL IS NOT A DELETE: the watcher has stopped hearing, keeps what it
	// last knew and watches again.
	WatchNotes(ctx context.Context, turnID string) (<-chan NotesView, error)
}

// ConversationRef is where an arriving delivery came from, as a parked run is
// matched against it: the durable conversation and the inbox partition it
// arrived in.
//
// TWO VALUES, EACH DECIDING A HALF. The identity decides WHICH runs a delivery
// may answer (where a person answers). The partition decides WHICH OF THEM it
// answers when several may: the identity is coarse on purpose (every run parked
// on one DM shares it), so without the batch a DM's clarifications are told
// apart by creation time alone. See [ConversationRef.Best], the whole rule.
//
// A struct rather than two string arguments, since a swapped pair fails silently
// as a run nobody can answer.
type ConversationRef struct {
	// Identity is the durable conversation notify derives for the delivery's
	// partition, and what the row's ConversationKey holds.
	Identity string

	// Partition is the inbox batch the delivery arrived in, what the row's
	// PartitionKey holds.
	Partition string
}

// Answers reports whether this delivery is the reply parked run is waiting for.
//
// ON THE IDENTITY, where a person answers: a seat replying to a top-level DM replies AS A
// THREAD, a FINER partition than the channel the run parked under, so partition equality
// never met; so a top-level reply now also answers a run parked from a thread. A DM is ONE conversation however threaded. Wider means SEVERAL rows can be
// admitted; [ConversationRef.Best] chooses. AN EMPTY VALUE NEVER MATCHES, either side: two
// absences would make every A2A or task delivery the answer to every such run, and the
// engine never offers a delivery nobody can reply into (internal/notify's Derived). A HELD
// CALL IS NEVER A CONVERSATION'S TO ANSWER (ADR-0077): approval has its own route.
func (c ConversationRef) Answers(run PendingRun) bool {
	return run.HeldCall == nil && run.ConversationKey != "" && run.ConversationKey == c.Identity
}

// sameBatch reports whether this delivery arrived in the very batch the run was
// launched from. The empty check is [ConversationRef.Answers]': two absences
// comparing equal would prefer every partition-less run alike.
func (c ConversationRef) sameBatch(run PendingRun) bool {
	return c.Partition != "" && run.PartitionKey == c.Partition
}

// Best is the parked run a delivery answers, out of the runs one seat has
// waiting: the whole rule, so a store hands over candidates and decides nothing.
//
// TWO STAGES: the identity ADMITS, the partition DISAMBIGUATES. Two runs parked from two
// threads of one DM are both admitted by a reply in either, and recency alone would splice
// thread A's answer into thread B's run. RECENCY IS THE LAST WORD among candidates agreeing
// on partition; the turn id breaks a same-instant tie so every node picks the same.
func (c ConversationRef) Best(parked []PendingRun) (PendingRun, bool) {
	var best PendingRun
	found := false
	for _, run := range parked {
		if !c.Answers(run) {
			continue
		}
		if !found || c.preferred(run, best) {
			best, found = run, true
		}
	}
	return best, found
}

// preferred reports whether a is the better answer of two admitted candidates.
func (c ConversationRef) preferred(a, b PendingRun) bool {
	if am, bm := c.sameBatch(a), c.sameBatch(b); am != bm {
		return am
	}
	if !a.CreatedAt.Equal(b.CreatedAt) {
		return a.CreatedAt.After(b.CreatedAt)
	}
	return a.TurnID > b.TurnID
}

// Clarification is what parking a run on the question its job asked records
// beside it. The question is on the row already, written by the ask
// ([PendingStore.RecordAsk]).
//
// NEVER THE SESSION THE COLLECTION READ OFF THE BOX: the answer carries the run
// on in the session its launch named and its box said it ran
// ([PendingRun.SessionRan]); what the collection read is a file the run's model
// could rewrite.
type Clarification struct {
	// InputTokens and OutputTokens are what the asking job cost, carried to the
	// resume its answer drives ([PendingRun.ParkedInputTokens]).
	InputTokens  int
	OutputTokens int
	// CacheReadTokens and CacheWriteTokens are InputTokens' cached share
	// ([PendingRun.ParkedCacheReadTokens]).
	CacheReadTokens  int
	CacheWriteTokens int

	// Condensed is what condensing the parking collection cost the seat's
	// auxiliary model, carried to the same resume ([LaunchRecord.Condensed]).
	Condensed AuxTokens

	// Answerers is who may answer, resolved from Audience against the chart
	// ([PendingRun.AudienceHandles]).
	Answerers Audience

	// AskedAt is when the question was put, taken before it was announced
	// ([PendingRun.AskedAt]). Required.
	AskedAt time.Time
}

// RecordedAnswer is a person's reply, recorded on the run it answers before
// anything is done with it, by either route: a chat reply matched on its
// conversation, or an answer by turn naming the run ([RecordedAnswer.Via]).
//
// RECORDING IS NOT RESUMING, which is the point. The reply was once held by its
// inbox delivery until a resume succeeded, and a failed resume handed the MESSAGE
// back, which on the only broker shipped returns it BEHIND newer mail: the
// person's next message reached the waiting run first and was spliced in as the
// answer, the first coming round later to answer whatever the run asked next.
// Recorded by a compare-and-set only the first qualifying reply wins, the answer
// is durable once the delivery is acknowledged, and retrying the resume is the
// coordinator's job ([Coordinator.retryOwed]), not the inbox's.
type RecordedAnswer struct {
	// Sealed is the answer itself: its text and who gave it, bound to the run,
	// question and deliveries, sealed under the fleet keyring by the recording
	// node ([AnswerSeal]). Required; [PendingStore.RecordAnswer] refuses it absent.
	//
	// THE ONLY COPY of its words and answerer: no field here carries either in the
	// clear, so a reader opens the seal ([AnswerSeal.Open]); answerseal.go says why.
	// The deliveries below are the messages they are, and nothing reads the answer
	// off them.
	Sealed []byte `json:"sealed"`

	// Via is the route the answer came by: a chat reply matched on its conversation,
	// or an answer by turn naming the run. Required, and refused by
	// [PendingStore.RecordAnswer] unless one of the two: it decides how the resumed
	// turn is told who answered, what the answer's record says, and what a copy
	// becomes when let go (a chat reply becomes the ordinary message it is; an answer
	// by turn has no ordinary form and its copy is spent by the seat's holder as an
	// answer the run no longer takes). IN THE CLEAR because the store and hand-back
	// decide on it, and inside the seal too, where a reader holds the two to each
	// other.
	Via types.AnswerVia `json:"via"`

	// EventIDs are the deliveries the answer was made of. A copy of one reaching the
	// seat again is the same answer, already recorded, never a second one or an
	// ordinary message.
	EventIDs []string `json:"event_ids"`

	// Events are those deliveries, encoded, oldest first: the resume raises the
	// working indicator off the newest, and an answer this node gives up on hands
	// them back to the seat's ordinary route as what they are.
	Events []json.RawMessage `json:"events,omitempty"`

	// PostedAt is when the newest was posted, the instant the question's
	// [PendingRun.AskedAt] was compared against.
	PostedAt time.Time `json:"posted_at,omitzero"`

	// RecordedAt is when the answer was recorded.
	RecordedAt time.Time `json:"recorded_at"`

	// From is the status the record took the run out of (awaiting, or reseed when
	// the pause reaper had reclaimed the box) and the one a let-go answer restores.
	From string `json:"from"`

	// TakenAt is when a resumed turn TOOK this answer ([PendingStore.TakeAnswer], at
	// [ResumeRequest.Begin]). Zero while the answer is still the run's; cleared by a release
	// giving the claim back ([PendingStore.ReleaseClaim]). THE ONE FACT A DEAD CLAIM CANNOT
	// TELL ANY OTHER WAY: a claimed row reads [StatusResumed] whether the resume never
	// reached its turn (the reply is still owed, its delivery spent) or the turn ran and died
	// (used). The seat's next holder ([Coordinator.RecoverSeat]) REVIVES the first
	// ([PendingStore.ReviveAnswer], or the reap hands it back, [ErrAnswerOwed]) and spends the
	// second ([ErrAnswerTaken]). WRITTEN AT THE LAST MOMENT BEFORE THE TURN, since any earlier
	// marker would read an unacted reply as used; the delivery is recorded as worked then
	// ([CoordinatorOptions.Spent]).
	TakenAt time.Time `json:"taken_at,omitzero"`

	// LostClaims counts claims of this answer whose node stopped before their turn
	// took it, each revived by the seat's next holder ([PendingStore.ReviveAnswer]),
	// and FirstLostAt is when the first was revived. A claim its own node made, could
	// not confirm and gave back is none of them ([Revival.Lost]). ON THE ROW because
	// they bound a series no one node sees: a dying claim is a stopped node, and a
	// node's own attempt count resets with exactly that ([MaxAnswerAttempts]). See
	// [MaxAnswerRevivals].
	LostClaims  int       `json:"lost_claims,omitempty"`
	FirstLostAt time.Time `json:"first_lost_at,omitzero"`
}

// Taken reports whether a resumed turn took this answer ([RecordedAnswer.TakenAt]).
func (a RecordedAnswer) Taken() bool { return !a.TakenAt.IsZero() }

// AnswerRefusal is what a run's row records of a reply past what it carries as an answer
// ([MaxAnswerBytes]): its deliveries and the figures it was refused on, never a word
// (ADR-0114; [PendingRun.Answer] names the deliveries, [AnswerWords.Refusal]).
// THE QUESTION'S OUTCOME, RESUMED LIKE AN ANSWER: left waiting, the question took the
// person's next short message ("any luck?") as its answer. Recorded, nothing written after
// it qualifies and the run resumes told that the reply did not reach it, to ask again
// ([PendingRun.AskedAt]) or carry on, with every guarantee of the answered path. LET GO
// (declined after its attempts, or an unopenable seal), the question waits again under its
// original anchor with the reply declined, and the let-go is announced
// ([Coordinator.announceRefusalLetGo]). NO EXTRA OF ITS OWN: a later field is a top-level
// key beside [RecordedAnswer] (wirekeys.go), which this build would otherwise drop.
type AnswerRefusal struct {
	// EventIDs are the deliveries the reply was made of, the answer's own
	// ([RecordedAnswer.EventIDs]), binding the refusal to it. Their count is the
	// reply's message count.
	EventIDs []string `json:"event_ids"`

	// Bytes is what the reply weighed as the row would have held it as an answer
	// (the larger of the answer and the copies letting it go would owe,
	// [AnswerTooLargeError.Weighs]) and MaxBytes what an answer may weigh there: the
	// two figures the run, record and API state, Bytes rounded up (kept.SizeUp) so a
	// reply past the share never reads as within it.
	Bytes    int `json:"bytes"`
	MaxBytes int `json:"max_bytes"`

	// Address is where the reply was posted: transport metadata of its newest message
	// (transport, conversation and thread, message and sender ids), none of its words.
	// The resume this refusal drives raises its working indicator there
	// ([AnswerRefusal.trigger]) as an answer's resume does off its deliveries;
	// otherwise the person saw no sign of activity on a vendor chat where the engine
	// posts nothing itself. Nil where the message stamped none or more than
	// [MaxRefusalAddressBytes]; the resume then raises no indicator and loses nothing
	// else.
	Address map[string]string `json:"address,omitempty"`
}

// Messages is how many messages the refused reply was: one delivery each.
func (r AnswerRefusal) Messages() int { return len(r.EventIDs) }

// equal reports whether two refusals are the same, nil equal to nil alone.
func (r *AnswerRefusal) equal(other *AnswerRefusal) bool {
	if r == nil || other == nil {
		return r == other
	}
	return r.Bytes == other.Bytes && r.MaxBytes == other.MaxBytes &&
		slices.Equal(r.EventIDs, other.EventIDs) && maps.Equal(r.Address, other.Address)
}

// RoundRefusal is one refused reply as the round's record keeps it for review
// ([PendingRun.RoundRefusals]): the job whose question it answered, its message
// count and the figures it was refused on ([AnswerRefusal]); no words or
// deliveries.
type RoundRefusal struct {
	Job      string `json:"job"`
	Messages int    `json:"messages"`
	Bytes    int    `json:"bytes"`
	MaxBytes int    `json:"max_bytes"`
}

// maxRoundRefusals bounds [PendingRun.RoundRefusals]. Eight: each is a reply past
// half a megabyte to one question that the run was told of and asked again, so a
// round past that is looping, which its review reads from the calls anyway; the
// newest eight say so, at about a KiB on the row.
const maxRoundRefusals = 8

// withRoundRefusal is refusals with the refusal of the question job asked
// appended once, however often the carrying-on relaunch is written, the oldest
// dropped past [maxRoundRefusals].
func withRoundRefusal(refusals []RoundRefusal, job string, refusal AnswerRefusal) []RoundRefusal {
	if slices.ContainsFunc(refusals, func(r RoundRefusal) bool { return r.Job == job }) {
		return refusals
	}
	out := append(slices.Clone(refusals), RoundRefusal{Job: job, Messages: refusal.Messages(),
		Bytes: refusal.Bytes, MaxBytes: refusal.MaxBytes})
	if len(out) > maxRoundRefusals {
		out = out[len(out)-maxRoundRefusals:]
	}
	return out
}

// carriedOnFromRefusal reports whether the run's round was carried on from the
// question job asked by the relaunch that told the run its reply was refused
// ([PendingRun.RoundRefusals]). Only a turn that TOOK the refusal makes that
// relaunch (it is how the run is told), so this is the row's own word that a turn
// kept the refusal, written in the write clearing it ([CoordStore.resetLaunch])
// and readable on every node whatever the completion ledger says
// ([Coordinator.refusedReply]).
func (r PendingRun) carriedOnFromRefusal(job string) bool {
	return slices.ContainsFunc(r.RoundRefusals, func(refused RoundRefusal) bool { return refused.Job == job })
}

// Refused is the refusal this run's recorded answer is, and whether it is one:
// [PendingRun.AnswerRefusal] where the row holds an answer naming exactly the
// deliveries it names. BOUND TO THE ANSWER so no other answer reads as a refusal:
// a key kept at the top of the row by a build without this field (wirekeys.go)
// while it let the answer go and recorded another names different deliveries and
// reads as none.
func (r PendingRun) Refused() (AnswerRefusal, bool) {
	if r.Answer == nil || r.AnswerRefusal == nil || !slices.Equal(r.AnswerRefusal.EventIDs, r.Answer.EventIDs) {
		return AnswerRefusal{}, false
	}
	return *r.AnswerRefusal, true
}

// refusal is [PendingRun.Refused] as a pointer, nil where the answer is none.
func (r PendingRun) refusal() *AnswerRefusal {
	refusal, ok := r.Refused()
	if !ok {
		return nil
	}
	return &refusal
}

// WaitsOn reports whether this run still waits on the question its job launch
// asked: that job is the run's, parked on its question, with no answer recorded
// and its ending undecided.
//
// THE ONE RULE for whether a question waits, read by the store recording an
// answer ([PendingRun.takesAnswer]) and by the coordinator matching a reply
// ([Reply.Best]), so a reply is matched only to a question the store would record
// it against.
func (r PendingRun) WaitsOn(launch string) bool {
	return r.LaunchID == launch && slices.Contains(Awaiting, r.Status) && r.Answer == nil && r.Ending == nil
}

// takesAnswer reports whether this run would record an answer made of deliveries
// ids as the answer to the question launch asked: it still waits on it
// ([PendingRun.WaitsOn]) and none of ids was let go of
// ([PendingRun.DeclinedAnswers]).
//
// THE ONE RULE for both halves of an answer's fate: whether
// [PendingStore.RecordAnswer] records it, and whether one past its share of the
// row is refused as too large or is the ordinary reply to an already-answered
// question, so a reply is never refused for size against a question the store
// would not have recorded it for.
func (r PendingRun) takesAnswer(launch string, ids []string) bool {
	if !r.WaitsOn(launch) {
		return false
	}
	for _, id := range ids {
		if slices.Contains(r.DeclinedAnswers, id) {
			return false
		}
	}
	return true
}

// HandedBack is one copy of a declined answer's delivery, owed to the seat's
// inbox as the ordinary message it is — see [PendingRun.HandBack].
type HandedBack struct {
	// ID is the copy's own id, derived from the original's, so publishing it
	// twice is one message to every reader that dedupes on ids.
	ID string `json:"id"`

	// Original is the delivery it is a copy of.
	Original string `json:"original"`

	// Event is the copy as it is published, encoded.
	Event json.RawMessage `json:"event"`
}

// Audience is who a parked question may be answered by, and whether they are a
// fallback rather than what the run asked for.
//
// FALLBACK IS A FACT A READER NEEDS: a question to "manager" on a seat with none,
// or to a name nobody in the chart has, still waits on the seat's lead chain, and
// a person reading "what is waiting on me" must tell a question put to them from
// one that reached them because nobody else could be named.
type Audience struct {
	Handles  []string
	Fallback bool
}

// BoxRef is the box and command a run is attached to.
type BoxRef struct {
	SandboxID string
	CommandID string

	// Placement is the cell holding the box, written with it. ONE WRITE FOR THE
	// THREE, being one fact: a box id read through another cell's backend reports a
	// vanished box, and a relaunch can land in another cell than the job before (a
	// fallback onto a member with its own run_in), so a row taking the new box but
	// keeping the old cell was polled, collected and killed through the wrong
	// provider. Empty keeps the row's cell, as an attach that is not a launch's does.
	Placement string

	// Node is the node whose host the box is on ([PendingRun.LaunchNode]): the
	// attaching node for a launch's attach, written with the box like its cell,
	// since a relaunch on another node keeps the previous box's host on the row until
	// this write names its own box. Empty keeps the row's node.
	Node string

	Harness     string
	SessionID   string
	PauseTTLSec float64

	// FreshSessionID is the session of its own a continuing job's script starts in
	// the continued one's place ([RunHandle.FreshSessionID]), written onto the job's
	// record by the attach naming its command ([LaunchRecord.FreshSessionID]).
	FreshSessionID string

	// Handed is the secrets the launch handed the box, sealed
	// ([PendingStore.Handed]): written beside the record by the attach naming the
	// command, the one write saying the job exists.
	Handed string

	// Brief is which brief the launch handed the job's run ([LaunchRecord.Brief]),
	// written with the command as Handed is, and NotContinued its verdict on the
	// session it did not continue ([LaunchRecord.NotContinued]).
	Brief        BriefKind
	NotContinued *executorkeep.NotContinued

	// BriefOmitted is what that brief left out of the round the launch carries on,
	// FreshOmitted what the fresh brief the box falls back to left out, beside a
	// session-continuing brief; BriefLateOmitted and FreshLateOmitted the late calls
	// each left out ([BriefTold], [LaunchRecord.BriefOmitted],
	// [LaunchRecord.FreshOmitted]): written with the command as Brief is.
	BriefOmitted, FreshOmitted         []int
	BriefLateOmitted, FreshLateOmitted []int

	// Leads is what that brief opened with of its own plus the earlier leads it told
	// again or was to and could not read ([BriefLeads]): taken into the run's leads
	// part by the attach naming the command before its row write, and marked on the
	// job's record in that write.
	Leads BriefLeads

	// Context is what the session handed the job's run carried at its newest request
	// the engine measured, along the copy's lineage and saying where from
	// ([continuedLaunch.measure]): written onto the job's record by the same attach
	// ([LaunchRecord.Context]) for the collection to keep where the job makes no
	// request of its own. Nil for a session of the run's own.
	Context *SessionMeasure

	// Restart is the newest place the session handed the job's run restarts at, as the
	// launch knew it ([continuedLaunch.restart]): written onto the record by the same
	// attach ([LaunchRecord.BasisRestart]) so a later relaunch can tell whether the
	// job restarted the session after its brief. Nil where unknown.
	Restart *string

	// Continued is the save point of the kept capture handed the job's run and that
	// capture's launch ([continuedLaunch.point]), written onto the record by the same
	// attach ([LaunchRecord.Continued]). Nil where the launch handed none or one
	// stating no point above zero.
	Continued *ContinuedPoint
}

// PriorBox is the box a relaunch found on its run's row ([PendingRun.Prior]).
type PriorBox struct {
	SandboxID string
	Placement string
	Harness   string
	// Status is the row's status before the reset: whether the job that used the box
	// is over ([StatusResumed], the relaunching resumed turn) or may still run in it.
	Status string
	// LaunchNode is the node that launched the job that used the box, whose host the
	// box is on ([PendingRun.LaunchNode]), the one node it may be continued in or
	// reclaimed by.
	LaunchNode string
	// Since is when the box was first attached ([PendingRun.BoxSince]): how old its
	// checkouts can be.
	Since time.Time
	// Handed is what the job that used the box was handed, sealed
	// ([PendingStore.Handed]): a job continuing in the box carries it forward, since
	// the box holds those values and the session it resumes their lines
	// ([Coordinator.sealHanded]).
	Handed string
	// Context is what the session left in the box carried at its newest measured
	// request, as that job's record held it ([LaunchRecord.Context]): a job
	// continuing that copy carries it forward on its own record, saying where from, so
	// a job making no request of its own is measured by its copy's lineage
	// ([continuedLaunch.measure]). Nil where the record held none.
	Context *SessionMeasure
	// Restart is the newest place that copy restarts at, as the read collecting the
	// job that used the box found it ([LaunchRecord.ResumedCount]): the basis a job
	// continuing the copy records ([LaunchRecord.BasisRestart]). Nil where that read
	// did not see the thread whole or by its release's rules, or the record held no
	// count.
	Restart *string
	// Entry is the providers.llm entry the job that used the box was launched
	// under ([LaunchRecord.Entry]): the one its copy of the session continues
	// under, where its harness is not launch-routed ([EntryContinues]).
	Entry string
	// Writer is the release that wrote that copy, as the job's record's count named it
	// ([LaunchRecord.CountRelease]): what the box's OpenCode point is paired with where
	// the copy is continued in place. "" where none was named.
	Writer string
	// Wakes are the wake items of the job that used the box that no step read, which
	// the reset removed ([openNotes]): that job is over, so the launch reports each
	// ([CoordinatorOptions.WakesEnded]), and its delivery runs as a turn of its own.
	Wakes []RunNote
}

// KeptBoxMaxAge is how old a turn's box may be, from its first attach
// ([PendingRun.BoxSince]), for a relaunch to still continue in it
// ([PriorBox.ContinuesIn]); an older one is reclaimed and the job starts fresh from the
// pushed branch. A BOX'S CHECKOUTS ARE AS OLD AS THE BOX and no one job bounds that
// (unbounded pause_ttl_seconds would keep one working as long as the turn). Gitlet keeps a deleted seat branch's engine commits gitlet.PruneGrace
// (fourteen days) and refuses a box's push carrying one it no longer holds
// (forged_committer); a job's credential lives gitlet/host's RunTokenTTL (seven days). So
// SEVEN DAYS, the grace less the credential; gitlet/host holds the three to that sum at
// compile time. EVERY KEPT BOX IS HELD TO IT, a run with no repository included, since a
// later job in the box may hold a grant the checking-out one lacked. A fresh box costs the
// provisioning and clone and a parked session, replaced by telling the job the question and
// answer ([LaunchRequest.Continue]).
const KeptBoxMaxAge = 7 * 24 * time.Hour

// ContinuesIn reports whether a launch of spec, at now, may continue in this box
// rather than a fresh one.
//
// ONLY THE SAME HARNESS IN THE SAME CELL, and only once the job that used it is
// over: a box is installed for one harness in one cell's backend, and one whose job
// may still run (a row reset from any status but the relaunching resumed turn's
// claim) is reclaimed, not shared, since two jobs in one checkout overwrite each
// other.
//
// AND ONLY A BOX YOUNGER THAN [KeptBoxMaxAge]; a box the row never dated has no age
// to hold to the bound and is not continued either.
func (p PriorBox) ContinuesIn(spec Spec, now time.Time) bool {
	return p.SandboxID != "" && p.Status == StatusResumed &&
		p.Placement == string(spec.Placement) && p.Harness == spec.Harness &&
		!p.Since.IsZero() && now.Sub(p.Since) < KeptBoxMaxAge
}

// ReachableFrom reports whether node can reach the box at all: one node launched
// ([PendingRun.LaunchNode]), whose process tree or container no other node touches.
// The launch's test of a box it continues in beside [PriorBox.ContinuesIn], and
// every reader's.
func (p PriorBox) ReachableFrom(node string) bool { return p.LaunchNode == node }

// Fence is the ownership token a mutation carries.
//
// A ZERO FENCE MEANS UNFENCED, deliberately: recovery writes and the boot pass
// legitimately have no lease yet, and a node with no seat host has no next holder
// to be fenced out by. What must never happen is a node writing under a lease it
// has LOST, which a non-zero fence closes. So the writes only a seat's holder makes
// (claim, take and record of an answer) refuse the zero fence too on a run any
// lease owns ([PendingStore.ClaimForResume]), and a node knowing it does not hold
// the seat never reaches them: its lease seam says so rather than answering the
// zero fence ([CoordinatorOptions.Lease], [ErrSeatNotHeld]).
type Fence struct {
	Owner string
	Epoch int64
}

// Fenced reports whether this token constrains anything.
func (f Fence) Fenced() bool { return f.Epoch > 0 }

// Suspension is what [PendingStore.MarkSuspended] writes: the suspended Execute
// loop and the turn iteration it suspended in.
//
// The iteration travels BESIDE the conversation, not read out of it: the
// conversation is opaque here by design (carried back to the engine undecoded) and
// reaching into it would be a second reader of a format this package does not own.
// Each launch's own record is keyed on it ([LaunchRecord]).
type Suspension struct {
	// State is the serialized loop, [PendingRun.ExecuteState].
	State json.RawMessage

	// Iteration is the turn iteration the executor suspended in, under which each
	// launch's own record is filed.
	Iteration int
}

// LaunchRecord is what one job's own record (`executor_collected`) needs that
// nothing else on the row says: when it started, which executor iteration launched
// it, its model, and whether that record was published.
//
// THE JOB'S, NOT THE TURN'S: the row outlives each job, so
// [PendingStore.BeginLaunch] replaces this record WHOLE on every launch, in the
// write naming the job ([PendingRun.LaunchID]). A surviving record would tell the
// next job it launched at the previous instant and was already published. One field
// rather than four for the same reason: a later fact is replaced with its job by
// construction.
type LaunchRecord struct {
	// StartedAt is when the job was launched, on the writing node's clock, written by
	// [PendingStore.BeginLaunch] as the launch exists. Another node reading it as an
	// age reads across two clocks, which may differ by up to twice
	// objstore.AssumedClockSkew.
	StartedAt time.Time `json:"started_at,omitzero"`

	// Started is whether the job's harness was started in its box: written by the
	// attach naming the command ([PendingStore.AttachSandbox]), once its job exists
	// and before the launch returns. It tells a brief's notes a job ran with from
	// ones read into the brief of a launch that failed before its job
	// ([PendingRun.EndedNotes]).
	//
	// ON THE JOB'S RECORD, NEVER CLEARED FOR THE JOB, and not read off the row's
	// command ([PendingRun.CommandID]), which is the BOX's: a park's teardown
	// ([PendingStore.ReleaseBox]) and an expired pause ([PendingStore.ExpirePause])
	// forget the box and command though the job ran. Read that way, a turn parked past
	// its pause reported its brief's notes expired after the runner recorded them
	// delivered.
	Started bool `json:"started,omitempty"`

	// Model is the model the harness was pointed at, empty when the launch named none
	// and the harness chose.
	Model string `json:"model,omitempty"`

	// ProviderKey is the providers.llm entry the job was launched on, the
	// executor-chain member that took it ([LaunchRequest.ProviderKey]). The job's
	// record and the resumed executor's are filed under it so spend by provider names
	// the entry that ran.
	ProviderKey string `json:"provider_key,omitempty"`

	// SignInID is the sign-in identity the harness was handed at launch: the entry's
	// harness SignInID, a keyed digest naming no credential. Empty where none.
	//
	// WHY THE ROW CARRIES IT: a credential replaced while a job runs cannot reach it
	// (a process's environment cannot change), so the job can end refused on the OLD
	// credential while the entry signs in with the new one. Comparing this with the
	// entry's identity at collection tells that ending from an ordinary refusal on
	// whichever node collects; the fleet keyring keys it so it compares across nodes
	// and restarts. Empty reads as "not recorded", which relaunches nothing, the safe
	// direction.
	SignInID string `json:"sign_in_id,omitempty"`

	// Entry is the providers.llm entry the job was launched under
	// ([LaunchRequest.Entry]): what every capture of the job names its session
	// as having run under ([SessionCapture.Entry]), and what a relaunch
	// continuing the job's copy in its box is held to ([PriorBox.Entry]).
	Entry string `json:"entry,omitempty"`

	// FollowUp is why this job launched as a follow-up of the previous job in the
	// round, where the collection acts on it ([FollowUpReason]); empty otherwise.
	FollowUp FollowUpReason `json:"follow_up,omitempty"`

	// Brief is which of three briefs the launch handed the job's run: continuing the
	// turn's session in the box last worked in, continuing it in a replacement box, or
	// for a session of its own (restore.go). Written by the attach naming the command
	// ([PendingStore.AttachSandbox]) for the record its collection publishes beside
	// whether the run continued the session, which only its box can say
	// ([Result.Continued]).
	Brief BriefKind `json:"brief,omitempty"`

	// NotContinued is the launch's verdict on the newest session its turn or
	// conversation left that its run does not continue
	// ([LaunchRequest.NotContinued]): written by the same attach as Brief, and
	// published on the job's `executor_launched` and `executor_collected`
	// events, the latter turning a box that fell back to a fresh session into
	// [executorkeep.LossRestoreFailed] ([finalNotContinued]). Nil where it lost
	// none. Fitted ([executorkeep.NotContinued.Fit]) before the row carries it.
	NotContinued *executorkeep.NotContinued `json:"not_continued,omitempty"`

	// BriefOmitted is the reads of the carried-on round that the job's brief counted
	// without listing, and FreshOmitted those the fresh brief the box falls back to
	// counted so, beside a session-continuing brief: by place in the round's log
	// ([BriefTold]), written with Brief where the launch carried a round on. Read
	// through [LaunchRecord.OmittedCalls] by the brief the run read.
	//
	// ON THE JOB'S RECORD, because what a session holds is what its briefs TOLD: the
	// box's copy, and a capture placed by its own read, hold those reads only as a
	// count, and a relaunch continuing either reads the record long after this launch,
	// on any node; the next launch replaces it whole once read. Never on the executor
	// log, whose turn row carries no save point to read a set against.
	BriefOmitted []int `json:"brief_omitted,omitempty"`
	FreshOmitted []int `json:"fresh_omitted,omitempty"`

	// BriefLateOmitted and FreshLateOmitted are the late calls each brief counted
	// without listing ([BriefTold.LateOmitted]), apart from the reads it owes, on the
	// record for the same reason. Read through [LaunchRecord.LateOmittedCalls].
	BriefLateOmitted []int `json:"brief_late_omitted,omitempty"`
	FreshLateOmitted []int `json:"fresh_late_omitted,omitempty"`

	// LeadMarks is the leads the run's brief told (its own and every earlier one told
	// again or unreadable, [BriefTold.ToldUnread]) by the number the run's leads part
	// gave each ([KeptLead.Appended]), ascending: written by the attach naming the
	// command once the part took the brief's own ([PendingStore.AttachSandbox]),
	// cleared by the reset opening the next job. Each names a kept lead, so at most
	// [MaxKeptLeads].
	//
	// ON THE JOB'S RECORD like the omitted reads: a continuation of the session that
	// may lack its brief tells the leads that brief told again
	// ([LaunchRecord.ToldLeads]), read beside the row long after the launch, on any
	// node.
	LeadMarks []int `json:"lead_marks,omitempty"`

	// FreshSessionID is the session of its own the job's script starts in the
	// continued one's place, where its launch continued one and the box neither held
	// nor restored it ([RunHandle.FreshSessionID]); empty where none was continued.
	// Written with the command like Brief: with SessionStart it names the session the
	// box ran ([PendingRun.SessionRan]).
	FreshSessionID string `json:"fresh_session_id,omitempty"`

	// SessionStart is which session the job's box said it started, where its launch chose to
	// continue one: that session, or the job's own after a failed restore. Said by WHICH of
	// the job's two bridge tokens its harness dialled (internal/api/mcpbridge), written once
	// ([PendingStore.MarkSessionStart]): the one witness the run's model cannot forge, since
	// the other session's token is gone before the harness starts, while the record the
	// script writes is a file the model can rewrite ([Result.FellBack]).
	//
	// WHAT EVERY LATER REBUILD OF THE SKILL GUARD READS: the skills armed on the chosen
	// session are the job's only where this is [SessionStartContinued]. Empty reads as a
	// session of the job's own holding none, the safe direction (a short guard asks for a
	// reload; one holding an unread skill lets a call through). Also names the session the
	// box ran ([PendingRun.SessionRan]).
	SessionStart SessionStart `json:"session_start,omitempty"`

	// Minted is the session a minting harness's job reported and the coordinator bound to it
	// ([PendingStore.BindSession]): once, a fresh id or the one its launch continued; the
	// job's session wherever it is read ([PendingRun.SessionRan]). Empty until a report is
	// bound, and for a harness that names its session as its run starts.
	Minted string `json:"minted,omitempty"`

	// ResumeCarry is what the turn resuming from this job's collection does with its round,
	// said as that turn began ([PendingStore.MarkResumeBegun], [ResumeRequest.Begin]):
	// collects it, or carries it on. The claim's own: cleared by the release handing the
	// claim back. Empty before the turn begins and on a person's answer, which carries the
	// round on ([PendingRun.Answer]). FOR EVERY READER OF WHETHER A RELAUNCH WOULD CONTINUE
	// THE JOB'S SESSION ([ReadContinuityOf]): the row says nothing else of its turn from claim
	// to end. Best effort: a missed record reads as undecided, never as either.
	ResumeCarry ResumeCarry `json:"resume_carry,omitempty"`

	// Iteration is the turn iteration the launching executor suspended in, written by
	// [PendingStore.MarkSuspended].
	Iteration int `json:"iteration,omitempty"`

	// Published is whether the job's own `executor_collected` record went out.
	//
	// ONCE PER JOB, on the row, for [PendingRun.Charged]'s reason: the publish sits in
	// the retried part of the completion tail, the retry may run on another node or
	// after a restart, and every reader (spend rollup, each data node's daily usage)
	// counts the record as spend, so a second copy is a second charge everywhere.
	// WRITTEN BY THE RELEASE ([Release.Published]), the one write a retry reaches the
	// publish through.
	Published bool `json:"published,omitempty"`

	// CollectFailures is how many collections of this job failed to read its box back
	// SINCE THE LAST ONE THAT DID, and CollectFailingSince when the first failed: the
	// run of consecutive failures [Coordinator.OnCompleted] bounds before giving the job
	// up as unreachable ([Coordinator.collectFailed]). A collection that read the box
	// ends the run ([Release.Collected]).
	//
	// ON THE JOB'S RECORD for Published's reason: each failure hands the claim back, the
	// retry may run on another node or after a restart, and a memory count would grant
	// every node a fresh allowance. WRITTEN BY THE RELEASE ([Release.CollectFailedAt]).
	CollectFailures     int       `json:"collect_failures,omitempty"`
	CollectFailingSince time.Time `json:"collect_failing_since,omitzero"`

	// ResumeFailures is how many resumes of this job's turn failed, and
	// ResumeFailingSince when the first failed: the run [Coordinator.resumeAndSettle]
	// bounds before settling the run ([ResumeGiveUp]). A resume held on a condition is
	// a wait and counts nothing.
	//
	// ON THE JOB'S RECORD for CollectFailures' reason: a memory count granted every
	// attempt a fresh allowance, and an always-failing resume was retried on every poll
	// as long as the engine ran. WRITTEN BY THE RELEASE ([Release.ResumeFailedAt]).
	ResumeFailures     int       `json:"resume_failures,omitempty"`
	ResumeFailingSince time.Time `json:"resume_failing_since,omitzero"`

	// RelaunchRefusedIn is the budget window the last relaunch a resume of this job's turn
	// tried was refused below its floor in ([Refusal.Window]). The next relaunch reads it to
	// know it waited that window out, so in a later window it is held to the floor of a
	// brief-less launch (internal/engine's launchfloor.go) and waits one turnover at most;
	// else its floor, past what a busy window leaves at the read, would go out in none. ON
	// THE JOB'S RECORD, WRITTEN BY THE RELEASE ([Release.RelaunchRefusedIn]), like
	// ResumeFailures: the retry may run on another node. Kept by a release naming none,
	// replaced with the job by the relaunch; a record holding none reads as never refused.
	RelaunchRefusedIn BudgetWindow `json:"relaunch_refused_in,omitzero"`

	// Collected is whether a collection READ THIS JOB'S BOX BACK and handed what it
	// found to the job's turn, whose resume then gave the claim back (held by the seat's
	// budget, or to retry; [Release.Collected]). The row returns to running, but the job
	// is over, its spend the resume's to charge, and what runs for it is that resume, a
	// turn the seat's fence stops at its first round. So a stop leaves it
	// ([Stoppable]): ended by the stop instead, the turn's close charged the work item
	// the job's spend a second time under the stop's segment, atop the resume's. The stop
	// ends what that resume waits on ([Coordinator.endCollectedWait]).
	//
	// ON THE JOB'S RECORD, written by the release like CollectFailures, never cleared for
	// the job: a later collection unable to read the box does not put the job back out.
	Collected bool `json:"collected,omitempty"`

	// CollectedAt is when the collection that FIRST read this job's box back read it, on
	// that node's clock: what every later collection reads the job's ending against
	// ([RunEnding.At]), so a reset its harness named as a time of day ("resets 4:30pm")
	// is that instant, not the next 4:30pm after a resume held for hours. Written by the
	// release with Collected ([Release.CollectedAt]), never moved for the job; zero for a
	// job no release handed back collected.
	CollectedAt time.Time `json:"collected_at,omitzero"`

	// EndedAtDeadline says a collection SAW THIS JOB STILL WORKING past its turn's
	// wall-clock cap ([PendingRun.Deadline]) (its poll answered that it ran, or its
	// script heard the request to end) and ended it ([Coordinator.readBox]): written the
	// moment it is seen, before it is asked to end or killed
	// ([PendingStore.MarkEndedAtDeadline]), so every later collection reads the run as
	// cut off by its cap ([endedAtDeadline]) whatever its clock reads.
	//
	// ON THE JOB'S RECORD because the end can leave nothing else saying so: a read cut
	// short as it ends the job kills it, possibly before its script records the request
	// ([Result.JobEnd]), so a collection made again found a job its cap stopped with no
	// reason, read it as ended by itself, and parked it on its dying question.
	EndedAtDeadline bool `json:"ended_at_deadline,omitempty"`

	// HostStoreSilentSince, where set, says the host of this job's box last answered a
	// collection that its own store did not answer its read of the row
	// ([BoxReadReply.Unsettled]), and is when the collecting node's clock on that store
	// started: the durable twin of that clock ([hostUnsettled]), written and cleared by
	// claim releases ([Release.HostStoreSilentSince], [Release.HostStoreAnswered]). An
	// instant, not a flag, since an announcement from a read asked after it ends it
	// ([announcedAfter]). On the record because the clock is one node's memory and the
	// node next acknowledging a delivery reads it (renew.go). See the package doc,
	// "Unannounced jobs".
	HostStoreSilentSince time.Time `json:"host_store_silent_since,omitzero"`

	// RefusalOwed, where set, says the seat's holder acknowledged a delivery of this
	// job's completion that it refused on this condition and owes its re-fire;
	// RefusalOwedSince is when, on that holder's clock. Both or neither. Written by
	// [PendingStore.OweRefusal] and by a release holding the resume ([Release.Hold]),
	// only where none is owed; cleared by the write settling the completion. On the
	// record because the holder's memory does not survive its restart or the seat's move
	// ([Coordinator.RecoverSeat]). An obligation, never a hold, though a save's license
	// reads it as the completion announced ([PendingRun.jobOver]); an older build keeps
	// both keys in [LaunchRecord.Extra]. See the package doc, "Re-arming owed refusals
	// and holds".
	RefusalOwed      Condition `json:"refusal_owed,omitempty"`
	RefusalOwedSince time.Time `json:"refusal_owed_since,omitzero"`

	// HarnessEnd is how the job's harness ended, as the read collecting the job found it
	// ([Coordinator.readBox], [PendingStore.MarkHarnessEnd]): its own run, or cut off, by the
	// line its capture is placed by ([readCapture]). What a relaunch carrying the round on IN
	// ITS BOX reads (harnessend.go): a copy whose harness ended its own run holds every call
	// the job made; any other may lack calls and the brief, so that session is told every call
	// of the round and every note read. EMPTY IS NEVER AN OWN RUN ([HarnessEnd.EndedOwnRun]):
	// the safe direction, told calls it may hold rather than never told one it lacks. Cut off
	// once, cut off for the job ([HarnessEnd.after]).
	HarnessEnd HarnessEnd `json:"harness_end,omitempty"`

	// Context is what the session the job's box holds carried at its newest request the engine
	// measured (harnessend.go): written by the attach as the measure of the session handed the
	// run, carried along the copy's lineage ([BoxRef.Context]), and by the collecting read, in
	// its one write with HarnessEnd, as the job's newest request. What a relaunch continuing the
	// copy holds its floor to (internal/engine's continuedFloor; [PriorBox.Context]). Nil where
	// nothing measured the copy. ONLY A SOURCE OF [MeasureRequest] NAMING THIS JOB IS A FRESH
	// MEASURE; every other is carried and says from where ([MeasureSource]).
	Context *SessionMeasure `json:"session_context,omitempty"`

	// ResumedCount is what the harness counts the copy of the session the job's box holds
	// at before its next request (resumed.go), as the read collecting the job found it in
	// one read of the job's main thread (mainthread.go), written with HarnessEnd in that
	// read's one write ([PendingStore.MarkHarnessEnd]): what a relaunch continuing the
	// copy in the box judges its first request by (internal/agent/runner's firstRequest)
	// and, where a kept capture of the job is continued, what the capture's own count is
	// held beside. NEVER CARRIED: the next launch's record starts with none. Nil where
	// nothing read the copy (another harness, an older build's job, an unlanded write),
	// each read as a copy that may compact.
	ResumedCount *ResumedCount `json:"resumed_count,omitempty"`

	// CountProvider is the provider of the reply an OpenCode ResumedCount is anchored on,
	// as the same read found it: the count is OpenCode's estimate for the provider that
	// answered, to which a continuation on another provider's model is not held. BESIDE
	// THE COUNT, NEVER IN IT: ResumedCount is closed, so a later build's knowledge of a
	// count rides a key of the record holding it, which an older build's write carries in
	// Extra. Written and cleared where the count is ([LaunchRecord.counted]), NEVER
	// CARRIED. "" for a Claude Code count (no anchoring reply) and an older build's
	// record. Held to [ValidCountProvider].
	CountProvider string `json:"count_provider,omitempty"`

	// CountRelease is the release of the harness whose thread ResumedCount read
	// ([Result.HarnessVersion]): the writer of the copy the box holds, which a relaunch
	// continuing it pairs the box's OpenCode point with ([PriorBox.Writer]). On the
	// record, not only a capture's stamp, since a company keeping no session takes none.
	// Written and cleared where the count is ([LaunchRecord.counted]), NEVER CARRIED, held
	// to [ValidRelease]. "" where no read named a release and on an older build's record:
	// no pair, the copy told as a summary.
	CountRelease string `json:"count_release,omitempty"`

	// NextRequestTokens and NextRequestOlderTokens are the harness's own estimate of the
	// copy's next request at each of the two rates Claude Code counts text at
	// ([NextRequest], [LaunchRecord.NextRequest]), as the read collecting the job found it
	// in the same main-thread read as ResumedCount, written with HarnessEnd in its one
	// write ([PendingStore.MarkHarnessEnd]): what a relaunch continuing the copy is held
	// to with its brief (internal/engine's continuedFloor), where the newest request's
	// input (Context) under-counts it. NEVER CARRIED. Zero where nothing estimated it
	// (another harness, a thread not read whole, an older build's job, an unlanded write),
	// each read as Context.
	NextRequestTokens      int64 `json:"next_request_tokens,omitempty"`
	NextRequestOlderTokens int64 `json:"next_request_older_tokens,omitempty"`

	// BasisRestart is the newest place the session handed the run restarts at
	// ([ResumedCount.Restart]), as the launch knew it when opening the job: for the copy
	// the previous job left in the box, that job's count; for a kept capture, its own;
	// for a session of the run's own, a thread restarting nowhere, "". NIL WHERE IT IS
	// NOT KNOWN, which is not "": a basis whose count did not read its thread whole or by
	// its release's rules, the session the conversation's last turn left (no count
	// describes it), and an older build's record. A job whose own copy's newest restart,
	// read whole, is not this one restarted after its brief, or may have
	// ([RestartedSince]).
	BasisRestart *string `json:"basis_restart,omitempty"`

	// Continued is the save point of the kept capture handed the run and that capture's
	// launch ([ContinuedPoint]), written with BasisRestart by the attach naming the
	// command, from the same capture: what a capture of this job no read places is raised
	// to once its take names the newest compaction BasisRestart does (savepoint.go). Nil
	// for a launch that opened its round, the box's copy continued in place, the
	// conversation's base, a session of the run's own, a kept capture stating no point or
	// zero, and an older build's record.
	Continued *ContinuedPoint `json:"continued,omitempty"`

	// Bridged is what this job's BRIDGED calls cost the engine (auxiliary calls the
	// seat's tools made for the harness and its delegated workers) as the newest total the
	// bridge wrote with its calls ([PendingStore.AppendBridgeCall]). The segment resuming
	// from the job pays it to the turn's work item (ADR-0022), since no segment's tally is
	// open while a job is out.
	//
	// ON THE JOB'S RECORD as every fact here: the resume may run on another node after a
	// restart, and a relaunch must not hand one job's spend to the next.
	Bridged EngineSpend `json:"bridged_spend,omitzero"`

	// Condensed is what condensing the collection that PARKED this job on a question cost
	// the seat's auxiliary model ([Result.Condensed]), written with the question
	// ([PendingStore.MarkAwaiting]) and paid by the answer's resume, like
	// [PendingRun.ParkedInputTokens].
	Condensed AuxTokens `json:"parked_condensed,omitzero"`

	// Extra is every key of the job's record THIS BUILD DOES NOT KNOW, kept verbatim and
	// written back by every write that is not a relaunch's (a later build's fact survives
	// this build's status flips, leases, bridged calls and notes, wirekeys.go) and dropped
	// with the record by the relaunch replacing it whole, since it is the job's.
	Extra map[string]json.RawMessage `json:"-"`
}

// ContinuedPoint is the save point of the kept capture a launch handed its run
// ([SessionCapture.Through]) and that capture's launch ordinal
// ([LaunchRecord.Continued]); no decision reads the capture's own place. CLOSED
// like [ResumedCount]: a later build adds a record key beside it, never a field, so
// it has no Extra. A LaunchSeq or Through at or below zero reads as none.
type ContinuedPoint struct {
	LaunchSeq int64 `json:"launch_seq"`
	Through   int   `json:"bridge_through"`
}

// OmittedCalls is the reads of the round the job run's brief counted without listing
// ([LaunchRecord.BriefOmitted], [BriefTold]), by which brief the launch handed
// ([LaunchRecord.Brief]) and which session the box said it started
// ([LaunchRecord.SessionStart]): a fresh brief's own set; a continuing brief the box took
// up, that brief's; a box that started its own session after a failed restore, the fresh
// brief's; and where the box never said or a brief is unknown, BOTH (the union), told again
// rather than never. Nil where the brief left none out.
func (l LaunchRecord) OmittedCalls() []int { return l.readBriefs(l.BriefOmitted, l.FreshOmitted) }

// LateOmittedCalls is the late calls the job run's brief counted without listing
// ([LaunchRecord.BriefLateOmitted], [BriefTold.LateOmitted]), resolved as
// [LaunchRecord.OmittedCalls] resolves the reads: the union where the record cannot
// say which brief was read, crediting the session no telling of either's.
func (l LaunchRecord) LateOmittedCalls() []int {
	return l.readBriefs(l.BriefLateOmitted, l.FreshLateOmitted)
}

// readBriefs is brief's set or fresh's per [LaunchRecord.OmittedCalls], both where the
// record cannot say which the run read.
func (l LaunchRecord) readBriefs(brief, fresh []int) []int {
	switch {
	case l.Brief == BriefFresh:
		return brief
	case l.Brief.Continues() && l.SessionStart == SessionStartContinued:
		return brief
	case l.Brief.Continues() && l.SessionStart == SessionStartFresh:
		return fresh
	}
	union := slices.Clone(brief)
	for _, place := range fresh {
		if !slices.Contains(union, place) {
			union = append(union, place)
		}
	}
	slices.Sort(union)
	return union
}

// SessionRan is the harness session the row's job worked in, for a relaunch carrying its
// round on in that box to continue; "" where the row cannot say. FROM WHAT THE LAUNCH NAMED AND THE TOKEN THE BOX DIALLED, NOTHING ELSE: the launch's
// session where it continued none, the continued one where the box took it up, the job's
// own ([RunHandle.FreshSessionID]) where the box started that, per
// [LaunchRecord.SessionStart]. Never a file in the box, which the run's model could
// rewrite to name a transcript of its own. UNKNOWN WHERE THE BOX'S WORD NEVER REACHED THE
// ROW: a relaunch then continues neither by this word but from the turn's kept capture or
// a session of its own, since continuing in place would hand the guard skill loads made
// perhaps in the other session. THIS IS THE IN-PLACE RELAUNCH'S WORD ALONE: a kept capture
// names its session from the box's own record ([SessionCapture.SessionID]) and continues
// it in whichever box it gets. A minting job ran the one bound where it continued none or
// its box started its own ([LaunchRecord.Minted]); the token's word stands over it.
func (r PendingRun) SessionRan() string {
	switch r.Launch.SessionStart {
	case SessionStartFresh:
		if r.Launch.Minted != "" {
			return r.Launch.Minted
		}
		return r.Launch.FreshSessionID
	case SessionStartContinued:
		return r.SessionID
	}
	switch {
	case r.Launch.FreshSessionID != "":
		return ""
	case r.SessionID == "":
		return r.Launch.Minted
	}
	return r.SessionID
}

// spentBy is how a job of this turn spent session, as its collection read the
// job's end ([sessionSpent]), "" where none did. A row a newer build wrote may
// name another loss, read as a value.
func (r PendingRun) spentBy(session string) executorkeep.Loss {
	if session == "" {
		return ""
	}
	for _, spent := range r.SpentSessions {
		if spent.Session == session {
			return spent.Loss
		}
	}
	return ""
}

// SessionStart is which session a job's box said it started, where its launch
// chose to continue one ([LaunchRecord.SessionStart]).
type SessionStart string

const (
	// SessionStartContinued is the session the launch chose: the box held it or
	// restored the archive carried into it, and its harness took it up.
	SessionStartContinued SessionStart = "continued"

	// SessionStartFresh is a session of the job's own, which its script started in
	// place of the chosen one where the box neither held nor could restore it
	// (archive failed to unpack, import refused by OpenCode, release too old,
	// [Result.FellBack]).
	SessionStartFresh SessionStart = "fresh"
)

// Valid reports whether s is a session a box can say it started. A newer build's
// record may name an unknown one, read as the value it is and as no continued
// session.
func (s SessionStart) Valid() bool { return s == SessionStartContinued || s == SessionStartFresh }

// ResumeCarry is what the turn resuming from a collected job does with the job's
// round, said as it begins ([LaunchRecord.ResumeCarry]).
type ResumeCarry string

const (
	// ResumeCollects is a turn that goes on from the job's work: its review, whose
	// send-back is the round's only relaunch, continuing no session in the job's box.
	ResumeCollects ResumeCarry = "collects"

	// ResumeCarriesOn is a turn that relaunches the round at once rather than
	// collecting it: a job lost with its host, or refused on a sign-in replaced while
	// it ran, whose round was not yet submitted.
	ResumeCarriesOn ResumeCarry = "carries_on"
)

// ResumeCarries are every carry a turn says.
var ResumeCarries = []ResumeCarry{ResumeCollects, ResumeCarriesOn}

// Valid reports whether c is one this build names. A newer build's record may name
// another, read as a turn that began whose carry this build cannot say.
func (c ResumeCarry) Valid() bool { return slices.Contains(ResumeCarries, c) }

// FollowUpReason is why a job was launched as a follow-up of the job before it in
// the same round, where the new job's collection has to know.
type FollowUpReason string

const (
	// FollowUpSignInReplaced is a job relaunched because the job before it was
	// refused by its vendor on a sign-in replaced while it ran. ONCE: refused again,
	// it is an ordinary refusal, so a credential that keeps failing fails the turn
	// rather than relaunching for ever.
	FollowUpSignInReplaced FollowUpReason = "sign_in_replaced"
	// FollowUpRequestRefused is a job relaunched in a fresh session because the
	// provider refused the request the job before it made in a session it continued.
	// ONCE: refused again, it is collected and its round ends the turn.
	FollowUpRequestRefused FollowUpReason = "request_refused"
)

// Valid reports whether the reason is one this build names.
func (r FollowUpReason) Valid() bool {
	return r == FollowUpSignInReplaced || r == FollowUpRequestRefused
}

// AuxTokens is what calls to a seat's AUXILIARY model cost, split as every engine
// token figure is: the whole input, the share the provider's prompt cache served
// and stored (a breakdown, never an addition), and the output.
type AuxTokens struct {
	Input      int `json:"input,omitempty"`
	Output     int `json:"output,omitempty"`
	CacheRead  int `json:"cache_read,omitempty"`
	CacheWrite int `json:"cache_write,omitempty"`
}

// Plus is a and b together.
func (a AuxTokens) Plus(b AuxTokens) AuxTokens {
	return AuxTokens{Input: a.Input + b.Input, Output: a.Output + b.Output,
		CacheRead: a.CacheRead + b.CacheRead, CacheWrite: a.CacheWrite + b.CacheWrite}
}

// newest is the larger of a and b in each figure — see [EngineSpend.Newest].
func (a AuxTokens) newest(b AuxTokens) AuxTokens {
	return AuxTokens{Input: max(a.Input, b.Input), Output: max(a.Output, b.Output),
		CacheRead: max(a.CacheRead, b.CacheRead), CacheWrite: max(a.CacheWrite, b.CacheWrite)}
}

// EngineSpend is what the ENGINE spent on a job outside every segment of its
// turn: its own auxiliary calls and the workers it ran for the job. The executor's
// OWN tokens are not here; they are the job's, reported by its harness
// ([Result.InputTokens]).
//
// Two things put it there, both while no segment runs to tally them: a run's
// bridge calls, which continue after the launching segment ended, and the
// condensation of a collected run's account ([Condenser]) made between segments.
// The segment resuming from the job pays it ([ResumeRequest.Engine]).
type EngineSpend struct {
	Aux AuxTokens `json:"aux,omitzero"`
	// Workers is how many delegated workers ran, and WorkerInput and
	// WorkerOutput what they cost between them.
	Workers      int `json:"workers,omitempty"`
	WorkerInput  int `json:"worker_input,omitempty"`
	WorkerOutput int `json:"worker_output,omitempty"`
}

// Plus is s and o together.
func (s EngineSpend) Plus(o EngineSpend) EngineSpend {
	return EngineSpend{Aux: s.Aux.Plus(o.Aux), Workers: s.Workers + o.Workers,
		WorkerInput: s.WorkerInput + o.WorkerInput, WorkerOutput: s.WorkerOutput + o.WorkerOutput}
}

// Newest is the larger of s and o in each figure: of two totals one meter took at
// different moments, the later, whichever was WRITTEN later.
//
// The bridge writes its meter's running total with every call, and two calls
// finishing together race to the row, so the earlier call's total can land after
// the later's. Figures only grow within one meter, so the larger is the later and
// this merge never moves a total backwards.
func (s EngineSpend) Newest(o EngineSpend) EngineSpend {
	return EngineSpend{Aux: s.Aux.newest(o.Aux), Workers: max(s.Workers, o.Workers),
		WorkerInput: max(s.WorkerInput, o.WorkerInput), WorkerOutput: max(s.WorkerOutput, o.WorkerOutput)}
}

// Held reports whether the row's resume is held back at now ([PendingRun.HeldFor]).
func (r PendingRun) Held(now time.Time) bool {
	return r.HeldFor != "" && now.Before(r.HeldUntil)
}

// Handle is the job this row holds now, as every read of its box is handed it:
// command and session.
func (r PendingRun) Handle() RunHandle {
	return RunHandle{CommandID: r.CommandID, SessionID: r.SessionID}
}
