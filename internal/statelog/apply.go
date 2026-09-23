package statelog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/statelog/metrics"
	"github.com/crewlet/crewlet/internal/store"
)

// ReorderBufferBytes bounds what a strict loop holds while it waits for a
// missing sequence.
//
// A strict log is contiguous by construction, so an out-of-order arrival is a
// redelivery rather than a hole — and it resolves when the broker redelivers
// the missing record. The buffer is what carries the loop across that window,
// sized at two full fetches plus slack.
//
// OVERFLOW IS A FAULT, not a gap to step over. A strict domain that cannot
// close a hole has a stream that is not the stream it thinks it is, and
// applying past the hole would produce state no other node holds — which is
// the one failure a replicated log has no way to notice later.
const ReorderBufferBytes = 64 << 20

// Message is one record as the broker delivered it.
type Message struct {
	// Seq is the broker's sequence.
	Seq uint64

	// StoredAt is the broker's own timestamp, which every node reads
	// identically — unlike a clock each node would read for itself.
	StoredAt time.Time

	// Payload is the record's bytes.
	Payload []byte

	// Ack acknowledges this delivery. Called only AFTER the transaction
	// that consumed it commits, and never inside one.
	Ack func() error
}

// Fetcher delivers a domain's records in stream order.
//
// DECLARED HERE because the applier is the caller, and deliberately narrow:
// the loop needs records and it needs to know whether more are waiting. Every
// other property of a durable consumer — its name, its ack policy, its
// redelivery window — belongs to whoever creates it.
type Fetcher interface {
	// Fetch pulls up to maxMessages records or maxBytes of them,
	// whichever binds first, waiting up to wait for the first one.
	//
	// EVERYTHING THE BROKER DELIVERED IS RETURNED. An implementation may
	// not take a prefix of what a pull handed over and drop the rest: a
	// delivered record the loop never sees is one the broker holds
	// against the consumer's ack-pending cap and redelivers only after
	// its ack window — a hole in a strict log, on every pull, for as long
	// as the window is. Where the broker cannot bound a pull by both
	// count and bytes, the count is the consumer's own in-flight ceiling
	// (see [FetchMessages]) and maxMessages is honoured by that.
	Fetch(ctx context.Context, maxMessages, maxBytes int, wait time.Duration) ([]Message, error)

	// Pending is how many records this consumer has not yet delivered. It
	// is what tells a partially filled batch whether waiting would buy
	// anything.
	Pending(ctx context.Context) (uint64, error)
}

// CheckpointLog is the log as the applier reads it BY POSITION rather than by
// following it: its two ends, and the record at one sequence.
//
// DECLARED HERE because the applier is the caller, and it needs exactly one
// answer from it — whether the record the log holds at this node's checkpoint
// is the one the checkpoint names ([Runner.VerifyCheckpoint]). A consumer
// cannot answer that: it delivers what follows a position, never what is at
// it.
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

// Estate is the replicated database as the applier needs it, RESOLVED PER
// CALL rather than held.
//
// DECLARED HERE because the applier is the caller, and the shape is the
// point: an adoption replaces the replicated file underneath a running node —
// closed, renamed over, reopened — and every subsystem that captured the file's
// handle at boot would go on answering from a database that is no longer at
// that name. So nothing in this package holds one. What it holds is something
// that reaches the CURRENT estate on every read and every pin, and answers
// [store.ErrNoEstate] in the window where there is none.
type Estate interface {
	// Read runs fn in a read transaction on the current estate.
	Read(ctx context.Context, fn func(*sql.Tx) error) error

	// Tx runs fn in a write transaction on the current estate.
	Tx(ctx context.Context, fn func(*sql.Tx) error) error

	// Writer pins one connection on the current estate for the life of
	// an apply loop, and the loop releases it before the estate can be
	// replaced.
	Writer(ctx context.Context) (*store.Writer, error)

	// Caps is the CURRENT estate's probed capabilities, and it is here
	// for the same reason the other three are: an adoption replaces the
	// file, so a value captured at construction describes a database this
	// runner is no longer writing to. [ApplyOptions.MaxVariables] is
	// filled from it once per batch rather than once per process.
	Caps() store.Capabilities
}

// RunnerDeps is everything an applier needs that it does not own.
type RunnerDeps struct {
	Domain  Domain
	Applier Applier
	Fetch   Fetcher

	// Log is the same stream read by position, which is how the applier
	// establishes that the log still holds, at its checkpoint's sequence,
	// the record it consumed there before it applies anything past it
	// ([Runner.VerifyCheckpoint]). REQUIRED: a runner that cannot ask
	// applies a restored log's new history on top of rows it is not.
	Log CheckpointLog

	// DB is the REPLICATED estate — the file this domain's rows, its
	// operation ledger, its deferred records, its anchors and its
	// checkpoint all live in, because contract 2 puts them in one
	// transaction and a transaction is one file. Resolved per call, for
	// the reason [Estate] gives: the file can be replaced under a running
	// node.
	DB Estate

	// Checkpoint is THIS DOMAIN's committed checkpoint as its row holds it —
	// the stream, its generation (each domain's log has its own) and the
	// sequence — and CheckpointStoredAt the broker's instant for the record
	// at it, zero where the row names none. It is where the runner stands
	// from construction until its loop loads the row, which then answers
	// for it; the one thing that moves it under a running process is a
	// reanchor of this domain's own stream ([Runner.Reanchored]).
	//
	// THE WHOLE CHECKPOINT, never its generation alone. A runner built at
	// the generation stood at sequence 0 until its loop had loaded the row,
	// and everything that read it in between read that zero as this node's
	// position: the first heartbeat published it to the fleet — so a node
	// past a restored log's end, or diverged from it, looked to every peer
	// like one at the very beginning, and a peer's truncation fence could
	// lift on it — and a verification of the checkpoint's record compared
	// nothing, because a zero sequence names no record. The empty stream is
	// this domain's own; any other is refused.
	Checkpoint         Position
	CheckpointStoredAt time.Time

	// StreamCreatedAt is the broker's own creation instant for this
	// stream, AS THE BROKER REPORTS IT NOW, stored beside the checkpoint as
	// the DETECTOR: a recreated stream starts its sequences again, and this
	// is what notices — the loop compares it against the instant the
	// checkpoint was committed under and STOPS on a difference, because
	// every position it holds names a number space that no longer exists.
	//
	// It must come from the broker, never from the checkpoint row: a
	// value read back out of the row is compared against itself and
	// detects nothing, which is exactly what the engine did until the
	// wiring was tested. The zero value declares no identity at all, which
	// skips the comparison; the engine never passes it, and a caller that
	// cannot say which stream it is on is one that should refuse to run.
	//
	// A reanchor re-keys it ([Runner.Reanchored]) to the instant the
	// operator confirmed, which is the stream the checkpoint then names.
	StreamCreatedAt time.Time

	// Epoch is the per-epoch configuration the domain declared it reads.
	Epoch map[string]any

	Metrics *metrics.Recorder

	// Logger is where this writes. Nil is the package's own component
	// logger, never silence: see loggerOr for what silence cost.
	Logger *slog.Logger

	// Now is the clock, injectable so a test can drive the time budget.
	Now func() time.Time
}

