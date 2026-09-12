package builtin

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/tracker"
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
// purge destroys every row on every node and has no inverse; it is a CLI
// gesture with a typed confirmation and a required reason for that reason
// alone. Somebody reaching for "delete" should land here.

// TrashWriter is the write side of the trash, declared by the consumer.
type TrashWriter interface {
	RemoveTask(ctx context.Context, opID, id, project string, subtree bool,
		notify *tracker.Notify) (tracker.WriteResult, error)
	RestoreTask(ctx context.Context, opID, id, project string,
		notify *tracker.Notify) (tracker.WriteResult, error)
}

// ---- remove_work_item --------------------------------------------------- //

type removeWorkItem struct{ deps WorkDeps }

var _ tools.SeatCallable = (*removeWorkItem)(nil)

func (t *removeWorkItem) Name() string { return tracker.RemoveWorkItemTool }

func (t *removeWorkItem) Description() string {
	return "Put a work item in the trash. It disappears from every list and " +
		"board, its history is untouched, and it can be restored at any age " +
		"with restore_work_item — there is no window. Nothing is destroyed: " +
		"that is `crewlet work purge`, which has no inverse. Pass " +
		"`subtree: true` to take its children with it; leaving them behind " +
		"makes them orphans somebody has to find."
}

func (t *removeWorkItem) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"item": map[string]any{
				"type":        "string",
				"description": "The item key (ENG-42) or its id.",
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
	}
}

func (t *removeWorkItem) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

func (t *removeWorkItem) CallForTurn(ctx context.Context, turn *turnctx.Turn,
	args map[string]any) (tools.Result, error) {

	actor, err := t.deps.actor(ctx, turn)
	if err != nil {
		//nolint:nilerr // A tool failure is a RESULT the caller reads.
		return notInATurn(tracker.RemoveWorkItemTool), nil
	}
	if t.deps.TrashWriter == nil || t.deps.Reader == nil {
		return unconfigured(tracker.RemoveWorkItemTool), nil
	}
	ref := strings.TrimSpace(argString(args, "item"))
	if ref == "" {
		return failed("remove_work_item needs an `item` — a key like ENG-42, or an id."), nil
	}
	before, err := t.deps.Reader.Task(ctx, ref, tracker.DetailWants{}, seatReadLevel)
	switch {
	case errors.Is(err, tracker.ErrNoTask):
		return failed(fmt.Sprintf("There is no work item %q.", clip(ref))), nil
	case err != nil:
		return failed(readFailure(tracker.RemoveWorkItemTool, err)), nil
	}

	after := before.Task
	// THE WAKE'S OWN SNAPSHOT SHOWS THE TASK AS REMOVED, which is what the
	// `removed` change kind is about — a wake whose After still looked live
	// would describe the state the removal left behind.
	after.Removed = &tracker.Tombstone{
		By: actor.Handle, Kind: actor.Kind, At: t.deps.now(),
	}
	got, err := t.deps.TrashWriter(actor).RemoveTask(ctx,
		opIDFor(actor, "remove", before.Task.ID), before.Task.ID,
		before.Task.Project, argBool(args, "subtree"),
		tracker.Wake{
			Kind: tracker.ChangeRemoved, Before: before.Task, After: after,
		}.Notify(t.deps.Leads))
	if err != nil {
		return failed(writeFailure(tracker.RemoveWorkItemTool, err)), nil
	}
	t.deps.settle(ctx, got.Position)
	return jsonResult(map[string]any{
		"key": before.Task.Key, "removed": true,
		"outcome": string(got.Outcome), "version": got.Version,
	})
}

// ---- restore_work_item -------------------------------------------------- //

type restoreWorkItem struct{ deps WorkDeps }

var _ tools.SeatCallable = (*restoreWorkItem)(nil)

func (t *restoreWorkItem) Name() string { return tracker.RestoreWorkItemTool }

func (t *restoreWorkItem) Description() string {
	return "Take a work item out of the trash, at any age — one commit, no " +
		"window and no rebuild. Anything removed by the same gesture comes " +
		"back with it; anything that was already in the trash for its own " +
		"reasons stays there. List the trash with list_work_items and " +
		"`removed: true`."
}

func (t *restoreWorkItem) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"item": map[string]any{
				"type":        "string",
				"description": "The item key (ENG-42) or its id.",
			},
		},
		"required": []any{"item"},
	}
}

func (t *restoreWorkItem) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

func (t *restoreWorkItem) CallForTurn(ctx context.Context, turn *turnctx.Turn,
	args map[string]any) (tools.Result, error) {

	actor, err := t.deps.actor(ctx, turn)
	if err != nil {
		//nolint:nilerr // A tool failure is a RESULT the caller reads.
		return notInATurn(tracker.RestoreWorkItemTool), nil
	}
	if t.deps.TrashWriter == nil || t.deps.Reader == nil {
		return unconfigured(tracker.RestoreWorkItemTool), nil
	}
	ref := strings.TrimSpace(argString(args, "item"))
	if ref == "" {
		return failed("restore_work_item needs an `item` — a key like ENG-42, or an id."), nil
	}
	// THE REMOVED HALF IS WHAT THIS READS. A restore is about a task that
	// is by definition out of every ordinary list, so the detail read is
	// the only way to reach it — and it answers for a removed task, which
	// is what makes the trash readable from both ends.
	before, err := t.deps.Reader.Task(ctx, ref, tracker.DetailWants{}, seatReadLevel)
	switch {
	case errors.Is(err, tracker.ErrNoTask):
		return failed(fmt.Sprintf("There is no work item %q.", clip(ref))), nil
	case err != nil:
		return failed(readFailure(tracker.RestoreWorkItemTool, err)), nil
	}
	if before.Task.Removed == nil {
		return failed(fmt.Sprintf("%s is not in the trash, so there is "+
			"nothing to restore.", before.Task.Key)), nil
	}

	after := before.Task
	after.Removed = nil
	got, err := t.deps.TrashWriter(actor).RestoreTask(ctx,
		opIDFor(actor, "restore", before.Task.ID), before.Task.ID,
		before.Task.Project,
		tracker.Wake{
			Kind: tracker.ChangeRestored, Before: before.Task, After: after,
		}.Notify(t.deps.Leads))
	if err != nil {
		return failed(writeFailure(tracker.RestoreWorkItemTool, err)), nil
	}
	t.deps.settle(ctx, got.Position)
	return jsonResult(map[string]any{
		"key": before.Task.Key, "restored": true,
		"outcome": string(got.Outcome), "version": got.Version,
	})
}
