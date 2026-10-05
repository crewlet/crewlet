package prompts

import (
	"reflect"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/events/types"
)

// outlined is one prompt of the corpus, with the keys its outline must carry
// in order and the passages of EMBEDDED content each named section must hold.
type outlined struct {
	name   string
	prompt Prompt
	keys   []string
	// nested maps a section key to text that must fall inside that section
	// — a heading the content brought with it, which a reader splitting on
	// "##" lines would have filed somewhere else.
	nested map[string][]string
}

// outlineCorpus is every builder over real-shaped inputs (corpus_test.go) and
// over the zero inputs, where a section's absence is what is being tested.
func outlineCorpus() []outlined {
	rescued := richReviewInput()
	rescued.Rescued, rescued.Outcome = true, "incomplete"
	prose := richSubagentInput()
	prose.Submits, prose.Skills, prose.ToolCatalogue = false, nil, ""
	return []outlined{
		{
			name:   "executor, every block",
			prompt: BuildExecutor(richSeat(), richExecutorInput()),
			keys: []string{
				"identity", "company_context", "background", "responsibilities",
				"behavioral_guidelines", "unit", "policies", "team", "human_colleagues",
				"your_turn", "escalation", "sandbox",
				KeyThreadContext, KeyOnboardingHint, KeyPersonalMemory, KeySynthesizedSkills,
				KeyRelevantKnowledge, KeyEpisodeRecall, KeyCounterparty,
				"workers", "tool_skills", "available_tools",
			},
			nested: map[string][]string{
				KeyThreadContext:     {"## not a heading of ours"},
				KeyPersonalMemory:    {"# Diary"},
				KeySynthesizedSkills: {"## Deploy summaries"},
				KeyRelevantKnowledge: {"## Runbook: deploys", "ship 🚢"},
				KeyEpisodeRecall:     {"### 2026-09-30 — deploy 2.2"},
				KeyOnboardingHint:    {"`## Engineering handbook`"},
			},
		},
		{name: "executor, no seat", prompt: BuildExecutor(Seat{}, ExecutorInput{}), keys: []string{"your_turn"}},
		{
			name:   "executor, a plain seat",
			prompt: BuildExecutor(engineer(), ExecutorInput{}),
			keys: []string{"identity", "company_context", "responsibilities",
				"behavioral_guidelines", "unit", "policies", "your_turn"},
		},
		{
			name:   "review, every piece of evidence",
			prompt: BuildReview(richSeat(), richReviewInput()),
			keys: []string{"identity", "review_phase", "tool_skills", "earlier_rounds", "intent",
				"outcome", "blocked_by", "tool_log", "open_questions", "produced"},
			nested: map[string][]string{
				"produced":       {"## Summary", "## Next", "p99 → 200 ms"},
				"earlier_rounds": {"### Round 1 — sent back", "\"## Deploy 2.3 — done 🎉\""},
				"blocked_by":     {"## Blocked"},
			},
		},
		{
			name:   "review, rescued",
			prompt: BuildReview(richSeat(), rescued),
			keys: []string{"identity", "review_phase", "tool_skills", "earlier_rounds", "intent",
				"outcome", "blocked_by", "tool_log", "open_questions", "produced"},
		},
		{name: "review, nothing", prompt: BuildReview(Seat{}, ReviewInput{}), keys: []string{"review_phase", "tool_log"}},
		{
			name: "onboarding",
			prompt: BuildOnboarding(richSeat(), OnboardingInput{
				Hint: "## Pages\n- handbook ✅", ToolCatalogue: "- atlassian",
			}),
			keys:   []string{"identity", "onboarding_phase", "what_to_do", "available_tools"},
			nested: map[string][]string{"what_to_do": {"## Pages"}},
		},
		{name: "onboarding, no seat", prompt: BuildOnboarding(Seat{}, OnboardingInput{}), keys: []string{"onboarding_phase"}},
		{name: "onboarding, the ask", prompt: BuildOnboardingUserMessage(), keys: []string{"instruction"}},
		{
			name:   "worker",
			prompt: BuildSubagent(richSeat(), richSubagentInput()),
			keys:   []string{"task", "tool_skills", "available_tools", "worker_rules"},
			nested: map[string][]string{"task": {"## Steps", "all of it"}},
		},
		{name: "worker, prose", prompt: BuildSubagent(richSeat(), prose), keys: []string{"task", "worker_rules"}},
		{name: "worker, nothing", prompt: BuildSubagent(Seat{}, SubagentInput{}), keys: []string{"worker_rules"}},
		{
			name:   "user message, both ledgers",
			prompt: BuildPhaseUserMessage(richUserMessage()),
			keys:   []string{"conversation_history", "task", "prior_work"},
			nested: map[string][]string{
				"task": {"## Triage — decide BEFORE replying", "## Thread context",
					"naïve cache warmed ✅"},
				"prior_work":           {"### Round 2", "résumé"},
				"conversation_history": {"\"## Done ✅\""},
			},
		},
		{
			name:   "user message, a pull request",
			prompt: BuildPhaseUserMessage(UserMessage{TaskDescription: prTrigger}),
			keys:   []string{"task"},
			nested: map[string][]string{"task": {"# Add rate limiting", "## Why", "## How", "🧯"}},
		},
		{name: "user message, nothing", prompt: BuildPhaseUserMessage(UserMessage{}), keys: []string{"task"}},
	}
}

