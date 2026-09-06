package prompts

import (
	"strings"
	"testing"
)

// allSkills is the reference registry: one tool-keyed skill, one
// server-keyed, and one that fires on either of two servers. Inserted in an
// order that is NOT the key order, so the catalogue's own sort is what has to
// produce the rendered order.
func allSkills() *fakeCatalogue {
	return &fakeCatalogue{skills: []fakeSkill{
		{
			key: "skill:platform_mentions", mcpServer: "atlassian",
			summary: "Per-platform mention markup for the tracker and chat.",
			body:    "MENTIONS-BODY",
			phases:  []Phase{PhaseExecute, PhaseReview, PhaseSubagent},
		},
		{
			key: "mcp:github", mcpServer: "github",
			summary: "GitHub MCP tools incl. Copilot delegation.",
			body:    "GITHUB-BODY",
			phases:  []Phase{PhaseExecute, PhaseSubagent},
		},
		{
			key: "tool:refresh_memory", tool: "refresh_memory",
			summary: "Re-filter personal memory after recon changed the picture.",
			body:    "MEMORY-BODY",
			phases:  []Phase{PhaseExecute},
		},
	}}
}

// The catalogue is one line per skill: key + summary. Bodies are never
// inlined — the model loads them on demand with load_tool_skill, which is
// what keeps a phase prompt from carrying every skill's full text.
func TestNoPhaseInlinesSkillBodies(t *testing.T) {
	t.Parallel()
	cat := allSkills()
	s := engineer()
	for name, p := range map[string]string{
		"executor": BuildExecutor(s, ExecutorInput{
			AvailableTools: []string{"refresh_memory"}, Skills: cat,
		}),
		"review": BuildReview(s, ReviewInput{Skills: cat}),
		"subagent": BuildSubagent(s, SubagentInput{
			ParentSystemPrompt: "P", Skills: cat,
		}),
	} {
		t.Run(name, func(t *testing.T) {
			excludes(t, p, "MENTIONS-BODY", "GITHUB-BODY", "MEMORY-BODY")
			// Each header points the model at the explicit-load mechanism
			// and must never promise automatic injection — a model that
			// believes the body is coming skips the call and waits for
			// guidance that never arrives.
			if strings.Contains(p, "## Tool skills") {
				contains(t, p, "load_tool_skill")
			}
		})
	}
}

func TestPlanCatalogueFiresOnToolsAndServers(t *testing.T) {
	t.Parallel()
	cat := allSkills()

	// A tool-keyed skill needs its tool in the surface.
	without := BuildExecutor(engineer(), ExecutorInput{Skills: cat})
	excludes(t, without, "tool:refresh_memory")

	with := BuildExecutor(engineer(), ExecutorInput{
		AvailableTools: []string{"refresh_memory"}, Skills: cat,
	})
	contains(t, with, "## Tool skills", "tool:refresh_memory",
		"Re-filter personal memory after recon")

	// A server-keyed skill fires on the role's mcp_env, independent of
	// tools: the Engineer carries atlassian and github.
	contains(t, without, "mcp:github", "skill:platform_mentions")

	// The Engineering Lead carries neither server.
	excludes(t, BuildExecutor(lead(), ExecutorInput{Skills: cat}),
		"mcp:github", "skill:platform_mentions")
}

// Deterministic key-sorted order is what keeps the prompt prefix byte-stable
// across turns. Sorted HERE rather than trusted from the registry: a
// catalogue answering in map order would move the prefix and nothing
// downstream would report it as anything but a larger bill.
func TestCatalogueOrdersEntriesByKey(t *testing.T) {
	t.Parallel()
	p := BuildExecutor(engineer(), ExecutorInput{
		AvailableTools: []string{"refresh_memory"}, Skills: allSkills(),
	})
	order(t, p, "- `mcp:github`", "- `skill:platform_mentions`", "- `tool:refresh_memory`")
}

// Operators routinely author summaries as YAML "|" literal blocks; an
// embedded newline would break the one-skill-per-line catalogue format.
func TestCatalogueCollapsesAMultilineSummary(t *testing.T) {
	t.Parallel()
	cat := &fakeCatalogue{skills: []fakeSkill{{
		key: "skill:multiline", tool: "anything",
		summary: "First sentence here.\nSecond sentence here.\n",
	}}}
	p := BuildExecutor(engineer(), ExecutorInput{
		AvailableTools: []string{"anything"}, Skills: cat,
	})
	contains(t, p, "- `skill:multiline` — First sentence here. Second sentence here.")
	excludes(t, p, "\nSecond sentence here.")
}

// ${var} references render with the registry's variables, so an
// operator-defined fact (a tenant URL) appears substituted in the catalogue
// rather than as the reference.
func TestCatalogueSubstitutesSkillVariables(t *testing.T) {
	t.Parallel()
	cat := &fakeCatalogue{
		skills: []fakeSkill{{key: "tool:x", tool: "x", summary: "base is ${wiki_base_url}"}},
		vars:   map[string]string{"wiki_base_url": "https://acme.example.com/wiki"},
	}
	p := BuildExecutor(engineer(), ExecutorInput{AvailableTools: []string{"x"}, Skills: cat})
	contains(t, p, "base is https://acme.example.com/wiki")
	excludes(t, p, "${wiki_base_url}")
}

