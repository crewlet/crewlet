package harness

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/providers/llm/harness/harnessprofile"
)

// A subscription seat must not have less reach than the same harness at a
// terminal: web is the one local tool every profile keeps on. The claude
// profile has to ALLOW it, not merely stop denying it — print mode never
// prompts, and an unlisted permission-gated tool is refused.
func TestNoShippedProfileDeniesTheWeb(t *testing.T) {
	t.Parallel()
	for _, name := range harnessprofile.BuiltinNames() {
		p, _ := harnessprofile.Builtin(name)
		for i, arg := range p.CompleteArgs {
			if arg != "--disallowedTools" {
				continue
			}
			for _, denied := range p.CompleteArgs[i+1:] {
				if strings.HasPrefix(denied, "--") {
					break
				}
				if denied == "WebFetch" || denied == "WebSearch" {
					t.Errorf("%s denies %s", name, denied)
				}
			}
		}
		for _, arg := range p.CompleteArgs {
			if strings.Contains(arg, "--excluded-tools") {
				t.Errorf("%s excludes tools with %q — web must stay on", name, arg)
			}
		}
		for _, f := range p.SeedFiles {
			if strings.Contains(f.Content, `"webfetch":"deny"`) ||
				strings.Contains(f.Content, `"websearch":"deny"`) ||
				strings.Contains(f.Content, `"WebFetch(`) && strings.Contains(f.Content, `"deny":[`) &&
					strings.Index(f.Content, `"WebFetch(`) > strings.Index(f.Content, `"deny":[`) {
				t.Errorf("%s seeds a file that denies the web:\n%s", name, f.Content)
			}
		}
	}
	claude, _ := harnessprofile.Builtin("claude-code")
	if !allowsAfterFlag(claude.CompleteArgs, "--allowedTools", "WebFetch", "WebSearch") {
		t.Errorf("claude-code must --allowedTools WebFetch WebSearch: %q", claude.CompleteArgs)
	}
	copilot, _ := harnessprofile.Builtin("copilot")
	joined := strings.Join(copilot.CompleteArgs, " ")
	if !strings.Contains(joined, "--allow-tool web_fetch") || !strings.Contains(joined, "--allow-tool web_search") {
		t.Errorf("copilot must allow its web tools explicitly: %q", copilot.CompleteArgs)
	}
	codex, _ := harnessprofile.Builtin("codex")
	if !strings.Contains(strings.Join(codex.CompleteArgs, " "), `web_search="live"`) {
		t.Errorf("codex must switch its web search to live, or it answers from an offline index: %q",
			codex.CompleteArgs)
	}
}

// A BLANKET TOOL DENIAL IS ONLY HONEST WHERE THE VENDOR SHIPS NO WEB TOOL.
//
// `pi` is the one profile that switches every tool off rather than allowing
// two, and it is correct there for a reason no other vendor has: this harness's
// built-in set is read, bash, powershell, edit, write, grep, find and ls —
// there is no web tool in it to keep. Adopting the same flag on a harness that
// HAS one would cut the web silently, which is the rule this file exists for,
// so the carve-out is named rather than left to whoever reads the argv next.
func TestOnlyTheProfileWithNoWebToolDeniesEveryTool(t *testing.T) {
	t.Parallel()
	for _, name := range harnessprofile.BuiltinNames() {
		p, _ := harnessprofile.Builtin(name)
		for _, arg := range p.CompleteArgs {
			if arg != "--no-tools" && arg != "-nt" {
				continue
			}
			if name != "pi" {
				t.Errorf("%s denies every tool with %q — web is the one local tool "+
					"this backend never denies, and only a harness that ships none "+
					"may switch the lot off", name, arg)
			}
		}
	}
}

