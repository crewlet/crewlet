package engine

import (
	"bytes"
	"context"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/crewlet/crewlet/internal/agent/execstate"
	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/httpx/httpxtest"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// A PARKED CALL'S RESULT REACHES ITS SESSION BY THE RELAUNCH, WITH NO NOTE, for
// a runner that states a ceiling and reads no notes (Cursor Agent's), through
// the real launcher, bridge, ledger, store and runner: whether the model ends
// its turn or works on, its job is parked on the call, the call lands with no
// item, and the relaunch tells the session the call returned, with its result,
// once (internal/api/mcpbridge's "A call longer than its harness waits").
//
// Mutation: launch-names-no-ceiling, and nothing answers before the ceiling;
// Park unset, and the row holds no wait; no WaitsOnCall in fileLateItems, and
// the job is handed an item; Called dropped from runnerAsked, and the relaunch
// tells a person's answer.
func TestAParkedCallsResultReachesItsSessionByTheRelaunchWithNoNote(t *testing.T) {
	t.Parallel()
	for name, worksOn := range map[string]bool{"the model ends its turn": false, "the model works on": true} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			const (
				turnID  = "t-call-ceiling"
				ceiling = 6 * time.Second
			)
			read, list := newHeldTool("read_page"), newHeldTool("list_pages")
			close(list.release)
			rig := bridgedRig(t, turnID, read, list)
			rig.jobs.Ceiling, rig.jobs.TakesNotes = ceiling, false
			run := rig.launch(t, nil)
			var freed sync.Once
			free := func() { freed.Do(func() { close(read.release) }) }
			t.Cleanup(free)

			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			answered := make(chan *mcp.CallToolResult, 1)
			go func() {
				res, err := run.session.CallTool(ctx, &mcp.CallToolParams{Name: "read_page",
					Arguments: map[string]any{}})
				if err != nil {
					res = &mcp.CallToolResult{IsError: true}
				}
				answered <- res
			}()
			select {
			case <-read.entered:
			case <-ctx.Done():
				t.Fatal("the call never reached its tool")
			}
			select {
			case res := <-answered:
				if res.IsError || len(res.Content) != 1 ||
					!strings.Contains(res.Content[0].(*mcp.TextContent).Text, "END YOUR TURN NOW") {
					t.Fatalf("the call was answered %+v while its tool ran, want it still running and the "+
						"turn to end, not as failed", res)
				}
			case <-ctx.Done():
				t.Fatalf("the call was not answered while its tool ran, %v before its harness's ceiling of %v",
					ceiling-toolloop.ParkBefore(ceiling), ceiling)
			}
			row := run.row(t, turnID)
			if !row.WaitsOnOwnCalls() || len(row.ParkedCalls) != 1 || row.ParkedCalls[0].Tool != "read_page" ||
				row.ParkedCalls[0].Seq != 1 || row.Question != sandbox.CallWaitQuestion("read_page") ||
				!row.AskedBy(row.LaunchID) {
				t.Fatalf("the run's row parks on %+v asking %q, want its job parked on its call to read_page",
					row.ParkedCalls, row.Question)
			}
			if worksOn {
				if res, err := run.session.CallTool(ctx, &mcp.CallToolParams{Name: "list_pages",
					Arguments: map[string]any{}}); err != nil || res.IsError {
					t.Fatalf("a call made after the park was answered %+v, %v, want served", res, err)
				}
			}

			// THE CALL LANDS AND THE JOB ENDS: its session on the bridge ends, and
			// the bridge waits for the call it runs to be recorded.
			free()
			run.engine.bridge.Close(turnID)
			run.engine.bridge.AwaitCalls(ctx)
			logged := run.calls(t, turnID)
			i := len(logged.Calls) - 1
			if i < 0 || logged.Calls[i].Name != "read_page" || !logged.Calls[i].Abandoned ||
				logged.Calls[i].Output != "read_page done" {
				t.Fatalf("the run's log holds %+v, want the parked call with what its tool did, as one "+
					"nobody waited for", logged.Calls)
			}
			row = run.row(t, turnID)
			for _, n := range row.Steer.Notes {
				if n.LateCall != nil {
					t.Errorf("the job parked on its call is handed it as an item: %+v", n)
				}
			}
			if !row.WaitsOnCall() {
				t.Fatalf("the run's row no longer waits on the call once it landed: %+v", row.WaitsFor)
			}

			// THE PARK IS ANSWERED, and the relaunch it carries continues the job's
			// own copy, its harness having ended its own run.
			relaunchTellsTheCallOnce(t, run, turnID, row)
		})
	}
}

