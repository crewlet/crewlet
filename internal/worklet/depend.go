package worklet

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
)

// SEQUENCE: a dependency. `waiting_on` is AUTHORED on the dependent (the fact);
// [Task.Dependents] on the blocker is its MIRROR, so a close can name whom it
// unblocks without a reverse scan.
//  1. every counterparty is read first: a tombstoned, absent or full
//     ([MaxDependents]) blocker refuses before an edge it can never mirror lands;
//  2. the authored commits, one per task whose `waiting_on` changes, each
//     routed to its own task's parties;
//  3. the mirrors, ONE commit per task whose `Dependents` changes, carrying the
//     [Snapshot.Dependents] that routes `blocking` to the blocker's assignee.
// A crash after 2 leaves an edge the applier flags `one_sided`; the duty writes
// its mirror `Late: true`, its assignee never told. Mirroring first would leave
// a dependent no scan can find, with no authored row to flag it by.

// DependencyChange is one call's dependency edits on ONE task. `WaitingOn`
// names the tasks this one waits for and `Blocking` the tasks that wait for it:
// separate gestures against separate counterparties.
type DependencyChange struct {
	// Task is the subject by id, and Project its home, which the scope needs
	// and the caller already holds, so no read here fetches it again.
	Task    string
	Project string

	WaitingOnAdd, WaitingOnRemove []string
	BlockingAdd, BlockingRemove   []string

	// Note rides every edge this call AUTHORS, and nothing it removes.
	Note string
}

// Empty reports a change that would write nothing.
func (d DependencyChange) Empty() bool {
	return len(d.WaitingOnAdd) == 0 && len(d.WaitingOnRemove) == 0 &&
		len(d.BlockingAdd) == 0 && len(d.BlockingRemove) == 0
}

// DependencyResult is what the sequence did, per counterparty.
type DependencyResult struct {
	// WriteResult answers for EVERY commit this call made
	// ([DependencyResult.fold]). Its Version is this call's OWN task's, zero
	// where nothing landed on that task's subject: never the last commit's,
	// which is usually another task's.
	WriteResult

	// Mirrored is every task whose mirror commit landed, and OneSided every
	// one whose did not: REPORTED, not raised, since the authored edge is
	// durable and the duty repairs the rest; a caller told "failed" would
	// re-issue it.
	Mirrored []string
	OneSided []string
}

