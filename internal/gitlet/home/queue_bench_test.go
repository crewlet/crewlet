package home_test

import (
	"fmt"
	"math/bits"
	"math/rand/v2"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/gitlet/home"
)

// BenchmarkQueueThroughput decides config.CodeDefaultQueueBatch (M5-D13): a
// queue simulated round by round (a round is one required check's run, the
// cost of each candidate and bisect probe) for each failure rate p, depth D
// and batch B under SATURATING arrivals, D changes a round, so trains always
// run full. [home.BatchOf] cuts each train, the longest green prefix lands,
// and the first red candidate is bisected a probe a round, landing nothing,
// its culprit then ejected. Per cell: `p99-rounds`, from first test to
// landing (what a batch costs the changes beside a broken one); `p99-wait`,
// from arrival, growing wherever the queue lands fewer than D a round; and
// `checks/landed`, check runs per landed change. The default's rule is
// TestTheDefaultQueueBatchLandsSoonestFromArrival's. -benchtime 1x runs each
// cell's 2 000 rounds once, a fixed seed a run.
func BenchmarkQueueThroughput(b *testing.B) {
	for _, pct := range []int{1, 5, 10} {
		for _, depth := range []int{1, 4, 8} {
			for _, batch := range []int{1, 2, 4, 8} {
				b.Run(fmt.Sprintf("p=%d%%/D=%d/B=%d", pct, depth, batch), func(b *testing.B) {
					var r queueRun
					for i := range b.N {
						r = simulateQueue(uint64(i), float64(pct)/100, depth, batch, 2000)
					}
					b.ReportMetric(r.p99, "p99-rounds")
					b.ReportMetric(r.p99Wait, "p99-wait")
					b.ReportMetric(r.checksPerLanded, "checks/landed")
				})
			}
		}
	}
}

// queueRun is one simulation's figures.
type queueRun struct {
	p99, p99Wait    float64
	checksPerLanded float64
}

// simChange is a waiting change: the round it arrived, the round a train
// first tested it (-1 before any) and whether it breaks the check.
type simChange struct {
	arrived, tested int
	bad             bool
}

// simulateQueue runs rounds rounds of a queue at failure rate p, depth and
// batch, seeded by seed, and answers its figures. It charges nothing for the
// worker cancelling a given-up train's checks (queue.go's cancelAbandoned): a
// run lasts one round and a train is given up only between rounds, so a
// cancellation ends no run the model charges. What cancelling saves is
// container time under contention (`gitlet.max_checks` a node), which a model
// of unbounded containers cannot price, so it moves no figure or default.
func simulateQueue(seed uint64, p float64, depth, batch, rounds int) queueRun {
	rng := rand.New(rand.NewPCG(seed, 0x51ab))
	var (
		waiting      []simChange
		landed, wait []int
		checks       int
	)
	arrive := func(round int) {
		for range depth {
			waiting = append(waiting, simChange{arrived: round, tested: -1, bad: rng.Float64() < p})
		}
	}
	for round := 0; round < rounds; {
		arrive(round)
		n := home.BatchOf(len(waiting), depth, batch)
		take := min(len(waiting), n*depth)
		for i := range take {
			if waiting[i].tested < 0 {
				waiting[i].tested = round
			}
		}
		candidates := (take + n - 1) / n
		checks += candidates
		red := -1
		for c := range candidates {
			if slices.ContainsFunc(waiting[c*n:min((c+1)*n, take)], func(s simChange) bool { return s.bad }) {
				red = c
				break
			}
		}
		round++
		upto := take
		if red >= 0 {
			upto = red * n
		}
		for _, s := range waiting[:upto] {
			landed = append(landed, round-s.tested)
			wait = append(wait, round-s.arrived)
		}
		waiting = waiting[upto:]
		if red < 0 {
			continue
		}
		// THE RED CANDIDATE, first once its green prefix landed (the worker's
		// later candidates stand, each built onto the one before), is
		// bisected next, never tested again: ⌈log₂ n⌉ probes, a round each
		// while changes keep arriving, then its first bad change ejected and
		// the rest waiting at the front.
		size := min(n, take-upto)
		for range bits.Len(uint(size - 1)) {
			checks++
			arrive(round)
			round++
		}
		culprit := slices.IndexFunc(waiting[:size], func(s simChange) bool { return s.bad })
		waiting = slices.Delete(waiting, culprit, culprit+1)
	}
	if len(landed) == 0 {
		return queueRun{}
	}
	slices.Sort(landed)
	slices.Sort(wait)
	return queueRun{p99: float64(landed[(len(landed)*99)/100]), p99Wait: float64(wait[(len(wait)*99)/100]),
		checksPerLanded: float64(checks) / float64(len(landed))}
}

// TestTheDefaultQueueBatchLandsSoonestFromArrival holds
// config.CodeDefaultQueueBatch to the batch whose changes land soonest FROM
// ARRIVAL at p99, at a 5 % failure rate and the default depth, the larger of
// a tie (fewer checks), off BenchmarkQueueThroughput's simulation through the
// worker's own [home.BatchOf], so a batching change that moves the answer
// fails here. Not the design's rule (the largest B whose p99 from first test
// is at most 3 rounds): it would keep B = 1 wherever a batch of two missed
// it, and B = 1 is the one batch whose backlog grows without bound under
// saturation (148 rounds from arrival at p99, against 5 at B = 2). A person
// waits from the moment a change was queued, and that is what a batch is
// there to bound.
func TestTheDefaultQueueBatchLandsSoonestFromArrival(t *testing.T) {
	t.Parallel()
	best, bestWait := 0, 0.0
	for _, batch := range []int{1, 2, 4, 8} {
		r := simulateQueue(0, 0.05, config.CodeDefaultQueueDepth, batch, 2000)
		t.Logf("D=%d B=%d: p99 %.0f rounds to land, %.0f from arrival, %.2f checks a landed change",
			config.CodeDefaultQueueDepth, batch, r.p99, r.p99Wait, r.checksPerLanded)
		if r.p99Wait == 0 {
			t.Fatalf("D=%d B=%d landed nothing in 2 000 rounds", config.CodeDefaultQueueDepth, batch)
		}
		if best == 0 || r.p99Wait <= bestWait {
			best, bestWait = batch, r.p99Wait
		}
	}
	if best != config.CodeDefaultQueueBatch {
		t.Fatalf("the batch landing soonest from arrival at p99, at 5 %% and depth %d, is %d (%.0f rounds), "+
			"and CodeDefaultQueueBatch is %d", config.CodeDefaultQueueDepth, best, bestWait,
			config.CodeDefaultQueueBatch)
	}
}
