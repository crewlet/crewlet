package sandbox

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/queue/topics"
)

func launchReq(turnID string) LaunchRequest {
	return LaunchRequest{
		Turn: TurnRef{
			TurnID: turnID, AgentID: "a-1", AgentHandle: "swe", Role: "SWE",
			ConversationKey: "chat:C1", Reply: "tool",
			TraceID: "tr-1", SpanID: "sp-1",
		},
		Brief: "Clone example.com/acme/api and fix the failing test",
		Task:  "get CI green",
		Spec:  Spec{CodingAgent: "claude-code", PauseTTLSec: 1800},
	}
}

func TestALaunchStartsTheJobAndRecordsWhatOutlivesTheTurn(t *testing.T) {
	rig := newWaiterRig(t)

	res, err := Launch(t.Context(), rig.manager, rig.pending, rig.queue, launchReq("t1"))
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if res.SandboxID == "" || res.CommandID == "" {
		t.Fatalf("Launch = %+v, want a box and a command", res)
	}
	if res.Reused {
		t.Fatal("a fresh launch reported reusing a box")
	}
	run := rig.get("t1")
	// LAUNCHING, not running. The job is executing, and the turn that
	// started it has not yet written the conversation a resume re-enters —
	// so nothing may poll or claim it yet. See [StatusLaunching].
	if run.Status != StatusLaunching {
		t.Fatalf("status = %q, want %q", run.Status, StatusLaunching)
	}
	if run.SandboxID != res.SandboxID || run.CommandID != res.CommandID {
		t.Fatalf("the row does not name the job: %+v", run)
	}
	if run.TaskDescription != "get CI green" {
		t.Fatalf("the task was not persisted: %+v", run)
	}
	// THE DELIVERY OBLIGATION, persisted because the resumed turn never
	// sees its trigger: without it a turn somebody asked for comes back
	// from its coding run free to end in silence.
	if run.Reply != "tool" {
		t.Fatalf("who is waiting was not persisted: %+v", run)
	}
	if run.ConversationKey != "chat:C1" || run.TraceID != "tr-1" {
		t.Fatalf("the routing and trace were not persisted: %+v", run)
	}
	if !rig.runner.Installed(res.SandboxID) {
		t.Fatal("the coding agent was never installed in the box")
	}
}

// The panel reads the announcement; the seat's owner reads the control copy.
func TestALaunchIsAnnouncedAndRoutedToTheSeat(t *testing.T) {
	rig := newWaiterRig(t)
	if _, err := Launch(t.Context(), rig.manager, rig.pending, rig.queue, launchReq("t1")); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	got := rig.queue.topics()
	want := []string{
		topics.Event(types.SandboxRunStarted{}.EventType()),
		topics.AgentControl("swe"),
	}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("published to %v, want %v", got, want)
	}
}

// THE STARTED EVENT NAMES THE RUN BY THE ROW'S OWN TASK, WHOLE.
//
// A screen shows the event's value until the run record arrives and the
// record's task_description after it, so the two must be one value — and
// that value is on the row whole, so cutting it here would only give the
// same text a second, shorter spelling. The brief is neither: it is what the
// coding agent is told, not what the run is for.
func TestTheStartedEventNamesTheRunByTheRowsOwnTask(t *testing.T) {
	rig := newWaiterRig(t)
	req := launchReq("t1")
	req.Task = strings.Repeat("get CI green on every supported platform. ", 20) +
		"\nThen tell the requester which tests were flaky."
	if _, err := Launch(t.Context(), rig.manager, rig.pending, rig.queue, req); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	rig.queue.mu.Lock()
	payload := rig.queue.published[0].event.Data.(*types.SandboxRunStarted)
	rig.queue.mu.Unlock()

	if payload.Task != req.Task {
		t.Errorf("the started event names the run %q, want the turn's task whole: %q",
			payload.Task, req.Task)
	}
	if row := rig.get("t1"); payload.Task != row.TaskDescription {
		t.Errorf("the event says %q and the row says %q: a screen showing one "+
			"and then the other relabels the run under its reader",
			payload.Task, row.TaskDescription)
	}
}

