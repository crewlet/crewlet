package statelog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"math"
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

	// DB is the REPLICATED estate — the file this domain's rows, its
	// operation ledger, its deferred records, its anchors and its
	// checkpoint all live in, because contract 2 puts them in one
	// transaction and a transaction is one file. Resolved per call, for
	// the reason [Estate] gives: the file can be replaced under a running
	// node.
	DB Estate

	// Generation is the generation of the checkpoint the runner starts
	// from, and only that: from its first run on, the loop's generation is
	// its checkpoint's, read from the estate each time it starts — an
	// adoption can install a peer's checkpoint at a newer generation under
	// a runner that keeps running, and the reanchor verb moves it too.
	Generation uint32

	// StreamCreatedAt is the broker's own creation instant for this
	// stream, AS THE BROKER REPORTS IT NOW, stored beside the checkpoint as
	// the DETECTOR: a recreated stream starts its sequences again, and this
	// is what notices — the loop compares it against the instant the
	// checkpoint was committed under and STOPS on a difference, because
	// every position it holds names a number space that no longer exists.
	//
	// It must come from the broker, never from the checkpoint row: a
	// value read back out of the row is compared against itself and
	// detects nothing. The zero value declares no identity at all, which
	// skips the comparison; the engine never passes it, and a caller that
	// cannot say which stream it is on is one that should refuse to run.
	StreamCreatedAt time.Time

	// Epoch is the per-epoch configuration the domain declared it reads.
	Epoch map[string]any

	Metrics *metrics.Recorder
	Logger  *slog.Logger

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
	db      Estate
	tables  tables
	spec    StreamSpec
	metrics *metrics.Recorder
	logger  *slog.Logger
	now     func() time.Time
	opts    ApplyOptions
	created time.Time

	waiters waiters

	// cursor is the checkpoint this loop has reached, and ITS GENERATION IS
	// THE LOOP'S: every record it applies is stamped with it and every
	// anchor is read at it. One field rather than a generation beside it,
	// because an adoption replaces the checkpoint under a runner that keeps
	// running — the file it resumes from may be a peer's at a newer
	// generation — and a copy taken at construction would go on stamping
	// the old one over rows keyed to the new.
	mu     sync.Mutex
	cursor Position

	// deferred is the earliest record this node has retained, and retained
	// how many it holds — zero when it holds none. A record is retained
	// when this build cannot decode it, or when its scope meets a record
	// already retained. Read together in one transaction and kept together
	// under mu, so no reader pairs a count from one refresh with an earliest
	// from another.
	deferred Deferral
	retained uint64

	stopped   error
	appliedAt time.Time

	// fault is the transient error the loop is currently retrying, and
	// faultSince when the first of the run of failures happened. Nil
	// between faults. See [Runner.Fault] for what a reader does with it.
	fault      error
	faultSince time.Time

	// drain is this loop's measured records per second, smoothed.
	//
	// RECORDS, not rows: every record a run moves over — applied, retained,
	// gated or skipped as a redelivery — over the run's own duration, from
	// asking for the store's writer to acknowledging the run. A backlog is
	// counted in records, so records are what it is divided by.
	//
	// THE WHOLE RUN, deliberately wider than the transaction the
	// `apply.tx.duration` histogram times: the wait for the writer, an
	// attempt the store rolled back and ran again, and the acknowledgement
	// after the commit are all time a backlog costs to apply, so a rate that
	// left them out would state every backlog as shorter than it is.
	//
	// # Why it is measured rather than a constant
	//
	// A record backlog divided by it — through [BacklogTime] — is how this
	// node states a backlog as a TIME: against a stale read's staleness
	// bound, in a refusal's `retry_after_seconds`, and in a bulk edit's
	// projected occupancy and the wait a caller refused behind it is told
	// to take. Divided by a
	// constant instead, every one of those is wrong wherever the real rate
	// differs from it: at [DrainFloor], a node two thousand records behind
	// states itself over half an hour behind whatever its real rate, and
	// refuses a stale read whose bound that rate would meet.
	//
	// SMOOTHED rather than last-batch, because a single small batch at the
	// tail of a burst is not this loop's rate: an exponentially weighted
	// mean over batches lets the figure follow a genuine slowdown while
	// ignoring the shape of any one fetch.
	drain float64

	// commits is the same measurement over TRANSACTIONS rather than
	// records, smoothed identically.
	//
	// A SECOND RATE BECAUSE IT IS A SECOND RESOURCE. Records per second is
	// what a backlog is divided by; commits per second is the FSYNC rate,
	// and under `synchronous = FULL` that is the number a device's write
	// budget is actually spent by. The two move independently by design —
	// [Runner.nextRun] fills a run toward the transaction budget while
	// records are pending, so one commit carries many records rather than
	// one — so a node whose records/s is healthy and whose commits/s has
	// doubled is a node whose disk is doing twice the work for the same
	// progress, which neither number alone can say.
	commits float64
}

