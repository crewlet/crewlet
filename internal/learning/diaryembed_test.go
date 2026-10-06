package learning_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/learning"
	"github.com/crewlet/crewlet/internal/providers/embeddings"
)

// fakeEmbed is the learning.Embed seam over the deterministic fake embedder,
// tagged with the fake's model — what the engine hands a writer, minus the
// network.
func fakeEmbed(f *embeddings.Fake) learning.Embed {
	return func(ctx context.Context, text string) (learning.Vector, error) {
		v, err := f.Embed(ctx, text)
		if err != nil {
			return learning.Vector{}, err
		}
		return learning.Vector{Values: v, Model: f.Model()}, nil
	}
}

// A NOTE IS EMBEDDED AS IT IS WRITTEN, and recalled by meaning afterwards.
//
// The end-to-end gap this closes: both writers built a note with no vector,
// the store wrote NULL, and the similarity half of `## Personal memory` read
// only rows with one — so it was empty for every seat that ever lived, and a
// test that seeded vectors by hand certified a path production never took.
func TestAWrittenNoteIsRecalledByMeaning(t *testing.T) {
	t.Parallel()
	db := learningStore(t)
	fake := embeddings.NewFake(4)
	d := learning.NewDiary(db, learning.WithEmbed(fakeEmbed(fake)))
	mustWrite(t, d, learning.DiaryEntry{
		ID: "train", AgentID: "a", Kind: learning.DiaryLong,
		Content: "the release train leaves on thursdays", CreatedAt: base,
	})
	mustWrite(t, d, learning.DiaryEntry{
		ID: "lunch", AgentID: "a", Kind: learning.DiaryLong,
		Content: "pat prefers digests over pings", CreatedAt: base,
	})

	query, err := fake.Embed(context.Background(), "when does the release train leave")
	if err != nil {
		t.Fatalf("embed the query: %v", err)
	}
	hits, err := d.Recall(context.Background(), "a", learning.RecallQuery{
		Embedding: query, Model: fake.Model(),
	}, base)
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if len(hits) == 0 || hits[0].Entry.ID != "train" {
		t.Fatalf("hits = %v, want the note about the release train first", diaryHitIDs(hits))
	}
	if hits[0].Entry.EmbeddingModel != fake.Model() {
		t.Errorf("the note's vector names model %q", hits[0].Entry.EmbeddingModel)
	}
}

// A NOTE WHOSE VECTOR CANNOT BE HAD IS STILL KEPT. What the seat asked to keep
// is the note; its vector only decides whether recall reaches it by meaning,
// and the holder's fill gives it one later.
func TestANoteIsKeptWhenItsVectorCannotBeHad(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		embed learning.Embed
	}{
		{"no embeddings configured", func(context.Context, string) (learning.Vector, error) {
			return learning.Vector{}, learning.ErrNoEmbeddings
		}},
		{"the provider failed", func(context.Context, string) (learning.Vector, error) {
			return learning.Vector{}, errors.New("the provider is down")
		}},
		{"the provider is slower than the budget", func(ctx context.Context, _ string) (learning.Vector, error) {
			<-ctx.Done()
			return learning.Vector{}, ctx.Err()
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := learning.NewDiary(learningStore(t), learning.WithEmbed(tc.embed))
			mustWrite(t, d, longEntry("n", "a", "a fact worth keeping", base))
			got, err := d.Recent(context.Background(), "a", base, 10)
			if err != nil || len(got) != 1 {
				t.Fatalf("notes = %d, %v; want the note", len(got), err)
			}
			if got[0].Embedding != nil {
				t.Errorf("a vector was stored: %v", got[0].Embedding)
			}
		})
	}
}

// A WRITE IS BOUNDED BY ITS BUDGET, not by the provider: reflect_and_persist
// has a model waiting on it.
func TestAWriteWaitsNoLongerThanItsBudgetForAVector(t *testing.T) {
	t.Parallel()
	d := learning.NewDiary(learningStore(t), learning.WithEmbed(
		func(ctx context.Context, _ string) (learning.Vector, error) {
			<-ctx.Done()
			return learning.Vector{}, ctx.Err()
		}))
	began := time.Now()
	mustWrite(t, d, longEntry("n", "a", "a fact worth keeping", base))
	if took := time.Since(began); took > learning.DiaryEmbedBudget+time.Second {
		t.Fatalf("the write waited %v for a vector, past its %v budget", took, learning.DiaryEmbedBudget)
	}
}

