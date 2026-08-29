package builtin_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/learning"
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
