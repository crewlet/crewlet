package worklet

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

	"github.com/crewlet/crewlet/internal/caveat"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/words"
)

// The five cross-object sequences: a create (1), a cross-project move (7) and
// a merge (13) here, a dependency in depend.go, a board drag across lanes in
// place.go. They cannot be one append like every other write, because they
// mint from one object and write another (a counter's number lands on a task).
//
// Each states its ORDER of appends, the DURABLE CLAIM that stops two nodes
// running it at once, the CRASH RESIDUE that order leaves, and the REPAIRER (or
// why none is needed). A create mints before filing (a gap, never a shared
// key); a move writes the alias before re-keying (a pasted key keeps
// resolving); a drag writes status first (the half if_match guards). Every step
// is a record on the log, never a row: nothing to roll back, and a re-run that
// re-derives the same identities is refused rather than duplicated.

const (
	// MaxTasksNamed is how many other tasks one call may name: a dependency
	// change's counterparties ([Writer.Depend]) and a rank order's own moves,
	// beside the neighbours a re-spread adds ([Writer.MoveTasks]). The same 64
	// as [WalkBatch] and [MaxScopeTerms]: how many objects one step may name
	// before the work is a batch rather than a gesture.
	MaxTasksNamed = 64

	// WalkBatch is one batch of a paced walk — a merge's children, a
	// re-spread's placements — and of every duty selection that finishes
	// one. Not a move's: that walk is one append per descendant of a
	// subtree it read whole, bounded by [MaxDescendants].
	WalkBatch = 64

	// ClaimTTL is how long a walking sequence's durable claim survives
	// unrenewed: four heartbeats, so three misses are survivable and the
	// fourth hands the walk to the duty. The lease's expiry is the only
	// staleness there is: the duty finishes a walk only under its claim, so a
	// second "abandoned" threshold would be an opinion the lease ignores.
	ClaimTTL = 60 * time.Second

	// ClaimHeartbeat is how often the holder renews that claim: a quarter of
	// [ClaimTTL].
	ClaimHeartbeat = 15 * time.Second
)

// Claims is the coordination a multi-append sequence takes its claim from:
// the four [coord.Backend] methods this package uses. Three-valued on purpose:
// a lease is held, (nil, nil) says a peer holds it, an error says nothing is
// known. A walk fails closed on both of the last two and tells its caller
// which ([Writer.hold]): a peer's hold is the walk already running, an unknown
// is a store that did not answer.
type Claims interface {
	TryAcquire(ctx context.Context, resource string, opts coord.AcquireOptions) (*coord.Lease, coord.Refusal, error)
	Renew(ctx context.Context, resource, owner string, epoch int64, ttl time.Duration) (bool, error)
	Release(ctx context.Context, resource, owner string, epoch int64) (bool, error)
	Get(ctx context.Context, resource string) (*coord.Lease, error)
}

// The claim names, one function each because a claim spelled two ways is two
// claims and admits the concurrency it was taken to exclude. Built through
// [coord.Class] like every lease, since they share the fleet's store and its
// one naming grammar; the class is the key's leading subject token.
//
// Held to [coord.MaxDutyTTL], never the seat lease TTL ([coord.HeldToSeatTTL]),
// so [ClaimTTL] moves with nothing an operator tunes for failover.
const (
	classMove  coord.Class = "move"
	classMerge coord.Class = "merge"
)

func moveClaim(task string) string  { return classMove.Resource(task) }
func mergeClaim(task string) string { return classMerge.Resource(task) }

// stepID derives one append's operation id from the gesture's own: stable and
// distinct. The ledger dedupes one RECORD and a sequence publishes several, so
// a shared id would answer the second append from the first's row; an id
// minted per attempt would defeat the ledger in the lost-acknowledgement case.
//
// It is the state log's grammar ([statelog.StepOpID]) so the ledger reads the
// gesture's mint instant off it; any other shape reads as minted at the zero
// instant and is answered `unknown` wherever the ledger's sweep lost a row.
func stepID(opID, step string) string { return statelog.StepOpID(opID, step) }

// ErrStepUnresolved reports a walking sequence that stopped at a step whose
// outcome is UNKNOWN: the steps before it landed, it may or may not be on the
// log, and nothing after it was written. The caller re-runs the gesture under
// the SAME operation id, and every step answers for itself (each sequence's
// REPAIRER).
var ErrStepUnresolved = errors.New("worklet: a step's outcome is unresolved")

// ErrStepUnvouched reports a walking sequence stopped at a step whose outcome
// is unknown because this node's operation ledger cannot vouch for it
// ([statelog.Result.Unvouched]): the step was minted before the instant the
// ledger may have lost rows from, and it holds no row for it.
//
// It is an [ErrStepUnresolved] (errors.Is answers true for both) that a retry
// on THIS node never finishes, since the missing row is the one lost; another
// node, or one whose ledger lost nothing that far back, can.
var ErrStepUnvouched = fmt.Errorf("worklet: this node's operation ledger "+
	"cannot vouch for a step: %w", ErrStepUnresolved)

// resolved reads one step of a walking sequence: a step that failed stops the
// walk, and so does one whose outcome is UNKNOWN. [Writer.UpdateTask] answers
// unknown with a nil error, so checking only the error would carry on over a
// step nobody can vouch for (descendants moved under a root that may not have
// moved) and report success. Stopping leaves the walk's mark up, which hands
// the remainder to the re-run or the duty. An unknown this node's ledger
// cannot vouch for says so ([ErrStepUnvouched]): a re-run here never ends it.
func resolved(step string, result WriteResult, err error) error {
	switch {
	case err != nil:
		return err
	case result.Outcome == statelog.OutcomeUnknown && result.Unvouched:
		return fmt.Errorf("worklet: whether %s landed cannot be told on this "+
			"node (operation %s): its operation ledger may have lost the record "+
			"of it, so the walk stopped there and a re-run here stops at the same "+
			"step; another node, or one whose ledger lost nothing that far back, "+
			"can finish it under the same operation id: %w",
			step, result.OpID, ErrStepUnvouched)
	case result.Outcome == statelog.OutcomeUnknown:
		return fmt.Errorf("worklet: whether %s landed is unknown (operation "+
			"%s), so the walk stopped there; re-run it under the same "+
			"operation id, which answers what landed and carries on from it: %w",
			step, result.OpID, ErrStepUnresolved)
	}
	return nil
}

// WriteResult is what a tracker write returns: the framework's three-valued
// outcome UNCHANGED (applied, pending or unknown), plus what only this domain
// can say: what a mint produced, and what the caller should know but was not
// refused for.
type WriteResult struct {
	statelog.Result

	// Key and Rank are what a create or a move minted. Empty on every
	// other path.
	Key  string
	Rank Rank

	// KeyCollision says Key opens ANOTHER task, one that claimed it first
	// (what a counter restored beside newer work mints), so this write's task
	// is reached only by its id ([ItemAddress]). Read in the snapshot that
	// decided or read back the write ([keyHeldByAnother]); false wherever Key
	// is empty. On the receipt because a caller handed the key alone acts on
	// the claimant with its next call.
	KeyCollision bool

	// Assignee is who a create filed the task to: its named assignee, else
	// the project's default ([Writer.landsOn]), else empty for triage. Only
	// the create's snapshot decides the default, so a caller answering from
	// what it SENT would misreport where the work went. Empty elsewhere.
	Assignee string

	// Declared are the tags a write brought into existence, reported from
	// inside the snapshot that decided them rather than from a read before
	// it — so a caller is never told it created a word a colleague
	// declared a moment earlier.
	Declared []string

	// Posted is the remark a post found already on its task under its id
	// (the same call, made again), answered from that row with nothing
	// published ([PostedRemark]). Read in the deciding snapshot, because a
	// read before the write misses a first attempt that landed after it.
	// Nil on every other path.
	Posted *PostedRemark

	// Caveats are what the caller should know and was not refused for: a
	// body carrying more task keys than the applier resolves, a tag that
	// nearly matched an existing one. The apply must never depend on one:
	// every data node applies the record, and only this node saw the caveat.
	// Each carries a model's phrasing and a person's ([caveat.Caveat]); the
	// estate router rebuilds them from a peer's JSON answer
	// ([caveat.RebuildFromPeer], ADR-0060).
	Caveats []caveat.Caveat
}

