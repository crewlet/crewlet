package search

import (
	"cmp"
	"context"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/knowledge"
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
	// MethodLexical is BM25 over this node's own inverted list — the
	// ranker that answers on the words a query actually used.
	MethodLexical Method = "lexical"

	// MethodSemantic is the two-stage vector scan — the ranker that
	// answers on meaning, and the one a company without an embeddings
	// provider never runs.
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

	// Building says this participant's LEXICAL INDEX has not completed a
	// lap over the sources this query named, so it did not cover its
	// range at all and the coordinator must count its buckets MISSING.
	//
	// It is not a degradation like the field above, and that is the whole
	// point of it being separate. Every node holds the whole corpus, but
	// the lexical index over it is each node's OWN — built by that node's
	// own walk, in its own database, on its own schedule — so a node that
	// joined a minute ago holds the corpus and can find nothing in it.
	// Handed a bucket range, such a node answers almost nothing and the
	// coordinator, having no way to tell that from a range with nothing
	// in it, reported COMPLETE coverage over a corpus it had silently
	// dropped a slice of. That is the one thing this file's whole coverage
	// arithmetic exists to prevent — "an answer that is quietly short is
	// indistinguishable from a corpus that is quietly short" — and the
	// hole was that the sentence was only ever true for a participant that
	// did not reply.
	//
	// The zero value is "ready", which is what a peer on a build that
	// predates this field sends, and it is the old behaviour: evolution
	// here is additive and an unknown field is ignored, so a rolling
	// upgrade degrades a ranking rather than a search.
	Building bool
}

// FanQuery is what every participant is asked.
type FanQuery struct {
	Text       string
	Containers []string
	Sources    []string

	// Vector is the query embedding, packed as [SemanticQuery.Vector].
	// Empty runs the lexical half alone, which is what a company with no
	// embeddings provider gets.
	//
	// A COORDINATOR NEVER NEEDS TO SET IT: [FanOut.Search] computes it
	// from [FanOut.Vectors] when the mode ranks by meaning. It is here
	// because it is what travels to a participant, and so a caller that
	// already holds a vector (the eval harness) can pass one.
	Vector []byte
	Model  string
	Dim    int

	// Mode is the ranking asked for. The zero value is hybrid. The
	// coordinator resolves it into [FanQuery.Methods] and a vector before
	// anything is scattered, so a participant never reads it.
	Mode knowledge.Mode

	// Methods are the rankers a participant runs. EMPTY IS BOTH, which is
	// what a coordinator on a build that predates modes sends and what
	// the zero value has always meant — so the field is additive on the
	// wire in both directions: an older participant that ignores it runs
	// both, and the coordinator fuses only the ones it asked for.
	Methods []Method

	// Limit caps the fused answer. Zero takes [ReturnDepth].
	Limit int
}

// runs reports whether this query asks a participant to run method m.
func (q FanQuery) runs(m Method) bool {
	return len(q.Methods) == 0 || slices.Contains(q.Methods, m)
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

	// Vectors computes the query's embedding when the mode ranks by
	// meaning. NIL IS A NODE WITH NO SEMANTIC SEARCH — a company with no
	// embeddings provider, an embedded engine, most tests — and every
	// answer then says so rather than ranking by the words alone and
	// calling it hybrid.
	Vectors *QueryVectors

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

	// Enter is called when a search BEGINS, and the function it returns
	// when that search ends.
	//
	// A SECOND SEAM RATHER THAN A FIELD ON THE REPORT, because the two
	// answer different questions and only one of them can be answered at
	// the end. [FanOut.Report] says what an answer covered and what it
	// cost, which is knowable once. How many scans are IN FLIGHT is a
	// LEVEL, and a level has to be sampled while they are running — it is
	// the row of the supported-corpus table this node is actually on,
	// where every published scan figure was measured with one reader on an
	// idle node. Nil counts nothing.
	Enter func() func()
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

	// Absent names the nodes that did not COVER their assignment, so an
	// operator has somewhere to look. Sorted, for a stable log line.
	//
	// Two ways in, and they read the same to a caller because the corpus
	// lost the same slice either way: a participant that never replied,
	// and one that replied saying its lexical index is still building
	// (see [Slice.Building]).
	Absent []string

	// SemanticSkipped says at least one answering participant ran without
	// its semantic half.
	SemanticSkipped bool

	// Nodes is every participant in the assignment table, sorted by id,
	// and whether each covered its range — the per-node half of
	// [Answer.Coverage]. Absent is the same fact, narrowed to the ones
	// that did not.
	Nodes []knowledge.NodeCoverage

	// Served, Modes and Degraded are what the search actually ranked by,
	// what this node could have ranked by, and why the two differ from
	// what was asked — see [knowledge.Outcome].
	Served   knowledge.Mode
	Modes    []knowledge.Mode
	Degraded knowledge.Degradation
}

