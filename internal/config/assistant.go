package config

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/providers/llm/cliagent/cliprofile"
)

// Assistant is the Tier B `assistant:` block: whether the Dashboard Assistant
// runs for this company, which models it runs on, and the bounds of one turn.
//
//	assistant:
//	  enabled: true                 # absent means off
//	  llm: [assistant]              # providers.llm keys, a fallback chain
//	  max_steps: 30                 # 1..40
//	  turn_timeout_seconds: 900     # 30..1200
//	  proposal_ttl_seconds: 900     # 60..3600
//	  retention_days: 30            # 1..365
//	  token_budget: { day: 200000 } # per person, optional
//	  deep_work:                    # optional; absent means quick mode only
//	    llm: assistant-deep         # a Claude Code providers.llm key
//	    timeout_seconds: 1200       # 60..1200
//	    max_steps: 150              # 10..400
//	    max_tokens: 3000000         # 10000..20000000
//
// # Off unless a person turns it on
//
// Enabled is a pointer because its zero value must be meaningful, and the
// meaningful zero here is OFF: a company that never mentions the assistant
// does not run one, and a company that wrote `enabled: false` reads the same.
// A model reading the company's data for a person is something somebody
// chooses, never something an upgrade switches on.
//
// # Every numeric zero is its default
//
// Zero lies outside every range below, so a zero can only mean "not set" and
// reads as the default without a pointer. A value outside its range is refused
// at admission and, on a revision stored before the rule (or written by a peer
// that relaxed it), clamped to the nearest bound by its accessor, so a running
// node never acts on a turn bound nobody could have meant.
//
// # Only a model that can reach nothing else
//
// Every member of `llm` must be an entry whose profile denies the CLI's own
// local tools AND its own web tools, so the one tool source its run on the
// engine host has is the engine's `crewlet` server, serving the person's own
// requests (ADR-0049). The assistant's guarantee (ADR-0050) is that it reads
// only what the person's own requests return; a shell reads whatever the
// engine user can, and a web fetch is both text no authority decision ever
// saw and a way out for whatever the person's data put in the prompt.
// [Company.AssistantChain] is the one place the chain is handed out, and it
// refuses a chain that breaks this, so a stored revision carrying one leaves
// the assistant unavailable rather than unsafe.
type Assistant struct {
	Enabled *bool `yaml:"enabled,omitempty" json:"enabled,omitempty" desc:"Run the Dashboard Assistant for this company. Absent or false: off."`

	// LLM is the assistant's provider chain, the first key tried first, in
	// the shape a seat's llm_auxiliary takes.
	LLM ProviderKeys `yaml:"llm,omitempty" json:"llm,omitzero" desc:"providers.llm keys the assistant runs on, a fallback chain. Required when enabled. Each must be an entry whose CLI's local tools are denied and, through overrides.web_tools, its web tools."`

	MaxSteps           int `yaml:"max_steps,omitempty" json:"max_steps,omitempty" js:"min=0;max=40" desc:"Most tool steps one assistant turn may take, 1..40. Absent: 30."`
	TurnTimeoutSeconds int `yaml:"turn_timeout_seconds,omitempty" json:"turn_timeout_seconds,omitempty" js:"min=0;max=1200" desc:"Longest one assistant turn may run, 30..1200 seconds. Absent: 900."`
	ProposalTTLSeconds int `yaml:"proposal_ttl_seconds,omitempty" json:"proposal_ttl_seconds,omitempty" js:"min=0;max=3600" desc:"How long a proposed change waits for approval, 60..3600 seconds. Absent: 900."`
	RetentionDays      int `yaml:"retention_days,omitempty" json:"retention_days,omitempty" js:"min=0;max=365" desc:"How long a person's assistant conversations are kept, 1..365 days. Absent: 30."`

	// TokenBudget is each PERSON's ceiling per calendar window, in the shape
	// a seat's budget takes, charged on top of the company's own.
	TokenBudget TokenBudget `yaml:"token_budget,omitempty" json:"token_budget,omitzero" desc:"Each person's assistant token ceilings per calendar window on the company clock: day, week and month, each optional. Absent = no per-person ceiling; the company's own budget still applies. A ceiling at or above the company's on a window that holds it is warned of, since the company's is reached first."`

	// DeepWork is the assistant's escalation into a coding CLI's own
	// harness for a task that needs research, scripts, files or many steps
	// (ADR-0054). Absent: the assistant answers in quick mode only.
	DeepWork *AssistantDeepWork `yaml:"deep_work,omitempty" json:"deep_work,omitempty" desc:"Deep work: the assistant hands a large task to a coding CLI running its own tools in a sandbox, reaching Crewlet only through the person's own assistant tools. Absent: quick mode only."`
}

