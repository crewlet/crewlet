package engine

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/estate/partmap"
	"github.com/crewlet/crewlet/internal/statelog"
)

// The operator's gestures on the estate map: take a data node out of it, put
// it back, hold the map through planned maintenance, release the hold, and
// move one partition off one node or cancel that move.
//
// # The same read, decide, compare-and-set as the object map's
//
// Each gesture reads the stored map, applies the PURE gesture from
// internal/estate/partmap and writes the result back at the version read,
// through the loop both maps share (mapcontrol.go) — so a gesture that raced
// the maintainer's tick is applied again to what the tick wrote, one whose
// answer is the map as it stands writes nothing, and one that lost every race
// answers Landed false with the map as it now stands. Any node that can reach
// the coordination store may make one: the map is one record there, and a
// gesture is a compare-and-set on it rather than a message to the node that
// holds the duty.
//
// # What a caller can tell apart
//
// [partmap.ErrNoMap] (no estate map), [partmap.ErrUnknownPartition],
// [partmap.ErrNotAHolder] and [partmap.ErrNowhereToMove] for a move, and
// internal/membership's refusals for the rest — ErrRemovedMember tested
// before ErrUnknownMember, which it wraps, and ErrNowhereToRebuild for an out
// no other member could take the copies of, which is a move's refusal made by
// the membership rule both maps share. A hold or a release confirmed by
// no generation at all is [ErrEstateUnconfirmed], and one confirmed for
// another map [ErrEstateOtherMap] — both judged against the stored map, so
// where there is none the refusal is the no-map one below; a store that did
// not answer is [ErrEstateUnavailable], and a map this build cannot rewrite
// [ErrEstateNewerMap].
//
// # Where there is no map
//
// [EstateControl.State] answers that there is none, and EVERY gesture is
// refused with [partmap.ErrNoMap], saying why in the words of the layout THIS
// NODE RUNS, because the two absences send an operator in opposite
// directions. Under the single-file layout — the only one this build runs —
// every data node holds the whole estate and the map's duty writes none, so
// there is no partition to place, move or hold anybody for. At a partitioned
// layout the estate IS divided and no map has been written yet, so nothing is
// placed until the duty writes the first — which is a fleet to wait for or
// look into, never one to be told it holds everything everywhere.

// ErrEstateUnavailable is a gesture whose read or write the coordination store
// did not answer. It says nothing about the map, which is unchanged or changed
// as far as anyone here can tell; asking again is safe.
var ErrEstateUnavailable = errors.New("engine: the estate map could not be read or written")

// ErrEstateNewerMap is a stored estate map a newer build wrote. This build must
// not rewrite it — a map written back in an older shape drops whatever that
// build added — so the gesture belongs on a node running the newer build.
var ErrEstateNewerMap = errors.New("engine: the estate map was written by a newer build; " +
	"make this gesture from a node running it")

// ErrEstateWhole is every gesture's refusal on a node running layout 0, where
// the estate is not divided into partitions and no map will ever be written:
// [partmap.ErrNoMap], in the words every surface gives that layout
// ([partmap.WholeEstate]).
//
// A SENTINEL OF ITS OWN beside the no-map refusal it wraps, because the two
// absences send an operator in opposite directions — see the file's doc — and a
// surface answering them has to tell them apart without reading the sentence:
// this one is a fact about the fleet, answered the same way for as long as it
// runs this layout, and the other a map not written yet, which is a wait.
var ErrEstateWhole = fmt.Errorf("%w: %s", partmap.ErrNoMap, partmap.WholeEstate)

// noEstateMap is the refusal of a gesture where there is no estate map:
// [partmap.ErrNoMap], saying what a fleet with none is on the layout this node
// runs — see the file's doc.
func noEstateMap(running statelog.Layout) error {
	if running.Number == 0 {
		return ErrEstateWhole
	}
	return fmt.Errorf("%w: %s", partmap.ErrNoMap, partmap.Unplaced(running.Number))
}

// estateMapStore is what the gestures need from the coordination store.
type estateMapStore interface {
	EstateMap(ctx context.Context) (coord.EstateMapRecord, bool, error)
	UpdateEstateMap(ctx context.Context, value []byte, version uint64) (coord.EstateMapRecord, bool, error)
}

// EstateGesture is what a gesture on the estate map did ([MapGesture]).
type EstateGesture = MapGesture[partmap.MapState]

// EstateControl applies an operator's gestures to the estate map. Nil on a node
// with no coordination store to hold one.
type EstateControl struct {
	store estateMapStore

	// running is the layout this node runs, which says what a fleet with no
	// map is.
	running statelog.Layout

	now func() time.Time
}

