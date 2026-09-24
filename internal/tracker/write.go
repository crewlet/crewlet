package tracker

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
	"github.com/crewlet/crewlet/internal/store"
)

// The write paths, and the one sentence they all follow.
//
//	Take ONE snapshot of your own rows. Decide and form the expectation
//	inside it. Publish. Let the broker arbitrate. Never guess.
//
// Everything below is that sentence with a payload attached. The framework
// owns the loop — the snapshot, the expectation, the rounds, the resolution of
// an ambiguous append — and what a write path contributes is a DECISION
// FUNCTION: given the rows in this transaction, what record should be
// published, or what should the caller be told instead.
//
// # Three outcomes, never a bool
//
// A write is `applied`, `pending` or `unknown`. Applied means the rows are
// here; pending means the record is committed on the log and this node has not
// consumed it yet; unknown means the acknowledgement was lost and the record
// may or may not be there. Collapsing the last two into "failed" is how a
// caller retries a write that landed — and collapsing them into "succeeded" is
// how a caller reports work that was never recorded.

// ErrStaleVersion reports an update conditioned on a version that has moved.
//
// ITS OWN SENTINEL because the caller's answer differs from every other
// refusal: nothing is wrong with the request, somebody else simply got there
// first, and the correct next move is to re-read and decide again — never to
// retry the same patch, which is what a generic failure invites.
var ErrStaleVersion = errors.New("tracker: the task has changed since it was read")

// ErrReassignmentBudget reports a hand-off past [ReassignmentBudget].
var ErrReassignmentBudget = errors.New("tracker: this task has been handed on too many times")

// NoIfMatch omits an update's version precondition, which MERGES the patch
// onto whatever the task currently is. Named rather than a bare zero, because
// a literal 0 in a seven-argument call says nothing about which of the two
// behaviours it selects.
const NoIfMatch uint64 = 0

// ReassignmentBudget is how many times AGENTS may hand one task on before the
// engine refuses and the item needs a person.
//
// # Why the budget is on the ITEM and not on the delegation depth
//
// An assignment is an ownership transfer down a chart of KNOWN HEIGHT, not a
// nested ask: handing a task to your report is one hop on a graph a founder
// authored, and bounding it by the delegation cap would refuse a legitimate
// escalation four levels down while permitting an infinite ping-pong between
// two peers. What actually goes wrong is the loop — two seats each convinced
// the other owns it — and a loop is a property of the ITEM.
//
// Eight, because the deepest chart this engine is designed for is four tiers
// (founder, lead, senior, individual) and a legitimate path is an escalation
// UP and a delegation back DOWN — seven hops at the very worst, plus one so
// the honest worst case is not itself the refusal. Past that nothing is being
// routed; the item is circulating, and the counter resets on any human or
// operator touch so a person unblocking it hands back a full budget.
const ReassignmentBudget = 8

// Writer is the tracker's write authority.
//
// Every write takes the OPERATION ID it publishes under from its caller, and
// the id carries the instant it was minted: a caller mints it with
// [statelog.NewOpID], or derives it with [statelog.DeriveOpID] when a retry
// must reproduce it. There is no instant beside it to stamp, because the
// state log reads that one to decide whether this node's ledger can vouch for
// a retry — see [statelog.OpMintedAt] — and the writer's own clock at the call
// is always later than the mint on exactly the retry that question is for.
type Writer struct {
	publisher *statelog.Publisher

	// db is the replicated estate, read by the sequences that need to see
	// a subtree BEFORE their first append. It is never the write path's
	// own snapshot — that is the framework's, taken per append — and
	// nothing decided here is paired with an expectation.
	db *store.DB

	// claims is the coordination a walking sequence takes its claim from,
	// and nodeID is where it runs — the prefix of every claim's owner, never
	// the owner itself ([Writer.claimOwner]). Both may be nil or empty on a
	// writer that only makes single-append writes; a sequence that needs one
	// refuses by name rather than running without it.
	claims Claims
	nodeID string

	// Actor and ActorKind are who this writer acts as, and OperatorID,
	// TurnID and Chain the provenance that travels with it.
	//
	// ON THE WRITER rather than on each call, because a surface acts as
	// exactly one party — and a tracker whose author field is an argument
	// is not an audit trail. A surface serving many parties takes one
	// writer per party through [Writer.As], which is the same rule stated
	// the other way round: the identity comes from the surface's own
	// immutable context, never from the call.
	Actor      string
	ActorKind  AuthorKind
	OperatorID string
	TurnID     string
	Chain      []string

	// Seat is the chart seat this writer's credential is bound to, empty
	// for every writer that is already a seat and for a token nobody
	// bound. See [Provenance.Seat] and [Writer.Record].
	Seat string

	// metrics is where the counters this package owns are recorded. Nil
	// records nothing, which is what a writer built for a test gets: the
	// instruments are the engine's, and a nil check here is cheaper than
	// a second recorder nobody reads.
	metrics *metrics.Recorder

	// Drain is the applier's measured rows a second on this node, which
	// is the divisor of every projection and every retry hint computed
	// from one. Nil means unmeasured, which the projection reads as its
	// pessimistic floor rather than as infinity.
	Drain func() float64

	// Leads resolves a project's or a unit's lead, and it is the ONE
	// thing this writer needs that the rows cannot give: a project's lead
	// is chart-owned, `tracker_projects` carries no column for it, and the
	// applier may not read an org at all — two nodes briefly on different
	// epochs would write different rows from the same record.
	//
	// ON THE WRITER rather than per call, unlike the task wakes' own
	// [Wake.Notify], because the caller that needs it here is a fleet
	// singleton on a tick, with no tool arguments to carry a seam through.
	// Nil resolves to no lead, which costs a wake its fallback recipient
	// and never its delivery.
	Leads Leads

	// World is the two lookups the custom-field coercion table cannot do
	// itself — a relationship's task and a people field's seat.
	//
	// ON THE WRITER for the same reason [Writer.Leads] is: the caller that
	// needs it may be a duty on a tick with no arguments to carry a seam
	// through. Nil refuses those two field types BY NAME rather than
	// admitting anything, because a handle nothing checked is a value that
	// filters against nobody.
	World FieldWorld

	// Now is the clock the AUTHORED instants are stamped from. An
	// argument rather than a package call, so a test can pin it and so
	// nothing on the write path reads a clock the applier is forbidden.
	Now func() time.Time

	// after is one of this writer's OWN earlier writes, carried so the
	// framework waits for this node's applier to reach it before it opens
	// the next snapshot. Zero means there is nothing the next write has to
	// see first, which is every write a surface makes on its own. See
	// [Writer.After].
	after statelog.Position

	// refusal is set by [Writer.As] when the identity it was handed
	// cannot author a record. It is checked at the one funnel every write
	// passes through — see the comment there for why it is carried rather
	// than returned.
	refusal error
}

