package engine

import (
	"context"
	"errors"
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

func (s seatsHeld) Held() []string { return append([]string(nil), s.held...) }

func (s seatsHeld) MayStart(handle string) (int64, bool) {
	return 1, s.established[handle]
}

// allEstablished holds and establishes every handle named.
func allEstablished(handles ...string) seatsHeld {
	s := seatsHeld{held: handles, established: map[string]bool{}}
	for _, h := range handles {
		s.established[h] = true
	}
	return s
}

// diaryFixture is a node store at width 64 holding the vectorless notes named
// for each agent.
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
	left, err := diary.Unfilled(t.Context(), agent, model, time.Now().UTC(),
		learning.FillCursor{}, 1000)
	if err != nil {
		t.Fatalf("Unfilled: %v", err)
	}
	return len(left)
}

// facts is n notes of one length, so a budget in bytes is a budget in notes.
func facts(n int, marker string) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("%s durable fact number %03d", marker, i)
	}
	return out
}

// tick runs one tick of the fill over seats as fillMemory would, under the
// given memory and pass allowance.
func tick(t *testing.T, db *store.DB, seats heldSeats, ids func(string) string,
	provider embeddings.BatchEmbedder, memory *embeddings.Refusals, at time.Time,
	requests, bytes int, resume string,
) fillReport {
	t.Helper()
	pass := embeddings.NewPass(memory, at, requests, bytes)
	return fillHeldMemory(t.Context(), seats, memorySources(db, ids), provider, pass, resume)
}

var fillAt = time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)

