package engine

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/prompts"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	"github.com/crewlet/crewlet/internal/chatlet"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/executorkeep"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/provenance"
	queuemem "github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/sandboxtest"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
	"github.com/crewlet/crewlet/internal/worklet"
)

// runsRig is a node holding seat swe's runs on one store, under a coordinator
// whose claude-code runner reads notes and whose quiet runner reads none.
type runsRig struct {
	e      *Engine
	store  *sandbox.CoordStore
	steers *runSteers
	desk   *runsDesk
}

func newRunsRig(t *testing.T) *runsRig {
	t.Helper()
	store := sandbox.NewCoordStore(coordmem.NewFleet())
	steers, _ := steersOver(t, steerRing, store)
	reads := sandbox.NewFakeRunner("claude-code")
	reads.TakesNotes = true
	manager, err := sandbox.NewManager(sandbox.ManagerOptions{
		Providers: map[sandbox.Placement]sandbox.Provider{sandbox.Direct: sandbox.NewFakeProvider()},
		Runners:   map[string]sandbox.Runner{"claude-code": reads, "quiet": sandbox.NewFakeRunner("quiet")},
	})
	if err != nil {
		t.Fatal(err)
	}
	coordinator, err := sandbox.NewCoordinator(sandbox.CoordinatorOptions{
		Answers: testAnswers(), Queue: queuemem.NewBroker().Client(), Pending: store, Manager: manager,
		Resume: nilResumer{}, Audience: nobodyAudience{}, Node: "n1", Pushes: noPushes{},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(coordinator.Stop)
	e := &Engine{id: "n1", runSteers: steers}
	e.sandbox.Store(&sandboxRuntime{pending: store, coordinator: coordinator})
	return &runsRig{e: e, store: store, steers: steers, desk: newRunsDesk(e)}
}

// launch is turn's run of seat swe working for requester under harness, asked
// task, charged to item; it answers the run's job.
func (r *runsRig) launch(t *testing.T, turn, requester, harness, task, item string) string {
	t.Helper()
	run := sandbox.PendingRun{TurnID: turn, AgentID: sweAgent, AgentHandle: "swe", Role: "SWE", Harness: harness,
		Requester: requester, TaskDescription: task, ConversationKey: "chatlet:c1", TraceID: "tr-1",
		CreatedAt: time.Now().UTC(), Steer: sandbox.RunSteer{Seat: r.steers.anchor(turn, sweAgent, "swe")}}
	if item != "" {
		run.WorkItem = &types.WorkItem{Backend: types.WorkWorklet, ID: item, Key: item}
	}
	opened, err := r.store.BeginLaunch(t.Context(), run, sandbox.Fence{})
	if err != nil {
		t.Fatalf("BeginLaunch %s: %v", turn, err)
	}
	if suspended, err := r.store.MarkSuspended(t.Context(), turn,
		sandbox.Suspension{State: json.RawMessage(`{"version":4}`), Iteration: 1}); err != nil || !suspended {
		t.Fatalf("MarkSuspended %s = %v, %v", turn, suspended, err)
	}
	return opened.LaunchID
}

// park parks turn's job on a question put to its requester.
func (r *runsRig) park(t *testing.T, turn, job string) {
	t.Helper()
	ask := sandboxtest.Question("Which branch?", sandbox.AskToRequester)
	if got, err := r.store.RecordAsk(t.Context(), turn, job, ask); err != nil || got != sandbox.AskRecorded {
		t.Fatalf("RecordAsk %s = %v, %v", turn, got, err)
	}
	if err := r.store.MarkAwaiting(t.Context(), turn, sandbox.Clarification{AskedAt: time.Now().UTC()}); err != nil {
		t.Fatalf("MarkAwaiting %s: %v", turn, err)
	}
}

// refusedFor is why err refused a call, "" for none.
func refusedFor(err error) builtin.RunRefusal {
	if refused, ok := errors.AsType[*builtin.RunRefused](err); ok {
		return refused.Reason
	}
	return ""
}

// LIST_RUNS SHOWS A TITLE ONLY FOR THE SAME REQUESTER. A run's task carries its
// requester's words, so a run of the same seat working for somebody else is
// shown by its stage and item alone, in `list_runs`' answer and in a launch's
// brief alike; one of the caller's own requester shows its title, inside the
// fence of what its turn was asked, its conversation and its start.
//
// Mutation: show every run's title (title-for-anyone), and this goes red.
func TestListRunsShowsATitleOnlyForTheSameRequester(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	r := newRunsRig(t)
	job := r.launch(t, "run-a", "ana", "claude-code", "fix the login page\nall of it", "ENG-1")
	r.launch(t, "run-b", "ana", "claude-code", "add the logout button\nand its tests", "ENG-1")
	r.launch(t, "run-c", "bob", "claude-code", "bob's private plan", "ENG-2")
	around, err := r.desk.Around(ctx, "run-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(around) != 2 {
		t.Fatalf("run-a is shown %d runs, want the seat's other two: %+v", len(around), around)
	}
	same, other := around[0], around[1]
	if same.Turn != "run-b" || !same.SameRequester || same.Title != "add the logout button" || same.Item != "ENG-1" ||
		!same.SameConversation || same.Conversation != "chatlet" || !same.TakesMessages ||
		same.Label.Source != provenance.SourceExecutorSession || same.Stage != string(sandbox.StageRunning) {
		t.Errorf("the same requester's run is shown as %+v", same)
	}
	if other.Turn != "run-c" || other.SameRequester || other.Title != "" || other.Conversation != "" ||
		other.Item != "ENG-2" || !other.Started.IsZero() {
		t.Errorf("another requester's run is shown as %+v, want its stage and item alone", other)
	}
	b := provenance.NewBoundary()
	brief := prompts.RunsAround(around, b)
	if strings.Contains(brief, "private plan") || !strings.Contains(brief, "a run for another requester") ||
		!slices.ContainsFunc(provenance.Fences(brief, b), func(f string) bool {
			return strings.Contains(f, "add the logout button") && strings.Contains(f, "source=executor_session")
		}) {
		t.Errorf("the brief lists the runs as:\n%s", brief)
	}
	list := builtin.NewRunTools(r.desk)[0]
	got, err := list.CallForTurn(turnctx.WithJob(ctx, job), &turnctx.Turn{RunID: "run-a", Boundary: b}, nil)
	if err != nil || got.Failed {
		t.Fatalf("list_runs = %+v, %v", got, err)
	}
	if strings.Contains(got.Output, "private plan") || !slices.ContainsFunc(provenance.Fences(got.Output, b),
		func(f string) bool { return strings.Contains(f, "add the logout button") }) {
		t.Errorf("list_runs answered:\n%s", got.Output)
	}
}

// A MESSAGE IS REFUSED ACROSS REQUESTERS, TO ITSELF, PAST ITS BOUNDS AND TO A RUN
// NOT WORKING. Words pass only between runs of one requester; a run sends two
// in a launch and a launch takes two, the third refused and none counted twice
// (a retry is one message, a send no run took is given back); and a run parked,
// one under a harness that reads no note, or none at all takes nothing.
//
// Mutation: drop the requester check (message-anyone), or the sender's bound,
// and this goes red.
func TestAMessageIsRefusedAcrossRequestersToItselfPastItsBoundsAndToARunNotWorking(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	r := newRunsRig(t)
	job := r.launch(t, "run-a", "ana", "claude-code", "fix the login page", "ENG-1")
	r.launch(t, "run-b", "ana", "claude-code", "add the logout button", "ENG-1")
	r.launch(t, "run-c", "bob", "claude-code", "bob's work", "")
	r.launch(t, "run-d", "ana", "quiet", "a quiet run", "")
	parked := r.launch(t, "run-e", "ana", "claude-code", "a parked run", "")
	r.park(t, "run-e", parked)
	other := r.launch(t, "run-f", "ana", "claude-code", "a third run of ana's", "")
	for target, want := range map[string]builtin.RunRefusal{
		"run-a": builtin.RunRefusedSelf, "run-c": builtin.RunRefusedRequester, "run-d": builtin.RunRefusedHarness,
		"run-e": builtin.RunRefusedNotWorking, "run-z": builtin.RunRefusedUnknown,
	} {
		if got := refusedFor(r.desk.Message(ctx, "run-a", job, target, "I am on the login form")); got != want {
			t.Errorf("a message to %s was refused %q, want %q", target, got, want)
		}
	}
	for _, text := range []string{"I am on the login form", "I pushed login-fix", "I am on the login form"} {
		if err := r.desk.Message(ctx, "run-a", job, "run-b", text); err != nil {
			t.Fatalf("message %q to run-b: %v", text, err)
		}
	}
	if got := refusedFor(r.desk.Message(ctx, "run-a", job, "run-b", "a third")); got != builtin.RunRefusedBound {
		t.Errorf("a launch's third message was refused %q, want bound", got)
	}
	if got := refusedFor(r.desk.Message(ctx, "run-f", other, "run-b", "hello")); got != builtin.RunRefusedBound {
		t.Errorf("a message to a launch holding two was refused %q, want bound", got)
	}
	if sent := mustRow(t, r.store, "run-f").RunSends; sent != 0 {
		t.Errorf("run-f's refused message is counted: it has sent %d", sent)
	}
	if sent := mustRow(t, r.store, "run-a").RunSends; sent != sandbox.MaxJobRunSends {
		t.Errorf("run-a has sent %d, want %d with its retry counted once", sent, sandbox.MaxJobRunSends)
	}
}

// ANOTHER RUN'S MESSAGE IS READ FENCED AT THE NEXT STEP, AS INFORMATION. It
// reaches the receiving job's next hook under a wake's heading, which names the
// sending run and its seat, the line saying it is information from a run of
// the same requester and never a request, its words inside the fence of the
// sending executor's words under the session's token; and its read records no
// delivery joined, since nothing of the seat's inbox waits on it.
//
// Mutation: open a run's message under the wake's seal (one-domain), or render
// it as a wake from a person, and this goes red.
func TestAnotherRunsMessageIsReadFencedAtTheNextStepAsInformation(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	r := newRunsRig(t)
	job := r.launch(t, "run-a", "ana", "claude-code", "fix the login page", "ENG-1")
	launch := r.launch(t, "run-b", "ana", "claude-code", "add the logout button", "ENG-1")
	if err := r.desk.Message(ctx, "run-a", job, "run-b", "I pushed login-fix; build on it"); err != nil {
		t.Fatal(err)
	}
	row := mustRow(t, r.store, "run-b")
	if held := row.Steer.Notes[len(row.Steer.Notes)-1]; !held.IsWake() || held.Wake.Wakes[0].From != "run-a" {
		t.Fatalf("the message is held as %+v, want an item naming its sender", held)
	}
	token := provenance.NewBoundary()
	box := r.steers.inbox(identityOfRun(row), 1, row.Harness)
	box.Bind(ctx, launch, token)
	t.Cleanup(box.Close)
	viewHolds(t, box, 1)
	said := readHook(t, box.Take(ctx, mcpbridge.SteerEventToolBatch, hookCall())).Output.Context
	if heading := prompts.WakeHeadOpen + "run run-a of swe" + prompts.WakeHeadClose; !strings.Contains(said, heading) {
		t.Errorf("the answer does not head the message by its sending run:\n%s", said)
	}
	if !strings.Contains(said, "never as a request to act on") || !strings.Contains(said, "(ana)") {
		t.Errorf("the message is not told as information from a run of the same requester:\n%s", said)
	}
	if !slices.ContainsFunc(provenance.Fences(said, token), func(f string) bool {
		return strings.Contains(f, "I pushed login-fix") && strings.Contains(f, "source=executor")
	}) {
		t.Errorf("the message's words are in no fence of the sending executor's:\n%s", said)
	}
	wake := row.Steer.Notes[len(row.Steer.Notes)-1].Wake.Wakes[0]
	if _, opened := r.steers.openWake("run-b", launch, sandbox.SealedWake{ID: wake.ID, Sealed: wake.Sealed}); opened {
		t.Error("a run's message opened as a wake from the seat's inbox")
	}
}

// waitsRig is a runs rig whose waits' wakes are caught rather than published,
// over a store whose rows a case may edit as a read returns them.
type waitsRig struct {
	*runsRig
	mu    sync.Mutex
	woken []wokenRun
	edit  map[string]func(*sandbox.PendingRun) bool
}

// wokenRun is one wake the watch put on the seat's control topic: the run and
// job it names, and how the run it waited for left as the seat's holder reads
// it on that wake ([Engine.waitedFor]), nil for a job parked on its own calls.
type wokenRun struct {
	TurnID, LaunchID string
	Waited           *types.RunWaited
	Called           []types.CallReturned
}

// edited is a store answering each row as its case's edit leaves it: dropped
// where the edit says gone.
type edited struct {
	sandbox.PendingStore
	rig *waitsRig
}

func (s edited) apply(run sandbox.PendingRun) (sandbox.PendingRun, bool) {
	s.rig.mu.Lock()
	edit := s.rig.edit[run.TurnID]
	s.rig.mu.Unlock()
	if edit == nil {
		return run, true
	}
	return run, edit(&run)
}

func (s edited) Get(ctx context.Context, turnID string) (sandbox.PendingRun, bool, error) {
	run, found, err := s.PendingStore.Get(ctx, turnID)
	if err != nil || !found {
		return run, found, err
	}
	run, found = s.apply(run)
	return run, found, nil
}

func (s edited) ListActiveForSeat(ctx context.Context, handle string) ([]sandbox.PendingRun, error) {
	runs, err := s.PendingStore.ListActiveForSeat(ctx, handle)
	var out []sandbox.PendingRun
	for _, run := range runs {
		if run, kept := s.apply(run); kept {
			out = append(out, run)
		}
	}
	return out, err
}

func newWaitsRig(t *testing.T) *waitsRig {
	t.Helper()
	w := &waitsRig{runsRig: newRunsRig(t), edit: map[string]func(*sandbox.PendingRun) bool{}}
	rt := w.e.sandbox.Load()
	w.e.sandbox.Store(&sandboxRuntime{pending: edited{PendingStore: rt.pending, rig: w}, coordinator: rt.coordinator})
	w.e.runWaits.deliver = func(ctx context.Context, run sandbox.PendingRun) error {
		woke := wokenRun{TurnID: run.TurnID, LaunchID: run.LaunchID}
		if waited, left := w.e.waitedFor(ctx, run); left {
			woke.Waited = &waited
		}
		if run.WaitsOnOwnCalls() {
			woke.Called = run.CallsReturned()
		}
		w.mu.Lock()
		defer w.mu.Unlock()
		w.woken = append(w.woken, woke)
		return nil
	}
	return w
}

// answers is every wake the watch delivered so far.
func (w *waitsRig) answers() []wokenRun {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.woken)
}

// A WAIT PARKS AND IS ANSWERED ON EACH WAY ITS RUN LEAVES. A run's wait is
// recorded on its job as a question put to nobody, and nothing answers it while
// the run it waits for holds its slot; once that run parks on a person, ends
// with its end owed (the first lines of its reviewer's account told only to a
// run of the same requester), or is gone with nothing on record, the wait's own
// job is woken once, its holder reading how.
//
// Mutation: answer a wait while its run still holds a slot (answer-early), or
// tell another requester's report, and this goes red.
func TestAWaitParksAndIsAnsweredOnEachWayItsRunLeaves(t *testing.T) {
	t.Parallel()
	owed, err := json.Marshal(executorkeep.Ending{Outcome: executorkeep.OutcomeCompleted,
		CompletedWork: "landed the logout button\non branch logout"})
	if err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		requester string
		leave     func(*testing.T, *waitsRig, string)
		left      types.RunLeft
		outcome   string
		report    string
	}{
		"parks on a person": {requester: "ana", left: types.RunLeftParked,
			outcome: string(sandbox.StageParkedQuestion),
			leave:   func(t *testing.T, w *waitsRig, job string) { w.park(t, "run-t", job) }},
		"ends": {requester: "ana", left: types.RunLeftEnded, outcome: "completed",
			report: "landed the logout button\non branch logout",
			leave: func(_ *testing.T, w *waitsRig, _ string) {
				w.mu.Lock()
				defer w.mu.Unlock()
				w.edit["run-t"] = func(run *sandbox.PendingRun) bool {
					run.Ending, run.OwedEnd = &sandbox.RecordedEnding{ID: "e-1"}, owed
					return true
				}
			}},
		"ends for another requester": {requester: "bob", left: types.RunLeftEnded, outcome: "completed",
			leave: func(_ *testing.T, w *waitsRig, _ string) {
				w.mu.Lock()
				defer w.mu.Unlock()
				w.edit["run-t"] = func(run *sandbox.PendingRun) bool {
					run.Ending, run.OwedEnd = &sandbox.RecordedEnding{ID: "e-1"}, owed
					return true
				}
			}},
		"is gone": {requester: "ana", left: types.RunLeftGone,
			leave: func(_ *testing.T, w *waitsRig, _ string) {
				w.mu.Lock()
				defer w.mu.Unlock()
				w.edit["run-t"] = func(*sandbox.PendingRun) bool { return false }
			}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			w := newWaitsRig(t)
			waiter := w.launch(t, "run-w", "ana", "claude-code", "fix the login page", "ENG-1")
			target := w.launch(t, "run-t", tc.requester, "claude-code", "add the logout button", "ENG-1")
			if err := w.desk.Wait(ctx, "run-w", waiter, "run-t", "it pushes the branch I need", "login"); err != nil {
				t.Fatal(err)
			}
			row := mustRow(t, w.store, "run-w")
			if row.WaitsFor == nil || row.WaitsFor.Turn != "run-t" || row.Question == "" || row.Branch != "login" {
				t.Fatalf("the wait is recorded as %+v, %q", row.WaitsFor, row.Question)
			}
			if err := w.store.MarkAwaiting(ctx, "run-w", sandbox.Clarification{AskedAt: time.Now().UTC()}); err != nil {
				t.Fatal(err)
			}
			if waiting, err := w.e.answerWaits(ctx, "swe"); err != nil || waiting != 1 || len(w.answers()) != 0 {
				t.Fatalf("while its run works the wait is answered %v (%d waiting, %v)", w.answers(), waiting, err)
			}
			tc.leave(t, w, target)
			if _, err := w.e.answerWaits(ctx, "swe"); err != nil {
				t.Fatal(err)
			}
			if _, err := w.e.answerWaits(ctx, "swe"); err != nil {
				t.Fatal(err)
			}
			got := w.answers()
			if len(got) != 1 {
				t.Fatalf("the wait was answered %d times, want once: %+v", len(got), got)
			}
			given := got[0]
			if given.TurnID != "run-w" || given.LaunchID != waiter || given.Waited == nil {
				t.Fatalf("the wait is answered as %+v", given)
			}
			if waited := *given.Waited; waited.Turn != "run-t" || waited.Left != tc.left ||
				waited.Outcome != tc.outcome || waited.Report != tc.report {
				t.Errorf("the run waited for is said to have left as %+v", waited)
			}
		})
	}
}

