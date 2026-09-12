package tracker

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
)

// SEQUENCE: a dependency, which is the one relation with two ends.
//
// # The two ends are not symmetric
//
// `waiting_on` is AUTHORED on the dependent; [Task.Dependents] on the blocker
// is the MIRROR, kept so a close can name who it unblocks without a reverse
// scan over every task in the company. That asymmetry decides everything else
// here: the authored edge is the fact, the mirror is a copy of it, and a scan
// that wants to find a half-written dependency looks for an authored edge
// whose mirror is missing.
//
// # The order, the residue, and the repair
//
//  1. EVERY COUNTERPARTY IS READ FIRST, before anything is published. A
//     blocker that is tombstoned, absent, or already at [MaxDependents]
//     refuses the edge — and refusing it after the authored commit had landed
//     would leave an edge whose mirror can never be written.
//  2. THE AUTHORED COMMITS, one per task whose `waiting_on` set this call
//     changes. Each routes to ITS OWN task's parties, which is why none of
//     them is the blocker's notice.
//  3. THE MIRRORS, one per task whose `Dependents` set this call changes —
//     ONE commit per counterparty however many edges of this call name it.
//     The mirror carries [Snapshot.Dependents], and that is what routes
//     `blocking` to the blocker's assignee.
//
// THE CRASH RESIDUE IS A ONE-SIDED EDGE: step 2 landed and a step-3 commit did
// not, so a task waits on a blocker that does not know it. That is a NAMED
// state rather than a silent one — the applier flags such an edge `one_sided`
// — and the `tracker` duty writes the missing commit, carrying the same
// [Snapshot.Dependents] and `Late: true`, because the blocker's assignee was
// never told at all.
//
// The other order was considered and is worse: mirroring first leaves a
// blocker listing a dependent whose own edge does not exist, which no scan can
// find — there is no authored `waiting_on` row to flag it by.

// DependencyChange is one call's worth of dependency edits on ONE task.
//
// THE TWO DIRECTIONS ARE SEPARATE FIELDS because they are separate gestures
// against separate counterparties: `WaitingOn` names the tasks THIS one waits
// for, and `Blocking` the tasks that wait for it.
type DependencyChange struct {
	// Task is the subject this call is about, by id, and Project its
	// home — which the scope needs and no read here should be asked for
	// twice.
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
	WriteResult

	// Mirrored is every task whose mirror commit landed, and OneSided
	// every one whose did not.
	//
	// REPORTED RATHER THAN RAISED: the authored edge is durable either
	// way, and the duty repairs the rest. A caller told "failed" about a
	// dependency that exists would re-issue it.
	Mirrored []string
	OneSided []string
}

// Depend writes a dependency change end to end.
func (w *Writer) Depend(ctx context.Context, opID string, change DependencyChange,
	leads Leads) (DependencyResult, error) {

	switch {
	case change.Task == "":
		return DependencyResult{}, fmt.Errorf("tracker: a dependency change names no task")
	case change.Project == "":
		return DependencyResult{}, fmt.Errorf("tracker: the dependency change on "+
			"task %s names no project — the caller resolved a key to reach "+
			"this task and therefore holds one", change.Task)
	case change.Empty():
		return DependencyResult{}, fmt.Errorf("tracker: the dependency change on "+
			"task %s adds and removes nothing", change.Task)
	case len(change.Note) > MaxRelationNote:
		return DependencyResult{}, fmt.Errorf("tracker: the note on task %s's "+
			"dependency change is %d bytes and the maximum is %d",
			change.Task, len(change.Note), MaxRelationNote)
	}

	found, err := w.readParties(ctx, change)
	if err != nil {
		return DependencyResult{}, err
	}
	at := w.Now()
	var out DependencyResult

	// STEP 2 — THE AUTHORED EDGES.
	//
	// This task's own `waiting_on` delta is one commit on its subject;
	// each task this call makes WAIT on it carries its own `waiting_on`
	// edge, so those are commits on their subjects rather than on this.
	if intent := edgesOn(change.WaitingOnAdd, change.WaitingOnRemove,
		w.Actor, change.Note, at); intent != nil {
		result, err := w.UpdateTask(ctx, stepID(opID, "waiting"), change.Task,
			change.Project, NoIfMatch, TaskPatch{Relate: intent},
			ChangeRelations, found.wakeFor(found.self, nil, leads))
		if err != nil {
			return out, err
		}
		out.WriteResult = result
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
		// AN UNBLOCK TELLS NOBODY. "You no longer wait on X" is not an
		// event anybody acts on, and the wake that matters — that the
		// work is now startable — is the `unblocked` one a close
		// carries.
		var notify *Notify
		if adding {
			notify = found.wakeFor(other, nil, leads)
		}
		if _, err := w.UpdateTask(ctx, stepID(opID, fmt.Sprintf("b%d", i)),
			id, other.Project, NoIfMatch, TaskPatch{Relate: intent},
			ChangeRelations, notify); err != nil {
			return out, fmt.Errorf("tracker: %d of this call's edges were "+
				"written before task %s refused its own: %w", i, id, err)
		}
	}

	// STEP 3 — THE MIRRORS, best effort.
	//
	// THE AUTHORED COMMIT'S POSITION TRAVELS WITH THEM, because a call
	// naming both directions writes THIS task's subject twice: once above
	// for its own `waiting_on` edges, and once here for the dependents it
	// gains. See [Writer.After] for what the second one would otherwise
	// spend discovering that the first had moved it.
	var last WriteResult
	out.Mirrored, out.OneSided, last = w.mirror(ctx, opID, change, found, leads,
		out.Position)
	// THE POSITION IS THE LAST COMMIT THIS CALL MADE, whatever its shape.
	// A `blocking`-only change writes nothing on its own subject, so the
	// authored branch above never ran — and a caller that settled at the
	// zero position would barrier at nothing and read its own write back
	// missing.
	if last.Position.Seq > out.Position.Seq {
		out.WriteResult = last
	}
	return out, nil
}

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

