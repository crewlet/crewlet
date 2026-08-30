package learning_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/learning"
	"github.com/crewlet/crewlet/internal/store"
)

func diary(t *testing.T) *learning.Diary {
	t.Helper()
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "d.db"), store.Options{})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return learning.NewDiary(db)
}

func longEntry(id, agent, content string, at time.Time) learning.DiaryEntry {
	return learning.DiaryEntry{
		ID: id, AgentID: agent, Kind: learning.DiaryLong,
		Content: content, CreatedAt: at, Source: "reflect",
	}
}

func mustWrite(t *testing.T, d *learning.Diary, e learning.DiaryEntry) {
	t.Helper()
	if err := d.Write(context.Background(), e); err != nil {
		t.Fatalf("Write(%s): %v", e.ID, err)
	}
}

func TestADiaryEntryRoundTrips(t *testing.T) {
	t.Parallel()
	d := diary(t)
	want := longEntry("a", "agent-1", "the release train leaves on Thursdays", base)
	want.TurnID = "turn-9"
	want.Metadata = map[string]any{"confidence": "high"}
	want.Embedding = []float32{0.1, 0.2}
	mustWrite(t, d, want)

	got, err := d.Recent(context.Background(), "agent-1", base, 10)
	if err != nil {
		t.Fatalf("Recent: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("recent = %d entries", len(got))
	}
	g := got[0]
	if g.Content != want.Content || g.Source != "reflect" || g.TurnID != "turn-9" {
		t.Errorf("entry = %+v", g)
	}
	if g.Metadata["confidence"] != "high" {
		t.Errorf("metadata = %v", g.Metadata)
	}
	if len(g.Embedding) != 2 {
		t.Errorf("embedding = %v", g.Embedding)
	}
	if !g.TTLUntil.IsZero() {
		t.Errorf("a long entry carries a deadline: %v", g.TTLUntil)
	}
}

func TestTheDiaryIsPrivateToOneAgent(t *testing.T) {
	t.Parallel()
	// The reason it is called a diary and not a memory: it is the seat's
	// own log, not knowledge other seats can query.
	d := diary(t)
	mustWrite(t, d, longEntry("a", "agent-1", "mine", base))
	got, err := d.Recent(context.Background(), "agent-2", base, 10)
	if err != nil {
		t.Fatalf("Recent: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("another agent read %d entries", len(got))
	}
}

func TestAKeyedByDerivedIDSoARenameOrphansCleanly(t *testing.T) {
	t.Parallel()
	// Keyed by the derived uuid rather than the handle, so renaming a
	// handle orphans the old rows instead of mixing them into the new
	// identity's memory.
	d := diary(t)
	mustWrite(t, d, longEntry("a", "uuid-old", "from the old identity", base))
	got, _ := d.Recent(context.Background(), "uuid-new", base, 10)
	if len(got) != 0 {
		t.Errorf("the new identity inherited %d entries", len(got))
	}
}

func TestAMislabelledEntryIsRefusedWithItsOwnField(t *testing.T) {
	t.Parallel()
	// The column has a CHECK constraint, so an unknown kind would fail at
	// the driver with a message naming the constraint and not the caller.
	d := diary(t)
	for name, bad := range map[string]learning.DiaryEntry{
		"no id":    {AgentID: "a", Kind: learning.DiaryLong},
		"no agent": {ID: "a", Kind: learning.DiaryLong},
		"bad kind": {ID: "a", AgentID: "b", Kind: "whatever"},
		// A short entry with no deadline never expires, which makes it a
		// long entry wearing the wrong label — and one the expiry sweep
		// never looks at, because that scan is indexed on a non-NULL ttl.
		"short with no deadline": {ID: "a", AgentID: "b", Kind: learning.DiaryShort},
		"long with a deadline":   {ID: "a", AgentID: "b", Kind: learning.DiaryLong, TTLUntil: base},
	} {
		if err := d.Write(context.Background(), bad); err == nil {
			t.Errorf("%s: written cleanly", name)
		}
	}

	// The column's CHECK constraint would refuse a bad kind too, so the Go
	// guard's value is entirely in WHAT IT SAYS: which field, and which
	// values are allowed. A driver error names the constraint and not the
	// caller, which sends a reader to the schema instead of to their code.
	//
	// Found by mutation: removing the Go guard still failed the write.
	err := d.Write(context.Background(), learning.DiaryEntry{
		ID: "a", AgentID: "b", Kind: "whatever",
	})
	if err == nil {
		t.Fatal("a bad kind was written")
	}
	for _, want := range []string{"kind", string(learning.DiaryLong), string(learning.DiaryShort), "whatever"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not mention %q: %v", want, err)
		}
	}
}

func TestAnExpiredEntryIsFilteredOnReadNotJustSwept(t *testing.T) {
	t.Parallel()
	// The sweep runs on a timer, and a memory that has just passed its
	// deadline is exactly as wrong as one that passed it a week ago.
	d := diary(t)
	short := learning.DiaryEntry{
		ID: "s", AgentID: "a", Kind: learning.DiaryShort,
		Content: "the incident bridge is open", CreatedAt: base,
		TTLUntil: base.Add(time.Hour),
	}
	mustWrite(t, d, short)
	mustWrite(t, d, longEntry("l", "a", "durable", base))

	live, _ := d.Recent(context.Background(), "a", base, 10)
	if len(live) != 2 {
		t.Fatalf("before the deadline: %d entries, want both", len(live))
	}
	after, _ := d.Recent(context.Background(), "a", base.Add(2*time.Hour), 10)
	if len(after) != 1 || after[0].ID != "l" {
		t.Errorf("after the deadline: %v, want only the durable one", entryIDs(after))
	}
	// And the entry itself can say so.
	if !short.Expired(base.Add(2 * time.Hour)) {
		t.Error("an entry past its deadline does not report as expired")
	}
	if short.Expired(base) {
		t.Error("an entry inside its deadline reports as expired")
	}
}

func TestExpirySweepsOnlyTheShortEntries(t *testing.T) {
	t.Parallel()
	d := diary(t)
	mustWrite(t, d, learning.DiaryEntry{
		ID: "s", AgentID: "a", Kind: learning.DiaryShort,
		Content: "temporary", CreatedAt: base, TTLUntil: base.Add(time.Hour),
	})
	mustWrite(t, d, longEntry("l", "a", "durable", base))

	n, err := d.Expire(context.Background(), base.Add(2*time.Hour))
	if err != nil {
		t.Fatalf("Expire: %v", err)
	}
	if n != 1 {
		t.Errorf("expired %d, want 1", n)
	}
	got, _ := d.Recent(context.Background(), "a", base.Add(2*time.Hour), 10)
	if len(got) != 1 || got[0].ID != "l" {
		t.Errorf("survivors = %v", entryIDs(got))
	}
}

func TestExpiryDoesNotTakeAnEntryOnItsDeadline(t *testing.T) {
	t.Parallel()
	// Boundary. An entry whose deadline is exactly now has just expired —
	// the read filter uses > and the sweep uses <=, so both must agree, or
	// a memory is invisible while still stored.
	d := diary(t)
	mustWrite(t, d, learning.DiaryEntry{
		ID: "s", AgentID: "a", Kind: learning.DiaryShort,
		Content: "x", CreatedAt: base, TTLUntil: base.Add(time.Hour),
	})
	at := base.Add(time.Hour)
	got, _ := d.Recent(context.Background(), "a", at, 10)
	if len(got) != 0 {
		t.Errorf("an entry at its deadline still reads as live: %v", entryIDs(got))
	}
	n, _ := d.Expire(context.Background(), at)
	if n != 1 {
		t.Errorf("the sweep left %d entries the read had already hidden", 1-n)
	}
}

func TestDiaryRecallRanksAndFiltersLikeEpisodeRecall(t *testing.T) {
	t.Parallel()
	d := diary(t)
	near := longEntry("near", "a", "close", base)
	near.Embedding = []float32{1, 0, 0, 0}
	far := longEntry("far", "a", "orthogonal", base)
	far.Embedding = []float32{0, 1, 0, 0}
	blind := longEntry("blind", "a", "no vector", base)
	expired := learning.DiaryEntry{
		ID: "expired", AgentID: "a", Kind: learning.DiaryShort,
		Content: "stale", CreatedAt: base, TTLUntil: base.Add(time.Hour),
		Embedding: []float32{1, 0, 0, 0},
	}
	// Another agent's entry, identical in every way that matters to the
	// ranking. Without it a recall that ignored the agent scope would still
	// return the right answer — the diary's privacy would be asserted
	// nowhere on the read path that matters most.
	someoneElse := longEntry("theirs", "b", "close", base)
	someoneElse.Embedding = []float32{1, 0, 0, 0}
	for _, e := range []learning.DiaryEntry{near, far, blind, expired, someoneElse} {
		mustWrite(t, d, e)
	}

	hits, err := d.Recall(context.Background(), "a",
		learning.RecallQuery{Embedding: []float32{1, 0, 0, 0}}, base.Add(2*time.Hour))
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if len(hits) != 1 || hits[0].Entry.ID != "near" {
		t.Errorf("hits = %v, want just the similar live entry", diaryHitIDs(hits))
	}
}

func TestDiaryRecallRefusesWhatItCannotAnswer(t *testing.T) {
	t.Parallel()
	d := diary(t)
	if _, err := d.Recall(context.Background(), "a", learning.RecallQuery{}, base); !errors.Is(err, learning.ErrNoEmbedding) {
		t.Errorf("err = %v, want ErrNoEmbedding", err)
	}
	if _, err := d.Recall(context.Background(), "",
		learning.RecallQuery{Embedding: []float32{1}}, base); err == nil {
		t.Error("a recall with no agent was accepted")
	}
}

func TestRetrievalBookkeepingNeverCostsTheRecall(t *testing.T) {
	t.Parallel()
	// A seat that recalled a memory has had the benefit whether or not the
	// counter moved. MarkRetrieved is therefore separate from Recall and
	// returns nothing to fail on.
	d := diary(t)
	e := longEntry("a", "agent-1", "x", base)
	e.Embedding = []float32{1, 0}
	mustWrite(t, d, e)

	d.MarkRetrieved(context.Background(), []string{"a", "does-not-exist"}, base.Add(time.Hour))
	got, _ := d.Recent(context.Background(), "agent-1", base.Add(time.Hour), 10)
	if len(got) != 1 {
		t.Fatalf("recent = %d", len(got))
	}
	if got[0].RetrievalCount != 1 {
		t.Errorf("retrieval count = %d, want 1", got[0].RetrievalCount)
	}
	if !got[0].LastRetrievedAt.Equal(base.Add(time.Hour)) {
		t.Errorf("last retrieved = %v", got[0].LastRetrievedAt)
	}
}

func TestWritingTheSameEntryTwiceKeepsTheFirst(t *testing.T) {
	t.Parallel()
	// A retried write of one observation. Recording it twice would double
	// its weight in every later recall.
	d := diary(t)
	mustWrite(t, d, longEntry("a", "agent-1", "first", base))
	mustWrite(t, d, longEntry("a", "agent-1", "second", base))
	got, _ := d.Recent(context.Background(), "agent-1", base, 10)
	if len(got) != 1 || got[0].Content != "first" {
		t.Errorf("entries = %d, content = %q", len(got), got[0].Content)
	}
}

func TestMetadataAlwaysHoldsAJSONObject(t *testing.T) {
	t.Parallel()
	// The column is NOT NULL with a '{}' default, and a nil map marshals to
	// the four characters "null", which then fails every JSON query.
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "m.db"), store.Options{})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	d := learning.NewDiary(db)
	mustWrite(t, d, longEntry("a", "agent-1", "x", base))

	var metadata string
	if err := db.SQL().QueryRowContext(t.Context(),
		`SELECT metadata FROM agent_diary WHERE id = 'a'`).Scan(&metadata); err != nil {
		t.Fatalf("read raw column: %v", err)
	}
	if metadata != "{}" {
		t.Errorf("metadata = %q, want an empty JSON object", metadata)
	}
}

