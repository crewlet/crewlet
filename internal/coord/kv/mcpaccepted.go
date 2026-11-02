package kv

import (
	"context"
	"fmt"

	"github.com/crewlet/crewlet/internal/coord"
)

// ---- MCP accepted definitions ------------------------------------------ //

// One record per (server, tool), filed in the positions register under a
// class of its own: see internal/coord's mcpaccepted.go. Written through
// [entryClass], whose "entry" is the whole key, as a listing's is, because two
// segments name it.

var acceptedClass = entryClass[coord.MCPAcceptedTool]{
	name:   "the MCP accepted definitions",
	filter: coord.DocumentFilter(coord.MCPAcceptedClass),
	key:    func(key string) string { return key },
	entry: func(key string) (string, bool) {
		_, _, ok := coord.MCPAcceptedOf(key)
		return key, ok
	},
	// THE KEY NAMES THE SERVER AND THE TOOL, and a record cannot say
	// otherwise.
	stored: func(a coord.MCPAcceptedTool, key string, version uint64) coord.MCPAcceptedTool {
		a.Server, a.Tool, _ = coord.MCPAcceptedOf(key)
		a.At, a.Version = a.At.UTC(), version
		return a
	},
}

// PutMCPAccepted writes one tool's accepted definition at a version: zero
// creates it only where none is held, any other updates it only at that
// version.
func (f *FleetStore) PutMCPAccepted(ctx context.Context, a coord.MCPAcceptedTool, version uint64) (coord.MCPAcceptedTool, bool, error) {
	a = a.Bounded()
	if err := a.Validate(); err != nil {
		return coord.MCPAcceptedTool{}, false, err
	}
	key := coord.MCPAcceptedKey(a.Server, a.Tool)
	if version == 0 {
		return acceptedClass.create(ctx, f, key, a)
	}
	return acceptedClass.update(ctx, f, key, a, version)
}

// DeleteMCPAccepted removes one tool's accepted definition at a version.
func (f *FleetStore) DeleteMCPAccepted(ctx context.Context, server, tool string, version uint64) (bool, error) {
	if server == "" || tool == "" {
		return false, fmt.Errorf("coord/kv: removing an accepted MCP tool definition needs its server and tool")
	}
	return acceptedClass.remove(ctx, f, coord.MCPAcceptedKey(server, tool), version)
}

// ListMCPAccepted returns every accepted definition, ordered by server and
// then tool.
func (f *FleetStore) ListMCPAccepted(ctx context.Context) ([]coord.MCPAcceptedTool, error) {
	out, err := acceptedClass.list(ctx, f)
	if err != nil {
		return nil, err
	}
	coord.SortMCPAccepted(out)
	return out, nil
}

// WatchMCPAccepted streams every accepted definition, the marker, then every
// change.
func (f *FleetStore) WatchMCPAccepted(ctx context.Context) (<-chan coord.MCPAcceptedUpdate, error) {
	return watchEntries(ctx, f, acceptedClass,
		func(v *coord.MCPAcceptedTool, key string, current bool) coord.MCPAcceptedUpdate {
			server, tool, _ := coord.MCPAcceptedOf(key)
			return coord.MCPAcceptedUpdate{Accepted: v, Server: server, Tool: tool, Current: current}
		})
}
