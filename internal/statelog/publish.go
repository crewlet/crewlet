package statelog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// casRounds bounds a read-decide-write retry.
//
// SIXTEEN, the shape [internal/sandbox]'s run store and [internal/work]'s
// writes both use and for the same reason: each round is a lost race against
// another writer, and sixteen consecutive losses on one object means
// something is rewriting it in a loop rather than that this caller is
// unlucky. Reporting "it kept changing" then is the honest answer; retrying
// for ever would hold a request open against a write storm.
const casRounds = 16

// Pattern is how a write arbitrates, and there are three because there are
// three different questions the broker can be asked.
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
)

// Waiter is this node's own applier, as the publisher needs it.
type Waiter interface {
	// Committed is this node's committed position on the stream.
	Committed() Position

	// WaitCommitted blocks until this node's applier has committed
	// through p. It is what turns a lost race into a retry that can win:
	// re-deciding without it re-reads the anchor the applier has not yet
	// advanced, sixteen times, to a refusal.
	WaitCommitted(ctx context.Context, p Position) error

	// WaitApplied blocks until this node holds the derived consequences
	// of everything up to p for the objects in s.
	WaitApplied(ctx context.Context, s ScopeSet, p Position) error
}

// Fence refuses a write this node must not make.
//
// TWO METHODS BECAUSE THERE ARE TWO PRICES. Evicted runs on EVERY append and
// is answered from what this node already knows, for free. ClearForZero runs
// only where being wrong costs a silent lost update, and pays a coordination
// round trip for a fresh answer.
type Fence interface {
	// Evicted reports this node's own eviction.
	//
	// It must consult every source that is fresh in a failure mode the
	// others are not — in particular this node's own applied eviction
	// rows, which are the only source still fresh when the coordination
	// path is wedged, and a wedged coordination path is a precondition of
	// an eviction being permitted at all.
	Evicted(ctx context.Context) (bool, error)

	// ClearForZero verifies, freshly, that publishing at an expectation
	// of ZERO is safe from this node: that it is not evicted, and that
	// the published trim floor is at or below this node's own cursor.
	//
	// A READ THAT ANSWERS UNKNOWN MUST REFUSE, which is a deliberate
	// departure from the fail-open rule a delivery claim uses. Failing
	// open there is a duplicate delivery, which is recoverable; failing
	// open here is a lost update, which is not.
	ClearForZero(ctx context.Context, cursor Position) error
}

// Gates answers whether a durable record produced rows on NO node.
//
// Both gates — a permanent deletion marker and a node's eviction — drop a
// record the broker accepted, so no node ever writes its ops row. Without
// this seam the resolution rule reads "absent, so somebody else won", the
// writer re-decides, republishes, is dropped again, and burns its whole round
// budget to a conflict a model reads as a colleague editing the same object.
type Gates interface {
	// GatedAt reports the gate that dropped a record at p on this
	// subject, published by writer.
	GatedAt(ctx context.Context, subj Subject, writer string, p Position) (Reason, bool, error)

	// AdoptedAt is when this node's adoption of a donated snapshot
	// completed, reporting false when it never adopted one.
	//
	// The ops table is this node's own and is scrubbed from every
	// donated snapshot, so an op id minted before this instant cannot be
	// answered for here at all — and reading its absence as "somebody
	// else won" would re-decide against a row that moved because of this
	// very write.
	AdoptedAt(ctx context.Context) (time.Time, bool, error)
}

// Request is one caller-visible write.
type Request struct {
	// Subject is the object this write arbitrates over.
	Subject Subject

	// Scope is every object this write's rows touch, which is what the
	// deferral probe is run over and what the resolution waits on.
	Scope ScopeSet

	// OpID is the operation id, minted ONCE per caller-visible operation
	// and stable across every round and every retry.
	//
	// A REGENERATED ID DEFEATS THE LEDGER for exactly the lost-ack case
	// the ledger exists for. A rejected attempt cannot dedupe against
	// itself — a rejection means nothing landed — so the only case a
	// stable id collapses is a retry after a copy that landed, which is
	// the case it is for.
	OpID string

	// MintedAt is when the op id was minted, which is what the
	// pre-adoption arm compares against.
	MintedAt time.Time

	// Session is this caller's own high-water mark on the stream. The
	// publisher waits for its own applier to reach it BEFORE it opens a
	// snapshot, so a caller that created a task and immediately edits it
	// decides from a state that contains its own write.
	Session Position

	// Pattern is how this write arbitrates.
	Pattern Pattern

	// Decide runs inside the snapshot's transaction and returns the
	// record to publish. It may run more than once — each round takes a
	// fresh snapshot — and must decide only from rows it reads there.
	Decide func(*sql.Tx) (Decision, error)
}

