package setup

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/integration"
)

// gatePass blocks until released, so a second Start can be attempted while
// the first is genuinely mid-flight.
type gatePass struct {
	kind     integration.Kind
	entered  chan struct{}
	release  chan struct{}
	findings []integration.Finding
	err      error

	mu   sync.Mutex
	runs int
	once sync.Once
}

func (p *gatePass) Kind() integration.Kind { return p.kind }
func (p *gatePass) Needs() *Requirement    { return nil }

func (p *gatePass) Run(context.Context, PassInput) ([]integration.Finding, error) {
	p.mu.Lock()
	p.runs++
	p.mu.Unlock()
	if p.entered != nil {
		// Once: the release case below runs this pass twice.
		p.once.Do(func() { close(p.entered) })
	}
	if p.release != nil {
		<-p.release
	}
	return p.findings, p.err
}

func (p *gatePass) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.runs
}

func newGate(kind integration.Kind) *gatePass {
	return &gatePass{kind: kind, entered: make(chan struct{}), release: make(chan struct{})}
}

var pinnedNow = func() time.Time { return time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC) }

// runPass mirrors exactly what every caller of this package does: take the
// surface's guard, run the pass on the context it hands back, and give the
// guard back only once the outcome has been recorded.
//
// A HELPER RATHER THAN A METHOD, deliberately. It used to be [Runner.Start],
// which took the guard and released it the instant the pass returned — and
// that put the caller's status write outside the lease, which is the lost
// update the whole guard exists to prevent. Making the caller hold is what
// fixed it, so a test that wants a pass has to do what a caller does.
func runPass(
	t *testing.T, r *Runner, kind integration.Kind, in PassInput, id string,
) (*Run, error) {
	t.Helper()
	ctx, release, held, err := r.Hold(context.Background(), kind)
	switch {
	case err != nil:
		return nil, err
	case !held:
		return nil, ErrPassInFlight
	}
	defer release()
	return r.Execute(ctx, kind, in, id)
}

// TWO OPERATORS MUST NOT MINT AT ONE VENDOR AT ONCE, and the in-process half
// of that guard is this one: the fleet lease stops two NODES, and nothing
// about it stops two goroutines on the same node racing each other.
func TestASecondPassForOneVendorIsRefusedWhileTheFirstRuns(t *testing.T) {
	pass := newGate(integration.KindGitHub)
	r := NewRunner([]Pass{pass}, nil, pinnedNow)

	first := make(chan error, 1)
	go func() {
		_, err := runPass(t, r, integration.KindGitHub, PassInput{}, "run-1")
		first <- err
	}()
	<-pass.entered

	if _, err := runPass(t, r, integration.KindGitHub, PassInput{}, "run-2"); !errors.Is(err, ErrPassInFlight) {
		t.Fatalf("second Start returned %v, want ErrPassInFlight", err)
	}
	if got := pass.count(); got != 1 {
		t.Errorf("the pass ran %d times; the second call must not reach the third-party app", got)
	}

	close(pass.release)
	if err := <-first; err != nil {
		t.Fatalf("the first pass failed: %v", err)
	}

	// AND THE GUARD IS RELEASED. A claim that outlived its pass would lock
	// the third-party app out of every later attempt until the process
	// restarted.
	done := newGate(integration.KindGitHub)
	close(done.release)
	r2 := NewRunner([]Pass{done}, nil, pinnedNow)
	if _, err := runPass(t, r2, integration.KindGitHub, PassInput{}, "a"); err != nil {
		t.Fatalf("first: %v", err)
	}
	if _, err := runPass(t, r2, integration.KindGitHub, PassInput{}, "b"); err != nil {
		t.Fatalf("a second pass after the first finished was refused: %v", err)
	}
}

// UNKNOWN IS NOT "SOMEBODY ELSE HOLDS IT". A coordination store that could
// not answer is not evidence of a competing minter, and collapsing the two
// would make a two-second store blip look like a conflict — the single most
// incident-hardened rule in this engine (see internal/coord).
func TestALeaseThatCannotBeReadIsNotReportedAsInFlight(t *testing.T) {
	pass := newGate(integration.KindGitHub)
	close(pass.release)
	blip := errors.New("coordination store unreachable")
	r := NewRunner([]Pass{pass}, func(integration.Kind) Duty {
		return func(context.Context) (func(), bool, error) { return nil, false, blip }
	}, pinnedNow)

	_, err := runPass(t, r, integration.KindGitHub, PassInput{}, "run-1")
	if errors.Is(err, ErrPassInFlight) {
		t.Fatal("an unreadable lease was reported as a pass already running")
	}
	if !errors.Is(err, blip) {
		t.Fatalf("err = %v, want it to wrap the store's own failure", err)
	}
	if got := pass.count(); got != 0 {
		t.Errorf("the pass ran %d times without holding its lease", got)
	}
}

