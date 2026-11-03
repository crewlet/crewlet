package code

import (
	"context"
	"errors"
	"time"
)

// A GIT READ IS SERVED UNTIL ITS ANSWER IS DUE, AND NOT A MOMENT LONGER.
//
// A git node answering a read by object id ([GitQuery]) for an asker that
// waits until a deadline must SEND its answer while the asker still listens:
// one that arrives after the asker's attempt has ended is heard as nothing,
// and an asker that heard nothing for its whole attempt takes the node for
// gone — asks it last for every operation for half a minute and tells the
// fleet's placements it went unanswered — while the node was only busy, and
// every repository it is home to sends its readers to a runner-up that then
// downloads a copy of its own. So the read runs under a context that ends
// [ReplyMargin] before the asker's deadline ([AnswerBy]), and EVERY wait it
// makes takes that context: its copy's sync, its unit of the node's share, the
// read lane's queue for each of its processes, and each process itself. A
// wait that context ends is answered [ErrGitUnready], naming what the read
// was waiting on, while the asker still listens ([AnswerDue]) — never the
// context's own error, which the asker would take for the repository's
// answer, and never a refusal sent too late to be heard.
//
// One context ends every wait, rather than each wait cut to the asker's
// deadline less the margin, because a wait that is not told the cut cannot
// keep it: the read lane's queue and a process's own timer are gitproc's, and
// a process's deadline computed before it queued for its slot runs past the
// cut by however long it queued.
//
// THE SAME CUT HOLDS A READ ANSWERED IN PROCESS: a git node's own seats' reads
// are given the attempt a remote asker's are, ending the same margin before
// it, so what a read is answered does not depend on which node asked it.

// ErrAnswerDue is the cause the context a git read is served under ends with
// at the instant its answer is due: its asker's deadline less [ReplyMargin]
// ([AnswerBy]). A context ended by anything else — its caller gone, an earlier
// deadline of its own — carries that end's cause instead.
var ErrAnswerDue = errors.New("code: the read's answer is due")

// AnswerBy is ctx for serving a git read whose asker waits until deadline: it
// ends [ReplyMargin] before deadline, its cause [ErrAnswerDue], so the read
// answers while its asker still listens. A zero deadline is no asker waiting
// on a wire: ctx comes back with nothing cut. cancel releases it, as
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
