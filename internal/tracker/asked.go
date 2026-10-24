package tracker

import (
	"context"
	"errors"

	"github.com/crewlet/crewlet/internal/statelog"
)

// THE ASKED PROBE: whether somebody without the tracker may read a work item
// and answer on it, because an open question on it is addressed to them.
//
// A company that keeps somebody off the tracker still asks them things — a
// decision only legal can make, a figure only finance has — and the ask is
// filed on a work item. Refused the item, the person cannot read what they
// were asked; refused the comment, they cannot answer it; and the ask sits
// open for ever while its author waits. So the authority table's `asked` arm
// (internal/authz, ClassAppOrAsked) admits exactly the person an OPEN ask on
// the item names, and this is the one read that says whether one does.

// ThreadAsker is the one tracker read the probe needs. [*Reader] satisfies it,
// and so does every seam that routes a tracker read to a data node — the
// estate router's work seam and the builtins' reader — so a surface asks the
// probe of whatever reader it already holds, on a data node or not.
type ThreadAsker interface {
	Thread(ctx context.Context, q ThreadQuery, fresh statelog.Freshness) (ResolvedThread, error)
}

// AskedOn reports whether an OPEN ask on task (an id) is addressed to handle.
//
// # One read, and it is the answer's own
//
// It asks [Reader.Thread] with the handle as the comment's author and nothing
// else, which is the read a comment's write makes to find the question it
// answers: it INFERS the one open ask addressed to that author and checks it
// is still answerable by them. So "may this person answer here" and "would
// this person's comment answer something" are one rule rather than two that
// could drift — and an ask somebody else was asked, one already answered, one
// resolved and one removed are each no ask at all.
//
// # Open asks only
//
// Once a person has answered, the item is the tracker's again: they read it
// with the tracker or not at all. Answering what you were asked is the whole
// of what the arm grants, and a closed question asks nothing.
//
// # Several open asks are asked
//
// Several open asks naming the person make the inference refuse
// ([*ErrAmbiguousAnswer]) — a comment cannot tell which it answers — but they
// make the person MORE asked, not less, so that refusal answers true here. The
// write then makes them name the ask, as it makes everybody.
//
// Any other error is unknown and returned with false: a probe that could not
// read the item has not found an ask, and a caller decides on that as the
// refusal's unknown rather than as "not asked".
func AskedOn(ctx context.Context, threads ThreadAsker, task, handle string,
	fresh statelog.Freshness) (bool, error) {

	if task == "" || handle == "" {
		// NOBODY IS ASKED ON NOTHING, and an empty author would make the
		// inference skip — so an empty handle is a plain no rather than a
		// read that asks the store a question it cannot answer.
		return false, nil
	}
	thread, err := threads.Thread(ctx, ThreadQuery{Task: task, Author: handle}, fresh)
	if err != nil {
		var ambiguous *ErrAmbiguousAnswer
		if errors.As(err, &ambiguous) {
			return true, nil
		}
		return false, err
	}
	return thread.Answers != "", nil
}
