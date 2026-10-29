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
		Answers: rigAnswers(),
		Queue:   rig.queue, Pending: fenced, Manager: rig.manager,
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
	item := types.WorkItem{Backend: types.WorkWorklet, ID: "task-1", Key: "ENG-1", Project: "ENG"}
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

// The agent is asked the task, and told as a standing instruction what its box
// already provides — so it does not spend rounds rediscovering it — on the
// channel its CLI adds to its own system prompt. Each text carries its own
// half and nothing of the other's.
//
// There is no success-criteria section: it came from the planner's declared
// criteria, and with one loop there is no separate plan to declare them. What
// "done" means is the executor's own brief, written by the frame that will
// read the answer.
func TestTheCodingAgentIsToldItsTaskAndItsEnvironment(t *testing.T) {
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
		"fix the failing test", // the turn's ask
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

// A RUN'S STANDING INSTRUCTIONS LEAD WITH THE SEAT'S SYSTEM PROMPT: it reaches
// the runner ahead of the environment and the ask, whole, opened by the note
// that says it is the role rather than the coding tool's own prompt, and none
// of it in the brief — which is the turn's ask, once, as the runner hands it.
//
// AND THE RUN IS NAMED THE SAME WAY LIVE AND FROM ITS RECORD: by its caller's
// label where it gave one (the launch names its turn's task, since the brief
// opens with a heading), else by the brief's first line.
func TestARunsOwnSystemTextLeadsItsStandingInstructions(t *testing.T) {
	t.Parallel()
	const seat = "You are Agent SWE at Acme.\nSEAT-IDENTITY-MARKER"
	const ask = "## If you get blocked on a human decision\nCall ask_and_wait with to: founder"
	for name, tc := range map[string]struct{ label, named string }{
		"labelled":   {label: "Ship the login fix\nand tell Ana", named: "Ship the login fix"},
		"unlabelled": {},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			rig := newWaiterRig(t)
			req := launchReq("t1")
			req.System, req.Ask, req.Label = seat, ask, tc.label
			named := cmp.Or(tc.named, req.Brief)
			if _, err := rig.launchVia(t.Context(), rig.manager, req); err != nil {
				t.Fatalf("Launch: %v", err)
			}
			started := rig.runner.Started()
			if len(started) != 1 {
				t.Fatalf("started %d runs, want 1", len(started))
			}
			got := started[0]
			opens := agentRunNote + "\n\n" + seat + "\n\n## Your environment\n"
			if !strings.HasPrefix(got.System, opens) {
				t.Errorf("the system text opens\n%q\nwant\n%q", got.System, opens)
			}
			if !strings.HasSuffix(got.System, "\n\n"+ask) {
				t.Errorf("the ask is not the last standing instruction the launch adds:\n%s", got.System)
			}
			if got.Brief != req.Brief {
				t.Errorf("the brief is\n%q\nwant the turn's ask alone\n%q", got.Brief, req.Brief)
			}
			if strings.Contains(got.Brief, "SEAT-IDENTITY-MARKER") || strings.Contains(got.Brief, "ask_and_wait") {
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
// a run held the ask tool with no word of when to use it, so a run blocked on
// a decision could not park on a question at all.
//
// And the row records WHO ASKED, which is what the park resolves a question to
// "the requester" against days later.
func TestTheCodingAgentIsToldHowToAskAndTheRowWhoAsked(t *testing.T) {
	t.Parallel()
	rig := newWaiterRig(t)
	req := launchReq("t1")
	req.Ask = "## If you get blocked on a human decision\nCall ask_and_wait with to: founder"
	req.Turn.Requester = "ada"
	if _, err := rig.launchVia(t.Context(), rig.manager, req); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	started := rig.runner.Started()
	if len(started) != 1 || !strings.Contains(started[0].System, "ask_and_wait with to: founder") {
		t.Fatalf("the coding agent's standing instructions do not carry how to ask:\n%v", started)
	}
	if got := rig.get("t1").Requester; got != "ada" {
		t.Errorf("the row records requester %q, want the seat whose wake started the turn", got)
	}
}

// THE ROW CARRIES THE COLLEAGUE'S ASK THE TURN ANSWERS: the frame that ends a
// parked turn sees no trigger, and the ask on the row is what it replies to.
//
// Mutation: leave the launch's ask off the row, and the row carries none.
func TestTheRowCarriesTheColleaguesAskTheTurnAnswers(t *testing.T) {
	t.Parallel()
	rig := newWaiterRig(t)
	req := launchReq("t1")
	req.Turn.ColleagueAsk = &ColleagueAsk{ChannelID: "a2a-1", Question: "which branch?",
		Chain: []string{"ceo"}, TraceID: "tr-ask", SpanID: "sp-ask"}
	if _, err := rig.launchVia(t.Context(), rig.manager, req); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	got := rig.get("t1").ColleagueAsk
	if got == nil || got.ChannelID != "a2a-1" || got.Question != "which branch?" ||
		len(got.Chain) != 1 || got.TraceID != "tr-ask" || got.SpanID != "sp-ask" {
		t.Fatalf("the row carries ask %+v, want the launch's", got)
	}
	if again := launchReq("t2"); again.Turn.ColleagueAsk != nil {
		t.Fatal("the fixture's turn carries an ask of its own")
	}
	if _, err := rig.launchVia(t.Context(), rig.manager, launchReq("t2")); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if got := rig.get("t2").ColleagueAsk; got != nil {
		t.Fatalf("a turn no ask woke carries ask %+v", *got)
	}
}

// A LAUNCH NAMES ITS RUNNER, and one that names none is refused before its row
// exists: the runner is the CLI of the entry whose login and model the run
// carries, and a box running some default CLI could read neither.
func TestALaunchThatNamesNoCodingAgentIsRefused(t *testing.T) {
	t.Parallel()
	rig := newWaiterRig(t)
	req := launchReq("t1")
	req.Spec.CodingAgent = ""
	if _, err := rig.launchVia(t.Context(), rig.manager, req); err == nil {
		t.Fatal("a launch naming no coding agent started a run")
	}
	if _, found, err := rig.pending.Get(t.Context(), "t1"); err != nil || found {
		t.Fatalf("a refused launch left a row (found %v, err %v)", found, err)
	}
	if started := rig.runner.Started(); len(started) != 0 {
		t.Fatalf("a refused launch started %d jobs", len(started))
	}
}

// THE ENTRY THE RUN WAS LAUNCHED ON IS THE JOB'S, on its record: the member of
// the seat's executor chain that took it, which a fallback makes other than
// the head. The phase record its collection publishes is filed under it, so
// spend by provider names the entry that ran — and a relaunch on another entry
// files the next job under that one rather than carrying the first.
func TestAJobRecordsTheEntryItWasLaunchedOn(t *testing.T) {
	t.Parallel()
	rig := newWaiterRig(t)
	req := launchReq("t1")
	req.ProviderKey, req.Model = "fallback", "sonnet"
	if _, err := rig.launchVia(t.Context(), rig.manager, req); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if got := rig.get("t1").Launch; got.ProviderKey != "fallback" || got.Model != "sonnet" {
		t.Fatalf("the job's record = %+v, want the entry and model it was launched on", got)
	}
	rec := runPhase(rig.get("t1"), rig.get("t1").Launch, Result{Success: true}, time.Now())
	if rec.ProviderKey != "fallback" || rec.Model != "sonnet" {
		t.Errorf("the job's phase record is filed under %q/%q, want fallback/sonnet",
			rec.ProviderKey, rec.Model)
	}

	again := launchReq("t1")
	again.ProviderKey, again.Model = "head", "opus"
	if _, err := rig.launchVia(t.Context(), rig.manager, again); err != nil {
		t.Fatalf("relaunch: %v", err)
	}
	if got := rig.get("t1").Launch; got.ProviderKey != "head" || got.Model != "opus" {
		t.Errorf("the relaunched job's record = %+v, want its own entry, not the first job's", got)
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

// THE JOB IS NAMED BEFORE ITS BOX EXISTS. A run's bridge session
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
	req.MCPServers = map[string]MCPServer{
		"jira": {Name: "jira", Transport: TransportStdio, Command: "jira-mcp"},
	}
	// THE TURN'S NOTES RIDE THE ROW THE LAUNCH OPENS: the seat anchor, and
	// the brief's note read by the job the write names.
	req.SteerSeat = []byte("sealed-seat")
	req.BriefNotes = []RunNote{{ID: "note-1", Sealed: []byte("sealed-note"),
		Read: &NoteRead{Phase: "execute", Iteration: 1, Round: 1, Boundary: "brief"}}}
	req.Opened = func(job OpenedJob) (Opening, error) {
		witness.mu.Lock()
		defer witness.mu.Unlock()
		witness.named = append(witness.named, job.LaunchID)
		endpoint := "https://engine.example.com/mcp/" + job.LaunchID
		return Opening{
			Servers: map[string]MCPServer{"crewlet": {Name: "crewlet", Transport: TransportHTTP,
				URL: endpoint}},
			Steer: SteerWiring{Server: "crewlet", Tool: "steer_take", URL: endpoint + "/steer"},
		}, nil
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
	run := rig.get("t1")
	if len(witness.named) != 1 || witness.named[0] != run.LaunchID {
		t.Fatalf("the launch named %v, and the row holds job %q", witness.named, run.LaunchID)
	}

	// AND WHAT THE NAMING ANSWERS REACHES THE BOX beside the servers the
	// launch was given: an endpoint that names the job, which only exists
	// once the job does.
	started := rig.runner.Started()
	if len(started) != 1 {
		t.Fatalf("%d jobs started, want 1", len(started))
	}
	servers := started[0].MCPServers
	if servers["crewlet"].URL != "https://engine.example.com/mcp/"+run.LaunchID ||
		servers["jira"].Command != "jira-mcp" {
		t.Errorf("the box was given %+v, want the seat's own server and the job's endpoint", servers)
	}
	if !strings.Contains(started[0].System, "crewlet, jira") {
		t.Errorf("the run's standing instructions do not list the servers it was given:\n%s",
			started[0].System)
	}
	// AND WHERE THE JOB'S CLI ASKS FOR ITS TURN'S NOTES, as the naming
	// answered it — never persisted, so only the job it was minted for.
	if want := (SteerWiring{Server: "crewlet", Tool: "steer_take",
		URL: "https://engine.example.com/mcp/" + run.LaunchID + "/steer"}); started[0].Steer != want {
		t.Errorf("the job was handed steer wiring %+v, want %+v", started[0].Steer, want)
	}
	if string(run.Steer.Seat) != "sealed-seat" || len(run.Steer.Notes) != 1 ||
		run.Steer.Notes[0].Read == nil || run.Steer.Notes[0].Read.Launch != run.LaunchID ||
		run.Steer.Notes[0].Read.Boundary != "brief" {
		t.Errorf("the row the launch opened carries notes %+v, want the anchor and the brief's "+
			"note read by job %s", run.Steer, run.LaunchID)
	}
}

// A JOB THAT CANNOT BE NAMED TO WHAT SERVES ITS BOX IS NOT LAUNCHED. The naming
// is what mints the box's endpoint, and a run with no endpoint is a
// coding agent with none of the seat's tools: the launch is abandoned before
// any box exists, and its row ended, rather than started blind.
//
// Mutation: ignore the naming's error, and a box is created and its job run.
func TestALaunchWhoseJobCannotBeNamedStartsNothing(t *testing.T) {
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
	req.Opened = func(OpenedJob) (Opening, error) {
		return Opening{}, errors.New("no endpoint for the job")
	}
	if _, err := rig.launchVia(t.Context(), manager, req); err == nil {
		t.Fatal("a launch whose job could not be named started")
	}
	witness.mu.Lock()
	created := witness.created
	witness.mu.Unlock()
	if created {
		t.Error("a box was created for a job nothing can serve")
	}
	if started := rig.runner.Started(); len(started) != 0 {
		t.Errorf("%d jobs started for a launch that failed", len(started))
	}
	if _, found, err := rig.pending.Get(t.Context(), "t1"); err != nil || found {
		t.Errorf("the abandoned launch left its row (found %v, err %v)", found, err)
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
		Answers: rigAnswers(),
		Queue:   r.queue, Pending: r.pending, Manager: m,
		Resume: &resumeSpy{}, Audience: &audienceSpy{},
		Lease: leased(rigLease),
	})
	if err != nil {
		r.t.Fatalf("NewCoordinator: %v", err)
	}
	r.t.Cleanup(c.Stop)
	return c.Launch(ctx, m, req)
}

// THE ANNOUNCEMENT NAMES THE MEMBER THAT TOOK THE RUN, and its model: a launch
// picks the first member of the seat's executor chain that can take it, so a
// fallback runs a job on an entry other than the chain's head — and which
// entry's plan a running job is spending is what a person watching it asks,
// long before its own record is collected.
//
// Mutation: drop either field from the announcement and this goes red.
func TestTheStartedEventNamesTheMemberThatTookTheRun(t *testing.T) {
	t.Parallel()
	rig := newWaiterRig(t)
	req := launchReq("t1")
	req.ProviderKey, req.Model = "spare", "claude-haiku"
	if _, err := rig.launchVia(t.Context(), rig.manager, req); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	rig.queue.mu.Lock()
	defer rig.queue.mu.Unlock()
	started := rig.queue.published[0].event.Data.(*types.SandboxRunStarted)
	if started.ProviderKey != "spare" || started.Model != "claude-haiku" {
		t.Fatalf("the announcement names %q/%q, want the member that took the run",
			started.ProviderKey, started.Model)
	}
}
