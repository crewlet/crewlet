package memory

import (
	"context"
	"errors"

	"github.com/crewlet/crewlet/internal/coord"
)

// ---- MCP grants and sign-ins --------------------------------------------- //

// The model retries' class machinery (modelrecords.go), keyed by the server's
// name: one grant per server, watched by every node, and one sign-in per
// server, which nothing watches.

func newGrantRecords() entryRecords[coord.MCPGrant, coord.MCPGrantUpdate] {
	return entryRecords[coord.MCPGrant, coord.MCPGrantUpdate]{
		stamp: func(v coord.MCPGrant, version uint64) coord.MCPGrant {
			v.At, v.Version = v.At.UTC(), version
			return v
		},
		update: func(v *coord.MCPGrant, server string, current bool) coord.MCPGrantUpdate {
			return coord.MCPGrantUpdate{Grant: v, Server: server, Current: current}
		},
	}
}

func newMCPSignInRecords() entryRecords[coord.MCPSignIn, coord.MCPSignIn] {
	return entryRecords[coord.MCPSignIn, coord.MCPSignIn]{
		stamp: func(v coord.MCPSignIn, version uint64) coord.MCPSignIn {
			v.StartedAt, v.ExpiresAt, v.Version = v.StartedAt.UTC(), v.ExpiresAt.UTC(), version
			return v
		},
		update: func(v *coord.MCPSignIn, _ string, _ bool) coord.MCPSignIn {
			if v == nil {
				return coord.MCPSignIn{}
			}
			return *v
		},
	}
}

var errNoOAuthServer = errors.New("coord/memory: an MCP grant or sign-in needs the server's name")

// MCPGrant reads one server's grant.
func (f *Fleet) MCPGrant(_ context.Context, server string) (coord.MCPGrant, bool, error) {
	if server == "" {
		return coord.MCPGrant{}, false, errNoOAuthServer
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	g, ok := f.grants.records[server]
	return g, ok, nil
}

// ListMCPGrants returns every grant, ordered by server.
func (f *Fleet) ListMCPGrants(context.Context) ([]coord.MCPGrant, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.grants.sortedLocked(), nil
}

// CreateMCPGrant writes a grant for a server that has none.
func (f *Fleet) CreateMCPGrant(_ context.Context, g coord.MCPGrant) (coord.MCPGrant, bool, error) {
	if err := g.Validate(); err != nil {
		return coord.MCPGrant{}, false, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, exists := f.grants.records[g.Server]; exists {
		return coord.MCPGrant{}, false, nil
	}
	return f.grants.storeLocked(f, g.Server, g), true, nil
}

// UpdateMCPGrant writes a grant at the version it was read at.
func (f *Fleet) UpdateMCPGrant(_ context.Context, g coord.MCPGrant) (coord.MCPGrant, bool, error) {
	if err := g.Validate(); err != nil {
		return coord.MCPGrant{}, false, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	held, ok := f.grants.records[g.Server]
	if !ok || g.Version == 0 || held.Version != g.Version {
		return coord.MCPGrant{}, false, nil
	}
	return f.grants.storeLocked(f, g.Server, g), true, nil
}

// DeleteMCPGrant removes a server's grant at a version.
func (f *Fleet) DeleteMCPGrant(_ context.Context, server string, version uint64) (bool, error) {
	if server == "" {
		return false, errNoOAuthServer
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	held, ok := f.grants.records[server]
	if !ok || version == 0 || held.Version != version {
		return false, nil
	}
	f.grants.removeLocked(server)
	return true, nil
}

// WatchMCPGrants streams every grant, the marker, then every change.
func (f *Fleet) WatchMCPGrants(ctx context.Context) (<-chan coord.MCPGrantUpdate, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.grants.watchLocked(ctx, f), nil
}

// MCPSignIn reads one server's sign-in.
func (f *Fleet) MCPSignIn(_ context.Context, server string) (coord.MCPSignIn, bool, error) {
	if server == "" {
		return coord.MCPSignIn{}, false, errNoOAuthServer
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.mcpSignIns.records[server]
	return s, ok, nil
}

// ListMCPSignIns returns every sign-in, ordered by server.
func (f *Fleet) ListMCPSignIns(context.Context) ([]coord.MCPSignIn, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.mcpSignIns.sortedLocked(), nil
}

// CreateMCPSignIn writes a sign-in for a server that has no record.
func (f *Fleet) CreateMCPSignIn(_ context.Context, s coord.MCPSignIn) (coord.MCPSignIn, bool, error) {
	if err := s.Validate(); err != nil {
		return coord.MCPSignIn{}, false, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, exists := f.mcpSignIns.records[s.Server]; exists {
		return coord.MCPSignIn{}, false, nil
	}
	return f.mcpSignIns.storeLocked(f, s.Server, s), true, nil
}

// UpdateMCPSignIn writes a sign-in at the version it was read at.
func (f *Fleet) UpdateMCPSignIn(_ context.Context, s coord.MCPSignIn) (coord.MCPSignIn, bool, error) {
	if err := s.Validate(); err != nil {
		return coord.MCPSignIn{}, false, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	held, ok := f.mcpSignIns.records[s.Server]
	if !ok || s.Version == 0 || held.Version != s.Version {
		return coord.MCPSignIn{}, false, nil
	}
	return f.mcpSignIns.storeLocked(f, s.Server, s), true, nil
}

// DeleteMCPSignIn removes a server's sign-in at a version.
func (f *Fleet) DeleteMCPSignIn(_ context.Context, server string, version uint64) (bool, error) {
	if server == "" {
		return false, errNoOAuthServer
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	held, ok := f.mcpSignIns.records[server]
	if !ok || version == 0 || held.Version != version {
		return false, nil
	}
	f.mcpSignIns.removeLocked(server)
	return true, nil
}