// DrainSmoothing is how much of a new batch's rate enters the measurement.
//
// A QUARTER, which is the ratio this tree already uses for a smoothed
// operational figure: four batches of a new rate move the estimate about 68 %
// of the way, so a real slowdown is visible within seconds of applying while
// one anomalous batch moves it by a quarter of its own error.
const DrainSmoothing = 0.25

// DrainFloor is the rate, in records a second, that [BacklogTime] divides a
// backlog by when nobody has measured one: [Runner.Drain] is zero until the
// loop has consumed a batch, and a backlog divided by zero is infinite.
//
// ONE A SECOND, deliberately low, so that before the loop has measured its own
// rate a backlog reads long rather than short: at the floor a backlog of n
// records reads as n seconds. Long is the safe direction for every consumer of
// the conversion — a refused read's retry hint ([RetryHint]) that sends its
// caller back after the backlog rather than into a second refusal, a staleness
// bound ([Query.MaxLag]) that refuses rather than serves past what its caller
// accepts, and a bulk edit's projection in the tracker, which a bulk refused
// behind it is told to wait out and which counts against the fleet's read
// budget: overstated, a caller waits longer than it had to; understated, it
// comes back into a second refusal and the budget reads healthier than it is.
//
// THE UNMEASURED ZERO ONLY. A MEASURED rate below one record a second is used
// as measured, for the same reason: flooring it would state a slow applier's
// backlog as shorter than that applier will take — a hint that sends its
// caller back early, a bound that serves a read past it, a bulk projected to
// end before it has applied.
const DrainFloor = 1.0

// maxBacklog is the longest time [BacklogTime] states: the largest whole
// number of seconds a time.Duration holds, about 292 years.
//
// A CEILING THE CONVERSION CAN REACH, because a measured drain can be a small
// fraction of a record a second — one slow run is enough, since the first run
// after a boot is the estimate — and a large backlog over it passes a
// Duration's range. Converted past that range a float is
// implementation-defined, and on amd64 it is the most negative duration: a
// backlog stated as a node AHEAD of the log, which passes every staleness bound
// and hands out a retry hint already due.
//
// WHOLE SECONDS, so [RetryHint] can round a hint up to the next second without
// passing it.
const maxBacklog = time.Duration(math.MaxInt64/int64(time.Second)) * time.Second

// BacklogTime is how long this node needs to apply a backlog of records at
// drain records a second: the ONE conversion from a record count to a time, so
// the staleness bound, the retry hint and the tracker's bulk projection cannot
// state one backlog as three different times.
//
// DIVIDED AS A FLOAT, because the drain is one: a rate of 1.5 records a second
// truncated to a whole one before the division states a backlog half as long
// again as it is. A drain that is not a positive rate is a rate nobody
// measured, which [DrainFloor] stands in for.
func BacklogTime(records uint64, drain float64) time.Duration {
	if records == 0 {
		return 0
	}
	if !(drain > 0) {
		drain = DrainFloor
	}
	seconds := float64(records) / drain
	if seconds >= float64(maxBacklog/time.Second) {
		return maxBacklog
	}
	return time.Duration(seconds * float64(time.Second))
}

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
	case d.DB == nil:
		return nil, fmt.Errorf("statelog: applier has no database")
	}
	spec := d.Domain.Stream()
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	t, err := newTables(d.Domain)
	if err != nil {
		return nil, err
	}
	logger := d.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	now := d.Now
	if now == nil {
		now = time.Now
	}
	return &Runner{
		domain:  d.Domain,
		applier: d.Applier,
		fetch:   d.Fetch,
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
		cursor: Position{Stream: spec.Name, Generation: d.Generation},
	}, nil
}

