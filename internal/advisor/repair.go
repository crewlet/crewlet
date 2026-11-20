package advisor

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/config"
)

// WHAT THE MODEL FOUND REACHES THE PAGE (ADR-0166). A finding the model
// proposes stands on its numbers: each is checked against what it read, and a
// number not there drops it. Its words and its fix are another matter. A fix
// the engine cannot build leaves the finding listed with no fix, and a plain
// line saying what a person would do. Words the wording rule refuses are
// mended: the deep run is told the rule's reason in the same run and rewrites
// them (a bounded number of times), and what still breaks the rule is said by
// the engine from the finding's own facts. Each finding listed after a repair
// is counted by the repair, as each one dropped is by why.

// Repair is how a submitted finding was mended to be listed.
type Repair string

// The repairs.
const (
	// RepairNoFix is a finding listed without the fix the model asked for,
	// which the engine could not build: a plain line says what to do.
	RepairNoFix Repair = "fix_not_built"
	// RepairReworded is a finding the model rewrote in the same run, told
	// why the wording rule refused its words.
	RepairReworded Repair = "reworded"
	// RepairEngineWords is a finding whose words the rule still refused,
	// said by the engine from its own facts.
	RepairEngineWords Repair = "worded_by_engine"
	// RepairSubject is a finding about a seat the chart does not hold, said
	// about the company.
	RepairSubject Repair = "subject_widened"
)

// Repairs is every repair, as a run's record counts them.
func Repairs() []Repair {
	return []Repair{RepairNoFix, RepairReworded, RepairEngineWords, RepairSubject}
}

// wordsProblem is why s breaks the wording rule for a line of at most limit
// characters, "" where it does not.
func wordsProblem(s string, limit int) string {
	switch {
	case utf8.RuneCountInString(s) > limit:
		return fmt.Sprintf("is longer than %d characters", limit)
	case strings.Contains(s, "://"):
		return "holds a link"
	case strings.ContainsAny(s, "<>`[]"):
		return "holds markup (<, >, `, [ or ])"
	case strings.ContainsFunc(s, func(r rune) bool { return r < ' ' }):
		return "holds a line break or a control character"
	}
	return ""
}

// Precheck is every way sub breaks the wording rule, each in words a model
// rewrites it by ("title is longer than 120 characters"): what the deep run is
// told before its findings are taken, so it mends them itself.
func Precheck(sub Submitted, g Grounding) []string {
	var out []string
	for _, w := range []struct {
		field, text string
		limit       int
	}{{"headline", sub.Headline, MaxAnalystTitle}, {"title", sub.Title, MaxAnalystTitle},
		{"why", sub.Why, MaxAnalystWhy}, {"item", sub.Item, MaxAnalystTitle}} {
		if p := wordsProblem(w.text, w.limit); p != "" {
			out = append(out, w.field+" "+p)
		}
	}
	if strings.TrimSpace(sub.Title) == "" {
		out = append(out, "title is empty")
	}
	if sub.Severity != SeverityCosts && sub.Severity != SeverityNote {
		out = append(out, "severity is costs or note")
	}
	for _, c := range sub.Facts {
		switch {
		case strings.TrimSpace(c.Label) == "":
			out = append(out, fmt.Sprintf("the fact %s has no label", c.Key))
		case wordsProblem(c.Label, MaxAnalystFactLabel) != "":
			out = append(out, fmt.Sprintf("the fact %s's label %s", c.Key, wordsProblem(c.Label, MaxAnalystFactLabel)))
		}
		if (g.Units && c.Unit == "") || (c.Unit != "" && !c.Unit.Valid()) {
			out = append(out, fmt.Sprintf("the fact %s says no unit of %s", c.Key, unitsSaid()))
		}
	}
	if g.Headlines && sub.Headline != "" {
		values := make([]int64, 0, len(sub.Facts))
		for _, c := range sub.Facts {
			values = append(values, c.Value)
		}
		if n, ok := headlineStray(sub.Headline, values); ok {
			out = append(out, fmt.Sprintf("headline says %s, which no fact cites", n))
		}
	}
	return out
}

// unitsSaid is every unit, as a model is told them.
func unitsSaid() string {
	words := make([]string, 0, len(Units()))
	for _, u := range Units() {
		words = append(words, string(u))
	}
	return strings.Join(words, ", ")
}

