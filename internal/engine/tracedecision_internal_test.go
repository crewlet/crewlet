package engine

import (
	"fmt"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/tracing"
)

// The trace every case here is launched or resumed under.
const decisionTrace, decisionSpan = "4bf92f3577b34da6a3ce929d0e0e4736", "00f067aa0ba902b7"

// A DETACHED RUN RECORDS THE SAMPLING DECISION OF THE SPAN THAT LAUNCHED IT,
// through the one builder both launch paths share — run_sandbox and a
// cli-agent's agent mode alike. The row is where the resume restores the
// trace from and the box's TRACEPARENT is rendered from, so a decision the
// builder left behind is one neither of them can have.
//
// Mutation: drop TraceUnsampled from sandboxTurnRef and the unsampled case
// goes red.
func TestALaunchedRunRecordsItsSpansSamplingDecision(t *testing.T) {
	t.Parallel()
	for _, unsampled := range []bool{false, true} {
		t.Run(fmt.Sprintf("unsampled=%v", unsampled), func(t *testing.T) {
			t.Parallel()
			ctx := tracing.WithRemote(t.Context(), events.TraceContext{
				TraceID: decisionTrace, SpanID: decisionSpan, Unsampled: unsampled,
			})
			ctx, span := tracing.Start(ctx, "engine", "agent.turn.execute")
			defer span.End()
			active := tracing.TraceOf(ctx)

			ref := sandboxTurnRef(ctx, &turnctx.Turn{RunID: "run-1"}, "SWE")
			want := events.TraceContext{TraceID: active.TraceID, SpanID: active.SpanID,
				Unsampled: unsampled}
			if got := ref.Trace(); got != want {
				t.Errorf("the run records the trace %+v, want the launching span's %+v", got, want)
			}
		})
	}
}

// A RESUMED TURN KEEPS ITS RUN'S SAMPLING DECISION, off the row, whatever the
// resuming node's own sampler would say.
//
// The resume reconstructs the suspended span from the row's ids and opens the
// resumed half beneath it; it used to reconstruct it SAMPLED whatever the row
// said, so the second half of a turn the operator's sampler had decided
// against was exported — days later, under a parent nobody exported — and every
// event it published said sampled. This process runs the default sampler,
// which samples every root, so the unsampled case passes only by honouring the
// row.
//
// Mutation: restore `{TraceID: in.Run.TraceID, SpanID: in.Run.SpanID}` by hand
// in resumeTurn and the unsampled case goes red.
func TestAResumedTurnKeepsItsRunsSamplingDecision(t *testing.T) {
	t.Parallel()
	for _, unsampled := range []bool{false, true} {
		t.Run(fmt.Sprintf("unsampled=%v", unsampled), func(t *testing.T) {
			t.Parallel()
			e, p := starting(t, refusingModels(t))
			company := e.Company()
			seat := company.Org.AgentSeatByHandle("swe")
			completion := events.New(types.SandboxRunCompleted{
				AgentHandle: "swe", RoleName: "SWE", TurnID: "run-1", SandboxID: "sbx-1",
			}, events.TraceContext{})

			// The segment fails at its model, which no case here reaches for
			// an answer: what is under test is the trace every event it
			// published on the way is filed under, its failure included.
			_ = e.resumeTurn(t.Context(), resumeInput{
				Company: company,
				Run: sandbox.PendingRun{
					TurnID: "run-1", WorkKey: "wk-1", AgentHandle: "swe", Reply: "tool",
					TaskDescription: "fix the failing test", ConversationKey: "conv-1",
					TraceID: decisionTrace, SpanID: decisionSpan, TraceUnsampled: unsampled,
				},
				Turn:    &turnctx.Turn{RunID: "run-1", WorkKey: "wk-1", Seat: seat, Org: company.Org},
				Answer:  "the tests pass now",
				Trigger: completion,
			})

			starts := startsOf(p.published())
			if len(starts) != 1 {
				t.Fatalf("published %d starts, want the segment's one — the premise", len(starts))
			}
			for _, ev := range p.published() {
				if ev.TraceID != decisionTrace {
					t.Errorf("%s left the run's trace for %q", ev.Type, ev.TraceID)
					continue
				}
				if ev.TraceUnsampled != unsampled {
					t.Errorf("%s carries unsampled=%v, want the run's %v",
						ev.Type, ev.TraceUnsampled, unsampled)
				}
			}
		})
	}
}
