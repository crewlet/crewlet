package sandbox

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
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
	if row.Status != StatusRunning || row.LaunchFacts().CollectFailures != 1 {
		t.Fatalf("row %s with %d failures; want running again with the failure counted",
			row.Status, row.LaunchFacts().CollectFailures)
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
	if got := rig.get("t1").LaunchFacts(); got.CollectFailures != 2 || !got.CollectFailingSince.Equal(start.UTC()) {
		t.Fatalf("record %+v; want two failures dated from the first", got)
	}
	// Past it: given up, announced as unreachable, the seat freed.
	rig.now = start.Add(ConnectGiveUp)
	if err := collect(); err != nil {
		t.Fatalf("a failure past the window = %v; want the run settled", err)
	}
	failed := rig.failures()
	if len(failed) != 1 || failed[0].Reason != types.SandboxFailureCollect {
		t.Fatalf("failures = %+v; want one %q", failed, types.SandboxFailureCollect)
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
	if got := rig.get("t1").LaunchFacts(); got.CollectFailures != 0 || !got.CollectFailingSince.IsZero() {
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
	if got := rig.get("t1").LaunchFacts(); got.CollectFailures != 1 ||
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
	if got := row.LaunchFacts(); row.Status != StatusRunning || got.CollectFailures != 0 || !got.CollectFailingSince.IsZero() {
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
	if got := rig.get("t1").LaunchFacts(); got.CollectFailures != 1 {
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
	if failed := rig.failures(); len(failed) != 1 || failed[0].Reason != types.SandboxFailureCollect {
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
