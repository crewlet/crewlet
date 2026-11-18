package engine

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/learning/memsync"
	"github.com/crewlet/crewlet/internal/org"
)

// memorySyncInterval is how often a node carries what its seats have just
// learned onto the memory changelog: the crash window while the cycles keep
// up, and that alone sizes it. Thirty seconds against a turn of minutes loses
// at worst about one turn's diary entries and one episode, which a re-run turn
// writes again; longer loses whole turns, shorter buys little on a rare
// failure. It sits inside the 45-second lease TTL, so a stopped node has
// usually published once more before a peer can take its seats. Under overload
// the window grows whatever this value is: up to 2k + 3 cycles for k seats a
// cycle runs out in ([memorySync.resume]), longer behind a burst
// (crewlet/crewlet#1041), profiles and skills that outlast a cycle
// (crewlet/crewlet#1040) or a row the broker refuses (crewlet/crewlet#349).
const memorySyncInterval = 30 * time.Second

// memoryFlushTimeout bounds the last publish a released seat gets: five
// seconds, the bound of the seat's last lifecycle event beside it
// ([lifecyclePublishBudget]), so a stream that stopped acknowledging costs a
// release one bound, not two, and a drain (seat.Host.ReleaseAll) the five
// seconds docs/concepts/agent-runtime.md counts. No test pins it. At one round
// trip a row that is about fifty rows at 100 ms, a thousand at 5 ms. It carries
// them in a cycle's order ([publishTurn]); what it misses of the seat's own
// pass is lost as a crash loses it, which can exceed the last cycle's writes
// (crewlet/crewlet#1040, #1041 and #349), and what it misses of the backlog
// stays in this node's store. What it carries can still be overwritten by an
// older copy, from the row a cycle had in flight as the release began or from
// a node whose lease lapsed under it (crewlet/crewlet#511).
const memoryFlushTimeout = 5 * time.Second

// memoryAgentID is the id a seat's memory changelog is addressed by: the
// UUIDv5 over (company name, handle) (ADR-0013), derived rather than looked up
// in the roster, which no longer holds a seat a revision removed by the time
// its release flush runs; an empty seat token is a subject the broker refuses.
// Empty only with no company, which the memory syncer carries nothing for.
func memoryAgentID(company *Company, handle string) string {
	if company == nil || company.Org == nil {
		return ""
	}
	id, ok := org.DeriveAgentID(company.Org.Name, handle)
	if !ok {
		return ""
	}
	return id.String()
}

// backlogsTurn names the backlogs' turn where [memorySync.resume] and a cycle's
// log (`next`, `not_reached`) name a seat's by its handle: no handle can take
// it, since a handle matches [a-z0-9][a-z0-9-]* (org.ValidHandle).
const backlogsTurn = "(backlogs)"

// memorySync is the publish loop, the handle that stops it, and the state its
// cycles share.
type memorySync struct {
	stop chan struct{}
	done sync.WaitGroup

	// resume is the turn the next cycle starts at, a seat's or [backlogsTurn],
	// "" for the first seat; backlogResume the seat the next backlogs' turn
	// starts at, "" for the first. Touched only by the loop's goroutine.
	//
	// A cycle is one lap ([lapOrder]): each held seat's own pass in sorted
	// order, then the backlogs' turn ([publishBacklogs]). The next lap starts at
	// the turn the bound ran out inside, or after it when that turn was the
	// first ([nextStart]); at the first unreached turn when it ran out between
	// two; at the first seat when it did not run out, or when a cut backlogs'
	// turn moved a mark. So no seat waits more than 2k + 2 cycles, for k seats a
	// cycle runs out in, and a crash loses up to 2k + 3 cycles' writes (package
	// doc, "Memory sync cycles").
	resume, backlogResume string
}

