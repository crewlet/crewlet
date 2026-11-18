package sandbox

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/httpx/httpxtest"
)

// errBoxGoneMidRead is a collection whose box is GONE — the provider reclaimed
// it — which no retry can read, so the run is settled at once.
var errBoxGoneMidRead = fmt.Errorf("the box died mid-read: %w", ErrBoxGone)

// errBoxBlip is a collection that could not read a box that is still there.
var errBoxBlip = errors.New("envd: connection reset by peer")

// A BOX THAT COULD NOT BE READ BACK IS ASKED AGAIN. Nothing the tail writes
// happens before a collection succeeds, so handing the claim back leaves the
// run as the completion found it: running, its box alive, its turn suspended.
// Settling on the first failure destroyed the turn — and its charge and its
// record — over one transport blip.
func TestACollectionThatCouldNotReadTheBoxIsAskedAgain(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	run := rig.launch("t1")
	rig.coordinator.countRun("swe", StatusRunning)
	rig.runner.Finish(Result{Success: true, Text: "Outcome: succeeded", InputTokens: 900, OutputTokens: 100})
	rig.runner.CollectErr = errBoxBlip

	payload, ev := rig.completion("t1")
	if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); !errors.Is(err, errBoxBlip) {
		t.Fatalf("OnCompleted = %v; want the failure handed back so the completion is retried", err)
	}
	row := rig.get("t1")
	if row.Status != StatusRunning || row.Launch.CollectFailures != 1 {
		t.Fatalf("row %s with %d failures; want running again with the failure counted",
			row.Status, row.Launch.CollectFailures)
	}
	if !rig.coordinator.SeatHeldBySandbox("swe") {
		t.Error("the seat was freed under a run that is still being collected")
	}
	if len(rig.failures()) != 0 || len(rig.provider.KilledIDs()) != 0 {
		t.Errorf("a retried collection announced %v and killed %v", rig.failures(), rig.provider.KilledIDs())
	}
	if len(rig.resumer.requests) != 0 || rig.accountant.callCount() != 0 {
		t.Error("a failed collection resumed or charged anything")
	}

	// The retry reads it: collected, charged once, resumed.
	rig.runner.CollectErr = nil
	payload, ev = rig.completion("t1")
	if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
		t.Fatalf("the retried OnCompleted: %v", err)
	}
	if len(rig.resumer.requests) != 1 || rig.accountant.callCount() != 1 {
		t.Errorf("the retry resumed %d times and charged %d; want once each",
			len(rig.resumer.requests), rig.accountant.callCount())
	}
	rig.finished("t1")
	if killed := rig.provider.KilledIDs(); len(killed) != 1 || killed[0] != run.SandboxID {
		t.Errorf("killed %v; want the box reclaimed once the turn was done", killed)
	}
}

// BUT NOT FOR EVER. A box that stays unreadable is given up exactly as the
// poll gives up a box it cannot reach — at least MinConnectFailures attempts
// spanning ConnectGiveUp — and the count lives on the job's record, so a
// retry on another node or after a restart does not start a fresh allowance.
func TestACollectionThatKeepsFailingIsGivenUpAfterThePollsWindow(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	rig.launch("t1")
	rig.coordinator.countRun("swe", StatusRunning)
	rig.runner.Finish(Result{Success: true})
	rig.runner.CollectErr = errBoxBlip
	start := rig.now

	collect := func() error {
		payload, ev := rig.completion("t1")
		return rig.coordinator.OnCompleted(t.Context(), payload, ev)
	}
	if err := collect(); err == nil {
		t.Fatal("the first failure was not handed back")
	}
	// Inside the window, however many attempts: still retried.
	rig.now = start.Add(ConnectGiveUp / 2)
	if err := collect(); err == nil {
		t.Fatal("a failure inside the window was not handed back")
	}
	if got := rig.get("t1").Launch; got.CollectFailures != 2 || !got.CollectFailingSince.Equal(start.UTC()) {
		t.Fatalf("record %+v; want two failures dated from the first", got)
	}
	// Past it: given up, announced as unreachable, the seat freed.
	rig.now = start.Add(ConnectGiveUp)
	if err := collect(); err != nil {
		t.Fatalf("a failure past the window = %v; want the run settled", err)
	}
	failed := rig.failures()
	if len(failed) != 1 || failed[0].Reason != types.ExecutorFailureCollect {
		t.Fatalf("failures = %+v; want one %q", failed, types.ExecutorFailureCollect)
	}
	if rig.coordinator.SeatHeldBySandbox("swe") {
		t.Error("the seat stayed parked on a run that was given up")
	}
	rig.finished("t1")
}

