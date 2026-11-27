package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

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
	"github.com/crewlet/crewlet/internal/providers/llm/llmtest"
	"github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
	"github.com/crewlet/crewlet/internal/seat/placement"
	"github.com/crewlet/crewlet/internal/slack"
	"github.com/crewlet/crewlet/internal/tools"
)

// The working indicator, end to end from the frame that owns a turn, through a
// real transport: asserting against the notify layer alone cannot catch a
// poster nothing in production calls. Only an assertion from [Engine.runTurn]
// to what a chat backend was asked to do can.

// workspace is a Slack that records the statuses it was asked to show.
type workspace struct {
	*httptest.Server

	mu       sync.Mutex
	statuses []string
	// posted is closed and replaced on every recorded status, so a waiter
	// that took it under the same lock as its snapshot cannot miss the next
	// post; a counted channel would drop one once its buffer filled.
	posted chan struct{}
}

func newWorkspace(t *testing.T) *workspace {
	t.Helper()
	ws := &workspace{posted: make(chan struct{})}
	ws.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method := strings.TrimPrefix(r.URL.Path, "/api/")
		if method == "assistant.threads.setStatus" {
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			status, _ := body["status"].(string)
			ws.mu.Lock()
			ws.statuses = append(ws.statuses, status)
			close(ws.posted)
			ws.posted = make(chan struct{})
			ws.mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		// One identity for auth.test, ok for everything else: a turn's
		// thread read on this token gets an empty thread.
		_, _ = w.Write([]byte(`{"ok":true,"user_id":"U0BOT","team_id":"T0ACME"}`))
	}))
	t.Cleanup(ws.Close)
	return ws
}

// shown is what the workspace was asked to display, raises and clears alike:
// Slack clears by setting an empty status, so "" is the indicator coming down.
func (w *workspace) shown() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.statuses...)
}

// raises counts the non-empty statuses: announcements, not clears.
func (w *workspace) raises() int {
	var n int
	for _, s := range w.shown() {
		if s != "" {
			n++
		}
	}
	return n
}

// WHAT IS PROMISED, AND WHEN — the rule every wait below is written against.
//
// The session's own goroutine makes every post: a raise or a phase change
// only writes its state and wakes it, and the teardown cancels it before
// clearing, so a post in flight is abandoned and one not started is skipped
// (the indicator is cosmetic; a turn never waits on a chat backend). So a
// session's state reaches the backend only WHILE IT IS LIVE; after its last
// holder ends, the only promised post is the clear, made synchronously by the
// ending turn. Every case therefore waits for a raise or phase line while the
// turn is still running; waiting after it ended asserts a property the engine
// does not have.

// postDeadline bounds a hang on a post that IS promised, never a retry until
// something passes: ten seconds is far past a loopback request under any load
// this suite puts on a machine.
const postDeadline = 10 * time.Second

// awaitShown blocks until the workspace has been asked for n statuses.
func (w *workspace) awaitShown(t *testing.T, n int) []string {
	t.Helper()
	return w.await(t, fmt.Sprintf("%d statuses", n),
		func(shown []string) bool { return len(shown) >= n })
}

// awaitAnyOf blocks until the workspace has been shown one of these lines,
// which is how a case waits for a PHASE rather than for a count.
func (w *workspace) awaitAnyOf(t *testing.T, want []string) []string {
	t.Helper()
	return w.await(t, fmt.Sprintf("one of %v", want), func(shown []string) bool {
		for _, s := range shown {
			if slices.Contains(want, s) {
				return true
			}
		}
		return false
	})
}

// await blocks until the workspace's record satisfies done. The snapshot and
// the wake channel are taken under one lock, so a post landing between them
// wakes this wait rather than being missed.
func (w *workspace) await(t *testing.T, want string, done func([]string) bool) []string {
	t.Helper()
	timeout := time.NewTimer(postDeadline)
	defer timeout.Stop()
	for {
		w.mu.Lock()
		shown := append([]string(nil), w.statuses...)
		posted := w.posted
		w.mu.Unlock()
		if done(shown) {
			return shown
		}
		select {
		case <-posted:
		case <-timeout.C:
			t.Fatalf("the workspace was asked for %v, want %s", shown, want)
		}
	}
}

// firstOf reports where the first of these lines sits in what the workspace
// was shown, or -1. A position, not membership: [workspace.awaitAnyOf]
// already promises presence, so only where a line sits relative to the raise
// shows the phase seam moved the words.
func firstOf(shown, want []string) int {
	for i, line := range shown {
		if slices.Contains(want, line) {
			return i
		}
	}
	return -1
}

// raisedNothing asserts "no indicator" deterministically: only Begin creates a
// session, so a driver holding none can never post, where an empty request
// log alone could be a post still in flight.
func (e *Engine) raisedNothing(t *testing.T, ws *workspace) {
	t.Helper()
	if live := e.notify.slack.Status().Live(); len(live) != 0 {
		t.Fatalf("an indicator was raised in %v", live)
	}
	if got := ws.shown(); len(got) != 0 {
		t.Fatalf("the workspace was told %v", got)
	}
}

// refusingProvider stands in for the seat's model, which no case reaches:
// every turn ends on a guard before a provider call, and a refusal says so.
type refusingProvider struct{}