// THE PI PROFILE SHUTS EVERY DISCOVERY PATH ITS HARNESS HAS.
//
// This vendor is unusually generous about it — one `--no-*` flag per path —
// and that is exactly why a missing one is easy not to notice: each hole is a
// separate silent admission of something from a host the engine does not
// control, and an extension is EXECUTABLE.
func TestThePiProfileAdmitsNothingFromTheHost(t *testing.T) {
	t.Parallel()
	p, ok := harnessprofile.Builtin("pi")
	if !ok {
		t.Fatal("no built-in pi profile")
	}
	for flag, why := range map[string]string{
		"-p":                    "print mode: answer and exit rather than opening the TUI",
		"--no-tools":            "the tool denial itself",
		"--no-context-files":    "the AGENTS.md / CLAUDE.md walk up to /, which the engine's .git file does not end",
		"--no-extensions":       "extension discovery — an extension executes, and on 1.4.0 so would an mcp.json server",
		"--no-skills":           "skill discovery",
		"--no-prompt-templates": "prompt-template discovery",
		"--no-themes":           "theme discovery",
		"--no-approve":          "project trust, which otherwise falls back to defaultProjectTrust in the seat's settings",
		"--no-session":          "the session store: nothing of one turn reaches the next",
	} {
		if !slices.Contains(p.CompleteArgs, flag) {
			t.Errorf("complete_args = %v, want %s (%s)", p.CompleteArgs, flag, why)
		}
	}
	// --system-prompt REPLACES and --append-system-prompt keeps pi's own
	// prompt in front of the seat's: each channel takes its own flag.
	for field, tc := range map[string]struct {
		args []string
		flag string
	}{
		"system_prompt":        {p.SystemPrompt.Args, "--system-prompt"},
		"append_system_prompt": {p.AppendSystemPrompt.Args, "--append-system-prompt"},
	} {
		if len(tc.args) == 0 || tc.args[0] != tc.flag {
			t.Errorf("%s.args = %v, want %s", field, tc.args, tc.flag)
		}
	}
	// THE TRANSCRIPT ON STDIN, never on argv, where every account on the
	// machine reads it through /proc/<pid>/cmdline and ARG_MAX bounds it.
	if p.EffectivePromptMode() != harnessprofile.PromptStdin || len(p.PromptArgs) > 0 {
		t.Errorf("prompt_mode = %q, prompt_args = %v, want the transcript on stdin "+
			"and nothing on argv to introduce it", p.EffectivePromptMode(), p.PromptArgs)
	}
}

// THE PI PROFILE'S ARGV MUST PARSE, and only the real binary can say so.
//
// Nine flags of a vendor's own spelling, on each system-prompt channel, with
// the transcript on stdin as a call hands it over. Skipped unless a `pi` is on
// PATH; it stops at the harness's own missing-credential failure, which is far
// enough to prove the flags were understood.
func TestThePiProfileArgvParsesAgainstTheRealHarness(t *testing.T) {
	t.Parallel()
	binary, err := exec.LookPath("pi")
	if err != nil {
		t.Skip("no pi on PATH")
	}
	p, _ := harnessprofile.Builtin("pi")
	for field, ch := range map[string]harnessprofile.Channel{
		"system_prompt":        p.SystemPrompt,
		"append_system_prompt": p.AppendSystemPrompt,
	} {
		dir := t.TempDir()
		system := filepath.Join(dir, harnessprofile.DefaultSystemPromptFile)
		if err := os.WriteFile(system, []byte("You are Agent CTO."), 0o600); err != nil {
			t.Fatal(err)
		}
		args := append([]string(nil), p.CompleteArgs...)
		for _, a := range p.ModelArgs {
			args = append(args, strings.ReplaceAll(a, "{model}", "anthropic/claude-sonnet-4.6"))
		}
		for _, a := range ch.Args {
			args = append(args, strings.ReplaceAll(a, "{file}", system))
		}

		cmd := exec.Command(binary, args...)
		cmd.Dir = dir
		cmd.Stdin = strings.NewReader("say hello")
		// PI_CODING_AGENT_DIR and PI_OFFLINE are both the profile's own; see
		// vendorHarnessEnv.
		cmd.Env = vendorHarnessEnv(p, dir, nil)
		combined, _ := cmd.CombinedOutput()
		t.Run(field, func(t *testing.T) { assertArgvReachedAuth(t, args, string(combined)) })
	}
}

// allowsAfterFlag reports whether every want follows flag before the next
// flag begins.
func allowsAfterFlag(args []string, flag string, want ...string) bool {
	for i, arg := range args {
		if arg != flag {
			continue
		}
		seen := map[string]bool{}
		for _, v := range args[i+1:] {
			if strings.HasPrefix(v, "--") {
				break
			}
			seen[v] = true
		}
		for _, w := range want {
			if !seen[w] {
				return false
			}
		}
		return true
	}
	return false
}

