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
// missing sequence: two full fetches plus slack. On a strict log an
// out-of-order arrival is a redelivery, not a hole: the broker redelivers the
// missing record.
//
// OVERFLOW IS A FAULT, not a gap to step over: applying past a hole produces
// state no other node holds, which a replicated log cannot notice later.
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

	// Release hands this delivery back to the broker, which redelivers it at
	// once. Called instead of Ack on a record the loop will NOT consume: one held
	// from before a moment after which the log may be another history (a failed
	// fetch, an end read below what it holds; [Runner.Run]'s unwind). Never
	// inside a transaction.
	//
	// Nil for none: the record then waits out its ack window, and a strict log
	// stands still behind it.
	Release func() error
}

// Fetcher delivers a domain's records in stream order. Deliberately narrow: a
// consumer's name, ack policy and redelivery window belong to its creator.
type Fetcher interface {
	// Fetch pulls up to maxMessages records or maxBytes of them, whichever
	// binds first, waiting up to wait for the first one.
	//
	// EVERYTHING THE BROKER DELIVERED IS RETURNED, by this call or a later one:
	// a delivered record the loop never sees holds the ack-pending cap and is
	// redelivered only after its ack window, a hole in a strict log. Where the
	// broker cannot bound a pull by both count and bytes, the consumer's
	// in-flight ceiling ([FetchMessages]) honours maxMessages.
	//
	// EXCEPT ACROSS A FAILURE: a failing call hands back whatever it held. The
	// loop gives back every record from before a failed fetch ([Runner.Run]),
	// so a record kept across one would look delivered after it.
	Fetch(ctx context.Context, maxMessages, maxBytes int, wait time.Duration) ([]Message, error)

	// Pending is how many records this consumer has not yet handed to a Fetch:
	// whether waiting would fill a partial batch. A HINT asked on every run, so
	// answer it from what fetches already learned, never with a request on the
	// commit path. An error is "not known": it closes the run, not the applier,
	// and never claims the loop level with the log.
	Pending(ctx context.Context) (uint64, error)
}

// CheckpointLog is the log read BY POSITION: its two ends and the record at one
// sequence. The applier asks it whether the record at its checkpoint is the
// one the checkpoint names ([Runner.VerifyCheckpoint]), which a consumer
// cannot answer: it delivers what follows a position.
type CheckpointLog interface {
	LogReader

	// Bounds is the log's first surviving sequence and its last, in one
	// answer.
	Bounds(ctx context.Context) (first, last uint64, err error)
}

// ErrStopped reports an applier that halted rather than fell behind. The two
// need different answers: a behind node catches up, and a stopped one needs a
// different build.
var ErrStopped = errors.New("statelog: the applier stopped")

// Estate is the replicated database as the applier needs it, RESOLVED PER CALL
// rather than held: an adoption replaces the file under a running node
// (closed, renamed over, reopened), so a handle captured at boot answers from
// a database no longer at that name. It answers [store.ErrNoEstate] in the
// window where there is none.
type Estate interface {
	// Read runs fn in a read transaction on the current estate.
	Read(ctx context.Context, fn func(*sql.Tx) error) error

	// Tx runs fn in a write transaction on the current estate.
	Tx(ctx context.Context, fn func(*sql.Tx) error) error

	// Writer pins one connection on the current estate for the life of
	// an apply loop, and the loop releases it before the estate can be
	// replaced.
	Writer(ctx context.Context) (*store.Writer, error)

	// Caps is the CURRENT estate's probed capabilities, for the same reason: a
	// value captured at construction describes a replaced file.
	// [ApplyOptions.MaxVariables] is filled from it once per batch.
	Caps() store.Capabilities
}

// RunnerDeps is everything an applier needs that it does not own.
type RunnerDeps struct {
	Domain  Domain
	Applier Applier
	Fetch   Fetcher

	// Spec is the stream this runner applies: the domain's own ([Domain.Stream])
	// with the byte ceiling this node's Tier A sized, and held to the
	// declaration in all else ([StreamSpec.Instantiates]), because the
	// checkpoint row, generation, anchors and stream identity are keyed by name.
	Spec StreamSpec

	// Verifier authenticates every record before this domain decodes one.
	// REQUIRED: a runner built without it would apply whatever reached the
	// broker, and the broker's admission vouches for a connection, never a
	// record. See [recordseal].
	Verifier *recordseal.Verifier

	// Log is the same stream read by position, so the applier can establish
	// that the log still holds, at its checkpoint's sequence, the record it
	// consumed there ([Runner.VerifyCheckpoint]). REQUIRED: without it a
	// restored log's new history is applied on top of rows it is not.
	Log CheckpointLog

	// Node is this node's OWN estate, where the applier keeps a verdict the log
	// may not show it again: that the log diverged from its rows
	// ([ErrLogDiverged]). REQUIRED, for [NodeEstate]'s reason.
	Node NodeEstate

	// DB is the REPLICATED estate: this domain's rows, operation ledger,
	// deferred records, anchors and checkpoint, one file because contract 2 puts
	// them in one transaction. Resolved per call ([Estate]).
	DB Estate

	// Checkpoint is THIS DOMAIN's committed checkpoint as its row holds it
	// (stream, generation, sequence), and CheckpointStoredAt the broker's
	// instant for the record at it, zero where the row names none. The runner
	// stands there until its loop loads the row; only a reanchor of this
	// domain's stream moves it under a running process ([Runner.Reanchored]).
	//
	// THE WHOLE CHECKPOINT, never its generation alone: until the loop loads the
	// row, the heartbeat, a peer's truncation fence and the checkpoint's
	// verification read this as the node's position, and a zero sequence names
	// no record. The empty stream is this domain's own; any other is refused.
	Checkpoint         Position
	CheckpointStoredAt time.Time

	// StreamCreatedAt is this stream's creation instant ([StreamCreatedAt]) AS A
	// READING OF THE STREAM REPORTS IT NOW, stored beside the checkpoint to
	// detect a recreated stream, which starts its sequences again: the loop
	// STOPS when it differs from the instant the checkpoint was committed under.
	// Never take it from the checkpoint row, which would compare the value with
	// itself. The zero value skips the comparison; the engine never passes it.
	//
	// A reanchor re-keys it to the instant the operator confirmed
	// ([Runner.Reanchored]).
	StreamCreatedAt time.Time

	// Epoch is where the per-epoch configuration the domain declared it reads
	// comes from, asked once per BATCH ([ApplyOptions.Epoch]). Nil hands every
	// batch none.
	//
	// A SOURCE, NOT A VALUE: the runner outlives every epoch it applies under,
	// and a value captured at boot applies one record under different epochs on
	// nodes that restarted at different times. Once per batch because the batch
	// is the transaction, as with [ApplyOptions.Now]. It must not block (it runs
	// on the apply loop) and must be safe to call from that goroutine while the
	// caller changes it.
	Epoch func() map[string]any

	Metrics *metrics.Recorder

	// Logger is where this writes. Nil is the package's component logger, never
	// silence ([loggerOr]).
	Logger *slog.Logger

	// NodeID is this node's own id, and Evicted answers whether the fleet has
	// evicted a node: both pick the remedy for a record from a generation these
	// rows never entered ([passedGeneration.err]). An empty id or a nil Evicted
	// leaves the peer's sentence, which names both remedies.
	NodeID  string
	Evicted func(ctx context.Context, node string) (bool, error)

	// OnDrained is told when this loop first reaches the end of its log after it
	// starts (when [Runner.Drained] turns true), and not again until it runs
	// again. Nil tells nobody. A PUSH because a node's snapshot gate waits on it
	// ([SkipUnhydrated]), and asking on a timer learns of it a timer late.
	//
	// CALLED ON THE APPLY LOOP'S GOROUTINE, outside the runner's lock and after
	// [Runner.Drained] reads true, so a reader it wakes sees the latch set. It
	// must not block: the loop applies nothing until it returns.
	OnDrained func()

	// OnLevel is told each time this loop comes LEVEL with its log (an empty
	// fetch while it holds nothing) having been otherwise since last told: just
	// started, holding records, or retrying a failure. Nil tells nobody. A
	// separate latch from OnDrained because seat claims wait on being level NOW,
	// the strict half of [Health.Established], which recurs after every burst
	// and every outage.
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
// writer. An applier's methods are called by one goroutine at a time (the
// loop waits for the one beside it first), so an applier needs no lock.
//
// The store MAY RE-RUN a transaction body (an effect inside would announce a
// position that never committed), so every effect follows the commit, in this
// order: the publisher's cursor, the waiters at or below it, the domain's
// non-row consequences, then the acks, by identity over every record consumed.
//
// It keeps two positions, [Runner.Committed] and [Runner.Reached], and gives
// back at a doubt what it holds past the checkpoint ([Runner.doubtLocked]).
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

	// created is the creation instant of the stream this runner's positions
	// are keyed to — the stream's own ([StreamCreatedAt]) at construction, and
	// the one a reanchor confirmed after it. UNDER mu, because a reanchor
	// re-keys it while the heartbeat compares live readings against it.
	created time.Time

	// cursor is the committed checkpoint, and ITS GENERATION IS THE RUNNER'S:
	// every record the loop decodes is placed at it and every anchor read with
	// it. One field, so a reanchor cannot move one without the other.
	cursor Position
	// reached is the REACHED POSITION ([Runner.Reached]): never below cursor,
	// moved past it only by [Runner.reach], and set wherever cursor is set
	// rather than advanced — a load, a reanchor, a join — because each of
	// those replaces the rows it was a claim about. UNDER mu.
	reached Position
	// changed is the position of the last record a committed transaction
	// of this loop's CHANGED A ROW with — every record but a barrier this
	// node applied ([writesNoRow]) — and the checkpoint wherever cursor is
	// set rather than advanced, for reached's reason ([Runner.Changed]).
	// UNDER mu.
	changed Position
	// checkpointAt is the broker's instant for the record at the cursor's
	// sequence, zero where unknown ([cursorRow.storedAt]). UNDER mu AND MOVED
	// WITH cursor: [Runner.VerifyCheckpoint] reads the pair while the loop runs,
	// and a sequence paired with another record's instant is a false divergence.
	checkpointAt time.Time
	// rows counts the times the rows under the checkpoint may have been
	// replaced or re-keyed — every load of the row, every reanchor, every
	// join — so a verification that read the checkpoint before one of them
	// and the log after it can tell that its finding is about a copy this
	// runner no longer holds ([Runner.VerifyCheckpoint]).
	rows uint64
	// verify is a verification of the checkpoint's record the loop owes before
	// applying past it, set at each moment after which the log past the
	// checkpoint may be another history ([Runner.doubtLocked]) and cleared only
	// by the loop ([Runner.verifyBeforeApplying]).
	verify bool
	// doubts counts those moments: every record held from before one (in hand,
	// in the reorder buffer, waiting for a checkpoint transaction) is given back
	// rather than applied, reached or deduplicated ([Runner.Run]'s unwind). A
	// verification settles only the doubt it was asked under. UNDER mu.
	doubts uint64
	// void is what the reanchor that placed the checkpoint made void, read with
	// it by [Runner.loadCursor]: a record of an ABANDONED generation, or one a
	// restored reanchor OVERTOOK, is consumed into no row ([ReanchorPlan.From],
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
	// reader sampling lag on a timer can miss every instant a busy log is empty,
	// while the loop sees each empty fetch, or run committed whole with nothing
	// pending, while it holds nothing ([Runner.nextRun]). RESET WHEN THE LOOP
	// RESTARTS, because an adoption installs a peer's rows under a new history.
	drained bool

	// level is whether the loop's last fetch found it level with its log and it
	// has since fetched no records, met no failed fetch and not restarted: what
	// makes the next empty fetch news ([RunnerDeps.OnLevel]).
	level bool

	// foreign is the stream this applier's positions do NOT belong to, once the
	// boot's comparison or a live reading ([Runner.ObserveStream]) finds the
	// broker serving another under this domain's name. Nil until then.
	//
	// NOT CLEARED BY A RE-RUN: it is a verdict about the positions, which a
	// re-run starts from. A reanchor ([Runner.Reanchored]) or an adoption
	// ([Runner.Rejoined]) moves them, and a re-run re-derives it against the row
	// it loads ([Runner.loadCursor]). See [Runner.StreamIdentity].
	foreign *foreignStream

	// passed is the generation the FLEET is on for this domain, once a peer is
	// found to have re-anchored the log past these rows: by the heartbeat
	// ([Runner.ObserveFleetGeneration]) or by a record from a generation this
	// applier never entered ([Runner.decode]). Never set for a domain that claims
	// no identity, whose nodes re-anchor one at a time.
	//
	// STICKY LIKE foreign: nothing on the log moves these rows into the new
	// generation. An adoption replaces them ([Runner.Rejoined]), and a re-run
	// judges it against the checkpoint it loads.
	passed *passedGeneration

	// ahead is the verdict of the last reading that found this applier's
	// checkpoint PAST the log's end ([Runner.ObserveEnd]), else nil. Kept rather
	// than read live because fence 0 runs on every append and answers only from
	// what this node already knows.
	//
	// NOT STICKY: an end below the checkpoint can be one member's stale view (a
	// leaderless stream group answers from its own state). A later reading clears
	// it only once its end reaches THIS verdict's checkpoint, and
	// [Runner.loadCursor] drops it for a checkpoint no longer stood at. Clearing
	// costs nothing: past that point the record at the checkpoint separates the
	// histories, which the loop verifies ([Runner.diverged]).
	ahead *aheadOfLog

	// diverged is the verdict that the log's record at this applier's checkpoint
	// is not the one it consumed there ([ErrLogDiverged]); nil until
	// [Runner.VerifyCheckpoint] finds it. STICKY, unlike ahead: a lagging member
	// answers that it holds no record, never a different one. The loop applies
	// nothing past the checkpoint while it holds; a reanchor
	// ([Runner.Reanchored]), an adoption ([Runner.Rejoined]) or a re-run over
	// another checkpoint ([Runner.loadCursor]) ends it.
	//
	// DURABLE in the node estate ([NodeEstate]), since the log may lose the
	// record it was found by ([Runner.recallDiverged]). divergedStored says
	// whether this one has been; if not, every verification records it again.
	diverged       *divergedLog
	divergedStored bool

	// truncated is the verdict that the log lost records a PEER's rows hold
	// ([ErrLogTruncated]), nil while no reading of the positions register finds
	// one ([Runner.ObserveTruncation]). NOT part of [Runner.StreamIdentity]: this
	// node's rows are the log's own history, so only its writes refuse
	// ([Runner.Truncated]). RECOMPUTED ON EVERY READING, being a fact about
	// other nodes' rows.
	truncated *Truncation

	// drain is this loop's measured records per second. A bounded stale read, a
	// refusal's `retry_after_seconds` and the apply-lag alarm divide a backlog
	// by it to report a TIME, so a constant rate would misreport a node two
	// thousand records behind as half an hour behind. An exponentially weighted
	// mean over batches, because one small batch at a burst's tail is not the
	// loop's rate.
	drain float64

	// commits is the same measurement over TRANSACTIONS, smoothed identically:
	// the FSYNC rate, which under `synchronous = FULL` spends a device's write
	// budget. It moves apart from drain because [Runner.nextRun] fills a run
	// toward the transaction budget, so a doubled commits/s at a healthy rows/s
	// is a disk doing twice the work for the same progress.
	commits float64
}

