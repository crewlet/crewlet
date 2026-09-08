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

// TWO OPERATORS MUST NOT MINT AT ONE VENDOR AT ONCE, and the in-process half
// of that guard is this one: the fleet lease stops two NODES, and nothing
// about it stops two goroutines on the same node racing each other.
func TestASecondPassForOneVendorIsRefusedWhileTheFirstRuns(t *testing.T) {
	pass := newGate(integration.KindGitHub)
	r := NewRunner([]Pass{pass}, nil, pinnedNow)

	first := make(chan error, 1)
	go func() {
		_, err := r.Start(context.Background(), integration.KindGitHub, PassInput{}, "run-1")
		first <- err
	}()
	<-pass.entered

	if _, err := r.Start(context.Background(), integration.KindGitHub, PassInput{}, "run-2"); !errors.Is(err, ErrPassInFlight) {
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
	if _, err := r2.Start(context.Background(), integration.KindGitHub, PassInput{}, "a"); err != nil {
		t.Fatalf("first: %v", err)
	}
	if _, err := r2.Start(context.Background(), integration.KindGitHub, PassInput{}, "b"); err != nil {
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

	_, err := r.Start(context.Background(), integration.KindGitHub, PassInput{}, "run-1")
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

	if _, err := r.Start(context.Background(), integration.KindGitHub, PassInput{}, "run-1"); !errors.Is(err, ErrPassInFlight) {
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
	if _, err := r.Start(context.Background(), integration.KindSlack, PassInput{}, "run-1"); !errors.Is(err, ErrNoPass) {
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

	run, err := r.Start(context.Background(), integration.KindGitHub, PassInput{}, "run-1")
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

	if _, err := r.Start(context.Background(), integration.KindGitHub, PassInput{}, "run-1"); err != nil {
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

	if _, err := r.Start(context.Background(), integration.KindGitHub, PassInput{}, "run-1"); err == nil {
		t.Fatal("a failing pass reported success")
	}
	if _, released := lease.counts(); released != 1 {
		t.Errorf("the lease was given back %d times after a failure, want 1", released)
	}
}

// tearPass is a pass that can also be torn down.
type tearPass struct{ gatePass }

func (p *tearPass) Teardown(context.Context, TeardownInput) error { return nil }

// AND A TEARDOWN GIVES IT BACK TOO. It takes the same lease for the same
// reason — a teardown and a pass both write at the third-party app — so it
// has to return it on the same terms.
func TestATeardownGivesTheLeaseBack(t *testing.T) {
	pass := &tearPass{gatePass: gatePass{kind: integration.KindGitHub}}
	lease := new(countingDuty)
	r := NewRunner([]Pass{pass}, lease.duty, pinnedNow)

	if _, err := r.StartTeardown(
		context.Background(), integration.KindGitHub, TeardownInput{}, "run-1",
	); err != nil {
		t.Fatalf("StartTeardown: %v", err)
	}
	if held, released := lease.counts(); held != 1 || released != 1 {
		t.Errorf("lease taken %d times and given back %d, want 1 and 1", held, released)
	}
}
