package engine

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/config"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/httpx/httpxtest"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/slack"
	"github.com/crewlet/crewlet/internal/tools"
)

// The working indicator, end to end from the frame that owns a turn.
//
// THROUGH A REAL TRANSPORT, deliberately. The defect these cases exist for was
// not a broken driver — both backends' posters were complete and tested — it
// was that nothing in production ever called one, so every test in the tree
// passed while no agent had ever raised an indicator on a running company.
// Asserting against the notify layer alone would reproduce exactly that: the
// only assertion that can catch it is one that starts at [Engine.runTurn] and
// ends at what a chat backend was asked to do.

// workspace is a Slack that records the statuses it was asked to show.
type workspace struct {
	*httptest.Server

	mu       sync.Mutex
	statuses []string
}

func newWorkspace(t *testing.T) *workspace {
	t.Helper()
	ws := &workspace{}
	ws.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method := strings.TrimPrefix(r.URL.Path, "/api/")
		if method == "assistant.threads.setStatus" {
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			status, _ := body["status"].(string)
			ws.mu.Lock()
			ws.statuses = append(ws.statuses, status)
			ws.mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		// One identity for auth.test and ok for everything else: the thread
		// read a turn makes on this token is answered with an empty thread.
		_, _ = w.Write([]byte(`{"ok":true,"user_id":"U0BOT","team_id":"T0ACME"}`))
	}))
	t.Cleanup(ws.Close)
	return ws
}

// shown is what the workspace was asked to display, raises and clears alike —
// Slack clears by setting an EMPTY status, so the empty string in this list is
// the indicator coming down.
func (w *workspace) shown() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.statuses...)
}

// raises counts the non-empty statuses: how many times an agent was announced
// as working, as opposed to cleared.
func (w *workspace) raises() int {
	var n int
	for _, s := range w.shown() {
		if s != "" {
			n++
		}
	}
	return n
}

// shownAtLeast waits until the workspace has been asked for n statuses.
//
// WAITED FOR, because no post is made by the goroutine that asked for one: a
// raise and a phase change are both on a turn's critical path, so they wake the
// session's own goroutine instead of calling a chat backend inline. A case that
// read straight after the turn would be asserting the opposite property.
func (w *workspace) shownAtLeast(t *testing.T, n int) []string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if shown := w.shown(); len(shown) >= n {
			return shown
		}
		if time.Now().After(deadline) {
			t.Fatalf("the workspace was asked for %v in two seconds, want %d statuses",
				w.shown(), n)
		}
		time.Sleep(time.Millisecond)
	}
}

// raisedNothing asserts the deterministic half of "no indicator": a session is
// created by Begin itself, so a driver holding none can never post — where an
// empty request log alone could be a post still in flight.
func (e *Engine) raisedNothing(t *testing.T, ws *workspace) {
	t.Helper()
	if live := e.notify.slack.Status().Live(); len(live) != 0 {
		t.Fatalf("an indicator was raised in %v", live)
	}
	if got := ws.shown(); len(got) != 0 {
		t.Fatalf("the workspace was told %v", got)
	}
}

// refusingProvider stands in for the seat's model. No case here reaches it:
// every turn below ends on a guard that costs no provider call, and a refusal
// rather than a canned answer is what says so out loud.
type refusingProvider struct{}

func (refusingProvider) Model() string { return "refusing" }
func (refusingProvider) Complete(context.Context, llm.Request) (*llm.Completion, error) {
	return nil, &llm.Error{Kind: llm.KindFatal, Provider: "test", Model: "refusing"}
}

// scripted answers each phase with its own submission, chosen from the tools
// the request offers — the phases are what the offered tools distinguish, and a
// flat sequence would drift the moment one phase took a round more.
type scripted struct{}

func (scripted) Model() string { return "scripted" }
func (scripted) Complete(_ context.Context, req llm.Request) (*llm.Completion, error) {
	name, args := runner.SubmitWorkTool, map[string]any{
		"outcome": "blocked", "summary": "nothing to do here",
		"evidence": "this seat holds no write tool",
	}
	for _, tool := range req.Tools {
		if tool.Name == runner.SubmitReviewTool {
			name, args = runner.SubmitReviewTool, map[string]any{"decision": "done"}
		}
	}
	return &llm.Completion{
		ToolCalls: []llm.ToolCall{{ID: "c1", Name: name, Arguments: args}},
	}, nil
}

