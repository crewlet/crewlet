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

// EACH CELL IS ANSWERED FOR, AND ONLY A DECLARED CATALOGUE IS PROBED: with no
// providers.sandbox, or one configuring no box, every cell is unconfigured,
// never "not ready" (nothing is wrong with it), and a cell of a declared
// catalogue that cannot start a box says why and what to do. The fake
// catalogue plays every cell in process, so every cell is ready.
//
// Mutation: probe without a catalogue, or drop a refusal's fix, and this fails.
func TestEveryCellIsAnsweredAndOnlyADeclaredCatalogueIsProbed(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)

	// NONE, AND ONE WRITTEN EMPTY, which configures no box either.
	for _, spec := range []*config.SandboxProvider{nil, {}} {
		got := probeCells(t.Context(), spec, now)
		for name, cell := range map[string]coord.RunnerCell{"direct": got.Direct, "container": got.Container} {
			if cell.State != coord.RunnerUnconfigured || !cell.CheckedAt.Equal(now) ||
				!strings.Contains(cell.Fix, "providers.sandbox.local") {
				t.Errorf("%s with providers.sandbox %+v = %+v, want unconfigured, checked now, "+
					"naming the block to add", name, spec, cell)
			}
		}
	}

	declared := &config.SandboxProvider{Local: &config.LocalSandbox{StateDir: t.TempDir()}}
	got := probeCells(t.Context(), declared, now)
	if got.Direct.State != coord.RunnerReady {
		t.Errorf("direct with a writable state dir = %+v, want ready", got.Direct)
	}
	if got.Container.State != coord.RunnerNotReady || !strings.Contains(got.Container.Detail, "image") ||
		got.Container.Fix == "" {
		t.Errorf("container with no image = %+v, want not ready, naming the image and a fix", got.Container)
	}

	fake := probeCells(t.Context(), &config.SandboxProvider{Fake: true}, now)
	if fake.Direct.State != coord.RunnerReady || fake.Container.State != coord.RunnerReady {
		t.Errorf("the fake catalogue = %+v, want every cell ready", fake)
	}
}

// THE STATUS RECORD CARRIES THE LATEST PROBE, and an apply asks for a new one: a
// revision that declares a catalogue is answered for at once, not a probe
// interval later.
//
// IN A SYNCTEST BUBBLE: synctest.Wait returns once the probe loop blocks again,
// which is only after it stores its answer; a signal sent from inside the
// probe precedes that store.
//
// Mutation: drop the store, or the kick's probe, and the beat carries no
// answer or the boot probe's.
func TestTheStatusRecordCarriesTheLatestProbeAndAnApplyProbesAgain(t *testing.T) {
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
		if got := statusRecordOf(t, e).Runners; got == nil || got.Direct.State != coord.RunnerUnconfigured {
			t.Fatalf("the beat carries %+v, want the boot probe's answer", got)
		}
		e.kickRunnerProbe()
		synctest.Wait()
		if got := statusRecordOf(t, e).Runners; got == nil || got.Direct.State != coord.RunnerReady {
			t.Fatalf("the beat carries %+v after an apply's probe, want its answer", got)
		}
	})
}