// A WAIT THAT WOULD CLOSE A CYCLE OF RUNS IS REFUSED. A run waiting for one
// that waits on it, directly or through a third, is refused before anything is
// recorded, as is a wait on itself or on a run holding no slot; a wait on a
// chain of runs ending at one waiting on nobody is recorded.
//
// Mutation: check only a direct cycle (cycle-one-hop), and the three-run ring
// is recorded; walk one hop and call the rest a cycle, and the chain is refused.
func TestAWaitThatWouldCloseACycleOfRunsIsRefused(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	r := newRunsRig(t)
	jobs := map[string]string{}
	for _, turn := range []string{"run-a", "run-b", "run-c", "run-d"} {
		jobs[turn] = r.launch(t, turn, "ana", "claude-code", "work of "+turn, "")
	}
	parked := r.launch(t, "run-p", "ana", "claude-code", "a parked run", "")
	r.park(t, "run-p", parked)
	// run-b waits on run-c, and run-c on run-a, each still working as its
	// harness ends.
	for waiter, target := range map[string]string{"run-b": "run-c", "run-c": "run-a"} {
		if err := r.desk.Wait(ctx, waiter, jobs[waiter], target, "its branch comes first", ""); err != nil {
			t.Fatalf("%s waiting on %s: %v", waiter, target, err)
		}
	}
	for target, want := range map[string]builtin.RunRefusal{
		"run-c": builtin.RunRefusedCycle, "run-b": builtin.RunRefusedCycle, "run-a": builtin.RunRefusedSelf,
		"run-p": builtin.RunRefusedNotWorking,
	} {
		if got := refusedFor(r.desk.Wait(ctx, "run-a", jobs["run-a"], target, "its branch comes first", "")); got != want {
			t.Errorf("run-a waiting on %s was refused %q, want %q", target, got, want)
		}
	}
	if row := mustRow(t, r.store, "run-a"); row.WaitsFor != nil || row.Question != "" {
		t.Fatalf("a refused wait was recorded: %+v", row.WaitsFor)
	}
	// A CHAIN ENDING AT A RUN WAITING ON NOBODY is no cycle, however long:
	// run-c waits on run-a, which waits on nothing.
	if err := r.desk.Wait(ctx, "run-d", jobs["run-d"], "run-c", "its branch comes first", ""); err != nil {
		t.Errorf("a wait closing no cycle was refused: %v", err)
	}
}