// indicating builds an engine whose one seat is on a Slack workspace, with a
// company that REFUSES A TURN AT ITS DEPTH GUARD.
//
// The guard is the fixture's whole trick: [turn.Run] checks the delegation
// depth before it opens a phase, so every case below drives the real runTurn —
// its prefetch, its runner build, its completion event — without a model call
// and without the network. What it cannot exercise is a turn that suspends;
// that is [TestASuspendedTurnKeepsTheIndicatorUp], on the same rule the engine
// drives keepAlive off.
func indicating(t *testing.T, mode notify.StatusMode) (*Engine, *workspace) {
	t.Helper()
	return indicatingWith(t, mode, refusingProvider{})
}

// indicatingWith is [indicating] over a chosen model, for the one case that
// has to let a turn run its phases.
func indicatingWith(t *testing.T, mode notify.StatusMode, prov llm.Provider) (*Engine, *workspace) {
	t.Helper()
	ws := newWorkspace(t)
	transport, err := slack.NewTransport(slack.TransportOptions{
		Config: slack.Config{
			Status: mode,
			Seats:  []slack.SeatConfig{{Handle: "swe", Token: "xoxb-swe"}},
		},
		HTTP: &http.Client{Transport: httpxtest.Rewrite(t, ws.Server)},
	})
	if err != nil {
		t.Fatalf("slack.NewTransport: %v", err)
	}
	if err := transport.Start(t.Context()); err != nil {
		t.Fatalf("the transport did not come up: %v", err)
	}
	t.Cleanup(func() { transport.Stop(context.Background()) })

	seat := &org.Role{Name: "SWE", DeclaredHandle: "swe", LLM: org.ProviderKeys{"only"}}
	organization := &org.Organization{Name: "Acme", Roles: []*org.Role{seat}}
	models, err := phase.NewRegistry([]phase.Entry{{Key: "only", Provider: prov}})
	if err != nil {
		t.Fatalf("phase.NewRegistry: %v", err)
	}
	q := memory.New()
	if err := q.Start(t.Context()); err != nil {
		t.Fatalf("queue.Start: %v", err)
	}
	t.Cleanup(func() { _ = q.Stop(context.Background()) })

	e := &Engine{backends: &Backends{Queue: q}}
	e.epoch.current.Store(&Company{
		Org:    organization,
		Models: models,
		Tools:  tools.NewRegistry(),
		// The depth cap this fixture rests on, plus one round so a turn
		// that got past it would run exactly one.
		Config: &config.Company{Name: "Acme", TurnEngine: config.TurnEngine{
			MaxIterations: 1, DelegationDepthLimit: 1, MaxToolRounds: 3,
		}},
	})
	e.notify.registry = notify.NewRegistry(organization)
	e.notify.slack = transport
	return e, ws
}

// chatTrigger is one inbound Slack message in one channel, stamped the way the
// Slack parser stamps what it parses.
//
// THE CHANNEL IS AN ARGUMENT because a session is keyed on (handle, channel,
// thread): two triggers in one channel are one indicator that two turns hold,
// which is the shape [TestASuspendedTurnKeepsTheIndicatorUp] has to keep apart
// from two independent turns.
func chatTrigger(channel string) *events.Event {
	ev := events.New(types.ExternalNotification{
		NotificationSource: slack.Backend, SourceEventType: "message",
		Sender: "ana", Subject: "a message", Body: "can you look at this",
		Metadata: map[string]string{
			notify.TransportField: slack.Backend,
			"channel":             channel,
			"ts":                  "1700000001.000100",
			"channel_type":        "im",
		},
	}, events.TraceContext{})
	ev.Source = "notify." + slack.Backend
	return ev
}

