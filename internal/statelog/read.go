package statelog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// ReadLevel is how fresh an answer has to be, and there are four because
// there are four different questions a caller can be asking.
type ReadLevel string

const (
	// ReadLinearizable is "no answer from before this read arrived". It
	// costs a quorum-committed append and a wait, and it is what a caller
	// asks for when the answer decides something.
	ReadLinearizable ReadLevel = "linearizable"

	// ReadSession is "no answer from before MY last write". It costs
	// nothing when the caller is caught up, which is the common case, and
	// it is the honest default for a caller reading its own work — a
	// caller that can NAME that write: the position it landed at is what
	// the level waits for ([Query.Session] inside the engine,
	// [Query.MinPosition] from outside it), and a session read that was
	// handed no position waits for nothing and serves this node's own
	// prefix under a stronger name. That is why no read grammar accepts
	// the level without a `min_position` beside it.
	ReadSession ReadLevel = "session"

	// ReadStale is "whatever this node holds, with its lag reported". It
	// takes no broker call at all, which is why it keeps answering when a
	// full log has stopped every level that does.
	ReadStale ReadLevel = "stale"

	// ReadConsistentPrefix is "a coherent point in the log's own order,
	// possibly behind". NO SURFACE EVER DEFAULTS TO IT and only a caller
	// may ask: it is weaker than session in a way that is invisible in
	// the answer, so a default would silently downgrade every reader that
	// did not know to ask for more.
	ReadConsistentPrefix ReadLevel = "consistent_prefix"
)

// ReadLevels are the four, in decreasing strength.
var ReadLevels = []ReadLevel{
	ReadLinearizable, ReadSession, ReadStale, ReadConsistentPrefix,
}

// Valid reports whether a level off the wire is one this build knows.
func (l ReadLevel) Valid() bool { return slices.Contains(ReadLevels, l) }

// ReadRefusal is why a read was not served, and each value names a different
// thing for the caller to do.
type ReadRefusal string

const (
	// RefuseBehind — this node has not reached the position this read
	// needs. It clears on its own, and the hint says roughly when.
	RefuseBehind ReadRefusal = "behind"

	// RefuseDeferred — this node retains a record covering what this read
	// is about: one it cannot decode, or one held back behind such a record.
	// Another node can answer; this one cannot, and no amount of waiting
	// changes that.
	RefuseDeferred ReadRefusal = "deferred"

	// RefuseDeferredScopeUnknown — the deferred record's own scope could
	// not be read, so nothing can be said about what it covers. It blocks
	// the whole domain, which is why it is a different code from the one
	// that blocks a named set of objects.
	RefuseDeferredScopeUnknown ReadRefusal = "deferred_scope_unknown"

	// RefuseStalled — this node's applied prefix has stopped moving. Its
	// rows are frozen, so a short answer would be wrong rather than old.
	RefuseStalled ReadRefusal = "stalled"

	// RefuseBelowFloor — records this node never applied have been
	// trimmed, so its rows are missing state no replay can supply.
	RefuseBelowFloor ReadRefusal = "below_floor"

	// RefuseFloorUnknown — the published floor could not be established:
	// the read of it did not answer, or a read this node makes before it
	// failed. The third value BLOCKS, because guessing here keeps a node
	// serving over a hole it cannot see — and it clears the moment a read
	// answers, so it is worth coming back for.
	RefuseFloorUnknown ReadRefusal = "floor_unknown"

	// RefuseGenerationLeft — the published floor was read, and it is at a
	// generation this node's rows are not on: the fleet re-anchored the
	// domain and this node did not follow. Waiting does not clear it; this
	// node adopting the fleet's generation does.
	RefuseGenerationLeft ReadRefusal = "generation_left"

	// RefuseEvicted — this node has been removed from the fleet.
	RefuseEvicted ReadRefusal = "evicted"

	// RefuseBrokerUnreachable — the broker did not answer, so no read
	// index could be established.
	RefuseBrokerUnreachable ReadRefusal = "broker_unreachable"

	// RefuseNoQuorum — the barrier did not commit. It is not the same as
	// unreachable: the broker answered, and a majority did not agree.
	RefuseNoQuorum ReadRefusal = "no_quorum"

	// RefuseBrokerBusy — the stream's ingest queue was full, so the broker
	// stored nothing and said so. Like `no_quorum` the broker answered, and
	// unlike it no majority was asked: the queue in front of the log is
	// what refused, and it clears as the queue drains. It costs only the
	// levels that append.
	RefuseBrokerBusy ReadRefusal = "broker_busy"

	// RefuseLogFull — the log is at its byte ceiling and refuses appends,
	// so a barrier cannot be written. A full log therefore costs the
	// levels that append; the ones that do not keep answering.
	RefuseLogFull ReadRefusal = "log_full"

	// RefuseBarrierRefused — the broker refused the barrier for a reason
	// other than a full log or a full ingest queue: a size limit set on the
	// stream below a barrier's own size, a sealed stream, a server at its
	// storage limit. The broker's words are the detail. Like a full log it
	// costs only the levels that append, and waiting on this node does not
	// change a broker's setting.
	RefuseBarrierRefused ReadRefusal = "barrier_refused"

	// RefuseWrongStream — the position this read was asked to reach is on
	// another stream, which is a caller bug rather than a state.
	RefuseWrongStream ReadRefusal = "wrong_stream"

	// RefuseTooStale — this node's lag is past what the caller said it
	// would accept.
	RefuseTooStale ReadRefusal = "too_stale"
)

