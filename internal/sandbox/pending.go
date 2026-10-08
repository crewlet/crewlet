package sandbox

import (
	"context"
	"encoding/json"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/events/types"
)

// The durable state of a detached coding job.
//
// A run OUTLIVES its kick-off turn, its process, and sometimes its node. What
// survives is this row: the completion turn rebuilds everything it needs from
// it, and a startup pass re-attaches to still-live boxes after a restart —
// without which a restart orphans a box that runs to its TTL, bills for every
// second, and hands its result to nobody.

// The run's state machine. Small on purpose: every state below is one a
// RECOVERY pass has to be able to act on, and a state nobody recovers from is
// a state that leaks a box.
//
// A RUN THAT IS OVER HAS NO STATE, BECAUSE IT HAS NO RECORD. Settling a run,
// done or failed, deletes its record ([PendingStore.Finish]) once its box is
// reclaimed. The record lives in an ageless bucket, since a parked run can
// wait days and a bucket age would reap the only thing that knows a billed box
// exists, and every completion poll and seat recovery reads the whole bucket.
// A terminal status kept there would be read on every one of those passes for
// the life of the deployment by readers that all skip it, while what a run
// ended as is already on the record that outlives it: the phase events of the
// resumed turn, or its sandbox_run_failed announcement.
const (
	// StatusLaunching — the job has been started, but the turn has not yet
	// written the conversation a resume re-enters. The seat is BUSY and a
	// box exists; what does not exist yet is anything to resume INTO.
	//
	// This state is the launch's two halves made visible, and it exists
	// because they are not one write. [Launch] starts a detached process
	// and returns; the suspended Execute conversation is serialized onto
	// the row only once the turn's frame unwinds, which is milliseconds
	// later on an idle host and hundreds of milliseconds on a loaded one.
	// A completion fired inside that window was claimed against a row with
	// no ExecuteState, and the coordinator — finding nothing to resume —
	// FAILED the run: a coding job that finished too fast destroyed the
	// agent's whole turn, permanently, with no retry. The window is not
	// theoretical, it is measured: a job that completes in ~100 ms against
	// an unwind that takes longer is the ordinary case for a trivial run.
	//
	// So the row says "launching" until the conversation is on it, and it
	// is [PendingStore.MarkSuspended] — one write, both facts — that makes
	// the run pollable and claimable at the same instant.
	StatusLaunching = "launching"

	// StatusRunning — the job is executing, the suspended conversation is
	// on the row, and the seat is BUSY.
	StatusRunning = "running"

	// StatusAwaiting — the agent asked a person and stopped. The seat is
	// FREE: a clarification can wait days, and holding a seat closed for
	// one would stop every other thing that seat does.
	StatusAwaiting = "awaiting_clarification"

	// StatusAnswered — a person's reply to the question is RECORDED on the
	// row and the resume it drives is owed. Nobody is being waited on any
	// more, so it is not [Awaiting]; nothing is running yet, so it does not
	// hold the seat either — what keeps the seat's later mail behind the
	// resume is an inbox hold the coordinator takes for as long as the
	// answer is owed (see [Coordinator.TryResumeFromAnswer]).
	//
	// ITS OWN STATUS rather than a field on an awaiting row, because the
	// fact it records — this question has its answer — is what every reader
	// that matches, reaps or lists questions has to see: the answer match
	// lists [Awaiting] rows only, so it can never take a later reply as the
	// answer to a question that already has one, without each reader having
	// to remember a field beside the status.
	StatusAnswered = "answered"

	// StatusResumed — the tail has been claimed. THE AT-MOST-ONCE GATE.
	//
	// A claim is not a resume: the row reads resumed from the claim to its
	// settle, and a turn may or may not have run in between. For a claimed
	// ANSWER the row says which ([RecordedAnswer.TakenAt]), because that is
	// what decides whether the reply is still owed to the seat when the
	// claim dies with its process.
	StatusResumed = "resumed"

	// StatusReseed — a paused box was reaped past its pause TTL. The run is
	// NOT over: the answer can still arrive, and the work re-seeds from the
	// pushed branch rather than from a snapshot that no longer exists.
	StatusReseed = "reseed"
)

// Claimable are the statuses whose tail has not run yet.
//
// Reseed belongs here, and that is the whole reason it is a state rather than
// a flag: reaping the box does not end the run.
//
// LAUNCHING IS DELIBERATELY ABSENT. A claim is the promise that a resume can
// follow it, and a launching row has no conversation to resume — claiming one
// is exactly the mistake [StatusLaunching] exists to make impossible.
//
// ANSWERED IS HERE because recording an answer is not resuming with it: the
// resume claims the row out of [StatusAnswered], and a resume that fails hands
// the claim back there, for the coordinator's own retry rather than for a
// redelivery of the person's message.
var Claimable = []string{StatusRunning, StatusAwaiting, StatusReseed, StatusAnswered}

// Tail is what a claim expects to find on a run: the job the signal is about,
// and the statuses that signal may take the tail out of.
//
// A CLAIM NAMES WHAT IT CLAIMS, because the two signals that take a tail are
// about different moments of one row. A completion says a job finished, which
// is only ever true of a RUNNING row still holding that job; an answer says a
// parked question was answered, which is only ever true of an [Awaiting] row
// holding the job that asked it. A claim that took any claimable status took
// whichever the row happened to be in when the signal arrived. A duplicate
// completion therefore re-collected a run that had parked on its question and
// asked it again, and one that outlived its own job claimed the next job
// while that one was still running: the turn resumed on a half-written result
// and the settle tore the box down under the job.
type Tail struct {
	// Launch is the [PendingRun.LaunchID] the signal was raised for,
	// matched exactly, the empty value included.
	Launch string

	// From is the statuses the signal may claim out of. A status outside
	// [Claimable] is never claimed, whatever this says.
	From []string
}

// CompletionTail is what a completion of one job claims: that job, while it
// is running. The poll only fires on a running row, so a completion that
// finds its job in any other status is a duplicate of one that already took
// it.
func CompletionTail(launch string) Tail {
	return Tail{Launch: launch, From: []string{StatusRunning}}
}

// RecordedAnswerTail is what the resume of a RECORDED answer claims: the job
// that asked, once its reply is on the row. See [StatusAnswered]. It is the one
// tail an answer claims, on either route: a chat reply and an answer by turn are
// both recorded on the run before anything is done with them.
func RecordedAnswerTail(launch string) Tail {
	return Tail{Launch: launch, From: []string{StatusAnswered}}
}

// Release is how a claimed tail is handed back for its signal's retry: the
// claim it hands back, where to, and what the claim already did.
//
// A RELEASE NAMES WHAT IT RELEASES, for the reason a claim names what it
// claims. The claim is held across the resume, and the resumed turn is free to
// move the row on: an executor that calls run_sandbox again opens a new launch
// on it, and a seat's next owner reaps a claim its previous owner abandoned. A
// failed resume that put back whatever the row held by then reverted the new
// launch to running, with no box or no conversation, or revived a run already
// reaped and announced lost.
type Release struct {
	// Launch is the [PendingRun.LaunchID] the claim took, matched exactly.
	Launch string

	// To is the status the claim took the run out of, which is one of
	// [Claimable].
	To string

	// Charged is whether the run's spend is on the token counter, recorded
	// on the row by the release itself. See [PendingRun.Charged].
	Charged bool

	// CompanyCharged is whether the company's share of it is, where the
	// seat's is not, recorded the same way. See [PendingRun.CompanyCharged].
	CompanyCharged bool

	// Published is whether the run's own phase record went out, recorded
	// the same way and for the same reason. See [LaunchRecord.Published].
	Published bool

	// CollectFailedAt, when set, is the instant a collection of this job
	// could not read its box back: the release counts it onto the job's
	// record ([LaunchRecord.CollectFailures]), dating the run of failures
	// from the first. Zero for a release that is not a failed collection.
	CollectFailedAt time.Time

	// Collected is whether the claim being handed back READ THE JOB'S BOX
	// BACK — a collection that succeeded, whose park or resume then failed
	// — and it ENDS the job's run of failed collections: the release clears
	// [LaunchRecord.CollectFailures] and [LaunchRecord.CollectFailingSince].
	//
	// The bound is on CONSECUTIVE failures, the waiter's own rule (a
	// reconnect that succeeds clears its streak), and nothing else wrote the
	// end of one: a box that failed once, was then collected, and was handed
	// back because its resume failed kept the count and the first failure's
	// instant, so the next single failure of its re-collection was measured
	// from before the success and gave a box that had just answered up as
	// lost. A release cannot say both — a collection either read the box or
	// it did not — and one that does is refused.
	Collected bool

	// Fence is the lease the claim was taken under.
	Fence Fence
}

// License is what an ending may be DECIDED under ([PendingStore.DecideEnding]):
// the lease, the statuses and the job it was decided on.
//
// A LICENSE, NOT A FILTER. It is what this ending is entitled to end, and a run
// that has moved off it — to a newer lease, another status, another job — is
// somebody else's to end. An ending decided on a row its caller has READ takes
// the widest statuses and jobs there are, because the caller has seen the run
// is over ([Coordinator.finish]); the narrow one belongs to the ending whose
// decision is made without having read the row — a claim's own
// ([Coordinator.endClaim]). Every one of them keeps its fence, which the store
// checks against the row as it stands — and since the box is reclaimed only
// after the decision ([Decision.Reclaim]), that check guards the kill as well
// as the delete.
type License struct {
	// Fence is the lease the ending is made under; a newer one owns the
	// run.
	Fence Fence

	// WhileIn is the statuses the run may be ended from. [Active] — every
	// status a record can hold — is the widest. An empty set licenses
	// nothing and deletes nothing, which is the safe way round for a zero
	// value.
	WhileIn []string

	// Launch is the job the ending was decided on, and the run is ended
	// only while it still holds that job — or whatever job it holds, for
	// [EveryLaunch]. A claim's own ending names its job, because a status
	// alone does not tell the claim apart from the next one: the resumed
	// turn can relaunch on the row, and that job's own completion claims
	// it in the same status. The zero value names no job — every launch
	// mints one — so it ends nothing rather than a run the caller did not
	// mean.
	Launch string

	// Unused says the run's recorded answer went unused even where a turn
	// took it: the claim's own ending, after its turn gave the claim back as
	// a retry — which says nothing it did reached anybody — and the release
	// could not land. Recorded on the ending ([RecordedEnding.Unused]): the
	// run is then not deleted while it holds a recorded answer at all
	// ([ErrAnswerOwed]), and every other ending deletes one a turn took with
	// the run, because it has been used.
	Unused bool
}