// Every shipped profile states what it does about the harness's own tools, and a
// profile that admits them says why — the silent hole this field closes is a
// profile whose isolation was assumed rather than declared.
func TestEveryShippedProfileDeclaresItsLocalToolsStance(t *testing.T) {
	t.Parallel()
	for _, name := range harnessprofile.BuiltinNames() {
		if name == "custom" {
			continue
		}
		p, _ := harnessprofile.Builtin(name)
		if !p.LocalTools.Valid() {
			t.Errorf("%s: local_tools = %q, want denied or vendor-default", name, p.LocalTools)
		}
		if p.LocalTools == harnessprofile.LocalToolsVendorDefault && p.LocalToolsNote == "" {
			t.Errorf("%s: vendor-default with no local_tools_note", name)
		}
	}
	for _, name := range []string{
		"claude-code", "codex", "gemini-cli", "qwen-code", "opencode",
		"cursor-agent", "copilot", "muse-code", "kimi-code", "hermes", "pi",
	} {
		p, _ := harnessprofile.Builtin(name)
		if p.LocalTools != harnessprofile.LocalToolsDenied {
			t.Errorf("%s: local_tools = %q, want denied", name, p.LocalTools)
		}
	}
}

// THE ASSISTANT'S HARNESS HAS NO TOOL OF ITS OWN, BY CONSTRUCTION. Every profile
// the Dashboard Assistant may run on leaves its model no shell, file tool or
// tool server in the exact argv the assistant runs (web denied, as written and
// streamed): checked off the argv, never taken from the profile's claim. The
// set is pinned: Claude Code's empty allow list and pi's --no-tools were
// measured; Codex keeps apply_patch, which has no switch, and OpenCode's
// deny-all policy has never been measured to leave a call offered no tool, so
// both are refused. A seat's argv is untouched by any of it.
func TestTheAssistantArgvLeavesTheHarnessWithNoToolOfItsOwn(t *testing.T) {
	t.Parallel()
	if got, want := harnessprofile.AssistantEligible(), []string{"claude-code", "pi"}; !slices.Equal(got, want) {
		t.Errorf("assistant-eligible profiles = %v, want %v", got, want)
	}
	for _, name := range harnessprofile.AssistantEligible() {
		p, err := harnessprofile.Load(name, map[string]any{"web_tools": "denied"})
		if err != nil {
			t.Fatal(err)
		}
		if problem := p.ToolsOffProblem(); problem != "" {
			t.Errorf("%s: %s", name, problem)
		}
	}
	claude, err := harnessprofile.Load("claude-code", map[string]any{"web_tools": "denied"})
	if err != nil {
		t.Fatal(err)
	}
	if i := slices.Index(claude.CompleteArgs, "--tools"); i < 0 || i+1 >= len(claude.CompleteArgs) ||
		claude.CompleteArgs[i+1] != "" {
		t.Errorf("the assistant's claude-code argv %q does not carry the empty allow list", claude.CompleteArgs)
	}
	seat, err := harnessprofile.Load("claude-code", nil)
	if err != nil {
		t.Fatal(err)
	}
	builtin, _ := harnessprofile.Builtin("claude-code")
	if i := slices.Index(seat.CompleteArgs, "--tools"); !slices.Equal(seat.CompleteArgs, builtin.CompleteArgs) ||
		i < 0 || seat.CompleteArgs[i+1] != "WebFetch,WebSearch" || seat.ToolsOffProblem() == "" {
		t.Errorf("the seats' claude-code argv changed, or lost its web tools: %q", seat.CompleteArgs)
	}
	for _, name := range []string{"codex", "opencode"} {
		p, err := harnessprofile.Load(name, map[string]any{"web_tools": "denied"})
		if err != nil {
			t.Fatal(err)
		}
		if p.ToolsOffProblem() == "" {
			t.Errorf("%s: its run is judged to have no tool of its own, which was never measured", name)
		}
	}
	// AN OVERRIDE THAT BRINGS A TOOL BACK IS CAUGHT ON THE ARGV: a second
	// --tools (the harness takes the last) and a tool server's config.
	for _, extra := range [][]string{{"--tools", "default"}, {"--mcp-config", "servers.json"}} {
		args := append(slices.Clone(claude.CompleteArgs), extra...)
		p, err := harnessprofile.Load("claude-code", map[string]any{"web_tools": "denied",
			"web_denial": map[string]any{"complete_args": toAny(args)}})
		if err != nil {
			t.Fatal(err)
		}
		if p.ToolsOffProblem() == "" {
			t.Errorf("an argv ending %q is judged to have no tool", extra)
		}
	}
}