// plainSaid is s as the wording rule keeps it: control characters as spaces,
// no markup, no word holding an address, and at most limit characters, cut at
// a word with an ellipsis.
func plainSaid(s string, limit int) string {
	mapped := strings.Map(func(r rune) rune {
		switch {
		case r < ' ':
			return ' '
		case strings.ContainsRune("<>`[]", r):
			return -1
		}
		return r
	}, s)
	var kept []string
	for _, w := range strings.Fields(mapped) {
		if !strings.Contains(w, "://") {
			kept = append(kept, w)
		}
	}
	out := strings.Join(kept, " ")
	if utf8.RuneCountInString(out) <= limit {
		return out
	}
	runes := []rune(out)[:limit-1]
	if at := strings.LastIndex(string(runes), " "); at > limit/2 {
		return strings.TrimRight(string(runes)[:at], " ,;:") + "…"
	}
	return string(runes) + "…"
}

// unitOf is the unit a fact counts, read off its path and label where the
// model said none: tokens, turns, hours, minutes, items, a share, or a count.
func unitOf(c CitedFact) Unit {
	words := strings.ToLower(c.From + " " + c.Label + " " + c.Key)
	for _, u := range []struct {
		unit  Unit
		words []string
	}{{UnitTokens, []string{"token"}}, {UnitTurns, []string{"turn"}}, {UnitMinutes, []string{"minute"}},
		{UnitHours, []string{"hour"}}, {UnitPercent, []string{"percent", "share", "%"}},
		{UnitBytes, []string{"bytes", "memory"}}, {UnitItems, []string{"item", "task"}}} {
		if slices.ContainsFunc(u.words, func(w string) bool { return strings.Contains(words, w) }) {
			return u.unit
		}
	}
	return UnitCount
}

// labelOf is a fact's key as words, for a fact the model gave no label.
func labelOf(key string) string {
	s := strings.TrimSpace(strings.NewReplacer("_", " ", "-", " ", ".", " ").Replace(key))
	if s == "" {
		return "Number"
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// headlineFrom is a headline the engine says from facts: its first fact's
// label and number, which the facts cite.
func headlineFrom(facts []Fact) string {
	if len(facts) == 0 {
		return ""
	}
	f := facts[0]
	n := groupedInt(f.Value)
	if f.Counts() == UnitTokens {
		n = tokensSaid(f.Value)
	}
	return plainSaid(fmt.Sprintf("%s: %s.", f.Said(), n), MaxAnalystTitle)
}

// fixWords is the plain line a finding says where the engine could not build
// the fix its model asked for: what a person would do instead.
func fixWords(c *config.Company, f SubmittedFix) string {
	seat := "the seat"
	if role, _ := roleOf(c, f.Seat); role != nil && role.Name != "" {
		seat = role.Name
	} else if f.Seat != "" {
		seat = f.Seat
	}
	switch f.Builder {
	case BuildRoundCap:
		return fmt.Sprintf("No change could be built: check %s's round cap (sandbox.max_turns) on its page.", seat)
	case BuildTokenBudget:
		return fmt.Sprintf("No change could be built: check %s's token budget on its page.", seat)
	case BuildAuxiliaryModel:
		return fmt.Sprintf("No change could be built: point %s's learning (llm_auxiliary) at a smaller model "+
			"in its settings.", seat)
	case BuildOpenItem:
		if f.Target != "" {
			return fmt.Sprintf("Open %s and decide what it needs.", plainSaid(f.Target, 40))
		}
	case BuildOpenSeat:
		if f.Target != "" {
			return fmt.Sprintf("Open %s's page and decide what it needs.", plainSaid(f.Target, 40))
		}
	}
	return "Nothing to send: read its numbers and the records it names, and decide what to change."
}

// HeadlineGrounded reports whether every number headline says is one of
// values, a day of a month aside.
func HeadlineGrounded(headline string, values []int64) bool {
	_, stray := headlineStray(headline, values)
	return !stray
}

// headlineStray is the first number headline says that values do not hold,
// as written, and whether there is one.
func headlineStray(headline string, values []int64) (string, bool) {
	for _, m := range headlineNumber().FindAllStringSubmatchIndex(headline, -1) {
		said := headline[m[2]:m[3]]
		n, err := strconv.ParseInt(strings.ReplaceAll(said, ",", ""), 10, 64)
		if err != nil {
			return said, true
		}
		if rest := strings.TrimSpace(headline[m[1]:]); n >= 1 && n <= 31 && monthAhead(rest) {
			continue
		}
		if !slices.Contains(values, n) {
			return said, true
		}
	}
	return "", false
}

// headlineNumber is a number as a headline says it, thousands grouped or not.
// Compiled at first use, not at init: Gitlet's push hook is this binary.
var headlineNumber = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`\b(\d[\d,]*)\b`)
})

// monthAhead reports whether s starts with a month's name or its first three letters.
func monthAhead(s string) bool {
	for _, m := range []string{"Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"} {
		if strings.HasPrefix(s, m) {
			return true
		}
	}
	return false
}
