package gitlet

import (
	"context"
	"errors"
	"time"
)

// A GIT READ IS SERVED UNTIL ITS ANSWER IS DUE, AND NOT A MOMENT LONGER.
//
// An answer to a [GitQuery] that arrives after the asker's attempt ended is
// heard as nothing, and the asker takes a merely busy node for gone: demotes
// it for half a minute and sends its readers to a runner-up that downloads its
// own copy. So the read runs under one context ending [ReplyMargin] before its
// answer is due ([AnswerBy]), and EVERY wait takes it (the copy's sync, the node's
// share, the read lane's queue, each process): a per-wait cut fails, as
// gitproc's queue and a timer set before queuing run past it. A wait it ends
// answers [ErrGitUnready] ([AnswerDue]), naming what it waited on, while the
// asker still listens, never the context's error, which reads as the
// repository's answer. An in-process read gets the same cut.

// ErrAnswerDue is the cause the context a git read is served under ends with
// at the instant its answer is due: the end of its asker's wait less
// [ReplyMargin] ([AnswerBy]). A context ended by anything else — its caller
// gone, an earlier deadline of its own — carries that end's cause instead.
var ErrAnswerDue = errors.New("gitlet: the read's answer is due")

// AnswerBy is ctx for serving a git read whose asker waits until deadline, on
// this process's runtime clock: a peer's wait counted from its request's
// arrival (signedask.Due), or an in-process attempt's own end. It ends
// [ReplyMargin] before deadline, its cause [ErrAnswerDue], so the read answers
// while its asker still listens. A zero deadline is no asker waiting on a
// wire: ctx comes back with nothing cut. cancel releases it, as
// context.WithDeadline's does.
func AnswerBy(ctx context.Context, deadline time.Time) (context.Context, context.CancelFunc) {
	if deadline.IsZero() {
		return context.WithCancel(ctx)
	}
	return context.WithDeadlineCause(ctx, deadline.Add(-ReplyMargin), ErrAnswerDue)
}

// AnswerDue reports whether ctx has ended because the read it serves fell due
// ([AnswerBy]) — its asker still listening for a reply's margin, so the read
// answers [ErrGitUnready] — rather than because its caller went away, which
// nobody waits to hear about.
func AnswerDue(ctx context.Context) bool {
	return ctx.Err() != nil && errors.Is(context.Cause(ctx), ErrAnswerDue)
}
