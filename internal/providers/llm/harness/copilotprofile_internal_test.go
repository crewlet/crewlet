package harness

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/harness/harnessprofile"
)

// The invariants the built-in copilot profile keeps, each measured against
// GitHub Copilot CLI 1.0.114 (see the profile's comments): a call offers the
// web tools and nothing else, a call with the web denied none, the transcript
// reaches the harness on stdin, every
// call's system prompt is an AGENTS.md in its own directory under the
// vendor's prompt, the usage report is charged once, and nothing a call
// leaves in the seat's home outlives the calls that share it.

// copilot is the shipped profile, which every case here reads.
func copilot(t *testing.T) harnessprofile.Profile {
	t.Helper()
	p, ok := harnessprofile.Builtin("copilot")
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
// deny list, 21 tools were offered and refused only once called. A fetch asks
// a URL permission prompt mode cannot answer, so `--allow-all-urls` is what
// lets web_fetch run. Each of these flags takes a LIST, so every value stands
// right after a flag of its own: a second value after one flag would be read
// into its list, and a stray one into whichever list came last.
func TestCopilotOffersTheWebToolsAndNothingElse(t *testing.T) {
	t.Parallel()
	p := copilot(t)
	got := eachValueOf(p.CompleteArgs, "--available-tools")
	slices.Sort(got)
	if web := []string{"web_fetch", "web_search"}; !slices.Equal(got, web) {
		t.Errorf("--available-tools names %q, want exactly %q", got, web)
	}
	if got := eachValueOf(p.CompleteArgs, "--allow-tool"); !slices.Equal(got, []string{"web_search"}) {
		t.Errorf("--allow-tool names %q, want web_search alone", got)
	}
	if !slices.Contains(p.CompleteArgs, "--allow-all-urls") {
		t.Errorf("complete_args = %q: without --allow-all-urls every fetch is denied", p.CompleteArgs)
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

// A CALL WITH THE WEB DENIED CANNOT FETCH AT ALL: web_fetch is not among the
// tools that exist for it, so no URL allowance anywhere reaches it (measured
// on 1.0.114: "Tool 'web_fetch' does not exist" with every URL allowed). The
// allowlist names `crewlet` and nothing of the harness's own: without one, or
// with an empty one, all 21 tools are offered. Nothing rests on --deny-tool,
// which does not stop a fetch.
//
// Mutation: keep web_fetch in the denied --available-tools, or drop the
// allowlist from it, and this fails.
func TestCopilotWithTheWebDeniedCannotFetch(t *testing.T) {
	t.Parallel()
	p, err := harnessprofile.Load("copilot", map[string]any{"web_tools": "denied"})
	if err != nil {
		t.Fatalf("Load with web_tools denied: %v", err)
	}
	argvs := [][]string{p.CompleteArgs}
	if p.MCP != nil {
		argvs = append(argvs, slices.Concat(p.CompleteArgs, p.MCP.Args, p.MCP.AllowArgs))
	}
	for _, argv := range argvs {
		available := eachValueOf(argv, "--available-tools")
		if len(available) == 0 || slices.Contains(available, "") {
			t.Errorf("argv %q has no allowlist, so every tool the harness has is offered", argv)
		}
		for _, tool := range available {
			if tool != "crewlet" {
				t.Errorf("argv %q makes %q available beside the engine's tools", argv, tool)
			}
		}
		for _, flag := range []string{"--allow-all-urls", "--allow-url", "--yolo", "--allow-all"} {
			if slices.Contains(argv, flag) {
				t.Errorf("argv %q carries %s", argv, flag)
			}
		}
	}
}

// copilotProvider is the shipped profile driven against the fake harness.
func copilotProvider(t *testing.T, env map[string]string) *Provider {
	t.Helper()
	dir := t.TempDir()
	t.Cleanup(func() { forgetWorkspace(dir) })
	p, err := New(Config{
		Key: "copilot", Model: "claude-sonnet-4.5", Harness: "copilot", StateDir: dir,
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
// harness's own prompt — a seat's and a side pass's alike, since the harness has no way
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

// THE TRANSCRIPT REACHES THE HARNESS ON STDIN, which prompt mode reads to its end
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

// THE USAGE REPORT IS CHARGED ONCE, ITS CACHED READS REPORTED. The harness's input
// count already holds the cached tokens (measured: 1,000 prompt tokens of which
// 600 cached were recorded as inputTokens 1000 and cacheReadTokens 600), so the
// 600 are reported as cache reads and not added to the input a second time.
// Its cache writes are inside the input as well, and reported apart. The
// report is the harness's own --usage-output-file, verbatim as 1.0.114 wrote
// it for one request to a stand-in model reporting 11 tokens written.
func TestCopilotChargesItsUsageReportOnce(t *testing.T) {
	t.Parallel()
	const report = `{"totalPremiumRequestCost":0,"totalUserRequests":1,"totalNanoAiu":0,` +
		`"aiCreditsStatus":"unavailable","totalApiDurationMs":20,"sessionStartTime":"2026-11-22T09:18:07.319Z",` +
		`"codeChanges":{"linesAdded":0,"linesRemoved":0,"filesModifiedCount":0,"filesModified":[]},` +
		`"modelMetrics":{"stub-1":{"requests":{"count":1,"cost":0},` +
		`"usage":{"inputTokens":1000,"outputTokens":10,"cacheReadTokens":600,"cacheWriteTokens":11},` +
		`"totalNanoAiu":0,"aiCreditsStatus":"unavailable"}},` +
		`"agentMetrics":{"main":{"totalApiDurationMs":20,"totalNanoAiu":0,"modelMetrics":{"stub-1":` +
		`{"requests":{"count":1,"cost":0},` +
		`"usage":{"inputTokens":1000,"outputTokens":10,"cacheReadTokens":600,"cacheWriteTokens":11},` +
		`"totalNanoAiu":0}}}},` +
		`"providerModelMetrics":[{"provider":null,"modelId":"stub-1","metrics":{"requests":{"count":1,"cost":0},` +
		`"usage":{"inputTokens":1000,"outputTokens":10,"cacheReadTokens":600,"cacheWriteTokens":11},` +
		`"totalNanoAiu":0,"aiCreditsStatus":"unavailable"},"modelDisplayName":"stub-1"}],` +
		`"currentModel":"stub-1","lastCallInputTokens":1000,"lastCallOutputTokens":10}`
	p := copilot(t)
	if !p.ReadsUsageReport() {
		t.Fatal("the copilot profile reads no usage report, so every call is charged an estimate")
	}
	prov := &Provider{profile: p, model: "stub-1", now: time.Now}
	comp, err := prov.completion(t.Context(), "prompt", &rawResult{stdout: "ok\n"}, report)
	if err != nil {
		t.Fatalf("completion: %v", err)
	}
	if comp.InputTokens != 1000 || comp.OutputTokens != 10 || comp.CacheRead != 600 || comp.CacheWrite != 11 {
		t.Errorf("charged in %d out %d cache read %d write %d, want in 1000 out 10 cache read 600 write 11: "+
			"the cached 600 and the 11 written are inside the 1000",
			comp.InputTokens, comp.OutputTokens, comp.CacheRead, comp.CacheWrite)
	}
	if comp.Content != "ok" {
		t.Errorf("content = %q, want the answer stdout carried", comp.Content)
	}
}

// NOTHING A CALL LEAVES IN THE SEAT'S HOME OUTLIVES ITS GENERATION: the
// session directory holding the call's events, the session index every
// session is written to with its write-ahead log and shared memory, the
// empty installed-plugins directory and settings.json.lock, all measured on
// 1.0.114, go with the prune.
//
// Mutation: drop installed-plugins or settings.json.lock from volatile_paths,
// and it outlives the prune.
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
		".copilot/session-store.db-wal",
		".copilot/session-store.db-shm",
		".copilot/logs/process-1792894077341-21027.log",
		".copilot/settings.json.lock",
	}
	if err := os.MkdirAll(filepath.Join(home, ".copilot", "installed-plugins"), 0o700); err != nil {
		t.Fatal(err)
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
	for _, rel := range append(left, ".copilot/installed-plugins") {
		if _, err := os.Stat(filepath.Join(home, rel)); !os.IsNotExist(err) {
			t.Errorf("%s outlived the prune (%v): the next call of the seat would find it", rel, err)
		}
	}
}

// COPILOT HAS NO LOGOUT COMMAND (`copilot logout` exits 1 on "Invalid command
// format", measured on 1.0.114), so the profile declares none and `crewlet llm
// logout` removes the login's files rather than reporting a failed command.
//
// Mutation: give the copilot profile logout_args, and this fails.
func TestCopilotDeclaresNoLogoutCommand(t *testing.T) {
	t.Parallel()
	if got := copilot(t).LogoutArgs; len(got) > 0 {
		t.Errorf("logout_args = %q: copilot has no logout command", got)
	}
}

// NO CALL UPDATES THE HARNESS: a standalone install downloads a newer harness into
// each seat's own cache and runs that instead of the one installed, unless
// told not to — on every command the profile runs, a login included.
func TestCopilotCallsDoNotUpdateTheHarness(t *testing.T) {
	t.Parallel()
	env := childEnv(copilot(t), &Checkout{Home: t.TempDir(), Cache: t.TempDir()}, nil, Auth{})
	if got := env["COPILOT_AUTO_UPDATE"]; got != "false" {
		t.Errorf("COPILOT_AUTO_UPDATE = %q, want false", got)
	}
}

// A HARNESS THAT CANNOT USE ITS LOGIN NAMES THE LOGIN AS WHAT FAILED, in its own
// words — what 1.0.114 printed signed out, and the refusals its bundle carries
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
		"a refused credential":    "Authentication failed for this account. Sign in again with /login, then retry.\n",
		"a refusal with a reason": "Error: Authentication failed: the token was rejected\n",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			hit, ok := classifyMarkers(p, harnessOutput{stderr: stderr, failed: true})
			if !ok || hit.Kind != llm.KindAuth {
				t.Errorf("classified %+v (matched %v), want the login named for:\n%s", hit, ok, stderr)
			}
		})
	}
}
