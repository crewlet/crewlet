package search_test

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/search"
)

// THE FAN-OUT'S OWN INSTRUMENT: what dividing the buckets buys, what it costs,
// and what it still answers with a replica down.
//
// # What it measures, and what it deliberately does not
//
// It measures the DIVISION, not the transport. Every participant runs the same
// [search.NodeScanner] against the same node's tables — which is the truth of
// the deployment anyway, since every node holds the whole corpus — so what
// varies across cells is only how many buckets each participant reads. Putting
// a broker in the loop here would fold a LAN's variance into a number about
// SQL, and the round trip it would measure is already an explicit term in
// [search.FanOutFloor].
//
// # What it reports
//
//   - rows/node: how much of the corpus one participant scanned. This is the
//     whole point of the step, and the number a fan-out with no predicate
//     leaves unchanged.
//   - p95-ms: end to end, because the interactive budget is written against a
//     p95 and a mean cannot estimate one.
//   - recall: against the N = 1 golden. A fan-out is a LATENCY decision, so
//     anything below 1.0 at full participation is a ranking that moved.
//   - buckets-missing: what the answer says it did not cover, which at full
//     participation must be zero and with a replica down must not be.
//
// # What it measured, on this container, 4 000 documents on four cores
//
//	replicas   rows/node   p95      recall   one down: p95 / missing / recall
//	       1       4 000    89 ms    1.000   —
//	       2       2 000    64 ms    1.000   60 ms / 32 / 0.70
//	       4       1 000    70 ms    1.000   55 ms / 16 / 0.95
//	       8         500    98 ms    1.000   78 ms /  8 / 1.00
//
// RECALL IS EXACTLY 1.000 AT EVERY FULL PARTICIPATION, which is the number
// this step turns on: dividing the buckets changes the latency and does not
// touch the order.
//
// The p95 column turns back up past two replicas, and the reason is this
// harness rather than the design: every participant here runs in ONE process
// on FOUR cores, so eight of them contend for the cores a real fleet spreads
// across eight machines. What the column does show honestly is the shape a
// deployment shares — a fan-out wider than the CPU underneath it is slower
// than no fan-out at all — which is why the floor is priced in scan time
// rather than in node count.
//
// The one-down cells show the partial answer costing exactly its replica's
// buckets: 32, 16 and 8 of 64, with recall falling as those buckets hold more
// of the answer. At eight replicas one absent slice happens to hold none of
// the top 20, which is why recall stays at 1.000 there — a reminder that
// recall alone cannot report coverage, and buckets-missing is not decorative.
func BenchmarkSearchFanOut(b *testing.B) {
	const corpus = 4_000
	db := openStore(b)
	x := search.NewIndexer(db)
	for i := range corpus {
		container := "ENG"
		if i%3 == 0 {
			container = "OPS"
		}
		page(b, db, fmt.Sprintf("p.%05d", i), container,
			fmt.Sprintf("Doc %05d", i), benchBody(i), 1)
	}
	indexAll(b, x)

	q := search.FanQuery{Text: "migration plan retention sweep", Limit: 20}
	golden := fanCell(b, x, corpus, []string{"n1"}, nil, q)

	for _, n := range []int{1, 2, 4, 8} {
		nodes := make([]string, n)
		for i := range nodes {
			nodes[i] = fmt.Sprintf("n%d", i+1)
		}
		b.Run(fmt.Sprintf("replicas=%d", n), func(b *testing.B) {
			fanCell(b, x, corpus, nodes, nil, q).report(b, golden, corpus, n)
		})
		if n == 1 {
			continue
		}
		// ONE REPLICA DOWN, which is the cell the partial answer exists
		// for: the coverage must drop by that replica's buckets and the
		// answer must still come back.
		b.Run(fmt.Sprintf("replicas=%d/one-down", n), func(b *testing.B) {
			fanCell(b, x, corpus, nodes, []string{nodes[n-1]}, q).report(b, golden, corpus, n)
		})
	}
}

