package search_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/rand/v2"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/search"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
)

// ONE BENCHMARK, TWO AXES: corpus size and CONCURRENCY.
//
// The supported corpus is derived from a per-row scan coefficient, and a
// single reader on an idle node gives the cheapest one. A real node has seats
// taking turns, the applier committing and the dashboard polling, so the idle
// number must not be quoted as the budget: the one-reader cell is a CELL of
// this benchmark, not a benchmark of its own. Eight readers on four cores cost
// 2.4x the idle p95 at 3 072 dimensions. The published capacity comes from
// BenchmarkSemanticIVFUnderLoad's scan arm, which also measures the index.
//
// Each cell reports its own p95 from at least 400 samples beside the per-row
// cost, because the interactive budget is a p95 and a mean cannot estimate it.
func BenchmarkSemanticScanUnderLoad(b *testing.B) {
	for _, n := range []int{10_000, 40_000} {
		corpus := newScanCorpus(b, n)
		for _, readers := range []int{1, 2, 4, 8} {
			b.Run(fmt.Sprintf("n=%d/readers=%d", n, readers), func(b *testing.B) {
				corpus.run(b, readers)
			})
		}
	}
}

// BenchmarkSemanticScan is the ZERO-CONCURRENCY CELL, kept under its own name
// because that is what every published figure in this design was taken at —
// and running it alone is how a number gets quoted without its axis.
func BenchmarkSemanticScan(b *testing.B) {
	for _, n := range []int{10_000, 40_000} {
		corpus := newScanCorpus(b, n)
		b.Run(fmt.Sprint(n), func(b *testing.B) { corpus.run(b, 1) })
	}
}

// BenchmarkBinaryRecallAtScale is the HALF-MILLION RECALL ARM, and it is a
// benchmark rather than a gate for one measured reason.
//
// The per-pull-request gate runs at twenty thousand and a hundred and twenty
// thousand, which costs 96 s under the race detector — generation is the whole
// of it. Half a million alone is several times that, and a cost every
// contributor pays on every run is the wrong place for the largest arm. It
// still ASSERTS its floor: a benchmark that only reported a number would be a
// number nobody reads.
//
// It holds only the 1-bit codes, which is what makes it possible at all: as
// f32 the corpus would be 500 000 x 3 072 x 4 B = 6.14 GB.
func BenchmarkBinaryRecallAtScale(b *testing.B) {
	const n = 500_000
	f := search.NewFixture(n, 1)
	for b.Loop() {
		recall, head := 0.0, 0
		const queries = 10
		for q := range queries {
			code, similarity := f.Query(uint64(2000 + q))
			want := search.Exact(f.Len(), similarity, search.ReturnDepth)
			got := search.TwoStage(f.Codes, code, similarity,
				search.Stage1Depth, search.ReturnDepth)
			recall += search.Recall(got, want)
			for _, rank := range search.MissRanks(got, want) {
				if rank < 10 {
					head++
				}
			}
		}
		recall /= queries
		b.ReportMetric(recall, "recall")
		b.ReportMetric(float64(head), "head-misses")
		if floor := search.FloorAt(n); recall < floor {
			b.Fatalf("recall@%d from a stage-1 depth of %d is %.4f at %d "+
				"sources, below the %.4f floor",
				search.ReturnDepth, search.Stage1Depth, recall, n, floor)
		}
	}
}

// scanCorpus is one store with a corpus in it, built once per size and shared
// by every concurrency cell — generation is what the wall clock would
// otherwise be.
type scanCorpus struct {
	db    *store.DB
	dim   int
	model string
	n     int
}

const benchDim = 3072

