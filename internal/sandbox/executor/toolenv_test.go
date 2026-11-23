package executor_test

import (
	"encoding/json"
	"maps"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
)

// signInEnv is a run environment holding a sign-in beside variables its tools
// use, and signInNames the names of it that sign the harness in.
var (
	signInEnv = map[string]string{
		"ANTHROPIC_API_KEY": "sk-ant-api03-placeholder", "ANTHROPIC_AUTH_TOKEN": "gateway-placeholder",
		"CREWLET_GIT_ACCESS_FILE": "/home/box/.crewlet/git-access", "GITHUB_TOKEN": "ghp_placeholder", "AWS_REGION": "eu-west-1",
	}
	signInNames = []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN"}
	toolNames   = []string{"CREWLET_GIT_ACCESS_FILE", "GITHUB_TOKEN", "AWS_REGION"}

	// telemetryEnv is a run's telemetry export as an OpenCode run's harness
	// keeps it for its own.
	telemetryEnv = map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "https://engine.example.com/otlp/placeholder",
		"OTEL_EXPORTER_OTLP_HEADERS": "x-placeholder=1"}
)

// envLines is an `env` listing as a map.
func envLines(listing string) map[string]string {
	out := map[string]string{}
	for line := range strings.SplitSeq(listing, "\n") {
		if name, value, ok := strings.Cut(line, "="); ok {
			out[name] = value
		}
	}
	return out
}

// A CLAUDE CODE RUN'S COMMANDS START WITHOUT ITS SIGN-IN, AND ITS HARNESS WITH
// IT. The harness signs in from its own environment; the file CLAUDE_ENV_FILE
// names is run in the shell of each command its model runs, ahead of the
// command, as Claude Code does (measured on 2.1.330). Played on a real box, the
// harness a stand-in recording its own environment and running one command so.
// A run handed no sign-in names no file, and an image's own CLAUDE_ENV_FILE
// stays.
//
// Mutation: drop the export, or the names from the file, and the command holds
// the sign-in; unset the names in the script, and the harness loses it.
func TestAClaudeCodeRunsCommandsStartWithoutItsSignIn(t *testing.T) {
	t.Parallel()
	runner, b := realBox(t)
	harness := strings.Replace(sessionHarness("2.1.330"), "env > \"$HOME/env\"\n",
		"env > \"$HOME/env\"\nsh -c '[ -z \"$CLAUDE_ENV_FILE\" ] || . \"$CLAUDE_ENV_FILE\"; env' > \"$HOME/toolenv\"\n", 1)
	writeBoxStandIn(t, b, executor.PathsFor(b).BinDir()+"/claude", harness)

	runToEnd(t, runner, b, sandbox.RunRequest{Brief: "begin", Env: signInEnv, SignInEnv: signInNames})
	own, tools := envLines(boxFile(t, b, b.Home()+"/env")), envLines(boxFile(t, b, b.Home()+"/toolenv"))
	for name, value := range signInEnv {
		if own[name] != value {
			t.Errorf("the harness started with %s=%q, want %q", name, own[name], value)
		}
	}
	for _, name := range signInNames {
		if value, held := tools[name]; held {
			t.Errorf("the model's command started with the sign-in's %s=%q", name, value)
		}
	}
	for _, name := range toolNames {
		if tools[name] != signInEnv[name] {
			t.Errorf("the model's command started with %s=%q, want the run's %q", name, tools[name], signInEnv[name])
		}
	}

	image := map[string]string{"CLAUDE_ENV_FILE": "/etc/profile.d/image.sh"}
	runToEnd(t, runner, b, sandbox.RunRequest{Brief: "again", Env: image})
	if got := envLines(boxFile(t, b, b.Home()+"/env"))["CLAUDE_ENV_FILE"]; got != image["CLAUDE_ENV_FILE"] {
		t.Errorf("a run handed no sign-in started its harness with CLAUDE_ENV_FILE=%q, want the image's", got)
	}
}

// toolEnvPlugin is the tool-environment plugin's entry in a run's configuration.
type toolEnvPlugin struct {
	Package string `json:"package"`
	Options struct {
		Names []string `json:"names"`
	} `json:"options"`
}