// EveryLaunch licenses an ending whatever job its run holds — see
// [License.Launch]. Never a launch id: the store mints those as UUIDs.
const EveryLaunch = "*"

// LetGo is how a decided ending lets go of the recorded answer its run still
// holds ([PendingStore.OweHandBack]): which ending, which answer, and the copies
// it owes the seat for it.
//
// NO LEASE AND NO STATUSES, which the let-go used to carry so that it did not do
// what the delete after it would refuse. It is a step of an ending that is
// already DECIDED ([PendingRun.Ending]): the license was the decision's, and
// the row takes no other write from then on, so nothing can have moved it off
// that license since.
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
	// ([types.SandboxRunFailed]); an empty Reason announces nothing.
	Reason, Detail string

	// Reclaim is whether the box the row names is reclaimed by the attempt
	// that finishes the ending, before the row is deleted: every ending that
	// has not reclaimed a box of its own, because a box killed before the
	// decision is killed on the caller's snapshot, which a newer lease may
	// have overtaken ([Coordinator.finish]).
	Reclaim bool
}

// RecordedEnding is an ending DECIDED on a run and not yet finished: what it
// announces and under which identity, what it hands back, and what it reclaims.
//
// THE DECISION IS THE COMMITMENT, and it is recorded because an ending is
// several effects in several systems — a reply handed back to an inbox, a box
// reclaimed, an announcement published, a record deleted — and a process can
// stop between any two. Decided only in the moment of the delete, as an ending
// was, its announcement could be made only after it, and a node that stopped
// between the two — or a delete that landed and reported a failure — left a
// run lost with no account of it; a seat's next holder, finding the row the
// delete had not taken, announced a reason of its own. Recorded first, every
// step after it is the SAME ending's, finished by whoever reads the row next —
// this node's retry, or the seat's next holder — and the announcement goes out
// BEFORE the delete under the identity recorded here ([RecordedEnding.ID],
// [RecordedEnding.At]), so a repeat after a crash is the same event to every
// store that keeps it ((event_time, event_id) is the event store's key, and the
// fleet's reads merge on it), never a second one.
//
// AND IT FREEZES THE RUN: from the decision on, the store refuses every write
// but the ending's own steps ([ErrRunEnding]) — no claim, no take of the
// answer, no release, no relaunch, no record of a new answer. That is what
// makes publishing the announcement before the delete safe: nothing can move
// the run off the ending once it is announced, so a run is never announced as
// lost and then resumed.
type RecordedEnding struct {
	// ID names the ending, and with the run's turn it derives the id its
	// announcement is published under ([endingEventID]).
	ID string `json:"id"`

	// At is when the ending was decided, on the store's clock, and the
	// instant its announcement carries.
	At time.Time `json:"at"`

	// Reason and Detail are what it announces; an empty Reason announces
	// nothing (an ordinary ending, whose turn came back).
	Reason string `json:"reason,omitempty"`
	Detail string `json:"detail,omitempty"`

	// Returned is whether the ending hands a person's answer back to the
	// seat — one the run held no turn took (or that went [Unused]), or copies
	// it already owed — decided with the ending, so the announcement says so
	// whichever attempt makes it.
	Returned bool `json:"returned,omitempty"`

	// Unused is the license's ([License.Unused]): the recorded answer goes
	// back to the seat even though a turn took it.
	Unused bool `json:"unused,omitempty"`

	// Reclaim is the decision's ([Decision.Reclaim]).
	Reclaim bool `json:"reclaim,omitempty"`
}

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

	// Lost says the claim DIED: its node stopped, or the seat moved, between
	// the claim and the turn that would have taken the answer, and the seat's
	// next holder revives it ([Coordinator.reapTail]). The revival COUNTS a
	// lost claim on the answer ([RecordedAnswer.LostClaims],
	// [RecordedAnswer.FirstLostAt]), which is what bounds a resume that takes
	// its node down.
	//
	// FALSE IS A CLAIM NOBODY LOST: one a node made itself, whose write
	// reported a failure and landed, given back by the series that made it
	// under the lease it was taken under ([Coordinator.suspectClaim]). No node
	// stopped, so nothing is counted — counted, a coordination store that
	// answered a few of a healthy node's claims with errors ended the run as
	// an abandoned tail, announced as a node that stopped. That series is
	// bounded by its own attempts ([MaxAnswerAttempts], [answerWindow]),
	// which end in a decline rather than an ending.
	Lost bool
}

// Holding are the statuses in which a run holds its seat, so the seat takes no
// new turn while it is in one.
//
// Launching is here and awaiting is not, and both for the same reason: the
// seat is held while the engine is driving the run, and freed while a person
// is. A launching run is a fraction of a second of engine work; a parked one
// can wait days for an answer that arrives on the seat's own inbox.
var Holding = []string{StatusLaunching, StatusRunning, StatusResumed}

// Awaiting are the statuses still waiting on a person's answer, matched back
// by conversation.
var Awaiting = []string{StatusAwaiting, StatusReseed}

// Active are the statuses that still own engine-side state — a seat, a box, or
// a pending tail — and so must survive a restart.
//
// It is also every status a record can hold, since an ended run has no record,
// so it is the closed set [PendingStore.SetStatus] accepts as well as the one
// every listing matches. Asserted at the write rather than assumed: a status
// nothing recovers from is a status that leaks a box, so a typo has to be
// refused instead of becoming a record no recovery pass matches. And matched
// at the read, so a record carrying a status this build does not know is never
// mistaken for live work.
//
// RESUMED IS HERE, which looks wrong and is not: boot recovery has to be able
// to SEE a tail that died mid-flight with the previous engine. Nothing else
// would ever look at that row again, and its paused box would leak for ever.
// LAUNCHING is here for the same reason and only that reason — it is never
// polled, but a node that died mid-launch left a box behind, and a row nobody
// lists is a box nobody reclaims.
//
// ANSWERED is here because the resume it owes has to be found again by
// whichever node holds the seat next: an answer recorded on a node that then
// stopped is re-driven by the successor's recovery pass, not by the person
// sending it a second time.
var Active = []string{
	StatusLaunching, StatusRunning, StatusAwaiting, StatusReseed, StatusAnswered, StatusResumed,
}

// BridgeCall is one tool call a bridged run made.
//
// Its own shape rather than [tools.Call], because this crosses the wire into
// the fleet's coordination store where a node running a different build reads
// it — so the json tags are a wire format and the type must not carry a
// dependency the store layer has no business on. Same rule the rest of
// [PendingRun] follows: add fields, never rename one.
type BridgeCall struct {
	Name string `json:"name"`
	// Args is what the caller passed, as JSON text rather than a decoded
	// map: the map's values would round-trip through the store's own
	// encoder a second time, and a large id survives one pass and not two.
	Args   string    `json:"args,omitempty"`
	Output string    `json:"output,omitempty"`
	Failed bool      `json:"failed,omitempty"`
	At     time.Time `json:"at"`
}

// BridgeAppend is one finished bridged call as the bridge hands it to the row
// — see [PendingStore.AppendBridgeCall].
type BridgeAppend struct {
	// Launch is the job the session serves, as [PendingStore.BeginLaunch]
	// named it — handed to the session through [LaunchRequest.Opened] before
	// the job's box existed, so no call the box makes can name any other.
	// REQUIRED: an append naming no job is recorded under none.
	Launch string

	Call BridgeCall

	// Spent is the session's running total: what every bridged call it has
	// served so far cost the engine.
	Spent EngineSpend
}

// MaxBridgeCalls bounds the durable log of a bridged run.
//
// The row is ONE VALUE in the coordination store, read and written whole on
// every mutation, and a coding run can make hundreds of calls — so an
// unbounded list turns a busy run's every status change into a growing write.
// Two hundred is well past what a reviewer reads (the ledger elides a long log
// anyway) and small enough that the row stays a row.
//
// The MIDDLE is what gets dropped, never the start: how a run began and how it
// ended are what explain it, and a log truncated to its last N loses the
// former entirely.
const MaxBridgeCalls = 200

