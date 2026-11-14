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
// every seat's own pass, what each wrote since its last one. Each cycle
// carries every held seat's own pass, what it wrote while held here and then
// its tables sent whole, before it gives the backlogs their turn: the rows an
// acquisition or a restart left this node to carry back for each seat, which
// the changelog mostly holds already ([memorySync.resume]). So what a seat
// writes waits for no backlog while the seats' own passes fit a cycle.
// Otherwise the window is longer whatever this value is. Cycles that run out
// in k seats' own passes leave a seat waiting up to 2k + 2 cycles between two
// that reach its own, so a crash can lose up to 2k + 3 cycles' writes
// ([memorySync.resume]); a seat still being taken is among the k, every row
// of it its own until its hydration completes. A seat's new rows wait behind
// a burst it wrote, too, such as a vector fill after a change of embedding
// model (crewlet/crewlet#1041). A seat whose counterparty profiles and skills
// outlast a cycle never has its onboarding marker carried, while the seats'
// backlogs still take their turn once in every lap of the seats
// (crewlet/crewlet#1040). A row larger than the broker takes fails every pass
// of its seat that meets it, at that row, so nothing after it in that pass,
// in its table or in the tables the pass carries after it, is carried
// (crewlet/crewlet#349). A graceful handoff's flush starts where the cycles
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
// thirty-second cycle carries between all the node's seats. It carries them in
// a cycle's order ([flushSeatMemory]): what the seat wrote while held here
// first, then its counterparty profiles, skills and onboarding marker, which
// every own pass sends whole, and then, while the bound has time, the rows
// this node held for the seat when it took it, its backlog, which the
// changelog mostly holds already. What it has not reached of the first two
// when it expires is lost, as a crash loses it, and that can be more than the
// last cycle's writes:
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

// backlogsTurn names the backlogs' turn among a cycle's turns, where
// [memorySync.resume] and what a cycle says (`next`, `not_reached`) name each
// seat's turn by the seat's handle: a name no handle takes, since a handle
// matches [a-z0-9][a-z0-9-]* (org.ValidHandle).
const backlogsTurn = "(backlogs)"