// ReadRefusals are every code. The COUNT IS DERIVED from this slice rather
// than written beside it in prose: a number in a comment and a set in code are
// two lists, and the comment is the one nobody updates.
var ReadRefusals = []ReadRefusal{
	RefuseBehind, RefuseDeferred, RefuseDeferredScopeUnknown, RefuseStalled,
	RefuseBelowFloor, RefuseFloorUnknown, RefuseGenerationLeft, RefuseEvicted,
	RefuseBrokerUnreachable, RefuseNoQuorum, RefuseBrokerBusy, RefuseLogFull,
	RefuseBarrierRefused, RefuseWrongStream, RefuseTooStale,
}

// Valid reports whether a refusal code off the wire is one this build knows.
func (r ReadRefusal) Valid() bool { return slices.Contains(ReadRefusals, r) }

// Retryable reports whether coming back to THIS node could produce a
// different answer.
//
// It is what separates a hint from a redirect. A node that is behind will
// catch up; a node holding a record it cannot decode will not, however long
// the caller waits, and a hint there would send a caller round a loop that
// cannot terminate.
func (r ReadRefusal) Retryable() bool {
	switch r {
	case RefuseBehind, RefuseNoQuorum, RefuseBrokerUnreachable, RefuseBrokerBusy,
		RefuseStalled, RefuseFloorUnknown:
		return true
	}
	return false
}

// OrdinaryLag reports whether the refusal says only that this node is behind
// what the read accepts — which every node is, for a moment after every write
// — rather than naming a fault.
//
// It is the one classification the `read_refusals` alarm separates on, and
// both halves of that alarm read it: the reading that decides whether the
// alarm fires, and the remedy that tells an operator which codes are a wait.
// Two lists of which codes are waits would be two answers that drift.
//
// NOT [ReadRefusal.Retryable]. The two sets differ in both directions: a
// barrier that did not commit is worth coming back for and is a fault while it
// lasts, and a read bounded tighter than this node's lag is refused with no
// hint at all and is still nothing but lag.
func (r ReadRefusal) OrdinaryLag() bool {
	return r == RefuseBehind || r == RefuseTooStale
}

// ElectionRetryHint is what a caller is told to wait after a barrier that did
// not commit.
//
// FOUR SECONDS, which is the broker's own minimum election timeout: a barrier
// that failed to reach a quorum is waiting on an election, and a hint shorter
// than the election itself sends every reader back before anything can have
// changed.
const ElectionRetryHint = 4 * time.Second

// BusyRetryHint is what a caller is told to wait after a barrier the broker's
// full ingest queue refused.
//
// ONE SECOND. Nothing on this node measures how fast the broker drains that
// queue, so the hint is not derived; it is the write path's own first pause
// after the same refusal ([ApplyRetryBeat]) rounded up to the whole second a
// hint is stated in, so a reader and a writer meeting one full queue come back
// to it on the same scale.
const BusyRetryHint = time.Second

