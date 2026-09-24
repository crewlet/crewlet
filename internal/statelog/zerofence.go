package statelog

import (
	"context"
	"fmt"
)

// VerifyZero is the check behind [Fence.ClearForZero], for every domain whose
// fence installs the eviction gate: that publishing at an expectation of ZERO
// is safe from this node.
//
// An expectation of zero says "this subject holds nothing". That is true from
// this node only when it is not evicted and has consumed everything the trim
// may have removed — clause (ii) of the floor theorem in this package's doc,
// verified within the call rather than trusted from a heartbeat. Each way the
// claim cannot be established is a refusal of its own, because each has its
// own remedy:
//
//   - `evicted`: this node's records apply nowhere, and an operator readmits
//     it. Classified exactly as fence 0 classifies it ([evictionRefusal]), so
//     one write path gives one answer for one fact.
//   - `floor_unknown`: the published floor could not be read. THE THIRD VALUE
//     BLOCKS — failing open where a delivery claim is deduplicated costs a
//     duplicate, which is recoverable; failing open here costs a lost update,
//     which is not.
//   - `below_floor`: the trim may have removed records this node never
//     consumed, so an absent anchor may be a record trimmed beneath it. The
//     position is the one this node must reach, which it does by catching up
//     or by adopting a snapshot.
//
// ONE IMPLEMENTATION for every such fence, because the comparison is the
// theorem's and a second copy is one that can drift from it.
func VerifyZero(ctx context.Context, evicted func(context.Context) (bool, error),
	floor func(context.Context) (uint64, error), cursor Position) error {

	gone, err := evicted(ctx)
	if refusal := evictionRefusal(gone, err); refusal != nil {
		return refusal
	}
	if floor == nil {
		return &Unavailable{
			Reason: ReasonFloorUnknown,
			Detail: "no published trim floor is readable on this node, so it " +
				"cannot establish that an absent anchor means a subject nothing " +
				"wrote rather than a record trimmed beneath it",
		}
	}
	at, err := floor(ctx)
	if err != nil {
		return &Unavailable{
			Reason: ReasonFloorUnknown,
			Detail: fmt.Sprintf("the published trim floor could not be read: %v — "+
				"a floor that cannot be read is not a floor that is low, and "+
				"publishing at zero on the guess is a lost update nothing recovers",
				err),
		}
	}
	// THE FLOOR IS THE FIRST SEQUENCE THE TRIM HAS NOT LICENSED REMOVING, so
	// a node that has consumed through the one before it has consumed
	// everything that may be gone. Compared against the cursor itself the
	// check would refuse a node exactly at the floor, which is the ordinary
	// state of every node the instant the trim advances to it.
	if at > cursor.Seq+1 {
		return &Unavailable{
			Reason: ReasonBelowFloor,
			Detail: fmt.Sprintf("the trim may have removed everything below %d and "+
				"this node has consumed through %d, so an absent anchor may be a "+
				"record trimmed beneath it rather than a subject nothing wrote",
				at, cursor.Seq),
			Position: Position{Stream: cursor.Stream, Generation: cursor.Generation,
				Seq: at - 1},
		}
	}
	return nil
}

// evictionRefusal is what a write is told about this node's own eviction
// state, or nil when it may publish.
//
// THE THIRD VALUE BLOCKS. An eviction that cannot be read is not an eviction
// that did not happen, and publishing under it produces durable records every
// node drops.
func evictionRefusal(evicted bool, err error) *Unavailable {
	switch {
	case err != nil:
		return &Unavailable{
			Reason: ReasonEvicted,
			Detail: fmt.Sprintf("this node's own eviction state could not be read: %v", err),
		}
	case evicted:
		return &Unavailable{
			Reason: ReasonEvicted,
			Detail: "this node has been removed from the fleet; nothing it " +
				"publishes will be applied anywhere. An operator readmits it",
		}
	}
	return nil
}
