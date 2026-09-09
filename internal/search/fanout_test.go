package search_test

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/search"
)

// FUSION RANKS BY SCORE, NEVER BY WHICH SLICE A DOCUMENT LANDED IN.
//
// The fixture is exact and it is the whole reason the merge is arranged the
// way it is. Reciprocal rank fusion combines DIFFERENT RANKERS over ONE
// corpus; run over ONE ranker across DISJOINT SLICES it ranks by placement —
// at k = 60 a rank-1 document contributes 1/61 = 0.016393 and a rank-2
// contributes 1/62 = 0.016129, so a 0.10 on a weak slice beats a 0.98 on a
// strong one.
func TestFusionRanksByScoreNotByAssignmentPlacement(t *testing.T) {
	t.Parallel()
	// A and B are the two strongest documents in the corpus and they are
	// in ONE slice. C is a weak document alone in another, where it ranks
	// first.
	strong := search.Slice{
		Node: "n1", Shards: search.Assignment{From: 0, To: 32},
		Lexical: []search.Scored{{Key: "page:A", Score: 0.99}, {Key: "page:B", Score: 0.98}},
	}
	weak := search.Slice{
		Node: "n2", Shards: search.Assignment{From: 32, To: 64},
		Lexical: []search.Scored{{Key: "page:C", Score: 0.10}},
	}

	fan := &search.FanOut{Self: "n1", Local: fixed{strong}, Peers: fixed{weak}}
	answer := run(t, fan, search.FanQuery{Limit: 10}, []string{"n1", "n2"})
	if want := []string{"page:A", "page:B", "page:C"}; !slices.Equal(answer.Hits, want) {
		t.Fatalf("the fan-out ranked %v, want %v — fusing per-slice RANKS puts "+
			"C (0.10, rank 1 on its slice) above B (0.98, rank 2 on its), which "+
			"is ranking by placement rather than by relevance",
			answer.Hits, want)
	}
	if answer.Partial() {
		t.Fatalf("every assignment answered and the result reports %d missing "+
			"buckets", answer.BucketsMissing)
	}
}

// A MISSING ASSIGNMENT IS NAMED, NEVER SILENTLY TRUNCATED.
//
// The coordinator holds the whole corpus, so it could always have answered
// alone — which is exactly why a short answer must say so. A search that
// returns one fewer result looks identical to a corpus with one fewer
// document.
func TestAMissingAssignmentIsNamedNotSilent(t *testing.T) {
	t.Parallel()
	mine := search.Slice{
		Node: "n1", Shards: search.Assignment{From: 0, To: 22},
		Lexical: []search.Scored{{Key: "page:A", Score: 0.9}},
	}
	answered := search.Slice{
		Node: "n2", Shards: search.Assignment{From: 22, To: 43},
		Lexical: []search.Scored{{Key: "page:B", Score: 0.8}},
	}
	// n3 is in the table and never answers.
	fan := &search.FanOut{Self: "n1", Local: fixed{mine}, Peers: fixed{answered}}
	answer := run(t, fan, search.FanQuery{Limit: 10}, []string{"n1", "n2", "n3"})

	if !answer.Partial() {
		t.Fatal("a node that never answered left the result reporting a complete " +
			"scan — the answers were complete for what was searched and silent " +
			"about what was not, which is the failure this field exists for")
	}
	if want := []string{"n3"}; !slices.Equal(answer.Absent, want) {
		t.Fatalf("the absent nodes are %v, want %v — an operator has nowhere to "+
			"look for a slice that names nobody", answer.Absent, want)
	}
	if answer.BucketsAnswered+answer.BucketsMissing != search.SearchShards {
		t.Fatalf("%d buckets answered and %d missing, which is not the %d the "+
			"corpus has — the two must partition it or neither number means "+
			"anything", answer.BucketsAnswered, answer.BucketsMissing,
			search.SearchShards)
	}
	if answer.BucketsMissing != 21 {
		t.Fatalf("n3 held 21 buckets and %d are reported missing",
			answer.BucketsMissing)
	}
	// AND WHAT DID ANSWER IS STILL RETURNED. A partial answer that
	// returned nothing would make a slow peer an outage.
	if want := []string{"page:A", "page:B"}; !slices.Equal(answer.Hits, want) {
		t.Fatalf("a partial answer returned %v, want %v", answer.Hits, want)
	}
}

