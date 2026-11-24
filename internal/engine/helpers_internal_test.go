package engine

import (
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/providers/llm/harness"
	"github.com/crewlet/crewlet/internal/providers/llm/harness/harnessprofile"
)

// A HELPER'S RUN HAS NO WEB TOOL, WHATEVER ITS ENTRY SAYS. Ask Crewlet is
// assigned the ordinary entry the seats run with their web tools on, and the
// run the engine builds for it carries the empty allow list and no tool of its
// own, while the seats' run of the same entry keeps WebFetch and WebSearch; an
// entry no helper names gets no helper's run. Mutation: build the helper from
// the entry's own overrides and its argv keeps the web.
func TestAHelpersRunHasNoWebToolWhateverItsEntrySays(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	doc := fmt.Sprintf(`name: Acme
providers:
  llm:
    sonnet:
      harness: claude-code
      model: sonnet
      state_dir: %[1]s/sonnet
      overrides: {web_tools: allowed}
    other:
      harness: claude-code
      model: haiku
      state_dir: %[1]s/other
assistant:
  model: sonnet
`, dir)
	c, err := NewCompany(parseCompany(t, doc))
	if err != nil {
		t.Fatal(err)
	}
	seat, ok := c.Models.Provider("sonnet")
	if !ok {
		t.Fatal("the seats' run of the entry was not built")
	}
	if args := seat.(*harness.Provider).Profile().CompleteArgs; !slices.Contains(args, "WebFetch,WebSearch") {
		t.Errorf("the seats' run lost its web tools: %q", args)
	}
	helper, ok := c.Helpers["sonnet"].(*harness.Provider)
	if !ok {
		t.Fatalf("Ask Crewlet's model has no helper's run: %v", c.Helpers)
	}
	profile := helper.Profile()
	if profile.EffectiveWebTools() != harnessprofile.WebToolsDenied || profile.ToolsOffProblem() != "" {
		t.Errorf("the helper's run keeps a tool of its own: web %s, %s", profile.EffectiveWebTools(),
			profile.ToolsOffProblem())
	}
	if slices.Contains(profile.CompleteArgs, "WebFetch,WebSearch") {
		t.Errorf("the helper's argv allows the web: %q", profile.CompleteArgs)
	}
	if _, built := c.Helpers["other"]; built {
		t.Error("an entry no helper names was built as a helper's run")
	}
}

// A HELPER READS IMAGES WHERE ITS HARNESS TAKES THEM: Ask Crewlet assigned a
// Claude Code entry hands a question's images to it, and one assigned a Pi
// entry, whose profile declares no image input, says it cannot see them.
// Mutation: answer true for every model and the Pi case goes red.
func TestAHelperReadsImagesWhereItsHarnessTakesThem(t *testing.T) {
	t.Parallel()
	for harness, want := range map[string]bool{"claude-code": true, "pi": false} {
		dir := t.TempDir()
		doc := fmt.Sprintf(`name: Acme
providers:
  llm:
    helper:
      harness: %s
      model: sonnet
      state_dir: %s/helper
assistant:
  model: helper
`, harness, dir)
		c, err := NewCompany(parseCompany(t, doc))
		if err != nil {
			t.Fatal(err)
		}
		e := &Engine{}
		e.epoch.current.Store(c)
		if got := AssistantModels(e).ReadsImages(); got != want {
			t.Errorf("%s: ReadsImages = %v, want %v", harness, got, want)
		}
	}
}

