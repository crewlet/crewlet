package claudemodel

import (
	"errors"
	"fmt"
	"slices"
	"strings"
)

// The rules an entry's settings are held to, against the profile its requests
// are shaped from. Written ONCE, here, because two callers ask them: the
// config tier, when the model is written literally and `crewlet validate` can
// judge it offline, and the backend's constructor, when it is a `${VAR}` that
// only resolves at build. Written twice, the two would drift, and an entry
// that validated clean would be refused at boot — or the reverse, and every
// call would be a 400.
//
// Each failure wraps a sentinel, so each caller can classify it (a conflict
// or a value out of range) and say it in its own field names; the message
// after the sentinel names the model and the fact that makes it a 400.

// MinThinkingBudget is the smallest `budget_tokens` the API accepts.
const MinThinkingBudget = 1024

// DefaultEffort is the level an entry that names none is sent at, on a model
// that takes effort at all.
//
// HIGH because every phase this engine runs is long-horizon agentic tool use,
// which is what the vendor recommends at least `high` for — and because
// sending nothing hands the depth to the model's own default, which is
// `medium` on Opus 5.5 and `high` everywhere else, so moving an entry to the
// newer model would silently make it think less. Every row that takes effort
// takes this level (held by a test), so it can never be the 400 it would be
// if it were not.
const DefaultEffort = EffortHigh

// The failures, for errors.Is.
var (
	// ErrNotInTable is a `claude_model` naming no row of the table.
	ErrNotInTable = errors.New("not a model the capability table knows")
	// ErrOverrideNotNeeded is a `claude_model` beside a model the table
	// already reads: one entry may not carry two answers to "which model
	// is this".
	ErrOverrideNotNeeded = errors.New("the capability table already reads this model")
	// ErrTakesNoEffort is an effort level on a model that takes none.
	ErrTakesNoEffort = errors.New("model takes no effort level")
	// ErrEffortLevel is a level the model does not accept.
	ErrEffortLevel = errors.New("effort level not accepted")
	// ErrTakesNoBudget is a thinking budget on a model that thinks
	// adaptively.
	ErrTakesNoBudget = errors.New("model takes no thinking budget")
	// ErrBudgetRange is a budget the API refuses for its size.
	ErrBudgetRange = errors.New("thinking budget out of range")
)

// Resolve is the profile an entry's requests are shaped from: claudeModel's
// row when it names one, else model's, else [Modern].
func Resolve(model, claudeModel string) (Profile, error) {
	if claudeModel == "" {
		profile, _ := Lookup(model)
		return profile, nil
	}
	if !Known(claudeModel) {
		return Profile{}, fmt.Errorf("%w: %q (want one of %s)",
			ErrNotInTable, claudeModel, strings.Join(IDs(), ", "))
	}
	if own, known := Lookup(model); known {
		return Profile{}, fmt.Errorf(
			"%w: %q is %s, so naming %s for it says the same thing twice "+
				"or contradicts it; an override is only for an alias the table cannot read",
			ErrOverrideNotNeeded, model, own.ID, claudeModel)
	}
	profile, _ := Lookup(claudeModel)
	return profile, nil
}

// Thinks reports whether a request to this model thinks, given the entry's
// budget: always on an adaptive model, and only with a budget on a budget-era
// one. It is what decides whether a temperature or a caller's output cap can
// be sent at all — a thinking request takes neither.
func (p Profile) Thinks(budget int) bool {
	return p.Thinking == ThinkingAdaptive || budget > 0
}

// Takes reports whether the model accepts effort level e.
func (p Profile) Takes(e Effort) bool { return slices.Contains(p.Efforts, e) }

// CheckEffort refuses an effort level the model would answer with a 400.
// Empty is no level, and is always accepted. model is the id as the entry
// wrote it, for the message.
func (p Profile) CheckEffort(model string, e Effort) error {
	switch {
	case e == "":
		return nil
	case !e.Valid():
		return fmt.Errorf("%w: %q is not a level (want %s)", ErrEffortLevel, e, levels(Efforts))
	case len(p.Efforts) == 0:
		return fmt.Errorf("%w: %s answers output_config.effort with a 400",
			ErrTakesNoEffort, p.label(model))
	case !p.Takes(e):
		return fmt.Errorf("%w: %s takes %s, not %q",
			ErrEffortLevel, p.label(model), levels(p.Efforts), e)
	}
	return nil
}

// CheckBudget refuses a thinking budget the model would answer with a 400.
// Zero is no budget, and is always accepted.
func (p Profile) CheckBudget(model string, budget int) error {
	switch {
	case budget == 0:
		return nil
	case budget < 0:
		return fmt.Errorf("%w: must not be negative, got %d", ErrBudgetRange, budget)
	case p.Thinking != ThinkingBudget:
		return fmt.Errorf("%w: %s thinks adaptively, where budget_tokens is a 400 "+
			"or deprecated", ErrTakesNoBudget, p.label(model))
	case budget < MinThinkingBudget:
		return fmt.Errorf("%w: %d is below the API's minimum of %d",
			ErrBudgetRange, budget, MinThinkingBudget)
	case budget >= p.MaxOutput:
		return fmt.Errorf("%w: %d is not below %s's output cap of %d tokens, "+
			"which the thinking and the answer share", ErrBudgetRange, budget, p.label(model), p.MaxOutput)
	}
	return nil
}

// label names the model a message is about: its row, or the id as written
// when the table does not know it.
func (p Profile) label(model string) string {
	if p.ID != "" {
		return p.ID
	}
	return fmt.Sprintf("%q (not in the capability table, so shaped as the current generation)", model)
}

func levels(efforts []Effort) string {
	out := make([]string, len(efforts))
	for i, e := range efforts {
		out[i] = string(e)
	}
	return strings.Join(out, ", ")
}
