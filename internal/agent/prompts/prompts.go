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
//     executor's contract + prefetch blocks. No tool list: every tool it
//     holds reaches its harness over the bridge. No tool-skills catalogue:
//     its harness is handed its skills as its own, and lists them itself
//     (internal/agent/runner, handedskills.go).
//   - Review — one-line identity + the rules that decide its decision + the
//     round's evidence + the skills scoped to review. No policies, no
//     roster: the reviewer judges one round's record and needs nothing else.
//   - Onboarding — one-line identity + the one-time setup contract + the
//     tools it can discover.
//   - Sub-agent — parent-provided task prompt + the tools it can discover +
//     the mandated runtime rules.
//
// # The engine states only what the model cannot know
//
// A seat's phase runs inside a vendor's harness, whose own harness and
// whose model already know how to be an agent. So the prompt carries what
// nothing else can: the seat's identity and chart, the company's own words,
// what the engine prefetched for this turn, and the phase's contract — and
// each rule once. A tool's semantics are its description and a field's are
// its schema, both of which reach the model with the tool on every round, so
// a rule about a tool or a field lives there and not here as well. The
// executor is the harness's own agentic run in a box, its seat's tools reaching
// it over the tool bridge; every other phase is the same harness's own loop on
// this host, its tools reaching it as one MCP server. The one protocol fact
// no harness can teach — that the harness's final text reaches nobody, so an
// action is a tool call and prose performs nothing — is stated here, once.
// Generic agent behaviour, worked examples and restated emphasis are
// not: a second copy of a rule is two texts that drift, and a model reads
// whichever it saw last. No tool the phase already holds is listed again:
// "## Tools to discover" names only what a phase that discovers can still
// reach, and the executor lists none.
//
// Two prompts, not three: the frame that decides is the frame that acts, so
// the identity scaffold is sent once and not twice with one thrown away
// between.
//
// A section that sends a seat to a tool renders only where the seat holds
// EVERY tool its claims rest on (the executor's code sections, gated on
// [CodeHistoryTools], [CodeSearchTools], [CodeWriteTools] and
// [CodeMergeTools], which internal/agent/builtin's tests hold to what it
// registers). Code history is read on the server and not from a clone, a
// Gitlet repository being cloned blobless, so it is for building and not for
// answering questions. A server-keyed tool skill fires on the servers the
// registry holds a tool of, never on the keys of mcp_env, which name
// credentials.
//
// [KnowledgeAnswerSystem] ends on [provenance.StandingInstruction] inside the
// constant, so its prefix stays byte-stable, and every source is fenced under
// the label its backend gave it (ADR-0040).
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
//  2. Anything that GROWS as a phase loops — the prior-work ledger — rides the
//     user message ([BuildPhaseUserMessage]), never a system prompt; and so
//     does how the conversation's earlier turns ended, which is the turn's
//     and not the seat's.
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
//
// A kept session its job went on past is told [SavedSession] in place of
// [InterruptedSession]: it was saved while its run still worked, when, and that
// the run went on after it, so it ends where it was saved and not where the run
// stopped, and what the run did beyond the calls told after it is in no record.
package prompts

