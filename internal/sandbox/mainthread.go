package sandbox

import (
	"context"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/events/types"
)

// THE JOB'S MAIN THREAD, READ ONCE. A relaunch carrying a round on in the job's
// own box continues the copy the box holds, which its harness may compact
// before its first request (resumed.go); the event stream measures only the
// newest request's input ([Result.LastRequest]), so only the box's thread (a
// Claude Code job's main thread, an OpenCode job's export) bounds its count.
// The read of a job's box reads it ONCE, on the box's host, before the take
// ([Coordinator.readMainThread]), every reading sharing the read
// ([MainThreadAsk]): the count and next request, written after the take in the
// collection's one write of the job's end ([EndRead]), and a Claude Code
// hook's serves' receipts ([HookReceipts]). A stop's and a loss's read ask
// receipts and no count: no relaunch carries their round on in place. A job
// not over is counted unread, its receipts read.

// MainThreadReadWait bounds the read of a job's main thread at its collection
// ([Coordinator.readMainThread]): one wait every reading of the read shares.
//
// FIFTEEN SECONDS: the main thread at its bound, its chain, count and receipts
// in one walk, reads in 2.2 to 2.4 s on two dedicated cores (the executor's
// claudechain.go), twice that loaded or streamed through envd. A read past its
// base waits for the executor's one large slot INSIDE this wait: 9.6 s at worst
// behind such a pass. A pass keeping its chain's records past the base over the
// bound's slowest lines, 16.4 to 17.5 s, is cut here holding it, so one behind
// it ends unread too (the executor's package doc, "A collection's read of a
// thread"). A read past it answers [CountUnread] and decides no receipt, the
// safe side. It is held in [BoxReadBudget] before the take.
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
	// Count asks for [ResumedCount], what the harness counts the session at
	// before its next request: a collection's alone.
	Count bool
	// Next asks for [NextRequest], what the harness estimates the session's
	// next request at.
	Next bool
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
// saying why where none is decided. A thread that compacted or rooted again
// decides none (executor's package doc, "A hook's note").
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
	// Provider is the provider of the reply an OpenCode count is anchored on
	// ([LaunchRecord.CountProvider]), "" for a Claude Code count and where no
	// reply anchored one.
	Provider string
	// Next is the harness's own estimate of the session's next request where
	// it was asked; zero where it was not, and where the read made none.
	Next NextRequest
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
// every reading read asks of it ([asksMainThread]): at a Claude Code or an
// OpenCode job's collection what its harness counts the copy at and estimates
// its next request at, and at a Claude Code job's collection, stop or loss the
// receipts of the hooks' serves the copy holds ([askedTakes]). A job not over,
// a runner that reads no thread and an OpenCode job whose harness did not end
// its own run (its export may lack what its copy holds) are counted
// [CountUnread] with no count read. The count carries the least window the run
// reported ([Result.ReportedWindow]). Held within [Coordinator.mainThreadWait],
// armed here: past it the read ends where it is, the count is [CountUnread]
// and no receipt is decided; a marked count is logged.
func (c *Coordinator) readMainThread(ctx context.Context, run PendingRun, box Sandbox, runner Runner,
	read BoxRead, at capturePoint, result Result) MainThreadReading {

	if !asksMainThread(run, read) {
		return MainThreadReading{}
	}
	counts, openCode := read == BoxReadCollect, run.Harness == OpenCodeHarness
	counted := counts && at.final && (!openCode || !at.cutOff)
	ask := MainThreadAsk{Session: run.SessionRan(), Release: result.HarnessVersion, Count: counted,
		Next: counted, Ended: !at.cutOff}
	if asksHookReceipts(run) {
		ask.Takes = askedTakes(run)
	}
	var reading MainThreadReading
	reader, reads := runner.(mainThreadReader)
	if reads && (ask.Count || len(ask.Takes) > 0) {
		readCtx, cancel := context.WithTimeout(ctx, c.mainThreadWait)
		reading = reader.ReadMainThread(readCtx, box, ask)
		cancel()
	}
	if !counts {
		reading.Count, reading.Provider, reading.Next = nil, "", NextRequest{}
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
		case openCode && at.cutOff:
			why = "the job's harness did not end its own run, so its export may lack what its copy holds"
		case !reads:
			why = "the job's runner reads no main thread"
		}
		// THE SAME OUTCOME FOR EITHER HARNESS: a marked count bounds no first
		// request, so the copy is told as a cut-off one; an OpenCode one's
		// floor, which only its count would bound, reads its newest
		// request's input as well.
		outcome := "a relaunch that continues this copy in its box is told it as a cut-off one"
		if openCode {
			outcome += ", and is held to its newest request's input"
		}
		log.InfoContext(ctx, "executor_session_count_unbounded", "turn_id", run.TurnID,
			"launch_id", run.LaunchID, "mark", string(count.Mark), "detail", why, "outcome", outcome)
	}
	reading.Count = count
	return reading
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
// anything: a Claude Code or an OpenCode job's collection, and any read of a
// box whose copy holds a serve its thread settles ([asksHookReceipts]).
// [BoxReadBudget] holds the read's wait for exactly these.
func asksMainThread(run PendingRun, read BoxRead) bool {
	collects := read == BoxReadCollect && (run.Harness == ClaudeCodeHarness || run.Harness == OpenCodeHarness)
	return collects || asksHookReceipts(run)
}

// asksHookReceipts reports whether the read of run's box asks its job's
// session for receipts: a Claude Code job's copy that holds a serve to the job
// ([PendingRun.asksReceipts]). Only Claude Code's hooks serve a take its
// thread shows, so an OpenCode job's read asks none, nor holds a verdict's
// share ([BoxReadBudget]).
func asksHookReceipts(run PendingRun) bool {
	return run.Harness == ClaudeCodeHarness && run.asksReceipts()
}
