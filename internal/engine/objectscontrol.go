package engine

import (
	"context"
	"errors"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/objstore/upkeep"
)

// The operator's gestures on the placement map: take a member out, put it
// back, hold the map through planned maintenance, release the hold.
//
// # Read, decide, compare-and-set — never a message to the duty holder
//
// The map is one record in the coordination store, and whoever holds its duty
// writes it with a compare-and-set. A gesture is the same kind of write from a
// different node: read the record, apply the PURE gesture from
// internal/objstore/upkeep, and write it back at the version read. A gesture
// that raced the maintainer's tick loses the compare-and-set and is applied
// again to what the tick wrote, so neither overwrites the other — and there is
// no second path into the map for the two to disagree about.
//
// The retries are BOUNDED at [mapGestureAttempts]: a map that moved under
// every attempt is a map being written faster than a person can act on it,
// and the honest answer is what it says now and that the gesture did not land,
// rather than a loop that holds an API request until the maintainer pauses.
//
// # What a caller can tell apart
//
// A refusal is one of the gestures' sentinels, so a surface maps each to its
// own answer: [upkeep.ErrNoMap] (no data node has joined yet), and
// [internal/membership]'s ErrRemovedMember (taking out a node the map removed
// for absence, which places nothing already — tested BEFORE the next, which it
// wraps), ErrUnknownMember, ErrNothingPlaceable (taking this member out would
// leave nowhere to write) and ErrHoldRange. A store that did not answer is
// [ErrObjectsUnavailable], and a map this build cannot rewrite is
// [ErrObjectsNewerMap]. A gesture that lost every race is not an error: it
// answers Landed false with the map as it now stands.
//
// # What a gesture sent again does
//
// An out, an in and a release the map already says are ALREADY SO: the
// answer is landed and nothing is written, so the record's version does not
// move under a maintainer's tick and the first operator's who, why and when
// stand. A hold is the exception, by design ([upkeep.HoldFor]): it is the
// operator saying now how much longer the maintenance needs, so it always
// writes, ending its length after the one sent last.

// ErrObjectsUnavailable is a gesture whose read or write the coordination
// store did not answer. It says nothing about the map, which is unchanged or
// changed as far as anyone here can tell; asking again is safe.
var ErrObjectsUnavailable = errors.New("engine: the placement map could not be read or written")

// ErrObjectsNewerMap is a stored map a newer build wrote. This build must not
// rewrite it — a map written back in an older shape drops whatever that build
// added — so the gesture belongs on a node running the newer build.
var ErrObjectsNewerMap = errors.New("engine: the placement map was written by a newer build; " +
	"make this gesture from a node running it")

// objectMapStore is what the gestures need from the coordination store.
type objectMapStore interface {
	ObjectMap(ctx context.Context) (coord.ObjectMapRecord, bool, error)
	UpdateObjectMap(ctx context.Context, value []byte, version uint64) (coord.ObjectMapRecord, bool, error)
}

// objectMapObserver is told every map a gesture wrote, so this node places by
// it at once.
type objectMapObserver interface {
	Observe(state objstore.MapState, version uint64)
}

// ObjectsControl applies an operator's gestures to the placement map. Nil on a
// node that runs no object store.
type ObjectsControl struct {
	store    objectMapStore
	observer objectMapObserver
	now      func() time.Time
}

// ObjectsGesture is what a gesture on the placement map did ([MapGesture]).
type ObjectsGesture = MapGesture[objstore.MapState]

// Out takes a member out of the placement map: nothing new is placed on it,
// and its share is copied to the other members while it keeps serving what it
// holds. by and reason are recorded on the map, for the operator who looks
// next.
func (c *ObjectsControl) Out(ctx context.Context, node, by, reason string) (ObjectsGesture, error) {
	return c.apply(ctx, "out", func(s objstore.MapState) (objstore.MapState, error) {
		return upkeep.Out(s, node, by, reason, c.now())
	}, "node", node, "by", by, "reason", reason)
}

