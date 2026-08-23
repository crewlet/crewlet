package queuetest

import (
	"context"
	"errors"
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
// BEFORE ADDING A "MUST NOT" CASE HERE, GREP rewrite/decisions/ FOR THE
// OPERATION. A recorded degradation is a permitted exception, and the corpus is
// where permission lives — a case that forbids what a decision allows makes a
// correct backend look broken, and the author who investigates pays far more
// than the grep cost. This suite has already made that mistake twice: it
// required a free deferral, which d-102 decisions 1-2 explicitly trade away on
// JetStream, and it required head-replay on nak, which d-102 measures as
// Pulsar-only. Both are capabilities now, not requirements.
//
// The four cases below were checked that way rather than by waiting for a
// failure. d-101 §3 defines all four verbs and permits none of these writes;
// d-102's only nearby exceptions are the deferral cost (gated as FreeDeferral)
// and Unquiesce not needing to reclaim a prefetch on a pull backend, which
// nothing here asserts either way. d-102 also records that delete-and-recreate
// "resets the cursor, so unusable", which is the same conclusion case A reaches
// from the other direction.
func (s *suite) runNegativePaths(t *testing.T) {
	ctx := context.Background()

	t.Run("ensure_subscription_on_an_existing_one_keeps_its_mail", func(t *testing.T) {
		t.Parallel()
		backlog := s.needBacklog(t)
		q := s.start(t)
		const topic, group = "crewlet.agent.bob.inbox", "agent-bob"

		if _, err := q.EnsureSubscription(ctx, topic, group); err != nil {
			t.Fatalf("EnsureSubscription: %v", err)
		}
		publish(t, q, topic, newEvent("e0"))
		publish(t, q, topic, newEvent("e1"))
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
		q := s.start(t)
		const topic = "crewlet.events.task_created"
		const doomed, neighbour = "doomed-grp", "neighbour-grp"

		for _, group := range []string{doomed, neighbour} {
			if _, err := q.EnsureSubscription(ctx, topic, group); err != nil {
				t.Fatalf("EnsureSubscription(%s): %v", group, err)
			}
		}
		publish(t, q, topic, newEvent("e0"))
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
			t.Skip("backend implements deferral with a nak, which costs one " +
				"delivery count by design (rewrite/decisions/102, decisions 1-2)")
		}
		newQueueWithAttempts := s.needAttempts(t)
		deadLetters := s.needDeadLetters(t)
		q := startQueue(t, newQueueWithAttempts(t, 2))

		naks := newJournal()
		var deferred bool
		subscribe(t, q, "topic.budget", "grp", func(context.Context, *events.Event) queue.Result {
			if !deferred {
				deferred = true
				return queue.Defer("lease moved")
			}
			naks.record("nak")
			return queue.Nak(errors.New("still failing"))
		})

		publish(t, q, "topic.budget", newEvent("e0"))
		awaitState(t, "the deferral to quiesce the attachment", func() bool {
			return len(backlog(s, q, "topic.budget", "grp")) == 1
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
		q := s.start(t)
		const topic, group = "seat.unowned", "grp"

		if _, err := q.EnsureSubscription(ctx, topic, group); err != nil {
			t.Fatalf("EnsureSubscription: %v", err)
		}
		publish(t, q, topic, newEvent("e0"))
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
		subscribe(t, q, topic, group, recordingHandler(j))
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

// backlog reads a subscription's retained mail through the capability, for the
// cases that have already established it is available.
func backlog(s *suite, q queue.EventQueue, topic, group string) []*events.Event {
	if s.caps.Backlog == nil {
		return nil
	}
	return s.caps.Backlog(q, topic, group)
}
