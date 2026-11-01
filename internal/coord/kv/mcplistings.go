package kv

import (
	"context"
	"fmt"

	"github.com/crewlet/crewlet/internal/coord"
)

// ---- MCP listings and tool schemas ------------------------------------- //

// Both are classes of the positions register, for the reason internal/coord's
// mcplisting.go gives, and both are written through [entryClass], whose verbs
// the model records already share. A listing's "entry" is its whole key,
// because it is named by two segments (the server and the node) where an
// entry class names one; the schema's is its digest.

var (
	listingClass = entryClass[coord.MCPListing]{
		name:   "the MCP listings",
		filter: coord.DocumentFilter(coord.MCPListingClass),
		key:    func(key string) string { return key },
		entry: func(key string) (string, bool) {
			_, _, ok := coord.MCPListingOf(key)
			return key, ok
		},
		// THE KEY NAMES THE SERVER AND THE NODE, and a record cannot say
		// otherwise: a node writing under its own key describes its own
		// listing of that server, whatever the body it was handed claims.
		stored: func(l coord.MCPListing, key string, version uint64) coord.MCPListing {
			l.Server, l.Node, _ = coord.MCPListingOf(key)
			l.At, l.Version = l.At.UTC(), version
			return l
		},
	}
	schemaClass = entryClass[coord.MCPToolSchema]{
		name:   "the MCP tool schemas",
		filter: coord.DocumentFilter(coord.MCPSchemaClass),
		key:    coord.MCPSchemaKey,
		entry:  coord.MCPSchemaDigest,
		stored: func(s coord.MCPToolSchema, digest string, version uint64) coord.MCPToolSchema {
			s.Digest, s.At, s.Version = digest, s.At.UTC(), version
			return s
		},
	}
)

// PutMCPListing writes one node's listing of one server, replacing it.
//
// A PLAIN PUT, as a node's positions row is: the only writer of the key is the
// node it names, so there is no race to arbitrate, and a compare-and-set would
// fail a node on a value only its own process can have changed.
func (f *FleetStore) PutMCPListing(ctx context.Context, l coord.MCPListing) (coord.MCPListing, error) {
	l = l.Bounded()
	if err := l.Validate(); err != nil {
		return coord.MCPListing{}, err
	}
	key := coord.MCPListingKey(l.Server, l.Node)
	raw, err := listingClass.encode(key, l)
	if err != nil {
		return coord.MCPListing{}, err
	}
	revision, err := f.positions.Put(ctx, key, raw)
	if err != nil {
		return coord.MCPListing{}, unavailable(fmt.Sprintf("write %s's listing of %s", l.Node, l.Server), err)
	}
	return listingClass.stored(l, key, revision), nil
}

// DeleteMCPListing removes one node's listing at a version.
func (f *FleetStore) DeleteMCPListing(ctx context.Context, server, node string, version uint64) (bool, error) {
	if server == "" || node == "" {
		return false, fmt.Errorf("coord/kv: removing an MCP listing needs its server and node")
	}
	return listingClass.remove(ctx, f, coord.MCPListingKey(server, node), version)
}

// ListMCPListings returns every listing, ordered by server and then node.
func (f *FleetStore) ListMCPListings(ctx context.Context) ([]coord.MCPListing, error) {
	out, err := listingClass.list(ctx, f)
	if err != nil {
		return nil, err
	}
	coord.SortMCPListings(out)
	return out, nil
}

// WatchMCPListings streams every listing, the marker, then every change.
func (f *FleetStore) WatchMCPListings(ctx context.Context) (<-chan coord.MCPListingUpdate, error) {
	return watchEntries(ctx, f, listingClass,
		func(v *coord.MCPListing, key string, current bool) coord.MCPListingUpdate {
			server, node, _ := coord.MCPListingOf(key)
			return coord.MCPListingUpdate{Listing: v, Server: server, Node: node, Current: current}
		})
}

// CreateMCPToolSchema stores a definition's schemas unless its digest holds
// some.
func (f *FleetStore) CreateMCPToolSchema(ctx context.Context, s coord.MCPToolSchema) (bool, error) {
	if err := s.Validate(); err != nil {
		return false, err
	}
	_, created, err := schemaClass.create(ctx, f, s.Digest, s)
	return created, err
}

// MCPToolSchema reads one definition's schemas.
func (f *FleetStore) MCPToolSchema(ctx context.Context, digest string) (coord.MCPToolSchema, bool, error) {
	if !coord.IsMCPDigest(digest) {
		return coord.MCPToolSchema{}, false, fmt.Errorf("coord/kv: %q is not a definition digest", digest)
	}
	return schemaClass.read(ctx, f, digest)
}

// ListMCPToolSchemas returns every stored schema, ordered by digest.
func (f *FleetStore) ListMCPToolSchemas(ctx context.Context) ([]coord.MCPToolSchema, error) {
	return schemaClass.list(ctx, f)
}

// DeleteMCPToolSchema removes a definition's schemas at a version.
func (f *FleetStore) DeleteMCPToolSchema(ctx context.Context, digest string, version uint64) (bool, error) {
	if !coord.IsMCPDigest(digest) {
		return false, fmt.Errorf("coord/kv: %q is not a definition digest", digest)
	}
	return schemaClass.remove(ctx, f, digest, version)
}