// Out takes a data node out of the estate map: every partition's target stops
// naming it, so what it holds is rebuilt on the others while it keeps serving,
// and then released. by and reason are recorded on the map.
func (c *EstateControl) Out(ctx context.Context, node, by, reason string) (EstateGesture, error) {
	return c.apply(ctx, "out", func(s partmap.MapState) (partmap.MapState, error) {
		return partmap.Out(s, node, by, reason, c.now())
	}, "node", node, "by", by, "reason", reason)
}

// Bar bars a data node from the estate map — an eviction's part in it: taken out
// if it is a member, and recorded whether or not the map holds it, so that
// neither its removal nor its return lifts it; only [EstateControl.In] does.
// by and reason are recorded on the map.
func (c *EstateControl) Bar(ctx context.Context, node, by, reason string) (EstateGesture, error) {
	return c.apply(ctx, "bar", func(s partmap.MapState) (partmap.MapState, error) {
		return partmap.Bar(s, node, by, reason, c.now())
	}, "node", node, "by", by, "reason", reason)
}

// In puts a data node back: the targets may name it again — and vouches for
// one the map removed for absence and has on probation, and lifts a bar.
func (c *EstateControl) In(ctx context.Context, node, by string) (EstateGesture, error) {
	return c.apply(ctx, "in", func(s partmap.MapState) (partmap.MapState, error) {
		return partmap.In(s, node)
	}, "node", node, "by", by)
}

// Hold holds the map for d, at most internal/membership's MaxHold: no member
// is removed for absence until it expires or is released. confirm is what the
// operator repeated to confirm it — the map's generation, as they typed it
// ([ErrEstateUnconfirmed], [ErrEstateOtherMap]).
func (c *EstateControl) Hold(ctx context.Context, confirm string, d time.Duration,
	by, reason string) (EstateGesture, error) {

	return c.apply(ctx, "hold", func(s partmap.MapState) (partmap.MapState, error) {
		if err := sameEstateMap(s, confirm); err != nil {
			return s, err
		}
		return partmap.HoldFor(s, d, by, reason, c.now())
	}, "for", d.String(), "by", by, "reason", reason)
}

// Release ends a hold. confirm is what the operator repeated to confirm it, as
// [EstateControl.Hold]'s is.
func (c *EstateControl) Release(ctx context.Context, confirm, by string) (EstateGesture, error) {
	return c.apply(ctx, "release", func(s partmap.MapState) (partmap.MapState, error) {
		if err := sameEstateMap(s, confirm); err != nil {
			return s, err
		}
		return partmap.Release(s)
	}, "by", by)
}

// ErrEstateUnconfirmed is a hold or a release whose confirmation is no map
// generation at all — nothing repeated, or something that does not parse as
// one.
//
// JUDGED INSIDE THE COMPARE-AND-SET, like [ErrEstateOtherMap], and never by a
// caller before it: a generation is something only a map has, so whether one
// was repeated is a question with no meaning until there is a map to have
// repeated it from. Asked first, it put a refusal no operator could satisfy in
// front of the one that says why — at layout 0 there is no map and no
// generation to copy, so a hold went round in a circle asking for one and the
// fleet's own answer ([ErrEstateWhole]) was never reached.
var ErrEstateUnconfirmed = errors.New("engine: the estate map's generation was not repeated: " +
	"a hold or a release acts on the whole map, and its generation is what says it is " +
	"this fleet's map")

// ErrEstateOtherMap is a hold or a release confirmed for a map other than the
// stored one: another fleet's — a gesture sent through the wrong node — or this
// fleet's before its map was written again from nothing.
//
// THE TWO GESTURES THAT NAME NO NODE ARE CONFIRMED BY THE MAP'S GENERATION,
// where every other gesture repeats the node it moves: each acts on the whole
// map — a hold keeps every gone member's partitions a copy short for as long
// as it says, and a release lets the maintainer remove them at its next tick —
// so the confirmation is what says the operator looked at THIS map. Checked
// INSIDE the compare-and-set, on the record the gesture is applied to, rather
// than against a read beside it.
var ErrEstateOtherMap = errors.New("engine: the estate map is not the one the gesture was " +
	"confirmed for")

// sameEstateMap refuses a gesture whose confirmation is not the stored map's
// generation: [ErrEstateUnconfirmed] where it is no generation at all, and
// [ErrEstateOtherMap] where it is another map's.
func sameEstateMap(s partmap.MapState, confirm string) error {
	generation, err := uuid.Parse(confirm)
	switch {
	case confirm == "":
		return fmt.Errorf("%w: the confirmation was empty", ErrEstateUnconfirmed)
	case err != nil:
		return fmt.Errorf("%w: %q is not a map generation", ErrEstateUnconfirmed, confirm)
	case s.Map.Generation != generation:
		return fmt.Errorf("%w: it was confirmed for generation %s, and the stored map is "+
			"generation %s", ErrEstateOtherMap, generation, s.Map.Generation)
	}
	return nil
}

