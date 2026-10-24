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

// place_work_item: a board drag, as an operator reaches it. Never a move to
// another project, which re-keys what it carries and is `move_work_item`
// (workmove.go): the two differ in every property a tool is registered with —
// this one is a person's and idempotent, that one a seat's behind a lead's
// gate and not — so one name could not be both.
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
//
// # The lane is sent as dropped
//
// The drop's `status` is passed to the tracker AS SENT, never compared with
// the card first. A retry of a drag whose lane change already landed reads the
// card as in its new lane; a tool that dropped the status there conditioned
// the placement on the version it read BEFORE that change and was refused as
// stale by nobody but itself. The tracker answers the lane step of a retry
// from its ledger, and decides a drop into the lane the card is already in to
// write nothing on the task — so sending it is always safe, and the gate is on
// the OPERATION rather than on the state it produced ([tracker.Place]).

// WorkPlacer is the board-drag write side, declared by the consumer.
type WorkPlacer interface {
	PlaceTask(ctx context.Context, opID string, place tracker.Place,
		notify *tracker.Notify) (tracker.PlaceResult, error)
}

type placeWorkItem struct{ deps WorkDeps }

var _ tools.SeatCallable = (*placeWorkItem)(nil)

func (t *placeWorkItem) Name() string { return tracker.PlaceWorkItemTool }

func (t *placeWorkItem) Description() string {
	return "Place a work item on its project's board: drop it `before` or " +
		"`after` another item of the same project, and optionally into " +
		"another lane with `status` — both in one call. Name the neighbour, " +
		"never a position; the engine places it between that item and the " +
		"one beside it as the board stands when the drop lands. `if_match` " +
		"is the item's `version` as you read it, and a drop of an item " +
		"somebody changed since is refused. To move an item to another " +
		"project, use move_work_item."
}

func (t *placeWorkItem) Parameters() map[string]any {
	return t.deps.operationParam(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"item": map[string]any{
				"type":        "string",
				"description": "The item to place. " + itemRef,
			},
			"before": map[string]any{
				"type": "string",
				"description": "The item it now sits directly above. Name " +
					"this or `after`, never both. " + itemRef,
			},
			"after": map[string]any{
				"type": "string",
				"description": "The item it now sits directly below. Name " +
					"this or `before`, never both. " + itemRef,
			},
			"status": map[string]any{
				"type": "string",
				"description": "The lane it was dropped into — one of: " +
					statusList() + ". Send the lane it was dropped into " +
					"even when it is the item's own: that writes nothing on " +
					"the item. Omitted keeps its own. Given with neither " +
					"neighbour, it is a drop into an empty lane: the status " +
					"changes and its place in the order does not.",
			},
			"if_match": map[string]any{
				"type": "integer",
				"description": "The item's `version` from get_work_item or the " +
					"board. The drop is refused if anybody changed the item " +
					"since. A placement itself never changes it, so dragging " +
					"the same card twice does not need a re-read.",
			},
		},
		"required": []any{"item", "if_match"},
	})
}

func (t *placeWorkItem) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

