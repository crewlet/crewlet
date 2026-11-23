package executor_test

import (
	"encoding/json"
	"maps"
	"os"
	"os/exec"
	"path"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
	"github.com/crewlet/crewlet/internal/shellword"
)

// runEnv is a run's environment as a launch hands it: a seat's own tool
// credential and Gitlet's names, which stay, beside Crewlet's own telemetry
// and trace context (sandbox.OtelReceiver.RunEnv, an operator's OTEL_
// variable among them), which the model's shell runs without. The sign-in is
// each runner's own ([shellRow.signIn]).
func runEnv() map[string]string {
	return map[string]string{
		"GITLAB_TOKEN":                       "glpat-the-seats-tool",
		sandbox.GitURLVar:                    "https://git.example.com/box/git",
		sandbox.GitAccessFileVar:             "/home/box/.crewlet/git-access",
		"OTEL_EXPORTER_OTLP_ENDPOINT":        "https://engine.example.com/otlp/" + otelToken + "/v1/traces",
		"OTEL_EXPORTER_OTLP_HEADERS":         "",
		"OTEL_EXPORTER_OTLP_PROTOCOL":        "http/protobuf",
		"OTEL_EXPORTER_OTLP_CUSTOM_OPERATOR": "an operator's",
		"OTEL_RESOURCE_ATTRIBUTES":           "crewlet.turn_id=t,crewlet.agent_handle=h",
		"TRACEPARENT":                        "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01",
		"TRACESTATE":                         "crewlet=t",
	}
}

// otelToken and bridgeToken are the run's telemetry and bridge tokens, which
// no variable of the model's shell holds.
const (
	otelToken   = "otel-token-placeholder"
	bridgeToken = "bridge-token-placeholder"
)

// crewletsOwn reports whether name is one of Crewlet's own variables.
func crewletsOwn(name string) bool {
	return strings.HasPrefix(name, "OTEL_") || name == "TRACEPARENT" || name == "TRACESTATE"
}

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

// shellRow is how one runner's harness starts its model's shell, played in a
// real box whose stand-in harness runs $HOME/scenario.sh from inside the
// harness process, under the environment the run's script gave it, writing
// $HOME/observed.
type shellRow struct {
	// signIn is a variable the harness signs in by.
	signIn string
	box    func(t *testing.T) (*executor.Runner, sandbox.Sandbox, string)
	// scenario starts the shell as the harness does, printing its `env`.
	scenario string
	// shell is the model's shell's environment from what the scenario
	// printed, where the harness's own mechanism applies inside it (nil: as
	// printed).
	shell func(t *testing.T, b sandbox.Sandbox, record string, printed map[string]string) map[string]string
}

// fromCredit is a [creditBoxes] box, which keeps no record.
func fromCredit(name string) func(t *testing.T) (*executor.Runner, sandbox.Sandbox, string) {
	return func(t *testing.T) (*executor.Runner, sandbox.Sandbox, string) {
		runner, b := creditBoxes[name](t)
		return runner, b, ""
	}
}

