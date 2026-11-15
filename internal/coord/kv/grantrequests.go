package kv

import (
	"bytes"
	"context"
	"errors"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/coord"
)

// ---- the agents' grant requests ---------------------------------------- //

// A request is filed in the positions register under its own class, by id:
// see [coord.GrantRequests] for why that bucket, which is the one with no age.

// GrantRequest returns one request, and whether there is one.
func (f *FleetStore) GrantRequest(ctx context.Context, id string) ([]byte, bool, error) {
	if id == "" {
		return nil, false, errors.New("coord/kv: a grant request is read by its id")
	}
	entry, err := f.get(ctx, f.positions, coord.GrantRequestKey(id))
	switch {
	case errors.Is(err, jetstream.ErrKeyNotFound):
		return nil, false, nil
	case err != nil:
		return nil, false, unavailable("read grant request "+id, err)
	}
	// COPIED: the entry's buffer belongs to the client.
	return bytes.Clone(entry.Value()), true, nil
}

// GrantRequests returns every request, by id.
func (f *FleetStore) GrantRequests(ctx context.Context) (map[string][]byte, error) {
	out := map[string][]byte{}
	err := f.eachUnder(ctx, f.positions, coord.GrantRequestFilter(), "the grant requests",
		func(kve jetstream.KeyValueEntry) error {
			if id, ok := coord.GrantRequestID(kve.Key()); ok {
				out[id] = bytes.Clone(kve.Value())
			}
			return nil
		})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// PutGrantRequest stores one request.
func (f *FleetStore) PutGrantRequest(ctx context.Context, id string, value []byte) error {
	if id == "" {
		return errors.New("coord/kv: a grant request needs its id")
	}
	if _, err := f.positions.Put(ctx, coord.GrantRequestKey(id), value); err != nil {
		return unavailable("store grant request "+id, err)
	}
	return nil
}

// DeleteGrantRequest drops one request.
//
// PURGED, the standing rule for a bucket no clock ages: see markers.go.
func (f *FleetStore) DeleteGrantRequest(ctx context.Context, id string) error {
	if id == "" {
		return errors.New("coord/kv: a grant request needs its id")
	}
	err := f.positions.Purge(ctx, coord.GrantRequestKey(id))
	if err != nil && !errors.Is(err, jetstream.ErrKeyNotFound) {
		return unavailable("delete grant request "+id, err)
	}
	return nil
}
