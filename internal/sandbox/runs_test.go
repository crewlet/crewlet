package sandbox_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// launchedRun is turnID's run of seat swe, launching on store; it answers the
// run's job.
func launchedRun(t *testing.T, store *sandbox.CoordStore, turnID string) string {
	t.Helper()
	run, err := store.BeginLaunch(t.Context(), sandbox.PendingRun{TurnID: turnID, AgentHandle: "swe",
		Status: sandbox.StatusLaunching}, sandbox.Fence{})
	if err != nil {
		t.Fatalf("BeginLaunch %s: %v", turnID, err)
	}
	return run.LaunchID
}

// fileFrom files a wake named id for job on turnID's record, sent by the run
// from ("" for a wake from the seat's inbox).
func fileFrom(t *testing.T, store *sandbox.CoordStore, turnID, job, id, from string) (sandbox.RunNote, error) {
	t.Helper()
	return store.FileWake(t.Context(), turnID, sandbox.WakeFiling{Launch: job,
		Wake: sandbox.SealedWake{ID: id, Sealed: []byte(strings.Repeat("s", 64)), From: from}})
}

// A RUN'S MESSAGE IS AN ITEM OF ITS OWN, WITHIN ITS JOB'S BOUNDS. Another run's
// message opens an item no wake joins and that joins no wake, so it is handed
// over whole; a job takes two from other runs, a third refused whatever item
// room is left, a retry of one held answering its item; and a sender outside
// the id charset is refused.
//
// Mutation: let a message join the newest item (message-joins), or drop the
// job's bound on messages, and this goes red.
func TestARunsMessageIsAnItemOfItsOwnWithinItsJobsBounds(t *testing.T) {
	t.Parallel()
	store := sandbox.NewCoordStore(memory.NewFleet())
	job := launchedRun(t, store, "t1")
	wake, err := fileFrom(t, store, "t1", job, "w-1", "")
	if err != nil {
		t.Fatal(err)
	}
	first, err := fileFrom(t, store, "t1", job, "m-1", "t2")
	if err != nil {
		t.Fatal(err)
	}
	if first.ID == wake.ID || len(first.Wake.Wakes) != 1 {
		t.Errorf("a run's message joined the wake's item %s: %+v", wake.ID, first.Wake)
	}
	next, err := fileFrom(t, store, "t1", job, "w-2", "")
	if err != nil {
		t.Fatal(err)
	}
	if next.ID == first.ID {
		t.Errorf("a wake joined a run's message's item %s", first.ID)
	}
	if _, err := fileFrom(t, store, "t1", job, "m-2", "t3"); err != nil {
		t.Fatal(err)
	}
	if again, err := fileFrom(t, store, "t1", job, "m-1", "t2"); err != nil || again.ID != first.ID {
		t.Errorf("a retry of a message held answered %+v, %v, want its item", again, err)
	}
	if _, err := fileFrom(t, store, "t1", job, "m-3", "t2"); !errors.Is(err, sandbox.ErrRunMessagesSpent) {
		t.Errorf("a job's third message from other runs answered %v, want ErrRunMessagesSpent", err)
	}
	if _, err := fileFrom(t, store, "t1", job, "m-4", "t 2"); err == nil {
		t.Error("a message naming a sender outside the id charset was filed")
	}
}