// shellRows is, for every registered runner, how its model's shell starts.
var shellRows = map[string]shellRow{
	// Claude Code runs the file CLAUDE_ENV_FILE names ahead of each command.
	executor.ClaudeCodeName: {signIn: "ANTHROPIC_API_KEY", box: fromCredit(executor.ClaudeCodeName),
		scenario: `[ -z "${CLAUDE_ENV_FILE-}" ] || . "$CLAUDE_ENV_FILE"` + "\nenv\n"},
	// Codex starts each command with its environment less the server's
	// shell_environment_policy.exclude, patterns matched case-blind.
	executor.CodexName: {signIn: "CODEX_API_KEY", box: shellCodexBox, scenario: "env\n",
		shell: func(t *testing.T, _ sandbox.Sandbox, record string, printed map[string]string) map[string]string {
			excluded := codexShellExcluded(t, recorded(t, record, "server-argv"))
			return maps.Collect(func(yield func(string, string) bool) {
				for name, value := range printed {
					if !slices.ContainsFunc(excluded, func(p string) bool {
						ok, _ := path.Match(strings.ToUpper(p), strings.ToUpper(name))
						return ok
					}) && !yield(name, value) {
						return
					}
				}
			})
		}},
	executor.GeminiCLIName: {signIn: "GEMINI_API_KEY", box: fromCredit(executor.GeminiCLIName), scenario: "env\n"},
	executor.QwenCodeName:  {signIn: "OPENAI_API_KEY", box: fromCredit(executor.QwenCodeName), scenario: "env\n"},
	// OpenCode's server hands each command a copy of its environment through
	// the tool-environment plugin, played under node as written.
	executor.OpenCodeName: {signIn: "ANTHROPIC_API_KEY", box: shellOpenCodeBox, scenario: "env\n",
		shell: func(t *testing.T, b sandbox.Sandbox, _ string, printed map[string]string) map[string]string {
			var cfg struct {
				Plugins []json.RawMessage `json:"plugins"`
			}
			raw := boxFile(t, b, executor.PathsFor(b).OpenCodeConfig())
			if err := json.Unmarshal([]byte(raw), &cfg); err != nil || len(cfg.Plugins) == 0 {
				t.Fatalf("the run's configuration names no plugin (%v):\n%s", err, raw)
			}
			var last struct {
				Options json.RawMessage `json:"options"`
			}
			if err := json.Unmarshal(cfg.Plugins[len(cfg.Plugins)-1], &last); err != nil {
				t.Fatal(err)
			}
			_, command, _ := probeToolEnv(t, string(last.Options), printed)
			return command
		}},
	// Copilot's shell is bash, which runs the file BASH_ENV names where the
	// run's settings switch bashEnv on.
	executor.CopilotName: {signIn: "GH_TOKEN", box: fromCredit(executor.CopilotName),
		scenario: `if grep -q '"bashEnv": *true' "$COPILOT_HOME/settings.json"; then exec bash -c env; fi` + "\nenv -u BASH_ENV env\n"},
	executor.MuseName: {signIn: "META_API_KEY", box: fromCredit(executor.MuseName), scenario: "env\n"},
	executor.KimiName: {signIn: "KIMI_MODEL_API_KEY", box: fromCredit(executor.KimiName), scenario: "env\n"},
	// Pi's bash tool runs each command under the extension's spawn hook,
	// which leaves out the names and prefixes the run's extension is written
	// with (the hook itself: TestThePiExtensionRunsTheShellWithoutCrewletsVariables).
	executor.PiName: {signIn: "ANTHROPIC_API_KEY", box: fromCredit(executor.PiName), scenario: "env\n",
		shell: func(t *testing.T, b sandbox.Sandbox, _ string, printed map[string]string) map[string]string {
			_, rest, ok := strings.Cut(boxFile(t, b, executor.PathsFor(b).PiExtension()), "const config = ")
			line, _, _ := strings.Cut(rest, "\n")
			var s struct {
				Withheld         []string `json:"withheld"`
				WithheldPrefixes []string `json:"withheldPrefixes"`
			}
			if err := json.Unmarshal([]byte(line), &s); !ok || err != nil {
				t.Fatalf("the run's extension holds no settings (%v)", err)
			}
			return maps.Collect(func(yield func(string, string) bool) {
				for name, value := range printed {
					if !slices.Contains(s.Withheld, name) &&
						!slices.ContainsFunc(s.WithheldPrefixes, func(p string) bool { return strings.HasPrefix(name, p) }) &&
						!yield(name, value) {
						return
					}
				}
			})
		}},
}

// shellCodexBox is [creditBoxes]' Codex box with the stand-in's record kept.
func shellCodexBox(t *testing.T) (*executor.Runner, sandbox.Sandbox, string) {
	runner, b, record := codexBox(t, "0.171.0")
	bin := executor.PathsFor(b).BinDir() + "/codex"
	standIn, err := b.ReadFile(t.Context(), bin)
	if err != nil || !strings.Contains(string(standIn), "\napp-server)\n") {
		t.Fatalf("the codex stand-in has no app-server branch (%v)", err)
	}
	writeBoxStandIn(t, b, bin, strings.Replace(string(standIn), "\napp-server)\n", "\napp-server)\n  "+scenarioRun, 1))
	return runner, b, record
}

