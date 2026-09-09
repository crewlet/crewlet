package search

import (
	"cmp"
	"context"
	"slices"
	"time"
)

// THE SEARCH FAN-OUT: how a company's corpus scan is divided across the nodes
// that already hold it, and what an answer says when part of the division did
// not come back.
//
// # What is being divided, and what is not
//
// Every node holds the WHOLE corpus — that is the fleet shape, and nothing
// here changes it. So a fan-out divides CPU, never data: the coordinator hands
// each participant a range of [SearchShards] buckets, every participant scans
// only its range, and the union is one scan of the corpus done N ways. A node
// that does not answer therefore costs the answer a slice of RELEVANCE rather
// than a slice of the company — and since the coordinator holds the whole
// corpus too, it could always have answered alone. That is precisely why an
// unanswered assignment must be NAMED: an answer that is quietly short is
// indistinguishable from a corpus that is quietly short.
//
// # Why the merge is score-then-RRF and never RRF-then-merge
//
// Reciprocal rank fusion combines DIFFERENT RANKERS over ONE corpus. Using it
// over ONE ranker across DISJOINT SLICES is a different operation with a
// different meaning, and it ranks by placement: at [FuseK] = 60 a document
// ranked 1 on a weak slice contributes 1/61 = 0.016393 and a far stronger
// document ranked 2 on a strong slice contributes 1/62 = 0.016129, so 0.10
// outranks 0.98.
//
// So each participant returns its top-[FuseN] PER METHOD WITH SCORES, the
// coordinator merges each method's candidates into one global list BY SCORE,
// and RRF runs ONCE over the two global lists. That is byte-identical to the
// single-scan path whenever every assignment answers — which is what makes a
// fan-out a latency decision rather than a ranking one.
//
// # Why the scores are comparable, and the reason differs per method
//
// Semantic similarity is comparable by construction: one model, one metric,
// one vector space. BM25 would NOT be comparable if each participant computed
// its statistics from its own slice — but it does not have to, because every
// node holds the whole corpus, so document frequency and the document count
// come from that node's complete tables whatever buckets it scanned. THE SCAN
// IS BUCKET-LIMITED; THE STATISTICS ARE GLOBAL. That sentence is the whole of
// why lexical fusion stays correct here and would not in a design that
// divided storage.

// Method names one of the two rankers a hybrid answer fuses.
type Method string

const (
	MethodLexical  Method = "lexical"
	MethodSemantic Method = "semantic"
)

// Scored is one document and the score the method that found it gave it.
//
// HIGHER IS BETTER, for both methods, so one merge serves both. The semantic
// scan produces a DISTANCE, and its participant reports the NEGATION rather
// than a similarity: negation is exact in IEEE 754, so the order the scan
// produced survives the merge bit for bit, where `1 - d` is the prettier
// number and collapses two distances that differ below the ulp of 1.
type Scored struct {
	Key   string
	Score float64
}

// Assigned is one participant and the bucket range it was given.
type Assigned struct {
	Node   string
	Shards Assignment
}

// Slice is one participant's answer: its top-[FuseN] per method, WITH SCORES.
//
// The scores are what make the merge exact. A participant returning ranks
// would force the coordinator to fuse ranks across slices, which is the
// failure this whole file is arranged against.
type Slice struct {
	// Node and Shards say WHOSE answer this is and for WHICH buckets.
	// Carried in the answer rather than tracked by the caller because a
	// scatter has no roster: the transport cannot say who replied, so the
	// payload must.
	Node   string
	Shards Assignment

	Lexical  []Scored
	Semantic []Scored

	// SemanticSkipped says this participant answered without its semantic
	// half — no embeddings provider, no vectors yet, or a scan that
	// failed. SEPARATE FROM A MISSING SLICE, because they degrade the
	// answer differently: a missing slice loses a range of the corpus,
	// while this loses the half that finds what shares no word with the
	// query, across the range it did scan.
	SemanticSkipped bool
}

// FanQuery is what every participant is asked.
type FanQuery struct {
	Text       string
	Containers []string
	Sources    []string

	// Vector is the query embedding, packed as [SemanticQuery.Vector].
	// Empty runs the lexical half alone, which is what a company with no
	// embeddings provider gets.
	Vector []byte
	Model  string
	Dim    int

	// Limit caps the fused answer. Zero takes [ReturnDepth].
	Limit int
}