// Partial reports whether part of the corpus went unscanned.
func (a Answer) Partial() bool { return a.BucketsMissing > 0 }

// Coverage is this answer's division as every fleet answer reports one.
func (a Answer) Coverage() knowledge.Coverage {
	nodes := a.Nodes
	if nodes == nil {
		nodes = []knowledge.NodeCoverage{}
	}
	return knowledge.Coverage{
		Nodes:          nodes,
		Complete:       !a.Partial(),
		BucketsMissing: a.BucketsMissing,
	}
}

// Outcome is what this answer did, as a backend reports it.
func (a Answer) Outcome() knowledge.Outcome {
	return knowledge.Outcome{
		ServedMode: a.Served,
		Modes:      slices.Clone(a.Modes),
		Coverage:   a.Coverage(),
		Degraded:   a.Degraded,
	}
}

// Modes is what a node with this fan-out can serve as asked, with no I/O.
//
// ALL THREE WHEN A QUERY VECTOR COULD BE COMPUTED, and only keyword when not:
// hybrid on a node with no semantic ranking is served as its keyword half and
// said so, which is a degraded answer and therefore not a mode on offer.
func (f *FanOut) Modes() []knowledge.Mode {
	if f != nil && f.Vectors.Available() {
		return slices.Clone(knowledge.Modes)
	}
	return []knowledge.Mode{knowledge.ModeKeyword}
}

// resolve turns the asked mode into the rankers to run and the vector they
// need, and says what will be served.
//
// THE ONE PLACE A MODE BECOMES A PLAN, so the knowledge search and the item
// search degrade identically: hybrid without a vector serves its keyword half;
// semantic without one runs nothing at all, because the keyword ranking is
// the one ranking guaranteed not to find what somebody asking for meaning is
// looking for.
func (f *FanOut) resolve(ctx context.Context, q FanQuery) (FanQuery, knowledge.Mode, knowledge.Degradation) {
	mode := q.Mode.Resolved()
	var degraded knowledge.Degradation
	if mode.Semantic() && len(q.Vector) == 0 {
		var vector QueryVector
		vector, degraded = f.Vectors.Vector(ctx, q.Text)
		q.Vector, q.Model, q.Dim = vector.Vector, vector.Model, vector.Dim
	}
	semantic := mode.Semantic() && len(q.Vector) > 0 && q.Model != "" && q.Dim > 0
	if !semantic {
		// NO VECTOR TRAVELS WITH A QUERY THAT WILL NOT RANK BY MEANING,
		// so a keyword search costs a participant no vector scan.
		q.Vector, q.Model, q.Dim = nil, "", 0
	}
	served := mode
	switch {
	case mode == knowledge.ModeHybrid && !semantic:
		served = knowledge.ModeKeyword
	case mode == knowledge.ModeSemantic && !semantic:
		served = ""
	}
	q.Methods = nil
	if served.Lexical() && served != "" {
		q.Methods = append(q.Methods, MethodLexical)
	}
	if semantic {
		q.Methods = append(q.Methods, MethodSemantic)
	}
	return q, served, degraded
}

