package engine

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// THE RUNNER PROBE answers, for each executor cell, "can a box start here, on
// this node, now?" (M13 of the Models design): what the Models page's "where
// it runs" step disables a cell by, and what a person reads when a run did not
// start. Each node probes its own cells and publishes the answer on its status
// record ([coord.NodeStatusRecord.Runners]): readiness is a fact of the node
// (a container runtime on one host, a writable box root on another).
//
// It probes at boot, at every apply (a revision can declare or drop the
// catalogue, or change its image or runtime) and every [runnerProbeInterval],
// never on a read: a page polled by every open tab must not multiply daemon
// round trips. Who agrees on it: this node alone, in memory, re-probed at boot.

// runnerProbeInterval is how often a node re-probes its cells between applies:
// [sandbox.ConnectGiveUp], so a container daemon that stops between applies
// shows on the Models page no later than its boxes are given up. A probe is a
// temp-file write and one runtime version read bounded by
// [sandbox.CellProbeBudget], cheap enough to run every minute on every node.
const runnerProbeInterval = sandbox.ConnectGiveUp

// runnerProbe is this node's latest cell readiness and the loop that keeps it.
type runnerProbe struct {
	latest atomic.Pointer[coord.RunnerStatus]

	// kick asks the loop to probe now (an apply); buffered by one, so a
	// burst of applies is one probe.
	kick chan struct{}

	cancel context.CancelFunc
	done   chan struct{}

	// probe is the probe itself, [Engine.probeRunners] outside a test.
	probe func(ctx context.Context) *coord.RunnerStatus

	// once makes kick.
	once sync.Once
}

// startRunnerProbe probes once and keeps probing, until stopRunnerProbe.
func (e *Engine) startRunnerProbe(ctx context.Context) {
	p := &e.runners
	kick := p.kicks()
	probe := p.probe
	if probe == nil {
		probe = e.probeRunners
	}
	loopCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	p.cancel, p.done = cancel, make(chan struct{})
	go func() {
		defer close(p.done)
		ticker := time.NewTicker(runnerProbeInterval)
		defer ticker.Stop()
		for {
			p.latest.Store(probe(loopCtx))
			select {
			case <-loopCtx.Done():
				return
			case <-ticker.C:
			case <-kick:
			}
		}
	}()
}

// stopRunnerProbe ends the loop and waits for it.
func (e *Engine) stopRunnerProbe() {
	if e.runners.cancel == nil {
		return
	}
	e.runners.cancel()
	<-e.runners.done
}

// kickRunnerProbe asks for a probe now, without waiting for it. Before the
// loop starts it is remembered, and the loop's first probe answers it.
func (e *Engine) kickRunnerProbe() {
	select {
	case e.runners.kicks() <- struct{}{}:
	default:
	}
}

// kicks is the loop's kick channel, made once whichever of an apply and the
// loop's start reaches it first.
func (p *runnerProbe) kicks() chan struct{} {
	p.once.Do(func() { p.kick = make(chan struct{}, 1) })
	return p.kick
}

// runnerStatus is the latest probe, nil before the first.
func (e *Engine) runnerStatus() *coord.RunnerStatus { return e.runners.latest.Load() }

// probeRunners probes every cell of the active revision's providers.sandbox.
func (e *Engine) probeRunners(ctx context.Context) *coord.RunnerStatus {
	var spec *config.SandboxProvider
	if c := e.Company(); c != nil && c.Config != nil {
		spec = c.Config.Providers.Sandbox
	}
	return probeCells(ctx, spec, time.Now().UTC())
}

// probeCells probes each cell when spec configures boxes, and says every cell
// is unconfigured when it does not: `local:` and the double each serve both.
func probeCells(ctx context.Context, spec *config.SandboxProvider, now time.Time) *coord.RunnerStatus {
	cell := func(p config.Placement, probe func() sandbox.CellProbe) coord.RunnerCell {
		if !spec.Enabled() {
			return coord.RunnerCell{State: coord.RunnerUnconfigured, CheckedAt: now,
				Detail: "providers.sandbox declares no " + string(p) + " cell",
				Fix:    "Add providers.sandbox.local to run executors in " + string(p)}
		}
		if spec.Fake {
			return coord.RunnerCell{State: coord.RunnerReady, CheckedAt: now,
				Detail: "providers.sandbox.fake plays every run in process"}
		}
		got := probe()
		out := coord.RunnerCell{State: coord.RunnerNotReady, Detail: got.Detail, Fix: got.Fix, CheckedAt: now}
		if got.Ready {
			out.State, out.Fix = coord.RunnerReady, ""
		}
		return out
	}
	var local config.LocalSandbox
	if spec != nil && spec.Local != nil {
		local = *spec.Local
	}
	return &coord.RunnerStatus{
		Direct: cell(config.PlacementDirect, func() sandbox.CellProbe {
			return sandbox.ProbeDirect(local.StateDir)
		}),
		Container: cell(config.PlacementContainer, func() sandbox.CellProbe {
			if local.Image == "" {
				return sandbox.CellProbe{Detail: "providers.sandbox.local.image is not set",
					Fix: "Set providers.sandbox.local.image to an image with the harness installed"}
			}
			return sandbox.ProbeContainer(ctx, string(local.Runtime))
		}),
	}
}
