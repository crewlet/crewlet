package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/auxspend"
	"github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	queuememory "github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/tracing/tracingtest"
)

// signalledResume records that a run was resumed, and resumes nothing.
type signalledResume struct{ done chan struct{} }

func (r signalledResume) Resume(context.Context, sandbox.ResumeRequest) error {
	close(r.done)
	return nil
}

// A COLLECTED CODING RUN'S CONDENSATION IS FILED IN THE RUN'S TRACE, as ONE
// record, under the run's own sampling decision — end to end, through the
// seat's real control-topic handler, the engine's condenser and its spend
// ledger.
//
// The completion reaches the handler on the queue consumer's context, which
// holds no span. A report past the record's bound is condensed by the seat's
// auxiliary chain in several calls, and each of them used to open
// `llm.auxiliary` as the ROOT of a trace of its own: the root rule decided each
// afresh — an unsampled run's condensation exported — and since a spend record
// coalesces only the calls of one trace, one condensation was published as one
// record per call, each in a trace no screen of the run's could reach.
//
// Mutation: restore no trace in sandbox.Coordinator — neither the delivery's in
// OnEvent nor the row's after the claim in OnCompleted — and the report's one
// condensation is filed as one record per call (24 measured), each in a trace
// of its own and decided sampled. Each restore alone is pinned by
// internal/sandbox's TestACollectedRunIsCondensedInItsOwnTrace; here the
// envelope names the run's trace, so either one would hold this green.
func TestACollectedRunsCondensationIsOneRecordInTheRunsTrace(t *testing.T) {
	t.Parallel()
	for _, unsampled := range []bool{false, true} {
		t.Run(fmt.Sprintf("unsampled=%v", unsampled), func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			e, _ := spendingEngine(t)
			q := e.backends.Queue.(*queuememory.Queue)
			e.auxSpend = auxspend.NewLedger(q)
			swapModel(t, e, fullBudgetModel{})

			run := tracingtest.Remote()
			run.Unsampled = unsampled
			tracingtest.Watch(t, run.TraceID)

			fence := sandbox.Fence{Owner: "node-a:1", Epoch: 1}
			pending := sandbox.NewCoordStore(memory.NewFleet())
			provider, runner := sandbox.NewFakeProvider(), sandbox.NewFakeRunner("claude-code")
			manager, err := sandbox.NewManager(sandbox.ManagerOptions{
				Providers: map[sandbox.Placement]sandbox.Provider{sandbox.Direct: provider},
				Runners:   map[string]sandbox.Runner{"claude-code": runner},
			})
			if err != nil {
				t.Fatalf("NewManager: %v", err)
			}
			resumed := signalledResume{done: make(chan struct{})}
			coordinator, err := sandbox.NewCoordinator(sandbox.CoordinatorOptions{
				Queue: q, Pending: pending, Manager: manager, Resume: resumed,
				Audience: noAudience{}, Condense: runCondenser{engine: e},
				Lease: func(string) (sandbox.Fence, bool) { return fence, true },
			})
			if err != nil {
				t.Fatalf("NewCoordinator: %v", err)
			}
			t.Cleanup(coordinator.Stop)
			e.useSandbox(pending, coordinator)
			if err := e.prepareSeatSandbox(ctx, e.sandbox.Load(), "swe", fence.Epoch, fence.Owner); err != nil {
				t.Fatalf("prepareSeatSandbox: %v", err)
			}

			box, err := provider.Create(ctx, sandbox.Spec{})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			if _, err := pending.BeginLaunch(ctx, sandbox.PendingRun{
				TurnID: "t1", WorkKey: "wk-1", AgentHandle: "swe", AgentID: chargeAgent,
				Role: "SWE", CodingAgent: "claude-code",
				TraceID: run.TraceID, SpanID: run.SpanID, TraceUnsampled: unsampled,
			}, fence); err != nil {
				t.Fatalf("BeginLaunch: %v", err)
			}
			if err := pending.AttachSandbox(ctx, "t1", sandbox.BoxRef{
				SandboxID: box.ID(), CommandID: "cmd-1", CodingAgent: "claude-code",
			}, fence); err != nil {
				t.Fatalf("AttachSandbox: %v", err)
			}
			if ok, err := pending.MarkSuspended(ctx, "t1", sandbox.Suspension{State: json.RawMessage(
				`{"pending_tool_name":"run_sandbox"}`)}); err != nil || !ok {
				t.Fatalf("MarkSuspended = %v, %v", ok, err)
			}
			// Past the record's bound, so the report is condensed.
			report := strings.Repeat("the coding agent changed the parser and its tests\n",
				sandbox.MaxRunTextBytes/40)
			runner.Finish(sandbox.Result{Success: true, Text: report})

			row, _, err := pending.Get(ctx, "t1")
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			// Published as the waiter publishes it: beneath the span the
			// run was launched from, onto the seat's control topic.
			completed := types.SandboxRunCompleted{
				TurnID: "t1", LaunchID: row.LaunchID, AgentHandle: "swe", Agent: chargeAgent,
				RoleName: "SWE", SandboxID: box.ID(), CodingAgent: "claude-code",
			}
			if err := q.Publish(ctx, topics.AgentControl("swe"),
				events.New(completed, row.Trace().Beneath())); err != nil {
				t.Fatalf("Publish: %v", err)
			}
			select {
			case <-resumed.done:
			case <-time.After(10 * time.Second):
				t.Fatal("the completion was never collected and resumed")
			}
			e.auxSpend.FlushTurn(ctx, "t1")

			purpose := types.AuxCondense(string(runPartKind(sandbox.PartReport)))
			var records []*events.Event
			for _, ev := range q.History() {
				rec, ok := events.DataAs[*types.AuxiliarySpend](ev)
				if ok && rec.Purpose == purpose && rec.TurnID == "t1" {
					records = append(records, ev)
				}
			}
			if len(records) != 1 {
				traces := map[string]bool{}
				for _, ev := range records {
					traces[ev.TraceID] = true
				}
				t.Fatalf("the report's condensation was filed as %d records in %d traces — "+
					"want one, in the run's %s", len(records), len(traces), run.TraceID)
			}
			if got := records[0]; got.TraceID != run.TraceID || got.TraceUnsampled != unsampled {
				t.Errorf("the condensation is filed in %s unsampled=%v, want the run's %s "+
					"unsampled=%v", got.TraceID, got.TraceUnsampled, run.TraceID, unsampled)
			}
			if unsampled {
				// Nothing of an unsampled trace is recorded to look at.
				return
			}
			spans := tracingtest.Named(run.TraceID, auxiliarySpan)
			if len(spans) == 0 {
				t.Fatalf("no %s span in the run's trace: %v", auxiliarySpan, tracingtest.Names(run.TraceID))
			}
			for _, span := range spans {
				if got := span.Parent().SpanID().String(); got != run.SpanID {
					t.Errorf("%s's parent is %s, want the span the run was launched from, %s",
						auxiliarySpan, got, run.SpanID)
				}
			}
		})
	}
}
