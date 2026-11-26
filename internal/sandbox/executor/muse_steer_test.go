package executor_test

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
)

// museInbox is a run's notes as a real bridge serves them, rendered as a Muse
// Code hook's answer.
type museInbox struct {
	probeInbox
	mu    sync.Mutex
	asked []string
}

func (i *museInbox) Take(_ context.Context, event string, _ *sandbox.NoteCall) string {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.asked = append(i.asked, event)
	return executor.HookAnswer(executor.MuseName, event, probeTake, []executor.HookNote{{By: "ada", Message: probeNote}})
}

// museReachesTheBridgesTool is Muse Code's case of a real box's notes reaching
// the bridge (TestARealBoxsNotesReachTheBridgesRoute): the plugin and take
// config a run is written ([executor.Muse.WriteConfig]), each hook's take
// played as the note helper plays it, against a real bridge: each PostToolUse
// hook takes at the after-a-batch boundary and the Stop hook at the finish,
// each answered in Muse Code's shape with the take's token beside it.
//
// Mutation: point a hook at another config, take at Muse's own event names,
// or render the answer in another harness's shape — this goes red.
func museReachesTheBridgesTool(t *testing.T) {
	t.Parallel()
	inbox := &museInbox{}
	endpoint := probeBridge(t, inbox, &probeLedger{})
	box := sandbox.NewFakeSandbox("b")
	box.ExecFunc = func(_ context.Context, cmd string, _ sandbox.ExecOptions) (sandbox.ExecResult, error) {
		if cmd == "uname -m" {
			return sandbox.ExecResult{Stdout: "x86_64\n"}, nil
		}
		return sandbox.ExecResult{}, nil
	}
	p := executor.PathsFor(box)
	if _, err := (executor.Muse{}).WriteConfig(t.Context(), box, sandbox.RunRequest{
		MCPServers: map[string]sandbox.MCPServer{"crewlet": {Transport: sandbox.TransportHTTP, URL: endpoint}},
		Steer:      sandbox.SteerWiring{Server: "crewlet", Tool: mcpbridge.SteerTool, URL: mcpbridge.SteerURL(endpoint)},
	}, executor.Session{ID: "s"}, p); err != nil {
		t.Fatalf("WriteConfig: %v", err)
	}
	raw, _ := box.ReadFile(t.Context(), p.MuseNotesPlugin()+"/.muse-plugin/plugin.json")
	var manifest struct {
		Name         string `json:"name"`
		Capabilities struct {
			Hooks []struct {
				ID, Event string
				Command   []string
			} `json:"hooks"`
		} `json:"capabilities"`
	}
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatalf("the plugin's manifest: %v\n%s", err, raw)
	}
	raw, _ = box.ReadFile(t.Context(), p.MuseTakeConfig())
	var take struct {
		Endpoint, Tool string
	}
	if err := json.Unmarshal(raw, &take); err != nil || take.Endpoint != endpoint || take.Tool != mcpbridge.SteerTool {
		t.Fatalf("the take config %s (%v): want the run's endpoint and steer_take", raw, err)
	}
	if !handedProgram(t, box, p.MuseNoteHelper()) {
		t.Error("the run was written no note helper")
	}
	events := map[string]int{}
	for _, h := range manifest.Capabilities.Hooks {
		events[h.Event]++
		if len(h.Command) < 5 || h.Command[0] != p.MuseNoteHelper() || !slices.Equal(h.Command[1:5],
			[]string{"-adapter", "muse-hook", "-config", p.MuseTakeConfig()}) {
			t.Errorf("hook %s runs %q, want the run's note helper on its take config", h.ID, h.Command)
		}
		boundary := map[string]string{"PostToolUse": mcpbridge.SteerEventToolBatch, "Stop": mcpbridge.SteerEventFinish}[h.Event]
		answer := callHookTool(t, take.Endpoint, take.Tool, map[string]any{"event": boundary, "agent": ""})
		var got struct {
			Take   string `json:"crewlet_take"`
			Output struct {
				Event   string `json:"hookEventName"`
				Context string `json:"additionalContext"`
			} `json:"hookSpecificOutput"`
			Decision, Reason string
		}
		if err := json.Unmarshal([]byte(answer), &got); err != nil || !strings.Contains(got.Take, probeTake) {
			t.Fatalf("hook %s was answered %q (%v)", h.ID, answer, err)
		}
		switch h.Event {
		case "PostToolUse":
			if got.Output.Event != "PostToolUse" || got.Output.Context != probeNote {
				t.Errorf("hook %s was answered %+v", h.ID, got)
			}
		case "Stop":
			if got.Decision != "block" || got.Reason != probeNote {
				t.Errorf("hook %s was answered %+v", h.ID, got)
			}
		}
	}
	if events["PostToolUse"] != 3 || events["Stop"] != 1 || manifest.Name != "crewlet-notes" {
		t.Errorf("the plugin %q has hooks %v, want three PostToolUse and a Stop", manifest.Name, events)
	}
}
