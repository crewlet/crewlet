package builtin

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/tracker"
)

// Moving a work item to another project.
//
// # Why this is a verb and not an argument
//
// A project is not a field a patch can change: the item's KEY is minted from
// its project's counter, so a move re-keys it, and every subtask under it has
// to follow or the tree is split across two boards. That is the tracker's
// cross-project move — a sequence that reads the subtree before its first
// append, takes a fleet claim, mints a key range and walks the descendants —
// and a patch tool has no read of the subtree to do any of it. The sequence
// existed, and so did the documentation of what it leaves behind when it
// stops part of the way through; nothing could start one.
//
// # Why it is a lead's, or a person's
//
// Moving work to another project hands it to another team's board, which is
// the decision `routing_unit` already puts behind the project's LEAD — a
// re-route is this without the re-key. The same two arms apply for the same
// reasons: the lead of the project the item is in, asked about the person
// behind the credential ([Actor.Record]), or a person acting through their own
// credential ([tracker.AuthorKind.Person]).

// ---- move_work_item ----------------------------------------------------- //

type moveWorkItem struct {
	deps WorkDeps

	// leads answers whether a handle leads the project an item is in —
	// the same seam [updateWorkItem] gates a re-route on.
	leads LeadsProject
}

var _ tools.SeatCallable = (*moveWorkItem)(nil)

func (t *moveWorkItem) Name() string { return tracker.MoveWorkItemTool }

func (t *moveWorkItem) Description() string {
	return "Move a work item to another project, with everything under it. " +
		"Only a top-level item moves: its subtasks follow, each re-keyed in " +
		"the new project (ENG-7 becomes OPS-3) with its old key still " +
		"resolving. The lead of the item's project decides this, or a person. " +
		"An item in the trash anywhere in the subtree refuses the move until " +
		"it is restored."
}

func (t *moveWorkItem) Parameters() map[string]any {
	return t.deps.operationParam(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"item": map[string]any{
				"type": "string",
				"description": "The top-level item to move: its key (ENG-42) " +
					"or its id.",
			},
			"project": map[string]any{
				"type":        "string",
				"description": "The key of the project it moves to.",
			},
		},
		"required": []any{"item", "project"},
	})
}

func (t *moveWorkItem) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

func (t *moveWorkItem) CallForTurn(ctx context.Context, turn *turnctx.Turn,
	args map[string]any) (tools.Result, error) {

	actor, err := t.deps.actor(ctx, turn)
	if err != nil {
		//nolint:nilerr // A tool failure is a RESULT the caller reads.
		return notInATurn(tracker.MoveWorkItemTool), nil
	}
	if t.deps.Moves == nil || t.deps.Reader == nil {
		return unconfigured(tracker.MoveWorkItemTool), nil
	}
	actor, denied := t.deps.bindOperation(actor, tracker.MoveWorkItemTool, args)
	if denied != "" {
		return failed(denied), nil
	}
	ref := strings.TrimSpace(argString(args, "item"))
	target := strings.ToUpper(strings.TrimSpace(argString(args, "project")))
	switch {
	case ref == "":
		return failed("move_work_item needs an `item` — a key like ENG-42, or an id."), nil
	case target == "":
		return failed("move_work_item needs a `project` — the key of the " +
			"project the item moves to."), nil
	}
	before, err := t.deps.Reader.Task(ctx, ref, tracker.DetailWants{}, seatRead)
	switch {
	case errors.Is(err, tracker.ErrNoTask):
		return failed(fmt.Sprintf("There is no work item %q.", clip(ref))), nil
	case err != nil:
		return failed(readFailure(tracker.MoveWorkItemTool, err)), nil
	}
	lead := t.leads != nil && t.leads(ctx, actor.Record(), before.Task.Project)
	if !lead && !actor.Kind.Person() {
		return failed(fmt.Sprintf("Moving %s out of %s is the lead of %s's "+
			"decision, not yours. Ask them, or say in a comment where it "+
			"belongs.", before.Task.Key, before.Task.Project,
			before.Task.Project)), nil
	}

	// THE WAKE DESCRIBES WHERE THE ITEM IS GOING. Its new key is minted by
	// the move itself, so the snapshot names the project it lands in and
	// the recipient reads the key off the item.
	after := before.Task
	after.Project = target
	opID := opIDFor(actor, t.Name(), "move", before.Task.ID, args)
	got, err := t.deps.Moves(actor).MoveTaskToProject(ctx, opID, before.Task.ID,
		target, tracker.Wake{
			Kind: tracker.ChangeMoved, Before: before.Task, After: after,
		}.Notify(t.deps.Leads))
	if err != nil {
		return failed(writeFailure(actor, tracker.MoveWorkItemTool, err)), nil
	}
	if got.Outcome == statelog.OutcomeUnknown {
		// NEVER A NEW KEY FOR A MOVE NOBODY CAN SAY LANDED: the one this
		// attempt minted is a gap if the root never moved. See
		// [unknownWrite], and [mergeWorkItem] for why the seam's contract
		// is held here.
		return failed(unknownWrite(actor, tracker.MoveWorkItemTool,
			fmt.Sprintf("%s moved to %s", before.Task.Key, target), opID,
			got.Unvouched, unknownNext(got.Unvouched,
				sameCall(actor, tracker.MoveWorkItemTool),
				fmt.Sprintf("Read %s with get_work_item — its key and project "+
					"say where it is now", before.Task.Key),
				"it is refused, because the item is already there"))), nil
	}
	t.deps.settle(ctx, got.Position)
	return jsonResult(withOperation(map[string]any{
		"key": got.Key, "moved_from": before.Task.Key, "project": target,
		"outcome": string(got.Outcome), "position": positionOf(got.Position),
		"version": got.Version,
	}, actor))
}