// DrainSmoothing is how much of a new batch's rate enters the measurement. A
// quarter: four batches of a new rate move the estimate about 68 % of the
// way, while one anomalous batch moves it by a quarter of its own error.
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
		// THE REPLICATED ESTATE'S OWN DATABASE is the one handle the type
		// does not stop — it answers Estate like the node's — and the verdict
		// written into it would be lost on the first adoption that
		// replaces the file, which is when it is needed.
		return nil, fmt.Errorf("statelog: applier was handed the %q estate as "+
			"this node's own; a log's divergence verdict is kept in the node's "+
			"file (store.OpenNode), which no adoption replaces", d.Node.Estate())
	case d.DB == nil:
		return nil, fmt.Errorf("statelog: applier has no database")
	case d.Verifier == nil:
		// REFUSED, NOT DEFAULTED TO ACCEPTING: the broker's admission vouches for
		// no record, so a missing keyring fails the boot naming what to configure.
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

// batchOptions is what every record of one transaction is applied under: the
// runner's static declarations, the batch's instant, the store's bind limit
// and the epoch AS IT STANDS NOW — read here, per batch, so an activation
// reaches the next batch rather than the next process ([RunnerDeps.Epoch]).
func (r *Runner) batchOptions(now time.Time) ApplyOptions {
	opts := r.opts
	opts.Now = now
	opts.MaxVariables = r.db.Caps().MaxVariables
	if r.epoch != nil {
		opts.Epoch = r.epoch()
	}
	return opts
}

// Committed is this node's COMMITTED CHECKPOINT on the stream: what its
// checkpoint row holds, and so what everything durable reads — where a restart
// resumes, what the heartbeat publishes ([Runner.Stance]), the write fence.
// What a reader waits for is [Runner.Reached].
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
// so a read answers from the same rows whether its checkpoint has committed or
// not, and a stalled fsync is no reason to answer `behind`. But a barrier
// reached and not committed is unacknowledged, and a restart reaches it again.
func (r *Runner) Reached() Position {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.reached
}

// doubtLocked records one moment after which the log past the checkpoint may
// be ANOTHER HISTORY: a run starting, a fetch failing, an end read below what
// the loop holds. Called under mu. The loop waits for a transaction already
// committing before it verifies ([Runner.Run]). It:
//
//   - owes a verification ([Runner.verifyBeforeApplying]): nothing past the
//     checkpoint applies until the log holds the record the checkpoint names;
//   - sends the reached position back to the checkpoint, since a reached,
//     uncommitted barrier may be gone from the log, until that settles;
//   - counts the doubt ([Runner.doubts]), so the loop gives back everything
//     it holds from before it and an older verification does not settle it.
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

// settledLocked takes a verification that found, at the checkpoint, the record
// the checkpoint names, asked when [Runner.doubts] stood at doubts. It clears
// the verification owed unless a doubt was recorded since, which only a look
// of its own settles. Called under mu.
func (r *Runner) settledLocked(doubts uint64) {
	if r.doubts == doubts {
		r.verify = false
	}
}

// mayCommitBeside reports whether the checkpoint transaction beside the loop
// may commit batch now ([checkpoints]): not while a divergence holds, nor while
// a verification is owed for anything in it past the checkpoint (the loop
// settles that itself and gives such a batch back). A batch of redeliveries at
// or below the checkpoint needs no verification, so its acks are not held up.
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
	// At is the position of the last record this node committed whose
	// transaction changed a row: every record but a barrier.
	At Position

	// loads is how many times the rows under the checkpoint may have been
	// replaced or reloaded when it was read ([Runner.rows]): a load
	// reprocesses what an earlier build retained, which changes rows and
	// moves no position.
	loads uint64
}

// String is the reading as one token, which two readings of the same rows share
// and two of different rows do not.
func (c Change) String() string { return fmt.Sprintf("%s#%d", c.At, c.loads) }