func (refusingProvider) Model() string { return "refusing" }
func (refusingProvider) Complete(context.Context, llm.Request) (*llm.Completion, error) {
	return nil, &llm.Error{Kind: llm.KindFatal, Provider: "test", Model: "refusing"}
}

// scripted answers each phase with its own submission, chosen from the tools
// the request offers (a flat sequence drifts once a phase takes a round more),
// as a harness on the real session ([phasedHarness]).
//
// waiting runs on the turn's own goroutine as the harness run starts, before
// the answer, so a case can hold the turn open until the indicator it asserts
// on has reached the workspace: the window where that post is promised (see
// "WHAT IS PROMISED, AND WHEN" above).
type scripted struct{ waiting func(reviewing bool) }

func (scripted) Model() string { return "scripted" }

func (s scripted) Complete(ctx context.Context, req llm.Request) (*llm.Completion, error) {
	return phasedHarness{model: "scripted", pick: s.run}.Complete(ctx, req)
}

func (s scripted) run(offered []string) llmtest.Run {
	name, args := runner.SubmitWorkTool, map[string]any{
		"outcome": "blocked", "summary": "nothing to do here",
		"evidence": "this seat holds no write tool",
	}
	reviewing := offers(offered, runner.SubmitReviewTool)
	if reviewing {
		name, args = runner.SubmitReviewTool, map[string]any{"decision": "done"}
	}
	if s.waiting != nil {
		s.waiting(reviewing)
	}
	return calling(name, args, llmtest.DefaultUsage)
}

// indicating builds an engine whose one seat is on a Slack workspace, with a
// company that refuses a turn at its depth guard: [turn.Run] checks the
// delegation depth before opening a phase, so a case drives the real runTurn
// (prefetch, runner build, completion event) with no model call or network.
// A turn that suspends is [TestAnIndicatorSurvivesOnlyWhatIsStillWorking]'s.
func indicating(t *testing.T, mode notify.StatusMode) (*Engine, *workspace) {
	t.Helper()
	return indicatingWith(t, mode, refusingProvider{})
}

// indicatingWith is [indicating] over a chosen model, for the one case that
// has to let a turn run its phases.
func indicatingWith(t *testing.T, mode notify.StatusMode, prov llm.Provider) (*Engine, *workspace) {
	t.Helper()
	return indicatingOn(t, mode, prov, executor.ClaudeCodeName)
}

// indicatingOn is [indicatingWith] with the seat's executor launching on the
// harness named ([runnableOn]).
func indicatingOn(t *testing.T, mode notify.StatusMode, prov llm.Provider,
	runnerHarness string) (*Engine, *workspace) {

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
	models := runnableOn(t, runnerHarness, []phase.Entry{{Key: "only", Provider: prov}}, seat)
	q := memory.New()
	if err := q.Start(t.Context()); err != nil {
		t.Fatalf("queue.Start: %v", err)
	}
	t.Cleanup(func() { _ = q.Stop(context.Background()) })

	// A coordination store, as every `crewlet run` engine has ([starting]
	// says what a turn needs it for).
	e := withSessions(t, &Engine{backends: &Backends{Queue: q, Fleet: coordmem.NewFleet()}})
	e.epoch.current.Store(&Company{
		Org:    organization,
		Models: models,
		Tools:  tools.NewRegistry(),
		// The depth cap this fixture rests on.
		Config: &config.Company{Name: "Acme", TurnEngine: config.TurnEngine{
			MaxIterations: 1, DelegationDepthLimit: 1,
		}},
	})
	e.notify.registry = notify.NewRegistry(organization, nil)
	e.notify.slack = transport
	return e, ws
}

// chatTrigger is one inbound Slack message in channel, stamped as the Slack
// parser stamps it. The channel is an argument because a session is keyed on
// (handle, channel, thread): two triggers in one channel share one indicator.
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

// A CHAT-TRIGGERED TURN RAISES AN INDICATOR AND CLEARS IT. The raise is
// asserted from inside the turn, the only window it is promised in: the model
// does not answer until the workspace has heard it. The clear is asserted
// after, since the segment that ends the turn makes it.
func TestAChatTriggeredTurnRaisesTheIndicatorAndClearsIt(t *testing.T) {
	t.Parallel()
	var (
		e      *Engine
		ws     *workspace
		raised []string
	)
	model := scripted{waiting: func(reviewing bool) {
		if !reviewing {
			raised = ws.awaitShown(t, 1)
		}
	}}
	e, ws = indicatingWith(t, notify.StatusAddressed, model)
	rig := rigRuns(t, e, modelPlayer(t, model.run))

	res, err := e.runTurn(t.Context(), Request{
		Handle: "swe", WorkKey: "wk-1", RunID: "run-1", WorkSince: time.Now().UTC(),
		Events: []*events.Event{chatTrigger("D0ANA")},
	})
	if err != nil || !res.Suspended {
		t.Fatalf("runTurn = %+v, %v; want the turn parked on its executor's run", res, err)
	}
	if err := rig.collect(t, "run-1"); err != nil {
		t.Fatalf("collecting the run: %v", err)
	}

	// Two checks: an empty record means the wait itself failed, a different
	// report from a workspace whose first status was a clear.
	if len(raised) == 0 {
		t.Fatal("the raise the turn waited for never reached the workspace")
	}
	if raised[0] == "" {
		t.Errorf("the running turn first showed %v, want the indicator raised "+
			"rather than cleared", raised)
	}
	shown := ws.shown()
	if last := shown[len(shown)-1]; last != "" {
		t.Errorf("the workspace last heard %q, want the clear that takes the "+
			"indicator down — a turn that ended leaves nobody working", last)
	}
	if got := e.notify.slack.Status().Live(); len(got) != 0 {
		t.Errorf("the finished turn left a live indicator: %v", got)
	}
}

