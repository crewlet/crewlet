package builtin_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/sandboxtest"
	"github.com/crewlet/crewlet/internal/tools"
)

// A JOB PARKED ON ITS OWN CALL IS TOLD SO IF IT ASKS. A call answered still
// running before its harness's ceiling parks its job on it; a model that works
// on and calls `ask_and_wait` is refused, told the call it waits on and to end
// its turn, never that it already asked a question it never asked. A job that
// did ask a person first is still told it asked.
//
// Mutation: drop the parked-on-call lookup, and the parked job is told it asked
// a question; take it for any asked row (drop the own-calls check), and the
// job that asked a person is told of a call instead.
func TestAJobParkedOnItsOwnCallIsToldSoIfItAsks(t *testing.T) {
	t.Parallel()
	call := sandbox.CallWait{Tool: "delegate", Seq: 2}
	for name, c := range map[string]struct {
		first   sandbox.Ask
		want    []string
		without string
	}{
		"parked on its call": {first: sandbox.Ask{Call: &call},
			want:    []string{"call to `delegate` is still running", "END YOUR TURN NOW"},
			without: "already asked a question"},
		"asked a person": {first: sandboxtest.Question("Which branch?", sandbox.AskToRequester),
			want: []string{"already asked a question"}, without: "delegate"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			store := sandbox.NewCoordStore(memory.NewFleet())
			run, err := store.BeginLaunch(t.Context(), sandbox.PendingRun{TurnID: "run-1",
				AgentHandle: "swe", Status: sandbox.StatusLaunching}, sandbox.Fence{})
			if err != nil {
				t.Fatalf("BeginLaunch: %v", err)
			}
			if outcome, err := store.RecordAsk(t.Context(), "run-1", run.LaunchID, c.first); err != nil ||
				outcome != sandbox.AskRecorded {
				t.Fatalf("the first ask: %v, %v", outcome, err)
			}
			ask := builtin.NewAskAndWait(builtin.RunAskDeps{Desk: store})
			got, err := ask.CallForTurn(turnctx.WithJob(t.Context(), run.LaunchID), &turnctx.Turn{RunID: "run-1"},
				branchAsk())
			if err != nil || !got.Failed || tools.RefusalOf(got) != tools.RefusalNotRunning {
				t.Fatalf("ask_and_wait answered %+v, %v; want a not-running refusal", got, err)
			}
			for _, want := range c.want {
				if !strings.Contains(got.Output, want) {
					t.Errorf("the refusal does not say %q: %s", want, got.Output)
				}
			}
			if strings.Contains(got.Output, c.without) {
				t.Errorf("the refusal says %q: %s", c.without, got.Output)
			}
		})
	}
}