// A COLLECTION THAT READ THE BOX ENDS THE RUN OF FAILURES BEHIND IT. The bound
// is on consecutive failures, as the poll's is: a box that failed once, then
// answered, and was handed back only because the turn could not be resumed is
// a box that is reachable, and its next failure starts a run of its own. The
// record kept the first failure's instant through the success, so one failure
// of the re-collection a minute later was already "past the window" and a box
// that had answered seconds before was given up as lost.
//
// Mutation: drop Collected from the release (or its clear in the store), and
// the last failure settles the run.
func TestACollectionThatReadTheBoxEndsTheRunOfFailures(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	rig.launch("t1")
	rig.coordinator.countRun("swe", StatusRunning)
	rig.runner.Finish(Result{Success: true, Text: "Outcome: succeeded"})
	start := rig.now
	collect := func() error {
		payload, ev := rig.completion("t1")
		return rig.coordinator.OnCompleted(t.Context(), payload, ev)
	}

	// A blip: counted.
	rig.runner.CollectErr = errBoxBlip
	if err := collect(); !errors.Is(err, errBoxBlip) {
		t.Fatalf("the first collection = %v; want it handed back", err)
	}
	// Fifteen seconds on the box answers, and the RESUME fails retryably:
	// the claim goes back with the job collected.
	rig.now = start.Add(15 * time.Second)
	rig.runner.CollectErr = nil
	rig.resumer.err = errors.New("the node lost the seat mid-resume")
	if err := collect(); err == nil {
		t.Fatal("a failed resume was not handed back for a retry")
	}
	if got := rig.get("t1").Launch; got.CollectFailures != 0 || !got.CollectFailingSince.IsZero() {
		t.Fatalf("record %+v after a collection that read the box; want the run of failures ended", got)
	}

	// Past the window from the FIRST failure, one more blip: the start of a
	// new run, not the end of the old one.
	rig.now = start.Add(ConnectGiveUp + 10*time.Second)
	rig.runner.CollectErr = errBoxBlip
	if err := collect(); !errors.Is(err, errBoxBlip) {
		t.Fatalf("one failure after a collection that read the box = %v; want it retried", err)
	}
	if failed := rig.failures(); len(failed) != 0 {
		t.Fatalf("a box that answered %s earlier was given up: %+v",
			ConnectGiveUp-5*time.Second, failed)
	}
	if got := rig.get("t1").Launch; got.CollectFailures != 1 ||
		!got.CollectFailingSince.Equal(rig.now.UTC()) {
		t.Errorf("record %+v; want one failure dated from now", got)
	}

	rig.runner.CollectErr, rig.resumer.err = nil, nil
	if err := collect(); err != nil {
		t.Fatalf("the retry: %v", err)
	}
	rig.finished("t1")
}

// A COLLECTION THIS NODE CANCELLED IS NOT THE BOX'S FAILURE. A drain, a
// restart or the seat moving away ends the delivery's context mid-read, and
// counted, that opened the failure window at the drain — so the first real
// failure after a restart minutes later was already past it, and a reachable
// run was given up after one attempt. It is handed back uncounted.
//
// Mutation: drop the cancellation branch, and the interrupted attempt is
// counted and dates the window.
func TestACollectionThisNodeCancelledIsHandedBackUncounted(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	rig.launch("t1")
	rig.coordinator.countRun("swe", StatusRunning)
	rig.runner.Finish(Result{Success: true})
	ctx, cancel := context.WithCancel(t.Context())
	rig.runner.CollectFunc = func(ctx context.Context) error {
		cancel() // the drain arrives mid-read
		return ctx.Err()
	}

	payload, ev := rig.completion("t1")
	if err := rig.coordinator.OnCompleted(ctx, payload, ev); err == nil {
		t.Fatal("an interrupted collection was not handed back for a retry")
	}
	row := rig.get("t1")
	if got := row.Launch; row.Status != StatusRunning || got.CollectFailures != 0 || !got.CollectFailingSince.IsZero() {
		t.Fatalf("row %s, record %+v; want running again with nothing counted", row.Status, got)
	}
	if len(rig.failures()) != 0 {
		t.Errorf("an interrupted collection announced %v", rig.failures())
	}

	// A DEADLINE IS COUNTED: a box whose collection always outlasts the
	// delivery would otherwise be retried for ever.
	rig.runner.CollectFunc = func(ctx context.Context) error {
		<-ctx.Done() // the read outlasts the delivery
		return ctx.Err()
	}
	short, stop := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer stop()
	payload, ev = rig.completion("t1")
	_ = rig.coordinator.OnCompleted(short, payload, ev)
	if got := rig.get("t1").Launch; got.CollectFailures != 1 {
		t.Errorf("record %+v after a collection that ran out of time; want it counted", got)
	}
	rig.runner.CollectFunc = nil
	payload, ev = rig.completion("t1")
	if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
		t.Fatalf("the retry: %v", err)
	}
	rig.finished("t1")
}

