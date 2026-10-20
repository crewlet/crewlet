package kv

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/coord/coordtest"
)

// SEVERAL NODES ON ONE STORE, each over a connection of its own.
//
// [TestFleetContract] hands every case one FleetStore, and one store is one
// node: it coalesces its own writes to a key, so the cases' concurrency is ONE
// contender on the key however many goroutines put it there. The shared cases
// are what put several nodes on one record at once — three stores on three
// connections to one embedded broker, the easy topology.
func TestSharedFleetContract(t *testing.T) {
	t.Parallel()
	srv, _ := embeddedServer(t)
	coordtest.RunSharedFleet(t, func(t *testing.T) []coord.Fleet {
		prefix := fmt.Sprintf("f%d", bucketSeq.Add(1))
		out := make([]coord.Fleet, sharedFleetNodes)
		for i := range out {
			nc, err := nats.Connect("", nats.InProcessServer(srv))
			if err != nil {
				t.Fatalf("connect node %d: %v", i, err)
			}
			t.Cleanup(nc.Close)
			out[i] = openFleetForTest(t, nc, prefix)
		}
		return out
	})
}

// AND THROUGH LEAVES, which is where the counter failed: every node a leaf of
// its own, joined to one member, reaching the members' buckets across its
// link. A round trip through a leaf is longer than one on a member, so the
// fixed 32 ms backoff ceiling that spread a member's writers no longer spread
// these, and the org's counter lost every one of its sixteen attempts to the
// contract's thirty-two concurrent charges under the solo half's load. Here
// [coordtest.ContendedCharges] charges go through each of three leaves at
// once: ten times that contention through every node, from several nodes.
func TestSharedFleetContractThroughLeaves(t *testing.T) {
	t.Parallel()
	port := leafMember(t)
	clients := make([]jetstream.JetStream, sharedFleetNodes)
	for i := range clients {
		clients[i] = leafClientOf(t, port, fmt.Sprintf("leaf-%d", i))
	}
	coordtest.RunSharedFleet(t, func(t *testing.T) []coord.Fleet {
		prefix := fmt.Sprintf("f%d", bucketSeq.Add(1))
		out := make([]coord.Fleet, len(clients))
		for i, client := range clients {
			out[i] = openFleetVia(t, client, prefix)
		}
		return out
	})
}

// sharedFleetNodes is how many nodes the shared cases put on one store: three,
// the smallest fleet that has a quorum, and enough that one node's batch is
// raced by more than one other.
const sharedFleetNodes = 3

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
