package builtin_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/learning"
	"github.com/crewlet/crewlet/internal/org"
)

// The company's own retrieval_limit, honoured. It was validated (1..20),
// schema'd and documented as "Episode-query hits returned", and read by
// nothing: setting `retrieval_limit: 20` produced a new revision and changed
// nothing an operator could observe.
func TestQueryEpisodesHonoursTheConfiguredRetrievalLimit(t *testing.T) {
	t.Parallel()
	episodes := &countingEpisodes{}
	tool := registered(t, builtin.Deps{Episodes: episodes, EpisodeLimit: 17},
		builtin.QueryEpisodesTool)
	turn := turnFor(t, "agent-ceo")

	callFor(t, tool, turn, map[string]any{})
	if episodes.limit != 17 {
		t.Errorf("recalled %d turns, want the company's 17", episodes.limit)
	}
	// The model's own argument still wins, and is still bounded by what a
	// prompt can carry rather than by what an operator asked for.
	callFor(t, tool, turn, map[string]any{"limit": 3})
	if episodes.limit != 3 {
		t.Errorf("recalled %d turns, want the model's 3", episodes.limit)
	}
	callFor(t, tool, turn, map[string]any{"limit": 500})
	if episodes.limit != 25 {
		t.Errorf("recalled %d turns, want the prompt ceiling of 25", episodes.limit)
	}
}

// A registry built with no company still gets a working tool, not one that
// returns nothing.
func TestQueryEpisodesFallsBackToTheShippedLimit(t *testing.T) {
	t.Parallel()
	episodes := &countingEpisodes{}
	tool := registered(t, builtin.Deps{Episodes: episodes}, builtin.QueryEpisodesTool)

	callFor(t, tool, turnFor(t, "agent-ceo"), map[string]any{})
	if episodes.limit != builtin.DefaultEpisodeLimit {
		t.Errorf("recalled %d turns, want the shipped %d",
			episodes.limit, builtin.DefaultEpisodeLimit)
	}
}

// countingEpisodes records the limit it was asked for.
type countingEpisodes struct{ limit int }

func (c *countingEpisodes) Recent(_ context.Context, _ string, limit int) ([]learning.Episode, error) {
	c.limit = limit
	return nil, nil
}

func (c *countingEpisodes) ForConversation(_ context.Context, _, _ string, limit int) ([]learning.Episode, error) {
	c.limit = limit
	return nil, nil
}

// The body cap. It was documented as a runaway guard — "Ceiling on a refined
// skill's body" — and enforced nowhere: refine_skill clipped only the note it
// records beside the archived version, and never measured the body it stored.
// A skill that grows an annotation per turn grows without bound.
func TestARefinementOverTheBodyCapIsRefused(t *testing.T) {
	t.Parallel()
	skills := &recordingSkills{skill: learning.Skill{ID: "s-1", Name: "deploys", Version: 3}}
	tool := registered(t, builtin.Deps{Refinable: skills, SkillBodyMax: 100},
		builtin.RefineSkillTool)

	res := callFor(t, tool, turnFor(t, "agent-ceo"), map[string]any{
		"skill_name": "deploys",
		"content":    strings.Repeat("x", 200),
	})
	if !res.Failed {
		t.Fatalf("a 200-character body was accepted under a cap of 100: %q", res.Output)
	}
	if !strings.Contains(res.Output, "max_body_chars") {
		t.Errorf("the refusal does not name the setting: %q", res.Output)
	}
	// REFUSED, not truncated, and therefore not written: half a procedure
	// is worse than the one the seat already has.
	if skills.updated {
		t.Error("the over-cap body was stored anyway")
	}
}

// And the history bound reaches the store. `max_versions_kept` was hardcoded
// at 10, so a company that set 3 or 40 got 10 either way — with the store's
// own comment noting the prune is the ONLY bound on that table.
func TestARefinementCarriesTheConfiguredVersionBound(t *testing.T) {
	t.Parallel()
	skills := &recordingSkills{skill: learning.Skill{ID: "s-1", Name: "deploys", Version: 3}}
	tool := registered(t, builtin.Deps{Refinable: skills, SkillVersionsKept: 3},
		builtin.RefineSkillTool)

	callFor(t, tool, turnFor(t, "agent-ceo"), map[string]any{
		"skill_name": "deploys", "content": "step one",
	})
	if !skills.updated {
		t.Fatal("the refinement was not stored")
	}
	if skills.refinement.KeepVersions != 3 {
		t.Errorf("KeepVersions = %d, want the company's 3", skills.refinement.KeepVersions)
	}
}