// A HELPER'S MODEL FOLLOWS THE REVISION THE NODE RUNS, never the one it booted
// on: each applied revision assigns or unassigns both helpers with no restart.
// Mutation: resolve a helper once and keep it, and later epochs read the first.
func TestAHelpersModelFollowsTheRevisionTheNodeRuns(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	epoch := func(helpers string) *Company {
		t.Helper()
		c, err := NewCompany(parseCompany(t, fmt.Sprintf(`name: Acme
providers:
  llm:
    sonnet:
      harness: claude-code
      model: sonnet
      state_dir: %s/sonnet
%s`, dir, helpers)))
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	e := &Engine{}
	assigned := func() (string, bool) {
		t.Helper()
		_, run, err := assistantModels{e: e}.entry(false)
		key := run.key
		if err != nil {
			key = ""
		}
		return key, AdvisorModels(e).Assigned()
	}
	install := func(c *Company) { e.epoch.current.Store(c) }

	install(epoch(""))
	if ask, advisor := assigned(); ask != "" || advisor {
		t.Fatalf("booted with no helper model: Ask Crewlet on %q, Advisor assigned %v", ask, advisor)
	}
	install(epoch("assistant: {model: sonnet}\nadvisor: {model: sonnet}\n"))
	if ask, advisor := assigned(); ask != "sonnet" || !advisor {
		t.Errorf("after the revision assigning both: Ask Crewlet on %q, Advisor assigned %v", ask, advisor)
	}
	deps, err := AdvisorModels(e).Deps(t.Context(), nil)
	if err != nil || deps.ProviderKey != "sonnet" {
		t.Errorf("the Advisor's model pass runs on %q (%v), want sonnet", deps.ProviderKey, err)
	}
	install(epoch(""))
	if ask, advisor := assigned(); ask != "" || advisor {
		t.Errorf("after the revision assigning none: Ask Crewlet on %q, Advisor assigned %v", ask, advisor)
	}
}

// A HELPER ON A FLEET IS BUILT AND RUNS ON A MEMBER: a company whose Ask
// Crewlet and Advisor are set to a fleet builds (a helper's run and a search
// run of each member), each helper takes a member's run, and Ask Crewlet
// searches through its member's search run. On a fleet with no member the
// company still builds, and each helper answers the empty fleet's reason
// rather than a model the company does not build.
//
// Mutation: read assistant.model as an entry key in the search build, and the
// company with members is refused.
func TestAHelperOnAFleetIsBuiltAndRunsOnAMember(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	epoch := func(members string) *Company {
		t.Helper()
		c, err := NewCompany(parseCompany(t, fmt.Sprintf(`name: Acme
providers:
  llm:
    sonnet:
      harness: claude-code
      model: sonnet
      state_dir: %s/sonnet
  llm_fleets:
    team:
      name: Team
      members: [%s]
assistant: {model: "fleet:team"}
advisor: {model: "fleet:team"}
`, dir, members)))
		if err != nil {
			t.Fatalf("a company with its helpers on a fleet of [%s] was refused: %v", members, err)
		}
		return c
	}
	e := &Engine{}
	e.epoch.current.Store(epoch("sonnet"))
	_, run, err := assistantModels{e: e}.entry(false)
	if err != nil || run.key != "sonnet" || run.fleet.Fleet != "team" {
		t.Fatalf("Ask Crewlet on the fleet = %+v, %v; want sonnet, picked by team", run, err)
	}
	if !e.CanResearchSearch() || e.researchSearcher() != e.Company().Searchers["sonnet"] {
		t.Errorf("Ask Crewlet on the fleet has no search run of its member")
	}
	deps, err := AdvisorModels(e).Deps(t.Context(), nil)
	if err != nil || deps.ProviderKey != "sonnet" {
		t.Errorf("the Advisor on the fleet runs on %q (%v), want sonnet", deps.ProviderKey, err)
	}

	e.epoch.current.Store(epoch(""))
	var empty *phase.EmptyFleetError
	if _, _, err := (assistantModels{e: e}).entry(true); !errors.As(err, &empty) || empty.Fleet != "team" {
		t.Errorf("Ask Crewlet on an empty fleet = %v, want the fleet's empty reason", err)
	}
	if _, err := AdvisorModels(e).Deps(t.Context(), nil); !errors.As(err, &empty) {
		t.Errorf("the Advisor on an empty fleet = %v, want the fleet's empty reason", err)
	}
	if e.CanResearchSearch() {
		t.Error("Ask Crewlet on an empty fleet offers a search")
	}
}
