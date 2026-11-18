package builtin

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/tools/argval"
	"github.com/crewlet/crewlet/internal/worklet"
)

// Folding one work item into another. A verb, not `update_work_item` setting
// `duplicates` and `cancelled` by hand: a merge also moves the CHILDREN, which
// needs the subtree read before the first append, and a hand-rolled merge
// orphans the subtasks under a cancelled parent. And it is a sequence: mark the
// duplicate, re-parent each child, close the duplicate last, all under a fleet
// claim so one item is not folded twice, the close carrying the mark's session
// mark so its snapshot holds the commit that started the gesture.
//
// `duplicate_of` stays: it records an OVERLAP while both are still worked.
// Folding the two together would make a patch re-parent a subtree, breaking
// "only the fields you pass are changed".

// ---- merge_work_item ---------------------------------------------------- //

type mergeWorkItem struct {
	itemTarget
	deps WorkDeps
}

var _ tools.SeatCallable = (*mergeWorkItem)(nil)

func (t *mergeWorkItem) Name() string { return worklet.MergeWorkItemTool }

func (t *mergeWorkItem) Description() string {
	return "Fold a duplicate work item into the one that survives: the " +
		"duplicate is linked to it, its subtasks are re-parented onto it, " +
		"and the duplicate is closed as `cancelled`. Nothing is destroyed " +
		"and the duplicate stays readable, so the history of both is intact. " +
		"Use this rather than closing by hand — a cancelled item with " +
		"subtasks still under it hides work nobody will find. Say what you " +
		"folded and why with " + worklet.CommentOnWorkTool + "."
}

func (t *mergeWorkItem) Parameters() map[string]any {
	return t.deps.ops.param(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"item": map[string]any{
				"type": "string",
				"description": "The DUPLICATE — the item that is closed. " +
					itemRef,
			},
			"into": map[string]any{
				"type": "string",
				"description": "The item that SURVIVES and carries the work " +
					"on. " + itemRef,
			},
			"move_subtasks": map[string]any{
				"type": "boolean",
				"description": "Default true: the duplicate's subtasks are " +
					"re-parented onto the surviving item. Say false only " +
					"when they belong under the duplicate and nowhere else " +
					"— they then stay under a closed parent.",
			},
		},
		"required": []any{"item", "into"},
	})
}

func (t *mergeWorkItem) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

func (t *mergeWorkItem) CallForTurn(ctx context.Context, turn *turnctx.Turn,
	args map[string]any) (tools.Result, error) {

	actor, err := t.deps.actor(ctx, turn)
	if err != nil {
		//nolint:nilerr // A tool failure is a RESULT the caller reads.
		return unattributed(worklet.MergeWorkItemTool, err), nil
	}
	if t.deps.Merges == nil || t.deps.Reader == nil {
		return unconfigured(worklet.MergeWorkItemTool), nil
	}
	actor, denied := t.deps.bindOperation(actor, worklet.MergeWorkItemTool, args)
	if denied != "" {
		return failed(denied), nil
	}
	ref := strings.TrimSpace(argString(args, "item"))
	into := strings.TrimSpace(argString(args, "into"))
	switch {
	case ref == "":
		return failed(MergeWorkItemTool + " needs an `item` — the duplicate, by " +
			"key like ENG-42 or by id."), nil
	case into == "":
		return failed(MergeWorkItemTool + " needs an `into` — the item that " +
			"survives. Without it the call would close the duplicate and " +
			"say nothing about where the work went."), nil
	}
	before, err := t.deps.Reader.Task(ctx, ref, worklet.DetailWants{}, seatRead)
	switch {
	case errors.Is(err, worklet.ErrNoTask):
		return refusedBy(tools.RefusalNotFound, err, fmt.Sprintf("There is no work item %q.",
			clip(ref))), nil
	case err != nil:
		return readFailure(ctx, worklet.MergeWorkItemTool, err), nil
	}
	survivor, refusal := t.deps.resolveRef(ctx, worklet.MergeWorkItemTool, "`into`", into)
	if refusal != nil {
		return *refusal, nil
	}
	if survivor == before.Task.ID {
		return failed(fmt.Sprintf("%s cannot be folded into itself.",
			before.Named())), nil
	}

	// The snapshot describes what the merge LEAVES: the close is the last
	// append, so nothing after it would correct a `status` wake whose
	// snapshot still showed the item open.
	cancelled := before.Task
	cancelled.Status = worklet.StatusCancelled
	cancelled.StatusGroup = worklet.StatusCancelled.Group()
	opID := opIDFor(actor, t.Name(), "merge", before.Task.ID, args)
	got, err := t.deps.Merges(actor).MergeDuplicates(ctx, opID, before.Task.ID, survivor,
		moveSubtasks(args), worklet.Wake{
			Kind: worklet.ChangeStatus, Before: before.Task, After: cancelled,
		}.Notify(t.deps.Leads))
	if errors.Is(err, worklet.ErrReparentAcrossProjects) {
		// The refusal names both ways out in this surface's words: the
		// tracker knows the gestures, not the tool or argument reaching them.
		return refusedBy(tools.RefusalInvalid, err, fmt.Sprintf(MergeWorkItemTool+" "+
			"was refused: %s. Move %s "+
			"into the surviving item's project with "+worklet.MoveWorkItemTool+" first — its "+
			"subtasks go with it — or merge with `move_subtasks: false`, which "+
			"leaves them under the duplicate.", worklet.Sentence(err), before.Address())), nil
	}
	if err != nil {
		return writeFailure(ctx, actor, worklet.MergeWorkItemTool, err), nil
	}
	if got.Outcome == statelog.OutcomeUnknown {
		// Never a receipt for a merge nobody can say landed ([unknownWrite]).
		// The sequence already stops at an unknown step with its own error;
		// this holds the seam's contract rather than trusting that habit.
		return unknownWrite(actor, worklet.MergeWorkItemTool,
			fmt.Sprintf("%s was merged", before.Named()), opID, got.Unvouched,
			unknownNext(got.Unvouched, sameCall(actor, worklet.MergeWorkItemTool),
				fmt.Sprintf("Read %s with "+GetWorkItemTool+" — a merged item is "+
					"cancelled and links the one it was folded into", before.Address()),
				"it merges it a second time")), nil
	}
	t.deps.settle(ctx, got.Position)
	return jsonResult(withOperation(ReceiptOf(map[string]any{
		"merged_into":    survivor,
		"subtasks_moved": moveSubtasks(args),
		"status":         string(worklet.StatusCancelled),
		"outcome":        string(got.Outcome), "position": positionOf(got.Position), "version": got.Version,
	}, before), actor))
}

// moveSubtasks reads the verb's one knob, and ABSENT IS TRUE: a bool's zero
// value would leave children under a closed parent, the orphan this verb
// exists to prevent, so only an explicit `false` keeps them there.
func moveSubtasks(args map[string]any) bool {
	raw, held := argval.Bool(args["move_subtasks"])
	return !held || raw
}
