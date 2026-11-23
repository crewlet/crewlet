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

// piRouteReport is one line the stand-in of pi's extension API printed.
type piRouteReport struct {
	Op     string          `json:"op"`
	Result json.RawMessage `json:"result"`
}

// PI'S EXTENSION READS THE BRIDGE'S OWN NOTES ROUTE: the extension a run is
// written ([executor.Pi.WriteConfig]), its route a real bridge's (its inbox a
// stand-in of the engine's), hands the note the route answers over at the
// turn's end under the inbox id the engine gives it, whole, and posts no read:
// pi's record confirms it ([executor.PiConfirmsATake]).
//
// Mutation: rename `inbox`, `message` or `seq` on mcpbridge.SteerNote, or hand
// the note over under another id, or post the route's /read: this goes red.
func piReachesTheBridgesRoute(t *testing.T) {
	t.Parallel()
	node := nodeForNotes(t)
	note := mcpbridge.SteerNote{Seq: 3, ID: "req-7", Inbox: executor.InboxID(executor.PiName, "job-1", "req-7"),
		Message: "## A note from ada, sent while this turn was running\n\nTarget the release branch."}
	inbox := &routeInbox{notes: []mcpbridge.SteerNote{note}, reads: make(chan []string, 4)}
	endpoint := probeBridge(t, inbox, &probeLedger{})

	box := sandbox.NewFakeSandbox("b")
	p := executor.PathsFor(box)
	if _, err := (executor.Pi{}).WriteConfig(t.Context(), box, sandbox.RunRequest{
		Steer: sandbox.SteerWiring{Server: "crewlet", Tool: mcpbridge.SteerTool, URL: mcpbridge.SteerURL(endpoint)},
	}, executor.Session{}, p); err != nil {
		t.Fatalf("WriteConfig: %v", err)
	}
	extension, err := box.ReadFile(t.Context(), p.PiExtension())
	if err != nil || extension == nil {
		t.Fatalf("the run was written no extension (%v)", err)
	}
	file := filepath.Join(t.TempDir(), "extension.mjs")
	if err := os.WriteFile(file, extension, 0o600); err != nil {
		t.Fatal(err)
	}
	probe, err := filepath.Abs(executor.PiProbe)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(node, probe, file) //nolint:gosec // the suite's own probe
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
	reports := make(chan piRouteReport, 64)
	go func() {
		defer close(reports)
		scan := bufio.NewScanner(stdout)
		for scan.Scan() {
			var r piRouteReport
			_ = json.Unmarshal(scan.Bytes(), &r)
			reports <- r
		}
	}()
	expect := func(op string) piRouteReport {
		t.Helper()
		select {
		case r, ok := <-reports:
			if !ok || r.Op != op {
				t.Fatalf("the probe reported %+v, want %s", r, op)
			}
			return r
		case <-time.After(10 * time.Second): //fixedwait:liveness
			t.Fatalf("waited 10s for the probe to report %s", op)
			return piRouteReport{}
		}
	}
	expect("loaded")
	_, _ = io.WriteString(stdin, `{"op":"event","name":"agent_start"}`+"\n")
	expect("handled")
	// Each turn's end hands over what the poll admitted by then: asked again
	// until the note is there, which only a real round with the route admits.
	for range 50 {
		_, _ = io.WriteString(stdin, `{"op":"event","name":"turn_end","event":{"outcome":"completed","entries":[]}}`+"\n")
		var got struct {
			Entries []struct {
				Content string `json:"content"`
				Details struct {
					Inbox string `json:"inbox"`
				} `json:"details"`
			} `json:"entries"`
		}
		r := expect("handled")
		_ = json.Unmarshal(r.Result, &got)
		if len(got.Entries) == 0 {
			<-time.After(200 * time.Millisecond) //fixedwait:liveness
			continue
		}
		if len(got.Entries) != 1 || got.Entries[0].Content != note.Message || got.Entries[0].Details.Inbox != note.Inbox {
			t.Fatalf("the extension handed over %+v, want the note under its inbox id", got.Entries)
		}
		select {
		case ids := <-inbox.reads:
			t.Fatalf("the extension posted a read of %v; pi's record confirms a note", ids)
		default:
		}
		return
	}
	t.Fatal("the extension never handed the route's note over")
}
