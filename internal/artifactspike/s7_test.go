package artifactspike

import (
	"bytes"
	"errors"
	"fmt"
	"math/rand/v2"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/objstore"
	js "github.com/crewlet/crewlet/internal/queue/jetstream"
)

// SPIKE S7: the default object store under load.
//
// On the `nats` backend the object bucket is one more replicated stream on the
// members that hold the write-ahead log, so a large publish shares their
// disks, their fsyncs and their replication links with every state-log append.
// Three benchmarks:
//
//   - WAL: on a three-member fleet with `sync: always`, the append latency of
//     a state log (a probe stream, appended to at a steady rate) and the
//     round trip of datastore writes, before and while two 64 MiB publishes
//     stream into the bucket through the transfer leg.
//   - Ceiling: how much content a bucket reservation really holds before the
//     broker refuses a put with objstore.ErrFull, for large and small files.
//   - Memory: the broker's heap per object held, which is what a member pays
//     for a bucket of many small files whatever their bytes.
//
// Member replacement at 10, 50 and 100 GiB of objects needs disks this
// harness does not claim; docs/reference/artifact-capacity.md gives the
// procedure.
//
// Run: go test -run '^$' -bench S7 -benchtime 1x ./internal/artifactspike

const (
	// walProbeEvery is the probe's append interval: twenty appends a
	// second, a busy company's steady state across its logs, and light
	// enough that the baseline measures the log rather than a queue the
	// probe built itself.
	walProbeEvery = 50 * time.Millisecond

	// walWriteEvery is the datastore writes' interval beside it: five a
	// second, the low end of a review's peak on one store.
	walWriteEvery = 200 * time.Millisecond

	// walBaseline is how long the load runs before the publishes start.
	walBaseline = 8 * time.Second

	// walPublishes is how many 64 MiB publishes stream, one after another.
	walPublishes = 2

	// walProbePayload is one probe append: a small record's size.
	walProbePayload = 512
)

// walProbeStream is the probe log: a stream of its own, replicated as the
// state logs are, so its appends measure the write-ahead log and nothing an
// applier does.
var walProbeStream = js.DomainStream{
	Name: "SPIKE_WAL_PROBE", Subjects: []string{"spike.wal.>"},
	MaxBytes: 64 << 20, MaxMessageBytes: 4 << 10, Duplicates: 2 * time.Minute,
}

// BenchmarkS7WALUnderPublish measures the write-ahead log while the bucket
// takes two maximum publishes.
func BenchmarkS7WALUnderPublish(b *testing.B) {
	for range b.N {
		b.StopTimer()
		r := rig(b, "wal", topoFleet, 1)
		q := r.brokers.queues
		if err := q[0].EnsureDomainStream(b.Context(), walProbeStream); err != nil {
			b.Fatalf("provision the probe log: %v", err)
		}
		probeLog, err := q[1].DomainLog(b.Context(), walProbeStream.Name)
		if err != nil {
			b.Fatalf("open the probe log: %v", err)
		}
		uploads := newUploadRig(b, openBucket(b, q[0], len(q), publishBucketBytes))
		writer := r.nodes[2]
		id := r.stores[0]

		var (
			publishing                atomic.Bool
			appendBase, appendPublish latencies
			writeBase, writePublish   latencies
			failures                  atomic.Int64
			firstErr                  atomic.Value
			load                      sync.WaitGroup
		)
		fail := func(err error) {
			failures.Add(1)
			firstErr.CompareAndSwap(nil, err.Error())
		}
		stop := make(chan struct{})
		payload := bytes.Repeat([]byte("w"), walProbePayload)
		// OPEN-LOOP LOAD: each tick starts its own call, so an append that
		// stalls shows as its own latency rather than as fewer appends.
		load.Go(func() {
			var calls sync.WaitGroup
			defer calls.Wait()
			tick := time.NewTicker(walProbeEvery)
			defer tick.Stop()
			for k := 0; ; k++ {
				select {
				case <-stop:
					return
				case <-tick.C:
				}
				during := publishing.Load()
				calls.Go(func() {
					t0 := time.Now()
					if _, _, err := probeLog.Append(b.Context(), "spike.wal.probe",
						fmt.Sprintf("probe-%d", k), nil, payload); err != nil {
						fail(err)
						return
					}
					if during {
						appendPublish.add(time.Since(t0))
					} else {
						appendBase.add(time.Since(t0))
					}
				})
			}
		})
		load.Go(func() {
			var calls sync.WaitGroup
			defer calls.Wait()
			tick := time.NewTicker(walWriteEvery)
			defer tick.Stop()
			rnd := rand.New(rand.NewPCG(7, 7))
			for {
				select {
				case <-stop:
					return
				case <-tick.C:
				}
				during := publishing.Load()
				ops := opsOf(rnd, shapeCell, seededRows)
				calls.Go(func() {
					t0 := time.Now()
					res, err := writer.writer.Write(b.Context(), editor("member-05"), id,
						artifacts.DataWrite{Ops: ops})
					switch {
					case err != nil:
						fail(err)
						return
					case res.Verdict != artifacts.DataApplied:
						fail(fmt.Errorf("a datastore write answered %q (%s)", res.Verdict, res.Reason))
						return
					}
					if during {
						writePublish.add(time.Since(t0))
					} else {
						writeBase.add(time.Since(t0))
					}
				})
			}
		})

		time.Sleep(walBaseline)
		b.StartTimer()
		publishing.Store(true)
		started := time.Now()
		// A FAILED PUBLISH IS A RESULT, not a broken harness: it is what
		// the bucket did under this load, so it is counted and named and
		// the log's latencies are still reported for the window it ran.
		refused := 0
		for round := range walPublishes {
			if _, err := uploads.streamPublish(b, round); err != nil {
				refused++
				b.Logf("publish %d failed under the load: %v", round, err)
			}
		}
		window := time.Since(started)
		publishing.Store(false)
		b.StopTimer()
		close(stop)
		load.Wait()
		if n := failures.Load(); n > 0 {
			b.Fatalf("%d append(s) or write(s) failed under the publish; the first: %v", n, firstErr.Load())
		}
		appendBase.report(b, "append-base")
		appendPublish.report(b, "append-publish")
		writeBase.report(b, "write-base")
		writePublish.report(b, "write-publish")
		b.ReportMetric(window.Seconds()/walPublishes, "publish-s")
		b.ReportMetric(float64(refused), "publishes-failed")
		b.ReportMetric(float64(appendPublish.count()), "append-samples-publish")
	}
}