// The vendors that take their policy from a file get a file that parses and
// says what the profile claims: local tools off, web on.
func TestSeededSettingsFilesAreValidJSONThatKeepsTheWebOn(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		path  string
		scope harnessprofile.SeedScope
		must  []string
		never []string
	}{
		// gemini-cli's tool policy is a policy file, not JSON: see
		// TestGeminiOffersTheWebToolsAndNothingElse.
		// An allowlist of the web tools, every other tool denied by name, and
		// the web tools allowed to run unprompted.
		"qwen-code": {".qwen/settings.json", harnessprofile.SeedHome,
			[]string{
				`"core":["web_fetch","web_search"]`,
				`"allow":["web_fetch","web_search"]`,
				`"run_shell_command"`, `"enter_worktree"`, `"agent"`,
			},
			[]string{`"ask"`}},
		// 2.0's own permissions format, deny-all first: the LAST matching
		// rule wins, and 2.0 allows whatever is not denied, so a list of
		// local tools to deny let its new ones through. In the call's own
		// configuration directory, the only one OpenCode reads.
		"opencode": {".crewlet-config/opencode.json", harnessprofile.SeedWork,
			[]string{
				`"permissions":[{"action":"*","resource":"*","effect":"deny"},`,
				`{"action":"webfetch","resource":"*","effect":"allow"}`,
				`{"action":"websearch","resource":"*","effect":"allow"}`,
			},
			[]string{`"ask"`, `"permission":`}},
		"cursor-agent": {".cursor/cli.json", harnessprofile.SeedWork,
			[]string{`Shell(*)`, `Write(*)`, `WebFetch(*)`}, nil},
	}
	for name, c := range cases {
		p, _ := harnessprofile.Builtin(name)
		var found *harnessprofile.SeedFile
		for i := range p.SeedFiles {
			if p.SeedFiles[i].Path == c.path {
				found = &p.SeedFiles[i]
			}
		}
		if found == nil {
			t.Errorf("%s seeds no %s", name, c.path)
			continue
		}
		if found.Scope() != c.scope {
			t.Errorf("%s: %s scope = %q, want %q", name, c.path, found.Scope(), c.scope)
		}
		var doc map[string]any
		if err := json.Unmarshal([]byte(found.Content), &doc); err != nil {
			t.Errorf("%s: %s is not JSON: %v", name, c.path, err)
		}
		for _, m := range c.must {
			if !strings.Contains(found.Content, m) {
				t.Errorf("%s: %s lacks %q", name, c.path, m)
			}
		}
		for _, n := range c.never {
			if strings.Contains(found.Content, n) {
				t.Errorf("%s: %s must not contain %q — a headless run cannot answer it", name, c.path, n)
			}
		}
	}
}

// A seed file lands where its scope says: home files once per seat
// generation, work files on every call, both rooted so an override cannot
// write outside the seat.
func TestSeedFilesLandInTheirScope(t *testing.T) {
	t.Parallel()
	p := fakeProvider(t, nil, map[string]any{
		"seed_files": []any{
			map[string]any{"path": ".vendor/settings.json", "content": `{"home":true}`},
			map[string]any{"path": ".vendor/project.json", "in": "work", "content": `{"work":true}`},
		},
	})
	co, err := p.ws.Acquire(t.Context(), "seat-a", "call-1", p)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	t.Cleanup(func() { _ = co.Release(context.Background()) })

	home, err := os.ReadFile(filepath.Join(co.Home, ".vendor", "settings.json"))
	if err != nil || string(home) != `{"home":true}` {
		t.Errorf("home seed = %q, %v", home, err)
	}
	work, err := os.ReadFile(filepath.Join(co.Work, ".vendor", "project.json"))
	if err != nil || string(work) != `{"work":true}` {
		t.Errorf("work seed = %q, %v", work, err)
	}
	if _, err := os.Stat(filepath.Join(co.Home, ".vendor", "project.json")); !os.IsNotExist(err) {
		t.Errorf("a work-scoped file was written into the home")
	}
	info, err := os.Stat(filepath.Join(co.Home, ".vendor", "settings.json"))
	if err == nil && info.Mode().Perm() != 0o600 {
		t.Errorf("seed file mode = %o, want 0600", info.Mode().Perm())
	}
}

// A seed path that escapes its scope is refused at validation — prune and
// seed both root their targets, and an override is operator input.
func TestASeedFileMayNotEscapeItsScope(t *testing.T) {
	t.Parallel()
	for _, path := range []string{"../outside.json", "/etc/settings.json"} {
		_, err := New(Config{
			Key: "sub", Harness: "custom", StateDir: t.TempDir(), Timeout: time.Second, MaxConcurrent: 1,
			Overrides: map[string]any{
				"binary": "x", "complete_args": []any{"-p"}, "output": "text", "model_args": []any{},
				"seed_files": []any{map[string]any{"path": path, "content": "{}"}},
			},
		})
		if err == nil || !strings.Contains(err.Error(), "seed_files[0].path") {
			t.Errorf("path %q: err = %v, want a refusal naming seed_files[0].path", path, err)
		}
	}
	_, err := New(Config{
		Key: "sub", Harness: "custom", StateDir: t.TempDir(), Timeout: time.Second, MaxConcurrent: 1,
		Overrides: map[string]any{
			"binary": "x", "complete_args": []any{"-p"}, "output": "text", "model_args": []any{},
			"local_tools": "vendor-default",
		},
	})
	if err == nil || !strings.Contains(err.Error(), "local_tools_note") {
		t.Errorf("vendor-default without a note: err = %v, want a refusal naming local_tools_note", err)
	}
}

