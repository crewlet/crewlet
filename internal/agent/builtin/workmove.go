package builtin

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/authz"
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
// # Why it is the lead's of the project the item is in
//
// Moving work to another project hands it to another team's board, which is
// the decision `routing_unit` already puts behind the project's LEAD — a
// re-route is this without the re-key — so it is decided by the same class
// ([authz.ClassContainer]): the lead of the project the item is in, or a
// holder of the admin grant. Which project that is comes out of the stored
// row rather than the arguments, so the tool asks its own action once it has
// read the item, exactly as the trash does ([subjectOf] marks it `inTool`).
//
// A MOVE INTO THE PROJECT THE ITEM IS ALREADY IN takes neither: it moves
// nothing, and it is how the retry an unknown or a stopped move prescribes
// arrives once its first attempt landed. The tracker answers it from the
// move's own ledger — this operation's move, finished — or refuses it as
// somebody else's, so the authority that matters was checked when the item
// left its project.

// ---- move_work_item ----------------------------------------------------- //

type moveWorkItem struct {
	deps WorkDeps
}

var _ tools.SeatCallable = (*moveWorkItem)(nil)

func (t *moveWorkItem) Name() string { return tracker.MoveWorkItemTool }

func (t *moveWorkItem) Description() string {
	return "Move a work item to another project, with everything under it. " +
		"Only a top-level item moves: its subtasks follow, each re-keyed in " +
		"the new project (ENG-7 becomes OPS-3) with its old key still " +
		"resolving. The lead of the item's project decides this. " +
		"An item in the trash anywhere in the subtree refuses the move until " +
		"it is restored."
}

