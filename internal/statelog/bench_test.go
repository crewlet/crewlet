package statelog_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/jsapi"
	"github.com/crewlet/crewlet/internal/logging"
	js "github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/queue/jetstream/jetstreamtest"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/recordseal"
	"github.com/crewlet/crewlet/internal/statelog"
)

// BenchmarkArbitratedWrite is one arbitrated write end to end on a node whose
// every seam is real ([wiredNode]): the fence's eviction read, the session
// wait, ONE snapshot, the decision, the broker's arbitration, this node's
// applier and WaitApplied; beside it, the two halves of a linearizable read.
// On the probe domain, one row a record, so these are the FRAMEWORK'S FLOOR.
//
// Arms: members=1|3 as [fleetOf] says; sync=always is production's default
// fsync per write, sync=interval a thirty-second window no write waits on;
// writers and readers as [benchDriver] says. Each arm reports its operations
// a second and the p50 and p99 of one operation's latency.
func BenchmarkArbitratedWrite(b *testing.B) {
	quiet(b)
	for _, members := range []int{1, 3} {
		for _, mode := range []string{"always", "interval"} {
			b.Run(fmt.Sprintf("members=%d/sync=%s", members, mode), func(b *testing.B) {
				cfg := js.Config{SyncAlways: mode == "always"}
				if !cfg.SyncAlways {
					cfg.SyncInterval = 30 * time.Second
				}
				for _, d := range fleetOf(b, members, cfg) {
					benchDriver(b, d)
				}
			})
		}
	}
}

// driver is a node an arm drives, and the name its arms carry.
type driver struct {
	name string
	node *wiredNode
}

// fleetOf stands up members data nodes on their own broker (one) or cluster
// (more), each applying the log, and answers the nodes the arms drive: solo,
// or the stream's leader as the arm began and a follower. Three members share
// ONE machine with no network hop, so a cluster's figures are pessimistic per
// node and say nothing of a datacenter's round trip.
func fleetOf(b *testing.B, members int, cfg js.Config) []driver {
	b.Helper()
	if members == 1 {
		return []driver{{"solo", running(newWiredNode(b, soloBroker(b, cfg), "node-a"))}}
	}
	c := jetstreamtest.StartCluster(b, members, cfg)
	spec := ensureWiredLog(b, c.Client(b, 0))
	leader := streamLeader(b, c, spec.Name)
	fleet := make([]*wiredNode, members)
	for i := range members {
		fleet[i] = running(newWiredNode(b, c.Client(b, i), fmt.Sprintf("node-%d", i)))
	}
	return []driver{{"leader", fleet[leader]}, {"follower", fleet[(leader+1)%members]}}
}

// benchDriver runs d's arms. writers=1 is ONE SUBJECT'S SERIAL CHAIN, what
// one object can do before batching; writers=16 sixteen subjects at once. No
// two writers share a subject, since the design serialises an object's writes
// through one writer, and each write carries its writer's last as its
// session. read=barrier is the append alone, read=linearizable it and the
// applier reaching it; readers=16 coalesce ([statelog.ReadIndex.Read]), so
// the read arms report `appends/read`.
func benchDriver(b *testing.B, d driver) {
	for _, writers := range []int{1, 16} {
		b.Run(fmt.Sprintf("from=%s/writers=%d", d.name, writers), func(b *testing.B) {
			// EACH WRITER'S OWN LAST WRITE, written and read by that
			// writer's goroutine alone.
			sessions := make([]statelog.Position, writers)
			measure(b, writers, "writes/s", func(ctx context.Context, w int) error {
				res, err := d.node.write(ctx, probeSubject(fmt.Sprintf("%s-%d-%d", d.name, writers, w)),
					sessions[w])
				sessions[w] = res.Position
				return err
			})
		})
	}
	for _, read := range []struct {
		name string
		op   func(context.Context) (statelog.Position, error)
	}{
		{"barrier", d.node.barrier},
		{"linearizable", d.node.read},
	} {
		for _, readers := range []int{1, 16} {
			b.Run(fmt.Sprintf("from=%s/read=%s/readers=%d", d.name, read.name, readers), func(b *testing.B) {
				before := logEnd(b, d.node)
				measure(b, readers, "reads/s", func(ctx context.Context, _ int) error {
					_, err := read.op(ctx)
					return err
				})
				// NOTHING ELSE APPENDS during a read arm (no writes, no
				// heartbeat), so every record gained is a read's barrier.
				b.ReportMetric(float64(logEnd(b, d.node)-before)/float64(b.N), "appends/read")
			})
		}
	}
}