// Scanner answers one bucket range out of the corpus this process holds.
//
// The interface is declared here, by the coordinator that calls it, and kept
// to the one method it needs: this node's own scan and a peer reached over the
// broker are the same question asked two ways.
type Scanner interface {
	Scan(ctx context.Context, q FanQuery, shards Assignment) (Slice, error)
}

// Peers scatters one query's assignment table across the fleet and returns
// the answers that arrived.
//
// ONE CALL FOR THE WHOLE TABLE rather than one per participant, because that
// is what the transport underneath is: a single scattered request that every
// node receives and only the named ones answer. Fewer answers than assignments
// is the ORDINARY outcome and never an error — a node that is slow, restarting
// or gone is exactly what the partial answer exists to report.
type Peers interface {
	Scatter(ctx context.Context, q FanQuery, table []Assigned) ([]Slice, error)
}

// FanOutFloor is the corpus size below which a search does not fan out.
//
// TEN THOUSAND DOCUMENTS, and the arithmetic is the anchor rather than the
// round number. [SemanticScanBudget] is one second at the measured coefficient
// of ≈ 740 000 embeddable sources, so a scan costs ≈ 1.35 µs per source; a
// broker round trip on a LAN is ≈ 1 ms. Splitting pays only when the local
// scan is worth several round trips, and 10 000 documents is ≈ 13.5 ms — an
// order of magnitude above the trip it replaces, and the point below which a
// fan-out spends more wall clock arranging the work than doing it.
//
// It is a FLOOR rather than a switch because the alternative is worse in both
// directions: a company that has written 200 pages would pay a network hop per
// keystroke, and one with a million would scan them all on one core because
// somebody never turned a knob on.
const FanOutFloor = 10_000

// FanOut is the coordinator.
//
// THE LOCAL SCAN IS NOT A PARTICIPANT LIKE THE OTHERS. This node always takes
// an assignment and runs it directly, never through the broker: a search that
// answered nothing because the broker hiccupped would be a fleet-wide outage
// of a read every node could serve alone.
type FanOut struct {
	// Self is this node's id, which is its name in the assignment table.
	Self string

	// Local is this node's own scan. Required.
	Local Scanner

	// Peers reaches the rest of the fleet. NIL IS A LEGAL DEPLOYMENT and
	// the common one — a single node, an embedded engine, every test — and
	// it means the local scan takes every bucket.
	Peers Peers

	// Roster answers which nodes are live, this one included. Nil, or an
	// error, degrades to the local scan alone rather than failing the
	// search: a coordination store that cannot be reached must not stop a
	// company searching what this node already holds.
	Roster func(ctx context.Context) ([]string, error)

	// Corpus reports how many documents this node holds, for the
	// [FanOutFloor] decision. Nil never fans out.
	Corpus func(ctx context.Context) (int, error)

	// Budget bounds the wait for the peers' assignments. Zero takes
	// [SemanticScanBudget], which is the ceiling one search already has.
	Budget time.Duration

	// Report is told what every answer covered and what it cost.
	//
	// A HOOK RATHER THAN A METRICS DEPENDENCY, so this package does not
	// import the recorder to say one sentence. It is what lets
	// `search_scoped`, `search_degraded` and `search_slow` fire at all:
	// each of those alarms is a fraction of the answers, and a fraction
	// nobody counts is an alarm that is permanently silent and looks
	// exactly like a system with nothing wrong.
	Report func(Answer, time.Duration)
}

// Answer is one fused result and what it is missing.
type Answer struct {
	// Hits are the fused keys, best first, capped at the query's limit.
	Hits []string

	// BucketsAnswered and BucketsMissing partition [SearchShards]. A
	// caller renders "answered over 42 of 64 buckets" from these, which is
	// the honest sentence: the answers were complete for what was searched
	// and silent about what was not.
	BucketsAnswered int
	BucketsMissing  int

	// Absent names the nodes whose assignments did not come back, so an
	// operator has somewhere to look. Sorted, for a stable log line.
	Absent []string

	// SemanticSkipped says at least one answering participant ran without
	// its semantic half.
	SemanticSkipped bool
}

// Partial reports whether part of the corpus went unscanned.
func (a Answer) Partial() bool { return a.BucketsMissing > 0 }

