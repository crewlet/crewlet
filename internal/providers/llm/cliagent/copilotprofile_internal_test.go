package cliagent

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/cliagent/cliprofile"
)

// The invariants the built-in copilot profile keeps, each measured against
// GitHub Copilot CLI 1.0.103 (see the profile's comments): a call offers the
// web tools and nothing else, the transcript reaches the CLI on stdin, every
// call's system prompt is an AGENTS.md in its own directory under the
// vendor's prompt, the usage report is charged once, and nothing a call
// leaves in the seat's home outlives the calls that share it.

// copilot is the shipped profile, which every case here reads.
func copilot(t *testing.T) cliprofile.Profile {
	t.Helper()
	p, ok := cliprofile.Builtin("copilot")
	if !ok {
		t.Fatal("no built-in copilot profile")
	}
	return p
}

// eachValueOf returns the value after every occurrence of flag in argv.
func eachValueOf(argv []string, flag string) []string {
	var out []string
	for i, arg := range argv {
		if arg == flag && i+1 < len(argv) {
			out = append(out, argv[i+1])
		}
	}
	return out
}

// A CALL OFFERS THE WEB TOOLS AND NOTHING ELSE, and prints only the answer.
// `--available-tools` names the tools that exist for the call; with only the
// deny list, 21 tools were offered and refused only once called. Each of these
// flags takes a LIST, so every value stands right after a flag of its own: a
// second value after one flag would be read into its list, and a stray one
// into whichever list came last.
func TestCopilotOffersTheWebToolsAndNothingElse(t *testing.T) {
	t.Parallel()
	p := copilot(t)
	web := []string{"web_fetch", "web_search"}
	for _, flag := range []string{"--available-tools", "--allow-tool"} {
		got := eachValueOf(p.CompleteArgs, flag)
		slices.Sort(got)
		if !slices.Equal(got, web) {
			t.Errorf("%s names %q, want exactly %q", flag, got, web)
		}
	}
	denied := eachValueOf(p.CompleteArgs, "--deny-tool")
	for _, kind := range []string{"shell", "write", "read"} {
		if !slices.Contains(denied, kind) {
			t.Errorf("--deny-tool names %q, want %s among them", denied, kind)
		}
	}
	for i, arg := range p.CompleteArgs {
		if !slices.Contains([]string{"--available-tools", "--allow-tool", "--deny-tool"}, arg) {
			continue
		}
		if i+2 < len(p.CompleteArgs) && !strings.HasPrefix(p.CompleteArgs[i+2], "-") {
			t.Errorf("complete_args = %q: %s at %d is followed by two values, and takes both "+
				"into its list", p.CompleteArgs, arg, i)
		}
	}
	for _, want := range []string{"-s", "--disable-client-telemetry"} {
		if !slices.Contains(p.CompleteArgs, want) {
			t.Errorf("complete_args = %q, want %s", p.CompleteArgs, want)
		}
	}
	// `--no-custom-instructions` would switch off the AGENTS.md the append
	// channel writes; `-p` takes the prompt as its value and swallowed the
	// model flag that followed it.
	for _, never := range []string{"--no-custom-instructions", "-p", "--prompt"} {
		if slices.Contains(p.CompleteArgs, never) {
			t.Errorf("complete_args = %q carries %s", p.CompleteArgs, never)
		}
	}
}