// startMemorySync carries the memory of the seats this node holds.
//
// A per-node loop, not a fleet singleton like the retention sweep: only each
// node can read its own seats' memory from its own store. Nothing here fails a
// turn or a seat: a publish that does not land keeps, of a table with a
// watermark, every row the changelog acknowledged, and sends a
// whole-each-cycle table whole again; a release's flush carries on from the
// same marks ([memoryFlushTimeout]).
func (e *Engine) startMemorySync(ctx context.Context) {
	if e.memory == nil {
		return
	}
	// Detached from the signal context: one bound to it stops at SIGTERM,
	// before the drain has released the seats it carries. stopMemorySync
	// closes the stop channel that ends it, after the drain.
	ctx = context.WithoutCancel(ctx)
	loop := &memorySync{stop: make(chan struct{})}
	e.memorySync = loop
	ticker := time.NewTicker(memorySyncInterval)
	loop.done.Go(func() {
		defer ticker.Stop()
		for {
			select {
			case <-loop.stop:
				return
			case <-ticker.C:
				e.syncSeatMemory(ctx, loop)
			}
		}
	})
}

// stopMemorySync ends the loop, waiting for an in-flight cycle: one
// mid-publish holds a store read and a broker write, and closing either under
// it turns an orderly shutdown into a logged failure. Called after the drain;
// a cycle begun before a release stops at that seat's next row
// (memsync.ErrNotHeld), so this waits for its other seats and the row in
// flight, for up to the cycle's bound.
func (e *Engine) stopMemorySync() {
	if e.memorySync == nil {
		return
	}
	close(e.memorySync.stop)
	e.memorySync.done.Wait()
	e.memorySync = nil
}

// syncSeatMemory publishes one cycle for every seat this node holds
// ([memorySync.cycle]), bounded by the interval: the loop's context is
// detached, so an unanswering broker would otherwise block the cycle, and
// stopMemorySync with it, for ever, and a cycle still running when the next is
// due has fallen behind whatever it does.
func (e *Engine) syncSeatMemory(ctx context.Context, loop *memorySync) {
	node := e.node
	if node == nil {
		return
	}
	host := node.Host()
	if host == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, memorySyncInterval)
	defer cancel()
	loop.cycle(ctx, host, e.memory)
}

// heldHandles is the part of the seat host a memory sync cycle reads: which
// seats this node holds.
type heldHandles interface {
	Held() []string
}

// memoryPublisher is the part of the memory syncer a cycle calls: a seat's own
// pass and its backlog, and what the changelog acknowledged of each. Each
// carries a seat only while its holding here is live, and answers
// memsync.ErrNotHeld for one still being taken, being released or released.
type memoryPublisher interface {
	PublishOwn(ctx context.Context, handle string) (memsync.Carried, error)
	PublishBacklog(ctx context.Context, handle string) (memsync.Carried, error)
}

// memoryFlusher is the part of the memory syncer a release's flush calls: the
// same two passes as a cycle's ([memoryPublisher]), carried only while the
// seat's holding here is releasing, and answering memsync.ErrNotHeld
// otherwise.
type memoryFlusher interface {
	FlushOwn(ctx context.Context, handle string) (memsync.Carried, error)
	FlushBacklog(ctx context.Context, handle string) (memsync.Carried, error)
}

// releasedMemory is the part of the memory syncer a release calls
// ([releaseSeatMemory]): the turn of the seat's holding to releasing, which
// says whether it had one to turn, the flush, and the forgetting of the seat.
type releasedMemory interface {
	memoryFlusher
	Releasing(handle string) bool
	Forget(handle string)
}

// memoryPass names which of a seat's two publishes a call is, as every line
// about one says it (`pass`): its own pass ([memsync.Syncer.PublishOwn]) or its
// backlog ([memsync.Syncer.PublishBacklog]).
type memoryPass string

const (
	passOwn     memoryPass = "own"
	passBacklog memoryPass = "backlog"
)

// seatTurn is what a released seat's last turn under a bound did
// ([publishTurn]): its own pass, and its backlog where the bound left time for
// one.
type seatTurn struct {
	own, backlog passCall

	// backlogBegun says the backlog was called: the own pass returned with
	// the bound still running.
	backlogBegun bool
}

