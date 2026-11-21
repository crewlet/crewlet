package statelog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/recordseal"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/words"
)

// ReorderBufferBytes bounds what a strict loop holds while it waits for a
// missing sequence: two full fetches plus slack. The broker redelivers the
// missing record, so an out-of-order arrival is not a hole.
//
// OVERFLOW IS A FAULT, not a gap to step over: applying past a hole produces
// state no other node holds.
const ReorderBufferBytes = 64 << 20

// Message is one record as the broker delivered it.
type Message struct {
	// Seq is the broker's sequence.
	Seq uint64

	// StoredAt is the broker's own timestamp, one instant on every node. The
	// client reports it at nanoseconds in the local zone, so the loop hands an
	// applier [AppliedInstant] of it rather than this.
	StoredAt time.Time

	// Payload is the record's bytes.
	Payload []byte

	// Ack acknowledges this delivery. Called only AFTER the transaction
	// that consumed it commits, and never inside one.
	Ack func() error

	// Release hands this delivery back for immediate redelivery, instead of Ack,
	// on a record the loop will NOT consume: one held from before a doubt
	// ([Runner.Run]'s unwind). Never inside a transaction. Nil: the record waits
	// out its ack window and a strict log stands still behind it.
	Release func() error
}

// Fetcher delivers a domain's records in stream order. Deliberately narrow: a
// consumer's name, ack policy and redelivery window belong to its creator.
type Fetcher interface {
	// Fetch pulls up to maxMessages records or maxBytes of them, whichever
	// binds first, waiting up to wait for the first one.
	//
	// EVERYTHING THE BROKER DELIVERED IS RETURNED, by this call or a later one:
	// a delivered record the loop never sees holds the ack-pending cap and is a
	// hole in a strict log until its ack window ends. Where the broker cannot
	// bound a pull by count and bytes, the in-flight ceiling ([FetchMessages])
	// honours maxMessages.
	//
	// EXCEPT ACROSS A FAILURE: a failing call hands back whatever it held, since
	// the loop gives back every record from before a failed fetch ([Runner.Run]).
	Fetch(ctx context.Context, maxMessages, maxBytes int, wait time.Duration) ([]Message, error)

	// Pending is how many records this consumer has not yet handed to a Fetch:
	// whether waiting would fill a partial batch. A HINT asked on every run, so
	// answer it from what fetches learned, never with a request on the commit
	// path. An error is "not known": it closes the run and never claims the loop
	// level.
	Pending(ctx context.Context) (uint64, error)
}

// CheckpointLog is the log read BY POSITION: its two ends and the record at one
// sequence, to verify the checkpoint's record ([Runner.VerifyCheckpoint]),
// which a consumer cannot answer.
type CheckpointLog interface {
	LogReader

	// Bounds is the log's first surviving sequence and its last, in one
	// answer.
	Bounds(ctx context.Context) (first, last uint64, err error)
}

// ErrStopped reports an applier that halted rather than fell behind: a behind
// node catches up, a stopped one needs a different build.
var ErrStopped = errors.New("statelog: the applier stopped")

// Estate is the replicated database as the applier needs it, RESOLVED PER CALL
// rather than held: an adoption replaces the file under a running node, so a
// handle captured at boot answers from a database no longer at that name. It
// answers [store.ErrNoEstate] in the window where there is none.
type Estate interface {
	// Read runs fn in a read transaction on the current estate.
	Read(ctx context.Context, fn func(*sql.Tx) error) error

	// Tx runs fn in a write transaction on the current estate.
	Tx(ctx context.Context, fn func(*sql.Tx) error) error

	// Writer pins one connection for the life of an apply loop, released
	// before the estate can be replaced.
	Writer(ctx context.Context) (*store.Writer, error)

	// Caps is the CURRENT estate's probed capabilities (a captured value
	// describes a replaced file). [ApplyOptions.MaxVariables] is filled from it
	// once per batch.
	Caps() store.Capabilities
}

// RunnerDeps is everything an applier needs that it does not own.
type RunnerDeps struct {
	Domain  Domain
	Applier Applier
	Fetch   Fetcher

	// Spec is the stream this runner applies: the domain's own ([Domain.Stream])
	// with the byte ceiling this node's Tier A sized, held to the declaration in
	// all else ([StreamSpec.Instantiates]): checkpoint row, generation, anchors
	// and stream identity are keyed by name.
	Spec StreamSpec

	// Verifier authenticates every record before this domain decodes one.
	// REQUIRED: the broker's admission vouches for a connection, never a record.
	// See [recordseal].
	Verifier *recordseal.Verifier

	// Log is the same stream read by position, to establish that it still holds
	// the record consumed at the checkpoint ([Runner.VerifyCheckpoint]).
	// REQUIRED: without it a restored log's new history is applied on top of
	// rows it is not.
	Log CheckpointLog

	// Node is this node's OWN estate, where the applier keeps the verdict that
	// the log diverged from its rows ([ErrLogDiverged]). REQUIRED, for
	// [NodeEstate]'s reason.
	Node NodeEstate

	// DB is the REPLICATED estate: this domain's rows, operation ledger,
	// deferred records, anchors and checkpoint, one file because contract 2 puts
	// them in one transaction. Resolved per call ([Estate]).
	DB Estate

	// Checkpoint is THIS DOMAIN's committed checkpoint as its row holds it
	// (stream, generation, sequence), and CheckpointStoredAt the broker's
	// instant for the record at it, zero where the row names none. Only a
	// reanchor moves it under a running process ([Runner.Reanchored]).
	//
	// THE WHOLE CHECKPOINT, never its generation alone: until the loop loads the
	// row, the heartbeat, a peer's truncation fence and the verification read
	// this as the node's position, and a zero sequence names no record. The
	// empty stream is this domain's own; any other is refused.
	Checkpoint         Position
	CheckpointStoredAt time.Time

	// StreamCreatedAt is this stream's creation instant ([StreamCreatedAt]) AS A
	// READING OF THE STREAM REPORTS IT NOW, to detect a recreated stream (which
	// restarts its sequences): the loop STOPS when it differs from the instant
	// the checkpoint was committed under. Never take it from the checkpoint row,
	// which would compare the value with itself. Zero skips the comparison; the
	// engine never passes it. A reanchor re-keys it ([Runner.Reanchored]).
	StreamCreatedAt time.Time

	// Epoch is where the per-epoch configuration the domain declared it reads
	// comes from, asked once per BATCH ([ApplyOptions.Epoch]), the transaction,
	// as with [ApplyOptions.Now]. Nil hands every batch none.
	//
	// A SOURCE, NOT A VALUE: a value captured at boot applies one record under
	// different epochs on nodes that restarted at different times. It must not
	// block (it runs on the apply loop) and must be safe to call while the
	// caller changes it.
	Epoch func() map[string]any

	Metrics *metrics.Recorder

	// Logger is where this writes. Nil is the package's component logger, never
	// silence ([loggerOr]).
	Logger *slog.Logger

	// NodeID is this node's own id, and Evicted answers whether the fleet has
	// evicted a node: both pick the remedy for a record from a generation these
	// rows never entered ([passedGeneration.err]). Either unset leaves the
	// peer's sentence, which names both remedies.
	NodeID  string
	Evicted func(ctx context.Context, node string) (bool, error)

	// OnDrained is told when this loop first reaches the end of its log after it
	// starts ([Runner.Drained] turns true), not again until it runs again. Nil
	// tells nobody. A push because a node's snapshot gate waits on it
	// ([SkipUnhydrated]).
	//
	// CALLED ON THE APPLY LOOP'S GOROUTINE, outside the runner's lock and after
	// [Runner.Drained] reads true. It must not block: the loop applies nothing
	// until it returns.
	OnDrained func()

	// OnLevel is told each time this loop comes LEVEL with its log (an empty
	// fetch while it holds nothing) having been otherwise since last told. Nil
	// tells nobody. A separate latch from OnDrained because seat claims wait on
	// being level NOW, the strict half of [Health.Established], which recurs
	// after every burst and outage.
	//
	// CALLED ON THE APPLY LOOP'S GOROUTINE, outside the runner's lock, after the
	// drain latch is set and the applied records committed. It must not block.
	OnLevel func()

	// Now is the clock, injectable so a test can drive the time budget.
	Now func() time.Time

	// Witness is told about a record this runner would not authenticate.
	// Optional; see [Witness].
	Witness Witness
}

// Runner is one domain's apply loop: one goroutine beside one checkpoint
// transaction ([checkpoints]), on one pinned connection, its tables' one
// writer. An applier's methods run on one goroutine at a time, so it needs no
// lock.
//
// The store MAY RE-RUN a transaction body, so every effect follows the commit,
// in this order: the publisher's cursor, the waiters at or below it, the
// domain's non-row consequences, then the acks.
//
// Two positions, [Runner.Committed] and [Runner.Reached]; at a doubt it gives
// back what it holds past the checkpoint ([Runner.doubtLocked]).
type Runner struct {
	domain   Domain
	applier  Applier
	fetch    Fetcher
	verifier *recordseal.Verifier
	log      CheckpointLog
	node     NodeEstate
	db       Estate
	tables   tables
	spec     StreamSpec
	metrics  *metrics.Recorder
	logger   *slog.Logger
	now      func() time.Time
	opts     ApplyOptions
	epoch    func() map[string]any
	nodeID   string
	evicted  func(ctx context.Context, node string) (bool, error)
	// onDrained is [RunnerDeps.OnDrained].
	onDrained func()
	// onLevel is [RunnerDeps.OnLevel].
	onLevel func()

	// witnessTo and witnessedKeys are the audit half of a refusal; see
	// [Witness].
	witnessTo     Witness
	witnessedKeys witnessed

	waiters waiters

	mu sync.Mutex

	// created is the creation instant of the stream the positions are keyed to
	// ([StreamCreatedAt] at construction, or what a reanchor confirmed). UNDER
	// mu: a reanchor re-keys it while the heartbeat compares readings to it.
	created time.Time

	// cursor is the committed checkpoint, and ITS GENERATION IS THE RUNNER'S:
	// every record decoded is placed at it, every anchor read with it. One
	// field, so a reanchor cannot move one without the other.
	cursor Position
	// reached is the REACHED POSITION ([Runner.Reached]): never below cursor,
	// moved past it only by [Runner.reach], and SET wherever cursor is set (a
	// load, a reanchor, a join), each of which replaces the rows it claimed
	// about. UNDER mu.
	reached Position
	// changed is the position of the last record a committed transaction of
	// this loop CHANGED A ROW with (every record but a barrier, [writesNoRow]),
	// and the checkpoint wherever cursor is set, for reached's reason
	// ([Runner.Changed]). UNDER mu.
	changed Position
	// checkpointAt is the broker's instant for the record at the cursor's
	// sequence, zero where unknown ([cursorRow.storedAt]). UNDER mu AND MOVED
	// WITH cursor: a sequence paired with another record's instant is a false
	// divergence ([Runner.VerifyCheckpoint]).
	checkpointAt time.Time
	// rows counts the times the rows under the checkpoint may have been replaced
	// or re-keyed (every load, reanchor, join), so a verification can tell its
	// finding is about a copy this runner no longer holds
	// ([Runner.VerifyCheckpoint]).
	rows uint64
	// verify is a verification of the checkpoint's record the loop owes before
	// applying past it, set by [Runner.doubtLocked] and cleared only by the loop
	// ([Runner.verifyBeforeApplying]).
	verify bool
	// doubts counts those moments: every record held from before one is given
	// back, not applied, reached or deduplicated ([Runner.Run]'s unwind). A
	// verification settles only the doubt it was asked under. UNDER mu.
	doubts uint64
	// void is what the reanchor that placed the checkpoint made void, read by
	// [Runner.loadCursor]: a record of an ABANDONED generation, or one a restored
	// reanchor OVERTOOK, is consumed into no row ([ReanchorPlan.From],
	// [ReanchorPlan.StaleAfter]).
	void      voidRange
	deferred  Deferral
	hasDefer  bool
	stopped   error
	appliedAt time.Time

	// fault is the transient error the loop is retrying, and faultSince when the
	// run of failures began; nil between faults ([Runner.Fault]).
	fault      error
	faultSince time.Time
	// faultReported is whether this run has been written as
	// `statelog_apply_faulted`, once per run ([Runner.faulted]). Cleared with
	// fault.
	faultReported bool
	// faultSides is every part of the loop that has failed in this run and not
	// succeeded since ([faultSide]); the run ends when the last does
	// ([Runner.recoveredOn]). Cleared with fault.
	faultSides faultSide

	// drained records that this loop has reached the end of its log at least
	// once since it started ([Health.Drained]). Only the loop can witness it: a
	// timer-sampled lag can miss every instant a busy log is empty
	// ([Runner.nextRun]). RESET WHEN THE LOOP RESTARTS, because an adoption
	// installs a peer's rows under a new history.
	drained bool

	// level is whether the last fetch found the loop level with its log and it
	// has since fetched no records, met no failed fetch and not restarted: what
	// makes the next empty fetch news ([RunnerDeps.OnLevel]).
	level bool

	// foreign is the stream this applier's positions do NOT belong to, once the
	// boot's comparison or a live reading ([Runner.ObserveStream]) finds the
	// broker serving another under this domain's name. NOT CLEARED BY A RE-RUN
	// (a verdict about the positions): a reanchor ([Runner.Reanchored]) or
	// adoption ([Runner.Rejoined]) moves them, and a re-run re-derives it from
	// the row ([Runner.loadCursor]). See [Runner.StreamIdentity].
	foreign *foreignStream

	// passed is the generation the FLEET is on for this domain, once a peer
	// re-anchored the log past these rows: found by the heartbeat
	// ([Runner.ObserveFleetGeneration]) or by a record from a generation this
	// applier never entered ([Runner.decode]). Never set for a domain claiming
	// no identity, whose nodes re-anchor one at a time. STICKY LIKE foreign.
	passed *passedGeneration

	// ahead is the verdict of the last reading that found the checkpoint PAST
	// the log's end ([Runner.ObserveEnd]), else nil; kept because fence 0 runs on
	// every append and answers only from what this node knows.
	//
	// NOT STICKY: an end below the checkpoint can be one member's stale view. A
	// later reading clears it once its end reaches THIS verdict's checkpoint, and
	// [Runner.loadCursor] drops it for a checkpoint left. Clearing is safe: past
	// that point the record at the checkpoint separates the histories
	// ([Runner.diverged]).
	ahead *aheadOfLog

	// diverged is the verdict that the log's record at this applier's checkpoint
	// is not the one it consumed ([ErrLogDiverged]); nil until
	// [Runner.VerifyCheckpoint] finds it. STICKY, unlike ahead: a lagging member
	// holds no record, never a different one. Nothing applies past the
	// checkpoint while it holds; a reanchor, an adoption or a re-run over another
	// checkpoint ([Runner.loadCursor]) ends it.
	//
	// DURABLE in the node estate ([NodeEstate]), since the log may lose the
	// record it was found by ([Runner.recallDiverged]); divergedStored says
	// whether it is recorded.
	diverged       *divergedLog
	divergedStored bool

	// truncated is the verdict that the log lost records a PEER's rows hold
	// ([ErrLogTruncated]), nil while no reading of the positions register finds
	// one ([Runner.ObserveTruncation]). NOT part of [Runner.StreamIdentity]: only
	// this node's writes refuse ([Runner.Truncated]). RECOMPUTED ON EVERY
	// READING, being a fact about other nodes' rows.
	truncated *Truncation

	// drain is this loop's measured records per second, an exponentially
	// weighted mean over batches (one small batch at a burst's tail is not the
	// rate). A bounded stale read, a refusal's `retry_after_seconds` and the
	// apply-lag alarm divide a backlog by it to report a TIME.
	drain float64

	// commits is the same measurement over TRANSACTIONS: the FSYNC rate, which
	// under `synchronous = FULL` spends a device's write budget. It moves apart
	// from drain because [Runner.nextRun] fills a run toward the transaction
	// budget, so a doubled commits/s at a healthy rows/s is a disk doing twice
	// the work for the same progress.
	commits float64
}

