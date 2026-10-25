package runner_test

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/prompts"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/agent/steer"
	"github.com/crewlet/crewlet/internal/agent/subagent"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/llmtest"
	"github.com/crewlet/crewlet/internal/tools"
)

// offeringTool is a builtin that, when the executor calls it, has a person
// offer a note to the running turn — the moment a person steers is exactly
// while a turn is busy.
type offeringTool struct {
	box  *steer.Box
	note steer.Note
}

func (o offeringTool) Name() string               { return "peek" }
func (o offeringTool) Description() string        { return "peek at something" }
func (o offeringTool) Parameters() map[string]any { return nil }
func (o offeringTool) Call(context.Context, map[string]any) (tools.Result, error) {
	o.box.Offer(o.note)
	return tools.Result{Output: "peeked"}, nil
}

// founderNote is a note a bound person sent through their session: By is the
// seat they act as, as iam.ActorFor names them, and OperatorID the credential.
var founderNote = steer.Note{ID: "req-1", Text: "post to #staging, not #general",
	By: "founder", ByKind: iam.ActorHuman, OperatorID: "session:founder-1"}

// holds reports whether a request's conversation carries the note as the one
// user message the model reads it as.
func holds(req llm.Request) bool {
	want := prompts.SteerMessage(founderNote.By, founderNote.Text)
	return slices.ContainsFunc(req.Messages, func(m llm.Message) bool {
		return m.Role == llm.RoleUser && m.Content == want
	})
}

// steered is every agent_turn_steered a turn published.
func (c *capture) steered() []*types.AgentTurnSteered {
	c.mu <- struct{}{}
	defer func() { <-c.mu }()
	var out []*types.AgentTurnSteered
	for _, ev := range c.events {
		if got, ok := events.DataAs[*types.AgentTurnSteered](ev); ok {
			out = append(out, got)
		}
	}
	return out
}

