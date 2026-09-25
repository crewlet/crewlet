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

// move_work_item: a board drag, as an operator reaches it.
//
// # Why no seat holds it
//
// A board's manual order is furniture a person arranges — where a card sits
// says what somebody wants looked at first, and nothing about what the work
// is. A seat that could rearrange a shared board would be one more thing a
// founder supervises for no delivery, which is the reason the saved-view tools
// are the operator's too. The LANE half is not a seat's loss: a status change
// is `update_work_item`'s, which every seat holds.
//
// # Neighbours, never keys
//
// The call names the card it was dropped BESIDE, and the tracker mints the
// key inside its own write — from the order the broker arbitrates, not from
// the board the caller last rendered. A key a caller computed is the guess the
// write authority forbids, and two people dragging in one project at once
// would both land between two keys that no longer bound anything.

// WorkMover is the board-drag write side, declared by the consumer.
type WorkMover interface {
	MoveTask(ctx context.Context, opID string, move tracker.Move,
		notify *tracker.Notify) (tracker.MoveResult, error)
}

type moveWorkItem struct{ deps WorkDeps }

var _ tools.SeatCallable = (*moveWorkItem)(nil)

func (t *moveWorkItem) Name() string { return tracker.MoveWorkItemTool }

func (t *moveWorkItem) Description() string {
	return "Move a work item on its project's board: drop it `before` or " +
		"`after` another item of the same project, and optionally into " +
		"another lane with `status` — both in one call. Name the neighbour, " +
		"never a position; the engine places it between that item and the " +
		"one beside it as the board stands when the move lands. `if_match` is " +
		"the item's `version` as you read it, and a move of an item somebody " +
		"changed since is refused."
}

func (t *moveWorkItem) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"item": map[string]any{
				"type":        "string",
				"description": "The item to move: its key (ENG-42) or its id.",
			},
			"before": map[string]any{
				"type": "string",
				"description": "The item it now sits directly above, by key " +
					"or id. Name this or `after`, never both.",
			},
			"after": map[string]any{
				"type": "string",
				"description": "The item it now sits directly below, by key " +
					"or id. Name this or `before`, never both.",
			},
			"status": map[string]any{
				"type": "string",
				"description": "The lane it was dropped into — one of: " +
					statusList() + ". Omitted keeps its own. Given with " +
					"neither neighbour, it is a drop into an empty lane: the " +
					"status changes and its place in the order does not.",
			},
			"if_match": map[string]any{
				"type": "integer",
				"description": "The item's `version` from get_work_item or the " +
					"board. The move is refused if anybody changed the item " +
					"since. A move itself never changes it, so dragging the " +
					"same card twice does not need a re-read.",
			},
		},
		"required": []any{"item", "if_match"},
	}
}

func (t *moveWorkItem) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