// REFANNING DOES NOT CHANGE RELEVANCE ORDER.
//
// The same corpus divided one, two and four ways must fuse to one order. This
// is what makes a fan-out a LATENCY decision rather than a ranking one — and
// it is the arm that catches per-slice statistics: an IDF computed from a
// slice makes a term's weight a function of how the fleet was divided, so the
// lexical half reorders with N.
func TestRefanningDoesNotChangeRelevanceOrder(t *testing.T) {
	t.Parallel()
	corpus := map[string]float64{}
	for i := range 40 {
		corpus[fmt.Sprintf("page:d%02d", i)] = float64(100-i) / 100
	}
	// A DELIBERATE TIE, IN THE HEAD OF THE RANKING, because ties are where
	// a merge across concurrent answerers can disagree with itself: two
	// documents at one score have no arrival order worth preserving, so
	// the key decides. It has to be inside the query's own limit or the
	// tie is never compared and this arm proves nothing — which is what a
	// mutation removing the key tie-break found when the pair scored below
	// the twentieth hit.
	corpus["page:tie-b"] = 0.985
	corpus["page:tie-a"] = 0.985

	var reference []string
	for _, n := range []int{1, 2, 4} {
		nodes := make([]string, n)
		for i := range nodes {
			nodes[i] = fmt.Sprintf("n%d", i+1)
		}
		table := search.Divide(nodes)
		slicesByNode := map[string]search.Slice{}
		for _, a := range table {
			slicesByNode[a.Node] = search.Slice{Node: a.Node, Shards: a.Shards}
		}
		for key, score := range corpus {
			source, id, _ := strings.Cut(key, ":")
			shard := search.ShardOf(source, id)
			for _, a := range table {
				if !a.Shards.Contains(shard) {
					continue
				}
				s := slicesByNode[a.Node]
				s.Lexical = append(s.Lexical, search.Scored{Key: key, Score: score})
				slicesByNode[a.Node] = s
			}
		}
		// EACH SLICE IS TRIMMED TO FuseN, exactly as a participant
		// would trim it. Without this the test would prove that
		// merging untrimmed lists is exact, which is not the claim.
		var peers []search.Slice
		var mine search.Slice
		for _, a := range table {
			s := slicesByNode[a.Node]
			s.Lexical = search.MergeByScore([][]search.Scored{s.Lexical}, search.FuseN)
			if a.Node == "n1" {
				mine = s
				continue
			}
			peers = append(peers, s)
		}
		fan := &search.FanOut{Self: "n1", Local: fixed{mine}, Peers: fixedMany(peers)}
		answer := run(t, fan, search.FanQuery{Limit: 20}, nodes)
		if answer.Partial() {
			t.Fatalf("N = %d: %d buckets went unanswered", n, answer.BucketsMissing)
		}
		if reference == nil {
			reference = answer.Hits
			continue
		}
		if at := slices.Index(answer.Hits, "page:tie-a"); at < 0 ||
			at+1 >= len(answer.Hits) || answer.Hits[at+1] != "page:tie-b" {
			t.Fatalf("N = %d: the tied pair ranks %v — a tie the answer does "+
				"not order by key is a board that changes between reads of "+
				"the same data, and one outside the limit is a tie this test "+
				"never compares", n, answer.Hits)
		}
		if !slices.Equal(answer.Hits, reference) {
			t.Fatalf("N = %d ranked\n  %v\nand N = 1 ranks\n  %v\n\nA search that "+
				"answers differently depending on how the fleet is divided is a "+
				"fan-out that changed the ranking rather than the latency",
				n, answer.Hits, reference)
		}
	}
	if len(reference) != 20 {
		t.Fatalf("the reference answer holds %d hits, want the query's limit of 20",
			len(reference))
	}
}

