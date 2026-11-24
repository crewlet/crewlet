package executor_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
)

// claudeSettingsRecorder is a stand-in `claude` that keeps a copy of the
// settings file its launch hands it (`--settings`) and answers as finished.
const claudeSettingsRecorder = "#!/bin/sh\ncase \"$1\" in --version) echo '2.1.330 (Claude Code)'; exit 0;; esac\n" +
	"while [ $# -gt 0 ]; do [ \"$1\" = --settings ] && cp \"$2\" \"$HOME/handed-settings.json\"; shift; done\n" +
	"cat > /dev/null\necho '{\"type\":\"result\",\"subtype\":\"success\",\"is_error\":false,\"result\":\"done\"}'\n"

// A CLAUDE CODE RUN'S SETTINGS KEEP ITS BRIDGE DIRECT AND THE PERSON'S
// NO_PROXY WHOLE. Claude Code 2.1.330 assigns a settings file's `env` over the
// process's key by key and reads no_proxy first, so a home's or a project's
// value leaving the bridge's host out sent the bridge through the proxy, and
// only both spellings in the run's own settings opened it (measured). Where a
// home, project or local file sets either, the settings the run hands Claude
// Code set both to one list: the process's (the operator's entry, then the
// bridge's host the wrapper added), then every file's entries, in order. A
// run no file sets one for leaves both to the wrapper's environment.
//
// Mutations, each red: one spelling only; the settings files' entries
// dropped; no merge at launch (the holder handed as written).
func TestAClaudeCodeRunsSettingsKeepItsBridgeDirectBesideThePersonsNoProxy(t *testing.T) {
	t.Parallel()
	bridge := map[string]sandbox.MCPServer{"crewlet": {Transport: sandbox.TransportHTTP,
		URL: "http://127.0.0.1:7/mcp/run-token-placeholder", AlwaysLoaded: true}}
	for name, tc := range map[string]struct {
		plant map[string]string
		want  string
	}{
		"no settings file sets one": {plant: map[string]string{".claude/settings.json": `{"env":{"FOO":"bar"}}`}},
		"the home sets only no_proxy, leaving the bridge out": {
			plant: map[string]string{".claude/settings.json": `{"env":{"no_proxy":"home.example.internal"}}`},
			want:  "operator.example.internal,127.0.0.1,home.example.internal"},
		"the local file sets both": {
			plant: map[string]string{".claude/settings.json": `{"env":{"FOO":"bar"}}`,
				"workspace/.claude/settings.local.json": `{"env":{"NO_PROXY":"local-a.example.internal, 127.0.0.1",` +
					`"no_proxy":"local-b.example.internal"}}`},
			want: "operator.example.internal,127.0.0.1,local-a.example.internal,local-b.example.internal"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			runner, b := realBox(t)
			writeBoxStandIn(t, b, executor.PathsFor(b).BinDir()+"/claude", claudeSettingsRecorder)
			plant(t, b, tc.plant)
			handle, err := runner.Start(t.Context(), b, sandbox.RunRequest{Brief: "go", MCPServers: bridge,
				Env: map[string]string{"NO_PROXY": "operator.example.internal"}})
			if err != nil {
				t.Fatalf("Start: %v", err)
			}
			awaitOver(t, runner, b, handle, time.Minute, "a stand-in that answers at once takes a second")
			raw := boxFile(t, b, b.Home()+"/handed-settings.json")
			var settings struct {
				Env map[string]string `json:"env"`
			}
			if err := json.Unmarshal([]byte(raw), &settings); err != nil {
				t.Fatalf("the handed settings: %v\n%s", err, raw)
			}
			for _, spelling := range []string{"NO_PROXY", "no_proxy"} {
				got, ok := settings.Env[spelling]
				if ok != (tc.want != "") || got != tc.want {
					t.Errorf("the handed settings state %s %q (set %v), want %q:\n%s", spelling, got, ok, tc.want, raw)
				}
			}
		})
	}
}