// memorySync is the publish loop, the handle that stops it, and the state its
// cycles share.
type memorySync struct {
	stop chan struct{}
	done sync.WaitGroup

	// resume is the turn the next cycle starts at, a seat's or the
	// backlogs' ([backlogsTurn]), "" for the first held seat's; and
	// backlogResume is the seat the next backlogs' turn starts at, "" for
	// the first held seat. Touched only by the loop's own goroutine.
	//
	// A CYCLE IS ONE LAP OVER ITS TURNS ([publishHeldMemory]): one for each
	// held seat, in sorted order, which is the seat's own pass, what it
	// wrote while held here and then its tables sent whole
	// ([memsync.Syncer.PublishOwn]), and one more after the last of them,
	// the BACKLOGS' TURN, which carries each held seat's backlog
	// ([memsync.Syncer.PublishBacklog]), the rows this node held for the
	// seat when it took it, with what the cycle has left, the seats taking
	// turns within it from backlogResume ([publishBacklogs]).
	//
	// THE RULE, over a lap's turns and over the seats within the backlogs'
	// turn alike: the next starts at the turn the bound ran out inside,
	// which had only part of a cycle, or, when that turn was the first and
	// so had the whole cycle, at the turn after it, wrapping round, so the
	// first seat's after the backlogs', a lone turn being its own next
	// ([nextStart]); at the first turn not reached when the bound ran out
	// between two; at the first when every turn was had. WITH ONE
	// EXCEPTION: a backlogs' turn the bound cut after it moved a mark hands
	// the next cycle to the first seat, wherever it stood in this one. So
	// the backlogs take a cycle's start only after a turn of theirs that
	// moved nothing while it was not the first, which had too little of the
	// cycle to move a mark, or when the cycle ran out just before their
	// turn. Without the exception, a backlog longer than a cycle, such as
	// the whole store a restart leaves a data node to carry back for each
	// seat it held, would take the start of every other cycle, and in those
	// the seats' own rows would wait behind it, which is what carrying them
	// first exists to end (crewlet/crewlet#1039). With it, while the seats'
	// own passes leave the backlogs' turn the time to move a mark, every
	// cycle starts at the first seat and carries every seat's own rows
	// before any backlog.
	//
	// THE BACKLOGS TAKE A TURN OF THEIR OWN, AND FOR GOOD, rather than what
	// each cycle has left once every seat's own pass has returned. That
	// order carries no backlog in a cycle whose own passes leave it less
	// than one round trip, and none at all while that holds cycle after
	// cycle: under a burst a seat wrote (crewlet/crewlet#1041), while seats
	// are still being taken, every row of each its own until its hydration
	// completes, or on a node whose seats' profiles and skills, which every
	// own pass sends whole, together fill a cycle (crewlet/crewlet#1040). A
	// turn in the rotation keeps the backlogs moving under any overload of
	// the own passes, whatever its kind, as the rotation keeps every seat's
	// own pass moving, so it stays once crewlet/crewlet#1040 carries the
	// profiles and skills by change sequence: it is not that issue's
	// stopgap. What it costs: under overload a cycle can start part-way
	// through the seats, and the backlogs' turn then comes before the own
	// passes of the seats after it in that cycle, which had theirs in the
	// cycle before.
	//
	// NOT THE MEMORY FILL'S RULE ([memoryFill.resume]), which starts at the
	// seat the last tick ran out at whatever its place. Here a turn that had
	// the whole cycle would then have every cycle until what it carries ran
	// dry, and every other turn would wait that long, the seats sorted
	// before it included, which a sorted start carried every cycle: at
	// 100 ms a publish, 18 000 rows are half an hour. Nor the turn after it
	// every time: a turn that ran a cycle out second would then never go
	// first, so of two seats that each need most of a cycle the second would
	// never have a whole one: its whole-each-cycle tables, which keep
	// nothing of a cut publish, would never be carried past where its cycles
	// cut them, nor the onboarding marker after them.
	//
	// WHAT IT GUARANTEES, with k the held seats whose own pass a cycle runs
	// out in, whether the pass needs more than a whole cycle (a burst,
	// profiles and skills that outlast one, a seat still being taken) or was
	// reached with less of one left than it needs: NO SEAT WAITS MORE THAN
	// 2k + 2 CYCLES between two that reach its own pass. A cycle that runs
	// out inside a turn stops there, and the next starts at that turn, which
	// then has the whole cycle, or after it when it has just had one; so
	// each turn a cycle runs out in holds the turns after it back for at
	// most two cycles before the cycles pass it, and they come back to it
	// only after reaching every other turn. The backlogs' turn is one more
	// such turn, which is the 2, and the exception only shortens what it
	// holds back: after a cut that moved a mark the cycles pass it at once.
	// A turn a cycle cut short goes first in the next, so a seat whose own
	// pass fits a whole cycle completes it at least once in every 2k + 3
	// cycles, and a seat whose own pass needs more has a whole cycle at
	// least once in every 2k + 2; and A CRASH LOSES UP TO 2k + 3 CYCLES'
	// WRITES of a seat, the cycles it waited and the one that reached it
	// ([memorySyncInterval]). The backlogs' turn waits no longer between two
	// cycles that reach it, and has the whole of the next after a cycle that
	// reached it with too little left to move a mark, or ran out just before
	// it. The bounds are reached. Of held seats a, b, c and d, in cycles
	// from b's turn where the own passes run out in c's alone, twice, and
	// the backlogs' turn moves nothing in two: b completes and the first
	// cycle runs out in c's turn, c has the whole second, d completes and
	// the third runs out in the backlogs' turn, the backlogs have the whole
	// fourth, and a, which none of them reached, waits those four: 2k + 2
	// for k = 1. A smaller bound would need a fact per turn that one resume
	// cannot hold, whether a turn cut part-way through the rotation has had
	// its whole cycle since, and denying that turn its whole cycle instead
	// is the turn-after-it rule, which starves.
	resume, backlogResume string
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
// ONE LAP OVER THE SEATS' TURNS AND THE BACKLOGS' ([memorySync.resume]): each
// held seat's own pass, what it wrote while held here and its tables sent
// whole, and then, with what the cycle has left, the backlogs' turn, the rows
// this node held for each seat when it took it. WHAT A CYCLE THE BOUND CUTS
// KEEPS: of a table with a watermark, every row the changelog acknowledged,
// and the seat's next call of that range resumes it at the first row it did
// not; of a whole-each-cycle table, nothing, and the seat's next own pass
// sends it whole again. The turns the cycle did not reach are not called on a
// bound already spent, and the next cycle starts at the turn this one ran out
// in, after it, or at the first seat, by [memorySync.resume]'s rule. A cycle
// that ran out says so in one line, naming the call it ran out in: a call
// that moved a mark is a backlog draining or a burst of the seat's own, at
// INFO, and so is a cycle that ran out between two calls; one that moved
// none, or that had the whole cycle and was cut inside a whole-each-cycle
// table, warns ([syncReport.lines]).
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

// memoryPublisher is the part of the memory syncer a cycle and a release's
// flush call: a seat's own pass and its backlog, and what the changelog
// acknowledged of each.
type memoryPublisher interface {
	PublishOwn(ctx context.Context, handle string) (memsync.Carried, error)
	PublishBacklog(ctx context.Context, handle string) (memsync.Carried, error)
}

// memoryPass names which of a seat's two publishes a call is, as every line
// about one says it (`pass`): its own pass, what the seat wrote while held
// here and its tables sent whole ([memsync.Syncer.PublishOwn]), or its
// backlog, the rows this node held for it when it took it
// ([memsync.Syncer.PublishBacklog]).
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

// failed says the call returned an error while the bound still had time: a
// refusal or a broker error, the call's own failure.
func (c passCall) failed() bool { return c.err != nil && !c.spent }

// cut says the bound ran out inside the call.
func (c passCall) cut() bool { return c.err != nil && c.spent }

// publishTurn is one seat's whole turn under ctx, its bound: the seat's own
// pass, then, while ctx has time, its backlog, whatever the own pass returned.
// It is a released seat's last turn ([flushSeatMemory]); a cycle takes the two
// apart, every held seat's own pass before the backlogs' turn
// ([publishHeldMemory]).
//
// THE OWN PASS FIRST, so what the seat wrote while held here, and then its
// profiles, skills and onboarding marker, which every own pass sends whole, go
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
			// THE BOUND RAN OUT BEFORE THIS TURN BEGAN, just as the one
			// before it returned, so the next cycle starts here. Called
			// anyway, as seats used to be, this turn and every one after it
			// failed at once, each with a warning of its own, in every cycle
			// a backlog outlasted.
			report.ranOut, report.resume, report.notReached = true, turn, lap[i:]
			report.cut.pass = passOwn
			if turn == backlogsTurn {
				report.cut.pass = passBacklog
			}
			return report
		}
		var movedMarks bool
		if turn == backlogsTurn {
			// THE SEATS HELD ARE READ AGAIN AS THE TURN BEGINS, so a seat
			// released since the lap began is not called, and one taken
			// since is: its boundary is set if its hydration has completed,
			// and otherwise its backlog carries nothing.
			backlogs := publishBacklogs(ctx, held.Held(), memory, backlogResume)
			report.backlogResume = backlogs.resume
			report.failed = append(report.failed, backlogs.failed...)
			if !backlogs.ranOut {
				continue
			}
			report.cut, report.inBacklogs = backlogs.cut, true
			report.backlogsNotReached = backlogs.notReached
			movedMarks = backlogs.marked > 0
		} else {
			// EACH CALL IS CLASSIFIED ON ITS OWN, by the bound as it
			// returned ([syncReport.lines]). A FAILURE WITH TIME LEFT, a
			// refusal or a broker error, is that call's: the seat has had
			// its turn, its marks keep what the changelog acknowledged
			// before the row that failed, and the cycle moves on without
			// touching where the next one starts.
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
		// THE BOUND RAN OUT INSIDE THIS TURN: a turn that was not the
		// cycle's first had only part of a cycle, so the next cycle starts
		// at it, and the first had the whole cycle, so the next starts at
		// the turn after it ([nextStart]). The rule's bound, and why it is
		// not the memory fill's rule: [memorySync.resume].
		report.ranOut, report.first = true, i == 0
		report.resume, report.notReached = nextStart(lap, i), lap[i+1:]
		if turn == backlogsTurn && movedMarks {
			// THE ONE EXCEPTION: a backlogs' turn the bound cut after it
			// moved a mark needs no whole cycle to move more, so the next
			// cycle starts at the first seat, and every seat's own rows go
			// before what is left of the backlogs ([memorySync.resume]).
			report.resume = ""
		}
		return report
	}
	return report
}

