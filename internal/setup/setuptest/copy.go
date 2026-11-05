package setuptest

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/crewlet/crewlet/internal/setup"
)

// The rules every vendor's words on a connect form are held to, written once
// so the probe suite here and setupapi's copy test agree on what a hint, a
// step and a Test result may say. A form that opened on a wall of text grew
// it one vendor at a time, so the limits are checked per vendor, not hoped
// for.

// MaxHintWords is the most words a field's hint says, its link's words
// included: one line under an input, read at a glance.
const MaxHintWords = 14

// MaxSummaryWords is the most words a Test result's first sentence says
// beside the button. The facts after it are notes.
const MaxSummaryWords = 20

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

// VerdictProblem is why v is not a Test result a person can read at a
// glance, "" when it is: a summary of one sentence (after an optional
// "Works."), short enough to sit beside the button, notes that are each a
// line, and none of it repeating a credential that was checked.
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
	for _, note := range v.Notes {
		if problem := LineProblem(note, "."); problem != "" {
			return "a note " + problem + ": " + note
		}
	}
	for _, secret := range secrets {
		if secret = strings.TrimSpace(secret); secret == "" {
			continue
		}
		for _, said := range append([]string{v.Summary}, v.Notes...) {
			if strings.Contains(said, secret) {
				return "it repeats the credential that was checked: " + said
			}
		}
	}
	return ""
}

// Said fails t when v breaks a rule of [VerdictProblem], for a vendor's own
// test of a verdict the shared clauses do not reach.
func Said(t TB, label string, v setup.Verdict, secrets ...string) {
	t.Helper()
	if problem := VerdictProblem(v, secrets...); problem != "" {
		t.Errorf("%s answered %s, and %s", label, v.Outcome, problem)
	}
}
