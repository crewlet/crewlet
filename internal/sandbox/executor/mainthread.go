package executor

import (
	"context"
	"errors"
	"io"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// A CLAUDE CODE MAIN THREAD, WALKED ONCE BY EVERY READING ASKED OF IT.
//
// Two places read a session's main thread whole: the take's context meter, over
// the archive it keeps (measure.go), and the read that collects a job, over the
// copy its box holds (internal/sandbox's mainthread.go,
// [Runner.ReadMainThread]). Each is ONE WALK of the thread's whole lines
// through the line layer ([walkThread]), into which every reading the caller
// asks is composed ([threadReading]): the count of what Claude Code counts the
// session at ([resumedReading]), and at the take the floor's figure, the last
// request's input ([contextReading]). A reading never reads the thread itself,
// so a thread is read once whatever readings ask of it, and a reading a later
// change adds joins the same walk rather than read the thread again.
//
// IN THE BOX, the read is the collection's, before the take, within the one wait
// every reading of it shares (internal/sandbox's MainThreadReadWait), and in this
// order, cheapest first, so a count marked before the walk costs no read:
// a session whose id names no file is unread; a release the count's rules were
// not read in is [sandbox.CountRelease]; a saved compaction under either name
// Claude Code reads one by ([claudePrecompact]), found by a look at each name's
// size and never a read of its body, is [sandbox.CountPrecompact]; and only
// then is the thread opened, streamed (through envd on e2b) and walked.

// threadReading is one reader's account of a Claude Code thread, composed into
// the ONE walk of its whole lines ([walkThread]): handed every whole line in
// file order, each line the layer refuses, and then how the walk ended. A
// reading never reads the thread itself, so a thread is read once whatever
// readings ask of it: the count ([resumedReading]) and the meter's floor figure
// ([contextReading]) now, and any later reading of the thread when it joins.
type threadReading interface {
	// line is the thread's next whole line that is one record. Its handouts
	// are valid only while line runs ([threadLine]).
	line(l threadLine)
	// refused is the thread's next whole line that is no record: what it
	// costs the reading is the reading's to decide (the layer's rule,
	// [threadReader.Next]).
	refused(err *threadLineError)
	// end is how the walk ended: err nil at the member's end, torn whether a
	// last piece with no break was left unread; else what ended it
	// ([errThreadBound], the member reader's error, ctx's).
	end(torn bool, err error)
}

// walkThread reads r, one thread member, a whole line at a time through the
// layer's reader ([newThreadReader]) within maxBytes, handing each line to
// every reading in the order given, ctx checked between lines, and each
// reading's end once. It goes on past a refused line, the reader staying
// usable, so a reading that reads past one (the floor's figure) reads the
// thread as it did before the layer.
func walkThread(ctx context.Context, r io.Reader, maxBytes int64, readings ...threadReading) {
	reader := newThreadReader(r, maxBytes)
	ended := func(torn bool, err error) {
		for _, reading := range readings {
			reading.end(torn, err)
		}
	}
	for {
		if err := ctx.Err(); err != nil {
			ended(false, err)
			return
		}
		l, err := reader.Next()
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

// ReadMainThread reads the main thread of the session a job's box ran, once,
// for every reading ask asks of it (internal/sandbox's mainthread.go), through
// the runner's harness: zero for a harness that reads none, as OpenCode's
// reads none.
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
// box, in the order the file comment states: the session's id, the release,
// the saved compaction's two names, then ONE walk of the thread for every
// reading asked.
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
		// A LOOK AT ITS SIZE, never a read of its body: a file of no bytes is
		// one the harness cannot read, so it holds no saved compaction.
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
