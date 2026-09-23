package config

import (
	"fmt"
	"strconv"

	"gopkg.in/yaml.v3"

	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/period"
)

// TokenBudget is the AUTHORED form of a token budget: a `token_budget:`
// mapping that names a ceiling for each calendar window it caps, on the
// company and on any agent seat.
//
//	token_budget:
//	  day: 2000000      # one local day, midnight to midnight
//	  month: 40000000   # one calendar month
//
// # A budget is a ceiling per window, not one number
//
// A single number was a ceiling for the life of the deployment, and a ceiling
// nothing ever resets is one a company only escapes by an operator zeroing a
// counter by hand. A company's spend has a rhythm — a bill per month, a
// runaway loop that burns a day's allowance in an hour — so a budget names
// the windows it bounds, and each one opens again on its own when the window
// turns over: a day at the company's local midnight, a week at the start of
// its ISO week (Monday), a month on the 1st, all on the company's one clock
// (`timezone`, ADR-0018). Every key is optional and the windows are
// independent: a turn is admitted only while every capped window has room.
//
// # An absent key is no ceiling, and 0 is refused
//
// The pointers are what tell the two apart, and the rule they serve is the
// engine's for every setting whose zero is a real value: zero must mean one
// thing or be refused. A ceiling of 0 read as "unlimited" is what the single
// number used to mean, and read as "nothing may be spent" is what the word
// ceiling says — two readers, two opposite companies. So there is exactly one
// way to leave a window uncapped, which is to leave its key out, and a 0 or a
// negative is refused naming the key to remove. Stopping a seat on purpose is
// not a budget's job at all.
//
// # What the engine reads
//
// [TokenBudget.Ceilings]: one period to one number, capped windows only. The
// org model carries that ([org.TokenCeilings]), so nothing downstream of the
// configuration ever sees a pointer.
type TokenBudget struct {
	Day   *int `yaml:"day,omitempty" json:"day,omitempty" js:"min=1" desc:"Most tokens one day may spend, local midnight to midnight on the company clock. Absent = no daily ceiling; 0 is refused."`
	Week  *int `yaml:"week,omitempty" json:"week,omitempty" js:"min=1" desc:"Most tokens one ISO week (from Monday, on the company clock) may spend. Absent = no weekly ceiling; 0 is refused."`
	Month *int `yaml:"month,omitempty" json:"month,omitempty" js:"min=1" desc:"Most tokens one calendar month (on the company clock) may spend. Absent = no monthly ceiling; 0 is refused."`
}

// byPeriod is the authored ceilings in [period.Periods] order.
//
// AN ARRAY SIZED BY THE PERIOD LIST, so a fourth period added there stops
// this compiling until it has a key here, rather than becoming a window every
// company silently leaves uncapped because no document can name it.
func (b TokenBudget) byPeriod() [len(period.Periods)]*int {
	return [...]*int{b.Day, b.Week, b.Month}
}

// Ceilings is the budget as the engine reads it: each capped window's
// ceiling, keyed by period. A window the document leaves out is absent, and a
// budget that caps nothing is nil.
//
// It returns what was written, a refused value included: [Company.Validate]
// is what stands between a 0 and a running company, and a Ceilings that
// quietly dropped one would turn a refused document into an uncapped one
// wherever a caller built the org without validating.
func (b TokenBudget) Ceilings() org.TokenCeilings {
	var out org.TokenCeilings
	for i, ceiling := range b.byPeriod() {
		if ceiling == nil {
			continue
		}
		if out == nil {
			out = org.TokenCeilings{}
		}
		out[period.Periods[i]] = *ceiling
	}
	return out
}

// tokenBudgetFields decodes the mapping without re-entering TokenBudget's own
// unmarshaler; see [phaseLLMFields] for why a distinct type is the way.
type tokenBudgetFields TokenBudget

// A custom unmarshaler is matched structurally, so a signature that drifts by
// one character would stop being called with nothing saying so.
var _ yaml.Unmarshaler = (*TokenBudget)(nil)