// AssistantDeepWork is the `assistant.deep_work` block.
//
// # A coding CLI with its own tools, never on the engine unconfined
//
// The entry it names runs as its CLI's own run in a box, as a seat's
// executor does: the CLI's shell, files, web search and image reading are the
// point. Its one way into Crewlet is the person's
// own assistant tools, bridged, so every read is still the person's request
// and every write still a proposal they approve. Where it runs is the entry's
// own `run_in`, else the first of e2b and container this company configures,
// else the engine host under OS confinement, which a host that cannot enforce
// one refuses rather than run open (internal/sandbox's confinement).
//
// # Every numeric zero is its default
//
// As in the block around it: zero lies outside every range, so it reads as
// the default without a pointer, and a stored value out of range is clamped
// by its accessor.
type AssistantDeepWork struct {
	LLM            string `yaml:"llm,omitempty" json:"llm,omitempty" desc:"providers.llm key deep work runs on: an entry driving claude-code. Required."`
	TimeoutSeconds int    `yaml:"timeout_seconds,omitempty" json:"timeout_seconds,omitempty" js:"min=0;max=1200" desc:"Longest one deep-work run may take, 60..1200 seconds. Absent: 1200."`
	MaxSteps       int    `yaml:"max_steps,omitempty" json:"max_steps,omitempty" js:"min=0;max=400" desc:"Most agentic rounds and Crewlet tool calls one deep-work run may make, 10..400. Absent: 150."`
	MaxTokens      int    `yaml:"max_tokens,omitempty" json:"max_tokens,omitempty" js:"min=0;max=20000000" desc:"Most tokens one deep-work run may spend before it is stopped, 10000..20000000. Absent: 3000000. The person's and the company's budgets still apply."`
}

// assistantBound is one numeric field's range and the default its zero reads as.
type assistantBound struct {
	key      string
	min, max int
	def      int
	unit     string
}

// The four bounds, each anchored where it is consumed:
//
//   - max_steps: a step is one tool call and one model round. Thirty lets a
//     turn look a route up, read the resource and the records around it,
//     correct a refused call and propose the change, which is what a question
//     about the person's whole API takes (a seat added to the org reads the
//     route's documentation, the unit and its seats before it proposes). Forty
//     is the ceiling because every step's result stays in the conversation the
//     model re-reads: forty of internal/assistant's sixteen kibibyte results
//     are about 160,000 tokens, which with the prompt still fits a 200,000
//     token context, where sixty would not. The budget meters, the person's
//     and the company's, still stop a turn at its spend whatever its count.
//   - turn_timeout_seconds: every round is a CLI launch of fifteen to
//     twenty-five seconds, so thirty rounds need up to twelve and a half
//     minutes: fifteen is the default, and twenty minutes carries the forty
//     step ceiling at the same latency. Thirty seconds is one slow round.
//   - proposal_ttl_seconds: long enough to read a diff and step up, short
//     enough that what was previewed is still what the write meets.
//   - retention_days: a month of history by default, a year at most, because
//     a transcript holds whatever the person's reads returned.
//
// And deep work's three, anchored the same way:
//
//   - timeout_seconds: a run lives inside one turn segment, whose read grant
//     lives at most [AssistantTurnTimeoutCeiling], so twenty minutes is both
//     the default and the ceiling: a run past it would call out on a grant
//     that has ended. A run that needs longer parks on a proposal and is
//     continued on the fresh grant its approval derives. One minute is a run
//     that installs the CLI and reads one page.
//   - max_steps: a redesign that reads a brand, writes assets, renders and
//     checks them takes a hundred-odd agentic rounds; four hundred is the
//     most a twenty-minute run can make at the CLI's three-second round.
//   - max_tokens: a hundred and fifty rounds re-reading a growing context
//     are about three million tokens, most of them cache reads; twenty
//     million bounds a run nobody meant.
var (
	deepTimeout = assistantBound{key: "timeout_seconds", min: 60, max: AssistantTurnTimeoutCeiling, def: AssistantTurnTimeoutCeiling, unit: "seconds"}
	deepSteps   = assistantBound{key: "max_steps", min: 10, max: 400, def: 150, unit: "steps"}
	deepTokens  = assistantBound{key: "max_tokens", min: 10_000, max: 20_000_000, def: 3_000_000, unit: "tokens"}
)