// A CHAT-TRIGGERED TURN RAISES AN INDICATOR AND CLEARS IT. The whole of the
// defect: the subsystem existed, the engine held the driver set, and no turn
// ever called it — so this is the case that fails on the code as it was.
func TestAChatTriggeredTurnRaisesTheIndicatorAndClearsIt(t *testing.T) {
	e, ws := indicating(t, notify.StatusAddressed)

	res, err := e.runTurn(t.Context(), Request{
		Handle: "swe", WorkKey: "wk-1", Depth: 3,
		Events: []*events.Event{chatTrigger("D0ANA")},
	})
	if err != nil {
		t.Fatalf("runTurn: %v", err)
	}
	if res.Breach == nil {
		t.Fatalf("the fixture's guard did not fire, so this turn called a model: %+v", res)
	}

	shown := ws.shownAtLeast(t, 2)
	if shown[0] == "" {
		t.Errorf("the first thing the workspace heard was %q, want the indicator raised", shown[0])
	}
	if last := shown[len(shown)-1]; last != "" {
		t.Errorf("the workspace last heard %q, want the clear that takes the "+
			"indicator down — a turn that ended leaves nobody working", last)
	}
	if got := e.notify.slack.Status().Live(); len(got) != 0 {
		t.Errorf("the finished turn left a live indicator: %v", got)
	}
}

// THE INDICATOR OPENS ON THE EXECUTOR'S PHASE, which is what makes the phase
// seam's first report free: every turn's first phase is the executor's, so a
// turn that opened on the `default` pool would spend a second request saying
// what the first one said — and `default` would stop meaning what its config
// field documents, a phase added later with no pool of its own.
func TestTheIndicatorOpensOnTheExecutorsPhase(t *testing.T) {
	e, ws := indicating(t, notify.StatusAlways)

	s := e.beginWorkingStatus(t.Context(), "swe", "wk-open",
		[]*events.Event{chatTrigger("D0ANA")})
	if s == nil {
		t.Fatal("no indicator was raised")
	}
	// The pool's own line for that phase, at this session's seed and no
	// rotation: the pick is deterministic, which is what lets a test name
	// the phase a raise came out of.
	want := notify.NewPhrases(nil).Pick(phase.Execute.String(), "wk-open", 0)
	if got := ws.shownAtLeast(t, 1)[0]; got != want {
		t.Errorf("the indicator opened on %q, want the executor's %q", got, want)
	}
	// And the seam's first report costs nothing, because it names the phase
	// the indicator is already showing.
	s.Phase(phase.Execute.String())
	time.Sleep(20 * time.Millisecond)
	if got := ws.shown(); len(got) != 1 {
		t.Errorf("the executor's own phase cost a second request: %v", got)
	}
	// A phase that is genuinely new does move it.
	s.Phase(phase.Review.String())
	ws.shownAtLeast(t, 2)
}

// AND THE SEAM IS WIRED, so the words move as the turn moves. The reviewer is
// what makes that observable: the opening raise is already the executor's, so
// a turn whose indicator never reached a review line is a turn whose phases
// reached nobody.
func TestTheIndicatorFollowsTheTurnsPhases(t *testing.T) {
	e, ws := indicatingWith(t, notify.StatusAlways, scripted{})

	res, err := e.runTurn(t.Context(), Request{
		Handle: "swe", WorkKey: "wk-phases",
		Events: []*events.Event{chatTrigger("D0ANA")},
	})
	if err != nil {
		t.Fatalf("runTurn: %v", err)
	}
	if res.Rounds != 1 {
		t.Fatalf("the turn ran %d rounds, so this case did not exercise its "+
			"phases: %+v", res.Rounds, res)
	}
	reviews := notify.PhasePhrases[phase.Review.String()]
	var moved bool
	for _, shown := range ws.shownAtLeast(t, 2) {
		moved = moved || slices.Contains(reviews, shown)
	}
	if !moved {
		t.Errorf("the indicator showed %v, none of it the reviewer's wording — "+
			"the turn's phases never reached the person watching", ws.shown())
	}
}

// A TURN NOBODY IS WATCHING A COMPOSER FOR RAISES NOTHING. A schedule tick, an
// A2A ask, a sandbox completion and a work-item assignment all reach a seat the
// same way and none of them stamps a transport — so there is no conversation to
// raise an indicator in, and no person to read it.
func TestANonChatTriggerRaisesNoIndicator(t *testing.T) {
	e, ws := indicating(t, notify.StatusAlways)

	// The wake a schedule actually publishes, and the same shape a
	// delegation and a tracker assignment arrive in.
	tick := events.New(types.TaskAssigned{
		TaskID: "t-1", RoleName: "SWE", Description: "write the standup note",
		Schedule: "standup",
	}, events.TraceContext{})
	if _, err := e.runTurn(t.Context(), Request{
		Handle: "swe", WorkKey: "wk-2", Depth: 3,
		Events: []*events.Event{tick},
	}); err != nil {
		t.Fatalf("runTurn: %v", err)
	}
	// A scheduled fire has no conversation and nobody watching a composer.
	e.raisedNothing(t, ws)
}

