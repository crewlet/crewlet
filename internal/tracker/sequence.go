package tracker

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// The FIVE sequences that stay genuinely cross-object — a create (1), a
// cross-project move (7), a merge (13) and a bulk edit (26) here, and a
// dependency in depend.go — and the one property that makes them tolerable.
//
// # Why a multi-append sequence is not a transaction, and must not pretend
//
// Every single-object write in this domain is ONE append: snapshot, decide
// inside it, commit at the anchor the same transaction read. There is no
// "between" and therefore no crash residue. A handful of gestures cannot
// reach that shape, because the object they mint from and the object they
// write are different objects with different arbitration — a key comes from a
// project's counter and lands on a task.
//
// So each states FOUR things and this file implements exactly them: the
// ORDER of its appends, the DURABLE CLAIM that stops two nodes running it at
// once, the CRASH RESIDUE the order was chosen to leave, and the REPAIRER
// that clears it — or the argument for why none is needed.
//
// The order is never arbitrary. A create mints its number first and its task
// second, so a crash leaves a numbering GAP rather than two tasks sharing a
// key, because a key is what people paste into chat. A cross-project move
// writes the alias for a key BEFORE it re-keys the task, so a key somebody
// pasted keeps resolving from the moment it stops being current.
//
// # Every step is a step ON THE LOG
//
// Nothing here writes a row. Each step publishes a record and lets the
// framework arbitrate it, so a sequence interrupted anywhere leaves only
// records — no half-written state, nothing to roll back, and a re-run that
// re-derives the same identities is refused rather than duplicated.

const (
	// MaxBulkTasks is how many distinct task subjects one bulk gesture
	// may carry.
	//
	// THE SAME 64 as every other fan-out in this design — a merge batch,
	// the scope term cap — because they are the same quantity: how much
	// work one durable claim covers before it has to heartbeat.
	MaxBulkTasks = 64

	// MaxBulkBytes is the pre-flight ceiling over the commits a bulk
	// patch will write, checked BEFORE the first append.
	//
	// A bulk gesture is the one write whose refusal has to come before any
	// of it lands: a partial batch is reported and re-run, and a batch
	// refused halfway is a caller told "failed" about tasks that changed.
	MaxBulkBytes = 8 << 20

	// WalkBatch is one batch of a paced walk — a merge's children, a
	// re-spread's placements — and of every duty selection that finishes
	// one. Not a move's: that walk is one append per descendant of a
	// subtree it read whole, bounded by [MaxDescendants].
	WalkBatch = 64

	// ClaimTTL is how long the durable claim a walking sequence holds
	// survives unrenewed. FOUR HEARTBEATS, so three consecutive misses are
	// survivable and the fourth hands the walk to the duty.
	//
	// THE LEASE'S OWN EXPIRY IS THE ONLY STALENESS THERE IS. The duty
	// finishes a walk only under the walk's claim, so "abandoned" means
	// "a claim the duty could take", and nothing else decides it: a second
	// figure — the thirty-second ClaimStale this block used to declare,
	// which nothing ever read — would be a second opinion about one event,
	// and a coordination lease cannot be taken before it expires whatever
	// that figure said.
	ClaimTTL = 60 * time.Second

	// ClaimHeartbeat is how often the holder renews that claim — a quarter
	// of [ClaimTTL], which is the arithmetic its own comment rests on.
	ClaimHeartbeat = 15 * time.Second
)

// Claims is the coordination a multi-append sequence takes its claim from.
//
// FOUR METHODS OF [coord.Backend], named by the consumer as this package uses
// them. Three-valued by construction and that is the whole reason it is not a
// bool: a lease is held, a (nil, nil) says a peer holds it, and an error says
// NOTHING IS KNOWN — which is what lets the bulk admission fail OPEN over the
// third while a cross-project move fails closed over it. Collapsed to two
// values, one of those two behaviours would have to be wrong.
type Claims interface {
	TryAcquire(ctx context.Context, resource string, opts coord.AcquireOptions) (*coord.Lease, error)
	Renew(ctx context.Context, resource, owner string, epoch int64, ttl time.Duration) (bool, error)
	Release(ctx context.Context, resource, owner string, epoch int64) (bool, error)
	Get(ctx context.Context, resource string) (*coord.Lease, error)
}

// The claim names. One function each rather than a format string at the call
// site, because a claim spelled two ways is two claims and admits exactly the
// concurrency it was taken to exclude.
//
// Built through [coord.Class] like every other lease, because these land in
// the SAME bucket as the fleet's own and a bucket with two naming conventions
// in it has two grammars to keep right. The class is the key's leading
// SUBJECT TOKEN, so each of these is filterable on its own.
const (
	classBulk  coord.Class = "bulk"
	classMove  coord.Class = "move"
	classMerge coord.Class = "merge"
)

func bulkClaim(domain string) string { return classBulk.Resource(domain) }
func moveClaim(task string) string   { return classMove.Resource(task) }
func mergeClaim(task string) string  { return classMerge.Resource(task) }

// stepID derives one append's operation id from the gesture's own.
//
// STABLE AND DISTINCT, and both halves matter. The operation ledger dedupes
// one RECORD, and a sequence publishes several — so a shared id would make the
// second append's resolution read the first's ledger row and report a write
// that never happened. A freshly minted id per attempt would defeat the ledger
// for exactly the lost-acknowledgement case it exists for, which is contract
// 2's own rule about why an operation id is minted once.
//
// THE STATE LOG'S OWN GRAMMAR ([statelog.StepOpID]), so a step carries the
// gesture's mint instant: the ledger's vouching reads it off the step's id,
// and a step spelled here in a shape that grammar did not recognise would be
// read as minted at the zero instant — answered `unknown` on any node whose
// ledger ever lost a row, to its sweep or to a snapshot from a donor that
// scrubbed it.
func stepID(opID, step string) string { return statelog.StepOpID(opID, step) }

// ErrStepUnresolved reports a walking sequence that stopped at a step whose
// outcome is UNKNOWN.
//
// NEITHER "DONE" NOR "NOT MADE": the steps before it landed, the step itself
// may or may not be on the log, and nothing after it was written. A caller
// told this re-runs the gesture under the SAME operation id, and every step
// answers for itself — see each sequence's "Re-running it".
var ErrStepUnresolved = errors.New("tracker: a step's outcome is unresolved")

// ErrStepUnvouched reports a walking sequence that stopped at a step whose
// outcome is unknown BECAUSE THIS NODE'S OPERATION LEDGER CANNOT VOUCH FOR IT
// ([statelog.Result.Unvouched]): the step's operation was minted before the
// instant the ledger may have lost rows from, and it holds no row for it.
//
// IT IS AN [ErrStepUnresolved] — errors.Is answers true for both — and the one
// kind of it the same operation id retried HERE never finishes: the row the
// step needs is the one the loss took, so every re-run on this node stops at
// the same step. What can finish it is another node, or one whose ledger lost
// nothing that far back. A caller told "call again" about it goes round a loop
// the answer already knew the end of.
var ErrStepUnvouched = fmt.Errorf("tracker: this node's operation ledger "+
	"cannot vouch for a step: %w", ErrStepUnresolved)

// resolved reads one step of a walking sequence the way the walk must: a step
// that failed stops it, and so does one whose outcome is UNKNOWN.
//
// AN UNKNOWN STEP IS NOT ONE THAT LANDED. [Writer.UpdateTask] answers unknown
// with a nil error — the record may be on the log and may not — and every walk
// checked only the error, so it carried on over a step it could not vouch for:
// descendants moved under a root whose own move might not have landed, a mark
// taken down over a child still keyed in the old project, a duplicate closed
// with a subtask still under it, and a caller told the whole gesture
// succeeded. Stopping leaves the walk's own mark up, which is what hands the
// remainder to the re-run or the duty rather than to nobody.
//
// AN UNKNOWN THIS NODE'S LEDGER CANNOT VOUCH FOR IS SAID SO ([ErrStepUnvouched]),
// because the re-run the ordinary unknown asks for is exactly what never
// finishes it here.
func resolved(step string, result WriteResult, err error) error {
	switch {
	case err != nil:
		return err
	case result.Outcome == statelog.OutcomeUnknown && result.Unvouched:
		return fmt.Errorf("tracker: whether %s landed cannot be told on this "+
			"node (operation %s): its operation ledger may have lost the record "+
			"of it, so the walk stopped there and a re-run here stops at the same "+
			"step; another node, or one whose ledger lost nothing that far back, "+
			"can finish it under the same operation id: %w",
			step, result.OpID, ErrStepUnvouched)
	case result.Outcome == statelog.OutcomeUnknown:
		return fmt.Errorf("tracker: whether %s landed is unknown (operation "+
			"%s), so the walk stopped there; re-run it under the same "+
			"operation id, which answers what landed and carries on from it: %w",
			step, result.OpID, ErrStepUnresolved)
	}
	return nil
}

// WriteResult is what a tracker write returns.
//
// It carries the framework's own three-valued outcome UNCHANGED — a write is
// applied, pending or unknown, and nothing here collapses them — plus the two
// things only this domain can say: what a mint produced, and what the caller
// should know but was not refused for.
type WriteResult struct {
	statelog.Result

	// Key and Rank are what a create or a move minted. Empty on every
	// other path.
	Key  string
	Rank Rank

	// KeyCollision says Key opens ANOTHER task — one that claimed it first,
	// which is what a counter restored beside newer work mints — so the task
	// this write landed is reached only by its id ([ItemAddress]). Read from
	// the key directory in the snapshot that decided the write, or that read
	// the task back ([keyHeldByAnother]); false wherever Key is empty.
	//
	// ON THE RECEIPT because the receipt is the first thing that names the
	// duplicate: a create after a counter restore is what PRODUCES one, and
	// a caller handed the key alone acts on the claimant with its next call.
	KeyCollision bool

	// Applied and Failed are a bulk gesture's per-task outcome. A bulk
	// write is NOT atomic and never was: Applied is every task whose change
	// is durable — applied here, or pending — and Failed says, per task,
	// why the rest are not, including a change whose outcome is unknown.
	// The caller re-runs the gesture under the same operation id.
	Applied []string
	Failed  map[string]string

	// Declared are the tags a write brought into existence, reported from
	// inside the snapshot that decided them rather than from a read before
	// it — so a caller is never told it created a word a colleague
	// declared a moment earlier.
	Declared []string

	// Warnings are what the caller should know and was not refused for —
	// a body carrying more task keys than the applier will resolve, a tag
	// that nearly matched an existing one. THE APPLY MUST NEVER DEPEND ON
	// ONE: every node applies the same record, and only this node saw the
	// warning.
	Warnings []string
}

// CreateTask mints a task. SEQUENCE 1.
//
//	Rs the project and its catalogues (refuse an archived project, check
//	the required fields) → A on the counter at its own anchor → A on the
//	task at expectation 0, carrying rank = intPart(n).
//
// CRASH RESIDUE: a numbering gap — the counter moved and the task never
// landed. REPAIRER: nobody, and it stays documented. ENG-7 exists, ENG-8 never
// did, ENG-9 is next, which is what Jira does too and is strictly better than
// the alternative: minting the item first and the number after would let two
// items share a key, and a key is what people paste into chat.
//
// # A retry under the same operation id
//
// THE TASK'S ID MUST BE A FUNCTION OF THE OPERATION — the builtin derives it
// from the operation id — because the id is the subject the second append
// arbitrates on: a retry that named a different task under the same
// operation would be a second task the broker has no reason to refuse.
//
// A retry whose counter step already landed cannot use the number: that step
// is answered from the ledger rather than decided ([statelog.Result.Collapsed]),
// so the number it would have taken is one the counter never recorded. So the
// retry asks for the task step the same way ([Writer.resumeCreate]): a task
// that landed is answered with its own key, and only a task that never did
// is filed on a FRESH mint — leaving the gap the crash residue already
// describes, and never a second task or a shared key.
//
// THE RANK COMES FROM THE COUNTER VALUE and nothing arbitrates it a second
// time. n is unique and increasing under the counter row's own arbitration, so
// no two creates collide and every create's key is strictly the new maximum —
// new tasks land at the tail in creation order. It cannot collide with a drag
// either, and the proof is about the VALUE rather than about any bound: a
// create's key is a pure integer at or above [RankOrigin], and every mint a
// move makes carries a fractional part or sits strictly below the origin.
func (w *Writer) CreateTask(ctx context.Context, opID string, task Task,
	notify *Notify) (WriteResult, error) {

	// ONE READING OF THE CHART for every name this write resolves and
	// every name it is worded with — see [Writer.pinned].
	w = w.pinned()
	switch {
	case task.ID == "":
		return WriteResult{}, fmt.Errorf("tracker: a create names no task id")
	case task.Project == "":
		return WriteResult{}, fmt.Errorf("tracker: task %s names no project "+
			"— a task's scope path sits under its project's, so one without a "+
			"project files its deferral where no project-scoped probe looks",
			task.ID)
	}
	// EVERY PERSON THE TASK NAMES, BY THEIR IDENTITY — its reporter, its
	// assignee, whoever watches it — so the rows are keyed where a rename
	// cannot move them. See people.go.
	task = identified(w.chart(), task)
	// BEFORE ANY READ, because a cap is a property of the value rather
	// than of the database: a title past its cap is refused identically
	// whichever node is asked and whatever the project holds, so paying
	// for a transaction to say so would buy nothing.
	if err := checkTextCaps(task.ID, &task.Title, &task.Body, nil); err != nil {
		return WriteResult{}, err
	}
	// BEFORE THE MINT, because the catalogue check runs inside it. The
	// other two defaults below cannot: they are applied after the key is
	// minted and nothing validates them.
	if task.Type == "" {
		task.Type = DefaultTaskType
	}
	// THE TAGS ARE NORMALISED HERE and checked against the project's set
	// inside the mint's own snapshot, for the same split: the spelling is
	// a property of the argument and the declaration is a property of the
	// project, and only the second needs a read.
	tags, err := normaliseTags(task.Project, task.Tags)
	if err != nil {
		return WriteResult{}, err
	}
	task.Tags = tags

	// WHAT THE SNAPSHOT SETTLED COMES BACK OUT OF IT, and the LAST run of
	// the closure is the one whose mint was accepted — so every field of
	// it is assigned rather than appended to, exactly as the update path
	// does with its own warnings.
	var settled settledCreate
	n, minted, err := w.mintKey(ctx, stepID(opID, "counter"), task.Project, 1,
		w.settleCreate(ctx, task, &settled))
	switch {
	case errors.Is(err, errMintLanded):
		return w.resumeCreate(ctx, opID, task, notify)
	case minted.Outcome == statelog.OutcomeUnknown && minted.Unvouched:
		return w.unvouchedCreate(ctx, opID, task, minted)
	case err != nil:
		return WriteResult{Result: minted}, err
	}
	return w.fileTask(ctx, opID, task, n, settled, notify)
}