// ErrExists reports a first-writer-wins create for an object that is already
// there, established from the guarding row rather than from the broker.
var ErrExists = errors.New("statelog: the object already exists")

// Publisher is a domain's write authority.
//
// # The rule, stated once, because it is the whole safety argument
//
//	Take ONE snapshot of your own rows. Decide and form the expectation
//	inside it. Publish. Let the broker arbitrate. Never guess.
//
// What it buys is that a behind node cannot corrupt anything — it can only
// fail to write. Every clause is load-bearing:
//
//   - ONE snapshot, because an expectation read separately from the decision
//     it travels with can be NEW while the decision is OLD, and the broker
//     accepts exactly that pair.
//   - The expectation from the ARBITRATION ANCHOR and never from the row: an
//     expectation is a claim about the log and a version is a claim about the
//     domain's accepted state, and a gate is by definition the rule that makes
//     the two differ.
//   - Let the BROKER arbitrate, because it is the only party that sees every
//     writer.
//   - Never guess: an unknown outcome is resolved, not retried blindly and not
//     reported as a loss.
type Publisher struct {
	domain  Domain
	stream  string
	prefix  string
	log     Appender
	rows    Rows
	fence   Fence
	gates   Gates
	waiter  Waiter
	metrics *metrics.Recorder
	logger  *slog.Logger

	// nodeID is this node's own identity, stamped on every record so the
	// eviction gate has something to compare against.
	nodeID string

	// generation is the estate's current generation, read fresh on every
	// publish rather than captured, because a reanchor moves it under a
	// running process.
	generation func() uint32

	// resolveBudget bounds the wait a resolution spends before it
	// answers pending.
	//
	// FIVE SECONDS, which is the applier's own stall grace divided by
	// twelve: long enough that an ordinary batch commit and its linger
	// are covered many times over, short enough that a caller holding a
	// request open learns "durable, unresolved here" rather than waiting
	// out a node that has stopped applying. A node that cannot answer
	// inside it has a health fault the caller cannot fix by waiting.
	resolveBudget time.Duration
}

// DefaultResolveBudget is how long a write waits for its own applier before
// it answers pending. See [Publisher.resolveBudget].
const DefaultResolveBudget = 5 * time.Second