// recordingSkills is a RefinableSkills that remembers what it was asked to do.
type recordingSkills struct {
	skill      learning.Skill
	updated    bool
	refinement learning.Refinement
}

func (r *recordingSkills) Get(context.Context, string, string) (learning.Skill, bool, error) {
	return r.skill, true, nil
}

func (r *recordingSkills) Update(_ context.Context, _ string, _ learning.Revision,
	ref learning.Refinement,
) (learning.Skill, error) {
	r.updated, r.refinement = true, ref
	return r.skill, nil
}

func (r *recordingSkills) List(context.Context, string, learning.ListOptions) ([]learning.Skill, error) {
	return []learning.Skill{r.skill}, nil
}

func (r *recordingSkills) MarkUsed(context.Context, string, time.Time) learning.Use {
	return learning.Use{}
}

// The skill lifecycle's telemetry. `skill_used` was a registered type with a
// topic, a summary and a category, and NOTHING anywhere constructed it: the
// builtin bumped a database counter and said nothing, so "are the skills the
// synthesizer drafts ever loaded again" — the one question skill induction has
// to answer to be worth its cost — was answerable only by diffing a column.
func TestLoadingASkillIsPublished(t *testing.T) {
	t.Parallel()
	out := &recordingTelemetry{}
	skills := &recordingSkills{skill: learning.Skill{
		ID: "s-1", Name: "deploys", Content: "step one",
	}}
	tool := registered(t, builtin.Deps{Skills: skills, Events: out}, builtin.UseSkillTool)

	turn := turnFor(t, "agent-ceo")
	res := callFor(t, tool, turn, map[string]any{"skill_name": "deploys"})
	if res.Failed {
		t.Fatalf("use_skill failed: %q", res.Output)
	}
	if len(out.sent) != 1 {
		t.Fatalf("published %d events, want one per load (topics %v)", len(out.sent), out.topics)
	}
	payload, ok := out.sent[0].Data.(*types.SkillUsed)
	if !ok {
		t.Fatalf("payload is %T", out.sent[0].Data)
	}
	if payload.SkillName != "deploys" || payload.SkillID != "s-1" {
		t.Errorf("event names %q/%q", payload.SkillName, payload.SkillID)
	}
	if payload.SourceKind != types.SkillSourceSynthesized {
		t.Errorf("source kind = %q, want %q — a company-published tool skill "+
			"and a seat reusing its own answer different questions",
			payload.SourceKind, types.SkillSourceSynthesized)
	}
	if payload.AgentHandle != "agent-ceo" || payload.TurnID != turn.ID {
		t.Errorf("event does not place the load: handle %q turn %q",
			payload.AgentHandle, payload.TurnID)
	}
	if out.sent[0].Source != "agent-ceo" {
		t.Errorf("source = %q, want the seat — the activity feed groups on it",
			out.sent[0].Source)
	}
}

// A refinement is published with its VERSION, which is what makes successive
// refinements of one skill distinguishable in the feed.
func TestARefinementIsPublished(t *testing.T) {
	t.Parallel()
	out := &recordingTelemetry{}
	skills := &recordingSkills{skill: learning.Skill{ID: "s-1", Name: "deploys", Version: 4}}
	tool := registered(t, builtin.Deps{Refinable: skills, Events: out}, builtin.RefineSkillTool)

	callFor(t, tool, turnFor(t, "agent-ceo"), map[string]any{
		"skill_name": "deploys", "content": "step one",
	})
	if len(out.sent) != 1 {
		t.Fatalf("published %d events, want one (topics %v)", len(out.sent), out.topics)
	}
	payload, ok := out.sent[0].Data.(*types.SkillRefined)
	if !ok {
		t.Fatalf("payload is %T", out.sent[0].Data)
	}
	if payload.SkillVersion != 4 {
		t.Errorf("version = %d, want the stored 4", payload.SkillVersion)
	}
	if payload.RefinementKind == "" {
		t.Error("no refinement kind — a success annotation and a counter-example " +
			"read the same without it")
	}
}

