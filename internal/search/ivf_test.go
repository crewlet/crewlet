package search_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"fmt"
	"math"
	"math/bits"
	"math/rand/v2"
	"slices"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/search"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
)

// THE INDEX MEETS THE FLOOR CURVE ON EVERY MEMBER OF THE FIXTURE FAMILY, IN
// EVERY SHAPE A SEARCH IS ISSUED IN — at the probe count its own training
// chose, on queries the training never saw.
//
// # What this holds, and why on queries the training did not choose on
//
// A training measures recall on held-out documents and installs the smallest
// probe count that meets the floor with no head miss ([search.ChooseProbes]).
// That is a claim about the index's recall on EVERY query, made from
// twenty-five — so the gate measures the installed count on the fixture's own
// gate queries, which the training never ranked a list for. A training that
// chose its probes on the answers it was graded on would pass its own
// measurement and fail here.
//
// # In every shape, against that shape's own exact answer
//
// Every search the engine issues is narrowed — to a source, and often to the
// containers a knowledge scope names — and a narrowed search's true answer is
// spread over lists an unfiltered probe never reads. So each gate query is
// measured unfiltered, narrowed to the tenth of the corpus that is pages, to
// the rest that is tasks, and to the container its nearest document is filed
// in (a sixty-fourth of the corpus, clustered in the space as a company's
// projects are), each against the EXACT top of the rows that shape searches
// and the floor for that many rows — through the rule a search runs
// ([search.ProbeCount]): more lists until it has seen enough of its own rows,
// or the full scan.
//
// # Both members, because each one alone certifies the wrong thing
//
// The isotropic member has no structure a partition can find, and meeting the
// floor there takes about every list — the index is then not worth installing,
// and the gate asserts that the training SAYS so rather than installing an
// index that reads the whole table through a second structure. The topical
// member is a company's corpus: meeting the floor there must take no more
// than [search.IVFProbeCeiling] of the lists, or the index is bookkeeping for
// nothing on exactly the corpora it exists for.
//
// Twenty thousand sources, the smaller gate size, because training is the
// cost: the hundred-and-twenty-thousand arm is BenchmarkIVFRecallAtScale, for
// the reason the half-million recall arm is a benchmark.
func TestIVFRecallMeetsTheFloorCurve(t *testing.T) {
	t.Parallel()
	for _, member := range []struct {
		name    string
		fixture func() *search.Fixture
		// installs is whether this member's corpus should get an index.
		installs bool
	}{
		{"isotropic", func() *search.Fixture { return gateFixture(20_000) }, false},
		{"topical", topicalGateFixture, true},
	} {
		t.Run(member.name, func(t *testing.T) {
			t.Parallel()
			f := member.fixture()
			c := gateCorpus(f)
			lists := search.IVFLists(f.Len())
			index, err := search.TrainIVF(t.Context(), c.Codes, lists,
				search.IVFSeed("gate", 0), nil)
			if err != nil {
				t.Fatal(err)
			}
			byList := search.GroupByList(filedIn(t, index, c.Codes), lists)

			// THE TRAINING'S OWN HELD-OUT QUERIES, drawn from a seed range
			// the gate's never uses.
			choice, err := search.ChooseProbes(t.Context(), c, byList, index,
				gateTrials(f, c, 5000, search.EvalQueries))
			if err != nil {
				t.Fatal(err)
			}
			if !choice.Passed() {
				t.Fatalf("no probe count up to every list met every shape's "+
					"floor on the training's own queries (%+v at %d lists) — at "+
					"every list every shape is the full scan, so the fixture "+
					"itself is below its floor", choice.Shapes, choice.Probes)
			}
			if got := choice.Worthwhile(lists); got != member.installs {
				t.Fatalf("the %s corpus needed %d of %d lists, and the training "+
					"%s — an index on a corpus with no topics reads nearly the "+
					"whole table through a second structure, and one that will "+
					"not install on a topical corpus is bookkeeping for nothing",
					member.name, choice.Probes, lists,
					map[bool]string{true: "installs it", false: "declines it"}[got])
			}

			results := map[search.ShapeFilter]*shapeTally{}
			for _, trial := range gateTrials(f, c, 1000, gateQueries) {
				pool, read := searchAsRun(c, byList, index, trial, choice.Probes)
				key := search.ShapeFilter{Shape: trial.Filter.Shape, Source: trial.Filter.Source}
				tally := results[key]
				if tally == nil {
					tally = &shapeTally{}
					results[key] = tally
				}
				tally.add(pool, trial)
				if read == 0 {
					tally.scanned++
				}
			}
			containers := 0
			for key := range results {
				if key.Shape == search.ShapeContainer {
					containers++
				}
			}
			if results[search.ShapeFilter{Shape: search.ShapeAll}] == nil ||
				results[search.ShapeFilter{Shape: search.ShapeSource, Source: search.SourcePage}] == nil ||
				results[search.ShapeFilter{Shape: search.ShapeSource, Source: search.SourceTask}] == nil ||
				containers == 0 {
				t.Fatalf("the gate measured %d shapes, want unfiltered, each "+
					"source and a container", len(results))
			}
			for key, tally := range results {
				recall, floor := tally.recall(), tally.floor()
				if recall < floor || tally.head != 0 {
					t.Fatalf("the %s index probing %d of %d lists recalls %.4f "+
						"with %d head miss(es) in the %s%s shape on queries its "+
						"training never saw, against a %.4f floor", member.name,
						choice.Probes, lists, recall, tally.head, key.Shape,
						sourceSuffix(key.Source), floor)
				}
				t.Logf("%s %s%s: %d lists, %d probed, recall %.4f (floor %.4f, "+
					"%d of %d scanned)", member.name, key.Shape, sourceSuffix(key.Source),
					lists, choice.Probes, recall, floor, tally.scanned, tally.trials)
			}
		})
	}
}

// A NARROWED SEARCH READS UNTIL IT HAS SEEN ENOUGH OF ITS OWN ROWS, and scans
// when that would take more than the ceiling.
//
// Four lists at increasing distance from the query, a hundred rows each; the
// unfiltered probe reads one. A filter keeping every row reads that same one
// list; one keeping half of each list reads two — as many matching rows as the
// unfiltered probe reads rows; one keeping a tenth of each list would need ten
// lists and the index has four, so it scans; and a filter whose rows all sit in
// the farthest list is found there only if the ceiling allows it, and scanned
// otherwise. Reading the unfiltered count for every filter — the shape this
// replaced — kept a tenth of a narrowed answer's rows in the pool.
func TestANarrowedSearchReadsUntilItHasSeenEnoughOfItsRows(t *testing.T) {
	t.Parallel()
	order := []int{0, 1, 2, 3}
	total := func(int) int { return 100 }
	every := func(int) (int, error) { return 100, nil }
	half := func(int) (int, error) { return 50, nil }
	tenth := func(int) (int, error) { return 10, nil }
	far := func(list int) (int, error) {
		if list == 3 {
			return 100, nil
		}
		return 0, nil
	}
	for _, c := range []struct {
		name     string
		matching func(int) (int, error)
		ceiling  int
		want     int
		indexed  bool
	}{
		{"an unnarrowed search reads the probe count", nil, 2, 1, true},
		{"a filter keeping every row reads the same lists", every, 2, 1, true},
		{"one keeping half of each reads twice as many", half, 2, 2, true},
		{"one keeping a tenth needs more lists than the index has", tenth, 4, 0, false},
		{"rows only in the farthest list are read there within the ceiling", far, 4, 4, true},
		{"and scanned when the ceiling stops short of them", far, 2, 0, false},
	} {
		got, indexed, err := search.ProbeCount(order, 1, c.ceiling, total, c.matching)
		if err != nil {
			t.Fatal(err)
		}
		if got != c.want || indexed != c.indexed {
			t.Errorf("%s: read %d lists (indexed %v), want %d (indexed %v)",
				c.name, got, indexed, c.want, c.indexed)
		}
	}
}

