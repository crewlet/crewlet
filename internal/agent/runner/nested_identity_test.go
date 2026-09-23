package runner

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/subagent"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
)

// collector keeps what was published, in order.
type collector struct {
	mu     sync.Mutex
	events []*events.Event
}

func (c *collector) Publish(_ context.Context, _ string, ev *events.Event) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, ev)
	return nil
}

// A NESTED PHASE'S IDENTITY HAS TO CARRY ITS ROUND.
//
// A phase is identified by (turn, phase, iteration), plus the task id that
// tells one worker of a fan-out from the next. Task ids are unique only WITHIN
// one delegate call — `subagent.plan` refuses a repeat there, and nothing
// constrains the next round — so with iteration left at zero a self-iterating
// turn that delegated a task named the same thing twice published two records
// under one identity. Every consumer that keys on it, the dashboard's phase
// merge included, kept whichever arrived last: a worker, its prompt, its tools
// and its failure were simply not there.
func TestASubagentPhaseCarriesTheRoundItRanIn(t *testing.T) {
	t.Parallel()
	pub := &collector{}
	var mu sync.Mutex
	base := emitter{
		pub:   pub,
		turn:  Turn{RunID: "tn-1", AgentID: "agent-1"},
		role:  "Lead",
		tally: &Spend{},
		mu:    &mu,
	}

	// The same task id, delegated in two different execute rounds — which is
	// a model naming its work the same thing twice, not a misuse.
	for _, round := range []int{1, 2} {
		base.nestedAt(round).subagentCompleted(context.Background(), subagent.Result{
			ID: "research", Worker: "researcher", Status: subagent.StatusOK,
		})
	}

	var iterations []int
	for _, ev := range pub.events {
		done, ok := ev.Data.(*types.AgentPhaseCompleted)
		if !ok || done.Phase != types.PhaseSubagent {
			continue
		}
		if done.TaskID != "research" {
			t.Errorf("task id = %q, want the id the parent wrote", done.TaskID)
		}
		if done.HostIteration != done.Iteration {
			t.Errorf("iteration %d and host iteration %d disagree about the round",
				done.Iteration, done.HostIteration)
		}
		iterations = append(iterations, done.Iteration)
	}
	if len(iterations) != 2 {
		t.Fatalf("published %d subagent phases, want 2", len(iterations))
	}
	if iterations[0] == iterations[1] {
		t.Errorf("both rounds published iteration %d, so one worker's record "+
			"overwrites the other wherever a phase is keyed by identity",
			iterations[0])
	}
}

// A NESTED PHASE PUBLISHES ITS OWN DURATION, because nothing else can give it
// one.
//
// The turn's own phases pair with an `agent_phase_started`; a worker and the
// round-cap judge publish no start at all — they nest under a host phase that
// is already showing one. So for as long as a duration was reconstructed from
// two events, a delegate fan-out of eight had eight records with no wall clock
// anywhere between them, and "which worker was slow" was unanswerable on every
// screen in the product. The worker measures itself and this carries it.
func TestASubagentPhaseCarriesTheWorkersOwnWallClock(t *testing.T) {
	t.Parallel()
	pub := &collector{}
	var mu sync.Mutex
	base := emitter{
		pub:   pub,
		turn:  Turn{RunID: "tn-1", AgentID: "agent-1"},
		role:  "Lead",
		tally: &Spend{},
		mu:    &mu,
	}

	base.nestedAt(1).subagentCompleted(context.Background(), subagent.Result{
		ID: "research", Worker: "researcher", Status: subagent.StatusOK,
		Elapsed: 1500 * time.Millisecond,
	})

	var found bool
	for _, ev := range pub.events {
		done, ok := ev.Data.(*types.AgentPhaseCompleted)
		if !ok || done.Phase != types.PhaseSubagent {
			continue
		}
		found = true
		if done.DurationMS != 1500 {
			t.Errorf("duration_ms = %d, want the 1500 the worker measured",
				done.DurationMS)
		}
	}
	if !found {
		t.Fatal("no subagent phase was published")
	}
}

// A WORKER CUT OFF BY ITS OUTPUT CAP SAYS SO ON ITS OWN CARD.
//
// A length stop is a successful call with a short body, so a worker whose
// answer stopped mid-sentence and one that finished are the same record unless
// the flag rides it. The parent's model is told beside the answer; the phase
// record is what an operator reads, and it has to carry the same fact.
func TestASubagentPhaseSaysItsOutputWasCut(t *testing.T) {
	t.Parallel()
	for _, truncated := range []bool{true, false} {
		pub := &collector{}
		var mu sync.Mutex
		base := emitter{
			pub: pub, turn: Turn{RunID: "tn-1", AgentID: "agent-1"},
			role: "Lead", tally: &Spend{}, mu: &mu,
		}
		base.nestedAt(1).subagentCompleted(context.Background(), subagent.Result{
			ID: "research", Worker: "researcher", Status: subagent.StatusOK, Truncated: truncated,
		})
		var found bool
		for _, ev := range pub.events {
			done, ok := ev.Data.(*types.AgentPhaseCompleted)
			if !ok || done.Phase != types.PhaseSubagent {
				continue
			}
			found = true
			if done.OutputTruncated != truncated {
				t.Errorf("output_truncated = %v for a worker whose result said %v",
					done.OutputTruncated, truncated)
			}
		}
		if !found {
			t.Fatal("no subagent phase was published")
		}
	}
}
