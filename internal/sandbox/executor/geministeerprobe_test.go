package executor_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
)

// geminiProbeInbox is a note inbox answering every take with one note, in
// Gemini CLI's own answer, and keeping the boundaries it was asked at.
type geminiProbeInbox struct {
	probeInbox
	mu     sync.Mutex
	events []string
}

func (i *geminiProbeInbox) Take(_ context.Context, event string, _ *sandbox.NoteCall) string {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.events = append(i.events, event)
	return executor.HookAnswer(executor.GeminiCLIName, event, probeTake, []executor.HookNote{{By: "ada", Message: probeNote}})
}

func (i *geminiProbeInbox) asked() []string {
	i.mu.Lock()
	defer i.mu.Unlock()
	return slices.Clone(i.events)
}

// geminiTakeReachesTheBridgeTool is Gemini CLI's case of the note contract's
// real route (TestARealBoxsNotesReachTheBridgesRoute). A run in a real `direct`
// box writes its settings, and each hook they name is run as Gemini CLI runs
// it, the take script under node with the hook's JSON on stdin, against a real
// bridge: AfterAgent takes at the finish, hands Gemini CLI the note as the
// block's reason without the take's token, and records the token beside the
// text's digest; AfterTool takes nothing; a finish continuing on a block takes
// a waiting note again (0.70.0 delivers a second block: session-twin-end).
//
// Mutations, each red: name the finish by another boundary than the bridge's;
// take at AfterTool; read the bridge's address from anywhere but the take
// script's configuration; take nothing while stop_hook_active.
func geminiTakeReachesTheBridgeTool(t *testing.T) {
	t.Parallel()
	node := geminiNode(t)
	inbox := &geminiProbeInbox{}
	endpoint := probeBridge(t, inbox, &probeLedger{})
	record := t.TempDir()
	runner, box := geminiBox(t, `cp "$HOME/.gemini/settings.json" '`+record+`/settings.json'
cat > '`+record+`/stdin'
echo '{"type":"result","status":"success"}'`)
	p := executor.PathsFor(box)
	waitRun(t, runner, box, sandbox.RunRequest{Brief: "fix the flake", System: "You are the seat.",
		MCPServers: map[string]sandbox.MCPServer{"crewlet": {Transport: sandbox.TransportHTTP, URL: endpoint}},
		Steer: sandbox.SteerWiring{Server: "crewlet", Tool: mcpbridge.SteerTool,
			URL: mcpbridge.SteerURL(endpoint)}})

	var settings struct {
		Hooks map[string][]struct {
			Hooks []struct {
				Type, Command string
				Timeout       int
			} `json:"hooks"`
		} `json:"hooks"`
	}
	readJSON(t, filepath.Join(record, "settings.json"), &settings)
	// THE RUN'S RECORD OF TAKES EXISTS, EMPTY, BEFORE ANY HOOK FIRES, so a
	// take never recorded reads absent once the job ends rather than undecided.
	if before, err := box.ReadFile(t.Context(), p.GeminiTakes()); err != nil || len(before) != 0 {
		t.Errorf("before any hook the record of takes is %q (%v), want an empty file", before, err)
	}
	// EACH HOOK, PLAYED AS THE HARNESS PLAYS IT: its command, in sh, the
	// hook's input on stdin.
	play := func(event string, input map[string]any) []string {
		var answers []string
		for _, matcher := range settings.Hooks[event] {
			for _, hook := range matcher.Hooks {
				if hook.Type != "command" || hook.Timeout != 10000 {
					t.Fatalf("a %s hook is %q under %d ms, want a command under 10,000", event, hook.Type, hook.Timeout)
				}
				raw, _ := json.Marshal(input)
				cmd := exec.CommandContext(t.Context(), "sh", "-c", hook.Command)
				cmd.Env = append(os.Environ(), "PATH="+filepath.Dir(node)+":"+os.Getenv("PATH"))
				cmd.Stdin = strings.NewReader(string(raw))
				out, err := cmd.Output()
				if err != nil {
					t.Fatalf("the %s hook: %v", event, err)
				}
				answers = append(answers, string(out))
			}
		}
		return answers
	}
	if answers := play("AfterTool", map[string]any{"hook_event_name": "AfterTool", "tool_name": "mcp_crewlet_echo",
		"tool_response": map[string]any{"llmContent": "echo: hello"}}); len(answers) != 0 {
		t.Errorf("a run handed no skill has AfterTool hooks answering %q", answers)
	}
	finish := map[string]any{"hook_event_name": "AfterAgent", "stop_hook_active": false}
	answers := play("AfterAgent", finish)
	if len(answers) != 1 {
		t.Fatalf("AfterAgent has %d hooks, want one", len(answers))
	}
	answers0 := answers[0]
	var answer map[string]any
	if err := json.Unmarshal([]byte(answers[0]), &answer); err != nil {
		t.Fatalf("the AfterAgent hook's answer is not a hook's JSON: %v\n%s", err, answers[0])
	}
	if answer["decision"] != "block" || answer["reason"] != probeNote || len(answer) != 2 {
		t.Errorf("the finish was answered %v, want a block with the note as its reason and nothing else", answer)
	}
	recorded, err := box.ReadFile(t.Context(), p.GeminiTakes())
	if err != nil || !strings.Contains(string(recorded), `"take":"`+probeTake+`","digest":"`+
		executor.ItemDigest(probeNote)+`"`) {
		t.Errorf("the take script recorded %q (%v), want the take's token beside its text's digest", recorded, err)
	}
	continuing := map[string]any{"hook_event_name": "AfterAgent", "stop_hook_active": true}
	if answers := play("AfterAgent", continuing); len(answers) != 1 || answers[0] != answers0 {
		t.Errorf("a finish continuing on a block, a note waiting, was answered %q, want the block %q", answers, answers0)
	}
	if got := inbox.asked(); !slices.Equal(got, []string{mcpbridge.SteerEventFinish, mcpbridge.SteerEventFinish}) {
		t.Errorf("the inbox was asked at %v, want both finishes", got)
	}
}
