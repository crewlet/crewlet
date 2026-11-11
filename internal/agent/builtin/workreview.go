package builtin

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/agent/colleague"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/worklet"
)

// A REVIEW NAMES ITS REVIEWER (ADR-0124). An agent that moves an item it keeps
// into review with nobody named leaves a review nobody holds: a seat never
// reviews what it submitted itself, and a person never hears of it. So the move
// names `reviewer`: a colleague seat is handed the item, and a person is asked.

// reviewerParam is update_work_item's `reviewer` argument.
var reviewerParam = map[string]any{
	"type": "string",
	"description": "Who reviews the item, by handle, when it is in `in_review`. Required of an " +
		"agent moving an item it keeps into review. A colleague seat is handed the item to " +
		"review. A person is asked: ask them first on this item with " + worklet.CommentOnWorkTool +
		", `ask` and a `decision` (approve, or what to change), which puts the review in their " +
		"Inbox, and keep the item until they answer.",
}

// holdReview applies `reviewer` to an update, and refuses an agent's move of an
// item into review that leaves it with nobody reviewing it. It hands the item to
// an agent reviewer by setting the patch's assignee and kind.
func (d WorkDeps) holdReview(ctx context.Context, actor Actor, before worklet.TaskDetail,
	args map[string]any, patch *worklet.TaskPatch, kind *worklet.ChangeKind) *tools.Result {

	status := before.Task.Status
	if patch.Status != nil {
		status = *patch.Status
	}
	holder := before.Task.Assignee
	if patch.Assignee != nil {
		holder = *patch.Assignee
	}
	named := strings.TrimSpace(argString(args, "reviewer"))
	into := status == worklet.StatusInReview && before.Task.Status != worklet.StatusInReview
	switch {
	case named == "" && (actor.Kind != worklet.AuthorAgent || !into):
		return nil
	case named != "" && status != worklet.StatusInReview:
		return refusalOf(failed(UpdateWorkItemTool + ": `reviewer` names who reviews an item in " +
			"`in_review`, and this call leaves " + before.Named() + " in `" + string(status) +
			"`. Move it into review with `status` as well, or leave `reviewer` out. Nothing was " +
			"written."))
	case d.Seats == nil:
		return refusalOf(unconfigured(UpdateWorkItemTool))
	}
	roster := d.Seats()
	if named == "" {
		if seat, held := seatIn(roster, holder); held && holder != actor.Handle &&
			seat.Kind == string(org.KindAgent) {
			// HANDED TO A COLLEAGUE: the seat it goes to is its reviewer, and
			// the hand-off is an assignment, which asks it to answer.
			if patch.Assignee != nil {
				*kind = worklet.ChangeAssignee
			}
			return nil
		}
		return refusalOf(failed(fmt.Sprintf("%s: moving %s into review needs its `reviewer`, "+
			"since a seat never reviews what it submitted itself and a review nobody holds "+
			"waits on nobody. Name a colleague seat to hand it to, or a person you have asked on "+
			"the item with %s and `ask`. Nothing was written.", UpdateWorkItemTool, before.Named(),
			worklet.CommentOnWorkTool)))
	}
	reviewer, refusal := d.resolveHandle(ctx, UpdateWorkItemTool, "`reviewer`", named)
	if refusal != "" {
		return refusalOf(failed(refusal))
	}
	seat, held := seatIn(roster, reviewer)
	switch {
	case reviewer == actor.Handle:
		return refusalOf(failed(UpdateWorkItemTool + ": you cannot review your own work. Name a " +
			"colleague or a person as `reviewer`. Nothing was written."))
	case !held:
		return refusalOf(failed(fmt.Sprintf("%s: `reviewer` %q is not a seat here. Nothing was "+
			"written.", UpdateWorkItemTool, clip(reviewer))))
	case seat.Kind == string(org.KindAgent):
		if patch.Assignee != nil && *patch.Assignee != reviewer {
			return refusalOf(failed(fmt.Sprintf("%s: `reviewer` %s is handed the item, and "+
				"`assignee` names %s. Name one of them. Nothing was written.", UpdateWorkItemTool,
				reviewer, *patch.Assignee)))
		}
		patch.Assignee = &reviewer
		*kind = worklet.ChangeAssignee
		return nil
	}
	if actor.Kind != worklet.AuthorAgent {
		return nil
	}
	asked, refused := d.openAsksBy(ctx, before, actor.Handle)
	if refused != nil {
		return refused
	}
	if slices.Contains(asked, reviewer) {
		return nil
	}
	return refusalOf(failed(fmt.Sprintf("%s: %s reviews as a person, and you have no open ask "+
		"to them on %s. Ask them first with %s, `ask` and a `decision` (approve, or what to "+
		"change), which puts the review in their Inbox; then move the item into review naming "+
		"them as `reviewer`, and keep it until they answer. Nothing was written.",
		UpdateWorkItemTool, reviewer, before.Named(), worklet.CommentOnWorkTool)))
}

// agentHandles is the roster's agent seats, nil where no roster is wired.
func (d WorkDeps) agentHandles() []string {
	if d.Seats == nil {
		return nil
	}
	var out []string
	for _, seat := range d.Seats() {
		if seat.Kind == string(org.KindAgent) {
			out = append(out, seat.Handle)
		}
	}
	return out
}

// seatIn is the roster's seat with handle, if it holds one.
func seatIn(roster []colleague.Seat, handle string) (colleague.Seat, bool) {
	for _, seat := range roster {
		if seat.Handle == handle {
			return seat, true
		}
	}
	return colleague.Seat{}, false
}