var (
	assistantSteps     = assistantBound{key: "max_steps", min: 1, max: 40, def: 30, unit: "steps"}
	assistantTimeout   = assistantBound{key: "turn_timeout_seconds", min: 30, max: AssistantTurnTimeoutCeiling, def: 900, unit: "seconds"}
	assistantProposal  = assistantBound{key: "proposal_ttl_seconds", min: 60, max: 3600, def: 900, unit: "seconds"}
	assistantRetention = assistantBound{key: "retention_days", min: 1, max: 365, def: 30, unit: "days"}
)

// AssistantTurnTimeoutCeiling is the longest any assistant turn may run, in
// seconds: the top of turn_timeout_seconds' range, and so the longest a turn's
// read grant has to live (internal/api/auth's AssistReadLifetime).
const AssistantTurnTimeoutCeiling = 1200

// value is v with its default applied and clamped into range.
func (b assistantBound) value(v int) int {
	switch {
	case v == 0:
		return b.def
	case v < b.min:
		return b.min
	case v > b.max:
		return b.max
	}
	return v
}

// check reports v when it is set and out of range.
func (b assistantBound) check(path Path, v int, p *problems) {
	if v == 0 || (v >= b.min && v <= b.max) {
		return
	}
	p.add(at(path, b.key), ErrOutOfRange,
		"must be %d..%d %s, or absent for the default of %d, got %d",
		b.min, b.max, b.unit, b.def, v)
}

// On reports whether the company turned the assistant on.
func (a Assistant) On() bool { return a.Enabled != nil && *a.Enabled }

// Steps is the most tool steps one turn may take.
func (a Assistant) Steps() int { return assistantSteps.value(a.MaxSteps) }

// TurnTimeout is the longest one turn may run.
func (a Assistant) TurnTimeout() time.Duration {
	return time.Duration(assistantTimeout.value(a.TurnTimeoutSeconds)) * time.Second
}

// ProposalTTL is how long a proposed change waits for its owner's approval.
func (a Assistant) ProposalTTL() time.Duration {
	return time.Duration(assistantProposal.value(a.ProposalTTLSeconds)) * time.Second
}

// Retention is how long a person's conversations are kept.
func (a Assistant) Retention() time.Duration {
	return time.Duration(assistantRetention.value(a.RetentionDays)) * 24 * time.Hour
}

// IsZero lets an unset block drop out of a JSON round trip.
func (a Assistant) IsZero() bool {
	return a.Enabled == nil && len(a.LLM) == 0 && a.MaxSteps == 0 && a.TurnTimeoutSeconds == 0 &&
		a.ProposalTTLSeconds == 0 && a.RetentionDays == 0 && a.TokenBudget == (TokenBudget{}) &&
		a.DeepWork == nil
}

// Timeout is the longest one deep-work run may take.
func (d AssistantDeepWork) Timeout() time.Duration {
	return time.Duration(deepTimeout.value(d.TimeoutSeconds)) * time.Second
}

// Steps is the most agentic rounds and bridged calls one run may make.
func (d AssistantDeepWork) Steps() int { return deepSteps.value(d.MaxSteps) }

