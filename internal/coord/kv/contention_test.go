package kv

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
)

// BenchmarkAnOrgCounterChargedThroughLeaves measures what one hot key costs a
// fleet of leaves: every node a leaf of its own on one member, each running
// sixty-four charging goroutines in a closed loop against the org's counter —
// the shape of a fleet whose every seat is mid-round — and reports, per
// batch, how many attempts it took to land and how many charges it carried,
// and per charge how long it waited. A batch that runs out of attempts fails
// the benchmark: it is a healthy round failed closed.
func BenchmarkAnOrgCounterChargedThroughLeaves(b *testing.B) {
	for _, nodes := range []int{1, 3, 8, 16, 32, 64, 128} {
		b.Run(fmt.Sprintf("nodes=%d", nodes), func(b *testing.B) {
			benchmarkOrgCounter(b, nodes)
		})
	}
}

func benchmarkOrgCounter(b *testing.B, nodes int) {
	const chargers = 64
	port := leafMember(b)
	prefix := fmt.Sprintf("f%d", bucketSeq.Add(1))
	stores := make([]*FleetStore, nodes)
	var (
		mu       sync.Mutex
		attempts []int
		sizes    []int
	)
	org := encodeKey(coord.OrgScope)
	for i := range stores {
		stores[i] = openFleetVia(b, leafClientOf(b, port, fmt.Sprintf("leaf-%d", i)), prefix)
		stores[i].tallies.observe = func(key string, batch, n int) {
			if key != org {
				return
			}
			mu.Lock()
			defer mu.Unlock()
			attempts = append(attempts, n)
			sizes = append(sizes, batch)
		}
	}
	w := coord.WindowsAt(time.Now(), time.UTC)
	var (
		next      atomic.Int64
		failed    atomic.Int64
		latencies = make([][]time.Duration, nodes*chargers)
		wg        sync.WaitGroup
	)
	b.ResetTimer()
	for i := range nodes * chargers {
		store := stores[i%nodes]
		wg.Go(func() {
			for next.Add(1) <= int64(b.N) {
				start := time.Now()
				if _, err := store.Charge(context.Background(), coord.ChargeRequest{
					Seat: coord.AgentScope(fmt.Sprintf("bench-%d", i)), Tokens: 1, Windows: w,
				}); err != nil {
					failed.Add(1)
					b.Error(err)
					return
				}
				latencies[i] = append(latencies[i], time.Since(start))
			}
		})
	}
	wg.Wait()
	b.StopTimer()
	var all []time.Duration
	for _, l := range latencies {
		all = append(all, l...)
	}
	slices.Sort(all)
	mu.Lock()
	defer mu.Unlock()
	slices.Sort(attempts)
	if len(all) == 0 || len(attempts) == 0 {
		return
	}
	pick := func(p float64) int { return attempts[min(len(attempts)-1, int(p*float64(len(attempts))))] }
	lat := func(p float64) time.Duration { return all[min(len(all)-1, int(p*float64(len(all))))] }
	total := 0
	for _, s := range sizes {
		total += s
	}
	b.ReportMetric(float64(pick(0.5)), "attempts-p50")
	b.ReportMetric(float64(pick(0.99)), "attempts-p99")
	b.ReportMetric(float64(attempts[len(attempts)-1]), "attempts-max")
	b.ReportMetric(float64(total)/float64(len(sizes)), "charges/batch")
	b.ReportMetric(float64(lat(0.5).Microseconds())/1000, "ms-p50")
	b.ReportMetric(float64(lat(0.99).Microseconds())/1000, "ms-p99")
	b.ReportMetric(float64(failed.Load()), "failed")
}