// Runner is one domain's apply loop: one goroutine, one pinned connection, one
// writer of this domain's tables.
//
// # The order after the commit, and why it is a list rather than a habit
//
// The store MAY RE-RUN a transaction's body, so anything with an effect
// outside the transaction must happen after the outer call returns. A
// *sql.Tx in hand makes updating the cache or waking a waiter look natural
// inside the loop, and both are wrong there: the attempt may be rolled back
// and run again, so the cache would announce a position that never committed
// and the waiter would be released on it.
//
// The four are, in this order: the in-memory cursor the publisher's fast path
// reads; the waiters at or below it; the domain's own non-row consequences;
// and only then the acknowledgement, by IDENTITY over every record consumed.
type Runner struct {
	domain  Domain
	applier Applier
	fetch   Fetcher
	log     CheckpointLog
	db      Estate
	tables  tables
	spec    StreamSpec
	metrics *metrics.Recorder
	logger  *slog.Logger
	now     func() time.Time
	opts    ApplyOptions

	waiters waiters

	mu sync.Mutex

	// created is the creation instant of the stream this runner's positions
	// are keyed to — the broker's own at construction, and the one a
	// reanchor confirmed after it. UNDER mu, because a reanchor re-keys it
	// while the heartbeat compares live readings against it.
	created time.Time

	// cursor is the committed checkpoint, and ITS GENERATION IS THE
	// RUNNER'S: every record this loop decodes is placed at it and every
	// anchor read with it. One field rather than a second copy of the
	// number beside it, because the two were once allowed to differ — a
	// generation fixed at construction and a checkpoint a reanchor had
	// moved — and a loop resumed after a reanchor then placed the adopted
	// stream's records in the generation it had just left.
	cursor Position
	// checkpointAt is the broker's instant for the record at the cursor's
	// sequence — the one this node consumed there — and zero where that is
	// unknown ([cursorRow.storedAt]). UNDER mu AND MOVED WITH cursor, never
	// apart from it: [Runner.VerifyCheckpoint] reads the pair while the loop
	// runs, and a sequence paired with another record's instant is a
	// divergence that never happened.
	checkpointAt time.Time
	// rows counts the times the rows under the checkpoint may have been
	// replaced or re-keyed — every load of the row, every reanchor, every
	// join — so a verification that read the checkpoint before one of them
	// and the log after it can tell that its finding is about a copy this
	// runner no longer holds ([Runner.VerifyCheckpoint]).
	rows uint64
	// verify is a verification of the checkpoint's record the loop owes
	// before it applies anything past it: set when a run starts, when a
	// fetch fails, and when a reading finds the log ending below the
	// checkpoint — the three moments after which what follows the
	// checkpoint on the log may be another history — and cleared by a
	// verification that settles it ([Runner.verifyBeforeApplying]).
	verify bool
	// void is the generations the reanchor that placed the checkpoint
	// ABANDONED, read with it by [Runner.loadCursor]: a record written in
	// one is consumed and applied into no row — see [ReanchorPlan.From].
	void      voidRange
	deferred  Deferral
	hasDefer  bool
	stopped   error
	appliedAt time.Time

	// fault is the transient error the loop is currently retrying, and
	// faultSince when the first of the run of failures happened. Nil
	// between faults. See [Runner.Fault] for what a reader does with it.
	fault      error
	faultSince time.Time
	// faultReported is whether the current run of failures has been
	// written as `statelog_apply_faulted`, which happens once per run —
	// see [Runner.faulted] for why. Cleared with fault.
	faultReported bool

	// drained records that this loop has reached the end of its log at
	// least once since it started, which is what [Health.Drained] carries.
	//
	// THE LOOP IS THE ONLY HONEST WITNESS. Whether a node is behind right
	// now is a subtraction anybody can do against the stream's last
	// sequence; whether its rows have ever been the WHOLE state is a fact
	// about a series, and a reader sampling the lag on a timer can miss
	// every instant a busy log is empty. This loop cannot: a fetch the
	// broker answers with nothing, while it holds nothing itself, is that
	// instant.
	//
	// RESET WHEN THE LOOP RESTARTS, beside `stopped` and `fault`, because
	// an adoption installs a peer's rows under a new history — what this
	// node drained before says nothing about the one it now applies.
	drained bool

	// foreign is the stream this applier's positions do NOT belong to,
	// once it has been established that the broker serves one under this
	// domain's name — by the boot's comparison of the checkpoint against
	// the live instant, or by a live reading of the instant while the
	// loop runs ([Runner.ObserveStream]). Nil until then.
	//
	// NOT CLEARED BY A RE-RUN, unlike a stop or a fault: this is a verdict
	// about the positions this runner was built on, and a re-run starts
	// from the same ones. Two things move them: an operator's reanchor of
	// this domain's stream, which re-keys the runner to the stream it
	// adopted ([Runner.Reanchored]), and a snapshot adoption, which
	// replaces them wholesale with rows keyed to the live stream
	// ([Runner.Rejoined]). See [Runner.StreamIdentity].
	//
	// A re-run RE-DERIVES it rather than trusting it, though, against the
	// row it loads ([Runner.loadCursor]): an adoption whose re-key did not
	// happen has still replaced the rows, and a verdict about the rows the
	// file used to hold says nothing about the ones it holds now.
	foreign *foreignStream

	// passed is the generation the FLEET is on for this domain, once it
	// has been established that a peer re-anchored the log past this
	// applier's rows — by the heartbeat's reading of the positions register
	// ([Runner.ObserveFleetGeneration]) or by a record on the log written in
	// a generation this applier never entered ([Runner.decode]). Nil until
	// then, and never set for a domain that claims no identity, whose
	// nodes re-anchor their own copies one at a time by design.
	//
	// STICKY LIKE foreign, and for the same reason: it is a verdict about
	// the positions the rows stand at, and nothing on the log can move
	// those into the new generation — the re-anchoring peer's rows are what
	// it continues from. An adoption replaces them ([Runner.Rejoined]), and
	// a re-run judges it against the checkpoint it loads, as it does
	// foreign.
	passed *passedGeneration

	// ahead is the verdict of the last reading that found this applier's
	// checkpoint PAST the log's end ([Runner.ObserveEnd]), nil while none
	// has or once a later one found the end at or past that checkpoint.
	//
	// # Why it is kept at all, when [Health.AheadOfLog] reads the end live
	//
	// Because the write path cannot afford the read. Fence 0 runs on every
	// append and answers from what this node already knows, and this is
	// what it knows: the heartbeat's reading every interval, the zero
	// fence's reading within every write that reaches it, and the boot's.
	//
	// # Why it is NOT sticky, unlike foreign
	//
	// Because of what each reading can establish. A creation instant that
	// moved is a fact about the broker, and nothing the process does moves
	// it back. An end below the checkpoint can be one member's STALE view: a
	// stream group without a leader lets a member answer with its own state,
	// which trails the truth, so a verdict nothing could clear would stop a
	// healthy node's writes over one election for the life of the process. So
	// a later reading clears it — but only one whose end has reached THIS
	// verdict's checkpoint, never merely one paired with a lower checkpoint,
	// which says nothing about the one the verdict is about. A checkpoint
	// this runner no longer stands at — a re-run over an adopted snapshot —
	// drops it in [Runner.loadCursor] instead.
	//
	// What clearing costs is nothing, because it is not the last word: once
	// a restored log has been written past this node's checkpoint the end
	// is an ordinary end again, and what separates the log from this node's
	// history is the RECORD at the checkpoint — which the loop verifies
	// before it applies past it, and which is [Runner.diverged] when it is
	// another.
	ahead *aheadOfLog

	// diverged is the verdict that the log's record at this applier's
	// checkpoint is not the one it consumed there ([ErrLogDiverged]) — a
	// broker restored from an older copy and since written past these rows.
	// Nil until a verification finds it ([Runner.VerifyCheckpoint]).
	//
	// STICKY, unlike ahead, because what establishes it cannot be a stale
	// view: a member that has not caught up answers that it holds no record
	// at a sequence, never with a different one. Nothing on the log brings
	// these rows level, so the loop applies nothing past the checkpoint
	// while it holds. A reanchor re-keys the runner to a checkpoint that
	// names the log's own record ([Runner.Reanchored]), an adoption replaces
	// the rows ([Runner.Rejoined]), and a re-run over a checkpoint this
	// runner no longer stands at drops it ([Runner.loadCursor]).
	diverged *divergedLog

	// truncated is the verdict that the log lost records a PEER's rows hold
	// ([ErrLogTruncated]), nil while no reading of the positions register
	// has found such a peer ([Runner.ObserveTruncation]). NOT part of
	// [Runner.StreamIdentity]: this node's rows are the log's own history,
	// so its reads are served, and only its writes refuse ([Runner.Truncated]).
	//
	// RECOMPUTED ON EVERY READING rather than sticky, because it is a fact
	// about other nodes' rows: it ends when the peer is evicted, re-anchors
	// (and stands in another generation), is rebuilt from a peer, or the
	// log turns out to hold its checkpoint after all.
	truncated *Truncation

	// drain is this loop's measured records per second, smoothed.
	//
	// # Why it is measured rather than a constant
	//
	// Three answers divide a record backlog by it and report the result as
	// a TIME: a bounded stale read's "am I within the caller's staleness",
	// a refusal's `retry_after_seconds`, and the apply-lag alarm. Without a
	// measurement all three fall back to one record per second — so a node
	// two thousand records behind, which is one second of real work,
	// reports itself half an hour behind, refuses reads that should have
	// been served and fires an alarm nobody can act on.
	//
	// SMOOTHED rather than last-batch, because a single small batch at the
	// tail of a burst is not this loop's rate: an exponentially weighted
	// mean over batches lets the figure follow a genuine slowdown while
	// ignoring the shape of any one fetch.
	drain float64

	// commits is the same measurement over TRANSACTIONS rather than
	// records, smoothed identically.
	//
	// A SECOND RATE BECAUSE IT IS A SECOND RESOURCE. Rows per second is
	// what a backlog is divided by; commits per second is the FSYNC rate,
	// and under `synchronous = FULL` that is the number a device's write
	// budget is actually spent by. The two move independently by design —
	// [Runner.nextRun] fills a run toward the transaction budget precisely
	// so that a barrier-heavy stream commits once per two dozen records
	// rather than once per record — so a node whose rows/s is healthy and
	// whose commits/s has doubled is a node whose disk is doing twice the
	// work for the same progress, which neither number alone can say.
	commits float64
}

// DrainSmoothing is how much of a new batch's rate enters the measurement.
//
// A QUARTER, which is the ratio this tree already uses for a smoothed
// operational figure: four batches of a new rate move the estimate about 68 %
// of the way, so a real slowdown is visible within seconds of applying while
// one anomalous batch moves it by a quarter of its own error.
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
	case d.DB == nil:
		return nil, fmt.Errorf("statelog: applier has no database")
	}
	spec := d.Domain.Stream()
	if err := spec.Validate(); err != nil {
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
	t, err := newTables(d.Domain)
	if err != nil {
		return nil, err
	}
	logger := loggerOr(d.Logger)
	now := d.Now
	if now == nil {
		now = time.Now
	}
	return &Runner{
		domain:  d.Domain,
		applier: d.Applier,
		fetch:   d.Fetch,
		log:     d.Log,
		db:      d.DB,
		tables:  t,
		spec:    spec,
		metrics: d.Metrics,
		logger:  logger,
		now:     now,
		created: d.StreamCreatedAt,
		opts: ApplyOptions{
			ArbitratedKinds: spec.ArbitratedKinds,
			Epoch:           d.Epoch,
		},
		// THE CHECKPOINT AND THE RECORD IT NAMES, together, for the reason
		// [Runner.checkpointAt] gives.
		cursor:       checkpoint,
		checkpointAt: d.CheckpointStoredAt,
	}, nil
}

// Committed is this node's committed position on the stream.
func (r *Runner) Committed() Position {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cursor
}

// StreamCreatedAt is the creation instant of the stream this runner's
// positions are keyed to: the broker's own when the runner was built, and the
// one a reanchor confirmed after that. It is what every live reading of the
// stream is compared against ([Runner.ObserveStream]) and what the checkpoint
// is loaded against.
//
// THE RUNNER'S, rather than a copy the engine sampled at boot, because a
// reanchor re-keys it under a running process — and a copy kept elsewhere went
// on naming the stream the node booted against after the checkpoint had moved
// to another.
func (r *Runner) StreamCreatedAt() time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.created
}

// KeyedTo is the creation instant of the stream this runner's CHECKPOINT is
// keyed to: the one its rows were derived from.
//
// It is [Runner.StreamCreatedAt] except while the recreation verdict holds.
// Then the broker serves another stream and the rows still belong to the one
// before it — and which of the two a runner carries as its own instant depends
// on when the rebuild was met: one built at a boot after it carries the live
// instant, one that was running carries the old one. The verdict holds both,
// so this answers the rows' own whichever way it came about, which is what a
// position published to the fleet has to name ([coord.DomainPosition]).
func (r *Runner) KeyedTo() time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.foreign != nil {
		return r.foreign.keyed
	}
	return r.created
}