// unvouchedCreate answers a create whose counter step this node's operation
// ledger cannot vouch for ([statelog.Result.Unvouched]).
//
// # Why it is not "not made"
//
// The operation was minted before the ledger may have lost rows — a seat
// re-running a turn whose trigger was queued before its node adopted a
// snapshot, a caller finishing a month-old `unknown` — so its first run may
// well have filed the task, on this node or another. An unknown counter was
// turned into ErrUnavailable, which every caller read as "the change was NOT
// made": the seat then rephrased and filed a duplicate under a new operation,
// or gave up on work that already existed.
//
// THE TASK'S OWN ROW CAN SAY, because its id is a function of the operation
// ([Task.ID] is derived from the op id): a row under it is this operation's
// task, filed by an earlier run, and is answered with its key exactly as a
// resumed create would. No row is not proof of absence — the first run may
// have filed it on a node this one has not caught up with — so it is answered
// `unknown`, still unvouched, with no error: neither made nor not made.
func (w *Writer) unvouchedCreate(ctx context.Context, opID string, task Task,
	minted statelog.Result) (WriteResult, error) {

	if w.db == nil {
		return WriteResult{Result: minted}, nil
	}
	var held bool
	if err := w.db.Replicated().Read(ctx, func(tx *sql.Tx) error {
		var err error
		_, held, err = readTask(ctx, tx, task.ID)
		return err
	}); err != nil {
		return WriteResult{Result: minted}, fmt.Errorf("tracker: the create of %s "+
			"cannot be vouched for here; read whether its task landed: %w",
			task.ID, err)
	}
	if !held {
		return WriteResult{Result: minted}, nil
	}
	return w.landedTask(ctx, statelog.Result{
		Outcome: statelog.OutcomeApplied, OpID: stepID(opID, "task"), Collapsed: true,
	}, task.ID)
}

// settleCreate is the read a create's mint runs inside its own snapshot: the
// refusals that need rows, and the values only rows can settle, assigned to
// settled on every run of the closure.
func (w *Writer) settleCreate(ctx context.Context, task Task,
	settled *settledCreate) func(*sql.Tx) error {

	return func(tx *sql.Tx) error {
		got, err := w.refuseCreate(ctx, tx, task)
		*settled = got
		return err
	}
}

// fileTask is a create's second append, on the key number n its mint took.
func (w *Writer) fileTask(ctx context.Context, opID string, task Task, n uint64,
	settled settledCreate, notify *Notify) (WriteResult, error) {

	if settled.fields != nil {
		task.Fields = settled.fields
	}
	task.FiledUnit, task.RoutingUnit = filedUnit(
		task.FiledUnit, task.RoutingUnit, settled.unit)
	rank, err := IntegerAt(n)
	if err != nil {
		return WriteResult{}, fmt.Errorf("tracker: derive %s-%d's rank from the "+
			"counter value it minted: %w", task.Project, n, err)
	}
	at := w.Now()
	task.Key = fmt.Sprintf("%s-%d", task.Project, n)
	task.Rank = rank
	task.CreatedAt, task.UpdatedAt = at, at
	if task.Status == "" {
		task.Status = StatusTodo
	}
	task.StatusGroup = task.Status.Group()
	if task.Priority == "" {
		task.Priority = PriorityNone
	}

	result, err := w.writeTask(ctx, stepID(opID, "task"), task, notify, at)
	if err == nil && result.Collapsed {
		// THE TASK STEP LANDED UNDER AN EARLIER COPY of this operation —
		// found once a fresh mint's append met it on the task's subject
		// — so the task is that copy's, under the number it took, and n
		// is the gap this retry left.
		return w.landedTask(ctx, result.Result, task.ID)
	}
	result.Key, result.Rank = task.Key, task.Rank
	result.Warnings = append(result.Warnings, settled.warnings...)
	return result, err
}

// resumeCreate finishes a create whose counter step already landed under its
// operation id — a retry of a create the first attempt got at least halfway
// through.
//
// THE TASK STEP IS ASKED FOR FIRST, AND ONLY ANSWERED. Its decision cannot be
// taken here — it needs the number the earlier copy's counter step took,
// which is on the log and not in this call — so its Decide refuses; the
// ledger answers it before the Decide runs if the task landed, and then the
// retry is the task the first attempt filed, under its own key, with no
// second counter record. Minting first would spend a number on every retry
// of a create that had already finished.
//
// ONLY A TASK THAT HAS NOT APPLIED HERE IS FILED ON A FRESH MINT, under an
// operation of its own: the earlier number is the documented gap, and the
// fresh one is unique under the counter's arbitration like any other. If the
// task did land and this node simply has not applied it yet, the fresh mint
// still costs only a number: the task step then meets the earlier copy on the
// task's own subject, is refused its expectation of zero, and is answered
// from the ledger once this node catches up ([Writer.fileTask]).
func (w *Writer) resumeCreate(ctx context.Context, opID string, task Task,
	notify *Notify) (WriteResult, error) {

	subject := TaskSubject(task.ID)
	scope := ScopeSet{Subject: true, Container: task.Project}
	answered, err := w.publish(ctx, statelog.Request{
		Subject: wire(subject),
		Scope:   scope.Resolve(subject),
		OpID:    stepID(opID, "task"),
		Pattern: statelog.PatternCreate,
		Decide: func(*sql.Tx, statelog.Stamp) (statelog.Decision, error) {
			return statelog.Decision{}, errTaskNotApplied
		},
	})
	switch {
	case err == nil && answered.Outcome == statelog.OutcomeUnknown:
		// THE LEDGER CANNOT VOUCH FOR THE TASK STEP, so its silence is not
		// "the task has not applied" and filing it now could file it
		// twice: the answer is the step's own, and it is not a landing.
		return WriteResult{Result: answered}, nil
	case err == nil:
		return w.landedTask(ctx, answered, task.ID)
	case !errors.Is(err, errTaskNotApplied):
		return WriteResult{Result: answered}, err
	}

	var settled settledCreate
	n, minted, err := w.mintFresh(ctx, task.Project, 1,
		w.settleCreate(ctx, task, &settled))
	if err != nil {
		return WriteResult{Result: minted}, err
	}
	return w.fileTask(ctx, opID, task, n, settled, notify)
}

// errTaskNotApplied is a resumed create's task step finding no ledger row to
// answer it: the task has not applied on this node.
var errTaskNotApplied = errors.New("tracker: the create's task has not applied here")

// landedTask is the answer to a create whose task step already landed: the
// ledger's position, and the key and rank the task was filed under, read off
// its row.
//
// A TASK THAT IS NO LONGER HERE — purged since — still landed, so the
// outcome stands and the missing key is a warning rather than a refusal.
func (w *Writer) landedTask(ctx context.Context, result statelog.Result,
	id string) (WriteResult, error) {

	out := WriteResult{Result: result}
	if w.db == nil {
		out.Warnings = append(out.Warnings, fmt.Sprintf("task %s was filed by an "+
			"earlier copy of this operation, and this writer has no store to "+
			"read its key from", id))
		return out, nil
	}
	var task Task
	var held, collision bool
	if err := w.db.Replicated().Read(ctx, func(tx *sql.Tx) error {
		var err error
		task, held, err = readTask(ctx, tx, id)
		if err != nil || !held {
			return err
		}
		collision, err = keyHeldByAnother(ctx, tx, task.Key, id)
		return err
	}); err != nil {
		return out, fmt.Errorf("tracker: task %s was filed by an earlier copy of "+
			"this operation; read its key: %w", id, err)
	}
	if !held {
		out.Warnings = append(out.Warnings, fmt.Sprintf("task %s was filed by an "+
			"earlier copy of this operation and is no longer on this node", id))
		return out, nil
	}
	out.Key, out.Rank, out.KeyCollision = task.Key, task.Rank, collision
	return out, nil
}

// filedUnit is which team a new task belongs to, and which team's lead hears
// about it.
//
// THE PROJECT'S OWN UNIT WHEN THE WRITER NAMED NONE, because that is what the
// field means: `filed_unit` is the unit the work belongs to — it is what
// `unit=` filters on, what a board's `unit` axis groups by, and what the
// dashboard labels "Filed into" — and a task's project already has an owning
// unit, written by the chart apply and carried on the project's own row.
//
// Deriving it here rather than at a caller is what makes the answer the same
// whoever files the work. It was a caller's job once and only one caller did
// it, stamping the FILING SEAT'S own team: every write from any other surface
// landed with no unit at all. An item an operator filed into ENG through
// `/operator/mcp` — an operator holds no seat and therefore no team — read
// "Filed into: no unit" on a page whose project said it belonged to Core, and
// so did every item filed by a root-level seat, which belongs to no unit
// either. Nothing derived it from the project, although the project knew.
//
// THE ROW RATHER THAN A SEAM, unlike the people fields [FieldWorld] resolves:
// a project's unit is a row this package reads inside the decide's own
// transaction, so it needs no caller to supply it — the same distinction
// [FieldWorld]'s doc draws for a relationship's task. It is also the value
// every node already holds, so a writer cannot file against a chart its peers
// have not applied.
//
// AN EXPLICIT UNIT IS LEFT ALONE, because it is the one thing the project
// cannot say: work that belongs to another team than the one that owns the
// project it sits in is exactly what `create_work_item`'s `unit` argument is
// for. And the routing half follows whatever the filed half settles on
// whenever the writer named no routing unit of its own, so the unit the work
// belongs to is the unit whose lead hears about it — while a create that
// deliberately routed somewhere else keeps that.
func filedUnit(stated, routed, project string) (filed, routing string) {
	filed, routing = stated, routed
	if filed == "" {
		filed = project
	}
	if routing == "" {
		routing = filed
	}
	return filed, routing
}

// writeTask is sequence 1's second append: a whole task at expectation zero,
// guarded by its own row.
func (w *Writer) writeTask(ctx context.Context, opID string, task Task,
	notify *Notify, at time.Time) (WriteResult, error) {

	subject := TaskSubject(task.ID)
	scope := ScopeSet{Subject: true, Container: task.Project}
	// WHETHER THE KEY THIS CREATE TOOK IS ANOTHER TASK'S, from the snapshot
	// the last round decided in — the round whose record was published.
	var collision bool
	result, err := w.publish(ctx, statelog.Request{
		Subject: wire(subject),
		Scope:   scope.Resolve(subject),
		OpID:    opID,
		Pattern: statelog.PatternCreate,
		Decide: func(tx *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			// THE GUARD ROW IS READ INSIDE THE SNAPSHOT, because a task
			// below the trim floor has no record left on the log to
			// prove it existed and its own row is what still says so.
			var present int
			if err := tx.QueryRowContext(ctx,
				`SELECT COUNT(*) FROM tracker_tasks WHERE id = ?`, task.ID).
				Scan(&present); err != nil {
				return statelog.Decision{}, fmt.Errorf("tracker: read the "+
					"guarding row for %s: %w", task.ID, err)
			}
			if present > 0 {
				return statelog.Decision{}, statelog.ErrExists
			}
			held, err := keyHeldByAnother(ctx, tx, task.Key, task.ID)
			if err != nil {
				return statelog.Decision{}, fmt.Errorf("tracker: read whether "+
					"key %s is already another task's: %w", task.Key, err)
			}
			collision = held
			return w.decide(ctx, tx, stamp, subject, OpCreate, ChangeCreated, scope, opID,
				task, notify, at)
		},
	})
	return WriteResult{
		Result: result, Warnings: bodyWarnings(task.Body), KeyCollision: collision,
	}, err
}