// THE INDICATOR OPENS ON THE EXECUTOR'S PHASE, every turn's first, so the
// phase seam's first report costs no request; opening on the `default` pool
// would also break what that field documents (a later phase with no pool).
func TestTheIndicatorOpensOnTheExecutorsPhase(t *testing.T) {
	t.Parallel()
	e, ws := indicating(t, notify.StatusAlways)

	s := e.beginWorkingStatus(t.Context(), "swe", "run-open",
		[]*events.Event{chatTrigger("D0ANA")})
	if s == nil {
		t.Fatal("no indicator was raised")
	}
	// The pool's line for that phase at this session's seed and no
	// rotation: the pick is deterministic.
	want := notify.NewPhrases(nil).Pick(phase.Execute.String(), "run-open", 0)
	if got := ws.awaitShown(t, 1)[0]; got != want {
		t.Errorf("the indicator opened on %q, want the executor's %q", got, want)
	}
	// The seam's first report costs nothing, proved by what comes next: a new
	// phase posts, so a costly executor report would make the second status
	// another executor line.
	s.Phase(phase.Execute.String())
	s.Phase(phase.Review.String())
	if got := ws.awaitShown(t, 2)[1]; !slices.Contains(notify.PhasePhrases[phase.Review.String()], got) {
		t.Errorf("the second status was %q, want the reviewer's — the executor's "+
			"own phase cost a request to say what the raise already said", got)
	}
}

// AND THE SEAM IS WIRED, so the words move as the turn moves: the opening
// raise is already the executor's, so only a review line shows it.
func TestTheIndicatorFollowsTheTurnsPhases(t *testing.T) {
	t.Parallel()
	reviews := notify.PhasePhrases[phase.Review.String()]
	// The reviewer's call holds the turn open until its phase line lands
	// (the seam fires before a phase's first provider call); waiting after
	// the turn would wait on a post the teardown cancelled.
	var (
		e     *Engine
		ws    *workspace
		moved []string
	)
	model := scripted{
		waiting: func(reviewing bool) {
			if !reviewing {
				// The raise must land first, or there is no order to
				// assert: a phase change beating it to the session's
				// goroutine puts the reviewer's line at index 0.
				ws.awaitShown(t, 1)
				return
			}
			moved = ws.awaitAnyOf(t, reviews)
		},
	}
	e, ws = indicatingWith(t, notify.StatusAlways, model)
	rig := rigRuns(t, e, modelPlayer(t, model.run))

	res, err := e.runTurn(t.Context(), Request{
		Handle: "swe", WorkKey: "wk-phases", RunID: "run-phases", WorkSince: time.Now().UTC(),
		Events: []*events.Event{chatTrigger("D0ANA")},
	})
	if err != nil || !res.Suspended {
		t.Fatalf("runTurn = %+v, %v; want the turn parked on its executor's run", res, err)
	}
	// The reviewer runs in the segment the run's collection resumes.
	if err := rig.collect(t, "run-phases"); err != nil {
		t.Fatalf("collecting the run: %v", err)
	}
	// Where the reviewer's line sits, not that it is there (the wait says
	// so): the seam must move the words from the raise to the reviewer.
	if len(moved) == 0 || !slices.Contains(notify.PhasePhrases[phase.Execute.String()], moved[0]) {
		t.Fatalf("the workspace first heard %v, want the executor's opening raise", moved)
	}
	if at := firstOf(moved, reviews); at != 1 {
		t.Errorf("the reviewer's wording sits at %d in %v, want it second — "+
			"straight after the raise, because the executor's own report names "+
			"the phase the raise already showed and costs no request", at, moved)
	}
}

// A TURN NOBODY IS WATCHING A COMPOSER FOR RAISES NOTHING: a schedule tick, an
// A2A ask, a sandbox completion and a work-item assignment stamp no transport,
// so there is no conversation to raise one in.
func TestANonChatTriggerRaisesNoIndicator(t *testing.T) {
	t.Parallel()
	e, ws := indicating(t, notify.StatusAlways)

	// A schedule's wake, the shape a delegation and a tracker assignment
	// share: only a transport's stamp raises an indicator ([chatMetadataOf]).
	tick := events.New(types.TaskAssigned{
		TaskID: "t-1", RoleName: "SWE", Description: "write the standup note",
		Schedule: "standup",
	}, events.TraceContext{})
	if _, err := e.runTurn(t.Context(), Request{
		Handle: "swe", WorkKey: "wk-2", Depth: 3, WorkSince: time.Now().UTC(),
		Events: []*events.Event{tick},
	}); err != nil {
		t.Fatalf("runTurn: %v", err)
	}
	e.raisedNothing(t, ws)
}