// A lease another node holds IS in-flight, which is the answer the first test
// exercises in-process and this one over the fleet.
func TestALeaseHeldElsewhereIsInFlight(t *testing.T) {
	pass := newGate(integration.KindGitHub)
	close(pass.release)
	r := NewRunner([]Pass{pass}, func(integration.Kind) Duty {
		return func(context.Context) (func(), bool, error) { return nil, false, nil }
	}, pinnedNow)

	if _, err := runPass(t, r, integration.KindGitHub, PassInput{}, "run-1"); !errors.Is(err, ErrPassInFlight) {
		t.Fatalf("err = %v, want ErrPassInFlight", err)
	}
	if got := pass.count(); got != 0 {
		t.Errorf("the pass ran %d times while another node held the lease", got)
	}
}

// A third-party app this build cannot provision is refused by name rather
// than reported as a pass that did nothing.
func TestAVendorWithNoPassIsRefused(t *testing.T) {
	r := NewRunner(nil, nil, pinnedNow)
	if _, err := runPass(t, r, integration.KindSlack, PassInput{}, "run-1"); !errors.Is(err, ErrNoPass) {
		t.Fatalf("err = %v, want ErrNoPass", err)
	}
	if r.Serves(integration.KindSlack) {
		t.Error("Serves says a third-party app with no pass is servable")
	}
}

// A FAILED PASS IS REMEMBERED AS FAILED, with the fault on the run, because
// the screen polls this row after the request that started it has returned.
func TestAFailedPassIsRecordedRatherThanForgotten(t *testing.T) {
	boom := errors.New("third-party app refused")
	pass := &gatePass{kind: integration.KindGitHub, err: boom}
	r := NewRunner([]Pass{pass}, nil, pinnedNow)

	run, err := runPass(t, r, integration.KindGitHub, PassInput{}, "run-1")
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the third-party app's own failure", err)
	}
	if run == nil {
		t.Fatal("a failed pass returned no run, so nothing can be polled for it")
	}
	if run.State != RunFailed {
		t.Errorf("state = %q, want %q", run.State, RunFailed)
	}
	if run.EndedAt == nil {
		t.Error("a finished run carries no end time")
	}
	got, ok := r.Get("run-1")
	if !ok || got.State != RunFailed {
		t.Error("the failed run is not retrievable by id")
	}
}

// countingDuty is a lease that records how often it was taken and given back.
type countingDuty struct {
	mu       sync.Mutex
	held     int
	released int
}

func (d *countingDuty) duty(integration.Kind) Duty {
	return func(context.Context) (func(), bool, error) {
		d.mu.Lock()
		d.held++
		d.mu.Unlock()
		return func() {
			d.mu.Lock()
			d.released++
			d.mu.Unlock()
		}, true, nil
	}
}

func (d *countingDuty) counts() (int, int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.held, d.released
}