// THE RUN CALLS COUNTER'S CATALOGUE ENTRY NAMES EVERY VALUE IT COUNTS but the
// run tools'. The metrics catalogue is a leaf the assistant imports, reading
// neither this package's values nor the tools' names, so its text names the
// tools by role (a rename moves nothing it says) and this holds the rest to
// what is counted: every outcome, every refusal's reason and the engine's own
// answer to a wait.
//
// Mutation: add a refusal reason or an outcome, or rename `wait_answered`,
// without the catalogue's text, and this is red.
func TestTheRunCallsCounterNamesEveryValueItCounts(t *testing.T) {
	t.Parallel()
	var shows string
	for _, in := range metrics.Catalogue() {
		if in.Name == metrics.SeatRunCalls {
			shows = in.Shows
		}
	}
	if shows == "" {
		t.Fatalf("the catalogue has no entry for %s", metrics.SeatRunCalls)
	}
	var counted []string
	for _, tool := range runCallTools {
		if !slices.Contains([]runCallTool{runToolList, runToolMessage, runToolWait}, tool) {
			counted = append(counted, string(tool))
		}
	}
	for _, outcome := range runCallOutcomes {
		counted = append(counted, string(outcome))
	}
	for _, reason := range builtin.RunRefusals {
		counted = append(counted, string(reason))
	}
	for _, value := range counted {
		if !strings.Contains(shows, "`"+value+"`") {
			t.Errorf("%s's catalogue text names no `%s`, a value it counts", metrics.SeatRunCalls, value)
		}
	}
}

