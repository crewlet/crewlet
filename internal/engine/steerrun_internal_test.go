package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/agent/steer"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/httpx/httpxtest"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/provenance"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/llmtest"
	"github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/runtoken"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
	"github.com/crewlet/crewlet/internal/sandbox/sandboxtest"
	"github.com/crewlet/crewlet/internal/seat"
	"github.com/crewlet/crewlet/internal/tools"
)

// A person's note to a turn whose executor is out, through the real turn: the
// launch, the run's bridge, a go-sdk client playing the run's harness, and the
// resumed segment that reviews it. What each case holds is where the note goes
// and what it is recorded as, whichever step reads it.

// steerable gives e what every engine `crewlet run` builds to steer a turn: the
// desk its turns' boxes register with and the run steers over its fleet's run
// records — an engine a case builds by hand has neither.
func steerable(t *testing.T, e *Engine) {
	t.Helper()
	seals, err := newSteerSeals(steerRing)
	if err != nil {
		t.Fatal(err)
	}
	e.runSteers = newRunSteers(seals, sandbox.NewCoordStore(e.backends.Fleet), e.backends.Queue)
	e.steers = mustSteerDesk(t, steerRing)
	e.steers.runs = e.runSteers
}

// reviewWatch is [scripted], recording what its reviewer is opened with — and
// sending the round back for its first sendBack reviews.
type reviewWatch struct {
	scripted
	mu       sync.Mutex
	reviews  []string
	sendBack int
}

func (r *reviewWatch) Complete(ctx context.Context, req llm.Request) (*llm.Completion, error) {
	if req.Tools != nil {
		if offered, err := offeredTo(req); err == nil && offers(offered, runner.SubmitReviewTool) {
			var b strings.Builder
			for _, m := range req.Messages {
				b.WriteString(m.Content)
				b.WriteString("\n")
			}
			r.mu.Lock()
			r.reviews = append(r.reviews, b.String())
			back := r.sendBack > 0
			if back {
				r.sendBack--
			}
			r.mu.Unlock()
			if back {
				return phasedHarness{model: "scripted", pick: func([]string) llmtest.Run {
					return calling(runner.SubmitReviewTool, map[string]any{
						"decision": "self_iterate", "notes": "not done yet: run it again",
					}, llmtest.DefaultUsage)
				}}.Complete(ctx, req)
			}
		}
	}
	return r.scripted.Complete(ctx, req)
}

// reviewed is what the reviewer was opened with, every round.
func (r *reviewWatch) reviewed() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.reviews)
}

// steerCase is a seat whose turn parks on its executor, steerable, played by
// a harness that does pull with the run's bridge before its script.
type steerCase struct {
	e     *Engine
	rig   *runRig
	q     *memory.Queue
	model *reviewWatch

	// dial, when set, is the address the harness dials instead of the one it
	// was launched with: its node restarted under it.
	mu   sync.Mutex
	dial string
}

// pull is what a run's harness does with its bridge before its script: its hooks
// and its plugin, played by a case — over the run's session, the notes route
// its launch hands it beside the session's endpoint, and a read of its record.
type pull func(ctx context.Context, session *mcp.ClientSession, notes string, row func() sandbox.PendingRun)

func newSteerCase(t *testing.T, before pull) *steerCase {
	t.Helper()
	return newSteerCaseOn(t, &reviewWatch{}, before)
}

// newSteerCaseOn is [newSteerCase] over a model of the case's own.
func newSteerCaseOn(t *testing.T, model *reviewWatch, before pull) *steerCase {
	t.Helper()
	e, _ := indicatingWith(t, notify.StatusAlways, model)
	steerable(t, e)
	c := &steerCase{e: e, q: e.backends.Queue.(*memory.Queue), model: model}
	script := modelPlayer(t, model.run)
	client := httpxtest.Pool(t)
	c.rig = rigRuns(t, e, func(ctx context.Context, req sandbox.RunRequest) sandbox.Result {
		endpoint := req.MCPServers[config.ToolServerName].URL
		c.mu.Lock()
		if c.dial != "" {
			endpoint = c.dial
		}
		c.mu.Unlock()
		if before != nil {
			session, err := mcp.NewClient(&mcp.Implementation{Name: "claude-code", Version: "1"}, nil).
				Connect(ctx, &mcp.StreamableClientTransport{Endpoint: endpoint, HTTPClient: client}, nil)
			if err != nil {
				return sandbox.Result{Error: "connecting to the bridge: " + err.Error()}
			}
			before(ctx, session, mcpbridge.SteerURL(endpoint), func() sandbox.PendingRun {
				// THE CASE'S ONE TURN: every case here parks run-1.
				row, _, _ := e.sandbox.Load().pending.Get(ctx, "run-1")
				return row
			})
			_ = session.Close()
		}
		req.MCPServers = map[string]sandbox.MCPServer{config.ToolServerName: {URL: endpoint}}
		return script(ctx, req)
	})
	return c
}

// file files a note to runID's turn as steer_turn does once the run is out.
func (c *steerCase) file(t *testing.T, runID, id, text string) {
	t.Helper()
	filed, err := c.e.RunSteers().File(t.Context(), runID, steer.Note{
		ID: id, Text: text, By: "founder", ByKind: iam.ActorHuman, OperatorID: "session:f",
	})
	if err != nil || filed.Status != steer.StatusAccepted {
		t.Fatalf("filing %s = %+v, %v", id, filed, err)
	}
}

// steered is every agent_turn_steered the case's queue has carried.
func (c *steerCase) steered() []*events.Event {
	var out []*events.Event
	for _, ev := range c.q.History() {
		if ev.Type == (types.AgentTurnSteered{}).EventType() {
			out = append(out, ev)
		}
	}
	return out
}

// records is every record of what became of a note the case's queue has
// carried, in the order it carried them.
func (c *steerCase) records() []types.AgentTurnSteered {
	var out []types.AgentTurnSteered
	for _, ev := range c.steered() {
		rec, _ := events.DataAs[*types.AgentTurnSteered](ev)
		out = append(out, *rec)
	}
	return out
}

// awaitSteered waits until the queue has carried n records of what became of a
// note, and answers them.
func (c *steerCase) awaitSteered(t *testing.T, n int) []types.AgentTurnSteered {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for len(c.steered()) < n && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	out := c.records()
	if len(out) < n {
		t.Fatalf("%d records of a note were published, want %d", len(out), n)
	}
	return out
}

// steeringOver is every record of a note the queue carried for runID's turn
// once the turn's steering is over: the n the case expects carried, each
// published off the step that read its note, and the run's record gone with
// its turn. Every step that could record a note again runs before the record
// goes: a later hook (answers what it took), the review (reads what the run
// left waiting) and the ending (records every note still waiting as expired,
// then deletes the record). So the records carried then are all the turn has,
// however late a duplicate came.
//
// NOT A NAP AFTER THE FIRST RECORD, which would miss a duplicate from a later
// step, such as an ending off the resume's frame.
func (c *steerCase) steeringOver(t *testing.T, runID string, n int) []types.AgentTurnSteered {
	t.Helper()
	c.awaitSteered(t, n)
	pending := c.e.sandbox.Load().pending
	eventually(t, "the run's record to go with its turn", func() bool {
		_, found, err := pending.Get(t.Context(), runID)
		return err == nil && !found
	})
	return c.records()
}

// takeAt is one steer_take a Claude Code hook makes, answered.
func takeAt(ctx context.Context, session *mcp.ClientSession, args map[string]any) (string, bool, error) {
	res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: mcpbridge.SteerTool, Arguments: args})
	if err != nil {
		return "", false, err
	}
	var text []string
	for _, content := range res.Content {
		if tc, ok := content.(*mcp.TextContent); ok {
			text = append(text, tc.Text)
		}
	}
	return strings.Join(text, ""), res.IsError, nil
}

