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

// The FIVE sequences that stay genuinely cross-object — a create (1), an item
// promotion (1a), a merge (13) and a bulk edit (26) here, and a dependency in
// depend.go — and the one property that makes them tolerable.
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
	// THE SAME 64 as every other fan-out in this design — a merge batch,
	// the scope term cap — because they are the same
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

	// WalkBatch is one batch of a paced walk — a merge's children, and
	// every duty selection that finishes one.
	WalkBatch = 64

	// ClaimTTL is how long the durable claim a walking sequence holds
	// survives unrenewed. FOUR HEARTBEATS, so three consecutive misses are
	// survivable and the fourth hands the walk to the duty — which takes
	// the same claim before it finishes anything, so a walk whose holder is
	// alive is one it leaves alone.
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
// third while a merge's walk fails closed over it. Collapsed to two
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
	classMerge coord.Class = "merge"
)

func bulkClaim(domain string) string { return classBulk.Resource(domain) }
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
	n, minted, err := w.mintKey(ctx, stepID(opID, "counter"), task.Project,
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
			return w.decide(subject, OpCreate, ChangeCreated, scope, opID,
				task, notify, at)
		},
	})
	return WriteResult{
		Result: result, Warnings: bodyWarnings(task.Body),
	}, err
}

// mintKey takes the next key number in a project. SEQUENCE 1's first append,
// and 1a's.
//
// # Why the number is a record and not a row read
//
// A counter read inside the snapshot says what this node has applied. Two
// nodes reading it would read the same number and mint two tasks with one key,
// which is precisely what a conditional append on the counter's own subject
// makes impossible: the loser is rejected, re-decides against the winner's
// number and takes the next one.
func (w *Writer) mintKey(ctx context.Context, opID, project string,
	guard func(*sql.Tx) error) (uint64, statelog.Result, error) {

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
				V: DocumentVersion, Project: project, Last: counter.Last + 1,
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
	if task.Parent != nil && *task.Parent != "" {
		if err := refuseParent(ctx, tx, task, *task.Parent); err != nil {
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

// refuseParent refuses a parent a task cannot be filed under.
//
// # A SUBTREE LIVES IN ONE PROJECT
//
// A board draws a project's ROOTS and lets their subtrees ride along, and the
// query carries the container on the outer row as well — so a subtask filed in
// another project from its root is on neither project's board: its root's
// filters it out by project, and its own finds no root to hang it from. The
// attention set's `inconsistent_project` names the shape; this is what keeps
// a writer from making it. A task's project never changes — its key is minted
// in it — so the parent's project read in THIS snapshot is the one it will
// always have, and a refusal here closes the shape rather than flagging it
// afterwards.
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
	case parent.Project != task.Project:
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
	// STATED BEFORE THE MINT, so the create's own parent check reads the
	// parent in the mint's snapshot like every other subtask's.
	subtask.Parent = &parentID

	var parent Task
	n, minted, err := w.mintKey(ctx, stepID(opID, "counter"), subtask.Project,
		func(tx *sql.Tx) error {
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

// errWalkRunning is a walk somebody is already running: another node's lease,
// or another goroutine on this one.
var errWalkRunning = errors.New("tracker: this walk is already running")

// localHolds is the IN-PROCESS half of a claim, and it exists because the
// lease cannot be the whole of one.
//
// A lease names its holder by NODE, and a claim by an owner that already holds
// it DOUBLES AS A RENEW (see [coord.Backend.TryAcquire]) — so two goroutines on
// one node, two requests to fold one duplicate or the duty finishing a merge
// this node is still walking, were both told yes. And the first of them to
// finish RELEASED the lease the other was still walking under, admitting a
// third walk from anywhere in the fleet. One map shared by every copy of the
// node's writer closes both.
type localHolds struct {
	mu   sync.Mutex
	held map[string]bool
}

// take claims a resource in this process, reporting whether it was free.
func (l *localHolds) take(resource string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.held[resource] {
		return false
	}
	l.held[resource] = true
	return true
}

// give releases a resource this process took.
func (l *localHolds) give(resource string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.held, resource)
}

// held is one durable claim, heartbeated for as long as a walk runs.
type held struct {
	claims   Claims
	local    *localHolds
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
func (w *Writer) hold(ctx context.Context, resource string) (*held, error) {
	if w.claims == nil || w.local == nil {
		return nil, fmt.Errorf("tracker: this writer has no coordination, so "+
			"it cannot take %s — a walking sequence without a claim is two "+
			"nodes rewriting one subtree", resource)
	}
	// THIS PROCESS FIRST, because the lease cannot see a second walk here:
	// the same owner's claim is a renew.
	if !w.local.take(resource) {
		return nil, fmt.Errorf("%w on this node (%s): %w", errWalkRunning,
			resource, statelog.ErrUnavailable)
	}
	lease, err := w.claims.TryAcquire(ctx, resource, coord.AcquireOptions{
		Owner: w.nodeID, TTL: ClaimTTL,
	})
	if err != nil || lease == nil {
		w.local.give(resource)
	}
	switch {
	case err != nil:
		// UNKNOWN, AND THIS ONE FAILS CLOSED. A merge re-parents a
		// duplicate's whole subtree; two of them interleaved — one
		// duplicate folded into two different tasks at once — split its
		// children between two parents, and nothing afterwards can say
		// which fold each child belonged to.
		return nil, fmt.Errorf("tracker: take %s: %w", resource, err)
	case lease == nil:
		return nil, fmt.Errorf("%w on another node (%s): %w", errWalkRunning,
			resource, statelog.ErrUnavailable)
	}
	h := &held{
		claims: w.claims, local: w.local, resource: resource, owner: w.nodeID,
		epoch: lease.Epoch, stop: make(chan struct{}), done: make(chan struct{}),
	}
	go h.beat(context.WithoutCancel(ctx))
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
	// THE LEASE FIRST, so no other goroutine here takes the resource and
	// claims a lease this release is about to drop.
	h.local.give(h.resource)
}

// MergeDuplicates folds one task into another. SEQUENCE 13.
//
//	Rk, then take merge/<task> → A on the duplicate (the merge marker and
//	the `duplicates` relation) → per batch of ≤64: A re-parenting each
//	child onto Into → A clearing the marker and writing the cancelled
//	status → release.
//
// CRASH RESIDUE: children partly re-parented. REPAIRER: the tracker duty, and
// it is idempotent BY SELECTION — a child already carrying the new parent is
// not selected, so a completion writes only what is left.
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
			return absentTask(ctx, tx, duplicate, "task")
		case current.Removed != nil:
			return fmt.Errorf("tracker: task %s was removed by %s at %s; "+
				"restore it before merging it", duplicate,
				current.Removed.By, current.Removed.At.Format(time.RFC3339))
		}
		task = current
		canonical, held, err := readTask(ctx, tx, into)
		switch {
		case err != nil:
			return err
		case !held:
			return absentTask(ctx, tx, into, "task")
		case canonical.Removed != nil:
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
				canonical.Key, into, canonical.Removed.By,
				canonical.Removed.At.Format(time.RFC3339))
		case reparent && canonical.Project != current.Project:
			// REFUSED BEFORE THE MARK, because every re-parent below
			// would be refused on its own subject — a subtask lives in
			// its parent's project — and a mark with nothing that can
			// finish it is a merge the duty retries for ever.
			return fmt.Errorf("tracker: %s is filed under %s and %s under "+
				"%s, so the duplicate's subtasks cannot move onto it — a "+
				"subtask lives in its parent's project. Merge without "+
				"moving the subtasks to leave them where they are",
				current.Key, current.Project, canonical.Key, canonical.Project)
		}
		if !reparent {
			return nil
		}
		// AND NOT INTO ITS OWN SUBTREE when the subtasks move, for the
		// same reason: the child the survivor sits under would be
		// re-parented onto the survivor, which [refuseParent] refuses
		// as a cycle — so the mark would stand with nothing to finish it.
		under, err := inSubtree(ctx, tx, duplicate, into)
		switch {
		case err != nil:
			return err
		case under:
			return fmt.Errorf("tracker: %s is one of %s's own subtasks, so "+
				"the duplicate's subtasks cannot move onto it — one of them "+
				"would end up under itself. Merge without moving the "+
				"subtasks to leave them where they are",
				canonical.Key, current.Key)
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
	if err != nil {
		return WriteResult{}, err
	}

	if reparent {
		if _, err := w.reparentOnto(ctx, opID, duplicate, into); err != nil {
			return WriteResult{}, err
		}
	}

	cancelled := StatusCancelled
	done := false
	// THE CLOSE LANDS ON THE SUBJECT THE MARK ALREADY MOVED, and the
	// re-parented children in between are on subjects of their own — so
	// the mark's position is what this last append has to see, whatever
	// the loop above published. See [Writer.After].
	return w.After(marked.Position).UpdateTask(ctx, stepID(opID, "close"),
		duplicate, task.Project, NoIfMatch,
		TaskPatch{Status: &cancelled, Merging: &done}, ChangeStatus, notify)
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
			if _, err := w.UpdateTask(ctx, stepID(opID, "c/"+child.ID),
				child.ID, child.Project, NoIfMatch, TaskPatch{Parent: &into},
				ChangeReparented, nil); err != nil {
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

// admit takes the fleet-wide bulk lease, or reports why it did not.
//
// THREE ANSWERS AND THREE BEHAVIOURS, which is why [Claims] is not a bool: a
// held lease runs, a peer's lease refuses with the holder's remaining time as
// the caller's hint, and a coordination store that cannot be reached ADMITS —
// see the sequence's own doc for why those last two must differ.
func (w *Writer) admit(ctx context.Context, rows int) (func(), error) {
	if w.claims == nil || w.local == nil {
		return func() {}, nil
	}
	resource := bulkClaim(trackerStream)
	// THIS PROCESS FIRST, for [localHolds]' reason: a second bulk from this
	// node renews the lease the first holds rather than being refused by
	// it, and the first to finish releases the lease the other is still
	// applying under. Refused whatever the coordination store says, because
	// this half is known locally and is never an unknown.
	if !w.local.take(resource) {
		return nil, w.bulkInFlight(ctx, resource)
	}
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
	lease, err := w.claims.TryAcquire(ctx, resource, coord.AcquireOptions{
		Owner: w.nodeID, TTL: ttl,
	})
	switch {
	case err != nil:
		// FAIL OPEN. See the doc above: the log is correct with two
		// bulks in flight and merely slow, and refusing here on an
		// unknown is a seat told a colleague is editing when nobody is.
		// The in-process half still holds until this bulk ends.
		//nolint:nilerr // Deliberate fail-open: see the paragraph above.
		return func() { w.local.give(resource) }, nil
	case lease == nil:
		w.local.give(resource)
		return nil, w.bulkInFlight(ctx, resource)
	}
	epoch := lease.Epoch
	return func() {
		_, _ = w.claims.Release(context.WithoutCancel(ctx), resource, w.nodeID, epoch)
		w.local.give(resource)
	}, nil
}

// bulkInFlight is the refusal of a bulk while another applies, with the
// holder's remaining lease as the caller's hint — whichever node holds it,
// this one included, since the lease is what bounds the other bulk's run.
func (w *Writer) bulkInFlight(ctx context.Context, resource string) error {
	remaining := time.Duration(0)
	if held, err := w.claims.Get(ctx, resource); err == nil && held != nil {
		remaining = time.Until(held.ExpiresAt)
	}
	return fmt.Errorf("%w; retry in about %d seconds: %w",
		ErrBulkInFlight, int(max(remaining.Seconds(), 1)),
		statelog.ErrUnavailable)
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