// Tokens is the most tokens one run may spend.
func (d AssistantDeepWork) Tokens() int { return deepTokens.value(d.MaxTokens) }

// ErrDeepWorkOff is what [Company.AssistantDeepWork] answers for a company
// whose assistant has no deep work.
var ErrDeepWorkOff = errors.New("the assistant has no deep work: set assistant.deep_work.llm " +
	"to a Claude Code providers.llm entry")

// AssistantDeepWork is the providers.llm entry deep work runs on and its key,
// or why there is none: [ErrAssistantOff], [ErrDeepWorkOff], or the problems
// admission reports for the block. THE ONE PLACE IT IS HANDED OUT, for the
// reason [Company.AssistantChain] is.
func (c *Company) AssistantDeepWork() (string, LLMProvider, error) {
	if !c.Assistant.On() {
		return "", LLMProvider{}, ErrAssistantOff
	}
	d := c.Assistant.DeepWork
	if d == nil || d.LLM == "" {
		return "", LLMProvider{}, ErrDeepWorkOff
	}
	if err := c.deepWorkProblems(at(field("assistant"), "deep_work")); err != nil {
		return "", LLMProvider{}, err
	}
	return d.LLM, c.Providers.LLM[d.LLM], nil
}

// DeepWorkPlacement is the cell deep work runs in, and whether the company
// runs deep work at all: its entry's own `run_in`, else the first of e2b and
// container the company configures, else [PlacementDirect], the engine host,
// where the engine confines every run (ADR-0054). A REMOTE OR CONTAINED CELL
// IS PREFERRED whenever one is configured, because the host is the cell whose
// boundary is the engine's to hold rather than a machine's.
func (c *Company) DeepWorkPlacement() (Placement, bool) {
	_, entry, err := c.AssistantDeepWork()
	if err != nil {
		return "", false
	}
	if entry.RunIn != "" {
		return entry.RunIn, true
	}
	sb := c.Providers.Sandbox
	switch {
	case sb.Configured(PlacementE2B):
		return PlacementE2B, true
	case sb.Configured(PlacementContainer) && sb.Local != nil && sb.Local.Image != "":
		return PlacementContainer, true
	}
	return PlacementDirect, true
}

// deepWorkProblems reports every way `assistant.deep_work` cannot run: no
// entry, an unknown one, a CLI other than Claude Code, or a run_in this
// company configures no backend for.
func (c *Company) deepWorkProblems(path Path) error {
	var p problems
	d := c.Assistant.DeepWork
	if d == nil {
		return nil
	}
	for _, b := range []struct {
		bound assistantBound
		value int
	}{
		{deepTimeout, d.TimeoutSeconds},
		{deepSteps, d.MaxSteps},
		{deepTokens, d.MaxTokens},
	} {
		b.bound.check(path, b.value, &p)
	}
	llmPath := at(path, "llm")
	if d.LLM == "" {
		p.add(llmPath, ErrMissing, "deep work names no model: set it to a providers.llm "+
			"key whose entry has agent: claude-code")
		return p.err()
	}
	entry, ok := c.Providers.LLM[d.LLM]
	if !ok {
		p.add(llmPath, ErrUnknownValue, "%q is not a configured provider: providers.llm has %s",
			d.LLM, strings.Join(slices.Sorted(maps.Keys(c.Providers.LLM)), ", "))
		return p.err()
	}
	entryField := func(f string) Path { return at(entryPath(d.LLM), f) }
	// CLAUDE CODE ALONE. Both in-box runners continue a parked run in its
	// CLI's own session after the approval, but deep work's run on the
	// engine host is confined around one CLI's install and temp roots
	// (internal/sandbox's confinement, measured against Claude Code's), and
	// the engine drives it with that CLI's runner: another CLI there would
	// meet a confinement nobody measured it against.
	if CLIAgentName(entry.AgentName()) != CLIClaudeCode {
		p.add(llmPath, ErrConflict, "providers.llm.%s drives %s, and deep work runs on %s "+
			"alone: its run is confined around that CLI's own install and temp "+
			"directories, which no other CLI has been measured against. Set %s: %s",
			d.LLM, entry.AgentName(), CLIClaudeCode, entryField("agent"), CLIClaudeCode)
	}
	if entry.RunIn != "" && entry.RunIn != PlacementDirect && !c.Providers.Sandbox.Configured(entry.RunIn) {
		p.add(entryField("run_in"), ErrConflict, "deep work on providers.llm.%s runs in %s, "+
			"which providers.sandbox configures no backend for: configure it, or remove "+
			"run_in to take the first of e2b and container the company configures, else "+
			"the engine host under confinement", d.LLM, entry.RunIn)
	}
	return p.err()
}

