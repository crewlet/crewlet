package config

import (
	"bytes"
	"encoding/json"
	"errors"
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
// unmarshalers; see [phaseLLMFields] for why a distinct type is the way.
type tokenBudgetFields TokenBudget

// A custom unmarshaler is matched structurally, so a signature that drifts by
// one character would stop being called with nothing saying so.
var (
	_ yaml.Unmarshaler = (*TokenBudget)(nil)
	_ json.Unmarshaler = (*TokenBudget)(nil)
)

// errTokenBudgetShape is the refusal of a budget that is neither a mapping nor
// a number, in both encodings.
var errTokenBudgetShape = errors.New("token_budget must be a mapping of " +
	"ceilings per calendar window — `day`, `week` and `month`, each " +
	"optional — e.g. `token_budget: {day: 2000000}`")

// oneNumberRefusal is the refusal of `token_budget: <number>`, in both
// encodings: the author's own number in the example when it could be a
// ceiling, so the line to write is one they can paste — a 0 or a negative was
// never a ceiling, and echoing it would suggest one.
func oneNumberRefusal(value string) string {
	example := "40000000"
	if n, err := strconv.Atoi(value); err == nil && n >= 1 {
		example = strconv.Itoa(n)
	}
	return "token_budget is a ceiling per calendar window, not one number: " +
		"one number was a ceiling for the life of the deployment, which no " +
		"window is. Write it under the window it should reset on, e.g. " +
		"`token_budget: {month: " + example + "}` — `day`, `week` and `month` " +
		"are each optional, and a window with no key has no ceiling"
}

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
		return nodeFault(node, oneNumberRefusal(node.Value))
	default:
		return nodeFault(node, errTokenBudgetShape.Error())
	}
}

// UnmarshalJSON is [TokenBudget.UnmarshalYAML] for the JSON door: a document
// reaches the engine through it as well — a `PUT /config` body, a stored
// revision — and encoding/json's own word on
// `"token_budget": 10000000` names a Go type rather than the line to write.
//
// LENIENT ABOUT A KEY IT DOES NOT KNOW, where the YAML reader is strict, for
// [DecodeCompany]'s reason: a stored revision is read by peers on other
// builds, and refusing a window a newer build added would make a mixed fleet
// an outage in the older direction. Strictness is the import's, which reads
// YAML. A 0 decodes, as it does from YAML, and [Company.Validate] refuses it
// at the key.
func (b *TokenBudget) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	switch {
	case len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")):
		*b = TokenBudget{}
		return nil
	case trimmed[0] == '{':
		var fields tokenBudgetFields
		if err := json.Unmarshal(trimmed, &fields); err != nil {
			// NAMED AT THE KEY, and never at the decoding type: the
			// library's own word for `{"day": "many"}` names a struct
			// nobody wrote.
			var typed *json.UnmarshalTypeError
			if errors.As(err, &typed) && typed.Field != "" {
				return fmt.Errorf("token_budget.%s must be a whole number of "+
					"tokens, got a JSON %s", typed.Field, typed.Value)
			}
			return fmt.Errorf("token_budget: %w", err)
		}
		*b = TokenBudget(fields)
		return nil
	}
	var n json.Number
	if err := json.Unmarshal(trimmed, &n); err == nil {
		return errors.New(oneNumberRefusal(n.String()))
	}
	return errTokenBudgetShape
}

// validate refuses a ceiling below one token, at the key to remove.
//
// THE COMPANY'S OWN BLOCK ONLY, and through internal/org's one rule
// ([org.TokenCeilings.Faults]), which a seat's budget is held to as well — by
// [org.Role.Validate], which [Company.Validate] runs over every seat. A seat's
// block is not checked here too: it would report the same key twice, once in
// each package's words.
func (b TokenBudget) validate(path Path) error {
	var p problems
	for _, f := range b.Ceilings().Faults() {
		p.add(at(path, string(f.Window)), ErrOutOfRange, "%s", f.Error())
	}
	return p.err()
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

// IdleCeiling is one seat ceiling the company's own budget makes unable to
// refuse the seat a turn: everything a seat spends is the company's spend too,
// so a company ceiling at or below the seat's, on a window that holds the
// seat's whole ([holds]), is always reached first.
type IdleCeiling struct {
	// Window is the seat's window, and Ceiling its ceiling there.
	Window  period.Period
	Ceiling int

	// Over is the company window that is reached first, and Limit its
	// ceiling: the smallest company ceiling that idles this one when several
	// do, because that is the one an author has to get under for the seat's
	// key to do anything.
	Over  period.Period
	Limit int
}

// IdleUnder is every ceiling in seat that this budget — the company's — leaves
// idle, in [period.Periods] order.
//
// ONE JUDGEMENT, read by [Company.Warnings] over the document's own two
// ceilings, so a write that leaves a seat's ceiling idle is told so in the
// answer that stored it. Kept as a method on the company's budget so the
// boundary cases the rule is about — an equal ceiling, a day under a month, a
// week that straddles two months — are decided in one place.
//
// A ceiling below one on either side is not judged: validation owns a ceiling
// that cannot be met, and a warning beside that refusal would be advice about
// a key that has to go anyway.
func (b TokenBudget) IdleUnder(seat org.TokenCeilings) []IdleCeiling {
	company := b.Ceilings()
	var out []IdleCeiling
	for _, window := range period.Periods {
		ceiling, capped := seat[window]
		if !capped || ceiling < 1 {
			continue
		}
		var over period.Period
		limit := 0
		for _, outer := range period.Periods {
			o, held := company[outer]
			if !held || o < 1 || o > ceiling || !holds(outer, window) {
				continue
			}
			if limit == 0 || o < limit {
				over, limit = outer, o
			}
		}
		if limit == 0 {
			continue
		}
		out = append(out, IdleCeiling{Window: window, Ceiling: ceiling, Over: over, Limit: limit})
	}
	return out
}

// Sentence is what an author is told about an idle ceiling of the seat called
// handle: which company ceiling reaches first, and the two ways out.
func (i IdleCeiling) Sentence(handle string) string {
	reason := fmt.Sprintf("the company's (%d)", i.Limit)
	if i.Over != i.Window {
		reason = fmt.Sprintf("the company's token_budget.%s (%d), and a %s lies inside one %s",
			i.Over, i.Limit, i.Window, i.Over)
	}
	return fmt.Sprintf(
		"seat %q's token_budget.%s (%d) is at or above %s, so it can "+
			"never refuse this seat a turn: everything a seat spends is "+
			"the company's spend too, so the company's ceiling is reached "+
			"first. Remove it, or lower it below %d to hold this seat to "+
			"a share of the company's",
		handle, i.Window, i.Ceiling, reason, i.Limit)
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
	for role, path := range c.EachRole() {
		seat := role.Seat()
		if !seat.IsAgent() {
			continue
		}
		here := at(path, "token_budget")
		own := role.TokenBudget.warnings(here)
		for _, idle := range c.TokenBudget.IdleUnder(role.TokenBudget.Ceilings()) {
			own = append(own, advisory(at(here, string(idle.Window)), idle.Sentence(seat.Handle())))
		}
		for i := range own {
			own[i].Seat = seat.Handle()
		}
		out = append(out, own...)
	}
	return out
}