// Refused is a read that was not served.
type Refused struct {
	// Code says what happened and what to do about it.
	Code ReadRefusal

	// Level is the level that was asked for.
	Level ReadLevel

	// Detail names the specific thing — the deferred record's version and
	// position, the field an operator has to change.
	Detail string

	// RetryAfter is DERIVED rather than fixed where it can be: how far
	// behind this node is, divided by how fast it is actually draining.
	// Zero means no hint, which is the honest answer for a refusal
	// waiting cannot clear.
	RetryAfter time.Duration
}

func (r *Refused) Error() string {
	if r.Detail == "" {
		return fmt.Sprintf("statelog: %s read refused (%s)", r.Level, r.Code)
	}
	return fmt.Sprintf("statelog: %s read refused (%s): %s", r.Level, r.Code, r.Detail)
}

// Unwrap makes every refusal answer errors.Is against the shared sentinel, so
// a caller can tell a refusal from a failure before it looks at the code.
func (r *Refused) Unwrap() error { return ErrUnavailable }

// RetryHint derives how long a caller should wait before coming back.
//
// FROM THE OBSERVED DRAIN, not from a constant. A flat hint is wrong in both
// directions on the same fleet: it sends a caller back too early on a node
// grinding through a bulk apply, and holds one waiting on a node that caught
// up in milliseconds. So the backlog is converted through [BacklogTime], the
// one conversion every record count stated as a time goes through. Zero means
// no hint at all, which is what a refusal waiting cannot clear deserves.
func RetryHint(code ReadRefusal, lag uint64, recordsPerSecond float64) time.Duration {
	if !code.Retryable() {
		return 0
	}
	switch code {
	case RefuseNoQuorum, RefuseBrokerUnreachable, RefuseFloorUnknown:
		// AN UNREAD FLOOR TAKES THE ELECTION'S HINT. The floor is a
		// coordination record and coordination rides the broker's own
		// connection, so a read of it that did not answer is waiting on
		// the same election an uncommitted barrier is. The code's other
		// cause — this node's own store failing a read the health makes
		// before the floor — has no measured recovery time of its own,
		// so it is given the same one.
		return ElectionRetryHint
	case RefuseBrokerBusy:
		return BusyRetryHint
	}
	if lag == 0 {
		// NO BACKLOG TO DIVIDE. What the caller is waiting for is not a
		// record this node has yet to apply — it is a position the log has
		// not reached, or a stalled applier's next attempt — and nothing
		// here measures either, so the hint is the short constant a
		// barrier that did not commit gets.
		return ElectionRetryHint
	}
	// ROUNDED UP to a whole second, because a hint is read by a person and
	// by a retry loop and neither wants milliseconds. [BacklogTime]'s
	// ceiling is itself a whole number of seconds, so rounding up to the
	// next one cannot pass it.
	hint := BacklogTime(lag, recordsPerSecond)
	if part := hint % time.Second; part != 0 {
		hint += time.Second - part
	}
	return hint
}

// Query is what a read is about and how fresh it has to be.
type Query struct {
	// Level is the freshness asked for.
	Level ReadLevel

	// Scope is the read's CLOSURE — the objects the answer is actually
	// about, computed from the rows it is about to read — rather than its
	// argument. A filter's smallest covering term set, never an
	// enumeration.
	Scope ScopeSet

	// Session is the caller's own high-water mark on this stream, which
	// is what a session read waits for.
	Session Position

	// MinPosition is an explicit floor a caller may name — the position a
	// wake carried, or one it was handed by its own previous write. It is
	// honoured at EVERY level (see [Freshness]): the later of it and the
	// level's own target is what the read waits for, so a caller that
	// holds a position is never served rows from before it whatever else
	// it asked for.
	MinPosition Position

	// MaxLag is what a stale read says it will accept, and zero means it
	// accepts anything.
	MaxLag time.Duration

	// MaxLagSeq is the same bound counted in RECORDS, and it is the
	// one the broker actually answers: [Health.Lag] is a record count, and
	// the duration above is derived from it through this node's own drain
	// rate. A caller that knows how many records it can tolerate — a
	// screen redrawing on the next wake — says so here rather than
	// translating through a rate it cannot see.
	//
	// Both may be set, and the read refuses on WHICHEVER IS REACHED FIRST:
	// they are two readings of one distance rather than two distances, so
	// an answer past either is past the caller's bound.
	MaxLagSeq uint64

	// Set reports a read whose answer is a SET rather than one object.
	//
	// It changes what a deferred scope does: a point read refuses,
	// because the object it is about may be stale; a set read cannot
	// enumerate what would have ENTERED the set — a deferred create is an
	// absence with no local row — so it is served at the level asked for
	// and makes no completeness claim at all.
	Set bool
}

