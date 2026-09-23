package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/memory"
)

// publishedPhases runs rec through an emitter over the in-memory queue — which
// refuses what the real broker refuses, at the same ceiling — and returns the
// phase records that were accepted.
func publishedPhases(t *testing.T, rec phaseRecord) []*types.AgentPhaseCompleted {
	t.Helper()
	q := memory.New()
	if err := q.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = q.Stop(context.Background()) })
	var mu sync.Mutex
	var got []*types.AgentPhaseCompleted
	q.AddPublishListener(func(_ context.Context, _ string, ev *events.Event) {
		if done, ok := ev.Data.(*types.AgentPhaseCompleted); ok {
			mu.Lock()
			got = append(got, done)
			mu.Unlock()
		}
	})
	var emu sync.Mutex
	e := emitter{pub: q, turn: Turn{RunID: "tn-1", AgentID: "agent-1"}, role: "Lead",
		tally: &Spend{}, mu: &emu}
	e.completed(t.Context(), rec)
	return got
}

// A PHASE RECORD TOO LARGE FOR ONE EVENT IS FIT, NOT DROPPED.
//
// Past the transport's ceiling an event is refused whole, so a phase whose
// tool calls returned more than the ceiling in total had no record at all —
// the event store and every screen lost exactly the phases that did the most.
// Fit, it keeps every call: the small results whole, the large ones cut to a
// common level, each cut marked and its whole length on its row.
func TestAPhaseRecordTooLargeForOneEventIsFitNotDropped(t *testing.T) {
	t.Parallel()
	const heavy = 300
	// Three-byte characters, so a cut through one would show.
	big := strings.Repeat("あ", 14_000)
	res := toolloop.Result{RoundsUsed: 1}
	for range heavy {
		res.Executions = append(res.Executions, toolloop.Execution{
			Round: 1, Name: "read_page", Args: map[string]any{"id": 1}, Output: big,
		})
	}
	res.Executions = append(res.Executions, toolloop.Execution{
		Round: 1, Name: "slack_post", Args: map[string]any{"channel": "C1"}, Output: "posted",
	})
	// The premise: whole, this record is past the ceiling.
	if heavy*len(big) <= queue.MaxPayloadBytes {
		t.Fatalf("the fixture is %d bytes of results, not past the %d-byte ceiling",
			heavy*len(big), queue.MaxPayloadBytes)
	}

	got := publishedPhases(t, phaseRecord{Phase: phase.Execute, Iteration: 1, Result: res})
	if len(got) != 1 {
		t.Fatalf("%d phase records were published, want the one — fit rather than dropped", len(got))
	}
	rec := got[0]
	if len(rec.ToolExecutions) != heavy+1 {
		t.Fatalf("the record carries %d tool calls, want every one of the %d", len(rec.ToolExecutions), heavy+1)
	}
	small := rec.ToolExecutions[heavy]
	if small["result"] != "posted" {
		t.Errorf("a result far under the level was cut: %q", small["result"])
	}
	if _, marked := small["result_bytes"]; marked {
		t.Error("a whole result is marked as cut")
	}
	for i, row := range rec.ToolExecutions[:heavy] {
		result, _ := row["result"].(string)
		if !strings.HasSuffix(result, "…") || !utf8.ValidString(result) {
			t.Fatalf("call %d's cut result is unmarked or broken: …%q", i, result[max(0, len(result)-9):])
		}
		if !strings.HasPrefix(big, strings.TrimSuffix(result, "…")) {
			t.Fatalf("call %d's cut result is not a head of the whole", i)
		}
		if row["result_bytes"] != len(big) {
			t.Fatalf("call %d: result_bytes = %v, want the whole result's %d", i, row["result_bytes"], len(big))
		}
		if row["arguments"] != `{"id":1}` {
			t.Fatalf("call %d's arguments were cut while its results could make the room: %v", i, row["arguments"])
		}
	}
	if !strings.Contains(rec.Notes, "record cut to fit one event") {
		t.Errorf("notes = %q, want the record to say it was fit", rec.Notes)
	}
	raw, err := json.Marshal(events.New(*rec, events.TraceContext{}))
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) > queue.MaxPayloadBytes {
		t.Errorf("the fitted record is %d bytes, past the %d-byte ceiling", len(raw), queue.MaxPayloadBytes)
	}
}

