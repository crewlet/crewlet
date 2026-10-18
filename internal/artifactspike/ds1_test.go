package artifactspike

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/artifactdata"
	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/objectives"
	"github.com/crewlet/crewlet/internal/statelog"
)

// SPIKE DS-1 (and S2): the datastore's write rate, in rows a second, through
// the real publisher and applier.
//
// Every data node applies every record of the log through ONE writer into ONE
// replicated file, so the figure that bounds a company is the slowest node's
// applied rows a second, and adding nodes adds none. The benchmarks report it
// as the runner itself counts it ([metrics.StatelogApplyBatchRows]), beside the
// records a second, the rows one record costs, and each write's round trip as
// the writer saw it (append, quorum, this node's apply, the verdict read back).
//
// Run: go test -run '^$' -bench DS1 -benchtime 1x -timeout 60m ./internal/artifactspike

const (
	// saturationWriters is how many writers keep the log busy. Each waits
	// for its own verdict, so a run is closed-loop: enough writers that
	// the applier, not the writers' round trips, is what the run measures
	// (a few times the records one apply transaction takes in, as the
	// rates below show).
	saturationWriters = 32

	// saturationWindow is how long a saturation run writes for: long
	// enough for the applier's batching to reach its steady state.
	saturationWindow = 8 * time.Second

	// seededRows is how many rows each store holds before a run, so cell
	// edits land on rows that exist and rows are rewritten rather than
	// created.
	seededRows = 1000
)

// BenchmarkDS1WriteRate is the saturation rate: closed-loop writers on one
// store and on ten, in each write shape, on one node and on a three-member
// fleet.
func BenchmarkDS1WriteRate(b *testing.B) {
	for _, topo := range []topology{topoSolo, topoFleet} {
		for _, s := range []shape{shapeCell, shapeRow, shapeBulk} {
			for _, stores := range []int{1, 10} {
				name := fmt.Sprintf("topo=%s/shape=%s/stores=%d", topo, s, stores)
				b.Run(name, func(b *testing.B) {
					for range b.N {
						saturate(b, name, topo, s, stores)
					}
				})
			}
		}
	}
}

// writeTally counts what writes answered.
type writeTally struct {
	verdicts sync.Map // artifacts.DataVerdict -> *atomic.Int64
	pending  atomic.Int64
	limited  atomic.Int64
	failed   atomic.Int64
	firstErr atomic.Value
}

func (w *writeTally) note(res artifactdata.WriteResult, err error) {
	var limited *artifactdata.RateLimited
	switch {
	case errors.As(err, &limited):
		w.limited.Add(1)
	case err != nil:
		w.failed.Add(1)
		w.firstErr.CompareAndSwap(nil, err.Error())
	case res.Verdict == "":
		w.pending.Add(1)
	default:
		v, _ := w.verdicts.LoadOrStore(res.Verdict, new(atomic.Int64))
		v.(*atomic.Int64).Add(1)
	}
}

func (w *writeTally) verdict(v artifacts.DataVerdict) int64 {
	if n, ok := w.verdicts.Load(v); ok {
		return n.(*atomic.Int64).Load()
	}
	return 0
}

// check fails the spike when a write failed outright: a number taken over
// failing writes describes the failure, not the datastore.
func (w *writeTally) check(b *testing.B) {
	b.Helper()
	for _, v := range []artifacts.DataVerdict{artifacts.DataInvalid, artifacts.DataForbidden,
		artifacts.DataQuota, artifacts.DataGone} {
		if n := w.verdict(v); n > 0 {
			b.Fatalf("%d write(s) answered %s: the workload is built to be admitted, so the "+
				"run measured refusals rather than writes", n, v)
		}
	}
	if n := w.failed.Load(); n > 0 {
		b.Fatalf("%d write(s) failed; the first: %v", n, w.firstErr.Load())
	}
	if n := w.limited.Load(); n > 0 {
		b.Fatalf("%d write(s) were refused by the writer's own rate limits, which the "+
			"spike's clock lifts: the run measured the guard, not the applier", n)
	}
}

// fleetRig is a started topology with a data node on every member and
// stores seeded.
type fleetRig struct {
	brokers *brokers
	nodes   []*dataNode
	stores  []artifacts.ID
}