// Changed is where this node's rows LAST CHANGED: the position of the last
// record its loop committed whose transaction wrote a row, beside how many
// times the rows under the checkpoint may have been replaced or reloaded. Two
// equal readings are of the same rows.
//
// NOT [Runner.Reached] OR [Runner.Committed]: both move on every barrier, which
// every linearizable read in the fleet writes, so a cache keyed on them (the
// knowledge answers) is retired by its own reads. A barrier writes no row
// ([writesNoRow]); this moves on everything else, a retained record included.
func (r *Runner) Changed() Change {
	r.mu.Lock()
	defer r.mu.Unlock()
	return Change{At: r.changed, loads: r.rows}
}

// reach moves the reached position over what run carries that writes no row
// ([reachedOver]) and releases every waiter at or below it, on the loop before
// any transaction: a barrier leaves nothing a re-run body could roll back
// ([waiters.release]).
//
// NOT WHILE ANYTHING PAST THE CHECKPOINT MAY BE ANOTHER HISTORY (a verification
// owed, a divergence found), AND NOT WHILE THIS NODE RETAINS A RECORD: a
// barrier whose scope meets a retained one is retained too, a row, so there
// only commits move it.
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
// ([coord.DomainPosition]) and counted by the trim, so a published, uncommitted
// barrier would let a restart resume below a floor already purged. ONE READ
// under one lock: a sequence paired with another record's instant names no
// record ([Runner.checkpointAt]), and a verdict is judged against the rows its
// reading was about ([Runner.ObserveTruncation]). The unexported reached
// position is for [Runner.ObserveEnd] alone, read BEFORE the end; nothing
// publishes it.
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

// StreamCreatedAt is the creation instant of the stream this runner's
// positions are keyed to: the stream's own ([StreamCreatedAt]) when built, and
// the one a reanchor confirmed after that. Every live reading
// ([Runner.ObserveStream]) and the checkpoint's load compare against it. The
// runner's own rather than a boot-time copy, because a reanchor re-keys it
// under a running process.
func (r *Runner) StreamCreatedAt() time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.created
}

// KeyedTo is the creation instant of the stream this runner's CHECKPOINT is
// keyed to: the one its rows were derived from. It is [Runner.StreamCreatedAt]
// except while the recreation verdict holds, when the runner may carry either
// instant depending on whether it met the rebuild at boot or running; the
// verdict holds the rows' own, which a published position must name
// ([coord.DomainPosition]).
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
// It clears what a re-run may not: the positions are sequences on the live
// stream again, so the recreation verdict, a passed generation the new one
// reached, an end read against the old checkpoint and a divergence about its
// record no longer hold. CALLED WITH THE LOOP STOPPED, the loop being the one
// writer of the checkpoint: a running loop could commit at the old generation
// after the reanchor's transaction. The next run resumes from the committed
// row ([ReanchorCase]).
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
	// THE DIVERGENCE TOO, and its record in the node estate is about the
	// checkpoint this left, which [Runner.recallDiverged] removes the next
	// time it reads it.
	r.foreign, r.ahead, r.diverged, r.divergedStored = nil, nil, nil, false
	// AND A PEER'S TRUNCATION, which was about peers in the generation this
	// left: in the one it opened, this node's rows are the fleet's history.
	r.truncated = nil
	if r.passed != nil && at.Generation >= r.passed.fleet {
		r.passed = nil
	}
	// AND THE STOP THE RECREATION OR THE DIVERGENCE CAUSED, which the reanchor
	// answers; a stop for any other reason is about this build and stays.
	if errors.Is(r.stopped, ErrStreamRecreated) || errors.Is(r.stopped, ErrLogDiverged) {
		r.stopped = nil
	}
	return nil
}

// Stopped is the error that halted this applier, or nil.
//
// A STOP IS PERMANENT and only [ErrStopped] is one: a gate this build cannot
// read, a hole that will not close, a recreated stream, an envelope no build
// could decode. Every other failure is retried in place — see [Runner.Run] —
// and is reported through [Runner.Fault] instead.
func (r *Runner) Stopped() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.stopped
}

// ObserveStream takes one LIVE reading of the creation instant of this
// applier's stream ([StreamCreatedAt]), and answers true the one time a reading
// establishes that the stream is not the one this applier started against.
//
// TOLD RATHER THAN ASKING: a stream remade under a running node counts from 1
// at the same generation, so nothing the loop reads sees it. The position
// heartbeat ([PositionHeartbeat]) and the fence on an expectation of zero read
// the stream's state and hand the instant here, so the read and write paths
// consult one verdict. A zero instant ([StreamUnknown]), or a runner with no
// identity declared, establishes nothing.
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
// An instant is never reissued, but an adoption can key the rows to the live
// stream ([Runner.Rejoined]). When its re-key failed (a failed join's estate
// restore could not read the live instant), [Runner.loadCursor] judges a
// stream it never read as another: the loop stops as recreated over the
// fleet's history, and a reanchor refuses. So when this answers true, the
// engine asks for a join to re-key against a reading taken now.
func (r *Runner) RecreationStale(live time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.foreign != nil && IdentityOf(r.foreign.keyed, live, true) == StreamSame
}