// The skills catalogue and the tool catalogue are conceptually one section:
// "how to use these" directly above "here are the names".
func TestSkillCatalogueLandsImmediatelyBeforeTheToolCatalogue(t *testing.T) {
	t.Parallel()
	cat := &fakeCatalogue{skills: []fakeSkill{{key: "tool:x", tool: "x", summary: "Tight summary of X."}}}
	p := BuildExecutor(engineer(), ExecutorInput{
		ToolCatalogue: "- x: does x", AvailableTools: []string{"x"}, Skills: cat,
	})
	order(t, p, "## Tool skills", "## Available tools")
	between := p[strings.Index(p, "## Tool skills"):strings.Index(p, "## Available tools")]
	if n := strings.Count(between, "##"); n != 1 {
		t.Errorf("%d headings sit between the two catalogues, want only the Tool skills one", n)
	}
}

// A required skill carries a visible marker and an enforcement note, so the
// model learns the contract up front. The guard's error message is the
// recovery path, not the discovery path.
func TestRequiredSkillsAreMarkedInEnforceablePhases(t *testing.T) {
	t.Parallel()
	cat := &fakeCatalogue{skills: []fakeSkill{
		{
			key: "mcp:github", mcpServer: "github", required: true,
			summary: "GITHUB-SUMMARY",
			phases:  []Phase{PhaseExecute, PhaseReview, PhaseSubagent},
		},
		{
			key: "skill:platform_mentions", mcpServer: "atlassian",
			summary: "MENTIONS-SUMMARY",
			phases:  []Phase{PhaseExecute, PhaseReview, PhaseSubagent},
		},
	}}
	s := engineer()
	for name, p := range map[string]string{
		"executor": BuildExecutor(s, ExecutorInput{Skills: cat}),
		"subagent": BuildSubagent(s, SubagentInput{ParentSystemPrompt: "P", Skills: cat}),
	} {
		t.Run(name, func(t *testing.T) {
			contains(t, p, "- `mcp:github` (required — load before use) — GITHUB-SUMMARY")
			contains(t, p, "engine rejects calls")
			// Only the required entry is marked.
			contains(t, p, "- `skill:platform_mentions` — MENTIONS-SUMMARY")
		})
	}

	// Review is the exception: it has no domain tools and no
	// load_tool_skill, so nothing is enforced there and the marker would
	// point at a tool the reviewer does not have.
	rv := BuildReview(s, ReviewInput{Skills: cat})
	contains(t, rv, "- `mcp:github` — GITHUB-SUMMARY")
	excludes(t, rv, "(required — load before use)", "engine rejects calls")

	// No enforcement note when nothing catalogued is required.
	advisory := BuildExecutor(s, ExecutorInput{Skills: &fakeCatalogue{skills: []fakeSkill{
		{key: "mcp:github", mcpServer: "github", summary: "GITHUB-SUMMARY"},
	}}})
	contains(t, advisory, "## Tool skills")
	excludes(t, advisory, "(required — load before use)", "engine rejects calls")
}

// The engine ships no skill prose of its own. A nil registry, or one that
// matches nothing, leaves the prompt with zero skill scaffolding — not an
// empty heading.
func TestNoRegistryMeansNoSkillScaffolding(t *testing.T) {
	t.Parallel()
	s := engineer()
	empty := &fakeCatalogue{}
	for name, p := range map[string]string{
		"executor/nil": BuildExecutor(s, ExecutorInput{AvailableTools: []string{
			"reflect_and_persist", "refine_skill", "query_knowledge", "refresh_memory",
		}}),
		"executor/empty": BuildExecutor(s, ExecutorInput{Skills: empty}),
		"review/empty":   BuildReview(s, ReviewInput{Skills: empty}),
		"subagent/empty": BuildSubagent(s, SubagentInput{ParentSystemPrompt: "P", Skills: empty}),
	} {
		t.Run(name, func(t *testing.T) {
			excludes(t, p, "## Tool skills",
				"## Persisting durable facts", "## Skill refinement",
				"## Mentioning teammates", "## GitHub tools")
		})
	}
}

// Execute's surface is the plan's tools_needed plus the always-on builtins,
// so a skill for a tool this executor will never call must not appear.
func TestExecuteCatalogueIsScopedToThePlannedSurface(t *testing.T) {
	t.Parallel()
	cat := allSkills()
	p := BuildExecutor(engineer(), ExecutorInput{Skills: cat})
	excludes(t, p, "tool:refresh_memory")
	// Server-keyed skills still fire: they key on the role, not the plan.
	contains(t, p, "mcp:github")
}
