package runner

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/agent/prompts"
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

// A WORKER'S RECORD CARRIES ITS PROMPTS' OUTLINES — and never a map that does
// not tile its text: the wire promise is that every map a reader receives can
// be sliced by, so one this node already knows is wrong is withheld and the
// reader falls back to the prompt's own headings.
func TestAWorkerRecordCarriesItsPromptOutlinesOnlyWhenTheyTile(t *testing.T) {
	t.Parallel()
	pub := &collector{}
	var mu sync.Mutex
	base := emitter{
		pub: pub, turn: Turn{RunID: "tn-1", AgentID: "agent-1"},
		role: "Lead", tally: &Spend{}, mu: &mu,
	}
	system := prompts.BuildSubagent(prompts.Seat{}, prompts.SubagentInput{
		ParentSystemPrompt: "You research.\n\n## Steps\n1. read — all of it",
		Submits:            true,
	})
	broken := prompts.Prompt{Text: "gather it", Sections: []prompts.Section{
		{Key: "task", Title: "Task", Bytes: 4},
	}}
	base.nestedAt(1).subagentCompleted(context.Background(), subagent.Result{
		ID: "research", Status: subagent.StatusOK, SystemPrompt: system, UserPrompt: broken,
	})
	for _, ev := range pub.events {
		done, ok := ev.Data.(*types.AgentPhaseCompleted)
		if !ok || done.Phase != types.PhaseSubagent {
			continue
		}
		if done.SystemPrompt != system.Text || len(done.SystemSections) != len(system.Sections) {
			t.Errorf("system_sections = %+v, want the builder's %+v", done.SystemSections, system.Sections)
		}
		if done.UserPrompt != "gather it" || done.UserSections != nil {
			t.Errorf("a map that does not tile its text was published: %+v", done.UserSections)
		}
		return
	}
	t.Fatal("no subagent phase was published")
}

// A COLLECTED RUN'S RECORD SAYS WHAT ITS RUN'S RECORD DID NOT KEEP. The
// execute phase's record lists the calls the run's row kept — a long run's
// head and tail — and outlives the row: once the run is gone, that record is
// the turn's account of what its executor did. So the dropped calls are counted
// in its round's calls and said by tool, how many and how many failed, the
// calls whose tool was not kept either last.
//
// Mutation: leave the dropped calls off the record, or out of the round's
// count, and a long run reads as the two hundred calls its row kept.
func TestACollectedRunsRecordSaysWhatItsRunsRecordDidNotKeep(t *testing.T) {
	t.Parallel()
	pub := &collector{}
	var mu sync.Mutex
	e := emitter{pub: pub, turn: Turn{RunID: "tn-1", AgentID: "agent-1"}, role: "Dev", tally: &Spend{}, mu: &mu}
	at := time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC)
	dropped := []ledger.Elided{
		{Name: "read_file", Digest: "a", Calls: 40},
		{Name: "read_file", Digest: "b", Calls: 19, Failed: 2},
		{Name: "comment_on_work_item", Digest: "c", Calls: 1},
		{Calls: 3},
	}
	res := agentRunResult("done", []BridgedCall{{Call: ledger.Call{Name: "read_file"}, Seq: 1}}, dropped,
		RunRecord{Harness: "claude-code", SandboxID: "box-1"}, at, at.Add(time.Hour))
	e.completed(context.Background(), phaseRecord{Phase: "execute", Iteration: 1, Result: res.Result,
		Run: RunRecord{Harness: "claude-code", SandboxID: "box-1"}, RunElided: res.RunElided})
	if len(pub.events) != 1 {
		t.Fatalf("published %d records, want the execute phase's", len(pub.events))
	}
	done, ok := pub.events[0].Data.(*types.AgentPhaseCompleted)
	if !ok {
		t.Fatalf("published %T", pub.events[0].Data)
	}
	want := []types.ElidedCalls{{Name: "comment_on_work_item", Calls: 1}, {Name: "read_file", Calls: 59, Failed: 2},
		{Calls: 3}}
	if done.CallsElided != 63 || !slices.Equal(done.ElidedCalls, want) {
		t.Errorf("the record says %d calls were dropped, %+v; want 63, %+v", done.CallsElided,
			done.ElidedCalls, want)
	}
	if len(done.Rounds) != 1 || done.Rounds[0].ToolCalls != 64 {
		t.Errorf("the round counts %+v calls, want the one kept and the 63 dropped", done.Rounds)
	}
}

// A HELD CALL AND AN APPROVED ONE ARE RECORDED AS THE CALLS THEY ARE. The two
// calls whose arguments every record withholds — a call held for a person's
// approval and the one they let be made — carry `{}` as their arguments on the
// execute phase's record only because none is kept in the clear, and the held
// one is recorded failed only because nothing it asked for happened: so the
// record says which each is, and a reader draws its arguments as sealed and
// the held call as the run's question, never a call made with nothing or a
// tool that broke.
//
// Mutation: build the execution without the marks, or write the row without
// them, and the turn's execute card draws `{}` for both and the held call as
// failed.
func TestAHeldCallAndAnApprovedOneAreRecordedAsTheCallsTheyAre(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC)
	res := agentRunResult("done", []BridgedCall{
		{Call: ledger.Call{Name: "gh_delete_repo", Failed: true, Withheld: true}, Seq: 1, Held: true},
		{Call: ledger.Call{Name: "gh_delete_repo", Withheld: true}, Seq: 2, Approved: true},
		{Call: ledger.Call{Name: "read_file"}, Seq: 3},
	}, nil, RunRecord{Harness: "claude-code"}, at, at.Add(time.Hour))
	rows := toolExecutions(res.Result.Executions)
	if len(rows) != 3 {
		t.Fatalf("%d rows, want the three calls", len(rows))
	}
	if rows[0]["held"] != true || rows[0]["approved"] != nil || rows[1]["approved"] != true ||
		rows[1]["held"] != nil || rows[2]["held"] != nil || rows[2]["approved"] != nil {
		t.Errorf("the rows mark %v, %v and %v; want the held call held, the approved one approved, the "+
			"third neither", rows[0], rows[1], rows[2])
	}
}
