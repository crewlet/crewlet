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

// Folding one work item into another.
//
// # Why this is a verb and not an argument
//
// `update_work_item` can already draw the `duplicates` link and set the status
// to `cancelled`, and a model that does both by hand has done most of a merge.
// What it has not done is move the CHILDREN, and it has no way to: the subtree
// is read before the first append, and a patch tool has no read of it. So a
// hand-rolled merge leaves subtasks under a cancelled parent, which is an
// orphan nobody finds — the board shows a closed item and the work under it
// disappears with it.
//
// It is also a SEQUENCE and the patch verb is not. The fold marks the
// duplicate, re-parents each child onto the survivor, and closes the
// duplicate last, all under a fleet claim so two callers cannot fold one item
// twice — and the close carries the session mark of the mark, so its snapshot
// contains the commit that started the gesture.
//
// # And why `duplicate_of` still exists
//
// They are different statements. `duplicate_of` records that two items
// OVERLAP, which is a link somebody may want while both are still being
// worked; this closes one of them and moves its work. Collapsing the two would
// make an ordinary patch silently re-parent a subtree, which is the one thing
// "only the fields you pass are changed" promises it will not do.

// ---- merge_work_item ---------------------------------------------------- //

type mergeWorkItem struct{ deps WorkDeps }

var _ tools.SeatCallable = (*mergeWorkItem)(nil)

func (t *mergeWorkItem) Name() string { return tracker.MergeWorkItemTool }

func (t *mergeWorkItem) Description() string {
	return "Fold a duplicate work item into the one that survives: the " +
		"duplicate is linked to it, its subtasks are re-parented onto it, " +
		"and the duplicate is closed as `cancelled`. Nothing is destroyed " +
		"and the duplicate stays readable, so the history of both is intact. " +
		"Use this rather than closing by hand — a cancelled item with " +
		"subtasks still under it hides work nobody will find. Say what you " +
		"folded and why with comment_on_work_item."
}

func (t *mergeWorkItem) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"item": map[string]any{
				"type": "string",
				"description": "The DUPLICATE — the item that is closed. Its " +
					"key (ENG-42) or its id.",
			},
			"into": map[string]any{
				"type": "string",
				"description": "The item that SURVIVES and carries the work " +
					"on. Its key or its id.",
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
	}
}

func (t *mergeWorkItem) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

func (t *mergeWorkItem) CallForTurn(ctx context.Context, turn *turnctx.Turn,
	args map[string]any) (tools.Result, error) {

	actor, err := t.deps.actor(ctx, turn)
	if err != nil {
		//nolint:nilerr // A tool failure is a RESULT the caller reads.
		return notInATurn(tracker.MergeWorkItemTool), nil
	}
	if t.deps.Merges == nil || t.deps.Reader == nil {
		return unconfigured(tracker.MergeWorkItemTool), nil
	}
	ref := strings.TrimSpace(argString(args, "item"))
	into := strings.TrimSpace(argString(args, "into"))
	switch {
	case ref == "":
		return failed("merge_work_item needs an `item` — the duplicate, by " +
			"key like ENG-42 or by id."), nil
	case into == "":
		return failed("merge_work_item needs an `into` — the item that " +
			"survives. Without it the call would close the duplicate and " +
			"say nothing about where the work went."), nil
	}
	before, err := t.deps.Reader.Task(ctx, ref, tracker.DetailWants{}, seatReadLevel)
	switch {
	case errors.Is(err, tracker.ErrNoTask):
		return failed(fmt.Sprintf("There is no work item %q.", clip(ref))), nil
	case err != nil:
		return failed(readFailure(tracker.MergeWorkItemTool, err)), nil
	}
	survivor, refusal := t.deps.resolveRef(ctx, tracker.MergeWorkItemTool, "`into`", into)
	if refusal != "" {
		return failed(refusal), nil
	}
	if survivor == before.Task.ID {
		return failed(fmt.Sprintf("%s cannot be folded into itself.",
			before.Task.Key)), nil
	}

	// THE SNAPSHOT DESCRIBES WHAT THE MERGE LEAVES, because that is what
	// the `status` wake is about: a recipient reading a snapshot that still
	// showed the item open would be told about a state the gesture was
	// closing, and the close is the last append so nothing after it corrects
	// the picture.
	cancelled := before.Task
	cancelled.Status = tracker.StatusCancelled
	cancelled.StatusGroup = tracker.StatusCancelled.Group()
	got, err := t.deps.Merges(actor).MergeDuplicates(ctx,
		opIDFor(actor, "merge", before.Task.ID), before.Task.ID, survivor,
		moveSubtasks(args), tracker.Wake{
			Kind: tracker.ChangeStatus, Before: before.Task, After: cancelled,
		}.Notify(t.deps.Leads))
	if err != nil {
		return failed(writeFailure(tracker.MergeWorkItemTool, err)), nil
	}
	t.deps.settle(ctx, got.Position)
	return jsonResult(map[string]any{
		"key": before.Task.Key, "merged_into": survivor,
		"subtasks_moved": moveSubtasks(args),
		"status":         string(tracker.StatusCancelled),
		"outcome":        string(got.Outcome), "version": got.Version,
	})
}

// moveSubtasks reads the one knob this verb has, and ABSENT IS TRUE.
//
// The zero value of a bool is the wrong default here: leaving children under a
// closed parent is the orphan this whole verb exists to prevent, so a caller
// that said nothing gets the safe outcome and only an explicit `false` keeps
// them where they are.
func moveSubtasks(args map[string]any) bool {
	raw, held := args["move_subtasks"].(bool)
	return !held || raw
}