// Answer is what a read returns beside its rows.
type Answer struct {
	// Level is the level the answer was actually served at, which is the
	// one asked for or a refusal.
	Level ReadLevel

	// Position is the checkpoint the answer was read at.
	Position Position

	// Lag is how far behind the log this answer is, and nil when the
	// broker could not say.
	Lag *uint64

	// Complete reports whether the answer is about every row that matches.
	//
	// SEPARATE FROM Level, because coverage and freshness are two facts.
	// An answer can be perfectly fresh and incomplete — a deferred create
	// is a row that would have entered the set and has no local trace —
	// and folding them into one field makes the second invisible.
	Complete bool

	// Incomplete says what is missing when Complete is false.
	Incomplete *Incomplete
}

// Incomplete describes a set answer's gap.
//
// NO IDENTIFIERS. Enumerating the objects a deferred record touched discloses
// neither the DIRECTION of the difference — a row that would have entered the
// set, or one that would have left it — nor reliably the right ids, and it
// truncates. What a caller can act on is that there IS a gap, where it starts,
// and which scope it is in.
type Incomplete struct {
	// Records is how many retained records have a declared scope that
	// meets this read — each one this build cannot decode, or one held back
	// behind such a record.
	Records uint64

	// From is the lowest of their positions.
	From Position

	// Scope is what the earliest of them declared it touches.
	Scope ScopeSet

	// Direction is always "unknown", and it is a field rather than an
	// omission so a reader meets the fact rather than inferring it.
	Direction string

	// Version is the record version the earliest of them was written at —
	// the same record a point read's refusal names. Above this build's own,
	// it is the number an operator picks a build by; at or below it, that
	// record is held back behind one this build cannot decode.
	Version int
}

// Reader answers reads at a level, over one domain.
type Reader struct {
	domain  Domain
	tables  tables
	db      readStore
	index   *ReadIndex
	waiter  Waiter
	health  func() Health
	drain   func() float64
	metrics *metrics.Recorder
	now     func() time.Time

	// watch is the current run of fault-class refusals at each level, for
	// [Reader.FaultRefusingSince].
	watch refusalWatch
}

// readStore is the database, as narrowly as a read needs it.
type readStore interface {
	Read(ctx context.Context, fn func(*sql.Tx) error) error
}

// ReaderDeps is everything a reader needs that it does not own.
type ReaderDeps struct {
	Domain Domain
	DB     readStore
	Index  *ReadIndex
	Waiter Waiter

	// Health is this domain's readiness, read fresh on every read because
	// every one of its terms can change between two of them.
	Health func() Health

	// Drain is this node's measured drain in records a second, which a
	// refusal's retry hint and a read's staleness bound divide a backlog
	// by, through [BacklogTime]. Nil is a rate nobody measured, which
	// [DrainFloor] stands in for.
	Drain func() float64

	Metrics *metrics.Recorder

	// Now is the clock the local refusals and the refusal watch read. Nil
	// is the wall clock; a case that is about how long a state has held
	// sets it rather than sleeping through the state.
	Now func() time.Time
}

// NewReader builds a domain's read path.
func NewReader(d ReaderDeps) (*Reader, error) {
	switch {
	case d.Domain == nil:
		return nil, fmt.Errorf("statelog: reader has no domain")
	case d.DB == nil:
		return nil, fmt.Errorf("statelog: reader has no database")
	case d.Waiter == nil:
		return nil, fmt.Errorf("statelog: reader has no waiter")
	case d.Health == nil:
		return nil, fmt.Errorf("statelog: reader has no health source")
	}
	t, err := newTables(d.Domain)
	if err != nil {
		return nil, err
	}
	drain := d.Drain
	if drain == nil {
		drain = func() float64 { return 0 }
	}
	now := d.Now
	if now == nil {
		now = time.Now
	}
	return &Reader{
		domain:  d.Domain,
		tables:  t,
		db:      d.DB,
		index:   d.Index,
		waiter:  d.Waiter,
		health:  d.Health,
		drain:   drain,
		metrics: d.Metrics,
		now:     now,
	}, nil
}

