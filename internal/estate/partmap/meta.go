package partmap

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/membership"
	"github.com/crewlet/crewlet/internal/placement"
	"github.com/crewlet/crewlet/internal/statelog"
)

// What a data node says on its `estate:` lease (coord.ClassEstate): the share
// it offers, whether its store can hold partitions, which layout it runs, the
// map it last acted on, and what it holds of each partition. Its estate
// runtime writes it; the map's maintainer reads every one of them each tick.

// PartitionState is what a node says it is doing with one partition — its own
// account, beside the map's [HolderState] for it.
type PartitionState string

const (
	// PartAdopting is a node installing a copy of the partition: a donor's
	// snapshot, or an empty file a fresh log replays into.
	PartAdopting PartitionState = "adopting"

	// PartCatchingUp is a node applying the partition's logs up to their
	// tails from the copy it has.
	PartCatchingUp PartitionState = "catching_up"

	// PartServing is a node whose copy is established — drained, within
	// the snapshot lag of every log's tail, above the floor, its identity
	// verified — and that answers for the partition.
	PartServing PartitionState = "serving"

	// PartFaulted is a node whose copy stopped applying: it answers for
	// nothing, and holds what it has for an operator.
	PartFaulted PartitionState = "faulted"

	// PartDraining is a node that has started giving the partition up: it
	// publishes nothing more on its logs and serves nothing, and from here
	// the leave is never reversed.
	PartDraining PartitionState = "draining"

	// PartReleased is a node that has given the partition up: its release
	// is on every log and applied, and it counts on none of them.
	PartReleased PartitionState = "released"
)

// PartitionStates is every state, for the enum's own validation.
var PartitionStates = []PartitionState{
	PartAdopting, PartCatchingUp, PartServing, PartFaulted, PartDraining, PartReleased,
}

// Valid reports whether s is a state this build knows.
func (s PartitionState) Valid() bool { return slices.Contains(PartitionStates, s) }

// Meta is one data node's estate lease, decoded.
//
// # Absent is not zero
//
// The facts the map decides by and whose zero would be a claim — whether the
// store is healthy, which layout the node runs — are pointers, and nil is
// "this lease does not say". A node that does not say its store is healthy is
// not a node the map places on (see the package doc); a lease that names no
// layout is not one running the map's.
type Meta struct {
	// Weight is the share of the estate the node offers
	// (store.estate.weight, resolved), 1..placement.MaxWeight. A lease with
	// none readable offers nothing: see [MetaFromLease].
	Weight int

	// Labels are the node's own node.labels, which the map's failure domain
	// is read from.
	Labels map[string]string

	// Layout is the layout number the node runs, and nil when the lease
	// does not say.
	Layout *int

	// MapGeneration and MapEpoch are the map the node last ACTED on: its
	// generation, and its epoch within that generation. The epoch is
	// comparable with a holder's Since only within one generation — a map
	// recreated after its key was lost starts its epochs again, and an old
	// generation's epoch 57 is no proof of having read a new one's epoch 3.
	MapGeneration uuid.UUID
	MapEpoch      uint64

	// Partitions is what the node holds of each partition, by partition
	// name: EVERY partition it has a file of, in whatever state, so a
	// partition it does not list is one it holds nothing of — which is how a
	// node that was told to leave a partition it never adopted says it has
	// gone. Nil when the lease does not say, which says nothing either way.
	Partitions map[string]PartitionState

	// FreeBytes is what is free on the volume the node's partition files
	// are on.
	FreeBytes int64

	// Healthy is whether the node's store can hold partitions, and nil when
	// the lease does not say. Detail is what it said about it.
	Healthy *bool
	Detail  string

	// Building is the held partitions whose lexical index is still being
	// built on this node, so a search prefers another holder.
	Building []string
}

// The lease's keys, one per fact.
const (
	metaWeight        = "weight"
	metaLabels        = "labels"
	metaLayout        = "layout"
	metaMapGeneration = "map_generation"
	metaMapEpoch      = "map_epoch"
	metaPartitions    = "partitions"
	metaFreeBytes     = "free_bytes"
	metaHealthy       = "healthy"
	metaDetail        = "detail"
	metaBuilding      = "building"
)

// ErrMetaIncomplete is a lease this build refuses to write because it does not
// say what the map decides by.
var ErrMetaIncomplete = errors.New("estate/partmap: the estate lease is incomplete")