// A SLICE'S TOP-FuseN IS SUFFICIENT FOR AN EXACT GLOBAL TOP-FuseN.
//
// The claim the merge rests on: the slices are disjoint, so a document outside
// its own slice's top-N has N better documents in that slice alone and cannot
// be in the global top-N. Asking each participant for FuseN is exact, not a
// sample.
func TestTheGlobalTopIsInsideTheUnionOfTheSliceTops(t *testing.T) {
	t.Parallel()
	// One slice holds the whole head of the ranking and the other holds a
	// long tail — the distribution that would break a scheme taking a
	// FIXED SHARE from each slice rather than a fixed DEPTH.
	head := make([]search.Scored, 0, 200)
	for i := range 200 {
		head = append(head, search.Scored{
			Key: fmt.Sprintf("page:h%03d", i), Score: 1 - float64(i)/1000,
		})
	}
	tail := make([]search.Scored, 0, 200)
	for i := range 200 {
		tail = append(tail, search.Scored{
			Key: fmt.Sprintf("page:t%03d", i), Score: 0.1 - float64(i)/10000,
		})
	}
	whole := search.MergeByScore([][]search.Scored{append(slices.Clone(head), tail...)},
		search.FuseN)
	split := search.MergeByScore([][]search.Scored{
		search.MergeByScore([][]search.Scored{head}, search.FuseN),
		search.MergeByScore([][]search.Scored{tail}, search.FuseN),
	}, search.FuseN)
	if !slices.Equal(keysOf(whole), keysOf(split)) {
		t.Fatalf("the merged top-%d is\n  %v\nand one scan's top-%d is\n  %v",
			search.FuseN, keysOf(split), search.FuseN, keysOf(whole))
	}
}

// THE DIVISION COVERS EVERY BUCKET EXACTLY ONCE.
//
// A bucket in nobody's range is a slice of the corpus no answer covers and
// nothing reports; a bucket in two ranges is a document counted twice, merged
// against itself, and ranked above where it belongs.
func TestTheDivisionCoversEveryBucketExactlyOnce(t *testing.T) {
	t.Parallel()
	for _, n := range []int{1, 2, 3, 5, 7, 8, 64, 100} {
		nodes := make([]string, n)
		for i := range nodes {
			nodes[i] = fmt.Sprintf("node-%03d", i)
		}
		covered := make([]int, search.SearchShards)
		widths := 0
		for _, a := range search.Divide(nodes) {
			widths += a.Shards.Width()
			for shard := range search.SearchShards {
				if a.Shards.Covers() && a.Shards.Contains(shard) {
					covered[shard]++
				}
			}
		}
		for shard, times := range covered {
			if times != 1 {
				t.Fatalf("N = %d: bucket %d is held by %d nodes", n, shard, times)
			}
		}
		if widths != search.SearchShards {
			t.Fatalf("N = %d: the assignment widths sum to %d, not %d",
				n, widths, search.SearchShards)
		}
	}
}

// THE DIVISION IS THE SAME ON EVERY NODE, from the same roster.
//
// Nothing publishes the table and nothing agrees on it: every node computes it
// from a sorted roster, so a coordinator that asked a peer to confirm the plan
// would need one more thing to be up.
func TestEveryNodeComputesTheSameDivision(t *testing.T) {
	t.Parallel()
	roster := []string{"beta", "alpha", "gamma"}
	want := search.Divide(roster)
	for _, shuffled := range [][]string{
		{"gamma", "beta", "alpha"}, {"alpha", "gamma", "beta"},
	} {
		if got := search.Divide(shuffled); !slices.Equal(got, want) {
			t.Fatalf("a roster in %v order divides as %v, and in %v order as %v "+
				"— two nodes would scan overlapping ranges and leave a gap",
				shuffled, got, roster, want)
		}
	}
}

