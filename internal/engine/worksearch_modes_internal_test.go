package engine

import (
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"math"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/knowledge"
	"github.com/crewlet/crewlet/internal/providers/embeddings"
	"github.com/crewlet/crewlet/internal/search"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/tracker"
)

// The item search's mode, through the wiring this package owns.
//
// `internal/search` certifies that a fan-out fuses the rankers a mode names,
// and the query layer certifies that it hands the mode down. Neither reaches
// the one line between them — [itemRanker.RankItems] building the fan-out's
// query — so every `work_search`, and the dashboard's mode control over it,
// could rank hybrid whatever was asked while both suites stayed green. These
// cases stand the real chain up: a [tracker.Searcher] over [itemRanker] over a
// [search.FanOut] with a [search.NodeScanner] on an indexed task corpus.

const (
	modeTestModel = "m1"
	modeTestDim   = 16
	modeTestQuery = "backoff gateway"
)

// modeTestStore opens both estates of one node.
func modeTestStore(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "node.db"),
		store.Options{})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// modeTestTask writes one applied work item.
func modeTestTask(t *testing.T, db *store.DB, id, title, body string) {
	t.Helper()
	if _, err := db.Replicated().SQL().ExecContext(t.Context(), `
		INSERT INTO tracker_tasks (id, key, project_key, root_id, type, title,
		                           status, status_group, rank, document,
		                           version, created_at, updated_at)
		VALUES (?, ?, 'ENG', ?, 'task', ?, 'todo', 'not_started', 'a0',
		        json_object('body', ?), 1, 0, 0)`,
		id, id, id, title, body); err != nil {
		t.Fatalf("insert task %s: %v", id, err)
	}
}

// modeTestVector applies one task's embedding through the domain's own
// applier, so what the scan reads is what a committed record writes.
func modeTestVector(t *testing.T, db *store.DB, seq uint64, id string, v []float32) {
	t.Helper()
	packed := make([]byte, 4*len(v))
	for i, f := range v {
		binary.LittleEndian.PutUint32(packed[4*i:], math.Float32bits(f))
	}
	subject := search.Subject{Source: search.SourceTask, ID: id}
	rec := search.VectorRecord{
		RecordEnvelope: search.RecordEnvelope{
			Subject: subject, Op: search.OpEmbed, Gen: 1,
			CreatedAt: time.Unix(1_700_000_000, 0).UTC(),
			Scope: statelog.ScopeSet{
				Paths: []string{search.ScopePath("ENG", subject)},
			},
		},
		Container: "ENG", Model: modeTestModel, Dim: modeTestDim,
		Embedding: packed,
	}
	payload, err := rec.Encode()
	if err != nil {
		t.Fatalf("encode vector %s: %v", id, err)
	}
	if err := db.Replicated().Tx(t.Context(), func(tx *sql.Tx) error {
		_, err := search.NewApplier().Apply(t.Context(), tx, statelog.Record{
			Position: statelog.Position{Stream: "S", Generation: 1, Seq: seq},
			Payload:  payload,
		}, statelog.ApplyOptions{})
		return err
	}); err != nil {
		t.Fatalf("apply vector %s: %v", id, err)
	}
}

// modeTestEmbed is the fake provider's vector for text.
func modeTestEmbed(t *testing.T, fake *embeddings.Fake, text string) []float32 {
	t.Helper()
	v, err := fake.Embed(t.Context(), text)
	if err != nil {
		t.Fatalf("embed %q: %v", text, err)
	}
	return v
}

// modeTestSearcher is the production chain over db and index, with vectors
// as the node's query-vector cache (nil is a node with no provider).
func modeTestSearcher(db *store.DB, index *search.Indexer, vectors *search.QueryVectors) *tracker.Searcher {
	return tracker.NewSearcher(db, itemRanker{
		index: index,
		fan: &search.FanOut{
			Self:    "n1",
			Local:   search.NodeScanner{Index: index},
			Vectors: vectors,
		},
	})
}

// modeTestSettle drives the indexer until two sweeps in a row find nothing.
func modeTestSettle(t *testing.T, index *search.Indexer) {
	t.Helper()
	quiet := 0
	for range 200 {
		worked, err := index.Sweep(t.Context())
		if err != nil {
			t.Fatalf("index sweep: %v", err)
		}
		if worked {
			quiet = 0
			continue
		}
		if quiet++; quiet == 2 {
			return
		}
	}
	t.Fatal("the indexer never settled")
}

func hitIDs(answer tracker.SearchAnswer) []string {
	out := make([]string, 0, len(answer.Hits))
	for _, h := range answer.Hits {
		out = append(out, h.ID)
	}
	return out
}