// CreateTask mints a task. SEQUENCE 1: read the project and its catalogues
// (refuse an archived project, check required fields), append on the counter
// at its anchor, then on the task at expectation 0 with rank = intPart(n).
//
// CRASH RESIDUE: a numbering gap, which nobody repairs (as in Jira); minting
// the task first would let two tasks share a key. The task id MUST derive from
// the operation id (as the builtin's does), else a retry is a second task the
// broker cannot refuse. A retry whose counter step landed asks for the task
// step ([Writer.resumeCreate]), filing on a fresh mint only if none landed. A
// task already under the id (a rebased re-run, ADR-0029) is answered from its
// row before the mint, spending no number. The rank is the counter value:
// unique, the new maximum, never a drag's (fractional or below [RankOrigin]).
func (w *Writer) CreateTask(ctx context.Context, opID string, task Task,
	notify *Notify) (WriteResult, error) {

	return w.createTask(ctx, opID, task, nil, notify)
}

// CreateTaskAsking is [Writer.CreateTask] for an item filed AS A QUESTION: the
// task and its ask (a [Comment] with `ask` set, and a [Decision] when somebody
// must choose) land in ONE record. Filed as two, a crash losing the comment
// leaves a task with nobody asked: it wakes its assignee as work,
// `asked_of_me` never lists it, and the answer has no ask to close.
//
// The ask is the question itself, so it answers, replies to and chooses
// nothing: the task holds no comment yet. The person asked starts following
// the item ([followAuto]).
func (w *Writer) CreateTaskAsking(ctx context.Context, opID string, task Task,
	ask Comment, notify *Notify) (WriteResult, error) {

	return w.createTask(ctx, opID, task, &ask, notify)
}

// createTask is sequence 1, with or without the ask a create may carry.
func (w *Writer) createTask(ctx context.Context, opID string, task Task,
	ask *Comment, notify *Notify) (WriteResult, error) {

	// The task as handed in, which its wake was built from; what is settled
	// below is brought onto the wake once filed ([Notify.settled]).
	handed := task
	switch {
	case task.ID == "":
		return WriteResult{}, invalid("a create names no task id")
	case task.Project == "":
		return WriteResult{}, invalid("task %s names no project "+
			"— a task's scope path sits under its project's, so one without a "+
			"project files its deferral where no project-scoped probe looks",
			task.ID)
	}
	if ask != nil {
		// A COPY, so the instant this write stamps on the question does
		// not reach the caller's value.
		copied := *ask
		ask = &copied
	}
	// Before any read: a cap is a property of the value, refused the same on
	// every node, so a transaction would buy nothing.
	if err := checkTextCaps(&task.Title, &task.Body, nil); err != nil {
		return WriteResult{}, err
	}
	// The instants too, for the same reason ([Task.CheckInstants]).
	if err := task.CheckInstants(); err != nil {
		return WriteResult{}, err
	}
	if ask != nil {
		// Before the mint too: a malformed question refused after the counter
		// moved is a needless numbering gap.
		if err := ask.CheckInstants(); err != nil {
			return WriteResult{}, err
		}
		if err := checkCreateAsk(&task, ask); err != nil {
			return WriteResult{}, err
		}
	}
	// Before the mint, because the catalogue check runs inside it; the other
	// defaults are applied after the key and nothing validates them.
	if task.Type == "" {
		task.Type = DefaultTaskType
	}
	// Tags are normalised here (a property of the argument) and checked
	// against the project's set inside the mint's snapshot (a property of the
	// project, the only half needing a read).
	tags, err := normaliseTags(task.Project, task.Tags)
	if err != nil {
		return WriteResult{}, err
	}
	task.Tags = tags

	// The LAST run of the closure is the one whose mint was accepted, so
	// every field of settled is assigned, never appended to.
	var settled settledCreate
	n, minted, err := w.mintKey(ctx, stepID(opID, "counter"), task.Project, 1,
		w.settleCreate(ctx, task, &settled))
	switch {
	case errors.Is(err, errMintLanded):
		return w.resumeCreate(ctx, opID, task, handed, ask, notify)
	case errors.Is(err, errTaskFiled):
		return w.filedTask(ctx, opID, task.ID)
	case minted.Outcome == statelog.OutcomeUnknown && minted.Unvouched:
		return w.unvouchedCreate(ctx, opID, task, minted)
	case err != nil:
		return WriteResult{Result: minted}, err
	}
	return w.fileTask(ctx, opID, task, handed, ask, n, settled, notify)
}