// passCall is one call of a seat's own pass or of its backlog: what the
// changelog acknowledged, the error the call ended on, and whether the bound
// had run out by the time it returned, which tells a call the bound cut from
// one that failed with time left.
type passCall struct {
	carried memsync.Carried
	err     error
	spent   bool
}

// notHeld says this node's holding of the seat stopped the call
// (memsync.ErrNotHeld). To a cycle that is a seat not its to carry, neither a
// failure nor a cut, and said nothing of; to a flush, which begins only under a
// holding its release turned ([releaseSeatMemory]), a holding ended under it,
// which [flushSeatMemory] warns of.
func (c passCall) notHeld() bool { return errors.Is(c.err, memsync.ErrNotHeld) }

// failed says the call returned an error while the bound still had time: a
// refusal or a broker error, the call's own failure. Never memsync.ErrNotHeld
// ([passCall.notHeld]).
func (c passCall) failed() bool { return c.err != nil && !c.spent && !c.notHeld() }

// cut says the bound ran out inside the call. Never memsync.ErrNotHeld
// ([passCall.notHeld]).
func (c passCall) cut() bool { return c.err != nil && c.spent && !c.notHeld() }

// publishTurn is a released seat's last turn under ctx ([flushSeatMemory]): its
// own pass, then, while ctx has time, its backlog. A cycle takes the two apart
// ([publishHeldMemory]).
//
// The own pass first, so what the seat wrote, and the tables every own pass
// sends whole, go before the backlog the changelog mostly holds already: a cut
// loses the backlog first (crewlet/crewlet#1039). The backlog runs after an own
// pass that failed with time left, since a refused row of the seat's own
// (crewlet/crewlet#349) is no reason to lose it; not after one that answered
// memsync.ErrNotHeld, which the backlog would answer too, nor on a spent bound.
func publishTurn(ctx context.Context, memory memoryFlusher, handle string) seatTurn {
	var turn seatTurn
	turn.own.carried, turn.own.err = memory.FlushOwn(ctx, handle)
	if turn.own.spent = ctx.Err() != nil; turn.own.spent || turn.own.notHeld() {
		return turn
	}
	turn.backlogBegun = true
	turn.backlog.carried, turn.backlog.err = memory.FlushBacklog(ctx, handle)
	turn.backlog.spent = ctx.Err() != nil
	return turn
}

// cycle is one cycle of the loop under ctx, the cycle's bound: it carries the
// seats held names from where the last cycle ran out, keeps where this one
// did for the next, and says what it did.
func (loop *memorySync) cycle(ctx context.Context, held heldHandles,
	memory memoryPublisher,
) syncReport {
	report := publishHeldMemory(ctx, held, memory, loop.resume, loop.backlogResume)
	loop.resume, loop.backlogResume = report.resume, report.backlogResume
	report.log(ctx)
	return report
}