// Read serves one query, running fn inside the answer's own transaction.
//
// # The order, and why every step is where it is
//
//  1. The cheap LOCAL refusals, so a doomed read never appends: eviction,
//     then the floor, then a stall. Each is answered from what this node
//     already knows, for nothing.
//  2. COVERAGE, still before any append. A read whose objects this node
//     cannot answer for could never be certified however fresh it got, so it
//     must not spend a quorum round trip to find that out — and coverage is
//     DECIDED rather than waited for: the number it would wait on is pinned
//     below the checkpoint while a barrier is appended above it, so the wait
//     could not terminate.
//  3. The freshness target, by level. Only linearizable appends.
//  4. The WAIT, on this node's own checkpoint, holding no connection and no
//     transaction.
//  5. ONE read transaction, whose FIRST statement is the coverage probe
//     again — a deferred record landing between the two is the same hole
//     through another door — and then the answer.
func (r *Reader) Read(ctx context.Context, q Query, fn func(*sql.Tx) error) (Answer, error) {
	started := time.Now()
	if !q.Level.Valid() {
		return Answer{}, fmt.Errorf("statelog: %q is not a read level (want %v)",
			q.Level, ReadLevels)
	}
	h := r.health()

	// 1. THE LOCAL REFUSALS, in order.
	now := r.now()
	if code := h.Refusal(now); code != "" {
		// A CONSISTENT-PREFIX READ SURVIVES A STALL and nothing else
		// does: it asks only for a coherent point in the log's order,
		// which a frozen prefix still is. Every other refusal here says
		// this node's rows are wrong rather than old.
		if code != RefuseStalled || q.Level != ReadConsistentPrefix {
			return r.refuse(h, q, code, r.refusalDetail(h, code, now), started)
		}
	}

	// 2. COVERAGE, before any append.
	answer := Answer{Level: q.Level, Position: h.Position, Lag: h.Lag, Complete: true}
	if h.Deferred > 0 {
		gap, err := r.coverage(ctx, q.Scope)
		if err != nil {
			return r.refuse(h, q, RefuseDeferredScopeUnknown, err.Error(), started)
		}
		if gap != nil {
			if !q.Set {
				return r.refuse(h, q, RefuseDeferred,
					fmt.Sprintf("this node retains %s, covering what this read "+
						"is about", gap.found.Describe(r.domain.RecordVersion())), started)
			}
			// A SET READ CONTINUES AND CLAIMS NOTHING. It cannot
			// enumerate what would have entered the set, so the
			// honest answer is the rows it has plus the fact that
			// there is a gap.
			answer.Complete = false
			answer.Incomplete = &Incomplete{
				Records:   gap.records,
				From:      gap.found.Position,
				Scope:     gap.found.Scope,
				Direction: "unknown",
				Version:   gap.found.Version,
			}
		}
	}

	// 3. The freshness target.
	target, err := r.target(ctx, q, h)
	if err != nil {
		var refusal *Refused
		if errors.As(err, &refusal) {
			return r.refuse(h, q, refusal.Code, refusal.Detail, started)
		}
		return Answer{}, err
	}

	// 4. The wait, holding nothing.
	if !target.IsZero() {
		if target.Stream != "" && target.Stream != r.domain.Stream().Name {
			return r.refuse(h, q, RefuseWrongStream,
				fmt.Sprintf("this read names a position on %s and this domain "+
					"reads %s", target.Stream, r.domain.Stream().Name), started)
		}
		waitCtx, cancel := context.WithTimeout(ctx, ReadBudget)
		err := r.waiter.WaitCommitted(waitCtx, target)
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				return Answer{}, ctx.Err()
			}
			return r.refuse(h, q, RefuseBehind,
				fmt.Sprintf("this node has not reached %s within %s", target, ReadBudget),
				started)
		}
		answer.Position = r.waiter.Committed()

		// THE HEALTH IS RE-READ, and the bound is enforced again
		// against it.
		//
		// Everything above was decided from a snapshot taken before a
		// wait that may have lasted the whole of [ReadBudget]. What
		// ended that wait is this node reaching a position; the log ran
		// on meanwhile, and on a busy company it ran on by more than
		// the caller said it would accept. Reporting the pre-wait lag
		// beside post-wait rows is the same defect the bound itself was
		// fixed for once: a number that describes a different moment
		// from the answer it is printed beside.
		post := r.health()
		answer.Lag = post.Lag
		if refusal := r.pastBound(q, post); refusal != nil {
			return r.refuse(post, q, refusal.Code, refusal.Detail, started)
		}
	}

	// 5. ONE transaction, coverage first.
	if err := r.db.Read(ctx, func(tx *sql.Tx) error {
		if h.Deferred > 0 && !q.Set {
			// THE SAME HOLE THROUGH ANOTHER DOOR. A deferred record
			// landing between the probe above and this transaction
			// would otherwise be invisible to both.
			if d, hit, err := r.tables.deferredIn(ctx, tx, q.Scope); err != nil {
				return err
			} else if hit {
				return &Refused{
					Code: RefuseDeferred, Level: q.Level,
					Detail: fmt.Sprintf("%s landed while this read was waiting",
						d.Describe(r.domain.RecordVersion())),
				}
			}
		}
		return fn(tx)
	}); err != nil {
		var refusal *Refused
		if errors.As(err, &refusal) {
			return r.refuse(h, q, refusal.Code, refusal.Detail, started)
		}
		return Answer{}, err
	}

	r.observeServed(q.Level, started)
	return answer, nil
}

