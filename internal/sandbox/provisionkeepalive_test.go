package sandbox

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

// slowStep is each setup command's duration: four minutes, so a launch's
// fourth command ends past the box's 15-minute timer, and all ten far past it.
const slowStep = 4 * time.Minute

// provisionSteps is how many operator steps the case's launch is given.
const provisionSteps = 10

// A BOX IS KEPT ALIVE FOR AS LONG AS ITS LAUNCH PROVISIONS IT. A provider ends
// a box at its timer, which only a refresh moves, and the waiter's keepalive
// refreshes only a running row's box, never one still given setup steps. A box
// given ten four-minute steps is refreshed to the box timeout when
// provisioning begins (before the first command) and at most every
// provisionRefresh after, so its timer never runs out; and nothing refreshes
// it once the launch has returned.
//
// Mutations: start no keepalive in Acquire — reaped at its fourth command,
// red; refresh only on the ticker — the first refresh comes an interval after
// the first command, red; never stop it — refreshes go on after the launch, red.
func TestABoxIsKeptAliveWhileItsLaunchProvisionsIt(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		rig := newWaiterRig(t)
		boxes := newTimedBoxes(rig, slowStep)
		manager := boxes.manager(t, rig)
		req := launchReq("t1")
		req.Spec.TimeoutSec = DefaultBoxTimeout.Seconds()
		for i := range provisionSteps {
			req.Setup = append(req.Setup, SetupStep{Name: fmt.Sprintf("operator-%d", i),
				Commands: []string{"make deps"}})
		}

		began := time.Now()
		res, err := rig.launchVia(t.Context(), manager, req)
		if err != nil {
			t.Fatalf("a launch whose provisioning outlasts its box's timer: %v", err)
		}
		boxes.keptAlive(t, rig.provider.Box(res.SandboxID), began, provisionSteps)
	})
}

// timedBoxes is a provider whose boxes end at their timer, as a remote
// provider's do: create sets it to the spec's TTL, a reconnect (of a paused
// box, whose snapshot ran no timer) restarts it at E2B's
// [defaultSandboxTTLSeconds], each [Sandbox.SetTimeout] moves it, and once it
// runs out the box is gone for good, so a command on it fails as on a
// reclaimed box. Every command takes step.
//
// ONLY IN A BUBBLE ([newTimedBoxes]): timers, refreshes and commands are
// instants on the case's clock, which only a bubble keeps exact.
type timedBoxes struct {
	*FakeProvider
	mu   sync.Mutex
	step time.Duration
	// command is [timedBoxes.timedCommand], set by [newTimedBoxes]: Create,
	// an exported method no bubble declaration can hold to its callers,
	// reaches the timed command through this rather than by name.
	command func(box *FakeSandbox) (ExecResult, error)
	// set is the timers each box's create and reconnects set, by box.
	set map[string][]FakeTimeout
	// ran is the instant each command began, in order, across every box.
	ran []time.Time
}

// errReclaimed is a command run on a box its timer ended.
var errReclaimed = errors.New("the box is gone: its provider reclaimed it at its timer")

// newTimedBoxes is a provider of timed boxes over rig's, each command step
// long.
//
//fixedwait:bubble
func newTimedBoxes(rig *waiterRig, step time.Duration) *timedBoxes {
	p := &timedBoxes{FakeProvider: rig.provider, step: step}
	p.command = p.timedCommand
	return p
}

