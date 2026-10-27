// Package caveattest is what a person's words may never carry, as one check
// shared by every walk over a sentence a person can be shown: the caveat
// catalog's, the refusal catalog's and the act route's own (ADR-0060).
//
// ONE STATEMENT OF THE VOCABULARY, so the walks cannot come to disagree about
// what reads as a model's: a pattern added for one is a pattern every walk
// holds.
package caveattest

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/agent/builtin"
)

// modelVocabulary is each mark of a model's (or a wire protocol's) vocabulary,
// with why a person sees no control for it.
var modelVocabulary = []struct {
	pattern *regexp.Regexp
	what    string
}{
	// `default_assignee`, `if_match`, `op_id`, and every builtin tool's
	// name: an argument or a tool is spelled in snake case, and nothing a
	// person reads on a screen is.
	{regexp.MustCompile(`\b[a-z0-9]+(?:_[a-z0-9]+)+\b`), "a snake_case argument or tool name"},
	// `ask: ana`, `labels: [...]`: an argument as a model writes it.
	{regexp.MustCompile(`\b[a-z]+:\s`), "an argument written as `name: value`"},
	// The mark a tool's sentence puts around a value it names.
	{regexp.MustCompile("`"), "a backticked argument"},
	// A row's internal id, which no screen shows.
	{regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`), "an internal id"},
	// Process vocabulary a model is told about and a person never is.
	{regexp.MustCompile(`(?i)\b(operation|node|writer|tool|argument|call)\b`), "engine-internal vocabulary"},
}

// toolNames is every builtin tool a person could be told to use by name.
var toolNames = slices.Concat(builtin.WorkTools(), builtin.PageTools(),
	builtin.ArtifactTools(), builtin.ArtifactDataTools())

// Problems is every reason text is not worded for a person, or none. It names
// no tool, no argument, no internal id and no engine vocabulary, carries no em
// dash, and ends as a sentence.
func Problems(text string) []string {
	var out []string
	if len(toolNames) == 0 {
		return []string{"no builtin tool names to hold the text against"}
	}
	if strings.TrimSpace(text) == "" {
		return []string{"it is empty"}
	}
	if strings.ContainsRune(text, '\u2014') {
		out = append(out, "it carries an em dash")
	}
	if !strings.HasSuffix(strings.TrimSpace(text), ".") {
		out = append(out, "it is not a sentence")
	}
	for _, tool := range toolNames {
		if strings.Contains(text, tool) {
			out = append(out, fmt.Sprintf("it names the tool %s", tool))
		}
	}
	for _, banned := range modelVocabulary {
		if found := banned.pattern.FindString(text); found != "" {
			out = append(out, fmt.Sprintf("it says %q, %s", found, banned.what))
		}
	}
	return out
}

// valueFields are the keys of a write's answer, at any depth, whose strings are
// values a screen reads (ids, keys, positions, codes, enumerations, the
// grants and the fields a refusal names) and never sentences it draws.
var valueFields = []string{"tool", "outcome", "position", "op_id", "id", "item", "key",
	"status", "rank", "code", "action", "type", "project", "assignee", "labels_created",
	"error", "reason", "grants", "field", "fields", "window"}

// AnswerProblems is every sentence a write's decoded answer carries that is not
// worded for a person, each named by its path: every string at any depth
// outside the value fields, so a receipt field a tool fills with a model's
// sentence is found whatever it is called.
func AnswerProblems(answer any) []string {
	var out []string
	sentences("", answer, func(path, text string) {
		for _, problem := range Problems(text) {
			out = append(out, fmt.Sprintf("%s is not worded for a person: %s: %q",
				path, problem, text))
		}
	})
	return out
}

// sentences calls check with every string value carries outside the value
// fields, at any depth, named by its path.
func sentences(path string, value any, check func(path, text string)) {
	switch v := value.(type) {
	case string:
		check(path, v)
	case []any:
		for i, item := range v {
			sentences(fmt.Sprintf("%s[%d]", path, i), item, check)
		}
	case map[string]any:
		for key, item := range v {
			if slices.Contains(valueFields, key) {
				continue
			}
			sentences(path+"."+key, item, check)
		}
	}
}
