package org

import (
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
// # Built from the authored block, never written directly
//
// config.TokenBudget.Ceilings makes one from a `token_budget:` mapping, and
// the rules a ceiling must meet are enforced there, where a refusal can name
// the key an author wrote. This is the shape everything downstream of the
// configuration reads — one period to one number — so no reader has to know
// that the authored form holds pointers to tell an absent key from a 0.
//
// The windows are the company calendar's ([period]), cut on the company's one
// clock: a day ceiling is what one local day may spend, a week ceiling one ISO
// week from Monday, a month ceiling one calendar month. The fleet's counters
// keep a slot per window and admit a charge only while every capped window has
// room for it, each window's allowance coming back when it turns over; a charge
// carries this map to them as coord.Caps (ADR-0019).
type TokenCeilings map[period.Period]int