// rig starts topo, joins a data node per member, seeds stores sheets of
// seededRows rows, and waits until every node has applied the seed.
func rig(b *testing.B, run string, topo topology, stores int) *fleetRig {
	b.Helper()
	br := startBrokers(b, topo)
	br.provisionDataLog(b)
	out := &fleetRig{brokers: br, stores: storeIDs(run, stores)}
	for i, q := range br.queues {
		out.nodes = append(out.nodes, joinData(b, q, fmt.Sprintf("node-%d", i)))
	}
	for _, id := range out.stores {
		seedSheet(b, out.nodes[0], id, seededRows)
	}
	applied(b, out.nodes)
	return out
}

// saturate is one saturation run.
func saturate(b *testing.B, run string, topo topology, s shape, stores int) {
	b.Helper()
	b.StopTimer()
	r := rig(b, run, topo, stores)
	rowsBefore := make([]float64, len(r.nodes))
	recordsBefore := make([]uint64, len(r.nodes))
	for i, n := range r.nodes {
		rowsBefore[i], recordsBefore[i] = n.appliedRows(), n.appliedRecords()
	}
	var (
		lat   latencies
		tally writeTally
		wg    sync.WaitGroup
	)
	cpuBefore := cpuTime(b)
	b.StartTimer()
	start := time.Now()
	stop := start.Add(saturationWindow)
	for w := range saturationWriters {
		wg.Go(func() {
			node := r.nodes[w%len(r.nodes)]
			id := r.stores[w%len(r.stores)]
			call := editor(fmt.Sprintf("member-%02d", w%len(spikeSeats)))
			rnd := rand.New(rand.NewPCG(uint64(w), 7))
			for time.Now().Before(stop) {
				ops := opsOf(rnd, s, seededRows)
				t0 := time.Now()
				res, err := node.writer.Write(b.Context(), call, id, artifacts.DataWrite{Ops: ops})
				lat.add(time.Since(t0))
				tally.note(res, err)
			}
		})
	}
	wg.Wait()
	acked := time.Since(start)
	done := applied(b, r.nodes)
	b.StopTimer()
	cpu := cpuTime(b) - cpuBefore
	tally.check(b)

	// THE SLOWEST NODE BOUNDS THE FLEET: every node applies every record,
	// and a read at any level waits on the node it is served from.
	minRows, minRecords := -1.0, -1.0
	var rowsTotal float64
	var recordsTotal uint64
	for i, n := range r.nodes {
		elapsed := done[i].Sub(start).Seconds()
		rows := n.appliedRows() - rowsBefore[i]
		records := n.appliedRecords() - recordsBefore[i]
		rowsTotal += rows
		recordsTotal += records
		if rate := rows / elapsed; minRows < 0 || rate < minRows {
			minRows = rate
		}
		if rate := float64(records) / elapsed; minRecords < 0 || rate < minRecords {
			minRecords = rate
		}
	}
	b.ReportMetric(minRows, "rows/s")
	b.ReportMetric(minRecords, "records/s")
	if recordsTotal > 0 {
		b.ReportMetric(rowsTotal/float64(recordsTotal), "rows/record")
		b.ReportMetric(ms(cpu)/float64(recordsTotal)*float64(len(r.nodes)), "cpu-ms/record")
	}
	b.ReportMetric(float64(lat.count())/acked.Seconds(), "writes/s")
	b.ReportMetric(float64(tally.pending.Load()), "pending")
	lat.report(b, "rtt")
}

// reviewEditors, reviewInterval and reviewHotShare are the review's peak
// (plan 04 section 9.1): fifty editors at half an op a second each, a fifth of
// the edits on one hot row.
const (
	reviewEditors  = 50
	reviewInterval = 2 * time.Second
	reviewHotShare = 0.2
	reviewWindow   = 20 * time.Second
)

