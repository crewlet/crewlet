package search_test

import (
	"database/sql"
	"fmt"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/search"
	"github.com/crewlet/crewlet/internal/store/storetest"
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
	if err := storetest.EstateOf(db).Read(t.Context(), func(tx *sql.Tx) error {
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

// A REFILL IN PROGRESS IS VISIBLE, because until it finishes a search over the
// other space returns nothing at all — the scan filters on the pair.
func TestTheEvaluationNamesEveryEmbeddingSpace(t *testing.T) {
	t.Parallel()
	db, dim, model := seedVectors(t, 40)
	writeVectors(t, db, "half-refilled", dim, 10, 100, 1000)

	var spaces []search.Space
	var report search.EvalReport
	if err := storetest.EstateOf(db).Read(t.Context(), func(tx *sql.Tx) error {
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

// THE GROUND TRUTH IS THE REST OF EACH SHAPE'S ROWS: every (query, shape)'s
// exact top-K over the rows that shape searches, and never the query document
// itself.
//
// A query is a document the corpus holds, found by every first stage at
// distance zero: counted, it is a free hit in every query's recall and a slot
// of the top ten no search can miss — an evaluation, and a training measuring
// the same way, would report an index better than the searches it serves. And
// a narrowed shape's truth is its own filter's rows, counted, because its
// floor is read at that count. Held here against the per-query statement the
// one pass replaced, shape by shape.
func TestTheGroundTruthIsTheRestOfEachShapesRows(t *testing.T) {
	t.Parallel()
	const dim, model, limit = 32, "truth-embed", 20
	db, _ := indexedStore(t, model, dim, 400)
	if err := storetest.EstateOf(db).Read(t.Context(), func(tx *sql.Tx) error {
		docs, err := search.SampleDocuments(t.Context(), tx, model, dim, 6)
		if err != nil {
			return err
		}
		if len(docs) != 6 {
			return fmt.Errorf("sampled %d documents, want 6", len(docs))
		}
		shapes := make([][]search.ShapeQuery, len(docs))
		for i, doc := range docs {
			shapes[i] = search.ShapesFor(doc, search.Sources)
		}
		tops, err := search.ExactTops(t.Context(), tx, docs, shapes, model, dim, limit)
		if err != nil {
			return err
		}
		for i, doc := range docs {
			self := search.Key(doc.Source, doc.ID)
			for j, shape := range shapes[i] {
				where := "model = ? AND dim = ? AND NOT (source = ? AND source_id = ?)"
				args := []any{model, dim, string(doc.Source), doc.ID}
				if shape.Shape != search.ShapeAll {
					where += " AND source = ?"
					args = append(args, string(shape.Source))
				}
				if shape.Shape == search.ShapeContainer {
					where += " AND container = ?"
					args = append(args, shape.Container)
				}
				var matching int
				if err := tx.QueryRowContext(t.Context(),
					`SELECT COUNT(*) FROM kb_vectors WHERE `+where, args...).Scan(&matching); err != nil {
					return err
				}
				rows, err := tx.QueryContext(t.Context(), `
					SELECT source || ':' || source_id FROM kb_vectors WHERE `+where+`
					ORDER BY vector_distance_cos(embedding, ?), source, source_id
					LIMIT ?`, append(args, doc.Vector, limit)...)
				if err != nil {
					return err
				}
				var want []string
				for rows.Next() {
					var key string
					if err := rows.Scan(&key); err != nil {
						_ = rows.Close()
						return err
					}
					want = append(want, key)
				}
				_ = rows.Close()
				got := tops[i][j]
				if slices.Contains(got.Keys, self) {
					return fmt.Errorf("query %s counts itself in its own %s truth",
						self, shape.Shape)
				}
				if !slices.Equal(got.Keys, want) || got.Matching != matching {
					return fmt.Errorf("query %s, shape %+v: the one pass found %d "+
						"of %d rows %v, the per-query statement %d of %d %v", self,
						shape, len(got.Keys), got.Matching, got.Keys, len(want),
						matching, want)
				}
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
