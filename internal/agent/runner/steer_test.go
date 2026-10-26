package runner_test

import (
	"context"
	"fmt"
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
)

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

// THE REVIEWER READS A NOTE SENT WHILE THE RUN WORKED.
//
// A person who corrects a turn while its run is out is correcting the work the
// reviewer is about to judge. The segment that collects the run drains the
// turn's box at the reviewer's first round, so the reviewer judges the work
// against the task as corrected — and the read is recorded once, as delivered
// to the phase that read it.
func TestTheReviewerReadsANoteSentWhileTheRunWorked(t *testing.T) {
	t.Parallel()
	box := steer.New()
	pub := newCapture()
	prov := &scriptedProvider{review: []llmtest.Response{submitCall(t, runner.SubmitReviewTool,
		`{"decision":"done","final_artifact":"a"}`)}}
	entries := []phase.Entry{{Key: "default", Provider: prov}}
	opts := buildOpts{steer: box, pub: pub}
	launched, req := launch(t, entries, opts, 1, "", nil)
	bridged := play(t, req, deliver(t, "posted"))
	// Sent while the run was out.
	box.Offer(founderNote)

	collector, work, _ := collect(t, entries, opts, launched, nil, bridged, "posted", runner.RunRecord{})
	if _, err := collector.Review(t.Context(), 1, work, nil); err != nil {
		t.Fatalf("Review: %v", err)
	}
	review := prov.requestsFor("review")
	if len(review) == 0 || !holds(review[0]) {
		t.Fatal("the reviewer judged the work without the note sent while it was done")
	}
	got := pub.steered()
	if len(got) != 1 {
		t.Fatalf("published %d agent_turn_steered, want 1: %+v", len(got), got)
	}
	if got[0].Outcome != types.SteerDelivered || got[0].Phase != types.Phase(phase.Review) ||
		got[0].Round != 1 || got[0].NoteID != "req-1" || got[0].SteeredBy != "founder" ||
		got[0].SteeredByKind != string(iam.ActorHuman) ||
		got[0].OperatorID != "session:founder-1" ||
		got[0].Note != founderNote.Text || got[0].TurnID != "t-1" || got[0].AgentHandle != "cto" {
		t.Errorf("the delivery record is %+v", got[0])
	}
}

// A NOTE BINDS THE TURN'S LATER ITERATIONS, across every park between them.
// The reviewer that read it sends the work back; the next run is launched
// with the note in its brief, since a run is a fresh session with nowhere
// else to read it; and the reviewer of THAT run — a fresh runner in whichever
// process collects it — opens with the note too, carried on the park rather
// than read again.
//
// Mutations: leave the carried notes out of the brief, or off the park, and
// one half goes red.
func TestANoteBindsTheTurnsLaterIterations(t *testing.T) {
	t.Parallel()
	box := steer.New()
	pub := newCapture()
	prov := &scriptedProvider{review: []llmtest.Response{submitCall(t, runner.SubmitReviewTool,
		`{"decision":"self_iterate","notes":"try again"}`)}}
	entries := []phase.Entry{{Key: "default", Provider: prov}}
	opts := buildOpts{steer: box, pub: pub}

	note := prompts.SteerMessage(founderNote.By, founderNote.Text)

	launched, req := launch(t, entries, opts, 1, "", nil)
	bridged := play(t, req, deliver(t, "posted"))
	// The collecting runner's own launcher: it is the one that launches the
	// turn's next run.
	relauncher := &playLauncher{}
	collecting := opts
	collecting.launcher = relauncher
	collector, work, _ := collect(t, entries, collecting, launched, nil, bridged, "posted", runner.RunRecord{})
	box.Offer(founderNote)
	if _, err := collector.Review(t.Context(), 1, work, nil); err != nil {
		t.Fatalf("Review: %v", err)
	}
	if strings.Contains(req.Brief, note) {
		t.Fatal("the first run was launched with a note offered after it finished")
	}

	// The turn sent back: the collecting runner launches the next run.
	if _, _, err := collector.Execute(t.Context(), 2, "try again", nil); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	parked, ok := collector.Suspended()
	if !ok {
		t.Fatal("the second launch recorded no park")
	}
	relaunched := relauncher.last(t)
	if !strings.Contains(relaunched.Brief, note) {
		t.Fatalf("the second run's brief lost the note the reviewer read:\n%s", relaunched.Brief)
	}

	// Collected by a fresh runner, whose reviewer is a fresh conversation.
	again := &scriptedProvider{review: []llmtest.Response{submitCall(t, runner.SubmitReviewTool,
		`{"decision":"done","final_artifact":"a"}`)}}
	final, _ := buildWith(t, []phase.Entry{{Key: "default", Provider: again}}, buildOpts{
		steer: steer.New(), pub: pub,
		resume: &runner.Resume{State: parked.State, Answer: "posted",
			Bridged: play(t, relaunched, deliver(t, "posted to staging"))},
	})
	work, _, err := final.Resume(t.Context(), nil)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if _, err := final.Review(t.Context(), 2, work, nil); err != nil {
		t.Fatalf("Review: %v", err)
	}
	if review := again.requestsFor("review"); len(review) == 0 || !holds(review[0]) {
		t.Error("the second reviewer judged the work without the note the turn had read")
	}
	// CARRIED, not read again: the one delivery is the first reviewer's.
	if got := pub.steered(); len(got) != 1 {
		t.Errorf("published %d agent_turn_steered for one note, want 1: %+v", len(got), got)
	}
}

