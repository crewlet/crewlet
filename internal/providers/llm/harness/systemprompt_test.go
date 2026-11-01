package harness_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/harness"
	"github.com/crewlet/crewlet/internal/providers/llm/harness/harnessprofile"
)

// A SYSTEM PROMPT IS NOT A USER MESSAGE, where the harness has a channel for it.
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

	system, rest := harness.SplitSystem(req)

	if want := "You are Agent PM at Nimbus.\n\nAlways reply in the thread."; system != want {
		t.Errorf("system = %q, want the messages joined in order", system)
	}
	if len(rest.Messages) != 1 || rest.Messages[0].Role != llm.RoleUser {
		t.Fatalf("rest = %+v, want only the user message", rest.Messages)
	}
	prompt, err := harness.RenderPrompt(rest)
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

// NOTHING TO LIFT IS NOT AN EMPTY SYSTEM PROMPT. A harness handed an empty one
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
			if system, _ := harness.SplitSystem(llm.Request{Messages: tc.msgs}); system != "" {
				t.Errorf("system = %q, want empty", system)
			}
		})
	}
}

// THE SHIPPED CLAUDE PROFILE USES THE FILE VARIANTS, NOT THE INLINE ONES, on
// both of its channels.
//
// A seat's system prompt carries the org chart, its policies, its roster and
// its own memory. On argv that is readable by every account on the machine
// through /proc/<pid>/cmdline, and bounded by ARG_MAX. Swap {file} for
// {system} on either channel and this fails.
func TestTheClaudeProfileKeepsTheSystemPromptOffArgv(t *testing.T) {
	t.Parallel()
	profile, ok := harnessprofile.Builtin("claude-code")
	if !ok {
		t.Fatal("no built-in claude-code profile")
	}
	for _, tc := range []struct {
		field     string
		args      []string
		flag      string
		replacing bool
	}{
		{"system_prompt", profile.SystemPrompt.Args, "--system-prompt-file", true},
		{"append_system_prompt", profile.AppendSystemPrompt.Args, "--append-system-prompt-file", false},
	} {
		if len(tc.args) == 0 {
			t.Errorf("the claude-code profile carries no %s.args, so the calls that take "+
				"that channel deliver the seat's identity as an ordinary user message", tc.field)
			continue
		}
		joined := strings.Join(tc.args, " ")
		if !strings.Contains(joined, "{file}") {
			t.Errorf("%s.args = %v, want a {file} substitution: {system} puts the "+
				"company's org chart and the seat's memory on argv", tc.field, tc.args)
		}
		if strings.Contains(joined, "{system}") {
			t.Errorf("%s.args = %v puts the prompt text on argv", tc.field, tc.args)
		}
		if !slices.Contains(tc.args, tc.flag) {
			t.Errorf("%s.args = %v, want %s", tc.field, tc.args, tc.flag)
		}
		// Each channel takes its own flag: an appending one on the replacing
		// channel would keep the coding-agent prompt on a side pass that
		// promised to drop it, and a replacing one on the appending channel
		// would drop the vendor's prompt a seat was meant to keep.
		if appends := strings.Contains(joined, "--append"); appends == tc.replacing {
			t.Errorf("%s.args = %v, want the %s flag", tc.field, tc.args,
				map[bool]string{true: "replacing", false: "appending"}[tc.replacing])
		}
	}
}

// envChannel marks a harness that reads its system prompt from a FILE named by an
// environment variable rather than from argv — a third channel, and the one
// `--help` never shows.
const envChannel = "{env}"

// discoveredChannel marks a harness that FINDS its system prompt in a file at a
// place of its own choosing in the working directory, and is told on argv
// only which of what it found to use — a fourth channel, and one with no
// placeholder at all.
const discoveredChannel = "{discovered}"