// A publish that fails must not cost the model the skill it asked for: the
// load already happened, and the event describes it rather than causing it.
func TestAFailedPublishStillReturnsTheSkill(t *testing.T) {
	t.Parallel()
	out := &recordingTelemetry{err: errors.New("the broker is unreachable")}
	skills := &recordingSkills{skill: learning.Skill{
		ID: "s-1", Name: "deploys", Content: "step one",
	}}
	tool := registered(t, builtin.Deps{Skills: skills, Events: out}, builtin.UseSkillTool)

	res := callFor(t, tool, turnFor(t, "agent-ceo"), map[string]any{"skill_name": "deploys"})
	if res.Failed {
		t.Errorf("a telemetry failure cost the agent its skill: %q", res.Output)
	}
	if !strings.Contains(res.Output, "step one") {
		t.Errorf("output = %q, want the skill body", res.Output)
	}
}

// recordingTelemetry captures what a builtin published.
type recordingTelemetry struct {
	topics []string
	sent   []*events.Event
	err    error
}

func (r *recordingTelemetry) Publish(_ context.Context, topic string, ev *events.Event) error {
	r.topics = append(r.topics, topic)
	r.sent = append(r.sent, ev)
	return r.err
}

// --- the pull side of the Plan phase's two searches ----------------------- //

// The re-query-after-recon path the docs lean on, and which did not exist:
// query_episodes declared `conversation` and `limit` and nothing else, so the
// escape hatch for a thin trigger — a pointer with no content, where the Plan
// block deliberately renders a hint instead of a search — had nothing to
// search with.
func TestQueryEpisodesSearchesByMeaning(t *testing.T) {
	t.Parallel()
	recall := &fakeRecall{hits: []learning.Hit{
		{Episode: learning.Episode{TaskSummary: "fixed the search indexing bug",
			ReviewOutcome: "done"}},
	}}
	episodes := &countingEpisodes{}
	tool := registered(t, builtin.Deps{Episodes: episodes, Recall: recall},
		builtin.QueryEpisodesTool)

	res := callFor(t, tool, turnFor(t, "agent-ceo"), map[string]any{
		"query": "search indexing",
	})
	if res.Failed {
		t.Fatalf("query_episodes failed: %q", res.Output)
	}
	if recall.text != "search indexing" {
		t.Errorf("searched for %q, want the model's own words", recall.text)
	}
	if episodes.limit != 0 {
		t.Error("a semantic query fell through to the recency read")
	}
	if !strings.Contains(res.Output, "fixed the search indexing bug") {
		t.Errorf("output = %q, want the hit", res.Output)
	}
}

// "Nothing resembles this" and "this deployment cannot search by meaning" send
// a model to opposite places: the second has a fallback it can still use, so
// it must not read as the first.
func TestQueryEpisodesSaysWhenItCannotSearchByMeaning(t *testing.T) {
	t.Parallel()
	tool := registered(t, builtin.Deps{Episodes: &countingEpisodes{}},
		builtin.QueryEpisodesTool)

	res := callFor(t, tool, turnFor(t, "agent-ceo"), map[string]any{"query": "anything"})
	if !res.Failed {
		t.Fatalf("a query with no recall configured reported success: %q", res.Output)
	}
	if !strings.Contains(res.Output, "embeddings") {
		t.Errorf("the refusal does not say why: %q", res.Output)
	}
}

// The outcome filter, and the over-fetch that makes it usable: the search
// ranks by similarity and the filter is applied to what came back, so asking
// for two and keeping only the failures would otherwise return none.
func TestQueryEpisodesFiltersByOutcome(t *testing.T) {
	t.Parallel()
	recall := &fakeRecall{hits: []learning.Hit{
		{Episode: learning.Episode{TaskSummary: "one", ReviewOutcome: "done"}},
		{Episode: learning.Episode{TaskSummary: "two", ReviewOutcome: "failed"}},
		{Episode: learning.Episode{TaskSummary: "three", ReviewOutcome: "done"}},
	}}
	tool := registered(t, builtin.Deps{Episodes: &countingEpisodes{}, Recall: recall},
		builtin.QueryEpisodesTool)

	res := callFor(t, tool, turnFor(t, "agent-ceo"), map[string]any{
		"query": "anything", "outcome_filter": "FAILED", "limit": 2,
	})
	if strings.Contains(res.Output, "one") || strings.Contains(res.Output, "three") {
		t.Errorf("output = %q, want only the failed turn", res.Output)
	}
	if !strings.Contains(res.Output, "two") {
		t.Errorf("output = %q, want the failed turn", res.Output)
	}
	if recall.limit <= 2 {
		t.Errorf("searched for %d hits with a filter on a limit of 2 — a filter "+
			"applied to what came back needs a wider search", recall.limit)
	}
}