// EVERY MODE REACHES THE FAN-OUT, and each answer serves the mode asked.
//
// Two tasks, built so the rankings disagree: `t.words` carries the query's
// words and a vector far from it, `t.meaning` carries none of the words and
// the query's own vector. Keyword finds the first alone, semantic puts the
// second first, and hybrid holds both — so a ranker that dropped the mode on
// the way to the fan-out answers hybrid to all three and fails every one.
func TestWorkSearchHonoursTheModeThroughTheFanOut(t *testing.T) {
	t.Parallel()
	db := modeTestStore(t)
	fake := embeddings.NewFake(modeTestDim)
	modeTestTask(t, db, "t.words", "Flaky checkout",
		"the payment client retries with no backoff and hammers the gateway")
	modeTestTask(t, db, "t.meaning", "Payment resilience",
		"outbound calls to the processor should degrade gracefully")
	modeTestVector(t, db, 1, "t.words", modeTestEmbed(t, fake, "sailing regatta schedule"))
	modeTestVector(t, db, 2, "t.meaning", modeTestEmbed(t, fake, modeTestQuery))
	index := search.NewIndexerOver(db, []search.LexicalSource{search.TaskSource{}})
	modeTestSettle(t, index)

	var provider embeddings.Embedder = fake
	searcher := modeTestSearcher(db, index, search.NewQueryVectors(
		func() (embeddings.Embedder, string, bool) { return provider, modeTestModel, true }))

	for _, tc := range []struct {
		mode  knowledge.Mode
		check func([]string) bool
		want  string
	}{
		{knowledge.ModeKeyword, func(ids []string) bool {
			return slices.Equal(ids, []string{"t.words"})
		}, "the task carrying the words, alone"},
		{knowledge.ModeSemantic, func(ids []string) bool {
			return len(ids) > 0 && ids[0] == "t.meaning"
		}, "the task carrying the query's meaning first"},
		{knowledge.ModeHybrid, func(ids []string) bool {
			return slices.Contains(ids, "t.words") && slices.Contains(ids, "t.meaning")
		}, "both tasks"},
	} {
		answer, err := searcher.Search(t.Context(), tracker.SearchQuery{
			Text: modeTestQuery, Mode: tc.mode,
		})
		if err != nil {
			t.Fatalf("%s: search: %v", tc.mode, err)
		}
		if answer.ServedMode != tc.mode || answer.Degraded != knowledge.NotDegraded {
			t.Errorf("%s: served %q degraded %q — the mode asked did not reach "+
				"the fan-out", tc.mode, answer.ServedMode, answer.Degraded)
		}
		if ids := hitIDs(answer); !tc.check(ids) {
			t.Errorf("%s answered %v, want %s", tc.mode, ids, tc.want)
		}
		if !slices.Equal(answer.Modes, knowledge.Modes) {
			t.Errorf("%s: modes on offer %v, want all three with a provider",
				tc.mode, answer.Modes)
		}
	}
}

// WITH NO PROVIDER, SEMANTIC SERVES NOTHING AND SAYS WHY — even while the
// lexical index is still building.
//
// The building gate is the LEXICAL index's state. A semantic search never read
// that index, so an empty semantic answer on a building node is not "wait for
// the index": it is "there is nothing to rank meaning with", and answering the
// first threw the second away. A hybrid search on the same node DID read the
// lexical index, and its empty answer is still refused as building.
func TestASemanticWorkSearchWithNoProviderSaysSoEvenWhileBuilding(t *testing.T) {
	t.Parallel()
	db := modeTestStore(t)
	modeTestTask(t, db, "t.words", "Flaky checkout",
		"the payment client retries with no backoff and hammers the gateway")
	// NEVER SWEPT: the index has not finished its first lap.
	index := search.NewIndexerOver(db, []search.LexicalSource{search.TaskSource{}})
	if index.Ready() {
		t.Fatal("an index that was never swept reports itself ready")
	}

	failing := embeddings.Embedder(failingEmbedder{embeddings.NewFake(modeTestDim)})
	for _, tc := range []struct {
		name    string
		vectors *search.QueryVectors
		want    knowledge.Degradation
	}{
		{"no provider", nil, knowledge.DegradedNoEmbeddings},
		{"the provider failed", search.NewQueryVectors(
			func() (embeddings.Embedder, string, bool) { return failing, modeTestModel, true }),
			knowledge.DegradedEmbeddingFailed},
	} {
		searcher := modeTestSearcher(db, index, tc.vectors)
		answer, err := searcher.Search(t.Context(), tracker.SearchQuery{
			Text: modeTestQuery, Mode: knowledge.ModeSemantic,
		})
		if err != nil {
			t.Fatalf("%s: a semantic search failed with %v — the answer that "+
				"explains its emptiness was thrown away", tc.name, err)
		}
		if answer.ServedMode != "" || len(answer.Hits) != 0 || answer.Degraded != tc.want {
			t.Errorf("%s: served %q with %v degraded %q, want nothing served and %q",
				tc.name, answer.ServedMode, hitIDs(answer), answer.Degraded, tc.want)
		}

		if _, err := searcher.Search(t.Context(), tracker.SearchQuery{
			Text: modeTestQuery,
		}); !errors.Is(err, tracker.ErrIndexBuilding) {
			t.Errorf("%s: a hybrid search on a building index answered %v, "+
				"want ErrIndexBuilding", tc.name, err)
		}
	}
}

// failingEmbedder is a provider at a real width whose every call fails.
type failingEmbedder struct{ *embeddings.Fake }

func (failingEmbedder) Embed(context.Context, string) ([]float32, error) {
	return nil, errors.New("the provider is rate limiting")
}
