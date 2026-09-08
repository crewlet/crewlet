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
		return func(context.Context) (bool, error) { return false, blip }
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
		return func(context.Context) (bool, error) { return false, nil }
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
