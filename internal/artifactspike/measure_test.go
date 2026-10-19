package artifactspike

import (
	"math"
	"runtime"
	"slices"
	"sync"
	"syscall"
	"testing"
	"time"
)

// THE MEASUREMENTS EVERY SPIKE SHARES. A quantile here is EXACT, read from the
// sorted samples, unlike the metrics recorder's histogram estimate (accurate
// to a bucket): a spike reports a p99 an objective is judged against, and an
// estimate that may be twice the truth cannot settle whether it holds.

// latencies collects durations from many goroutines.
type latencies struct {
	mu      sync.Mutex
	samples []time.Duration
}

func (l *latencies) add(d time.Duration) {
	l.mu.Lock()
	l.samples = append(l.samples, d)
	l.mu.Unlock()
}

// count is how many samples were taken.
func (l *latencies) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.samples)
}

// quantile is the qth quantile by the nearest-rank method, zero with no
// samples.
func (l *latencies) quantile(q float64) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.samples) == 0 {
		return 0
	}
	sorted := slices.Clone(l.samples)
	slices.Sort(sorted)
	rank := int(math.Ceil(q*float64(len(sorted)))) - 1
	return sorted[min(max(rank, 0), len(sorted)-1)]
}

// report states p50, p95 and p99 in milliseconds under the given prefix.
func (l *latencies) report(b *testing.B, prefix string) {
	b.ReportMetric(ms(l.quantile(0.50)), prefix+"-p50-ms")
	b.ReportMetric(ms(l.quantile(0.95)), prefix+"-p95-ms")
	b.ReportMetric(ms(l.quantile(0.99)), prefix+"-p99-ms")
}

func ms(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }

// cpuTime is the CPU this process has used, user and system together.
//
// THE PROCESS, NOT A GOROUTINE: a spike's broker members, runners and hubs
// share one process, so the figure is everything the topology spent; a spike
// reports it per unit of work over a window in which nothing else ran.
func cpuTime(tb testing.TB) time.Duration {
	tb.Helper()
	var u syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &u); err != nil {
		tb.Fatalf("read this process's CPU time: %v", err)
	}
	return time.Duration(u.Utime.Nano() + u.Stime.Nano())
}

// heapPeak samples the in-use heap every interval until stopped, answering the
// highest reading and the reading it started from: what a streaming path
// holds is the difference.
type heapPeak struct {
	stop chan struct{}
	done chan struct{}
	base uint64
	peak uint64
}

func watchHeap(interval time.Duration) *heapPeak {
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	h := &heapPeak{stop: make(chan struct{}), done: make(chan struct{}),
		base: m.HeapInuse, peak: m.HeapInuse}
	go func() {
		defer close(h.done)
		tick := time.NewTicker(interval)
		defer tick.Stop()
		for {
			select {
			case <-h.stop:
				return
			case <-tick.C:
				runtime.ReadMemStats(&m)
				h.peak = max(h.peak, m.HeapInuse)
			}
		}
	}()
	return h
}

// growth stops the sampler and answers how far the heap rose above where it
// started, never negative.
func (h *heapPeak) growth() uint64 {
	close(h.stop)
	<-h.done
	if h.peak < h.base {
		return 0
	}
	return h.peak - h.base
}

// settled waits d, collects, and answers how far the heap stands above where
// the sampler started: what a path left behind once the caches it filled
// (the broker's message block cache among them) have expired. Call it after
// [heapPeak.growth].
func (h *heapPeak) settled(d time.Duration) int64 {
	time.Sleep(d)
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return int64(m.HeapInuse) - int64(h.base)
}

const mib = 1 << 20