// probeProvider is a fake harness that answers the two isolation probes with
// the given replies, under the given stance.
func probeProvider(t *testing.T, stance harnessprofile.LocalTools, shellReply, webReply string) *Provider {
	t.Helper()
	overrides := map[string]any{"local_tools": string(stance)}
	if stance == harnessprofile.LocalToolsVendorDefault {
		overrides["local_tools_note"] = "no denial flag on this fake"
	}
	overrides["version_args"] = []any{"-test.run=NoSuchTest"}
	return fakeProvider(t, map[string]string{
		"FAKE_STDOUT":             `{"message":"","tool_calls":[{"name":"crewlet_smoke","arguments":{"ok":true}}]}`,
		"FAKE_SHELL_PROBE_STDOUT": shellReply,
		"FAKE_WEB_PROBE_STDOUT":   webReply,
	}, overrides)
}

func nowEpoch() string { return strconv.FormatInt(time.Now().Unix(), 10) }

// The evidence both probes turn on: a current epoch means the tool ran; a
// refusal, an old epoch, or a plausible-looking guess does not.
func TestReportsCurrentClockBelievesOnlyTheClock(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_800_000_000, 0)
	cases := map[string]bool{
		"1800000000":                  true,
		"```\n1800000042\n```":        true,
		"The output was: 1799999900.": true,
		"ts=1800000012.345":           true,
		"NO-LOCAL-TOOLS":              false,
		"1700000000":                  false, // a guessed epoch, months out
		"1800000000123":               false, // milliseconds: not what was asked
		"2026":                        false,
		"I cannot run commands here.": false,
		"":                            false,
	}
	for text, want := range cases {
		if got := reportsCurrentClock(text, now); got != want {
			t.Errorf("reportsCurrentClock(%q) = %v, want %v", text, got, want)
		}
	}
}

// A profile that says "denied" and a harness that refused is the healthy case.
func TestDoctorProbesReportADeniedShellAndAReachableWeb(t *testing.T) {
	t.Parallel()
	p := probeProvider(t, harnessprofile.LocalToolsDenied, noLocalToolsReply, "ts="+nowEpoch()+".123")
	d := p.Diagnose(t.Context(), DiagnoseOptions{Smoke: true})
	if !strings.HasPrefix(d.LocalTools, "denied by profile; probe: refused") {
		t.Errorf("LocalTools = %q", d.LocalTools)
	}
	if !strings.HasPrefix(d.Web, "allowed; ok") {
		t.Errorf("Web = %q", d.Web)
	}
	// Not Healthy(): the fake has no usage paths and no login, which are
	// real findings of their own. What this case is about is that neither
	// probe contributed one.
	if problem := probeProblem(d); problem != "" {
		t.Errorf("a probe reported a problem on the healthy path: %s", problem)
	}
}

// probeProblem returns the first problem either isolation probe raised, or
// "". The other checks in a diagnosis are not this file's subject.
func probeProblem(d Diagnosis) string {
	for _, p := range d.Problems {
		if strings.Contains(p, "shell command") || strings.Contains(p, "could not fetch") ||
			strings.Contains(p, "engine user") {
			return p
		}
	}
	return ""
}

// A profile that says "denied" and a harness that ran the command is the finding
// the probe exists for: the vendor's flag is not taking effect on this build.
func TestDoctorReportsAShellThatRanDespiteTheProfile(t *testing.T) {
	t.Parallel()
	p := probeProvider(t, harnessprofile.LocalToolsDenied, nowEpoch()+"\n", "ts="+nowEpoch())
	d := p.Diagnose(t.Context(), DiagnoseOptions{Smoke: true})
	if !strings.Contains(d.LocalTools, "SHELL RAN") {
		t.Errorf("LocalTools = %q", d.LocalTools)
	}
	joined := strings.Join(d.Problems, "\n")
	if !strings.Contains(joined, "not taking effect") {
		t.Errorf("the problem does not say the denial is not holding:\n%s", joined)
	}
}