// AND NEITHER DOES A CHAT MESSAGE NOBODY ADDRESSED TO THIS SEAT. The mode
// decision is the driver's, which the engine must ask, not re-implement: a
// passive channel message wakes every bot in the room.
func TestAnUnaddressedChatTriggerRaisesNoIndicator(t *testing.T) {
	t.Parallel()
	e, ws := indicating(t, notify.StatusAddressed)

	passive := chatTrigger("D0ANA")
	n, _ := events.DataAs[*types.ExternalNotification](passive)
	n.Metadata["channel"], n.Metadata["channel_type"] = "C0ENG", "channel"

	if _, err := e.runTurn(t.Context(), Request{
		Handle: "swe", WorkKey: "wk-3", Depth: 3, WorkSince: time.Now().UTC(),
		Events: []*events.Event{passive},
	}); err != nil {
		t.Fatalf("runTurn: %v", err)
	}
	e.raisedNothing(t, ws)
}

// AN INDICATOR SURVIVES A TURN ONLY WHILE SOMETHING IS STILL WORKING: a turn
// suspended into a detached executor established as coming back keeps it up,
// since the person is still waiting; every other ending clears. Driven
// through [endWorkingStatus], the rule both turn frames read; the cases below
// drive what establishes its input end to end.
func TestAnIndicatorSurvivesOnlyWhatIsStillWorking(t *testing.T) {
	t.Parallel()
	e, ws := indicating(t, notify.StatusAlways)
	// One conversation per case: a second turn in the same channel would
	// join the first's session, asserting the reference count, not the rule.
	raise := func(key, channel string) *notify.StatusSession {
		s := e.beginWorkingStatus(t.Context(), "swe", key,
			[]*events.Event{chatTrigger(channel)})
		if s == nil {
			t.Fatal("no indicator was raised for a chat trigger")
		}
		return s
	}

	working := raise("run-suspend", "D0ANA")
	ws.awaitShown(t, 1)
	endWorkingStatus(t.Context(), working, true)
	if got := ws.shown(); slices.Contains(got, "") {
		t.Fatalf("a turn whose detached run is still working cleared its indicator: %v", got)
	}
	if len(e.notify.slack.Status().Live()) != 1 {
		t.Fatal("the kept-alive indicator is not live")
	}

	// Any other ending (done, failed, skipped, a guard breach, an unbuildable
	// runner, a suspension settled instead of recorded) clears.
	before := len(ws.shown())
	ending := raise("run-end", "D0END")
	ws.awaitShown(t, before+1)
	endWorkingStatus(t.Context(), ending, false)
	if shown := ws.shown(); shown[len(shown)-1] != "" {
		t.Fatalf("a turn that ended did not clear: %v", shown[before:])
	}
	if live := e.notify.slack.Status().Live(); len(live) != 1 {
		t.Errorf("the ending took down %d indicators, want only its own: %v", 2-len(live), live)
	}
}

// AND "STILL WORKING" IS THE RUN'S ROW, NOT THE TURN'S INTENT TO SUSPEND.
// [Engine.persistSuspension] answers three ways: a row that landed is resumed
// by the completion poll, a row not written is a run already settled, and a
// store that could not answer is neither (the write may have landed; the
// settle declines a row no longer launching). An unknown counts as working,
// as internal/coord reads an unreachable store: read as loss, a store blip
// takes the indicator down mid-run.
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