// In puts a member back: the map places on it again.
func (c *ObjectsControl) In(ctx context.Context, node, by string) (ObjectsGesture, error) {
	return c.apply(ctx, "in", func(s objstore.MapState) (objstore.MapState, error) {
		return upkeep.In(s, node)
	}, "node", node, "by", by)
}

// Hold holds the map for d, at most internal/membership's MaxHold: no member
// is removed for absence until it expires or is released.
func (c *ObjectsControl) Hold(ctx context.Context, d time.Duration, by, reason string) (ObjectsGesture, error) {
	return c.apply(ctx, "hold", func(s objstore.MapState) (objstore.MapState, error) {
		return upkeep.HoldFor(s, d, by, reason, c.now())
	}, "for", d.String(), "by", by, "reason", reason)
}

// Release ends a hold.
func (c *ObjectsControl) Release(ctx context.Context, by string) (ObjectsGesture, error) {
	return c.apply(ctx, "release", func(s objstore.MapState) (objstore.MapState, error) {
		return upkeep.Release(s), nil
	}, "by", by)
}

// State reads the stored map as it is now, and false while there is none.
//
// FROM THE STORE, not this node's cache: a gesture's caller wants the map the
// next gesture will be applied to, and the cache is up to a refresh behind it.
func (c *ObjectsControl) State(ctx context.Context) (objstore.MapState, uint64, bool, error) {
	return c.read(ctx)
}

// read is the stored map, decoded for a rewrite.
func (c *ObjectsControl) read(ctx context.Context) (objstore.MapState, uint64, bool, error) {
	return c.record().state(ctx)
}

// apply reads the map, applies the gesture and writes it back at the version
// read, retrying a lost race — the loop both maps share (mapcontrol.go).
func (c *ObjectsControl) apply(ctx context.Context, gesture string,
	change func(objstore.MapState) (objstore.MapState, error), attrs ...any) (ObjectsGesture, error) {

	return c.record().apply(ctx, gesture, change, attrs...)
}

// record is the placement map as the shared gesture loop reads and writes it.
func (c *ObjectsControl) record() casMap[objstore.MapState] {
	m := casMap[objstore.MapState]{
		event: "object_map",
		read: func(ctx context.Context) ([]byte, uint64, bool, error) {
			rec, found, err := c.store.ObjectMap(ctx)
			return rec.Value, rec.Version, found, err
		},
		update: func(ctx context.Context, raw []byte, version uint64) (uint64, bool, error) {
			rec, won, err := c.store.UpdateObjectMap(ctx, raw, version)
			return rec.Version, won, err
		},
		decode:      objstore.DecodeMapStateForUpdate,
		encode:      objstore.MapState.Encode,
		noMap:       upkeep.ErrNoMap,
		unavailable: ErrObjectsUnavailable,
		newer:       ErrObjectsNewerMap,
		fields: func(before, after objstore.MapState) []any {
			return append([]any{"epoch", after.Map.Epoch}, gestureBalance(before, after)...)
		},
	}
	if c.observer != nil {
		m.observe = c.observer.Observe
	}
	return m
}

// gestureBalance is the balance a gesture ran, as log fields — none when it
// ran none (a hold, a release, an out that changed nothing placed).
//
// A LOG FIELD RATHER THAN NOTHING because an out or an in balances on the
// node that served it, never on the node keeping the map, so the gesture's
// own line is the only log that ever carries that balance — and
// `balance_converged=false` here is the same finding it is on the
// maintainer's `object_map_changed`.
func gestureBalance(before, after objstore.MapState) []any {
	if after.Balance == before.Balance || after.Balance.Epoch != after.Map.Epoch {
		return nil
	}
	return []any{"balance_rounds", after.Balance.Rounds,
		"balance_deviation", after.Balance.Deviation,
		"balance_converged", after.Balance.Converged}
}
