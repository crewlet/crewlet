package learning

import (
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/providers/llm"
)

// completionOf is a model answer carrying nothing but its text.
func completionOf(content string) *llm.Completion {
	return &llm.Completion{Content: content}
}

// THE LADDER, IN ORDER, AND WHAT EACH RUNG IS FOR.
//
// The order is the contract: the trimmed text first so a clean answer decodes
// exactly as sent, then the unfenced body, then the brace span — most
// destructive last, because it will carve a brace pair out of prose that was
// never JSON.
func TestTheJSONRecoveryLadderTriesTheLeastDestructiveFirst(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		raw  string
		want []string
	}{
		{"nothing at all", "   \n ", nil},
		// A clean answer produces ONE candidate: unfencing and the brace
		// span both reproduce it, and a duplicate would be a wasted decode
		// on the overwhelmingly common case.
		{"a clean object", ` {"kind":"FACT"} `, []string{`{"kind":"FACT"}`}},
		{
			"fenced",
			"```json\n{\"kind\":\"FACT\"}\n```",
			[]string{"```json\n{\"kind\":\"FACT\"}\n```", `{"kind":"FACT"}`},
		},
		{
			"prose around it",
			`Here's the classification: {"kind":"FACT"} — hope that helps`,
			[]string{
				`Here's the classification: {"kind":"FACT"} — hope that helps`,
				`{"kind":"FACT"}`,
			},
		},
		// BOTH RUNGS, and they differ: unfencing leaves the apology, and
		// only the brace span gets past it.
		{
			"fenced AND apologised for",
			"```\nSorry! {\"kind\":\"FACT\"}\n```",
			[]string{
				"```\nSorry! {\"kind\":\"FACT\"}\n```",
				`Sorry! {"kind":"FACT"}`,
				`{"kind":"FACT"}`,
			},
		},
		// Nothing brace-shaped to carve: the ladder stops rather than
		// inventing a candidate.
		{"pure prose", "I don't have anything to add.", []string{"I don't have anything to add."}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := modelJSONCandidates(tc.raw); !slices.Equal(got, tc.want) {
				t.Errorf("modelJSONCandidates(%q) =\n  %q\nwant\n  %q", tc.raw, got, tc.want)
			}
		})
	}
}

// EVERY WORKER READS THE SAME MALFORMATIONS.
//
// Three recovery rules had grown between the four passes that ask a model for
// JSON: one unfenced, one took the brace span, one did neither. Which
// malformations a worker survived was an accident of which helper its author
// reached for — and a worker that declines a fenced answer is observationally
// identical to a model with nothing to say, so it just quietly stops producing
// at whatever rate that model fences.
func TestEveryModelJSONParserSurvivesTheSameMalformations(t *testing.T) {
	t.Parallel()
	// The three shapes a model actually sends, each of which at least one
	// of the old parsers dropped on the floor.
	shapes := map[string]func(body string) string{
		"bare":      func(body string) string { return body },
		"fenced":    func(body string) string { return "```json\n" + body + "\n```" },
		"prose":     func(body string) string { return "Sure — " + body + "\n\nLet me know!" },
		"both":      func(body string) string { return "```\nSure — " + body + "\n```" },
		"one-liner": func(body string) string { return "```json" + body + "```" },
	}
	for name, wrap := range shapes {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			// The persistence classifier and the counterparty profiler.
			if obj, ok := extractJSONObject(wrap(`{"kind":"FACT"}`)); !ok || stringField(obj, "kind") != "FACT" {
				t.Errorf("extractJSONObject declined a %s answer", name)
			}
			// The compaction summary.
			summary, err := ParseSummary(wrap(`{"common_task_pattern":"triage"}`))
			if err != nil || summary.CommonTaskPattern != "triage" {
				t.Errorf("ParseSummary declined a %s answer: %+v %v", name, summary, err)
			}
			// The skill synthesizer.
			draft, ok := parseSkillDraft(completionOf(
				wrap(`{"name":"n","description":"d","content":"c"}`)))
			if !ok || draft.Name != "n" {
				t.Errorf("parseSkillDraft declined a %s answer", name)
			}
			// The skill refiner.
			choice, ok := parseRefinement(completionOf(
				wrap(`{"skill_name":"n","bullet":"b"}`)))
			if !ok || choice.Bullet != "b" {
				t.Errorf("parseRefinement declined a %s answer", name)
			}
		})
	}
}