// A JOB'S MESSAGES TO OTHER RUNS ARE COUNTED WITHIN THEIR BOUND. A job sends two;
// a third is refused, a send given back makes room again and never counts below
// none, and a job the record no longer names counts nothing.
//
// Mutation: count per turn rather than per job, or let the count pass its
// bound, and this goes red.
func TestAJobsMessagesToOtherRunsAreCountedWithinTheirBound(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store := sandbox.NewCoordStore(memory.NewFleet())
	job := launchedRun(t, store, "t1")
	for range sandbox.MaxJobRunSends {
		if err := store.CountRunSend(ctx, "t1", job, 1); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.CountRunSend(ctx, "t1", job, 1); !errors.Is(err, sandbox.ErrRunSendsSpent) {
		t.Errorf("a job's third send answered %v, want ErrRunSendsSpent", err)
	}
	if err := store.CountRunSend(ctx, "t1", job, -1); err != nil {
		t.Fatal(err)
	}
	if err := store.CountRunSend(ctx, "t1", job, 1); err != nil {
		t.Errorf("a send given back left no room: %v", err)
	}
	if err := store.CountRunSend(ctx, "t1", "job-9", 1); !errors.Is(err, sandbox.ErrWakeClosed) {
		t.Errorf("a send for a job the record does not name answered %v, want ErrWakeClosed", err)
	}
	run, _, err := store.Get(ctx, "t1")
	if err != nil || run.RunSendsFor != job || run.RunSends != sandbox.MaxJobRunSends {
		t.Errorf("the record counts %d sends for %q (%v), want %d for the job", run.RunSends, run.RunSendsFor, err,
			sandbox.MaxJobRunSends)
	}
}

// TWO RUNS SHARE A REQUESTER ONLY WHERE THEIR CEILINGS NAME ONE. Whom a run works
// for is read as its ceiling composes it: a carried seat or credential first,
// then its requester, then its own work as its seat's; a stranger is nobody's,
// so two strangers' runs share none.
//
// Mutation: compare requesters by their handles alone (handle-only), or let two
// strangers share one, and this goes red.
func TestTwoRunsShareARequesterOnlyWhereTheirCeilingsNameOne(t *testing.T) {
	t.Parallel()
	seat := func(handle string) *types.WorkingFor {
		return &types.WorkingFor{Kind: types.WorkingForSeat, Seat: handle}
	}
	credential := func(login string, apps ...iam.Grant) *types.WorkingFor {
		return &types.WorkingFor{Kind: types.WorkingForCredential, Login: login, Apps: apps}
	}
	for name, tc := range map[string]struct {
		a, b sandbox.PendingRun
		same bool
	}{
		"one person": {sandbox.PendingRun{AgentHandle: "swe", Requester: "ana"},
			sandbox.PendingRun{AgentHandle: "swe", Requester: "ana"}, true},
		"two people": {sandbox.PendingRun{AgentHandle: "swe", Requester: "ana"},
			sandbox.PendingRun{AgentHandle: "swe", Requester: "bob"}, false},
		"a person and their work handed on": {sandbox.PendingRun{AgentHandle: "swe", Requester: "ana"},
			sandbox.PendingRun{AgentHandle: "swe", Requester: "cto", WorkingFor: seat("ana")}, true},
		"the seat's own work": {sandbox.PendingRun{AgentHandle: "swe"}, sandbox.PendingRun{AgentHandle: "swe"}, true},
		"two seats' own work": {sandbox.PendingRun{AgentHandle: "swe"}, sandbox.PendingRun{AgentHandle: "qa"}, false},
		"one credential": {sandbox.PendingRun{AgentHandle: "swe", WorkingFor: credential("ci", iam.Grant("worklet"))},
			sandbox.PendingRun{AgentHandle: "swe", RequesterLogin: "ci", RequesterApps: []iam.Grant{"worklet"}}, true},
		"two strangers": {sandbox.PendingRun{AgentHandle: "swe", WorkingFor: credential("ci")},
			sandbox.PendingRun{AgentHandle: "swe", WorkingFor: credential("ci")}, false},
	} {
		if got := sandbox.SameRequester(tc.a, tc.b); got != tc.same {
			t.Errorf("%s: SameRequester = %v, want %v", name, got, tc.same)
		}
	}
}

// A WAIT CLOSING A CYCLE OF RUNS IS FOUND, through any number of runs up to the
// walk's bound and as a cycle past it; a chain ending in a run waiting on
// nobody, or one not listed, is none.
//
// Mutation: follow one hop only (one-hop), and the three-run ring is missed.
func TestAWaitClosingACycleOfRunsIsFound(t *testing.T) {
	t.Parallel()
	waits := func(pairs ...string) map[string]sandbox.PendingRun {
		out := map[string]sandbox.PendingRun{}
		for i := 0; i+1 < len(pairs); i += 2 {
			out[pairs[i]] = sandbox.PendingRun{TurnID: pairs[i], WaitsFor: &sandbox.RunWait{Turn: pairs[i+1]}}
		}
		return out
	}
	long := []string{}
	for i := range sandbox.MaxRunWaitHops + 1 {
		long = append(long, "r"+string(rune('a'+i)), "r"+string(rune('a'+i+1)))
	}
	for name, tc := range map[string]struct {
		runs   map[string]sandbox.PendingRun
		target string
		cycle  bool
	}{
		"direct":          {waits("b", "a"), "b", true},
		"through a third": {waits("b", "c", "c", "a"), "b", true},
		"a chain to none": {waits("b", "c"), "b", false},
		"an unlisted run": {waits("b", "z"), "b", false},
		"past the bound":  {waits(long...), "ra", true},
	} {
		lookup := func(turn string) (sandbox.PendingRun, bool) { run, ok := tc.runs[turn]; return run, ok }
		if got := sandbox.WaitCycle("a", tc.target, lookup); got != tc.cycle {
			t.Errorf("%s: WaitCycle = %v, want %v", name, got, tc.cycle)
		}
	}
}

// A WAIT IS A QUESTION PUT TO NOBODY, RECORDED ON ITS JOB. Its reason is held to
// what the question leaves beside the engine's words and redacted; it names no
// audience nor choice, and a wait that also asks a person is refused; recorded,
// the run's stage is waiting, and the next job's opening clears it.
//
// Mutation: require a wait to name an audience (wait-asks-someone), and this
// goes red.
func TestAWaitIsAQuestionPutToNobodyRecordedOnItsJob(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store := sandbox.NewCoordStore(memory.NewFleet())
	job := launchedRun(t, store, "t1")
	wait := &sandbox.RunWait{Turn: "0190f3a8-0000-7000-8000-0000000000a1", Seat: "swe",
		Reason: strings.Repeat("x", sandbox.MaxRunWaitReasonBytes+1)}
	ask := sandbox.Ask{Question: sandbox.RunWaitQuestion(wait.Reason), Wait: wait}
	if _, err := store.RecordAsk(ctx, "t1", job, ask); err == nil {
		t.Error("a wait past its reason's bound was recorded")
	}
	wait.Reason = "it pushes the branch first"
	ask = sandbox.Ask{Question: sandbox.RunWaitQuestion(wait.Reason), Wait: wait, To: sandbox.AskToRequester}
	if _, err := store.RecordAsk(ctx, "t1", job, ask); err == nil {
		t.Error("a wait also put to a person was recorded")
	}
	ask.To = ""
	if got, err := store.RecordAsk(ctx, "t1", job, ask); err != nil || got != sandbox.AskRecorded {
		t.Fatalf("RecordAsk of a wait = %v, %v", got, err)
	}
	run, _, err := store.Get(ctx, "t1")
	if err != nil || run.WaitsFor == nil || run.WaitsFor.Turn != wait.Turn || run.Audience != "" ||
		run.Decision != nil || !run.AskedBy(job) {
		t.Fatalf("the wait is recorded as %+v (%v)", run.WaitsFor, err)
	}
	run.Status = sandbox.StatusAwaiting
	if stage := sandbox.StageOf(run); stage != sandbox.StageWaiting {
		t.Errorf("a parked wait's stage is %q, want %q", stage, sandbox.StageWaiting)
	}
}

// OTHER SEATS' RUNS ON THE WORK ARE ONE BOUNDED LISTING. A run's item, the items
// linked to it and its conversation find other seats' unfinished runs on them,
// never the caller's own seat's nor a run on other work, oldest first and at
// most [sandbox.MaxRunsAround]; work naming nothing finds none.
//
// Mutation: list the caller's own seat too (no-except), or drop the bound, and
// this goes red.
func TestOtherSeatsRunsOnTheWorkAreOneBoundedListing(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store := sandbox.NewCoordStore(memory.NewFleet())
	at := time.Date(2026, 11, 21, 9, 0, 0, 0, time.UTC)
	launch := func(turn, seat, item, conversation string, n int) {
		t.Helper()
		run := sandbox.PendingRun{TurnID: turn, AgentHandle: seat, Status: sandbox.StatusLaunching,
			ConversationKey: conversation, CreatedAt: at.Add(time.Duration(n) * time.Minute)}
		if item != "" {
			run.WorkItem = &types.WorkItem{Backend: types.WorkWorklet, ID: item}
		}
		if _, err := store.BeginLaunch(ctx, run, sandbox.Fence{}); err != nil {
			t.Fatalf("BeginLaunch %s: %v", turn, err)
		}
	}
	launch("own", "swe", "i-1", "chatlet:c1", 0)
	launch("same-item", "qa", "i-1", "", 3)
	launch("same-conversation", "pm", "", "chatlet:c1", 2)
	launch("linked", "ops", "i-2", "", 1)
	launch("elsewhere", "qa", "i-9", "chatlet:c9", 4)
	item := func(id string) string { return types.WorkItem{Backend: types.WorkWorklet, ID: id}.Ref() }
	on := sandbox.RunsOn{Item: item("i-1"), Linked: []string{item("i-2")}, Conversation: "chatlet:c1", Except: "swe"}
	runs, err := store.ListActiveOn(ctx, on)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, run := range runs {
		got = append(got, run.TurnID)
	}
	if want := []string{"linked", "same-conversation", "same-item"}; !slices.Equal(got, want) {
		t.Errorf("the runs on the work are %v, want %v", got, want)
	}
	for i := range sandbox.MaxRunsAround + 2 {
		launch(fmt.Sprintf("many-%02d", i), "qa", "i-1", "", 10+i)
	}
	if runs, err := store.ListActiveOn(ctx, on); err != nil || len(runs) != sandbox.MaxRunsAround {
		t.Errorf("a crowded item listed %d runs (%v), want %d", len(runs), err, sandbox.MaxRunsAround)
	}
	if runs, err := store.ListActiveOn(ctx, sandbox.RunsOn{Except: "swe"}); err != nil || len(runs) != 0 {
		t.Errorf("work naming nothing listed %d runs (%v)", len(runs), err)
	}
}

// countedRuns is the fleet's run records, counting the listings made of them
// and the reads of one run's record or of a part beside it.
type countedRuns struct {
	runStore
	listings, reads atomic.Int32
}

func (c *countedRuns) SandboxRuns(ctx context.Context) ([]coord.Record, error) {
	c.listings.Add(1)
	return c.runStore.SandboxRuns(ctx)
}

func (c *countedRuns) SandboxRun(ctx context.Context, turnID string) (coord.Record, bool, error) {
	c.reads.Add(1)
	return c.runStore.SandboxRun(ctx, turnID)
}

func (c *countedRuns) SandboxRunPart(ctx context.Context, turnID string, part coord.RunPart) (coord.Record, bool,
	error) {
	c.reads.Add(1)
	return c.runStore.SandboxRunPart(ctx, turnID, part)
}

// WHAT EITHER RUN WROTE FINDS IT, IN ONE LISTING OF THE ROWS. A run answering a
// thread files a ticket that wakes another seat: the ticket's run finds the
// thread's by the ticket in what that run wrote, and the thread's run finds the
// ticket's by what its own row says it wrote, each in one listing of the
// fleet's runs and no read of a run's record or log; another seat's run whose
// calls wrote other work, and that is on other work, is found by neither.
//
// Mutation: match the written records nowhere (written-unmatched), keep them on
// the log alone (log-only), or read each run's log in the listing, and this
// goes red.
func TestWhatEitherRunWroteFindsItInOneListingOfTheRows(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	runs := &countedRuns{runStore: memory.NewFleet()}
	store := sandbox.NewCoordStore(runs)
	launch := func(turn, seat, item, conversation string) sandbox.PendingRun {
		t.Helper()
		run := sandbox.PendingRun{TurnID: turn, AgentHandle: seat, Status: sandbox.StatusLaunching,
			ConversationKey: conversation}
		if item != "" {
			run.WorkItem = &types.WorkItem{Backend: types.WorkWorklet, ID: item}
		}
		opened, err := store.BeginLaunch(ctx, run, sandbox.Fence{})
		if err != nil {
			t.Fatalf("BeginLaunch %s: %v", turn, err)
		}
		return opened
	}
	wrote := func(run sandbox.PendingRun, records ...types.WrittenRecord) {
		t.Helper()
		if _, err := store.AppendBridgeCall(ctx, run.TurnID, sandbox.BridgeAppend{Launch: run.LaunchID,
			Round: run.Round, Call: sandbox.BridgeCall{Name: "create_work_item", Seq: 1}, Wrote: records}); err != nil {
			t.Fatalf("AppendBridgeCall %s: %v", run.TurnID, err)
		}
	}
	ticket := types.WorkItem{Backend: types.WorkWorklet, ID: "T-1"}
	thread := launch("thread", "swe", "", "chatlet:room-1:msg-0")
	wrote(thread, types.ItemWritten(ticket))
	launch("ticket", "qa", "T-1", "worklet:T-1")
	other := launch("other", "ops", "T-9", "chatlet:room-9:msg-0")
	wrote(other, types.ItemWritten(types.WorkItem{Backend: types.WorkWorklet, ID: "T-8"}),
		types.ConversationWritten("chatlet:room-7:msg-0"))
	row, _, err := store.Get(ctx, "thread")
	if err != nil || len(row.WroteKeys) != 1 {
		t.Fatalf("the thread's run keeps %d keys of what it wrote (%v), want the ticket's", len(row.WroteKeys), err)
	}
	runs.listings.Store(0)
	runs.reads.Store(0)
	for _, c := range []struct {
		name string
		on   sandbox.RunsOn
		want string
	}{
		{"the ticket's run", sandbox.RunsOn{Item: ticket.Ref(), Conversation: "worklet:T-1", Except: "qa"}, "thread"},
		{"the thread's run", sandbox.RunsOn{Conversation: "chatlet:room-1:msg-0", Wrote: row.WroteKeys,
			Except: "swe"}, "ticket"},
	} {
		found, err := store.ListActiveOn(ctx, c.on)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, run := range found {
			got = append(got, run.TurnID)
		}
		if !slices.Equal(got, []string{c.want}) {
			t.Errorf("%s finds %v, want %s alone", c.name, got, c.want)
		}
	}
	if listings, reads := runs.listings.Load(), runs.reads.Load(); listings != 2 || reads != 0 {
		t.Errorf("two lookups made %d listings and %d reads of a run, want one listing each and no read",
			listings, reads)
	}
}
