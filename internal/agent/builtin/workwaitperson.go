package builtin

import (
	"context"
	"fmt"
	"slices"

	"github.com/crewlet/crewlet/internal/agent/colleague"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/words"
	"github.com/crewlet/crewlet/internal/worklet"
)

// A WAIT ON A PERSON IS AN ASK (ADR-0122). An agent that sets an item aside
// until a date while the reason names a person waits on that person with
// nothing in their Inbox: the date passes whether or not they answered, and
// they never learn they were waited on. So such a wait is refused unless the
// agent already holds an open ask to that person on the item.

// checkWaitOnPerson refuses an agent's `wait_until` whose `wait_reason` names a
// person it has no open ask to on the item, and nil for every other write.
func (d WorkDeps) checkWaitOnPerson(ctx context.Context, actor Actor, item worklet.TaskDetail,
	patch worklet.TaskPatch) *tools.Result {

	if actor.Kind != worklet.AuthorAgent || patch.WaitReason == nil || d.Seats == nil {
		return nil
	}
	persons := worklet.PersonsNamed(*patch.WaitReason, PersonSeats(d.Seats()))
	if len(persons) == 0 {
		return nil
	}
	asked, refused := d.openAsksBy(ctx, item, actor.Handle)
	if refused != nil {
		return refused
	}
	var unasked []string
	for _, handle := range persons {
		if !slices.Contains(asked, handle) {
			unasked = append(unasked, handle)
		}
	}
	if len(unasked) == 0 {
		return nil
	}
	return refusalOf(failed(fmt.Sprintf("%s: `wait_reason` waits on %s, a person, and you "+
		"have no open ask to them on %s. A wait on a person is an ask, not a date: ask them "+
		"with %s and `ask` (with a `decision`), which puts the question in their Inbox and "+
		"holds the item until they answer. Set `wait_until` only for a real date, such as a "+
		"release window or an agreed start. Nothing was written.", UpdateWorkItemTool,
		words.And(unasked), item.Named(), CommentOnWorkTool)))
}

// openAsksBy is whom author's open asks on item are put to: every comment of
// the item read, page by page, since an ask stays open however old it is.
func (d WorkDeps) openAsksBy(ctx context.Context, item worklet.TaskDetail,
	author string) ([]string, *tools.Result) {

	var comments []worklet.Comment
	cursor := ""
	for {
		page, err := d.Reader.Task(ctx, item.Task.ID, worklet.DetailWants{Comments: true,
			CommentLimit: worklet.MaxCommentPage, CommentCursor: cursor}, seatRead)
		if err != nil {
			refused := readFailure(ctx, UpdateWorkItemTool, err)
			return nil, &refused
		}
		comments = append(comments, page.Comments...)
		if page.CommentsCursor == "" || page.CommentsCursor == cursor {
			break
		}
		cursor = page.CommentsCursor
	}
	answered := map[string]bool{}
	for _, c := range comments {
		if c.Answers != nil {
			answered[*c.Answers] = true
		}
	}
	var out []string
	for _, c := range comments {
		if c.Author == author && c.Ask != "" && !c.Resolved && !c.Removed && !answered[c.ID] &&
			!slices.Contains(out, c.Ask) {
			out = append(out, c.Ask)
		}
	}
	return out, nil
}

// PersonSeats is the persons of roster a wait's reason can name, as the
// tracker's matcher reads them ([worklet.PersonsNamed]): every person seat the
// directory does not withhold, since a person nobody can reach cannot be asked.
func PersonSeats(roster []colleague.Seat) []worklet.PersonSeat {
	var out []worklet.PersonSeat
	for _, seat := range roster {
		if seat.Kind == string(org.KindHuman) && !seat.Withheld {
			out = append(out, worklet.PersonSeat{Handle: seat.Handle, Name: seat.Name})
		}
	}
	return out
}
