package engine

import (
	"context"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/configplane"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/estate/partmap"
	"github.com/crewlet/crewlet/internal/membership"
	"github.com/crewlet/crewlet/internal/schedule"
	"github.com/crewlet/crewlet/internal/statelog"
)

// The estate map, wired: its duty (`worker:estate-map`) and the operator's
// gestures on it (estatecontrol.go).
//
// # What the duty does under the single-file layout
//
// This build runs layout 0, where every data node holds the whole estate, and
// the estate map places nothing: [partmap.Next] creates no map for layout 0,
// so none exists and none is written. The duty still runs, on every node that
// runs workers, exactly as it will once the estate is partitioned — and on each
// tick its holder reads the live `estate:` leases, the stored map (absent) and
// the company's `estate` block, decides nothing, and writes NOTHING. The one
// record it keeps is its own duty lease, claimed per tick like every fleet
// singleton's. It paces at the tick's interval: a tick that found no map where
// none is wanted has nothing to poll for.
//
// Should a map exist — written by a node running a partitioned layout — which
// layout the fleet runs is RECORDED in it, never assumed from one node's build.
// But the first tick of a tenure that finds a map creates that map's logs
// before it moves a holder, and this build cannot create a partition log of
// any layout ([refuseEstateLogs]), so its duty leaves such a map exactly as it
// is, saying why on every tick, until a node that runs the map's layout holds
// the duty.

// estateMapDuty is the fleet singleton that maintains the estate map.
const estateMapDuty = "estate-map"

// startEstateMap arms the estate map's duty. After the node exists, which the
// duty's lease is claimed through.
func (e *Engine) startEstateMap(ctx context.Context) error {
	if e.backends == nil || e.backends.Coord == nil || e.backends.Fleet == nil {
		return nil
	}
	m, err := e.newEstateMaintainer()
	if err != nil {
		return err
	}
	l := &estateMapLoop{
		converge: m.Converge,
		live: func(ctx context.Context) ([]partmap.Presence, error) {
			return estateHolders(ctx, e.backends.Coord)
		},
		sleep: sleep, now: time.Now, poll: estateLeasePoll,
	}
	duty := mapDuty{event: "estate_map", claim: l.claimed(e.workerDuty(estateMapDuty, mapDutyTTL)),
		tick: l.ticked(estateMapTick(m)), released: m.Forget}
	e.estateMaintainer = startLoop(ctx, l.wait, duty.turn)
	return nil
}

// estateLeasePoll is how often the estate map's duty holder lists the estate
// leases between two ticks, to converge the map when one of them changes
// ([estateMapLoop.wait]).
//
// ONE SECOND, [coord.MinViewRefresh] and for its reason: one listing of one
// lease class a second is what a lease view may already cost when callers keep
// invalidating it, and it is paid on ONE node — the duty's holder — and only
// while a map exists, so never under the single-file layout. A node's word
// reaches the map on its lease's renewal, and this is how late after that the
// map answers it: a second, plus the second the listing must hold still.
const estateLeasePoll = coord.MinViewRefresh

// estateMapLoop is the estate map's duty between two ticks: while the last turn
// held the duty and found a map, it lists the estate leases every poll and,
// once their [partmap.LeaseKey] has moved and then held still for a poll, runs
// the maintainer's convergence ([partmap.Maintainer.Converge]) — so a joiner
// is routed to, and the copy it replaces let go, as soon as its lease says it
// serves rather than up to a tick later.
//
// # Debounced, and never a tick
//
// A convergence runs on a key that held still for one poll, so a fleet whose
// leases are changing — a first map's many joins starting at once — is
// converged once they settle rather than once per listing. It counts no
// absence and names no join (partmap.Converge), so however often it runs the
// tick's cadence still decides both; and it writes by compare-and-set, so one
// running as the duty moves loses its race rather than overwriting the
// successor's tick.
//
// Its fields are the LOOP GOROUTINE'S ALONE: [startLoop] runs a turn and then
// its wait on one goroutine, and nothing else reads them.
type estateMapLoop struct {
	converge func(context.Context) (bool, error)
	live     func(context.Context) ([]partmap.Presence, error)
	sleep    func(context.Context, time.Duration)
	now      func() time.Time
	poll     time.Duration

	// mapped is whether the last turn held the duty and its tick read a
	// map: what makes the wait before the next one a time to converge in.
	mapped bool
}

// claimed wraps the duty's claim so a turn that does not hold it converges
// nothing until the next that does; nil, the node with no fleet to be a
// singleton among, stays nil.
func (l *estateMapLoop) claimed(claim schedule.DutyFunc) schedule.DutyFunc {
	if claim == nil {
		return nil
	}
	return func(ctx context.Context) (bool, error) {
		mine, err := claim(ctx)
		if err != nil || !mine {
			l.mapped = false
		}
		return mine, err
	}
}

// ticked wraps the tick so the wait after it knows whether it read a map.
func (l *estateMapLoop) ticked(tick func(context.Context) (mapTick, error)) func(context.Context) (mapTick, error) {
	return func(ctx context.Context) (mapTick, error) {
		res, err := tick(ctx)
		l.mapped = err == nil && res.mapped
		return res, err
	}
}