// THE REVIEWER SEES A NOTE THE EXECUTOR WAS STEERED WITH.
//
// The note is read by the executor at its next round, and the reviewer — a
// fresh conversation judging the work — opens with it too. Without that the
// reviewer grades the work against the task the person corrected: an executor
// that followed "post to #staging" would be sent back for not posting to the
// channel the original ask named.
func TestTheReviewerSeesANoteTheExecutorWasSteeredWith(t *testing.T) {
	t.Parallel()
	box := steer.New()
	pub := newCapture()
	prov := &scriptedProvider{
		execute: []llmtest.Response{
			calling(llmtest.Call{Name: "peek"}),
			submitWork(t),
		},
		review: []llmtest.Response{submitCall(t, runner.SubmitReviewTool,
			`{"decision":"done","final_artifact":"a"}`)},
	}
	r, _ := buildWith(t, []phase.Entry{{Key: "default", Provider: prov}}, buildOpts{
		steer: box, pub: pub,
		register: func(t *testing.T, reg *tools.Registry) {
			if err := reg.Register(offeringTool{box: box, note: founderNote}, tools.OriginBuiltin); err != nil {
				t.Fatalf("Register: %v", err)
			}
		},
	})
	work, _, err := r.Execute(t.Context(), 1, "", nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if _, err := r.Review(t.Context(), 1, work, nil); err != nil {
		t.Fatalf("Review: %v", err)
	}

	exec := prov.requestsFor("execute")
	if len(exec) != 2 || holds(exec[0]) || !holds(exec[1]) {
		t.Fatalf("the executor did not read the note at its next round (%d rounds)", len(exec))
	}
	review := prov.requestsFor("review")
	if len(review) == 0 || !holds(review[0]) {
		t.Fatal("the reviewer judged the work without the note the executor was steered with")
	}
	// Recorded ONCE, as delivered, at the executor round that read it —
	// carrying the note into the reviewer is not a second delivery.
	got := pub.steered()
	if len(got) != 1 {
		t.Fatalf("published %d agent_turn_steered, want 1: %+v", len(got), got)
	}
	if got[0].Outcome != types.SteerDelivered || got[0].Phase != types.Phase(phase.Execute) ||
		got[0].Round != 2 || got[0].NoteID != "req-1" || got[0].SteeredBy != "founder" ||
		got[0].SteeredByKind != string(iam.ActorHuman) ||
		got[0].OperatorID != "session:founder-1" ||
		got[0].Note != founderNote.Text || got[0].TurnID != "t-1" || got[0].AgentHandle != "cto" {
		t.Errorf("the delivery record is %+v", got[0])
	}
}

// A NOTE READ BY THE REVIEWER BINDS THE NEXT EXECUTOR ITERATION. A turn sent
// back for another pass opens a fresh executor conversation, and the note is
// carried into it rather than lost with the conversation it was read in.
func TestANoteBindsTheTurnsLaterIterations(t *testing.T) {
	t.Parallel()
	box := steer.New()
	prov := &scriptedProvider{
		execute: []llmtest.Response{submitWork(t)},
		review: []llmtest.Response{submitCall(t, runner.SubmitReviewTool,
			`{"decision":"self_iterate","notes":"try again"}`)},
	}
	r, _ := buildWith(t, []phase.Entry{{Key: "default", Provider: prov}}, buildOpts{steer: box})
	work, _, err := r.Execute(t.Context(), 1, "", nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	// Offered between the phases: the reviewer's first round reads it.
	box.Offer(founderNote)
	if _, err := r.Review(t.Context(), 1, work, nil); err != nil {
		t.Fatalf("Review: %v", err)
	}
	if _, _, err := r.Execute(t.Context(), 2, "try again", nil); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	exec := prov.requestsFor("execute")
	if holds(exec[0]) {
		t.Fatal("the first iteration read a note offered after it finished")
	}
	if !holds(exec[len(exec)-1]) {
		t.Error("the second executor iteration lost the note the reviewer read")
	}
}

// workerProvider tells the executor's runs from a worker's by the submission
// each surface offers, and has a person steer the turn while the worker is
// running.
type workerProvider struct {
	t   *testing.T
	box *steer.Box

	mu sync.Mutex
	// runs are each kind's CLI runs, and responses how many model requests
	// each kind was asked for across them.
	runs      map[string][]*llmtest.Conn
	requests  map[string][]llm.Request
	responses map[string]int
}

func (p *workerProvider) Model() string { return "scripted" }

func (p *workerProvider) Complete(ctx context.Context, req llm.Request) (*llm.Completion, error) {
	kind := ""
	return llmtest.Drive(ctx, req, llmtest.ConnOptions{}, func(c *llmtest.Conn) (llmtest.Response, error) {
		p.mu.Lock()
		defer p.mu.Unlock()
		if kind == "" {
			switch offered := c.OfferedNames(); {
			case slices.Contains(offered, subagent.SubmitTool):
				kind = "worker"
			case slices.Contains(offered, runner.SubmitReviewTool):
				kind = "review"
			default:
				kind = "execute"
			}
			p.runs[kind] = append(p.runs[kind], c)
			p.requests[kind] = append(p.requests[kind], req)
		}
		p.responses[kind]++
		switch kind {
		case "worker":
			if p.responses[kind] == 1 {
				// The person steers the turn while its worker runs, and
				// the worker's first submission is malformed — so the
				// worker has a SECOND round, the one that would read the
				// note if a worker were steered.
				p.box.Offer(founderNote)
				return calling(llmtest.Call{Name: subagent.SubmitTool, Args: `{}`}), nil
			}
			return calling(llmtest.Call{Name: subagent.SubmitTool, Args: `{"result":"found it"}`}), nil
		case "review":
			return submitCall(p.t, runner.SubmitReviewTool, `{"decision":"done","final_artifact":"a"}`), nil
		default:
			if p.responses[kind] == 1 {
				return calling(callOf(p.t, subagent.ToolName, map[string]any{"tasks": []any{
					map[string]any{"id": "look", "prompt": "look into it",
						"system_prompt": "You look into things."},
				}})), nil
			}
			return submitWork(p.t), nil
		}
	})
}

// SUBAGENTS ARE NOT STEERED. A worker is a leaf its parent directs, and a note
// meant for the turn is the turn's to read: offered while a worker runs, it
// waits in the box through every round the worker takes and reaches the
// executor's next round instead.
func TestSubagentsAreNotSteered(t *testing.T) {
	t.Parallel()
	box := steer.New()
	prov := &workerProvider{t: t, box: box, runs: map[string][]*llmtest.Conn{},
		requests: map[string][]llm.Request{}, responses: map[string]int{}}
	r, _ := buildWith(t, []phase.Entry{{Key: "default", Provider: prov}}, buildOpts{
		steer: box, subagent: &runner.SubagentConfig{Limits: shipped()},
	})
	if _, _, err := r.Execute(t.Context(), 1, "", nil); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	prov.mu.Lock()
	defer prov.mu.Unlock()
	if n := prov.responses["worker"]; n < 2 {
		t.Fatalf("the worker ran %d rounds; this case needs a round after the offer", n)
	}
	// NOT PAUSED FOR IT: a steerable loop ends its CLI's run once a note is
	// waiting, and the next run reads it — so a worker steered by mistake
	// would show a second run that opens on the note. One run, its rounds
	// one CLI loop, is a worker the note never reached.
	workers := prov.requests["worker"]
	if len(workers) != 1 {
		t.Errorf("the worker took %d CLI runs, want one the note never ended", len(workers))
	}
	for i, req := range workers {
		if holds(req) {
			t.Fatalf("worker run %d read the turn's note", i+1)
		}
		if strings.Contains(req.Messages[0].Content, founderNote.Text) {
			t.Error("the note reached a worker's prompt")
		}
	}
	// And never inside a tool's result, where any tool's output could
	// imitate one.
	for _, conn := range prov.runs["worker"] {
		for _, res := range conn.Results() {
			if strings.Contains(res.Output, founderNote.Text) {
				t.Errorf("the worker read the note in a tool's result: %q", res.Output)
			}
		}
	}
	// THE DELEGATE CALL ENDED THE EXECUTOR'S RUN and the next run read the
	// note: a call that waits on other model runs is parked locally, and its
	// result is handed to the run after it.
	exec := prov.requests["execute"]
	if len(exec) < 2 || !holds(exec[1]) {
		t.Error("the executor's run after the delegate call did not read the note")
	}
	if len(exec) >= 2 && !slices.ContainsFunc(exec[1].Messages, func(m llm.Message) bool {
		return m.Role == llm.RoleTool && m.Name == subagent.ToolName
	}) {
		t.Error("the executor's run after the delegate call was not handed its result")
	}
}

// A RESUMED EXECUTOR READS A NOTE AT ITS ROUND BOUNDARY, as the pass it
// continues would have. The engine hands a resumed segment the turn's note box
// ("a resumed segment is steerable like any other"), and a phase that drained
// nothing left the note in the box: the executor finished the parked work
// without the correction and only the reviewer read it.
//
// The round the delivery is announced at is on the PHASE's scale — the
// re-entry's first round is the phase's third, after the two it ran before it
// parked — so the record sits beside the round that read it.
func TestAResumedExecutorReadsANoteAtItsRoundBoundary(t *testing.T) {
	t.Parallel()
	box := steer.New()
	pub := newCapture()
	prov := &scriptedProvider{execute: []llmtest.Response{submitWork(t)}}
	r, _ := buildWith(t, []phase.Entry{{Key: "default", Provider: prov}}, buildOpts{
		steer: box, pub: pub,
		resume: &runner.Resume{State: suspendedAfterTwoRounds(), Answer: "the run succeeded"},
	})
	// Sent while the turn's coding run was collected, before the re-entered
	// loop's first round.
	box.Offer(founderNote)
	if _, _, err := r.Resume(t.Context(), nil); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	exec := prov.requestsFor("execute")
	if len(exec) == 0 || !holds(exec[0]) {
		t.Fatal("the resumed executor did not read the note at its first round")
	}
	got := pub.steered()
	if len(got) != 1 {
		t.Fatalf("published %d agent_turn_steered, want 1: %+v", len(got), got)
	}
	if got[0].Outcome != types.SteerDelivered || got[0].Phase != types.Phase(phase.Execute) ||
		got[0].Round != 3 {
		t.Errorf("the delivery record is %+v, want delivered at execute round 3", got[0])
	}
}