// Deps is everything a publisher needs that it does not own.
type Deps struct {
	Domain     Domain
	Log        Appender
	Rows       Rows
	Fence      Fence
	Gates      Gates
	Waiter     Waiter
	Metrics    *metrics.Recorder
	Logger     *slog.Logger
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
	case d.Rows == nil:
		return nil, fmt.Errorf("statelog: publisher has no rows")
	case d.Fence == nil:
		return nil, fmt.Errorf("statelog: publisher has no fence — every " +
			"append checks eviction, and a publisher that cannot is one " +
			"that collects acknowledgements for records every node drops")
	case d.Gates == nil:
		return nil, fmt.Errorf("statelog: publisher has no gates")
	case d.Waiter == nil:
		return nil, fmt.Errorf("statelog: publisher has no waiter")
	case d.Generation == nil:
		return nil, fmt.Errorf("statelog: publisher has no generation source")
	case d.NodeID == "":
		return nil, fmt.Errorf("statelog: publisher has no node id — the " +
			"eviction gate compares against it, so a record with none is a " +
			"record no gate can drop")
	}
	spec := d.Domain.Stream()
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	logger := d.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	budget := d.ResolveBudget
	if budget <= 0 {
		budget = DefaultResolveBudget
	}
	return &Publisher{
		domain:        d.Domain,
		stream:        spec.Name,
		prefix:        spec.SubjectPrefix,
		log:           d.Log,
		rows:          d.Rows,
		fence:         d.Fence,
		gates:         d.Gates,
		waiter:        d.Waiter,
		metrics:       d.Metrics,
		logger:        logger,
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
		return Result{}, fmt.Errorf("statelog: write has no op id — it is what " +
			"an ambiguous publish is resolved by, so a write without one has no " +
			"third answer at all")
	}
	if req.Decide == nil {
		return Result{}, fmt.Errorf("statelog: write has no decide function")
	}
	if req.Scope.Empty() {
		return Result{}, fmt.Errorf("statelog: write on %s declares no scope — an "+
			"empty scope claims the record makes nothing stale, which is the one "+
			"claim a record no build may be able to read cannot make", req.Subject)
	}

	// FENCE 0, BEFORE ANYTHING ELSE AND ON EVERY APPEND. It costs one
	// indexed local read of a table that is empty in every healthy
	// company, and what it removes is the shape where a node that KNOWS
	// it has been removed from the fleet still collects acknowledgements
	// for records every applier will drop.
	if err := p.checkEvicted(ctx); err != nil {
		return Result{}, err
	}

	// THE SESSION WAIT IS OUTSIDE AND BEFORE THE SNAPSHOT. A snapshot
	// taken below the caller's own previous write forms an expectation
	// that write has already made stale — the broker refuses it, and an
	// immediate re-snapshot reads the same anchor sixteen times to a
	// conflict. It costs nothing when the caller is caught up, which is
	// the common case.
	if err := p.waitSession(ctx, req); err != nil {
		return Result{}, err
	}

	gen := p.generation()
	for round := 1; round <= casRounds; round++ {
		snap, err := p.rows.Snapshot(ctx, req.Subject, req.Scope, req.Decide)
		if err != nil {
			return Result{Rounds: round}, err
		}
		if refusal := p.refuseFromSnapshot(req, snap); refusal != nil {
			return Result{Rounds: round}, refusal
		}
		if snap.Decision.Empty() {
			// NOTHING TO PUBLISH IS A SUCCESS, not an error: an update
			// that changes no field is a no-op the caller should be
			// told succeeded. The position is zero because no record
			// exists, and the version is the one the decision read.
			return Result{
				Outcome: OutcomeApplied,
				OpID:    req.OpID,
				Version: snap.Decision.Version,
				Rounds:  round,
			}, nil
		}

		expect, behind, err := p.expectation(ctx, req, snap, gen)
		if err != nil {
			return Result{Rounds: round}, err
		}
		if behind != nil {
			// A PEER WROTE IN THIS GENERATION AND THIS NODE IS BEHIND.
			// Never expectation zero: the subject is not empty, it is
			// ahead of this node.
			//
			// UNDER THE SAME BUDGET AS THE SESSION WAIT, and refusing
			// `behind` on expiry. An unbounded wait here is a caller
			// blocked for as long as this node stays behind — which is
			// unbounded by construction, because the very state that
			// produces it is an applier that has not caught up. The
			// caller's own context is not a budget either: a request
			// with no deadline waits for ever, and one with a deadline
			// gets a cancellation where it needs the reason.
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

		// THE BROKER REFUSED THE EXPECTATION. Discriminate it: a
		// trimmed anchor is retried ONCE at zero within this round, and
		// anything else re-decides from a fresh snapshot.
		zero, err := p.afterRejection(ctx, req, expect)
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

// disposition is what one append attempt leaves the round loop to do, and
// there are three because there are three different next steps — collapsing
// any pair sends a rejection through the discriminator's LastSeq call for a
// record that was never refused, or sends an ambiguous publish there for a
// rejection that never happened.
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
	seq, _, err := p.append(ctx, req, snap, expect)
	switch f, detail := classify(err); f {
	case faultNone:
		at := Position{Stream: p.stream, Generation: gen, Seq: seq}
		if err := at.Valid(); err != nil {
			return Result{Rounds: round}, dispDone, err
		}
		res, err := p.Resolve(ctx, req, at, true)
		res.Rounds = round
		return res, dispDone, err

	case faultFull:
		return Result{Rounds: round}, dispDone, &Unavailable{
			Reason: ReasonLogFull,
			Detail: fmt.Sprintf("the broker refused to store the record: %s — a "+
				"full log refuses appends rather than dropping records, so raise "+
				"the stream's byte ceiling or unblock the trim", detail),
			OpID: req.OpID,
		}

	case faultUnknown:
		res, err := p.classifyAmbiguous(ctx, req, snap, detail)
		res.Rounds = round
		if err != nil || res.Outcome != "" {
			return res, dispDone, err
		}
		// Nothing landed, or somebody else's record did. Either way
		// there is no rejection to discriminate: take a fresh snapshot.
		return Result{Rounds: round}, dispRetake, nil

	default:
		p.count("crewlet.statelog.publish.rejections", metrics.Attrs{
			"domain": p.domain.Name(), "subject_kind": req.Subject.Kind,
		})
		return Result{Rounds: round}, dispRejected, nil
	}
}

// refuseFromSnapshot is every refusal a committed snapshot settles on its
// own, before any broker call.
func (p *Publisher) refuseFromSnapshot(req Request, snap Snap) error {
	// STEP 0 — the deferred-SCOPE probe, and it is a correctness
	// precondition rather than a courtesy. A record this node cannot
	// decode makes the rows it touched permanently stale here; if its
	// position has also been trimmed, the retry-at-zero branch below
	// would fire and silently overwrite it.
	//
	// SCOPE-KEYED AND NEVER SUBJECT-KEYED. A record deferred under one
	// object rewrote a NEIGHBOUR's row too, and there is nothing on the
	// neighbour's own subject to say so — so a subject-keyed probe passes,
	// the neighbour's next writer takes retry-at-zero, and the deferred
	// mutation is overwritten with nothing anywhere reporting it.
	if snap.Deferred {
		return &Unavailable{
			Reason: ReasonDeferred,
			Detail: fmt.Sprintf("this node holds a record at version %d it cannot "+
				"decode, at %s, whose scope covers %s — its rows are stale here "+
				"and another node can serve this write",
				snap.Deferral.Version, snap.Deferral.Position, req.Subject),
			Position: snap.Deferral.Position,
			OpID:     req.OpID,
		}
	}
	if req.Pattern != PatternCreate {
		return nil
	}
	// A DELETION MARKER IS PERMANENT WHERE A GUARDING ROW IS NOT. A purge
	// deletes the row and leaves the marker, so on a subject a producer
	// can derive, the guard is gone while the object stays deleted for
	// ever — and without this the writer is told "it already exists"
	// above the trim floor and "it succeeded" below it, for a record
	// every applier drops.
	if snap.Deleted {
		return &Unavailable{
			Reason: ReasonDeleted,
			Detail: fmt.Sprintf("%s was deleted and stays deleted", req.Subject),
			OpID:   req.OpID,
		}
	}
	// BELOW THE TRIM FLOOR THE BROKER CLAIM IS NOT THE GUARD — THE ROW IS.
	// Once a claim's record is trimmed its subject holds nothing, an
	// expectation of zero succeeds again, and the broker enforces no
	// uniqueness at all. What still holds is the floor theorem: every
	// serving node has applied everything below the floor, so the row is
	// present and authoritative.
	if snap.Guard {
		return fmt.Errorf("%w: %s", ErrExists, req.Subject)
	}
	return nil
}

// expectation forms what the broker will arbitrate against, and reports a
// position to wait for instead when this node is simply behind.
//
// THE ANCHOR AND NOTHING ELSE. An expectation is a claim about the LOG — what
// is the last message on this subject — and the log is the framework's;
// version is a claim about the DOMAIN's accepted state. They are equal only
// while every accepted record produces rows, and a gate is precisely the rule
// that makes them differ: an evicted node's accepted-then-gated append leaves
// the broker's last sequence above every node's version, and a writer forming
// its expectation from the row re-reads the number that produced its own
// rejection until it runs out of rounds.
func (p *Publisher) expectation(ctx context.Context, req Request, snap Snap, gen uint32) (*uint64, *Position, error) {
	if req.Pattern == PatternAdditive {
		return nil, nil, nil
	}
	// AN ANCHOR AT THE CURRENT GENERATION, and SEQ IS WHAT MAKES IT ONE.
	// A row carrying this generation and a sequence of zero says this
	// node has consumed nothing on this subject — which is a PROBE, not
	// a licence to publish at zero: only the broker can tell a subject
	// that has never been written from one whose only record this node
	// has not applied.
	if snap.Anchor.Seq > 0 && snap.Anchor.Generation == gen {
		seq := snap.Anchor.Seq
		return &seq, nil, nil
	}

	// NO ANCHOR AT THE CURRENT GENERATION means this node has consumed
	// nothing on this subject in this generation. That covers three cases
	// at once — a subject never written, a row last written before a
	// reanchor, and a subject whose only record this node has not yet
	// applied — and only the broker can tell them apart.
	subject := p.subjectOf(req.Subject)
	seq, found, err := p.log.LastSeq(ctx, subject)
	if err != nil {
		return nil, nil, fmt.Errorf("statelog: read the last message on %s: %w", subject, err)
	}
	if !found {
		// The subject genuinely holds nothing. Publishing at zero is
		// correct — and fenced, because being wrong here is a lost
		// update rather than a refused write.
		if err := p.fence.ClearForZero(ctx, p.waiter.Committed()); err != nil {
			return nil, nil, err
		}
		zero := uint64(0)
		return &zero, nil, nil
	}
	at := Position{Stream: p.stream, Generation: gen, Seq: seq}
	return nil, &at, nil
}

// afterRejection discriminates a rejection, which is the one place a trimmed
// anchor and a lost race are told apart.
//
// It returns a non-nil expectation ONLY for the trimmed-anchor retry.
func (p *Publisher) afterRejection(ctx context.Context, req Request, expect *uint64) (*uint64, error) {
	subject := p.subjectOf(req.Subject)

	// ON EVERY REJECTION, and the cheap pre-filter that used to stand in
	// front of this call is deliberately absent: it skipped the broker
	// call on the lost-race path, and the lost-race path needs the call
	// anyway — it needs the sequence to know what to wait for before it
	// re-decides.
	seq, found, err := p.log.LastSeq(ctx, subject)
	if err != nil {
		return nil, fmt.Errorf("statelog: discriminate a rejection on %s: %w", subject, err)
	}

	switch {
	case !found && expect != nil && *expect > 0:
		// THE ANCHOR WAS TRIMMED. The server evaluates an expectation
		// by loading the subject's last message and rescues only the
		// zero case, so an expectation above zero on an empty subject
		// is refusable FOR EVER: the writer re-reads, gets the same
		// number, retries, and livelocks. A quiet object would become
		// permanently unwritable.
		//
		// Retrying at zero is safe here and ONLY here, under the floor
		// theorem in this package's doc — all three of whose clauses
		// are enforced, none of them defensively. It is attempted once
		// per ROUND rather than once per write: each round decides
		// again from a fresh snapshot, so a second attempt is a second
		// decision rather than the same one repeated, and a subject
		// somebody is genuinely racing on runs out of rounds and is
		// told so.
		if err := p.fence.ClearForZero(ctx, p.waiter.Committed()); err != nil {
			return nil, err
		}
		zero := uint64(0)
		return &zero, nil

	case !found:
		// An empty subject under an expectation of zero: a genuine lost
		// create race whose winner's record has since been trimmed.
		// Retake the snapshot; the guarding row is the answer.
		return nil, nil

	case expect != nil && seq < *expect:
		// IMPOSSIBLE ON A HEALTHY STREAM. The anchor came from a record
		// this node applied, so the broker holding an EARLIER last
		// message means a store and a stream were restored out of step
		// with each other — and retrying against it would either
		// livelock or overwrite, so it refuses and names the skew.
		return nil, &Unavailable{
			Reason: ReasonSkew,
			Detail: fmt.Sprintf("%s holds sequence %d but this node expected %d, "+
				"which it read from a record it applied — a store and a stream "+
				"restored out of step", subject, seq, *expect),
			Position: Position{Stream: p.stream, Seq: seq},
			OpID:     req.OpID,
		}

	default:
		// A LOST RACE. Drive this node's applier to the winner's
		// position before re-deciding: a retake with no wait re-reads
		// the anchor the applier has not yet advanced, and does it
		// sixteen times.
		//
		// UNDER THE WRITE PATH'S OWN BUDGET, exactly as the behind
		// branch above is: the winner's record may be one this node
		// never applies, and the caller's context is not a bound —
		// with no deadline it waits for ever, and with one it gets a
		// cancellation where it needs a position to retry against.
		at := Position{Stream: p.stream, Generation: p.generation(), Seq: seq}
		if err := p.waitBehind(ctx, req, at); err != nil {
			return nil, err
		}
		return nil, nil
	}
}

// classifyAmbiguous is the ordered classification of a publish whose outcome
// nobody knows, and it answers in ONE of four ways.
//
// A zero Outcome with a nil error means nothing landed and the caller should
// retake its snapshot.
func (p *Publisher) classifyAmbiguous(ctx context.Context, req Request, snap Snap, detail string) (Result, error) {
	subject := p.subjectOf(req.Subject)
	seq, found, err := p.log.LastSeq(ctx, subject)
	switch {
	case err != nil:
		// NOTHING ANSWERS AT ALL — the fourth arm, and the honest third
		// value. The op id travels with it because retrying under the
		// SAME id is the only safe retry: a fresh one would defeat the
		// ledger that exists for exactly this case.
		p.logger.Warn("statelog_publish_unknown",
			"domain", p.domain.Name(), "subject", subject,
			"op_id", req.OpID, "publish_error", detail, "probe_error", err.Error())
		return Result{Outcome: OutcomeUnknown, OpID: req.OpID}, nil

	case !found, seq <= snap.Anchor.Seq:
		// At or below the anchor this write decided against, so the
		// subject holds nothing this write put there. Nothing landed:
		// retake the snapshot and re-decide.
		return Result{}, nil

	default:
		// SOMETHING LANDED ABOVE THE ANCHOR. Whether it is THIS write's
		// record is what the resolution answers, from this node's own
		// applied rows rather than from a guess — and its gated arm is
		// why "landed" is not the same as "applied".
		p.logger.Debug("statelog_publish_ambiguous",
			"domain", p.domain.Name(), "subject", subject,
			"op_id", req.OpID, "at", seq, "publish_error", detail)
		at := Position{Stream: p.stream, Generation: p.generation(), Seq: seq}
		return p.Resolve(ctx, req, at, false)
	}
}

// Resolve answers what a durable record at p actually produced, and it is
// called by the ambiguous path AND by every ordinary write the moment its own
// acknowledgement names a position — INCLUDING the branch where the wait
// succeeds.
//
// # Why the ordinary branch needs it too, which is where the lie was
//
// A live node E is current at 100; a peer commits E's eviction at 101; E
// publishes at 102 and is acknowledged; E's own applier applies 101, applies
// 102, DROPS 102 under its own eviction gate, and passes 102 well inside the
// budget. A design answering from the wait alone reports the record as
// APPLIED — the strictly stronger and equally false answer, on the COMMON
// branch rather than on a timeout — while the fact that killed the record sat
// in a durable local table in the same database the answer came from.
//
// One resolution function, called by both paths. Two copies of it is how the
// ordinary path came to have none.
//
// It needs no coordination read, no clock and no cadence, because the applier
// is contiguous: passing p forces applying everything below it, an eviction
// gate drops a record only when the eviction commit is strictly below it, and
// a deletion marker that could gate p is on the record's own subject and
// therefore below it, or the broker would have refused the append.
// mine says whether the caller KNOWS the record at at is its own — true when
// the broker acknowledged it, false when the ambiguous path merely found
// something above the anchor. The two differ in one arm and it matters: an
// absent ledger row means "somebody else won" only when the record might have
// been somebody else's.
func (p *Publisher) Resolve(ctx context.Context, req Request, at Position, mine bool) (Result, error) {
	waitCtx, cancel := context.WithTimeout(ctx, p.resolveBudget)
	defer cancel()

	if err := p.waiter.WaitApplied(waitCtx, req.Scope, at); err != nil {
		if ctx.Err() != nil {
			return Result{}, ctx.Err()
		}
		// DURABLE AT p, UNRESOLVED HERE. Never "applied", and never a
		// failure: every other node will apply it, and the caller has a
		// position to resolve with rather than a retry to make.
		return Result{Outcome: OutcomePending, Position: at, OpID: req.OpID}, nil
	}

	// THE LEDGER IS ONLY ASKED WHERE THERE IS ONE. A ledgerless domain's
	// table is permanently empty, so a read of it would answer "absent"
	// for every record including its own.
	if p.domain.OpsTable() != "" {
		where, ok, err := p.rows.Op(ctx, req.OpID)
		if err != nil {
			return Result{}, fmt.Errorf("statelog: read the operation ledger: %w", err)
		}
		if ok {
			return Result{
				Outcome:  OutcomeApplied,
				Position: where,
				OpID:     req.OpID,
				Version:  where.Packed(),
			}, nil
		}
	}

	// ABSENT, so ask the two questions that make absence mean something
	// other than "somebody else won" — in the order that makes each
	// answer conclusive.
	if reason, gated, err := p.gates.GatedAt(ctx, req.Subject, p.nodeID, at); err != nil {
		return Result{}, fmt.Errorf("statelog: read the apply gates: %w", err)
	} else if gated {
		// THE RECORD APPLIED NOWHERE AND NEVER WILL. A refusal rather
		// than an outcome, and never a re-decide: republishing produces
		// another durable record nothing applies.
		p.logger.Warn("statelog_write_gated",
			"domain", p.domain.Name(), "subject", req.Subject.String(),
			"gate", string(reason), "position", at.String(), "op_id", req.OpID)
		p.count("crewlet.statelog.records_gated", metrics.Attrs{
			"gate": string(reason), "subject_kind": req.Subject.Kind,
		})
		return Result{}, &Unavailable{
			Reason:   reason,
			Detail:   fmt.Sprintf("the record at %s was durable and applied nowhere", at),
			Position: at,
			OpID:     req.OpID,
		}
	}

	// A DOMAIN WITH NO LEDGER answers from the wait and the gates alone.
	// The contract that licenses it is narrow — a total apply under a
	// monotone version guard, whose writer never reports a committed
	// position to a caller — and without this arm every one of its writes
	// would read its own empty table as "somebody else won" and republish
	// for ever.
	if p.domain.OpsTable() == "" {
		return Result{
			Outcome:  OutcomeApplied,
			Position: at,
			OpID:     req.OpID,
			Version:  at.Packed(),
		}, nil
	}

	if mine {
		// THE BROKER ACKNOWLEDGED THIS RECORD, this node applied past
		// it, and no gate dropped it — so the applier applied it and
		// wrote no ledger row. That is a contract violation rather than
		// a race, and re-deciding would republish a record that already
		// landed, so it is reported instead of guessed at.
		return Result{}, fmt.Errorf("statelog: %s applied the record at %s but "+
			"wrote no %s row for operation %q — that ledger is what an ambiguous "+
			"publish is resolved by, and a record applied without one cannot be "+
			"answered for", p.domain.Name(), at, p.domain.OpsTable(), req.OpID)
	}

	adopted, ever, err := p.gates.AdoptedAt(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("statelog: read the adoption record: %w", err)
	}
	if ever && !req.MintedAt.IsZero() && req.MintedAt.Before(adopted) {
		// THE LEDGER CANNOT ANSWER FOR THIS OPERATION ON THIS NODE. It
		// is scrubbed from every donated snapshot, so a node that
		// adopted one arrives with an empty table — and reading that
		// absence as "somebody else won" would re-decide against a row
		// that moved because of this very write.
		return Result{Outcome: OutcomeUnknown, Position: at, OpID: req.OpID}, nil
	}

	// Somebody else won. Re-decide.
	return Result{}, nil
}

// append publishes one record, stamping the framework's own fields onto the
// envelope the domain decided.
func (p *Publisher) append(ctx context.Context, req Request, snap Snap, expect *uint64) (uint64, bool, error) {
	// FENCE 0 AGAIN, because a round is not free of it: a write that has
	// spent fifteen rounds losing races has been running for as long as
	// those races took, and the eviction it must not publish under may
	// have landed inside that window.
	if err := p.checkEvicted(ctx); err != nil {
		return 0, false, err
	}
	return p.log.Append(ctx, p.subjectOf(req.Subject), req.OpID, expect, snap.Decision.Payload)
}

// checkEvicted is fence 0.
func (p *Publisher) checkEvicted(ctx context.Context) error {
	evicted, err := p.fence.Evicted(ctx)
	if err != nil {
		// THE THIRD VALUE BLOCKS. An eviction that cannot be read is
		// not an eviction that did not happen, and publishing under it
		// produces durable records every node drops.
		return &Unavailable{
			Reason: ReasonEvicted,
			Detail: fmt.Sprintf("this node's own eviction state could not be read: %v", err),
		}
	}
	if evicted {
		return &Unavailable{
			Reason: ReasonEvicted,
			Detail: "this node has been removed from the fleet; nothing it " +
				"publishes will be applied anywhere. An operator readmits it",
		}
	}
	return nil
}

// waitSession waits for this node's applier to reach the caller's own
// high-water mark, before any snapshot opens.
func (p *Publisher) waitSession(ctx context.Context, req Request) error {
	if req.Session.IsZero() {
		return nil
	}
	if before, err := p.waiter.Committed().Before(req.Session); err != nil {
		return err
	} else if !before {
		return nil
	}
	started := time.Now()
	waitCtx, cancel := context.WithTimeout(ctx, p.resolveBudget)
	defer cancel()
	err := p.waiter.WaitCommitted(waitCtx, req.Session)
	p.observeMillis("crewlet.statelog.write.session_wait", started, metrics.Attrs{
		"domain": p.domain.Name(),
	})
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// NEVER A SILENT STALE SNAPSHOT. A caller that wrote and
		// immediately wrote again must see its own row, and deciding
		// from a state below it is exactly the sixteen-round conflict
		// this wait exists to avoid.
		return &Unavailable{
			Reason: ReasonBehind,
			Detail: fmt.Sprintf("this node has not applied this caller's own "+
				"write at %s within %s", req.Session, p.resolveBudget),
			Position: req.Session,
			OpID:     req.OpID,
		}
	}
	return nil
}