// AND NEITHER DOES A CHAT MESSAGE NOBODY ADDRESSED TO THIS SEAT. The mode
// decision is the driver's — the engine must not re-implement it, and must not
// leave it unasked either: a passive channel message wakes every bot in the
// room, and lighting up N indicators is noise rather than signal.
func TestAnUnaddressedChatTriggerRaisesNoIndicator(t *testing.T) {
	e, ws := indicating(t, notify.StatusAddressed)

	passive := chatTrigger("D0ANA")
	n, _ := events.DataAs[*types.ExternalNotification](passive)
	n.Metadata["channel"], n.Metadata["channel_type"] = "C0ENG", "channel"

	if _, err := e.runTurn(t.Context(), Request{
		Handle: "swe", WorkKey: "wk-3", Depth: 3,
		Events: []*events.Event{passive},
	}); err != nil {
		t.Fatalf("runTurn: %v", err)
	}
	e.raisedNothing(t, ws)
}

// AN INDICATOR SURVIVES A TURN ONLY WHILE SOMETHING IS STILL WORKING.
//
// The keep-alive case is a turn that suspended into a detached coding run the
// engine has established is coming back: the box is running, the same turn
// resumes when it reports, and the person who asked is still waiting — so
// taking it down would say the agent had stopped in the middle of the longest
// thing it does. Every other ending clears.
//
// Driven through [endWorkingStatus] rather than a whole turn, because that
// function IS the rule both turn frames read. WHAT ESTABLISHES the fact it is
// given is the frames' own business, and the cases below it drive that
// end to end.
func TestAnIndicatorSurvivesOnlyWhatIsStillWorking(t *testing.T) {
	e, ws := indicating(t, notify.StatusAlways)
	// One conversation per case: a second turn in the SAME channel would
	// JOIN the first's session rather than raise its own, so a case that
	// shared one would be asserting the reference count instead of the rule.
	raise := func(key, channel string) *notify.StatusSession {
		s := e.beginWorkingStatus(t.Context(), "swe", key,
			[]*events.Event{chatTrigger(channel)})
		if s == nil {
			t.Fatal("no indicator was raised for a chat trigger")
		}
		return s
	}

	working := raise("wk-suspend", "D0ANA")
	ws.shownAtLeast(t, 1)
	endWorkingStatus(t.Context(), working, true)
	if got := ws.shown(); slices.Contains(got, "") {
		t.Fatalf("a turn whose detached run is still working cleared its indicator: %v", got)
	}
	if len(e.notify.slack.Status().Live()) != 1 {
		t.Fatal("the kept-alive indicator is not live")
	}

	// And an ending clears — done, failed, skipped, a guard breach, a
	// runner that could never be built, and a suspension whose run was
	// settled instead of recorded are all the same fact here: nothing is
	// working any more.
	before := len(ws.shown())
	ending := raise("wk-end", "D0END")
	ws.shownAtLeast(t, before+1)
	endWorkingStatus(t.Context(), ending, false)
	if shown := ws.shown(); len(shown) <= before || shown[len(shown)-1] != "" {
		t.Fatalf("a turn that ended did not clear: %v", shown[before:])
	}
	if live := e.notify.slack.Status().Live(); len(live) != 1 {
		t.Errorf("the ending took down %d indicators, want only its own: %v", 2-len(live), live)
	}
}

// AND "STILL WORKING" IS THE RUN'S ROW, NOT THE TURN'S INTENT TO SUSPEND.
//
// [turn.Result.Suspended] says the executor asked to park. Whether anything
// can ever resume it is what [Engine.persistSuspension] answers, and its three
// answers are three different facts: a row that landed is a run the completion
// poll resumes, a row that could not be written is a run already settled and
// reclaimed, and a store that could not answer is neither — the write may well
// have landed, and the settle that follows declines a row that is no longer
// launching.
//
// AN UNKNOWN THEREFORE COUNTS AS WORKING, the same reading internal/coord takes
// of an unreachable store: read as loss, a two-second store blip takes the
// indicator down in the middle of the longest thing an agent does.
func TestOnlyARecordedSuspensionKeepsTheIndicatorUp(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		resumable bool
		err       error
		want      bool
	}{
		"a row the completion poll will resume":              {resumable: true, want: true},
		"a run settled because its row could not be written": {want: false},
		"a store that could not say either way": {
			err: errors.New("the coordination store did not answer"), want: true,
		},
	} {
		if got := stillWorking(tc.resumable, tc.err); got != tc.want {
			t.Errorf("%s: stillWorking(%v, %v) = %v, want %v",
				name, tc.resumable, tc.err, got, tc.want)
		}
	}
}