// A vendor-default profile whose harness ran the command is reported as what it
// is — a harness with a shell on the engine host — with the note the profile gave.
func TestDoctorReportsAVendorDefaultShellHonestly(t *testing.T) {
	t.Parallel()
	p := probeProvider(t, harnessprofile.LocalToolsVendorDefault, nowEpoch(), "ts="+nowEpoch())
	d := p.Diagnose(t.Context(), DiagnoseOptions{Smoke: true})
	if !strings.HasPrefix(d.LocalTools, "vendor default (no denial flag on this fake); probe: SHELL RAN") {
		t.Errorf("LocalTools = %q", d.LocalTools)
	}
	joined := strings.Join(d.Problems, "\n")
	if !strings.Contains(joined, "engine user") || !strings.Contains(joined, "no denial flag on this fake") {
		t.Errorf("the problem does not name the trust the operator is taking on:\n%s", joined)
	}
}

// Web is meant to stay on; a harness that cannot reach it is a problem naming
// the usual causes.
func TestDoctorReportsAnUnreachableWeb(t *testing.T) {
	t.Parallel()
	p := probeProvider(t, harnessprofile.LocalToolsDenied, noLocalToolsReply, noWebReply)
	d := p.Diagnose(t.Context(), DiagnoseOptions{Smoke: true})
	if !strings.HasPrefix(d.Web, "allowed; failed") {
		t.Errorf("Web = %q", d.Web)
	}
	joined := strings.Join(d.Problems, "\n")
	if !strings.Contains(joined, "egress proxy") {
		t.Errorf("the problem does not point at the proxy:\n%s", joined)
	}
}

// deniedWebProvider is a fake harness on an entry that turns its web tools off,
// answering the web probe with webReply. The fake's web_denial seeds a file
// rather than replacing argv, because its argv is what makes it the fake.
func deniedWebProvider(t *testing.T, webReply string) *Provider {
	t.Helper()
	return fakeProvider(t, map[string]string{
		"FAKE_STDOUT":             `{"message":"","tool_calls":[{"name":"crewlet_smoke","arguments":{"ok":true}}]}`,
		"FAKE_SHELL_PROBE_STDOUT": noLocalToolsReply,
		"FAKE_WEB_PROBE_STDOUT":   webReply,
	}, map[string]any{
		"local_tools":    "denied",
		"version_args":   []any{"-test.run=NoSuchTest"},
		"web_tools":      "denied",
		"web_tools_note": "the fake's own policy file",
		"web_denial": map[string]any{"seed_files": []any{
			map[string]any{"path": ".fake/web.json", "in": "work", "content": `{"web":false}`},
		}},
	})
}

// THE DOCTOR PROVES A WEB FETCH IS REFUSED on an entry that denies it, as it
// proves a shell is refused on a profile denying local tools. The denial is
// the claim the Dashboard Assistant is admitted on, so a harness that fetched
// anyway is the finding this probe exists for, and a refusal is the healthy
// answer rather than the "web is unreachable" problem it is on a seat.
//
// Mutation: drop the stance from webProbe's switch, and the fetch reads "ok"
// while the refusal reads as a failure; both halves go red.
func TestTheDoctorProvesAWebFetchIsRefused(t *testing.T) {
	t.Parallel()
	t.Run("refused", func(t *testing.T) {
		t.Parallel()
		d := deniedWebProvider(t, noWebReply).Diagnose(t.Context(), DiagnoseOptions{Smoke: true})
		if !strings.HasPrefix(d.Web, "denied by entry (the fake's own policy file); probe: refused") {
			t.Errorf("Web = %q", d.Web)
		}
		for _, problem := range d.Problems {
			if strings.Contains(problem, "web") {
				t.Errorf("a refused fetch on a denied entry is the healthy answer, reported: %s", problem)
			}
		}
	})
	t.Run("fetched", func(t *testing.T) {
		t.Parallel()
		d := deniedWebProvider(t, "ts="+nowEpoch()).Diagnose(t.Context(), DiagnoseOptions{Smoke: true})
		if !strings.Contains(d.Web, "probe: FETCHED") {
			t.Errorf("Web = %q", d.Web)
		}
		joined := strings.Join(d.Problems, "\n")
		if !strings.Contains(joined, "web_denial is not taking effect") {
			t.Errorf("a fetch through a denied entry was not reported as the denial failing:\n%s", joined)
		}
	})
	t.Run("silent", func(t *testing.T) {
		t.Parallel()
		// No answer is no evidence: neither a refusal nor a fetch, and
		// a problem rather than a pass, since the denial is unproven.
		d := deniedWebProvider(t, "").Diagnose(t.Context(), DiagnoseOptions{Smoke: true})
		if strings.Contains(d.Web, "refused") {
			t.Errorf("an empty answer was read as a refusal: Web = %q", d.Web)
		}
		if !slices.ContainsFunc(d.Problems, func(p string) bool { return strings.Contains(p, "web probe") }) {
			t.Errorf("an unproven denial raised no problem: %q", d.Problems)
		}
	})
}

