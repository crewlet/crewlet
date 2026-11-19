package config

import (
	"errors"
)

// Assistant is the Tier B `assistant:` block: the one model Ask Crewlet runs
// on, and each person's spend ceiling.
//
//	assistant:
//	  model: assistant    # a providers.llm key; absent means off
//
// Every bound of a turn is fixed in code where it is consumed. An
// absent model is off, so an upgrade never switches on a model reading the
// company's data.
type Assistant struct {
	// Model is the providers.llm entry Ask Crewlet runs on, held to
	// [Company.HelperModelProblem]: a harness the engine can run with no tool
	// of its own and no web tool, so its only tool source on the engine host
	// is the engine's `crewlet` server, serving the person's own requests
	// (ADR-0050). [Company.AssistantModel] hands it out.
	Model string `yaml:"model,omitempty" json:"model,omitempty" desc:"The providers.llm key Ask Crewlet runs on: any entry on a harness the engine can run with no tool of its own (Claude Code or pi), which turns the harness's own web tools off for Ask Crewlet whatever the entry says. Absent: Ask Crewlet is off."`

	// TokenBudget is each PERSON's ceiling per calendar window, in the shape
	// a seat's budget takes, charged on top of the company's own.
	TokenBudget TokenBudget `yaml:"token_budget,omitempty" json:"token_budget,omitzero" desc:"Each person's Ask Crewlet token ceilings per calendar window on the company clock: day, week and month, each optional. Absent = no per-person ceiling; the company's own budget still applies. A ceiling at or above the company's on a window that holds it is warned of, since the company's is reached first."`
}

// On reports whether the company assigned Ask Crewlet a model.
func (a Assistant) On() bool { return a.Model != "" }

// IsZero lets an unset block drop out of a JSON round trip.
func (a Assistant) IsZero() bool {
	return a.Model == "" && a.TokenBudget == (TokenBudget{})
}

// ErrAssistantOff is what [Company.AssistantModel] answers for a company that
// assigns Ask Crewlet no model.
var ErrAssistantOff = errors.New("assistant.model is not set, so Ask Crewlet is off for this " +
	"company: set it to a providers.llm key")

// AssistantModel is the providers.llm key Ask Crewlet runs on, or why there is
// none: [ErrAssistantOff] for a company that assigns it none, and otherwise the
// problem admission reports for `assistant.model`.
//
// THE ONE PLACE THE MODEL IS HANDED OUT. Admission refuses a document whose
// model no helper can run on, but a revision is APPLIED under the runnable
// rules alone (see [Company.ValidateRunnable]), so a stored revision can still
// carry one. Whatever builds the assistant's model asks here on every turn
// rather than reading `Assistant.Model`, so such a revision leaves Ask Crewlet
// unavailable, with this error as the reason, and every seat running.
func (c *Company) AssistantModel() (string, error) {
	if !c.Assistant.On() {
		return "", ErrAssistantOff
	}
	if err := c.helperModelProblem(field("assistant.model"), c.Assistant.Model); err != nil {
		return "", err
	}
	return c.Assistant.Model, nil
}

// validateAssistant holds the `assistant:` block to its rules.
//
// AN ADMISSION RULE, every part of it: nothing about running a SEAT depends on
// the block, and the model's rule is enforced again where it is handed out
// ([Company.AssistantModel]), so applying a revision that breaks one of these
// is safe and refusing it would stop every seat over Ask Crewlet.
func (c *Company) validateAssistant() error {
	path := field("assistant")
	a := c.Assistant
	var p problems
	p.wrap(a.TokenBudget.validate(at(path, "token_budget")))
	if a.On() {
		p.wrap(c.helperModelProblem(at(path, "model"), a.Model))
	}
	return p.err()
}