// shellOpenCodeBox is an OpenCode box whose server's stand-in runs the
// scenario as it starts: the model's commands are the server's.
func shellOpenCodeBox(t *testing.T) (*executor.Runner, sandbox.Sandbox, string) {
	runner, b := openCodeBox(t, t.TempDir())
	standIn := strings.Replace(fakeOpenCode, `d="$FAKE_DIR"`, "d="+shellword.Quote(t.TempDir()), 1)
	standIn = strings.Replace(standIn, "serve)\n", "serve)\n  "+scenarioRun, 1)
	writeBoxStandIn(t, b, executor.PathsFor(b).BinDir()+"/opencode", standIn)
	return runner, b, ""
}

// EVERY RUNNER'S MODEL SHELL HOLDS ITS SIGN-IN AND NONE OF CREWLET'S OWN
// VARIABLES. Each registered runner's whole run, in a real box, its harness a
// stand-in starting the shell as the harness does: the variable the harness
// signs in by reaches the shell, as it does a person's, and so do the seat's
// tool credential and Gitlet's names; no OTEL_ variable, TRACEPARENT or
// TRACESTATE does, and no variable holds the run's bridge or telemetry token.
//
// Mutations, each red: drop the prefix loop from the shared strip (every
// runner but Codex, OpenCode and Pi), its globs' prefix (Codex), the plugin's
// prefixes (OpenCode) or the extension's (Pi); a trace name from the strip;
// unset the sign-in in any runner's script; drop a runner from shellRows.
func TestEveryRunnersShellHoldsItsSignInAndNoneOfCrewletsVariables(t *testing.T) {
	t.Parallel()
	const bridge = "http://127.0.0.1:7/mcp/" + bridgeToken
	for _, name := range executor.Names() {
		row, ok := shellRows[name]
		if !ok {
			t.Errorf("runner %s has no row in shellRows, so nothing shows what its model's shell holds", name)
			continue
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			runner, b, record := row.box(t)
			if err := b.WriteFile(t.Context(), b.Home()+"/scenario.sh", []byte(row.scenario)); err != nil {
				t.Fatalf("write the scenario: %v", err)
			}
			env := runEnv()
			env[row.signIn] = "sign-in-placeholder"
			handle, err := runner.Start(t.Context(), b, sandbox.RunRequest{Brief: "begin", Env: env,
				MCPServers: map[string]sandbox.MCPServer{"crewlet-bridge": {Name: "crewlet-bridge",
					Transport: sandbox.TransportHTTP, URL: bridge, AlwaysLoaded: true}}})
			if err != nil {
				t.Fatalf("Start: %v", err)
			}
			awaitOver(t, runner, b, handle, time.Minute, "a stand-in printing its environment takes seconds")
			printed := boxFile(t, b, b.Home()+"/observed")
			if printed == "" {
				t.Fatalf("the scenario printed nothing: %s", boxFile(t, b, b.Home()+"/observed.err"))
			}
			shell := envLines(printed)
			if row.shell != nil {
				shell = row.shell(t, b, record, shell)
			}
			for _, kept := range []string{row.signIn, "GITLAB_TOKEN", sandbox.GitURLVar, sandbox.GitAccessFileVar} {
				if want := env[kept]; shell[kept] != want {
					t.Errorf("the model's shell holds %s=%q, want the run's %q", kept, shell[kept], want)
				}
			}
			for name, value := range shell {
				if crewletsOwn(name) || strings.Contains(value, bridgeToken) || strings.Contains(value, otelToken) {
					t.Errorf("the model's shell holds Crewlet's own %s=%q", name, value)
				}
			}
		})
	}
}

// toolEnvPlugin is the tool-environment plugin's entry in a run's configuration.
type toolEnvPlugin struct {
	Package string `json:"package"`
	Options struct {
		Names    []string `json:"names"`
		Prefixes []string `json:"prefixes"`
	} `json:"options"`
}

