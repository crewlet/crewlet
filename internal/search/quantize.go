// Package search is the arithmetic behind the engine's semantic half: the
// 1-bit quantization its first retrieval stage scans, the Hamming distance
// that orders it, the inverted file that makes the first stage an INDEX
// (ADR-0028, ivf.go), the two-stage composition itself, and the rank fusion
// that joins it to the lexical half.
//
// # The first stage is an index, and the index is replicated state
//
// The corpus's sign codes are filed in lists by k-means in Hamming space, and
// a search reads the lists nearest its own code instead of every row — the
// exact rerank above it unchanged. The index is trained by the embedding duty,
// MEASURED against the exact scan before it is installed, and published on the
// vector log as a record every data node applies, so every copy of the estate
// holds the same centroids and files every row in the same list. How many
// lists a search reads is that measurement's answer, never a constant, and a
// corpus with no structure a k-means can find gets no index
// at all: the full scan stays its first stage (below [IVFMinCorpus], with no
// topics a list can find, or in a space the index was not trained in). Per-node
// training would give every data node a different index, so one holder would
// find what another misses. The index's generation is the packed log position
// of its centroids record, since a counter is minted twice by an unresolved
// publish. Older rows are re-filed in reassign batches on later ticks, their
// key ranges cut once by the training and carried in the centroids record,
// each batch's scope nested under the centroids': ranges re-cut at each
// publication left a compacted log a replaying node could not tile. A search
// reads every row not yet re-filed in full beside the lists it probes, so a
// rollout never hides a document. See ADR-0028 for the decision
// and ivf.go, ivfrecord.go, apply_ivf.go and ivfduty.go for each half.
//
// # Pure functions over values, and why that is the whole point
//
// Everything in THIS FILE and its siblings on the arithmetic — [Quantize],
// [Hamming], [TwoStage], [Fuse], [TrainIVF], [ChooseProbes] — takes values
// and returns values. No
// database, no store, no query. The SQL that scans `kb_vectors_bin` lives in
// this package too (the estate boundary is why: the vectors are replicated
// and the lexical index is this node's own, so no read joins them), but in
// its own files, and the arithmetic is what says whether the ranking that SQL
// produces is the right one.
//
// The reason is the reason [textindex] gives for the same shape: a ranking
// that can only be exercised through a database is a ranking nobody
// re-measures, and an index rebuild can then quietly change it. Held as
// functions over slices, the quality gate runs in a unit test, at three
// corpus sizes, with a seeded corpus and no I/O at all.
//
// # What the gate measures, and what it does NOT
//
// [Quantize] keeps only each vector's ORTHANT — the sign of every coordinate
// and nothing else. How much an orthant says about cosine rank is a property
// of the corpus's own distribution and of nothing else: over a family of
// embedding-shaped generators at one corpus size, recall at the shipped
// oversample spans 0.29 to 0.98. So the deterministic gate here measures the
// ARITHMETIC — that an exact rerank over a 1-bit candidate pool recovers the
// exact ranking at sufficient depth — and never recall on a particular
// company's documents. `crewlet search eval` is what answers that, against
// that company's own vectors.
//
// # Which corpora are in the semantic half at all
//
// [Sources] is the whole of it: the knowledge base's pages and the native
// tracker's work items. It is a BUDGET rather than a list of what would be
// useful — every member is a vector per document in the replicated estate,
// a record per document on the log, and a share of an embedding duty that
// runs about a thousand sources a minute for every corpus a company has.
//
// The company's CHAT is deliberately outside it, with a lexical index of its
// own in this package ([ChatIndexer]) and no semantic half at all: a year at
// the declared census is several times the entire supported vector corpus,
// and the knowledge base's own embeddings are what would pay for it. ADR-0071
// is the record of that decision and of what admitting chat to one half and
// not the other cost the last time it happened. Gitlet's changes have a third
// keyword index of their own here ([ChangeIndexer], ADR-0156), unranked, since a listing
// of changes keeps its order.
//
// # The index of changes' words
//
// [ChangeIndexer] indexes Gitlet changes' titles and descriptions (ADR-0156,
// which holds its keys, batches and rebuilds). Its first build walks newest
// first by id over the rows at or below the code log's applied prefix where it
// began, and a forward walk over code_changes.position, never past that
// prefix, keeps every later move, so a record this node retains is never
// passed. Each document is keyed by its id's mint time, so newest is the same
// on every node, and its postings are one range of the postings key, so an
// edit or a purge removes them with no term list kept. [ChangeIndexer.Holding]
// reads the narrowest of a search's scope and words first and answers the
// newest minted changes holding every word, with how far each walk reached and
// the code log's position its writes were decided from.
//
// # The lexical index crosses the estate boundary
//
// [Indexer] keeps the BM25 inverted list over [textindex]'s analyzer. Its
// sources are replicated rows and its index is this node's own, so no query
// joins them: every comparison is a batch from each side, and the walk that
// replaces the join repairs a drifted row, where a version compare only ever
// noticed a source that moved. The same boundary is why fusion is over two
// ranked lists of ids rather than a UNION.
//
// # Shards and the fan-out
//
// Every indexed document carries a SHARD in both estates, a stable hash of its
// own identity into 64 buckets, so a re-filed source keeps its bucket. Nothing
// routes on it: every data node holds and reads every bucket, and the column
// only makes a scan's cost measurable per bucket range. Neither estate indexes
// it (migration 0004 measured 1 min 44 s against 50 ms for an index of that
// kind); a bucket range is seekable, and an index over it is how a planner
// stops driving on the term. A coordinator divides the buckets across the
// live data nodes and merges SCORE-WITHIN-METHOD FIRST, RRF ACROSS METHODS
// SECOND: RRF over one ranker across disjoint slices ranks by placement (a
// rank 1 on a weak slice, 0.10, would outrank a rank 2 on a strong one, 0.98).
// Fusing per slice is exact because the slices are disjoint, and BM25 stays
// comparable because every node holds the whole corpus: the scan is
// bucket-limited and the statistics are global. An assignment that does not
// answer is named (buckets answered, buckets missing, absent nodes), never
// silently truncated, because a short result is indistinguishable from a short
// corpus.
//
// # The index's probe count and its narrowed searches
//
// The probe count is the smallest meeting [FloorAt] with no head miss in every
// query shape a search is issued in (unfiltered, each source, one container),
// and an index that would read more than half its lists is not installed. A
// narrowed search reads lists until it has seen as many of its own rows as an
// unfiltered one reads ([ProbeCount]) and scans past half of them: the
// unfiltered count kept 0.949 of a pages-only answer and under 0.75 of one
// container's. The covering kb_vectors_bin_ivf_idx (0033) does not contradict
// 0004: a probe reads a share of the rows, the premise 0004 dropped every
// index for lacking.
//
// # The chat index is a second corpus
//
// [ChatIndexer] is keyword-only (ADR-0071). Its walk is forward only over
// chat_messages.version with a durable watermark, and its statistics are
// maintained and kept per room, because a term's document count off the
// postings is a range count the length of its list (some 160 ms a term at a
// year of chat) and because a search is ranked by the rooms it searches, never
// the company's. Rows that vanish are followed explicitly: the retention prune
// by a rotation over the rooms, the compliance erase by a forward walk over
// chat_deletions.version (markers are never pruned). A replaced estate is
// announced ([ChatIndexer.RequestRebuild]) and its index dropped a batch a
// sweep from a durable mark, since one transaction would hold the node's only
// writer. A search names the rooms its viewer may see, and one naming none is
// refused, not read as everything. A frequent term contributes its newest
// postings, the scan walking its list backwards by UUIDv7 id.
//
// # Plan gates
//
// The vector tables' and the lexical index's plan gates refuse a plan that
// answers a per-row aggregate from its whole child table
// (storetest.Decorrelated). The lexical gate plans the indexer's own
// statements and holds them complete with storetest.StatementLiterals over
// kb_docs and kb_postings, because its hand-written copies had drifted and
// could not fail. The embedding duty selects stale pages by walking
// pages_heads_live_updated_idx (0065) in update order.
//
// # A read of another domain's table is a named, exported statement
//
// This package reads tables it does not own (the tracker's tasks and
// relations, the knowledge base's page heads, Gitlet's changes) for the
// lexical index, the embedding duty, the backlinks, the window report and the
// index of changes' words. Each of those
// statements is a function returning the statement and its arguments
// ([TaskVersionsRead], [PageSelection], [LinkedTasksRead] and the rest), and
// it is the one the reader runs, so the gate of the package that owns the
// table — the one that owns its indexes — plans exactly what runs. Written
// inline, none of them could be planned by anything but a copy, and a copy
// certifies a statement nobody runs: the embedding duty's task selection had
// drifted from its copy by two columns and a predicate before it was named.
package search