// WriterDeps is everything a writer needs that it does not own.
type WriterDeps struct {
	Publisher *statelog.Publisher
	DB        *store.DB
	Claims    Claims
	NodeID    string

	// World is the chart seam the custom-field coercion needs for the one
	// field type whose value is a colleague — see [Writer.World].
	World FieldWorld

	Metrics   *metrics.Recorder
	Drain     func() float64
	Actor     string
	ActorKind AuthorKind
	Leads     Leads
	Now       func() time.Time
}

// As is this writer acting as somebody else, and it is the ONLY way the actor
// ever changes.
//
// # Why a copy rather than an argument
//
// The rule is that a writer acts as exactly one party, because a history row
// whose author was chosen by the caller is not an audit trail. A surface that
// serves many parties — a seat's tool loop, an operator's MCP session —
// therefore takes one writer per party, derived from that surface's own
// IMMUTABLE identity: the seat bound into the turn context, or the credential
// on the request. Neither is something a model or a request body can set.
//
// The copy is shallow and shares the publisher, the store and the claims,
// which is what makes a per-call writer cost nothing.
func (w *Writer) As(actor string, kind AuthorKind, provenance Provenance) *Writer {
	if w == nil {
		return nil
	}
	clone := *w
	if actor == "" || !kind.Valid() {
		// THE SAME REFUSAL [NewWriter] MAKES, because this is the other
		// door into the same state: a clone that skipped it would be the
		// one writer in the tree recording history rows nobody can
		// attribute.
		//
		// CARRIED rather than returned, because the caller is a surface
		// resolving an identity it has already established — there is no
		// second identity to fall back to and nothing useful to do with
		// an error at that point. Every write goes through one funnel,
		// so the refusal surfaces at the call that would have written,
		// naming the operation, rather than as a nil pointer somewhere
		// deeper.
		clone.refusal = fmt.Errorf("tracker: a writer cannot act as %q of "+
			"kind %q — every record carries who wrote it", actor, kind)
	}
	clone.Actor = actor
	clone.ActorKind = kind
	clone.OperatorID = provenance.OperatorID
	clone.TurnID = provenance.TurnID
	clone.Chain = provenance.Chain
	clone.Seat = provenance.Seat
	return &clone
}

// Record is whose OWN STATE this writer's person writes belong to: the seat
// the credential is bound to, or the actor itself where nothing is bound.
//
// # It is deliberately not the author
//
// [Writer.Actor] is who WROTE the record and it stays the token, because a
// tracker whose author field is chosen by the writer is not an audit trail
// (see internal/api/opsmcp). This answers the other question — whose inbox,
// whose pins, whose queue — and the answer there is the PERSON. A founder
// whose assistant marks their inbox read is marking `jane-founder`'s inbox and
// signing it `founder`: one gesture with two different correct answers, rather
// than one answer used for both.
//
// The actor answered both, so a bound founder grew a SECOND person record
// under their credential's name — reachable only through [readPartyRecord]'s
// fallback, and invisible to somebody who also had a record under their seat.
func (w *Writer) Record() string {
	if w == nil {
		return ""
	}
	if seat := strings.TrimSpace(w.Seat); seat != "" {
		return seat
	}
	return w.Actor
}

// Party is every identity a record this writer OWNS may be filed under — the
// person first, the credential behind them.
//
// FOR AN OWNERSHIP TEST AND NEVER FOR AN AUTHOR FIELD. A row written before a
// company bound the token carries the credential's name and the same person is
// behind both, so a check that admitted only [Writer.Record] would lock a
// founder out of what their own assistant saved.
func (w *Writer) Party() Party {
	if w == nil {
		return Party{}
	}
	return Party{Handle: w.Record(), OperatorID: w.OperatorID}
}

// After is this writer carrying one of its own earlier writes, and it is how a
// GESTURE that touches one subject twice stays a single wait rather than a
// wasted round.
//
// # The shape it is for
//
// A sequence's later append onto a subject an earlier append of the SAME
// sequence already moved decides from a snapshot of THIS NODE's rows — and the
// record that moved that subject is still on its way to this node's applier.
// So the expectation formed in that snapshot is one the earlier append has
// already made stale. The framework recovers correctly, but only after it has
// opened a snapshot, run the decide, asked the broker for the subject's last
// sequence and been refused — and what it does then is wait for exactly the
// position this writer was holding all along.
//
// [statelog.Request.Session] is that wait moved AHEAD of all of it. It costs
// nothing when the applier is caught up, which is the common case; the decide
// never runs against a state below the caller's own write, so no decision is
// formed from rows that are about to move; and when the wait does expire the
// refusal names THE CALLER'S OWN WRITE rather than "the log" — the difference
// between "my applier is lagging" and "a colleague is editing this", which an
// operator acts on differently and which the session-wait histogram separates
// from every other reason a write waits.
//
// # Why a copy, and why it is not accumulated
//
// [Writer.As]'s idiom, for a weaker version of its reason: the mark belongs to
// ONE gesture. A writer that remembered every position it had published would
// carry a stale one into every later write a surface made — and on a surface
// serving a whole turn that is an unbounded wait for a record nothing is
// waiting on.
//
// It is deliberately not the tool layer's mechanism either. A tool call that
// writes and then READS its own rows settles — waiting after the write, best
// effort, because the write already landed and telling a model its create
// failed is how a duplicate gets filed. This one waits before the NEXT WRITE
// and refuses, because a write decided from a stale snapshot is not a stale
// answer, it is a wrong one.
//
// # And why it is not carried on the TURN
//
// A turn-wide mark was the obvious next step and is the wrong scope twice
// over. It would be a mutable value SHARED by every goroutine a turn ever
// started — the one hazard the turn context exists to prevent, and a live data
// race rather than a merely obscured dependency. And a mark that is a stream
// position rather than a subject's makes every append of every sequence wait
// for the one before it: a create is three appends on three different
// subjects, none of which reads what the others wrote, so the wait would buy
// nothing and cost a whole apply per step.
//
// What actually covers a turn is already there. Between two tool calls the
// settle above has run, so the second call's writes are not below the first's;
// and when that settle fails — it is best effort — the framework still
// recovers, one wasted round later, with the same refusal under a less exact
// name. Only a GESTURE's own appends have no settle between them, which is
// precisely the scope this method has.
//
// THE ZERO POSITION IS A NO-OP, so a step whose predecessor published nothing
// — a dependency change that touches only the far end — passes what it has
// without a branch.
func (w *Writer) After(at statelog.Position) *Writer {
	if w == nil {
		return nil
	}
	clone := *w
	clone.after = at
	return &clone
}

