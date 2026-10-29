package sandbox

import (
	"cmp"
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/queue/topics"
)

func launchReq(turnID string) LaunchRequest {
	return LaunchRequest{
		Turn: TurnRef{
			TurnID: turnID, AgentID: sweID, AgentHandle: "swe", Role: "SWE",
			WorkKey: "wk-1", WorkSince: launchWorkSince,
			// A DIRECT MESSAGE, the one shape where the two keys differ:
			// the conversation an answer is matched on is the bare
			// channel, while the partition the kick-off arrived in is a
			// thread inside it. Equal ones here would let a launch that
			// copied one field onto both look correct.
			PartitionKey: "chat:D1:root-1", ConversationKey: "chat:D1",
			Reply:   "tool",
			TraceID: "tr-1", SpanID: "sp-1",
		},
		Brief: "Clone example.com/acme/api and fix the failing test",
		Wider: "get CI green",
		Task:  "get CI green",
		Spec:  Spec{CodingAgent: "claude-code", PauseTTLSec: 1800},
	}
}

// launchWorkSince is when the launching turn's unit of work began.
var launchWorkSince = time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)

// A LAUNCH IS STAMPED WITH THE LEASE ITS SEAT IS HELD UNDER, asked of the
// coordinator's own [CoordinatorOptions.Lease] rather than handed in by each
// launch path, so a path cannot forget it: the row carries the lease, and the
// node's every later write carries it back off the row — which is what lets
// the seat's next holder fence this node out. Launched unfenced, the row sat
// at the zero epoch, which constrains nothing.
func TestALaunchIsStampedWithTheLeaseItsSeatIsHeldUnder(t *testing.T) {
	t.Parallel()
	rig := newWaiterRig(t)
	if _, err := rig.launchVia(t.Context(), rig.manager, launchReq("t1")); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if got := rig.get("t1"); got.Owner != rigLease.Owner || got.OwnerEpoch != rigLease.Epoch {
		t.Fatalf("owner %q epoch %d, want the seat's lease %+v", got.Owner, got.OwnerEpoch, rigLease)
	}
	// AND A LAUNCH THAT FAILS ENDS ITS ROW UNDER IT, rather than unfenced.
	fenced := &finishFences{PendingStore: rig.pending}
	c, err := NewCoordinator(CoordinatorOptions{
		Pushes: &pushSpy{},
		Queue:  rig.queue, Pending: fenced, Manager: rig.manager,
		Resume: &resumeSpy{}, Audience: &audienceSpy{},
		Lease: leased(rigLease),
	})
	if err != nil {
		t.Fatalf("NewCoordinator: %v", err)
	}
	t.Cleanup(c.Stop)
	rig.provider.CreateErr = errors.New("no capacity")
	if _, err := c.Launch(t.Context(), rig.manager, launchReq("t2")); err == nil {
		t.Fatal("a launch whose box could not be made succeeded")
	}
	if !slices.Equal(fenced.fences, []Fence{rigLease}) {
		t.Fatalf("the failed launch ended its row under %+v, want the seat's lease", fenced.fences)
	}
}

// finishFences records the fence every ending is decided under.
type finishFences struct {
	PendingStore
	fences []Fence
}

func (s *finishFences) DecideEnding(ctx context.Context, turnID string, d Decision,
) (PendingRun, bool, error) {
	s.fences = append(s.fences, d.License.Fence)
	return s.PendingStore.DecideEnding(ctx, turnID, d)
}

func TestALaunchStartsTheJobAndRecordsWhatOutlivesTheTurn(t *testing.T) {
	t.Parallel()
	rig := newWaiterRig(t)

	res, err := rig.launchVia(t.Context(), rig.manager, launchReq("t1"))
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
		t.Fatalf("the brief was not persisted: %+v", run)
	}
	// THE DELIVERY OBLIGATION, persisted because the resumed turn never
	// sees its trigger: without it a turn somebody asked for comes back
	// from its coding run free to end in silence.
	if run.Reply != "tool" {
		t.Fatalf("who is waiting was not persisted: %+v", run)
	}
	// BOTH CONVERSATION VALUES, because the row asks two questions of them
	// and one construction site forgetting one is exactly how this field
	// was empty at every site for the life of the feature.
	if run.PartitionKey != "chat:D1:root-1" || run.TraceID != "tr-1" {
		t.Fatalf("the routing and trace were not persisted: %+v", run)
	}
	if run.ConversationKey != "chat:D1" {
		t.Fatalf("the run has nowhere to report back to: %+v", run)
	}
	// THE UNIT OF WORK AND WHEN IT BEGAN, together: the resumed turn
	// derives its writes' operation ids from both, and an id derived from
	// the key under another instant is a DIFFERENT operation from the one
	// the first half of the turn wrote under.
	if run.WorkKey != "wk-1" || !run.WorkSince.Equal(launchWorkSince) {
		t.Fatalf("the row keeps work %q begun at %s, want %q begun at %s",
			run.WorkKey, run.WorkSince, "wk-1", launchWorkSince)
	}
	if !rig.runner.Installed(res.SandboxID) {
		t.Fatal("the coding agent was never installed in the box")
	}
}

