package statelog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/recordseal"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// casRounds bounds a read-decide-write retry.
//
// SIXTEEN, as [internal/sandbox]'s run store: sixteen consecutive lost races
// on one object means something rewrites it in a loop, so "it kept changing"
// is the honest answer; retrying for ever would hold a request open against a
// write storm.
const casRounds = 16

// Pattern is how a write arbitrates. There are four, and they ask the broker
// three different questions: the last two ask it none, and differ in WHO
// decides a record's effect once it is ordered.
type Pattern int

const (
	// PatternArbitrated is the ordinary optimistic update: the
	// expectation is the subject's arbitration anchor, and a rejection
	// means somebody wrote first.
	PatternArbitrated Pattern = iota

	// PatternCreate is first-writer-wins: the object's id IS the subject,
	// the expectation is zero, and the guarding row is what still holds
	// below the trim floor where the broker's own claim does not.
	PatternCreate

	// PatternAdditive carries NO expectation at all. It is correct only
	// where the records commute — a sum, an append-only log of turns —
	// because a commutative fold needs no arbitration and paying for one
	// would serialize the hottest subject in the domain for nothing.
	PatternAdditive

	// PatternSequenced carries NO expectation either, and its records do
	// NOT commute: the log ORDERS them and the APPLIER DECIDES each one, as
	// a pure function of the record and its rows at that position, writing
	// a verdict row for every record (applied, or refused and why). The
	// writer refuses only what must be refused before appending, and owes
	// the read of its verdict row before it reports what the write did.
	//
	// Not additive, because such a write (a guarded datastore batch) does
	// not commute; not arbitrated, because a batch over many objects cannot
	// carry one expectation each, and one shared subject would turn a burst
	// of clicks into rounds that exhaust [casRounds]. The publisher treats
	// it exactly as an additive write.
	PatternSequenced
)

// expects reports whether a write of this pattern carries a per-subject
// expectation the broker arbitrates.
func (p Pattern) expects() bool {
	return p != PatternAdditive && p != PatternSequenced
}

// Waiter is this node's own applier, as the publisher and the reader need it.
type Waiter interface {
	// Reached is the position this node's rows are current through
	// ([Runner.Reached]): every record at or below it committed, or a
	// barrier that writes no row.
	Reached() Position

	// WaitReached blocks until this node's applier has reached p
	// ([Runner.WaitReached]). It turns a lost race into a retry that can
	// win: re-deciding without it re-reads the stale anchor every round.
	WaitReached(ctx context.Context, p Position) error

	// WaitApplied blocks until this node holds the derived consequences
	// of everything up to p for the objects in s.
	WaitApplied(ctx context.Context, s ScopeSet, p Position) error
}

// Voids is this node's applier's reanchor rules, as a resolution asks them —
// [Runner.Voided] in the engine.
type Voids interface {
	// Voided reports whether this node's applier drops a record stamped
	// with generation gen at sequence seq under a reanchor's rule, and
	// which ([ReasonAbandoned], [ReasonOvertaken]).
	//
	// The framework's own gates, which no domain's [Gates] sees: a record
	// they dropped wrote no ledger row, and a resolution that asked only
	// the domain reported it as a ledger contract violation.
	Voided(gen uint32, seq uint64) (Reason, bool)
}

// Identity is whether this node's positions are sequences on the stream the
// broker serves under the domain's name: [Runner.StreamIdentity], as the
// publisher needs it. Every write asks it first, every time, and every
// pattern refuses on a rebuilt stream or a checkpoint past the log's end: its
// expectations, zero fence and resolutions would be numbers in another space
// (package doc, "And the premise under all of it: ONE STREAM"). A reading
// that could not be taken establishes nothing; the next one decides.
//
// Truncated answers for a PEER whose rows hold records the log lost
// ([ErrLogTruncated]): a write from here is one that peer's restored reanchor
// would apply nowhere, so every write refuses on it but a node gate, the
// gesture that ends it ([Request.NodeGate]).
type Identity interface {
	StreamIdentity() error
	Truncated() error
}

// Fence refuses a write this node must not make.
//
// TWO METHODS, TWO PRICES: Evicted runs on every append, answered from what
// this node already knows; ClearForZero runs only where being wrong is a
// silent lost update, and pays a coordination round trip for a fresh answer.
type Fence interface {
	// Evicted reports this node's own eviction.
	//
	// It must consult every source fresh in a failure mode the others are
	// not, in particular this node's own applied eviction rows: the only
	// source still fresh when the coordination path is wedged, which is a
	// precondition of an eviction being permitted at all.
	Evicted(ctx context.Context) (bool, error)

	// ClearForZero verifies, freshly, that publishing at an expectation of
	// zero is safe: not evicted, the applier's live checkpoint not past the
	// log's end, and [Replayable](cursor, F) for F the higher of the trim
	// floor (read at cursor's generation) and the log's first sequence: the
	// floor theorem (package doc), written once as [ZeroFence]. cursor is
	// the write's [Snap.Checkpoint], never the live position, which clears a
	// decision against records it never read. Every refusal is an
	// [*Unavailable], never [ErrConflict]; an unknown read refuses, since
	// failing open here is a lost update. Its read of the log hands the
	// applier the stream's instant and end ([Runner.ObserveStream],
	// [Runner.ObserveEnd]) and the publisher asks [Identity] after it
	// ([Publisher.clearForZero]).
	ClearForZero(ctx context.Context, cursor Position) error
}

// Gates answers whether a durable record produced rows on NO node: a
// deletion marker or a node's eviction dropped a record the broker accepted,
// so no ops row exists, and without this seam the resolution reads "somebody
// else won" and burns its rounds to a false conflict.
//
// Every domain's reader answers by one rule, certified by
// statelogtest.RunGates (RunEvictionGates where a domain keeps no marker):
// the deletion gate is reported first, since it stays true; the marker has
// no position and covers every subject the applier's does; the purge's own
// record is exempt from its marker by operation id; the eviction window is
// half-open at both ends (package doc, "The rule every gate reader keeps").
type Gates interface {
	// GatedAt reports whether a record at p on this subject, published by
	// writer under opID, applies nowhere, and the gate that answers by the
	// rule above.
	//
	// opID exempts a purge's record from the marker it wrote, or a purge
	// whose acknowledgement was lost is told it did not happen. writer is
	// the record's writer, not always this node: a collapsed copy is asked
	// about its own writer, read off the log ([Publisher.resolve]). body is
	// the record's own body when the record at p is this operation's, else
	// nil; it is for a gate keyed on what the payload names (the identity
	// estate's directory records and sessions), and with nil a reader
	// answers from the subject alone.
	GatedAt(ctx context.Context, subj Subject, writer, opID string, p Position, body []byte) (Reason, bool, error)
}

// Request is one caller-visible write.
type Request struct {
	// Subject is the object this write arbitrates over.
	Subject Subject

	// Scope is every object this write's rows touch, which is what the
	// deferral probe is run over and what the resolution waits on.
	Scope ScopeSet

	// OpID is the operation id, minted ONCE per caller-visible operation
	// and stable across every round and retry: a regenerated id defeats the
	// ledger in the lost-ack case it exists for. It carries its mint instant
	// in its own bytes ([NewOpID], [DeriveOpID], [OpMintedAt]); there is
	// deliberately no field for it, since a caller-stamped instant is the
	// call's, later than the mint on every retry.
	OpID string

	// Session is this caller's own high-water mark on the stream: the
	// publisher waits for its applier to reach it BEFORE opening a
	// snapshot, so a caller that just created a task decides its edit from
	// a state that contains it. The zero position waits for nothing.
	//
	// SET PER GESTURE, never remembered by the publisher: it is a domain's
	// singleton, so an accumulated mark would make one gesture wait on an
	// unrelated one's record. The gesture hands it from step to step
	// (`worklet.Writer.After`).
	Session Position

	// Pattern is how this write arbitrates.
	Pattern Pattern

	// NodeGate marks an eviction or a readmission of a node: a statement
	// about a MACHINE, arbitrated on its own subject, decided from nothing
	// in this node's rows. So it alone passes the fences between an
	// operator and the gesture that ends a fault: a passed generation
	// ([Publisher.checkIdentity]), a peer's truncated rows
	// ([Publisher.checkTruncated]) and the gate reserve every other write
	// is refused `log_full` short of ([Reservation.Bytes]). The publisher
	// refuses the flag on a record the domain does not call a node gate
	// ([Domain.NodeGate], [Publisher.stamped]).
	NodeGate bool

	// Standing judges a retry this node's ledger already answers
	// ([Snap.Held]) for a write a LATER write can undo: nil while the
	// operation's record is still in force, else a refusal
	// ([ReasonSuperseded]). It runs in the snapshot's transaction, after
	// [Publisher.heldHere]. Nil where a ledger row is the whole answer; a
	// node gate sets it ([GateStanding]), since an eviction a readmission
	// inverted must not be answered `applied`.
	Standing func(tx *sql.Tx, held Position) error

	// Decide runs inside the snapshot's transaction and returns the record
	// to publish. It may run once per round, each on a fresh snapshot, and
	// must decide only from rows it reads there. The record must carry the
	// round's [Stamp] in its envelope, or the publisher refuses it before
	// appending.
	Decide func(tx *sql.Tx, stamp Stamp) (Decision, error)

	// NoWait answers with what the broker's acknowledgement establishes
	// (durable at a position) rather than waiting for this node's applier.
	// It is for a caller that holds the write's result independently of
	// the rows: a sign-in, whose cookie carries the position and is
	// validated against each node's applier, where the wait cost 250-500 ms
	// against a 50 ms verify. It does NOT skip [Request.Session]: that wait
	// is "may I decide yet", this one "may I report what it produced".
	//
	// The caller owes the position to whoever reads next ([Result.Waited]
	// says which pending came back), and that reader owes the skipped wait:
	// internal/api/auth's session arm makes a write wait for the bearer's
	// position before deciding it.
	NoWait bool
}

