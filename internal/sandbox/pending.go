package sandbox

import (
	"context"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/workkey"
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

	// StatusResumed — the tail has been claimed. THE AT-MOST-ONCE GATE.
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
var Claimable = []string{StatusRunning, StatusAwaiting, StatusReseed}

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

// AnswerTail is what an answer claims: the job that asked, while it waits.
func AnswerTail(launch string) Tail {
	return Tail{Launch: launch, From: Awaiting}
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

	// Fence is the lease the claim was taken under.
	Fence Fence
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
var Active = []string{
	StatusLaunching, StatusRunning, StatusAwaiting, StatusReseed, StatusResumed,
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

// UnitOfWork is the identity this run's once-per-unit-of-work writes collapse
// on, or "" when the run has none.
//
// NOT the raw field, because nothing rewrites a parked row: a run suspended by
// a build from before ADR-0017 carries no work key at all, and its TurnID IS
// one. A resume days later reads that row and has no trigger left to
// re-derive from, so without this its conversation entry and its tracker
// writes would dedupe against an empty key.
//
// ON SHAPE rather than on absence, for the reason [workkey.IsDerived] gives:
// a post-split run with no ledgerable trigger is also missing the field, and
// answering it with a run id would arm a dedupe guard with a value that means
// nothing.
func (r PendingRun) UnitOfWork() string {
	if r.WorkKey != "" {
		return r.WorkKey
	}
	if workkey.IsDerived(r.TurnID) {
		return r.TurnID
	}
	return ""
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
	// no trigger left to re-derive it from. Empty on a row written before
	// this field existed, and on a turn with no ledgerable trigger, which
	// is the documented "nothing to collapse" case.
	WorkKey string `json:"work_key,omitempty"`

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
	// abandoned as gone. A row written before this field existed decodes
	// empty, which the manager reads as the provider default.
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
	// status is: a caller that could choose it could reuse one. Empty on a
	// row a build that predates it wrote, and a completion from such a
	// build carries none, so the two still match each other and nothing
	// else.
	LaunchID string `json:"launch_id,omitempty"`

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
	// would come back from its coding run free to end in silence. An empty
	// value decodes as "nobody is waiting", which is the safe half — see
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
	// It is also written for two PEER reasons of its own. It is the only
	// conversation value a row from before the split carries, so it is what
	// such a row degrades to matching on; and a node still running that
	// build matches every row — including the ones written here — on it by
	// equality, so dropping it would strand a run whose answer lands on the
	// other half of a mixed fleet.
	//
	// THE GO NAME MOVED WITH THE CONCEPT; THE WIRE STRING DID NOT. This
	// field was ConversationKey and its column is still "conversation_key",
	// because the value is what two builds exchange through one
	// coordination record while the name is only what this build calls it:
	// a peer that predates the split writes and matches on that column, and
	// a parked row outlives any upgrade window by design, since it is
	// waiting for a person. Same trade [notify.PartitionField] makes for
	// the event payload's copy of it.
	PartitionKey string `json:"conversation_key"`

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
	// ITS COLUMN IS conversation_identity, which is the name the split gave
	// it on the wire and the one a peer already writes; only the Go name
	// moved, onto the concept it holds and away from the partition beside
	// it. See [PartitionKey], which made the same trade in the other
	// direction.
	//
	// ADDITIVE on this row, which is what a coordination-KV record needs:
	// nothing rewrites a parked run, so a run launched by an older build
	// decodes with this empty and BOTH readers fall back to the field that
	// is there — [PendingRun.Conversation] for the report-back and
	// [ConversationRef.Answers] for the match. Omitted when empty for the
	// same reason.
	ConversationKey string `json:"conversation_identity,omitempty"`

	// Branch is the pushed WIP branch: the durable half of the work, and
	// what a re-seeded run starts from when its snapshot is gone.
	Branch    string `json:"branch"`
	SessionID string `json:"session_id"`

	Question string `json:"question"`
	Audience string `json:"audience"`

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
	// WITHOUT one can now only mean the row was written by a build that
	// predates the launching state, which the coordinator fails rather
	// than resuming into nothing.
	ExecuteState map[string]any `json:"execute_state"`

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

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
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
// is what made such a box invisible to [Waiter.reapExpiredPauses] for good.
//
// THE FALLBACK IS THE ROW'S OWN LAST WRITE, because on a parked row that write
// IS the park — the one write that has to land for the run to be parked at all
// ([Coordinator.park] gives its claim back where it does not) — and it lands
// milliseconds after the pause it failed to record. It is therefore never
// EARLIER than the true pause instant, which is the safe direction to be
// wrong in: the box is held a moment longer rather than reclaimed out from
// under a person who is still typing.
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
	if !slices.Contains(Awaiting, r.Status) || r.UpdatedAt.IsZero() {
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
	BeginLaunch(ctx context.Context, run PendingRun, fence Fence) error

	Get(ctx context.Context, turnID string) (PendingRun, bool, error)

	// ClaimForResume atomically flips a run to resumed, when it holds the
	// tail's launch in one of the tail's statuses.
	//
	// Reports the row IFF THIS CALL WON: the at-most-once tail guard, and
	// the reason a claim names its launch (see [Tail]). The returned row
	// carries ClaimedFrom, so a failed dispatch can put it back exactly
	// where it was.
	ClaimForResume(ctx context.Context, turnID string, tail Tail) (PendingRun, bool, error)

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
	// FALSE IS NOT AN ERROR: it is a run that moved on, or a row that is
	// gone. A release to a status outside [Claimable] is an error, because
	// no claim ever takes a run out of one.
	ReleaseClaim(ctx context.Context, turnID string, release Release) (bool, error)

	// MarkAwaiting parks a run on a question, freeing the seat.
	MarkAwaiting(ctx context.Context, turnID string, q Clarification) error

	// ClaimOwnership takes the run for a node, reporting whether it won.
	// A run whose epoch is already higher is not stolen.
	ClaimOwnership(ctx context.Context, turnID, owner string, epoch int64) (bool, error)

	// SetStatus moves a run between the live states, fenced on the epoch.
	// Ending a run is not a status; see Finish.
	SetStatus(ctx context.Context, turnID, status string, fence Fence) error

	// Finish ends a run by deleting its record, unless a newer lease
	// outranks the fence, reporting whether THIS call deleted it.
	//
	// The caller reclaims the box FIRST. A record naming a box that is
	// already gone is harmless (recovery reaps it and a kill of a gone box
	// is a no-op), while a live box whose record was deleted is named by
	// nothing and billed until its provider's TTL.
	//
	// Conditional on the version it read and re-decided on a lost race, so
	// a delete racing a write sees that write before it deletes. FALSE IS
	// NOT AN ERROR: the run is already gone, which is the ordinary shape of
	// two parties reaching the end of one run, or a newer lease owns it.
	Finish(ctx context.Context, turnID string, fence Fence) (bool, error)

	// ExpirePause flips a run parked on a clarification to reseed AND
	// clears its box record, reporting whether THIS call won.
	//
	// The pause reaper's authority, and the reason it is a conditional flip
	// rather than a plain SetStatus. The reaper decides from a snapshot
	// taken seconds ago, and the answer that un-parks the run may have
	// arrived since — ClaimForResume has already moved the row and an
	// Execute loop is reconnecting to that very box. Killing the box before
	// this returns true destroys it underneath that resume. Conditional on
	// StatusAwaiting alone: a run already reseeded has no snapshot left to
	// expire, and any other status means somebody else owns the tail.
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
	MarkSuspended(ctx context.Context, turnID string, state map[string]any) (bool, error)

	// AppendBridgeCall records one tool call a bridged run made through the
	// MCP bridge, so the reviewer of a run that outlived its process still
	// has the tool log. See [BridgeCall].
	//
	// NO FENCE, unlike every other mutation here: this is a log append
	// rather than an ownership decision, and refusing to record a call that
	// already ran because the seat's lease moved would lose evidence of
	// something that is true either way.
	//
	// Reports whether the append landed. FALSE IS NOT AN ERROR: it is a run
	// whose row is gone, which is the ordinary shape of a late call from a
	// box that is shutting down, and the caller must not fail the box's
	// call over it.
	AppendBridgeCall(ctx context.Context, turnID string, call BridgeCall) (bool, error)

	// ListActive returns every run that still owns engine-side state.
	ListActive(ctx context.Context) ([]PendingRun, error)

	// ListActiveForSeat is the "is this seat busy?" read.
	ListActiveForSeat(ctx context.Context, handle string) ([]PendingRun, error)

	// FindAwaitingByConversation matches a person's answer back to the run
	// that asked, on the CONVERSATION the question was asked in.
	//
	// The rule is [ConversationRef.Best] and lives there rather than in an
	// implementation, because it is a statement about two VALUES that every
	// store has to make the same way — which rows a delivery may answer,
	// which of them it answers when several may, and what either does with a
	// row written before the conversation identity existed. A store lists
	// the seat's parked runs and decides none of it.
	FindAwaitingByConversation(ctx context.Context, handle string, conv ConversationRef) (PendingRun, bool, error)
}

// Conversation is the durable conversation this run reports back to.
//
// FALLS BACK to the partition key, for the peer reason
// [notify.ConversationIdentityOf] gives about the event it mirrors: a row
// parked by a build from before the split carries only conversation_key, and
// that value is what such a build would have reported back under. Reading the
// absence as "no conversation" instead would make a resumed turn record
// nothing at all — the very gap [Engine.recordResume] exists to close — and a
// parked run outlives any upgrade window by design, because it waits for a
// person to answer.
func (r PendingRun) Conversation() string {
	if r.ConversationKey != "" {
		return r.ConversationKey
	}
	return r.PartitionKey
}

// ConversationRef is where an arriving delivery came from, as a parked run is
// matched against it: the durable conversation it belongs to, and the inbox
// partition it arrived in.
//
// TWO VALUES, AND EACH DECIDES A DIFFERENT HALF. The identity decides WHICH
// runs a delivery may answer, because that is where a person answers and it is
// the only value a row from before the split can be read as. The partition
// decides WHICH OF THEM it answers when several may: the identity is coarse on
// purpose — every run parked on one direct message shares it — so without the
// batch the two halves of a DM's clarification are told apart by nothing but
// creation time. See [ConversationRef.Best], which is the whole rule.
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
// A ROW WITH NO IDENTITY IS A ROW FROM BEFORE THE SPLIT, and it degrades to
// today's behaviour rather than to a run nobody can answer: its one value is
// compared against the PARTITION, which is what the build that wrote it
// derived and compared. It is compared against the identity too, and that is
// not a second spelling of one rule — such a row launched from a top-level DM
// holds the bare channel, which is precisely what this build calls the
// identity, so reading it that way is what repairs the rows already stranded
// by the defect. Nothing rewrites a parked run and one waits for a person, so
// this row shape outlives any upgrade window.
//
// A DELIVERY WITH NO IDENTITY IS AN EVENT FROM BEFORE THE SPLIT, which is the
// third peer direction and the one neither field's doc covers: a wake
// published by a peer that predates it carries only the partition, so
// [notify.ConversationIdentityOf] falls back to that value and this ref
// arrives with both fields holding the one string that peer derived.
//
// IT IS NEVER NARROWER than the match that peer would have made, and that is
// the fallback earning its place: against a pre-split row both clauses
// compare that string to the row's one value, which is the old equality
// exactly, so reading the absence as "no conversation" instead would refuse
// every one of those answers and leave the box waiting out its pause TTL with
// the reply sitting in the seat's inbox.
//
// IT IS SOMETIMES WIDER, and where it widens it repairs. A peer that predates
// the split derives the bare channel for a TOP-LEVEL direct message — that is
// the partition rule both builds share — and the bare channel is precisely
// what this build calls the identity of that line. So such a delivery answers
// a run this build parked from a thread on it, which its own publisher could
// never have matched. What it cannot repair is that peer's DM THREAD REPLY:
// it stamped the thread and derived no identity for anyone to read, so this
// build reads the thread as the identity and only a run parked in that same
// thread matches. That is the pre-split behaviour, and it is unreachable from
// this end however the reader is written.
//
// It never widens ACROSS conversations either way: every clause compares
// against a value that peer derived from the same channel, so a reply on a
// different line still fails all of them.
//
// AN EMPTY VALUE NEVER MATCHES, on either side. A run launched by a schedule
// tick or an A2A wake stored no conversation, and a wake that could not name
// one carries none — so the one explicit check below is the one place two
// absences would otherwise compare equal and make every such delivery the
// answer to every such run. Everywhere else an empty value simply fails the
// comparison, which is why there is no second guard: a clause that cannot
// decide anything is a claim, not a check.
//
// THE IDENTITY BRANCH ALSO ACCEPTS THE PARTITION, which admits nothing new for
// a well-formed row and rescues one that is not. A row this build wrote from a
// delivery whose partition refines its identity is matched by the identity
// already — equal partitions imply equal identities there, so the second
// clause never decides anything. What it rescues is a row whose stored
// identity is PARTITION-GRAINED: a pre-split row this build resumed and
// re-parked carries the value that build derived in a field this one reads as
// the identity, so a reply in the very thread the question was asked in would
// otherwise match nothing at all — strictly worse than the equality this
// replaced, which would still have found it.
func (c ConversationRef) Answers(run PendingRun) bool {
	if run.ConversationKey != "" {
		return run.ConversationKey == c.Identity || c.sameBatch(run)
	}
	if run.PartitionKey == "" {
		return false
	}
	return run.PartitionKey == c.Identity || run.PartitionKey == c.Partition
}

// sameBatch reports whether this delivery arrived in the very batch the run
// was launched from.
//
// The empty check is the one [ConversationRef.Answers] explains: two absences
// comparing equal would make every conversation-less delivery the answer to
// every conversation-less run.
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
// writes and the boot pass legitimately have no lease yet. What must never
// happen is a node writing under a lease it has LOST, and that is the case a
// non-zero fence closes.
type Fence struct {
	Owner string
	Epoch int64
}

// Fenced reports whether this token constrains anything.
func (f Fence) Fenced() bool { return f.Epoch > 0 }
