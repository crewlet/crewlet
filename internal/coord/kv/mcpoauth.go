package kv

import (
	"context"

	"github.com/crewlet/crewlet/internal/coord"
)

// ---- MCP grants and sign-ins --------------------------------------------- //

// One grant and one sign-in per server, each filed in the positions register
// under a class of its own: see internal/coord's mcpoauth.go. The model
// retries' verbs (modelrecords.go), over the server's name.

var (
	mcpGrantClass = entryClass[coord.MCPGrant]{
		name:   "the MCP grants",
		filter: coord.DocumentFilter(coord.MCPGrantClass),
		key:    coord.MCPGrantKey,
		entry:  coord.MCPGrantServerOf,
		stored: func(g coord.MCPGrant, server string, version uint64) coord.MCPGrant {
			// A RECORD DESCRIBES THE SERVER IT IS FILED UNDER, whatever its
			// body says.
			g.Server, g.At, g.Version = server, g.At.UTC(), version
			return g
		},
	}
	mcpSignInClass = entryClass[coord.MCPSignIn]{
		name:   "the MCP sign-ins",
		filter: coord.DocumentFilter(coord.MCPSignInClass),
		key:    coord.MCPSignInKey,
		entry:  coord.MCPSignInServerOf,
		stored: func(s coord.MCPSignIn, server string, version uint64) coord.MCPSignIn {
			s.Server, s.Version = server, version
			s.StartedAt, s.ExpiresAt = s.StartedAt.UTC(), s.ExpiresAt.UTC()
			return s
		},
	}
)

// MCPGrant reads one server's grant.
func (f *FleetStore) MCPGrant(ctx context.Context, server string) (coord.MCPGrant, bool, error) {
	return mcpGrantClass.read(ctx, f, server)
}

// ListMCPGrants returns every grant, ordered by server.
func (f *FleetStore) ListMCPGrants(ctx context.Context) ([]coord.MCPGrant, error) {
	return mcpGrantClass.list(ctx, f)
}

// CreateMCPGrant writes a grant for a server that has none.
func (f *FleetStore) CreateMCPGrant(ctx context.Context, g coord.MCPGrant) (coord.MCPGrant, bool, error) {
	if err := g.Validate(); err != nil {
		return coord.MCPGrant{}, false, err
	}
	return mcpGrantClass.create(ctx, f, g.Server, g)
}

// UpdateMCPGrant writes a grant at the version it was read at.
func (f *FleetStore) UpdateMCPGrant(ctx context.Context, g coord.MCPGrant) (coord.MCPGrant, bool, error) {
	if err := g.Validate(); err != nil {
		return coord.MCPGrant{}, false, err
	}
	return mcpGrantClass.update(ctx, f, g.Server, g, g.Version)
}

// DeleteMCPGrant removes a server's grant at a version.
func (f *FleetStore) DeleteMCPGrant(ctx context.Context, server string, version uint64) (bool, error) {
	return mcpGrantClass.remove(ctx, f, server, version)
}

// WatchMCPGrants streams every grant, the marker, then every change.
func (f *FleetStore) WatchMCPGrants(ctx context.Context) (<-chan coord.MCPGrantUpdate, error) {
	return watchEntries(ctx, f, mcpGrantClass,
		func(v *coord.MCPGrant, server string, current bool) coord.MCPGrantUpdate {
			return coord.MCPGrantUpdate{Grant: v, Server: server, Current: current}
		})
}

// MCPSignIn reads one server's sign-in.
func (f *FleetStore) MCPSignIn(ctx context.Context, server string) (coord.MCPSignIn, bool, error) {
	return mcpSignInClass.read(ctx, f, server)
}

// ListMCPSignIns returns every sign-in, ordered by server.
func (f *FleetStore) ListMCPSignIns(ctx context.Context) ([]coord.MCPSignIn, error) {
	return mcpSignInClass.list(ctx, f)
}

// CreateMCPSignIn writes a sign-in for a server that has no record.
func (f *FleetStore) CreateMCPSignIn(ctx context.Context, s coord.MCPSignIn) (coord.MCPSignIn, bool, error) {
	if err := s.Validate(); err != nil {
		return coord.MCPSignIn{}, false, err
	}
	return mcpSignInClass.create(ctx, f, s.Server, s)
}

// UpdateMCPSignIn writes a sign-in at the version it was read at.
func (f *FleetStore) UpdateMCPSignIn(ctx context.Context, s coord.MCPSignIn) (coord.MCPSignIn, bool, error) {
	if err := s.Validate(); err != nil {
		return coord.MCPSignIn{}, false, err
	}
	return mcpSignInClass.update(ctx, f, s.Server, s, s.Version)
}

// DeleteMCPSignIn removes a server's sign-in at a version.
func (f *FleetStore) DeleteMCPSignIn(ctx context.Context, server string, version uint64) (bool, error) {
	return mcpSignInClass.remove(ctx, f, server, version)
}
