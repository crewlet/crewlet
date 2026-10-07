package jetstream

import (
	"testing"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/topics"
)

// A PAIR IS LISTED ONLY WHEN IT ADDRESSES THE CONSUMER IT WAS READ FROM.
//
// A sweep acts on the pair, never on the consumer: it deletes
// consumerName(topic, group). Metadata naming some other pair would send it to
// delete a subscription it never looked at while the consumer it did find kept
// its mail. And an ephemeral consumer is not a subscription at all, which is a
// different answer from an unprovable one: the second is logged, and every
// dashboard socket holds a consumer of the first kind.
func TestPairOfListsOnlyAPairThatAddressesTheConsumer(t *testing.T) {
	t.Parallel()
	consumer := func(durable, filter string, meta map[string]string) *jetstream.ConsumerInfo {
		return &jetstream.ConsumerInfo{Config: jetstream.ConsumerConfig{
			Durable: durable, Name: durable, FilterSubject: filter, Metadata: meta,
		}}
	}
	ephemeral := &jetstream.ConsumerInfo{Config: jetstream.ConsumerConfig{
		Name: "Xy12ab", FilterSubject: topics.AgentInbox("alice"),
	}}
	for _, tc := range []struct {
		name    string
		info    *jetstream.ConsumerInfo
		want    queue.Subscription
		verdict pairVerdict
	}{
		{"no consumer", nil, queue.Subscription{}, pairNotSubscription},
		{"an ephemeral consumer", ephemeral, queue.Subscription{}, pairNotSubscription},
		{"metadata that derives the name",
			consumer(consumerName("t.x", "h.i"), "t.x", subscriptionMetadata("t.x", "h.i")),
			queue.Subscription{Topic: "t.x", Group: "h.i"}, pairListed},
		{"metadata naming another pair",
			consumer(consumerName("t.x", "g"), "t.x", subscriptionMetadata("t.victim", "grp")),
			queue.Subscription{}, pairUnprovable},
		{"metadata naming half a pair",
			consumer(consumerName("t.x", "g"), "t.x", map[string]string{metaTopic: "t.x"}),
			queue.Subscription{}, pairUnprovable},
		{"no metadata",
			consumer(consumerName("t.x", "g"), "t.x", nil),
			queue.Subscription{}, pairUnprovable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, verdict := pairOf(tc.info)
			if verdict != tc.verdict || got != tc.want {
				t.Fatalf("pairOf = (%+v, %d), want (%+v, %d)", got, verdict, tc.want, tc.verdict)
			}
		})
	}
}

// The same rule through the broker: a consumer whose metadata was rewritten to
// name another subscription is never listed under that subscription.
func TestAConsumerIsNeverListedUnderAPairThatDoesNotAddressIt(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	q := openForTest(t, Config{})
	stream, err := q.streamFor(ctx, "moved.topic")
	if err != nil {
		t.Fatalf("streamFor: %v", err)
	}
	if _, err := q.js.CreateOrUpdateConsumer(ctx, stream, jetstream.ConsumerConfig{
		Durable:       consumerName("moved.topic", "h.i"),
		FilterSubject: "moved.topic",
		Metadata:      subscriptionMetadata("moved.victim", "grp"),
		AckPolicy:     jetstream.AckExplicitPolicy,
	}); err != nil {
		t.Fatalf("CreateOrUpdateConsumer: %v", err)
	}
	got, err := q.ListSubscriptions(ctx, "moved.>")
	if err != nil {
		t.Fatalf("ListSubscriptions: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("ListSubscriptions = %v, want nothing: the metadata names a pair that does not "+
			"address this consumer", got)
	}
}

// A stream this backend did not provision is not searched, even when a
// consumer on it looks exactly like a subscription: an external cluster's
// account can hold another application's streams, and a sweep that found one
// of their consumers would be sent to delete it.
func TestAStreamThisBackendDidNotProvisionIsNotListed(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	q := openForTest(t, Config{})
	if _, err := q.js.CreateStream(ctx, jetstream.StreamConfig{
		Name: "OTHER_APP", Subjects: []string{"otherapp.>"},
	}); err != nil {
		t.Fatalf("CreateStream: %v", err)
	}
	if _, err := q.js.CreateOrUpdateConsumer(ctx, "OTHER_APP", jetstream.ConsumerConfig{
		Durable:       consumerName("otherapp.inbox", "grp"),
		FilterSubject: "otherapp.inbox",
		Metadata:      subscriptionMetadata("otherapp.inbox", "grp"),
	}); err != nil {
		t.Fatalf("CreateOrUpdateConsumer: %v", err)
	}
	got, err := q.ListSubscriptions(ctx, ">")
	if err != nil {
		t.Fatalf("ListSubscriptions: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("ListSubscriptions(>) = %v, want nothing from a stream this backend does not own", got)
	}
}
