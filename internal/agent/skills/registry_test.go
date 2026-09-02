package skills_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/prompts"
	"github.com/crewlet/crewlet/internal/agent/skills"
)

func skill(key string, trigger skills.Trigger, required bool) skills.Skill {
	return skills.Skill{
		Key: key, Title: strings.ToUpper(key[:1]) + key[1:],
		Summary: "how to use " + key, Body: "the body of " + key,
		Trigger: trigger, Required: required,
	}
}

func registry(t *testing.T, in ...skills.Skill) *skills.Registry {
	t.Helper()
	r := skills.NewRegistry()
	for _, s := range in {
		if err := r.Upsert(s); err != nil {
			t.Fatalf("Upsert %s: %v", s.Key, err)
		}
	}
	return r
}

func keysOf(in []skills.Skill) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		out = append(out, s.Key)
	}
	return out
}

// The catalogue offers only what this phase's surface can call: a skill for
// a tool the phase cannot reach is noise the model has to read past.
func TestOnlyTheSkillsForThisSurfaceAreOffered(t *testing.T) {
	t.Parallel()
	r := registry(t,
		skill("chat", skills.Trigger{MCPServer: "mattermost"}, true),
		skill("code", skills.Trigger{MCPServer: "gitlab"}, true),
		skill("recall", skills.Trigger{Tool: "query_episodes"}, false),
	)
	got := keysOf(r.Matching(prompts.PhaseExecute,
		prompts.Surface{Tools: []string{"query_episodes"}, MCPServers: []string{"gitlab"}}))
	if !slices.Equal(got, []string{"code", "recall"}) {
		t.Fatalf("offered %v", got)
	}
}