// PendingRun is one detached job's durable state, keyed by its kick-off turn.
//
// The json tags are a WIRE FORMAT, not decoration: the record lives in the
// fleet's coordination store, where a node running a different build reads
// what this one wrote. Renaming a field renames a key, and a key the reader
// does not know decodes to a zero value — an emptied box reference is a
// leaked sandbox. Add fields; never rename or repurpose one.
type PendingRun struct {
	// TurnID is the RUN this record belongs to — one execution of a turn,
	// and this record's own key. See ADR-0017.
	TurnID string `json:"turn_id"`

	// WorkKey is the unit of work that run was dispatched for. It rides
	// the row so a resumed turn keeps the identity its writes have to be
	// idempotent against — the resume re-enters the loop mid-round, with
	// no trigger left to re-derive it from. Empty on a turn with no
	// ledgerable trigger, which is the documented "nothing to collapse"
	// case.
	WorkKey string `json:"work_key,omitempty"`

	// WorkSince is when that unit of work began, and it rides the row for
	// the reason the key does: every operation id a turn derives from the
	// key carries this instant as its mint time, so a resumed turn that
	// could not reproduce it would derive DIFFERENT ids for the same
	// writes, and the state log reads it to refuse deciding again an
	// operation minted before its node's operation ledger may have lost
	// rows to the ledger's sweep.
	//
	// THE START, NOT NECESSARILY WHERE THE RESUMED HALF MINTS: a resume so
	// long after this instant that the operation ledger may have swept it
	// mints its writes at the instant the engine rebases it onto instead,
	// because every operation minted here would then be one no node can
	// vouch for. That rule is the engine's, judged at every attempt against
	// the attempt's own clock and recorded in the fleet's coordination
	// store rather than on this row — it is a rule about the ledger and
	// about the unit of work, which outlives any one run's row.
	//
	// Zero where there is no work key, or where the trigger carried no
	// timestamp; a zero start is rebased by the engine like any start past
	// the horizon, identically in both halves of the turn.
	WorkSince time.Time `json:"work_since,omitzero"`

	AgentHandle string `json:"agent_handle"`
	AgentID     string `json:"agent_id"`
	Role        string `json:"role"`

	SandboxID   string `json:"sandbox_id"`
	CodingAgent string `json:"coding_agent"`

	// Placement is which configured backend holds this run's box.
	//
	// PERSISTED, not re-derived from the config, and that is the whole
	// reason the field exists: the completion turn may run in a different
	// process on a different node, minutes or days later, and the company
	// configuration may have been applied again in between. Reconnecting to
	// a remote box through the local backend does not error usefully — it
	// reports a box that has vanished, and a run that is still going is
	// abandoned as gone. Always resolved at launch ([Manager.BuildSpec]
	// fills the default).
	Placement string `json:"placement,omitempty"`
	CommandID string `json:"command_id"`
	Status    string `json:"status"`

	// LaunchID names the job this row currently holds.
	//
	// The row is the TURN's, and a turn can run more than one job: a
	// resumed executor that calls run_sandbox again reuses it, and
	// [PendingStore.BeginLaunch] names each launch anew. A completion
	// carries the name of the job it saw finish, which is what lets a
	// claim tell the job it was raised for from one that has replaced it.
	// See [Tail].
	//
	// Minted by the store and never by the caller, for the reason the
	// status is: a caller that could choose it could reuse one. Never
	// empty on a row: the store mints it in the write that creates the row
	// and again in every relaunch.
	LaunchID string `json:"launch_id,omitempty"`

	// Launch is what the run's own phase record needs about the job named
	// by LaunchID — see [LaunchRecord]. Written with LaunchID, whole, on
	// every launch.
	Launch LaunchRecord `json:"launch_record,omitzero"`

	// Owner is the process INCARNATION that owns this run's seat, and
	// OwnerEpoch the seat lease's epoch at the moment of the claim.
	//
	// The epoch is the FENCING TOKEN: every mutation on a live run carries
	// it, so a node whose lease moved cannot write even if it has not
	// noticed yet. The ownership check is an optimisation; the fence is the
	// guarantee. Empty owner means unclaimed — what an in-flight run looks
	// like the instant before its seat's new owner recovers it.
	Owner      string `json:"owner"`
	OwnerEpoch int64  `json:"owner_epoch"`

	// TaskDescription is the ask the suspended turn was working on, so a
	// resume days later has the brief even when the trigger that produced
	// it is long gone.
	TaskDescription string `json:"task_description"`

	// Reply is who is waiting for the suspended turn — [turn.Reply]'s wire
	// value, written as a plain string because this row crosses builds.
	//
	// It has to be persisted rather than re-derived: the resumed turn does
	// not see the trigger, so without this a turn somebody was waiting on
	// would come back from its coding run free to end in silence. Every
	// launch writes a kind — `none` when nobody is waiting — so a resume
	// refuses a row that names none rather than guessing; see
	// [turn.Reply].
	Reply string `json:"reply,omitempty"`

	// PartitionKey is the inbox PARTITION key the run was launched
	// under: the batch its kick-off trigger arrived in.
	//
	// NOT WHAT AN ANSWER IS MATCHED ON, which it was, and the engine's own
	// prompt is why it could not stay so. A run launched from a TOP-LEVEL
	// direct message parks under the bare DM channel, because a top-level
	// burst coalesces on the channel — and the chat prompt then tells the
	// seat to reply AS A THREAD, so the person's answer arrives keyed on
	// that thread. Under equality the two strings never met: the
	// clarification a box is parked waiting for was never delivered and
	// the run sat until its pause TTL reaped it.
	//
	// NOT WHAT ADMITS AN ANSWER, but still what tells two admitted runs
	// apart: a direct message's identity is the whole channel, so two runs
	// parked from two threads of it are admitted by a reply in either, and
	// this is the only field that says which thread each was asked in. See
	// [ConversationRef.Best].
	//
	// Empty on a run launched by a wake that named no conversation (a
	// schedule tick, an A2A ask).
	PartitionKey string `json:"partition_key,omitempty"`

	// ConversationKey is the durable conversation this run belongs
	// to: what the resumed turn's ledger entry is filed under, and — since
	// a person answers on the conversation rather than into the batch —
	// what admits an arriving delivery as its answer.
	//
	// The two were one field, and its doc said so — "where to report back
	// AND what matches a person's answer". They are different questions
	// and, for a direct message, different values: the partition is the
	// batch a delivery arrives in, the conversation is the line a person is
	// talking on. One value answering both cost one failure each way —
	// filing the resumed turn under the batch put a DM's coding work in a
	// ledger row the next turn never looked up, and matching on it lost
	// the answer outright.
	//
	// Spelled `conversation_key` on the row as it is on every event that
	// carries the identity — the two sandbox events, both turn completions
	// and the phase record — and empty on a run launched by a wake that
	// named no conversation.
	ConversationKey string `json:"conversation_key,omitempty"`

	// Branch is the pushed WIP branch: the durable half of the work, and
	// what a re-seeded run starts from when its snapshot is gone.
	Branch    string `json:"branch"`
	SessionID string `json:"session_id"`

	Question string `json:"question"`
	Audience string `json:"audience"`

	// ParkedInputTokens and ParkedOutputTokens are what the job that
	// asked the question cost, recorded with the question.
	//
	// ON THE ROW because the segment that pays for them is not the one
	// that collected them: a job that parks on a question is collected,
	// charged to the counters and parked, and the turn resumes only when a
	// person answers — possibly days later, on another node, with nothing
	// collected at all. That resumed segment is the job's segment (it
	// resumes under the same launch), and its charge to the turn's work
	// item includes the job's tokens exactly as a completion's resume does
	// (ADR-0022). Without these the answer's resume charged the task for
	// the collection and never for the coding run that asked.
	ParkedInputTokens  int `json:"parked_input_tokens,omitempty"`
	ParkedOutputTokens int `json:"parked_output_tokens,omitempty"`

	// AskedAt is the instant the question was put — taken before it was
	// announced, so it precedes the moment anybody could have read it.
	//
	// THE ANCHOR EVERY ANSWER IS MEASURED AGAINST. A reply qualifies only if
	// it was posted at or after this instant ([Reply.Posted]): a message
	// written before the question existed cannot be its answer, however it
	// is threaded. Without it the match was purely positional — "the next
	// inbound on the conversation" — so a reply that answered an EARLIER
	// question, held back by a failed resume and redelivered behind the
	// conversation's newer mail, was spliced into the run as the answer to
	// whatever it asked next, and a message sent while the job was still
	// running was taken as the answer the moment it parked.
	//
	// REQUIRED on every park ([PendingStore.MarkAwaiting]): a question with
	// no anchor is one no reply could be shown to answer.
	AskedAt time.Time `json:"asked_at,omitzero"`

	// Answer is the reply recorded as this question's answer, set exactly
	// while the run is [StatusAnswered] and while that answer's resume is
	// claimed — until a turn takes it, or the run's ending lets it go back
	// to the seat ([PendingStore.OweHandBack]). A row holding one no turn
	// took is never deleted ([ErrAnswerOwed]). See [RecordedAnswer].
	Answer *RecordedAnswer `json:"answer,omitempty"`

	// DeclinedAnswers are the deliveries this question was recorded with
	// and then let go of — every attempt to resume with them failed, so they
	// went on to the seat's ordinary route instead (see
	// [Coordinator.declineAnswer]) — by event id, and the ids of the copies
	// handed back. Neither is ever recorded as this question's answer
	// again, which is what stops a copy circling the run it already failed
	// to reach. Bounded by [maxDeclinedAnswers]; cleared with the question.
	DeclinedAnswers []string `json:"declined_answers,omitempty"`

	// HandBack is what an answer let go of still owes the seat's inbox: the
	// copies of its deliveries, under the ids they are published with,
	// written IN THE SAME WRITE that lets the answer go
	// ([PendingStore.DeclineAnswer], or [PendingStore.OweHandBack] for a
	// run that ends before any turn took the answer) and removed once they
	// are published ([PendingStore.ClearHandBack]).
	//
	// AN OUTBOX ON THE ROW, because the decline is two effects in two
	// systems — a compare-and-set here and a publish to the broker — and
	// neither order of the two survives a crash between them on its own.
	// Published first, a crash before the write left the answer recorded
	// AND its copy on the inbox, so the reply was delivered twice: once as
	// the answer a later resume used, once as an ordinary message. Written
	// first with nothing beside it, a crash before the publish would lose the
	// reply outright. Written first WITH the copies, the write is the
	// decision and the copies are its durable consequence: whichever node
	// reads the row next publishes them, under ids derived from the
	// originals, so a publish repeated after a crash is the same message
	// twice — collapsed by the inbox's same-id dedupe and the completion
	// ledger — and never a second one.
	//
	// It is the row's and survives everything the row does — a new question,
	// a relaunch, a recorded answer — because what it owes is owed to the
	// seat rather than to any question. A run that ENDS publishes what it
	// still carries BEFORE its record is deleted, and the delete refuses
	// while anything is owed ([PendingStore.Finish], [Coordinator.endRecord]):
	// published after, a crash between the delete and the publish lost the
	// copies with the only record of them.
	//
	// BOUNDED BY ONE DECLINE. A write that adds copies lands only on a row
	// that owes none ([ErrHandBackOwed]), so the row never carries more than
	// one let-go answer's deliveries — at most one inbox batch
	// ([queue.DefaultBatchOptions], 20 events by default) — which is the size
	// of the [PendingRun.Answer] the copies were made from. Unbounded, a
	// broker that kept refusing the publishes while answers kept being
	// recorded and let go grew the row by one batch each time. Past the
	// bound nothing is lost: the refused decline leaves the answer recorded
	// and owed, the seat's inbox stays held behind it, and the retry
	// publishes the copies already owed before it decides again
	// ([Coordinator.retryOwed]).
	HandBack []HandedBack `json:"hand_back,omitempty"`

	// Ending is the run's ending once one is DECIDED, nil while the run is
	// live. From the decision on, the row takes no write but the ending's
	// own steps, and whoever reads it next finishes that ending on the terms
	// recorded here — see [RecordedEnding].
	Ending *RecordedEnding `json:"ending,omitempty"`

	// WorkItem is the one work item the launching turn was charged to, nil
	// when it was on nothing.
	//
	// ON THE ROW because the resumed turn has no trigger to resolve it from:
	// the dispatch that named the item is gone, and a person's answer that
	// resumes a parked run is an ordinary chat message naming nothing. The
	// resume reads it back as its own item, under
	// [types.BasisResume] — the same turn, still on the same work.
	WorkItem *types.WorkItem `json:"work_item,omitempty"`

	// AudienceHandles are the seats a parked question may be answered by,
	// and AudienceFallback whether that set fell back to a default rather
	// than being what the run named.
	//
	// RESOLVED ONCE, AT THE PARK, by [CoordinatorOptions.Audience] against
	// the chart the parking node holds, and written in the same write as the
	// question. [PendingRun.Audience] is a free label the coding agent chose
	// — "requester", "team", "manager", or a name it typed — and it was
	// never resolved at all, so "what is waiting on me" had no answer: a
	// person could see every parked question in the company and none of
	// them said it was theirs. Resolved at the park rather than at every
	// read because the label is about the moment it was asked: who the
	// requester's manager WAS then is who was asked.
	//
	// Empty until the run parks on a question, and cleared with the
	// question when a new job opens on the row ([PendingStore.BeginLaunch]).
	// A key a NEWER build adds beside these survives this build's
	// compare-and-swap through [PendingRun.Extra].
	AudienceHandles  []string `json:"audience_handles,omitempty"`
	AudienceFallback bool     `json:"audience_fallback,omitempty"`

	// Requester is the seat whose message, notice or ask woke the turn
	// that launched this run — the person a question addressed to
	// "requester" means — and empty when nothing a seat said woke it (a
	// schedule, a sender this company's chart does not know).
	//
	// ON THE ROW because the park that resolves the audience is not the
	// frame that saw the trigger: it runs when the job finishes, possibly
	// days later and on another node, with nothing of the dispatch left.
	// A row without it resolves "requester" to the fallback, which is what
	// a run whose requester nobody recorded is.
	Requester string `json:"requester,omitempty"`

	// TraceID and SpanID are the trace the run started under, so the
	// follow-up turn nests beneath it rather than appearing as unrelated
	// work minutes later.
	TraceID string `json:"trace_id"`
	SpanID  string `json:"span_id"`

	DelegationDepth int      `json:"delegation_depth"`
	DelegationChain []string `json:"delegation_chain"`

	// ExecuteState is the SUSPENDED Execute conversation: the serialized
	// messages, including the assistant turn with the dangling tool call,
	// plus the surface bookkeeping needed to re-enter the loop where it
	// stopped.
	//
	// Empty for exactly as long as the run is [StatusLaunching], which is
	// the state that says so: the launch starts the job, and the turn
	// writes this when its frame unwinds. Nothing polls or claims a run in
	// that window, so a claimed run always has one — and a claimed run
	// WITHOUT one is a row the launch path did not write, which the
	// coordinator fails rather than resuming into nothing.
	//
	// RAW BYTES, because this package carries the conversation and never
	// reads it. Held as a decoded map it was decoded twice on every
	// suspension — once for the row and once more off the coordination
	// record — and each decode read every number as a float64, so an id
	// longer than 2^53 a model had passed as a tool argument came back from
	// the row as a different id. Bytes are carried, not decoded, and a JSON
	// null (what a launching run writes) is read back as none at all — see
	// decodeRun.
	ExecuteState json.RawMessage `json:"execute_state"`

	// BridgeCalls is what a run made through the MCP bridge, in order.
	//
	// DURABLE, because this is the one tool log that has nowhere else to
	// live. A native tool loop keeps its calls on a surface in memory and
	// the turn writes them when it ends; a bridged run's calls are made by
	// a process outside the engine, minutes or hours apart, and possibly
	// across a restart. Without this the reviewer of a resumed run judges a
	// turn whose entire tool log is gone — and "it called nothing" is
	// exactly the shape the delivery check reads as a turn that did not act.
	//
	// Bounded: see [MaxBridgeCalls]. Empty on every run that is not
	// bridged, which is every ordinary coding run.
	BridgeCalls []BridgeCall `json:"bridge_calls,omitempty"`

	// BridgeCallsElided counts the calls dropped from the middle of that
	// list, so a reader can tell a short run from a long one whose middle
	// was cut. Reported rather than hidden, because a log that silently
	// skips is a log that lies about what the run did.
	BridgeCallsElided int `json:"bridge_calls_elided,omitempty"`

	// Charged is whether this launch's collected tokens are on the fleet's
	// token counter.
	//
	// ON THE ROW, not in the coordinator's memory, because the charge sits
	// inside the part of the tail that is RETRIED. A resume that fails
	// hands the claim back and the completion comes back, to this node or
	// to the seat's next owner, and the retry collects the same finished
	// job again. With nothing recording the first charge, every retry
	// charged the run again, against the seat's budget and the company's,
	// for as long as the resume kept failing.
	//
	// WRITTEN BY THE RELEASE that hands the claim back (see [Release]),
	// because that is the only write through which a retry reaches the
	// charge again: recorded in a write of its own, a store that refused it
	// and then accepted the release reopened the run with no record, and the
	// retry charged it twice. A run parked on its question is reached only
	// by an answer, which charges nothing, so its park needs no record.
	//
	// Launch-scoped, like the suspension: a second run_sandbox call in one
	// turn is a second job with spend of its own, so [PendingStore.BeginLaunch]
	// clears it, and nothing else does.
	Charged bool `json:"charged,omitempty"`

	// CompanyCharged is whether this launch's collected tokens are on the
	// COMPANY's counter while the seat's write failed
	// ([coord.SeatUncountedError]) — meaningful only while Charged is not.
	//
	// The partial is KEPT, never undone: the run spent what it spent, and
	// the compensation that used to take the company's share back left it
	// on neither counter whenever the resume then succeeded, since only a
	// failed resume brings a collected run back to be charged again. Kept
	// and recorded here, the retry a failed resume does bring finishes the
	// seat's share alone rather than counting the company twice; a resume
	// that succeeds leaves only the seat short, by this run.
	//
	// Written and cleared exactly as Charged is.
	CompanyCharged bool `json:"company_charged,omitempty"`

	PauseTTLSeconds float64 `json:"pause_ttl_seconds"`

	// PausedAt is when this run's box was paused, zero when nothing
	// recorded a pause. Together with SandboxID it is the engine's record
	// of the box, and what lets the reaper reclaim a snapshot nothing else
	// would ever free.
	//
	// ZERO IS NOT "NO SNAPSHOT", which is the reading that leaked boxes:
	// the stamp is a SECOND write, made after the box is already paused and
	// warn-only when it fails, so a parked run can hold a snapshot this
	// field says nothing about. [PendingRun.HeldSince] is the reading every
	// caller that acts on a held box takes.
	PausedAt time.Time `json:"paused_at"`

	// ClaimedFrom is TRANSIENT and never persisted — hence `json:"-"` —
	// the status the row held immediately BEFORE a claim flipped it to
	// resumed. A failed resume
	// dispatch reverts to exactly this, so a NAK'd trigger can re-claim on
	// redelivery. Inferring it from the other fields is unsound — a reused
	// run keeps its old question.
	ClaimedFrom string `json:"-"`

	// Collected is TRANSIENT too: set on a claimed row by the completion
	// tail once THIS claim's collection read the box back, so whichever
	// hand-back follows — a park or a resume that failed — ends the job's
	// run of failed collections ([Release.Collected]). Never persisted,
	// because it is a fact about one claim and the next claim starts
	// without it.
	Collected bool `json:"-"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`

	// Extra is every key on the stored record THIS BUILD DOES NOT KNOW,
	// kept verbatim and written back on every re-encode.
	//
	// THE ROW IS READ, MODIFIED AND WRITTEN WHOLE by every node that touches
	// it — every status flip, claim and release is a compare-and-swap of the
	// entire value — and a fleet mid-upgrade has two builds doing that to
	// one row. Decoded into this struct alone, an older build's flip wrote
	// back only the fields IT knew, so the first claim or release it made
	// silently deleted whatever a newer build had added: the item the run
	// is charged to, the audience its question is waiting on. Nothing
	// failed; the newer node simply read the row back without them.
	//
	// Filled by the decode in [CoordStore] and re-emitted by its encode,
	// never set by a caller: a known field always wins over a key of the
	// same name here, so this can only ever carry what the struct cannot.
	Extra map[string]json.RawMessage `json:"-"`
}

