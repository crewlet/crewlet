package engine

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/textcut"
)

// A TURN'S RECORD AT EVERY BOUND FITS ONE MESSAGE. It carries every phase's
// calls at once — three phases' worth here, each list at its record's worst —
// beside the turn's answer at the longest a run's report or a review's final
// artifact may be, the trigger at its worst and an error past its clip, every
// payload of the byte that escapes the most: and it is published within the
// queue's ceiling, its calls held to a record's one share and the rest counted
// by tool, rather than refused by it — which would leave the seat's live row
// running for good, since this is the record that ends it.
//
// Mutation: publish the phases' calls whole, and the record is past the
// ceiling.
func TestATurnsRecordAtEveryBoundFitsOneMessage(t *testing.T) {
	t.Parallel()
	e, p, tel := failing(t)
	tel.trigger = types.Trigger{ID: strings.Repeat("n", 64), Type: strings.Repeat("n", 64),
		Summary: textcut.Within(strings.Repeat("<", 1<<20), types.MaxTriggerSummaryBytes),
		Actor:   strings.Repeat("<", 256), Sender: strings.Repeat("<", 256)}
	for range 100 {
		tel.trigger.Origins = append(tel.trigger.Origins, types.TriggerOrigin{Kind: strings.Repeat("n", 32),
			ID: strings.Repeat("n", 256), Reason: strings.Repeat("n", 64)})
	}
	var spend runner.Spend
	answer := strings.Repeat("<", sandbox.MaxRunTextBytes)
	// EACH PAST ITS ROW'S SHARES, and no further: a row's arguments past
	// sandbox.MaxBridgeArgsBytes and its answer past sandbox.MaxBridgeOutputBytes
	// once escaped, shared by every row, so the list is past its share whole
	// and past it again with every row cut, without the case weighing its
	// own test binary down by more than the record it measures.
	name, args, output := strings.Repeat("<", 512), strings.Repeat("<", 2<<10), strings.Repeat("<", 1<<10)
	for i := range 3 * sandbox.MaxBridgeCalls {
		spend.ToolExecutions = append(spend.ToolExecutions, types.ToolExecution{
			"name": fmt.Sprintf("%d%s", i, name), "arguments": args, "result": output, "error": output,
			"success": false, "round": 1})
	}
	for i := range 4096 {
		spend.ElidedCalls = append(spend.ElidedCalls, types.ElidedCalls{Name: fmt.Sprintf("%d%s", i,
			strings.Repeat("<", 256)), Calls: 9, Failed: 3})
	}

	e.publishTurnCompleted(t.Context(), tel, spend, turn.Result{Decision: phase.Done, Artifact: answer},
		errors.New(strings.Repeat("<", 1<<20)), time.Now())

	var record *events.Event
	for _, ev := range p.events {
		if ev.Type == (types.AgentTurnCompleted{}).EventType() {
			record = ev
		}
	}
	if record == nil {
		t.Fatalf("no turn record was published (published: %s)", typesOf(p.events))
	}
	wire, err := json.Marshal(record)
	if err != nil {
		t.Fatalf("the record does not encode: %v", err)
	}
	if len(wire) > queue.MaxPayloadBytes {
		t.Fatalf("the turn's record weighs %d bytes, past the queue's %d", len(wire), queue.MaxPayloadBytes)
	}
	got := only[*types.AgentTurnCompleted](t, p, "agent_turn_completed")
	if len(got.ToolExecutions) != sandbox.MaxBridgeCalls || got.CallsElided == 0 || len(got.ElidedCalls) == 0 {
		t.Errorf("the record lists %d calls and counts %d more in %d tallies, want the cap's and the rest counted",
			len(got.ToolExecutions), got.CallsElided, len(got.ElidedCalls))
	}
}