// Move moves partition p off node: p's copy there is rebuilt on another member
// and then released. It lasts until [EstateControl.CancelMove] or the node
// leaves the map.
func (c *EstateControl) Move(ctx context.Context, p statelog.PartitionID, node, by, reason string) (EstateGesture, error) {
	return c.apply(ctx, "move", func(s partmap.MapState) (partmap.MapState, error) {
		return partmap.Move(s, p, node, by, reason, c.now())
	}, "partition", p.String(), "node", node, "by", by, "reason", reason)
}

// CancelMove lifts a move: p's target may name node again.
func (c *EstateControl) CancelMove(ctx context.Context, p statelog.PartitionID, node, by string) (EstateGesture, error) {
	return c.apply(ctx, "cancel_move", func(s partmap.MapState) (partmap.MapState, error) {
		return partmap.CancelMove(s, p, node)
	}, "partition", p.String(), "node", node, "by", by)
}

// Running is the layout this node runs, which is what a fleet with no estate
// map is read as: the single-file layout — every data node holding the whole
// estate — where it is layout 0, and otherwise a partitioned layout whose map
// has not been written yet.
func (c *EstateControl) Running() statelog.Layout { return c.running }

// EstateMap reads the stored record as it is now, undecoded, and false while
// there is none — for a surface that renders the map as EVERY node reads it,
// which must not refuse a newer build's field the way a gesture's rewrite
// does ([partmap.DecodeMapState] against [partmap.DecodeMapStateForUpdate]).
func (c *EstateControl) EstateMap(ctx context.Context) (coord.EstateMapRecord, bool, error) {
	return c.store.EstateMap(ctx)
}

// State reads the stored map as it is now, and false while there is none —
// which, under the single-file layout this build runs, is always.
//
// FROM THE STORE, not a watched view: a gesture's caller wants the map the next
// gesture will be applied to.
func (c *EstateControl) State(ctx context.Context) (partmap.MapState, uint64, bool, error) {
	return c.record().state(ctx)
}

// apply is one gesture through the loop both maps share.
func (c *EstateControl) apply(ctx context.Context, gesture string,
	change func(partmap.MapState) (partmap.MapState, error), attrs ...any) (EstateGesture, error) {

	return c.record().apply(ctx, gesture, change, attrs...)
}

// record is the estate map as the shared gesture loop reads and writes it.
func (c *EstateControl) record() casMap[partmap.MapState] {
	return casMap[partmap.MapState]{
		event: "estate_map",
		read: func(ctx context.Context) ([]byte, uint64, bool, error) {
			rec, found, err := c.store.EstateMap(ctx)
			return rec.Value, rec.Version, found, err
		},
		update: func(ctx context.Context, raw []byte, version uint64) (uint64, bool, error) {
			rec, won, err := c.store.UpdateEstateMap(ctx, raw, version)
			return rec.Version, won, err
		},
		decode:      partmap.DecodeMapStateForUpdate,
		encode:      partmap.MapState.Encode,
		noMap:       noEstateMap(c.running),
		unavailable: ErrEstateUnavailable,
		newer:       ErrEstateNewerMap,
		fields:      estateGestureFields,
	}
}

// estateGestureFields is what a written gesture's log line adds: the balance it
// ran, when it ran one (an out or an in that changed what the map places).
//
// A LOG FIELD RATHER THAN NOTHING for the object map's reason
// ([gestureBalance]): a gesture balances on the node that served it, never on
// the node keeping the map, so its own line is the only log that carries that
// balance. No epoch moves: a gesture changes the targets, never the holders.
func estateGestureFields(before, after partmap.MapState) []any {
	out := []any{"epoch", after.Map.Epoch}
	if after.Balance == before.Balance {
		return out
	}
	return append(out, "balance_tolerance", after.Balance.Tolerance,
		"balance_rounds", after.Balance.Rounds,
		"balance_deviation", after.Balance.Deviation,
		"balance_converged", after.Balance.Converged)
}

// EstateControl is the operator's gestures on the estate map, nil on a node
// with no coordination store to hold one. Any node that has one may make them:
// the map is one record there, and a gesture is a compare-and-set on it.
func (e *Engine) EstateControl() *EstateControl { return e.estateControl }
