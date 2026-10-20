// Package prompts builds the per-phase system prompts a turn runs on.
//
// Each phase gets only what it needs. Org-config context (mission / vision /
// policies / role profile / unit context / team roster) renders directly into
// the executor's prompt from the in-memory org chart — no DB seed step, no
// per-turn knowledge round-trip. Static org config is in the prompt;
// agent-written diary memory and team knowledge-base docs arrive as the
// "## Personal memory" / "## Relevant knowledge" prefetch blocks.
//
//   - Executor — identity + role profile + unit context + org mission/vision +
//     full policies + roster (team-member profiles for leads, for as many
//     reports as the ONE roster allowance pays for; then names; then a line
//     naming the tool that finds the rest — see rosterAllowanceTokens, the
//     only budget in this package the prompt itself enforces) + the
//     executor's contract + prefetch blocks + tool-skills catalogue + the
//     tools it can still discover.
//   - Review — one-line identity + the rules that decide its decision + the
//     round's evidence. No policies, no roster, no catalogue: the reviewer
//     judges one round's record and needs nothing else.
//   - Onboarding — one-line identity + the one-time setup contract + the
//     tools it can discover.
//   - Sub-agent — parent-provided task prompt + the tools it can discover +
//     the mandated runtime rules.
//
// # The engine states only what the model cannot know
//
// A seat's phase runs inside a vendor's coding CLI, whose own harness and
// whose model already know how to be an agent. So the prompt carries what
// nothing else can: the seat's identity and chart, the company's own words,
// what the engine prefetched for this turn, and the phase's contract — and
// each rule once. A tool's semantics are its description and a field's are
// its schema, both of which reach the model with the tool on every round, so
// a rule about a tool or a field lives there and not here as well. The one
// protocol fact no harness can teach — that the CLI's final text reaches
// nobody, so an action is a tool call and prose performs nothing — is stated
// here, once. Generic agent behaviour, worked examples and restated emphasis are
// not: a second copy of a rule is two texts that drift, and a model reads
// whichever it saw last.
//
// # Prompt text is behaviour
//
// The prose was tuned against observed model behaviour, and nearly every rule
// left in it is a production failure that already happened once — the
// comment beside each says which. A reworded sentence is therefore a
// behaviour change, made deliberately: with the tests that pin what it must
// still say, the docs that describe it, and a reason a reviewer can weigh.
// What no test here can show is how a model reads the new words, so a change
// says in its own record what it expects and why.
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
//
// # Every builder returns its outline beside its text
//
// A builder returns a [Prompt]: the text, and the sections it was assembled
// from — a stable key, a title and a byte length each, tiling the text exactly
// (see [Prompt] for the invariants). The phase record publishes both, so a
// reader shows a prompt as the parts this package appended rather than
// guessing them from the text's `##` lines, which embedded content — a
// trigger's body, a quoted reply, review evidence — brings with it. The
// [Builder] records a boundary where a part is appended and never changes a
// byte of the join, because the text is the cache key and the behaviour.
package prompts

import (
	"maps"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/toolname"
)

// Phase names the pass a prompt is built for. The values are the ones an
// operator writes in a tool skill's `phases:` list, so they are wire strings,
// not an internal enum.
type Phase string

// The three passes a prompt is built for. Execute and Review are the turn's
// own two; Subagent is the short-lived worker one of them spawned.
//
// `execute` keeps its wire string although the pass it names now decides as
// well as acts: it is what operators have written in every tool skill's
// `phases:` list, and renaming it would silently stop matching every one of
// them. `plan` is gone from the vocabulary entirely — see
// [github.com/crewlet/crewlet/internal/agent/skills].ParsePhases, which names
// the set an unknown value is refused against.
const (
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

	// Withheld reports whether a human seat's contact identities are
	// withheld — its holder suspended, retired or removed — by the same
	// reading of the identity directory the party registry was built from
	// (see turnctx.Turn.Withheld). A withheld seat renders no identity: an
	// inbound message from that person already resolves to an outside
	// party, and a roster that still printed their Slack id would have an
	// agent DMing somebody the company has off-boarded. Nil withholds
	// nothing.
	Withheld func(handle string) bool

	// Chatlet says the company runs the engine's own chat
	// (`chat.backend: chatlet`), where every seat — a person's included —
	// answers to its HANDLE: `@jane` in a room reaches the person holding
	// that seat, and `send_dm` opens a conversation with them, with no
	// vendor account behind either. So on Chatlet a human report with
	// no contact identity at all is still somebody to @-mention, and a
	// roster deciding reachability from contact identities alone told a
	// lead they had nobody to address. A bool rather than the company's
	// backend, because reachability by handle is the one thing this
	// package asks of it.
	Chatlet bool
}