// relaunchTellsTheCallOnce answers the park of turnID's run on its call to
// read_page, row as the park left it, and holds the relaunch the answer
// carries, continuing the job's own copy, to telling the session the call
// returned, with what it returned, once. Under the test's own context, as the
// reads and the relaunch it makes through the rig's helpers are.
func relaunchTellsTheCallOnce(t *testing.T, run bridgedRun, turnID string, row sandbox.PendingRun) {
	t.Helper()
	ctx := t.Context()
	if err := run.pending.MarkAwaiting(ctx, turnID, sandbox.Clarification{AskedAt: time.Now().UTC()}); err != nil {
		t.Fatalf("MarkAwaiting: %v", err)
	}
	if _, ok, err := run.pending.RecordAnswer(ctx, turnID, row.LaunchID, sandbox.RecordedAnswer{
		Sealed: []byte("sealed: returned"), Via: types.AnswerViaOperator, EventIDs: []string{"r1"}},
		sandbox.Fence{}); err != nil || !ok {
		t.Fatalf("RecordAnswer = %v, %v", ok, err)
	}
	if _, ok, err := run.pending.ClaimForResume(ctx, turnID, sandbox.RecordedAnswerTail(row.LaunchID),
		sandbox.Fence{}); err != nil || !ok {
		t.Fatalf("ClaimForResume = %v, %v", ok, err)
	}
	logged := run.calls(t, turnID)
	resumed := run.row(t, turnID)
	resumed.Launch.HarnessEnd = sandbox.HarnessEndOwnRun
	collectedFarBelow(&resumed.Launch)
	brief := relaunchedInPlace(t, run, runnerResume(resumeInput{Run: resumed, Bridged: logged,
		BridgedRead: logged.Read(), State: execstate.State{Version: execstate.Version, Round: 1},
		Asked: &sandbox.AskedRun{Question: row.Question, Answer: "Your call to read_page returned.",
			SessionID: "sess-ask", BoxKept: true,
			Called: []types.CallReturned{{Tool: "read_page", Seq: 1}}, CallsReturned: true}}, nil, nil, nil))
	if !strings.Contains(brief, "## Your call returned") || strings.Count(brief, "read_page done") != 1 ||
		strings.Contains(brief, "answered:") {
		t.Errorf("the relaunch does not tell the session its call returned, with what it returned, "+
			"once:\n%s", brief)
	}
}

// parkHeldStore is a run store that holds a park on a job's own call
// ([sandbox.Ask.Call]) until released, and says when the record of a call to
// tool has landed on it.
type parkHeldStore struct {
	sandbox.PendingStore
	tool      string
	asked     chan struct{}
	release   chan struct{}
	askedOnce sync.Once
	recorded  atomic.Bool
}

func (s *parkHeldStore) RecordAsk(ctx context.Context, turnID, launch string, ask sandbox.Ask,
) (sandbox.AskOutcome, error) {
	if ask.Call != nil {
		s.askedOnce.Do(func() { close(s.asked) })
		select {
		case <-s.release:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	return s.PendingStore.RecordAsk(ctx, turnID, launch, ask)
}

func (s *parkHeldStore) AppendBridgeCall(ctx context.Context, turnID string, a sandbox.BridgeAppend,
) (bool, error) {
	ok, err := s.PendingStore.AppendBridgeCall(ctx, turnID, a)
	if a.Step == sandbox.AppendRecord && a.Call.Name == s.tool {
		s.recorded.Store(true)
	}
	return ok, err
}

// recordWaitsOnAParksMark reports whether a bridged call's record step waits on
// its park's mark on the run's row, read off the process's goroutines: one
// receiving in internal/api/mcpbridge's binding.claim, the one receive there.
// Another case's call matching it only frees a hold early, which a build whose
// record does not wait passes no sooner: that build has no receive there.
func recordWaitsOnAParksMark() bool {
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			buf = buf[:n]
			break
		}
		buf = make([]byte, 2*len(buf))
	}
	for _, g := range bytes.Split(buf, []byte("\n\n")) {
		header, frames, _ := bytes.Cut(g, []byte("\n"))
		top, _, _ := bytes.Cut(frames, []byte("\n"))
		if bytes.Contains(header, []byte("[chan receive")) &&
			bytes.Contains(top, []byte("internal/api/mcpbridge.(*binding).claim(")) {
			return true
		}
	}
	return false
}