// Stamp is what every record a publisher appends carries about WHERE IT CAME
// FROM, handed to the domain's decision rather than left for it to find.
//
// Both halves are read by somebody who cannot ask the writer: the eviction
// gate compares Writer against an eviction on every applier, and the
// generation makes a pre-reanchor record safely stale rather than plausible.
// A record missing them is one no eviction drops, and nothing looks wrong, so
// [Publisher] decodes what the decision returned through [Domain.Envelope]
// and refuses a record that does not carry them ([Publisher.stamped]).
type Stamp struct {
	// Writer is this node's id — the publisher's own, which is what the
	// eviction gate compares against.
	Writer string

	// Gen is the generation of [Snap.Checkpoint], read in the decision's
	// own transaction, so the record carries the generation of the rows it
	// was decided from.
	Gen uint32
}

// Admission holds a log's ordinary appends out of its gate reserve. Declared
// by its callers, this publisher and the read index; satisfied by [*Reserve].
type Admission interface {
	// Admit takes room for one ordinary append of size bytes, refusing it
	// `log_full` past the log's ordinary ceiling. On a nil error the caller
	// releases the room once the broker has answered.
	Admit(ctx context.Context, size int64) (release func(), err error)
}

// ErrExists reports a first-writer-wins create for an object that is already
// there, established from the guarding row rather than from the broker.
var ErrExists = errors.New("statelog: the object already exists")

// Publisher is a domain's write authority: one snapshot, decide and form the
// expectation inside it, publish, let the broker arbitrate, never guess
// (package doc, "The write authority, stated once because it is the whole
// safety argument").
//
// The expectation comes from the ARBITRATION ANCHOR, never the row's version:
// one is a claim about the log, the other about the domain's accepted state,
// and a gate is exactly what makes them differ.
type Publisher struct {
	domain Domain
	stream string
	prefix string

	// maxRecord is the domain's [StreamSpec.MaxRecordBytes], which every
	// record is held to before anything is sent.
	maxRecord int64

	log      Appender
	records  VerifiedReader
	rows     Rows
	fence    Fence
	gates    Gates
	waiter   Waiter
	voids    Voids
	identity Identity
	metrics  *metrics.Recorder
	logger   *slog.Logger

	// duplicates is the log's duplicate window ([StreamSpec.Duplicates]):
	// how long the broker collapses an append onto the record its operation
	// id first landed, which a refusal of a gated record tells its caller.
	duplicates time.Duration

	// admission holds this log's ordinary appends out of its gate reserve,
	// nil on a log that keeps none ([ReservationOf]).
	admission Admission

	// nodeID is this node's own identity, handed to every decision as
	// [Stamp.Writer] and checked on every record before it is appended, so
	// the eviction gate has something to compare against.
	nodeID string

	// generation is read fresh on every publish, never captured: a
	// reanchor moves it under a running process.
	generation func() uint32

	// resolveBudget bounds the wait a resolution spends before it answers
	// pending.
	//
	// FIVE SECONDS, the applier's stall grace divided by twelve: an ordinary
	// batch commit and linger many times over, short enough that a caller
	// learns "durable, unresolved here" rather than waiting out a node that
	// has stopped applying.
	resolveBudget time.Duration
}

// DefaultResolveBudget is how long a write waits for its own applier before
// it answers pending. See [Publisher.resolveBudget].
const DefaultResolveBudget = 5 * time.Second

// resolveBudgetKey carries a per-call resolution budget ([WithResolveBudget]).
type resolveBudgetKey struct{}

// WithResolveBudget is ctx with every write made on it waiting at most d for
// this node's applier (its own record's resolution, the caller's session and
// a position it finds itself behind) in place of the publisher's
// [Deps.ResolveBudget]. A non-positive d changes nothing.
//
// One production caller: a seat's executor removal (the engine's mailbox
// retirement), which needs its record durable rather than applied here and
// waits out of a lease's budget. Otherwise tests, for a write that cannot
// resolve on a halted applier. PER CALL rather than a shorter publisher
// budget, which would turn the same case's other writes into `pending` on a
// loaded runner.
func WithResolveBudget(ctx context.Context, d time.Duration) context.Context {
	if d <= 0 {
		return ctx
	}
	return context.WithValue(ctx, resolveBudgetKey{}, d)
}

// budget is how long a write made on ctx waits for this node's applier: the
// call's own budget if it carries one ([WithResolveBudget]), the publisher's
// otherwise.
func (p *Publisher) budget(ctx context.Context) time.Duration {
	if d, ok := ctx.Value(resolveBudgetKey{}).(time.Duration); ok {
		return d
	}
	return p.resolveBudget
}

// Deps is everything a publisher needs that it does not own.
type Deps struct {
	Domain Domain

	// Spec is the stream this publisher writes: the domain's own
	// ([Domain.Stream]) with the byte ceiling this node's Tier A sized,
	// held to the declaration in everything else
	// ([StreamSpec.Instantiates]). Its [StreamSpec.MaxRecordBytes] bounds
	// every record before anything is sent.
	Spec StreamSpec

	Log Appender

	// Signer signs every record this authority appends. REQUIRED: the
	// broker's admission vouches for no record ([recordseal]).
	Signer *recordseal.Signer

	// Records is the same log read by position, through this node's
	// [recordseal.Verifier]. REQUIRED: a write collapsed onto, or found as,
	// a record this call did not append is judged by that record's writer,
	// which only the record names ([Publisher.resolve]). Verified, because
	// a signed frame read raw decodes as nothing.
	Records VerifiedReader

	Rows   Rows
	Fence  Fence
	Gates  Gates
	Waiter Waiter

	// Voids is the reanchor rules this node's applier drops records by (in
	// the engine the same runner as Waiter). REQUIRED ([Voids]).
	Voids Voids

	// Identity is the stream identity of the positions Waiter holds (in the
	// engine the same runner, where the boot's comparison and every live
	// reading land).
	Identity Identity

	Metrics *metrics.Recorder

	// Logger is where this writes. Nil is the package's own component
	// logger, never silence (see loggerOr).
	Logger *slog.Logger

	// Admission is the log's gate reserve: required of a domain that keeps
	// one ([ReservationOf]) and refused on one that does not.
	Admission Admission

	NodeID     string
	Generation func() uint32

	// ResolveBudget overrides [DefaultResolveBudget]. Zero takes it.
	ResolveBudget time.Duration
}

// NewPublisher builds a domain's write authority, refusing a dependency set
// that cannot produce a correct write rather than discovering it at the first
// append.
func NewPublisher(d Deps) (*Publisher, error) {
	switch {
	case d.Domain == nil:
		return nil, fmt.Errorf("statelog: publisher has no domain")
	case d.Log == nil:
		return nil, fmt.Errorf("statelog: publisher has no appender")
	case d.Records == nil:
		return nil, fmt.Errorf("statelog: publisher has no reader of its log: a " +
			"write collapsed onto another node's copy of its operation is judged " +
			"by that copy's writer, which only the record on the log names")
	case d.Rows == nil:
		return nil, fmt.Errorf("statelog: publisher has no rows")
	case d.Fence == nil:
		return nil, fmt.Errorf("statelog: publisher has no fence, and every " +
			"append checks eviction, and a publisher that cannot is one " +
			"that collects acknowledgements for records every node drops")
	case d.Gates == nil:
		return nil, fmt.Errorf("statelog: publisher has no gates")
	case d.Waiter == nil:
		return nil, fmt.Errorf("statelog: publisher has no waiter")
	case d.Voids == nil:
		return nil, fmt.Errorf("statelog: publisher has no reanchor rules: a " +
			"record a reanchor voided wrote no ledger row, and a resolution that " +
			"cannot ask would report it as a ledger contract violation")
	case d.Identity == nil:
		return nil, fmt.Errorf("statelog: publisher has no stream identity: " +
			"every expectation it forms is a sequence on the stream its rows " +
			"came from, and one that cannot ask whether that is still the live " +
			"stream is one whose writes a rebuilt log arbitrates as its own")
	case d.Generation == nil:
		return nil, fmt.Errorf("statelog: publisher has no generation source")
	case d.NodeID == "":
		return nil, fmt.Errorf("statelog: publisher has no node id: the " +
			"eviction gate compares against it, so a record with none is a " +
			"record no gate can drop")
	case ReservationOf(d.Domain).Kept && d.Admission == nil:
		return nil, fmt.Errorf("statelog: %s claims identity and its publisher "+
			"has no admission. Without one its ordinary writes fill the log to "+
			"the broker's ceiling and the eviction that could unpin it is refused "+
			"with them", d.Domain.Name())
	case !ReservationOf(d.Domain).Kept && d.Admission != nil:
		return nil, fmt.Errorf("statelog: %s claims no identity, so its log "+
			"carries no gate record and a reserve would only refuse its ordinary "+
			"writes early", d.Domain.Name())
	}
	spec := d.Spec
	if err := spec.Instantiates(d.Domain); err != nil {
		return nil, err
	}
	// SIGNED AT THE APPENDER, so the next append somebody adds is signed by
	// construction rather than by review.
	log, err := signing(d.Log, d.Signer, d.Domain.Name())
	if err != nil {
		return nil, err
	}
	logger := loggerOr(d.Logger)
	budget := d.ResolveBudget
	if budget <= 0 {
		budget = DefaultResolveBudget
	}
	return &Publisher{
		domain:        d.Domain,
		stream:        spec.Name,
		prefix:        spec.SubjectPrefix,
		maxRecord:     spec.MaxRecordBytes,
		duplicates:    spec.Duplicates,
		log:           log,
		records:       d.Records,
		rows:          d.Rows,
		fence:         d.Fence,
		gates:         d.Gates,
		waiter:        d.Waiter,
		voids:         d.Voids,
		identity:      d.Identity,
		metrics:       d.Metrics,
		logger:        logger,
		admission:     d.Admission,
		nodeID:        d.NodeID,
		generation:    d.Generation,
		resolveBudget: budget,
	}, nil
}

