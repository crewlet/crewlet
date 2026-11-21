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
	"github.com/crewlet/crewlet/internal/redact/redacttest"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/textcut"
)

// A TURN'S RECORD AT EVERY BOUND FITS ONE MESSAGE: three phases' calls each at
// their worst, the longest answer a run's report or review may be, the trigger
// at its worst and an error past its clip, all of the most-escaping byte, and
// it publishes within the queue's ceiling, its calls held to one share and the
// rest counted by tool. Refused, it would leave the seat's live row running
// for good, since this is the record that ends it.
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
	// EACH PAST ITS ROW'S SHARES, and no further: arguments past
	// sandbox.MaxBridgeArgsBytes and answer past sandbox.MaxBridgeOutputBytes
	// once escaped, so the list is past its share whole and again with every
	// row cut, without weighing the test down more than the record it measures.
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

// A TURN'S RECORDS HOLD NO SECRET ITS WORDS CARRIED. A provider's key in the
// turn's answer, the reviewer's account of it, its failure and the guard that
// stopped it reaches every record the turn's end publishes as a marker, the
// words around it kept, as the phases' records hold them.
//
// Mutation: publish the answer or the summary unredacted (Response,
// planSummary), or clip a diagnostic before redacting it (events.Diagnostic),
// and its record holds the key.
func TestATurnsRecordsHoldNoSecretItsWordsCarried(t *testing.T) {
	t.Parallel()
	var key string
	for _, s := range redacttest.Shapes() {
		if s.Name == "anthropic-key" {
			key = s.Sample
		}
	}
	said := "deployed with\n" + key + "\nthen checked the logs"
	for name, res := range map[string]turn.Result{
		"the answer": {Decision: phase.Failed, Artifact: said,
			Breach: &turn.Breach{Kind: types.GuardMaxIter, Detail: said}},
		"the reviewer's account": {Decision: phase.Done, Artifact: "done",
			LastReview: &turn.Review{CompletedWork: said}},
	} {
		e, p, tel := failing(t)
		e.publishTurnCompleted(t.Context(), tel, runner.Spend{}, res, errors.New("execute: "+said), time.Now())
		if len(p.events) < 2 {
			t.Fatalf("%s: published %s, want the turn's records", name, typesOf(p.events))
		}
		for _, ev := range p.events {
			wire, err := json.Marshal(ev)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(wire), key[len(key)-12:]) {
				t.Errorf("%s: %s holds the key: %s", name, ev.Type, wire)
			}
		}
		summary := only[*types.TurnCompleted](t, p, "turn_completed")
		if !strings.Contains(summary.PlanSummary, "then checked") || !strings.Contains(summary.PlanSummary, "[REDACTED:") {
			t.Errorf("%s: the turn's summary is %q, want the words around a marker", name, summary.PlanSummary)
		}
	}
}

// A TURN'S RECORDS HOLD NO SECRET ITS TRIGGER'S SUMMARY CARRIED. A subject is
// somebody's words: a provider's key in a mail's subject reaches
// agent_turn_completed's prompt and trigger and turn_completed's task summary
// as a marker, while the reviewer's account the turn's result holds stays as
// the model wrote it. Where the summary's cut falls is the descriptor's own
// test (types.TestATriggersSummaryHoldsNoSecretHoweverItsCutFalls).
//
// Mutation: keep the event's summary unredacted (types.DescribeTrigger), and
// every record holds the key.
func TestATurnsRecordsHoldNoSecretItsTriggersSummaryCarried(t *testing.T) {
	t.Parallel()
	var key string
	for _, s := range redacttest.Shapes() {
		if s.Name == "anthropic-key" {
			key = s.Sample
		}
	}
	e, p, tel := failing(t)
	tel.trigger = types.DescribeTrigger(events.New(types.ExternalNotification{NotificationSource: "mail",
		Sender: "alice", Subject: "deploy key " + key + " for the release"}, events.TraceContext{}))
	review := &turn.Review{CompletedWork: "shipped with " + key}
	e.publishTurnCompleted(t.Context(), tel, runner.Spend{}, turn.Result{Decision: phase.Done,
		Artifact: "done", LastReview: review}, nil, time.Now())
	if review.CompletedWork != "shipped with "+key {
		t.Error("the reviewer's account was changed: the model's copy must stay as it came")
	}
	for _, ev := range p.events {
		wire, err := json.Marshal(ev)
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i+6 <= len(key); i++ {
			if strings.Contains(string(wire), key[i:i+6]) {
				t.Errorf("%s holds %q of the key: %.300s", ev.Type, key[i:i+6], wire)
				break
			}
		}
	}
	summary := only[*types.AgentTurnCompleted](t, p, "agent_turn_completed")
	completed := only[*types.TurnCompleted](t, p, "turn_completed")
	for field, text := range map[string]string{"prompt": summary.Prompt, "task_summary": completed.TaskSummary} {
		if !strings.HasPrefix(text, "Message from alice: deploy key [REDACTED:") {
			t.Errorf("%s is %.80q, want the subject with its key as a marker", field, text)
		}
	}
}