// publishHeldMemory is one cycle over the seats held names: its turns, one
// after another in [lapOrder] from resume, each held seat's own pass and then
// the backlogs' turn ([publishBacklogs]) from backlogResume, until every turn
// has been had or ctx, the cycle's bound, runs out. It reports what the cycle
// did, where the next one starts, and where the next backlogs' turn does.
func publishHeldMemory(ctx context.Context, held heldHandles, memory memoryPublisher,
	resume, backlogResume string,
) syncReport {
	report := syncReport{backlogResume: backlogResume}
	lap := lapOrder(held.Held(), resume)
	for i, turn := range lap {
		if ctx.Err() != nil {
			// The bound ran out as the turn before returned: the next cycle
			// starts here, and calling the rest on a spent bound would only
			// fail each with a warning of its own.
			report.ranOut, report.resume, report.notReached = true, turn, lap[i:]
			report.cut.pass = passOwn
			if turn == backlogsTurn {
				report.cut.pass = passBacklog
			}
			return report
		}
		var movedMarks bool
		if turn == backlogsTurn {
			// The seats held are read again: one released since the lap
			// began is not called, and one taken since is, carrying nothing
			// until its hydration completes.
			backlogs := publishBacklogs(ctx, held.Held(), memory, backlogResume, i == 0)
			report.backlogResume = backlogs.resume
			report.failed = append(report.failed, backlogs.failed...)
			if !backlogs.ranOut {
				continue
			}
			report.cut, report.inBacklogs = backlogs.cut, true
			report.backlogsNotReached = backlogs.notReached
			movedMarks = backlogs.marked > 0
		} else {
			// Each call is classified by the bound as it returned
			// ([syncReport.lines]). A failure with time left is that call's:
			// its marks keep what the changelog acknowledged, and where the
			// next cycle starts is untouched. A seat not the cycle's to
			// carry ([passCall.notHeld]) has had its turn, said nothing of.
			var own passCall
			own.carried, own.err = memory.PublishOwn(ctx, turn)
			own.spent = ctx.Err() != nil
			call := seatPublish{handle: turn, pass: passOwn, carried: own.carried, err: own.err}
			if own.failed() {
				report.failed = append(report.failed, call)
			}
			if !own.cut() {
				continue
			}
			report.cut = call
		}
		// The bound ran out inside this turn: [nextStart] picks where the
		// next cycle starts, by [memorySync.resume]'s rule.
		report.ranOut, report.first = true, i == 0
		report.resume, report.notReached = nextStart(lap, i), lap[i+1:]
		if turn == backlogsTurn && movedMarks {
			// The exception: a backlogs' turn cut after it moved a mark
			// needs no whole cycle to move more, so every seat's own rows go
			// first.
			report.resume = ""
		}
		return report
	}
	return report
}

// publishBacklogs is a cycle's backlogs' turn under ctx, the cycle's bound:
// each held seat's backlog ([memsync.Syncer.PublishBacklog]) in [cycleOrder]
// from resume, by the rule a cycle's lap follows ([memorySync.resume]). first
// says the turn is the cycle's first, so its first seat has the whole cycle.
func publishBacklogs(ctx context.Context, held []string, memory memoryPublisher,
	resume string, first bool,
) backlogsReport {
	var report backlogsReport
	order := cycleOrder(held, resume)
	for i, handle := range order {
		if ctx.Err() != nil {
			// The bound ran out between two seats' backlogs: the next
			// backlogs' turn starts here.
			report.ranOut, report.resume, report.notReached = true, handle, order[i:]
			report.cut.pass = passBacklog
			return report
		}
		var backlog passCall
		backlog.carried, backlog.err = memory.PublishBacklog(ctx, handle)
		backlog.spent = ctx.Err() != nil
		report.marked += backlog.carried.Marked
		call := seatPublish{handle: handle, pass: passBacklog, carried: backlog.carried,
			err: backlog.err}
		if backlog.failed() {
			report.failed = append(report.failed, call)
		}
		if !backlog.cut() {
			continue
		}
		report.ranOut, report.cut = true, call
		report.resume, report.notReached = nextStart(order, i), order[i+1:]
		if !first && report.marked == 0 {
			// A late turn that moved nothing: the lap gives the next cycle
			// to the backlogs' turn ([publishHeldMemory]), and that whole
			// cycle is this seat's, whose next row needs more than the turn
			// had, even as the turn's first, which [nextStart] would pass.
			// Judged by the turn's marks, as the lap judges it.
			report.resume = handle
		}
		return report
	}
	return report
}

// backlogsReport is what a cycle's backlogs' turn did ([publishBacklogs]).
type backlogsReport struct {
	// resume is the seat the next backlogs' turn starts at
	// ([memorySync.backlogResume]).
	resume string

	// failed is every seat's backlog that failed while the cycle still had
	// time, with what the changelog acknowledged before the failure.
	failed []seatPublish

	// ranOut says the cycle's bound ran out before every seat's backlog had
	// returned. cut is the call it ran out inside, with no handle when it
	// ran out between two, and notReached the seats whose backlog the turn
	// never called, in its order.
	ranOut     bool
	cut        seatPublish
	notReached []string

	// marked is the rows of every call of the turn that moved a mark: a turn
	// cut after any seat's backlog moved one is draining, whatever the cut
	// call carried ([memorySync.resume]'s exception).
	marked int
}