// The agent is told the concrete task, the wider goal it serves, and what its
// box already provides — the last so it does not spend rounds rediscovering
// it.
//
// There is no success-criteria section: it came from the planner's declared
// criteria, and with one loop there is no separate plan to declare them. What
// "done" means is the executor's own brief, written by the frame that will
// read the answer.
func TestTheCodingAgentIsToldTheGoalAndItsEnvironment(t *testing.T) {
	rig := newWaiterRig(t)
	req := launchReq("t1")
	req.Setup = []SetupStep{{Name: "git-auth", Brief: "git is already authenticated."}}
	req.MCPServers = map[string]MCPServer{"linear": {Name: "linear"}}

	if _, err := Launch(t.Context(), rig.manager, rig.pending, rig.queue, req); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	started := rig.runner.Started()
	if len(started) != 1 {
		t.Fatalf("started %d runs, want 1", len(started))
	}
	brief := started[0].Brief
	for _, want := range []string{
		"fix the failing test", // the executor's own ask
		"get CI green",         // the wider task
		"git is already authenticated.",
		"linear",
	} {
		if !strings.Contains(brief, want) {
			t.Fatalf("the brief does not mention %q:\n%s", want, brief)
		}
	}
	if strings.Contains(brief, "Success criteria") {
		t.Errorf("the retired criteria section is still rendered:\n%s", brief)
	}
}

// The checkout is the expensive half of a coding run; a second call in one
// turn continues where the first stopped.
func TestASecondCallInOneTurnReusesTheBox(t *testing.T) {
	rig := newWaiterRig(t)
	first, err := Launch(t.Context(), rig.manager, rig.pending, rig.queue, launchReq("t1"))
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	req := launchReq("t1")
	req.ReuseBox = first.SandboxID
	second, err := Launch(t.Context(), rig.manager, rig.pending, rig.queue, req)
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if !second.Reused || second.SandboxID != first.SandboxID {
		t.Fatalf("second launch = %+v, want it on the first box", second)
	}
}

// A box that is gone is exactly the case the pushed branch exists for.
func TestAReuseOfAVanishedBoxFallsBackToAFreshOne(t *testing.T) {
	rig := newWaiterRig(t)
	req := launchReq("t1")
	req.ReuseBox = "box-that-was-reaped"

	res, err := Launch(t.Context(), rig.manager, rig.pending, rig.queue, req)
	if err != nil {
		t.Fatalf("a launch whose reuse target is gone must still run: %v", err)
	}
	if res.Reused || res.SandboxID == "" {
		t.Fatalf("Launch = %+v, want a fresh box", res)
	}
}

// A box that nothing names is billed for until its TTL and collected by
// nobody.
func TestABoxIsReclaimedWhenTheJobCannotStart(t *testing.T) {
	rig := newWaiterRig(t)
	rig.runner.StartErr = errors.New("the coding CLI is not installed")

	if _, err := Launch(t.Context(), rig.manager, rig.pending, rig.queue, launchReq("t1")); err == nil {
		t.Fatal("a launch whose job never started reported success")
	}
	if killed := rig.provider.KilledIDs(); len(killed) != 1 {
		t.Fatalf("killed %v, want the unreferenced box reclaimed", killed)
	}
	rig.finished("t1")
}

