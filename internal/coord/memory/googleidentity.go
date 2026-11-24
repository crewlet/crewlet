package memory

import (
	"context"
	"errors"

	"github.com/crewlet/crewlet/internal/coord"
)

// ---- Google growth identities -------------------------------------------- //

// The model retries' class machinery (modelrecords.go), keyed by the seat's
// handle; nothing watches it.

func newGoogleIdentityRecords() entryRecords[coord.GoogleIdentity, coord.GoogleIdentity] {
	return entryRecords[coord.GoogleIdentity, coord.GoogleIdentity]{
		stamp: func(v coord.GoogleIdentity, version uint64) coord.GoogleIdentity {
			v.CreatedAt, v.Version = v.CreatedAt.UTC(), version
			return v
		},
		update: func(v *coord.GoogleIdentity, _ string, _ bool) coord.GoogleIdentity {
			if v == nil {
				return coord.GoogleIdentity{}
			}
			return *v
		},
	}
}

var errNoIdentityHandle = errors.New("coord/memory: a Google identity needs the seat's handle")

// GoogleIdentity reads one seat's identity.
func (f *Fleet) GoogleIdentity(_ context.Context, handle string) (coord.GoogleIdentity, bool, error) {
	if handle == "" {
		return coord.GoogleIdentity{}, false, errNoIdentityHandle
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	g, ok := f.googleIdentities.records[handle]
	return g, ok, nil
}

// ListGoogleIdentities returns every identity, ordered by handle.
func (f *Fleet) ListGoogleIdentities(context.Context) ([]coord.GoogleIdentity, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.googleIdentities.sortedLocked(), nil
}

// CreateGoogleIdentity writes an identity for a seat that has none.
func (f *Fleet) CreateGoogleIdentity(_ context.Context, g coord.GoogleIdentity) (coord.GoogleIdentity, bool, error) {
	if err := g.Validate(); err != nil {
		return coord.GoogleIdentity{}, false, err
	}
	if err := holdRecord(coord.GoogleIdentityKey(g.Handle), g, coord.SmallValueBytes); err != nil {
		return coord.GoogleIdentity{}, false, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, exists := f.googleIdentities.records[g.Handle]; exists {
		return coord.GoogleIdentity{}, false, nil
	}
	return f.googleIdentities.storeLocked(f, g.Handle, g), true, nil
}

// UpdateGoogleIdentity writes an identity at the version it was read at.
func (f *Fleet) UpdateGoogleIdentity(_ context.Context, g coord.GoogleIdentity) (coord.GoogleIdentity, bool, error) {
	if err := g.Validate(); err != nil {
		return coord.GoogleIdentity{}, false, err
	}
	if err := holdRecord(coord.GoogleIdentityKey(g.Handle), g, coord.SmallValueBytes); err != nil {
		return coord.GoogleIdentity{}, false, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	held, ok := f.googleIdentities.records[g.Handle]
	if !ok || g.Version == 0 || held.Version != g.Version {
		return coord.GoogleIdentity{}, false, nil
	}
	return f.googleIdentities.storeLocked(f, g.Handle, g), true, nil
}

// DeleteGoogleIdentity removes a seat's identity at a version.
func (f *Fleet) DeleteGoogleIdentity(_ context.Context, handle string, version uint64) (bool, error) {
	if handle == "" {
		return false, errNoIdentityHandle
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	held, ok := f.googleIdentities.records[handle]
	if !ok || version == 0 || held.Version != version {
		return false, nil
	}
	f.googleIdentities.removeLocked(handle)
	return true, nil
}