// refresh_memory's own escape hatch. It declared `limit` and nothing else, so
// the mid-turn re-filter its own docs describe — with the per-turn cap and the
// idempotency cache — was advertised and absent.
func TestRefreshMemoryRefiltersOnAHint(t *testing.T) {
	t.Parallel()
	recall := &fakeRecall{notes: []learning.DiaryEntry{
		{Kind: "convention", Content: "semantic commit messages"},
	}}
	diary := &countingDiary{}
	tool := registered(t, builtin.Deps{Diary: diary, Recall: recall},
		builtin.RefreshMemoryTool)

	res := callFor(t, tool, turnFor(t, "agent-ceo"), map[string]any{
		"context_hint": "fixing the indexing bug",
	})
	if res.Failed {
		t.Fatalf("refresh_memory failed: %q", res.Output)
	}
	if recall.hint != "fixing the indexing bug" {
		t.Errorf("filtered on %q", recall.hint)
	}
	if diary.limit != 0 {
		t.Error("a hinted refresh fell through to the recency dump")
	}
	if !strings.Contains(res.Output, "semantic commit messages") {
		t.Errorf("output = %q, want the selected note", res.Output)
	}
}

// The cap, and the idempotency that makes it fair. The filter is an auxiliary
// model call, so a model that re-hints every round spends a completion per
// round for answers that converge after the second — but charging a REPEAT
// against the cap would teach it to vary its wording instead.
func TestRefreshMemoryCapsDistinctHintsAndNotRepeats(t *testing.T) {
	t.Parallel()
	recall := &fakeRecall{notes: []learning.DiaryEntry{{Kind: "k", Content: "c"}}}
	tool := registered(t, builtin.Deps{
		Diary: &countingDiary{}, Recall: recall, RefreshesPerTurn: 2,
	}, builtin.RefreshMemoryTool)
	turn := turnFor(t, "agent-ceo")

	for _, hint := range []string{"first thing", "second thing"} {
		if res := callFor(t, tool, turn, map[string]any{"context_hint": hint}); res.Failed {
			t.Fatalf("hint %q was refused inside the budget: %q", hint, res.Output)
		}
	}
	// A repeat, spelled differently. Case and surrounding whitespace are
	// normalised because a model does not spell consistently and the same
	// question asked twice is not a second question.
	if res := callFor(t, tool, turn, map[string]any{"context_hint": "  First Thing "}); res.Failed {
		t.Errorf("a repeat of an already-used hint was charged against the cap: %q", res.Output)
	}
	res := callFor(t, tool, turn, map[string]any{"context_hint": "third thing"})
	if !res.Failed {
		t.Fatalf("a third distinct hint was allowed under a cap of 2: %q", res.Output)
	}
	if !strings.Contains(res.Output, "2") {
		t.Errorf("the refusal does not say what the limit is: %q", res.Output)
	}

	// PER TURN. A different turn starts with the whole budget, or one busy
	// turn would silence the tool for every turn after it.
	other := turnFor(t, "agent-ceo")
	other.ID = "wk-2"
	if res := callFor(t, tool, other, map[string]any{"context_hint": "third thing"}); res.Failed {
		t.Errorf("a new turn inherited the previous turn's spend: %q", res.Output)
	}
}

