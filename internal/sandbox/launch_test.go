package sandbox

import (
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
			TurnID: turnID, AgentID: "a-1", AgentHandle: "swe", Role: "SWE",
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
		Queue: rig.queue, Pending: fenced, Manager: rig.manager,
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
		topics.AgentControl("swe"),
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

// The full brief lives on the row; the wire carries its first line as the
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

// The agent is told the concrete task, the wider goal it serves, and what its
// box already provides — the last so it does not spend rounds rediscovering
// it.
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

// THE CODING AGENT IS TOLD IT MAY ASK, and whom — the addendum the engine
// composes rides the brief the box is started with. Without it the ask shim was
// installed and never mentioned, so a run blocked on a decision could not park
// on a question at all.
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
	if len(started) != 1 || !strings.Contains(started[0].Brief, "crewlet-ask --to founder") {
		t.Fatalf("the coding agent's brief does not carry how to ask:\n%v", started)
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
		Queue: r.queue, Pending: r.pending, Manager: m,
		Resume: &resumeSpy{}, Audience: &audienceSpy{},
		Lease: leased(rigLease),
	})
	if err != nil {
		r.t.Fatalf("NewCoordinator: %v", err)
	}
	r.t.Cleanup(c.Stop)
	return c.Launch(ctx, m, req)
}

// A RUNNER THAT REFUSES THE PAIRING REFUSES THE LAUNCH BEFORE ANYTHING EXISTS:
// no row to settle, no box to reclaim, and the runner's own sentence carried
// to the caller — rather than a box provisioned, set up and started for an
// agent that exits at its first breath, its reason buried in its error stream.
//
// Mutation: ask the runner after the row is written, or after the box is
// created, and the row or the box is there.
func TestARefusedPairingBuildsNothing(t *testing.T) {
	t.Parallel()
	rig := newWaiterRig(t)
	rig.runner.AdmitErr = errors.New("run the engine as a non-root user")

	_, err := rig.launchVia(t.Context(), rig.manager, launchReq("t1"))
	if err == nil || !strings.Contains(err.Error(), "non-root user") {
		t.Fatalf("Launch: %v, want the runner's own refusal", err)
	}
	if _, found, gerr := rig.pending.Get(t.Context(), "t1"); gerr != nil || found {
		t.Errorf("a refused launch left a row (found %v, %v)", found, gerr)
	}
	rig.provider.mu.Lock()
	created := rig.provider.next
	rig.provider.mu.Unlock()
	if created != 0 {
		t.Errorf("a refused launch provisioned %d boxes", created)
	}
	if started := rig.runner.Started(); len(started) != 0 {
		t.Errorf("a refused launch started %d jobs", len(started))
	}
}

// THE RUNNER IS TOLD WHICH CELL ITS BOX IS IN, which decides how Claude Code is
// invoked there, and its admission is told what the BACKEND says about the
// box — whether its job runs as the engine's own user on the engine's host —
// rather than inferring it from the cell's name, which the in-process fake
// answers too.
//
// Mutation: hand Admit the cell's name instead of the backend's answer and
// the second launch's record goes red.
func TestTheRunnerIsToldItsCellAndWhatItsBackendRunsAs(t *testing.T) {
	t.Parallel()
	rig := newWaiterRig(t)
	req := launchReq("t1")
	req.Spec.Placement = Direct
	if _, err := rig.launchVia(t.Context(), rig.manager, req); err != nil {
		t.Fatal(err)
	}
	if started := rig.runner.Started(); len(started) != 1 || started[0].Placement != Direct {
		t.Errorf("the runner was started with %+v", started)
	}

	rig.provider.mu.Lock()
	rig.provider.AsEngine = true
	rig.provider.mu.Unlock()
	if _, err := rig.launchVia(t.Context(), rig.manager, launchReq("t2")); err != nil {
		t.Fatal(err)
	}
	rig.runner.mu.Lock()
	admitted := slices.Clone(rig.runner.AdmittedAsEngine)
	rig.runner.mu.Unlock()
	if !slices.Equal(admitted, []bool{false, true}) {
		t.Errorf("the admissions were told %v, want the fake's own answer each time", admitted)
	}
}

// EACH BACKEND SAYS WHAT ITS JOBS RUN AS. A direct box's job is a process of
// the engine's own user on the engine's host, so it is the one the answer is
// yes for; a container is a boundary, and so is a remote VM.
//
// Mutation: answer by the backend's kind rather than its placement and the
// container case goes red.
func TestOnlyADirectBoxRunsAsTheEngine(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		provider Provider
		want     bool
	}{
		{&Local{opts: LocalOptions{Placement: Direct}}, true},
		{&Local{opts: LocalOptions{Placement: Container}}, false},
		{&E2BProvider{}, false},
		{NewFakeProvider(), false},
	} {
		if got := tc.provider.RunsAsEngine(); got != tc.want {
			t.Errorf("%T %+v: RunsAsEngine = %v, want %v", tc.provider, tc.provider, got, tc.want)
		}
	}
}

// THE RUNNER IS TOLD WHICH OF ITS ENVIRONMENT IS TELEMETRY, at the values the
// environment carries, so one that can pin those values above a settings file
// in the box ([RunRequest.Telemetry]) pins what the launch decided — and a
// launch that adds none tells it none.
//
// Mutation: leave Telemetry off the run request, or fill it with the minted
// values rather than the merged ones, and this goes red.
func TestTheRunnerIsToldWhichOfItsEnvironmentIsTelemetry(t *testing.T) {
	t.Parallel()
	rig := newWaiterRig(t)
	receiver, err := NewOtelReceiver(OtelReceiverOptions{
		BaseURL: "https://engine.internal",
		Tokens:  NewOtelTokens(OtelTokenOptions{Key: []byte("k")}),
	})
	if err != nil {
		t.Fatal(err)
	}
	rig.runner.Exports = map[string]string{"CLAUDE_CODE_ENABLE_TELEMETRY": "1"}
	manager, err := NewManager(ManagerOptions{
		Providers: map[Placement]Provider{Direct: rig.provider},
		Runners:   map[string]Runner{"claude-code": rig.runner},
		Telemetry: receiver,
	})
	if err != nil {
		t.Fatal(err)
	}
	req := launchReq("t1")
	req.Spec.Env = map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "https://our-own.example", "GITHUB_TOKEN": "t"}
	if _, err := rig.launchVia(t.Context(), manager, req); err != nil {
		t.Fatal(err)
	}
	started := rig.runner.Started()
	if len(started) != 1 {
		t.Fatalf("started %d runs", len(started))
	}
	got := started[0]
	if got.Telemetry["CLAUDE_CODE_ENABLE_TELEMETRY"] != "1" ||
		got.Telemetry["OTEL_EXPORTER_OTLP_ENDPOINT"] != "https://our-own.example" {
		t.Errorf("the runner was told the telemetry is %v", got.Telemetry)
	}
	if _, ok := got.Telemetry["GITHUB_TOKEN"]; ok {
		t.Error("the operator's own variable was told to the runner as telemetry")
	}
	for name, value := range got.Telemetry {
		if got.Env[name] != value {
			t.Errorf("telemetry %s=%q, the environment %q", name, value, got.Env[name])
		}
	}

	// AND NONE WHERE THE LAUNCH ADDED NONE.
	if _, err := rig.launchVia(t.Context(), rig.manager, launchReq("t2")); err != nil {
		t.Fatal(err)
	}
	if started := rig.runner.Started(); started[len(started)-1].Telemetry != nil {
		t.Errorf("a run with no receiver was told telemetry %v", started[len(started)-1].Telemetry)
	}
}