// mintKey takes the next key number in a project. SEQUENCE 1's first append,
// and 7's.
//
// # Why the number is a record and not a row read
//
// A counter read inside the snapshot says what this node has applied. Two
// nodes reading it would read the same number and mint two tasks with one key,
// which is precisely what a conditional append on the counter's own subject
// makes impossible: the loser is rejected, re-decides against the winner's
// number and takes the next one.
//
// It mints a RANGE of k, because a cross-project move re-keys a whole subtree
// and doing it one at a time would be one append per descendant on the busiest
// subject in the project.
func (w *Writer) mintKey(ctx context.Context, opID, project string, k int,
	guard func(*sql.Tx) error) (uint64, statelog.Result, error) {

	if k < 1 {
		return 0, statelog.Result{}, fmt.Errorf("tracker: a key mint takes %d "+
			"numbers, and a mint of none moves the counter for nothing", k)
	}
	subject := CounterSubject(project)
	scope := ScopeSet{Subject: true}
	at := w.Now()

	// THE VALUE THE LAST DECISION FORMED, captured rather than returned:
	// the framework re-decides on a rejected append, so the number that
	// landed is the one the final decision took, and only the closure sees
	// it. Reading the row afterwards would read whatever the fleet has
	// since minted.
	var base uint64
	result, err := w.publish(ctx, statelog.Request{
		Subject: wire(subject),
		Scope:   scope.Resolve(subject),
		OpID:    opID,
		Pattern: statelog.PatternArbitrated,
		Decide: func(tx *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			if guard != nil {
				if err := guard(tx); err != nil {
					return statelog.Decision{}, err
				}
			}
			counter, held, err := readCounter(ctx, tx, project)
			if err != nil {
				return statelog.Decision{}, err
			}
			base = uint64(counter.Last) + 1
			next := Counter{
				V: DocumentVersion, Project: project, Last: counter.Last + k,
			}
			decision, err := w.decide(ctx, tx, stamp, subject, OpPatch, "", scope, opID,
				next, nil, at)
			if err != nil {
				return statelog.Decision{}, err
			}
			if held {
				decision.Version = int64(counter.Version)
			}
			return decision, nil
		},
	})
	if err != nil {
		return 0, result, err
	}
	if result.Outcome == statelog.OutcomeUnknown {
		// AN UNKNOWN MINT CANNOT BE BUILT ON. The record may or may not
		// be on the log, so the number may or may not be ours — and a
		// task published against a number somebody else also holds is
		// two tasks with one key, which is the single thing this order
		// exists to prevent.
		return 0, result, fmt.Errorf("tracker: the key mint for %s is "+
			"unresolved, so the number it took cannot be built on: %w",
			project, statelog.ErrUnavailable)
	}
	if result.Collapsed {
		// NOR CAN A MINT THAT WAS ANSWERED RATHER THAN DECIDED. This
		// operation's counter already moved under an earlier copy of it,
		// and `base` is what THIS call would have taken — a number the
		// counter never recorded and the next create is free to take too,
		// which is two tasks with one key. The number the earlier copy
		// took is on the log, not here: a create resumes from its task
		// step instead ([Writer.resumeCreate]), and every other caller
		// refuses.
		return 0, result, fmt.Errorf("tracker: the key mint for %s already "+
			"landed under operation %s, and the number it took is the earlier "+
			"copy's, so it cannot be built on: %w", project, opID, errMintLanded)
	}
	return base, result, nil
}

// mintFresh takes k numbers under an operation minted for this call alone — the
// mint a create resumes on, and a cross-project move's re-mint for what its
// first range could not carry.
//
// AN ANSWER IT CANNOT PROVE ITS OWN IS MINTED AGAIN. No earlier copy of a
// fresh operation exists, but a mint whose acknowledgement was lost is
// resolved from the ledger, and the ledger cannot say whose copy of the
// operation it names, so the publisher reports it collapsed ([errMintLanded])
// and the number it took cannot be built on. Another fresh operation takes
// another number and leaves that one as a gap, which is what every other
// crash residue here costs.
//
// BOUNDED at [freshMintAttempts], because each such answer needs a lost
// acknowledgement, and the attempts after the first are spent on a broker
// that is dropping them.
func (w *Writer) mintFresh(ctx context.Context, project string, k int,
	guard func(*sql.Tx) error) (uint64, statelog.Result, error) {

	var (
		n      uint64
		minted statelog.Result
		err    error
	)
	for range freshMintAttempts {
		n, minted, err = w.mintKey(ctx,
			stepID(statelog.NewOpID(time.Now(), "remint"), "counter"),
			project, k, guard)
		if !errors.Is(err, errMintLanded) {
			return n, minted, err
		}
	}
	return 0, minted, fmt.Errorf("tracker: %d fresh key mints for %s in a row "+
		"were answered from a copy nobody can prove was theirs — each is a lost "+
		"acknowledgement, which is a broker that is dropping them: %w",
		freshMintAttempts, project, err)
}

// freshMintAttempts is how many fresh mints [Writer.mintFresh] makes before it
// gives up.
//
// THREE, because only a lost acknowledgement makes one fail, and one of those
// is the broker's ordinary weather: a second in a row on the same subject is
// already unusual, and a third is a broker failing in a way a fourth mint does
// not fix — while every attempt spends a number the project never gets back.
const freshMintAttempts = 3

// errMintLanded is a key mint answered from an earlier copy of its operation.
// Unavailable, because to a caller that cannot resume it is exactly that: the
// number is on the log and not in this call.
var errMintLanded = fmt.Errorf("the mint's number is an earlier copy's: %w",
	statelog.ErrUnavailable)

// settledCreate is what a create's own snapshot decided: the values that could
// only be settled against rows, carried back out to the record the sequence's
// second append publishes.
//
// A STRUCT RATHER THAN THREE RETURNS, because every one of them is the same
// kind of thing — read inside the mint's transaction, assigned rather than
// accumulated across the closure's runs — and a fourth loose value is where a
// caller starts pairing one run's answer with another's.
type settledCreate struct {
	// fields are the task's custom-field values in their canonical form,
	// nil when nothing needed settling.
	fields map[string]json.RawMessage

	// warnings are what the writer is told and was not refused for.
	warnings []string

	// unit is the project's own chart-owned unit, which a task that names
	// none is filed into. Empty for a project the chart gave no unit —
	// which is honest rather than a default, and is what a company with a
	// root-level project has.
	unit string
}

// refuseCreate is what sequence 1 reads the project and its catalogues for.
// It also COERCES the task's custom-field values, which is why it answers with
// them rather than only with an error: a value is normalised against the
// declarations this snapshot holds — an option spelling to the option's id, a
// timestamp on a date-only field to its date — and the record has to carry
// that canonical form, so every node writes identical rows without re-deciding
// anything.
//
// The project's own UNIT rides back out for the same reason and on the same
// row it already reads: see [filedUnit] for why the derivation belongs at the
// write rather than at whichever caller happened to know its filer's team.
func (w *Writer) refuseCreate(ctx context.Context, tx *sql.Tx, task Task) (
	settledCreate, error) {

	project, held, err := readProject(ctx, tx, task.Project)
	switch {
	case err != nil:
		return settledCreate{}, err
	case !held:
		return settledCreate{}, fmt.Errorf("tracker: project %s is not on this "+
			"node: %w", task.Project, statelog.ErrUnavailable)
	case project.Archived:
		return settledCreate{}, fmt.Errorf("tracker: project %s is archived, so "+
			"it takes no new work; unarchive it first", task.Project)
	}
	if task.Parent != nil && *task.Parent != "" {
		if err = refuseParent(ctx, tx, task, *task.Parent); err != nil {
			return settledCreate{}, err
		}
	}
	if err = declaredType(ctx, tx, task); err != nil {
		return settledCreate{}, err
	}
	if err = declaredTags(ctx, tx, task.Project, task.Tags); err != nil {
		return settledCreate{}, err
	}
	if err = requiredFields(ctx, tx, project, task); err != nil {
		return settledCreate{}, err
	}
	fields, warnings, err := settleFields(ctx, tx, task.Project, task.Type,
		task.Fields, w.fieldWorld())
	if err != nil {
		return settledCreate{}, err
	}
	return settledCreate{fields: fields, warnings: warnings, unit: project.Unit}, nil
}

// refuseParent refuses a parent a task cannot be filed under.
//
// # A SUBTREE LIVES IN ONE PROJECT
//
// A board draws a project's ROOTS and lets their subtrees ride along, and the
// query carries the container on the outer row as well — so a subtask filed in
// another project from its root is on neither project's board: its root's
// filters it out by project, and its own finds no root to hang it from. The
// attention set's `inconsistent_project` names the shape; this is what keeps
// a writer from making it. A task's project changes only when a cross-project
// move carries its whole subtree ([Writer.MoveTaskToProject]), so the parent's
// project read in THIS snapshot is the one its subtree lives in, and a refusal
// here closes the shape rather than flagging it afterwards.
//
// # Nor does the part of a subtree a move has not carried yet
//
// A move re-keys the root first, marking it mid-move, and then each
// descendant, from a subtree it read ONCE before its first append — so a task
// filed under a descendant the walk has not reached is one that walk never
// carries: it passes the project check below, since its parent is still in
// the old project. So a parent with a MARKED task above it in another project
// takes no new child until the walk has carried it ([refuseMidMove]). It is
// [statelog.ErrUnavailable] rather than a refusal, because the walk — or the
// duty that finishes an abandoned one — carries the parent without anybody
// acting, and the same call made then files the child where the parent went.
// It is asked BEFORE the project comparison, so a caller who named the
// project the parent is going to is told to come back rather than that the
// parent is somewhere else.
//
// The mark is only as good as the snapshot that reads it: a create decided on
// a node that had not applied the root's move yet cannot see it. The walk's
// last append is what catches that one — the mark does not come down while
// anything beneath the root is outside its project ([carriedAll]), so the
// walk carries it in one more pass, or the re-run or the duty does.
//
// # And a parent in the trash takes no new child
//
// A live child under a removed parent is the orphan a subtree removal
// publishes in depth order to avoid, and a restore brings back only what the
// removal took — so a child filed under the removed parent in between would
// stay a live row under a parent nobody can see.
//
// # Nor does a task's own subtree
//
// A task filed under itself, or under one of its own descendants, makes its
// parent chain a CYCLE: the applier applies it — a committed record is never
// refused there — and raises `cycle`, and the subtree drops off every board,
// since a board draws roots and a cycle has none. The closure doc says two
// concurrent re-parents on two subjects can form one no single write sees;
// ONE write forming it is simply a write nobody refused, so it is refused
// here, in the snapshot that also reads the parent.
//
// # An absent parent is one of two facts
//
// See [absentTask]: a PURGED parent is gone for good and is refused as such,
// while one this node holds no row of at all may be a create it has not
// applied yet, which is the only absence worth coming back for.
func refuseParent(ctx context.Context, tx *sql.Tx, task Task, parentID string) error {
	parent, held, err := readTask(ctx, tx, parentID)
	switch {
	case err != nil:
		return err
	case !held:
		return absentTask(ctx, tx, parentID, "parent task")
	case parent.Removed != nil:
		return fmt.Errorf("tracker: parent task %s (%s) was removed by %s at "+
			"%s; restore it before filing anything under it", parent.Key,
			parent.ID, parent.Removed.By, parent.Removed.At.Format(time.RFC3339))
	}
	if err = refuseMidMove(ctx, tx, parent, "parent task",
		"a task filed under it now would be left behind"); err != nil {
		return err
	}
	if parent.Project != task.Project {
		return fmt.Errorf("tracker: task %s is filed under %s and its parent "+
			"%s under %s — a subtask lives in its parent's project, which is "+
			"%s", task.ID, task.Project, parent.Key, parent.Project,
			parent.Project)
	}
	under, err := inSubtree(ctx, tx, task.ID, parentID)
	switch {
	case err != nil:
		return err
	case under:
		return fmt.Errorf("tracker: task %s cannot be filed under %s (%s), "+
			"which is itself or one of its own subtasks — the parent chain "+
			"would be a cycle with no root for a board to draw it from",
			task.ID, parent.Key, parentID)
	}
	return nil
}

// refuseMidMove refuses a write that would change what a task is filed under,
// or what is filed under it, while a move it is part of has not carried it —
// the task sits beneath a task marked mid-move ([TaskPatch.Moving]) that is in
// another project than it is. why says what the write would do if it landed.
//
// UNAVAILABLE, NEVER A REFUSAL: the walk, a re-run of it or the duty that
// finishes an abandoned one carries the task without anybody acting, and the
// same write asked again then is decided on the project it went to.
//
// THREE WRITES ASK IT. A create or a re-parent asks it of the PARENT, since a
// child filed under an uncarried task is one the walk never read. A re-parent
// asks it of the TASK too, since the walk still carries a task it read however
// it has been filed since: moved out from under the subtree it would land in
// the new project under a parent in the old one, and moved within it, under a
// parent the walk may already have carried. And a merge that re-parents asks it
// of both ends before its mark, for the same two reasons.
func refuseMidMove(ctx context.Context, tx *sql.Tx, task Task, role, why string) error {
	target, err := movingAway(ctx, tx, task.ID, task.Project)
	switch {
	case err != nil:
		return err
	case target == "":
		return nil
	}
	return fmt.Errorf("tracker: %s %s (%s) is still in %s under a task a move "+
		"into %s is carrying, and the walk has not reached it — %s; ask again "+
		"once the move has finished, when it is in %s: %w", role, task.Key,
		task.ID, task.Project, target, why, target, statelog.ErrUnavailable)
}

// movingAway reports the project a move is carrying a task into when a task
// ABOVE it is marked mid-move and in another project than it is, and "" when
// none is — see [refuseMidMove].
//
// ANY MARKED ANCESTOR, read off the closure, and never the task's root alone:
// the mark is on the task that MOVED, and a root re-parented under another
// while its walk runs is no longer anybody's root — asked by `root_id`, every
// task its walk had not carried read as settled. The outermost is answered
// when two are marked, so the answer names the move that decides where the
// task ends up. A task that is itself marked is in the project it is moving
// into, so it is never its own answer.
func movingAway(ctx context.Context, tx *sql.Tx, id, project string) (string, error) {
	var target string
	err := tx.QueryRowContext(ctx, `
		SELECT a.project_key
		FROM tracker_task_closure c JOIN tracker_tasks a ON a.id = c.ancestor_id
		WHERE c.descendant_id = ? AND a.moving = 1 AND a.project_key <> ?
		ORDER BY c.distance DESC LIMIT 1`, id, project).Scan(&target)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", nil
	case err != nil:
		return "", fmt.Errorf("tracker: read whether %s is in a subtree mid-move: %w",
			id, err)
	}
	return target, nil
}