// assertTiles checks the outline's invariants directly — not through
// [Prompt.Valid], which is code under test like the builders are.
func assertTiles(t *testing.T, p Prompt) {
	t.Helper()
	sum, seen := 0, map[string]bool{}
	for _, s := range p.Sections {
		if s.Bytes <= 0 {
			t.Errorf("section %q measures %d bytes", s.Key, s.Bytes)
		}
		if seen[s.Key] {
			t.Errorf("key %q repeats", s.Key)
		}
		seen[s.Key] = true
		sum += s.Bytes
		if sum < len(p.Text) && !utf8.RuneStart(p.Text[sum]) {
			t.Errorf("section %q ends inside a rune, at byte %d", s.Key, sum)
		}
		if s.Title == "" {
			t.Errorf("section %q has no title", s.Key)
		}
	}
	if sum != len(p.Text) {
		t.Errorf("sections sum to %d bytes, the text is %d", sum, len(p.Text))
	}
	if !p.Valid() {
		t.Error("Valid refuses an outline that tiles its text")
	}
}

// slice is the text of the section with key, or false.
func slice(p Prompt, key string) (string, bool) {
	at := 0
	for _, s := range p.Sections {
		if s.Key == key {
			return p.Text[at : at+s.Bytes], true
		}
		at += s.Bytes
	}
	return "", false
}

// EVERY BUILDER'S OUTLINE TILES ITS TEXT, names the sections it rendered and no
// others, and keeps every heading its CONTENT brought inside the section that
// quotes it — the case a reader splitting on "##" lines gets wrong.
func TestEveryPromptsOutlineTilesItsText(t *testing.T) {
	t.Parallel()
	for _, tc := range outlineCorpus() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assertTiles(t, tc.prompt)
			var keys []string
			for _, s := range tc.prompt.Sections {
				keys = append(keys, s.Key)
			}
			if !slices.Equal(keys, tc.keys) {
				t.Errorf("keys = %v\nwant   %v", keys, tc.keys)
			}
			for key, passages := range tc.nested {
				text, ok := slice(tc.prompt, key)
				if !ok {
					t.Errorf("no %q section", key)
					continue
				}
				for _, passage := range passages {
					if !strings.Contains(text, passage) {
						t.Errorf("%q is not inside the %q section:\n%s", passage, key, text)
					}
				}
			}
		})
	}
}

// A HEADED SECTION STARTS WITH ITS OWN HEADING LINE, and its title is that
// heading's text — so the blank line a builder writes before a heading stays
// with the section before it, and a reader can render the slice as-is.
func TestAHeadedSectionStartsWithItsHeading(t *testing.T) {
	t.Parallel()
	// The sections whose text carries no heading of their own: the builder
	// named them.
	headless := map[string]bool{
		"identity": true, "task": true, "worker_rules": true, "instruction": true,
	}
	for _, tc := range outlineCorpus() {
		at := 0
		for i, s := range tc.prompt.Sections {
			text := tc.prompt.Text[at : at+s.Bytes]
			at += s.Bytes
			if headless[s.Key] && !strings.HasPrefix(text, "#") {
				continue
			}
			if i == 0 {
				// The first section starts at byte 0 whatever precedes its
				// heading (a seat with no identity opens on a blank line).
				text = strings.TrimLeft(text, "\n")
			}
			first, _, _ := strings.Cut(text, "\n")
			if !strings.HasPrefix(first, "#") {
				t.Errorf("%s: section %q starts %q, not with its heading", tc.name, s.Key, first)
				continue
			}
			if got := strings.TrimSpace(strings.TrimLeft(first, "#")); got != s.Title {
				t.Errorf("%s: section %q titled %q, its heading says %q", tc.name, s.Key, s.Title, got)
			}
		}
	}
}