// Paused reports whether a pause was RECORDED for this run's box.
//
// The record's own fact, and deliberately not the question "is a box being
// held" — that is [PendingRun.HeldSince], and it is what a caller acting on a
// held box reads. The two differ on exactly one row, which is the row this
// distinction exists for: a run parked on a question whose pause reached the
// box and whose stamp never reached the row.
func (r PendingRun) Paused() bool { return !r.PausedAt.IsZero() }

// HasBox reports whether a box exists for this run at all.
func (r PendingRun) HasBox() bool { return r.SandboxID != "" }

// HeldSince is when this run's box started being held as a snapshot nothing is
// driving, and whether it is being held at all.
//
// THE ROW DESCRIBES THE BOX; THE STAMP ONLY DATES IT. [Coordinator.collect]
// pauses the box and THEN records the instant, and that record is a second
// coordination write that fails on its own — warn-only, because the job is
// over either way and throwing a collected result away over a timestamp would
// be far worse. So a run PARKED ON A QUESTION can hold a paused box with no
// stamp on its row, and that row still describes a box being paid for: the
// park is the one state whose box is deliberately held for an open-ended human
// wait, the completion poll skips it so nothing refreshes its keepalive, and
// no tail is coming to settle it. Reading the missing stamp as "no snapshot"
// is what made such a box invisible to the reaper ([pauseExpired]) for good.
//
// THE FALLBACK IS THE ROW'S OWN LAST WRITE, because on a parked row that write
// IS the park — the one write that has to land for the run to be parked at all
// ([Coordinator.park] gives its claim back where it does not) — and it lands
// milliseconds after the pause it failed to record. It is therefore never
// EARLIER than the true pause instant, which is the safe direction to be
// wrong in: the box is held a moment longer rather than reclaimed out from
// under a person who is still typing.
//
// AN ANSWERED RUN TAKES THE SAME FALLBACK, for the same reason: its box is
// still the park's, held while the recorded answer waits on its resume, and
// its last write is the record of that answer — later still than the park.
//
// A RUN THE ENGINE IS DRIVING TAKES NO FALLBACK. Every other pause in the
// lifecycle lasts one dispatch and is settled by the tail that made it, so
// there the stamp is the whole answer and its absence means the box is live —
// dating one of those from the last write would report a running job as a
// snapshot being billed for.
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

