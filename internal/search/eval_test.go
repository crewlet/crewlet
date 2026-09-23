package search_test

import (
	"database/sql"
	"fmt"
	"math"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/search"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// THE EVALUATION MEASURES THE COMPANY'S OWN VECTORS, and it can fail.
//
// The deterministic gate in this package measures the arithmetic on a seeded
// fixture. This is the other half — the one that answers "is semantic search
// working for US" — and its whole value is that the answer comes from the
// store rather than from a constant. So the assertion is that a healthy corpus
// clears its floor with no head misses, and that a candidate pool too small to
// hold the answer FAILS: an evaluation that cannot report a bad search is a
// number nobody should act on.
func TestTheEvaluationClearsItsFloorAndCanFail(t *testing.T) {
	t.Parallel()
	db, dim, model := seedVectors(t, 300)

	var healthy, starved search.EvalReport
	if err := db.Replicated().Read(t.Context(), func(tx *sql.Tx) error {
		var err error
		healthy, err = search.Eval(t.Context(), tx, search.EvalOptions{
			Model: model, Dim: dim, Queries: 10, Limit: 20,
		})
		if err != nil {
			return err
		}
		// A CANDIDATE POOL THE SIZE OF THE ANSWER is the configuration
		// that measured 0.40 recall when it was tried: the rerank has
		// nothing to choose between, so the sign code IS the ranking.
		starved, err = search.Eval(t.Context(), tx, search.EvalOptions{
			Model: model, Dim: dim, Queries: 10, Limit: 20, Candidates: 20,
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}

	if healthy.Sources != 300 {
		t.Fatalf("the report counts %d sources over a corpus of 300", healthy.Sources)
	}
	if healthy.Model != model || healthy.Dim != dim {
		t.Fatalf("the report describes %s at %d", healthy.Model, healthy.Dim)
	}
	if !healthy.Passed() {
		t.Fatalf("a healthy corpus scored %.4f against a floor of %.4f with %d "+
			"head misses", healthy.Recall, healthy.Floor, healthy.HeadMisses)
	}
	if starved.Recall >= healthy.Recall {
		t.Fatalf("a candidate pool the size of the answer scored %.4f and the "+
			"shipped depth scored %.4f — an evaluation that cannot tell those "+
			"apart cannot report a search that has stopped working",
			starved.Recall, healthy.Recall)
	}
	if starved.Passed() {
		t.Fatalf("a search with no rerank depth at all passed, at %.4f against "+
			"a floor of %.4f", starved.Recall, starved.Floor)
	}
	// AND THE MEAN COSINE IS MEASURED rather than left at zero, because it
	// is what a fitting run re-derives the fixture's pinned constant from.
	if healthy.MeanPairCos == 0 {
		t.Fatal("the corpus's mean pairwise cosine came back zero")
	}
}

// THE REPORT NAMES THE DEPTHS THE SEARCH RAN AT, not the ones it was asked for.
//
// A candidate pool smaller than the answer is raised to it, so a report that
// echoed the request would print a stage-one depth nothing was measured at —
// and an operator raising the depth to test a remedy reads the recall against
// that number. A depth far past the shipped one is measured as asked, too:
// the report is only worth reading if nothing between it and the scan
// changed the depth without saying so.
func TestTheEvaluationReportsTheDepthsItRanAt(t *testing.T) {
	t.Parallel()
	db, dim, model := seedVectors(t, 60)

	var raised, deep search.EvalReport
	if err := db.Replicated().Read(t.Context(), func(tx *sql.Tx) error {
		var err error
		raised, err = search.Eval(t.Context(), tx, search.EvalOptions{
			Model: model, Dim: dim, Queries: 3, Limit: 20, Candidates: 10,
		})
		if err != nil {
			return err
		}
		deep, err = search.Eval(t.Context(), tx, search.EvalOptions{
			Model: model, Dim: dim, Queries: 3, Limit: 20, Candidates: 9_000,
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if raised.Limit != 20 || raised.Candidates != 20 {
		t.Fatalf("asked for 10 candidates under a limit of 20, the report says "+
			"%d at depth %d — the scan ran 20, because a pool smaller than the "+
			"answer cannot fill it", raised.Candidates, raised.Limit)
	}
	if deep.Candidates != 9_000 {
		t.Fatalf("asked for 9 000 candidates, the report says %d", deep.Candidates)
	}
	// AND IT RAN AT THAT DEPTH: a pool larger than the corpus holds every
	// document, so the rerank is the exact scan and recalls all of it.
	if deep.Recall != 1 {
		t.Fatalf("a candidate pool larger than the corpus recalled %.4f of the "+
			"exact answer", deep.Recall)
	}
}

// A REFILL IN PROGRESS IS VISIBLE, because until it finishes a search over the
// other space returns nothing at all — the scan filters on the pair.
func TestTheEvaluationNamesEveryEmbeddingSpace(t *testing.T) {
	t.Parallel()
	db, dim, model := seedVectors(t, 40)
	writeVectors(t, db, "half-refilled", dim, 10, 100, 1000)

	var spaces []search.Space
	var report search.EvalReport
	if err := db.Replicated().Read(t.Context(), func(tx *sql.Tx) error {
		var err error
		if spaces, err = search.SpacesIn(t.Context(), tx); err != nil {
			return err
		}
		report, err = search.Eval(t.Context(), tx, search.EvalOptions{
			Queries: 5, Limit: 10,
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(spaces) != 2 {
		t.Fatalf("%d embedding space(s) reported over a corpus holding two", len(spaces))
	}
	if spaces[0].Model != model || spaces[0].Sources != 40 {
		t.Fatalf("the largest space is %q with %d sources", spaces[0].Model,
			spaces[0].Sources)
	}
	// AND AN EVALUATION THAT WAS NOT TOLD WHICH takes the most populated
	// pair, because measuring the ten rows of a refill that has just
	// started would report a number about nothing.
	if report.Model != model || report.Sources != 40 {
		t.Fatalf("an unqualified evaluation measured %q over %d sources",
			report.Model, report.Sources)
	}
}

// THE MEAN COSINE IS A SAMPLE OF THE WHOLE CORPUS, not of whichever source
// sorts first.
//
// The key order leads with the source, so every page sorts before any task.
// Here the pages sit close together and the tasks point anywhere, so a sample
// drawn from the head of the key order compares every query with pages and
// reads a corpus whose pairs are mostly unrelated as one whose pairs are
// mostly alike. Seven hundred documents against a sample of four hundred is
// also where an integer stride truncates to one, which reaches the same head
// of the key order by a different route.
//
// The reference is every pair, computed here: the estimate is only a number
// worth fitting a constant from if it lands near that.
func TestTheMeanCosineIsASampleOfTheWholeCorpus(t *testing.T) {
	t.Parallel()
	const dim, model = 32, "text-embedding-3-large"
	rng := rand.New(rand.NewPCG(5, 5))
	var docs []evalDoc
	for i := range 350 {
		v := make([]float32, dim)
		v[0] = 1
		for k := range v {
			v[k] += float32(0.05 * rng.NormFloat64())
		}
		docs = append(docs, evalDoc{source: search.SourcePage,
			id: fmt.Sprintf("p%04d", i), vector: v})
	}
	for i := range 350 {
		v := make([]float32, dim)
		for k := range v {
			v[k] = float32(rng.NormFloat64())
		}
		docs = append(docs, evalDoc{source: search.SourceTask,
			id: fmt.Sprintf("t%04d", i), vector: v})
	}
	db := openReplicated(t)
	writeEvalCorpus(t, db, model, dim, docs)

	var everyPair float64
	var pairs int
	for i, a := range docs {
		for j, b := range docs {
			if i == j {
				continue
			}
			everyPair += cosine(a.vector, b.vector)
			pairs++
		}
	}
	everyPair /= float64(pairs)

	var report search.EvalReport
	if err := db.Replicated().Read(t.Context(), func(tx *sql.Tx) error {
		var err error
		report, err = search.Eval(t.Context(), tx, search.EvalOptions{
			Model: model, Dim: dim, Limit: 10,
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if math.Abs(report.MeanPairCos-everyPair) > 0.05 {
		t.Fatalf("the evaluation estimates the mean pairwise cosine at %.4f and "+
			"every pair of the corpus averages %.4f — a sample of the head of the "+
			"key order compares every query with pages alone, and a fixture "+
			"fitted from it describes a different corpus",
			report.MeanPairCos, everyPair)
	}
}

// evalDoc is one document of a corpus a test lays out by hand.
type evalDoc struct {
	source search.Source
	id     string
	vector []float32
}

// writeEvalCorpus fills the corpus through the APPLIER, on [writeVectors]'
// terms, with vectors the test chose rather than random ones.
func writeEvalCorpus(t *testing.T, db *store.DB, model string, dim int, docs []evalDoc) {
	t.Helper()
	applier := search.NewApplier()
	if err := db.Replicated().Tx(t.Context(), func(tx *sql.Tx) error {
		for i, doc := range docs {
			subject := search.Subject{Source: doc.source, ID: doc.id}
			rec := search.VectorRecord{
				RecordEnvelope: search.RecordEnvelope{
					Subject: subject, Op: search.OpEmbed, Gen: 1,
					CreatedAt: time.Unix(1_700_000_000, 0).UTC(),
					Scope: statelog.ScopeSet{
						Paths: []string{search.ScopePath("ENG", subject)},
					},
				},
				Container: "ENG", Model: model, Dim: dim,
				Embedding: pack(doc.vector),
			}
			payload, err := rec.Encode()
			if err != nil {
				return err
			}
			if _, err := applier.Apply(t.Context(), tx, statelog.Record{
				Position: statelog.Position{Stream: "S", Generation: 1, Seq: uint64(i) + 1},
				Payload:  payload,
			}, statelog.ApplyOptions{}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
}

func cosine(a, b []float32) float64 {
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	return dot / math.Sqrt(na*nb)
}