// Committed is this node's committed position on the stream.
func (r *Runner) Committed() Position {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cursor
}

// Stopped is the error that halted this applier, or nil.
//
// A STOP IS PERMANENT and only [ErrStopped] is one: a gate this build cannot
// read, a hole that will not close, a recreated stream, and a record whose
// envelope no build could read or that declares no scope. Every other failure
// is retried in place — see [Runner.Run] — and is reported through
// [Runner.Fault] instead; a cancellation of the loop's own context is returned
// from Run and recorded here not at all.
func (r *Runner) Stopped() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.stopped
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

// Deferred is the earliest record this node has retained, and how many it
// retains; the count is zero when it retains none. The count covers every
// retained record — those this build cannot decode, and those held back behind
// one because their scopes meet — so it is not a count of undecodable records.
//
// THE COUNT IS THE RETAINED TABLE'S OWN rather than a flag: it is what the
// register row and the `deferred.count` gauge publish, and a flag would state
// one retained record and a thousand as the same thing.
func (r *Runner) Deferred() (Deferral, uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.deferred, r.retained
}

// Waiting is how many callers are blocked on this applier right now.
//
// READ FROM THE WAITERS THEMSELVES rather than from the loop, so it goes on
// counting while the loop is stopped — which is when callers pile up behind
// it, and when the engine's position heartbeat publishing this matters most.
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
	if _, held := r.Deferred(); held == 0 {
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
			Detail: fmt.Sprintf("this node retains %s, whose scope covers what "+
				"this operation is about", d.Describe(r.domain.RecordVersion())),
			Position: d.Position,
		}
	}
	return nil
}

