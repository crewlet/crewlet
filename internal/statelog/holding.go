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
// append ([Deps.Holding]).
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

// Copies is which partitions THIS node keeps an established copy of: the ones
// it takes snapshots of and offers a joiner ([DonorDeps.Keeps]).
//
// NOT [Holding], which is whether this node may WRITE a partition's logs, and
// the two part exactly where a partition is short of copies. A copy of a log is
// the same function of the log's records on every node that applies them,
// whoever may write: a holder the estate map is moving away keeps applying
// until its leave begins, and so does one an eviction barred from the map,
// whose own records each log drops but whose applier applies everyone's. Either
// may be the partition's ONLY copy — the last server of a partition whose other
// holders were lost, a barred machine back with its files after its eviction —
// and held to the write rule neither offered it, so a joiner of that partition
// had nowhere to fetch from, the partition never had a serving holder again,
// and every gesture that has to reach its logs (a readmission among them) could
// never finish.
//
// A copy is kept from the moment it is adopted and catching up until the node
// begins to give it up: never while a fetched file is being installed over it,
// and never once it has faulted, since a copy that diverged is not one to hand
// on.
type Copies interface {
	// Keeps reports whether this node keeps an established copy of p now.
	//
	// THREE-VALUED, as [Holding.Serving] is: an error is "cannot tell", and
	// a copy nobody can vouch for is not one to offer.
	Keeps(p PartitionID) (bool, error)
}

// ServesOnly is a [Holding] over a fixed set of partitions: what a node whose
// partitions do not change while it runs answers — every node under layout 0,
// where the one partition is held from boot by a data node and by nothing else.
// No partitions at all is a node that serves nothing, which is the honest
// answer for one that holds no data.
func ServesOnly(partitions ...PartitionID) Holding {
	return fixedSet(slices.Clone(partitions))
}

// KeepsOnly is [ServesOnly]'s [Copies]: a fixed set of partitions this node
// keeps a copy of, which on a node whose partitions do not change while it runs
// is the set it serves.
func KeepsOnly(partitions ...PartitionID) Copies {
	return fixedSet(slices.Clone(partitions))
}

// fixedSet is [ServesOnly]'s and [KeepsOnly]'s answer. Its zero value serves
// and keeps nothing.
type fixedSet []PartitionID

// Serving reports whether p is one of the set's partitions. A fixed set is read
// from memory and cannot fail to answer.
func (h fixedSet) Serving(p PartitionID) (bool, error) {
	return slices.Contains(h, p), nil
}

// Keeps reports whether p is one of the set's partitions, as [fixedSet.Serving].
func (h fixedSet) Keeps(p PartitionID) (bool, error) {
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