// Publish runs the write authority for one request.
func (p *Publisher) Publish(ctx context.Context, req Request) (Result, error) {
	started := time.Now()
	res, err := p.publish(ctx, req)
	p.observe(started, req, res, err)
	return res, err
}

func (p *Publisher) publish(ctx context.Context, req Request) (Result, error) {
	if req.OpID == "" {
		return Result{}, fmt.Errorf("statelog: write has no op id: it is what " +
			"an ambiguous publish is resolved by, so a write without one has no " +
			"third answer at all")
	}
	if req.Decide == nil {
		return Result{}, fmt.Errorf("statelog: write has no decide function")
	}
	if req.Scope.Empty() {
		return Result{}, fmt.Errorf("statelog: write on %s declares no scope: an "+
			"empty scope claims the record makes nothing stale, which is the one "+
			"claim a record no build may be able to read cannot make", req.Subject)
	}

	// FENCE 0, BEFORE ANYTHING ELSE AND ON EVERY APPEND, for a field read
	// and one indexed local read: it refuses the two nodes that KNOW their
	// writes are meaningless, one whose log was rebuilt under it and one
	// removed from the fleet, whose records every applier drops.
	if err := p.fence0(ctx, req); err != nil {
		return Result{}, err
	}

	// THE SESSION WAIT IS BEFORE THE SNAPSHOT: one taken below the caller's
	// own previous write forms an expectation that write already made
	// stale, and re-snapshotting reads the same anchor every round.
	if err := p.waitSession(ctx, req); err != nil {
		return Result{}, err
	}

	gen := p.generation()
	for round := 1; round <= casRounds; round++ {
		snap, err := p.snapshot(ctx, req)
		if err != nil {
			return p.refusedUnlessUnvouched(ctx, req, round, err)
		}
		if snap.HeldOK {
			// THIS OPERATION ALREADY APPLIED HERE: answered with the first
			// copy's position, never decided again ([Snap.Held]), and before
			// any refusal, which would say nothing landed.
			return Result{
				Outcome:   OutcomeApplied,
				Position:  snap.Held,
				OpID:      req.OpID,
				Version:   snap.Held.Packed(),
				Rounds:    round,
				Collapsed: true,
			}, nil
		}
		if refusal := p.refuseFromSnapshot(req, snap); refusal != nil {
			return p.refusedUnlessUnvouched(ctx, req, round, refusal)
		}
		if snap.Decision.Empty() {
			// NOTHING TO PUBLISH IS A SUCCESS: a no-op update succeeded. No
			// record, so no position; the version is the one decided from.
			return Result{
				Outcome: OutcomeApplied,
				OpID:    req.OpID,
				Version: snap.Decision.Version,
				Rounds:  round,
			}, nil
		}
		// BEFORE ANY BROKER CALL: an unstamped record is one no eviction
		// gate can drop, and nothing after the append takes it back.
		if err = p.stamped(req, snap); err != nil {
			return Result{Rounds: round}, err
		}
		// A DECISION THIS NODE'S LEDGER CANNOT VOUCH FOR IS NEVER PUBLISHED
		// ([Publisher.vouches]). Read AFTER the snapshot, so it sees an
		// adoption that replaced the rows this decision read.
		vouched, err := p.vouches(ctx, req)
		if err != nil {
			return Result{Rounds: round}, err
		}
		if !vouched {
			p.logger.WarnContext(ctx, "statelog_write_unvouched",
				"domain", p.domain.Name(), "subject", req.Subject.String(),
				"op_id", req.OpID, "minted_at", mintedAt(req.OpID),
				"detail", "this operation was minted before this node's "+
					"operation ledger may have lost rows to the ledger's "+
					"retention sweep, and it holds no row saying "+
					"whether the operation already applied, so it is "+
					"answered unknown rather than decided a second time; "+
					"a node whose ledger lost nothing that far back can "+
					"answer it")
			return Result{Outcome: OutcomeUnknown, OpID: req.OpID, Rounds: round,
				Unvouched: true}, nil
		}

		expect, behind, err := p.expectation(ctx, req, snap, gen)
		if err != nil {
			return Result{Rounds: round}, err
		}
		if behind != nil {
			// A PEER WROTE IN THIS GENERATION AND THIS NODE IS BEHIND; never
			// expectation zero, since the subject is ahead, not empty. Under
			// the session wait's budget, refusing `behind` on expiry: being
			// behind is unbounded by construction, and the caller's context
			// is no budget (none waits for ever, a deadline hides the reason).
			//nolint:govet // shadow: scoped to this block; see .golangci.yml
			if err := p.waitBehind(ctx, req, *behind); err != nil {
				return Result{Rounds: round}, err
			}
			continue
		}

		res, disp, err := p.attempt(ctx, req, snap, expect, gen, round)
		if err != nil || disp == dispDone {
			return res, err
		}
		if disp == dispRetake {
			continue
		}

		// THE BROKER REFUSED THE EXPECTATION: a trimmed anchor is retried
		// ONCE at zero within this round; anything else re-decides.
		zero, err := p.afterRejection(ctx, req, snap, expect)
		if err != nil {
			return Result{Rounds: round}, err
		}
		if zero == nil {
			continue
		}
		res, disp, err = p.attempt(ctx, req, snap, zero, gen, round)
		if err != nil || disp == dispDone {
			return res, err
		}
	}
	return Result{Rounds: casRounds}, fmt.Errorf("%w: %s kept changing under this write",
		ErrConflict, req.Subject)
}

// refusedUnlessUnvouched is a refusal this round settled on, or `unknown`
// where this node's operation ledger cannot vouch for the operation.
//
// A refusal is a decision too, and the sharpest case of one taken on rows that
// may hold this operation's own first application: they refuse BECAUSE it
// happened (a create finds its guarding row, an update its version moved).
// Where the ledger may have lost that row, the only true answer is that this
// node cannot say. Except an [Unavailable]: a fact about this node, true
// whether or not the operation applied, naming its remedy. A ledger that
// cannot be read, or a caller that has gone, leaves the refusal standing.
func (p *Publisher) refusedUnlessUnvouched(ctx context.Context, req Request,
	round int, refusal error) (Result, error) {

	var unavailable *Unavailable
	if ctx.Err() != nil || errors.As(refusal, &unavailable) {
		return Result{Rounds: round}, refusal
	}
	vouched, err := p.vouches(ctx, req)
	if err != nil || vouched {
		return Result{Rounds: round}, refusal
	}
	p.logger.WarnContext(ctx, "statelog_write_unvouched",
		"domain", p.domain.Name(), "subject", req.Subject.String(),
		"op_id", req.OpID, "minted_at", mintedAt(req.OpID),
		"refusal", refusal.Error(),
		"detail", "this operation was minted before this node's operation "+
			"ledger may have lost rows, it holds no row saying whether the "+
			"operation already applied, and its decision refused on rows that "+
			"may hold that very application — so it is answered unknown "+
			"rather than refused; a node whose ledger lost nothing that far "+
			"back can answer it")
	return Result{Outcome: OutcomeUnknown, OpID: req.OpID, Rounds: round,
		Unvouched: true}, nil
}

// snapshot takes one round's snapshot, handing the decision its [Stamp]:
// this node's id and the generation of the checkpoint the same transaction
// read.
func (p *Publisher) snapshot(ctx context.Context, req Request) (Snap, error) {
	return p.rows.Snapshot(ctx, req.Subject, req.Scope, req.OpID,
		func(tx *sql.Tx, held OpEntry) error {
			if err := p.heldHere(req, held); err != nil {
				return err
			}
			if req.Standing != nil {
				return req.Standing(tx, held.Position)
			}
			return nil
		},
		func(tx *sql.Tx, checkpoint Position) (Decision, error) {
			return req.Decide(tx, p.stampAt(checkpoint))
		})
}