// EVERY HOLDER BUILDS THE SAME INDEX: the same records give the same centroids,
// every row the same list and the same per-list counts, whether a holder
// applied the log as it was written or replayed what the compaction left of
// it.
//
// # What the log is, and why each phase is in it
//
// Two thousand six hundred vectors, an index trained over them and its
// rollout; then eight hundred forgotten, a hundred re-embedded and fifty new;
// then a SECOND training over the smaller corpus, a measurement of it that
// changes its probe count, and its rollout — whose batches sit on subjects
// of their own beside the first rollout's, naming an index nothing has
// installed any more — and finally six hundred vectors written after the
// second index, which are filed as they are written. A holder that replays
// the compacted log sees every one of those shapes: an embed the compaction
// moved past the index, batches for a replaced index, a forget with nothing
// left to forget.
//
// # And the properties the index rests on
//
// The training is a pure function of the codes and the seed — trained twice it
// is the same bytes, and a different seed is different bytes, so the first
// check is not vacuous. Every row is filed in the list whose centroid is
// nearest its code in HAMMING distance, lowest list on a tie — checked here
// against a popcount this test does itself, because an applier that filed by
// a float distance would still agree with every other holder on this machine
// and disagree with one on another architecture. And the per-list counts are
// exactly a recount of the rows, on every holder.
func TestEveryHolderBuildsTheSameIndex(t *testing.T) {
	t.Parallel()
	const dim, model = 32, "holder-embed"
	rng := rand.New(rand.NewPCG(31, 31))
	topics := topicCentres(rng, 24, dim)

	var log []logged
	seq := uint64(0)
	add := func(rec search.VectorRecord) {
		seq++
		log = append(log, logged{seq: seq, rec: rec})
	}
	embed := func(source search.Source, id string) {
		add(embedRecord(source, id, model, topicalEmbedding(rng, topics, 0.3)))
	}
	live := openReplicated(t)
	// THE LIVE HOLDER applies each record as it is appended, which is what
	// lets the duty's steps read the rows the log has produced so far.
	applied := 0
	catchUp := func() {
		t.Helper()
		applyAll(t, live, log[applied:])
		applied = len(log)
	}
	train := func() int64 {
		t.Helper()
		catchUp()
		index := trainedIndex(t, live, model, dim)
		add(index)
		catchUp()
		generation := statelog.Position{Stream: "S", Generation: 1, Seq: seq}.Packed()
		for n := range index.Index.Rollout {
			add(reassignRecord(generation, n, len(index.Index.Rollout)))
		}
		catchUp()
		return generation
	}

	for i := range 1300 {
		embed(search.SourcePage, fmt.Sprintf("p%05d", i))
		embed(search.SourceTask, fmt.Sprintf("t%05d", i))
	}
	train()
	for i := range 400 {
		add(forgetRecord(search.SourcePage, fmt.Sprintf("p%05d", i)))
		add(forgetRecord(search.SourceTask, fmt.Sprintf("t%05d", i)))
	}
	for i := 500; i < 600; i++ {
		embed(search.SourcePage, fmt.Sprintf("p%05d", i))
	}
	for i := range 50 {
		embed(search.SourceTask, fmt.Sprintf("n%05d", i))
	}
	current := train()
	add(measureRecord(current, 3))
	// SIX HUNDRED written after the second index, which the applier files
	// as it writes them rather than by a reassign — the path the popcount
	// check below holds to Hamming distance on its own.
	for i := range 600 {
		embed(search.SourcePage, fmt.Sprintf("q%05d", i))
	}
	// AND FORGETS UNDER IT: rows the reassign filed and rows the applier
	// filed as it wrote them, each leaving a list it is counted in — the
	// path the count moves DOWN on, which nothing before this line took.
	for i := 450; i < 550; i++ {
		add(forgetRecord(search.SourcePage, fmt.Sprintf("p%05d", i)))
		add(forgetRecord(search.SourcePage, fmt.Sprintf("q%05d", i)))
	}
	catchUp()

	again := openReplicated(t)
	applyAll(t, again, log)
	replayed := openReplicated(t)
	applyAll(t, replayed, compacted(log))

	want := indexOf(t, live)
	if want.generation != current || want.lists == 0 || want.probes != 3 {
		t.Fatalf("the live holder installed generation %d with %d lists probing "+
			"%d, and the log's second index is %d, measured at 3 probes",
			want.generation, want.lists, want.probes, current)
	}
	for name, holder := range map[string]*store.DB{
		"a holder applying the same log":       again,
		"a holder replaying the compacted log": replayed,
	} {
		got := indexOf(t, holder)
		if got.generation != want.generation || got.lists != want.lists ||
			got.probes != want.probes || !bytes.Equal(got.centroids, want.centroids) {
			t.Errorf("%s installed generation %d, %d lists probing %d (%d centroid "+
				"bytes); the live holder generation %d, %d lists probing %d (%d)",
				name, got.generation, got.lists, got.probes, len(got.centroids),
				want.generation, want.lists, want.probes, len(want.centroids))
		}
		if gotRows, wantRows := filedRows(t, holder), filedRows(t, live); !slices.Equal(gotRows, wantRows) {
			t.Errorf("%s filed %d rows and the live holder %d, and they differ "+
				"— first difference: %s", name, len(gotRows), len(wantRows),
				firstDifference(gotRows, wantRows))
		}
		if gotCounts, wantCounts := listCounts(t, holder), listCounts(t, live); !slices.Equal(gotCounts, wantCounts) {
			t.Errorf("%s counts its lists differently from the live holder — "+
				"first difference: %s", name, firstDifference(gotCounts, wantCounts))
		}
	}
	for _, holder := range []*store.DB{live, again, replayed} {
		if got, recount := listCounts(t, holder), recountLists(t, holder); !slices.Equal(got, recount) {
			t.Fatalf("kb_ivf_lists is not a recount of the rows filed under the "+
				"installed index — first difference: %s", firstDifference(got, recount))
		}
	}

	// EVERY ROW IS FILED UNDER THE CURRENT INDEX, in its Hamming-nearest
	// list. The rollout converged: nothing predates it any more.
	centroids := decodeCentroids(want.centroids, dim, want.lists)
	for _, row := range rowsWithCodes(t, live, dim) {
		if row.generation != current {
			t.Fatalf("%s is filed under generation %d after the rollout of %d "+
				"applied — the rollout left a row behind", row.key, row.generation,
				current)
		}
		if nearest := nearestByPopcount(row.code, centroids); row.list != nearest {
			t.Fatalf("%s is filed in list %d and its nearest centroid by "+
				"Hamming distance is list %d", row.key, row.list, nearest)
		}
	}

	// AND THE TRAINING IS A FUNCTION OF ITS INPUTS.
	reading := readingOf(t, live, model, dim)
	lists := search.IVFLists(reading.Corpus.Codes.Len())
	trainWith := func(seed uint64) []byte {
		t.Helper()
		index, err := search.TrainIVF(t.Context(), reading.Corpus.Codes, lists, seed,
			reading.HeldOut)
		if err != nil {
			t.Fatal(err)
		}
		return index.Bytes()
	}
	first, second, other := trainWith(search.IVFSeed("S", 7)),
		trainWith(search.IVFSeed("S", 7)), trainWith(search.IVFSeed("S", 8))
	if !bytes.Equal(first, second) {
		t.Fatal("one training over the same codes with the same seed produced " +
			"two different indexes — two holders checking each other, or a " +
			"retry after an unresolved publish, would disagree")
	}
	if bytes.Equal(first, other) {
		t.Fatal("two seeds trained the same index, so the determinism check " +
			"above cannot tell a seeded training from one that ignores its seed")
	}
}

