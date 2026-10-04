package statelog

import "slices"

// Copies is which partitions THIS node keeps an established copy of: the ones
// it takes snapshots of and offers a joiner ([DonorDeps.Keeps]).
//
// NOT whether this node may WRITE: a copy of a log is the same function of the
// log's records on every node that applies them, whoever may write — an evicted
// node's own records each log drops, but its applier applies everyone's. Such a
// node may hold the fleet's ONLY copy — a barred machine back with its files
// after its eviction — and held to the write rule it would offer nothing, so a
// joiner had nowhere to fetch from and every gesture that has to reach the logs
// (a readmission among them) could never finish.
//
// A copy is kept from the moment it is adopted and catching up until the node
// begins to give it up: never while a fetched file is being installed over it,
// and never once it has faulted, since a copy that diverged is not one to hand
// on.
type Copies interface {
	// Keeps reports whether this node keeps an established copy of p now.
	//
	// THREE-VALUED: an error is "cannot tell", and a copy nobody can vouch
	// for is not one to offer.
	Keeps(p PartitionID) (bool, error)
}

// KeepsOnly is a [Copies] over a fixed set of partitions this node keeps a copy
// of: what a node whose partitions do not change while it runs answers. No
// partitions at all is a node that keeps nothing, which is the honest answer
// for one that holds no data.
func KeepsOnly(partitions ...PartitionID) Copies {
	return fixedSet(slices.Clone(partitions))
}

// fixedSet is [KeepsOnly]'s answer. Its zero value keeps nothing.
type fixedSet []PartitionID

// Keeps reports whether p is one of the set's partitions. A fixed set is read
// from memory and cannot fail to answer.
func (h fixedSet) Keeps(p PartitionID) (bool, error) {
	return slices.Contains(h, p), nil
}
