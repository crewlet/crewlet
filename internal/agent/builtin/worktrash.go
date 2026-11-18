package builtin

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/caveat"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/worklet"
)

// The trash, as an operator reaches it. No seat holds these: a seat that could
// hide work it did not want would duck it leaving only an absence, where every
// other way (a status, a reassignment, a comment) leaves a trace a person reads.
// A restore is its inverse, an operator's for the same reason. Neither is a
// facet of `update_work_item`, which refuses a removed task ("restore it
// first"): a freeze an ordinary field write lifts is not a freeze.
//
// Nor are they `purge`: a removal is reversible at any age and destroys nothing
// (`list_work_items` with `removed: true` is the trash), while a purge destroys
// every row on every data node, so it alone is a harness gesture with a typed
// confirmation and a required reason. Somebody reaching for "delete" lands here.

// TrashWriter is the write side of the trash, declared by the consumer.
// RestoreTask's alone is how a caller admitted only as the item's author
// restores it: the tracker refuses it with [worklet.ErrRestoresOthers] when the
// removal took other tasks, decided by the writer rather than a racing read here.
type TrashWriter interface {
	RemoveTask(ctx context.Context, opID, id, project string, subtree bool,
		notify *worklet.Notify) (worklet.WriteResult, error)
	RestoreTask(ctx context.Context, opID, id, project string, alone bool,
		notify *worklet.Notify) (worklet.WriteResult, error)
}

// trashObject is a task as the trash's verbs are decided on it
// ([authz.ClassTidy]): its project, whose lead tidies it, and, with author, its
// reporter, set once at the create, so who filed it and nobody an edit named.
func trashObject(task worklet.Task, author bool) authz.Object {
	o := authz.Object{Kind: authz.KindTask, Container: task.Project}
	if author {
		o.Author = task.Reporter
	}
	return o
}

// authorAloneNote is the sentence a refusal of an author's subtree gesture
// ends with: what they may do, and what is somebody else's.
func authorAloneNote(verb, next string) string {
	return "Whoever filed an item may put it in the trash and take it out " +
		"again; to " + verb + " the items that go with it as well is its " +
		"project lead's or an admin's. " + next
}

// authorDetail is the tracker's own account of what went with the item, for
// a refusal that has to say why the restore was more than the caller's.
func authorDetail(err error) string { return worklet.Sentence(err) + "." }

// ---- remove_work_item --------------------------------------------------- //

type removeWorkItem struct {
	itemTarget
	deps WorkDeps
}

var _ tools.SeatCallable = (*removeWorkItem)(nil)

func (t *removeWorkItem) Name() string { return worklet.RemoveWorkItemTool }

func (t *removeWorkItem) Description() string {
	return "Put a work item in the trash. It disappears from every list and " +
		"board, its history is untouched, and it can be restored at any age " +
		"with " + worklet.RestoreWorkItemTool + " — there is no window. Nothing is destroyed: " +
		"that is `crewlet worklet purge`, which has no inverse. Pass " +
		"`subtree: true` to take its children with it; left behind, each " +
		"stays on every list and board it is on, as the root of what is " +
		"left, until this item comes back."
}

func (t *removeWorkItem) Parameters() map[string]any {
	return t.deps.ops.param(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"item": map[string]any{
				"type":        "string",
				"description": "The item to put in the trash. " + itemRef,
			},
			"subtree": map[string]any{
				"type": "boolean",
				"description": "Remove its descendants with it. Each one's " +
					"tombstone names this item, so restoring it brings back " +
					"exactly what this removal took and nothing that was " +
					"already in the trash.",
			},
		},
		"required": []any{"item"},
	})
}

func (t *removeWorkItem) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

