package executor_test

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
)

// copilotRealRouteReport is one line the stand-in SDK printed.
type copilotRealRouteReport struct {
	Op      string         `json:"op"`
	Input   map[string]any `json:"input"`
	Message string         `json:"message"`
}

// COPILOT'S EXTENSION READS THE BRIDGE'S OWN NOTES ROUTE: the extension a run
// is written ([executor.Copilot.WriteConfig]) in a real box, so its state file
// is where the run keeps it, its route a real bridge's (its
// inbox a stand-in of the engine's), sends the note the route answers under the
// clientTurnId of the inbox id the engine gives it, polls past it, and posts no
// read: Copilot's record confirms it ([executor.CopilotConfirmsATake]).
//
// Mutation: rename `inbox`, `message` or `seq` on mcpbridge.SteerNote, or send
// under another id, or post the route's /read — this goes red.
func copilotReachesTheBridgesRoute(t *testing.T) {
	t.Parallel()
	node := nodeForNotes(t)
	note := mcpbridge.SteerNote{Seq: 3, ID: "req-7", Inbox: executor.InboxID(executor.CopilotName, "job-1", "req-7"),
		Message: "## A note from ada, sent while this turn was running\n\nTarget the release branch."}
	inbox := &routeInbox{notes: []mcpbridge.SteerNote{note}, reads: make(chan []string, 4)}
	endpoint := probeBridge(t, inbox, &probeLedger{})

	_, box := standInBox(t, executor.NewCopilot(), nil)
	p := executor.PathsFor(box)
	if _, err := (executor.Copilot{}).WriteConfig(t.Context(), box, sandbox.RunRequest{
		Steer: sandbox.SteerWiring{Server: "crewlet", Tool: mcpbridge.SteerTool, URL: mcpbridge.SteerURL(endpoint)},
	}, executor.Session{}, p); err != nil {
		t.Fatalf("WriteConfig: %v", err)
	}
	extension, err := box.ReadFile(t.Context(), p.CopilotExtension()+"/extension.mjs")
	if err != nil || extension == nil {
		t.Fatalf("the run was written no extension (%v)", err)
	}
	stub, err := os.ReadFile(executor.CopilotProbeSDK)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	sdk := filepath.Join(dir, "node_modules", "@microsoft", "copilot-sdk")
	for name, body := range map[string][]byte{
		filepath.Join(sdk, "package.json"): []byte(`{"name":"@microsoft/copilot-sdk","type":"module",` +
			`"exports":{"./extension":"./extension.mjs"}}`),
		filepath.Join(sdk, "extension.mjs"):        stub,
		filepath.Join(dir, "ext", "extension.mjs"): extension,
	} {
		if err := os.MkdirAll(filepath.Dir(name), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(name, body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command(node, filepath.Join(dir, "ext", "extension.mjs")) //nolint:gosec // the suite's own probe
	cmd.Env = append(os.Environ(), "NODE_NO_WARNINGS=1")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = stdin.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	reports := make(chan copilotRealRouteReport, 64)
	go func() {
		defer close(reports)
		scan := bufio.NewScanner(stdout)
		for scan.Scan() {
			var r copilotRealRouteReport
			_ = json.Unmarshal(scan.Bytes(), &r)
			reports <- r
		}
	}()
	expect := func(op string) copilotRealRouteReport {
		t.Helper()
		select {
		case r, ok := <-reports:
			if !ok || r.Op != op {
				t.Fatalf("the probe reported %+v, want %s", r, op)
			}
			return r
		case <-time.After(10 * time.Second): //fixedwait:liveness
			t.Fatalf("waited 10s for the probe to report %s", op)
			return copilotRealRouteReport{}
		}
	}
	expect("joined")
	expect("log")
	_, _ = io.WriteString(stdin, `{"op":"event","event":{"type":"assistant.turn_start","data":{}}}`+"\n")
	sent := expect("send")
	if sent.Input["clientTurnId"] != executor.CopilotClientTurnID(note.Inbox) || sent.Input["prompt"] != note.Message {
		t.Fatalf("the extension sent %v, want the note under its inbox id", sent.Input)
	}
	select {
	case r := <-reports:
		t.Fatalf("the extension did more after the one note: %+v", r)
	case ids := <-inbox.reads:
		t.Fatalf("the extension posted a read of %v; Copilot's record confirms a note", ids)
	case <-time.After(1500 * time.Millisecond): //fixedwait:liveness
	}
}
