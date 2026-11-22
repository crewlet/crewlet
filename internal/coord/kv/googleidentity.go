package kv

import (
	"context"

	"github.com/crewlet/crewlet/internal/coord"
)

// ---- Google growth identities -------------------------------------------- //

// One identity per seat, a positions-register class of its own
// (internal/coord's googleidentity.go), over [entryClass] keyed by handle.

var googleIdentityClass = entryClass[coord.GoogleIdentity]{
	name:   "the Google identities",
	filter: coord.DocumentFilter(coord.GoogleIdentityClass),
	key:    coord.GoogleIdentityKey,
	entry:  coord.GoogleIdentityHandleOf,
	stored: func(g coord.GoogleIdentity, handle string, version uint64) coord.GoogleIdentity {
		// The handle it is filed under, whatever its body says.
		g.Handle, g.CreatedAt, g.Version = handle, g.CreatedAt.UTC(), version
		return g
	},
}

// GoogleIdentity reads one seat's identity.
func (f *FleetStore) GoogleIdentity(ctx context.Context, handle string) (coord.GoogleIdentity, bool, error) {
	return googleIdentityClass.read(ctx, f, handle)
}

// ListGoogleIdentities returns every identity, ordered by handle.
func (f *FleetStore) ListGoogleIdentities(ctx context.Context) ([]coord.GoogleIdentity, error) {
	return googleIdentityClass.list(ctx, f)
}

// CreateGoogleIdentity writes an identity for a seat that has none.
func (f *FleetStore) CreateGoogleIdentity(
	ctx context.Context, g coord.GoogleIdentity,
) (coord.GoogleIdentity, bool, error) {
	if err := g.Validate(); err != nil {
		return coord.GoogleIdentity{}, false, err
	}
	return googleIdentityClass.create(ctx, f, g.Handle, g)
}

// UpdateGoogleIdentity writes an identity at the version it was read at.
func (f *FleetStore) UpdateGoogleIdentity(
	ctx context.Context, g coord.GoogleIdentity,
) (coord.GoogleIdentity, bool, error) {
	if err := g.Validate(); err != nil {
		return coord.GoogleIdentity{}, false, err
	}
	return googleIdentityClass.update(ctx, f, g.Handle, g, g.Version)
}

// DeleteGoogleIdentity removes a seat's identity at a version.
func (f *FleetStore) DeleteGoogleIdentity(ctx context.Context, handle string, version uint64) (bool, error) {
	return googleIdentityClass.remove(ctx, f, handle, version)
}
