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

// A NESTED PHASE'S IDENTITY HAS TO CARRY ITS ROUND. A phase is identified by
// (turn, phase, iteration) plus its task id, and task ids are unique only
// within one delegate call (`subagent.plan`), so without the round two rounds'
// same-named tasks share an identity and every consumer keying on it, the
// dashboard's phase merge included, keeps only the last.
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

	// The same task id in two execute rounds: legitimate, not a misuse.
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
// one: a worker and the round-cap judge publish no `agent_phase_started` to
// pair with, so without the worker's own measure no screen can say which
// worker of a fan-out was slow.
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

// A WORKER'S RECORD CARRIES ITS PROMPTS' OUTLINES, never a map that does not
// tile its text: every map a reader receives must be safe to slice by, so a
// wrong one is withheld and the reader falls back to the prompt's headings.
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
// execute phase's record outlives the run's log, which keeps a long run's head
// and tail only, so the dropped calls count in the round's calls and are said
// by tool, with how many failed, the calls of no kept tool last.
//
// Mutation: leave the dropped calls off the record, or out of the round's
// count, and a long run reads as the two hundred calls its log kept.
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

// A HELD CALL AND AN APPROVED ONE ARE RECORDED AS THE CALLS THEY ARE. Both
// carry `{}` because their arguments are withheld, and the held one is failed
// only because nothing it asked for happened, so the record marks each and a
// reader draws sealed arguments and a question, not a bare call or a broken
// tool.
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

// A CALL WHOSE ARGUMENTS ARE NOT KNOWN IS RECORDED AS ONE, with why: they
// were unparsable or do not read back (ArgsUnread), or the call was in flight
// with only its identity kept (Unfinished with no arguments). The row says
// which, so `{}` is not drawn as a call made with none. An in-flight call with
// arguments, a withheld one (marked its own way) and a call made with none say
// nothing of the kind.
//
// Mutation: build the execution without argsUnknown, write the row without it,
// tell the two reasons alike, or mark a withheld or an unfinished call that
// carries arguments, and this goes red.
func TestACallWhoseArgumentsAreNotKnownIsRecordedAsOne(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 11, 15, 9, 0, 0, 0, time.UTC)
	res := agentRunResult("done", []BridgedCall{
		{Call: ledger.Call{Name: "slack_post", Failed: true, ArgsUnread: true}, Seq: 1},
		{Call: ledger.Call{Name: "deploy", Failed: true, Unfinished: true, Digest: "d"}, Seq: 2},
		{Call: ledger.Call{Name: "deploy", Failed: true, Unfinished: true,
			Args: map[string]any{"env": "prod"}}, Seq: 3},
		{Call: ledger.Call{Name: "gh_delete_repo", Failed: true, Withheld: true, Unfinished: true}, Seq: 4,
			Held: true},
		{Call: ledger.Call{Name: "list_files"}, Seq: 5},
	}, nil, RunRecord{Harness: "claude-code"}, at, at.Add(time.Hour))
	rows := toolExecutions(res.Result.Executions)
	if len(rows) != 5 {
		t.Fatalf("%d rows, want the five calls", len(rows))
	}
	for i, want := range []any{"unread", "unkept", nil, nil, nil} {
		if got := rows[i]["arguments_unknown"]; got != want {
			t.Errorf("%s's row %d: arguments_unknown = %v, want %v", rows[i]["name"], i, got, want)
		}
	}
}
