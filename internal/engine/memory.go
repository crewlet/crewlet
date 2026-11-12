package engine

import (
	"context"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/learning/memsync"
	"github.com/crewlet/crewlet/internal/org"
)

// memorySyncInterval is how often a node carries what its seats have just
// learned onto the memory changelog.
//
// IT IS THE CRASH WINDOW WHILE THE CYCLES KEEP UP, and that is the only thing
// sizing it. A node that dies without releasing loses what of its seats'
// memory the changelog has not acknowledged: while every cycle gets through
// every seat's turn, what each wrote since its last one. Otherwise the window
// is longer whatever this value is. A seat's turn carries what it wrote
// before its own backlog ([publishTurn]), but its new rows still wait behind
// the backlogs of the seats a cycle carries before it: the rows an
// acquisition or a restart leaves this node to carry back for each of them
// (crewlet/crewlet#1039). They wait behind a burst the seat wrote, too, such
// as a vector fill after a change of embedding model (crewlet/crewlet#1041).
// A seat whose counterparty profiles and skills outlast a cycle never has its
// onboarding marker carried, nor the rows this node held for it when it took
// it, which its turn reaches only after them (crewlet/crewlet#1040). A row
// larger than the broker takes fails every pass of its seat that meets it, at
// that row, so nothing after it in that pass, in its table or in the tables
// the pass carries after it, is carried (crewlet/crewlet#349). And cycles that
// run out at k seats leave a seat waiting up to 2k cycles between two turns
// ([memorySync.resume]). A graceful handoff's flush starts where the cycles
// left off, and what it cannot reach in its own bound is lost too
// ([memoryFlushTimeout]).
//
// Thirty seconds against a turn that takes minutes, so while the cycles keep
// up the worst case is roughly one turn's worth of diary entries and one
// episode — and a seat that re-runs a lost turn writes them again. Shorter
// buys a smaller window on a failure that is already rare; longer starts
// losing whole turns of learning to a single crash.
//
// It is also comfortably inside the 45-second lease TTL, so a node that stops
// publishing has usually published once more before a peer can take its seats
// and hydrate.
const memorySyncInterval = 30 * time.Second

// memoryFlushTimeout bounds the last publish a released seat gets.
//
// FIVE SECONDS, held there by what runs beside it rather than by a count of
// rows. The seat's last lifecycle event is published beside the teardown this
// runs in, under the same five seconds ([lifecyclePublishBudget]), so a stream
// that has stopped acknowledging costs a release one bound rather than two. A
// drain releases every seat at once (seat.Host.ReleaseAll), so the flushes
// cost it five seconds together, which is what docs/concepts/agent-runtime.md
// counts for them in how long a stop takes. No test pins the value, so these
// are what a change to it answers to.
//
// WHAT FIVE SECONDS CARRIES, at one broker round trip a row from the marks the
// seat's cycles left, is about fifty rows at 100 ms (a leaf link to another
// region), a thousand at 5 ms and five thousand at 1 ms: a sixth of what a
// thirty-second cycle carries between all the node's seats. It carries them as
// a cycle's turn does ([flushSeatMemory]): what the seat wrote while held here
// first, then its counterparty profiles, skills and onboarding marker, which
// every publish sends whole, and then, while the bound has time, the rows this
// node held for the seat when it took it, its backlog, which the changelog
// mostly holds already. What it has not reached of the first two when it
// expires is lost, as a crash loses it, and that can be more than the last
// cycle's writes:
//
//   - when the seat's counterparty profiles and skills alone outlast the five
//     seconds: the flush is cut inside them, short of the onboarding marker
//     and the backlog after them (crewlet/crewlet#1040);
//   - behind a burst the seat wrote, such as a vector fill after a change of
//     embedding model, larger than five seconds of round trips carry
//     (crewlet/crewlet#1041).
//
// A flush that fails loses the same. One that meets a row larger than the
// broker takes fails that pass at it, as every cycle of the seat has, so
// nothing after that row in its pass, in its table or in the tables the pass
// carries after it, is carried (crewlet/crewlet#349): such a row of the
// seat's own stops its own pass, and leaves the backlog the rest of the
// bound. What a flush has not reached of the backlog stays in this node's
// store, and the fleet loses it only where the changelog lacks it: rows an
// earlier holding's flush did not reach, or rows a changelog restored while
// the node ran does not hold. And what a flush does carry of the profiles,
// skills and marker can be replaced by an older copy published after it: by
// this node's own sync, from a cycle it began before the release, by the next
// holder while it is still taking the seat, or by a node whose taking of the
// seat failed, in the flush that gives it back (crewlet/crewlet#511).
const memoryFlushTimeout = 5 * time.Second