// unvouchedCreate answers a create whose counter step this node's operation
// ledger cannot vouch for ([statelog.Result.Unvouched]). It is not "not made":
// the operation predates rows the ledger may have lost (a turn re-run after
// the thirty-day sweep, a month-old `unknown` finished), so its first run may
// have filed the task; told "not made", a seat files a duplicate.
//
// The task's own row can say, since [Task.ID] derives from the operation: a
// row is this operation's task, answered with its key as a resumed create
// would. No row is not proof of absence (the task may be on a node this one
// lags), so it answers `unknown`, still unvouched, with no error.
func (w *Writer) unvouchedCreate(ctx context.Context, opID string, task Task,
	minted statelog.Result) (WriteResult, error) {

	if w.db.IsZero() {
		return WriteResult{Result: minted}, nil
	}
	var held bool
	if err := w.db.Read(ctx, func(tx *sql.Tx) error {
		var err error
		_, held, err = readTask(ctx, tx, task.ID)
		return err
	}); err != nil {
		return WriteResult{Result: minted}, fmt.Errorf("worklet: the create of %s "+
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

// fileTask is a create's second append, on the key number n its mint took —
// carrying ask, the question the task is filed as, where it is one. handed is
// the task as the create's caller handed it in, which its wake was built from.
func (w *Writer) fileTask(ctx context.Context, opID string, task, handed Task,
	ask *Comment, n uint64, settled settledCreate, notify *Notify) (WriteResult, error) {

	if settled.fields != nil {
		task.Fields = settled.fields
	}
	task.FiledUnit, task.RoutingUnit = filedUnit(
		task.FiledUnit, task.RoutingUnit, settled.unit)
	if filed := w.landsOn(task, settled.assignee); filed.assignee != task.Assignee {
		task.Assignee = filed.assignee
		// The assignee watches what they hold, as a named assignee does.
		task.Watchers, _ = followAuto(task.Watchers, task.Muted, task.Assignee)
	} else if !filed.caveat.IsZero() {
		settled.caveats = append(settled.caveats, filed.caveat)
	}
	rank, err := IntegerAt(n)
	if err != nil {
		return WriteResult{}, fmt.Errorf("worklet: derive %s-%d's rank from the "+
			"counter value it minted: %w", task.Project, n, err)
	}
	at := w.Now()
	task.Key = mintedKey(task.Project, n)
	task.Rank = rank
	task.CreatedAt, task.UpdatedAt = at, at
	if task.Status == "" {
		task.Status = StatusTodo
	}
	task.StatusGroup = task.Status.Group()
	if task.Priority == "" {
		task.Priority = PriorityNone
	}
	// EVERY SETTLEMENT IS MADE, so the wake is brought up to all of them at
	// once: what the record carries, its wake names.
	notify.settled(handed, task, w.Leads)

	if ask != nil {
		// THE TASK'S OWN INSTANT, so the question is not older than the
		// item it was filed on.
		ask.CreatedAt = at
	}
	result, err := w.writeTask(ctx, stepID(opID, "task"), task, ask, notify, at)
	if errors.Is(err, statelog.ErrExists) {
		// THE TASK WAS FILED BETWEEN THE MINT AND THIS APPEND, under the
		// create's own id — see [Writer.CreateTask], "A create whose task
		// is already filed" — so it is this create's, and n is a gap.
		return w.filedTask(ctx, opID, task.ID)
	}
	if err == nil && result.Collapsed {
		// The task step landed under an earlier copy of this operation (met on
		// the task's subject after a fresh mint), so the task is that copy's and
		// n is this retry's gap.
		return w.landedTask(ctx, result.Result, task.ID)
	}
	result.Key, result.Rank = task.Key, task.Rank
	result.Assignee = task.Assignee
	result.Caveats = append(result.Caveats, settled.caveats...)
	return result, err
}

// resumeCreate finishes a create whose counter step already landed under its
// operation id. The task step is asked for first and only answered: deciding
// it needs the number the earlier counter step took, so its Decide refuses
// and only the ledger can answer it. Minting first would spend a number on
// every retry of a finished create.
//
// Only a task that has not applied here is filed on a fresh mint under an
// operation of its own; the earlier number is the documented gap. If the task
// landed but has not applied here yet, the fresh mint costs only a number: its
// task step is refused expectation zero and answered from the ledger once
// this node catches up ([Writer.fileTask]).
func (w *Writer) resumeCreate(ctx context.Context, opID string, task, handed Task,
	ask *Comment, notify *Notify) (WriteResult, error) {

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
		// The ledger cannot vouch for the task step, so its silence is not "not
		// applied" and filing now could file twice.
		return WriteResult{Result: answered}, nil
	case err == nil:
		return w.landedTask(ctx, answered, task.ID)
	case !errors.Is(err, errTaskNotApplied):
		return WriteResult{Result: answered}, err
	}

	var settled settledCreate
	n, minted, err := w.mintFresh(ctx, task.Project, 1,
		w.settleCreate(ctx, task, &settled))
	switch {
	case errors.Is(err, errTaskFiled):
		return w.filedTask(ctx, opID, task.ID)
	case err != nil:
		return WriteResult{Result: minted}, err
	}
	return w.fileTask(ctx, opID, task, handed, ask, n, settled, notify)
}

// errTaskFiled is a create's mint finding the task already filed under the
// create's own id: the create was made before, and is answered with its task
// ([Writer.filedTask]) rather than minted again.
var errTaskFiled = errors.New("worklet: the create's task is already filed")

// filedTask answers a create whose task is already filed under its id — see
// [Writer.CreateTask], "A create whose task is already filed" — from the
// task's row: applied, with nothing published, under the key the task holds.
func (w *Writer) filedTask(ctx context.Context, opID, id string) (WriteResult, error) {
	return w.landedTask(ctx, statelog.Result{
		Outcome: statelog.OutcomeApplied, OpID: stepID(opID, "task"),
	}, id)
}

// errTaskNotApplied is a resumed create's task step finding no ledger row to
// answer it: the task has not applied on this node.
var errTaskNotApplied = errors.New("worklet: the create's task has not applied here")

// landedTask is the answer to a create whose task step already landed: the
// ledger's position, and the key and rank the task was filed under, read off
// its row.
//
// A TASK THAT IS NO LONGER HERE — purged since — still landed, so the
// outcome stands and the missing key is a warning rather than a refusal.
func (w *Writer) landedTask(ctx context.Context, result statelog.Result,
	id string) (WriteResult, error) {

	out := WriteResult{Result: result}
	if w.db.IsZero() {
		out.Caveats = append(out.Caveats, caveat.FiledEarlierUnread(id))
		return out, nil
	}
	var task Task
	var held, collision bool
	if err := w.db.Read(ctx, func(tx *sql.Tx) error {
		var err error
		task, held, err = readTask(ctx, tx, id)
		if err != nil || !held {
			return err
		}
		collision, err = keyHeldByAnother(ctx, tx, task.Key, id)
		return err
	}); err != nil {
		return out, fmt.Errorf("worklet: task %s was filed by an earlier copy of "+
			"this operation; read its key: %w", id, err)
	}
	if !held {
		out.Caveats = append(out.Caveats, caveat.FiledEarlierRemoved(id))
		return out, nil
	}
	// AND WHO HOLDS IT, as a create's own answer does ([WriteResult.Assignee]):
	// the retry is told where the work sits, never the assignee it SENT.
	out.Key, out.Rank, out.KeyCollision = task.Key, task.Rank, collision
	out.Assignee = task.Assignee
	return out, nil
}

// filedUnit is which team a new task belongs to, and which team's lead hears
// about it. With no unit named, the project's own unit (from the chart apply,
// on the project row): `filed_unit` is what `unit=` filters, a board's `unit`
// axis groups and the dashboard's "Filed into" shows. Derived here, from a row
// the decide's transaction reads, so every surface gets the same answer (an
// operator or a root-level seat has no team to stamp) and no writer files
// against a chart its peers have not applied.
//
// An explicit unit is left alone: work belonging to another team than the
// project's is what `create_work_item`'s `unit` is for. Routing follows the
// filed unit unless the writer named its own, so the lead who hears is the
// lead of the unit the work belongs to.
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

// landing is who a new task is filed to, and what the caller is told about it.
type landing struct {
	assignee string
	caveat   caveat.Caveat
}

// landsOn is who a new task is assigned to: with nobody named, the project's
// `default_assignee` ("who unassigned work lands on"), as `write_project` and
// the dashboard's New task sheet promise. Decided in the create's own
// snapshot, beside [filedUnit] and for its reason. A named assignee is left
// alone, and so is an empty default (the project's choice of triage).
//
// A default that no longer names a seat is not applied: the chart apply that
// removed the seat does not rewrite the project, and a task filed to it would
// wake nobody. It lands in triage, where the lead is told, with a caveat
// naming the setting. [Writer.World] must answer with the SAME handle, since
// its colleague match would turn a departed `ana` into `anabel`; with no
// World the stored handle, checked when it was set, is trusted.
func (w *Writer) landsOn(task Task, projectDefault string) landing {
	if task.Assignee != "" || projectDefault == "" {
		return landing{assignee: task.Assignee}
	}
	if world := w.World; world != nil {
		if seat, found := world.ResolveSeat(projectDefault); !found || seat != projectDefault {
			return landing{caveat: caveat.DefaultAssigneeGone(task.Project,
				projectDefault)}
		}
	}
	return landing{assignee: projectDefault}
}

// checkCreateAsk is what the ask a create carries must be, on its own: a
// question, put to somebody, on this task, answering and replying to nothing.
// It also puts the person asked on the new task's watchers, which is what an
// ask does wherever it is written — see [followAuto].
func checkCreateAsk(task *Task, ask *Comment) error {
	switch {
	case ask.ID == "":
		return invalid("the ask filed with task %s names no comment "+
			"id — the id is the row's key on every data node, so it is "+
			"derived by the writer rather than minted at apply", task.ID)
	case ask.Ask == "":
		return invalid("the comment filed with task %s asks nobody — "+
			"a create carries a comment only as the question it was filed as",
			task.ID)
	case ask.Answers != nil, ask.Choice != "", ask.ReplyTo != nil:
		return invalid("the ask filed with task %s answers or replies "+
			"to a comment, and a task that does not exist yet has none", task.ID)
	case ask.Task != "" && ask.Task != task.ID:
		return invalid("the ask filed with task %s names task %s",
			task.ID, ask.Task)
	}
	ask.Task = task.ID
	if err := checkTextCaps(nil, nil, &ask.Body); err != nil {
		return err
	}
	if err := checkCommentShape(task.ID, ask); err != nil {
		return err
	}
	if watchers, ok := followAuto(task.Watchers, task.Muted, ask.Ask); ok {
		task.Watchers = watchers
	}
	return nil
}

// TaskCreate is a create record's payload: the whole task, and the question it
// was filed as when it was. The task is embedded, so a create without an ask
// encodes as a bare [Task] and every reader decoding one still reads it; the
// comment is its own row on apply, so it rides beside the task, not on it.
type TaskCreate struct {
	Task
	Comment *Comment `json:"comment,omitempty"`

	// Origin is the chat surface and conversation the filing turn was woken
	// on — see [Origin]. Stated by the writer from its provenance
	// ([Writer.Origin]); absent for a create nothing on a chat surface
	// caused.
	Origin *Origin `json:"origin,omitempty"`
}

// taskPresentStatement is [Writer.writeTask]'s read of a create's guarding
// row, named for the plan test.
const taskPresentStatement = `SELECT COUNT(*) FROM tracker_tasks WHERE id = ?`

// writeTask is sequence 1's second append: a whole task at expectation zero,
// guarded by its own row — carrying the ask the task was filed as, where it is
// one.
func (w *Writer) writeTask(ctx context.Context, opID string, task Task,
	ask *Comment, notify *Notify, at time.Time) (WriteResult, error) {

	subject := TaskSubject(task.ID)
	// The task and every key it carries: the apply claims each unclaimed key
	// in log order, so a node retaining another create of the same key would
	// otherwise give the claim to whichever applied first ([keyTerm]). The
	// keys are fixed before the request, so the scope cannot fall short.
	scope := ScopeSet{Subject: true, Container: task.Project}.
		withKeys(task.ID, keysOf(task)).capped()
	// WHETHER THE KEY THIS CREATE TOOK IS ANOTHER TASK'S, from the snapshot
	// the last round decided in — the round whose record was published.
	var collision bool
	// THE KEY THIS WRITE MINTED goes on the wake it publishes: the caller
	// built the wake from a task that had none yet (see [Notify.keyed]).
	notify.keyed(task.Key)
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
			if err := tx.QueryRowContext(ctx, taskPresentStatement, task.ID).
				Scan(&present); err != nil {
				return statelog.Decision{}, fmt.Errorf("worklet: read the "+
					"guarding row for %s: %w", task.ID, err)
			}
			if present > 0 {
				return statelog.Decision{}, statelog.ErrExists
			}
			held, err := keyHeldByAnother(ctx, tx, task.Key, task.ID)
			if err != nil {
				return statelog.Decision{}, fmt.Errorf("worklet: read whether "+
					"key %s is already another task's: %w", task.Key, err)
			}
			collision = held
			return w.decide(ctx, tx, stamp, subject, OpCreate, ChangeCreated, scope, opID,
				TaskCreate{Task: task, Comment: ask, Origin: w.origin()}, notify, at)
		},
	})
	return WriteResult{
		Result: result, Caveats: bodyCaveats(task.Body), KeyCollision: collision,
	}, err
}

