package executor_test

import (
	"encoding/json"
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
)

// A CLAUDE CODE RUN'S SETTINGS KEEP ITS BRIDGE DIRECT AND THE PERSON'S
// NO_PROXY WHOLE. Claude Code 2.1.330 assigns a settings file's `env` over the
// process's, so a home's NO_PROXY leaving the bridge's host out cut the run off
// its tools (measured). The run's own settings, plain and pinned, state each
// spelling a home, project or local file sets: the one Claude Code takes
// (local over project over home), its entries in their order, with the
// bridge's host appended where it is missing. A spelling no file sets is left
// to the wrapper's environment, and a run without servers states none.
//
// Mutations, each red: state no bypass; replace the person's value with the
// hosts; take the home's file over the local one; state a spelling no file sets.
func TestAClaudeCodeRunsSettingsKeepItsBridgeDirectBesideThePersonsNoProxy(t *testing.T) {
	t.Parallel()
	bridge := map[string]sandbox.MCPServer{"crewlet": {Transport: sandbox.TransportHTTP,
		URL: "http://127.0.0.1:7/mcp/run-token-placeholder", AlwaysLoaded: true}}
	for name, tc := range map[string]struct {
		plant   map[string]string
		servers map[string]sandbox.MCPServer
		want    map[string]string
	}{
		"no settings file sets one": {servers: bridge,
			plant: map[string]string{".claude/settings.json": `{"env":{"FOO":"bar"}}`}, want: map[string]string{}},
		"the home's leaves the bridge out": {servers: bridge,
			plant: map[string]string{".claude/settings.json": `{"env":{"NO_PROXY":"home.example.internal,10.0.0.0/8"}}`},
			want:  map[string]string{"NO_PROXY": "home.example.internal,10.0.0.0/8,127.0.0.1"}},
		"the local file over the project's over the home's, each spelling its own": {servers: bridge,
			plant: map[string]string{
				".claude/settings.json":                 `{"env":{"NO_PROXY":"home.example.internal","no_proxy":"home.example.internal"}}`,
				"workspace/.claude/settings.json":       `{"env":{"no_proxy":"project.example.internal"}}`,
				"workspace/.claude/settings.local.json": `{"env":{"NO_PROXY":"local.example.internal,127.0.0.1"}}`},
			want: map[string]string{"NO_PROXY": "local.example.internal,127.0.0.1",
				"no_proxy": "project.example.internal,127.0.0.1"}},
		"a run without servers": {
			plant: map[string]string{".claude/settings.json": `{"env":{"NO_PROXY":"home.example.internal"}}`},
			want:  map[string]string{}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			b := sandbox.NewFakeSandbox("box-1")
			for rel, body := range tc.plant {
				if err := b.WriteFile(t.Context(), b.Home()+"/"+rel, []byte(body)); err != nil {
					t.Fatalf("plant %s: %v", rel, err)
				}
			}
			paths := executor.PathsFor(b)
			if _, err := (executor.ClaudeCode{}).WriteConfig(t.Context(), b,
				sandbox.RunRequest{MCPServers: tc.servers}, executor.Session{}, paths); err != nil {
				t.Fatalf("WriteConfig: %v", err)
			}
			for _, file := range []string{paths.ClaudeSettings(), paths.ClaudeSettingsPinned()} {
				raw, _ := b.ReadFile(t.Context(), file)
				var settings struct {
					Env map[string]string `json:"env"`
				}
				if err := json.Unmarshal(raw, &settings); err != nil {
					t.Fatalf("unmarshal %s: %v\n%s", file, err, raw)
				}
				for _, spelling := range []string{"NO_PROXY", "no_proxy"} {
					got, ok := settings.Env[spelling]
					want, wanted := tc.want[spelling]
					if ok != wanted || got != want {
						t.Errorf("%s states %s %q (set %v), want %q (set %v)", file, spelling, got, ok, want, wanted)
					}
				}
			}
		})
	}
}
