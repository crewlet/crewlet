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
//	  max_steps: 8                  # 1..16
//	  turn_timeout_seconds: 120     # 30..600
//	  proposal_ttl_seconds: 900     # 60..3600
//	  retention_days: 30            # 1..365
//	  token_budget: { day: 200000 } # per person, optional
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
// Every member of `llm` must be a text-mode entry whose profile denies the
// CLI's own local tools AND its own web tools. The assistant's guarantee
// (ADR-0046) is that it reads only what the person's own requests return; a
// CLI in agent mode runs its own tool loop whose calls never pass the guard, a
// shell reads whatever the engine user can, and a web fetch is both text no
// authority decision ever saw and a way out for whatever the person's data
// put in the prompt. [Company.AssistantChain] is the one place the chain is
// handed out, and it refuses a chain that breaks this, so a stored revision
// carrying one leaves the assistant unavailable rather than unsafe.
type Assistant struct {
	Enabled *bool `yaml:"enabled,omitempty" json:"enabled,omitempty" desc:"Run the Dashboard Assistant for this company. Absent or false: off."`

	// LLM is the assistant's provider chain, the first key tried first, in
	// the shape a seat's llm_auxiliary takes.
	LLM ProviderKeys `yaml:"llm,omitempty" json:"llm,omitzero" desc:"providers.llm keys the assistant runs on, a fallback chain. Required when enabled. Each must be a text-mode entry denying local tools and, through overrides.web_tools, web tools."`

	MaxSteps           int `yaml:"max_steps,omitempty" json:"max_steps,omitempty" js:"min=0;max=16" desc:"Most tool steps one assistant turn may take, 1..16. Absent: 8."`
	TurnTimeoutSeconds int `yaml:"turn_timeout_seconds,omitempty" json:"turn_timeout_seconds,omitempty" js:"min=0;max=600" desc:"Longest one assistant turn may run, 30..600 seconds. Absent: 120."`
	ProposalTTLSeconds int `yaml:"proposal_ttl_seconds,omitempty" json:"proposal_ttl_seconds,omitempty" js:"min=0;max=3600" desc:"How long a proposed change waits for approval, 60..3600 seconds. Absent: 900."`
	RetentionDays      int `yaml:"retention_days,omitempty" json:"retention_days,omitempty" js:"min=0;max=365" desc:"How long a person's assistant conversations are kept, 1..365 days. Absent: 30."`

	// TokenBudget is each PERSON's ceiling per calendar window, in the shape
	// a seat's budget takes, charged on top of the company's own.
	TokenBudget TokenBudget `yaml:"token_budget,omitempty" json:"token_budget,omitzero" desc:"Each person's assistant token ceilings per calendar window on the company clock: day, week and month, each optional. Absent = no per-person ceiling; the company's own budget still applies."`
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
//   - max_steps: a step is one tool call and one model round, and a turn that
//     needs more than sixteen is a task for a seat, not a question in a drawer.
//     Eight answers a three-read question with room for a correction.
//   - turn_timeout_seconds: every round is a CLI launch, so a turn of eight
//     rounds needs about two minutes at the latency the plan measures; thirty
//     seconds is one slow round, and ten minutes is a seat's coding call.
//   - proposal_ttl_seconds: long enough to read a diff and step up, short
//     enough that what was previewed is still what the write meets.
//   - retention_days: a month of history by default, a year at most, because
//     a transcript holds whatever the person's reads returned.
var (
	assistantSteps     = assistantBound{key: "max_steps", min: 1, max: 16, def: 8, unit: "steps"}
	assistantTimeout   = assistantBound{key: "turn_timeout_seconds", min: 30, max: 600, def: 120, unit: "seconds"}
	assistantProposal  = assistantBound{key: "proposal_ttl_seconds", min: 60, max: 3600, def: 900, unit: "seconds"}
	assistantRetention = assistantBound{key: "retention_days", min: 1, max: 365, def: 30, unit: "days"}
)

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
		a.ProposalTTLSeconds == 0 && a.RetentionDays == 0 && a.TokenBudget == (TokenBudget{})
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
	return p.err()
}

// assistantChainProblems reports every way `assistant.llm` cannot carry the
// assistant: empty while enabled, a key providers.llm does not configure, an
// entry in agent mode, or one whose merged profile leaves the CLI's local or
// web tools reachable. Each problem names the field to change.
func (c *Company) assistantChainProblems(path Path) error {
	var p problems
	llmPath := at(path, "llm")
	if len(c.Assistant.LLM) == 0 {
		p.add(llmPath, ErrMissing,
			"the assistant is enabled but names no model: list a providers.llm "+
				"key, e.g. llm: [assistant], whose entry is in text mode and has "+
				"overrides: {web_tools: denied}")
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

// assistantMemberProblems judges one chain member against the assistant's
// rules, naming the entry's own field each time, since that is what changes.
func assistantMemberProblems(member Path, key string, entry *LLMProvider) error {
	var p problems
	entryField := func(f string) Path { return at(entryPath(key), f) }
	if entry.AgentMode() {
		p.add(member, ErrConflict,
			"providers.llm.%s is in agent mode, where the CLI runs its own tool loop "+
				"and its calls never pass the guard the assistant's every read goes "+
				"through: point the assistant at a text-mode entry (remove %s)",
			key, entryField("mode"))
	}
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
	if profile.LocalTools != cliprofile.LocalToolsDenied {
		p.add(member, ErrConflict,
			"providers.llm.%s runs the %s CLI with its own local tools %s, so a shell "+
				"on the engine host could read what no request of the person's "+
				"returned: the assistant needs a CLI whose profile has local_tools: "+
				"denied, and this one's does not", key, entry.AgentName(), localToolsWord(profile.LocalTools))
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
