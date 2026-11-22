package kv

import (
	"context"

	"github.com/crewlet/crewlet/internal/coord"
)

// ---- OAuth grants and sign-ins ------------------------------------------- //

// One grant and one sign-in per name in each family, each a positions-register
// class of the family's own (internal/coord's oauthgrant.go), over
// [entryClass] keyed by name.

// grantClass is family f's grant class.
func grantClass(f coord.GrantFamily) entryClass[coord.OAuthGrant] {
	return entryClass[coord.OAuthGrant]{
		name:   "the " + f.GrantClass + " grants",
		filter: coord.DocumentFilter(f.GrantClass),
		key:    f.GrantKey,
		entry:  f.GrantNameOf,
		stored: func(g coord.OAuthGrant, name string, version uint64) coord.OAuthGrant {
			// The name it is filed under, whatever its body says.
			g.Name, g.At, g.Version = name, g.At.UTC(), version
			return g
		},
	}
}

// oauthSignInClass is family f's sign-in class.
func oauthSignInClass(f coord.GrantFamily) entryClass[coord.OAuthSignIn] {
	return entryClass[coord.OAuthSignIn]{
		name:   "the " + f.SignInClass + " sign-ins",
		filter: coord.DocumentFilter(f.SignInClass),
		key:    f.SignInKey,
		entry:  f.SignInNameOf,
		stored: func(s coord.OAuthSignIn, name string, version uint64) coord.OAuthSignIn {
			s.Name, s.Version = name, version
			s.StartedAt, s.ExpiresAt = s.StartedAt.UTC(), s.ExpiresAt.UTC()
			return s
		},
	}
}

// OAuthGrant reads one grant.
func (f *FleetStore) OAuthGrant(ctx context.Context, fam coord.GrantFamily, name string) (coord.OAuthGrant, bool, error) {
	if !fam.Valid() {
		return coord.OAuthGrant{}, false, coord.ErrUnknownGrantFamily
	}
	return grantClass(fam).read(ctx, f, name)
}

// ListOAuthGrants returns every grant of a family, ordered by name.
func (f *FleetStore) ListOAuthGrants(ctx context.Context, fam coord.GrantFamily) ([]coord.OAuthGrant, error) {
	if !fam.Valid() {
		return nil, coord.ErrUnknownGrantFamily
	}
	return grantClass(fam).list(ctx, f)
}

// CreateOAuthGrant writes a grant for a name that has none.
func (f *FleetStore) CreateOAuthGrant(ctx context.Context, fam coord.GrantFamily, g coord.OAuthGrant) (coord.OAuthGrant, bool, error) {
	if err := g.Validate(fam); err != nil {
		return coord.OAuthGrant{}, false, err
	}
	return grantClass(fam).create(ctx, f, g.Name, g)
}

// UpdateOAuthGrant writes a grant at the version it was read at.
func (f *FleetStore) UpdateOAuthGrant(ctx context.Context, fam coord.GrantFamily, g coord.OAuthGrant) (coord.OAuthGrant, bool, error) {
	if err := g.Validate(fam); err != nil {
		return coord.OAuthGrant{}, false, err
	}
	return grantClass(fam).update(ctx, f, g.Name, g, g.Version)
}

// DeleteOAuthGrant removes a grant at a version.
func (f *FleetStore) DeleteOAuthGrant(ctx context.Context, fam coord.GrantFamily, name string, version uint64) (bool, error) {
	if !fam.Valid() {
		return false, coord.ErrUnknownGrantFamily
	}
	return grantClass(fam).remove(ctx, f, name, version)
}

// WatchOAuthGrants streams every grant of a family, the marker, then every
// change.
func (f *FleetStore) WatchOAuthGrants(ctx context.Context, fam coord.GrantFamily) (<-chan coord.OAuthGrantUpdate, error) {
	if !fam.Valid() {
		return nil, coord.ErrUnknownGrantFamily
	}
	return watchEntries(ctx, f, grantClass(fam),
		func(v *coord.OAuthGrant, name string, current bool) coord.OAuthGrantUpdate {
			return coord.OAuthGrantUpdate{Grant: v, Name: name, Current: current}
		})
}

// OAuthSignIn reads one grant's sign-in.
func (f *FleetStore) OAuthSignIn(ctx context.Context, fam coord.GrantFamily, name string) (coord.OAuthSignIn, bool, error) {
	if !fam.Valid() {
		return coord.OAuthSignIn{}, false, coord.ErrUnknownGrantFamily
	}
	return oauthSignInClass(fam).read(ctx, f, name)
}

// ListOAuthSignIns returns every sign-in of a family, ordered by name.
func (f *FleetStore) ListOAuthSignIns(ctx context.Context, fam coord.GrantFamily) ([]coord.OAuthSignIn, error) {
	if !fam.Valid() {
		return nil, coord.ErrUnknownGrantFamily
	}
	return oauthSignInClass(fam).list(ctx, f)
}

// CreateOAuthSignIn writes a sign-in for a name that has no record.
func (f *FleetStore) CreateOAuthSignIn(ctx context.Context, fam coord.GrantFamily, s coord.OAuthSignIn) (coord.OAuthSignIn, bool, error) {
	if err := s.Validate(fam); err != nil {
		return coord.OAuthSignIn{}, false, err
	}
	return oauthSignInClass(fam).create(ctx, f, s.Name, s)
}

// UpdateOAuthSignIn writes a sign-in at the version it was read at.
func (f *FleetStore) UpdateOAuthSignIn(ctx context.Context, fam coord.GrantFamily, s coord.OAuthSignIn) (coord.OAuthSignIn, bool, error) {
	if err := s.Validate(fam); err != nil {
		return coord.OAuthSignIn{}, false, err
	}
	return oauthSignInClass(fam).update(ctx, f, s.Name, s, s.Version)
}

// DeleteOAuthSignIn removes a sign-in at a version.
func (f *FleetStore) DeleteOAuthSignIn(ctx context.Context, fam coord.GrantFamily, name string, version uint64) (bool, error) {
	if !fam.Valid() {
		return false, coord.ErrUnknownGrantFamily
	}
	return oauthSignInClass(fam).remove(ctx, f, name, version)
}
