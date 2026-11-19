package assistant_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/assistant/assistanttest"
	"github.com/crewlet/crewlet/internal/assistant/catalog"
	"github.com/crewlet/crewlet/internal/assistant/evals"
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
			readTask(), evals.Call("c2", catalog.ReadAPITool, taskArgs("ENG-7")),
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
