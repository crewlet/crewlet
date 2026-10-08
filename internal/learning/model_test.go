package learning_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/crewlet/crewlet/internal/learning"
	"github.com/crewlet/crewlet/internal/store"
)

// A VECTOR IS A POINT IN ONE MODEL'S SPACE, and recall compares like with
// like. Two models of one width are two spaces — text-embedding-3-small and
// embed-v4.0 both answer 1 536 floats — so the width filter alone admitted
// both, and a company that switched between them ranked a query from one
// space against rows from the other.

// learningStore is a node store at width 4, the width of every vector these
// tests write.
func learningStore(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.OpenNode(t.Context(), filepath.Join(t.TempDir(), "m.db"),
		store.Options{EmbeddingDim: 4})
	if err != nil {
		t.Fatalf("store.OpenNode: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestEpisodeRecallComparesOnlyVectorsOfTheQuerysModel(t *testing.T) {
	t.Parallel()
	db := learningStore(t)
	e := learning.NewEpisodes(db)
	same := ep("same", "ceo", base)
	same.Embedding = []float32{1, 0, 0, 0}
	// The SAME floats from another model — the strongest possible match by
	// cosine, and no match at all.
	other := ep("other", "ceo", base)
	other.Embedding, other.EmbeddingModel = []float32{1, 0, 0, 0}, "another-model"
	for _, x := range []learning.Episode{same, other} {
		mustAppend(t, e, x)
	}

	hits, err := e.Recall(context.Background(), learning.RecallQuery{
		Handle: "ceo", Embedding: []float32{1, 0, 0, 0}, Model: testModel,
	})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if len(hits) != 1 || hits[0].Episode.ID != "same" {
		t.Fatalf("hits = %v, want only the row from the query's model", hitIDs(hits))
	}
	if hits[0].Episode.EmbeddingModel != testModel {
		t.Errorf("the hit names model %q", hits[0].Episode.EmbeddingModel)
	}
}

func TestDiaryRecallComparesOnlyVectorsOfTheQuerysModel(t *testing.T) {
	t.Parallel()
	db := learningStore(t)
	d := learning.NewDiary(db)
	same := longEntry("same", "a", "the release train is thursdays", base)
	same.Embedding = []float32{1, 0, 0, 0}
	other := longEntry("other", "a", "the release train is thursdays", base)
	other.Embedding, other.EmbeddingModel = []float32{1, 0, 0, 0}, "another-model"
	for _, x := range []learning.DiaryEntry{same, other} {
		mustWrite(t, d, x)
	}

	hits, err := d.Recall(context.Background(), "a", learning.RecallQuery{
		Embedding: []float32{1, 0, 0, 0}, Model: testModel,
	}, base)
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if len(hits) != 1 || hits[0].Entry.ID != "same" {
		t.Fatalf("hits = %v, want only the note from the query's model", diaryHitIDs(hits))
	}
}

// A QUERY THAT DOES NOT NAME ITS MODEL IS HALF A QUERY, refused like one with
// no vector: there is no space to compare it in.
func TestARecallWithoutItsModelIsRefused(t *testing.T) {
	t.Parallel()
	db := learningStore(t)
	if _, err := learning.NewEpisodes(db).Recall(context.Background(), learning.RecallQuery{
		Handle: "ceo", Embedding: []float32{1, 0, 0, 0},
	}); !errors.Is(err, learning.ErrNoEmbedding) {
		t.Errorf("episode recall with no model: err = %v, want ErrNoEmbedding", err)
	}
	if _, err := learning.NewDiary(db).Recall(context.Background(), "a", learning.RecallQuery{
		Embedding: []float32{1, 0, 0, 0},
	}, base); !errors.Is(err, learning.ErrNoEmbedding) {
		t.Errorf("diary recall with no model: err = %v, want ErrNoEmbedding", err)
	}
}

// A VECTOR THAT NAMES NO MODEL COSTS THE VECTOR, NOT THE ROW. It is in no
// space a recall could compare, and failing the write would lose an episode
// or a note over half a fact — so it lands without one, like a non-finite
// vector does.
func TestAnUntaggedVectorIsDiscardedAndTheRowStillLands(t *testing.T) {
	t.Parallel()
	db := learningStore(t)
	e := learning.NewEpisodes(db)
	untagged := ep("a", "ceo", base)
	untagged.Embedding, untagged.EmbeddingModel = []float32{1, 0, 0, 0}, ""
	mustAppend(t, e, untagged)
	got, err := e.Recent(context.Background(), "ceo", 10)
	if err != nil || len(got) != 1 {
		t.Fatalf("episodes = %d, %v; want the row", len(got), err)
	}
	if got[0].Embedding != nil || got[0].EmbeddingModel != "" {
		t.Errorf("an untagged vector was stored: %v in %q", got[0].Embedding, got[0].EmbeddingModel)
	}

	d := learning.NewDiary(db)
	note := longEntry("n", "a", "a fact", base)
	note.Embedding, note.EmbeddingModel = []float32{1, 0, 0, 0}, ""
	mustWrite(t, d, note)
	notes, err := d.Recent(context.Background(), "a", base, 10)
	if err != nil || len(notes) != 1 {
		t.Fatalf("notes = %d, %v; want the row", len(notes), err)
	}
	if notes[0].Embedding != nil {
		t.Errorf("an untagged vector was stored on a note: %v", notes[0].Embedding)
	}
}

// THE MODEL ROUND-TRIPS, on both tables — it is what every later recall
// filters on, so a scanner that dropped it would make every row unreachable.
func TestTheVectorsModelRoundTrips(t *testing.T) {
	t.Parallel()
	db := learningStore(t)
	e := learning.NewEpisodes(db)
	tagged := ep("a", "ceo", base)
	tagged.Embedding = []float32{0.1, 0.2, 0.3, 0.4}
	mustAppend(t, e, tagged)
	got, _ := e.Recent(context.Background(), "ceo", 10)
	if len(got) != 1 || got[0].EmbeddingModel != testModel {
		t.Fatalf("episode = %+v, want model %q", got, testModel)
	}
	d := learning.NewDiary(db)
	note := longEntry("n", "a", "a fact", base)
	note.Embedding = []float32{0.1, 0.2, 0.3, 0.4}
	mustWrite(t, d, note)
	notes, _ := d.Recent(context.Background(), "a", base, 10)
	if len(notes) != 1 || notes[0].EmbeddingModel != testModel {
		t.Fatalf("note = %+v, want model %q", notes, testModel)
	}
}