// The panel reads the announcement; the seat's owner reads the control copy.
func TestALaunchIsAnnouncedAndRoutedToTheSeat(t *testing.T) {
	t.Parallel()
	rig := newWaiterRig(t)
	if _, err := rig.launchVia(t.Context(), rig.manager, launchReq("t1")); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	got := rig.queue.topics()
	want := []string{
		topics.Event(types.SandboxRunStarted{}.EventType()),
		sweControl(),
	}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("published to %v, want %v", got, want)
	}
}

// A LAUNCH RECORDS THE ITEM ITS TURN IS ON, on the row and on the announcement.
//
// The row is what the resumed turn reads its item from — the event that
// resumes it names none — and the announcement is what a running-runs panel
// reads, which cannot join back to a turn that parked days ago.
func TestALaunchRecordsTheItemItsTurnIsOn(t *testing.T) {
	t.Parallel()
	rig := newWaiterRig(t)
	req := launchReq("t1")
	item := types.WorkItem{Backend: types.WorkNative, ID: "task-1", Key: "ENG-1", Project: "ENG"}
	req.Turn.WorkItem = &item
	if _, err := rig.launchVia(t.Context(), rig.manager, req); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if run := rig.get("t1"); run.WorkItem == nil || *run.WorkItem != item {
		t.Fatalf("the row records %+v, want the turn's item", run.WorkItem)
	}
	rig.queue.mu.Lock()
	defer rig.queue.mu.Unlock()
	started := rig.queue.published[0].event.Data.(*types.SandboxRunStarted)
	if started.WorkItem == nil || *started.WorkItem != item {
		t.Fatalf("the announcement names %+v, want the turn's item", started.WorkItem)
	}
}

// THE ANNOUNCEMENT NAMES ITS JOB: the launch id the store minted and when it
// recorded the launch — what pairs the start with the job's own phase record,
// and what a request for the running job's live output names. A turn can
// launch more than one job, so without it a watcher could not say which of
// them is still running.
func TestTheStartedEventNamesTheJobItAnnounces(t *testing.T) {
	t.Parallel()
	rig := newWaiterRig(t)
	if _, err := rig.launchVia(t.Context(), rig.manager, launchReq("t1")); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	run := rig.get("t1")
	rig.queue.mu.Lock()
	defer rig.queue.mu.Unlock()
	started := rig.queue.published[0].event.Data.(*types.SandboxRunStarted)
	if started.LaunchID == "" || started.LaunchID != run.LaunchID {
		t.Errorf("the announcement names launch %q; the row holds %q", started.LaunchID, run.LaunchID)
	}
	if !started.StartedAt.Equal(run.Launch.StartedAt) || started.StartedAt.IsZero() {
		t.Errorf("the announcement says the job started at %v; the row says %v",
			started.StartedAt, run.Launch.StartedAt)
	}
}

// The full brief goes to the run; the wire carries its first line as the
// run's label — WHOLE, because a row too narrow for it is the renderer's to
// elide, and a label cut to a width put half a sentence on the run's page as
// its title.
func TestTheStartedEventCarriesTheBriefsFirstLineWhole(t *testing.T) {
	t.Parallel()
	rig := newWaiterRig(t)
	req := launchReq("t1")
	first := strings.TrimSpace(strings.Repeat("a very long brief. ", 40))
	req.Brief = first + "\nThe goal it serves: the release.\n"
	if _, err := rig.launchVia(t.Context(), rig.manager, req); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	rig.queue.mu.Lock()
	defer rig.queue.mu.Unlock()
	payload := rig.queue.published[0].event.Data.(*types.SandboxRunStarted)
	if payload.Task != first {
		t.Fatalf("the started event's label is %q, want the brief's first line whole", payload.Task)
	}
}

