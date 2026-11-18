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
// Somebody kept off the tracker can still be asked things on a work item, and
// refused the item they could neither read nor answer the ask. So
// `get_work_item` and `comment_on_work_item` are decided by
// [authz.ClassAppOrAsked]: a tracker holder, OR the person an OPEN ask on the
// item names, whose comment is admitted only as THE ANSWER to that ask. The
// asks are a stored row, so the decision is taken twice: [WorkDeps.itemGate]
// on the unread item ([authz.Object.Unresolved]), [WorkDeps.askedOn] on the
// row.

// itemGate is the first half: the decision on an item nobody has read yet.
// It answers true for a caller the table admits outright (a tracker holder,
// or admin through it), false with no refusal for one who may yet be admitted
// as the person an open ask names ([authz.ErrUnresolved]), and otherwise the
// refusal to return: a refusal on any other ground, or a node that could not
// decide.
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
// handle, and decide again on the row. It answers the item, read with no
// parts, or the refusal to return.
//
// Whether the caller is asked AT ALL is settled first ([worklet.AskedOn]),
// because [worklet.Reader.Thread] refuses an `answers` naming somebody else's
// question by saying whose it was: told to a caller nobody asked, that reads
// the thread out one refusal at a time.
//
// self derives the id the caller's own comment will carry ([commentID]), nil
// for a read: it still admits an answer retried after it closed its ask.
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
		// A probe that could not read has found neither an ask nor its
		// absence: a refusal would send somebody who WAS asked to go and
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

// notAsked is the refusal of somebody without Worklet on an item no open ask
// names them on, and on an item that is not there at all: told apart, the two
// would let a caller kept off the board probe which keys exist. It is the
// table's own decision on the bare kind, so it names the grant that would
// admit ([authz.Decision.Grants]) in every surface's words. nil means the
// table admitted the bare kind after all (an authorizer disagreeing with
// itself): the caller goes on as a holder and the ordinary read answers.
func (d WorkDeps) notAsked(ctx context.Context, action authz.Action) *tools.Result {
	return d.mayWrite(ctx, action, authz.Object{Kind: authz.KindTask})
}

// errAuthorityContradicted is an authority decision that admitted, on the bare
// kind, a caller it had just admitted only as the person an ask names — two
// answers to one question about one principal within one call.
var errAuthorityContradicted = errors.New("builtin: the authority decision " +
	"admitted the tracker to a caller it had just admitted only as the person " +
	"an ask names")

// answerOnly is [WorkDeps.notAsked] for a comment from somebody admitted only
// as the person an ask names that is not an answer to it, saying what they may
// do instead, so the refusal does not leave them guessing which part failed.
//
// An authorizer admitting the bare kind here contradicts itself
// ([authz.ClassAppOrAsked] decides one principal the same way every time), so
// that is a FAULT of this node ([faulted]), not a refusal a grant could lift
// (TestAnAuthorizerThatContradictsItselfIsAFault).
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