// ObserveEnd takes one LIVE reading of the log's last sequence, paired with
// where this applier stood as the caller read it BEFORE asking the broker
// ([Runner.Stance]): established is true the one time a reading finds the
// checkpoint past the end, reached the one time a later one finds the end at
// or past the checkpoint an earlier one was established on. In that ORDER,
// because a checkpoint read after the end passes it on a healthy busy log.
//
// An end below the reached position owes a verification ([Runner.doubtLocked])
// even short of the checkpoint, though it establishes nothing. Once a restored
// log is written past the checkpoint its end clears the verdict, so the record
// at the checkpoint decides instead ([Runner.verifyBeforeApplying]): another is
// [ErrLogDiverged], which does not clear.
func (r *Runner) ObserveEnd(s Stance, last uint64) (established, reached bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	at := s.At
	if at.Generation < r.cursor.Generation {
		// A READING FROM BEFORE A REANCHOR, paired with a checkpoint on the old
		// stream: where the adopted one ends says nothing about it, and a verdict
		// on it would refuse writes until the log reached a number the node will
		// never stand at again.
		return false, false
	}
	if pastEnd(s.reached.Seq, last) {
		// WHATEVER FOLLOWS THE CHECKPOINT may now be another history. The reached
		// position is never below the checkpoint, so this covers a checkpoint past
		// the end too.
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
		// STILL PAST IT: this reading was paired with a lower checkpoint and
		// moves only the end. A NEW value, never a write through the shared
		// pointer: [Runner.StreamIdentity] reads it after unlocking.
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
//   - [ErrStreamRecreated]: the stream was rebuilt (found at boot or by
//     [Runner.ObserveStream]); until a reanchor or an adoption.
//   - [ErrGenerationPassed]: a peer re-anchored past this generation; until an
//     adoption.
//   - [ErrLogDiverged]: the log's record at the checkpoint is another
//     ([Runner.VerifyCheckpoint]); until a reanchor or an adoption.
//   - [ErrAheadOfLog]: the log's end was last read below the checkpoint
//     ([Runner.ObserveEnd]); while it stays below.
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
		// BEFORE THE END: a divergence is definitive where a checkpoint past
		// the end may be one member's stale answer, and both can hold at
		// once — a log written past this node's rows and then read from a
		// member behind it.
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
// are other nodes'; a reading that could not be taken is never handed here, so
// an unreadable register leaves the verdict as it was.
//
// s is where this runner stood when the reading was taken ([Runner.Stance]).
// A reading about rows it no longer holds (a reanchor or join landed in the
// round trips between) changes nothing either way: it would refuse writes of a
// node whose rows now head the log, or clear a fence still needed.
func (r *Runner) ObserveTruncation(s Stance, t *Truncation) (established, cleared bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	// THE ROWS' OWN COUNT, which every reload, reanchor and join moves — and
	// every event that moves the generation is one of those three.
	if s.rows != r.rows {
		return false, false
	}
	had := r.truncated != nil
	if t == nil {
		r.truncated = nil
		return false, had
	}
	// A NEW VALUE, never a write through the shared pointer: [Runner.Truncated]
	// reads it after the lock is released.
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
	// nothing can be compared: the checkpoint names no record (sequence 0, or
	// one a reanchor placed where the log held none), the record is gone below
	// the log's first sequence, or a compacted log superseded it.
	checkpointSettled checkpointCheck = iota

	// checkpointUnreached: the log ends below the checkpoint, so there is no
	// record at it to compare yet — the state [Runner.ObserveEnd] refuses
	// on, and one the loop has nothing past the checkpoint to apply in.
	checkpointUnreached

	// checkpointDiverged: the log holds ANOTHER record at the checkpoint's
	// sequence — [ErrLogDiverged].
	checkpointDiverged
)

// VerifyCheckpoint compares the record the log holds at this applier's
// checkpoint with the one the checkpoint names, and answers true the one time
// that establishes that the log diverged from these rows ([ErrLogDiverged]).
//
// On one stream a sequence names one record for ever (a lagging member holds
// none, never another), so a different record there means a broker restored
// from an older copy and written past these rows. Two broker instants are
// compared for EQUALITY, so no clocks are ordered. Asked by the loop after a
// doubt ([Runner.verifyBeforeApplying]) and by the position heartbeat every
// [PositionHeartbeat], which catches a restore between doubts or on a stopped
// loop. A verdict recorded before a restart is recalled first
// ([Runner.recallDiverged]), so a log that lost the record cannot lift it.
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
// adopted only while it is about the checkpoint this runner stands at — the
// same position, naming the same record — because that is the statement that
// these are the rows it was found against. Any other is about a file a
// reanchor or an adoption has since replaced, and is removed: those are the
// two things that end a divergence, and a reading of the log is never one.
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
	// THE SAME RECORD: a verdict is only ever reached against a checkpoint
	// that names one, so a row naming another — or none, as a reanchor
	// leaves one — is another file's.
	if recorded.at != at || consumed.IsZero() || !sameRecord(recorded.consumed, consumed) {
		return false, clearDiverged(ctx, r.node, r.spec.Name, recorded)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.rows != rows || r.diverged != nil {
		// RE-KEYED UNDER THE READ, or reached meanwhile: the next call
		// judges the row against what this runner then stands at.
		return false, nil
	}
	r.diverged, r.divergedStored = &recorded, true
	return true, nil
}

// checkCheckpoint is [Runner.VerifyCheckpoint], reporting which of the three
// outcomes it reached.
func (r *Runner) checkCheckpoint(ctx context.Context) (checkpointCheck, bool, error) {
	// THE PAIR, under one lock: a sequence and the instant of the record
	// consumed there, read together or not at all.
	r.mu.Lock()
	at, consumed, rows := r.cursor, r.checkpointAt, r.rows
	r.mu.Unlock()
	if at.Seq > 0 && consumed.IsZero() {
		// A CHECKPOINT THAT NAMES NO RECORD (a reanchor placed it where the log
		// held none) is compared with nothing; the first batch this node commits
		// names its own. Past the log's end it is unreached, the reading
		// [Runner.ObserveEnd] refuses on.
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
		// THE ROWS WERE REPLACED OR RE-KEYED BETWEEN THE READS — a run
		// that loaded another row, a reanchor, a join — so this is a
		// finding about a copy this runner no longer holds. The next
		// verification reads the pair it does hold.
		return checkpointSettled, false, nil
	}
	established := r.diverged == nil
	if established {
		r.diverged = &divergedLog{at: at, consumed: consumed, held: held}
		r.divergedStored = false
	}
	r.mu.Unlock()
	// RECORDED AT ONCE, in the node estate, so a restart after the log has
	// lost the record this was found by cannot lift it. A failure leaves it
	// held in memory — the loop refuses on it all the same — and recorded
	// on the next verification ([Runner.recallDiverged]).
	_, err = r.recallDiverged(ctx)
	return checkpointDiverged, established, err
}

// CheckpointDiverged reports whether log holds, at checkpoint at, ANOTHER
// record than the one consumed names, and the instant of the one it holds,
// for a reader that is not the checkpoint's runner: a reanchor deciding its
// case from the row. FALSE WHENEVER NOTHING CAN BE COMPARED
// ([checkpointSettled], [checkpointUnreached]): none of those is a second
// history.
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
// almost always prove nothing happened, so the loop asks once after each doubt
// ([Runner.doubtLocked]). Ends that do not reach the checkpoint while the run
// holds a record past it are two members disagreeing: retried, never applied
// on the guess. A divergence, found here or by the heartbeat, STOPS the loop.
// THE LOOP'S ALONE, with no checkpoint transaction in flight and nothing held
// from before the doubt ([Runner.Run]); it settles only the doubt it was
// asked under ([Runner.doubts]).
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
		// RE-KEYED UNDER THE READ: the finding was about rows this runner
		// no longer holds, and the next attempt verifies the ones it does.
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
// A generation moves only by a reanchor or by adopting a snapshot of a node
// that ran one, so a peer at a later one holds the rows the log continues
// from, which nothing on the log brings these rows to. NEVER FOR A DOMAIN THAT
// CLAIMS NO IDENTITY: its nodes re-anchor one at a time by design
// ([PermitReanchor]), so a peer ahead is a recovery in progress.
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
		// A LATER GENERATION replaces what is held, and the SAME one gains
		// the writer the register could not name — which is what a later
		// eviction of that writer is judged by ([Runner.RecheckPassed]).
		switch {
		case p.fleet > r.passed.fleet:
		case p.fleet == r.passed.fleet && r.passed.writer == "" && p.writer != "":
			p.holder = r.passed.holder
		default:
			return false
		}
		// A NEW VALUE, never a write through the shared pointer:
		// [Runner.StreamIdentity] reads it after the lock is released.
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
// A PEER WHEREVER IT CANNOT TELL — no writer, no way to ask, or a question that
// could not be answered — because that sentence names both remedies, the
// adoption and the eviction beside it, where the other two name one each.
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
// register-only verdict whose generation no un-evicted peer holds lost its
// holders to eviction (a published row is withdrawn by nothing else). This
// node's own failed attempt stays its own until the reanchor it started ends it.
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
// An adoption replaces every row, anchor and checkpoint with a peer's, checked
// against the live stream and the fleet's generation, so keeping the
// recreation or passed-generation verdict would stop the loop again. It
// RE-DERIVES them rather than clearing them, because a join that found every
// domain current may stand on an artefact it could not open: a join that
// replaced nothing changes nothing. Called with the loop stopped, for
// [Runner.Reanchored]'s reason.
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
	// A PEER'S TRUNCATION was about peers in the generation these rows stood at
	// ([Runner.Reanchored]): rows adopted into another generation are a peer's
	// history there. One adopted into the SAME generation stays under it until
	// the next reading.
	if at.Generation != r.cursor.Generation {
		r.truncated = nil
	}
	r.cursor, r.reached, r.changed = at, at, at
	r.rows++
	if IdentityOf(keyed, live, !keyed.IsZero()) == StreamRecreated {
		// THE ROWS ARE STILL ANOTHER STREAM'S: the verdict holds, and is
		// established here if nothing had yet.
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
	// A READING OF THE END paired with a checkpoint this runner no longer
	// stands at says nothing about the one it does — [Runner.loadCursor]'s
	// own rule.
	if r.ahead != nil && r.ahead.at != at {
		r.ahead = nil
	}
	// WHICH RECORD A MOVED CHECKPOINT NAMES is the row's to say, read when the
	// loop starts again; until then it is unknown, since comparing the new
	// position with the OLD record would report a divergence between two files.
	// A divergence about the old checkpoint goes with it. One at the SAME
	// position stays (clearing it would let a write through before the loop
	// looked again), and [Runner.loadCursor] re-derives it from the row.
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
// REPORTED LATE, unlike a stop: a stop says this node cannot run these
// records, a fault that the broker or disk did not answer just now, which is
// not worth moving work for. Past the budget it takes a stop's path: reads
// refuse `stalled` naming it, and the node takes this domain's copy out of
// service. It clears once every failed part of the loop has succeeded again
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
// IT NEVER GOES BACK TO FALSE while the loop runs: a drained node now behind
// has still held the whole state once, which a snapshot gate asks about; the
// DISTANCE is [Health.Lag]'s question.
func (r *Runner) Drained() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.drained
}

// markDrained records that the broker answered this loop's fetch with nothing
// while it held nothing, which is this node level with the log, and tells
// [RunnerDeps.OnDrained] the first time. The hook runs AFTER THE LATCH IS SET
// AND THE LOCK RELEASED, so a snapshot gate it wakes reads [Runner.Drained]
// true.
func (r *Runner) markDrained() {
	r.mu.Lock()
	first := !r.drained
	r.drained = true
	r.mu.Unlock()
	if first && r.onDrained != nil {
		r.onDrained()
	}
}

// markLevel records that the broker answered this loop's fetch with nothing
// while it held nothing, and tells [RunnerDeps.OnLevel] when that is news:
// the first such answer since the loop started, fetched records or failed.
// After [Runner.markDrained], so a reader the hook wakes sees the latch set.
func (r *Runner) markLevel() {
	r.mu.Lock()
	news := !r.level
	r.level = true
	r.mu.Unlock()
	if news && r.onLevel != nil {
		r.onLevel()
	}
}

// unlevel records that the loop is no longer level with its log as far as it
// knows — it fetched records — so the next empty fetch is news again.
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
// ([Runner.Reached]): every record through p that writes a row committed, and
// every barrier through it decoded — never a barrier's own commit, which
// changes no row.
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
// THE WAIT IS ON THE REACHED POSITION, never the coverage: coverage is DECIDED
// (a deferred record covers these objects or not), and where one does, the
// covered position is fixed at or below the checkpoint, so a wait on it never
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
// THIRTY DAYS, sized for the slowest client that retries: not the machine's
// sixteen rounds in five seconds ([DefaultResolveBudget]) but a seat carrying
// an op id to its next wake, after a weekend for a scheduled one. An op id
// that outlives its row resolves `unknown`, which re-decides work, never a
// second copy: the publisher reads each sweep's cutoff before trusting the
// ledger's silence ([Rows.LostBefore]). About 29 MB steady at the census rate
// (6 518 commits a day, ~150 bytes a row).
const OpsRetention = 30 * 24 * time.Hour

// PurgeOps deletes this node's operation rows applied before cutoff.
//
// PER NODE, because the table is one this node owns a copy of: it records
// which operations THIS applier wrote, so a fleet-singleton sweep would leave
// every other node's rows growing for ever while reporting that it had swept.
func (r *Runner) PurgeOps(ctx context.Context, cutoff time.Time) (int64, error) {
	return r.tables.purgeOps(ctx, r.db, cutoff)
}

// PurgeOpsOfKind deletes this node's operation rows on one subject kind applied
// before cutoff, for a kind whose writers never re-ask late: [OpsRetention] is
// sized for a seat re-asking after a weekend, and a kind re-asked only inside
// the publisher's resolve budget would hold a month of rows nobody looks up. A
// domain declares the kind's horizon at registration and ships the
// `(subject, applied_at)` index this seeks on.
func (r *Runner) PurgeOpsOfKind(ctx context.Context, kind string,
	cutoff time.Time) (int64, error) {

	return r.tables.purgeOpsOfKind(ctx, r.db, kind, cutoff)
}

// Op answers where an operation was applied on this node — the same ledger
// read [Rows.Op] makes, for a caller holding the runner rather than the rows.
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
// the file and starts the same Runner again from the new file's checkpoint,
// with its retained records reprocessed. A previous run's stop, fault,
// recreation and passed-generation verdicts are re-evaluated against the rows
// it loads, never remembered ([Runner.RecreationStale] covers the one case
// that judgement cannot reach alone).
func (r *Runner) Run(ctx context.Context) error {
	r.mu.Lock()
	r.stopped, r.fault, r.faultSince, r.faultReported = nil, nil, time.Time{}, false
	// AND THE DRAIN LATCH, for the reason its own field states: a restart
	// here is an adoption or a re-anchor, and a drain of the history this
	// loop used to apply is not a claim about the one it is about to.
	r.drained, r.level = false, false
	r.mu.Unlock()

	var w *store.Writer
	// THE ONE CHECKPOINT TRANSACTION BESIDE THE LOOP, on the writer below —
	// see [checkpoints].
	beside := &checkpoints{r: r}
	defer func() {
		// THE TRANSACTION BESIDE THE LOOP ENDS FIRST: it writes on the
		// connection closed next, and a commit closed under it is a
		// commit nobody can say landed.
		beside.stop()
		if w != nil {
			_ = w.Close()
		}
		// A WAITER LEFT ON A STOPPED APPLIER would wait out its whole
		// budget for a position nothing will ever reach — and one merely
		// RELEASED would read as satisfied and go on to read rows that
		// never reached its position. It is told, instead.
		r.waiters.abandonAll()
	}()

	// THE LOOP OUTLIVES A FAILURE: an unanswered fetch, a refused transaction
	// or an applier's error on a record says nothing about whether this node
	// can run these records, and a loop that returned left the domain dead for
	// the process's life. A failure that is not a STOP is retried in place, on
	// a pause doubling to a ceiling, with the same run re-applied rather than
	// left to redelivery. [Runner.Fault] reports it past the budget.
	var tail []Record
	var buffer reorderBuffer
	var pause time.Duration
	// TRUSTED is the doubt ([Runner.doubts]) everything this loop holds was
	// delivered under. Each step below that can wait compares it with the
	// runner's count after, and a doubt recorded meanwhile sends what it holds
	// back to the broker through the unwind at the top.
	var trusted uint64
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		// STARTUP IS INSIDE THE RETRY REGIME: pinning the connection, reading the
		// checkpoint and reprocessing are database work, and a store momentarily
		// unavailable (an adoption's close-to-reopen, a refused transaction) would
		// otherwise leave the domain with no applier, no stop and no fault,
		// publishing a caught-up position for ever.
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
		// WHAT THE TRANSACTION BESIDE THE LOOP DID since the loop last looked. A
		// FAILURE goes through this loop's regime: its records come back into the
		// loop's hands to be applied again after the pause. A COMMIT ends only
		// THAT transaction's failure (on a log of nothing but barriers no
		// transaction of the loop's own runs to clear it): what it committed may
		// predate a failed fetch, so the loop's own failures and their back-off
		// stand until a success of the loop's own.
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
		// A DOUBT SINCE THIS LOOP LAST TRUSTED WHAT IT HOLDS: everything in its
		// hands, the reorder buffer and the transaction beside it goes back to the
		// broker. Kept, it would be applied past a verification that compared only
		// the checkpoint, and deduplicated against the other history's records at
		// the same sequences, mixing two histories in one copy undetectably. Given
		// back, the broker redelivers its own records, and the verification
		// compares the checkpoint the transaction beside the loop left
		// ([Runner.unwind]).
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
			// A FETCH THAT FAILED may be a broker restored under this node, so what
			// the loop holds goes back ([Runner.doubtLocked]), with the run nextRun
			// hands back, and what follows the checkpoint is verified first
			// ([Runner.verifyBeforeApplying]).
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
		// THE BROKER ANSWERED A FETCH (empty after a failed one, which the unwind
		// gave back): the retry a failed fetch waited for, whatever the run holds,
		// since a run of barriers alone goes beside the loop and no transaction of
		// its own would end it.
		if r.recoveredOn(ctx, sideFetch) {
			pause = 0
		}
		if r.doubted() != trusted {
			// A DOUBT WHILE THE RUN WAS FETCHED: it is given back with the
			// rest, at the top.
			tail = run
			continue
		}
		if len(run) == 0 {
			// THE BROKER ANSWERED WITH NOTHING: whatever was wrong is not now, and
			// with nothing pending or in hand this node's rows are the WHOLE state,
			// so the drain latch is set and the loop is level ([RunnerDeps.OnLevel]),
			// unless the reorder buffer holds records above a hole.
			//
			// UNLESS THE TRANSACTION BESIDE THE LOOP HOLDS RECORDS: delivered and
			// unacknowledged, they may hold the broker back ([FetchMessages]), and a
			// fault it is retrying is cleared by its next commit, above.
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
			// THE SAME RUN, for the reason a failed commit keeps it.
			tail = run
			pause = r.backOff(ctx, pause)
			continue
		}
		if r.verifiedOn(ctx) {
			pause = 0
		}
		if r.doubted() != trusted {
			// A DOUBT WHILE THE CHECKPOINT WAS VERIFIED, which settled
			// nothing for it: the run is given back, and the next one
			// verified under the doubt it is delivered after.
			tail = run
			continue
		}
		// THE RUN IS REACHED as far as it carries barriers next in the log,
		// now that what follows the checkpoint has been verified to be this
		// node's history — the step [Runner.nextRun] skips while a
		// verification is owed.
		r.reach(run)
		if r.reachedThrough(run) {
			// A RUN WHOSE WHOLE TRANSACTION IS THE CHECKPOINT commits beside the
			// loop, so its fsync never stands before the decode of the records
			// behind it ([checkpoints]). Its waiters were released when reached; it
			// is acknowledged when it commits. It DRAINS THE LOG as a run committed
			// whole does when the broker had nothing more: every record held is
			// committed or writes no row.
			beside.offer(ctx, w, run)
			tail = nil
			if level {
				r.markDrained()
			}
			continue
		}
		// A TRANSACTION OF THE LOOP'S OWN, which waits for the one beside
		// it — one writer at a time on the connection — and carries every
		// barrier it had not yet taken, all of them below this run.
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
			// A DOUBT WHILE THE LOOP WAITED for the transaction beside it,
			// which can be a stalled fsync's seconds: the run and what that
			// transaction had not taken are given back with the rest.
			tail = run
			continue
		}
		consumed, err := r.applyRun(ctx, w, run)
		if err != nil {
			if stopped := r.faulted(ctx, sideApply, err); stopped != nil {
				return stopped
			}
			// THE SAME RUN, AGAIN: its records are delivered, unacknowledged and
			// contiguous from the checkpoint; dropped, the loop would fetch above a
			// hole for the whole ack window.
			tail = run
			pause = r.backOff(ctx, pause)
			continue
		}
		r.recovered(ctx)
		pause = 0
		tail = run[len(consumed):]
		if level && len(tail) == 0 {
			// THE OTHER DRAIN MOMENT, and on a busy log the only one: a run closed
			// because the broker had nothing more, committed whole, leaves nothing
			// pending or in hand. Latched only on an empty fetch, a node on a log
			// that gains a record more often than [FetchWait] would never drain, and
			// never serve as a snapshot's donor.
			r.markDrained()
		}
	}
}

