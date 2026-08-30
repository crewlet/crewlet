package pulsar

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/topics"
)

// The cases in this file are the ones that need no broker: the outcome
// mapping, the pause-hold bookkeeping, and the refusals that happen before
// anything reaches the wire. Everything that needs a real Pulsar is in
// conformance_test.go and skips without one.

// noopAdmin stands in for the broker's admin endpoint where a case never
// reaches it.
type noopAdmin struct{}

func (noopAdmin) Subscriptions(context.Context, string) ([]string, error) { return nil, nil }
func (noopAdmin) EnsureSubscription(context.Context, string, string) (bool, error) {
	return true, nil
}
func (noopAdmin) DeleteSubscription(context.Context, string, string) (bool, error) {
	return true, nil
}
func (noopAdmin) PeekBacklog(context.Context, string, string) ([][]byte, error) { return nil, nil }
func (noopAdmin) Close()                                                        {}

func offlineQueue(t *testing.T) *Queue {
	t.Helper()
	return newQueueOn(testCfg(), nil, noopAdmin{})
}

// TestActionForKeepsADeferralFree is the rule that distinguishes this backend
// from JetStream, and getting it wrong is silent in both directions.
//
// Defer means "this process has lost the right to do this work". Acking would
// claim work it will not perform. NAKing would spend dead-letter budget on a
// message nothing is wrong with — and a busy seat changes hands often, so a
// healthy event would eventually die having never failed. On Pulsar a
// graceful close returns unacked messages at redeliveryCount 0 (measured;
// adr-104), so leaving it unacked is both correct and free.
func TestActionForKeepsADeferralFree(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		outcome queue.Outcome
		want    brokerAction
	}{
		{"ack", queue.OutcomeAck, actionAck},
		{"nak", queue.OutcomeNak, actionNak},
		{"defer", queue.OutcomeDefer, actionLeave},
		// The zero value is Ack, the same default a bare return gives in
		// the Python engine.
		{"the zero outcome", queue.Result{}.Outcome, actionAck},
	} {
		if got := actionFor(tc.outcome); got != tc.want {
			t.Errorf("%s: actionFor = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestEveryReasonToStopBlocksTheAttachment.
//
// A blocked attachment holds nothing: the loop closes its consumer, which
// returns the prefetch AND everything unacked at redeliveryCount 0. That is
// one mechanism for all four reasons, so what has to be pinned is that all
// four reach it — a new flag added to the struct and forgotten here would
// leave a consumer sitting on mail nobody can see, and this client has no ack
// timeout to rescue it.
func TestEveryReasonToStopBlocksTheAttachment(t *testing.T) {
	t.Parallel()
	q := offlineQueue(t)
	key := attachKey{"seat.inbox", "grp"}
	for _, tc := range []struct {
		name string
		set  func(a *attachment)
		want bool
	}{
		{"nothing", func(*attachment) {}, false},
		{"quiesced", func(a *attachment) { a.quiesced.Store(true) }, true},
		{"detached", func(a *attachment) { a.detached.Store(true) }, true},
		{"draining", func(a *attachment) { a.paused.Store(true) }, true},
	} {
		a := &attachment{q: q, key: key, log: q.log}
		tc.set(a)
		if got := a.blocked(); got != tc.want {
			t.Errorf("%s: blocked() = %v, want %v", tc.name, got, tc.want)
		}
	}

	// The fourth reason lives on the QUEUE, not the attachment, because a
	// hold is routinely taken BEFORE anything attaches.
	a := &attachment{q: q, key: key, log: q.log}
	if err := q.PauseTopic(context.Background(), key.topic, key.group, "sandbox"); err != nil {
		t.Fatalf("PauseTopic: %v", err)
	}
	if !a.blocked() {
		t.Error("a held subscription is not blocked — it would take exactly the work it was told not to")
	}
}

// TestPauseHoldsAreReasonScoped. Two independent subsystems gate the same
// inbox — the sandbox busy gate and the config-divergence shed — and with one
// flat hold the sandbox resuming its own run would un-gate a node serving a
// stale company, on a completely ordinary code path.
func TestPauseHoldsAreReasonScoped(t *testing.T) {
	t.Parallel()
	q := offlineQueue(t)
	ctx := context.Background()
	const topic, group = "seat.inbox", "grp"

	for _, reason := range []string{"sandbox", "config-divergence"} {
		if err := q.PauseTopic(ctx, topic, group, reason); err != nil {
			t.Fatalf("PauseTopic(%s): %v", reason, err)
		}
	}
	if got := q.PauseHolds(topic, group); strings.Join(got, ",") != "config-divergence,sandbox" {
		t.Fatalf("PauseHolds = %v, want both reasons", got)
	}

	if err := q.ResumeTopic(ctx, topic, group, "sandbox"); err != nil {
		t.Fatalf("ResumeTopic: %v", err)
	}
	if !q.held(attachKey{topic, group}) {
		t.Fatal("one subsystem released its hold and un-gated another's")
	}
	if err := q.ResumeTopic(ctx, topic, group, "config-divergence"); err != nil {
		t.Fatalf("ResumeTopic: %v", err)
	}
	if q.held(attachKey{topic, group}) {
		t.Fatal("the last hold did not release the subscription")
	}
	// Releasing a hold that was never taken must not error, and must not
	// resurrect an entry.
	if err := q.ResumeTopic(ctx, "never.paused", group, "test"); err != nil {
		t.Fatalf("ResumeTopic on an unpaused subscription: %v", err)
	}
	if got := q.PauseHolds("never.paused", group); len(got) != 0 {
		t.Fatalf("PauseHolds on an unpaused subscription = %v, want none", got)
	}
}

// TestPauseHoldsAreKeyedByThePair. Keyed by topic alone, a hold gated every
// group on a shared subject like crewlet.events.* — one seat's sandbox pause
// silenced the fleet's routing.
func TestPauseHoldsAreKeyedByThePair(t *testing.T) {
	t.Parallel()
	q := offlineQueue(t)
	topic := topics.Event("task_created")
	if err := q.PauseTopic(context.Background(), topic, "held-grp", "sandbox"); err != nil {
		t.Fatalf("PauseTopic: %v", err)
	}
	if !q.held(attachKey{topic, "held-grp"}) {
		t.Fatal("the held group is not held")
	}
	if q.held(attachKey{topic, "free-grp"}) {
		t.Fatal("a hold on one group gated its neighbour on the same subject")
	}
}

// TestTheVerbsOnAnUnattachedPairChangeNothing. Each reports whether an
// attachment existed, and a Quiesce that set the flag anyway would leave a
// (topic, group) nothing can ever be delivered on — invisible from outside
// until someone attaches.
func TestTheVerbsOnAnUnattachedPairChangeNothing(t *testing.T) {
	t.Parallel()
	q := offlineQueue(t)
	ctx := context.Background()
	const topic, group = "seat.unowned", "grp"

	if quiesced, err := q.Quiesce(ctx, topic, group); err != nil || quiesced {
		t.Errorf("Quiesce with nothing attached = (%v, %v), want (false, nil)", quiesced, err)
	}
	if resumed, err := q.Unquiesce(ctx, topic, group); err != nil || resumed {
		t.Errorf("Unquiesce with nothing attached = (%v, %v), want (false, nil)", resumed, err)
	}
	if detached, err := q.Detach(ctx, topic, group); err != nil || detached {
		t.Errorf("Detach with nothing attached = (%v, %v), want (false, nil)", detached, err)
	}
	if q.Quiescing(topic, group) {
		t.Error("an unattached pair reports itself quiesced")
	}
	if got := q.Attachments(); len(got) != 0 {
		t.Errorf("Attachments = %v, want none", got)
	}
}

// TestDetachDropsThisAttachmentsPauseHolds. A hold is state about ONE
// attachment; one that outlived a detach would leave a node that re-attached
// later silently deaf, with nothing left to release it.
func TestDetachDropsThisAttachmentsPauseHolds(t *testing.T) {
	t.Parallel()
	q := offlineQueue(t)
	ctx := context.Background()
	const topic, group = "seat.inbox", "grp"
	if err := q.PauseTopic(ctx, topic, group, "sandbox"); err != nil {
		t.Fatalf("PauseTopic: %v", err)
	}
	if _, err := q.Detach(ctx, topic, group); err != nil {
		t.Fatalf("Detach: %v", err)
	}
	if got := q.PauseHolds(topic, group); len(got) != 0 {
		t.Fatalf("a pause hold survived the detach: %v", got)
	}
}

// TestPublishRefusesWhatWouldLandWhereNobodyReads, before it reaches a
// producer. An empty segment is a real Pulsar topic that no subscription
// covers, and a '/' addresses another namespace entirely.
func TestPublishRefusesAnUnroutableSubject(t *testing.T) {
	t.Parallel()
	q := offlineQueue(t)
	for _, subject := range []string{"", topics.AgentInbox(""), "crewlet.agent..inbox", "acme/prod/x"} {
		err := q.Publish(context.Background(), subject, &events.Event{Type: "probe"})
		if !errors.Is(err, ErrSubject) {
			t.Errorf("Publish(%q) = %v, want an ErrSubject", subject, err)
		}
	}
}

func TestPublishAfterStopIsRefused(t *testing.T) {
	t.Parallel()
	q := offlineQueue(t)
	if err := q.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	// Idempotent: a second stop is not an error.
	if err := q.Stop(context.Background()); err != nil {
		t.Fatalf("second Stop: %v", err)
	}
	if err := q.Publish(context.Background(), "topic.a", &events.Event{Type: "probe"}); !errors.Is(err, ErrClosed) {
		t.Fatalf("Publish after Stop = %v, want ErrClosed", err)
	}
}

// TestStopClearsPauseHolds. They are process-local state about attachments
// that no longer exist; carrying them past a stop would leave a reused queue
// silently deaf on subjects nothing is holding any more.
func TestStopClearsPauseHolds(t *testing.T) {
	t.Parallel()
	q := offlineQueue(t)
	ctx := context.Background()
	if err := q.PauseTopic(ctx, "seat.inbox", "grp", "sandbox"); err != nil {
		t.Fatalf("PauseTopic: %v", err)
	}
	if err := q.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if got := q.PauseHolds("seat.inbox", "grp"); len(got) != 0 {
		t.Fatalf("pause holds survived Stop: %v", got)
	}
}

// TestDeadLettersStayOutsideTheDashboardsFeed. Pulsar's own default DLQ name
// (<topic>-<sub>-DLQ) sits under the topic it came from, so on
// crewlet.events.* it would be picked up by the dashboard's crewlet.events.>
// broadcast stream and resurface a poison event as live traffic on every
// screen.
func TestDeadLettersStayOutsideTheDashboardsFeed(t *testing.T) {
	t.Parallel()
	q := offlineQueue(t)
	topic := topics.Event("task_created")
	dlq := q.deadLetterTopic(topic, "agent-alice")
	if !strings.HasPrefix(dlq, q.cfg.fullTopic(topics.DeadLetterPrefix)) {
		t.Fatalf("dead-letter topic %q is not under the dead-letter prefix", dlq)
	}
	subject := q.cfg.localSubject(dlq)
	if strings.HasPrefix(subject, topics.EventsPrefix) {
		t.Fatalf("dead-letter subject %q is inside the events feed", subject)
	}
	if err := checkSubject(subject); err != nil {
		t.Fatalf("the dead-letter subject is not a publishable subject: %v", err)
	}
}

// TestSubscribeRefusesANilHandler. Accepting one would attach a consumer that
// swallows a seat's mail and panics on the first delivery — the seat reads as
// served and answers nothing.
func TestSubscribeRefusesANilHandler(t *testing.T) {
	t.Parallel()
	q := offlineQueue(t)
	ctx := context.Background()
	if err := q.Subscribe(ctx, "topic.a", "grp", nil); !errors.Is(err, ErrSubject) {
		t.Errorf("Subscribe with no handler = %v, want an ErrSubject", err)
	}
	if err := q.SubscribeBatch(ctx, "topic.a", "grp", nil, nil, nil); !errors.Is(err, ErrSubject) {
		t.Errorf("SubscribeBatch with no handler = %v, want an ErrSubject", err)
	}
	if _, err := q.SubscribeStream(ctx, "crewlet.events.>", nil); !errors.Is(err, ErrSubject) {
		t.Errorf("SubscribeStream with no handler = %v, want an ErrSubject", err)
	}
}

// TestBackendIsStableAndLowercase — operators read it, nothing branches on it.
func TestBackendIsStableAndLowercase(t *testing.T) {
	t.Parallel()
	if got := offlineQueue(t).Backend(); got != "pulsar" {
		t.Fatalf("Backend() = %q, want pulsar", got)
	}
}

// EVERY VERB IS REFUSED AFTER STOP, AND WITH THE CONTRACT'S SENTINEL.
//
// The certification suite asserts this against a real broker, which is where
// it was caught — but the broker is not what makes it true, and a rule only a
// CI job with a container can check is a rule this package can regress on any
// afternoon. So the same eleven verbs run here against a stopped offline
// queue: the answers are decided before anything reaches the wire, so they are
// checkable without one.
//
// Two things were wrong and each is separately silent. Seven verbs — the two
// admin ones over a REST client Stop does not close, and the five that only
// touch process-local maps — returned SUCCESS, so a shutdown path believed it
// had gated, detached or provisioned a subscription that no longer existed;
// EnsureSubscription genuinely created durable broker state a stopped queue
// would never attach to or delete. And the refusals that did happen carried
// only this backend's own error, so a seat release testing queue.ErrNotLive
// read "the mailbox is already down" as "the detach failed" — and KEPT THE
// LEASE.
func TestEveryVerbIsRefusedAfterStop(t *testing.T) {
	t.Parallel()
	q := offlineQueue(t)
	ctx := context.Background()
	if err := q.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	handler := func(context.Context, *events.Event) queue.Result { return queue.Ack() }
	batch := func(context.Context, []*events.Event) queue.Result { return queue.Ack() }
	stream := func(context.Context, string, *events.Event) {}
	unsub, streamErr := q.SubscribeStream(ctx, "topic.>", stream)
	if unsub != nil {
		_ = unsub(ctx)
	}

	second := func(_ bool, err error) error { return err }
	for _, verb := range []struct {
		name string
		err  error
	}{
		{"Publish", q.Publish(ctx, "topic.t", &events.Event{Type: "probe"})},
		{"Subscribe", q.Subscribe(ctx, "topic.t", "g", handler)},
		{"SubscribeBatch", q.SubscribeBatch(ctx, "topic.t", "g", batch, nil, nil)},
		{"Quiesce", second(q.Quiesce(ctx, "topic.t", "g"))},
		{"Unquiesce", second(q.Unquiesce(ctx, "topic.t", "g"))},
		{"Detach", second(q.Detach(ctx, "topic.t", "g"))},
		{"EnsureSubscription", second(q.EnsureSubscription(ctx, "topic.t", "g"))},
		{"DeleteSubscription", second(q.DeleteSubscription(ctx, "topic.t", "g"))},
		{"SubscribeStream", streamErr},
		{"PauseTopic", q.PauseTopic(ctx, "topic.t", "g", "test")},
		{"ResumeTopic", q.ResumeTopic(ctx, "topic.t", "g", "test")},
	} {
		if verb.err == nil {
			t.Errorf("%s succeeded on a stopped queue; whatever it did was done "+
				"through a closed client, and any state it left behind outlives "+
				"the queue that took it", verb.name)
			continue
		}
		if !errors.Is(verb.err, queue.ErrNotLive) {
			t.Errorf("%s refused with %v, which is not queue.ErrNotLive; a caller "+
				"above the queue cannot tell a torn-down mailbox from a failed "+
				"teardown without branching on the backend", verb.name, verb.err)
		}
	}
}

// AND THE ADMIN ENDPOINT IS REALLY REACHABLE OTHERWISE, or the case above
// proves only that an offline queue cannot talk to a broker it never had.
//
// EnsureSubscription and DeleteSubscription run over a REST client that Stop
// does NOT close, which is exactly why they need a guard of their own: the one
// thing that stops them is the flag, and this is what says the flag is the
// only thing that stopped them.
func TestTheAdminVerbsWorkUntilTheQueueIsStopped(t *testing.T) {
	t.Parallel()
	q := offlineQueue(t)
	ctx := context.Background()
	if _, err := q.EnsureSubscription(ctx, "topic.t", "g"); err != nil {
		t.Fatalf("EnsureSubscription on a live queue: %v", err)
	}
	if _, err := q.DeleteSubscription(ctx, "topic.t", "g"); err != nil {
		t.Fatalf("DeleteSubscription on a live queue: %v", err)
	}
}
