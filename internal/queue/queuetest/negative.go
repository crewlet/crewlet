package queuetest

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/queue"
)

// runNegativePaths certifies that an operation which declines to act also
// declines to WRITE. Each case answered correctly and mutated anyway:
// EnsureSubscription wiping an existing subscription's retained mail (every
// unowned seat's, on every restart), DeleteSubscription keyed by topic
// destroying a neighbouring group's, Quiesce reporting "no attachment" yet
// setting the flag, a deferral never spending the dead-letter budget.
//
// BEFORE ADDING A "MUST NOT" CASE, ESTABLISH THAT THE CONTRACT FORBIDS THE
// OPERATION: a documented degradation is permitted (a free deferral, which
// JetStream trades away; head-replay on nak, see queue.OutcomeNak). A
// [Capabilities] flag is only for what the contract leaves open AND nothing
// above the queue is tested against.
func (s *suite) runNegativePaths(t *testing.T) {
	ctx := t.Context()

	t.Run("an_oversized_publish_is_refused_as_permanently_too_large", func(t *testing.T) {
		t.Parallel()
		// "Too large" is the one publish failure that never becomes
		// true on a retry, and a producer can only act on that if every
		// backend says it the same way — the layers above are forbidden
		// to ask which backend is running. So the sentinel is the
		// contract's, and both backends translate their own refusal
		// into it.
		//
		// Without this the webhook edge treated an oversized delivery
		// as a broker outage and asked the provider to retry something
		// that could not succeed, forever.
		q := s.start(ctx, t)
		const topic, group = "crewlet.agent.big.inbox", "agent-big"
		if _, err := q.EnsureSubscription(ctx, topic, group); err != nil {
			t.Fatalf("EnsureSubscription: %v", err)
		}

		ev := newEvent("oversized")
		// Extra rather than Data: this suite publishes no typed
		// payloads, and the bytes are the whole subject here. The key
		// must not be an envelope field — those are reserved and
		// silently dropped, which produced a 214-byte "oversized"
		// event the first time this was written.
		ev.Extra = map[string]json.RawMessage{
			"blob": json.RawMessage(`"` + strings.Repeat("x", queue.MaxPayloadBytes) + `"`),
		}

		err := q.Publish(ctx, topic, ev)
		if err == nil {
			t.Fatal("an event over the payload ceiling published successfully")
		}
		if !errors.Is(err, queue.ErrTooLarge) {
			t.Errorf("Publish refused an oversized event with %v, which does not "+
				"match queue.ErrTooLarge: a producer cannot tell it apart from a "+
				"broker that is merely down, so it retries forever", err)
		}
		// And it must not have half-landed: nothing is retained for a
		// delivery the transport refused.
		if backlog := s.optionalBacklog(t); backlog != nil {
			if got := backlog(q, topic, group); len(got) != 0 {
				t.Errorf("a refused publish left %d events in the mailbox", len(got))
			}
		}
	})

	t.Run("a_nil_event_is_refused_and_reaches_nobody", func(t *testing.T) {
		t.Parallel()
		// THE CONTRACT REFUSES IT, and the backends did not agree. The twin
		// refused a nil event; the broker-backed backend encoded it — as
		// `null` — stored it, and delivered an event with no id and no type
		// to every consumer on the topic, where it reached the completion
		// ledger and the dispatcher as a delivery of nothing. Neither half
		// of that is observable from the caller's side, which is why the
		// write that must not happen is asserted beside the answer.
		q := s.start(ctx, t)
		const topic, group = "crewlet.agent.void.inbox", "agent-void"
		if _, err := q.EnsureSubscription(ctx, topic, group); err != nil {
			t.Fatalf("EnsureSubscription: %v", err)
		}
		listened := newJournal()
		q.AddPublishListener(func(_ context.Context, topic string, ev *events.Event) {
			listened.record(topic + "/" + labelOf(ev))
		})

		if err := q.Publish(ctx, topic, nil); err == nil {
			t.Fatal("a nil event published successfully")
		}
		// READ AT ONCE, with nothing to wait for: a publish listener runs on
		// the publishing goroutine (queue.PublishListener), so a refused
		// Publish that reached one had called it before it returned.
		//
		// Mutation: hand the listeners the nil event before refusing it.
		if got := listened.all(); len(got) != 0 {
			t.Fatalf("a refused publish reached a publish listener: %v", got)
		}
		if backlog := s.optionalBacklog(t); backlog != nil {
			if got := backlog(q, topic, group); len(got) != 0 {
				t.Errorf("a refused nil publish left %d events in the mailbox", len(got))
			}
		}
	})

	t.Run("ensure_subscription_on_an_existing_one_keeps_its_mail_and_proposes_nothing", func(t *testing.T) {
		t.Parallel()
		backlog := s.needBacklog(t)
		proposals := s.optionalProposals(t)
		q := s.start(ctx, t)
		const topic, group = "crewlet.agent.bob.inbox", "agent-bob"

		if _, err := q.EnsureSubscription(ctx, topic, group); err != nil {
			t.Fatalf("EnsureSubscription: %v", err)
		}
		publish(ctx, t, q, topic, newEvent("e0"))
		publish(ctx, t, q, topic, newEvent("e1"))
		awaitState(t, "the mail to be retained", func() bool {
			return len(backlog(q, topic, group)) == 2
		})
		before := s.snapshot(t, q, topic, group)
		var proposedBefore int
		if proposals != nil {
			proposedBefore = proposals(q)
		}

		// Declaring a seat's inbox at boot must be a no-op when it is
		// already there. "Ensure" that re-creates is indistinguishable
		// from "ensure" that does nothing — until a node restarts while a
		// seat is unowned.
		created, err := q.EnsureSubscription(ctx, topic, group)
		if err != nil || created {
			t.Fatalf("EnsureSubscription on an existing one = (%v, %v), want (false, nil)", created, err)
		}
		s.assertUntouched(t, q, topic, group, before, "re-declaring an existing subscription")

		// AND IT MUST NOT HAVE ASKED. One case, one decision: a backend gets
		// both halves by looking the consumer up and returning, neither by
		// sending the create and letting the broker sort it out. Split, the
		// cheap half reads as an optimisation to undo for safety, which
		// costs a replicated write per seat on every boot and config apply.
		//
		// A POSITIVE lookup only. A backend that could not tell whether the
		// consumer was there must still propose the create, since skipping
		// it on an unanswered probe silently drops a seat's mail. That arm
		// is stated, not certified (nothing here makes a healthy broker
		// decline to answer), so the count is asserted only for a
		// subscription the previous call proved exists.
		if proposals != nil {
			if spent := proposals(q) - proposedBefore; spent != 0 {
				t.Errorf("re-declaring an existing subscription asked the broker for %d "+
					"consumer create(s), want 0: on a replicated broker each one is a "+
					"proposal through the metadata group, so a company's every mailbox "+
					"is re-proposed on every boot and every config apply", spent)
			}
		}
	})

	t.Run("delete_subscription_leaves_a_neighbouring_group_untouched", func(t *testing.T) {
		t.Parallel()
		backlog := s.needBacklog(t)
		q := s.start(ctx, t)
		const topic = "crewlet.events.agent_phase_started"
		const doomed, neighbour = "doomed-grp", "neighbour-grp"

		for _, group := range []string{doomed, neighbour} {
			if _, err := q.EnsureSubscription(ctx, topic, group); err != nil {
				t.Fatalf("EnsureSubscription(%s): %v", group, err)
			}
		}
		publish(ctx, t, q, topic, newEvent("e0"))
		awaitState(t, "both groups to retain a copy", func() bool {
			return len(backlog(q, topic, doomed)) == 1 && len(backlog(q, topic, neighbour)) == 1
		})
		before := s.snapshot(t, q, topic, neighbour)

		deleted, err := q.DeleteSubscription(ctx, topic, doomed)
		if err != nil || !deleted {
			t.Fatalf("DeleteSubscription = (%v, %v), want (true, nil)", deleted, err)
		}
		// Groups share a subject; only the named pair may be destroyed.
		s.assertUntouched(t, q, topic, neighbour, before, "deleting a sibling group's subscription")

		// And deleting one that is already gone must not reach the
		// neighbour either.
		if deleted, err = q.DeleteSubscription(ctx, topic, doomed); err != nil || deleted {
			t.Fatalf("second DeleteSubscription = (%v, %v), want (false, nil)", deleted, err)
		}
		s.assertUntouched(t, q, topic, neighbour, before, "deleting an absent subscription")
	})

	t.Run("a_hold_does_not_resurrect_a_deleted_subscription", func(t *testing.T) {
		t.Parallel()
		// DeleteSubscription exists so a decommissioned role's inbox cannot
		// accumulate undeliverable events for ever. A hold arriving after
		// the decommission, racing it, must not undo that.
		//
		// Found by building the full verb matrix at the DELETED lifecycle
		// point rather than probing the verb that looked suspicious: six verbs
		// left the subscription deleted and one recreated it, which is not the
		// one that would have been guessed. Measured on the twin, where
		// PauseTopic minted the subscription and every event published to that
		// topic was then retained for a role that no longer existed.
		backlog := s.needBacklog(t)
		q := s.start(ctx, t)
		const topic, group = "seat.decommissioned", "grp"

		if _, err := q.EnsureSubscription(ctx, topic, group); err != nil {
			t.Fatalf("EnsureSubscription: %v", err)
		}
		if deleted, err := q.DeleteSubscription(ctx, topic, group); err != nil || !deleted {
			t.Fatalf("DeleteSubscription = (%v, %v), want (true, nil)", deleted, err)
		}

		if err := q.PauseTopic(ctx, topic, group, attachmentOf(ctx, t, q, topic, group), holdReason); err != nil {
			// Refusing a hold on a pair it does not know is a fine answer.
			t.Skipf("backend refuses a hold on an unknown pair: %v", err)
		}
		publish(ctx, t, q, topic, newEvent("after-decommission"))

		if got := backlog(q, topic, group); len(got) != 0 {
			t.Fatalf("a hold resurrected a deleted subscription; it retained %v", labelsOf(got))
		}
	})

	t.Run("a_deferral_spends_one_delivery_like_every_other_hand_back", func(t *testing.T) {
		t.Parallel()
		// A DEFERRAL COSTS WHAT A NAK COSTS, on every backend. No broker here
		// has a "give this back without counting it": a prompt return is a
		// Nak, which spends a delivery. So the budget is
		// sized for handoffs as well as failures (25, not the 10 a
		// free-handoff broker would need), keeping a healthy event alive
		// across seat movements. A capability only the twin declared would
		// run the case against the twin ALONE and split one contract in two
		// (queue.DeliveriesLeft says every return spends one).
		//
		// OBSERVED ON THE CONTRACT'S OWN NUMBER, the one a caller acts on: a
		// three-attempt queue must count 2, 1, 0 across a deferral and two
		// failures as across three failures. A free deferral reports 2 twice.
		newQueueWithAttempts := s.needAttempts(t)
		deadLetters := s.needDeadLetters(t)
		q := s.startQueue(ctx, t, newQueueWithAttempts(t, 3))

		headroom := newJournal()
		var attempts int
		subscribe(ctx, t, q, "topic.budget", "grp",
			func(hctx context.Context, _ *events.Event) queue.Result {
				attempts++
				left, known := queue.DeliveriesLeft(hctx)
				if !known {
					headroom.record("unknown")
					return queue.Ack()
				}
				headroom.record(strconv.Itoa(left))
				if attempts == 1 {
					return queue.Defer(queue.DeferSeatMoved, "lease moved")
				}
				return queue.Nak(errors.New("still failing"))
			})

		publish(ctx, t, q, "topic.budget", newEvent("e0"))
		// Wait for the QUIESCE, not for the backlog. The backlog is 1 from the
		// moment the publish is acked, before the handler has run, let alone
		// deferred. On an asynchronous backend that wait returns at once, the
		// Unquiesce below finds nothing quiesced, the deferral lands a
		// millisecond later, and the attachment is quiesced with nothing left
		// to resume it. The twin cannot show this: InlineDispatch drains
		// Publish before returning, so the window does not exist there.
		quiescing := s.needQuiescing(t)
		awaitState(t, "the deferral to quiesce the attachment", func() bool {
			return quiescing(q, "topic.budget", "grp")
		})
		if _, err := q.Unquiesce(ctx, "topic.budget", "grp"); err != nil {
			t.Fatalf("Unquiesce: %v", err)
		}

		headroom.awaitLabels(t, "the headroom to count down across the deferral "+
			"exactly as it would across a failure", "2", "1", "0")
		headroom.staysAt(ctx, t, 3, "the event outlived its budget")
		// AND THE BOUNDARY IS THE SAME ONE. The delivery that reported no
		// headroom left is the last: the hand-back after it dead-letters,
		// with a copy on the dead-letter subject rather than a healthy
		// event discarded by a broker backstop in silence.
		awaitState(t, "the event to dead-letter after the delivery with no headroom left",
			func() bool { return len(deadLetters(q, "topic.budget", "grp")) == 1 })
	})

	t.Run("quiesce_with_no_attachment_changes_nothing", func(t *testing.T) {
		t.Parallel()
		q := s.start(ctx, t)
		const topic, group = "seat.unowned", "grp"

		if _, err := q.EnsureSubscription(ctx, topic, group); err != nil {
			t.Fatalf("EnsureSubscription: %v", err)
		}
		publish(ctx, t, q, topic, newEvent("e0"))
		before := s.snapshot(t, q, topic, group)

		quiesced, err := q.Quiesce(ctx, topic, group)
		if err != nil || quiesced {
			t.Fatalf("Quiesce with nothing attached = (%v, %v), want (false, nil)", quiesced, err)
		}
		s.assertUntouched(t, q, topic, group, before, "quiescing an unattached subscription")

		// The flag it must not have set is invisible until someone
		// attaches: a stale quiesce leaves a seat that is owned, attached
		// and permanently silent.
		j := newJournal()
		subscribe(ctx, t, q, topic, group, recordingHandler(j))
		j.awaitLabels(t, "the retained mail to reach a new consumer", "e0")
	})
}

