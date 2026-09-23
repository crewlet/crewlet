package tracker

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// The SIX sequences that stay genuinely cross-object, and the one property
// that makes them tolerable.
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
// key, because a key is what people paste into chat. An item promotion marks
// its parent LAST, because the other order leaves an item marked promoted with
// no subtask behind it.
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
	// THE SAME 64 as every other fan-out in this design — a move batch, a
	// merge batch, the scope term cap — because they are the same
	// quantity: how much work one durable claim covers before it has to
	// heartbeat.
	MaxBulkTasks = 64

	// MaxBulkBytes is the pre-flight ceiling over the commits a bulk
	// patch will write, checked BEFORE the first append.
	//
	// A bulk gesture is the one write whose refusal has to come before any
	// of it lands: a partial batch is reported and re-run, and a batch
	// refused halfway is a caller told "failed" about tasks that changed.
	MaxBulkBytes = 8 << 20

	// WalkBatch is one batch of a paced walk — a cross-project move's
	// descendants, a merge's children.
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
func resolved(step string, result WriteResult, err error) error {
	switch {
	case err != nil:
		return err
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

	// Key and Rank are what a create or a promotion minted. Empty on
	// every other path.
	Key  string
	Rank Rank

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

	switch {
	case task.ID == "":
		return WriteResult{}, fmt.Errorf("tracker: a create names no task id")
	case task.Project == "":
		return WriteResult{}, fmt.Errorf("tracker: task %s names no project "+
			"— a task's scope path sits under its project's, so one without a "+
			"project files its deferral where no project-scoped probe looks",
			task.ID)
	}
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
	case err != nil:
		return WriteResult{Result: minted}, err
	}
	return w.fileTask(ctx, opID, task, n, settled, notify)
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
	var held bool
	if err := w.db.Replicated().Read(ctx, func(tx *sql.Tx) error {
		var err error
		task, held, err = readTask(ctx, tx, id)
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
	out.Key, out.Rank = task.Key, task.Rank
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

// writeTask is sequence 1's second append and 1a's second, shared because they
// are the same append: a whole task at expectation zero, guarded by its own
// row.
func (w *Writer) writeTask(ctx context.Context, opID string, task Task,
	notify *Notify, at time.Time) (WriteResult, error) {

	subject := TaskSubject(task.ID)
	scope := ScopeSet{Subject: true, Container: task.Project}
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
			return w.decide(stamp, subject, OpCreate, ChangeCreated, scope, opID,
				task, notify, at)
		},
	})
	return WriteResult{
		Result: result, Warnings: bodyWarnings(task.Body),
	}, err
}

// mintKey takes the next key number in a project. SEQUENCE 1's first append,
// and 1a's and 7's.
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
			decision, err := w.decide(stamp, subject, OpPatch, "", scope, opID,
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
		task.Fields, w.World)
	if err != nil {
		return settledCreate{}, err
	}
	return settledCreate{fields: fields, warnings: warnings, unit: project.Unit}, nil
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
// one required field blocks every checklist item anybody promotes.
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