// Provenance is what an audit walks from a record back to what produced it.
//
// It BOUNDS NOTHING. A hand-off is charged to the task's own reassignment
// counter and not to the delegation depth, so the chain here is a trail rather
// than a budget.
type Provenance struct {
	// OperatorID is the credential a person's own write was made under,
	// recorded beside the actor rather than instead of it.
	OperatorID string

	// Seat is the chart seat that credential is BOUND to with
	// `contact.crewlet_operator_id`, resolved by the surface because this
	// package holds no org. Empty for a token nobody bound, and for every
	// writer that is already a seat.
	//
	// IT NAMES NO AUTHOR, on a record or anywhere else, which is why it
	// sits here with the rest of the provenance: the author stays the
	// token and the kind stays `operator`, because that is the audit
	// trail. What it decides is WHOSE STATE a person write lands on (see
	// [Writer.Record]) — and, on the record itself, WHO NOT TO WAKE.
	//
	// It travels as [MutationRecord.ActorSeat] for that second reader
	// alone: a bound operator's own gestures land on their seat, so
	// without it the wake's actor exclusion compared a candidate's handle
	// against a credential and woke the person for their own write.
	Seat string

	// TurnID is the turn that produced this write, and Chain the
	// delegation path that reached it.
	TurnID string
	Chain  []string
}

// NewWriter builds the tracker's write authority.
func NewWriter(d WriterDeps) (*Writer, error) {
	switch {
	case d.Publisher == nil:
		return nil, fmt.Errorf("tracker: a writer has no publisher")
	case d.Actor == "":
		return nil, fmt.Errorf("tracker: a writer has no actor — every record " +
			"carries who wrote it, and one that does not is a history row " +
			"nobody can attribute")
	case !d.ActorKind.Valid():
		return nil, fmt.Errorf("tracker: %q is not an author kind", d.ActorKind)
	}
	now := d.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &Writer{
		publisher: d.Publisher, db: d.DB, claims: d.Claims, nodeID: d.NodeID,
		metrics: d.Metrics, Actor: d.Actor, ActorKind: d.ActorKind,
		Drain: d.Drain, Leads: d.Leads, World: d.World, Now: now,
	}, nil
}