// DrainSmoothing is how much of a new batch's rate enters the measurement. A
// quarter: four batches of a new rate move the estimate about 68 %, while one
// anomalous batch moves it by a quarter of its own error.
const DrainSmoothing = 0.25

// NewRunner builds a domain's apply loop, refusing a dependency set that
// cannot produce a correct apply rather than discovering it mid-stream.
func NewRunner(d RunnerDeps) (*Runner, error) {
	switch {
	case d.Domain == nil:
		return nil, fmt.Errorf("statelog: applier has no domain")
	case d.Applier == nil:
		return nil, fmt.Errorf("statelog: applier has no state machine")
	case d.Fetch == nil:
		return nil, fmt.Errorf("statelog: applier has no fetcher")
	case d.Log == nil:
		return nil, fmt.Errorf("statelog: applier has no reader of its log by " +
			"position, so it cannot establish that the record at its checkpoint " +
			"is the one it consumed there — and without that a restored log " +
			"written past its rows is applied on top of them")
	case d.Node == nil:
		return nil, fmt.Errorf("statelog: applier has no estate of this node's " +
			"own to remember a log that diverged from its rows in, and would " +
			"forget it at the first restart after the log lost the record it " +
			"was found by")
	case d.Node.Estate() != store.EstateNode:
		// The replicated estate answers Estate like the node's, so the type
		// does not stop it; a verdict written there is lost on the adoption
		// that replaces the file, when it is needed.
		return nil, fmt.Errorf("statelog: applier was handed the %q estate as "+
			"this node's own; a log's divergence verdict is kept in the node's "+
			"file (store.OpenNode), which no adoption replaces", d.Node.Estate())
	case d.DB == nil:
		return nil, fmt.Errorf("statelog: applier has no database")
	case d.Verifier == nil:
		// Refused, not defaulted to accepting: the boot fails naming the keyring.
		return nil, fmt.Errorf("%w: %s's applier has no verifier, so it would apply "+
			"any record that reached the broker — Tier A secrets.keys is required "+
			"wherever a state-log domain runs", recordseal.ErrUnsigned, d.Domain.Name())
	}
	spec := d.Spec
	if err := spec.Instantiates(d.Domain); err != nil {
		return nil, err
	}
	checkpoint := d.Checkpoint
	switch checkpoint.Stream {
	case "":
		checkpoint.Stream = spec.Name
	case spec.Name:
	default:
		return nil, fmt.Errorf("%w: %s's applier was handed a checkpoint on %s",
			ErrWrongStream, d.Domain.Name(), checkpoint.Stream)
	}
	t, err := newTables(d.Domain, spec)
	if err != nil {
		return nil, err
	}
	if deriver, ok := d.Applier.(Deriver); ok {
		if t.derivation = deriver.DerivationVersion(); t.derivation < 1 {
			return nil, fmt.Errorf("statelog: %s's applier derives at version %d — "+
				"a Deriver's rules start at 1, because 0 is what a checkpoint "+
				"written by no known rules holds", d.Domain.Name(), t.derivation)
		}
	}
	logger := loggerOr(d.Logger)
	now := d.Now
	if now == nil {
		now = time.Now
	}
	return &Runner{
		domain:    d.Domain,
		applier:   d.Applier,
		fetch:     d.Fetch,
		verifier:  d.Verifier,
		log:       d.Log,
		node:      d.Node,
		db:        d.DB,
		tables:    t,
		spec:      spec,
		metrics:   d.Metrics,
		logger:    logger,
		now:       now,
		created:   d.StreamCreatedAt,
		nodeID:    d.NodeID,
		evicted:   d.Evicted,
		onDrained: d.OnDrained,
		onLevel:   d.OnLevel,

		witnessTo: d.Witness,
		opts: ApplyOptions{
			ArbitratedKinds: spec.ArbitratedKinds,
		},
		epoch: d.Epoch,
		// THE CHECKPOINT AND THE RECORD IT NAMES, together, for the reason
		// [Runner.checkpointAt] gives.
		cursor:       checkpoint,
		reached:      checkpoint,
		changed:      checkpoint,
		checkpointAt: d.CheckpointStoredAt,
	}, nil
}

// batchOptions is what every record of one transaction is applied under. The
// epoch is read here per batch, so an activation reaches the next batch
// ([RunnerDeps.Epoch]).
func (r *Runner) batchOptions(now time.Time) ApplyOptions {
	opts := r.opts
	opts.Now = now
	opts.MaxVariables = r.db.Caps().MaxVariables
	if r.epoch != nil {
		opts.Epoch = r.epoch()
	}
	return opts
}

// Committed is this node's COMMITTED CHECKPOINT: what its row holds, so where a
// restart resumes, what the heartbeat publishes ([Runner.Stance]) and the write
// fence reads. A reader waits for [Runner.Reached].
func (r *Runner) Committed() Position {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cursor
}

// Reached is the position this node's rows are CURRENT THROUGH: every record at
// or below it is committed, or is a barrier this loop has decoded and verified
// that writes no row ([writesNoRow]). Never below [Runner.Committed].
//
// IT IS WHAT A READER WAITS FOR, NEVER WHAT IS DURABLE. A barrier writes no row,
// so a stalled fsync is no reason to answer `behind`; but a barrier reached and
// not committed is unacknowledged, and a restart reaches it again.
func (r *Runner) Reached() Position {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.reached
}

// doubtLocked records a moment after which the log past the checkpoint may be
// ANOTHER HISTORY: a run starting, a fetch failing, an end read below what the
// loop holds. Called under mu. It:
//
//   - owes a verification ([Runner.verifyBeforeApplying]): nothing past the
//     checkpoint applies until the log holds the record the checkpoint names;
//   - sends reached back to the checkpoint, since an uncommitted barrier may be
//     gone from the log;
//   - counts the doubt ([Runner.doubts]), so the loop gives back everything
//     held from before it and an older verification does not settle it.
func (r *Runner) doubtLocked() {
	r.verify = true
	r.doubts++
	r.reached = r.cursor
}

// doubted is how many doubts this runner has recorded ([Runner.doubts]).
func (r *Runner) doubted() uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.doubts
}

// settledLocked takes a verification that found the checkpoint's record, asked
// when [Runner.doubts] stood at doubts. It clears the verification owed unless
// a doubt was recorded since. Called under mu.
func (r *Runner) settledLocked(doubts uint64) {
	if r.doubts == doubts {
		r.verify = false
	}
}

// mayCommitBeside reports whether the checkpoint transaction beside the loop
// may commit batch now ([checkpoints]): not while a divergence holds, nor while
// a verification is owed for anything in it past the checkpoint. Redeliveries
// at or below the checkpoint need none, so their acks are not held up.
func (r *Runner) mayCommitBeside(batch []Record) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.diverged != nil {
		return false
	}
	return !r.verify || highest(batch, Position{}).Packed() <= r.cursor.Packed()
}

// Change is one reading of where this node's rows last changed ([Runner.Changed]).
type Change struct {
	// At is the position of the last record this node committed that changed a
	// row: every record but a barrier.
	At Position

	// loads is [Runner.rows] when read: a load reprocesses what an earlier
	// build retained, changing rows without moving a position.
	loads uint64
}

// String is the reading as one token, which two readings of the same rows share
// and two of different rows do not.
func (c Change) String() string { return fmt.Sprintf("%s#%d", c.At, c.loads) }

// Changed is where this node's rows LAST CHANGED, beside how many times they
// may have been replaced or reloaded. Two equal readings are of the same rows.
//
// NOT [Runner.Reached] OR [Runner.Committed]: both move on every barrier, which
// every linearizable read writes, so a cache keyed on them (the knowledge
// answers) is retired by its own reads. This moves on everything but a barrier
// ([writesNoRow]), a retained record included.
func (r *Runner) Changed() Change {
	r.mu.Lock()
	defer r.mu.Unlock()
	return Change{At: r.changed, loads: r.rows}
}

// reach moves the reached position over what run carries that writes no row
// ([reachedOver]) and releases every waiter at or below it, on the loop before
// any transaction ([waiters.release]).
//
// NOT WHILE ANYTHING PAST THE CHECKPOINT MAY BE ANOTHER HISTORY (a verification
// owed, a divergence), AND NOT WHILE THIS NODE RETAINS A RECORD: a barrier
// whose scope meets a retained one is retained too, a row, so only commits move
// it.
func (r *Runner) reach(run []Record) {
	r.mu.Lock()
	if r.verify || r.diverged != nil || r.hasDefer {
		r.mu.Unlock()
		return
	}
	at := reachedOver(r.reached, run, r.spec.ArbitratedKinds)
	moved := at != r.reached
	r.reached = at
	r.mu.Unlock()
	if moved {
		r.waiters.release(at)
	}
}

// reachedThrough reports whether every record run carries is at or below the
// reached position — committed already, or a barrier this loop has reached —
// so its whole transaction would be the checkpoint, and it may go to the one
// beside the loop ([checkpoints]).
func (r *Runner) reachedThrough(run []Record) bool {
	return highest(run, Position{}).Packed() <= r.Reached().Packed()
}

// Stance is one reading of where a runner stands: its committed checkpoint, the
// broker's instant for the record at it (zero where unknown), and WHICH COPY of
// its rows it was read from.
//
// THE COMMITTED CHECKPOINT, NEVER [Runner.Reached]: a stance is published
// ([coord.DomainPosition]) and counted by the trim, so an uncommitted barrier
// would let a restart resume below a purged floor. ONE READ under one lock
// ([Runner.checkpointAt], [Runner.ObserveTruncation]). The unexported reached
// is for [Runner.ObserveEnd] alone, read BEFORE the end; nothing publishes it.
type Stance struct {
	At       Position
	StoredAt time.Time
	rows     uint64
	reached  Position
}

// Stance reads where this runner stands — see [Stance].
func (r *Runner) Stance() Stance {
	r.mu.Lock()
	defer r.mu.Unlock()
	return Stance{At: r.cursor, StoredAt: r.checkpointAt, rows: r.rows, reached: r.reached}
}

// StreamCreatedAt is the creation instant of the stream this runner's positions
// are keyed to, which live readings ([Runner.ObserveStream]) and the
// checkpoint's load compare against. Not a boot-time copy: a reanchor re-keys it.
func (r *Runner) StreamCreatedAt() time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.created
}

// KeyedTo is the creation instant of the stream this runner's CHECKPOINT is
// keyed to. It is [Runner.StreamCreatedAt] except while the recreation verdict
// holds, when the runner may carry either instant; the verdict holds the rows'
// own, which a published position must name ([coord.DomainPosition]).
func (r *Runner) KeyedTo() time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.foreign != nil {
		return r.foreign.keyed
	}
	return r.created
}