// Depend writes a dependency change end to end.
func (w *Writer) Depend(ctx context.Context, opID string, change DependencyChange,
	leads Leads) (DependencyResult, error) {

	switch {
	case change.Task == "":
		return DependencyResult{}, invalid("a dependency change names no task")
	case change.Project == "":
		return DependencyResult{}, invalid("the dependency change on "+
			"task %s names no project — the caller resolved a key to reach "+
			"this task and therefore holds one", change.Task)
	case change.Empty():
		return DependencyResult{}, invalid("the dependency change on "+
			"task %s adds and removes nothing", change.Task)
	case len(change.Note) > MaxRelationNote:
		return DependencyResult{}, invalid("the note on task %s's "+
			"dependency change is %d bytes and the maximum is %d",
			change.Task, len(change.Note), MaxRelationNote)
	}

	found, err := w.readParties(ctx, change)
	if err != nil {
		return DependencyResult{}, err
	}
	at := w.Now()
	var out DependencyResult
	// authored is step 2's position on this task's own subject, which this
	// task's own mirror must see ([Writer.After]).
	var authored statelog.Position

	// STEP 2 — THE AUTHORED EDGES: this task's own `waiting_on` delta on its
	// subject, then each task made to wait on it, on that task's subject.
	if intent := edgesOn(change.WaitingOnAdd, change.WaitingOnRemove,
		w.Actor, change.Note, at); intent != nil {
		result, err := w.UpdateTask(ctx, stepID(opID, "waiting"), change.Task,
			change.Project, NoIfMatch, TaskPatch{Relate: intent},
			ChangeRelations, found.wakeFor(found.self, nil, leads))
		out.fold(result, true)
		// AN UNKNOWN AUTHORED EDGE STOPS THE CALL BEFORE ITS MIRRORS: a
		// mirror over an edge that may not exist lists a dependent no scan
		// can find.
		if err = resolved(fmt.Sprintf("task %s's own dependency edges",
			change.Task), result, err); err != nil {
			return out, err
		}
		authored = result.Position
	}
	for i, id := range append(slices.Clone(change.BlockingAdd), change.BlockingRemove...) {
		other, held := found.byID[id]
		if !held {
			continue
		}
		adding := i < len(change.BlockingAdd)
		intent := &RelationIntent{}
		edge := Relation{Kind: RelationWaitingOn, Other: change.Task}
		if adding {
			edge.Note, edge.CreatedBy, edge.CreatedAt = change.Note, w.Actor, at
			intent.Add = []Relation{edge}
		} else {
			intent.Remove = []Relation{edge}
		}
		// AN UNBLOCK TELLS NOBODY: the wake that matters, that the work is
		// startable, is the `unblocked` one a close carries.
		var notify *Notify
		if adding {
			notify = found.wakeFor(other, nil, leads)
		}
		// NAMED BY THE TASK IT WRITES, never by its place in the list —
		// see [authoredStep].
		result, err := w.UpdateTask(ctx, stepID(opID, authoredStep(adding, id)),
			id, other.Project, NoIfMatch, TaskPatch{Relate: intent},
			ChangeRelations, notify)
		// FOLDED BEFORE IT IS JUDGED, so a `blocking`-only change whose own
		// mirror is left one-sided still reports the edges it authored, and
		// one stopped here reports the step it stopped at.
		out.fold(result, false)
		if err = resolved(fmt.Sprintf("task %s's edge to %s", id,
			change.Task), result, err); err != nil {
			return out, fmt.Errorf("worklet: %d of this call's edges were "+
				"written before task %s's own stopped the call: %w", i, id, err)
		}
	}

	// STEP 3 — THE MIRRORS, best effort, with the authored position: a call
	// naming both directions writes THIS task's subject twice ([Writer.After]).
	out.Mirrored, out.OneSided = w.mirror(ctx, opID, change, found, leads,
		authored, &out)
	return out, nil
}

// fold folds one commit of this call into the answer for EVERY record; own
// says the commit is on the call's own task.
//
//   - the POSITION is the latest, because a caller barriers its next read on
//     it and any earlier one is a floor below a record this call made;
//   - the OUTCOME is the least certain ([statelog.LessCertain]), so an unknown
//     authored commit is never reported applied because a later mirror was,
//     and an unvouched unknown says so ([statelog.Result.Unvouched]);
//   - the VERSION is this task's OWN, from its latest commit there, or zero:
//     a caller passes it back as an If-Match, and a counterparty's would
//     refuse its next edit.
func (out *DependencyResult) fold(result WriteResult, own bool) {
	if !result.Wrote() {
		return
	}
	out.Outcome = statelog.LessCertain(out.Outcome, result.Outcome)
	out.Position = statelog.Later(out.Position, result.Position)
	if result.Outcome == statelog.OutcomeUnknown && result.Unvouched {
		out.Unvouched = true
	}
	if out.OpID == "" {
		out.OpID = result.OpID
	}
	out.Rounds += result.Rounds
	if own && result.Version > out.Version {
		out.Version = result.Version
	}
	out.Caveats = append(out.Caveats, result.Caveats...)
}

// authoredStep and mirrorStep name one append of a dependency change BY THE
// TASK IT WRITES, never by its place in the list.
//
// A retry answers each step from the ledger by its id ([statelog.Snap.Held]),
// and a caller's retry recomputes the change against what already landed, so
// its lists lose the landed steps and the rest move up: named by position, a
// step is answered with another task's ledger row and its own write is never
// made (TestADependencyRetryAnswersEachStepWithItsOwnWrite).
//
// An authored step's name carries the SIGN, since one call may add an edge on a
// task and remove another's; a mirror needs none, one commit carrying both.
func authoredStep(adding bool, task string) string {
	if adding {
		return "b+/" + task
	}
	return "b-/" + task
}

// mirrorStep names the mirror commit on one task — see [authoredStep].
func mirrorStep(task string) string { return "m/" + task }

