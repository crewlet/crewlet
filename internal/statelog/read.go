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
	"github.com/crewlet/crewlet/internal/jsprovision"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
	"github.com/crewlet/crewlet/internal/words"
)

// ReadLevel is how fresh an answer has to be: four levels for the four
// questions a caller can ask.
type ReadLevel string

const (
	// ReadLinearizable is "no answer from before this read arrived". It
	// costs a quorum-committed append and a wait, and it is what a caller
	// asks for when the answer decides something.
	ReadLinearizable ReadLevel = "linearizable"

	// ReadSession is "no answer from before MY last write", free when the
	// caller is caught up. It waits for the position the caller's write landed
	// at ([Query.Session] inside the engine, [Query.Floors] from outside);
	// handed none, it waits for nothing and serves this node's prefix under a
	// stronger name, which is why no read grammar accepts it without a
	// `min_position`.
	ReadSession ReadLevel = "session"

	// ReadStale is "whatever this node holds, with its lag reported". It
	// takes no broker call at all, which is why it keeps answering when a
	// full log has stopped every level that does.
	ReadStale ReadLevel = "stale"

	// ReadConsistentPrefix is "a coherent point in the log's own order,
	// possibly behind". NO SURFACE EVER DEFAULTS TO IT: it is weaker than
	// session invisibly in the answer, so a default would silently downgrade
	// every reader that did not ask for more.
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
	// needs. It clears on its own, and the hint says roughly when. That
	// includes a node replaying up to the published trim floor through records
	// the log still holds ([FloorReplaying]).
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

	// RefuseBelowFloor — records this node never applied are gone from
	// the log, so its rows are missing state no replay can supply. It
	// clears when the node adopts a peer's snapshot, which it does on its
	// own; another node can answer meanwhile.
	RefuseBelowFloor ReadRefusal = "below_floor"

	// RefuseFloorUnknown — the published floor could not be read, and the
	// third value BLOCKS. Guessing here keeps a node serving over a hole
	// it cannot see. It clears the next time the floor is read.
	RefuseFloorUnknown ReadRefusal = "floor_unknown"

	// RefuseEvicted — this node has been removed from the fleet.
	RefuseEvicted ReadRefusal = "evicted"

	// RefuseBrokerUnreachable — the broker did not answer, so no read
	// index could be established: neither the barrier a linearizable read
	// appends nor this node's reading of its log's ends and eviction row
	// ([ReaderDeps.Health]).
	RefuseBrokerUnreachable ReadRefusal = "broker_unreachable"

	// RefuseNoQuorum — the barrier on a log SEVERAL members store did not
	// commit: in the read index's own budget, or in what a served request's
	// asker left it ([WithReplyMargin]). A majority did not agree in that
	// time, or no leader answered — an election, a member down or cut off.
	// Never a log one member stores, which has no majority to miss
	// ([ReadIndex.unconfirmed]).
	RefuseNoQuorum ReadRefusal = "no_quorum"

	// RefuseBrokerStalled — the barrier on a log ONE member stores was not
	// acknowledged in the read index's budget, or in what a served request's
	// asker left it: the member's own write did not complete (a disk not
	// taking the fsync `stream.sync: always` makes, or a broker that stopped
	// serving the stream). Every node's barrier goes to that member, so no
	// other node is better placed ([ReadRefusal.Elsewhere]); this one is, once
	// the write completes.
	RefuseBrokerStalled ReadRefusal = "broker_stalled"

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
	// the one the broker now holds (its checkpoint is past the log's end, or
	// the stream was rebuilt under it), or a position this node derived for
	// the read is on another log: a STATE of this node, answered by
	// re-anchoring ([Health.Refusal]). A CALLER'S floor on another log is the
	// request's mistake, [ErrForeignPosition], not a refusal.
	RefuseWrongStream ReadRefusal = "wrong_stream"

	// RefuseTooStale — this node's lag is past what the caller said it
	// would accept.
	RefuseTooStale ReadRefusal = "too_stale"

	// RefuseMaintenance — this node runs in a mode that publishes nothing
	// ([MaintenanceMode.Publishes]), and a linearizable read APPENDS a
	// barrier; the other levels keep answering. Not retryable: the mode is the
	// process's for its life, and every node of a fleet in a capacity window
	// is in one.
	RefuseMaintenance ReadRefusal = "maintenance"
)

// ReadRefusals are every code. A count of them is derived from this slice,
// never written in prose beside it, where nobody updates it.
var ReadRefusals = []ReadRefusal{
	RefuseBehind, RefuseDeferred, RefuseDeferredScopeUnknown, RefuseStalled,
	RefuseBelowFloor, RefuseFloorUnknown, RefuseEvicted, RefuseBrokerUnreachable,
	RefuseNoQuorum, RefuseBrokerStalled, RefuseLogFull, RefuseBrokerRefused,
	RefuseWrongStream, RefuseTooStale, RefuseMaintenance,
}