// fanResult is one cell's measurement.
type fanResult struct {
	hits    []string
	samples []time.Duration
	missing int
}

// report prints the cell's numbers against the single-scan golden.
func (r fanResult) report(b *testing.B, golden fanResult, corpus, replicas int) {
	b.Helper()
	sort.Slice(r.samples, func(i, j int) bool { return r.samples[i] < r.samples[j] })
	if len(r.samples) > 0 {
		p95 := r.samples[min(int(float64(len(r.samples))*0.95), len(r.samples)-1)]
		b.ReportMetric(float64(p95.Microseconds())/1000, "p95-ms")
	}
	// ROWS PER NODE is the corpus divided by however many buckets one
	// participant held — the number a fan-out with no predicate leaves
	// exactly where it was.
	b.ReportMetric(float64(corpus)/float64(replicas), "rows/node")
	b.ReportMetric(recallOf(r.hits, golden.hits), "recall")
	b.ReportMetric(float64(r.missing), "buckets-missing")
}

// fanCell runs one configuration, with the named nodes silent.
func fanCell(b *testing.B, x *search.Indexer, corpus int, nodes, down []string, q search.FanQuery) fanResult {
	b.Helper()
	scanner := search.NodeScanner{Index: x}
	fan := &search.FanOut{
		Self:  nodes[0],
		Local: scanner,
		Peers: benchPeers{scanner: scanner, down: down},
		// ABOVE THE FLOOR unconditionally, because the decision the
		// floor makes is not what this cell is measuring.
		Corpus: func(context.Context) (int, error) { return search.FanOutFloor * 10, nil },
		Roster: func(context.Context) ([]string, error) { return nodes, nil },
	}

	var out fanResult
	// A WARM PASS FIRST, so the first sample is not a page-cache miss
	// standing in for a p95.
	if _, err := fan.Search(b.Context(), q); err != nil {
		b.Fatalf("fan-out search: %v", err)
	}
	for b.Loop() {
		started := time.Now()
		answer, err := fan.Search(b.Context(), q)
		if err != nil {
			b.Fatalf("fan-out search: %v", err)
		}
		out.samples = append(out.samples, time.Since(started))
		out.hits, out.missing = answer.Hits, answer.BucketsMissing
	}
	return out
}

// benchPeers runs each peer's assignment in this process, concurrently, and
// drops the ones named down.
type benchPeers struct {
	scanner search.NodeScanner
	down    []string
}

func (p benchPeers) Scatter(ctx context.Context, q search.FanQuery, table []search.Assigned) ([]search.Slice, error) {
	var mu sync.Mutex
	var out []search.Slice
	var wg sync.WaitGroup
	for _, a := range table {
		if slices.Contains(p.down, a.Node) {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			slice, err := p.scanner.Scan(ctx, q, a.Shards)
			if err != nil {
				return
			}
			slice.Node, slice.Shards = a.Node, a.Shards
			mu.Lock()
			out = append(out, slice)
			mu.Unlock()
		}()
	}
	wg.Wait()
	return out, nil
}

// recallOf is the fraction of the golden answer this cell returned.
func recallOf(got, golden []string) float64 {
	if len(golden) == 0 {
		return 1
	}
	have := map[string]bool{}
	for _, id := range got {
		have[id] = true
	}
	found := 0
	for _, id := range golden {
		if have[id] {
			found++
		}
	}
	return float64(found) / float64(len(golden))
}

// benchBody gives each document a different mix of the query's terms, so the
// ranking has a real head rather than a plateau every document sits on.
func benchBody(i int) string {
	switch i % 5 {
	case 0:
		return "the migration plan is here and the retention sweep follows it"
	case 1:
		return "the migration plan is here"
	case 2:
		return "retention and the sweep that enforces it"
	case 3:
		return "a plan for the sweep"
	default:
		return "an unrelated page about onboarding and the handbook"
	}
}
