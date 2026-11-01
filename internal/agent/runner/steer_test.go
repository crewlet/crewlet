package runner_test

import (
	"context"
	"errors"
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
	"github.com/crewlet/crewlet/internal/agent/turnctx"
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
		got[0].Round != 1 || got[0].Boundary != types.SteerBoundaryRound || got[0].LaunchID != "" ||
		got[0].NoteID != "req-1" || got[0].SteeredBy != "founder" ||
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
// process collects it — opens with the note too, carried from the run's
// record (every read is written there) rather than read again.
//
// Mutations: leave the carried notes out of the brief, or out of what a
// resumed runner opens with, and one half goes red.
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
	// THE RECORD'S READS, as the first reviewer's take wrote them.
	delivered := pub.steered()
	if len(delivered) != 1 {
		t.Fatalf("published %d agent_turn_steered, want the reviewer's one", len(delivered))
	}
	read := steer.Read{Note: founderNote, Reading: steer.Reading{Phase: delivered[0].Phase,
		Iteration: delivered[0].Iteration, Round: delivered[0].Round, Boundary: delivered[0].Boundary}}
	final, _ := buildWith(t, []phase.Entry{{Key: "default", Provider: again}}, buildOpts{
		steer: steer.New(), pub: pub,
		resume: &runner.Resume{State: parked.State, Answer: "posted",
			Bridged: play(t, relaunched, deliver(t, "posted to staging")),
			Steered: []steer.Read{read}},
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
	// runs are each kind's harness runs, requests what each run was opened on,
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
	// NOT PAUSED FOR IT: a steerable loop ends its harness's run once a note is
	// waiting, and the next run reads it — so a worker steered by mistake
	// would show a second run that opens on the note. One run, its rounds
	// one harness loop, is a worker the note never reached.
	workers := prov.requests["worker"]
	if len(workers) != 1 {
		t.Errorf("the worker took %d harness runs, want one the note never ended", len(workers))
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

// watchingLauncher is a launcher that records what the turn had published of
// its notes at the moment it was asked to launch, and launches as a store
// names a job — or fails, where err is set, naming the job it opened before it
// did where wrote is set.
type watchingLauncher struct {
	pub   *capture
	err   error
	wrote string
	seen  []int
	reqs  []runner.AgentRunRequest
}

func (l *watchingLauncher) LaunchExecutor(_ context.Context, req runner.AgentRunRequest) (runner.Launched, error) {
	l.seen = append(l.seen, len(l.pub.steered()))
	l.reqs = append(l.reqs, req)
	if l.err != nil {
		return runner.Launched{LaunchID: l.wrote}, l.err
	}
	return runner.Launched{LaunchID: "job-1"}, nil
}

// A LAUNCH TAKES ITS BRIEF'S NOTES ONCE, AND RECORDS THEM DELIVERED ONLY ONCE
// LAUNCHED. The note is in the brief, after the task, and named to the launch so
// the write that opens the job records it read by that job; the box is empty
// after; and the delivery — at the brief, by the job — is published after the
// launch answered, never before: a launch that fails delivered nothing.
//
// Mutation: publish before the launch, or take the box twice, and this goes red.
func TestALaunchTakesItsBriefOnceAndPublishesItOnlyOnceLaunched(t *testing.T) {
	t.Parallel()
	box := steer.New()
	box.Offer(founderNote)
	pub := newCapture()
	launcher := &watchingLauncher{pub: pub}
	r, _ := buildWith(t, []phase.Entry{{Key: "default", Provider: &scriptedProvider{}}},
		buildOpts{steer: box, pub: pub, launcher: launcher})
	if _, _, err := r.Execute(t.Context(), 1, "", nil); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(launcher.reqs) != 1 {
		t.Fatalf("%d launches, want 1", len(launcher.reqs))
	}
	req := launcher.reqs[0]
	note := prompts.SteerMessage(founderNote.By, founderNote.Text)
	if !strings.Contains(req.Brief, note) || len(req.Notes) != 1 || req.Notes[0].ID != founderNote.ID {
		t.Fatalf("the launch was handed brief notes %+v and a brief without the note:\n%s",
			req.Notes, req.Brief)
	}
	if box.Waiting() {
		t.Error("the box still holds the note the brief took")
	}
	if launcher.seen[0] != 0 {
		t.Errorf("%d deliveries were published before the launch answered", launcher.seen[0])
	}
	got := pub.steered()
	if len(got) != 1 || got[0].Boundary != types.SteerBoundaryBrief || got[0].LaunchID != "job-1" ||
		got[0].Phase != types.PhaseExecute || got[0].Round != 1 || got[0].Iteration != 1 ||
		got[0].Outcome != types.SteerDelivered {
		t.Fatalf("published %+v, want the note delivered once, in job-1's brief", got)
	}
}

// A FAILED LAUNCH RETURNS ITS BRIEF'S NOTES: nothing read them, so nothing is
// published delivered, and they are the box's again — read by whatever step
// comes next, or recorded expired when the segment's box closes.
func TestAFailedLaunchReturnsItsBriefNotes(t *testing.T) {
	t.Parallel()
	box := steer.New()
	box.Offer(founderNote)
	pub := newCapture()
	launcher := &watchingLauncher{pub: pub, err: errors.New("no box could be had")}
	r, _ := buildWith(t, []phase.Entry{{Key: "default", Provider: &scriptedProvider{}}},
		buildOpts{steer: box, pub: pub, launcher: launcher})
	if _, _, err := r.Execute(t.Context(), 1, "", nil); err == nil {
		t.Fatal("a launch that failed reported none")
	}
	if got := pub.steered(); len(got) != 0 {
		t.Errorf("a failed launch published %+v", got)
	}
	expired := box.Close()
	if len(expired) != 1 || expired[0].ID != founderNote.ID {
		t.Errorf("the box holds %+v after the failed launch, want the note it gave the brief", expired)
	}
}

// A LAUNCH THAT FAILED AFTER ITS WRITE HANDS NOTHING BACK: it names the job it
// opened, so its brief's notes are on that job's record, read by a job that
// never ran — and the record's ending reports them. Handed back to the box as
// well, they were reported a second time when the segment closed it.
//
// Mutation: hand the brief back whatever the launch answered, and this goes red.
func TestALaunchThatFailedAfterItsWriteHandsNothingBack(t *testing.T) {
	t.Parallel()
	box := steer.New()
	box.Offer(founderNote)
	pub := newCapture()
	launcher := &watchingLauncher{pub: pub, err: errors.New("no box could be had"), wrote: "job-1"}
	r, _ := buildWith(t, []phase.Entry{{Key: "default", Provider: &scriptedProvider{}}},
		buildOpts{steer: box, pub: pub, launcher: launcher})
	if _, _, err := r.Execute(t.Context(), 1, "", nil); err == nil {
		t.Fatal("a launch that failed reported none")
	}
	if got := pub.steered(); len(got) != 0 {
		t.Errorf("a failed launch published %+v", got)
	}
	if expired := box.Close(); len(expired) != 0 {
		t.Errorf("the box holds %+v after a launch that wrote its record, want nothing", expired)
	}
}

// A RESUME CARRIES THE RUN'S READ NOTES: a segment that resumes the turn opens
// its reviewer with every note the run's record says the turn read — carried,
// not read again, so nothing more is published.
func TestAResumeCarriesTheRunsReadNotes(t *testing.T) {
	t.Parallel()
	entries := []phase.Entry{{Key: "default", Provider: &scriptedProvider{}}}
	launched, req := launch(t, entries, buildOpts{}, 1, "", nil)
	parked, _ := launched.Suspended()
	pub := newCapture()
	prov := &scriptedProvider{review: []llmtest.Response{submitCall(t, runner.SubmitReviewTool,
		`{"decision":"done","final_artifact":"a"}`)}}
	collector, _ := buildWith(t, []phase.Entry{{Key: "default", Provider: prov}}, buildOpts{
		pub: pub, steer: steer.New(),
		resume: &runner.Resume{State: parked.State, Answer: "posted",
			Bridged: play(t, req, deliver(t, "posted")),
			Steered: []steer.Read{{Note: founderNote, Reading: steer.Reading{
				Phase: types.PhaseExecute, Iteration: 1, Round: 1,
				Boundary: types.SteerBoundaryToolBatch, Launch: "job-1"}}}},
	})
	work, _, err := collector.Resume(t.Context(), nil)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if _, err := collector.Review(t.Context(), 1, work, nil); err != nil {
		t.Fatalf("Review: %v", err)
	}
	if review := prov.requestsFor("review"); len(review) == 0 || !holds(review[0]) {
		t.Fatal("the reviewer judged the run without the note the run had read")
	}
	if got := pub.steered(); len(got) != 0 {
		t.Errorf("a carried note was published again: %+v", got)
	}
}

// THE EXECUTE RECORD LISTS ITS ROUND'S READS — every job's of the round, its
// briefs' and its harness's, at the run's one round, each with where it was read —
// and no note a reviewer, the first segment or an earlier round's run read. A
// job carried on with a person's answer continues the asking job's round, whose
// calls the record lists first, so the notes the asking job read are listed
// with them, in the order read.
//
// Mutation: list the collected job's reads alone, and the asking job's note is
// missing; drop the round from the key, and the earlier round's is listed.
func TestTheExecuteRecordListsItsRoundsReads(t *testing.T) {
	t.Parallel()
	entries := []phase.Entry{{Key: "default", Provider: &scriptedProvider{}}}
	launched, req := launch(t, entries, buildOpts{}, 2, "", nil)
	parked, _ := launched.Suspended()
	pub := newCapture()
	read := func(id string, b types.SteerBoundary, ph types.Phase, iteration int, launch string) steer.Read {
		n := founderNote
		n.ID = id
		return steer.Read{Note: n, Reading: steer.Reading{Phase: ph, Iteration: iteration, Round: 1,
			Boundary: b, Launch: launch}}
	}
	collector, _ := buildWith(t, entries, buildOpts{pub: pub,
		resume: &runner.Resume{State: parked.State, Answer: "posted",
			Bridged: play(t, req, deliver(t, "posted")),
			Run:     runner.RunRecord{LaunchID: "job-2", CodingAgent: "claude-code"},
			Steered: []steer.Read{
				read("earlier-round", types.SteerBoundaryToolBatch, types.PhaseExecute, 1, "job-0"),
				read("reviewer", types.SteerBoundaryRound, types.PhaseReview, 1, ""),
				read("first-segment", types.SteerBoundaryRound, types.PhaseExecute, 2, ""),
				read("asking-job", types.SteerBoundaryToolBatch, types.PhaseExecute, 2, "job-1"),
				read("in-brief", types.SteerBoundaryBrief, types.PhaseExecute, 2, "job-2"),
				read("mid-run", types.SteerBoundaryFinish, types.PhaseExecute, 2, "job-2"),
			}}})
	if _, _, err := collector.Resume(t.Context(), nil); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	executes := phasesOfKind(t, pub, "execute")
	if len(executes) != 1 {
		t.Fatalf("%d execute records, want the collection's one", len(executes))
	}
	want := []types.PhaseSteer{
		{Round: 1, NoteID: "asking-job", Boundary: types.SteerBoundaryToolBatch},
		{Round: 1, NoteID: "in-brief", Boundary: types.SteerBoundaryBrief},
		{Round: 1, NoteID: "mid-run", Boundary: types.SteerBoundaryFinish},
	}
	if !slices.Equal(executes[0].Steers, want) {
		t.Errorf("the execute record lists %+v, want %+v", executes[0].Steers, want)
	}
}

// A FRESH SESSION'S BRIEF CARRIES EVERY READ NOTE; A CONTINUED ONE'S ONLY THE NEW.
// A run carried on with a person's answer continues the asking run's own harness
// session where it can, which already holds every note it read — so its
// prompt is the answer and the notes since; a fresh run started instead holds
// nothing, so its brief carries every note the turn read and then the new ones.
// Both take the new notes once, named to the launch.
func TestABriefCarriesTheNotesItsSessionLacks(t *testing.T) {
	t.Parallel()
	entries := []phase.Entry{{Key: "default", Provider: &scriptedProvider{}}}
	launched, req := launch(t, entries, buildOpts{}, 1, "", nil)
	before := play(t, req, run{})
	parked, _ := launched.Suspended()
	parked.State.ActiveTools = req.Surface.Active()

	carried := steer.Note{ID: "carried", Text: "use the staging database", By: "founder"}
	fresh := steer.Note{ID: "fresh", Text: "and tag the release", By: "founder"}
	box := steer.New()
	box.Offer(fresh)
	relauncher := &playLauncher{}
	collector, _ := buildWith(t, entries, buildOpts{
		calls: turnctx.NewCallLog(), launcher: relauncher, steer: box,
		resume: &runner.Resume{State: parked.State, Bridged: before,
			Asked: &runner.Asked{Question: "which branch?", Answer: "use main", By: "founder",
				SessionID: "sess-ask", BoxKept: true},
			Steered: []steer.Read{{Note: carried, Reading: steer.Reading{Phase: types.PhaseExecute,
				Iteration: 1, Round: 1, Boundary: types.SteerBoundaryToolBatch, Launch: "job-0"}}}},
	})
	if _, _, err := collector.Resume(t.Context(), nil); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	again := relauncher.last(t)
	carriedMsg := prompts.SteerMessage(carried.By, carried.Text)
	freshMsg := prompts.SteerMessage(fresh.By, fresh.Text)
	if !strings.Contains(again.Brief, carriedMsg) || !strings.Contains(again.Brief, freshMsg) ||
		strings.Index(again.Brief, carriedMsg) > strings.Index(again.Brief, freshMsg) {
		t.Errorf("a fresh run's brief does not carry every read note and then the new one:\n%s",
			again.Brief)
	}
	if again.Continue == nil || strings.Contains(again.Continue.Brief, carriedMsg) ||
		!strings.Contains(again.Continue.Brief, freshMsg) || !strings.Contains(again.Continue.Brief, "use main") {
		t.Errorf("the continued session's prompt is %+v, want the answer and only the new note",
			again.Continue)
	}
	if len(again.Notes) != 1 || again.Notes[0].ID != "fresh" {
		t.Errorf("the launch names %+v as its brief's new notes, want only the fresh one", again.Notes)
	}
	if box.Waiting() {
		t.Error("the new note is still waiting after the brief took it")
	}
}
