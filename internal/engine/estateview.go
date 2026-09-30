package engine

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/estate/partmap"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE ESTATE VIEW THIS NODE RUNS, AND WHAT THE ESTATE ALARMS READ OF IT.
//
// A node that evaluates the alarm table — one running the state log, whose
// retention loop is where the table is evaluated — watches the estate map the
// way every reader of it does (partmap.View: the map watched and confirmed
// every fifteen seconds, beside a watched listing of the estate leases), and
// samples it on the map maintainer's own cadence into a partmap.Watch. The
// four estate alarms are read off that: the partitions no copy can answer for,
// the one short of copies longest, the oldest join in flight, and how current
// the view itself is — by the view's OWN staleness rule (partmap.View.Staleness),
// never one restated here, so estate_view_stale fires exactly when the view
// stops being fresh.
//
// # A sample it could not see through is a break
//
// A view that is not fresh — its map or its leases last confirmed past their
// bound — is one nothing may decide from, and a sample taken through it is no
// sighting at all: the watch forgets what it had seen, and the three map
// alarms go quiet while estate_view_stale says why. A reading taken while the
// view is stale reports no finding either, whatever the last sample saw, so
// the two can never be read side by side. Keeping the runs across the gap
// would let an alarm fire on a condition nobody saw hold for its whole grace;
// losing them makes it fire a gap late, which is the direction a lower bound
// is allowed to err in.
//
// # Under layout 0
//
// There is no map, so there is nothing to be short or stalled, and the three
// map alarms never fire. The view is still run and its staleness still judged
// — the store answering that there is no map confirms the map half — and the
// fleet's presence is its roster, which layout 0's one partition is served by.
// The roster is not judged: it answers routing alone, which may use any age
// (partmap.View's doc).

// estateSampleInterval is how often the watch samples the view: the map
// maintainer's own tick (coord.ReconcileInterval, fifteen seconds), so a
// condition is seen at the resolution the map itself moves at, and a duration
// measured against membership's forty-tick grace is measured in the same ticks.
const estateSampleInterval = coord.ReconcileInterval

// estateWatch is this node's estate view and what its alarms read of it.
type estateWatch struct {
	view *partmap.View

	// budget is the operator's rejoin window — what a join is sized
	// against.
	budget time.Duration

	now func() time.Time

	mu    sync.Mutex
	watch partmap.Watch
	found partmap.Findings

	stop func()
}

// startEstateWatch builds and runs this node's estate view and its watch, once;
// a node with no coordination store has no map to watch.
//
// DETACHED from the caller's context like every loop this node owns, and
// stopped by [Engine.stopEstateWatch] in the teardown.
func (e *Engine) startEstateWatch(ctx context.Context) {
	if e.backends == nil || e.backends.Fleet == nil || e.backends.Coord == nil ||
		e.estateWatch.Load() != nil {
		return
	}
	w, err := newEstateWatch(e.boot, e.backends.Fleet, e.backends.Coord, e.dataView, LayoutZero(), time.Now)
	if err != nil {
		log.WarnContext(ctx, "estate_view_not_started", "error", err,
			"detail", "this node raises no estate alarm until it restarts")
		return
	}
	loop, cancel := context.WithCancel(context.WithoutCancel(ctx))
	var both sync.WaitGroup
	w.stop = func() {
		cancel()
		both.Wait()
	}
	// PUBLISHED BEFORE ANYTHING RUNS, so a teardown that finds it can stop
	// what it started, and a second start that loses the race started
	// nothing.
	if !e.estateWatch.CompareAndSwap(nil, w) {
		cancel()
		return
	}
	both.Add(2)
	go func() {
		defer both.Done()
		_ = w.view.Run(loop)
	}()
	go func() {
		defer both.Done()
		w.run(loop)
	}()
}

// stopEstateWatch ends the view and its watch and waits for them. Nil-safe,
// and safe to call twice.
func (e *Engine) stopEstateWatch() {
	if w := e.estateWatch.Swap(nil); w != nil && w.stop != nil {
		w.stop()
	}
}

