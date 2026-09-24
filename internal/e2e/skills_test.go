package e2e

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/prompts"
	"github.com/crewlet/crewlet/internal/agent/skills"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/pages"
)

// The tool-skill path, end to end: a page an operator published reaches the
// prompt of a phase that can call the tools it is about.
//
// The claim is not that the registry works (its own suite covers that) but
// that a real node builds one, fills it from its company's own knowledge base
// through the node's skill sync, and offers it to the phases.
//
// # Published as pages, never installed
//
// Every skill here is WRITTEN to the skills container and read back by the
// node's own walk, the path an operator's page takes. Installing pages into
// the registry directly would skip that path and race it besides: the node's
// boot walk replaces the registry wholesale, so a walk that landed after an
// install would silently empty what the test had just put there.

// skillPageText renders a skill back into page text, so the node's admission
// decides what it is rather than the test.
func skillPageText(s skills.Skill) string {
	required := "true"
	if !s.Required {
		required = "false"
	}
	return "---\nkey: " + s.Key + "\ntitle: " + s.Title +
		"\nsummary: " + s.Summary +
		"\nphases: [execute, review]" +
		"\nrequired: " + required +
		"\ntrigger:\n  tool: " + s.Trigger.Tool +
		"\n---\n" + s.Body
}

// writeSkillsPage writes one page into the company's skills container.
func writeSkillsPage(t *testing.T, n *node, title, body string) pages.Written {
	t.Helper()
	written, err := n.engine.PagesStore().Create(t.Context(), pageOperator(), pages.NewPage{
		Container: config.DefaultSkillsContainer, Title: title, Body: body,
	})
	if err != nil {
		t.Fatalf("write %q to the skills container: %v", title, err)
	}
	return written
}

// publish writes each skill as a page in the skills container and waits for
// every one of them to reach the node's registry.
func publish(t *testing.T, n *node, in ...skills.Skill) []pages.Written {
	t.Helper()
	waitFor(t, "the native backends to hydrate", n.engine.NativeHydrated)
	out := make([]pages.Written, 0, len(in))
	for _, s := range in {
		out = append(out, writeSkillsPage(t, n, s.Title, skillPageText(s)))
	}
	for _, s := range in {
		waitFor(t, "skill "+s.Key+" to reach the registry", func() bool {
			_, ok := n.engine.Skills().Get(s.Key)
			return ok
		})
	}
	return out
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
func TestAPublishedSkillReachesTheExecutorsPrompt(t *testing.T) {
	n := start(t)
	waitForSeat(t, n, "ceo")
	publish(t, n, toolSkill("recall-conventions", "query_episodes", true))

	n.wake(t, "ceo", "How did the week go?")
	waitForTurn(t, n)

	system := executorPrompt(t, n)
	for _, want := range []string{
		"## Tool skills", "recall-conventions",
		"how this company uses query_episodes",
		// The REQUIRED marker and its note, so the model learns the
		// contract up front rather than from a blocked call: the guard's
		// error is the recovery path, not the discovery path.
		"(required — load before use)", "load_tool_skill(key)",
	} {
		if !strings.Contains(system, want) {
			t.Fatalf("the executor prompt is missing %q:\n%s", want, tail(system))
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

	if system := executorPrompt(t, n); strings.Contains(system, "jira-conventions") {
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

	if system := executorPrompt(t, n); strings.Contains(system, "## Tool skills") {
		t.Fatalf("a company with no skills got a catalogue:\n%s", tail(system))
	}
}

// A TRASHED PAGE TAKES ITS SKILL WITH IT: the skill stops being offered as
// soon as the node reads the container again, rather than lingering until a
// restart.
func TestATrashedSkillPageLeavesTheRegistry(t *testing.T) {
	n := start(t)
	waitForSeat(t, n, "ceo")

	written := publish(t, n, toolSkill("recall-conventions", "query_episodes", true))
	if _, err := n.engine.PagesStore().Trash(t.Context(), pageOperator(),
		written[0].Page.ID); err != nil {
		t.Fatalf("trash the skill page: %v", err)
	}
	waitFor(t, "the trashed page's skill to leave the registry", func() bool {
		_, ok := n.engine.Skills().Get("recall-conventions")
		return !ok
	})
}

// An ordinary page in the same container is not a broken skill: a project
// home page or an operator's notes sit beside the skills without becoming one.
func TestAnOrdinaryPageInTheContainerIsNotASkill(t *testing.T) {
	n := start(t)
	waitForSeat(t, n, "ceo")

	waitFor(t, "the native backends to hydrate", n.engine.NativeHydrated)
	writeSkillsPage(t, n, "Project home", "# Welcome\n\nRead the runbooks.")
	publish(t, n, toolSkill("recall-conventions", "query_episodes", true))

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

	loaded, ok := n.engine.Skills().Load("recall-conventions")
	if !ok {
		t.Fatal("the skill has no body")
	}
	if !strings.Contains(loaded.Body, "the nimbus workspace") {
		t.Fatalf("the variable was not substituted:\n%s", loaded.Body)
	}
	offered := n.engine.Skills().SkillsFor(prompts.PhaseExecute,
		prompts.Surface{Tools: []string{"query_episodes"}})
	if len(offered) != 1 {
		t.Fatalf("offered %+v", offered)
	}
	if got := n.engine.Skills().Render(offered[0].Summary); got != "recall on nimbus" {
		t.Fatalf("the catalogue summary rendered as %q", got)
	}
}

// A SKILL PAGE PUBLISHED TO THE COMPANY'S OWN KNOWLEDGE BASE REACHES THE
// REGISTRY, and one edited into an ordinary page leaves it.
//
// The native backend has no page webhook, so what notices is the page
// projection's own apply: it derives the skill flag on every page it writes,
// which is the one thing that sees both a page becoming a skill and a page
// ceasing to be one. It asks the node's skill sync to walk the container
// afterwards, and this is the whole path, from a wiki write to what an
// executor would be offered.
func TestASkillPagePublishedNativelyReachesTheRegistry(t *testing.T) {
	n := start(t)
	waitFor(t, "the native backends to hydrate", n.engine.NativeHydrated)

	const frontmatter = "---\nkey: deploy-conventions\ntitle: Deploying\n" +
		"summary: how this company deploys\nphases: [execute]\n" +
		"trigger:\n  tool: run_sandbox\n---\n"
	written, err := n.engine.PagesStore().Create(t.Context(), pageOperator(), pages.NewPage{
		Container: "TS", Title: "Deploying",
		Body: frontmatter + "Tag the release before you announce it.",
	})
	if err != nil {
		t.Fatalf("publish the skill page: %v", err)
	}
	waitFor(t, "the published skill to reach the registry", func() bool {
		_, ok := n.engine.Skills().Get("deploy-conventions")
		return ok
	})

	body := "This used to be a skill and is now a note."
	if _, err := n.engine.PagesStore().SavePage(t.Context(), pageOperator(),
		written.Page.ID, pages.Save{BaseVersion: written.Page.Version, Body: &body}); err != nil {
		t.Fatalf("edit the page into an ordinary one: %v", err)
	}
	waitFor(t, "the skill to leave the registry", func() bool {
		_, ok := n.engine.Skills().Get("deploy-conventions")
		return !ok
	})
}
