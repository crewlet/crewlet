package e2e

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/prompts"
	"github.com/crewlet/crewlet/internal/agent/skills"
	"github.com/crewlet/crewlet/internal/engine"
)

// The tool-skill path, end to end: a page an operator published reaches the
// prompt of a phase that can call the tools it is about.
//
// The claim is not that the registry works — its own suite covers that — but
// that a real node builds one, syncs pages into it, and offers it to the
// phases through the same admission path a real page takes.

// pagesFor renders skills back into page text, so this exercises the
// admission path rather than reaching past it into the registry.
func pagesFor(in []skills.Skill) []engine.Page {
	out := make([]engine.Page, 0, len(in))
	for _, s := range in {
		phases := []string{"plan", "execute"}
		required := "true"
		if !s.Required {
			required = "false"
		}
		out = append(out, engine.Page{
			ID: s.Key + "-page", Title: s.Title, Version: 1,
			Text: "---\nkey: " + s.Key + "\ntitle: " + s.Title +
				"\nsummary: " + s.Summary +
				"\nphases: [" + strings.Join(phases, ", ") + "]" +
				"\nrequired: " + required +
				"\ntrigger:\n  tool: " + s.Trigger.Tool +
				"\n---\n" + s.Body,
		})
	}
	return out
}

func publish(t *testing.T, n *node, in ...skills.Skill) {
	t.Helper()
	n.engine.SyncSkills(pagesFor(in))
}

func toolSkill(key, tool string, required bool) skills.Skill {
	return skills.Skill{
		Key: key, Title: strings.ToUpper(key[:1]) + key[1:],
		Summary: "how this company uses " + tool,
		Body:    "Always pass the ticket id in the subject when calling " + tool + ".",
		Trigger: skills.Trigger{Tool: tool}, Required: required,
	}
}

// A PUBLISHED PAGE BECOMES A CATALOGUE ENTRY. The point of sourcing skills
// from the knowledge base is that publishing one is a wiki edit — no
// restart, no deploy, no config push.
func TestAPublishedSkillReachesThePlanPrompt(t *testing.T) {
	n := start(t)
	waitForSeat(t, n, "ceo")
	publish(t, n, toolSkill("recall-conventions", "query_episodes", true))

	n.wake(t, "ceo", "How did the week go?")
	waitForTurn(t, n)

	system := planPrompt(t, n)
	for _, want := range []string{
		"## Tool skills", "recall-conventions",
		"how this company uses query_episodes",
		// The REQUIRED marker and its note, so the model learns the
		// contract up front rather than from a blocked call: the guard's
		// error is the recovery path, not the discovery path.
		"(required — load before use)", "load_tool_skill(key)",
	} {
		if !strings.Contains(system, want) {
			t.Fatalf("the plan prompt is missing %q:\n%s", want, tail(system))
		}
	}
	// The BODY is not inlined — the catalogue is a menu, and a company
	// with twenty servers would otherwise spend its prompt on
	// documentation for tools this turn will not touch.
	if strings.Contains(system, "Always pass the ticket id") {
		t.Fatalf("the body was inlined into the catalogue:\n%s", tail(system))
	}
}

// A SKILL FOR A TOOL THIS PHASE CANNOT CALL is noise the model reads past.
func TestASkillForAnAbsentToolIsNotOffered(t *testing.T) {
	n := start(t)
	waitForSeat(t, n, "ceo")
	publish(t, n, toolSkill("jira-conventions", "jira_transition", true))

	n.wake(t, "ceo", "How did the week go?")
	waitForTurn(t, n)

	if system := planPrompt(t, n); strings.Contains(system, "jira-conventions") {
		t.Fatalf("a skill for an absent tool was offered:\n%s", tail(system))
	}
}

// A company that has published none gets no skill scaffolding at all — not
// an empty section.
func TestNoSkillsMeansNoCatalogue(t *testing.T) {
	n := start(t)
	waitForSeat(t, n, "ceo")

	n.wake(t, "ceo", "How did the week go?")
	waitForTurn(t, n)

	if system := planPrompt(t, n); strings.Contains(system, "## Tool skills") {
		t.Fatalf("a company with no skills got a catalogue:\n%s", tail(system))
	}
}

// A WALK REPLACES: a skill whose page was removed stops being offered on the
// next walk rather than lingering until a restart.
func TestAWalkThatNoLongerCarriesAPageDropsItsSkill(t *testing.T) {
	n := start(t)
	waitForSeat(t, n, "ceo")

	publish(t, n, toolSkill("recall-conventions", "query_episodes", true))
	if _, ok := n.engine.Skills().Get("recall-conventions"); !ok {
		t.Fatal("the published skill is not registered")
	}
	publish(t, n)
	if _, ok := n.engine.Skills().Get("recall-conventions"); ok {
		t.Fatal("a removed page's skill survived the walk")
	}
}

// An ordinary page in the same container is not a broken skill: a project
// home page or an operator's notes are admitted-as-not-a-skill rather than
// reported as a decode failure on every walk.
func TestAnOrdinaryPageInTheContainerIsNotASkill(t *testing.T) {
	n := start(t)
	waitForSeat(t, n, "ceo")

	n.engine.SyncSkills(append(
		[]engine.Page{{ID: "home", Title: "Project home",
			Text: "# Welcome\n\nRead the runbooks."}},
		pagesFor([]skills.Skill{
			toolSkill("recall-conventions", "query_episodes", true)})...))

	if got := n.engine.Skills().Len(); got != 1 {
		t.Fatalf("the walk registered %d skills, want just the real one", got)
	}
}

// THE OPERATOR'S VARIABLES are substituted, and they are CONFIG — refreshed
// per epoch, unlike the skills themselves, which come from the knowledge
// base and outlive one.
func TestOperatorVariablesAreSubstitutedIntoASkill(t *testing.T) {
	t.Setenv("CREWLET_TEST_TENANT", "nimbus")
	n := startWith(t, func(doc string) string {
		return strings.Replace(doc, "name: Nimbus",
			"name: Nimbus\nskill_variables:\n  tenant: ${CREWLET_TEST_TENANT}", 1)
	})
	waitForSeat(t, n, "ceo")

	with := toolSkill("recall-conventions", "query_episodes", true)
	with.Summary = "recall on ${tenant}"
	with.Body = "the ${tenant} workspace keeps its runbooks in TS"
	publish(t, n, with)

	body, ok := n.engine.Skills().Body("recall-conventions")
	if !ok {
		t.Fatal("the skill has no body")
	}
	if !strings.Contains(body, "the nimbus workspace") {
		t.Fatalf("the variable was not substituted:\n%s", body)
	}
	offered := n.engine.Skills().SkillsFor(prompts.PhasePlan,
		prompts.Surface{Tools: []string{"query_episodes"}})
	if len(offered) != 1 {
		t.Fatalf("offered %+v", offered)
	}
	if got := n.engine.Skills().Render(offered[0].Summary); got != "recall on nimbus" {
		t.Fatalf("the catalogue summary rendered as %q", got)
	}
}
