package sandbox

import (
	"context"
	"slices"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/crewlet/crewlet/internal/coord/memory"
)

// A NODE PACKS ONE SESSION AT A TIME IN ITS OWN MEMORY: a second pack in a box
// that shares the node's memory, a save's or a take's on demand, does not start
// until the first pack returns, and starts once it has, whatever is left of the
// first's take; one in a box that owns its memory starts beside the first.
//
// Mutation: drop the slot, take it for every cell, or give it back after the
// take rather than the pack.
func TestANodePacksOneSessionAtATimeInItsOwnMemory(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// firstTakes and secondTakes: the pack is a take's on demand, else a
		// save's.
		firstTakes, secondTakes bool
		second                  Placement
	}{
		{name: "a save's pack, then a direct save's", second: Direct},
		{name: "a save's pack, then a container save's", second: Container},
		{name: "a save's pack, then a direct take's on demand", secondTakes: true, second: Direct},
		{name: "a take's pack on demand, then a direct save's", firstTakes: true, second: Direct},
		{name: "a take's pack on demand, then a container save's", firstTakes: true, second: Container},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			inBubble(t, func(t *testing.T) {
				rig, _, _ := savingRig(t, memory.NewFleet(), newTakerStore())
				everyCell(t, rig)
				first := rig.launchKept(t, "first")
				rig.launchKept(t, "second")
				reshape(t, rig, "second", func(row *PendingRun) { row.Placement = string(tc.second) })
				second := rig.get("second")

				var (
					mu     sync.Mutex
					packed []string
					takes  int
				)
				// THE FIRST PACK HELD until hold closes; THE REST OF EVERY
				// TAKE after it (a save's take, a take's stream) until rest
				// closes, so a slot held past the pack is held while the
				// case looks.
				hold, rest, entered := make(chan struct{}), make(chan struct{}), make(chan struct{})
				rig.runner.Packing = func(_ context.Context, box Sandbox) {
					mu.Lock()
					packed = append(packed, map[string]string{first.SandboxID: "first",
						second.SandboxID: "second"}[box.ID()])
					mu.Unlock()
					if box.ID() == first.SandboxID {
						close(entered)
						<-hold
					}
				}
				rig.runner.Session.Taking = func(context.Context) {
					mu.Lock()
					takes++
					firstTake := takes == 1
					mu.Unlock()
					switch {
					case firstTake && tc.firstTakes:
						close(entered)
						<-hold
					case !tc.firstTakes:
						<-rest
					}
				}
				rig.runner.Session.Streaming = func(context.Context) { <-rest }
				// THE SECOND'S START: a take's on demand is recorded as it
				// begins, a save's as its pack does.
				secondStarted := func() bool {
					if tc.secondTakes {
						return slices.ContainsFunc(rig.runner.Takes(), func(take SessionTake) bool {
							return take.Pack
						})
					}
					mu.Lock()
					defer mu.Unlock()
					return slices.Contains(packed, "second")
				}

				var wg sync.WaitGroup
				run := func(who string, onDemand bool) {
					wg.Go(func() {
						if !onDemand {
							saveOf(rig, who, nil)
							return
						}
						row := rig.get(who)
						spooled, _ := rig.coordinator.takeCapture(t.Context(), row,
							rig.provider.Box(row.SandboxID), rig.runner, readCapture(BoxReadCollect, Result{}, false))
						rig.coordinator.keepLater(t.Context(), spooled)
					})
				}
				run("first", tc.firstTakes)
				<-entered
				run("second", tc.secondTakes)
				synctest.Wait()
				beside := tc.second.OwnsMemory()
				if got := secondStarted(); got != beside {
					t.Errorf("with the first pack held, the second (its box %s) started = %v, want %v: one "+
						"pack at a time only where a box shares the node's memory", tc.second, got, beside)
				}
				close(hold)
				synctest.Wait()
				if !secondStarted() {
					t.Error("the second pack had not started once the first pack returned, its take still " +
						"held: the slot is the pack's, not the take's")
				}
				close(rest)
				wg.Wait()
				rig.awaitKeeps()
			})
		})
	}
}