// PendingStore is the persistence surface for detached runs.
//
// ONE IMPLEMENTATION, [CoordStore], on the fleet's coordination store, whose
// record operations are certified on both coordination backends. It stays an
// interface because the coordinator's hardest properties (the at-most-once
// claim, the scoped release, epoch fencing) are properties of the store's
// conditional writes, which the sandboxtest suite certifies apart from any
// caller, and because a coordinator case can stage a store that refuses one
// write only by wrapping it.
type PendingStore interface {
	// BeginLaunch opens a launch on this turn's row: it creates the row
	// when there is none, and RESETS an existing one to launching —
	// clearing the previous job's suspended conversation, the question
	// it was parked on and the record of its charge, while keeping the
	// row's identity and its box. Either way the launch gets a new
	// [PendingRun.LaunchID].
	//
	// CREATE-OR-RESET rather than create-if-absent, because the SECOND
	// run_sandbox call in one turn presents the same turn id as the first
	// and is a different job. Left alone, the row kept the first
	// suspension's conversation and whatever status the tail had reached —
	// so a resume that relaunched read back as `resumed`, the settle path
	// could not tell it from a finished turn, and it tore down the box the
	// new job was running in.
	//
	// THE ROW IS OWNED BY THE LEASE THAT LAUNCHED IT: a fenced launch stamps
	// its fence as the row's [PendingRun.Owner] and [PendingRun.OwnerEpoch],
	// and every later write the launching node makes carries that fence back
	// off the row. Unstamped, a row stayed at the zero epoch until a seat's
	// next holder recovered it, and the zero fence constrains nothing — so a
	// node that had lost the seat could still release, take and end its runs
	// under the next holder's feet, which is the one write the fence exists
	// to refuse.
	//
	// ANSWERS THE ROW AS IT OPENED THE JOB — the job's name, minted here
	// ([PendingRun.LaunchID]), and the instant it began
	// ([LaunchRecord.StartedAt]) — because the store is the one party that knows
	// the name, and something has to hold it before the job's box can act:
	// an agent-mode run's bridge session records every call under its own
	// job ([BridgeAppend.Launch]), and a session that learned the name from
	// the row later would learn whichever job the row held by then.
	//
	// A RESET THE ROW REFUSES IS AN ERROR, never a launch that went ahead: a
	// newer lease owns the run, or its ending is decided ([ErrRunEnding]). A
	// launch told nothing went on to start a job on a row that never named
	// it, in a box nothing would reclaim. A row that went between the create
	// and the reset is created again.
	BeginLaunch(ctx context.Context, run PendingRun, fence Fence) (PendingRun, error)

	Get(ctx context.Context, turnID string) (PendingRun, bool, error)

	// ClaimForResume atomically flips a run to resumed, when it holds the
	// tail's launch in one of the tail's statuses.
	//
	// Reports the row IFF THIS CALL WON: the at-most-once tail guard, and
	// the reason a claim names its launch (see [Tail]). The returned row
	// carries ClaimedFrom, so a failed dispatch can put it back exactly
	// where it was.
	//
	// TAKEN UNDER THE CLAIMANT'S LEASE. A claim is refused where a newer
	// lease than the fence owns the run, and a fenced one stamps the fence
	// on the row as its [PendingRun.Owner] and [PendingRun.OwnerEpoch]:
	// every write the claim makes afterwards — the take of its answer, its
	// release, its ending — carries that lease back off the row it returns,
	// so the seat's next holder fences the CLAIMANT out, whatever lease the
	// row was stamped with before. Carried off a row stamped by somebody
	// else, the fence fenced out nobody: a row launched under no lease, or
	// one that no recovery re-stamped, sat at the zero epoch, which
	// constrains nothing.
	//
	// SUPERSEDED, NOT OUTRANKED, as a take is ([PendingStore.TakeAnswer]): a
	// ZERO fence claims only a row no lease has ever owned, and leaves its
	// owner as it stands. Exempted as a write that holds no lease elsewhere
	// is, a node that had noticed it lost the seat — whose lease it then
	// answered as the zero fence — claimed the answer on a row its successor
	// had fenced, took it and ran the turn on a seat it did not hold. Nothing
	// that claims holds no lease where a lease exists: a recovery fences
	// under the lease it took the seat with.
	ClaimForResume(ctx context.Context, turnID string, tail Tail, fence Fence) (PendingRun, bool, error)

	// ReleaseClaim hands a claimed run back to the status it was claimed
	// from, reporting whether THIS call did.
	//
	// Only while the claim still stands: the run is resumed, holds the
	// release's launch, and no newer lease outranks the release's fence.
	// Anything else means the run has moved on from the claim (see
	// [Release]), and a retry of the signal has nothing left to take.
	//
	// The claim's charge is recorded IN THE SAME WRITE, and never cleared
	// by one: a run is reopened to a retry with its record or not at all
	// (see [PendingRun.Charged]).
	//
	// AND A RECORDED ANSWER THE CLAIM'S TURN TOOK IS THE RUN'S AGAIN
	// ([RecordedAnswer.TakenAt] cleared): a resume whose turn gives its
	// claim back has reported that nothing it did reached anybody, which is
	// what makes the retry safe, and the retry hands the same answer to a
	// turn of its own. A claim whose answer an ending has already let go of
	// is not released to [StatusAnswered] at all: an answered run carries
	// its answer, and one that did not would be owed a resume nobody could
	// make.
	//
	// FALSE IS NOT AN ERROR: it is a run that moved on, or a row that is
	// gone. A release to a status outside [Claimable] is an error, because
	// no claim ever takes a run out of one.
	ReleaseClaim(ctx context.Context, turnID string, release Release) (bool, error)

	// MarkAwaiting parks a run on a question, freeing the seat. A
	// question with no [Clarification.AskedAt] is refused: it is the anchor
	// every answer is measured against ([PendingRun.AskedAt]).
	MarkAwaiting(ctx context.Context, turnID string, q Clarification) error

	// ClaimOwnership takes the run for a node, reporting whether it won.
	// A run whose epoch is already higher is not stolen.
	ClaimOwnership(ctx context.Context, turnID, owner string, epoch int64) (bool, error)

	// SetStatus moves a run between the live states, fenced on the epoch.
	// Ending a run is not a status; see DecideEnding.
	SetStatus(ctx context.Context, turnID, status string, fence Fence) error

	// DecideEnding records that a run is ENDING, on the terms of d — while
	// its license holds, see [License] — and hands back the row as it now
	// stands, its [PendingRun.Ending] the one this call recorded or the one
	// already there. See [RecordedEnding] for why an ending is decided before
	// any of it is done.
	//
	// ONE DECISION PER RUN: a run whose ending is already decided keeps it,
	// and a second decider is handed that one to finish rather than deciding
	// its own — the first decider's reason, detail and identity are the ones
	// every attempt announces. And from the decision on, the row refuses every
	// write but the ending's own steps ([ErrRunEnding]).
	//
	// FALSE IS NOT AN ERROR: the run is gone, a newer lease than the
	// license's owns it, or it is not what the license entitles this ending
	// to end. An error is a decision that may or may not have landed; the
	// caller decides again, and finds it recorded if it did.
	DecideEnding(ctx context.Context, turnID string, d Decision) (PendingRun, bool, error)

	// Finish deletes the record of a run whose ending is decided — the
	// ending named, and only once it owes the seat nothing — and hands back
	// the record it deleted.
	//
	// The ending's other steps come FIRST ([Coordinator.finishEnding]): its
	// box reclaimed where it says so, the reply it lets go of handed back, and
	// its announcement published, because nothing reads a deleted row again
	// and a step left for after the delete is lost to a crash between the
	// two. FALSE IS NOT AN ERROR: the run is already gone — two parties
	// finishing one ending — or it is not that ending's.
	//
	// A ROW THAT STILL OWES THE SEAT COPIES IS NOT DELETED ([ErrHandBackOwed],
	// with the row as it stands): the copies are the seat's, and a delete
	// before their publish loses them. NOR IS A ROW THAT STILL HOLDS A
	// PERSON'S REPLY NO TURN TOOK ([ErrAnswerOwed], with the row as it
	// stands) — or, for an ending whose reply went unused, any reply at all:
	// the reply's delivery was spent when it was recorded, so the row is the
	// only thing still carrying it. The caller lets it go
	// ([PendingStore.OweHandBack]), hands it back and finishes again. A reply
	// a turn took has been used, and goes with the run.
	Finish(ctx context.Context, turnID, ending string) (PendingRun, bool, error)

	// ExpirePause flips a run parked on a clarification to reseed AND
	// clears its box record, reporting whether THIS call won.
	//
	// The pause reaper's authority, and the reason it is a conditional flip
	// rather than a plain SetStatus. The reaper decides from a snapshot
	// taken seconds ago, and the answer that un-parks the run may have
	// arrived since — ClaimForResume has already moved the row and an
	// Execute loop is reconnecting to that very box. Killing the box before
	// this returns true destroys it underneath that resume. Conditional on
	// StatusAwaiting or [StatusAnswered]: a run already reseeded has no
	// snapshot left to expire, and any other status means somebody else
	// owns the tail.
	//
	// AN ANSWERED RUN EXPIRES TOO, because its box is held for exactly as
	// open-ended a wait as a parked one's: the answer is recorded, but its
	// resume waits on the seat's holder and its conditions, and a seat no
	// node holds may wait for days. It keeps its status and its answer —
	// the run is still owed its resume, which re-seeds from the branch as
	// a reseeded run's does — and the answer's From becomes
	// [StatusReseed], so an answer that is let go of reopens the question
	// on a run with no box rather than one naming the box just destroyed.
	// The flip is still the authority over the box: a resume that claimed
	// the row first moved it out of answered, and this loses.
	//
	// It clears the box IN THE SAME WRITE rather than leaving that to a
	// following ReleaseBox, because the gap between two writes is a state a
	// reader can see: a run reading as `reseed` while still naming its box
	// tells an arriving answer that the checkout is live, moments before it
	// is destroyed. One statement, no window.
	ExpirePause(ctx context.Context, turnID string) (bool, error)

	// AttachSandbox records the box and the command a run is using.
	AttachSandbox(ctx context.Context, turnID string, box BoxRef, fence Fence) error

	// MarkBoxPaused and ReleaseBox are the two halves of the box record:
	// paused_at set means a snapshot exists and is being billed for,
	// cleared means the box is gone.
	MarkBoxPaused(ctx context.Context, turnID string, at time.Time) error
	ReleaseBox(ctx context.Context, turnID string) error

	// MarkSuspended writes the suspended conversation AND flips launching to
	// running, in one compare-and-swap.
	//
	// ONE WRITE, because it is one fact: the run becomes resumable and
	// becomes pollable at the same instant. Two writes leave a state a
	// reader can see — a `running` row with no conversation — and the
	// reader is the completion poll, which fires on it. See
	// [StatusLaunching] for what that cost.
	//
	// Reports whether the flip happened. FALSE IS NOT AN ERROR and is not a
	// lost race either — it is a run that is no longer launching, which is
	// left alone rather than overwritten: re-arming a row whose tail has
	// already been claimed would hand a redelivered completion a second
	// resume of a turn that is over. The caller has a suspended
	// conversation with nowhere to put it, so the run cannot be resumed and
	// must be failed rather than left holding a box.
	MarkSuspended(ctx context.Context, turnID string, s Suspension) (bool, error)

	// AppendBridgeCall records one tool call a bridged run made through the
	// MCP bridge, so the reviewer of a run that outlived its process still
	// has the tool log. See [BridgeCall].
	//
	// NO FENCE, unlike every other mutation here: this is a log append
	// rather than an ownership decision, and refusing to record a call that
	// already ran because the seat's lease moved would lose evidence of
	// something that is true either way.
	//
	// THE SESSION'S SPEND RIDES THE SAME WRITE: what its bridged calls have
	// cost the engine so far ([BridgeAppend.Spent]), merged onto the job's
	// record by [EngineSpend.Newest], so the segment that resumes from the
	// job can pay it ([LaunchRecord.Bridged]).
	//
	// PINNED TO ONE JOB. Every append names the job its session serves
	// ([BridgeAppend.Launch]), and is recorded only while the row holds that
	// job. A call still in flight when its job was REPLACED — a worker that
	// outlived a killed CLI, finishing after the reviewer relaunched — is not
	// recorded: it would land its call and its spend on the next job's
	// record, and that job's resume would pay for it as its own. One that
	// finishes after its job's resume CLAIMED it, and before any relaunch, is
	// recorded on its own job and is NOT PAID: the log is evidence, and a
	// resume that fails and is retried claims again and reads it, but the
	// segment that pays for a job pays what the job's record held at its
	// claim ([ResumeRequest.Engine]). So a late call can leave the task short
	// of the turn's cost, never past it.
	//
	// Reports whether the call was recorded. FALSE IS NOT AN ERROR: it is a
	// run whose row is gone, or a job that is over — the ordinary shape of a
	// late call from a box that is shutting down — and the caller must not
	// fail the box's call over it.
	AppendBridgeCall(ctx context.Context, turnID string, a BridgeAppend) (bool, error)

	// ListActive returns every run that still owns engine-side state.
	ListActive(ctx context.Context) ([]PendingRun, error)

	// ListActiveForSeat is the "is this seat busy?" read.
	ListActiveForSeat(ctx context.Context, handle string) ([]PendingRun, error)

	// RecordAnswer records a person's reply as the answer to the question a
	// run is parked on, flipping it to [StatusAnswered], and hands back the
	// row IFF THIS CALL WON.
	//
	// A COMPARE-AND-SET, because the first qualifying reply is the answer
	// and every later one is not: the run must still be [Awaiting], on the
	// launch the caller matched, with no answer recorded and none of the
	// reply's deliveries among [PendingRun.DeclinedAnswers]. Two replies
	// racing for one question — on two nodes across a seat handoff, or a
	// chat reply and an answer by turn — resolve here, and the loser reads
	// false. FALSE IS NOT AN ERROR.
	//
	// UNDER THE RECORDING NODE'S LEASE, superseded rather than outranked as
	// a claim is ([PendingStore.ClaimForResume]): a row a newer lease than
	// the fence owns — the zero fence included, on a row any lease owns — is
	// refused with [ErrSeatNotHeld], because the answer is the seat holder's
	// to record and drive. Unfenced, a node that lost the seat recorded an
	// answer on a run its successor had recovered as awaiting, and nothing
	// drove it until the seat moved again. The lease is not stamped:
	// recording is not a claim of the run.
	RecordAnswer(ctx context.Context, turnID, launch string, answer RecordedAnswer, fence Fence,
	) (PendingRun, bool, error)

	// DeclineAnswer lets go of a recorded answer the run could not be
	// resumed with, IN ONE WRITE: the run goes back to the status the record
	// took it out of, the answer is cleared, its deliveries and the ids of
	// the copies handed back for them join [PendingRun.DeclinedAnswers], and
	// the copies themselves join [PendingRun.HandBack] for the caller to
	// publish. Only while the run is still [StatusAnswered] on that launch,
	// holding exactly the answer made of the deliveries named, and no newer
	// lease outranks the fence. Returns the row as written IFF THIS CALL
	// DID. FALSE IS NOT AN ERROR.
	//
	// AND ONLY ON A ROW THAT OWES THE SEAT NOTHING YET: one that still
	// carries an earlier decline's unpublished copies refuses with
	// [ErrHandBackOwed], which is what bounds [PendingRun.HandBack] — see
	// there.
	DeclineAnswer(ctx context.Context, turnID, launch string, answer []string,
		handBack []HandedBack, fence Fence) (PendingRun, bool, error)

	// TakeAnswer records that the resumed turn a claim drives TOOK the
	// recorded answer it was claimed for ([RecordedAnswer.TakenAt]),
	// reporting whether the run is still that claim.
	//
	// The write a resume makes at the last moment before its turn runs
	// ([ResumeRequest.Begin]), and a resume that cannot make it does not
	// run: a turn that ran without it would be read, by whoever reaps the
	// row after a crash, as one that never got the answer — and the reply
	// handed back to the seat a second time.
	//
	// Only while the run is [StatusResumed] on that launch, still carrying
	// the answer, its ending not yet decided ([PendingStore.DecideEnding],
	// which is what makes an ending's let-go of the answer certain) — and
	// the fence is the row's own lease or a newer one: a seat's next
	// holder fences the row to its own lease ([PendingStore.ClaimOwnership])
	// before an ending lets the reply go, so a process that lost the seat
	// can never take an answer the holder is about to return. That holds
	// because the fence is the CLAIMANT's — the lease its claim stamped on
	// the row ([PendingStore.ClaimForResume]) — and because, UNLIKE EVERY
	// OTHER WRITE HERE, A ZERO FENCE IS NOT EXEMPT: a take under no lease is
	// refused on a row any lease owns. A zero fence constrains nothing
	// elsewhere so a recovery that holds no lease yet can still write, and
	// no recovery takes an answer; exempted here, a stalled claimant that
	// held no lease took the answer on a row its successor had fenced, and
	// the person was answered by its turn and by the copy. FALSE IS NOT AN
	// ERROR. A run whose answer is already taken answers true and is not
	// written again.
	TakeAnswer(ctx context.Context, turnID, launch string, fence Fence) (bool, error)

	// ReviveAnswer gives a claim no resume holds back to the recorded answer
	// it was taken for: the run returns to [StatusAnswered], owed the resume
	// the answer drives, and the reviving node drives it. In the same write
	// the row is FENCED to the reviving node's lease and — for a claim whose
	// node stopped before its turn took the answer ([Revival.Lost]) — the loss
	// COUNTED on the answer ([RecordedAnswer.LostClaims],
	// [RecordedAnswer.FirstLostAt]); a claim its own node made and could not
	// confirm is given back uncounted. Returns the row as written IFF THIS
	// CALL DID.
	//
	// Only while the run is still that claim — [StatusResumed] on that
	// launch, holding exactly that answer, its ending not decided — and no
	// newer lease outranks the fence. FALSE IS NOT AN ERROR.
	//
	// EXCLUSIVE WITH THE TAKE, as an ending's let-go is: an answer a turn
	// already took has been used, and is refused with [ErrAnswerTaken] and the
	// row as it stands — the caller reaps the claim as spent — while a take
	// after the revival finds no claim to take it under.
	ReviveAnswer(ctx context.Context, turnID string, revival Revival) (PendingRun, bool, error)

	// OweHandBack LETS GO of the recorded answer a run whose ending is
	// decided still holds: in one write, the answer leaves the row and the
	// copies of its deliveries are recorded as owed to the seat's inbox
	// ([PendingRun.HandBack]), for the ending to publish before it deletes the
	// row ([PendingStore.Finish]). Returns the row as written IFF THIS CALL
	// DID.
	//
	// It is the decline's outbox for the other ways a recorded answer is
	// let go of: a run that ends before any turn took the reply — no
	// conversation to re-enter, a claim that could not be given back, a
	// resume that broke before its turn began, a claim the seat's next holder
	// reaps because the run cannot be resumed with it, a seat that left the
	// company. Every one of them has nowhere to send the reply but the seat's
	// inbox, because the delivery that brought it was spent when the answer
	// was recorded.
	//
	// ONLY A STEP OF THE ENDING NAMED ([LetGo.Ending]), on the answer named,
	// and FALSE IS NOT AN ERROR otherwise. The ending was licensed when it was
	// decided, and the row has taken no other write since — in particular no
	// TAKE of the answer ([PendingStore.TakeAnswer]), which the decision
	// fences off — so whether the answer goes back is settled by the decision
	// itself: one no turn took before it does, and one a turn took does not,
	// unless the ending says it went unused ([RecordedEnding.Unused]). A taken
	// answer the ending must not let go is refused with [ErrAnswerTaken].
	//
	// A row that already owes copies refuses with [ErrHandBackOwed] and the
	// row as it stands, for the bound [PendingRun.HandBack] states. An answer
	// none of whose deliveries could be carried owes no copy, and is let go
	// of with nothing to hand back — what its decline does too.
	OweHandBack(ctx context.Context, turnID string, letGo LetGo) (PendingRun, bool, error)

	// ClearHandBack removes the copies a decline owed the seat's inbox that
	// have now been published, by their ids, and reports whether it removed
	// any. Unconditioned on status, launch or lease: what it records is a
	// fact about the broker, true whoever published them. FALSE IS NOT AN
	// ERROR — a run that is gone, or copies a peer already cleared.
	ClearHandBack(ctx context.Context, turnID string, ids []string) (bool, error)
}

