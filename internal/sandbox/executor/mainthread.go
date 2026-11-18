package executor

import (
	"context"
	"errors"
	"io"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// A CLAUDE CODE MAIN THREAD, WALKED ONCE BY EVERY READING ASKED OF IT.
//
// The take's context meter (measure.go) and the job's collection before it
// ([Runner.ReadMainThread]) each read a thread in ONE WALK ([walkThread]) that
// every reading asked is composed into ([threadReading]).
//
// IN THE BOX the read runs within internal/sandbox's MainThreadReadWait, cheapest
// check first, so a count marked early costs no read: a session id naming no
// file is unread; a release below [claudeCountFloor], or none, is
// [sandbox.CountRelease]; a saved compaction under either name ([claudePrecompact]), found by its size,
// never its body, is [sandbox.CountPrecompact]; only then is the thread opened
// (through envd on e2b) and walked.

// threadReading is one reader's account of a Claude Code thread, composed into
// the one walk ([walkThread]) and never reading the thread itself, so a thread
// is read once however many readings ask: the count ([resumedReading]), the
// meter's floor figure ([contextReading]), and any reading added later.
type threadReading interface {
	// line is the thread's next whole line that is one record ([threadLine]).
	line(l threadLine)
	// refused is the next whole line that is no record; what it costs is the
	// reading's to decide ([threadReader.Next]).
	refused(err *threadLineError)
	// end is how the walk ended: err nil at the member's end, torn whether a
	// last piece with no break was left unread; else what ended it
	// ([errThreadBound], the member reader's error, ctx's).
	end(torn bool, err error)
}

// walkThread reads r, one thread member, a whole line at a time within maxBytes
// ([newThreadReader], which checks ctx before each line), handing each line to
// every reading in order and each reading's end once. It goes on past a refused
// line, so a reading that tolerates one (the floor's figure) reads every line
// after it.
func walkThread(ctx context.Context, r io.Reader, maxBytes int64, readings ...threadReading) {
	reader := newThreadReader(r, maxBytes)
	ended := func(torn bool, err error) {
		for _, reading := range readings {
			reading.end(torn, err)
		}
	}
	for {
		l, err := reader.Next(ctx)
		var refused *threadLineError
		switch {
		case err == nil:
			for _, reading := range readings {
				reading.line(l)
			}
		case errors.As(err, &refused):
			for _, reading := range readings {
				reading.refused(refused)
			}
		case errors.Is(err, io.EOF):
			ended(reader.Torn(), nil)
			return
		default:
			ended(false, err)
			return
		}
	}
}

// mainThreadHarness is a harness that reads the main thread of the session a
// job's box ran ([Runner.ReadMainThread]): Claude Code's.
type mainThreadHarness interface {
	readMainThread(ctx context.Context, box sandbox.Sandbox, paths Paths,
		ask sandbox.MainThreadAsk) sandbox.MainThreadReading
}

// ReadMainThread reads the main thread of the session a job's box ran, once
// for every reading ask asks of it, through the runner's harness: zero for a
// harness that reads none, as OpenCode reads none.
func (r *Runner) ReadMainThread(ctx context.Context, box sandbox.Sandbox,
	ask sandbox.MainThreadAsk) sandbox.MainThreadReading {

	reads, ok := r.harness.(mainThreadHarness)
	if !ok {
		return sandbox.MainThreadReading{}
	}
	return reads.readMainThread(ctx, box, PathsFor(box), ask)
}

var _ mainThreadHarness = ClaudeCode{}

// readMainThread is Claude Code's read of the main thread of ask's session in
// box, in the order the file comment states.
func (ClaudeCode) readMainThread(ctx context.Context, box sandbox.Sandbox, paths Paths,
	ask sandbox.MainThreadAsk) sandbox.MainThreadReading {

	if !ask.Count {
		return sandbox.MainThreadReading{}
	}
	marked := func(mark sandbox.CountMark) sandbox.MainThreadReading {
		return sandbox.MainThreadReading{Count: &sandbox.ResumedCount{Mark: mark}}
	}
	thread, ok := claudeMainThread(ask.Session)
	if !ok {
		return marked(sandbox.CountUnread)
	}
	if ask.Release == "" || !releaseAtLeast(ask.Release, claudeCountFloor) {
		return marked(sandbox.CountRelease)
	}
	saved, _ := claudePrecompact(ask.Session)
	for _, name := range saved {
		// Its size, never its body: an empty file is one the harness cannot
		// read, so it holds no saved compaction.
		tail, err := box.ReadTail(ctx, paths.ClaudeConfigDir()+"/"+name, 0)
		switch {
		case err != nil:
			return marked(sandbox.CountUnread)
		case tail.Size > 0:
			return marked(sandbox.CountPrecompact)
		}
	}
	body, err := box.OpenFile(ctx, paths.ClaudeConfigDir()+"/"+thread)
	if err != nil {
		return marked(sandbox.CountUnread)
	}
	defer func() { _ = body.Close() }()
	var count resumedReading
	walkThread(ctx, body, threadPlainMax, &count)
	return sandbox.MainThreadReading{Count: count.count(0)}
}
