package sandbox

import (
	"fmt"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/events"
)

// A COLLECTED RUN IS FITTED IN ITS OWN TRACE, under its own decision, whatever
// context the completion was delivered on and whatever trace its envelope
// names.
//
// The queue hands a consumer a context with no span, and the collection's
// condensation of a report too long to carry is a model call on the seat's
// auxiliary chain: the engine's condenser opens `llm.auxiliary` beneath
// whatever span ctx holds, the SDK's span beneath that, and files the
// `auxiliary_spend` record in that trace. Handed no span, each call rooted a
// trace of its own, decided by the root rule afresh, and one condensation
// became as many spend records as it made calls. So the condenser must be
// handed the RUN's span — the one the run was launched from, as the claimed
// row names it, never the envelope's — remote, and sampled exactly as the run
// was. And a line logged before anything is claimed (a completion that is not
// this delivery's to run) is filed in the delivery's trace.
//
// Not parallel: it captures the process-wide logger ([captureLogs]).
//
// Mutations: drop the restore after the claim in OnCompleted (the condenser is
// handed the envelope's trace); drop the restore in OnEvent (the
// already-claimed line carries no trace id).
func TestACollectedRunIsCondensedInItsOwnTrace(t *testing.T) {
	const (
		traceID = "4bf92f3577b34da6a3ce929d0e0e4736"
		spanID  = "00f067aa0ba902b7"
		foreign = "0af7651916cd43dd8448eb211c80319c"
	)
	for _, unsampled := range []bool{false, true} {
		t.Run(fmt.Sprintf("unsampled=%v", unsampled), func(t *testing.T) {
			logs := captureLogs(t)
			rig := newCoordRig(t)
			rig.trace = events.TraceContext{TraceID: traceID, SpanID: spanID, Unsampled: unsampled}
			condenser := &fakeCondenser{cost: condensing,
				answer: func(part RunPart, _ string, _ int) (string, error) {
					return "(condensed) the " + string(part), nil
				}}
			rig.coordinator.condense = condenser
			run := rig.launch("t1")
			rig.coordinator.countRun("swe", StatusRunning)
			rig.runner.Finish(Result{Success: true, Text: lines("report", 7000)})

			// An envelope naming ANOTHER trace: the row the claim reads
			// is the authority, not what a publisher stamped.
			payload, _ := rig.completion("t1")
			if err := rig.coordinator.OnEvent(t.Context(),
				events.New(payload, events.TraceContext{TraceID: foreign})); err != nil {
				t.Fatalf("OnEvent: %v", err)
			}
			if len(condenser.spans) == 0 {
				t.Fatal("a report past the record's bound was not condensed — the premise")
			}
			for i, sc := range condenser.spans {
				if sc.TraceID().String() != traceID || sc.SpanID().String() != spanID ||
					!sc.IsRemote() || sc.IsSampled() == unsampled {
					t.Errorf("condensation %d ran beneath trace=%s span=%s remote=%v sampled=%v, "+
						"want the run's own %s/%s sampled=%v", i, sc.TraceID(), sc.SpanID(),
						sc.IsRemote(), sc.IsSampled(), traceID, spanID, !unsampled)
				}
			}

			// The completion again, as the waiter publishes it: beneath
			// the span the run was launched from. Nothing is claimed — the
			// run is over — so what is left to see is the line saying so.
			if err := rig.coordinator.OnEvent(t.Context(),
				events.New(payload, run.Trace().Beneath())); err != nil {
				t.Fatalf("OnEvent (again): %v", err)
			}
			var claimed string
			for line := range strings.SplitSeq(logs.String(), "\n") {
				if strings.Contains(line, "sandbox_completion_already_claimed") {
					claimed = line
				}
			}
			if claimed == "" {
				t.Fatalf("the second completion was not refused as claimed: %q", logs.String())
			}
			if !strings.Contains(claimed, "trace_id="+traceID) {
				t.Errorf("a line about the run is filed in no trace: %q", claimed)
			}
		})
	}
}
