package advisor

import (
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/config"
)

// A COST FIX ONLY ON A COST THE WORK DROVE (ADR-0166). A seat's token total
// counts the cache: a long context read again each call is mostly cache
// reads, which cost a fraction of fresh input. A budget or a cap cuts the
// cache reads with everything else, so an investigation's finding about a
// seat's tokens offers one only once the split it read shows fresh input or
// output drove the tokens (their rise, where it read the range before). Where
// cache reads drove them it says so and offers none, and where it read no
// split it offers none either.

// raises reports a budget or a cap fix that raises one the seat holds now.
func raises(c *config.Company, f SubmittedFix) bool {
	role, _ := roleOf(c, f.Seat)
	if role == nil {
		return false
	}
	switch f.Builder {
	case BuildTokenBudget:
		now := ceilingOf(role.TokenBudget, f.Period)
		return now > 0 && f.Value > now
	case BuildRoundCap:
		now := roundCapOf(c, role)
		return now > 0 && (f.Value == 0 || f.Value > now)
	}
	return false
}

// tokenSplit is a finding's token numbers by part, read off where each was
// cited: input (which includes the cache reads), the cache reads, output, and
// each one's range before where the finding read it.
type tokenSplit struct {
	input, cache, output          int64
	hasInput, hasCache, hasOutput bool
	inputWas, cacheWas, outputWas *int64
}

// splitOf is the token split s cites, its facts as grounded in facts.
func splitOf(s Submitted, facts []Fact) tokenSplit {
	var out tokenSplit
	for i, c := range s.Facts {
		if i >= len(facts) {
			break
		}
		switch last := c.From[strings.LastIndex(c.From, ".")+1:]; {
		case strings.Contains(last, "cache_read"):
			out.cache, out.hasCache, out.cacheWas = c.Value, true, facts[i].Was
		case strings.Contains(last, "input_tokens"):
			out.input, out.hasInput, out.inputWas = c.Value, true, facts[i].Was
		case strings.Contains(last, "output_tokens"):
			out.output, out.hasOutput, out.outputWas = c.Value, true, facts[i].Was
		}
	}
	return out
}

// aboutTokens reports a finding that cites a token number, or offers a budget.
func aboutTokens(s Submitted) bool {
	if s.Fix.Builder == BuildTokenBudget {
		return true
	}
	for _, c := range s.Facts {
		if last := c.From[strings.LastIndex(c.From, ".")+1:]; c.Unit == UnitTokens || strings.Contains(last, "token") {
			return true
		}
	}
	return false
}

// freshDrove reports whether fresh input or output drove the tokens rather
// than cache reads: their rise over the range before where the split names
// one for both input and the cache, else their share now.
func (t tokenSplit) freshDrove() bool {
	if t.inputWas != nil && t.cacheWas != nil {
		cache := t.cache - *t.cacheWas
		fresh := (t.input - *t.inputWas) - cache
		if t.outputWas != nil {
			fresh += t.output - *t.outputWas
		}
		return fresh > cache
	}
	return t.input-t.cache+t.output > t.cache
}

// costGate is fix as an investigation may offer it for s: a budget or a cap
// that cuts a seat's tokens stands only where the split shows fresh input or
// output drove them; otherwise the finding offers nothing, saying why. One
// that raises a budget or a cap the seat's work outgrew cuts nothing, and
// stands as it is.
func costGate(c *config.Company, s Submitted, facts []Fact, fix Fix) Fix {
	if (s.Fix.Builder != BuildTokenBudget && s.Fix.Builder != BuildRoundCap) || !aboutTokens(s) ||
		raises(c, s.Fix) {
		return fix
	}
	none := Fix{Kind: FixNone, Options: []Option{}}
	t := splitOf(s, facts)
	switch {
	case !t.hasInput || !t.hasCache:
		none.Sentence = "No budget or cap is offered: the tokens were not split into fresh input, cache reads " +
			"and output, so what drove them is not known."
	case !t.freshDrove():
		none.Sentence = fmt.Sprintf("No budget or cap is offered: cache reads drove these tokens (%s of %s "+
			"input), which cost a fraction of fresh input, so a budget would stop the work without cutting "+
			"much of what it costs. A shorter context, or reading less again, cuts them.",
			tokensSaid(t.cache), tokensSaid(t.input))
	default:
		return fix
	}
	return none
}