// Search runs one query across the fleet and fuses what comes back.
func (f *FanOut) Search(ctx context.Context, q FanQuery) (Answer, error) {
	started := time.Now()
	table, err := f.plan(ctx)
	if err != nil {
		return Answer{}, err
	}

	mine := Assignment{}
	peers := make([]Assigned, 0, len(table))
	for _, a := range table {
		if a.Node == f.Self {
			mine = a.Shards
			continue
		}
		peers = append(peers, a)
	}

	// THE PEERS ARE ASKED FIRST AND WAITED ON LAST, so the local scan runs
	// inside the same window rather than after it. Asked afterwards, a
	// fan-out would cost the local scan PLUS the round trip and be slower
	// than not fanning out at all.
	type scattered struct {
		slices []Slice
		err    error
	}
	var replies chan scattered
	if len(peers) > 0 && f.Peers != nil {
		replies = make(chan scattered, 1)
		budget := cmp.Or(f.Budget, SemanticScanBudget)
		go func() {
			deadline, done := context.WithTimeout(ctx, budget)
			defer done()
			out, err := f.Peers.Scatter(deadline, q, peers)
			replies <- scattered{out, err}
		}()
	}

	local, err := f.Local.Scan(ctx, q, mine)
	if err != nil {
		// THE LOCAL SCAN IS THE ONE FAILURE THAT IS AN ERROR. Every
		// other participant's silence is a partial answer; this one is
		// the store under the caller's own feet.
		return Answer{}, err
	}
	local.Node, local.Shards = f.Self, mine

	answers := []Slice{local}
	if replies != nil {
		got := <-replies
		if got.err == nil {
			answers = append(answers, got.slices...)
		}
	}
	answer := fuseSlices(answers, table, q.Limit)
	if f.Report != nil {
		f.Report(answer, time.Since(started))
	}
	return answer, nil
}

// plan decides who participates and which buckets each one takes.
func (f *FanOut) plan(ctx context.Context) ([]Assigned, error) {
	solo := []Assigned{{Node: f.Self, Shards: Everything()}}
	if f.Peers == nil || f.Roster == nil || f.Corpus == nil {
		return solo, nil
	}
	held, err := f.Corpus(ctx)
	if err != nil {
		return nil, err
	}
	if held < FanOutFloor {
		return solo, nil
	}
	nodes, err := f.Roster(ctx)
	if err != nil {
		// A ROSTER THAT CANNOT BE READ IS NOT A SEARCH THAT FAILS. The
		// fleet's membership is a coordination read, and a company must
		// still be able to search what this node holds when
		// coordination is slow.
		return solo, nil
	}
	nodes = slices.Compact(slices.Sorted(slices.Values(nodes)))
	if !slices.Contains(nodes, f.Self) {
		// THIS NODE IS ALWAYS A PARTICIPANT, whatever presence says.
		// A roster that has not yet seen this node's own lease would
		// otherwise hand every bucket to peers and make a local read
		// depend on the network.
		nodes = append(nodes, f.Self)
		slices.Sort(nodes)
	}
	if len(nodes) < 2 {
		return solo, nil
	}
	if len(nodes) > SearchShards {
		// THIS NODE KEEPS ITS SEAT when the fleet is larger than the
		// bucket count. [Divide] drops the surplus by sorted id, and a
		// coordinator that dropped ITSELF would scan every bucket
		// locally while the peers scanned them too — every document
		// counted twice and merged against itself. The table is this
		// coordinator's own plan and travels with the request, so two
		// coordinators picking different sixty-four are not in conflict.
		keep := make([]string, 0, SearchShards)
		keep = append(keep, f.Self)
		for _, node := range nodes {
			if len(keep) == SearchShards {
				break
			}
			if node != f.Self {
				keep = append(keep, node)
			}
		}
		nodes = keep
	}
	return Divide(nodes), nil
}

