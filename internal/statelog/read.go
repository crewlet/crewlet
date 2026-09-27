package statelog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
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

	// RefuseDeferred — this node holds a record it cannot decode covering
	// what this read is about. Another node can answer; this one cannot,
	// and no amount of waiting changes that.
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
	// trimmed, so its rows are missing state no replay can supply. It
	// clears when the node adopts a peer's snapshot, which it does on its
	// own.
	RefuseBelowFloor ReadRefusal = "below_floor"

	// RefuseFloorUnknown — the published floor could not be read, and the
	// third value BLOCKS. Guessing here keeps a node serving over a hole
	// it cannot see. It clears the next time the floor is read.
	RefuseFloorUnknown ReadRefusal = "floor_unknown"

	// RefuseEvicted — this node has been removed from the fleet.
	RefuseEvicted ReadRefusal = "evicted"

	// RefuseBrokerUnreachable — the broker did not answer, so no read
	// index could be established.
	RefuseBrokerUnreachable ReadRefusal = "broker_unreachable"

	// RefuseNoQuorum — the barrier did not commit. It is not the same as
	// unreachable: the broker answered, and a majority did not agree.
	RefuseNoQuorum ReadRefusal = "no_quorum"

	// RefuseLogFull — the log is at its byte ceiling and refuses appends,
	// so a barrier cannot be written. A full log therefore costs the
	// levels that append; the ones that do not keep answering.
	RefuseLogFull ReadRefusal = "log_full"

	// RefuseBrokerRefused — the broker refused to store the barrier and
	// named a reason this framework has no remedy for: a sealed stream, a
	// JetStream store with no resources left. The levels that append are
	// refused until somebody acts on the broker's words, and the ones that
	// do not keep answering. Not a missed quorum: the broker answered.
	RefuseBrokerRefused ReadRefusal = "broker_refused"

	// RefuseWrongStream — this node's rows are keyed to a log that is not
	// the one the broker now holds: its checkpoint is past the log's end,
	// or the stream was deleted and rebuilt under it — or a position this
	// node derived for the read, a session mark or a barrier, is on another
	// log. A STATE of this node, which an operator answers by re-anchoring
	// — see [Health.Refusal].
	//
	// A CALLER'S floor on another log is not this refusal. It is the
	// request's mistake, which every node answers the same, so it is
	// [ErrForeignFloor] and not a refusal at all.
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
	RefuseBelowFloor, RefuseFloorUnknown, RefuseEvicted, RefuseBrokerUnreachable,
	RefuseNoQuorum, RefuseLogFull, RefuseBrokerRefused, RefuseWrongStream,
	RefuseTooStale,
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
//
// A FLOOR THAT COULD NOT BE READ IS READ AGAIN, and a node BELOW the floor
// rejoins by adopting a peer's snapshot on its own — the position heartbeat
// requests it — so both clear without anybody doing anything, which is what
// their write-side twins ([ReasonFloorUnknown], [ReasonBelowFloor]) already
// said. Answered as final here, a coordination blip long enough to age the
// cached floor told every reader on the node to stop asking, while every
// writer on it was told to come back.
func (r ReadRefusal) Retryable() bool {
	switch r {
	case RefuseBehind, RefuseNoQuorum, RefuseBrokerUnreachable, RefuseStalled,
		RefuseBelowFloor, RefuseFloorUnknown:
		return true
	}
	return false
}

// ElectionRetryHint is what a caller is told to wait after a barrier that did
// not commit.
//
// FOUR SECONDS, which is the broker's own minimum election timeout: a barrier
// that failed to reach a quorum is waiting on an election, and a hint shorter
// than the election itself sends every reader back before anything can have
// changed.
const ElectionRetryHint = 4 * time.Second