// A crash between the row and the box leaves a record recovery can act on;
// the reverse ordering leaves a box nothing names.
//
// Asserted at the moment the box is created rather than after the launch
// returns: a launch that fails closes the row it opened, so afterwards there is
// nothing to see either way, and only the provider's own view of the store
// can tell the two orderings apart.
func TestTheRowExistsBeforeTheBoxDoes(t *testing.T) {
	rig := newWaiterRig(t)
	witness := &rowWitness{FakeProvider: rig.provider, store: rig.pending, turnID: "t1"}
	manager, err := NewManager(ManagerOptions{
		Providers: map[Placement]Provider{Direct: witness},
		Runners:   map[string]Runner{"claude-code": rig.runner},
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	if _, err := Launch(t.Context(), manager, rig.pending, rig.queue, launchReq("t1")); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if !witness.checked {
		t.Fatal("the launch never created a box, so the ordering was not observed")
	}
	if !witness.found {
		t.Fatal("the box was created before the run's row existed: a crash there leaves a box nothing names")
	}
}

// rowWitness is a provider that looks for the run's row at the moment it is
// asked for a box.
type rowWitness struct {
	*FakeProvider
	store          PendingStore
	turnID         string
	checked, found bool
}

func (w *rowWitness) Create(ctx context.Context, spec Spec) (Sandbox, error) {
	_, found, err := w.store.Get(ctx, w.turnID)
	if err != nil {
		return nil, err
	}
	w.checked, w.found = true, found
	return w.FakeProvider.Create(ctx, spec)
}

// A launch that opened a row and then failed must CLOSE it. A run left
// launching is polled by nothing and claimed by nothing — it just holds its
// seat's busy count, and its box where it got that far, until the seat happens
// to move to another node and recovery reaps it.
func TestEveryFailedLaunchClosesTheRowItOpened(t *testing.T) {
	for _, tc := range []struct {
		name   string
		derail func(*waiterRig)
	}{
		{"the box cannot be provisioned", func(r *waiterRig) {
			r.provider.CreateErr = errors.New("no capacity")
		}},
		{"the coding agent cannot be started", func(r *waiterRig) {
			r.runner.StartErr = errors.New("the coding CLI is not installed")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newWaiterRig(t)
			tc.derail(rig)

			if _, err := Launch(t.Context(), rig.manager, rig.pending, rig.queue, launchReq("t1")); err == nil {
				t.Fatal("a launch that could not finish reported success")
			}
			// Closed means gone: a launching row left behind is polled by
			// nothing and claimed by nothing.
			rig.finished("t1")
		})
	}
}

func TestALaunchNeedsATurnAndABrief(t *testing.T) {
	rig := newWaiterRig(t)
	for _, req := range []LaunchRequest{
		{Brief: "do the thing"},
		launchWithoutBrief(),
	} {
		if _, err := Launch(t.Context(), rig.manager, rig.pending, rig.queue, req); err == nil {
			t.Fatalf("Launch(%+v) succeeded", req.Turn)
		}
	}
}

func launchWithoutBrief() LaunchRequest {
	req := launchReq("t1")
	req.Brief = "   "
	return req
}

// THE TURN'S INSTANT RIDES THE ROW. A resume re-enters the turn with no trigger
// left to re-derive when its operation ids can first have been minted, so the
// launch writes the instant its turn carried.
func TestALaunchRecordsTheInstantItsTurnCouldFirstMint(t *testing.T) {
	rig := newWaiterRig(t)
	req := launchReq("t1")
	req.Turn.TriggeredAt = rig.now.Add(-time.Hour)
	if _, err := Launch(t.Context(), rig.manager, rig.pending, rig.queue, req); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if got := rig.get("t1").TriggeredAt; !got.Equal(req.Turn.TriggeredAt) {
		t.Errorf("the row carries %v, want the turn's %v", got, req.Turn.TriggeredAt)
	}
}

// A TURN'S CODING RUNS ARE BOUNDED ACROSS ITS RESUMES. Each resume re-enters
// with a fresh tool loop, so a round relaunching on every resume is bounded by
// nothing the loop counts; the row counts the launches, and the one past the
// bound is refused before a box is provisioned or the row is touched, naming
// the count and the bound.
func TestALaunchPastTheTurnsBoundIsRefusedNamingIt(t *testing.T) {
	rig := newWaiterRig(t)
	req := launchReq("t1")
	req.MaxLaunches = 2
	for launch := range 2 {
		if _, err := Launch(t.Context(), rig.manager, rig.pending, rig.queue, req); err != nil {
			t.Fatalf("launch %d of 2: %v", launch+1, err)
		}
	}
	before := rig.get("t1")
	started := len(rig.runner.Started())

	_, err := Launch(t.Context(), rig.manager, rig.pending, rig.queue, req)
	var capped *LaunchCapError
	if !errors.As(err, &capped) || !errors.Is(err, ErrLaunchCap) ||
		capped.Launched != 2 || capped.Max != 2 {
		t.Fatalf("the third launch = %v, want a refusal at 2 of 2", err)
	}
	if !strings.Contains(err.Error(), "2") {
		t.Errorf("the refusal %q does not name the bound", err)
	}
	after := rig.get("t1")
	if after.LaunchID != before.LaunchID || after.Launches != 2 || len(rig.runner.Started()) != started {
		t.Errorf("the refused launch touched the run: launch %q→%q, count %d, jobs started %d→%d",
			before.LaunchID, after.LaunchID, after.Launches, started, len(rig.runner.Started()))
	}
}
