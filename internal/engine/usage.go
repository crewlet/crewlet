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

// The usage domain's writer: the loop that turns this node's own event log into
// the replicated record of what its seats, its schedules and the people its
// auxiliary model spent for did each day.
//
// Every data node runs one, and only its own; unlike the embedding duty it is
// no fleet singleton. A data node's event log holds only what ITS seats did and
// what it keeps in custody for a node without `data` (internal/observe), so only
// it can derive its day, and the node in every subject it writes keeps N writers
// from meeting on one. A node without `data` runs none: its keeper derives and
// publishes its day. Without the publisher the domain replicates an empty
// stream, every spend answer is partial, and every surface reports a healthy log.

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
		// Resolved on every read ([Engine.usageEstate]), and THIS NODE'S
		// OWN COPY, never the router's: a day is joined with the rows this
		// node's applier wrote, whose readiness is [stateLog.usageCaught].
		Estate: e.usageEstate(),
		NodeID: s.nodeID,
		Caught: s.usageCaught,
		// A rise is a reanchor of some node's copy, which owes every day
		// again ([stateLog.usageGenerations]).
		Generations: s.usageGenerations,
		// The clock and the chart are read per tick: an apply that moves
		// the company's timezone moves the next day's cut with it.
		Zone:   e.Zone,
		Handle: e.seatHandle,
		Every:  e.usageEvery,
		// The sweep's retention for the dispatch ledger: a re-derived day
		// the sweep may have cut into is published without its schedules,
		// so a short schedule-day never replaces a whole one.
		FireHistory: maintenance.ScheduledRunRetention,
		// No logger: the publisher's lines carry `component=usage`.
	})
	if err != nil {
		log.WarnContext(ctx, "usage_publisher_unbuilt", "err", err,
			"detail", "this node's spend, turns and reads do not replicate, "+
				"so fleet-wide history omits this node until it restarts")
		return
	}
	// DETACHED, as the embedding duty is: a loop bound to a signal context
	// stops at SIGTERM, before the appliers it publishes into.
	loop, stop := context.WithCancel(context.WithoutCancel(ctx))
	u := &usageLoop{stop: stop, done: make(chan struct{}), log: s}
	e.usage = u
	// What the usage applier's first drain wakes ([stateLog.onDrained]), at
	// boot and after a reanchor relaunches it.
	s.usagePublisher.Store(publisher)
	go func() {
		defer close(u.done)
		publisher.Run(loop)
	}()
}

// onDrained is what one domain's applier tells on its first drain after it
// starts (statelog.RunnerDeps.OnDrained): the snapshot loop for every domain,
// and for the usage log the usage publisher too, which derives no day until
// that applier has caught up (usage.PublisherDeps.Caught) and, left to its next
// tick, would wait a whole interval after a relaunch or a reanchor.
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
// the usage log's generation on every data node that runs it: the one place a
// node's reanchor of its copy is visible to the others, since the usage log
// keeps no generation record (internal/usage's republish.go).
//
// THIS NODE'S OWN FROM ITS RUNNER, never its row: a reanchor reads the register
// before writing it, so the row can still name the generation just left, and
// this node's own rise is the one its publisher must see at once.
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

// usageCaught reports whether THIS node's usage applier, and no other domain's,
// has reached the end of its log since it started, read per tick off the
// domain's current runner (usage.PublisherDeps.Caught): a day is joined with
// the rows this node applied, which right after a restore lag what was
// published since. A node running no usage log has no rows to join with.
func (s *stateLog) usageCaught() bool {
	r := s.Domain(usage.Domain{}.Name())
	return r != nil && r.runner != nil && r.runner.Drained()
}

// stopUsage ends the loop, waiting for an in-flight flush and the final one
// ([usage.Publisher.Run], today and yesterday, bounded by
// [usage.FinalFlushBudget]).
//
// The teardown runs it after every writer of a row this node's day counts (the
// learning passes, the scheduler, Gitlet and its unit-repository reads, the
// auxiliary-spend ledger's last flush, the custody keeper leaving its group)
// and before the trim and the core's logs
// (TestTheDutiesStopBeforeTheirLeasesAndTheLogs): the final day is then the
// node's last, and the log it publishes into is still applied, so the day
// reaches the fleet even if the node never starts again.
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

// UsageReads is the usage domain's reads, the source of the spend, activity
// and skill-reader answers (ADR-0020), through this node's estate router as
// its tracker and knowledge-base reads are ([OperatorWork]): a data node whose
// usage copy is out of service answers from a peer's (ADR-0079).
func (e *Engine) UsageReads() estate.Usage { return e.router.Usage() }

// usageEstate is the replicated estate as this node's own usage reads need it:
// the backend its router answers from in-process, and the rows its publisher
// joins a day with.
//
// RESOLVED ON EVERY READ, never captured: an adoption closes and reopens the
// file, and a captured handle would answer from a file no longer at that name.
// An estate not open here answers [store.ErrNoEstate], which the query surface
// reports as "not available yet".
func (e *Engine) usageEstate() usage.Estate {
	if e.backends == nil || e.backends.Store == nil {
		return store.ReplicatedReader{}
	}
	return e.backends.Store.Replicated().Reader()
}