// Reanchored re-keys this runner to the stream an operator's reanchor adopted:
// the checkpoint it committed, the creation instant the operator confirmed,
// and storedAt, the broker's instant for the log's record there (zero where
// none), which a diverged log is judged against from here on.
//
// It clears what a re-run may not: the recreation verdict, a passed generation
// the new one reached, an end read against the old checkpoint and a divergence
// about its record no longer hold. CALLED WITH THE LOOP STOPPED: a running loop
// could commit at the old generation after the reanchor's transaction. The next
// run resumes from the committed row ([ReanchorCase]).
func (r *Runner) Reanchored(at Position, created, storedAt time.Time) error {
	if at.Stream != r.spec.Name {
		return fmt.Errorf("%w: a reanchor of %s named a checkpoint on %s",
			ErrWrongStream, r.spec.Name, at.Stream)
	}
	if created.IsZero() {
		return fmt.Errorf("statelog: a reanchor of %s named no creation instant, "+
			"and a runner keyed to none detects no later rebuild", r.spec.Name)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if at.Generation <= r.cursor.Generation {
		return fmt.Errorf("statelog: a reanchor of %s named generation %d and this "+
			"runner already stands at %s — a reanchor moves a domain into a "+
			"generation it has not been in", r.spec.Name, at.Generation, r.cursor)
	}
	r.created = created
	r.cursor, r.checkpointAt = at, storedAt
	r.reached, r.changed = at, at
	r.rows++
	// The divergence's node-estate record is about the checkpoint this left;
	// [Runner.recallDiverged] removes it on its next read.
	r.foreign, r.ahead, r.diverged, r.divergedStored = nil, nil, nil, false
	// A peer's truncation was about the generation this left.
	r.truncated = nil
	if r.passed != nil && at.Generation >= r.passed.fleet {
		r.passed = nil
	}
	// Only the stops the reanchor answers; any other is about this build.
	if errors.Is(r.stopped, ErrStreamRecreated) || errors.Is(r.stopped, ErrLogDiverged) {
		r.stopped = nil
	}
	return nil
}

// Stopped is the error that halted this applier, or nil.
//
// A STOP IS PERMANENT and only [ErrStopped] is one: a gate this build cannot
// read, a hole that will not close, a recreated stream, an undecodable
// envelope. Every other failure is retried ([Runner.Run]) and reported through
// [Runner.Fault].
func (r *Runner) Stopped() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.stopped
}

// ObserveStream takes one LIVE reading of the creation instant of this
// applier's stream ([StreamCreatedAt]), and answers true the one time it
// establishes that the stream is not the one this applier started against.
//
// TOLD RATHER THAN ASKING: a stream remade under a running node counts from 1
// at the same generation, so nothing the loop reads sees it. The heartbeat
// ([PositionHeartbeat]) and the fence on an expectation of zero hand the
// instant here, so reads and writes consult one verdict. A zero instant
// ([StreamUnknown]), or a runner with no identity declared, establishes nothing.
func (r *Runner) ObserveStream(live time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.foreign != nil || IdentityOf(r.created, live, true) != StreamRecreated {
		return false
	}
	r.foreign = &foreignStream{keyed: r.created, live: live}
	return true
}

// RecreationStale reports whether this applier holds a recreation verdict that
// a live reading contradicts: its rows are keyed to exactly the stream the
// broker serves.
//
// An adoption can key the rows to the live stream ([Runner.Rejoined]). When its
// re-key failed, [Runner.loadCursor] judges a stream it never read as another:
// the loop stops as recreated over the fleet's history, and a reanchor refuses.
// So when this answers true, the engine asks for a join to re-key against a
// reading taken now.
func (r *Runner) RecreationStale(live time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.foreign != nil && IdentityOf(r.foreign.keyed, live, true) == StreamSame
}

// ObserveEnd takes one LIVE reading of the log's last sequence, paired with
// where this applier stood as the caller read it BEFORE asking the broker
// ([Runner.Stance]; a checkpoint read after the end passes it on a healthy busy
// log): established is true the one time a reading finds the checkpoint past
// the end, reached the one time a later one finds the end at or past it.
//
// An end below the reached position owes a verification ([Runner.doubtLocked])
// even short of the checkpoint. Once a restored log is written past the
// checkpoint its end clears the verdict, so the record at the checkpoint decides
// ([Runner.verifyBeforeApplying]): another is [ErrLogDiverged], which does not
// clear.
func (r *Runner) ObserveEnd(s Stance, last uint64) (established, reached bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	at := s.At
	if at.Generation < r.cursor.Generation {
		// A reading from before a reanchor says nothing about the adopted
		// stream, and a verdict on it would refuse writes until the log reached
		// a number the node never stands at again.
		return false, false
	}
	if pastEnd(s.reached.Seq, last) {
		// Reached is never below the checkpoint, so this covers a checkpoint
		// past the end too.
		r.doubtLocked()
	}
	if pastEnd(at.Seq, last) {
		established = r.ahead == nil
		r.ahead = &aheadOfLog{at: at, last: last}
		return established, false
	}
	if r.ahead == nil {
		return false, false
	}
	if pastEnd(r.ahead.at.Seq, last) {
		// Still past it: moves only the end. A NEW value, never a write through
		// the shared pointer ([Runner.StreamIdentity] reads it unlocked).
		r.ahead = &aheadOfLog{at: r.ahead.at, last: last}
		return false, false
	}
	r.ahead = nil
	return false, true
}

// StreamIdentity is nil while every position this applier holds is a sequence
// on the stream the broker serves, in the history the log continues. Reads and
// writes both consult it. Otherwise it wraps, in this order:
//
//   - [ErrStreamRecreated]: the stream was rebuilt; until a reanchor or adoption.
//   - [ErrGenerationPassed]: a peer re-anchored past this generation; until an
//     adoption.
//   - [ErrLogDiverged]: the log's record at the checkpoint is another; until a
//     reanchor or adoption.
//   - [ErrAheadOfLog]: the log's end was last read below the checkpoint; while
//     it stays below.
func (r *Runner) StreamIdentity() error {
	r.mu.Lock()
	foreign, passed, diverged, ahead := r.foreign, r.passed, r.diverged, r.ahead
	r.mu.Unlock()
	switch {
	case foreign != nil:
		return foreign.err(r.domain.Name(), r.spec.Name)
	case passed != nil:
		return passed.err(r.domain.Name(), r.spec.Name)
	case diverged != nil:
		// Before the end: a divergence is definitive, a checkpoint past the end
		// may be one member's stale answer, and both can hold at once.
		return diverged.err(r.spec.Name)
	case ahead != nil:
		return ahead.err(r.spec.Name)
	}
	return nil
}

// ObserveTruncation takes one reading of the positions register (the peer whose
// rows hold records the log lost, or nil when no peer's do) and reports the
// transition: established the one time a reading finds such a peer, cleared
// the one time a later one finds none. THE ENGINE'S READING, because the facts
// are other nodes'; an unreadable register is never handed here and leaves the
// verdict as it was.
//
// s is where this runner stood when the reading was taken ([Runner.Stance]). A
// reading about rows it no longer holds changes nothing: it would refuse writes
// of a node whose rows now head the log, or clear a fence still needed.
func (r *Runner) ObserveTruncation(s Stance, t *Truncation) (established, cleared bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	// Every reload, reanchor and join moves the count, and every generation
	// change is one of those.
	if s.rows != r.rows {
		return false, false
	}
	had := r.truncated != nil
	if t == nil {
		r.truncated = nil
		return false, had
	}
	// A NEW VALUE, never a write through the shared pointer.
	found := *t
	r.truncated = &found
	return !had, false
}

// Truncated is nil while no peer's rows are known to hold records the log lost,
// and the refusal naming the peer once one is — see [ErrLogTruncated]. The
// publisher refuses every write on it but an eviction or a readmission.
func (r *Runner) Truncated() error {
	r.mu.Lock()
	t := r.truncated
	r.mu.Unlock()
	if t == nil {
		return nil
	}
	return t.err(r.spec.Name)
}

// Diverged reports whether this applier holds the verdict that the log diverged
// from its rows ([ErrLogDiverged]), whatever else [Runner.StreamIdentity]
// would answer first.
func (r *Runner) Diverged() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.diverged != nil
}

// checkpointCheck is what one comparison of the checkpoint's record with the
// log concluded.
type checkpointCheck int

const (
	// checkpointSettled: the log holds the record the checkpoint names, or
	// nothing can be compared: the checkpoint names no record, the record is
	// below the log's first sequence, or a compacted log superseded it.
	checkpointSettled checkpointCheck = iota

	// checkpointUnreached: the log ends below the checkpoint ([Runner.ObserveEnd]
	// refuses on it).
	checkpointUnreached

	// checkpointDiverged: the log holds ANOTHER record at the checkpoint's
	// sequence ([ErrLogDiverged]).
	checkpointDiverged
)

// VerifyCheckpoint compares the record the log holds at this applier's
// checkpoint with the one the checkpoint names, and answers true the one time
// that establishes that the log diverged from these rows ([ErrLogDiverged]).
//
// On one stream a sequence names one record for ever (a lagging member holds
// none, never another), so a different record means a broker restored from an
// older copy and written past these rows. Two broker instants are compared for
// EQUALITY, so no clocks are ordered. Asked by the loop after a doubt
// ([Runner.verifyBeforeApplying]) and by the heartbeat every [PositionHeartbeat]
// (a restore between doubts or on a stopped loop). A verdict recorded before a
// restart is recalled first ([Runner.recallDiverged]), so a log that lost the
// record cannot lift it.
func (r *Runner) VerifyCheckpoint(ctx context.Context) (bool, error) {
	recalled, err := r.recallDiverged(ctx)
	if err != nil {
		return false, err
	}
	_, established, err := r.checkCheckpoint(ctx)
	return recalled || established, err
}