// Encode renders the lease's Meta, refusing one that leaves out what the map
// decides by: a weight in range, the layout, and whether the store is
// healthy — a writer that does not know says unhealthy and why.
func (m Meta) Encode() (map[string]any, error) {
	switch {
	case m.Weight < 1 || m.Weight > placement.MaxWeight:
		return nil, fmt.Errorf("%w: weight %d, want 1..%d", ErrMetaIncomplete, m.Weight,
			placement.MaxWeight)
	case m.Layout == nil:
		return nil, fmt.Errorf("%w: it names no layout", ErrMetaIncomplete)
	case m.Healthy == nil:
		return nil, fmt.Errorf("%w: it does not say whether the store is healthy", ErrMetaIncomplete)
	}
	out := map[string]any{
		metaWeight:     m.Weight,
		metaLayout:     *m.Layout,
		metaMapEpoch:   m.MapEpoch,
		metaFreeBytes:  m.FreeBytes,
		metaHealthy:    *m.Healthy,
		metaPartitions: map[string]any{},
	}
	if m.MapGeneration != uuid.Nil {
		out[metaMapGeneration] = m.MapGeneration.String()
	}
	if len(m.Labels) > 0 {
		out[metaLabels] = maps.Clone(m.Labels)
	}
	parts := out[metaPartitions].(map[string]any)
	for id, state := range m.Partitions {
		parts[id] = string(state)
	}
	if m.Detail != "" {
		out[metaDetail] = m.Detail
	}
	if len(m.Building) > 0 {
		out[metaBuilding] = slices.Clone(m.Building)
	}
	return out, nil
}

// MetaFromLease reads a peer's estate lease, reporting false when it offers no
// share at all — no weight, or one that is not a whole number from 1 up.
//
// EVERY OTHER FIELD FAILS SOFT, one at a time, for the object lease's reason: a
// peer's malformed report must not take a working member out of the map. An
// unreadable health or layout is ABSENT, which the map reads as not saying; an
// unreadable map epoch is none; a partition named in a way no layout names is
// dropped, and a state this build does not know is kept as it was written —
// neither serving nor released, so it promotes and removes nobody.
//
// A lease's Meta is a map[string]any a backend is free to round-trip through
// JSON — an int comes back a float64, a map a map[string]any — so each field is
// read by re-encoding the value it holds and decoding it as the type it should
// be, which accepts either form and refuses a fraction where a count belongs.
func MetaFromLease(meta map[string]any) (Meta, bool) {
	weight, ok := field[int](meta, metaWeight)
	if !ok || weight < 1 {
		return Meta{}, false
	}
	out := Meta{Weight: weight}
	out.Labels, _ = field[map[string]string](meta, metaLabels)
	if layout, ok := field[int](meta, metaLayout); ok {
		out.Layout = &layout
	}
	out.MapGeneration, _ = field[uuid.UUID](meta, metaMapGeneration)
	out.MapEpoch, _ = field[uint64](meta, metaMapEpoch)
	if parts, ok := field[map[string]string](meta, metaPartitions); ok {
		out.Partitions = make(map[string]PartitionState, len(parts))
		for id, state := range parts {
			if _, err := statelog.ParsePartitionID(id); err == nil {
				out.Partitions[id] = PartitionState(state)
			}
		}
	}
	out.FreeBytes, _ = field[int64](meta, metaFreeBytes)
	if healthy, ok := field[bool](meta, metaHealthy); ok {
		out.Healthy = &healthy
	}
	out.Detail, _ = field[string](meta, metaDetail)
	out.Building, _ = field[[]string](meta, metaBuilding)
	return out, true
}

// field is one field of a lease's Meta as a T, and false when it is absent or
// is not one.
func field[T any](meta map[string]any, key string) (T, bool) {
	var out T
	v, ok := meta[key]
	if !ok || v == nil {
		return out, false
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return out, false
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		var zero T
		return zero, false
	}
	return out, true
}

// Presence is one live estate lease as the maintainer reads it.
type Presence struct {
	// Node is the node the lease names.
	Node string

	// Meta is what the lease says.
	Meta Meta
}

// PresenceOf reads one live estate lease, reporting false for a lease of
// another class or one that offers no share ([MetaFromLease]).
func PresenceOf(lease coord.Lease) (Presence, bool) {
	node, ok := coord.EstateNode(lease.Resource)
	if !ok || node == "" {
		return Presence{}, false
	}
	meta, ok := MetaFromLease(lease.Meta)
	if !ok {
		return Presence{}, false
	}
	return Presence{Node: node, Meta: meta}, true
}

// membership is the membership half of a presence, for the tick: a node that
// does not say its store is healthy, or does not run the map's layout, is
// counted as an unhealthy one, with what it said instead as the detail an
// operator reads.
func (p Presence) membership(layout int) membership.Presence {
	mp := membership.Presence{Node: p.Node, Weight: p.Meta.Weight, Labels: p.Meta.Labels}
	switch m := p.Meta; {
	case m.Healthy == nil:
		mp.Unhealthy = true
		mp.Detail = "its estate lease does not say whether its store is healthy"
	case !*m.Healthy:
		mp.Unhealthy = true
		mp.Detail = m.Detail
	case m.Layout == nil:
		mp.Unhealthy = true
		mp.Detail = "its estate lease does not say which layout it runs"
	case *m.Layout != layout:
		mp.Unhealthy = true
		mp.Detail = fmt.Sprintf("it runs layout %d and the map places layout %d", *m.Layout, layout)
	}
	return mp
}