// nextStart is where a rotation over order starts next when the cycle's bound
// ran out inside its i-th turn ([memorySync.resume]): at that turn, which had
// only part of the cycle, or after the first, which had the whole cycle,
// wrapping round, a lone turn being its own next.
func nextStart(order []string, i int) string {
	if i == 0 {
		return order[1%len(order)]
	}
	return order[i]
}

// lapOrder is a cycle's turns: the held seats sorted, then [backlogsTurn],
// rotated to start at resume. A resume the node no longer holds starts at the
// turn after it, which the last cycle did not reach either (the backlogs' turn
// for one sorting after every held seat); "" starts at the first seat.
func lapOrder(held []string, resume string) []string {
	turns := append(slices.Sorted(slices.Values(held)), backlogsTurn)
	at := len(turns) - 1
	if resume != backlogsTurn {
		at, _ = slices.BinarySearch(turns[:len(turns)-1], resume)
	}
	return slices.Concat(turns[at:], turns[:at])
}

// cycleOrder is the seats held in the order a backlogs' turn calls them:
// sorted, so a seat's place does not depend on how the host lists them, and
// rotated to start at resume, or at the first seat after a resume no longer
// held; one sorting after every held seat, or "", starts at the first.
func cycleOrder(held []string, resume string) []string {
	seats := slices.Sorted(slices.Values(held))
	if at, _ := slices.BinarySearch(seats, resume); at < len(seats) {
		seats = slices.Concat(seats[at:], seats[:at])
	}
	return seats
}

// syncReport is what one cycle did.
type syncReport struct {
	// resume is the turn the next cycle starts at ([memorySync.resume]), and
	// backlogResume the seat the next backlogs' turn starts at, which a cycle
	// that did not reach the backlogs' turn leaves where it was.
	resume, backlogResume string

	// failed is every call that failed while the cycle still had time, a
	// seat's own pass or its backlog, with what the changelog acknowledged
	// before the failure.
	failed []seatPublish

	// ranOut says the cycle's bound ran out before every turn had been had.
	// cut is the call it ran out inside, with no handle when it ran out
	// between two calls, its pass then that of the call not begun.
	// inBacklogs says it ran out in the backlogs' turn, and first that it ran
	// out in the cycle's first turn, which had the whole cycle. notReached is
	// the turns never begun, in order, and backlogsNotReached the seats whose
	// backlog the backlogs' turn never called.
	ranOut             bool
	cut                seatPublish
	inBacklogs, first  bool
	notReached         []string
	backlogsNotReached []string
}

// seatPublish is one call of a cycle: which of a seat's two publishes it was,
// what the changelog acknowledged, and the error the call ended on.
type seatPublish struct {
	handle  string
	pass    memoryPass
	carried memsync.Carried
	err     error
}

// log says what a cycle did ([syncReport.lines]).
func (r syncReport) log(ctx context.Context) {
	for _, line := range r.lines() {
		log.Log(ctx, line.level, line.msg, line.args...)
	}
}

