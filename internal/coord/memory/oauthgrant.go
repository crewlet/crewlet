package memory

import (
	"context"
	"errors"

	"github.com/crewlet/crewlet/internal/coord"
)

// ---- OAuth grants and sign-ins ------------------------------------------- //

// The model retries' class machinery (modelrecords.go), one set per family
// keyed by the grant's name: one grant per name, watched by every node, and
// one sign-in per name, which nothing watches.

func newGrantRecords() *entryRecords[coord.OAuthGrant, coord.OAuthGrantUpdate] {
	return &entryRecords[coord.OAuthGrant, coord.OAuthGrantUpdate]{
		stamp: func(v coord.OAuthGrant, version uint64) coord.OAuthGrant {
			v.At, v.Version = v.At.UTC(), version
			return v
		},
		update: func(v *coord.OAuthGrant, name string, current bool) coord.OAuthGrantUpdate {
			return coord.OAuthGrantUpdate{Grant: v, Name: name, Current: current}
		},
	}
}

func newOAuthSignInRecords() *entryRecords[coord.OAuthSignIn, coord.OAuthSignIn] {
	return &entryRecords[coord.OAuthSignIn, coord.OAuthSignIn]{
		stamp: func(v coord.OAuthSignIn, version uint64) coord.OAuthSignIn {
			v.StartedAt, v.ExpiresAt, v.Version = v.StartedAt.UTC(), v.ExpiresAt.UTC(), version
			return v
		},
		update: func(v *coord.OAuthSignIn, _ string, _ bool) coord.OAuthSignIn {
			if v == nil {
				return coord.OAuthSignIn{}
			}
			return *v
		},
	}
}

// newGrantFamilies is one set of grant and sign-in records per family.
func newGrantFamilies() map[coord.GrantFamily]*grantFamilyRecords {
	out := make(map[coord.GrantFamily]*grantFamilyRecords, len(coord.GrantFamilies))
	for _, f := range coord.GrantFamilies {
		out[f] = &grantFamilyRecords{grants: newGrantRecords(), signIns: newOAuthSignInRecords()}
	}
	return out
}

// grantFamilyRecords are one family's grants and sign-ins.
type grantFamilyRecords struct {
	grants  *entryRecords[coord.OAuthGrant, coord.OAuthGrantUpdate]
	signIns *entryRecords[coord.OAuthSignIn, coord.OAuthSignIn]
}

var errNoGrantName = errors.New("coord/memory: an OAuth grant or sign-in needs its name")

// familyLocked is fam's records, or why there are none.
func (f *Fleet) familyLocked(fam coord.GrantFamily) (*grantFamilyRecords, error) {
	r, ok := f.oauth[fam]
	if !ok {
		return nil, coord.ErrUnknownGrantFamily
	}
	return r, nil
}

