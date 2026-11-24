package sandbox

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

// slowStep is each setup command's duration: four minutes, so a launch's
// fourth command ends past the 15-minute host grace, and all ten far past it.
const slowStep = 4 * time.Minute

// provisionSteps is how many operator steps the case's launch is given.
const provisionSteps = 10

// A BOX IS KEPT ALIVE FOR AS LONG AS ITS LAUNCH PROVISIONS IT. A box with no
// job yet that records nothing is taken by another launch's orphan sweep once
// its stamp is older than the host grace, and the waiter's keepalive touches
// only a running row's box, never one still given setup steps. A box given ten
// four-minute steps is touched when provisioning begins (before the first
// command) and at most every provisionRefresh after, so its stamp never ages
// out; and nothing touches it once the launch has returned.
//
// Mutations: start no keepalive in Acquire — swept at its fourth command,
// red; touch only on the ticker — the first touch comes an interval after the
// first command, red; never stop it — touches go on after the launch, red.
func TestABoxIsKeptAliveWhileItsLaunchProvisionsIt(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		rig := newWaiterRig(t)
		boxes := newSweptBoxes(rig, slowStep)
		manager := boxes.manager(t, rig)
		req := launchReq("t1")
		for i := range provisionSteps {
			req.Setup = append(req.Setup, SetupStep{Name: fmt.Sprintf("operator-%d", i),
				Commands: []string{"make deps"}})
		}

		began := time.Now()
		res, err := rig.launchVia(t.Context(), manager, req)
		if err != nil {
			t.Fatalf("a launch whose provisioning outlasts the host grace: %v", err)
		}
		boxes.keptTouched(t, rig.provider.Box(res.SandboxID), began, provisionSteps)
	})
}

// sweptBoxes is a provider whose boxes record nothing, so another launch's
// orphan sweep takes them as the local backend's would ([Local.reapOrphans]): a
// box with no job whose stamp — its create, else its last [Sandbox.Touch] — has
// gone [DefaultHostGrace] untouched is gone for good, so a command on it fails
// as on a reaped box. Every command takes step.
//
// ONLY IN A BUBBLE ([newSweptBoxes]): stamps, touches and commands are
// instants on the case's clock, which only a bubble keeps exact.
type sweptBoxes struct {
	*FakeProvider
	mu   sync.Mutex
	step time.Duration
	// command is [sweptBoxes.sweptCommand], set by [newSweptBoxes]: Create,
	// an exported method no bubble declaration can hold to its callers,
	// reaches the swept command through this rather than by name.
	command func(box *FakeSandbox) (ExecResult, error)
	// made is the instant each box was created, by box: its first stamp.
	made map[string]time.Time
	// ran is the instant each command began, in order, across every box.
	ran []time.Time
}

// errSwept is a command run on a box an orphan sweep took.
var errSwept = errors.New("the box is gone: an orphan sweep took it, its stamp older than the host grace")

// newSweptBoxes is a provider of swept boxes over rig's, each command step
// long.
//
//fixedwait:bubble
func newSweptBoxes(rig *waiterRig, step time.Duration) *sweptBoxes {
	p := &sweptBoxes{FakeProvider: rig.provider, step: step, made: map[string]time.Time{}}
	p.command = p.sweptCommand
	return p
}

// manager is a manager over these boxes and the rig's runner.
func (p *sweptBoxes) manager(t *testing.T, rig *waiterRig) *Manager {
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

func (p *sweptBoxes) Create(ctx context.Context, spec Spec) (Sandbox, error) {
	box, err := p.FakeProvider.Create(ctx, spec)
	if err != nil {
		return nil, err
	}
	fake := box.(*FakeSandbox)
	p.mu.Lock()
	p.made[fake.ID()] = time.Now()
	p.mu.Unlock()
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

// sweptCommand is a command on box: refused on a box a sweep took, then step
// long, and failed if a sweep took the box while it ran.
//
//fixedwait:bubble
func (p *sweptBoxes) sweptCommand(box *FakeSandbox) (ExecResult, error) {
	p.mu.Lock()
	p.ran = append(p.ran, time.Now())
	step := p.step
	p.mu.Unlock()
	if !p.unsweptAt(box, time.Now()) {
		return ExecResult{}, errSwept
	}
	time.Sleep(step)
	if !p.unsweptAt(box, time.Now()) {
		return ExecResult{}, errSwept
	}
	return ExecResult{}, nil
}

// unsweptAt reports whether box's stamp, from its create through every touch
// up to now, never went longer than the host grace untouched: a sweep in any
// such gap would have taken it.
//
//fixedwait:bubble
func (p *sweptBoxes) unsweptAt(box *FakeSandbox, now time.Time) bool {
	p.mu.Lock()
	stamp, made := p.made[box.ID()]
	p.mu.Unlock()
	if !made {
		return false
	}
	for _, touch := range box.Touches() {
		if touch.After(now) {
			break
		}
		if touch.Sub(stamp) > DefaultHostGrace {
			return false
		}
		stamp = touch
	}
	return now.Sub(stamp) <= DefaultHostGrace
}

// keptTouched fails t unless box survived a provisioning that began at began
// and ran that many commands, together longer than the host grace: touched at
// began, before the first command, then at most every provisionRefresh, and
// never once the launch had returned.
//
//fixedwait:bubble
func (p *sweptBoxes) keptTouched(t *testing.T, box *FakeSandbox, began time.Time, commands int) {
	t.Helper()
	ended := time.Now()
	if took := ended.Sub(began); took < time.Duration(commands)*slowStep || took <= DefaultHostGrace {
		t.Fatalf("the provisioning took %v, want its %d commands' %v, past the %v host grace", took, commands,
			time.Duration(commands)*slowStep, DefaultHostGrace)
	}
	if !p.unsweptAt(box, ended) {
		t.Fatal("the box's stamp aged past the host grace while the launch provisioned it")
	}
	var during []time.Time
	for _, touch := range box.Touches() {
		if !touch.Before(began) {
			during = append(during, touch)
		}
	}
	p.mu.Lock()
	first := p.ran[len(p.ran)-commands]
	p.mu.Unlock()
	switch {
	case len(during) == 0:
		t.Fatal("the box was never touched while the launch provisioned it")
	case !during[0].Equal(began) || during[0].After(first):
		t.Errorf("the box was first touched %v after its provisioning began, its first command at %v: "+
			"its steps began on whatever stamp its create left", during[0].Sub(began), first.Sub(began))
	}
	at := began
	for _, touch := range during {
		if gap := touch.Sub(at); gap > provisionRefresh {
			t.Errorf("the box went %v untouched while it was provisioned, past provisionRefresh's %v",
				gap, provisionRefresh)
		}
		at = touch
	}
	if gap := ended.Sub(at); gap > provisionRefresh {
		t.Errorf("the box's last touch came %v before its provisioning ended", gap)
	}
	touched := box.Keepalives()
	time.Sleep(10 * provisionRefresh)
	if more := box.Keepalives() - touched; more != 0 {
		t.Errorf("the box was touched %d more time(s) after its launch returned: the keepalive "+
			"outlived the provisioning it is for", more)
	}
}
