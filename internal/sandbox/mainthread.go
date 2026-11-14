package sandbox

import (
	"context"
	"time"
)

// THE JOB'S MAIN THREAD, READ ONCE BY THE COLLECTION.
//
// A relaunch that carries a round on in the job's own box continues the copy of
// the session that box holds, and Claude Code may compact that copy before the
// relaunch's first request (resumed.go). The run's event stream measures the
// input of the newest request alone ([Result.LastRequest]), never the reply's
// output nor what came after it, so only the box's thread bounds what the
// harness counts there. So the read that collects a job reads the job's main
// thread ONCE, on the box's host, before the take ([Coordinator.readMainThread]),
// and every reading the collection asks of that thread shares the one read
// ([MainThreadAsk]): what Claude Code counts the copy at ([ResumedCount]) now,
// and whatever later reading of the thread joins it, never a second read of
// the thread.
//
// ITS ANSWER IS WRITTEN WITH HOW THE HARNESS ENDED, after the take, in the
// collection's one write of the job's end ([EndRead.Count]), never in a write of
// its own. A stop's read and a loss's ask nothing: no relaunch carries their
// round on in place. A job not over is not read: its harness may still write.

// MainThreadReadWait bounds the read of a job's main thread at its collection
// ([Coordinator.readMainThread]): the one wait every reading of the one read
// shares.
//
// FIFTEEN SECONDS, the ground the hook receipt's read of the same thread
// stated: the main thread at its bound, a quarter of a gibibyte read a whole
// line at a time with each line's head decoded, and on e2b streamed through
// envd, on a loaded host. A read past it answers [CountUnread], the safe side:
// a continuation told as one that may be cut off. It is held inside the box
// read's budget ([BoxReadBudget]) for a Claude Code collection, before the
// take, so the take keeps its own.
const MainThreadReadWait = 15 * time.Second

// mainThreadReader is a [Runner] whose harness reads the main thread of the
// session a job's box ran: ONE READ, which every reading a collection asks of
// that thread shares ([MainThreadAsk]). Optional: a runner that is none reads
// nothing, and a Claude Code count asked of it is [CountUnread].
type mainThreadReader interface {
	ReadMainThread(ctx context.Context, box Sandbox, ask MainThreadAsk) MainThreadReading
}

// MainThreadAsk is what a collection asks of its job's main thread.
type MainThreadAsk struct {
	// Session is the session the job ran, the box's word by the token it
	// dialled ([PendingRun.SessionRan]).
	Session string
	// Release is the harness release that ran it, as the collection read it
	// ([Result.HarnessVersion]): the release that wrote the thread and that
	// resumes it in place.
	Release string
	// Count asks for [ResumedCount], what Claude Code counts the session at
	// before its next request.
	Count bool
}

// MainThreadReading is what one read answered, a part per reading asked. The
// zero value is no reading.
type MainThreadReading struct {
	// Count is the session's count where it was asked: a bound, or a mark
	// saying why there is none. Nil where it was not asked.
	Count *ResumedCount
}

// readMainThread reads run's job's main thread in box through runner, once, for
// every reading the collection asks of it: what Claude Code counts the copy at,
// for a Claude Code job's collection ([BoxReadCollect]), whether or not its
// harness ended its own run, since the count decides the copy's skill guard
// too. Nothing where nothing is asked: a stop's read, a loss's, and another
// harness's job. A job not over is [CountUnread] with no read
// ([capturePoint.final]), and so is a runner that reads no thread. The count
// carries the least window the run reported ([Result.ReportedWindow]).
//
// WITHIN [Coordinator.mainThreadWait], armed here, inside the read's context:
// past it, the read ends where it is, and the count is [CountUnread]. Logged
// where the count is marked.
func (c *Coordinator) readMainThread(ctx context.Context, run PendingRun, box Sandbox, runner Runner,
	read BoxRead, at capturePoint, result Result) MainThreadReading {

	if !asksMainThread(run, read) {
		return MainThreadReading{}
	}
	var count *ResumedCount
	reader, reads := runner.(mainThreadReader)
	switch {
	case !at.final:
		count = &ResumedCount{Mark: CountUnread}
	case !reads:
		count = &ResumedCount{Mark: CountUnread}
	default:
		readCtx, cancel := context.WithTimeout(ctx, c.mainThreadWait)
		count = reader.ReadMainThread(readCtx, box, MainThreadAsk{Session: run.SessionRan(),
			Release: result.HarnessVersion, Count: true}).Count
		cancel()
		if count == nil {
			count = &ResumedCount{Mark: CountUnread}
		}
	}
	count.Window = result.ReportedWindow
	if count.Mark != "" {
		why := unboundedBecause(count.Mark)
		switch {
		case !at.final:
			why = "the job was not over as its box was read, so its harness may still write its thread"
		case !reads:
			why = "the job's runner reads no main thread"
		}
		log.InfoContext(ctx, "executor_session_count_unbounded", "turn_id", run.TurnID,
			"launch_id", run.LaunchID, "mark", string(count.Mark), "detail", why,
			"outcome", "a relaunch that continues this copy in its box is told it as a cut-off one")
	}
	return MainThreadReading{Count: count}
}

// unboundedBecause is why a read of a job's main thread that answered mark holds
// no bound, as its log says it: of a thread not read whole, what may have
// stopped the read; of a release the count's rules were not read in, or a saved
// compaction, that the thread was not read at all, since each is found before
// it is opened (internal/sandbox/executor's mainthread.go); and of every other
// mark, that the thread was read whole and its bytes cannot bound what the mark
// names.
func unboundedBecause(mark CountMark) string {
	switch mark {
	case CountUnread:
		return "the thread was not read whole: no session named it, a look at a saved compaction or the " +
			"thread's open failed, a line is no record, the thread or a line is past its bound, the read's " +
			"wait was spent, it holds no chained line, its newest restart's uuid does not read, or the read " +
			"answered nothing"
	case CountRelease:
		return "the release that ran the job is none the count's rules were read in, so the thread was not read"
	case CountPrecompact:
		return "the session holds a saved compaction its harness swaps in where its threshold fires, so the " +
			"thread was not read"
	}
	return "the thread was read whole, and the mark names what its bytes cannot bound"
}

// asksMainThread reports whether read of run's box asks the job's main thread
// anything: a collection's, of a Claude Code job. [BoxReadBudget] holds the
// read's wait for exactly these.
func asksMainThread(run PendingRun, read BoxRead) bool {
	return read == BoxReadCollect && run.Harness == ClaudeCodeHarness
}
