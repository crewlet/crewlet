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
	prose.Submits, prose.Skills, prose.Discoverable = false, nil, ""
	return []outlined{
		{
			name:   "executor, every block",
			prompt: BuildExecutor(richSeat(), richExecutorInput()),
			keys: []string{
				"identity", "company_context", "background", "responsibilities",
				"behavioral_guidelines", "unit", "policies", KeyAccess, "team", "human_colleagues",
				"your_turn", "escalation", KeyUntrustedText,
				KeyThreadContext, KeyOpenAsks, KeySystems, KeyPersonalMemory, KeySynthesizedSkills,
				KeyRelevantKnowledge, KeyEpisodeRecall, KeyCounterparty,
				"workers", KeyCodeHistory, KeyCodeSearch, KeyCodeWrites, KeyCodeMerging,
			},
			nested: map[string][]string{
				KeyThreadContext:     {"## not a heading of ours"},
				KeyPersonalMemory:    {"# Diary"},
				KeySynthesizedSkills: {"## Deploy summaries"},
				KeyRelevantKnowledge: {"## Runbook: deploys", "ship 🚢"},
				KeyEpisodeRecall:     {"### 2026-09-30 — deploy 2.2"},
			},
		},
		{name: "executor, no seat", prompt: BuildExecutor(Seat{}, ExecutorInput{}), keys: []string{"your_turn", KeyUntrustedText}},
		{
			name:   "executor, a plain seat",
			prompt: BuildExecutor(engineer(), ExecutorInput{}),
			keys: []string{"identity", "company_context", "responsibilities",
				"behavioral_guidelines", "unit", "policies", "your_turn", KeyUntrustedText},
		},
		{
			name:   "review, every piece of evidence",
			prompt: BuildReview(richSeat(), richReviewInput()),
			keys: []string{"identity", "review_phase", "tool_skills", "earlier_rounds", "intent",
				"outcome", "blocked_by", "tool_log", "refused_replies", "open_questions", "produced",
				"delivered_refs"},
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
				"outcome", "blocked_by", "tool_log", "refused_replies", "open_questions", "produced",
				"delivered_refs"},
		},
		{name: "review, nothing", prompt: BuildReview(Seat{}, ReviewInput{}), keys: []string{"review_phase", "tool_log"}},
		{
			name: "onboarding",
			prompt: BuildOnboarding(richSeat(), OnboardingInput{
				Hint: "## Pages\n- handbook ✅", Discoverable: "- atlassian",
			}),
			keys:   []string{"identity", "onboarding_phase", "what_to_do", KeyDiscoverableTools},
			nested: map[string][]string{"what_to_do": {"## Pages"}},
		},
		{name: "onboarding, no seat", prompt: BuildOnboarding(Seat{}, OnboardingInput{}), keys: []string{"onboarding_phase"}},
		{name: "onboarding, the ask", prompt: BuildOnboardingUserMessage(), keys: []string{"instruction"}},
		{
			name:   "worker",
			prompt: BuildSubagent(richSeat(), richSubagentInput()),
			keys:   []string{"task", "tool_skills", KeyDiscoverableTools, "worker_rules"},
			nested: map[string][]string{"task": {"## Steps", "all of it"}},
		},
		{name: "worker, prose", prompt: BuildSubagent(richSeat(), prose), keys: []string{"task", "worker_rules"}},
		{name: "worker, nothing", prompt: BuildSubagent(Seat{}, SubagentInput{}), keys: []string{"worker_rules"}},
		{
			name:   "user message, both ledgers",
			prompt: BuildPhaseUserMessage(richUserMessage()),
			keys:   []string{"conversation_endings", KeyCapabilityChanges, "task", "prior_work"},
			nested: map[string][]string{
				"task": {"## Triage — decide BEFORE replying", "## Thread context",
					"naïve cache warmed ✅"},
				"prior_work":           {"### Round 2", "résumé"},
				"conversation_endings": {"\"## Done ✅\""},
			},
		},
		{
			name:   "user message, a pull request",
			prompt: BuildPhaseUserMessage(UserMessage{TaskDescription: prTrigger}),
			keys:   []string{"task"},
			nested: map[string][]string{"task": {"# Add rate limiting", "## Why", "## How", "🧯"}},
		},
		{name: "user message, nothing", prompt: BuildPhaseUserMessage(UserMessage{}), keys: []string{"task"}},
		{
			// A SECTION APPENDED TO A BUILT PROMPT ([Prompt.Then]) tiles it
			// as a builder's last section would.
			name: "user message, then a kept session it does not continue",
			prompt: BuildPhaseUserMessage(richUserMessage()).Then(KeyKeptSession,
				KeptSession("t-0 🧰", false, false)),
			keys: []string{"conversation_endings", KeyCapabilityChanges, "task", "prior_work", KeyKeptSession},
		},
		{
			name: "user message, nothing, then a kept session",
			prompt: BuildPhaseUserMessage(UserMessage{}).Then(KeyKeptSession,
				KeptSession("t-1", true, true)),
			keys: []string{"task", KeyKeptSession},
		},
		{
			name: "a send-back, continued",
			prompt: BuildContinuedRound(ContinuedRound{Task: chatTrigger,
				Review: "## Missing\n- the summary was never posted — post it in the thread 🧵"}),
			keys: []string{"sent_back", "task", "prior_work", "review"},
			nested: map[string][]string{
				"task":   {"## Triage — decide BEFORE replying", "## Thread context", "naïve cache warmed ✅"},
				"review": {"## Missing", "🧵"},
			},
		},
		{name: "a send-back, nothing", prompt: BuildContinuedRound(ContinuedRound{}), keys: []string{"sent_back", "task", "prior_work"}},
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
// with the section before it, and a reader can render the slice as-is. And the
// map SAYS which sections those are: every other one is a part the builder
// named, which no reader can tell from one opening on somebody else's heading.
func TestAHeadedSectionStartsWithItsHeading(t *testing.T) {
	t.Parallel()
	// The sections that may carry no heading of their own, where the builder
	// named them — `identity` and `task` are headed in some prompts and
	// named in others.
	headless := map[string]bool{
		"identity": true, "task": true, "worker_rules": true, "instruction": true,
		"reference": true,
	}
	for _, tc := range outlineCorpus() {
		at := 0
		for i, s := range tc.prompt.Sections {
			text := tc.prompt.Text[at : at+s.Bytes]
			at += s.Bytes
			if !s.Headed && !headless[s.Key] {
				t.Errorf("%s: section %q is not marked headed", tc.name, s.Key)
			}
			if !s.Headed {
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
		KeyThreadContext, KeyOpenAsks, KeySystems, KeyPersonalMemory, KeySynthesizedSkills,
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

// HEADED IS THE BUILDER'S WORD, NEVER THE TEXT'S. A lead part carrying
// somebody else's markdown — a worker persona, a task the executor wrote —
// often opens with a heading of its own, and that heading is the content's,
// under the title the builder gave the part: a reader that took it for the
// part's own heading lost its words.
func TestALeadIsNeverHeadedWhateverItsTextOpensWith(t *testing.T) {
	t.Parallel()
	b := NewBuilder("\n")
	b.Lead("task", "Task", "## Goal\nDo X\n\n## Steps\n1. a")
	b.Heading("rules", "## Rules", "be brief")
	p := b.Build()
	assertTiles(t, p)
	if len(p.Sections) != 2 {
		t.Fatalf("sections = %+v", p.Sections)
	}
	if task := p.Sections[0]; task.Headed || task.Title != "Task" {
		t.Errorf("lead = %+v, want the builder's title and not headed", task)
	}
	if rules := p.Sections[1]; !rules.Headed || rules.Title != "Rules" {
		t.Errorf("heading = %+v, want headed under its own heading's text", rules)
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
	// Bytes that add up over text that is not UTF-8 do not tile what a
	// reader receives: JSON rewrites each bad byte as U+FFFD, three bytes
	// for one, and every boundary after it moves.
	bad := "## A\nn\xffe\n## B\nx"
	if (Prompt{Text: bad, Sections: []Section{{Key: "a", Bytes: 9}, {Key: "b", Bytes: 6}}}).Valid() {
		t.Error("an outline over invalid UTF-8 was accepted")
	}
}

// A PROMPT TOLD AS A PART OF ANOTHER MOVES ITS BUILDER'S HEADINGS AND NOTHING
// ELSE. A later brief tells again, under a heading of its own, a send-back or a
// turn's task that a session may lack, nested a level down so it reads as part
// of that message ([Prompt.Nested]): every heading a builder opened a section
// with moves one level, by the outline that builder recorded, and every other
// byte stays where it was. Somebody else's words under those headings (a
// trigger's own "## Triage", a pull request's "# Title", a reviewer's "##
// Missing", a code block's "#include", a worker's task opening on its own "#
// Fix the login bug") are kept byte for byte, as every builder's prompt in the
// corpus holds them, and the outline still tiles the text, each section keeping
// its key, its title and whether it is headed. A prompt with no outline, or one
// that does not tile, is returned as it is.
//
// Mutation: move every line that begins with a '#', move a section that is not
// headed, leave the outline measuring the text as it was, or slice by an
// outline that does not tile, and a case goes red.
func TestNestingAPromptMovesOnlyTheHeadingsItsBuilderWrote(t *testing.T) {
	t.Parallel()
	// SOMEBODY'S WORDS, fenced under who wrote them as a chat reply is, with
	// a heading, a code block's "#include" and a heading of their own.
	words := "[untrusted:AAAAAAAAAAAAAAAA source=chat_thread trust=colleague authors=ana]\n" +
		"# install the deps first\n```c\n#include <stdio.h>\n```\n## Steps to reproduce\n- run make\n" +
		"[/untrusted:AAAAAAAAAAAAAAAA]"
	corpus := append(outlineCorpus(),
		outlined{name: "a send-back whose task and review hold somebody's headings",
			prompt: BuildContinuedRound(ContinuedRound{Task: words, Review: words})},
		outlined{name: "a task and a ledger holding somebody's headings",
			prompt: BuildPhaseUserMessage(UserMessage{TaskDescription: words,
				PriorWork: "### Iteration 1\nProduced: " + words})},
		// A PART THE BUILDER NAMED, which opens on its writer's heading: the
		// worker's task, a lead section, is no heading of the builder's.
		outlined{name: "a worker's task opening on its writer's heading",
			prompt: BuildSubagent(Seat{}, SubagentInput{ParentSystemPrompt: "# Fix the login bug\n" + words})})
	for _, tc := range corpus {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := tc.prompt.Nested()
			assertTiles(t, got)
			if len(got.Sections) != len(tc.prompt.Sections) {
				t.Fatalf("nested, the outline has %d sections, want the %d it had", len(got.Sections),
					len(tc.prompt.Sections))
			}
			at, was := 0, 0
			for i, s := range got.Sections {
				before := tc.prompt.Sections[i]
				part, original := got.Text[at:at+s.Bytes], tc.prompt.Text[was:was+before.Bytes]
				at, was = at+s.Bytes, was+before.Bytes
				if s.Key != before.Key || s.Title != before.Title || s.Headed != before.Headed {
					t.Errorf("section %+v is nested as %+v, want its key, title and heading kept", before, s)
				}
				// ITS OWN HEADING ONE LEVEL DOWN, where the builder opened it
				// with one, and every other byte as it was.
				want := original
				if blank := len(original) - len(strings.TrimLeft(original, "\n")); before.Headed {
					if !strings.HasPrefix(original[blank:], "#") {
						t.Fatalf("headed section %q does not open on its heading: %q", before.Key, original)
					}
					want = original[:blank] + "#" + original[blank:]
				}
				if part != want {
					t.Errorf("section %q is nested as\n%s\nwant\n%s", s.Key, part, want)
				}
			}
			if n := strings.Count(tc.prompt.Text, words); n > 0 && strings.Count(got.Text, words) != n {
				t.Errorf("somebody's words are told %d times, want the %d the prompt held, byte for byte:\n%s",
					strings.Count(got.Text, words), n, got.Text)
			}
		})
	}
	for name, p := range map[string]Prompt{
		"no outline": {Text: "## A\n# install the deps first"},
		"an outline that overruns": {Text: "## A\nx",
			Sections: []Section{{Key: "a", Title: "A", Bytes: 99, Headed: true}}},
		"an outline that falls short": {Text: "## A\nx\n## B",
			Sections: []Section{{Key: "a", Title: "A", Bytes: 3, Headed: true}}},
	} {
		if got := p.Nested(); got.Text != p.Text || !slices.Equal(got.Sections, p.Sections) {
			t.Errorf("%s: the prompt is nested as %+v, want it as it was: no heading of its builder's is known",
				name, got)
		}
	}
}
