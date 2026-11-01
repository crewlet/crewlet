package engine

import (
	"context"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/estate"
	"github.com/crewlet/crewlet/internal/maintenance"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/usage"
)

// The usage domain's writer, armed — the loop that turns this node's own event
// log into the replicated record of what its seats, its schedules and the
// people its auxiliary model spent for did each day.
//
// # Every data node runs one, and no node runs anybody else's
//
// Unlike the embedding duty beside it this is not a fleet singleton: a data
// node's event log holds only what ITS seats did and the events it keeps in
// custody for a node without `data` (internal/observe), so the node is the only
// thing that can derive its own day, and the node in every subject it writes
// is what keeps N writers from ever meeting on one. A node without `data` keeps
// no event log of its own, so its seats' day is derived, and published, inside
// its keeper's, and it runs no publisher at all. Without it the domain is
// registered, applied and replicated over an empty stream — and every spend
// answer is exactly as partial as it was before the domain existed, while
// every surface reports a healthy log.

// usageLoop is one node's usage publisher and the goroutine running it.
type usageLoop struct {
	stop context.CancelFunc
	done chan struct{}

	// log is the state log the publisher was armed on, whose
	// [stateLog.usagePublisher] the stop clears.
	log *stateLog
}

// startUsage arms this node's usage publisher, or does nothing on a node that
// runs no state log — a node without `data` among them.
func (e *Engine) startUsage(ctx context.Context, s *stateLog) {
	if s == nil || e.backends == nil || e.backends.Store == nil {
		return
	}
	running := s.Domain(usage.Domain{}.Name())
	if running == nil || running.publisher == nil {
		return
	}
	publisher, err := usage.NewPublisher(usage.PublisherDeps{
		Store: e.backends.Store,
		Log:   running.publisher,
		// RESOLVED ON EVERY READ ([Engine.usageEstate]): an adoption
		// replaces the file under the handle this node booted with. THIS
		// NODE'S OWN COPY, never the router's: the rows a day is joined
		// with are the ones this node's applier wrote, which
		// [stateLog.usageCaught] is the readiness of.
		Estate: e.usageEstate(),
		NodeID: s.nodeID,
		// WHETHER THIS NODE'S USAGE APPLIER HAS REACHED THE END OF ITS LOG
		// ([stateLog.usageCaught]).
		Caught: s.usageCaught,
		// THE FLEET'S GENERATIONS OF THE LOG, as the heartbeat read the
		// register ([stateLog.usageGenerations]): a rise is a reanchor of
		// some node's copy, which owes every day again.
		Generations: s.usageGenerations,
		// THE CLOCK AND THE CHART ARE READ PER TICK, from whichever epoch
		// is current: an apply that moves the company's timezone moves
		// the next day's cut with it.
		Zone:   e.Zone,
		Handle: e.seatHandle,
		Every:  e.usageEvery,
		// THE SWEEP'S OWN RETENTION for the dispatch ledger: a day the
		// publisher derives again that the sweep may have cut into is
		// published without its schedules, so a short schedule-day never
		// replaces a whole one.
		FireHistory: maintenance.ScheduledRunRetention,
		// NO LOGGER: the publisher's own lines carry `component=usage`,
		// the subsystem that wrote them, rather than this package's.
	})
	if err != nil {
		log.WarnContext(ctx, "usage_publisher_unbuilt", "err", err,
			"detail", "this node's spend, turns and reads do not replicate, "+
				"so fleet-wide history omits this node until it restarts")
		return
	}
	// DETACHED from the caller's context, for the embedding duty's reason:
	// a loop bound to a signal context stops at SIGTERM, a moment before
	// the appliers it publishes into.
	loop, stop := context.WithCancel(context.WithoutCancel(ctx))
	u := &usageLoop{stop: stop, done: make(chan struct{}), log: s}
	e.usage = u
	// WHAT THE USAGE APPLIER'S FIRST DRAIN WAKES ([stateLog.onDrained]) —
	// at boot, and after a reanchor of the usage log relaunches it.
	s.usagePublisher.Store(publisher)
	go func() {
		defer close(u.done)
		publisher.Run(loop)
	}()
}

// onDrained is what one domain's applier tells on its first drain after it
// starts (statelog.RunnerDeps.OnDrained): the snapshot loop, for every domain —
// and, for the usage log, the usage publisher too, which derives no day until
// that applier has caught up (usage.PublisherDeps.Caught). Told only by its
// next tick, a publisher whose applier had just been relaunched — at boot, and
// after the reanchor whose pass over every day it is waiting to publish —
// waited out a whole interval for a precondition that had already ended.
func (s *stateLog) onDrained(domain statelog.Domain) func() {
	if domain.Name() != (usage.Domain{}).Name() {
		return s.nudgeSnapshot
	}
	return func() {
		s.nudgeSnapshot()
		if p := s.usagePublisher.Load(); p != nil {
			p.Nudge()
		}
	}
}