// startup is everything the loop needs before it may consume a record: a
// pinned connection, the checkpoint, and whatever an earlier build retained.
// IT CLEANS UP AFTER ITSELF, being RETRIED: a connection leaked per failed
// attempt would exhaust the small pool and make a transient failure permanent.
func (r *Runner) startup(ctx context.Context) (*store.Writer, error) {
	// A PINNED CONNECTION for the loop's life: every reader draws from the
	// small pool, usually waiting on what this writer is about to commit, and
	// under load would hold every connection while the writer queues.
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
	// THE DOUBT THE LOAD RECORDED is the only one the comparison below settles
	// ([Runner.settledLocked]): the heartbeat may read the end below the
	// checkpoint after the comparison read its record (a broker restored that
	// moment), and that doubt must still be looked at.
	doubts := r.doubted()
	// AND THE RECORD THE CHECKPOINT NAMES, at once: a log written EXACTLY to
	// this checkpoint delivers nothing past it, leaving a diverged node serving
	// until the heartbeat looked. A log that does not reach the checkpoint
	// leaves the verification owed ([Runner.verifyBeforeApplying]). A
	// divergence recorded about this checkpoint is recalled first, since the
	// log may have lost the record it was found by.
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
	// THE ROWS ARE BROUGHT TO THIS BUILD'S DERIVATION RULES before anything is
	// applied, retained records included: a reprocess maintains derived columns
	// incrementally, and an increment on a column the old rules wrote mixes
	// both. Here because a build changes, and an adopted artefact arrives, only
	// at a boot. AFTER the checkpoint's record is verified, so a diverged node
	// stops with its rows as found.
	if err := r.rederive(ctx, w); err != nil {
		_ = w.Close()
		return nil, err
	}
	// WHAT AN EARLIER BUILD RETAINED, THIS ONE MAY NOW READ. A retained
	// record is applied by the build that can decode it, and the only
	// moment a build changes is a boot — so this is where the promise is
	// kept, before the loop consumes anything above it.
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
// A run remembers WHICH parts failed because only a success of the part that
// failed ends it, and the parts succeed apart: a transaction beside the loop
// commits barriers handed over before a fetch failed, while that fetch is
// still unretried. Ended by any success, the run would log a false
// `statelog_apply_recovered`, reset the fault's clock and back-off, and start
// [Runner.Fault] late.
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
// the first retry past [ApplyRetryBudget], and `statelog_apply_recovered`
// with its duration when a retry succeeds. An ERROR per retry would count one
// incident as hundreds (so [Tracker] logs an alarm's transitions, not its
// level); recovering and failing again is a second run. The level lives in
// [Runner.Fault] and `crewlet.statelog.apply.retries`, and the ERROR lands on
// the retry that makes [Runner.Fault] start answering.
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

// recovered clears the fault a retry just outlived, and closes its run: the
// next failure starts a new one, written again from `statelog_apply_retrying`.
// It is the success that ends a run whatever failed in it — the loop's startup,
// a transaction of its own, a fetch answered with nothing while the loop holds
// nothing — and [Runner.recoveredOn] the one that ends only its own part.
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

// recoveredOn is a success of side alone: it ends side's part of the current
// run, and the run once no part of it is left failing ([faultSide]). It answers
// whether no run is left standing, which is when the loop's back-off starts
// again from the beat.
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
// a verification that SETTLED, and only one. A run with nothing past the
// checkpoint passes without one ([Runner.verifyBeforeApplying]) and leaves the
// verification and its failure owed; taken for a success, it would log a false
// recovery and restart the fault's clock. It answers what [Runner.recoveredOn]
// does, and false while the verification is owed.
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
		// THE ABANDONED GENERATIONS TRAVEL WITH THE CHECKPOINT: this loop may be
		// resuming part-way through them, after a restart or over a peer's
		// snapshot.
		r.void = row.void
		if found {
			// THE CHECKPOINT AND THE RECORD IT NAMES, together — see
			// [Runner.checkpointAt].
			r.cursor, r.checkpointAt = at, row.storedAt
		}
		r.changed = r.cursor
		r.rows++
		// A RUN THAT STARTS OWES A VERIFICATION of that record before it applies
		// past it: a boot is when a node meets a broker restored from an older
		// copy ([Runner.verifyBeforeApplying]). AND THE REACHED POSITION GOES BACK
		// TO IT ([Runner.doubtLocked]): a barrier a previous run reached
		// uncommitted comes back, and an adoption may have replaced the rows it
		// was reached over.
		r.doubtLocked()
		// A VERDICT ABOUT A CHECKPOINT THIS RUNNER NO LONGER STANDS AT is dropped:
		// over an adopted snapshot, an end paired with the old checkpoint says
		// nothing about the donor's, and kept would refuse writes until the log
		// reached a number this node has left. The boot's own reading is paired
		// with this row, so it survives the first load.
		if r.ahead != nil && r.ahead.at != r.cursor {
			r.ahead = nil
		}
		r.deferred, r.hasDefer = d, hasDefer
		// A RECREATED STREAM IS DETECTED HERE AND IS A STOP: every stored sequence
		// names a dead number space, and a consumer resumed from the checkpoint
		// waits for ever reporting nothing pending, until an operator reanchors.
		// RECORDED as well as returned, because the publisher asks
		// [Runner.StreamIdentity], never the loop's error. Compared through
		// [IdentityOf] at the row's microseconds (the broker reports nanoseconds),
		// against the live instant this runner knows best: a recreation verdict's,
		// else the one it was built with.
		//
		// RE-DERIVED against the row, never remembered: an adoption may have
		// replaced it without the re-key ([Runner.Rejoined]), and an instant is
		// never reissued, so a row keyed to the live stream clears it.
		live := r.created
		if r.foreign != nil {
			live = r.foreign.live
		}
		switch {
		case live.IsZero():
		case !found:
			// NO ROW TO JUDGE BY: nothing here says which stream the
			// positions in memory belong to, so a verdict that holds
			// stands and none is reached.
		case IdentityOf(created, live, true) == StreamRecreated:
			// A NEW VALUE, never a write through the shared pointer
			// ([Runner.StreamIdentity] reads it unlocked), keyed to the ROW'S
			// instant, which [Runner.KeyedTo] publishes and
			// [Runner.RecreationStale] judges by.
			r.foreign = &foreignStream{keyed: created, live: live}
		default:
			r.created, r.foreign = live, nil
		}
		if r.foreign != nil {
			return fmt.Errorf("%w: %w", ErrStopped,
				r.foreign.err(r.domain.Name(), r.spec.Name))
		}
		// SO DOES A GENERATION THE FLEET HAS MOVED PAST: a re-run from these rows
		// would apply the new generation's records into them. A running loop is
		// stopped by [Runner.decode] instead.
		if r.passed != nil && found && at.Generation >= r.passed.fleet {
			r.passed = nil
		}
		if r.passed != nil {
			return fmt.Errorf("%w: %w", ErrStopped,
				r.passed.err(r.domain.Name(), r.spec.Name))
		}
		// AND A LOG THAT DIVERGED FROM THESE ROWS, judged by the record the row
		// names: the same checkpoint and record are the same rows, and anything
		// else is a file an adoption replaced.
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

// ReprocessPage bounds how many retained records one read of the table takes,
// so a node that sat out a long upgrade walks its backlog in pages rather than
// loading it whole.
const ReprocessPage = 256

// reprocess applies every retained record this build can now decode, in
// position order, and releases each one in the transaction that applied it.
//
// THE LIVE LOOP'S RULE, REPLAYED: a readable record stays retained while an
// EARLIER retained record covers its scope, since applying it first produces
// state no other node holds; walked oldest first, releasing as it goes, the
// below-position probe is the live check. A record still above this build's
// version stays, with everything it covers.
//
// Neither the checkpoint nor the anchor moves: the log consumed the record
// when it was retained, and the anchor is a MAX. One transaction per record,
// holding the writer as long as its ordinary apply would.
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
			// RE-VERIFIED, NEVER TRUSTED BECAUSE IT WAS ONCE FILED: the table
			// holds the bytes as published, so a record retained for an unheld
			// key stays retained until the key arrives, and one applied is
			// authenticated by this build.
			body, verdict := r.verifier.Open(row.payload)
			at := Position{
				Stream:     r.spec.Name,
				Generation: uint32(row.position / GenerationStride),
				Seq:        uint64(row.position % GenerationStride),
			}
			switch verdict {
			case recordseal.KeyUnknown:
				// STILL UNREADABLE, and said once per key for this
				// process: after a restart this is the only place
				// the node meets the record again.
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
				// THROUGH [AppliedInstant] ON THIS PATH TOO, though the deferred
				// table's reader already decodes the store's encoding: both paths
				// hand an applier one value by construction.
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
				// A GATE DROPPED IT, which the retaining build could not ask: no
				// row written, so it counts as the live loop's `gated`, beside the
				// gate's own counter, now its transaction has committed.
				r.gatedRecord(ctx, rec, result.reason)
				gated++
			default:
				applied++
				// A trimmed record is version 1 and declares the
				// framework's scope, so nothing retains one — but a
				// removal is counted where its transaction committed,
				// whichever path committed it.
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
// still covers it, reporting whether it landed — released from the retained
// table — and what applying it did: whether a gate dropped it rather than
// applied it, and which.
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
		// A RECORD THIS BUILD COULD NOT READ WHEN IT ARRIVED, now in the
		// rows — the one case the checkpoint's own write never sees, since a
		// reprocess does not move it.
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

// nextRun fills a run toward the transaction budget, starting from whatever
// the previous transaction did not consume: per fetch, the commit rate would
// follow the fetch size rather than the budget, which on a barrier-heavy
// stream is one fsync per two dozen records. It REACHES WHAT IT ADMITS
// ([Runner.reach]) as it fills, unless a verification is owed.
//
// LEVEL says the run, committed whole, leaves this node holding everything the
// log held when it closed (the broker had nothing more, and the reorder buffer
// nothing above a hole); it latches [Runner.Drained]. A run closed for any
// other reason says nothing either way. An error comes back with
// the run filled so far, so the loop holds every record until it consumes it
// or gives it back.
func (r *Runner) nextRun(ctx context.Context, tail []Record, buffer *reorderBuffer) (run []Record, level bool, err error) {
	run = tail
	// WHAT THE LAST TRANSACTION LEFT is reached first: a run its budget
	// ended leaves the barriers past it in hand, and their readers waiting.
	r.reach(run)
	for {
		if len(run) > 0 {
			pending, perr := r.fetch.Pending(ctx)
			if perr != nil || pending == 0 {
				// Pending is a HINT: its failure closes the run, not the applier,
				// since what the run holds is safe to commit. A broker that cannot
				// answer fails the next Fetch, which is the call that reports.
				return run, perr == nil && len(buffer.held) == 0, nil
			}
			if r.budgetWouldBind(run) {
				return run, false, nil
			}
		}
		wait := FetchWait
		if len(run) == 0 && r.waiters.len() > 0 {
			// SOMEBODY IS ALREADY WAITING: collect what is there rather than
			// hold the batch open; on a quiet log this wait is the whole latency
			// a linearizable read pays.
			wait = ApplyLinger
		}
		if len(run) > 0 {
			// A PARTIAL RUN LINGERS, but yields the instant somebody waits on a
			// record it holds: a read waiting for the record before its barrier,
			// or a write for its own, would otherwise pay the whole linger, on an
			// idle company for every record.
			wait = ApplyLinger
			if target, waiting := r.waiters.minimum(); waiting && r.have(run, target) {
				r.count(metrics.StatelogLingerYields)
				return run, false, nil
			}
		}
		batch, err := r.fetch.Fetch(ctx, FetchMessages, FetchBytes, wait)
		if err != nil {
			// THE RUN FILLED SO FAR comes back with the error, for the
			// loop to give back to the broker ([Runner.Run]): dropped here,
			// what an earlier fetch of this run delivered was neither
			// acknowledged nor handed back, a hole for an ack window.
			if ctx.Err() != nil {
				return run, false, ctx.Err()
			}
			return run, false, fmt.Errorf("statelog: fetch from %s: %w", r.spec.Name, err)
		}
		records, err := r.decode(ctx, batch)
		if err != nil {
			return run, false, err
		}
		// CONTIGUOUS FROM THE REACHED POSITION, not the checkpoint: every record
		// between the two is already in this loop's hands, so a redelivery of
		// one is passed through to be acknowledged.
		ready, err := buffer.admit(records, r.Reached(), r.spec.Replay, run)
		if err != nil {
			return run, false, r.stop(ctx, err)
		}
		run = append(run, ready...)
		r.reach(run)
		if len(batch) == 0 {
			if len(run) == 0 {
				// THE BROKER ANSWERED, WITH NOTHING — level, unless the
				// buffer holds records above a hole: those are records
				// this node has not applied, waiting for one the broker
				// will deliver again.
				return nil, len(buffer.held) == 0, nil
			}
			// THE LINGER EXPIRED WITH RECORDS IN HAND, so the run commits. An
			// empty pull while records are pending is the broker WITHHOLDING
			// them: this run fills the consumer's in-flight ceiling, freed only
			// by acknowledging after this commit, so pulling on would wait for
			// ever.
			return run, false, nil
		}
	}
}

// have reports whether this run already carries the position somebody is
// waiting for.
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
// A ROW ESTIMATE, deliberately: the exact count is only known inside the
// transaction, and this is the question of whether to fetch MORE rather than
// of when to stop applying. The estimate is one row per record, which is a
// floor — so it never stops a fetch that would have fitted, and the
// transaction's own budget is what actually ends a batch.
func (r *Runner) budgetWouldBind(run []Record) bool {
	return len(run) >= ApplyTxRowBudget
}

// decode turns broker messages into records, with the envelope the domain can
// always read.
func (r *Runner) decode(ctx context.Context, batch []Message) ([]Record, error) {
	out := make([]Record, 0, len(batch))
	// THE CHECKPOINT'S GENERATION, read once per batch: the loop is the one
	// writer of the cursor and nothing moves its generation while it runs.
	// And whether the fleet is already known to have left it, which the
	// heartbeat can establish between two batches.
	r.mu.Lock()
	gen, passed := r.cursor.Generation, r.passed != nil
	r.mu.Unlock()
	for _, m := range batch {
		// VERIFIED BEFORE THE DOMAIN SEES IT: the broker vouches for a
		// connection, never a record, and a domain's decoder is where
		// attacker-controlled bytes get parsed.
		body, verdict := r.verifier.Open(m.Payload)
		if verdict != recordseal.Verified {
			// OUTSIDE THE TRANSACTION, where the refusal is decided:
			// the retention that follows for an unknown key runs in a
			// body the store may run twice.
			r.witness(ctx, verdict, m.Payload,
				Position{Stream: r.spec.Name, Generation: gen, Seq: m.Seq})
		}
		if verdict == recordseal.Tampered {
			// PERMANENT, and it stops the loop. A record whose MAC
			// fails under a key this fleet holds was written by
			// something that is not this fleet; applying past it
			// would be applying whatever it was written to precede.
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
			// AN UNREADABLE ENVELOPE IS A STOP: every build reads the envelope, so
			// the record is not this domain's. Retaining it would index nothing
			// (the index's fields are in the envelope), and a retry reads the same
			// bytes the same way.
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
		// A RECORD FROM A GENERATION THIS APPLIER NEVER ENTERED IS A STOP on a
		// domain that claims identity: a peer re-anchored and this node did not,
		// and applying what follows would mix two histories. The reanchor's own
		// record is the new generation's first, so this sees the move at once.
		// ANY RECORD stops too once the move is known (the heartbeat may learn it
		// first: a node past a restored end skips that record, and a record need
		// not carry its writer's generation); a re-run stops in
		// [Runner.loadCursor]. The node adopts the peer's snapshot; the verdict
		// refuses its reads and writes until then.
		if passed || (env.Gen > gen && r.domain.ClaimsIdentity()) {
			// WHO HOLDS IT decides the remedy the stop names, and it is asked
			// before the stop is written, which is the one line an operator
			// is sure to read.
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
			// THE BODY TO THE APPLIER, THE FRAME TO THE TABLE: a retained record
			// is re-verified by the build that can finally read it.
			Payload: body,
			framed:  m.Payload,
			// THE INSTANT AS THE STORE KEEPS IT ([AppliedInstant]): the
			// broker's own, in the node's zone at nanoseconds, is a value
			// no record held back and applied later carries.
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
	// changed is the position of the last record this transaction changed a
	// row with ([Runner.Changed]).
	var changed Position
	var tally results
	var rows int
	var boundBy string
	var notes []applyNote
	started := r.now()

	// THE ABORTS ARE COUNTED HERE, the only place that can:
	// `crewlet.statelog.apply.tx.aborts` answers whether an apply's body is
	// ever run twice, which fourteen of this design's throughput figures
	// assume it is not. It READS ZERO BY CONSTRUCTION: internal/store takes the
	// write lock at BEGIN, so no other commit can abort an apply, and a count
	// means a TRANSIENT FAILURE inside a body the store ran again. The store
	// cannot count it (it may not import metrics), but it re-runs the body, so
	// this closure's invocations minus one are the number.
	attempts := 0
	err := w.Tx(ctx, func(tx *sql.Tx) error {
		attempts++
		// RESET ON EVERY ATTEMPT: the store re-runs a transiently failed body, and
		// accumulated counters would report rolled-back work. The NOTES too: a
		// note is said after the commit, and one kept from an abandoned attempt
		// would announce a retention that may not have committed.
		consumed, committedAt, tally, rows, boundBy = consumed[:0], Position{}, results{}, 0, ""
		notes = notes[:0]
		committedRecord, changed = time.Time{}, Position{}
		txStart := r.now()

		hasDeferred, err := r.anyDeferred(ctx, tx)
		if err != nil {
			return err
		}
		// THE HIGH-WATER GUARD, the checkpoint's rule applied WITHIN the
		// transaction: a duplicate above the committed checkpoint but at or below
		// what this transaction already applied is skipped too, never applied
		// against rows its first copy wrote.
		highWater := r.Committed()
		opts := r.batchOptions(txStart)

		for i := range run {
			rec := run[i]
			if rec.Position.Packed() <= highWater.Packed() {
				// AT OR BELOW THE CHECKPOINT: consumed, never applied, and
				// acknowledged, or the broker redelivers it for ever.
				consumed = append(consumed, rec)
				tally.skipped++
				continue
			}
			if rec.Kind == BarrierKind {
				tally.barrier(rec.StoredAt, txStart)
			}
			// THE FRAMEWORK'S OWN WRITE FIRST, from the
			// always-decodable envelope, because it happens
			// whatever the record then does.
			if arbitrates(r.spec.ArbitratedKinds, rec.Subject.Kind) {
				if err := r.tables.advanceAnchor(ctx, tx, r.tables.subjectOf(rec.Subject), rec.Position); err != nil {
					return err
				}
			}

			// WHETHER THIS RECORD CHANGES A ROW ([Runner.Changed]): every
			// record does but a barrier applied here — one retained below,
			// under a key this node lacks or behind a retained record's
			// scope, is a row of the deferral index.
			wrote := true
			switch {
			case rec.verdict == recordseal.KeyUnknown:
				// THE SAME DISPOSITION AS A RECORD FROM A NEWER BUILD: both mean "this
				// build cannot read this yet" until an upgrade or the key arrives.
				// Retained under its own scope, so later records on the same objects
				// wait behind it.
				//
				// A GATE IS STILL A STOP, as in the version arm: a deferred gate
				// licenses every record above it.
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
					// deferred by the node it evicts leaves that node passing every
					// fence it has.
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
					// A RECORD ON STALE ROWS IS RETAINED TOO: applied on rows a
					// deferred record never wrote, it makes state no other node
					// holds, undetectably.
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
		// [reorderBuffer.admit] passes a late redelivery below the high-water mark
		// through to be acknowledged, so a run can close as [11, 12, 11].
		// Checkpointing the tail writes 11 beside 12's rows, a cursor that
		// UNDERSTATES its database: a read waiting for 12 is refused `behind`, and
		// the next boot re-applies 12. The floor is the committed cursor, so a run
		// of redeliveries is acknowledged without moving it.
		before := r.Committed()
		committedAt = highest(consumed, before)
		// AND THE RECORD IT NAMES: the one consumed at that position, or for a run
		// of redeliveries the one already named ([Runner.checkpointAt]). NEVER A
		// REDELIVERY'S OWN: a restored consumer redelivers the log's record,
		// whichever history it is, and naming the checkpoint by it would adopt
		// that history as this node's.
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
	// WHAT THE COMMITTED ATTEMPT SAW, first and once ([applyNote]), before the
	// checkpoint moves, so a caller that sees this node past a gated record
	// sees its count too ([Runner.gatedRecord]).
	r.announce(ctx, notes)

	// THE POSITION THE TRANSACTION COMMITTED, so what this node publishes and
	// resumes from is the checkpoint row's value.
	at := committedAt
	if tally.retained > 0 {
		// WHAT THIS NODE CANNOT READ is what its readiness and its
		// coverage both turn on, so it is refreshed the moment it
		// changes rather than at the next boot.
		if err := r.refreshDeferred(ctx); err != nil {
			return nil, err
		}
	}
	// AND EVERY WAITER AT OR BELOW THE REACHED POSITION THE COMMIT LEAVES,
	// which is never below it: a commit reaches every record it covers, and
	// barriers already reached past it stay reached.
	r.waiters.release(r.advance(at, committedRecord, changed))
	if tally.retained > 0 {
		// WHAT THE BATCH RETAINED, which no Apply call saw — see
		// [RetentionHook]. AFTER the deferral index is refreshed above,
		// so a listener that reads what this node can vouch for reads
		// the deferral this batch committed.
		if hook, ok := r.applier.(RetentionHook); ok {
			hook.Retained(ctx, tally.retained)
		}
	}
	r.applier.Committed(ctx)
	r.ack(ctx, consumed)

	r.measureDrain(started, len(consumed))
	r.countAborts(attempts)
	// THE TOP RECORD, for the same reason: latency is measured from a record's
	// StoredAt, and a stale redelivery's would report its own age.
	r.observe(started, rows, boundBy, tally, topRecord(consumed))
	return consumed, nil
}

// measureDrain folds one batch's rate into the smoothed estimate.
//
// A BATCH THAT TOOK NO MEASURABLE TIME IS SKIPPED rather than treated as
// infinitely fast: the clock's resolution is not a rate, and one such batch
// would push the estimate to a number that makes every lag read as zero.
func (r *Runner) measureDrain(started time.Time, records int) {
	elapsed := r.now().Sub(started)
	if records <= 0 || elapsed <= 0 {
		return
	}
	rate := float64(records) / elapsed.Seconds()
	// ONE COMMIT PER RUN, which is what [Runner.applyRun] is: the whole
	// batch lands in a single transaction, so this batch's contribution to
	// the commit rate is one over the same elapsed time.
	commits := 1 / elapsed.Seconds()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.drain == 0 {
		// THE FIRST BATCH IS THE ESTIMATE: smoothing from zero would understate
		// the rate for several batches right after a restart, when the backlog
		// is longest.
		r.drain, r.commits = rate, commits
		return
	}
	r.drain += DrainSmoothing * (rate - r.drain)
	r.commits += DrainSmoothing * (commits - r.commits)
}

// Drain is this loop's measured records per second, and 0 before it has
// applied anything. ZERO MEANS UNMEASURED: every caller falls back to a floor
// rather than divide by it.
func (r *Runner) Drain() float64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.drain
}

// Commits is this loop's measured transactions per second, and 0 (unmeasured)
// before it has applied anything. Published for an operator to compare with
// the device's write rate; nothing divides by it.
func (r *Runner) Commits() float64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.commits
}

// applyResult is what applying one record did, for the caller to count once
// its transaction commits — never from inside it, for [Runner.gatedRecord]'s
// reason.
type applyResult struct {
	// rows is every row the record wrote or removed: the input the
	// transaction's row budget is held to.
	rows int

	// swept is how many anchors a trimmed record removed (ADR-0080).
	swept int

	// gated is whether a gate dropped the record, and reason which.
	gated  bool
	reason Reason
}

// applyOne runs the domain's state machine for one record, unless a gate says
// it must produce no rows — reporting which gate, and whether one did, and
// saying NOTHING: it runs inside a transaction body the store may run again,
// so the caller notes the drop ([applyNote], or the reprocess's own answer)
// and it is reported once the transaction commits ([Runner.gatedRecord]).
func (r *Runner) applyOne(ctx context.Context, tx *sql.Tx, rec Record, opts ApplyOptions) (applyResult, error) {
	started := r.now()
	opts.StoredAt = rec.StoredAt
	// THE FRAMEWORK'S OWN GATES FIRST, both the checkpoint's rules: a record of
	// a generation the reanchor that placed this checkpoint abandoned, and one
	// a restored reanchor overtook. Each is asked of the record's OWN
	// generation (the writer's stamp), never the loop's composed position,
	// which carries this checkpoint's generation for every record.
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
		// THE FRAMEWORK'S OWN EFFECT OF A TRIMMED RECORD (ADR-0080), in its own
		// transaction once the gates say it applies. AFTER the domain's apply, so
		// a domain that forgot the kind faults rather than having anchors removed
		// by a record it cannot name. A FLOOR THAT DOES NOT PARSE IS A FAULT: the
		// one writer ([PlanSweep]) checks every body, so such a record came from
		// somewhere this build cannot explain.
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
		// ONE RECORD'S APPLY, the real ceiling on how long a read can be delayed
		// (one record past the time budget is still one transaction). A TIMING,
		// so it stays inside the body: a rolled-back attempt still held the
		// applier. Every COUNT moves out, which an abandoned attempt would state
		// twice.
		r.metrics.Observe(metrics.StatelogApplyRecordDuration, r.now().Sub(started),
			metrics.Attrs{"domain": r.domain.Name(), "kind": rec.Kind})
	}
	return out, nil
}

// applyNote is one thing an apply transaction observed about one record that
// is SAID rather than written: a warning line, and for two of the three kinds
// a counter an alarm reads.
//
// A VALUE, NOT A CALL: the store re-runs a transiently failed body
// ([store.Writer.Tx]) and rolls back its rows, but not a log line or counter,
// so a record retained on an abandoned attempt would be announced, and
// counted by the alarms, twice. The body notes, resets its notes on every
// attempt, and [Runner.announce] says them ONCE after the commit.
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
// at sequence seq under a rule the reanchor that placed its checkpoint made — a
// generation it abandoned, or one a restored reanchor overtook — and which. The
// rules applyOne asks, read from the checkpoint they travel on ([Voids]).
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

// gatedRecord says, where an operator looks, that a durable record applies
// nowhere, and under which gate. Such a record still advanced the anchor and
// the checkpoint (skipping it would replay it for ever) but leaves no trace in
// the rows, so the line and the counter are its only witnesses.
//
// ONLY ONCE THE DROP HAS COMMITTED: the store re-runs a body after a transient
// failure ([store.Writer.Tx]), and a report from inside would count one drop
// per attempt on the counter the records_gated alarm reads. The transaction
// notes it ([applyNote], or the reprocess's own answer).
func (r *Runner) gatedRecord(ctx context.Context, rec Record, reason Reason) {
	r.logger.WarnContext(ctx, "statelog_record_gated",
		"domain", r.domain.Name(), "position", rec.Position.String(),
		"kind", rec.Kind, "gate", string(reason), "writer", rec.Writer)
	r.countWith(metrics.StatelogRecordsGated, metrics.Attrs{
		"gate": string(reason), "subject_kind": rec.Subject.Kind,
	})
}

// anyDeferred reports whether this node holds any record it cannot decode, so
// the per-record probe is skipped entirely on the ordinary path.
//
// AN EXISTENCE PROBE rather than [tables.oldestDeferred], which reads the
// record's scope as well: this runs at the top of every apply transaction, and
// the scope is nothing it needs.
func (r *Runner) anyDeferred(ctx context.Context, tx *sql.Tx) (bool, error) {
	return r.tables.holdsDeferred(ctx, tx)
}

// ack acknowledges every record this transaction consumed, BY IDENTITY, not
// count: a run mixes applied, retained, gated and below-checkpoint records,
// and acknowledging "as many as were applied" acknowledges the wrong ones,
// leaving a retained record's delivery redelivered for ever.
//
// NEVER A NAK: a record this node cannot apply is retained, stopped on or
// already committed, and a nak would hand it straight back to this node.
func (r *Runner) ack(ctx context.Context, consumed []Record) {
	for _, rec := range consumed {
		if rec.ack == nil {
			continue
		}
		if err := rec.ack(); err != nil {
			// A FAILED ACKNOWLEDGEMENT IS A REDELIVERY, which the
			// checkpoint drops. Worth a line and not worth stopping
			// for.
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
// position a transaction committed, the reached position with it and the
// position its rows last changed at ([Runner.Changed]) to changed, and answers
// the reached position it leaves.
func (r *Runner) advance(at Position, record time.Time, changed Position) Position {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.changed.Packed() < changed.Packed() {
		r.changed = changed
	}
	if r.cursor.Packed() < at.Packed() {
		// THE PAIR MOVES TOGETHER — see [Runner.checkpointAt].
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
// HEALTH GOES FALSE IMMEDIATELY, with no grace: "this node cannot run this
// company's records" is worth moving work for, unlike "briefly behind". Every
// stop comes through here, so this is the ONE line a stop writes, saying
// which log, where it froze, why and what resumes it.
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
// outcome: a node whose position advances while it applies NOTHING (every
// record retained) is healthy on lag and useless in fact.
type results struct {
	applied  int
	retained int
	skipped  int

	// gated is how many records a gate dropped in this attempt, each noted
	// beside it ([applyNote]) and reported once the transaction commits
	// ([Runner.gatedRecord]).
	gated int

	// version is the highest record version of a record this transaction
	// APPLIED — what the checkpoint row's applied record version is raised
	// to ([tables.setCursor]) — zero where it applied none.
	version int

	// swept is how many anchors this transaction's trimmed records removed
	// (ADR-0080).
	swept int

	// barriers is how many of the records this batch consumed for the
	// first time were barriers, whatever became of them — each is a record
	// on the log, which is what the census counts — BY THE HOUR THE BROKER
	// STORED EACH, as the start of that hour. Nil until the first.
	barriers map[time.Time]uint64
}

// barrier counts one barrier the broker stored at storedAt, or at applied (the
// transaction's own instant) for a record that carries no broker instant.
//
// BY ITS COMMIT HOUR, never the hour it is applied in: the census reads these
// as the reads the log took in the last day, and a node replaying days of
// backlog in minutes would otherwise read as a company far past its census.
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
// ATTEMPTS MINUS ONE, because the last one is the one that committed. Zero on
// the ordinary path, which is why it is a counter rather than a gauge: what an
// operator watches is whether it moves at all.
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
	// THE LOG'S READ RATE as this node applies it: counted after the commit,
	// so a re-run attempt counts once, filed at each barrier's COMMIT hour
	// ([results.barrier]), and labelled with the STREAM an operator finds the
	// log by on the broker.
	for hour, n := range tally.barriers {
		r.metrics.AddAt(metrics.StatelogBarriersApplied, n, hour,
			metrics.Attrs{"domain": domain, "stream": r.spec.Name})
	}
	r.countSwept(tally.swept)
	// THE COMMIT-TO-APPLY GAP, from the BROKER's own timestamp rather
	// than from when this node fetched the record: every read level is a
	// policy about this quantity, and a node's own fetch time hides
	// exactly the delay the policy is about.
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

// countSwept counts the anchors trimmed records removed in a transaction that
// has COMMITTED — never from inside one, which the store may run again
// ([Runner.gatedRecord]).
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