// UpdateTask changes one.
//
// ARBITRATED AGAINST THE TASK'S OWN LAST RECORD, which is what makes two
// writers on one task contend at the broker and two writers on different tasks
// never contend at all.
//
// # ifMatch, and why it is a PARAMETER rather than a patch field
//
// ifMatch is the caller's own precondition: the version it read, refused if
// anybody has moved the task since. Zero omits it, which MERGES — the honest
// default for a caller naming two fields it does not want to lose a race over.
//
// It is not on [TaskPatch] because a patch is the RECORD, replayed by every
// node's applier forever, and a precondition has no meaning at apply time: the
// broker already arbitrated, so a node re-checking it would either agree
// (waste) or disagree (and diverge from its peers). The check belongs at the
// one place the framework guarantees a single consistent read — inside the
// decide snapshot — which is exactly where it is.
//
// # And why the hand-off budget is charged here too
//
// [ReassignmentBudget] is a decision about the task's CURRENT counter, so it
// is formed from the same snapshot as everything else and travels as a value
// on the record. Deciding it in the applier instead would make every node
// re-derive it from rows it applied in its own order, and a counter derived
// twice is a counter two nodes can disagree about.
func (w *Writer) UpdateTask(ctx context.Context, opID, id, project string,
	ifMatch uint64, patch TaskPatch, kind ChangeKind,
	notify *Notify) (WriteResult, error) {

	switch {
	case id == "":
		return WriteResult{}, fmt.Errorf("tracker: an update names no task")
	case project == "":
		return WriteResult{}, fmt.Errorf("tracker: an update on task %s "+
			"names no project — the caller resolved a key to reach this task "+
			"and therefore holds one", id)
	}
	// THE COMMENT'S BODY IS CHECKED HERE TOO, because a comment rides a
	// task write rather than having a write of its own — so this is the
	// one place every comment in the engine passes through.
	var commentBody *string
	if patch.Comment != nil {
		commentBody = &patch.Comment.Body
	}
	if err := checkTextCaps(id, patch.Title, patch.Body, commentBody); err != nil {
		return WriteResult{}, err
	}
	if patch.Tags != nil {
		// NORMALISED BEFORE THE PUBLISH, so the record carries the
		// spelling the rows hold rather than the one somebody typed —
		// every node applies this payload, and a node that re-derived
		// the slug itself would be a second implementation of the
		// grammar.
		tags, err := normaliseTags(project, *patch.Tags)
		if err != nil {
			return WriteResult{}, err
		}
		patch.Tags = &tags
	}
	subject := TaskSubject(id)
	// A PROJECT MOVE TOUCHES BOTH CONTAINERS, so it states them: the
	// task's rows leave one project's closure and arrive in another's, and
	// a scope naming only the destination would let a write into the
	// project it left slip past a deferral that covers it.
	scope := ScopeSet{Subject: true, Container: project}
	if patch.Project != nil && *patch.Project != project {
		scope = ScopeSet{Terms: []ScopeTerm{
			{Kind: TermObject, Container: project, ID: id},
			{Kind: TermObject, Container: *patch.Project, ID: id},
		}}
	}
	// EVERY PATCH, not only a status one. The apply rewrites every row
	// naming this task as a blocker on EVERY task apply — `maintainDeps`
	// runs out of `explodeTask`, which nothing gates — so a patch that
	// changed only a due date wrote a dependent's row under a scope that
	// never named it.
	//
	// Gated on the status, this was UNRECOVERABLE rather than merely
	// narrow: [ScopeSet.covers] inside the decide refuses such a write and
	// tells the caller to re-run, and the re-run took the same gate and
	// came up short again. Any edit at all to a task somebody waits on was
	// refused for ever, with an error promising it would not be.
	var err error
	if scope, err = w.scopeForDependents(ctx, id, project, scope); err != nil {
		return WriteResult{}, err
	}
	at := w.Now()

	// WARNINGS ARE COLLECTED FROM INSIDE THE DECIDE, which runs again on
	// every round: the LAST run is the one whose record was published, so
	// the slice is replaced rather than appended to.
	var fieldWarnings, promoteWarnings []string
	result, err := w.published(ctx, statelog.Request{
		Subject: wire(subject),
		Scope:   scope.Resolve(subject),
		OpID:    opID,
		Pattern: statelog.PatternArbitrated,
		Decide: func(tx *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			//nolint:govet // shadow: `x, err := f()` declares x too; see .golangci.yml
			current, held, err := readTask(ctx, tx, id)
			if err != nil {
				return statelog.Decision{}, err
			}
			if !held {
				return statelog.Decision{}, fmt.Errorf("tracker: task %s is not "+
					"on this node: %w", id, statelog.ErrUnavailable)
			}
			if current.Removed != nil {
				// A TOMBSTONED TASK IS FROZEN — no comment, body, field
				// or relation of it can change — which is what makes a
				// removal an entirely local decision with no walk
				// behind it.
				return statelog.Decision{}, fmt.Errorf("tracker: task %s was "+
					"removed by %s at %s; restore it first",
					id, current.Removed.By, current.Removed.At.Format(time.RFC3339))
			}
			if current.Project != project {
				// THE SCOPE NAMES THE PROJECT THE CALLER SAID, and the
				// record is filed and probed under it. A task in
				// another one would be written under a container it is
				// not in — so a deferral on its real project would not
				// hold this write back, and one on the named project
				// would hold back a write that never touches it. A bulk
				// edit names one project for every task in it, which is
				// where a task from elsewhere arrives.
				return statelog.Decision{}, fmt.Errorf("tracker: task %s is in "+
					"project %s, not %s — resolve it again and name the "+
					"project it is in", id, current.Project, project)
			}
			if ifMatch != 0 && current.Version != ifMatch {
				return statelog.Decision{}, fmt.Errorf("%w: task %s is at "+
					"version %d and the edit was conditioned on %d — re-read "+
					"it and decide again rather than re-sending this patch",
					ErrStaleVersion, id, current.Version, ifMatch)
			}
			if markOnly(patch) && *patch.Moving == current.Moving {
				// THE MARK ALREADY SAYS IT: a walk's last append whose
				// mark somebody else took down — the holder, racing the
				// duty that finished its walk — or a re-run of one that
				// landed. An empty decision is a success, as it is for
				// a promotion with nothing left to mark, rather than a
				// history row recording that nothing moved.
				return statelog.Decision{Version: int64(current.Version)}, nil
			}
			if patch.Tags != nil {
				// AGAINST THE TASK'S OWN PROJECT rather than the
				// argument's, because a move carries the tags into
				// the destination before it re-homes the task and
				// this read is the one that sees both.
				home := current.Project
				if patch.Project != nil {
					home = *patch.Project
				}
				//nolint:govet // shadow: scoped to this block; see .golangci.yml
				if err := declaredTags(ctx, tx, home, *patch.Tags); err != nil {
					return statelog.Decision{}, err
				}
			}
			charged, err := w.chargeHandOff(current, patch)
			if err != nil {
				return statelog.Decision{}, err
			}
			charged, err = settleWatch(current, charged)
			if err != nil {
				return statelog.Decision{}, err
			}
			charged, err = w.settleRelations(current, charged)
			if err != nil {
				return statelog.Decision{}, err
			}
			charged, err = settleDependents(current, charged)
			if err != nil {
				return statelog.Decision{}, err
			}
			var note string
			charged, note, err = settlePromote(current, charged)
			if err != nil {
				return statelog.Decision{}, err
			}
			promoteWarnings = nil
			if note != "" {
				promoteWarnings = []string{note}
			}
			if patch.Promote != nil && charged.Empty() {
				// NOTHING LEFT TO MARK: the item already points at
				// this subtask, or is no longer on the parent. An
				// empty decision is a success rather than a record
				// saying nothing — which is what makes the parent
				// step of a promotion re-runnable.
				return statelog.Decision{Version: int64(current.Version)}, nil
			}
			if charged.Fields != nil {
				// THE COERCION TABLE, and the required-field half that
				// only ever ran on a create. A patch reaching here
				// unchecked is how "about 7 hours" became 7 on a number
				// field and a required field was emptied by an update
				// the next create of the same shape would refuse.
				//nolint:govet // shadow: `x, err := f()` declares x too; see .golangci.yml
				coerced, warned, err := settleFields(ctx, tx, current.Project,
					current.Type, *charged.Fields, w.World)
				if err != nil {
					return statelog.Decision{}, err
				}
				if err := refuseUnsetRequired(ctx, tx, current, coerced); err != nil {
					return statelog.Decision{}, err
				}
				charged.Fields = &coerced
				fieldWarnings = warned
			}
			// THE SCOPE THE REQUEST CLAIMED STILL COVERS THIS WRITE.
			//
			// EVERY task apply rewrites `blocker_open` and
			// `cleared_at` on every row that names this task as a
			// blocker — rows keyed on OTHER tasks — so the record
			// enumerates one object term per dependent (see
			// [Writer.scopeForDependents]). That enumeration is read
			// BEFORE the request is built, because the publisher probes
			// the deferral index with the REQUEST's scope while the
			// applier files a deferral under the ENVELOPE's, and the
			// two must be one set.
			//
			// This is the check that makes reading it early honest: if
			// a dependent arrived between that read and this snapshot,
			// the claim is short by one object and the write is refused
			// rather than published under a scope that does not cover
			// it. The caller re-runs and the second attempt enumerates
			// the dependent that arrived.
			//nolint:govet // shadow: scoped to this block; see .golangci.yml
			if err := scope.covers(current.Dependents); err != nil {
				return statelog.Decision{}, err
			}
			decision, err := w.decide(stamp, subject, OpPatch, kind, scope, opID,
				charged, notify, at)
			if err != nil {
				return statelog.Decision{}, err
			}
			decision.Version = int64(current.Version)
			return decision, nil
		},
	})
	if patch.Body != nil {
		result.Warnings = bodyWarnings(*patch.Body)
	}
	result.Warnings = append(result.Warnings, fieldWarnings...)
	result.Warnings = append(result.Warnings, promoteWarnings...)
	return result, err
}

