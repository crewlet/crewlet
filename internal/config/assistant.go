package config

import (
	"encoding/json"
	"errors"
	"slices"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/redact"
	"github.com/crewlet/crewlet/internal/words"
)

// Assistant is the Tier B `assistant:` block: the one model Ask Crewlet runs
// on, each person's spend ceiling, and the company's instructions on how it
// writes.
//
//	assistant:
//	  model: assistant    # a providers.llm key; absent means off
//	  instructions: {length: brief, text: Use British spelling.}
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

	// Instructions are the company's layer of [AssistantInstructions]; each
	// person's own layer is kept beside their Home board and wins over it.
	Instructions AssistantInstructions `yaml:"instructions,omitempty" json:"instructions,omitzero" desc:"How Ask Crewlet writes for everybody in the company: answer length, layout and free text on tone and style. Each person may add their own, which win where the two differ. Instructions never change what Ask Crewlet may read, propose or do."`
}

// AssistantInstructions is standing guidance on how Ask Crewlet writes, the
// company's or one person's. It shapes tone, length and layout only: the
// prompt fences it below the engine's rules, and every permission and approval
// is enforced in code whatever it says.
type AssistantInstructions struct {
	Length AnswerLength `yaml:"length,omitempty" json:"length,omitempty" js:"enum=brief|balanced|detailed" desc:"How long answers run: brief (a few sentences), balanced (a short paragraph or list) or detailed (the reasons and relevant detail too). Absent: as short as each question allows."`
	Format AnswerFormat `yaml:"format,omitempty" json:"format,omitempty" js:"enum=lead|lists|tldr" desc:"How answers are laid out: lead (the answer first, in plain sentences), lists (bullets and tables where they help) or tldr (a one-line TL;DR first). Absent: the answer first, with lists and tables only where they carry structure."`
	Text   string       `yaml:"text,omitempty" json:"text,omitempty" js:"maxlen=1000" desc:"Anything else about how Ask Crewlet writes, in your own words: tone, terms to use or avoid, structure. At most 1000 characters. A credential is refused: the text reaches every answer's prompt."`
}

// AssistantInstructionsMaxRunes bounds an instructions text, in characters:
// a dozen short preferences, about 250 tokens a layer on every round's
// prompt. A test holds the schema's maxlen to it.
const AssistantInstructionsMaxRunes = 1000

// AnswerLength is how long Ask Crewlet's answers run.
type AnswerLength string

// The lengths a layer may set; none leaves the engine's default.
const (
	AnswerBrief    AnswerLength = "brief"
	AnswerBalanced AnswerLength = "balanced"
	AnswerDetailed AnswerLength = "detailed"
)

// AnswerLengths are the three, shortest first.
var AnswerLengths = []AnswerLength{AnswerBrief, AnswerBalanced, AnswerDetailed}

// Valid reports whether l is one of [AnswerLengths].
func (l AnswerLength) Valid() bool { return slices.Contains(AnswerLengths, l) }

// AnswerFormat is how Ask Crewlet lays an answer out.
type AnswerFormat string

// The formats a layer may set; none leaves the engine's default.
const (
	AnswerLead  AnswerFormat = "lead"
	AnswerLists AnswerFormat = "lists"
	AnswerTLDR  AnswerFormat = "tldr"
)

// AnswerFormats are the three.
var AnswerFormats = []AnswerFormat{AnswerLead, AnswerLists, AnswerTLDR}

// Valid reports whether f is one of [AnswerFormats].
func (f AnswerFormat) Valid() bool { return slices.Contains(AnswerFormats, f) }

// IsZero reports a layer that sets nothing.
func (i AssistantInstructions) IsZero() bool { return i == AssistantInstructions{} }

// ReadAssistantInstructions is a stored person's layer as every reader takes
// it: one that does not decode or validate (a value only a newer build knows)
// reads as none, so a style preference never stops an answer.
func ReadAssistantInstructions(raw []byte) AssistantInstructions {
	var i AssistantInstructions
	if json.Unmarshal(raw, &i) != nil || i.Validate(nil) != nil {
		return AssistantInstructions{}
	}
	return i
}

// Validate holds a layer to its rules, naming each fault under path: the
// company's under `assistant.instructions`, a person's under the tool's
// argument. A credential is refused, never echoed, so none reaches a prompt
// or the record of a change.
func (i AssistantInstructions) Validate(path Path) error {
	var p problems
	if i.Length != "" && !i.Length.Valid() {
		p.add(at(path, "length"), ErrUnknownValue, "%q is not a length; give %s", i.Length,
			words.Or(AnswerLengths))
	}
	if i.Format != "" && !i.Format.Valid() {
		p.add(at(path, "format"), ErrUnknownValue, "%q is not a format; give %s", i.Format,
			words.Or(AnswerFormats))
	}
	if n := utf8.RuneCountInString(i.Text); n > AssistantInstructionsMaxRunes {
		p.add(at(path, "text"), ErrOutOfRange, "the text runs to %d characters; shorten it "+
			"to at most %d", n, AssistantInstructionsMaxRunes)
	} else if !utf8.ValidString(i.Text) {
		p.add(at(path, "text"), ErrShape, "the text is not valid UTF-8")
	}
	if redact.Contains(i.Text) {
		p.add(at(path, "text"), ErrConflict, "the text holds what looks like a credential; "+
			"remove it, since instructions reach every answer's prompt")
	}
	return p.err()
}

// On reports whether the company assigned Ask Crewlet a model.
func (a Assistant) On() bool { return a.Model != "" }

// IsZero lets an unset block drop out of a JSON round trip.
func (a Assistant) IsZero() bool {
	return a.Model == "" && a.TokenBudget == (TokenBudget{}) && a.Instructions.IsZero()
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
	p.wrap(a.Instructions.Validate(at(path, "instructions")))
	if a.On() {
		p.wrap(c.helperModelProblem(at(path, "model"), a.Model))
	}
	return p.err()
}