// EVERY PROFILE THAT CAN SERVE THE ASSISTANT TURNS THE WEB OFF. For each
// built-in with a web_denial, the profile Load returns for an entry setting
// `web_tools: denied` must offer the model no web tool by any of the
// spellings this table uses, keep its local tools denied, and differ from the
// seats' profile, whose web stays on.
//
// The set itself is pinned: it is the S2 measurement (Claude Code and Codex
// measured refused, OpenCode derived from its policy format, pi shipping no
// web tool), and a profile gaining a web_denial is a decision that lands here
// with its measurement.
func TestEveryAssistantProfileTurnsTheWebOff(t *testing.T) {
	t.Parallel()
	want := []string{"claude-code", "codex", "opencode", "pi"}
	if got := harnessprofile.WebDeniable(); !slices.Equal(got, want) {
		t.Errorf("profiles with a web denial = %v, want %v", got, want)
	}
	for _, name := range harnessprofile.WebDeniable() {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			seat, err := harnessprofile.Load(name, nil)
			if err != nil {
				t.Fatal(err)
			}
			p, err := harnessprofile.Load(name, map[string]any{"web_tools": "denied"})
			if err != nil {
				t.Fatalf("Load with web_tools denied: %v", err)
			}
			if p.EffectiveWebTools() != harnessprofile.WebToolsDenied {
				t.Errorf("web %q: the denied profile keeps the web", p.EffectiveWebTools())
			}
			// EVERY ARGV A CALL CAN RUN: the profile's own, and a run with
			// tools' own where its mcp block replaces it.
			argvs := [][]string{p.CompleteArgs}
			if p.MCP != nil && len(p.MCP.CompleteArgs) > 0 {
				argvs = append(argvs, p.MCP.CompleteArgs)
			}
			for _, argv := range argvs {
				for _, arg := range argv {
					if strings.Contains(arg, `web_search="live"`) || strings.Contains(arg, `web_search="cached"`) ||
						strings.Contains(arg, `web_search="indexed"`) {
						t.Errorf("argv still turns a web search on: %q", arg)
					}
				}
				for _, flag := range []string{"--allowedTools", "--allowed-tools", "--allow-tool"} {
					for _, tool := range []string{"WebFetch", "WebSearch", "web_fetch", "web_search"} {
						if allowsAfterFlag(argv, flag, tool) {
							t.Errorf("argv allows %s through %s: %q", tool, flag, argv)
						}
					}
				}
			}
			for _, f := range p.SeedFiles {
				lowered := strings.ToLower(f.Content)
				for _, tool := range []string{"webfetch", "websearch", "web_fetch", "web_search"} {
					if strings.Contains(lowered, tool) {
						t.Errorf("seeded %s still names %s:\n%s", f.Path, tool, f.Content)
					}
				}
			}
			if name == "claude-code" && !allowsAfterFlag(p.CompleteArgs, "--disallowedTools", "WebFetch", "WebSearch") {
				t.Errorf("claude-code must put both web tools on --disallowedTools: %q", p.CompleteArgs)
			}
			if name != "pi" && slices.Equal(p.CompleteArgs, seat.CompleteArgs) &&
				slices.Equal(p.SeedFiles, seat.SeedFiles) {
				t.Error("the denied profile runs exactly as the seats' does, so it denies nothing")
			}
		})
	}
}

// -no-smoke skips the probes and says so on both lines, rather than reporting
// an unmeasured stance as a measurement.
func TestDoctorWithoutSmokeSkipsTheProbesVisibly(t *testing.T) {
	t.Parallel()
	p := probeProvider(t, harnessprofile.LocalToolsDenied, nowEpoch(), "ts="+nowEpoch())
	d := p.Diagnose(t.Context(), DiagnoseOptions{Smoke: false})
	if !strings.Contains(d.LocalTools, "probe skipped") || !strings.Contains(d.Web, "skipped") {
		t.Errorf("LocalTools = %q, Web = %q", d.LocalTools, d.Web)
	}
	if problem := probeProblem(d); problem != "" {
		t.Errorf("a skipped probe must not be a problem: %s", problem)
	}
	var out strings.Builder
	d.Render(&out)
	for _, want := range []string{"local tools", "web"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the report has no %q line:\n%s", want, out.String())
		}
	}
}

