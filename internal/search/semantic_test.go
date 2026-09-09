package search_test

import (
	"context"
	"database/sql"
	"encoding/binary"
	"fmt"
	"math"
	"math/rand/v2"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/search"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// THE TWO-STAGE SEARCH AGREES WITH THE EXACT SCAN AT THE SHIPPED DEPTH.
//
// The pure gate in this package measures the arithmetic over slices; this
// measures the SQL that runs it, which is a different thing that can be wrong
// on its own — a predicate on the wrong table, a tie break nobody declared, a
// query code computed per row instead of once. The corpus is small enough that
// the candidate pool covers it, so agreement is exact and any disagreement is
// the statement rather than the quantization.
func TestTheTwoStageScanAgreesWithTheExactRanking(t *testing.T) {
	t.Parallel()
	db, dim, model := seedVectors(t, 400)
	rng := rand.New(rand.NewPCG(9, 9))
	query := randomEmbedding(rng, dim)

	var got, want []string
	if err := db.Replicated().Read(t.Context(), func(tx *sql.Tx) error {
		hits, err := search.Semantic(t.Context(), tx, search.SemanticQuery{
			Vector: query, Model: model, Dim: dim, Limit: 20,
		})
		if err != nil {
			return err
		}
		for _, h := range hits {
			got = append(got, search.Key(h.Source, h.ID))
		}
		want, err = exactRanking(t.Context(), tx, query, dim, 20)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(got) != 20 {
		t.Fatalf("the two-stage scan returned %d hits, want 20", len(got))
	}
	if !slices.Equal(got, want) {
		t.Fatalf("the two-stage scan ranked\n  %v\nand the exact scan ranks\n  %v",
			got, want)
	}
}

// THE PREDICATE EXCLUDES ANOTHER EMBEDDING SPACE, and this is the case
// `model` exists for.
//
// A model change at the SAME width leaves both models' vectors in one
// candidate pool for as long as a refill takes, ranked against each other in
// incompatible spaces. Nothing about the vectors themselves says which is
// which — the widths agree, the distances are finite, and every row looks
// scannable.
func TestASecondModelAtTheSameWidthIsExcluded(t *testing.T) {
	t.Parallel()
	db, dim, model := seedVectors(t, 40)
	// FORTY OTHER SOURCES on a different model at the same width, which is
	// what a refill in progress actually leaves: the duty re-embeds one
	// source at a time, so for the hours it runs the table holds both
	// spaces on different rows and nothing about a row says which is which
	// — the widths agree, the distances are finite, and every row looks
	// scannable.
	writeVectors(t, db, "other-model", dim, 40, 100, 1000)

	if err := db.Replicated().Read(t.Context(), func(tx *sql.Tx) error {
		rng := rand.New(rand.NewPCG(3, 3))
		hits, err := search.Semantic(t.Context(), tx, search.SemanticQuery{
			Vector: randomEmbedding(rng, dim), Model: model, Dim: dim, Limit: 100,
		})
		if err != nil {
			return err
		}
		if len(hits) != 40 {
			return fmt.Errorf("the scan returned %d hits over a corpus of 40 "+
				"documents embedded by this model and 40 by another — a pool "+
				"holding two embedding spaces ranks arithmetic on incompatible "+
				"vectors", len(hits))
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// A CONTAINER FILTER AND A SOURCE FILTER NARROW THE CANDIDATE POOL ITSELF.
//
// Both are on the NARROW table, which is the only place they do any good: on
// the wide one they would filter after the scan they exist to shrink.
func TestTheScopeFiltersNarrowTheCandidatePool(t *testing.T) {
	t.Parallel()
	db, dim, model := seedVectors(t, 60)
	rng := rand.New(rand.NewPCG(11, 11))
	query := randomEmbedding(rng, dim)

	if err := db.Replicated().Read(t.Context(), func(tx *sql.Tx) error {
		hits, err := search.Semantic(t.Context(), tx, search.SemanticQuery{
			Vector: query, Model: model, Dim: dim, Limit: 100,
			Containers: []string{"ENG"},
		})
		if err != nil {
			return err
		}
		for _, h := range hits {
			if h.Container != "ENG" {
				return fmt.Errorf("a hit from container %q survived a filter "+
					"on ENG", h.Container)
			}
		}
		if len(hits) == 0 || len(hits) == 60 {
			return fmt.Errorf("the container filter returned %d of 60 hits, "+
				"so the fixture cannot tell a working filter from an absent "+
				"one", len(hits))
		}
		pages, err := search.Semantic(t.Context(), tx, search.SemanticQuery{
			Vector: query, Model: model, Dim: dim, Limit: 100,
			Sources: []search.Source{search.SourcePage},
		})
		if err != nil {
			return err
		}
		for _, h := range pages {
			if h.Source != search.SourcePage {
				return fmt.Errorf("a %s hit survived a filter on pages", h.Source)
			}
		}
		if len(pages) == 0 || len(pages) == 60 {
			return fmt.Errorf("the source filter returned %d of 60 hits", len(pages))
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// THE ANSWER IS ORDERED BY THE EXACT DISTANCE, never by the quantized one.
//
// The rerank's whole purpose: a sign code decides which documents are looked
// at and never how they are ordered. A statement that returned the stage-1
// order would still look plausible — the same documents, a similar order — and
// would silently ship 0.40 recall, which is what a 1-bit ranking with no
// rerank measures.
func TestTheAnswerIsOrderedByTheExactDistance(t *testing.T) {
	t.Parallel()
	db, dim, model := seedVectors(t, 200)
	rng := rand.New(rand.NewPCG(5, 5))
	if err := db.Replicated().Read(t.Context(), func(tx *sql.Tx) error {
		hits, err := search.Semantic(t.Context(), tx, search.SemanticQuery{
			Vector: randomEmbedding(rng, dim), Model: model, Dim: dim, Limit: 30,
		})
		if err != nil {
			return err
		}
		previous := -1.0
		for i, h := range hits {
			if h.Distance < previous {
				return fmt.Errorf("hit %d is at distance %v, behind %v",
					i, h.Distance, previous)
			}
			previous = h.Distance
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// THE WRITE IS ONE PAIR OR NEITHER, which is what answers the objection that
// `model` and `dim` on the narrow table are a fact that can disagree.
//
// A redelivery must move neither row, and a newer record must move both. There
// is no window in which one is written and the other is not, because the guard
// on the narrow row is the wide row's own.
func TestTheTwoRowsMoveTogetherOrNotAtAll(t *testing.T) {
	t.Parallel()
	db := openReplicated(t)
	applier := search.NewApplier()
	subject := search.Subject{Source: search.SourcePage, ID: "p-lockstep"}

	apply := func(seq uint64, model string, dim int, v []float32) {
		t.Helper()
		rec := search.VectorRecord{
			RecordEnvelope: search.RecordEnvelope{
				Subject: subject, Op: search.OpEmbed, Gen: 1,
				CreatedAt: time.Unix(1_700_000_000, 0).UTC(),
				Scope: statelog.ScopeSet{
					Paths: []string{search.ScopePath("ENG", subject)},
				},
			},
			Container: "ENG", Model: model, Dim: dim, Embedding: pack(v),
		}
		payload, err := rec.Encode()
		if err != nil {
			t.Fatal(err)
		}
		if err := db.Replicated().Tx(t.Context(), func(tx *sql.Tx) error {
			_, err := applier.Apply(t.Context(), tx, statelog.Record{
				Envelope: statelog.Envelope{Subject: statelog.Subject{
					Kind: string(subject.Source), ID: subject.ID}},
				Position: statelog.Position{Stream: "S", Generation: 1, Seq: seq},
				Payload:  payload,
			}, statelog.ApplyOptions{})
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}

	apply(10, "model-a", 4, []float32{1, 1, 1, 1})
	// A REDELIVERY at a lower position: neither row moves.
	apply(5, "model-b", 4, []float32{-1, -1, -1, -1})
	assertPair(t, db, subject, "model-a")
	// A NEWER RECORD: both move.
	apply(20, "model-b", 4, []float32{-1, -1, -1, -1})
	assertPair(t, db, subject, "model-b")

	// AND A FORGET REMOVES BOTH.
	forget := search.VectorRecord{
		RecordEnvelope: search.RecordEnvelope{
			Subject: subject, Op: search.OpForget, Gen: 1,
			Scope: statelog.ScopeSet{
				Paths: []string{search.ScopePath("ENG", subject)},
			},
		},
	}
	payload, err := forget.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Replicated().Tx(t.Context(), func(tx *sql.Tx) error {
		_, err := applier.Apply(t.Context(), tx, statelog.Record{
			Position: statelog.Position{Stream: "S", Generation: 1, Seq: 30},
			Payload:  payload,
		}, statelog.ApplyOptions{})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	assertPair(t, db, subject, "")
}

// assertPair reads both rows and reports the model they agree on, or fails
// when they do not exist together.
func assertPair(t *testing.T, db *store.DB, subject search.Subject, want string) {
	t.Helper()
	var wide, narrow string
	if err := db.Replicated().Read(t.Context(), func(tx *sql.Tx) error {
		read := func(table string) (string, error) {
			var model string
			err := tx.QueryRowContext(t.Context(),
				`SELECT model FROM `+table+` WHERE source = ? AND source_id = ?`,
				string(subject.Source), subject.ID).Scan(&model)
			if err == sql.ErrNoRows {
				return "", nil
			}
			return model, err
		}
		var err error
		if wide, err = read("kb_vectors"); err != nil {
			return err
		}
		narrow, err = read("kb_vectors_bin")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if wide != want || narrow != want {
		t.Fatalf("kb_vectors holds %q and kb_vectors_bin holds %q, want %q on "+
			"both — the pair is written by one applier in one transaction, so "+
			"a difference is a window that is not supposed to exist",
			wide, narrow, want)
	}
}

// ---- the fixture ------------------------------------------------------ //

func openReplicated(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(t.Context(),
		filepath.Join(t.TempDir(), "node.db"), store.Options{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// seedVectors fills the corpus through the APPLIER rather than by hand, so
// what the search reads is what a committed record would have written.
func seedVectors(t *testing.T, n int) (*store.DB, int, string) {
	t.Helper()
	const dim, model = 32, "text-embedding-3-large"
	db := openReplicated(t)
	writeVectors(t, db, model, dim, n, 0, 0)
	return db, dim, model
}

func writeVectors(t *testing.T, db *store.DB, model string, dim, n int, seqBase uint64, idBase int) {
	t.Helper()
	applier := search.NewApplier()
	rng := rand.New(rand.NewPCG(1, uint64(len(model))))
	if err := db.Replicated().Tx(t.Context(), func(tx *sql.Tx) error {
		for i := range n {
			source := search.SourcePage
			if i%2 == 1 {
				source = search.SourceTask
			}
			container := "ENG"
			if i%3 == 0 {
				container = "OPS"
			}
			subject := search.Subject{Source: source, ID: fmt.Sprintf("s%04d", idBase+i)}
			rec := search.VectorRecord{
				RecordEnvelope: search.RecordEnvelope{
					Subject: subject, Op: search.OpEmbed, Gen: 1,
					CreatedAt: time.Unix(1_700_000_000, 0).UTC(),
					Scope: statelog.ScopeSet{
						Paths: []string{search.ScopePath(container, subject)},
					},
				},
				Container: container, Model: model, Dim: dim,
				Embedding: randomEmbedding(rng, dim),
			}
			payload, err := rec.Encode()
			if err != nil {
				return err
			}
			if _, err := applier.Apply(t.Context(), tx, statelog.Record{
				Position: statelog.Position{
					Stream: "S", Generation: 1, Seq: seqBase + uint64(i) + 1,
				},
				Payload: payload,
			}, statelog.ApplyOptions{}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
}

// exactRanking is the ground truth: one full scan of the wide table, no
// candidate pool at all.
func exactRanking(ctx context.Context, tx *sql.Tx, query []byte, dim, limit int) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT source, source_id
		FROM kb_vectors
		WHERE length(embedding) = ?
		ORDER BY vector_distance_cos(embedding, ?), source, source_id
		LIMIT ?`, 4*dim, query, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var source, id string
		if err := rows.Scan(&source, &id); err != nil {
			return nil, err
		}
		out = append(out, search.Key(search.Source(source), id))
	}
	return out, rows.Err()
}

func randomEmbedding(rng *rand.Rand, dim int) []byte {
	v := make([]float32, dim)
	for i := range v {
		v[i] = float32(rng.NormFloat64())
	}
	return pack(v)
}

func pack(v []float32) []byte {
	out := make([]byte, 4*len(v))
	for i, f := range v {
		binary.LittleEndian.PutUint32(out[4*i:], math.Float32bits(f))
	}
	return out
}
