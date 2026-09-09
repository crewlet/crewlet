package search

import "hash/fnv"

// SEARCH SHARDS: how a corpus is divided so a scan can be measured, and later
// split, without any node needing a plan.
//
// # What a shard is, and what it is deliberately not
//
// A shard is a stable hash of a document's own identity into a fixed number of
// buckets. It is UNRELATED to anything else the engine partitions on — not a
// stream, not a project, not a container, not a log position, not the node
// that holds the row. That independence is the whole value:
//
//   - A SOURCE THAT MOVES KEEPS ITS BUCKET. Sharding on the filed project
//     would re-bucket every task a re-file touches, and a query scanning the
//     old bucket would miss it — silently, because a search that returns one
//     fewer result looks exactly like a search over a corpus that has one
//     fewer document.
//   - THE DISTRIBUTION FOLLOWS THE CORPUS, not the company's shape. Sharding
//     on a project puts a company's busiest project in one bucket, and the
//     node holding it scans as much as the whole fleet did.
//   - A DOCUMENT WITH NO EMBEDDING IS STILL IN A BUCKET, because the bucket is
//     a function of its id rather than of anything derived from it. A scheme
//     keyed on the vector would leave an un-embedded document out of every
//     assignment.
//
// # Why every query still consults every bucket
//
// There is NO ROUTING PLAN here and nothing computes one. A single node takes
// every bucket, which is exactly what it does today; what the column buys now
// is that a scan's cost is measurable per BUCKET RANGE rather than per corpus
// held, which is the measurement the fan-out is decided from.

// SearchShards is how many buckets a corpus is divided into.
//
// SIXTY-FOUR, and the number is chosen for what it divides rather than for
// what it holds: it is a fleet's worth of assignments at every size an
// operator will run — 64 nodes take one each, 8 take eight, 3 take 21 or 22 —
// and it is a power of two, so an assignment is a contiguous range with no
// remainder to hand to somebody.
//
// It is FIXED for the life of a deployment. Changing it re-buckets every
// document, which costs a full index rebuild rather than a rebalance, so it is
// deliberately not configurable: a knob that can only be turned once, at the
// price of the whole index, is a knob that will be turned by somebody who did
// not know the price.
const SearchShards = 64

// ShardOf is a document's bucket.
//
// FNV-1a over the source kind and the id, which is the cheapest stable hash in
// the standard library and needs no allocation per call — this runs once per
// indexed document and once per embedded one, and it must produce the same
// answer on every node for the life of the deployment.
//
// The SOURCE KIND is in the hash because the ids are not one namespace: a page
// is a uuid and a work item is a project key, and two corpora hashed
// separately would each be even while their union was not.
func ShardOf(source, id string) int {
	h := fnv.New64a()
	_, _ = h.Write([]byte(source))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(id))
	return int(h.Sum64() % uint64(SearchShards))
}

// Assignment is the set of buckets one scan may read.
//
// EMPTY MEANS EVERY BUCKET, which is what a single node has and what every
// query on a fleet that has not divided its corpus gets. It is the zero value
// on purpose: a caller that forgot to say which buckets it holds scans them
// all, which is slow rather than wrong — where a zero value meaning "no
// buckets" would answer every search with nothing.
type Assignment struct {
	// From and To bound a CONTIGUOUS range, half-open at the top. A range
	// rather than a set because that is what a power-of-two split produces
	// and what a SQL predicate can read as an index seek; a set would be
	// an IN clause of up to sixty-four terms on the hottest scan there is.
	From, To int
}

// Everything is the assignment a node with no fan-out has.
func Everything() Assignment { return Assignment{} }

// Covers reports whether this assignment names a bounded range.
func (a Assignment) Covers() bool { return a.To > a.From }

// Width is how many buckets this assignment holds.
//
// AN UNBOUNDED ASSIGNMENT IS [SearchShards] WIDE, not zero, on the same
// reasoning [Everything] rests on: a range that covers everything holds
// everything, and reporting nought would make a solo node's search claim it
// scanned none of the corpus it in fact scanned all of.
func (a Assignment) Width() int {
	if !a.Covers() {
		return SearchShards
	}
	return a.To - a.From
}

// Contains reports whether one shard is in this assignment.
func (a Assignment) Contains(shard int) bool {
	if !a.Covers() {
		return true
	}
	return shard >= a.From && shard < a.To
}