// logEnd is the last sequence on node's log.
func logEnd(b *testing.B, node *wiredNode) uint64 {
	b.Helper()
	_, last, err := node.log.Bounds(b.Context())
	if err != nil {
		b.Fatalf("read the log's end: %v", err)
	}
	return last
}

// BenchmarkApplierDrain is the apply lane for small records: a burst already
// on the log, drained by an applier started after it landed, each record's
// framework bookkeeping (anchor, ledger row, checkpoint) plus one row.
//
// The burst is [statelog.ApplyTxRowBudget] records, one transaction's row
// budget, so batches end on the time budget as a burst's do; large records
// are internal/store's BenchmarkLogApplyDrain. Each burst's own broker and
// node are built and torn down outside the timer, which runs from the
// applier's start to its reaching the last record.
func BenchmarkApplierDrain(b *testing.B) {
	quiet(b)
	const records = statelog.ApplyTxRowBudget
	b.Run(fmt.Sprintf("records=%d", records), func(b *testing.B) {
		var drained time.Duration
		bursts := 0
		for b.Loop() {
			b.StopTimer()
			q := soloBroker(b, js.Config{SyncAlways: true})
			last := appendBurst(b, q, records)
			node := newWiredNode(b, q, "node-d")
			began := time.Now()
			b.StartTimer()
			node.start()
			ctx, cancel := context.WithTimeout(b.Context(), 5*time.Minute)
			err := node.runner.WaitReached(ctx, last)
			cancel()
			b.StopTimer()
			drained += time.Since(began)
			bursts++
			if err != nil {
				b.Fatalf("drain %d records: %v", records, err)
			}
			// TORN DOWN NOW, so no burst is measured beside the idle
			// brokers and appliers of the ones before it.
			if err := node.stop(); err != nil {
				b.Fatalf("the applier stopped with %v", err)
			}
			if err := q.Stop(b.Context()); err != nil {
				b.Fatalf("stop the broker: %v", err)
			}
			b.StartTimer()
		}
		b.ReportMetric(float64(bursts*records)/drained.Seconds(), "records/s")
	})
}

// appendBurst lands n probe records on q's probe log, each on its own subject,
// straight onto the log but SIGNED as a peer's publisher signs them (a wired
// node's applier opens every frame), and answers the last one's position.
func appendBurst(b *testing.B, q *js.Queue, n int) statelog.Position {
	b.Helper()
	spec := ensureWiredLog(b, q)
	log, err := q.DomainLog(b.Context(), spec.Name)
	if err != nil {
		b.Fatalf("open the log: %v", err)
	}
	signer, err := recordseal.NewSigner(probeDomain{}.Name(), testRing())
	if err != nil {
		b.Fatalf("NewSigner: %v", err)
	}
	var last uint64
	for i := range n {
		subject := probeSubject(fmt.Sprintf("drain-%d", i))
		opID := statelog.NewOpID(time.Now(), "drain")
		record := probeRecord(statelog.Stamp{Gen: 1, Writer: "node-w"}, subject,
			statelog.ScopeSet{Paths: []string{subject.String()}}, opID, "drain")
		if last, _, err = log.Append(b.Context(),
			topics.LogSubject(spec.SubjectPrefix, subject.Kind, subject.ID), opID, nil,
			signer.Seal(record)); err != nil {
			b.Fatalf("append record %d of the burst: %v", i, err)
		}
	}
	return statelog.Position{Stream: spec.Name, Generation: 1, Seq: last}
}