// batchTake is the hook after a batch of the main agent's tool calls, asked
// again until it answers or its deadline passes — as the harness asks at every
// batch, the bridge answers from its view of the record, which follows a filing
// by a moment.
func batchTake(ctx context.Context, session *mcp.ClientSession) (string, error) {
	deadline := time.Now().Add(5 * time.Second)
	for {
		got, failed, err := takeAt(ctx, session, map[string]any{"event": "PostToolBatch", "agent": ""})
		if err != nil || failed {
			return got, err
		}
		if got != "" || time.Now().After(deadline) {
			return got, nil
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A NOTE TO A RUNNING RUN IS FILED ON ITS RECORD, sealed, where any node reads
// it: the record holds the note under the person's request id and nothing of
// its words in the clear, and steer_turn is told the run reads it at its next
// step.
func TestANoteToARunningRunIsFiledOnItsRecord(t *testing.T) {
	t.Parallel()
	c := newSteerCase(t, nil)
	parkOnARun(t, c.e, c.rig, "run-1")
	filed, err := c.e.RunSteers().File(t.Context(), "run-1", steer.Note{
		ID: "req-1", Text: "use staging", By: "founder", ByKind: iam.ActorHuman})
	if err != nil || filed.Status != steer.StatusAccepted || filed.Reads != steer.ReadsNextStep ||
		filed.AgentHandle != "swe" {
		t.Fatalf("File = %+v, %v, want accepted, read at the run's next step, on swe", filed, err)
	}
	row := c.rig.row(t, "run-1")
	if len(row.Steer.Notes) != 1 || row.Steer.Notes[0].ID != "req-1" || !row.Steer.Notes[0].Waiting() {
		t.Fatalf("the record holds %+v, want the note waiting", row.Steer.Notes)
	}
	// THE SEAL IS THE ONLY COPY: nothing beside it on the record carries the
	// note's words or who sent them for a reader to use without opening it.
	bare := row
	bare.Steer.Notes = []sandbox.RunNote{row.Steer.Notes[0]}
	bare.Steer.Notes[0].Sealed = nil
	if raw, err := json.Marshal(bare); err != nil || strings.Contains(string(raw), "use staging") ||
		strings.Contains(string(raw), "founder") {
		t.Errorf("the record carries the note beside its seal: %s", raw)
	}
	if _, ok := c.e.runSteers.open("run-1", row.Steer.Notes[0]); !ok {
		t.Error("the note on the record does not open as this turn's")
	}
	if seat := c.e.runSteers.openSeat(row); seat.Agent != row.AgentID || seat.Handle != "swe" {
		t.Errorf("the record's anchor opens as %+v, want the run's seat", seat)
	}
}

// STEER_TAKE ANSWERS THE HOOK WITH THE NOTE: a Claude Code hook after a batch of
// the main agent's calls is answered with the note as the hook's additional
// context, in the shape the harness reads, under its take's token; and once the
// job's session shows it took the note, it is read there, by this job, at that
// kind of step.
func TestSteerTakeAnswersTheHookWithTheNote(t *testing.T) {
	t.Parallel()
	var answer string
	var takeErr error
	c := newSteerCase(t, func(ctx context.Context, s *mcp.ClientSession, _ string, _ func() sandbox.PendingRun) {
		answer, takeErr = batchTake(ctx, s)
	})
	parkOnARun(t, c.e, c.rig, "run-1")
	c.file(t, "run-1", "req-1", "use staging")
	row := c.rig.row(t, "run-1")
	launch := row.LaunchID
	c.rig.runners[row.Harness].MainThread = showsEveryTake
	if err := c.rig.collect(t, "run-1"); err != nil {
		t.Fatalf("collect: %v", err)
	}
	if takeErr != nil {
		t.Fatalf("steer_take: %v", takeErr)
	}
	if !strings.Contains(answer, `"hookEventName":"PostToolBatch"`) ||
		!strings.Contains(answer, `"additionalContext":"## A note from founder`) ||
		!strings.Contains(answer, "use staging") || !takeToken.MatchString(answer) {
		t.Fatalf("the hook was answered %q, want the note as its additional context", answer)
	}
	got := c.awaitSteered(t, 1)[0]
	if got.Outcome != types.SteerDelivered || got.Phase != types.PhaseExecute ||
		got.Boundary != types.SteerBoundaryToolBatch || got.LaunchID != launch || got.Round != 1 {
		t.Errorf("the note was recorded %+v, want delivered in execute at a tool batch of job %s",
			got, launch)
	}
}

// A COLLECTION CONFIRMS WHAT THE SESSION SHOWS, AND PUBLISHES IT ONCE. The hook
// that took the note served it, and the run's later hook is served it no more;
// the read that collects the job, finding the job's session shows the hook
// took it, records it read by the job at the serve's step and publishes it
// delivered there, once: the reviewer, which reads every note the turn has
// read, records none of it again.
//
// Mutation: publish it at the take as well, or offer the live-served note at
// the finish, and a check goes red.
func TestACollectionConfirmsWhatTheSessionShowsAndPublishesItOnce(t *testing.T) {
	t.Parallel()
	var again string
	c := newSteerCase(t, func(ctx context.Context, s *mcp.ClientSession, _ string, _ func() sandbox.PendingRun) {
		if _, err := batchTake(ctx, s); err != nil {
			return
		}
		again, _, _ = takeAt(ctx, s, map[string]any{"event": "Stop", "agent": ""})
	})
	parkOnARun(t, c.e, c.rig, "run-1")
	c.file(t, "run-1", "req-1", "use staging")
	row := c.rig.row(t, "run-1")
	c.rig.runners[row.Harness].MainThread = showsEveryTake
	if err := c.rig.collect(t, "run-1"); err != nil {
		t.Fatalf("collect: %v", err)
	}
	if again != "" {
		t.Errorf("the run's finish was served the note again: %q", again)
	}
	deliveredAtTheBatch(t, c.steeringOver(t, "run-1", 1), "req-1", row.LaunchID)
}

// THE REVIEWER OPENS WITH A NOTE THE RUN TOOK: the run's read is on its record
// once its collection found the job's session took it, so the segment that
// reviews the run carries it — the reviewer judges the work against the note
// the work was steered by.
func TestTheReviewerOpensWithANoteTheRunTook(t *testing.T) {
	t.Parallel()
	c := newSteerCase(t, func(ctx context.Context, s *mcp.ClientSession, _ string, _ func() sandbox.PendingRun) {
		_, _ = batchTake(ctx, s)
	})
	parkOnARun(t, c.e, c.rig, "run-1")
	c.rig.runners[c.rig.row(t, "run-1").Harness].MainThread = showsEveryTake
	c.file(t, "run-1", "req-1", "use the staging database")
	if err := c.rig.collect(t, "run-1"); err != nil {
		t.Fatalf("collect: %v", err)
	}
	reviews := c.model.reviewed()
	if len(reviews) == 0 {
		t.Fatal("no reviewer ran")
	}
	if !strings.Contains(reviews[0], "use the staging database") {
		t.Errorf("the reviewer opened without the note the run took:\n%s", reviews[0])
	}
}

// A SUBAGENT'S BATCH TAKES NOTHING, and neither does a hook whose agent the harness
// did not fill in: a note handed to a delegated loop binds that piece of work
// alone and is lost with it. Both leave the note waiting, for the main agent's
// next boundary or — as here, where it has none — the reviewer.
//
// ASKED ONCE THE NOTE IS IN THE BRIDGE'S VIEW, which a main agent's batch is
// answered from ([awaitInView]). The note is filed before the run plays and
// the view follows the record by a moment: a take made before the view held
// it answered nothing whether or not a subagent's batch takes notes.
//
// Mutation: answer a subagent's batch as the main agent's, and a1's takes the
// note.
func TestASubagentsBatchTakesNothing(t *testing.T) {
	t.Parallel()
	var answers []string
	var waiting []string
	var viewed error
	client := httpxtest.Pool(t)
	c := newSteerCase(t, func(ctx context.Context, s *mcp.ClientSession, notes string, row func() sandbox.PendingRun) {
		if viewed = awaitInView(ctx, client, notes, "req-1"); viewed != nil {
			return
		}
		for _, agent := range []string{"a1", "${agent_id}"} {
			got, _, _ := takeAt(ctx, s, map[string]any{"event": "PostToolBatch", "agent": agent})
			answers = append(answers, got)
		}
		for _, n := range row().Steer.Notes {
			if n.Waiting() {
				waiting = append(waiting, n.ID)
			}
		}
	})
	parkOnARun(t, c.e, c.rig, "run-1")
	c.file(t, "run-1", "req-1", "use staging")
	if err := c.rig.collect(t, "run-1"); err != nil {
		t.Fatalf("collect: %v", err)
	}
	if viewed != nil {
		t.Fatalf("the premise: %v", viewed)
	}
	for _, got := range answers {
		if got != "" {
			t.Errorf("a subagent's hook was answered %q", got)
		}
	}
	if !slices.Equal(waiting, []string{"req-1"}) {
		t.Errorf("after the subagents' hooks the notes waiting were %v, want the note", waiting)
	}
	if got := c.awaitSteered(t, 1)[0]; got.Phase != types.PhaseReview || got.Boundary != types.SteerBoundaryRound {
		t.Errorf("the note was recorded %+v, want read by the reviewer", got)
	}
}

// STEER_TAKE IS NEVER LISTED: the harness's model is offered the seat's tools and
// never the hook's, which only the harness calls.
func TestSteerTakeIsNeverListed(t *testing.T) {
	t.Parallel()
	var listed []string
	c := newSteerCase(t, func(ctx context.Context, s *mcp.ClientSession, _ string, _ func() sandbox.PendingRun) {
		tools, err := s.ListTools(ctx, nil)
		if err != nil {
			return
		}
		for _, tool := range tools.Tools {
			listed = append(listed, tool.Name)
		}
	})
	parkOnARun(t, c.e, c.rig, "run-1")
	if err := c.rig.collect(t, "run-1"); err != nil {
		t.Fatalf("collect: %v", err)
	}
	if len(listed) == 0 {
		t.Fatal("the run listed no tools, so this asserts nothing")
	}
	if slices.Contains(listed, mcpbridge.SteerTool) {
		t.Errorf("the run's model is offered %s: %v", mcpbridge.SteerTool, listed)
	}
}

// A NOTE THE RUN NEVER PULLED IS READ BY THE REVIEWER, at its first round: a harness
// that never asked between its steps — one with no reader, or a run that
// finished first — leaves its notes waiting on the record, and the segment that
// reviews the run takes them before its first provider call.
func TestANoteTheRunNeverPulledIsReadByTheReviewer(t *testing.T) {
	t.Parallel()
	c := newSteerCase(t, nil)
	parkOnARun(t, c.e, c.rig, "run-1")
	c.file(t, "run-1", "req-1", "use the staging database")
	if err := c.rig.collect(t, "run-1"); err != nil {
		t.Fatalf("collect: %v", err)
	}
	got := c.awaitSteered(t, 1)[0]
	if got.Outcome != types.SteerDelivered || got.Phase != types.PhaseReview || got.Round != 1 ||
		got.Boundary != types.SteerBoundaryRound || got.LaunchID != "" {
		t.Errorf("the note was recorded %+v, want delivered at the reviewer's round 1", got)
	}
	if reviews := c.model.reviewed(); len(reviews) == 0 || !strings.Contains(reviews[0], "use the staging database") {
		t.Errorf("the reviewer's first round did not read the note: %v", reviews)
	}
}

// A RUN IS STEERABLE AFTER ITS LAUNCHING NODE RESTARTS: nothing that holds a note
// lives in one process, so a note filed by another node onto a run whose bridge
// was opened again after a restart reaches the run's harness there — and a note the
// run read before the restart is still the turn's, carried into its review.
func TestARunIsSteerableAfterItsLaunchingNodeRestarts(t *testing.T) {
	t.Parallel()
	var answer string
	c := newSteerCase(t, func(ctx context.Context, s *mcp.ClientSession, _ string, _ func() sandbox.PendingRun) {
		answer, _, _ = takeAt(ctx, s, map[string]any{"event": "Stop", "agent": ""})
	})
	endpoint := parkOnARun(t, c.e, c.rig, "run-1")
	row := c.rig.row(t, "run-1")

	// BEFORE THE RESTART the run reads one note at a batch.
	c.file(t, "run-1", "req-before", "use the staging database")
	before := c.e.runSteers.inbox(identityOfRun(row), 1, row.Harness)
	before.Bind(t.Context(), row.LaunchID, provenance.NewBoundary())
	got := before.Take(t.Context(), mcpbridge.SteerEventFinish, hookCall())
	if !strings.Contains(got, "staging") {
		t.Fatalf("the run was not served its first note before the restart: %q", got)
	}
	servedNotRead(t, c.e.sandbox.Load().pending, "run-1", "req-before", row.LaunchID, takeOf(t, got))
	before.Close()

	// THE NODE RESTARTS: a fresh bridge holding nothing, and the seat taken
	// back. ANOTHER NODE files the second note, on the record.
	dialed := restartBridge(t, c.e, endpoint)
	seals, err := newSteerSeals(steerRing)
	if err != nil {
		t.Fatal(err)
	}
	other := newRunSteers(seals, sandbox.NewCoordStore(c.e.backends.Fleet), c.e.backends.Queue)
	if filed, err := other.File(t.Context(), "run-1", steer.Note{ID: "req-after",
		Text: "and tag the release", By: "founder"}); err != nil || filed.Status != steer.StatusAccepted {
		t.Fatalf("another node's filing = %+v, %v", filed, err)
	}
	c.e.reopenAgentRun(t.Context(), c.rig.row(t, "run-1"))
	c.mu.Lock()
	c.dial = dialed
	c.mu.Unlock()

	if err := c.rig.collect(t, "run-1"); err != nil {
		t.Fatalf("collect: %v", err)
	}
	if !strings.Contains(answer, "and tag the release") {
		t.Errorf("the run's harness was not handed the note after the restart: %q", answer)
	}
	reviews := c.model.reviewed()
	if len(reviews) == 0 || !strings.Contains(reviews[0], "use the staging database") ||
		!strings.Contains(reviews[0], "and tag the release") {
		t.Errorf("the reviewer did not carry both notes the run read: %v", reviews)
	}
}

// failingStore is a run store whose reads and takes fail, or whose watch never
// hears — the two ways a run's inbox can be left without the record.
type failingStore struct {
	sandbox.PendingStore
	reads     bool
	deafWatch bool
}

func (s failingStore) Get(ctx context.Context, turnID string) (sandbox.PendingRun, bool, error) {
	if s.reads {
		return sandbox.PendingRun{}, false, context.DeadlineExceeded
	}
	return s.PendingStore.Get(ctx, turnID)
}

func (s failingStore) TakeNotes(ctx context.Context, turnID string, take sandbox.NoteTake) ([]sandbox.RunNote, error) {
	if s.reads {
		return nil, context.DeadlineExceeded
	}
	return s.PendingStore.TakeNotes(ctx, turnID, take)
}

func (s failingStore) ServeHook(ctx context.Context, turnID string, serve sandbox.HookServe) ([]sandbox.RunNote,
	error) {
	if s.reads {
		return nil, context.DeadlineExceeded
	}
	return s.PendingStore.ServeHook(ctx, turnID, serve)
}

func (s failingStore) WatchNotes(ctx context.Context, turnID string) (<-chan sandbox.NotesView, error) {
	if s.deafWatch {
		never := make(chan sandbox.NotesView)
		go func() {
			<-ctx.Done()
			close(never)
		}()
		return never, nil
	}
	return s.PendingStore.WatchNotes(ctx, turnID)
}

// A STORE FAILURE ANSWERS EMPTY, NEVER AN ERROR: a hook whose call is answered
// with an error makes Claude Code drop and re-initialise the run's whole bridge,
// so a take the record cannot serve is answered with nothing, the note waits
// for the next step, and a read that could not be written is the poller's to
// retry.
func TestAStoreFailureAnswersEmptyNeverAnError(t *testing.T) {
	t.Parallel()
	store := sandbox.NewCoordStore(coordmem.NewFleet())
	runs, _ := steersOver(t, steerRing, store)
	launch := workingRun(t, store, runs, "run-1")
	fileSealed(t, runs, "run-1", steer.Note{ID: "n-1", Text: "use staging", By: "founder"})
	broken, _ := steersOver(t, steerRing, failingStore{PendingStore: store, reads: true})
	inbox := boundInbox(t, broken, store, "run-1", launch)
	for _, event := range []string{mcpbridge.SteerEventToolBatch, mcpbridge.SteerEventFinish} {
		if got := inbox.Take(t.Context(), event, hookCall()); got != "" {
			t.Errorf("a take the record could not serve at %s answered %q", event, got)
		}
	}
	if err := inbox.Read(t.Context(), []string{"n-1"}); err == nil {
		t.Error("a read that could not be written answered done")
	}
	if _, waiting := recordedNotes(t, store, "run-1"); !slices.Equal(waiting, []string{"n-1"}) {
		t.Errorf("the note does not wait after the failures: %v", waiting)
	}
}

// lostAnswerStore is a run store whose takes land and answer an error, as a take
// does whose compare-and-swap the store answered after its caller's deadline;
// its settles fail as many times as failSettles says before they are made.
type lostAnswerStore struct {
	sandbox.PendingStore
	mu          sync.Mutex
	failSettles int
}

func (s *lostAnswerStore) TakeNotes(ctx context.Context, turnID string, take sandbox.NoteTake) ([]sandbox.RunNote,
	error) {
	if _, err := s.PendingStore.TakeNotes(ctx, turnID, take); err != nil {
		return nil, err
	}
	return nil, context.DeadlineExceeded
}

func (s *lostAnswerStore) SettleTake(ctx context.Context, turnID string, settle sandbox.NoteSettle) (
	[]sandbox.RunNote, error) {
	s.mu.Lock()
	failing := s.failSettles > 0
	if failing {
		s.failSettles--
	}
	s.mu.Unlock()
	if failing {
		return nil, context.DeadlineExceeded
	}
	return s.PendingStore.SettleTake(ctx, turnID, settle)
}

// readOn is the read turnID's record holds of note id, nil while it waits or
// the record cannot say.
func readOn(ctx context.Context, store sandbox.PendingStore, turnID, id string) *sandbox.NoteRead {
	run, found, err := store.Get(ctx, turnID)
	if err != nil || !found {
		return nil
	}
	for _, n := range run.Steer.Notes {
		if n.ID == id {
			return n.Read
		}
	}
	return nil
}

// claimedSource is the notes of a segment that resumes run-1 from launch, as
// runs serves them, closed with the case.
func claimedSource(t *testing.T, store sandbox.PendingStore, runs *runSteers, launch string) *rowSource {
	t.Helper()
	claimed, won, err := store.ClaimForResume(t.Context(), "run-1", sandbox.CompletionTail(launch), sandbox.Fence{})
	if err != nil || !won {
		t.Fatalf("ClaimForResume = %v, %v", won, err)
	}
	source := runs.source(t.Context(), claimed, sandbox.Fence{Owner: claimed.Owner, Epoch: claimed.OwnerEpoch})
	t.Cleanup(func() { source.Close(t.Context()) })
	return source
}

// viewCaughtUp waits until the segment's view of the record holds no waiting
// note: a view follows the record a write behind, and a note it still shows
// waiting would answer Waiting for the lost take a case asks about.
func viewCaughtUp(t *testing.T, source *rowSource) {
	t.Helper()
	eventually(t, "the segment's view to hold no waiting note", func() bool {
		view, opened, _ := source.watch.snapshot()
		return view.Found && len(opened) == 0
	})
}

// reviewRound is the reading of round n of a review's first iteration.
func reviewRound(n int) steer.Reading {
	return steer.Reading{Phase: types.PhaseReview, Iteration: 1, Round: n, Boundary: types.SteerBoundaryRound}
}

// A REVIEW'S TAKE WHOSE ANSWER WAS LOST HANDS ITS NOTES OVER ALL THE SAME. A take
// of a resumed segment whose write landed and whose answer was an error is
// settled on its own path: the settle answers what the take recorded and the
// round is handed that. Where the store answers the settle no more than the
// take, the take is kept as a note waiting; the next round's take, or a
// relaunch's brief, settles it again, never marking it. Only a take still
// unsettled when the segment ends is handed off its path, marked unanswered.
//
// Mutation: hand nothing where the settle answers, settle at no later round or
// brief, mark where one answers, forget on a handed-back brief, misplace or
// misnumber the read, or never hand it off at the segment's end: red.
func TestAReviewsTakeWhoseAnswerWasLostHandsItsNotesOver(t *testing.T) {
	t.Parallel()
	setup := func(t *testing.T, failSettles int) (*sandbox.CoordStore, *runSteers, *rowSource) {
		t.Helper()
		store := sandbox.NewCoordStore(coordmem.NewFleet())
		runs, _ := steersOver(t, steerRing, &lostAnswerStore{PendingStore: store, failSettles: failSettles})
		runs.stopped = t.Context().Done()
		launch := workingRun(t, store, runs, "run-1")
		fileSealed(t, runs, "run-1", steer.Note{ID: "n-1", Text: "use staging", By: "founder"})
		return store, runs, claimedSource(t, store, runs, launch)
	}
	handed := func(t *testing.T, got []steer.Note, what string) {
		t.Helper()
		if len(got) != 1 || got[0].ID != "n-1" || got[0].Text != "use staging" {
			t.Fatalf("%s was handed %+v, want the note the lost take recorded", what, got)
		}
	}
	answered := func(t *testing.T, store *sandbox.CoordStore) {
		t.Helper()
		if read := readOn(t.Context(), store, "run-1", "n-1"); read == nil || read.Unanswered ||
			read.Boundary != string(types.SteerBoundaryRound) || read.Take == "" {
			t.Errorf("the note handed over is read %+v, want the review take's read, never marked", read)
		}
	}
	t.Run("its settle answering", func(t *testing.T) {
		t.Parallel()
		store, _, source := setup(t, 0)
		handed(t, source.Take(t.Context(), reviewRound(1)), "the round whose take was lost")
		answered(t, store)
		viewCaughtUp(t, source)
		if source.Waiting() {
			t.Error("a source with nothing left to hand over reports a note waiting")
		}
	})
	t.Run("its settle failing once, the next round settling it", func(t *testing.T) {
		t.Parallel()
		store, _, source := setup(t, 1)
		if got := source.Take(t.Context(), reviewRound(1)); len(got) != 0 {
			t.Fatalf("the round was handed %+v by a take nothing settled", got)
		}
		viewCaughtUp(t, source)
		if !source.Waiting() {
			t.Error("a lost take the store may yet settle does not stand for a note waiting")
		}
		handed(t, source.Take(t.Context(), reviewRound(2)), "the next round")
		answered(t, store)
		if source.Waiting() {
			t.Error("a source whose lost take was handed over still reports a note waiting")
		}
	})
	t.Run("its settle failing once, a relaunch's brief settling it", func(t *testing.T) {
		t.Parallel()
		store, _, source := setup(t, 1)
		if got := source.Take(t.Context(), reviewRound(1)); len(got) != 0 {
			t.Fatalf("the round was handed %+v by a take nothing settled", got)
		}
		brief := source.Brief(t.Context())
		handed(t, brief.Notes, "the relaunch's brief")
		// THE LAUNCH FAILS BEFORE ITS WRITE: the brief, and the take with it,
		// come back, and the next round hands the note over again.
		source.Return(t.Context(), brief.Notes)
		handed(t, source.Take(t.Context(), reviewRound(2)), "the round after the failed launch")
		answered(t, store)
	})
	t.Run("its settle failing once, a relaunch's brief settling it and its launch writing", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		store, runs, source := setup(t, 1)
		if got := source.Take(ctx, reviewRound(1)); len(got) != 0 {
			t.Fatalf("the round was handed %+v by a take nothing settled", got)
		}
		fileSealed(t, runs, "run-1", steer.Note{ID: "n-2", Text: "and tag the release", By: "founder"})
		brief := source.Brief(ctx)
		var briefed []string
		for _, n := range brief.Notes {
			briefed = append(briefed, n.ID)
		}
		if !slices.Equal(briefed, []string{"n-1", "n-2"}) {
			t.Fatalf("the relaunch's brief carries %v, want the lost take's note, then the waiting one", briefed)
		}
		// THE LAUNCH'S WRITE, its brief sealed as its launcher seals it, and
		// then the launch fails: its job never starts.
		sealed, err := runs.briefNotes("run-1", 2, brief.Notes, source)
		if err != nil {
			t.Fatal(err)
		}
		claimed := mustRow(t, store, "run-1")
		relaunch := claimed
		relaunch.BriefNotes = sealed
		relaunched, err := store.BeginLaunch(ctx, relaunch, sandbox.Fence{Owner: claimed.Owner,
			Epoch: claimed.OwnerEpoch})
		if err != nil {
			t.Fatalf("BeginLaunch: %v", err)
		}
		row := mustRow(t, store, "run-1")
		for _, read := range runs.carried(row) {
			if read.Phase != types.PhaseExecute || read.Iteration != 2 ||
				read.Boundary != types.SteerBoundaryBrief || read.Launch != relaunched.LaunchID {
				t.Errorf("note %s is carried read %+v, want it read by job %s's brief", read.ID, read.Reading,
					relaunched.LaunchID)
			}
		}
		var expired []string
		for _, n := range row.EndedNotes() {
			if n.Outcome == types.SteerExpired {
				expired = append(expired, n.Note.ID)
			}
		}
		if !slices.Equal(expired, []string{"n-1", "n-2"}) {
			t.Errorf("the ending of the job that never started reports %v expired, want every note its brief "+
				"carried", expired)
		}
	})
	t.Run("a later round's note correcting it, its brief handing it over in its place", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		// ITS SETTLE FAILING TWICE: on the round's own path, and again at the
		// next round, whose own take of the later note is answered.
		store, runs, source := setup(t, 2)
		if got := source.Take(ctx, reviewRound(1)); len(got) != 0 {
			t.Fatalf("the round was handed %+v by a take nothing settled", got)
		}
		fileSealed(t, runs, "run-1", steer.Note{ID: "n-2", Text: "no, use production instead of staging",
			By: "founder"})
		if got := source.Take(ctx, reviewRound(2)); len(got) != 1 || got[0].ID != "n-2" {
			t.Fatalf("the next round was handed %+v, want the later note alone", got)
		}
		brief := source.Brief(ctx)
		handed(t, brief.Notes, "the relaunch's brief")
		if lost, later := brief.Orders["n-1"], brief.Orders["n-2"]; lost == 0 || later <= lost {
			t.Errorf("the brief places the lost take's note at %d and the later round's at %d, want the lost "+
				"one first, where its take was made", lost, later)
		}
		sealed, err := runs.briefNotes("run-1", 2, brief.Notes, source)
		if err != nil {
			t.Fatal(err)
		}
		claimed := mustRow(t, store, "run-1")
		relaunch := claimed
		relaunch.BriefNotes = sealed
		if _, err := store.BeginLaunch(ctx, relaunch, sandbox.Fence{Owner: claimed.Owner,
			Epoch: claimed.OwnerEpoch}); err != nil {
			t.Fatalf("BeginLaunch: %v", err)
		}
		// EVERY LATER BRIEF TELLS THE TWO IN THE ORDER THE RECORD READ THEM,
		// the correction after the note it corrects.
		var carried []string
		for _, read := range runs.carried(mustRow(t, store, "run-1")) {
			carried = append(carried, read.ID)
		}
		if !slices.Equal(carried, []string{"n-1", "n-2"}) {
			t.Errorf("the record carries the notes as %v, want n-1 then n-2: the later round's note, "+
				"correcting it, is told ahead of the note it corrects", carried)
		}
	})
	t.Run("its settle failing until the segment ends", func(t *testing.T) {
		t.Parallel()
		store, _, source := setup(t, 2)
		for round := 1; round <= 2; round++ {
			if got := source.Take(t.Context(), reviewRound(round)); len(got) != 0 {
				t.Fatalf("round %d was handed %+v by a take nothing settled", round, got)
			}
		}
		if read := readOn(t.Context(), store, "run-1", "n-1"); read == nil || read.Unanswered {
			t.Fatalf("the note is read %+v before the segment ends, want it read and unmarked", read)
		}
		source.Close(t.Context())
		eventually(t, "the take still lost at the segment's end marked unanswered", func() bool {
			read := readOn(t.Context(), store, "run-1", "n-1")
			return read != nil && read.Unanswered
		})
	})
	t.Run("a lost take standing for a note for its patience", func(t *testing.T) {
		t.Parallel()
		_, runs, source := setup(t, 1000)
		clock := time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)
		runs.now = func() time.Time { return clock }
		if got := source.Take(t.Context(), reviewRound(1)); len(got) != 0 {
			t.Fatalf("the round was handed %+v by a take nothing settled", got)
		}
		viewCaughtUp(t, source)
		clock = clock.Add(steerSettlePatience - time.Second)
		if !source.Waiting() {
			t.Error("a take lost less than its patience ago does not stand for a note waiting")
		}
		clock = clock.Add(2 * time.Second)
		if source.Waiting() {
			t.Error("a take lost past its patience still stands for a note waiting: a silent store would " +
				"hold the review's rounds open for as long as it is down")
		}
	})
}

