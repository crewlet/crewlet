package statelog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// casRounds bounds a read-decide-write retry.
//
// SIXTEEN, the shape [internal/sandbox]'s run store uses and for the same
// reason: each round is a lost race against another writer, and sixteen
// consecutive losses on one object means
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

// Identity is whether this node's positions are sequences on the stream the
// broker serves under the domain's name — [Runner.StreamIdentity], as the
// publisher needs it.
//
// # Why a write asks this before anything else, and asks it EVERY time
//
// The write authority's safety argument is that the broker arbitrates, and
// arbitration means something only while the expectation and the log are in
// ONE sequence space. A stream deleted and rebuilt under the same name keeps
// the generation and counts from 1 again, so every number this node holds is
// still a plausible number there — and the broker, which cannot know which
// history a number was read from, arbitrates it as one about ITSELF:
//
//   - An ordinary expectation is a claim that the subject's last record is the
//     one at A. On the rebuilt log A is a different record or none, and where
//     the subject's last sequence happens to be A the broker ACCEPTS a
//     decision taken from rows that never saw that record.
//   - An expectation of zero is cleared against this node's checkpoint and
//     the floor, both from the old history, so the floor theorem — every term
//     of which is a sequence on one stream — proves nothing about the new
//     one, and a subject whose anchor sat above the rebuilt log's content
//     is overwritten from nothing.
//   - Even an additive write, which forms no expectation, is RESOLVED against
//     this node's applier: a position on the rebuilt log compared with a
//     checkpoint from the old one, so a record at a low sequence reads as
//     already applied and the answer — applied, or a ledger contract
//     violation — is false either way.
//
// And whatever lands is not a local mistake: the recovery that follows a
// rebuild follows the new log from its head, so every node applies it as
// though it continued a history it was never arbitrated in. So every pattern
// refuses, not only the retry at zero the floor theorem names.
//
// # And a checkpoint past the log's end is the same failure, without the instant
//
// A broker restored from a copy older than this node's rows keeps its stream,
// creation instant and all, so the rebuild check passes — and the log ENDS
// below this node's checkpoint. That is the premise failing just as surely:
// the sequences between the log's end and the checkpoint are ones this node
// applied records at and the log will issue to different ones. And it has a
// consequence of its own that decides the question for every pattern at once:
// whatever this node appends lands at the log's next sequence, which is at or
// below its own checkpoint — a record its applier has already passed and will
// never apply. The resolution then has nothing true to say: an arbitrated or
// created record is absent from a ledger the applier never wrote, which reads
// as a contract violation, and a ledgerless one is reported applied because
// the wait is already past it — while the record is on the log for every other
// node. An expectation above the end is refused by the broker into the retry at
// zero, the lost update the floor theorem names, and one at or below it is
// accepted on a subject whose history on this log the node's rows may not
// contain. So [Runner.StreamIdentity] answers for both findings, and every
// pattern refuses on either.
//
// The answer is what this node has ESTABLISHED, and a reading that could not
// be taken establishes nothing in either direction — the next one decides. On
// the zero branch that costs nothing, because it reads the log within the call
// and its fence refuses on a log it cannot read; on every other branch it
// widens the window [Publisher.clearForZero] states by one missed reading,
// inside which harm still needs the rebuilt log's history on the subject to
// end at exactly the sequence this node's row names.
//
// # And what the fleet knows that this node's own log cannot show
//
// Truncated answers for a PEER: a node whose rows hold records the log lost
// ([ErrLogTruncated]). This node's rows are the log's history, so its reads are
// served and StreamIdentity says nothing — but on a broker restored from an
// older copy, a write from here is one the restored reanchor of that peer would
// apply nowhere, acknowledged to a caller who then loses it. So every write
// refuses on it too, but for the one gesture that is how the operator ends it:
// the peer's eviction ([Request.NodeGate]).
type Identity interface {
	StreamIdentity() error
	Truncated() error
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
	// of ZERO is safe from this node: that it is not evicted, that its
	// applier's checkpoint is not past the log's end, and that
	// [Replayable](cursor, F) holds for F the HIGHER of the published trim
	// floor and the log's own first surviving sequence — the F the floor
	// theorem in this package's doc is stated over. Either bound alone
	// clears a node the other refuses. [ZeroFence] is that check, written
	// once; a domain that can reach an expectation of zero supplies its
	// four reads to it.
	//
	// EVERY REFUSAL IS AN [*Unavailable] NAMING ITS REASON — evicted,
	// floor_unknown, wrong_stream, below_floor, or behind for a node
	// replaying up to the floor — and never [ErrConflict], which a caller
	// reads as a colleague editing the same object.
	//
	// cursor is the write's own [Snap.Checkpoint] — the position the rows
	// its decision was made from are at — and the published floor is read
	// at cursor's GENERATION. Neither may be the applier's live position:
	// that only moves forward, and a check against it clears a node whose
	// decision predates a record it has since applied and the trim has
	// since removed. And a floor read at another generation is a number
	// in another sequence space, which says nothing about this cursor.
	// The END is the exception, and is compared with the applier's live
	// checkpoint rather than with cursor: it asks where a record this node
	// appends will land relative to where its applier stands, which is a
	// property of the node and not of the decision.
	//
	// A READ THAT ANSWERS UNKNOWN MUST REFUSE, which is a deliberate
	// departure from the fail-open rule a delivery claim uses. Failing
	// open there is a duplicate delivery, which is recoverable; failing
	// open here is a lost update, which is not.
	//
	// THE READ OF THE LOG IS ALSO THE IDENTITY CHECK, and the publisher
	// relies on it: the answer that carries the log's ends carries the
	// stream's creation instant beside it, so an implementation that reads
	// the live log hands both to this node's applier
	// ([Runner.ObserveStream], [Runner.ObserveEnd]) — and whatever the
	// publisher does next asks [Identity] after that read: over a refusal
	// from this, at once, and otherwise in the fence 0 it runs before the
	// append. That is what makes the zero branch's identity as fresh as its
	// floor, within the call, rather than as fresh as the last heartbeat.
	ClearForZero(ctx context.Context, cursor Position) error
}