// memoryAgentID is the id a seat's memory changelog is addressed by: the
// UUIDv5 over (company name, handle) (ADR-0013), DERIVED rather than looked up
// in the roster.
//
// The roster cannot answer for the seat the release flush exists for. A
// revision that removes a seat is already installed when the node lets the
// seat go, so a lookup found nobody and the flush published to a subject with
// an empty seat token, which the broker refuses — every removed seat logged
// `seat_memory_not_flushed` and what its flush was to carry never reached the
// changelog that a seat re-added under the same handle hydrates from.
//
// Empty only with no company to derive it in, which the memory syncer reads as
// a seat with no address and carries nothing for.
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

// memorySync is the publish loop, the handle that stops it, and the state its
// cycles share.
type memorySync struct {
	stop chan struct{}
	done sync.WaitGroup

	// resume is the seat the next cycle starts at, "" for the first held
	// seat: the seat the last cycle ran out at, or, when that seat was the
	// cycle's first and so had the whole cycle, the seat after it in the
	// cycle's order, wrapping round, which for a lone seat is the seat
	// itself. Touched only by the loop's own goroutine.
	//
	// NOT THE MEMORY FILL'S RULE ([memoryFill.resume]), which starts at the
	// seat the last tick ran out at whatever its place. Here a seat that had
	// the whole cycle would then have every cycle until its backlog drained,
	// and every other seat the node holds would go uncarried that long, the
	// seats sorted before it included, which a sorted start carried every
	// cycle: at 100 ms a publish, 18 000 rows are half an hour. Nor the seat
	// after it every time: a seat that ran a cycle out second would then
	// never go first, so of two seats that each need most of a cycle the
	// second would never have a whole one: its whole-each-cycle tables, which
	// keep nothing of a cut publish, would never be carried past where its
	// cycles cut them, nor what its turn carries after them.
	//
	// THE RULE IS A SEAT'S TURN'S ([publishTurn]): a cycle that runs out in
	// either of a seat's two calls, or between them, has run out at that
	// seat. WHAT IT GUARANTEES, with k the held seats a cycle runs out at,
	// whether a seat needs more than a whole cycle or was reached with less
	// of one left than its turn needs: NO SEAT WAITS MORE THAN 2k CYCLES
	// between two that reach it. A cycle that runs out at a seat stops there,
	// and the next starts at that seat, which then has the whole cycle, or
	// after it when it has just had one; so each of the k holds the seats
	// after it back for at most two cycles before the cycles pass it, and
	// they come back to it only after reaching every other seat. A seat a
	// cycle cut short goes first in the next, so a seat whose turn fits a
	// whole cycle completes it at least once in every 2k + 1 cycles, and a
	// seat with a backlog has a whole cycle at least once in every 2k. Both
	// bounds are reached. Of held seats a, b and c, where the cycles run out
	// at b alone, twice, a completes and the first cycle runs out at b, b has
	// the whole second, and c, which neither reached, waits those two: 2k for
	// k = 1. Of held seats a, b, c and d, where a and c run every cycle out,
	// a and c each have a whole cycle once every four: 2k for k = 2. A
	// smaller bound would need a fact per seat that one resume cannot hold,
	// whether a seat cut part-way through the rotation has had its whole
	// cycle since, and denying that seat its whole cycle instead is the
	// seat-after-it rule, which starves.
	resume string
}