// steeringWorker answers a delegated worker and the reviewer, and has a person
// steer the turn while the worker is running.
type steeringWorker struct {
	t   *testing.T
	box *steer.Box

	mu sync.Mutex
	// runs are each kind's CLI runs, requests what each run was opened on,
	// and responses how many model requests each kind was asked for across
	// them.
	runs      map[string][]*llmtest.Conn
	requests  map[string][]llm.Request
	responses map[string]int
}

func (p *steeringWorker) Model() string { return "scripted" }

func (p *steeringWorker) Complete(ctx context.Context, req llm.Request) (*llm.Completion, error) {
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
				return llmtest.Response{}, fmt.Errorf(
					"steeringWorker: asked by a phase it does not play (tools %v)", offered)
			}
			p.runs[kind] = append(p.runs[kind], c)
			p.requests[kind] = append(p.requests[kind], req)
		}
		p.responses[kind]++
		if kind == "review" {
			return submitCall(p.t, runner.SubmitReviewTool, `{"decision":"done","final_artifact":"a"}`), nil
		}
		if p.responses[kind] == 1 {
			// The person steers the turn while its worker runs, and the
			// worker's first submission is malformed — so the worker has
			// a SECOND round, the one that would read the note if a
			// worker were steered.
			p.box.Offer(founderNote)
			return calling(llmtest.Call{Name: subagent.SubmitTool, Args: `{}`}), nil
		}
		return calling(llmtest.Call{Name: subagent.SubmitTool, Args: `{"result":"found it"}`}), nil
	})
}

// SUBAGENTS ARE NOT STEERED. A worker is a leaf its parent directs, and a note
// meant for the turn is the turn's to read: offered while a worker the run
// delegated to is running, it waits in the box through every round the worker
// takes and reaches the reviewer that judges the run instead.
func TestSubagentsAreNotSteered(t *testing.T) {
	t.Parallel()
	box := steer.New()
	prov := &steeringWorker{t: t, box: box, runs: map[string][]*llmtest.Conn{},
		requests: map[string][]llm.Request{}, responses: map[string]int{}}
	entries := []phase.Entry{{Key: "default", Provider: prov}}
	opts := buildOpts{steer: box, subagent: &runner.SubagentConfig{Limits: shipped()}}
	round := executeRound(t, entries, opts, 1, "", nil, run{calls: []llm.ToolCall{
		{ID: "d", Name: subagent.ToolName, Arguments: map[string]any{"tasks": []any{
			map[string]any{"id": "look", "prompt": "look into it", "system_prompt": "You look into things."},
		}}},
		submitWork(t),
	}}, runner.RunRecord{})
	if _, err := round.collector.Review(t.Context(), 1, round.work, nil); err != nil {
		t.Fatalf("Review: %v", err)
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
	if review := prov.requests["review"]; len(review) == 0 || !holds(review[0]) {
		t.Error("the reviewer after the delegate call did not read the note")
	}
}
