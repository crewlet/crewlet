package engine

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A TURN READS NO OLDER THAN THE CHANGE THAT WOKE IT: the committing record's
// position, which a first-party wake carries, is in this node's floors once the
// turn has begun — so every read the turn routes, to whichever holder answers
// it, waits for it — and a token this build cannot read costs the turn its
// floor, never the turn.
func TestATurnReadsNoOlderThanTheChangeThatWokeIt(t *testing.T) {
	t.Parallel()
	e, _ := spendingEngine(t)
	stream, _ := LayoutZero().Stream(statelog.LogID{Domain: "tracker",
		Partition: statelog.EstatePartition})
	at := statelog.Position{Stream: stream, Generation: 2, Seq: 77}
	woke := taskWake("task-9", "ENG-9")
	woke.Payload = map[string]any{notify.TriggerField: at.String()}
	garbled := taskWake("task-9", "ENG-9")
	garbled.Payload = map[string]any{notify.TriggerField: "not a position"}

	if _, err := e.runTurn(t.Context(), Request{
		Handle: "swe", WorkKey: "wk-1", RunID: "run-1", WorkSince: time.Now().UTC(),
		Events: []*events.Event{garbled, woke},
	}); err != nil {
		t.Fatalf("runTurn: %v", err)
	}
	floors := e.router.Session().Floors([]string{stream})
	if len(floors) != 1 || floors[0] != at {
		t.Fatalf("this node's floors on %s are %v, want the trigger %v", stream, floors, at)
	}
}
