package engine

import (
	"context"
	"fmt"

	"github.com/crewlet/crewlet/internal/configplane"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/estate/partmap"
	"github.com/crewlet/crewlet/internal/membership"
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
// Should a map exist — written by a node running a partitioned layout — the
// duty maintains it whatever layout this node runs: which layout the fleet
// runs is RECORDED in the map, never assumed from one node's build, and this
// node's own lease, at layout 0, counts it as a member that cannot hold that
// map's partitions.

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
	duty := mapDuty{event: "estate_map", claim: e.workerDuty(estateMapDuty, mapDutyTTL),
		tick: estateMapTick(m)}
	e.estateMaintainer = startLoop(ctx, sleep, duty.turn)
	return nil
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
		// at — and layout 0 creates none, so no provisioner is needed:
		// there are no partition logs to create before a map that is
		// never written.
		Layout: LayoutZero(),
	})
	if err != nil {
		return nil, fmt.Errorf("engine: the estate map's maintainer: %w", err)
	}
	return m, nil
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
