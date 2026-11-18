package sandbox

import (
	"context"
	"time"
)

// THE JOB'S MAIN THREAD, READ ONCE BY THE COLLECTION. A relaunch carrying a
// round on in the job's own box continues the session copy the box holds, which
// its harness may compact before its first request (resumed.go); the event
// stream measures only the newest request's input ([Result.LastRequest]), so
// only the box's thread bounds what the harness counts: a Claude Code job's
// main thread, an OpenCode job's export its script left. So the collecting read
// reads that thread ONCE, on the box's host, before the take
// ([Coordinator.readMainThread]), and every reading asked of it ([ResumedCount],
// [NextRequest]) shares the one read ([MainThreadAsk]), written after the take
// in the collection's one write of the job's end ([EndRead.Count],
// [EndRead.Next]). A stop's read and a loss's ask nothing; a job not over is
// not read: its harness may still write.

// MainThreadReadWait bounds the read of a job's main thread at its collection
// ([Coordinator.readMainThread]): one wait every reading of the read shares.
//
// FIFTEEN SECONDS: the main thread at its bound, a quarter of a gibibyte read a
// line at a time with each line decoded, takes 1.9 to 2.0 s on two cores of
// the engine host (the executor's BenchmarkTheMainThreadReadAtItsBound), twice
// that loaded, and more on e2b, streamed through envd. A read past it answers
// [CountUnread], the safe side: a continuation told as one that may be cut
// off. It is held inside [BoxReadBudget] for a Claude Code or an OpenCode
// collection, before the take, so the take keeps its own.
const MainThreadReadWait = 15 * time.Second

// mainThreadReader is a [Runner] whose harness reads the main thread of the
// session a job's box ran: ONE READ, which every reading a collection asks of
// that thread shares ([MainThreadAsk]). Optional: a runner that is none reads
// nothing, and a count asked of it is [CountUnread].
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
	// Count asks for [ResumedCount], what the harness counts the session at
	// before its next request.
	Count bool
	// Next asks for [NextRequest], what the harness estimates the session's
	// next request at.
	Next bool
}

// MainThreadReading is what one read answered, a part per reading asked. The
// zero value is no reading.
type MainThreadReading struct {
	// Count is the session's count where it was asked: a bound, or a mark
	// saying why there is none. Nil where it was not asked.
	Count *ResumedCount
	// Provider is the provider of the reply an OpenCode count is anchored on
	// ([LaunchRecord.CountProvider]), "" for a Claude Code count and where no
	// reply anchored one.
	Provider string
	// Next is the harness's own estimate of the session's next request where
	// it was asked; zero where it was not, and where the read made none.
	Next NextRequest
}

// readMainThread reads run's job's main thread in box through runner, once, for
// every reading the collection asks: what the harness counts the copy at and
// estimates its next request at, for a Claude Code or an OpenCode job's
// collection ([BoxReadCollect]); a Claude Code job's whether or not its harness
// ended its own run, since the count decides the copy's skill guard too. A
// stop's read, a loss's and another harness's job ask nothing. A job not over
// ([capturePoint.final]), a runner that reads no thread, and an OpenCode job
// whose harness did not end its own run ([capturePoint.cutOff]; its export may
// lack what its copy holds) are [CountUnread] with no read. The count carries
// the least window the run reported ([Result.ReportedWindow]). The read is held
// within [Coordinator.mainThreadWait], armed here, past which the count is
// [CountUnread]; a marked count is logged.
func (c *Coordinator) readMainThread(ctx context.Context, run PendingRun, box Sandbox, runner Runner,
	read BoxRead, at capturePoint, result Result) MainThreadReading {

	if !asksMainThread(run, read) {
		return MainThreadReading{}
	}
	var (
		count    *ResumedCount
		next     NextRequest
		provider string
	)
	reader, reads := runner.(mainThreadReader)
	openCode := run.Harness == OpenCodeHarness
	switch {
	case !at.final:
		count = &ResumedCount{Mark: CountUnread}
	case openCode && at.cutOff:
		count = &ResumedCount{Mark: CountUnread}
	case !reads:
		count = &ResumedCount{Mark: CountUnread}
	default:
		readCtx, cancel := context.WithTimeout(ctx, c.mainThreadWait)
		reading := reader.ReadMainThread(readCtx, box, MainThreadAsk{Session: run.SessionRan(),
			Release: result.HarnessVersion, Count: true, Next: true})
		cancel()
		count, next, provider = reading.Count, reading.Next, reading.Provider
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
		case openCode && at.cutOff:
			why = "the job's harness did not end its own run, so its export may lack what its copy holds"
		case !reads:
			why = "the job's runner reads no main thread"
		}
		// THE OUTCOME IS THE HARNESS'S: a Claude Code continuation's first
		// request is judged, and an OpenCode one's is not yet, so its marked
		// count costs it the estimate of its next request alone.
		outcome := "a relaunch that continues this copy in its box is told it as a cut-off one"
		if openCode {
			outcome = "a relaunch that continues this copy in its box is held to its newest request's input"
		}
		log.InfoContext(ctx, "executor_session_count_unbounded", "turn_id", run.TurnID,
			"launch_id", run.LaunchID, "mark", string(count.Mark), "detail", why, "outcome", outcome)
	}
	return MainThreadReading{Count: count, Next: next, Provider: provider}
}

// unboundedBecause is why a read that answered mark holds no bound, as its log
// says: of a thread not read whole, what may have stopped the read; of a
// release the count's rules were not read in, or a saved compaction, that the
// thread was not read at all, each being found before it is opened
// (internal/sandbox/executor's mainthread.go); of any other mark, that the
// thread was read whole and its bytes cannot bound what the mark names.
func unboundedBecause(mark CountMark) string {
	switch mark {
	case CountUnread:
		return "the thread was not read whole: no session named it, a look at a saved compaction or the " +
			"thread's open failed, a line is no record, the thread or a line is past its bound, the read's " +
			"wait was spent, it holds no chained line, its newest restart's uuid does not read, or the read " +
			"answered nothing"
	case CountRelease:
		return "the release that ran the job is none the count's rules were read in, so the thread was not read"
	case CountUnanchored:
		return "the session was read whole, and holds no counted reply since its start or its newest " +
			"completed compaction, so its harness estimates what no export holds"
	case CountPrecompact:
		return "the session holds a saved compaction its harness swaps in where its threshold fires, so the " +
			"thread was not read"
	}
	return "the thread was read whole, and the mark names what its bytes cannot bound"
}

// asksMainThread reports whether read of run's box asks the job's main thread
// anything: a collection's, of a Claude Code or an OpenCode job. [BoxReadBudget]
// holds the read's wait for exactly these.
func asksMainThread(run PendingRun, read BoxRead) bool {
	return read == BoxReadCollect && (run.Harness == ClaudeCodeHarness || run.Harness == OpenCodeHarness)
}