// edgesOn is the `waiting_on` gesture for one task, or nil when this call does
// not touch that task's own edges.
func edgesOn(add, remove []string, actor, note string, at time.Time) *RelationIntent {

	if len(add) == 0 && len(remove) == 0 {
		return nil
	}
	intent := &RelationIntent{}
	for _, id := range add {
		intent.Add = append(intent.Add, Relation{
			Kind: RelationWaitingOn, Other: id, Note: note,
			CreatedBy: actor, CreatedAt: at,
		})
	}
	for _, id := range remove {
		intent.Remove = append(intent.Remove, Relation{
			Kind: RelationWaitingOn, Other: id,
		})
	}
	return intent
}

// mirror is step 3: ONE commit per task whose `Dependents` set this call
// changes. BEST EFFORT: the authored edge is durable after step 2, so a failed
// mirror is COLLECTED as one-sided, for the duty to repair, rather than raised.
// authored is step 2's position on this call's own task (zero when it authored
// none), the session mark for the ONE mirror on that subject.
func (w *Writer) mirror(ctx context.Context, opID string, change DependencyChange,
	found parties, leads Leads,
	authored statelog.Position, out *DependencyResult) (mirrored, oneSided []string) {

	type edit struct {
		add, remove []string
		announce    []TaskParty
	}
	edits := map[string]*edit{}
	order := []string{}
	touch := func(id string) *edit {
		e, held := edits[id]
		if !held {
			e = &edit{}
			edits[id] = e
			order = append(order, id)
		}
		return e
	}
	// A BLOCKER THIS TASK NOW WAITS ON GAINS THIS TASK as a dependent.
	for _, id := range change.WaitingOnAdd {
		e := touch(id)
		e.add = append(e.add, change.Task)
		e.announce = append(e.announce, found.partyOf(found.self))
	}
	for _, id := range change.WaitingOnRemove {
		touch(id).remove = append(touch(id).remove, change.Task)
	}
	// AND THIS TASK GAINS THE ONES IT NOW BLOCKS, in ONE commit on its
	// own subject.
	if len(change.BlockingAdd) > 0 || len(change.BlockingRemove) > 0 {
		e := touch(change.Task)
		for _, id := range change.BlockingAdd {
			other, held := found.byID[id]
			if !held {
				continue
			}
			e.add = append(e.add, id)
			e.announce = append(e.announce, found.partyOf(other))
		}
		e.remove = append(e.remove, change.BlockingRemove...)
	}

	for _, id := range order {
		subject, held := found.byID[id]
		if id == change.Task {
			subject, held = found.self, true
		}
		if !held {
			continue
		}
		e := edits[id]
		intent := &DependentIntent{Add: e.add, Remove: e.remove}
		// A REMOVAL-ONLY MIRROR IS QUIET, and nil is the only way to
		// say so.
		var notify *Notify
		if len(e.announce) > 0 {
			notify = found.wakeFor(subject, e.announce, leads)
		}
		// ONLY THIS CALL'S OWN TASK CARRIES THE MARK: every other mirror is
		// this gesture's FIRST record on its subject, with nothing to wait for.
		writer := w
		if id == change.Task {
			writer = w.After(authored)
		}
		result, err := writer.UpdateTask(ctx, stepID(opID, mirrorStep(id)),
			id, subject.Project, NoIfMatch, TaskPatch{Depend: intent},
			ChangeRelations, notify)
		// AN UNKNOWN MIRROR IS REPORTED ONE-SIDED: it may not be on the log,
		// and reported mirrored, a caller would believe a blocker knows of a
		// dependent it may never hear of.
		if resolved("a mirror", result, err) != nil {
			oneSided = append(oneSided, id)
			continue
		}
		out.fold(result, id == change.Task)
		mirrored = append(mirrored, id)
	}
	slices.Sort(mirrored)
	slices.Sort(oneSided)
	return mirrored, oneSided
}

// parties is the pre-flight read: every counterparty this call names, and the
// task itself, read in ONE transaction.
type parties struct {
	self Task
	byID map[string]Task
}

// partyOf is one task as the other end of a dependency sees it.
func (p parties) partyOf(t Task) TaskParty {
	return TaskParty{Task: t.ID, Key: t.Key, Assignee: t.Assignee}
}

