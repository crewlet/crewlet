package sandbox

import (
	"fmt"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/coord/memory"
)

// A SAVE ITS BOX HAS NO ROOM FOR WAITS, NEVER DOUBLED: a pack refused for want
// of the box's room ([ErrSaveNoRoom]) ends the save not yet, its failures
// unchanged, so it is due again at one interval; the job's first such save says
// why at Warn, and every one after at Debug alone.
//
// Mutation: classify it failed, or log it at Warn every time.
func TestASaveItsBoxHasNoRoomForWaitsNeverDoubled(t *testing.T) {
	logs := captureLogs(t)
	inBubble(t, func(t *testing.T) {
		rig, _, _ := savingRig(t, memory.NewFleet(), newTakerStore())
		rig.launchKept(t, "t1")
		rig.runner.PackSaveErr = fmt.Errorf("%w: the pack needs 300 MiB, and the box has 200 MiB", ErrSaveNoRoom)
		if out := saveOf(rig, "t1", nil); out.class != saveNotYet || out.unplaced != "room" {
			t.Fatalf("the save came to %+v, want it not yet, its box's room named", out)
		}
		for offer := range 3 {
			rig.now = rig.now.Add(rigSaveInterval)
			if !offerOf(t, rig, "t1") {
				t.Fatalf("offer %d, one interval after the last, started no save: a roomless save was doubled",
					offer+1)
			}
			rig.coordinator.saves.mu.Lock()
			memo := *rig.coordinator.saves.memos["t1"]
			rig.coordinator.saves.mu.Unlock()
			if due := memo.due(rigSaveInterval); memo.failures != 0 || !due.Equal(memo.last.Add(rigSaveInterval)) {
				t.Errorf("after offer %d the job's memory counts %d failures and is due %v after its last "+
					"attempt, want none and one interval", offer+1, memo.failures, due.Sub(memo.last))
			}
		}
		warned := 0
		for line := range strings.Lines(logs.String()) {
			if strings.Contains(line, "executor_session_not_kept") && strings.Contains(line, "lacks the memory") {
				warned++
			}
		}
		if warned != 1 {
			t.Errorf("three roomless saves of one launch logged it %d times at Warn, want once", warned)
		}
	})
}
