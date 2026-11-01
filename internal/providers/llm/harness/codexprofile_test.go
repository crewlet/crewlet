package harness

import (
	"math"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/providers/llm/harness/harnessprofile"
)

// codexSetting reports whether the profile's argv passes value as a `-c`
// setting: the value, directly after a `-c`.
func codexSetting(args []string, value string) bool {
	for i := 1; i < len(args); i++ {
		if args[i] == value && args[i-1] == "-c" {
			return true
		}
	}
	return false
}

// THE CODEX PROFILE KEEPS THE CLI TO ITS CALL: nothing from above the call's
// directory, nothing kept from one call for the next, and of codex's own tools
// web search and an apply_patch that writes nothing. Each of these was
// measured on 0.153.4 and 0.164.0 to be what removes the thing named; the
// profile's comments carry the measurements.
func TestTheCodexProfileKeepsTheHarnessToItsCall(t *testing.T) {
	t.Parallel()
	p, ok := harnessprofile.Builtin("codex")
	if !ok {
		t.Fatal("no built-in codex profile")
	}
	for _, flag := range []string{"--ephemeral", "--ignore-rules", "--skip-git-repo-check"} {
		if !slices.Contains(p.CompleteArgs, flag) {
			t.Errorf("complete_args = %v, want %s", p.CompleteArgs, flag)
		}
	}
	if i := slices.Index(p.CompleteArgs, "--sandbox"); i < 0 || i+1 >= len(p.CompleteArgs) ||
		p.CompleteArgs[i+1] != "read-only" {
		t.Errorf("complete_args = %v, want --sandbox read-only, so apply_patch writes nothing", p.CompleteArgs)
	}
	for setting, why := range map[string]string{
		"project_root_markers=[]":                             "the call's directory is the project root, so nothing above it is read",
		"skills.include_instructions=false":                   "no skills block",
		"skills.bundled.enabled=false":                        "no bundled skills installed into the seat's home",
		"features.shell_tool=false":                           "no shell",
		"features.unified_exec=false":                         "no shell",
		"features.shell_snapshot=false":                       "no snapshot of the shell kept in the home",
		"features.view_image=false":                           "no image viewer",
		"features.goals=false":                                "no goal tools",
		"features.multi_agent=false":                          "no sub-agents, and no tool_search with them",
		"features.image_generation=false":                     "no image generation",
		"features.apps=false":                                 "no connectors",
		"features.plugins=false":                              "no plugins",
		"features.hooks=false":                                "no hooks",
		"features.memories=false":                             "no memories",
		"tools.experimental_request_user_input.enabled=false": "no question put to a user who is not there",
	} {
		if !codexSetting(p.CompleteArgs, setting) {
			t.Errorf("complete_args = %v, want -c %s (%s)", p.CompleteArgs, setting, why)
		}
	}
	// The databases codex opens on every run hold the thread, so they are the
	// call's own: in its directory, which is removed with it.
	rel, ok := p.WorkConfigEnv["CODEX_SQLITE_HOME"]
	if !ok || !filepath.IsLocal(rel) {
		t.Errorf("work_config_env = %v, want CODEX_SQLITE_HOME inside the call's directory", p.WorkConfigEnv)
	}
}

// A SEAT KEEPS CODEX'S OWN PROMPT AND A SIDE PASS REPLACES IT. The seat's text
// rides the AGENTS.md codex reads from the call's directory, and codex cuts that
// file at project_doc_max_bytes without a word — 32 KiB unless told otherwise,
// measured to cut a 240 KB seat prompt — so the cap is the largest the setting
// takes. The side pass's switches that drop codex's permissions and
// environment blocks belong to the replacing channel alone: a seat keeps them.
func TestTheCodexChannelsKeepOrReplaceItsPromptWhole(t *testing.T) {
	t.Parallel()
	p, _ := harnessprofile.Builtin("codex")
	if f := p.AppendSystemPrompt.File; f == nil || f.Name != "AGENTS.md" || !f.ReadAlways() {
		t.Errorf("append_system_prompt.file = %+v, want an AGENTS.md codex reads on every run", f)
	}
	if want := "project_doc_max_bytes=" + strconv.FormatInt(math.MaxInt64, 10); !codexSetting(p.CompleteArgs, want) {
		t.Errorf("complete_args = %v, want -c %s, so the seat's AGENTS.md is never cut", p.CompleteArgs, want)
	}
	for _, setting := range []string{"include_permissions_instructions=false", "include_environment_context=false"} {
		if !codexSetting(p.SystemPrompt.Args, setting) {
			t.Errorf("system_prompt.args = %v, want -c %s", p.SystemPrompt.Args, setting)
		}
		if slices.Contains(p.CompleteArgs, setting) {
			t.Errorf("complete_args carries %s, which takes codex's own blocks from a seat too", setting)
		}
	}
}

