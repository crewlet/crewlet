package sandbox

import (
	"errors"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/queue/topics"
)

func launchReq(turnID string) LaunchRequest {
	return LaunchRequest{
		Turn: TurnRef{
			TurnID: turnID, AgentID: "a-1", AgentHandle: "swe", Role: "SWE",
			ConversationKey: "chat:C1", TraceID: "tr-1", SpanID: "sp-1",
		},
		Brief:    "Clone example.com/acme/api and fix the failing test",
		Task:     "get CI green",
		Criteria: []string{"the suite passes", "a PR is open"},
		Spec:     Spec{CodingAgent: "claude-code", PauseTTLSec: 1800},
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
	if run.TaskDescription != "get CI green" || len(run.SuccessCriteria) != 2 {
		t.Fatalf("the plan was not persisted: %+v", run)
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

// The full brief lives on the row; the wire carries a label for one panel row.
func TestTheStartedEventCarriesALabelNotTheWholeBrief(t *testing.T) {
	rig := newWaiterRig(t)
	req := launchReq("t1")
	req.Brief = strings.Repeat("a very long brief. ", 40)
	if _, err := Launch(t.Context(), rig.manager, rig.pending, rig.queue, req); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	rig.queue.mu.Lock()
	defer rig.queue.mu.Unlock()
	payload := rig.queue.published[0].event.Data.(*types.SandboxRunStarted)
	if len(payload.Task) > briefSummaryLimit+4 {
		t.Fatalf("the started event carries %d characters of brief", len(payload.Task))
	}
	if !strings.HasSuffix(payload.Task, "…") {
		t.Fatalf("a truncated label does not say it was cut: %q", payload.Task)
	}
}

// The agent is told the task, the goal, what done means, and what its box
// already provides — the last so it does not spend rounds rediscovering it.
func TestTheCodingAgentIsToldTheGoalTheCriteriaAndItsEnvironment(t *testing.T) {
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
		"the suite passes",     // the criteria
		"git is already authenticated.",
		"linear",
	} {
		if !strings.Contains(brief, want) {
			t.Fatalf("the brief does not mention %q:\n%s", want, brief)
		}
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
	run := rig.get("t1")
	if run.Status != StatusFailed {
		t.Fatalf("status = %q, want %q", run.Status, StatusFailed)
	}
	if run.SandboxID != "" {
		t.Fatalf("the row still names a box that was killed: %q", run.SandboxID)
	}
}

// A crash between the row and the box leaves a record recovery can act on;
// the reverse ordering leaves a box nothing names.
func TestTheRowExistsBeforeTheBoxDoes(t *testing.T) {
	rig := newWaiterRig(t)
	rig.provider.CreateErr = errors.New("no capacity")

	if _, err := Launch(t.Context(), rig.manager, rig.pending, rig.queue, launchReq("t1")); err == nil {
		t.Fatal("a launch with no box reported success")
	}
	if _, found, err := rig.pending.Get(t.Context(), "t1"); err != nil || !found {
		t.Fatalf("Get = %v, %v; the row must exist so recovery has something to act on", found, err)
	}
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
			run := rig.get("t1")
			if run.Status == StatusLaunching {
				t.Fatalf("the row was left open at %q — nothing will ever poll or claim it", run.Status)
			}
			if run.Status != StatusFailed {
				t.Fatalf("status = %q, want %q", run.Status, StatusFailed)
			}
			if run.SandboxID != "" {
				t.Fatalf("the row still names a box: %q", run.SandboxID)
			}
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
