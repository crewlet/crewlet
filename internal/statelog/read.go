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
	"github.com/crewlet/crewlet/internal/words"
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
	// [Query.Floors] from outside it), and a session read that was
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
	// needs. It clears on its own, and the hint says roughly when. That
	// includes a node below the published trim floor whose missing records
	// the log still holds ([FloorReplaying]): the floor is the lowest
	// position any read may be answered from, and the node is replaying
	// its way to it.
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
	// index could be established: to the barrier a linearizable read
	// appends, or to this node's own reading of what every read is checked
	// against — its log's ends, its eviction row — so that no readiness
	// could be established either ([ReaderDeps.Health]).
	RefuseBrokerUnreachable ReadRefusal = "broker_unreachable"

	// RefuseNoQuorum — the barrier did not commit: in the read index's own
	// budget, or in what a served request's asker left it
	// ([WithReplyMargin]). It is not the same as unreachable: the broker
	// answered, and a majority did not agree in that time.
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
	// [ErrForeignPosition] and not a refusal at all.
	RefuseWrongStream ReadRefusal = "wrong_stream"

	// RefuseTooStale — this node's lag is past what the caller said it
	// would accept.
	RefuseTooStale ReadRefusal = "too_stale"

	// RefuseMaintenance — this node runs in a mode that publishes nothing
	// ([MaintenanceMode.Publishes]), and a linearizable read establishes the
	// log's end by APPENDING a barrier to it. The other levels append
	// nothing and keep answering; this one is answered again once the
	// fleet is back in normal mode. Not retryable: the mode is the
	// process's for its life, and every node of a fleet in a capacity
	// window is in one, so neither waiting nor another node clears it.
	RefuseMaintenance ReadRefusal = "maintenance"
)