// Divide splits [SearchShards] into one contiguous range per node.
//
// SORTED BY NODE ID and remainder-first, so every node in the fleet computes
// the same table from the same roster with nothing to agree on. A coordinator
// that asked for a plan would need one more thing to be up.
func Divide(nodes []string) []Assigned {
	if len(nodes) == 0 {
		return nil
	}
	ordered := slices.Clone(nodes)
	slices.Sort(ordered)
	if len(ordered) > SearchShards {
		// A FLEET LARGER THAN THE BUCKET COUNT CANNOT DIVIDE FURTHER,
		// so the surplus nodes are not participants — and dropping them
		// is the only safe answer rather than a tidy one. An empty range
		// is [Assignment]'s zero value, which means EVERY bucket: a node
		// handed one would scan the whole corpus, its documents would be
		// merged against every other node's, and the coverage arithmetic
		// would report more buckets than the corpus has.
		//
		// The first [SearchShards] by sorted id take one bucket each,
		// which every node computes identically from the same roster.
		ordered = ordered[:SearchShards]
	}

	base, extra := SearchShards/len(ordered), SearchShards%len(ordered)
	out := make([]Assigned, 0, len(ordered))
	at := 0
	for i, node := range ordered {
		width := base
		if i < extra {
			// THE REMAINDER GOES TO THE FIRST FEW rather than to the
			// last one: 64 over 3 is 22, 21, 21 and never 21, 21, 22
			// plus a leftover. A range nobody holds is a range no
			// answer covers.
			width++
		}
		out = append(out, Assigned{Node: node, Shards: Assignment{From: at, To: at + width}})
		at += width
	}
	return out
}

// fuseSlices merges the answers by score within each method, then fuses the
// two global lists once.
//
// THE COVERAGE IS COUNTED FROM THE TABLE, never from the answers. A
// participant reports which range it scanned, but what a range is WORTH is the
// coordinator's own arithmetic: counting an answer's own claim would let one
// slow peer's stale table inflate the coverage of a search that missed a third
// of the corpus, and the partial report exists precisely to be untrusting
// here. For the same reason a slice from a node the table does not name is
// DROPPED WHOLE — its range overlaps somebody's, so its documents would be
// merged against themselves and ranked above where they belong.
func fuseSlices(answers []Slice, table []Assigned, limit int) Answer {
	assigned := make(map[string]Assignment, len(table))
	for _, a := range table {
		assigned[a.Node] = a.Shards
	}

	lexical := make([][]Scored, 0, len(answers))
	semantic := make([][]Scored, 0, len(answers))
	answered := make(map[string]bool, len(answers))
	out := Answer{}
	for _, a := range answers {
		if _, named := assigned[a.Node]; !named || answered[a.Node] {
			// Not in the table, or a second answer from one node —
			// either way a range somebody else also holds.
			continue
		}
		answered[a.Node] = true
		lexical = append(lexical, a.Lexical)
		semantic = append(semantic, a.Semantic)
		out.SemanticSkipped = out.SemanticSkipped || a.SemanticSkipped
	}
	for _, a := range table {
		if answered[a.Node] {
			out.BucketsAnswered += a.Shards.Width()
			continue
		}
		out.BucketsMissing += a.Shards.Width()
		out.Absent = append(out.Absent, a.Node)
	}
	slices.Sort(out.Absent)

	keys := func(s []Scored) []string {
		ids := make([]string, 0, len(s))
		for _, one := range s {
			ids = append(ids, one.Key)
		}
		return ids
	}
	fused := Fuse(
		keys(MergeByScore(lexical, FuseN)),
		keys(MergeByScore(semantic, FuseN)),
	)
	if limit <= 0 {
		limit = ReturnDepth
	}
	if len(fused) > limit {
		fused = fused[:limit]
	}
	out.Hits = fused
	return out
}

// MergeByScore merges per-slice candidate lists into one global list.
//
// # Why taking depth per slice is EXACT rather than approximate
//
// The slices are disjoint, so a document appears in exactly one of them. The
// global top-N is therefore contained in the union of the per-slice top-N: a
// document outside its own slice's top-N has at least N better documents in
// that slice alone, so it cannot be in the global top-N. Asking each
// participant for [FuseN] is sufficient for an identical global list, not a
// sample of one.
//
// TIES BREAK ON THE KEY, ascending, because a merge across concurrent
// answerers has no arrival order worth preserving — a fused order that is not
// total is a board that changes between reads of the same data.
func MergeByScore(lists [][]Scored, depth int) []Scored {
	merged := make([]Scored, 0, depth*max(len(lists), 1))
	for _, list := range lists {
		merged = append(merged, list...)
	}
	slices.SortStableFunc(merged, func(a, b Scored) int {
		if c := cmp.Compare(b.Score, a.Score); c != 0 {
			return c
		}
		return cmp.Compare(a.Key, b.Key)
	})
	if depth > 0 && len(merged) > depth {
		merged = merged[:depth]
	}
	return merged
}