// Valid reports whether a refusal code off the wire is one this build knows.
func (r ReadRefusal) Valid() bool { return slices.Contains(ReadRefusals, r) }

// Retryable reports whether coming back to THIS node could produce a
// different answer: a hint, where [ReadRefusal.Elsewhere] is a redirect. A
// node behind catches up; one holding a record it cannot decode never
// will, and a hint there loops forever. An unread floor is read again, and
// a node below the floor adopts a peer's snapshot on its own (the position
// heartbeat requests it), so both are retryable, as their write-side twins
// ([ReasonFloorUnknown], [ReasonBelowFloor]) are.
func (r ReadRefusal) Retryable() bool {
	switch r {
	case RefuseBehind, RefuseNoQuorum, RefuseBrokerStalled, RefuseBrokerUnreachable,
		RefuseStalled, RefuseBelowFloor, RefuseFloorUnknown:
		return true
	}
	return false
}

// Elsewhere reports whether ANOTHER node may serve a read this one refused
// with r: every code but four facts about the whole fleet — `log_full` and
// `broker_refused` (every node appends its barrier to the one log),
// `broker_stalled` (a log one member stores takes every barrier through
// that member's one write path, [Spent.Unconfirmed]) and `maintenance`
// (every node of a fleet in a capacity window is in one). A node that
// cannot decode a record never serves the read while a peer may at once;
// `wrong_stream` may be this node's rows alone. A code every node gives
// alike costs the router only a question per node, never a different
// answer: the refusal carries what the read paid for ([Refused.Spent],
// [WithSpent]), so the walk appends at most one barrier and waits at most
// one bound.
func (r ReadRefusal) Elsewhere() bool {
	switch r {
	case RefuseLogFull, RefuseBrokerRefused, RefuseBrokerStalled, RefuseMaintenance:
		return false
	}
	return r.Valid()
}

// ElectionRetryHint is what a caller is told to wait after a barrier several
// members did not commit, or a broker that did not answer: the broker's own
// minimum election timeout, since a shorter hint sends readers back before
// the election can have changed anything. A log one member stores holds no
// election ([StalledRetryHint]).
const ElectionRetryHint = 4 * time.Second

// StalledRetryHint is what a caller is told to wait after a barrier the sole
// member of its log did not acknowledge in time (`broker_stalled`): one
// barrier budget, the one thing measured about the stall. A reader sent back
// sooner only waits out a budget of its own behind the same held write. Not
// an election's four seconds: there is no election.
const StalledRetryHint = SoloBarrierBudget