// Anchor reads a subject's arbitration anchor, which is what the publisher
// forms its expectation from.
func (r *Runner) Anchor(ctx context.Context, subject string) (Position, error) {
	gen := r.Committed().Generation
	var p Position
	err := r.db.Read(ctx, func(tx *sql.Tx) error {
		var err error
		p, err = r.tables.anchor(ctx, tx, subject, gen)
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
// work it already did.
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

// Op answers where an operation was applied on this node.
func (r *Runner) Op(ctx context.Context, opID string) (Position, bool, error) {
	var p Position
	var ok bool
	err := r.db.Read(ctx, func(tx *sql.Tx) error {
		var err error
		p, ok, err = r.tables.op(ctx, tx, opID)
		return err
	})
	return p, ok, err
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
// has.
func (r *Runner) Run(ctx context.Context) error {
	r.mu.Lock()
	r.stopped, r.fault, r.faultSince = nil, nil, time.Time{}
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
			if stopped := r.faulted(ctx, err); stopped != nil {
				return stopped
			}
			pause = r.backOff(ctx, pause)
			continue
		}
		if len(run) == 0 {
			// THE BROKER ANSWERED, with nothing: whatever was wrong
			// is not wrong now.
			r.recovered(ctx)
			pause = 0
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
func (r *Runner) faulted(ctx context.Context, err error) error {
	if errors.Is(err, ErrStopped) || ctx.Err() != nil {
		return err
	}
	r.mu.Lock()
	first := r.fault == nil
	if first {
		r.faultSince = r.now()
	}
	r.fault = err
	since := r.faultSince
	r.mu.Unlock()
	if first {
		r.logger.WarnContext(ctx, "statelog_apply_retrying",
			"domain", r.domain.Name(), "stream", r.spec.Name,
			"position", r.Committed().String(), "error", err.Error(),
			"reported_after", ApplyRetryBudget)
	} else if r.now().Sub(since) >= ApplyRetryBudget {
		r.logger.ErrorContext(ctx, "statelog_apply_faulted",
			"domain", r.domain.Name(), "stream", r.spec.Name,
			"position", r.Committed().String(), "error", err.Error(),
			"since", since, "detail", "this node's rows have stopped moving; "+
				"its reads refuse and its seats move until a retry succeeds")
	}
	r.count(metrics.StatelogApplyRetries)
	return nil
}

// recovered clears the fault a retry just outlived.
func (r *Runner) recovered(ctx context.Context) {
	r.mu.Lock()
	had, since := r.fault, r.faultSince
	r.fault, r.faultSince = nil, time.Time{}
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
		at, created, found, err := r.tables.readCursor(ctx, tx)
		if err != nil {
			return err
		}
		d, held, err := r.tables.retainedState(ctx, tx)
		if err != nil {
			return err
		}
		r.mu.Lock()
		defer r.mu.Unlock()
		if found {
			r.cursor = at
		}
		r.deferred, r.retained = d, held
		// A RECREATED STREAM IS DETECTED HERE and nowhere else, and IT
		// IS A STOP. The generation is the response and this is what
		// notices: the broker's own creation instant moving means every
		// stored sequence — the checkpoint, every anchor, every version —
		// is a number in a space it no longer belongs to, and a consumer
		// resumed from the checkpoint waits for a sequence that never
		// arrives while reporting nothing pending. An operator reanchors;
		// until then this node's rows are frozen and its health says so.
		//
		// Compared through [IdentityOf], at the resolution the row keeps,
		// because the broker reports nanoseconds and the row keeps
		// microseconds — compared exactly, every boot after the first
		// would read as a recreation.
		if !r.created.IsZero() {
			if state := IdentityOf(created, r.created, found); state == StreamRecreated {
				return fmt.Errorf("%w: %w — %s's checkpoint was committed against a "+
					"stream created at %s and the broker's %s was created at %s, so "+
					"every position this node holds names a sequence space that no "+
					"longer exists; `crewlet retention reanchor -stream %s` is what "+
					"follows the new stream from its head",
					ErrStopped, ErrStreamRecreated, r.domain.Name(),
					created.UTC().Format(time.RFC3339Nano),
					r.spec.Name, r.created.UTC().Format(time.RFC3339Nano), r.spec.Name)
			}
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
// rate — and the fsync rate with it — a property of the fetch size instead of
// the budget.
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
	gen := r.Committed().Generation
	out := make([]Record, 0, len(batch))
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
	var tally results
	var rows int
	var boundBy string
	started := r.now()

	// THE ABORTS ARE COUNTED HERE, and this is the only place that can.
	//
	// What `crewlet.statelog.apply.tx.aborts` answers is whether an apply's
	// body is ever run twice. It reads zero by construction: the driver
	// detects write conflicts per FILE, so internal/store takes the write
	// lock at BEGIN and queues its writers for it — nothing committing
	// elsewhere in the file can abort an apply, and a contended one waits
	// rather than losing. A count here is therefore a TRANSIENT FAILURE that
	// surfaced from inside a body the store then ran again: the retry budget
	// being spent rather than held in reserve.
	//
	// It cannot be counted in the store: [store.DB.Tx]'s retry is where the
	// re-run happens, and internal/store may not import a metrics package
	// the whole engine sits above. But the store RE-RUNS the body, so this
	// closure's own invocation count is the same number — attempts minus
	// the one that committed — read from the layer that owns the
	// instrument.
	attempts := 0
	// txStart is when the attempt now running began its body, and after
	// the commit it is the COMMITTING attempt's. The store has taken the
	// file's write lock by the time a body runs and holds it to the commit,
	// so the span from there to the commit is the transaction's hold on the
	// writer — what every writer queued behind it waits out, and what
	// `apply.tx.duration` times.
	var txStart time.Time
	err := w.Tx(ctx, func(tx *sql.Tx) error {
		attempts++
		// RESET ON EVERY ATTEMPT. The store re-runs the body of an
		// attempt that failed transiently, so a counter accumulated
		// across attempts counts the abandoned one too — and the
		// metrics would report work that was rolled back.
		consumed, committedAt, tally, rows, boundBy = consumed[:0], Position{}, results{}, 0, ""
		txStart = r.now()

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
		return r.tables.setCursor(ctx, tx, committedAt, r.created, r.now())
	})
	// THE COMMIT'S OWN INSTANT, taken before anything that follows it: the
	// hold ends here, and a record is applied here rather than when its
	// acknowledgement goes out.
	committed := r.now()
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
	r.advance(at)
	r.waiters.release(at)
	r.applier.Committed(ctx)
	r.ack(ctx, consumed)

	r.measureDrain(started, len(consumed))
	r.countAborts(attempts)
	// THE TOP RECORD, for the same reason: the apply LATENCY is measured
	// from a record's own StoredAt, and a stale redelivery's is an hour
	// old, so reading the tail reports the redelivery's age as this
	// batch's latency.
	r.observe(committed.Sub(txStart), committed, rows, boundBy, tally, topRecord(consumed))
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
// consumed anything.
//
// ZERO MEANS UNMEASURED: a node that has consumed nothing has no rate, which
// is a different fact from a node applying nothing per second. A backlog is
// divided by it through [BacklogTime], which reads the zero as exactly that
// and takes [DrainFloor] in its place.
func (r *Runner) Drain() float64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.drain
}

// Commits is this loop's measured transactions per second, and 0 before it
// has consumed anything — the same zero [Runner.Drain] answers, from the same
// runs.
//
// NOTHING DIVIDES BY IT. The engine's position heartbeat reads it and
// publishes it beside the drain, because what an operator does with it is
// compare it against the device's own committed write rate.
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
	reason, gated, err := r.applier.Gated(ctx, tx, rec)
	if err != nil {
		return 0, false, fmt.Errorf("statelog: read the apply gates at %s: %w", rec.Position, err)
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
	if err := r.tables.writeOp(ctx, tx, rec.OpID, r.tables.subjectOf(rec.Subject), rec.Position, opts.Now); err != nil {
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

// anyDeferred reports whether this node retains any record, so the per-record
// probe is skipped entirely on the ordinary path.
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

// refreshDeferred re-reads the earliest record this node retains and how many
// it retains.
func (r *Runner) refreshDeferred(ctx context.Context) error {
	return r.db.Read(ctx, func(tx *sql.Tx) error {
		d, held, err := r.tables.retainedState(ctx, tx)
		if err != nil {
			return err
		}
		r.mu.Lock()
		defer r.mu.Unlock()
		r.deferred, r.retained = d, held
		return nil
	})
}

// advance moves the in-memory checkpoint the publisher's fast path reads.
func (r *Runner) advance(at Position) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cursor.Packed() < at.Packed() {
		r.cursor = at
	}
	r.appliedAt = r.now()
}

// stop halts the applier and records why.
//
// HEALTH GOES FALSE IMMEDIATELY rather than after a grace: "this node cannot
// run this company's records" is a different answer from "this node is briefly
// behind", and only the first is worth moving a company's work for.
func (r *Runner) stop(ctx context.Context, err error) error {
	r.mu.Lock()
	if r.stopped == nil {
		r.stopped = err
	}
	r.mu.Unlock()
	r.logger.ErrorContext(ctx, "statelog_applier_stopped",
		"domain", r.domain.Name(), "stream", r.spec.Name,
		"position", r.Committed().String(), "error", err.Error())
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

// observe records one committed run: held is the committing attempt's hold on
// the writer and committed the commit's own instant.
func (r *Runner) observe(held time.Duration, committed time.Time, rows int, boundBy string,
	tally results, last Record) {

	if r.metrics == nil {
		return
	}
	domain := r.domain.Name()
	if boundBy == "" {
		boundBy = "drained"
	}
	r.metrics.Observe(metrics.StatelogApplyTxDuration, held,
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
	//
	// NO WAITERS GAUGE HERE. How many callers are blocked is a STATE, and
	// sampled as a run ends it stops moving exactly when it matters — an
	// applier that has stopped ends no runs while its waiters pile up — so
	// the position heartbeat samples [Runner.Waiting] instead.
	if !last.StoredAt.IsZero() {
		r.metrics.Observe(metrics.StatelogApplyLatency, committed.Sub(last.StoredAt),
			metrics.Attrs{"domain": domain})
	}
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