// usageGenerations is the usage log's generation on every data node as the
// heartbeat last read the positions register (usage.PublisherDeps.Generations),
// and false before it has read it once.
func (s *stateLog) usageGenerations() (usage.Generations, bool) {
	g := s.usageSeen.Load()
	if g == nil {
		return nil, false
	}
	return *g, true
}

// observeUsageGenerations records, from one reading of the positions register,
// the usage log's generation on every data node that runs it — the one place a
// node's reanchor of its own copy of that log is visible to the others, since
// the usage log keeps no generation record (internal/usage's republish.go).
//
// THIS NODE'S OWN FROM ITS RUNNER, never from its row: a reanchor publishes the
// row by reading the register first and writing after, so the row read here can
// still name the generation the reanchor just left — and this node's own rise
// is the one its publisher must see at once.
func (s *stateLog) observeUsageGenerations(rows []coord.NodePositions) {
	name := usage.Domain{}.Name()
	running := s.Domain(name)
	if running == nil {
		return
	}
	g := usage.Generations{}
	for _, row := range rows {
		if at, runs := row.Domains[name]; runs {
			g[row.NodeID] = at.Generation
		}
	}
	g[s.nodeID] = running.runner.Committed().Generation
	s.usageSeen.Store(&g)
}

// usageCaught reports whether THIS node's usage applier — that domain's runner
// and no other's — has reached the end of its log since it started, read per
// tick off whichever runner the domain runs now (usage.PublisherDeps.Caught):
// every day a node publishes is joined with the rows it applied, which right
// after a restore are the copy's until it has replayed what was published
// since, so no day is derived until it has. Another domain's applier drained
// says nothing about this one's rows, and a node running no usage log has none
// to join with.
func (s *stateLog) usageCaught() bool {
	r := s.Domain(usage.Domain{}.Name())
	return r != nil && r.runner != nil && r.runner.Drained()
}

// stopUsage ends the loop, waiting for an in-flight flush and for the last
// one the loop runs as it ends ([usage.Publisher.Run], today and yesterday,
// bounded by [usage.FinalFlushBudget]).
//
// ORDERED BY THE TEARDOWN after every writer of a row this node's day counts —
// the learning passes, the scheduler, the code host and the reads of unit
// repositories it serves, the auxiliary-spend ledger's last flush, the custody
// keeper leaving its group — and before the
// trim and long before the core's logs (TestTheDutiesStopBeforeTheirLeasesAndTheLogs).
// Both halves are what that last flush needs: the day it derives is the
// node's last, with nothing written after it, and the usage log it publishes
// into is still applied, so it resolves in milliseconds and the end of this
// node's day reaches the fleet even if the node never starts again.
func (e *Engine) stopUsage() {
	if e.usage == nil {
		return
	}
	e.usage.log.usagePublisher.Store(nil)
	e.usage.stop()
	<-e.usage.done
	e.usage = nil
}

// seatHandle names a seat by its derived id from the current chart, and false
// for an id the chart no longer holds.
func (e *Engine) seatHandle(agentID string) (string, bool) {
	c := e.Company()
	if c == nil || c.Org == nil {
		return "", false
	}
	id, err := uuid.Parse(agentID)
	if err != nil {
		return "", false
	}
	seat := c.Org.AgentSeatByID(id)
	if seat == nil {
		return "", false
	}
	return seat.Handle(), true
}

// UsageReads is the usage domain's reads as the operator's surfaces reach them
// — the spend, activity and skill-reader answers' source (ADR-0020) — through
// this node's estate router, as its tracker and knowledge-base reads are
// ([OperatorWork]): a data node whose usage copy is in service answers from
// it, and one whose copy is out of service answers from a peer's rather than
// from the copy it stopped serving (ADR-0072).
func (e *Engine) UsageReads() estate.Usage { return e.router.Usage() }

// usageEstate is the replicated estate as this node's own reader of the usage
// domain's rows needs it — the backend its router answers from in-process, and
// the rows its own publisher joins a day with.
//
// RESOLVED ON EVERY READ through the node handle, never captured: an adoption
// closes the file and reopens it, and a reader holding the handle it booted
// with would answer every named spend window from a file no longer at that
// name. An estate that is not open here answers [store.ErrNoEstate], which
// the query surface reports as "not available yet".
func (e *Engine) usageEstate() usage.Estate {
	if e.backends == nil || e.backends.Store == nil {
		return store.ReplicatedReader{}
	}
	return e.backends.Store.Replicated().Reader()
}