// BenchmarkS7BucketCeiling fills a bucket to its reservation with files of
// one size and reports the content it held before the broker refused a put,
// as a share of the reservation, and what each object cost beyond its bytes.
func BenchmarkS7BucketCeiling(b *testing.B) {
	for _, c := range []struct {
		name        string
		reservation int64
		size        int64
	}{
		{"files=1MiB", 64 << 20, 1 << 20},
		{"files=4KiB", 8 << 20, 4 << 10},
	} {
		b.Run(c.name, func(b *testing.B) {
			for range b.N {
				b.StopTimer()
				br := startBrokers(b, topoSolo)
				store, err := objstore.NewStore(openBucket(b, br.queues[0], 1, c.reservation))
				if err != nil {
					b.Fatalf("build the object store: %v", err)
				}
				body := bytes.Repeat([]byte("x"), int(c.size))
				b.StartTimer()
				var held int64
				objects := 0
				for {
					_, err := store.Put(b.Context(), bytes.NewReader(body), c.size, objstore.PutMeta{})
					if errors.Is(err, objstore.ErrFull) {
						break
					}
					if err != nil {
						b.Fatalf("put object %d: %v", objects, err)
					}
					held += c.size
					objects++
					if held > c.reservation {
						b.Fatalf("the bucket took %d bytes past its %d-byte reservation without "+
							"refusing a put", held, c.reservation)
					}
				}
				b.StopTimer()
				b.ReportMetric(float64(held)/float64(c.reservation), "content/reservation")
				b.ReportMetric(float64(objects), "objects")
				b.ReportMetric(float64(c.reservation-held)/float64(objects), "overhead-B/object")
			}
		})
	}
}

// memoryObjects is how many objects the memory benchmark holds: enough that
// the per-object figure is not the bucket's fixed cost.
const memoryObjects = 10_000

// BenchmarkS7BrokerMemoryPerObject reports the heap one object held in the
// bucket costs the process, the embedded broker included.
func BenchmarkS7BrokerMemoryPerObject(b *testing.B) {
	for range b.N {
		b.StopTimer()
		br := startBrokers(b, topoSolo)
		store, err := objstore.NewStore(openBucket(b, br.queues[0], 1, 256<<20))
		if err != nil {
			b.Fatalf("build the object store: %v", err)
		}
		body := bytes.Repeat([]byte("m"), 1<<10)
		before := settledHeap()
		b.StartTimer()
		for k := range memoryObjects {
			if _, err := store.Put(b.Context(), bytes.NewReader(body), int64(len(body)),
				objstore.PutMeta{}); err != nil {
				b.Fatalf("put object %d: %v", k, err)
			}
		}
		b.StopTimer()
		after := settledHeap()
		b.ReportMetric(float64(after-before)/memoryObjects, "heap-B/object")
		b.ReportMetric(float64(after-before)/memoryObjects*1_000_000/mib, "heap-MiB-per-million")
	}
}

// settledHeap is the in-use heap once the broker's caches have expired.
func settledHeap() int64 {
	time.Sleep(heapSettle)
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return int64(m.HeapInuse)
}