// heldHere judges a row of this node's ledger found under the write's own
// operation id: nil when it answers for this write, a refusal when it does not.
// One judgement for both places the ledger answers, before and after the
// broker ([Snap.Held], [Publisher.resolve]).
//
// The ledger keeps an id's FIRST row, and the id is the caller's, so the same
// id sent with a write to another object finds the first object's row (or
// collapses onto its record) and would confirm a write to b at a position on
// a, with nothing written to b. It is refused `op_reused` instead.
func (p *Publisher) heldHere(req Request, held OpEntry) error {
	if held.Subject == p.subjectOf(req.Subject) {
		return nil
	}
	return &Unavailable{
		Reason: ReasonOpReused,
		Detail: fmt.Sprintf("operation %q landed on %s at %s, and this write is "+
			"to %s. An operation id names one write, so a different one needs "+
			"a fresh id", req.OpID, held.Subject, held.Position,
			p.subjectOf(req.Subject)),
		Position: held.Position,
		OpID:     req.OpID,
	}
}

// stampAt is the stamp a decision taken at checkpoint is handed: one function
// for the handing and the check in [Publisher.stamped], so they cannot differ.
func (p *Publisher) stampAt(checkpoint Position) Stamp {
	return Stamp{Writer: p.nodeID, Gen: checkpoint.Generation}
}

// stamped refuses a decision whose record does not carry the stamp it was
// decided under ([Stamp]).
//
// The envelope is decoded by the domain's own reader, the only one any applier
// uses. The record must also carry the REQUEST's op id: the request's is the
// message id and what the ledger is asked for, the record's is what the
// applier writes there, and a mismatch reads as an operation that never landed.
func (p *Publisher) stamped(req Request, snap Snap) error {
	want := p.stampAt(snap.Checkpoint)
	env, err := p.domain.Envelope(snap.Decision.Payload)
	if err != nil {
		return fmt.Errorf("statelog: the %s record decided for %s does not "+
			"decode through its own domain's envelope reader, which every "+
			"applier reads it through first: %w", p.domain.Name(), req.Subject, err)
	}
	switch {
	case env.Writer != want.Writer:
		return fmt.Errorf("statelog: the %s record decided for %s names writer "+
			"%q, and this publisher is %q. The eviction gate compares a record's "+
			"writer against every eviction on the log, so a record carrying "+
			"another node's id, or none, is one no eviction of this node can "+
			"drop; the domain's decision must put [Stamp.Writer] in its envelope",
			p.domain.Name(), req.Subject, env.Writer, want.Writer)
	case env.Gen != want.Gen:
		return fmt.Errorf("statelog: the %s record decided for %s is stamped "+
			"generation %d, and the snapshot it was decided from is at %d: the "+
			"domain's decision must put [Stamp.Gen] in its envelope",
			p.domain.Name(), req.Subject, env.Gen, want.Gen)
	case env.OpID != req.OpID:
		return fmt.Errorf("statelog: the %s record decided for %s carries "+
			"operation %q and the write is %q. The ledger an ambiguous publish "+
			"is resolved by is keyed on the record's, so this write could never "+
			"be answered for", p.domain.Name(), req.Subject, env.OpID, req.OpID)
	case req.NodeGate && !p.domain.NodeGate(env):
		// A NODE GATE IS JUDGED FROM THE RECORD: a flag the caller set alone
		// would let an ordinary write past the three fences it excuses.
		// Asked of the NODE gate, not the apply gate, which a purge has too.
		return fmt.Errorf("statelog: the %s record decided for %s is a %s %q "+
			"record, which is not a node's eviction or readmission, and the "+
			"write is flagged a node gate: only those may pass the fences a "+
			"node gate is excused",
			p.domain.Name(), req.Subject, env.Kind, env.Op)
	}
	return nil
}

// disposition is what one append attempt leaves the round loop to do. Three,
// because collapsing any pair sends a never-refused record, or an ambiguous
// publish, through the rejection discriminator.
type disposition int

const (
	// dispDone: the write is finished, with a result or a refusal.
	dispDone disposition = iota

	// dispRejected: the broker refused the expectation. Only the
	// discriminator can say whether that was a lost race or a trimmed
	// anchor, and the two have opposite remedies.
	dispRejected

	// dispRetake: nothing landed. Take a fresh snapshot and decide
	// again — there is nothing to discriminate.
	dispRetake
)

// attempt publishes once and reads the answer.
func (p *Publisher) attempt(ctx context.Context, req Request, snap Snap, expect *uint64, gen uint32, round int) (Result, disposition, error) {
	// FENCE 0 AGAIN: an eviction, or a reading of a rebuilt log, may have
	// landed while earlier rounds lost races. Its refusal ends the write
	// HERE, never through [classify], which reads any non-broker error as no
	// answer and so would probe, retake and run out of rounds as a conflict.
	if err := p.fence0(ctx, req); err != nil {
		return Result{Rounds: round}, dispDone, err
	}
	// THE DOMAIN'S LARGEST RECORD, for every append (a node gate's too),
	// before the reserve, which is sized by it. The broker also refuses it
	// at max_msg_size ([StreamSpec.MaxAppendBytes]), but only after sending
	// and naming a stream setting. No retry shrinks it, so the write ends.
	if size := int64(len(snap.Decision.Payload)); size > p.maxRecord {
		return Result{Rounds: round}, dispDone, &Unavailable{
			Reason: ReasonRecordTooLarge,
			Detail: fmt.Sprintf("the record is %d bytes and %s takes none larger "+
				"than %d (the %s log's declared largest record, which its gate "+
				"reserve is sized by and the broker enforces as the stream's "+
				"max_msg_size), so no retry places it, here or on any node: split "+
				"the change into smaller writes", size, p.stream, p.maxRecord,
				p.domain.Name()),
			OpID:  req.OpID,
			Cause: queue.ErrTooLarge,
		}
	}
	// THE RESERVE, for every append but a node gate's, held until the
	// broker answers and not past it (the resolution can wait a whole
	// budget): the reading it admits against cannot see this append. A
	// refusal ends the write here, for fence 0's reason.
	release := func() {}
	if p.admission != nil && !req.NodeGate {
		var err error
		if release, err = p.admission.Admit(ctx, appendBytes(snap.Decision.Payload)); err != nil {
			return Result{Rounds: round}, dispDone, withOpID(err, req.OpID)
		}
	}
	seq, duplicate, appendErr := p.log.Append(ctx, p.subjectOf(req.Subject), req.OpID, expect, snap.Decision.Payload)
	release()
	f, detail := classify(appendErr)
	switch f {
	case faultNone:
		at := Position{Stream: p.stream, Generation: gen, Seq: seq}
		if err := at.Valid(); err != nil {
			return Result{Rounds: round}, dispDone, err
		}
		// WHOSE RECORD IS AT `at` is what the gates are asked about: this
		// call's own, with its payload, or on a duplicate the copy the broker
		// collapsed it onto, whose body only the log holds.
		landed, body := landedOwn, snap.Decision.Payload
		if duplicate {
			landed, body = landedCopy, nil
		}
		res, err := p.resolve(ctx, req, at, landed, snap.Checkpoint.Generation, body)
		res.Rounds = round
		// A DUPLICATE ACKNOWLEDGEMENT means this call's decision was never
		// stored ([Result.Collapsed]). OR-ed into what the resolution
		// concluded, which may already have found it collapsed.
		res.Collapsed = res.Collapsed || (duplicate && res.Outcome != "")
		return res, dispDone, err

	case faultFull:
		// The blocking term is named by `crewlet retention status`, not
		// fetched here: it is a property of the last tick, and a refusal
		// path takes no coordination round trip.
		remedy := "raise the stream's byte ceiling (`crewlet retention " +
			"set-capacity`) or unblock the trim (`crewlet retention status` " +
			"names the term holding it)"
		if req.NodeGate && p.admission != nil {
			// A GATE RECORD found its reserve spent too, and its gesture is
			// often what unblocks the trim: only a larger ceiling helps.
			remedy = "this was a gate record, refused past even the reserve " +
				"kept for it, so raise the stream's byte ceiling with " +
				"`crewlet retention set-capacity`: no retry makes room"
		}
		return Result{Rounds: round}, dispDone, &Unavailable{
			Reason: ReasonLogFull,
			Detail: fmt.Sprintf("the broker refused to store the record: %s. A "+
				"full log refuses appends rather than dropping records, so %s",
				detail, remedy),
			OpID: req.OpID,
		}

	case faultTooLarge:
		return Result{Rounds: round}, dispDone, &Unavailable{
			Reason: ReasonRecordTooLarge,
			// THE RECORD'S OWN SIZE, known only here before signing, beside
			// the limit [classify] names; the log's ceiling is not involved.
			Detail: fmt.Sprintf("the record is %d bytes before its signature and "+
				"no retry places it, here or on any node: %s",
				len(snap.Decision.Payload), detail),
			OpID:  req.OpID,
			Cause: queue.ErrTooLarge,
		}

	case faultRefused:
		return Result{Rounds: round}, dispDone, &Unavailable{
			Reason: ReasonBrokerRefused,
			Detail: fmt.Sprintf("the broker refused to store the record (%s), a "+
				"refusal this build has no remedy for, so the broker's own words "+
				"are the one to act on, and asking again changes nothing", detail),
			OpID:  req.OpID,
			Cause: appendErr,
		}

	case faultUnknown:
		res, err := p.classifyAmbiguous(ctx, req, snap, gen, detail)
		res.Rounds = round
		if err != nil || res.Outcome != "" {
			return res, dispDone, err
		}
		// Nothing landed, or somebody else's record did. Either way
		// there is no rejection to discriminate: take a fresh snapshot.
		return Result{Rounds: round}, dispRetake, nil

	case faultUnsettled:
		// THE BROKER SAID THIS OPERATION'S RECORD MAY YET LAND (in flight
		// under this op id, or committed under a closed store), so it is
		// resolved as an unanswered append: it may already be visible.
		res, err := p.classifyAmbiguous(ctx, req, snap, gen, detail)
		res.Rounds = round
		if err != nil || res.Outcome != "" {
			return res, dispDone, err
		}
		// AND NEVER A RETAKE: a fresh snapshot would decide against a state
		// about to hold this very write (a create told it exists). Nor a
		// refusal, which a caller re-files under a fresh id and writes twice.
		// Only the same op id retries safely, meeting the duplicate ack.
		p.logger.Warn("statelog_publish_unsettled",
			"domain", p.domain.Name(), "subject", p.subjectOf(req.Subject),
			"op_id", req.OpID, "publish_error", detail)
		return Result{Outcome: OutcomeUnknown, OpID: req.OpID, Rounds: round},
			dispDone, nil

	case faultRejected:
		p.count(metrics.StatelogPublishRejections, metrics.Attrs{
			"domain": p.domain.Name(), "subject_kind": req.Subject.Kind,
		})
		return Result{Rounds: round}, dispRejected, nil
	}
	// UNREACHABLE while every fault has an arm; refused rather than
	// defaulted, so a new fault is never retried as a lost race.
	return Result{Rounds: round}, dispDone, fmt.Errorf("statelog: a publish to %s "+
		"ended in fault %d, which this build does not classify: %s", req.Subject,
		f, detail)
}