// END TO END: A SUSPENSION NOTHING RECORDED TAKES THE INDICATOR DOWN.
//
// This is the failure the rule above exists for, and it is reachable on a
// single node with no seat movement at all: the turn returns Suspended, the
// row is not written, the run is settled and its box reclaimed — and the turn
// never comes back. Keyed on the intent, the indicator said "is thinking…"
// every refresh interval for the life of the process, which is exactly what
// [notify.StatusDriver.ClearFor] was added to bound and what nothing else
// would ever have taken down.
func TestASuspendedTurnKeepsItsIndicatorOnlyIfItsRunWasRecorded(t *testing.T) {
	launching := func(t *testing.T, store *sandbox.CoordStore) sandbox.PendingStore {
		t.Helper()
		if err := store.BeginLaunch(t.Context(), sandbox.PendingRun{
			TurnID: "wk-code", AgentHandle: "swe", Role: "SWE",
		}, sandbox.Fence{}); err != nil {
			t.Fatalf("BeginLaunch: %v", err)
		}
		return store
	}

	for name, tc := range map[string]struct {
		pending func(*testing.T, *sandbox.CoordStore) sandbox.PendingStore
		live    int
	}{
		"a run whose row is open to the completion poll": {pending: launching, live: 1},
		"a run whose row was never launched": {
			// MarkSuspended finds nothing launching, so the suspension
			// has nowhere to go and the run is settled.
			pending: func(_ *testing.T, store *sandbox.CoordStore) sandbox.PendingStore { return store },
			live:    0,
		},
		"a store that could not answer": {
			pending: func(t *testing.T, store *sandbox.CoordStore) sandbox.PendingStore {
				return unwritableRuns{PendingStore: launching(t, store)}
			},
			live: 1,
		},
	} {
		t.Run(name, func(t *testing.T) {
			e, ws := indicatingWith(t, notify.StatusAlways, suspendingModel{})
			store := sandbox.NewCoordStore(coordmem.NewFleet())
			equipForCode(t, e, tc.pending(t, store))

			res, err := e.runTurn(t.Context(), Request{
				Handle: "swe", WorkKey: "wk-code",
				Events: []*events.Event{chatTrigger("D0ANA")},
			})
			if err != nil {
				t.Fatalf("runTurn: %v", err)
			}
			if !res.Suspended {
				t.Fatalf("the turn did not suspend, so this case asserts nothing: %+v", res)
			}
			if live := e.notify.slack.Status().Live(); len(live) != tc.live {
				t.Fatalf("%d indicators live, want %d: %v (the workspace heard %v)",
					len(live), tc.live, live, ws.shown())
			}
		})
	}
}

// unwritableRuns is a run store whose suspension write fails without saying
// whether it landed — the one answer that is neither "resumable" nor "lost".
type unwritableRuns struct{ sandbox.PendingStore }

func (unwritableRuns) MarkSuspended(context.Context, string, map[string]any) (bool, error) {
	return false, errors.New("the coordination store did not answer")
}