// Reanchored re-keys this runner to the stream an operator's reanchor adopted:
// the checkpoint the reanchor committed, and the creation instant the operator
// confirmed.
//
// # What it clears, and why a re-run may not
//
// The recreation verdict ([Runner.StreamIdentity]) survives a re-run because it
// is a statement that the positions this runner was built on name a stream the
// broker no longer serves, and a re-run starts from the same positions. A
// reanchor replaces them — as an adoption does from a peer's rows
// ([Runner.Rejoined]): it committed a checkpoint in a new generation on the
// live stream, keyed to the live instant. Re-keyed to both, the runner's
// positions are sequences on that stream again, so the verdict no longer holds
// — nor does a passed generation the new one has reached — and a
// reading of where the log ENDS taken against the old checkpoint says nothing
// about the new one.
//
// # Why it is called with the loop stopped, and what that buys
//
// The loop is the one writer of the checkpoint and of the cursor this sets. A
// loop still running could commit a batch at the old generation after the
// reanchor's transaction — writing back the generation and the instant the
// reanchor had just left — so the engine halts this domain's loop before the
// transition and starts it again after this. The next run loads the committed
// row, which this has already agreed with, and resumes from it in the new
// generation: one below the stream's first surviving sequence for a recreated
// stream, the log's end for a restored one ([ReanchorCase]).
//
// The readers that run beside the stopped loop are the heartbeat and the zero
// fence, which hand this runner live readings. A reading of the instant taken
// before the reanchor names the stream this re-keys to, which reads as the
// same stream; a reading of the end paired with the old checkpoint is refused
// by [Runner.ObserveEnd] because its generation is behind this one.
//
// storedAt is the broker's instant for the log's record at the new checkpoint,
// zero where the log holds none there — the record the checkpoint now names,
// and so what a diverged log is judged against from here on. A divergence
// verdict is cleared with the others: it was about the record the OLD
// checkpoint named.
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
	r.rows++
	r.foreign, r.ahead, r.diverged = nil, nil, nil
	// AND A PEER'S TRUNCATION, which was about peers in the generation this
	// left: in the one it opened, this node's rows are the fleet's history.
	r.truncated = nil
	if r.passed != nil && at.Generation >= r.passed.fleet {
		r.passed = nil
	}
	// AND THE STOP THE RECREATION OR THE DIVERGENCE CAUSED, because the
	// reanchor is what answers either — left in place until the loop's next
	// run cleared it, the health would go on reporting a stopped applier for
	// the moment in between. A stop for any other reason is a verdict about
	// this build, which a reanchor does not change, and it stays.
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

// ObserveStream takes one LIVE reading of the broker's creation instant for
// this applier's stream, and answers true the one time a reading establishes
// that the stream is not the one this applier started against.
//
// # Why the applier is told rather than asking
//
// The boot compares the checkpoint against the instant once, and nothing the
// loop reads afterwards can see a rebuild: a stream deleted and remade under a
// running node comes back at the same generation counting from 1, so its
// sequences are perfectly plausible and, once it has published past the
// checkpoint, every sequence term reads healthy. What does see it is any read
// of the stream's own state — the position heartbeat takes one every interval,
// and the fence on an expectation of zero takes one within every such write —
// and each hands the instant here, so the one verdict the read path and the
// write path both consult has every observation behind it rather than
// whichever loop happened to look.
//
// A zero instant is no reading ([StreamUnknown]), and a runner built with none
// declared no identity to compare against; neither establishes anything.
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
// a live reading of the stream's instant contradicts: its rows are keyed to
// exactly the stream the broker serves.
//
// # How a verdict comes to be wrong, and why only a join can say so
//
// An instant is never reissued, so the broker cannot come back to the stream
// the rows were keyed to — but the rows can come to the stream the broker
// serves. An adoption replaces every row with a peer's, keyed to the live
// stream, and re-keys this runner to them ([Runner.Rejoined]); when that
// re-key could not happen — the restore of an estate a failed join left closed
// could not read the live instant — the loop's own re-derivation
// ([Runner.loadCursor]) has only the instants this runner has read, and a
// stream it never read is one it judges as another. The loop then stops as
// recreated over rows that are the fleet's history, and the reanchor an
// operator would reach for refuses, because the rows ARE keyed to the live
// stream. What releases it is the join re-keying it against a reading taken
// now, so the engine asks for one when this answers true.
func (r *Runner) RecreationStale(live time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.foreign != nil && IdentityOf(r.foreign.keyed, live, true) == StreamSame
}

// ObserveEnd takes one LIVE reading of the log's last sequence, paired with
// this applier's checkpoint as the caller read it BEFORE it asked the broker,
// and reports what the reading changed: established is true the one time a
// reading finds the checkpoint past the end, reached the one time a later
// reading finds the end at or past the checkpoint an earlier one was
// established on.
//
// # Why the caller pairs them, and in that order
//
// Because the verdict is a property of a PAIR, and only that order makes the
// pair mean anything. On a healthy log the end only grows and the checkpoint
// only follows it, so a checkpoint read before the end can never exceed it —
// while one read AFTER can, whenever a record lands and is applied between the
// two reads, which on a busy company is every few milliseconds. Paired the
// wrong way round, a healthy node refuses its own writes.
//
// # What it cannot see, and what sees it instead
//
// A broker restored from an older copy is caught here only while its log ends
// below this node's checkpoint. Once something writes it past that — a peer
// whose own checkpoint was not ahead — its sequences are ordinary sequences
// again and the reading clears the verdict: a verdict that could not clear
// would turn one stale member's answer into a permanent outage, and an end is
// all this has to read. So the reading that establishes the verdict also
// leaves the loop OWING A VERIFICATION of the record at the checkpoint, which
// it settles before it applies anything past it ([Runner.verifyBeforeApplying]):
// the same record there is the stale member it looked like, and another is
// [ErrLogDiverged], which does not clear.
func (r *Runner) ObserveEnd(at Position, last uint64) (established, reached bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if at.Generation < r.cursor.Generation {
		// A READING FROM BEFORE A REANCHOR, paired with a checkpoint in
		// the generation this runner has since left. That checkpoint was
		// a sequence on the old stream, so where the adopted one ends says
		// nothing about it — and a verdict established on it would refuse
		// every write until the log happened to reach a number the node
		// will never stand at again.
		return false, false
	}
	if pastEnd(at.Seq, last) {
		established = r.ahead == nil
		r.ahead = &aheadOfLog{at: at, last: last}
		// WHATEVER FOLLOWS THE CHECKPOINT NOW may be another history, and
		// the loop must not apply it before it has looked.
		r.verify = true
		return established, false
	}
	if r.ahead == nil {
		return false, false
	}
	if pastEnd(r.ahead.at.Seq, last) {
		// STILL PAST IT: this reading was paired with a lower checkpoint
		// and says nothing about the one the verdict is about, except
		// where the log now ends. A NEW value, never a write through the
		// shared pointer: [Runner.StreamIdentity] reads it after the lock
		// is released.
		r.ahead = &aheadOfLog{at: r.ahead.at, last: last}
		return false, false
	}
	r.ahead = nil
	return false, true
}

// StreamIdentity is nil while every position this applier holds — its
// checkpoint, every anchor it wrote, every version — is a sequence on the
// stream the broker serves under this domain's name, in the history the log
// continues, and an error once that is known not to be so. Three findings
// answer it, in this order:
//
//   - wrapping [ErrStreamRecreated]: the stream was rebuilt — at boot, where the
//     checkpoint names another stream, or while running, where
//     [Runner.ObserveStream] found a new creation instant. Until a reanchor or
//     an adoption.
//   - wrapping [ErrGenerationPassed]: a peer re-anchored the log past this
//     applier's generation ([Runner.ObserveFleetGeneration], or a record
//     written in a generation this applier never entered), so the log now
//     continues from that peer's rows. Until an adoption.
//   - wrapping [ErrLogDiverged]: the log's record at this applier's checkpoint
//     is not the one it consumed there ([Runner.VerifyCheckpoint]) — a broker
//     restored from an older copy and since written past these rows. Until a
//     reanchor or an adoption.
//   - wrapping [ErrAheadOfLog]: the last reading of the log's end found it below
//     this applier's checkpoint ([Runner.ObserveEnd]) — a stream rebuilt and
//     not yet re-read, or a broker restored from an older copy, which keeps its
//     instant. For as long as the end stays below.
//
// # One answer, for the reads and the writes alike
//
// Both paths turn on the same fact — that a position this node holds names the
// record the broker holds at it — and two sources for it are how a node comes
// to refuse its reads while its writes go on landing. The writes are the half
// that cannot be repaired afterwards: a read served from the wrong history is
// wrong once, and an append arbitrated against it is on the log for every
// node to apply.
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

