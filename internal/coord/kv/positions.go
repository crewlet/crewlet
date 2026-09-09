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
		// THE BUCKET HOLDS TWO KEY CLASSES — a node's positions and a
		// trim hold — so the listing filters, and this filter is the
		// load-bearing half of putting them together. A hold decoded as
		// a positions row yields an EMPTY node id and a domains map of
		// zero values, which the trim reads as a node that has applied
		// nothing: the pin becomes a permanent floor at zero, from a key
		// nobody thinks of as a node.
		segments, ok := coord.DocumentSegments(key)
		if !ok || len(segments) < 2 || segments[0] != "node" {
			continue
		}
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

// PutHold writes or renews a trim hold.
//
// IN THE POSITIONS BUCKET, under its own key class. That is a retention
// decision: this is the one bucket with no age at all, and a hold that expired
// on a timer would release the pin while its owner was still reading — the
// exact hole the hold exists to close. What bounds a crashed holder is the
// trim arithmetic's own stale window, which the trim can report.
func (f *FleetStore) PutHold(ctx context.Context, h coord.TrimHold) error {
	if err := h.Validate(); err != nil {
		return err
	}
	if h.At.IsZero() {
		h.At = time.Now().UTC()
	}
	body, err := json.Marshal(h)
	if err != nil {
		return fmt.Errorf("coord: encode the trim hold held by %s: %w", h.Owner, err)
	}
	if _, err := f.positions.Put(ctx, coord.HoldKey(h.Owner), body); err != nil {
		return unavailable("write a trim hold", err)
	}
	return nil
}

// Holds reads every live pin.
//
// A DECODE FAILURE IS RAISED, for the reason the positions listing gives and
// with the sign reversed: a hold silently dropped RAISES the trim floor, which
// deletes records the holder is in the middle of copying. "I could not read a
// hold" has to be louder than "there are no holds".
func (f *FleetStore) Holds(ctx context.Context) ([]coord.TrimHold, error) {
	keys, err := f.positions.ListKeys(ctx)
	if err != nil {
		return nil, unavailable("list the trim holds", err)
	}
	defer func() { _ = keys.Stop() }()

	var out []coord.TrimHold
	for key := range keys.Keys() {
		// THE BUCKET HOLDS TWO KEY CLASSES, so the listing filters. A
		// positions row decoded as a hold would answer Validate's
		// questions with zero values and pin nothing, which is the
		// quiet version of not reading it at all.
		segments, ok := coord.DocumentSegments(key)
		if !ok || len(segments) < 2 || segments[0] != "hold" {
			continue
		}
		entry, err := f.positions.Get(ctx, key)
		switch {
		case errors.Is(err, jetstream.ErrKeyNotFound):
			// Released between the listing and the read, which is the
			// normal path landing mid-sweep rather than a fault.
			continue
		case err != nil:
			return nil, unavailable("read a trim hold", err)
		}
		var hold coord.TrimHold
		if err := json.Unmarshal(entry.Value(), &hold); err != nil {
			return nil, fmt.Errorf("coord: the trim hold at %s does not "+
				"decode: a hold skipped here raises the trim floor and "+
				"deletes records its holder is copying: %w", key, err)
		}
		out = append(out, hold)
	}
	return out, ctx.Err()
}

// ReleaseHold removes one.
//
// PURGE rather than delete, on this estate's standing rule: a delete leaves a
// tombstone revision that outlives the deployment, and this listing would
// return it as a hold with no owner and no domains.
func (f *FleetStore) ReleaseHold(ctx context.Context, owner string) error {
	if err := f.positions.Purge(ctx, coord.HoldKey(owner)); err != nil &&
		!errors.Is(err, jetstream.ErrKeyNotFound) {
		return unavailable("release a trim hold", err)
	}
	return nil
}