// launchOn is turn's run of seat working for requester, charged to item and in
// conversation, under a harness that reads notes; it answers the run's job.
func (r *runsRig) launchOn(t *testing.T, turn, seat, requester, item, conversation string) string {
	t.Helper()
	agent := uuid.NewSHA1(uuid.NameSpaceOID, []byte(seat)).String()
	run := sandbox.PendingRun{TurnID: turn, AgentID: agent, AgentHandle: seat, Role: seat, Harness: "claude-code",
		Requester: requester, TaskDescription: "work of " + turn, ConversationKey: conversation, TraceID: "tr-1",
		CreatedAt: time.Now().UTC(), Steer: sandbox.RunSteer{Seat: r.steers.anchor(turn, agent, seat)}}
	if item != "" {
		run.WorkItem = &types.WorkItem{Backend: types.WorkWorklet, ID: item, Key: item}
	}
	opened, err := r.store.BeginLaunch(t.Context(), run, sandbox.Fence{})
	if err != nil {
		t.Fatalf("BeginLaunch %s: %v", turn, err)
	}
	if suspended, err := r.store.MarkSuspended(t.Context(), turn,
		sandbox.Suspension{State: json.RawMessage(`{"version":4}`), Iteration: 1}); err != nil || !suspended {
		t.Fatalf("MarkSuspended %s = %v, %v", turn, suspended, err)
	}
	return opened.LaunchID
}