func newScanCorpus(b *testing.B, n int) *scanCorpus {
	b.Helper()
	db, _ := storetest.OpenEstate(b, filepath.Join(b.TempDir(), "node.db"), store.Options{}, 1)
	b.Cleanup(func() { _ = db.Close() })

	c := &scanCorpus{db: db, dim: benchDim, model: "bench-embed", n: n}
	applier := search.NewApplier()
	rng := rand.New(rand.NewPCG(7, uint64(n)))
	// IN CHUNKS, because one transaction holding half a gigabyte of rows
	// is a measurement of the write path rather than a corpus.
	const chunk = 2_000
	for start := 0; start < n; start += chunk {
		if err := db.Replicated().Tx(b.Context(), func(tx *sql.Tx) error {
			for i := start; i < min(start+chunk, n); i++ {
				subject := search.Subject{
					Source: search.SourcePage, ID: fmt.Sprintf("p%07d", i),
				}
				rec := search.VectorRecord{
					RecordEnvelope: search.RecordEnvelope{
						Subject: subject, Op: search.OpEmbed, Gen: 1,
						CreatedAt: time.Unix(1_700_000_000, 0).UTC(),
						Scope: statelog.ScopeSet{
							Paths: []string{search.ScopePath("ENG", subject)},
						},
					},
					Container: "ENG", Model: c.model, Dim: c.dim,
					Embedding: randomEmbedding(rng, c.dim),
				}
				payload, err := rec.Encode()
				if err != nil {
					return err
				}
				if _, err := applier.Apply(b.Context(), tx, statelog.Record{
					Position: statelog.Position{
						Stream: "S", Generation: 1, Seq: uint64(i) + 1,
					},
					Payload: payload,
				}, statelog.ApplyOptions{}); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			b.Fatalf("seed: %v", err)
		}
	}
	return c
}

// run measures one cell: readers concurrent searches, reporting the p95 and
// the per-row cost the corpus projection is built from.
func (c *scanCorpus) run(b *testing.B, readers int) {
	b.Helper()
	rng := rand.New(rand.NewPCG(11, uint64(c.n)))
	queries := make([][]byte, 64)
	for i := range queries {
		queries[i] = randomEmbedding(rng, c.dim)
	}

	var mu sync.Mutex
	var samples []time.Duration
	var next atomic.Uint64

	one := func(ctx context.Context) {
		query := queries[next.Add(1)%uint64(len(queries))]
		started := time.Now()
		if err := c.db.Replicated().Read(ctx, func(tx *sql.Tx) error {
			_, _, err := search.Semantic(ctx, tx, search.SemanticQuery{
				Vector: query, Model: c.model, Dim: c.dim,
			})
			return err
		}); err != nil {
			b.Error(err)
			return
		}
		elapsed := time.Since(started)
		mu.Lock()
		samples = append(samples, elapsed)
		mu.Unlock()
	}

	b.ResetTimer()
	for b.Loop() {
		if readers == 1 {
			one(b.Context())
			continue
		}
		var wg sync.WaitGroup
		for range readers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				one(b.Context())
			}()
		}
		wg.Wait()
	}
	b.StopTimer()

	mu.Lock()
	defer mu.Unlock()
	if len(samples) == 0 {
		return
	}
	slices.Sort(samples)
	// THE NONPARAMETRIC ONE-SIDED MINIMUM for a p95 is 59 samples —
	// ln(0.05)/ln(0.95) — and a cell below it reports the count so the
	// number is read as what it is.
	p95 := samples[min(int(float64(len(samples))*0.95), len(samples)-1)]
	b.ReportMetric(float64(p95.Milliseconds()), "p95-ms")
	b.ReportMetric(float64(p95.Nanoseconds())/float64(c.n)/1000, "p95-us/row")
	b.ReportMetric(float64(len(samples)), "samples")
}

// THE SAME TWO AXES FOR THE INDEX, and the scan beside it on the SAME corpus
// and queries, so their ratio is one measurement, not two runs.
//
// The corpus is the fixture family's TOPICAL member: an inverted file needs
// structure a k-means can find, and the isotropic fixture has none (its
// training installs no index: TestIVFRecallMeetsTheFloorCurve). It is written
// through the applier and indexed as the embedding duty does. Every cell
// reports the share of lists probed and the recall delivered against the exact
// f32 scan, so a latency is never quoted without its recall.
//
// The figures are in semantic.go's package doc and ADR-0028; the per-source
// coefficient is what [SemanticScanBudget]'s projection is derived from.
func BenchmarkSemanticIVFUnderLoad(b *testing.B) {
	for _, n := range []int{10_000, 40_000} {
		corpus := newIndexedCorpus(b, n)
		for _, scan := range []bool{true, false} {
			method := "ivf"
			if scan {
				method = "scan"
			}
			for _, readers := range []int{1, 2, 4, 8} {
				b.Run(fmt.Sprintf("n=%d/%s/readers=%d", n, method, readers), func(b *testing.B) {
					corpus.run(b, readers, scan)
				})
			}
		}
	}
}

