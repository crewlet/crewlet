package memory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/crewlet/crewlet/internal/coord"
)

// ---- MCP listings and tool definitions ---------------------------------- //

// A listing is kept by its whole key, as the KV store files it, so the record
// is stamped with the server and node its key names, whatever its body says.
func newListingRecords() entryRecords[coord.MCPListing, coord.MCPListingUpdate] {
	return entryRecords[coord.MCPListing, coord.MCPListingUpdate]{
		stamp: func(v coord.MCPListing, version uint64) coord.MCPListing {
			v.At, v.Version = v.At.UTC(), version
			return v
		},
		update: func(v *coord.MCPListing, key string, current bool) coord.MCPListingUpdate {
			server, node, _ := coord.MCPListingOf(key)
			return coord.MCPListingUpdate{Listing: v, Server: server, Node: node, Current: current}
		},
	}
}

var errNoListing = errors.New("coord/memory: an MCP listing needs its server and node")

// PutMCPListing writes one node's listing of one server, replacing it.
func (f *Fleet) PutMCPListing(_ context.Context, l coord.MCPListing) (coord.MCPListing, error) {
	l = l.Bounded()
	if err := l.Validate(); err != nil {
		return coord.MCPListing{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	// Through JSON, as the KV store keeps it, so a caller's slices are never
	// the stored record's.
	stored, err := roundTrip(l)
	if err != nil {
		return coord.MCPListing{}, err
	}
	return f.listings.storeLocked(f, coord.MCPListingKey(l.Server, l.Node), stored), nil
}

// DeleteMCPListing removes one node's listing at a version.
func (f *Fleet) DeleteMCPListing(_ context.Context, server, node string, version uint64) (bool, error) {
	if server == "" || node == "" {
		return false, errNoListing
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	key := coord.MCPListingKey(server, node)
	held, ok := f.listings.records[key]
	if !ok || version == 0 || held.Version != version {
		return false, nil
	}
	f.listings.removeLocked(key)
	return true, nil
}

// ListMCPListings returns every listing, ordered by server and then node.
func (f *Fleet) ListMCPListings(context.Context) ([]coord.MCPListing, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.listings.sortedLocked()
	coord.SortMCPListings(out)
	return out, nil
}

// WatchMCPListings streams every listing, the marker, then every change.
func (f *Fleet) WatchMCPListings(ctx context.Context) (<-chan coord.MCPListingUpdate, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.listings.watchLocked(ctx, f), nil
}

// CreateMCPToolDefinition stores a definition unless its digest holds
// one.
func (f *Fleet) CreateMCPToolDefinition(_ context.Context, s coord.MCPToolDefinition) (bool, error) {
	if err := s.Validate(); err != nil {
		return false, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, held := f.definitions[s.Digest]; held {
		return false, nil
	}
	f.version++
	s.At, s.Version = s.At.UTC(), f.version
	s.Input, s.Output = slices.Clone(s.Input), slices.Clone(s.Output)
	if f.definitions == nil {
		f.definitions = map[string]coord.MCPToolDefinition{}
	}
	f.definitions[s.Digest] = s
	return true, nil
}

// MCPToolDefinition reads one definition.
func (f *Fleet) MCPToolDefinition(_ context.Context, digest string) (coord.MCPToolDefinition, bool, error) {
	if !coord.IsMCPDigest(digest) {
		return coord.MCPToolDefinition{}, false, fmt.Errorf("coord/memory: %q is not a definition digest", digest)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.definitions[digest]
	return s, ok, nil
}

// ListMCPToolDefinitions returns every stored definition, ordered by digest.
func (f *Fleet) ListMCPToolDefinitions(context.Context) ([]coord.MCPToolDefinition, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]coord.MCPToolDefinition, 0, len(f.definitions))
	for _, digest := range slices.Sorted(maps.Keys(f.definitions)) {
		out = append(out, f.definitions[digest])
	}
	return out, nil
}

// DeleteMCPToolDefinition removes a definition at a version.
func (f *Fleet) DeleteMCPToolDefinition(_ context.Context, digest string, version uint64) (bool, error) {
	if !coord.IsMCPDigest(digest) {
		return false, fmt.Errorf("coord/memory: %q is not a definition digest", digest)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	held, ok := f.definitions[digest]
	if !ok || version == 0 || held.Version != version {
		return false, nil
	}
	delete(f.definitions, digest)
	return true, nil
}

// roundTrip copies a listing through its JSON encoding, as a store that keeps
// bytes would hand it back: nothing the caller holds aliases the record.
func roundTrip(l coord.MCPListing) (coord.MCPListing, error) {
	raw, err := json.Marshal(l)
	if err != nil {
		return coord.MCPListing{}, fmt.Errorf("coord/memory: encode %s's listing of %s: %w", l.Node, l.Server, err)
	}
	var out coord.MCPListing
	if err := json.Unmarshal(raw, &out); err != nil {
		return coord.MCPListing{}, fmt.Errorf("coord/memory: decode %s's listing of %s: %w", l.Node, l.Server, err)
	}
	return out, nil
}