// ConversationRef is where an arriving delivery came from, as a parked run is
// matched against it: the durable conversation it belongs to, and the inbox
// partition it arrived in.
//
// TWO VALUES, AND EACH DECIDES A DIFFERENT HALF. The identity decides WHICH
// runs a delivery may answer, because that is where a person answers. The
// partition decides WHICH OF THEM it answers when several may: the identity is
// coarse on purpose — every run parked on one direct message shares it — so
// without the batch the two halves of a DM's clarification are told apart by
// nothing but creation time. See [ConversationRef.Best], which is the whole
// rule.
//
// A struct rather than two arguments because both are strings and a swapped
// pair fails silently — as a run nobody can answer, which is the defect this
// type exists to end.
type ConversationRef struct {
	// Identity is the durable conversation — what notify derives for the
	// delivery's partition, and what the row's ConversationKey holds.
	Identity string

	// Partition is the inbox batch the delivery arrived in — what the row's
	// PartitionKey holds.
	Partition string
}

// Answers reports whether this delivery is the reply that parked run is
// waiting for.
//
// ON THE IDENTITY, because that is where a person answers. The engine's own
// chat prompt tells a seat replying to a top-level direct message to reply AS
// A THREAD, so the answer to a question asked from such a turn arrives in a
// thread — a FINER partition than the bare channel the run parked under — and
// under equality on the partition the two never met. A direct message is ONE
// conversation however it is threaded, which is exactly what the identity
// says, so matching on it is what makes the answer arrive at all.
//
// THE WIDENING IS THE REPAIR, and the biggest part of it is the case read
// from the other end: a run parked from a THREAD on a direct message holds
// the bare channel as its identity, so a TOP-LEVEL reply on that line now
// answers it where before only a reply in that same thread could. A DM is one
// line, and a person answering the question they were asked on it does not
// owe the engine a thread — so both directions of the miss close together,
// and stating only the headline one hides the half that changes which
// deliveries reach a parked run at all.
//
// It widens nothing elsewhere: a partition key is always its identity or a
// finer cut of it, so on every other source the two coincide and this is the
// same match it always was.
//
// WIDER ALSO MEANS SEVERAL ROWS CAN BE ADMITTED — every run parked on one DM
// line is, so a line carrying more than one parked question admits them all
// — and admitting is not choosing: [ConversationRef.Best] picks between what
// this admits, on the partition first and recency second.
//
// AN EMPTY VALUE NEVER MATCHES, on either side. A run launched by a schedule
// tick or an A2A wake stored no conversation, and a wake that could not name
// one carries none — so the one explicit check below is the one place two
// absences would otherwise compare equal and make every such delivery the
// answer to every such run.
func (c ConversationRef) Answers(run PendingRun) bool {
	return run.ConversationKey != "" && run.ConversationKey == c.Identity
}

