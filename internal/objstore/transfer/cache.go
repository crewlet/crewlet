package transfer

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/objstore/placement"
)

// MapReader is what the cache needs from the coordination store.
type MapReader interface {
	ObjectMap(ctx context.Context) (coord.ObjectMapRecord, bool, error)
}

// CacheInterval is how often a node re-reads the placement map.
//
// TEN SECONDS: a map changes when a data node joins or after one has been gone
// for the maintainer's grace, both of them minutes-scale events, and placing
// by the previous map for ten seconds is correct rather than merely tolerable
// — every chunk a newer map moves stays where the older map put it until a
// member the newer map names has a copy (see the collector). One small read
// per node per interval is the whole cost.
//
// It also bounds one read: a read that has not answered by the time the next
// one is due is superseded by it, and it holds the lock every install takes.
const CacheInterval = 10 * time.Second

// Cache holds the placement map this node places by — and its LAYOUT, every
// group's holders computed once when the map is installed, which is what the
// server and the client place each chunk by.
//
// # Which map wins
//
// A map with a higher version than the one held replaces it; a lower or equal
// one is a slow read and never rolls a fast one back. A map of a different
// GENERATION replaces it whatever its version: the key was lost and written
// again, the store's versions started again with it, and a cache that
// compared them would keep the lost map until the new one's version overtook
// the old — for ever, if the old had been rewritten often. Every install is
// serialised under one lock, and so is every read of the store with the
// install that follows it, so two refreshes can never install out of order.
//
// What is NOT ordered is the maintainer's own read at the start of its tick,
// handed in through [Cache.Observe] after a refresh may have installed a later
// map: across one generation the version orders them, and across two it would
// take a second maintainer recreating the key during the first one's tick —
// two holders of a singleton duty at once, while the stored key is lost — and
// even then the next refresh, ten seconds on, installs the stored map again.
type Cache struct {
	store MapReader

	// installing serialises every install, and each refresh's read with
	// the install it leads to.
	installing sync.Mutex

	// current is what readers place by, swapped whole so a reader never
	// sees a map beside another map's layout.
	current atomic.Pointer[installed]
}

// installed is one map as the cache holds it.
type installed struct {
	state   objstore.MapState
	version uint64
	layout  *placement.Layout
}

// NewCache builds an empty cache; [Cache.Refresh] or [Cache.Run] fills it.
func NewCache(store MapReader) *Cache { return &Cache{store: store} }

// Current is the map to place by, and false before one was ever read.
func (c *Cache) Current() (placement.Map, bool) {
	in := c.current.Load()
	if in == nil {
		return placement.Map{}, false
	}
	return in.state.Map, true
}

// Layout is the current map's layout, and false before one was ever read. It
// is shared: read it, never change it.
func (c *Cache) Layout() (*placement.Layout, bool) {
	in := c.current.Load()
	if in == nil {
		return nil, false
	}
	return in.layout, true
}

// State is the whole stored record and the version it was read at.
func (c *Cache) State() (objstore.MapState, uint64, bool) {
	in := c.current.Load()
	if in == nil {
		return objstore.MapState{}, 0, false
	}
	return in.state, in.version, true
}

// Refresh reads the stored map. A map this build cannot read leaves the one it
// had in place and answers the error: placing by the last map it understood is
// correct for as long as it takes to upgrade, where placing by nothing would
// stop every upload on the node.
func (c *Cache) Refresh(ctx context.Context) error {
	c.installing.Lock()
	defer c.installing.Unlock()
	read, cancel := context.WithTimeout(ctx, CacheInterval)
	defer cancel()
	rec, found, err := c.store.ObjectMap(read)
	if err != nil || !found {
		return err
	}
	state, err := objstore.DecodeMapState(rec.Value)
	if err != nil {
		return err
	}
	c.install(state, rec.Version)
	return nil
}

// Observe installs a map this node wrote or read, unless the cache already
// holds a later one of the same generation — so the maintainer's own write is
// placed by at once rather than a refresh later, and a slow read never rolls
// back a fast one. See the type's doc for which map wins.
func (c *Cache) Observe(state objstore.MapState, version uint64) {
	c.installing.Lock()
	defer c.installing.Unlock()
	c.install(state, version)
}

// install replaces the held map when the offered one wins. The caller holds
// installing.
//
// THE LAYOUT IS KEPT when only the record around the map changed — an absence
// counted, a measurement taken, which is most of what the maintainer writes —
// because computing one is a fifth of a second at two hundred members.
func (c *Cache) install(state objstore.MapState, version uint64) {
	held := c.current.Load()
	if held != nil && state.Map.Generation == held.state.Map.Generation && version <= held.version {
		return
	}
	var layout *placement.Layout
	if held != nil && held.state.Map.Equal(state.Map) {
		layout = held.layout
	} else {
		layout = state.Map.Layout()
	}
	if held != nil && held.state.Map.Generation != state.Map.Generation {
		log.Warn("object_map_generation_changed", "was", held.state.Map.Generation.String(),
			"now", state.Map.Generation.String(), "epoch", state.Map.Epoch,
			"detail", "the stored placement map was written again from nothing; "+
				"this node places by the new one")
	}
	c.current.Store(&installed{state: state, version: version, layout: layout})
}

// Run refreshes every [CacheInterval] until ctx ends. A failed read is logged
// and the previous map kept.
func (c *Cache) Run(ctx context.Context) {
	ticker := time.NewTicker(CacheInterval)
	defer ticker.Stop()
	for {
		if err := c.Refresh(ctx); err != nil && ctx.Err() == nil {
			log.WarnContext(ctx, "object_map_unread", "error", err,
				"detail", "placing by the map this node last read")
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
