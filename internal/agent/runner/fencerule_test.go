package runner_test

import (
	"context"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/provenance"
)

// A LAUNCHED RUN IS TOLD WHAT A FENCE IS, in the standing instructions its CLI
// is launched with, whether the run is the round's first or carries on one a
// person's answer reached: every foreign word it reads, in its brief or from
// its tools over the bridge, is inside a fence, and a fence means something
// only to a model told what (ADR-0040).
//
// Mutation: drop the untrusted-text section from the executor's prompt and
// both launches are red.
func TestALaunchedRunIsToldWhatAFenceIs(t *testing.T) {
	t.Parallel()
	entries := []phase.Entry{{Key: "default", Provider: &scriptedProvider{}}}
	launched, first := launch(t, entries, buildOpts{}, 1, "", nil)
	parked, ok := launched.Suspended()
	if !ok {
		t.Fatal("the launch recorded no state")
	}
	relauncher := &playLauncher{}
	collector, _ := buildWith(t, entries, buildOpts{
		calls: turnctx.NewCallLog(), launcher: relauncher,
		resume: &runner.Resume{State: parked.State, Asked: &runner.Asked{
			Question: "which branch?", Answer: "use main", BoxKept: true, SessionID: "sess-ask"}},
	})
	if _, _, err := collector.Resume(context.Background(), nil); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	for name, req := range map[string]runner.AgentRunRequest{
		"the round's first run": first, "a run carried on": relauncher.last(t),
	} {
		if !strings.Contains(req.System, provenance.StandingInstruction) {
			t.Errorf("%s is launched with no word of what a fence is:\n%s", name, req.System)
		}
	}
}