// mintKey takes the next k key numbers in a project: SEQUENCE 1's first
// append, and 7's. The number is a record, not a row read: two nodes reading
// the counter would mint two tasks with one key, while a conditional append on
// the counter's subject rejects the loser, which re-decides and takes the
// next. A RANGE of k, because a cross-project move re-keys a whole subtree and
// one append per descendant would load the project's busiest subject.
func (w *Writer) mintKey(ctx context.Context, opID, project string, k int,
	guard func(*sql.Tx) error) (uint64, statelog.Result, error) {

	if k < 1 {
		return 0, statelog.Result{}, invalid("a key mint takes %d "+
			"numbers, and a mint of none moves the counter for nothing", k)
	}
	subject := CounterSubject(project)
	scope := ScopeSet{Subject: true}
	at := w.Now()

	// Captured, not read back: the framework re-decides on a rejected append,
	// so only the closure sees the number the final decision took; the row
	// afterwards holds whatever the fleet has minted since.
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
		// An unknown mint cannot be built on: the number may not be ours, and a
		// task on a number somebody else holds is two tasks with one key, the
		// one thing this order exists to prevent.
		return 0, result, fmt.Errorf("worklet: the key mint for %s is "+
			"unresolved, so the number it took cannot be built on: %w",
			project, statelog.ErrUnavailable)
	}
	if result.Collapsed {
		// Nor can a mint answered rather than decided: the counter moved under
		// an earlier copy, and `base` is a number the counter never recorded. A
		// create resumes from its task step instead ([Writer.resumeCreate]);
		// every other caller refuses.
		return 0, result, fmt.Errorf("worklet: the key mint for %s already "+
			"landed under operation %s, and the number it took is the earlier "+
			"copy's, so it cannot be built on: %w", project, opID, errMintLanded)
	}
	return base, result, nil
}

// mintFresh takes k numbers under an operation minted for this call alone:
// the mint a create resumes on, and a cross-project move's re-mint for what
// its first range could not carry. A mint whose acknowledgement was lost is
// answered from the ledger, which cannot say whose copy it names, so it is
// reported collapsed ([errMintLanded]) and minted again under another fresh
// operation, leaving a gap. Bounded at [freshMintAttempts].
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
	return 0, minted, fmt.Errorf("worklet: %d fresh key mints for %s in a row "+
		"were answered from a copy nobody can prove was theirs — each is a lost "+
		"acknowledgement, which is a broker that is dropping them: %w",
		freshMintAttempts, project, err)
}

// freshMintAttempts is how many fresh mints [Writer.mintFresh] makes before it
// gives up. Three: only a lost acknowledgement fails one, two in a row on one
// subject is already unusual, and a third is a failing broker a fourth mint
// does not fix, while each attempt spends a number the project never regains.
const freshMintAttempts = 3

// errMintLanded is a key mint answered from an earlier copy of its operation.
// Unavailable, because to a caller that cannot resume it is exactly that: the
// number is on the log and not in this call.
var errMintLanded = fmt.Errorf("the mint's number is an earlier copy's: %w",
	statelog.ErrUnavailable)

// settledCreate is what a create's own snapshot decided: the values only rows
// can settle, carried out to the record the second append publishes. One
// struct, assigned whole on each run of the closure, so no caller pairs one
// run's answer with another's.
type settledCreate struct {
	// fields are the task's custom-field values in their canonical form,
	// nil when nothing needed settling.
	fields map[string]json.RawMessage

	// caveats are what the writer is told and was not refused for.
	caveats []caveat.Caveat

	// unit is the project's chart-owned unit, which a task naming none is
	// filed into. Empty for a project the chart gave no unit (a root-level
	// project), honestly rather than by default.
	unit string

	// assignee is the project's DEFAULT ASSIGNEE, which a task that names
	// nobody is filed to — see [landsOn]. Empty is the project's own
	// setting for triage.
	assignee string
}

// refuseCreate is what sequence 1 reads the project and its catalogues for.
// It also COERCES the task's custom-field values against the declarations in
// this snapshot (an option spelling to its id, a timestamp on a date-only
// field to its date), so the record carries the canonical form and every node
// writes identical rows without re-deciding. The project's unit ([filedUnit])
// and default assignee ([landsOn]) ride back out from the same row.
func (w *Writer) refuseCreate(ctx context.Context, tx *sql.Tx, task Task) (
	settledCreate, error) {

	// A task already filed under this create's id is the create made before,
	// asked first: a project archived since is no reason to refuse the create
	// that filed it ([Writer.CreateTask]).
	var present int
	if err := tx.QueryRowContext(ctx, taskPresentStatement, task.ID).
		Scan(&present); err != nil {
		return settledCreate{}, fmt.Errorf("worklet: read whether task %s is "+
			"already filed: %w", task.ID, err)
	}
	if present > 0 {
		return settledCreate{}, errTaskFiled
	}
	project, held, err := readProject(ctx, tx, task.Project)
	switch {
	case err != nil:
		return settledCreate{}, err
	case !held:
		return settledCreate{}, fmt.Errorf("worklet: project %s is not on this "+
			"node: %w", task.Project, statelog.ErrUnavailable)
	case project.Archived:
		return settledCreate{}, refusedByState("project %s is archived, so "+
			"it takes no new work. %s", task.Project, unarchiveProject)
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
	fields, caveats, err := settleFields(ctx, tx, task.Project, task.Type,
		task.Fields, w.World, w.zone())
	if err != nil {
		return settledCreate{}, err
	}
	return settledCreate{fields: fields, caveats: caveats, unit: project.Unit,
		assignee: project.DefaultAssignee}, nil
}

// refuseParent refuses a parent a task cannot be filed under:
//   - one in another project: a board draws a project's roots with their
//     subtrees, so a subtask elsewhere is on neither board; only a move of the
//     whole subtree changes project ([Writer.MoveTaskToProject]).
//   - one beneath a task marked mid-move that the walk has not carried
//     ([refuseMidMove]), asked first so a caller naming the destination is
//     told to come back; [carriedAll] catches a node behind the mark.
//   - one in the trash: a restore brings back only what the removal took, so
//     a child filed between would stay live under a parent nobody sees.
//   - the task itself or its descendant: a cycle the applier would apply,
//     dropping the subtree off every board.
//   - an absent one, which is one of two facts ([missingTask]).
func refuseParent(ctx context.Context, tx *sql.Tx, task Task, parentID string) error {
	parent, held, err := readTask(ctx, tx, parentID)
	switch {
	case err != nil:
		return err
	case !held:
		return missingTask(ctx, tx, parentID, "nothing can be filed under it")
	case parent.Removed != nil:
		return invalid("parent task %s (%s) was removed by %s at "+
			"%s; restore it before filing anything under it", parent.Key,
			parent.ID, parent.Removed.By, parent.Removed.At.Format(time.RFC3339))
	}
	if err = refuseMidMove(ctx, tx, parent, "parent task",
		"a task filed under it now would be left behind"); err != nil {
		return err
	}
	if parent.Project != task.Project {
		return invalid("task %s is filed under %s and its parent "+
			"%s under %s — a subtask lives in its parent's project, which is "+
			"%s", task.ID, task.Project, parent.Key, parent.Project,
			parent.Project)
	}
	under, err := inSubtree(ctx, tx, task.ID, parentID)
	switch {
	case err != nil:
		return err
	case under:
		return invalid("task %s cannot be filed under %s (%s), "+
			"which is itself or one of its own subtasks — the parent chain "+
			"would be a cycle with no root for a board to draw it from",
			task.ID, parent.Key, parentID)
	}
	return nil
}

// refuseMidMove refuses a write that would change what a task is filed under,
// or what is filed under it, while a move has not yet carried it: the task
// sits beneath a task marked mid-move ([TaskPatch.Moving]) in another project.
// why says what the write would do. Unavailable, never a refusal: the walk,
// its re-run or the duty carries the task unprompted, and the write asked
// again is decided in the project it went to.
//
// A create or a re-parent asks it of the PARENT (the walk never read a new
// child); a re-parent also of the TASK (the walk carries what it read, however
// re-filed since); a merge that re-parents, of both ends before its mark.
func refuseMidMove(ctx context.Context, tx *sql.Tx, task Task, role, why string) error {
	target, err := movingAway(ctx, tx, task.ID, task.Project)
	switch {
	case err != nil:
		return err
	case target == "":
		return nil
	}
	return fmt.Errorf("worklet: %s %s (%s) is still in %s under a task a move "+
		"into %s is carrying, and the walk has not reached it — %s; ask again "+
		"once the move has finished, when it is in %s: %w", role, task.Key,
		task.ID, task.Project, target, why, target, statelog.ErrUnavailable)
}

// movingAwayStatement is [movingAway]'s read, named for the plan test: the
// task's ancestors by the closure, each read by its key.
const movingAwayStatement = `
		SELECT a.project_key
		FROM tracker_task_closure c JOIN tracker_tasks a ON a.id = c.ancestor_id
		WHERE c.descendant_id = ? AND a.moving = 1 AND a.project_key <> ?
		ORDER BY c.distance DESC LIMIT 1`

// movingAway reports the project a move is carrying a task into when a task
// ABOVE it is marked mid-move and in another project, and "" when none is
// ([refuseMidMove]). Any marked ancestor off the closure, never the root by
// `root_id`: the mark is on the task that moved, which may since have been
// re-parented. The outermost answers when two are marked, since it decides
// where the task ends up; a marked task is already in its target project, so
// it is never its own answer.
func movingAway(ctx context.Context, tx *sql.Tx, id, project string) (string, error) {
	var target string
	err := tx.QueryRowContext(ctx, movingAwayStatement, id, project).Scan(&target)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", nil
	case err != nil:
		return "", fmt.Errorf("worklet: read whether %s is in a subtree mid-move: %w",
			id, err)
	}
	return target, nil
}