// manager is a manager over these boxes and the rig's runner.
func (p *timedBoxes) manager(t *testing.T, rig *waiterRig) *Manager {
	t.Helper()
	m, err := NewManager(ManagerOptions{
		Providers: map[Placement]Provider{Direct: p},
		Runners:   map[string]Runner{"claude-code": rig.runner},
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	return m
}

func (p *timedBoxes) Create(ctx context.Context, spec Spec) (Sandbox, error) {
	box, err := p.FakeProvider.Create(ctx, spec)
	if err != nil {
		return nil, err
	}
	fake := box.(*FakeSandbox)
	p.timer(fake.ID(), spec.TimeoutSec)
	fake.ExecFunc = func(_ context.Context, cmd string, _ ExecOptions) (ExecResult, error) {
		if cmd == boxFactsScript {
			// THE LAUNCH'S READ OF THE BOX, after its provisioning and held to
			// seconds by its own bound: not one of the steps this case times.
			return ExecResult{}, nil
		}
		return p.command(fake)
	}
	return box, nil
}

func (p *timedBoxes) Connect(ctx context.Context, sandboxID string) (Sandbox, error) {
	box, err := p.FakeProvider.Connect(ctx, sandboxID)
	if err != nil {
		return nil, err
	}
	p.timer(sandboxID, defaultSandboxTTLSeconds)
	return box, nil
}

// timer records the timer the provider set on box id now.
func (p *timedBoxes) timer(id string, seconds float64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.set == nil {
		p.set = map[string][]FakeTimeout{}
	}
	p.set[id] = append(p.set[id], FakeTimeout{At: time.Now(), Seconds: seconds})
}

// timedCommand is a command on box: refused on a box its timer ended, then
// step long, and failed if the box's timer ended while it ran.
//
//fixedwait:bubble
func (p *timedBoxes) timedCommand(box *FakeSandbox) (ExecResult, error) {
	p.mu.Lock()
	p.ran = append(p.ran, time.Now())
	step := p.step
	p.mu.Unlock()
	if !p.aliveAt(box, time.Now()) {
		return ExecResult{}, errReclaimed
	}
	time.Sleep(step)
	if !p.aliveAt(box, time.Now()) {
		return ExecResult{}, errReclaimed
	}
	return ExecResult{}, nil
}

// aliveAt reports whether box's timer, since its provider last set it (create
// or the reconnect that woke it), has not run out by now: every later refresh
// came before the timer it replaced ran out, and the last has not.
//
//fixedwait:bubble
func (p *timedBoxes) aliveAt(box *FakeSandbox, now time.Time) bool {
	p.mu.Lock()
	set := slices.Clone(p.set[box.ID()])
	p.mu.Unlock()
	var start FakeTimeout
	for _, timer := range set {
		if !timer.At.After(now) {
			start = timer
		}
	}
	if start.At.IsZero() {
		return false
	}
	ends := start.At.Add(time.Duration(start.Seconds * float64(time.Second)))
	for _, refresh := range box.Timeouts() {
		if refresh.At.Before(start.At) || refresh.At.After(now) {
			continue
		}
		if refresh.At.After(ends) {
			return false
		}
		ends = refresh.At.Add(time.Duration(refresh.Seconds * float64(time.Second)))
	}
	return !now.After(ends)
}

// keptAlive fails t unless box survived a provisioning that began at began and
// ran that many commands, together longer than its timer: refreshed to the box
// timeout at began, before the first command, then at most every
// provisionRefresh, and never once the launch had returned.
//
//fixedwait:bubble
func (p *timedBoxes) keptAlive(t *testing.T, box *FakeSandbox, began time.Time, commands int) {
	t.Helper()
	ended := time.Now()
	if took := ended.Sub(began); took < time.Duration(commands)*slowStep || took <= DefaultBoxTimeout {
		t.Fatalf("the provisioning took %v, want its %d commands' %v, past the box's %v timer", took, commands,
			time.Duration(commands)*slowStep, DefaultBoxTimeout)
	}
	if !p.aliveAt(box, ended) {
		t.Fatal("the box's timer ran out while the launch provisioned it")
	}
	var during []FakeTimeout
	for _, r := range box.Timeouts() {
		if !r.At.Before(began) {
			during = append(during, r)
		}
	}
	p.mu.Lock()
	first := p.ran[len(p.ran)-commands]
	p.mu.Unlock()
	switch {
	case len(during) == 0:
		t.Fatal("the box's timer was never refreshed while the launch provisioned it")
	case !during[0].At.Equal(began) || during[0].At.After(first):
		t.Errorf("the box's timer was first refreshed %v after its provisioning began, its first command at %v: "+
			"its steps began on whatever timer its create left", during[0].At.Sub(began), first.Sub(began))
	}
	at := began
	for _, r := range during {
		if r.Seconds != DefaultBoxTimeout.Seconds() {
			t.Errorf("a refresh set the box's timer to %vs, want the box timeout's %vs", r.Seconds,
				DefaultBoxTimeout.Seconds())
		}
		if gap := r.At.Sub(at); gap > provisionRefresh {
			t.Errorf("the box went %v without a refresh while it was provisioned, past provisionRefresh's %v",
				gap, provisionRefresh)
		}
		at = r.At
	}
	if gap := ended.Sub(at); gap > provisionRefresh {
		t.Errorf("the box's last refresh came %v before its provisioning ended", gap)
	}
	refreshed := box.Keepalives()
	time.Sleep(10 * provisionRefresh)
	if more := box.Keepalives() - refreshed; more != 0 {
		t.Errorf("the box's timer was refreshed %d more time(s) after its launch returned: the keepalive "+
			"outlived the provisioning it is for", more)
	}
}
