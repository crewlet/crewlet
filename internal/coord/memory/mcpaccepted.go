package memory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/crewlet/crewlet/internal/coord"
)

// ---- MCP accepted definitions ------------------------------------------ //

// Kept by the whole key, as the KV store files it, so a record is stamped with
// the server and tool its key names, whatever its body says.
func newAcceptedRecords() entryRecords[coord.MCPAcceptedTool, coord.MCPAcceptedUpdate] {
	return entryRecords[coord.MCPAcceptedTool, coord.MCPAcceptedUpdate]{
		stamp: func(v coord.MCPAcceptedTool, version uint64) coord.MCPAcceptedTool {
			v.At, v.Version = v.At.UTC(), version
			return v
		},
		update: func(v *coord.MCPAcceptedTool, key string, current bool) coord.MCPAcceptedUpdate {
			server, tool, _ := coord.MCPAcceptedOf(key)
			return coord.MCPAcceptedUpdate{Accepted: v, Server: server, Tool: tool, Current: current}
		},
	}
}

var errNoAccepted = errors.New("coord/memory: an accepted MCP tool definition needs its server and tool")

// PutMCPAccepted writes one tool's accepted definition at a version: zero
// creates it only where none is held.
func (f *Fleet) PutMCPAccepted(_ context.Context, a coord.MCPAcceptedTool, version uint64) (coord.MCPAcceptedTool, bool, error) {
	a = a.Bounded()
	if err := a.Validate(); err != nil {
		return coord.MCPAcceptedTool{}, false, err
	}
	// Through JSON, as the KV store keeps it.
	raw, err := json.Marshal(a)
	if err != nil {
		return coord.MCPAcceptedTool{}, false, fmt.Errorf("coord/memory: encode the accepted definition of %s on %s: %w",
			a.Tool, a.Server, err)
	}
	var stored coord.MCPAcceptedTool
	if err := json.Unmarshal(raw, &stored); err != nil {
		return coord.MCPAcceptedTool{}, false, fmt.Errorf("coord/memory: decode the accepted definition of %s on %s: %w",
			a.Tool, a.Server, err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	key := coord.MCPAcceptedKey(a.Server, a.Tool)
	held, found := f.accepted.records[key]
	if (version == 0 && found) || (version != 0 && (!found || held.Version != version)) {
		return coord.MCPAcceptedTool{}, false, nil
	}
	return f.accepted.storeLocked(f, key, stored), true, nil
}

// DeleteMCPAccepted removes one tool's accepted definition at a version.
func (f *Fleet) DeleteMCPAccepted(_ context.Context, server, tool string, version uint64) (bool, error) {
	if server == "" || tool == "" {
		return false, errNoAccepted
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	key := coord.MCPAcceptedKey(server, tool)
	held, ok := f.accepted.records[key]
	if !ok || version == 0 || held.Version != version {
		return false, nil
	}
	f.accepted.removeLocked(key)
	return true, nil
}

// ListMCPAccepted returns every accepted definition, ordered by server and
// then tool.
func (f *Fleet) ListMCPAccepted(context.Context) ([]coord.MCPAcceptedTool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.accepted.sortedLocked()
	coord.SortMCPAccepted(out)
	return out, nil
}

// WatchMCPAccepted streams every accepted definition, the marker, then every
// change.
func (f *Fleet) WatchMCPAccepted(ctx context.Context) (<-chan coord.MCPAcceptedUpdate, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.accepted.watchLocked(ctx, f), nil
}