// UnmarshalYAML accepts the mapping, through the strict decoder, and refuses
// every other shape by what the author should write instead.
//
// It EXISTS FOR THE REFUSAL. The accepted shape is exactly the struct's, which
// is why the schema needs no hand-written fragment for it; what yaml's own
// decoder would say about `token_budget: 10000000` — the form every example
// company and the quickstart used to show — names a Go type, and the person
// reading it needs the line to write instead.
func (b *TokenBudget) UnmarshalYAML(node *yaml.Node) error {
	for node.Kind == yaml.AliasNode && node.Alias != nil {
		node = node.Alias
	}
	switch {
	case node.Tag == "!!null":
		*b = TokenBudget{}
		return nil
	case node.Kind == yaml.MappingNode:
		var fields tokenBudgetFields
		// Strictly: `token_budget: {dya: 5}` must not read as a budget
		// that caps nothing.
		if err := decodeKnown(node, &fields); err != nil {
			return err
		}
		*b = TokenBudget(fields)
		return nil
	case node.Kind == yaml.ScalarNode && node.ShortTag() == "!!int":
		// The author's own number in the example when it could be a
		// ceiling, so the line to write is one they can paste; a 0 or a
		// negative was never a ceiling, and echoing it would suggest one.
		example := "40000000"
		if n, err := strconv.Atoi(node.Value); err == nil && n >= 1 {
			example = strconv.Itoa(n)
		}
		return nodeFault(node, "token_budget is a ceiling per calendar "+
			"window, not one number: one number was a ceiling for the life "+
			"of the deployment, which no window is. Write it under the "+
			"window it should reset on, e.g. `token_budget: {month: "+
			example+"}` — `day`, `week` and `month` are each optional, and "+
			"a window with no key has no ceiling")
	default:
		return nodeFault(node, "token_budget must be a mapping of ceilings "+
			"per calendar window — `day`, `week` and `month`, each optional — "+
			"e.g. `token_budget: {day: 2000000}`")
	}
}

// validate refuses a ceiling below one token, at the key to remove.
func (b TokenBudget) validate(path Path) error {
	var p problems
	for i, ceiling := range b.byPeriod() {
		if ceiling == nil || *ceiling >= 1 {
			continue
		}
		window := period.Periods[i]
		p.add(at(path, string(window)), ErrOutOfRange,
			"must be at least 1 token, got %d: a window is left without a "+
				"ceiling by leaving its key out, never by writing 0 — remove "+
				"`token_budget.%s` for no %s ceiling",
			*ceiling, window, adjective(window))
	}
	return p.err()
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
	return string(p)
}

// windowPairs is every pair of windows one scope can cap together, shorter
// first, with what bounds the longer window's spend in terms of the shorter.
//
// `most` is the largest number of shorter windows one longer window can
// overlap: a week is seven days, the longest month is 31, and a month
// overlaps at most six ISO weeks (a 30-day month that begins on a Sunday is a
// one-day week, four whole weeks and another one-day week). `inside`
// says whether every shorter window lies wholly inside one longer window — a
// day always does, and a week does not, because a week can straddle two
// months.
var windowPairs = []struct {
	short, long period.Period
	most        int
	spans       string
	inside      bool
}{
	{period.Day, period.Week, 7, "a week is 7 days", true},
	{period.Day, period.Month, 31, "the longest month is 31 days", true},
	{period.Week, period.Month, 6, "a month overlaps at most 6 ISO weeks", false},
}

