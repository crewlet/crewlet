package tracker

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
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

// ErrPurged is a write refused because the task it would file something under,
// or fold something into, was PURGED.
//
// IT WRAPS [ErrNoTask], because to every surface that answers "not found" a
// purged task is exactly that. And it is a sentinel of its own because one
// caller has to tell it from every other refusal: a merge that meets it
// part-way has met the one refusal that can never clear, so it gives the merge
// up rather than leaving a marker for a repair that would meet it again
// ([Writer.MergeDuplicates]).
var ErrPurged = fmt.Errorf("%w: it was purged", ErrNoTask)

// refusePurged refuses a write that names a PURGED task as the place to put
// something — a parent, or a merge's target — read inside the decide's own
// snapshot: the one read the record is decided from, so a purge a caller's
// earlier read missed is seen here once this node has applied it.
//
// # Why a refusal and not a guess
//
// Every alternative writes something nobody asked for. A root drops the tree
// the task was meant to join; the purged id is a parent no row holds, which no
// reader can tell from a parent held on another node; and the purged task's own
// parent is a place the caller did not name. The caller asked for a place that
// no longer exists, and while the answer can still be a refusal, the honest one
// says so.
//
// It cannot close the race with a purge this node has not applied yet — the
// two are writes to different subjects — and a record that crossed one is
// already committed, so the applier cannot refuse it: [placedParent] is the
// place it lands instead.
func refusePurged(ctx context.Context, tx *sql.Tx, id, role string) error {
	var key, by string
	var at int64
	err := tx.QueryRowContext(ctx,
		`SELECT task_key, by, at FROM tracker_deletions WHERE task_id = ?`,
		id).Scan(&key, &by, &at)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil
	case err != nil:
		return fmt.Errorf("tracker: read whether %s %s was purged: %w", role, id, err)
	}
	return fmt.Errorf("%w: the %s named, %s (%s), was purged by %s at %s, and "+
		"a purged task holds nothing — name a task that still exists",
		ErrPurged, role, key, id, by, store.DecodeTime(at).Format(time.RFC3339))
}

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
type Writer struct {
	publisher *statelog.Publisher

	// db is the replicated estate, read by the sequences that need to see
	// a subtree BEFORE their first append. It is never the write path's
	// own snapshot — that is the framework's, taken per append — and
	// nothing decided here is paired with an expectation.
	db *store.DB

	// claims is the coordination a walking sequence takes its claim from,
	// and nodeID is who holds it. Both may be nil or empty on a writer
	// that only makes single-append writes; a sequence that needs one
	// refuses by name rather than running without it.
	claims Claims
	nodeID string

	// local is this node's half of every such claim, shared by every clone
	// of this writer — see [localClaims].
	local *localClaims

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

	// metrics is where the counters this package owns are recorded. Nil
	// records nothing, which is what a writer built for a test gets: the
	// instruments are the engine's, and a nil check here is cheaper than
	// a second recorder nobody reads.
	metrics *metrics.Recorder

	// Drain is the applier's measured drain on this node in RECORDS a
	// second, which a bulk edit's projection divides its record count by
	// — and through that projection, the lease the bulk holds and the
	// retry hint a bulk refused behind it reads. Nil means unmeasured,
	// which the projection reads as its pessimistic floor rather than as
	// infinity.
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

	// merge is what the next [Writer.UpdateTask] has to find still true
	// when it is one append of a merge — set by [Writer.mergingInto],
	// [Writer.whileMerging] and [Writer.movingOutOf]. The zero value asks
	// nothing, which is every write that is not a merge's.
	merge mergeStep
}

// mergeStep is what one append of a merge is decided under, each part read
// inside that append's own decide snapshot.
//
// # Why the merge reads these again at every append
//
// A merge reads its duplicate, then the duplicate's subtasks, and then makes
// one append per step — and every fact those reads found is one another writer
// can change before the append it feeds. Decided from the earlier read, a step
// writes over that change: a subtask somebody moved elsewhere in between is
// moved back onto the target, and a merge a peer already closed is closed a
// second time. Read in the decide, a fact that moved refuses the step, and a
// snapshot too old to have seen it is refused by the broker and decided again
// from rows that have.
//
// A PRECONDITION ON THE WRITE rather than a field of the patch, because the
// patch is the record and every field of it is a change a replay applies —
// these change nothing, and a record has nothing of them to replay. They ride
// the writer for the reason [Writer.After] does: they are about this one call.
type mergeStep struct {
	// into is the task being folded into, refused when it has been purged
	// or is not on this node ([mergeTargetHeld]).
	into string

	// marked is the task being folded still mid-merge, refused as
	// [errMergeOver] when its marker has been cleared.
	marked bool

	// from is the task being folded, when this append moves one of its
	// subtasks: refused as [errLeftTheMerge] when the task is no longer a
	// live subtask of it.
	from string
}

