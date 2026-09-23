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
// week from Monday, a month ceiling one calendar month.
type TokenCeilings map[period.Period]int

// Tightest is the smallest ceiling the scope declares, and false when it
// declares none.
//
// It is the one number a counter that knows nothing of windows can be held
// to without admitting a charge the windows would refuse. Such a counter —
// one scope's spend since it was last reset — never reads less than what any
// single window of that span has spent, so a charge that fits under the
// smallest ceiling on it fits every window's ceiling too. The converse does
// not hold, which is the price of the counter knowing no calendar: it refuses
// charges a window would still admit once the day, week or month it began in
// has turned over.
func (c TokenCeilings) Tightest() (int, bool) {
	limit, capped := 0, false
	for _, ceiling := range c {
		if !capped || ceiling < limit {
			limit, capped = ceiling, true
		}
	}
	return limit, capped
}