// PromoteItem turns a checklist item into a subtask. SEQUENCE 1a.
//
//	Rs the parent and its project → A on the counter → A on the subtask at
//	expectation 0, carrying rank = intPart(n) → A on the PARENT, marking
//	the item promoted.
//
// # Why this is a sequence at all, and why the parent commit is LAST
//
// A promoted item is a subtask, and a subtask has a KEY — so the promotion
// mints a counter value and carries the identical counter-then-task window a
// create does, including the numbering gap. It was listed among the one-append
// gestures for as long as nobody asked what its key came from.
//
// The parent is marked last because the other order leaves an item marked
// promoted with no subtask behind it — a struck-through line pointing at
// nothing, which no reader can tell from a subtask somebody purged.
//
// # Re-running it
//
// A RETRY under the same operation id is answered step by step from the
// ledger: a counter step that landed is not minted again ([Writer.resumeCreate]
// asks for the subtask instead), a subtask that landed is that one under the
// key it took, and a parent step that landed is the retry's answer — so a
// retry of a promotion that finished answers `applied` with the first run's
// position and appends nothing.
//
// A RE-RUN under a new operation re-derives the same subtask id, because it is
// a uuid5 over the item: its create is refused on the subtask's guarding row,
// the answer is the subtask already filed, and the run proceeds to the parent
// step. It spends a number on its own mint, which is the documented gap.
//
// # The parent's mark is decided on the PARENT'S OWN SNAPSHOT
//
// It is a [PromoteIntent] rather than a checklist set, resolved inside the
// parent step's decide ([settlePromote]): the lists are carried whole, and a
// set composed from the read the mint made discarded every checklist edit that
// landed while the subtask was being filed. An item that already points at the
// subtask needs no record, and one somebody deleted meanwhile is a warning.
//
// CRASH RESIDUE: a numbering gap, exactly as row 1; or a landed subtask whose
// parent item is still un-marked. REPAIRER: nobody for the gap, and the re-run
// or retry above for the mark. A subtask that was PURGED is refused as deleted
// rather than resurrected.
func (w *Writer) PromoteItem(ctx context.Context, opID, parentID, itemID string,
	subtask Task, notify *Notify) (WriteResult, error) {

	switch {
	case parentID == "":
		return WriteResult{}, fmt.Errorf("tracker: a promotion names no parent")
	case itemID == "":
		return WriteResult{}, fmt.Errorf("tracker: a promotion names no item")
	case subtask.ID == "":
		return WriteResult{}, fmt.Errorf("tracker: a promotion mints no subtask "+
			"id — it is a uuid5 over item %s, which is what makes a retry "+
			"re-derive the same subtask rather than a second one", itemID)
	case subtask.Project == "":
		return WriteResult{}, fmt.Errorf("tracker: subtask %s names no project "+
			"to take its key from", subtask.ID)
	}
	// THE SAME DEFAULT AS A PLAIN CREATE, and for the same reason: a
	// checklist item carries no type, so a promotion that named none would
	// be refused by the catalogue check inside the mint.
	if subtask.Type == "" {
		subtask.Type = DefaultTaskType
	}
	subtask.Parent = &parentID

	// WHAT THE MINT'S SNAPSHOT SETTLED, assigned on every run of its
	// closure so the last run — the accepted one — is what survives.
	var (
		parentProject string
		settled       settledCreate
	)
	n, minted, err := w.mintKey(ctx, stepID(opID, "counter"), subtask.Project, 1,
		func(tx *sql.Tx) error {
			project, err := promotableParent(ctx, tx, parentID, itemID)
			parentProject = project
			if err != nil {
				return err
			}
			// A PROMOTION'S SUBTASK CARRIES NO CUSTOM FIELDS — it is
			// built from a checklist item, which has none — so the
			// coerced map it answers with is empty. Its PROJECT'S
			// UNIT is not: a promoted subtask is work in a project
			// like any other, and one filed into no unit is one that
			// unit's lead is no fallback for.
			settled, err = w.refuseCreate(ctx, tx, subtask)
			return err
		})
	var created WriteResult
	switch {
	case errors.Is(err, errMintLanded):
		created, err = w.resumeCreate(ctx, opID, subtask, notify)
	case err != nil:
		return WriteResult{Result: minted}, err
	default:
		created, err = w.fileTask(ctx, opID, subtask, n, settled, notify)
	}
	if errors.Is(err, statelog.ErrExists) {
		// THE SUBTASK IS ALREADY FILED, by an earlier run under another
		// operation — its id is the item's — so it is that one, under the
		// key IT took; this run's number is the gap.
		created, err = w.landedTask(ctx, created.Result, subtask.ID)
	}
	// A SUBTASK WHOSE CREATE IS UNKNOWN IS NOT ONE TO POINT AT: marking the
	// item over it is the struck-through line pointing at nothing that this
	// order exists to prevent.
	if err = resolved(fmt.Sprintf("subtask %s's create", subtask.ID),
		created, err); err != nil {
		return created, err
	}
	if parentProject == "" {
		// THE MINT WAS ANSWERED, NOT DECIDED, so its snapshot never ran
		// and the parent's project is read here: it is what the parent
		// step's scope names.
		if parentProject, err = w.taskProject(ctx, parentID); err != nil {
			return created, err
		}
	}

	marked, err := w.UpdateTask(ctx, stepID(opID, "parent"), parentID,
		parentProject, NoIfMatch,
		TaskPatch{Promote: &PromoteIntent{Item: itemID, Subtask: subtask.ID}},
		ChangeChecklist, nil)
	if err = resolved(fmt.Sprintf("the mark on %s's item", parentID),
		marked, err); err != nil {
		return created, fmt.Errorf("tracker: subtask %s is filed and its item "+
			"in %s may not be marked yet; retry the promotion under the same "+
			"operation id, which completes it: %w", created.Key, parentID, err)
	}
	created.Result = marked.Result
	created.Warnings = append(created.Warnings, marked.Warnings...)
	return created, nil
}

