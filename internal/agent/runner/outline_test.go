package runner_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/prompts"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/llmtest"
)

// chatTask is a chat trigger's body as internal/notify renders it, with the
// engine's own sections written INTO the body — the headings a reader
// splitting the prompt on "##" lines filed outside the task.
const chatTask = "Ana in #eng — \"post the deploy summary? 🚀\"\n" +
	"\n## Triage — decide BEFORE replying\n1. Is this addressed to you?\n" +
	"\n## Thread context\n- Bo: café rollout done ✅\n"

// outline is a published section map back in the builder's shape, so the
// assertion is the builder's own tiling check rather than a second copy of it.
func outline(text string, sections []types.PromptSection) prompts.Prompt {
	p := prompts.Prompt{Text: text}
	for _, s := range sections {
		p.Sections = append(p.Sections, prompts.Section{
			Key: s.Key, Title: s.Title, Bytes: s.Bytes, Headed: s.Headed,
		})
	}
	return p
}

func sectionText(p prompts.Prompt, key string) string {
	at := 0
	for _, s := range p.Sections {
		if s.Key == key {
			return p.Text[at : at+s.Bytes]
		}
		at += s.Bytes
	}
	return ""
}

func keysOf(sections []types.PromptSection) []string {
	var out []string
	for _, s := range sections {
		out = append(out, s.Key)
	}
	return out
}

// EVERY PROMPT A PHASE SENDS CARRIES ITS OUTLINE: the dashboard draws a
// prompt's parts from the map its builder recorded, so a trigger's own
// "## Triage" stays inside the task it belongs to rather than becoming a
// section of the prompt beside it. The live opening frame carries it for
// every phase — for the executor, it is the one record of what its run was
// sent — and an engine-driven phase's completion record carries the same.
func TestThePromptsAPhaseSendsCarryTheirOutlines(t *testing.T) {
	t.Parallel()
	pub := newCapture()
	prov := &scriptedProvider{
		review: []llmtest.Response{submitCall(t, runner.SubmitReviewTool,
			`{"decision":"done","final_artifact":"a"}`)},
	}
	entries := []phase.Entry{{Key: "default", Provider: prov}}
	round := executeRound(t, entries, buildOpts{pub: pub, task: chatTask}, 1, "", nil,
		run{calls: []llm.ToolCall{submitWork(t)}}, runner.RunRecord{})
	if _, err := round.collector.Review(context.Background(), 1, round.work, nil); err != nil {
		t.Fatalf("Review: %v", err)
	}

	for _, tc := range []struct {
		phase    string
		userKeys []string
		// Whether each user section opens on its own heading: the
		// executor's task does ("## Task"), the reviewer's two are parts
		// the builder named, whatever the trigger's body opens with.
		userHeaded []bool
	}{
		{"execute", []string{"task"}, []bool{true}},
		{"review", []string{"reference", "task"}, []bool{false, false}},
	} {
		open := openingFrame(t, pub, tc.phase)
		if len(open.PromptMessages) != 2 {
			t.Fatalf("%s: opening frame prompt_messages = %+v", tc.phase, open.PromptMessages)
		}
		sys, usr := open.PromptMessages[0], open.PromptMessages[1]
		system := outline(sys.Content, sys.Sections)
		user := outline(usr.Content, usr.Sections)
		if len(system.Sections) == 0 || !system.Valid() {
			t.Errorf("%s: system sections %v do not tile the system prompt", tc.phase, sys.Sections)
		}
		if !user.Valid() || !slices.Equal(keysOf(usr.Sections), tc.userKeys) {
			t.Errorf("%s: user sections = %v, want %v tiling the user prompt",
				tc.phase, usr.Sections, tc.userKeys)
		}
		task := sectionText(user, "task")
		for _, embedded := range []string{"## Triage — decide BEFORE replying", "## Thread context", "café"} {
			if !strings.Contains(task, embedded) {
				t.Errorf("%s: %q is not inside the task section:\n%s", tc.phase, embedded, task)
			}
		}
		// HEADED TRAVELS, section by section, as the builder said it.
		var headed []bool
		for _, s := range usr.Sections {
			headed = append(headed, s.Headed)
		}
		if !slices.Equal(headed, tc.userHeaded) {
			t.Errorf("%s: user sections headed = %v, want %v", tc.phase, headed, tc.userHeaded)
		}
		if !slices.ContainsFunc(sys.Sections, func(s types.PromptSection) bool { return s.Headed }) {
			t.Errorf("%s: no system section is marked headed: %+v", tc.phase, sys.Sections)
		}
	}

	// The reviewer's record carries the maps its live frame did.
	done := completedPhase(t, pub, "review")
	open := openingFrame(t, pub, "review")
	if !slices.Equal(open.PromptMessages[0].Sections, done.SystemSections) ||
		!slices.Equal(open.PromptMessages[1].Sections, done.UserSections) {
		t.Error("review: the live frame's outlines differ from the record's")
	}
}

// A PROMPT THAT IS NOT UTF-8 PUBLISHES NO MAP. Its text travels as JSON, which
// rewrites each invalid byte to three, so a map measured over the bytes the
// builder joined no longer tiles the text a reader receives. A vendor's body
// is where such bytes come from, and the reader falls back to the headings.
func TestAPromptThatIsNotUTF8PublishesNoOutline(t *testing.T) {
	t.Parallel()
	pub := newCapture()
	launch(t, []phase.Entry{{Key: "default", Provider: &scriptedProvider{}}},
		buildOpts{pub: pub, task: "a vendor body with a bad byte: \xff — and more"}, 1, "", nil)
	open := openingFrame(t, pub, "execute")
	if len(open.PromptMessages) != 2 {
		t.Fatalf("opening frame prompt_messages = %+v", open.PromptMessages)
	}
	if got := open.PromptMessages[1].Sections; len(got) != 0 {
		t.Errorf("user sections = %v over text JSON will rewrite", got)
	}
	if len(open.PromptMessages[0].Sections) == 0 {
		t.Error("the system prompt, which is UTF-8, lost its outline too")
	}
}

// A COLLECTED RUN OPENED NO CONVERSATION, so the record its collection
// publishes carries no prompt and no outline — an empty map rather than one
// describing a prompt this segment did not send. The launch's opening frame
// is where what the run was sent is on the record.
func TestACollectedRunCarriesNoOutline(t *testing.T) {
	t.Parallel()
	pub := newCapture()
	executeRound(t, []phase.Entry{{Key: "default", Provider: &scriptedProvider{}}},
		buildOpts{pub: pub}, 1, "", nil, run{calls: []llm.ToolCall{submitWork(t)}}, runner.RunRecord{})
	done := completedPhase(t, pub, "execute")
	if done.SystemPrompt != "" || len(done.SystemSections) != 0 || len(done.UserSections) != 0 {
		t.Errorf("a collected run's record published a prompt outline: %v %v",
			done.SystemSections, done.UserSections)
	}
}
