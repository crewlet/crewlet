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
// take at AfterTool; read the bridge's address elsewhere than the take config;
// take nothing while stop_hook_active; name the record after another session.
func geminiTakeReachesTheBridgeTool(t *testing.T) {
	t.Parallel()
	node := geminiNode(t)
	inbox := &geminiProbeInbox{}
	endpoint := probeBridge(t, inbox, &probeLedger{})
	record := t.TempDir()
	runner, box := geminiBox(t, `cp "$HOME/.gemini/settings.json" '`+record+`/settings.json'
printf '%s\n' "$@" > '`+record+`/args'
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
	// THE HOOK RECORDS IN THE RECORD OF THE SESSION GEMINI CLI WAS HANDED, which
	// opens with its start line before any hook fires, holding where the run
	// began in the session (its first line, for a session the run starts): a
	// take never recorded reads absent once the job ends rather than undecided.
	args, err := os.ReadFile(filepath.Join(record, "args"))
	if err != nil {
		t.Fatal(err)
	}
	session := ""
	if _, after, ok := strings.Cut(string(args), "--session-id\n"); ok {
		session, _, _ = strings.Cut(after, "\n")
	}
	var take struct{ Takes string }
	readJSON(t, p.GeminiTakeConfig(), &take)
	takes := take.Takes
	if session == "" || takes != p.GeminiTakes(session) {
		t.Fatalf("the hook records in %q, want the record of the session Gemini CLI was handed (%q)", takes, session)
	}
	if before, err := box.ReadFile(t.Context(), takes); err != nil || string(before) != `{"from":0}`+"\n" {
		t.Errorf("before any hook the record of takes is %q (%v), want the run's start alone", before, err)
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
	// A RUN HANDED NO SKILL RUNS NO TAKE AFTER A TOOL: its one AfterTool hook
	// is the checkouts' ([geminiCheckoutsEntry]), which, with no checkout
	// cloned, answers nothing.
	for _, matcher := range settings.Hooks["AfterTool"] {
		for _, hook := range matcher.Hooks {
			if strings.Contains(hook.Command, p.GeminiTake()) {
				t.Errorf("a run handed no skill runs its take script after a tool: %s", hook.Command)
			}
		}
	}
	if answers := play("AfterTool", map[string]any{"hook_event_name": "AfterTool", "tool_name": "mcp_crewlet_echo",
		"tool_response": map[string]any{"llmContent": "echo: hello"}}); slices.ContainsFunc(answers,
		func(a string) bool { return a != "" }) {
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
	recorded, err := box.ReadFile(t.Context(), takes)
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

// A JOB'S RECORD OF TAKES OUTLIVES THE NEXT LAUNCH IN ITS BOX. A job whose
// session shows one of its two takes delivered has ended, and another job
// launches in the same real box, clearing every run's artifacts; the first
// job's receipts, asked after, still read its own record: the delivered take
// held, the other absent (served again, never told twice).
//
// Mutations, each red: clear the records' directory with the run's artifacts;
// read a job's receipts off the record of the box's latest run.
func TestAJobsRecordOfTakesOutlivesTheNextLaunchInItsBox(t *testing.T) {
	t.Parallel()
	const session = "11111111-2222-4333-8444-555555555555"
	const delivered = "## A note from Lead, sent while this turn was running: CAP-TWIN-END"
	runner, box := geminiBox(t, `echo '{"type":"result","status":"success"}'`)
	p := executor.PathsFor(box)
	chat, err := os.ReadFile(filepath.Join("testdata", "gemini", "0.70.0", "session-twin-end.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	for path, content := range map[string]string{
		p.GeminiChats() + "/session-2026-11-21T22-24-11111111.jsonl": string(chat),
		p.GeminiTakes(session): `{"from":0}` + "\n" + `{"take":"take-a","digest":"` + executor.ItemDigest(delivered) + `"}` + "\n" +
			`{"take":"take-b","digest":"` + executor.ItemDigest("## A note never handed over") + `"}` + "\n",
	} {
		if err := box.WriteFile(t.Context(), path, []byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	endpoint := probeBridge(t, &geminiProbeInbox{}, &probeLedger{})
	waitRun(t, runner, box, sandbox.RunRequest{Brief: "the next job",
		MCPServers: map[string]sandbox.MCPServer{"crewlet": {Transport: sandbox.TransportHTTP, URL: endpoint}},
		Steer: sandbox.SteerWiring{Server: "crewlet", Tool: mcpbridge.SteerTool,
			URL: mcpbridge.SteerURL(endpoint)}})
	got := runner.ReadMainThread(t.Context(), box, sandbox.MainThreadAsk{Session: session,
		Takes: []sandbox.AskedTake{{Take: "take-a"}, {Take: "take-b"}}, Ended: true}).Receipts
	if !slices.Equal(got.Held, []string{"take-a"}) || !slices.Equal(got.Absent, []string{"take-b"}) {
		t.Errorf("after the next launch the first job's takes read held %q, absent %q (%s); "+
			"want take-a held, take-b absent", got.Held, got.Absent, got.Why)
	}
}