// A REPEAT IS ANSWERED FROM THE LEDGER, not merely left uncharged.
//
// The filter is an auxiliary model call. If a repeat re-ran it for free the
// cap would bound nothing: a model alternating two hints could spend a
// completion per round for the rest of the turn without ever taking a third
// slot.
func TestARepeatedHintCostsNoSecondFilterCall(t *testing.T) {
	t.Parallel()
	recall := &fakeRecall{notes: []learning.DiaryEntry{
		{Kind: "preference", Content: "one"},
		{Kind: "preference", Content: "two"},
	}}
	tool := registered(t, builtin.Deps{
		Diary: &countingDiary{}, Recall: recall, RefreshesPerTurn: 2,
	}, builtin.RefreshMemoryTool)
	turn := turnFor(t, "agent-ceo")

	first := callFor(t, tool, turn, map[string]any{"context_hint": "the indexing bug"})
	if first.Failed {
		t.Fatalf("the first call was refused: %q", first.Output)
	}
	if recall.memoryCalls != 1 {
		t.Fatalf("filter calls = %d, want 1", recall.memoryCalls)
	}
	again := callFor(t, tool, turn, map[string]any{"context_hint": "The Indexing Bug  "})
	if again.Failed {
		t.Fatalf("the repeat was refused: %q", again.Output)
	}
	if recall.memoryCalls != 1 {
		t.Fatalf("filter calls = %d — a repeat fired a second auxiliary call, "+
			"so the per-turn cap bounds nothing", recall.memoryCalls)
	}
	// The same notes, in the same order. The heading echoes the caller's
	// own spelling of the hint, so only the rows are compared.
	if notesIn(again.Output) != notesIn(first.Output) {
		t.Fatalf("the repeat answered with different notes:\n%q\nvs\n%q",
			again.Output, first.Output)
	}
}

// THE CACHE HOLDS THE ROWS, NOT THE RENDERING. A repeat asking for more notes
// than the first call printed gets them — the auxiliary call is the expensive
// half, and keying the cap on `limit` instead would let a model spend a
// completion per integer.
func TestARepeatedHintHonoursItsOwnLimit(t *testing.T) {
	t.Parallel()
	recall := &fakeRecall{notes: []learning.DiaryEntry{
		{Kind: "preference", Content: "one"},
		{Kind: "preference", Content: "two"},
		{Kind: "preference", Content: "three"},
	}}
	tool := registered(t, builtin.Deps{
		Diary: &countingDiary{}, Recall: recall, RefreshesPerTurn: 2,
	}, builtin.RefreshMemoryTool)
	turn := turnFor(t, "agent-ceo")

	narrow := callFor(t, tool, turn, map[string]any{"context_hint": "indexing", "limit": 1})
	if strings.Contains(narrow.Output, "two") {
		t.Fatalf("limit=1 printed more than one note:\n%s", narrow.Output)
	}
	wide := callFor(t, tool, turn, map[string]any{"context_hint": "indexing", "limit": 3})
	if !strings.Contains(wide.Output, "three") {
		t.Fatalf("the repeat was answered with the first call's rendering:\n%s", wide.Output)
	}
	if recall.memoryCalls != 1 {
		t.Fatalf("filter calls = %d, want the second answered from the ledger", recall.memoryCalls)
	}
}

// A HINT WHOSE FILTER FAILED STILL COSTS ITS SLOT, and is not answered from an
// empty cache entry. Otherwise a failing call is retryable without bound —
// the same unbounded spend the cap exists to stop.
func TestAFailedFilterSpendsItsSlotAndIsNotCached(t *testing.T) {
	t.Parallel()
	recall := &fakeRecall{err: errors.New("the aux model is down")}
	tool := registered(t, builtin.Deps{
		Diary: &countingDiary{}, Recall: recall, RefreshesPerTurn: 1,
	}, builtin.RefreshMemoryTool)
	turn := turnFor(t, "agent-ceo")

	if res := callFor(t, tool, turn, map[string]any{"context_hint": "indexing"}); !res.Failed {
		t.Fatalf("a failed filter reported success: %q", res.Output)
	}
	// The retry is allowed — the slot is already this hint's — and reaches
	// the filter rather than being answered with nothing.
	res := callFor(t, tool, turn, map[string]any{"context_hint": "indexing"})
	if !res.Failed || recall.memoryCalls != 2 {
		t.Fatalf("retry: failed=%v calls=%d, want the filter re-tried",
			res.Failed, recall.memoryCalls)
	}
	// But a DIFFERENT hint is refused: the failed one spent the budget.
	if res := callFor(t, tool, turn, map[string]any{"context_hint": "something else"}); !res.Failed {
		t.Fatalf("a second distinct hint was allowed under a cap of 1: %q", res.Output)
	}
	if recall.memoryCalls != 2 {
		t.Fatalf("filter calls = %d — the refused hint reached the model", recall.memoryCalls)
	}
}