// startMemorySync carries the memory of the seats this node holds.
//
// A PER-NODE LOOP rather than a fleet singleton, which is the opposite of the
// retention sweep beside it: a singleton is right when N nodes would do the
// same work, and here they would each do DIFFERENT work — every node has its
// own store holding its own seats' memory, and only that node can read it.
//
// Nothing here fails a turn or a seat. What a publish that does not land
// costs is split by table: one with a watermark keeps every row the changelog
// acknowledged, and the seat's next publish resumes at the first it did not,
// while a whole-each-cycle table keeps nothing of it and is sent whole again.
// A release's flush carries on from the same marks, for [memoryFlushTimeout],
// and what it does not reach is lost to the seat's next holder, which that
// constant says can be more than the last cycle's writes.
func (e *Engine) startMemorySync(ctx context.Context) {
	if e.memory == nil {
		return
	}
	// DETACHED from the signal context, like the sweep and the waiter
	// beside it: a loop bound to it stops at SIGTERM, before the drain has
	// released the seats whose memory it is carrying. Its own stop channel
	// is what ends it, and stopMemorySync is what closes that — after the
	// drain, so the loop carries every seat until its release's flush.
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

// stopMemorySync ends the loop, waiting for an in-flight cycle.
//
// Waiting rather than signalling and walking away: a cycle mid-publish holds
// a store read and a broker write, and letting the process close either one
// underneath it turns an orderly shutdown into a logged failure on the way
// out. Called AFTER the drain, which has released every seat this node held,
// each with its flush. A cycle that began before a release can still be
// running, though: it walks the seats it found held then, so it can still be
// publishing a released seat, or start on one after the release forgot its
// marks and publish everything this node holds for it, over what the next
// holder has written since (crewlet/crewlet#511). This waits for that too, for
// up to the cycle's bound.
func (e *Engine) stopMemorySync() {
	if e.memorySync == nil {
		return
	}
	close(e.memorySync.stop)
	e.memorySync.done.Wait()
	e.memorySync = nil
}

// syncSeatMemory publishes one cycle for every seat this node holds.
//
// BOUNDED BY THE INTERVAL ITSELF. The loop's context is detached, so without
// this a publish against a broker that has stopped answering blocks the cycle
// for ever — and stopMemorySync, which waits for it, would then block the
// shutdown for ever with it. A cycle that cannot finish before the next one
// is due has fallen behind whatever it does next, so the interval is the
// value.
//
// EACH SEAT HAS A TURN ([publishTurn]): what it wrote while held here and its
// tables sent whole, then, while the bound has time, the rows this node held
// for it when it took it, its backlog. WHAT A CYCLE THE BOUND CUTS KEEPS: of a
// table with a watermark, every row the changelog acknowledged, and the
// seat's next turn resumes each range at the first row it did not; of a
// whole-each-cycle table, nothing, and the next turn sends it whole again.
// The seats the cycle did not reach are not called on a bound already spent,
// and the next cycle starts at the seat this one ran out at, or at the seat
// after it when that seat had the whole cycle ([memorySync.resume]). A cycle
// that ran out says so in one line, naming the call it ran out in: a call
// that moved a mark is a backlog draining or a burst of the seat's own, at
// INFO, and so is a turn whose own pass returned as the bound ran out; one
// that moved none, or that had the whole cycle and was cut inside a
// whole-each-cycle table, warns ([syncReport.lines]).
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

// memoryPublisher is the part of the memory syncer a seat's turn calls: its
// own pass and its backlog, and what the changelog acknowledged of each.
type memoryPublisher interface {
	PublishOwn(ctx context.Context, handle string) (memsync.Carried, error)
	PublishBacklog(ctx context.Context, handle string) (memsync.Carried, error)
}

// memoryPass names one of the two publishes of a seat's turn, as every line
// about one says it (`pass`): its own pass, what the seat wrote while held
// here and its tables sent whole ([memsync.Syncer.PublishOwn]), or its
// backlog, the rows this node held for it when it took it
// ([memsync.Syncer.PublishBacklog]).
type memoryPass string

const (
	passOwn     memoryPass = "own"
	passBacklog memoryPass = "backlog"
)

// seatTurn is what one seat's turn under a bound did: its own pass, and its
// backlog where the bound left time for one.
type seatTurn struct {
	own, backlog passCall

	// backlogBegun says the backlog was called: the own pass returned with
	// the bound still running.
	backlogBegun bool
}

// passCall is one call of a seat's turn: what the changelog acknowledged, the
// error the call ended on, and whether the bound had run out by the time it
// returned, which tells a call the bound cut from one that failed with time
// left.
type passCall struct {
	carried memsync.Carried
	err     error
	spent   bool
}

// failed says the call returned an error while the bound still had time: a
// refusal or a broker error, the call's own failure.
func (c passCall) failed() bool { return c.err != nil && !c.spent }

// cut says the bound ran out inside the call.
func (c passCall) cut() bool { return c.err != nil && c.spent }

// publishTurn is one seat's turn under ctx, its bound: the seat's own pass,
// then, while ctx has time, its backlog, whatever the own pass returned.
//
// THE OWN PASS FIRST, so what the seat wrote while held here, and then its
// profiles, skills and onboarding marker, which every publish sends whole, go
// before the rows this node held for it when it took it: every acquisition and
// restart leaves the node that backlog, which the changelog mostly holds
// already, and what the seat wrote waited behind it, so a handoff or a crash
// before it had drained lost what the seat wrote (crewlet/crewlet#1039). A cut
// now loses the backlog first.
//
// THE BACKLOG AFTER AN OWN PASS THAT FAILED WITH TIME LEFT too: a row of the
// seat's own the broker refuses (crewlet/crewlet#349) stops that pass and
// never the backlog, which loses nothing by having the time the own pass left.
// NO BACKLOG ON A SPENT BOUND, as no seat is called on one: an own pass the
// bound cut, or one that returned as it ran out, ends the turn there.
func publishTurn(ctx context.Context, memory memoryPublisher, handle string) seatTurn {
	var turn seatTurn
	turn.own.carried, turn.own.err = memory.PublishOwn(ctx, handle)
	if turn.own.spent = ctx.Err() != nil; turn.own.spent {
		return turn
	}
	turn.backlogBegun = true
	turn.backlog.carried, turn.backlog.err = memory.PublishBacklog(ctx, handle)
	turn.backlog.spent = ctx.Err() != nil
	return turn
}

// cycle is one cycle of the loop under ctx, the cycle's bound: it carries the
// seats held names from where the last cycle ran out, keeps where this one
// did for the next, and says what it did.
func (loop *memorySync) cycle(ctx context.Context, held heldHandles,
	memory memoryPublisher,
) syncReport {
	report := publishHeldMemory(ctx, held, memory, loop.resume)
	loop.resume = report.resume
	report.log(ctx)
	return report
}

// publishHeldMemory is one cycle over the seats held names: each seat's turn
// ([publishTurn]), one after another in [cycleOrder] from resume, until every
// seat has had one or ctx, the cycle's bound, runs out. It reports what the
// cycle did and where the next one starts.
func publishHeldMemory(ctx context.Context, held heldHandles, memory memoryPublisher,
	resume string,
) syncReport {
	var report syncReport
	order := cycleOrder(held.Held(), resume)
	for i, handle := range order {
		if ctx.Err() != nil {
			// THE BOUND RAN OUT BEFORE THIS SEAT'S TURN BEGAN, just as the
			// turn before it returned, so the next cycle starts here.
			// Called anyway, as they used to be, this seat and every one
			// after it failed at once, each with a warning of its own, in
			// every cycle a backlog outlasted.
			report.ranOut, report.resume, report.notReached = true, handle, order[i:]
			return report
		}
		turn := publishTurn(ctx, memory, handle)
		// EACH OF THE TURN'S CALLS IS CLASSIFIED ON ITS OWN, by the bound
		// as it returned ([syncReport.lines]). A FAILURE WITH TIME LEFT, a
		// refusal or a broker error, is that call's: the seat has had it,
		// its marks keep what the changelog acknowledged before the row
		// that failed, and the cycle moves on without touching where the
		// next one starts. An own pass's failure is said whatever its
		// backlog then did.
		if turn.own.failed() {
			report.failed = append(report.failed, seatPublish{handle: handle, pass: passOwn,
				carried: turn.own.carried, err: turn.own.err})
		}
		if turn.backlog.failed() {
			report.failed = append(report.failed, seatPublish{handle: handle,
				pass: passBacklog, carried: turn.backlog.carried, err: turn.backlog.err})
		}
		var cut seatPublish
		switch {
		case turn.own.cut():
			cut = seatPublish{handle: handle, pass: passOwn, carried: turn.own.carried,
				err: turn.own.err}
		case !turn.backlogBegun:
			// THE BOUND RAN OUT AS THE OWN PASS RETURNED, whole, and before
			// the backlog: the turn is cut between its two calls, and the
			// call it ran out in is the backlog, never called.
			cut = seatPublish{handle: handle, pass: passBacklog}
		case turn.backlog.cut():
			cut = seatPublish{handle: handle, pass: passBacklog,
				carried: turn.backlog.carried, err: turn.backlog.err}
		default:
			continue
		}
		// THE BOUND RAN OUT INSIDE THIS SEAT'S TURN, in either call or
		// between them. A seat that was not the cycle's first had only part
		// of a cycle, so the next cycle starts at it; the first had the
		// whole cycle, so the next starts at the seat after it, wrapping
		// round, a lone seat being its own next. The rule's bound, no seat
		// waiting more than 2k cycles for k seats the cycles run out at,
		// and why it is not the memory fill's rule: [memorySync.resume].
		report.ranOut, report.first = true, i == 0
		report.cut = cut
		report.resume, report.notReached = handle, order[i+1:]
		if report.first {
			report.resume = order[(i+1)%len(order)]
		}
		return report
	}
	return report
}

// cycleOrder is the seats held in the order a cycle carries them: sorted, so a
// seat's place does not depend on how the host lists them, and rotated to
// start at resume, or, for a resume the node no longer holds, at the first
// seat after it. A resume that sorts after every held seat, or none, starts
// at the first.
func cycleOrder(held []string, resume string) []string {
	seats := slices.Sorted(slices.Values(held))
	if at, _ := slices.BinarySearch(seats, resume); at < len(seats) {
		seats = slices.Concat(seats[at:], seats[:at])
	}
	return seats
}

// syncReport is what one cycle did.
type syncReport struct {
	// resume is the seat the next cycle starts at ([memorySync.resume]).
	resume string

	// failed is every call of a seat's turn that failed while the cycle
	// still had time, with what the changelog acknowledged before the
	// failure: a turn whose two calls both failed is in it twice.
	failed []seatPublish

	// ranOut says the cycle's bound ran out before every seat's turn had
	// returned. cut is the call it ran out inside: with no handle when it ran
	// out between two seats' turns, and with no error when it ran out between
	// a turn's two calls, the backlog it names never called. first says that
	// seat was the cycle's first, so had the whole cycle. notReached is the
	// seats the cycle never called, in its order.
	ranOut     bool
	cut        seatPublish
	first      bool
	notReached []string
}

// seatPublish is one call of a seat's turn in a cycle: which of the seat's two
// publishes it was, what the changelog acknowledged, and the error the call
// ended on.
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

// lines is what a cycle says: a warning for each call of a seat's turn that
// failed with time left, and one line for a cycle whose bound ran out, naming
// the call it ran out in and the seats it did not reach, which say nothing of
// their own. Every line names the pass of the call it is about (`pass`): the
// call that failed, the call the bound ran out in, or, for a cycle that ran
// out between two seats' turns, the first call it did not reach, the next
// seat's own pass. So two lines about one seat are told apart.
//
// EACH OF A TURN'S CALLS IS SAID ON ITS OWN, never the two merged into one. A
// turn whose own pass was refused with time left and whose backlog the bound
// then cut would read, merged, as a cut that moved marks: a backlog draining,
// at INFO, hiding the own pass's failure for as long as the seat's backlog
// lasted, and a row the broker refuses is refused every cycle
// (crewlet/crewlet#349). So an own pass's failure warns whatever its backlog
// then did, and the run-out is said of the call the bound ran out in, on what
// that call carried.
//
// THE RUN-OUT'S LEVEL IS DECIDED BY PROGRESS. A call cut after its rows moved
// a mark says so at INFO: of a backlog, the backlog draining that a seat's
// first cycles after an acquisition carry; of an own pass, a burst the seat
// wrote (crewlet/crewlet#1041), or a seat still being taken, whose hydration
// has not completed and every row of which counts as its own until it has. A
// warning every thirty seconds for that would read as a fault on every boot.
// A call that moved no mark had nothing acknowledged that stays carried and
// warns, as every seat did every cycle while the broker would not
// acknowledge. So does a seat that had the whole cycle and was cut inside a
// whole-each-cycle table, whatever it marked, since its marks say nothing of
// those tables: they keep nothing of a cut publish, so a seat whose
// counterparty profiles and skills outlast a cycle never has the rest of them
// carried past where each cycle cuts them, nor the onboarding marker after
// them, nor its backlog, which its turn reaches only after them
// (crewlet/crewlet#1040), and a whole cycle cut there is the sign of it this
// loop sees. A seat cut there with only part of a cycle is given a whole one
// next, and says so at INFO when it moved a mark. Those two cases are an own
// pass's alone: a backlog sends no table whole, and every row it carries moves
// a mark. And a turn whose own pass returned whole just as the bound ran out,
// before its backlog, is said at INFO: a warning would read a complete own
// pass as a fault.
//
// A VALUE, so what a cycle says is a fact a test reads rather than a side
// effect on the process's logger.
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
	notReached := strings.Join(r.notReached, ",")
	if r.cut.handle == "" {
		return append(out, reportLine{level: slog.LevelInfo, msg: "memory_sync_ran_out",
			args: []any{"pass", passOwn, "not_reached", notReached, "next", r.resume,
				"detail", "the cycle's bound ran out between two seats' turns; the next " +
					"cycle starts at the first seat it did not reach"}})
	}
	c := r.cut.carried
	line := reportLine{level: slog.LevelWarn, msg: "seat_memory_not_carried",
		args: []any{"seat", r.cut.handle, "pass", r.cut.pass, "table", c.StoppedIn,
			"rows", c.Rows, "marked", c.Marked, "not_reached", notReached, "next", r.resume}}
	var detail string
	switch {
	case r.cut.err == nil:
		line.level, line.msg = slog.LevelInfo, "memory_sync_ran_out"
		detail = "the cycle's bound ran out as this seat's own pass returned, before its " +
			"backlog: what the seat wrote while held here and its tables sent whole were " +
			"carried, and the rows this node held for it when it took it wait for its " +
			"next turn"
	case c.Rows == 0:
		detail = "the changelog acknowledged none of this seat's rows before the cycle's " +
			"bound ran out: a broker that is not acknowledging, a row that takes longer " +
			"to publish than the seat had, or, for a seat the cycle reached late, too " +
			"little of the cycle left"
	case r.first && c.Whole:
		detail = "this seat had the whole cycle and was cut inside a table every publish " +
			"sends whole, which keeps nothing of a cut publish: a seat whose counterparty " +
			"profiles and skills outlast a cycle never has the rest of them carried past " +
			"where each cycle cuts them, nor its onboarding marker after them, nor the " +
			"rows this node held for it when it took it, which its turn reaches only " +
			"after them (crewlet/crewlet#1040)"
	case c.Marked == 0:
		detail = "the changelog acknowledged only rows of the tables every publish sends " +
			"whole, which keep nothing of a cut publish, so this cycle carried nothing of " +
			"the seat that stays carried"
	case r.cut.pass == passOwn:
		line.level, line.msg = slog.LevelInfo, "memory_sync_ran_out"
		detail = "the cycle's bound ran out inside this seat's own pass after it moved " +
			"marks: a burst the seat wrote, such as a vector fill after a change of " +
			"embedding model (crewlet/crewlet#1041), or a seat still being taken, whose " +
			"hydration has not completed, so that every row this node holds for it " +
			"counts as its own; the seat's next turn carries on from the first row past them"
	default:
		line.level, line.msg = slog.LevelInfo, "memory_sync_ran_out"
		detail = "a backlog draining: the cycle's bound ran out inside the rows this node " +
			"held for the seat when it took it, after they moved marks, and the seat's " +
			"next turn carries on from the first row past them, after what it writes"
	}
	line.args = append(line.args, "detail", detail)
	return append(out, line)
}