// A PARKED CALL'S RECORD WAITS FOR ITS PARK'S MARK ON THE ROW, so its job is
// told the call once. A tool can return while its park is still being written:
// recorded first, it is a call nobody waited for on a row waiting on no call,
// which the store hands the job as an item, and the relaunch the park carries
// tells it again. Through the real bridge and store, the park's row write held
// until the tool returned and its record landed or waits on the mark.
//
// Mutation: drop the record's wait on the mark (binding.claim's <-marked in
// internal/api/mcpbridge), and the job is handed the call as an item.
func TestAParkedCallsRecordWaitsForItsParksMarkOnTheRow(t *testing.T) {
	t.Parallel()
	const (
		turnID  = "t-call-mark"
		ceiling = 6 * time.Second
	)
	read := newHeldTool("read_page")
	rig := bridgedRig(t, turnID, read)
	rig.jobs.Ceiling, rig.jobs.TakesNotes = ceiling, false
	// THE LAUNCH'S STORE, so the park its session makes and the record its
	// ledger writes both go through the hold.
	rt := rig.engine.sandbox.Load()
	store := &parkHeldStore{PendingStore: rt.pending, tool: "read_page",
		asked: make(chan struct{}), release: make(chan struct{})}
	rig.engine.useSandbox(store, rt.coordinator)
	run := rig.launch(t, nil)
	var freed, let sync.Once
	free := func() { freed.Do(func() { close(read.release) }) }
	letGo := func() { let.Do(func() { close(store.release) }) }
	t.Cleanup(free)
	t.Cleanup(letGo)

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	answered := make(chan *mcp.CallToolResult, 1)
	go func() {
		res, err := run.session.CallTool(ctx, &mcp.CallToolParams{Name: "read_page",
			Arguments: map[string]any{}})
		if err != nil {
			res = &mcp.CallToolResult{IsError: true}
		}
		answered <- res
	}()
	for what, reached := range map[string]chan struct{}{"its tool": read.entered, "its park": store.asked} {
		select {
		case <-reached:
		case <-ctx.Done():
			t.Fatalf("the call never reached %s", what)
		}
	}

	// THE TOOL RETURNS WHILE THE PARK IS WRITTEN, and the hold lasts until its
	// record has landed or waits on the park's mark.
	free()
	waitFor(t, "the parked call's record to land or wait on its park's mark", func() bool {
		return store.recorded.Load() || recordWaitsOnAParksMark()
	})
	letGo()
	select {
	case res := <-answered:
		if res.IsError || !strings.Contains(contentText(res), "END YOUR TURN NOW") {
			t.Errorf("the call was answered %+v, want it parked and the turn to end", res)
		}
	case <-ctx.Done():
		t.Fatal("the call was never answered")
	}
	run.engine.bridge.Close(turnID)
	run.engine.bridge.AwaitCalls(ctx)
	logged := run.calls(t, turnID)
	if len(logged.Calls) != 1 || logged.Calls[0].Name != "read_page" || !logged.Calls[0].Abandoned ||
		logged.Calls[0].Output != "read_page done" {
		t.Fatalf("the run's log holds %+v, want the parked call with what its tool did, as one nobody "+
			"waited for", logged.Calls)
	}
	row := run.row(t, turnID)
	for _, n := range row.Steer.Notes {
		if n.LateCall != nil {
			t.Errorf("the job parked on its call is handed it as an item too: %+v", n.LateCall)
		}
	}
	if !row.WaitsOnOwnCalls() || len(row.ParkedCalls) != 1 || row.ParkedCalls[0].Tool != "read_page" {
		t.Fatalf("the run's row parks on %+v, want its job parked on its call to read_page", row.ParkedCalls)
	}
	relaunchTellsTheCallOnce(t, run, turnID, row)
}

