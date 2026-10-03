package org

import (
	"errors"
	"fmt"
	"slices"
	"strconv"

	"github.com/crewlet/crewlet/internal/period"
)

// TokenCeilings is a token budget as the engine reads it: for each calendar
// window that is capped, the most one scope — the whole company, or one seat
// — may spend inside it.
//
// # A period that is absent is uncapped
//
// And that is the ONLY way a window is left open. There is no ceiling of 0
// meaning "unlimited" and no ceiling of 0 meaning "nothing may be spent":
// the authored form refuses a ceiling below one token
// (config.TokenBudget), because one number read two opposite ways by two
// readers is how a company that meant to stop a seat leaves it spending. So
// the zero value of this type — no entry at all — is a scope nobody capped,
// and every entry is a real ceiling.
//
// # The rules a ceiling must meet are HERE, and nowhere else
//
// config.TokenBudget.Ceilings makes one from a `token_budget:` mapping, and
// a seat's chart runtime half decodes straight into one, so this is the one
// type both doors reach — which is why [TokenCeilings.Faults] is the one
// statement of what a ceiling may be. A company file's `token_budget` and every
// seat's are refused through it, and so is a runtime write to the org chart,
// which no config validation ever sees. This is the shape everything
// downstream of the configuration reads — one period to one number — so no
// reader has to know that the authored form holds pointers to tell an absent
// key from a 0.
//
// The windows are the company calendar's ([period]), cut on the company's one
// clock: a day ceiling is what one local day may spend, a week ceiling one ISO
// week from Monday, a month ceiling one calendar month. The fleet's counters
// keep a slot per window and admit a charge only while every capped window has
// room for it, each window's allowance coming back when it turns over; a charge
// carries this map to them as coord.Caps (ADR-0019).
type TokenCeilings map[period.Period]int

// ErrTokenCeiling is a window capped below one token, which is no ceiling.
var ErrTokenCeiling = errors.New("token ceiling below one token")

// ErrUnknownWindow is a token budget keyed by something that is not one of the
// company calendar's windows.
var ErrUnknownWindow = errors.New("not a calendar window")

// CeilingFault is one key of a token budget that is not a ceiling.
//
// ITS SENTENCE NAMES THE KEY TO CHANGE AND THE REMEDY, whichever door the
// budget came through: an author reads it against a company file, an
// operator against a `PATCH /chart` body, and in both the line to change is
// `token_budget.<window>`. The PATH to that key is the caller's to place —
// a company's own block, a seat in a file, a seat in the chart — so the
// sentence leads with the key rather than with where it sits.
type CeilingFault struct {
	// Window is the key, which is a [period.Period] that may not be Valid.
	Window period.Period
	// Ceiling is the value written under it.
	Ceiling int
}

// Error is the sentence a caller places at the key.
func (f CeilingFault) Error() string {
	if !f.Window.Valid() {
		return fmt.Sprintf("%q is not a calendar window: a budget caps a `day`, "+
			"a `week` or a `month`, each optional — remove `token_budget.%s`, or "+
			"write the ceiling under the window it should reset on",
			string(f.Window), string(f.Window))
	}
	return fmt.Sprintf("must be at least 1 token, got %d: a window is left "+
		"without a ceiling by leaving its key out, never by writing 0 — remove "+
		"`token_budget.%s` for no %s ceiling", f.Ceiling, f.Window, adjective(f.Window))
}

// Unwrap is the rule broken: [ErrUnknownWindow] for a key that is no window,
// [ErrTokenCeiling] for a window capped below one token.
func (f CeilingFault) Unwrap() error {
	if !f.Window.Valid() {
		return ErrUnknownWindow
	}
	return ErrTokenCeiling
}

// Faults is every key of c that is not a ceiling: the calendar's windows
// shortest first, then every key that is no window, in byte order — so a
// budget with two faults names them in the same order on every read.
//
// A CEILING OF ONE TOKEN IS THE SMALLEST, and there is no other bound: one
// number read two opposite ways — 0 as "unlimited" and as "nothing" — is how a
// company that meant to stop a seat leaves it spending, so neither reading is
// offered, and an uncapped window is one whose key is absent.
func (c TokenCeilings) Faults() []CeilingFault {
	var out []CeilingFault
	for _, window := range period.Periods {
		if ceiling, capped := c[window]; capped && ceiling < 1 {
			out = append(out, CeilingFault{Window: window, Ceiling: ceiling})
		}
	}
	var unknown []period.Period
	for window := range c {
		if !window.Valid() {
			unknown = append(unknown, window)
		}
	}
	slices.Sort(unknown)
	for _, window := range unknown {
		out = append(out, CeilingFault{Window: window, Ceiling: c[window]})
	}
	return out
}

// adjective is how a sentence names a window's ceiling: "no daily ceiling".
func adjective(p period.Period) string {
	switch p {
	case period.Day:
		return "daily"
	case period.Week:
		return "weekly"
	case period.Month:
		return "monthly"
	}
	return strconv.Quote(string(p))
}
