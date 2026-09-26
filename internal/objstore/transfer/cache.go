package transfer

import (
	"context"
	"sync"
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
const CacheInterval = 10 * time.Second

// Cache holds the placement map this node places by.
type Cache struct {
	store MapReader

	mu      sync.RWMutex
	state   objstore.MapState
	version uint64
	have    bool
}

// NewCache builds an empty cache; [Cache.Refresh] or [Cache.Run] fills it.
func NewCache(store MapReader) *Cache { return &Cache{store: store} }

// Current is the map to place by, and false before one was ever read.
func (c *Cache) Current() (placement.Map, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.state.Map, c.have
}

// State is the whole stored record and the version it was read at.
func (c *Cache) State() (objstore.MapState, uint64, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.state, c.version, c.have
}

// Refresh reads the stored map. A map this build cannot read leaves the one it
// had in place and answers the error: placing by the last map it understood is
// correct for as long as it takes to upgrade, where placing by nothing would
// stop every upload on the node.
func (c *Cache) Refresh(ctx context.Context) error {
	rec, found, err := c.store.ObjectMap(ctx)
	if err != nil || !found {
		return err
	}
	state, err := objstore.DecodeMapState(rec.Value)
	if err != nil {
		return err
	}
	c.Observe(state, rec.Version)
	return nil
}

// Observe installs a map this node wrote or read, unless the cache already
// holds a later one — so the maintainer's own write is placed by at once
// rather than a refresh later, and a slow read never rolls back a fast one.
func (c *Cache) Observe(state objstore.MapState, version uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.have && version <= c.version {
		return
	}
	c.state, c.version, c.have = state, version, true
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