// THE FILL FINDS EXACTLY WHAT RECALL CANNOT REACH: a seat's live notes with no
// vector, or with one of another model, or of no model — newest first, and
// never a note of the current model, an expired one, another seat's, or one
// with nothing to embed.
func TestUnembeddedIsWhatRecallCannotReach(t *testing.T) {
	t.Parallel()
	db := learningStore(t)
	d := learning.NewDiary(db)
	at := func(minutes int) time.Time { return base.Add(time.Duration(minutes) * time.Minute) }
	write := func(e learning.DiaryEntry) { mustWrite(t, d, e) }

	write(longEntry("bare", "a", "never embedded", at(1)))
	old := longEntry("old-model", "a", "embedded under the old model", at(2))
	old.Embedding, old.EmbeddingModel = []float32{1, 0, 0, 0}, "old-model"
	write(old)
	legacy := longEntry("legacy", "a", "embedded before vectors named a model", at(3))
	legacy.Embedding = []float32{1, 0, 0, 0}
	write(legacy)
	untag(t, db, "agent_diary", "legacy")
	current := longEntry("current", "a", "embedded under this model", at(4))
	current.Embedding = []float32{1, 0, 0, 0}
	write(current)
	write(learning.DiaryEntry{
		ID: "expired", AgentID: "a", Kind: learning.DiaryShort, Content: "a stale fact",
		CreatedAt: at(5), TTLUntil: at(6),
	})
	write(longEntry("theirs", "b", "another seat's note", at(7)))
	write(longEntry("blank", "a", " \n\t ", at(8)))

	got, err := d.Unembedded(context.Background(), "a", testModel, at(10), 10)
	if err != nil {
		t.Fatalf("Unembedded: %v", err)
	}
	ids := make([]string, 0, len(got))
	for _, e := range got {
		ids = append(ids, e.ID)
	}
	if want := []string{"legacy", "old-model", "bare"}; !slices.Equal(ids, want) {
		t.Fatalf("unembedded = %v, want %v", ids, want)
	}
	if limited, _ := d.Unembedded(context.Background(), "a", testModel, at(10), 1); len(limited) != 1 {
		t.Errorf("a limit of 1 returned %d notes", len(limited))
	}
}

// A FILL STORES THE VECTOR AND ITS MODEL, once: a second fill of the same model
// — two passes, or one racing the note's own write — changes nothing and is
// not counted, and a note of another model is replaced.
func TestAFillStoresTheVectorOnce(t *testing.T) {
	t.Parallel()
	db := learningStore(t)
	d := learning.NewDiary(db)
	mustWrite(t, d, longEntry("n", "a", "a fact", base))
	fill := []learning.DiaryFill{{ID: "n", Vector: learning.Vector{
		Values: []float32{0.1, 0.2, 0.3, 0.4}, Model: testModel}}}

	filled, err := d.FillEmbeddings(context.Background(), fill)
	if err != nil || filled != 1 {
		t.Fatalf("first fill = %d, %v; want 1", filled, err)
	}
	got, _ := d.Recent(context.Background(), "a", base, 10)
	if len(got) != 1 || len(got[0].Embedding) != 4 || got[0].EmbeddingModel != testModel {
		t.Fatalf("note = %+v, want the filled vector in %q", got, testModel)
	}
	if again, err := d.FillEmbeddings(context.Background(), fill); err != nil || again != 0 {
		t.Errorf("a second fill of the same model = %d, %v; want nothing changed", again, err)
	}
	if other, err := d.FillEmbeddings(context.Background(), []learning.DiaryFill{{ID: "n",
		Vector: learning.Vector{Values: []float32{0.4, 0.3, 0.2, 0.1}, Model: "next-model"}}}); err != nil || other != 1 {
		t.Errorf("a fill of another model = %d, %v; want the note re-embedded", other, err)
	}
	if left, _ := d.Unembedded(context.Background(), "a", "next-model", base, 10); len(left) != 0 {
		t.Errorf("after the fill %d notes are still unembedded", len(left))
	}
}

// A FILL OF THE WRONG WIDTH FAILS WHOLE: a provider answering another width is
// a configuration fault, and every vector beside it is suspect.
func TestAFillOfTheWrongWidthFailsWhole(t *testing.T) {
	t.Parallel()
	db := learningStore(t)
	d := learning.NewDiary(db)
	mustWrite(t, d, longEntry("a1", "a", "one", base))
	mustWrite(t, d, longEntry("a2", "a", "two", base))
	_, err := d.FillEmbeddings(context.Background(), []learning.DiaryFill{
		{ID: "a1", Vector: learning.Vector{Values: []float32{1, 0, 0, 0}, Model: testModel}},
		{ID: "a2", Vector: learning.Vector{Values: []float32{1, 0}, Model: testModel}},
	})
	if err == nil {
		t.Fatal("a fill with a vector of the wrong width was accepted")
	}
	if left, _ := d.Unembedded(context.Background(), "a", testModel, base, 10); len(left) != 2 {
		t.Errorf("a failed fill left %d of 2 notes unembedded, want both", len(left))
	}
}

// A NOTE AT A WIDTH A RESTART LEFT BEHIND IS UNFILLED, though its model is the
// current one: one model answers at whatever width is asked, and recall's width
// filter no longer admits it.
func TestANoteAtAnOldWidthIsUnembedded(t *testing.T) {
	t.Parallel()
	db := learningStore(t)
	d := learning.NewDiary(db)
	mustWrite(t, d, longEntry("narrow", "a", "embedded at the old width", base))
	if _, err := db.SQL().ExecContext(t.Context(),
		`UPDATE agent_diary SET embedding = ?, embedding_model = ? WHERE id = 'narrow'`,
		[]byte{0, 0, 128, 63, 0, 0, 0, 0}, testModel); err != nil {
		t.Fatalf("store an old-width vector: %v", err)
	}
	got, err := d.Unembedded(context.Background(), "a", testModel, base, 10)
	if err != nil || len(got) != 1 || got[0].ID != "narrow" {
		t.Fatalf("unembedded = %v, %v; want the old-width note", got, err)
	}
}