// lines is what a cycle says, as a value a test reads rather than a side effect
// on the logger. Each call is said on its own: a warning for each that failed
// with time left (a refused row is refused every cycle, crewlet/crewlet#349),
// and one line for a run-out naming its `pass` (or the first not begun),
// `next`, `not_reached` and, in the backlogs' turn, `backlogs_next` and
// `backlogs_not_reached`. A run-out after the call moved a mark (a backlog
// draining, a burst: crewlet/crewlet#1041) is INFO, since a warning every
// thirty seconds would read as a fault on every boot, and so is one between two
// calls, the last of them complete. One that moved no mark warns, as does a
// seat that had the whole cycle and was cut in a whole-each-cycle table, which
// keeps nothing of a cut publish: the sign of crewlet/crewlet#1040.
func (r syncReport) lines() []reportLine {
	var out []reportLine
	for _, failed := range r.failed {
		c := failed.carried
		out = append(out, reportLine{level: slog.LevelWarn, msg: "seat_memory_not_carried",
			args: []any{"seat", failed.handle, "pass", failed.pass, "table", c.StoppedIn,
				"rows", c.Rows, "marked", c.Marked, "error", failed.err}})
	}
	if !r.ranOut {
		return out
	}
	where := []any{"not_reached", strings.Join(r.notReached, ","), "next", r.resume}
	if r.inBacklogs {
		where = append(where, "backlogs_not_reached", strings.Join(r.backlogsNotReached, ","),
			"backlogs_next", r.backlogResume)
	}
	if r.cut.handle == "" {
		var detail string
		switch {
		case r.inBacklogs:
			detail = "the cycle's bound ran out in the backlogs' turn, between two seats' " +
				"backlogs; the next backlogs' turn starts at the first seat it did not reach"
		case r.cut.pass == passBacklog:
			detail = "the cycle's bound ran out just before the backlogs' turn, which carries " +
				"the rows this node held for its seats when it took them; the next cycle " +
				"starts with that turn"
		default:
			detail = "the cycle's bound ran out between two of its turns; the next cycle " +
				"starts at the first turn it did not reach"
		}
		return append(out, reportLine{level: slog.LevelInfo, msg: "memory_sync_ran_out",
			args: slices.Concat([]any{"pass", r.cut.pass}, where, []any{"detail", detail})})
	}
	c := r.cut.carried
	line := reportLine{level: slog.LevelWarn, msg: "seat_memory_not_carried",
		args: slices.Concat([]any{"seat", r.cut.handle, "pass", r.cut.pass,
			"table", c.StoppedIn, "rows", c.Rows, "marked", c.Marked}, where)}
	var detail string
	switch {
	case c.Rows == 0 && r.cut.pass == passBacklog:
		detail = "the changelog acknowledged none of this seat's backlog before the " +
			"cycle's bound ran out: a broker that is not acknowledging, a row that takes " +
			"longer to publish than the cycle had left, or too little of the cycle left " +
			"once the seats' own passes before the backlogs' turn had had theirs"
	case c.Rows == 0:
		detail = "the changelog acknowledged none of this seat's rows before the cycle's " +
			"bound ran out: a broker that is not acknowledging, a row that takes longer " +
			"to publish than the seat had, or, for a seat the cycle reached late, too " +
			"little of the cycle left"
	case r.first && c.Whole:
		detail = "this seat had the whole cycle and was cut inside a table every own pass " +
			"sends whole, which keeps nothing of a cut publish: a seat whose counterparty " +
			"profiles and skills outlast a cycle never has the rest of them carried past " +
			"where each cycle cuts them, nor its onboarding marker after them " +
			"(crewlet/crewlet#1040)"
	case c.Marked == 0:
		detail = "the changelog acknowledged only rows of the tables every own pass sends " +
			"whole, which keep nothing of a cut publish, so this cycle carried nothing of " +
			"the seat that stays carried"
	case r.cut.pass == passOwn:
		line.level, line.msg = slog.LevelInfo, "memory_sync_ran_out"
		detail = "the cycle's bound ran out inside this seat's own pass after it moved " +
			"marks: a burst the seat wrote, such as a vector fill after a change of " +
			"embedding model (crewlet/crewlet#1041); the seat's next turn carries on " +
			"from the first row past them"
	default:
		line.level, line.msg = slog.LevelInfo, "memory_sync_ran_out"
		detail = "a backlog draining: the cycle's bound ran out inside the rows this node " +
			"held for the seat when it took it, after they moved marks, and this seat's " +
			"backlog carries on from the first row past them when a backlogs' turn next " +
			"reaches it, after every seat's own pass"
	}
	line.args = append(line.args, "detail", detail)
	return append(out, line)
}