// markOnly reports a patch whose one change is the cross-project move's mark.
func markOnly(patch TaskPatch) bool {
	if patch.Moving == nil {
		return false
	}
	patch.Moving = nil
	return patch.Empty()
}

// settlePromote resolves a promotion's mark into the parent's whole checklist
// set, against the parent this decide read.
//
// INSIDE THE DECIDE SNAPSHOT for the reason [settleWatch] gives: the mark is a
// change to ONE item of a collection carried whole, and composing the whole
// from any other read loses whatever landed in between.
//
// Three outcomes besides the mark. The item ALREADY POINTS AT THIS SUBTASK —
// an earlier run marked it — and there is nothing to write. The item is NO
// LONGER THERE — somebody deleted the line after the subtask was minted — and
// there is nothing to mark either: the subtask stands on its own, and the
// caller is told rather than refused, because a refusal would make the
// promotion unfinishable for a reason nothing can undo. And the item already
// became ANOTHER task, which is refused: one line is one piece of work.
//
// It returns a COPY, like every settle here.
func settlePromote(current Task, patch TaskPatch) (TaskPatch, string, error) {
	if patch.Promote == nil {
		return patch, "", nil
	}
	intent := *patch.Promote
	switch {
	case patch.Checklists != nil:
		return patch, "", fmt.Errorf("tracker: this patch carries both a "+
			"promotion of item %s and a whole checklist set — a caller states "+
			"one or the other", intent.Item)
	case intent.Item == "" || intent.Subtask == "":
		return patch, "", fmt.Errorf("tracker: a promotion mark names item %q "+
			"and subtask %q, and needs both", intent.Item, intent.Subtask)
	}
	patch.Promote = nil
	l, i, found := findItem(current, intent.Item)
	if !found {
		return patch, fmt.Sprintf("checklist item %s is no longer on task %s, so "+
			"nothing marks it as having become task %s; the subtask stands on "+
			"its own", intent.Item, current.ID, intent.Subtask), nil
	}
	if to := current.Checklists[l].Items[i].PromotedTo; to != nil {
		if *to == intent.Subtask {
			return patch, "", nil
		}
		return patch, "", fmt.Errorf("tracker: checklist item %s of task %s "+
			"already became task %s, so it cannot also become %s",
			intent.Item, current.ID, *to, intent.Subtask)
	}
	lists := markPromoted(current, intent.Item, intent.Subtask)
	patch.Checklists = &lists
	return patch, "", nil
}

// settleWatch resolves a membership gesture into the whole watcher sets.
//
// INSIDE THE DECIDE SNAPSHOT, exactly where [Writer.chargeHandOff] settles the
// hand-off counter and for the same reason this file's own head gives: "add me
// to the watchers" is a decision about the task's CURRENT set, and the decide
// closure is the one place this engine guarantees a single consistent read of
// it. A caller that read the set in one transaction and wrote it in another
// would drop everybody who arrived in between.
//
// BOTH SETS MOVE TOGETHER, because [Task.Watchers] is the set and [Task.Muted]
// the subtraction: watching is "in watchers, not in muted" and un-watching is
// the mirror. Writing one without the other leaves a person who cannot be told
// apart from somebody who never watched, which is the distinction those two
// fields exist to keep.
//
// And it returns a COPY rather than writing through the patch it was given:
// Decide runs again on a retry, and a resolution folded into the captured
// patch would compound across attempts.
func settleWatch(current Task, patch TaskPatch) (TaskPatch, error) {
	if patch.Watch == nil {
		return patch, nil
	}
	if patch.Watchers != nil || patch.Muted != nil {
		// BOTH SPELLINGS AT ONCE IS A PROGRAMMING ERROR, refused rather
		// than resolved in some order: one of them is a gesture about
		// one person and the other is the whole set, and whichever won
		// would silently discard the other.
		return patch, fmt.Errorf("tracker: this patch carries both a watch "+
			"gesture for %s and a whole watcher set — a caller states one or "+
			"the other", patch.Watch.Handle)
	}
	handle := patch.Watch.Handle
	if handle == "" {
		return patch, fmt.Errorf("tracker: a watch gesture names no handle")
	}
	watchers := without(current.Watchers, []string{handle})
	muted := without(current.Muted, []string{handle})
	if !patch.Watch.Watch {
		// THE MUTE IS THE WHOLE POINT OF AN UNWATCH. Dropping somebody
		// from the watcher set only says they are not watching NOW; the
		// mute is what says they CHOSE not to, which is what stops the
		// next mention silently putting them back.
		muted = append(muted, handle)
		// AND AN UNWATCH IS NEVER REFUSED, which is why the branch
		// returns here rather than falling through to the cap below.
		// That check used to run on both branches, over a set an unwatch
		// can only SHRINK — so a task that had somehow grown past the
		// cap was one nobody could leave, and the only gesture that
		// could have brought it back under was the one being refused.
		patch.Watch = nil
		patch.Watchers, patch.Muted = &watchers, &muted
		return patch, nil
	}
	watchers = append(watchers, handle)
	if len(watchers) > MaxWatchers {
		// THE ROUTING CAP, enforced at the one place a watcher set grows
		// by one. Past it an item is a broadcast rather than a thing
		// people follow, and the wake it sends is the company's whole
		// inbox — which is what [MaxWatchers] was declared to bound and,
		// until this gesture existed, nothing in this package did.
		//
		// AN AUTOMATIC WATCH IS SKIPPED RATHER THAN REFUSED, which is
		// [WatchIntent.Auto]'s whole purpose: a commenter picks up a
		// watch by commenting, and failing their comment because
		// sixty-four other people are watching refuses a write for a
		// reason that has nothing to do with what they asked for. The
		// comment lands; the watch does not.
		if patch.Watch.Auto {
			patch.Watch = nil
			return patch, nil
		}
		return patch, fmt.Errorf("tracker: task %s already has %d watchers and "+
			"the maximum is %d — an item this many people follow is an "+
			"announcement, and a comment on it wakes all of them",
			current.ID, len(current.Watchers), MaxWatchers)
	}
	patch.Watch = nil
	patch.Watchers, patch.Muted = &watchers, &muted
	return patch, nil
}