// "NOTHING BEARS ON THIS" IS AN ANSWER, and is cached like any other. A repeat
// would otherwise cost another completion to be told the same thing — and the
// empty answer is exactly the one a model is most likely to ask again.
func TestAnEmptyFilterAnswerIsCachedToo(t *testing.T) {
	t.Parallel()
	recall := &fakeRecall{} // the filter matches nothing
	tool := registered(t, builtin.Deps{
		Diary: &countingDiary{}, Recall: recall, RefreshesPerTurn: 2,
	}, builtin.RefreshMemoryTool)
	turn := turnFor(t, "agent-ceo")

	first := callFor(t, tool, turn, map[string]any{"context_hint": "indexing"})
	if first.Failed || !strings.Contains(first.Output, "Nothing in your notes") {
		t.Fatalf("first call: failed=%v %q", first.Failed, first.Output)
	}
	again := callFor(t, tool, turn, map[string]any{"context_hint": "INDEXING"})
	if again.Failed || !strings.Contains(again.Output, "Nothing in your notes") {
		t.Fatalf("repeat: failed=%v %q", again.Failed, again.Output)
	}
	if recall.memoryCalls != 1 {
		t.Fatalf("filter calls = %d — an empty answer was not cached, so the "+
			"hint a model repeats most costs a completion every time", recall.memoryCalls)
	}
}

// THE LEDGER IS BOUNDED. A turn's entries are dead the moment it ends and
// nothing tells the tool when that was, so without the bound this map is a
// leak that grows for the life of the process.
func TestTheHintLedgerForgetsOldTurns(t *testing.T) {
	t.Parallel()
	recall := &fakeRecall{notes: []learning.DiaryEntry{{Kind: "k", Content: "c"}}}
	tool := registered(t, builtin.Deps{
		Diary: &countingDiary{}, Recall: recall, RefreshesPerTurn: 1,
	}, builtin.RefreshMemoryTool)

	oldest := turnFor(t, "agent-ceo")
	oldest.ID = "wk-oldest"
	if res := callFor(t, tool, oldest, map[string]any{"context_hint": "indexing"}); res.Failed {
		t.Fatalf("the first call was refused: %q", res.Output)
	}
	// Well past the bound, so the first turn's entry must have fallen out.
	for i := range builtin.HintLedgerTurns + 1 {
		turn := turnFor(t, "agent-ceo")
		turn.ID = fmt.Sprintf("wk-%d", i)
		callFor(t, tool, turn, map[string]any{"context_hint": "indexing"})
	}
	before := recall.memoryCalls
	// The oldest turn asking its own hint again is a MISS now: its entry
	// is gone, so the filter runs rather than the answer being replayed.
	callFor(t, tool, oldest, map[string]any{"context_hint": "indexing"})
	if recall.memoryCalls != before+1 {
		t.Fatalf("filter calls %d -> %d: the ledger still holds a turn %d turns "+
			"old, so it grows for the life of the process",
			before, recall.memoryCalls, builtin.HintLedgerTurns+1)
	}
}

// notesIn is the bullet lines of a rendered digest, without its heading.
func notesIn(out string) string {
	_, notes, _ := strings.Cut(out, "\n\n")
	return notes
}

// fakeRecall stands in for the Plan phase's searches.
type fakeRecall struct {
	hits  []learning.Hit
	notes []learning.DiaryEntry
	err   error
	text  string
	hint  string
	limit int
	// memoryCalls counts what the ledger's cache is there to avoid.
	memoryCalls int
}

func (f *fakeRecall) RecallEpisodes(_ context.Context, _ *org.Role, text string, limit int) ([]learning.Hit, error) {
	f.text, f.limit = text, limit
	if f.err != nil {
		return nil, f.err
	}
	return f.hits, nil
}

func (f *fakeRecall) RecallMemories(_ context.Context, _ *org.Role, _, hint string) ([]learning.DiaryEntry, error) {
	f.hint = hint
	f.memoryCalls++
	if f.err != nil {
		return nil, f.err
	}
	return f.notes, nil
}

// countingDiary records the limit its recency read was asked for.
type countingDiary struct{ limit int }

func (c *countingDiary) Write(context.Context, learning.DiaryEntry) error { return nil }

func (c *countingDiary) Recent(_ context.Context, _ string, _ time.Time, limit int) ([]learning.DiaryEntry, error) {
	c.limit = limit
	return nil, nil
}