// withOpID puts the write's operation id on a refusal that was made without
// it, so a caller retrying under the id the refusal names retries this write.
func withOpID(err error, opID string) error {
	var refusal *Unavailable
	if errors.As(err, &refusal) && refusal.OpID == "" {
		refusal.OpID = opID
	}
	return err
}

// refuseFromSnapshot is every refusal a committed snapshot settles on its
// own, before any broker call.
func (p *Publisher) refuseFromSnapshot(req Request, snap Snap) error {
	// STEP 0, the deferred-SCOPE probe, is a correctness precondition: a
	// record this node cannot decode leaves its rows stale here, and if it
	// is also trimmed, the retry at zero would silently overwrite it. Keyed
	// on SCOPE, never subject: a deferred record may have rewritten a
	// neighbour's row, and nothing on the neighbour's subject says so.
	if snap.Deferred {
		return &Unavailable{
			Reason: ReasonDeferred,
			Detail: fmt.Sprintf("this node holds a record at version %d it cannot "+
				"decode, at %s, whose scope covers %s, so its rows are stale here "+
				"and another node can serve this write",
				snap.Deferral.Version, snap.Deferral.Position, req.Subject),
			Position: snap.Deferral.Position,
			OpID:     req.OpID,
		}
	}
	if req.Pattern != PatternCreate {
		return nil
	}
	// A DELETION MARKER OUTLIVES THE GUARDING ROW a purge deletes, so on a
	// subject a producer can derive the object stays deleted for ever, and
	// the writer must not be told "exists" or, below the floor, "succeeded"
	// for a record every applier drops.
	if snap.Deleted {
		return &Unavailable{
			Reason: ReasonDeleted,
			Detail: fmt.Sprintf("%s was deleted and stays deleted", req.Subject),
			OpID:   req.OpID,
		}
	}
	// BELOW THE TRIM FLOOR THE ROW, NOT THE BROKER CLAIM, IS THE GUARD: a
	// trimmed claim leaves its subject empty, so zero succeeds again, while
	// the floor theorem keeps the row present on every serving node.
	if snap.Guard {
		return fmt.Errorf("%w: %s", ErrExists, req.Subject)
	}
	return nil
}

// expectation forms what the broker will arbitrate against, and reports a
// position to wait for instead when this node is simply behind.
//
// THE ANCHOR AND NOTHING ELSE, never the row's version ([Publisher]): an
// evicted node's accepted-then-gated append leaves the broker's last sequence
// above every node's version, and an expectation from the row re-reads the
// number that was refused until it runs out of rounds.
func (p *Publisher) expectation(ctx context.Context, req Request, snap Snap, gen uint32) (*uint64, *Position, error) {
	if !req.Pattern.expects() {
		return nil, nil, nil
	}
	// AN ANCHOR AT THE CURRENT GENERATION, and seq > 0 is what makes it
	// one. Zero is a PROBE, not a licence to publish at zero.
	if seq := snap.anchorIn(gen); seq > 0 {
		return &seq, nil, nil
	}

	// NO ANCHOR HERE covers a subject never written, one last written before
	// a reanchor, and one whose only record this node has not applied; only
	// the broker tells them apart.
	subject := p.subjectOf(req.Subject)
	seq, found, err := p.log.LastSeq(ctx, subject)
	if err != nil {
		return nil, nil, fmt.Errorf("statelog: read the last message on %s: %w", subject, err)
	}
	if !found {
		// Nothing there: zero is correct, and fenced at the SNAPSHOT's
		// checkpoint ([Snap.Checkpoint]), since being wrong is a lost update.
		if err := p.clearForZero(ctx, req, snap.Checkpoint); err != nil {
			return nil, nil, err
		}
		zero := uint64(0)
		return &zero, nil, nil
	}
	// A RECORD THE SNAPSHOT'S CHECKPOINT ALREADY COVERS is the expectation,
	// not a position to wait for. Only a RESTORED log's reanchor reaches
	// this ([ReanchorRestored]): its checkpoint sits at the log's end with
	// every anchor in the generation before, so waiting returns at once and
	// every write ran out of rounds as a conflict nobody caused. At the
	// snapshot's checkpoint, as the zero fence is.
	if snap.Checkpoint.Generation == gen && seq <= snap.Checkpoint.Seq {
		return &seq, nil, nil
	}
	at := Position{Stream: p.stream, Generation: gen, Seq: seq}
	return nil, &at, nil
}

// afterRejection discriminates a rejection: the one place a trimmed anchor
// and a lost race are told apart.
//
// It returns a non-nil expectation ONLY for the trimmed-anchor retry, which
// publishes snap's decision again, so snap is what the fence is asked about.
func (p *Publisher) afterRejection(ctx context.Context, req Request, snap Snap, expect *uint64) (*uint64, error) {
	subject := p.subjectOf(req.Subject)

	// ON EVERY REJECTION, with no cheaper pre-filter: the lost-race path
	// needs the sequence anyway, to know what to wait for.
	seq, found, err := p.log.LastSeq(ctx, subject)
	if err != nil {
		return nil, fmt.Errorf("statelog: discriminate a rejection on %s: %w", subject, err)
	}

	switch {
	case !found && expect != nil && *expect > 0:
		// THE ANCHOR WAS TRIMMED. The server rescues only an expectation of
		// zero on an empty subject, so any other is refused FOR EVER and a
		// quiet object becomes unwritable. Retrying at zero is safe here
		// and ONLY here, under the floor theorem (package doc), once per
		// ROUND: each round decides afresh, and real contention still runs
		// out of rounds. An empty subject is also what a rebuilt log looks
		// like, hence the identity check after the fence's read
		// ([Publisher.clearForZero]). Fenced at the SNAPSHOT's checkpoint:
		// the theorem's conclusion is about the rows the decision read.
		if err := p.clearForZero(ctx, req, snap.Checkpoint); err != nil {
			return nil, err
		}
		zero := uint64(0)
		return &zero, nil

	case !found:
		// A lost create race whose winner's record has since been trimmed:
		// retake the snapshot, and the guarding row answers.
		return nil, nil

	case expect != nil && seq < *expect:
		// IMPOSSIBLE ON A HEALTHY STREAM: the anchor came from an applied
		// record, so an earlier last message means a store and a stream
		// restored out of step. Retrying would livelock or overwrite.
		return nil, &Unavailable{
			Reason: ReasonSkew,
			Detail: fmt.Sprintf("%s holds sequence %d but this node expected %d, "+
				"which it read from a record it applied: a store and a stream "+
				"restored out of step", subject, seq, *expect),
			Position: Position{Stream: p.stream, Seq: seq},
			OpID:     req.OpID,
		}

	default:
		// A LOST RACE. Reach the winner's position before re-deciding, or
		// the retake re-reads the stale anchor every round; under the write
		// path's budget, as the behind branch is.
		at := Position{Stream: p.stream, Generation: p.generation(), Seq: seq}
		if err := p.waitBehind(ctx, req, at); err != nil {
			return nil, err
		}
		return nil, nil
	}
}