// copilotProvider is the shipped profile driven against the fake CLI.
func copilotProvider(t *testing.T, env map[string]string) *Provider {
	t.Helper()
	dir := t.TempDir()
	t.Cleanup(func() { forgetWorkspace(dir) })
	p, err := New(Config{
		Key: "copilot", Model: "claude-sonnet-4.5", Agent: "copilot", StateDir: dir,
		Overrides: map[string]any{"binary": os.Args[0]},
		Timeout:   20 * time.Second, MaxConcurrent: 1,
		Env:  fakeChildEnv(env),
		Auth: Auth{Mode: AuthSubscription},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p
}

// EVERY CALL'S SYSTEM PROMPT IS AN AGENTS.md IN ITS OWN DIRECTORY, under the
// CLI's own prompt — a seat's and a side pass's alike, since the CLI has no way
// to replace its prompt — opened by the note that says which of the two is the
// role, private, and absent from argv. Asked from inside the child, because the
// per-call directory is removed on release.
func TestCopilotReadsEveryCallsSystemPromptFromItsAgentsMd(t *testing.T) {
	t.Parallel()
	const identity = "You are Agent PM at Nimbus."
	for name, agent := range map[string]bool{"a seat at work": true, "a side pass": false} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p := copilotProvider(t, map[string]string{"FAKE_READ_DISCOVERED": "AGENTS.md"})
			comp, err := ask(t, p, llm.Request{Agent: agent, Messages: []llm.Message{
				{Role: llm.RoleSystem, Content: identity},
				{Role: llm.RoleUser, Content: "status?"},
			}})
			if err != nil {
				t.Fatalf("Complete: %v", err)
			}
			parts := strings.SplitN(comp.Content, "|", 3)
			if len(parts) != 3 {
				t.Fatalf("the child found no AGENTS.md in its directory: %s", comp.Content)
			}
			argv, mode, encoded := parts[0], parts[1], parts[2]
			body, err := base64.StdEncoding.DecodeString(encoded)
			if err != nil {
				t.Fatal(err)
			}
			if want := harnessNote + "\n\n" + identity; string(body) != want {
				t.Errorf("AGENTS.md holds %q, want %q", body, want)
			}
			if mode != "600" {
				t.Errorf("mode = %s, want 600: the file is the org chart and the seat's memory", mode)
			}
			if strings.Contains(argv, "Agent PM") {
				t.Errorf("argv = %q carries the system prompt", argv)
			}
		})
	}
}

// THE TRANSCRIPT REACHES THE CLI ON STDIN, which prompt mode reads to its end
// when no -p is given: off argv, where ARG_MAX bounds it and every account on
// the machine can read it. The system prompt is not in it — it is the AGENTS.md
// above, and sending it twice would hand the model its role as a user message
// too.
func TestCopilotTakesTheTranscriptOnStdin(t *testing.T) {
	t.Parallel()
	rec := t.TempDir()
	p := copilotProvider(t, map[string]string{"FAKE_ECHO_STDIN": "1", "FAKE_RECORD_ARGV": rec})
	comp, err := ask(t, p, llm.Request{Agent: true, Messages: []llm.Message{
		{Role: llm.RoleSystem, Content: "You are Agent PM at Nimbus."},
		{Role: llm.RoleUser, Content: "what is the status of the launch?"},
	}})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	stdin, err := base64.StdEncoding.DecodeString(strings.TrimSpace(comp.Content))
	if err != nil {
		t.Fatalf("the child echoed %q: %v", comp.Content, err)
	}
	if !strings.Contains(string(stdin), "what is the status of the launch?") {
		t.Errorf("stdin = %q, want the transcript", stdin)
	}
	if strings.Contains(string(stdin), "Agent PM") {
		t.Errorf("stdin = %q carries the system prompt as well as the AGENTS.md", stdin)
	}
	for _, arg := range completionArgv(t, rec) {
		if strings.Contains(arg, "status of the launch") || strings.Contains(arg, "Agent PM") {
			t.Errorf("argv carries %q", arg)
		}
	}
}

// THE USAGE REPORT IS CHARGED ONCE, ITS CACHED READS REPORTED. The CLI's input
// count already holds the cached tokens (measured: 1,000 prompt tokens of which
// 600 cached were recorded as inputTokens 1000 and cacheReadTokens 600), so the
// 600 are reported as cache reads and not added to the input a second time.
// The report is the CLI's own --usage-output-file, verbatim in the shape
// 1.0.103 wrote it.
func TestCopilotChargesItsUsageReportOnce(t *testing.T) {
	t.Parallel()
	const report = `{"totalPremiumRequestCost":0,"totalUserRequests":1,` +
		`"modelMetrics":{"claude-sonnet-4.5":{"requests":{"count":1,"cost":0},` +
		`"usage":{"inputTokens":1000,"outputTokens":10,"cacheReadTokens":600,"cacheWriteTokens":0}}},` +
		`"providerModelMetrics":[{"provider":null,"modelId":"claude-sonnet-4.5",` +
		`"metrics":{"requests":{"count":1,"cost":0},` +
		`"usage":{"inputTokens":1000,"outputTokens":10,"cacheReadTokens":600,"cacheWriteTokens":0}}}],` +
		`"currentModel":"claude-sonnet-4.5","lastCallInputTokens":1000,"lastCallOutputTokens":10}`
	p := copilot(t)
	if !p.ReadsUsageReport() {
		t.Fatal("the copilot profile reads no usage report, so every call is charged an estimate")
	}
	prov := &Provider{profile: p, model: "claude-sonnet-4.5", now: time.Now}
	comp, err := prov.completion(t.Context(), "prompt", &rawResult{stdout: "ok\n"}, report)
	if err != nil {
		t.Fatalf("completion: %v", err)
	}
	if comp.InputTokens != 1000 || comp.OutputTokens != 10 || comp.CacheRead != 600 {
		t.Errorf("charged in %d out %d cache read %d, want in 1000 out 10 cache read 600 — the "+
			"cached 600 are inside the 1000", comp.InputTokens, comp.OutputTokens, comp.CacheRead)
	}
	if comp.Content != "ok" {
		t.Errorf("content = %q, want the answer stdout carried", comp.Content)
	}
}