// Search runs one query across the fleet and fuses what comes back.
func (f *FanOut) Search(ctx context.Context, q FanQuery) (Answer, error) {
	started := time.Now()
	if f.Enter != nil {
		// BEFORE THE PLAN, because the plan is a coordination read and
		// a search waiting on one is a search this node is running.
		defer f.Enter()()
	}
	q, served, degraded := f.resolve(ctx, q)
	if served == "" {
		// NOTHING TO RUN: a semantic search with no vector. Not an
		// error and not a scan over nothing — an answer that says what
		// it could not do, and covered no node because it asked none.
		//
		// STILL REPORTED, because it is an answer somebody was given:
		// a semantic search whose query vector the provider failed is
		// the purest case of `search_degraded` there is, and an early
		// return past the hook left it out of the fraction entirely —
		// counting only the hybrid searches that failed the same way.
		answer := Answer{Modes: f.Modes(), Degraded: degraded,
			Nodes: []knowledge.NodeCoverage{}}
		if f.Report != nil {
			f.Report(answer, time.Since(started))
		}
		return answer, nil
	}
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
			// THIS ERR MUST BE THE GOROUTINE'S OWN. Assigning the
			// outer one would race the local scan below, which
			// writes it while this is in flight; the reply travels
			// down the channel instead, which is the handoff.
			//nolint:govet // shadow: deliberate; see the paragraph above.
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
	var scatterErr error
	if replies != nil {
		got := <-replies
		if got.err == nil {
			answers = append(answers, got.slices...)
		} else {
			scatterErr = got.err
		}
	}
	answer := fuseSlices(answers, table, q, scatterErr)
	answer.Served, answer.Modes, answer.Degraded = served, f.Modes(), degraded
	if answer.Degraded == knowledge.NotDegraded && answer.SemanticSkipped &&
		served.Semantic() {
		answer.Degraded = knowledge.DegradedSemanticPartial
	}
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
		//nolint:nilerr // Solo IS the answer here: this node holds the whole
		// corpus, so an unreadable roster costs parallelism and nothing else.
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
//
// ONLY THE RANKERS THE QUERY ASKED FOR ARE FUSED, whatever a participant sent
// back: a peer on a build that predates [FanQuery.Methods] runs both, and
// fusing the half nobody asked for would turn a keyword search into a hybrid
// one on whichever buckets that peer happened to hold.
//
// scatterErr is why the peers could not be asked at all, which becomes every
// unanswered peer's own reason rather than a generic silence.
func fuseSlices(answers []Slice, table []Assigned, q FanQuery, scatterErr error) Answer {
	limit := q.Limit
	assigned := make(map[string]Assignment, len(table))
	for _, a := range table {
		assigned[a.Node] = a.Shards
	}
	building := make(map[string]bool)

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
		if a.Building {
			// A REPLY THAT SAYS IT COVERED NOTHING IS NOT AN ANSWER.
			// Counting it would put this participant's buckets in
			// BucketsAnswered on the strength of a reply that states
			// the opposite, which is the silent-short-answer the whole
			// partition exists to make visible. Its hits are dropped
			// with it rather than merged beside a missing range: a
			// range cannot be both scanned and unscanned, and half of
			// one merged under "complete" is how the coverage number
			// stops meaning anything.
			building[a.Node] = true
			continue
		}
		answered[a.Node] = true
		if q.runs(MethodLexical) {
			lexical = append(lexical, a.Lexical)
		}
		if q.runs(MethodSemantic) {
			semantic = append(semantic, a.Semantic)
			out.SemanticSkipped = out.SemanticSkipped || a.SemanticSkipped
		}
	}
	out.Nodes = make([]knowledge.NodeCoverage, 0, len(table))
	for _, a := range table {
		if answered[a.Node] {
			out.BucketsAnswered += a.Shards.Width()
			out.Nodes = append(out.Nodes, knowledge.NodeCoverage{ID: a.Node, Answered: true})
			continue
		}
		out.BucketsMissing += a.Shards.Width()
		out.Absent = append(out.Absent, a.Node)
		out.Nodes = append(out.Nodes, knowledge.NodeCoverage{
			ID: a.Node, Error: absentReason(building[a.Node], scatterErr),
		})
	}
	slices.Sort(out.Absent)
	slices.SortFunc(out.Nodes, func(a, b knowledge.NodeCoverage) int {
		return cmp.Compare(a.ID, b.ID)
	})

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

// absentReason is why one participant's range went unscanned, in words an
// operator can act on.
func absentReason(building bool, scatterErr error) string {
	switch {
	case building:
		return "its search index is still building its first pass, so it " +
			"covered none of its range"
	case scatterErr != nil:
		return "the fleet could not be asked: " + scatterErr.Error()
	}
	return "no answer arrived inside the search budget"
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
