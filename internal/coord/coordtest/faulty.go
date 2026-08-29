package coordtest

import (
	"context"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
)

// Faulty wraps a coord.Backend and takes the store away on demand.
//
// The tri-state is the most incident-hardened rule in this contract, and a
// healthy backend can only ever produce two of its three answers. Without a
// way to make the store unreachable, "definitely not yours" and "cannot tell"
// are tested by inspection — which is how the Python engine shipped a version
// where an unreachable store returned the same value as a peer holding the
// seat. A two-second blip then had a node logging "another process holds this
// node id's presence lease", pointing an operator at a configuration problem
// that did not exist, while it quietly stopped refreshing its own presence
// during exactly the outage it is built to ride out.
//
// Exported because callers of coord.Backend need it too: any component that
// branches on unknown-versus-lost — the seat host above all — should be
// certified against a store that goes away mid-sweep.
type Faulty struct {
	inner coord.Backend

	mu  sync.RWMutex
	err error
}

// NewFaulty wraps inner. It starts healthy.
func NewFaulty(inner coord.Backend) *Faulty { return &Faulty{inner: inner} }

// Break makes every subsequent call answer "unknown" with err, or with
// coord.ErrUnavailable when err is nil. It does not touch the records
// underneath, which is the point: an outage says nothing about ownership, and
// what a caller does during one is judged by what it still holds afterwards.
func (f *Faulty) Break(err error) {
	if err == nil {
		err = coord.ErrUnavailable
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

// Heal restores the backend.
func (f *Faulty) Heal() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = nil
}

func (f *Faulty) fault() error {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.err
}

// TryAcquire delegates, or fails with the configured error.
func (f *Faulty) TryAcquire(ctx context.Context, resource string, opts coord.AcquireOptions) (*coord.Lease, error) {
	if err := f.fault(); err != nil {
		return nil, err
	}
	return f.inner.TryAcquire(ctx, resource, opts)
}

// Renew delegates, or fails with the configured error.
func (f *Faulty) Renew(ctx context.Context, resource, owner string, epoch int64, ttl time.Duration) (bool, error) {
	if err := f.fault(); err != nil {
		return false, err
	}
	return f.inner.Renew(ctx, resource, owner, epoch, ttl)
}

// Release delegates, or fails with the configured error.
func (f *Faulty) Release(ctx context.Context, resource, owner string, epoch int64) (bool, error) {
	if err := f.fault(); err != nil {
		return false, err
	}
	return f.inner.Release(ctx, resource, owner, epoch)
}

// Get delegates, or fails with the configured error.
func (f *Faulty) Get(ctx context.Context, resource string) (*coord.Lease, error) {
	if err := f.fault(); err != nil {
		return nil, err
	}
	return f.inner.Get(ctx, resource)
}

// ListOwned delegates, or fails with the configured error.
func (f *Faulty) ListOwned(ctx context.Context, owner string) ([]coord.Lease, error) {
	if err := f.fault(); err != nil {
		return nil, err
	}
	return f.inner.ListOwned(ctx, owner)
}

// ListLive delegates, or fails with the configured error.
func (f *Faulty) ListLive(ctx context.Context, prefix string) ([]coord.Lease, error) {
	if err := f.fault(); err != nil {
		return nil, err
	}
	return f.inner.ListLive(ctx, prefix)
}

// PreferredResources delegates, or fails with the configured error.
func (f *Faulty) PreferredResources(ctx context.Context, prefix, nodeID string) (map[string]struct{}, error) {
	if err := f.fault(); err != nil {
		return nil, err
	}
	return f.inner.PreferredResources(ctx, prefix, nodeID)
}

// FleetProtocolFloor delegates, or fails with the configured error.
func (f *Faulty) FleetProtocolFloor(ctx context.Context) (int, bool, error) {
	if err := f.fault(); err != nil {
		return 0, false, err
	}
	return f.inner.FleetProtocolFloor(ctx)
}

var _ coord.Backend = (*Faulty)(nil)
