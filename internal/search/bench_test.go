package search_test

import (
	"context"
	"database/sql"
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

// ONE BENCHMARK, TWO AXES — corpus size and CONCURRENCY.
//
// # Why the idle number is not the supported one
//
// The corpus this engine declares supported is derived from a per-row scan
// coefficient, and every figure behind that coefficient was taken by a single
// reader on an otherwise idle node. A company's node is not idle: seats are
// taking turns, the applier is committing records, the dashboard is polling.
// A benchmark that only ever measured one reader would let the idle number be
// quoted as the budget, which is the specific mistake this shape exists to
// prevent — so the zero-concurrency cell is a CELL of this benchmark rather
// than a benchmark of its own.
//
// # What it measured, on this container, at 3 072 dimensions
//
// Thirty iterations per cell, four cores, the shipped depths (1 200 candidates,
// 150 returned), p95 in milliseconds:
//
//	readers    n = 10 000              n = 40 000
//	      1     49 ms  (4.96 us/row)   103 ms  (2.58 us/row)
//	      2     62 ms  (6.22)          145 ms  (3.63)
//	      4     75 ms  (7.53)          111 ms  (2.79)
//	      8    125 ms  (12.55)         245 ms  (6.14)
//
// EIGHT CONCURRENT READERS COST 2.4x THE IDLE p95, on four cores, which is the
// whole reason the concurrency axis exists: a supported-corpus figure derived
// from the one-reader coefficient is a figure for a node nobody runs. At the
// 1.0 s interactive target the same two coefficients gave ≈ 390 000 sources
// idle and ≈ 160 000 under eight readers — and the honest number is whichever
// of those describes the deployment.
//
// Those two figures are SUPERSEDED as the published ones, and the table above
// is kept as what this benchmark measured rather than edited to agree:
// BenchmarkSemanticIVFUnderLoad's scan arm re-measured the same statement on
// the topical fixture at 2.90 and 7.34 µs a source (≈ 345 000 and ≈ 136 000),
// and it is the one that also measures the index beside it on the same corpus
// and the same queries — so that is where the published capacity comes from.
//
// # What it reports beyond ns/op
//
// A mean is not a p95, and the interactive budget is written against a p95:
// at twelve probes the chance that all twelve fell below the true p95 is
// 0.95^12 = 0.54, so a mean-reported probe count cannot estimate it at all.
// Every cell here reports its own p95 from at least 400 samples, alongside the
// per-row cost that the corpus projection is actually built from.
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
		if err := storetest.EstateOf(db).Tx(b.Context(), func(tx *sql.Tx) error {
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
		if err := storetest.EstateOf(c.db).Read(ctx, func(tx *sql.Tx) error {
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

// THE SAME TWO AXES FOR THE INDEX — corpus size and concurrency — and the scan
// beside it on the SAME corpus and queries, so the ratio between them is a
// measurement rather than two numbers from two runs.
//
// # Why the topical fixture, and what that choice claims
//
// An inverted file is only worth having where the corpus has structure a
// partition can find, and the isotropic fixture has none: its training
// declines to install an index at all (TestIVFRecallMeetsTheFloorCurve). So
// the corpus here is the fixture family's topical member, at the shipped
// width, written through the applier and indexed exactly as the embedding duty
// indexes it — trained, measured against the exact scan, installed at the
// probe count the training chose, rolled out. Every cell reports the share of
// lists it probed and the recall it actually delivered against the exact f32
// scan on its own queries, so a latency is never quoted without the recall it
// was bought at.
//
// The figures this benchmark produced are in the package doc of semantic.go
// and in ADR-0022; the per-source coefficient is what [SemanticScanBudget]'s
// supported-corpus projection is derived from.
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
					byList := search.GroupByList(index.Assign(c.Codes), lists)
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
		if err := storetest.EstateOf(db).Tx(b.Context(), func(tx *sql.Tx) error {
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
	index := trainedIndex(b, c.db, c.model, search.FixtureWidth)
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
	if err := storetest.EstateOf(c.db).Read(b.Context(), func(tx *sql.Tx) error {
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
	if err := storetest.EstateOf(db).Tx(b.Context(), func(tx *sql.Tx) error {
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
		if err := storetest.EstateOf(c.db).Read(ctx, func(tx *sql.Tx) error {
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
// 0025).
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
			if err := storetest.EstateOf(db).Read(b.Context(), func(tx *sql.Tx) error {
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

// BenchmarkIndexTraining is the embedding duty's TRAINING TICK as the duty runs
// it, on a real store, phase by phase: the reading (every code in key order,
// the sampled documents, and the one exact pass that is their ground truth in
// every shape), the k-means, filing every code, and choosing the probe count.
//
// # What it is for
//
// A tick is bounded by the duty's lease, and a training cut off by that bound
// publishes nothing — so the index a large partition needs is one it only gets
// if its training fits. The reading and the filing grow with the corpus and
// are measured here per source, which is what they are projected from; the
// k-means grows with the LIST COUNT and is measured at scale in memory by
// BenchmarkIVFRecallAtScale's train-s. The projection is in the package doc of
// ivfduty.go and beside [search.IVFMaxLists].
func BenchmarkIndexTraining(b *testing.B) {
	for _, n := range []int{20_000, 40_000} {
		b.Run(fmt.Sprintf("n=%d", n), func(b *testing.B) {
			const model = "bench-embed"
			db, _ := seedCorpus(b, n, model)
			for b.Loop() {
				started := time.Now()
				reading := readingOf(b, db, model, search.FixtureWidth)
				read := time.Since(started)

				docs := make([]search.SampledDoc, 0, search.EvalQueries)
				var shapes [][]search.ShapeQuery
				var sample time.Duration
				if err := storetest.EstateOf(db).Read(b.Context(), func(tx *sql.Tx) error {
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
				byList := search.GroupByList(index.Assign(codes), lists)
				filing := time.Since(started)
				started = time.Now()
				if _, err := search.ChooseProbes(b.Context(), &reading.Corpus, byList,
					index, reading.Trials); err != nil {
					b.Fatal(err)
				}
				choose := time.Since(started)
				perSource := func(d time.Duration) float64 {
					return float64(d.Microseconds()) / float64(n)
				}
				b.ReportMetric(read.Seconds(), "read-s")
				b.ReportMetric(perSource(read), "read-us/source")
				b.ReportMetric(perSource(exact), "exact-us/source")
				b.ReportMetric(perSource(sample), "sample-us/source")
				b.ReportMetric(kmeans.Seconds(), "kmeans-s")
				b.ReportMetric(perSource(filing), "filing-us/source")
				b.ReportMetric(choose.Seconds(), "choose-s")
			}
		})
	}
}
