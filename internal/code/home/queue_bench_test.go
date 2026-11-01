package home_test

import (
	"fmt"
	"math/bits"
	"math/rand/v2"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/code/home"
	"github.com/crewlet/crewlet/internal/config"
)

// BenchmarkQueueThroughput is what decides config.CodeDefaultQueueBatch
// (M5-D13): a `pre_land` queue simulated round by round — a round one
// required check's run, the unit every candidate and every bisect probe
// costs — for each failure rate p (1, 5, 10 %), depth D (1, 4, 8) and batch
// B (1, 2, 4, 8), under SATURATING arrivals: D changes a round, the most a
// queue of depth D lands with every candidate green and one change each, so
// its trains always run full. Each round the worker's own [home.BatchOf]
// cuts the train: up to D candidates of min(B, ⌈W/D⌉) waiting changes; the
// longest green prefix lands; a red first candidate is bisected, a probe a
// round, in ⌈log₂ n⌉ rounds while the queue lands nothing, and its culprit
// ejected, the rest waiting again at the front.
//
// It reports, per cell: `p99-rounds`, a landed change's p99 TIME TO LAND —
// rounds from the train that first tested it to the round that landed it,
// which is what a batch costs the changes beside a broken one; `p99-wait`,
// the p99 from its arrival, which grows with the run wherever the queue
// lands fewer than D a round — the backlog no batch rule can bound, only a
// higher rate of landing; and `checks/landed`, the check runs (candidates and
// probes) a landed change costs.
//
// DefaultQueueBatch is the B whose changes land soonest FROM ARRIVAL — the
// lowest `p99-wait` — at p = 5 % and the default depth, the larger of a tie
// (it costs fewer checks); TestTheDefaultQueueBatchLandsSoonestFromArrival
// reads the same simulation. The design's rule, the largest B whose p99
// from first test is at most 3 rounds, picks only B = 1: a red first
// candidate stalls every candidate behind it for the ⌈log₂ n⌉ rounds of its
// bisect, so no batch above one lands its train's changes inside 3 rounds at
// p99, and B = 1 is the one batch whose backlog grows without bound under
// saturation — the time a person waits for a change, from the moment it was
// queued, is what a batch is there to bound. Bounded: -benchtime 1x runs
// each cell's 2 000 rounds once, a fixed seed a run.
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
// batch, seeded by seed, and answers its figures.
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
		if red != 0 {
			upto := take
			if red > 0 {
				upto = red * n
			}
			for _, s := range waiting[:upto] {
				landed = append(landed, round-s.tested)
				wait = append(wait, round-s.arrived)
			}
			waiting = waiting[upto:]
			continue
		}
		// THE FIRST CANDIDATE RED: ⌈log₂ n⌉ probes, a round each, while
		// changes keep arriving, then its first bad change ejected.
		size := min(n, take)
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
// config.CodeDefaultQueueBatch to BenchmarkQueueThroughput's rule: of the
// batches the benchmark measures, the one whose changes land soonest from
// their arrival at p99, at a 5 % failure rate and the default depth under
// saturating arrivals, the larger of a tie — read off the same simulation
// through the worker's own [home.BatchOf], so a change to the worker's
// batching that moves the answer fails here before the constant is left
// wrong.
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