// errMergeOver refuses one of a merge's appends because the merge has already
// ended: its marker was cleared by a close or a give-up that landed first.
var errMergeOver = errors.New("tracker: the merge had already ended")

// errLeftTheMerge refuses the move of a subtask a merge no longer moves: one
// somebody re-parented elsewhere, or put in the trash, after the merge read it.
var errLeftTheMerge = errors.New("tracker: the task is no longer a live subtask " +
	"of the one being merged")

// subtask refuses the move of a task that is no longer one of the subtasks the
// merge moves.
//
// A SUBTASK IN THE TRASH IS NOT ONE. A tombstone is a freeze — this writer
// refuses every patch on a removed task — so its move would be refused on
// every attempt: a merge that could never finish, and a sweep failing on it at
// every tick. It stays where it was removed from, and comes back there if it
// is ever restored.
func (s mergeStep) subtask(current Task) error {
	if s.from == "" {
		return nil
	}
	if current.Removed != nil || current.Parent == nil || *current.Parent != s.from {
		return fmt.Errorf("%w: %s, of %s", errLeftTheMerge, current.ID, s.from)
	}
	return nil
}

// holds refuses a merge's append whose merge has ended or whose target is gone.
func (s mergeStep) holds(ctx context.Context, tx *sql.Tx, current Task) error {
	if s.marked && !current.Merging {
		return fmt.Errorf("%w: %s is no longer mid-merge", errMergeOver, current.ID)
	}
	if s.into != "" {
		return mergeTargetHeld(ctx, tx, current.ID, s.into)
	}
	return nil
}

// mergingInto is this writer making one append of a merge into `into`, which
// its decide reads as still there ([mergeStep.into]).
func (w *Writer) mergingInto(into string) *Writer {
	if w == nil {
		return nil
	}
	clone := *w
	clone.merge.into = into
	return &clone
}

// whileMerging is this writer making one append of a merge that its decide
// reads as still running ([mergeStep.marked]).
func (w *Writer) whileMerging() *Writer {
	if w == nil {
		return nil
	}
	clone := *w
	clone.merge.marked = true
	return &clone
}

// movingOutOf is this writer moving one of `duplicate`'s subtasks for its
// merge, which its decide reads as still a live subtask of it
// ([mergeStep.from]).
func (w *Writer) movingOutOf(duplicate string) *Writer {
	if w == nil {
		return nil
	}
	clone := *w
	clone.merge.from = duplicate
	return &clone
}