// mirror is step 3: every task whose `Dependents` set this call changes, ONE
// commit each.
//
// BEST EFFORT, by design: the authored edge is durable after step 2, and a
// mirror that lost its race is a `one_sided` edge the duty repairs. So a
// failure here is COLLECTED rather than raised — a caller told "failed" about
// a dependency that exists would write it a second time.
// authored is the position of step 2's commit on this call's own task, or the
// zero position when this call authored no `waiting_on` edge of its own. It is
// the session mark for the ONE mirror that lands on that same subject.
func (w *Writer) mirror(ctx context.Context, opID string, change DependencyChange,
	found parties, leads Leads,
	authored statelog.Position) (mirrored, oneSided []string, last WriteResult) {

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

	for i, id := range order {
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
		// ONLY THIS CALL'S OWN TASK CARRIES THE MARK. Every other
		// mirror is the FIRST record this gesture puts on its subject,
		// so a wait there would be a wait for a position that says
		// nothing about it.
		writer := w
		if id == change.Task {
			writer = w.After(authored)
		}
		result, err := writer.UpdateTask(ctx, stepID(opID, fmt.Sprintf("m%d", i)),
			id, subject.Project, NoIfMatch, TaskPatch{Depend: intent},
			ChangeRelations, notify)
		if err != nil {
			oneSided = append(oneSided, id)
			continue
		}
		if result.Position.Seq > last.Position.Seq {
			last = result
		}
		mirrored = append(mirrored, id)
	}
	slices.Sort(mirrored)
	slices.Sort(oneSided)
	return mirrored, oneSided, last
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
// THROUGH [Wake] rather than a literal, so the snapshot a dependency commit
// carries is the same snapshot every other commit carries — a second builder
// here is how one of them stops filling a field the router reads.
func (p parties) wakeFor(subject Task, dependents []TaskParty, leads Leads) *Notify {
	return Wake{
		Kind: ChangeRelations, Before: subject, After: subject,
		Dependents: dependents,
	}.Notify(leads)
}

// readParties is step 1: ONE transaction over every counterparty this call
// names, plus the task itself.
//
// ONE READ RATHER THAN ONE PER EDGE, because the checks are about a SET — is
// any blocker full, tombstoned or absent — and a read per edge would answer
// each question against a different instant.
func (w *Writer) readParties(ctx context.Context, change DependencyChange) (parties, error) {
	if w.db == nil {
		return parties{}, fmt.Errorf("tracker: this writer has no replicated " +
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
				return parties{}, fmt.Errorf("tracker: the dependency change "+
					"on task %s names an empty counterparty", change.Task)
			case id == change.Task:
				return parties{}, fmt.Errorf("tracker: task %s cannot depend "+
					"on itself — a task that blocks itself is one no close "+
					"can ever clear", change.Task)
			}
			if !slices.Contains(wanted, id) {
				wanted = append(wanted, id)
			}
		}
	}
	if len(wanted) > MaxBulkTasks {
		return parties{}, fmt.Errorf("tracker: this dependency change names %d "+
			"counterparties and one call may name %d — each is a commit of "+
			"its own, and a gesture larger than that is a batch",
			len(wanted), MaxBulkTasks)
	}
	required := append(slices.Clone(change.WaitingOnAdd), change.BlockingAdd...)
	out := parties{byID: make(map[string]Task, len(wanted))}
	err := w.db.Replicated().Read(ctx, func(tx *sql.Tx) error {
		self, held, err := readTask(ctx, tx, change.Task)
		if err != nil {
			return err
		}
		if !held {
			return fmt.Errorf("tracker: task %s is not on this node: %w",
				change.Task, statelog.ErrUnavailable)
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
				return fmt.Errorf("tracker: there is no task %s, so the "+
					"dependency naming it cannot be written — an edge to a "+
					"task that does not exist resolves to nothing on every "+
					"node, for ever", id)
			case !held:
				// A REMOVE OF AN EDGE TO A VANISHED TASK IS THE
				// CORRECTION SOMEBODY IS MAKING, so it is skipped
				// here and still applied on this task's own side.
				continue
			case current.Removed != nil && adding:
				return fmt.Errorf("tracker: task %s was removed by %s at %s, "+
					"so nothing can be made to wait on it; restore it first",
					id, current.Removed.By,
					current.Removed.At.Format(time.RFC3339))
			case adding && len(current.Dependents) >= MaxDependents:
				return fmt.Errorf("tracker: %d tasks already wait on task %s "+
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