// A RECORD THAT FITS IS PUBLISHED EXACTLY AS IT WAS: the fit runs only when
// the transport refuses the whole.
func TestAPhaseRecordThatFitsIsNotTouched(t *testing.T) {
	t.Parallel()
	res := toolloop.Result{RoundsUsed: 1, Executions: []toolloop.Execution{
		{Round: 1, Name: "read_page", Output: strings.Repeat("x", 100_000)},
	}}
	got := publishedPhases(t, phaseRecord{Phase: phase.Execute, Iteration: 1, Result: res, Notes: "missing: jira"})
	if len(got) != 1 {
		t.Fatalf("%d records published", len(got))
	}
	row := got[0].ToolExecutions[0]
	if row["result"] != strings.Repeat("x", 100_000) {
		t.Error("a record that fits had a result cut")
	}
	if _, marked := row["result_bytes"]; marked {
		t.Error("a record that fits carries a cut mark")
	}
	if got[0].Notes != "missing: jira" {
		t.Errorf("notes = %q, want the phase's own", got[0].Notes)
	}
}

// THE LEVEL LEAVES THE MOST OF EVERY TEXT: the longest are cut to one level
// and every text at or under it is left whole.
func TestTheWaterLevelCutsOnlyTheLongest(t *testing.T) {
	t.Parallel()
	slots := []*textSlot{
		{text: strings.Repeat("a", 1000)},
		{text: strings.Repeat("b", 600)},
		{text: strings.Repeat("c", 100)},
	}
	// Shedding 500 bytes, with each first cut charged its overhead: the two
	// longest go to one level and the short one is untouched.
	level, ok := waterLevel(slots, 500)
	if !ok {
		t.Fatal("a tier with room to shed reported none")
	}
	shed := 0
	for _, s := range slots {
		if len(s.text) > level {
			shed += len(s.text) - level - phaseCutOverhead
		}
	}
	if shed < 500 {
		t.Errorf("level %d sheds %d bytes, want at least 500", level, shed)
	}
	if level <= 100 {
		t.Errorf("level %d cuts the 100-byte text too, where the two longest could shed it", level)
	}
	if level >= 600 {
		t.Errorf("level %d leaves the 600-byte text whole; it cannot shed 500 from one text", level)
	}
	if _, ok := waterLevel([]*textSlot{{text: "…"}}, 10); ok {
		t.Error("a tier holding nothing but marks reported room to shed")
	}
}

// refusingPublisher refuses every event as too large, the way a NATS server
// configured below the contract's ceiling refuses an event within it.
type refusingPublisher struct {
	mu       sync.Mutex
	attempts int
}

func (p *refusingPublisher) Publish(context.Context, string, *events.Event) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.attempts++
	return fmt.Errorf("publish: the server accepts less than this: %w", queue.ErrTooLarge)
}

// A RECORD REFUSED WITHIN THE CEILING IS NOT "FIT".
//
// A record that already fits the contract's ceiling and is refused anyway was
// refused by a server configured below it. No cut answers that: the fit finds
// nothing over the ceiling, cuts nothing, and publishing the same bytes again
// is refused the same way — reported as a fit that cut no text, which is the
// one account of the loss that points nowhere near its cause.
func TestARecordRefusedWithinTheCeilingIsNotSentAgainAsFit(t *testing.T) {
	t.Parallel()
	pub := &refusingPublisher{}
	var emu sync.Mutex
	e := emitter{pub: pub, turn: Turn{RunID: "tn-1", AgentID: "agent-1"}, role: "Lead",
		tally: &Spend{}, mu: &emu}
	e.completed(t.Context(), phaseRecord{Phase: phase.Execute, Iteration: 1, Result: toolloop.Result{
		RoundsUsed: 1, Executions: []toolloop.Execution{{Round: 1, Name: "read_page", Output: "a page"}},
	}})
	if pub.attempts != 1 {
		t.Errorf("the record was published %d times, want once: a record within the ceiling has "+
			"nothing a fit can cut", pub.attempts)
	}
}
