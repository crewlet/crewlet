package assistant_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/assistant/assistanttest"
	"github.com/crewlet/crewlet/internal/assistant/catalog"
	"github.com/crewlet/crewlet/internal/assistant/evals"
	"github.com/crewlet/crewlet/internal/assistant/exec"
	"github.com/crewlet/crewlet/internal/assistant/wire"
	"github.com/crewlet/crewlet/internal/providers/llm"
)

// THE SAME READ IS NOT SENT OR DRAWN TWICE IN ONE SEGMENT: a model that asks
// for exactly what it already read is handed that answer again, and the
// person sees one step and one card, not the same card twice (the "Listing
// tasks" three times report). A read with other arguments is another read.
// Mutation: drop the memo and two requests are sent and two cards drawn.
func TestTheSameReadIsNotSentTwiceInOneSegment(t *testing.T) {
	t.Parallel()
	f := assistanttest.NewRig(t, evals.RigOptions{
		Tools: []catalog.Tool{getTask(t)}, Handler: task, Rounds: []evals.Round{
			readTask(), evals.Call("c2", "get_task", map[string]any{"item": "ENG-7"}),
			evals.Say("ENG-7 is about rotating the signing keys.")}})
	accepted, err := f.Ask("twice", "What is ENG-7?", nil)
	if err != nil {
		t.Fatal(err)
	}
	frames := f.Follow(accepted.TurnID, 0)
	if sent := len(f.Dispatcher.Sent()); sent != 1 {
		t.Errorf("%d requests sent, want the one read", sent)
	}
	drawn := 0
	for _, fr := range frames {
		if fr.Kind == wire.EventCard {
			drawn++
		}
	}
	if drawn != 1 {
		t.Errorf("%d cards drawn, want one", drawn)
	}
	var results []string
	for _, msg := range f.Model.Seen() {
		if msg.Role == llm.RoleTool {
			results = append(results, msg.Content)
		}
	}
	if len(results) != 2 || !strings.HasPrefix(results[1], "You already made this exact read") ||
		!strings.Contains(results[1], "Rotate") {
		t.Errorf("the second read was answered %q", results)
	}
}

// A NAME IS RESOLVED BEFORE THE READ IS SENT: a seat asked for by its name is
// read by its handle, through the person's own chart, and a name nothing on
// the chart answers to is refused before anything is sent, listing the seats.
// Mutation: send the argument as written and the first request reads
// /agents/QA%20Engineer.
func TestASeatNamedByItsNameIsReadByItsHandle(t *testing.T) {
	t.Parallel()
	world := assistanttest.Routes(map[string]exec.Response{
		"GET /org": evals.JSON(map[string]any{"timezone": "UTC", "roles": []any{
			map[string]any{"handle": "qa", "name": "QA Engineer", "kind": "agent"}}}),
		"GET /agents/qa": evals.JSON(map[string]any{"handle": "qa", "role": "QA Engineer",
			"live": map[string]any{"activity": "idle"}}),
	})
	f := assistanttest.NewRig(t, evals.RigOptions{
		Tools: []catalog.Tool{assistanttest.Tool(t, "get_agent")}, Handler: world,
		Rounds: []evals.Round{
			evals.Call("c1", "get_agent", map[string]any{"handle": "QA Engineer"}),
			evals.Call("c2", "get_agent", map[string]any{"handle": "Head of Fun"}),
			evals.Say("The QA Engineer is idle.")}})
	accepted, err := f.Ask("named", "What is the QA Engineer doing?", nil)
	if err != nil {
		t.Fatal(err)
	}
	f.Follow(accepted.TurnID, 0)
	var paths []string
	for _, s := range f.Dispatcher.Sent() {
		paths = append(paths, s.Request.Path)
	}
	if strings.Join(paths, " ") != "/org /agents/qa" {
		t.Errorf("sent %v, want the chart once and the seat by its handle", paths)
	}
	var results []string
	for _, msg := range f.Model.Seen() {
		if msg.Role == llm.RoleTool {
			results = append(results, msg.Content)
		}
	}
	if len(results) != 2 || !strings.Contains(results[1], "The seats are: QA Engineer (qa)") {
		t.Errorf("the unknown name was answered %q", results)
	}
}