// classifyAmbiguous is the ordered classification of a publish whose outcome
// nobody knows, answering in ONE of four ways. A zero Outcome with a nil error
// means nothing landed: retake the snapshot.
//
// gen is the generation the append was made in, the number space of the
// broker's answer, and so the only anchor it compares with ([Snap.anchorIn]).
func (p *Publisher) classifyAmbiguous(ctx context.Context, req Request, snap Snap, gen uint32, detail string) (Result, error) {
	subject := p.subjectOf(req.Subject)
	seq, found, err := p.log.LastSeq(ctx, subject)
	switch {
	case err != nil:
		// NOTHING ANSWERS AT ALL: unknown, with the op id, since retrying
		// under the SAME id is the only safe retry.
		p.logger.WarnContext(ctx, "statelog_publish_unknown",
			"domain", p.domain.Name(), "subject", subject,
			"op_id", req.OpID, "publish_error", detail, "probe_error", err.Error())
		return Result{Outcome: OutcomeUnknown, OpID: req.OpID}, nil

	case !found, seq <= snap.anchorIn(gen):
		// At or below the anchor decided against: nothing of this write's
		// landed, so re-decide. THIS GENERATION'S anchor only: a restored
		// log counts again below an older one, so a landed record read as
		// below it and was decided again until it came back a conflict.
		return Result{}, nil

	default:
		// SOMETHING LANDED ABOVE THE ANCHOR; whether it is this write's, and
		// whether it applied, the resolution answers from this node's rows.
		p.logger.DebugContext(ctx, "statelog_publish_ambiguous",
			"domain", p.domain.Name(), "subject", subject,
			"op_id", req.OpID, "at", seq, "publish_error", detail)
		at := Position{Stream: p.stream, Generation: p.generation(), Seq: seq}
		return p.resolve(ctx, req, at, landedFound, snap.Checkpoint.Generation, nil)
	}
}

// landing is what a resolution knows, before it reads anything, about the
// record at the position it resolves.
type landing int

const (
	// landedOwn: the broker acknowledged THIS call's append there, so the
	// record is this node's, under this operation.
	landedOwn landing = iota

	// landedCopy: the broker COLLAPSED this call's append onto a copy of
	// the operation inside its duplicate window, written by whichever node
	// published that copy (this one earlier, or another handed the same
	// operation).
	landedCopy

	// landedFound: the ambiguous path found the position newest on the
	// subject; it may be anybody's record under any operation.
	landedFound
)

// unresolved is the answer for a durable append this call could not resolve
// here (the wait ended, a read past it failed, or [Request.NoWait]): what is
// known of the record at `at`. waited is [Result.Waited].
//
// PENDING only where the record is THIS operation's AND every node will apply
// it ([OutcomePending]): this call's own append (fence 0 just cleared it), this
// node's earlier copy, or another node's copy whose writer still stands there
// ([Publisher.copyStands]); an evicted writer's copy applies nowhere. Anything
// else is UNKNOWN WITH NO POSITION ([Result.Position]), and the same op id
// retried meets the copy's writer's gate once this node has applied that far.
// A caller that stopped waiting is not read for: its own append is pending,
// anything else unknown.
func (p *Publisher) unresolved(ctx context.Context, req Request, at Position,
	landed landing, waited bool) Result {

	pending := Result{Outcome: OutcomePending, Position: at, OpID: req.OpID, Waited: waited}
	unknown := Result{Outcome: OutcomeUnknown, OpID: req.OpID, Waited: waited}
	if landed == landedOwn {
		return pending
	}
	if ctx.Err() != nil {
		return unknown
	}
	env, body, found, err := p.recordAt(ctx, at)
	if err != nil || !found || env.OpID != req.OpID {
		return unknown
	}
	if env.Writer == p.nodeID {
		return pending
	}
	stands, why, err := p.copyStands(ctx, req, at, env, body)
	if err != nil || !stands {
		if err != nil {
			why = err.Error()
		}
		p.logger.WarnContext(ctx, "statelog_resolve_copy_unvouched",
			"domain", p.domain.Name(), "subject", req.Subject.String(),
			"op_id", req.OpID, "position", at.String(), "writer", env.Writer,
			"why", why)
		return unknown
	}
	return pending
}

// copyStands reports whether another node's copy of this operation at `at`
// applies on every node as far as this node can tell, and if not, why.
//
// First the applier's own gates on the copy's writer ([Publisher.voided]). If
// this node's rows have reached `at`, they are the whole answer (the
// checkpoint is read BEFORE the rows, so a commit between cannot hide a
// record). Otherwise the log: the writer's last eviction-subject record
// ([lastStanding]) is held by the rows if at or below their checkpoint;
// between them and `at` it is the standing at the copy (an eviction holds it,
// a readmission does not); above `at`, what held at the copy cannot be read,
// so the copy is not vouched for.
func (p *Publisher) copyStands(ctx context.Context, req Request, at Position,
	env Envelope, body []byte) (bool, string, error) {

	committed := p.waiter.Reached()
	reason, gated, err := p.voided(ctx, req, at, true, env.Writer, env.Gen, body)
	if err != nil {
		return false, "", err
	}
	if gated {
		return false, fmt.Sprintf("this node's rows hold the copy under the %s gate", reason), nil
	}
	probe, probed := p.domain.(EvictionProbe)
	if !probed {
		return true, "", nil
	}
	if committed.Stream == at.Stream && committed.Generation == at.Generation &&
		committed.Seq >= at.Seq {
		return true, "", nil
	}
	seq, evicts, found, err := lastStanding(ctx, probe, p.prefix, p.stream,
		standing{VerifiedReader: p.records, last: p.log}, env.Writer)
	switch {
	case err != nil:
		return false, "", err
	case !found:
		return true, "", nil
	case committed.Stream == at.Stream && committed.Generation == at.Generation &&
		seq <= committed.Seq:
		return true, "", nil
	case seq < at.Seq && !evicts:
		return true, "", nil
	case seq < at.Seq:
		return false, fmt.Sprintf("the log evicts %s at sequence %d, below the copy",
			env.Writer, seq), nil
	}
	return false, fmt.Sprintf("%s's last standing, at sequence %d, is above both "+
		"this node's rows and the copy, so what held at the copy cannot be read",
		env.Writer, seq), nil
}