// recallDiverged makes this runner's divergence verdict durable, or adopts one
// this node recorded before a restart, and answers true the one time it adopts
// one.
//
// A HELD VERDICT NOT YET RECORDED is recorded. With none held, a recorded one is
// adopted only while it is about the checkpoint this runner stands at (same
// position, same record): those are the rows it was found against. Any other is
// about a file a reanchor or adoption replaced, and is removed: those alone end
// a divergence, never a reading of the log.
func (r *Runner) recallDiverged(ctx context.Context) (bool, error) {
	r.mu.Lock()
	at, consumed, held, stored, rows := r.cursor, r.checkpointAt, r.diverged,
		r.divergedStored, r.rows
	r.mu.Unlock()
	if held != nil {
		if stored {
			return false, nil
		}
		if err := writeDiverged(ctx, r.node, r.spec.Name, *held, r.now()); err != nil {
			return false, err
		}
		r.mu.Lock()
		if r.diverged == held {
			r.divergedStored = true
		}
		r.mu.Unlock()
		return false, nil
	}
	recorded, found, err := readDiverged(ctx, r.node, r.spec.Name)
	if err != nil || !found {
		return false, err
	}
	// A verdict is only reached against a checkpoint naming a record, so a row
	// naming another, or none, is another file's.
	if recorded.at != at || consumed.IsZero() || !sameRecord(recorded.consumed, consumed) {
		return false, clearDiverged(ctx, r.node, r.spec.Name, recorded)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.rows != rows || r.diverged != nil {
		// Re-keyed under the read: the next call judges it afresh.
		return false, nil
	}
	r.diverged, r.divergedStored = &recorded, true
	return true, nil
}

// checkCheckpoint is [Runner.VerifyCheckpoint], reporting which of the three
// outcomes it reached.
func (r *Runner) checkCheckpoint(ctx context.Context) (checkpointCheck, bool, error) {
	// The pair under one lock, read together or not at all.
	r.mu.Lock()
	at, consumed, rows := r.cursor, r.checkpointAt, r.rows
	r.mu.Unlock()
	if at.Seq > 0 && consumed.IsZero() {
		// A checkpoint naming no record (a reanchor placed it where the log held
		// none) is compared with nothing; past the end it is unreached.
		_, last, err := r.log.Bounds(ctx)
		if err != nil {
			return 0, false, fmt.Errorf("statelog: read %s's ends to verify the "+
				"checkpoint %s: %w", r.spec.Name, at, err)
		}
		if pastEnd(at.Seq, last) {
			return checkpointUnreached, false, nil
		}
		return checkpointSettled, false, nil
	}
	check, held, err := compareCheckpoint(ctx, r.log, at, consumed)
	if err != nil || check != checkpointDiverged {
		return check, false, err
	}
	r.mu.Lock()
	if r.rows != rows || at.Generation < r.cursor.Generation {
		r.mu.Unlock()
		// Rows replaced between the reads: a finding about a copy no longer
		// held; the next verification reads the current pair.
		return checkpointSettled, false, nil
	}
	established := r.diverged == nil
	if established {
		r.diverged = &divergedLog{at: at, consumed: consumed, held: held}
		r.divergedStored = false
	}
	r.mu.Unlock()
	// Recorded at once in the node estate, so a restart after the log lost the
	// record cannot lift it. A failure leaves it held in memory (the loop refuses
	// on it) and recorded on the next verification ([Runner.recallDiverged]).
	_, err = r.recallDiverged(ctx)
	return checkpointDiverged, established, err
}

// CheckpointDiverged reports whether log holds, at checkpoint at, ANOTHER
// record than the one consumed names, and the instant of the one it holds, for
// a reader that is not the checkpoint's runner (a reanchor deciding its case).
// FALSE WHENEVER NOTHING CAN BE COMPARED ([checkpointSettled],
// [checkpointUnreached]).
func CheckpointDiverged(ctx context.Context, log CheckpointLog, at Position,
	consumed time.Time) (time.Time, bool, error) {

	check, held, err := compareCheckpoint(ctx, log, at, consumed)
	return held, check == checkpointDiverged, err
}

// compareCheckpoint is the one comparison of a checkpoint's record with the
// log's record at the same sequence — see [Runner.VerifyCheckpoint].
func compareCheckpoint(ctx context.Context, log CheckpointLog, at Position,
	consumed time.Time) (checkpointCheck, time.Time, error) {

	if at.Seq == 0 || consumed.IsZero() {
		return checkpointSettled, time.Time{}, nil
	}
	first, last, err := log.Bounds(ctx)
	if err != nil {
		return 0, time.Time{}, fmt.Errorf("statelog: read %s's ends to verify the "+
			"record at the checkpoint %s: %w", at.Stream, at, err)
	}
	switch {
	case pastEnd(at.Seq, last):
		return checkpointUnreached, time.Time{}, nil
	case at.Seq < first:
		return checkpointSettled, time.Time{}, nil
	}
	_, _, held, ok, err := log.At(ctx, at.Seq)
	if err != nil {
		return 0, time.Time{}, fmt.Errorf("statelog: read %s's record at the "+
			"checkpoint %s to verify it: %w", at.Stream, at, err)
	}
	if !ok || sameRecord(held, consumed) {
		return checkpointSettled, held, nil
	}
	return checkpointDiverged, held, nil
}

// verifyBeforeApplying settles a verification the loop owes before it applies
// run, which holds a record past the checkpoint; nil means the run may be
// applied.
//
// OWED, NOT PERIODIC: a broker round trip per batch on the hottest path would
// almost always prove nothing, so the loop asks once after each doubt
// ([Runner.doubtLocked]). Ends that do not reach the checkpoint while the run
// holds a record past it are members disagreeing: retried, never applied on the
// guess. A divergence, found here or by the heartbeat, STOPS the loop. THE
// LOOP'S ALONE, with no checkpoint transaction in flight and nothing held from
// before the doubt ([Runner.Run]); it settles only its own doubt
// ([Runner.doubts]).
func (r *Runner) verifyBeforeApplying(ctx context.Context, run []Record) error {
	r.mu.Lock()
	owed, cursor, diverged, doubts := r.verify, r.cursor, r.diverged, r.doubts
	r.mu.Unlock()
	if diverged == nil && (!owed || topRecord(run).Position.Packed() <= cursor.Packed()) {
		return nil
	}
	check := checkpointDiverged
	if diverged == nil {
		var err error
		if check, _, err = r.checkCheckpoint(ctx); err != nil {
			return err
		}
	}
	switch check {
	case checkpointSettled:
		r.mu.Lock()
		r.settledLocked(doubts)
		r.mu.Unlock()
		return nil
	case checkpointUnreached:
		return fmt.Errorf("statelog: %s holds records past this node's checkpoint "+
			"%s and its ends say it does not reach it — the log is read again "+
			"before anything past the checkpoint is applied", r.spec.Name, cursor)
	}
	r.mu.Lock()
	verdict := r.diverged
	r.mu.Unlock()
	if verdict == nil {
		// Re-keyed under the read; the next attempt verifies the current rows.
		return fmt.Errorf("statelog: %s's checkpoint moved while its record was "+
			"verified — verified again before anything past it is applied",
			r.spec.Name)
	}
	return fmt.Errorf("%w: %w", ErrStopped, verdict.err(r.spec.Name))
}

// ObserveFleetGeneration takes the generation the FLEET is on for this domain
// (the highest any node or the trim has published) and answers true the one
// time it establishes that a peer re-anchored the log past this applier's rows.
//
// A generation moves only by a reanchor or adopting a snapshot of a node that
// ran one, so a peer at a later one holds the rows the log continues from,
// which nothing on the log brings these rows to. NEVER FOR A DOMAIN THAT CLAIMS
// NO IDENTITY: its nodes re-anchor one at a time ([PermitReanchor]), so a peer
// ahead is a recovery in progress.
func (r *Runner) ObserveFleetGeneration(fleet uint32) bool {
	if !r.domain.ClaimsIdentity() {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.passedLocked(passedGeneration{fleet: fleet})
}

// passedLocked records that the log is on generation p.fleet — by p.writer's
// record, or by the register where that is empty — answering true the one time
// that establishes the verdict. The caller holds mu.
func (r *Runner) passedLocked(p passedGeneration) bool {
	if p.fleet <= r.cursor.Generation {
		return false
	}
	if r.passed != nil {
		// A later generation replaces what is held; the same one gains the
		// writer the register could not name ([Runner.RecheckPassed]).
		switch {
		case p.fleet > r.passed.fleet:
		case p.fleet == r.passed.fleet && r.passed.writer == "" && p.writer != "":
			p.holder = r.passed.holder
		default:
			return false
		}
		// A NEW VALUE, never a write through the shared pointer.
		p.at = r.passed.at
		r.passed = &p
		return false
	}
	p.at = r.cursor
	r.passed = &p
	return true
}

// holderOf is who holds the generation a record by writer opened: this node's
// own failed reanchor, a node the fleet has evicted, or a peer.
//
// A PEER WHEREVER IT CANNOT TELL (no writer, no way to ask, no answer): that
// sentence names both remedies, adoption and eviction.
func (r *Runner) holderOf(ctx context.Context, writer string) passedHolder {
	switch {
	case writer == "":
		return passedByPeer
	case writer == r.nodeID:
		return passedByOwnAttempt
	case r.evicted == nil:
		return passedByPeer
	}
	evicted, err := r.evicted(ctx, writer)
	if err != nil || !evicted {
		return passedByPeer
	}
	return passedByEvicted
}

// RecheckPassed re-derives who holds the generation a passed verdict names,
// given fleet, the generation the live, un-evicted peers stand at in the
// register now, and reports whether that changed it.
//
// The remedy moves while the verdict holds: an evicted peer leaves no snapshot
// to adopt, and a readmission turns that back. A live peer at the generation
// means adoption; otherwise a record's writer is judged by its eviction, and a
// register-only verdict no un-evicted peer holds lost its holders to eviction.
// This node's own failed attempt stays its own until its reanchor ends it.
func (r *Runner) RecheckPassed(ctx context.Context, fleet uint32) (bool, error) {
	r.mu.Lock()
	held := r.passed
	r.mu.Unlock()
	if held == nil || held.holder == passedByOwnAttempt {
		return false, nil
	}
	holder := passedByEvicted
	switch {
	case fleet >= held.fleet:
		holder = passedByPeer
	case held.writer != "":
		if r.evicted == nil {
			return false, nil
		}
		evicted, err := r.evicted(ctx, held.writer)
		if err != nil {
			return false, fmt.Errorf("statelog: read whether %s, which opened "+
				"generation %d of %s, is evicted: %w", held.writer, held.fleet,
				r.spec.Name, err)
		}
		if !evicted {
			holder = passedByPeer
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.passed != held || held.holder == holder {
		return false, nil
	}
	next := *held
	next.holder = holder
	r.passed = &next
	return true, nil
}

// Rejoined re-derives what this runner knows about its positions from the
// checkpoint a join left in the replicated file: that checkpoint, the creation
// instant its rows are keyed to (zero where the file holds none), and the live
// instant of the stream the join was judged against.
//
// An adoption replaces every row, anchor and checkpoint with a peer's, so
// keeping the recreation or passed-generation verdict would stop the loop
// again. It RE-DERIVES them rather than clearing them: a join that found every
// domain current replaced nothing and changes nothing. Called with the loop
// stopped, for [Runner.Reanchored]'s reason.
func (r *Runner) Rejoined(at Position, keyed, live time.Time) error {
	if at.Stream != r.spec.Name {
		return fmt.Errorf("%w: a join for %s named a checkpoint on %s",
			ErrWrongStream, r.spec.Name, at.Stream)
	}
	if live.IsZero() {
		return fmt.Errorf("statelog: a join for %s named no live creation instant, "+
			"and a runner keyed to none detects no later rebuild", r.spec.Name)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	moved := at != r.cursor
	// A peer's truncation was about the generation these rows stood at
	// ([Runner.Reanchored]); one adopted into the SAME generation stays until
	// the next reading.
	if at.Generation != r.cursor.Generation {
		r.truncated = nil
	}
	r.cursor, r.reached, r.changed = at, at, at
	r.rows++
	if IdentityOf(keyed, live, !keyed.IsZero()) == StreamRecreated {
		// The rows are still another stream's: the verdict holds.
		if r.foreign == nil {
			r.foreign = &foreignStream{keyed: keyed, live: live}
		}
	} else {
		r.created = live
		r.foreign = nil
	}
	if r.passed != nil && at.Generation >= r.passed.fleet {
		r.passed = nil
	}
	// An end reading paired with another checkpoint says nothing about this one
	// ([Runner.loadCursor]'s rule).
	if r.ahead != nil && r.ahead.at != at {
		r.ahead = nil
	}
	// Which record a moved checkpoint names is the row's to say, read when the
	// loop restarts; comparing the new position with the OLD record would report
	// a divergence between two files. A divergence about the old checkpoint goes
	// with it; one at the SAME position stays (clearing it would let a write
	// through) and [Runner.loadCursor] re-derives it.
	if moved {
		r.checkpointAt = time.Time{}
		if r.diverged != nil && r.diverged.at != at {
			r.diverged, r.divergedStored = nil, false
		}
	}
	if (r.foreign == nil && errors.Is(r.stopped, ErrStreamRecreated)) ||
		(r.passed == nil && errors.Is(r.stopped, ErrGenerationPassed)) ||
		(r.diverged == nil && errors.Is(r.stopped, ErrLogDiverged)) {
		r.stopped = nil
	}
	return nil
}

// Fault is the transient failure this applier has been retrying for longer
// than [ApplyRetryBudget], as of now, and false when there is none or it is
// younger than that.
//
// REPORTED LATE, unlike a stop: a fault means the broker or disk did not answer
// just now, not worth moving work for. Past the budget it takes a stop's path:
// reads refuse `stalled` naming it, and the node takes this domain's copy out
// of service. It clears once every failed part of the loop has succeeded
// ([Runner.recoveredOn]).
func (r *Runner) Fault(now time.Time) (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fault == nil || now.Sub(r.faultSince) < ApplyRetryBudget {
		return "", false
	}
	return fmt.Sprintf("%v (retried since %s)", r.fault,
		r.faultSince.UTC().Format(time.RFC3339)), true
}

// Drained reports whether this loop has reached the end of its log at least
// once since it started ([Health.Drained]).
//
// IT NEVER GOES BACK TO FALSE while the loop runs: a snapshot gate asks whether
// the node ever held the whole state; the DISTANCE is [Health.Lag]'s.
func (r *Runner) Drained() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.drained
}

// markDrained records that the broker answered a fetch with nothing while the
// loop held nothing, and tells [RunnerDeps.OnDrained] the first time, AFTER THE
// LATCH IS SET AND THE LOCK RELEASED.
func (r *Runner) markDrained() {
	r.mu.Lock()
	first := !r.drained
	r.drained = true
	r.mu.Unlock()
	if first && r.onDrained != nil {
		r.onDrained()
	}
}

// markLevel is [Runner.markDrained]'s twin for [RunnerDeps.OnLevel], told when
// news: the first such answer since the loop started, fetched records or failed.
// Called after markDrained.
func (r *Runner) markLevel() {
	r.mu.Lock()
	news := !r.level
	r.level = true
	r.mu.Unlock()
	if news && r.onLevel != nil {
		r.onLevel()
	}
}

// unlevel records that the loop fetched records, so the next empty fetch is
// news again.
func (r *Runner) unlevel() {
	r.mu.Lock()
	r.level = false
	r.mu.Unlock()
}

// Deferred is the earliest record this node holds and cannot decode, and false
// when it holds none.
func (r *Runner) Deferred() (Deferral, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.deferred, r.hasDefer
}

// Waiting is how many callers are blocked on this applier.
func (r *Runner) Waiting() int { return r.waiters.len() }

// WaitReached blocks until this node's reached position is at or past p
// ([Runner.Reached]): every row-writing record through p committed, every
// barrier decoded, never a barrier's own commit.
func (r *Runner) WaitReached(ctx context.Context, p Position) error {
	if p.Stream != "" && p.Stream != r.spec.Name {
		return fmt.Errorf("%w: waiting on %s from an applier of %s",
			ErrWrongStream, p.Stream, r.spec.Name)
	}
	return r.waiters.awaitPosition(ctx, p, r.Reached)
}

// WaitApplied blocks until this node holds the derived consequences of
// everything up to p for the objects in s.
//
// THE WAIT IS ON THE REACHED POSITION, never the coverage: a deferred record's
// covered position is fixed at or below the checkpoint, so waiting on it never
// ends. Coverage is checked; the reached position is waited for.
func (r *Runner) WaitApplied(ctx context.Context, s ScopeSet, p Position) error {
	if err := r.WaitReached(ctx, p); err != nil {
		return err
	}
	if _, blocked := r.Deferred(); !blocked {
		return nil
	}
	var hit bool
	var d Deferral
	err := r.db.Read(ctx, func(tx *sql.Tx) error {
		var err error
		d, hit, err = r.tables.deferredIn(ctx, tx, s)
		return err
	})
	if err != nil {
		return err
	}
	if hit {
		return &Unavailable{
			Reason: ReasonDeferred,
			Detail: fmt.Sprintf("this node holds a record at version %d it cannot "+
				"decode, at %s, whose scope covers what this operation is about",
				d.Version, d.Position),
			Position: d.Position,
		}
	}
	return nil
}

// OpsRetention is how long a node keeps its record of which operations it
// applied.
//
// THIRTY DAYS, sized for the slowest retrying client: a seat carrying an op id
// to its next wake, after a weekend for a scheduled one, not the machine's
// sixteen rounds in five seconds ([DefaultResolveBudget]). An op id that
// outlives its row resolves `unknown`, which re-decides work, never a second
// copy: the publisher reads each sweep's cutoff before trusting the ledger's
// silence ([Rows.LostBefore]). About 29 MB steady at the census rate (6 518
// commits a day, ~150 bytes a row).
const OpsRetention = 30 * 24 * time.Hour

// PurgeOps deletes this node's operation rows applied before cutoff.
//
// PER NODE: each node's table records the operations THIS applier wrote, so a
// fleet-singleton sweep would leave every other node's rows growing.
func (r *Runner) PurgeOps(ctx context.Context, cutoff time.Time) (int64, error) {
	return r.tables.purgeOps(ctx, r.db, cutoff)
}

// PurgeOpsOfKind deletes this node's operation rows on one subject kind applied
// before cutoff, for a kind whose writers never re-ask late ([OpsRetention]
// would hold a month of rows nobody looks up). A domain declares the horizon at
// registration and ships the `(subject, applied_at)` index this seeks on.
func (r *Runner) PurgeOpsOfKind(ctx context.Context, kind string,
	cutoff time.Time) (int64, error) {

	return r.tables.purgeOpsOfKind(ctx, r.db, kind, cutoff)
}

// Op answers where an operation was applied on this node, as [Rows.Op] does.
func (r *Runner) Op(ctx context.Context, opID string) (OpEntry, bool, error) {
	var entry OpEntry
	var ok bool
	err := r.db.Read(ctx, func(tx *sql.Tx) error {
		var err error
		entry, ok, err = r.tables.op(ctx, tx, opID)
		return err
	})
	return entry, ok, err
}

// Run drives the loop until the context ends or the applier stops.
//
// It may be RUN AGAIN after it returns: an adoption ends the loops, replaces
// the file and restarts the same Runner from the new file's checkpoint, with
// retained records reprocessed. A previous run's stop, fault, recreation and
// passed-generation verdicts are re-evaluated against the rows it loads
// ([Runner.RecreationStale] covers the one case that cannot reach alone).
func (r *Runner) Run(ctx context.Context) error {
	r.mu.Lock()
	r.stopped, r.fault, r.faultSince, r.faultReported = nil, nil, time.Time{}, false
	// A restart is an adoption or re-anchor: a drain of the old history is no
	// claim about the new one.
	r.drained, r.level = false, false
	r.mu.Unlock()

	var w *store.Writer
	// The one checkpoint transaction beside the loop, on the writer below
	// ([checkpoints]).
	beside := &checkpoints{r: r}
	defer func() {
		// It ends first: it writes on the connection closed next, and a commit
		// closed under it is one nobody can say landed.
		beside.stop()
		if w != nil {
			_ = w.Close()
		}
		// A waiter on a stopped applier is told, not released: released it would
		// read as satisfied and go on to read rows that never reached its position.
		r.waiters.abandonAll()
	}()

	// THE LOOP OUTLIVES A FAILURE: an unanswered fetch, refused transaction or
	// applier error says nothing about whether this node can run these records,
	// and a returned loop left the domain dead for the process's life. A failure
	// that is not a STOP is retried in place, on a pause doubling to a ceiling,
	// with the same run re-applied. [Runner.Fault] reports it past the budget.
	var tail []Record
	var buffer reorderBuffer
	var pause time.Duration
	// trusted is the doubt ([Runner.doubts]) everything held was delivered under.
	// Each step that can wait compares it with the runner's count after; a doubt
	// meanwhile sends what is held back through the unwind at the top.
	var trusted uint64
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		// Startup is inside the retry regime: a store momentarily unavailable
		// (an adoption's close-to-reopen) would otherwise leave the domain with
		// no applier, stop or fault, publishing a caught-up position for ever.
		if w == nil {
			pinned, err := r.startup(ctx)
			if err != nil {
				if stopped := r.faulted(ctx, sideApply, err); stopped != nil {
					return stopped
				}
				pause = r.backOff(ctx, pause)
				continue
			}
			w = pinned
			r.recovered(ctx)
			pause = 0
		}
		// What the transaction beside the loop did. A FAILURE returns its records
		// to the loop to be applied again after the pause. A COMMIT ends only THAT
		// transaction's failure: it may predate a failed fetch, so the loop's own
		// failures and back-off stand until a success of its own.
		if taken, committed, failed := beside.outcome(); failed != nil {
			tail = append(taken, tail...)
			if stopped := r.faulted(ctx, sideBeside, failed); stopped != nil {
				return stopped
			}
			pause = r.backOff(ctx, pause)
			continue
		} else if committed && r.recoveredOn(ctx, sideBeside) {
			pause = 0
		}
		// A doubt since the loop last trusted what it holds: everything in hand, the
		// reorder buffer and the transaction beside goes back to the broker ([Runner.unwind]).
		// Kept, it would be applied past a verification that compared only the
		// checkpoint, and deduplicated against another history's records at the
		// same sequences, mixing two histories undetectably.
		if doubts := r.doubted(); doubts != trusted {
			failed := r.unwind(ctx, beside, tail, &buffer)
			tail, trusted = nil, doubts
			if failed != nil {
				if stopped := r.faulted(ctx, sideBeside, failed); stopped != nil {
					return stopped
				}
				pause = r.backOff(ctx, pause)
				continue
			}
		}
		run, level, err := r.nextRun(ctx, tail, &buffer)
		if err != nil {
			// A failed fetch may be a broker restored under this node: what is held
			// goes back ([Runner.doubtLocked]) and what follows the checkpoint is
			// verified first.
			r.mu.Lock()
			r.doubtLocked()
			r.level = false
			r.mu.Unlock()
			tail = run
			if stopped := r.faulted(ctx, sideFetch, err); stopped != nil {
				return stopped
			}
			pause = r.backOff(ctx, pause)
			continue
		}
		// The broker answered: the retry a failed fetch waited for, whatever the
		// run holds (a run of barriers alone goes beside the loop, which no
		// transaction of its own would end).
		if r.recoveredOn(ctx, sideFetch) {
			pause = 0
		}
		if r.doubted() != trusted {
			// A doubt while fetching: given back with the rest, at the top.
			tail = run
			continue
		}
		if len(run) == 0 {
			// Nothing pending or in hand: this node's rows are the WHOLE state, so
			// the drain latch is set and the loop is level ([RunnerDeps.OnLevel]),
			// unless the reorder buffer holds records above a hole.
			//
			// UNLESS THE TRANSACTION BESIDE THE LOOP HOLDS RECORDS: unacknowledged,
			// they may hold the broker back ([FetchMessages]), and a fault it is
			// retrying is cleared by its next commit, above.
			if !beside.holds() {
				if level {
					r.markDrained()
					r.markLevel()
				}
				r.recovered(ctx)
				pause = 0
			}
			continue
		}
		r.unlevel()
		if verifyErr := r.verifyBeforeApplying(ctx, run); verifyErr != nil {
			if errors.Is(verifyErr, ErrStopped) {
				verifyErr = r.stop(ctx, verifyErr)
			}
			if stopped := r.faulted(ctx, sideVerify, verifyErr); stopped != nil {
				return stopped
			}
			tail = run
			pause = r.backOff(ctx, pause)
			continue
		}
		if r.verifiedOn(ctx) {
			pause = 0
		}
		if r.doubted() != trusted {
			// A doubt while verifying settled nothing: the run is given back.
			tail = run
			continue
		}
		// Reached as far as the run carries barriers, now that what follows the
		// checkpoint is verified (the step [Runner.nextRun] skips while one is owed).
		r.reach(run)
		if r.reachedThrough(run) {
			// A run whose whole transaction is the checkpoint commits beside the
			// loop, so its fsync never stands before the decode of the records
			// behind it ([checkpoints]). Waiters were released when reached; it is
			// acknowledged when it commits. It DRAINS THE LOG as a run committed
			// whole does: every record held is committed or writes no row.
			beside.offer(ctx, w, run)
			tail = nil
			if level {
				r.markDrained()
			}
			continue
		}
		// The loop's own transaction waits for the one beside it (one writer at a
		// time) and carries every barrier it had not taken, all below this run.
		taken, failed := beside.settle()
		run = append(taken, run...)
		if failed != nil {
			if stopped := r.faulted(ctx, sideBeside, failed); stopped != nil {
				return stopped
			}
			tail = run
			pause = r.backOff(ctx, pause)
			continue
		}
		if r.doubted() != trusted {
			// A doubt while waiting (a stalled fsync's seconds): given back.
			tail = run
			continue
		}
		consumed, err := r.applyRun(ctx, w, run)
		if err != nil {
			if stopped := r.faulted(ctx, sideApply, err); stopped != nil {
				return stopped
			}
			// The same run again: dropped, the loop would fetch above a hole for
			// the whole ack window.
			tail = run
			pause = r.backOff(ctx, pause)
			continue
		}
		r.recovered(ctx)
		pause = 0
		tail = run[len(consumed):]
		if level && len(tail) == 0 {
			// The other drain moment, and on a busy log the only one: latched only
			// on an empty fetch, a node on a log gaining a record more often than
			// [FetchWait] would never drain or serve as a snapshot's donor.
			r.markDrained()
		}
	}
}

// startup is everything the loop needs before it may consume a record: a
// pinned connection, the checkpoint, and whatever an earlier build retained.
// IT CLEANS UP AFTER ITSELF, being RETRIED: a connection leaked per failed
// attempt would exhaust the small pool and make a transient failure permanent.
func (r *Runner) startup(ctx context.Context) (*store.Writer, error) {
	// Pinned for the loop's life: readers wait on what this writer commits and
	// under load would hold every pooled connection while it queues.
	w, err := r.db.Writer(ctx)
	if err != nil {
		return nil, fmt.Errorf("statelog: pin the applier's connection: %w", err)
	}
	if err := r.loadCursor(ctx); err != nil {
		_ = w.Close()
		if errors.Is(err, ErrStopped) {
			return nil, r.stop(ctx, err)
		}
		return nil, err
	}
	// Only the load's doubt is settled by the comparison below
	// ([Runner.settledLocked]): the heartbeat may read the end below the
	// checkpoint after the comparison read its record.
	doubts := r.doubted()
	// The checkpoint's record is verified at once: a log written EXACTLY to this
	// checkpoint delivers nothing past it, leaving a diverged node serving until
	// the heartbeat looked. A log not reaching it leaves the verification owed.
	// A recorded divergence is recalled first, since the log may have lost the
	// record it was found by.
	if _, err := r.recallDiverged(ctx); err != nil {
		_ = w.Close()
		return nil, err
	}
	check, _, checkErr := r.checkCheckpoint(ctx)
	if checkErr != nil {
		_ = w.Close()
		return nil, checkErr
	}
	r.mu.Lock()
	verdict := r.diverged
	if verdict == nil && check == checkpointSettled {
		r.settledLocked(doubts)
	}
	r.mu.Unlock()
	switch {
	case verdict != nil:
		_ = w.Close()
		return nil, r.stop(ctx, fmt.Errorf("%w: %w", ErrStopped, verdict.err(r.spec.Name)))
	case check == checkpointDiverged:
		_ = w.Close()
		return nil, fmt.Errorf("statelog: %s's checkpoint moved while its record "+
			"was verified at start — verified again", r.spec.Name)
	}
	// Rows are brought to this build's derivation rules before anything is
	// applied, retained records included: a reprocess maintains derived columns
	// incrementally, and an increment on a column the old rules wrote mixes both.
	// AFTER the verification, so a diverged node stops with its rows as found.
	if err := r.rederive(ctx, w); err != nil {
		_ = w.Close()
		return nil, err
	}
	// What an earlier build retained, this one may now read: a build changes
	// only at a boot, so the promise is kept here, before consuming anything above.
	if err := r.reprocess(ctx, w); err != nil {
		_ = w.Close()
		return nil, err
	}
	r.logger.InfoContext(ctx, "statelog_applier_started",
		"domain", r.domain.Name(), "stream", r.spec.Name,
		"protocol", string(r.spec.Replay), "position", r.Committed().String())
	return w, nil
}

// faultSide is one part of the apply loop that can fail, as a bit of the run
// of failures it joins ([Runner.faultSides]).
//
// A run remembers WHICH parts failed because only that part's success ends it:
// a transaction beside the loop commits barriers handed over before a fetch
// failed, while that fetch is still unretried. Ended by any success, the run
// would log a false `statelog_apply_recovered`, reset the fault's clock and
// back-off, and start [Runner.Fault] late.
type faultSide uint8

const (
	// sideApply is the loop's startup or a transaction of its own; a
	// transaction of its own that commits ends the whole run, since it
	// carries whatever the others failed on.
	sideApply faultSide = 1 << iota
	// sideFetch is a fetch the broker did not answer, ended by the next one
	// it does.
	sideFetch
	// sideVerify is the verification of the checkpoint's record, ended by
	// the next that settles ([Runner.verifiedOn]).
	sideVerify
	// sideBeside is the checkpoint transaction beside the loop, ended by its
	// next commit ([checkpoints]).
	sideBeside
)

// faulted classifies a failure of side: a STOP is returned to end the loop,
// anything else is recorded as the fault being retried and swallowed.
//
// A run of failures is written as three lines, never as a level:
// `statelog_apply_retrying` when it starts, `statelog_apply_faulted` ONCE on
// the first retry past [ApplyRetryBudget], and `statelog_apply_recovered` with
// its duration. An ERROR per retry would count one incident as hundreds ([Tracker]
// logs transitions, not levels). The level lives in [Runner.Fault] and
// `crewlet.statelog.apply.retries`.
func (r *Runner) faulted(ctx context.Context, side faultSide, err error) error {
	if errors.Is(err, ErrStopped) || ctx.Err() != nil {
		return err
	}
	now := r.now()
	r.mu.Lock()
	first := r.fault == nil
	if first {
		r.faultSince = now
	}
	r.fault = err
	r.faultSides |= side
	since := r.faultSince
	report := !r.faultReported && now.Sub(since) >= ApplyRetryBudget
	if report {
		r.faultReported = true
	}
	r.mu.Unlock()
	if first {
		r.logger.WarnContext(ctx, "statelog_apply_retrying",
			"domain", r.domain.Name(), "stream", r.spec.Name,
			"position", r.Committed().String(), "error", err.Error(),
			"reported_after", ApplyRetryBudget)
	}
	if report {
		r.logger.ErrorContext(ctx, "statelog_apply_faulted",
			"domain", r.domain.Name(), "stream", r.spec.Name,
			"position", r.Committed().String(), "error", err.Error(),
			"since", since, "detail", "this node's rows have stopped moving; "+
				"its reads refuse and it takes its copy of this domain out of "+
				"service, so this domain's operations go to the other data nodes, "+
				"until a retry succeeds")
	}
	r.count(metrics.StatelogApplyRetries)
	return nil
}

// recovered clears the fault a retry just outlived and closes its run: the next
// failure starts a new one. It is the success that ends a run whatever failed
// in it (startup, a transaction of the loop's own, an empty fetch while it holds
// nothing); [Runner.recoveredOn] ends only its own part.
func (r *Runner) recovered(ctx context.Context) {
	r.mu.Lock()
	had, since := r.fault, r.faultSince
	r.fault, r.faultSince, r.faultReported, r.faultSides = nil, time.Time{}, false, 0
	r.mu.Unlock()
	if had != nil {
		r.logger.InfoContext(ctx, "statelog_apply_recovered",
			"domain", r.domain.Name(), "stream", r.spec.Name,
			"after", r.now().Sub(since).Round(time.Millisecond), "error", had.Error())
	}
}

// recoveredOn is a success of side alone: it ends side's part of the run, and
// the run once no part is left failing ([faultSide]). It answers whether no run
// is left standing, when the back-off restarts from the beat.
func (r *Runner) recoveredOn(ctx context.Context, side faultSide) bool {
	r.mu.Lock()
	if r.fault == nil {
		r.mu.Unlock()
		return true
	}
	r.faultSides &^= side
	open := r.faultSides != 0
	r.mu.Unlock()
	if open {
		return false
	}
	r.recovered(ctx)
	return true
}

// verifiedOn is the success that ends a verification's failure ([sideVerify]):
// a verification that SETTLED, and only one: a run with nothing past the
// checkpoint passes without one ([Runner.verifyBeforeApplying]) and, taken for
// a success, would log a false recovery and restart the fault's clock. False
// while the verification is owed.
func (r *Runner) verifiedOn(ctx context.Context) bool {
	r.mu.Lock()
	owed := r.verify
	r.mu.Unlock()
	return !owed && r.recoveredOn(ctx, sideVerify)
}

// backOff waits before the next attempt and returns the pause the attempt
// after that will take: the beat on the first retry, doubling to the ceiling.
func (r *Runner) backOff(ctx context.Context, pause time.Duration) time.Duration {
	if pause <= 0 {
		pause = ApplyRetryBeat
	}
	t := time.NewTimer(pause)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
	return min(pause*2, ApplyRetryCeiling)
}

// loadCursor reads this domain's checkpoint at boot.
func (r *Runner) loadCursor(ctx context.Context) error {
	return r.db.Read(ctx, func(tx *sql.Tx) error {
		row, found, err := r.tables.readCursor(ctx, tx)
		if err != nil {
			return err
		}
		at, created := row.at, row.created
		d, hasDefer, err := r.tables.oldestDeferred(ctx, tx)
		if err != nil {
			return err
		}
		r.mu.Lock()
		defer r.mu.Unlock()
		// The abandoned generations travel with the checkpoint: this loop may be
		// resuming part-way through them, after a restart or over a peer's snapshot.
		r.void = row.void
		if found {
			// Together, for [Runner.checkpointAt]'s reason.
			r.cursor, r.checkpointAt = at, row.storedAt
		}
		r.changed = r.cursor
		r.rows++
		// A starting run owes a verification of that record before applying past
		// it: a boot is when a node meets a broker restored from an older copy.
		// Reached goes back too ([Runner.doubtLocked]): an uncommitted barrier of
		// a previous run comes back, and an adoption may have replaced its rows.
		r.doubtLocked()
		// A verdict about a checkpoint this runner left is dropped: over an adopted
		// snapshot it would refuse writes until the log reached a number this node
		// has left. The boot's own reading is paired with this row and survives.
		if r.ahead != nil && r.ahead.at != r.cursor {
			r.ahead = nil
		}
		r.deferred, r.hasDefer = d, hasDefer
		// A RECREATED STREAM IS DETECTED HERE AND IS A STOP: every stored sequence
		// names a dead number space, and a resumed consumer waits for ever
		// reporting nothing pending, until an operator reanchors. RECORDED as well
		// as returned, because the publisher asks [Runner.StreamIdentity], never
		// the loop's error. Compared through [IdentityOf] at the row's microseconds
		// (the broker reports nanoseconds), against the live instant this runner
		// knows best: a recreation verdict's, else the one it was built with.
		//
		// RE-DERIVED against the row, never remembered: an adoption may have
		// replaced it without the re-key ([Runner.Rejoined]), and a row keyed to
		// the live stream clears it.
		live := r.created
		if r.foreign != nil {
			live = r.foreign.live
		}
		switch {
		case live.IsZero():
		case !found:
			// No row to judge by: a verdict that holds stands, none is reached.
		case IdentityOf(created, live, true) == StreamRecreated:
			// A NEW VALUE, keyed to the ROW'S instant, which [Runner.KeyedTo]
			// publishes and [Runner.RecreationStale] judges by.
			r.foreign = &foreignStream{keyed: created, live: live}
		default:
			r.created, r.foreign = live, nil
		}
		if r.foreign != nil {
			return fmt.Errorf("%w: %w", ErrStopped,
				r.foreign.err(r.domain.Name(), r.spec.Name))
		}
		// So does a generation the fleet moved past: a re-run would apply the new
		// generation's records into these rows ([Runner.decode] stops a running loop).
		if r.passed != nil && found && at.Generation >= r.passed.fleet {
			r.passed = nil
		}
		if r.passed != nil {
			return fmt.Errorf("%w: %w", ErrStopped,
				r.passed.err(r.domain.Name(), r.spec.Name))
		}
		// A divergence is judged by the record the row names: same checkpoint and
		// record are the same rows, anything else is a file an adoption replaced.
		if r.diverged != nil && found &&
			(r.diverged.at != at || !sameRecord(row.storedAt, r.diverged.consumed)) {
			r.diverged, r.divergedStored = nil, false
		}
		if r.diverged != nil {
			return fmt.Errorf("%w: %w", ErrStopped, r.diverged.err(r.spec.Name))
		}
		return nil
	})
}

// ReprocessPage bounds how many retained records one read takes, so a node
// that sat out a long upgrade walks its backlog in pages.
const ReprocessPage = 256

// reprocess applies every retained record this build can now decode, in
// position order, releasing each in the transaction that applied it.
//
// THE LIVE LOOP'S RULE, REPLAYED: a readable record stays retained while an
// EARLIER retained record covers its scope (applying it first produces state no
// other node holds); walked oldest first, the below-position probe is the live
// check. A record above this build's version stays, with everything it covers.
//
// Neither the checkpoint nor the anchor moves: the log consumed the record when
// it was retained, and the anchor is a MAX. One transaction per record.
func (r *Runner) reprocess(ctx context.Context, w *store.Writer) error {
	var after int64
	var applied, gated, kept int
	for {
		var page []retained
		if err := r.db.Read(ctx, func(tx *sql.Tx) error {
			var err error
			page, err = r.tables.deferredAfter(ctx, tx, after, ReprocessPage)
			return err
		}); err != nil {
			return err
		}
		if len(page) == 0 {
			break
		}
		for _, row := range page {
			after = row.position
			if !(Envelope{V: row.version}).ReadableBy(r.domain.RecordVersion()) {
				kept++
				continue
			}
			// Re-verified, never trusted for having been filed: the table holds
			// the bytes as published.
			body, verdict := r.verifier.Open(row.payload)
			at := Position{
				Stream:     r.spec.Name,
				Generation: uint32(row.position / GenerationStride),
				Seq:        uint64(row.position % GenerationStride),
			}
			switch verdict {
			case recordseal.KeyUnknown:
				// Said once per key per process: after a restart this is the only
				// place the node meets the record again.
				r.witness(ctx, verdict, row.payload, at)
				kept++
				continue
			case recordseal.Tampered:
				r.witness(ctx, verdict, row.payload, at)
				return fmt.Errorf("%w: %s's retained record at packed position %d is "+
					"not signed by this fleet — it was filed under a key this node "+
					"did not hold and now fails under one it does",
					ErrStopped, r.domain.Name(), row.position)
			}
			env, err := r.domain.Envelope(body)
			if err != nil {
				return fmt.Errorf("statelog: %s could not read the envelope of a "+
					"retained record at packed position %d, which every build must "+
					"be able to: %w", r.domain.Name(), row.position, err)
			}
			rec := Record{
				Envelope: env,
				Position: at,
				Payload:  body,
				framed:   row.payload,
				// Through [AppliedInstant] on this path too: both paths hand an
				// applier one value.
				StoredAt: AppliedInstant(row.storedAt),
			}
			landed, result, err := r.reprocessOne(ctx, w, rec)
			switch {
			case err != nil:
				return err
			case !landed:
				kept++
				continue
			case result.gated:
				// A gate the retaining build could not ask dropped it: counted as
				// the live loop's `gated` now its transaction has committed.
				r.gatedRecord(ctx, rec, result.reason)
				gated++
			default:
				applied++
				// Nothing retains a trimmed record, but a removal is counted
				// where its transaction committed, whichever path that was.
				r.countSwept(result.swept)
			}
			r.applier.Committed(ctx)
		}
	}
	if applied == 0 && gated == 0 && kept == 0 {
		return nil
	}
	if err := r.refreshDeferred(ctx); err != nil {
		return err
	}
	r.logger.InfoContext(ctx, "statelog_retained_reprocessed",
		"domain", r.domain.Name(), "applied", applied, "gated", gated,
		"kept", kept, "build_reads", r.domain.RecordVersion())
	if r.metrics != nil {
		for result, n := range map[string]int{"reprocessed": applied, "gated": gated} {
			if n > 0 {
				r.metrics.Add(metrics.StatelogApplyRecords, uint64(n),
					metrics.Attrs{"domain": r.domain.Name(), "result": result})
			}
		}
	}
	return nil
}

// reprocessOne applies one retained record unless an earlier retained record
// still covers it, reporting whether it landed (released from the retained
// table) and whether a gate dropped it, and which.
func (r *Runner) reprocessOne(ctx context.Context, w *store.Writer, rec Record) (bool, applyResult, error) {
	var landed bool
	var result applyResult
	err := w.Tx(ctx, func(tx *sql.Tx) error {
		landed, result = false, applyResult{}
		if _, covered, err := r.tables.deferredBelow(ctx, tx, rec.Scope, &rec.Position); err != nil {
			return err
		} else if covered {
			return nil
		}
		a, err := r.applyOne(ctx, tx, rec, r.batchOptions(r.now()))
		if err != nil {
			return err
		}
		// The one case the checkpoint's write never sees, since a reprocess does
		// not move it.
		if !a.gated {
			if err := r.tables.raiseApplied(ctx, tx, rec.V); err != nil {
				return err
			}
		}
		if err := r.tables.release(ctx, tx, rec.Position); err != nil {
			return err
		}
		landed, result = true, a
		return nil
	})
	if err != nil {
		return false, applyResult{}, fmt.Errorf("statelog: reprocess the retained record at %s: %w",
			rec.Position, err)
	}
	return landed, result, nil
}

// nextRun fills a run toward the transaction budget, starting from what the
// previous transaction did not consume: per fetch, the commit rate would follow
// the fetch size, on a barrier-heavy stream one fsync per two dozen records. It
// REACHES WHAT IT ADMITS ([Runner.reach]) unless a verification is owed.
//
// LEVEL says the run, committed whole, leaves this node holding everything the
// log held when it closed (the broker had nothing more, the reorder buffer
// nothing above a hole); it latches [Runner.Drained]. An error comes back with
// the run so far, so the loop holds every record until it consumes or gives it
// back.
func (r *Runner) nextRun(ctx context.Context, tail []Record, buffer *reorderBuffer) (run []Record, level bool, err error) {
	run = tail
	// A run its budget ended leaves barriers in hand and their readers waiting.
	r.reach(run)
	for {
		if len(run) > 0 {
			pending, perr := r.fetch.Pending(ctx)
			if perr != nil || pending == 0 {
				// Pending is a hint: its failure closes the run, not the applier;
				// the next Fetch is the call that reports.
				return run, perr == nil && len(buffer.held) == 0, nil
			}
			if r.budgetWouldBind(run) {
				return run, false, nil
			}
		}
		wait := FetchWait
		if len(run) == 0 && r.waiters.len() > 0 {
			// Somebody is waiting: on a quiet log this wait is the whole latency a
			// linearizable read pays.
			wait = ApplyLinger
		}
		if len(run) > 0 {
			// A partial run lingers but yields the instant somebody waits on a
			// record it holds, or an idle company pays the linger per record.
			wait = ApplyLinger
			if target, waiting := r.waiters.minimum(); waiting && r.have(run, target) {
				r.count(metrics.StatelogLingerYields)
				return run, false, nil
			}
		}
		batch, err := r.fetch.Fetch(ctx, FetchMessages, FetchBytes, wait)
		if err != nil {
			// The run so far comes back for the loop to give back ([Runner.Run]):
			// dropped, it would be a hole for an ack window.
			if ctx.Err() != nil {
				return run, false, ctx.Err()
			}
			return run, false, fmt.Errorf("statelog: fetch from %s: %w", r.spec.Name, err)
		}
		records, err := r.decode(ctx, batch)
		if err != nil {
			return run, false, err
		}
		// Contiguous from the REACHED position, not the checkpoint: records
		// between are in hand, so a redelivery is passed through to be acked.
		ready, err := buffer.admit(records, r.Reached(), r.spec.Replay, run)
		if err != nil {
			return run, false, r.stop(ctx, err)
		}
		run = append(run, ready...)
		r.reach(run)
		if len(batch) == 0 {
			if len(run) == 0 {
				// Level, unless the buffer holds records above a hole.
				return nil, len(buffer.held) == 0, nil
			}
			// Linger expired with records in hand: commit. An empty pull while
			// records are pending is the broker WITHHOLDING them (the run fills
			// the in-flight ceiling, freed only by acking after this commit).
			return run, false, nil
		}
	}
}

// have reports whether run carries the position somebody waits for.
func (r *Runner) have(run []Record, target Position) bool {
	for _, rec := range run {
		if rec.Position.Packed() >= target.Packed() {
			return true
		}
	}
	return false
}

// budgetWouldBind reports whether the run already holds enough that another
// fetch could not be committed with it.
//
// A ROW ESTIMATE, deliberately: the exact count is known only inside the
// transaction. One row per record is a floor, so it never stops a fetch that
// would have fitted; the transaction's own budget ends a batch.
func (r *Runner) budgetWouldBind(run []Record) bool {
	return len(run) >= ApplyTxRowBudget
}

// decode turns broker messages into records, with the envelope the domain can
// always read.
func (r *Runner) decode(ctx context.Context, batch []Message) ([]Record, error) {
	out := make([]Record, 0, len(batch))
	// The checkpoint's generation, read once per batch (nothing moves it while
	// the loop runs), and whether the fleet is known to have left it, which the
	// heartbeat can establish between batches.
	r.mu.Lock()
	gen, passed := r.cursor.Generation, r.passed != nil
	r.mu.Unlock()
	for _, m := range batch {
		// Verified before the domain sees it: a domain's decoder is where
		// attacker-controlled bytes get parsed.
		body, verdict := r.verifier.Open(m.Payload)
		if verdict != recordseal.Verified {
			// Outside the transaction, which the store may run twice.
			r.witness(ctx, verdict, m.Payload,
				Position{Stream: r.spec.Name, Generation: gen, Seq: m.Seq})
		}
		if verdict == recordseal.Tampered {
			// PERMANENT, and it stops the loop: a MAC failing under a key this
			// fleet holds was written by something that is not this fleet.
			r.countWith(metrics.StatelogRecordsTampered, metrics.Attrs{
				"domain": r.domain.Name(),
			})
			return nil, r.stop(ctx, fmt.Errorf("%w: %s's record at sequence %d is not "+
				"signed by this fleet — its frame fails under the keys this node "+
				"holds (%s), so something that can reach the broker wrote it and "+
				"this node will not apply it or anything after it",
				ErrStopped, r.domain.Name(), m.Seq, words.And(r.verifier.KeyIDs())))
		}
		env, err := r.domain.Envelope(body)
		if err != nil {
			// A STOP: every build reads the envelope, so the record is not this
			// domain's; retaining it would index nothing.
			return nil, r.stop(ctx, fmt.Errorf("%w: %s could not read the envelope "+
				"at sequence %d, which every build must be able to: %w",
				ErrStopped, r.domain.Name(), m.Seq, err))
		}
		if env.Scope.Empty() {
			return nil, r.stop(ctx, fmt.Errorf("%w: the record at sequence %d "+
				"declares no scope — an empty scope claims it makes nothing "+
				"stale, which is the one claim a record no build may be able to "+
				"read cannot make", ErrStopped, m.Seq))
		}
		// A record from a generation this applier never entered is a STOP on a
		// domain that claims identity: a peer re-anchored and this node did not,
		// and applying on would mix two histories. The reanchor's own record is
		// the new generation's first, so this sees the move at once. ANY RECORD
		// stops once the move is known (the heartbeat may learn it first, and a
		// record need not carry its writer's generation); a re-run stops in
		// [Runner.loadCursor]. The node adopts the peer's snapshot; the verdict
		// refuses its reads and writes until then.
		if passed || (env.Gen > gen && r.domain.ClaimsIdentity()) {
			// Who holds it decides the remedy the stop names, so ask before
			// writing the line an operator is sure to read.
			observed := passedGeneration{fleet: env.Gen, writer: env.Writer}
			if env.Gen > gen && r.domain.ClaimsIdentity() {
				observed.holder = r.holderOf(ctx, env.Writer)
			}
			r.mu.Lock()
			r.passedLocked(observed)
			verdict := *r.passed
			r.mu.Unlock()
			return nil, r.stop(ctx, fmt.Errorf("%w: the record at sequence %d, "+
				"written in generation %d, is not applied: %w", ErrStopped, m.Seq,
				env.Gen, verdict.err(r.domain.Name(), r.spec.Name)))
		}
		out = append(out, Record{
			Envelope: env,
			Position: Position{Stream: r.spec.Name, Generation: gen, Seq: m.Seq},
			// The body to the applier, the frame to the table: a retained record is
			// re-verified by the build that can finally read it.
			Payload: body,
			framed:  m.Payload,
			// The instant as the store keeps it ([AppliedInstant]); the broker's
			// own is a value no record applied later carries.
			StoredAt: AppliedInstant(m.StoredAt),
			verdict:  verdict,
			ack:      m.Ack,
			release:  m.Release,
		})
	}
	return out, nil
}

// applyRun commits as much of run as the budget allows, then does the four
// things that must happen after the commit, in order.
func (r *Runner) applyRun(ctx context.Context, w *store.Writer, run []Record) ([]Record, error) {
	var consumed []Record
	var committedAt Position
	var committedRecord time.Time
	// The last record this transaction changed a row with ([Runner.Changed]).
	var changed Position
	var tally results
	var rows int
	var boundBy string
	var notes []applyNote
	started := r.now()

	// Aborts are counted here, the only place that can: the store may not import
	// metrics but re-runs the body, so invocations minus one is
	// `crewlet.statelog.apply.tx.aborts`. It READS ZERO BY CONSTRUCTION
	// (internal/store takes the write lock at BEGIN, so no commit can abort an
	// apply); a count is a TRANSIENT FAILURE inside a re-run body, which this
	// design's throughput figures assume never happens.
	attempts := 0
	err := w.Tx(ctx, func(tx *sql.Tx) error {
		attempts++
		// Reset on every attempt: accumulated counters would report rolled-back
		// work, and a note kept from an abandoned attempt would announce a
		// retention that may not have committed.
		consumed, committedAt, tally, rows, boundBy = consumed[:0], Position{}, results{}, 0, ""
		notes = notes[:0]
		committedRecord, changed = time.Time{}, Position{}
		txStart := r.now()

		hasDeferred, err := r.anyDeferred(ctx, tx)
		if err != nil {
			return err
		}
		// The high-water guard, the checkpoint's rule WITHIN the transaction: a
		// duplicate at or below what this transaction applied is skipped too.
		highWater := r.Committed()
		opts := r.batchOptions(txStart)

		for i := range run {
			rec := run[i]
			if rec.Position.Packed() <= highWater.Packed() {
				// At or below the checkpoint: consumed, acked, never applied.
				consumed = append(consumed, rec)
				tally.skipped++
				continue
			}
			if rec.Kind == BarrierKind {
				tally.barrier(rec.StoredAt, txStart)
			}
			// The framework's own write first, from the always-decodable
			// envelope, whatever the record then does.
			if arbitrates(r.spec.ArbitratedKinds, rec.Subject.Kind) {
				if err := r.tables.advanceAnchor(ctx, tx, r.tables.subjectOf(rec.Subject), rec.Position); err != nil {
					return err
				}
			}

			// Every record changes a row ([Runner.Changed]) but a barrier applied
			// here; one retained below is a row of the deferral index.
			wrote := true
			switch {
			case rec.verdict == recordseal.KeyUnknown:
				// Same disposition as a record from a newer build: "cannot read
				// this yet". Retained under its own scope, so later records on the
				// same objects wait behind it. A gate is still a STOP, as in the
				// version arm.
				if r.domain.InstallsGate(rec.Envelope) {
					return fmt.Errorf("%w: %s at %s installs an apply gate and is "+
						"signed under a key this node does not hold (it holds %s) "+
						"— a gate this node cannot authenticate would license "+
						"every record above it, so the applier halts and this "+
						"node takes its copy of the estate out of service, so its "+
						"seats read it from a data node that can",
						ErrStopped, rec.Kind, rec.Position, words.And(r.verifier.KeyIDs()))
				}
				if err := r.tables.retain(ctx, tx, rec, r.spec.Replay == ReplayCompacted, opts.MaxVariables); err != nil {
					return err
				}
				hasDeferred = true
				tally.retained++
				notes = append(notes, applyNote{rec: rec, kind: noteUnverifiable})

			case !rec.ReadableBy(r.domain.RecordVersion()):
				if r.domain.InstallsGate(rec.Envelope) {
					// A GATE IS A STOP, the one place the retain rule inverts: a
					// deferred gate licenses every record above it, and an eviction
					// deferred by the evicted node leaves it passing every fence.
					return fmt.Errorf("%w: %s at %s installs an apply gate at "+
						"record version %d and this build reads %d — a gate this "+
						"node cannot read would license every record above it, so "+
						"the applier halts and this node takes its copy of this domain "+
						"out of service, so this domain's operations go to a data node "+
						"that can",
						ErrStopped, rec.Kind, rec.Position, rec.V, r.domain.RecordVersion())
				}
				if err := r.tables.retain(ctx, tx, rec, r.spec.Replay == ReplayCompacted, opts.MaxVariables); err != nil {
					return err
				}
				hasDeferred = true
				tally.retained++
				notes = append(notes, applyNote{rec: rec, kind: noteDeferred})

			default:
				blocked := false
				if hasDeferred {
					// Retained too: applied on rows a deferred record never wrote,
					// it makes state no other node holds, undetectably.
					if _, hit, err := r.tables.deferredIn(ctx, tx, rec.Scope); err != nil {
						return err
					} else if hit {
						if err := r.tables.retain(ctx, tx, rec, r.spec.Replay == ReplayCompacted, opts.MaxVariables); err != nil {
							return err
						}
						blocked = true
						tally.retained++
					}
				}
				if !blocked {
					a, err := r.applyOne(ctx, tx, rec, opts)
					if err != nil {
						return err
					}
					rows += a.rows
					tally.swept += a.swept
					if a.gated {
						tally.gated++
						notes = append(notes, applyNote{rec: rec, kind: noteGated, gate: a.reason})
					} else {
						tally.applied++
						tally.version = max(tally.version, rec.V)
					}
					wrote = !writesNoRow(rec, r.spec.ArbitratedKinds)
				}
			}

			if wrote {
				changed = rec.Position
			}
			consumed = append(consumed, rec)
			highWater = rec.Position
			switch {
			case rows >= ApplyTxRowBudget:
				boundBy = "rows"
			case r.now().Sub(txStart) >= ApplyTxTimeBudget:
				boundBy = "time"
			}
			if boundBy != "" {
				break
			}
		}
		if len(consumed) == 0 {
			return nil
		}
		// THE HIGHEST POSITION THIS TRANSACTION HOLDS, never the run's tail:
		// [reorderBuffer.admit] passes a late redelivery through to be acked, so a
		// run can close as [11, 12, 11]. Checkpointing the tail writes 11 beside
		// 12's rows, a cursor that UNDERSTATES its database: a read waiting for 12
		// is refused `behind` and the next boot re-applies 12. The floor is the
		// committed cursor, so redeliveries are acked without moving it.
		before := r.Committed()
		committedAt = highest(consumed, before)
		// The record it names is the one consumed at that position, or for
		// redeliveries the one already named ([Runner.checkpointAt]). NEVER A
		// REDELIVERY'S OWN: a restored consumer redelivers the log's record from
		// whichever history, and naming the checkpoint by it would adopt it.
		committedRecord = r.checkpointRecord()
		if top := topRecord(consumed); top.Position == committedAt && committedAt != before {
			committedRecord = top.StoredAt
		}
		return r.tables.setCursor(ctx, tx, committedAt, r.StreamCreatedAt(),
			committedRecord, r.now(), tally.version)
	})
	if err != nil {
		if errors.Is(err, ErrStopped) {
			return nil, r.stop(ctx, err)
		}
		return nil, err
	}
	if len(consumed) == 0 {
		return consumed, nil
	}
	// AFTER THE OUTER TRANSACTION RETURNS, IN THIS ORDER ([Runner]).
	//
	// What the committed attempt saw, once ([applyNote]), before the checkpoint
	// moves, so a caller seeing this node past a gated record sees its count.
	r.announce(ctx, notes)

	// What this node publishes and resumes from is the checkpoint row's value.
	at := committedAt
	if tally.retained > 0 {
		// Readiness and coverage turn on what this node cannot read, so refresh
		// it now rather than at the next boot.
		if err := r.refreshDeferred(ctx); err != nil {
			return nil, err
		}
	}
	// Every waiter at or below the reached position the commit leaves, never
	// below it: barriers already reached past it stay reached.
	r.waiters.release(r.advance(at, committedRecord, changed))
	if tally.retained > 0 {
		// What the batch retained, which no Apply call saw ([RetentionHook]),
		// AFTER the deferral index refresh so a listener reads this batch's.
		if hook, ok := r.applier.(RetentionHook); ok {
			hook.Retained(ctx, tally.retained)
		}
	}
	r.applier.Committed(ctx)
	r.ack(ctx, consumed)

	r.measureDrain(started, len(consumed))
	r.countAborts(attempts)
	// The top record: a stale redelivery's StoredAt would report its own age.
	r.observe(started, rows, boundBy, tally, topRecord(consumed))
	return consumed, nil
}

// measureDrain folds one batch's rate into the smoothed estimate.
//
// A BATCH THAT TOOK NO MEASURABLE TIME IS SKIPPED, not treated as infinitely
// fast: one would push the estimate to a number that reads every lag as zero.
func (r *Runner) measureDrain(started time.Time, records int) {
	elapsed := r.now().Sub(started)
	if records <= 0 || elapsed <= 0 {
		return
	}
	rate := float64(records) / elapsed.Seconds()
	// One commit per run ([Runner.applyRun]).
	commits := 1 / elapsed.Seconds()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.drain == 0 {
		// The first batch is the estimate: smoothing from zero would understate
		// the rate right after a restart, when the backlog is longest.
		r.drain, r.commits = rate, commits
		return
	}
	r.drain += DrainSmoothing * (rate - r.drain)
	r.commits += DrainSmoothing * (commits - r.commits)
}

// Drain is this loop's measured records per second. ZERO MEANS UNMEASURED:
// every caller falls back to a floor rather than divide by it.
func (r *Runner) Drain() float64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.drain
}

// Commits is this loop's measured transactions per second, 0 unmeasured.
// Published for an operator to compare with the device's write rate.
func (r *Runner) Commits() float64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.commits
}

// applyResult is what applying one record did, counted once its transaction
// commits, never inside it ([Runner.gatedRecord]).
type applyResult struct {
	// rows is every row the record wrote or removed, for the row budget.
	rows int

	// swept is how many anchors a trimmed record removed (ADR-0080).
	swept int

	// gated is whether a gate dropped the record, and reason which.
	gated  bool
	reason Reason
}

// applyOne runs the domain's state machine for one record, unless a gate says
// it must produce no rows, reporting which. It says NOTHING: it runs inside a
// body the store may run again, so the caller notes the drop ([applyNote], or
// the reprocess's answer) for [Runner.gatedRecord] after commit.
func (r *Runner) applyOne(ctx context.Context, tx *sql.Tx, rec Record, opts ApplyOptions) (applyResult, error) {
	started := r.now()
	opts.StoredAt = rec.StoredAt
	// The framework's gates first, the checkpoint's rules: a generation the
	// placing reanchor abandoned, and one a restored reanchor overtook. Asked of
	// the record's OWN generation (the writer's stamp), never the loop's composed
	// position, which carries the checkpoint's generation for every record.
	var reason Reason
	var gated bool
	switch {
	case r.void.abandons(rec.Gen):
		reason, gated = ReasonAbandoned, true
	case r.void.overtakes(rec.Position.Seq, rec.Gen):
		reason, gated = ReasonOvertaken, true
	default:
		var err error
		reason, gated, err = r.applier.Gated(ctx, tx, rec)
		if err != nil {
			return applyResult{}, fmt.Errorf("statelog: read the apply gates at %s: %w", rec.Position, err)
		}
	}
	if gated {
		return applyResult{gated: true, reason: reason}, nil
	}
	n, err := r.applier.Apply(ctx, tx, rec, opts)
	if err != nil {
		return applyResult{}, fmt.Errorf("statelog: apply %s at %s: %w", rec.Kind, rec.Position, err)
	}
	if err := r.tables.writeOp(ctx, tx, rec.OpID, r.tables.subjectOf(rec.Subject), rec.Position,
		rec.StoredAt, opts.Now); err != nil {
		return applyResult{}, err
	}
	out := applyResult{rows: n}
	if rec.Subject.Kind == TrimmedKind {
		// The framework's effect of a trimmed record (ADR-0080), AFTER the domain's
		// apply, so a domain that forgot the kind faults rather than having
		// anchors removed by a record it cannot name. A FLOOR THAT DOES NOT PARSE
		// IS A FAULT: the one writer ([PlanSweep]) checks every body.
		floor, err := TrimmedFloor(r.spec.Name, rec.Subject)
		if err != nil {
			return applyResult{}, fmt.Errorf("statelog: the trimmed record at %s: %w", rec.Position, err)
		}
		if out.swept, err = r.tables.sweepAnchors(ctx, tx, floor); err != nil {
			return applyResult{}, err
		}
		out.rows += out.swept
	}
	if r.metrics != nil {
		// One record's apply bounds how long a read can be delayed. A TIMING, so
		// it stays inside the body (a rolled-back attempt still held the applier);
		// every COUNT moves out, or an abandoned attempt states it twice.
		r.metrics.Observe(metrics.StatelogApplyRecordDuration, r.now().Sub(started),
			metrics.Attrs{"domain": r.domain.Name(), "kind": rec.Kind})
	}
	return out, nil
}

// applyNote is one thing an apply transaction observed about one record that is
// SAID rather than written: a warning line, and for two of the three kinds a
// counter an alarm reads.
//
// A VALUE, NOT A CALL: the store re-runs a transiently failed body
// ([store.Writer.Tx]) and rolls back its rows, not a log line or counter, so a
// retention on an abandoned attempt would be announced and counted twice. The
// body notes, resets on every attempt, and [Runner.announce] says them ONCE
// after the commit.
type applyNote struct {
	rec  Record
	kind noteKind

	// gate is the reason a gated record applied nowhere.
	gate Reason
}

// noteKind is which of the three things a note says.
type noteKind int

const (
	// noteUnverifiable — retained because it is signed under a key this
	// node does not hold.
	noteUnverifiable noteKind = iota

	// noteDeferred — retained because a newer build wrote it.
	noteDeferred

	// noteGated — consumed, and applied nowhere, because a gate said so.
	noteGated
)

// announce says what one committed transaction noted, once.
func (r *Runner) announce(ctx context.Context, notes []applyNote) {
	for _, note := range notes {
		rec := note.rec
		switch note.kind {
		case noteUnverifiable:
			r.countWith(metrics.StatelogRecordsUnverifiable, metrics.Attrs{
				"domain": r.domain.Name(),
			})
			r.logger.WarnContext(ctx, "statelog_record_unverifiable",
				"domain", r.domain.Name(), "position", rec.Position.String(),
				"kind", rec.Kind, "node_holds", r.verifier.KeyIDs(),
				"detail", "retained until this node's secrets.keys holds the key "+
					"it was signed under; expected briefly during a keyring "+
					"rotation and an operator error if it persists")
		case noteDeferred:
			r.logger.WarnContext(ctx, "statelog_record_deferred",
				"domain", r.domain.Name(), "position", rec.Position.String(),
				"kind", rec.Kind, "record_version", rec.V,
				"build_reads", r.domain.RecordVersion())
		case noteGated:
			r.gatedRecord(ctx, rec, note.gate)
		}
	}
}

// Voided reports whether this applier drops a record stamped with generation gen
// at sequence seq under a rule the placing reanchor made (abandoned or
// overtaken), and which: the rules applyOne asks ([Voids]).
func (r *Runner) Voided(gen uint32, seq uint64) (Reason, bool) {
	r.mu.Lock()
	void := r.void
	r.mu.Unlock()
	switch {
	case void.abandons(gen):
		return ReasonAbandoned, true
	case void.overtakes(seq, gen):
		return ReasonOvertaken, true
	}
	return "", false
}

// gatedRecord says that a durable record applies nowhere, and under which gate.
// It still advanced the anchor and checkpoint but leaves no trace in the rows,
// so the line and counter are its only witnesses.
//
// ONLY ONCE THE DROP HAS COMMITTED: a report from inside a re-run body would
// count one drop per attempt on the counter the records_gated alarm reads.
func (r *Runner) gatedRecord(ctx context.Context, rec Record, reason Reason) {
	r.logger.WarnContext(ctx, "statelog_record_gated",
		"domain", r.domain.Name(), "position", rec.Position.String(),
		"kind", rec.Kind, "gate", string(reason), "writer", rec.Writer)
	r.countWith(metrics.StatelogRecordsGated, metrics.Attrs{
		"gate": string(reason), "subject_kind": rec.Subject.Kind,
	})
}

// anyDeferred reports whether this node holds any record it cannot decode, so
// the per-record probe is skipped on the ordinary path. An existence probe, not
// [tables.oldestDeferred], which also reads the scope: this runs at the top of
// every apply transaction.
func (r *Runner) anyDeferred(ctx context.Context, tx *sql.Tx) (bool, error) {
	return r.tables.holdsDeferred(ctx, tx)
}

// ack acknowledges every record this transaction consumed, BY IDENTITY, not
// count: a run mixes applied, retained, gated and below-checkpoint records, and
// acking "as many as were applied" acks the wrong ones.
//
// NEVER A NAK: a record this node cannot apply is retained, stopped on or
// committed, and a nak would hand it straight back.
func (r *Runner) ack(ctx context.Context, consumed []Record) {
	for _, rec := range consumed {
		if rec.ack == nil {
			continue
		}
		if err := rec.ack(); err != nil {
			// A failed ack is a redelivery, which the checkpoint drops.
			r.logger.WarnContext(ctx, "statelog_ack_failed",
				"domain", r.domain.Name(), "position", rec.Position.String(),
				"error", err.Error())
		}
	}
}

// refreshDeferred re-reads the earliest record this node cannot decode.
func (r *Runner) refreshDeferred(ctx context.Context) error {
	return r.db.Read(ctx, func(tx *sql.Tx) error {
		d, ok, err := r.tables.oldestDeferred(ctx, tx)
		if err != nil {
			return err
		}
		r.mu.Lock()
		defer r.mu.Unlock()
		r.deferred, r.hasDefer = d, ok
		return nil
	})
}

// advance moves the in-memory checkpoint the publisher's fast path reads to a
// committed position, with reached and changed ([Runner.Changed]), and answers
// the reached position it leaves.
func (r *Runner) advance(at Position, record time.Time, changed Position) Position {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.changed.Packed() < changed.Packed() {
		r.changed = changed
	}
	if r.cursor.Packed() < at.Packed() {
		// The pair moves together ([Runner.checkpointAt]).
		r.cursor, r.checkpointAt = at, record
	}
	if r.reached.Packed() < r.cursor.Packed() {
		r.reached = r.cursor
	}
	r.appliedAt = r.now()
	return r.reached
}

// checkpointRecord is the instant of the record the committed checkpoint names.
func (r *Runner) checkpointRecord() time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.checkpointAt
}

