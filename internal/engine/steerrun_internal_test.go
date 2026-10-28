package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/agent/steer"
	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/httpx/httpxtest"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/llmtest"
	"github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/codingagent"
)

// A person's note to a turn whose coding run is out, through the real turn: the
// launch, the run's bridge, a go-sdk client playing the run's CLI, and the
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
				return phasedCLI{model: "scripted", pick: func([]string) llmtest.Run {
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

// steerCase is a seat whose turn parks on its coding run, steerable, played by
// a CLI that does pull with the run's bridge before its script.
type steerCase struct {
	e     *Engine
	rig   *runRig
	q     *memory.Queue
	model *reviewWatch

	// dial, when set, is the address the CLI dials instead of the one it
	// was launched with: its node restarted under it.
	mu   sync.Mutex
	dial string
}

// pull is what a run's CLI does with its bridge before its script: its hooks
// and its plugin, played by a case.
type pull func(ctx context.Context, session *mcp.ClientSession, row func() sandbox.PendingRun)

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
			before(ctx, session, func() sandbox.PendingRun {
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

// awaitSteered waits until the queue has carried n records of what became of a
// note, and answers them.
func (c *steerCase) awaitSteered(t *testing.T, n int) []types.AgentTurnSteered {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for len(c.steered()) < n && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	var out []types.AgentTurnSteered
	for _, ev := range c.steered() {
		rec, _ := events.DataAs[*types.AgentTurnSteered](ev)
		out = append(out, *rec)
	}
	if len(out) < n {
		t.Fatalf("%d records of a note were published, want %d", len(out), n)
	}
	return out
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
// again until it answers or its deadline passes — as the CLI asks at every
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
// context, in the shape the CLI reads — and the note is read there, by this
// job, at that kind of step.
func TestSteerTakeAnswersTheHookWithTheNote(t *testing.T) {
	t.Parallel()
	var answer string
	var takeErr error
	c := newSteerCase(t, func(ctx context.Context, s *mcp.ClientSession, _ func() sandbox.PendingRun) {
		answer, takeErr = batchTake(ctx, s)
	})
	parkOnARun(t, c.e, c.rig, "run-1")
	c.file(t, "run-1", "req-1", "use staging")
	launch := c.rig.row(t, "run-1").LaunchID
	if err := c.rig.collect(t, "run-1"); err != nil {
		t.Fatalf("collect: %v", err)
	}
	if takeErr != nil {
		t.Fatalf("steer_take: %v", takeErr)
	}
	if !strings.Contains(answer, `"hookEventName":"PostToolBatch"`) ||
		!strings.Contains(answer, `"additionalContext":"## A note from founder`) ||
		!strings.Contains(answer, "use staging") {
		t.Fatalf("the hook was answered %q, want the note as its additional context", answer)
	}
	got := c.awaitSteered(t, 1)[0]
	if got.Outcome != types.SteerDelivered || got.Phase != types.PhaseExecute ||
		got.Boundary != types.SteerBoundaryToolBatch || got.LaunchID != launch || got.Round != 1 {
		t.Errorf("the note was recorded %+v, want delivered in execute at a tool batch of job %s",
			got, launch)
	}
}

// A TAKEN NOTE IS RECORDED DELIVERED ONCE: the hook that took it is the only
// step that reads it — the run's later hooks take nothing, and the reviewer,
// which reads every note the turn has read, records none of them again.
func TestATakenNoteIsRecordedDeliveredOnce(t *testing.T) {
	t.Parallel()
	var again string
	c := newSteerCase(t, func(ctx context.Context, s *mcp.ClientSession, _ func() sandbox.PendingRun) {
		if _, err := batchTake(ctx, s); err != nil {
			return
		}
		again, _, _ = takeAt(ctx, s, map[string]any{"event": "Stop", "agent": ""})
	})
	parkOnARun(t, c.e, c.rig, "run-1")
	c.file(t, "run-1", "req-1", "use staging")
	if err := c.rig.collect(t, "run-1"); err != nil {
		t.Fatalf("collect: %v", err)
	}
	if again != "" {
		t.Errorf("the run's finish took the note again: %q", again)
	}
	c.awaitSteered(t, 1)
	time.Sleep(200 * time.Millisecond)
	if got := c.steered(); len(got) != 1 {
		t.Errorf("%d records of the note were published, want one", len(got))
	}
}

// THE REVIEWER OPENS WITH A NOTE THE RUN TOOK: the run's read is on its record,
// so the segment that reviews the run carries it — the reviewer judges the
// work against the note the work was steered by.
func TestTheReviewerOpensWithANoteTheRunTook(t *testing.T) {
	t.Parallel()
	c := newSteerCase(t, func(ctx context.Context, s *mcp.ClientSession, _ func() sandbox.PendingRun) {
		_, _ = batchTake(ctx, s)
	})
	parkOnARun(t, c.e, c.rig, "run-1")
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

// A SUBAGENT'S BATCH TAKES NOTHING, and neither does a hook whose agent the CLI
// did not fill in: a note handed to a delegated loop binds that piece of work
// alone and is lost with it. Both leave the note waiting, for the main agent's
// next boundary or — as here, where it has none — the reviewer.
func TestASubagentsBatchTakesNothing(t *testing.T) {
	t.Parallel()
	var answers []string
	var waiting []string
	c := newSteerCase(t, func(ctx context.Context, s *mcp.ClientSession, row func() sandbox.PendingRun) {
		time.Sleep(200 * time.Millisecond)
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

// STEER_TAKE IS NEVER LISTED: the CLI's model is offered the seat's tools and
// never the hook's, which only the CLI calls.
func TestSteerTakeIsNeverListed(t *testing.T) {
	t.Parallel()
	var listed []string
	c := newSteerCase(t, func(ctx context.Context, s *mcp.ClientSession, _ func() sandbox.PendingRun) {
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

// A NOTE THE RUN NEVER PULLED IS READ BY THE REVIEWER, at its first round: a CLI
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
// was opened again after a restart reaches the run's CLI there — and a note the
// run read before the restart is still the turn's, carried into its review.
func TestARunIsSteerableAfterItsLaunchingNodeRestarts(t *testing.T) {
	t.Parallel()
	var answer string
	c := newSteerCase(t, func(ctx context.Context, s *mcp.ClientSession, _ func() sandbox.PendingRun) {
		answer, _, _ = takeAt(ctx, s, map[string]any{"event": "Stop", "agent": ""})
	})
	endpoint := parkOnARun(t, c.e, c.rig, "run-1")
	row := c.rig.row(t, "run-1")

	// BEFORE THE RESTART the run reads one note at a batch.
	c.file(t, "run-1", "req-before", "use the staging database")
	before := c.e.runSteers.inbox(identityOfRun(row), 1, row.CodingAgent)
	before.Bind(t.Context(), row.LaunchID)
	if got := before.Take(t.Context(), mcpbridge.SteerEventFinish); !strings.Contains(got, "staging") {
		t.Fatalf("the run did not read its first note before the restart: %q", got)
	}
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
		t.Errorf("the run's CLI was not handed the note after the restart: %q", answer)
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

// A STORE FAILURE ANSWERS EMPTY, NEVER AN ERROR: a hook whose call failed makes
// Claude Code drop and re-initialise the run's whole bridge, so a take the
// record cannot serve is answered with nothing, the note waits for the next
// step, and a read that could not be written is the poller's to retry.
func TestAStoreFailureAnswersEmptyNeverAnError(t *testing.T) {
	t.Parallel()
	store := sandbox.NewCoordStore(coordmem.NewFleet())
	runs, _ := steersOver(t, steerRing, store)
	launch := workingRun(t, store, runs, "run-1")
	fileSealed(t, runs, "run-1", steer.Note{ID: "n-1", Text: "use staging", By: "founder"})
	broken, _ := steersOver(t, steerRing, failingStore{PendingStore: store, reads: true})
	inbox := boundInbox(t, broken, store, "run-1", launch)
	for _, event := range []string{mcpbridge.SteerEventToolBatch, mcpbridge.SteerEventFinish} {
		if got := inbox.Take(t.Context(), event); got != "" {
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
	if got := inbox.Take(t.Context(), mcpbridge.SteerEventToolBatch); got != "" {
		t.Fatalf("a batch answered %q from a view that never heard the record", got)
	}
	if got := inbox.Take(t.Context(), mcpbridge.SteerEventFinish); !strings.Contains(got, "use staging") {
		t.Errorf("the finish answered %q, want the note the record holds", got)
	}
}

// A RUN PARKED ON ITS QUESTION HOLDS ITS NOTES FOR WHAT RESUMES IT: a note to it
// is read after the answer — so steer_turn says — its CLI takes nothing while it
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
		CodingAgent: "claude-code", PauseTTLSec: 1800}, sandbox.Fence{}); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkAwaiting(ctx, "run-1", sandbox.Clarification{Question: "which branch?",
		Audience: "requester", Branch: "wip/run-1", AskedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	filed, err := runs.File(ctx, "run-1", steer.Note{ID: "n-parked", Text: "and tag it", By: "founder"})
	if err != nil || filed.Status != steer.StatusAccepted || filed.Reads != steer.ReadsAfterAnswer {
		t.Fatalf("a note to the parked run = %+v, %v, want accepted and read after the answer", filed, err)
	}
	if got := inbox.Take(ctx, mcpbridge.SteerEventFinish); got != "" {
		t.Errorf("the parked run's CLI took %q", got)
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
	t.Cleanup(source.Close)
	brief := source.Brief(ctx)
	if len(brief) != 2 || brief[0].ID != "n-before" || brief[1].ID != "n-parked" {
		t.Fatalf("the relaunch's brief carries %+v, want both notes, oldest first", brief)
	}
	sealed, err := runs.briefNotes("run-1", 2, brief)
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
		if err := runs.expired(ctx, run); err != nil {
			t.Fatalf("expired: %v", err)
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
	if err := runs.expired(ctx, other); err != nil {
		t.Fatal(err)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.events[2].ID == published[0].ID {
		t.Error("a different ending of the turn published under the same id")
	}
}

// A NOTE LEFT WAITING WHEN A RUN ENDS IS RECORDED EXPIRED BY THE ENGINE'S OWN
// COORDINATOR. The engine builds the coordinator that ends its runs
// (sandboxRuntimeOn) and hands it the step that records the run's waiting
// notes before the record goes ([sandbox.CoordinatorOptions.SteersExpired]);
// nothing else does it, and nothing else holds the note once the record is
// deleted. A person who sent one and was told it waits learns what became of
// it from that record alone. Here a person's stop ends the run its turn is
// parked on, through the engine's own pause path, with a note filed on the
// record that no step of the run took: one record of it, `expired`, at the
// ending's instant — the run's stop announcement's — and none `delivered`.
//
// Mutation: drop SteersExpired from the engine's coordinator options, and no
// record of the note is published.
func TestANoteLeftWaitingWhenARunEndsIsRecordedExpiredByTheEnginesCoordinator(t *testing.T) {
	t.Parallel()
	c := newSteerCase(t, nil)
	parkOnARun(t, c.e, c.rig, "run-1")
	c.file(t, "run-1", "req-1", "use the staging database")

	pause := janeStops()
	c.e.applySeatPause(t.Context(), coord.SeatPauseUpdate{Seat: pause.Seat, Pause: &pause})
	if _, found, _ := c.e.sandbox.Load().pending.Get(t.Context(), "run-1"); found {
		t.Fatal("the stopped run is still on the record")
	}

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
// was taken was sent before any note a person files on the record once the
// launch has written one — and the moment that write lands, any node's
// steer_turn finds the record and files on it directly. So the box's note rides
// the write that creates the record, ahead of everything: every reader of a
// run's notes takes in the record's order, and where two notes disagree the
// later word is the one a model is told to keep. Handed over by the launch once
// the record existed, it was filed behind a note filed on the record in that
// window, and read after it.
//
// Mutation: hand the box over after the launch's write, as it was, and the note
// filed as the record is written is first.
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
	e, _ := indicatingWith(t, notify.StatusAlways, model)
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
		seen.notes[0].Inbox != codingagent.OpenCodeInboxID(launch, "req-1") ||
		!strings.Contains(seen.notes[0].Message, "use the staging database") {
		t.Fatalf("the poll answered %+v, want the note under the id job %s admits it as", seen.notes, launch)
	}
	if seen.read == nil || seen.read.Boundary != string(types.SteerBoundaryStep) || seen.read.Launch != launch ||
		seen.read.Phase != string(types.PhaseExecute) {
		t.Errorf("the record holds the note read %+v, want read at boundary step by job %s", seen.read, launch)
	}
	var delivered []types.AgentTurnSteered
	for _, rec := range c.awaitSteered(t, 1) {
		if rec.Outcome == types.SteerDelivered {
			delivered = append(delivered, rec)
		}
	}
	time.Sleep(200 * time.Millisecond)
	if all := c.steered(); len(all) != 1 {
		t.Errorf("%d records of the note were published, want one", len(all))
	}
	if len(delivered) != 1 || delivered[0].Boundary != types.SteerBoundaryStep || delivered[0].LaunchID != launch {
		t.Errorf("the note was recorded %+v, want delivered once at a step of job %s", delivered, launch)
	}
}

// pollAndConfirm is a run's notes plugin, once: a poll of the route its launch
// handed it, and a read of every note the poll answered.
func pollAndConfirm(ctx context.Context, client *http.Client, route string, seen *polledNote) error {
	if route == "" {
		return errors.New("the run was handed no notes route")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, route+"/notes?after=0", nil)
	if err != nil {
		return err
	}
	res, err := client.Do(req)
	if err != nil {
		return err
	}
	var body struct {
		Notes []mcpbridge.SteerNote `json:"notes"`
	}
	err = json.NewDecoder(res.Body).Decode(&body)
	_ = res.Body.Close()
	if err != nil {
		return fmt.Errorf("the poll's answer (%d): %w", res.StatusCode, err)
	}
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("the poll answered %d", res.StatusCode)
	}
	seen.notes = body.Notes
	ids := make([]string, 0, len(body.Notes))
	for _, n := range body.Notes {
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
// open on it has handed its follower a view holding one note: the moment the
// follower's view — what the tool loop's Waiting reads — has it too.
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
// WITH IT. The reviewer of a turn resumed from its coding run is a run of the
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
	cli := phasedCLI{model: "scripted", pick: pick}
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