// BenchmarkIVFRecallAtScale is the LARGER ARMS of TestIVFRecallMeetsTheFloorCurve,
// for the reason BenchmarkBinaryRecallAtScale is a benchmark: training at a
// hundred and twenty thousand sources under the race detector is minutes on
// every contributor's run. It still ASSERTS the floor in every shape, on
// queries the training never chose its probe count on, and reports the probe
// share the training chose — the number the index's whole cost model rests on
// — and how long the k-means took, which is the term of a training tick that
// grows with the list count ([search.IVFMaxLists]).
func BenchmarkIVFRecallAtScale(b *testing.B) {
	for _, n := range []int{120_000, 500_000} {
		for name, build := range map[string]func(int, uint64) *search.Fixture{
			"isotropic": search.NewFixture, "topical": search.NewTopicalFixture,
		} {
			b.Run(fmt.Sprintf("n=%d/%s", n, name), func(b *testing.B) {
				f := build(n, 1)
				c := gateCorpus(f)
				for b.Loop() {
					lists := search.IVFLists(n)
					started := time.Now()
					index, err := search.TrainIVF(b.Context(), c.Codes, lists,
						search.IVFSeed("bench", 0), nil)
					if err != nil {
						b.Fatal(err)
					}
					trained := time.Since(started)
					byList := search.GroupByList(filedIn(b, index, c.Codes), lists)
					choice, err := search.ChooseProbes(b.Context(), c, byList, index,
						gateTrials(f, c, 5000, search.EvalQueries))
					if err != nil {
						b.Fatal(err)
					}
					results := map[search.ShapeFilter]*shapeTally{}
					for _, trial := range gateTrials(f, c, 2000, 10) {
						pool, lists := searchAsRun(c, byList, index, trial, choice.Probes)
						key := search.ShapeFilter{Shape: trial.Filter.Shape,
							Source: trial.Filter.Source}
						if results[key] == nil {
							results[key] = &shapeTally{}
						}
						results[key].add(pool, trial)
						if lists == 0 {
							results[key].scanned++
						}
					}
					all := results[search.ShapeFilter{Shape: search.ShapeAll}]
					b.ReportMetric(all.recall(), "recall")
					b.ReportMetric(float64(all.head), "head-misses")
					b.ReportMetric(float64(choice.Probes)/float64(lists), "probe-share")
					b.ReportMetric(trained.Seconds(), "train-s")
					for key, tally := range results {
						if choice.Passed() && (tally.recall() < tally.floor() || tally.head != 0) {
							b.Fatalf("an index probing %d of %d lists recalls %.4f "+
								"with %d head miss(es) in the %s%s shape against a "+
								"%.4f floor", choice.Probes, lists, tally.recall(),
								tally.head, key.Shape, sourceSuffix(key.Source),
								tally.floor())
						}
						b.Logf("%s%s: recall %.4f (floor %.4f), %d head misses, "+
							"%d of %d scanned", key.Shape, sourceSuffix(key.Source),
							tally.recall(), tally.floor(), tally.head, tally.scanned,
							tally.trials)
					}
				}
			})
		}
	}
}

// indexedCorpus is one store holding the topical fixture at the shipped width,
// with its index installed as the duty installs it.
type indexedCorpus struct {
	db      *store.DB
	model   string
	n       int
	queries [][]byte
	// wants are each query's exact top ReturnDepth, as keys.
	wants [][]string
	lists int
	// probes is the training's own choice, and installed whether the duty
	// would have installed it (the benchmark installs it either way, so the
	// probe's cost is measured even where the training declines it).
	probes    int
	installed bool
}

