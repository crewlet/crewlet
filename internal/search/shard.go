package search

import "hash/fnv"

// SEARCH SHARDS: how a corpus is divided so a scan can be measured, and later
// split, without any node needing a plan. A shard is a stable hash of a
// document's own identity into a fixed number of buckets, UNRELATED to anything
// else the engine partitions on (stream, project, container, log position,
// holding node). So a SOURCE THAT MOVES KEEPS ITS BUCKET (a re-file would otherwise hide it from a scan of the old bucket,
// silently), the DISTRIBUTION FOLLOWS THE CORPUS, and a DOCUMENT WITH NO
// EMBEDDING IS STILL IN A BUCKET, as the bucket is a function of its id.
//
// Every query still consults every bucket: there is NO ROUTING PLAN, a single
// node takes all of them, and the column makes a scan's cost measurable per
// BUCKET RANGE, the measurement the fan-out is decided from.

// SearchShards is how many buckets a corpus is divided into.
//
// SIXTY-FOUR, chosen for what it divides: 64 nodes take one each, 8 take eight,
// 3 take 21 or 22, and as a power of two an assignment is a contiguous range
// with no remainder.
//
// FIXED for the life of a deployment: changing it re-buckets every document (a
// full index rebuild), so it is deliberately not configurable.
const SearchShards = 64

// ShardOf is a document's bucket.
//
// FNV-1a over the source kind and the id, which is the cheapest stable hash in
// the standard library and needs no allocation per call — this runs once per
// indexed document and once per embedded one, and it must produce the same
// answer on every node for the life of the deployment.
//
// The SOURCE KIND is in the hash because a document's identity is the PAIR:
// both corpora mint their own uuids, independently, and every table that
// carries a document keys it on (source, id). Hashing the id alone would make
// the bucket a function of something narrower than the identity it is filed
// under — and hashing the pair costs one byte of input.
func ShardOf(source, id string) int {
	h := fnv.New64a()
	_, _ = h.Write([]byte(source))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(id))
	return int(h.Sum64() % uint64(SearchShards))
}

// ChatShardOf is a chat message's bucket.
//
// ONE RULE FOR BOTH ESTATES that file a message: the chat applier stamps it on
// the replicated row and the chat keyword index ([ChatIndexer]) on its document.
// Two copies could disagree, and a fan-out range such as [16, 32) would look for
// a message in the wrong bucket: one fewer hit, silently.
//
// A NAMESPACE OF ITS OWN rather than a member of [Source]: that enum is what the
// embedding duty walks, so a value added for chat would enrol the transcript in
// the semantic half.
func ChatShardOf(messageID string) int { return ShardOf(chatShardSource, messageID) }

// chatShardSource is the namespace [ChatShardOf] hashes a message under.
const chatShardSource = "message"

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