// waitBehind waits for this node to reach a position a peer already wrote,
// under the write path's own budget.
//
// THE SAME BUDGET AND THE SAME REFUSAL AS THE SESSION WAIT, because they are
// the same situation seen from two sides: a snapshot below a position the
// broker has already accepted. Both answer `behind` with the position they
// were waiting for, which is what turns "a colleague is editing this" — the
// conflict a caller would otherwise be told after sixteen rounds — into a
// number the caller can retry against.
func (p *Publisher) waitBehind(ctx context.Context, req Request, at Position) error {
	waitCtx, cancel := context.WithTimeout(ctx, p.resolveBudget)
	defer cancel()
	if err := p.waiter.WaitCommitted(waitCtx, at); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return &Unavailable{
			Reason: ReasonBehind,
			Detail: fmt.Sprintf("%s is at %s on the log and this node has not "+
				"applied it within %s, so a write against it would be decided "+
				"from a state below it", req.Subject, at, p.resolveBudget),
			Position: at,
			OpID:     req.OpID,
		}
	}
	return nil
}

// subjectOf renders a subject as the wire string, which is the domain's own
// prefix plus the object's kind and id.
//
// The publisher keeps its own because it holds the prefix and not the table
// helper; both are the same one line, and the ANCHOR's key comes from the
// table helper on both sides — which is the pairing that has to agree.
func (p *Publisher) subjectOf(s Subject) string {
	return p.prefix + "." + s.String()
}

