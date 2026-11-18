package config

import (
	"errors"
	"slices"

	"github.com/crewlet/crewlet/internal/words"
)

// Advisor is the setup advisor's own settings: its analyst, which reads the
// company's records with a model and lists only what it can cite (ADR-0157).
type Advisor struct {
	Analyst AdvisorAnalyst `yaml:"analyst,omitempty" json:"analyst,omitzero" desc:"The advisor's analyst: a run on the company's own models that reads the week's records and lists problems no rule names, each with the records and numbers it rests on. Absent = off."`
}

// AdvisorAnalyst turns the analyst on and bounds it.
type AdvisorAnalyst struct {
	LLM ProviderKeys `yaml:"llm,omitempty" json:"llm,omitempty" desc:"The providers.llm entries the analyst runs on, the first first, each held to Ask Crewlet's rules for a member (the harness's local and web tools denied). Absent = the analyst is off."`

	Cadence AnalystCadence `yaml:"cadence,omitempty" json:"cadence,omitempty" desc:"How often it runs on its own: daily (the default), weekly (on Mondays) or off. Run now on Settings, Advisor runs it whatever this says."`

	Hour *int `yaml:"hour,omitempty" json:"hour,omitempty" js:"min=0;max=23" desc:"The hour of the company clock a scheduled run starts at, 0 to 23. Absent = 6."`

	MaxTokens *int `yaml:"max_tokens,omitempty" json:"max_tokens,omitempty" js:"min=20000" desc:"The most tokens one run may spend, every round together, charged to the company's budget and no seat's; a run that reaches it stops and lists what it grounded so far. Absent = 200000; at least 20000."`
}

// AnalystCadence is how often the analyst runs on its own.
type AnalystCadence string

// The cadences.
const (
	AnalystDaily  AnalystCadence = "daily"
	AnalystWeekly AnalystCadence = "weekly"
	AnalystOff    AnalystCadence = "off"
)

// AnalystCadences is every cadence.
var AnalystCadences = []AnalystCadence{AnalystDaily, AnalystWeekly, AnalystOff}

// Valid reports whether c is a cadence; empty is the default, daily.
func (c AnalystCadence) Valid() bool { return c == "" || slices.Contains(AnalystCadences, c) }

// The analyst's defaults and floor. A run reads about ten tool answers of a few
// thousand tokens each beside its prompt; the floor is one round of that.
const (
	DefaultAnalystHour      = 6
	DefaultAnalystMaxTokens = 200_000
	MinAnalystMaxTokens     = 20_000
)

// On reports whether the analyst names a model.
func (a AdvisorAnalyst) On() bool { return len(a.LLM) > 0 }

// Every is the cadence it runs on, daily where none is set.
func (a AdvisorAnalyst) Every() AnalystCadence {
	if a.Cadence == "" {
		return AnalystDaily
	}
	return a.Cadence
}

// StartHour is the company-clock hour a scheduled run starts at.
func (a AdvisorAnalyst) StartHour() int {
	if a.Hour == nil {
		return DefaultAnalystHour
	}
	return *a.Hour
}

// Ceiling is the most tokens one run may spend.
func (a AdvisorAnalyst) Ceiling() int {
	if a.MaxTokens == nil {
		return DefaultAnalystMaxTokens
	}
	return *a.MaxTokens
}

// ErrAnalystOff is what [Company.AnalystChain] answers for a company whose
// analyst names no model.
var ErrAnalystOff = errors.New("advisor.analyst.llm names no model, so the analyst is off: " +
	"list a providers.llm key, e.g. llm: [assistant]")

// AnalystChain is the one place `advisor.analyst.llm` is handed out: the chain
// held to Ask Crewlet's member rules, or why there is none.
func (c *Company) AnalystChain() (ProviderKeys, error) {
	if !c.Advisor.Analyst.On() {
		return nil, ErrAnalystOff
	}
	if err := c.chainProblems(field("advisor.analyst.llm"), c.Advisor.Analyst.LLM, ""); err != nil {
		return nil, err
	}
	return slices.Clone(c.Advisor.Analyst.LLM), nil
}

// validateAdvisor holds the `advisor:` block to its rules: an admission rule,
// since no seat depends on it and the chain is judged again where it is handed out.
func (c *Company) validateAdvisor() error {
	a := c.Advisor.Analyst
	path := field("advisor.analyst")
	var p problems
	if !a.Cadence.Valid() {
		p.add(at(path, "cadence"), ErrUnknownValue, "%q (want %s)", a.Cadence, words.Or(AnalystCadences))
	}
	if a.Hour != nil && (*a.Hour < 0 || *a.Hour > 23) {
		p.add(at(path, "hour"), ErrOutOfRange, "must be an hour of the company clock, 0 to 23, got %d", *a.Hour)
	}
	if a.MaxTokens != nil && *a.MaxTokens < MinAnalystMaxTokens {
		p.add(at(path, "max_tokens"), ErrOutOfRange, "must be at least %d, one round of the analyst's "+
			"prompt and tool answers, got %d", MinAnalystMaxTokens, *a.MaxTokens)
	}
	if a.On() {
		p.wrap(c.chainProblems(at(path, "llm"), a.LLM, ""))
	}
	return p.err()
}