func (t *removeWorkItem) CallForTurn(ctx context.Context, turn *turnctx.Turn,
	args map[string]any) (tools.Result, error) {

	actor, err := t.deps.actor(ctx, turn)
	if err != nil {
		//nolint:nilerr // A tool failure is a RESULT the caller reads.
		return unattributed(worklet.RemoveWorkItemTool, err), nil
	}
	if t.deps.TrashWriter == nil || t.deps.Reader == nil {
		return unconfigured(worklet.RemoveWorkItemTool), nil
	}
	actor, denied := t.deps.bindOperation(actor, worklet.RemoveWorkItemTool, args)
	if denied != "" {
		return failed(denied), nil
	}
	ref := strings.TrimSpace(argString(args, "item"))
	if ref == "" {
		return failed(worklet.RemoveWorkItemTool + " needs an `item` — a key like ENG-42, or an id."), nil
	}
	before, err := t.deps.Reader.Task(ctx, ref, worklet.DetailWants{}, seatRead)
	switch {
	case errors.Is(err, worklet.ErrNoTask):
		return refusedBy(tools.RefusalNotFound, err, fmt.Sprintf("There is no work item %q.",
			clip(ref))), nil
	case err != nil:
		return readFailure(ctx, worklet.RemoveWorkItemTool, err), nil
	}
	// The stored task's project and reporter decide; the `inTool` entries in
	// authority.go say why the gate could not ask.
	if denied := t.deps.mayWrite(ctx, authz.ActionWorkRemove,
		trashObject(before.Task, true)); denied != nil {
		return *denied, nil
	}
	subtree := argBool(args, "subtree")
	if subtree {
		// An author tidies their own item and nothing below it, which may be
		// anybody's: a subtree is the lead's or an admin's, asked again
		// without the author arm.
		whole := t.deps.decide(ctx, authz.ActionWorkRemove, trashObject(before.Task, false))
		switch {
		case whole == nil:
		case errors.Is(whole, ErrRefused):
			refusal := authorityRefusal(worklet.RemoveWorkItemTool, whole)
			refusal.Output += " " + authorAloneNote("remove", "Leave `subtree` "+
				"out to put this item in the trash on its own.")
			return refusal, nil
		default:
			// This node could not tell whether the caller leads the project:
			// the table's answer, with no note telling an author to drop a
			// subtree a lead may be entitled to.
			return authorityRefusal(worklet.RemoveWorkItemTool, whole), nil
		}
	}

	after := before.Task
	// The wake's After shows the task removed, the state the `removed` change
	// kind describes.
	after.Removed = &worklet.Tombstone{
		By: actor.Handle, Kind: actor.Kind, At: t.deps.now(),
	}
	opID := opIDFor(actor, t.Name(), "remove", before.Task.ID, args)
	got, err := t.deps.TrashWriter(actor).RemoveTask(ctx, opID, before.Task.ID,
		before.Task.Project, subtree,
		worklet.Wake{
			Kind: worklet.ChangeRemoved, Before: before.Task, After: after,
		}.Notify(t.deps.Leads))
	var stopped *worklet.SubtreeStopped
	switch {
	case errors.As(err, &stopped):
		return t.deps.subtreeStopped(ctx, actor, worklet.RemoveWorkItemTool,
			before, "removed", got, stopped)
	case err != nil:
		return writeFailure(ctx, actor, worklet.RemoveWorkItemTool, err), nil
	case got.Outcome == statelog.OutcomeUnknown:
		// See [mergeWorkItem]: the seam's contract, held here.
		return unknownWrite(actor, worklet.RemoveWorkItemTool,
			fmt.Sprintf("%s went to the trash", before.Named()), opID,
			got.Unvouched, unknownNext(got.Unvouched,
				sameCall(actor, worklet.RemoveWorkItemTool),
				fmt.Sprintf("Read %s with "+GetWorkItemTool+" — it says whether it "+
					"is in the trash", before.Address()),
				"it changes nothing, since the item is already there")), nil
	}
	t.deps.settle(ctx, got.Position)
	return jsonResult(withOperation(ReceiptOf(map[string]any{
		"removed": true,
		"outcome": string(got.Outcome), "position": positionOf(got.Position), "version": got.Version,
	}, before), actor))
}