// A BOX THAT IS GONE IS SETTLED AT ONCE: its provider reclaimed it, so no
// attempt can read it, and the poll that fired for it has already waited the
// window out.
func TestACollectionWhoseBoxIsGoneIsSettledAtOnce(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	rig.launch("t1")
	rig.runner.Finish(Result{Success: true})
	rig.runner.CollectErr = errBoxGoneMidRead

	payload, ev := rig.completion("t1")
	if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
		t.Fatalf("OnCompleted = %v; want a gone box settled, not retried", err)
	}
	if failed := rig.failures(); len(failed) != 1 || failed[0].Reason != types.ExecutorFailureCollect {
		t.Fatalf("failures = %+v", failed)
	}
	rig.finished("t1")
}

// EVERY BACKEND SAYS A GONE BOX IS GONE, so the coordinator can tell one from
// a box it merely could not reach: a gone box is settled at once, an
// unreachable one retried for the poll's window. The fake, the engine host
// and E2B — the remote backend, and the one a box is most often reclaimed
// from — each on its own Connect and Attach.
//
// AND ONLY A GONE BOX: E2B's control plane failing (a 5xx) is a box that could
// not be reached this time, and settling it at once would destroy a run whose
// box is still there.
//
// Mutation: drop the boxGone wrap from E2B's Connect, and a reclaimed box is
// retried for a minute before it is settled.
func TestEveryBackendSaysAVanishedBoxIsGone(t *testing.T) {
	t.Parallel()
	type backend struct {
		name     string
		provider Provider
		id       string
	}
	backends := []backend{
		{"fake", NewFakeProvider(), "never-made"},
		{"local", newDirect(t), "0123456789abcdef"},
	}
	for _, status := range []int{http.StatusNotFound, http.StatusGone} {
		backends = append(backends, backend{fmt.Sprintf("e2b %d", status), e2bAnswering(t, status), "sbx-gone"})
	}
	for _, b := range backends {
		if _, err := b.provider.Connect(t.Context(), b.id); !errors.Is(err, ErrBoxGone) {
			t.Errorf("%s: Connect to a box that does not exist = %v; want ErrBoxGone", b.name, err)
		}
		if _, err := b.provider.Attach(t.Context(), b.id); !errors.Is(err, ErrBoxGone) {
			t.Errorf("%s: Attach to a box that does not exist = %v; want ErrBoxGone", b.name, err)
		}
	}

	failing := e2bAnswering(t, http.StatusServiceUnavailable)
	if _, err := failing.Connect(t.Context(), "sbx1"); err == nil || errors.Is(err, ErrBoxGone) {
		t.Errorf("Connect while E2B's control plane fails = %v; want an error that is not ErrBoxGone", err)
	}
	if _, err := failing.Attach(t.Context(), "sbx1"); err == nil || errors.Is(err, ErrBoxGone) {
		t.Errorf("Attach while E2B's control plane fails = %v; want an error that is not ErrBoxGone", err)
	}
}