// EVERY OPENCODE RUN NAMES THE TOOL-ENVIRONMENT PLUGIN, LAST, with its option
// the names its harness signs in by and its own variables (the server's and
// the telemetry export's), sorted, and
// the box holds this build's plugin; wired to its notes and skills or to
// nothing alike.
//
// Mutation: name the plugin only for a run handed a sign-in, or leave the
// harness's own variables or the sign-in out of its names, and a case goes red.
func TestEveryOpenCodeRunNamesTheToolEnvironmentPluginLast(t *testing.T) {
	t.Parallel()
	embedded, err := os.ReadFile("opencode_toolenv.js")
	if err != nil {
		t.Fatalf("read the plugin's source: %v", err)
	}
	for name, tc := range map[string]struct {
		req  sandbox.RunRequest
		want []string
	}{
		"signed in, wired to nothing": {req: sandbox.RunRequest{Brief: "x", SignInEnv: []string{"OPENROUTER_API_KEY", "ANTHROPIC_API_KEY"}},
			want: []string{"ANTHROPIC_API_KEY", "CREWLET_STEER_SESSION", "OPENCODE_CONFIG_CONTENT", "OPENROUTER_API_KEY",
				"OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_EXPORTER_OTLP_HEADERS"}},
		"handed no sign-in": {req: sandbox.RunRequest{Brief: "x"},
			want: []string{"CREWLET_STEER_SESSION", "OPENCODE_CONFIG_CONTENT", "OTEL_EXPORTER_OTLP_ENDPOINT",
				"OTEL_EXPORTER_OTLP_HEADERS"}},
		"wired to its notes and skills": {req: sandbox.RunRequest{Brief: "x", SignInEnv: []string{"ANTHROPIC_API_KEY"},
			Skills: handedSkills(), SkillLoads: skillWiring(), Steer: steerWiring},
			want: []string{"ANTHROPIC_API_KEY", "CREWLET_STEER_SESSION", "OPENCODE_CONFIG_CONTENT",
				"OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_EXPORTER_OTLP_HEADERS"}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, raw, b := writeOpenCodeConfigIn(t, tc.req)
			var cfg struct {
				Plugins []json.RawMessage `json:"plugins"`
			}
			if err := json.Unmarshal([]byte(raw), &cfg); err != nil || len(cfg.Plugins) == 0 {
				t.Fatalf("the configuration names no plugin (%v):\n%s", err, raw)
			}
			var last toolEnvPlugin
			if err := json.Unmarshal(cfg.Plugins[len(cfg.Plugins)-1], &last); err != nil {
				t.Fatalf("the last plugin entry is no plugin (%v):\n%s", err, raw)
			}
			if p := executor.PathsFor(b); last.Package != p.OpenCodeToolEnvPlugin() {
				t.Fatalf("the last plugin is %q, want %q:\n%s", last.Package, p.OpenCodeToolEnvPlugin(), raw)
			}
			if !slices.Equal(last.Options.Names, tc.want) {
				t.Errorf("the plugin keeps %v out of the commands, want %v", last.Options.Names, tc.want)
			}
			if got := boxFile(t, b, last.Package+"/index.js"); got != string(embedded) {
				t.Error("the box holds a tool-environment plugin that is not this build's")
			}
		})
	}
}

// THE TOOL-ENVIRONMENT PLUGIN TAKES EACH NAME FROM A COMMAND AND NONE FROM THE
// SERVER: OpenCode copies its own environment into each shell command's and
// hands that to the plugin's `create.before` hook, so the command lacks every
// name (a sign-in's, the telemetry export's) while the server, whose providers
// read their keys from it and whose export reads its endpoint, keeps them.
// With no names it registers nothing. Driven under node by
// opencode_toolenv_probe.mjs, as OpenCode 2.3.2 was measured to call it.
//
// Mutation: delete nothing in the hook, or delete from process.env, and a case
// goes red.
func TestTheToolEnvironmentPluginTakesEachNameFromACommandAndNoneFromTheServer(t *testing.T) {
	t.Parallel()
	node := nodeForNotes(t)
	harnessEnv := maps.Clone(signInEnv)
	maps.Copy(harnessEnv, telemetryEnv)
	omitted := slices.Concat(signInNames, slices.Sorted(maps.Keys(telemetryEnv)))
	probe := func(t *testing.T, names []string) (hooked []string, command, server map[string]string) {
		t.Helper()
		arg, err := json.Marshal(names)
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(node, "opencode_toolenv_probe.mjs", "opencode_toolenv.js", string(arg)) //nolint:gosec // the probe under test
		cmd.Env = []string{"PATH=" + os.Getenv("PATH")}
		for name, value := range harnessEnv {
			cmd.Env = append(cmd.Env, name+"="+value)
		}
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("the probe: %v\n%s", err, out)
		}
		var got struct {
			Hooked  []string          `json:"hooked"`
			Command map[string]string `json:"command"`
			Server  map[string]string `json:"server"`
		}
		if err = json.Unmarshal(out, &got); err != nil {
			t.Fatalf("the probe's report: %v\n%s", err, out)
		}
		return got.Hooked, got.Command, got.Server
	}

	hooked, command, server := probe(t, omitted)
	if !slices.Equal(hooked, []string{"create.before"}) {
		t.Fatalf("the plugin registered %v, want the shell's create.before", hooked)
	}
	for _, name := range omitted {
		if value, held := command[name]; held {
			t.Errorf("the command started with the harness's %s=%q", name, value)
		}
	}
	for name, value := range harnessEnv {
		if server[name] != value {
			t.Errorf("the server's own %s = %q, want %q", name, server[name], value)
		}
	}
	for _, name := range toolNames {
		if command[name] != signInEnv[name] {
			t.Errorf("the command started with %s=%q, want the run's %q", name, command[name], signInEnv[name])
		}
	}
	if hooked, _, _ = probe(t, nil); len(hooked) != 0 {
		t.Errorf("handed no names, the plugin registered %v", hooked)
	}
}