// charted is r over the chart the audience cases read: agent seats cto, swe
// and qa, and ada, a person.
func (r *runsRig) charted(t *testing.T) *runsRig {
	t.Helper()
	r.e.epoch.current.Store(&Company{Org: audienceChart(t)})
	return r
}

// OTHER SEATS' RUNS ON THE SAME WORK ARE LISTED BY HOW THEY RELATE. A run is
// shown, after its own seat's, other seats' runs on its work item, in its
// conversation or on an item the tracker links to its item, each naming its
// seat and relation, another requester's by stage and item alone; a run of
// another seat on other work is not shown.
//
// Mutation: drop the linked items from the listing's work (own-item-only), or
// show another seat's run on other work, and this goes red.
func TestOtherSeatsRunsOnTheSameWorkAreListedByHowTheyRelate(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	r := newRunsRig(t)
	r.desk.links = func(context.Context, types.WorkItem) []string {
		return []string{types.WorkItem{Backend: types.WorkWorklet, ID: "ENG-2"}.Ref()}
	}
	r.launchOn(t, "run-a", "swe", "ana", "ENG-1", "chatlet:c1")
	r.launchOn(t, "run-own", "swe", "ana", "", "")
	r.launchOn(t, "run-item", "qa", "ana", "ENG-1", "")
	r.launchOn(t, "run-thread", "pm", "ana", "", "chatlet:c1")
	r.launchOn(t, "run-linked", "ops", "bob", "ENG-2", "")
	r.launchOn(t, "run-far", "qa", "ana", "ENG-9", "chatlet:c9")
	around, err := r.desk.Around(ctx, "run-a")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]prompts.RunAround{}
	var order []string
	for _, run := range around {
		got[run.Turn], order = run, append(order, run.Turn)
	}
	if len(order) != 4 || order[0] != "run-own" || !got["run-own"].OwnSeat {
		t.Fatalf("run-a is shown %v, want its seat's run first and three of other seats'", order)
	}
	for turn, want := range map[string]prompts.RunRelation{"run-item": prompts.RelationSameItem,
		"run-thread": prompts.RelationSameConversation, "run-linked": prompts.RelationLinkedItem} {
		if run := got[turn]; run.OwnSeat || run.Relation != want {
			t.Errorf("%s is shown as %+v, want another seat's run %s", turn, run, want)
		}
	}
	if linked := got["run-linked"]; linked.SameRequester || linked.Title != "" {
		t.Errorf("another requester's run of another seat is shown as %+v", linked)
	}
	if brief := prompts.RunsAround(around, provenance.NewBoundary()); !strings.Contains(brief,
		"turn `run-item` (seat qa), running, on the same work item as yours") {
		t.Errorf("the brief does not relate another seat's run:\n%s", brief)
	}
}