import "math/bits"

// Quantize turns a vector into its sign code, LSB-first, with ZERO TAKING THE
// NEGATIVE BIT.
//
// The stage-1 scan orders by `vector_distance_cos(bits, :qb)` over the
// driver's `vector1bit`, so this must agree bit for bit or the pure gate
// certifies a ranking the database does not produce. Measured on `tursogo`:
// `vector1bit` of [0,1,-1,0,0.5,-0.5,2,-2] is 0x52, so bit i is at position
// i%8 of byte i/8, a set bit is STRICTLY POSITIVE, and zero takes the negative
// bit (padded embeddings carry whole dimensions of exact zeros). Words, because
// [Hamming] counts over words; padding bits are identical in every code.
func Quantize(v []float32) []uint64 {
	code := make([]uint64, (len(v)+63)/64)
	for i, x := range v {
		if x > 0 {
			code[i/64] |= 1 << uint(i%64)
		}
	}
	return code
}

// Hamming counts the bits two codes differ in.
//
// # This IS the driver's cosine distance, measured rather than assumed
//
// `vector_distance_cos` over two `vector1bit` values returns the raw Hamming
// distance (verified at the pin over 200 random 64-dimensional pairs, all
// agreeing). The whole two-stage design rests on it: otherwise the pure gate
// and the SQL would rank by different quantities.
//
// A length mismatch is a caller error, not a silent short read: codes of
// different widths are different embedding spaces, which `kb_vectors_bin`'s
// `model` and `dim` columns exist to make impossible.
func Hamming(a, b []uint64) int {
	if len(a) != len(b) {
		panic("search: Hamming over codes of different widths — two widths are " +
			"two embedding spaces, and the scan filters on model and dim so " +
			"they can never meet")
	}
	total := 0
	for i := range a {
		total += bits.OnesCount64(a[i] ^ b[i])
	}
	return total
}

// CodeBytes is how many bytes the DRIVER's encoding of an n-dimensional sign
// code occupies, which the table's size arithmetic is written against.
//
// The packed bits are followed by a trailer and the value is padded to an ODD
// length (two-byte alignment plus a type tag): overhead is three bytes over an
// even byte-count, two over an odd one, so a flat `+3` is wrong at 100
// dimensions. Measured across nineteen widths (TestTheEncodedWidthIsTwoByte
// Aligned): 3 B at 1 to 8, 5 B at 9 to 24, 15 B at 100, 195 B at 1 536, 387 B
// at 3 072. A function because the width is the company's model's.
func CodeBytes(dimensions int) int {
	packed := (dimensions + 7) / 8
	// The trailer, then padding to an odd total.
	return packed + 3 - packed%2
}
