package atlaswake

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/coord"
)

// CoordStore keeps the records on the fleet's coordination store: the whole
// company has to agree, now, on who was woken about what today, whichever
// node holds the duty and across every restart.
type CoordStore struct{ wakes coord.AtlasWakes }

var _ Store = (*CoordStore)(nil)

// NewCoordStore wraps the fleet's records, refusing a nil backend.
func NewCoordStore(wakes coord.AtlasWakes) (*CoordStore, error) {
	if wakes == nil {
		return nil, errors.New("atlaswake: NewCoordStore needs the fleet's coordination backend (wakes is nil), " +
			"which is where the once-a-day bound is kept")
	}
	return &CoordStore{wakes: wakes}, nil
}

// Load implements [Store]. A record this build cannot read, or under a key
// naming no entry, is skipped: no wake is decided from it, and nothing
// writes over it.
func (s *CoordStore) Load(ctx context.Context) ([]Kept, error) {
	raw, err := s.wakes.AtlasWakes(ctx)
	if err != nil {
		return nil, fmt.Errorf("atlaswake: read the wake records: %w", err)
	}
	out := make([]Kept, 0, len(raw))
	for key, r := range raw {
		handle, entry, ok := coord.AtlasWakeOf(key)
		id, idErr := uuid.Parse(entry)
		rec, recErr := DecodeRecord(r.Value)
		if !ok || idErr != nil || recErr != nil {
			log.WarnContext(ctx, "atlas_wake_record_undecodable", "key", key)
			continue
		}
		out = append(out, Kept{Handle: handle, Entry: id, Record: rec, Revision: r.Revision})
	}
	return out, nil
}

// Get implements [Store].
func (s *CoordStore) Get(ctx context.Context, handle string, entry uuid.UUID) (Kept, bool, error) {
	raw, revision, found, err := s.wakes.AtlasWake(ctx, handle, entry.String())
	if err != nil || !found {
		return Kept{}, false, err
	}
	rec, err := DecodeRecord(raw)
	if err != nil {
		return Kept{}, false, err
	}
	return Kept{Handle: handle, Entry: entry, Record: rec, Revision: revision}, true, nil
}

// Create implements [Store].
func (s *CoordStore) Create(ctx context.Context, handle string, entry uuid.UUID, rec Record) (bool, error) {
	raw, err := EncodeRecord(rec)
	if err != nil {
		return false, err
	}
	return s.wakes.CreateAtlasWake(ctx, handle, entry.String(), raw)
}

// Update implements [Store].
func (s *CoordStore) Update(ctx context.Context, k Kept, rec Record) (bool, error) {
	raw, err := EncodeRecord(rec)
	if err != nil {
		return false, err
	}
	return s.wakes.UpdateAtlasWake(ctx, k.Handle, k.Entry.String(), raw, k.Revision)
}

// Delete implements [Store].
func (s *CoordStore) Delete(ctx context.Context, k Kept) (bool, error) {
	return s.wakes.DeleteAtlasWake(ctx, k.Handle, k.Entry.String(), k.Revision)
}
