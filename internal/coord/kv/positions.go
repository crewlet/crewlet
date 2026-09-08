package kv

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/coord"
)

// PutPositions writes this node's row in the register.
//
// A plain Put rather than a compare-and-set: the only writer of a node's row
// is that node, so there is nothing to arbitrate, and a CAS would make a
// heartbeat fail on a value only this process can have changed.
func (f *FleetStore) PutPositions(ctx context.Context, p coord.NodePositions) error {
	if err := p.Validate(); err != nil {
		return err
	}
	if p.At.IsZero() {
		p.At = time.Now().UTC()
	}
	body, err := json.Marshal(p)
	if err != nil {
		return fmt.Errorf("coord: encode the positions of node %s: %w", p.NodeID, err)
	}
	if _, err := f.positions.Put(ctx, coord.PositionKey(p.NodeID), body); err != nil {
		return unavailable("write the node's log positions", err)
	}
	return nil
}

// Positions reads every node's row.
//
// A DECODE FAILURE IS RAISED, not skipped. The trim takes a minimum across
// these rows, so a row silently dropped raises that minimum — which is the
// engine deleting records a node still needs, reported by nothing. "I could
// not read node X" has to be louder than "there is no node X".
func (f *FleetStore) Positions(ctx context.Context) ([]coord.NodePositions, error) {
	keys, err := f.positions.ListKeys(ctx)
	if err != nil {
		return nil, unavailable("list the log positions", err)
	}
	defer func() { _ = keys.Stop() }()

	var out []coord.NodePositions
	for key := range keys.Keys() {
		entry, err := f.positions.Get(ctx, key)
		switch {
		case errors.Is(err, jetstream.ErrKeyNotFound):
			// Removed between the listing and the read, which is an
			// eviction landing mid-sweep rather than a fault.
			continue
		case err != nil:
			return nil, unavailable("read a node's log positions", err)
		}
		var row coord.NodePositions
		if err := json.Unmarshal(entry.Value(), &row); err != nil {
			return nil, fmt.Errorf("coord: the positions row at %s does not "+
				"decode: the trim takes a minimum across these rows, so a row "+
				"skipped here raises that minimum and deletes records a node "+
				"still needs: %w", key, err)
		}
		out = append(out, row)
	}
	return out, ctx.Err()
}

// ForgetPositions removes a node's row.
//
// PURGE rather than delete, on this estate's standing rule: a delete leaves a
// tombstone revision that outlives the deployment, and a register listing that
// returned tombstones would be a fleet view with ghosts in it.
func (f *FleetStore) ForgetPositions(ctx context.Context, nodeID string) error {
	if err := f.positions.Purge(ctx, coord.PositionKey(nodeID)); err != nil &&
		!errors.Is(err, jetstream.ErrKeyNotFound) {
		return unavailable("forget a node's log positions", err)
	}
	return nil
}