// stop halts the applier and records why.
//
// HEALTH GOES FALSE IMMEDIATELY, with no grace: "cannot run this company's
// records" is worth moving work for, unlike "briefly behind". Every stop comes
// through here: the ONE line a stop writes.
func (r *Runner) stop(ctx context.Context, err error) error {
	r.mu.Lock()
	if r.stopped == nil {
		r.stopped = err
	}
	r.mu.Unlock()
	r.logger.ErrorContext(ctx, "statelog_applier_stopped",
		"domain", r.domain.Name(), "stream", r.spec.Name,
		"position", r.Committed().String(), "error", err.Error(),
		"detail", "this node's rows for this domain are frozen here and every "+
			"read of them refuses; this node takes its copy of this domain out "+
			"of service and its operations on it go to the other data nodes, "+
			"while its seats stay; a build "+
			"that can read what this one could not resumes it at its next boot, "+
			"and for a recreated stream an operator's reanchor of this one "+
			"stream resumes it in place, with no restart")
	return err
}

// results counts what happened to the records one transaction consumed, by
// outcome: a node advancing while it applies NOTHING (every record retained) is
// healthy on lag and useless in fact.
type results struct {
	applied  int
	retained int
	skipped  int

	// gated is how many records a gate dropped in this attempt ([applyNote]).
	gated int

	// version is the highest record version this transaction APPLIED, which
	// raises the checkpoint row's applied record version ([tables.setCursor]);
	// zero where it applied none.
	version int

	// swept is how many anchors this transaction's trimmed records removed
	// (ADR-0080).
	swept int

	// barriers is how many records consumed for the first time were barriers
	// (each a record on the log, which the census counts), BY THE HOUR THE
	// BROKER STORED EACH. Nil until the first.
	barriers map[time.Time]uint64
}

