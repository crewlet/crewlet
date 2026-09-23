package learning_test

import (
	"context"
	"errors"
	"fmt"
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

// win is one vector as the single-window set [learning.Episode.Embeddings]
// holds.
//
// Almost every episode has exactly this shape — a summary short enough to be
// one window is almost every summary — so the tests that are not ABOUT
// windowing say so once here rather than spelling the extra slice each time.
func win(v ...float32) [][]float32 { return [][]float32{v} }

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

// listed reads one page, failing the test on an error.
func listed(t *testing.T, e *learning.Episodes, q learning.EpisodeQuery) learning.EpisodePage {
	t.Helper()
	page, err := e.List(t.Context(), q)
	if err != nil {
		t.Fatalf("List(%+v): %v", q, err)
	}
	return page
}

// THE FILTER RUNS BEFORE THE LIMIT. Applied to what a LIMIT returned, "the
// newest failure" is the failures among the newest few turns — so a seat whose
// recent turns all succeeded is told it has never failed, which is the answer
// query_episodes gave before the filter moved into the statement.
func TestAnOlderFailureIsFoundPastAPageOfNewerSuccesses(t *testing.T) {
	t.Parallel()
	e := episodes(t)
	failure := ep("old-failure", "ceo", base)
	failure.ReviewOutcome = "failed"
	mustAppend(t, e, failure)
	for i := range 5 {
		mustAppend(t, e, ep(fmt.Sprintf("success-%d", i), "ceo",
			base.Add(time.Duration(i+1)*time.Hour)))
	}

	page := listed(t, e, learning.EpisodeQuery{
		Handle: "ceo", Filter: learning.EpisodeFilter{Outcome: "failed"}, Limit: 2,
	})
	if got := ids(page.Episodes); len(page.Episodes) != 1 || got[0] != "old-failure" {
		t.Errorf("failed turns = %v, want the one failure behind five successes", got)
	}
	if page.Truncated {
		t.Error("a page holding every match reported more")
	}
}

// TRUNCATED IS EVIDENCE, NOT ARITHMETIC. A page exactly as long as the limit
// is what a seat holding exactly that many turns reads, and telling it there
// are more would send it paging for rows that do not exist.
func TestAPageIsTruncatedOnlyWhenARowLiesPastIt(t *testing.T) {
	t.Parallel()
	e := episodes(t)
	for i := range 3 {
		mustAppend(t, e, ep(fmt.Sprintf("e%d", i), "ceo", base.Add(time.Duration(i)*time.Minute)))
	}

	exact := listed(t, e, learning.EpisodeQuery{Handle: "ceo", Limit: 3})
	if len(exact.Episodes) != 3 || exact.Truncated {
		t.Errorf("limit 3 over 3 turns = %v truncated=%v, want all three and no more",
			ids(exact.Episodes), exact.Truncated)
	}
	short := listed(t, e, learning.EpisodeQuery{Handle: "ceo", Limit: 2})
	if got := ids(short.Episodes); len(got) != 2 || got[0] != "e2" || got[1] != "e1" || !short.Truncated {
		t.Errorf("limit 2 over 3 turns = %v truncated=%v, want the newest two and more",
			got, short.Truncated)
	}
	// And the rest is the next page, which is what the flag promises.
	rest := listed(t, e, learning.EpisodeQuery{Handle: "ceo", Offset: 2, Limit: 2})
	if got := ids(rest.Episodes); len(got) != 1 || got[0] != "e0" || rest.Truncated {
		t.Errorf("the page after = %v truncated=%v, want the oldest turn and no more",
			got, rest.Truncated)
	}
}

// Both filters narrow together, and no conversation is not "every
// conversation's turns and none of the unkeyed ones": the zero filter keeps
// everything, and a conversation keeps only its own.
func TestAConversationAndAnOutcomeNarrowTogether(t *testing.T) {
	t.Parallel()
	e := episodes(t)
	unkeyed := ep("unkeyed", "ceo", base)
	doneHere := ep("done-here", "ceo", base.Add(time.Minute))
	doneHere.ConversationKey = "slack:C1"
	failedHere := ep("failed-here", "ceo", base.Add(2*time.Minute))
	failedHere.ConversationKey = "slack:C1"
	failedHere.ReviewOutcome = "failed"
	failedElsewhere := ep("failed-elsewhere", "ceo", base.Add(3*time.Minute))
	failedElsewhere.ConversationKey = "slack:C2"
	failedElsewhere.ReviewOutcome = "failed"
	otherSeat := ep("other-seat", "cto", base.Add(4*time.Minute))
	otherSeat.ConversationKey = "slack:C1"
	otherSeat.ReviewOutcome = "failed"
	for _, episode := range []learning.Episode{unkeyed, doneHere, failedHere, failedElsewhere, otherSeat} {
		mustAppend(t, e, episode)
	}

	for _, tc := range []struct {
		filter learning.EpisodeFilter
		want   []string
	}{
		{learning.EpisodeFilter{}, []string{"failed-elsewhere", "failed-here", "done-here", "unkeyed"}},
		{learning.EpisodeFilter{Conversation: "slack:C1"}, []string{"failed-here", "done-here"}},
		{learning.EpisodeFilter{Outcome: "failed"}, []string{"failed-elsewhere", "failed-here"}},
		{learning.EpisodeFilter{Conversation: "slack:C1", Outcome: "failed"}, []string{"failed-here"}},
	} {
		page := listed(t, e, learning.EpisodeQuery{Handle: "ceo", Filter: tc.filter, Limit: 10})
		if got := ids(page.Episodes); !slices.Equal(got, tc.want) {
			t.Errorf("filter %+v = %v, want %v", tc.filter, got, tc.want)
		}
	}
}

// A listing states its own size. Zero has no honest default here — the
// caller is the one that knows what the rows are for — and a negative offset
// is a position that does not exist.
func TestAListingRefusesWhatItCannotAnswer(t *testing.T) {
	t.Parallel()
	e := episodes(t)
	for _, q := range []learning.EpisodeQuery{
		{Limit: 5},
		{Handle: "ceo"},
		{Handle: "ceo", Limit: 5, Offset: -1},
	} {
		if _, err := e.List(t.Context(), q); err == nil {
			t.Errorf("List(%+v) answered, want a refusal", q)
		}
	}
}

func TestAnEmbeddingSurvivesAndAMissingOneIsNotAFailure(t *testing.T) {
	t.Parallel()
	// Nil is a supported state: a transient embeddings outage must never
	// cost an episode. Recall skips such a row; every other query returns
	// it.
	e := episodes(t)
	withVec := ep("a", "ceo", base)
	withVec.Embeddings = win(0.1, 0.2, 0.3, 0.4)
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
	if len(byID["a"].Embeddings) != 1 || len(byID["a"].Embeddings[0]) != 4 ||
		byID["a"].Embeddings[0][0] != 0.1 {
		t.Errorf("embeddings = %v", byID["a"].Embeddings)
	}
	if byID["b"].Embeddings != nil {
		t.Errorf("an absent embedding came back as %v", byID["b"].Embeddings)
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
	bad.Embeddings = win(0.1, 0.2)
	if _, err := e.Append(context.Background(), bad); err == nil {
		t.Fatal("a wrong-width embedding was written")
	}
	// The counterfactual: the configured width goes in.
	good := ep("b", "ceo", base)
	good.Embeddings = win(0.1, 0.2, 0.3, 0.4)
	if _, err := e.Append(context.Background(), good); err != nil {
		t.Errorf("a correctly-sized embedding was refused: %v", err)
	}
}

func TestRecallRanksBySimilarity(t *testing.T) {
	t.Parallel()
	e := episodes(t)
	near := ep("near", "ceo", base)
	near.Embeddings = win(1, 0, 0, 0)
	far := ep("far", "ceo", base.Add(time.Minute))
	far.Embeddings = win(0, 1, 0, 0)
	mid := ep("mid", "ceo", base.Add(2*time.Minute))
	mid.Embeddings = win(0.9, 0.4, 0, 0)
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

// THE FILTER NARROWS WHAT IS RANKED, NOT WHAT THE RANKING RETURNED. The
// failure here is the least similar row the seat has, so a filter applied to
// the top of the ranking — which is what `query_episodes` did, four times over
// the limit — never reaches it, and the seat is told nothing like this ever
// failed. Both row shapes are covered: a filter that reached only the
// one-window branch would miss the multi-window failure.
func TestRecallFiltersBeforeItRanks(t *testing.T) {
	t.Parallel()
	e := episodes(t)
	for i := range 4 {
		success := ep(fmt.Sprintf("success-%d", i), "ceo", base.Add(time.Duration(i)*time.Minute))
		success.Embeddings = win(1, 0, 0, 0)
		mustAppend(t, e, success)
	}
	failure := ep("failure", "ceo", base.Add(time.Hour))
	failure.ReviewOutcome = "failed"
	failure.Embeddings = win(0.8, 0.6, 0, 0)
	mustAppend(t, e, failure)
	longFailure := ep("long-failure", "ceo", base.Add(2*time.Hour))
	longFailure.ReviewOutcome = "failed"
	longFailure.ConversationKey = "slack:C1"
	longFailure.Embeddings = [][]float32{{0, 1, 0, 0}, {0.7, 0.71, 0, 0}}
	mustAppend(t, e, longFailure)
	// The nearest row of all, and a multi-window one that matches no
	// filter below: a filter missing from that branch lets it through.
	longSuccess := ep("long-success", "ceo", base.Add(3*time.Hour))
	longSuccess.ConversationKey = "slack:C2"
	longSuccess.Embeddings = [][]float32{{0, 1, 0, 0}, {1, 0, 0, 0}}
	mustAppend(t, e, longSuccess)

	recall := func(f learning.EpisodeFilter, limit, offset int) []string {
		t.Helper()
		hits, err := e.Recall(t.Context(), learning.RecallQuery{
			Handle: "ceo", Embedding: []float32{1, 0, 0, 0},
			Limit: limit, Offset: offset, Filter: f,
		})
		if err != nil {
			t.Fatalf("Recall(%+v): %v", f, err)
		}
		return hitIDs(hits)
	}

	if got := recall(learning.EpisodeFilter{Outcome: "failed"}, 1, 0); len(got) != 1 || got[0] != "failure" {
		t.Errorf("the nearest failure = %v, want the one ranked behind four successes", got)
	}
	if got := recall(learning.EpisodeFilter{Outcome: "failed"}, 5, 0); !slices.Equal(got, []string{"failure", "long-failure"}) {
		t.Errorf("every failure = %v, want both, the multi-window one included", got)
	}
	if got := recall(learning.EpisodeFilter{Conversation: "slack:C1"}, 5, 0); !slices.Equal(got, []string{"long-failure"}) {
		t.Errorf("one conversation = %v, want only its own episode", got)
	}
	// AND THE RANKING READS ON: the page after the first failure is the
	// second one, which is where a caller told there is more goes next.
	if got := recall(learning.EpisodeFilter{Outcome: "failed"}, 1, 1); !slices.Equal(got, []string{"long-failure"}) {
		t.Errorf("the second failure = %v, want the next one in the ranking", got)
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
	seen.Embeddings = win(1, 0, 0, 0)
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
	mine.Embeddings = win(1, 0, 0, 0)
	theirs := ep("theirs", "cto", base)
	theirs.Embeddings = win(1, 0, 0, 0)
	cluster := ep("cluster", "ceo", base)
	cluster.Embeddings = win(1, 0, 0, 0)
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
		x.Embeddings = win(1, 0, 0, 0)
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
		x.Embeddings = win(1, 0, 0, 0)
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

// THE WIDTH GUARD IS EXACT, and the window count is what makes it exact again.
//
// A row's blob is N vectors packed end to end, so "is this row in the query's
// embedding space" is `length(embedding) = embedding_windows * width` and
// nothing weaker. Divisibility is the weaker one that looks right: a
// three-window row of 4-wide vectors is 48 bytes, which divides exactly by a
// 2-wide probe — so a company that changed embedding model would find rows
// from the old space admitted, sliced along boundaries that are not theirs,
// and scored by vector_distance_cos on the result.
//
// The cost is not a wrong number in a field nobody reads. The bad row SORTS,
// and it spends a slot of the LIMIT before the Go pass can refuse it — so a
// real memory is pushed out of the answer by a row that cannot be an answer at
// all, which is a recall that silently under-delivers rather than one that
// errors. That is exactly the failure [store.DB.EncodeVector] refuses
// non-finite vectors to avoid, arriving by the other door.
func TestAnEpisodeFromAnotherEmbeddingSpaceNeverSpendsARecallSlot(t *testing.T) {
	t.Parallel()
	// No configured width, which is what lets one store hold rows from two
	// embedding spaces — the state a company that changed model is in.
	e := episodes(t)

	// The row from the OLD space: three 4-wide windows, 48 bytes. Its first
	// two floats are the query's own vector, so read at the query's width
	// it would score a PERFECT match and sort ahead of everything.
	wrongSpace := ep("wrong-space", "ceo", base.Add(time.Hour))
	wrongSpace.Embeddings = [][]float32{{1, 0, 0, 0}, {0, 0, 0, 0}, {0, 0, 0, 0}}
	// The row that genuinely answers, in the current 2-wide space, at a
	// similarity above the floor but below a perfect match.
	current := ep("real", "ceo", base)
	current.Embeddings = win(0.9, 0.436)
	for _, x := range []learning.Episode{wrongSpace, current} {
		mustAppend(t, e, x)
	}

	hits, err := e.Recall(context.Background(), learning.RecallQuery{
		Handle: "ceo", Embedding: []float32{1, 0}, Limit: 1,
	})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if len(hits) != 1 || hits[0].Episode.ID != "real" {
		t.Fatalf("hits = %v, want the one row in this embedding space — a row "+
			"from another one took the slot and was then dropped in Go, so the "+
			"seat recalled nothing", hitIDs(hits))
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
	good.Embeddings = win(1, 0, 0, 0)
	zero := ep("zero", "ceo", base)
	zero.Embeddings = win(0, 0, 0, 0)
	nan := ep("nan", "ceo", base)
	nan.Embeddings = win(float32(math.NaN()), 0, 0, 0)
	inf := ep("inf", "ceo", base)
	inf.Embeddings = win(float32(math.Inf(1)), 0, 0, 0)
	narrow := ep("narrow", "ceo", base)
	narrow.Embeddings = win(1, 0)
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

// A POISONED EMBEDDING DOES NOT COST A REAL RESULT.
//
// The ranking is the database's now, so the LIMIT is spent before Go sees a
// row. A vector holding a NaN scores 0 there — a PERFECT match — so it sorts
// FIRST, and the Go re-score that correctly refuses it then hands back one
// hit fewer than the seat had. Under the old all-rows-into-Go loop this could
// not happen; moving the limit into SQL is what made it possible, and the
// write-side guard in store.EncodeVector is what closes it.
//
// Four rows and a limit of three: without the guard this returns two.
func TestAPoisonedEmbeddingDoesNotCostARealHit(t *testing.T) {
	t.Parallel()
	e := episodes(t)
	poison := ep("poison", "ceo", base.Add(3*time.Minute))
	poison.Embeddings = win(float32(math.NaN()), 0, 0, 0)
	for i, v := range [][]float32{{1, 0, 0, 0}, {0.99, 0.1, 0, 0}, {0.95, 0.2, 0, 0}} {
		good := ep(fmt.Sprintf("good-%d", i), "ceo", base.Add(time.Duration(i)*time.Minute))
		good.Embeddings = win(v...)
		mustAppend(t, e, good)
	}
	mustAppend(t, e, poison)

	hits, err := e.Recall(context.Background(), learning.RecallQuery{
		Handle: "ceo", Embedding: []float32{1, 0, 0, 0}, Limit: 3,
	})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if len(hits) != 3 {
		t.Fatalf("hits = %v, want all three well-defined episodes — a poisoned "+
			"row that reached the table would have taken one of the slots", hitIDs(hits))
	}
	for _, h := range hits {
		if h.Episode.ID == "poison" {
			t.Error("a NaN embedding was ranked as a hit")
		}
	}
}

// AND THE EPISODE ITSELF SURVIVES. The guard must cost the vector, never the
// row: an episode is the record of a turn that really happened, and losing one
// to a bad response from an embeddings API is the failure the nullable column
// exists to prevent.
func TestANonFiniteEmbeddingCostsTheVectorAndNotTheEpisode(t *testing.T) {
	t.Parallel()
	e := episodes(t)
	bad := ep("bad", "ceo", base)
	bad.Embeddings = win(1, float32(math.Inf(-1)), 0, 0)
	if _, err := e.Append(context.Background(), bad); err != nil {
		t.Fatalf("a non-finite embedding failed the whole write: %v", err)
	}
	got, err := e.Recent(context.Background(), "ceo", 10)
	if err != nil {
		t.Fatalf("Recent: %v", err)
	}
	if len(got) != 1 || got[0].ID != "bad" {
		t.Fatalf("recent = %v, want the episode written without its vector", ids(got))
	}
	if got[0].Embeddings != nil {
		t.Errorf("the non-finite vector was stored as %v", got[0].Embeddings)
	}
}