// EVERY SHIPPED PROFILE'S REPLACING CHANNEL IS SUBSTITUTABLE, and says which
// carrier it chose.
//
// A placeholder the code does not substitute is the failure mode with no
// symptom: the harness is handed the literal "{file}" as its system prompt, takes
// it without complaint, and every seat on that provider runs with a one-word
// identity. The two spellings are the whole vocabulary [systemArgs] knows.
//
// The table is the SHIPPED set, so adopting a flag for a new harness has to come
// through here — which is where the argv trade gets stated rather than
// stumbled into. It records system_prompt, the channel that replaces the
// harness's own prompt; the one-shape rules below hold both channels.
func TestEveryProfileWithASystemPromptChannelNamesASubstitutionWeMake(t *testing.T) {
	t.Parallel()
	// Checked against each harness's own --help — or, where the vendor
	// documents its flags rather than printing them all, against that — at
	// the version named in docs/concepts/llm-backends.md.
	// Seven of the twelve have no such flag at all — two of those read a
	// file named by an environment variable instead, and one discovers an
	// agent file the working directory holds — and an empty entry here is
	// that fact rather than an omission.
	want := map[string]string{
		"claude-code": "{file}", // 2.1.303 and 2.1.310: --system-prompt-file
		// 1.0.70, xAI's OWN HARNESS from x.ai/cli:
		// --system-prompt-override, string only — a path there is sent as
		// the text — and --rules the same on the appending channel. NOT
		// the same-named community package on npm, which has no such
		// flag — both put a `grok` on PATH and checking the wrong one
		// answers the wrong question.
		"grok": "{system}",
		// A FILE NAMED BY AN ENV VAR, which is the channel `--help` does
		// not show: GEMINI_SYSTEM_MD / QWEN_SYSTEM_MD. Both were once
		// recorded here as having no system-prompt channel at all,
		// because only their flags had been read.
		"gemini-cli": envChannel,
		"qwen-code":  envChannel,
		// 0.153.4 and 0.164.0: no flag, but a CONFIG KEY naming a file,
		// `-c model_instructions_file=<path>`, which replaces the base
		// instructions outright.
		"codex": "{file}",
		// 1.0.103: no replacing channel; it keeps its own prompt and adds
		// the engine's from an AGENTS.md in the call's directory
		// (append_system_prompt).
		"copilot": "",
		// 2026.09.10 and 2026.10.22: Cursor's own service assembles the
		// system prompt, and the run request carries none.
		"cursor-agent": "",
		// 2.0.32 and 2.1.4: no flag and no variable, but a DISCOVERED
		// AGENT — `agents/<name>.md` in its configuration directory, which
		// the profile points inside the per-call directory, its body
		// replacing the harness's own prompt — which `--agent` selects. Its
		// append channel is the AGENTS.md of the same directory, which the
		// default agent reads on every run.
		"opencode": discoveredChannel,
		// No per-call channel that REPLACES, on 1.0.3 or 1.4.13:
		// run.system_prompt is in the seat's settings file, which its
		// concurrent calls share, and 1.4's `-c` would put the text on
		// argv. Its APPEND channel is a file an undocumented variable
		// names (TBH_EVAL_APPEND_SYSTEM_PROMPT_FILE), which a side pass
		// takes too.
		"muse-code": "",
		// 0.42.0 and 2.3.1, MoonshotAI's OWN HARNESS from
		// @moonshot-ai/kimi-code: --agent-file, a path — but an AGENT
		// FILE rather than a bare prompt, which is what a channel's file
		// spec exists for, and the appending channel is the same file
		// with the vendor's `${base_prompt}` after the text. Not the
		// unscoped `kimi-code` package on npm, which is a Claude Code
		// wrapper and has none of this.
		"kimi-code": "{file}",
		// No channel, measured on 0.21.5. Its identity slot, SOUL.md,
		// lives in the harness's home, which a seat's concurrent calls share,
		// and a .hermes.md in the call's directory — the one per-call file
		// it reads — is replaced whole by a "[BLOCKED: …]" line, silently,
		// when ordinary company prose matches its scan. `-p` on this harness
		// selects a PROFILE, not a prompt.
		"hermes": "",
		// 0.85.1 and 1.4.0: --system-prompt, which reads its value as a
		// path when a file exists there — and sends the value itself as
		// the prompt when none does, silently, so the file has to be
		// written before the harness starts. --append-system-prompt reads it
		// the same way.
		"pi": "{file}",
	}
	// THE TABLE IS THE SHIPPED SET. Without this, a profile added later is
	// simply absent from the map and covered by nothing — the failure is
	// that adopting a system-prompt flag for a new harness never has to state
	// the argv trade here, which is the whole reason the table exists.
	for _, name := range harnessprofile.BuiltinNames() {
		if name == "custom" {
			// Ships nothing on purpose; an operator declares its
			// channel, and there is no built-in claim to check.
			continue
		}
		if _, ok := want[name]; !ok {
			t.Errorf("the %q profile ships but this table does not say which "+
				"system-prompt channel its harness has", name)
		}
	}
	for name, placeholder := range want {
		profile, ok := harnessprofile.Builtin(name)
		if !ok {
			t.Errorf("no built-in %q profile", name)
			continue
		}
		// ONE CARRIER PER CHANNEL, ever, on both: a channel naming an
		// argv and a variable would hand the harness the prompt twice, and
		// one naming {file} and {system} would write the prompt to disk
		// and then put it on argv as well.
		for field, ch := range map[string]harnessprofile.Channel{
			"system_prompt":        profile.SystemPrompt,
			"append_system_prompt": profile.AppendSystemPrompt,
		} {
			joined := strings.Join(ch.Args, " ")
			if joined != "" && ch.Env != "" {
				t.Errorf("%s declares BOTH %s.args and %s.env", name, field, field)
			}
			if strings.Contains(joined, "{file}") && strings.Contains(joined, "{system}") {
				t.Errorf("%s %s.args = %v names both {file} and {system}", name, field, ch.Args)
			}
		}
		ch := profile.SystemPrompt
		joined := strings.Join(ch.Args, " ")
		switch placeholder {
		case envChannel:
			if ch.Env == "" {
				t.Errorf("%s carries no system_prompt.env, so its seats' identities "+
					"go in the transcript although its harness reads a file", name)
			}
			if joined != "" {
				t.Errorf("%s puts the prompt on argv although its harness reads a file: %v",
					name, ch.Args)
			}
		case discoveredChannel:
			if ch.File == nil || ch.File.Discovered != harnessprofile.DiscoverySelected || ch.File.Name == "" {
				t.Errorf("%s discovers an agent file and selects it, but its "+
					"system_prompt.file is not a selected one with a name: %+v", name, ch.File)
				continue
			}
			if joined == "" || strings.Contains(joined, "{file}") || strings.Contains(joined, "{system}") {
				t.Errorf("%s system_prompt.args = %v, want the argv that selects the "+
					"discovered file and nothing that carries it", name, ch.Args)
			}
		case "":
			if ch.Declared() {
				t.Errorf("%s declares system_prompt = %+v, but its harness has no such "+
					"channel — the text would reach it as a literal argument, or "+
					"nowhere", name, ch)
			}
		default:
			if joined == "" {
				t.Errorf("%s carries no system_prompt.args, so every seat's identity "+
					"is delivered as an ordinary user message", name)
				continue
			}
			if !strings.Contains(joined, placeholder) {
				t.Errorf("%s system_prompt.args = %v, want a %s substitution",
					name, ch.Args, placeholder)
			}
		}
	}
}

// THE FILE IS PRIVATE AND LANDS IN THE PER-CALL DIRECTORY, which is created
// empty for one call and removed on release — so the text cannot outlive the
// call that needed it or reach the next one.
func TestSystemPromptFileIsPrivateToTheCall(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	args, err := harness.SystemArgsForTest(
		harnessprofile.Channel{Args: []string{"--system-prompt-file", "{file}"}}, "the seat's whole identity", dir)
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
