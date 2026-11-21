package engine

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/prompts"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/executorkeep"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/provenance"
	queuemem "github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/sandboxtest"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
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
	box := r.steers.inbox(identityOfRun(row), 1)
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

// waitsRig is a runs rig whose waits' answers are caught rather than published,
// over a store whose rows a case may edit as a read returns them.
type waitsRig struct {
	*runsRig
	mu       sync.Mutex
	answered []types.ExecutorAnswerGiven
	edit     map[string]func(*sandbox.PendingRun) bool
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
	w.e.runWaits.deliver = func(_ context.Context, given types.ExecutorAnswerGiven) error {
		w.mu.Lock()
		defer w.mu.Unlock()
		w.answered = append(w.answered, given)
		return nil
	}
	return w
}

// answers is every wait's answer delivered so far.
func (w *waitsRig) answers() []types.ExecutorAnswerGiven {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.answered)
}

// A WAIT PARKS AND IS ANSWERED ON EACH WAY ITS RUN LEAVES. A run's wait is
// recorded on its job as a question put to nobody, and nothing answers it while
// the run it waits for holds its slot; once that run parks on a person, ends
// with its end owed (the first lines of its reviewer's account told only to a
// run of the same requester), or is gone with nothing on record, the wait is
// answered once, as an answer by turn to the wait's own job saying how.
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
			if given.TurnID != "run-w" || given.LaunchID != waiter || given.Waited == nil ||
				given.AnsweredByKind != string(iam.ActorSystem) {
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