// Refused is a read that was not served.
type Refused struct {
	// Code says what happened and what to do about it.
	Code ReadRefusal

	// Level is the level that was asked for.
	Level ReadLevel

	// Detail names the specific thing — the deferred record's version and
	// position, the field an operator has to change — for WHOEVER RUNS THE
	// NODE: it goes to the node's log and tooling, never to a screen, which
	// shows [ConditionSentence]'s words. Never an error's own text, as
	// [Unavailable]'s detail.
	Detail string

	// RetryAfter is derived where it can be ([RetryHint]). Zero means no
	// hint, the honest answer for a refusal waiting cannot clear.
	RetryAfter time.Duration

	// Spent is what this read had paid for when refused (its read index, the
	// position it waited its whole bound for, the log a barrier ran out on),
	// which a router carries to the next node ([WithSpent]) so the walk pays
	// for each once.
	Spent Spent
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

// RetryHint derives how long a caller should wait before coming back, from
// the observed drain: a flat hint is too early on a node grinding a long
// backlog and too late on one that caught up at once. Zero means no hint,
// which a refusal waiting cannot clear deserves.
func RetryHint(code ReadRefusal, lag uint64, recordsPerSecond float64) time.Duration {
	if !code.Retryable() {
		return 0
	}
	switch code {
	case RefuseNoQuorum, RefuseBrokerUnreachable:
		return ElectionRetryHint
	case RefuseBrokerStalled:
		return StalledRetryHint
	case RefuseFloorUnknown, RefuseBelowFloor:
		// ONE HEARTBEAT, never the drain: neither clears by applying
		// what is ahead. The floor is read again, and a below-floor
		// node's rejoin requested, on the position heartbeat.
		return coord.ReconcileInterval
	}
	if recordsPerSecond <= 0 || lag == 0 {
		// Nothing measured, or nothing to catch up on: a hint from a
		// rate nobody observed is a constant in disguise.
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

	// Resolve replaces Scope for a read whose object is named by a REFERENCE
	// (a key, an alias, an address) rather than the path its records are
	// filed under; exactly one of the two is set, and [Query.Set] still
	// decides what a deferral does. A path cannot be formed from a reference:
	// task "ENG-12" is filed under `t/c/ENG/o/<uuid>`, and a scope built as
	// though the key were the id holds nothing, so the probe passes every
	// deferral. The resolver reads the rows INSIDE THE TRANSACTION THE ANSWER
	// IS READ IN: a scope resolved in another would certify an object a rename
	// had moved. A reference resolving to nothing still answers the widest
	// scope it bounds (an address, or the domain): "not found" is a claim
	// about rows too, and a deferred create is a row this node lacks.
	Resolve Resolver

	// Session is the caller's own high-water mark on this stream, which
	// is what a session read waits for.
	Session Position

	// Floors are positions a caller names: one a wake carried, or its own
	// previous write's. They are honoured at EVERY level ([Freshness]): the
	// read waits for the later of the floor on this log and the level's own
	// target. EVERY ONE IS ON THIS LOG or the read is refused
	// ([ErrForeignPosition]): a read of several logs hands each half only its
	// own ([Freshness.On]), so a floor on the wrong half was misplaced by its
	// composer, refused rather than dropped.
	Floors []Position

	// MaxLag is what a stale read says it will accept, and zero means it
	// accepts anything.
	MaxLag time.Duration

	// MaxLagSeq is the same bound in RECORDS, the unit the broker answers
	// in ([Health.Lag]); MaxLag is derived from it through this node's drain
	// rate. Both may be set, and the read refuses on WHICHEVER IS REACHED
	// FIRST: they are two readings of one distance.
	MaxLagSeq uint64

	// Set reports a read whose answer is a SET rather than one object, which
	// changes what a deferred scope does: a point read refuses, since its
	// object may be stale; a set read cannot enumerate what would have
	// ENTERED the set (a deferred create has no local row), so it is served
	// and makes no completeness claim.
	Set bool
}

// Resolver answers the scope a point read is about by reading the rows its
// reference resolves to. See [Query.Resolve].
type Resolver func(ctx context.Context, tx *sql.Tx) (ScopeSet, error)

// scopeIn is the scope a query is about, resolved inside tx when it names its
// object by reference.
func (q Query) scopeIn(ctx context.Context, tx *sql.Tx) (ScopeSet, error) {
	if q.Resolve == nil {
		return q.Scope, nil
	}
	s, err := q.Resolve(ctx, tx)
	if err != nil {
		return ScopeSet{}, fmt.Errorf("statelog: resolve what this read is about: %w", err)
	}
	return s, nil
}

// Answer is what a read returns beside its rows.
type Answer struct {
	// Level is the level the answer was actually served at, which is the
	// one asked for or a refusal.
	Level ReadLevel

	// Position is the position the answer's rows are current through: this
	// node's reached position when it read them ([Runner.Reached]) — every
	// record at or below it committed, or a barrier that writes no row.
	Position Position

	// Lag is how far behind the log this answer is, and nil when the
	// broker could not say.
	Lag *uint64

	// Complete reports whether the answer is about every row that matches,
	// SEPARATE FROM Level: an answer can be fresh and incomplete (a deferred
	// create leaves no local trace).
	Complete bool

	// Incomplete says what is missing when Complete is false.
	Incomplete *Incomplete
}

// Incomplete describes a set answer's gap, with NO IDENTIFIERS: listing the
// objects a deferred record touched tells neither the DIRECTION of the
// difference nor reliably the right ids, and truncates. A caller can act on
// there being a gap, where it starts, and its scope.
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

	// Version is the record version this node could not read: the ONE
	// number an operator needs to pick a build that can, as a point read's
	// refusal names it.
	Version int
}

// Reader answers reads at a level, over one domain.
type Reader struct {
	domain  Domain
	mode    MaintenanceMode
	stream  string
	tables  tables
	db      readStore
	index   *ReadIndex
	waiter  Waiter
	health  func() (Health, error)
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

	// Spec is the stream this reader answers for: the domain's own
	// ([Domain.Stream]) with this node's ceiling. A read's positions, its
	// barrier and the deferrals its coverage probe reads are all that
	// stream's.
	Spec StreamSpec

	// Mode is what this node started for, and decides whether a read may
	// append. A BARRIER IS A PUBLISH, and a mode that publishes nothing
	// exists so the logs hold still while a capacity window measures them. In
	// such a mode a linearizable read is refused [RefuseMaintenance], the
	// reader gets no [ReadIndex] ([NewReader] refuses one), and every other
	// level is served. REQUIRED: the zero mode is refused, since either
	// reading of it is wrong in one posture.
	Mode MaintenanceMode

	DB     readStore
	Index  *ReadIndex
	Waiter Waiter

	// Health is this domain's readiness, asked on every read and again after
	// one that waited, so it must cost no round trip. Broker-held terms may be
	// served from memory, each AGING at its own bound: the floor carries its
	// read instant ([Floor]) and past [FloorCacheStale] is [FloorUnknown]; the
	// ends ([Health.FirstSeq], [Health.LastSeq], [Health.Lag]) no older than
	// the provider's refresh (the engine's: re-read each second, at most three
	// seconds old with the [ReadBudget] that bounds it), past which no health
	// is served. AN ERROR IS A HEALTH NOBODY COULD READ, refused
	// [RefuseBrokerUnreachable] at every level; its own return, since folded
	// into [Health.Err] (an applier's stop) it names the wrong code. Where only
	// the floor is unread, a provider hands the last one with its instant, and
	// the read refuses [RefuseFloorUnknown].
	Health func() (Health, error)

	// Drain is the observed records-per-second every retry hint divides by
	// ([RetryHint]).
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
	case !d.Mode.Valid():
		return nil, fmt.Errorf("statelog: reader has no mode (%q; want one of %s): "+
			"whether a read may append a barrier turns on it", d.Mode, words.Or(MaintenanceModes))
	case !d.Mode.Publishes() && d.Index != nil:
		return nil, fmt.Errorf("statelog: a reader in %s mode was given a read index, "+
			"and a read index appends barriers to a log this mode publishes nothing to",
			d.Mode)
	}
	t, err := newTables(d.Domain, d.Spec)
	if err != nil {
		return nil, err
	}
	drain := d.Drain
	if drain == nil {
		drain = func() float64 { return 0 }
	}
	return &Reader{
		domain:  d.Domain,
		mode:    d.Mode,
		stream:  d.Spec.Name,
		tables:  t,
		db:      d.DB,
		index:   d.Index,
		waiter:  d.Waiter,
		health:  d.Health,
		drain:   drain,
		metrics: d.Metrics,
	}, nil
}