func entryIDs(es []learning.DiaryEntry) []string {
	out := make([]string, 0, len(es))
	for _, e := range es {
		out = append(out, e.ID)
	}
	return out
}

func diaryHitIDs(hs []learning.DiaryHit) []string {
	out := make([]string, 0, len(hs))
	for _, h := range hs {
		out = append(out, h.Entry.ID)
	}
	return out
}

// A durable entry has no deadline, so nothing ages it out — but recall scans
// and cosines every one of a seat's rows on every Plan phase, so an unbounded
// diary is a per-turn cost that only grows. The bound is a cap, and what it
// drops is decided by USE rather than by age.
func TestTheDurableDiaryIsCappedByWorthNotByAge(t *testing.T) {
	d := diary(t)
	ctx := context.Background()
	at := time.Now().UTC().Add(-30 * 24 * time.Hour)

	// Six entries for one seat. The two that have been recalled are the
	// valuable ones; among the rest the oldest is the most disposable.
	for i := range 6 {
		mustWrite(t, d, longEntry(fmt.Sprintf("e%d", i), "agent-a",
			fmt.Sprintf("fact %d", i), at.Add(time.Duration(i)*time.Hour)))
	}
	// e0 is the OLDEST and would go first on age alone — recalling it is
	// what proves the eviction is not an age sweep in disguise.
	d.MarkRetrieved(ctx, []string{"e0", "e0", "e1"}, at.Add(time.Hour))

	// Another seat, under the cap, must be untouched: the cap is per agent.
	mustWrite(t, d, longEntry("other", "agent-b", "theirs", at))

	dropped, err := d.TrimLong(ctx, 3)
	if err != nil {
		t.Fatalf("TrimLong: %v", err)
	}
	if dropped != 3 {
		t.Fatalf("dropped %d, want the 3 rows past the cap", dropped)
	}

	kept := map[string]bool{}
	rows, err := d.Recent(ctx, "agent-a", time.Now().UTC(), 100)
	if err != nil {
		t.Fatalf("Recent: %v", err)
	}
	for _, r := range rows {
		kept[r.ID] = true
	}
	if len(kept) != 3 {
		t.Fatalf("seat holds %d entries after a trim to 3: %v", len(kept), kept)
	}
	// The recalled pair survives, oldest-first ordering notwithstanding.
	for _, id := range []string{"e0", "e1"} {
		if !kept[id] {
			t.Errorf("%s was recalled before and still got evicted: %v", id, kept)
		}
	}
	// Of the never-recalled rows, the newest is the one worth keeping.
	if !kept["e5"] {
		t.Errorf("the newest un-recalled entry was evicted: %v", kept)
	}

	other, err := d.Recent(ctx, "agent-b", time.Now().UTC(), 10)
	if err != nil {
		t.Fatalf("Recent(agent-b): %v", err)
	}
	if len(other) != 1 {
		t.Errorf("a seat under the cap lost entries to another seat's trim: %d", len(other))
	}
}

// A seat under the cap is left entirely alone, and the sweep says it did
// nothing rather than reporting a number the driver invented.
func TestTrimmingASeatUnderTheCapChangesNothing(t *testing.T) {
	d := diary(t)
	ctx := context.Background()
	at := time.Now().UTC().Add(-time.Hour)
	for i := range 3 {
		mustWrite(t, d, longEntry(fmt.Sprintf("k%d", i), "agent-a", "fact", at))
	}
	dropped, err := d.TrimLong(ctx, 10)
	if err != nil {
		t.Fatalf("TrimLong: %v", err)
	}
	if dropped != 0 {
		t.Fatalf("dropped %d from a seat under the cap", dropped)
	}
	rows, err := d.Recent(ctx, "agent-a", time.Now().UTC(), 10)
	if err != nil {
		t.Fatalf("Recent: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("seat holds %d entries, want 3", len(rows))
	}
}