// ErrAssistantOff is what [Company.AssistantChain] answers for a company that
// has not turned the assistant on.
var ErrAssistantOff = errors.New("the assistant is not enabled: set assistant.enabled to true")

// AssistantChain is the provider chain the assistant runs on, or why there is
// none: [ErrAssistantOff] for a company that has not turned it on, and
// otherwise the same problems admission reports for `assistant.llm`.
//
// THE ONE PLACE THE CHAIN IS HANDED OUT. Admission refuses a document whose
// chain breaks the assistant's rules, but a revision is APPLIED under the
// runnable rules alone (see [Company.ValidateRunnable]), so a stored revision
// can still carry one. Whatever builds the assistant's models asks here on
// every turn rather than reading `Assistant.LLM`, so such a revision leaves
// the assistant unavailable, with this error as the reason, and every seat
// running.
func (c *Company) AssistantChain() (ProviderKeys, error) {
	if !c.Assistant.On() {
		return nil, ErrAssistantOff
	}
	if err := c.assistantChainProblems(field("assistant")); err != nil {
		return nil, err
	}
	return slices.Clone(c.Assistant.LLM), nil
}

// validateAssistant holds the `assistant:` block to its rules.
//
// AN ADMISSION RULE, every part of it: nothing about running a SEAT depends on
// the block, and the chain's safety is enforced again where the chain is
// handed out ([Company.AssistantChain]), so applying a revision that breaks
// one of these is safe and refusing it would stop every seat over a drawer.
func (c *Company) validateAssistant() error {
	path := field("assistant")
	a := c.Assistant
	var p problems
	for _, b := range []struct {
		bound assistantBound
		value int
	}{
		{assistantSteps, a.MaxSteps},
		{assistantTimeout, a.TurnTimeoutSeconds},
		{assistantProposal, a.ProposalTTLSeconds},
		{assistantRetention, a.RetentionDays},
	} {
		b.bound.check(path, b.value, &p)
	}
	p.wrap(a.TokenBudget.validate(at(path, "token_budget")))
	if a.On() || len(a.LLM) > 0 {
		p.wrap(c.assistantChainProblems(path))
	}
	p.wrap(c.deepWorkProblems(at(path, "deep_work")))
	return p.err()
}

// assistantChainProblems reports every way `assistant.llm` cannot carry the
// assistant: empty while enabled, a key providers.llm does not configure, or
// an entry whose merged profile leaves the CLI's local or web tools
// reachable. Each problem names the field to change.
func (c *Company) assistantChainProblems(path Path) error {
	var p problems
	llmPath := at(path, "llm")
	if len(c.Assistant.LLM) == 0 {
		p.add(llmPath, ErrMissing,
			"the assistant is enabled but names no model: list a providers.llm "+
				"key, e.g. llm: [assistant], whose CLI's local tools are denied and "+
				"whose entry has overrides: {web_tools: denied}")
		return p.err()
	}
	known := slices.Sorted(maps.Keys(c.Providers.LLM))
	for i, key := range c.Assistant.LLM {
		member := idx(llmPath, i)
		entry, ok := c.Providers.LLM[key]
		if !ok {
			configured := "none"
			if len(known) > 0 {
				configured = strings.Join(known, ", ")
			}
			p.add(member, ErrUnknownValue,
				"%q is not a configured provider: providers.llm has %s", key, configured)
			continue
		}
		p.wrap(assistantMemberProblems(member, key, &entry))
	}
	return p.err()
}

