package engine

import (
	"bytes"
	"context"
	"fmt"
)

// AN OPERATOR'S GESTURE ON A PLACEMENT MAP: read the record, apply a PURE
// gesture, write it back at the version read — for the object store's map
// (objectscontrol.go) and the estate map's (estatecontrol.go) alike.
//
// ONE LOOP FOR BOTH MAPS, because what it does is one rule (ADR-0008): a
// gesture that raced the maintainer's tick loses the compare-and-set and is
// applied again to what the tick wrote, so neither overwrites the other; a
// gesture whose answer is the map as it stands writes nothing, so the record's
// version does not move under a maintainer's tick and the first operator's
// who, why and when stand; and the retries are bounded, so a map moving
// faster than a person can act answers what it now says and that the gesture
// did not land. Two copies of that are two answers to "did my gesture land"
// waiting to disagree.

// MapGesture is what a gesture did: whether the map now says what was asked,
// and the map as it stands after the attempt.
type MapGesture[S any] struct {
	// Landed is whether the stored map says what the gesture asked —
	// written by it, or already so. False only when every attempt lost its
	// compare-and-set to another writer.
	Landed bool

	// State is the map after the gesture, or as it stood when the last
	// attempt lost; Version the store's version of it.
	State   S
	Version uint64
}

// mapGestureAttempts is how many compare-and-sets a gesture tries before
// answering that it did not land.
//
// THREE: a race is the maintainer's tick, fifteen seconds apart, or another
// operator — one retry covers the first and a second covers both at once. More
// than that is a map moving faster than the gesture can be reasoned about.
const mapGestureAttempts = 3

// casMap is one placement map's record as a gesture reads and writes it.
type casMap[S any] struct {
	// event names the map in its log lines: object_map, estate_map.
	event string

	// read is the stored record, false when there is none; update writes
	// one at a version, reporting whether that version still held.
	read   func(ctx context.Context) (raw []byte, version uint64, found bool, err error)
	update func(ctx context.Context, raw []byte, version uint64) (uint64, bool, error)

	// decode reads a record THIS NODE MEANS TO REWRITE — refusing one that
	// carries a field this build does not know — and encode renders one.
	decode func([]byte) (S, error)
	encode func(S) ([]byte, error)

	// noMap is the refusal of a gesture on a fleet with no map;
	// unavailable wraps a store that did not answer, and newer a map this
	// build must not rewrite.
	noMap, unavailable, newer error

	// observe, when set, is told every map a gesture wrote.
	observe func(S, uint64)

	// fields is what a written gesture adds to its log line.
	fields func(before, after S) []any
}

// state reads the stored map, decoded for a rewrite, and false while there is
// none.
func (c casMap[S]) state(ctx context.Context) (S, uint64, bool, error) {
	var zero S
	raw, version, found, err := c.read(ctx)
	if err != nil {
		return zero, 0, false, fmt.Errorf("%w: %w", c.unavailable, err)
	}
	if !found {
		return zero, 0, false, nil
	}
	s, err := c.decode(raw)
	if err != nil {
		return zero, 0, false, fmt.Errorf("%w: %w", c.newer, err)
	}
	return s, version, true, nil
}

// apply reads the map, applies the gesture and writes it back at the version
// read, retrying a lost race — see the file's doc.
func (c casMap[S]) apply(ctx context.Context, gesture string, change func(S) (S, error),
	attrs ...any) (MapGesture[S], error) {

	var last MapGesture[S]
	for range mapGestureAttempts {
		state, version, found, err := c.state(ctx)
		if err != nil {
			return MapGesture[S]{}, err
		}
		if !found {
			return MapGesture[S]{}, c.noMap
		}
		next, err := change(state)
		if err != nil {
			return MapGesture[S]{State: state, Version: version}, err
		}
		before, err := c.encode(state)
		if err != nil {
			return MapGesture[S]{}, err
		}
		raw, err := c.encode(next)
		if err != nil {
			return MapGesture[S]{}, err
		}
		// ALREADY SO: the map says what was asked, and a write of the
		// same bytes would move the record's version for nothing — which
		// a racing maintainer would then lose its tick to.
		if bytes.Equal(before, raw) {
			return MapGesture[S]{Landed: true, State: state, Version: version}, nil
		}
		wrote, won, err := c.update(ctx, raw, version)
		if err != nil {
			return MapGesture[S]{}, fmt.Errorf("%w: %w", c.unavailable, err)
		}
		if won {
			if c.observe != nil {
				c.observe(next, wrote)
			}
			fields := []any{"gesture", gesture}
			if c.fields != nil {
				fields = append(fields, c.fields(state, next)...)
			}
			log.InfoContext(ctx, c.event+"_gesture", append(fields, attrs...)...)
			return MapGesture[S]{Landed: true, State: next, Version: wrote}, nil
		}
		last = MapGesture[S]{State: state, Version: version}
	}
	// EVERY ATTEMPT LOST: answer the map as it stands now, which is what
	// the operator decides the next gesture against.
	if state, version, found, err := c.state(ctx); err == nil && found {
		last = MapGesture[S]{State: state, Version: version}
	}
	log.WarnContext(ctx, c.event+"_gesture_not_landed", append([]any{
		"gesture", gesture, "attempts", mapGestureAttempts}, attrs...)...)
	return last, nil
}
