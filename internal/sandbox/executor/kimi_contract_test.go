package executor_test

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
)

// KIMI'S DRIVER READS THE BRIDGE'S OWN NOTES ROUTE: the route the run is written
// ([executor.Kimi.WriteConfig]'s settings), a real bridge's (its inbox a
// stand-in of the engine's), is polled by the driver, which steers the note it
// answers under the prompt_id of the inbox id the engine gives it, and posts no
// read: Kimi's record confirms it ([executor.KimiConfirmsATake]).
//
// Mutation: rename `inbox`, `message` or `seq` on mcpbridge.SteerNote, steer
// under another id, write the route under another settings key, or post the
// route's /read — this goes red.
func kimiReachesTheBridgesRoute(t *testing.T) {
	t.Parallel()
	note := mcpbridge.SteerNote{Seq: 3, ID: "req-7", Inbox: executor.InboxID(executor.KimiName, "job-1", "req-7"),
		Message: "## A note from ada, sent while this turn was running\n\nTarget the release branch."}
	inbox := &routeInbox{notes: []mcpbridge.SteerNote{note}, reads: make(chan []string, 4)}
	endpoint := probeBridge(t, inbox, &probeLedger{})

	box := sandbox.NewFakeSandbox("b")
	p := executor.PathsFor(box)
	if _, err := (executor.Kimi{}).WriteConfig(t.Context(), box, sandbox.RunRequest{
		Steer: sandbox.SteerWiring{Server: "crewlet", Tool: mcpbridge.SteerTool, URL: mcpbridge.SteerURL(endpoint)},
	}, p); err != nil {
		t.Fatalf("WriteConfig: %v", err)
	}
	raw, err := box.ReadFile(t.Context(), p.KimiSettings())
	if err != nil {
		t.Fatal(err)
	}
	var settings struct {
		Notes string `json:"notes"`
	}
	if err := json.Unmarshal(raw, &settings); err != nil || settings.Notes == "" {
		t.Fatalf("the run was written no notes route (%v): %s", err, raw)
	}
	steers := executor.KimiSteersOffRoute(t, settings.Notes)
	if !slices.Equal(steers, []string{executor.KimiNoteID(note.Inbox) + " " + note.Message}) {
		t.Fatalf("the driver steered %q, want the note under its inbox id", steers)
	}
	select {
	case ids := <-inbox.reads:
		t.Fatalf("the driver posted a read of %v; Kimi's record confirms a note", ids)
	default:
	}
}
