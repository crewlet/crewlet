package setuptest

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/crewlet/crewlet/internal/setup"
)

// The rules every vendor's connect-form words are held to, shared by this
// probe suite and setupapi's copy test.

// MaxHintWords is the most words a field's hint says, its link's words
// included: one line under an input, read at a glance.
const MaxHintWords = 14

// MaxSummaryWords is the most words a Test result's first sentence says
// beside the button. What it found goes in its facts and warnings.
const MaxSummaryWords = 20

// MaxFactLabelWords is the most words a fact's label says: it is a term beside
// a value, read down a column.
const MaxFactLabelWords = 4

// markdownLink is `[words](address)`, the inline link a hint may carry.
var markdownLink = regexp.MustCompile(`\[([^\]]+)\]\([^)]+\)`)

// Visible is text as a person reads it: an inline link as its words, a
// [setup.LinkMark] as linkText, and code spans without their backticks.
func Visible(text, linkText string) string {
	text = markdownLink.ReplaceAllString(text, "$1")
	text = strings.ReplaceAll(text, setup.LinkMark, linkText)
	return strings.ReplaceAll(text, "`", "")
}

// Words counts the words a person reads in text.
func Words(text, linkText string) int {
	return len(strings.Fields(Visible(text, linkText)))
}

// LineProblem is why text is not one line of finished copy ending on end, ""
// when it is: empty, a line break, a dash standing in for punctuation, or a
// different last character.
func LineProblem(text, end string) string {
	switch {
	case strings.TrimSpace(text) == "":
		return "it is empty"
	case text != strings.TrimSpace(text):
		return "it has space around it"
	case strings.ContainsAny(text, "\r\n"):
		return "it runs over more than one line"
	case strings.ContainsRune(text, '\u2014'), strings.Contains(text, " - "), strings.Contains(text, "--"):
		return "it carries a dash where a comma, a colon or a full stop belongs"
	case !strings.HasSuffix(text, end):
		return "it does not end with " + end
	}
	return ""
}

// VerdictProblem is why v is not a glanceable Test result, "" when it is: one
// short summary sentence (after an optional "Works."), facts that are a term
// and a value, warnings that each say what to do, and no credential that was
// checked anywhere in it.
func VerdictProblem(v setup.Verdict, secrets ...string) string {
	if problem := LineProblem(v.Summary, "."); problem != "" {
		return "the summary " + problem + ": " + v.Summary
	}
	if strings.Contains(strings.TrimPrefix(v.Summary, "Works. "), ". ") {
		return "the summary says more than one sentence: " + v.Summary
	}
	if n := Words(v.Summary, ""); n > MaxSummaryWords {
		return "the summary runs to " + strconv.Itoa(n) + " words: " + v.Summary
	}
	said := []string{v.Summary}
	for _, fact := range v.Facts {
		if problem := factProblem(fact); problem != "" {
			return "a fact " + problem + ": " + fact.Label + ", " + fact.Value
		}
		said = append(said, fact.Label, fact.Value)
	}
	for _, warning := range v.Warnings {
		if problem := warningProblem(v.Outcome, warning); problem != "" {
			return "a warning " + problem + ": " + warning.Title + " " + warning.Action
		}
		said = append(said, warning.Title, warning.Action)
	}
	for _, secret := range secrets {
		if secret = strings.TrimSpace(secret); secret == "" {
			continue
		}
		for _, line := range said {
			if strings.Contains(line, secret) {
				return "it repeats the credential that was checked: " + line
			}
		}
	}
	return ""
}

// factProblem is why a fact is not a term and its value, "" when it is.
func factProblem(fact setup.Fact) string {
	if problem := LineProblem(fact.Label, ""); problem != "" {
		return "label " + problem
	}
	if n := Words(fact.Label, ""); n > MaxFactLabelWords {
		return "label runs to " + strconv.Itoa(n) + " words"
	}
	if strings.ContainsAny(fact.Label[len(fact.Label)-1:], ":.") {
		return "label carries its own punctuation"
	}
	if problem := LineProblem(fact.Value, ""); problem != "" {
		return "value " + problem
	}
	if strings.HasSuffix(fact.Value, ".") {
		return "value is a sentence rather than a value"
	}
	return ""
}

// warningProblem is why a warning does not say what is wrong and what to do,
// "" when it does. Only a failed check may leave its title to the summary: a
// working credential's summary does not say what the risk is.
func warningProblem(outcome setup.VerifyOutcome, warning setup.Warning) string {
	if problem := LineProblem(warning.Action, "."); problem != "" {
		return "action " + problem
	}
	if warning.Title == "" {
		if outcome == setup.VerifyWorks {
			return "on a working credential names no risk"
		}
		return ""
	}
	if problem := LineProblem(warning.Title, "."); problem != "" {
		return "title " + problem
	}
	return ""
}

// Spoken is everything v tells a person, in reading order: its summary, each
// fact as "label: value" and each warning as its title and action, so a test
// asserts on what is read rather than on which list carries it.
func Spoken(v setup.Verdict) string {
	said := []string{v.Summary}
	for _, fact := range v.Facts {
		said = append(said, fact.Label+": "+fact.Value)
	}
	for _, warning := range v.Warnings {
		said = append(said, strings.TrimSpace(warning.Title+" "+warning.Action))
	}
	return strings.Join(said, " | ")
}

// Said fails t when v breaks a rule of [VerdictProblem], for a vendor's own
// test of a verdict the shared clauses do not reach.
func Said(t TB, label string, v setup.Verdict, secrets ...string) {
	t.Helper()
	if problem := VerdictProblem(v, secrets...); problem != "" {
		t.Errorf("%s answered %s, and %s", label, v.Outcome, problem)
	}
}
