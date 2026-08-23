// Package prompts builds the per-phase system prompts a turn runs on.
//
// Each phase gets only what it needs. Org-config context (mission / vision /
// policies / role profile / unit context / team roster) renders directly into
// the Plan-phase prompt from the in-memory org chart — no DB seed step, no
// per-turn knowledge round-trip. Static org config is in the prompt;
// agent-written diary memory and team knowledge-base docs arrive as the
// "## Personal memory" / "## Relevant knowledge" prefetch blocks.
//
//   - Plan — identity + role profile + unit context + org mission/vision +
//     full policies + roster (with team-member profiles for leads) +
//     tool-skills catalogue + tool catalogue + the plan-phase contract.
//   - Execute — one-line identity + plan summary + the execute contract. No
//     policies (Plan already decided the action surface), no roster.
//   - Review — one-line identity + plan summary + the evidence logs + the
//     decision enum. Same trim-down as Execute.
//   - Onboarding — one-line identity + the one-time setup contract.
//   - Sub-agent — parent-provided task prompt + the mandated runtime preamble.
//
// # The English is the specification
//
// Every prose constant here is carried VERBATIM from the Python engine
// (src/crewlet/agent/prompts.py). These strings were tuned against observed
// model behaviour and most of them replaced a production failure: a reworded
// instruction is a behaviour change that no test in this repository can
// catch. Restructure the assembly around them freely; do not edit the words.
//
// # Byte-stability across rounds
//
// A phase's system prompt must be byte-identical on round 1 and round 9 of the
// same phase, because the provider's prompt cache keys on the prefix and a
// single changed byte costs the full uncached rate for every remaining round.
// Two rules keep that true, and both are structural rather than checked at
// runtime:
//
//  1. Every builder is a pure function of its inputs, and the inputs are
//     frozen at turn start. Nothing here reads a clock, a counter, or the
//     process environment on its own.
//  2. Anything that GROWS as a phase loops — the prior-work ledger, the
//     conversation history — rides the user message ([BuildPhaseUserMessage]),
//     never a system prompt.
//
// Assembly is deterministic for the same reason: map iteration order is not,
// so every set that reaches a prompt (the role's MCP servers, the matched
// skills) is sorted before it is rendered.
package prompts

import (
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/org"
)

// Phase names the pass a prompt is built for. The values are the ones an
// operator writes in a tool skill's `phases:` list, so they are wire strings,
// not an internal enum.
type Phase string

// The four passes a prompt is built for. Plan, Execute and Review are the
// turn; Subagent is the short-lived worker one of them spawned.
const (
	PhasePlan     Phase = "plan"
	PhaseExecute  Phase = "execute"
	PhaseReview   Phase = "review"
	PhaseSubagent Phase = "subagent"
)

// Seat is the narrow, prompt-facing view of the agent a prompt is built for:
// a chart and a seat within it.
//
// Deliberately not the engine's full agent definition (LLM keys, budgets,
// tool wiring): a prompt builder that could reach those would eventually
// render one, and the set of things that can change a cached prefix must stay
// as small as it is readable.
//
// Org and Role are both required — a Seat is built from a live chart. A Seat
// missing either renders its identity sections empty rather than panicking:
// a malformed seat must not take the whole turn down, and the phase contract
// on its own is still a usable prompt.
type Seat struct {
	Org  *org.Organization
	Role *org.Role

	// Env resolves the ${VAR} references a human teammate's contact
	// identities may carry. Nil reads the process environment, matching
	// [org.HumanContact.ResolvedIdentities]. It is a field rather than an
	// ambient read so a test can render a roster without touching the
	// environment every other test in the binary shares.
	Env org.EnvLookup
}

// ok reports whether this seat can render identity at all.
func (s Seat) ok() bool { return s.Org != nil && s.Role != nil }

// manager, reports and unit are the three chart walks every section needs.
// Each is nil-safe so a caller can render one section without pre-checking.

func (s Seat) manager() *org.Role {
	if !s.ok() {
		return nil
	}
	return s.Org.Manager(s.Role)
}

func (s Seat) reports() []*org.Role {
	if !s.ok() {
		return nil
	}
	return s.Org.Reports(s.Role)
}

func (s Seat) unit() *org.OrgUnit {
	if !s.ok() {
		return nil
	}
	return s.Org.UnitFor(s.Role)
}