// chargeHandOff settles the reassignment counter this patch leaves behind, or
// refuses the hand-off.
//
// THREE CASES, and each is a different answer to "who is routing this":
//
//   - An AGENT moving the assignee to somebody else spends one. Past
//     [ReassignmentBudget] it is refused: the task is circulating rather than
//     being routed, and the next move is a person's.
//   - A HUMAN or an OPERATOR touching the task at all resets it to zero,
//     whatever they changed. Somebody looked, so the budget that exists to
//     notice nobody is looking starts again.
//   - Anything else leaves the counter alone — the engine's own writes
//     (a duty's repair) neither spend nor forgive.
//
// A no-op assignment does not spend: re-asserting the current assignee is what
// an idempotent retry looks like, and charging it would let a redelivered
// record exhaust a budget nobody used.
func (w *Writer) chargeHandOff(current Task, patch TaskPatch) (TaskPatch, error) {
	switch w.ActorKind {
	case AuthorHuman, AuthorOperator:
		if current.Reassignments != 0 {
			reset := 0
			patch.Reassignments = &reset
		}
		return patch, nil
	case AuthorAgent:
		if patch.Assignee == nil || *patch.Assignee == current.Assignee {
			return patch, nil
		}
		if current.Reassignments >= ReassignmentBudget {
			return patch, fmt.Errorf("%w: %s has been handed on %d times "+
				"without a person touching it, and the budget is %d — say "+
				"what is blocking it on the item instead, and let somebody "+
				"reassign it", ErrReassignmentBudget, current.Key,
				current.Reassignments, ReassignmentBudget)
		}
		spent := current.Reassignments + 1
		patch.Reassignments = &spent
		return patch, nil
	default:
		return patch, nil
	}
}

// MoveTasks repositions tasks in a project's manual order.
//
// # Why the subject is the ORDER and not any of the tasks
//
// The object a drag mutates is the project's ORDER — a total order over its
// tasks that no single task owns and no single task's version can protect. Two
// people dragging two different cards in one project are editing the same
// object, and arbitrating on either card would let both writes land and leave
// the order neither of them intended.
//
// The scope is the project CONTAINER rather than an enumeration, because the
// affected set is the tasks named plus up to a re-spread's worth of
// neighbours: a writer whose set could exceed the term cap states the smallest
// covering term instead, which is what keeps the field bounded by construction
// rather than by a cap a writer can hit and then have to handle.
func (w *Writer) MoveTasks(ctx context.Context, opID, project string,
	placements []Placement) (WriteResult, error) {

	switch {
	case project == "":
		return WriteResult{}, fmt.Errorf("tracker: a move names no project")
	case len(placements) == 0:
		return WriteResult{}, fmt.Errorf("tracker: a move places no task")
	case len(placements) > MaxBulkTasks+RankRespreadInline:
		// THE CALLER'S OWN MOVES PLUS A RE-SPREAD'S WORTH OF
		// NEIGHBOURS. The scope is the project CONTAINER rather than an
		// enumeration precisely so the placement list is not bounded by
		// the term cap: one drag can legitimately rewrite hundreds of
		// neighbouring keys, and a covering term costs one path.
		return WriteResult{}, fmt.Errorf("tracker: a move carries %d "+
			"placements and one record carries at most %d — %d moves plus "+
			"a re-spread's %d neighbours", len(placements),
			MaxBulkTasks+RankRespreadInline, MaxBulkTasks, RankRespreadInline)
	}
	subject := RankOrderSubject(project)
	scope := ScopeSet{Terms: []ScopeTerm{{Kind: TermContainer, ID: project}}}
	at := w.Now()

	return w.published(ctx, statelog.Request{
		Subject: wire(subject),
		Scope:   scope.Resolve(subject),
		OpID:    opID,
		Pattern: statelog.PatternArbitrated,
		Decide: func(tx *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			for _, placement := range placements {
				if !placement.Rank.Valid() {
					return statelog.Decision{}, fmt.Errorf("tracker: %q is not "+
						"a well-formed rank key", placement.Rank)
				}
			}
			// A RANK MOVE CARRIES NO NOTIFICATION. A reposition is not
			// history: it changes where a card sits and nothing about
			// what the work is, so waking anybody for it would make a
			// board's own drag a source of inbox traffic.
			return w.decide(stamp, subject, OpPatch, "", scope, opID, RankOrder{
				V: DocumentVersion, Project: project, Placements: placements,
			}, nil, at)
		},
	})
}

// WriteDocument publishes a whole-document object.
//
// ONE PATH FOR SEVEN KINDS, because full post-state is one upsert with no
// patch semantics to get wrong — and the kinds that take it are exactly the
// ones small enough for that to be affordable.
func (w *Writer) WriteDocument(ctx context.Context, opID string, subject Subject,
	container string, document any, kind ChangeKind,
	notify *Notify) (WriteResult, error) {

	if _, _, err := documentTable(subject); err != nil {
		return WriteResult{}, err
	}
	// THE CONTAINER IS TAKEN AND CHECKED RATHER THAN ASSUMED. Most kinds
	// carry their own home in their subject and must not be given a second
	// one; a view chooses its own. Accepting one where it means nothing
	// would let a caller file a person's record under a project.
	if container != "" && !subject.Kind.HomedInAProject() {
		return WriteResult{}, fmt.Errorf("tracker: a %s names container %q, "+
			"and its own path is derived from its subject — a container here "+
			"would file its deferral where no probe for it looks",
			subject.Kind, container)
	}
	scope := ScopeSet{Subject: true, Container: container}
	at := w.Now()
	return w.published(ctx, statelog.Request{
		Subject: wire(subject),
		Scope:   scope.Resolve(subject),
		OpID:    opID,
		Pattern: statelog.PatternArbitrated,
		Decide: func(_ *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			return w.decide(stamp, subject, OpPatch, kind, scope, opID, document, notify, at)
		},
	})
}

