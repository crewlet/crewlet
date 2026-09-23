package tracker

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

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
// ORDER of its appends, the CLAIM that stops two walks of it running at once
// — the fleet's durable lease between nodes, and [localClaims] between two
// goroutines on one — the CRASH RESIDUE the order was chosen to leave, and the
// REPAIRER that clears it — or the argument for why none is needed.
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

	// WalkBatch is how many records one bounded step publishes — one
	// batch of a paced walk, or one whole tick of a duty repair.
	//
	// THE SAME 64 as [MaxBulkTasks] and for its reason: each of these is
	// a fan-out of independent records, and this is how many of them one
	// step hands every node's serial applier at once. The read that feeds
	// a step is bounded by the same number, so nothing here reads a
	// quantity that grows with the company.
	//
	// # IT MEANS A DIFFERENT THING AT EACH KIND OF CALL SITE
	//
	// A WALK PAGES UNTIL DRAINED inside its own claim, and there the
	// value is the size of a step rather than a bound on the walk. A
	// project's re-spread ([RespreadPlan.Batch]) publishes ONE record per
	// batch and sleeps [pace] over what that batch just cost every peer,
	// so what moves with the value is how much of the walk lands on an
	// applier at a time. A merge's children ([Writer.reparentOnto]) pages
	// straight on and publishes one record PER CHILD, so there it is only
	// how many rows one read returns.
	//
	// TWO DUTY REPAIRS TAKE ONE BATCH PER TICK and then wait for the next
	// sweep, and for those two this IS the company's fleet-wide repair
	// throughput: the missed-unblocked notices ([ScanUnblocked]), one
	// record per dependent told, and the one-sided mirrors
	// ([ScanOneSided]), at most this many EDGES resolved by
	// [PlanOneSided] into one record per subject. So each of them lands
	// at most 64 records on every node's applier per sweep, a sweep being
	// maintenance.Interval, fifteen minutes — a backlog of a thousand
	// late notices drains in about four hours, which is the figure
	// docs/guides/work-tracker.md gives an operator and which
	// TestTheRepairDrainIsWhatTheOperatorGuidePromises holds this number
	// to. Nothing is lost to that pacing: each keeps its position (or its
	// flag) on a window it could not finish and the rest come up next
	// sweep.
	//
	// THE OTHER TWO DUTY JOBS ARE BOUNDED PER UNIT, NOT PER TICK, and
	// that is the blast radius a retune has to price rather than a detail
	// of theirs. [duty.clearDuplicates] takes one batch PER PROJECT and
	// loops every flagged project, so one tick publishes a rank-order
	// record for each of them, each re-minting up to this many keys.
	// [duty.finishMerges] takes this many MERGES and runs
	// [Writer.reparentOnto] to exhaustion on every one, so one tick can
	// publish a record per subtask of 64 whole subtrees. Both keep their
	// own gate — the project flag, `merging = 1` — so what a tick cuts is
	// read again next sweep.
	//
	// Widening this therefore divides the repair drain AND multiplies
	// what one sweep lands on every applier, and narrowing it does the
	// reverse — which is the trade this number is, and why it is stated
	// here rather than at any one of its call sites.
	WalkBatch = 64

	// ClaimTTL is how long the durable claim a walking sequence holds
	// survives unrenewed. FOUR HEARTBEATS, so three consecutive misses are
	// survivable.
	ClaimTTL = 60 * time.Second

	// ClaimHeartbeat is how often the holder renews that claim — a quarter
	// of [ClaimTTL], which is the arithmetic its own comment rests on.
	ClaimHeartbeat = 15 * time.Second

	// ClaimStale is half the TTL past its last heartbeat, which is the
	// point at which a holder that is still alive would have renewed
	// twice.
	ClaimStale = 30 * time.Second
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
func stepID(opID, step string) string { return opID + "." + step }

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
	// write is NOT atomic and never was: the caller re-runs the failures,
	// which carry the current version in them.
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
	if err := checkChecklistCaps(task.ID, task.Checklists); err != nil {
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

	// THE COERCED VALUES COME BACK OUT OF THE SNAPSHOT, and the LAST run
	// of the closure is the one whose mint was accepted — so both are
	// assigned rather than appended to, exactly as the update path does
	// with its own warnings.
	var (
		coerced  map[string]json.RawMessage
		warnings []string
	)
	n, minted, err := w.mintKey(ctx, stepID(opID, "counter"), task.Project, 1,
		func(tx *sql.Tx) error {
			//nolint:govet // shadow: scoped to this block; see .golangci.yml
			var err error
			coerced, warnings, err = w.refuseCreate(ctx, tx, task)
			return err
		})
	if err != nil {
		return WriteResult{Result: minted}, err
	}
	if coerced != nil {
		task.Fields = coerced
	}
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
	result.Key, result.Rank = task.Key, task.Rank
	result.Warnings = append(result.Warnings, warnings...)
	return result, err
}

