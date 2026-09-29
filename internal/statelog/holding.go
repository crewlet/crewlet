package statelog

import (
	"errors"
	"fmt"
	"slices"
)

// Holding is whether THIS node may write a partition's logs: whether it SERVES
// the partition.
//
// DECLARED HERE because the write authority is the caller, and what it needs is
// one answer per write. The engine supplies it: a node serves a partition from
// the moment its join has established every one of the partition's logs until
// the moment its leave stops deciding, and never while it does not hold the
// partition at all.
//
// # Why the writers of a log have to be exactly its serving holders
//
// The floor theorem's premise is that EVERY WRITER IS COUNTED — that the trim
// never removes a record a node that may still decide a write has not applied
// (the package doc's "THE FLOOR THEOREM"). A partition's holders are counted on
// its logs from the moment they begin to join, so a node that writes only while
// it serves is a node the trim is already waiting for. One that wrote a log it
// does not hold — a router's stale choice, a leaver still deciding after it
// began to go — would decide from rows nobody counts, and a retry at zero on its
// behalf could overwrite a committed record the trim had removed.
//
// The publisher asks it before a write takes its snapshot and again before the
// append ([Deps.Holding]), and a node's RELEASE of a log is the one record it
// inverts the rule for ([Request.Release]).
type Holding interface {
	// Serving reports whether this node serves p now.
	//
	// THREE-VALUED: an error is "cannot tell", which is neither yes nor no,
	// and the write it was asked for is refused — failing open would be the
	// uncounted writer this exists to exclude.
	Serving(p PartitionID) (bool, error)
}

// ErrNotHolder reports a write asked of a node that does not serve the
// partition of the log it would be written to — gate 3 of the three the floor
// theorem is held by per log (the package doc's "who may write a log"). Another
// node that serves the partition takes the write; a refusal carrying it is
// [ReasonNotHolder].
var ErrNotHolder = errors.New("statelog: this node does not serve that partition")

// ErrReleaseWhileServing reports a release asked of a node that still serves
// the log's partition.
//
// A PROGRAMMING ERROR IN THE LEAVE, never a state to wait out: a node stops
// deciding writes for a partition before it releases the partition's logs, so
// that everything it decided is on the log below its release. A release
// published while it still serves is one its own later writes land above —
// every one of them dropped on every holder, and every caller told `released`
// about a node that had not yet left.
var ErrReleaseWhileServing = errors.New("statelog: a release is published after " +
	"the node stops serving the partition, never while it serves it")

// ServesOnly is a [Holding] over a fixed set of partitions: what a node whose
// partitions do not change while it runs answers — every node under layout 0,
// where the one partition is held from boot by a data node and by nothing else.
// No partitions at all is a node that serves nothing, which is the honest
// answer for one that holds no data.
func ServesOnly(partitions ...PartitionID) Holding {
	return fixedHolding(slices.Clone(partitions))
}

// fixedHolding is [ServesOnly]'s answer. Its zero value serves nothing.
type fixedHolding []PartitionID

// Serving reports whether p is one of the set's partitions. A fixed set is read
// from memory and cannot fail to answer.
func (h fixedHolding) Serving(p PartitionID) (bool, error) {
	return slices.Contains(h, p), nil
}

// refuseNotHolder is gate 3's refusal of a write this node may not make on
// log's partition because it does not serve it: `not_holder`.
func refuseNotHolder(log LogID, opID string) *Unavailable {
	return &Unavailable{
		Reason: ReasonNotHolder,
		Detail: fmt.Sprintf("this node does not serve %s, whose log %s is — only "+
			"a node that serves a partition writes to its logs, so the write goes "+
			"to one that does", log.Partition, log),
		OpID:  opID,
		Cause: ErrNotHolder,
	}
}

// refuseHoldingUnknown is gate 3's refusal of a write on a node that could not
// tell whether it serves log's partition: `holding_unknown`, carrying why.
func refuseHoldingUnknown(log LogID, opID string, err error) *Unavailable {
	return &Unavailable{
		Reason: ReasonHoldingUnknown,
		Detail: fmt.Sprintf("this node could not tell whether it serves %s, whose "+
			"log %s is, so it writes nothing there — a node that serves the "+
			"partition can take the write, and this one can once it can tell "+
			"again: %v", log.Partition, log, err),
		OpID:  opID,
		Cause: err,
	}
}
