package memory

import (
	"context"
	"maps"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
)

// PutPositions writes a node's row. See the KV backend for why it is a plain
// write rather than a compare-and-set.
func (f *Fleet) PutPositions(_ context.Context, p coord.NodePositions) error {
	if err := p.Validate(); err != nil {
		return err
	}
	if p.At.IsZero() {
		p.At = time.Now().UTC()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.positions == nil {
		f.positions = map[string]coord.NodePositions{}
	}
	// CLONED ON THE WAY IN. The caller keeps its map and would otherwise be
	// writing into the store's copy afterwards — a shared map is the one
	// aliasing bug a twin can have that the real backend cannot, because
	// the real one serialises.
	row := p
	row.Domains = maps.Clone(p.Domains)
	f.positions[p.NodeID] = row
	return nil
}

// Positions reads every node's row, in node order so two captures of one
// estate are diffable.
func (f *Fleet) Positions(_ context.Context) ([]coord.NodePositions, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]coord.NodePositions, 0, len(f.positions))
	for _, id := range slices.Sorted(maps.Keys(f.positions)) {
		row := f.positions[id]
		row.Domains = maps.Clone(row.Domains)
		out = append(out, row)
	}
	return out, nil
}

// ForgetPositions removes a node's row.
func (f *Fleet) ForgetPositions(_ context.Context, nodeID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.positions, nodeID)
	return nil
}

// PutHold writes or renews a trim hold. See the KV backend for why it lives
// in the same register as a node's positions.
func (f *Fleet) PutHold(_ context.Context, h coord.TrimHold) error {
	if err := h.Validate(); err != nil {
		return err
	}
	if h.At.IsZero() {
		h.At = time.Now().UTC()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.holds == nil {
		f.holds = map[string]coord.TrimHold{}
	}
	// CLONED ON THE WAY IN, for the reason the positions row is: a shared
	// map is the one aliasing bug a twin can have and the real backend
	// cannot, because the real one serialises.
	hold := h
	hold.Domains = maps.Clone(h.Domains)
	f.holds[h.Owner] = hold
	return nil
}

// Holds reads every live pin, in owner order so two captures are diffable.
func (f *Fleet) Holds(_ context.Context) ([]coord.TrimHold, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]coord.TrimHold, 0, len(f.holds))
	for _, owner := range slices.Sorted(maps.Keys(f.holds)) {
		hold := f.holds[owner]
		hold.Domains = maps.Clone(hold.Domains)
		out = append(out, hold)
	}
	return out, nil
}

// ReleaseHold removes one.
func (f *Fleet) ReleaseHold(_ context.Context, owner string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.holds, owner)
	return nil
}