// e2bAnswering is an E2B provider whose control plane answers every request
// with status.
func e2bAnswering(t *testing.T, status int) *E2BProvider {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = fmt.Fprintf(w, `{"code": %d, "message": "%s"}`, status, http.StatusText(status))
	}))
	t.Cleanup(server.Close)
	provider, err := NewE2B(E2BOptions{
		APIKey: "k", Domain: "test.invalid",
		HTTP: &http.Client{Transport: httpxtest.Rewrite(t, server)},
	})
	if err != nil {
		t.Fatal(err)
	}
	return provider
}

// callCount is how many charges the accountant was asked for.
func (l *ledgerSpy) callCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.calls
}

// errStoreUnanswered is what a call the coordination store did not answer
// returns: the words a request to the broker that timed out carries.
var errStoreUnanswered = errors.New("nats: timeout")

// silentStore is a run store whose coordination store does not answer the
// calls named in silent, for as long as they are named there, and answers
// every other call as the store beneath it does. "WatchLog" is every watch of
// a log; "WatchLog again" is a watch that stops hearing at once, delivering
// nothing, and the watch made again after it, which is not answered.
type silentStore struct {
	PendingStore
	mu      sync.Mutex
	silent  []string
	watches int
}

// silence names the calls the store stops answering.
func (s *silentStore) silence(calls ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.silent = append(s.silent, calls...)
}

// answer has the store answer every call again.
func (s *silentStore) answer() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.silent = nil
}

func (s *silentStore) isSilent(call string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Contains(s.silent, call)
}

func (s *silentStore) Get(ctx context.Context, turnID string) (PendingRun, bool, error) {
	if s.isSilent("Get") {
		return PendingRun{}, false, fmt.Errorf("sandbox: read run %s: %w", turnID, errStoreUnanswered)
	}
	return s.PendingStore.Get(ctx, turnID)
}

func (s *silentStore) RetireSessions(ctx context.Context, turnID string, sessions []string) error {
	if s.isSilent("RetireSessions") {
		return fmt.Errorf("sandbox: recording the sessions run %s ran out of context in: %w", turnID,
			errStoreUnanswered)
	}
	return s.PendingStore.RetireSessions(ctx, turnID, sessions)
}

func (s *silentStore) WatchLog(ctx context.Context, turnID string) (<-chan LogView, error) {
	s.mu.Lock()
	s.watches++
	n := s.watches
	s.mu.Unlock()
	switch again := s.isSilent("WatchLog again"); {
	case again && n%2 == 1:
		closed := make(chan LogView)
		close(closed)
		return closed, nil
	case again, s.isSilent("WatchLog"):
		return nil, fmt.Errorf("sandbox: watch the log of run %s: %w", turnID, errStoreUnanswered)
	}
	return s.PendingStore.WatchLog(ctx, turnID)
}

// collectAgain hands t1's completion to the rig's coordinator, as the queue
// redelivers it or the poll announces it again.
func collectAgain(t *testing.T, rig *coordRig) error {
	t.Helper()
	return collectAgainIn(t.Context(), t, rig)
}

// collectAgainIn is collectAgain from inside a call the coordinator made, as a
// second delivery of the completion arrives while the first is still being
// collected: under a context derived from that call's (without its
// cancellation, since the queue's second delivery outlives the first's
// collection), never one made up.
func collectAgainIn(ctx context.Context, t *testing.T, rig *coordRig) error {
	t.Helper()
	payload, ev := rig.completionIn(ctx, "t1")
	return rig.coordinator.OnCompleted(ctx, payload, ev)
}

// heldThroughSilence collects t1 once per give-up window, for longer than the
// window gives a box that does not answer, with the store silent throughout,
// and holds every attempt to a claim handed back UNCOUNTED: the completion
// handed back for a retry, the run claimable, its run of failed collections as
// it stood before the silence, and nothing ended, announced or resumed.
func heldThroughSilence(t *testing.T, rig *coordRig) {
	t.Helper()
	before := rig.get("t1").Launch
	for attempt := 1; attempt <= MinConnectFailures+1; attempt++ {
		if err := collectAgain(t, rig); !errors.Is(err, ErrStoreSilent) {
			t.Fatalf("attempt %d = %v; want the store's silence handed back for a retry", attempt, err)
		}
		row := rig.get("t1")
		if got := row.Launch; row.Status != StatusRunning || got.CollectFailures != before.CollectFailures ||
			!got.CollectFailingSince.Equal(before.CollectFailingSince) {
			t.Fatalf("attempt %d left the run %s with %d failed collections since %v; want it claimable, its "+
				"%d since %v as they stood, the store's silence counted against nothing", attempt, row.Status,
				got.CollectFailures, got.CollectFailingSince, before.CollectFailures, before.CollectFailingSince)
		}
		if failed, resumed := rig.failures(), rig.resumer.calls(); len(failed) != 0 || len(resumed) != 0 {
			t.Fatalf("attempt %d ended or resumed the run: announced %+v, resumed %d times", attempt, failed,
				len(resumed))
		}
		rig.now = rig.now.Add(ConnectGiveUp)
	}
}