// recordWrote records a call of turn's job that created or wrote records, as
// the bridge records a call's update.
func (r *runsRig) recordWrote(t *testing.T, turn, job string, records ...types.WrittenRecord) {
	t.Helper()
	row := mustRow(t, r.store, turn)
	if _, err := r.store.AppendBridgeCall(t.Context(), turn, sandbox.BridgeAppend{Launch: job, Round: row.Round,
		Call:  sandbox.BridgeCall{Name: "create_work_item", Seq: 1, StartedAt: time.Now().UTC()},
		Wrote: records}); err != nil {
		t.Fatalf("AppendBridgeCall %s: %v", turn, err)
	}
}

// A RUN AND THE RUN ON THE TICKET IT FILED SEE EACH OTHER. A run answering a
// chat thread files a ticket and posts in a second thread through its tools;
// the ticket wakes another seat and a reply in the second thread a third. Each
// woken run is shown the thread's as the run that filed or wrote what it is
// on, and the thread's run, in its brief and `list_runs`, each of them as a run
// on what it filed or wrote, though none shares another's item or
// conversation. The keys are the real ones: a thread's as Chatlet records a
// post and stamps a reply in it, an item's conversation as the tracker names
// it. A run whose calls wrote other work is shown to none.
//
// Mutation: relate the runs by item and conversation alone (written-unmatched),
// or name both ways alike (one-way), and this goes red.
func TestARunAndTheRunOnTheTicketItFiledSeeEachOther(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	r := newRunsRig(t)
	threadOf := func(room, message string) string {
		return chatlet.MessageConversation(chatlet.Channel{ID: room, Kind: chatlet.KindPublic},
			chatlet.Message{ID: message})
	}
	ticket := types.WorkItem{Backend: types.WorkWorklet, ID: "ENG-7"}
	thread := r.launchOn(t, "run-thread", "swe", "ana", "", threadOf("room-1", "msg-0"))
	r.recordWrote(t, "run-thread", thread, types.ItemWritten(ticket),
		types.ConversationWritten(threadOf("room-2", "msg-4")))
	r.launchOn(t, "run-ticket", "qa", "ana", "ENG-7", worklet.ItemConversation("ENG-7"))
	r.launchOn(t, "run-reply", "pm", "ana", "", threadOf("room-2", "msg-4"))
	other := r.launchOn(t, "run-other", "ops", "ana", "ENG-9", threadOf("room-9", "msg-0"))
	r.recordWrote(t, "run-other", other, types.ItemWritten(types.WorkItem{Backend: types.WorkWorklet, ID: "ENG-8"}),
		types.ConversationWritten(threadOf("room-7", "msg-0")))
	for caller, want := range map[string]map[string]prompts.RunRelation{
		"run-ticket": {"run-thread": prompts.RelationWroteWhatYouAreOn},
		"run-reply":  {"run-thread": prompts.RelationWroteWhatYouAreOn},
		"run-thread": {"run-ticket": prompts.RelationOnWhatYouWrote, "run-reply": prompts.RelationOnWhatYouWrote},
		"run-other":  {},
	} {
		around, err := r.desk.Around(ctx, caller)
		if err != nil {
			t.Fatal(err)
		}
		got := map[string]prompts.RunRelation{}
		for _, run := range around {
			got[run.Turn] = run.Relation
		}
		if !maps.Equal(got, want) {
			t.Errorf("%s is shown %v, want %v", caller, got, want)
		}
	}
	b := provenance.NewBoundary()
	list := builtin.NewRunTools(r.desk)[0]
	got, err := list.CallForTurn(turnctx.WithJob(ctx, thread), &turnctx.Turn{RunID: "run-thread", Boundary: b}, nil)
	if err != nil || got.Failed {
		t.Fatalf("list_runs = %+v, %v", got, err)
	}
	if !strings.Contains(got.Output, `"`+string(prompts.RelationOnWhatYouWrote)+`"`) {
		t.Errorf("list_runs does not relate the woken runs to the thread's:\n%s", got.Output)
	}
	around, err := r.desk.Around(ctx, "run-ticket")
	if err != nil {
		t.Fatal(err)
	}
	if brief := prompts.RunsAround(around, b); !strings.Contains(brief,
		"turn `run-thread` (seat swe), running, it filed or wrote the work item or conversation you are on") {
		t.Errorf("the ticket's run's brief does not say the thread's run filed its work:\n%s", brief)
	}
}