// seedCorpus writes n documents of the topical fixture into a fresh store
// through the applier: pages, filed in the ENG container.
func seedCorpus(b *testing.B, n int, model string) (*store.DB, *search.Fixture) {
	b.Helper()
	db, _ := storetest.OpenEstate(b, filepath.Join(b.TempDir(), "node.db"), store.Options{}, 1)
	b.Cleanup(func() { _ = db.Close() })
	f := search.NewTopicalFixture(n, 1)
	applier := search.NewApplier()
	const chunk = 2_000
	for start := 0; start < n; start += chunk {
		if err := db.Replicated().Tx(b.Context(), func(tx *sql.Tx) error {
			for i := start; i < min(start+chunk, n); i++ {
				rec := embedRecord(search.SourcePage, fmt.Sprintf("p%07d", i),
					model, pack(f.Vector(i)))
				payload, err := rec.Encode()
				if err != nil {
					return err
				}
				if _, err := applier.Apply(b.Context(), tx, statelog.Record{
					Position: statelog.Position{Stream: "S", Generation: 1, Seq: uint64(i) + 1},
					Payload:  payload,
				}, statelog.ApplyOptions{}); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			b.Fatalf("seed: %v", err)
		}
	}
	return db, f
}

func newIndexedCorpus(b *testing.B, n int) *indexedCorpus {
	b.Helper()
	c := &indexedCorpus{model: "bench-embed", n: n}
	var f *search.Fixture
	c.db, f = seedCorpus(b, n, c.model)

	// THE DUTY'S OWN TRAINING, on the store's own rows and held-out
	// documents, then installed and rolled out through the applier.
	index := trainedIndex(b, c.db, c.model, search.FixtureWidth, search.IVFReassignBatch)
	c.lists, c.probes = index.Index.Lists, index.Index.Probes
	c.installed = index.Index.Measurement.Passed() &&
		c.probes*search.IVFProbeCeiling <= c.lists
	seq := uint64(n) + 1
	applyRecordAt(b, c.db, index, seq)
	generation := statelog.Position{Stream: "S", Generation: 1, Seq: seq}.Packed()
	for batch := range index.Index.Rollout {
		seq++
		applyRecordAt(b, c.db, reassignRecord(generation, batch,
			len(index.Index.Rollout)), seq)
	}

	// THE QUERIES AND THEIR EXACT ANSWERS, so every cell reports the recall
	// its latency bought.
	var docs []search.SampledDoc
	var shapes [][]search.ShapeQuery
	for q := range 64 {
		vector := pack(f.QueryVector(uint64(3000 + q)))
		c.queries = append(c.queries, vector)
		docs = append(docs, search.SampledDoc{Vector: vector})
		shapes = append(shapes, []search.ShapeQuery{{Shape: search.ShapeAll}})
	}
	if err := c.db.Replicated().Read(b.Context(), func(tx *sql.Tx) error {
		tops, err := search.ExactTops(b.Context(), tx, docs, shapes, c.model,
			search.FixtureWidth, search.ReturnDepth)
		for _, top := range tops {
			c.wants = append(c.wants, top[0].Keys)
		}
		return err
	}); err != nil {
		b.Fatal(err)
	}
	return c
}

func applyRecordAt(b *testing.B, db *store.DB, rec search.VectorRecord, seq uint64) {
	b.Helper()
	payload, err := rec.Encode()
	if err != nil {
		b.Fatal(err)
	}
	if err := db.Replicated().Tx(b.Context(), func(tx *sql.Tx) error {
		_, err := search.NewApplier().Apply(b.Context(), tx, statelog.Record{
			Position: statelog.Position{Stream: "S", Generation: 1, Seq: seq},
			Payload:  payload,
		}, statelog.ApplyOptions{})
		return err
	}); err != nil {
		b.Fatal(err)
	}
}

// run measures one cell: readers concurrent searches through the index or the
// full scan, reporting the p95, the per-source coefficient, the probe share and
// the recall delivered.
func (c *indexedCorpus) run(b *testing.B, readers int, scan bool) {
	b.Helper()
	var mu sync.Mutex
	var samples []time.Duration
	var recall float64
	var measured int
	var next atomic.Uint64

	one := func(ctx context.Context) {
		i := next.Add(1) % uint64(len(c.queries))
		started := time.Now()
		var hits []search.SemanticHit
		if err := c.db.Replicated().Read(ctx, func(tx *sql.Tx) error {
			var err error
			hits, _, err = search.Semantic(ctx, tx, search.SemanticQuery{
				Vector: c.queries[i], Model: c.model, Dim: search.FixtureWidth,
				FullScan: scan,
			})
			return err
		}); err != nil {
			b.Error(err)
			return
		}
		elapsed := time.Since(started)
		got := map[string]bool{}
		for _, h := range hits {
			got[search.Key(h.Source, h.ID)] = true
		}
		found := 0
		for _, key := range c.wants[i] {
			if got[key] {
				found++
			}
		}
		mu.Lock()
		samples = append(samples, elapsed)
		if len(c.wants[i]) > 0 {
			recall += float64(found) / float64(len(c.wants[i]))
			measured++
		}
		mu.Unlock()
	}

	b.ResetTimer()
	for b.Loop() {
		if readers == 1 {
			one(b.Context())
			continue
		}
		var wg sync.WaitGroup
		for range readers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				one(b.Context())
			}()
		}
		wg.Wait()
	}
	b.StopTimer()

	mu.Lock()
	defer mu.Unlock()
	if len(samples) == 0 {
		return
	}
	slices.Sort(samples)
	p95 := samples[min(int(float64(len(samples))*0.95), len(samples)-1)]
	b.ReportMetric(float64(p95.Microseconds())/1000, "p95-ms")
	b.ReportMetric(float64(p95.Nanoseconds())/float64(c.n)/1000, "p95-us/source")
	b.ReportMetric(float64(len(samples)), "samples")
	if measured > 0 {
		b.ReportMetric(recall/float64(measured), "recall")
	}
	if !scan {
		b.ReportMetric(float64(c.probes)/float64(c.lists), "probe-share")
		b.ReportMetric(map[bool]float64{true: 1, false: 0}[c.installed], "installed")
	}
}

