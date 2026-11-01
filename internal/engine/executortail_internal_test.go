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
	"github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// failingRuns is a run store that can't be read, as a node's coordination
// store is during a blip.
type failingRuns struct{ err error }

func (f failingRuns) Get(context.Context, string) (sandbox.PendingRun, bool, error) {
	return sandbox.PendingRun{}, false, f.err
}

// THE ENGINE'S TAIL ANSWERER CLASSES A FAILURE OF ITS OWN BY THE ENGINE'S RULE.
//
// A node that owns a run answers a peer's tail request, and a failure it meets
// there is classed by the rule the engine hands it ([tailCondition], the one
// builtin.Condition reads). A coordination blip on the owner is a wait, which
// the asker answers 503 with a hint, and a fault of the owner's own is a 500.
// The sandbox package's own tests stand a rule of their own in for the
// engine's, so nothing held the wiring: a rule answering false would tell every
// blip on a peer-owned run as a fault, with nothing failing.
//
// Mutation: make tailCondition answer false, or wire ServeTail with a rule of
// its own in [serveExecutorTails], and the blip is answered as a fault.
func TestAPeersTailClassesItsOwnBlipAsAWait(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	runs := sandbox.NewCoordStore(coordmem.NewFleet())
	if _, err := runs.BeginLaunch(ctx, sandbox.PendingRun{
		TurnID: "t1", AgentHandle: "swe", Role: "SWE", CodingAgent: "claude-code",
		SandboxID: "box-1", Placement: string(sandbox.Direct), CommandID: "cmd-1",
	}, sandbox.Fence{}); err != nil {
		t.Fatal(err)
	}
	if ok, err := runs.MarkSuspended(ctx, "t1",
		sandbox.Suspension{State: json.RawMessage(`{"x":1}`)}); err != nil || !ok {
		t.Fatalf("mark suspended: %v %v", ok, err)
	}
	if ok, err := runs.ClaimOwnership(ctx, "t1", "n2:owner", 1); err != nil || !ok {
		t.Fatalf("claim ownership: %v %v", ok, err)
	}
	run, _, err := runs.Get(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}

	for name, c := range map[string]struct {
		read error
		wait bool
	}{
		"a coordination blip":        {fmt.Errorf("read: %w", coord.ErrUnavailable), true},
		"a fault of the owner's own": {errors.New("disk I/O error"), false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			// A BROKER OF ITS OWN: two owners serving one incarnation on one
			// broker would both answer the request.
			broker := memory.NewBroker()
			client := func() *memory.Queue {
				q := broker.Client()
				if err := q.Start(ctx); err != nil {
					t.Fatal(err)
				}
				return q
			}
			_, stop, err := serveExecutorTails(ctx, "n2:owner", "n2", failingRuns{err: c.read},
				nil, client())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = stop(context.WithoutCancel(ctx)) })
			asker := &sandbox.TailReader{Owner: "n1:asker", Pending: runs, Queue: client(),
				Budget: 30 * time.Second, Condition: tailCondition}
			_, err = asker.Tail(ctx, sandbox.TailQuery{TurnID: "t1", LaunchID: run.LaunchID})
			if err == nil {
				t.Fatal("the owner's failure was answered as output")
			}
			if waits := errors.Is(err, sandbox.ErrTailUnanswered); waits != c.wait {
				t.Errorf("the owner's failure is a wait: %v, want %v (%v)", waits, c.wait, err)
			}
			// A CONDITION, AND THE QUERY SURFACE'S 503 HINT, only for the wait:
			// a fault is no condition, which is its 500.
			if _, condition := builtin.Condition(err); condition != c.wait {
				t.Errorf("the owner's failure is a condition: %v, want %v", condition, c.wait)
			}
			if c.wait && builtin.RetryAfter(err, 5*time.Second) == 0 {
				t.Error("the owner's blip is answered with no wait")
			}
		})
	}
}
