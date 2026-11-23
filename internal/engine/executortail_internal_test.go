package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/coord"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// failingRuns is a run store that can't be read, as a node's coordination
// store is during a blip.
type failingRuns struct{ err error }

func (f failingRuns) Get(context.Context, string) (sandbox.PendingRun, bool, error) {
	return sandbox.PendingRun{}, false, f.err
}

// THE ENGINE'S TAIL ANSWERER CLASSES A FAILURE OF ITS OWN BY THE ENGINE'S RULE
// ([tailCondition]): a coordination blip on the node hosting a run's box is a
// wait, which the asker answers 503 with a hint, and the host's own fault a 500. The sandbox
// package's tests stand in a rule of their own, so only this test holds the
// wiring.
//
// Mutation: make tailCondition answer false, or wire ServeTail with a rule of
// its own in [serveExecutorTails], and the blip is answered as a fault.
func TestAPeersTailClassesItsOwnBlipAsAWait(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	runs := sandbox.NewCoordStore(coordmem.NewFleet())
	if _, err := runs.BeginLaunch(ctx, sandbox.PendingRun{
		TurnID: "t1", AgentHandle: "swe", Role: "SWE", Harness: "claude-code",
		SandboxID: "box-1", Placement: string(sandbox.Direct), CommandID: "cmd-1", LaunchNode: "n2",
	}, sandbox.Fence{}); err != nil {
		t.Fatal(err)
	}
	if ok, err := runs.MarkSuspended(ctx, "t1",
		sandbox.Suspension{State: json.RawMessage(`{"x":1}`)}); err != nil || !ok {
		t.Fatalf("mark suspended: %v %v", ok, err)
	}
	run, _, err := runs.Get(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}

	for name, c := range map[string]struct {
		read error
		wait bool
	}{
		"a coordination blip":       {fmt.Errorf("read: %w", coord.ErrUnavailable), true},
		"a fault of the host's own": {errors.New("disk I/O error"), false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			// A BROKER OF ITS OWN: two hosts serving one node id on one
			// broker would both answer the request.
			broker := memory.NewBroker()
			client := func() *memory.Queue {
				q := broker.Client()
				if err := q.Start(ctx); err != nil {
					t.Fatal(err)
				}
				return q
			}
			_, stop, err := serveExecutorTails(ctx, "n2", failingRuns{err: c.read},
				nil, client(), queue.NewAnswerBudget(nil))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = stop(context.WithoutCancel(ctx)) })
			asker := &sandbox.TailReader{Node: "n1", Pending: runs, Queue: client(),
				Budget: 30 * time.Second, Condition: tailCondition}
			_, err = asker.Tail(ctx, sandbox.TailQuery{TurnID: "t1", LaunchID: run.LaunchID})
			if err == nil {
				t.Fatal("the host's failure was answered as output")
			}
			if waits := errors.Is(err, sandbox.ErrTailUnanswered); waits != c.wait {
				t.Errorf("the host's failure is a wait: %v, want %v (%v)", waits, c.wait, err)
			}
			// A CONDITION, AND THE QUERY SURFACE'S 503 HINT, only for the wait:
			// a fault is no condition, which is its 500.
			if _, condition := builtin.Condition(err); condition != c.wait {
				t.Errorf("the host's failure is a condition: %v, want %v", condition, c.wait)
			}
			if c.wait && builtin.RetryAfter(err, 5*time.Second) == 0 {
				t.Error("the host's blip is answered with no wait")
			}
		})
	}
}
