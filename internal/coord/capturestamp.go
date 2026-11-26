package coord

import (
	"context"
	"fmt"
	"time"
)

// A NODE'S CAPTURE STAMP RECORD says, past its presence, the capture stamp
// version its build asks again whole when it settles a run's row
// ([NodeStatus.CaptureStampVersion]): a take keeps a capture's media apart
// only once every node that may settle the run says a version carrying them,
// and an offline node (a seats-only one has no positions row) says it nowhere
// else. Every node writes its own as it starts, before it claims presence.
//
// NO AGE AND NO REMOVAL, as the positions rows beside it: the version is what
// the node asks on its return. A retirement standing against it
// ([NodeRetirement.Stands]) takes a node gone for good out of the count; the
// node back under its id voids that by writing its record again.

// CaptureStampRecord is one node's record, as the register holds it.
type CaptureStampRecord struct {
	// NodeID is the node, and the key.
	NodeID string `json:"node_id"`

	// CaptureStampVersion is the version the node's build asks again whole;
	// zero is a node not saying.
	CaptureStampVersion int `json:"capture_stamp_version"`

	// At is when the node wrote it, by its own clock.
	At time.Time `json:"at"`

	// StoredAt is the instant the store took it, by the clock a retirement
	// is stored by ([NodeRetirement.StoredAt]). Filled by every read, never
	// written.
	StoredAt time.Time `json:"-"`
}

// Validate reports why a record cannot be written.
func (r CaptureStampRecord) Validate() error {
	switch {
	case r.NodeID == "" || !plainWithin(r.NodeID, MaxNodeIDBytes):
		return fmt.Errorf("coord: a capture stamp record needs the node id it is keyed by: "+
			"at most %d plain bytes", MaxNodeIDBytes)
	case r.CaptureStampVersion < 0:
		return fmt.Errorf("coord: node %s's capture stamp version %d is below zero, which "+
			"is a node not saying", r.NodeID, r.CaptureStampVersion)
	}
	return nil
}

// CaptureStampClass is the key class a node's record is filed under in the
// positions register.
const CaptureStampClass = "capture_stamp"

// CaptureStampKey is a node's record's key in the register.
func CaptureStampKey(nodeID string) string { return DocumentKey(CaptureStampClass, nodeID) }

// CaptureStampRecords is the fleet's record of the capture stamp version each
// node asks again whole.
type CaptureStampRecords interface {
	// PutCaptureStamp writes this node's record, replacing it. A plain write:
	// only the node writes its own key.
	PutCaptureStamp(ctx context.Context, r CaptureStampRecord) error

	// CaptureStamps reads every node's record, each with the instant the
	// store took it.
	CaptureStamps(ctx context.Context) ([]CaptureStampRecord, error)
}