// uncarriedStatement is [carriedAll]'s read, named for the plan test: the
// root's descendants by the closure, each read by its key.
const uncarriedStatement = `
		SELECT d.id, d.key, d.project_key
		FROM tracker_task_closure c JOIN tracker_tasks d ON d.id = c.descendant_id
		WHERE c.ancestor_id = ? AND c.distance > 0 AND d.project_key <> ?
		ORDER BY c.distance, d.id LIMIT 1`

// carriedAll is the amendment a move's last append is decided under: the mark
// comes down only in a snapshot where nothing beneath the root is outside the
// root's project. A walk carries the subtree it READ before its first append,
// and a create on a node behind the root's move passes [refuseMidMove] and
// lands under a descendant the walk never read; with the mark down it would
// stay in the old project for good. Checked here, after the walk's own appends
// ([Writer.After]), such a task (or one in the trash) refuses the append as
// unavailable ([errUncarried]), keeping the mark up for a pass
// ([Writer.followRoot]) that re-reads and carries it.
func carriedAll(ctx context.Context, tx *sql.Tx, current Task,
	patch TaskPatch) (TaskPatch, error) {

	var id, key, project string
	err := tx.QueryRowContext(ctx, uncarriedStatement, current.ID, current.Project).
		Scan(&id, &key, &project)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return patch, nil
	case err != nil:
		return TaskPatch{}, fmt.Errorf("worklet: read whether anything under %s "+
			"is still outside %s: %w", current.ID, current.Project, err)
	}
	return TaskPatch{}, fmt.Errorf("worklet: task %s (%s) under %s is still in %s "+
		"(filed under the subtree after the walk read it, or not carried yet), "+
		"so %s stays marked mid-move; re-run the move under the same operation "+
		"id, or leave it to the Worklet duty, and the next pass carries it into "+
		"%s: %w", key, id, current.Key, project, current.Key, current.Project,
		errUncarried)
}

// errUncarried is [carriedAll]'s refusal: the walk's last append met a task
// under the root that is still outside its project. Unavailable, because the
// next pass carries it with nobody acting — and a sentinel of its own, because
// the walk that met it is the one caller that runs that pass at once.
var errUncarried = fmt.Errorf("worklet: a task under a moved root is not "+
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
		return false, fmt.Errorf("worklet: read whether %s is under %s: %w",
			candidate, root, err)
	}
	return found == 1, nil
}