// promotableParent is the refusals a promotion reads its parent for, inside
// the mint's snapshot, and the project the parent is in.
func promotableParent(ctx context.Context, tx *sql.Tx, parentID,
	itemID string) (string, error) {

	current, held, err := readTask(ctx, tx, parentID)
	switch {
	case err != nil:
		return "", err
	case !held:
		return "", fmt.Errorf("tracker: parent task %s is not on this node: %w",
			parentID, statelog.ErrUnavailable)
	case current.Removed != nil:
		return "", fmt.Errorf("tracker: task %s was removed by %s at %s; "+
			"restore it before promoting anything out of it",
			parentID, current.Removed.By, current.Removed.At.Format(time.RFC3339))
	}
	if _, _, found := findItem(current, itemID); !found {
		return "", fmt.Errorf("tracker: task %s has no checklist item %s",
			parentID, itemID)
	}
	return current.Project, nil
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

// findItem locates a checklist item on a task.
func findItem(task Task, itemID string) (list, item int, found bool) {
	for l := range task.Checklists {
		for i := range task.Checklists[l].Items {
			if task.Checklists[l].Items[i].ID == itemID {
				return l, i, true
			}
		}
	}
	return 0, 0, false
}

// markPromoted is the parent's own new checklist state.
//
// THE ITEM IS NOT DELETED. It stays, pointing at the subtask, which is what
// renders it struck through with the new key — a deletion would lose the fact
// that this line became that task.
func markPromoted(parent Task, itemID, subtaskID string) []Checklist {
	lists := make([]Checklist, len(parent.Checklists))
	copy(lists, parent.Checklists)
	l, i, found := findItem(parent, itemID)
	if !found {
		return lists
	}
	items := make([]ChecklistItem, len(lists[l].Items))
	copy(items, lists[l].Items)
	items[i].PromotedTo = &subtaskID
	lists[l].Items = items
	return lists
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
	newTags []Tag) (WriteResult, error) {

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
		arrived bool
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
		return nil
	}); err != nil {
		return WriteResult{}, err
	}
	if arrived {
		return w.finishMove(ctx, opID, root, target, subtree)
	}
	if len(subtree) > MaxDescendants {
		return WriteResult{}, fmt.Errorf("tracker: task %s has %d descendants "+
			"and a move carries at most %d", taskID, len(subtree), MaxDescendants)
	}

	at := w.Now()
	if len(newTags) > 0 {
		//nolint:govet // shadow: `x, err := f()` declares x too; see .golangci.yml
		set, err := w.tagsOf(ctx, target)
		if err != nil {
			return WriteResult{}, err
		}
		if merged, changed := mergeTags(set, newTags); changed {
			declared, err := w.WriteDocument(ctx, stepID(opID, "tags"),
				TagsSubject(target), "", merged, ChangeTags, nil)
			if err = resolved("the moving subtree's tags in "+target,
				declared, err); err != nil {
				return declared, fmt.Errorf("tracker: declare the moving "+
					"subtree's tags in %s: %w", target, err)
			}
		}
	}

	// THE ALIAS BEFORE THE RE-KEY, so a key somebody pastes into chat
	// keeps resolving from the moment it stops being current.
	aliased, err := w.claimAlias(ctx, stepID(opID, "alias"), root.Key, taskID, at)
	if err = resolved("key "+root.Key+"'s alias", aliased, err); err != nil {
		return aliased, err
	}

	base, _, err := w.mintKey(ctx, stepID(opID, "counter"), target, 1+len(subtree), nil)
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
		&KeyMint{N: base}, len(subtree) > 0)
	// A ROOT WHOSE MOVE IS UNKNOWN HAS NO SUBTREE TO FOLLOW IT: moving the
	// descendants under a root that may still be in its old project splits
	// the subtree the other way round.
	if err = resolved(fmt.Sprintf("task %s's move into %s", taskID, target),
		result, err); err != nil {
		return result, err
	}
	if len(subtree) > 0 {
		if err = w.moveDescendants(ctx, opID, target, subtree, base+1); err != nil {
			return result, err
		}
		// THE MARK COMES DOWN ON THE SUBJECT THE ROOT'S MOVE ALREADY
		// MOVED, and the descendants in between are on subjects of their
		// own — so the root's position is what this last append has to
		// see, exactly as a merge's close waits for its mark. See
		// [Writer.After].
		if err = w.After(result.Position).endMove(ctx, opID, taskID, target); err != nil {
			return result, err
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
	target string, subtree []Task) (WriteResult, error) {

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
	if err := w.followRoot(ctx, opID, root, subtree); err != nil {
		return WriteResult{Result: answered}, err
	}
	out := WriteResult{Result: answered}
	out.Key, out.Rank = root.Key, root.Rank
	return out, nil
}

// finishAbandonedMove completes a move whose walk stopped with its root marked
// mid-move: the tracker duty's half of [Writer.MoveTaskToProject], run under
// the move's own claim, which the caller holds. It reports whether there was a
// walk to finish.
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
		root    Task
		subtree []Task
		marked  bool
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
		subtree, err = readSubtree(ctx, tx, id)
		return err
	})
	if err != nil || !marked {
		return false, err
	}
	return true, w.followRoot(ctx, opID, root, subtree)
}

