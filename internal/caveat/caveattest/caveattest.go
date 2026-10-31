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
	"github.com/crewlet/crewlet/internal/chatlet"
	"github.com/crewlet/crewlet/internal/provenance"
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

// toolNames is every builtin tool a person could be told to use by name, each
// app's, chat's included: the tools a seat holds there and the gestures only
// a person is given.
var toolNames = slices.Concat(builtin.WorkTools(), builtin.PageTools(),
	builtin.ArtifactTools(), builtin.ArtifactDataTools(), chatlet.Tools(),
	chatlet.OperatorOnlyTools())

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
// values a screen reads (ids, keys, handles, positions, codes, enumerations,
// addresses, the titles and paths people gave things, the grants and the
// fields a refusal names) and never sentences it draws. DENY BY DEFAULT: a key
// that is not listed here is held to a person's words, so a new key of prose
// fails every walk until somebody decides it is a value and lists it.
var valueFields = []string{"tool", "outcome", "position", "op_id", "id", "item", "key",
	"status", "rank", "code", "action", "type", "project", "assignee", "labels_created",
	"error", "reason", "grants", "field", "fields", "window",
	"comment_id", "page", "merged_into", "moved_from", "artifact", "url", "write",
	"handle", "title", "withdrawn_from", "verdict", "container", "content_type", "hash",
	"path", "folder", "move_waits_for", "operator_id", "paused_at", "paused_by",
	"paused_by_kind", "agent_handle", "note_id", "turn_id", "model", "kind", "ref",
	// THE QUESTION A RUN ASKED, which the person answering it read and is
	// shown again as what their answer answers.
	"question", "reference", "role", "shared_with", "asset", "name",
	// WHEN A NOTE IS READ, as steer_turn answers it: a steer.Reads value
	// (`next_step`, `after_answer`), which the dashboard words itself
	// (lib/steer.ts).
	"reads",
	// AND THE IDS AN ANSWER TO A RUN IS READ BACK BY: the question it answers
	// and the answer's own delivery, which a screen finds on the run's record.
	"launch_id", "answer_id",
	// A CHAT GESTURE'S ROOM AND MESSAGE, by the ids a screen opens them by,
	// the handles a membership names or an add put in, the emoji a reaction
	// is, and the topic and purpose people gave a room, shown as they were
	// given.
	"channel", "message_id", "members", "added", "emoji", "topic", "purpose"}

// writtenForThePerson are the answers whose text at one path is a model's
// answer written FOR the person who asked, by tool and path: the product
// itself, read as the person's own answer and never a tool's sentence to a
// model. Scoped to the tool and the path, never the key: the same key on
// another tool, or deeper in this one, is walked like any other.
var writtenForThePerson = map[string][]string{
	// answer_knowledge's answer, in markdown, as the tool answers it and
	// as the act route nests it under `receipt`.
	builtin.AnswerKnowledgeTool: {".answer_md", ".receipt.answer_md"},
}

// modelFraming is each mark a model's framing of somebody's words leaves on a
// string (internal/provenance): a fence's opening or closing line, the escape
// a marker-shaped run is defused with, and the stand-in for a turn's token.
// A person's answer carries none of it IN ANY FIELD, the value fields
// included, because the fence goes round the very value a screen shows (a
// room's topic, a run's question), and a walk that skipped the values
// skipped it.
var modelFraming = []struct {
	pattern *regexp.Regexp
	what    string
}{
	{regexp.MustCompile(`(?i)\[\s*/?\s*untrusted\s*:`), "a fence's line, which labels somebody's words for a model"},
	{regexp.MustCompile(`(?i)\[escaped `), "the escape a fence's marker is defused with for a model"},
	{regexp.MustCompile(regexp.QuoteMeta(provenance.TokenRemoved)), "the stand-in for a fence's token"},
}

// AnswerProblems is every sentence the decoded answer of tool carries that is
// not worded for a person, each named by its path: every string at any depth
// outside the value fields, so a receipt field a tool fills with a model's
// sentence is found whatever it is called; and a model's framing anywhere,
// the value fields included ([framed]).
func AnswerProblems(tool string, answer any) []string {
	var out []string
	sentences("", answer, func(path, text string) {
		if slices.Contains(writtenForThePerson[tool], path) {
			return
		}
		for _, problem := range Problems(text) {
			out = append(out, fmt.Sprintf("%s is not worded for a person: %s: %q",
				path, problem, text))
		}
	})
	framed("", answer, func(path, problem string) {
		out = append(out, fmt.Sprintf("%s is a model's framing, never a person's: %s",
			path, problem))
	})
	return out
}

// framed calls report with every place value carries a model's framing, at
// any depth and under any key: a string with one of [modelFraming]'s marks,
// and a key of [builtin.ModelOnlyKeys] (the envelope labelling a fenced
// answer, the model's caveats, a partial outcome's sentence), which a person's
// surface takes off the top of a receipt and which nothing nests deeper.
func framed(path string, value any, report func(path, problem string)) {
	switch v := value.(type) {
	case string:
		for _, mark := range modelFraming {
			if found := mark.pattern.FindString(v); found != "" {
				report(path, fmt.Sprintf("it carries %q, %s", found, mark.what))
			}
		}
	case []any:
		for i, item := range v {
			framed(fmt.Sprintf("%s[%d]", path, i), item, report)
		}
	case map[string]any:
		for key, item := range v {
			if slices.Contains(builtin.ModelOnlyKeys(), key) {
				report(path+"."+key, "it is a key that holds a model's words")
				continue
			}
			framed(path+"."+key, item, report)
		}
	}
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