// Read serves one query, running fn inside the answer's own transaction, in
// this order: 0, the REQUEST (a known level, floors on this log
// [Position.On]), refused alike by every node before any state of its own;
// 1, the free LOCAL refusals in [Health.Refusal]'s order, so a doomed read
// never appends; 2, COVERAGE, still before any append and DECIDED, not
// waited for (its number is pinned below the checkpoint while a barrier
// lands above it, so the wait would never end); 3, the freshness target,
// only linearizable appending; 4, the WAIT on the reached position
// ([Runner.Reached]), holding no transaction, which every linearizable
// read takes ([waitsThrough]) since it stamps and bounds the answer; 5, ONE
// read transaction whose first statement probes coverage again, since a
// deferral can land between the two.
func (r *Reader) Read(ctx context.Context, q Query, fn func(*sql.Tx) error) (Answer, error) {
	return r.serveRead(ctx, q, func(tx *sql.Tx, _ Position) error { return fn(tx) })
}

// ReadThrough is [Reader.Read] for a domain that STAMPS ITS ANSWER WITH A
// POSITION ITSELF (a cache's snapshot, a later page's floor): fn is handed
// this domain's checkpoint row as the answer's transaction reads it, its
// position carried to the reached position the read was released at
// ([currentThrough]). Neither alone will do. The checkpoint LAGS: a barrier
// is reached when decoded and committed later ([checkpoints]), so two
// linearizable reads on two nodes could be stamped in the wrong order. The
// released position is EARLY: the applier can commit before the
// transaction opens. The later names exactly the rows, since every record
// between them is a barrier, which writes no row ([writesNoRow]). A carried
// position has a zero [Checkpoint.StoredAt]: no row names its instant. The
// answer's Position is the released one.
func (r *Reader) ReadThrough(ctx context.Context, q Query,
	fn func(*sql.Tx, Checkpoint) error) (Answer, error) {

	return r.serveRead(ctx, q, func(tx *sql.Tx, released Position) error {
		row, _, err := r.tables.readCursor(ctx, tx)
		if err != nil {
			return err
		}
		cp := Checkpoint{At: currentThrough(row.at, released), KeyedTo: row.created, StoredAt: row.storedAt}
		if cp.At != row.at {
			cp.StoredAt = time.Time{}
		}
		return fn(tx, cp)
	})
}

// currentThrough is the position a read transaction's rows are current
// through: at, the checkpoint row it reads, carried to released (the
// reached position sampled before it opened) where released is past it on
// the same stream and generation, since every record between is a decoded,
// uncommitted barrier, which writes no row. Never across a generation (a
// reanchor's sequences are another space), and never backwards: a
// checkpoint committed past the release is what the rows hold.
func currentThrough(at, released Position) Position {
	if released.Stream != at.Stream || released.Generation != at.Generation || released.Seq <= at.Seq {
		return at
	}
	return released
}