// declaredType refuses a task naming a type the company has not declared, as
// `create_work_item` promises ("a task type from your workspace's own
// catalogue"): otherwise `Bug`, `bugfix` and `BUG` become three types beside
// `bug`. An archived type is refused for new work and left alone on old, which
// is what archiving a type is for.
func declaredType(ctx context.Context, tx *sql.Tx, task Task) error {
	catalogue, _, err := readTypeCatalogue(ctx, tx)
	if err != nil {
		return err
	}
	var live []string
	for _, t := range EffectiveTypes(catalogue.Types) {
		if t.Archived {
			if t.Slug == task.Type {
				return invalid("task type %q is archived, so no new "+
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
	return invalid("%q is not a task type this company declares — "+
		"the types are %s, and a new one is declared in the workspace "+
		"catalogue rather than invented at the create", task.Type, words.And(live))
}

// requiredFields refuses a task missing a field its company or its project
// declares required: the whole chain [declaredFields] composes (a project's
// declaration shadowing the workspace's), so a workspace-required field binds
// too. Only fields that apply to this task's type count ([appliesTo]): one
// that does not apply would be hidden the moment it was set, and counting it
// would refuse every plain task in a project that requires `severity` of its
// bugs. A subtask is judged by RequiredInSubtasks instead (as in ClickUp), so
// one required field is not a form for every step a task is broken into.
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
	return invalid("a %s in project %s requires %s, which this task "+
		"does not set", task.Type, project.Key, words.And(missing))
}

// bodyCaveats is what a writer is told and not refused for. The apply must not
// depend on it: every data node resolves the same first [MaxReferencesPerBody]
// keys in document order, so the cap is deterministic without it; the warning
// tells the author that the rest link nothing.
func bodyCaveats(body string) []caveat.Caveat {
	if body == "" {
		return nil
	}
	if found := taskKeysIn(body); len(found) > MaxReferencesPerBody {
		return []caveat.Caveat{caveat.BodyLinksCapped(len(found), MaxReferencesPerBody)}
	}
	return nil
}

// taskProject is the project a task is in, read outside any decision — for a
// sequence's later step, whose scope names the container and whose own decide
// re-reads everything it acts on. refused completes the final refusal of a
// purged task ([missingTask]): what the step cannot do to it.
func (w *Writer) taskProject(ctx context.Context, id, refused string) (string, error) {
	if w.db.IsZero() {
		return "", fmt.Errorf("worklet: this writer has no store to read task "+
			"%s's project from", id)
	}
	var project string
	err := w.db.Read(ctx, func(tx *sql.Tx) error {
		task, held, err := readTask(ctx, tx, id)
		switch {
		case err != nil:
			return err
		case !held:
			return missingTask(ctx, tx, id, refused)
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

// hold takes a claim and keeps it alive. The heartbeat goroutine is owned:
// started here and stopped by [held.release], since a heartbeat nobody stops
// is a claim nobody else can ever take. The owner is this claim's own
// ([Writer.claimOwner]), so a second walk on the SAME node is refused like a
// peer's.
func (w *Writer) hold(ctx context.Context, resource string) (*held, error) {
	if w.claims == nil {
		return nil, fmt.Errorf("worklet: this writer has no coordination, so "+
			"it cannot take %s — a walking sequence without a claim is two "+
			"nodes rewriting one subtree", resource)
	}
	owner := w.claimOwner()
	lease, refused, err := w.claims.TryAcquire(ctx, resource, coord.AcquireOptions{
		Owner: owner, TTL: ClaimTTL,
	})
	switch {
	case err != nil:
		// Unknown fails closed: two interleaved moves key a subtree into two
		// projects, which no duty can tell from an abandoned walk.
		return nil, fmt.Errorf("worklet: take %s: %w", resource, err)
	case lease == nil && refused == coord.RefusedHeld:
		return nil, fmt.Errorf("worklet: %s is held by another walk, on this "+
			"node or a peer, so this walk is already running: %w", resource,
			statelog.ErrUnavailable)
	case lease == nil:
		// Not "another walk": the protocol gate refuses every claim while a
		// node on a lower lease protocol is live. Told the walk was running, a
		// caller would wait on one nobody started.
		return nil, fmt.Errorf("worklet: %s was refused (%s): a node on a "+
			"lower lease protocol is live in this fleet and this node takes no "+
			"claim beside it, so the walk waits until that node has left: %w",
			resource, refused, statelog.ErrUnavailable)
	}
	h := &held{
		claims: w.claims, resource: resource, owner: owner,
		epoch: lease.Epoch, stop: make(chan struct{}), done: make(chan struct{}),
	}
	go h.beat(context.WithoutCancel(ctx))
	return h, nil
}

// claimOwner is the owner one claim is taken under: this node, and THIS call.
// [coord] reads an owner already holding a lease as RENEWING it, so under the
// node id alone two walks on one node would both be told yes, and the first to
// finish would release the other's lease mid-walk. Nor an in-process claim
// beside a node-wide owner ([setup.Hold]'s shape): a restarted process sharing
// the node id would renew its dead predecessor's lease. A dead holder's claim
// lapses over [ClaimTTL] for the duty or a re-run. The node id stays in front
// so a person reading a claim sees where its walk runs.
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
			// A failed renew is not fatal: the TTL is four heartbeats, and an
			// abandoned walk is idempotent and finished by the duty, so tearing
			// down on the first blip would abandon more walks, not fewer.
			_, _ = h.claims.Renew(ctx, h.resource, h.owner, h.epoch, ClaimTTL)
		}
	}
}

// release stops the heartbeat and gives the claim up, under
// [context.WithoutCancel]: the failure being undone is often the cancellation,
// and a claim left to expire blocks this walk for [ClaimTTL].
func (h *held) release(ctx context.Context) {
	close(h.stop)
	<-h.done
	_, _ = h.claims.Release(context.WithoutCancel(ctx), h.resource, h.owner, h.epoch)
}

// MoveStopped reports a cross-project move whose ROOT landed in its target and
// whose walk over the subtree stopped before carrying all of it. A type, for
// [SubtreeStopped]'s reason: the caller must say both that the item is in the
// new project and that some of what goes with it is not; "not made" about a
// re-keyed root sends a model to move it again and a person to a dead key.
//
// The remedy is the SAME operation only: a new one meets a root its own ledger
// never moved and is refused as somebody else's move ([Writer.finishMove]).
// The root stays marked mid-move while anything is left, so the Worklet duty
// finishes the walk once nobody holds its claim ([duty.finishMoves]).
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

	// Waiting is the ADDRESS ([ItemAddress]: its key, or its id where another
	// task claimed the key first) of a task under the root that is in the
	// TRASH and still in the old project; nothing carries it until it is
	// restored or purged. Empty for every other stop.
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
	next := fmt.Sprintf("the same operation (%s) finishes it (a new one is "+
		"refused, since the root is already in %s), and so does Worklet's "+
		"duty once nobody is walking it", e.OpID, e.Target)
	if e.Waiting != "" {
		next = fmt.Sprintf("task %s is in the trash and the move waits for it: "+
			"restore it and the same operation (%s), or the Worklet duty, "+
			"carries it into %s; or purge it", e.Waiting, e.OpID, e.Target)
	}
	return fmt.Sprintf("worklet: task %s moved into %s, and %d of the %d tasks "+
		"under it followed before the walk stopped — the rest are still in "+
		"their old project; %s: %v", root, e.Target, e.Followed, e.Of, next, e.Err)
}

func (e *MoveStopped) Unwrap() error { return e.Err }

// MoveTaskToProject re-homes a root task and everything beneath it. SEQUENCE
// 7: take move/<task>; read the target and its tag set, refusing an archived
// project, a missing required field, or a task in the trash in the subtree
// (its frozen step would stop every walk for good); ADD the subtree's tags the
// target lacks; alias the former key; mint a RANGE; move the root, MARKED
// mid-move ([TaskPatch.Moving]) when it has a subtree and alone carrying
// notify (one thing happened, not forty); move each descendant in (depth, id)
// order; take the mark down ([Writer.endMove]); release. The source's order is
// not rewritten: its rows leave, and the scope carries both containers.
// CRASH RESIDUE: an unused tag or alias (harmless), a numbering gap, and
// descendants in the old project under the marked root. REPAIRER: a re-run
// under the same op id ([Writer.finishMove]) or the duty ([duty.finishMoves]).
func (w *Writer) MoveTaskToProject(ctx context.Context, opID, taskID, target string,
	notify *Notify) (WriteResult, error) {

	switch {
	case taskID == "":
		return WriteResult{}, invalid("a cross-project move names no task")
	case target == "":
		return WriteResult{}, invalid("a cross-project move names no project")
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
	if w.db.IsZero() {
		return WriteResult{}, fmt.Errorf("worklet: this writer has no store, " +
			"so it cannot read the subtree a cross-project move re-keys")
	}
	if err := w.db.Read(ctx, func(tx *sql.Tx) error { //nolint:govet // shadow: scoped to this block; see .golangci.yml (trailing: covers this line only, not the closure)
		//nolint:govet // shadow: `x, err := f()` declares x too; see .golangci.yml
		current, held, err := readTask(ctx, tx, taskID)
		switch {
		case err != nil:
			return err
		case !held:
			return missingTask(ctx, tx, taskID, "it cannot be moved")
		case current.Parent != nil && *current.Parent != "":
			return invalid("task %s has a parent, and only a ROOT "+
				"task moves between projects — moving a subtask alone would "+
				"leave it in a project its parent is not in", taskID)
		case current.Removed != nil:
			return inTheTrash(current, "moving it")
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
			return fmt.Errorf("worklet: project %s is not on this node: %w",
				target, statelog.ErrUnavailable)
		case project.Archived:
			return invalid("project %s is archived, so nothing "+
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
				// A REMOVED TASK IS FROZEN, so the move could not carry
				// it and would leave it under a root in another project.
				return refusedByState("%s, under this task, is in the trash, "+
					"and a move can't carry it. Restore it or purge it, then "+
					"move again", nameOf(descendant))
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
		return WriteResult{}, invalid("task %s has %d descendants "+
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
			return declared, fmt.Errorf("worklet: declare the moving "+
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
		// Not "not moved": the operation predates this node's ledger loss, so
		// an earlier run may have moved the root elsewhere; the walk stops as
		// one this node cannot vouch for.
		return WriteResult{Result: minted}, resolved(fmt.Sprintf(
			"the key range for task %s's move into %s", taskID, target),
			WriteResult{Result: minted}, nil)
	}
	if errors.Is(err, errMintLanded) {
		// The counter step landed under an earlier copy, but the root has not
		// moved (read in its old project), so this run takes a range of its own
		// and the earlier one is the gap.
		base, _, err = w.mintFresh(ctx, target, 1+len(subtree), nil)
	}
	if err != nil {
		return WriteResult{}, err
	}

	// THE MARK RIDES THE ROOT'S OWN MOVE, and only when a subtree is
	// behind it: a leaf's move is one append, finished the moment it lands,
	// and a mark on it would be one more append to take it down.
	result, err := w.moveOne(ctx, stepID(opID, "root"), root, target,
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
			// The mark comes down only once this node holds the whole walk:
			// [carriedAll] reads the descendants' own subjects, so the last of
			// them, not only the root's position, must be applied ([Writer.After]).
			err = w.After(statelog.Later(result.Position, last)).endMove(ctx, opID,
				taskID, target)
			if errors.Is(err, errUncarried) {
				// A task filed behind the walk, now applied here: run the duty's
				// pass at once under the claim this call holds, which re-reads and
				// carries the subtree. Once only: a task filed behind THIS pass is
				// the duty's.
				_, err = w.finishAbandonedMove(ctx, opID, taskID)
			}
		}
		if err != nil {
			// THE ROOT MOVED, so whatever stopped the walk is a stop
			// and never a refusal of the move: see [MoveStopped].
			return result, w.moveStopped(ctx, result, opID, root, target,
				mintedKey(target, base), followed, len(subtree), err)
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
	result.Key, result.Rank = mintedKey(target, base), rootRank
	return result, nil
}

// finishMove answers a move whose root is already in the target project: a
// re-run of a move that got at least as far as its root. The root's step is
// asked for and only answered, as [Writer.resumeCreate] asks for a task: the
// ledger answers it if THIS operation moved the root, and a Decide that runs
// means somebody else did, which is refused. Then the rest follows
// ([Writer.followRoot]).
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
			return statelog.Decision{}, invalid("task %s is already "+
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
// mid-move: the duty's half of [Writer.MoveTaskToProject], under the move's
// claim, which the caller holds; the walk runs it once too, on [errUncarried].
// It reports whether there was a walk to finish. The root is read again under
// the claim, since the holder may have finished since the duty selected it: a
// mark down is a finished move, and a root in the trash is frozen.
func (w *Writer) finishAbandonedMove(ctx context.Context, opID, id string) (bool, error) {
	if w.db.IsZero() {
		return false, fmt.Errorf("worklet: this writer has no store, so it " +
			"cannot read the subtree an abandoned move left behind")
	}
	var (
		root      Task
		subtree   []Task
		marked    bool
		collision bool
	)
	err := w.db.Read(ctx, func(tx *sql.Tx) error {
		current, held, err := readTask(ctx, tx, id)
		switch {
		case err != nil:
			return err
		case !held:
			return missingTask(ctx, tx, id, "there is no move of it left to finish")
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

// carryStragglers carries into a root's project every live task under it still
// outside it while no move of that root runs: the duty's repair for a task
// filed behind a finished move ([duty.carryStragglers]), under the move's
// claim, which the caller holds. It answers how many it carried. The root is
// read again under the claim; one in the trash, MARKED (the abandoned-move
// job's) or with a parent (where a cycle's walk stopped) is not this repair's.
// A straggler in the trash is skipped, not waited for: with no walk to hold
// open, its restore brings it back to the duty.
func (w *Writer) carryStragglers(ctx context.Context, opID, id string) (int, error) {
	if w.db.IsZero() {
		return 0, fmt.Errorf("worklet: this writer has no store, so it " +
			"cannot read the subtree a move left a task behind in")
	}
	var (
		root      Task
		subtree   []Task
		left      int
		collision bool
	)
	err := w.db.Read(ctx, func(tx *sql.Tx) error {
		current, held, err := readTask(ctx, tx, id)
		switch {
		case err != nil:
			return err
		case !held:
			return fmt.Errorf("worklet: task %s has a task filed behind its "+
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

// followRoot moves every descendant a root's move has not carried yet, then
// takes the mark down: one algorithm for the re-run and the duty, every stop a
// [MoveStopped] (rootCollision, from [keyHeldByAnother], names the root in it).
// A descendant is LEFT when outside the root's project, so a task filed behind
// the first run's read is carried here ([carriedAll]). On a FRESH range: no
// record pairs the first run's numbers with tasks, and the subtree has changed
// since, so those numbers are a gap. On a node behind the log, a descendant
// already carried is refused in its own snapshot, so nothing moves twice.
//
// A descendant in the TRASH is waited for, never skipped: it is frozen, and a
// mark taken down around it would strand it in the old project. Everything
// else moves, the mark stays up, and the stop names it for a restore or purge.
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
		return stop(carried, "", invalid("task %s has %d descendants still "+
			"to move and a move carries at most %d", root.ID, len(left),
			MaxDescendants))
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
				return stop(carried, "", fmt.Errorf("worklet: declare the tags "+
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
			// A REMOVED TASK IS FROZEN, so the move waits for it.
			refusedByState("%s, under this task, is in the trash and still in "+
				"%s. Restore it and the next pass moves it into %s, or purge it",
				nameOf(frozen[0]), frozen[0].Project, root.Project))
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
// another task claimed the key first ([ItemAddress]). The id wherever the
// directory cannot be read: it is never wrong, only less readable.
func (w *Writer) addressOf(ctx context.Context, task Task) string {
	if w.db.IsZero() {
		return task.ID
	}
	var collision bool
	if err := w.db.Read(ctx, func(tx *sql.Tx) error {
		var err error
		collision, err = keyHeldByAnother(ctx, tx, task.Key, task.ID)
		return err
	}); err != nil {
		return task.ID
	}
	return ItemAddress(task.ID, task.Key, collision)
}

// endMove takes a root's mid-move mark down: the walk's last append, a quiet
// commit under [ChangeMoved] (the root's watchers heard at its first append).
// Decided in the append's own snapshot ([Writer.UpdateTask]): a mark already
// down publishes nothing, so a re-run or a duty racing the holder writes no
// empty history row; and a mark over an uncarried task stays up
// ([carriedAll]), so a caller waits out the walk's appends first
// ([Writer.After]) or reads its own walk as unfinished.
func (w *Writer) endMove(ctx context.Context, opID, rootID, project string) error {
	down := false
	result, err := w.updateTask(ctx, stepID(opID, "moved"), rootID, project,
		NoIfMatch, TaskPatch{Moving: &down}, ChangeMoved, nil, carriedAll)
	return resolved(fmt.Sprintf("the mid-move mark's removal from task %s",
		rootID), result, err)
}

// moveDescendants moves each of a subtree's descendants, in order, on the
// consecutive numbers from base, and answers how many it moved and the
// position of the last, which the mark's removal must have applied. A stop is
// returned as is: the caller counts what was carried before this pass. Each
// step is named by the DESCENDANT, never its place: a re-run's walk is
// shorter, so a positional id would answer one descendant with another's row.
func (w *Writer) moveDescendants(ctx context.Context, opID, target string,
	descendants []Task, base uint64) (int, statelog.Position, error) {

	var last statelog.Position
	for i, descendant := range descendants {
		moved, err := w.moveOne(ctx, stepID(opID, "task-"+descendant.ID),
			descendant, target, &KeyMint{N: base + uint64(i)}, false, nil)
		// An unknown descendant stops the walk like a refused one, keeping the
		// mark up: lowering it past a task that may still be in the old project
		// splits the subtree for good.
		if err = resolved(fmt.Sprintf("task %s's move into %s",
			descendant.ID, target), moved, err); err != nil {
			return i, last, err
		}
		last = statelog.Later(last, moved.Position)
	}
	return len(descendants), last, nil
}

// moveStopped is what [Writer.MoveTaskToProject] answers when the walk stops
// after the root's own move landed: a [MoveStopped], never the bare error. A
// later pass's own stop is kept whole, since it counted a subtree it re-read.
// key is the root's key from this run's range; a root moved under an EARLIER
// copy holds that copy's number, read off its row.
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

// moveOne re-homes one task of a moving subtree, marking it mid-move when it
// is a root with a walk still to come. Conditioned on the project the task was
// read in, so a second walk is harmless: a task already carried is elsewhere
// when its step decides, and [Writer.UpdateTask] refuses it. The key it leaves
// becomes a former key in the decide's own snapshot ([formerKeysAfterRekey]),
// never from the walk's read, which would drop former keys gained since.
func (w *Writer) moveOne(ctx context.Context, opID string, task Task,
	target string, mint *KeyMint, mark bool,
	notify *Notify) (WriteResult, error) {

	patch := TaskPatch{Project: &target, Mint: mint}
	if mark {
		patch.Moving = &mark
	}
	// Whether the key this step lands under is another task's, read in the
	// deciding snapshot: the applier composes the key from the mint, so only
	// the directory can say. The last round's assignment is the published one.
	var key string
	if mint != nil {
		key = mintedKey(target, mint.N)
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
				return TaskPatch{}, fmt.Errorf("worklet: read whether key %s "+
					"is already another task's: %w", key, err)
			}
			collision = held
			return patch, nil
		})
	result.KeyCollision = collision
	return result, err
}

// carriedTags is every tag the tasks a move carries hold, as their own project
// declares it (label, colour, description), so the target groups them the
// same; a slug the source no longer declares is carried as itself. Read
// beside the tasks it describes, in the source set's order, so two moves of
// the same subtree declare the same list.
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
		return nil, fmt.Errorf("worklet: read %s's tags for the move: %w",
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

// followedTags is [carriedTags] over the tasks a LATER pass carries (a re-run
// or the duty), each read against the project it is in now. A later pass
// declares its own: a task filed behind the walk ([carriedAll]) may carry tags
// the first run never read, and moved without them it would hold slugs its new
// project never declared, refused by its next tag edit.
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
	err := w.db.Read(ctx, func(tx *sql.Tx) error {
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

// tagsStep names the append that declares a set of carried tags by the set,
// not its place: a re-run's passes share the caller's operation, so a
// positional name would answer the second pass from the first's ledger row and
// never declare its tags.
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
// First writer wins on its own subject, and losing is not an error: somebody
// already recorded this key's move. A key the directory gives another task
// (the moving task is its duplicate, `key_collision`) was never this task's
// address, so the decide publishes nothing and the claimant keeps it; refusing
// would make a duplicate the one task a move could never re-key.
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
				return statelog.Decision{}, nil
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

// MergeDuplicates folds one task into another. SEQUENCE 13: take
// merge/<task>; append on the duplicate (the merge marker and `duplicates`);
// per batch of ≤64, re-parent each child onto into; clear the marker and
// cancel; release. The marker is cleared LAST, so the duplicate is visibly
// mid-merge rather than silently half-merged.
//
// CRASH RESIDUE: children partly re-parented. REPAIRER: the Worklet duty once
// the claim has lapsed, idempotent since a child already moved (or in the
// trash, frozen) is not selected. Refused before the mark, since nothing could
// finish it: a survivor in the trash, re-parenting onto the duplicate's own
// subtree or across projects ([ErrReparentAcrossProjects]); an end a move has
// not carried is unavailable ([refuseMidMove]). Later arrivals: [mergeWaits].
func (w *Writer) MergeDuplicates(ctx context.Context, opID, duplicate, into string,
	reparent bool, notify *Notify) (WriteResult, error) {

	switch {
	case duplicate == "":
		return WriteResult{}, invalid("a merge names no duplicate")
	case into == "":
		return WriteResult{}, invalid("a merge names no canonical task")
	case duplicate == into:
		return WriteResult{}, invalid("task %s cannot be a "+
			"duplicate of itself", duplicate)
	}
	claim, err := w.hold(ctx, mergeClaim(duplicate))
	if err != nil {
		return WriteResult{}, err
	}
	defer claim.release(ctx)

	var task Task
	if w.db.IsZero() {
		return WriteResult{}, fmt.Errorf("worklet: this writer has no store, " +
			"so it cannot read the children a merge re-parents")
	}
	if err := w.db.Read(ctx, func(tx *sql.Tx) error { //nolint:govet // shadow: scoped to this block; see .golangci.yml (trailing: covers this line only, not the closure)
		//nolint:govet // shadow: `x, err := f()` declares x too; see .golangci.yml
		current, held, err := readTask(ctx, tx, duplicate)
		switch {
		case err != nil:
			return err
		case !held:
			return missingTask(ctx, tx, duplicate, "it cannot be merged")
		case current.Removed != nil:
			return inTheTrash(current, "merging it")
		}
		task = current
		survivor, held, err := readTask(ctx, tx, into)
		switch {
		case err != nil:
			return err
		case !held:
			return missingTask(ctx, tx, into, "nothing can be merged into it")
		case survivor.Removed != nil:
			// Refused before the mark whichever way the subtasks go: moving
			// them, each re-parent is refused (a trashed parent takes no child),
			// a mark the duty retries forever; leaving them, the one live copy
			// is cancelled in favour of one nobody can see.
			return invalid("%s (%s) is in the trash — removed by %s at "+
				"%s; restore it before merging anything into it",
				survivor.Key, into, survivor.Removed.By,
				survivor.Removed.At.Format(time.RFC3339))
		case !reparent:
			return nil
		}
		// Nor into its own subtree when the subtasks move: [refuseParent]
		// refuses that re-parent as a cycle, so the mark could never finish.
		under, err := inSubtree(ctx, tx, duplicate, into)
		switch {
		case err != nil:
			return err
		case under:
			return invalid("%s is one of %s's own subtasks, so "+
				"the duplicate's subtasks cannot move onto it — one of them "+
				"would end up under itself. Merge without moving the "+
				"subtasks to leave them where they are",
				survivor.Key, current.Key)
		}
		// Only what there is to carry is judged below: with no subtask in its
		// project to move, neither the survivor's project nor a move in flight
		// is a reason to refuse.
		kids, err := readChildBatch(ctx, tx, duplicate, current.Project, "", 1)
		if err != nil || len(kids) == 0 {
			return err
		}
		// Nor while a move has not carried either end, asked before the
		// projects are compared since one is about to change: a subtask moved
		// off an uncarried duplicate lands under a survivor left behind, and
		// one moved onto an uncarried survivor is never read by that walk
		// ([refuseMidMove]).
		if err = refuseMidMove(ctx, tx, current, "task",
			"its subtasks moved now would be split across two projects"); err != nil {
			return err
		}
		if err = refuseMidMove(ctx, tx, survivor, "task",
			"subtasks moved onto it now would be left behind"); err != nil {
			return err
		}
		if survivor.Project != current.Project {
			// A subtask under an item in another project is on neither board
			// (`inconsistent_project`) and only a move of its root clears it,
			// so it is refused before the first append, naming both ways out.
			return invalid("task %s's subtasks are in %s and %s is "+
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

	// The edge is an ADD resolved inside the decide: the read above is a
	// pre-flight outside every snapshot, so a whole set built from it would
	// drop a relation a colleague added in between.
	//
	// The mark carries the intent (MergeReparent): a duty finishing this walk
	// reads it off the task, since a merge that declined to move the subtree
	// and one that crashed before its first child leave identical rows.
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
	// The close lands on the mark's subject while the children are on their
	// own, so the mark's position is what this append must see ([Writer.After]).
	closed, err := w.After(marked.Position).UpdateTask(ctx, stepID(opID, "close"),
		duplicate, task.Project, NoIfMatch,
		TaskPatch{Status: &cancelled, Merging: &done}, ChangeStatus, notify)
	return closed, resolved(fmt.Sprintf("task %s's close as a duplicate of %s",
		duplicate, into), closed, err)
}

// reparentOnto moves whatever is left of a duplicate's subtasks onto the
// canonical task, in batches of [WalkBatch] (a bounded read, and a bounded
// stretch between heartbeats). Shared by the sequence and the duty, so the
// duty's repair is a RE-RUN of what the holder did not reach. Each step id is
// keyed on the child, not its position: a re-run's batches divide differently,
// so an index would answer one child's move with another's ledger row.
//
// Only a child in the canonical task's project is selected, which makes
// [ErrReparentAcrossProjects] an invariant rather than a check at the door: a
// subtask in another project filed under the duplicate since stays there, with
// the trash's frozen children.
func (w *Writer) reparentOnto(ctx context.Context, opID, duplicate, into string) (int, error) {
	project, err := w.taskProject(ctx, into, "nothing can be merged into it")
	if err != nil {
		return 0, err
	}
	var moved int
	var after string
	for {
		var batch []Task
		if err := w.db.Read(ctx, func(tx *sql.Tx) error {
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
				// The duty finishes it once the move CAN land: a survivor trashed
				// since the pre-flight, or one filed under the duplicate since (a
				// cycle), leaves the merge waiting, reported and not retried each
				// tick ([mergeWaits]).
				return moved, fmt.Errorf("worklet: %d subtask(s) re-parented "+
					"onto %s and %s still has more, so it stays marked "+
					"mid-merge; the Worklet duty moves the rest once the "+
					"move can land: %w", moved, into, duplicate, err)
			}
			moved++
		}
	}
}

// ErrReparentAcrossProjects refuses a merge that would re-parent a duplicate's
// subtasks onto an item in another project. See [Writer.MergeDuplicates].
var ErrReparentAcrossProjects = errors.New("worklet: a merge cannot carry " +
	"subtasks into another project")