// OAuthGrant reads one grant.
func (f *Fleet) OAuthGrant(_ context.Context, fam coord.GrantFamily, name string) (coord.OAuthGrant, bool, error) {
	if name == "" {
		return coord.OAuthGrant{}, false, errNoGrantName
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	r, err := f.familyLocked(fam)
	if err != nil {
		return coord.OAuthGrant{}, false, err
	}
	g, ok := r.grants.records[name]
	return g, ok, nil
}

// ListOAuthGrants returns every grant of a family, ordered by name.
func (f *Fleet) ListOAuthGrants(_ context.Context, fam coord.GrantFamily) ([]coord.OAuthGrant, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, err := f.familyLocked(fam)
	if err != nil {
		return nil, err
	}
	return r.grants.sortedLocked(), nil
}

// CreateOAuthGrant writes a grant for a name that has none.
func (f *Fleet) CreateOAuthGrant(_ context.Context, fam coord.GrantFamily, g coord.OAuthGrant) (coord.OAuthGrant, bool, error) {
	if err := g.Validate(fam); err != nil {
		return coord.OAuthGrant{}, false, err
	}
	if err := holdRecord(fam.GrantKey(g.Name), g, coord.MaxOAuthGrantBytes); err != nil {
		return coord.OAuthGrant{}, false, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	r, err := f.familyLocked(fam)
	if err != nil {
		return coord.OAuthGrant{}, false, err
	}
	if _, exists := r.grants.records[g.Name]; exists {
		return coord.OAuthGrant{}, false, nil
	}
	return r.grants.storeLocked(f, g.Name, g), true, nil
}

// UpdateOAuthGrant writes a grant at the version it was read at.
func (f *Fleet) UpdateOAuthGrant(_ context.Context, fam coord.GrantFamily, g coord.OAuthGrant) (coord.OAuthGrant, bool, error) {
	if err := g.Validate(fam); err != nil {
		return coord.OAuthGrant{}, false, err
	}
	if err := holdRecord(fam.GrantKey(g.Name), g, coord.MaxOAuthGrantBytes); err != nil {
		return coord.OAuthGrant{}, false, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	r, err := f.familyLocked(fam)
	if err != nil {
		return coord.OAuthGrant{}, false, err
	}
	held, ok := r.grants.records[g.Name]
	if !ok || g.Version == 0 || held.Version != g.Version {
		return coord.OAuthGrant{}, false, nil
	}
	return r.grants.storeLocked(f, g.Name, g), true, nil
}

// DeleteOAuthGrant removes a grant at a version.
func (f *Fleet) DeleteOAuthGrant(_ context.Context, fam coord.GrantFamily, name string, version uint64) (bool, error) {
	if name == "" {
		return false, errNoGrantName
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	r, err := f.familyLocked(fam)
	if err != nil {
		return false, err
	}
	held, ok := r.grants.records[name]
	if !ok || version == 0 || held.Version != version {
		return false, nil
	}
	r.grants.removeLocked(name)
	return true, nil
}

// WatchOAuthGrants streams every grant of a family, the marker, then every
// change.
func (f *Fleet) WatchOAuthGrants(ctx context.Context, fam coord.GrantFamily) (<-chan coord.OAuthGrantUpdate, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, err := f.familyLocked(fam)
	if err != nil {
		return nil, err
	}
	return r.grants.watchLocked(ctx, f), nil
}

// OAuthSignIn reads one grant's sign-in.
func (f *Fleet) OAuthSignIn(_ context.Context, fam coord.GrantFamily, name string) (coord.OAuthSignIn, bool, error) {
	if name == "" {
		return coord.OAuthSignIn{}, false, errNoGrantName
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	r, err := f.familyLocked(fam)
	if err != nil {
		return coord.OAuthSignIn{}, false, err
	}
	s, ok := r.signIns.records[name]
	return s, ok, nil
}

// ListOAuthSignIns returns every sign-in of a family, ordered by name.
func (f *Fleet) ListOAuthSignIns(_ context.Context, fam coord.GrantFamily) ([]coord.OAuthSignIn, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, err := f.familyLocked(fam)
	if err != nil {
		return nil, err
	}
	return r.signIns.sortedLocked(), nil
}

// CreateOAuthSignIn writes a sign-in for a name that has no record.
func (f *Fleet) CreateOAuthSignIn(_ context.Context, fam coord.GrantFamily, s coord.OAuthSignIn) (coord.OAuthSignIn, bool, error) {
	if err := s.Validate(fam); err != nil {
		return coord.OAuthSignIn{}, false, err
	}
	if err := holdRecord(fam.SignInKey(s.Name), s, coord.MaxOAuthSignInBytes); err != nil {
		return coord.OAuthSignIn{}, false, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	r, err := f.familyLocked(fam)
	if err != nil {
		return coord.OAuthSignIn{}, false, err
	}
	if _, exists := r.signIns.records[s.Name]; exists {
		return coord.OAuthSignIn{}, false, nil
	}
	return r.signIns.storeLocked(f, s.Name, s), true, nil
}

// UpdateOAuthSignIn writes a sign-in at the version it was read at.
func (f *Fleet) UpdateOAuthSignIn(_ context.Context, fam coord.GrantFamily, s coord.OAuthSignIn) (coord.OAuthSignIn, bool, error) {
	if err := s.Validate(fam); err != nil {
		return coord.OAuthSignIn{}, false, err
	}
	if err := holdRecord(fam.SignInKey(s.Name), s, coord.MaxOAuthSignInBytes); err != nil {
		return coord.OAuthSignIn{}, false, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	r, err := f.familyLocked(fam)
	if err != nil {
		return coord.OAuthSignIn{}, false, err
	}
	held, ok := r.signIns.records[s.Name]
	if !ok || s.Version == 0 || held.Version != s.Version {
		return coord.OAuthSignIn{}, false, nil
	}
	return r.signIns.storeLocked(f, s.Name, s), true, nil
}

// DeleteOAuthSignIn removes a sign-in at a version.
func (f *Fleet) DeleteOAuthSignIn(_ context.Context, fam coord.GrantFamily, name string, version uint64) (bool, error) {
	if name == "" {
		return false, errNoGrantName
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	r, err := f.familyLocked(fam)
	if err != nil {
		return false, err
	}
	held, ok := r.signIns.records[name]
	if !ok || version == 0 || held.Version != version {
		return false, nil
	}
	r.signIns.removeLocked(name)
	return true, nil
}
