package upkeep

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/objstore/placement"
)

var log = logging.Get("objstore")

// OutGrace is how long a member may be gone before the map stops placing
// objects on it.
//
// TEN MINUTES, the interval Ceph waits before marking a down device out, for
// the same reason: a restart, a reboot and a rolling upgrade's turn at a node
// are all minutes, and taking a member out moves its whole share across the
// fleet — traffic that is pure waste for a node about to come back, and that
// competes with everything else the fleet is doing. Past it, the risk that a
// second failure finds a chunk one copy short outweighs the cost of the copy.
// Until then a displaced write keeps its replica count by landing on the next
// member of the ranking, so a short absence costs no durability.
const OutGrace = 10 * time.Minute

// Presence is one live data node as the maintainer sees it: its id and the
// share of the objects it offers to hold.
type Presence struct {
	Node   string
	Weight int
}

// Next is the map that should follow state, given who is live now.
//
// PURE, so the rules are tested without a store:
//
//   - A live node offering a weight is a member at that weight — a new one is
//     added, a changed weight is taken.
//   - A member that is not live is noted absent from now, and removed once it
//     has been absent for [OutGrace]; one that is live again is no longer
//     absent.
//   - The replica count is the one asked for.
//
// Any change to who holds what moves [placement.Map.Epoch]; a change to the
// absences alone does not. changed reports whether there is anything to
// write at all.
func Next(state objstore.MapState, live []Presence, replicas int, now time.Time) (objstore.MapState, bool) {
	weights := map[string]int{}
	for _, m := range state.Map.Members {
		weights[m.Node] = m.Weight
	}
	absent := maps.Clone(state.Absent)
	if absent == nil {
		absent = map[string]time.Time{}
	}
	seen := map[string]bool{}
	placementChanged := false
	for _, p := range live {
		if p.Node == "" || p.Weight < 1 {
			continue
		}
		weight := min(p.Weight, placement.MaxWeight)
		seen[p.Node] = true
		delete(absent, p.Node)
		if weights[p.Node] != weight {
			weights[p.Node] = weight
			placementChanged = true
		}
	}
	for node := range weights {
		if seen[node] {
			continue
		}
		since, noted := absent[node]
		switch {
		case !noted:
			absent[node] = now
		case now.Sub(since) >= OutGrace:
			delete(weights, node)
			delete(absent, node)
			placementChanged = true
		}
	}
	// AN ABSENCE FOR A NODE THAT IS NO LONGER A MEMBER is nothing to
	// remember.
	for node := range absent {
		if _, member := weights[node]; !member {
			delete(absent, node)
		}
	}
	replicas = max(replicas, 1)
	if replicas != state.Map.Replicas {
		placementChanged = true
	}

	next := objstore.MapState{Map: placement.Map{Epoch: state.Map.Epoch, Replicas: replicas}}
	for _, node := range slices.Sorted(maps.Keys(weights)) {
		next.Map.Members = append(next.Map.Members, placement.Member{Node: node, Weight: weights[node]})
	}
	if len(absent) > 0 {
		next.Absent = absent
	}
	if placementChanged {
		next.Map.Epoch++
	}
	return next, placementChanged || !maps.Equal(absent, state.Absent)
}

// MapStore is what the maintainer needs from the coordination store.
type MapStore interface {
	ObjectMap(ctx context.Context) (coord.ObjectMapRecord, bool, error)
	CreateObjectMap(ctx context.Context, value []byte) (coord.ObjectMapRecord, bool, error)
	UpdateObjectMap(ctx context.Context, value []byte, version uint64) (coord.ObjectMapRecord, bool, error)
}

// Observer is told every map the maintainer wrote, so its own node places by
// it at once.
type Observer interface {
	Observe(state objstore.MapState, version uint64)
}

// MaintainerOptions are a maintainer's dependencies.
type MaintainerOptions struct {
	Store MapStore

	// Live answers every live data node offering to hold objects.
	Live func(ctx context.Context) ([]Presence, error)

	// Replicas is how many members should hold each object.
	Replicas int

	// Observer, when set, is told every map written.
	Observer Observer

	// Now is the clock absences are measured on, injected for tests.
	Now func() time.Time
}

// Maintainer keeps the stored map in step with the fleet. A FLEET SINGLETON:
// the caller runs [Maintainer.Tick] only while it holds the duty, and every
// write is a compare-and-set, so a tick that lost the duty mid-write loses
// the race rather than overwriting its successor.
type Maintainer struct {
	opts MaintainerOptions
}

// NewMaintainer builds a maintainer.
func NewMaintainer(opts MaintainerOptions) (*Maintainer, error) {
	if opts.Store == nil || opts.Live == nil {
		return nil, errors.New("objstore/upkeep: a maintainer needs a store and a roster")
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Maintainer{opts: opts}, nil
}

// errNewerMap is a stored map this build cannot read, which it must not
// overwrite: a newer build wrote it, and a map rewritten in an older shape
// would drop whatever that build added.
var errNewerMap = errors.New("objstore/upkeep: the stored map is one this build cannot read")

// Tick brings the stored map up to date, once.
func (m *Maintainer) Tick(ctx context.Context) error {
	live, err := m.opts.Live(ctx)
	if err != nil {
		return fmt.Errorf("objstore/upkeep: read the live data nodes: %w", err)
	}
	rec, found, err := m.opts.Store.ObjectMap(ctx)
	if err != nil {
		return fmt.Errorf("objstore/upkeep: read the placement map: %w", err)
	}
	var state objstore.MapState
	if found {
		if state, err = objstore.DecodeMapState(rec.Value); err != nil {
			return fmt.Errorf("%w: %w", errNewerMap, err)
		}
		if m.opts.Observer != nil {
			m.opts.Observer.Observe(state, rec.Version)
		}
	}
	next, changed := Next(state, live, m.opts.Replicas, m.opts.Now().UTC())
	if !changed || (!found && len(next.Map.Members) == 0) {
		// NO MAP IS WRITTEN FOR A FLEET WITH NO MEMBERS: an empty one
		// would say the same thing as none, and cost a write per tick
		// for nothing.
		return nil
	}
	raw, err := next.Encode()
	if err != nil {
		return err
	}
	var wrote coord.ObjectMapRecord
	var won bool
	if found {
		wrote, won, err = m.opts.Store.UpdateObjectMap(ctx, raw, rec.Version)
	} else {
		wrote, won, err = m.opts.Store.CreateObjectMap(ctx, raw)
	}
	if err != nil {
		return fmt.Errorf("objstore/upkeep: write the placement map: %w", err)
	}
	if !won {
		// A LOST RACE: another holder wrote first. The next tick reads
		// what it wrote and starts from there.
		return nil
	}
	if next.Map.Epoch != state.Map.Epoch {
		log.InfoContext(ctx, "object_map_changed", "epoch", next.Map.Epoch,
			"members", len(next.Map.Members), "replicas", next.Map.Replicas)
	}
	if m.opts.Observer != nil {
		m.opts.Observer.Observe(next, wrote.Version)
	}
	return nil
}
