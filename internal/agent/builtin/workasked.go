package builtin

import (
	"context"
	"errors"

	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/worklet"
)

// ANSWERING WHAT YOU WERE ASKED, WITHOUT THE TRACKER.
//
// A company that keeps somebody off its tracker still asks them things — a
// decision only legal can make, a figure only finance holds — and the
// question is filed on a work item. Refused the item, they cannot read what
// they were asked; refused its thread, they cannot answer; and the ask sits
// open for ever while whoever put it waits. So `get_work_item` and
// `comment_on_work_item` are decided by [authz.ClassAppOrAsked]: a tracker
// holder as ever, OR the person an OPEN ask on the item names — and for the
// second, the comment is admitted only as THE ANSWER to that ask.
//
// # Two decisions, because the second needs the row
//
// Which asks are open on an item, and whom they name, is a stored row, so
// the first decision is taken on the item nobody has read yet
// ([authz.Object.Unresolved]): a tracker holder is admitted there and the
// tool goes on exactly as it always did, and anybody else is told
// [authz.ErrUnresolved] — read the item, resolve its asks, decide again.
// [WorkDeps.itemGate] is that first half and [WorkDeps.askedOn] the second.
//
// # One refusal for an item that is not there and one you were not asked on
//
// Told apart, a caller without Worklet could learn which keys exist one
// guess at a time — "there is no ENG-7" against "you may not read ENG-8" —
// on a board they were deliberately kept off. So a missing item is refused
// with the very decision an item they were not asked on gets: the table's
// own refusal naming the tracker, which is the one thing that would change
// either answer.
//
// # The probe asks before anything about the thread is said
//
// The answer's thread read ([worklet.Reader.Thread]) refuses an `answers`
// naming somebody else's question by saying whose it was. Told to a caller
// nobody asked anything, that would be the item's thread read out one refusal
// at a time; so whether they are asked AT ALL is settled first, by the one
// read that infers it ([worklet.AskedOn]), and only somebody who is asked
// (and who may therefore read the whole item anyway) is told anything about
// its questions.

// itemGate is the first half: the decision on an item nobody has read yet.
//
// It answers true for a caller the table admits outright (a tracker holder,
// or admin through it), false with no refusal for a caller who may yet be
// admitted as the person an open ask names, and otherwise the refusal to
// return — a caller the table refuses on any other ground, or a node that
// could not decide.
func (d WorkDeps) itemGate(ctx context.Context, action authz.Action) (bool, *tools.Result) {
	if d.Authorize == nil {
		refusal := authorityRefusal(string(action), ErrNoAuthorizer)
		return false, &refusal
	}
	err := d.Authorize(ctx, action, authz.Object{Kind: authz.KindTask, Unresolved: true})
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, authz.ErrUnresolved):
		return false, nil
	}
	refusal := authorityRefusal(string(action), err)
	return false, &refusal
}

// askedOn is the second half, for a caller [WorkDeps.itemGate] did not admit
// outright: read the item ref names, ask whether an open ask on it names
// handle, and decide again on what the row says.
//
// self derives, from the item's id, the id the caller's own comment will carry
// on it ([commentID]) — nil for a read, which posts nothing. It is what admits
// the caller's answer made again once it has landed and closed the ask that
// admitted it ([worklet.AskedOn]): that call writes nothing, and refusing it
// told somebody their answer needed the tracker after it had been given.
//
// It answers the item — read with no parts, which is what the probe and the
// decision need — or the refusal to return.
func (d WorkDeps) askedOn(ctx context.Context, action authz.Action, ref,
	handle string, self func(task string) string) (worklet.TaskDetail, *tools.Result) {

	item, err := d.Reader.Task(ctx, ref, worklet.DetailWants{}, seatRead)
	switch {
	case errors.Is(err, worklet.ErrNoTask):
		return worklet.TaskDetail{}, d.notAsked(ctx, action)
	case err != nil:
		return worklet.TaskDetail{}, refusalOf(readFailure(ctx, string(action), err))
	}
	comment := ""
	if self != nil {
		comment = self(item.Task.ID)
	}
	asked, err := worklet.AskedOn(ctx, d.Reader, item.Task.ID, handle, comment,
		seatRead)
	if err != nil {
		// A PROBE THAT COULD NOT READ HAS NOT FOUND AN ASK, and has not
		// found that there is none either: this node cannot tell, and
		// answering a refusal would send somebody who WAS asked to go and
		// ask for the tracker.
		return worklet.TaskDetail{}, refusalOf(readFailure(ctx, string(action), err))
	}
	object := authz.Object{Kind: authz.KindTask, Container: item.Task.Project}
	if asked {
		object.Asked = handle
	}
	if refused := d.mayWrite(ctx, action, object); refused != nil {
		return worklet.TaskDetail{}, refused
	}
	return item, nil
}

// notAsked is the refusal of somebody without Worklet on an item no open
// ask names them on — and, by the rule in this file's doc, on an item that is
// not there at all.
//
// THE TABLE'S OWN, decided on the bare kind, so it names the grant that would
// admit ([authz.Decision.Grants], the tracker) and reads in the same words on
// every surface. A table that admitted the bare kind after all — only an
// authorizer that disagrees with itself between two calls could — is told
// apart by the nil: the caller then goes on as a holder and the ordinary read
// says what it found.
func (d WorkDeps) notAsked(ctx context.Context, action authz.Action) *tools.Result {
	return d.mayWrite(ctx, action, authz.Object{Kind: authz.KindTask})
}

// errAuthorityContradicted is an authority decision that admitted, on the bare
// kind, a caller it had just admitted only as the person an ask names — two
// answers to one question about one principal within one call.
var errAuthorityContradicted = errors.New("builtin: the authority decision " +
	"admitted the tracker to a caller it had just admitted only as the person " +
	"an ask names")

// answerOnly is [WorkDeps.notAsked] for a comment from somebody admitted as the
// person an ask names that is not an answer to it, with what they may do
// instead: the table admits them to the answer and nothing else, and a
// refusal that said only "you need the tracker" would leave them guessing
// which part of the call was the problem.
//
// AN AUTHORIZER THAT ADMITS THE BARE KIND HERE CONTRADICTS ITSELF: the caller
// reached this only because it would not admit them as a tracker holder, and
// [authz.ClassAppOrAsked] decides one principal the same way every time. That
// is a FAULT of this node ([faulted]), its words in the log. It used to be a
// refusal made up on the spot, [ErrRefused] with no decision behind it, which
// every surface had to render as `unauthorized` naming no reason and no
// permission — a refusal nothing could lift, presented as one a grant would.
func (d WorkDeps) answerOnly(ctx context.Context, why string) *tools.Result {
	refused := d.notAsked(ctx, authz.ActionWorkComment)
	if refused == nil {
		fault := faulted(ctx, string(authz.ActionWorkComment), errAuthorityContradicted,
			string(authz.ActionWorkComment)+" didn't run: "+faultSaid+".")
		return &fault
	}
	refused.Output += " Without Worklet you may only answer a question on " +
		"this item that is addressed to you: " + why
	return refused
}
