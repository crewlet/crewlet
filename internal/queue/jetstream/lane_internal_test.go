package jetstream

import (
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/topics"
)

// A MAILBOX WHOSE LANE IS GONE IS LISTED AS PARTIAL, AND ENSURING IT REPAIRS IT.
//
// Under interest retention a priority event published to a lane no consumer
// covers is dropped at the broker with no error anywhere, so a seat whose
// lane went missing (a mailbox made before lanes existed, a consumer somebody
// deleted) would lose every message a person sent it. The listing is how a
// node's mailbox pass learns that, and EnsureSubscription is what it then
// calls; both halves are held here because the conformance suite cannot
// remove one consumer of a subscription without the backend's own client.
func TestALaneThatWentMissingIsListedPartialAndEnsuredBack(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	q := openForTest(t, Config{})
	topic := topics.AgentInbox(uuid.New())
	if _, err := q.EnsureSubscription(ctx, topic, "grp"); err != nil {
		t.Fatalf("EnsureSubscription: %v", err)
	}
	stream, err := q.streamFor(ctx, topic)
	if err != nil {
		t.Fatalf("streamFor: %v", err)
	}
	if err := q.js.DeleteConsumer(ctx, stream, consumerName(queue.LaneOf(topic), "grp")); err != nil {
		t.Fatalf("delete the lane's consumer: %v", err)
	}

	subs, err := q.ListSubscriptions(ctx, topic)
	if err != nil {
		t.Fatalf("ListSubscriptions: %v", err)
	}
	if len(subs) != 1 || !subs[0].Partial {
		t.Fatalf("a mailbox without its lane is listed as %+v, want it partial", subs)
	}

	made, err := q.EnsureSubscription(ctx, topic, "grp")
	if err != nil {
		t.Fatalf("EnsureSubscription again: %v", err)
	}
	if !made {
		t.Error("ensuring a partial mailbox reported provisioning nothing")
	}
	subs, err = q.ListSubscriptions(ctx, topic)
	if err != nil {
		t.Fatalf("ListSubscriptions again: %v", err)
	}
	if len(subs) != 1 || subs[0].Partial {
		t.Fatalf("after ensuring it the mailbox is listed as %+v, want it complete", subs)
	}
}