// A RELAUNCH'S BRIEF TELLS A NOTE A HOOK'S SERVE PLACED AT THAT PLACE. A note
// whose first serve to a hook fixed its place (sandbox.RunNote.Place), its
// hook's answer lost, waits while a later correction is read at a later place;
// the relaunch's brief peeks it, the launch's write reads it at its own place,
// and the brief hands that place (steer.Briefing.Orders) so the runner tells
// it ahead of the correction. Every peeked note a serve placed, at its place
// and not at its number (sandbox.RunNote.Seq); a note no hook was served is
// placed nowhere (the package doc's "Every read is on the record, so the
// record is the carry").
//
// Mutation: hand the brief without a peeked note's place, place one at its
// number, place only the first, or place an unserved note, and it goes red.
func TestARelaunchsBriefTellsANoteAHooksServePlacedAtThatPlace(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store := sandbox.NewCoordStore(coordmem.NewFleet())
	runs, _ := steersOver(t, steerRing, store)
	launch := workingRun(t, store, runs, "run-1")
	serve := func(take string, ids ...string) {
		t.Helper()
		if _, err := store.ServeHook(ctx, "run-1", sandbox.HookServe{IDs: ids, Launch: launch,
			Statuses: steerTakeStatuses, Serve: sandbox.HookServed{Take: take,
				Boundary: string(types.SteerBoundaryToolBatch), Iteration: 1}}); err != nil {
			t.Fatal(err)
		}
	}
	fileSealed(t, runs, "run-1", steer.Note{ID: "n-0", Text: "run the migrations first", By: "founder"})
	fileSealed(t, runs, "run-1", steer.Note{ID: "n-a", Text: "deploy to prod", By: "founder"})
	serve("take-1", "n-0", "n-a")
	fileSealed(t, runs, "run-1", steer.Note{ID: "n-b", Text: "no, deploy to staging", By: "founder"})
	serve("take-2", "n-b")
	if _, err := store.SettleHook(ctx, "run-1", sandbox.HookSettle{Launch: launch,
		Held: []string{"take-2"}}); err != nil {
		t.Fatal(err)
	}
	fileSealed(t, runs, "run-1", steer.Note{ID: "n-c", Text: "and tag the release", By: "founder"})
	for _, n := range mustRow(t, store, "run-1").Steer.Notes {
		if n.ID == "n-a" && (n.Place != 1 || n.Seq == n.Place) {
			t.Fatalf("the premise: note n-a is numbered %d among the turn's notes and placed %d among its reads, "+
				"want it placed 1 under another number", n.Seq, n.Place)
		}
	}
	source := claimedSource(t, store, runs, launch)
	brief := source.Brief(ctx)
	var peeked []string
	for _, n := range brief.Notes {
		peeked = append(peeked, n.ID)
	}
	if !slices.Equal(peeked, []string{"n-0", "n-a", "n-c"}) {
		t.Fatalf("the brief peeks %v, want the two notes a hook's serve placed and the one no hook was served",
			peeked)
	}
	sealed, err := runs.briefNotes("run-1", 2, brief.Notes, source)
	if err != nil {
		t.Fatal(err)
	}
	claimed := mustRow(t, store, "run-1")
	relaunch := claimed
	relaunch.BriefNotes = sealed
	if _, err := store.BeginLaunch(ctx, relaunch, sandbox.Fence{Owner: claimed.Owner,
		Epoch: claimed.OwnerEpoch}); err != nil {
		t.Fatalf("BeginLaunch: %v", err)
	}
	places := map[string]int{}
	for _, read := range runs.carried(mustRow(t, store, "run-1")) {
		places[read.ID] = read.Order
	}
	if places["n-0"] != 1 || places["n-a"] != 1 || places["n-b"] != 2 || places["n-c"] != 3 {
		t.Fatalf("the premise: the launch's write reads the notes at %v, want n-0 and n-a at their first serve's "+
			"place 1, ahead of the correction n-b at 2, and n-c at the write's own place 3", places)
	}
	for _, id := range []string{"n-0", "n-a"} {
		if got, ok := brief.Orders[id]; !ok || got != places[id] {
			t.Errorf("the brief places note %s, which a hook's serve placed, at %d (placed %v), want %d, where the "+
				"launch's write reads it, ahead of the correction at %d", id, got, ok, places[id], places["n-b"])
		}
	}
	if got, ok := brief.Orders["n-c"]; ok {
		t.Errorf("the brief places the note no hook was served at %d, want it unplaced: the launch's write reads "+
			"it after every read", got)
	}
}

// A REVIEW'S TAKE STILL LOST AT ITS SEGMENT'S END IS MARKED BEFORE THE SEGMENT
// RETURNS, so the run's ending reports its note missed. The review's round took
// a note, the take's answer was lost and its settle went unanswered too, and
// the review concluded without it. A segment that ends the turn is followed at
// once by the run's ending, which reports what no step reported reading and
// reads a lost take's unmarked note as one its step reported: marked off the
// segment's path, the mark could land after that read and no node reported the
// note. So the segment's end marks it in its own path, where the store answers.
// The case's node settles nothing off a path, its teardown having waited its
// settles out, so what is marked at return is what the segment's path marked.
//
// Mutation: hand every take still lost off the path at the segment's end: red.
func TestAReviewsTakeLostAtItsSegmentsEndIsMarkedBeforeItReturns(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store := sandbox.NewCoordStore(coordmem.NewFleet())
	runs, p := steersOver(t, steerRing, &lostAnswerStore{PendingStore: store, failSettles: 1})
	launch := workingRun(t, store, runs, "run-1")
	fileSealed(t, runs, "run-1", steer.Note{ID: "n-1", Text: "use staging", By: "founder"})
	source := claimedSource(t, store, runs, launch)
	if got := source.Take(ctx, reviewRound(1)); len(got) != 0 {
		t.Fatalf("a take whose answer and settle were lost handed its round %v", got)
	}
	if read := readOn(ctx, store, "run-1", "n-1"); read == nil || read.Unanswered {
		t.Fatalf("the lost take's note is read %+v, want it read by the take and unmarked", read)
	}
	runs.waitSettles()
	source.Close(ctx)
	if read := readOn(ctx, store, "run-1", "n-1"); read == nil || !read.Unanswered {
		t.Fatalf("the note is read %+v as its segment returns, want it marked unanswered", read)
	}
	row := mustRow(t, store, "run-1")
	row.Ending = &sandbox.RecordedEnding{ID: "end-1", At: time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)}
	if err := runs.ended(ctx, row); err != nil {
		t.Fatalf("ended: %v", err)
	}
	if got := steered(p); len(got) != 1 || got[0].NoteID != "n-1" || got[0].Outcome != types.SteerMissed ||
		got[0].Phase != types.PhaseReview || got[0].Round != 1 || got[0].Boundary != types.SteerBoundaryRound {
		t.Errorf("the ending reported %+v, want the note missed at the review's first round", got)
	}
}

// gatedSettles is a run store whose settles the store does not answer while
// unavailable says so, and which wait at gate while one is set: counting the
// attempts, and the most it held at once. Nil unavailable answers every settle;
// answering, where set, is called as a settle is about to be answered.
//
// UNAVAILABLE IS THE CASE'S OWN LITERAL, written inside its bubble, because
// whether the store answers yet is a decision on the clock, and only a bubble's
// is the case's to advance: SettleTake is the double's half of the store's
// contract, run inside the bubble by every case here but held by none
// lexically, so a reading written in it would sit outside every bubble — and as
// the method sandbox.PendingStore names, it can be neither unexported nor
// declared bubble-only (go doc ./internal/fixedwait).
type gatedSettles struct {
	sandbox.PendingStore
	unavailable func() bool
	answering   func()
	gate        chan struct{}

	mu                    sync.Mutex
	asked, inFlight, most int
	hooks                 []sandbox.HookSettle
}

func (s *gatedSettles) SettleTake(ctx context.Context, turnID string, settle sandbox.NoteSettle) (
	[]sandbox.RunNote, error) {
	if err := s.ask(ctx); err != nil {
		return nil, err
	}
	return s.PendingStore.SettleTake(ctx, turnID, settle)
}

func (s *gatedSettles) SettleHook(ctx context.Context, turnID string, settle sandbox.HookSettle) (
	[]sandbox.RunNote, error) {
	s.mu.Lock()
	s.hooks = append(s.hooks, settle)
	s.mu.Unlock()
	if err := s.ask(ctx); err != nil {
		return nil, err
	}
	return s.PendingStore.SettleHook(ctx, turnID, settle)
}

// ask is one settle asked of the store: counted, held at the gate, and failed
// where the store is unavailable.
func (s *gatedSettles) ask(ctx context.Context) error {
	s.mu.Lock()
	s.asked++
	s.inFlight++
	s.most = max(s.most, s.inFlight)
	gate := s.gate
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.inFlight--
		s.mu.Unlock()
	}()
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	// A CONTEXT THAT HAS ENDED IS ANSWERED ITS ERROR, as a store over a
	// connection answers it.
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.unavailable != nil && s.unavailable() {
		return coord.ErrUnavailable
	}
	if s.answering != nil {
		s.answering()
	}
	return nil
}

// counts is how many settles were asked, and the most held at once.
func (s *gatedSettles) counts() (asked, most int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.asked, s.most
}

// lostTakes is a working run-1 whose notes a review's takes recorded and whose
// answers were lost, one take a note: each named after its note, for a settle
// to find. It answers the run's job.
func lostTakes(t *testing.T, store sandbox.PendingStore, runs *runSteers, ids ...string) string {
	t.Helper()
	launch := workingRun(t, store, runs, "run-1")
	for _, id := range ids {
		fileSealed(t, runs, "run-1", steer.Note{ID: id, Text: "the note " + id, By: "founder"})
		if _, err := store.TakeNotes(t.Context(), "run-1", sandbox.NoteTake{IDs: []string{id}, Launch: launch,
			Statuses: steerTakeStatuses, Read: sandbox.NoteRead{Phase: "review", Round: 1,
				Boundary: string(types.SteerBoundaryRound), Take: "take-" + id}}); err != nil {
			t.Fatal(err)
		}
	}
	return launch
}

// settleKind is one kind of settle a run's loop holds, for the stop window's
// cases to run over each: a review's take ([runSteers.settleUnanswered]), and
// a job's hook settle of a serve answered nothing or of a cancelled call
// ([runSteers.settleHook]), whose own last-round mark is its loop's other half.
type settleKind struct {
	name string
	// lose records a loss of each of ids on run-1 and answers its job.
	lose func(t *testing.T, store sandbox.PendingStore, runs *runSteers, ids ...string) string
	// settle hands run-1's loop the loss of note id.
	settle func(ctx context.Context, runs *runSteers, launch, id string)
	// settled reports whether the loss of note id is settled on the record.
	settled func(t *testing.T, store sandbox.PendingStore, id string) bool
}

// settleKinds is every kind of settle a run's loop holds.
func settleKinds() []settleKind {
	hook := func(name string, settle func(launch, id string) sandbox.HookSettle,
		settled func(sandbox.RunNote) bool) settleKind {
		return settleKind{name: name, lose: servedHooks,
			settle: func(ctx context.Context, runs *runSteers, launch, id string) {
				runs.settleHook(ctx, "run-1", settle(launch, id))
			},
			settled: func(t *testing.T, store sandbox.PendingStore, id string) bool {
				return settled(noteOn(t, store, "run-1", id))
			}}
	}
	return []settleKind{
		{name: "a review's take", lose: lostTakes,
			settle: func(ctx context.Context, runs *runSteers, _, id string) {
				runs.settleUnanswered(ctx, "run-1", "take-"+id)
			},
			settled: func(t *testing.T, store sandbox.PendingStore, id string) bool {
				read := readOn(t.Context(), store, "run-1", id)
				return read != nil && read.Unanswered
			}},
		hook("a hook's serve answered nothing", func(launch, id string) sandbox.HookSettle {
			return sandbox.HookSettle{Launch: launch, Unanswered: []string{"take-" + id}}
		}, func(n sandbox.RunNote) bool { return n.Waiting() && len(n.Serves) == 0 }),
		hook("a hook's call cancelled", func(launch, id string) sandbox.HookSettle {
			return sandbox.HookSettle{Launch: launch, Cancelled: []sandbox.NoteCall{servedCall(id)}}
		}, func(n sandbox.RunNote) bool {
			return slices.ContainsFunc(n.Serves, func(h sandbox.HookServed) bool {
				return h.Lost == sandbox.ServeCancelled
			})
		}),
	}
}

// servedCall is the call a hook served note id by in [servedHooks].
func servedCall(id string) sandbox.NoteCall {
	return sandbox.NoteCall{Session: "mcp-session-1", ID: `"` + id + `"`}
}

// servedHooks is a working run-1 whose notes a hook's takes served to its job,
// one take a note, each named after its note and answering the note's call
// ([servedCall]). It answers the run's job.
func servedHooks(t *testing.T, store sandbox.PendingStore, runs *runSteers, ids ...string) string {
	t.Helper()
	launch := workingRun(t, store, runs, "run-1")
	for _, id := range ids {
		fileSealed(t, runs, "run-1", steer.Note{ID: id, Text: "the note " + id, By: "founder"})
		call := servedCall(id)
		if _, err := store.ServeHook(t.Context(), "run-1", sandbox.HookServe{IDs: []string{id}, Launch: launch,
			Statuses: steerTakeStatuses, Serve: sandbox.HookServed{Take: "take-" + id,
				Boundary: string(types.SteerBoundaryToolBatch), Call: &call}}); err != nil {
			t.Fatal(err)
		}
	}
	return launch
}

// A SETTLE WINDOW COUNTS FROM THE INSTANT THE TEARDOWN BEGAN, which its
// signal's cause names, never from when the callback arming it ran: on a
// loaded node that callback runs late, and the settles would hold the stop
// that much past their window. Here the teardown began a whole window ago, so
// the window is over the moment it is armed.
//
// Mutation: arm the window's timer for d from when the callback runs.
func TestASettleWindowCountsFromTheTeardownsStart(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		begun, begin := context.WithCancelCause(context.Background())
		window := windowFrom(begun, steerSettleStopBudget)
		begin(teardownBegan{at: time.Now().Add(-steerSettleStopBudget)})
		synctest.Wait()
		if window.Err() == nil {
			t.Fatalf("a settle window whose teardown began %v ago is still open: it counted its %v "+
				"from when its callback ran", steerSettleStopBudget, steerSettleStopBudget)
		}
	})
}