// flushSeatMemory is a released seat's last turn under ctx, the flush's bound
// ([memoryFlushTimeout]), from the marks the cycles left ([publishTurn]), and
// what it says, as a value a test reads. Each call is said at the level of what
// it lost. An own pass that returned an error warns (seat_memory_not_flushed):
// what it missed is lost to the next holder. A backlog that failed with time
// left warns too; one the bound cut or never began is INFO
// (memory_flush_ran_out), since the changelog mostly holds it and a warning
// would fire on every handoff soon after an acquisition. A holding ended under
// the flush (memsync.ErrNotHeld) warns: the release turned it and is serialised
// with the next taking, so nothing should end it, and silence would lose a
// seat's last writes without a line.
func flushSeatMemory(ctx context.Context, memory memoryFlusher, handle string) []reportLine {
	turn := publishTurn(ctx, memory, handle)
	var out []reportLine
	if turn.own.err != nil {
		c := turn.own.carried
		out = append(out, reportLine{level: slog.LevelWarn, msg: "seat_memory_not_flushed",
			args: []any{"seat", handle, "pass", passOwn, "table", c.StoppedIn, "rows", c.Rows,
				"marked", c.Marked, "error", turn.own.err,
				"detail", "what this flush did not reach of what the seat wrote while held " +
					"here and of its tables sent whole is lost to its next holder"}})
	}
	c := turn.backlog.carried
	switch {
	case !turn.backlogBegun && turn.own.err == nil:
		out = append(out, reportLine{level: slog.LevelInfo, msg: "memory_flush_ran_out",
			args: []any{"seat", handle, "pass", passBacklog, "rows", 0, "marked", 0,
				"detail", "the flush's bound ran out as the seat's own pass returned: it " +
					"carried what the seat wrote while held here and its tables sent whole, " +
					"and none of the rows this node held for it when it took it, which the " +
					"changelog mostly holds already"}})
	case turn.backlog.cut():
		out = append(out, reportLine{level: slog.LevelInfo, msg: "memory_flush_ran_out",
			args: []any{"seat", handle, "pass", passBacklog, "table", c.StoppedIn,
				"rows", c.Rows, "marked", c.Marked,
				"detail", "the flush carried what the seat wrote while held here and its " +
					"tables sent whole, and ran out in the rows this node held for it when " +
					"it took it, which the changelog mostly holds already: what of them it " +
					"did not reach stays in this node's store"}})
	case turn.backlog.failed() || turn.backlog.notHeld():
		out = append(out, reportLine{level: slog.LevelWarn, msg: "seat_memory_not_flushed",
			args: []any{"seat", handle, "pass", passBacklog, "table", c.StoppedIn,
				"rows", c.Rows, "marked", c.Marked, "error", turn.backlog.err,
				"detail", "the rows this node held for the seat when it took it that this " +
					"flush did not reach stay in this node's store, and are lost to the " +
					"fleet where the changelog does not hold them"}})
	}
	return out
}

// releaseSeatMemory is a released seat's memory: its holding here turned to
// releasing, its last turn ([flushSeatMemory]), then the seat forgotten.
// Releasing first, so a cycle partway through the seat stops at its next row
// rather than put older profiles, skills and onboarding marker over the
// flush's (crewlet/crewlet#511). No flush, and no line, without a live holding
// to turn ([memsync.Syncer.Releasing]): a store whose hydration failed or never
// ran can hold copies older than the changelog's; one that failed after its
// hydration completed is flushed, as it holds the changelog's own copies. The
// flush is bounded on a context the release's does not cut, since a drain
// releases every seat at once (seat.Host.ReleaseAll), and best effort: what it
// misses costs the successor as a crash does, where refusing to release would
// strand the seat for a full TTL.
func releaseSeatMemory(ctx context.Context, memory releasedMemory, handle string) {
	defer memory.Forget(handle)
	if !memory.Releasing(handle) {
		return
	}
	flushCtx, stopFlush := context.WithTimeout(context.WithoutCancel(ctx), memoryFlushTimeout)
	for _, line := range flushSeatMemory(flushCtx, memory, handle) {
		log.Log(ctx, line.level, line.msg, line.args...)
	}
	stopFlush()
}
