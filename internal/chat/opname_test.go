package chat_test

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/chat"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A DESTRUCTION'S OPERATION IS NAMED FOR THE GESTURE, because the room its
// record arbitrates on is not.
//
// An erase or a prune collapses onto the record its operation already holds on
// the room's subject, whatever that record says — so an id carried to other
// messages, or to a later cutoff, in the same room would be answered from the
// first gesture while nothing it named was destroyed. The name is what a
// brought-back id is held to, so it has to tell those apart, and has to agree
// with itself on one gesture spelled two ways.
func TestADestructionsOperationIsNamedForItsGesture(t *testing.T) {
	t.Parallel()
	erase := chat.EraseOpName("room-1", []string{"m-1", "m-2"})
	for name, other := range map[string]string{
		"another message set":         chat.EraseOpName("room-1", []string{"m-1", "m-3"}),
		"a subset of the messages":    chat.EraseOpName("room-1", []string{"m-1"}),
		"the same messages elsewhere": chat.EraseOpName("room-2", []string{"m-1", "m-2"}),
	} {
		if other == erase {
			t.Errorf("an erase of %s is named %q, the same as the first", name, erase)
		}
	}
	for name, same := range map[string]string{
		"in another order":   chat.EraseOpName("room-1", []string{"m-2", "m-1"}),
		"with one repeated":  chat.EraseOpName("room-1", []string{"m-1", "m-2", "m-1"}),
		"with spaces around": chat.EraseOpName(" room-1 ", []string{" m-1", "m-2 "}),
	} {
		if same != erase {
			t.Errorf("the same erase %s is named %q, not %q — the store reads the "+
				"list as a set, so a retry spelled that way would be refused", name,
				same, erase)
		}
	}

	cutoff := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	prune := chat.PruneOpName("room-1", cutoff)
	if later := chat.PruneOpName("room-1", cutoff.Add(time.Second)); later == prune {
		t.Errorf("a prune to a later cutoff is named %q, the same as the first", prune)
	}
	if elsewhere := chat.PruneOpName("room-2", cutoff); elsewhere == prune {
		t.Errorf("a prune of another room is named %q, the same as the first", prune)
	}
	offset := cutoff.In(time.FixedZone("UTC+2", 2*60*60))
	if again := chat.PruneOpName("room-1", offset); again != prune {
		t.Errorf("one instant spelled with an offset is named %q, not %q", again, prune)
	}
	if erase == prune {
		t.Error("an erase and a prune of one room share a name")
	}

	// AND EVERY ID IT NAMES IS ONE A CALLER CAN BRING BACK: a room id is a
	// uuid, so the longest name keeps the id inside the bound a brought-back
	// id is held to.
	room := "6dd4b0df-f455-448e-80e9-1f2a3b4c5d6e"
	for _, name := range []string{
		chat.EraseOpName(room, []string{"m-1"}),
		chat.PruneOpName(room, time.Date(2262, 4, 11, 23, 47, 16, 0, time.UTC)),
	} {
		id := statelog.NewOpID(time.Now(), name)
		if err := statelog.CheckCallerOpID(id); err != nil {
			t.Errorf("an operation named %q could not be brought back: %v", name, err)
		}
		if !statelog.OpNamed(id, name) {
			t.Errorf("an id minted under %q is not read as named for it", name)
		}
	}
}
