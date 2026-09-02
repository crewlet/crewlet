package sandbox

import (
	"context"
	"time"
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

	StatusDone   = "done"
	StatusFailed = "failed"

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

// allStatuses is the closed set [PendingStore.SetStatus] accepts.
//
// Asserted rather than assumed: a status nothing recovers from is a status
// that leaks a box, so a typo has to be refused at the write instead of
// becoming a row no recovery pass matches.
var allStatuses = []string{
	StatusLaunching, StatusRunning, StatusAwaiting, StatusResumed,
	StatusDone, StatusFailed, StatusReseed,
}

// Active are the statuses that still own engine-side state — a seat, a box, or
// a pending tail — and so must survive a restart.
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
	TurnID      string `json:"turn_id"`
	AgentHandle string `json:"agent_handle"`
	AgentID     string `json:"agent_id"`
	Role        string `json:"role"`

	SandboxID   string `json:"sandbox_id"`
	CodingAgent string `json:"coding_agent"`
	CommandID   string `json:"command_id"`
	Status      string `json:"status"`

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

	// ConversationKey is where to report back AND what matches a person's
	// answer to this run.
	ConversationKey string `json:"conversation_key"`

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

	PauseTTLSeconds float64 `json:"pause_ttl_seconds"`

	// PausedAt is when this run's box was paused, zero when it is not.
	// Together with SandboxID it is the engine's record of the box, and
	// what lets the reaper reclaim a snapshot nothing else would ever free.
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

// Paused reports whether this run's box is currently snapshotted.
func (r PendingRun) Paused() bool { return !r.PausedAt.IsZero() }

// HasBox reports whether a box exists for this run at all.
func (r PendingRun) HasBox() bool { return r.SandboxID != "" }

// PendingStore is the persistence surface for detached runs.
//
// An interface because there are two implementations under one contract suite
// — the SQL store and a memory twin — and because the coordinator's hardest
// properties (at-most-once claim, epoch fencing) are properties of the
// STATEMENTS, which is what makes running both against one suite worth doing.
type PendingStore interface {
	// BeginLaunch opens a launch on this turn's row: it creates the row
	// when there is none, and RESETS an existing one to launching —
	// clearing the previous job's suspended conversation and the question
	// it was parked on, while keeping the row's identity and its box.
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

	// ClaimForResume atomically flips a claimable status to resumed.
	//
	// Reports the row IFF THIS CALL WON — the at-most-once tail guard.
	// The returned row carries ClaimedFrom, so a failed dispatch can put
	// it back exactly where it was.
	ClaimForResume(ctx context.Context, turnID string) (PendingRun, bool, error)

	// MarkAwaiting parks a run on a question, freeing the seat.
	MarkAwaiting(ctx context.Context, turnID string, q Clarification) error

	// ClaimOwnership takes the run for a node, reporting whether it won.
	// A run whose epoch is already higher is not stolen.
	ClaimOwnership(ctx context.Context, turnID, owner string, epoch int64) (bool, error)

	// SetStatus moves the run, fenced on the epoch.
	SetStatus(ctx context.Context, turnID, status string, fence Fence) error

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
	// that asked.
	FindAwaitingByConversation(ctx context.Context, handle, conversation string) (PendingRun, bool, error)

	// ListPausedBefore returns paused runs whose snapshot is older than the
	// cutoff, for the reaper.
	ListPausedBefore(ctx context.Context, cutoff time.Time) ([]PendingRun, error)

	Delete(ctx context.Context, turnID string) error
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
