package cliagent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// A subscription seat must not have less reach than the same CLI at a
// terminal: web is the one local tool every profile keeps on. The claude
// profile has to ALLOW it, not merely stop denying it — print mode never
// prompts, and an unlisted permission-gated tool is refused.
func TestNoShippedProfileDeniesTheWeb(t *testing.T) {
	for _, name := range BuiltinNames() {
		p, _ := Builtin(name)
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
	claude, _ := Builtin("claude-code")
	if !allowsAfterFlag(claude.CompleteArgs, "--allowedTools", "WebFetch", "WebSearch") {
		t.Errorf("claude-code must --allowedTools WebFetch WebSearch: %q", claude.CompleteArgs)
	}
	copilot, _ := Builtin("copilot")
	joined := strings.Join(copilot.CompleteArgs, " ")
	if !strings.Contains(joined, "--allow-tool web_fetch") || !strings.Contains(joined, "--allow-tool web_search") {
		t.Errorf("copilot must allow its web tools explicitly: %q", copilot.CompleteArgs)
	}
	codex, _ := Builtin("codex")
	if !strings.Contains(strings.Join(codex.CompleteArgs, " "), `web_search="live"`) {
		t.Errorf("codex must switch its web search to live, or it answers from an offline index: %q",
			codex.CompleteArgs)
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

// Every shipped profile states what it does about the CLI's own tools, and a
// profile that admits them says why — the silent hole this field closes is a
// profile whose isolation was assumed rather than declared.
func TestEveryShippedProfileDeclaresItsLocalToolsStance(t *testing.T) {
	for _, name := range BuiltinNames() {
		if name == "custom" {
			continue
		}
		p, _ := Builtin(name)
		if !p.LocalTools.Valid() {
			t.Errorf("%s: local_tools = %q, want denied or vendor-default", name, p.LocalTools)
		}
		if p.LocalTools == LocalToolsVendorDefault && p.LocalToolsNote == "" {
			t.Errorf("%s: vendor-default with no local_tools_note", name)
		}
	}
	for _, name := range []string{"claude-code", "codex", "gemini-cli", "qwen-code", "opencode", "cursor-agent", "copilot"} {
		p, _ := Builtin(name)
		if p.LocalTools != LocalToolsDenied {
			t.Errorf("%s: local_tools = %q, want denied", name, p.LocalTools)
		}
	}
}

// The vendors that take their policy from a file get a file that parses and
// says what the profile claims: local tools off, web on.
func TestSeededSettingsFilesAreValidJSONThatKeepsTheWebOn(t *testing.T) {
	cases := map[string]struct {
		path  string
		scope SeedScope
		must  []string
		never []string
	}{
		"gemini-cli": {".gemini/settings.json", SeedHome,
			[]string{"run_shell_command", "write_file", `"web_fetch"`}, nil},
		"qwen-code": {".qwen/settings.json", SeedHome,
			[]string{"run_shell_command", `"web_fetch"`}, nil},
		"opencode": {".config/opencode/opencode.json", SeedHome,
			[]string{`"bash":"deny"`, `"edit":"deny"`, `"webfetch":"allow"`, `"websearch":"allow"`},
			[]string{`"ask"`}},
		"cursor-agent": {".cursor/cli.json", SeedWork,
			[]string{`Shell(*)`, `Write(*)`, `WebFetch(*)`}, nil},
	}
	for name, c := range cases {
		p, _ := Builtin(name)
		var found *SeedFile
		for i := range p.SeedFiles {
			if p.SeedFiles[i].Path == c.path {
				found = &p.SeedFiles[i]
			}
		}
		if found == nil {
			t.Errorf("%s seeds no %s", name, c.path)
			continue
		}
		if found.scope() != c.scope {
			t.Errorf("%s: %s scope = %q, want %q", name, c.path, found.scope(), c.scope)
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
	p := fakeProvider(t, nil, map[string]any{
		"seed_files": []any{
			map[string]any{"path": ".vendor/settings.json", "content": `{"home":true}`},
			map[string]any{"path": ".vendor/project.json", "in": "work", "content": `{"work":true}`},
		},
	})
	co, err := p.ws.Acquire("seat-a", "call-1")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	t.Cleanup(func() { _ = co.Release() })

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
	for _, path := range []string{"../outside.json", "/etc/settings.json"} {
		_, err := New(Config{
			Key: "sub", Agent: "custom", StateDir: t.TempDir(), Timeout: time.Second, MaxConcurrent: 1,
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
		Key: "sub", Agent: "custom", StateDir: t.TempDir(), Timeout: time.Second, MaxConcurrent: 1,
		Overrides: map[string]any{
			"binary": "x", "complete_args": []any{"-p"}, "output": "text", "model_args": []any{},
			"local_tools": "vendor-default",
		},
	})
	if err == nil || !strings.Contains(err.Error(), "local_tools_note") {
		t.Errorf("vendor-default without a note: err = %v, want a refusal naming local_tools_note", err)
	}
}

// probeProvider is a fake CLI that answers the two isolation probes with
// the given replies, under the given stance.
func probeProvider(t *testing.T, stance LocalTools, shellReply, webReply string) *Provider {
	t.Helper()
	overrides := map[string]any{"local_tools": string(stance)}
	if stance == LocalToolsVendorDefault {
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

// A profile that says "denied" and a CLI that refused is the healthy case.
func TestDoctorProbesReportADeniedShellAndAReachableWeb(t *testing.T) {
	p := probeProvider(t, LocalToolsDenied, noLocalToolsReply, "ts="+nowEpoch()+".123")
	d := p.Diagnose(t.Context(), DiagnoseOptions{Smoke: true})
	if !strings.HasPrefix(d.LocalTools, "denied by profile — probe: refused") {
		t.Errorf("LocalTools = %q", d.LocalTools)
	}
	if !strings.HasPrefix(d.Web, "ok") {
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

// A profile that says "denied" and a CLI that ran the command is the finding
// the probe exists for: the vendor's flag is not taking effect on this build.
func TestDoctorReportsAShellThatRanDespiteTheProfile(t *testing.T) {
	p := probeProvider(t, LocalToolsDenied, nowEpoch()+"\n", "ts="+nowEpoch())
	d := p.Diagnose(t.Context(), DiagnoseOptions{Smoke: true})
	if !strings.Contains(d.LocalTools, "SHELL RAN") {
		t.Errorf("LocalTools = %q", d.LocalTools)
	}
	joined := strings.Join(d.Problems, "\n")
	if !strings.Contains(joined, "not taking effect") {
		t.Errorf("the problem does not say the denial is not holding:\n%s", joined)
	}
}

// A vendor-default profile whose CLI ran the command is reported as what it
// is — a CLI with a shell on the engine host — with the note the profile gave.
func TestDoctorReportsAVendorDefaultShellHonestly(t *testing.T) {
	p := probeProvider(t, LocalToolsVendorDefault, nowEpoch(), "ts="+nowEpoch())
	d := p.Diagnose(t.Context(), DiagnoseOptions{Smoke: true})
	if !strings.HasPrefix(d.LocalTools, "vendor default (no denial flag on this fake) — probe: SHELL RAN") {
		t.Errorf("LocalTools = %q", d.LocalTools)
	}
	joined := strings.Join(d.Problems, "\n")
	if !strings.Contains(joined, "engine user") || !strings.Contains(joined, "no denial flag on this fake") {
		t.Errorf("the problem does not name the trust the operator is taking on:\n%s", joined)
	}
}

// Web is meant to stay on; a CLI that cannot reach it is a problem naming
// the usual causes.
func TestDoctorReportsAnUnreachableWeb(t *testing.T) {
	p := probeProvider(t, LocalToolsDenied, noLocalToolsReply, noWebReply)
	d := p.Diagnose(t.Context(), DiagnoseOptions{Smoke: true})
	if !strings.HasPrefix(d.Web, "failed") {
		t.Errorf("Web = %q", d.Web)
	}
	joined := strings.Join(d.Problems, "\n")
	if !strings.Contains(joined, "egress proxy") {
		t.Errorf("the problem does not point at the proxy:\n%s", joined)
	}
}

// -no-smoke skips the probes and says so on both lines, rather than reporting
// an unmeasured stance as a measurement.
func TestDoctorWithoutSmokeSkipsTheProbesVisibly(t *testing.T) {
	p := probeProvider(t, LocalToolsDenied, nowEpoch(), "ts="+nowEpoch())
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
