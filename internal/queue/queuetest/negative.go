package queuetest

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/queue"
)

// runNegativePaths certifies that an operation which declines to act also
// declines to WRITE.
//
// A "no" has two halves, and this suite used to assert only the first. Every
// case here was found by asking what a plausible wrong backend gets away with,
// then confirming it got away with it: each of the four mutations below
// answered correctly and mutated anyway, and every one passed the whole suite
// before these cases existed.
//
//   - EnsureSubscription on an existing subscription wiped its retained mail.
//     The boot path declares every seat's inbox, so this drops the mail of
//     every unowned seat on every restart — the property seat ownership rests
//     on — while reporting a clean boot.
//   - DeleteSubscription keyed by topic instead of by the (topic, group) pair
//     destroyed a neighbouring group's subscription on the same subject.
//     Decommissioning one role silently decommissions its neighbours; the same
//     keyed-by-topic mistake that the pause holds already had to be fixed for.
//   - A deferral spent the message's dead-letter budget. The contract says
//     precisely why it must not — a NAK "would spend dead-letter budget on a
//     message nothing is wrong with, and a healthy event eventually dies after
//     enough handoffs" — and nothing tested it, so a seat that changes hands
//     often would lose healthy work with no failure anywhere.
//   - Quiesce that reports "no attachment" set the quiesce flag anyway,
//     leaving a (topic, group) nothing can be delivered on.
//
// The snapshot helper compares everything the suite can observe about a
// subscription, so a new observable is covered here by default rather than
// needing a new case.
//
// BEFORE ADDING A "MUST NOT" CASE HERE, ESTABLISH THAT THE CONTRACT FORBIDS
// THE OPERATION — not merely that no backend here does it. A documented
// degradation is a permitted exception, and a case that forbids one makes a
// correct backend look broken; the author who investigates pays far more than
// the check costs. This suite has made that mistake twice: it required a free
// deferral, which JetStream trades away (a deferred message costs a
// redelivery there, measured), and it required head-replay on nak, which only
// the twin does. Both are [Capabilities] flags now, not requirements.
//
// The four cases below were checked that way rather than by waiting for a
// failure. The contract defines all four attachment verbs and permits none of
// these writes; the only nearby exceptions are the deferral cost (gated as
// FreeDeferral) and Unquiesce not needing to reclaim a prefetch on a pull
// backend, which nothing here asserts either way. Delete-and-recreate resets
// the cursor and so is unusable as a reattach, which is the same conclusion
// case A reaches from the other direction.
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
		if backlog := s.caps.Backlog; backlog != nil {
			if got := backlog(q, topic, group); len(got) != 0 {
				t.Errorf("a refused publish left %d events in the mailbox", len(got))
			}
		}
	})

	t.Run("ensure_subscription_on_an_existing_one_keeps_its_mail", func(t *testing.T) {
		t.Parallel()
		backlog := s.needBacklog(t)
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
		before := s.snapshot(q, topic, group)

		// Declaring a seat's inbox at boot must be a no-op when it is
		// already there. "Ensure" that re-creates is indistinguishable
		// from "ensure" that does nothing — until a node restarts while a
		// seat is unowned.
		created, err := q.EnsureSubscription(ctx, topic, group)
		if err != nil || created {
			t.Fatalf("EnsureSubscription on an existing one = (%v, %v), want (false, nil)", created, err)
		}
		s.assertUntouched(t, q, topic, group, before, "re-declaring an existing subscription")
	})

	t.Run("delete_subscription_leaves_a_neighbouring_group_untouched", func(t *testing.T) {
		t.Parallel()
		backlog := s.needBacklog(t)
		q := s.start(ctx, t)
		const topic = "crewlet.events.task_created"
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
		before := s.snapshot(q, topic, neighbour)

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
		// accumulate undeliverable events for ever. A gate arriving after the
		// decommission — a sandbox hold or a config shed racing it — must not
		// undo that.
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

		if err := q.PauseTopic(ctx, topic, group, "sandbox"); err != nil {
			// Refusing a hold on a pair it does not know is a fine answer.
			t.Skipf("backend refuses a hold on an unknown pair: %v", err)
		}
		publish(ctx, t, q, topic, newEvent("after-decommission"))

		if got := backlog(q, topic, group); len(got) != 0 {
			t.Fatalf("a hold resurrected a deleted subscription; it retained %v", labelsOf(got))
		}
	})

	t.Run("a_deferral_spends_no_dead_letter_budget", func(t *testing.T) {
		t.Parallel()
		// A seat whose lease moved is not a failed handler. If a deferral
		// charged the message, an event handed between nodes often enough
		// would be dead-lettered while nothing was ever wrong with it —
		// and a busy seat changes hands often.
		//
		// NOT universal, and the skip above says why: a backend whose
		// deferral is a nak spends a count on every handoff and absorbs it
		// with a larger budget instead. What every backend owes is that a
		// healthy event does not die from being handed over; this case
		// certifies the stronger form, for the backends that offer it.
		//
		// Observed through what budget remains: after one deferral the
		// event must still have its FULL retry budget, so a two-attempt
		// queue still gives two failing deliveries before dead-lettering.
		// A deferral that charged one would leave only one.
		if !s.caps.FreeDeferral {
			t.Skip("backend implements deferral with a nak, which costs " +
				"one delivery count by design; see the FreeDeferral " +
				"capability")
		}
		newQueueWithAttempts := s.needAttempts(t)
		deadLetters := s.needDeadLetters(t)
		q := startQueue(ctx, t, newQueueWithAttempts(t, 2))

		naks := newJournal()
		var deferred bool
		subscribe(ctx, t, q, "topic.budget", "grp", func(context.Context, *events.Event) queue.Result {
			if !deferred {
				deferred = true
				return queue.Defer("lease moved")
			}
			naks.record("nak")
			return queue.Nak(errors.New("still failing"))
		})

		publish(ctx, t, q, "topic.budget", newEvent("e0"))
		// Wait for the QUIESCE, not for the backlog.
		//
		// The backlog is 1 from the moment the publish is acked — before the
		// broker has dispatched anything and so before the handler has run,
		// let alone deferred. On an asynchronous backend this wait returns at
		// once, the Unquiesce below finds nothing quiesced and does nothing,
		// the deferral lands a millisecond later, and the attachment is
		// quiesced with nothing left to resume it. Measured on a broker-backed
		// backend at 3 of 12 full-suite runs, and 16 of 20 run alone.
		//
		// It could not fail on the backend it was written against: the twin
		// declares InlineDispatch, so Publish drains before returning and the
		// window does not exist. Exactly the trap this suite's own doc warns
		// about, committed in the group whose header claims its cases were
		// checked against a plausible wrong backend — they were checked
		// against a wrong backend, never against an asynchronous one.
		quiescing := s.needQuiescing(t)
		awaitState(t, "the deferral to quiesce the attachment", func() bool {
			return quiescing(q, "topic.budget", "grp")
		})
		if _, err := q.Unquiesce(ctx, "topic.budget", "grp"); err != nil {
			t.Fatalf("Unquiesce: %v", err)
		}

		naks.await(t, "the full retry budget to survive the deferral",
			func(seen []string) bool { return len(seen) == 2 })
		naks.staysAt(t, 2, "the event outlived its budget")
		awaitState(t, "the event to dead-letter only after its own budget",
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
		before := s.snapshot(q, topic, group)

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
// Deliberately a snapshot of every observable rather than a list of fields each
// case names: an operation that declines to act declines to touch ANY of it, so
// the next observable a backend learns to report is covered here without a new
// assertion being written for it.
//
// Its honesty has one condition, and it has already failed once: this covers
// every observable only as long as a new Capabilities field that reports
// subscription state is also added HERE. Quiescing was missing at first — an
// observable the backend already exported — and the case meant to catch a stale
// quiesce passed against a backend setting one. A helper that silently stops
// generalising is exactly the failure it exists to prevent, so treat this
// struct as part of the Capabilities definition, not as a local convenience.
type subscriptionState struct {
	backlog     []string
	deadLetters []string
	pauseHolds  []string
	attached    bool
	quiescing   bool
	known       bool
}

func (s *suite) snapshot(q queue.EventQueue, topic, group string) subscriptionState {
	var out subscriptionState
	if s.caps.Backlog != nil {
		out.backlog = labelsOf(s.caps.Backlog(q, topic, group))
		out.known = true
	}
	if s.caps.DeadLetters != nil {
		out.deadLetters = labelsOf(s.caps.DeadLetters(q, topic, group))
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
	after := s.snapshot(q, topic, group)
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