// subscriptionState is everything the suite can observe about one
// (topic, group). A negative path must leave all of it exactly as it found it.
//
// A snapshot of every observable rather than a list of fields each case
// names: a declined operation touches ANY of it, so a new observable is
// covered without a new assertion. That holds only while a new Capabilities
// field reporting subscription state is also added HERE (quiescing was once
// missing, and the case meant to catch a stale quiesce passed against a
// backend setting one): treat this struct as part of the Capabilities
// definition.
type subscriptionState struct {
	backlog     []string
	deadLetters []string
	pauseHolds  []string
	attached    bool
	quiescing   bool
	known       bool
}

func (s *suite) snapshot(t *testing.T, q queue.EventQueue, topic, group string) subscriptionState {
	t.Helper()
	var out subscriptionState
	if s.caps.Backlog != nil {
		out.backlog = labelsOf(s.caps.Backlog(t, q, topic, group))
		out.known = true
	}
	if s.caps.DeadLetters != nil {
		out.deadLetters = labelsOf(s.caps.DeadLetters(t, q, topic, group))
		out.known = true
	}
	if s.caps.PauseHolds != nil {
		out.pauseHolds = s.caps.PauseHolds(q, topic, group)
		out.known = true
	}
	if s.caps.Quiescing != nil {
		out.quiescing = s.caps.Quiescing(q, topic, group)
		out.known = true
	}
	if s.caps.Attachments != nil {
		for _, pair := range s.caps.Attachments(q) {
			if pair[0] == topic && pair[1] == group {
				out.attached = true
			}
		}
		out.known = true
	}
	return out
}