// A STORE THAT DOES NOT ANSWER A COLLECTION NEVER COUNTS AGAINST ITS BOX. A
// collection makes steps the coordination store must answer and that it cannot
// go on without: the read of the run's row that licenses a read of its box on
// its host, the drain's watch of the run's log (and the watch made again after
// one that stopped hearing), and the record of the sessions a job ran out of
// context in or whose request its provider refused. None of them is the box's,
// which answered every read. So a store
// silent for longer than the give-up window hands the claim back uncounted,
// the completion retried, and leaves the run claimable with its result in its
// box: the collection made once the store answers resumes the turn on it,
// through a remote host's reply as on this node. Counted, the window's last
// silence settled the run as unreachable, "its result is lost", while its box
// held the result throughout.
//
// AND A LOST JOB'S CARRY is handed back for it too, never carried on without
// its read: a box that failed one read before the store went silent has a run
// of failures the window counts, and given up there, the round went on
// uncharged for what its box still held.
//
// Mutation: count the silence in collectFailed; leave any one step unwrapped
// (the host's row read, either watch, the retirement); drop the reply's flag,
// or Err's wrap of it; or have collectGivenUp give up on it: each turns a case
// red.
func TestAStoreThatDoesNotAnswerACollectionNeverCountsAgainstItsBox(t *testing.T) {
	t.Parallel()
	account := Result{Success: true, Text: "the box's account", InputTokens: 900, OutputTokens: 100}
	ranOut := Result{Error: "Prompt is too long", ContextExhausted: true, SessionID: "s-1"}
	for _, tc := range []struct {
		name string
		// box is what t1's job left in its box, and arrange launches it
		// and silences the store, returning the store to answer again.
		box     Result
		arrange func(t *testing.T, rig *coordRig) *silentStore
	}{
		{"the host's read of the run's row", account, func(t *testing.T, rig *coordRig) *silentStore {
			store := &silentStore{PendingStore: rig.pending, silent: []string{"Get"}}
			rig.onNode(t, "node-b", hostAnswers{host: rig.hostOn(t, "node-a", store)}, nil)
			rig.launchedBy("t1", "node-a")
			return store
		}},
		{"the drain's watch of the run's log", account, func(_ *testing.T, rig *coordRig) *silentStore {
			rig.launch("t1")
			store := &silentStore{PendingStore: rig.coordinator.pending, silent: []string{"WatchLog"}}
			rig.coordinator.pending = store
			return store
		}},
		{"the drain's watch made again", account, func(_ *testing.T, rig *coordRig) *silentStore {
			rig.launch("t1")
			store := &silentStore{PendingStore: rig.coordinator.pending, silent: []string{"WatchLog again"}}
			rig.coordinator.pending = store
			return store
		}},
		{"the record of the session it ran out of context in or whose request its provider refused", ranOut,
			func(_ *testing.T, rig *coordRig) *silentStore {
				rig.launch("t1")
				store := &silentStore{PendingStore: rig.coordinator.pending, silent: []string{"RetireSessions"}}
				rig.coordinator.pending = store
				return store
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rig := newCoordRig(t)
			store := tc.arrange(t, rig)
			rig.runner.Finish(tc.box)

			heldThroughSilence(t, rig)

			store.answer()
			if err := collectAgain(t, rig); err != nil {
				t.Fatalf("the collection once the store answered: %v", err)
			}
			resumed := rig.resumer.calls()
			if len(resumed) != 1 || resumed[0].Ended.Text != tc.box.Text || resumed[0].Ended.Error != tc.box.Error {
				t.Fatalf("the turn resumed on %+v; want it resumed once on its box's account, intact", resumed)
			}
			if failed := rig.failures(); len(failed) != 0 {
				t.Fatalf("announced %+v for a run whose box answered every read", failed)
			}
		})
	}

	partial := Result{Text: "half way through", InputTokens: 900, OutputTokens: 100}
	for _, tc := range []struct {
		name    string
		arrange func(t *testing.T, rig *coordRig) *silentStore
	}{
		{"a lost job's read on its host", func(t *testing.T, rig *coordRig) *silentStore {
			store := &silentStore{PendingStore: rig.pending}
			rig.onNode(t, "node-b", hostAnswers{host: rig.hostOn(t, "node-a", store)}, nil)
			run := rig.launchedBy("t1", "node-a")
			if marked, err := rig.pending.MarkLost(t.Context(), "t1", run.LaunchID,
				JobLoss{Cause: LossEngineRestarted, Node: "node-a", At: rig.now}); err != nil || !marked {
				t.Fatalf("MarkLost = %v, %v", marked, err)
			}
			rig.runner.Lose(partial)
			// THE BOX FAILED A READ FIRST, which the window counts, and the
			// window has passed since when the store goes silent.
			rig.runner.CollectErr = errBoxBlip
			if err := collectAgain(t, rig); err == nil || errors.Is(err, ErrStoreSilent) {
				t.Fatalf("the box's failed read = %v; want it handed back as the box's", err)
			}
			rig.runner.CollectErr = nil
			if got := rig.get("t1").Launch; got.CollectFailures != 1 {
				t.Fatalf("the premise: the box's failed read counted %d failures, want 1", got.CollectFailures)
			}
			rig.now = rig.now.Add(ConnectGiveUp)
			store.silence("Get")
			return store
		}},
		{"a lost job's drain", func(t *testing.T, rig *coordRig) *silentStore {
			store := &silentStore{PendingStore: rig.coordinator.pending, silent: []string{"WatchLog"}}
			rig.coordinator.pending = store
			rig.launch("t1")
			rig.runner.Lose(partial)
			if fired := rig.tick(); fired != 1 {
				t.Fatalf("announced %d completions for a lost job, want 1", fired)
			}
			return store
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rig := newCoordRig(t)
			store := tc.arrange(t, rig)

			heldThroughSilence(t, rig)
			if charged := rig.accountant.total(); charged != 0 {
				t.Fatalf("charged %d tokens while the store was silent", charged)
			}

			store.answer()
			if err := collectAgain(t, rig); err != nil {
				t.Fatalf("the carry once the store answered: %v", err)
			}
			resumed := rig.resumer.calls()
			if len(resumed) != 1 || resumed[0].Lost == nil || !strings.Contains(resumed[0].Answer, partial.Text) {
				t.Fatalf("the turn resumed on %+v; want the round carried on once, on what its box read", resumed)
			}
			if charged := rig.accountant.total(); charged != 1000 {
				t.Fatalf("charged %d tokens; want the 1000 the lost job's box read once the store answered", charged)
			}
		})
	}
}

