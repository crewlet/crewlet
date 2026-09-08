package cliagent_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/cliagent"
)

// A SYSTEM PROMPT IS NOT A USER MESSAGE, where the CLI has a channel for it.
//
// Folded into the transcript it arrives as user content — the model is asked
// to treat an ordinary message as its standing instructions, underneath a
// vendor default that keeps announcing what it is. SplitSystem is what lifts
// it out; this pins that the transcript then carries neither the text nor its
// heading, and that everything else is untouched.
func TestSplitSystemLiftsTheSystemPromptOutOfTheTranscript(t *testing.T) {
	t.Parallel()
	req := llm.Request{Messages: []llm.Message{
		{Role: llm.RoleSystem, Content: "You are Agent PM at Nimbus."},
		{Role: llm.RoleUser, Content: "what are you working on?"},
		{Role: llm.RoleSystem, Content: "Always reply in the thread."},
	}}

	system, rest := cliagent.SplitSystem(req)

	if want := "You are Agent PM at Nimbus.\n\nAlways reply in the thread."; system != want {
		t.Errorf("system = %q, want the messages joined in order", system)
	}
	if len(rest.Messages) != 1 || rest.Messages[0].Role != llm.RoleUser {
		t.Fatalf("rest = %+v, want only the user message", rest.Messages)
	}
	prompt, err := cliagent.RenderPrompt(rest)
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{"## system", "Agent PM", "in the thread"} {
		if strings.Contains(prompt, leak) {
			t.Errorf("the transcript still carries %q, so the prompt is sent twice", leak)
		}
	}
	if !strings.Contains(prompt, "what are you working on?") {
		t.Error("the user message did not survive the split")
	}
}

// NOTHING TO LIFT IS NOT AN EMPTY SYSTEM PROMPT. A CLI handed an empty one
// would replace its default with nothing; the caller passes no flag at all,
// which it can only do if this reports the absence honestly.
func TestSplitSystemReportsNothingToLift(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		msgs []llm.Message
	}{
		{"no system message", []llm.Message{{Role: llm.RoleUser, Content: "hi"}}},
		{"a blank one", []llm.Message{{Role: llm.RoleSystem, Content: "   \n "}, {Role: llm.RoleUser, Content: "hi"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if system, _ := cliagent.SplitSystem(llm.Request{Messages: tc.msgs}); system != "" {
				t.Errorf("system = %q, want empty", system)
			}
		})
	}
}

// THE SHIPPED CLAUDE PROFILE USES THE FILE VARIANT, NOT THE INLINE ONE.
//
// A seat's system prompt carries the org chart, its policies, its roster and
// its own memory. On argv that is readable by every account on the machine
// through /proc/<pid>/cmdline, and bounded by ARG_MAX. Swap {file} for
// {system} here and this fails.
func TestTheClaudeProfileKeepsTheSystemPromptOffArgv(t *testing.T) {
	t.Parallel()
	profile, ok := cliagent.Builtin("claude-code")
	if !ok {
		t.Fatal("no built-in claude-code profile")
	}
	if len(profile.SystemPromptArgs) == 0 {
		t.Fatal("the claude-code profile carries no system_prompt_args, so every " +
			"seat's identity is delivered as an ordinary user message")
	}
	joined := strings.Join(profile.SystemPromptArgs, " ")
	if !strings.Contains(joined, "{file}") {
		t.Errorf("system_prompt_args = %v, want a {file} substitution: {system} "+
			"puts the company's org chart and the seat's memory on argv", profile.SystemPromptArgs)
	}
	if strings.Contains(joined, "{system}") {
		t.Errorf("system_prompt_args = %v puts the prompt text on argv", profile.SystemPromptArgs)
	}
	// Replace, not append: the default prompt announces a coding-agent
	// identity over the top of whatever seat is actually being served.
	if !strings.Contains(joined, "--system-prompt-file") {
		t.Errorf("system_prompt_args = %v, want the replacing flag", profile.SystemPromptArgs)
	}
}