// withholds is [Seat.Withheld], nil-safe.
func (s Seat) withholds(handle string) bool {
	return s.Withheld != nil && s.Withheld(handle)
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

func (s Seat) unit() *org.Unit {
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
	names := slices.Sorted(maps.Keys(s.Role.MCPEnv))
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
	// skillCatalogue.
	SkillsFor(phase Phase, surface Surface) []Skill

	// Render substitutes the registry's ${VAR} variables (operator-defined
	// facts such as a tenant URL) in text.
	Render(text string) string
}

const skillCatalogueHeader = "\n## Tool skills" +
	"\nHow this company uses specific tools and MCP servers, one line per " +
	"skill. `" + toolname.LoadToolSkill + "(key)` fetches a skill's full body when the " +
	"summary is not enough."

// skillCatalogueReviewHeader is the catalogue's header in Review, which has no
// load_tool_skill: the loading instruction the other phases get would point
// the reviewer at a tool it does not have, on a surface whose only tool is its
// submission and whose loop asks again until it makes that call.
const skillCatalogueReviewHeader = "\n## Tool skills" +
	"\nHow this company uses specific tools and MCP servers, one line per " +
	"skill: weigh the work you are reviewing against these conventions."

const skillCatalogueRequiredNote = "\nThe engine rejects calls to the tools " +
	"an entry marked `(required — load before use)` covers until you have " +
	"loaded that skill with `" + toolname.LoadToolSkill + "` in this session."

const requiredMarker = " (required — load before use)"

// skillCatalogue is the one-line-per-skill catalogue section for the active
// surface, or nil when no skill fires — the section's lines, heading first, for
// a builder to open as one section of its outline.
//
// Bodies are NOT inlined: the model sees `key — summary` lines and decides
// whether to load the full body via load_tool_skill, which is always
// available to the executor and to a sub-agent.
//
// Required skills (the default; advisory skills opt out) carry a visible
// marker and an enforcement note after the header, so the model learns the
// contract up front instead of via a blocked tool call — the guard's error
// message is the recovery path, not the discovery path. Review is the
// exception: it has no domain tools and no load_tool_skill, so nothing is
// enforced there and the marker would point at a tool the reviewer does not
// have. Required skills render unmarked in Review, under a header that asks
// the reviewer to weigh the work against them rather than to load them.
func skillCatalogue(cat SkillCatalogue, phase Phase, surface Surface) []string {
	if cat == nil {
		return nil
	}
	skills := cat.SkillsFor(phase, surface)
	if len(skills) == 0 {
		return nil
	}
	var parts []string
	// Sorted here rather than trusted from the catalogue. The registry
	// contract says key-sorted, but the prompt's byte-stability is this
	// package's promise to keep: a catalogue that answered in map order
	// would move the prefix between two builds of one phase and nothing
	// downstream would report it as anything but a cache-miss bill.
	skills = slices.Clone(skills)
	slices.SortFunc(skills, func(a, b Skill) int { return strings.Compare(a.Key, b.Key) })

	// Enforcement needs a phase that HAS the tools and the loader.
	enforceable := phase != PhaseReview
	if enforceable {
		parts = append(parts, skillCatalogueHeader)
	} else {
		parts = append(parts, skillCatalogueReviewHeader)
	}
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
