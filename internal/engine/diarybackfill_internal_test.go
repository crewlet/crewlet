package engine

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/learning"
	"github.com/crewlet/crewlet/internal/providers/embeddings"
	"github.com/crewlet/crewlet/internal/store"
)

// seatsHeld is a seat host as the fill reads it: which seats are held, and
// which of those are established.
type seatsHeld struct {
	held        []string
	established map[string]bool
}

func (s seatsHeld) Held() []string { return s.held }

func (s seatsHeld) MayStart(handle string) (int64, bool) {
	return 1, s.established[handle]
}

// diaryFixture is a node store at width 64 holding n vectorless notes for each
// agent named.
func diaryFixture(t *testing.T, notes map[string][]string) (*store.DB, *learning.Diary) {
	t.Helper()
	db, err := store.OpenNode(t.Context(), t.TempDir()+"/n.db", store.Options{EmbeddingDim: 64})
	if err != nil {
		t.Fatalf("store.OpenNode: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	diary := learning.NewDiary(db)
	at := time.Now().UTC().Add(-time.Hour)
	for agent, contents := range notes {
		for i, content := range contents {
			if err := diary.Write(t.Context(), learning.DiaryEntry{
				ID: fmt.Sprintf("%s-%d", agent, i), AgentID: agent, Kind: learning.DiaryLong,
				Content: content, CreatedAt: at.Add(time.Duration(i) * time.Second),
			}); err != nil {
				t.Fatalf("write: %v", err)
			}
		}
	}
	return db, diary
}

func unfilled(t *testing.T, diary *learning.Diary, agent, model string) int {
	t.Helper()
	left, err := diary.Unembedded(t.Context(), agent, model, time.Now().UTC(), 1000)
	if err != nil {
		t.Fatalf("Unembedded: %v", err)
	}
	return len(left)
}

// ONLY AN ESTABLISHED SEAT THIS NODE HOLDS IS FILLED. A seat still
// establishing has not hydrated its memory — filling it would embed whatever
// stale copy this node happens to have — and a seat this node does not hold
// is its holder's to fill.
func TestTheFillTouchesOnlyEstablishedHeldSeats(t *testing.T) {
	t.Parallel()
	_, diary := diaryFixture(t, map[string][]string{
		"id-ready":   {"the release train is thursdays", "pat wants digests"},
		"id-joining": {"a note from a seat still hydrating"},
		"id-elsew":   {"a seat another node holds"},
	})
	fake := embeddings.NewFake(64)
	seats := seatsHeld{
		held:        []string{"joining", "ready"},
		established: map[string]bool{"ready": true},
	}
	ids := map[string]string{"ready": "id-ready", "joining": "id-joining", "elsewhere": "id-elsew"}
	filled := fillHeldDiaries(t.Context(), seats, func(h string) string { return ids[h] },
		diary, fake, diaryBackfillPerTick)
	if filled != 2 {
		t.Fatalf("filled %d notes, want the established seat's two", filled)
	}
	if n := unfilled(t, diary, "id-ready", fake.Model()); n != 0 {
		t.Errorf("the established seat has %d notes left", n)
	}
	if n := unfilled(t, diary, "id-joining", fake.Model()); n != 1 {
		t.Errorf("a seat still establishing was filled: %d of 1 left", n)
	}
	if n := unfilled(t, diary, "id-elsew", fake.Model()); n != 1 {
		t.Errorf("a seat this node does not hold was filled: %d of 1 left", n)
	}
}

// A TICK IS BOUNDED ACROSS SEATS: a model change leaves every seat's diary in
// the old space at once, and the budget is what keeps the first tick from
// sending all of it.
func TestAFillTickStopsAtItsBudget(t *testing.T) {
	t.Parallel()
	notes := make([]string, 5)
	for i := range notes {
		notes[i] = fmt.Sprintf("durable fact number %d", i)
	}
	_, diary := diaryFixture(t, map[string][]string{"id-a": notes, "id-b": notes})
	fake := embeddings.NewFake(64)
	seats := seatsHeld{held: []string{"a", "b"}, established: map[string]bool{"a": true, "b": true}}
	filled := fillHeldDiaries(t.Context(), seats, func(h string) string { return "id-" + h },
		diary, fake, 7)
	if filled != 7 {
		t.Fatalf("filled %d notes with a budget of 7", filled)
	}
	if left := unfilled(t, diary, "id-a", fake.Model()) + unfilled(t, diary, "id-b", fake.Model()); left != 3 {
		t.Errorf("%d notes left, want the 3 past the budget", left)
	}
}

// ONE NOTE THE PROVIDER REFUSES DOES NOT HOLD BACK ITS NEIGHBOURS: a provider
// refuses a whole request over one input, so a refused batch is retried note
// by note and every note it will take is filled.
func TestARefusedNoteDoesNotHoldBackItsNeighbours(t *testing.T) {
	t.Parallel()
	_, diary := diaryFixture(t, map[string][]string{
		"id-a": {"the release train is thursdays", "a poison note", "pat wants digests"},
	})
	fake := embeddings.NewFake(64)
	fake.Refuse("poison")
	seats := seatsHeld{held: []string{"a"}, established: map[string]bool{"a": true}}
	filled := fillHeldDiaries(t.Context(), seats, func(string) string { return "id-a" },
		diary, fake, diaryBackfillPerTick)
	if filled != 2 {
		t.Fatalf("filled %d notes, want the two the provider takes", filled)
	}
	left, err := diary.Unembedded(context.Background(), "id-a", fake.Model(), time.Now().UTC(), 10)
	if err != nil || len(left) != 1 || left[0].Content != "a poison note" {
		t.Fatalf("left = %v, %v; want only the refused note", left, err)
	}
}