// NOTHING A CALL LEAVES IN THE SEAT'S HOME OUTLIVES ITS GENERATION: the
// session directory holding the call's events, and the session index every
// session is written to, with its journal, write-ahead log and shared memory —
// all measured on 1.0.103 — go with the prune.
func TestCopilotPrunesWhatACallLeaves(t *testing.T) {
	t.Parallel()
	p := copilot(t)
	dir := t.TempDir()
	t.Cleanup(func() { forgetWorkspace(dir) })
	ws, err := Shared(dir, "copilot", p)
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	left := []string{
		".copilot/session-state/7200a8e5-4ad7-4c15-8f7a-d3217211d1b2/events.jsonl",
		".copilot/session-store.db",
		".copilot/session-store.db-journal",
		".copilot/session-store.db-wal",
		".copilot/session-store.db-shm",
		".copilot/logs/process-1792894077341-21027.log",
	}
	for _, rel := range left {
		path := filepath.Join(home, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := ws.prune(home); err != nil {
		t.Fatalf("prune: %v", err)
	}
	for _, rel := range left {
		if _, err := os.Stat(filepath.Join(home, rel)); !os.IsNotExist(err) {
			t.Errorf("%s outlived the prune (%v): the next call of the seat would find it", rel, err)
		}
	}
}

// NO CALL UPDATES THE CLI: a standalone install downloads a newer CLI into
// each seat's own cache and runs that instead of the one installed, unless
// told not to — on every command the profile runs, a login included.
func TestCopilotCallsDoNotUpdateTheCLI(t *testing.T) {
	t.Parallel()
	env := childEnv(copilot(t), &Checkout{Home: t.TempDir(), Cache: t.TempDir()}, nil, Auth{})
	if got := env["COPILOT_AUTO_UPDATE"]; got != "false" {
		t.Errorf("COPILOT_AUTO_UPDATE = %q, want false", got)
	}
}

// A CLI THAT CANNOT USE ITS LOGIN NAMES THE LOGIN AS WHAT FAILED, in its own
// words — what 1.0.103 printed signed out, and the refusal its bundle carries
// for a credential the service will not take — so the seat says its login is
// what failed rather than reporting a server error.
func TestCopilotNamesALoginItCannotUse(t *testing.T) {
	t.Parallel()
	p := copilot(t)
	for name, stderr := range map[string]string{
		"signed out": "Error: No authentication information found.\n\nCopilot can be " +
			"authenticated with GitHub using an OAuth Token or a Fine-Grained Personal Access Token.\n\n" +
			"To authenticate, you can use any of the following methods:\n" +
			"  • Start 'copilot' and run the '/login' command\n" +
			"  • Set the COPILOT_GITHUB_TOKEN, GH_TOKEN, or GITHUB_TOKEN environment variable\n" +
			"  • Run 'gh auth login' to authenticate with the GitHub CLI\n",
		"a refused credential": "Error: Authentication failed due to invalid credentials\n",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			hit, ok := classifyMarkers(p, cliOutput{stderr: stderr, failed: true})
			if !ok || hit.Kind != llm.KindAuth {
				t.Errorf("classified %+v (matched %v), want the login named for:\n%s", hit, ok, stderr)
			}
		})
	}
}
