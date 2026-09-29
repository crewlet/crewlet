package search

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"reflect"
	"runtime"
	"slices"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// A TRAINING TAKES AT MOST ITS SHARE OF THE CORES, never fewer than the two
// workers its tick needs where the node has two, and never none.
//
// The node holding the embedding duty also runs seats and answers searches,
// and the k-means and the filing are the only work on it that can occupy
// every core for a minute at a time. Unbounded they did: the training ran on
// GOMAXPROCS workers. Halved without a floor, a two- or three-core node
// trained on ONE, whose training at the largest partition an index serves
// does not fit the duty's tick ([ivfMinWorkers]) — so every tick on it was cut
// off and its partition never got an index. A node allowed a single core
// still trains, on it.
func TestATrainingTakesAtMostItsShareOfTheCores(t *testing.T) {
	t.Parallel()
	for procs := 1; procs <= 256; procs++ {
		workers := workersFor(procs)
		switch {
		case workers < 1:
			t.Fatalf("a node allowed %d core(s) trains on %d workers — it would "+
				"never train", procs, workers)
		case workers > procs:
			t.Fatalf("a node allowed %d core(s) trains on %d workers, more than "+
				"it has", procs, workers)
		case workers < min(procs, 2):
			t.Fatalf("a node allowed %d cores trains on %d worker(s): below two, "+
				"a training at the largest partition an index serves overruns the "+
				"tick, and the partition is never indexed", procs, workers)
		case workers > max(procs/ivfCoreShare, 2):
			t.Fatalf("a node allowed %d cores trains on %d workers, more than "+
				"1/%d of them", procs, workers, ivfCoreShare)
		}
	}
	if got, want := ivfWorkers(), workersFor(runtime.GOMAXPROCS(0)); got != want {
		t.Fatalf("this process trains on %d workers, want the share of its "+
			"own GOMAXPROCS (%d)", got, want)
	}
}

// A TRAINING RUNS ON ITS SHARE OF THE CORES — the public entry points, as the
// duty calls them, measured twice: by how many workers are alive at once, and
// by the CPU time this process spends against the wall-clock time they take.
//
// The bound above is only a number until the training is held to it: a
// TrainIVF or an Assign handing parallelRanges every core would pass every
// other case here. At GOMAXPROCS 4 the share is two workers.
//
// THE WORKERS ARE COUNTED because that is the measure no load can hide: every
// range is a goroutine, alive from its spawn to the end of its range, however
// little CPU the machine gives it. The CPU time is the property itself — cores
// the seats and the searches do not get — but it can only catch a training on
// every core when the machine has those cores free: on this repository's
// shared four-core container a training on every core measured 3.1 cores busy
// when idle and 1.9 under two other test runs, below the bound. So the count
// is what fails a build that trains on every core wherever it runs, and the
// CPU time is what fails one whose workers are bounded and still take more.
//
// NOT PARALLEL, because both readings are the whole process's: a test running
// beside it would add its own training's workers and its CPU time. Top-level
// parallel tests wait until every sequential one has finished, so nothing else
// in this package runs while it measures.
func TestATrainingRunsOnItsShareOfTheCores(t *testing.T) {
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(4))
	workers := ivfWorkers()
	f := NewTopicalFixture(20_000, 3)
	codes := NewCodes(len(f.Codes[0]), f.Len())
	for _, code := range f.Codes {
		codes.Append(code)
	}
	lists := IVFLists(codes.Len())
	var wall, spent time.Duration
	alive := workersAlive(t, func() {
		before, started := cpuTime(t), time.Now()
		index, err := TrainIVF(t.Context(), codes, lists, IVFSeed("S", 0), nil)
		if err != nil {
			t.Fatalf("train: %v", err)
		}
		if _, err := index.Assign(t.Context(), codes); err != nil {
			t.Fatalf("file: %v", err)
		}
		wall, spent = time.Since(started), cpuTime(t)-before
	})
	cores := spent.Seconds() / wall.Seconds()
	t.Logf("a share of %d workers ran %d of them at once and kept %.2f cores "+
		"busy for %v", workers, alive, cores, wall)
	if alive == 0 {
		// A sampler that matches nothing passes every bound below.
		t.Fatalf("the sampler saw no training worker at all over %v — it no "+
			"longer recognises one, and would pass a training on every core", wall)
	}
	if alive > workers {
		t.Fatalf("training and filing %d codes over %d lists ran %d workers at "+
			"once, want at most the %d of its share", codes.Len(), lists, alive, workers)
	}
	// HALF A CORE OF SLACK for the runtime's own work beside the workers —
	// the collector's marking, the scheduler, the sampler's stack dumps — which
	// is far from the two cores a training on every core would add.
	if cores > float64(workers)+0.5 {
		t.Fatalf("training and filing %d codes over %d lists kept %.2f cores busy "+
			"(%v of CPU in %v), want at most the %d workers of its share",
			codes.Len(), lists, cores, spent, wall, workers)
	}
}

