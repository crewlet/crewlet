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
	hold.Streams = maps.Clone(h.Streams)
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
		hold.Streams = maps.Clone(hold.Streams)
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

// PutFloor writes one domain's published trim floor. See the KV backend for
// why it lives in the same register as the positions and the holds.
func (f *Fleet) PutFloor(_ context.Context, floor coord.TrimFloor) error {
	if err := floor.Validate(); err != nil {
		return err
	}
	if floor.At.IsZero() {
		floor.At = time.Now().UTC()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.floors == nil {
		f.floors = map[string]coord.TrimFloor{}
	}
	// CLONED ON THE WAY IN, for the reason the other two rows are: the
	// terms are a slice the caller keeps, and a shared one is the aliasing
	// bug only a twin can have.
	row := floor
	row.Terms = slices.Clone(floor.Terms)
	f.floors[floor.Domain] = row
	return nil
}

// Floors reads every domain's row, in domain order so two captures are
// diffable.
func (f *Fleet) Floors(_ context.Context) ([]coord.TrimFloor, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]coord.TrimFloor, 0, len(f.floors))
	for _, domain := range slices.Sorted(maps.Keys(f.floors)) {
		row := f.floors[domain]
		row.Terms = slices.Clone(row.Terms)
		out = append(out, row)
	}
	return out, nil
}

// ForgetFloor removes one domain's row.
func (f *Fleet) ForgetFloor(_ context.Context, domain string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.floors, domain)
	return nil
}

// PutBackupPoint writes one owner's newest backup. See the KV backend for why
// it lives in the same register as the positions, the holds and the floors.
func (f *Fleet) PutBackupPoint(_ context.Context, p coord.BackupPoint) error {
	if err := p.Validate(); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.backups == nil {
		f.backups = map[string]coord.BackupPoint{}
	}
	// CLONED ON THE WAY IN, for the reason every row here is.
	row := p
	row.Streams = maps.Clone(p.Streams)
	f.backups[p.Owner] = row
	return nil
}

// BackupPoints reads every owner's row, in owner order so two captures are
// diffable.
func (f *Fleet) BackupPoints(_ context.Context) ([]coord.BackupPoint, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]coord.BackupPoint, 0, len(f.backups))
	for _, owner := range slices.Sorted(maps.Keys(f.backups)) {
		row := f.backups[owner]
		row.Streams = maps.Clone(row.Streams)
		out = append(out, row)
	}
	return out, nil
}

// ForgetBackupPoint removes one owner's row.
func (f *Fleet) ForgetBackupPoint(_ context.Context, owner string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.backups, owner)
	return nil
}