// Gates answers whether a durable record produced rows on NO node.
//
// Both gates — a permanent deletion marker and a node's eviction — drop a
// record the broker accepted, so no node ever writes its ops row. Without
// this seam the resolution rule reads "absent, so somebody else won", the
// writer re-decides, republishes, is dropped again, and burns its whole round
// budget to a conflict a model reads as a colleague editing the same object.
//
// # The rule every reader keeps
//
// Each domain spells its own two queries, and every one of them answers by
// ONE rule — certified for all of them by the same family,
// statelogtest.RunGates, because two readers that agree only by looking alike
// have already drifted apart once:
//
//   - THE DELETION GATE IS REPORTED FIRST. A marker is a fact about the
//     OBJECT, holding for every writer on every node for ever; an eviction is
//     a fact about one WRITER, and a readmission ends it. So a record both
//     gates hold is reported `deleted`: that is the answer that stays true,
//     and a caller told `evicted` who waited out a readmission would only
//     meet the marker on its next write. (The applier checks the eviction
//     first; its order decides only which gate a drop is COUNTED under.)
//   - THE MARKER HAS NO POSITION, exactly as the applier's does not: it gates
//     every record on its object that did not apply, whenever that record is
//     processed — one deferred past the purge and reprocessed after it
//     included. So once an object is purged, a record the eviction dropped
//     below the purge is reported `deleted` too. What is reported is the gate
//     that holds the record NOW, which is the one a caller can act on; which
//     gate fired first at p is the applier's `statelog_record_gated` line.
//   - THE PURGE'S OWN RECORD IS EXEMPT FROM ITS OWN MARKER, by operation id,
//     and from that gate alone: it still falls through to the eviction
//     window, because the exemption says nothing about its writer.
//   - THE EVICTION WINDOW IS HALF-OPEN AT BOTH ENDS, as the applier's is: a
//     record is gated when the eviction's position is below it and no
//     readmission is at or below it.
type Gates interface {
	// GatedAt reports whether a record at p on this subject, published by
	// writer under opID, applies nowhere, and the gate that answers for it
	// by the rule above.
	//
	// THE OPERATION ID IS NOT DECORATION. A gate that destroys an object
	// is written BY a record, and that record must not be gated by the
	// marker it wrote — otherwise a purge whose acknowledgement was lost
	// resolves as "applied nowhere" and its caller is told the
	// destruction it asked for did not happen, when it did.
	GatedAt(ctx context.Context, subj Subject, writer, opID string, p Position) (Reason, bool, error)
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
	//
	// AND IT CARRIES THE INSTANT IT WAS MINTED AT, in its own bytes —
	// see [NewOpID], [DeriveOpID] and [OpMintedAt]. There is deliberately
	// no field beside it for that instant: a retry reuses the id, so the
	// instant is the id's, and a value the caller stamps is the CALL's —
	// later than the mint on every retry, which is exactly the case the
	// ledger cannot vouch for.
	OpID string

	// Session is this caller's own high-water mark on the stream. The
	// publisher waits for its own applier to reach it BEFORE it opens a
	// snapshot, so a caller that created a task and immediately edits it
	// decides from a state that contains its own write.
	//
	// THE CALLER SETS IT PER GESTURE, and the framework deliberately does
	// not remember it instead. A publisher is a domain's singleton, shared
	// by every sequence on this node, so a mark it accumulated would make
	// one gesture's step wait for an unrelated gesture's record — and a
	// mark scoped to a long-lived writer would carry a stale position into
	// every later write that surface made. Only the gesture knows which of
	// its own appends the next one has to see, which is why a domain's
	// writer hands the position from step to step (see
	// `tracker.Writer.After`) rather than this package deriving it.
	//
	// The zero position waits for nothing, which is every write a surface
	// makes on its own.
	Session Position

	// Pattern is how this write arbitrates.
	Pattern Pattern

	// NodeGate marks an eviction or a readmission of a node: a statement
	// about a MACHINE, arbitrated on that machine's own subject, whose
	// content nothing in this node's rows decided. It is the one write a
	// node the fleet re-anchored past may still make, because the gesture
	// that releases a fleet stranded by a decommissioned peer is exactly
	// that peer's eviction — see [Publisher.checkIdentity].
	NodeGate bool

	// Standing judges a retry this node's ledger already answers
	// ([Snap.Held]), for a write whose landed record a LATER write can
	// undo: nil when the operation's record is still the one in force, a
	// refusal ([ReasonSuperseded]) when it is not. It runs in the snapshot's
	// own transaction, handed where the record landed, and only after the
	// row was found to be this write's ([Publisher.heldHere]).
	//
	// Nil for every write whose record nothing undoes, where a ledger row
	// is the whole answer. A node gate sets it ([GateStanding]): an
	// eviction a readmission has since inverted landed, and answering its
	// retry `applied` reports a node stopped that every row counts.
	Standing func(tx *sql.Tx, held Position) error

	// Decide runs inside the snapshot's transaction and returns the
	// record to publish. It may run more than once — each round takes a
	// fresh snapshot — and must decide only from rows it reads there.
	//
	// It is handed the round's [Stamp], and the record it returns MUST
	// carry both halves in its envelope: the publisher refuses one that
	// does not, before anything is appended.
	Decide func(tx *sql.Tx, stamp Stamp) (Decision, error)
}