// workersAlive runs work and reports the most training workers that were at
// work at once while it ran: the goroutines [parallelRanges] started, found by
// what their stacks say created them, while they are inside [walkStrides].
//
// BY CREATOR, NOT BY COUNT. The process runs goroutines of its own beside the
// training — the NATS server's process-statistics poll starts one on a timer
// in every binary that links the server — and a count of every goroutine
// read that one as a third worker beside a share of two.
//
// AND ONLY WHILE WALKING A RANGE. A worker that has signalled its WaitGroup
// has finished its range but is still listed until it returns, and on a busy
// machine it may not run again before the next k-means round has started its
// own two: counted by creator alone, that exit read as a third worker beside a
// share of two, and the case failed under `make test` while no worker ever
// ran over its share.
//
// SAMPLED every two milliseconds: a k-means round's ranges each run for tens of
// milliseconds and more, so a worker alive for a round cannot fall between two
// samples, while a stack dump of a handful of goroutines costs the workers
// little of their time.
func workersAlive(t *testing.T, work func()) int {
	t.Helper()
	creator := []byte("created by " +
		runtime.FuncForPC(reflect.ValueOf(parallelRanges).Pointer()).Name() + " ")
	walking := []byte(runtime.FuncForPC(reflect.ValueOf(walkStrides).Pointer()).Name() + "(")
	stop, peak := make(chan struct{}), make(chan int, 1)
	go func() {
		ticker := time.NewTicker(2 * time.Millisecond)
		defer ticker.Stop()
		buf, most := make([]byte, 64<<10), 0
		for {
			n := runtime.Stack(buf, true)
			for n == len(buf) {
				buf = make([]byte, 2*len(buf))
				n = runtime.Stack(buf, true)
			}
			working := 0
			for _, g := range bytes.Split(buf[:n], []byte("\n\n")) {
				if bytes.Contains(g, creator) && bytes.Contains(g, walking) {
					working++
				}
			}
			most = max(most, working)
			select {
			case <-stop:
				peak <- most
				return
			case <-ticker.C:
			}
		}
	}()
	// Closed by a defer, so a work that fails its test stops the sampler too.
	func() {
		defer close(stop)
		work()
	}()
	return <-peak
}

// cpuTime is the CPU time this process has spent, in user and system mode.
func cpuTime(t *testing.T) time.Duration {
	t.Helper()
	var usage syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &usage); err != nil {
		t.Fatalf("getrusage: %v", err)
	}
	return time.Duration(usage.Utime.Nano() + usage.Stime.Nano())
}

// THE RANGES RUN ON AT MOST THE WORKERS ASKED FOR, and cover every index
// exactly once.
//
// The first half is the bound [ivfWorkers] chooses actually binding: a split
// onto more goroutines than workers would put the training back on every core.
// It counts the GOROUTINES fn ran on rather than its calls, because a range is
// walked in strides ([ivfStride]) and one worker calls fn once a stride. The
// second is what makes the answer independent of the split.
func TestParallelRangesRunsAtMostItsWorkers(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ workers, n int }{
		{1, 1000}, {2, 1000}, {3, 1000}, {7, 1000}, {4, 5}, {4, 3}, {3, 0}, {0, 10},
		{2, 5*ivfStride + 3}, {3, 7 * ivfStride},
	} {
		t.Run(fmt.Sprintf("%d workers over %d", tc.workers, tc.n), func(t *testing.T) {
			t.Parallel()
			var (
				mu   sync.Mutex
				seen = make([]int, tc.n)
				ran  = map[string]bool{}
			)
			if err := parallelRanges(t.Context(), tc.workers, tc.n, func(from, to int) {
				mu.Lock()
				defer mu.Unlock()
				ran[goroutineOf(t)] = true
				for i := from; i < to; i++ {
					seen[i]++
				}
			}); err != nil {
				t.Fatalf("split: %v", err)
			}
			if limit := max(tc.workers, 1); len(ran) > limit {
				t.Errorf("split onto %d goroutines, want at most %d", len(ran), limit)
			}
			for i, n := range seen {
				if n != 1 {
					t.Fatalf("index %d was covered %d times, want once", i, n)
				}
			}
		})
	}
}