// A SETTLE ASKS UNTIL ITS STORE ANSWERS, AND NO LONGER THAN ITS NODE. A settle a
// clock gave up on would leave the record saying the run read notes its reader
// was never handed, so it asks until the store answers, a minute's outage
// included. A run's lost takes settle in one loop, never two at once: a take
// lost while a round is asked joins it. It stops with its node, after asking
// every take once more in a window the teardown opens (not the stop's
// allowance, owed to the later give-backs), and asks nothing once the teardown
// has waited the settles out.
//
// Mutation: give up at the patience, end with the losing take's context, ask
// past the stop, loop per take, sleep through the stop, make no last attempt
// or two (a hook settle's loop never ends), or use the stop's allowance: red.
func TestASettleAsksUntilItsStoreAnswersAndNoLongerThanItsNode(t *testing.T) {
	t.Parallel()
	t.Run("its store answering a minute after", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			store := sandbox.NewCoordStore(coordmem.NewFleet())
			answerAt := time.Now().Add(time.Minute)
			settles := &gatedSettles{PendingStore: store,
				unavailable: func() bool { return time.Now().Before(answerAt) }}
			runs, _ := steersOver(t, steerRing, settles)
			runs.stopped = t.Context().Done()
			lostTakes(t, store, runs, "n-1")
			took, ended := context.WithCancel(t.Context())
			runs.settleUnanswered(took, "run-1", "take-n-1")
			ended()
			time.Sleep(time.Minute + 30*time.Second)
			synctest.Wait()
			if read := readOn(t.Context(), store, "run-1", "n-1"); read == nil || !read.Unanswered {
				t.Errorf("the note is read %+v once the store answered, a minute in: the settle gave up", read)
			}
		})
	})
	for _, kind := range settleKinds() {
		t.Run("its node stopping first, "+kind.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				store := sandbox.NewCoordStore(coordmem.NewFleet())
				settles := &gatedSettles{PendingStore: store, unavailable: func() bool { return true }}
				runs, _ := steersOver(t, steerRing, settles)
				e := &Engine{}
				runs.stopWith(e)
				launch := kind.lose(t, store, runs, "n-1")
				kind.settle(t.Context(), runs, launch, "n-1")
				time.Sleep(20 * time.Second)
				synctest.Wait()
				before, _ := settles.counts()
				e.tearingDown()
				runs.waitSettles()
				if asked, _ := settles.counts(); asked != before+1 {
					t.Errorf("the settle was asked %d times as its node stopped, want once more", asked-before)
				}
				time.Sleep(time.Minute)
				synctest.Wait()
				if asked, _ := settles.counts(); asked != before+1 {
					t.Errorf("the settle was asked %d times after its last attempt", asked-before-1)
				}
				if kind.settled(t, store, "n-1") {
					t.Error("the note's loss is settled, want it as it was: nothing settled it")
				}
			})
		})
		t.Run("its store back as its node stops, a settle joining its last round, "+kind.name,
			func(t *testing.T) {
				t.Parallel()
				synctest.Test(t, func(t *testing.T) {
					store := sandbox.NewCoordStore(coordmem.NewFleet())
					var back, joined atomic.Bool
					var runs *runSteers
					var launch string
					settles := &gatedSettles{PendingStore: store,
						unavailable: func() bool { return !back.Load() },
						// A SEGMENT OR AN INBOX THE TEARDOWN ENDED, handing off
						// a settle of its own while the loop's last round is
						// asked.
						answering: func() {
							if joined.CompareAndSwap(false, true) {
								kind.settle(t.Context(), runs, launch, "n-2")
							}
						}}
					runs, _ = steersOver(t, steerRing, settles)
					e := &Engine{}
					runs.stopWith(e)
					launch = kind.lose(t, store, runs, "n-1", "n-2")
					kind.settle(t.Context(), runs, launch, "n-1")
					time.Sleep(10 * time.Second)
					synctest.Wait()
					// THE STORE IS BACK WHILE THE LOOP SLEEPS BETWEEN ROUNDS,
					// and the node stops before the loop's next round.
					back.Store(true)
					e.tearingDown()
					synctest.Wait()
					for _, id := range []string{"n-1", "n-2"} {
						if !kind.settled(t, store, id) {
							t.Errorf("note %s is unsettled as its node stops, want it settled by the "+
								"settle's last attempt", id)
						}
					}
					runs.waitSettles()
				})
			})
		t.Run("a settle handed off once its node stopped, "+kind.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				store := sandbox.NewCoordStore(coordmem.NewFleet())
				settles := &gatedSettles{PendingStore: store}
				runs, _ := steersOver(t, steerRing, settles)
				e := &Engine{}
				runs.stopWith(e)
				launch := kind.lose(t, store, runs, "n-1")
				e.tearingDown()
				kind.settle(t.Context(), runs, launch, "n-1")
				runs.waitSettles()
				if asked, _ := settles.counts(); asked != 1 {
					t.Errorf("a settle handed off once its node stopped was asked %d times, want once", asked)
				}
				if !kind.settled(t, store, "n-1") {
					t.Error("the note's loss is unsettled, want it settled by the settle's one attempt")
				}
			})
		})
		t.Run("a settle asked once its node waited its settles out, "+kind.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				store := sandbox.NewCoordStore(coordmem.NewFleet())
				settles := &gatedSettles{PendingStore: store}
				runs, _ := steersOver(t, steerRing, settles)
				e := &Engine{}
				runs.stopWith(e)
				launch := kind.lose(t, store, runs, "n-1")
				e.tearingDown()
				runs.waitSettles()
				kind.settle(t.Context(), runs, launch, "n-1")
				synctest.Wait()
				if asked, _ := settles.counts(); asked != 0 {
					t.Errorf("a settle asked once its node waited its settles out was asked %d times, want none",
						asked)
				}
			})
		})
	}
	t.Run("its last attempts in a window of their own, beside the stop's allowance", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			store := sandbox.NewCoordStore(coordmem.NewFleet())
			// A STORE THAT NEVER ANSWERS: every attempt waits until its
			// context ends.
			settles := &gatedSettles{PendingStore: store, gate: make(chan struct{})}
			runs, _ := steersOver(t, steerRing, settles)
			// THE SHIPPED TTL, of an Engine literal as the stop's other cases
			// build: its allowance is longer than the settles' window, so last
			// attempts made on the allowance would hold the stop past the
			// window, and leave the give-backs after them less than the
			// allowance, or none.
			e := &Engine{}
			allowance := seat.StopAllowance(e.stopTTL()) - e.admissionShare()
			if allowance <= steerSettleStopBudget {
				t.Fatalf("the premise: the stop's allowance of %v is no longer than the settles' window of %v",
					allowance, steerSettleStopBudget)
			}
			runs.stopWith(e)
			// SEVERAL RUNS AND TAKES, each loop's first attempt in flight as
			// the teardown begins, and a take joining one of them then.
			lostTakes(t, store, runs, "n-1", "n-2")
			runs.settleUnanswered(t.Context(), "run-1", "take-n-1")
			runs.settleUnanswered(t.Context(), "run-1", "take-n-2")
			runs.settleUnanswered(t.Context(), "run-2", "take-x")
			synctest.Wait()
			begun := time.Now()
			e.tearingDown()
			runs.settleUnanswered(t.Context(), "run-1", "take-joined")
			runs.waitSettles()
			if took := time.Since(begun); took > steerSettleStopBudget {
				t.Errorf("the settles held their node's stop %v, past their window of %v", took,
					steerSettleStopBudget)
			}
			// AND A GIVE-BACK MADE AFTER THEM IS HANDED THE WHOLE ALLOWANCE.
			step, done := seat.StopStep(seat.WithStopBudget(context.Background(), e.stopping()))
			defer done()
			if deadline, ok := step.Deadline(); !ok || time.Until(deadline) != allowance {
				t.Errorf("a give-back after the settles is handed %v, want the whole allowance of %v: the "+
					"settles spent the rest", time.Until(deadline), allowance)
			}
		})
	})
	t.Run("a take joining the round the store first answers, past the patience", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			store := sandbox.NewCoordStore(coordmem.NewFleet())
			answerAt := time.Now().Add(steerSettlePatience)
			var runs *runSteers
			var joined atomic.Bool
			settles := &gatedSettles{PendingStore: store,
				unavailable: func() bool { return time.Now().Before(answerAt) },
				// ANOTHER TAKE LOST ON THE SAME OUTAGE, failing as the
				// store comes back: it joins the run's loop while the loop's
				// first answered round is asked.
				answering: func() {
					if joined.CompareAndSwap(false, true) {
						runs.settleUnanswered(t.Context(), "run-1", "take-n-2")
					}
				}}
			runs, _ = steersOver(t, steerRing, settles)
			runs.stopped = t.Context().Done()
			lostTakes(t, store, runs, "n-1", "n-2")
			runs.settleUnanswered(t.Context(), "run-1", "take-n-1")
			time.Sleep(2 * time.Minute)
			synctest.Wait()
			for _, id := range []string{"n-1", "n-2"} {
				if read := readOn(t.Context(), store, "run-1", id); read == nil || !read.Unanswered {
					t.Errorf("note %s is read %+v, want it marked once the store answered", id, read)
				}
			}
		})
	})
	t.Run("a job's hook settles, merged into one ask a round", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			store := sandbox.NewCoordStore(coordmem.NewFleet())
			gate := make(chan struct{})
			settles := &gatedSettles{PendingStore: store, gate: gate}
			runs, _ := steersOver(t, steerRing, settles)
			runs.stopped = t.Context().Done()
			launch := workingRun(t, store, runs, "run-1")
			for _, id := range []string{"n-1", "n-2", "n-3"} {
				fileSealed(t, runs, "run-1", steer.Note{ID: id, Text: "the note " + id, By: "founder"})
				if _, err := store.ServeHook(t.Context(), "run-1", sandbox.HookServe{IDs: []string{id},
					Launch: launch, Statuses: steerTakeStatuses, Serve: sandbox.HookServed{Take: "take-" + id,
						Boundary: string(types.SteerBoundaryToolBatch)}}); err != nil {
					t.Fatal(err)
				}
			}
			// THE FIRST HELD IN ITS ASK, the two after it joining the loop.
			runs.settleHook(t.Context(), "run-1", sandbox.HookSettle{Launch: launch,
				Unanswered: []string{"take-n-1"}})
			synctest.Wait()
			for _, id := range []string{"n-2", "n-3"} {
				runs.settleHook(t.Context(), "run-1", sandbox.HookSettle{Launch: launch,
					Unanswered: []string{"take-" + id}})
			}
			close(gate)
			time.Sleep(time.Minute)
			synctest.Wait()
			if _, most := settles.counts(); most != 1 {
				t.Errorf("%d settles of one run were asked at once, want one loop", most)
			}
			settles.mu.Lock()
			asked := slices.Clone(settles.hooks)
			settles.mu.Unlock()
			if len(asked) != 2 || !slices.Equal(asked[0].Unanswered, []string{"take-n-1"}) ||
				!slices.Equal(asked[1].Unanswered, []string{"take-n-2", "take-n-3"}) {
				t.Errorf("the store was asked %+v, want the first settle, then the two that joined it merged "+
					"into one ask", asked)
			}
			run := mustRow(t, store, "run-1")
			for _, n := range run.Steer.Notes {
				if n.Served != "" || len(n.Serves) != 0 || !n.Waiting() {
					t.Errorf("note %s holds %+v, want its serve removed and the note waiting", n.ID, n)
				}
			}
		})
	})
	t.Run("two takes of one run", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			store := sandbox.NewCoordStore(coordmem.NewFleet())
			gate := make(chan struct{})
			settles := &gatedSettles{PendingStore: store, gate: gate}
			runs, _ := steersOver(t, steerRing, settles)
			runs.stopped = t.Context().Done()
			lostTakes(t, store, runs, "n-1", "n-2")
			runs.settleUnanswered(t.Context(), "run-1", "take-n-1")
			runs.settleUnanswered(t.Context(), "run-1", "take-n-2")
			synctest.Wait()
			if _, most := settles.counts(); most != 1 {
				t.Errorf("%d settles of one run were asked at once, want its takes in one loop", most)
			}
			close(gate)
			time.Sleep(time.Minute)
			synctest.Wait()
			for _, id := range []string{"n-1", "n-2"} {
				if read := readOn(t.Context(), store, "run-1", id); read == nil || !read.Unanswered {
					t.Errorf("note %s is read %+v, want it marked by the run's one loop", id, read)
				}
			}
		})
	})
}

// unservingStore is a run store whose serves of a poll's notes fail.
type unservingStore struct{ sandbox.PendingStore }

func (unservingStore) ServeNotes(context.Context, string, sandbox.NoteServe) ([]sandbox.RunNote, error) {
	return nil, context.DeadlineExceeded
}

// A POLLED NOTE IS SERVED ON ITS RECORD BEFORE IT IS ANSWERED. A harness that
// polls for its notes (OpenCode's plugin) delivers what it is answered and
// confirms it only once it has, so a note answered with nothing on the record
// could sit in a session the record says never read it. The poll answers a note
// only once its record says it was served to the run's job: still waiting, its
// read the confirmation's alone, the mark kept once read and carried, by id, to
// the resume. A poll whose serve the store does not answer answers nothing from
// the note it could not mark, though a later note was marked by an earlier
// poll: the cursor moves past every note answered and would never return.
//
// Mutation: answer without the serve, answer past an unmarked note, drop the
// mark once read, or carry none to the resume: red.
func TestAPolledNoteIsServedOnItsRecordBeforeItIsAnswered(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store := sandbox.NewCoordStore(coordmem.NewFleet())
	runs, _ := steersOver(t, steerRing, store)
	launch := workingRun(t, store, runs, "run-1")
	fileSealed(t, runs, "run-1", steer.Note{ID: "n-1", Text: "use staging", By: "founder"})
	fileSealed(t, runs, "run-1", steer.Note{ID: "n-2", Text: "and tag the release", By: "founder"})
	// THE LATER NOTE SERVED ALREADY, by an earlier poll.
	if served, err := store.ServeNotes(ctx, "run-1", sandbox.NoteServe{IDs: []string{"n-2"}, Launch: launch,
		Statuses: steerTakeStatuses}); err != nil || len(served) != 1 {
		t.Fatalf("serving n-2 = %+v, %v", served, err)
	}

	// THE STORE NOT ANSWERING THE SERVE: nothing is answered, n-2 included.
	unserved, _ := steersOver(t, steerRing, unservingStore{store})
	refused := boundInbox(t, unserved, store, "run-1", launch)
	viewHolds(t, refused, 2)
	if notes, _ := refused.Wait(ctx, 0); len(notes) != 0 {
		t.Fatalf("a poll whose serve failed was answered %+v, want nothing", notes)
	}
	if row, _, _ := store.Get(ctx, "run-1"); row.Steer.Notes[0].Served != "" || !row.Steer.Notes[0].Waiting() {
		t.Fatalf("a serve that failed left n-1 %+v, want it waiting and unmarked", row.Steer.Notes[0])
	}

	inbox := boundInbox(t, runs, store, "run-1", launch)
	viewHolds(t, inbox, 2)
	notes, _ := inbox.Wait(ctx, 0)
	if len(notes) != 2 || notes[0].ID != "n-1" || notes[1].ID != "n-2" {
		t.Fatalf("the poll was answered %+v, want both notes, in order", notes)
	}
	row, _, err := store.Get(ctx, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	if n := row.Steer.Notes[0]; n.Served != launch || !n.Waiting() {
		t.Fatalf("the note answered is %+v on its record, want it waiting, served to job %s", n, launch)
	}
	if got := runnerResume(resumeInput{Run: row}, nil, nil, nil).Served; !slices.Equal(got, []string{"n-1", "n-2"}) {
		t.Errorf("the resume carries the served notes %v, want n-1 and n-2", got)
	}
	// CONFIRMED: read by the job, still served.
	if err := inbox.Read(ctx, []string{"n-1"}); err != nil {
		t.Fatalf("Read: %v", err)
	}
	if row, _, _ := store.Get(ctx, "run-1"); row.Steer.Notes[0].Waiting() ||
		row.Steer.Notes[0].Served != launch {
		t.Errorf("the confirmed note is %+v, want it read and still served", row.Steer.Notes[0])
	}
}

// A CONFIRMATION WHOSE ANSWER WAS LOST REPORTS ITS NOTE, ONCE. OpenCode's plugin
// confirms a note once OpenCode delivered it, and the confirmation's take
// reports the note read: a take whose write landed and answer was lost
// reported nothing, the plugin's repeat found the note read and skipped it, and
// the run's ending reported nothing either. So the take is settled on the
// confirmation's own path and what it recorded is reported there; where the
// store answers that settle no more than the take, the confirmation answers its
// error and the repeat reports the note. A repeat after a confirmation that
// reported does so under the same id and instant: one record.
//
// Mutation: answer a lost take's confirmation unsettled, skip a note an earlier
// confirmation read, or report it again under another id or instant: red.
func TestAConfirmationWhoseAnswerWasLostReportsItsNoteOnce(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		store func(sandbox.PendingStore) sandbox.PendingStore
		// answered is whether the first confirmation is answered done, again
		// whether the plugin confirms once more, and reports how many times the
		// note is published delivered.
		answered, again bool
		reports         int
	}{
		"its take lost, its settle answered": {
			store:    func(s sandbox.PendingStore) sandbox.PendingStore { return &lostAnswerStore{PendingStore: s} },
			answered: true, reports: 1},
		"its take and its settle lost, confirmed again": {
			store: func(s sandbox.PendingStore) sandbox.PendingStore {
				return &lostAnswerStore{PendingStore: s, failSettles: 1}
			},
			again: true, reports: 1},
		"its answer lost after its report, confirmed again": {
			store:    func(s sandbox.PendingStore) sandbox.PendingStore { return s },
			answered: true, again: true, reports: 2},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			store := sandbox.NewCoordStore(coordmem.NewFleet())
			runs, p := steersOver(t, steerRing, tc.store(store))
			launch := workingRun(t, store, runs, "run-1")
			fileSealed(t, runs, "run-1", steer.Note{ID: "n-1", Text: "use staging", By: "founder"})
			inbox := boundInbox(t, runs, store, "run-1", launch)
			viewHolds(t, inbox, 1)
			if notes, _ := inbox.Wait(ctx, 0); len(notes) != 1 {
				t.Fatalf("the poll was answered %+v, want n-1", notes)
			}
			if err := inbox.Read(ctx, []string{"n-1"}); (err == nil) != tc.answered {
				t.Fatalf("the confirmation answered %v, want it answered done: %v", err, tc.answered)
			}
			if read := readOn(ctx, store, "run-1", "n-1"); read == nil || read.Launch != launch ||
				read.Boundary != string(types.SteerBoundaryStep) {
				t.Fatalf("the note is read %+v, want it read at job %s's step", read, launch)
			}
			if tc.again {
				viewHolds(t, inbox, 0)
				if err := inbox.Read(ctx, []string{"n-1"}); err != nil {
					t.Fatalf("the confirmation made again answered %v", err)
				}
			}
			eventually(t, "the confirmed note's reports", func() bool { return len(steered(p)) >= tc.reports })
			// THE RUN'S ENDING, which reports nothing of a note a step read.
			row := mustRow(t, store, "run-1")
			row.Ending = &sandbox.RecordedEnding{ID: "end-1", At: time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)}
			if err := runs.ended(ctx, row); err != nil {
				t.Fatalf("ended: %v", err)
			}
			p.mu.Lock()
			published := slices.Clone(p.events)
			p.mu.Unlock()
			if len(published) != tc.reports {
				t.Fatalf("the note was published %d times, want %d", len(published), tc.reports)
			}
			for _, ev := range published {
				rec, _ := events.DataAs[*types.AgentTurnSteered](ev)
				if rec == nil || rec.NoteID != "n-1" || rec.Outcome != types.SteerDelivered ||
					rec.Boundary != types.SteerBoundaryStep || rec.LaunchID != launch {
					t.Errorf("the note is reported %+v, want it delivered at job %s's step", rec, launch)
				}
				if ev.ID != published[0].ID || !ev.Timestamp.Equal(published[0].Timestamp) {
					t.Errorf("the note is reported as %s at %v and as %s at %v, want one record", ev.ID,
						ev.Timestamp, published[0].ID, published[0].Timestamp)
				}
			}
		})
	}
}

// A CONFIRMATION AN INBOX SERVING NO JOB CANNOT TAKE SAYS SO, AND RECORDS
// NOTHING. The run's opening on the bridge can end as its notes route records
// a confirmation (the run ended, the seat left, or a recovery opened the job
// with an inbox of its own), closing the inbox the route found serving. Such an
// inbox answers mcpbridge.ErrNoteUntaken, which the bridge turns away to ask
// again, and the confirmation made again is recorded by the inbox that serves
// the job; an inbox not yet bound says so too
// (TestAnOpeningIsFiledOnlyOnceItsInboxIsBound). The package doc's "An OpenCode
// confirmation is recorded once, and reported" says why.
//
// Mutation: answer a read of an inbox that serves no job done (nil), and both
// of its reads go red.
func TestAConfirmationAnInboxServingNoJobCannotTakeSaysSo(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store := sandbox.NewCoordStore(coordmem.NewFleet())
	runs, p := steersOver(t, steerRing, store)
	launch := workingRun(t, store, runs, "run-1")
	fileSealed(t, runs, "run-1", steer.Note{ID: "n-1", Text: "use staging", By: "founder"})
	served := boundInbox(t, runs, store, "run-1", launch)
	viewHolds(t, served, 1)
	if notes, _ := served.Wait(ctx, 0); len(notes) != 1 {
		t.Fatalf("the poll was answered %+v, want n-1", notes)
	}

	// THE OPENING'S END CLOSED THE INBOX the route found serving.
	served.Close()
	if err := served.Read(ctx, []string{"n-1"}); !errors.Is(err, mcpbridge.ErrNoteUntaken) {
		t.Errorf("a confirmation reaching a closed inbox answered %v, want ErrNoteUntaken", err)
	}
	// AN INBOX NOT YET BOUND takes no read either.
	unbound := runs.inbox(identityOfRun(mustRow(t, store, "run-1")), 1, executor.ClaudeCodeName)
	t.Cleanup(unbound.Close)
	if err := unbound.Read(ctx, []string{"n-1"}); !errors.Is(err, mcpbridge.ErrNoteUntaken) {
		t.Errorf("a confirmation reaching an inbox not yet bound answered %v, want ErrNoteUntaken", err)
	}
	if n := mustRow(t, store, "run-1").Steer.Notes[0]; !n.Waiting() || n.Served != launch {
		t.Fatalf("a confirmation no inbox took left n-1 %+v, want it waiting, served to job %s", n, launch)
	}
	if got := steered(p); len(got) != 0 {
		t.Fatalf("a confirmation no inbox took reported %+v", got)
	}

	// THE CONFIRMATION MADE AGAIN reaches the inbox that serves the job now,
	// which records it.
	recovered := boundInbox(t, runs, store, "run-1", launch)
	viewHolds(t, recovered, 1)
	if err := recovered.Read(ctx, []string{"n-1"}); err != nil {
		t.Fatalf("the confirmation made again answered %v", err)
	}
	if read := readOn(ctx, store, "run-1", "n-1"); read == nil || read.Launch != launch ||
		read.Boundary != string(types.SteerBoundaryStep) {
		t.Errorf("the confirmation made again left n-1 read %+v, want it read at job %s's step", read, launch)
	}
}