// BenchmarkIndexHeadRead is what every embed apply and every search pays to
// learn which index is installed, at the widest index this build trains
// ([search.IVFMaxLists] lists at the shipped width).
//
// Two reads of one store: the head as it is stored — narrow, the centroids in
// a table of their own — and the same head WITH the centroids, which is what
// a head row carrying the blob cost on every read, because a row is read
// whole whatever columns are selected. The difference, times the embeds a
// node applies catching up, is why the blob is not in the head (migration
// 0033).
func BenchmarkIndexHeadRead(b *testing.B) {
	db, _ := storetest.OpenEstate(b, filepath.Join(b.TempDir(), "node.db"), store.Options{}, 1)
	b.Cleanup(func() { _ = db.Close() })
	record := indexRecordOver("bench-embed", search.FixtureWidth)
	lists := search.IVFMaxLists
	record.Index.Lists, record.Index.Probes = lists, lists/2
	record.Index.Centroids = make([]byte, 8*search.CodeWords(search.FixtureWidth)*lists)
	rng := rand.New(rand.NewPCG(1, 1))
	for i := range record.Index.Centroids {
		record.Index.Centroids[i] = byte(rng.Uint32())
	}
	applyRecordAt(b, db, record, 1)
	for _, c := range []struct {
		name string
		read func(*sql.Tx) error
	}{
		{"head", func(tx *sql.Tx) error {
			_, _, err := search.ReadIndex(b.Context(), tx)
			return err
		}},
		{"head-with-centroids", func(tx *sql.Tx) error {
			if _, _, err := search.ReadIndex(b.Context(), tx); err != nil {
				return err
			}
			var blob []byte
			return tx.QueryRowContext(b.Context(),
				`SELECT centroids FROM kb_ivf_centroids WHERE id = 1`).Scan(&blob)
		}},
	} {
		// INSIDE ONE TRANSACTION, as an apply batch reads it: the cost is
		// the read, not a transaction's begin and end.
		b.Run(c.name, func(b *testing.B) {
			if err := db.Replicated().Read(b.Context(), func(tx *sql.Tx) error {
				for b.Loop() {
					if err := c.read(tx); err != nil {
						return err
					}
				}
				return nil
			}); err != nil {
				b.Fatal(err)
			}
		})
	}
}

// BenchmarkReassignBatch is one batch of a rollout as every holder applies it:
// [search.IVFReassignBatch] rows of the shipped width re-filed under an index
// installed over them, in the one transaction the framework commits it in —
// rolled back after each iteration, so every iteration re-files the same
// rows. A rollout of the largest corpus an index serves is about 545 of these
// on every node, and so is a node replaying one.
func BenchmarkReassignBatch(b *testing.B) {
	const n, model = 5_000, "bench-embed"
	db, _ := seedCorpus(b, n, model)
	index := trainedIndex(b, db, model, search.FixtureWidth, search.IVFReassignBatch)
	applyRecordAt(b, db, index, n+1)
	generation := statelog.Position{Stream: "S", Generation: 1, Seq: n + 1}.Packed()
	payload, err := reassignRecord(generation, 0, len(index.Index.Rollout)).Encode()
	if err != nil {
		b.Fatal(err)
	}
	record := statelog.Record{
		Position: statelog.Position{Stream: "S", Generation: 1, Seq: n + 2},
		Payload:  payload,
	}
	errUndone := errors.New("undone so the next iteration re-files the same rows")
	applier := search.NewApplier()
	refiled := 0
	for b.Loop() {
		if err := db.Replicated().Tx(b.Context(), func(tx *sql.Tx) error {
			moved, err := applier.Apply(b.Context(), tx, record, statelog.ApplyOptions{})
			if err != nil {
				return err
			}
			refiled = moved
			return errUndone
		}); !errors.Is(err, errUndone) {
			b.Fatal(err)
		}
	}
	if refiled == 0 {
		b.Fatal("the batch re-filed nothing, so this measured nothing")
	}
	b.ReportMetric(float64(refiled), "rows/op")
}