// THE LEASE IS GIVEN BACK WHEN THE PASS ENDS.
//
// Held to its TTL instead, this lock is indistinguishable from an outage to
// every other caller. The reconcile loop takes the SAME lease for the same
// surface every few seconds, so a five-minute lease kept after a
// three-second pass would refuse every tick on every other node — and refuse
// an operator's next press of the button too.
func TestTheProvisioningLeaseIsGivenBack(t *testing.T) {
	pass := newGate(integration.KindGitHub)
	close(pass.release)
	lease := new(countingDuty)
	r := NewRunner([]Pass{pass}, lease.duty, pinnedNow)

	if _, err := runPass(t, r, integration.KindGitHub, PassInput{}, "run-1"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if held, released := lease.counts(); held != 1 || released != 1 {
		t.Errorf("lease taken %d times and given back %d, want 1 and 1", held, released)
	}
}

// AND GIVEN BACK WHEN THE PASS FAILS, which is when holding it hurts most: a
// surface that just refused is the one an operator retries immediately.
func TestAFailedPassStillGivesTheLeaseBack(t *testing.T) {
	pass := newGate(integration.KindGitHub)
	close(pass.release)
	pass.err = errors.New("the vendor refused")
	lease := new(countingDuty)
	r := NewRunner([]Pass{pass}, lease.duty, pinnedNow)

	if _, err := runPass(t, r, integration.KindGitHub, PassInput{}, "run-1"); err == nil {
		t.Fatal("a failing pass reported success")
	}
	if _, released := lease.counts(); released != 1 {
		t.Errorf("the lease was given back %d times after a failure, want 1", released)
	}
}

// AND A HOLD GIVES IT BACK TOO. The loop's tick and a disconnect's teardown
// reach the surface through Hold rather than Start, and a lease kept after
// they finish locks a peer out for the full TTL exactly as one kept after a
// pass would.
func TestAHoldGivesTheLeaseBack(t *testing.T) {
	lease := new(countingDuty)
	r := NewRunner([]Pass{newGate(integration.KindGitHub)}, lease.duty, pinnedNow)

	_, release, held, err := r.Hold(context.Background(), integration.KindGitHub)
	if err != nil || !held {
		t.Fatalf("Hold = (%v, %v), want held", held, err)
	}
	release()
	if got, released := lease.counts(); got != 1 || released != 1 {
		t.Errorf("lease taken %d times and given back %d, want 1 and 1", got, released)
	}
}

// THE FLEET LEASE ALONE DOES NOT STOP TWO WRITERS IN ONE PROCESS.
//
// coord's TryAcquire doubles as a renew for the owner that already holds the
// record, so a loop tick and an operator's button on ONE node are both told
// yes by it — which is what countingDuty reproduces. The in-process claim is
// what refuses the second, and without it the two run the same pass at one
// third-party app: both see a seat with no account, both create one.
func TestAHoldIsRefusedWhileAPassRunsOnThisNode(t *testing.T) {
	pass := newGate(integration.KindGitHub)
	r := NewRunner([]Pass{pass}, new(countingDuty).duty, pinnedNow)

	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		close(started)
		_, err := runPass(t, r, integration.KindGitHub, PassInput{}, "run-1")
		done <- err
	}()
	<-started
	<-pass.entered

	_, release, held, err := r.Hold(context.Background(), integration.KindGitHub)
	switch {
	case err != nil:
		t.Fatalf("Hold reported a fault rather than a busy surface: %v", err)
	case held:
		release()
		t.Fatal("a hold was granted while a pass was writing at the same surface")
	}

	close(pass.release)
	if err := <-done; err != nil {
		t.Fatalf("Start: %v", err)
	}
	// AND THE SURFACE IS FREE AGAIN once the pass ends, or the refusal
	// above would be a deadlock rather than a guard.
	_, release, held, err = r.Hold(context.Background(), integration.KindGitHub)
	if err != nil || !held {
		t.Fatalf("Hold after the pass ended = (%v, %v), want held", held, err)
	}
	release()
}

// A LOST LOCAL RACE TAKES NO FLEET LEASE. Claiming the remote one first and
// discovering the local claim second would leave a peer locked out for the
// full TTL by a caller that never wrote anything.
func TestARefusedHoldTakesNoLease(t *testing.T) {
	pass := newGate(integration.KindGitHub)
	lease := new(countingDuty)
	r := NewRunner([]Pass{pass}, lease.duty, pinnedNow)

	done := make(chan error, 1)
	go func() {
		_, err := runPass(t, r, integration.KindGitHub, PassInput{}, "run-1")
		done <- err
	}()
	<-pass.entered

	// The live pass holds one, so the assertion is that the refused hold
	// took NONE — not that nothing is held at all.
	beforeHeld, beforeReleased := lease.counts()
	if _, _, held, _ := r.Hold(context.Background(), integration.KindGitHub); held {
		t.Fatal("a hold was granted while a pass was writing at the same surface")
	}
	afterHeld, afterReleased := lease.counts()
	if afterHeld != beforeHeld || afterReleased != beforeReleased {
		t.Errorf("a refused hold moved the lease counts from (%d held, %d given back) "+
			"to (%d, %d); it reached the store at all",
			beforeHeld, beforeReleased, afterHeld, afterReleased)
	}

	close(pass.release)
	if err := <-done; err != nil {
		t.Fatalf("Start: %v", err)
	}
}