// Refused is a read that was not served.
type Refused struct {
	// Code says what happened and what to do about it.
	Code ReadRefusal

	// Level is the level that was asked for.
	Level ReadLevel

	// Detail names the specific thing — the deferred record's version and
	// position, the field an operator has to change. Written for a CALLER,
	// since every surface sends it on, so it is never an error's own text
	// — see [Unavailable]'s detail, held to the same rule.
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
// up in milliseconds. Zero means no hint at all, which is what a refusal
// waiting cannot clear deserves.
func RetryHint(code ReadRefusal, lag uint64, recordsPerSecond float64) time.Duration {
	if !code.Retryable() {
		return 0
	}
	switch code {
	case RefuseNoQuorum, RefuseBrokerUnreachable:
		return ElectionRetryHint
	case RefuseFloorUnknown, RefuseBelowFloor:
		// ONE HEARTBEAT, and never the drain: neither clears by
		// applying what is ahead of this node. The floor is read again,
		// and a below-floor node's rejoin is requested, on the position
		// heartbeat — so nothing can have changed sooner than the next
		// one, and a drain-derived figure would be a lag this node
		// cannot replay across.
		return coord.ReconcileInterval
	}
	if recordsPerSecond <= 0 || lag == 0 {
		// Nothing measured, or nothing to catch up on. A hint derived
		// from a rate nobody observed is a constant wearing a
		// derivation's clothes.
		return ElectionRetryHint
	}
	seconds := float64(lag) / recordsPerSecond
	if math.IsInf(seconds, 0) || math.IsNaN(seconds) {
		return ElectionRetryHint
	}
	// Rounded up to a whole second, because a hint is read by a person
	// and by a retry loop and neither wants milliseconds.
	return time.Duration(math.Ceil(seconds)) * time.Second
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
	// Records is how many deferred records could intersect this read.
	Records uint64

	// From is the lowest of their positions.
	From Position

	// Scope is what they declared they touch.
	Scope ScopeSet

	// Direction is always "unknown", and it is a field rather than an
	// omission so a reader meets the fact rather than inferring it.
	Direction string

	// Version is the record version this node could not read, which is
	// the ONE number an operator needs to pick a build that can.
	//
	// It is already what the REFUSAL says on a point read — "a record at
	// version %d it cannot decode" — and it was dropped on the set-read
	// path, where the answer continues and the gap travels to a caller
	// instead. Every surface rendering it therefore showed version 0,
	// which is not a version any build ever wrote.
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

	// Drain is the observed records-per-second, which every retry hint
	// divides by. A hint derived from a rate nobody measured is a
	// constant wearing a derivation's clothes.
	Drain func() float64

	Metrics *metrics.Recorder
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
	return &Reader{
		domain:  d.Domain,
		tables:  t,
		db:      d.DB,
		index:   d.Index,
		waiter:  d.Waiter,
		health:  d.Health,
		drain:   drain,
		metrics: d.Metrics,
	}, nil
}

// Read serves one query, running fn inside the answer's own transaction.
//
// # The order, and why every step is where it is
//
//  0. The REQUEST: a level this build knows, and a floor on this read's own
//     log ([ErrForeignFloor]). Neither is a state of this node, so neither
//     waits behind one — every node refuses the same request the same way.
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
	// THE CALLER'S FLOOR NAMES A STREAM, and it is checked before any
	// level — or this node's own health — looks at anything.
	// [Query.MinPosition] is the one value here that came off a wire; the
	// session mark and the barrier are this domain's own.
	if err := floorOn(q.MinPosition, r.domain.Stream().Name); err != nil {
		return Answer{}, err
	}
	h := r.health()

	// 1. THE LOCAL REFUSALS, in order.
	now := time.Now()
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
			// A CALLER THAT GAVE UP is not a scope this node could not
			// read, exactly as it is not a node that fell behind in
			// the wait below.
			if ctx.Err() != nil {
				return Answer{}, ctx.Err()
			}
			return r.refuse(h, q, RefuseDeferredScopeUnknown, r.scopeUnread(err), started)
		}
		if gap != nil {
			if !q.Set {
				return r.refuse(h, q, RefuseDeferred,
					fmt.Sprintf("this node holds a record at version %d it cannot "+
						"decode, at %s, covering what this read is about",
						gap.version, gap.from), started)
			}
			// A SET READ CONTINUES AND CLAIMS NOTHING. It cannot
			// enumerate what would have entered the set, so the
			// honest answer is the rows it has plus the fact that
			// there is a gap.
			answer.Complete = false
			answer.Incomplete = &Incomplete{
				Records:   gap.records,
				From:      gap.from,
				Scope:     gap.scope,
				Direction: "unknown",
				Version:   gap.version,
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
		// THE CALLER'S FLOOR WAS HELD TO THIS LOG in step 0, so what
		// can still name another here is a position this node derived
		// for the read — a session mark, a barrier — which is this
		// node's own state rather than the request's.
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
					Detail: fmt.Sprintf("a record at version %d this node cannot "+
						"decode, at %s, landed while this read was waiting",
						d.Version, d.Position),
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

// gap is what a coverage probe found.
type gap struct {
	records uint64
	from    Position
	version int
	scope   ScopeSet
}

// errNoScope is a read that names no objects on a node holding a deferred
// record: it cannot be certified about any, so it is the domain term rather
// than a free pass. Its words are this package's own, and a caller is told
// them.
var errNoScope = errors.New("this read declares no scope, so nothing can be " +
	"said about what a deferred record would cover")

// coverage probes this node's deferred scope index for anything covering what
// the read is about.
func (r *Reader) coverage(ctx context.Context, s ScopeSet) (*gap, error) {
	if s.Empty() {
		return nil, errNoScope
	}
	var found *gap
	err := r.db.Read(ctx, func(tx *sql.Tx) error {
		d, hit, err := r.tables.deferredIn(ctx, tx, s)
		if err != nil {
			return err
		}
		if hit {
			found = &gap{records: 1, from: d.Position, version: d.Version, scope: d.Scope}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return found, nil
}

// scopeUnread is what a caller is told about a deferred scope this node could
// not read, and the one place the reason is sent to the log instead.
//
// A REFUSAL'S DETAIL IS WRITTEN FOR A CALLER — every surface that answers a
// refusal sends it on, because it names what the refusal is about and what
// changes it. The probe's own error is not that: it is the store's, a driver's
// words or a database path, and it reached every holder of the question's
// grant in a REST body and a socket frame the day refusals stopped being
// answered as faults. So the sentence is this package's, and the error is
// logged where an operator reads it.
func (r *Reader) scopeUnread(err error) string {
	if errors.Is(err, errNoScope) {
		return errNoScope.Error()
	}
	log.Warn("statelog_deferred_scope_unread", "domain", r.domain.Name(),
		"error", err.Error())
	return "the scope of a record this node cannot decode could not be read " +
		"from its store, so nothing can be said about what it covers — the " +
		"reason is in this node's log"
}

// target is the position this read must wait for, by level.
func (r *Reader) target(ctx context.Context, q Query, h Health) (Position, error) {
	// THE CALLER'S FLOOR IS ON THIS LOG: [Reader.Read] refused one that is
	// not before it got here, because comparing a foreign floor against a
	// local target is comparing coordinates from two number spaces — see
	// [floorOn].
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
			// A CALLER THAT GAVE UP IS NOT A MISSED QUORUM. The read
			// index answers a caller's own cancellation or deadline with
			// that context's error, and mapped onto `no_quorum` it was
			// counted as a refusal, handed an election's retry hint, and
			// — on a deadline the caller set — told a caller still
			// listening that the broker's members had not agreed.
			if ctx.Err() != nil {
				return Position{}, ctx.Err()
			}
			return Position{}, barrierRefusal(q.Level, r.index.subject, err)
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
// # Why it is a function and not the lines it replaced
//
// Because the bound has to be applied to TWO snapshots and there must be one
// rule for both. It was enforced only against the snapshot taken before the
// read waited — and a read can now wait for the caller's own floor
// ([Query.MinPosition]) for up to [ReadBudget], a wait whose ending says this
// node reached a position and says nothing whatever about how far the log has
// run on in the meantime. A caller declaring `max_lag_seq=250` could therefore
// be served an answer assembled when the node was thousands behind, with
// `Lag` reporting the figure from before the wait: the bound checked, the
// answer past it, and the number beside the rows agreeing with neither.
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
	behind := time.Duration(*h.Lag) * time.Second / time.Duration(max(int64(r.drain()), 1))
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
// The four are genuinely different: a majority that did not agree, a log that
// is full, a broker that refused the barrier for a reason of its own, and a
// duplicate answer that proves nothing. Only the first is worth coming back
// for; a full log names a field an operator has to change, and a broker's
// refusal names what the broker wants changed.
//
// A BARRIER NOBODY CONFIRMED IS TOLD IN THIS PACKAGE'S WORDS, never the
// error's. The error is the client's or the broker's own — a timeout, a
// connection closed, whatever the transport said — and a refusal's detail is
// sent to the caller by every surface that answers one, so it reached every
// holder of the question's grant the day refusals stopped being answered as
// faults. [barrierAppendFailed] logs the error once per append, where an
// operator reads it, rather than once per reader waiting on it.
func barrierRefusal(level ReadLevel, subject string, err error) error {
	var unavailable *Unavailable
	if errors.As(err, &unavailable) {
		switch unavailable.Reason {
		case ReasonLogFull:
			return &Refused{
				Code: RefuseLogFull, Level: level,
				Detail: unavailable.Detail + " — a full log costs every level " +
					"that appends, while `stale` and `session` keep answering",
			}
		case ReasonBrokerRefused:
			return &Refused{
				Code: RefuseBrokerRefused, Level: level,
				Detail: unavailable.Detail + " — every level that appends is " +
					"refused until it is acted on, while `stale` and `session` " +
					"keep answering",
			}
		case ReasonSkew:
			return &Refused{Code: RefuseNoQuorum, Level: level, Detail: unavailable.Detail}
		}
	}
	return &Refused{
		Code: RefuseNoQuorum, Level: level,
		Detail: fmt.Sprintf("the barrier on %s was not confirmed — the broker "+
			"did not answer its append, or a majority of its members did not "+
			"agree — so no position could be established as the log's end; "+
			"the broker's own error is in this node's log", subject),
	}
}

// refuse records and returns one refusal.
func (r *Reader) refuse(h Health, q Query, code ReadRefusal, detail string, started time.Time) (Answer, error) {
	lag := uint64(0)
	if h.Lag != nil {
		lag = *h.Lag
	}
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
	case RefuseFloorUnknown:
		if h.Floor.ReadAt.IsZero() {
			return "the published trim floor has never been read on this node, " +
				"which is UNKNOWN rather than satisfied"
		}
		return fmt.Sprintf("the published trim floor was last read %s ago, past "+
			"the %s a cached value stays credible for — an unreadable floor is "+
			"UNKNOWN rather than satisfied",
			h.Floor.Age(now).Round(time.Second), FloorCacheStale)
	case RefuseBelowFloor:
		return "records this node never applied have been trimmed, so its rows " +
			"are missing state no replay can supply; it adopts a peer's snapshot"
	case RefuseStalled:
		if h.Err != "" {
			// NOT h.Err, which is the applier's own error or the one a
			// health read failed with — a driver's message, a database
			// path — and a refusal's detail reaches every caller. An
			// operator reads the error where it already is.
			return "this node's applier has stopped, or has retried one " +
				"failure for longer than its budget, so its rows are frozen " +
				"rather than merely old — `crewlet retention status` names the " +
				"domain, its position and the error, which this node's log " +
				"carries too"
		}
		return fmt.Sprintf("this node's applied prefix has not moved for %s, so "+
			"its rows are frozen rather than merely old", StallGrace)
	}
	return ""
}

func (r *Reader) observeServed(level ReadLevel, started time.Time) {
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
