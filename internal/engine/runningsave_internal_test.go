package engine

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	queuememory "github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// THE NODE'S WAITER OFFERS THE BOXES IT FINDS RUNNING FOR A SAVE: every box is
// polled by its launching node's (ADR-0065), so a waiter built without the
// hook leaves every box unsaved until its job ends.
//
// Mutation: leave Save off the options, or hand it anything but the
// coordinator's SaveRunning, and this goes red.
func TestTheWaiterTheEngineBuildsOffersItsBoxesForASave(t *testing.T) {
	t.Parallel()
	e := &Engine{backends: &Backends{Queue: queuememory.New()}, id: "node-a", answers: testAnswers()}
	manager, err := sandbox.NewManager(sandbox.ManagerOptions{
		Providers:        map[sandbox.Placement]sandbox.Provider{sandbox.Direct: sandbox.NewFakeProvider()},
		DefaultPlacement: sandbox.Direct,
		Runners:          map[string]sandbox.Runner{"claude-code": sandbox.NewFakeRunner("claude-code")},
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	rt, err := e.sandboxRuntimeOn(sandbox.NewCoordStore(coordmem.NewFleet()), manager)
	if err != nil {
		t.Fatalf("sandboxRuntimeOn: %v", err)
	}
	t.Cleanup(rt.coordinator.Stop)
	want := reflect.ValueOf(rt.coordinator.SaveRunning).Pointer()
	opts := e.sandboxWaiterOptions(rt, time.Minute)
	if opts.Save == nil || reflect.ValueOf(opts.Save).Pointer() != want {
		t.Errorf("the waiter's Save is set %v, and is not the coordinator's SaveRunning: the boxes "+
			"it polls are offered for no save", opts.Save != nil)
	}
}

// A SAVE'S INTERVAL IS READ OFF THE REVISION THIS NODE RUNS, the default where
// it runs none: a cadence binds only the node driving the box, so the fleet's
// revision, which a capture's budget is read off, is never asked.
//
// Mutation: answer a constant, or read the interval off the revision the
// fleet's activation pointer names (this fleet names none), and the company's
// fifteen minutes below go red.
func TestASavesIntervalIsReadOffThisNodesRevision(t *testing.T) {
	t.Parallel()
	e := &Engine{backends: &Backends{Fleet: coordmem.NewFleet()}}
	keep := executorKeep{engine: e}
	if got := keep.SaveInterval(); got != config.DefaultSaveInterval {
		t.Errorf("a node running no company saves every %s, want the default %s", got, config.DefaultSaveInterval)
	}
	e.epoch.current.Store(&Company{})
	if got := keep.SaveInterval(); got != config.DefaultSaveInterval {
		t.Errorf("a node whose company states no config saves every %s, want the default %s",
			got, config.DefaultSaveInterval)
	}
	company := &config.Company{}
	company.TurnEngine.ConversationSession.SaveIntervalSeconds = 900
	e.epoch.current.Store(&Company{Config: company})
	if got := keep.SaveInterval(); got != 15*time.Minute {
		t.Errorf("a node whose company saves every 900 s saves every %s, want 15m0s", got)
	}
}

// THE DRAIN SAVES BESIDE ITS WAIT FOR TURNS, AND RETURNS ONLY ONCE THE SAVE HAS
// ENDED. The save of the direct job this node launched begins while a turn
// still holds the drain, and the drain waits for it once the turn is done: the
// order is read off the line the drain's wait logs, and off the attach the
// case holds, never a clock.
//
// NOT IN PARALLEL: it reads the process's log ([captureEngineLogs]).
//
// Mutation: start the drain's save after the node's drain, or never join it, or
// leave the fleet's save ledger off the coordinator (no save is taken), and a
// check below goes red.
func TestTheDrainSavesBesideItsWaitForTurns(t *testing.T) {
	logs := captureEngineLogs(t)
	r := newDrainRig(t)
	// THE SAVE'S ATTACH OF THE JOB'S BOX, held once armed until the case lets
	// it go, then refused, which ends the save there.
	var armed, ended atomic.Bool
	entered, release := make(chan struct{}), make(chan struct{})
	var enter, free sync.Once
	r.provider.AttachFunc = func(ctx context.Context, _ string) error {
		if !armed.Load() {
			return nil
		}
		enter.Do(func() { close(entered) })
		select {
		case <-release:
		case <-ctx.Done():
		}
		ended.Store(true)
		return errors.New("the case ends the drain's save at its attach")
	}
	e := r.start(t)
	// FREED FIRST of everything the case built, so no stop waits on it.
	t.Cleanup(func() { free.Do(func() { close(release) }) })
	r.seedRun(t, e, "t1")
	// NO POLL ATTACHES THE BOX OR OFFERS IT A SAVE from here: only the drain's
	// save reaches the held attach, and the runner saves only once no poll
	// reads it.
	stopWaiter(e)
	r.runner.SavesSessions = true
	armed.Store(true)

	_, finish, err := e.node.AdmitTurn(t.Context(), "swe", "")
	if err != nil {
		t.Fatalf("AdmitTurn: %v", err)
	}
	t.Cleanup(finish)
	drained := make(chan struct{})
	var endedFirst atomic.Bool
	go func() {
		defer close(drained)
		e.Drain(context.Background())
		endedFirst.Store(ended.Load())
	}()

	// THE SAVE BEGINS WHILE THE TURN HOLDS THE DRAIN.
	select {
	case <-entered:
	case <-time.After(20 * time.Second): //fixedwait:liveness
		t.Fatal("the drain began no save of its node's direct job while a turn held it")
	}
	finish()
	// AND THE DRAIN WAITS FOR IT, where it would have returned.
	eventually(t, "the drain to wait for its save, or to return", func() bool {
		return len(logs.about("executor_session_saves_draining", "component", "engine")) > 0 ||
			closedNow(drained)
	})
	if closedNow(drained) {
		t.Fatal("the drain returned while its save of the node's direct job was under way")
	}
	free.Do(func() { close(release) })
	select {
	case <-drained:
	case <-time.After(30 * time.Second): //fixedwait:liveness
		t.Fatal("the drain did not return once its save ended")
	}
	if !endedFirst.Load() {
		t.Error("the drain returned before its save ended")
	}
}