// carriedAll is the amendment a move's last append is decided under: the mark
// comes down only in a snapshot where nothing beneath the root is outside the
// root's project.
//
// # Why the last append checks rather than trusting the walk
//
// A walk carries the subtree it READ, before its first append, and
// [refuseMidMove] refuses a new child under an uncarried task only on a node
// that has applied the root's move — a create decided on a node behind it
// passes, and lands under a descendant the walk read before it existed. With
// the mark taken down behind it, that task stayed in the old project for good,
// under a root in the new one, flagged `inconsistent_project` and repaired by
// nothing. Checked here, in the snapshot the last append is decided in and
// after the walk's own appends ([Writer.After]), such a task keeps the mark up:
// the append is refused as unavailable ([errUncarried]), and a pass
// ([Writer.followRoot]) re-reads the subtree and carries it — the walk's own,
// at once, or the re-run's or the duty's. A task in the trash under the root is
// the same answer, and the pass that meets it names it.
func carriedAll(ctx context.Context, tx *sql.Tx, current Task,
	patch TaskPatch) (TaskPatch, error) {

	var id, key, project string
	err := tx.QueryRowContext(ctx, `
		SELECT d.id, d.key, d.project_key
		FROM tracker_task_closure c JOIN tracker_tasks d ON d.id = c.descendant_id
		WHERE c.ancestor_id = ? AND c.distance > 0 AND d.project_key <> ?
		ORDER BY c.distance, d.id LIMIT 1`, current.ID, current.Project).
		Scan(&id, &key, &project)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return patch, nil
	case err != nil:
		return TaskPatch{}, fmt.Errorf("tracker: read whether anything under %s "+
			"is still outside %s: %w", current.ID, current.Project, err)
	}
	return TaskPatch{}, fmt.Errorf("tracker: task %s (%s) under %s is still in %s "+
		"— filed under the subtree after the walk read it, or not carried yet — "+
		"so %s stays marked mid-move; re-run the move under the same operation "+
		"id, or leave it to the tracker duty, and the next pass carries it into "+
		"%s: %w", key, id, current.Key, project, current.Key, current.Project,
		errUncarried)
}

// errUncarried is [carriedAll]'s refusal: the walk's last append met a task
// under the root that is still outside its project. Unavailable, because the
// next pass carries it with nobody acting — and a sentinel of its own, because
// the walk that met it is the one caller that runs that pass at once.
var errUncarried = fmt.Errorf("tracker: a task under a moved root is not "+
	"carried yet: %w", statelog.ErrUnavailable)

// inSubtree reports whether candidate is root itself or anywhere beneath it,
// from the closure this snapshot holds — which carries every task at distance
// zero from itself, so the one probe answers both.
func inSubtree(ctx context.Context, tx *sql.Tx, root, candidate string) (bool, error) {
	if root == "" || candidate == "" {
		return false, nil
	}
	if root == candidate {
		return true, nil
	}
	var found int
	if err := tx.QueryRowContext(ctx, `
		SELECT EXISTS (SELECT 1 FROM tracker_task_closure
		               WHERE ancestor_id = ? AND descendant_id = ?)`,
		root, candidate).Scan(&found); err != nil {
		return false, fmt.Errorf("tracker: read whether %s is under %s: %w",
			candidate, root, err)
	}
	return found == 1, nil
}

// absentTask is the answer for a task a sequence names and this snapshot holds
// no row of — THREE-VALUED, because the absence is two different facts and
// the third value is the read failing.
//
// A PURGE LEAVES A MARKER that outlives the row (see [taskGuards]), so a task
// carrying one is gone on every node and for good: that is a REFUSAL, and it
// says so. Answered as [statelog.ErrUnavailable] — "not on this node" — it
// told the caller to come back to a task that will never return, and a merge
// walk or a duty retrying it did so on every tick for ever. Only a task with
// no row AND no marker is one this node may simply not have applied yet,
// which is the absence worth coming back for.
func absentTask(ctx context.Context, tx *sql.Tx, id, role string) error {
	key, purged, err := purgedTask(ctx, tx, id)
	switch {
	case err != nil:
		return err
	case !purged:
		return fmt.Errorf("tracker: %s %s is not on this node: %w", role, id,
			statelog.ErrUnavailable)
	}
	return fmt.Errorf("tracker: %s %s (%s) was purged, and a purge is "+
		"permanent — name another task", role, key, id)
}

// purgedTask reads a task's deletion marker: the key it held, and whether a
// purge destroyed it.
func purgedTask(ctx context.Context, tx *sql.Tx, id string) (string, bool, error) {
	var key string
	err := tx.QueryRowContext(ctx,
		`SELECT task_key FROM tracker_deletions WHERE task_id = ?`, id).Scan(&key)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", false, nil
	case err != nil:
		return "", false, fmt.Errorf("tracker: read whether task %s was "+
			"purged: %w", id, err)
	}
	return key, true, nil
}

// declaredType refuses a task naming a type the company has not declared.
//
// THE TOOL HAS ALWAYS SAID SO — `create_work_item` asks for "a task type from
// your workspace's own catalogue" — and nothing checked it, so any string a
// model invented became a type: `Bug`, `bugfix` and `BUG` filed three
// different types beside `bug`, and every board grouped and filtered on them
// as if they were real.
//
// AN ARCHIVED TYPE IS REFUSED FOR NEW WORK and left alone on old, which is
// what archiving a type is FOR: the tasks already filed under it still render
// as what they are.
func declaredType(ctx context.Context, tx *sql.Tx, task Task) error {
	catalogue, _, err := readTypeCatalogue(ctx, tx)
	if err != nil {
		return err
	}
	var live []string
	for _, t := range EffectiveTypes(catalogue.Types) {
		if t.Archived {
			if t.Slug == task.Type {
				return fmt.Errorf("tracker: task type %q is archived, so no new "+
					"work is filed under it — the tasks already under it keep "+
					"it", task.Type)
			}
			continue
		}
		if t.Slug == task.Type {
			return nil
		}
		live = append(live, t.Slug)
	}
	sort.Strings(live)
	return fmt.Errorf("tracker: %q is not a task type this company declares — "+
		"the types are %v, and a new one is declared in the workspace "+
		"catalogue rather than invented at the create", task.Type, live)
}