// A prune is only as good as its blast radius.
//
// The claude profile drops `.claude/.claude.json` between seat generations
// because its `projects` map carries per-directory conversation history — and
// Claude Code keeps timestamped BYTE COPIES of that same file under
// `.claude/backups`, which the prune did not touch. Removing the original and
// leaving the copies carried the history into the next turn by the long way
// round, and left a directory that grew one file per rotation for the life of
// the seat.
func TestTheClaudeProfilePrunesTheBackupsOfWhatItPrunes(t *testing.T) {
	t.Parallel()
	p, _ := harnessprofile.Builtin("claude-code")
	for _, want := range []string{".claude/.claude.json", ".claude/backups"} {
		if !slices.Contains(p.VolatilePaths, want) {
			t.Errorf("claude-code volatile_paths lacks %q: %q", want, p.VolatilePaths)
		}
	}
}

// A REFUSED SHELL IS NOT A REFUSED HOST. The file tools read the engine host as
// well as a shell does, so once the shell probe is refused the doctor writes a
// file outside the seat's home, with a value minted for the run, and asks the
// harness to read it on the assistant's argv: a harness that echoes the value read the
// host, which is a problem on a profile that says its local tools are denied.
func TestDoctorReportsAFileReadAfterTheShellWasRefused(t *testing.T) {
	t.Parallel()
	refused := probeProvider(t, harnessprofile.LocalToolsDenied, noLocalToolsReply, noWebReply)
	d := refused.Diagnose(t.Context(), DiagnoseOptions{Smoke: true})
	if !strings.Contains(d.LocalTools, "refused (shell and file read)") {
		t.Errorf("LocalTools = %q, want both refused", d.LocalTools)
	}

	reads := probeProvider(t, harnessprofile.LocalToolsDenied, noLocalToolsReply, noWebReply)
	reads.env["FAKE_FILE_PROBE"] = "read"
	d = reads.Diagnose(t.Context(), DiagnoseOptions{Smoke: true})
	if !strings.Contains(d.LocalTools, "FILE READ") {
		t.Errorf("LocalTools = %q, want the read reported", d.LocalTools)
	}
	if joined := strings.Join(d.Problems, "\n"); !strings.Contains(joined, "file tools are not off") {
		t.Errorf("the problem does not say the file tools are on:\n%s", joined)
	}
}

// QUICK MODE'S ARGV IS PINNED WHOLE, beside deep work (ADR-0054): the
// assistant's own model runs Claude Code with no tool of its own and no
// session, and one tool server, the engine's `crewlet`, whose tools are the
// person's own requests (ADR-0049); deep work, which runs the same harness with
// its harness in a box, never reaches this argv. A flag added to it, a tool
// given back or another server allowed is a change to what a quick answer can
// reach, made on purpose here or not at all. Mutation: drop "--tools" "", or
// widen the allow list, and this fails beside the test above.
func TestTheAssistantsQuickModeArgvIsUnchanged(t *testing.T) {
	t.Parallel()
	claude, err := harnessprofile.Load("claude-code", map[string]any{"web_tools": "denied"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"-p", "--output-format", "stream-json", "--verbose",
		"--include-partial-messages", "--no-session-persistence", "--setting-sources", "",
		"--strict-mcp-config", "--disable-slash-commands", "--tools", "", "--disallowedTools",
		"Bash", "Read", "Write", "Edit", "NotebookEdit", "Agent", "WebFetch", "WebSearch"}
	if !slices.Equal(claude.CompleteArgs, want) {
		t.Fatalf("the assistant's quick-mode argv changed:\n got %q\nwant %q", claude.CompleteArgs, want)
	}
	m := claude.MCP
	if m == nil || len(m.CompleteArgs) != 0 {
		t.Fatalf("a run with tools does not run the argv above: %+v", m)
	}
	if want := []string{"--mcp-config", "{work}/crewlet-mcp.json"}; !slices.Equal(m.Args, want) {
		t.Errorf("the run's server arguments changed:\n got %q\nwant %q", m.Args, want)
	}
	if want := []string{"--allowedTools", "mcp__crewlet__*"}; !slices.Equal(m.AllowArgs, want) {
		t.Errorf("the run's allow list changed:\n got %q\nwant %q", m.AllowArgs, want)
	}
	if problem := claude.ToolsOffProblem(); problem != "" {
		t.Errorf("the pinned argv leaves the harness a tool of its own: %s", problem)
	}
}