// RecordTurn adds a turn's spend to a task.
//
// THE ONE ADDITIVE WRITE: it carries no expectation and races nobody, because
// a turn records something that already happened. Its idempotency is its own
// row's insert rather than an arbitration.
func (w *Writer) RecordTurn(ctx context.Context, opID, taskID, project string,
	payload any) (WriteResult, error) {

	if project == "" {
		return WriteResult{}, fmt.Errorf("tracker: a turn on task %s names "+
			"no project — a turn's path is its task's, so one without a project "+
			"files under a container the task is not in", taskID)
	}
	subject := TurnSubject(taskID)
	scope := ScopeSet{Subject: true, Container: project}
	at := w.Now()
	return w.published(ctx, statelog.Request{
		Subject: wire(subject),
		Scope:   scope.Resolve(subject),
		OpID:    opID,
		Pattern: statelog.PatternAdditive,
		Decide: func(_ *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			return w.decide(stamp, subject, OpTurn, "", scope, opID, payload, nil, at)
		},
	})
}

// checkChangeKind is the rule that a record which writes a history row states
// what it did, and one that writes none states nothing.
//
// # Why a writer states it rather than the applier deriving it
//
// Because the applier cannot. "What happened" is the caller's own knowledge —
// it created, it merged, it purged — and everything derivable from
// the two document states is already derived. What is NOT derivable is the
// difference between a catalogue patch and a view save, or between a tombstone
// and a purge: those are the same operation on different subjects, and the
// operation is all the applier has.
//
// It used to guess, from the operation and the moved fields, whenever a record
// carried no [Notify]. The guess is still there for records an older build
// wrote ([fallbackKind]) and it is no longer allowed to answer with a
// non-[ChangeKind]; but a build that can state the fact states it.
//
// THE THIRD RULE IS THE ONE WORTH THE FUNCTION: when a record carries both a
// kind and a notification, they must AGREE. One fact with two carriers is one
// fact that can disagree with itself, and the disagreement would be invisible
// — the feed would file the row under one word and the card render the other.
func checkChangeKind(subject Subject, op OpKind, kind ChangeKind, notify *Notify) error {
	switch {
	case !subject.Kind.RecordsHistory():
		if kind != "" {
			return fmt.Errorf("tracker: a %s record names change kind %q, and "+
				"an apply of it writes no history row for that kind to "+
				"describe — see ObjectKind.RecordsHistory", subject.Kind, kind)
		}
		if notify != nil {
			return fmt.Errorf("tracker: a %s record carries a notification, "+
				"and an apply of it writes no history row a wake could be "+
				"derived from", subject.Kind)
		}
		return nil
	case kind == "":
		return fmt.Errorf("tracker: a %s %s record names no change kind — its "+
			"apply writes a history row, and the kind is what every feed "+
			"filter, report window and repair scan selects on", subject.Kind, op)
	case !kind.Valid():
		return fmt.Errorf("tracker: %q is not a change kind this build writes "+
			"— every kind has exactly one writer, so an unknown one is a "+
			"history row no filter can name", kind)
	case notify != nil && notify.Kind != kind:
		return fmt.Errorf("tracker: this %s record says it is a %q and its "+
			"notification says %q — one fact with two carriers is one fact "+
			"that can disagree with itself, and a reader would see the feed "+
			"and the card name different things", subject.Kind, kind, notify.Kind)
	}
	return nil
}

// decide builds the record a write path publishes.
//
// ONE BUILDER for every path, so the envelope's eight reserved keys are filled
// in one place: a path that filled seven of them would publish a record the
// deferral index could not file, and the failure would only show up on a
// rolling upgrade.
//
// IT FILLED SIX. The generation and the writer were declared, documented and
// read — the writer by the eviction gate on every applier — and never set, so
// every record this domain ever published named nobody and the gate had no
// node to drop. Both now arrive as the framework's [statelog.Stamp], which the
// publisher checks on the encoded record before it appends anything.
func (w *Writer) decide(stamp statelog.Stamp, subject Subject, op OpKind,
	kind ChangeKind, scope ScopeSet, opID string, payload any, notify *Notify,
	at time.Time) (statelog.Decision, error) {

	if err := checkChangeKind(subject, op, kind, notify); err != nil {
		return statelog.Decision{}, err
	}
	if err := notify.Validate(); err != nil {
		return statelog.Decision{}, err
	}
	if err := scope.Validate(); err != nil {
		return statelog.Decision{}, err
	}
	if scope.Subject && scope.Container == "" && subject.Kind.RequiresAProject() {
		return statelog.Decision{}, fmt.Errorf("tracker: a %s record for %s "+
			"states no container, and there is no %s outside a project — an "+
			"empty one resolves to the workspace and files its deferral where "+
			"no project-scoped probe looks", op, subject, subject.Kind)
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return statelog.Decision{}, fmt.Errorf("tracker: encode the %s payload "+
			"for %s: %w", op, subject, err)
	}
	record := MutationRecord{
		RecordEnvelope: RecordEnvelope{
			V: recordVersionOf(payload), OpID: opID, Subject: subject, Op: op,
			CreatedAt: at, Gen: stamp.Gen, Writer: stamp.Writer, Scope: scope,
		},
		Kind:       kind,
		Mutation:   body,
		Actor:      w.Actor,
		ActorKind:  w.ActorKind,
		OperatorID: w.OperatorID,
		// THE SEAT BESIDE THE AUTHOR, never instead of it — see
		// [Provenance.Seat]. Empty for every writer that is already a
		// seat, which is every in-engine caller.
		ActorSeat: w.Seat,
		TurnID:    w.TurnID,
		Chain:     w.Chain,
		Notify:    notify,
	}
	encoded, err := record.Encode()
	if err != nil {
		return statelog.Decision{}, err
	}
	if len(encoded) > MaxCommitBytes {
		// REFUSED NAMING THE SIZE, never cut to fit: a record silently
		// trimmed is a row that cannot be rebuilt from it, which is the
		// one property the whole record format exists to have.
		return statelog.Decision{}, fmt.Errorf("tracker: the %s record for %s "+
			"is %d bytes and the design maximum is %d — a record is refused "+
			"rather than trimmed, because a trimmed one cannot rebuild its row",
			op, subject, len(encoded), MaxCommitBytes)
	}
	return statelog.Decision{Payload: encoded}, nil
}

// wire is the framework's subject for one of this domain's.
func wire(s Subject) statelog.Subject {
	return statelog.Subject{Kind: string(s.Kind), ID: s.ID}
}