// ObserveTruncation takes one reading of the positions register — the peer
// whose rows hold records the log lost, or nil when no peer's do — and reports
// the transition it made: established the one time a reading finds such a peer,
// cleared the one time a later reading finds none.
//
// THE ENGINE'S READING, not this runner's, because the facts are other nodes':
// their published positions and flags, which of them the fleet has evicted, and
// whether the log holds the record at a peer's checkpoint. A reading that could
// not be taken is not handed here at all, so an unreadable register leaves the
// verdict where it was rather than clearing it.
func (r *Runner) ObserveTruncation(t *Truncation) (established, cleared bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
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
	// checkpointSettled: the log holds the record the checkpoint names — or
	// nothing can be compared, because the checkpoint names no record (a
	// row older than the column, sequence 0), the record is gone below the
	// log's first sequence, or a compacted log superseded it. None of those
	// leaves a verification that could still find anything.
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
// # Why the record, and why it settles what the end could not
//
// On one stream a sequence names one record for ever: the broker never
// rewrites one in place, and a member that has not caught up answers that it
// holds none rather than holding another. So the record at the checkpoint's
// sequence being the one this node consumed there is exactly the statement that
// the log is the history these rows came from up to that point — and a
// different one is the statement that it is not, which a broker restored from
// an older copy and then written past these rows produces and nothing else
// does. Two broker instants compared for EQUALITY: no clock is ordered against
// another, which is what every other reading of "is this the same history"
// available here would have needed.
//
// # Who asks
//
// The loop, before it applies anything past the checkpoint whenever what
// follows it may be another history ([Runner.verifyBeforeApplying]) — at every
// start, after a failed fetch, and after a reading found the log ending below
// it. And the engine's position heartbeat, every interval, which is what
// catches a broker restored under a running node between two of those moments
// and what a node whose loop has stopped for another reason still answers.
func (r *Runner) VerifyCheckpoint(ctx context.Context) (bool, error) {
	_, established, err := r.checkCheckpoint(ctx)
	return established, err
}

// checkCheckpoint is [Runner.VerifyCheckpoint], reporting which of the three
// outcomes it reached.
func (r *Runner) checkCheckpoint(ctx context.Context) (checkpointCheck, bool, error) {
	// THE PAIR, under one lock: a sequence and the instant of the record
	// consumed there, read together or not at all.
	r.mu.Lock()
	at, consumed, rows := r.cursor, r.checkpointAt, r.rows
	r.mu.Unlock()
	check, held, err := compareCheckpoint(ctx, r.log, at, consumed)
	if err != nil || check != checkpointDiverged {
		return check, false, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.rows != rows || at.Generation < r.cursor.Generation {
		// THE ROWS WERE REPLACED OR RE-KEYED BETWEEN THE READS — a run
		// that loaded another row, a reanchor, a join — so this is a
		// finding about a copy this runner no longer holds. The next
		// verification reads the pair it does hold.
		return checkpointSettled, false, nil
	}
	established := r.diverged == nil
	if established {
		r.diverged = &divergedLog{at: at, consumed: consumed, held: held}
	}
	return checkpointDiverged, established, nil
}

// ObserveDiverged takes a finding, made before this runner's loop has loaded
// its checkpoint, that the log holds another record at checkpoint at than the
// one it names — consumed is the instant the checkpoint names, held the one the
// log's record carries — and answers true the one time it establishes the
// verdict.
//
// THE BOOT'S, for the reason [Runner.ObserveEnd] is handed the end there: a
// broker restored from an older copy is met at a boot, and until the loop has
// loaded the row nothing in this runner names the record the checkpoint does,
// so a write arriving first would find nothing refusing it. The loop's own
// load keeps the verdict for exactly the row it was reached about
// ([Runner.loadCursor]).
func (r *Runner) ObserveDiverged(at Position, consumed, held time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.diverged != nil || at.Generation < r.cursor.Generation {
		return false
	}
	r.diverged = &divergedLog{at: at, consumed: consumed, held: held}
	return true
}

// CheckpointDiverged reports whether log holds, at checkpoint at, ANOTHER record
// than the one the checkpoint names by consumed — and the instant of the one it
// does hold there — for the readers of a checkpoint that are not its runner:
// the boot, before the loop has loaded the row, and a reanchor deciding its
// case from the row itself.
//
// FALSE WHENEVER NOTHING CAN BE COMPARED — no record named, the log ending
// below the checkpoint, the record gone below the log's first sequence or
// superseded on a compacted log — because each of those has its own answer
// elsewhere and none of them is a second history.
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
// OWED, NOT PERIODIC: the loop is the hottest path this package has, and a
// broker round trip per batch would be spent proving, almost always, that
// nothing happened. What makes the next record past the checkpoint possibly
// another history is one of three moments — a run starting (a boot is when a
// node meets a restored broker), a fetch failing (a broker restored under a
// running node takes its consumer with it), or a reading finding the log
// ending below the checkpoint — and each leaves [Runner.verify] set, so the
// loop asks exactly once after each, before the first record it would apply.
//
// A log that does not reach the checkpoint although the run holds a record
// past it is two readings from members that disagree: the verification is left
// owed and the run retried, never applied on the guess. A divergence STOPS the
// loop — nothing past the checkpoint of rows the log diverged from may be
// applied — and so does one the heartbeat established while this loop ran.
func (r *Runner) verifyBeforeApplying(ctx context.Context, run []Record) error {
	r.mu.Lock()
	owed, cursor, diverged := r.verify, r.cursor, r.diverged
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
		r.verify = false
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

// ObserveFleetGeneration takes the generation the FLEET is on for this domain —
// the highest any node or the trim has published — and answers true the one
// time it establishes that a peer re-anchored the log past this applier's rows.
//
// # Why a number from the positions register can establish it
//
// A generation moves only by a reanchor, or by adopting the snapshot of a node
// that ran one, so a peer standing at a later one holds the rows the log's
// history continues from — and nothing on the log can bring this node's rows
// there: a reanchor opens its generation from one node's rows, which include
// whatever that node held past what the log still carries. Before this, a node
// left on the old generation carried on as though nothing had happened wherever
// its own readings could not see the move — below a restored broker's end, or
// past it once something had written the log beyond its checkpoint — and
// served reads and arbitrated writes from rows the fleet had left behind.
//
// NEVER FOR A DOMAIN THAT CLAIMS NO IDENTITY. Its nodes re-anchor their own
// copies one at a time by design ([PermitReanchor]), so a peer ahead of this
// node's generation there is the ordinary state of a recovery in progress
// rather than a history this node has lost.
func (r *Runner) ObserveFleetGeneration(fleet uint32) bool {
	if !r.domain.ClaimsIdentity() {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.passedLocked(fleet)
}

// passedLocked records that the fleet is on generation fleet, answering true
// the one time that establishes the verdict. The caller holds mu.
func (r *Runner) passedLocked(fleet uint32) bool {
	if fleet <= r.cursor.Generation {
		return false
	}
	if r.passed != nil {
		if fleet > r.passed.fleet {
			// A NEW VALUE, never a write through the shared pointer:
			// [Runner.StreamIdentity] reads it after the lock is
			// released.
			r.passed = &passedGeneration{at: r.passed.at, fleet: fleet}
		}
		return false
	}
	r.passed = &passedGeneration{at: r.cursor, fleet: fleet}
	return true
}

// Rejoined re-derives what this runner knows about its positions from the
// checkpoint a join left in the replicated file: that checkpoint, the creation
// instant its rows are keyed to (zero where the file holds none), and the live
// instant of the stream the join was judged against.
//
// # Why a join can clear what only a reanchor could before
//
// The recreation verdict says the positions this runner was built on name a
// stream the broker no longer serves, and the passed-generation verdict says
// they stand in a generation the log has left. An adoption REPLACES those
// positions wholesale — every row, every anchor, the checkpoint — with a peer's,
// and the join installs an artefact only once it has checked, before the
// transfer and again before the install, that it was taken against the stream
// this node is live on and at the generation the fleet is on. A runner that kept
// either verdict would stop again the moment its loop loaded the adopted
// checkpoint, so the one repair that can bring such a node back left it exactly
// where it was — which is how deleting the database came to be the only way
// out.
//
// # Why it RE-DERIVES them rather than clearing them
//
// Because the caller cannot always say whether the file was replaced: a join
// that finds every domain current may be standing on an artefact an earlier
// rejoin installed and could not open. So each verdict is judged against the
// file as it now is, by the rule that established it: the recreation holds
// while the rows are keyed to another stream than the live one, and a passed
// generation holds while the rows stand below the generation it recorded — the
// highest this runner has seen, which a record on the log may have shown it
// before the positions register did. A join that replaced nothing therefore
// changes nothing, and one that installed the fleet's history clears both.
//
// # Called with the loop stopped
//
// For [Runner.Reanchored]'s reason: the loop is the one writer of the cursor
// this sets. The engine's rejoin halts every applier before the file can be
// replaced and starts them again after this.
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
	r.cursor = at
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
	// AND WHICH RECORD A MOVED CHECKPOINT NAMES is the row's to say, which
	// the loop reads when it starts again: until then it is unknown, and a
	// verification of the new position against the record the OLD
	// checkpoint named would report a divergence between two files. A
	// divergence verdict about a checkpoint this runner no longer stands at
	// goes with it. One at the SAME position stays — a join that replaced
	// nothing leaves rows the log has diverged from, and clearing it here
	// would let a write through before the loop had looked again — and the
	// loop re-derives it from the row it loads, by the record the row names
	// ([Runner.loadCursor]).
	if moved {
		r.checkpointAt = time.Time{}
		if r.diverged != nil && r.diverged.at != at {
			r.diverged = nil
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
// # Why a fault is reported late and a stop at once
//
// A stop says this node cannot run this company's records; a fault says the
// broker or the disk did not answer just now. The first is worth moving a
// company's work for and the second is not — a two-second store blip is the
// incident this whole engine's three-valued discipline was learned on — so a
// fault is retried quietly inside the budget and reported only past it, when
// the honest reading is that this node's rows have stopped moving. Reported,
// it takes the same path a stop does: reads refuse `stalled` naming it, and
// the seats move. It clears the moment a retry succeeds.
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
// once since it started — the fact [Health.Drained] carries, and the one
// question about this applier that no single reading of the lag can answer.
//
// IT NEVER GOES BACK TO FALSE while the loop runs. A node that drained and is
// now a thousand records behind has still held the whole state once, which is
// what a snapshot gate is asking about; what makes such a node a bad donor is
// the DISTANCE, and that is [Health.Lag]'s question, measured separately.
func (r *Runner) Drained() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.drained
}

// markDrained records that the broker answered this loop's fetch with nothing
// while it held nothing itself, which is this node being level with the log.
func (r *Runner) markDrained() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.drained = true
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

// WaitCommitted blocks until this node's applier has committed through p.
func (r *Runner) WaitCommitted(ctx context.Context, p Position) error {
	if p.Stream != "" && p.Stream != r.spec.Name {
		return fmt.Errorf("%w: waiting on %s from an applier of %s",
			ErrWrongStream, p.Stream, r.spec.Name)
	}
	return r.waiters.awaitPosition(ctx, p, r.Committed)
}

// WaitApplied blocks until this node holds the derived consequences of
// everything up to p for the objects in s.
//
// THE WAIT IS ON THE CHECKPOINT and never on the coverage. Coverage is a fact
// that is DECIDED — this node either holds a deferred record covering these
// objects or it does not — and a wait on it could never terminate: where a
// deferred scope intersects, the covered position is fixed at or below the
// checkpoint, so a caller would spend its whole budget waiting for a number
// that is already behind it. So the coverage is checked and the checkpoint is
// waited for, and the two are separate answers.
func (r *Runner) WaitApplied(ctx context.Context, s ScopeSet, p Position) error {
	if err := r.WaitCommitted(ctx, p); err != nil {
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

// Anchor reads a subject's arbitration anchor, which is what the publisher
// forms its expectation from.
func (r *Runner) Anchor(ctx context.Context, subject string) (Position, error) {
	var p Position
	err := r.db.Read(ctx, func(tx *sql.Tx) error {
		var err error
		p, err = r.tables.anchor(ctx, tx, subject, r.Committed().Generation)
		return err
	})
	return p, err
}

// OpsRetention is how long a node keeps its record of which operations it
// applied.
//
// THIRTY DAYS, and it is derived from the client that actually retries rather
// than from the machine one. The longest MACHINE retry is sixteen rounds
// inside a five-second wait ([DefaultResolveBudget]); but a SEAT is told to
// carry an op id forward and re-ask, and it does that on its next wake —
// hours later, and after a weekend
// for a seat that only runs on a schedule. An op id that outlives its row
// resolves `unknown` rather than `applied`, which sends a turn to re-decide
// work it already did — and never a second copy of the operation: the sweep
// records the cutoff of every pass that deleted a row, and the publisher reads
// that record before trusting the ledger's silence ([Rows.LostBefore]).
//
// It costs about 29 MB steady at the census rate (6 518 commits a day, ~150
// bytes a row) against 357 MB a year kept for ever — and "for ever" is what
// this table had before the sweep existed, on a schema whose own migration
// says it is swept and ships the index for it.
const OpsRetention = 30 * 24 * time.Hour

// PurgeOps deletes this node's operation rows applied before cutoff.
//
// PER NODE, because the table is one this node owns a copy of: it records
// which operations THIS applier wrote, so a fleet-singleton sweep would leave
// every other node's rows growing for ever while reporting that it had swept.
func (r *Runner) PurgeOps(ctx context.Context, cutoff time.Time) (int64, error) {
	return r.tables.purgeOps(ctx, r.db, cutoff)
}

// Run drives the loop until the context ends or the applier stops.
//
// # It may be run AGAIN after it returns
//
// A node that falls below the trim floor while running adopts a peer's
// snapshot: its apply loops are ended, the replicated file is replaced, and
// the loops are started again over what arrived. Every subsystem that holds
// this runner keeps holding it, so it is the same Runner that runs again —
// from the checkpoint the new file keeps, with whatever it retained
// reprocessed, and with a stop or a fault from the previous run re-evaluated
// rather than remembered: they were verdicts about rows this node no longer
// has. So are the recreation and passed-generation verdicts, which are judged
// again against the checkpoint the run loads — see [Runner.RecreationStale]
// for the one case that judgement cannot reach alone.
func (r *Runner) Run(ctx context.Context) error {
	r.mu.Lock()
	r.stopped, r.fault, r.faultSince, r.faultReported = nil, nil, time.Time{}, false
	// AND THE DRAIN LATCH, for the reason its own field states: a restart
	// here is an adoption or a re-anchor, and a drain of the history this
	// loop used to apply is not a claim about the one it is about to.
	r.drained = false
	r.mu.Unlock()

	var w *store.Writer
	defer func() {
		if w != nil {
			_ = w.Close()
		}
		// A WAITER LEFT ON A STOPPED APPLIER would wait out its whole
		// budget for a position nothing will ever reach — and one merely
		// RELEASED would read as satisfied and go on to read rows that
		// never reached its position. It is told, instead.
		r.waiters.abandonAll()
	}()

	// THE LOOP OUTLIVES A FAILURE. A fetch the broker did not answer, a
	// transaction the disk refused, an applier that errored on a record:
	// none of them says this node cannot run the company's records, and a
	// loop that returned on any of them left the domain dead for the life
	// of the process with nothing to restart it — a broker blip at the
	// wrong moment took a node's tracker down until an operator noticed
	// the seats had moved and restarted it. So a failure that is not a
	// STOP is retried here, in place, on a pause that doubles to a
	// ceiling, with the same run re-applied rather than abandoned to a
	// thirty-second redelivery. What the outside sees is [Runner.Fault]:
	// nothing inside the retry budget, and past it the honest report that
	// this node's rows have stopped moving.
	var tail []Record
	var buffer reorderBuffer
	var pause time.Duration
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		// STARTUP IS INSIDE THE RETRY REGIME, and it is the same regime.
		//
		// Pinning the connection, reading the checkpoint and reprocessing
		// what an earlier build retained are all database work, and all
		// three sat ABOVE this loop: a store that was momentarily
		// unavailable — the adoption bracket between a close and a
		// reopen, a refused transaction, a disk that answered slowly —
		// returned straight out of Run and left the domain with no
		// applier for the life of the process. Nothing restarted it and
		// nothing reported it either: Stopped stayed nil and no fault was
		// recorded, so the node went on publishing a caught-up position
		// for a domain that would never apply another record. That is the
		// one failure this loop's whole retry design exists to rule out,
		// and it was reachable through the door above it.
		if w == nil {
			pinned, err := r.startup(ctx)
			if err != nil {
				if stopped := r.faulted(ctx, err); stopped != nil {
					return stopped
				}
				pause = r.backOff(ctx, pause)
				continue
			}
			w = pinned
			r.recovered(ctx)
			pause = 0
		}
		run, err := r.nextRun(ctx, tail, &buffer)
		if err != nil {
			// A FETCH THAT FAILED may be a broker restored under this
			// running node, which takes the consumer with it: whatever
			// is delivered past the checkpoint after it is verified
			// first ([Runner.verifyBeforeApplying]).
			r.mu.Lock()
			r.verify = true
			r.mu.Unlock()
			if stopped := r.faulted(ctx, err); stopped != nil {
				return stopped
			}
			pause = r.backOff(ctx, pause)
			continue
		}
		if len(run) == 0 {
			// THE BROKER ANSWERED, with nothing: whatever was wrong
			// is not wrong now. It is also the one moment this node
			// can prove its rows are the WHOLE state rather than a
			// prefix of it — nothing is pending and nothing is in
			// hand — so it is where the drain latch is set.
			r.markDrained()
			r.recovered(ctx)
			pause = 0
			continue
		}
		if verifyErr := r.verifyBeforeApplying(ctx, run); verifyErr != nil {
			if errors.Is(verifyErr, ErrStopped) {
				verifyErr = r.stop(ctx, verifyErr)
			}
			if stopped := r.faulted(ctx, verifyErr); stopped != nil {
				return stopped
			}
			// THE SAME RUN, for the reason a failed commit keeps it.
			tail = run
			pause = r.backOff(ctx, pause)
			continue
		}
		consumed, err := r.applyRun(ctx, w, run)
		if err != nil {
			if stopped := r.faulted(ctx, err); stopped != nil {
				return stopped
			}
			// THE SAME RUN, AGAIN. Its records are delivered and
			// unacknowledged, contiguous from the checkpoint, and
			// nothing about them changed; dropping them here would
			// leave the loop fetching records above a hole for the
			// whole ack window before the broker handed these back.
			tail = run
			pause = r.backOff(ctx, pause)
			continue
		}
		r.recovered(ctx)
		pause = 0
		tail = run[len(consumed):]
	}
}

// startup is everything the loop needs before it may consume a record: a
// pinned connection, the checkpoint, and whatever an earlier build retained.
//
// IT CLEANS UP AFTER ITSELF, because it is RETRIED: a connection pinned by an
// attempt that then failed to read the checkpoint would be leaked once per
// attempt, and the pool this draws from is small enough that a few minutes of
// retrying would exhaust it — turning a transient failure into the permanent
// one the retry exists to avoid.
func (r *Runner) startup(ctx context.Context) (*store.Writer, error) {
	// A PINNED CONNECTION for the loop's life. The pool is small and every
	// reader on this node draws from it — and the readers are usually
	// waiting on state this writer is about to commit, so under load they
	// occupy every connection while the writer queues behind them for the
	// one that would unblock them.
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
	// AND THE RECORD THE CHECKPOINT NAMES, before this run can apply
	// anything past it — at once rather than when the first such record
	// arrives, because a log written EXACTLY to this checkpoint delivers
	// nothing past it and would leave a diverged node answering reads and
	// writes until the heartbeat looked. A log that does not reach the
	// checkpoint leaves the verification owed ([Runner.verifyBeforeApplying]).
	switch check, _, err := r.checkCheckpoint(ctx); {
	case err != nil:
		_ = w.Close()
		return nil, err
	case check == checkpointSettled:
		r.mu.Lock()
		r.verify = false
		r.mu.Unlock()
	case check == checkpointDiverged:
		_ = w.Close()
		r.mu.Lock()
		verdict := r.diverged
		r.mu.Unlock()
		if verdict == nil {
			return nil, fmt.Errorf("statelog: %s's checkpoint moved while its "+
				"record was verified at start — verified again", r.spec.Name)
		}
		return nil, r.stop(ctx, fmt.Errorf("%w: %w", ErrStopped, verdict.err(r.spec.Name)))
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

// faulted classifies a failure: a STOP is returned to end the loop, anything
// else is recorded as the fault being retried and swallowed.
//
// # Why a run of failures is written as three lines and never as a level
//
// `statelog_apply_retrying` when the run starts, `statelog_apply_faulted`
// ONCE, on the first retry past [ApplyRetryBudget], and
// `statelog_apply_recovered` — carrying how long the run lasted — when a retry
// succeeds.
//
// The ERROR used to be written on every retry past the budget. The loop
// retries at least once every [ApplyRetryCeiling], and a fault outliving the
// budget is precisely the kind that lasts — a full disk, a store refusing every
// transaction — so that was twelve ERROR lines a minute per domain for as long
// as it lasted: anything counting the event read one incident as hundreds, a
// count that measured how long a fault ran rather than how many there were,
// and the line marking the moment it crossed the budget was one of hundreds
// identical to it. That is why [Tracker] logs an alarm's transitions rather
// than its level, and this is the same decision for the same reason. Once per
// RUN rather than once per process, because the run is the incident:
// recovering and failing again is a second one, and it is written again.
//
// Nothing is hidden by it, because the LEVEL is carried elsewhere, by
// surfaces that are levels by construction: [Runner.Fault] names the CURRENT
// error on every read that refuses and in the node's status for as long as the
// run lasts, and `crewlet.statelog.apply.retries` counts every attempt. And
// the ERROR lands on the same retry that makes [Runner.Fault] start
// answering, so the line and the refusals and seat moves it describes begin
// together.
func (r *Runner) faulted(ctx context.Context, err error) error {
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
				"its reads refuse and its seats move until a retry succeeds")
	}
	r.count(metrics.StatelogApplyRetries)
	return nil
}

// recovered clears the fault a retry just outlived, and closes its run: the
// next failure starts a new one, written again from `statelog_apply_retrying`.
func (r *Runner) recovered(ctx context.Context) {
	r.mu.Lock()
	had, since := r.fault, r.faultSince
	r.fault, r.faultSince, r.faultReported = nil, time.Time{}, false
	r.mu.Unlock()
	if had != nil {
		r.logger.InfoContext(ctx, "statelog_apply_recovered",
			"domain", r.domain.Name(), "stream", r.spec.Name,
			"after", r.now().Sub(since).Round(time.Millisecond), "error", had.Error())
	}
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
		// THE ABANDONED GENERATIONS TRAVEL WITH THE CHECKPOINT, and are read
		// with it for the reason the range is on the row at all: this loop
		// may be the one resuming part-way through them, after a restart or
		// over a peer's snapshot.
		r.void = row.void
		if found {
			// THE CHECKPOINT AND THE RECORD IT NAMES, together — see
			// [Runner.checkpointAt].
			r.cursor, r.checkpointAt = at, row.storedAt
		}
		r.rows++
		// A RUN THAT STARTS OWES A VERIFICATION of that record before it
		// applies past it: a boot is exactly when a node meets a broker
		// restored from an older copy, and one already written past this
		// checkpoint looks, by its end, like the history these rows came
		// from ([Runner.verifyBeforeApplying]).
		r.verify = true
		// A VERDICT ABOUT A CHECKPOINT THIS RUNNER NO LONGER STANDS AT is
		// dropped. The runner is re-run over an adopted snapshot, whose
		// checkpoint is the donor's — one it applied from the live log —
		// and a reading of the end paired with the old checkpoint says
		// nothing about the new one; kept, it would refuse every write until
		// the log happened to reach a number this node has left behind. The
		// boot's own reading is paired with the row this loads, so it
		// survives the first load exactly as it should.
		if r.ahead != nil && r.ahead.at != r.cursor {
			r.ahead = nil
		}
		r.deferred, r.hasDefer = d, hasDefer
		// A RECREATED STREAM IS DETECTED HERE and nowhere else, and IT
		// IS A STOP. The generation is the response and this is what
		// notices: the broker's own creation instant moving means every
		// stored sequence — the checkpoint, every anchor, every version —
		// is a number in a space it no longer belongs to, and a consumer
		// resumed from the checkpoint waits for a sequence that never
		// arrives while reporting nothing pending. An operator reanchors;
		// until then this node's rows are frozen and its health says so.
		//
		// AND ITS WRITES REFUSE, which is why the verdict is recorded as
		// well as returned: the stop ends this loop, and the publisher
		// beside it never reads a loop's error — it asks
		// [Runner.StreamIdentity], which is this same finding.
		//
		// Compared through [IdentityOf], at the resolution the row keeps,
		// because the broker reports nanoseconds and the row keeps
		// microseconds — compared exactly, every boot after the first
		// would read as a recreation.
		//
		// AGAINST THE LIVE INSTANT THIS RUNNER KNOWS BEST: the one a
		// recreation verdict recorded, when one holds — a reading taken
		// since the runner was built — and the instant it was built with
		// otherwise. So a rebuild already established while the loop ran
		// stops a re-run too: the row still names the instant this runner
		// was built with, and resuming would apply the new stream's records
		// into rows keyed to the one before it.
		//
		// # And a verdict is RE-DERIVED here, never merely remembered
		//
		// The row this loads may not be the one the verdict was reached
		// about. A re-run follows an adoption, which replaces every row with
		// a peer's, and the join re-keys this runner to them
		// ([Runner.Rejoined]) — but not always: the restore of an estate a
		// failed join left closed judges the file against a live instant it
		// may be unable to read, and a re-key that did not happen would
		// otherwise leave this loop stopping on every re-run over rows that
		// are the fleet's history, with nothing but deleting the database to
		// release it. So each verdict is judged against the row by the rule
		// that established it. The recreation holds while the row is keyed
		// to another stream than the live one, and a row keyed to the live
		// one clears it: an instant is never reissued, so only an adoption
		// or a reanchor writes a row keyed to a stream this runner did not
		// start on. A passed generation holds while the row stands below the
		// generation it recorded.
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
			// A NEW VALUE, never a write through the shared pointer:
			// [Runner.StreamIdentity] reads it after the lock is
			// released. Keyed to the ROW'S instant, because that is
			// what [Runner.KeyedTo] publishes to the fleet and what
			// [Runner.RecreationStale] is judged by.
			r.foreign = &foreignStream{keyed: created, live: live}
		default:
			r.created, r.foreign = live, nil
		}
		if r.foreign != nil {
			return fmt.Errorf("%w: %w", ErrStopped,
				r.foreign.err(r.domain.Name(), r.spec.Name))
		}
		// SO DOES A GENERATION THE FLEET HAS MOVED PAST, for the same
		// reason: a re-run from the same rows would apply the new
		// generation's records into them. A rejoin that found no donor
		// starts the loop again, and this is what stops it at once, saying
		// why — where a loop that was already running when the verdict
		// landed is stopped by [Runner.decode] at its next record.
		if r.passed != nil && found && at.Generation >= r.passed.fleet {
			r.passed = nil
		}
		if r.passed != nil {
			return fmt.Errorf("%w: %w", ErrStopped,
				r.passed.err(r.domain.Name(), r.spec.Name))
		}
		// AND A LOG THAT DIVERGED FROM THESE ROWS, judged by the record the
		// row names: the same checkpoint naming the same record is the same
		// rows, and a re-run from them would apply the other history past
		// it. Anything else is a file an adoption replaced — a donor's
		// checkpoint names the log's own record — and the verdict was about
		// the one before it.
		if r.diverged != nil && found &&
			(r.diverged.at != at || !sameRecord(row.storedAt, r.diverged.consumed)) {
			r.diverged = nil
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
// # The rule is the live loop's, replayed over the table
//
// The loop retained a record for one of two reasons: its version was above
// what the build could read, or its scope met a record already retained. The
// second is why order matters here and why a probe below the current position
// is the one question asked: a record this build can read stays retained
// while a record EARLIER in the log that covers its scope is still retained,
// and applying it first would produce state no other node holds — the same
// state the retain rule refused to produce live. Walked oldest first with each
// applied record released as it goes, that check is exactly the live one.
//
// A record still above this build's version is left where it is, and so is
// everything it covers, however many builds it waits through.
//
// # The checkpoint does not move and the anchor does not move
//
// Both advanced when the record was retained: the log CONSUMED it then, and
// the anchor is a MAX so a replay at the original position is a no-op on it.
// What a reprocess writes is the rows, the operation id and the release, in
// one transaction per record — a transaction per record rather than one over
// the whole backlog, because a retained record's own apply is the ordinary
// one and holds the writer for exactly as long as it would have.
//
// Until this existed nothing read the retained table to apply from it. A node
// that deferred a record on a rolling upgrade kept the deferral after it was
// upgraded, refused every read and write about the objects it covered for the
// life of the deployment, and reported the version it needed — which it was
// already running.
func (r *Runner) reprocess(ctx context.Context, w *store.Writer) error {
	var after int64
	var applied, kept int
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
			if row.version > r.domain.RecordVersion() {
				kept++
				continue
			}
			env, err := r.domain.Envelope(row.payload)
			if err != nil {
				return fmt.Errorf("statelog: %s could not read the envelope of a "+
					"retained record at packed position %d, which every build must "+
					"be able to: %w", r.domain.Name(), row.position, err)
			}
			rec := Record{
				Envelope: env,
				Position: Position{
					Stream:     r.spec.Name,
					Generation: uint32(row.position / GenerationStride),
					Seq:        uint64(row.position % GenerationStride),
				},
				Payload:  row.payload,
				StoredAt: row.storedAt,
			}
			landed, err := r.reprocessOne(ctx, w, rec)
			if err != nil {
				return err
			}
			if landed {
				applied++
				r.applier.Committed(ctx)
			} else {
				kept++
			}
		}
	}
	if applied == 0 && kept == 0 {
		return nil
	}
	if err := r.refreshDeferred(ctx); err != nil {
		return err
	}
	r.logger.InfoContext(ctx, "statelog_retained_reprocessed",
		"domain", r.domain.Name(), "applied", applied, "kept", kept,
		"build_reads", r.domain.RecordVersion())
	if applied > 0 && r.metrics != nil {
		r.metrics.Add(metrics.StatelogApplyRecords, uint64(applied),
			metrics.Attrs{"domain": r.domain.Name(), "result": "reprocessed"})
	}
	return nil
}

// reprocessOne applies one retained record unless an earlier retained record
// still covers it, reporting whether it landed.
func (r *Runner) reprocessOne(ctx context.Context, w *store.Writer, rec Record) (bool, error) {
	landed := false
	err := w.Tx(ctx, func(tx *sql.Tx) error {
		landed = false
		if _, covered, err := r.tables.deferredBelow(ctx, tx, rec.Scope, &rec.Position); err != nil {
			return err
		} else if covered {
			return nil
		}
		opts := r.opts
		opts.Now = r.now()
		opts.MaxVariables = r.db.Caps().MaxVariables
		if _, _, err := r.applyOne(ctx, tx, rec, opts); err != nil {
			return err
		}
		if err := r.tables.release(ctx, tx, rec.Position); err != nil {
			return err
		}
		landed = true
		return nil
	})
	if err != nil {
		return false, fmt.Errorf("statelog: reprocess the retained record at %s: %w",
			rec.Position, err)
	}
	return landed, nil
}

// nextRun fills a run toward the transaction budget, starting from whatever
// the previous transaction did not consume.
//
// FILLED TOWARD THE BUDGET while records are pending, rather than one fetch
// per transaction: a fetch is bounded by bytes and a transaction by rows and
// time, and committing whatever one fetch happened to contain makes the commit
// rate a property of the fetch size instead of the budget. On a barrier-heavy
// stream that is one commit — and one fsync — per two dozen records.
func (r *Runner) nextRun(ctx context.Context, tail []Record, buffer *reorderBuffer) ([]Record, error) {
	run := tail
	for {
		if len(run) > 0 {
			pending, err := r.fetch.Pending(ctx)
			if err != nil || pending == 0 {
				//nolint:nilerr // Pending is a HINT and its failure
				// closes the batch rather than the applier: it says
				// only whether another fetch could add anything, and
				// what the run already holds is decoded, contiguous
				// and safe to commit either way. A broker that cannot
				// answer it fails the next Fetch too — and THAT call
				// is the one that reports, because it is the one whose
				// failure means the loop cannot proceed.
				return run, nil
			}
			if r.budgetWouldBind(run) {
				return run, nil
			}
		}
		wait := FetchWait
		if len(run) == 0 && r.waiters.len() > 0 {
			// SOMEBODY IS ALREADY WAITING, so collect what is there
			// rather than holding the batch open for records that may
			// never come. The batch closes when it is FULL or after
			// this wait, so a long one on a quiet log is the whole
			// latency a linearizable read pays.
			wait = ApplyLinger
		}
		if len(run) > 0 {
			// A PARTIAL RUN LINGERS, and gives it up the instant
			// somebody is waiting: a caller that appended a barrier
			// and is waiting for it would otherwise pay the whole
			// linger for a record already in hand — a hundred times
			// the append itself, and on an idle company every barrier
			// is a partial batch.
			wait = ApplyLinger
			if target, waiting := r.waiters.minimum(); waiting && r.have(run, target) {
				r.count(metrics.StatelogLingerYields)
				return run, nil
			}
		}
		batch, err := r.fetch.Fetch(ctx, FetchMessages, FetchBytes, wait)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, fmt.Errorf("statelog: fetch from %s: %w", r.spec.Name, err)
		}
		records, err := r.decode(ctx, batch)
		if err != nil {
			return nil, err
		}
		ready, err := buffer.admit(records, r.Committed(), r.spec.Replay, run)
		if err != nil {
			return nil, r.stop(ctx, err)
		}
		run = append(run, ready...)
		if len(batch) == 0 {
			if len(run) == 0 {
				return nil, nil
			}
			// THE LINGER EXPIRED WITH RECORDS IN HAND, so the run
			// commits. A pull that handed over nothing while the
			// broker still reports records pending is the broker
			// WITHHOLDING them: the consumer's in-flight ceiling is
			// reached by exactly the records this run holds, and it
			// hands over no more until they are acknowledged — which
			// happens only after this commit. A loop that kept pulling
			// here waited for a delivery its own commit was the
			// precondition of, and did so for ever.
			return run, nil
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
		env, err := r.domain.Envelope(m.Payload)
		if err != nil {
			// AN UNREADABLE ENVELOPE IS A STOP, not a deferral and not
			// a retry. The envelope is the half every build can read,
			// so failing on it means the record is not this domain's —
			// retaining it would index nothing, because every field
			// the index needs is inside the envelope, and retrying it
			// would read the same bytes the same way.
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
		// A RECORD FROM A GENERATION THIS APPLIER NEVER ENTERED IS A STOP,
		// on a domain that claims identity. Its writer stood in that
		// generation, so a peer re-anchored the log and this node did not:
		// the log now continues from that peer's rows, and applying what
		// follows into these would mix two histories nothing can
		// separate afterwards. It is the one reading that sees the move
		// the moment it reaches this node — the reanchor's own record is
		// the first thing the new generation puts on the log — where the
		// positions register is a heartbeat away.
		//
		// AND SO IS ANY RECORD ONCE THE MOVE IS KNOWN, however it became
		// known: the heartbeat's reading of the register can establish it
		// while this loop runs and no record of the new generation has
		// reached this node — a node ahead of a restored broker's end skips
		// the reanchor's own record, and a record need not carry its
		// writer's generation — and a loop that went on applying would put
		// the new generation's records into these rows all the same. (A
		// re-run is stopped before it fetches, by [Runner.loadCursor].) The
		// node adopts the peer's snapshot; the verdict is what refuses its
		// reads and writes until it has.
		if passed || (env.Gen > gen && r.domain.ClaimsIdentity()) {
			r.mu.Lock()
			r.passedLocked(env.Gen)
			verdict := *r.passed
			r.mu.Unlock()
			return nil, r.stop(ctx, fmt.Errorf("%w: the record at sequence %d, "+
				"written in generation %d, is not applied: %w", ErrStopped, m.Seq,
				env.Gen, verdict.err(r.domain.Name(), r.spec.Name)))
		}
		out = append(out, Record{
			Envelope: env,
			Position: Position{Stream: r.spec.Name, Generation: gen, Seq: m.Seq},
			Payload:  m.Payload,
			StoredAt: m.StoredAt,
			ack:      m.Ack,
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
	var tally results
	var rows int
	var boundBy string
	started := r.now()

	// THE ABORTS ARE COUNTED HERE, and this is the only place that can.
	//
	// `crewlet.statelog.apply.tx.aborts` was declared and catalogued and
	// never written, so the series was permanently absent and the gauge
	// documented as "measured on the operator's own hardware rather than on
	// a benchmark's" read as no-data for ever. What it answers is whether
	// an apply's body is ever run twice, which is the assumption fourteen
	// of this design's throughput figures rest on.
	//
	// IT READS ZERO BY CONSTRUCTION NOW, and that is a stronger statement
	// than the one it replaced rather than a reason to retire it. The
	// driver detects write conflicts per FILE, so internal/store takes the
	// write lock at BEGIN and queues its writers for it: nothing committing
	// elsewhere in the file can abort an apply, and a contended one waits
	// rather than losing. What a count here means is therefore a TRANSIENT
	// FAILURE that surfaced from inside a body the store then ran again —
	// the retry budget being spent rather than held in reserve.
	//
	// It cannot be counted in the store: [store.DB.Tx]'s retry is where the
	// re-run happens, and internal/store may not import a metrics package
	// the whole engine sits above. But the store RE-RUNS the body, so this
	// closure's own invocation count is the same number — attempts minus
	// the one that committed — read from the layer that owns the
	// instrument.
	attempts := 0
	err := w.Tx(ctx, func(tx *sql.Tx) error {
		attempts++
		// RESET ON EVERY ATTEMPT. The store re-runs the body of an
		// attempt that failed transiently, so a counter accumulated
		// across attempts counts the abandoned one too — and the
		// metrics would report work that was rolled back.
		consumed, committedAt, tally, rows, boundBy = consumed[:0], Position{}, results{}, 0, ""
		committedRecord = time.Time{}
		txStart := r.now()

		hasDeferred, err := r.anyDeferred(ctx, tx)
		if err != nil {
			return err
		}
		// THE HIGH-WATER GUARD, and it is the checkpoint's own rule
		// applied WITHIN the transaction: records apply in strictly
		// increasing position, so a duplicate that arrives above the
		// committed checkpoint but at or below something this
		// transaction has already applied must be skipped too. Reading
		// only the committed cursor would apply such a record twice —
		// the second time against rows the first one wrote.
		highWater := r.Committed()
		opts := r.opts
		opts.Now = txStart
		opts.MaxVariables = r.db.Caps().MaxVariables

		for i := range run {
			rec := run[i]
			if rec.Position.Packed() <= highWater.Packed() {
				// AT OR BELOW THE CHECKPOINT: consumed, never
				// applied. A redelivery of a record this node
				// already committed must be acknowledged, or the
				// broker redelivers it for ever.
				consumed = append(consumed, rec)
				tally.skipped++
				continue
			}
			// THE FRAMEWORK'S OWN WRITE FIRST, from the
			// always-decodable envelope, because it happens
			// whatever the record then does.
			if arbitrates(r.spec.ArbitratedKinds, rec.Subject.Kind) {
				if err := r.tables.advanceAnchor(ctx, tx, r.tables.subjectOf(rec.Subject), rec.Position); err != nil {
					return err
				}
			}

			switch {
			case rec.V > r.domain.RecordVersion():
				if r.domain.InstallsGate(rec.Envelope) {
					// A GATE IS A STOP, and it is the one
					// place the retain rule inverts. A
					// deferred gate does not postpone one
					// record's effect on one node — it
					// silently licenses every record above
					// it, and an eviction deferred by the
					// node it evicts leaves that node
					// passing every fence it has.
					return fmt.Errorf("%w: %s at %s installs an apply gate at "+
						"record version %d and this build reads %d — a gate this "+
						"node cannot read would license every record above it, so "+
						"the applier halts and its seats move to a node that can",
						ErrStopped, rec.Kind, rec.Position, rec.V, r.domain.RecordVersion())
				}
				if err := r.tables.retain(ctx, tx, rec, r.spec.Replay == ReplayCompacted, opts.MaxVariables); err != nil {
					return err
				}
				hasDeferred = true
				tally.retained++
				r.logger.WarnContext(ctx, "statelog_record_deferred",
					"domain", r.domain.Name(), "position", rec.Position.String(),
					"kind", rec.Kind, "record_version", rec.V,
					"build_reads", r.domain.RecordVersion())

			default:
				blocked := false
				if hasDeferred {
					// A RECORD ON STALE ROWS IS RETAINED
					// TOO. Applying it on top of rows a
					// deferred record never wrote produces
					// state no other node holds, and
					// nothing later can tell that it did.
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
					n, gated, err := r.applyOne(ctx, tx, rec, opts)
					if err != nil {
						return err
					}
					rows += n
					if gated {
						tally.gated++
					} else {
						tally.applied++
					}
				}
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
		// THE HIGHEST POSITION THIS TRANSACTION HOLDS, never the tail of
		// the run.
		//
		// A run's last element is a stale REDELIVERY whenever one
		// arrived late: [reorderBuffer.admit] passes a record below the
		// run's high-water mark straight through so the caller
		// acknowledges it, so a run legitimately closes as [11, 12, 11].
		// Checkpointing the tail there writes 11 in the same transaction
		// that committed 12's rows — a cursor that UNDERSTATES its own
		// database, which is the one thing the checkpoint exists to
		// rule out. The damage outlives the transaction: the waiters
		// release through 11, so a linearizable read waiting for 12 is
		// refused `behind` over rows this node already holds; and the
		// next boot resumes at 12 and re-applies a record whose anchor
		// was already advanced past it.
		//
		// The floor is this node's committed cursor rather than zero,
		// because a run made ENTIRELY of redeliveries below the
		// checkpoint has to be acknowledged without moving it at all.
		committedAt = highest(consumed, r.Committed())
		// AND THE RECORD IT NAMES: the one consumed at that position, or
		// — for a run of nothing but redeliveries — the one the checkpoint
		// already named ([Runner.checkpointAt]).
		committedRecord = r.checkpointRecord()
		if top := topRecord(consumed); top.Position == committedAt {
			committedRecord = top.StoredAt
		}
		return r.tables.setCursor(ctx, tx, committedAt, r.StreamCreatedAt(),
			committedRecord, r.now())
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

	// AFTER THE OUTER TRANSACTION RETURNS, IN THIS ORDER.
	//
	// THE POSITION THE TRANSACTION COMMITTED, so what this node reports,
	// releases waiters through and resumes from is the same value the
	// checkpoint row holds — see the comment at the setCursor above.
	at := committedAt
	if tally.retained > 0 {
		// WHAT THIS NODE CANNOT READ is what its readiness and its
		// coverage both turn on, so it is refreshed the moment it
		// changes rather than at the next boot.
		if err := r.refreshDeferred(ctx); err != nil {
			return nil, err
		}
	}
	r.advance(at, committedRecord)
	r.waiters.release(at)
	r.applier.Committed(ctx)
	r.ack(ctx, consumed)

	r.measureDrain(started, len(consumed))
	r.countAborts(attempts)
	// THE TOP RECORD, for the same reason: the apply LATENCY is measured
	// from a record's own StoredAt, and a stale redelivery's is an hour
	// old, so reading the tail reports the redelivery's age as this
	// batch's latency.
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
		// THE FIRST BATCH IS THE ESTIMATE, because smoothing from zero
		// would make a node report a quarter of its real rate for its
		// first several batches — which is exactly the window after a
		// restart, when the backlog is longest and the figure matters
		// most.
		r.drain, r.commits = rate, commits
		return
	}
	r.drain += DrainSmoothing * (rate - r.drain)
	r.commits += DrainSmoothing * (commits - r.commits)
}

// Drain is this loop's measured records per second, and 0 before it has
// applied anything.
//
// ZERO MEANS UNMEASURED, and every caller reads it that way: dividing a
// backlog by it would be a division by zero, so each one falls back to a floor
// rather than to a guess. A node that has applied nothing has no rate, which
// is a different fact from a node applying nothing per second.
func (r *Runner) Drain() float64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.drain
}

// Commits is this loop's measured transactions per second, and 0 before it
// has applied anything.
//
// ZERO MEANS UNMEASURED, exactly as [Runner.Drain]'s does. Nothing divides by
// this one — it is published rather than consumed, because what an operator
// does with it is compare it against the device's own committed write rate.
func (r *Runner) Commits() float64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.commits
}

// applyOne runs the domain's state machine for one record, unless a gate says
// it must produce no rows.
func (r *Runner) applyOne(ctx context.Context, tx *sql.Tx, rec Record, opts ApplyOptions) (int, bool, error) {
	started := r.now()
	opts.StoredAt = rec.StoredAt
	// THE FRAMEWORK'S OWN GATE FIRST: a record written in a generation the
	// reanchor that placed this checkpoint abandoned. It is the framework's
	// because the range is the checkpoint's, and it is asked of the record's
	// OWN generation — the writer's stamp — never of the position the loop
	// composed, which is this checkpoint's generation for every record.
	reason, gated := ReasonAbandoned, r.void.holds(rec.Gen)
	if !gated {
		var err error
		reason, gated, err = r.applier.Gated(ctx, tx, rec)
		if err != nil {
			return 0, false, fmt.Errorf("statelog: read the apply gates at %s: %w", rec.Position, err)
		}
	}
	if gated {
		// A DURABLE RECORD THAT APPLIES NOWHERE. It still advanced the
		// anchor and it still advances the checkpoint: the log consumed
		// it, and a checkpoint that skipped it would replay it for ever.
		r.logger.WarnContext(ctx, "statelog_record_gated",
			"domain", r.domain.Name(), "position", rec.Position.String(),
			"kind", rec.Kind, "gate", string(reason), "writer", rec.Writer)
		r.countWith(metrics.StatelogRecordsGated, metrics.Attrs{
			"gate": string(reason), "subject_kind": rec.Subject.Kind,
		})
		return 0, true, nil
	}
	n, err := r.applier.Apply(ctx, tx, rec, opts)
	if err != nil {
		return 0, false, fmt.Errorf("statelog: apply %s at %s: %w", rec.Kind, rec.Position, err)
	}
	if err := r.tables.writeOp(ctx, tx, rec.OpID, r.tables.subjectOf(rec.Subject), rec.Position,
		rec.StoredAt, opts.Now); err != nil {
		return 0, false, err
	}
	if r.metrics != nil {
		// ONE RECORD'S APPLY, which is the real ceiling on how long a
		// read can be delayed: a single record past the time budget is
		// still one transaction, so the batch's own duration cannot
		// bound it.
		r.metrics.Observe(metrics.StatelogApplyRecordDuration, r.now().Sub(started),
			metrics.Attrs{"domain": r.domain.Name(), "kind": rec.Kind})
	}
	return n, false, nil
}

// anyDeferred reports whether this node holds any record it cannot decode, so
// the per-record probe is skipped entirely on the ordinary path.
func (r *Runner) anyDeferred(ctx context.Context, tx *sql.Tx) (bool, error) {
	_, ok, err := r.tables.oldestDeferred(ctx, tx)
	return ok, err
}

// ack acknowledges every record this transaction consumed, BY IDENTITY.
//
// Not by count, and the difference is a real defect rather than a style
// preference: a run carries records that were applied, records retained
// because this build could not read them, records dropped by a gate and
// records already below the checkpoint — and acknowledging "as many as were
// applied" acknowledges the wrong ones. The retained record's delivery is left
// open and redelivered for ever; an applied record's is acknowledged in its
// place.
//
// NEVER A NAK. A record this node cannot apply is retained, stopped on, or
// already committed — and a nak would hand it back to a broker that will
// deliver it to this same node again.
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

// advance moves the in-memory checkpoint the publisher's fast path reads.
func (r *Runner) advance(at Position, record time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cursor.Packed() < at.Packed() {
		// THE PAIR MOVES TOGETHER — see [Runner.checkpointAt].
		r.cursor, r.checkpointAt = at, record
	} else if r.cursor == at && r.checkpointAt.IsZero() {
		// A REDELIVERY OF THE CHECKPOINT'S OWN RECORD names it where the
		// row did not: a checkpoint committed before the column existed.
		r.checkpointAt = record
	}
	r.appliedAt = r.now()
}

// checkpointRecord is the instant of the record the committed checkpoint names.
func (r *Runner) checkpointRecord() time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.checkpointAt
}

// stop halts the applier and records why.
//
// HEALTH GOES FALSE IMMEDIATELY rather than after a grace: "this node cannot
// run this company's records" is a different answer from "this node is briefly
// behind", and only the first is worth moving a company's work for.
//
// # The one line a stop writes
//
// Every stop the loop makes comes through here, so this is the line, and it
// says what an operator needs to act on it: which log, where it froze, why,
// and what resumes it. The engine used to write a second line under the same
// name when [Runner.Run] returned, carrying only that last sentence, and one
// stop read as two — under `component=engine`, which is not where the
// replication guide sends an operator looking for it.
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
			"read of them refuses; for a domain that gates seat admission this "+
			"node also stops claiming seats, and the ones it holds move; a build "+
			"that can read what this one could not resumes it at its next boot, "+
			"and for a recreated stream an operator's reanchor of this one "+
			"stream resumes it in place, with no restart")
	return err
}

// results counts what happened to the records one transaction consumed.
//
// FOUR OUTCOMES, counted separately, because "records consumed" alone cannot
// answer the question an operator actually has: a node whose position advances
// while it applies NOTHING — every record retained because it cannot read them
// — is healthy on lag and useless in fact.
type results struct {
	applied  int
	retained int
	gated    int
	skipped  int
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

func (r *Runner) countWith(name string, attrs metrics.Attrs) {
	if r.metrics == nil {
		return
	}
	r.metrics.Add(name, 1, attrs)
}
