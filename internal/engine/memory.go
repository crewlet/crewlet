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
// every seat's publish, what each wrote since its last one. Otherwise the
// window is longer whatever this value is. A seat's new rows wait behind any
// backlog ahead of them: the rows an acquisition or a restart leaves this
// node to carry back (crewlet/crewlet#1039), or a burst the seat wrote, such
// as a vector fill after a change of embedding model (crewlet/crewlet#1041).
// A seat whose counterparty profiles and skills outlast a cycle never has its
// skill versions and onboarding marker carried (crewlet/crewlet#1040). A row
// larger than the broker takes fails every publish of its seat at that row,
// so nothing after it, in its table or in the tables after that one, is
// carried (crewlet/crewlet#349). And cycles that run out at k seats leave a
// seat waiting up to 2k cycles between two publishes ([memorySync.resume]). A
// graceful handoff's flush starts where the cycles left off, and what it
// cannot reach in its own bound is lost too ([memoryFlushTimeout]).
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
// thirty-second cycle carries between all the node's seats. What it has not
// reached when it expires is lost, as a crash loses it, and that is more than
// the last cycle's writes:
//
//   - while the cycles after an acquisition or a restart are still carrying
//     back every row this node holds for the seat, which everything the seat
//     wrote meanwhile waits behind (crewlet/crewlet#1039);
//   - when the seat's counterparty profiles and skills, which every publish
//     sends whole, alone outlast the five seconds: the flush is cut inside
//     them, short of the skill versions and the onboarding marker after them
//     (crewlet/crewlet#1040);
//   - behind a burst the seat wrote, such as a vector fill after a change of
//     embedding model, larger than five seconds of round trips carry
//     (crewlet/crewlet#1041).
//
// A flush that fails loses the same. One that meets a row larger than the
// broker takes fails at it, as every cycle of the seat has, so nothing after
// that row, in its table or in the tables after that one, is carried
// (crewlet/crewlet#349). And what a flush does carry of the profiles, skills
// and marker can be replaced by an older copy published after it: by this
// node's own sync, from a cycle it began before the release, or by the next
// holder while it is still taking the seat (crewlet/crewlet#511).
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
	// cycles cut them, nor the tables the registry lists after them.
	//
	// WHAT THE RULE GUARANTEES, with k the held seats a cycle runs out at,
	// whether a seat needs more than a whole cycle or was reached with less
	// of one left than its publish needs: NO SEAT WAITS MORE THAN 2k CYCLES
	// between two that reach it. A cycle that runs out at a seat stops there,
	// and the next starts at that seat, which then has the whole cycle, or
	// after it when it has just had one; so each of the k holds the seats
	// after it back for at most two cycles before the cycles pass it, and
	// they come back to it only after reaching every other seat. A seat a
	// cycle cut short goes first in the next, so a seat whose publish fits a
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
// WHAT A CYCLE THE BOUND CUTS KEEPS: of a table with a watermark, every row
// the changelog acknowledged, and the seat's next publish resumes at the
// first row it did not; of a whole-each-cycle table, nothing, and the next
// publish sends it whole again. The seats the cycle did not reach are not
// called on a bound already spent, and the next cycle starts at the seat
// this one ran out at, or at the seat after it when that seat had the whole
// cycle ([memorySync.resume]). A cycle that ran out says so in one line: a
// seat whose publish moved a mark is a backlog draining, at INFO; one that
// moved none, or that had the whole cycle and was cut inside a
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