// A SMALL CORPUS DOES NOT FAN OUT.
//
// The round trip costs more than the scan it splits below [search.FanOutFloor],
// so a company with two hundred pages must not pay a network hop per keystroke.
func TestASmallCorpusIsAnsweredWithoutTheFleet(t *testing.T) {
	t.Parallel()
	asked := false
	fan := &search.FanOut{
		Self:  "n1",
		Local: fixed{search.Slice{Node: "n1", Lexical: []search.Scored{{Key: "page:A", Score: 1}}}},
		Peers: peersFunc(func(context.Context, search.FanQuery, []search.Assigned) ([]search.Slice, error) {
			asked = true
			return nil, nil
		}),
		Roster: func(context.Context) ([]string, error) { return []string{"n1", "n2"}, nil },
		Corpus: func(context.Context) (int, error) { return search.FanOutFloor - 1, nil },
	}
	answer, err := fan.Search(t.Context(), search.FanQuery{Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	if asked {
		t.Fatalf("a corpus of %d documents fanned out — below the floor the "+
			"round trip costs more than the scan it divides",
			search.FanOutFloor-1)
	}
	if answer.BucketsAnswered != search.SearchShards || answer.Partial() {
		t.Fatalf("the solo scan reported %d answered and %d missing, want every "+
			"bucket answered", answer.BucketsAnswered, answer.BucketsMissing)
	}
}

// AN UNREADABLE ROSTER STILL ANSWERS.
//
// Fleet membership is a coordination read, and a company must be able to
// search what this node already holds while coordination is slow.
func TestAnUnreadableRosterFallsBackToTheLocalScan(t *testing.T) {
	t.Parallel()
	fan := &search.FanOut{
		Self:   "n1",
		Local:  fixed{search.Slice{Node: "n1", Lexical: []search.Scored{{Key: "page:A", Score: 1}}}},
		Peers:  peersFunc(func(context.Context, search.FanQuery, []search.Assigned) ([]search.Slice, error) { return nil, nil }),
		Roster: func(context.Context) ([]string, error) { return nil, errors.New("coordination is unreachable") },
		Corpus: func(context.Context) (int, error) { return search.FanOutFloor * 10, nil },
	}
	answer, err := fan.Search(t.Context(), search.FanQuery{Limit: 5})
	if err != nil {
		t.Fatalf("an unreadable roster failed the search: %v", err)
	}
	if !slices.Equal(answer.Hits, []string{"page:A"}) {
		t.Fatalf("the local answer is %v", answer.Hits)
	}
	if answer.Partial() {
		t.Fatal("a solo scan covers every bucket and must not report a partial " +
			"answer — an operator chasing a phantom missing slice is worse than " +
			"no report at all")
	}
}

// A PEER'S DEADLINE DOES NOT BECOME THE SEARCH'S.
func TestASlowPeerCostsItsBucketsAndNotTheAnswer(t *testing.T) {
	t.Parallel()
	fan := &search.FanOut{
		Self:  "n1",
		Local: fixed{search.Slice{Node: "n1", Lexical: []search.Scored{{Key: "page:A", Score: 1}}}},
		Peers: peersFunc(func(ctx context.Context, _ search.FanQuery, _ []search.Assigned) ([]search.Slice, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		}),
		Roster: func(context.Context) ([]string, error) { return []string{"n1", "n2"}, nil },
		Corpus: func(context.Context) (int, error) { return search.FanOutFloor * 10, nil },
		Budget: 200 * time.Millisecond,
	}
	started := time.Now()
	answer, err := fan.Search(t.Context(), search.FanQuery{Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("a peer that never answered held the search for %s", elapsed)
	}
	if !answer.Partial() || !slices.Equal(answer.Absent, []string{"n2"}) {
		t.Fatalf("a peer that timed out is reported as %v with %d buckets missing",
			answer.Absent, answer.BucketsMissing)
	}
	if !slices.Equal(answer.Hits, []string{"page:A"}) {
		t.Fatalf("the local half of a partial answer is %v", answer.Hits)
	}
}

// --- fixtures -------------------------------------------------------------

// fixed is a scanner and a peer set that answers with one canned slice.
type fixed struct{ slice search.Slice }

func (f fixed) Scan(context.Context, search.FanQuery, search.Assignment) (search.Slice, error) {
	return f.slice, nil
}

func (f fixed) Scatter(context.Context, search.FanQuery, []search.Assigned) ([]search.Slice, error) {
	return []search.Slice{f.slice}, nil
}

// fixedMany answers with a canned set of peer slices.
type fixedMany []search.Slice

func (f fixedMany) Scatter(context.Context, search.FanQuery, []search.Assigned) ([]search.Slice, error) {
	return f, nil
}

// peersFunc adapts a function to the peer seam.
type peersFunc func(context.Context, search.FanQuery, []search.Assigned) ([]search.Slice, error)

func (p peersFunc) Scatter(ctx context.Context, q search.FanQuery, table []search.Assigned) ([]search.Slice, error) {
	return p(ctx, q, table)
}

// run drives one search over an explicit roster above the fan-out floor.
func run(t *testing.T, fan *search.FanOut, q search.FanQuery, nodes []string) search.Answer {
	t.Helper()
	fan.Roster = func(context.Context) ([]string, error) { return nodes, nil }
	fan.Corpus = func(context.Context) (int, error) { return search.FanOutFloor * 10, nil }
	answer, err := fan.Search(t.Context(), q)
	if err != nil {
		t.Fatalf("fan-out search: %v", err)
	}
	return answer
}

func keysOf(s []search.Scored) []string {
	out := make([]string, 0, len(s))
	for _, one := range s {
		out = append(out, one.Key)
	}
	return out
}

// A FLEET LARGER THAN THE BUCKET COUNT STILL INCLUDES THE COORDINATOR.
//
// 64 buckets cannot be divided 100 ways, so the surplus nodes are not
// participants. Which ones are dropped is arbitrary; that the ASKING node is
// never among them is not — a coordinator with no row in its own table falls
// back to an unbounded assignment, scans every bucket, and merges its whole
// corpus against the peers' slices of the same corpus.
func TestACoordinatorKeepsItsSeatInAnOversizedFleet(t *testing.T) {
	t.Parallel()
	nodes := make([]string, 0, 100)
	for i := range 100 {
		nodes = append(nodes, fmt.Sprintf("node-%03d", i))
	}
	// The LAST node by sorted id is the one Divide would drop first.
	self := nodes[len(nodes)-1]

	var table []search.Assigned
	fan := &search.FanOut{
		Self:  self,
		Local: fixed{search.Slice{Node: self}},
		Peers: peersFunc(func(_ context.Context, _ search.FanQuery, t []search.Assigned) ([]search.Slice, error) {
			table = t
			return nil, nil
		}),
		Roster: func(context.Context) ([]string, error) { return nodes, nil },
		Corpus: func(context.Context) (int, error) { return search.FanOutFloor * 10, nil },
	}
	answer, err := fan.Search(t.Context(), search.FanQuery{Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(table) != search.SearchShards-1 {
		t.Fatalf("the coordinator handed out %d peer assignments over %d "+
			"buckets", len(table), search.SearchShards)
	}
	for _, a := range table {
		if a.Node == self {
			t.Fatal("the coordinator is in its own peer table, so it would " +
				"scan its assignment twice")
		}
		if a.Shards.Width() != 1 {
			t.Fatalf("%s holds %d buckets in a fleet of %d over %d",
				a.Node, a.Shards.Width(), search.SearchShards, search.SearchShards)
		}
	}
	// The coordinator answered one bucket, and the other 63 are missing
	// because this fixture's peers never reply.
	if answer.BucketsAnswered != 1 {
		t.Fatalf("the coordinator reports %d buckets answered, want its own 1 "+
			"— an unbounded assignment would claim all %d",
			answer.BucketsAnswered, search.SearchShards)
	}
	if answer.BucketsAnswered+answer.BucketsMissing != search.SearchShards {
		t.Fatalf("%d answered and %d missing is not %d",
			answer.BucketsAnswered, answer.BucketsMissing, search.SearchShards)
	}
}

// THE SEMANTIC SCORE PRESERVES EVERY DISTANCE THE SCAN COULD DISTINGUISH.
//
// The merge is higher-is-better for both methods, so a distance has to be
// turned into a score. The pretty conversion is `1 - d` — the cosine
// similarity itself — and it rounds away everything below the ulp of 1, which
// is precisely the range near-duplicate pages live in: two documents the scan
// ranked apart become a tie and are then ordered by their ids.
func TestTheSemanticScoreKeepsDistancesTheScanCouldTellApart(t *testing.T) {
	t.Parallel()
	// A pair the scan distinguishes and `1 - d` does not.
	near := []float64{1e-17, 2e-17}
	first, second := search.ScoreOfDistance(near[0]), search.ScoreOfDistance(near[1])
	if first == second {
		t.Fatalf("distances %g and %g both score %g — a conversion that "+
			"collapses them hands two ranked documents to the id tie-break",
			near[0], near[1], first)
	}
	if !(first > second) {
		t.Fatalf("the closer document scored %g and the further one %g — the "+
			"conversion must be order-REVERSING, since a smaller distance is "+
			"a better match and a bigger score is a better rank", first, second)
	}

	// AND OVER THE WHOLE COSINE RANGE, because an order that held only
	// near zero would reverse the tail of every answer.
	previous := math.Inf(1)
	for _, d := range []float64{0, 1e-17, 1e-9, 0.25, 0.5, 1, 1.5, 2} {
		score := search.ScoreOfDistance(d)
		if score >= previous {
			t.Fatalf("distance %g scores %g, which is not below the previous "+
				"%g", d, score, previous)
		}
		previous = score
	}
}

// THE LOCAL SCAN AND THE SCATTER RUN INSIDE ONE WINDOW.
//
// If the peers were asked after the local scan finished, a fan-out would cost
// the local scan PLUS the round trip and every peer's scan in turn — which is
// slower than not fanning out at all, while every ranking assertion in this
// file still passed. The whole point of dividing the buckets is that the
// participants run at the same time.
func TestTheLocalScanRunsWhileThePeersAreAnswering(t *testing.T) {
	t.Parallel()
	const each = 700 * time.Millisecond
	fan := &search.FanOut{
		Self: "n1",
		Local: slowScan{after: each, slice: search.Slice{
			Node: "n1", Lexical: []search.Scored{{Key: "page:A", Score: 1}},
		}},
		Peers: peersFunc(func(ctx context.Context, _ search.FanQuery, table []search.Assigned) ([]search.Slice, error) {
			select {
			case <-time.After(each):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			out := make([]search.Slice, 0, len(table))
			for _, a := range table {
				out = append(out, search.Slice{
					Node: a.Node, Shards: a.Shards,
					Lexical: []search.Scored{{Key: "page:" + a.Node, Score: 0.5}},
				})
			}
			return out, nil
		}),
		Roster: func(context.Context) ([]string, error) { return []string{"n1", "n2"}, nil },
		Corpus: func(context.Context) (int, error) { return search.FanOutFloor * 10, nil },
		Budget: 10 * time.Second,
	}

	started := time.Now()
	answer, err := fan.Search(t.Context(), search.FanQuery{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if answer.Partial() {
		t.Fatalf("both halves answered and %d buckets are missing",
			answer.BucketsMissing)
	}
	// GENEROUS, because this is wall clock on a shared machine — but the
	// two shapes are 700 ms and 1400 ms apart, so anything under 1100 ms
	// can only be the concurrent one.
	if elapsed := time.Since(started); elapsed > each+400*time.Millisecond {
		t.Fatalf("a local scan of %s beside a scatter of %s took %s — the two "+
			"ran one after the other, so a fan-out costs the sum of its "+
			"participants rather than the slowest of them", each, each, elapsed)
	}
}

// slowScan answers after a delay, so a serial coordinator is measurable.
type slowScan struct {
	after time.Duration
	slice search.Slice
}

func (s slowScan) Scan(ctx context.Context, _ search.FanQuery, _ search.Assignment) (search.Slice, error) {
	select {
	case <-time.After(s.after):
		return s.slice, nil
	case <-ctx.Done():
		return search.Slice{}, ctx.Err()
	}
}
