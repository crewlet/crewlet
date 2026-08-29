package learning_test

import (
	"context"
	"errors"
	"math"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/learning"
	"github.com/crewlet/crewlet/internal/store"
)

var base = time.Date(2026, 8, 20, 9, 0, 0, 0, time.UTC)

func episodes(t *testing.T, opts ...func(*store.Options)) *learning.Episodes {
	t.Helper()
	o := store.Options{}
	for _, fn := range opts {
		fn(&o)
	}
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "l.db"), o)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return learning.NewEpisodes(db)
}

func ep(id, handle string, at time.Time) learning.Episode {
	return learning.Episode{
		ID: id, Handle: handle, Role: "CTO", TurnID: "turn-" + id,
		StartedAt: at, EndedAt: at, TaskSummary: "did " + id,
		ReviewOutcome: "done", Duration: 3 * time.Second,
	}
}

func mustAppend(t *testing.T, e *learning.Episodes, episode learning.Episode) bool {
	t.Helper()
	wrote, err := e.Append(context.Background(), episode)
	if err != nil {
		t.Fatalf("Append(%s): %v", episode.ID, err)
	}
	return wrote
}

func TestAnEpisodeRoundTrips(t *testing.T) {
	t.Parallel()
	e := episodes(t)
	want := ep("a", "ceo", base)
	want.ToolSequence = []string{"slack_post", "jira_get"}
	want.SkillsUsed = []string{"weekly-summary"}
	want.WorkKey = "wk-1"
	want.ConversationKey = "slack:C1"
	want.TaskID = "T-9"
	mustAppend(t, e, want)

	got, err := e.Recent(context.Background(), "ceo", 10)
	if err != nil {
		t.Fatalf("Recent: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("recent = %d episodes", len(got))
	}
	g := got[0]
	if g.ID != "a" || g.Role != "CTO" || g.TaskID != "T-9" || g.TurnID != "turn-a" {
		t.Errorf("identity fields lost: %+v", g)
	}
	if !g.EndedAt.Equal(base) || g.Duration != 3*time.Second {
		t.Errorf("time fields lost: ended %v duration %v", g.EndedAt, g.Duration)
	}
	if len(g.ToolSequence) != 2 || g.ToolSequence[0] != "slack_post" {
		t.Errorf("tool sequence = %v", g.ToolSequence)
	}
	if g.WorkKey != "wk-1" || g.ConversationKey != "slack:C1" {
		t.Errorf("keys lost: %q / %q", g.WorkKey, g.ConversationKey)
	}
	if g.Kind != learning.KindRaw || g.Count != 1 {
		t.Errorf("kind = %q count = %d, want a raw single turn", g.Kind, g.Count)
	}
}

func TestOneWorkKeyRecordsOneEpisode(t *testing.T) {
	t.Parallel()
	// Two nodes can both complete a turn for one trigger — a zombie
	// finishing between fence checks, or an honest re-run after the
	// completion ledger fails open. An episode keyed on nothing lands
	// twice and then feeds every later recall and skill synthesis,
	// weighting the agent's behaviour with an event that happened once.
	e := episodes(t)
	first := ep("a", "ceo", base)
	first.WorkKey = "wk-1"
	second := ep("b", "ceo", base.Add(time.Minute))
	second.WorkKey = "wk-1"

	if !mustAppend(t, e, first) {
		t.Error("the first episode was not written")
	}
	if mustAppend(t, e, second) {
		t.Error("a duplicate work key was written as a second episode")
	}
	got, err := e.Recent(context.Background(), "ceo", 10)
	if err != nil {
		t.Fatalf("Recent: %v", err)
	}
	if len(got) != 1 || got[0].ID != "a" {
		t.Errorf("stored %d episodes, want the first only", len(got))
	}
}

func TestUnkeyedTurnsAreNeverCollapsed(t *testing.T) {
	t.Parallel()
	// '' means "no ledgerable trigger" — a scheduled fire, a sub-agent, a
	// sandbox resume — not "the same trigger". Deduping on it keeps ONE
	// episode for every unkeyed turn a seat ever ran.
	e := episodes(t)
	for _, id := range []string{"a", "b", "c"} {
		if !mustAppend(t, e, ep(id, "ceo", base)) {
			t.Errorf("unkeyed episode %s was collapsed", id)
		}
	}
	got, _ := e.Recent(context.Background(), "ceo", 10)
	if len(got) != 3 {
		t.Errorf("stored %d episodes, want all three unkeyed turns", len(got))
	}
}

func TestTheWorkKeyIsScopedToTheSeat(t *testing.T) {
	t.Parallel()
	// Two seats legitimately act on one trigger — a broadcast, a task
	// assigned to a unit — and each one's episode is its own memory.
	e := episodes(t)
	a := ep("a", "ceo", base)
	a.WorkKey = "wk-1"
	b := ep("b", "cto", base)
	b.WorkKey = "wk-1"
	if !mustAppend(t, e, a) || !mustAppend(t, e, b) {
		t.Error("one seat's episode suppressed another's")
	}
}

func TestAnEpisodeNeedsAnIdentity(t *testing.T) {
	t.Parallel()
	e := episodes(t)
	for name, bad := range map[string]learning.Episode{
		"no id":   {Handle: "ceo"},
		"no seat": {ID: "a"},
	} {
		if _, err := e.Append(context.Background(), bad); err == nil {
			t.Errorf("%s: appended cleanly", name)
		}
	}
}

func TestRecentIsNewestFirstAndBounded(t *testing.T) {
	t.Parallel()
	e := episodes(t)
	for i, id := range []string{"a", "b", "c"} {
		mustAppend(t, e, ep(id, "ceo", base.Add(time.Duration(i)*time.Minute)))
	}
	got, err := e.Recent(context.Background(), "ceo", 2)
	if err != nil {
		t.Fatalf("Recent: %v", err)
	}
	if len(got) != 2 || got[0].ID != "c" || got[1].ID != "b" {
		t.Errorf("recent = %v, want the newest two, newest first", ids(got))
	}
}

func TestConversationLookupDoesNotMatchTurnsWithNoConversation(t *testing.T) {
	t.Parallel()
	// No conversation is not "every conversation". Querying for it would
	// match the NULLs — the turns that HAVE no conversation — and a seat
	// then reads unrelated work as this thread's history.
	e := episodes(t)
	unkeyed := ep("a", "ceo", base)
	keyed := ep("b", "ceo", base)
	keyed.ConversationKey = "slack:C1"
	mustAppend(t, e, unkeyed)
	mustAppend(t, e, keyed)

	got, err := e.ForConversation(context.Background(), "ceo", "", 10)
	if err != nil {
		t.Fatalf("ForConversation: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("an empty conversation key matched %v", ids(got))
	}
	got, err = e.ForConversation(context.Background(), "ceo", "slack:C1", 10)
	if err != nil {
		t.Fatalf("ForConversation: %v", err)
	}
	if len(got) != 1 || got[0].ID != "b" {
		t.Errorf("conversation lookup = %v, want just the keyed episode", ids(got))
	}
}

func TestAnEmbeddingSurvivesAndAMissingOneIsNotAFailure(t *testing.T) {
	t.Parallel()
	// Nil is a supported state: a transient embeddings outage must never
	// cost an episode. Recall skips such a row; every other query returns
	// it.
	e := episodes(t)
	withVec := ep("a", "ceo", base)
	withVec.Embedding = []float32{0.1, 0.2, 0.3, 0.4}
	mustAppend(t, e, withVec)
	mustAppend(t, e, ep("b", "ceo", base.Add(time.Minute)))

	got, _ := e.Recent(context.Background(), "ceo", 10)
	if len(got) != 2 {
		t.Fatalf("recent = %d", len(got))
	}
	byID := map[string]learning.Episode{}
	for _, g := range got {
		byID[g.ID] = g
	}
	if len(byID["a"].Embedding) != 4 || byID["a"].Embedding[0] != 0.1 {
		t.Errorf("embedding = %v", byID["a"].Embedding)
	}
	if byID["b"].Embedding != nil {
		t.Errorf("an absent embedding came back as %v", byID["b"].Embedding)
	}
}

func TestAWrongWidthEmbeddingIsRefusedAtWrite(t *testing.T) {
	t.Parallel()
	// The column is a plain BLOB and Turso does not enforce a declared
	// vector width, so a mismatched vector stores happily and then makes
	// every distance query against it return nothing — a seat whose recall
	// silently stops working, with no error anywhere.
	e := episodes(t, func(o *store.Options) { o.EmbeddingDim = 4 })
	bad := ep("a", "ceo", base)
	bad.Embedding = []float32{0.1, 0.2}
	if _, err := e.Append(context.Background(), bad); err == nil {
		t.Fatal("a wrong-width embedding was written")
	}
	// The counterfactual: the configured width goes in.
	good := ep("b", "ceo", base)
	good.Embedding = []float32{0.1, 0.2, 0.3, 0.4}
	if _, err := e.Append(context.Background(), good); err != nil {
		t.Errorf("a correctly-sized embedding was refused: %v", err)
	}
}

func TestRecallRanksBySimilarity(t *testing.T) {
	t.Parallel()
	e := episodes(t)
	near := ep("near", "ceo", base)
	near.Embedding = []float32{1, 0, 0, 0}
	far := ep("far", "ceo", base.Add(time.Minute))
	far.Embedding = []float32{0, 1, 0, 0}
	mid := ep("mid", "ceo", base.Add(2*time.Minute))
	mid.Embedding = []float32{0.9, 0.4, 0, 0}
	for _, x := range []learning.Episode{near, far, mid} {
		mustAppend(t, e, x)
	}

	hits, err := e.Recall(context.Background(), learning.RecallQuery{
		Handle: "ceo", Embedding: []float32{1, 0, 0, 0},
	})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("hits = %v, want the two above the floor", hitIDs(hits))
	}
	if hits[0].Episode.ID != "near" || hits[1].Episode.ID != "mid" {
		t.Errorf("ranking = %v, want [near mid]", hitIDs(hits))
	}
	// The orthogonal one is below the floor. Without a floor the nearest N
	// rows always come back, so a seat with three episodes recalls all
	// three on every turn however irrelevant.
	for _, h := range hits {
		if h.Episode.ID == "far" {
			t.Error("an orthogonal memory passed the relevance floor")
		}
	}
}

func TestRecallSkipsRowsWithNoEmbedding(t *testing.T) {
	t.Parallel()
	// Written during an embeddings outage. Treating a missing vector as a
	// zero vector would score it maximally dissimilar to everything and
	// rank it consistently last — which reads as a judgment about its
	// content.
	e := episodes(t)
	mustAppend(t, e, ep("blind", "ceo", base))
	seen := ep("seen", "ceo", base)
	seen.Embedding = []float32{1, 0, 0, 0}
	mustAppend(t, e, seen)

	hits, err := e.Recall(context.Background(), learning.RecallQuery{
		Handle: "ceo", Embedding: []float32{1, 0, 0, 0},
	})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if len(hits) != 1 || hits[0].Episode.ID != "seen" {
		t.Errorf("hits = %v, want just the embedded episode", hitIDs(hits))
	}
}

func TestRecallIsScopedToTheSeatAndToRawEpisodes(t *testing.T) {
	t.Parallel()
	// A compacted cluster summarises many turns and reads in a prompt like
	// one turn that did all of them.
	e := episodes(t)
	mine := ep("mine", "ceo", base)
	mine.Embedding = []float32{1, 0, 0, 0}
	theirs := ep("theirs", "cto", base)
	theirs.Embedding = []float32{1, 0, 0, 0}
	cluster := ep("cluster", "ceo", base)
	cluster.Embedding = []float32{1, 0, 0, 0}
	cluster.Kind = learning.KindCompacted
	cluster.Count = 12
	for _, x := range []learning.Episode{mine, theirs, cluster} {
		mustAppend(t, e, x)
	}

	hits, _ := e.Recall(context.Background(), learning.RecallQuery{
		Handle: "ceo", Embedding: []float32{1, 0, 0, 0},
	})
	if len(hits) != 1 || hits[0].Episode.ID != "mine" {
		t.Errorf("hits = %v, want only this seat's raw episode", hitIDs(hits))
	}
	// Asking for clusters explicitly returns them, or the compaction
	// worker's output would be unreadable.
	hits, _ = e.Recall(context.Background(), learning.RecallQuery{
		Handle: "ceo", Embedding: []float32{1, 0, 0, 0},
		Kinds: []learning.Kind{learning.KindCompacted},
	})
	if len(hits) != 1 || hits[0].Episode.ID != "cluster" {
		t.Errorf("hits = %v, want the cluster", hitIDs(hits))
	}
}

func TestRecallIsStableAcrossTies(t *testing.T) {
	t.Parallel()
	// Two episodes of one recurring task score identically. A scan-order
	// tiebreak gives a seat a different memory on every turn for no reason
	// it could act on.
	e := episodes(t)
	for i, id := range []string{"a", "b", "c"} {
		x := ep(id, "ceo", base.Add(time.Duration(i)*time.Minute))
		x.Embedding = []float32{1, 0, 0, 0}
		mustAppend(t, e, x)
	}
	first, err := e.Recall(context.Background(), learning.RecallQuery{
		Handle: "ceo", Embedding: []float32{1, 0, 0, 0}, Limit: 2,
	})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	// Newest first among equals: a seat recalling one of several identical
	// turns should recall the most recent one.
	if len(first) != 2 || first[0].Episode.ID != "c" || first[1].Episode.ID != "b" {
		t.Fatalf("hits = %v, want the newest two", hitIDs(first))
	}
	for range 20 {
		again, _ := e.Recall(context.Background(), learning.RecallQuery{
			Handle: "ceo", Embedding: []float32{1, 0, 0, 0}, Limit: 2,
		})
		if len(again) != len(first) || again[0].Episode.ID != first[0].Episode.ID {
			t.Fatalf("unstable ranking: %v then %v", hitIDs(first), hitIDs(again))
		}
	}

	// The FULL tie: same score AND same timestamp, which is what a batch of
	// turns written in one second looks like. Recency cannot separate them,
	// so the id has to — otherwise the order comes from the scan.
	//
	// Found by mutation: with distinct timestamps above, the id tiebreak
	// never ran and could be deleted with nothing failing.
	same := episodes(t)
	for _, id := range []string{"m", "a", "z"} {
		x := ep(id, "ceo", base)
		x.Embedding = []float32{1, 0, 0, 0}
		mustAppend(t, same, x)
	}
	tied, err := same.Recall(context.Background(), learning.RecallQuery{
		Handle: "ceo", Embedding: []float32{1, 0, 0, 0}, Limit: 3,
	})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	want := hitIDs(tied)
	if len(want) != 3 {
		t.Fatalf("hits = %v, want all three", want)
	}
	for range 20 {
		again, _ := same.Recall(context.Background(), learning.RecallQuery{
			Handle: "ceo", Embedding: []float32{1, 0, 0, 0}, Limit: 3,
		})
		if got := hitIDs(again); !slices.Equal(got, want) {
			t.Fatalf("fully-tied ranking is unstable: %v then %v", want, got)
		}
	}
}

func TestRecallRefusesAQueryItCannotAnswer(t *testing.T) {
	t.Parallel()
	e := episodes(t)
	if _, err := e.Recall(context.Background(), learning.RecallQuery{Handle: "ceo"}); !errors.Is(err, learning.ErrNoEmbedding) {
		t.Errorf("err = %v, want ErrNoEmbedding", err)
	}
	if _, err := e.Recall(context.Background(), learning.RecallQuery{Embedding: []float32{1}}); err == nil {
		t.Error("a recall with no seat was accepted")
	}
}

func TestUndefinedSimilarityIsSkippedRatherThanRanked(t *testing.T) {
	t.Parallel()
	// A NaN compares false against everything, so a single one lands
	// wherever the sort's pivot choices put it, and a seat's recall order
	// becomes a property of its data layout. Three ways it arises, all of
	// which happen: a width change mid-life, an embedding of empty text,
	// and a provider returning a non-finite value.
	e := episodes(t)
	good := ep("good", "ceo", base)
	good.Embedding = []float32{1, 0, 0, 0}
	zero := ep("zero", "ceo", base)
	zero.Embedding = []float32{0, 0, 0, 0}
	nan := ep("nan", "ceo", base)
	nan.Embedding = []float32{float32(math.NaN()), 0, 0, 0}
	inf := ep("inf", "ceo", base)
	inf.Embedding = []float32{float32(math.Inf(1)), 0, 0, 0}
	narrow := ep("narrow", "ceo", base)
	narrow.Embedding = []float32{1, 0}
	for _, x := range []learning.Episode{good, zero, nan, inf, narrow} {
		mustAppend(t, e, x)
	}

	hits, err := e.Recall(context.Background(), learning.RecallQuery{
		Handle: "ceo", Embedding: []float32{1, 0, 0, 0}, Limit: 10,
	})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if len(hits) != 1 || hits[0].Episode.ID != "good" {
		t.Errorf("hits = %v, want only the well-defined one", hitIDs(hits))
	}
}

func ids(es []learning.Episode) []string {
	out := make([]string, 0, len(es))
	for _, e := range es {
		out = append(out, e.ID)
	}
	return out
}

func hitIDs(hs []learning.Hit) []string {
	out := make([]string, 0, len(hs))
	for _, h := range hs {
		out = append(out, h.Episode.ID)
	}
	return out
}

func TestListColumnsAlwaysHoldAJSONArray(t *testing.T) {
	t.Parallel()
	// The columns are NOT NULL with a '[]' default, and a nil slice
	// marshals to the four characters "null". Both read back as an empty
	// list through this API, which is why the guard is invisible here — but
	// a dashboard query calling json_array_length() on "null" fails, and
	// the schema says these hold arrays.
	//
	// Found by mutation: dropping the guard changed no Go-visible
	// behaviour, so the property had to be asserted at the column.
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "j.db"), store.Options{})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	e := learning.NewEpisodes(db)
	mustAppend(t, e, ep("a", "ceo", base)) // every list nil

	var toolSeq, skills, exemplars, subjects string
	if err := db.SQL().QueryRowContext(t.Context(),
		`SELECT tool_sequence, skills_used, exemplar_turn_ids, subjects_involved
		 FROM episodes WHERE id = 'a'`).Scan(&toolSeq, &skills, &exemplars, &subjects); err != nil {
		t.Fatalf("read raw columns: %v", err)
	}
	for name, got := range map[string]string{
		"tool_sequence": toolSeq, "skills_used": skills,
		"exemplar_turn_ids": exemplars, "subjects_involved": subjects,
	} {
		if got != "[]" {
			t.Errorf("%s = %q, want an empty JSON array", name, got)
		}
	}
	// And the column really is queryable as an array.
	var n int
	if err := db.SQL().QueryRowContext(t.Context(),
		`SELECT json_array_length(tool_sequence) FROM episodes WHERE id = 'a'`).Scan(&n); err != nil {
		t.Errorf("the column is not a JSON array: %v", err)
	}
}