// EVERY OPENCODE RUN NAMES THE TOOL-ENVIRONMENT PLUGIN, LAST, with its options
// Crewlet's own variables, the server's included, sorted, and the prefix of
// every telemetry variable; and the box holds this build's plugin; wired to
// its notes and skills or to nothing alike.
//
// Mutation: name the plugin only for a run wired to its notes, or leave the
// server's variables or the prefix out of its options, and a case goes red.
func TestEveryOpenCodeRunNamesTheToolEnvironmentPluginLast(t *testing.T) {
	t.Parallel()
	embedded, err := os.ReadFile("opencode_toolenv.js")
	if err != nil {
		t.Fatalf("read the plugin's source: %v", err)
	}
	want := []string{"CREWLET_STEER_SESSION", "OPENCODE_CONFIG_CONTENT", "TRACEPARENT", "TRACESTATE"}
	for name, req := range map[string]sandbox.RunRequest{
		"wired to nothing": {Brief: "x"},
		"wired to its notes and skills": {Brief: "x", Skills: handedSkills(), SkillLoads: skillWiring(),
			Steer: steerWiring},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, raw, b := writeOpenCodeConfigIn(t, req)
			var cfg struct {
				Plugins []toolEnvPlugin `json:"plugins"`
			}
			if err := json.Unmarshal([]byte(raw), &cfg); err != nil || len(cfg.Plugins) == 0 {
				t.Fatalf("the configuration names no plugin (%v):\n%s", err, raw)
			}
			last := cfg.Plugins[len(cfg.Plugins)-1]
			if p := executor.PathsFor(b); last.Package != p.OpenCodeToolEnvPlugin() {
				t.Fatalf("the last plugin is %q, want %q:\n%s", last.Package, p.OpenCodeToolEnvPlugin(), raw)
			}
			if !slices.Equal(last.Options.Names, want) || !slices.Equal(last.Options.Prefixes, []string{"OTEL_"}) {
				t.Errorf("the plugin keeps %v and %v out of the commands, want %v and OTEL_", last.Options.Names,
					last.Options.Prefixes, want)
			}
			if got := boxFile(t, b, last.Package+"/index.js"); got != string(embedded) {
				t.Error("the box holds a tool-environment plugin that is not this build's")
			}
		})
	}
}

// probeToolEnv runs the tool-environment plugin under node with options
// (JSON) in a server whose environment is env, answering the hooks it
// registered, the command's environment and the server's own.
func probeToolEnv(t *testing.T, options string, env map[string]string) (hooked []string, command, server map[string]string) {
	t.Helper()
	node := nodeForNotes(t)
	cmd := exec.Command(node, "opencode_toolenv_probe.mjs", "opencode_toolenv.js", options) //nolint:gosec // the probe under test
	cmd.Env = []string{"PATH=" + os.Getenv("PATH")}
	for name, value := range env {
		if name != "PATH" {
			cmd.Env = append(cmd.Env, name+"="+value)
		}
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

// THE TOOL-ENVIRONMENT PLUGIN TAKES EACH NAME FROM A COMMAND AND NONE FROM THE
// SERVER: OpenCode copies its own environment into each shell command's and
// hands that to the plugin's `create.before` hook, so the command lacks every
// name the plugin is handed and every name under a prefix it is handed, while
// the server, whose export reads its endpoint, keeps them, and the command
// keeps the rest, the sign-in included. With no names and no prefixes it
// registers nothing. Driven under node by opencode_toolenv_probe.mjs, as
// OpenCode 2.0.32 to 2.3.0 were measured to call it.
//
// Mutation: delete nothing in the hook, match the prefixes as names, or
// delete from process.env, and a case goes red.
func TestTheToolEnvironmentPluginTakesEachNameFromACommandAndNoneFromTheServer(t *testing.T) {
	t.Parallel()
	env := runEnv()
	env["ANTHROPIC_API_KEY"] = "sk-ant-api03-placeholder"
	env["OPENCODE_CONFIG_CONTENT"] = "{}"
	hooked, command, server := probeToolEnv(t,
		`{"names":["OPENCODE_CONFIG_CONTENT","TRACEPARENT","TRACESTATE"],"prefixes":["OTEL_"]}`, env)
	if !slices.Equal(hooked, []string{"create.before"}) {
		t.Fatalf("the plugin registered %v, want the shell's create.before", hooked)
	}
	for name, value := range env {
		if server[name] != value {
			t.Errorf("the server's own %s = %q, want %q", name, server[name], value)
		}
		taken := crewletsOwn(name) || name == "OPENCODE_CONFIG_CONTENT"
		if got, held := command[name]; held == taken || (held && got != value) {
			t.Errorf("the command started with %s=%q (held %v), want it taken %v", name, got, held, taken)
		}
	}
	if hooked, _, _ = probeToolEnv(t, `{}`, env); len(hooked) != 0 {
		t.Errorf("handed no names, the plugin registered %v", hooked)
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