// resolve answers what a durable record at `at` actually produced, for the
// ambiguous path and for every acknowledged write, a successful wait included:
// an evicted node's applier drops its own record well inside the budget, and
// the wait alone would answer applied.
//
// No coordination read is needed: the applier is contiguous, so a gate that
// dropped `at` is in rows the wait covered. Gates are asked about the record's
// own WRITER, read off the log for a record this call did not append, the
// framework's rules first ([Publisher.voided]); gen and body are this call's
// own for [landedOwn]. Past the acknowledgement only a conclusion the reads
// reached is a failure; anything unfinished answers what is known of the
// record ([Publisher.unresolved]) (package doc, "Resolving a durable record").
func (p *Publisher) resolve(ctx context.Context, req Request, at Position, landed landing, gen uint32, body []byte) (Result, error) {
	if req.NoWait && landed != landedFound {
		// THE ANSWER THE ACKNOWLEDGEMENT ESTABLISHES ([Request.NoWait]): the
		// same outcome as a timed-out wait, a different fact ([Result.Waited]).
		// A copy is still asked whose it is.
		return p.unresolved(ctx, req, at, landed, false), nil
	}
	// WITHIN WHAT THE ASKER LEFT ([WithReplyMargin]): `pending` said after
	// the asker stopped listening is a node heard as gone.
	waitCtx, cancel, _ := WaitWithin(ctx, p.budget(ctx))
	defer cancel()

	if err := p.waiter.WaitApplied(waitCtx, req.Scope, at); err != nil {
		// DURABLE, UNRESOLVED HERE, budget spent or caller gone: never
		// "applied", never a failure. Waited, so this pending says the NODE did
		// not arrive in the time it was given.
		return p.unresolved(ctx, req, at, landed, true), nil
	}
	// unread answers a failed read past the wait by what is known of the
	// record, not by the failure: the caller stopping, or a store busy or
	// closed during an adoption, fails the read, not the durable write.
	unread := func(err error) (Result, error) {
		if ctx.Err() == nil {
			p.logger.WarnContext(ctx, "statelog_resolve_incomplete",
				"domain", p.domain.Name(), "subject", req.Subject.String(),
				"op_id", req.OpID, "position", at.String(), "err", err.Error())
		}
		return p.unresolved(ctx, req, at, landed, true), nil
	}

	// THE LEDGER IS ONLY ASKED WHERE THERE IS ONE: a ledgerless domain's
	// empty table would answer "absent" for its own records too.
	if p.domain.OpsTable() != "" {
		entry, ok, err := p.rows.Op(ctx, req.OpID)
		if err != nil {
			return unread(fmt.Errorf("statelog: read the operation ledger: %w", err))
		}
		if ok {
			if err := p.heldHere(req, entry); err != nil {
				return Result{}, err
			}
			// COLLAPSED UNLESS THE ROW IS PROVABLY THIS CALL'S: the broker
			// acknowledged this call's append at the very position the ledger
			// names. Otherwise the row is a copy this call did not decide, perhaps
			// from a behind node's stale rows (a key mint would hand back the
			// wrong number).
			return Result{
				Outcome:   OutcomeApplied,
				Position:  entry.Position,
				OpID:      req.OpID,
				Version:   entry.Position.Packed(),
				Collapsed: landed != landedOwn || entry.Position != at,
				Waited:    true,
			}, nil
		}
	}

	// ABSENT: establish whose record `at` holds before asking the two
	// questions that make absence mean other than "somebody else won".
	writer, ours := p.nodeID, true
	if landed != landedOwn {
		env, read, found, err := p.recordAt(ctx, at)
		if (err != nil || !found) && landed == landedCopy {
			// THIS OPERATION'S RECORD BY THE BROKER'S WORD, so a failed read is
			// answered as any read past the acknowledgement.
			if err == nil {
				err = fmt.Errorf("statelog: the log no longer holds the record at %s "+
					"its acknowledgement named", at)
			}
			return unread(err)
		}
		if err != nil || !found {
			// NOBODY HERE CAN NAME ITS WRITER, so no gate can be judged; and NO
			// POSITION, since it is not known to be this operation's
			// ([Result.Position]).
			p.logger.WarnContext(ctx, "statelog_resolve_unread",
				"domain", p.domain.Name(), "subject", req.Subject.String(),
				"op_id", req.OpID, "position", at.String(), "found", found,
				"err", fmt.Sprint(err))
			return Result{Outcome: OutcomeUnknown, OpID: req.OpID, Waited: true}, nil
		}
		ours = env.OpID == req.OpID
		switch {
		case ours:
			// THIS OPERATION'S COPY: its body is what a payload-keyed gate reads.
			writer, gen, body = env.Writer, env.Gen, read
		case landed == landedCopy:
			// THE DUPLICATE WINDOW IS KEYED ON THE OPERATION ID, so a copy
			// without it is a log nobody can reason about.
			return Result{}, fmt.Errorf("statelog: the broker collapsed operation "+
				"%q onto the %s record at %s, which carries operation %q. The "+
				"duplicate window is keyed on the operation id, so this log does "+
				"not hold what its acknowledgement said", req.OpID, p.domain.Name(),
				at, env.OpID)
		}
	}
	if !ours {
		// ANOTHER OPERATION'S RECORD: the gates are asked about this node,
		// from the subject alone.
		body = nil
	}
	reason, gated, err := p.voided(ctx, req, at, ours, writer, gen, body)
	if err != nil {
		return unread(err)
	}
	if gated && !ours {
		// THE GATE HOLDS THIS NODE AT ANOTHER OPERATION'S RECORD, so a refusal
		// would claim this write's own append (below `at`, if it landed)
		// applies nowhere: the ledger's silence read, so asked only where the
		// ledger vouches, and unknown where it cannot.
		vouched, vouchErr := p.vouches(ctx, req)
		if vouchErr != nil {
			return Result{}, vouchErr
		}
		if !vouched {
			return Result{Outcome: OutcomeUnknown, OpID: req.OpID, Unvouched: true}, nil
		}
	}
	if gated {
		// THE WRITE APPLIED NOWHERE AND NEVER WILL: a refusal, never a
		// re-decide, which would publish another record nothing applies.
		//
		// NOT COUNTED AS A DROPPED RECORD: this node's applier counted it when
		// the wait passed `at`, and the refusal is counted by reason
		// ([Publisher.observe]). The line keeps writer, position, and whether
		// the record is this operation's ([Unavailable.Dropped]).
		p.logger.WarnContext(ctx, "statelog_write_gated",
			"domain", p.domain.Name(), "subject", req.Subject.String(),
			"gate", string(reason), "position", at.String(), "op_id", req.OpID,
			"writer", writer, "dropped", ours)
		// THE OPERATION ID IS SPENT for the duplicate window: sent again before
		// then it collapses onto the record and is refused again. Afterwards it
		// writes afresh, and cannot apply twice.
		spent := fmt.Sprintf("the broker holds the operation id for %s from "+
			"when that record landed, so the id sent again before then is "+
			"collapsed onto it and refused the same way", p.duplicates)
		var detail string
		switch {
		case !ours:
			detail = fmt.Sprintf("the newest record on the subject, at %s, is "+
				"another operation's, and the gate holds this node there: whatever "+
				"it appended under this operation lies below that record and "+
				"applies nowhere, and if that landed, %s", at, spent)
		case writer != p.nodeID:
			detail = fmt.Sprintf("the record at %s is node %s's copy of this "+
				"operation, which this node's append was collapsed onto, and it "+
				"was durable and applied nowhere: %s", at, writer, spent)
		default:
			detail = fmt.Sprintf("the record at %s was durable and applied "+
				"nowhere: %s", at, spent)
		}
		// NAME THE COPY'S WRITER when the gate blames it ([Reason.BlamesWriter]),
		// or a caller reads `evicted` as this node's own and sends the write away
		// from a node that can make it; never beside `deleted`, which holds this
		// node's record too. The window's length is then the retry hint.
		var copyWriter string
		var after time.Duration
		if writer != p.nodeID && reason.BlamesWriter() {
			copyWriter = writer
			after = p.duplicates
		}
		return Result{}, &Unavailable{
			Reason:     reason,
			Detail:     detail,
			Position:   at,
			OpID:       req.OpID,
			CopyWriter: copyWriter,
			Dropped:    ours,
			RetryAfter: after,
		}
	}

	// A DOMAIN WITH NO LEDGER answers from the wait and the gates alone,
	// licensed by its narrow contract (a total apply under a monotone version
	// guard, never reporting a committed position); else every write would
	// read its empty table as "somebody else won" for ever.
	if p.domain.OpsTable() == "" {
		return Result{
			Outcome:  OutcomeApplied,
			Position: at,
			OpID:     req.OpID,
			Version:  at.Packed(),
			Waited:   true,
		}, nil
	}

	// THE LEDGER'S SILENCE MEANS SOMETHING ONLY WHERE IT CAN VOUCH, asked for
	// the ours arm too: a sweep loses exactly such a row.
	vouched, err := p.vouches(ctx, req)
	if err != nil {
		return unread(err)
	}

	if ours {
		if !vouched {
			// THE RECORD IS THE PROOF: this operation's, passed and not gated, so
			// it applied and its row went with the ledger's loss. Collapsed unless
			// this call's own append, as on the ledger arm.
			return Result{
				Outcome:   OutcomeApplied,
				Position:  at,
				OpID:      req.OpID,
				Version:   at.Packed(),
				Collapsed: landed != landedOwn,
				Waited:    true,
			}, nil
		}
		// Applied past, not gated, ledger complete since the mint, and no row:
		// a contract violation, not a race. Re-deciding would publish it twice,
		// so it is reported.
		return Result{}, fmt.Errorf("statelog: %s applied the record at %s but "+
			"wrote no %s row for operation %q. That ledger is what an ambiguous "+
			"publish is resolved by, and a record applied without one cannot be "+
			"answered for", p.domain.Name(), at, p.domain.OpsTable(), req.OpID)
	}

	if !vouched {
		// THE LEDGER CANNOT ANSWER HERE: reading its silence as "somebody else
		// won" would re-decide against rows this very write moved. NO POSITION
		// ([Result.Position]): `at` is merely the newest record, and a caller
		// would act on another writer's record as its own.
		return Result{
			Outcome: OutcomeUnknown, OpID: req.OpID, Unvouched: true, Waited: true,
		}, nil
	}

	// Somebody else won. Re-decide.
	return Result{}, nil
}

// voided is whether the record at `at` applied nowhere, and the gate that
// answers: the framework's own first (a reanchor's rules), as the applier
// asks them, then the domain's, about writer.
//
// The framework's are asked only of this operation's own record: another
// operation's record found newest on the subject is not this write's
// landing, and the domain's gates are asked about this node for it.
func (p *Publisher) voided(ctx context.Context, req Request, at Position,
	ours bool, writer string, gen uint32, body []byte) (Reason, bool, error) {

	if ours {
		if reason, gated := p.voids.Voided(gen, at.Seq); gated {
			return reason, true, nil
		}
	}
	reason, gated, err := p.gates.GatedAt(ctx, req.Subject, writer, req.OpID, at, body)
	if err != nil {
		return "", false, fmt.Errorf("statelog: read the apply gates: %w", err)
	}
	return reason, gated, nil
}

// recordAt is the envelope and the body of the record at, read off the log,
// and false for a sequence the log no longer holds.
//
// Verified ([Deps.Records]): a record this fleet's keys do not verify is an
// error, never a writer to name.
func (p *Publisher) recordAt(ctx context.Context, at Position) (Envelope, []byte, bool, error) {
	_, body, _, found, err := p.records.At(ctx, at.Seq)
	if err != nil || !found {
		return Envelope{}, nil, found, err
	}
	env, err := p.domain.Envelope(body)
	if err != nil {
		return Envelope{}, nil, false, fmt.Errorf("statelog: decode the envelope at %s: %w", at, err)
	}
	return env, body, true, nil
}