// BenchmarkIndexTraining is the embedding duty's TRAINING TICK as the duty runs
// it, on a real store. The tick bounds the two halves differently
// ([search.Budget]): the READING shows progress every
// [search.ProgressStride] rows, the ARITHMETIC cannot
// wedge and runs exempt, so they are reported apart (reading-us/source,
// arithmetic-s). The k-means grows with the list count, not the corpus; see
// BenchmarkIVFTrainingShare. reading-us/source is [search.ReadTrainingSet]
// timed ONCE, whole; its parts are timed by re-running the sample and exact
// pass alone, and the codes are the remainder. Each size runs idle and with two
// searchers ([search.SearchWhile]); pin the searched arm with `taskset -c 0`
// too, since the store's native threads escape GOMAXPROCS.
func BenchmarkIndexTraining(b *testing.B) {
	for _, n := range []int{20_000, 40_000} {
		for _, searched := range []bool{false, true} {
			arm := "idle"
			if searched {
				arm = "searched"
			}
			b.Run(fmt.Sprintf("n=%d/%s", n, arm), func(b *testing.B) {
				const model = "bench-embed"
				db, f := seedCorpus(b, n, model)
				scanned := search.NewCodes(len(f.Codes[0]), f.Len())
				for _, code := range f.Codes {
					scanned.Append(code)
				}
				for b.Loop() {
					stop := make(chan struct{})
					latencies := func() []time.Duration { return nil }
					if searched {
						latencies = search.SearchWhile(scanned, stop)
					}
					started := time.Now()
					reading := readingOf(b, db, model, search.FixtureWidth)
					whole := time.Since(started)

					docs := make([]search.SampledDoc, 0, search.EvalQueries)
					var shapes [][]search.ShapeQuery
					var sample time.Duration
					if err := db.Replicated().Read(b.Context(), func(tx *sql.Tx) error {
						var err error
						started = time.Now()
						docs, err = search.SampleDocuments(b.Context(), tx, model,
							search.FixtureWidth, search.EvalQueries)
						if err != nil {
							return err
						}
						sample = time.Since(started)
						for _, doc := range docs {
							shapes = append(shapes, search.ShapesFor(doc, search.Sources))
						}
						started = time.Now()
						_, err = search.ExactTops(b.Context(), tx, docs, shapes, model,
							search.FixtureWidth, search.ReturnDepth)
						return err
					}); err != nil {
						b.Fatal(err)
					}
					exact := time.Since(started)

					codes := reading.Corpus.Codes
					lists := search.IVFLists(n)
					started = time.Now()
					index, err := search.TrainIVF(b.Context(), codes, lists,
						search.IVFSeed("S", 0), reading.HeldOut)
					if err != nil {
						b.Fatal(err)
					}
					kmeans := time.Since(started)
					started = time.Now()
					byList := search.GroupByList(filedIn(b, index, codes), lists)
					filing := time.Since(started)
					started = time.Now()
					if _, err := search.ChooseProbes(b.Context(), &reading.Corpus, byList,
						index, reading.Trials); err != nil {
						b.Fatal(err)
					}
					choose := time.Since(started)
					close(stop)
					perSource := func(d time.Duration) float64 {
						return float64(d.Microseconds()) / float64(n)
					}
					// THE STREAMING HALF — once, whole — and its parts, and
					// the exempt half.
					codesRead := max(whole-sample-exact, 0)
					arithmetic := kmeans + filing + choose
					b.ReportMetric(whole.Seconds(), "reading-s")
					b.ReportMetric(perSource(whole), "reading-us/source")
					b.ReportMetric(perSource(codesRead), "codes-us/source")
					b.ReportMetric(perSource(exact), "exact-us/source")
					b.ReportMetric(perSource(sample), "sample-us/source")
					b.ReportMetric(kmeans.Seconds(), "kmeans-s")
					b.ReportMetric(perSource(filing), "filing-us/source")
					b.ReportMetric(choose.Seconds(), "choose-s")
					b.ReportMetric(arithmetic.Seconds(), "arithmetic-s")
					if searched {
						b.ReportMetric(search.P95ms(latencies()), "scan-p95-ms")
					}
				}
			})
		}
	}
}