// followRoot moves every descendant a root's move has not carried yet, and
// then takes the root's mark down. SHARED BY THE RE-RUN AND THE DUTY, so a walk
// finished either way is finished by one algorithm rather than by two that
// have to keep agreeing.
//
// ON A FRESH RANGE, never the first run's — see [Writer.MoveTaskToProject] —
// and a descendant is LEFT when it is outside the ROOT'S project, whichever
// project that is: a subtree can hold a child a merge re-parented in from
// another project, and the move carries the whole subtree.
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
func (w *Writer) followRoot(ctx context.Context, opID string, root Task,
	subtree []Task) error {

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
	if len(left) > MaxDescendants {
		return fmt.Errorf("tracker: task %s has %d descendants still to move "+
			"and a move carries at most %d", root.ID, len(left), MaxDescendants)
	}
	if len(left) > 0 {
		base, _, err := w.mintFresh(ctx, root.Project, len(left), nil)
		if err != nil {
			return err
		}
		if err := w.moveDescendants(ctx, opID, root.Project, left, base); err != nil {
			return err
		}
	}
	if len(frozen) > 0 {
		return fmt.Errorf("tracker: task %s under %s is in the trash and still "+
			"in project %s, and a removed task is frozen — the move waits for "+
			"it: restore it and the next pass carries it into %s, or purge it",
			frozen[0].ID, root.ID, frozen[0].Project, root.Project)
	}
	return w.endMove(ctx, opID, root.ID, root.Project)
}

// endMove takes a root's mid-move mark down: the walk's last append.
//
// A MARK ALREADY DOWN IS NOTHING TO WRITE, decided inside the append's own
// snapshot ([Writer.UpdateTask]) — so a re-run of a walk whose last append
// landed, and a duty that raced the holder's own, each publish nothing rather
// than a history row saying nothing.
//
// A QUIET COMMIT UNDER [ChangeMoved]: it is the move finishing, and the people
// watching the root heard about the move from its first append.
func (w *Writer) endMove(ctx context.Context, opID, rootID, project string) error {
	down := false
	result, err := w.UpdateTask(ctx, stepID(opID, "moved"), rootID, project,
		NoIfMatch, TaskPatch{Moving: &down}, ChangeMoved, nil)
	return resolved(fmt.Sprintf("the mid-move mark's removal from task %s",
		rootID), result, err)
}

// moveDescendants moves each of a subtree's descendants, in order, on the
// consecutive numbers from base.
func (w *Writer) moveDescendants(ctx context.Context, opID, target string,
	descendants []Task, base uint64) error {

	for i, descendant := range descendants {
		moved, err := w.moveOne(ctx, stepID(opID, "task-"+descendant.ID),
			descendant, target,
			append(append([]string{}, descendant.FormerKeys...), descendant.Key),
			&KeyMint{N: base + uint64(i)}, false)
		// AN UNKNOWN DESCENDANT STOPS THE WALK like a refused one, and
		// the root's mark stays up over it: lowering the mark past a
		// task that may still be in the old project is the split subtree
		// nothing would look for again.
		if err = resolved(fmt.Sprintf("task %s's move into %s",
			descendant.ID, target), moved, err); err != nil {
			return fmt.Errorf("tracker: %d of %d descendants moved; re-run the "+
				"move under the same operation id, which moves the rest: %w",
				i, len(descendants), err)
		}
	}
	return nil
}