func (t *placeWorkItem) CallForTurn(ctx context.Context, turn *turnctx.Turn,
	args map[string]any) (tools.Result, error) {

	const name = tracker.PlaceWorkItemTool
	actor, err := t.deps.actor(ctx, turn)
	if err != nil {
		//nolint:nilerr // A tool failure is a RESULT the caller reads.
		return notInATurn(name), nil
	}
	if t.deps.Placer == nil || t.deps.Reader == nil {
		return unconfigured(name), nil
	}
	actor, denied := t.deps.bindOperation(actor, name, args)
	if denied != "" {
		return failed(denied), nil
	}
	ref := strings.TrimSpace(argString(args, "item"))
	if ref == "" {
		return failed("place_work_item needs an `item` — a key like ENG-42, or an id."), nil
	}
	ifMatch, given := argIntValue(args["if_match"])
	if !given || ifMatch <= 0 {
		return failed("place_work_item needs `if_match`: the item's `version` " +
			"as you read it, so a drop of a card somebody changed since is " +
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
		return refusedBy(tools.RefusalNotFound, err, fmt.Sprintf("There is no work item %q.",
			clip(ref))), nil
	case err != nil:
		return readFailure(ctx, name, err), nil
	}
	task := current.Task

	place := tracker.Place{Task: task.ID, Project: task.Project, IfMatch: uint64(ifMatch)}
	if raw := strings.TrimSpace(argString(args, "status")); raw != "" {
		status := tracker.Status(raw)
		if !status.Valid() {
			return failed(fmt.Sprintf("%q is not a status. The statuses are: %s.",
				clip(raw), statusList())), nil
		}
		// AS SENT — see the file head. The tracker decides a drop into
		// the lane the card is in to write nothing on the task.
		place.Status = &status
	}
	for _, side := range []struct {
		field, ref string
		into       *string
	}{
		{"`before`", beforeRef, &place.Before},
		{"`after`", afterRef, &place.After},
	} {
		if side.ref == "" {
			continue
		}
		id, refusal := t.deps.resolveRef(ctx, name, side.field, side.ref)
		if refusal != nil {
			return *refusal, nil
		}
		if id == task.ID {
			return failed(fmt.Sprintf("%s is the item being placed — name the "+
				"item it was dropped beside.", side.field)), nil
		}
		*side.into = id
	}
	if place.Before == "" && place.After == "" && place.Status == nil {
		return failed("place_work_item needs `before`, `after` or a `status` " +
			"— as sent it moves nothing."), nil
	}

	// A WAKE ONLY FOR A LANE THE CARD LEAVES: a drop into its own lane
	// publishes no status record, so there is nobody to tell.
	var notify *tracker.Notify
	if place.Status != nil && *place.Status != task.Status {
		patch := tracker.TaskPatch{Status: place.Status}
		notify = tracker.Wake{
			Kind: tracker.ChangeStatus, Before: task, After: patched(task, patch),
			Parent: t.deps.parentParty(ctx, task, patch),
		}.Notify(t.deps.Leads)
	}
	// ONE OPERATION PER DISTINCT CALL, derived from what was sent — see
	// [opIDFor]. A retry is the same drag and is answered step by step from
	// the ledger; the same card dropped somewhere else is another.
	opID := opIDFor(actor, t.Name(), "place", task.ID, args)
	got, err := t.deps.Placer(actor).PlaceTask(ctx, opID, place, notify)
	if err != nil {
		return writeFailure(ctx, actor, name, err), nil
	}
	if got.Lane.Outcome == statelog.OutcomeUnknown ||
		got.Order.Outcome == statelog.OutcomeUnknown {
		// NEVER A RECEIPT FOR A DROP NOBODY CAN SAY LANDED — see
		// [unknownWrite]. A rank beside the word "unknown" reads as where
		// the card now sits, and a person's board would draw it there.
		unvouched := (got.Lane.Outcome == statelog.OutcomeUnknown && got.Lane.Unvouched) ||
			(got.Order.Outcome == statelog.OutcomeUnknown && got.Order.Unvouched)
		return unknownWrite(actor, name, dropped(current, args),
			opID, unvouched, unknownNext(unvouched, sameCall(actor, name),
				fmt.Sprintf("Read %s with get_work_item — its status and the "+
					"board say where it is now", current.Address()),
				"it places the card again, which changes nothing where it "+
					"already sits")), nil
	}

	answer := withOperation(ReceiptOf(map[string]any{
		"status": task.Status, "placed": got.Unplaced == nil,
		// THE ITEM'S VERSION AFTER THE CALL, which is what the board's
		// next if_match on this card states: the lane change moves it and
		// the placement never does.
		"version": got.Version,
	}, current), actor)
	if place.Status != nil {
		answer["status"] = *place.Status
	}
	// THE CALL ANSWERS FOR BOTH RECORDS — see [callOutcome] — and a drop
	// that appended nothing (the card already sat there) is `applied` with
	// nothing to wait for.
	var call callOutcome
	call.add(got.Lane.Result)
	call.add(got.Order.Result)
	call.stamp(answer)
	if got.Rank != "" {
		answer["rank"] = string(got.Rank)
	}
	// THE RESIDUE IS REPORTED, never swallowed: the lane changed and the
	// card did not take its place, so the honest answer names both rather
	// than failing a call whose status write landed.
	if got.Unplaced != nil {
		answer["unplaced"] = writeFailure(ctx, actor, name, got.Unplaced).Output
	}
	t.deps.settle(ctx, call.at)
	return jsonResult(answer)
}

// dropped says what a drag asked for, as the clause an unknown answer names:
// "ENG-4 dropped before ENG-2 into in_progress" — the card named as prose names
// a task ([tracker.TaskDetail.Named]), since a key another task claimed first
// would name that one.
func dropped(card tracker.TaskDetail, args map[string]any) string {
	what := card.Named() + " dropped"
	if ref := strings.TrimSpace(argString(args, "before")); ref != "" {
		what += " before " + clip(ref)
	}
	if ref := strings.TrimSpace(argString(args, "after")); ref != "" {
		what += " after " + clip(ref)
	}
	if status := strings.TrimSpace(argString(args, "status")); status != "" {
		what += " into " + clip(status)
	}
	return what
}