// assertUntouched compares state against a snapshot taken before an operation
// that should not have written.
//
// It reads with NO wait, because an absence has no signal to wait on — which
// makes it depend on the read-your-own-write requirement stated on
// Capabilities: if a backend's inspection lags its own completed operation, a
// real write is invisible here and this passes having checked nothing.
func (s *suite) assertUntouched(t *testing.T, q queue.EventQueue, topic, group string, before subscriptionState, what string) {
	t.Helper()
	if !before.known {
		t.Skip("backend reports nothing about a subscription's state")
	}
	after := s.snapshot(t, q, topic, group)
	if !equalStrings(after.backlog, before.backlog) {
		t.Errorf("%s changed the retained mail of (%s, %s): %v -> %v",
			what, topic, group, before.backlog, after.backlog)
	}
	if !equalStrings(after.deadLetters, before.deadLetters) {
		t.Errorf("%s changed the dead letters of (%s, %s): %v -> %v",
			what, topic, group, before.deadLetters, after.deadLetters)
	}
	if !equalStrings(after.pauseHolds, before.pauseHolds) {
		t.Errorf("%s changed the pause holds of (%s, %s): %v -> %v",
			what, topic, group, before.pauseHolds, after.pauseHolds)
	}
	if after.quiescing != before.quiescing {
		t.Errorf("%s changed whether (%s, %s) is quiesced: %v -> %v",
			what, topic, group, before.quiescing, after.quiescing)
	}
	if after.attached != before.attached {
		t.Errorf("%s changed whether (%s, %s) is attached: %v -> %v",
			what, topic, group, before.attached, after.attached)
	}
}
