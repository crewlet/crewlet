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

// Moving a work item to another project: `move_work_item`. A board drag inside
// a project is `place_work_item` (workplace.go), a separate verb because the
// two differ in every property a tool is registered with. Not a patch field:
// the KEY is minted from the project's counter, so a move re-keys the item and
// its whole subtree, the tracker's cross-project move, which reads the subtree
// before its first append as a patch tool cannot.
//
// Like `routing_unit` (a re-route is a move without the re-key), it is the
// LEAD's of the item's CURRENT project, or an admin's, with the tracker
// ([authz.ClassDestructive], as a re-route's [authz.ClassContainer]), asked
// once the tool has read the stored row ([subjectOf] marks it `inTool`). A
// move into its own project is ungated: a retry the ledger answers or refuses.

// ---- move_work_item ----------------------------------------------------- //

type moveWorkItem struct {
	itemTarget
	deps WorkDeps
}

var _ tools.SeatCallable = (*moveWorkItem)(nil)

func (t *moveWorkItem) Name() string { return worklet.MoveWorkItemTool }

func (t *moveWorkItem) Description() string {
	return "Move a work item to another project, with everything under it. " +
		"Only a top-level item moves: its subtasks follow, each re-keyed in " +
		"the new project (ENG-7 becomes OPS-3) with its old key still " +
		"resolving. The lead of the item's project decides this. " +
		"An item in the trash anywhere in the subtree refuses the move until " +
		"it is restored."
}