// A ROLLOUT PUBLISHED TWICE, THE SECOND TIME ONLY IN PART, STILL FILES EVERY
// ROW ON A HOLDER THAT REPLAYS THE COMPACTED LOG.
//
// The rollout's ranges are the training's, carried in the centroids record,
// and each batch has a subject of its own naming its index — so a
// re-publication says exactly what the first publication of each batch said,
// and whatever mix of the two the compaction keeps tiles the key space. Here
// the index's rollout is published once in full, then — after a thousand new
// pages arrived and a thousand more tasks, which a rollout cut afresh would
// have shifted every boundary for — again for its first two batches only, as
// a publisher that stopped partway would. A holder replaying what the
// compaction kept must file every row, and a batch of an OLDER index published
// after the new one's must change nothing.
func TestAPartialRepublicationStillFilesEveryRow(t *testing.T) {
	t.Parallel()
	const dim, model = 32, "republish-embed"
	rng := rand.New(rand.NewPCG(41, 41))
	topics := topicCentres(rng, 16, dim)

	var log []logged
	seq := uint64(0)
	add := func(rec search.VectorRecord) {
		seq++
		log = append(log, logged{seq: seq, rec: rec})
	}
	for i := range 3 * search.IVFReassignBatch {
		add(embedRecord(search.SourceTask, fmt.Sprintf("t%05d", i), model,
			topicalEmbedding(rng, topics, 0.3)))
	}
	live := openReplicated(t)
	applyAll(t, live, log)
	older := trainedIndex(t, live, model, dim)
	add(older)
	olderGeneration := statelog.Position{Stream: "S", Generation: 1, Seq: seq}.Packed()
	applyAll(t, live, log[len(log)-1:])
	index := trainedIndex(t, live, model, dim)
	index.Index.Seed++ // another training, so another index
	add(index)
	generation := statelog.Position{Stream: "S", Generation: 1, Seq: seq}.Packed()
	batches := len(index.Index.Rollout)
	if batches < 3 {
		t.Fatalf("the rollout has %d batches; the case needs several", batches)
	}
	for n := range batches {
		add(reassignRecord(generation, n, batches))
	}
	for i := range search.IVFReassignBatch {
		add(embedRecord(search.SourcePage, fmt.Sprintf("p%05d", i), model,
			topicalEmbedding(rng, topics, 0.3)))
		add(embedRecord(search.SourceTask, fmt.Sprintf("s%05d", i), model,
			topicalEmbedding(rng, topics, 0.3)))
	}
	// THE PARTIAL RE-PUBLICATION, and a batch of the replaced index.
	add(reassignRecord(generation, 0, batches))
	add(reassignRecord(generation, 1, batches))
	add(reassignRecord(olderGeneration, 2, len(older.Index.Rollout)))

	replayed := openReplicated(t)
	applyAll(t, replayed, compacted(log))
	if got := indexOf(t, replayed); got.generation != generation {
		t.Fatalf("the replaying holder installed %d, want %d", got.generation, generation)
	}
	for _, row := range rowsWithCodes(t, replayed, dim) {
		if row.generation != generation {
			t.Fatalf("%s is filed under %d on a holder that replayed the "+
				"compacted log — a batch of the index's rollout is missing from "+
				"what the compaction kept", row.key, row.generation)
		}
	}
	if got, recount := listCounts(t, replayed), recountLists(t, replayed); !slices.Equal(got, recount) {
		t.Fatalf("the replayed holder's list counts are not a recount: %s",
			firstDifference(got, recount))
	}
}

// THE PROBE IN SQL IS THE PROBE THE TRAINING MEASURED — unfiltered and
// narrowed.
//
// The training grades an index with [search.IVFCandidates] and
// [search.ProbeCount], pure functions over codes loaded in key order; searches
// run the SQL probe. If the two disagreed — a missing tie break, a list read
// from the wrong generation, a filter on the wrong side of the join, a
// narrowed search that read the unfiltered lists — the recall a training
// recorded would describe a first stage no search runs. So: one store, one
// index, the same queries through both, in every shape, and the candidate
// lists must be identical. And at every list the probe must be the full scan
// exactly, since every row is then probed.
func TestTheProbeIsThePoolTheTrainingMeasured(t *testing.T) {
	t.Parallel()
	const dim, model = 32, "probe-embed"
	db, generation := indexedStore(t, model, dim, 1600)
	c, keys := corpusAndKeys(t, db, model, dim)
	head := indexOf(t, db)
	index, err := search.DecodeIVF(dim, head.lists, head.centroids)
	if err != nil {
		t.Fatal(err)
	}
	byList := search.GroupByList(filedIn(t, index, c.Codes), head.lists)

	shapes := []struct {
		name       string
		sources    []search.Source
		containers []string
		filter     search.ShapeFilter
	}{
		{"unfiltered", nil, nil, search.ShapeFilter{Shape: search.ShapeAll}},
		{"one source", []search.Source{search.SourcePage}, nil,
			search.ShapeFilter{Shape: search.ShapeSource, Source: search.SourcePage}},
		{"one source in one container", []search.Source{search.SourceTask},
			[]string{"OPS"}, search.ShapeFilter{Shape: search.ShapeContainer,
				Source: search.SourceTask, Container: containerIndex(t, db, "OPS")}},
	}
	rng := rand.New(rand.NewPCG(77, 77))
	methods := map[search.Stage1Method]int{}
	for q := range 8 {
		query := randomEmbedding(rng, dim)
		code := search.Quantize(unpackFloats(query))
		for _, shape := range shapes {
			for _, probes := range []int{1, 3, head.lists} {
				got, report := stage1(t, db, search.SemanticQuery{
					Vector: query, Model: model, Dim: dim, Probes: probes,
					Sources: shape.sources, Containers: shape.containers,
					Candidates: 200, Limit: 50,
				})
				trial := search.IVFTrial{Query: code, Filter: shape.filter, Self: -1}
				pool, lists := searchAsRunDepth(c, byList, index, trial, probes, 200)
				var want []string
				for _, row := range pool {
					want = append(want, keys[row])
				}
				if !slices.Equal(got, want) {
					t.Fatalf("query %d, %s, at %d probes: the SQL first stage "+
						"kept %d candidates and the training's measure of it %d "+
						"— first difference: %s", q, shape.name, probes, len(got),
						len(want), firstDifference(got, want))
				}
				methods[report.Method]++
				switch {
				case lists == 0 && (report.Method != search.Stage1Scan ||
					report.Why != search.ScanFiltered):
					t.Fatalf("query %d, %s, at %d probes: the training scanned "+
						"and the SQL first stage reports %+v", q, shape.name,
						probes, report)
				case lists > 0 && (report.Method != search.Stage1IVF ||
					report.IVFGeneration != generation || report.Probed != lists ||
					report.Stale):
					t.Fatalf("query %d, %s, at %d probes: the training read %d "+
						"lists and the SQL first stage reports %+v", q,
						shape.name, probes, lists, report)
				}
				if probes == head.lists {
					scan, _ := stage1(t, db, search.SemanticQuery{
						Vector: query, Model: model, Dim: dim, FullScan: true,
						Sources: shape.sources, Containers: shape.containers,
						Candidates: 200, Limit: 50,
					})
					if !slices.Equal(got, scan) {
						t.Fatalf("query %d, %s, probing every list kept "+
							"different candidates from the full scan: %s", q,
							shape.name, firstDifference(got, scan))
					}
				}
			}
		}
	}
	if methods[search.Stage1IVF] == 0 || methods[search.Stage1Scan] == 0 {
		t.Fatalf("the shapes ran %v — the comparison must cover a narrowed "+
			"search the index answers AND one it hands to the scan", methods)
	}
}