// The agent is asked the concrete task and the wider goal it serves, and told
// as a standing instruction what its box already provides — so it does not
// spend rounds rediscovering it — on the channel its CLI adds to its own
// system prompt. Each text carries its own half and nothing of the other's.
//
// There is no success-criteria section: it came from the planner's declared
// criteria, and with one loop there is no separate plan to declare them. What
// "done" means is the executor's own brief, written by the frame that will
// read the answer.
func TestTheCodingAgentIsToldTheGoalAndItsEnvironment(t *testing.T) {
	t.Parallel()
	rig := newWaiterRig(t)
	req := launchReq("t1")
	req.Setup = []SetupStep{{Name: "git-auth", Brief: "git is already authenticated."}}
	req.MCPServers = map[string]MCPServer{"linear": {Name: "linear"}}

	if _, err := rig.launchVia(t.Context(), rig.manager, req); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	started := rig.runner.Started()
	if len(started) != 1 {
		t.Fatalf("started %d runs, want 1", len(started))
	}
	brief, system := started[0].Brief, started[0].System
	for _, want := range []string{
		"fix the failing test", // the executor's own ask
		"get CI green",         // the wider task
	} {
		if !strings.Contains(brief, want) {
			t.Errorf("the brief does not mention %q:\n%s", want, brief)
		}
		if strings.Contains(system, want) {
			t.Errorf("the system text repeats the task's %q:\n%s", want, system)
		}
	}
	for _, want := range []string{"## Your environment", "git is already authenticated.", "linear"} {
		if !strings.Contains(system, want) {
			t.Errorf("the system text does not mention %q:\n%s", want, system)
		}
		if strings.Contains(brief, want) {
			t.Errorf("the brief carries the standing %q:\n%s", want, brief)
		}
	}
	if strings.Contains(brief+system, "Success criteria") {
		t.Errorf("the retired criteria section is still rendered:\n%s\n%s", brief, system)
	}
}

// A RUN'S OWN SYSTEM TEXT LEADS ITS STANDING INSTRUCTIONS, and the task leads
// its brief.
//
// An agent-mode executor launches with the seat's system prompt: it reaches
// the runner ahead of the environment and the ask, whole, opened by the note
// that says it is the role rather than the coding tool's own prompt, and none
// of it in the brief. A run with no system text of its own (a run_sandbox job)
// gets the standing instructions alone, opening on the first of them rather
// than on an empty section or a note introducing nothing.
//
// AND THE RUN IS NAMED THE SAME WAY LIVE AND FROM ITS RECORD: by its caller's
// label where it gave one (an agent-mode executor names its turn's task, since
// its brief opens with a heading), else by the brief's first line.
func TestARunsOwnSystemTextLeadsItsStandingInstructions(t *testing.T) {
	t.Parallel()
	const seat = "You are Agent SWE at Acme.\nSEAT-IDENTITY-MARKER"
	const ask = "## If you get blocked on a human decision\nRun crewlet-ask --to founder"
	for name, tc := range map[string]struct {
		system, label, opens, named string
		// agent marks an agent-mode executor, whose brief is the turn's
		// own ask and so carries no wider task.
		agent bool
	}{
		"agent mode": {
			system: seat, label: "Ship the login fix\nand tell Ana", agent: true,
			opens: agentRunNote + "\n\n" + seat + "\n\n## Your environment\n", named: "Ship the login fix",
		},
		"run_sandbox": {system: "", opens: "## Your environment\n"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			rig := newWaiterRig(t)
			req := launchReq("t1")
			req.System, req.Ask, req.Label = tc.system, ask, tc.label
			if tc.agent {
				req.Wider = ""
			}
			named := cmp.Or(tc.named, req.Brief)
			if _, err := rig.launchVia(t.Context(), rig.manager, req); err != nil {
				t.Fatalf("Launch: %v", err)
			}
			started := rig.runner.Started()
			if len(started) != 1 {
				t.Fatalf("started %d runs, want 1", len(started))
			}
			got := started[0]
			if !strings.HasPrefix(got.System, tc.opens) {
				t.Errorf("the system text opens\n%q\nwant\n%q", got.System, tc.opens)
			}
			if !strings.HasSuffix(got.System, "\n\n"+ask) {
				t.Errorf("the ask is not the last standing instruction the launch adds:\n%s", got.System)
			}
			// THE TASK ONCE: an agent-mode executor's brief is the turn's
			// ask already, and a run_sandbox job's is one step of a goal
			// framed after it.
			want := req.Brief
			if !tc.agent {
				want += "\n\n## The wider task\n" + req.Wider
			}
			if got.Brief != want {
				t.Errorf("the brief is\n%q\nwant\n%q", got.Brief, want)
			}
			if strings.Contains(got.Brief, "SEAT-IDENTITY-MARKER") || strings.Contains(got.Brief, "crewlet-ask") {
				t.Errorf("standing instructions reached the brief:\n%s", got.Brief)
			}
			rig.queue.mu.Lock()
			defer rig.queue.mu.Unlock()
			if label := rig.queue.published[0].event.Data.(*types.SandboxRunStarted).Task; label != named {
				t.Errorf("the run is announced as %q, want %q", label, named)
			}
			if label := rig.get("t1").Label; label != named {
				t.Errorf("the run's record names it %q, want %q, the name it was announced by", label, named)
			}
		})
	}
}

