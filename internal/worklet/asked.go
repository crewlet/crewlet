package worklet

import (
	"context"
	"errors"

	"github.com/crewlet/crewlet/internal/statelog"
)

// THE ASKED PROBE: whether somebody without Worklet may read a work item
// and answer on it, because an open question on it is addressed to them.
//
// A company that keeps somebody off the tracker still asks them things, filed
// on a work item; refused the item or the comment, they cannot read or answer
// it, and the ask sits open for ever. So the authority table's `asked` arm
// (internal/authz, ClassAppOrAsked) admits exactly the person an OPEN ask on
// the item names, and this is the one read that says whether one does.

// ThreadAsker is the one tracker read the probe needs. [*Reader] satisfies it,
// and so does every seam that routes a tracker read to a data node — the
// estate router's work seam and the builtins' reader — so a surface asks the
// probe of whatever reader it already holds, on a data node or not.
type ThreadAsker interface {
	Thread(ctx context.Context, q ThreadQuery, fresh statelog.Freshness) (ResolvedThread, error)
}

// AskedOn reports whether an OPEN ask on task (an id) is addressed to handle,
// or whether self — the id of the remark handle is about to post, empty on a
// read — is already on task and answered one: a retried call whose answer
// landed is answered from its row ([ThreadQuery.Comment]), not refused.
//
// It is [Reader.Thread]'s inference with handle as author and self as id, the
// read a comment's write makes, so "may this person answer here" and "would
// their comment answer something" are one rule. An ask to somebody else, or
// answered, resolved or removed, is no ask. Several open asks
// ([*ErrAmbiguousAnswer]) answer true: the person is MORE asked, and the write
// makes them name one. Any other error returns false, for the caller to treat
// as the refusal's unknown, not as "not asked".
func AskedOn(ctx context.Context, threads ThreadAsker, task, handle, self string,
	fresh statelog.Freshness) (bool, error) {

	if task == "" || handle == "" {
		// NOBODY IS ASKED ON NOTHING: an empty author would make the inference
		// skip, so an empty handle is a plain no without a read.
		return false, nil
	}
	thread, err := threads.Thread(ctx, ThreadQuery{Task: task, Author: handle,
		Comment: self}, fresh)
	if err != nil {
		var ambiguous *ErrAmbiguousAnswer
		if errors.As(err, &ambiguous) {
			return true, nil
		}
		return false, err
	}
	return thread.Answers != "", nil
}