// published is [Writer.publish] with the domain's own result around it, for
// the paths whose only extra fact is the body warning — which is every
// single-append path.
func (w *Writer) published(ctx context.Context, req statelog.Request) (WriteResult, error) {
	result, err := w.publish(ctx, req)
	return WriteResult{Result: result}, err
}

// publish runs one request and translates the framework's refusals into the
// caller's own vocabulary.
//
// THE THREE OUTCOMES TRAVEL UNCHANGED. What this adds is the ONE thing the
// framework cannot know: which of its refusals a caller can act on, and what
// to do about it — a refused create is a name already taken, a refused write
// on a removed task is a restore, and an unknown outcome is a resolution
// rather than a retry.
func (w *Writer) publish(ctx context.Context, req statelog.Request) (statelog.Result, error) {
	if w.refusal != nil {
		// THE GUARD IS HERE AND NOT IN [Writer.published], because two
		// sequences publish through this directly — a guard on the outer
		// helper would cover ten write paths and leave two open, which
		// is the shape of every hole this package has had.
		return statelog.Result{}, w.refusal
	}
	// THE CALLER'S OWN HIGH-WATER MARK, and this is the one place it is
	// set: the gesture that made the earlier write hands it on through
	// [Writer.After], so nothing here has to remember what this writer has
	// published. Zero — every write a surface makes on its own — waits for
	// nothing.
	req.Session = w.after
	result, err := w.publisher.Publish(ctx, req)
	switch {
	case err == nil:
		return result, nil
	case errors.Is(err, statelog.ErrExists):
		return result, fmt.Errorf("tracker: %s already exists: %w",
			req.Subject.ID, err)
	}
	return result, err
}

// MoveTask drops one task between two neighbours, re-spreading inline when the
// gap has run out of room.
//
// # The gesture, and the one place a rank key can grow without bound
//
// A drag mints a key strictly between the two neighbours it landed between.
// Repeatedly dropping at the same spot subdivides the same gap, and the key
// grows one symbol per halving — so a board somebody keeps re-ordering at one
// point reaches [RankRenormaliseAt] in a few hundred drags. That is the
// designed rate, not a fault: what makes it harmless is that the mint is
// REPLACED by a re-spread rather than allowed to keep growing.
//
// A re-spread rewrites a window of neighbours to evenly spaced short keys and
// carries them in the SAME record as the drag, so the order is never observed
// half-spread. The window is derived from the gap rather than fixed —
// [RespreadWindow] widens until the keys it would produce are short — and it
// stops at [RankRespreadInline]. Past that the drag STILL SUCCEEDS with its
// long key, and the applier flags the project for the duty's paced walk on
// every node: a drag refused because a board is crowded is a person told their
// own board is broken.
func (w *Writer) MoveTask(ctx context.Context, opID, project, taskID string,
	after, before Rank) (WriteResult, error) {

	switch {
	case project == "":
		return WriteResult{}, fmt.Errorf("tracker: a move names no project")
	case taskID == "":
		return WriteResult{}, fmt.Errorf("tracker: a move names no task")
	}
	placements, err := w.placeBetween(ctx, project, taskID, after, before)
	if err != nil {
		return WriteResult{}, err
	}
	return w.MoveTasks(ctx, opID, project, placements)
}

// placeBetween mints the drag's own key and, when it is too long, the window
// of neighbours that shortens it.
func (w *Writer) placeBetween(ctx context.Context, project, taskID string,
	after, before Rank) ([]Placement, error) {

	key, err := KeyBetween(after, before)
	if err != nil {
		return nil, fmt.Errorf("tracker: mint a key between %q and %q: %w",
			after, before, err)
	}
	if len(key) <= RankRenormaliseAt {
		return []Placement{{Task: taskID, Rank: key}}, nil
	}
	window, err := RespreadWindow(after, before)
	if err != nil {
		return nil, err
	}
	if w.db == nil {
		// NO STORE, NO RE-SPREAD, AND THE DRAG STILL LANDS. The applier
		// flags the project from the key's own length, so the repair is
		// scheduled by the record rather than by whoever wrote it.
		return []Placement{{Task: taskID, Rank: key}}, nil
	}

	var neighbours []Placement
	if err := w.db.Replicated().Read(ctx, func(tx *sql.Tx) error { //nolint:govet // shadow: scoped to this block; see .golangci.yml (trailing: covers this line only, not the closure)
		//nolint:govet // shadow: `x, err := f()` declares x too; see .golangci.yml
		rows, err := tx.QueryContext(ctx, `
			SELECT id, rank FROM tracker_tasks
			WHERE project_key = ? AND rank > ? AND rank < ?
			ORDER BY rank, id LIMIT ?`,
			project, string(after), string(before), window)
		if err != nil {
			return fmt.Errorf("tracker: read the re-spread window in %s: %w",
				project, err)
		}
		defer rows.Close()
		for rows.Next() {
			var p Placement
			var rank string
			if err := rows.Scan(&p.Task, &rank); err != nil {
				return fmt.Errorf("tracker: read a re-spread neighbour: %w", err)
			}
			p.Rank = Rank(rank)
			neighbours = append(neighbours, p)
		}
		return rows.Err()
	}); err != nil {
		return nil, err
	}

	// THE MOVED TASK TAKES ITS PLACE IN THE WINDOW rather than being
	// appended to it: the whole point of the re-spread is that the record
	// states one consistent order, and a drag written beside a window it
	// is not part of would land between two keys the same record has just
	// moved.
	fresh, err := KeysBetween(after, before, len(neighbours)+1)
	if err != nil {
		return nil, fmt.Errorf("tracker: re-spread %d neighbours between %q "+
			"and %q: %w", len(neighbours), after, before, err)
	}
	placements := make([]Placement, 0, len(fresh))
	placements = append(placements, Placement{Task: taskID, Rank: fresh[0]})
	for i, neighbour := range neighbours {
		placements = append(placements, Placement{
			Task: neighbour.Task, Rank: fresh[i+1],
		})
	}
	return placements, nil
}

// count records one of this package's own counters.
//
// NIL RECORDS NOTHING, deliberately: the instruments belong to the engine,
// which builds one recorder for the whole process, and a writer in a test has
// no reason to carry a second one whose numbers nobody reads.
func (w *Writer) count(name string, attrs metrics.Attrs) {
	if w.metrics != nil {
		w.metrics.Add(name, 1, attrs)
	}
}
