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

// RunnerDeps is everything an applier needs that it does not own.
type RunnerDeps struct {
	Domain  Domain
	Applier Applier
	Fetch   Fetcher

	// DB is the REPLICATED estate — the file this domain's rows, its
	// operation ledger, its deferred records, its anchors and its
	// checkpoint all live in, because contract 2 puts them in one
	// transaction and a transaction is one file.
	DB *store.DB

	// Generation is the estate's generation, read once per loop
	// generation because only an operator's reanchor moves it.
	Generation uint32

	// StreamCreatedAt is the broker's own creation instant for this
	// stream, stored beside the checkpoint as the DETECTOR: a recreated
	// stream starts its sequences again, and this is what notices.
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
// The store RE-RUNS a conflicted transaction's body, so anything with an
// effect outside the transaction must happen after the outer call returns. A
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
	db      *store.DB
	tables  tables
	spec    StreamSpec
	metrics *metrics.Recorder
	logger  *slog.Logger
	now     func() time.Time
	opts    ApplyOptions
	created time.Time
	gen     uint32

	waiters waiters

	mu        sync.Mutex
	cursor    Position
	deferred  Deferral
	hasDefer  bool
	stopped   error
	appliedAt time.Time

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
		gen:     d.Generation,
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
func (r *Runner) Stopped() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.stopped
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
		p, err = r.tables.anchor(ctx, tx, subject, r.gen)
		return err
	})
	return p, err
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
func (r *Runner) Run(ctx context.Context) error {
	// A PINNED CONNECTION for the loop's life. The pool is small and every
	// reader on this node draws from it — and the readers are usually
	// waiting on state this writer is about to commit, so under load they
	// occupy every connection while the writer queues behind them for the
	// one that would unblock them.
	w, err := r.db.Writer(ctx)
	if err != nil {
		return fmt.Errorf("statelog: pin the applier's connection: %w", err)
	}
	defer func() {
		_ = w.Close()
		// A WAITER LEFT ON A STOPPED APPLIER waits out its whole budget
		// for a position nothing will ever reach.
		r.waiters.releaseAll()
	}()

	if err := r.loadCursor(ctx); err != nil {
		return err
	}
	r.logger.InfoContext(ctx, "statelog_applier_started",
		"domain", r.domain.Name(), "stream", r.spec.Name,
		"protocol", string(r.spec.Replay), "position", r.Committed().String())

	var tail []Record
	var buffer reorderBuffer
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		run, err := r.nextRun(ctx, tail, &buffer)
		if err != nil {
			return err
		}
		if len(run) == 0 {
			continue
		}
		consumed, err := r.applyRun(ctx, w, run)
		if err != nil {
			return err
		}
		tail = run[len(consumed):]
	}
}

// loadCursor reads this domain's checkpoint at boot.
func (r *Runner) loadCursor(ctx context.Context) error {
	return r.db.Read(ctx, func(tx *sql.Tx) error {
		at, created, found, err := r.tables.readCursor(ctx, tx)
		if err != nil {
			return err
		}
		d, hasDefer, err := r.tables.oldestDeferred(ctx, tx)
		if err != nil {
			return err
		}
		r.mu.Lock()
		defer r.mu.Unlock()
		if found {
			r.cursor = at
			// A RECREATED STREAM IS DETECTED HERE and nowhere else. The
			// generation is the response and this is what notices: the
			// broker's own creation instant moving means every stored
			// sequence is a number in a space it no longer belongs to.
			if !r.created.IsZero() && !created.IsZero() && !created.Equal(r.created) {
				r.logger.WarnContext(ctx, "statelog_stream_recreated",
					"domain", r.domain.Name(), "stream", r.spec.Name,
					"cursor_created_at", created, "stream_created_at", r.created,
					"generation", r.gen)
			}
		}
		r.deferred, r.hasDefer = d, hasDefer
		return nil
	})
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
		records, err := r.decode(batch)
		if err != nil {
			return nil, err
		}
		ready, err := buffer.admit(records, r.Committed(), r.spec.Replay, len(run))
		if err != nil {
			return nil, r.stop(ctx, err)
		}
		run = append(run, ready...)
		if len(run) == 0 && len(batch) == 0 {
			return nil, nil
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
func (r *Runner) decode(batch []Message) ([]Record, error) {
	out := make([]Record, 0, len(batch))
	for _, m := range batch {
		env, err := r.domain.Envelope(m.Payload)
		if err != nil {
			// AN UNREADABLE ENVELOPE IS A STOP, not a deferral. The
			// envelope is the half every build can read, so failing
			// on it means the record is not this domain's — and
			// retaining it would index nothing, because every field
			// the index needs is inside the envelope.
			return nil, fmt.Errorf("statelog: %s could not read the envelope at "+
				"sequence %d, which every build must be able to: %w",
				r.domain.Name(), m.Seq, err)
		}
		if env.Scope.Empty() {
			return nil, fmt.Errorf("statelog: the record at sequence %d declares "+
				"no scope — an empty scope claims it makes nothing stale, which "+
				"is the one claim a record no build may be able to read cannot "+
				"make", m.Seq)
		}
		out = append(out, Record{
			Envelope: env,
			Position: Position{Stream: r.spec.Name, Generation: r.gen, Seq: m.Seq},
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
	// this driver's transaction conflicts are row-scoped or
	// database-scoped, which is the assumption fourteen of this design's
	// throughput figures rest on.
	//
	// It cannot be counted in the store: [store.retryStale] is where the
	// abort happens, and internal/store may not import a metrics package
	// the whole engine sits above. But the store RE-RUNS the body, so this
	// closure's own invocation count is the same number — attempts minus
	// the one that committed — read from the layer that owns the
	// instrument.
	attempts := 0
	err := w.Tx(ctx, func(tx *sql.Tx) error {
		attempts++
		// RESET ON EVERY ATTEMPT. The store re-runs a conflicted
		// transaction's body, so a counter accumulated across attempts
		// counts the abandoned one too — and the metrics would report
		// work that was rolled back.
		consumed, tally, rows, boundBy = consumed[:0], results{}, 0, ""
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
				if err := r.tables.retain(ctx, tx, rec, r.spec.Replay == ReplayCompacted); err != nil {
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
						if err := r.tables.retain(ctx, tx, rec, r.spec.Replay == ReplayCompacted); err != nil {
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
		return r.tables.setCursor(ctx, tx,
			consumed[len(consumed)-1].Position, r.created, r.now())
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
	at := consumed[len(consumed)-1].Position
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
	r.observe(started, rows, boundBy, tally, consumed[len(consumed)-1])
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