// EVERY SHIPPED PROFILE'S system_prompt_args IS SUBSTITUTABLE, and says which
// channel it chose.
//
// A placeholder the code does not substitute is the failure mode with no
// symptom: the CLI is handed the literal "{file}" as its system prompt, takes
// it without complaint, and every seat on that provider runs with a one-word
// identity. The two spellings are the whole vocabulary [systemArgs] knows.
//
// The table is the SHIPPED set, so adopting a flag for a new CLI has to come
// through here — which is where the argv trade gets stated rather than
// stumbled into.
func TestEveryProfileWithASystemPromptChannelNamesASubstitutionWeMake(t *testing.T) {
	t.Parallel()
	// Checked against each CLI's own --help at the version named in
	// docs/concepts/subscription-llm-backends.md. Six of the eight have no
	// such flag at all, and an empty entry here is that fact rather than an
	// omission.
	want := map[string]string{
		"claude-code": "{file}",   // 2.1.263: --system-prompt-file
		"qwen-code":   "{system}", // 0.23.0: --system-prompt, string only
		// 1.0.13, xAI's OWN CLI from x.ai/cli:
		// --system-prompt-override, string only. NOT the same-named
		// community package on npm, which has no such flag — both put a
		// `grok` on PATH and checking the wrong one answers the wrong
		// question.
		"grok":         "{system}",
		"codex":        "", // 0.153.4, on `codex` and `codex exec` alike
		"gemini-cli":   "", // 0.58.0
		"opencode":     "", // 1.18.29 (`--agent` names a config persona)
		"copilot":      "", // 1.0.83
		"cursor-agent": "", // 2026.09.02
	}
	for name, placeholder := range want {
		profile, ok := cliagent.Builtin(name)
		if !ok {
			t.Errorf("no built-in %q profile", name)
			continue
		}
		joined := strings.Join(profile.SystemPromptArgs, " ")
		if placeholder == "" {
			if joined != "" {
				t.Errorf("%s declares system_prompt_args = %v, but its CLI has no "+
					"such flag — the text would reach it as a literal argument",
					name, profile.SystemPromptArgs)
			}
			continue
		}
		if joined == "" {
			t.Errorf("%s carries no system_prompt_args, so every seat's identity "+
				"is delivered as an ordinary user message", name)
			continue
		}
		if !strings.Contains(joined, placeholder) {
			t.Errorf("%s system_prompt_args = %v, want a %s substitution",
				name, profile.SystemPromptArgs, placeholder)
		}
		// Exactly one channel, never both: a profile that names {file} AND
		// {system} writes the prompt to disk and then puts it on argv too.
		other := "{system}"
		if placeholder == "{system}" {
			other = "{file}"
		}
		if strings.Contains(joined, other) {
			t.Errorf("%s system_prompt_args = %v names both channels",
				name, profile.SystemPromptArgs)
		}
	}
}

// THE FILE IS PRIVATE AND LANDS IN THE PER-CALL DIRECTORY, which is created
// empty for one call and removed on release — so the text cannot outlive the
// call that needed it or reach the next one.
func TestSystemPromptFileIsPrivateToTheCall(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	args, err := cliagent.SystemArgsForTest(
		[]string{"--system-prompt-file", "{file}"}, "the seat's whole identity", dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(args) != 2 || args[0] != "--system-prompt-file" {
		t.Fatalf("args = %v", args)
	}
	if filepath.Dir(args[1]) != dir {
		t.Errorf("the prompt landed at %q, outside the per-call directory %q", args[1], dir)
	}
	info, err := os.Stat(args[1])
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("mode = %o, want 600: the prompt carries company-internal content "+
			"and the box is a directory other accounts can reach", perm)
	}
	body, err := os.ReadFile(args[1])
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "the seat's whole identity" {
		t.Errorf("file holds %q", body)
	}
}