// A CONFIRMATION ITS JOB'S RECORD NO LONGER TAKES SAYS SO, AND RECORDS NOTHING.
// A job's completion claims its run, moving the record out of the statuses its
// harness takes notes in while its session still serves: the inbox answers
// mcpbridge.ErrNoteUntaken for a note its take left waiting, and once the claim
// is handed back and the run goes on in its box, the confirmation made again
// is recorded. A note named that no longer waits, though the inbox's view still
// shows it, is answered done (the package doc's "An OpenCode confirmation is
// recorded once, and reported").
//
// Mutation: answer a confirmation whose take left a note waiting done, and the
// read after the claim goes red; turn away every confirmation that took fewer
// notes than it named, or any that left a note, and the last one goes red.
func TestAConfirmationItsJobsRecordNoLongerTakesSaysSo(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store := sandbox.NewCoordStore(coordmem.NewFleet())
	runs, p := steersOver(t, steerRing, store)
	launch := workingRun(t, store, runs, "run-1")
	fileSealed(t, runs, "run-1", steer.Note{ID: "n-1", Text: "use staging", By: "founder"})
	inbox := boundInbox(t, runs, store, "run-1", launch)
	viewHolds(t, inbox, 1)
	if notes, _ := inbox.Wait(ctx, 0); len(notes) != 1 {
		t.Fatalf("the poll was answered %+v, want n-1", notes)
	}
	// AN INBOX OF THE SAME JOB WHOSE VIEW STAYS AS IT IS NOW, the note waiting.
	staleRuns, _ := steersOver(t, steerRing, frozenWatch{PendingStore: store})
	stale := boundInbox(t, staleRuns, store, "run-1", launch)
	viewHolds(t, stale, 1)

	// THE JOB'S COMPLETION CLAIMS THE RUN, its session and its inbox still
	// serving.
	if _, ok, err := store.ClaimForResume(ctx, "run-1", sandbox.CompletionTail(launch),
		sandbox.Fence{}); err != nil || !ok {
		t.Fatalf("ClaimForResume = %v, %v", ok, err)
	}
	if err := inbox.Read(ctx, []string{"n-1"}); !errors.Is(err, mcpbridge.ErrNoteUntaken) {
		t.Errorf("a confirmation its job's record no longer takes answered %v, want ErrNoteUntaken", err)
	}
	if n := mustRow(t, store, "run-1").Steer.Notes[0]; !n.Waiting() || n.Served != launch {
		t.Fatalf("a confirmation the record did not take left n-1 %+v, want it waiting, served to job %s",
			n, launch)
	}
	if got := steered(p); len(got) != 0 {
		t.Fatalf("a confirmation the record did not take reported %+v", got)
	}

	// THE CLAIM IS HANDED BACK, and the run goes on in its box: the
	// confirmation made again is recorded.
	if ok, err := store.ReleaseClaim(ctx, "run-1",
		sandbox.Release{Launch: launch, To: sandbox.StatusRunning}); err != nil || !ok {
		t.Fatalf("ReleaseClaim = %v, %v", ok, err)
	}
	if err := inbox.Read(ctx, []string{"n-1"}); err != nil {
		t.Fatalf("the confirmation made again answered %v", err)
	}
	if read := readOn(ctx, store, "run-1", "n-1"); read == nil || read.Launch != launch ||
		read.Boundary != string(types.SteerBoundaryStep) {
		t.Fatalf("the confirmation made again left n-1 read %+v, want it read at job %s's step", read, launch)
	}
	// AND MADE ONCE MORE, by the inbox that still sees the note waiting: its
	// take leaves the note, which its job read already, and it is answered
	// done.
	if err := stale.Read(ctx, []string{"n-1"}); err != nil {
		t.Errorf("a confirmation of a note its job read already answered %v, want it done", err)
	}
}

// A CONFIRMATION IS JUDGED BY THE RECORD READ AFTER ITS TAKE, NEVER ONE READ
// BEFORE IT. OpenCode's plugin names every note it owes in one confirmation, so
// the one it makes again after an answer it lost names a note its job read
// already beside one OpenCode delivered since. The inbox's view then shows the
// second alone waiting, and the read asks the record before its take, which
// still holds the second waiting though the take that follows records it.
// Judged by that record, what the take left would be a note its job's record no
// longer takes, and the confirmation turned away: the plugin asked again for
// nothing, every time it confirmed a note again beside a new one.
//
// Mutation: judge what the take left off the record read before the take,
// where the read asked it one, and the confirmation is turned away.
func TestAConfirmationIsJudgedByTheRecordReadAfterItsTake(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store := sandbox.NewCoordStore(coordmem.NewFleet())
	runs, _ := steersOver(t, steerRing, store)
	launch := workingRun(t, store, runs, "run-1")
	fileSealed(t, runs, "run-1", steer.Note{ID: "n-1", Text: "use staging", By: "founder"})
	fileSealed(t, runs, "run-1", steer.Note{ID: "n-2", Text: "and keep the flag off", By: "founder"})
	inbox := boundInbox(t, runs, store, "run-1", launch)
	viewHolds(t, inbox, 2)
	if notes, _ := inbox.Wait(ctx, 0); len(notes) != 2 {
		t.Fatalf("the poll was answered %+v, want n-1 and n-2", notes)
	}
	// N-1 CONFIRMED, the answer lost on its way back to the plugin.
	if err := inbox.Read(ctx, []string{"n-1"}); err != nil {
		t.Fatalf("the confirmation of n-1 answered %v", err)
	}
	viewHolds(t, inbox, 1)

	// MADE AGAIN WITH N-2, which OpenCode delivered since: the view shows n-2
	// alone waiting, so the record is read before the take.
	if err := inbox.Read(ctx, []string{"n-1", "n-2"}); err != nil {
		t.Errorf("a confirmation whose take recorded every note of it still waiting answered %v, "+
			"want it done", err)
	}
	for _, id := range []string{"n-1", "n-2"} {
		if read := readOn(ctx, store, "run-1", id); read == nil || read.Launch != launch ||
			read.Boundary != string(types.SteerBoundaryStep) {
			t.Errorf("%s is read %+v, want it read at job %s's step", id, read, launch)
		}
	}
}

// A CONFIRMATION TURNED AWAY STILL REPORTS WHAT ITS JOB READ BEFORE. The
// confirmation made again after a lost answer names a note an earlier
// confirmation of its job read beside one it owes now, and reports that earlier
// read under the same id and instant (runInbox.confirmedBefore), even where it
// is turned away for the note still waiting (the completion claimed the run) or
// refused (the claim ended it): the run's ending reports nothing of a note a
// step read, so turned away first, that read would be reported by nobody.
//
// Mutation: turn the confirmation away before it reports what its job read
// before, and that report is never made.
func TestAConfirmationTurnedAwayStillReportsWhatItsJobReadBefore(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store := sandbox.NewCoordStore(coordmem.NewFleet())
	runs, p := steersOver(t, steerRing, store)
	launch := workingRun(t, store, runs, "run-1")
	fileSealed(t, runs, "run-1", steer.Note{ID: "n-1", Text: "use staging", By: "founder"})
	fileSealed(t, runs, "run-1", steer.Note{ID: "n-2", Text: "and keep the flag off", By: "founder"})
	inbox := boundInbox(t, runs, store, "run-1", launch)
	viewHolds(t, inbox, 2)
	if notes, _ := inbox.Wait(ctx, 0); len(notes) != 2 {
		t.Fatalf("the poll was answered %+v, want n-1 and n-2", notes)
	}
	// N-1 CONFIRMED AND REPORTED, the answer lost on its way back to the plugin.
	if err := inbox.Read(ctx, []string{"n-1"}); err != nil {
		t.Fatalf("the confirmation of n-1 answered %v", err)
	}
	eventually(t, "n-1's report", func() bool { return len(steered(p)) == 1 })

	// THE JOB'S COMPLETION CLAIMS THE RUN, and the plugin confirms n-1 again
	// beside n-2, which OpenCode delivered since.
	if _, ok, err := store.ClaimForResume(ctx, "run-1", sandbox.CompletionTail(launch),
		sandbox.Fence{}); err != nil || !ok {
		t.Fatalf("ClaimForResume = %v, %v", ok, err)
	}
	if err := inbox.Read(ctx, []string{"n-1", "n-2"}); !errors.Is(err, mcpbridge.ErrNoteUntaken) {
		t.Fatalf("a confirmation its job's record no longer takes answered %v, want ErrNoteUntaken", err)
	}
	// N-1 REPORTED AGAIN, as one record: the same id at the same instant.
	eventually(t, "n-1's report made again", func() bool { return len(steered(p)) == 2 })
	p.mu.Lock()
	published := slices.Clone(p.events)
	p.mu.Unlock()
	for _, ev := range published {
		rec, _ := events.DataAs[*types.AgentTurnSteered](ev)
		if rec == nil || rec.NoteID != "n-1" || rec.Outcome != types.SteerDelivered ||
			rec.Boundary != types.SteerBoundaryStep || rec.LaunchID != launch {
			t.Errorf("a confirmation reported %+v, want n-1 delivered at job %s's step", rec, launch)
		}
		if ev.ID != published[0].ID || !ev.Timestamp.Equal(published[0].Timestamp) {
			t.Errorf("n-1 is reported as %s at %v and as %s at %v, want one record", ev.ID, ev.Timestamp,
				published[0].ID, published[0].Timestamp)
		}
	}
	if read := readOn(ctx, store, "run-1", "n-2"); read != nil {
		t.Errorf("the confirmation turned away left n-2 read %+v, want it waiting", read)
	}
}

// frozenWatch is a run store whose watch of a run's notes hands its follower
// the record as it stands when the watch opens, and nothing after: a view that
// stays behind the record.
type frozenWatch struct{ sandbox.PendingStore }

func (s frozenWatch) WatchNotes(ctx context.Context, turnID string) (<-chan sandbox.NotesView, error) {
	views, err := s.PendingStore.WatchNotes(ctx, turnID)
	if err != nil {
		return nil, err
	}
	first := make(chan sandbox.NotesView, 1)
	go func() {
		defer close(first)
		handed := false
		// EVERY LATER VIEW IS READ AND DROPPED, so the watch beneath never
		// waits on this one, and closes with it.
		for view := range views {
			if !handed {
				first <- view
				handed = true
			}
		}
	}()
	return first, nil
}

// AN OPENING IS FILED ONLY ONCE ITS INBOX IS BOUND. A box reaches its job's
// opening the moment the bridge files it, a recovery's at once, and the bridge
// files it before ending the job's earlier opening, which waits up to five
// seconds for its answers. So the launch and the recovery bind the inbox before
// the bridge files the opening (Engine.openBridge), and a confirmation reaching
// the opening as the earlier one ends is recorded; bound after Open returned,
// the inbox served no job all that while (the package doc's "An OpenCode
// confirmation is recorded once, and reported").
//
// Mutation: bind the inbox once the bridge's Open has returned, and the
// confirmation is turned away.
func TestAnOpeningIsFiledOnlyOnceItsInboxIsBound(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store := sandbox.NewCoordStore(coordmem.NewFleet())
	runs, _ := steersOver(t, steerRing, store)
	launch := workingRun(t, store, runs, "run-1")
	fileSealed(t, runs, "run-1", steer.Note{ID: "n-1", Text: "use staging", By: "founder"})
	// SERVED TO THE JOB by the plugin's poll, before the recovery.
	served := boundInbox(t, runs, store, "run-1", launch)
	viewHolds(t, served, 1)
	if notes, _ := served.Wait(ctx, 0); len(notes) != 1 {
		t.Fatalf("the poll was answered %+v, want n-1", notes)
	}
	served.Close()

	server := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(server.Close)
	bridge := bridgeFor(t, mcpbridge.Options{Material: runtoken.OneKey("k", "test-key"), BaseURL: server.URL})
	mux := http.NewServeMux()
	mux.Handle(mcpbridge.PathPrefix+"{token}", bridge.Handler())
	mux.Handle(mcpbridge.SteerPattern, bridge.SteerHandler())
	server.Config.Handler = mux
	e := &Engine{bridge: bridge, runSteers: runs}
	company := specCompany(t, `
name: Acme
providers:
  sandbox: {fake: true}
  llm:
    zulu:
      model: claude-sonnet-5
roles:
  - {name: SWE, handle: swe, llm: zulu}
`)
	swe := company.Org.AgentSeatByHandle("swe")
	if swe == nil {
		t.Fatal("the company has no swe seat")
	}
	surface := tools.NewSurface("execute", tools.NewRegistry().Snapshot(), nil).
		ForTurn(&turnctx.Turn{RunID: "run-1", Seat: swe, Org: company.Org})

	// THE JOB'S OPENING THIS NODE HELD UNTIL THE RECOVERY, whose end is caught
	// as it closes that opening's inbox: by then the bridge has filed the
	// recovery's opening, and serves it.
	before := &heldInbox{closing: make(chan struct{}), release: make(chan struct{})}
	t.Cleanup(before.letGo)
	endpoint := bridge.Open(ctx, &mcpbridge.Session{RunID: "run-1", Launch: launch, Handle: "swe",
		Role: swe.Name, Surface: surface, Steer: before})
	if endpoint == "" {
		t.Fatal("the bridge opened no session for the job")
	}

	// THE RECOVERY'S SESSION, with an inbox of its own, as reopenAgentRun
	// builds it.
	row := mustRow(t, store, "run-1")
	inbox := runs.inbox(identityOfRun(row), 1, row.Harness)
	t.Cleanup(inbox.Close)
	ledger := newBridgeLedger(store, nil)
	session := &mcpbridge.Session{RunID: "run-1", Handle: "swe", Role: swe.Name, Surface: surface,
		Ledger: ledger, Steer: inbox}
	opened := make(chan error, 1)
	go func() {
		_, err := e.openBridge(ctx, session, ledger, inbox, row.NamedJob(0, "", 0))
		opened <- err
	}()
	select {
	case <-before.closing:
	case err := <-opened:
		t.Fatalf("the recovery's opening returned (%v) without ending the job's opening before it", err)
	}

	// THE BOX'S CONFIRMATION, as the job's earlier opening ends.
	status := confirmOn(t, server.Client(), mcpbridge.SteerURL(endpoint), "n-1")
	before.letGo()
	if err := <-opened; err != nil {
		t.Fatalf("openBridge: %v", err)
	}
	if status != http.StatusNoContent {
		t.Fatalf("a confirmation reaching the recovery's opening as the job's earlier one ended was "+
			"answered %d, want it recorded: 204", status)
	}
	if read := readOn(ctx, store, "run-1", "n-1"); read == nil || read.Launch != launch ||
		read.Boundary != string(types.SteerBoundaryStep) {
		t.Errorf("the confirmation left n-1 read %+v, want it read at job %s's step", read, launch)
	}
}

// heldInbox is a run's note inbox whose close holds until it is let go: the
// inbox of an opening whose end a case catches midway.
type heldInbox struct {
	closing chan struct{}
	release chan struct{}
	enter   sync.Once
	free    sync.Once
}

func (h *heldInbox) Take(context.Context, string, *sandbox.NoteCall) string { return "" }

func (h *heldInbox) Cancelled(context.Context, sandbox.NoteCall) {}

func (h *heldInbox) Wait(context.Context, int) ([]mcpbridge.SteerNote, <-chan struct{}) {
	return nil, h.release
}

func (h *heldInbox) Read(context.Context, []string) error { return mcpbridge.ErrNoteUntaken }

func (h *heldInbox) Again(context.Context, []string) (map[string]string, []string, error) {
	return nil, nil, mcpbridge.ErrNoteUntaken
}

func (h *heldInbox) Folded(context.Context, []string) {}

func (h *heldInbox) Close() {
	h.enter.Do(func() { close(h.closing) })
	<-h.release
}

func (h *heldInbox) letGo() { h.free.Do(func() { close(h.release) }) }

// confirmOn posts a run's confirmation that its harness read the notes ids to
// the run's notes route, and answers its status.
func confirmOn(t *testing.T, client *http.Client, route string, ids ...string) int {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"ids": ids})
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, route+"/read", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := client.Do(req)
	if err != nil {
		t.Fatalf("the confirmation failed on the wire: %v", err)
	}
	_ = res.Body.Close()
	return res.StatusCode
}

// THE FINISH READS THE STORE EVEN WHEN THE VIEW IS STALE: the run's last
// boundary is its last chance to read a note in its own loop, so it asks the
// record itself, where a batch answers from a view one write behind.
//
// Mutation: answer the finish from the view, and the note is missed.
func TestStopReadsTheStoreEvenWhenTheViewIsStale(t *testing.T) {
	t.Parallel()
	store := sandbox.NewCoordStore(coordmem.NewFleet())
	runs, _ := steersOver(t, steerRing, failingStore{PendingStore: store, deafWatch: true})
	launch := workingRun(t, store, runs, "run-1")
	fileSealed(t, runs, "run-1", steer.Note{ID: "n-1", Text: "use staging", By: "founder"})
	inbox := boundInbox(t, runs, store, "run-1", launch)
	if got := inbox.Take(t.Context(), mcpbridge.SteerEventToolBatch, hookCall()); got != "" {
		t.Fatalf("a batch answered %q from a view that never heard the record", got)
	}
	got := inbox.Take(t.Context(), mcpbridge.SteerEventFinish, hookCall())
	if !strings.Contains(got, "use staging") {
		t.Fatalf("the finish answered %q, want the note the record holds", got)
	}
	servedNotRead(t, store, "run-1", "n-1", launch, takeOf(t, got))
}

