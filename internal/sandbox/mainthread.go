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
// newest request's input ([Result.LastRequest]), so only the box's thread
// bounds its count: Claude Code's, Codex's, OpenCode's, Gemini CLI's, Qwen
// Code's. It is read ONCE, on the box's host, before the take, every reading
// sharing the read ([Coordinator.readMainThread], [MainThreadAsk]): the count
// and next request, written after the take in the collection's one write of
// the job's end ([EndRead]), and a hook's serves' receipts ([TakeReceipts]):
// Claude Code's, Gemini CLI's, Grok Build's, Muse Code's. A stop's and a
// loss's read ask receipts and no count: no relaunch carries their round on in
// place. A job not over is counted unread, its receipts read.

// MainThreadReadWait bounds the read of a job's main thread at its collection
// ([Coordinator.readMainThread]): one wait every reading of the read shares.
//
// TWENTY-SEVEN SECONDS: the slowest shape a session writes, a tool result of
// text blocks to the plaintext's bound, read with the count and the next
// request's estimate in one walk, takes 9.45 to 9.49 s on a dedicated two-core
// slot (the executor's BenchmarkTheMainThreadReadAtItsBound; no slower under
// the engine's memory limit), twice that loaded, and the wait
// keeps holdingsReadWait's headroom over that, about 1.4x. Lines no release
// writes run past it loaded and are cut, the safe side (the executor's package
// doc, "A collection's read of a thread"): a read past it answers [CountUnread]
// and decides no receipt. It is held in [BoxReadBudget] before the take.
const MainThreadReadWait = 27 * time.Second

// mainThreadReader is a [Runner] whose harness reads the main thread of the
// session a job's box ran: ONE READ, which every reading a collection asks of
// that thread shares ([MainThreadAsk]), and what that read settles
// ([ThreadTraits]). Optional: a runner that is none reads nothing.
type mainThreadReader interface {
	ReadMainThread(ctx context.Context, box Sandbox, ask MainThreadAsk) MainThreadReading
	ThreadTraits() ThreadTraits
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
	// asked ([askedTakes]), at most [RunNote.MaxServes] for each of the job's
	// notes and items; none asks no receipt.
	Takes []AskedTake
	// Served is every serve of a note to the job in the record's order, its
	// read notes' included and marked ([servedTakes]): Takes, each in its
	// place among those. A runner that matches its record by text in serve
	// order reads it, so a read note keeps its place and its record settles
	// no later take of the same words; nothing is asked of a read note's
	// serve. Nil where the job has no read note among its serves, Takes being
	// the order then.
	Served []AskedTake
	// Ended says the job's harness ended its own run ([capturePoint.cutOff]):
	// only a thread so ended can prove a take absent.
	Ended bool
	// Summary asks for the newest compaction summary on the job's main
	// thread ([ThreadSummary]), scrubbed of Scrub, the values the job's
	// capture is scrubbed of ([Coordinator.scrubValues]): a summary is read
	// only where it can be kept, and never leaves the read unscrubbed.
	Summary bool
	Scrub   []string
}

// AskedTake is one hook's serve whose receipt is asked: its take
// ([HookServed.Take]), the boundary it was served at ([HookServed.Boundary]),
// which the runner reads as its harness's own event, and a poll serve's item
// digest ([HookServed.Digest]).
type AskedTake struct {
	Take     string
	Boundary types.SteerBoundary
	Digest   string
	// Read marks a serve of a note already read, in [MainThreadAsk.Served]
	// alone: no receipt is asked of it.
	Read bool
}