// sameBatch reports whether this delivery arrived in the very batch the run
// was launched from.
//
// The empty check is the one [ConversationRef.Answers] explains: two absences
// comparing equal would prefer every partition-less run alike.
func (c ConversationRef) sameBatch(run PendingRun) bool {
	return c.Partition != "" && run.PartitionKey == c.Partition
}

// Best is the parked run a delivery answers, out of the runs one seat has
// waiting — the whole rule, so that a store hands over its candidates and
// decides nothing of its own.
//
// TWO STAGES, AND THE SECOND IS WHY THIS IS NOT JUST [ConversationRef.Answers]
// IN A LOOP. The identity ADMITS, because that is where a person answers; the
// partition DISAMBIGUATES, because the identity is deliberately coarse. In a
// direct message every run parked on that channel shares one identity, so two
// runs parked from two threads are both admitted by a reply in either of them,
// and picking by recency alone resumes whichever asked LAST — the answer to
// the question in thread A spliced into the run waiting in thread B, with the
// arriving partition and each row's own partition holding exactly the fact
// that would have told them apart.
//
// RECENCY IS THE LAST WORD, not the first: among candidates that agree on the
// partition — the ordinary case of two questions asked in one thread — the
// person is replying to what they were just asked. The turn id breaks a tie
// between two runs created in the same instant, so the answer is the same on
// every node and on every read rather than depending on a map's order.
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

// Clarification is what a parked run is waiting for.
type Clarification struct {
	Question string
	// Audience is who should answer: "requester", "team", "manager", or a
	// handle. Carried rather than derived, because the coding agent chose
	// it and the engine has no better information about who knows.
	Audience string
	// Branch is the WIP pushed before parking — the durable half of the work
	// while the question waits.
	Branch    string
	SessionID string

	// InputTokens and OutputTokens are what the job that asked cost,
	// carried to the resume its answer drives — see
	// [PendingRun.ParkedInputTokens].
	InputTokens  int
	OutputTokens int

	// Condensed is what condensing the collection that parked cost the
	// seat's auxiliary model, carried to the same resume — see
	// [LaunchRecord.Condensed].
	Condensed AuxTokens

	// Answerers is who the question may be answered by, resolved from
	// Audience against the chart — see [PendingRun.AudienceHandles].
	Answerers Audience

	// AskedAt is when the question was put, taken before it was announced
	// — see [PendingRun.AskedAt]. Required.
	AskedAt time.Time
}

// RecordedAnswer is a person's reply, recorded on the run it answers before
// anything is done with it — on either route: a chat reply matched on its
// conversation, or an answer by turn that named the run ([RecordedAnswer.Via]).
//
// RECORDING IS NOT RESUMING, and splitting the two is the whole point. The
// reply used to be held by its inbox delivery until a resume succeeded: a
// resume that failed handed the MESSAGE back to the queue, which on the only
// broker this engine ships returns a failure BEHIND the conversation's newer
// mail — so the person's next message reached the still-waiting run first
// and was spliced in as the answer, and the first one came round afterwards
// to answer whatever the run asked next. Recorded here, by a compare-and-set
// that only the first qualifying reply wins, the answer is durable the moment
// the delivery is acknowledged, and retrying the resume is the coordinator's
// job ([Coordinator.retryOwed]) rather than the inbox's.
type RecordedAnswer struct {
	// Text is the reply as the resumed turn is handed it.
	Text string `json:"text"`

	// Via is the route the answer came by: a chat reply matched on its
	// conversation, or an answer by turn that named the run. Required, and
	// refused by [PendingStore.RecordAnswer] when it is not one of the two:
	// it decides how the resumed turn is told who answered, what the
	// answer's record says, and what a copy of it becomes when it is let go
	// of — a chat reply becomes the ordinary message it is, while an answer
	// by turn has no ordinary form, and its copy is spent by the node holding
	// the seat as an answer the run no longer takes.
	Via types.AnswerVia `json:"via"`

	// By is who gave it: the chat sender as the delivery's envelope names
	// them, or the operator credential an answer by turn was given under.
	By string `json:"by,omitempty"`

	// BySeat is the person an answer by turn's credential is bound to, empty
	// for a chat reply and for a credential nobody bound.
	BySeat string `json:"by_seat,omitempty"`

	// EventIDs are the deliveries the answer was made of. A copy of one of
	// them reaching the seat again is the same answer, already recorded —
	// never a second one, and never an ordinary message.
	EventIDs []string `json:"event_ids"`

	// Events are those deliveries, encoded, oldest first: the resume raises
	// the working indicator off the newest one, and an answer this node gives
	// up on hands them back to the seat's ordinary route as what they are.
	Events []json.RawMessage `json:"events,omitempty"`

	// PostedAt is when the newest of them was posted, the instant the
	// question's [PendingRun.AskedAt] was compared against.
	PostedAt time.Time `json:"posted_at,omitzero"`

	// RecordedAt is when the answer was recorded.
	RecordedAt time.Time `json:"recorded_at"`

	// From is the status the record took the run out of — awaiting, or
	// reseed when the pause reaper had already reclaimed the box — and the
	// one an answer that is let go of puts it back to.
	From string `json:"from"`

	// TakenAt is when a resumed turn TOOK this answer: the instant the claim
	// holding it handed it to a turn that was certain to run
	// ([PendingStore.TakeAnswer], at [ResumeRequest.Begin]). Zero while the
	// answer is still the run's — recorded and owed, or claimed by a resume
	// that has not reached its turn yet — and cleared again by a release
	// that gives the claim back for a retry ([PendingStore.ReleaseClaim]).
	//
	// THE ONE FACT A CLAIM THAT DIED CANNOT TELL ANY OTHER WAY. A claimed
	// row reads [StatusResumed] from the instant of the claim to the
	// instant of its settle, and between the two lie both a resume that
	// never got as far as the turn (a crash, a node stopped, a seat moved)
	// and a turn that ran with the answer and died mid-round. The first
	// still owes the person's reply to the seat — its delivery was spent
	// when the answer was recorded, so nothing else will ever bring it back
	// — and the second has used it. The seat's next holder finds both rows
	// as tails nobody drives ([Coordinator.RecoverSeat]), and this is what
	// tells them apart: the first it REVIVES, the answer given back to the
	// run ([PendingStore.ReviveAnswer]) — or, past the revival's bounds,
	// reaps, and the store will not delete it while it holds the reply
	// ([ErrAnswerOwed]), so the reap hands it back — while the second's the
	// store will neither revive nor let go ([ErrAnswerTaken]), so the reap
	// spends it. Without it, every such reply was read as spent and lost.
	//
	// WRITTEN AT THE LAST MOMENT BEFORE THE TURN, never at the claim: every
	// step between the claim and the turn can fail or stop the process, and
	// a marker written earlier would read a reply nobody acted on as used.
	// And the reply's delivery is recorded as worked at the same moment
	// ([CoordinatorOptions.Spent]), because from here a copy of it reaching
	// the seat is the reply a turn already has.
	TakenAt time.Time `json:"taken_at,omitzero"`

	// LostClaims counts the claims of this answer whose node stopped before
	// their turn took it, each revived by the seat's next holder so that the
	// answer reaches the run it answered ([PendingStore.ReviveAnswer]), and
	// FirstLostAt is when the first of them was revived. A claim its own node
	// made, could not confirm and gave back is none of them ([Revival.Lost]):
	// no node stopped. ON THE ROW, because
	// what they bound is a series no one node sees: a claim that dies is a
	// node that stopped, and the count a node keeps of its own attempts
	// resets with exactly that ([MaxAnswerAttempts]). See [MaxAnswerRevivals].
	LostClaims  int       `json:"lost_claims,omitempty"`
	FirstLostAt time.Time `json:"first_lost_at,omitzero"`
}

