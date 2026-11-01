package harness

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/harness/harnessprofile"
)

// The invariants the built-in claude-code profile keeps, each measured against
// Claude Code 2.1.303 and 2.1.310 (see the profile's comments): a call offers
// the two web tools and nothing else, a seat's phases keep the CLI's own
// system prompt and a side pass replaces it, nothing per call is put ahead of
// a seat's text, and nothing a call leaves in the seat's home outlives the
// calls that share it.

// claudeCode is the shipped profile, which every case here reads.
func claudeCode(t *testing.T) harnessprofile.Profile {
	t.Helper()
	p, ok := harnessprofile.Builtin("claude-code")
	if !ok {
		t.Fatal("no built-in claude-code profile")
	}
	return p
}

// flagValues returns the values that follow flag in argv, up to the next flag,
// split on commas the way the CLI reads a tool list.
func flagValues(argv []string, flag string) ([]string, bool) {
	i := slices.Index(argv, flag)
	if i < 0 {
		return nil, false
	}
	var out []string
	for _, arg := range argv[i+1:] {
		if strings.HasPrefix(arg, "-") {
			break
		}
		for name := range strings.SplitSeq(arg, ",") {
			if name = strings.TrimSpace(name); name != "" {
				out = append(out, name)
			}
		}
	}
	return out, true
}

// A CALL OFFERS THE WEB TOOLS AND NOTHING ELSE. `--tools` names the tools that
// exist for the call; without it, a deny list of the file and shell tools left
// fourteen others on offer — schedulers, worktrees, agent messaging, workflows
// — and some of them ran when the model asked. The allow list has to name the
// same two, or print mode refuses the fetch it never prompts for.
func TestClaudeCodeOffersTheWebToolsAndNothingElse(t *testing.T) {
	t.Parallel()
	p := claudeCode(t)
	web := []string{"WebFetch", "WebSearch"}
	tools, ok := flagValues(p.CompleteArgs, "--tools")
	if !ok {
		t.Fatalf("complete_args = %q carries no --tools, so the CLI offers every tool "+
			"its build ships that the deny list does not name", p.CompleteArgs)
	}
	slices.Sort(tools)
	if !slices.Equal(tools, web) {
		t.Errorf("--tools offers %q, want exactly %q", tools, web)
	}
	allowed, _ := flagValues(p.CompleteArgs, "--allowedTools")
	for _, name := range web {
		if !slices.Contains(allowed, name) {
			t.Errorf("--allowedTools = %q lacks %s: print mode refuses a tool it is not "+
				"allowed to use, so the seat could not fetch", allowed, name)
		}
	}
}

// A SEAT KEEPS THE CLI'S OWN PROMPT AND A SIDE PASS REPLACES IT, each through a
// file, and only the seat's call carries the switch that keeps the working
// directory and the environment out of the system prompt — the replacing
// prompt ignores it, and a side pass has nothing of the vendor's to keep
// stable. Driven through Complete with the shipped profile, so what is checked
// is the argv a call is handed, not the profile's data.
func TestClaudeCodeSeatAppendsAndASidePassReplaces(t *testing.T) {
	t.Parallel()
	const exclude = "--exclude-dynamic-system-prompt-sections"
	for name, tc := range map[string]struct {
		agent      bool
		take, skip string
		exclude    bool
	}{
		"a seat at work": {agent: true, take: "--append-system-prompt-file", skip: "--system-prompt-file", exclude: true},
		"a side pass":    {agent: false, take: "--system-prompt-file", skip: "--append-system-prompt-file"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			rec := t.TempDir()
			dir := t.TempDir()
			t.Cleanup(func() { forgetWorkspace(dir) })
			p, err := New(Config{
				Key: "claude", Model: "claude-opus-5-5", Harness: "claude-code", StateDir: dir,
				Overrides: map[string]any{"binary": os.Args[0]},
				Timeout:   20 * time.Second, MaxConcurrent: 1,
				Env: fakeChildEnv(map[string]string{
					"FAKE_STDOUT":      `{"type":"result","is_error":false,"result":"ok","usage":{"input_tokens":3,"output_tokens":1}}`,
					"FAKE_RECORD_ARGV": rec,
				}),
				Auth: Auth{Mode: AuthSubscription},
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			comp, err := ask(t, p, llm.Request{Agent: tc.agent, Messages: []llm.Message{
				{Role: llm.RoleSystem, Content: "You are Agent PM at Nimbus."},
				{Role: llm.RoleUser, Content: "status?"},
			}})
			if err != nil {
				t.Fatalf("Complete: %v", err)
			}
			if comp.Content != "ok" {
				t.Errorf("content = %q, want the result the CLI printed", comp.Content)
			}
			argv := completionArgv(t, rec)
			path, ok := valueAfter(argv, tc.take)
			if !ok {
				t.Fatalf("argv = %q, want the system prompt on %s", argv, tc.take)
			}
			if !filepath.IsAbs(path) || strings.Contains(path, "Agent PM") {
				t.Errorf("%s carried %q, want the path of the file holding the text", tc.take, path)
			}
			if slices.Contains(argv, tc.skip) {
				t.Errorf("argv = %q also carries %s — the CLI is handed the prompt twice", argv, tc.skip)
			}
			if got := slices.Contains(argv, exclude); got != tc.exclude {
				t.Errorf("argv = %q: %s present = %v, want %v", argv, exclude, got, tc.exclude)
			}
		})
	}
}

// EVERY CALL CARRIES THE VENDOR'S SWITCHES: no connection but the call's own
// (measured: four to the vendor per call without it, none with it, and the
// request unchanged), and no repository status attached to a seat's call,
// which describes the engine's `.git` marker at best and a checkout above the
// state directory at worst.
func TestClaudeCodeCallsCarryTheVendorsSwitches(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	env := childEnv(claudeCode(t), &Checkout{Home: home, Cache: t.TempDir(), Work: t.TempDir()}, nil, Auth{})
	for name, want := range map[string]string{
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1",
		"CLAUDE_CODE_DISABLE_GIT_INSTRUCTIONS":     "1",
	} {
		if got := env[name]; got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
}