// gap is what a coverage probe found: how many retained records could
// intersect the read, and the earliest of them.
type gap struct {
	records uint64
	found   Deferral
}

// coverage probes this node's deferred scope index for anything covering what
// the read is about.
func (r *Reader) coverage(ctx context.Context, s ScopeSet) (*gap, error) {
	if s.Empty() {
		// A READ THAT NAMES NO OBJECTS cannot be certified about any,
		// so it is the domain term rather than a free pass.
		return nil, fmt.Errorf("this read declares no scope, so nothing can be " +
			"said about what a deferred record would cover")
	}
	var found *gap
	err := r.db.Read(ctx, func(tx *sql.Tx) error {
		d, n, hit, err := r.tables.meeting(ctx, tx, s)
		if err != nil || !hit {
			return err
		}
		found = &gap{records: n, found: d}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return found, nil
}

// Retained is the coverage probe a read makes, for a domain reader that
// certifies an answer from a transaction of its own: the earliest record this
// node retains whose declared scope meets s, how many retained records meet
// it, and false when none does.
//
// THE FRAMEWORK'S OWN PROBE, run in the caller's transaction so it describes
// the rows that transaction reads. A domain that wrote the SQL again would own
// a second copy of the one predicate whose clauses decide what a refusal
// covers: counted over the scope index's rows rather than over records, a
// record meeting a read on two of its paths is two records, and the earliest
// record's version is not the smallest version among them.
func Retained(ctx context.Context, tx *sql.Tx, d Domain, s ScopeSet) (Deferral, uint64, bool, error) {
	t, err := newTables(d)
	if err != nil {
		return Deferral{}, 0, false, err
	}
	return t.meeting(ctx, tx, s)
}

// target is the position this read must wait for, by level.
func (r *Reader) target(ctx context.Context, q Query, h Health) (Position, error) {
	// THE CALLER'S FLOOR NAMES A STREAM, AND IT IS CHECKED BEFORE ANY
	// LEVEL LOOKS AT IT.
	//
	// [Query.MinPosition] is the one value here that came off a wire; the
	// session mark and the barrier are this domain's own. A position is a
	// triple, but [Position.Packed] deliberately carries only two of it —
	// the stream is not in the number — so comparing a foreign floor
	// against a local target is comparing coordinates from two number
	// spaces. Selecting the maximum first therefore DISCARDS a foreign
	// floor whenever it happens to sort low (`OTHER@0:0` against any live
	// barrier), and the post-selection guard below then inspects a purely
	// local position and waves it through: the read is served as though no
	// floor was named, labelled with the level the caller asked for.
	//
	// Refusing here instead makes the answer honest at every level, and it
	// is the caller's bug rather than a state that clears — see
	// [RefuseWrongStream].
	if !q.MinPosition.IsZero() && q.MinPosition.Stream != "" &&
		q.MinPosition.Stream != r.domain.Stream().Name {

		return Position{}, &Refused{
			Code: RefuseWrongStream, Level: q.Level,
			Detail: fmt.Sprintf("this read floors at a position on %s and this "+
				"domain reads %s — a position names the log it is a position "+
				"in, and this one is not from this log",
				q.MinPosition.Stream, r.domain.Stream().Name),
		}
	}
	switch q.Level {
	case ReadLinearizable:
		if r.index == nil {
			return Position{}, &Refused{
				Code: RefuseBrokerUnreachable, Level: q.Level,
				Detail: "this node has no read index, so no position can be " +
					"established as the log's end",
			}
		}
		at, err := r.index.Read(ctx)
		if err != nil {
			return Position{}, barrierRefusal(q.Level, err)
		}
		// THE FLOOR CANNOT BE PAST THE BARRIER on the stream the barrier
		// was appended to — a position a write returned was acknowledged
		// before this append was — so on the honest path this is the
		// barrier. On the other path the floor names a sequence this
		// stream has not reached, and waiting for it is what turns a
		// pasted position into a `behind` refusal the caller can read
		// rather than an answer served from before it.
		return furthest(at, q.MinPosition), nil

	case ReadSession:
		// THE STRONGEST OF WHAT THE CALLER ALREADY KNOWS. A session
		// read that found nothing to wait for is a stale read wearing a
		// stronger name, so it waits for nothing rather than appending
		// a barrier nobody asked for — which is why the grammars refuse
		// the level to a caller that named no position.
		return furthest(q.Session, q.MinPosition), nil

	case ReadStale, ReadConsistentPrefix:
		if refusal := r.pastBound(q, h); refusal != nil {
			return Position{}, refusal
		}
		// AND THE FLOOR STILL HOLDS. A stale read with a position is
		// "whatever this node holds, from here on": the lag is checked
		// above and reported on the answer, and the wait below is for
		// the caller's own write rather than for the log's end.
		return q.MinPosition, nil
	}
	return Position{}, fmt.Errorf("statelog: unreachable read level %q", q.Level)
}

// pastBound is the staleness bound's whole rule, against ONE health snapshot.
//
// # Why it is a function
//
// Because the bound is applied to TWO snapshots and there must be one rule for
// both: the one taken before the read waits, and the one taken after. A read
// may wait for the caller's own floor ([Query.MinPosition]) for up to
// [ReadBudget], a wait whose ending says this node reached a position and says
// nothing whatever about how far the log has run on in the meantime. Checked
// only before the wait, a caller declaring `max_lag_seq=250` would be served
// an answer assembled when the node was thousands behind, with `Lag` reporting
// the figure from before the wait: the bound checked, the answer past it, and
// the number beside the rows agreeing with neither.
//
// A nil return is "within the bound", which includes a read that declared no
// bound at all — zero accepts anything, and that is what makes declaring one
// the caller's own decision.
func (r *Reader) pastBound(q Query, h Health) *Refused {
	if q.MaxLag <= 0 && q.MaxLagSeq == 0 {
		return nil
	}
	if h.Lag == nil {
		return &Refused{
			Code: RefuseBrokerUnreachable, Level: q.Level,
			Detail: "this read bounds its staleness and the broker could " +
				"not say how far behind this node is",
		}
	}
	// THE RECORD COUNT FIRST, because it is the reading the broker gave:
	// the duration below is derived from it through this node's own drain
	// rate, so a bound stated in records is checked against the number
	// itself rather than against an estimate made from it.
	if q.MaxLagSeq > 0 && *h.Lag > q.MaxLagSeq {
		return &Refused{
			Code: RefuseTooStale, Level: q.Level,
			Detail: fmt.Sprintf("this node is %d records behind and this "+
				"read accepts %d", *h.Lag, q.MaxLagSeq),
		}
	}
	// THE DURATION IS THE RECORD COUNT OVER THIS NODE'S MEASURED DRAIN,
	// through [BacklogTime], the one conversion every backlog stated as a
	// time takes — fraction of a rate and all.
	behind := BacklogTime(*h.Lag, r.drain())
	if q.MaxLag > 0 && behind > q.MaxLag {
		return &Refused{
			Code: RefuseTooStale, Level: q.Level,
			Detail: fmt.Sprintf("this node is about %s behind and this read "+
				"accepts %s", behind.Round(time.Second), q.MaxLag),
		}
	}
	return nil
}

// barrierRefusal maps the read index's own failures onto refusal codes.
//
// They are genuinely different: a barrier no majority committed, an ingest
// queue that was full, a log that is full, and a broker that refused the
// append for a reason of its own. The first two are worth coming back for, and
// the other two name a setting an operator has to change — so each is read
// from the refusal [ReadIndex] made out of the broker's answer, and anything
// that is not one is the barrier that did not commit within its budget.
func barrierRefusal(level ReadLevel, err error) error {
	const answering = " — it costs every level that appends, while `stale` and " +
		"`session` keep answering"
	var unavailable *Unavailable
	if errors.As(err, &unavailable) {
		switch unavailable.Reason {
		case ReasonBusy:
			return &Refused{Code: RefuseBrokerBusy, Level: level,
				Detail: unavailable.Detail + answering}
		case ReasonLogFull:
			return &Refused{Code: RefuseLogFull, Level: level,
				Detail: unavailable.Detail + answering}
		case ReasonTooLarge, ReasonRefused:
			return &Refused{Code: RefuseBarrierRefused, Level: level,
				Detail: unavailable.Detail + answering}
		case ReasonSkew:
			return &Refused{Code: RefuseNoQuorum, Level: level, Detail: unavailable.Detail}
		}
	}
	return &Refused{
		Code: RefuseNoQuorum, Level: level,
		Detail: err.Error(),
	}
}

// refuse records and returns one refusal.
func (r *Reader) refuse(h Health, q Query, code ReadRefusal, detail string, started time.Time) (Answer, error) {
	lag := uint64(0)
	if h.Lag != nil {
		lag = *h.Lag
	}
	r.watch.refused(q.Level, code, r.now())
	if r.metrics != nil {
		r.metrics.Add(metrics.StatelogReadRefusals, 1, metrics.Attrs{
			"domain": r.domain.Name(), "level": string(q.Level), "code": string(code),
		})
		r.metrics.Observe(metrics.StatelogReadWait, time.Since(started), metrics.Attrs{
			"domain": r.domain.Name(), "level": string(q.Level),
		})
	}
	return Answer{Level: q.Level}, &Refused{
		Code:       code,
		Level:      q.Level,
		Detail:     detail,
		RetryAfter: RetryHint(code, lag, r.drain()),
	}
}

// refusalDetail says what an operator has to do about a local refusal.
func (r *Reader) refusalDetail(h Health, code ReadRefusal, now time.Time) string {
	switch code {
	case RefuseEvicted:
		return "this node has been removed from the fleet; an operator readmits it"
	case RefuseBrokerUnreachable:
		return "the broker did not answer this node's read of the log's bounds, " +
			"which every freshness term is measured against: " + h.BrokerErr
	case RefuseFloorUnknown:
		if h.Floor.ReadAt.IsZero() {
			if h.Err != "" {
				// THE HEALTH READ ITSELF FAILED before it established the
				// floor, and its error is the one statement of why — an
				// unanswered coordination read, or a read of this node's
				// own store that came before the floor.
				return h.Err
			}
			return "the published trim floor has never been read on this node, " +
				"which is UNKNOWN rather than satisfied"
		}
		return fmt.Sprintf("the published trim floor was last read %s ago, past "+
			"the %s a cached value stays credible for — an unreadable floor is "+
			"UNKNOWN rather than satisfied",
			h.Floor.Age(now).Round(time.Second), FloorCacheStale)
	case RefuseGenerationLeft:
		if h.Err != "" {
			// The health read's own words name both generations.
			return h.Err
		}
		return "the published trim floor is at a generation this node's rows " +
			"are not on; this node adopting a snapshot of the fleet's " +
			"generation is what clears it"
	case RefuseBelowFloor:
		return "records this node never applied have been trimmed, so its rows " +
			"are missing state no replay can supply; it adopts a peer's snapshot"
	case RefuseStalled:
		if h.Err != "" {
			return h.Err
		}
		return fmt.Sprintf("this node's applied prefix has not moved for %s, so "+
			"its rows are frozen rather than merely old", StallGrace)
	}
	return ""
}

func (r *Reader) observeServed(level ReadLevel, started time.Time) {
	r.watch.served(level)
	if r.metrics == nil {
		return
	}
	r.metrics.Add(metrics.StatelogReadServed, 1, metrics.Attrs{
		"domain": r.domain.Name(), "level": string(level),
	})
	r.metrics.Observe(metrics.StatelogReadWait, time.Since(started), metrics.Attrs{
		"domain": r.domain.Name(), "level": string(level),
	})
}
