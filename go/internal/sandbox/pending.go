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
	// StatusRunning — the job is executing and the seat is BUSY.
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
var Claimable = []string{StatusRunning, StatusAwaiting, StatusReseed}

// Awaiting are the statuses still waiting on a person's answer, matched back
// by conversation.
var Awaiting = []string{StatusAwaiting, StatusReseed}

// Active are the statuses that still own engine-side state — a seat, a box, or
// a pending tail — and so must survive a restart.
//
// RESUMED IS HERE, which looks wrong and is not: boot recovery has to be able
// to SEE a tail that died mid-flight with the previous engine. Nothing else
// would ever look at that row again, and its paused box would leak for ever.
var Active = []string{StatusRunning, StatusAwaiting, StatusReseed, StatusResumed}

// PendingRun is one detached job's durable state, keyed by its kick-off turn.
type PendingRun struct {
	TurnID      string
	AgentHandle string
	AgentID     string
	Role        string

	SandboxID   string
	CodingAgent string
	CommandID   string
	Status      string

	// Owner is the process INCARNATION that owns this run's seat, and
	// OwnerEpoch the seat lease's epoch at the moment of the claim.
	//
	// The epoch is the FENCING TOKEN: every mutation on a live run carries
	// it, so a node whose lease moved cannot write even if it has not
	// noticed yet. The ownership check is an optimisation; the fence is the
	// guarantee. Empty owner means unclaimed — what an in-flight run looks
	// like the instant before its seat's new owner recovers it.
	Owner      string
	OwnerEpoch int64

	Plan            map[string]any
	TaskDescription string
	SuccessCriteria []string

	// ConversationKey is where to report back AND what matches a person's
	// answer to this run. NotificationMetadata carries the trigger's channel
	// and thread, so the reply lands in the conversation that asked.
	ConversationKey      string
	NotificationMetadata map[string]any

	// Branch is the pushed WIP branch: the durable half of the work, and
	// what a re-seeded run starts from when its snapshot is gone.
	Branch    string
	SessionID string

	Question string
	Audience string

	// TraceID and SpanID are the trace the run started under, so the
	// follow-up turn nests beneath it rather than appearing as unrelated
	// work minutes later.
	TraceID string
	SpanID  string

	BudgetRemaining int
	DelegationDepth int
	DelegationChain []string

	// ExecuteState is the SUSPENDED Execute conversation: the serialized
	// messages, including the assistant turn with the dangling tool call,
	// plus the surface bookkeeping needed to re-enter the loop where it
	// stopped. Empty only when a crash landed between launch and the
	// suspend persist — the coordinator then fails the run rather than
	// resuming into nothing.
	ExecuteState map[string]any

	PauseTTLSeconds float64

	// PausedAt is when this run's box was paused, zero when it is not.
	// Together with SandboxID it is the engine's record of the box, and
	// what lets the reaper reclaim a snapshot nothing else would ever free.
	PausedAt time.Time

	// ClaimedFrom is TRANSIENT and never persisted: the status the row held
	// immediately BEFORE a claim flipped it to resumed. A failed resume
	// dispatch reverts to exactly this, so a NAK'd trigger can re-claim on
	// redelivery. Inferring it from the other fields is unsound — a reused
	// run keeps its old question.
	ClaimedFrom string

	CreatedAt time.Time
	UpdatedAt time.Time
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
	// Create persists a new running row, idempotently on the turn id.
	Create(ctx context.Context, run PendingRun) error

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

	// AttachSandbox records the box and the command a run is using.
	AttachSandbox(ctx context.Context, turnID string, box BoxRef, fence Fence) error

	// MarkBoxPaused and ReleaseBox are the two halves of the box record:
	// paused_at set means a snapshot exists and is being billed for,
	// cleared means the box is gone.
	MarkBoxPaused(ctx context.Context, turnID string, at time.Time) error
	ReleaseBox(ctx context.Context, turnID string) error

	// SaveExecuteState persists the suspended conversation.
	SaveExecuteState(ctx context.Context, turnID string, state map[string]any) error

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