func (t *moveWorkItem) CallForTurn(ctx context.Context, turn *turnctx.Turn,
	args map[string]any) (tools.Result, error) {

	const name = tracker.MoveWorkItemTool
	actor, err := t.deps.actor(ctx, turn)
	if err != nil {
		//nolint:nilerr // A tool failure is a RESULT the caller reads.
		return notInATurn(name), nil
	}
	if t.deps.Mover == nil || t.deps.Reader == nil {
		return unconfigured(name), nil
	}
	ref := strings.TrimSpace(argString(args, "item"))
	if ref == "" {
		return failed("move_work_item needs an `item` — a key like ENG-42, or an id."), nil
	}
	ifMatch, given := argIntValue(args["if_match"])
	if !given || ifMatch <= 0 {
		return failed("move_work_item needs `if_match`: the item's `version` " +
			"as you read it, so a move of a card somebody changed since is " +
			"refused rather than placed."), nil
	}
	beforeRef := strings.TrimSpace(argString(args, "before"))
	afterRef := strings.TrimSpace(argString(args, "after"))
	if beforeRef != "" && afterRef != "" {
		return failed("Name `before` or `after`, not both — the one item it " +
			"was dropped beside."), nil
	}

	current, err := t.deps.Reader.Task(ctx, ref, tracker.DetailWants{}, seatRead)
	switch {
	case errors.Is(err, tracker.ErrNoTask):
		return refused(tools.RefusalNotFound, fmt.Sprintf("There is no work item %q.", clip(ref))), nil
	case err != nil:
		return readFailure(name, err), nil
	}
	task := current.Task

	move := tracker.Move{Task: task.ID, Project: task.Project, IfMatch: uint64(ifMatch)}
	if raw := strings.TrimSpace(argString(args, "status")); raw != "" {
		status := tracker.Status(raw)
		if !status.Valid() {
			return failed(fmt.Sprintf("%q is not a status. The statuses are: %s.",
				clip(raw), statusList())), nil
		}
		// THE SAME LANE IS NO LANE CHANGE: an append that changes no
		// field still stamps a version and writes a history row, and a
		// drag within a lane would put a status commit in the feed that
		// moved nothing.
		if status != task.Status {
			move.Status = &status
		}
	}
	for _, side := range []struct {
		field, ref string
		into       *string
	}{
		{"`before`", beforeRef, &move.Before},
		{"`after`", afterRef, &move.After},
	} {
		if side.ref == "" {
			continue
		}
		id, refusal := t.deps.resolveRef(ctx, name, side.field, side.ref)
		if refusal != nil {
			return *refusal, nil
		}
		if id == task.ID {
			return failed(fmt.Sprintf("%s is the item being moved — name the "+
				"item it was dropped beside.", side.field)), nil
		}
		*side.into = id
	}
	if move.Before == "" && move.After == "" && move.Status == nil {
		return failed("move_work_item needs `before`, `after` or a `status` " +
			"other than the item's own — as sent it moves nothing."), nil
	}

	var notify *tracker.Notify
	if move.Status != nil {
		patch := tracker.TaskPatch{Status: move.Status}
		notify = tracker.Wake{
			Kind: tracker.ChangeStatus, Before: task, After: patched(task, patch),
			Parent: t.deps.parentParty(ctx, task, patch),
		}.Notify(t.deps.Leads)
	}
	// ONE OPERATION PER DISTINCT CALL, keyed on what was sent — see
	// [contentKey]. A retry is the same drag; the same card dropped
	// somewhere else is another.
	got, err := t.deps.Mover(actor).MoveTask(ctx,
		opIDFor(actor, "move", task.ID+"."+contentKey(args)), move, notify)
	if err != nil {
		return writeFailure(name, err), nil
	}

	answer := map[string]any{"key": task.Key, "status": task.Status, "placed": got.Unplaced == nil}
	// THE ITEM'S VERSION AFTER THE CALL, which is what the board's next
	// if_match on this card states: the lane change moves it and the
	// placement never does.
	version := int64(task.Version)
	var (
		outcome statelog.Outcome
		at      statelog.Position
	)
	for _, part := range []tracker.WriteResult{got.Lane, got.Order} {
		if !part.Wrote() {
			continue
		}
		outcome = statelog.LessCertain(outcome, part.Outcome)
		at = statelog.Later(at, part.Position)
	}
	if move.Status != nil {
		answer["status"] = *move.Status
		if !got.Lane.Position.IsZero() {
			version = got.Lane.Position.Packed()
		}
	}
	if outcome == "" {
		// NOTHING WAS APPENDED — the card already sat there — and that is
		// a success with nothing to wait for.
		outcome = statelog.OutcomeApplied
	}
	answer["outcome"], answer["version"] = string(outcome), version
	answer["position"] = positionOf(at)
	if got.Rank != "" {
		answer["rank"] = string(got.Rank)
	}
	// THE RESIDUE IS REPORTED, never swallowed: the lane changed and the
	// card did not take its place, so the honest answer names both rather
	// than failing a call whose status write landed.
	if got.Unplaced != nil {
		answer["unplaced"] = writeFailure(name, got.Unplaced).Output
	}
	t.deps.settle(ctx, at)
	return jsonResult(answer)
}
