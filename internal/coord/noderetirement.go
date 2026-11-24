package coord

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/iam"
)

// A NODE'S RETIREMENT is an operator's assertion, for the whole fleet and
// past any one capacity window, that a node id will never run again: its
// machine is gone for good (a StatefulSet scaled down, a host rebuilt under a
// new id). The positions register keeps every node id's row for life
// ([PositionRegister]), and every row's node takes part in each capacity
// window ([MaxMaintenanceParticipants]), so without it ids long gone would
// count against every window for ever.
//
// NOT AN EVICTION, which says whose records apply: an evicted node may still
// run and queue a request. It is VOID once the node is back, by a row stored
// after it or live presence.

// NodeRetirement is one retired node id, as the register holds it.
type NodeRetirement struct {
	// NodeID is the node retired, and the key.
	NodeID string `json:"node_id"`

	// By, ByKind and OperatorID are who asserted it, as [SeatPause] records
	// them, and At when, by the writer's clock.
	By         string        `json:"by"`
	ByKind     iam.ActorKind `json:"by_kind"`
	OperatorID string        `json:"operator_id,omitempty"`
	At         time.Time     `json:"at"`

	// StoredAt is the instant the store took it, by the store's clock, the
	// clock every node's positions row is stored by ([NodePositions.StoredAt]).
	// Filled by every read, never written.
	StoredAt time.Time `json:"-"`
}

// Validate reports why a retirement cannot be written.
func (r NodeRetirement) Validate() error {
	switch {
	case strings.TrimSpace(r.NodeID) == "":
		return errors.New("coord: a retirement names no node, and the node is its key")
	case !plainWithin(r.NodeID, MaxNodeIDBytes):
		return errors.New("coord: a retirement names no node id: at most 64 plain bytes")
	case r.By == "" || !r.ByKind.Valid():
		return errors.New("coord: a retirement needs who asserted it")
	case r.At.IsZero():
		return errors.New("coord: a retirement needs when it was asserted")
	}
	return checkActor("a retirement", r.By, r.OperatorID)
}

// Stands reports whether the retirement still holds against the node's row,
// stored at rowStored (zero for no row): void once the node wrote it after
// the retirement was stored, since a node writing is a node running.
func (r NodeRetirement) Stands(rowStored time.Time) bool { return !rowStored.After(r.StoredAt) }

// RetirementClass is the key class a retirement is filed under in the
// positions register.
const RetirementClass = "retired"

// RetirementKey is a node's retirement's key in the register.
func RetirementKey(nodeID string) string { return DocumentKey(RetirementClass, nodeID) }

// NodeRetirements is the fleet's record of retired node ids. Like the rows it
// answers for, it has no age and no removal: a node back under its id voids
// its own retirement ([NodeRetirement.Stands]).
type NodeRetirements interface {
	// RetireNode writes or replaces a node's retirement.
	RetireNode(ctx context.Context, r NodeRetirement) error

	// NodeRetirements reads every retirement, each with the instant the
	// store took it.
	NodeRetirements(ctx context.Context) ([]NodeRetirement, error)
}