// BenchmarkDS1Review is the review at its peak on the smallest fleet,
// open-loop: each editor edits a cell every two seconds on average whatever
// the last one took, so a slow round trip shows as latency rather than as a
// lower rate.
func BenchmarkDS1Review(b *testing.B) {
	for range b.N {
		b.StopTimer()
		r := rig(b, "review", topoFleet, 1)
		id := r.stores[0]
		var (
			lat   latencies
			tally writeTally
			wg    sync.WaitGroup
		)
		b.StartTimer()
		stop := time.Now().Add(reviewWindow)
		for e := range reviewEditors {
			wg.Go(func() {
				node := r.nodes[e%len(r.nodes)]
				call := editor(fmt.Sprintf("member-%02d", e))
				rnd := rand.New(rand.NewPCG(uint64(e), 11))
				var inner sync.WaitGroup
				defer inner.Wait()
				for {
					// UNIFORM ON (0, 2×interval): a mean of the
					// interval, and editors that drift apart rather
					// than click in lockstep.
					next := time.Duration(rnd.Int64N(int64(2 * reviewInterval)))
					if time.Now().Add(next).After(stop) {
						return
					}
					time.Sleep(next)
					ops := opsOf(rnd, shapeCell, seededRows)
					if rnd.Float64() < reviewHotShare {
						ops[0].Doc = docID(0)
					}
					inner.Go(func() {
						t0 := time.Now()
						res, err := node.writer.Write(b.Context(), call, id, artifacts.DataWrite{Ops: ops})
						lat.add(time.Since(t0))
						tally.note(res, err)
					})
				}
			})
		}
		wg.Wait()
		b.StopTimer()
		tally.check(b)
		applied(b, r.nodes)
		lat.report(b, "rtt")
		b.ReportMetric(float64(lat.count())/reviewWindow.Seconds(), "writes/s")
		b.ReportMetric(withinShare(&lat, objectives.DataRoundTripWithin), "within-objective")
		b.ReportMetric(float64(tally.pending.Load()), "pending")
	}
}

// withinShare is the share of samples at or under bound.
func withinShare(l *latencies, bound time.Duration) float64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.samples) == 0 {
		return 0
	}
	within := 0
	for _, d := range l.samples {
		if d <= bound {
			within++
		}
	}
	return float64(within) / float64(len(l.samples))
}

// contention's shape: thirty writers on ten rows of one store, half guarded
// by the version they read and half blind.
const (
	contentionWriters = 30
	contentionRows    = 10
	contentionWindow  = 10 * time.Second
)

// BenchmarkDS1Contention is thirty concurrent writers on one store's hottest
// rows: the guarded half read a row and write it back conditioned on the
// version read, the blind half write without a condition. It reports each
// half's verdicts and round trips, and fails if a blind write ever conflicts,
// which the datastore's ordering forbids (the log orders a write with no
// condition and the applier decides it, internal/artifactdata).
func BenchmarkDS1Contention(b *testing.B) {
	for range b.N {
		b.StopTimer()
		r := rig(b, "contention", topoFleet, 1)
		id := r.stores[0]
		var (
			guardedLat, blindLat     latencies
			guardedTally, blindTally writeTally
			wg                       sync.WaitGroup
		)
		b.StartTimer()
		stop := time.Now().Add(contentionWindow)
		for w := range contentionWriters {
			guarded := w < contentionWriters/2
			wg.Go(func() {
				node := r.nodes[w%len(r.nodes)]
				call := editor(fmt.Sprintf("member-%02d", w))
				reading := artifactdata.Reading{Record: "session:" + call.Authority.Actor.Name,
					Role: artifacts.RoleEditor, Fresh: statelog.Freshness{Level: statelog.ReadSession}}
				rnd := rand.New(rand.NewPCG(uint64(w), 13))
				for time.Now().Before(stop) {
					ops := opsOf(rnd, shapeCell, contentionRows)
					t0 := time.Now()
					if guarded {
						doc, err := node.reader.Doc(b.Context(), id, reading, sheetCollection, ops[0].Doc)
						if err != nil {
							guardedTally.note(artifactdata.WriteResult{}, err)
							continue
						}
						v := doc.Version
						ops[0].IfVersion = &v
					}
					res, err := node.writer.Write(b.Context(), call, id, artifacts.DataWrite{Ops: ops})
					if guarded {
						guardedLat.add(time.Since(t0))
						guardedTally.note(res, err)
					} else {
						blindLat.add(time.Since(t0))
						blindTally.note(res, err)
					}
				}
			})
		}
		wg.Wait()
		b.StopTimer()
		guardedTally.check(b)
		blindTally.check(b)
		applied(b, r.nodes)
		if n := blindTally.verdict(artifacts.DataConflict); n > 0 {
			b.Errorf("%d blind write(s) answered conflict: a write with no condition is "+
				"ordered by the log and never conflicts", n)
		}
		guardedLat.report(b, "guarded-rtt")
		blindLat.report(b, "blind-rtt")
		b.ReportMetric(float64(guardedTally.verdict(artifacts.DataApplied)), "guarded-applied")
		b.ReportMetric(float64(guardedTally.verdict(artifacts.DataConflict)), "guarded-conflict")
		b.ReportMetric(float64(blindTally.verdict(artifacts.DataApplied)), "blind-applied")
		b.ReportMetric(float64(blindTally.verdict(artifacts.DataConflict)), "blind-conflict")
	}
}