// serveRead is [Reader.Read]'s body, in its order. fn runs inside the answer's
// transaction, handed the position the read was released at: the answer's
// Position.
func (r *Reader) serveRead(ctx context.Context, q Query, fn func(*sql.Tx, Position) error) (Answer, error) {

	started := time.Now()
	if !q.Level.Valid() {
		return Answer{}, fmt.Errorf("statelog: %q is not a read level (want %s)",
			q.Level, words.Or(ReadLevels))
	}
	// THE CALLER'S FLOORS ([Query.Floors], the one value here off a wire)
	// are checked before the level or this node's health, every one and
	// not only the one waited for: dropping another log's would serve that
	// half of a composed read ([Freshness.On]) from before a write its
	// caller named.
	if err := onlyOn(q.Floors, r.stream); err != nil {
		return Answer{}, fmt.Errorf("this read's floor: %w", err)
	}
	floor := floorOn(q.Floors, r.stream)
	if q.Resolve != nil && !q.Scope.Empty() {
		return Answer{}, fmt.Errorf("statelog: this read both declares a scope " +
			"and resolves one — an object named by reference has no scope " +
			"until its rows are read")
	}
	h, unread := r.health()
	if unread != nil {
		return r.refuse(Health{}, q, RefuseBrokerUnreachable, unreadHealth(unread), started)
	}

	// 1. THE LOCAL REFUSALS, in order.
	now := time.Now()
	if code := h.Refusal(now); code != "" {
		// A CONSISTENT-PREFIX READ SURVIVES A STALL and nothing else: a
		// frozen prefix is still a coherent point in the log's order. Only
		// a stall at or above the floor, though: [Health.Refusal] reports a
		// stall before a replay up to the floor, and whether a node answers
		// must not turn on which below-the-floor state a possibly stale
		// first sequence picked.
		if code != RefuseStalled || q.Level != ReadConsistentPrefix ||
			h.Floor.Effective(now) != FloorOK {
			return r.refuse(h, q, code, r.refusalDetail(h, code, now), started)
		}
	}

	// 2. COVERAGE, before any append.
	answer := Answer{Level: q.Level, Position: h.Position, Lag: h.Lag, Complete: true}
	if h.Deferred > 0 {
		gap, err := r.coverage(ctx, q)
		if err != nil {
			// A CALLER THAT GAVE UP is not a scope this node could not
			// read.
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
			// A SET READ CONTINUES AND CLAIMS NOTHING ([Query.Set]).
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

	// 3. The freshness target and, for a linearizable read, its read index,
	// which every refusal from here on carries ([Refused.Spent]) so the next
	// node asked appends no barrier of its own.
	spent := spentOf(ctx)
	target, index, waits, err := r.target(ctx, q, floor, h, spent)
	if err != nil {
		var refusal *Refused
		if errors.As(err, &refusal) {
			return r.refusePaid(h, q, refusal.Code, refusal.Detail, started, refusal.Spent)
		}
		return Answer{}, err
	}
	var paid Spent
	if !index.IsZero() {
		paid.Index = []Position{index}
	}

	// 4. The wait, holding nothing, keyed on waits rather than a non-zero
	// target: a barrier at a log's start (the code log's @0:1) has the zero
	// position before it.
	if waits {
		// The caller's floor was held to this log in step 0, so a
		// foreign position here is one this node derived (a session
		// mark, a barrier): its own state.
		if target.Stream != "" && target.Stream != r.stream {
			return r.refusePaid(h, q, RefuseWrongStream,
				fmt.Sprintf("this read names a position on %s and this domain "+
					"reads %s", target.Stream, r.stream), started, paid)
		}
		// WITHIN WHAT THE ASKER LEFT ([WithReplyMargin]): `behind` said
		// after it stopped listening is heard as a node gone. Not at all
		// for a position a node asked earlier already waited out
		// ([Spent.Waited]): the walk waits one bound wherever spent.
		budget := ReadBudget
		waited := spent.WaitedFor(target)
		if waited {
			budget = 0
		}
		waitCtx, cancel, bound := WaitWithin(ctx, budget)
		err := r.waiter.WaitReached(waitCtx, target)
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				return Answer{}, ctx.Err()
			}
			// A wait that ran its whole bound spares the next node; one
			// the applier abandoned spent nothing.
			if errors.Is(err, context.DeadlineExceeded) {
				paid.Waited = []Position{target}
			}
			return r.refusePaid(h, q, RefuseBehind,
				r.behindDetail(target, index, spent, bound, waited), started, paid)
		}
		answer.Position = r.waiter.Reached()

		// THE HEALTH IS RE-READ and the bound enforced again
		// ([Reader.pastBound]): the log ran on during a wait of up to
		// [ReadBudget], and a pre-wait lag beside post-wait rows
		// describes another moment.
		post, unread := r.health()
		if unread != nil {
			return r.refusePaid(Health{}, q, RefuseBrokerUnreachable, unreadHealth(unread), started, paid)
		}
		answer.Lag = post.Lag
		if refusal := r.pastBound(q, post); refusal != nil {
			return r.refusePaid(post, q, refusal.Code, refusal.Detail, started, paid)
		}
	}

	// 5. ONE transaction, coverage first. released, sampled before it opens,
	// is a position its rows hold every row of ([Reader.ReadThrough]).
	released := answer.Position
	if err := r.db.Read(ctx, func(tx *sql.Tx) error {
		// THE SAME HOLE THROUGH ANOTHER DOOR: a deferred record landing
		// between the probe above and this transaction. UNCONDITIONAL, not
		// gated on step 1's snapshot: the wait is exactly where such a
		// record arrives, below a linearizable read's barrier. On a node
		// with nothing retained the indexed join is over nothing.
		scope, err := q.scopeIn(ctx, tx)
		if err != nil {
			return err
		}
		d, hit, err := r.tables.deferredIn(ctx, tx, scope)
		switch {
		case err != nil:
			return err
		case hit && !q.Set:
			return &Refused{
				Code: RefuseDeferred, Level: q.Level,
				Detail: fmt.Sprintf("a record at version %d this node cannot "+
					"decode, at %s, covers what this read is about — it "+
					"landed while the read was waiting, or the reference "+
					"now resolves to an object it covers",
					d.Version, d.Position),
			}
		case hit && answer.Complete:
			// A SET READ CONTINUES AND CLAIMS NOTHING, as in step 2: one
			// that began complete and waited across a deferral would
			// otherwise report every row as there.
			answer.Complete = false
			answer.Incomplete = &Incomplete{
				Records:   1,
				From:      d.Position,
				Scope:     d.Scope,
				Direction: "unknown",
				Version:   d.Version,
			}
		}
		return fn(tx, released)
	}); err != nil {
		var refusal *Refused
		if errors.As(err, &refusal) {
			return r.refusePaid(h, q, refusal.Code, refusal.Detail, started, paid.Merge(refusal.Spent))
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
func (r *Reader) coverage(ctx context.Context, q Query) (*gap, error) {
	var found *gap
	err := r.db.Read(ctx, func(tx *sql.Tx) error {
		s, err := q.scopeIn(ctx, tx)
		if err != nil {
			return err
		}
		if s.Empty() {
			// A READ THAT NAMES NO OBJECTS is the domain term, decided
			// inside the transaction because a read by reference has no
			// scope until its rows are read.
			return errNoScope
		}
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
// not read. A refusal's detail reaches every holder of the question's grant
// (REST bodies, socket frames), and the probe's error is the store's (driver
// words, a database path), so the sentence is this package's and the error
// goes to the node's log.
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

// behindDetail is what a read whose wait ran out is told: the position it
// waited for and, where that is the record before its read index, the barrier
// too — so a caller reading `behind` sees both what this node lacked and the
// barrier that is NOT what it waited for. waited is a position a node asked
// before this one already waited a whole bound for ([Spent.Waited]).
func (r *Reader) behindDetail(target, index Position, spent Spent, bound WaitBound,
	waited bool) string {

	// THE APPOSITIVE only where the record before the read index was waited
	// for, not a caller's floor past it.
	what := target.String()
	if !index.IsZero() && target == waitsThrough(index) {
		barrier := "the barrier this read appended"
		if held, ok := spent.IndexOn(r.stream); ok && held == index {
			barrier = "the barrier a node asked for this read before this one appended"
		}
		what = fmt.Sprintf("%s, the record before %s at %s,", target, barrier, index)
	} else if waited {
		what += ","
	}
	// "REACHED" as [Runner.Reached] defines it: `behind` here is an
	// uncommitted row or a record not yet handed over, never a barrier's
	// checkpoint.
	if waited {
		return fmt.Sprintf("this node has not reached %s which a node asked for "+
			"this read before it already waited a whole bound for — so it waited "+
			"for nothing", what)
	}
	return fmt.Sprintf("this node has not reached %s within %s", what, bound)
}

// waitsThrough is the position a linearizable read resting on the read index
// at waits for: the record BEFORE the barrier. Everything the read must see is
// below a barrier appended after it arrived ([ReadIndex.Read]), and the
// barrier writes no row ([FrameworkScope]): waiting for it put its commit's
// fsync, seconds on a stalling disk, in front of every read. Waited on the
// reached position ([Runner.Reached]), so barriers below it count once
// decoded too. ONE DERIVATION for a read's own index and a [Spent.Index], so
// every node of a walk derives the same target and [Spent.WaitedFor] compares
// like with like. An index at sequence 0 is waited for as itself: a [Spent]
// arrives off the wire, and the subtraction must not wrap.
func waitsThrough(at Position) Position {
	if at.Seq == 0 {
		return at
	}
	return at.At(at.Seq - 1)
}

// target is the position this read must wait for, by level; whether it waits,
// its own answer (a linearizable read always waits, and the record before a
// barrier at a log's start is the zero position); and, for a linearizable
// read, the read index it rests on: its own barrier, or one a node asked
// earlier established ([Spent]). floor is the caller's own on this log, zero
// where it named none.
func (r *Reader) target(ctx context.Context, q Query, floor Position, h Health,
	spent Spent) (target, index Position, waits bool, err error) {
	// The caller's floor is on this log: [Reader.Read] refused a foreign one,
	// whose coordinates are another number space ([Position.On]).
	switch q.Level {
	case ReadLinearizable:
		// A READ INDEX THIS READ ALREADY HOLDS, from a node asked earlier,
		// is waited through with nothing appended, whatever this node's
		// mode: as linearizable as its own barrier ([Spent]). Not one from
		// a generation this node's log has passed, whose sequence is in
		// another space and would read as reached.
		if at, held := spent.IndexOn(r.stream); held &&
			at.Generation >= r.waiter.Reached().Generation {
			return furthest(waitsThrough(at), floor), at, true, nil
		}
		if !r.mode.Publishes() {
			return Position{}, Position{}, false, &Refused{
				Code: RefuseMaintenance, Level: q.Level,
				Detail: fmt.Sprintf("this node runs in %s mode, which publishes "+
					"nothing, and a linearizable read establishes the log's end "+
					"by appending a barrier to it — `session`, `consistent_prefix` "+
					"and `stale` reads append nothing and are still served here, "+
					"and `linearizable` is answered again once the fleet is back "+
					"in normal mode", r.mode),
			}
		}
		if r.index == nil {
			return Position{}, Position{}, false, &Refused{
				Code: RefuseBrokerUnreachable, Level: q.Level,
				Detail: "this node has no read index, so no position can be " +
					"established as the log's end",
			}
		}
		// NO SECOND BARRIER on a log a node asked earlier spent a whole
		// budget on ([Spent.Unconfirmed]): every node's goes through the
		// same leader, or the same stalled sole member.
		if spent.UnconfirmedOn(r.stream) {
			code := r.index.unconfirmed(true)
			why := "the log has no quorum"
			if code != RefuseNoQuorum {
				why = "the log's one member did not store it, its write stalled"
			}
			return Position{}, Position{}, false, &Refused{
				Code: code, Level: q.Level,
				Detail: "a node asked for this read before this one spent a barrier's " +
					"whole budget without it committing — " + why + " — so " +
					"this one appends no second barrier",
				Spent: Spent{Unconfirmed: []string{r.stream}},
			}
		}
		// The barrier's wait ends where the asker's attempt does
		// ([WithReplyMargin]): a refusal sent after it stopped listening is
		// heard as a node gone.
		barrierCtx, cancel, served := untilAnswered(ctx)
		began := time.Now()
		at, err := r.index.Read(barrierCtx)
		// Whether the asker's time ended it, read before cancel (which ends
		// the context regardless) and from the error, so a broker's refusal
		// (a full log) stays the broker's answer.
		ranOut := served && barrierCtx.Err() != nil && errors.Is(err, context.DeadlineExceeded)
		cancel()
		if err != nil {
			switch {
			case ctx.Err() != nil:
				// A CALLER THAT GAVE UP IS NOT A MISSED QUORUM: as
				// `no_quorum` it would be counted, handed an election's
				// hint and graphed as a majority failing to agree. Told
				// so where the log had no leader meanwhile.
				var none *jsprovision.NoLeader
				if errors.As(err, &none) {
					return Position{}, Position{}, false, err
				}
				return Position{}, Position{}, false, ctx.Err()
			case ranOut:
				code := r.index.unconfirmed(true)
				stalled := ""
				var none *jsprovision.NoLeader
				switch {
				case errors.As(err, &none):
					stalled = " — the " + none.Group + " stream had no leader, a broker election " +
						"outlasting the wait"
				case code != RefuseNoQuorum:
					stalled = " — the log's one member had not stored it, its write stalled"
				}
				return Position{}, Position{}, false, &Refused{
					Code: code, Level: q.Level,
					Detail: fmt.Sprintf("the barrier this read appended to establish the "+
						"log's end had not committed after %s, all its asker's attempt left%s",
						time.Since(began).Round(time.Millisecond), stalled),
					Spent: Spent{Unconfirmed: []string{r.stream}},
				}
			}
			timedOut := errors.Is(err, context.DeadlineExceeded)
			// A BROKER THAT ANSWERED IT HAS NO LEADER did not stall, though
			// the barrier spent its whole budget hearing so.
			var none *jsprovision.NoLeader
			unconfirmed := r.index.unconfirmed(timedOut && !errors.As(err, &none))
			refusal := barrierRefusal(q.Level, r.index.subject, unconfirmed, err)
			// Only a barrier that ran its own whole budget spares the next node
			// appending behind it; one refused at once spent nothing.
			if timedOut && refusal.Code == unconfirmed {
				refusal.Spent = Spent{Unconfirmed: []string{r.stream}}
			}
			return Position{}, Position{}, false, refusal
		}
		// THE FLOOR CANNOT HONESTLY BE PAST THE BARRIER'S PREDECESSOR: any
		// position a write returned or a read was answered at was on the log
		// before this append. A floor past it names a sequence not yet reached,
		// and waiting turns a pasted position into a `behind` refusal rather
		// than an answer from before it.
		return furthest(waitsThrough(at), floor), at, true, nil

	case ReadSession:
		// THE STRONGEST OF WHAT THE CALLER ALREADY KNOWS. With nothing to
		// wait for it is a stale read under a stronger name and appends
		// nothing, which is why the grammars refuse the level without a
		// position.
		session := furthest(q.Session, floor)
		return session, Position{}, !session.IsZero(), nil

	case ReadStale, ReadConsistentPrefix:
		if refusal := r.pastBound(q, h); refusal != nil {
			return Position{}, Position{}, false, refusal
		}
		// AND THE FLOOR STILL HOLDS: the lag is checked above, and the wait
		// is for the caller's own write, not the log's end.
		return floor, Position{}, !floor.IsZero(), nil
	}
	return Position{}, Position{}, false, fmt.Errorf("statelog: unreachable read level %q", q.Level)
}

// pastBound is the staleness bound's whole rule against ONE health snapshot,
// applied before the wait and again after it ([Reader.Read]'s step 4): a
// wait for a caller's floor can last [ReadBudget] while the log runs on. A
// nil return is within the bound, which includes no bound: zero accepts
// anything.
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
	// THE RECORD COUNT FIRST: it is the broker's own reading, and the
	// duration is an estimate from it.
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

// unreadHealth is the detail of a read refused because this node's own
// readiness could not be read ([ReaderDeps.Health]).
func unreadHealth(err error) string {
	return fmt.Sprintf("this node could not read its own readiness — %v — and a "+
		"read certified against a readiness nobody could establish is certified "+
		"against nothing", err)
}

// barrierRefusal maps the read index's own failures onto refusal codes: an
// unconfirmed barrier (the only one worth coming back for), a full log (a
// field an operator changes), a broker's own refusal (what it wants changed),
// and a duplicate answer, unconfirmed too since its sequence proves nothing.
// What unconfirmed means turns on how many members store the log, so the
// caller hands the code in ([ReadIndex.unconfirmed]). A barrier whose
// admission could not read the log's usage was never appended, and says so.
// The detail is this package's words, never the error's: a refusal's detail
// reaches every holder of the question's grant, and the append logs the
// error once ([frameworkAppendError]).
func barrierRefusal(level ReadLevel, subject string, unconfirmed ReadRefusal, err error) *Refused {
	var none *jsprovision.NoLeader
	if errors.As(err, &none) {
		what := "was not confirmed"
		if unavailable := (*Unavailable)(nil); errors.As(none.Answer, &unavailable) &&
			unavailable.Reason == ReasonUsageUnknown {
			what = "was never appended, the log's usage unread to admit it"
		}
		return &Refused{
			Code: unconfirmed, Level: level,
			Detail: fmt.Sprintf("the barrier on %s %s, so no position could be established as "+
				"the log's end: the %s stream had no leader for the whole of the barrier's "+
				"budget, a broker election outlasting it", subject, what, none.Group),
		}
	}
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
			return &Refused{Code: unconfirmed, Level: level, Detail: unavailable.Detail}
		case ReasonUsageUnknown:
			// NEVER APPENDED, so the sentence below, about an append
			// nobody confirmed, would be false.
			return &Refused{
				Code: unconfirmed, Level: level,
				Detail: fmt.Sprintf("the barrier on %s was never appended: the log's "+
					"usage could not be read to admit it, so no position could be "+
					"established as the log's end; the broker's own error is in this "+
					"node's log", subject),
			}
		}
	}
	why := "the broker did not answer its append, or a majority of its members " +
		"did not agree"
	switch unconfirmed {
	case RefuseBrokerStalled:
		why = "the log's one member did not store it within the barrier's budget, " +
			"so its own write is stalled — no peer was waited on"
	case RefuseBrokerUnreachable:
		why = "the broker, the log's one member, did not answer its append"
	}
	return &Refused{
		Code: unconfirmed, Level: level,
		Detail: fmt.Sprintf("the barrier on %s was not confirmed — %s — so no "+
			"position could be established as the log's end; the broker's own "+
			"error is in this node's log", subject, why),
	}
}

// refuse records and returns one refusal of a read that paid for nothing a
// node asked next would be spared ([Spent]).
func (r *Reader) refuse(h Health, q Query, code ReadRefusal, detail string, started time.Time) (Answer, error) {
	return r.refusePaid(h, q, code, detail, started, Spent{})
}

// refusePaid records and returns one refusal, carrying what the read had paid
// for when it was refused ([Refused.Spent]).
func (r *Reader) refusePaid(h Health, q Query, code ReadRefusal, detail string, started time.Time,
	paid Spent) (Answer, error) {

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
		Spent:      paid,
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
		return "records this node never applied are gone from the log, so its " +
			"rows are missing state no replay can supply; it adopts a peer's " +
			"snapshot"
	case RefuseBehind:
		// A LOCAL `behind` IS ONLY EVER THE FLOOR'S: every other lag is
		// found by the wait, which writes its own detail.
		if h.TrimFloor == nil || *h.TrimFloor == 0 {
			return ""
		}
		return fmt.Sprintf("this node is replaying records below the published "+
			"trim floor %d: the trim has licensed removing them and the log "+
			"still holds them, so this clears on its own once it has applied "+
			"through %d", *h.TrimFloor, *h.TrimFloor-1)
	case RefuseStalled:
		if h.Err != "" {
			// NOT h.Err, an applier's or health read's error (driver
			// words, a path): a refusal's detail reaches every caller.
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