// WriterDeps is everything a writer needs that it does not own.
type WriterDeps struct {
	Publisher *statelog.Publisher
	DB        *store.DB
	Claims    Claims
	NodeID    string

	// World is the chart seam the custom-field coercion needs for the one
	// field type whose value is a colleague — see [Writer.World].
	World     FieldWorld
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
	return &clone
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
		local:   newLocalClaims(),
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
	// A COLLECTION THE PATCH TOUCHES IS CARRIED WHOLE — that is the record's
	// own rule — so a checklists patch is the complete new tree and this is
	// the whole of what the task will hold.
	if patch.Checklists != nil {
		if err := checkChecklistCaps(id, *patch.Checklists); err != nil {
			return WriteResult{}, err
		}
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
	var fieldWarnings []string
	result, err := w.published(ctx, statelog.Request{
		Subject:  wire(subject),
		Scope:    scope.Resolve(subject),
		OpID:     opID,
		MintedAt: at,
		Pattern:  statelog.PatternArbitrated,
		Decide: func(tx *sql.Tx) (statelog.Decision, error) {
			//nolint:govet // shadow: `x, err := f()` declares x too; see .golangci.yml
			current, held, err := readTask(ctx, tx, id)
			if err != nil {
				return statelog.Decision{}, err
			}
			if !held {
				return statelog.Decision{}, fmt.Errorf("tracker: task %s is not "+
					"on this node: %w", id, statelog.ErrUnavailable)
			}
			// BEFORE THE FREEZE, because a merge's move of a subtask in the
			// trash is not a write to refuse but one the merge does not
			// make — see [mergeStep.subtask].
			if err = w.merge.subtask(current); err != nil {
				return statelog.Decision{}, err
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
			if ifMatch != 0 && current.Version != ifMatch {
				return statelog.Decision{}, fmt.Errorf("%w: task %s is at "+
					"version %d and the edit was conditioned on %d — re-read "+
					"it and decide again rather than re-sending this patch",
					ErrStaleVersion, id, current.Version, ifMatch)
			}
			// A NEW PARENT AND A MERGE'S TARGET ARE READ HERE, in the
			// snapshot this record is decided from, and nowhere earlier: a
			// purge that lands after a caller's own read and before this
			// one is a purge this refuses — see [refusePurged].
			if patch.Parent != nil && *patch.Parent != "" {
				if err = refusePurged(ctx, tx, *patch.Parent, "parent"); err != nil {
					return statelog.Decision{}, err
				}
			}
			if err = w.merge.holds(ctx, tx, current); err != nil {
				return statelog.Decision{}, err
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
			// A GESTURE ON AN EDGE COLLECTION REWRITES IT WHOLE, so it
			// starts from the collection without its edges to purged
			// tasks: the document still lists them and no row does
			// ([withoutPurged]), and resolved from the document the cap
			// would count an edge nobody can see and the record would carry
			// it forward. Resolved from this, the record carries the
			// collection clean and the document heals.
			if charged.Relate != nil || charged.Depend != nil {
				if current, err = withoutPurged(ctx, tx, current,
					w.maxVariables(), everyPurge); err != nil {
					return statelog.Decision{}, err
				}
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
			//
			// AGAINST `project`, which is the container the enumeration
			// filed every dependent under — so the covering container
			// it collapses to past [MaxScopeTerms] satisfies this check
			// exactly where it satisfies the apply.
			//nolint:govet // shadow: scoped to this block; see .golangci.yml
			if err := scope.covers(project, current.Dependents); err != nil {
				return statelog.Decision{}, err
			}
			decision, err := w.decide(subject, OpPatch, kind, scope, opID,
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
	return result, err
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

// ErrOrderMoved is a reposition refused because the project's manual order
// changed after the read its placements were decided from.
//
// A CONFLICT, and it wraps [statelog.ErrConflict] for that reason: what the
// caller does next is read the order again and decide again, which is what
// every other lost race in this package asks for.
var ErrOrderMoved = fmt.Errorf("%w: the project's manual order moved after "+
	"the read these placements were decided from", statelog.ErrConflict)

// MoveTasks repositions tasks in a project's manual order, AS DECIDED FROM ONE
// READ OF IT.
//
// # Why the subject is the ORDER and not any of the tasks
//
// The object a drag mutates is the project's ORDER — a total order over its
// tasks that no single task owns and no single task's version can protect. Two
// people dragging two different cards in one project are editing the same
// object, and arbitrating on either card would let both writes land and leave
// the order neither of them intended.
//
// The scope is the project CONTAINER rather than an enumeration of the tasks
// it places: every placement is a task in that project, so the container is
// the one term that covers any set of them.
//
// # Why it takes the order's version, and every caller states one
//
// These placements are keys somebody computed from a read of the order — the
// re-spread walk from its plan, the duplicate repair from the keys it found
// shared — and that read is not this write's snapshot. The broker arbitrates
// on the order's own subject, but the expectation it checks is formed inside
// THIS snapshot, so on its own it would accept keys decided against an order
// that has since moved: the pairing of an old decision with a new expectation
// that the write authority exists to rule out. `against` closes that gap. It
// is the version [OrderVersion] read beside the keys — the position of the
// last rank-order record applied, zero for an order nothing has placed yet —
// and the decide refuses with [ErrOrderMoved] unless the order is still
// there. The broker's expectation, formed in the same snapshot as that check,
// then covers the rest of the way to the append.
//
// The VERSION rather than the arbitration anchor, because it is the question
// the decision depends on: whether the ROWS the keys were computed from have
// moved. A gated record advances the anchor and writes nothing, and a check
// against the anchor would send a caller to re-read an order that has not
// changed.
//
// AT MOST [MaxBulkTasks] PLACEMENTS, which is one bulk gesture's bound and
// what every caller in the tree stays inside: the re-spread walk and the
// duplicate repair publish [WalkBatch] at a time.
func (w *Writer) MoveTasks(ctx context.Context, opID, project string,
	against int64, placements []Placement) (WriteResult, error) {

	switch {
	case len(placements) == 0:
		return WriteResult{}, fmt.Errorf("tracker: a move places no task")
	case len(placements) > MaxBulkTasks:
		return WriteResult{}, fmt.Errorf("tracker: a move carries %d "+
			"placements and one record carries at most %d — split it into "+
			"records of that many", len(placements), MaxBulkTasks)
	}
	return w.reposition(ctx, opID, project, func(tx *sql.Tx) ([]Placement, error) {
		version, err := OrderVersion(ctx, tx, project)
		if err != nil {
			return nil, err
		}
		if version != against {
			return nil, fmt.Errorf("%w: %s's order is at version %d and these "+
				"placements were decided at %d", ErrOrderMoved, project,
				version, against)
		}
		return placements, nil
	})
}

// OrderVersion is a project's manual order as of the transaction it reads in:
// the position of the last rank-order record applied to it, and zero for an
// order no placement has ever been applied to.
//
// It is the value [Writer.MoveTasks] checks, and a caller computing keys reads
// it in the SAME transaction as the keys — a version read apart from them
// could be newer than the order they were computed from.
func OrderVersion(ctx context.Context, tx *sql.Tx, project string) (int64, error) {
	var version int64
	err := tx.QueryRowContext(ctx,
		`SELECT version FROM tracker_rank_orders WHERE project_key = ?`,
		project).Scan(&version)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return 0, nil
	case err != nil:
		return 0, fmt.Errorf("tracker: read %s's order version: %w", project, err)
	}
	return version, nil
}

// reposition publishes one rank-order record whose placements are decided
// inside the write's own snapshot.
//
// ONE PATH FOR THE TWO WAYS a placement is decided — from an earlier read and
// checked here ([Writer.MoveTasks]), or from the rows here ([Writer.MoveTask])
// — so the refusals every key is held to are written once.
func (w *Writer) reposition(ctx context.Context, opID, project string,
	decide func(*sql.Tx) ([]Placement, error)) (WriteResult, error) {

	if project == "" {
		return WriteResult{}, fmt.Errorf("tracker: a move names no project")
	}
	subject := RankOrderSubject(project)
	scope := ScopeSet{Terms: []ScopeTerm{{Kind: TermContainer, ID: project}}}
	at := w.Now()

	return w.published(ctx, statelog.Request{
		Subject:  wire(subject),
		Scope:    scope.Resolve(subject),
		OpID:     opID,
		MintedAt: at,
		Pattern:  statelog.PatternArbitrated,
		Decide: func(tx *sql.Tx) (statelog.Decision, error) {
			placements, err := decide(tx)
			if err != nil {
				return statelog.Decision{}, err
			}
			for _, placement := range placements {
				// THE LENGTH FIRST AND ON ITS OWN, because it is the one
				// refusal a well-formed request reaches — a drag into a
				// gap subdivided past [RankRefuseAt] — and whoever made
				// it is owed the remedy rather than a verdict on the key.
				if len(placement.Rank) > RankRefuseAt {
					return statelog.Decision{}, fmt.Errorf("tracker: placing %s "+
						"needs a rank key of %d characters and a key holds at "+
						"most %d (tracker.RankRefuseAt) — its gap has been "+
						"dropped into so often that its keys outgrew the "+
						"schema; the re-spread walk gives every task in %s a "+
						"short key again, and a move between the same two "+
						"cards, re-read after it has run, fits: %w",
						placement.Task, len(placement.Rank), RankRefuseAt,
						project, ErrRankTooLong)
				}
				if !placement.Rank.Valid() {
					return statelog.Decision{}, fmt.Errorf("tracker: %q is not "+
						"a well-formed rank key", placement.Rank)
				}
			}
			// A RANK MOVE CARRIES NO NOTIFICATION. A reposition is not
			// history: it changes where a card sits and nothing about
			// what the work is, so waking anybody for it would make a
			// board's own drag a source of inbox traffic.
			return w.decide(subject, OpPatch, "", scope, opID, RankOrder{
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
	// one; a view and a goal choose theirs. Accepting one where it means
	// nothing would let a caller file a person's record under a project.
	if container != "" && !subject.Kind.HomedInAProject() {
		return WriteResult{}, fmt.Errorf("tracker: a %s names container %q, "+
			"and its own path is derived from its subject — a container here "+
			"would file its deferral where no probe for it looks",
			subject.Kind, container)
	}
	scope := ScopeSet{Subject: true, Container: container}
	at := w.Now()
	return w.published(ctx, statelog.Request{
		Subject:  wire(subject),
		Scope:    scope.Resolve(subject),
		OpID:     opID,
		MintedAt: at,
		Pattern:  statelog.PatternArbitrated,
		Decide: func(*sql.Tx) (statelog.Decision, error) {
			return w.decide(subject, OpPatch, kind, scope, opID, document, notify, at)
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
		Subject:  wire(subject),
		Scope:    scope.Resolve(subject),
		OpID:     opID,
		MintedAt: at,
		Pattern:  statelog.PatternAdditive,
		Decide: func(*sql.Tx) (statelog.Decision, error) {
			return w.decide(subject, OpTurn, "", scope, opID, payload, nil, at)
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
func (w *Writer) decide(subject Subject, op OpKind, kind ChangeKind,
	scope ScopeSet, opID string, payload any, notify *Notify,
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
			V: recordVersionOf(subject, op), OpID: opID, Subject: subject, Op: op,
			CreatedAt: at, Scope: scope,
		},
		Kind:       kind,
		Mutation:   body,
		Actor:      w.Actor,
		ActorKind:  w.ActorKind,
		OperatorID: w.OperatorID,
		TurnID:     w.TurnID,
		Chain:      w.Chain,
		Notify:     notify,
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
	return statelog.Decision{
		Payload:  encoded,
		Envelope: envelopeOf(record, scope, subject),
	}, nil
}

// envelopeOf is the framework's own view of a record.
//
// IT RESOLVES THE SAME SCOPE THE RECORD CARRIES, through the same one-argument
// call every other side uses. The publisher probes the deferral index with the
// request's scope and the applier files a deferral under the envelope's, so a
// second spelling here is a record filed where its own writer never looks.
func envelopeOf(record MutationRecord, scope ScopeSet, subject Subject) statelog.Envelope {
	return statelog.Envelope{
		V:       record.V,
		Kind:    string(subject.Kind),
		Subject: wire(subject),
		Op:      string(record.Op),
		OpID:    record.OpID,
		Scope:   scope.Resolve(subject),
	}
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

// MoveTask drops one task between two neighbours, and it moves that task and
// nothing else.
//
// # The gesture, and the one place a rank key can grow without bound
//
// A drag mints a key strictly between the two neighbours it landed between.
// Repeatedly dropping at the same spot subdivides the same gap, and a symbol
// holds sixty-two positions, so the key grows by one for about every six
// drags — a board somebody keeps re-ordering at one point reaches
// [RankRenormaliseAt] in a few hundred. That is the designed rate, not a
// fault: what makes it harmless is that a key past the threshold is REPLACED
// by the duty's re-spread walk rather than allowed to keep growing. The drag
// still lands with its long key — a drag refused because a board is crowded
// is a person told their own board is broken — and the applier flags the
// project from that key's own length, so the repair is scheduled by the
// record rather than by whoever wrote it. The one length it is refused at is
// [RankRefuseAt], the schema's own ceiling, and that refusal wraps
// [ErrRankTooLong] and says the walk is what makes room again.
//
// # Why the drag re-keys none of its neighbours
//
// A key is long because the gap it was minted in is narrow, and every key
// inside a narrow gap is long: re-keying the tasks inside it cannot bring them
// back under the threshold. And a gap the board shows as empty may not be — a
// card a filter or a column hides sits in it too — so a re-key of what a
// bounded read found there would leave the rest at keys the fresh ones can
// overtake: a reorder nobody asked for and nothing reports. Shortening keys
// takes a fresh integer position, which only the walk takes ([PlanRespread]).
//
// # The neighbours are TASKS, and their keys are read inside the write
//
// `after` and `before` name the cards the drop landed between — either may be
// empty for a drop at an end, not both — and their keys are read in the
// write's own snapshot rather than handed in. A key the caller read earlier is
// a decision formed outside the snapshot: a re-spread batch that moved both
// neighbours in between leaves those keys naming a place in an order that no
// longer exists, and the drop would land there, accepted, somewhere nobody
// dropped it. Read here, the key is minted from the order the broker is about
// to arbitrate against, and a batch that lands first sends this write round
// again to read the new one.
//
// An OPEN END is bounded by the nearest key in the project, removed tasks
// included — [rankAbove] says why a key past the last card must not come from
// the create lattice, and a key before the first is bounded below the same
// way — so a drop at either end of what the caller saw lands next to that
// card rather than past a card the board was not showing.
func (w *Writer) MoveTask(ctx context.Context, opID, project, taskID string,
	after, before string) (WriteResult, error) {

	switch {
	case taskID == "":
		return WriteResult{}, fmt.Errorf("tracker: a move names no task")
	case after == "" && before == "":
		return WriteResult{}, fmt.Errorf("tracker: the drop of %s names neither "+
			"the card it lands after nor the one it lands before, so it names "+
			"no place in the order", taskID)
	case after == taskID || before == taskID:
		return WriteResult{}, fmt.Errorf("tracker: %s cannot land beside "+
			"itself — name the cards on either side of where it goes", taskID)
	}
	return w.reposition(ctx, opID, project, func(tx *sql.Tx) ([]Placement, error) {
		if _, err := cardRank(ctx, tx, project, taskID); err != nil {
			return nil, err
		}
		lo, hi, err := dropBetween(ctx, tx, project, after, before)
		if err != nil {
			return nil, err
		}
		key, err := KeyBetween(lo, hi)
		if err != nil {
			return nil, fmt.Errorf("tracker: mint a key between %q and %q: %w",
				lo, hi, err)
		}
		return []Placement{{Task: taskID, Rank: key}}, nil
	})
}

// dropBetween is the two keys a drop lands between, read in the write's own
// snapshot — see [Writer.MoveTask].
func dropBetween(ctx context.Context, tx *sql.Tx, project, after, before string) (
	Rank, Rank, error) {

	var lo, hi Rank
	var err error
	if after != "" {
		if lo, err = cardRank(ctx, tx, project, after); err != nil {
			return "", "", err
		}
	}
	if before != "" {
		if hi, err = cardRank(ctx, tx, project, before); err != nil {
			return "", "", err
		}
	}
	switch {
	case after != "" && before != "" && string(lo) > string(hi):
		return "", "", fmt.Errorf("%w: %s is no longer above %s in %s's order, "+
			"so the gap this drop names is not there", ErrOrderMoved, after,
			before, project)
	case after != "" && before != "" && lo == hi:
		return "", "", fmt.Errorf("tracker: %s and %s share the key %q, so there "+
			"is no gap between them — the duplicate repair gives one of them a "+
			"fresh key, and the same drop lands once it has", after, before, lo)
	case before == "":
		hi, err = rankAbove(ctx, tx, project, lo)
	case after == "":
		lo, err = rankBelow(ctx, tx, project, hi)
	}
	return lo, hi, err
}

// cardRank is one live task's key in a project, or a refusal naming why a drop
// cannot use it.
func cardRank(ctx context.Context, tx *sql.Tx, project, id string) (Rank, error) {
	var rank string
	err := tx.QueryRowContext(ctx, `
		SELECT rank FROM tracker_tasks
		WHERE id = ? AND project_key = ? AND removed_at IS NULL`,
		id, project).Scan(&rank)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", fmt.Errorf("tracker: %s is not a live task in %s — it was "+
			"moved, removed or never there — so a drop cannot place it or land "+
			"beside it; read the board again", id, project)
	case err != nil:
		return "", fmt.Errorf("tracker: read %s's key in %s: %w", id, project, err)
	}
	return Rank(rank), nil
}

// maxVariables is the replicated estate's parameter limit, which
// [withoutPurged] chunks its lookup to; zero, one id per statement, when this
// writer has no estate.
func (w *Writer) maxVariables() int {
	if w.db == nil {
		return 0
	}
	return w.db.Caps().MaxVariables
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