// wait sleeps for d — the duty's pace — converging the map on each settled
// lease change inside it while the last turn held the duty and found a map.
//
// THE FIRST LISTING IS A CHANGE: the tick read the leases before it decided,
// and a lease written between that read and the first poll would otherwise go
// unanswered until the next tick. A listing that fails decides nothing, and a
// convergence that fails is said and retried on the next change.
func (l *estateMapLoop) wait(ctx context.Context, d time.Duration) {
	if !l.mapped {
		l.sleep(ctx, d)
		return
	}
	deadline := l.now().Add(d)
	var last string
	listed, moved := false, true
	for {
		left := deadline.Sub(l.now())
		if left <= 0 || ctx.Err() != nil {
			return
		}
		l.sleep(ctx, min(l.poll, left))
		if ctx.Err() != nil {
			return
		}
		live, err := l.live(ctx)
		if err != nil {
			continue
		}
		key := partmap.LeaseKey(live)
		switch {
		case !listed || key != last:
			listed, last, moved = true, key, moved || listed
		case moved:
			moved = false
			if _, err := l.converge(ctx); err != nil && ctx.Err() == nil {
				log.WarnContext(ctx, "estate_map_not_converged", "error", err)
			}
		}
	}
}

// newEstateMaintainer is the estate map's maintainer over this node's stores,
// company and layout.
func (e *Engine) newEstateMaintainer() (*partmap.Maintainer, error) {
	m, err := partmap.NewMaintainer(partmap.MaintainerOptions{
		Store: e.backends.Fleet,
		Live: func(ctx context.Context) ([]partmap.Presence, error) {
			return estateHolders(ctx, e.backends.Coord)
		},
		Company: e.estateCompany,
		// THE LAYOUT THIS BUILD RUNS, which a first map would be created
		// at — and layout 0 creates none.
		Layout:    LayoutZero(),
		Provision: refuseEstateLogs,
	})
	if err != nil {
		return nil, fmt.Errorf("engine: the estate map's maintainer: %w", err)
	}
	return m, nil
}

// refuseEstateLogs is this build's answer to "create a map's partition logs":
// it cannot, and says so.
//
// THIS BUILD RUNS LAYOUT 0, whose one partition's logs are the domains' own
// streams, created by every data node's state log as it boots; it has no stream
// shape for a partition of any other layout. It never creates a map, so the
// question is asked only of a map a node running a partitioned layout wrote —
// and the maintainer's answer to logs it cannot vouch for is to change nothing
// and say why, each tick, rather than name joiners of logs that may not exist.
// The map waits for a duty holder that runs its layout.
func refuseEstateLogs(_ context.Context, l statelog.Layout) error {
	return fmt.Errorf("engine: this node runs layout 0 and cannot create the partition "+
		"logs of layout %d; the estate map at that layout is maintained by a node that "+
		"runs it", l.Number)
}

// estateMapTick is the estate map's maintainer as its duty paces it: a map is
// awaited only where this node runs a partitioned layout.
func estateMapTick(m *partmap.Maintainer) func(context.Context) (mapTick, error) {
	return func(ctx context.Context) (mapTick, error) {
		res, err := m.Tick(ctx)
		return mapTick{mapped: res.Mapped, awaited: res.Awaited}, err
	}
}

// stopEstateMap ends the estate map duty's loop, waiting out a turn in flight.
// Nil-safe.
//
// BEFORE [Engine.releaseDuties], for the object map's reason
// ([Engine.stopObjectMap]): the loop claims its duty afresh every turn, so one
// still running past the release takes it straight back and the node exits
// holding it.
func (e *Engine) stopEstateMap() {
	if e.estateMaintainer == nil {
		return
	}
	e.estateMaintainer.stop()
	e.estateMaintainer = nil
}

// estateCompany is what the company currently in force says about the estate,
// read afresh on every tick of the map duty, stamped with the activation it
// came from — the object map's rule ([Engine.objectsCompany]), and for its
// reason: a holder a revision behind must not set the map back.
func (e *Engine) estateCompany() (membership.Company, bool) {
	c, stamp, ok := e.activatedCompany()
	if !ok {
		return membership.Company{}, false
	}
	return membership.Company{
		Epoch:         stamp,
		Replicas:      c.Config.Estate.ReplicaCount(),
		FailureDomain: c.Config.Estate.FailureDomain,
		Block:         "estate",
	}, true
}

// activatedCompany is the company in force and the activation stamp a placement
// map takes it at, and false when there is none a map may take anything from.
//
// ONE TEST FOR BOTH MAPS: the zero instant — no activation, a Tier B file a
// node booted with before its reconciler published it — stamps before the Unix
// epoch, as would any instant an unsigned epoch cannot carry without wrapping
// into one that outranks every later activation. The map takes nothing from
// either, exactly as [Engine.applyChart] writes nothing.
func (e *Engine) activatedCompany() (*Company, uint64, bool) {
	c := e.Company()
	if c == nil || c.Config == nil {
		return nil, 0, false
	}
	stamp := configplane.ActivationStamp(c.ActivatedAt)
	if stamp <= 0 {
		return nil, 0, false
	}
	return c, uint64(stamp), true
}

// estateHolders is every live estate lease, as the estate map's maintainer
// weighs it ([partmap.PresenceOf]).
//
// THE ESTATE LEASES AND NOTHING ELSE — never presence, which a drain gives up
// while the node still serves (estatelease.go). A lease whose weight cannot be
// read offers no share and is skipped; everything else it says — whether its
// store is healthy, which layout it runs — is the map's to judge, read three
// ways (partmap's package doc).
func estateHolders(ctx context.Context, leases liveLeases) ([]partmap.Presence, error) {
	held, err := leases.ListLive(ctx, coord.ClassEstate)
	if err != nil {
		return nil, err
	}
	out := make([]partmap.Presence, 0, len(held))
	for _, lease := range held {
		if p, ok := partmap.PresenceOf(lease); ok {
			out = append(out, p)
		}
	}
	return out, nil
}