// quiet raises the process's log level to Warn while a benchmark runs: an
// INFO line from the applier's goroutine can land inside a result line, which
// a comparison tool cannot read. Warn and above still print, as findings.
func quiet(b *testing.B) {
	logging.SetVerbosity(slog.LevelWarn, logging.FormatConsole)
	b.Cleanup(func() { logging.SetVerbosity(slog.LevelInfo, logging.FormatConsole) })
}

// measure runs b.N operations across workers goroutines and reports them a
// second, under unit, with the p50 and p99 of one operation's latency.
//
// b.N is COUNTED BY HAND, not by b.Loop, for objstoretest's reason: through
// b.Loop a pool's last operations finish after the timer stops. So this runs
// at every step of the ramp, and an arm's nodes are built in its parent.
func measure(b *testing.B, workers int, unit string, op func(ctx context.Context, worker int) error) {
	var (
		next atomic.Int64
		wg   sync.WaitGroup
		mu   sync.Mutex
		took []time.Duration
	)
	errs := make([]error, workers)
	b.ResetTimer()
	started := time.Now()
	for w := range workers {
		wg.Go(func() {
			var mine []time.Duration
			for next.Add(1) <= int64(b.N) {
				at := time.Now()
				if err := op(b.Context(), w); err != nil {
					errs[w] = err
					break
				}
				mine = append(mine, time.Since(at))
			}
			mu.Lock()
			took = append(took, mine...)
			mu.Unlock()
		})
	}
	wg.Wait()
	elapsed := time.Since(started)
	b.StopTimer()
	if err := errors.Join(errs...); err != nil {
		b.Fatal(err)
	}
	slices.Sort(took)
	b.ReportMetric(quantile(took, 0.50), "p50-ms")
	b.ReportMetric(quantile(took, 0.99), "p99-ms")
	b.ReportMetric(float64(b.N)/elapsed.Seconds(), unit)
}

// quantile is the nearest-rank q-quantile of sorted, in milliseconds.
func quantile(sorted []time.Duration, q float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	rank := max(int(math.Ceil(q*float64(len(sorted)))), 1)
	return float64(sorted[rank-1]) / float64(time.Millisecond)
}

// streamLeader is the index of the member leading stream, waiting for one to
// be elected. A benchmark may read the cluster's own view, which the read path
// may not (TestStreamInfoIsNotOnTheReadPath): here it only labels an arm.
func streamLeader(b *testing.B, c *jetstreamtest.Cluster, stream string) int {
	b.Helper()
	nc, err := c.Servers[0].Conn()
	if err != nil {
		b.Fatalf("connect to member 0: %v", err)
	}
	defer nc.Close()
	client, err := jsapi.Embedded().Client(nc)
	if err != nil {
		b.Fatalf("a JetStream client: %v", err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		ctx, cancel := context.WithTimeout(b.Context(), 5*time.Second)
		leader, err := leaderOf(ctx, client, stream)
		cancel()
		if err == nil && leader != "" {
			for i, cfg := range c.Configs {
				if cfg.ServerName == leader {
					return i
				}
			}
			b.Fatalf("stream %s is led by %q, which is no member of the cluster", stream, leader)
		}
		if time.Now().After(deadline) {
			b.Fatalf("stream %s elected no leader in thirty seconds: %v", stream, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// leaderOf is the server name the cluster reports leading stream, or "".
func leaderOf(ctx context.Context, client jetstream.JetStream, stream string) (string, error) {
	s, err := client.Stream(ctx, stream)
	if err != nil {
		return "", err
	}
	info, err := s.Info(ctx)
	if err != nil || info.Cluster == nil {
		return "", err
	}
	return info.Cluster.Leader, nil
}
