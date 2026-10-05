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

// EVERY PHASE RECORD CARRIES ITS PROMPTS' OUTLINES, and so does the live
// opening frame: the dashboard draws a prompt's parts from the map its builder
// recorded, so a trigger's own "## Triage" stays inside the task it belongs to
// rather than becoming a section of the prompt beside it.
func TestThePhaseRecordsCarryTheirPromptsOutlines(t *testing.T) {
	t.Parallel()
	pub := newCapture()
	prov := &scriptedProvider{
		execute: []llm.Completion{submitWork(t)},
		review: []llm.Completion{submitCall(t, runner.SubmitReviewTool,
			`{"decision":"done","final_artifact":"a"}`)},
	}
	r, _ := buildWith(t, []phase.Entry{{Key: "default", Provider: prov}},
		buildOpts{pub: pub, task: chatTask})
	work, _, err := r.Execute(context.Background(), 1, "", nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if _, err := r.Review(context.Background(), 1, work, nil); err != nil {
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
		done := completedPhase(t, pub, tc.phase)
		system := outline(done.SystemPrompt, done.SystemSections)
		user := outline(done.UserPrompt, done.UserSections)
		if len(system.Sections) == 0 || !system.Valid() {
			t.Errorf("%s: system_sections %v do not tile the system prompt", tc.phase, done.SystemSections)
		}
		if !user.Valid() || !slices.Equal(keysOf(done.UserSections), tc.userKeys) {
			t.Errorf("%s: user_sections = %v, want %v tiling the user prompt",
				tc.phase, done.UserSections, tc.userKeys)
		}
		task := sectionText(user, "task")
		for _, embedded := range []string{"## Triage — decide BEFORE replying", "## Thread context", "café"} {
			if !strings.Contains(task, embedded) {
				t.Errorf("%s: %q is not inside the task section:\n%s", tc.phase, embedded, task)
			}
		}
		// HEADED TRAVELS, section by section, as the builder said it.
		var headed []bool
		for _, s := range done.UserSections {
			headed = append(headed, s.Headed)
		}
		if !slices.Equal(headed, tc.userHeaded) {
			t.Errorf("%s: user sections headed = %v, want %v", tc.phase, headed, tc.userHeaded)
		}
		if !slices.ContainsFunc(done.SystemSections, func(s types.PromptSection) bool { return s.Headed }) {
			t.Errorf("%s: no system section is marked headed: %+v", tc.phase, done.SystemSections)
		}

		// The live opening frame carries the same maps, message by message.
		open := openingFrame(t, pub, tc.phase)
		if len(open.PromptMessages) != 2 {
			t.Fatalf("%s: opening frame prompt_messages = %+v", tc.phase, open.PromptMessages)
		}
		if !slices.Equal(open.PromptMessages[0].Sections, done.SystemSections) ||
			!slices.Equal(open.PromptMessages[1].Sections, done.UserSections) {
			t.Errorf("%s: the live frame's outlines differ from the record's", tc.phase)
		}
	}
}

// A PROMPT THAT IS NOT UTF-8 PUBLISHES NO MAP. Its text travels as JSON, which
// rewrites each invalid byte to three, so a map measured over the bytes the
// builder joined no longer tiles the text a reader receives. A vendor's body
// is where such bytes come from, and the reader falls back to the headings.
func TestAPromptThatIsNotUTF8PublishesNoOutline(t *testing.T) {
	t.Parallel()
	pub := newCapture()
	prov := &scriptedProvider{execute: []llm.Completion{submitWork(t)}}
	r, _ := buildWith(t, []phase.Entry{{Key: "default", Provider: prov}},
		buildOpts{pub: pub, task: "a vendor body with a bad byte: \xff — and more"})
	if _, _, err := r.Execute(context.Background(), 1, "", nil); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	done := completedPhase(t, pub, "execute")
	if len(done.UserSections) != 0 {
		t.Errorf("user_sections = %v over text JSON will rewrite", done.UserSections)
	}
	if len(done.SystemSections) == 0 {
		t.Error("the system prompt, which is UTF-8, lost its outline too")
	}
}

// A RESUMED EXECUTOR OPENED NO CONVERSATION, so its record carries no prompt
// and no outline — an empty map rather than one describing a prompt that was
// not sent this time.
func TestAResumedPhaseCarriesNoOutline(t *testing.T) {
	t.Parallel()
	pub := newCapture()
	prov := &scriptedProvider{execute: []llm.Completion{submitWork(t)}}
	r, _ := buildWith(t, []phase.Entry{{Key: "default", Provider: prov}}, buildOpts{
		pub:    pub,
		resume: &runner.Resume{State: suspendedAfterTwoRounds(), Answer: "the run succeeded"},
	})
	if _, _, err := r.Resume(context.Background(), nil); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	done := completedPhase(t, pub, "execute")
	if done.SystemPrompt != "" || len(done.SystemSections) != 0 || len(done.UserSections) != 0 {
		t.Errorf("a re-entered phase published a prompt outline: %v %v",
			done.SystemSections, done.UserSections)
	}
}
