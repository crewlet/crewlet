package executor_test

import (
	"context"
	"encoding/json"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"

	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
)

// grokInbox is a run's notes as a real bridge serves them, rendered as a Grok
// Build hook's answer.
type grokInbox struct{ probeInbox }

func (*grokInbox) Take(_ context.Context, event string, _ *sandbox.NoteCall) string {
	return executor.HookAnswer(executor.GrokName, event, probeTake, []executor.HookNote{{By: "ada", Message: probeNote}})
}

// grokSettingsHooks is each event's first hook command in a run's settings,
// as Grok Build reads its config.toml, by event.
func grokSettingsHooks(t *testing.T, settings []byte) map[string]string {
	t.Helper()
	var read struct {
		Hooks map[string][]struct {
			Hooks []struct {
				Type, Command string
				Timeout       int
			}
		}
	}
	if _, err := toml.Decode(string(settings), &read); err != nil {
		t.Fatalf("the settings are no TOML: %v\n%s", err, settings)
	}
	commands := map[string]string{}
	for event, entries := range read.Hooks {
		if len(entries) == 0 || len(entries[0].Hooks) != 1 || entries[0].Hooks[0].Type != "command" ||
			entries[0].Hooks[0].Timeout != 10 {
			t.Fatalf("the %s hooks are %+v, want the run's one command first, waited on 10 s", event, entries)
		}
		commands[event] = entries[0].Hooks[0].Command
	}
	return commands
}

// grokReachesTheBridgesTool is Grok Build's case of a real box's notes reaching
// the bridge (TestARealBoxsNotesReachTheBridgesRoute): the settings and take
// config a run is written ([executor.Grok.WriteConfig]), each hook's take
// played as the note helper plays it, against a real bridge: PostToolUse and
// PostToolUseFailure take at the after-a-batch boundary and Stop at the
// finish, each answered in Grok's shape, the note then the take's token.
//
// Mutation: point a hook at another config, drop a hook, or render the answer
// in another harness's shape.
func grokReachesTheBridgesTool(t *testing.T) {
	t.Parallel()
	endpoint := probeBridge(t, &grokInbox{}, &probeLedger{})
	box := sandbox.NewFakeSandbox("b")
	box.ExecFunc = func(_ context.Context, cmd string, _ sandbox.ExecOptions) (sandbox.ExecResult, error) {
		if cmd == "uname -m" {
			return sandbox.ExecResult{Stdout: "x86_64\n"}, nil
		}
		return sandbox.ExecResult{}, nil
	}
	p := executor.PathsFor(box)
	if _, err := (executor.Grok{}).WriteConfig(t.Context(), box, sandbox.RunRequest{
		MCPServers: map[string]sandbox.MCPServer{"crewlet": {Transport: sandbox.TransportHTTP, URL: endpoint}},
		Steer:      sandbox.SteerWiring{Server: "crewlet", Tool: mcpbridge.SteerTool, URL: mcpbridge.SteerURL(endpoint)},
	}, executor.Session{ID: "s"}, p); err != nil {
		t.Fatalf("WriteConfig: %v", err)
	}
	settings, _ := box.ReadFile(t.Context(), p.GrokConfig())
	raw, _ := box.ReadFile(t.Context(), p.GrokTakeConfig())
	var take struct{ Endpoint, Tool string }
	if err := json.Unmarshal(raw, &take); err != nil || take.Endpoint != endpoint || take.Tool != mcpbridge.SteerTool {
		t.Fatalf("the take config %s (%v): want the run's endpoint and steer_take", raw, err)
	}
	if !handedProgram(t, box, p.GrokNoteHelper()) {
		t.Error("the run was written no note helper")
	}
	hooks := grokSettingsHooks(t, settings)
	for _, event := range slices.Sorted(maps.Keys(hooks)) {
		command := hooks[event]
		want := "'" + p.GrokNoteHelper() + "' '-adapter' 'grok-hook' '-config' '" + p.GrokTakeConfig() + "'"
		if !strings.HasPrefix(command, want) {
			t.Errorf("the %s hook runs %q, want the run's note helper on its take config", event, command)
		}
		boundary := mcpbridge.SteerEventToolBatch
		if event == "Stop" {
			boundary = mcpbridge.SteerEventFinish
		}
		answer := callHookTool(t, take.Endpoint, take.Tool, map[string]any{"event": boundary, "agent": ""})
		var got struct {
			Take   string `json:"crewlet_take"`
			Output struct {
				Context string `json:"additionalContext"`
			} `json:"hookSpecificOutput"`
			Decision, Reason string
		}
		if err := json.Unmarshal([]byte(answer), &got); err != nil || !strings.Contains(got.Take, probeTake) {
			t.Fatalf("the %s hook was answered %q (%v)", event, answer, err)
		}
		handed := got.Output.Context
		if event == "Stop" {
			handed = got.Reason
			if got.Decision != "block" {
				t.Errorf("the finish was answered %+v, want a block", got)
			}
		}
		if handed != probeNote+"\n\n"+got.Take {
			t.Errorf("the %s hook hands %q, want the note then its token", event, handed)
		}
	}
	if got := slices.Sorted(maps.Keys(hooks)); !slices.Equal(got, []string{"PostToolUse", "PostToolUseFailure", "Stop"}) {
		t.Errorf("the settings' hooks are %v, want PostToolUse, PostToolUseFailure and Stop", got)
	}
}