// ---- restore_work_item -------------------------------------------------- //

type restoreWorkItem struct {
	itemTarget
	deps WorkDeps
}

var _ tools.SeatCallable = (*restoreWorkItem)(nil)

func (t *restoreWorkItem) Name() string { return worklet.RestoreWorkItemTool }

func (t *restoreWorkItem) Description() string {
	return "Take a work item out of the trash, at any age — one commit, no " +
		"window and no rebuild. Anything removed by the same gesture comes " +
		"back with it; anything that was already in the trash for its own " +
		"reasons stays there. List the trash with " + ListWorkItemsTool + " and " +
		"`removed: true`."
}

func (t *restoreWorkItem) Parameters() map[string]any {
	return t.deps.ops.param(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"item": map[string]any{
				"type":        "string",
				"description": "The item to take out of the trash. " + itemRef,
			},
		},
		"required": []any{"item"},
	})
}

func (t *restoreWorkItem) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

func (t *restoreWorkItem) CallForTurn(ctx context.Context, turn *turnctx.Turn,
	args map[string]any) (tools.Result, error) {

	actor, err := t.deps.actor(ctx, turn)
	if err != nil {
		//nolint:nilerr // A tool failure is a RESULT the caller reads.
		return unattributed(worklet.RestoreWorkItemTool, err), nil
	}
	if t.deps.TrashWriter == nil || t.deps.Reader == nil {
		return unconfigured(worklet.RestoreWorkItemTool), nil
	}
	actor, denied := t.deps.bindOperation(actor, worklet.RestoreWorkItemTool, args)
	if denied != "" {
		return failed(denied), nil
	}
	ref := strings.TrimSpace(argString(args, "item"))
	if ref == "" {
		return failed(worklet.RestoreWorkItemTool + " needs an `item` — a key like ENG-42, or an id."), nil
	}
	// The detail read is the only one that answers for a removed task, which
	// is out of every list.
	before, err := t.deps.Reader.Task(ctx, ref, worklet.DetailWants{}, seatRead)
	switch {
	case errors.Is(err, worklet.ErrNoTask):
		return refusedBy(tools.RefusalNotFound, err, fmt.Sprintf("There is no work item %q.",
			clip(ref))), nil
	case err != nil:
		return readFailure(ctx, worklet.RestoreWorkItemTool, err), nil
	}
	if denied := t.deps.mayWrite(ctx, authz.ActionWorkRestore,
		trashObject(before.Task, true)); denied != nil {
		return *denied, nil
	}
	// A restore brings back exactly what its removal took, and an author
	// restores their own item and nothing below it: a caller admitted only as
	// the author restores it alone, which the tracker refuses when the removal
	// took others with it.
	whole := t.deps.decide(ctx, authz.ActionWorkRestore, trashObject(before.Task, false))
	if whole != nil && !errors.Is(whole, ErrRefused) {
		// This node could not tell whether the caller leads the project, and
		// guessing "alone" would refuse a lead: give the table's answer.
		return authorityRefusal(worklet.RestoreWorkItemTool, whole), nil
	}
	alone := whole != nil
	// A live item is not refused: a restore stopped part way leaves the root
	// back and the rest not, and this call finishes it. The tracker says when
	// nothing is left to bring back.

	after := before.Task
	after.Removed = nil
	opID := opIDFor(actor, t.Name(), "restore", before.Task.ID, args)
	got, err := t.deps.TrashWriter(actor).RestoreTask(ctx, opID, before.Task.ID,
		before.Task.Project, alone,
		worklet.Wake{
			Kind: worklet.ChangeRestored, Before: before.Task, After: after,
		}.Notify(t.deps.Leads))
	var stopped *worklet.SubtreeStopped
	switch {
	case errors.Is(err, worklet.ErrRestoresOthers):
		// Forbidden, on the table's refusal of the whole gesture: the item is
		// the caller's to restore and what went with it is not.
		refusal := authorityRefusal(worklet.RestoreWorkItemTool, whole)
		refusal.Output += " " + authorDetail(err) + " " + authorAloneNote("restore",
			"Ask the project's lead or an admin to restore it.")
		return refusal, nil
	case errors.Is(err, worklet.ErrNothingToRestore):
		// Conflict: the item's state refuses this; somebody restored it and
		// what went with it since the caller's read.
		return refusedBy(tools.RefusalConflict, err, fmt.Sprintf("%s is not in "+
			"the trash, and nothing is in the trash with it, so there is "+
			"nothing to restore.", before.Named())), nil
	case errors.As(err, &stopped):
		return t.deps.subtreeStopped(ctx, actor, worklet.RestoreWorkItemTool,
			before, "restored", got, stopped)
	case err != nil:
		return writeFailure(ctx, actor, worklet.RestoreWorkItemTool, err), nil
	case got.Outcome == statelog.OutcomeUnknown:
		return unknownWrite(actor, worklet.RestoreWorkItemTool,
			fmt.Sprintf("%s came out of the trash", before.Named()), opID,
			got.Unvouched, unknownNext(got.Unvouched,
				sameCall(actor, worklet.RestoreWorkItemTool),
				fmt.Sprintf("Read %s with "+GetWorkItemTool+" — it says whether it "+
					"is still in the trash", before.Address()),
				"it is refused, since there is nothing left to restore")), nil
	}
	t.deps.settle(ctx, got.Position)
	return jsonResult(withOperation(ReceiptOf(map[string]any{
		"restored": true,
		"outcome":  string(got.Outcome), "position": positionOf(got.Position), "version": got.Version,
	}, before), actor))
}