// ONLY AN ESTABLISHED SEAT THIS NODE HOLDS IS FILLED. A seat still
// establishing has not hydrated its memory — filling it would embed whatever
// stale copy this node happens to have — and a seat this node does not hold
// is its holder's to fill.
func TestTheFillTouchesOnlyEstablishedHeldSeats(t *testing.T) {
	t.Parallel()
	db, diary := diaryFixture(t, map[string][]string{
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
	report := tick(t, db, seats, func(h string) string { return ids[h] }, fake,
		embeddings.NewRefusals(), fillAt, memoryFillRequestsPerTick, memoryFillBytesPerTick, "")
	if report.filled["diary"] != 2 {
		t.Fatalf("filled %d notes, want the established seat's two", report.filled["diary"])
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

// A TICK IS BOUNDED BY WHAT IT SENDS, ACROSS SEATS: a model change leaves every
// seat's diary in the old space at once, and the budget is what keeps the first
// tick from sending all of it. The next tick starts at the seat this one ran
// out at, so the seats behind it are not for ever behind the ones before.
func TestAFillTickStopsAtItsBudgetAndTheNextResumesWhereItStopped(t *testing.T) {
	t.Parallel()
	db, diary := diaryFixture(t, map[string][]string{
		"id-a": facts(5, "a"), "id-b": facts(5, "b"), "id-c": facts(5, "c"),
	})
	fake := embeddings.NewFake(64)
	seats := allEstablished("a", "b", "c")
	ids := func(h string) string { return "id-" + h }
	one := len(embeddings.Prepare(facts(1, "a")[0]))
	memory := embeddings.NewRefusals()

	first := tick(t, db, seats, ids, fake, memory, fillAt, memoryFillRequestsPerTick, 7*one, "")
	if first.filled["diary"] != 7 || first.pass.Bytes > 7*one {
		t.Fatalf("filled %d notes in %d bytes with a budget of 7 notes", first.filled["diary"], first.pass.Bytes)
	}
	if first.resume != "b" {
		t.Fatalf("the tick ran out at seat %q, want b", first.resume)
	}
	second := tick(t, db, seats, ids, fake, memory, fillAt.Add(time.Minute),
		memoryFillRequestsPerTick, 5*one, first.resume)
	if second.filled["diary"] != 5 {
		t.Fatalf("the next tick filled %d notes", second.filled["diary"])
	}
	if n := unfilled(t, diary, "id-b", fake.Model()); n != 0 {
		t.Errorf("the seat the first tick ran out at still has %d notes: the next tick did not resume there", n)
	}
	if n := unfilled(t, diary, "id-c", fake.Model()); n != 3 {
		t.Errorf("seat c has %d notes left, want the 3 past the second budget", n)
	}
}

// ONE NOTE THE PROVIDER REFUSES DOES NOT HOLD BACK ITS NEIGHBOURS, AND IS NOT
// SENT AGAIN ON THE NEXT TICK: a refused call is split until the note is
// alone, every note the provider takes is filled, and the poison is held back
// for the hour rather than isolated again every minute.
func TestARefusedNoteIsIsolatedOnceAndNotSentAgainOnTheNextTick(t *testing.T) {
	t.Parallel()
	db, diary := diaryFixture(t, map[string][]string{
		"id-a": {"the release train is thursdays", "a poison note", "pat wants digests"},
	})
	fake := embeddings.NewFake(64)
	fake.Refuse("poison")
	seats := allEstablished("a")
	ids := func(string) string { return "id-a" }
	memory := embeddings.NewRefusals()
	first := tick(t, db, seats, ids, fake, memory, fillAt, memoryFillRequestsPerTick,
		memoryFillBytesPerTick, "")
	if first.filled["diary"] != 2 {
		t.Fatalf("filled %d notes, want the two the provider takes", first.filled["diary"])
	}
	if first.pass.Requests > 5 {
		t.Errorf("isolating one note among three took %d requests, want at most 1 + 2·⌈log₂3⌉ = 5",
			first.pass.Requests)
	}
	left, err := diary.Unfilled(context.Background(), "id-a", fake.Model(), time.Now().UTC(),
		learning.FillCursor{}, 10)
	if err != nil || len(left) != 1 || left[0].Text != "a poison note" {
		t.Fatalf("left = %v, %v; want only the refused note", left, err)
	}

	before := len(fake.Requests())
	next := tick(t, db, seats, ids, fake, memory, fillAt.Add(time.Minute),
		memoryFillRequestsPerTick, memoryFillBytesPerTick, first.resume)
	if sent := len(fake.Requests()) - before; sent != 0 || next.pass.Held != 1 {
		t.Fatalf("the next tick sent %d requests for the held note (held %d)", sent, next.pass.Held)
	}
}

// A FAILURE THAT IS NOT ABOUT A NOTE ENDS THE TICK FOR EVERY SEAT: a throttled
// account or a revoked key answers every seat's batch the same way, and each
// held seat sending its own into it every minute was the burst a throttled
// account least needs.
func TestAProviderFailureEndsTheTickForEverySeat(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		fail  func(*embeddings.Fake)
		class error
	}{
		{"429", func(f *embeddings.Fake) { f.FailTransiently("durable", 1000) }, embeddings.ErrTransient},
		{"401", func(f *embeddings.Fake) { f.FailConfiguration("durable", 1000) }, embeddings.ErrConfiguration},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			db, diary := diaryFixture(t, map[string][]string{
				"id-a": facts(3, "a"), "id-b": facts(3, "b"), "id-c": facts(3, "c"),
			})
			fake := embeddings.NewFake(64)
			tc.fail(fake)
			report := tick(t, db, allEstablished("a", "b", "c"), func(h string) string { return "id-" + h },
				fake, embeddings.NewRefusals(), fillAt, memoryFillRequestsPerTick,
				memoryFillBytesPerTick, "")
			if !errors.Is(report.pass.Err(), tc.class) {
				t.Fatalf("the tick ended with %v, want %v", report.pass.Err(), tc.class)
			}
			if sent := len(fake.Requests()); sent != 1 {
				t.Fatalf("the tick sent %d requests across three seats, want the one "+
					"that met the failure", sent)
			}
			if report.resume != "a" {
				t.Errorf("the next tick would start at %q, want the seat the failure met", report.resume)
			}
			for _, agent := range []string{"id-a", "id-b", "id-c"} {
				if n := unfilled(t, diary, agent, fake.Model()); n != 3 {
					t.Errorf("%s has %d of 3 notes left", agent, n)
				}
			}
		})
	}
}

// A PROVIDER THAT REFUSES EVERYTHING IS HELD TO THE REQUEST BOUND ACROSS SEATS,
// concluded the configuration's refusal, and then sent nothing for the pause —
// where retrying each note alone was one request and one warning per note per
// seat per minute.
func TestAProviderRefusingEveryNoteIsBoundedThenLeftAlone(t *testing.T) {
	t.Parallel()
	db, _ := diaryFixture(t, map[string][]string{
		"id-a": facts(embeddings.PassBatch, "a"), "id-b": facts(embeddings.PassBatch, "b"),
		"id-c": facts(embeddings.PassBatch, "c"),
	})
	fake := embeddings.NewFake(64)
	fake.Refuse("durable")
	seats := allEstablished("a", "b", "c")
	ids := func(h string) string { return "id-" + h }
	memory := embeddings.NewRefusals()
	resume := ""
	concluded := -1
	for minute := range 10 {
		before := len(fake.Requests())
		report := tick(t, db, seats, ids, fake, memory, fillAt.Add(time.Duration(minute)*time.Minute),
			memoryFillRequestsPerTick, memoryFillBytesPerTick, resume)
		resume = report.resume
		if sent := len(fake.Requests()) - before; sent > memoryFillRequestsPerTick {
			t.Fatalf("tick %d sent %d requests, past the bound of %d", minute, sent,
				memoryFillRequestsPerTick)
		}
		if report.filled["diary"] != 0 {
			t.Fatalf("tick %d filled %d notes from a provider that refuses all of them",
				minute, report.filled["diary"])
		}
		if report.pass.Concluded {
			concluded = minute
			break
		}
	}
	if concluded < 0 {
		t.Fatal("ten ticks of nothing but refusals never concluded the configuration is refused")
	}
	before := len(fake.Requests())
	paused := tick(t, db, seats, ids, fake, memory, fillAt.Add(time.Duration(concluded+1)*time.Minute),
		memoryFillRequestsPerTick, memoryFillBytesPerTick, resume)
	if sent := len(fake.Requests()) - before; sent != 0 || !paused.pass.Paused {
		t.Fatalf("the tick after the conclusion sent %d requests (paused %v)", sent, paused.pass.Paused)
	}
}

// THE ACCOUNT'S BUDGET IS SHARED: every node's fill draws on one account, so
// each takes its share of the company's bytes a tick rather than all of them.
func TestEachNodeTakesItsShareOfTheAccountBudget(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ nodes, want int }{
		{1, memoryFillBytesPerTick}, {4, memoryFillBytesPerTick / 4}, {3, 333_334}, {0, memoryFillBytesPerTick},
	} {
		if got := shareOf(memoryFillBytesPerTick, tc.nodes); got != tc.want {
			t.Errorf("share of %d nodes = %d, want %d", tc.nodes, got, tc.want)
		}
	}
}
