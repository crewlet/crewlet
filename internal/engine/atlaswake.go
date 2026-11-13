package engine

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/atlas"
	"github.com/crewlet/crewlet/internal/atlas/gather"
	"github.com/crewlet/crewlet/internal/atlaswake"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE ATLAS-WAKE DUTY, wired: the fleet singleton that reads the atlas for
// drift and wakes the agent seat that owns a drifting entry, at most once a
// UTC day (ADR-0143, internal/atlaswake).

// atlasWakeDutyName is the fleet singleton the duty claims.
const atlasWakeDutyName = atlaswake.DutyName

// atlasWakeDutyTTL is how long the duty survives without a re-claim: the
// longest pass it admits plus two ticks to renew in, as the collect duty's
// is sized from its longest visit.
const atlasWakeDutyTTL = atlaswake.PassDeadline + 2*atlaswake.Tick

// atlasDriftSources are the places a drift is read from: the claims and the
// records an entry's health is joined from. A read that misses one of them
// may be missing a drift, so a pass over it forgets nothing. Gitlet, read
// only as somebody, and the sources that say only who uses an entry are not
// among them.
var atlasDriftSources = []gather.SourceName{
	gather.SourceSettings, gather.SourceLog, gather.SourceIntegrations, gather.SourceToolServers,
	gather.SourceNodes, gather.SourceServerTests, gather.SourceAlarms,
}

// startAtlasWake arms the duty on a node with a coordination store and a
// queue; any worker may hold it, since it reads the atlas through the router.
func (e *Engine) startAtlasWake(ctx context.Context) {
	if e.backends == nil || e.backends.Fleet == nil || e.backends.Queue == nil {
		log.InfoContext(ctx, "atlas_wake_idle",
			"detail", "this node has no coordination store or no queue, so it wakes no owner of an atlas entry")
		return
	}
	store, err := atlaswake.NewCoordStore(e.backends.Fleet)
	if err != nil {
		log.ErrorContext(ctx, "atlas_wake_unavailable", "error", err)
		return
	}
	worker, err := atlaswake.New(atlaswake.Options{
		Store: store,
		Read:  e.atlasDrift,
		// THE COMPANY'S SWITCH, read per pass so turning it off applies at once.
		Enabled: func() bool {
			company := e.Company()
			return company != nil && company.Config != nil && company.Config.Atlas.WakesOwners()
		},
		TakesWork: e.seatTakesWork,
		Publish:   e.publishAtlasWake,
		ClaimDuty: e.workerDuty(atlasWakeDutyName, atlasWakeDutyTTL),
		Admits:    e.admits,
	})
	if err != nil {
		// NOT FATAL: a company whose owners are not woken still runs every seat.
		log.ErrorContext(ctx, "atlas_wake_unavailable", "error", err)
		return
	}
	e.atlasWake = worker
	// Detached, for the reason the reconcile loop is.
	worker.Start(context.WithoutCancel(ctx))
}

// stopAtlasWake ends the loop, waiting for a pass in flight, before the
// duties are released, as every loop that publishes is.
func (e *Engine) stopAtlasWake() {
	if e.atlasWake != nil {
		e.atlasWake.Stop()
	}
}

// atlasDrift reads the atlas for drift: once as nobody, for who owns what,
// and narrowed to each owning seat's own read for what is open, so a wake
// never points at what its seat is not shown. False where this node runs no
// company yet.
func (e *Engine) atlasDrift(ctx context.Context) (atlaswake.Reading, bool, error) {
	company := e.Company()
	if company == nil || company.Config == nil || company.Org == nil {
		return atlaswake.Reading{}, false, nil
	}
	deps := AtlasToolDeps(e)
	// GITLET IS READ ONLY AS SOMEBODY (ADR-0127), and the duty is nobody:
	// a Gitlet repository's drift reaches its people in Review.
	answer, running := deps.AtlasRead(ctx, nil, "", gitlet.Principal{},
		statelog.Freshness{Level: statelog.DefaultReadLevel(statelog.SurfaceSeat)}, time.Now().UTC())
	if !running {
		return atlaswake.Reading{}, false, nil
	}
	whole := true
	for _, name := range atlasDriftSources {
		whole = whole && answer.Complete(name)
	}
	agent := func(handle string) bool { return company.Org.AgentSeatByHandle(handle) != nil }
	seen := func(handle string) (atlas.View, bool) {
		seat := company.Org.AgentSeatByHandle(handle)
		if seat == nil {
			return atlas.View{}, false
		}
		view, _ := builtin.AtlasForSeat(answer.View, company.Config, seat)
		return view, true
	}
	return atlaswake.ReadingOf(answer.View, whole, agent, seen), true, nil
}

// seatTakesWork reports whether seat handle would start a turn on a wake
// now: an agent seat the chart holds, not paused, held by a node, not parked
// on its budget and not parked by its holder (a spent budget below a launch's
// floor, or every model at its limit). The four reasons a seat is stopped
// (livestate.StoppedReasons), read where each is agreed. Three-valued: an
// error is a read that could not be made.
func (e *Engine) seatTakesWork(ctx context.Context, handle string) (bool, error) {
	company := e.Company()
	if company == nil || company.Org == nil {
		return false, nil
	}
	id, ok := company.Org.AgentIDFor(company.Org.AgentSeatByHandle(handle))
	if !ok {
		return false, nil
	}
	paused, known := e.pauseOfHandle(handle)
	if !known {
		return false, fmt.Errorf("engine: whether %s is paused: %w", handle, errPausesUnread)
	}
	if paused {
		return false, nil
	}
	if e.backends != nil && e.backends.Coord != nil {
		lease, err := e.backends.Coord.Get(ctx, coord.SeatResource(id))
		if err != nil {
			return false, fmt.Errorf("engine: which node holds %s could not be read: %w", handle, err)
		}
		if lease == nil || !lease.Live(time.Now()) {
			return false, nil
		}
	}
	parked, err := e.budgetParked(ctx, company, handle)
	if err != nil {
		return false, fmt.Errorf("engine: whether %s is parked on its budget: %w", handle, err)
	}
	if parked {
		return false, nil
	}
	held, err := e.heldParks().parked(ctx, id)
	if err != nil {
		return false, fmt.Errorf("engine: whether a park holds the inbox of %s: %w", handle, err)
	}
	return !held, nil
}

// publishAtlasWake sends one wake as the inbound delivery atlaswake reads,
// the alarms' route (ADR-0095).
func (e *Engine) publishAtlasWake(ctx context.Context, req atlaswake.Request) error {
	if e.backends == nil || e.backends.Queue == nil {
		return errors.New("engine: this node has no queue to publish an atlas wake on")
	}
	w, err := atlaswake.Encode(req)
	if err != nil {
		return err
	}
	ev := events.New(w, events.NewTrace())
	ev.Source = atlaswake.Source
	return e.backends.Queue.Publish(ctx, topics.NotificationsInbound, ev)
}