// AssistantEntryProblems reports every way the providers.llm entry key could
// not carry the assistant, judged as a member of `assistant.llm` would be:
// for `crewlet assistant eval -model`, which measures a candidate before the
// chain names it, so a model that may never run the assistant is never
// measured as though it could.
func (c *Company) AssistantEntryProblems(key string) error {
	entry, ok := c.Providers.LLM[key]
	if !ok {
		return fmt.Errorf("%w: providers.llm has no entry %q", ErrUnknownValue, key)
	}
	return assistantMemberProblems(entryPath(key), key, &entry)
}

// assistantMemberProblems judges one chain member against the assistant's
// rules, naming the entry's own field each time, since that is what changes.
func assistantMemberProblems(member Path, key string, entry *LLMProvider) error {
	var p problems
	entryField := func(f string) Path { return at(entryPath(key), f) }
	if entry.Agent != "" && !entry.Agent.Valid() {
		return p.err() // the entry's own rule names the agent; there is no profile
	}
	profile, err := cliprofile.Load(entry.AgentName(), entry.Overrides)
	if err != nil {
		// The entry's own validation reports why its profile does not
		// load; an assistant member nobody can judge is refused here too,
		// so AssistantChain never hands one out.
		p.add(member, ErrConflict,
			"providers.llm.%s cannot be judged for the assistant because its profile "+
				"does not load (see %s)", key, entryField("overrides"))
		return p.err()
	}
	// NO TOOL OF ITS OWN, BY CONSTRUCTION. The stance is a claim; what the
	// assistant is admitted on is the argv its calls run (the profile Load
	// returned, its web denial applied), which must leave the model no shell,
	// file tool or tool server but the engine's own: one on the engine host
	// reads the engine's own data and keys, outside every request of the
	// person's (ADR-0050).
	eligible := strings.Join(cliprofile.AssistantEligible(), ", ")
	switch {
	case profile.LocalTools != cliprofile.LocalToolsDenied:
		p.add(member, ErrConflict,
			"providers.llm.%s runs the %s CLI with its own local tools %s (%s), so a shell "+
				"or a file reader on the engine host could read what no request of the "+
				"person's returned: the assistant needs a CLI whose profile has local_tools: "+
				"denied and whose run has no tool of its own, which of the built-in profiles "+
				"is %s", key, entry.AgentName(), localToolsWord(profile.LocalTools),
			strings.TrimSpace(profile.LocalToolsNote), eligible)
	default:
		if problem := profile.ToolsOffProblem(); problem != "" {
			p.add(member, ErrConflict,
				"providers.llm.%s says the %s CLI's local tools are denied, but %s, so its "+
					"run is not shown to leave the model without a tool of its own: a shell or "+
					"a file reader on the engine host could read what no request of the "+
					"person's returned. The assistant runs only on a CLI whose argv removes "+
					"every tool (tools_off), which of the built-in profiles is %s; check %s",
				key, entry.AgentName(), problem, eligible, entryField("overrides"))
		}
	}
	if profile.EffectiveWebTools() != cliprofile.WebToolsDenied {
		remedy := fmt.Sprintf("set %s: {web_tools: denied}", entryField("overrides"))
		if !profile.CanDenyWeb() {
			remedy = fmt.Sprintf("the %s profile has no measured web denial (%s), so "+
				"use a CLI that has one: %s",
				entry.AgentName(), strings.TrimSpace(profile.WebToolsNote),
				strings.Join(cliprofile.WebDeniable(), ", "))
		}
		p.add(member, ErrConflict,
			"providers.llm.%s keeps the CLI's own web tools on, so the model could "+
				"read pages no authority decision saw and send the person's data "+
				"out: %s", key, remedy)
	}
	return p.err()
}

// localToolsWord renders a local-tools stance inside a sentence.
func localToolsWord(t cliprofile.LocalTools) string {
	if t == "" {
		return "undeclared"
	}
	return "at the " + string(t) + " stance"
}