// newEstateWatch is a watch over the estate map and leases in maps and leases,
// routing layout 0 by presence.
//
// ON THE ESTATE LEASE'S OWN TERMS, as the presence view is on the presence
// lease's: the leases renew once per heartbeat and a listing is trusted for one
// TTL, both from the TTL in force — and the view lists them at least every
// fifteen seconds whatever the heartbeat, which is what keeps it fresh on a
// healthy fleet at a long TTL (partmap.View).
func newEstateWatch(b *config.Bootstrap, maps partmap.MapSource, leases coord.Backend,
	presence *coord.LeaseView, running statelog.Layout, now func() time.Time) (*estateWatch, error) {

	ttl := effectiveLeaseTTL(b, leases)
	var roster partmap.Roster
	if presence != nil {
		roster = presenceRoster{view: presence}
	}
	view, err := partmap.NewView(partmap.ViewOptions{
		Maps: maps, Leases: leases, Running: running, Roster: roster,
		Heartbeat: memberLeaseInterval(ttl), TTL: ttl, Now: now,
	})
	if err != nil {
		return nil, fmt.Errorf("engine: watch the estate map: %w", err)
	}
	return &estateWatch{view: view, budget: b.Stream.TrackerRetention.RejoinWindow(), now: now}, nil
}

// run samples the view on [estateSampleInterval] until ctx ends.
func (w *estateWatch) run(ctx context.Context) {
	ticker := time.NewTicker(estateSampleInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.sample(w.now())
		}
	}
}

// sample takes one sighting of the map through the view — see the file's doc
// for why one taken through a view that is not fresh is a break.
func (w *estateWatch) sample(now time.Time) {
	m, _, found, err := w.view.Map()
	var live []partmap.Presence
	if err == nil && found {
		live, err = w.view.Presences()
	}
	fresh := w.view.Fresh()
	w.mu.Lock()
	defer w.mu.Unlock()
	if err != nil || !found || !fresh {
		w.watch.Forget()
		w.found = partmap.Findings{}
		return
	}
	w.found = w.watch.Observe(m, live, now)
}

// reading fills the estate alarms' half of a reading: the view's staleness by
// its own rule, and — only while the view is not stale — what the watch last
// saw through it.
func (w *estateWatch) reading(now time.Time, out *statelog.Reading) {
	out.EstateJoinBudget = w.budget
	st := w.view.Staleness(now)
	out.EstateView = &statelog.EstateViewAge{Half: st.Half, Age: st.Age, Bound: st.Bound}
	if st.Stale() {
		return
	}

	w.mu.Lock()
	f := w.found
	w.mu.Unlock()
	out.EstateUnserved = len(f.Unserved)
	out.EstateUnservedWhich = findingNames(f.Unserved, func(x partmap.Finding) string {
		return x.Partition
	})
	out.EstateShort = len(f.Short)
	if len(f.Short) > 0 {
		out.EstateShortFor = f.Short[0].For
		out.EstateShortWhich = fmt.Sprintf("%s (%d of %d copies)", f.Short[0].Partition,
			f.Short[0].Serving, f.Short[0].Wanted)
	}
	if len(f.Joining) > 0 {
		out.EstateJoiningFor = f.Joining[0].For
		out.EstateJoiningWhich = f.Joining[0].Partition + " on " + f.Joining[0].Node
	}
}

// findingNames is the first few findings by name, and how many more — the
// one line an alarm's detail carries.
func findingNames(findings []partmap.Finding, name func(partmap.Finding) string) string {
	// THREE, so an alarm's line names enough to start from and stays one
	// line however many partitions a lost node held.
	const named = 3
	var b strings.Builder
	for i, f := range findings {
		if i == named {
			fmt.Fprintf(&b, " and %d more", len(findings)-named)
			break
		}
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(name(f))
	}
	return b.String()
}

// estateReading is the estate alarms' half of this node's reading, nothing on
// a node running no estate view.
func (e *Engine) estateReading(now time.Time, out *statelog.Reading) {
	if w := e.estateWatch.Load(); w != nil {
		w.reading(now, out)
	}
}