// outlastedStore is a run store that answers the step named in at only once its
// asker's context has ended, as the store answers a request its caller's
// deadline cut, and every other call as the store beneath it does: "Get", the
// read of a run's row; "WatchLog", every watch of a log; "WatchLog again", a
// first watch that stops hearing at once, delivering nothing, and the watch
// made again after it, outlasted; and "RetireSessions".
type outlastedStore struct {
	PendingStore
	at string

	mu      sync.Mutex
	watches int
}

func (s *outlastedStore) Get(ctx context.Context, turnID string) (PendingRun, bool, error) {
	if s.at != "Get" {
		return s.PendingStore.Get(ctx, turnID)
	}
	<-ctx.Done()
	return PendingRun{}, false, fmt.Errorf("sandbox: read run %s: %w", turnID, ctx.Err())
}

func (s *outlastedStore) WatchLog(ctx context.Context, turnID string) (<-chan LogView, error) {
	s.mu.Lock()
	s.watches++
	first := s.watches == 1
	s.mu.Unlock()
	switch {
	case s.at == "WatchLog again" && first:
		closed := make(chan LogView)
		close(closed)
		return closed, nil
	case s.at != "WatchLog" && s.at != "WatchLog again":
		return s.PendingStore.WatchLog(ctx, turnID)
	}
	<-ctx.Done()
	return nil, fmt.Errorf("sandbox: watch the log of run %s: %w", turnID, ctx.Err())
}