// equipForCode gives the fixture's seat a code gate, a run_sandbox that
// detaches, and the store the engine records its suspension in.
//
// The tool is a stand-in for the real one and does what the real one does to
// the LOOP: it detaches, so the Execute phase suspends with its call
// unanswered. What is behind it — a provider, a box, a coding agent — is the
// sandbox package's business and none of it changes what the ENGINE does with
// a suspension, which is what these cases are about.
func equipForCode(t *testing.T, e *Engine, pending sandbox.PendingStore) {
	t.Helper()
	company := e.Company()
	seat := company.Org.AgentSeatByHandle("swe")
	seat.Sandbox = &org.RoleSandbox{Enabled: true}
	if err := company.Tools.Register(suspendingTool{}, tools.OriginBuiltin); err != nil {
		t.Fatalf("registering the detaching tool: %v", err)
	}
	manager, err := sandbox.NewManager(sandbox.ManagerOptions{
		Providers: map[sandbox.Placement]sandbox.Provider{sandbox.Direct: sandbox.NewFakeProvider()},
		Runners:   map[string]sandbox.Runner{"claude-code": sandbox.NewFakeRunner("claude-code")},
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	coordinator, err := sandbox.NewCoordinator(sandbox.CoordinatorOptions{
		Queue: e.backends.Queue, Pending: pending, Manager: manager,
	})
	if err != nil {
		t.Fatalf("NewCoordinator: %v", err)
	}
	e.sandboxPending, e.sandboxCoordinator = pending, coordinator
}

// suspendingModel hands its first round to the detaching tool, which is all it
// takes: the loop suspends with that call unanswered and the turn returns.
type suspendingModel struct{}

func (suspendingModel) Model() string { return "suspending" }
func (suspendingModel) Complete(context.Context, llm.Request) (*llm.Completion, error) {
	return &llm.Completion{ToolCalls: []llm.ToolCall{{
		ID: "c1", Name: builtin.RunSandboxTool,
		Arguments: map[string]any{"brief": "fix the failing test"},
	}}}, nil
}

// suspendingTool is run_sandbox as far as the tool loop can tell.
type suspendingTool struct{}

var _ tools.Detached = suspendingTool{}

func (suspendingTool) Name() string { return builtin.RunSandboxTool }
func (suspendingTool) Description() string {
	return "Hand a concrete code task to a coding agent in an isolated sandbox."
}

func (suspendingTool) Parameters() map[string]any {
	return map[string]any{
		"type":       "object",
		"properties": map[string]any{"brief": map[string]any{"type": "string"}},
		"required":   []any{"brief"},
	}
}

func (suspendingTool) Call(context.Context, map[string]any) (tools.Result, error) {
	return tools.Result{Output: "this tool can only be called where the loop can suspend",
		Failed: true}, nil
}

func (suspendingTool) CallDetached(_ context.Context, t *turnctx.Turn, _ map[string]any) (tools.DetachedResult, error) {
	return tools.DetachedResult{
		Result:  tools.Result{Output: "(the coding job is running)"},
		Suspend: true,
		Payload: map[string]any{"turn_id": t.RunID},
	}, nil
}

// A RUN THAT PARKED ON A QUESTION TAKES ITS INDICATOR DOWN.
//
// The other half of the keep-alive, and the one the suspension rule cannot
// answer: the run was recorded, the turn IS coming back, and the agent has
// nonetheless stopped — everything now waits on the person the question was
// asked of. An indicator that keeps saying "is thinking…" at that person is
// worse than none at all, because it tells the one human who could unblock the
// work that nobody is waiting on them.
//
// AND IT IS ONE TURN'S HOLD, not the seat's indicators: a second turn in the
// same thread is still working and keeps its own.
func TestAParkedRunTakesItsIndicatorDown(t *testing.T) {
	e, ws := indicating(t, notify.StatusAlways)

	coding := e.beginWorkingStatus(t.Context(), "swe", "wk-code",
		[]*events.Event{chatTrigger("D0ANA")})
	if coding == nil {
		t.Fatal("no indicator was raised for a chat trigger")
	}
	ws.shownAtLeast(t, 1)
	endWorkingStatus(t.Context(), coding, true)

	// A colleague's ask lands in the same thread while the box runs, and it
	// is a different turn: the session is shared and reference-counted.
	alongside := e.beginWorkingStatus(t.Context(), "swe", "wk-alongside",
		[]*events.Event{chatTrigger("D0ANA")})
	if alongside == nil {
		t.Fatal("a second turn in the same thread joined no session")
	}

	e.releaseWorkingStatus(t.Context(), "swe", "wk-code")

	if got := ws.shown(); slices.Contains(got, "") {
		t.Fatalf("the park cleared an indicator a second turn is still holding: %v", got)
	}
	if live := e.notify.slack.Status().Live(); len(live) != 1 {
		t.Fatalf("the second turn's indicator went down with the park: %v", live)
	}

	// And when that turn ends too, the last hold takes it down.
	endWorkingStatus(t.Context(), alongside, false)
	if shown := ws.shown(); shown[len(shown)-1] != "" {
		t.Errorf("the last hold ended without clearing: %v", shown)
	}
	if live := e.notify.slack.Status().Live(); len(live) != 0 {
		t.Errorf("an indicator outlived every turn holding it: %v", live)
	}
}

// AND A PERSON'S ANSWER RAISES ONE AGAIN.
//
// The park released the hold, so a resume that only ever rejoined would work
// through the answer in silence — the exact gap this indicator exists to
// close, at the one moment a person has just asked for something. A run
// resumed by an ANSWER is woken by an ordinary chat message, so the trigger
// carries the conversation to raise in; a run resumed by a BOX is not, and a
// resume that invented a conversation for it would be claiming a thread it
// cannot prove it is in.
func TestAnAnsweredClarificationRaisesTheIndicatorAgain(t *testing.T) {
	e, ws := indicating(t, notify.StatusAlways)

	answered := e.resumeWorkingStatus(t.Context(), "swe", "wk-code", chatTrigger("D0ANA"))
	if answered == nil {
		t.Fatal("the answer that resumed a parked run raised no indicator")
	}
	if got := ws.shownAtLeast(t, 1)[0]; got == "" {
		t.Errorf("the workspace was asked for %q, want the indicator raised", got)
	}

	// A completion carries no chat conversation, and with no hold left to
	// take back there is nothing to raise: a parked run's row deliberately
	// keeps no chat metadata.
	completion := events.New(types.SandboxRunCompleted{TurnID: "wk-box"}, events.TraceContext{})
	if s := e.resumeWorkingStatus(t.Context(), "swe", "wk-box", completion); s != nil {
		t.Errorf("a box's completion raised an indicator in %v", s.Conversation())
	}
}

// A RESUMED TURN TAKES BACK THE INDICATOR ITS SUSPENSION KEPT ALIVE.
//
// Two halves of one rule: it must not raise a SECOND indicator over the one
// that is already up — the hold is one turn id, so a rejoin is idempotent where
// a second raise would reset the words a reader is watching — and it must end
// that hold when the resumed turn ends, or the indicator this node is
// re-asserting outlives every turn there is.
func TestAResumedTurnRejoinsTheIndicatorItKeptAlive(t *testing.T) {
	e, ws := indicatingWith(t, notify.StatusAlways, scripted{})
	company := e.Company()
	seat := company.Org.AgentSeatByHandle("swe")

	// The turn that suspended into a detached coding run.
	suspended := e.beginWorkingStatus(t.Context(), "swe", "wk-1",
		[]*events.Event{chatTrigger("D0ANA")})
	ws.shownAtLeast(t, 1)
	endWorkingStatus(t.Context(), suspended, true)
	if got := ws.raises(); got != 1 {
		t.Fatalf("the suspended turn raised %d indicators, want one", got)
	}

	// The box reports and the resume re-enters on this node, runs the phases
	// the answer unblocks and ends — which is one of the endings that clears.
	err := e.resumeTurn(t.Context(), resumeInput{
		Company: company,
		Run: sandbox.PendingRun{
			TurnID: "wk-1", AgentHandle: "swe", Reply: "tool",
			TaskDescription: "fix the failing test",
		},
		Turn:   &turnctx.Turn{RunID: "run-1", WorkKey: "wk-1", Seat: seat, Org: company.Org},
		Answer: "the tests pass now",
	})
	if err != nil {
		t.Fatalf("resumeTurn: %v", err)
	}
	// NOT A SECOND RAISE. The hold is one turn id, so a rejoin is
	// idempotent — where a second Begin would reset the words a reader is
	// watching and leave a count that needs two Ends to clear.
	if got := ws.raises(); got != 2 {
		t.Errorf("the resumed turn produced %d raises, want the one it kept "+
			"alive plus its reviewer's phase: %v", got, ws.shown())
	}
	// And its phases moved the words, which is the resume path's own OnPhase
	// wiring: without it a resumed turn shows whatever the suspended half
	// last said, for however long the box's answer takes to work through.
	reviews := notify.PhasePhrases[phase.Review.String()]
	var moved bool
	for _, shown := range ws.shown() {
		moved = moved || slices.Contains(reviews, shown)
	}
	if !moved {
		t.Errorf("the resumed turn's phases reached nobody: %v", ws.shown())
	}
	if shown := ws.shown(); shown[len(shown)-1] != "" {
		t.Errorf("the resumed turn ended without clearing: %v", shown)
	}
	if live := e.notify.slack.Status().Live(); len(live) != 0 {
		t.Errorf("the resumed turn left an indicator up: %v", live)
	}
}

// THE METADATA IS THE TRIGGER'S OWN, taken rather than reconstructed: the
// channel and the thread anchor a status is raised in are exactly the ones the
// chat parser stamped, which is what makes the indicator appear where the
// reply will land and what makes the thread block, the reply target and the
// indicator agree about which conversation a turn is in.
func TestTheIndicatorsConversationComesOffTheTrigger(t *testing.T) {
	t.Parallel()
	chat := chatTrigger("D0ANA")
	tracker := events.New(types.ExternalNotification{
		NotificationSource: "jira", SourceEventType: "comment", Sender: "ana",
		// A tracker's notification carries metadata too — and none of it
		// addresses a conversation any chat backend can raise a status in.
		Metadata: map[string]string{"issue_key": "ENG-42", "project": "ENG"},
	}, events.TraceContext{})
	task := events.New(types.TaskAssigned{TaskID: "t-1", RoleName: "SWE"},
		events.TraceContext{})

	for name, tc := range map[string]struct {
		evs  []*events.Event
		want string
	}{
		"a chat message":              {[]*events.Event{chat}, "D0ANA"},
		"a work-item assignment":      {[]*events.Event{task}, ""},
		"a tracker comment":           {[]*events.Event{tracker}, ""},
		"nothing at all":              {nil, ""},
		"a chat message among others": {[]*events.Event{task, tracker, chat}, "D0ANA"},
	} {
		got := chatMetadataOf(tc.evs)
		if got["channel"] != tc.want {
			t.Errorf("%s: resolved channel %q, want %q", name, got["channel"], tc.want)
		}
	}
}

// A NODE THAT HANDS A SEAT ON TAKES ITS INDICATORS DOWN.
//
// The kept-alive session is why this has to exist. A turn suspended into a
// detached coding run leaves its indicator up deliberately, and the resume
// lands on whichever node holds the seat WHEN THE BOX REPORTS — so a node that
// released the seat in between would otherwise re-assert "is thinking…" every
// refresh interval, for a turn it is not running, until the process died. It is
// the chat-surface twin of the `terminated` event this same hook publishes,
// except that a stale indicator is a live request rather than a stale row.
func TestReleasingASeatTakesItsIndicatorDown(t *testing.T) {
	e, ws := indicating(t, notify.StatusAlways)

	s := e.beginWorkingStatus(t.Context(), "swe", "wk-1",
		[]*events.Event{chatTrigger("D0ANA")})
	ws.shownAtLeast(t, 1)
	endWorkingStatus(t.Context(), s, true)
	if len(e.notify.slack.Status().Live()) != 1 {
		t.Fatal("the suspended turn's indicator is not live")
	}

	e.releaseSeat(t.Context(), "swe")

	if shown := ws.shown(); shown[len(shown)-1] != "" {
		t.Errorf("the released seat's indicator was not cleared: %v", shown)
	}
	if live := e.notify.slack.Status().Live(); len(live) != 0 {
		t.Errorf("a seat this node no longer runs still shows an indicator: %v", live)
	}
}

// A TEARDOWN TAKES A DETACHED CONTEXT, and this is the case that says why.
//
// The ending an indicator reports is often the cancellation itself — a shed
// seat, a drained node, a turn that ran out of its wall clock — and a clear
// made on a dead context does nothing at all, which leaves the indicator
// claiming the agent is still working until the backend expires it. The same
// rule the rest of the engine's rollbacks follow.
func TestTheClearSurvivesTheCancellationThatEndedTheTurn(t *testing.T) {
	e, ws := indicating(t, notify.StatusAlways)
	ctx, cancel := context.WithCancel(t.Context())

	s := e.beginWorkingStatus(ctx, "swe", "wk-cancel", []*events.Event{chatTrigger("D0ANA")})
	ws.shownAtLeast(t, 1)
	cancel()
	endWorkingStatus(ctx, s, false)

	if shown := ws.shown(); shown[len(shown)-1] != "" {
		t.Errorf("a turn ended by a cancellation left its indicator up: %v", shown)
	}
	if live := e.notify.slack.Status().Live(); len(live) != 0 {
		t.Errorf("the session survived its own teardown: %v", live)
	}
}