// warnings is every ceiling in this one budget that another ceiling in it
// makes unable to refuse a turn, located at the key that does nothing.
//
// PAIRWISE, and each warning names the one pair it is about, because what an
// author does next is decided by that pair: lower the idle ceiling, or remove
// it. A ceiling made idle only by two others at once is not reported.
func (b TokenBudget) warnings(path Path) []Warning {
	ceilings := b.Ceilings()
	var out []Warning
	for _, pair := range windowPairs {
		short, hasShort := ceilings[pair.short]
		long, hasLong := ceilings[pair.long]
		if !hasShort || !hasLong || short < 1 || long < 1 {
			// Refused, or not a pair: validate owns a ceiling below one.
			continue
		}
		switch {
		case short >= long && pair.inside:
			out = append(out, advisory(at(path, string(pair.short)), fmt.Sprintf(
				"token_budget.%s (%d) is at or above token_budget.%s (%d), so it "+
					"can never refuse a turn: a %s lies inside one %s, whose "+
					"ceiling is reached first. Remove it, or lower it below %d to "+
					"hold one %s to a share of the %s",
				pair.short, short, pair.long, long, pair.short, pair.long, long,
				pair.short, pair.long)))
		case short >= long:
			out = append(out, advisory(at(path, string(pair.short)), fmt.Sprintf(
				"token_budget.%s (%d) is at or above token_budget.%s (%d), so it "+
					"can refuse a turn only in a %s that straddles two %ss: "+
					"inside one %s, the %s's ceiling is reached first. Lower it "+
					"below %d, or check the two are not swapped",
				pair.short, short, pair.long, long, pair.short, pair.long,
				pair.long, pair.long, long)))
		// Divided rather than multiplied, so a ceiling near the int
		// range cannot overflow into a warning that is not true:
		// most × short <= long exactly when short <= long / most.
		case short <= long/pair.most:
			full := pair.most * short
			out = append(out, advisory(at(path, string(pair.long)), fmt.Sprintf(
				"token_budget.%s (%d) can never refuse a turn: %s, and %d at "+
					"token_budget.%s (%d) is %d, which it does not exceed. Remove "+
					"it, or lower it below %d to hold a %s to less than %d full %ss",
				pair.long, long, pair.spans, pair.most, pair.short, short, full,
				full, pair.long, pair.most, pair.short)))
		}
	}
	return out
}

// holds reports whether every window of period inner lies wholly inside one
// window of period outer: a window holds itself, and a day lies inside one
// week and one month. A week does not lie inside one month, because a week
// can straddle two.
func holds(outer, inner period.Period) bool {
	if outer == inner {
		return true
	}
	for _, pair := range windowPairs {
		if pair.short == inner && pair.long == outer {
			return pair.inside
		}
	}
	return false
}

// budgetWarnings is every ceiling in the company that can never refuse a
// turn: one another ceiling in the same budget always reaches first (see
// [TokenBudget.warnings]), and a seat's ceiling at or above a company ceiling
// on a window that holds the seat's whole ([holds]) — which the company's
// reaches first, because everything a seat spends is the company's spend too.
//
// Warnings rather than refusals, because every one of them runs exactly as
// written and none of them is dangerous: an idle ceiling admits nothing a
// working one would refuse. What it does is mislead — a founder reading
// "this seat may spend 5M a day" beside a company capped at 2M believes the
// seat has a headroom it does not — and that is what a warning is for.
//
// Agent seats only. A human seat carrying a budget is refused as a conflict,
// and a warning beside that refusal would be advice about a key that has to
// go anyway.
func (c *Company) budgetWarnings() []Warning {
	out := c.TokenBudget.warnings(field("token_budget"))
	company := c.TokenBudget.Ceilings()
	for role, path := range c.EachRole() {
		seat := role.Seat()
		if !seat.IsAgent() {
			continue
		}
		here := at(path, "token_budget")
		own := role.TokenBudget.warnings(here)
		for i, ceiling := range role.TokenBudget.byPeriod() {
			window := period.Periods[i]
			if ceiling == nil || *ceiling < 1 {
				continue
			}
			// The company ceiling that leaves this one idle, and the
			// smallest of them when several do: that is the one an author
			// has to get under for this key to do anything.
			var over period.Period
			limit := 0
			for _, outer := range period.Periods {
				o, capped := company[outer]
				if !capped || o < 1 || o > *ceiling || !holds(outer, window) {
					continue
				}
				if limit == 0 || o < limit {
					over, limit = outer, o
				}
			}
			if limit == 0 {
				continue
			}
			reason := fmt.Sprintf("the company's (%d)", limit)
			if over != window {
				reason = fmt.Sprintf("the company's token_budget.%s (%d), and a %s lies inside one %s",
					over, limit, window, over)
			}
			own = append(own, advisory(at(here, string(window)), fmt.Sprintf(
				"seat %q's token_budget.%s (%d) is at or above %s, so it can "+
					"never refuse this seat a turn: everything a seat spends is "+
					"the company's spend too, so the company's ceiling is reached "+
					"first. Remove it, or lower it below %d to hold this seat to "+
					"a share of the company's",
				seat.Handle(), window, *ceiling, reason, limit)))
		}
		for i := range own {
			own[i].Seat = seat.Handle()
		}
		out = append(out, own...)
	}
	return out
}