// A RUN PARKED ON ITS QUESTION HOLDS ITS NOTES FOR WHAT RESUMES IT: a note to it
// is read after the answer — so steer_turn says — its harness takes nothing while it
// waits, and the launch that carries the answer carries the notes in its brief,
// read there by the new job.
func TestAParkedRunHoldsItsNotesForWhatResumesIt(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store := sandbox.NewCoordStore(coordmem.NewFleet())
	runs, _ := steersOver(t, steerRing, store)
	launch := workingRun(t, store, runs, "run-1")
	fileSealed(t, runs, "run-1", steer.Note{ID: "n-before", Text: "use staging", By: "founder"})
	inbox := boundInbox(t, runs, store, "run-1", launch)

	if err := store.AttachSandbox(ctx, "run-1", sandbox.BoxRef{SandboxID: "box-1",
		Harness: "claude-code", PauseTTLSec: 1800}, sandbox.Fence{}); err != nil {
		t.Fatal(err)
	}
	asked := sandboxtest.Question("which branch?", sandbox.AskToRequester)
	asked.Branch = "wip/run-1"
	sandboxtest.Ask(t, store, "run-1", asked)
	if err := store.MarkAwaiting(ctx, "run-1", sandbox.Clarification{AskedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	filed, err := runs.File(ctx, "run-1", steer.Note{ID: "n-parked", Text: "and tag it", By: "founder"})
	if err != nil || filed.Status != steer.StatusAccepted || filed.Reads != steer.ReadsAfterAnswer {
		t.Fatalf("a note to the parked run = %+v, %v, want accepted and read after the answer", filed, err)
	}
	if got := inbox.Take(ctx, mcpbridge.SteerEventFinish, hookCall()); got != "" {
		t.Errorf("the parked run's harness took %q", got)
	}

	// THE ANSWER ARRIVES and is claimed; the relaunch carries the notes.
	if _, ok, err := store.RecordAnswer(ctx, "run-1", launch, sandbox.RecordedAnswer{
		Sealed: []byte("sealed"), Via: types.AnswerViaChat, EventIDs: []string{"ev-1"},
	}, sandbox.Fence{}); err != nil || !ok {
		t.Fatalf("RecordAnswer = %v, %v", ok, err)
	}
	claimed, ok, err := store.ClaimForResume(ctx, "run-1", sandbox.RecordedAnswerTail(launch), sandbox.Fence{})
	if err != nil || !ok {
		t.Fatalf("ClaimForResume = %v, %v", ok, err)
	}
	source := runs.source(ctx, claimed, sandbox.Fence{Owner: claimed.Owner, Epoch: claimed.OwnerEpoch})
	t.Cleanup(func() { source.Close(t.Context()) })
	brief := source.Brief(ctx).Notes
	if len(brief) != 2 || brief[0].ID != "n-before" || brief[1].ID != "n-parked" {
		t.Fatalf("the relaunch's brief carries %+v, want both notes, oldest first", brief)
	}
	sealed, err := runs.briefNotes("run-1", 2, brief, source)
	if err != nil {
		t.Fatal(err)
	}
	relaunch := claimed
	relaunch.BriefNotes = sealed
	relaunched, err := store.BeginLaunch(ctx, relaunch, sandbox.Fence{Owner: claimed.Owner, Epoch: claimed.OwnerEpoch})
	if err != nil {
		t.Fatalf("BeginLaunch: %v", err)
	}
	carried := runs.carried(mustRow(t, store, "run-1"))
	if len(carried) != 2 {
		t.Fatalf("the turn carries %+v, want both notes read", carried)
	}
	for _, read := range carried {
		if read.Boundary != types.SteerBoundaryBrief || read.Launch != relaunched.LaunchID {
			t.Errorf("note %s was read %+v, want in the brief of job %s", read.ID, read.Reading,
				relaunched.LaunchID)
		}
	}
}

// mustRow is turnID's record.
func mustRow(t *testing.T, store sandbox.PendingStore, turnID string) sandbox.PendingRun {
	t.Helper()
	row, found, err := store.Get(t.Context(), turnID)
	if err != nil || !found {
		t.Fatalf("Get %s = %v, %v", turnID, found, err)
	}
	return row
}

// AN ENDING EXPIRES ITS WAITING NOTES ONCE, UNDER STABLE IDS: the step an ending
// takes before it deletes the record records every note still waiting as
// expired, at the ending's instant and under ids derived from the turn, the
// ending and the note — so an ending retried after a failure publishes the same
// records, which a reader of the event log keeps once — and a note the turn
// read is not expired.
//
// Mutation: publish under fresh ids, and the retry's records differ.
func TestAnEndingExpiresWaitingNotesOnceUnderStableIDs(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store := sandbox.NewCoordStore(coordmem.NewFleet())
	runs, p := steersOver(t, steerRing, store)
	launch := workingRun(t, store, runs, "run-1")
	fileSealed(t, runs, "run-1", steer.Note{ID: "n-read", Text: "use staging", By: "founder"})
	fileSealed(t, runs, "run-1", steer.Note{ID: "n-unread", Text: "and tag it", By: "founder"})
	if _, err := store.TakeNotes(ctx, "run-1", sandbox.NoteTake{IDs: []string{"n-read"}, Launch: launch,
		Statuses: steerTakeStatuses, Read: sandbox.NoteRead{Phase: "execute", Boundary: "tool_batch",
			Launch: launch}}); err != nil {
		t.Fatal(err)
	}
	run := mustRow(t, store, "run-1")
	run.Ending = &sandbox.RecordedEnding{ID: "end-1", At: time.Date(2026, 10, 26, 9, 0, 0, 0, time.UTC)}
	for range 2 {
		if err := runs.ended(ctx, run); err != nil {
			t.Fatalf("ended: %v", err)
		}
	}
	p.mu.Lock()
	published := slices.Clone(p.events)
	p.mu.Unlock()
	if len(published) != 2 || published[0].ID != published[1].ID {
		t.Fatalf("the ending and its retry published %d records (%v), want one record twice", len(published),
			published)
	}
	rec, _ := events.DataAs[*types.AgentTurnSteered](published[0])
	if rec.NoteID != "n-unread" || rec.Outcome != types.SteerExpired || rec.Boundary != "" ||
		!published[0].Timestamp.Equal(run.Ending.At) {
		t.Errorf("the ending recorded %+v at %v, want the unread note expired at the ending's instant",
			rec, published[0].Timestamp)
	}
	other := run
	other.Ending = &sandbox.RecordedEnding{ID: "end-2", At: run.Ending.At}
	if err := runs.ended(ctx, other); err != nil {
		t.Fatal(err)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.events[2].ID == published[0].ID {
		t.Error("a different ending of the turn published under the same id")
	}
}

// AN ENDING REPORTS EACH NOTE NO STEP REPORTED, AS ITS RECORD KNOWS IT: a note a
// review's take recorded read and whose answer was lost, marked unanswered and
// told by no later launch, MISSED, at the round that took it; one a hook served
// whose job's session proves it never took, MISSED at that serve; one the
// run's bridge handed to its job's harness, never confirmed, UNCONFIRMED, at
// that job and the iteration the record keeps for it; one still waiting
// EXPIRED, naming no step; and nothing of a note a step reported reading.
//
// Mutation: report a missed note at no step, an unconfirmed one at no job or as
// expired, or leave any out, and the case goes red.
func TestAnEndingReportsEachNoteAsItsRecordKnowsIt(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store := sandbox.NewCoordStore(coordmem.NewFleet())
	runs, p := steersOver(t, steerRing, store)
	launch := workingRun(t, store, runs, "run-1")
	for _, id := range []string{"n-read", "n-missed", "n-absent", "n-handed", "n-waiting"} {
		fileSealed(t, runs, "run-1", steer.Note{ID: id, Text: "the note " + id, By: "founder"})
	}
	take := func(id, name string) {
		t.Helper()
		if _, err := store.TakeNotes(ctx, "run-1", sandbox.NoteTake{IDs: []string{id}, Launch: launch,
			Statuses: steerTakeStatuses, Read: sandbox.NoteRead{Phase: "review", Round: 1,
				Boundary: string(types.SteerBoundaryRound), Take: name}}); err != nil {
			t.Fatal(err)
		}
	}
	take("n-read", "answered")
	take("n-missed", "lost")
	if _, err := store.SettleTake(ctx, "run-1", sandbox.NoteSettle{Take: "lost", Unanswered: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ServeHook(ctx, "run-1", sandbox.HookServe{IDs: []string{"n-absent"}, Launch: launch,
		Statuses: steerTakeStatuses, Serve: sandbox.HookServed{Take: "absent",
			Boundary: string(types.SteerBoundaryToolBatch), Iteration: 1}}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SettleHook(ctx, "run-1", sandbox.HookSettle{Launch: launch,
		Absent: []string{"absent"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ServeNotes(ctx, "run-1", sandbox.NoteServe{IDs: []string{"n-handed"}, Launch: launch,
		Statuses: steerTakeStatuses}); err != nil {
		t.Fatal(err)
	}
	run := mustRow(t, store, "run-1")
	if run.Launch.Iteration == 0 {
		t.Fatal("the premise: the record keeps no iteration for its job")
	}
	run.Ending = &sandbox.RecordedEnding{ID: "end-1", At: time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)}
	if err := runs.ended(ctx, run); err != nil {
		t.Fatalf("ended: %v", err)
	}
	got := map[string]types.AgentTurnSteered{}
	for _, rec := range steered(p) {
		if _, again := got[rec.NoteID]; again {
			t.Errorf("note %s was reported twice", rec.NoteID)
		}
		got[rec.NoteID] = rec
	}
	if rec, ok := got["n-read"]; ok {
		t.Errorf("the ending reported a note a step read: %+v", rec)
	}
	if rec := got["n-waiting"]; rec.Outcome != types.SteerExpired || rec.Boundary != "" || rec.LaunchID != "" {
		t.Errorf("the waiting note was reported %+v, want it expired at no step", rec)
	}
	if rec := got["n-missed"]; rec.Outcome != types.SteerMissed || rec.Phase != types.PhaseReview ||
		rec.Round != 1 || rec.Boundary != types.SteerBoundaryRound || rec.LaunchID != "" {
		t.Errorf("the note a lost take recorded was reported %+v, want it missed at the review's first round", rec)
	}
	if rec := got["n-absent"]; rec.Outcome != types.SteerMissed || rec.Phase != types.PhaseExecute ||
		rec.Iteration != 1 || rec.Boundary != types.SteerBoundaryToolBatch || rec.LaunchID != launch {
		t.Errorf("the note a serve proved absent was reported %+v, want it missed at the tool batch of job %s",
			rec, launch)
	}
	if rec := got["n-handed"]; rec.Outcome != types.SteerUnconfirmed || rec.Phase != types.PhaseExecute ||
		rec.Round != 1 || rec.Boundary != types.SteerBoundaryStep || rec.LaunchID != launch ||
		rec.Iteration != run.Launch.Iteration {
		t.Errorf("the note handed to the job was reported %+v, want it unconfirmed at job %s, iteration %d",
			rec, launch, run.Launch.Iteration)
	}
	if len(got) != 4 {
		t.Errorf("the ending reported %d notes, want the four no step reported", len(got))
	}
}

// A NOTE A TAKE NEVER HANDED OVER IS DELIVERED WHERE A LATER LAUNCH TOLD IT
// AGAIN, and missed only where none did. A review's take whose answer was lost
// is marked unanswered: its reader never had the note. Every later launch's
// brief tells it again (runner.AgentRunRequest.Retold); the launch's write
// records it read by the job it opens, at the take's own place among the
// reads, so the turn carries it unmarked, ahead of a later take's note. The
// ending reports it delivered at that brief where the job started, else missed
// at the take. Twin: a hook's serve the session lacks leaves the note waiting
// at its first serve's place, reported missed there where no brief told it.
//
// Mutation: leave the marked read as found, place the telling at the write's
// place, or report a note as nothing, missed or a plain read: red.
func TestAMarkedNoteIsDeliveredWhereALaterLaunchToldItAgain(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		hook, relaunch, start bool
		want                  types.SteerOutcome
	}{
		"told again by a later job that started":             {relaunch: true, start: true, want: types.SteerDelivered},
		"told again by a launch that failed after its write": {relaunch: true, want: types.SteerMissed},
		"never told again": {want: types.SteerMissed},
		"a lost serve, told by a later job that started": {hook: true, relaunch: true, start: true},
		"a lost serve, never told again":                 {hook: true, want: types.SteerMissed},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			store := sandbox.NewCoordStore(coordmem.NewFleet())
			runs, p := steersOver(t, steerRing, store)
			launch := workingRun(t, store, runs, "run-1")
			fileSealed(t, runs, "run-1", steer.Note{ID: "n-1", Text: "use staging", By: "founder"})
			fileSealed(t, runs, "run-1", steer.Note{ID: "n-2", Text: "and tag the release", By: "founder"})
			take := func(id, name string) {
				t.Helper()
				if _, err := store.TakeNotes(ctx, "run-1", sandbox.NoteTake{IDs: []string{id}, Launch: launch,
					Statuses: steerTakeStatuses, Read: sandbox.NoteRead{Phase: string(types.PhaseReview),
						Iteration: 1, Round: 1, Boundary: string(types.SteerBoundaryRound), Take: name}}); err != nil {
					t.Fatal(err)
				}
			}
			placeOf := 1
			if tc.hook {
				if _, err := store.ServeHook(ctx, "run-1", sandbox.HookServe{IDs: []string{"n-1"}, Launch: launch,
					Statuses: steerTakeStatuses, Serve: sandbox.HookServed{Take: "lost",
						Boundary: string(types.SteerBoundaryToolBatch), Iteration: 1}}); err != nil {
					t.Fatal(err)
				}
				if _, err := store.SettleHook(ctx, "run-1", sandbox.HookSettle{Launch: launch,
					Absent: []string{"lost"}}); err != nil {
					t.Fatal(err)
				}
			} else {
				take("n-1", "lost")
				if _, err := store.SettleTake(ctx, "run-1", sandbox.NoteSettle{Take: "lost", Unanswered: true}); err != nil {
					t.Fatal(err)
				}
				placeOf = readOn(ctx, store, "run-1", "n-1").Order
			}
			take("n-2", "answered")
			row := mustRow(t, store, "run-1")
			var relaunched string
			if tc.relaunch {
				source := claimedSource(t, store, runs, launch)
				brief := source.Brief(ctx)
				claimed := mustRow(t, store, "run-1")
				fence := sandbox.Fence{Owner: claimed.Owner, Epoch: claimed.OwnerEpoch}
				relaunch := claimed
				if tc.hook {
					sealed, err := runs.briefNotes("run-1", 2, brief.Notes, source)
					if err != nil || len(sealed) != 1 || sealed[0].ID != "n-1" {
						t.Fatalf("the relaunch's brief carries %+v (%v), want n-1", sealed, err)
					}
					relaunch.BriefNotes = sealed
				} else {
					if !slices.Equal(brief.Unanswered, []string{"n-1"}) {
						t.Fatalf("the relaunch's brief says %v unanswered, want n-1", brief.Unanswered)
					}
					relaunch.RetoldNotes = retoldNotes(2, brief.Unanswered)
				}
				opened, err := store.BeginLaunch(ctx, relaunch, fence)
				if err != nil {
					t.Fatalf("BeginLaunch: %v", err)
				}
				relaunched = opened.LaunchID
				if tc.start {
					if err := store.AttachSandbox(ctx, "run-1", sandbox.BoxRef{SandboxID: "box-2", CommandID: "cmd-2",
						Harness: "claude-code", PauseTTLSec: 1800}, fence); err != nil {
						t.Fatalf("AttachSandbox: %v", err)
					}
				}
				row = mustRow(t, store, "run-1")
				var order []string
				for _, read := range runs.carried(row) {
					order = append(order, read.ID)
					if read.ID == "n-1" && (read.Unanswered || read.Launch != relaunched ||
						read.Boundary != types.SteerBoundaryBrief || read.Iteration != 2 || read.Order != placeOf) {
						t.Errorf("the note told again is carried %+v, want job %s's brief's read, unmarked, at its "+
							"place %d", read.Reading, relaunched, placeOf)
					}
				}
				if !slices.Equal(order, []string{"n-1", "n-2"}) {
					t.Errorf("the turn carries its notes as %v, want n-1 then n-2, the note told again at its "+
						"place", order)
				}
			}
			row.Ending = &sandbox.RecordedEnding{ID: "end-1", At: time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)}
			if err := runs.ended(ctx, row); err != nil {
				t.Fatalf("ended: %v", err)
			}
			var rec *types.AgentTurnSteered
			for _, r := range steered(p) {
				switch r.NoteID {
				case "n-1":
					if rec != nil {
						t.Errorf("the note was reported twice: %+v and %+v", *rec, r)
					}
					rec = &r
				case "n-2":
					t.Errorf("the ending reported a note an answered take read: %+v", r)
				}
			}
			// THE BRIEF'S OWN READ, its job started, is its runner's to report.
			if tc.want == "" {
				if rec != nil {
					t.Errorf("the ending reported %+v, want nothing of a note a started job's brief read", *rec)
				}
				return
			}
			if rec == nil {
				t.Fatalf("the ending reported nothing of the note, want it %s", tc.want)
			}
			got := *rec
			switch {
			case tc.want == types.SteerDelivered:
				if got.Outcome != tc.want || got.Retold == tc.hook || got.Phase != types.PhaseExecute ||
					got.Iteration != 2 || got.Round != 1 || got.Boundary != types.SteerBoundaryBrief ||
					got.LaunchID != relaunched {
					t.Errorf("the note is reported %+v, want it delivered at job %s's brief, told again unless "+
						"no read of it was marked", got, relaunched)
				}
			case tc.hook:
				if got.Outcome != tc.want || got.Phase != types.PhaseExecute || got.Iteration != 1 ||
					got.Boundary != types.SteerBoundaryToolBatch || got.LaunchID != launch {
					t.Errorf("the note is reported %+v, want it missed at job %s's serve", got, launch)
				}
			default:
				if got.Outcome != tc.want || got.Retold || got.Phase != types.PhaseReview || got.Iteration != 1 ||
					got.Boundary != types.SteerBoundaryRound || got.LaunchID != "" {
					t.Errorf("the note is reported %+v, want it missed at the review's round", got)
				}
			}
		})
	}
}

// A NOTE A TAKE NEVER HANDED OVER IS TOLD AGAIN BY THE NEXT LAUNCH, THROUGH THE
// REAL TURN, AND DELIVERED THERE. A review's take of a note landed, its answer
// lost and its read marked unanswered; the review sends the round back, the
// relaunch's runner names the note told again, its launcher hands that on, and
// the launch's write records it read by the job it opens, told again, at the
// take's place; the run's brief holds it. The review accepts, and the ending
// reports it delivered at that brief, told again, once. Its twin, a hook's
// serve the job's session lacks, leaves the note for the review's first round.
//
// Mutation: drop the notes told again from the runner's request, from the
// launcher's hand-over or from the launch's write, or report the note as
// anything but delivered there, told again, and the case goes red.
func TestAMarkedNoteIsToldAgainByTheNextLaunchThroughTheRealTurn(t *testing.T) {
	t.Parallel()
	t.Run("a lost serve", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		c := newSteerCase(t, nil)
		parkOnARun(t, c.e, c.rig, "run-1")
		first := c.rig.row(t, "run-1")
		c.file(t, "run-1", "n-1", "use the staging database")
		pending := c.e.sandbox.Load().pending
		servedByAHook(t, pending, first, "n-1")
		if _, err := pending.SettleHook(ctx, "run-1", sandbox.HookSettle{Launch: first.LaunchID,
			Absent: []string{"take-1"}}); err != nil {
			t.Fatal(err)
		}
		if err := c.rig.collect(t, "run-1"); err != nil {
			t.Fatalf("collect: %v", err)
		}
		got := c.steeringOver(t, "run-1", 1)
		if len(got) != 1 || got[0].Outcome != types.SteerDelivered || got[0].Phase != types.PhaseReview ||
			got[0].Boundary != types.SteerBoundaryRound {
			t.Errorf("the turn recorded %+v, want the note delivered once, at the review's round", got)
		}
	})
	ctx := t.Context()
	c := newSteerCaseOn(t, &reviewWatch{sendBack: 1}, nil)
	// ROOM FOR THE ROUND THE REVIEW SENDS BACK.
	c.e.Company().Config.TurnEngine.MaxIterations = 2
	parkOnARun(t, c.e, c.rig, "run-1")
	first := c.rig.row(t, "run-1").LaunchID
	c.file(t, "run-1", "n-1", "use the staging database")
	pending := c.e.sandbox.Load().pending
	if _, err := pending.TakeNotes(ctx, "run-1", sandbox.NoteTake{IDs: []string{"n-1"}, Launch: first,
		Statuses: steerTakeStatuses, Read: sandbox.NoteRead{Phase: string(types.PhaseReview), Iteration: 1,
			Round: 1, Boundary: string(types.SteerBoundaryRound), Take: "lost"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := pending.SettleTake(ctx, "run-1", sandbox.NoteSettle{Take: "lost", Unanswered: true}); err != nil {
		t.Fatal(err)
	}
	marked := *readOn(ctx, pending, "run-1", "n-1")

	if err := c.rig.collect(t, "run-1"); err != nil {
		t.Fatalf("collect: %v", err)
	}
	row := c.rig.row(t, "run-1")
	if row.LaunchID == first || row.Status != sandbox.StatusRunning {
		t.Fatalf("the review's send-back did not park the turn on a run of its own: %s (first job %s)",
			row.Status, first)
	}
	if read := readOn(ctx, pending, "run-1", "n-1"); read == nil || read.Retold == nil || read.Unanswered ||
		read.Take != "" || read.Launch != row.LaunchID || read.Boundary != string(types.SteerBoundaryBrief) ||
		read.Order != marked.Order {
		t.Fatalf("the note is read %+v, want it told again by job %s's brief, at its take's place %d", read,
			row.LaunchID, marked.Order)
	}
	// ONLY THE RELAUNCH'S BRIEF CAN HOLD THE NOTE, filed after the first run
	// launched.
	if started := c.rig.started(); len(started) != 2 || !slices.ContainsFunc(started, func(r sandbox.RunRequest) bool {
		return strings.Contains(r.Brief, "use the staging database")
	}) {
		t.Fatalf("the relaunch's run was not handed the note in its brief (%d runs started)", len(started))
	}

	if err := c.rig.collect(t, "run-1"); err != nil {
		t.Fatalf("collect: %v", err)
	}
	if _, found, _ := pending.Get(ctx, "run-1"); found {
		t.Fatal("the review did not end the turn")
	}
	got := c.awaitSteered(t, 1)
	if len(got) != 1 || got[0].NoteID != "n-1" || got[0].Outcome != types.SteerDelivered || !got[0].Retold ||
		got[0].LaunchID != row.LaunchID || got[0].Boundary != types.SteerBoundaryBrief ||
		got[0].Phase != types.PhaseExecute {
		t.Fatalf("the turn recorded %+v, want the note delivered at job %s's brief, told again, once", got,
			row.LaunchID)
	}
}

// A NOTE LEFT WAITING WHEN A RUN ENDS IS RECORDED EXPIRED BY THE ENGINE'S OWN
// COORDINATOR. The engine builds the coordinator that ends its runs
// (sandboxRuntimeOn) and hands it the step that records the run's waiting
// notes before the record goes ([sandbox.CoordinatorOptions.SteersEnded]);
// nothing else holds the note once the record is deleted, and a person told it
// waits learns its fate from that record alone. Here a person's stop ends the
// run its turn is parked on, through the engine's own pause path, with a note
// no step took: one record of it, `expired`, at the ending's instant (the
// stop announcement's), and none `delivered`.
//
// Mutation: drop SteersEnded from the coordinator options: no record.
func TestANoteLeftWaitingWhenARunEndsIsRecordedExpiredByTheEnginesCoordinator(t *testing.T) {
	t.Parallel()
	c := newSteerCase(t, nil)
	parkOnARun(t, c.e, c.rig, "run-1")
	c.file(t, "run-1", "req-1", "use the staging database")

	pause := janeStops()
	c.e.applySeatPause(t.Context(), coord.SeatPauseUpdate{Seat: pause.Seat, Pause: &pause})
	stopFinished(t, c.e, "run-1")

	got := c.awaitSteered(t, 1)
	if len(got) != 1 || got[0].NoteID != "req-1" || got[0].TurnID != "run-1" ||
		got[0].Outcome != types.SteerExpired || got[0].Boundary != "" || got[0].LaunchID != "" {
		t.Fatalf("the run's ending recorded %+v, want the waiting note expired, once", got)
	}
	// ONE ANNOUNCEMENT, on the two topics every run's announcement goes to.
	stops := recordsOf(c.q, "run-1").runStops
	if len(stops) == 0 || slices.ContainsFunc(stops, func(ev *events.Event) bool { return ev.ID != stops[0].ID }) {
		t.Fatalf("the run's stop was announced as %d records, want one", len(stops))
	}
	if expired := c.steered()[0]; !expired.Timestamp.Equal(stops[0].Timestamp) {
		t.Errorf("the note expired at %v, want the ending's instant %v", expired.Timestamp, stops[0].Timestamp)
	}
}

// A RESUMED SEGMENT THAT ENDS THE TURN IS REMEMBERED CLOSED, on whichever node
// resumed it. The run's record goes with the turn, so a note that reaches a
// node after it is answered by the node's desk alone — and the node that
// resumes a parked turn need not be the one that launched it, nor the same
// process: here its desk is a fresh one, as a restarted node's is, holding
// nothing of the turn. Its resumed segment ends the turn, and from then the
// desk answers a note to it `closed`, naming its seat — never nobody, which a
// person reads as a lost note to send again.
//
// Mutation: drop the remember from the resume, and the desk answers nothing.
func TestAResumedSegmentThatEndsTheTurnIsRememberedClosed(t *testing.T) {
	t.Parallel()
	c := newSteerCase(t, nil)
	parkOnARun(t, c.e, c.rig, "run-1")
	restartedDesk(t, c.e)
	if _, _, _, err := c.e.steers.offer(t.Context(), "run-1", steer.Note{ID: "n", Text: "x"}); err == nil {
		t.Fatal("a desk that never held the turn answered for it")
	}

	if err := c.rig.collect(t, "run-1"); err != nil {
		t.Fatalf("collect: %v", err)
	}
	if _, found, _ := c.e.sandbox.Load().pending.Get(t.Context(), "run-1"); found {
		t.Fatal("the resumed segment did not end the turn")
	}
	status, _, seat, err := c.e.steers.offer(t.Context(), "run-1", steer.Note{ID: "n", Text: "x"})
	if err != nil || status != steer.StatusClosed || seat.agent != seatOf("swe").String() || seat.handle != "swe" {
		t.Errorf("a note to the turn its resumed segment ended answered (%s, %+v, %v), want closed on swe",
			status, seat, err)
	}
	if status, seat, err := c.e.steers.probe("run-1"); err != nil || status != steer.StatusClosed ||
		seat.handle != "swe" {
		t.Errorf("the turn's probe answered (%s, %+v, %v), want closed on swe", status, seat, err)
	}
}

// A RESUMED SEGMENT THAT PARKS AGAIN IS NOT REMEMBERED: its turn goes on, on a
// new run whose record is its notes' home — so the desk answers nothing for it,
// and a note goes to the record, accepted, where a desk that called the turn
// ended would have told the person it was over.
//
// Mutation: remember every resumed segment that returns, and the desk answers
// the running turn closed.
func TestAResumedSegmentThatParksAgainIsNotRemembered(t *testing.T) {
	t.Parallel()
	c := newSteerCaseOn(t, &reviewWatch{sendBack: 1}, nil)
	// ROOM FOR THE ROUND THE REVIEW SENDS BACK.
	c.e.Company().Config.TurnEngine.MaxIterations = 2
	parkOnARun(t, c.e, c.rig, "run-1")
	first := c.rig.row(t, "run-1").LaunchID
	restartedDesk(t, c.e)

	if err := c.rig.collect(t, "run-1"); err != nil {
		t.Fatalf("collect: %v", err)
	}
	if row := c.rig.row(t, "run-1"); row.LaunchID == first || row.Status != sandbox.StatusRunning {
		t.Fatalf("the resumed segment did not park on a run of its own: %+v (first job %s)", row.Status, first)
	}
	if status, _, _, err := c.e.steers.offer(t.Context(), "run-1", steer.Note{ID: "n", Text: "x"}); err == nil {
		t.Errorf("the desk answered %s for a turn that parked again, want nothing", status)
	}
	c.file(t, "run-1", "req-1", "use the staging database")
}

// restartedDesk gives e a desk of its own holding nothing, as a node that
// restarted has: every turn it knew is gone with the process.
func restartedDesk(t *testing.T, e *Engine) {
	t.Helper()
	d := mustSteerDesk(t, steerRing)
	d.runs = e.runSteers
	e.steers = d
}

// A LAUNCHED TURN'S LATE NOTE IS FILED ON ITS RECORD. The launch hands the
// first segment's box over once the run's record exists, so a note that read
// "no record" a moment before and was scattered to the node that held the box
// is filed on the run's record by that node — answered as the record answers
// it, never `closed` for a turn that is running.
//
// Mutation: drop the hand-over from the launch, and the segment's end closes
// the box as an ended turn's.
func TestALaunchedTurnsLateNoteIsFiledOnItsRecord(t *testing.T) {
	t.Parallel()
	c := newSteerCase(t, nil)
	parkOnARun(t, c.e, c.rig, "run-1")
	status, reads, seat, err := c.e.steers.offer(t.Context(), "run-1", steer.Note{
		ID: "req-late", Text: "use staging", By: "founder", ByKind: iam.ActorHuman})
	if err != nil || status != steer.StatusAccepted || reads != steer.ReadsNextStep || seat.handle != "swe" {
		t.Fatalf("a late note to the launched turn answered (%s, %s, %+v, %v), want accepted on swe",
			status, reads, seat, err)
	}
	row := c.rig.row(t, "run-1")
	if len(row.Steer.Notes) != 1 || row.Steer.Notes[0].ID != "req-late" || !row.Steer.Notes[0].Waiting() {
		t.Errorf("the run's record holds %+v, want the late note waiting", row.Steer.Notes)
	}
}

// launchWindow is a run store that lets a case act inside a launch: the moment
// the write that opens the run's record has landed — before the launch has
// gone on to anything else — and when the launch first attaches a box.
type launchWindow struct {
	sandbox.PendingStore
	mu       sync.Mutex
	written  func(ctx context.Context, turnID string)
	attached func(ctx context.Context, turnID string)
}

func (w *launchWindow) BeginLaunch(ctx context.Context, run sandbox.PendingRun, fence sandbox.Fence) (sandbox.PendingRun, error) {
	opened, err := w.PendingStore.BeginLaunch(ctx, run, fence)
	w.mu.Lock()
	hook := w.written
	w.written = nil
	w.mu.Unlock()
	if hook != nil && err == nil {
		hook(ctx, run.TurnID)
	}
	return opened, err
}

func (w *launchWindow) AttachSandbox(ctx context.Context, turnID string, box sandbox.BoxRef, fence sandbox.Fence) error {
	w.mu.Lock()
	hook := w.attached
	w.attached = nil
	w.mu.Unlock()
	if hook != nil {
		hook(ctx, turnID)
	}
	return w.PendingStore.AttachSandbox(ctx, turnID, box, fence)
}

// A NOTE THE BOX HELD AT THE LAUNCH IS READ BEFORE ONE FILED ON THE RECORD
// AS IT IS WRITTEN. A note that reached the first segment's box after its brief
// was taken was sent before any note filed on the record once the launch has
// written one, and the moment that write lands any node's steer_turn files on
// it directly. So the box's note rides the write that creates the record,
// ahead of everything: readers take in the record's order, and where two notes
// disagree the later word is the one a model is told to keep. Handed over after
// the record existed, it was filed behind a note filed in that window.
//
// Mutation: hand the box over after the launch's write: the note filed as the
// record is written is first.
func TestANoteTheBoxHeldAtTheLaunchIsReadBeforeOneFiledDuringIt(t *testing.T) {
	t.Parallel()
	model := &reviewWatch{}
	e, _ := indicatingWith(t, notify.StatusAlways, model)
	steerable(t, e)
	// THE BOX'S NOTE, offered as the executor's phase opens: its brief has
	// been taken, and its launch has not begun.
	var once sync.Once
	e.Backends().Queue.AddPublishListener(func(ctx context.Context, _ string, ev *events.Event) {
		started, ok := events.DataAs[*types.AgentPhaseStarted](ev)
		if !ok || started.Phase != types.PhaseExecute {
			return
		}
		once.Do(func() {
			status, _, _, err := e.steers.offer(ctx, started.TurnID, steer.Note{
				ID: "use-postgres", Text: "use postgres", By: "founder", ByKind: iam.ActorHuman})
			if err != nil || status != steer.StatusAccepted {
				t.Errorf("the note to the box before the launch answered (%s, %v)", status, err)
			}
		})
	})
	store := &launchWindow{PendingStore: sandbox.NewCoordStore(e.backends.Fleet)}
	var direct builtin.RunFiled
	store.written = func(ctx context.Context, turnID string) {
		var err error
		direct, err = e.RunSteers().File(ctx, turnID, steer.Note{
			ID: "use-sqlite", Text: "actually, sqlite", By: "founder", ByKind: iam.ActorHuman})
		if err != nil {
			t.Errorf("the note filed on the record as it was written: %v", err)
		}
	}
	rig := rigRunsOn(t, e, store, modelPlayer(t, model.run))
	parkOnARun(t, e, rig, "run-1")

	if direct.Status != steer.StatusAccepted {
		t.Fatalf("the note filed on the record as it was written answered %+v", direct)
	}
	notes := slices.Clone(rig.row(t, "run-1").Steer.Notes)
	slices.SortFunc(notes, func(a, b sandbox.RunNote) int { return a.Seq - b.Seq })
	var order []string
	for _, n := range notes {
		order = append(order, n.ID)
	}
	if want := []string{"use-postgres", "use-sqlite"}; !slices.Equal(order, want) {
		t.Errorf("the record holds the notes in the order %v, want them as they were sent, %v", order, want)
	}
}

// polledNote is what a run's notes plugin saw of its turn's notes over the
// bridge's real route, and what the record held once its confirmation landed.
type polledNote struct {
	mu    sync.Mutex
	notes []mcpbridge.SteerNote
	read  *sandbox.NoteRead
	err   error
}

// AN OPENCODE RUN'S NOTE IS READ AT ITS NEXT STEP BY ITS JOB, over the bridge's
// own notes route. The run's plugin polls the route its launch handed it, the
// run inbox answers the note under an id derived from the run's job — the id
// the plugin admits it to the session under, so a second admission is the
// first — and the plugin's confirmation through the route's read records the
// note read by that job at boundary `step`, and publishes it delivered once,
// naming the job.
//
// Mutation: record the read at another boundary, publish nothing for it, or
// derive the id from anything but the job, and this goes red.
func TestAnOpenCodeRunsNoteIsReadAtItsNextStepByItsJob(t *testing.T) {
	t.Parallel()
	model := &reviewWatch{}
	e, _ := indicatingOn(t, notify.StatusAlways, model, executor.OpenCodeName)
	steerable(t, e)
	script := modelPlayer(t, model.run)
	client := httpxtest.Pool(t)
	seen := &polledNote{}
	rig := rigRuns(t, e, func(ctx context.Context, req sandbox.RunRequest) sandbox.Result {
		seen.mu.Lock()
		defer seen.mu.Unlock()
		seen.err = pollAndConfirm(ctx, client, req.Steer.URL, seen)
		if row, found, err := e.sandbox.Load().pending.Get(ctx, "run-1"); err == nil && found {
			for _, n := range row.Steer.Notes {
				if n.ID == "req-1" {
					seen.read = n.Read
				}
			}
		}
		return script(ctx, req)
	})
	parkOnARun(t, e, rig, "run-1")
	c := &steerCase{e: e, rig: rig, q: e.backends.Queue.(*memory.Queue), model: model}
	c.file(t, "run-1", "req-1", "use the staging database")
	launch := rig.row(t, "run-1").LaunchID
	if err := rig.collect(t, "run-1"); err != nil {
		t.Fatalf("collect: %v", err)
	}
	seen.mu.Lock()
	defer seen.mu.Unlock()
	if seen.err != nil {
		t.Fatalf("the plugin's poll and read: %v", seen.err)
	}
	if len(seen.notes) != 1 || seen.notes[0].ID != "req-1" ||
		seen.notes[0].Inbox != executor.InboxID(executor.OpenCodeName, launch, "req-1") ||
		!strings.Contains(seen.notes[0].Message, "use the staging database") {
		t.Fatalf("the poll answered %+v, want the note under the id job %s admits it as", seen.notes, launch)
	}
	if seen.read == nil || seen.read.Boundary != string(types.SteerBoundaryStep) || seen.read.Launch != launch ||
		seen.read.Phase != string(types.PhaseExecute) {
		t.Errorf("the record holds the note read %+v, want read at boundary step by job %s", seen.read, launch)
	}
	if all := c.steeringOver(t, "run-1", 1); len(all) != 1 || all[0].Outcome != types.SteerDelivered ||
		all[0].Boundary != types.SteerBoundaryStep || all[0].LaunchID != launch {
		t.Errorf("the note was recorded %+v, want delivered once at a step of job %s", all, launch)
	}
}

// pollNotes is one poll of a run's notes route: the notes waiting in its
// inbox's view past the cursor after, which the bridge holds the poll for
// until one waits — a peek, which records nothing read.
func pollNotes(ctx context.Context, client *http.Client, route string, after int) ([]mcpbridge.SteerNote, error) {
	if route == "" {
		return nil, errors.New("the run was handed no notes route")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s/notes?after=%d", route, after), nil)
	if err != nil {
		return nil, err
	}
	res, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	var body struct {
		Notes []mcpbridge.SteerNote `json:"notes"`
	}
	err = json.NewDecoder(res.Body).Decode(&body)
	_ = res.Body.Close()
	if err != nil {
		return nil, fmt.Errorf("the poll's answer (%d): %w", res.StatusCode, err)
	}
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("the poll answered %d", res.StatusCode)
	}
	return body.Notes, nil
}

// awaitInView waits, within the bound a case gives every wait, until the view
// a run's main agent's batch is answered from holds the note id: the run's
// inbox's view of its record, which its notes route is answered from too, so
// a poll of the route that answers the note is the view holding it — and,
// being a peek, leaves the note waiting for the step the case is about.
func awaitInView(ctx context.Context, client *http.Client, route, id string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	for after := 0; ; {
		notes, err := pollNotes(ctx, client, route, after)
		if err != nil {
			return fmt.Errorf("the run's view never held note %s: %w", id, err)
		}
		for _, n := range notes {
			if n.ID == id {
				return nil
			}
			after = max(after, n.Seq)
		}
	}
}

// pollAndConfirm is a run's notes plugin, once: a poll of the route its launch
// handed it, and a read of every note the poll answered.
func pollAndConfirm(ctx context.Context, client *http.Client, route string, seen *polledNote) error {
	notes, err := pollNotes(ctx, client, route, 0)
	if err != nil {
		return err
	}
	seen.notes = notes
	ids := make([]string, 0, len(notes))
	for _, n := range notes {
		ids = append(ids, n.ID)
	}
	raw, _ := json.Marshal(map[string]any{"ids": ids})
	confirm, err := http.NewRequestWithContext(ctx, http.MethodPost, route+"/read", bytes.NewReader(raw))
	if err != nil {
		return err
	}
	confirm.Header.Set("Content-Type", "application/json")
	answered, err := client.Do(confirm)
	if err != nil {
		return err
	}
	_ = answered.Body.Close()
	if answered.StatusCode != http.StatusNoContent {
		return fmt.Errorf("the read answered %d", answered.StatusCode)
	}
	return nil
}

// watchedFor is a run store that tells a case when every watch of a run's notes
// open on it has handed its follower a view holding one note, and the follower
// has taken it: the moment the follower's view — what the tool loop's Waiting
// reads — has it too.
//
// TAKEN, NOT JUST HANDED OVER: a follower opens a view's notes and stores it
// between two receives, so the same view is handed to it again, and that send
// completes only once the first is its view. Marked as the first send
// completed, the case went on while the follower was still opening the note,
// and a review that asked Waiting in that gap read no note.
type watchedFor struct {
	sandbox.PendingStore
	note string

	mu     sync.Mutex
	next   int
	active map[int]bool
}

func (w *watchedFor) WatchNotes(ctx context.Context, turnID string) (<-chan sandbox.NotesView, error) {
	views, err := w.PendingStore.WatchNotes(ctx, turnID)
	if err != nil {
		return nil, err
	}
	w.mu.Lock()
	id := w.next
	w.next++
	w.active[id] = false
	w.mu.Unlock()
	out := make(chan sandbox.NotesView)
	go func() {
		defer func() {
			w.mu.Lock()
			delete(w.active, id)
			w.mu.Unlock()
			close(out)
		}()
		for view := range views {
			select {
			case out <- view:
			case <-ctx.Done():
				return
			}
			if slices.ContainsFunc(view.Steer.Notes, func(n sandbox.RunNote) bool {
				return n.ID == w.note && n.Waiting()
			}) {
				select {
				case out <- view:
				case <-ctx.Done():
					return
				}
				w.mu.Lock()
				if _, open := w.active[id]; open {
					w.active[id] = true
				}
				w.mu.Unlock()
			}
		}
	}()
	return out, nil
}

// everyWatchSaw reports whether every open watch has handed over the note.
func (w *watchedFor) everyWatchSaw() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, saw := range w.active {
		if !saw {
			return false
		}
	}
	return len(w.active) > 0
}

// A NOTE FILED WHILE THE REVIEWER WORKS ENDS ITS RUN, AND THE NEXT RUN OPENS
// WITH IT. The reviewer of a turn resumed from its executor is a run of the
// engine's own loop, which asks its source whether a note waits after every
// call (Waiting, under the dispatcher's lock): the segment's source is the run's
// record, so a note a person files on it while the review works ends the run
// once the response in flight has its answers, and the next run opens with the
// note as a user message — read at that run's round, never inside a tool's
// result.
//
// Mutation: answer Waiting from nothing (false), and the review's one run
// submits without ever reading the note, which its ending then expires.
func TestANoteFiledDuringTheReviewEndsTheReviewersRun(t *testing.T) {
	t.Parallel()
	const note = "the release branch, not main"
	var (
		mu      sync.Mutex
		reviews []string
	)
	store := &watchedFor{note: "req-mid-review", active: map[int]bool{}}
	var e *Engine
	pick := func(offered []string) llmtest.Run {
		if !offers(offered, runner.SubmitReviewTool) {
			return scripted{}.run(offered)
		}
		mu.Lock()
		first := len(reviews) == 1
		mu.Unlock()
		if !first {
			return calling(runner.SubmitReviewTool, map[string]any{"decision": "done"}, llmtest.DefaultUsage)
		}
		// THE PERSON'S NOTE, filed as the review's first run starts — after
		// the round's own read found nothing — and in the view the loop's
		// Waiting reads before the run's first call is answered.
		filed, err := e.RunSteers().File(context.Background(), "run-1", steer.Note{
			ID: "req-mid-review", Text: note, By: "founder", ByKind: iam.ActorHuman})
		if err != nil || filed.Status != steer.StatusAccepted {
			t.Errorf("filing the note mid-review answered (%+v, %v)", filed, err)
		}
		deadline := time.Now().Add(10 * time.Second)
		for !store.everyWatchSaw() && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
		// A SUBMISSION THE TOOL REFUSES, then one it takes: the run goes on
		// past the first unless the note ends it.
		return llmtest.Run{Responses: []llmtest.Response{
			{Calls: []llmtest.Call{{Name: runner.SubmitReviewTool, Args: `{"decision":"not-a-decision"}`}}},
			{Calls: []llmtest.Call{{Name: runner.SubmitReviewTool, Args: `{"decision":"done"}`}}},
		}}
	}
	cli := phasedHarness{model: "scripted", pick: pick}
	model := providerFunc(func(ctx context.Context, req llm.Request) (*llm.Completion, error) {
		if req.Tools != nil {
			if offered, err := offeredTo(req); err == nil && offers(offered, runner.SubmitReviewTool) {
				var b strings.Builder
				for _, m := range req.Messages {
					b.WriteString(m.Content)
					b.WriteString("\n")
				}
				mu.Lock()
				reviews = append(reviews, b.String())
				mu.Unlock()
			}
		}
		return cli.Complete(ctx, req)
	})
	e, _ = indicatingWith(t, notify.StatusAlways, model)
	seals, err := newSteerSeals(steerRing)
	if err != nil {
		t.Fatal(err)
	}
	store.PendingStore = sandbox.NewCoordStore(e.backends.Fleet)
	e.runSteers = newRunSteers(seals, store, e.backends.Queue)
	e.steers = mustSteerDesk(t, steerRing)
	e.steers.runs = e.runSteers
	q := e.backends.Queue.(*memory.Queue)
	rig := rigRuns(t, e, modelPlayer(t, scripted{}.run))
	parkOnARun(t, e, rig, "run-1")
	if err := rig.collect(t, "run-1"); err != nil {
		t.Fatalf("collect: %v", err)
	}
	mu.Lock()
	got := slices.Clone(reviews)
	mu.Unlock()
	if len(got) != 2 {
		t.Fatalf("the review ran %d times, want the run the note ended and the one that read it", len(got))
	}
	if strings.Contains(got[0], note) {
		t.Error("the review's first run read a note filed after it began")
	}
	if !strings.Contains(got[1], note) {
		t.Errorf("the review's second run did not open with the note:\n%s", got[1])
	}
	c := &steerCase{e: e, rig: rig, q: q}
	read := c.awaitSteered(t, 1)
	if len(read) != 1 || read[0].Outcome != types.SteerDelivered || read[0].Phase != types.PhaseReview ||
		read[0].Boundary != types.SteerBoundaryRound || read[0].Round != 2 {
		t.Errorf("the note was recorded %+v, want delivered at round 2 of the review", read)
	}
}

// providerFunc is a model answered by a function.
type providerFunc func(ctx context.Context, req llm.Request) (*llm.Completion, error)

func (providerFunc) Model() string { return "scripted" }

func (f providerFunc) Complete(ctx context.Context, req llm.Request) (*llm.Completion, error) {
	return f(ctx, req)
}

// AN ENDING REPORTS A NOTE A HOOK WAS SERVED AT THAT SERVE. A note a run's hook
// was served and its job's session never confirmed is UNCONFIRMED at the step
// the hook answered, of the job it served and at the iteration the serve kept,
// never at a poll's step; so is one whose serve its harness cancelled, whose
// session may hold it; one whose serve its session proves absent, read by no
// step since, is MISSED there; and the job's item is reported as nothing.
//
// Mutation: report a hook's live serve at a poll's step or at no step, report
// the cancelled serve's note missed (sandbox's endOf without its `lost != nil
// && mayHold` arm), the absent one's as anything but missed, or report the
// job's item, and the case goes red.
func TestAnEndingReportsAHookServedNoteAtItsServe(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	fleet := coordmem.NewFleet()
	store := sandbox.NewCoordStore(fleet)
	runs, p := steersOver(t, steerRing, store)
	launch := workingRun(t, store, runs, "run-1")
	for _, id := range []string{"n-live", "n-lost", "n-absent"} {
		fileSealed(t, runs, "run-1", steer.Note{ID: id, Text: "the note " + id, By: "founder"})
	}
	placeItemFor(t, runs, fleet, "run-1", "i-1", launch)
	serve := func(take string, call int, boundary types.SteerBoundary, iteration int, ids ...string) {
		t.Helper()
		if _, err := store.ServeHook(ctx, "run-1", sandbox.HookServe{IDs: ids, Launch: launch,
			Statuses: steerTakeStatuses, Serve: sandbox.HookServed{Take: take, Boundary: string(boundary),
				Iteration: iteration, Call: &sandbox.NoteCall{Session: "mcp-session-1", ID: fmt.Sprint(call)}}}); err != nil {
			t.Fatal(err)
		}
	}
	serve("take-1", 1, types.SteerBoundaryFinish, 4, "n-live", "i-1")
	serve("take-2", 2, types.SteerBoundaryToolBatch, 2, "n-lost")
	serve("take-3", 3, types.SteerBoundaryToolBatch, 3, "n-absent")
	if _, err := store.SettleHook(ctx, "run-1", sandbox.HookSettle{Launch: launch, Absent: []string{"take-3"},
		Cancelled: []sandbox.NoteCall{{Session: "mcp-session-1", ID: "2"}}}); err != nil {
		t.Fatal(err)
	}
	run := mustRow(t, store, "run-1")
	if run.Launch.Iteration == 4 {
		t.Fatal("the premise: the record keeps the serve's own iteration for its job")
	}
	run.Ending = &sandbox.RecordedEnding{ID: "end-1", At: time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)}
	if err := runs.ended(ctx, run); err != nil {
		t.Fatalf("ended: %v", err)
	}
	got := map[string]types.AgentTurnSteered{}
	for _, rec := range steered(p) {
		got[rec.NoteID] = rec
	}
	if rec := got["n-live"]; rec.Outcome != types.SteerUnconfirmed || rec.Phase != types.PhaseExecute ||
		rec.Round != 1 || rec.Boundary != types.SteerBoundaryFinish || rec.Iteration != 4 || rec.LaunchID != launch {
		t.Errorf("the note a live serve handed over was reported %+v, want it unconfirmed at the finish, "+
			"iteration 4, of job %s", rec, launch)
	}
	if rec := got["n-lost"]; rec.Outcome != types.SteerUnconfirmed || rec.Boundary != types.SteerBoundaryToolBatch ||
		rec.Iteration != 2 || rec.LaunchID != launch {
		t.Errorf("the note whose serve a cancel lost was reported %+v, want it unconfirmed at the tool batch, "+
			"iteration 2, of job %s", rec, launch)
	}
	if rec := got["n-absent"]; rec.Outcome != types.SteerMissed || rec.Boundary != types.SteerBoundaryToolBatch ||
		rec.Iteration != 3 || rec.LaunchID != launch {
		t.Errorf("the note whose serve its session proves absent was reported %+v, want it missed at the tool "+
			"batch, iteration 3, of job %s", rec, launch)
	}
	if rec, ok := got["i-1"]; ok || len(got) != 3 {
		t.Errorf("the ending reported %d notes, the job's item among them: %+v", len(got), rec)
	}
}

// A NOTE A HOOK WAS SERVED IS REPORTED AT THAT SERVE, WHATEVER TAKE THAT HANDED
// NOTHING OVER READ IT AFTER. A note a run's hook was served and its job's
// session never confirmed is taken by the review's round of the segment that
// resumes the turn, through a write that lands with its answer lost and a
// settle the store does not answer, and the segment's end marks that take
// unanswered: it never reached its reader, so it handed nothing over, as a
// brief whose job never started hands nothing over. The ending reports the
// note unconfirmed at the step the hook answered, of the job it served and at
// the iteration the serve kept, never at a poll's step.
//
// Mutation: read the serve's liveness off the note with the marked read kept,
// and the case goes red.
func TestAnEndingReportsAHookServedNoteAMarkedTakeReadAtItsServe(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store := sandbox.NewCoordStore(coordmem.NewFleet())
	runs, p := steersOver(t, steerRing, &lostAnswerStore{PendingStore: store, failSettles: 1})
	runs.stopped = ctx.Done()
	launch := workingRun(t, store, runs, "run-1")
	fileSealed(t, runs, "run-1", steer.Note{ID: "n-1", Text: "deploy to prod", By: "founder"})
	if _, err := store.ServeHook(ctx, "run-1", sandbox.HookServe{IDs: []string{"n-1"}, Launch: launch,
		Statuses: steerTakeStatuses, Serve: sandbox.HookServed{Take: "take-1",
			Boundary: string(types.SteerBoundaryToolBatch), Iteration: 3}}); err != nil {
		t.Fatal(err)
	}
	source := claimedSource(t, store, runs, launch)
	if got := source.Take(ctx, reviewRound(1)); len(got) != 0 {
		t.Fatalf("the review's round was handed %+v by a take whose answer and settle were lost", got)
	}
	source.Close(ctx)
	if read := readOn(ctx, store, "run-1", "n-1"); read == nil || !read.Unanswered ||
		read.Boundary != string(types.SteerBoundaryRound) {
		t.Fatalf("the premise: the note is read %+v, want the review's take's read, marked unanswered", read)
	}
	run := mustRow(t, store, "run-1")
	if run.Launch.Iteration == 3 {
		t.Fatal("the premise: the record keeps the serve's own iteration for its job")
	}
	run.Ending = &sandbox.RecordedEnding{ID: "end-1", At: time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)}
	if err := runs.ended(ctx, run); err != nil {
		t.Fatalf("ended: %v", err)
	}
	recs := steered(p)
	if len(recs) != 1 {
		t.Fatalf("the ending reported %+v, want the one note", recs)
	}
	if rec := recs[0]; rec.NoteID != "n-1" || rec.Outcome != types.SteerUnconfirmed ||
		rec.Phase != types.PhaseExecute || rec.Round != 1 || rec.Boundary != types.SteerBoundaryToolBatch ||
		rec.Iteration != 3 || rec.LaunchID != launch {
		t.Errorf("the note a hook served and a marked take read was reported %+v, want it unconfirmed at the "+
			"tool batch, iteration 3, of job %s", rec, launch)
	}
}

// AN ITEM FOR A JOB NEVER LEAVES IT. An item for one job (sandbox.RunNote.For)
// is that job's alone. On an OpenCode job its poll serves it and its
// confirmation reads it, as a person's note; and nothing outside the job takes,
// tells or reports it, whatever became of it: no record says it was delivered,
// the review takes none and ends no round for one, a relaunch's brief tells
// none, the segment resuming the turn carries none and tells none as one its
// session may show, the ending reports none, and the next launch's write
// removes it. On a Claude Code job its hook serves it, and its collection
// reads it where the job's session shows it, with the same silence outside.
//
// Mutation: let any of those readers take, tell, carry, record or report the
// item, or keep it past the next launch, and the case goes red.
func TestAnItemForAJobNeverLeavesIt(t *testing.T) {
	t.Parallel()
	t.Run("an OpenCode job's poll and confirmation", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		fleet := coordmem.NewFleet()
		store := sandbox.NewCoordStore(fleet)
		runs, p := steersOver(t, steerRing, store)
		launch := workingRun(t, store, runs, "run-1")
		// THE ITEMS ON THE RECORD FIRST, so the record of a confirmation that took
		// one would be published ahead of the person's note's.
		placeItemFor(t, runs, fleet, "run-1", "i-read", launch)
		placeItemFor(t, runs, fleet, "run-1", "i-waiting", launch)
		fileSealed(t, runs, "run-1", steer.Note{ID: "n-1", Text: "use staging", By: "founder"})
		inbox := boundInbox(t, runs, store, "run-1", launch)
		viewHolds(t, inbox, 3)
		polled, _ := inbox.Wait(ctx, 0)
		var ids []string
		for _, n := range polled {
			ids = append(ids, n.ID)
		}
		if want := []string{"i-read", "i-waiting", "n-1"}; !slices.Equal(ids, want) {
			t.Fatalf("the job's poll was answered %v, want its own items and the person's note: %v", ids, want)
		}
		if err := inbox.Read(ctx, []string{"i-read", "n-1"}); err != nil {
			t.Fatalf("Read: %v", err)
		}
		if read := readOn(ctx, store, "run-1", "i-read"); read == nil || read.Launch != launch ||
			read.Boundary != string(types.SteerBoundaryStep) {
			t.Fatalf("the job's confirmed item is read %+v, want it read at job %s's step", read, launch)
		}
		eventually(t, "the person's note recorded delivered", func() bool { return len(steered(p)) > 0 })

		source := claimedSource(t, store, runs, launch)
		eventually(t, "the segment's view to hold the waiting item", func() bool {
			view, opened, _ := source.watch.snapshot()
			return view.Found && len(opened) == 1
		})
		if source.Waiting() {
			t.Error("the review reports a note waiting where only the job's item waits")
		}
		if taken := source.Take(ctx, reviewRound(1)); len(taken) != 0 {
			t.Errorf("the review took %+v, want no job's item", taken)
		}
		if brief := source.Brief(ctx); len(brief.Notes) != 0 {
			t.Errorf("a relaunch's brief carries %+v, want no job's item", brief.Notes)
		}
		row := mustRow(t, store, "run-1")
		var carried []string
		for _, r := range runs.carried(row) {
			carried = append(carried, r.Note.ID)
		}
		if !slices.Equal(carried, []string{"n-1"}) {
			t.Errorf("the resumed segment carries %v, want the person's note alone", carried)
		}
		if got := servedNotes(row); !slices.Equal(got, []string{"n-1"}) {
			t.Errorf("a continued session is told %v as notes it may already show, want the person's note alone", got)
		}
		row.Ending = &sandbox.RecordedEnding{ID: "end-1", At: time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)}
		if err := runs.ended(ctx, row); err != nil {
			t.Fatalf("ended: %v", err)
		}
		for _, rec := range steered(p) {
			if rec.NoteID != "n-1" || rec.Outcome != types.SteerDelivered {
				t.Errorf("a record of the turn's notes says %+v, want the person's note delivered alone", rec)
			}
		}
		if _, err := store.BeginLaunch(ctx, sandbox.PendingRun{TurnID: "run-1", AgentID: sweAgent, AgentHandle: "swe",
			Role: "SWE", Harness: "opencode"}, sandbox.Fence{}); err != nil {
			t.Fatalf("relaunch: %v", err)
		}
		var left []string
		for _, n := range mustRow(t, store, "run-1").Steer.Notes {
			left = append(left, n.ID)
		}
		if !slices.Equal(left, []string{"n-1"}) {
			t.Errorf("the next launch left notes %v, want the person's note alone", left)
		}
	})
	t.Run("a Claude Code job's hook and collection", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		fleet := coordmem.NewFleet()
		store := sandbox.NewCoordStore(fleet)
		runs, p := steersOver(t, steerRing, store)
		launch := workingRun(t, store, runs, "run-1")
		placeItemFor(t, runs, fleet, "run-1", "i-held", launch)
		fileSealed(t, runs, "run-1", steer.Note{ID: "n-1", Text: "use staging", By: "founder"})
		inbox := boundInbox(t, runs, store, "run-1", launch)
		answer := inbox.Take(ctx, mcpbridge.SteerEventFinish, hookCall())
		if !strings.Contains(answer, "item for the job i-held") || !strings.Contains(answer, "use staging") {
			t.Fatalf("the job's hook was answered %q, want its own item and the person's note", answer)
		}
		// THE COLLECTION'S VERDICT, as the job's session shows the hook took them.
		confirmed, err := store.SettleHook(ctx, "run-1", sandbox.HookSettle{Launch: launch,
			Held: []string{takeOf(t, answer)}})
		if err != nil {
			t.Fatal(err)
		}
		runs.confirmed(ctx, mustRow(t, store, "run-1"), confirmed)
		eventually(t, "the person's note recorded delivered", func() bool { return len(steered(p)) > 0 })
		source := claimedSource(t, store, runs, launch)
		if taken := source.Take(ctx, reviewRound(1)); len(taken) != 0 {
			t.Errorf("the review took %+v, want nothing", taken)
		}
		if brief := source.Brief(ctx); len(brief.Notes) != 0 {
			t.Errorf("a relaunch's brief carries %+v, want no job's item", brief.Notes)
		}
		row := mustRow(t, store, "run-1")
		var carried []string
		for _, r := range runs.carried(row) {
			carried = append(carried, r.Note.ID)
		}
		if !slices.Equal(carried, []string{"n-1"}) {
			t.Errorf("the resumed segment carries %v, want the person's note alone", carried)
		}
		if got := servedNotes(row); !slices.Equal(got, []string{"n-1"}) {
			t.Errorf("a continued session is told %v as notes it may already show, want the person's note alone",
				got)
		}
		row.Ending = &sandbox.RecordedEnding{ID: "end-1", At: time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)}
		if err := runs.ended(ctx, row); err != nil {
			t.Fatalf("ended: %v", err)
		}
		for _, rec := range steered(p) {
			if rec.NoteID != "n-1" || rec.Outcome != types.SteerDelivered {
				t.Errorf("a record of the turn's notes says %+v, want the person's note delivered alone", rec)
			}
		}
	})
}