// subtreeStopped answers a subtree removal or restore whose root landed and
// whose walk over the rest did not finish. Not a failure: the named item did
// move, so "not made" would be false. Not a success: the tasks with it are
// split across the trash. So it is the root's receipt with the walk's count and
// what finishes it, as `dependencies_failed` answers a create whose item landed.
//
// Either verb finishes when made again, under the same operation or a new one,
// except at a step this node's ledger cannot vouch for: the same operation
// stops there every time, so only a new one (op_id left out, which the
// operator's surface these tools are served on allows) decides it afresh.
func (d WorkDeps) subtreeStopped(ctx context.Context, actor Actor, tool string,
	item worklet.TaskDetail, done string, got worklet.WriteResult,
	stopped *worklet.SubtreeStopped) (tools.Result, error) {

	next := fmt.Sprintf("Call %s on %s again, with the same arguments, to "+
		"finish: whatever has not followed yet goes, and what already has is "+
		"left where it is.", tool, item.Address())
	if errors.Is(stopped.Err, worklet.ErrStepUnvouched) {
		next = fmt.Sprintf("This node cannot vouch for the task it stopped at "+
			"under this operation, so call %s on %s again WITHOUT an op_id: a "+
			"new operation decides afresh and finishes it.", tool, item.Address())
	}
	d.settle(ctx, got.Position)
	answer := ReceiptOf(map[string]any{
		done:      true,
		"outcome": string(got.Outcome), "position": positionOf(got.Position),
		"version":          got.Version,
		"subtree_followed": stopped.Followed, "subtree_total": stopped.Of,
	}, item)
	said := partial(answer, SubtreeStoppedKey, caveat.SubtreeUnfinished(
		done == "restored", stopped.Followed, stopped.Of, fmt.Sprintf("%s is %s, "+
			"but only %d of the %d tasks that go with it followed before the walk "+
			"stopped (%s). Do not report it as done. %s", item.Named(), done,
			stopped.Followed, stopped.Of, writeTold(ctx, actor, tool, stopped.Err), next)))
	return caveated(withOperation(answer, actor), nil, said)
}