// A MESSAGE TO ANOTHER SEAT'S RUN CARRIES ITS REQUESTER AND NEEDS REACH. A run
// messages another seat's run on its work only where both work for one
// requester and its seat may reach that seat as a colleague's ask does (an
// agent's seat the chart offers it the ask to, never a person's nor one the
// chart lacks), and the message carries the sending run's requester, composed
// from its row, and names its seat.
//
// Mutation: skip the reach for another seat (reach-anyone), or file the
// message carrying nobody, and this goes red.
func TestAMessageToAnotherSeatsRunCarriesItsRequesterAndNeedsReach(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	r := newRunsRig(t).charted(t)
	job := r.launchOn(t, "run-a", "swe", "founder", "ENG-1", "")
	r.launchOn(t, "run-qa", "qa", "founder", "ENG-1", "")
	r.launchOn(t, "run-bob", "qa", "bob", "ENG-1", "")
	r.launchOn(t, "run-ada", "ada", "founder", "ENG-1", "")
	r.launchOn(t, "run-ghost", "ghost", "founder", "ENG-1", "")
	for target, want := range map[string]builtin.RunRefusal{"run-bob": builtin.RunRefusedRequester,
		"run-ada": builtin.RunRefusedUnreachable, "run-ghost": builtin.RunRefusedUnreachable} {
		if got := refusedFor(r.desk.Message(ctx, "run-a", job, target, "I have ENG-1")); got != want {
			t.Errorf("a message to %s was refused %q, want %q", target, got, want)
		}
	}
	if err := r.desk.Message(ctx, "run-a", job, "run-qa", "I have ENG-1's API half"); err != nil {
		t.Fatalf("a message to another seat's run of the same requester: %v", err)
	}
	row := mustRow(t, r.store, "run-qa")
	item := row.Steer.Notes[len(row.Steer.Notes)-1]
	w, ok := r.steers.openWake("run-qa", row.LaunchID, item.Wake.Wakes[0])
	carrier := notify.WorkingForKey(&types.WorkingFor{Kind: types.WorkingForSeat, Seat: "founder"})
	if !ok || w.FromSeat != "swe" || w.FromRun != "run-a" || w.WorkingFor != carrier || w.By != "founder" {
		t.Errorf("the message opens as %+v (%v), want it from swe's run carrying founder", w, ok)
	}
}

// A WAIT CYCLE THROUGH ANOTHER SEAT'S RUN IS REFUSED. The walk follows each wait
// into another seat's runs, reading a run the caller is not shown, so a ring
// through two seats is refused as one within a seat is; a wait on another
// seat's run of another requester is refused before it.
//
// Mutation: walk only the runs the caller is shown (shown-only), and the ring
// through a run of another item is recorded.
func TestAWaitCycleThroughAnotherSeatsRunIsRefused(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	r := newRunsRig(t).charted(t)
	jobs := map[string]string{
		"run-a":   r.launchOn(t, "run-a", "swe", "founder", "ENG-1", ""),
		"run-b":   r.launchOn(t, "run-b", "qa", "founder", "ENG-1", ""),
		"run-d":   r.launchOn(t, "run-d", "qa", "founder", "ENG-7", ""),
		"run-bob": r.launchOn(t, "run-bob", "qa", "bob", "ENG-1", ""),
	}
	// run-b waits on run-d, its own seat's, and run-d on run-a, a run it is
	// not shown, as a record of its wait any writer could make.
	if err := r.desk.Wait(ctx, "run-b", jobs["run-b"], "run-d", "its migration first", ""); err != nil {
		t.Fatal(err)
	}
	wait := &sandbox.RunWait{Turn: "run-a", Seat: "swe", Reason: "the API first"}
	if got, err := r.store.RecordAsk(ctx, "run-d", jobs["run-d"], sandbox.Ask{
		Question: sandbox.RunWaitQuestion(wait.Reason), Wait: wait}); err != nil || got != sandbox.AskRecorded {
		t.Fatalf("RecordAsk of run-d's wait = %v, %v", got, err)
	}
	if got := refusedFor(r.desk.Wait(ctx, "run-a", jobs["run-a"], "run-b", "its tests first", "")); got !=
		builtin.RunRefusedCycle {
		t.Errorf("a ring through another seat's runs was refused %q, want cycle", got)
	}
	if got := refusedFor(r.desk.Wait(ctx, "run-a", jobs["run-a"], "run-bob", "its tests first", "")); got !=
		builtin.RunRefusedRequester {
		t.Errorf("a wait on another seat's run of another requester was refused %q, want requester", got)
	}
}

