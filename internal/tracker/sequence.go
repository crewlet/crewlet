package tracker

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
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
// "between" and therefore no crash residue. Six gestures cannot reach that
// shape, because the object they mint from and the object they write are
// different objects with different arbitration — a key comes from a project's
// counter and lands on a task, a sprint's pointer lives on its project.
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
// no subtask behind it. A sprint moves the project's POINTER first, because
// that pointer is where two nodes racing to start sprint 7 are arbitrated.
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
	// descendants, a merge's children, a sprint rollover's spill.
	WalkBatch = 64

	// ClaimTTL and ClaimHeartbeat are the durable claim a walking sequence
	// holds. The TTL is four heartbeats, so three consecutive misses are
	// survivable and the fourth hands the walk to the duty.
	ClaimTTL       = 60 * time.Second
	ClaimHeartbeat = 15 * time.Second

	// ClaimStale is when a duty may complete somebody else's abandoned
	// walk: half the TTL past its last heartbeat, which is the point at
	// which a holder that is still alive would have renewed twice.
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
func bulkClaim(domain string) string { return "bulk/" + domain }
func moveClaim(task string) string   { return "move/" + task }
func mergeClaim(task string) string  { return "merge/" + task }
func rolloverClaim(p string, n int) string {
	return fmt.Sprintf("rollover/%s/%d", p, n)
}

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

	n, minted, err := w.mintKey(ctx, stepID(opID, "counter"), task.Project, 1,
		func(tx *sql.Tx) error { return refuseCreate(ctx, tx, task) })
	if err != nil {
		return WriteResult{Result: minted}, err
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
			return w.decide(subject, OpCreate, scope, opID, task, notify, at)
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
			decision, err := w.decide(subject, OpPatch, scope, opID, next, nil, at)
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
func refuseCreate(ctx context.Context, tx *sql.Tx, task Task) error {
	project, held, err := readProject(ctx, tx, task.Project)
	switch {
	case err != nil:
		return err
	case !held:
		return fmt.Errorf("tracker: project %s is not on this node: %w",
			task.Project, statelog.ErrUnavailable)
	case project.Archived:
		return fmt.Errorf("tracker: project %s is archived, so it takes no new "+
			"work; unarchive it first", task.Project)
	}
	return requiredFields(project, task)
}

// requiredFields refuses a task missing a field its project declares.
//
// A SUBTASK IS JUDGED BY A DIFFERENT FLAG. ClickUp ships two toggles and so
// does this, and the default that matters is the second: a field required on a
// task is NOT required on a subtask unless the definition says so — otherwise
// one required field blocks every checklist item anybody promotes.
func requiredFields(project Project, task Task) error {
	var missing []string
	for _, f := range project.Fields {
		required := f.Required
		if task.Parent != nil && *task.Parent != "" {
			required = f.RequiredInSubtasks
		}
		if !required || f.Archived {
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
	return fmt.Errorf("tracker: project %s requires %v, which this task does "+
		"not set", project.Key, missing)
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

	var parent Task
	n, minted, err := w.mintKey(ctx, stepID(opID, "counter"), subtask.Project, 1,
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
			return refuseCreate(ctx, tx, subtask)
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
		parent.Project, NoIfMatch, TaskPatch{Checklists: &lists}, nil)
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
func (w *Writer) hold(ctx context.Context, resource string) (*held, error) {
	if w.claims == nil {
		return nil, fmt.Errorf("tracker: this writer has no coordination, so "+
			"it cannot take %s — a walking sequence without a claim is two "+
			"nodes rewriting one subtree", resource)
	}
	lease, err := w.claims.TryAcquire(ctx, resource, coord.AcquireOptions{
		Owner: w.nodeID, TTL: ClaimTTL,
	})
	switch {
	case err != nil:
		// UNKNOWN, AND THIS ONE FAILS CLOSED. A cross-project move
		// rewrites a whole subtree's keys; two of them interleaved
		// produce a subtree keyed into two projects, which no duty can
		// tell from an abandoned walk.
		return nil, fmt.Errorf("tracker: take %s: %w", resource, err)
	case lease == nil:
		return nil, fmt.Errorf("tracker: %s is held by another node, so this "+
			"walk is already running: %w", resource, statelog.ErrUnavailable)
	}
	h := &held{
		claims: w.claims, resource: resource, owner: w.nodeID,
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
}

// MoveTaskToProject re-homes a task and everything beneath it. SEQUENCE 7.
//
//	Rk, then take move/<task> → Rs the target project and its tag set;
//	refuse archived, refuse a required field the task lacks → A the tags
//	the subtree carries that the target lacks → A the alias on the former
//	key at expectation 0 → A the counter, a RANGE mint for the whole
//	subtree → A the root task, carrying the range's BASE and its length →
//	per batch of ≤64 descendants, A per descendant on its own subject →
//	release.
//
// # Why the base rides the root record
//
// The range's base is NOT recoverable afterwards. By the time a duty completes
// an abandoned walk, other creates have advanced the counter — so a duty that
// recomputed the base would assign a different key to the same descendant on a
// different node, and the walk would stop being idempotent. The ordering by
// (depth, id) fixes the ORDER; only the base fixes the ORIGIN.
//
// THE SOURCE PROJECT'S ORDER IS NOT REWRITTEN. The rows leave it entirely, so
// there is nothing to place; what covers a reader whose closure names the
// source is this sequence's own scope, which carries BOTH containers.
//
// CRASH RESIDUE: a tag declared with no task yet (harmless); an alias for a key
// still held (harmless — the apply never lowers `current`); a numbering gap of
// at most 64; descendants still keyed in the old project. REPAIRER: the tracker
// duty, on a claim whose heartbeat aged past [ClaimStale], completes the walk
// idempotently — a descendant whose row already carries the target project
// writes nothing.
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
	if err := w.db.Replicated().Read(ctx, func(tx *sql.Tx) error {
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
		if err := requiredFields(project, current); err != nil {
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
		set, err := w.tagsOf(ctx, target)
		if err != nil {
			return WriteResult{}, err
		}
		if merged, changed := mergeTags(set, newTags); changed {
			if _, err := w.WriteDocument(ctx, stepID(opID, "tags"),
				TagsSubject(target), "", merged, nil); err != nil {
				return WriteResult{}, fmt.Errorf("tracker: declare the moving "+
					"subtree's tags in %s: %w", target, err)
			}
		}
	}

	// THE ALIAS BEFORE THE RE-KEY, so a key somebody pastes into chat
	// keeps resolving from the moment it stops being current.
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
			return result, fmt.Errorf("tracker: %d of %d descendants moved; the "+
				"tracker duty completes the rest idempotently: %w",
				i, len(subtree), err)
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
	return w.UpdateTask(ctx, opID, task.ID, task.Project, NoIfMatch, patch, nil)
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
			return w.decide(subject, OpCreate, scope, opID, KeyAlias{
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
	var children []Task
	if w.db == nil {
		return WriteResult{}, fmt.Errorf("tracker: this writer has no store, " +
			"so it cannot read the children a merge re-parents")
	}
	if err := w.db.Replicated().Read(ctx, func(tx *sql.Tx) error {
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
		if !reparent {
			return nil
		}
		children, err = readSubtree(ctx, tx, duplicate)
		return err
	}); err != nil {
		return WriteResult{}, err
	}

	relations := append(append([]Relation{}, task.Relations...), Relation{
		Kind: RelationDuplicates, Other: into,
		CreatedBy: w.Actor, CreatedAt: w.Now(),
	})
	merging := true
	if _, err := w.UpdateTask(ctx, stepID(opID, "mark"), duplicate, task.Project, NoIfMatch,
		TaskPatch{Relations: &relations, Merging: &merging}, nil); err != nil {
		return WriteResult{}, err
	}

	for i, child := range children {
		if child.Parent != nil && *child.Parent == into {
			continue
		}
		if _, err := w.UpdateTask(ctx, stepID(opID, fmt.Sprintf("c%d", i)),
			child.ID, child.Project, NoIfMatch, TaskPatch{Parent: &into}, nil); err != nil {
			return WriteResult{}, fmt.Errorf("tracker: %d of %d children "+
				"re-parented onto %s; the tracker duty completes the rest: %w",
				i, len(children), into, err)
		}
	}

	cancelled := StatusCancelled
	done := false
	return w.UpdateTask(ctx, stepID(opID, "close"), duplicate, task.Project, NoIfMatch,
		TaskPatch{Status: &cancelled, Merging: &done}, notify)
}

// StartSprint moves a project's active sprint on. SEQUENCE 18.
//
//	Rs the sprint row; refuse unless it is `future` → A on the PROJECT,
//	setting the active-sprint pointer at its own version — THIS IS WHERE
//	THE TWO-NODE RACE IS ARBITRATED → A on the sprint, future → active.
//
// CRASH RESIDUE: the pointer moved and the record did not — a project naming a
// sprint that is still `future`. REPAIRER: the tracker duty reads both rows and
// completes whichever half is missing; the sprint record's own state machine is
// what makes that idempotent.
//
// # Why the pointer moves FIRST
//
// The pointer is the authority for "is another sprint running", so it is what
// two nodes starting sprint 7 inside one apply linger contend on. Moving the
// record first would let both pass their own state check and leave the pointer
// to arbitrate a decision both had already published.
//
// The record keeps its own, separate veto: a start moves `future → active` and
// refuses any other state, so an already-closed sprint can never be resurrected
// past a nil pointer.
func (w *Writer) StartSprint(ctx context.Context, opID, project string,
	number int) (WriteResult, error) {

	return w.moveSprint(ctx, opID, project, number, SprintFuture, SprintActive)
}

// CloseSprint ends the active one. SEQUENCE 19, and the same order as 18 for
// the same reason.
func (w *Writer) CloseSprint(ctx context.Context, opID, project string,
	number int) (WriteResult, error) {

	return w.moveSprint(ctx, opID, project, number, SprintActive, SprintClosed)
}

// moveSprint is 18 and 19, which differ only in which transition they permit.
func (w *Writer) moveSprint(ctx context.Context, opID, project string, number int,
	from, to SprintState) (WriteResult, error) {

	switch {
	case project == "":
		return WriteResult{}, fmt.Errorf("tracker: a sprint transition names no project")
	case number < 1:
		return WriteResult{}, fmt.Errorf("tracker: %d is not a sprint number", number)
	}
	at := w.Now()

	// THE POINTER, at the project's own version.
	pointer := &number
	if to != SprintActive {
		pointer = nil
	}
	if _, err := w.setActiveSprint(ctx, stepID(opID, "pointer"), project,
		number, from, to, pointer, at); err != nil {
		return WriteResult{}, err
	}

	// THE RECORD, with its own state machine as the second veto.
	subject := SprintSubject(project, number)
	scope := ScopeSet{Subject: true}
	return w.published(ctx, statelog.Request{
		Subject:  wire(subject),
		Scope:    scope.Resolve(subject),
		OpID:     stepID(opID, "sprint"),
		MintedAt: at,
		Pattern:  statelog.PatternArbitrated,
		Decide: func(tx *sql.Tx) (statelog.Decision, error) {
			sprint, held, err := readSprint(ctx, tx, project, number)
			switch {
			case err != nil:
				return statelog.Decision{}, err
			case !held:
				return statelog.Decision{}, fmt.Errorf("tracker: sprint %s.%d "+
					"is not on this node: %w", project, number,
					statelog.ErrUnavailable)
			case sprint.State == to:
				// ALREADY THERE IS THE REPAIR'S OWN PATH: the duty
				// completing an interrupted transition finds the half
				// that landed and writes the half that did not.
				return statelog.Decision{}, statelog.ErrExists
			case sprint.State != from:
				return statelog.Decision{}, fmt.Errorf("tracker: sprint %s.%d "+
					"is %s, and only a %s sprint becomes %s — a sprint's state "+
					"moves one way, whatever its project's pointer says",
					project, number, sprint.State, from, to)
			}
			sprint.State = to
			sprint.UpdatedAt = at
			if to == SprintClosed {
				closed := at
				sprint.ClosedAt, sprint.ClosedBy = &closed, w.Actor
			}
			decision, err := w.decide(subject, OpPatch, scope,
				stepID(opID, "sprint"), sprint, nil, at)
			if err != nil {
				return statelog.Decision{}, err
			}
			decision.Version = int64(sprint.Version)
			return decision, nil
		},
	})
}

// setActiveSprint writes the project's pointer, and nothing else about it.
//
// `Notify == nil` and `UpdatedAt` untouched: a pointer move is not history.
func (w *Writer) setActiveSprint(ctx context.Context, opID, project string,
	number int, from, to SprintState, pointer *int, at time.Time) (WriteResult, error) {

	subject := ProjectSubject(project)
	scope := ScopeSet{Subject: true}
	return w.published(ctx, statelog.Request{
		Subject:  wire(subject),
		Scope:    scope.Resolve(subject),
		OpID:     opID,
		MintedAt: at,
		Pattern:  statelog.PatternArbitrated,
		Decide: func(tx *sql.Tx) (statelog.Decision, error) {
			current, held, err := readProject(ctx, tx, project)
			switch {
			case err != nil:
				return statelog.Decision{}, err
			case !held:
				return statelog.Decision{}, fmt.Errorf("tracker: project %s is "+
					"not on this node: %w", project, statelog.ErrUnavailable)
			}
			// THE SPRINT'S OWN STATE IS CHECKED HERE TOO, in this same
			// transaction, and it is not a duplicate of the record's
			// veto — it is what stops a REFUSED transition leaving the
			// pointer moved. The pointer is written first because that
			// is where two nodes racing to start sprint 7 are
			// arbitrated; without this clause, an attempt to restart a
			// closed sprint would pass the pointer's own check, move
			// it, and only then be refused by the record — leaving the
			// project claiming to run a sprint that is closed.
			sprint, held, err := readSprint(ctx, tx, project, number)
			switch {
			case err != nil:
				return statelog.Decision{}, err
			case !held:
				return statelog.Decision{}, fmt.Errorf("tracker: sprint %s.%d "+
					"is not on this node: %w", project, number,
					statelog.ErrUnavailable)
			case sprint.State != from && sprint.State != to:
				return statelog.Decision{}, fmt.Errorf("tracker: sprint %s.%d "+
					"is %s, and only a %s sprint becomes %s — a sprint's state "+
					"moves one way, whatever its project's pointer says",
					project, number, sprint.State, from, to)
			}
			if to == SprintActive && current.ActiveSprint != nil &&
				*current.ActiveSprint != number {
				return statelog.Decision{}, fmt.Errorf("tracker: project %s is "+
					"running sprint %d, and one project runs one sprint — close "+
					"it before starting %d", project, *current.ActiveSprint, number)
			}
			if to == SprintClosed && (current.ActiveSprint == nil ||
				*current.ActiveSprint != number) {
				// THE POINTER ALREADY MOVED, which is the interrupted
				// close this order leaves and the duty completes.
				return statelog.Decision{}, statelog.ErrExists
			}
			current.ActiveSprint = pointer
			decision, err := w.decide(subject, OpPatch, scope, opID, current, nil, at)
			if err != nil {
				return statelog.Decision{}, err
			}
			decision.Version = int64(current.Version)
			return decision, nil
		},
	})
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
	project string, patch TaskPatch, notify *Notify) (WriteResult, error) {

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
		w.count("crewlet.tracker.bulk.calls", metrics.Attrs{"result": "refused"})
		return WriteResult{}, err
	}
	defer release()
	w.count("crewlet.tracker.bulk.calls", metrics.Attrs{"result": "admitted"})

	result := WriteResult{Failed: map[string]string{}}
	for i, id := range subjects {
		one, err := w.UpdateTask(ctx, stepID(opID, fmt.Sprintf("b%d", i)),
			id, project, NoIfMatch, patch, notify)
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
		w.metrics.AddValue("crewlet.tracker.bulk.apply_seconds", projected, nil)
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
		_, _ = w.claims.Release(context.WithoutCancel(ctx), resource, w.nodeID, epoch)
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