// vouches reports whether this node's operation ledger can answer for req's
// operation: whether "its row is absent" means "it has not applied here".
//
// A sweep deletes rows older than [OpsRetention] and moves its cutoff, only
// forward, in the same transaction ([Rows.LostBefore]; a kind swept sooner by
// internal/maintenance's OpsHorizon.Kinds keeps its own, the later counting).
// An operation minted at or after the bound cannot have lost its row; one
// before it, or whose id carries no instant ([OpMintedAt]), only by its row.
// An adoption loses nothing: ledger and watermark travel with the rows. Asked
// BEFORE PUBLISHING: a retry's fresh snapshot already holds the first
// application, and its clean append would publish a second copy. Cheap: one
// row by key, plus the ledger row only below the bound.
func (p *Publisher) vouches(ctx context.Context, req Request) (bool, error) {
	if p.domain.OpsTable() == "" {
		return true, nil
	}
	bound, lost, err := p.rows.LostBefore(ctx, req.Subject.Kind)
	if err != nil {
		return false, fmt.Errorf("statelog: read how far back the operation "+
			"ledger may have lost rows: %w", err)
	}
	if !lost || !mintedAt(req.OpID).Before(bound) {
		return true, nil
	}
	// MINTED BEFORE THE BOUND: only the row can speak, read AFTER the bound
	// so a sweep or install between the two reads is seen.
	_, held, err := p.rows.Op(ctx, req.OpID)
	if err != nil {
		return false, fmt.Errorf("statelog: read the operation ledger: %w", err)
	}
	return held, nil
}

// fence0 is every refusal this node can make from what it already knows,
// cheapest first: the stream identity is a field read, the eviction a local
// row.
func (p *Publisher) fence0(ctx context.Context, req Request) error {
	if err := p.checkIdentity(req); err != nil {
		return err
	}
	if err := p.checkTruncated(req); err != nil {
		return err
	}
	return p.checkEvicted(ctx)
}

// checkTruncated refuses a write while a peer's rows hold records the log lost
// ([Identity]), except a node gate, which may be exactly that peer's eviction.
// After the identity, whose refusal is the truer one for this node.
func (p *Publisher) checkTruncated(req Request) error {
	err := p.identity.Truncated()
	if err == nil || req.NodeGate {
		return nil
	}
	return &Unavailable{
		Reason: ReasonLogTruncated,
		Detail: fmt.Sprintf("%v, so %s was not published", err, req.Subject),
		OpID:   req.OpID,
		Cause:  err,
	}
}

// clearForZero is the fence on an expectation of zero ([Fence.ClearForZero])
// at the decision's cursor ([Snap.Checkpoint]).
//
// A log rebuilt since the last heartbeat passes fence 0, and at zero that is
// a lost update; the fence's own read hands the applier the stream's instant,
// so the attempt's fence 0 refuses in the same round (package doc, "And the
// premise under all of it: ONE STREAM"). Other patterns keep the heartbeat's
// window: an additive or sequenced record claims no history, an arbitrated one
// lands only on a matching sequence, and closing it costs a broker round trip
// per append. Over a refusal the identity answers first, since a rebuild also
// puts the checkpoint past the end and is the real finding; over a clear the
// attempt's fence 0 is next, so nothing is asked here.
func (p *Publisher) clearForZero(ctx context.Context, req Request, cursor Position) error {
	fenced := p.fence.ClearForZero(ctx, cursor)
	if fenced == nil {
		return nil
	}
	if err := p.checkIdentity(req); err != nil {
		return err
	}
	return fenced
}

// checkIdentity is fence 0's identity half: a node whose log is not the one
// its positions are on refuses every write ([Identity]).
//
// Except a node gate over a passed generation ([ErrGenerationPassed]): it is
// decided from nothing in this node's rows, arbitrated on the gated node's own
// subject, and applied at its own position in the new generation, and it is
// the one gesture that releases a fleet stranded by a decommissioned peer. A
// RECREATED stream is never excused, and that finding outranks the passed one.
func (p *Publisher) checkIdentity(req Request) error {
	err := p.identity.StreamIdentity()
	if err == nil || (req.NodeGate && errors.Is(err, ErrGenerationPassed)) {
		return nil
	}
	return &Unavailable{
		Reason: ReasonWrongStream,
		Detail: fmt.Sprintf("%v. A write here would be arbitrated against a "+
			"history it was not decided from, so %s was not published", err, req.Subject),
		OpID:  req.OpID,
		Cause: err,
	}
}

// checkEvicted is fence 0's eviction half, answered in the one sentence the
// zero fence answers the same finding with ([evictionRefusal]).
func (p *Publisher) checkEvicted(ctx context.Context) error {
	evicted, err := p.fence.Evicted(ctx)
	return evictionRefusal(ctx, evicted, err)
}

// waitSession waits for this node's applier to reach the caller's own
// high-water mark, before any snapshot opens.
func (p *Publisher) waitSession(ctx context.Context, req Request) error {
	if req.Session.IsZero() {
		return nil
	}
	if before, err := p.waiter.Reached().Before(req.Session); err != nil {
		return err
	} else if !before {
		return nil
	}
	started := time.Now()
	waitCtx, cancel, bound := WaitWithin(ctx, p.budget(ctx))
	defer cancel()
	err := p.waiter.WaitReached(waitCtx, req.Session)
	p.observeMillis(metrics.StatelogWriteSessionWait, started, metrics.Attrs{
		"domain": p.domain.Name(),
	})
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// NEVER A SILENT STALE SNAPSHOT: deciding below the caller's own write is
		// the every-round conflict this wait exists to avoid.
		return &Unavailable{
			Reason: ReasonBehind,
			Detail: fmt.Sprintf("this node has not applied this caller's own "+
				"write at %s within %s", req.Session, bound),
			Position: req.Session,
			OpID:     req.OpID,
		}
	}
	return nil
}

// waitBehind waits for this node to reach a position a peer already wrote,
// under the write path's own budget.
//
// The same budget and refusal as the session wait, the same situation from
// the other side: `behind` with the position, which a caller can retry
// against, instead of a conflict after every round.
func (p *Publisher) waitBehind(ctx context.Context, req Request, at Position) error {
	waitCtx, cancel, bound := WaitWithin(ctx, p.budget(ctx))
	defer cancel()
	if err := p.waiter.WaitReached(waitCtx, at); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return &Unavailable{
			Reason: ReasonBehind,
			Detail: fmt.Sprintf("%s is at %s on the log and this node has not "+
				"applied it within %s, so a write against it would be decided "+
				"from a state below it", req.Subject, at, bound),
			Position: at,
			OpID:     req.OpID,
		}
	}
	return nil
}

// subjectOf renders a subject as the wire string: the domain's prefix plus
// the object's kind and id. The anchor's key comes from the table helper on
// both sides, which is the pairing that has to agree.
func (p *Publisher) subjectOf(s Subject) string {
	return wireSubject(p.prefix, s)
}

// observe records the write path's own instruments.
//
// A REFUSAL IS NOT AN OUTCOME, and is counted separately by reason: each has
// its own remedy, and one dimension would mix four meanings in one rate.
func (p *Publisher) observe(started time.Time, req Request, res Result, err error) {
	domain := p.domain.Name()
	if err != nil {
		reason := "error"
		var refusal *Unavailable
		switch {
		case errors.As(err, &refusal):
			reason = string(refusal.Reason)
		case errors.Is(err, ErrConflict):
			reason = "conflict"
			// AND BY KIND: one contended object is a design question, a contended
			// kind a hot subject.
			p.count(metrics.StatelogPublishConflicts, metrics.Attrs{
				"domain": domain, "subject_kind": req.Subject.Kind,
			})
		case errors.Is(err, ErrExists):
			reason = "exists"
		}
		p.observeMillis(metrics.StatelogPublishDuration, started,
			metrics.Attrs{"domain": domain, "outcome": "refused"})
		p.count(metrics.StatelogPublishRefusals,
			metrics.Attrs{"domain": domain, "reason": reason})
		return
	}
	attrs := metrics.Attrs{"domain": domain, "outcome": string(res.Outcome)}
	p.observeMillis(metrics.StatelogPublishDuration, started, attrs)
	p.count(metrics.StatelogPublishOutcomes, attrs)
	if res.Rounds > 0 && p.metrics != nil {
		p.metrics.ObserveValue(metrics.StatelogPublishRounds, float64(res.Rounds),
			metrics.Attrs{"domain": domain})
	}
}

func (p *Publisher) observeMillis(name string, started time.Time, attrs metrics.Attrs) {
	if p.metrics == nil {
		return
	}
	p.metrics.Observe(name, time.Since(started), attrs)
}

func (p *Publisher) count(name string, attrs metrics.Attrs) {
	if p.metrics == nil {
		return
	}
	p.metrics.Add(name, 1, attrs)
}
