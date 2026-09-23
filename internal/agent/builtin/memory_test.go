package builtin_test

import (
	"context"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/learning"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/store"
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

// countingEpisodes records the listing it was asked for.
type countingEpisodes struct {
	limit int
	query learning.EpisodeQuery
}

func (c *countingEpisodes) List(_ context.Context, q learning.EpisodeQuery) (learning.EpisodePage, error) {
	c.limit, c.query = q.Limit, q
	return learning.EpisodePage{}, nil
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
	if !strings.Contains(res.Output, "max_body_bytes") {
		t.Errorf("the refusal does not name the setting: %q", res.Output)
	}
	// REFUSED, not truncated, and therefore not written: half a procedure
	// is worse than the one the seat already has.
	if skills.updated {
		t.Error("the over-cap body was stored anyway")
	}
}

// ONE STORE, ONE RULE. Three writers share the diary's content bound and they
// used to disagree: reflect_and_persist refused an over-long note naming the
// limit, while mark_onboarded and refine_skill's `reason` silently clipped at
// the same number and reported success. A clipped note is worse than a refused
// one — the seat reads back half of what it wrote and cannot tell — and the
// refusal has to NAME the limit or a model has nothing to aim at.
func TestEveryDiaryWriterRefusesAnOverLongNoteAndNamesTheCap(t *testing.T) {
	t.Parallel()
	over := strings.Repeat("z", learning.MaxContentBytes+1)
	at := strings.Repeat("z", learning.MaxContentBytes)

	for _, tc := range []struct {
		name string
		args func(content string) map[string]any
		tool string
		deps func() (builtin.Deps, func() bool)
	}{
		{
			name: "mark_onboarded",
			tool: builtin.MarkOnboardedTool,
			args: func(c string) map[string]any { return map[string]any{"notes": c} },
			deps: func() (builtin.Deps, func() bool) {
				m := &onboardingStore{}
				return builtin.Deps{Onboarding: m}, func() bool { return len(m.marks) > 0 }
			},
		},
		{
			name: "refine_skill reason",
			tool: builtin.RefineSkillTool,
			args: func(c string) map[string]any {
				return map[string]any{"skill_name": "deploys", "content": "step one", "reason": c}
			},
			deps: func() (builtin.Deps, func() bool) {
				sk := &recordingSkills{skill: learning.Skill{ID: "s-1", Name: "deploys"}}
				return builtin.Deps{Refinable: sk}, func() bool { return sk.updated }
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			deps, wrote := tc.deps()
			res := callFor(t, registered(t, deps, tc.tool), turnFor(t, "agent-ceo"), tc.args(over))
			if !res.Failed {
				t.Fatalf("an over-long note was accepted: %q", res.Output)
			}
			if !strings.Contains(res.Output, strconv.Itoa(learning.MaxContentBytes)) {
				t.Errorf("the refusal does not name the cap: %q", res.Output)
			}
			if wrote() {
				t.Error("the over-long value was written anyway")
			}

			deps, wrote = tc.deps()
			if res := callFor(t, registered(t, deps, tc.tool), turnFor(t, "agent-ceo"),
				tc.args(at)); res.Failed {
				t.Errorf("a note exactly at the cap was refused: %q", res.Output)
			} else if !wrote() {
				t.Error("a note at the cap was accepted but not written")
			}
		})
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
	if payload.AgentHandle != "agent-ceo" || payload.TurnID != turn.RunID {
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

// --- the pull side of the turn-start prefetch's two searches -------------- //

// The re-query-after-recon path the docs lean on, and which did not exist:
// query_episodes declared `conversation` and `limit` and nothing else, so the
// escape hatch for a thin trigger (a pointer with no content, where the
// prefetch block deliberately renders a hint instead of a search) had nothing
// to search with.
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

// THE FILTERS REACH THE STORE, on both paths, rather than being applied to
// what came back. Applied afterwards to a similarity search, "the failures like
// this" is the failures among the nearest few turns — and the tool used to
// widen the search fourfold to make that usable, which still told a seat whose
// failures ranked below its successes that nothing like this had ever failed.
func TestTheFiltersNarrowTheSearchRatherThanItsAnswer(t *testing.T) {
	t.Parallel()
	recall := &fakeRecall{}
	episodes := &countingEpisodes{}
	tool := registered(t, builtin.Deps{Episodes: episodes, Recall: recall},
		builtin.QueryEpisodesTool)
	turn := turnFor(t, "agent-ceo")

	callFor(t, tool, turn, map[string]any{
		"query": "anything", "outcome_filter": " FAILED ", "conversation": "jira:ENG-1",
		"limit": 2, "offset": 4,
	})
	want := learning.EpisodeFilter{Conversation: "jira:ENG-1", Outcome: "failed"}
	if recall.filter != want {
		t.Errorf("the search was filtered on %+v, want %+v", recall.filter, want)
	}
	if recall.offset != 4 {
		t.Errorf("the search started %d in, want the model's offset of 4", recall.offset)
	}
	// ONE PAST THE PAGE, as evidence the ranking goes on — never a
	// multiple of it, which is what a filter over the answer needed.
	if recall.limit != 3 {
		t.Errorf("searched for %d hits on a limit of 2, want 3: the page and one "+
			"row of evidence", recall.limit)
	}

	callFor(t, tool, turn, map[string]any{
		"outcome_filter": "done", "conversation": "jira:ENG-1", "limit": 2, "offset": 4,
	})
	got := episodes.query
	if got.Handle != "agent-ceo" || got.Filter != (learning.EpisodeFilter{
		Conversation: "jira:ENG-1", Outcome: "done"}) || got.Offset != 4 || got.Limit != 2 {
		t.Errorf("the listing asked for %+v", got)
	}
}

// A SEARCH BY MEANING SAYS WHEN ITS RANKING GOES ON, from a hit it asked for
// past the page rather than from a page that happens to be full.
func TestASearchByMeaningSaysWhenMoreMatch(t *testing.T) {
	t.Parallel()
	hit := func(summary string) learning.Hit {
		return learning.Hit{Episode: learning.Episode{TaskSummary: summary, ReviewOutcome: "done"}}
	}
	recall := &fakeRecall{hits: []learning.Hit{hit("first"), hit("second"), hit("third")}}
	tool := registered(t, builtin.Deps{Episodes: &countingEpisodes{}, Recall: recall},
		builtin.QueryEpisodesTool)
	turn := turnFor(t, "agent-ceo")

	page := callFor(t, tool, turn, map[string]any{"query": "anything", "limit": 2})
	if strings.Contains(page.Output, "third") || !strings.Contains(page.Output, "offset 2") {
		t.Errorf("a page of two over three hits = %q, want two and the offset of the rest",
			page.Output)
	}
	whole := callFor(t, tool, turn, map[string]any{"query": "anything", "limit": 3})
	if strings.Contains(whole.Output, "offset") {
		t.Errorf("a page holding every hit claimed there were more: %q", whole.Output)
	}
	rest := callFor(t, tool, turn, map[string]any{"query": "anything", "limit": 2, "offset": 2})
	if !strings.Contains(rest.Output, "third") || strings.Contains(rest.Output, "offset 4") {
		t.Errorf("the page after = %q, want the last hit and nothing more", rest.Output)
	}
}

// AN OUTCOME NO TURN IS REMEMBERED WITH IS REFUSED, naming the ones that
// exist. Answered, it reads as "none of your turns ended that way" — true,
// and a model that guessed `success` learns nothing from it.
func TestAnOutcomeNoTurnCanHaveIsRefused(t *testing.T) {
	t.Parallel()
	episodes := &countingEpisodes{}
	tool := registered(t, builtin.Deps{Episodes: episodes}, builtin.QueryEpisodesTool)

	res := callFor(t, tool, turnFor(t, "agent-ceo"), map[string]any{"outcome_filter": "success"})
	if !res.Failed {
		t.Fatalf("an outcome no turn has was answered: %q", res.Output)
	}
	for _, outcome := range learning.SettledOutcomes() {
		if !strings.Contains(res.Output, outcome) {
			t.Errorf("the refusal does not name %q: %q", outcome, res.Output)
		}
	}
	if episodes.limit != 0 {
		t.Error("the refused call still read the store")
	}
}

// A PAGE THAT IS NOT EVERY MATCHING TURN SAYS SO, and the offset it names reads
// the rest. A silent page reads as the seat's whole history.
func TestAPageOfTurnsSaysWhereTheRestIs(t *testing.T) {
	t.Parallel()
	store := &newestEpisodes{}
	for i := range 5 {
		store.episodes = append(store.episodes, learning.Episode{
			Handle: "agent-ceo", TaskSummary: fmt.Sprintf("turn %d", 5-i),
			ReviewOutcome: "done",
		})
	}
	tool := registered(t, builtin.Deps{Episodes: store}, builtin.QueryEpisodesTool)
	turn := turnFor(t, "agent-ceo")

	first := callFor(t, tool, turn, map[string]any{"limit": 2})
	if !strings.Contains(first.Output, "offset 2") {
		t.Fatalf("a page of two over five turns does not name the offset of the "+
			"rest:\n%s", first.Output)
	}
	var seen []string
	for offset := 0; ; offset += 2 {
		res := callFor(t, tool, turn, map[string]any{"limit": 2, "offset": offset})
		for i := 5; i >= 1; i-- {
			if strings.Contains(res.Output, fmt.Sprintf("turn %d\n", i)) {
				seen = append(seen, strconv.Itoa(i))
			}
		}
		if !strings.Contains(res.Output, fmt.Sprintf("offset %d", offset+2)) {
			if offset+2 < 5 {
				t.Fatalf("the page at offset %d stopped naming the rest:\n%s",
					offset, res.Output)
			}
			break
		}
	}
	if strings.Join(seen, ",") != "5,4,3,2,1" {
		t.Errorf("paging read turns %v, want every one once, newest first", seen)
	}

	// And the page that IS the whole answer says nothing of more.
	whole := callFor(t, tool, turn, map[string]any{"limit": 5})
	if strings.Contains(whole.Output, "offset") {
		t.Errorf("a page holding every turn claimed there were more:\n%s", whole.Output)
	}
}

// A FOLDED ROW IS PRINTED AS WHAT IT IS. It has no task summary, so printed as a
// turn it was a timestamp that said nothing — the one record left of the turns
// it replaced, reading as none.
func TestACompactedRowShowsItsPattern(t *testing.T) {
	t.Parallel()
	store := &newestEpisodes{episodes: []learning.Episode{{
		Handle: "agent-ceo", Kind: learning.KindCompacted, Count: 14,
		CommonTaskPattern: "triaged inbound bug reports", CommonOutcome: "mostly routed",
		ReviewOutcome: "done",
	}}}
	tool := registered(t, builtin.Deps{Episodes: store}, builtin.QueryEpisodesTool)
	res := callFor(t, tool, turnFor(t, "agent-ceo"), nil)
	for _, want := range []string{"14 earlier turns", "triaged inbound bug reports", "mostly routed"} {
		if !strings.Contains(res.Output, want) {
			t.Errorf("a compacted row renders without %q:\n%s", want, res.Output)
		}
	}
}

// newestEpisodes is a store whose episodes are already newest first, and
// which pages them as the store does.
type newestEpisodes struct{ episodes []learning.Episode }

func (n *newestEpisodes) List(_ context.Context, q learning.EpisodeQuery) (learning.EpisodePage, error) {
	rest := n.episodes[min(q.Offset, len(n.episodes)):]
	page := learning.EpisodePage{Episodes: rest}
	if len(rest) > q.Limit {
		page.Episodes, page.Truncated = rest[:q.Limit], true
	}
	return page, nil
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
	other.RunID = "run-2"
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

// A PAGE OF A HINT'S ANSWER SAYS IT IS A PAGE, and names the offset that
// reads the rest.
//
// The heading says these are the notes that bear on the hint, so a page cut
// at `limit` with nothing after it reads as the whole set: a model shown five
// of seven relevant notes could not tell a short answer from a short set. The
// rest is reachable only because a repeat is answered from the ledger — the
// next offset has to be into the SAME ranked answer, not a fresh filter call.
func TestAHintedPageThatIsNotTheWholeAnswerSaysWhereTheRestIs(t *testing.T) {
	t.Parallel()
	var notes []learning.DiaryEntry
	for _, n := range []string{"one", "two", "three", "four", "five", "six", "seven"} {
		notes = append(notes, learning.DiaryEntry{Kind: "preference", Content: "note " + n})
	}
	recall := &fakeRecall{notes: notes}
	tool := registered(t, builtin.Deps{
		Diary: &countingDiary{}, Recall: recall, RefreshesPerTurn: 1,
	}, builtin.RefreshMemoryTool)
	turn := turnFor(t, "agent-ceo")

	first := callFor(t, tool, turn, map[string]any{"context_hint": "indexing"})
	if first.Failed {
		t.Fatalf("the first page was refused: %q", first.Output)
	}
	if strings.Contains(first.Output, "note six") {
		t.Fatalf("the default page printed past its limit:\n%s", first.Output)
	}
	if !strings.Contains(first.Output, "2 more") || !strings.Contains(first.Output, "offset 5") {
		t.Fatalf("a page of five out of seven did not say two more exist or "+
			"where they are:\n%s", first.Output)
	}

	rest := callFor(t, tool, turn, map[string]any{"context_hint": "indexing", "offset": 5})
	if rest.Failed {
		t.Fatalf("the offset the first page named was refused: %q", rest.Output)
	}
	for _, want := range []string{"note six", "note seven"} {
		if !strings.Contains(rest.Output, want) {
			t.Errorf("the second page is missing %q:\n%s", want, rest.Output)
		}
	}
	if strings.Contains(rest.Output, "note five") || strings.Contains(rest.Output, "more bear") {
		t.Errorf("the last page repeated a note or claimed more exist:\n%s", rest.Output)
	}
	if recall.memoryCalls != 1 {
		t.Fatalf("filter calls = %d — the second page re-ran the filter, so "+
			"its offset is into a different answer", recall.memoryCalls)
	}

	past := callFor(t, tool, turn, map[string]any{"context_hint": "indexing", "offset": 9})
	if past.Failed || !strings.Contains(past.Output, "7 of your notes") {
		t.Errorf("an offset past the end did not say how many there are: %q", past.Output)
	}
}

// A PAGE OF RECENT NOTES THAT STOPS SHORT OF THE OLDEST SAYS SO, and the
// offset it names reads on until every note has been read.
//
// A seat holding more notes than one page was shown the newest few under a
// heading that read as all of them, and nothing reached the rest. The store
// takes no offset, so the page is read with one row past its end: that probe
// is the only thing that can say older notes exist.
func TestRecentNotesArePagedAndSayWhenOlderOnesExist(t *testing.T) {
	t.Parallel()
	var notes []learning.DiaryEntry
	for i := range 7 {
		notes = append(notes, learning.DiaryEntry{Kind: "preference",
			Content: "note " + strconv.Itoa(i)})
	}
	diary := &newestFirst{notes: notes}
	tool := registered(t, builtin.Deps{Diary: diary}, builtin.RefreshMemoryTool)
	turn := turnFor(t, "agent-ceo")

	first := callFor(t, tool, turn, nil)
	if first.Failed {
		t.Fatalf("the first page failed: %q", first.Output)
	}
	if strings.Contains(first.Output, "note 5") {
		t.Fatalf("the default page printed past its limit:\n%s", first.Output)
	}
	if !strings.Contains(first.Output, "older notes") || !strings.Contains(first.Output, "offset 5") {
		t.Fatalf("a page of five of seven notes did not say older ones exist "+
			"or where they are:\n%s", first.Output)
	}

	rest := callFor(t, tool, turn, map[string]any{"offset": 5})
	if rest.Failed {
		t.Fatalf("the offset the first page named was refused: %q", rest.Output)
	}
	for _, want := range []string{"note 5", "note 6", "6 to 7 of 7"} {
		if !strings.Contains(rest.Output, want) {
			t.Errorf("the last page is missing %q:\n%s", want, rest.Output)
		}
	}
	if strings.Contains(rest.Output, "note 4") || strings.Contains(rest.Output, "older notes") {
		t.Errorf("the last page repeated a note or claimed older ones exist:\n%s", rest.Output)
	}

	all := callFor(t, tool, turn, map[string]any{"limit": 7})
	if !strings.Contains(all.Output, "All 7 of your notes") || strings.Contains(all.Output, "older notes") {
		t.Errorf("a page holding every note did not say it was all of them:\n%s", all.Output)
	}

	past := callFor(t, tool, turn, map[string]any{"offset": 9})
	if past.Failed || !strings.Contains(past.Output, "You have 7 notes") {
		t.Errorf("an offset past the end did not say how many there are: %q", past.Output)
	}
	huge := callFor(t, tool, turn, map[string]any{"offset": strconv.Itoa(math.MaxInt)})
	if huge.Failed || !strings.Contains(huge.Output, "You have 7 notes") {
		t.Errorf("an offset whose read size overflows was not answered as past "+
			"the end: %q", huge.Output)
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
	oldest.RunID = "run-oldest"
	if res := callFor(t, tool, oldest, map[string]any{"context_hint": "indexing"}); res.Failed {
		t.Fatalf("the first call was refused: %q", res.Output)
	}
	// Well past the bound, so the first turn's entry must have fallen out.
	for i := range builtin.HintLedgerTurns + 1 {
		turn := turnFor(t, "agent-ceo")
		turn.RunID = fmt.Sprintf("wk-%d", i)
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

// fakeRecall stands in for the turn-start prefetch's searches.
type fakeRecall struct {
	hits   []learning.Hit
	notes  []learning.DiaryEntry
	err    error
	text   string
	hint   string
	filter learning.EpisodeFilter
	offset int
	limit  int
	// memoryCalls counts what the ledger's cache is there to avoid.
	memoryCalls int
}

func (f *fakeRecall) RecallEpisodes(_ context.Context, _ *org.Role, text string,
	filter learning.EpisodeFilter, offset, limit int,
) ([]learning.Hit, error) {
	f.text, f.filter, f.offset, f.limit = text, filter, offset, limit
	if f.err != nil {
		return nil, f.err
	}
	// Paged as the store pages a ranking: offset in, at most limit.
	rest := f.hits[min(offset, len(f.hits)):]
	return rest[:min(limit, len(rest))], nil
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

// newestFirst is a diary whose notes are already in the store's order, newest
// first, and which reads at most limit of them as the store does.
type newestFirst struct{ notes []learning.DiaryEntry }

func (n *newestFirst) Write(context.Context, learning.DiaryEntry) error { return nil }

func (n *newestFirst) Recent(_ context.Context, _ string, _ time.Time, limit int) ([]learning.DiaryEntry, error) {
	return n.notes[:min(limit, len(n.notes))], nil
}

// realDiary is the store's own diary over a fresh file, so a kind or a
// deadline the store refuses is refused here too.
func realDiary(t *testing.T) *learning.Diary {
	t.Helper()
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "diary.db"), store.Options{})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return learning.NewDiary(db)
}

// EVERY KIND THE TOOL OFFERS IS ONE THE STORE KEEPS. The tool used to offer
// `diary_short` in its schema, describe it as `short`, and give no note a
// deadline — so the store refused `diary_short` for having none, and `short`
// became a durable note nobody asked for. Held against the real diary, because
// a fake that accepts anything is how that shipped.
func TestEveryKindTheToolOffersIsOneTheStoreKeeps(t *testing.T) {
	t.Parallel()
	diary := realDiary(t)
	tool := registered(t, builtin.Deps{Diary: diary}, builtin.ReflectAndPersistTool)
	turn := turnFor(t, "agent-ceo")
	agentID, _ := turn.Org.AgentIDFor(turn.Seat)

	kinds := tool.Parameters()["properties"].(map[string]any)["kind"].(map[string]any)["enum"].([]any)
	for _, kind := range kinds {
		res := callFor(t, tool, turn, map[string]any{
			"content": fmt.Sprintf("a %v fact", kind), "kind": kind,
		})
		if res.Failed {
			t.Errorf("kind %v, which the tool offers, was refused: %s", kind, res.Output)
		}
	}
	res := callFor(t, tool, turn, map[string]any{
		"content": "the freeze runs this week", "kind": string(learning.DiaryShort), "ttl_days": 3,
	})
	if res.Failed {
		t.Fatalf("a short note with its own duration was refused: %s", res.Output)
	}
	// A DURATION ALONE MAKES A SHORT NOTE, which is how the company's own
	// guidance tells a seat to keep one: "pass ttl_days for facts that age
	// out naturally".
	res = callFor(t, tool, turn, map[string]any{"content": "the review waits on MR 7", "ttl_days": 5})
	if res.Failed {
		t.Fatalf("a note with a duration and no kind was refused: %s", res.Output)
	}

	now := time.Now().UTC()
	kept, err := diary.Recent(t.Context(), agentID.String(), now, 10)
	if err != nil {
		t.Fatalf("Recent: %v", err)
	}
	deadlines := map[string]time.Time{}
	for _, e := range kept {
		deadlines[e.Content] = e.TTLUntil
		if e.Content == "a diary_short fact" && e.Kind != learning.DiaryShort {
			t.Errorf("a short note was kept as %s", e.Kind)
		}
	}
	within := func(got time.Time, days int) bool {
		want := now.Add(time.Duration(days) * 24 * time.Hour)
		return got.After(want.Add(-time.Minute)) && got.Before(want.Add(time.Minute))
	}
	if got := deadlines["a diary_short fact"]; !within(got, learning.ShortTTLDefaultDays) {
		t.Errorf("a short note with no ttl_days expires %v, want the tier's %d-day default",
			got, learning.ShortTTLDefaultDays)
	}
	if got := deadlines["the freeze runs this week"]; !within(got, 3) {
		t.Errorf("a short note asked for 3 days expires %v", got)
	}
	if got := deadlines["the review waits on MR 7"]; !within(got, 5) {
		t.Errorf("a note given 5 days and no kind expires %v", got)
	}
	if got := deadlines["a diary_long fact"]; !got.IsZero() {
		t.Errorf("a durable note carries a deadline, %v", got)
	}
}

// A KIND OR A DURATION THE TOOL CANNOT HONOUR IS REFUSED, naming what it
// takes, rather than kept as something the model did not ask for.
func TestANoteTheToolCannotKeepAsAskedIsRefused(t *testing.T) {
	t.Parallel()
	d := &diaryStore{}
	tool := registered(t, builtin.Deps{Diary: d}, builtin.ReflectAndPersistTool)
	turn := turnFor(t, "agent-ceo")
	for name, args := range map[string]map[string]any{
		"an unknown kind":          {"kind": "short"},
		"a deadline on a long one": {"kind": string(learning.DiaryLong), "ttl_days": 5},
		"too long to be short":     {"kind": string(learning.DiaryShort), "ttl_days": learning.ShortTTLMaxDays + 1},
		"no time at all":           {"kind": string(learning.DiaryShort), "ttl_days": 0},
	} {
		args["content"] = "a fact"
		res := callFor(t, tool, turn, args)
		if !res.Failed {
			t.Errorf("%s was kept: %s", name, res.Output)
		}
	}
	if len(d.wrote) != 0 {
		t.Errorf("refused notes were written: %+v", d.wrote)
	}
}