// publishBacklogs is a cycle's backlogs' turn under ctx, the cycle's bound:
// the backlog of each seat held names ([memsync.Syncer.PublishBacklog]), one
// after another in [cycleOrder] from resume, until every seat's has been
// called or ctx runs out, by the rule a cycle's lap follows
// ([memorySync.resume]). It reports what the turn did and where the next one
// starts.
func publishBacklogs(ctx context.Context, held []string, memory memoryPublisher,
	resume string,
) backlogsReport {
	var report backlogsReport
	order := cycleOrder(held, resume)
	for i, handle := range order {
		if ctx.Err() != nil {
			// THE BOUND RAN OUT BETWEEN TWO SEATS' BACKLOGS, as the one
			// before this returned: the next backlogs' turn starts here.
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

	// marked is the rows of every call of the turn that moved a mark, which
	// says whether a turn the bound cut moved one ([memorySync.resume]'s
	// exception): a turn that moved marks in one seat's backlog and was cut
	// in the next one's is draining, whatever the cut call itself carried.
	marked int
}

// nextStart is where a rotation over order starts next when the cycle's bound
// ran out inside its i-th turn ([memorySync.resume]): at that turn, which had
// only part of the cycle, or, when it was the first and so had the whole
// cycle, at the turn after it, wrapping round, a lone turn being its own next.
func nextStart(order []string, i int) string {
	if i == 0 {
		return order[1%len(order)]
	}
	return order[i]
}

// lapOrder is a cycle's turns in the order it takes them: one for each seat
// held, sorted as [cycleOrder] sorts them, and the backlogs' turn
// ([backlogsTurn]) after the last of them, rotated to start at resume. A
// resume the node no longer holds starts at the turn after it, which the last
// cycle did not reach either: for one that sorts after every seat still held,
// the backlogs' turn. "" starts at the first seat.
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
// rotated to start at resume, or, for a resume the node no longer holds, at
// the first seat after it. A resume that sorts after every held seat, or none,
// starts at the first.
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
	// cut is the call it ran out inside: with no handle when it ran out
	// between two turns, its pass then the pass of the turn it did not
	// begin, or between two seats' backlogs inside the backlogs' turn, which
	// inBacklogs says it ran out in. first says the turn it ran out in was
	// the cycle's first, so had the whole cycle. notReached is the turns the
	// cycle never began, in its order: the seats', by their handles, and the
	// backlogs', [backlogsTurn]. backlogsNotReached is, for a cycle that ran
	// out in the backlogs' turn, the seats whose backlog that turn never
	// called.
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

// lines is what a cycle says: a warning for each call that failed with time
// left, a seat's own pass or its backlog, and one line for a cycle whose bound
// ran out, naming the call it ran out in, where the next cycle starts (`next`,
// empty for the first seat) and the turns it never began (`not_reached`, the
// backlogs' turn written as [backlogsTurn]), which say nothing of their own;
// one that ran out in the backlogs' turn names too where that turn starts next
// and the seats whose backlog it never called (`backlogs_next`,
// `backlogs_not_reached`). Every line names the pass of the call it is about
// (`pass`): the call that failed, the call the bound ran out in, or, for a
// cycle that ran out between two calls, the first it did not make. So two
// lines about one seat are told apart.
//
// EACH CALL IS SAID ON ITS OWN, a seat's own pass and its backlog alike,
// never two merged into one: a seat whose own pass was refused with time left
// warns whatever its backlog then did, and a row the broker refuses is
// refused every cycle (crewlet/crewlet#349).
//
// THE RUN-OUT'S LEVEL IS DECIDED BY PROGRESS, by one rule for both passes. A
// call cut after its rows moved a mark says so at INFO: of a backlog, the
// backlog draining that a seat's first cycles after an acquisition carry; of
// an own pass, a burst the seat wrote (crewlet/crewlet#1041), or a seat still
// being taken, whose hydration has not completed and every row of which
// counts as its own until it has. A warning every thirty seconds for that
// would read as a fault on every boot. A call that moved no mark had nothing
// acknowledged that stays carried and warns, as every seat did every cycle
// while the broker would not acknowledge. So does a seat that had the whole
// cycle and was cut inside a whole-each-cycle table, whatever it marked,
// since its marks say nothing of those tables: they keep nothing of a cut
// publish, so a seat whose counterparty profiles and skills outlast a cycle
// never has the rest of them carried past where each cycle cuts them, nor the
// onboarding marker after them (crewlet/crewlet#1040), and a whole cycle cut
// there is the sign of it this loop sees. A seat cut there with only part of
// a cycle is given a whole one next, and says so at INFO when it moved a
// mark. Those two cases are an own pass's alone, and the rule needs no test
// of the pass for them: a backlog sends no table whole, and every row it
// carries moves a mark. And a cycle that ran out between two calls, the one
// before it whole, is said at INFO: a warning would read a complete call as a
// fault.
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
			"embedding model (crewlet/crewlet#1041), or a seat still being taken, whose " +
			"hydration has not completed, so that every row this node holds for it " +
			"counts as its own; the seat's next turn carries on from the first row past them"
	default:
		line.level, line.msg = slog.LevelInfo, "memory_sync_ran_out"
		detail = "a backlog draining: the cycle's bound ran out inside the rows this node " +
			"held for the seat when it took it, after they moved marks, and the next " +
			"backlogs' turn carries on from the first row past them, after every seat's " +
			"own pass"
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