// flushSeatMemory is a released seat's last turn under ctx, the flush's bound
// ([memoryFlushTimeout]), and what it says: the seat's own pass, then, while
// the bound has time, its backlog, from the marks the cycles left
// ([publishTurn]).
//
// EACH CALL IS SAID ON ITS OWN, at the level of what it lost. An own pass that
// returned an error, cut by the bound or refused, warns
// (seat_memory_not_flushed, pass=own): what it did not reach of what the seat
// wrote while held here and of its tables sent whole is lost to the seat's
// next holder. So does a backlog that failed with time left (pass=backlog). A
// backlog the bound cut, or never began because the own pass returned whole
// as the bound ran out, is said at INFO (memory_flush_ran_out, pass=backlog):
// the flush carried what the seat wrote, and ran out in the rows this node
// held for it when it took it, which the changelog mostly holds already. One
// warning for any error of the flush, as before the backlog had a call of its
// own, would warn for the backlog's tail on every handoff that came soon after
// an acquisition.
//
// A VALUE, so what a flush says is a fact a test reads rather than a side
// effect on the process's logger.
func flushSeatMemory(ctx context.Context, memory memoryPublisher, handle string) []reportLine {
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
	case turn.backlog.failed():
		out = append(out, reportLine{level: slog.LevelWarn, msg: "seat_memory_not_flushed",
			args: []any{"seat", handle, "pass", passBacklog, "table", c.StoppedIn,
				"rows", c.Rows, "marked", c.Marked, "error", turn.backlog.err,
				"detail", "the rows this node held for the seat when it took it that this " +
					"flush did not reach stay in this node's store, and are lost to the " +
					"fleet where the changelog does not hold them"}})
	}
	return out
}