// moveOne re-homes one task of a moving subtree, and marks it mid-move when it
// is the root of one that has a walk still to come.
//
// CONDITIONED ON THE PROJECT THE TASK WAS READ IN, which is what makes a
// second walk over the same subtree harmless: a task somebody already carried
// is in another project by the time its step decides, and [Writer.UpdateTask]
// refuses a write naming the wrong one rather than re-keying it again.
func (w *Writer) moveOne(ctx context.Context, opID string, task Task,
	target string, former []string, mint *KeyMint, mark bool) (WriteResult, error) {

	patch := TaskPatch{Project: &target, FormerKeys: &former, Mint: mint}
	if mark {
		patch.Moving = &mark
	}
	// NO NOTIFICATION AND NO HISTORY BUMP on a descendant: a subtree that
	// moved wakes the people watching the root, not everybody watching
	// every task beneath it.
	return w.UpdateTask(ctx, opID, task.ID, task.Project, NoIfMatch, patch,
		ChangeMoved, nil)
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
			return w.decide(stamp, subject, OpCreate, "", scope, opID, KeyAlias{
				Key: key, TaskID: taskID,
			}, nil, at)
		},
	})
	if errors.Is(err, statelog.ErrExists) {
		return result, nil
	}
	return result, err
}

// tagsOf reads a project's tag set outside any decision.
func (w *Writer) tagsOf(ctx context.Context, project string) (TagSet, error) {
	var set TagSet
	err := w.db.Replicated().Read(ctx, func(tx *sql.Tx) error {
		stored, held, err := readTagSet(ctx, tx, project)
		if err != nil {
			return err
		}
		if !held {
			stored = TagSet{V: DocumentVersion, Project: project}
		}
		set = stored
		return nil
	})
	return set, err
}

// mergeTags adds the tags a moving subtree carries that the target lacks.
//
// BY SLUG AND NEVER BY LABEL, because a slug is what a stored value points at
// and a label is what somebody renamed last week. Reports whether anything
// changed, so a move that brings no new tag publishes no record at all.
func mergeTags(set TagSet, incoming []Tag) (TagSet, bool) {
	have := make(map[string]bool, len(set.Tags))
	for _, t := range set.Tags {
		have[t.Slug] = true
	}
	changed := false
	for _, t := range incoming {
		if t.Slug == "" || have[t.Slug] {
			continue
		}
		have[t.Slug] = true
		set.Tags = append(set.Tags, t)
		changed = true
	}
	if changed {
		set.TagsVersion++
	}
	return set, changed
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
			return fmt.Errorf("tracker: task %s is not on this node: %w",
				duplicate, statelog.ErrUnavailable)
		case current.Removed != nil:
			return fmt.Errorf("tracker: task %s was removed by %s at %s; "+
				"restore it before merging it", duplicate,
				current.Removed.By, current.Removed.At.Format(time.RFC3339))
		}
		task = current
		if _, held, err := readTask(ctx, tx, into); err != nil {
			return err
		} else if !held {
			return fmt.Errorf("tracker: task %s is not on this node: %w",
				into, statelog.ErrUnavailable)
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
// A merge's children are one of the three walks [WalkBatch] is named for, and
// the value buys the same two things here as there — a bounded read whatever
// the subtree's size, and a bounded stretch between the claim's heartbeats.
//
// THE STEP ID IS KEYED ON THE CHILD rather than on its position in the walk,
// because a re-run's batches do not divide the same way: the moved ones are
// gone from the selection, so an index would give a different child the same
// operation id and the ledger would answer one move's question with another's
// row.
func (w *Writer) reparentOnto(ctx context.Context, opID, duplicate, into string) (int, error) {
	var moved int
	var after string
	for {
		var batch []Task
		if err := w.db.Replicated().Read(ctx, func(tx *sql.Tx) error {
			var err error
			batch, err = readChildBatch(ctx, tx, duplicate, after, WalkBatch)
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
				return moved, fmt.Errorf("tracker: %d subtask(s) re-parented "+
					"onto %s and %s still has more; the tracker duty completes "+
					"the rest idempotently: %w", moved, into, duplicate, err)
			}
			moved++
		}
	}
}

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