// THE REPLACING FILE'S PATH REACHES CODEX VERBATIM, whatever the state
// directory is called. A `-c` value is TOML, and taken as a literal string only
// where it does not parse: an absolute path never does, so it is passed bare,
// where quoting it would have codex read a backslash in it as an escape.
// Measured on both releases with a state directory named like this one.
func TestCodexIsHandedTheReplacingFileAsAPathItTakesVerbatim(t *testing.T) {
	t.Parallel()
	p, _ := harnessprofile.Builtin("codex")
	dir := filepath.Join(t.TempDir(), `state 'q' "d" #x\y`)
	args, err := systemArgs(p.SystemPrompt, "You are Agent PM at Nimbus.", dir)
	if err != nil {
		t.Fatalf("systemArgs: %v", err)
	}
	want := "model_instructions_file=" + filepath.Join(dir, harnessprofile.DefaultSystemPromptFile)
	if !codexSetting(args, want) {
		t.Errorf("system_prompt renders %q, want -c %q: the path bare and absolute", args, want)
	}
}

// A real `codex exec --json` stream from 0.164.0 (0.153.4 prints the same
// lines), against a stand-in for the Responses API that summarised its
// reasoning and reported 1,000 input tokens, 600 of them cached, and 50 output,
// 20 of them reasoning. Verbatim but for three more reconnect lines like the
// first.
const codexStream = `{"type":"thread.started","thread_id":"01a16ffb-d297-78e0-b60d-9440bada2f0e"}
{"type":"turn.started"}
{"type":"error","message":"Reconnecting... 2/5 (unexpected status 404 Not Found: not found, url: ws://127.0.0.1:21410/v1/responses)"}
{"type":"item.completed","item":{"id":"item_0","type":"error","message":"Falling back from WebSockets to HTTPS transport. unexpected status 404 Not Found: Unknown error, url: ws://127.0.0.1:21410/v1/responses"}}
{"type":"item.completed","item":{"id":"item_1","type":"reasoning","text":"REASONING-SUMMARY-MARKER"}}
{"type":"item.completed","item":{"id":"item_2","type":"agent_message","text":"ok"}}
{"type":"turn.completed","usage":{"input_tokens":1000,"cached_input_tokens":600,"cache_write_input_tokens":0,"output_tokens":50,"reasoning_output_tokens":20}}`

// THE CODEX PROFILE READS THE ANSWER AND NOTHING ELSE, AND CHARGES THE TURN
// ONCE. A reasoning item carries `text` as the answer does, and read by the
// path alone it was spliced into the reply; and codex's input_tokens already
// holds its cached tokens, which a sum with cached_input_tokens charged twice.
func TestTheCodexProfileReadsARealStream(t *testing.T) {
	t.Parallel()
	p, err := harnessprofile.Load("codex", nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	prov := &Provider{profile: p, model: "gpt-5.5", harness: "codex", key: "coding", now: time.Now}
	comp, err := prov.completion(t.Context(), "prompt", &rawResult{stdout: codexStream}, "")
	if err != nil {
		t.Fatalf("completion: %v", err)
	}
	if comp.Content != "ok" {
		t.Errorf("Content = %q, want the agent_message alone", comp.Content)
	}
	// OpenAI's arithmetic: the cached 600 are inside the 1,000, and the 20
	// reasoning tokens inside the 50.
	if comp.InputTokens != 1000 || comp.CacheRead != 600 || comp.OutputTokens != 50 {
		t.Errorf("usage = in %d (cache read %d) out %d, want 1000 (600) and 50",
			comp.InputTokens, comp.CacheRead, comp.OutputTokens)
	}
	if strings.Contains(comp.Content, "REASONING") {
		t.Errorf("the reasoning summary reached the answer: %q", comp.Content)
	}
}