// memoryPublisher is the part of the memory syncer a cycle calls: one seat's
// publish, and what the changelog acknowledged of it.
type memoryPublisher interface {
	Publish(ctx context.Context, handle string) (memsync.Carried, error)
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

// publishHeldMemory is one cycle over the seats held names: each seat's
// publish, one after another in [cycleOrder] from resume, until every seat has
// been called or ctx, the cycle's bound, runs out. It reports what the cycle
// did and where the next one starts.
func publishHeldMemory(ctx context.Context, held heldHandles, memory memoryPublisher,
	resume string,
) syncReport {
	var report syncReport
	order := cycleOrder(held.Held(), resume)
	for i, handle := range order {
		if ctx.Err() != nil {
			// THE BOUND RAN OUT BEFORE THIS SEAT WAS REACHED, just as the
			// seat before it returned, so the next cycle starts here.
			// Called anyway, as they used to be, this seat and every one
			// after it failed at once, each with a warning of its own, in
			// every cycle a backlog outlasted.
			report.ranOut, report.resume, report.notReached = true, handle, order[i:]
			return report
		}
		carried, err := memory.Publish(ctx, handle)
		if err == nil {
			continue
		}
		if ctx.Err() == nil {
			// A FAILURE WITH TIME LEFT, a refusal or a broker error: the
			// seat has had its publish, its marks keep what the changelog
			// acknowledged before the row that failed, and the cycle
			// moves on without touching where the next one starts.
			report.failed = append(report.failed,
				seatPublish{handle: handle, carried: carried, err: err})
			continue
		}
		// THE BOUND RAN OUT INSIDE THIS SEAT'S PUBLISH. A seat that was
		// not the cycle's first had only part of a cycle, so the next
		// cycle starts at it; the first had the whole cycle, so the next
		// starts at the seat after it, wrapping round, a lone seat being
		// its own next. The rule's bound, no seat waiting more than 2k
		// cycles for k seats the cycles run out at, and why it is not the
		// memory fill's rule: [memorySync.resume].
		report.ranOut, report.first = true, i == 0
		report.cut = seatPublish{handle: handle, carried: carried, err: err}
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

	// failed is every seat whose publish failed while the cycle still had
	// time, with what the changelog acknowledged before the failure.
	failed []seatPublish

	// ranOut says the cycle's bound ran out before every seat's publish
	// had returned. cut is the seat it ran out inside, with no handle when
	// it ran out between two seats, and first says that seat was the
	// cycle's first, so had the whole cycle. notReached is the seats the
	// cycle never called, in its order.
	ranOut     bool
	cut        seatPublish
	first      bool
	notReached []string
}

// seatPublish is one seat's publish in a cycle: what the changelog
// acknowledged, and the error the publish ended on.
type seatPublish struct {
	handle  string
	carried memsync.Carried
	err     error
}

// log says what a cycle did ([syncReport.lines]).
func (r syncReport) log(ctx context.Context) {
	for _, line := range r.lines() {
		log.Log(ctx, line.level, line.msg, line.args...)
	}
}

// lines is what a cycle says: a warning for each seat whose publish failed
// with time left, and one line for a cycle whose bound ran out, naming the
// seats it did not reach, which say nothing of their own.
//
// THE RUN-OUT'S LEVEL IS DECIDED BY PROGRESS. A seat cut after its rows moved
// a mark is a backlog draining, which is what a seat's first cycles after an
// acquisition are, and says so at INFO: a warning every thirty seconds for
// that would read as a fault on every boot. A seat that moved no mark had
// nothing acknowledged that stays carried and warns, as every seat did every
// cycle while the broker would not acknowledge. So does a seat that had the
// whole cycle and was cut inside a whole-each-cycle table, whatever it
// marked, since its marks say nothing of those tables: they keep nothing of a
// cut publish, so a seat whose counterparty profiles and skills outlast a
// cycle never has the rest of them carried past where each cycle cuts them,
// nor the skill versions and onboarding marker after them
// (crewlet/crewlet#1040), and a whole cycle cut there is the sign of it this
// loop sees. A seat cut there with only part of a cycle is given a whole one
// next, and says so at INFO when it moved a mark.
//
// A VALUE, so what a cycle says is a fact a test reads rather than a side
// effect on the process's logger.
func (r syncReport) lines() []reportLine {
	var out []reportLine
	for _, failed := range r.failed {
		c := failed.carried
		out = append(out, reportLine{level: slog.LevelWarn, msg: "seat_memory_not_carried",
			args: []any{"seat", failed.handle, "table", c.StoppedIn, "rows", c.Rows,
				"marked", c.Marked, "error", failed.err}})
	}
	if !r.ranOut {
		return out
	}
	notReached := strings.Join(r.notReached, ",")
	if r.cut.handle == "" {
		return append(out, reportLine{level: slog.LevelInfo, msg: "memory_sync_ran_out",
			args: []any{"not_reached", notReached, "next", r.resume,
				"detail", "the cycle's bound ran out between two seats' publishes; the " +
					"next cycle starts at the first seat it did not reach"}})
	}
	c := r.cut.carried
	line := reportLine{level: slog.LevelWarn, msg: "seat_memory_not_carried",
		args: []any{"seat", r.cut.handle, "table", c.StoppedIn, "rows", c.Rows,
			"marked", c.Marked, "not_reached", notReached, "next", r.resume}}
	var detail string
	switch {
	case c.Rows == 0:
		detail = "the changelog acknowledged none of this seat's rows before the cycle's " +
			"bound ran out: a broker that is not acknowledging, a row that takes longer " +
			"to publish than the seat had, or, for a seat the cycle reached late, too " +
			"little of the cycle left"
	case r.first && c.Whole:
		detail = "this seat had the whole cycle and was cut inside a table every publish " +
			"sends whole, which keeps nothing of a cut publish: a seat whose counterparty " +
			"profiles and skills outlast a cycle never has the rest of them carried past " +
			"where each cycle cuts them, nor its skill versions and onboarding marker " +
			"after them (crewlet/crewlet#1040)"
	case c.Marked == 0:
		detail = "the changelog acknowledged only rows of the tables every publish sends " +
			"whole, which keep nothing of a cut publish, so this cycle carried nothing of " +
			"the seat that stays carried"
	default:
		line.level, line.msg = slog.LevelInfo, "memory_sync_ran_out"
		detail = "a backlog draining: the cycle's bound ran out inside this seat's " +
			"publish after it moved marks, and the seat's next publish carries on " +
			"from the first row past them"
	}
	line.args = append(line.args, "detail", detail)
	return append(out, line)
}