// mcpServers is the role's MCP-server set, sorted.
//
// Sorted because the source is a map: unsorted, the argument handed to a
// skill catalogue would differ between two builds of the same prompt, and a
// catalogue that ordered its answer by the surface it was given would emit a
// different prefix on every round.
func (s Seat) mcpServers() []string {
	if !s.ok() {
		return nil
	}
	names := make([]string, 0, len(s.Role.MCPEnv))
	for name := range s.Role.MCPEnv {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// Skill is one entry in a phase's tool-skill catalogue.
//
// Only the three fields the catalogue renders: the body is deliberately
// absent, because it is never inlined into a prompt — the model loads it on
// demand with load_tool_skill.
type Skill struct {
	// Key is what the model passes to load_tool_skill, and the sort key
	// that fixes catalogue order.
	Key string

	// Summary is the one-line gloss. May carry ${VAR} references and may
	// be authored as a multi-line YAML literal; the catalogue renders and
	// flattens it.
	Summary string

	// Required marks a skill the engine enforces: calls to the tools its
	// trigger covers are rejected until the model has loaded it in the
	// current session.
	Required bool
}

// Surface is the tool surface a phase will actually have. It scopes which
// skills the catalogue offers — a skill for a tool this phase cannot call is
// noise the model has to read past.
type Surface struct {
	Tools      []string
	MCPServers []string
}

// SkillCatalogue is the narrow view of the engine's tool-skill registry that
// prompt assembly needs: which skills fire for a phase's surface, and how a
// ${VAR} reference inside one renders.
//
// Consumer-defined and two methods wide. The registry itself is a live,
// webhook-updated store of skills sourced from the team knowledge base; none
// of that reaches here.
type SkillCatalogue interface {
	// SkillsFor returns the skills whose trigger fires for surface and
	// whose phases include phase. Order is not trusted — see
	// injectSkillCatalogue.
	SkillsFor(phase Phase, surface Surface) []Skill

	// Render substitutes the registry's ${VAR} variables (operator-defined
	// facts such as a tenant URL) in text.
	Render(text string) string
}

const skillCatalogueHeader = "\n## Tool skills" +
	"\nGuidance on how to use specific tools / MCP servers.  Each entry " +
	"below is one line summarising a skill; call `load_tool_skill(key)` " +
	"to fetch the rich body (workflow examples, mention markup, handoff " +
	"conventions) when the summary is not enough."

const skillCatalogueRequiredNote = "\nEntries marked `(required — load before use)` are enforced: the " +
	"engine rejects calls to the tools they cover until you have loaded " +
	"the skill with `load_tool_skill(key)` in the current session.  Load " +
	"them before your first call to those tools."

const requiredMarker = " (required — load before use)"

// injectSkillCatalogue appends a one-line-per-skill catalogue for the active
// surface.
//
// Bodies are NOT inlined: the model sees `key — summary` lines and decides
// whether to load the full body via load_tool_skill, which is always
// available in Plan, Execute and Sub-agent.
//
// Required skills (the default; advisory skills opt out) carry a visible
// marker and an enforcement note after the header, so the model learns the
// contract up front instead of via a blocked tool call — the guard's error
// message is the recovery path, not the discovery path. Review is the
// exception: it has no domain tools and no load_tool_skill, so nothing is
// enforced there and the marker would point at a tool the reviewer does not
// have. Required skills render unmarked in Review.
func injectSkillCatalogue(parts []string, cat SkillCatalogue, phase Phase, surface Surface) []string {
	if cat == nil {
		return parts
	}
	skills := cat.SkillsFor(phase, surface)
	if len(skills) == 0 {
		return parts
	}
	// Sorted here rather than trusted from the catalogue. The registry
	// contract says key-sorted, but the prompt's byte-stability is this
	// package's promise to keep: a catalogue that answered in map order
	// would move the prefix between two builds of one phase and nothing
	// downstream would report it as anything but a cache-miss bill.
	skills = slices.Clone(skills)
	slices.SortFunc(skills, func(a, b Skill) int { return strings.Compare(a.Key, b.Key) })

	// Enforcement needs a phase that HAS the tools and the loader.
	enforceable := phase != PhaseReview
	parts = append(parts, skillCatalogueHeader)
	if enforceable && slices.ContainsFunc(skills, func(s Skill) bool { return s.Required }) {
		parts = append(parts, skillCatalogueRequiredNote)
	}
	for _, skill := range skills {
		// Collapse whitespace in the summary so a multi-line YAML literal
		// (summary: |) still renders as one catalogue bullet — an embedded
		// newline breaks the one-skill-per-line format. Render ${var}
		// references first, so operator-defined facts (a tenant URL, say)
		// appear substituted in the catalogue rather than as the reference.
		summary := strings.Join(strings.Fields(cat.Render(skill.Summary)), " ")
		marker := ""
		if skill.Required && enforceable {
			marker = requiredMarker
		}
		parts = append(parts, "- `"+skill.Key+"`"+marker+" — "+summary)
	}
	return parts
}

// joinSections flattens section-lists into one string, skipping empty
// sections. A section builder returns nil for "this section does not apply",
// which is how an empty policy list leaves no heading behind.
func joinSections(sections ...[]string) string {
	var out []string
	for _, section := range sections {
		out = append(out, section...)
	}
	return strings.Join(out, "\n")
}