// goroutineOf names the goroutine it runs on, from the first line of its own
// stack ("goroutine 42 [running]:").
func goroutineOf(t *testing.T) string {
	t.Helper()
	buf := make([]byte, 64)
	line, _, _ := bytes.Cut(buf[:runtime.Stack(buf, false)], []byte(" ["))
	if !bytes.HasPrefix(line, []byte("goroutine ")) {
		t.Fatalf("a stack that does not start by naming its goroutine: %q", line)
	}
	return string(line)
}

// A TRAINING CUT OFF HANDS ITS CORES BACK WITHIN A STRIDE of its tick ending,
// in every CPU-bound step it takes.
//
// The duty's tick is bounded — by its budget, and by the lease it renews as it
// runs — and a step that overran it publishes nothing. The k-means looked at
// its context only between rounds and the filing not at all, so a tick cut off
// as the filing began went on filing every code on its share of the cores for
// the better part of a minute, for a result it then threw away, on a node that
// might no longer hold the duty.
//
// The split itself is held to it exactly: a worker reads the context before
// each stride, so after it ends no worker starts another, and what was covered
// is at most the stride each worker was in.
func TestATrainingStopsWithinAStrideOfItsTickEnding(t *testing.T) {
	t.Parallel()
	t.Run("the split", func(t *testing.T) {
		t.Parallel()
		for _, workers := range []int{1, 2, 3} {
			ctx, cancel := context.WithCancel(t.Context())
			var covered atomic.Int64
			err := parallelRanges(ctx, workers, 64*ivfStride, func(from, to int) {
				cancel()
				covered.Add(int64(to - from))
			})
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("on %d workers a split whose context ended answered %v, "+
					"want the context's error", workers, err)
			}
			if got, most := covered.Load(), int64(workers*ivfStride); got > most {
				t.Errorf("on %d workers the split went on for %d indices after its "+
					"context ended, want at most a stride a worker (%d)", workers, got, most)
			}
		}
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if err := parallelRanges(ctx, 2, 10*ivfStride, func(int, int) {
			t.Error("a split whose context had already ended ran a stride")
		}); !errors.Is(err, context.Canceled) {
			t.Fatalf("a split whose context had already ended answered %v", err)
		}
	})

	f := NewTopicalFixture(4_000, 11)
	codes := NewCodes(len(f.Codes[0]), f.Len())
	for _, code := range f.Codes {
		codes.Append(code)
	}
	lists := IVFLists(codes.Len())
	index, err := TrainIVF(t.Context(), codes, lists, IVFSeed("S", 0), nil)
	if err != nil {
		t.Fatalf("train: %v", err)
	}
	ended, cancel := context.WithCancel(t.Context())
	cancel()
	t.Run("the k-means", func(t *testing.T) {
		t.Parallel()
		if _, err := TrainIVF(ended, codes, lists, IVFSeed("S", 0), nil); !errors.Is(err, context.Canceled) {
			t.Fatalf("a training whose tick had ended answered %v, want the "+
				"context's error", err)
		}
	})
	t.Run("the filing", func(t *testing.T) {
		t.Parallel()
		filed, err := index.Assign(ended, codes)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("a filing whose tick had ended answered %v, want the "+
				"context's error", err)
		}
		if filed != nil {
			t.Fatalf("a filing cut off answered %d rows beside its error — a "+
				"partial filing is one the caller could install", len(filed))
		}
	})
}