// A JOB PARKED ON ITS OWN CALL IS ANSWERED ONCE, AT ITS PARK. A call its
// harness was answered still running before its ceiling parks the job on it,
// put to nobody; nothing answers it while the job still works, and once its
// run is collected and parked (which waited for the call first) the wait is
// answered at once, as an answer by turn to that job naming the call, and
// only once.
//
// Mutation: answer a call wait while its job still works (answer-early), or
// leave it to the target-run branch, and this goes red.
func TestAJobParkedOnItsOwnCallIsAnsweredOnceAtItsPark(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	w := newWaitsRig(t)
	job := w.launch(t, "run-c", "ana", "claude-code", "fix the login page", "ENG-1")
	call := sandbox.CallWait{Tool: "delegate", Seq: 4}
	if outcome, err := w.store.RecordAsk(ctx, "run-c", job, sandbox.Ask{Call: &call}); err != nil ||
		outcome != sandbox.AskRecorded {
		t.Fatalf("parking the job on its call: %v, %v", outcome, err)
	}
	if waiting, err := w.e.answerWaits(ctx, "swe"); err != nil || len(w.answers()) != 0 {
		t.Fatalf("while its job still works the call wait is answered %v (%d waiting, %v)", w.answers(),
			waiting, err)
	}
	if err := w.store.MarkAwaiting(ctx, "run-c", sandbox.Clarification{AskedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := w.e.answerWaits(ctx, "swe"); err != nil {
			t.Fatal(err)
		}
	}
	got := w.answers()
	if len(got) != 1 {
		t.Fatalf("the call wait was answered %d times, want once: %+v", len(got), got)
	}
	if given := got[0]; given.TurnID != "run-c" || given.LaunchID != job || given.Waited != nil ||
		len(given.Called) != 1 || given.Called[0] != (types.CallReturned{Tool: "delegate", Seq: 4}) {
		t.Errorf("the call wait is answered as %+v", given)
	}
}

// A RUN PARKED ON ITS OWN CALLS HAS NOT LEFT ITS SEAT. The engine answers such
// a park at once and the run carries straight on, so to every other run it is
// working: a wait on it is not answered by the park, a new wait on it is
// taken, and it is listed as running, never as waiting for a run.
//
// Mutation: drop the own-calls case from waitedOf, and the waiting run is
// woken with a false account of a run that never left; drop it from the
// wait's refusal, and a wait on the run is refused; drop it from StageOf, and
// the run is listed as waiting_for_run.
func TestARunParkedOnItsOwnCallsHasNotLeftItsSeat(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	w := newWaitsRig(t)
	parked := w.launch(t, "run-c", "ana", "claude-code", "fix the login page", "ENG-1")
	waiter := w.launch(t, "run-w", "ana", "claude-code", "ship the release", "ENG-1")
	if err := w.desk.Wait(ctx, "run-w", waiter, "run-c", "its fix first", ""); err != nil {
		t.Fatal(err)
	}
	call := sandbox.CallWait{Tool: "delegate", Seq: 4}
	if outcome, err := w.store.RecordAsk(ctx, "run-c", parked, sandbox.Ask{Call: &call}); err != nil ||
		outcome != sandbox.AskRecorded {
		t.Fatalf("parking the job on its call: %v, %v", outcome, err)
	}
	for _, turn := range []string{"run-w", "run-c"} {
		if err := w.store.MarkAwaiting(ctx, turn, sandbox.Clarification{AskedAt: time.Now().UTC()}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := w.e.answerWaits(ctx, "swe"); err != nil {
		t.Fatal(err)
	}
	for _, given := range w.answers() {
		if given.TurnID == "run-w" {
			t.Errorf("the wait on a run parked on its own call was answered: %+v", given)
		}
	}
	if got := len(w.answers()); got != 1 {
		t.Errorf("%d answers were given, want the parked call's own: %+v", got, w.answers())
	}
	other := w.launch(t, "run-x", "ana", "claude-code", "update the docs", "ENG-1")
	if err := w.desk.Wait(ctx, "run-x", other, "run-c", "its fix first", ""); err != nil {
		t.Errorf("a wait on a run parked on its own call was refused: %v", err)
	}
	row, found, err := w.store.Get(ctx, "run-c")
	if err != nil || !found {
		t.Fatalf("Get: %v, %v", found, err)
	}
	if stage := sandbox.StageOf(row); stage != sandbox.StageRunning {
		t.Errorf("a run parked on its own call is listed as %q, want running", stage)
	}
}
