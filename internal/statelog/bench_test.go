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
	"github.com/crewlet/crewlet/internal/statelog"
)

// BenchmarkArbitratedWrite is one arbitrated write end to end, on a node whose
// every seam a healthy write takes is real ([wiredNode]): the fence's read of
// this node's own eviction row, the session wait for the writer's own last
// write, ONE snapshot of the real rows, the decision, the append the broker
// arbitrates, this node's runner applying it from its own consumer, and
// WaitApplied — and the two halves of a linearizable read beside it.
//
// ON THE PROBE DOMAIN, one row a record, so these are the FRAMEWORK'S FLOOR:
// a real domain's decision and apply add their own cost on top.
//
// THE ARMS, by the names a comparison keys on:
//
//   - members=1 is a solo node; members=3 a fleet of three data nodes, every
//     one running its own applier, driven from the stream's leader
//     (`from=leader`, as it was when the arm began — raft may move it) and
//     from a follower (`from=follower`). THE THREE MEMBERS SHARE ONE MACHINE:
//     three brokers, three appliers and three estates on its CPUs, with no
//     network hop between them, so the cluster's figures are pessimistic per
//     node and say nothing about a datacenter's round trip.
//   - sync=always is production's default `stream.sync`, an fsync per write
//     on every member; sync=interval declines it, at a thirty-second window —
//     the write path never waits on the flush, so the window's length does
//     not enter the figure.
//   - writers=1 is ONE SUBJECT'S SERIAL CHAIN, each write arbitrated against
//     the anchor the one before it advanced — what one object, or one
//     repository's committer, can do before batching; writers=16 is sixteen
//     subjects written at once, a node's aggregate. Each writer has a subject
//     of its own: writers CONTENDING on one subject are deliberately not
//     measured, since the design serialises an object's writes through one
//     writer precisely so they never do. Every write carries its writer's
//     own last write as its session, as a seat's turn does.
//   - read=barrier is the barrier a linearizable read appends, alone;
//     read=linearizable is that and this node's applier reaching it, which is
//     what such a read waits through before it reads a row. readers=1 is one
//     reader at a time; readers=16 is sixteen at once on one node, which the
//     read index COALESCES onto shared appends ([statelog.ReadIndex.Read]) —
//     a reader that arrives while one is in flight waits it out and joins the
//     next — and those arms also report the barriers actually appended per
//     read (`appends/read`), which is what the coalescing buys.
//
// Each arm reports its operations a second and the p50 and p99 of one
// operation's latency.
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
// (more), each applying the log, and answers the nodes the arms drive.
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
				// NOTHING ELSE APPENDS while a read arm runs — no arm
				// writes, and no node runs a heartbeat — so every
				// record the log gained is a barrier one of these reads
				// appended or joined.
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

// BenchmarkApplierDrain is the apply lane for small records: a burst of probe
// records already on the log, drained by an applier started after they
// landed — every record's framework bookkeeping (its anchor, its ledger row,
// the checkpoint) plus one row of its own, the cost every domain's records
// share on a node's one write lane.
//
// A BURST OF [statelog.ApplyTxRowBudget] records, one transaction's row budget
// at a row a record, so the batches end on the time budget as a burst's do.
// Large records' rows are internal/store's BenchmarkLogApplyDrain.
//
// Each burst gets a broker and a node of its own, built and torn down outside
// the timer, which runs from the applier's start to its reaching the last
// record.
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
			// TORN DOWN NOW rather than when the benchmark ends, so no
			// burst is measured beside the idle brokers and appliers of the
			// ones before it.
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

// appendBurst lands n probe records on q's probe log, each on a subject of its
// own, as peers' writes would have — straight onto the log, through no
// publisher, but SIGNED as a peer's publisher signs them, since a wired node's
// applier opens every frame — and answers the position of the last.
func appendBurst(b *testing.B, q *js.Queue, n int) statelog.Position {
	b.Helper()
	spec := ensureWiredLog(b, q)
	log, err := q.DomainLog(b.Context(), spec.Name)
	if err != nil {
		b.Fatalf("open the log: %v", err)
	}
	signer, err := statelog.NewSigner(probeDomain{}.Name(), testRing())
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

// quiet raises the process's log level to Warn while a benchmark runs.
//
// An INFO line the applier writes from its own goroutine was measured landing
// in the middle of a benchmark's result line, which a comparison tool cannot
// read. Warn and above still print, because one of those during a
// measurement is a finding. Put back to the level the logging package starts
// a process at.
func quiet(b *testing.B) {
	logging.SetVerbosity(slog.LevelWarn, logging.FormatConsole)
	b.Cleanup(func() { logging.SetVerbosity(slog.LevelInfo, logging.FormatConsole) })
}

// measure runs b.N operations across workers goroutines and reports them a
// second, under unit, with the p50 and p99 of one operation's latency.
//
// b.N COUNTED BY HAND rather than through b.Loop, for objstoretest's reason:
// fed through b.Loop, a pool's last operations finish after the timer stops.
// So the function is run again for every step of the ramp to its final b.N,
// which is why every node an arm drives is built in its parent.
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
// be elected.
//
// THE CLUSTER'S OWN VIEW, which a benchmark may read and the read path may
// not (TestStreamInfoIsNotOnTheReadPath holds that of the non-test code): a
// leader's report is exactly what an isolated former leader gets wrong, and
// here it only labels which member an arm drives.
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