// THE PASS MUST FINISH INSIDE THE LEASE THAT PROTECTS IT.
//
// The lease is taken once and never renewed, so a pass allowed to outlive it
// would go on creating accounts at a third-party app with nothing left
// excluding a peer — two nodes creating an identity for one seat, which no
// later pass can detect or repair. That is the entire argument for a deadline,
// and it holds only while these two numbers stand in this relationship.
//
// Pinned rather than remembered: they were in two packages, one of which did
// not apply a deadline at all, and nothing anywhere compared them.
func TestThePassDeadlineFitsInsideTheLease(t *testing.T) {
	t.Parallel()
	if PassDeadline >= LeaseTTL {
		t.Fatalf("PassDeadline is %s against a LeaseTTL of %s, so a pass that "+
			"runs to its deadline is writing at a third-party app with no lease "+
			"left to exclude a peer", PassDeadline, LeaseTTL)
	}
	if RecordDeadline <= 0 {
		t.Fatalf("RecordDeadline is %s, so the write that records a pass has no "+
			"room inside the lease the pass ran under", RecordDeadline)
	}
}

// AND THE HOLD IS WHAT APPLIES IT, so no caller can forget.
//
// The deadline used to be the caller's to remember, and one of the two callers
// did not: every reconcile-loop pass ran unbounded against a lease it could
// outlive, while the dashboard's identical pass had been bounded for exactly
// this reason since the day it was written.
func TestAHoldBoundsTheWorkItAdmits(t *testing.T) {
	t.Parallel()
	r := NewRunner([]Pass{&gatePass{kind: integration.KindGitHub}}, nil, nil)

	ctx, release, held, err := r.Hold(context.Background(), integration.KindGitHub)
	if err != nil || !held {
		t.Fatalf("Hold: held=%v err=%v", held, err)
	}
	defer release()

	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatal("the context a hold returns carries no deadline, so a pass can " +
			"outlive the lease protecting it")
	}
	if left := time.Until(deadline); left > PassDeadline {
		t.Fatalf("the deadline is %s away, which is more than PassDeadline (%s)",
			left, PassDeadline)
	}
}

// A HOLD DERIVES FROM THE CALLER'S CONTEXT rather than replacing it.
//
// Detaching here instead would look like a tidier place for it and break two
// things at once: the shared certification suite drives a pass with an
// already-cancelled context and requires a fault rather than a clean report,
// and the reconcile loop's Stop would no longer reach a pass in flight.
func TestAHoldStillCarriesTheCallersCancellation(t *testing.T) {
	t.Parallel()
	r := NewRunner([]Pass{&gatePass{kind: integration.KindGitHub}}, nil, nil)

	parent, cancel := context.WithCancel(context.Background())
	ctx, release, held, err := r.Hold(parent, integration.KindGitHub)
	if err != nil || !held {
		t.Fatalf("Hold: held=%v err=%v", held, err)
	}
	defer release()

	cancel()
	if ctx.Err() == nil {
		t.Fatal("cancelling the caller's context did not reach the pass, so a " +
			"stopping worker cannot stop the work it started")
	}
}

// A NODE WITH NO RUNNER IS STILL BOUNDED. It has no keyring, so it mints
// nothing and holds no lease — but it still reads a third-party app, and a read
// that never returns wedges the loop just as thoroughly as one that outlived a
// lease would.
func TestANilRunnerStillBoundsThePass(t *testing.T) {
	t.Parallel()
	var r *Runner

	ctx, release, held, err := r.Hold(context.Background(), integration.KindGitHub)
	if err != nil || !held {
		t.Fatalf("Hold on a nil runner: held=%v err=%v", held, err)
	}
	defer release()

	if _, ok := ctx.Deadline(); !ok {
		t.Fatal("a node with no runner runs its passes unbounded")
	}
}

// A PASS WHOSE DEADLINE IS ALREADY SPENT IS A FAULT, NOT A CLEAN REPORT.
//
// The dashboard's pass runs detached from the HTTP request, so what expires
// its context is [PassDeadline] rather than a closing tab — and a pass started
// on a spent one is running against a lease about to lapse. Worse, several
// third-party apps conclude from the company document before their first
// network call, so they would answer with no findings and no error, which the
// fold records as a READY integration nobody actually looked at.
//
// The reconcile loop's half of this rule lives in engine.passConverger; this
// is the dashboard's, and until it existed the loop had a guard the dashboard
// did not.
func TestAPassOnASpentContextIsAFault(t *testing.T) {
	t.Parallel()
	pass := &gatePass{kind: integration.KindGitHub}
	r := NewRunner([]Pass{pass}, nil, nil)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	run, err := r.Execute(ctx, integration.KindGitHub, PassInput{}, "run-1")
	if err == nil {
		t.Fatalf("a pass on a dead context reported %+v with no error, which "+
			"the fold records as ready", run)
	}
	if pass.count() != 0 {
		t.Fatalf("the pass ran %d times on a dead context", pass.count())
	}
}