func (t *moveWorkItem) Parameters() map[string]any {
	return t.deps.operationParam(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"item": map[string]any{
				"type":        "string",
				"description": "The top-level item to move. " + itemRef,
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
		return failedBy(err, fmt.Sprintf("There is no work item %q.", clip(ref))), nil
	case err != nil:
		return readFailed(tracker.MoveWorkItemTool, err), nil
	}
	// THE GATE IS ON THE OPERATION, NOT ON THE STATE IT PRODUCED. An item
	// already in the target is either this operation's own move answered
	// again — the retry an `unknown` or a stopped walk prescribes, whose
	// first attempt landed — or somebody else's, and which of the two only
	// the move's ledger can say ([tracker.Writer.MoveTaskToProject] answers
	// the first and refuses the second without writing). Gating it on the
	// lead of the project the item is in NOW asked the lead of the TARGET:
	// a seat leading ENG and not OPS was told to ask OPS's lead for a move
	// that had already happened, and never learned it had.
	//
	// `from` is the KEY it is leaving, which is what `moved_from` reports —
	// what the item was called, not a reference to it. The prose names the
	// item as it can be opened NOW ([tracker.TaskDetail.Named]), because a
	// duplicate of a key named by the key alone is the claimant.
	from, named := before.Task.Key, before.Named()
	if before.Task.Project == target {
		from = replacedKey(before.Task)
	} else if refused := t.deps.mayWrite(ctx, authz.Action(t.Name()), authz.Object{
		Kind: authz.KindTask, Container: before.Task.Project,
	}); refused != nil {
		return *refused, nil
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
	var stopped *tracker.MoveStopped
	switch {
	case errors.As(err, &stopped):
		// THE ROOT MOVED — whatever stopped the walk, and an unknown step
		// included, since the root's own move is not the step in doubt.
		return t.deps.moveStopped(ctx, actor, from, named, got, stopped)
	case err != nil:
		return writeFailed(actor, tracker.MoveWorkItemTool, err), nil
	}
	if got.Outcome == statelog.OutcomeUnknown {
		// NEVER A NEW KEY FOR A MOVE NOBODY CAN SAY LANDED: the one this
		// attempt minted is a gap if the root never moved. See
		// [unknownWrite], and [mergeWorkItem] for why the seam's contract
		// is held here.
		return unknownWrite(actor, tracker.MoveWorkItemTool,
			fmt.Sprintf("%s moved to %s", named, target), opID,
			got.Unvouched, unknownNext(got.Unvouched,
				sameCall(actor, tracker.MoveWorkItemTool),
				fmt.Sprintf("Read %s with get_work_item — its key and project "+
					"say where it is now", before.Address()),
				"it is refused, because the item is already there")), nil
	}
	t.deps.settle(ctx, got.Position)
	return jsonResult(withOperation(receiptItem(map[string]any{
		"moved_from": from, "project": target,
		"outcome": string(got.Outcome), "position": positionOf(got.Position),
		"version": got.Version,
	}, before.Task.ID, got.Key, got.KeyCollision), actor))
}

// moveStopped answers a cross-project move whose ROOT landed in the target and
// whose walk over the subtree did not finish ([tracker.MoveStopped]).
//
// # Why it is not a failure, and not a success either
//
// For [WorkDeps.subtreeStopped]'s reason: the item the caller named IS in the
// new project, under a new key, so "the change was NOT made" — which is what
// every such stop but an unknown step answered — was false about the one thing
// the caller asked about by name, and a caller told it moved the item again.
// And the gesture is NOT done: part of its subtree is still in the old
// project. So the answer is the root's receipt with the walk's own count
// beside it and what finishes it.
//
// # Why the remedy is the SAME operation, where a removal's is either
//
// A second removal finishes a first because a task already in the trash is
// nothing to do. A second MOVE under a new operation is refused: the root is
// in the target and that operation's ledger never put it there, which is
// exactly how somebody else's move looks ([tracker.Writer.MoveTaskToProject]).
// So the one call that finishes it here is the same one — and the root stays
// marked mid-move for as long as anything is left, so the tracker duty
// finishes the walk on its own once nobody holds its claim, which is the
// remedy where this caller has no repeat that is the same operation, or where
// this node cannot vouch for the task the walk stopped at.
func (d WorkDeps) moveStopped(ctx context.Context, actor Actor, from, named string,
	got tracker.WriteResult, stopped *tracker.MoveStopped) (tools.Result, error) {

	const tool = tracker.MoveWorkItemTool
	as := ""
	if stopped.Key != "" {
		as = " as " + tracker.ItemNamed(stopped.Root, stopped.Key, stopped.KeyCollision)
	}
	duty := "the tracker duty finishes the walk on its own once nobody is " +
		"walking it — read the item with get_work_item to see it done"
	frozen := fmt.Sprintf("%s is in the trash under it and still in its old "+
		"project, and a task in the trash is frozen, so the move waits for it: "+
		"it has to be restored (restore_work_item, where you have it) or purged "+
		"first", stopped.Waiting)
	again := sameCall(actor, tool)
	var next string
	switch {
	case errors.Is(stopped.Err, tracker.ErrStepUnvouched):
		next = fmt.Sprintf("This node cannot vouch for the task the walk "+
			"stopped at under this operation, so the same call here stops there "+
			"again, and a new one is refused. Leave it: %s.", duty)
	case stopped.Waiting != "" && again != "":
		next = fmt.Sprintf("%s. Then %s to finish the move, or leave it: %s.",
			frozen, again, duty)
	case stopped.Waiting != "":
		next = fmt.Sprintf("%s. Then %s.", frozen, duty)
	case again != "":
		next = fmt.Sprintf("%s to finish it: what already moved is left where "+
			"it is and the rest follows. Or leave it: %s.", capitalize(again), duty)
	default:
		next = fmt.Sprintf("Leave it: %s. Calling %s again here is a new "+
			"operation, and a new one is refused.", duty, tool)
	}
	d.settle(ctx, got.Position)
	answer := receiptItem(map[string]any{
		"moved_from": from, "project": stopped.Target,
		"outcome": string(got.Outcome), "position": positionOf(got.Position),
		"version":          got.Version,
		"subtree_followed": stopped.Followed, "subtree_total": stopped.Of,
		"move_stopped": fmt.Sprintf("%s moved to %s%s, but only %d of the %d "+
			"tasks under it followed before the walk stopped (%v); the rest are "+
			"still in their old project. Do not report it as done, and do not "+
			"move it again as a new call. %s", named, stopped.Target, as,
			stopped.Followed, stopped.Of, stopped.Err, next),
	}, stopped.Root, stopped.Key, stopped.KeyCollision)
	if stopped.Waiting != "" {
		answer["move_waits_for"] = stopped.Waiting
	}
	if errors.Is(stopped.Err, tracker.ErrStepUnvouched) {
		answer["move_unvouched"] = true
	}
	return jsonResult(withOperation(answer, actor))
}

// replacedKey is the key a move into the project an item is already in
// replaced: the newest of its former keys, since every move appends the key it
// replaces ([tracker.Writer.MoveTaskToProject]). The item's CURRENT key is the
// one the move minted, so an answer reporting it as `moved_from` names the
// same key twice.
//
// Newest, not this operation's own: the ledger that answers the retry keeps no
// key, so an item somebody moved out of the target and back since reports the
// key that later move replaced. An item with no former key was never moved,
// and the move refuses it rather than answering; its own key is what is left.
func replacedKey(task tracker.Task) string {
	if n := len(task.FormerKeys); n > 0 {
		return task.FormerKeys[n-1]
	}
	return task.Key
}