// wakeFor builds a commit's notification from the task the commit is ABOUT.
//
// THROUGH [Wake] rather than a literal, so a dependency commit carries the same
// snapshot as every other, with every field the router reads filled.
func (p parties) wakeFor(subject Task, dependents []TaskParty, leads Leads) *Notify {
	return Wake{
		Kind: ChangeRelations, Before: subject, After: subject,
		Dependents: dependents,
	}.Notify(leads)
}

// readParties is step 1: ONE transaction over every counterparty this call
// names, plus the task itself.
//
// ONE READ RATHER THAN ONE PER EDGE: the checks are over a SET (any blocker
// full, tombstoned or absent), and a read per edge answers each at another
// instant.
func (w *Writer) readParties(ctx context.Context, change DependencyChange) (parties, error) {
	if w.db.IsZero() {
		return parties{}, fmt.Errorf("worklet: this writer has no replicated " +
			"estate, so it cannot read the counterparties a dependency has to " +
			"have checked before its first append")
	}
	wanted := make([]string, 0, len(change.WaitingOnAdd)+len(change.WaitingOnRemove)+
		len(change.BlockingAdd)+len(change.BlockingRemove))
	for _, group := range [][]string{
		change.WaitingOnAdd, change.WaitingOnRemove,
		change.BlockingAdd, change.BlockingRemove,
	} {
		for _, id := range group {
			switch {
			case id == "":
				return parties{}, invalid("the dependency change "+
					"on task %s names an empty counterparty", change.Task)
			case IsChangeRef(id) && slices.Contains(change.BlockingAdd, id):
				return parties{}, invalid("task %s cannot block %s: a change "+
					"waits on no task", change.Task, id)
			case IsChangeRef(id):
				// A CHANGE IS NO TASK: nothing to read, and no mirror.
				continue
			case id == change.Task:
				return parties{}, invalid("task %s cannot depend "+
					"on itself — a task that blocks itself is one no close "+
					"can ever clear", change.Task)
			}
			if !slices.Contains(wanted, id) {
				wanted = append(wanted, id)
			}
		}
	}
	if len(wanted) > MaxTasksNamed {
		return parties{}, invalid("this dependency change names %d "+
			"counterparties and one call may name %d — each is a commit of "+
			"its own, and a gesture larger than that is a batch",
			len(wanted), MaxTasksNamed)
	}
	required := append(slices.Clone(change.WaitingOnAdd), change.BlockingAdd...)
	out := parties{byID: make(map[string]Task, len(wanted))}
	err := w.db.Read(ctx, func(tx *sql.Tx) error {
		self, held, err := readTask(ctx, tx, change.Task)
		if err != nil {
			return err
		}
		if !held {
			return missingTask(ctx, tx, change.Task, "no dependency can be changed on it")
		}
		out.self = self
		for _, id := range wanted {
			current, held, err := readTask(ctx, tx, id)
			if err != nil {
				return err
			}
			adding := slices.Contains(required, id)
			switch {
			case !held && adding:
				// NO EDGE TO A TASK THIS NODE DOES NOT HOLD, refused with
				// [missingTask]'s two answers: a purged task is gone for
				// good, and one with no row and no marker may be a create
				// not yet applied here, which the caller may retry.
				return missingTask(ctx, tx, id, "no dependency can be drawn to it")
			case !held:
				// A REMOVE OF AN EDGE TO A VANISHED TASK is a correction:
				// skipped here, still applied on this task's own side.
				continue
			case current.Removed != nil && adding:
				return invalid("task %s was removed by %s at %s, "+
					"so nothing can be made to wait on it; restore it first",
					id, current.Removed.By,
					current.Removed.At.Format(time.RFC3339))
			case adding && len(current.Dependents) >= MaxDependents:
				return invalid("%d tasks already wait on task %s "+
					"and the maximum is %d — every one of them is an object a "+
					"close of that task has to name in its own scope",
					len(current.Dependents), id, MaxDependents)
			}
			out.byID[id] = current
		}
		return nil
	})
	if err != nil {
		return parties{}, err
	}
	return out, nil
}

// projectLead is the fallback, resolved only where a Leads exists.
func projectLead(leads Leads, project string) string {
	if leads == nil {
		return ""
	}
	return leads.ProjectLead(project)
}