// THE CODING AGENT IS TOLD IT MAY ASK, and whom — the addendum the engine
// composes rides the standing instructions the box is started with. Without it
// the ask shim was installed and never mentioned, so a run blocked on a
// decision could not park on a question at all.
//
// And the row records WHO ASKED, which is what the park resolves a question to
// "the requester" against days later.
func TestTheCodingAgentIsToldHowToAskAndTheRowWhoAsked(t *testing.T) {
	t.Parallel()
	rig := newWaiterRig(t)
	req := launchReq("t1")
	req.Ask = "## If you get blocked on a human decision\nRun crewlet-ask --to founder"
	req.Turn.Requester = "ada"
	if _, err := rig.launchVia(t.Context(), rig.manager, req); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	started := rig.runner.Started()
	if len(started) != 1 || !strings.Contains(started[0].System, "crewlet-ask --to founder") {
		t.Fatalf("the coding agent's standing instructions do not carry how to ask:\n%v", started)
	}
	if got := rig.get("t1").Requester; got != "ada" {
		t.Errorf("the row records requester %q, want the seat whose wake started the turn", got)
	}
}

// The checkout is the expensive half of a coding run; a second call in one
// turn continues where the first stopped.
func TestASecondCallInOneTurnReusesTheBox(t *testing.T) {
	t.Parallel()
	rig := newWaiterRig(t)
	first, err := rig.launchVia(t.Context(), rig.manager, launchReq("t1"))
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	req := launchReq("t1")
	req.ReuseBox = first.SandboxID
	second, err := rig.launchVia(t.Context(), rig.manager, req)
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if !second.Reused || second.SandboxID != first.SandboxID {
		t.Fatalf("second launch = %+v, want it on the first box", second)
	}
}

// A box that is gone is exactly the case the pushed branch exists for.
func TestAReuseOfAVanishedBoxFallsBackToAFreshOne(t *testing.T) {
	t.Parallel()
	rig := newWaiterRig(t)
	req := launchReq("t1")
	req.ReuseBox = "box-that-was-reaped"

	res, err := rig.launchVia(t.Context(), rig.manager, req)
	if err != nil {
		t.Fatalf("a launch whose reuse target is gone must still run: %v", err)
	}
	if res.Reused || res.SandboxID == "" {
		t.Fatalf("Launch = %+v, want a fresh box", res)
	}
}

