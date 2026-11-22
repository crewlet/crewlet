package engine

import (
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox/executor"
)

// EVERY RUNNER'S CEILING HOLDS THE ENGINE'S LARGEST ITEM: a hook answer carries
// a person's widest note and, beside it, an item at [maxLateItemUnits]; a poll
// message carries such an item alone. A ceiling below either keeps that item
// from ever reaching the runner. The bounds are the engine's own: a runner that
// measures lower lowers them for every runner, never cuts an item for one.
//
// Mutation: raise maxLateItemUnits or widestNoteUnits past what Claude Code's
// answer leaves, or lower a runner's ceiling, and this goes red.
func TestEveryRunnersCeilingHoldsTheLargestItem(t *testing.T) {
	t.Parallel()
	if got := executor.UTF16Len(widestNote()); got != widestNoteUnits {
		t.Errorf("the widest note is %d units, widestNoteUnits says %d", got, widestNoteUnits)
	}
	for _, name := range executor.Names() {
		if room, hooks := executor.HookRoomBeside(name, widestNote()); hooks && room < maxLateItemUnits {
			t.Errorf("%s's hook answer leaves %d units beside the widest note, under the item bound %d",
				name, room, maxLateItemUnits)
		}
		if ceiling, polls := executor.PollCeiling(name); polls && ceiling < maxLateItemUnits {
			t.Errorf("%s's poll message carries %d units, under the item bound %d", name, ceiling, maxLateItemUnits)
		}
	}
}