// A SIGN-IN NAME NO SHELL CAN UNSET IS REFUSED by every runner that keeps the
// sign-in from its commands (Claude Code, OpenCode, Codex, Qwen Code) before
// its harness starts, rather than left in the commands' environment, and
// before the box is touched: the refused launch ran no command and wrote no
// file there.
//
// Mutation: skip the check in any runner, and it writes its configuration;
// prune the home or write a file before it, and the box was touched.
func TestASignInNameNoShellCanUnsetIsRefused(t *testing.T) {
	t.Parallel()
	req := sandbox.RunRequest{Brief: "x", SignInEnv: []string{"ANTHROPIC_API_KEY", "MY-KEY"}}
	for _, h := range []executor.Harness{executor.ClaudeCode{}, executor.OpenCode{}, executor.Codex{},
		executor.QwenCode{}} {
		b := &writesRecorded{FakeSandbox: sandbox.NewFakeSandbox("box-1")}
		if _, err := h.WriteConfig(t.Context(), b, req, executor.Session{}, executor.PathsFor(b)); err == nil ||
			!strings.Contains(err.Error(), `"MY-KEY"`) {
			t.Errorf("%s: WriteConfig = %v, want the name refused", h.Name(), err)
		}
		if execs := b.Execs(); len(execs) != 0 || len(b.written) != 0 {
			t.Errorf("%s: the refused launch ran %v and wrote %q in the box, want nothing", h.Name(), execs, b.written)
		}
	}
}

// A LAUNCH REFUSED FOR ITS WIRING LEAVES THE BOX AS IT FOUND IT. A run whose
// wiring for a session of its own (FreshWiring) is not wired as the one it
// continues is refused by Gemini CLI's, Qwen Code's and Codex's WriteConfig:
// its notes wired on the request alone, or its skill loads (with a skill to
// load, for Codex). The refusal comes before the home's prune and every
// write, so the box ran no command and holds no file of the refused launch.
//
// Mutation: prune the home, or write a file, before any one refusal (as Qwen
// Code's prune and Codex's prune and configuration once came), and a row goes red.
func TestALaunchRefusedForItsWiringLeavesTheBoxAsItFoundIt(t *testing.T) {
	t.Parallel()
	const endpoint = "http://127.0.0.1:8000/mcp/tok"
	steer := sandbox.SteerWiring{Server: "crewlet", Tool: "steer", URL: endpoint + "/notes"}
	loads := sandbox.SkillWiring{Server: "crewlet", Tool: "skill_loaded", URL: endpoint + "/skills"}
	skills := []sandbox.Skill{{Name: "deploy", Description: "How to deploy.", Body: "Run make deploy."}}
	for name, tc := range map[string]struct {
		h   executor.Harness
		req sandbox.RunRequest
	}{
		"gemini, notes":     {executor.GeminiCLI{}, sandbox.RunRequest{Steer: steer, FreshWiring: &sandbox.Wiring{}}},
		"gemini, skills":    {executor.GeminiCLI{}, sandbox.RunRequest{SkillLoads: loads, FreshWiring: &sandbox.Wiring{}}},
		"qwen code, notes":  {executor.QwenCode{}, sandbox.RunRequest{Steer: steer, FreshWiring: &sandbox.Wiring{}}},
		"qwen code, skills": {executor.QwenCode{}, sandbox.RunRequest{SkillLoads: loads, Skills: skills, FreshWiring: &sandbox.Wiring{}}},
		"codex, notes":      {executor.Codex{}, sandbox.RunRequest{Steer: steer, FreshWiring: &sandbox.Wiring{}}},
		"codex, skills":     {executor.Codex{}, sandbox.RunRequest{SkillLoads: loads, Skills: skills, FreshWiring: &sandbox.Wiring{}}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			b := &writesRecorded{FakeSandbox: sandbox.NewFakeSandbox("box-1")}
			tc.req.Brief = "x"
			tc.req.MCPServers = map[string]sandbox.MCPServer{
				"crewlet": {Name: "crewlet", Transport: sandbox.TransportHTTP, URL: endpoint}}
			_, err := tc.h.WriteConfig(t.Context(), b, tc.req, executor.Session{ID: "s-1", Fresh: "s-2"},
				executor.PathsFor(b))
			if err == nil || !strings.Contains(err.Error(), "two wirings") {
				t.Fatalf("WriteConfig = %v, want the mismatched wirings refused", err)
			}
			if execs := b.Execs(); len(execs) != 0 || len(b.written) != 0 {
				t.Errorf("the refused launch ran %v and wrote %q in the box, want nothing", execs, b.written)
			}
		})
	}
}