// THE INDEX IS THE SAME BYTES ON ANY NUMBER OF WORKERS, and so is its filing.
//
// The bound on the workers is a property of the NODE — its cores and its
// GOMAXPROCS — and the duty moves between nodes: a centroid that depended on
// the split would make the index a function of which machine happened to hold
// the lease.
func TestTheIndexIsTheSameOnAnyNumberOfWorkers(t *testing.T) {
	t.Parallel()
	f := NewTopicalFixture(6_000, 7)
	codes := NewCodes(len(f.Codes[0]), f.Len())
	for _, code := range f.Codes {
		codes.Append(code)
	}
	lists := IVFLists(codes.Len())
	seed := IVFSeed("S", 0)
	var (
		want     []byte
		wantRows []int32
	)
	for _, workers := range []int{1, 2, 3, 8} {
		index, err := trainIVF(t.Context(), codes, lists, seed, nil, workers)
		if err != nil {
			t.Fatalf("train on %d workers: %v", workers, err)
		}
		rows, err := index.assign(t.Context(), codes, workers)
		if err != nil {
			t.Fatalf("file on %d workers: %v", workers, err)
		}
		if want == nil {
			want, wantRows = index.Bytes(), rows
			continue
		}
		if !bytes.Equal(index.Bytes(), want) {
			t.Fatalf("trained on %d workers the centroids differ from one "+
				"worker's", workers)
		}
		if !slices.Equal(rows, wantRows) {
			t.Fatalf("filed on %d workers the rows differ from one worker's", workers)
		}
	}
}

// BenchmarkIVFTrainingShare is a training's two CPU-bound steps — the k-means
// and filing every code — at the largest partition an index serves and the
// largest list count, on the share of the cores the duty runs them on
// ([ivfWorkers]) and on every core, which is what they ran on before the share
// existed: the figures behind [ivfCoreShare].
//
// And what the rest of the node gets meanwhile. Two searchers loop over the
// first stage's own arithmetic — a Hamming top-[Stage1Depth] over
// [scanProbeRows] codes — for as long as the training runs, and the arm reports
// their p95; the idle arm is the same two with no training at all.
func BenchmarkIVFTrainingShare(b *testing.B) {
	const n = 500_000
	f := NewTopicalFixture(n, 1)
	codes := NewCodes(len(f.Codes[0]), f.Len())
	for _, code := range f.Codes {
		codes.Append(code)
	}
	lists := IVFLists(n)
	for _, arm := range []struct {
		name    string
		workers int
	}{
		{"idle", 0},
		{"share", ivfWorkers()},
		{"every-core", runtime.GOMAXPROCS(0)},
	} {
		b.Run(fmt.Sprintf("n=%d/lists=%d/%s=%d", n, lists, arm.name, arm.workers),
			func(b *testing.B) {
				for b.Loop() {
					stop := make(chan struct{})
					latencies := searchWhile(codes, stop)
					if arm.workers == 0 {
						time.Sleep(20 * time.Second)
						close(stop)
						b.ReportMetric(p95ms(latencies()), "scan-p95-ms")
						continue
					}
					started := time.Now()
					index, err := trainIVF(b.Context(), codes, lists,
						IVFSeed("bench", 0), nil, arm.workers)
					if err != nil {
						b.Fatal(err)
					}
					kmeans := time.Since(started)
					started = time.Now()
					if _, err := index.assign(b.Context(), codes, arm.workers); err != nil {
						b.Fatal(err)
					}
					filing := time.Since(started)
					close(stop)
					b.ReportMetric(kmeans.Seconds(), "kmeans-s")
					b.ReportMetric(filing.Seconds(), "filing-s")
					b.ReportMetric(p95ms(latencies()), "scan-p95-ms")
				}
			})
	}
}

// scanProbeRows is how many codes a searcher in [BenchmarkIVFTrainingShare]
// scans a query over: the 40 000 sources the scan's own benchmarks measure at.
const scanProbeRows = 40_000

// searchWhile runs two searchers until stop closes, and returns what their
// queries took once they have stopped.
func searchWhile(codes Codes, stop <-chan struct{}) func() []time.Duration {
	var (
		mu  sync.Mutex
		out []time.Duration
		wg  sync.WaitGroup
	)
	for s := range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for q := s; ; q += 2 {
				select {
				case <-stop:
					return
				default:
				}
				query := codes.At(q % codes.Len())
				started := time.Now()
				top := newTopK(Stage1Depth)
				for i := range min(scanProbeRows, codes.Len()) {
					top.offer(i, float64(-Hamming(codes.At(i), query)))
				}
				_ = top.ordered()
				took := time.Since(started)
				mu.Lock()
				out = append(out, took)
				mu.Unlock()
			}
		}()
	}
	return func() []time.Duration {
		wg.Wait()
		return out
	}
}

// p95ms is the 95th percentile of took, in milliseconds.
func p95ms(took []time.Duration) float64 {
	if len(took) == 0 {
		return 0
	}
	slices.Sort(took)
	return float64(took[(len(took)*95)/100]) / float64(time.Millisecond)
}
