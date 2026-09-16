package jetstream

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/topics"
)

// legacyConsumer creates a durable consumer exactly as a build that predates
// the subscription metadata did: the same name and filter, and no pair.
func legacyConsumer(ctx context.Context, t *testing.T, q *Queue, topic, group string) {
	t.Helper()
	stream, err := q.streamFor(ctx, topic)
	if err != nil {
		t.Fatalf("streamFor(%s): %v", topic, err)
	}
	if _, err := q.js.CreateOrUpdateConsumer(ctx, stream, jetstream.ConsumerConfig{
		Durable:       consumerName(topic, group),
		FilterSubject: topic,
		AckPolicy:     jetstream.AckExplicitPolicy,
		DeliverPolicy: jetstream.DeliverAllPolicy,
	}); err != nil {
		t.Fatalf("create the legacy consumer for %s/%s: %v", topic, group, err)
	}
}

// A mailbox created before consumers carried their pair is exactly the one the
// listing exists for: a seat removed before this build shipped left it, and no
// node will ever declare it again to stamp the pair on. So its pair is
// recovered from the name, where the name proves it.
func TestAConsumerFromBeforeTheMetadataIsListedByItsName(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	q := openForTest(t, Config{})
	inbox := queue.Subscription{Topic: topics.AgentInbox("gone-seat"), Group: topics.AgentInboxGroup("gone-seat")}
	control := queue.Subscription{Topic: topics.AgentControl("gone-seat"), Group: topics.AgentControlGroup("gone-seat")}
	legacyConsumer(ctx, t, q, inbox.Topic, inbox.Group)
	legacyConsumer(ctx, t, q, control.Topic, control.Group)

	got, err := q.ListSubscriptions(ctx, topics.AgentInboxPrefix+">")
	if err != nil {
		t.Fatalf("ListSubscriptions: %v", err)
	}
	want := []queue.Subscription{control, inbox}
	if !slices.Equal(got, want) {
		t.Fatalf("ListSubscriptions = %v, want the recovered pairs %v", got, want)
	}
}

// A legacy consumer whose name cannot prove its pair is left out, and does not
// cost the rest of the listing: one unreadable consumer must not hide every
// mailbox beside it.
func TestAnUnprovableLegacyConsumerIsLeftOutAndTheRestAreListed(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	q := openForTest(t, Config{})
	// A dotted group: its rewrite is lossy, so the name cannot say whether
	// the group was "h.i" or "h_i".
	legacyConsumer(ctx, t, q, "odd.topic", "h.i")
	if _, err := q.EnsureSubscription(ctx, "odd.other", "grp"); err != nil {
		t.Fatalf("EnsureSubscription: %v", err)
	}
	got, err := q.ListSubscriptions(ctx, "odd.>")
	if err != nil {
		t.Fatalf("ListSubscriptions: %v", err)
	}
	if want := []queue.Subscription{{Topic: "odd.other", Group: "grp"}}; !slices.Equal(got, want) {
		t.Fatalf("ListSubscriptions = %v, want only the provable %v", got, want)
	}
}

// Declaring a subscription again stamps its pair onto a consumer that lacked
// it, so an old mailbox of a seat still in the company stops depending on its
// name at the next boot.
func TestEnsuringALegacySubscriptionStampsItsPair(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	q := openForTest(t, Config{})
	legacyConsumer(ctx, t, q, "stamp.t", "h.i")
	if created, err := q.EnsureSubscription(ctx, "stamp.t", "h.i"); err != nil || created {
		t.Fatalf("EnsureSubscription over the legacy consumer = (%v, %v), want (false, nil)", created, err)
	}
	stream, err := q.streamFor(ctx, "stamp.t")
	if err != nil {
		t.Fatalf("streamFor: %v", err)
	}
	cons, err := q.js.Consumer(ctx, stream, consumerName("stamp.t", "h.i"))
	if err != nil {
		t.Fatalf("Consumer: %v", err)
	}
	if md := cons.CachedInfo().Config.Metadata; md[metaTopic] != "stamp.t" || md[metaGroup] != "h.i" {
		t.Fatalf("consumer metadata = %v, want the pair stamped", md)
	}
	got, err := q.ListSubscriptions(ctx, "stamp.>")
	if err != nil {
		t.Fatalf("ListSubscriptions: %v", err)
	}
	if want := []queue.Subscription{{Topic: "stamp.t", Group: "h.i"}}; !slices.Equal(got, want) {
		t.Fatalf("ListSubscriptions = %v, want %v", got, want)
	}
}

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
		{"metadata that derives the name, over a name that cannot prove it",
			consumer(consumerName("t.x", "h.i"), "t.x", subscriptionMetadata("t.x", "h.i")),
			queue.Subscription{Topic: "t.x", Group: "h.i"}, pairListed},
		{"metadata naming another pair, over a name that cannot prove its own",
			consumer(consumerName("t.x", "h.i"), "t.x", subscriptionMetadata("t.victim", "grp")),
			queue.Subscription{}, pairUnprovable},
		{"metadata naming another pair, over a name that proves its own",
			consumer(consumerName("t.x", "g"), "t.x", subscriptionMetadata("t.victim", "grp")),
			queue.Subscription{Topic: "t.x", Group: "g"}, pairListed},
		{"no metadata, a provable name",
			consumer(consumerName("t.x", "g"), "t.x", nil),
			queue.Subscription{Topic: "t.x", Group: "g"}, pairListed},
		{"no metadata, a lossy name",
			consumer(consumerName("t.x", "h.i"), "t.x", nil),
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
			"address this consumer, and its own name proves none", got)
	}
}

func TestPairFromConsumerNameAcceptsOnlyWhatItCanProve(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("x", consumerNameMax)
	for _, tc := range []struct {
		name, nameTopic, group, topic string
		want                          bool
	}{
		{"a seat inbox", topics.AgentInbox("alice"), topics.AgentInboxGroup("alice"), topics.AgentInbox("alice"), true},
		{"an underscore in the group", "t.x", "a_b", "t.x", true},
		{"a lossy group", "t.x", "a.b", "t.x", false},
		{"a truncated name", "t.x", long, "t.x", false},
		{"a filter that is not the named topic", "t.x", "g", "t.y", false},
		{"no filter", "t.x", "g", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			name := consumerName(tc.nameTopic, tc.group)
			group, ok := pairFromConsumerName(name, tc.topic)
			if ok != tc.want {
				t.Fatalf("pairFromConsumerName(%q, %q) ok = %v, want %v", name, tc.topic, ok, tc.want)
			}
			if ok && group != tc.group {
				t.Fatalf("recovered group %q, want %q", group, tc.group)
			}
		})
	}
	if _, ok := pairFromConsumerName("short", "t.x"); ok {
		t.Fatal("a name too short to hold a digest was accepted")
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