// barrier counts one barrier the broker stored at storedAt, or at applied (the
// transaction's instant) for a record that carries no broker instant.
//
// BY ITS COMMIT HOUR, never the hour it is applied in: the census reads these
// as the log's reads in the last day, and a node replaying days of backlog in
// minutes would read as a company far past its census.
func (t *results) barrier(storedAt, applied time.Time) {
	at := storedAt
	if at.IsZero() {
		at = applied
	}
	if t.barriers == nil {
		t.barriers = map[time.Time]uint64{}
	}
	t.barriers[at.UTC().Truncate(time.Hour)]++
}

// countAborts records the transactions the store rolled back under this apply.
//
// ATTEMPTS MINUS ONE, the last being the one that committed. Zero on the
// ordinary path, hence a counter: an operator watches whether it moves at all.
func (r *Runner) countAborts(attempts int) {
	if r.metrics == nil || attempts <= 1 {
		return
	}
	r.metrics.Add(metrics.StatelogApplyTxAborts, uint64(attempts-1),
		metrics.Attrs{"domain": r.domain.Name()})
}

func (r *Runner) observe(started time.Time, rows int, boundBy string, tally results, last Record) {
	if r.metrics == nil {
		return
	}
	domain := r.domain.Name()
	if boundBy == "" {
		boundBy = "drained"
	}
	now := r.now()
	r.metrics.Observe(metrics.StatelogApplyTxDuration, now.Sub(started),
		metrics.Attrs{"domain": domain, "bound_by": boundBy})
	r.metrics.ObserveValue(metrics.StatelogApplyBatchRows, float64(rows),
		metrics.Attrs{"domain": domain})
	for result, n := range map[string]int{
		"applied": tally.applied, "retained": tally.retained,
		"gated": tally.gated, "skipped": tally.skipped,
	} {
		if n > 0 {
			r.metrics.Add(metrics.StatelogApplyRecords, uint64(n),
				metrics.Attrs{"domain": domain, "result": result})
		}
	}
	// The log's read rate: counted after the commit, filed at each barrier's
	// COMMIT hour ([results.barrier]), labelled with the broker's STREAM name.
	for hour, n := range tally.barriers {
		r.metrics.AddAt(metrics.StatelogBarriersApplied, n, hour,
			metrics.Attrs{"domain": domain, "stream": r.spec.Name})
	}
	r.countSwept(tally.swept)
	// The commit-to-apply gap, from the BROKER's timestamp: a node's own fetch
	// time hides exactly the delay every read level is a policy about.
	if !last.StoredAt.IsZero() {
		r.metrics.Observe(metrics.StatelogApplyLatency, now.Sub(last.StoredAt),
			metrics.Attrs{"domain": domain})
	}
	r.metrics.Set(metrics.StatelogWaiters, float64(r.waiters.len()),
		metrics.Attrs{"domain": domain})
}

func (r *Runner) count(name string) {
	r.countWith(name, metrics.Attrs{"domain": r.domain.Name()})
}

// countSwept counts anchors removed in a COMMITTED transaction, never inside
// one ([Runner.gatedRecord]).
func (r *Runner) countSwept(n int) {
	if r.metrics == nil || n == 0 {
		return
	}
	r.metrics.Add(metrics.StatelogAnchorsSwept, uint64(n),
		metrics.Attrs{"domain": r.domain.Name()})
}

func (r *Runner) countWith(name string, attrs metrics.Attrs) {
	if r.metrics == nil {
		return
	}
	r.metrics.Add(name, 1, attrs)
}
