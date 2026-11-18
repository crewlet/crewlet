package sandbox

import (
	"context"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/events/types"
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
// and whether its session shows a hook's serve of a note taken ([HookReceipts]),
// never a second read of the thread.
//
// ITS COUNT IS WRITTEN WITH HOW THE HARNESS ENDED, after the take, in the
// collection's one write of the job's end ([EndRead.Count]). A stop's read and
// a loss's ask their served notes' receipts and no count: no relaunch carries
// their round on in place. A job not over is counted unread, its receipts read.

// MainThreadReadWait bounds the read of a job's main thread at its collection
// ([Coordinator.readMainThread]): the one wait every reading of the one read
// shares.
//
// FIFTEEN SECONDS: the main thread at its bound, its chain, count and receipts
// in one walk, reads in 2.78 to 2.85 s on two cores of the engine host (the
// executor's claudechain.go), twice that on a loaded host or streamed through
// envd, with a large thread's wait for its one slot inside it. A read past it
// answers [CountUnread] and decides no receipt, the safe side. It is held in
// the box read's budget ([BoxReadBudget]) before the take.
const MainThreadReadWait = 15 * time.Second

// mainThreadReader is a [Runner] whose harness reads the main thread of the
// session a job's box ran: ONE READ, which every reading a collection asks of
// that thread shares ([MainThreadAsk]). Optional: a runner that is none reads
// nothing, and a Claude Code count asked of it is [CountUnread].
type mainThreadReader interface {
	ReadMainThread(ctx context.Context, box Sandbox, ask MainThreadAsk) MainThreadReading
}

// MainThreadAsk is what a box read asks of its job's main thread.
type MainThreadAsk struct {
	// Session is the session the job ran, the box's word by the token it
	// dialled ([PendingRun.SessionRan]).
	Session string
	// Release is the harness release that ran it, as the read collected it
	// ([Result.HarnessVersion]): the release that wrote the thread and that
	// resumes it in place.
	Release string
	// Count asks for [ResumedCount], what Claude Code counts the session at
	// before its next request: a collection's alone.
	Count bool
	// Takes are the hooks' serves of a note to the job whose receipts are
	// asked ([askedTakes]), at most [MaxNoteServes] for each of the job's
	// notes and items; none asks no receipt.
	Takes []AskedTake
	// Ended says the job's harness ended its own run ([capturePoint.cutOff]):
	// only a thread so ended can prove a take absent.
	Ended bool
}

// AskedTake is one hook's serve whose receipt is asked: its take
// ([HookServed.Take]) and the Claude Code hook event it answered,
// "PostToolBatch" or "Stop".
type AskedTake struct {
	Take  string
	Event string
}

// The Claude Code hook events a serve answers, by its boundary
// ([HookServed.Boundary]).
const (
	hookEventToolBatch = "PostToolBatch"
	hookEventFinish    = "Stop"
)

// HookReceipts is what the job's session shows of the takes asked: each asked
// take in one list at most, or in none where the read cannot decide it, Why
// saying why where none is decided.
type HookReceipts struct {
	// Held are takes the session shows taken after its last restart.
	Held []string
	// Restarted are takes it shows taken only before its last restart.
	Restarted []string
	// Absent are takes its whole thread proves it never took.
	Absent []string
	// Why is why the read decided no take, for the log.
	Why string
}

// decided reports whether the receipts settle any take.
func (r HookReceipts) decided() bool {
	return len(r.Held) > 0 || len(r.Restarted) > 0 || len(r.Absent) > 0
}

// MainThreadReading is what one read answered, a part per reading asked. The
// zero value is no reading.
type MainThreadReading struct {
	// Count is the session's count where it was asked: a bound, or a mark
	// saying why there is none. Nil where it was not asked.
	Count *ResumedCount
	// Receipts is what the session shows of the takes asked.
	Receipts HookReceipts
}

// askedTakes is every hook's serve on run's copy whose receipt only its job's
// session settles: of each unread note or item for the job ([RunNote.IsFor]),
// its serves to the job that no loss struck or that its harness cancelled,
// once each, in the record's order. A serve at a boundary no Claude Code hook
// answers is asked nothing.
func askedTakes(run PendingRun) []AskedTake {
	var takes []AskedTake
	for _, n := range run.Steer.Notes {
		if n.Read != nil || !n.IsFor(run.LaunchID) {
			continue
		}
		for _, h := range n.Serves {
			event := hookEventOf(h.Boundary)
			if h.Launch != run.LaunchID || event == "" || (h.Lost != "" && h.Lost != ServeCancelled) ||
				slices.ContainsFunc(takes, func(t AskedTake) bool { return t.Take == h.Take }) {
				continue
			}
			takes = append(takes, AskedTake{Take: h.Take, Event: event})
		}
	}
	return takes
}

// hookEventOf is the Claude Code hook event a serve at boundary answered, ""
// for a boundary no hook answers.
func hookEventOf(boundary string) string {
	switch types.SteerBoundary(boundary) {
	case types.SteerBoundaryToolBatch:
		return hookEventToolBatch
	case types.SteerBoundaryFinish:
		return hookEventFinish
	}
	return ""
}

// readMainThread reads run's job's main thread in box through runner, once, for
// every reading read asks of it ([asksMainThread]): at a Claude Code job's
// collection what Claude Code counts the copy at, and at it, a stop or a loss
// the receipts of the hooks' serves the copy holds ([askedTakes]). A job not
// over is counted [CountUnread] with no count read ([capturePoint.final]), and
// so is a runner that reads no thread. The count carries the least window the
// run reported ([Result.ReportedWindow]).
//
// WITHIN [Coordinator.mainThreadWait], armed here, inside the read's context:
// past it, the read ends where it is, the count is [CountUnread] and no
// receipt is decided. Logged where the count is marked.
func (c *Coordinator) readMainThread(ctx context.Context, run PendingRun, box Sandbox, runner Runner,
	read BoxRead, at capturePoint, result Result) MainThreadReading {

	if !asksMainThread(run, read) {
		return MainThreadReading{}
	}
	counts := read == BoxReadCollect
	ask := MainThreadAsk{Session: run.SessionRan(), Release: result.HarnessVersion, Count: counts && at.final,
		Takes: askedTakes(run), Ended: !at.cutOff}
	var reading MainThreadReading
	reader, reads := runner.(mainThreadReader)
	if reads && (ask.Count || len(ask.Takes) > 0) {
		readCtx, cancel := context.WithTimeout(ctx, c.mainThreadWait)
		reading = reader.ReadMainThread(readCtx, box, ask)
		cancel()
	}
	if !counts {
		reading.Count = nil
		return reading
	}
	count := reading.Count
	if !ask.Count || count == nil {
		count = &ResumedCount{Mark: CountUnread}
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
	reading.Count = count
	return reading
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
// anything: a Claude Code job's collection, and its stop's or loss's read where
// its copy holds a serve to the job ([PendingRun.asksReceipts]).
// [BoxReadBudget] holds the read's wait for exactly these.
func asksMainThread(run PendingRun, read BoxRead) bool {
	return run.Harness == ClaudeCodeHarness && (read == BoxReadCollect || run.asksReceipts())
}