// END TO END: A SUSPENSION NOTHING RECORDED TAKES THE INDICATOR DOWN. Reachable
// on one node with no seat movement: the turn suspends, the suspension is not
// recorded, the run is settled and the turn never returns, so an indicator
// keyed on the intent would refresh for the life of the process, with only
// [notify.StatusDriver.ClearFor] to bound it. Through the real launch, over a
// run store whose answer to the suspension is the case.
func TestASuspendedTurnKeepsItsIndicatorOnlyIfItsRunWasRecorded(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		pending func(sandbox.PendingStore) sandbox.PendingStore
		live    int
	}{
		"a run whose row is open to the completion poll": {
			pending: func(store sandbox.PendingStore) sandbox.PendingStore { return store },
			live:    1,
		},
		"a run no longer launching when its turn parks": {
			// MarkSuspended finds nothing launching (the run ended
			// before its turn parked), so the run is settled.
			pending: func(store sandbox.PendingStore) sandbox.PendingStore {
				return endedRuns{PendingStore: store}
			},
			live: 0,
		},
		"a store that could not answer, whose write landed": {
			// The row is open to the poll, so the settle after the
			// error declines it and the hold stays up.
			pending: func(store sandbox.PendingStore) sandbox.PendingStore {
				return unwritableRuns{PendingStore: store, landed: true}
			},
			live: 1,
		},
		"a store that could not answer, whose write was lost": {
			// Over a row still launching the settle ends the run, and
			// its stop takes down the hold, keyed by the run.
			pending: func(store sandbox.PendingStore) sandbox.PendingStore {
				return unwritableRuns{PendingStore: store}
			},
			live: 0,
		},
	} {
		t.Run(name, func(t *testing.T) {
			e, ws := indicating(t, notify.StatusAlways)
			// Under the fixture's depth cap, so the turn reaches its
			// executor's launch.
			e.Company().Config.TurnEngine.DelegationDepthLimit = 3
			rigRunsOn(t, e, tc.pending(sandbox.NewCoordStore(coordmem.NewFleet())), nil)

			res, err := e.runTurn(t.Context(), Request{
				Handle: "swe", WorkKey: "wk-code", RunID: "run-code", WorkSince: time.Now().UTC(),
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

// A SUSPENDED TURN'S INDICATOR IS HELD UNDER ITS RUN, the key its resume and
// its stop name it by: the suspension's row is keyed on the run (ADR-0017).
// Held under the work key, neither finds it and it stays up until the seat
// moves. Driven through [Engine.runTurn] with the run and work key apart.
//
// Mutation: begin the dispatch's indicator under req.WorkKey, and the rejoin
// finds nothing and the stop leaves it live.
func TestASuspendedTurnsIndicatorIsHeldUnderItsRun(t *testing.T) {
	t.Parallel()
	for name, settle := range map[string]func(*testing.T, *Engine){
		"a box's completion takes the hold back": func(t *testing.T, e *Engine) {
			completion := events.New(types.ExecutorFinished{TurnID: "run-code"},
				events.TraceContext{})
			session, rejoined := e.resumeWorkingStatus(t.Context(), "swe", "run-code", completion)
			if !rejoined || session == nil {
				t.Fatalf("the resume of run-code rejoined %v, want the hold its "+
					"suspension kept alive", rejoined)
			}
			endWorkingStatus(t.Context(), session, false)
		},
		"a stop drops it": func(t *testing.T, e *Engine) {
			e.releaseWorkingStatus(t.Context(), "swe", "run-code")
		},
	} {
		t.Run(name, func(t *testing.T) {
			e, ws := indicating(t, notify.StatusAlways)
			// The real launch writes the run's row under req.RunID and parks
			// the turn on it, so hold and row are the ones production makes.
			rigRuns(t, e, nil)

			res, err := e.runTurn(t.Context(), Request{
				Handle: "swe", WorkKey: "wk-code", RunID: "run-code", WorkSince: time.Now().UTC(),
				Events: []*events.Event{chatTrigger("D0ANA")},
			})
			if err != nil {
				t.Fatalf("runTurn: %v", err)
			}
			if !res.Suspended || len(e.notify.slack.Status().Live()) != 1 {
				t.Fatalf("the turn suspended %v with %v live, so this case asserts nothing",
					res.Suspended, e.notify.slack.Status().Live())
			}
			settle(t, e)
			if live := e.notify.slack.Status().Live(); len(live) != 0 {
				t.Errorf("%v still live (the workspace heard %v), want the suspended "+
					"turn's hold gone", live, ws.shown())
			}
		})
	}
}

// unwritableRuns is a run store whose suspension write fails without saying
// whether it landed; landed says which it secretly was.
type unwritableRuns struct {
	sandbox.PendingStore
	landed bool
}

func (u unwritableRuns) MarkSuspended(ctx context.Context, turnID string, s sandbox.Suspension) (bool, error) {
	if u.landed {
		if _, err := u.PendingStore.MarkSuspended(ctx, turnID, s); err != nil {
			return false, err
		}
	}
	return false, errors.New("the coordination store did not answer")
}

// endedRuns is a run store whose run is no longer launching by the time its
// turn records the suspension.
type endedRuns struct{ sandbox.PendingStore }

func (endedRuns) MarkSuspended(context.Context, string, sandbox.Suspension) (bool, error) {
	return false, nil
}

// A RUN THAT PARKED ON A QUESTION TAKES ITS INDICATOR DOWN, though recorded and
// coming back: everything waits on the person asked, and "is thinking…" would
// tell them nobody is waiting on them. It is one turn's hold: a second turn in
// the same thread keeps its own.
func TestAParkedRunTakesItsIndicatorDown(t *testing.T) {
	t.Parallel()
	e, ws := indicating(t, notify.StatusAlways)

	coding := e.beginWorkingStatus(t.Context(), "swe", "run-code",
		[]*events.Event{chatTrigger("D0ANA")})
	if coding == nil {
		t.Fatal("no indicator was raised for a chat trigger")
	}
	ws.awaitShown(t, 1)
	endWorkingStatus(t.Context(), coding, true)

	// A colleague's ask lands in the same thread while the box runs, and it
	// is a different turn: the session is shared and reference-counted.
	alongside := e.beginWorkingStatus(t.Context(), "swe", "run-alongside",
		[]*events.Event{chatTrigger("D0ANA")})
	if alongside == nil {
		t.Fatal("a second turn in the same thread joined no session")
	}

	e.releaseWorkingStatus(t.Context(), "swe", "run-code")

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

// AND SO DOES A RUN THE ENGINE DESTROYS (settleFailed): the turn never returns
// to say so, so without [sandbox.CoordinatorOptions.Stopped] the indicator
// outlives the process. Driven through [Engine.startSandbox], the coordinator
// the engine builds, because an option declared and never passed is what a
// test holding its own coordinator cannot see. One turn's hold: a second turn
// in the same thread keeps its own.
func TestASettledRunTakesItsIndicatorDown(t *testing.T) {
	t.Parallel()
	e, ws := indicating(t, notify.StatusAlways)
	e.backends.Fleet = coordmem.NewFleet()
	company := e.Company()
	// The double suffices: the run is settled before it ever has a box.
	company.Config.Providers.Sandbox = &config.SandboxProvider{Fake: true}
	manager, err := buildSandbox(company.Config, e.resolver(), e.sandboxOtel, nil, nil)
	if err != nil {
		t.Fatalf("buildSandbox: %v", err)
	}
	e.answers = testAnswers()
	if started, err := e.startSandbox(t.Context(), manager); err != nil || !started {
		t.Fatalf("startSandbox = (%v, %v), want a runtime brought up", started, err)
	}
	rt := e.sandbox.Load()
	if _, err := rt.pending.BeginLaunch(t.Context(), sandbox.PendingRun{
		TurnID: "run-code", AgentHandle: "swe", Role: "SWE",
	}, sandbox.Fence{}); err != nil {
		t.Fatalf("BeginLaunch: %v", err)
	}

	coding := e.beginWorkingStatus(t.Context(), "swe", "run-code",
		[]*events.Event{chatTrigger("D0ANA")})
	if coding == nil {
		t.Fatal("no indicator was raised for a chat trigger")
	}
	ws.awaitShown(t, 1)
	endWorkingStatus(t.Context(), coding, true)

	// A colleague's ask lands in the same thread while the box runs, and it
	// is a different turn: the session is shared and reference-counted.
	alongside := e.beginWorkingStatus(t.Context(), "swe", "run-alongside",
		[]*events.Event{chatTrigger("D0ANA")})
	if alongside == nil {
		t.Fatal("a second turn in the same thread joined no session")
	}

	// The run is settled rather than resumed: its record is deleted, its box
	// reclaimed, and the turn suspended into it is over.
	if err := rt.coordinator.FailRun(t.Context(), "run-code",
		types.ExecutorFailureSuspensionUnrecorded,
		"the suspended conversation could not be written"); err != nil {
		t.Fatalf("FailRun: %v", err)
	}

	if got := ws.shown(); slices.Contains(got, "") {
		t.Fatalf("the settle cleared an indicator a second turn is still holding: %v", got)
	}
	if live := e.notify.slack.Status().Live(); len(live) != 1 {
		t.Fatalf("the second turn's indicator went down with the settle: %v", live)
	}
	// The settled turn's hold is gone: still counted, the clear below would
	// never come.
	endWorkingStatus(t.Context(), alongside, false)
	if shown := ws.shown(); shown[len(shown)-1] != "" {
		t.Errorf("the settled turn's hold outlived the run it was held for: %v", shown)
	}
	if live := e.notify.slack.Status().Live(); len(live) != 0 {
		t.Errorf("an indicator outlived every turn holding it: %v", live)
	}
}

// AND A PERSON'S ANSWER RAISES ONE AGAIN, since the park released the hold and
// a rejoin-only resume would work through the answer in silence. An answer is
// an ordinary chat message carrying its conversation; a box's completion is
// not, and must not claim a thread it cannot prove it is in.
func TestAnAnsweredClarificationRaisesTheIndicatorAgain(t *testing.T) {
	t.Parallel()
	e, ws := indicating(t, notify.StatusAlways)

	answered, rejoined := e.resumeWorkingStatus(t.Context(), "swe", "run-code", chatTrigger("D0ANA"))
	if answered == nil {
		t.Fatal("the answer that resumed a parked run raised no indicator")
	}
	// And it reports no rejoin, which the caller's retry rule reads
	// ([TestAResumeThatHandsWorkBackToAPersonClearsItsIndicator]).
	if rejoined {
		t.Error("the answer route reported a rejoined hold, so a retry would keep " +
			"an indicator up over a wait on the person who just answered")
	}
	if got := ws.awaitShown(t, 1)[0]; got == "" {
		t.Errorf("the workspace was asked for %q, want the indicator raised", got)
	}

	// A completion carries no conversation and finds no hold: a parked
	// run's row keeps no chat metadata.
	completion := events.New(types.ExecutorFinished{TurnID: "run-box"}, events.TraceContext{})
	if s, rejoined := e.resumeWorkingStatus(t.Context(), "swe", "run-box", completion); s != nil || rejoined {
		t.Errorf("a box's completion raised %v (rejoined %v), want neither",
			s.Conversation(), rejoined)
	}
}

// A RESUMED TURN TAKES BACK THE INDICATOR ITS SUSPENSION KEPT ALIVE: it raises
// no second one (the hold is one turn id, so a rejoin is idempotent where a
// raise would reset the words), and it ends that hold when the turn ends.
func TestAResumedTurnRejoinsTheIndicatorItKeptAlive(t *testing.T) {
	t.Parallel()
	reviews := notify.PhasePhrases[phase.Review.String()]
	// The resumed turn's reviewer holds it open until its phase line lands,
	// as in [TestTheIndicatorFollowsTheTurnsPhases].
	var (
		e     *Engine
		ws    *workspace
		moved []string
	)
	e, ws = indicatingWith(t, notify.StatusAlways, scripted{
		waiting: func(reviewing bool) {
			if reviewing {
				moved = ws.awaitAnyOf(t, reviews)
			}
		},
	})
	company := e.Company()
	seat := company.Org.AgentSeatByHandle("swe")

	// The turn that suspended into its executor's run, holding its indicator
	// under the run's id.
	suspended := e.beginWorkingStatus(t.Context(), "swe", "run-1",
		[]*events.Event{chatTrigger("D0ANA")})
	ws.awaitShown(t, 1)
	endWorkingStatus(t.Context(), suspended, true)
	if got := ws.raises(); got != 1 {
		t.Fatalf("the suspended turn raised %d indicators, want one", got)
	}

	// The box reports and the resume re-enters on this node, runs the phases
	// the answer unblocks and ends — which is one of the endings that clears.
	err := e.resumeTurn(t.Context(), resumeInput{
		Company: company,
		Run: sandbox.PendingRun{
			TurnID: "run-1", WorkKey: "wk-1", AgentHandle: "swe", Reply: "tool",
			TaskDescription: "fix the failing test",
		},
		Turn: &turnctx.Turn{RunID: "run-1", WorkKey: "wk-1", Seat: seat, Org: company.Org,
			WorkSince: time.Now().UTC()},
		Answer: "the tests pass now",
	})
	if err != nil {
		t.Fatalf("resumeTurn: %v", err)
	}
	// Not a second raise, which would reset the words and need two Ends
	// to clear.
	if got := ws.raises(); got != 2 {
		t.Errorf("the resumed turn produced %d raises, want the one it kept "+
			"alive plus its reviewer's phase: %v", got, ws.shown())
	}
	// Its phases moved the words (the resume path's own OnPhase wiring),
	// asserted as a position as in [TestTheIndicatorFollowsTheTurnsPhases]:
	// second, straight after the line the suspension left, which also says
	// the rejoin did not redraw it.
	if len(moved) == 0 || !slices.Contains(notify.PhasePhrases[phase.Execute.String()], moved[0]) {
		t.Fatalf("the workspace first heard %v, want the line the suspended half left up", moved)
	}
	if at := firstOf(moved, reviews); at != 1 {
		t.Errorf("the resumed turn's reviewer wording sits at %d in %v, want it "+
			"second — straight after the hold it took back", at, moved)
	}
	if shown := ws.shown(); shown[len(shown)-1] != "" {
		t.Errorf("the resumed turn ended without clearing: %v", shown)
	}
	if live := e.notify.slack.Status().Live(); len(live) != 0 {
		t.Errorf("the resumed turn left an indicator up: %v", live)
	}
}

// A RESUME THAT NEVER REACHED ITS TURN KEEPS THE INDICATOR IT TOOK BACK. Each
// of [resumeRetries] is a retry: the coordinator reverts its claim, the box's
// work is intact and somebody is still waiting. Nothing else can put a cleared
// indicator back: a redelivered completion has only the hold, as a parked
// run's row carries no chat metadata. The completion route only; the answer
// route is the opposite
// ([TestAResumeThatHandsWorkBackToAPersonClearsItsIndicator]).
func TestAResumeThatCouldNotStartKeepsItsIndicator(t *testing.T) {
	t.Parallel()
	for name, derail := range resumeRetries {
		t.Run(name, func(t *testing.T) {
			e, ws := indicating(t, notify.StatusAlways)
			company := e.Company()
			seat := company.Org.AgentSeatByHandle("swe")

			suspended := e.beginWorkingStatus(t.Context(), "swe", "run-wk-1",
				[]*events.Event{chatTrigger("D0ANA")})
			if suspended == nil {
				t.Fatal("no indicator was raised for a chat trigger")
			}
			ws.awaitShown(t, 1)
			endWorkingStatus(t.Context(), suspended, true)

			err := e.resumeTurn(t.Context(), resumeInput{
				Company: company,
				Run: sandbox.PendingRun{
					TurnID: "run-wk-1", AgentHandle: "swe", Reply: derail(company),
					TaskDescription: "fix the failing test",
				},
				Turn: &turnctx.Turn{RunID: "run-wk-1", WorkKey: "wk-1", Seat: seat, Org: company.Org,
					WorkSince: time.Now().UTC()},
				Answer: "the tests pass now",
			})
			if err == nil {
				t.Fatal("the resume did not fail, so this case asserts nothing")
			}
			if got := ws.shown(); slices.Contains(got, "") {
				t.Errorf("a resume that will be retried cleared its indicator: %v", got)
			}
			if live := e.notify.slack.Status().Live(); len(live) != 1 {
				t.Errorf("live = %v, want the indicator the retry will take back", live)
			}
		})
	}
}

// resumeRetries are the three ways [Engine.resumeTurn] ends without ending its
// turn, each derailing the company it is handed and returning the run's Reply.
// Named by the return each takes: an unresolvable provider chain does not fail
// the runner build, it fails the phase's first call.
var resumeRetries = map[string]func(*Company) string{
	"a reply this build cannot read": func(*Company) string { return "nonsense" },
	"a runner that could not be built": func(c *Company) string {
		// No provider registry, the one shape RunnerFor refuses: a
		// revision removed every provider.
		c.Models = nil
		return "tool"
	},
	"a turn that broke without writing outside the engine": func(c *Company) string {
		// The chain builds and answers nothing, so the failure is the
		// executor's first round rather than the build.
		c.Org.AgentSeatByHandle("swe").LLM = org.ProviderKeys{"a key this company does not have"}
		return "tool"
	},
}

// AND A RESUME THAT HANDS THE WORK BACK TO A PERSON CLEARS ITS OWN: the claim
// reverts to where it came from, and an answer was claimed from a question
// still open, so the run awaits that person again and an indicator would lie
// to them. It must clear here: the revert reports no stop, and the answer's
// delivery is requeued rather than run, so no fresh turn replaces it; the next
// attempt raises its own.
func TestAResumeThatHandsWorkBackToAPersonClearsItsIndicator(t *testing.T) {
	t.Parallel()
	for name, derail := range resumeRetries {
		t.Run(name, func(t *testing.T) {
			e, ws := indicating(t, notify.StatusAlways)
			company := e.Company()
			seat := company.Org.AgentSeatByHandle("swe")

			// No hold to take back makes this the answer route: the park
			// released it, and the answer raises a fresh one.
			err := e.resumeTurn(t.Context(), resumeInput{
				Company: company,
				Run: sandbox.PendingRun{
					TurnID: "run-wk-code", AgentHandle: "swe", Reply: derail(company),
					TaskDescription: "fix the failing test",
					// Where the coordinator's claim will put it back.
					ClaimedFrom: sandbox.StatusAwaiting,
				},
				Turn: &turnctx.Turn{RunID: "run-wk-code", WorkKey: "wk-code", Seat: seat, Org: company.Org,
					WorkSince: time.Now().UTC()},
				Answer:  "use the release branch",
				Trigger: chatTrigger("D0ANA"),
			})
			if err == nil {
				t.Fatal("the resume did not fail, so this case asserts nothing")
			}
			// Only the clear is promised after the last holder ends (the
			// raise may have been cancelled), so this reads the record's end.
			if shown := ws.shown(); len(shown) == 0 || shown[len(shown)-1] != "" {
				t.Errorf("the workspace last heard %v, want the clear: the run is "+
					"awaiting the same person again", shown)
			}
			if live := e.notify.slack.Status().Live(); len(live) != 0 {
				t.Errorf("live = %v, want nothing: nothing is working behind an "+
					"indicator over a question a person has yet to answer", live)
			}
		})
	}
}

// THE METADATA IS THE TRIGGER'S OWN, taken rather than reconstructed, so the
// indicator, the thread block and the reply target agree on the conversation.
func TestTheIndicatorsConversationComesOffTheTrigger(t *testing.T) {
	t.Parallel()
	chat := chatTrigger("D0ANA")
	tracker := events.New(types.ExternalNotification{
		NotificationSource: "jira", SourceEventType: "comment", Sender: "ana",
		// A tracker's metadata addresses no chat conversation.
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

// A NODE THAT HANDS A SEAT ON TAKES ITS INDICATORS DOWN: a kept-alive
// indicator's resume lands on whichever node holds the seat when the box
// reports, so a node that released it would otherwise refresh "is thinking…"
// for a turn it is not running until the process died. The chat twin of the
// `terminated` event this hook publishes.
func TestReleasingASeatTakesItsIndicatorDown(t *testing.T) {
	t.Parallel()
	e, ws := indicating(t, notify.StatusAlways)

	s := e.beginWorkingStatus(t.Context(), "swe", "run-1",
		[]*events.Event{chatTrigger("D0ANA")})
	ws.awaitShown(t, 1)
	endWorkingStatus(t.Context(), s, true)
	if len(e.notify.slack.Status().Live()) != 1 {
		t.Fatal("the suspended turn's indicator is not live")
	}

	e.releaseSeat(t.Context(), placement.Seat{Handle: "swe"})

	if shown := ws.shown(); shown[len(shown)-1] != "" {
		t.Errorf("the released seat's indicator was not cleared: %v", shown)
	}
	if live := e.notify.slack.Status().Live(); len(live) != 0 {
		t.Errorf("a seat this node no longer runs still shows an indicator: %v", live)
	}
}

// A TEARDOWN TAKES A DETACHED CONTEXT: the ending is often the cancellation
// itself (a shed seat, a drained node, a wall-clock cap), and a clear on a dead
// context does nothing, leaving the indicator up until the backend expires it.
func TestTheClearSurvivesTheCancellationThatEndedTheTurn(t *testing.T) {
	t.Parallel()
	e, ws := indicating(t, notify.StatusAlways)
	ctx, cancel := context.WithCancel(t.Context())

	s := e.beginWorkingStatus(ctx, "swe", "run-cancel", []*events.Event{chatTrigger("D0ANA")})
	ws.awaitShown(t, 1)
	cancel()
	endWorkingStatus(ctx, s, false)

	if shown := ws.shown(); shown[len(shown)-1] != "" {
		t.Errorf("a turn ended by a cancellation left its indicator up: %v", shown)
	}
	if live := e.notify.slack.Status().Live(); len(live) != 0 {
		t.Errorf("the session survived its own teardown: %v", live)
	}
}