// observe records the write path's own instruments.
//
// A REFUSAL IS NOT AN OUTCOME and is counted separately. The three outcomes
// say what happened to a record; a refusal says no record happened, and each
// reason has its own remedy — so folding them into one dimension would give
// an operator a rate with four different meanings in it.
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
			// AND BY KIND, because the remedy differs: the refusal
			// counter says a write lost every round and not what it
			// was about, and one contended object is a design
			// question where a contended KIND is a hot subject.
			p.count("crewlet.statelog.publish.conflicts", metrics.Attrs{
				"domain": domain, "subject_kind": req.Subject.Kind,
			})
		case errors.Is(err, ErrExists):
			reason = "exists"
		}
		p.observeMillis("crewlet.statelog.publish.duration", started,
			metrics.Attrs{"domain": domain, "outcome": "refused"})
		p.count("crewlet.statelog.publish.refusals",
			metrics.Attrs{"domain": domain, "reason": reason})
		return
	}
	attrs := metrics.Attrs{"domain": domain, "outcome": string(res.Outcome)}
	p.observeMillis("crewlet.statelog.publish.duration", started, attrs)
	p.count("crewlet.statelog.publish.outcomes", attrs)
	if res.Rounds > 0 && p.metrics != nil {
		p.metrics.ObserveValue("crewlet.statelog.publish.rounds", float64(res.Rounds),
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