// A box that nothing names is billed for until its TTL and collected by
// nobody. The launch kills its own box and the ending reclaims the box the row
// names, which here are one box — killed by both, which a provider answers as
// the reclaim it wanted — and nothing else is killed.
func TestABoxIsReclaimedWhenTheJobCannotStart(t *testing.T) {
	t.Parallel()
	rig := newWaiterRig(t)
	rig.runner.StartErr = errors.New("the coding CLI is not installed")

	if _, err := rig.launchVia(t.Context(), rig.manager, launchReq("t1")); err == nil {
		t.Fatal("a launch whose job never started reported success")
	}
	if killed := slices.Compact(rig.provider.KilledIDs()); len(killed) != 1 {
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
	t.Parallel()
	rig := newWaiterRig(t)
	witness := &rowWitness{FakeProvider: rig.provider, store: rig.pending, turnID: "t1"}
	manager, err := NewManager(ManagerOptions{
		Providers: map[Placement]Provider{Direct: witness},
		Runners:   map[string]Runner{"claude-code": rig.runner},
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	if _, err := rig.launchVia(t.Context(), manager, launchReq("t1")); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if !witness.checked {
		t.Fatal("the launch never created a box, so the ordering was not observed")
	}
	if !witness.found {
		t.Fatal("the box was created before the run's row existed: a crash there leaves a box nothing names")
	}
}

// THE JOB IS NAMED BEFORE ITS BOX EXISTS. An agent-mode run's bridge session
// files every call the box makes under the name a launch hands it, so the name
// has to reach it before the box can make one — and it has to be the name the
// row holds, which is the one the store minted. Learned later, from the row,
// a call that outlived its job would learn whichever job the row held by then.
//
// Mutation: call Opened after the box is created, or hand it anything but the
// store's answer, and this goes red.
func TestALaunchNamesItsJobBeforeItsBoxExists(t *testing.T) {
	t.Parallel()
	rig := newWaiterRig(t)
	witness := &openedWitness{FakeProvider: rig.provider}
	manager, err := NewManager(ManagerOptions{
		Providers: map[Placement]Provider{Direct: witness},
		Runners:   map[string]Runner{"claude-code": rig.runner},
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	req := launchReq("t1")
	req.Opened = func(launch string) {
		witness.mu.Lock()
		defer witness.mu.Unlock()
		witness.named = append(witness.named, launch)
	}
	if _, err := rig.launchVia(t.Context(), manager, req); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	witness.mu.Lock()
	defer witness.mu.Unlock()
	if !witness.created {
		t.Fatal("the launch never created a box, so the ordering was not observed")
	}
	if witness.namedAtCreate != 1 {
		t.Fatalf("the box was created after %d namings, want 1 — the job must be named first",
			witness.namedAtCreate)
	}
	if run := rig.get("t1"); len(witness.named) != 1 || witness.named[0] != run.LaunchID {
		t.Fatalf("the launch named %v, and the row holds job %q", witness.named, run.LaunchID)
	}
}

// openedWitness is a provider that counts, at the moment it is asked for a
// box, how many times the launch has named its job.
type openedWitness struct {
	*FakeProvider
	mu            sync.Mutex
	named         []string
	created       bool
	namedAtCreate int
}

func (w *openedWitness) Create(ctx context.Context, spec Spec) (Sandbox, error) {
	w.mu.Lock()
	w.created, w.namedAtCreate = true, len(w.named)
	w.mu.Unlock()
	return w.FakeProvider.Create(ctx, spec)
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
	t.Parallel()
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

			if _, err := rig.launchVia(t.Context(), rig.manager, launchReq("t1")); err == nil {
				t.Fatal("a launch that could not finish reported success")
			}
			// Closed means gone: a launching row left behind is polled by
			// nothing and claimed by nothing.
			rig.finished("t1")
		})
	}
}

func TestALaunchNeedsATurnAndABrief(t *testing.T) {
	t.Parallel()
	rig := newWaiterRig(t)
	for _, req := range []LaunchRequest{
		{Brief: "do the thing"},
		launchWithoutBrief(),
	} {
		if _, err := rig.launchVia(t.Context(), rig.manager, req); err == nil {
			t.Fatalf("Launch(%+v) succeeded", req.Turn)
		}
	}
}

func launchWithoutBrief() LaunchRequest {
	req := launchReq("t1")
	req.Brief = "   "
	return req
}

// launchVia launches through a coordinator of its own over the rig's store and
// queue, for a case about the launch rather than about what a coordinator
// counts: the rig's own coordinator, where it has one, is left untouched. The
// seat is held under [rigLease], as a real node's is.
func (r *waiterRig) launchVia(ctx context.Context, m *Manager, req LaunchRequest) (LaunchResult, error) {
	r.t.Helper()
	c, err := NewCoordinator(CoordinatorOptions{
		Pushes: &pushSpy{},
		Queue:  r.queue, Pending: r.pending, Manager: m,
		Resume: &resumeSpy{}, Audience: &audienceSpy{},
		Lease: leased(rigLease),
	})
	if err != nil {
		r.t.Fatalf("NewCoordinator: %v", err)
	}
	r.t.Cleanup(c.Stop)
	return c.Launch(ctx, m, req)
}