func (t *moveWorkItem) Parameters() map[string]any {
	return t.deps.ops.param(map[string]any{
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
		return unattributed(worklet.MoveWorkItemTool, err), nil
	}
	if t.deps.Moves == nil || t.deps.Reader == nil {
		return unconfigured(worklet.MoveWorkItemTool), nil
	}
	actor, denied := t.deps.bindOperation(actor, worklet.MoveWorkItemTool, args)
	if denied != "" {
		return failed(denied), nil
	}
	ref := strings.TrimSpace(argString(args, "item"))
	target := strings.ToUpper(strings.TrimSpace(argString(args, "project")))
	switch {
	case ref == "":
		return failed(worklet.MoveWorkItemTool + " needs an `item` — a key like ENG-42, or an id."), nil
	case target == "":
		return failed(worklet.MoveWorkItemTool + " needs a `project` — the key of the " +
			"project the item moves to."), nil
	}
	before, err := t.deps.Reader.Task(ctx, ref, worklet.DetailWants{}, seatRead)
	switch {
	case errors.Is(err, worklet.ErrNoTask):
		return refusedBy(tools.RefusalNotFound, err, fmt.Sprintf("There is no work item %q.",
			clip(ref))), nil
	case err != nil:
		return readFailure(ctx, worklet.MoveWorkItemTool, err), nil
	}
	// The gate is on the operation, not the state it produced: an item
	// already in the target is this operation's retry (after an `unknown` or
	// a stopped walk) or somebody else's move, and only the move's ledger can
	// tell ([worklet.Writer.MoveTaskToProject] answers the first, refuses the
	// second). Gating on the item's project NOW would ask the TARGET's lead.
	//
	// `from` is the KEY being left, which `moved_from` reports; the prose
	// names the item as it opens NOW ([worklet.TaskDetail.Named]), because a
	// duplicate of a key named by the key alone is the claimant.
	from, named := before.Task.Key, before.Named()
	if before.Task.Project == target {
		from = replacedKey(before.Task)
	} else if denied := t.deps.mayWrite(ctx, authz.Action(t.Name()), authz.Object{
		Kind: authz.KindTask, Container: before.Task.Project,
	}); denied != nil {
		return *denied, nil
	}

	// The wake names the project the item lands in; the move mints its new
	// key, which the recipient reads off the item.
	after := before.Task
	after.Project = target
	opID := opIDFor(actor, t.Name(), "move", before.Task.ID, args)
	got, err := t.deps.Moves(actor).MoveTaskToProject(ctx, opID, before.Task.ID,
		target, worklet.Wake{
			Kind: worklet.ChangeMoved, Before: before.Task, After: after,
		}.Notify(t.deps.Leads))
	var stopped *worklet.MoveStopped
	switch {
	case errors.As(err, &stopped):
		// The root moved, whatever stopped the walk (an unknown step too:
		// the root's own move is not the step in doubt).
		return t.deps.moveStopped(ctx, actor, from, named, got, stopped)
	case err != nil:
		return writeFailure(ctx, actor, worklet.MoveWorkItemTool, err), nil
	}
	if got.Outcome == statelog.OutcomeUnknown {
		// Never a new key for a move nobody can say landed: the one minted
		// is a gap if the root never moved ([unknownWrite]; [mergeWorkItem]
		// says why the seam's contract is held here).
		return unknownWrite(actor, worklet.MoveWorkItemTool,
			fmt.Sprintf("%s moved to %s", named, target), opID,
			got.Unvouched, unknownNext(got.Unvouched,
				sameCall(actor, worklet.MoveWorkItemTool),
				fmt.Sprintf("Read %s with "+GetWorkItemTool+" — its key and project "+
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
// whose walk over the subtree did not finish ([worklet.MoveStopped]). Neither
// failure nor success, for [WorkDeps.subtreeStopped]'s reason: the named item
// IS moved and re-keyed, so "NOT made" would get it moved again, yet part of
// its subtree is still in the old project. So: the root's receipt, the walk's
// count, and what finishes it.
//
// The remedy is the SAME operation: unlike a second removal, a second move
// under a new operation is refused as somebody else's
// ([worklet.Writer.MoveTaskToProject]). The root stays marked mid-move, so the
// Worklet duty finishes the walk once nobody holds its claim: the remedy where
// the caller has no same-operation repeat, or no vouching for its stop.
func (d WorkDeps) moveStopped(ctx context.Context, actor Actor, from, named string,
	got worklet.WriteResult, stopped *worklet.MoveStopped) (tools.Result, error) {

	const tool = worklet.MoveWorkItemTool
	as := ""
	if stopped.Key != "" {
		as = " as " + worklet.ItemNamed(stopped.Root, stopped.Key, stopped.KeyCollision)
	}
	duty := "the Worklet duty finishes the walk on its own once nobody is " +
		"walking it; read the item with " + GetWorkItemTool + " to see it done"
	frozen := fmt.Sprintf("%s is in the trash under it and still in its old "+
		"project, and a task in the trash is frozen, so the move waits for it: "+
		"it has to be restored ("+worklet.RestoreWorkItemTool+", where you have it) or purged "+
		"first", stopped.Waiting)
	again := sameCall(actor, tool)
	var next string
	switch {
	case errors.Is(stopped.Err, worklet.ErrStepUnvouched):
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
	}, stopped.Root, stopped.Key, stopped.KeyCollision)
	said := partial(answer, MoveStoppedKey, caveat.MoveUnfinished(stopped.Followed,
		stopped.Of, stopped.Waiting != "", fmt.Sprintf("%s moved to %s%s, but only "+
			"%d of the %d tasks under it followed before the walk stopped (%s); the "+
			"rest are still in their old project. Do not report it as done, and do "+
			"not move it again as a new call. %s", named, stopped.Target, as,
			stopped.Followed, stopped.Of, writeTold(ctx, actor, tool, stopped.Err), next)))
	if stopped.Waiting != "" {
		answer["move_waits_for"] = stopped.Waiting
	}
	if errors.Is(stopped.Err, worklet.ErrStepUnvouched) {
		answer["move_unvouched"] = true
	}
	return caveated(withOperation(answer, actor), nil, said)
}

// replacedKey is the key a move into the project an item is already in
// replaced: the newest former key, since every move appends the key it
// replaces ([worklet.Writer.MoveTaskToProject]); the CURRENT key is the one the
// move minted, which as `moved_from` would name one key twice. Newest, not this
// operation's own: the ledger answering the retry keeps no key, so an item
// moved out and back since reports that later move's key. An item with no
// former key was never moved (the move refuses it); its own key is left.
func replacedKey(task worklet.Task) string {
	if n := len(task.FormerKeys); n > 0 {
		return task.FormerKeys[n-1]
	}
	return task.Key
}
