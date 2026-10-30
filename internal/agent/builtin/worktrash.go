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

// The trash, as an operator reaches it.
//
// # Why no seat holds these
//
// A removal hides a task from every list in the company, and a seat that could
// hide work it did not want to do would be marking its own homework in the one
// way nobody notices: the board simply has one fewer item on it. Every other
// way a seat can duck work leaves a trace a person reads — a status, a
// reassignment, a comment — and this one leaves an absence.
//
// A restore is its inverse and is an operator's for the same reason. Neither is
// a facet of `update_work_item`, because that verb refuses a removed task
// outright ("restore it first"), and a freeze anybody can lift with an ordinary
// field write is not a freeze.
//
// # And why they are not `purge`
//
// A removal is REVERSIBLE AT ANY AGE and destroys nothing — the rows stay, the
// history answers, and `list_work_items` with `removed: true` is the trash. A
// purge destroys every row on every data node and has no inverse; it is a CLI
// gesture with a typed confirmation and a required reason for that reason
// alone. Somebody reaching for "delete" should land here.

// TrashWriter is the write side of the trash, declared by the consumer.
//
// A RESTORE CAN BE ASKED TO BRING THE ITEM BACK ALONE, which is how a caller
// admitted only as the item's author restores it: the tracker refuses that
// with [worklet.ErrRestoresOthers] when the removal took other tasks with it,
// decided by the writer that reads what went with it rather than by a read
// here that could race it.
type TrashWriter interface {
	RemoveTask(ctx context.Context, opID, id, project string, subtree bool,
		notify *worklet.Notify) (worklet.WriteResult, error)
	RestoreTask(ctx context.Context, opID, id, project string, alone bool,
		notify *worklet.Notify) (worklet.WriteResult, error)
}

// trashObject is a task as the trash's verbs are decided on it
// ([authz.ClassTidy]): its project, whose lead tidies it, and — with author —
// its reporter, who tidies their own item. The reporter is set once, at the
// create, so it is who filed the item and nobody a later edit named.
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
	return t.deps.operationParam(map[string]any{
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
		return notInATurn(worklet.RemoveWorkItemTool), nil
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
	// THE ROW DECIDES — the project it is filed under and who filed it,
	// read off the stored task — see the `inTool` entries in
	// authority.go for why the gate could not ask.
	if denied := t.deps.mayWrite(ctx, authz.ActionWorkRemove,
		trashObject(before.Task, true)); denied != nil {
		return *denied, nil
	}
	subtree := argBool(args, "subtree")
	if subtree {
		// AN AUTHOR TIDIES THEIR OWN ITEM AND NOTHING BELOW IT. The items
		// under it may be anybody's, so taking them out with it is the
		// project lead's or an admin's — asked again without the author
		// arm, which is what that sentence means as a decision.
		whole := t.deps.decide(ctx, authz.ActionWorkRemove, trashObject(before.Task, false))
		switch {
		case whole == nil:
		case errors.Is(whole, ErrRefused):
			refusal := authorityRefusal(worklet.RemoveWorkItemTool, whole)
			refusal.Output += " " + authorAloneNote("remove", "Leave `subtree` "+
				"out to put this item in the trash on its own.")
			return refusal, nil
		default:
			// THIS NODE COULD NOT TELL whether the caller leads the
			// project: the table's own answer, with no note telling an
			// author to drop a subtree a lead may well be entitled to.
			return authorityRefusal(worklet.RemoveWorkItemTool, whole), nil
		}
	}

	after := before.Task
	// THE WAKE'S OWN SNAPSHOT SHOWS THE TASK AS REMOVED, which is what the
	// `removed` change kind is about — a wake whose After still looked live
	// would describe the state the removal left behind.
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
	return t.deps.operationParam(map[string]any{
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
		return notInATurn(worklet.RestoreWorkItemTool), nil
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
	// THE REMOVED HALF IS WHAT THIS READS. A restore is about a task that
	// is by definition out of every ordinary list, so the detail read is
	// the only way to reach it — and it answers for a removed task, which
	// is what makes the trash readable from both ends.
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
	// WHETHER THIS CALLER MAY BRING BACK WHAT WENT WITH IT. A restore brings
	// back exactly what its removal took, and an author restores their own
	// item and nothing below it — so a caller the table admits only as the
	// item's author restores it ALONE, and the tracker refuses that when
	// the gesture that put it in the trash took others with it.
	whole := t.deps.decide(ctx, authz.ActionWorkRestore, trashObject(before.Task, false))
	if whole != nil && !errors.Is(whole, ErrRefused) {
		// THIS NODE COULD NOT TELL whether the caller leads the project,
		// and guessing "alone" would refuse a lead whatever the trash
		// holds: the honest answer is the one the table gave.
		return authorityRefusal(worklet.RestoreWorkItemTool, whole), nil
	}
	alone := whole != nil
	// A LIVE ITEM IS NOT REFUSED HERE, because it is exactly what a restore
	// that stopped part of the way through leaves: the root is back and
	// what went with it is not, and this call is how that is finished. The
	// tracker says when there is truly nothing to bring back.

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
		// FORBIDDEN, on the table's own refusal of the whole gesture: the
		// item is the caller's to restore and what went with it is not.
		refusal := authorityRefusal(worklet.RestoreWorkItemTool, whole)
		refusal.Output += " " + authorDetail(err) + " " + authorAloneNote("restore",
			"Ask the project's lead or an admin to restore it.")
		return refusal, nil
	case errors.Is(err, worklet.ErrNothingToRestore):
		// CONFLICT: the item's state, not the argument, is what refuses
		// this — somebody restored it, and what went with it, between the
		// caller's read and now.
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

// subtreeStopped answers a subtree removal or restore whose ROOT landed and
// whose walk over the rest did not finish.
//
// # Why it is not a failure, and not a success either
//
// The item the caller named IS in the trash, or out of it — so "the change was
// NOT made", which is what this answered, was false about the one thing the
// caller asked about by name, and a retry was refused because the restored
// root was no longer in the trash. And the gesture is NOT done: the tasks that
// go with it are split across the trash. So the answer is the root's receipt
// with the walk's own count beside it and what finishes it, the shape
// `dependencies_failed` gives a create whose item landed.
//
// EITHER VERB FINISHES WHEN MADE AGAIN, under the same operation or a new one
// — a task already where the gesture leaves it is nothing to do. The one stop
// that differs is a step this node's ledger cannot vouch for: the same
// operation stops at the same task here every time, and only a new one decides
// it afresh — which the operator's surface, the only one these two tools are
// served on, gets by leaving the op_id out.
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