// ReadRefusals are every code. The COUNT IS DERIVED from this slice rather
// than written beside it in prose: a number in a comment and a set in code are
// two lists, and the comment is the one nobody updates.
var ReadRefusals = []ReadRefusal{
	RefuseBehind, RefuseDeferred, RefuseDeferredScopeUnknown, RefuseStalled,
	RefuseBelowFloor, RefuseFloorUnknown, RefuseEvicted, RefuseBrokerUnreachable,
	RefuseNoQuorum, RefuseLogFull, RefuseBrokerRefused, RefuseWrongStream,
	RefuseTooStale, RefuseMaintenance,
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

// Elsewhere reports whether ANOTHER node may serve a read this one refused
// with r: every code but the three that are facts about the whole fleet rather
// than about this node's copy, its view of the log or its place in a
// partition — `log_full`, since every node appends its barrier to the one log
// at the one ceiling; `broker_refused`, since the stream that refused to store
// this node's barrier (sealed, or a store out of resources) is the one every
// node appends its own to; and `maintenance`, since every node of a fleet in a
// capacity window is in one.
//
// It is what makes a refusal a REDIRECT where [ReadRefusal.Retryable] makes it
// a hint, and the two differ: a node that cannot decode a record will never
// serve the read (not retryable) while its peer may at once (elsewhere); a
// node in the minority of a partition refuses `no_quorum` while one in the
// majority commits its barrier; `wrong_stream` is a caller's position on
// another log, which every node refuses alike, or this node's own rows keyed
// to another log, which a peer's are not — and a refusal every node gives is
// still the answer once each has given it, so treating it as one another node
// may serve costs asking, never a different final answer. The estate's router
// asks the next data node a read refused for any of these, and keeps the
// refusal as its answer only where no node does better.
//
// WHAT ASKING COSTS is a question per node and nothing more, because the
// refusal carries what the read paid for ([Refused.Spent]) and the router
// hands it to every node it asks next ([WithSpent]): the walk appends at most
// one barrier and waits at most one whole bound for a position, however many
// nodes it asks. Asked afresh, each node appended a barrier of its own and
// waited a whole [ReadBudget] for it — so a fleet the same burst put behind
// together paid a barrier and two seconds per data node for one final
// `behind`.
func (r ReadRefusal) Elsewhere() bool {
	switch r {
	case RefuseLogFull, RefuseBrokerRefused, RefuseMaintenance:
		return false
	}
	return r.Valid()
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
	// position, the field an operator has to change. Written for WHOEVER
	// RUNS THE NODE: it goes to the node's log and to the gate and
	// retention tooling, never to a screen, where a person is told the
	// code in [ConditionSentence]'s words. Never an error's own text — see
	// [Unavailable]'s detail, held to the same rule.
	Detail string

	// RetryAfter is DERIVED rather than fixed where it can be: how far
	// behind this node is, divided by how fast it is actually draining.
	// Zero means no hint, which is the honest answer for a refusal
	// waiting cannot clear.
	RetryAfter time.Duration

	// Spent is what this read had paid for when it was refused — the read
	// index it held, the position it waited its whole bound for, the log a
	// barrier ran out on — which a router asking another node for the same
	// read carries there ([Spent], [WithSpent]), so the walk pays for each
	// once rather than once a node.
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

// RetryHint derives how long a caller should wait before coming back.
//
// FROM THE OBSERVED DRAIN, not from a constant. A flat hint is wrong in both
// directions on the same fleet: it sends a caller back too early on a node
// grinding through a long backlog, and holds one waiting on a node that caught
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

	// Resolve replaces Scope for a read whose object is named by a
	// REFERENCE — a key, an alias, an address — rather than by the path
	// its own records are filed under. Exactly one of the two is set, and
	// [Query.Set] still decides what a deferral does: a point read refuses
	// and a read that reports its coverage is served and says so.
	//
	// A PATH CANNOT BE FORMED FROM A REFERENCE ALONE, and forming one
	// anyway is the failure this exists for: a task named "ENG-12" is
	// filed under `t/c/ENG/o/<uuid>`, and a scope built from the key as
	// though it were the id (`t/c/workspace/o/ENG-12`) contains nothing
	// any record is filed under — so the probe passes on every deferral
	// there is, and the point read answers from rows a record it cannot
	// decode has already made wrong. Only the rows know where the object
	// is filed, so the resolver reads them, and it reads them INSIDE THE
	// TRANSACTION THE ANSWER IS READ IN: a scope resolved in one and
	// probed in another would certify an object a rename had moved.
	//
	// A reference that resolves to NOTHING still answers a scope — the
	// widest one the reference itself bounds (an address, or the domain)
	// — because "not found" is a claim about the rows too, and a deferred
	// create is precisely a row this node does not have.
	Resolve Resolver

	// Session is the caller's own high-water mark on this stream, which
	// is what a session read waits for.
	Session Position

	// Floors are the explicit floors a caller may name — the position a
	// wake carried, or one it was handed by its own previous write. They
	// are honoured at EVERY level (see [Freshness]): the later of the
	// floor on this log and the level's own target is what the read waits
	// for, so a caller that holds a position is never served rows from
	// before it whatever else it asked for.
	//
	// EVERY ONE IS ON THIS LOG, or the read is refused before anything
	// else ([ErrForeignPosition]): a read of one log waits on that log's
	// floor alone, and a floor on another is the request's mistake — a
	// read of several logs hands each of its halves only that half's own
	// ([Freshness.On]), so a floor that reaches the wrong half is one its
	// composer misplaced, refused rather than dropped.
	Floors []Position

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

	// Mode is what this node started for, and it decides whether a read
	// may append.
	//
	// A BARRIER IS A PUBLISH. It is a record on the log, admitted, stored
	// and replicated like any other, and a node in a mode that publishes
	// nothing exists so that the fleet's logs hold still while a capacity
	// window measures them — a seal-mode acknowledgement is evidence
	// precisely because nothing on that node writes. So in such a mode a
	// linearizable read is refused [RefuseMaintenance] rather than
	// answered, the reader is given no [ReadIndex] to append through
	// ([NewReader] refuses one), and every level that appends nothing is
	// served as ever. REQUIRED: the zero mode is refused rather than read
	// as either, since each reading is wrong in one of the two postures.
	Mode MaintenanceMode

	DB     readStore
	Index  *ReadIndex
	Waiter Waiter

	// Health is this domain's readiness, asked on every read — and again
	// after a read that waited — because every one of its terms can change
	// between two of them.
	//
	// ASKED PER READ, SO IT MUST COST NO ROUND TRIP. The terms that live on
	// a broker — the published trim floor, the log's two ends — may be
	// served from memory, provided each AGES at a bound of its own:
	//
	//   - the floor carries the instant it was read ([Floor]), and one read
	//     longer ago than [FloorCacheStale] is [FloorUnknown], which
	//     refuses;
	//   - the ends — [Health.FirstSeq], [Health.LastSeq] and the
	//     [Health.Lag] a bounded read is checked against — are served no
	//     older than the provider's own refresh leaves them, and past that
	//     they are unread and the health is not served at all. A minute's
	//     bound is the floor's, and on the ends it served a
	//     `max_lag_seq` read a lag nobody had measured for that minute.
	//
	// The engine re-reads both off the read's path once a second and
	// serves ends at most three seconds old (a second's recheck plus the
	// [ReadBudget] the re-read is bounded by), so the stale level's "no
	// broker call at all" holds for the health too.
	//
	// AN ERROR IS A HEALTH NOBODY COULD READ, and every read refuses it
	// [RefuseBrokerUnreachable], naming it — at every level, since a read
	// certified against a readiness nobody could establish is certified
	// against nothing. ITS OWN RETURN rather than a [Health] with a
	// sentence in it: folded into [Health.Err], which is an applier's stop,
	// the zero floor beside it was refused first, and an unreadable log end
	// reached a caller as "the published trim floor has never been read on
	// this node" — the wrong code, a false sentence, and an operator sent
	// to coordination over a stream leader that had stopped answering.
	// Where the FLOOR is what could not be read, a provider hands the floor
	// it last read, carrying its instant, and the read refuses
	// [RefuseFloorUnknown] naming how long ago, as the alarm of that name
	// does.
	Health func() (Health, error)

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

// Read serves one query, running fn inside the answer's own transaction.
//
// # The order, and why every step is where it is
//
//  0. The REQUEST: a level this build knows, and a floor on this read's own
//     log ([Position.On]). Neither is a state of this node, so neither waits
//     behind one — every node refuses the same request the same way, and an
//     evicted or stalled node must not answer it with a refusal about itself.
//  1. The cheap LOCAL refusals, so a doomed read never appends, in
//     [Health.Refusal]'s order. Each is answered from what this node
//     already knows, for nothing.
//  2. COVERAGE, still before any append. A read whose objects this node
//     cannot answer for could never be certified however fresh it got, so it
//     must not spend a quorum round trip to find that out — and coverage is
//     DECIDED rather than waited for: the number it would wait on is pinned
//     below the checkpoint while a barrier is appended above it, so the wait
//     could not terminate.
//  3. The freshness target, by level. Only linearizable appends.
//  4. The WAIT, on this node's REACHED POSITION ([Runner.Reached]), holding no
//     connection and no transaction. A linearizable read waits for every
//     record BEFORE its barrier ([waitsThrough]) — for each that writes a row
//     to commit, and for each barrier among them to be decoded — and never for
//     a barrier's commit, its own or any other's: a barrier writes no row on
//     any node ([FrameworkScope]), so its commit is a checkpoint and an fsync
//     that change nothing the read reads, and the applier reaches it the
//     moment it decodes it. EVERY linearizable read takes this step, its
//     predecessor the zero position included, because the step is also where
//     its answer is stamped and its staleness bound checked against a health
//     read after the wait.
//  5. ONE read transaction, whose FIRST statement is the coverage probe
//     again — a deferred record landing between the two is the same hole
//     through another door — and then the answer.
func (r *Reader) Read(ctx context.Context, q Query, fn func(*sql.Tx) error) (Answer, error) {
	started := time.Now()
	if !q.Level.Valid() {
		return Answer{}, fmt.Errorf("statelog: %q is not a read level (want %s)",
			q.Level, words.Or(ReadLevels))
	}
	// THE CALLER'S FLOORS NAME STREAMS, and they are checked before any
	// level — or this node's own health — looks at anything.
	// [Query.Floors] is the one value here that came off a wire; the
	// session mark and the barrier are this domain's own. EVERY floor is
	// held to this log, not only the one this read would wait for: a read
	// handed a second log's floor beside its own is a composed read whose
	// halves were not given their own ([Freshness.On]), and dropping the
	// other would serve that half from before a write its caller named.
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
		// A CONSISTENT-PREFIX READ SURVIVES A STALL and nothing else
		// does: it asks only for a coherent point in the log's order,
		// which a frozen prefix still is. Every other refusal here says
		// this node's rows are wrong, or below the published floor every
		// node must hold before it answers.
		//
		// AND ONLY A STALL AT OR ABOVE THE FLOOR. [Health.Refusal]
		// reports a stall BEFORE a node replaying up to the floor,
		// because a replay that is not moving is not one to wait on — so
		// without this term a stalled node below the published floor
		// served these reads, where the same node with a lower first
		// sequence reported `below_floor` and served nothing. Whether a
		// node answers must not turn on which below-the-floor state a
		// possibly stale first sequence happened to pick.
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

	// 3. The freshness target — and, for a linearizable read, the read index
	// it rests on, which every refusal from here on carries ([Refused.Spent])
	// so a router asking another node for this read hands it on rather than
	// have that node append a barrier of its own.
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

	// 4. The wait, holding nothing — keyed on what the target SAYS rather than
	// on its being non-zero: a barrier at the very start of a log (the code
	// log's first record is @0:1) has the zero position before it, and a
	// linearizable read is stamped and bounded on the path below whatever its
	// barrier's sequence.
	if waits {
		// THE CALLER'S FLOOR WAS HELD TO THIS LOG in step 0, so what
		// can still name another here is a position this node derived
		// for the read — a session mark, a barrier — which is this
		// node's own state rather than the request's.
		if target.Stream != "" && target.Stream != r.stream {
			return r.refusePaid(h, q, RefuseWrongStream,
				fmt.Sprintf("this read names a position on %s and this domain "+
					"reads %s", target.Stream, r.stream), started, paid)
		}
		// WITHIN WHAT THE ASKER LEFT, where the read was asked across a wire
		// ([WithReplyMargin]): `behind` said after the asker stopped
		// listening is heard as a node that is gone.
		//
		// AND NOT AT ALL FOR A POSITION THIS READ ALREADY WAITED OUT on a
		// node asked for it before this one ([Spent.Waited]): the walk's
		// wait for a position is one bound wherever it is spent, so this
		// node serves the read if it holds the position now and refuses
		// `behind` at once if it does not.
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
			// THE WAIT RAN ITS WHOLE BOUND, which is what a node asked
			// next is spared; a wait the applier abandoned spent nothing
			// a later node's would.
			if errors.Is(err, context.DeadlineExceeded) {
				paid.Waited = []Position{target}
			}
			return r.refusePaid(h, q, RefuseBehind,
				r.behindDetail(target, index, spent, bound, waited), started, paid)
		}
		answer.Position = r.waiter.Reached()

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
		post, unread := r.health()
		if unread != nil {
			return r.refusePaid(Health{}, q, RefuseBrokerUnreachable, unreadHealth(unread), started, paid)
		}
		answer.Lag = post.Lag
		if refusal := r.pastBound(q, post); refusal != nil {
			return r.refusePaid(post, q, refusal.Code, refusal.Detail, started, paid)
		}
	}

	// 5. ONE transaction, coverage first.
	if err := r.db.Read(ctx, func(tx *sql.Tx) error {
		// THE SAME HOLE THROUGH ANOTHER DOOR. A deferred record landing
		// between the probe above and this transaction would otherwise be
		// invisible to both.
		//
		// UNCONDITIONAL, and not gated on the health read in step 1. That
		// snapshot was taken BEFORE the wait, and the wait is exactly
		// where a record this node cannot decode arrives: a linearizable
		// read's barrier sits above it, so the applier has to cross it to
		// release the read. Gated on the old snapshot, a node that held
		// nothing deferred when the read began skipped this probe on the
		// one path it was written for, and certified rows the record had
		// already made wrong. The probe is an indexed join over the
		// retained records, which is to say over nothing on every node
		// that has nothing retained.
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
			// A SET READ CONTINUES AND CLAIMS NOTHING, here as in
			// step 2 — and for the same reason it cannot be skipped
			// here: a set read that began complete and waited across
			// a deferral would otherwise report every row as there.
			answer.Complete = false
			answer.Incomplete = &Incomplete{
				Records:   1,
				From:      d.Position,
				Scope:     d.Scope,
				Direction: "unknown",
				Version:   d.Version,
			}
		}
		return fn(tx)
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
			// A READ THAT NAMES NO OBJECTS cannot be certified about
			// any, so it is the domain term rather than a free pass —
			// decided here, inside the transaction, because a read by
			// reference has no scope until its rows are read.
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

// behindDetail is what a read whose wait ran out is told: the position it
// waited for and, where that is the record before its read index, the barrier
// too — so a caller reading `behind` sees both what this node lacked and the
// barrier that is NOT what it waited for. waited is a position a node asked
// before this one already waited a whole bound for ([Spent.Waited]).
func (r *Reader) behindDetail(target, index Position, spent Spent, bound WaitBound,
	waited bool) string {

	// THE APPOSITIVE, where the position waited for is the record before the
	// read index — and not where a caller's floor past it is what was waited
	// for, which names a position of its own.
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
	// "REACHED", the word [Runner.Reached] defines: every record through the
	// position that writes a row committed, and every barrier decoded — so a
	// `behind` here is a row this node has not committed, or a record it has
	// not been handed yet, and never a barrier's checkpoint.
	if waited {
		return fmt.Sprintf("this node has not reached %s which a node asked for "+
			"this read before it already waited a whole bound for — so it waited "+
			"for nothing", what)
	}
	return fmt.Sprintf("this node has not reached %s within %s", what, bound)
}

// waitsThrough is the position a linearizable read resting on the read index
// at waits for: the record BEFORE the barrier, never the barrier itself.
//
// # Why the record before it
//
// Everything a linearizable read must see was acknowledged before the read
// arrived, so it is below a barrier appended after that ([ReadIndex.Read]) —
// and the barrier itself writes no row on any node ([FrameworkScope]): its
// apply is the checkpoint and the commit's fsync, which change nothing the read
// reads. Waiting for it put that fsync in front of every linearizable read, and
// on a disk whose fsyncs stall for seconds it held reads for the whole of
// [ReadBudget] behind a commit that wrote no row.
//
// WAITED FOR ON THE REACHED POSITION ([Runner.Reached]), which is what makes
// the same true of every barrier BELOW this one: a record before it that is
// itself a barrier — another read's, or another node's — is reached the moment
// this node decodes it, and its commit is waited for no more than this one's.
// What the read waits for is every record before its barrier that writes a
// row.
//
// ONE DERIVATION for both of a read's indexes — its own barrier, and the one a
// node asked before it established ([Spent.Index]) — so every node a walk asks
// derives the same target from the same index, and [Spent.WaitedFor] compares
// like with like.
//
// A READ INDEX AT SEQUENCE 0 IS WAITED FOR AS ITSELF. The broker never
// acknowledges an append there, but a [Spent] arrives off the wire, and the
// subtraction must not wrap round to the end of the number space.
func waitsThrough(at Position) Position {
	if at.Seq == 0 {
		return at
	}
	return at.At(at.Seq - 1)
}

// target is the position this read must wait for, by level, whether it waits
// at all, and — for a linearizable read — the read index it rests on: the
// barrier it appended, or the one a node asked for the same read before it
// established ([Spent]). floor is the caller's own on this log, zero where it
// named none.
//
// WHETHER IT WAITS IS ITS OWN ANSWER, never read off the target being non-zero:
// a linearizable read always waits ([Reader.Read]'s step 4), and the record
// before a barrier at the start of a log is the zero position.
func (r *Reader) target(ctx context.Context, q Query, floor Position, h Health,
	spent Spent) (target, index Position, waits bool, err error) {
	// THE CALLER'S FLOOR IS ON THIS LOG: [Reader.Read] refused one that is
	// not before it got here, because comparing a foreign floor against a
	// local target is comparing coordinates from two number spaces — see
	// [Position.On].
	switch q.Level {
	case ReadLinearizable:
		// A READ INDEX THIS READ ALREADY HOLDS — established by a node
		// asked for it before this one — IS WAITED THROUGH AND NOTHING IS
		// APPENDED: as linearizable as a barrier of this node's own, since
		// that barrier started after the request reached a node and the
		// broker committed it ([Spent]). And so whatever this node's mode,
		// since it appends nothing. NOT ONE FROM A GENERATION THIS NODE'S
		// LOG HAS PASSED: a sequence there is a number in another space,
		// and compared against this node's checkpoint it would read as
		// reached whatever the log holds — so this node establishes its
		// own.
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
		// NO SECOND BARRIER ON A LOG A NODE ASKED BEFORE THIS ONE SPENT A
		// BARRIER'S WHOLE BUDGET ON: every node's goes to the one log's
		// leader, which has no quorum ([Spent.NoQuorum]).
		if spent.NoQuorumOn(r.stream) {
			return Position{}, Position{}, false, &Refused{
				Code: RefuseNoQuorum, Level: q.Level,
				Detail: "a node asked for this read before this one spent a barrier's " +
					"whole budget without it committing — the log has no quorum — so " +
					"this one appends no second barrier",
				Spent: Spent{NoQuorum: []string{r.stream}},
			}
		}
		// THE BARRIER'S WAIT ENDS WHERE THE ASKER'S ATTEMPT LEAVES OFF, on a
		// read asked across a wire ([WithReplyMargin]): it may first wait
		// out a barrier already in flight and then its own, a read index's
		// budget each, and a refusal sent after the asker stopped
		// listening is a node heard as gone.
		barrierCtx, cancel, served := untilAnswered(ctx)
		began := time.Now()
		at, err := r.index.Read(barrierCtx)
		// WHETHER THE ASKER'S TIME IS WHAT ENDED IT, read before the cancel
		// below — which ends the context whatever ended the wait — and from
		// the error too, so a barrier the broker refused (a full log) is
		// still the broker's answer.
		ranOut := served && barrierCtx.Err() != nil && errors.Is(err, context.DeadlineExceeded)
		cancel()
		if err != nil {
			switch {
			case ctx.Err() != nil:
				// A CALLER THAT GAVE UP IS NOT A MISSED QUORUM. The
				// read index answers a caller's own cancellation or
				// deadline with that context's error, as the wait below
				// answers it: a reader that hung up says nothing about
				// the broker, and mapped onto `no_quorum` it was
				// counted as a refusal, handed an election's retry hint
				// and drawn as a majority failing to agree on every
				// operator's graph.
				return Position{}, Position{}, false, ctx.Err()
			case ranOut:
				return Position{}, Position{}, false, &Refused{
					Code: RefuseNoQuorum, Level: q.Level,
					Detail: fmt.Sprintf("the barrier this read appended to establish the "+
						"log's end had not committed after %s, all its asker's attempt left",
						time.Since(began).Round(time.Millisecond)),
					Spent: Spent{NoQuorum: []string{r.stream}},
				}
			}
			refusal := barrierRefusal(q.Level, r.index.subject, err)
			// A BARRIER THAT RAN ITS OWN WHOLE BUDGET is a log without a
			// quorum, which a node asked next is spared appending to; one
			// refused at once — a full log, a broker that did not answer —
			// spent nothing it would.
			if refusal.Code == RefuseNoQuorum && errors.Is(err, context.DeadlineExceeded) {
				refusal.Spent = Spent{NoQuorum: []string{r.stream}}
			}
			return Position{}, Position{}, false, refusal
		}
		// THE FLOOR CANNOT BE PAST THE BARRIER'S PREDECESSOR on the stream
		// the barrier was appended to — a position a write returned, or an
		// earlier read was answered at, was on the log before this append
		// was, so it is BELOW the barrier — and on the honest path this is
		// the record before the barrier. On the other path the floor names a
		// sequence this stream has not reached, and waiting for it is what
		// turns a pasted position into a `behind` refusal the caller can
		// read rather than an answer served from before it.
		return furthest(waitsThrough(at), floor), at, true, nil

	case ReadSession:
		// THE STRONGEST OF WHAT THE CALLER ALREADY KNOWS. A session
		// read that found nothing to wait for is a stale read wearing a
		// stronger name, so it waits for nothing rather than appending
		// a barrier nobody asked for — which is why the grammars refuse
		// the level to a caller that named no position.
		session := furthest(q.Session, floor)
		return session, Position{}, !session.IsZero(), nil

	case ReadStale, ReadConsistentPrefix:
		if refusal := r.pastBound(q, h); refusal != nil {
			return Position{}, Position{}, false, refusal
		}
		// AND THE FLOOR STILL HOLDS. A stale read with a position is
		// "whatever this node holds, from here on": the lag is checked
		// above and reported on the answer, and the wait below is for
		// the caller's own write rather than for the log's end.
		return floor, Position{}, !floor.IsZero(), nil
	}
	return Position{}, Position{}, false, fmt.Errorf("statelog: unreachable read level %q", q.Level)
}

// pastBound is the staleness bound's whole rule, against ONE health snapshot.
//
// # Why it is a function and not the lines it replaced
//
// Because the bound has to be applied to TWO snapshots and there must be one
// rule for both. It was enforced only against the snapshot taken before the
// read waited — and a read can now wait for the caller's own floor
// ([Query.Floors]) for up to [ReadBudget], a wait whose ending says this
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

// unreadHealth is the detail of a read refused because this node's own
// readiness could not be read ([ReaderDeps.Health]).
func unreadHealth(err error) string {
	return fmt.Sprintf("this node could not read its own readiness — %v — and a "+
		"read certified against a readiness nobody could establish is certified "+
		"against nothing", err)
}

// barrierRefusal maps the read index's own failures onto refusal codes.
//
// The four are genuinely different: a majority that did not agree (or a
// broker that did not answer), a log that is full, a broker that refused the
// barrier for a reason of its own, and a duplicate answer that proves nothing.
// Only the first is worth coming back for; a full log names a field an
// operator has to change, and a broker's refusal names what the broker wants
// changed.
//
// A BARRIER NOBODY CONFIRMED IS TOLD IN THIS PACKAGE'S WORDS, never the
// error's. The error is the client's or the broker's own — a timeout, a
// connection closed, whatever the transport said — and a refusal's detail is
// sent to the caller by every surface that answers one, so it reached every
// holder of the question's grant the day refusals stopped being answered as
// faults. The barrier's append logs the error once per append
// ([frameworkAppendError]), where an operator reads it, rather than once per
// reader waiting on it.
func barrierRefusal(level ReadLevel, subject string, err error) *Refused {
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