// writeTask is sequence 1's second append and 1a's second, shared because they
// are the same append: a whole task at expectation zero, guarded by its own
// row.
func (w *Writer) writeTask(ctx context.Context, opID string, task Task,
	notify *Notify, at time.Time) (WriteResult, error) {

	subject := TaskSubject(task.ID)
	scope := ScopeSet{Subject: true, Container: task.Project}
	result, err := w.publish(ctx, statelog.Request{
		Subject:  wire(subject),
		Scope:    scope.Resolve(subject),
		OpID:     opID,
		MintedAt: at,
		Pattern:  statelog.PatternCreate,
		Decide: func(tx *sql.Tx) (statelog.Decision, error) {
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
			// THE PARENT IS READ HERE AGAIN, in the snapshot this record
			// is decided from, because the key mint before it is a
			// separate append: a purge landing between the two is one
			// only this read can see. See [refusePurged].
			if task.Parent != nil && *task.Parent != "" {
				if err := refusePurged(ctx, tx, *task.Parent, "parent"); err != nil {
					return statelog.Decision{}, err
				}
			}
			return w.decide(subject, OpCreate, ChangeCreated, scope, opID,
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
		Subject:  wire(subject),
		Scope:    scope.Resolve(subject),
		OpID:     opID,
		MintedAt: at,
		Pattern:  statelog.PatternArbitrated,
		Decide: func(tx *sql.Tx) (statelog.Decision, error) {
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
			decision, err := w.decide(subject, OpPatch, "", scope, opID,
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
	return base, result, nil
}

// refuseCreate is what sequence 1 reads the project and its catalogues for.
// It also COERCES the task's custom-field values, which is why it answers with
// them rather than only with an error: a value is normalised against the
// declarations this snapshot holds — an option spelling to the option's id, a
// timestamp on a date-only field to its date — and the record has to carry
// that canonical form, so every node writes identical rows without re-deciding
// anything.
func (w *Writer) refuseCreate(ctx context.Context, tx *sql.Tx, task Task) (
	map[string]json.RawMessage, []string, error) {

	project, held, err := readProject(ctx, tx, task.Project)
	switch {
	case err != nil:
		return nil, nil, err
	case !held:
		return nil, nil, fmt.Errorf("tracker: project %s is not on this node: %w",
			task.Project, statelog.ErrUnavailable)
	case project.Archived:
		return nil, nil, fmt.Errorf("tracker: project %s is archived, so it "+
			"takes no new work; unarchive it first", task.Project)
	}
	// A PURGED PARENT IS REFUSED BEFORE THE MINT, so a create that can
	// never land takes no key number with it. The task's own append reads
	// it again ([Writer.writeTask]), which is what covers a purge landing
	// between the two.
	if task.Parent != nil && *task.Parent != "" {
		if err := refusePurged(ctx, tx, *task.Parent, "parent"); err != nil {
			return nil, nil, err
		}
	}
	if err := declaredType(ctx, tx, task); err != nil {
		return nil, nil, err
	}
	if err := declaredTags(ctx, tx, task.Project, task.Tags); err != nil {
		return nil, nil, err
	}
	if err := requiredFields(ctx, tx, project, task); err != nil {
		return nil, nil, err
	}
	return settleFields(ctx, tx, task.Project, task.Type, task.Fields, w.World)
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
// CRASH RESIDUE: a numbering gap, exactly as row 1; or a landed subtask whose
// parent item is still un-marked. REPAIRER: nobody for the gap. The un-marked
// parent needs none either, because the subtask's id is a uuid5 over the item:
// a retry re-derives the same id, the create is refused as already existing on
// its guarding row, and the retry proceeds to the parent commit. A subtask that
// was PURGED is refused as deleted rather than resurrected.
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
	}
	// THE SAME DEFAULT AS A PLAIN CREATE, and for the same reason: a
	// checklist item carries no type, so a promotion that named none would
	// be refused by the catalogue check inside the mint.
	if subtask.Type == "" {
		subtask.Type = DefaultTaskType
	}

	var parent Task
	n, minted, err := w.mintKey(ctx, stepID(opID, "counter"), subtask.Project, 1,
		func(tx *sql.Tx) error {
			// PURGED IS ASKED BEFORE ABSENT, because a purged parent is
			// absent too and the two call for different things: one is
			// never coming back, the other is a node still catching up.
			if err := refusePurged(ctx, tx, parentID, "parent"); err != nil {
				return err
			}
			current, held, err := readTask(ctx, tx, parentID)
			switch {
			case err != nil:
				return err
			case !held:
				return fmt.Errorf("tracker: parent task %s is not on this "+
					"node: %w", parentID, statelog.ErrUnavailable)
			case current.Removed != nil:
				return fmt.Errorf("tracker: task %s was removed by %s at %s; "+
					"restore it before promoting anything out of it",
					parentID, current.Removed.By,
					current.Removed.At.Format(time.RFC3339))
			}
			if _, _, found := findItem(current, itemID); !found {
				return fmt.Errorf("tracker: task %s has no checklist item %s",
					parentID, itemID)
			}
			parent = current
			// A PROMOTION'S SUBTASK CARRIES NO CUSTOM FIELDS — it is
			// built from a checklist item, which has none — so the
			// coerced map it answers with is empty and is dropped
			// deliberately rather than threaded through.
			_, _, err = w.refuseCreate(ctx, tx, subtask)
			return err
		})
	if err != nil {
		return WriteResult{Result: minted}, err
	}
	rank, err := IntegerAt(n)
	if err != nil {
		return WriteResult{}, fmt.Errorf("tracker: derive %s-%d's rank: %w",
			subtask.Project, n, err)
	}
	at := w.Now()
	subtask.Key = fmt.Sprintf("%s-%d", subtask.Project, n)
	subtask.Rank = rank
	subtask.Parent = &parentID
	subtask.CreatedAt, subtask.UpdatedAt = at, at
	if subtask.Status == "" {
		subtask.Status = StatusTodo
	}
	subtask.StatusGroup = subtask.Status.Group()
	if subtask.Priority == "" {
		subtask.Priority = PriorityNone
	}

	created, err := w.writeTask(ctx, stepID(opID, "subtask"), subtask, notify, at)
	created.Key, created.Rank = subtask.Key, subtask.Rank
	if err != nil && !errors.Is(err, statelog.ErrExists) {
		// ALREADY EXISTING IS THE RETRY'S OWN PATH, not a failure: the
		// id is derived from the item, so a re-run finds its own subtask
		// and carries on to the parent commit the first attempt did not
		// reach.
		return created, err
	}

	lists := markPromoted(parent, itemID, subtask.ID)
	marked, err := w.UpdateTask(ctx, stepID(opID, "parent"), parentID,
		parent.Project, NoIfMatch, TaskPatch{Checklists: &lists},
		ChangeChecklist, nil)
	if err != nil {
		return created, fmt.Errorf("tracker: subtask %s was created and its "+
			"item in %s is still un-marked; re-run the promotion, which "+
			"re-derives the same subtask and completes: %w",
			subtask.Key, parentID, err)
	}
	created.Result = marked.Result
	return created, nil
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

// localClaims is this node's half of every claim a sequence takes: the
// resources a goroutine on this node holds right now.
//
// # Why the lease is not enough on its own
//
// The lease is taken for the NODE ([Writer.nodeID] is its owner), and
// [Claims.TryAcquire] doubles as a renew for an owner that already holds one —
// so two goroutines here asking for the same resource are both told yes. Two
// seats on one node merging one duplicate into two different items would then
// walk its subtasks at once, each moving what the other had not, and a node's
// every bulk edit would be admitted whatever else it was applying. The lease
// stops two nodes; this stops two goroutines on one. A claim is both or
// nothing.
//
// One per node, because everything on a node writes through the writer the node
// built or a clone of it ([Writer.As]), and a clone shares this by pointer.
type localClaims struct {
	mu   sync.Mutex
	held map[string]bool
}

func newLocalClaims() *localClaims {
	return &localClaims{held: map[string]bool{}}
}

// take reports whether this goroutine now holds resource on this node — a
// bool, because "a walk here already holds it" is knowledge rather than a
// failure to look.
func (c *localClaims) take(resource string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.held[resource] {
		return false
	}
	c.held[resource] = true
	return true
}

func (c *localClaims) give(resource string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.held, resource)
}

// held is one claim, heartbeated for as long as a walk runs.
type held struct {
	claims   Claims
	local    *localClaims
	resource string
	owner    string
	epoch    int64
	stop     chan struct{}
	done     chan struct{}
}

// claim takes a walk's claim — this node's and the fleet's — and keeps it
// alive, answering in [Claims]' three values: the claim; nil when a walk
// already holds it, on this node or another; and an error when the
// coordination store could not say.
//
// THIS NODE'S HALF FIRST, so that only the goroutine holding it ever touches
// the lease. The lease is keyed on the node, so a second goroutine here that
// took it would renew the first one's lease as its own, and one that released
// it would release the first one's.
//
// THE HEARTBEAT IS A GOROUTINE WITH AN OWNER, per this tree's rule: it is
// started here, stopped by [held.release], and cannot outlive the sequence
// that took it. A heartbeat nobody stops is a claim nobody else can ever take.
func (w *Writer) claim(ctx context.Context, resource string) (*held, error) {
	if w.claims == nil {
		return nil, fmt.Errorf("tracker: this writer has no coordination, so "+
			"it cannot take %s — a walking sequence without a claim is two "+
			"nodes rewriting one subtree", resource)
	}
	if !w.local.take(resource) {
		return nil, nil
	}
	lease, err := w.claims.TryAcquire(ctx, resource, coord.AcquireOptions{
		Owner: w.nodeID, TTL: ClaimTTL,
	})
	switch {
	case err != nil:
		w.local.give(resource)
		return nil, fmt.Errorf("tracker: take %s: %w", resource, err)
	case lease == nil:
		w.local.give(resource)
		return nil, nil
	}
	h := &held{
		claims: w.claims, local: w.local, resource: resource, owner: w.nodeID,
		epoch: lease.Epoch, stop: make(chan struct{}), done: make(chan struct{}),
	}
	go h.beat(context.WithoutCancel(ctx))
	return h, nil
}

// hold is [Writer.claim] for a sequence that cannot run without it: a walk
// already holding the claim is refused as unavailable.
func (w *Writer) hold(ctx context.Context, resource string) (*held, error) {
	h, err := w.claim(ctx, resource)
	switch {
	case err != nil:
		// UNKNOWN, AND THIS ONE FAILS CLOSED. A cross-project move
		// rewrites a whole subtree's keys; two of them interleaved
		// produce a subtree keyed into two projects, which no duty can
		// tell from an abandoned walk.
		return nil, err
	case h == nil:
		return nil, fmt.Errorf("tracker: %s is held, so this walk is already "+
			"running on this node or another: %w", resource, statelog.ErrUnavailable)
	}
	return h, nil
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
			// that would be abandoned on the fourth is idempotent.
			// Tearing the walk down on the first blip would abandon more
			// of them, not fewer.
			_, _ = h.claims.Renew(ctx, h.resource, h.owner, h.epoch, ClaimTTL)
		}
	}
}

// release stops the heartbeat and gives the claim up — the lease, then this
// node's half.
//
// [context.WithoutCancel], because the failure being undone is often the
// cancellation itself and a release that inherited a dead context would leave
// the claim to expire — sixty seconds during which nobody else may run this
// walk and the duty has not yet decided it was abandoned.
//
// THIS NODE'S HALF LAST, for [Writer.claim]'s reason: given back first, a
// goroutine here could take it and renew the lease this release is about to
// give up, and then walk under a lease that is no longer held.
func (h *held) release(ctx context.Context) {
	close(h.stop)
	<-h.done
	_, _ = h.claims.Release(context.WithoutCancel(ctx), h.resource, h.owner, h.epoch)
	h.local.give(h.resource)
}

// MoveTaskToProject re-homes a task and everything beneath it. SEQUENCE 7.
//
//	Rk, then take move/<task> → Rs the target project and its tag set;
//	refuse archived, refuse a required field the task lacks → A the tags
//	the subtree carries that the target lacks → A the alias on the former
//	key at expectation 0 → A the counter, a RANGE mint for the whole
//	subtree → A the root task, carrying the range's BASE and its length →
//	A per descendant on its own subject, in the (depth, id) order the range
//	was minted against → release.
//
// # Why the base rides the root record
//
// The range's base is NOT recoverable afterwards. By the time anything picks
// up an abandoned walk, other creates have advanced the counter — so a
// completion that recomputed the base would assign a different key to the same
// descendant on a different node, and the walk would stop being idempotent. The
// ordering by (depth, id) fixes the ORDER; only the base fixes the ORIGIN. That
// is what a completion needs; what exists to perform one is the residue note
// below.
//
// THE SOURCE PROJECT'S ORDER IS NOT REWRITTEN. The rows leave it entirely, so
// there is nothing to place; what covers a reader whose closure names the
// source is this sequence's own scope, which carries BOTH containers.
//
// CRASH RESIDUE: a tag declared with no task yet (harmless); an alias for a key
// still held (harmless — the apply never lowers `current`); a numbering gap of
// at most 1 + [MaxDescendants], which is the range this mints in one go; and
// descendants still keyed in the old project, which is the one that shows.
//
// REPAIRER: NOBODY YET, AND THE GAP IS STATED RATHER THAN IMPLIED. The walk is
// idempotent by construction — a descendant whose row already carries the
// target project writes nothing, and the base rides the root record (see
// [KeyMint]) precisely so a completion assigns the same keys on any node at any
// later time — but [Jobs] registers no job that finds a half-moved subtree, and
// re-issuing the gesture is REFUSED by the pre-flight below, which reads the
// root as already in the target. So a walk that dies mid-subtree leaves a
// subtree nothing in this build finishes. Completing it needs the same shape
// every other repair here has: a fact a writer stamped (the applier flagging a
// task whose project differs from its parent's, as it flags a long rank and an
// abandoned merge) plus a gated duty job over that flag — a scan for it would
// be a whole-table join on every tick, which is what this file's duty is
// against. Until then the honest report is that there is no repairer.
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

	// The subtree, read ONCE and ordered by (depth, id) — the ordering the
	// range assignment is a pure function of, so a duty completing this
	// walk on another node assigns every descendant the same key.
	var root Task
	var subtree []Task
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
		case current.Project == target:
			return fmt.Errorf("tracker: task %s is already in %s", taskID, target)
		}
		root = current
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
		subtree, err = readSubtree(ctx, tx, taskID)
		return err
	}); err != nil {
		return WriteResult{}, err
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
			if _, err := w.WriteDocument(ctx, stepID(opID, "tags"),
				TagsSubject(target), "", merged, ChangeTags, nil); err != nil {
				return WriteResult{}, fmt.Errorf("tracker: declare the moving "+
					"subtree's tags in %s: %w", target, err)
			}
		}
	}

	// THE ALIAS BEFORE THE RE-KEY, so a key somebody pastes into chat
	// keeps resolving from the moment it stops being current.
	//nolint:govet // shadow: scoped to this block; see .golangci.yml
	if _, err := w.claimAlias(ctx, stepID(opID, "alias"), root.Key, taskID, at); err != nil {
		return WriteResult{}, err
	}

	base, _, err := w.mintKey(ctx, stepID(opID, "counter"), target, 1+len(subtree), nil)
	if err != nil {
		return WriteResult{}, err
	}

	former := append(append([]string{}, root.FormerKeys...), root.Key)
	rootKey := fmt.Sprintf("%s-%d", target, base)
	rootRank, err := IntegerAt(base)
	if err != nil {
		return WriteResult{}, err
	}
	result, err := w.moveOne(ctx, stepID(opID, "root"), root, target, former,
		&KeyMint{N: base, Base: base, Length: 1 + len(subtree)})
	if err != nil {
		return result, err
	}

	for i, descendant := range subtree {
		n := base + uint64(i) + 1
		step := stepID(opID, fmt.Sprintf("d%d", i))
		if _, err := w.moveOne(ctx, step, descendant, target,
			append(append([]string{}, descendant.FormerKeys...), descendant.Key),
			&KeyMint{N: n}); err != nil {
			// NAMING WHAT IS LEFT BEHIND rather than promising a
			// repair: no duty job completes this walk, and the root is
			// already in the target by now, so re-issuing the gesture
			// is refused by the pre-flight above. The subtree is split
			// until somebody moves the rest, and a caller told
			// "idempotent, it will sort itself out" would never look.
			return result, fmt.Errorf("tracker: task %s moved to %s with %d of "+
				"%d descendants; the rest are still in %s and nothing "+
				"completes this walk on its own — re-issuing the move is "+
				"refused because the root has already moved: %w",
				taskID, target, i, len(subtree), root.Project, err)
		}
	}
	result.Key, result.Rank = rootKey, rootRank
	return result, nil
}

