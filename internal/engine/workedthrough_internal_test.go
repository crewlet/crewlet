package engine

import (
	"testing"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/notify"
)

// THE RECORD AND THE DROP NAME A MESSAGE THE SAME WAY. A turn records what its
// thread block showed it under the thread it read and the ids the block
// reported; a retried message is looked up under its own metadata. If the two
// derivations disagreed by a byte, every record would be written and no drop
// would ever match it — a guard that cannot fire, passing every test that
// stubs one side.
func TestTheWorkedThroughRecordAndTheDropAgreeOnAMessagesKey(t *testing.T) {
	t.Parallel()
	message := func(ts string) *events.Event {
		return events.New(types.ExternalNotification{
			NotificationSource: "mattermost", SourceEventType: "message",
			Metadata: map[string]string{
				notify.TransportField: "mattermost", "channel": "C9",
				"thread_ts": "root-post", "ts": ts,
			},
		}, events.TraceContext{})
	}
	waiting, trigger := message("post-1"), message("post-2")

	if got := triggerMessageOf([]*events.Event{trigger}); got != "post-2" {
		t.Fatalf("the trigger's message is %q, want its own ts", got)
	}
	recorded := workedThroughKeys(threadOf([]*events.Event{trigger}), []string{"post-1"})
	looked, ok := chatKeyOf(waiting)
	if !ok || len(recorded) != 1 || recorded[0] != looked {
		t.Fatalf("recorded %v, the retry looks up %q (ok=%v): the two do not agree", recorded, looked, ok)
	}
	// AND A TOP-LEVEL MESSAGE HAS NO KEY: no thread block ever showed it to a
	// later turn, so nothing may drop it as answered there.
	toplevel := events.New(types.ExternalNotification{
		Metadata: map[string]string{notify.TransportField: "mattermost", "channel": "C9", "ts": "post-3"},
	}, events.TraceContext{})
	if _, ok := chatKeyOf(toplevel); ok {
		t.Fatal("a top-level message was given a worked-through key")
	}
}
