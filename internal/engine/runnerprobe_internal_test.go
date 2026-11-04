package engine

import (
	"context"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
)

// EACH CELL IS ANSWERED FOR, AND ONLY A DECLARED CELL IS PROBED: a cell the
// company does not declare is unconfigured, never "not ready" (nothing is
// wrong with it), and a declared cell that cannot start a box says why and
// what to do. The fake catalogue plays every cell in process, so every cell
// it declares is ready.
//
// Mutation: probe an undeclared cell, or drop a refusal's fix, and this fails.
func TestEveryCellIsAnsweredAndOnlyADeclaredOneIsProbed(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	noKey := func(string) string { return "" }

	got := probeCells(t.Context(), nil, noKey, now)
	for name, cell := range map[string]coord.RunnerCell{"direct": got.Direct, "container": got.Container, "e2b": got.E2B} {
		if cell.State != coord.RunnerUnconfigured || !cell.CheckedAt.Equal(now) {
			t.Errorf("%s with no providers.sandbox = %+v, want unconfigured, checked now", name, cell)
		}
	}

	declared := &config.SandboxProvider{
		Local: &config.LocalSandbox{StateDir: t.TempDir()},
		E2B:   &config.E2BSandbox{APIKey: "${E2B_API_KEY}"},
	}
	got = probeCells(t.Context(), declared, noKey, now)
	if got.Direct.State != coord.RunnerReady {
		t.Errorf("direct with a writable state dir = %+v, want ready", got.Direct)
	}
	if got.Container.State != coord.RunnerNotReady || !strings.Contains(got.Container.Detail, "image") ||
		got.Container.Fix == "" {
		t.Errorf("container with no image = %+v, want not ready, naming the image and a fix", got.Container)
	}
	if got.E2B.State != coord.RunnerNotReady || !strings.Contains(got.E2B.Fix, "Settings › Secrets") {
		t.Errorf("e2b whose key resolves to nothing = %+v, want not ready, naming where to set it", got.E2B)
	}

	fake := probeCells(t.Context(), &config.SandboxProvider{Fake: true}, noKey, now)
	if fake.Direct.State != coord.RunnerReady || fake.Container.State != coord.RunnerReady || fake.E2B.State != coord.RunnerReady {
		t.Errorf("the fake catalogue = %+v, want every cell ready", fake)
	}
}

// THE HEARTBEAT CARRIES THE LATEST PROBE, and an apply asks for a new one: a
// revision that declares a cell is answered for at once, not five minutes
// later.
//
// IN A SYNCTEST BUBBLE: synctest.Wait returns once the probe loop is blocked
// again, which it is only after storing its answer. A signal sent from inside
// the probe came before that store, and the beat was read as nil (CI run
// 41766528494).
//
// Mutation: drop the store, or the kick's probe, and the beat carries no
// answer or the boot probe's.
func TestTheHeartbeatCarriesTheLatestProbeAndAnApplyProbesAgain(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		e := &Engine{}
		answers := []coord.RunnerState{coord.RunnerUnconfigured, coord.RunnerReady}
		calls := 0
		e.runners.probe = func(context.Context) *coord.RunnerStatus {
			state := answers[min(calls, len(answers)-1)]
			calls++
			return &coord.RunnerStatus{Direct: coord.RunnerCell{State: state}}
		}
		e.startRunnerProbe(t.Context())
		t.Cleanup(e.stopRunnerProbe)
		synctest.Wait()
		if got := beatOf(t, e).Runners; got == nil || got.Direct.State != coord.RunnerUnconfigured {
			t.Fatalf("the beat carries %+v, want the boot probe's answer", got)
		}
		e.kickRunnerProbe()
		synctest.Wait()
		if got := beatOf(t, e).Runners; got == nil || got.Direct.State != coord.RunnerReady {
			t.Fatalf("the beat carries %+v after an apply's probe, want its answer", got)
		}
	})
}