// moveOne re-homes one task of a moving subtree.
func (w *Writer) moveOne(ctx context.Context, opID string, task Task,
	target string, former []string, mint *KeyMint) (WriteResult, error) {

	patch := TaskPatch{Project: &target, FormerKeys: &former, Mint: mint}
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
		Subject:  wire(subject),
		Scope:    scope.Resolve(subject),
		OpID:     opID,
		MintedAt: at,
		Pattern:  statelog.PatternCreate,
		Decide: func(tx *sql.Tx) (statelog.Decision, error) {
			if owner, claimed, err := readAlias(ctx, tx, key); err != nil {
				return statelog.Decision{}, err
			} else if claimed && owner != taskID {
				return statelog.Decision{}, fmt.Errorf("tracker: key %s belongs "+
					"to task %s, so it cannot be aliased to %s", key, owner, taskID)
			}
			return w.decide(subject, OpCreate, "", scope, opID, KeyAlias{
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
// CRASH RESIDUE: children partly re-parented. REPAIRER: the tracker duty, and
// it is idempotent for the same reason the move's walk is — a child already
// carrying the new parent is not selected, so a completion writes only what is
// left.
//
// THE MARKER IS CLEARED LAST. While it stands, the duplicate is visibly
// mid-merge rather than silently half-merged, which is the difference between
// a state somebody can wait out and one they have to reconstruct.
//
// # The target is read by every append that depends on it, and by no read
// before them
//
// A read before the first append answers for a moment none of the appends is
// decided in, and a purge of the target landing after it would see subtasks
// re-parented onto a task no row holds and the duplicate cancelled as merged
// into nothing. So the mark and the close read the target in their own decide
// snapshots ([Writer.mergingInto]), and every re-parent reads it as the
// parent it is about to write ([refusePurged]). A purge the mark sees refuses
// the merge before anything is written; one a later step sees gives it up
// ([Writer.abandonMerge]); both return [ErrPurged].
//
// The same holds for everything else those reads found ([mergeStep]): a
// subtask moved elsewhere or put in the trash after the walk read it is passed
// over rather than moved back, and a close or a give-up that finds the merge
// already ended writes nothing.
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
	// THE DUPLICATE'S OWN PROJECT is what this read is for, which every
	// append below names — and a removed duplicate is refused here, whole,
	// rather than leaving a marker with nothing to finish it.
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
	marked, err := w.mergingInto(into).UpdateTask(ctx, stepID(opID, "mark"),
		duplicate, task.Project, NoIfMatch,
		TaskPatch{Relate: &RelationIntent{Add: []Relation{{
			Kind: RelationDuplicates, Other: into,
		}}}, Merging: &merging, MergeReparent: &reparent},
		ChangeRelations, nil)
	if err != nil {
		return WriteResult{}, err
	}
	end, err := w.finishMerge(ctx, opID, duplicate, task.Project, into, reparent,
		marked.Position, notify)
	switch {
	case errors.Is(err, errMergeOver):
		return WriteResult{}, fmt.Errorf("tracker: the merge of %s into %s had "+
			"been ended by another writer when this call came to close it: %w",
			duplicate, into, err)
	case err != nil:
		return WriteResult{}, err
	case end.Purged != nil:
		return WriteResult{}, fmt.Errorf("tracker: the merge of %s into %s was "+
			"given up after it began: %w — %s is left open with no merge "+
			"marker, and a subtask already moved onto %s went where that "+
			"purge moved its children", duplicate, into, end.Purged,
			duplicate, into)
	}
	return end.Result, nil
}

// mergeEnd is how everything a merge does after its mark came out.
type mergeEnd struct {
	// Result is the close's own, or the marker's clear when the merge was
	// given up.
	Result WriteResult

	// Moved is how many subtasks this run re-parented onto the target.
	Moved int

	// Purged is the refusal that gave the merge up — the target was
	// purged after the mark — and nil when the merge closed.
	Purged error
}

// finishMerge is everything a merge does after its mark: the subtasks moved
// onto the target if the merge said to, then the close — or, when a step finds
// the target purged, the merge given up ([Writer.abandonMerge]).
//
// SHARED BY THE SEQUENCE AND THE DUTY, so the repair of a merge whose holder
// died is a re-run of the steps the holder did not reach rather than a second
// account of what a merge is. marked is the mark's own position, which the
// close has to see; the duty passes zero, because the mark it finishes was
// applied before its scan could read it.
//
// A merge found already ended — its marker cleared by a close or a give-up
// that landed first — is returned as [errMergeOver], with nothing written.
func (w *Writer) finishMerge(ctx context.Context, opID, duplicate, project, into string,
	reparent bool, marked statelog.Position, notify *Notify) (mergeEnd, error) {

	var end mergeEnd
	if reparent {
		moved, err := w.reparentOnto(ctx, opID, duplicate, into)
		end.Moved = moved
		switch {
		case errors.Is(err, ErrPurged):
			return w.abandonMerge(ctx, opID, duplicate, project, marked, end, err)
		case err != nil:
			return end, err
		}
	}
	cancelled := StatusCancelled
	done := false
	// THE CLOSE LANDS ON THE SUBJECT THE MARK ALREADY MOVED, and the
	// re-parented children in between are on subjects of their own — so
	// the mark's position is what this last append has to see, whatever
	// the loop above published. See [Writer.After].
	//
	// AND IT IS ONE APPEND: the cancelled status and the marker's clear
	// together, on the duplicate's own subject. Split in two it would leave
	// a cancelled task still marked mid-merge, which the duty would pick up
	// again on every tick for ever.
	closed, err := w.After(marked).mergingInto(into).whileMerging().UpdateTask(ctx,
		stepID(opID, "close"), duplicate, project, NoIfMatch,
		TaskPatch{Status: &cancelled, Merging: &done}, ChangeStatus, notify)
	switch {
	case errors.Is(err, ErrPurged):
		return w.abandonMerge(ctx, opID, duplicate, project, marked, end, err)
	case err != nil:
		return end, err
	}
	end.Result = closed
	return end, nil
}

// abandonMerge gives up a merge whose target was purged after its mark: the
// marker is cleared and the duplicate left open, with whatever subtasks it
// still has.
//
// # Why not finish it, and why not leave it
//
// Finishing it would cancel the duplicate as merged into a task that no longer
// exists, which closes an item nobody merged. Leaving the marker would hand the
// duty a merge whose every step meets the same refusal on every tick. So the
// merge did not happen and now cannot, and the marker goes — the same repair
// the duty makes for a marker naming no target at all.
//
// A subtask the walk moved before a step saw the purge is not moved back: it
// is where that purge puts the target's children — the ones it found
// ([Applier.purgeTask]) and the ones that arrived after it ([placedParent]).
func (w *Writer) abandonMerge(ctx context.Context, opID, duplicate, project string,
	marked statelog.Position, end mergeEnd, cause error) (mergeEnd, error) {

	done := false
	cleared, err := w.After(marked).whileMerging().UpdateTask(ctx,
		stepID(opID, "abandon"), duplicate, project, NoIfMatch,
		TaskPatch{Merging: &done}, ChangeFields, nil)
	if err != nil {
		// THE CAUSE AS TEXT AND NOT WRAPPED: what failed here is the clear,
		// and a caller asking whether this error is the purge would be told
		// yes about a merge whose marker may still stand.
		return end, fmt.Errorf("tracker: give up the merge of %s, whose "+
			"target was purged (%s): %w", duplicate, cause.Error(), err)
	}
	end.Result, end.Purged = cleared, cause
	return end, nil
}

// mergeTargetHeld refuses a merge step whose target has been purged or is not
// on this node — [Writer.mergingInto], read inside the step's own decide.
func mergeTargetHeld(ctx context.Context, tx *sql.Tx, duplicate, into string) error {
	if err := refusePurged(ctx, tx, into, "merge target"); err != nil {
		return err
	}
	var present int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM tracker_tasks WHERE id = ?`, into).Scan(&present); err != nil {
		return fmt.Errorf("tracker: read the merge target %s: %w", into, err)
	}
	if present == 0 {
		return fmt.Errorf("tracker: task %s, which %s is being merged into, is "+
			"not on this node: %w", into, duplicate, statelog.ErrUnavailable)
	}
	return nil
}

// reparentOnto moves whatever is left of a duplicate's subtasks onto the
// canonical task, in batches of [WalkBatch].
//
// SHARED BY THE SEQUENCE AND THE DUTY, which is what [readChildBatch]'s
// selection buys: the duty's repair is a RE-RUN of the batch the holder did
// not reach, not a second algorithm that has to keep agreeing with this one.
// A merge's children are one of the TWO walks that page until drained (the
// other is a project's re-spread; [WalkBatch] states what the number buys at
// each of its shapes), and here it buys the two a page buys: a read bounded
// whatever the subtree's size, and a burst bounded the same way.
//
// IT DOES NOT BOUND THE STRETCH BETWEEN THE CLAIM'S HEARTBEATS, which is the
// tempting second reason and is not true of this code: [held.beat] renews on a
// ticker in a goroutine of its own, every [ClaimHeartbeat] whatever this loop
// is doing, so no batch size could lengthen or shorten that.
//
// THE STEP ID IS KEYED ON THE CHILD rather than on its position in the walk,
// because a re-run's batches do not divide the same way: the moved ones are
// gone from the selection, so an index would give a different child the same
// operation id and the ledger would answer one move's question with another's
// row.
//
// EACH MOVE ASKS WHETHER ITS SUBTASK IS STILL ONE ([Writer.movingOutOf]),
// because the batch was read before it: a subtask re-parented elsewhere since
// then would otherwise be moved back over somebody's decision, and one in the
// trash — read here like any other child — would be refused on every attempt.
// Either is passed over, and does not count as moved.
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
			_, err := w.movingOutOf(duplicate).UpdateTask(ctx,
				stepID(opID, "c/"+child.ID), child.ID, child.Project, NoIfMatch,
				TaskPatch{Parent: &into}, ChangeReparented, nil)
			switch {
			case errors.Is(err, errLeftTheMerge):
				// NOT THIS MERGE'S TO MOVE ANY MORE — re-parented
				// elsewhere or put in the trash since the batch was read
				// — so it is passed over rather than moved back.
				continue
			case errors.Is(err, ErrPurged):
				// THE TARGET IS GONE, and that is the one refusal no
				// repair completes: it is returned as it is, for the
				// caller to give the merge up.
				return moved, err
			case err != nil:
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
// task and the caller re-runs; the failures carry the current version in them.
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
	for i, id := range subjects {
		one, err := w.UpdateTask(ctx, stepID(opID, fmt.Sprintf("b%d", i)),
			id, project, NoIfMatch, patch, kind, notify)
		if err != nil {
			result.Failed[id] = err.Error()
			continue
		}
		result.Applied = append(result.Applied, id)
		result.Result = one.Result
	}
	return result, nil
}

// admit takes the bulk claim — this node's half and the fleet's lease — or
// reports why it did not.
//
// THREE ANSWERS AND THREE BEHAVIOURS, which is why [Claims] is not a bool: a
// held lease runs, a peer's lease refuses with the holder's remaining time as
// the caller's hint, and a coordination store that cannot be reached ADMITS —
// see the sequence's own doc for why those last two must differ.
//
// A BULK ALREADY APPLYING ON THIS NODE is refused as a peer's is, and before
// the lease is asked for: the lease is this node's, so asking again would renew
// it rather than refuse ([localClaims]). That holds with the store unreachable
// too, because a bulk running here is something this node knows rather than
// something the store has to say.
func (w *Writer) admit(ctx context.Context, rows int) (func(), error) {
	if w.claims == nil {
		return func() {}, nil
	}
	resource := bulkClaim(trackerStream)
	// TWICE THE PROJECTED APPLY TIME, so the lease outlives the work it
	// admits without outliving it by so much that a crashed holder blocks
	// the company. The projection is the bulk's records — one per task —
	// over the applier's own measured drain in records a second, the same
	// rate a refused read's retry hint divides its backlog by.
	projected := float64(rows) / w.drainRows()
	ttl := 2 * time.Duration(projected*float64(time.Second))
	if ttl < ClaimHeartbeat {
		ttl = ClaimHeartbeat
	}
	if !w.local.take(resource) {
		return nil, w.bulkInFlight(ctx, resource)
	}
	lease, err := w.claims.TryAcquire(ctx, resource, coord.AcquireOptions{
		Owner: w.nodeID, TTL: ttl,
	})
	switch {
	case err != nil:
		// FAIL OPEN. See the doc above: the log is correct with two
		// bulks in flight and merely slow, and refusing here on an
		// unknown is a seat told a colleague is editing when nobody is.
		w.occupy(projected)
		//nolint:nilerr // Deliberate fail-open: see the paragraph above.
		return func() { w.local.give(resource) }, nil
	case lease == nil:
		w.local.give(resource)
		return nil, w.bulkInFlight(ctx, resource)
	}
	w.occupy(projected)
	epoch := lease.Epoch
	return func() {
		// THE LEASE FIRST, for [held.release]'s reason.
		_, _ = w.claims.Release(context.WithoutCancel(ctx), resource, w.nodeID, epoch)
		w.local.give(resource)
	}, nil
}

// occupy records the applier occupancy an ADMITTED bulk is about to impose.
//
// THE PROJECTION IS WHAT IS SUMMED, not the wall clock: it is the occupancy
// this call is about to impose on EVERY node, and the wall clock here would
// measure only this one. And only an admitted call's, because the counter is
// the fleet's read-degradation budget and a refused bulk applies nothing.
func (w *Writer) occupy(projected float64) {
	if w.metrics != nil {
		w.metrics.AddValue(metrics.TrackerBulkApplySeconds, projected, nil)
	}
}

// bulkInFlight refuses a bulk while another applies, with the time left on the
// holder's lease as the caller's hint.
func (w *Writer) bulkInFlight(ctx context.Context, resource string) error {
	remaining := time.Duration(0)
	if holder, err := w.claims.Get(ctx, resource); err == nil && holder != nil {
		remaining = time.Until(holder.ExpiresAt)
	}
	return fmt.Errorf("%w; retry in about %d seconds: %w",
		ErrBulkInFlight, int(max(remaining.Seconds(), 1)),
		statelog.ErrUnavailable)
}

// drainRows is the applier's measured drain in RECORDS a second, and the
// divisor of every projection computed from it.
//
// A FLOOR RATHER THAN A DEFAULT, because dividing by a drain that has not been
// measured yet — zero — is an infinite lease, which is worse than any wrong
// number. One record a second is deliberately pessimistic: it makes the first
// projection too long rather than too short, and a lease held too long delays a
// caller where one held too briefly admits the concurrency it exists to stop.
// A measured rate at or below it is floored too.
func (w *Writer) drainRows() float64 {
	if w.Drain == nil {
		return 1
	}
	if rows := w.Drain(); rows > 1 {
		return rows
	}
	return 1
}