// requiredFields refuses a task missing a field its company or its project
// declares required.
//
// # THE WHOLE CHAIN, not the project's half of it
//
// Fields are declared in TWO places — the workspace catalogue and the
// project — and [declaredFields] is what composes them, with a project's
// declaration shadowing a workspace one of the same id. Reading
// `project.Fields` alone meant a field marked required at the workspace was
// enforced on no task in any project: a rule an operator set, that nothing
// applied and nothing said was not applying.
//
// # AND ONLY THE FIELDS THAT APPLY TO THIS TASK
//
// `AppliesTo` names the TYPES a field is carried by, and a field that does not
// apply cannot be missing from a task — its value would be hidden the moment
// it was set (see [appliesTo] and the hidden state in `explodeFieldValues`).
// Ignoring it refused every plain task filed into a project that required
// `severity` of its bugs, which is the worked example this rule exists for.
//
// A SUBTASK IS JUDGED BY A DIFFERENT FLAG. ClickUp ships two toggles and so
// does this, and the default that matters is the second: a field required on a
// task is NOT required on a subtask unless the definition says so — otherwise
// one required field is a form to fill for every step somebody breaks a task
// into.
func requiredFields(ctx context.Context, tx *sql.Tx, project Project, task Task) error {
	declared, err := declaredFields(ctx, tx, project.Key)
	if err != nil {
		return err
	}
	var missing []string
	for _, f := range declared {
		required := f.Required
		if task.Parent != nil && *task.Parent != "" {
			required = f.RequiredInSubtasks
		}
		if !required || f.Archived || !appliesTo(f, task.Type) {
			continue
		}
		if _, ok := task.Fields[f.ID]; !ok {
			missing = append(missing, f.Slug)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	sort.Strings(missing)
	return fmt.Errorf("tracker: a %s in project %s requires %v, which this task "+
		"does not set", task.Type, project.Key, missing)
}

// bodyWarnings is what a writer is told and not refused for.
//
// THE APPLY MUST NOT DEPEND ON IT. Every node applies the same record and
// resolves the same first [MaxReferencesPerBody] keys in document order, so
// the cap is deterministic without this — the warning exists so the person who
// wrote a body full of keys learns that most of them link nothing, rather than
// discovering it on a page.
func bodyWarnings(body string) []string {
	if body == "" {
		return nil
	}
	if found := taskKeysIn(body); len(found) > MaxReferencesPerBody {
		return []string{fmt.Sprintf("this body names %d distinct task keys and "+
			"the first %d are linked; the rest are left as plain text",
			len(found), MaxReferencesPerBody)}
	}
	return nil
}

// taskProject is the project a task is in, read outside any decision — for a
// sequence's later step, whose scope names the container and whose own decide
// re-reads everything it acts on.
func (w *Writer) taskProject(ctx context.Context, id string) (string, error) {
	if w.db == nil {
		return "", fmt.Errorf("tracker: this writer has no store to read task "+
			"%s's project from", id)
	}
	var project string
	err := w.db.Replicated().Read(ctx, func(tx *sql.Tx) error {
		task, held, err := readTask(ctx, tx, id)
		switch {
		case err != nil:
			return err
		case !held:
			return fmt.Errorf("tracker: task %s is not on this node: %w",
				id, statelog.ErrUnavailable)
		}
		project = task.Project
		return nil
	})
	return project, err
}

// held is one durable claim, heartbeated for as long as a walk runs.
type held struct {
	claims   Claims
	resource string
	owner    string
	epoch    int64
	stop     chan struct{}
	done     chan struct{}
}

// hold takes a claim and keeps it alive.
//
// THE HEARTBEAT IS A GOROUTINE WITH AN OWNER, per this tree's rule: it is
// started here, stopped by [held.release], and cannot outlive the sequence
// that took it. A heartbeat nobody stops is a claim nobody else can ever take.
//
// UNDER AN OWNER OF ITS OWN ([Writer.claimOwner]), never this node's id, so a
// second walk of the same thing on the SAME node is refused exactly as a
// peer's would be.
func (w *Writer) hold(ctx context.Context, resource string) (*held, error) {
	if w.claims == nil {
		return nil, fmt.Errorf("tracker: this writer has no coordination, so "+
			"it cannot take %s — a walking sequence without a claim is two "+
			"nodes rewriting one subtree", resource)
	}
	owner := w.claimOwner()
	lease, err := w.claims.TryAcquire(ctx, resource, coord.AcquireOptions{
		Owner: owner, TTL: ClaimTTL,
	})
	switch {
	case err != nil:
		// UNKNOWN, AND THIS ONE FAILS CLOSED. A cross-project move
		// rewrites a whole subtree's keys; two of them interleaved
		// produce a subtree keyed into two projects, which no duty can
		// tell from an abandoned walk.
		return nil, fmt.Errorf("tracker: take %s: %w", resource, err)
	case lease == nil:
		return nil, fmt.Errorf("tracker: %s is held by another walk, on this "+
			"node or a peer, so this walk is already running: %w", resource,
			statelog.ErrUnavailable)
	}
	h := &held{
		claims: w.claims, resource: resource, owner: owner,
		epoch: lease.Epoch, stop: make(chan struct{}), done: make(chan struct{}),
	}
	go h.beat(context.WithoutCancel(ctx))
	return h, nil
}

// claimOwner is the owner one claim is taken under: this node, and THIS call.
//
// UNIQUE PER CLAIM, never the node id alone, and that is the whole of it.
// [coord] reads an owner that already holds a lease as that holder RENEWING
// it — an owner is a holder, not a machine — so under the node id two walks
// on one node were both told yes: the tracker duty's completion of a merge or
// a move ran beside the live walk it was meant to leave alone on every
// single-node deployment, two moves of one task walked it together, and
// whichever finished first released the other's lease in the middle of its
// walk, leaving it unclaimed for a third to join.
//
// Not an in-process claim beside a node-wide owner, the shape [setup.Hold]
// takes: that closes the collision inside one process and leaves it open
// across two that share a node id — a restarted process renewing the lease its
// dead predecessor held, which it cannot tell from one that is still walking.
// A holder of its own also costs that restart nothing it should keep: a claim
// whose process died lapses over [ClaimTTL], after which the duty or a re-run
// takes it, which is what an abandoned walk was always waiting for.
//
// The node id stays in front, so a claim a person reads still says where its
// walk runs.
func (w *Writer) claimOwner() string {
	return w.nodeID + "/" + uuid.NewString()
}

func (h *held) beat(ctx context.Context) {
	defer close(h.done)
	ticker := time.NewTicker(ClaimHeartbeat)
	defer ticker.Stop()
	for {
		select {
		case <-h.stop:
			return
		case <-ticker.C:
			// A FAILED RENEW IS NOT FATAL HERE. The claim's TTL is four
			// heartbeats, so three misses are survivable, and the walk
			// that would be abandoned on the fourth is idempotent and
			// completed by the duty. Tearing the walk down on the first
			// blip would abandon more of them, not fewer.
			_, _ = h.claims.Renew(ctx, h.resource, h.owner, h.epoch, ClaimTTL)
		}
	}
}

// release stops the heartbeat and gives the claim up.
//
// [context.WithoutCancel], because the failure being undone is often the
// cancellation itself and a release that inherited a dead context would leave
// the claim to expire — sixty seconds during which nobody else may run this
// walk and the duty has not yet decided it was abandoned.
func (h *held) release(ctx context.Context) {
	close(h.stop)
	<-h.done
	_, _ = h.claims.Release(context.WithoutCancel(ctx), h.resource, h.owner, h.epoch)
}

// MoveStopped reports a cross-project move whose ROOT landed in its target and
// whose walk over the subtree stopped before carrying all of it.
//
// A TYPE RATHER THAN A SENTENCE, for [SubtreeStopped]'s reason: the caller has
// to say two true things at once — the item it named is in the new project,
// and some of what goes with it is not — and a bare error made every caller
// say the second as though it were the first. The move tool answered every
// such stop but an unknown step with "the change was NOT made", about a root
// that had moved and been re-keyed; a model told that moves it again, and a
// person told that goes looking for an item under a key it no longer has.
//
// THE REMEDY IS THE SAME OPERATION, and only that — which is where it differs
// from a removal's: a new operation meets a root already in the target that
// its own ledger never moved there, and is refused as somebody else's move
// ([Writer.finishMove]). The same operation is answered from the ledger for
// the root and carries the rest; and the root stays marked mid-move for as
// long as anything is left, so the tracker duty finishes the walk on its own
// once nobody holds its claim ([duty.finishMoves]).
type MoveStopped struct {
	// Root is the task the move named, and Key the key it holds in the
	// target — empty only when this stop could not read it back, which a
	// get of Root answers.
	Root, Key string

	// KeyCollision says Key opens ANOTHER task — one that claimed it first —
	// so the root is reached by Root, its id. See [WriteResult.KeyCollision].
	KeyCollision bool

	// Target is the project the root is in now.
	Target string

	// Followed is how many of the tasks under the root are in the target,
	// and Of how many there are.
	Followed, Of int

	// Waiting is the ADDRESS of a task under the root that is in the TRASH
	// and still in the old project — its key, or its id where another task
	// claimed that key first ([ItemAddress]), since it is what a caller
	// hands a restore: frozen, so nothing carries it until somebody
	// restores it — or purges it, which takes it out of the subtree. Empty
	// for every other stop.
	Waiting string

	// OpID is the move's operation — what finishes it.
	OpID string

	// Err is why the walk stopped: a refused step, a step whose outcome
	// is unknown ([ErrStepUnresolved]) or one this node cannot vouch for
	// ([ErrStepUnvouched]), or the task in the trash it waits for.
	Err error
}

func (e *MoveStopped) Error() string {
	root := e.Root
	if e.Key != "" {
		root = fmt.Sprintf("%s (%s)", e.Key, e.Root)
	}
	next := fmt.Sprintf("the same operation (%s) finishes it — a new one is "+
		"refused, since the root is already in %s — and so does the tracker "+
		"duty once nobody is walking it", e.OpID, e.Target)
	if e.Waiting != "" {
		next = fmt.Sprintf("task %s is in the trash and the move waits for it: "+
			"restore it and the same operation (%s), or the tracker duty, "+
			"carries it into %s — or purge it", e.Waiting, e.OpID, e.Target)
	}
	return fmt.Sprintf("tracker: task %s moved into %s, and %d of the %d tasks "+
		"under it followed before the walk stopped — the rest are still in "+
		"their old project; %s: %v", root, e.Target, e.Followed, e.Of, next, e.Err)
}

func (e *MoveStopped) Unwrap() error { return e.Err }

// MoveTaskToProject re-homes a task and everything beneath it. SEQUENCE 7.
//
//	Rk, then take move/<task> → Rs the target project and its tag set;
//	refuse archived, refuse a required field the task lacks, refuse a
//	subtree with a task in the trash → A the tags the subtree carries that
//	the target lacks → A the alias on the former key at expectation 0 → A
//	the counter, a RANGE mint for the whole subtree → A the root task on
//	the range's first number, MARKED mid-move when it has a subtree → per
//	descendant, in (depth, id) order, A on its own subject on the next →
//	A taking the root's mark down → release.
//
// THE SOURCE PROJECT'S ORDER IS NOT REWRITTEN. The rows leave it entirely, so
// there is nothing to place; what covers a reader whose closure names the
// source is this sequence's own scope, which carries BOTH containers.
//
// # Re-running it
//
// Under the SAME operation id — what a caller told `unknown` does, and what a
// turn re-run after a crash does — and every step answers for itself: the
// tags, the alias and each task's move are answered from the ledger where they
// landed. A ROOT ALREADY IN THE TARGET that this operation moved is the move's
// answer, `applied`, and whatever descendants have not followed it are moved
// now ([Writer.finishMove]); one this operation did NOT move is refused, since
// somebody else moved it. It used to be refused either way — "already in" the
// project the move itself had put it in.
//
// The descendants left behind are moved on a FRESH range, and the numbers the
// first run minted for them are a gap. Nothing records which number was meant
// for which task, and a range re-derived from the subtree as it stands now
// would pair numbers with tasks differently: its membership moves as tasks are
// filed and re-parented, so the (depth, id) order fixes a walk's order and not
// a key assignment anybody can reproduce.
//
// Each descendant's step is named by the DESCENDANT, never by its place in the
// walk, for the reason [Writer.UpdateTasks] gives: the ledger answers a step's
// id with whatever it recorded under it, and a re-run's walk is shorter than
// the first run's.
//
// # When nobody re-runs it
//
// The root's own append carries the MARK ([TaskPatch.Moving]) whenever there is
// a subtree behind it, and the walk's last append takes it down. A walk that
// stopped between the two — its process died, a descendant's append was
// refused, its caller never retried — leaves a root that says so, and the
// tracker duty finishes it once this sequence's claim has lapsed: every
// descendant still outside the root's project follows it on a fresh range, and
// the mark comes down ([duty.finishMoves]). Before the mark, nothing did: the
// subtree stayed split across two projects until somebody re-ran the gesture
// under an operation id nobody had kept.
//
// While the mark is up, nothing is filed under, or moved from under, a task the
// walk has not carried ([refuseMidMove]); and the last append takes it down
// only once nothing under the root is outside its project ([carriedAll]). A
// task filed there by a node the mark had not reached yet is therefore carried
// rather than left behind: the walk runs one more pass for it, as the duty
// would, and one it cannot carry — put in the trash since — keeps the mark up
// and is named.
//
// # Who hears about it
//
// The people on the ROOT, through notify — the root's own move carries it, and
// nothing beneath it does: a subtree that moved is one thing that happened, and
// forty subtasks would otherwise wake everybody watching any of them for it.
// The root's step was published with no notification at all, so a move woke
// nobody, although the kind routes and falls back to a lead like a status
// change does.
//
// # The tags the subtree carries
//
// ARE READ HERE, from the subtree and its own project's declarations, and
// declared in the target as an ADD ([Writer.WriteTags]) resolved inside that
// write's own snapshot. The caller used to hand them in, which every caller
// did as nil — so the step never ran and a moved task arrived carrying labels
// its new project had never declared — and the step itself wrote the target's
// WHOLE set composed from a read outside any snapshot, which a tag a colleague
// declared in between would have been overwritten by.
//
// # What it refuses before the first append
//
// A task in the TRASH anywhere in the subtree, the root included. A tombstoned
// task refuses every write, so its step would stop the walk — and the re-run,
// and the duty behind both — on the same task for good, with the subtree split
// around it. So the move refuses whole and names it: restore it or purge it,
// then move again. A task removed while the walk runs cannot be refused this
// way, and the walk waits for it instead ([Writer.followRoot]).
//
// CRASH RESIDUE: a tag declared with no task yet (harmless); an alias for a key
// still held (harmless — the apply never lowers `current`); a numbering gap;
// descendants still keyed in the old project, under a root marked mid-move.
// REPAIRER: the re-run above or the tracker duty, whichever comes first — the
// other then finds nothing left to move.
func (w *Writer) MoveTaskToProject(ctx context.Context, opID, taskID, target string,
	notify *Notify) (WriteResult, error) {

	switch {
	case taskID == "":
		return WriteResult{}, fmt.Errorf("tracker: a cross-project move names no task")
	case target == "":
		return WriteResult{}, fmt.Errorf("tracker: a cross-project move names no project")
	}
	claim, err := w.hold(ctx, moveClaim(taskID))
	if err != nil {
		return WriteResult{}, err
	}
	defer claim.release(ctx)

	// The subtree, read ONCE and ordered by (depth, id) — see [readSubtree].
	var (
		root    Task
		subtree []Task
		carried []Tag
		arrived bool

		// rootCollision is whether the root's CURRENT key opens another
		// task, read beside it: what a move already in the target answers
		// with, and names its root by when its walk stops.
		rootCollision bool
	)
	if w.db == nil {
		return WriteResult{}, fmt.Errorf("tracker: this writer has no store, " +
			"so it cannot read the subtree a cross-project move re-keys")
	}
	if err := w.db.Replicated().Read(ctx, func(tx *sql.Tx) error { //nolint:govet // shadow: scoped to this block; see .golangci.yml (trailing: covers this line only, not the closure)
		//nolint:govet // shadow: `x, err := f()` declares x too; see .golangci.yml
		current, held, err := readTask(ctx, tx, taskID)
		switch {
		case err != nil:
			return err
		case !held:
			return fmt.Errorf("tracker: task %s is not on this node: %w",
				taskID, statelog.ErrUnavailable)
		case current.Parent != nil && *current.Parent != "":
			return fmt.Errorf("tracker: task %s has a parent, and only a ROOT "+
				"task moves between projects — moving a subtask alone would "+
				"leave it in a project its parent is not in", taskID)
		case current.Removed != nil:
			return fmt.Errorf("tracker: task %s was removed by %s at %s; "+
				"restore it before moving it", taskID, current.Removed.By,
				current.Removed.At.Format(time.RFC3339))
		}
		root = current
		if current.Project == target {
			// ALREADY THERE: a re-run of this move, or somebody else's
			// — which of the two is the ledger's to say, not this read.
			arrived = true
			if rootCollision, err = keyHeldByAnother(ctx, tx, current.Key,
				taskID); err != nil {
				return err
			}
			subtree, err = readSubtree(ctx, tx, taskID)
			return err
		}
		project, held, err := readProject(ctx, tx, target)
		switch {
		case err != nil:
			return err
		case !held:
			return fmt.Errorf("tracker: project %s is not on this node: %w",
				target, statelog.ErrUnavailable)
		case project.Archived:
			return fmt.Errorf("tracker: project %s is archived, so nothing "+
				"moves into it", target)
		}
		//nolint:govet // shadow: scoped to this block; see .golangci.yml
		if err := requiredFields(ctx, tx, project, current); err != nil {
			return err
		}
		if subtree, err = readSubtree(ctx, tx, taskID); err != nil {
			return err
		}
		for _, descendant := range subtree {
			if descendant.Removed != nil {
				return fmt.Errorf("tracker: task %s under %s is in the trash, "+
					"and a removed task is frozen, so the move could not carry "+
					"it and would leave it in %s under a root in %s — restore "+
					"it or purge it, then move again", descendant.ID, taskID,
					current.Project, target)
			}
		}
		carried, err = carriedTags(ctx, tx, current.Project,
			append([]Task{current}, subtree...))
		return err
	}); err != nil {
		return WriteResult{}, err
	}
	if arrived {
		return w.finishMove(ctx, opID, root, rootCollision, target, subtree)
	}
	if len(subtree) > MaxDescendants {
		return WriteResult{}, fmt.Errorf("tracker: task %s has %d descendants "+
			"and a move carries at most %d", taskID, len(subtree), MaxDescendants)
	}

	at := w.Now()
	if len(carried) > 0 {
		// AN ADD, NEVER A WHOLE SET: a tag the target already declares is
		// left as it is, and one a colleague declared a moment ago is not
		// overwritten by a set this call read before it.
		//nolint:govet // shadow: `x, err := f()` declares x too; see .golangci.yml
		declared, err := w.WriteTags(ctx, stepID(opID, "tags"), target,
			TagEdit{Add: carried}, TagAuthority{})
		if err = resolved("the moving subtree's tags in "+target,
			declared, err); err != nil {
			return declared, fmt.Errorf("tracker: declare the moving "+
				"subtree's tags in %s: %w", target, err)
		}
	}

	// THE ALIAS BEFORE THE RE-KEY, so a key somebody pastes into chat
	// keeps resolving from the moment it stops being current.
	aliased, err := w.claimAlias(ctx, stepID(opID, "alias"), root.Key, taskID, at)
	if err = resolved("key "+root.Key+"'s alias", aliased, err); err != nil {
		return aliased, err
	}

	base, minted, err := w.mintKey(ctx, stepID(opID, "counter"), target, 1+len(subtree), nil)
	if minted.Outcome == statelog.OutcomeUnknown && minted.Unvouched {
		// NOT "NOT MOVED": the operation predates this node's ledger loss,
		// so an earlier run may have moved the root on a node this one has
		// not caught up with. The walk stops as one this node cannot vouch
		// for, which is what the caller has to be told.
		return WriteResult{Result: minted}, resolved(fmt.Sprintf(
			"the key range for task %s's move into %s", taskID, target),
			WriteResult{Result: minted}, nil)
	}
	if errors.Is(err, errMintLanded) {
		// THE COUNTER STEP LANDED UNDER AN EARLIER COPY and the root has
		// not moved — this node read it in its old project — so the range
		// that copy took is on the log and not here. This run takes one
		// of its own, and the earlier one is the gap.
		base, _, err = w.mintFresh(ctx, target, 1+len(subtree), nil)
	}
	if err != nil {
		return WriteResult{}, err
	}

	// THE MARK RIDES THE ROOT'S OWN MOVE, and only when a subtree is
	// behind it: a leaf's move is one append, finished the moment it lands,
	// and a mark on it would be one more append to take it down.
	former := append(append([]string{}, root.FormerKeys...), root.Key)
	result, err := w.moveOne(ctx, stepID(opID, "root"), root, target, former,
		&KeyMint{N: base}, len(subtree) > 0, notify)
	// A ROOT WHOSE MOVE IS UNKNOWN HAS NO SUBTREE TO FOLLOW IT: moving the
	// descendants under a root that may still be in its old project splits
	// the subtree the other way round.
	if err = resolved(fmt.Sprintf("task %s's move into %s", taskID, target),
		result, err); err != nil {
		return result, err
	}
	if len(subtree) > 0 {
		var (
			followed int
			last     statelog.Position
		)
		followed, last, err = w.moveDescendants(ctx, opID, target, subtree, base+1)
		if err == nil {
			// THE MARK COMES DOWN ONLY ONCE THIS NODE HOLDS THE WHOLE
			// WALK: its decide asks whether anything under the root is
			// still outside the target ([carriedAll]), and the
			// descendants are on subjects of their own — so the last of
			// them, and never only the root's position, is what this
			// append has to see. See [Writer.After].
			err = w.After(laterOf(result.Position, last)).endMove(ctx, opID,
				taskID, target)
			if errors.Is(err, errUncarried) {
				// A TASK FILED BEHIND THE WALK, which this node has
				// now applied — the append that met it waited for the
				// whole walk first. So the pass the duty would run a
				// quarter of an hour from now runs here, under the
				// claim this call holds: it re-reads the subtree,
				// carries what the first read could not see, and takes
				// the mark down. Once — a second task filed behind THIS
				// pass is the duty's, and the caller is told.
				_, err = w.finishAbandonedMove(ctx, opID, taskID)
			}
		}
		if err != nil {
			// THE ROOT MOVED, so whatever stopped the walk is a stop
			// and never a refusal of the move: see [MoveStopped].
			return result, w.moveStopped(ctx, result, opID, root, target,
				fmt.Sprintf("%s-%d", target, base), followed, len(subtree), err)
		}
	}
	if result.Collapsed {
		// THE ROOT MOVED UNDER AN EARLIER COPY that this node had not
		// applied when it read it, so its key is that copy's number,
		// read off its row, and base is the gap.
		return w.landedTask(ctx, result.Result, taskID)
	}
	rootRank, err := IntegerAt(base)
	if err != nil {
		return result, err
	}
	result.Key, result.Rank = fmt.Sprintf("%s-%d", target, base), rootRank
	return result, nil
}

// finishMove answers a move whose root is already in the target project: a
// re-run of a move that got at least as far as its root.
//
// THE ROOT'S STEP IS ASKED FOR, AND ONLY ANSWERED, the way [Writer.resumeCreate]
// asks for a create's task: the ledger answers it before the Decide runs if
// THIS operation moved the root, and a Decide that does run means somebody
// else did, which is a refusal. Then the rest follows it ([Writer.followRoot]).
func (w *Writer) finishMove(ctx context.Context, opID string, root Task,
	rootCollision bool, target string, subtree []Task) (WriteResult, error) {

	subject := TaskSubject(root.ID)
	scope := ScopeSet{Subject: true, Container: target}
	answered, err := w.publish(ctx, statelog.Request{
		Subject: wire(subject),
		Scope:   scope.Resolve(subject),
		OpID:    stepID(opID, "root"),
		Pattern: statelog.PatternArbitrated,
		Decide: func(*sql.Tx, statelog.Stamp) (statelog.Decision, error) {
			return statelog.Decision{}, fmt.Errorf("tracker: task %s is already "+
				"in %s, and this node's operation ledger holds no record of "+
				"this move putting it there", root.ID, target)
		},
	})
	switch {
	case err != nil:
		return WriteResult{Result: answered}, err
	case answered.Outcome == statelog.OutcomeUnknown:
		// THE LEDGER CANNOT SAY, so neither can this call — and moving
		// the rest on a root that another operation may have moved would
		// finish somebody else's walk under this one's name.
		return WriteResult{Result: answered}, resolved(fmt.Sprintf(
			"task %s's move into %s", root.ID, target),
			WriteResult{Result: answered}, nil)
	}
	// The root is in the target and keyed there, so a stop past this point
	// is [Writer.followRoot]'s own [MoveStopped].
	if err := w.followRoot(ctx, opID, root, rootCollision, subtree); err != nil {
		return WriteResult{Result: answered}, err
	}
	out := WriteResult{Result: answered}
	out.Key, out.Rank, out.KeyCollision = root.Key, root.Rank, rootCollision
	return out, nil
}

// finishAbandonedMove completes a move whose walk stopped with its root marked
// mid-move: the tracker duty's half of [Writer.MoveTaskToProject], run under
// the move's own claim, which the caller holds. It reports whether there was a
// walk to finish. The walk runs it too, once, when its last append met a task
// filed behind it ([errUncarried]).
//
// THE ROOT IS READ AGAIN HERE, under the claim, because the holder may have
// finished between the duty's selection and its claim: a root whose mark is
// down is a move that is done, and one in the trash is frozen until somebody
// restores it.
func (w *Writer) finishAbandonedMove(ctx context.Context, opID, id string) (bool, error) {
	if w.db == nil {
		return false, fmt.Errorf("tracker: this writer has no store, so it " +
			"cannot read the subtree an abandoned move left behind")
	}
	var (
		root      Task
		subtree   []Task
		marked    bool
		collision bool
	)
	err := w.db.Replicated().Read(ctx, func(tx *sql.Tx) error {
		current, held, err := readTask(ctx, tx, id)
		switch {
		case err != nil:
			return err
		case !held:
			return fmt.Errorf("tracker: task %s is marked mid-move and not on "+
				"this node: %w", id, statelog.ErrUnavailable)
		case !current.Moving || current.Removed != nil:
			return nil
		}
		root, marked = current, true
		if collision, err = keyHeldByAnother(ctx, tx, current.Key, id); err != nil {
			return err
		}
		subtree, err = readSubtree(ctx, tx, id)
		return err
	})
	if err != nil || !marked {
		return false, err
	}
	return true, w.followRoot(ctx, opID, root, collision, subtree)
}

// carryStragglers carries into a root's project every live task under it that
// is still outside it although no move of that root is running: the tracker
// duty's repair for a task filed behind a finished move ([duty.carryStragglers]),
// run under the move's own claim, which the caller holds. It answers how many
// it carried.
//
// THE ROOT IS READ AGAIN HERE, under the claim, and anything that makes it no
// longer this repair's is nothing to do: a root in the trash is frozen, a
// MARKED one is a walk the abandoned-move job finishes, and one with a parent
// is where a cycle's walk stopped rather than a root. A straggler in the trash
// is left out of the pass rather than waited for — with the mark down there is
// no walk to hold open for it, and its restore brings it back to the duty.
func (w *Writer) carryStragglers(ctx context.Context, opID, id string) (int, error) {
	if w.db == nil {
		return 0, fmt.Errorf("tracker: this writer has no store, so it " +
			"cannot read the subtree a move left a task behind in")
	}
	var (
		root      Task
		subtree   []Task
		left      int
		collision bool
	)
	err := w.db.Replicated().Read(ctx, func(tx *sql.Tx) error {
		current, held, err := readTask(ctx, tx, id)
		switch {
		case err != nil:
			return err
		case !held:
			return fmt.Errorf("tracker: task %s has a task filed behind its "+
				"move and is not on this node: %w", id, statelog.ErrUnavailable)
		case current.Removed != nil, current.Moving,
			current.Parent != nil && *current.Parent != "":
			return nil
		}
		all, err := readSubtree(ctx, tx, id)
		if err != nil {
			return err
		}
		if collision, err = keyHeldByAnother(ctx, tx, current.Key, id); err != nil {
			return err
		}
		root = current
		for _, descendant := range all {
			if descendant.Removed != nil {
				continue
			}
			subtree = append(subtree, descendant)
			if descendant.Project != current.Project {
				left++
			}
		}
		return nil
	})
	if err != nil || left == 0 {
		return 0, err
	}
	if err := w.followRoot(ctx, opID, root, collision, subtree); err != nil {
		return 0, err
	}
	return left, nil
}

// followRoot moves every descendant a root's move has not carried yet, and
// then takes the root's mark down. SHARED BY THE RE-RUN AND THE DUTY, so a walk
// finished either way is finished by one algorithm rather than by two that
// have to keep agreeing.
//
// ON A FRESH RANGE, never the first run's — see [Writer.MoveTaskToProject] —
// and a descendant is LEFT when it is outside the ROOT'S project, whichever
// project that is: the move carries the whole subtree as it stands NOW, which
// includes a task filed under a descendant after the first run read it, by a
// node the root's mark had not reached yet ([refuseMidMove] refuses one
// after). That task is what keeps the first run's mark up ([carriedAll]), so
// this pass is the one that carries it.
//
// A DESCENDANT IN THE TRASH IS WAITED FOR, never skipped. The move refuses a
// subtree holding one before its first append, so this is a task removed
// while the walk ran; it is frozen, so nothing can carry it until somebody
// restores it, and taking the mark down around it would leave it in the old
// project for good, under a root in the new one. So everything else moves, the
// mark stays up, and the refusal names the task: the restore is what lets the
// next pass — the duty's or a re-run — finish the walk, and a purge takes it
// out of the subtree altogether.
//
// ON A NODE BEHIND THE LOG the subtree it reads may still show descendants the
// walk already carried. Their steps are refused inside their own snapshots —
// each is conditioned on the project it was read in, and the task is no longer
// there — so nothing is moved twice; the range minted for them is a gap, which
// is what every other crash residue here costs.
//
// EVERY FAILURE IS A [MoveStopped]: the root is in its project before this
// runs, so whatever stops the pass stops a move that has happened.
//
// rootCollision is whether the root's key opens another task, read beside the
// root by the caller ([keyHeldByAnother]), so a stop names the root the way a
// caller can open it.
func (w *Writer) followRoot(ctx context.Context, opID string, root Task,
	rootCollision bool, subtree []Task) error {

	var left, frozen []Task
	for _, descendant := range subtree {
		switch {
		case descendant.Project == root.Project:
		case descendant.Removed != nil:
			frozen = append(frozen, descendant)
		default:
			left = append(left, descendant)
		}
	}
	carried := len(subtree) - len(left) - len(frozen)
	stop := func(followed int, waiting string, err error) error {
		return &MoveStopped{Root: root.ID, Key: root.Key,
			KeyCollision: rootCollision, Target: root.Project,
			Followed: followed, Of: len(subtree), Waiting: waiting, OpID: opID,
			Err: err}
	}
	if len(left) > MaxDescendants {
		return stop(carried, "", fmt.Errorf("tracker: task %s has %d "+
			"descendants still to move and a move carries at most %d", root.ID,
			len(left), MaxDescendants))
	}
	var last statelog.Position
	if len(left) > 0 {
		tags, err := w.followedTags(ctx, left)
		if err != nil {
			return stop(carried, "", err)
		}
		if len(tags) > 0 {
			// AN ADD, as the first run's: see [Writer.MoveTaskToProject].
			declared, declareErr := w.WriteTags(ctx, stepID(opID, tagsStep(tags)),
				root.Project, TagEdit{Add: tags}, TagAuthority{})
			if err = resolved("the carried tags in "+root.Project,
				declared, declareErr); err != nil {
				return stop(carried, "", fmt.Errorf("tracker: declare the tags "+
					"the rest of %s's subtree carries in %s: %w", root.ID,
					root.Project, err))
			}
		}
		base, _, err := w.mintFresh(ctx, root.Project, len(left), nil)
		if err != nil {
			return stop(carried, "", err)
		}
		var moved int
		moved, last, err = w.moveDescendants(ctx, opID, root.Project, left, base)
		if err != nil {
			return stop(carried+moved, "", err)
		}
	}
	if len(frozen) > 0 {
		return stop(carried+len(left), w.addressOf(ctx, frozen[0]),
			fmt.Errorf("tracker: task %s (%s) under %s is in the trash and "+
				"still in project %s, and a removed task is frozen — the move "+
				"waits for it: restore it and the next pass carries it into %s, "+
				"or purge it", frozen[0].Key, frozen[0].ID, root.ID,
				frozen[0].Project, root.Project))
	}
	// AFTER WHAT THIS PASS MOVED, for the reason the sequence's own last
	// append waits: [carriedAll] asks this node's rows whether anything
	// under the root is still outside its project.
	if err := w.After(last).endMove(ctx, opID, root.ID, root.Project); err != nil {
		return stop(carried+len(left), "", err)
	}
	return nil
}

// addressOf is the reference a caller opens a task by: its key, or its id where
// another task claimed the key first ([ItemAddress]).
//
// ITS ID WHEREVER THE DIRECTORY CANNOT BE READ, because the id is the one
// reference that is never wrong — only less readable — and this names a task
// to somebody about to act on it.
func (w *Writer) addressOf(ctx context.Context, task Task) string {
	if w.db == nil {
		return task.ID
	}
	var collision bool
	if err := w.db.Replicated().Read(ctx, func(tx *sql.Tx) error {
		var err error
		collision, err = keyHeldByAnother(ctx, tx, task.Key, task.ID)
		return err
	}); err != nil {
		return task.ID
	}
	return ItemAddress(task.ID, task.Key, collision)
}

// endMove takes a root's mid-move mark down: the walk's last append.
//
// A MARK ALREADY DOWN IS NOTHING TO WRITE, decided inside the append's own
// snapshot ([Writer.UpdateTask]) — so a re-run of a walk whose last append
// landed, and a duty that raced the holder's own, each publish nothing rather
// than a history row saying nothing.
//
// A MARK OVER A TASK THE WALK HAS NOT CARRIED STAYS UP ([carriedAll]), decided
// in the same snapshot — so a caller waits the walk's appends out first
// ([Writer.After]) or it reads its own walk as unfinished.
//
// A QUIET COMMIT UNDER [ChangeMoved]: it is the move finishing, and the people
// watching the root heard about the move from its first append.
func (w *Writer) endMove(ctx context.Context, opID, rootID, project string) error {
	down := false
	result, err := w.updateTask(ctx, stepID(opID, "moved"), rootID, project,
		NoIfMatch, TaskPatch{Moving: &down}, ChangeMoved, nil, carriedAll)
	return resolved(fmt.Sprintf("the mid-move mark's removal from task %s",
		rootID), result, err)
}

// moveDescendants moves each of a subtree's descendants, in order, on the
// consecutive numbers from base, and answers how many it moved and the
// position of the last — what the append that takes the mark down has to have
// applied before it decides. A step that stops the walk is answered as it is:
// the caller knows how much of the subtree was carried before this pass, which
// the [MoveStopped] it forms has to count.
func (w *Writer) moveDescendants(ctx context.Context, opID, target string,
	descendants []Task, base uint64) (int, statelog.Position, error) {

	var last statelog.Position
	for i, descendant := range descendants {
		moved, err := w.moveOne(ctx, stepID(opID, "task-"+descendant.ID),
			descendant, target,
			append(append([]string{}, descendant.FormerKeys...), descendant.Key),
			&KeyMint{N: base + uint64(i)}, false, nil)
		// AN UNKNOWN DESCENDANT STOPS THE WALK like a refused one, and
		// the root's mark stays up over it: lowering the mark past a
		// task that may still be in the old project is the split subtree
		// nothing would look for again.
		if err = resolved(fmt.Sprintf("task %s's move into %s",
			descendant.ID, target), moved, err); err != nil {
			return i, last, err
		}
		last = laterOf(last, moved.Position)
	}
	return len(descendants), last, nil
}

// moveStopped is what [Writer.MoveTaskToProject] answers when the walk stops
// after the root's own move landed: a [MoveStopped], and never the bare error
// that stopped it.
//
// A LATER PASS's OWN STOP IS KEPT WHOLE — the pass the walk runs for a task
// filed behind it ([Writer.followRoot]) read the subtree again and counts what
// it found, which the first read did not hold. key is the root's key in the
// target by this run's own range; a root that moved under an EARLIER copy of
// the operation holds that copy's number instead, read off its row.
func (w *Writer) moveStopped(ctx context.Context, result WriteResult, opID string,
	root Task, target, key string, followed, of int, err error) error {

	var stop *MoveStopped
	if errors.As(err, &stop) {
		return err
	}
	collision := result.KeyCollision
	if result.Collapsed {
		key, collision = "", false
		if landed, readErr := w.landedTask(ctx, result.Result, root.ID); readErr == nil {
			key, collision = landed.Key, landed.KeyCollision
		}
	}
	return &MoveStopped{Root: root.ID, Key: key, KeyCollision: collision,
		Target: target, Followed: followed, Of: of, OpID: opID, Err: err}
}

// laterOf is the later of two positions on one log, the zero position being
// earlier than every other — what a step waits for when two of a gesture's
// appends went to different subjects and either may be the newer.
func laterOf(a, b statelog.Position) statelog.Position {
	switch {
	case a.IsZero():
		return b
	case b.IsZero():
		return a
	}
	if before, err := a.Before(b); err == nil && before {
		return b
	}
	return a
}

// moveOne re-homes one task of a moving subtree, and marks it mid-move when it
// is the root of one that has a walk still to come.
//
// CONDITIONED ON THE PROJECT THE TASK WAS READ IN, which is what makes a
// second walk over the same subtree harmless: a task somebody already carried
// is in another project by the time its step decides, and [Writer.UpdateTask]
// refuses a write naming the wrong one rather than re-keying it again.
func (w *Writer) moveOne(ctx context.Context, opID string, task Task,
	target string, former []string, mint *KeyMint, mark bool,
	notify *Notify) (WriteResult, error) {

	patch := TaskPatch{Project: &target, FormerKeys: &former, Mint: mint}
	if mark {
		patch.Moving = &mark
	}
	// WHETHER THE KEY THIS STEP LANDS THE TASK UNDER IS ANOTHER TASK'S,
	// read in the snapshot the step decides in — the key is the one the
	// applier composes from the mint, so no row holds it yet and the
	// directory is the only thing that can say. Assigned on every round;
	// the last is the one whose record was published.
	var key string
	if mint != nil {
		key = fmt.Sprintf("%s-%d", target, mint.N)
	}
	var collision bool
	// NO NOTIFICATION AND NO HISTORY BUMP on a descendant, whose caller
	// passes none: a subtree that moved wakes the people watching the
	// root, not everybody watching every task beneath it.
	result, err := w.updateTask(ctx, opID, task.ID, task.Project, NoIfMatch,
		patch, ChangeMoved, notify,
		func(ctx context.Context, tx *sql.Tx, _ Task, patch TaskPatch) (TaskPatch, error) {
			held, err := keyHeldByAnother(ctx, tx, key, task.ID)
			if err != nil {
				return TaskPatch{}, fmt.Errorf("tracker: read whether key %s "+
					"is already another task's: %w", key, err)
			}
			collision = held
			return patch, nil
		})
	result.KeyCollision = collision
	return result, err
}

// carriedTags is every tag the tasks a move carries hold, as their own project
// declares it — the label, colour and description a person chose — so the
// target is given the same grouping rather than a bare slug. A slug the source
// no longer declares (a set somebody edited after tagging) is carried as
// itself.
//
// INSIDE A READ BESIDE THE TASKS IT DESCRIBES — the move's pre-flight, or a
// later pass's ([Writer.followedTags]) — and in the source set's own order so
// two moves of the same subtree declare the same list.
func carriedTags(ctx context.Context, tx *sql.Tx, source string,
	tasks []Task) ([]Tag, error) {

	wanted := map[string]bool{}
	for _, task := range tasks {
		for _, slug := range task.Tags {
			wanted[slug] = true
		}
	}
	if len(wanted) == 0 {
		return nil, nil
	}
	set, _, err := readTagSet(ctx, tx, source)
	if err != nil {
		return nil, fmt.Errorf("tracker: read %s's tags for the move: %w",
			source, err)
	}
	var out []Tag
	for _, tag := range set.Tags {
		if wanted[tag.Slug] {
			out = append(out, Tag{Slug: tag.Slug, Label: tag.Label,
				Color: tag.Color, Description: tag.Description})
			delete(wanted, tag.Slug)
		}
	}
	rest := make([]string, 0, len(wanted))
	for slug := range wanted {
		rest = append(rest, slug)
	}
	slices.Sort(rest)
	for _, slug := range rest {
		out = append(out, Tag{Slug: slug, Label: slug})
	}
	return out, nil
}

// followedTags is [carriedTags] over the tasks a LATER pass carries — a re-run
// or the duty finishing a walk — each read against the project it is in now.
//
// A LATER PASS DECLARES WHAT IT CARRIES as the first run did, because what it
// carries is not always what the first run read: a task filed under the
// subtree behind the walk ([carriedAll]) is carrying whatever tags it was
// given, and moved without them it arrived holding slugs its new project had
// never declared — shown bare on every board, and refused back by the next
// edit of its tags.
func (w *Writer) followedTags(ctx context.Context, left []Task) ([]Tag, error) {
	byProject := map[string][]Task{}
	var projects []string
	for _, task := range left {
		if _, seen := byProject[task.Project]; !seen {
			projects = append(projects, task.Project)
		}
		byProject[task.Project] = append(byProject[task.Project], task)
	}
	slices.Sort(projects)
	var out []Tag
	seen := map[string]bool{}
	err := w.db.Replicated().Read(ctx, func(tx *sql.Tx) error {
		for _, project := range projects {
			tags, err := carriedTags(ctx, tx, project, byProject[project])
			if err != nil {
				return err
			}
			for _, tag := range tags {
				if !seen[tag.Slug] {
					seen[tag.Slug] = true
					out = append(out, tag)
				}
			}
		}
		return nil
	})
	return out, err
}

// tagsStep names the append that declares a set of carried tags, by the set.
//
// NAMED BY WHAT IT DECLARES, because a walk is finished by as many passes as it
// takes, each under the operation it was given — and a re-run's passes all
// share the caller's. A step named for its place would be answered, on the
// second pass, from the ledger row of the first pass's declaration, and the
// second pass's own tags would never be declared.
func tagsStep(tags []Tag) string {
	slugs := make([]string, 0, len(tags))
	for _, tag := range tags {
		slugs = append(slugs, tag.Slug)
	}
	slices.Sort(slugs)
	sum := sha256.Sum256([]byte(strings.Join(slugs, "\x00")))
	return "tags-" + hex.EncodeToString(sum[:8])
}

// claimAlias takes a former key, create-only, so the key keeps resolving.
//
// FIRST-WRITER-WINS ON ITS OWN SUBJECT, and a claim that loses is not an error:
// somebody already recorded this key's move, which is the fact the claim
// exists to establish.
func (w *Writer) claimAlias(ctx context.Context, opID, key, taskID string,
	at time.Time) (WriteResult, error) {

	subject := AliasSubject(key, 1)
	scope := ScopeSet{Subject: true}
	result, err := w.published(ctx, statelog.Request{
		Subject: wire(subject),
		Scope:   scope.Resolve(subject),
		OpID:    opID,
		Pattern: statelog.PatternCreate,
		Decide: func(tx *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			if owner, claimed, err := readAlias(ctx, tx, key); err != nil {
				return statelog.Decision{}, err
			} else if claimed && owner != taskID {
				return statelog.Decision{}, fmt.Errorf("tracker: key %s belongs "+
					"to task %s, so it cannot be aliased to %s", key, owner, taskID)
			}
			return w.decide(ctx, tx, stamp, subject, OpCreate, "", scope, opID, KeyAlias{
				Key: key, TaskID: taskID,
			}, nil, at)
		},
	})
	if errors.Is(err, statelog.ErrExists) {
		return result, nil
	}
	return result, err
}

// MergeDuplicates folds one task into another. SEQUENCE 13.
//
//	Rk, then take merge/<task> → A on the duplicate (the merge marker and
//	the `duplicates` relation) → per batch of ≤64: A re-parenting each
//	child onto Into → A clearing the marker and writing the cancelled
//	status → release.
//
// CRASH RESIDUE: children partly re-parented. REPAIRER: the tracker duty, once
// the claim this sequence heartbeats has lapsed — it finishes a walk only
// under that claim, never beside a live holder — and it is idempotent for the
// same reason the move's walk is: a child already carrying the new parent is
// not selected, so a completion writes only what is left. A child in the
// TRASH is not selected either, and stays under the duplicate: it is frozen,
// and a walk that tried to write it could never finish.
//
// THE MARKER IS CLEARED LAST. While it stands, the duplicate is visibly
// mid-merge rather than silently half-merged, which is the difference between
// a state somebody can wait out and one they have to reconstruct.
//
// WHAT NOTHING COULD FINISH IS REFUSED BEFORE THE MARK: a survivor in the
// trash, whichever way the subtasks go; moving the subtasks onto one of them,
// which would file one under itself; and moving them onto an item in another
// project ([ErrReparentAcrossProjects]). Each would be a mark the walk, the
// re-run and the duty all fail on for ever. And moving them while a
// cross-project move has not carried either end is UNAVAILABLE rather than
// refused ([refuseMidMove]), since which project each end is in is about to
// change. What arrives AFTER the mark is the duty's to wait out and name — see
// [mergeWaits].
func (w *Writer) MergeDuplicates(ctx context.Context, opID, duplicate, into string,
	reparent bool, notify *Notify) (WriteResult, error) {

	switch {
	case duplicate == "":
		return WriteResult{}, fmt.Errorf("tracker: a merge names no duplicate")
	case into == "":
		return WriteResult{}, fmt.Errorf("tracker: a merge names no canonical task")
	case duplicate == into:
		return WriteResult{}, fmt.Errorf("tracker: task %s cannot be a "+
			"duplicate of itself", duplicate)
	}
	claim, err := w.hold(ctx, mergeClaim(duplicate))
	if err != nil {
		return WriteResult{}, err
	}
	defer claim.release(ctx)

	var task Task
	if w.db == nil {
		return WriteResult{}, fmt.Errorf("tracker: this writer has no store, " +
			"so it cannot read the children a merge re-parents")
	}
	if err := w.db.Replicated().Read(ctx, func(tx *sql.Tx) error { //nolint:govet // shadow: scoped to this block; see .golangci.yml (trailing: covers this line only, not the closure)
		//nolint:govet // shadow: `x, err := f()` declares x too; see .golangci.yml
		current, held, err := readTask(ctx, tx, duplicate)
		switch {
		case err != nil:
			return err
		case !held:
			return absentTask(ctx, tx, duplicate, "task")
		case current.Removed != nil:
			return fmt.Errorf("tracker: task %s was removed by %s at %s; "+
				"restore it before merging it", duplicate,
				current.Removed.By, current.Removed.At.Format(time.RFC3339))
		}
		task = current
		survivor, held, err := readTask(ctx, tx, into)
		switch {
		case err != nil:
			return err
		case !held:
			return absentTask(ctx, tx, into, "task")
		case survivor.Removed != nil:
			// REFUSED BEFORE THE MARK, WHICHEVER WAY THE SUBTASKS GO.
			// Moving them, every re-parent is refused on its own
			// subject — a parent in the trash takes no new child — so
			// the mark would be one nothing can finish, retried by the
			// duty on every tick. Leaving them, the duplicate is
			// cancelled INTO an item nobody can see, so the one live
			// copy of the work is closed in favour of one in the trash.
			// Either way the survivor has to be live first.
			return fmt.Errorf("tracker: %s (%s) is in the trash — removed "+
				"by %s at %s; restore it before merging anything into it",
				survivor.Key, into, survivor.Removed.By,
				survivor.Removed.At.Format(time.RFC3339))
		case !reparent:
			return nil
		}
		// AND NOT INTO ITS OWN SUBTREE when the subtasks move: the child
		// the survivor sits under would be re-parented onto the survivor,
		// which [refuseParent] refuses as a cycle — so the mark would
		// stand with nothing to finish it.
		under, err := inSubtree(ctx, tx, duplicate, into)
		switch {
		case err != nil:
			return err
		case under:
			return fmt.Errorf("tracker: %s is one of %s's own subtasks, so "+
				"the duplicate's subtasks cannot move onto it — one of them "+
				"would end up under itself. Merge without moving the "+
				"subtasks to leave them where they are",
				survivor.Key, current.Key)
		}
		// ONLY WHAT THERE IS TO CARRY IS JUDGED BELOW: a duplicate with no
		// subtask in its own project moves nothing onto the survivor, so
		// neither which project the survivor is in nor whether a move is
		// still carrying either end is anything to refuse it over.
		kids, err := readChildBatch(ctx, tx, duplicate, current.Project, "", 1)
		if err != nil || len(kids) == 0 {
			return err
		}
		// NOR WHILE A MOVE HAS NOT CARRIED EITHER END, and asked before
		// the projects are compared, because one of them is about to
		// change: the walk carries whatever it read, so a subtask moved
		// off an uncarried duplicate would land in the move's project
		// under a survivor left in the old one, and one moved onto an
		// uncarried survivor is a child that walk never read. See
		// [refuseMidMove].
		if err = refuseMidMove(ctx, tx, current, "task",
			"its subtasks moved now would be split across two projects"); err != nil {
			return err
		}
		if err = refuseMidMove(ctx, tx, survivor, "task",
			"subtasks moved onto it now would be left behind"); err != nil {
			return err
		}
		if survivor.Project != current.Project {
			// A SUBTASK UNDER AN ITEM IN ANOTHER PROJECT is drawn under
			// that item on neither board and sits in the attention
			// queue as `inconsistent_project` — a state only a move of
			// its root clears, and a merge is not a move. Refused
			// before the first append, naming both ways out, rather
			// than left for somebody to find.
			return fmt.Errorf("tracker: task %s's subtasks are in %s and %s is "+
				"in %s, so re-parenting them onto it would file subtasks under "+
				"an item in another project: move %s into %s first — its "+
				"subtasks go with it — or merge without re-parenting them: %w",
				current.Key, current.Project, survivor.Key, survivor.Project,
				current.Key, survivor.Project, ErrReparentAcrossProjects)
		}
		return nil
	}); err != nil {
		return WriteResult{}, err
	}

	// THE EDGE IS A GESTURE, not a collection this sequence composes. The
	// read above is a PRE-FLIGHT — it happens before the first append and
	// outside every snapshot — so a set built from it and written whole
	// silently drops any relation a colleague added in between. An add is
	// resolved against the task's own rows inside the decide, which is the
	// one place a single consistent read of them exists.
	//
	// THE MARK CARRIES THE INTENT, because the duty that finishes this
	// walk if this process dies reads it off the task and has no other
	// way to know: a merge that declined to move the subtree and one that
	// crashed before moving its first child leave identical rows.
	merging := true
	marked, err := w.UpdateTask(ctx, stepID(opID, "mark"), duplicate, task.Project, NoIfMatch,
		TaskPatch{Relate: &RelationIntent{Add: []Relation{{
			Kind: RelationDuplicates, Other: into,
		}}}, Merging: &merging, MergeReparent: &reparent},
		ChangeRelations, nil)
	if err = resolved(fmt.Sprintf("task %s's merge marker", duplicate),
		marked, err); err != nil {
		return marked, err
	}

	if reparent {
		if _, err = w.reparentOnto(ctx, opID, duplicate, into); err != nil {
			return marked, err
		}
	}

	cancelled := StatusCancelled
	done := false
	// THE CLOSE LANDS ON THE SUBJECT THE MARK ALREADY MOVED, and the
	// re-parented children in between are on subjects of their own — so
	// the mark's position is what this last append has to see, whatever
	// the loop above published. See [Writer.After].
	closed, err := w.After(marked.Position).UpdateTask(ctx, stepID(opID, "close"),
		duplicate, task.Project, NoIfMatch,
		TaskPatch{Status: &cancelled, Merging: &done}, ChangeStatus, notify)
	return closed, resolved(fmt.Sprintf("task %s's close as a duplicate of %s",
		duplicate, into), closed, err)
}

// reparentOnto moves whatever is left of a duplicate's subtasks onto the
// canonical task, in batches of [WalkBatch].
//
// SHARED BY THE SEQUENCE AND THE DUTY, which is what [readChildBatch]'s
// selection buys: the duty's repair is a RE-RUN of the batch the holder did
// not reach, not a second algorithm that has to keep agreeing with this one.
// A merge's children are one of the walks [WalkBatch] is named for, and the
// value buys the same two things here as elsewhere — a bounded read whatever
// the subtree's size, and a bounded stretch between the claim's heartbeats.
//
// THE STEP ID IS KEYED ON THE CHILD rather than on its position in the walk,
// because a re-run's batches do not divide the same way: the moved ones are
// gone from the selection, so an index would give a different child the same
// operation id and the ledger would answer one move's question with another's
// row.
//
// ONLY A CHILD IN THE CANONICAL TASK'S OWN PROJECT IS SELECTED. The sequence
// refuses a merge that would carry one across ([ErrReparentAcrossProjects]),
// and this is what makes that an invariant rather than a check at the door: a
// subtask filed under the duplicate after that check, or a walk the duty
// finishes, never files a subtask under an item in another project. One that
// is not selected stays under the duplicate, where the trash's frozen children
// stay too.
func (w *Writer) reparentOnto(ctx context.Context, opID, duplicate, into string) (int, error) {
	project, err := w.taskProject(ctx, into)
	if err != nil {
		return 0, err
	}
	var moved int
	var after string
	for {
		var batch []Task
		if err := w.db.Replicated().Read(ctx, func(tx *sql.Tx) error {
			var err error
			batch, err = readChildBatch(ctx, tx, duplicate, project, after, WalkBatch)
			return err
		}); err != nil {
			return moved, err
		}
		if len(batch) == 0 {
			return moved, nil
		}
		for _, child := range batch {
			after = child.ID
			result, err := w.UpdateTask(ctx, stepID(opID, "c/"+child.ID),
				child.ID, child.Project, NoIfMatch, TaskPatch{Parent: &into},
				ChangeReparented, nil)
			// AN UNKNOWN CHILD STOPS THE WALK, so the close — which
			// takes the merge marker down — never runs over a subtask
			// that may still be under the duplicate.
			if err = resolved(fmt.Sprintf("subtask %s's move onto %s",
				child.ID, into), result, err); err != nil {
				// THE DUTY FINISHES IT ONCE THE MOVE CAN LAND, which is
				// not the same promise as "the duty finishes it": a
				// survivor that went to the trash after the pre-flight
				// takes no child until it is restored, one filed under
				// the duplicate since takes none of the subtasks above
				// it, and the duty leaves such a merge waiting —
				// reported, not retried — rather than failing on it
				// every tick. See [mergeWaits].
				return moved, fmt.Errorf("tracker: %d subtask(s) re-parented "+
					"onto %s and %s still has more, so it stays marked "+
					"mid-merge; the tracker duty moves the rest once the "+
					"move can land: %w", moved, into, duplicate, err)
			}
			moved++
		}
	}
}

// ErrReparentAcrossProjects refuses a merge that would re-parent a duplicate's
// subtasks onto an item in another project. See [Writer.MergeDuplicates].
var ErrReparentAcrossProjects = errors.New("tracker: a merge cannot carry " +
	"subtasks into another project")

// ErrBulkInFlight refuses a bulk gesture while another is applying.
var ErrBulkInFlight = errors.New("tracker: a bulk edit is already applying")

// UpdateTasks changes up to [MaxBulkTasks] tasks. SEQUENCE 26.
//
//	Take bulk/<domain> → Rs every listed task; pre-flight MaxBulkBytes
//	over the commits the patch will write → one A per DISTINCT task
//	subject, each at its own expectation, sharing one batch id.
//
// CRASH RESIDUE: a partial batch — some tasks committed, some not. REPAIRER:
// NOBODY, AND IT NEVER WAS ATOMIC. The result reports applied and failed per
// task and the caller re-runs.
//
// # Re-running it
//
// Under the SAME operation id, and each task's own step answers for that task:
// a change that landed is answered from the ledger, and only what did not is
// decided. The step is named by the TASK rather than by its place in the list,
// because the ledger answers a step's id with whatever record it recorded
// under it — so a step numbered by position answered a re-run that named the
// tasks in another order, or only the ones that failed, with ANOTHER task's
// record, and reported that task changed when nothing had touched it.
//
// A task whose outcome is UNKNOWN is a failure, not an application: its record
// may or may not be on the log, and the one answer that is true is that the
// re-run will say. Counting it applied told a caller to stop retrying a change
// that may never have landed.
//
// # Why it is admitted through a lease, and why that lease FAILS OPEN
//
// A maximal bulk edit occupies every peer's serial applier for seconds, during
// which every linearizable read fleet-wide is behind and every write is
// pending. The plan bounded how many rows one call may carry and nothing about
// how often — so six in a row are a rolling read outage on every node, which is
// what the admission bounds.
//
// It fails open because a refused bulk on a coordination blip is a seat told a
// colleague is editing when nobody is, and the log is CORRECT with two bulks in
// flight and merely slow. That is coordination's founding rule applied to
// admission: unknown is not "somebody else holds it".
//
// # What this costs the caller's NEXT write, stated because it is the ordinary case
//
// Every append here advances this caller's own session mark, so its next write
// on this stream — any subject — waits for the bulk to apply on this node and
// may be refused as behind, naming its own record. That is correct and it is
// what read-your-writes means: the alternative is a write that cannot see the
// write before it.
func (w *Writer) UpdateTasks(ctx context.Context, opID string, ids []string,
	project string, patch TaskPatch, kind ChangeKind,
	notify *Notify) (WriteResult, error) {

	switch {
	case len(ids) == 0:
		return WriteResult{}, fmt.Errorf("tracker: a bulk edit names no task")
	case len(ids) > MaxBulkTasks:
		return WriteResult{}, fmt.Errorf("tracker: a bulk edit names %d tasks "+
			"and one call carries at most %d — split it, and the second call "+
			"waits for the first to apply", len(ids), MaxBulkTasks)
	case project == "":
		return WriteResult{}, fmt.Errorf("tracker: a bulk edit names no project")
	}

	// DISTINCT SUBJECTS, in the caller's own order. One id named twice is
	// two appends on one subject at one expectation, so the second is
	// rejected against the first — a conflict the caller reads as a
	// colleague editing, produced entirely by its own list.
	seen := make(map[string]bool, len(ids))
	subjects := make([]string, 0, len(ids))
	for _, id := range ids {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		subjects = append(subjects, id)
	}

	// THE PRE-FLIGHT IS BEFORE THE FIRST APPEND. A bulk refused halfway
	// is a caller told "failed" about tasks that changed.
	body, err := json.Marshal(patch)
	if err != nil {
		return WriteResult{}, fmt.Errorf("tracker: encode the bulk patch: %w", err)
	}
	if total := len(body) * len(subjects); total > MaxBulkBytes {
		return WriteResult{}, fmt.Errorf("tracker: this patch over %d tasks "+
			"writes %d bytes of commits and a bulk edit carries at most %d — "+
			"the ceiling is on what the batch APPLIES, so a large patch takes "+
			"fewer tasks per call", len(subjects), total, MaxBulkBytes)
	}

	release, err := w.admit(ctx, len(subjects))
	if err != nil {
		w.count(metrics.TrackerBulkCalls, metrics.Attrs{"result": "refused"})
		return WriteResult{}, err
	}
	defer release()
	w.count(metrics.TrackerBulkCalls, metrics.Attrs{"result": "admitted"})

	result := WriteResult{Failed: map[string]string{}}
	for _, id := range subjects {
		one, err := w.UpdateTask(ctx, stepID(opID, "task-"+id),
			id, project, NoIfMatch, patch, kind, notify)
		switch {
		case err != nil:
			result.Failed[id] = err.Error()
			continue
		case one.Outcome == statelog.OutcomeUnknown:
			result.Failed[id] = fmt.Sprintf("whether this task's change landed "+
				"is unknown (operation %s); re-run the edit under the same "+
				"operation id, which answers what landed and applies what did "+
				"not", one.OpID)
			continue
		}
		result.Applied = append(result.Applied, id)
		result.Result = one.Result
	}
	return result, nil
}

// admit takes the fleet-wide bulk lease, or reports why it did not.
//
// THREE ANSWERS AND THREE BEHAVIOURS, which is why [Claims] is not a bool: a
// held lease runs, a peer's lease refuses with the holder's remaining time as
// the caller's hint, and a coordination store that cannot be reached ADMITS —
// see the sequence's own doc for why those last two must differ.
func (w *Writer) admit(ctx context.Context, rows int) (func(), error) {
	if w.claims == nil {
		return func() {}, nil
	}
	resource := bulkClaim(trackerStream)
	// TWICE THE PROJECTED APPLY TIME, so the lease outlives the work it
	// admits without outliving it by so much that a crashed holder blocks
	// the company. The projection is rows over the applier's own measured
	// drain, which is the same divisor every retry hint in this design is
	// computed from.
	projected := float64(rows) / w.drainRows()
	if w.metrics != nil {
		// THE PROJECTION IS WHAT IS SUMMED, not the wall clock: it is the
		// applier occupancy this call is about to impose on EVERY node,
		// and the wall clock here would measure only this one.
		w.metrics.AddValue(metrics.TrackerBulkApplySeconds, projected, nil)
	}
	ttl := 2 * time.Duration(projected*float64(time.Second))
	if ttl < ClaimHeartbeat {
		ttl = ClaimHeartbeat
	}
	// AN OWNER OF ITS OWN, for the reason [Writer.claimOwner] gives: under
	// the node id a second bulk on this node renewed the first one's lease
	// rather than being refused, and whichever finished first released the
	// other's.
	owner := w.claimOwner()
	lease, err := w.claims.TryAcquire(ctx, resource, coord.AcquireOptions{
		Owner: owner, TTL: ttl,
	})
	switch {
	case err != nil:
		// FAIL OPEN. See the doc above: the log is correct with two
		// bulks in flight and merely slow, and refusing here on an
		// unknown is a seat told a colleague is editing when nobody is.
		//nolint:nilerr // Deliberate fail-open: see the paragraph above.
		return func() {}, nil
	case lease == nil:
		remaining := time.Duration(0)
		if held, err := w.claims.Get(ctx, resource); err == nil && held != nil {
			remaining = time.Until(held.ExpiresAt)
		}
		return nil, fmt.Errorf("%w; retry in about %d seconds: %w",
			ErrBulkInFlight, int(max(remaining.Seconds(), 1)),
			statelog.ErrUnavailable)
	}
	epoch := lease.Epoch
	return func() {
		_, _ = w.claims.Release(context.WithoutCancel(ctx), resource, owner, epoch)
	}, nil
}

// drainRows is the applier's measured rows a second, and the divisor of every
// projection computed from it.
//
// A FLOOR RATHER THAN A DEFAULT, because dividing by a drain that has not been
// measured yet — zero — is an infinite lease, which is worse than any wrong
// number. One row a second is deliberately pessimistic: it makes the first
// projection too long rather than too short, and a lease held too long delays a
// caller where one held too briefly admits the concurrency it exists to stop.
func (w *Writer) drainRows() float64 {
	if w.Drain == nil {
		return 1
	}
	if rows := w.Drain(); rows > 1 {
		return rows
	}
	return 1
}
