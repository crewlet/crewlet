package advisor

import (
	"cmp"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// THE MODEL'S WORDS ARE CHECKED AGAINST THE ENGINE'S NUMBERS (ADR-0157): an
// explanation of a finding may cite only that finding's own numbers and
// records, and a report's summary only the report's numbers. Words that cite
// anything else are not kept, so no sentence a person reads says more than the
// records do. An explanation is bound to the verdict it was written for: the
// list hides it once the verdict moves, until a pass writes another.

// The bounds of the model's words.
const (
	// MaxExplained is how many findings one pass explains: the most severe
	// first, the rest on the next day's pass.
	MaxExplained = 20
	// MaxExplanationBytes and MaxSummaryBytes bound an explanation and a
	// report's summary; MaxExplanationSentences and MaxSummarySentences how
	// many sentences each says.
	MaxExplanationBytes     = 600
	MaxExplanationSentences = 3
	MaxSummaryBytes         = 1200
	MaxSummarySentences     = 5
)

// DropExplanation is an explanation the engine did not keep, counted with the
// pass's other drops.
const DropExplanation Drop = "explanation_ungrounded"

// Explainable is an open finding a pass is asked to explain: what it may
// cite, and the verdict an explanation is bound to.
type Explainable struct {
	ID            string   `json:"id"`
	Key           string   `json:"-"`
	VerdictDigest string   `json:"-"`
	Class         Class    `json:"class"`
	Severity      Severity `json:"severity"`
	Title         string   `json:"title"`
	Why           string   `json:"why"`
	Item          string   `json:"item"`
	Facts         []Fact   `json:"facts"`
	Records       []Record `json:"records"`
}

// Explained is an explanation the engine checked, for the verdict it names.
type Explained struct {
	ID, Key, VerdictDigest, Text string
}

// Unexplained is every open finding of stored whose verdict has no
// explanation yet, the most severe first and then the longest open, at most
// [MaxExplained].
func Unexplained(stored []Stored) []Explainable {
	open := slices.DeleteFunc(slices.Clone(stored), func(s Stored) bool {
		return s.State != StateOpen || s.VerdictDigest == "" || s.ExplainedDigest == s.VerdictDigest
	})
	slices.SortStableFunc(open, func(a, b Stored) int {
		return cmp.Or(cmp.Compare(a.Observation.Finding.Severity.rank(), b.Observation.Finding.Severity.rank()),
			a.OpenedAt.Compare(b.OpenedAt), cmp.Compare(a.ID, b.ID))
	})
	out := []Explainable{}
	for _, s := range open[:min(len(open), MaxExplained)] {
		f := s.Observation.Finding
		out = append(out, Explainable{ID: s.ID, Key: s.Observation.Key(), VerdictDigest: s.VerdictDigest,
			Class: f.Class, Severity: f.Severity, Title: f.Title, Why: f.Why, Item: f.Item,
			Facts: slices.Clone(f.Evidence), Records: slices.Clone(f.Records)})
	}
	return out
}

// ErrUngrounded is words that cite what the engine cannot vouch for.
var ErrUngrounded = errors.New("advisor: the words cite what the records do not show")

// GroundExplanation checks text as an explanation of e: one to three plain
// sentences whose every number is one of e's own (its facts, or a number its
// engine words already say) and whose every record is one e cites.
func GroundExplanation(text string, e Explainable) error {
	allowed := numbersOf(e.Title, e.Why, e.Item)
	for _, f := range e.Facts {
		allowed = append(allowed, spellings(f.Value)...)
	}
	records := make([]string, 0, len(e.Records))
	for _, r := range e.Records {
		records = append(records, r.ID)
	}
	return grounded(text, MaxExplanationBytes, MaxExplanationSentences, allowed, records)
}

// GroundSummary checks text as a summary of r: plain sentences whose every
// number is one the report states (its metrics and the week before's, its
// counts, how many findings it names) and whose every record is one of the
// findings it names.
func GroundSummary(text string, r Report) error {
	var allowed []string
	for _, m := range r.Metrics {
		allowed = append(allowed, spellings(m.Value)...)
		if m.Was != nil {
			allowed = append(allowed, spellings(*m.Was)...)
		}
	}
	for _, n := range []int{r.Opened, r.Resolved, r.Applied, r.Dismissed, len(r.Top)} {
		allowed = append(allowed, spellings(int64(n))...)
	}
	records := []string{}
	for _, f := range r.Top {
		records = append(records, f.ID)
		allowed = append(allowed, numbersOf(f.Title, f.Item)...)
	}
	return grounded(text, MaxSummaryBytes, MaxSummarySentences, allowed, records)
}

// recordShaped is a token that names a record: an item's key (ENG-42) or an
// engine id (a uuid, or a prefixed id such as turn-9).
var recordShaped = regexp.MustCompile(`\b(?:[A-Z][A-Z0-9]*-\d+|[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}|[a-z]+-\d+)\b`)

// numberShaped is a number as words say one: 7, 1,200, 26.5M, 812K, 40%.
var numberShaped = regexp.MustCompile(`\d[\d,]*(?:\.\d+)?[KM]?`)

// grounded is the check every model sentence passes.
func grounded(text string, maxBytes, maxSentences int, allowed, records []string) error {
	text = strings.TrimSpace(text)
	switch {
	case text == "" || len(text) > maxBytes:
		return fmt.Errorf("%w: it is empty or longer than %d bytes", ErrUngrounded, maxBytes)
	case strings.ContainsAny(text, "<>`*[]{}#|\n") || strings.Contains(text, "://"):
		return fmt.Errorf("%w: it carries markup or a link", ErrUngrounded)
	case sentences(text) > maxSentences:
		return fmt.Errorf("%w: it says more than %d sentences", ErrUngrounded, maxSentences)
	}
	for _, id := range recordShaped.FindAllString(text, -1) {
		if !slices.Contains(records, id) {
			return fmt.Errorf("%w: it cites %s, which it does not rest on", ErrUngrounded, id)
		}
	}
	rest := recordShaped.ReplaceAllString(text, " ")
	for _, n := range numberShaped.FindAllString(rest, -1) {
		if !slices.Contains(allowed, strings.TrimRight(n, ",")) {
			return fmt.Errorf("%w: it says %s, which no number it rests on is", ErrUngrounded, n)
		}
	}
	return nil
}

// sentences counts text's sentences by their ends.
func sentences(text string) int {
	n := 0
	for i, r := range text {
		if (r == '.' || r == '!' || r == '?') && (i+1 == len(text) || text[i+1] == ' ') {
			n++
		}
	}
	return max(n, 1)
}

// spellings is every way words say n: as digits, with thousands commas, and
// as a person reads a token count.
func spellings(n int64) []string {
	plain := fmt.Sprint(n)
	return []string{plain, withCommas(plain), tokensSaid(n)}
}

// withCommas is digits with a comma every three from the right.
func withCommas(digits string) string {
	sign := ""
	if strings.HasPrefix(digits, "-") {
		sign, digits = "-", digits[1:]
	}
	var b strings.Builder
	for i, d := range digits {
		if i > 0 && (len(digits)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(d)
	}
	return sign + b.String()
}

// numbersOf is every number the engine's own words say, which a sentence may repeat.
func numbersOf(texts ...string) []string {
	var out []string
	for _, t := range texts {
		for _, n := range numberShaped.FindAllString(recordShaped.ReplaceAllString(t, " "), -1) {
			out = append(out, strings.TrimRight(n, ","))
		}
	}
	return out
}