// KEY-SORTED. The prompt package sorts again — its byte-stability is its own
// promise — but answering in map order here would move every other caller's
// output between two identical builds for no reason anybody could see.
func TestTheCatalogueAnswersInAStableOrder(t *testing.T) {
	t.Parallel()
	r := registry(t,
		skill("zebra", skills.Trigger{Tool: "t"}, true),
		skill("alpha", skills.Trigger{Tool: "t"}, true),
		skill("middle", skills.Trigger{Tool: "t"}, true),
	)
	want := []string{"alpha", "middle", "zebra"}
	for range 8 {
		got := keysOf(r.Matching(prompts.PhaseExecute, prompts.Surface{Tools: []string{"t"}}))
		if !slices.Equal(got, want) {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
}

// A skill declaring phases is offered only in those; one declaring none is
// offered wherever its surface matches, which is the ordinary case — a skill
// about a tool applies wherever that tool can be called.
func TestPhaseScopingIsOptOut(t *testing.T) {
	t.Parallel()
	scoped := skill("review-only", skills.Trigger{Tool: "t"}, true)
	scoped.Phases = []prompts.Phase{prompts.PhaseReview}
	r := registry(t, scoped, skill("everywhere", skills.Trigger{Tool: "t"}, true))

	on := prompts.Surface{Tools: []string{"t"}}
	if got := keysOf(r.Matching(prompts.PhaseReview, on)); !slices.Equal(got,
		[]string{"everywhere", "review-only"}) {
		t.Fatalf("review offered %v", got)
	}
	if got := keysOf(r.Matching(prompts.PhaseExecute, on)); !slices.Equal(got,
		[]string{"everywhere"}) {
		t.Fatalf("execute offered %v", got)
	}
}

// AN UPSERT REPLACES rather than merges: a page IS the skill, so an edit
// that removed a trigger leaf must remove it here, and a merge would keep
// the skill matching a surface its author just stopped claiming.
func TestAnEditReplacesTheSkillWholesale(t *testing.T) {
	t.Parallel()
	r := registry(t, skill("chat", skills.Trigger{AnyOf: []skills.Trigger{
		{MCPServer: "mattermost"}, {MCPServer: "slack"}}}, true))

	narrowed := skill("chat", skills.Trigger{MCPServer: "mattermost"}, true)
	if err := r.Upsert(narrowed); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if got := r.Matching(prompts.PhaseExecute,
		prompts.Surface{MCPServers: []string{"slack"}}); len(got) != 0 {
		t.Fatalf("the removed leaf still matches: %v", keysOf(got))
	}
	if r.Len() != 1 {
		t.Fatalf("an edit produced %d skills", r.Len())
	}
}

// Evict reports whether it removed anything, which is what lets a sync
// worker tell "a skill page was deleted" from "a page that was never a skill
// was deleted" — it evicts on every removal and only one is worth a line.
func TestEvictReportsWhetherItRemovedAnything(t *testing.T) {
	t.Parallel()
	r := registry(t, skill("chat", skills.Trigger{Tool: "t"}, true))
	if !r.Evict("chat") {
		t.Fatal("evicting a registered skill reported nothing removed")
	}
	if r.Evict("chat") || r.Evict("never-existed") {
		t.Fatal("evicting an absent skill reported a removal")
	}
}

// A REPLACE IS ATOMIC, which is what makes a boot walk safe against a
// registry already serving: applied one at a time it would leave a window
// where half the company's guidance exists.
func TestAWalkSwapsTheWholeSetAndRefusesWhatIsInvalid(t *testing.T) {
	t.Parallel()
	r := registry(t, skill("old", skills.Trigger{Tool: "t"}, true))
	broken := skill("broken", skills.Trigger{}, true)

	r.Replace([]skills.Skill{skill("fresh", skills.Trigger{Tool: "t"}, true), broken})
	if _, ok := r.Get("old"); ok {
		t.Fatal("a walk left the previous set behind")
	}
	if _, ok := r.Get("broken"); ok {
		t.Fatal("a skill with no trigger was registered")
	}
	if _, ok := r.Get("fresh"); !ok {
		t.Fatal("the walk's own skill is missing")
	}
}

// A skill with no summary can never be chosen — the summary is the only
// thing that always reaches the prompt — so registering it would store
// something nothing can reach.
func TestASkillThatCannotBeOfferedIsRefused(t *testing.T) {
	t.Parallel()
	r := skills.NewRegistry()
	for _, tc := range []struct {
		name string
		in   skills.Skill
	}{
		{"no key", skills.Skill{Summary: "s", Trigger: skills.Trigger{Tool: "t"}}},
		{"no summary", skills.Skill{Key: "k", Trigger: skills.Trigger{Tool: "t"}}},
		{"no trigger", skills.Skill{Key: "k", Summary: "s"}},
		{"an over-long summary", skills.Skill{Key: "k", Trigger: skills.Trigger{Tool: "t"},
			Summary: strings.Repeat("x", skills.MaxSummaryBytes+1)}},
		{"an over-long body", skills.Skill{Key: "k", Summary: "s",
			Trigger: skills.Trigger{Tool: "t"},
			Body:    strings.Repeat("x", skills.MaxBodyBytes+1)}},
	} {
		if err := r.Upsert(tc.in); err == nil {
			t.Fatalf("%s was accepted", tc.name)
		}
	}
}

// The catalogue renders ${var} references so an operator's facts appear
// substituted rather than as the reference.
func TestTheCatalogueRendersOperatorVariables(t *testing.T) {
	t.Parallel()
	r := registry(t, skills.Skill{
		Key: "chat", Title: "Chat on ${tenant}",
		Summary: "post to ${tenant}.example.com",
		Body:    "the ${tenant} workspace",
		Trigger: skills.Trigger{Tool: "t"},
	})
	r.SetVariables(map[string]string{"tenant": "nimbus"})

	offered := r.SkillsFor(prompts.PhaseExecute, prompts.Surface{Tools: []string{"t"}})
	if len(offered) != 1 {
		t.Fatalf("offered %+v", offered)
	}
	// The prompt package renders through Render, so the SUMMARY comes back
	// raw here and substituted there — which is what keeps the source-byte
	// cap on the summary meaningful.
	if got := r.Render(offered[0].Summary); got != "post to nimbus.example.com" {
		t.Fatalf("rendered summary = %q", got)
	}
	body, ok := r.Body("chat")
	if !ok || !strings.Contains(body, "the nimbus workspace") ||
		!strings.Contains(body, "# Chat on nimbus") {
		t.Fatalf("body = %q", body)
	}
}

// A body carries its TITLE and summary, because a model that asked for a key
// gets back prose with no header otherwise — and a body it cannot attribute
// is a body it cannot decide to trust.
func TestALoadedBodyNamesWhatItIs(t *testing.T) {
	t.Parallel()
	r := registry(t, skill("chat", skills.Trigger{Tool: "t"}, true))
	body, ok := r.Body("chat")
	if !ok {
		t.Fatal("the skill has no body")
	}
	for _, want := range []string{"# Chat", "how to use chat", "the body of chat"} {
		if !strings.Contains(body, want) {
			t.Fatalf("the body is missing %q:\n%s", want, body)
		}
	}
	if _, ok := r.Body("never-existed"); ok {
		t.Fatal("an absent key produced a body")
	}
}

// A NIL REGISTRY is what a company that published no skills has, and every
// read must answer rather than panic — the prompt path consults it on every
// phase.
func TestANilRegistryAnswersEmpty(t *testing.T) {
	t.Parallel()
	var r *skills.Registry
	if got := r.Matching(prompts.PhaseExecute, prompts.Surface{Tools: []string{"t"}}); got != nil {
		t.Fatalf("Matching = %v", got)
	}
	if _, ok := r.Get("chat"); ok {
		t.Fatal("a nil registry has a skill")
	}
	if got := r.Render("${tenant}"); got != "${tenant}" {
		t.Fatalf("Render = %q", got)
	}
	if r.Len() != 0 || r.Evict("chat") {
		t.Fatal("a nil registry reported content")
	}
	r.SetVariables(map[string]string{"a": "b"})
	r.Audit(nil, nil)
}