// THE PREFETCH KEYS ARE THE PREFETCH SUMMARY'S OWN NAMES. Every block the
// executor's prefetch hands over is measured on PrefetchSummary as
// `<name>_hit` and `<name>_bytes`, and the outline keys the block on that same
// name — read here off the summary's tags, so a block renamed on one side and
// not the other fails rather than leaving a reader two names for one thing.
func TestThePrefetchKeysAreThePrefetchSummarysOwn(t *testing.T) {
	t.Parallel()
	var want []string
	summary := reflect.TypeFor[types.PrefetchSummary]()
	for i := range summary.NumField() {
		tag, _, _ := strings.Cut(summary.Field(i).Tag.Get("json"), ",")
		if name, ok := strings.CutSuffix(tag, "_hit"); ok {
			want = append(want, name)
		}
	}
	got := []string{
		KeyThreadContext, KeyOnboardingHint, KeyPersonalMemory, KeySynthesizedSkills,
		KeyRelevantKnowledge, KeyEpisodeRecall, KeyCounterparty,
	}
	slices.Sort(want)
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Errorf("prefetch keys = %v, PrefetchSummary measures %v", got, want)
	}
	// And the executor renders every one of them under that key.
	p := BuildExecutor(richSeat(), richExecutorInput())
	for _, key := range want {
		if _, ok := slice(p, key); !ok {
			t.Errorf("the executor's outline has no %q section", key)
		}
	}
}

// THE OUTLINE NEVER CHANGES A BYTE OF THE TEXT: a Builder's text is exactly
// the join of its parts, whatever sections were opened over them. A prompt's
// bytes are its cache key and its behaviour, so this is the property the whole
// outline rests on.
func TestABuilderTextIsExactlyTheJoinOfItsParts(t *testing.T) {
	t.Parallel()
	parts := []string{"", "lead — line", "\n## A", "body ✅", "", "## B", "\n\n", "\n### C", "tail"}
	for _, sep := range []string{"\n", "\n\n", ""} {
		b := NewBuilder(sep)
		b.Add(parts[0])
		b.Lead("lead", "Lead", parts[1])
		b.Heading("a", parts[2], parts[3])
		b.Add(parts[4])
		b.Heading("b", parts[5])
		b.Heading("blank", parts[6])
		b.Heading("c", parts[7], parts[8])
		p := b.Build()
		if p.Text != strings.Join(parts, sep) {
			t.Fatalf("sep %q: text = %q, want the join %q", sep, p.Text, strings.Join(parts, sep))
		}
		assertTiles(t, p)
		// The section of nothing but newlines measures nothing of its own
		// and is dropped rather than published as a blank part.
		for _, s := range p.Sections {
			if s.Key == "blank" {
				t.Errorf("sep %q: a newline-only section survived: %+v", sep, s)
			}
		}
	}
}

// Valid is what a reader of a map it did not build relies on before slicing by
// it, so it refuses each way a map can be wrong.
func TestValidRefusesAnOutlineThatDoesNotTile(t *testing.T) {
	t.Parallel()
	// "## A\nné\n" is nine bytes — é is two — and "## B\nx" six.
	text := "## A\nné\n## B\nx"
	good := []Section{{Key: "a", Title: "A", Bytes: 9}, {Key: "b", Title: "B", Bytes: 6}}
	if !(Prompt{Text: text, Sections: good}).Valid() {
		t.Fatal("a tiling outline was refused")
	}
	if !(Prompt{Text: text}).Valid() {
		t.Error("a prompt with no outline was refused")
	}
	for name, sections := range map[string][]Section{
		"short":         {{Key: "a", Bytes: 9}, {Key: "b", Bytes: 5}},
		"long":          {{Key: "a", Bytes: 9}, {Key: "b", Bytes: 7}},
		"inside a rune": {{Key: "a", Bytes: 7}, {Key: "b", Bytes: 8}},
		"empty":         {{Key: "a", Bytes: 9}, {Key: "z", Bytes: 0}, {Key: "b", Bytes: 6}},
		"repeated key":  {{Key: "a", Bytes: 9}, {Key: "a", Bytes: 6}},
		"no key":        {{Key: "a", Bytes: 9}, {Key: "", Bytes: 6}},
	} {
		if (Prompt{Text: text, Sections: sections}).Valid() {
			t.Errorf("%s: an outline that does not tile was accepted", name)
		}
	}
}