// Stamp is what every record a publisher appends carries about WHERE IT CAME
// FROM, handed to the domain's decision rather than left for the domain to
// find.
//
// # Why the framework hands it over, and then checks it
//
// Both halves are read by somebody who cannot ask the writer. The eviction gate
// compares a record's Writer against the node an eviction names, on every
// applier in the fleet and on a node that may not be able to decode the payload
// at all; the generation is what makes a record from before a reanchor
// comparable and safely stale rather than plausible. Each domain's envelope had
// a field for both and NO write path in the tree filled either — the publisher
// held its node id and stamped nothing — so every record reached every applier
// with an empty writer, the eviction gate's "is this writer evicted" never
// matched a row, and clause (iii) of the floor theorem, the one that holds when
// the other two are defeated, dropped nothing. Nothing looked wrong, because a
// gate that fires never and a gate with nothing to fire on read the same.
//
// So the values are the framework's and not the domain's to find: a domain's
// decision receives them, and [Publisher] decodes the envelope of what came
// back — through [Domain.Envelope], the same reader every applier uses — and
// refuses a record whose envelope does not carry them. A domain that forgets is
// a write that fails on its first attempt, naming the field, rather than a
// fleet whose evictions quietly do nothing.
type Stamp struct {
	// Writer is this node's id — the publisher's own, which is what the
	// eviction gate compares against.
	Writer string

	// Gen is the generation of the snapshot the decision was taken from:
	// [Snap.Checkpoint]'s, read in the decision's own transaction, so a
	// record is stamped with the generation of the rows it was decided
	// from rather than with a value read beside them.
	Gen uint32
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
	domain   Domain
	stream   string
	prefix   string
	log      Appender
	rows     Rows
	fence    Fence
	gates    Gates
	waiter   Waiter
	identity Identity
	metrics  *metrics.Recorder
	logger   *slog.Logger

	// nodeID is this node's own identity, handed to every decision as
	// [Stamp.Writer] and checked on every record before it is appended, so
	// the eviction gate has something to compare against.
	nodeID string

	// generation is this domain's current generation, read fresh on every
	// publish rather than captured, because a reanchor of this domain's
	// stream moves it under a running process.
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
	Domain Domain
	Log    Appender
	Rows   Rows
	Fence  Fence
	Gates  Gates
	Waiter Waiter

	// Identity is the stream identity of the positions Waiter holds —
	// in the engine the same runner, which is the one place both the
	// boot's comparison and every live reading land.
	Identity Identity

	Metrics *metrics.Recorder

	// Logger is where this writes. Nil is the package's own component
	// logger, never silence: see loggerOr for what silence cost.
	Logger *slog.Logger

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
	case d.Identity == nil:
		return nil, fmt.Errorf("statelog: publisher has no stream identity — " +
			"every expectation it forms is a sequence on the stream its rows " +
			"came from, and one that cannot ask whether that is still the live " +
			"stream is one whose writes a rebuilt log arbitrates as its own")
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
	logger := loggerOr(d.Logger)
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
		identity:      d.Identity,
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

	// FENCE 0, BEFORE ANYTHING ELSE AND ON EVERY APPEND. Its identity
	// half is a field read of what this node has already established, and
	// its eviction half one indexed local read of a table that is empty in
	// every healthy company. What they remove is the two shapes where a
	// node KNOWS its writes are meaningless and publishes anyway: a node
	// whose log was rebuilt under it, whose expectations the broker
	// arbitrates against a history they were not read from, and a node
	// removed from the fleet, which collects acknowledgements for records
	// every applier will drop.
	if err := p.fence0(ctx, req); err != nil {
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
		snap, err := p.snapshot(ctx, req)
		if err != nil {
			return Result{Rounds: round}, err
		}
		if snap.HeldOK {
			// THIS OPERATION ALREADY APPLIED HERE, so this call is its
			// retry and is answered with the first copy's position
			// rather than decided again — see [Snap.Held]. Before any
			// refusal, because a refusal says nothing landed and this
			// operation did.
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
		// BEFORE ANY BROKER CALL: a record that does not say who wrote it
		// is one no eviction gate can drop, and nothing after the append
		// could take it back.
		if err = p.stamped(req, snap); err != nil {
			return Result{Rounds: round}, err
		}
		// A DECISION THIS NODE'S LEDGER CANNOT VOUCH FOR IS NEVER
		// PUBLISHED. See [Publisher.vouches]; it is read AFTER the
		// snapshot, so an adoption that replaced the rows this decision
		// read is one it sees, and the snapshot already said the ledger
		// holds no row for it.
		vouched, err := p.vouches(ctx, req)
		if err != nil {
			return Result{Rounds: round}, err
		}
		if !vouched {
			p.logger.WarnContext(ctx, "statelog_write_unvouched",
				"domain", p.domain.Name(), "subject", req.Subject.String(),
				"op_id", req.OpID, "minted_at", mintedAt(req.OpID),
				"detail", "this operation was minted before this node's "+
					"operation ledger may have lost rows — to the ledger's "+
					"retention sweep, or to a snapshot adopted from a donor "+
					"that scrubbed its ledger — and it holds no row saying "+
					"whether the operation already applied, so it is "+
					"answered unknown rather than decided a second time; "+
					"a node whose ledger lost nothing that far back can "+
					"answer it")
			return Result{Outcome: OutcomeUnknown, OpID: req.OpID, Rounds: round}, nil
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

		// THE BROKER REFUSED THE EXPECTATION. Discriminate it: a
		// trimmed anchor is retried ONCE at zero within this round, and
		// anything else re-decides from a fresh snapshot.
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

// snapshot takes one round's snapshot, handing the domain's decision the
// [Stamp] it must carry: this node's id, and the generation of the checkpoint
// the same transaction read.
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
//
// ONE JUDGEMENT FOR BOTH PLACES THE LEDGER ANSWERS — a retry the snapshot
// finds already applied ([Snap.Held]) and an append whose answer is resolved
// from the ledger ([Publisher.Resolve]) — because they are the same question
// asked before and after the broker.
//
// # A row on another subject answers for some other write
//
// An operation id is the caller's — a seat carries one forward and asks again,
// a route takes `?op_id=` — and the ledger keeps an id's FIRST row. So the same
// id sent with a write to a second object finds the first object's row: the
// snapshot answered `applied` at the first object's position, and inside the
// broker's duplicate window the second append collapsed onto the first record
// and the resolution answered the same. Either way the caller was told a write
// to b was done, at a position on a, with nothing written to b at all. It is
// refused `op_reused`, naming where the id landed, rather than confirmed.
func (p *Publisher) heldHere(req Request, held OpEntry) error {
	if held.Subject == p.subjectOf(req.Subject) {
		return nil
	}
	return &Unavailable{
		Reason: ReasonOpReused,
		Detail: fmt.Sprintf("operation %q landed on %s at %s, and this write is "+
			"to %s — an operation id names one write, so a different one needs "+
			"a fresh id", req.OpID, held.Subject, held.Position,
			p.subjectOf(req.Subject)),
		Position: held.Position,
		OpID:     req.OpID,
	}
}

// stampAt is the stamp a decision taken at checkpoint is handed. One function
// for both the handing and the check in [Publisher.stamped], so the two can
// never name different values.
func (p *Publisher) stampAt(checkpoint Position) Stamp {
	return Stamp{Writer: p.nodeID, Gen: checkpoint.Generation}
}

// stamped refuses a decision whose record does not carry the stamp it was
// decided under — see [Stamp] for what an unstamped record cost.
//
// THE PAYLOAD'S OWN ENVELOPE, decoded by the domain's own reader: that is the
// only envelope any applier ever sees, so it is the only one worth checking.
// And it is the REQUEST's op id the record must carry too, because the two
// travel separately — the request's is the message id and the one Resolve asks
// the ledger for, the record's is the one the applier writes there — and a
// record that named another would be answered, by the ledger that exists to
// answer it, as an operation that never landed.
func (p *Publisher) stamped(req Request, snap Snap) error {
	want := p.stampAt(snap.Checkpoint)
	env, err := p.domain.Envelope(snap.Decision.Payload)
	if err != nil {
		return fmt.Errorf("statelog: the %s record decided for %s does not "+
			"decode through its own domain's envelope reader: %w — every "+
			"applier reads it that way first", p.domain.Name(), req.Subject, err)
	}
	switch {
	case env.Writer != want.Writer:
		return fmt.Errorf("statelog: the %s record decided for %s names writer "+
			"%q, and this publisher is %q — the eviction gate compares a record's "+
			"writer against every eviction on the log, so a record carrying "+
			"another node's id, or none, is one no eviction of this node can "+
			"drop; the domain's decision must put [Stamp.Writer] in its envelope",
			p.domain.Name(), req.Subject, env.Writer, want.Writer)
	case env.Gen != want.Gen:
		return fmt.Errorf("statelog: the %s record decided for %s is stamped "+
			"generation %d, and the snapshot it was decided from is at %d — the "+
			"domain's decision must put [Stamp.Gen] in its envelope",
			p.domain.Name(), req.Subject, env.Gen, want.Gen)
	case env.OpID != req.OpID:
		return fmt.Errorf("statelog: the %s record decided for %s carries "+
			"operation %q and the write is %q — the ledger an ambiguous publish "+
			"is resolved by is keyed on the record's, so this write could never "+
			"be answered for", p.domain.Name(), req.Subject, env.OpID, req.OpID)
	}
	return nil
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
	// FENCE 0 AGAIN, because a round is not free of it: a write that has
	// spent fifteen rounds losing races has been running for as long as
	// those races took, and the eviction it must not publish under — or
	// the heartbeat's reading of a rebuilt log — may have landed inside
	// that window.
	//
	// AND ITS REFUSAL ENDS THE WRITE HERE, before [classify] ever sees it.
	// It is this node's own decision, taken before anything reached the
	// broker, so there is nothing ambiguous about it — but to the
	// classifier every error that is not the broker's own is no answer at
	// all. Handed over with the append's, it sent the write to ask the log
	// what landed, find nothing, retake its snapshot and be refused again,
	// until the round budget reported a conflict: a colleague editing the
	// object, told to a caller this node had refused for a reason of its
	// own.
	if err := p.fence0(ctx, req); err != nil {
		return Result{Rounds: round}, dispDone, err
	}
	seq, duplicate, appendErr := p.log.Append(ctx, p.subjectOf(req.Subject), req.OpID, expect, snap.Decision.Payload)
	switch f, detail := classify(appendErr); f {
	case faultNone:
		at := Position{Stream: p.stream, Generation: gen, Seq: seq}
		if err := at.Valid(); err != nil {
			return Result{Rounds: round}, dispDone, err
		}
		res, err := p.Resolve(ctx, req, at, true)
		res.Rounds = round
		// A DUPLICATE ACKNOWLEDGEMENT IS THE BROKER COLLAPSING THIS
		// APPEND onto an earlier copy of the operation still inside its
		// window: `at` is that copy's, and this call's decision was never
		// stored — which a caller computing its answer inside the
		// decision has to be told. See [Result.Collapsed].
		res.Collapsed = duplicate && res.Outcome != ""
		return res, dispDone, err

	case faultFull:
		return Result{Rounds: round}, dispDone, &Unavailable{
			Reason: ReasonLogFull,
			// AND WHERE THE BLOCKING TERM IS NAMED. "Unblock the trim"
			// is a remedy an operator cannot act on without knowing
			// which of the six terms came lowest, and that is a
			// property of the last tick rather than of this append —
			// so the message points at the surface that holds it
			// instead of taking a coordination round trip on a
			// refusal path.
			Detail: fmt.Sprintf("the broker refused to store the record: %s — a "+
				"full log refuses appends rather than dropping records, so raise "+
				"the stream's byte ceiling or unblock the trim (`crewlet "+
				"retention status` names the term holding it)", detail),
			OpID: req.OpID,
		}

	case faultTooLarge:
		// NOTHING WAS STORED AND NOTHING EVER WILL BE: the same record
		// is refused the same way on every round, so there is no round
		// to run again.
		return Result{Rounds: round}, dispDone, fmt.Errorf("statelog: the %s "+
			"record for %s is %d bytes, more than the broker carries (%s), so "+
			"nothing was stored: %w", p.domain.Name(), req.Subject,
			len(snap.Decision.Payload), detail, queue.ErrTooLarge)

	case faultRefused:
		return Result{Rounds: round}, dispDone, fmt.Errorf("statelog: the broker "+
			"refused the %s record for %s and stored nothing: %w",
			p.domain.Name(), req.Subject, appendErr)

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
		p.count(metrics.StatelogPublishRejections, metrics.Attrs{
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
		// update rather than a refused write. Fenced at the SNAPSHOT's
		// checkpoint, since that snapshot's decision is what gets
		// published; see [Snap.Checkpoint].
		if err := p.clearForZero(ctx, req, snap.Checkpoint); err != nil {
			return nil, nil, err
		}
		zero := uint64(0)
		return &zero, nil, nil
	}
	// A RECORD THE SNAPSHOT'S CHECKPOINT ALREADY COVERS is one these rows
	// hold, so it is the expectation rather than a position to wait for.
	//
	// On an ordinary log this cannot be reached: every record at or below
	// the checkpoint was consumed in this generation, and consuming an
	// arbitrated one writes its anchor, so the branch above answered. What
	// reaches it is a checkpoint placed where the log's records were never
	// consumed in this generation — a RESTORED log's reanchor, which puts it
	// at the log's end because the rows already hold every record the copy
	// kept ([ReanchorRestored]). There the subject's last record is below
	// the checkpoint and its anchor is in the generation before; waiting for
	// the applier to reach it returns at once, the snapshot reads the same
	// anchor, and every write to every object the copy kept ran out of
	// rounds as a conflict nobody was causing.
	//
	// AT THE SNAPSHOT'S checkpoint, not the applier's live one, for the
	// reason the zero fence takes it: the claim is about the rows the
	// decision read.
	if snap.Checkpoint.Generation == gen && seq <= snap.Checkpoint.Seq {
		return &seq, nil, nil
	}
	at := Position{Stream: p.stream, Generation: gen, Seq: seq}
	return nil, &at, nil
}

// afterRejection discriminates a rejection, which is the one place a trimmed
// anchor and a lost race are told apart.
//
// It returns a non-nil expectation ONLY for the trimmed-anchor retry, and
// that retry publishes snap's decision again — which is why snap is what the
// fence is asked about.
func (p *Publisher) afterRejection(ctx context.Context, req Request, snap Snap, expect *uint64) (*uint64, error) {
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
		//
		// AN EMPTY SUBJECT IS ALSO WHAT A REBUILT LOG LOOKS LIKE, and
		// nothing in this branch's own arithmetic can tell the two
		// apart: the anchor, the checkpoint and the floor are all
		// sequences on the stream this node's rows came from. That is
		// why the identity is asked after the clearance's own read of
		// the log, before the retry is appended — see
		// [Publisher.clearForZero].
		//
		// AT THE SNAPSHOT'S CHECKPOINT, not the applier's live one: the
		// retry publishes the decision this round's snapshot made, and
		// the theorem's conclusion — the trimmed record is already in
		// the rows — is about the rows that decision read. By now the
		// applier may have moved past a record that snapshot never saw.
		if err := p.clearForZero(ctx, req, snap.Checkpoint); err != nil {
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
		p.logger.WarnContext(ctx, "statelog_publish_unknown",
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
		p.logger.DebugContext(ctx, "statelog_publish_ambiguous",
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
// therefore below it, or the broker would have refused the append. So a gate
// that DID drop p is in the rows the wait already covered. The converse is
// not claimed: a marker can also land ABOVE p, after an eviction dropped the
// record, and the reader then reports `deleted` by the rule [Gates] states.
//
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
		entry, ok, err := p.rows.Op(ctx, req.OpID)
		if err != nil {
			return Result{}, fmt.Errorf("statelog: read the operation ledger: %w", err)
		}
		if ok {
			if err := p.heldHere(req, entry); err != nil {
				return Result{}, err
			}
			return Result{
				Outcome:  OutcomeApplied,
				Position: entry.Position,
				OpID:     req.OpID,
				Version:  entry.Position.Packed(),
			}, nil
		}
	}

	// ABSENT, so ask the two questions that make absence mean something
	// other than "somebody else won" — in the order that makes each
	// answer conclusive.
	if reason, gated, err := p.gates.GatedAt(ctx, req.Subject, p.nodeID, req.OpID, at); err != nil {
		return Result{}, fmt.Errorf("statelog: read the apply gates: %w", err)
	} else if gated {
		// THE RECORD APPLIED NOWHERE AND NEVER WILL. A refusal rather
		// than an outcome, and never a re-decide: republishing produces
		// another durable record nothing applies.
		p.logger.WarnContext(ctx, "statelog_write_gated",
			"domain", p.domain.Name(), "subject", req.Subject.String(),
			"gate", string(reason), "position", at.String(), "op_id", req.OpID)
		p.count(metrics.StatelogRecordsGated, metrics.Attrs{
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

	// THE LEDGER'S SILENCE MEANS SOMETHING ONLY WHERE IT CAN VOUCH, and
	// that is asked before either reading of it below — the mine arm's
	// included, because a sweep, or an adoption from a donor that
	// scrubbed its ledger, is exactly what loses the row of a record this
	// node was acknowledged for.
	vouched, err := p.vouches(ctx, req)
	if err != nil {
		return Result{}, err
	}

	if mine {
		if !vouched {
			// THE ACKNOWLEDGEMENT IS THE PROOF, and the ledger's loss
			// is why the row is missing. The broker named this
			// operation's own record at `at`, this node's rows are past
			// it and no gate dropped it — so it applied, and its row
			// went with the rows the watermark says this ledger lost.
			return Result{
				Outcome:  OutcomeApplied,
				Position: at,
				OpID:     req.OpID,
				Version:  at.Packed(),
			}, nil
		}
		// THE BROKER ACKNOWLEDGED THIS RECORD, this node applied past
		// it, no gate dropped it, and its ledger has lost nothing since
		// the operation was minted — so the applier applied it and wrote
		// no ledger row. That is a contract violation rather than a race,
		// and re-deciding would republish a record that already landed,
		// so it is reported instead of guessed at.
		return Result{}, fmt.Errorf("statelog: %s applied the record at %s but "+
			"wrote no %s row for operation %q — that ledger is what an ambiguous "+
			"publish is resolved by, and a record applied without one cannot be "+
			"answered for", p.domain.Name(), at, p.domain.OpsTable(), req.OpID)
	}

	if !vouched {
		// THE LEDGER CANNOT ANSWER FOR THIS OPERATION ON THIS NODE: it
		// may have lost the row, and reading that absence as "somebody
		// else won" would re-decide against a row that moved because of
		// this very write.
		//
		// NO POSITION, which is the whole content of unknown (see
		// [Result.Position]): the record at `at` is merely the newest on
		// the subject, and naming it would tell the caller "your write
		// landed here" — the one thing nobody here can say.
		return Result{Outcome: OutcomeUnknown, OpID: req.OpID}, nil
	}

	// Somebody else won. Re-decide.
	return Result{}, nil
}

// vouches reports whether this node's operation ledger can answer for req's
// operation — whether "its row is absent" means "it has not applied here".
//
// # Why the ledger can lose a row, and what that costs a retry
//
// ONE WATERMARK SAYS HOW FAR BACK IT MAY HAVE ([Rows.LostBefore]), and it
// travels in the same file as the ledger. Two things move it, each in the
// transaction or the file that loses the rows, and only ever forward.
//
// The SWEEP deletes every row applied more than [OpsRetention] ago, and
// records its cutoff with the delete. Every copy of an operation is applied at
// or after the instant it was minted, so one minted at or after that cutoff
// cannot have lost its row; one minted before it may have, and absence says
// nothing. Without this, a retry a month on — an operator repeating an
// `unknown`, a seat carrying an operation id across a long pause — was decided
// again and published a second copy.
//
// An ADOPTION FROM A DONOR THAT SCRUBBED ITS LEDGER — a build from before the
// ledger travelled — installs a file whose ledger holds none of the donor's
// rows, so the adopter writes the join's own start into that file before it
// installs it (see [Adopter.Join]): an operation minted after the start was
// published after every artefact the join could install, and its every copy
// is one this node's own applier writes a row for.
//
// AN ADOPTION FROM ANY OTHER DONOR LOSES NOTHING. Its ledger travels with its
// rows ([Domain.OpsTable]), so the adopter holds a row for every operation the
// donor applied and inherits the donor's own watermark besides — and a turn
// woken by a trigger from before the join, which derives its operation ids
// from that trigger's instant, has its FIRST attempts decided and published
// like anyone's. With the ledger scrubbed, those first attempts were answered
// `unknown` and never published: the recovering node refusing its own
// backlog.
//
// That is why this runs BEFORE A DECISION IS PUBLISHED and not only in the
// resolution of an ambiguous one. A retry — a turn re-run after a crash, a
// caller repeating an `unknown` under the same operation id — takes a fresh
// snapshot whose rows already hold the first application, decides again on
// top of them, and publishes a second copy of the operation the broker has no
// reason to refuse: its expectation is current, and the duplicate window
// that might have collapsed it is two minutes wide. The resolution never runs
// on that path at all, because the append is acknowledged cleanly.
//
// # The instant is the operation id's own
//
// Read off the id with [OpMintedAt], never off a field the caller fills, for
// the reason [Request.OpID] gives. An id that carries none is read as minted
// at the zero instant, so a ledger that has lost anything vouches for it only
// by holding its row — see opid.go.
//
// # Where it is cheap
//
// The watermark is one row by key, and the ledger row is read only for an
// operation minted before it — which on a ledger that has lost nothing is no
// operation at all. A domain with no ledger has nothing to vouch with and
// nothing that reads it, so it is answered without any read.
func (p *Publisher) vouches(ctx context.Context, req Request) (bool, error) {
	if p.domain.OpsTable() == "" {
		return true, nil
	}
	bound, lost, err := p.rows.LostBefore(ctx)
	if err != nil {
		return false, fmt.Errorf("statelog: read how far back the operation "+
			"ledger may have lost rows: %w", err)
	}
	if !lost || !mintedAt(req.OpID).Before(bound) {
		return true, nil
	}
	// MINTED BEFORE THE BOUND, so only the row itself can speak — and it
	// is read AFTER the bound, so a sweep or an install landing between
	// the two reads is one whose effect on the table this read sees.
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
// — see [Identity] — except a node gate, whose content is the operator's and
// whose purpose may be exactly to evict that peer.
//
// AFTER THE IDENTITY, because a node whose own rows are not the log's history
// has the truer refusal: its writes would be wrong whatever the fleet chose.
func (p *Publisher) checkTruncated(req Request) error {
	err := p.identity.Truncated()
	if err == nil || req.NodeGate {
		return nil
	}
	return &Unavailable{
		Reason: ReasonLogTruncated,
		Detail: fmt.Sprintf("%v — so %s was not published", err, req.Subject),
		OpID:   req.OpID,
		Cause:  err,
	}
}

// clearForZero is the fence on an expectation of zero, with the identity asked
// over any refusal the fence makes.
//
// cursor is the checkpoint of the snapshot the write decided from — see
// [Snap.Checkpoint] — and never the applier's live position, which only moves
// forward and so clears a decision against records it never read. The one
// comparison the fence makes against the live position instead is its own,
// with the log's end, because that one is about the NODE rather than the
// decision: whether what this node appends lands where its applier will apply
// it depends on where the applier stands now — see [ZeroFence].
//
// # How a rebuild the fence's own read finds is refused within the call
//
// Fence 0 answers from the last reading of the stream's instant and its end,
// and the reading that sets them on a running node is the position heartbeat
// — so a log rebuilt since the last beat passes the fence 0 at the top of the
// write, and on this branch that window is a lost update rather than a refused
// write: the rebuilt log holds nothing on the subject, the expectation of zero
// is accepted, and the floor the fence clears against is a number from the old
// history. What closes it is the fence's own read of the log, the one read on
// the write path that carries the stream's creation instant: it hands the
// instant to this node's applier (see [Fence.ClearForZero]), and the fence 0
// the attempt runs before its append ([Publisher.attempt]) asks the identity
// after that read — so the write is refused in the round that found the
// rebuild, before anything reaches the broker at zero, for exactly the branch
// where letting it through is not recoverable. A checkpoint past the log's END
// does not wait for that: the fence compares this node's checkpoint with the
// end it just read and refuses on the spot ([ZeroFence]), because that answer
// is its own and must not depend on what a concurrent reading made of the
// shared verdict in between.
//
// The other branches keep the heartbeat's window, and the trade is stated
// rather than hidden. An expectation above zero is accepted on a rebuilt log
// only where the subject's last sequence there happens to equal it. An
// additive record needs no such luck and does land — but it is the one kind
// that commutes, so a history it was never arbitrated in is one it cannot
// contradict; what is wrong about it is its resolution, and only for the
// seconds until the next beat. Checking the instant on every append would put
// a broker round trip on the hottest path the write authority has, to close a
// window that bounded. A checkpoint past the end has no such window at boot,
// which is when a restored broker is met: the engine's boot hands the applier
// the end it read beside the checkpoint before any write can run.
//
// # And why the identity OUTRANKS the fence's refusal, and is asked only there
//
// The fence's read that establishes a rebuild also finds the checkpoint past
// the rebuilt log's end — a log counting from 1 again ends below almost any
// checkpoint — and the fence refuses on the end, so the write never reaches
// the attempt whose fence 0 would have named the rebuild. Both are
// `wrong_stream`, but only one is the finding: a rebuild is permanent and is
// what an operator has to act on, and a checkpoint past the end of a log that
// was rebuilt is its consequence. So over any refusal the fence makes, the
// identity is asked and answers first — the order fence 0 asks in, and the
// order [Runner.StreamIdentity] reports in. Where it finds nothing the fence's
// refusal stands, which is what keeps the end check within the call even when
// a concurrent reading has cleared the shared verdict in between.
//
// Over a fence that CLEARED, nothing is asked here. The attempt's fence 0 is
// the next thing this write does, and it asks the same identity after the
// same read — so a second ask on that path could never be the one that
// refused, and a check nothing can ever find doing anything is a claim rather
// than a guard.
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
// its positions are on refuses every write — see [Identity] for why every
// pattern and not only the retry at zero.
//
// # With one exception: a node gate over a passed generation
//
// A node a peer re-anchored past holds rows the log no longer continues, so
// anything it decides from them is refused — but an eviction or a readmission
// ([Request.NodeGate]) is decided from nothing in them. Its content is the
// operator's gesture; its arbitration is on the gated node's own subject,
// which only another such gesture writes, so an expectation formed from this
// node's rows is either the broker's own last record there or refused by the
// broker; and the generation it is stamped with is below the fleet's, which
// every applier in the new generation applies at its own position. Refused
// here, it was the gesture no node of a fleet stranded by a decommissioned peer
// could make: every one of them had been passed by that peer, and evicting it
// is how the peer's generation stops being the fleet's. A RECREATED stream is
// never excused — this node's expectations there are sequences on another
// stream — and that finding outranks the passed one, so it is what is asked.
func (p *Publisher) checkIdentity(req Request) error {
	err := p.identity.StreamIdentity()
	if err == nil || (req.NodeGate && errors.Is(err, ErrGenerationPassed)) {
		return nil
	}
	return &Unavailable{
		Reason: ReasonWrongStream,
		Detail: fmt.Sprintf("%v — a write here would be arbitrated against a "+
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
	if before, err := p.waiter.Committed().Before(req.Session); err != nil {
		return err
	} else if !before {
		return nil
	}
	started := time.Now()
	waitCtx, cancel := context.WithTimeout(ctx, p.resolveBudget)
	defer cancel()
	err := p.waiter.WaitCommitted(waitCtx, req.Session)
	p.observeMillis(metrics.StatelogWriteSessionWait, started, metrics.Attrs{
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