import (
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
// `execute` keeps its wire string although the pass also decides: operators
// wrote it in every tool skill's `phases:` list, and a rename would silently
// stop matching them. The set an unknown value is refused against is
// [github.com/crewlet/crewlet/internal/agent/skills].ParsePhases.
const (
	PhaseExecute  Phase = "execute"
	PhaseReview   Phase = "review"
	PhaseSubagent Phase = "subagent"
)

// Seat is the narrow, prompt-facing view of the agent a prompt is built for:
// a chart and a seat within it.
//
// Not the full agent definition (LLM keys, budgets, tool wiring): a builder
// that could reach those would eventually render one, and what can change a
// cached prefix must stay small. Org and Role are both required; a Seat
// missing either renders its identity sections empty rather than panicking,
// since the phase contract alone is still a usable prompt.
type Seat struct {
	Org  *org.Organization
	Role *org.Role

	// Env resolves the ${VAR} references a human teammate's contact
	// identities may carry. Nil reads the process environment, matching
	// [org.HumanContact.ResolvedIdentities]; a field so a test need not touch
	// the environment the binary's other tests share.
	Env org.EnvLookup

	// Withheld reports whether a human seat's contact identities are
	// withheld (holder suspended, retired or removed), by the directory read
	// the party registry was built from (turnctx.Turn.Withheld). A withheld
	// seat renders no identity, or an agent would DM somebody the company
	// off-boarded. Nil withholds nothing.
	Withheld func(handle string) bool

	// Chatlet says the company runs the engine's own chat
	// (`chat.backend: chatlet`), where every seat, a person's included,
	// answers to its HANDLE (`@jane`, `send_dm`) with no vendor account
	// behind it. So a human report with no contact identity is still
	// reachable there. A bool, since reachability by handle is all this
	// package asks of the backend.
	Chatlet bool
}

// withholds is [Seat.Withheld], nil-safe.
func (s Seat) withholds(handle string) bool {
	return s.Withheld != nil && s.Withheld(handle)
}

// ok reports whether this seat can render identity at all.
func (s Seat) ok() bool { return s.Org != nil && s.Role != nil }

// manager, reports and unit are the chart walks the sections need, each
// nil-safe.

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

// Skill is one entry in a phase's tool-skill catalogue. It has no body: a
// body is never inlined, the model loads it with load_tool_skill.
type Skill struct {
	// Key is what the model passes to load_tool_skill, and the sort key
	// that fixes catalogue order.
	Key string

	// Summary is the one-line gloss. It may carry ${VAR} references and be
	// a multi-line YAML literal; the catalogue renders and flattens it.
	Summary string

	// Required marks a skill the engine enforces: calls to the tools its
	// trigger covers are rejected until the model has loaded it in the
	// current session.
	Required bool
}

// Surface is the tool surface a phase will actually have. It scopes which
// skills the catalogue offers: a skill for a tool the phase cannot call is
// noise.
//
// MCPServers is the servers the seat's tool registry holds a tool of
// ([github.com/crewlet/crewlet/internal/tools.Snapshot.MCPServers]), the set
// the skill guard arms its server triggers against, so the prompt offers
// exactly the skills the guard enforces. NEVER the keys of the seat's
// mcp_env: those name credentials, not reachable servers (a shared server
// needs no block; a vendor's block or a disabled server's has no server).
type Surface struct {
	Tools      []string
	MCPServers []string
}

// SkillCatalogue is the narrow view of the engine's tool-skill registry that
// prompt assembly needs: which skills fire for a phase's surface, and how a
// ${VAR} reference inside one renders.
type SkillCatalogue interface {
	// SkillsFor returns the skills whose trigger fires for surface and
	// whose phases include phase, in an order skillCatalogue does not trust.
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
// load_tool_skill: the other phases' loading instruction would point the
// reviewer at a tool it lacks, on a surface whose only tool is its submission.
const skillCatalogueReviewHeader = "\n## Tool skills" +
	"\nHow this company uses specific tools and MCP servers, one line per " +
	"skill: weigh the work you are reviewing against these conventions."

const skillCatalogueRequiredNote = "\nThe engine rejects calls to the tools " +
	"an entry marked `(required — load before use)` covers until you have " +
	"loaded that skill with `" + toolname.LoadToolSkill + "` in this session."

const requiredMarker = " (required — load before use)"

// skillCatalogue is the one-line-per-skill catalogue section for the active
// surface, heading first, or nil when no skill fires. Bodies are never
// inlined; the model loads one with load_tool_skill.
//
// Required skills (the default) carry a marker and an enforcement note, so the
// model learns the contract up front rather than from a blocked call: the
// guard's error is the recovery path, not the discovery path. Review enforces
// nothing and has no loader, so there they render unmarked, under a header
// asking the reviewer to weigh the work against them.
func skillCatalogue(cat SkillCatalogue, phase Phase, surface Surface) []string {
	if cat == nil {
		return nil
	}
	skills := cat.SkillsFor(phase, surface)
	if len(skills) == 0 {
		return nil
	}
	var parts []string
	// Sorted here, not trusted from the catalogue: byte-stability is this
	// package's promise (package doc, "Byte-stability across rounds").
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
		// Rendered, then whitespace-collapsed, so a multi-line YAML literal
		// stays one bullet and a ${var} appears substituted.
		summary := strings.Join(strings.Fields(cat.Render(skill.Summary)), " ")
		marker := ""
		if skill.Required && enforceable {
			marker = requiredMarker
		}
		parts = append(parts, "- `"+skill.Key+"`"+marker+" — "+summary)
	}
	return parts
}
