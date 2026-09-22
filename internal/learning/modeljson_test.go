package learning

import (
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

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
			draft, ok := parseSkillDraft(t.Context(), completionOf(
				wrap(`{"name":"n","description":"d","content":"c"}`)))
			if !ok || draft.Name != "n" {
				t.Errorf("parseSkillDraft declined a %s answer", name)
			}
			// The skill refiner.
			choice, ok := parseRefinement(t.Context(), completionOf(
				wrap(`{"skill_name":"n","bullet":"b"}`)))
			if !ok || choice.Bullet != "b" {
				t.Errorf("parseRefinement declined a %s answer", name)
			}
		})
	}
}

// TestTheVisibleAnswerLineIsBoundedAndTheDebugTwinIsWhole holds the pairing
// [answerLogFields] exists for: an operator-visible quote is a SHORTENING only
// while the rest of the answer is somewhere, and for a model answer that
// failed to decode the only "somewhere" this system has is the debug twin.
// A visible line that carried the whole answer, or a twin that carried a
// second copy of the quote, would each be one of the two lines doing nothing.
func TestTheVisibleAnswerLineIsBoundedAndTheDebugTwinIsWhole(t *testing.T) {
	t.Parallel()
	// Multi-byte throughout, so a cut that split a rune shows up as invalid
	// UTF-8 rather than as a byte count that happens to be right.
	answer := "ここに壊れた答えがあります。" + strings.Repeat("面", modelAnswerDetail)

	seen, whole := answerLogFields(answer, "turn_id", "t-1")

	seenFields := fieldMap(t, seen)
	wholeFields := fieldMap(t, whole)
	if seenFields["turn_id"] != "t-1" || wholeFields["turn_id"] != "t-1" {
		t.Fatalf("the caller's own fields did not survive: %v / %v", seen, whole)
	}
	quote, ok := seenFields["response"].(string)
	if !ok {
		t.Fatalf("the visible line carries no response field: %v", seen)
	}
	if quote == answer {
		t.Fatal("the visible line carried the whole answer, so nothing is bounded")
	}
	if len(quote) > modelAnswerDetail+len("…") {
		t.Errorf("the quote is %d bytes, past modelAnswerDetail (%d) plus the marker",
			len(quote), modelAnswerDetail)
	}
	if !strings.HasSuffix(quote, "…") {
		t.Errorf("the quote is not marked as cut, so a clipped answer reads as the whole one: %q", quote)
	}
	if !utf8.ValidString(quote) {
		t.Error("the quote split a rune")
	}
	if !strings.HasPrefix(answer, strings.TrimSuffix(quote, "…")) {
		t.Errorf("the quote is not the head of the answer: %q", quote)
	}
	if wholeFields["response"] != answer {
		t.Error("the debug twin does not carry the answer whole, so the quote above " +
			"is the value being destroyed rather than shortened")
	}
}

// TestTheTwoAnswerLinesDoNotShareABackingArray is the aliasing half of the
// same guarantee, and it is not hypothetical: both results extend the SAME
// caller slice, so building them with a plain append lets the second write
// land in the first's spare capacity — and the line an operator sees then
// carries the unbounded answer while looking exactly as if it were bounded.
// Only a caller whose slice has spare capacity can show it, which is why this
// one is built with room.
func TestTheTwoAnswerLinesDoNotShareABackingArray(t *testing.T) {
	t.Parallel()
	answer := strings.Repeat("x", modelAnswerDetail*2)
	fields := make([]any, 0, 8)
	fields = append(fields, "turn_id", "t-1")

	seen, whole := answerLogFields(answer, fields...)
	if quote := fieldMap(t, seen)["response"]; quote == answer {
		t.Fatal("the visible line was overwritten with the whole answer by the debug line's append")
	}
	if fieldMap(t, whole)["response"] != answer {
		t.Fatal("the debug line does not carry the answer whole")
	}
}

// fieldMap reads a slog-style key/value list, failing the test on a list that
// could not be one — an odd length or a non-string key is a log line that
// would reach a handler as "!BADKEY" rather than as the field it names.
func fieldMap(t *testing.T, fields []any) map[string]any {
	t.Helper()
	if len(fields)%2 != 0 {
		t.Fatalf("odd field list: %v", fields)
	}
	out := make(map[string]any, len(fields)/2)
	for i := 0; i < len(fields); i += 2 {
		key, ok := fields[i].(string)
		if !ok {
			t.Fatalf("field %d is not a string key: %v", i, fields[i])
		}
		out[key] = fields[i+1]
	}
	return out
}