// A RUN PARKED BEFORE ITS CEILING CARRIES ON UNDER ITS NEW JOB'S TOKEN ALONE. A
// job's token has no expiry: its live opening decides (internal/api/mcpbridge,
// "A job's token ends with its opening"). So the box parked on its call, which
// still holds its token, must reach nothing once its job is collected, and the
// park's relaunch is a job with an opening of its own. Through the real
// launcher, bridge, store and coordinator: the engine answers the park, the
// seat's holder resumes the run with the answer it recorded, and the
// relaunch's brief tells the parked call's result once.
//
// Mutation: keep the parked job's opening live after Bridge.Close, or
// resolve a token by its run alone, and its token is served; drop Called
// from internal/sandbox's askedRun, and the relaunch does not tell the call.
func TestARunParkedBeforeItsCeilingCarriesOnUnderItsNewJobsTokenAlone(t *testing.T) {
	t.Parallel()
	const (
		turnID  = "t-call-token"
		ceiling = 6 * time.Second
	)
	read, list := newHeldTool("read_page"), newHeldTool("list_pages")
	close(list.release)
	rig := bridgedRig(t, turnID, read, list)
	rig.jobs.Ceiling, rig.jobs.TakesNotes = ceiling, false
	// THE SEAT HOLDER'S COORDINATOR launches the run and resumes its answered
	// park into the case, which relaunches it as the resumed turn would.
	e, rt := rig.engine, rig.engine.sandbox.Load()
	resumes := &handingResumer{handed: make(chan handedResume), letGo: make(chan struct{})}
	holder, err := sandbox.NewCoordinator(sandbox.CoordinatorOptions{
		Answers: testAnswers(), Leads: testLeads(), Pushes: noPushes{}, Audience: noAudience{},
		Queue: e.backends.Queue, Pending: rt.pending, Manager: rt.coordinator.Manager(), Resume: resumes,
		SeatCap: e.seatRunCap,
	})
	if err != nil {
		t.Fatalf("NewCoordinator: %v", err)
	}
	t.Cleanup(holder.Stop)
	var let, freed sync.Once
	letGo := func() { let.Do(func() { close(resumes.letGo) }) }
	free := func() { freed.Do(func() { close(read.release) }) }
	t.Cleanup(letGo)
	t.Cleanup(free)
	e.useSandbox(rt.pending, holder)
	// THE ENGINE'S ANSWER DESK, its seat's control topic delivering to the holder.
	e.runWaits.deliver = func(ctx context.Context, parked sandbox.PendingRun) error {
		return sandbox.AnswerDesk{Pending: rt.pending, Queue: controlTopic{holder: holder},
			Seal: testAnswers()}.DeliverWaitOver(ctx, parked)
	}
	run := rig.launch(t, nil)
	parkedAt := run.jobs.Started()[0].MCPServers[config.ToolServerName].URL
	// AND THE LAUNCHING TURN PARKS, its state on the row for the resume.
	state := execstate.State{Version: execstate.Version, Round: 1}
	blob, err := execstate.Encode(state)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if ok, err := run.pending.MarkSuspended(t.Context(), turnID, sandbox.Suspension{State: blob,
		Iteration: state.Round}); err != nil || !ok {
		t.Fatalf("MarkSuspended = %v, %v", ok, err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	answered := make(chan *mcp.CallToolResult, 1)
	go func() {
		res, err := run.session.CallTool(ctx, &mcp.CallToolParams{Name: "read_page",
			Arguments: map[string]any{}})
		if err != nil {
			res = &mcp.CallToolResult{IsError: true}
		}
		answered <- res
	}()
	select {
	case <-read.entered:
	case <-ctx.Done():
		t.Fatal("the call never reached its tool")
	}
	select {
	case res := <-answered:
		if res.IsError || !strings.Contains(contentText(res), "END YOUR TURN NOW") {
			t.Fatalf("the call was answered %+v while its tool ran, want it parked and the turn to end", res)
		}
	case <-ctx.Done():
		t.Fatal("the call was not answered before its harness's ceiling")
	}
	parked := run.row(t, turnID)
	if !parked.WaitsOnOwnCalls() || len(parked.ParkedCalls) != 1 || parked.ParkedCalls[0].Tool != "read_page" {
		t.Fatalf("the run's row parks on %+v, want its job parked on its call to read_page", parked.ParkedCalls)
	}

	// THE JOB ENDS: its collection closes its opening, the tool returns and is
	// recorded, and the run parks.
	run.engine.bridge.Close(turnID)
	free()
	run.engine.bridge.AwaitCalls(ctx)
	if err := run.pending.MarkAwaiting(ctx, turnID, sandbox.Clarification{AskedAt: time.Now().UTC()}); err != nil {
		t.Fatalf("MarkAwaiting: %v", err)
	}

	// THE ENGINE WAKES THE PARK, and the holder carries the run on from its
	// row: a turn that begins, then relaunches the round as a new job.
	waited := make(chan error, 1)
	go func() {
		_, err := e.answerWaits(ctx, run.handle)
		waited <- err
	}()
	var resume handedResume
	select {
	case resume = <-resumes.handed:
	case <-ctx.Done():
		t.Fatal("the answered park was never resumed")
	}
	if begin := resume.req.Begin; begin != nil {
		if err := begin(resume.ctx, sandbox.ResumeCarriesOn); err != nil {
			t.Fatalf("Begin: %v", err)
		}
	}
	resumed := resume.req.Run
	resumed.Launch.HarnessEnd = sandbox.HarnessEndOwnRun
	collectedFarBelow(&resumed.Launch)
	carried := run.launch(t, relaunchedContinuation(t, run, runnerResume(resumeInput{Run: resumed,
		Bridged: resume.req.Bridged, BridgedRead: resume.req.BridgedRead,
		State: state, Asked: resume.req.Asked}, nil, nil, nil)))
	started, next := carried.jobs.Started(), carried.row(t, turnID)
	if len(started) != 2 || next.LaunchID == parked.LaunchID ||
		started[1].MCPServers[config.ToolServerName].URL == parkedAt || started[1].Resume == "" {
		t.Fatalf("the relaunch is job %s after %d launches, continuing session %q, want a new job dialled "+
			"at an endpoint of its own, continuing the parked job's session", next.LaunchID, len(started),
			started[len(started)-1].Resume)
	}
	if brief := started[1].Brief; !strings.Contains(brief, "## Your call returned") ||
		strings.Count(brief, "read_page done") != 1 || strings.Contains(brief, "answered:") {
		t.Errorf("the relaunch does not tell the session its call returned, with what it returned, "+
			"once:\n%s", brief)
	}

	// THE NEW JOB'S TOKEN IS SERVED, the parked job's refused, on its own
	// session and dialled afresh.
	if res, err := carried.session.CallTool(ctx, &mcp.CallToolParams{Name: "list_pages",
		Arguments: map[string]any{}}); err != nil || res.IsError {
		t.Errorf("the relaunched job's call was answered %+v, %v, want served", res, err)
	}
	refused := func(when string, session *mcp.ClientSession) {
		t.Helper()
		res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "list_pages", Arguments: map[string]any{}})
		if err == nil && !res.IsError {
			t.Errorf("%s, the parked job's call was answered %+v, want it refused", when, res.Content)
		}
	}
	refused("on its own session", run.session)
	redialled, err := mcp.NewClient(&mcp.Implementation{Name: "coding-agent", Version: "1"}, nil).
		Connect(ctx, &mcp.StreamableClientTransport{Endpoint: parkedAt, HTTPClient: httpxtest.Pool(t)}, nil)
	if err == nil {
		t.Cleanup(func() { _ = redialled.Close() })
		refused("dialled again", redialled)
	}

	letGo()
	select {
	case err := <-waited:
		if err != nil {
			t.Errorf("answering the park: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("the park's answer never returned")
	}
}

// handedResume is a resume a coordinator handed its resumer, with the context
// it runs under.
type handedResume struct {
	ctx context.Context
	req sandbox.ResumeRequest
}

// handingResumer is a [sandbox.Resumer] that hands each resume to the case and
// holds it until let go, so the case relaunches the run inside the resume, as
// the resumed turn does.
type handingResumer struct {
	handed chan handedResume
	letGo  chan struct{}
}

func (r *handingResumer) Resume(ctx context.Context, req sandbox.ResumeRequest) error {
	select {
	case r.handed <- handedResume{ctx: ctx, req: req}:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-r.letGo:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// controlTopic is a seat's control topic delivering to its holder's
// coordinator, as the holder's subscription does ([sandbox.Coordinator.OnEvent]).
type controlTopic struct{ holder *sandbox.Coordinator }

func (c controlTopic) Publish(ctx context.Context, _ string, ev *events.Event) error {
	return c.holder.OnEvent(ctx, ev)
}