// Taken reports whether a resumed turn took this answer — see
// [RecordedAnswer.TakenAt].
func (a RecordedAnswer) Taken() bool { return !a.TakenAt.IsZero() }

// attribution is who the resumed turn is told gave this answer: the person an
// answer by turn names — or its credential, where nobody is bound to it — and
// nobody for a chat reply, whose sender is already in the conversation the
// resumed turn reports back to.
func (a RecordedAnswer) attribution() string {
	if a.Via != types.AnswerViaOperator {
		return ""
	}
	if a.BySeat != "" {
		return a.BySeat
	}
	return a.By
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

// Audience is who a parked question may be answered by: the seats, and
// whether they are a fallback rather than what the run asked for.
//
// FALLBACK IS A FACT A READER NEEDS, not an apology. A question addressed to
// "manager" on a seat with no manager, or to a name nobody in the chart has,
// still waits on somebody — the seat's lead chain — and a person reading "what
// is waiting on me" has to be able to tell a question put to them from one
// that reached them because nobody else could be named.
type Audience struct {
	Handles  []string
	Fallback bool
}

// BoxRef is the box and command a run is attached to.
type BoxRef struct {
	SandboxID   string
	CommandID   string
	CodingAgent string
	SessionID   string
	PauseTTLSec float64
}

// Fence is the ownership token a mutation carries.
//
// A ZERO FENCE MEANS UNFENCED and is deliberate rather than a default: recovery
// writes and the boot pass legitimately have no lease yet, and a node with no
// seat host has no next holder to be fenced out by. What must never happen is a
// node writing under a lease it has LOST, and that is the case a non-zero fence
// closes. So the writes only a seat's holder makes — a claim, the take of an
// answer, the record of one — refuse the zero fence too on a run any lease owns
// ([PendingStore.ClaimForResume]), and a node that knows it does not hold the
// seat never reaches them: its lease seam says so rather than answering the zero
// fence ([CoordinatorOptions.Lease], [ErrSeatNotHeld]).
type Fence struct {
	Owner string
	Epoch int64
}

// Fenced reports whether this token constrains anything.
func (f Fence) Fenced() bool { return f.Epoch > 0 }

// Suspension is what [PendingStore.MarkSuspended] writes: the suspended
// Execute loop and the turn iteration it suspended in.
//
// The iteration travels BESIDE the conversation rather than being read back
// out of it, because the conversation is opaque here by design — the
// coordinator carries it back to the engine without ever decoding it, and a
// field reached into it would be a second reader of a format this package
// does not own. The run's phase record is keyed on it (see [LaunchRecord]).
type Suspension struct {
	// State is the serialized loop, [PendingRun.ExecuteState].
	State json.RawMessage

	// Iteration is the turn iteration the executor suspended in, which is
	// the iteration the run's own phase record is filed under.
	Iteration int
}

// LaunchRecord is what one job's own phase record needs that nothing else on
// the row says: when it started, which executor iteration launched it, the
// model it ran on, and whether that record has already been published.
//
// THE JOB'S, NOT THE TURN'S. The row is the turn's and outlives each job on
// it, so [PendingStore.BeginLaunch] replaces this record WHOLE on every launch,
// in the same write that names the job ([PendingRun.LaunchID]): a record that
// survived a relaunch would tell the next job it had been launched at the
// previous one's instant and already published.
//
// ONE FIELD RATHER THAN FOUR for the same reason: a fact added here later is
// replaced with its job by construction.
type LaunchRecord struct {
	// StartedAt is when the job was launched, on the store's clock —
	// written by [PendingStore.BeginLaunch], the moment the launch exists.
	StartedAt time.Time `json:"started_at,omitzero"`

	// Model is the model the coding agent was pointed at, empty when the
	// launch named none and the CLI chose its own.
	Model string `json:"model,omitempty"`

	// Iteration is the turn iteration the launching executor suspended in,
	// written by [PendingStore.MarkSuspended].
	Iteration int `json:"iteration,omitempty"`

	// Published is whether the job's `agent_phase_completed{phase: sandbox}`
	// record went out.
	//
	// ONCE PER JOB, and on the row, for exactly the reason
	// [PendingRun.Charged] is: the publish sits inside the part of the
	// completion tail that is retried, the retry may run on another node or
	// after a restart, and every reader of that record — the spend rollup,
	// each node's daily usage — counts it as spend. A second copy is a
	// second charge on every surface that shows one. WRITTEN BY THE
	// RELEASE ([Release.Published]), the one write through which a retry
	// reaches the publish again.
	Published bool `json:"published,omitempty"`

	// CollectFailures is how many collections of this job have failed to
	// read its box back SINCE THE LAST ONE THAT DID, and CollectFailingSince
	// when the first of those failed — the run of consecutive failures
	// [Coordinator.OnCompleted] bounds before it gives the job up as
	// unreachable (see [Coordinator.collectFailed]). A collection that read
	// the box ends the run ([Release.Collected]).
	//
	// ON THE JOB'S RECORD for the reason Published is: each failure hands
	// the claim back, and the retry that follows may run on another node or
	// after a restart, so a count held in memory would grant every node a
	// fresh allowance. WRITTEN BY THE RELEASE ([Release.CollectFailedAt]),
	// the one write through which a retry reaches the collection again.
	CollectFailures     int       `json:"collect_failures,omitempty"`
	CollectFailingSince time.Time `json:"collect_failing_since,omitzero"`

	// Bridged is what this job's BRIDGED calls cost the engine — the
	// auxiliary calls the seat's tools made for the coding agent and the
	// workers it delegated to — as the newest of the totals the bridge
	// wrote with its calls ([PendingStore.AppendBridgeCall]). The segment
	// that resumes from this job pays it to the turn's work item
	// (ADR-0022), because no segment's tally is open while a job is out.
	//
	// ON THE JOB'S RECORD for the reason every fact here is: the resume may
	// run on another node, after a restart, and a relaunch must not hand
	// one job's spend to the next.
	Bridged EngineSpend `json:"bridged_spend,omitzero"`

	// Condensed is what condensing the collection that PARKED this job on
	// a question cost the seat's auxiliary model ([Result.Condensed]),
	// written with the question ([PendingStore.MarkAwaiting]) and paid by
	// the resume the answer drives, as [PendingRun.ParkedInputTokens] is.
	Condensed AuxTokens `json:"parked_condensed,omitzero"`
}

// AuxTokens is what calls to a seat's AUXILIARY model cost, split the way
// every token figure in the engine is: the whole input, the share of it the
// provider's prompt cache served and stored (a breakdown, never an addition),
// and the output.
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

// EngineSpend is what the ENGINE spent on a job outside every segment of the
// turn it belongs to: its own auxiliary calls, and the workers it ran for the
// job. A coding run's OWN tokens are not here — they are the job's, reported
// by its CLI ([Result.InputTokens]).
//
// Two things put it there, and both happen while no segment is running to
// tally them: an agent-mode run's calls through the tool bridge, which keep
// running after the segment that launched it has ended, and the condensation
// of a collected run's account ([Condenser]), which the coordinator makes
// between two segments. The segment that resumes from the job pays it
// ([ResumeRequest.Engine]).
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

// Newest is the larger of s and o in each figure: of two totals one meter
// took at different moments, the later one, whichever was WRITTEN later.
//
// The bridge writes its meter's running total with every call it records, and
// two calls finishing together race to the row — so the total the earlier
// call read can land after the later one's. Every figure only grows within one
// meter, so the larger is the later, and a merge by this never moves a total
// backwards.
func (s EngineSpend) Newest(o EngineSpend) EngineSpend {
	return EngineSpend{Aux: s.Aux.newest(o.Aux), Workers: max(s.Workers, o.Workers),
		WorkerInput: max(s.WorkerInput, o.WorkerInput), WorkerOutput: max(s.WorkerOutput, o.WorkerOutput)}
}

// Handle is the job this row holds now, as every read of its box is handed
// it: its command and its session.
func (r PendingRun) Handle() RunHandle {
	return RunHandle{CommandID: r.CommandID, SessionID: r.SessionID}
}