// A ROLLOUT IN FLIGHT HIDES NOTHING.
//
// A new index is installed by one record and its rows are re-filed by many; in
// between, a row is filed under the index that was replaced, and its list
// means nothing to the new one. The probe reads those rows in full beside the
// lists, so a document that is the best answer is found whichever side of the
// rollout it is on — and the report says the union ran.
func TestARowTheRolloutHasNotReachedIsStillFound(t *testing.T) {
	t.Parallel()
	const dim, model = 32, "stale-embed"
	db, _ := indexedStore(t, model, dim, 1600)

	// A SECOND INDEX, installed and not rolled out: every row is stale.
	rec := trainedIndex(t, db, model, dim)
	rec.Index.Seed++ // a different training, so a different index
	applyAt(t, db, rec, 1_000_000)

	// THE BEST ANSWER to a query is the document the query IS.
	var target []byte
	var targetKey string
	if err := storetest.EstateOf(db).Read(t.Context(), func(tx *sql.Tx) error {
		var source, id string
		if err := tx.QueryRowContext(t.Context(), `
			SELECT source, source_id, embedding FROM kb_vectors
			ORDER BY source_id LIMIT 1 OFFSET 777`).Scan(&source, &id, &target); err != nil {
			return err
		}
		targetKey = search.Key(search.Source(source), id)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var hits []search.SemanticHit
	var report search.Stage1Report
	if err := storetest.EstateOf(db).Read(t.Context(), func(tx *sql.Tx) error {
		var err error
		hits, report, err = search.Semantic(t.Context(), tx, search.SemanticQuery{
			Vector: target, Model: model, Dim: dim, Probes: 1, Limit: 5,
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if report.Method != search.Stage1IVF || !report.Stale {
		t.Fatalf("a probe over an index nothing is filed under reported %+v — "+
			"it must run and say it read the unfiled rows", report)
	}
	if len(hits) == 0 || search.Key(hits[0].Source, hits[0].ID) != targetKey {
		t.Fatalf("the document the query is was not its first hit mid-rollout: "+
			"got %v, want %s first", hits, targetKey)
	}
}

// AN INDEX IS INSTALLED ONLY ABOVE THE ONE IT REPLACES, AND A MEASUREMENT ONLY
// OF THE INDEX IT NAMES AND ABOVE THE ONE IT REPLACES.
//
// The centroids subject is compacted to one record, but a node still meets
// the older one — a redelivery, a replay that began before the newer landed —
// and installing it over the newer would file every later vector against
// centroids no other holder has. A measurement is the same: one of a replaced
// index describes lists nobody reads, and an older one of this index would put
// back a probe count the corpus has moved past.
func TestAnOlderIndexNeverReplacesANewerOne(t *testing.T) {
	t.Parallel()
	const dim, model = 32, "order-embed"
	db, generation := indexedStore(t, model, dim, 1100)
	newer := indexOf(t, db)
	older := trainedIndex(t, db, model, dim)
	older.Index.Seed++
	applyAt(t, db, older, 5) // far below the installed generation
	if got := indexOf(t, db); got.generation != generation ||
		!bytes.Equal(got.centroids, newer.centroids) {
		t.Fatalf("a centroids record below the installed generation %d replaced "+
			"it (now %d)", generation, got.generation)
	}

	applyAt(t, db, measureRecord(generation, 2), 1<<30)
	applyAt(t, db, measureRecord(generation, 5), 1<<29)   // older
	applyAt(t, db, measureRecord(generation-1, 7), 1<<31) // another index
	if got := indexOf(t, db); got.probes != 2 || got.generation != generation {
		t.Fatalf("after a measurement at 2 probes, an older one at 5 and one of "+
			"another index at 7, the index probes %d (generation %d)",
			got.probes, got.generation)
	}
	if filed := filedRows(t, db); len(filed) == 0 {
		t.Fatal("no rows to check")
	}
	for _, row := range rowsWithCodes(t, db, dim) {
		if row.generation != generation {
			t.Fatalf("a measurement re-filed %s under %d — it changes how the "+
				"index is read and never a row", row.key, row.generation)
		}
	}
}

// AN INDEX RECORD IS JUDGED BY ITS STRUCTURE, NEVER BY THIS BUILD'S KNOBS.
//
// A later build may raise [search.IVFMaxLists] — a tuning constant whose
// rationale is corpus size — and write its index at the SAME record version,
// since the record's shape did not change. Refused here as a writer fault,
// every node still on this build would fail that record on every redelivery
// for the length of the upgrade. So an index of twice this build's ceiling is
// installed and probed like any other, and what IS refused is structure: a
// blob that is not the lists it claims, a probe count outside its lists.
func TestAnIndexFromALaterBuildIsJudgedByItsStructure(t *testing.T) {
	t.Parallel()
	const dim, model = 64, "later-embed"
	lists := 2 * search.IVFMaxLists
	record := indexRecordOver(model, dim)
	record.Index.Lists, record.Index.Probes = lists, 3
	record.Index.Centroids = make([]byte, 8*search.CodeWords(dim)*lists)
	db := openReplicated(t)
	applyAt(t, db, record, 7)
	if got := indexOf(t, db); got.lists != lists || got.probes != 3 {
		t.Fatalf("an index of %d lists — twice this build's ceiling — installed "+
			"as %+v", lists, got)
	}
	rng := rand.New(rand.NewPCG(3, 3))
	if _, report := stage1(t, db, search.SemanticQuery{Vector: randomEmbedding(rng, dim),
		Model: model, Dim: dim}); report.Method != search.Stage1IVF || report.Lists != lists {
		t.Fatalf("the later build's index is not probed: %+v", report)
	}

	for name, broken := range map[string]func(*search.IndexRecord){
		"centroids that are not the lists claimed": func(x *search.IndexRecord) {
			x.Centroids = x.Centroids[8:]
		},
		"a probe count above the lists": func(x *search.IndexRecord) { x.Probes = x.Lists + 1 },
		"no probe at all":               func(x *search.IndexRecord) { x.Probes = 0 },
	} {
		bad := indexRecordOver(model, dim)
		broken(bad.Index)
		if _, err := bad.Encode(); err == nil {
			t.Errorf("an index record with %s encoded", name)
		}
	}
}

// A VERDICT ABOUT ANOTHER EMBEDDING SPACE SAYS NOTHING ABOUT THIS SEARCH.
//
// A partition whose last training — under the model the company has since
// left — concluded it should have no index, scans in the new space because
// there is no index in it, not because of that verdict: reported as the
// verdict, an operator reads a training of a model they no longer run as the
// reason this search scanned.
func TestAVerdictAboutAnotherSpaceIsNotThisSearchs(t *testing.T) {
	t.Parallel()
	const dim = 16
	db := openReplicated(t)
	verdict := indexRecordOver("left-model", dim)
	verdict.Index.Lists, verdict.Index.Probes = 0, 0
	verdict.Index.Centroids, verdict.Index.Rollout = nil, nil
	verdict.Index.Why = search.VerdictNotWorthwhile
	applyAt(t, db, verdict, 3)
	rng := rand.New(rand.NewPCG(4, 4))
	_, report := stage1(t, db, search.SemanticQuery{Vector: randomEmbedding(rng, dim),
		Model: "current-model", Dim: dim})
	if report.Method != search.Stage1Scan || report.Why != search.ScanOtherSpace {
		t.Fatalf("a search in the current space over a verdict about another "+
			"reports %+v, want a scan for the other space", report)
	}
	_, report = stage1(t, db, search.SemanticQuery{Vector: randomEmbedding(rng, dim),
		Model: "left-model", Dim: dim})
	if report.Why != search.ScanRetired {
		t.Fatalf("a search in the verdict's own space reports %+v", report)
	}
}

// THE CODE A ROW STORES IS THE CODE THE PROBE COMPUTES.
//
// A written vector is filed from [search.Quantize] of its own bytes; a
// reassign files a row from the `vector1bit` blob the database stored; a query
// ranks lists from [search.Quantize] of its own vector. If the two codes
// differed in one bit a row would be filed where no query looks for it — so
// the stored blob, read back through the one decoder, must be Quantize's
// output exactly, at widths that are and are not a whole number of words and
// of bytes.
func TestTheStoredCodeIsTheQuantizedCode(t *testing.T) {
	t.Parallel()
	db := openReplicated(t)
	rng := rand.New(rand.NewPCG(5, 6))
	for _, dim := range []int{1, 7, 8, 9, 63, 64, 65, 100, 1536, 3072} {
		v := make([]float32, dim)
		for i := range v {
			switch i % 5 {
			case 0:
				v[i] = 0 // an exact zero takes the negative bit
			default:
				v[i] = float32(rng.NormFloat64())
			}
		}
		var blob []byte
		if err := storetest.EstateOf(db).Read(t.Context(), func(tx *sql.Tx) error {
			return tx.QueryRowContext(t.Context(), `SELECT vector1bit(?)`,
				pack(v)).Scan(&blob)
		}); err != nil {
			t.Fatal(err)
		}
		code, err := search.CodeFromBits(blob, dim)
		if err != nil {
			t.Fatalf("width %d: %v", dim, err)
		}
		if want := search.Quantize(v); !slices.Equal(code, want) {
			t.Fatalf("width %d: the stored code decodes to %x and Quantize "+
				"gives %x", dim, code, want)
		}
	}
}

// THE INDEX STEP FOLLOWS THE CORPUS: the one decision a duty tick takes, as a
// function of what it read.
func TestTheIndexStepFollowsTheCorpus(t *testing.T) {
	t.Parallel()
	const model, dim = "m", 64
	now := time.Unix(1_700_000_000, 0).UTC()
	live := func(trainedOn, lists, largest int) search.IndexHead {
		return search.IndexHead{Generation: 9, Model: model, Dim: dim,
			Lists: lists, Probes: 2, TrainedOn: trainedOn, Largest: largest,
			MeasuredAt: now.Add(-time.Hour)}
	}
	filed := func(s search.IndexState) search.IndexState {
		s.Filed, s.Now = s.Sources, now
		return s
	}
	for _, c := range []struct {
		name  string
		state search.IndexState
		want  search.IndexAction
	}{
		{"an empty partition wants nothing",
			search.IndexState{}, search.IndexKeep},
		{"a partition below the minimum corpus is left to the scan",
			search.IndexState{Sources: search.IVFMinCorpus - 1}, search.IndexKeep},
		{"a partition at the minimum corpus is trained",
			search.IndexState{Sources: search.IVFMinCorpus}, search.IndexTrain},
		{"a node behind the log decides nothing from its rows",
			search.IndexState{Sources: search.IVFMinCorpus, Behind: true}, search.IndexKeep},
		{"a node the log counts that cannot read the index holds it back",
			search.IndexState{Sources: search.IVFMinCorpus, Held: []string{"old"}},
			search.IndexKeep},
		{"an index in another embedding space is no index",
			search.IndexState{Indexed: true, Sources: 5000, Head: search.IndexHead{
				Generation: 9, Model: "old", Dim: dim, Lists: 64, Probes: 4,
				TrainedOn: 5000}}, search.IndexTrain},
		{"a verdict that it was too small is revisited once it is not",
			search.IndexState{Indexed: true, Sources: search.IVFMinCorpus,
				Head: search.IndexHead{Generation: 9, Model: model, Dim: dim,
					TrainedOn: 900, Why: search.VerdictTooSmall}}, search.IndexTrain},
		{"a verdict that no index was worth it holds until the corpus doubles",
			search.IndexState{Indexed: true, Sources: 7999,
				Head: search.IndexHead{Generation: 9, Model: model, Dim: dim,
					TrainedOn: 4000, Why: search.VerdictNotWorthwhile}}, search.IndexKeep},
		{"and is re-derived when it has",
			search.IndexState{Indexed: true, Sources: 8000,
				Head: search.IndexHead{Generation: 9, Model: model, Dim: dim,
					TrainedOn: 4000, Why: search.VerdictNotWorthwhile}}, search.IndexTrain},
		{"a live index with unfiled rows rolls out",
			search.IndexState{Indexed: true, Sources: 5000, Now: now,
				Head: live(5000, 64, 200)}, search.IndexRollout},
		{"a corpus that doubled is retrained before any rollout",
			search.IndexState{Indexed: true, Sources: 10000, Filed: 9997, Now: now,
				Head: live(5000, 64, 200)}, search.IndexTrain},
		{"a corpus that halved is retrained",
			filed(search.IndexState{Indexed: true, Sources: 2500,
				Head: live(5000, 64, 200)}), search.IndexTrain},
		{"a list that grew past four times the mean and twice its trained share is imbalance",
			filed(search.IndexState{Indexed: true, Sources: 6400, Largest: 401,
				Head: live(6400, 64, 200)}), search.IndexTrain},
		{"and one at four times the mean is not",
			filed(search.IndexState{Indexed: true, Sources: 6400, Largest: 400,
				Head: live(6400, 64, 200)}), search.IndexKeep},
		{"an index trained lopsided that has not grown so is kept",
			filed(search.IndexState{Indexed: true, Sources: 22000, Largest: 2030,
				Head: live(22000, 256, 2020)}), search.IndexKeep},
		{"and retrained once its fullest list has doubled its share",
			filed(search.IndexState{Indexed: true, Sources: 22000, Largest: 4040,
				Head: live(22000, 256, 2020)}), search.IndexTrain},
		{"a rollout in flight is finished before its lists are judged",
			search.IndexState{Indexed: true, Sources: 6400, Filed: 3400,
				Largest: 3000, Now: now, Head: live(6400, 64, 100)}, search.IndexRollout},
		{"a current, filed, balanced index is kept",
			filed(search.IndexState{Indexed: true, Sources: 6000, Largest: 120,
				Head: live(5000, 64, 100)}), search.IndexKeep},
		{"and measured again once its measurement is a day old",
			search.IndexState{Indexed: true, Sources: 6000, Filed: 6000,
				Largest: 120, Now: now.Add(search.IVFMeasureInterval),
				Head: live(5000, 64, 100)}, search.IndexMeasure},
	} {
		if got := search.DecideIndex(c.state, model, dim); got != c.want {
			t.Errorf("%s: %s, want %s", c.name, got, c.want)
		}
	}
}

// AN INDEX TRAINED OVER MANY IDENTICAL CODES COMES TO REST.
//
// Identical codes are nearest to the same centroid whatever the seed, so a
// corpus with a template copied thousands of times files every copy in one
// list and no training can split them. Judged against the mean alone, every
// tick after the rollout decided to retrain it — a full training and a
// rollout re-filing the partition on every holder, every two ticks for ever.
// Judged against what its own training achieved, the index trained here is
// kept, over three seeds.
func TestAnIndexTrainedOverDuplicatesComesToRest(t *testing.T) {
	t.Parallel()
	f := topicalGateFixture()
	codes := search.NewCodes(len(f.Codes[0]), f.Len()+2000)
	for _, code := range f.Codes {
		codes.Append(code)
	}
	for range 2000 {
		codes.Append(f.Codes[7])
	}
	n := codes.Len()
	lists := search.IVFLists(n)
	for basis := range int64(3) {
		index, err := search.TrainIVF(t.Context(), codes, lists,
			search.IVFSeed("dupes", basis), nil)
		if err != nil {
			t.Fatal(err)
		}
		largest := search.LargestList(search.GroupByList(filedIn(t, index, codes), lists))
		if largest*lists <= search.IVFImbalance*n {
			t.Fatalf("seed %d left a largest list of %d against a mean of %d — "+
				"the case needs a training that cannot balance its lists",
				basis, largest, n/lists)
		}
		state := search.IndexState{Indexed: true, Sources: n, Filed: n,
			Largest: largest, Now: time.Unix(1_700_000_000, 0).UTC(),
			Head: search.IndexHead{Generation: 9, Model: "m", Dim: 3072,
				Lists: lists, Probes: 8, TrainedOn: n, Largest: largest,
				MeasuredAt: time.Unix(1_700_000_000, 0).UTC()}}
		if got := search.DecideIndex(state, "m", 3072); got != search.IndexKeep {
			t.Fatalf("seed %d: an index whose training left a largest list of "+
				"%d (mean %d) decides %s over the corpus it was trained on — "+
				"every retrain lands in the same place", basis, largest, n/lists, got)
		}
	}
}

// A PROBE COUNT THAT LOSES THE HEAD OF AN ANSWER IS NOT CHOSEN, however good
// its aggregate.
//
// The training installs the smallest count meeting the floor WITH NO HEAD
// MISS — the judgement [search.EvalReport.Passed] makes — because a recall of
// 0.99 is compatible with losing the one document that matters, and a
// semantic-only document the first stage drops leaves the fused answer
// entirely. Here one list holds a query's best document and the other holds
// the rest of its answer: probing the second alone recalls 149 of 150, above
// every floor, and must still not be chosen.
func TestAProbeCountThatLosesTheHeadIsNotChosen(t *testing.T) {
	t.Parallel()
	near, far := []uint64{0}, []uint64{^uint64(0)}
	index, err := search.NewIVF(1, append(append([]uint64{}, near...), far...))
	if err != nil {
		t.Fatal(err)
	}
	c := &search.TrainingCorpus{Codes: search.NewCodes(1, 200)}
	for range 199 {
		c.Codes.Append(near)
	}
	c.Codes.Append(far) // row 199: the query's best document, in the far list
	for range c.Codes.Len() {
		c.Sources = append(c.Sources, search.SourceTask)
		c.Containers = append(c.Containers, 0)
	}
	byList := search.GroupByList(filedIn(t, index, c.Codes), index.Lists())
	want := []int{199}
	for row := range 149 {
		want = append(want, row)
	}
	choice, err := search.ChooseProbes(t.Context(), c, byList, index,
		[]search.IVFTrial{{Query: near, Filter: search.ShapeFilter{Shape: search.ShapeAll},
			Self: -1, Want: want, Floor: 0.98}})
	if err != nil {
		t.Fatal(err)
	}
	if choice.Probes != 2 || !choice.Passed() || choice.Measurement.HeadMisses != 0 {
		t.Fatalf("the training chose %+v — probing the near list alone recalls "+
			"149 of 150 and drops the best document", choice)
	}
}

// A ROLLOUT'S RANGES TILE EVERY SOURCE'S WHOLE KEY SPACE.
//
// Each batch is applied on every holder and re-files exactly the range the
// index's own record fixed — so the ranges alone must cover every id, of every
// source, including a source the partition holds no row of. A gap is a row no
// batch ever re-files; and a range the record's own validation would refuse
// is a centroids record no holder can install.
func TestARolloutCoversEveryKey(t *testing.T) {
	t.Parallel()
	ids := map[search.Source][]string{}
	for i := range 2*search.IVFReassignBatch + 17 {
		ids[search.SourcePage] = append(ids[search.SourcePage], fmt.Sprintf("p%06d", i))
	}
	ranges := search.RolloutRanges(ids)
	// Three page batches and one open task batch.
	if len(ranges) != 4 {
		t.Fatalf("%d batches for %d page ids and no task ids", len(ranges),
			len(ids[search.SourcePage]))
	}
	covered := func(source search.Source, id string) int {
		hits := 0
		for _, r := range ranges {
			if r.Holds(source, id) {
				hits++
			}
		}
		return hits
	}
	probes := append([]string{"", "!", "zzzzzz"}, ids[search.SourcePage]...)
	for _, id := range probes {
		for _, source := range search.Sources {
			if got := covered(source, id); got != 1 {
				t.Fatalf("%s %q is covered by %d batches, want exactly one", source,
					id, got)
			}
		}
	}
	for _, r := range ranges[:2] {
		if r.To == "" {
			t.Fatalf("a batch in the middle of a source runs to its end: %+v", r)
		}
	}
	record := search.VectorRecord{
		RecordEnvelope: search.RecordEnvelope{Subject: search.IndexCentroids,
			Op: search.OpCentroids, Scope: statelog.ScopeSet{
				Paths: []string{search.IndexScopePath(search.IndexCentroids)}}},
		Model: "m", Dim: 64,
		Index: &search.IndexRecord{Log: "S", Lists: 1, Probes: 1,
			Centroids: make([]byte, 8), Rollout: ranges},
	}
	if _, err := record.Encode(); err != nil {
		t.Fatalf("a record carrying the ranges RolloutRanges cut is refused: %v", err)
	}
	for name, broken := range map[string][]search.RolloutRange{
		"a gap":            {ranges[0], ranges[2], ranges[3]},
		"a source twice":   {ranges[3], ranges[0], ranges[1], ranges[2], ranges[3]},
		"an unclosed tail": {ranges[0], ranges[1], ranges[3]},
	} {
		record.Index.Rollout = broken
		if _, err := record.Encode(); err == nil {
			t.Errorf("a rollout with %s encoded", name)
		}
	}
}

// ---- helpers --------------------------------------------------------- //

var topicalFixtures sync.Map

// topicalGateFixture is the topical member at the gate's smaller size, built
// once for every case that reads it.
func topicalGateFixture() *search.Fixture {
	build, _ := topicalFixtures.LoadOrStore(20_000, sync.OnceValue(func() *search.Fixture {
		return search.NewTopicalFixture(20_000, 1)
	}))
	return build.(func() *search.Fixture)()
}

// gateCorpus files a fixture's documents as a company's: a tenth of them pages
// and the rest tasks, and each in a container — eight topics to a container
// on the topical member, a sixty-fourth of the corpus each and clustered in
// the space as a project is; by row on the isotropic one, which has no topics.
func gateCorpus(f *search.Fixture) *search.TrainingCorpus {
	c := &search.TrainingCorpus{Codes: search.NewCodes(len(f.Codes[0]), f.Len())}
	for i, code := range f.Codes {
		c.Codes.Append(code)
		source := search.SourceTask
		if i%10 == 0 {
			source = search.SourcePage
		}
		c.Sources = append(c.Sources, source)
		container := int32(i % 64)
		if f.Topics[i] >= 0 {
			container = f.Topics[i] / 8
		}
		c.Containers = append(c.Containers, container)
	}
	return c
}

// gateTrials is n fixture queries from seed on, each in every shape: whole,
// each source, and the source and container of its nearest document.
func gateTrials(f *search.Fixture, c *search.TrainingCorpus, seed uint64, n int) []search.IVFTrial {
	var out []search.IVFTrial
	for q := range n {
		code, similarity := f.Query(seed + uint64(q))
		nearest := search.Exact(f.Len(), similarity, 1)[0]
		for _, filter := range []search.ShapeFilter{
			{Shape: search.ShapeAll},
			{Shape: search.ShapeSource, Source: search.SourcePage},
			{Shape: search.ShapeSource, Source: search.SourceTask},
			{Shape: search.ShapeContainer, Source: c.Sources[nearest],
				Container: c.Containers[nearest]},
		} {
			var kept []int
			for row := range f.Len() {
				if keeps(c, filter, row) {
					kept = append(kept, row)
				}
			}
			out = append(out, search.IVFTrial{Query: code, Filter: filter, Self: -1,
				Want:  search.Rerank(kept, similarity, search.ReturnDepth),
				Floor: search.FloorAt(len(kept))})
		}
	}
	return out
}

// keeps is ShapeFilter's rule, written out here rather than taken from the
// package, so the gate's ground truth does not share the code it checks.
func keeps(c *search.TrainingCorpus, f search.ShapeFilter, row int) bool {
	switch f.Shape {
	case search.ShapeSource:
		return c.Sources[row] == f.Source
	case search.ShapeContainer:
		return c.Sources[row] == f.Source && c.Containers[row] == f.Container
	}
	return true
}

// searchAsRun is the first stage a search runs for trial at probes: the lists
// [search.ProbeCount] chooses, or the full scan over its filter when it
// chooses none. It reports how many lists it read, zero for the scan.
func searchAsRun(c *search.TrainingCorpus, byList [][]int32, index search.IVF, trial search.IVFTrial, probes int) ([]int, int) {
	return searchAsRunDepth(c, byList, index, trial, probes, search.Stage1Depth)
}

func searchAsRunDepth(c *search.TrainingCorpus, byList [][]int32, index search.IVF, trial search.IVFTrial, probes, depth int) ([]int, int) {
	order := index.ProbeOrder(trial.Query)
	var keep func(int) bool
	if trial.Self >= 0 {
		keep = func(row int) bool { return row != trial.Self }
	}
	var matching func(int) (int, error)
	if trial.Filter.Shape != search.ShapeAll {
		keep = func(row int) bool { return row != trial.Self && keeps(c, trial.Filter, row) }
		matching = func(list int) (int, error) {
			n := 0
			for _, row := range byList[list] {
				if keep(int(row)) {
					n++
				}
			}
			return n, nil
		}
	}
	lists, indexed, err := search.ProbeCount(order, probes,
		index.Lists()/search.IVFProbeCeiling,
		func(list int) int { return len(byList[list]) }, matching)
	if err != nil {
		panic(err)
	}
	if !indexed {
		every := make([]int, index.Lists())
		for j := range every {
			every[j] = j
		}
		return search.IVFCandidates(c.Codes, byList, every, trial.Query, depth, keep), 0
	}
	return search.IVFCandidates(c.Codes, byList, order[:lists], trial.Query, depth, keep), lists
}

// shapeTally accumulates one shape's measurement over its trials.
type shapeTally struct {
	trials, head, scanned int
	recallSum, floorSum   float64
}

func (s *shapeTally) add(pool []int, trial search.IVFTrial) {
	s.trials++
	s.recallSum += search.Recall(pool, trial.Want)
	s.floorSum += trial.Floor
	for _, rank := range search.MissRanks(pool, trial.Want) {
		if rank < 10 {
			s.head++
		}
	}
}

func (s *shapeTally) recall() float64 { return s.recallSum / float64(s.trials) }
func (s *shapeTally) floor() float64  { return s.floorSum / float64(s.trials) }

func sourceSuffix(source search.Source) string {
	if source == "" {
		return ""
	}
	return ":" + string(source)
}

type logged struct {
	seq uint64
	rec search.VectorRecord
}

// compacted is what a compacted stream keeps of log: the newest record of
// each subject, in the order the log holds them.
func compacted(log []logged) []logged {
	last := map[search.Subject]uint64{}
	for _, entry := range log {
		last[entry.rec.Subject] = entry.seq
	}
	var out []logged
	for _, entry := range log {
		if last[entry.rec.Subject] == entry.seq {
			out = append(out, entry)
		}
	}
	return out
}

func embedRecord(source search.Source, id, model string, embedding []byte) search.VectorRecord {
	return embedRecordIn(source, id, "ENG", model, embedding)
}

func embedRecordIn(source search.Source, id, container, model string, embedding []byte) search.VectorRecord {
	subject := search.Subject{Source: source, ID: id}
	return search.VectorRecord{
		RecordEnvelope: search.RecordEnvelope{
			Subject: subject, Op: search.OpEmbed, Gen: 1,
			CreatedAt: time.Unix(1_700_000_000, 0).UTC(),
			Scope:     statelog.ScopeSet{Paths: []string{search.ScopePath(container, subject)}},
		},
		Container: container, Model: model, Dim: len(embedding) / 4, Embedding: embedding,
	}
}

func forgetRecord(source search.Source, id string) search.VectorRecord {
	subject := search.Subject{Source: source, ID: id}
	return search.VectorRecord{RecordEnvelope: search.RecordEnvelope{
		Subject: subject, Op: search.OpForget, Gen: 1,
		Scope: statelog.ScopeSet{Paths: []string{search.ScopePath("ENG", subject)}},
	}}
}

func reassignRecord(generation int64, batch, batches int) search.VectorRecord {
	subject := search.ReassignSubject(generation, batch)
	return search.VectorRecord{
		RecordEnvelope: search.RecordEnvelope{
			Subject: subject, Op: search.OpReassign, Gen: 1,
			Scope: statelog.ScopeSet{Paths: []string{search.IndexScopePath(subject)}},
		},
		Reassign: &search.ReassignRecord{Index: generation, Batch: batch, Batches: batches},
	}
}

func measureRecord(generation int64, probes int) search.VectorRecord {
	subject := search.MeasureSubject(generation)
	return search.VectorRecord{
		RecordEnvelope: search.RecordEnvelope{
			Subject: subject, Op: search.OpMeasure, Gen: 1,
			CreatedAt: time.Unix(1_700_000_000, 0).UTC(),
			Scope:     statelog.ScopeSet{Paths: []string{search.IndexScopePath(subject)}},
		},
		Measure: &search.MeasureRecord{Index: generation, Probes: probes,
			Measurement: search.Measurement{Sources: 1, Recall: 0.99, Floor: 0.98,
				Shape: search.ShapeAll}},
	}
}

// readingOf reads a store's training set exactly as the duty does.
func readingOf(t testing.TB, db *store.DB, model string, dim int) search.TrainingReading {
	t.Helper()
	var reading search.TrainingReading
	if err := storetest.EstateOf(db).Read(t.Context(), func(tx *sql.Tx) error {
		var err error
		reading, err = search.ReadTrainingSet(t.Context(), tx, model, dim)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return reading
}

// trainedIndex trains db's index exactly as the duty does and returns its
// centroids record — INSTALLED whatever the training's verdict, because these
// tests are about what an installed index does.
func trainedIndex(t testing.TB, db *store.DB, model string, dim int) search.VectorRecord {
	t.Helper()
	reading := readingOf(t, db, model, dim)
	codes := reading.Corpus.Codes
	lists := search.IVFLists(codes.Len())
	seed := search.IVFSeed("S", 0)
	index, err := search.TrainIVF(context.Background(), codes, lists, seed, reading.HeldOut)
	if err != nil {
		t.Fatal(err)
	}
	byList := search.GroupByList(filedIn(t, index, codes), lists)
	choice, err := search.ChooseProbes(context.Background(), &reading.Corpus, byList,
		index, reading.Trials)
	if err != nil {
		t.Fatal(err)
	}
	measurement := choice.Measurement
	return search.VectorRecord{
		RecordEnvelope: search.RecordEnvelope{
			Subject: search.IndexCentroids, Op: search.OpCentroids, Gen: 1,
			CreatedAt: time.Unix(1_700_000_000, 0).UTC(),
			Scope: statelog.ScopeSet{Paths: []string{
				search.IndexScopePath(search.IndexCentroids)}},
		},
		Model: model, Dim: dim,
		Index: &search.IndexRecord{
			Log: "S", Seed: seed, Lists: lists, Probes: choice.Probes,
			TrainedOn: codes.Len(), Largest: search.LargestList(byList),
			Measurement: &measurement, Centroids: index.Bytes(),
			Rollout: search.RolloutRanges(reading.IDs),
		},
	}
}

// indexedStore is a store holding n topical vectors — alternately pages and
// tasks, alternately in the ENG and OPS containers — an installed index over
// them and its completed rollout, and the index's generation.
func indexedStore(t *testing.T, model string, dim, n int) (*store.DB, int64) {
	t.Helper()
	db := openReplicated(t)
	rng := rand.New(rand.NewPCG(uint64(n), uint64(dim)))
	topics := topicCentres(rng, 16, dim)
	seq := uint64(0)
	if err := storetest.EstateOf(db).Tx(t.Context(), func(tx *sql.Tx) error {
		applier := search.NewApplier()
		for i := range n {
			source := search.SourcePage
			if i%2 == 1 {
				source = search.SourceTask
			}
			container := "ENG"
			if i%4 >= 2 {
				container = "OPS"
			}
			seq++
			rec := embedRecordIn(source, fmt.Sprintf("s%05d", i), container, model,
				topicalEmbedding(rng, topics, 0.4))
			payload, err := rec.Encode()
			if err != nil {
				return err
			}
			if _, err := applier.Apply(t.Context(), tx, statelog.Record{
				Position: statelog.Position{Stream: "S", Generation: 1, Seq: seq},
				Payload:  payload,
			}, statelog.ApplyOptions{}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	seq++
	index := trainedIndex(t, db, model, dim)
	applyAt(t, db, index, seq)
	generation := statelog.Position{Stream: "S", Generation: 1, Seq: seq}.Packed()
	for n := range index.Index.Rollout {
		seq++
		applyAt(t, db, reassignRecord(generation, n, len(index.Index.Rollout)), seq)
	}
	return db, generation
}

// applyAt applies one record through the applier at seq.
func applyAt(t *testing.T, db *store.DB, rec search.VectorRecord, seq uint64) {
	t.Helper()
	applyAll(t, db, []logged{{seq: seq, rec: rec}})
}

// applyAll applies records through ONE applier, in order, each at its own
// sequence — one transaction for the lot, as a catching-up node's loop
// batches them.
func applyAll(t *testing.T, db *store.DB, entries []logged) {
	t.Helper()
	applier := search.NewApplier()
	if err := storetest.EstateOf(db).Tx(t.Context(), func(tx *sql.Tx) error {
		for _, entry := range entries {
			payload, err := entry.rec.Encode()
			if err != nil {
				return fmt.Errorf("encode the record at %d: %w", entry.seq, err)
			}
			if _, err := applier.Apply(t.Context(), tx, statelog.Record{
				Position: statelog.Position{Stream: "S", Generation: 1, Seq: entry.seq},
				Payload:  payload,
			}, statelog.ApplyOptions{}); err != nil {
				return fmt.Errorf("apply the record at %d: %w", entry.seq, err)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

type installed struct {
	generation    int64
	lists, probes int
	centroids     []byte
}

func indexOf(t *testing.T, db *store.DB) installed {
	t.Helper()
	var out installed
	if err := storetest.EstateOf(db).Read(t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(t.Context(), `
			SELECT h.generation, h.lists, h.probes, COALESCE(c.centroids, x'')
			FROM kb_ivf h LEFT JOIN kb_ivf_centroids c ON c.id = 1
			WHERE h.id = 1`).Scan(&out.generation, &out.lists, &out.probes,
			&out.centroids)
	}); err != nil {
		t.Fatalf("read the installed index: %v", err)
	}
	return out
}

// filedRows is every row's key and where it is filed, in key order.
func filedRows(t *testing.T, db *store.DB) []string {
	t.Helper()
	return queryStrings(t, db, `
		SELECT source || ':' || source_id || '@' || ivf_gen || '/' || ivf_list
		FROM kb_vectors_bin ORDER BY source, source_id`)
}

// listCounts is kb_ivf_lists, and recountLists what it must equal: a count of
// the rows filed under the installed generation.
func listCounts(t *testing.T, db *store.DB) []string {
	t.Helper()
	return queryStrings(t, db, `
		SELECT list || '/' || source || '=' || filed FROM kb_ivf_lists
		ORDER BY list, source`)
}

func recountLists(t *testing.T, db *store.DB) []string {
	t.Helper()
	return queryStrings(t, db, `
		SELECT b.ivf_list || '/' || b.source || '=' || COUNT(*)
		FROM kb_vectors_bin b JOIN kb_ivf h ON h.id = 1 AND h.lists > 0
		  AND b.ivf_gen = h.generation
		GROUP BY b.ivf_list, b.source ORDER BY b.ivf_list, b.source`)
}

func queryStrings(t *testing.T, db *store.DB, query string) []string {
	t.Helper()
	var out []string
	if err := storetest.EstateOf(db).Read(t.Context(), func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(t.Context(), query)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				return err
			}
			out = append(out, s)
		}
		return rows.Err()
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

type codedRow struct {
	key        string
	generation int64
	list       int
	code       []uint64
}

func rowsWithCodes(t *testing.T, db *store.DB, dim int) []codedRow {
	t.Helper()
	var out []codedRow
	if err := storetest.EstateOf(db).Read(t.Context(), func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(t.Context(), `
			SELECT source, source_id, ivf_gen, ivf_list, bits FROM kb_vectors_bin`)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var r codedRow
			var source, id string
			var blob []byte
			if err := rows.Scan(&source, &id, &r.generation, &r.list, &blob); err != nil {
				return err
			}
			r.key = source + ":" + id
			if r.code, err = search.CodeFromBits(blob, dim); err != nil {
				return err
			}
			out = append(out, r)
		}
		return rows.Err()
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

// corpusAndKeys is the space's training corpus in key order, and each row's
// key.
func corpusAndKeys(t *testing.T, db *store.DB, model string, dim int) (*search.TrainingCorpus, []string) {
	t.Helper()
	reading := readingOf(t, db, model, dim)
	var keys []string
	for _, source := range []search.Source{search.SourcePage, search.SourceTask} {
		for _, id := range reading.IDs[source] {
			keys = append(keys, search.Key(source, id))
		}
	}
	sort.SliceStable(keys, func(a, b int) bool { return keyLess(keys[a], keys[b]) })
	return &reading.Corpus, keys
}

// containerIndex is the number the training reading gave a container: the
// order containers first appear in, reading the rows in key order.
func containerIndex(t *testing.T, db *store.DB, container string) int32 {
	t.Helper()
	seen := map[string]int32{}
	for _, c := range queryStrings(t, db, `SELECT container FROM kb_vectors_bin
		ORDER BY source, source_id`) {
		if _, ok := seen[c]; !ok {
			seen[c] = int32(len(seen))
		}
	}
	at, ok := seen[container]
	if !ok {
		t.Fatalf("no row is filed in container %q", container)
	}
	return at
}

func keyLess(a, b string) bool {
	as, aid, _ := cut(a)
	bs, bid, _ := cut(b)
	if as != bs {
		return as < bs
	}
	return aid < bid
}

func cut(key string) (string, string, bool) {
	for i := range len(key) {
		if key[i] == ':' {
			return key[:i], key[i+1:], true
		}
	}
	return key, "", false
}

// stage1 runs the first stage and returns its candidates' keys and report.
func stage1(t *testing.T, db *store.DB, q search.SemanticQuery) ([]string, search.Stage1Report) {
	t.Helper()
	var out []string
	var report search.Stage1Report
	if err := storetest.EstateOf(db).Read(t.Context(), func(tx *sql.Tx) error {
		candidates, r, err := search.Stage1(t.Context(), tx, q)
		report = r
		for _, c := range candidates {
			out = append(out, search.Key(c.Source, c.ID))
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return out, report
}

// decodeCentroids reads an index's centroid words WITHOUT the package's own
// decoder, so the Hamming check below is independent of what it checks.
func decodeCentroids(blob []byte, dim, lists int) [][]uint64 {
	words := (dim + 63) / 64
	out := make([][]uint64, lists)
	for j := range out {
		out[j] = make([]uint64, words)
		for w := range words {
			out[j][w] = binary.LittleEndian.Uint64(blob[8*(j*words+w):])
		}
	}
	return out
}

// nearestByPopcount is the lowest list whose centroid differs from code in
// the fewest bits — written out here rather than taken from the package.
func nearestByPopcount(code []uint64, centroids [][]uint64) int {
	best, list := math.MaxInt, 0
	for j, c := range centroids {
		d := 0
		for w := range c {
			d += bits.OnesCount64(c[w] ^ code[w])
		}
		if d < best {
			best, list = d, j
		}
	}
	return list
}

func firstDifference(got, want []string) string {
	for i := range min(len(got), len(want)) {
		if got[i] != want[i] {
			return fmt.Sprintf("at %d, %q against %q", i, got[i], want[i])
		}
	}
	return fmt.Sprintf("lengths %d and %d", len(got), len(want))
}

// topicCentres are k random directions a topical corpus clusters around.
func topicCentres(rng *rand.Rand, k, dim int) [][]float32 {
	out := make([][]float32, k)
	for i := range out {
		c := make([]float32, dim)
		for d := range c {
			c[d] = float32(rng.NormFloat64())
		}
		out[i] = c
	}
	return out
}

// topicalEmbedding is one document of a topical corpus: a centre plus spread
// of noise, packed.
func topicalEmbedding(rng *rand.Rand, centres [][]float32, spread float64) []byte {
	centre := centres[rng.IntN(len(centres))]
	v := make([]float32, len(centre))
	for d := range v {
		v[d] = centre[d] + float32(spread*rng.NormFloat64())
	}
	return pack(v)
}

func unpackFloats(b []byte) []float32 {
	out := make([]float32, len(b)/4)
	for i := range out {
		out[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[4*i:]))
	}
	return out
}

// filedIn is every code's list in index, as the duty's training files them.
func filedIn(t testing.TB, index search.IVF, codes search.Codes) []int32 {
	t.Helper()
	filed, err := index.Assign(context.Background(), codes)
	if err != nil {
		t.Fatalf("file %d codes: %v", codes.Len(), err)
	}
	return filed
}