func (s *outlastedStore) RetireSessions(ctx context.Context, turnID string, sessions []string) error {
	if s.at != "RetireSessions" {
		return s.PendingStore.RetireSessions(ctx, turnID, sessions)
	}
	<-ctx.Done()
	return fmt.Errorf("sandbox: recording the sessions run %s ran out of context in: %w", turnID, ctx.Err())
}

// A COLLECTION THAT RAN OUT OF ITS DELIVERY'S TIME IS COUNTED, WHICHEVER STEP
// THE TIME RAN OUT IN. A step of the store's that the delivery's own deadline
// cut is the deadline's, not the store's silence: taken for the store's, a
// collection whose read takes the delivery's time up to its last store step
// would be handed back uncounted and asked again for ever, as one whose read
// always outlasts the delivery would be if that were not counted. Every step
// the store's silence is judged at is held to it on its own, since each judges
// its own context: the host's read of the run's row (cut there by the
// delivery's deadline, which bounds the host's read, and answered as the read
// running out of time, never as the store's silence), the drain's watch of the
// run's log and the watch made again after one that stopped hearing, and the
// record of the sessions the job ran out of context in or whose request its
// provider refused.
//
// Mutation: wrap any one step's error in ErrStoreSilent whatever its context
// says, or have storeSilent wrap every step's so, and the attempt the deadline
// cut at that step is handed back uncounted.
func TestACollectionThatRanOutOfTimeIsCountedWhicheverStepItWasIn(t *testing.T) {
	t.Parallel()
	account := Result{Success: true, Text: "the box's account"}
	ranOut := Result{Error: "Prompt is too long", ContextExhausted: true, SessionID: "s-1"}
	for _, tc := range []struct {
		name string
		// box is what t1's job left in its box, and arrange launches it
		// with the step outlasted.
		box     Result
		arrange func(t *testing.T, rig *coordRig)
	}{
		{"the host's read of the run's row", account, func(t *testing.T, rig *coordRig) {
			host := rig.hostOn(t, "node-a", &outlastedStore{PendingStore: rig.pending, at: "Get"})
			rig.onNode(t, "node-b", hostAnswers{host: host}, nil)
			rig.launchedBy("t1", "node-a")
		}},
		{"the drain's watch of the run's log", account, func(_ *testing.T, rig *coordRig) {
			rig.launch("t1")
			rig.coordinator.pending = &outlastedStore{PendingStore: rig.coordinator.pending, at: "WatchLog"}
		}},
		{"the drain's watch made again", account, func(_ *testing.T, rig *coordRig) {
			rig.launch("t1")
			rig.coordinator.pending = &outlastedStore{PendingStore: rig.coordinator.pending, at: "WatchLog again"}
		}},
		{"the record of the session it ran out of context in or whose request its provider refused", ranOut,
			func(_ *testing.T, rig *coordRig) {
				rig.launch("t1")
				rig.coordinator.pending = &outlastedStore{PendingStore: rig.coordinator.pending, at: "RetireSessions"}
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rig := newCoordRig(t)
			tc.arrange(t, rig)
			rig.runner.Finish(tc.box)

			// A SECOND, so every step before the outlasted one has its
			// time: the box read and the drain over the rig's own store
			// take milliseconds, and a watch made again waits its pause of
			// a tenth of one first.
			short, stop := context.WithTimeout(t.Context(), time.Second)
			defer stop()
			payload, ev := rig.completion("t1")
			if err := rig.coordinator.OnCompleted(short, payload, ev); err == nil || errors.Is(err, ErrStoreSilent) {
				t.Fatalf("OnCompleted = %v; want the deadline's failure handed back, never the store's silence",
					err)
			}
			row := rig.get("t1")
			if row.Status != StatusRunning || row.Launch.CollectFailures != 1 {
				t.Errorf("the run is %s with %d failed collections after one its deadline cut at %s; want it "+
					"handed back and counted", row.Status, row.Launch.CollectFailures, tc.name)
			}
		})
	}
}