// TakeReceipts is what the job's session shows of the takes asked: each asked
// take in one list at most, or in none where the read cannot decide it, Why
// saying why where none is decided. A thread that compacted or rooted again
// decides none (executor's package doc, "A hook's note").
type TakeReceipts struct {
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
func (r TakeReceipts) decided() bool {
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
	Receipts TakeReceipts
	// Summary is the thread's newest compaction summary where it was asked.
	Summary ThreadSummary
}

// ThreadSummary is the newest compaction summary on a job's main thread, as a
// box read keeps it: scrubbed of [MainThreadAsk.Scrub], redacted, and held to
// executorkeep.SummaryMaxBytes by its end (executorkeep.SummaryTail). The
// zero value is a thread holding none, or a read that asked none.
type ThreadSummary struct {
	// Text is the summary as kept, "" where there is none: the harness's
	// model wrote it from what others wrote.
	Text string `untrusted:"fence"`
	// Bytes is the whole summary's size, scrubbed and redacted, before the
	// bound; Cut says Text is its last part.
	Bytes int64
	Cut   bool
	// Unread is why the read could not say whether the thread holds one,
	// "" where it could.
	Unread string
}

// askedTakes is every hook's serve on run's copy whose receipt only its job's
// session settles: of each unread note or item for the job ([RunNote.IsFor]),
// its serves to the job that no loss struck or that its harness cancelled,
// once each, in the record's order. A serve the job's record cannot settle
// ([askedBoundary]) is asked nothing.
func askedTakes(run PendingRun) []AskedTake { return launchServes(run, false) }

// servedTakes is [askedTakes] with the serves of the job's read notes in their
// places ([MainThreadAsk.Served]), nil where it adds none.
func servedTakes(run PendingRun) []AskedTake {
	served := launchServes(run, true)
	if len(served) == len(askedTakes(run)) {
		return nil
	}
	return served
}

// launchServes is the serves [askedTakes] names, those of read notes too
// where withRead. A READ NOTE'S SERVE IS KEPT WHATEVER READ IT, a brief's or
// the review's as much as this job's own confirm: the serve went to this job,
// whose record may hold it, and a row it holds left out would match a later
// unread take of the same words, holding that take falsely.
func launchServes(run PendingRun, withRead bool) []AskedTake {
	var takes []AskedTake
	for _, n := range run.Steer.Notes {
		if (n.Read != nil && !withRead) || !n.IsFor(run.LaunchID) {
			continue
		}
		for _, h := range n.Serves {
			if h.Launch != run.LaunchID || !askedBoundary(h.Boundary) || (h.Lost != "" && h.Lost != ServeCancelled) ||
				slices.ContainsFunc(takes, func(t AskedTake) bool { return t.Take == h.Take }) {
				continue
			}
			takes = append(takes, AskedTake{Take: h.Take, Boundary: types.SteerBoundary(h.Boundary), Digest: h.Digest,
				Read: n.Read != nil})
		}
	}
	return takes
}

// askedBoundary reports whether a serve at boundary is one the job's record can
// settle: a hook's, after a step's tool calls or as the run was about to
// finish, or a poll's recorded under its inbox id ([NoteServe.Inbox]).
func askedBoundary(boundary string) bool {
	switch types.SteerBoundary(boundary) {
	case types.SteerBoundaryToolBatch, types.SteerBoundaryFinish, types.SteerBoundaryStep:
		return true
	}
	return false
}

// readMainThread reads run's job's main thread in box through runner, once, for
// every reading read asks of it ([asksMainThread]): at the collection of a job
// whose runner reads its thread what its harness counts the copy at and
// estimates its next request at, and where its hooks serve receipts, at its
// collection, stop or loss the receipts of the hooks' serves the copy holds
// ([askedTakes]). A job not over, and one whose harness did not end its own
// run where the runner counts no cut-off copy ([ThreadTraits.CountsCutOff]),
// are counted [CountUnread] with no count read. The count carries the least window the run
// reported ([Result.ReportedWindow]). Held within [Coordinator.mainThreadWait],
// armed here: past it the read ends where it is, the count is [CountUnread]
// and no receipt is decided; a marked count is logged.
func (c *Coordinator) readMainThread(ctx context.Context, run PendingRun, box Sandbox, runner Runner,
	read BoxRead, at capturePoint, result Result) MainThreadReading {

	traits := ThreadTraitsOf(runner)
	if !asksMainThread(run, read, traits) {
		return MainThreadReading{}
	}
	counts, uncounted := read == BoxReadCollect && traits.Reads, !traits.CountsCutOff
	counted := counts && at.final && (!uncounted || !at.cutOff)
	ask := MainThreadAsk{Session: run.SessionRan(), Release: result.HarnessVersion, Count: counted,
		Next: counted, Ended: !at.cutOff}
	if asksTakeReceipts(run, traits) {
		ask.Takes, ask.Served = askedTakes(run), servedTakes(run)
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
		case uncounted && at.cutOff:
			why = "the job's harness did not end its own run, so its record of the session may lack what " +
				"its copy holds"
		case !reads:
			why = "the job's runner reads no main thread"
		}
		// THE SAME OUTCOME FOR EVERY HARNESS: a marked count bounds no first
		// request, so the copy is told as a cut-off one; the floor of one
		// whose cut-off copy is uncounted, which only its count would bound,
		// reads its newest request's input as well.
		outcome := "a relaunch that continues this copy in its box is told it as a cut-off one"
		if uncounted {
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
// anything, its runner's read settling traits: the collection of a job whose
// runner reads its thread, and any read of a box whose copy holds a serve its
// thread settles ([asksTakeReceipts]). [BoxReadBudget] holds the read's wait
// for exactly these.
func asksMainThread(run PendingRun, read BoxRead, traits ThreadTraits) bool {
	collects := read == BoxReadCollect && traits.Reads
	return collects || asksTakeReceipts(run, traits)
}

// asksTakeReceipts reports whether the read of run's box asks its job's
// record for receipts: the copy of a job whose runner confirms off that record
// ([ThreadTraits.Receipts]) holding a serve to the job
// ([PendingRun.asksReceipts]). Any other job's read asks none, nor holds a
// verdict's share ([BoxReadBudget]).
func asksTakeReceipts(run PendingRun, traits ThreadTraits) bool {
	return traits.Receipts && run.asksReceipts()
}

// threadTraits is what the runner c's manager holds for harness settles by
// reading its main thread, zero where the node has no runtime or no runner
// for it.
func (c *Coordinator) threadTraits(harness string) ThreadTraits {
	manager := c.mgr()
	if manager == nil {
		return ThreadTraits{}
	}
	runner, err := manager.RunnerFor(harness)
	if err != nil {
		return ThreadTraits{}
	}
	return ThreadTraitsOf(runner)
}
