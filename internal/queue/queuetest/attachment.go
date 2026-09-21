package queuetest

import (
	"context"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/queue"
)

// runAttachment covers the four attachment verbs and the exact destructiveness
// of each. They are separate verbs precisely because they differ: quiesce keeps
// the seat and the mail, unquiesce is its reversible inverse, detach drops the
// consumer and keeps the mail, and only DeleteSubscription destroys anything.
func (s *suite) runAttachment(t *testing.T) {
	ctx := t.Context()

	t.Run("detach_stops_delivery_for_group", func(t *testing.T) {
		t.Parallel()
		q := s.start(ctx, t)
		mine, other := newJournal(), newJournal()
		subscribe(ctx, t, q, "topic.a", "grp", recordingHandler(mine))
		subscribe(ctx, t, q, "topic.a", "other-grp", recordingHandler(other))

		publish(ctx, t, q, "topic.a", newEvent("e1"))
		mine.awaitLabels(t, "the first delivery", "e1")
		other.awaitLabels(t, "the other group's copy", "e1")

		detached, err := q.Detach(ctx, "topic.a", "grp")
		if err != nil || !detached {
			t.Fatalf("Detach = (%v, %v), want (true, nil)", detached, err)
		}

		publish(ctx, t, q, "topic.a", newEvent("e2"))
		other.awaitLabels(t, "an untouched group to keep receiving", "e1", "e2")
		mine.staysAt(t, 1, "a detached pair kept receiving")

		if backlog := s.optionalBacklog(t); backlog != nil {
			// RETAINED for whoever attaches next — that is the whole
			// point of detach being non-destructive.
			awaitState(t, "e2 to be retained for the next attacher", func() bool {
				return equalStrings(labelsOf(backlog(q, "topic.a", "grp")), []string{"e2"})
			})
		}

		// Re-attaching replays it: a seat that comes back finds its mail.
		subscribe(ctx, t, q, "topic.a", "grp", recordingHandler(mine))
		mine.awaitLabels(t, "the retained mail to replay on re-attach", "e1", "e2")

		// Idempotent, and it reports whether an attachment existed.
		if detached, err = q.Detach(ctx, "topic.a", "grp"); err != nil || !detached {
			t.Fatalf("Detach of a live attachment = (%v, %v), want (true, nil)", detached, err)
		}
		if detached, err = q.Detach(ctx, "topic.a", "grp"); err != nil || detached {
			t.Fatalf("Detach of a dropped attachment = (%v, %v), want (false, nil)", detached, err)
		}
		if detached, err = q.Detach(ctx, "nope", "grp"); err != nil || detached {
			t.Fatalf("Detach of an unknown pair = (%v, %v), want (false, nil)", detached, err)
		}
	})

	t.Run("a_detach_inside_a_handler_stops_the_next_delivery", func(t *testing.T) {
		t.Parallel()
		// THE SINGLE-DELIVERY HALF of
		// Batch/a_detach_taken_mid_batch_stops_the_rest, and it is a
		// separate case because the two paths answer the same condition
		// through DIFFERENT mechanisms.
		//
		// The case above detaches between publishes, so a backend passes
		// it by checking anything at all at the point of delivery. This
		// one detaches from INSIDE a handler with the next event already
		// waiting — the arrangement the batch case exposed, where a
		// backend that had answered the question once went on to serve
		// the rest of what it had already drained. On the single path a
		// backend re-asks per event rather than holding a consumer across
		// a partition walk, so the question here is whether it re-asks at
		// all.
		//
		// WHY THAT IS NOT THE SAME CODE. On the batch path the twin reads
		// a flag on the consumer it is holding; on this one the consumer
		// is simply gone from the subscription's member list by the time
		// the next event is considered. Two mechanisms, one rule — so one
		// of them can rot while the other keeps passing, which is what a
		// case per path is for.
		//
		// WHAT THIS CASE DOES AND DOES NOT DISCRIMINATE, measured rather
		// than claimed, because the finding this was written alongside
		// was precisely a claim about coverage that nothing checked.
		// Each backend answers a detach on THIS path twice over — the
		// twin by the detached flag and by the member leaving
		// sub.members, JetStream by attachment.blocked() and by the
		// consume loop's context being cancelled — and removing either
		// half alone leaves this case GREEN on that backend. So it holds
		// the rule, not a mechanism: it goes red when a detach stops
		// stopping the next delivery, by whatever route. The mechanism
		// half is held by the batch case, which measurably fails when the
		// flag (twin) or blocked()'s detached term (JetStream) is
		// removed.
		q := s.start(ctx, t)
		release := holdBeforeAttach(ctx, t, q, "topic.detach1", "grp")

		seen := newJournal()
		var detached bool
		subscribe(ctx, t, q, "topic.detach1", "grp",
			func(hctx context.Context, ev *events.Event) queue.Result {
				seen.record(labelOf(ev))
				// ONCE: the seat is released on the first delivery, and
				// a second call would mean the property under test has
				// already failed — reporting it as a Detach error would
				// bury that under the wrong message.
				if !detached {
					detached = true
					if _, err := q.Detach(hctx, "topic.detach1", "grp"); err != nil {
						t.Errorf("Detach: %v", err)
					}
				}
				return queue.Ack()
			})

		// Both events are in the mailbox before anything can be
		// delivered, so the second one is already waiting when the first
		// one's handler gives the seat up. Publishing them without the
		// hold would let the first be delivered and acked before the
		// second was even accepted, and the case would assert nothing.
		//
		// THE HOLD IS TAKEN ABOVE, before the attachment exists, and that
		// is what makes the sentence above true rather than merely
		// intended: a hold taken after Subscribe cannot retract a fetch
		// already in flight, so e1 could be delivered DURING it — exactly
		// what this comment claims is impossible. See holdBeforeAttach.
		publish(ctx, t, q, "topic.detach1", newEvent("e1"))
		publish(ctx, t, q, "topic.detach1", newEvent("e2"))
		release()

		seen.awaitLabels(t, "only the first event to be handled", "e1")
		seen.staysAt(t, 1, "the detach did not stop the next delivery")

		if backlog := s.optionalBacklog(t); backlog != nil {
			awaitState(t, "the undelivered event to be retained", func() bool {
				return equalStrings(labelsOf(backlog(q, "topic.detach1", "grp")), []string{"e2"})
			})
		}
	})

	t.Run("detach_removes_batch_subscription", func(t *testing.T) {
		t.Parallel()
		q := s.start(ctx, t)
		batches := newBatchJournal()
		subscribeBatch(ctx, t, q, "topic.b", "grp", recordingBatchHandler(batches),
			queue.NewBatchOptions(0, 10))

		publish(ctx, t, q, "topic.b", newConvEvent("e1", "k"))
		batches.await(t, "the first batch", func(got [][]string) bool { return len(got) == 1 })

		if _, err := q.Detach(ctx, "topic.b", "grp"); err != nil {
			t.Fatalf("Detach: %v", err)
		}
		publish(ctx, t, q, "topic.b", newConvEvent("e2", "k"))
		batches.staysAt(t, 1, "a detached batch consumer kept receiving")

		if backlog := s.optionalBacklog(t); backlog != nil {
			awaitState(t, "e2 to be retained", func() bool {
				return equalStrings(labelsOf(backlog(q, "topic.b", "grp")), []string{"e2"})
			})
		}
	})

	t.Run("detach_releases_this_attachments_pause_holds", func(t *testing.T) {
		t.Parallel()
		// A hold is state about ONE attachment. One that outlived a
		// detach would leave a node that re-attached later silently deaf,
		// with nothing left to release it.
		q := s.start(ctx, t)
		j := newJournal()
		subscribe(ctx, t, q, "seat.inbox", "grp", recordingHandler(j))
		if err := q.PauseTopic(ctx, "seat.inbox", "grp", holdReason); err != nil {
			t.Fatalf("PauseTopic: %v", err)
		}
		if _, err := q.Detach(ctx, "seat.inbox", "grp"); err != nil {
			t.Fatalf("Detach: %v", err)
		}
		if holds := s.caps.PauseHolds; holds != nil {
			if got := holds(q, "seat.inbox", "grp"); len(got) != 0 {
				t.Fatalf("a pause hold survived the detach: %v", got)
			}
		}

		subscribe(ctx, t, q, "seat.inbox", "grp", recordingHandler(j))
		publish(ctx, t, q, "seat.inbox", newEvent("after-reattach"))
		j.awaitLabels(t, "a re-attached seat to receive again", "after-reattach")
	})

	t.Run("quiesce_reports_whether_an_attachment_existed", func(t *testing.T) {
		t.Parallel()
		q := s.start(ctx, t)
		if quiesced, err := q.Quiesce(ctx, "seat.inbox", "grp"); err != nil || quiesced {
			t.Fatalf("Quiesce with nothing attached = (%v, %v), want (false, nil)", quiesced, err)
		}
		subscribe(ctx, t, q, "seat.inbox", "grp", recordingHandler(newJournal()))
		for range 2 {
			quiesced, err := q.Quiesce(ctx, "seat.inbox", "grp")
			if err != nil || !quiesced {
				t.Fatalf("Quiesce = (%v, %v), want (true, nil)", quiesced, err)
			}
		}
	})

	t.Run("unquiesce_resumes_and_delivers_what_was_held", func(t *testing.T) {
		t.Parallel()
		// Quiesce is not always followed by a detach. A node whose lease
		// store blipped keeps its seats and its consumers; only the proof
		// of ownership lapsed. Without an inverse it would come back
		// healthy and never read from them again — owned, attached, and
		// permanently deaf.
		q := s.start(ctx, t)
		j := newJournal()
		subscribe(ctx, t, q, "seat.inbox", "grp", recordingHandler(j))
		if quiesced, err := q.Quiesce(ctx, "seat.inbox", "grp"); err != nil || !quiesced {
			t.Fatalf("Quiesce = (%v, %v), want (true, nil)", quiesced, err)
		}

		publish(ctx, t, q, "seat.inbox", newEvent("held"))
		j.staysAt(t, 0, "a quiesced attachment took new work")
		if backlog := s.optionalBacklog(t); backlog != nil {
			awaitState(t, "the held event to stay in the mailbox", func() bool {
				return equalStrings(labelsOf(backlog(q, "seat.inbox", "grp")), []string{"held"})
			})
		}

		resumed, err := q.Unquiesce(ctx, "seat.inbox", "grp")
		if err != nil || !resumed {
			t.Fatalf("Unquiesce = (%v, %v), want (true, nil)", resumed, err)
		}
		j.awaitLabels(t, "what the quiesce held back", "held")

		// It reports whether it WAS quiesced, so a caller can tell a
		// real resume from a no-op.
		if resumed, err = q.Unquiesce(ctx, "seat.inbox", "grp"); err != nil || resumed {
			t.Fatalf("second Unquiesce = (%v, %v), want (false, nil)", resumed, err)
		}
	})

	t.Run("unquiesce_leaves_pause_holds_alone", func(t *testing.T) {
		t.Parallel()
		// A seat resuming from a stale-renew window may still be
		// legitimately held (on a node with no turn engine, by the park's
		// own pause); lifting that would restart the requeue loop the hold
		// exists to stop.
		q := s.start(ctx, t)
		j := newJournal()
		subscribe(ctx, t, q, "seat.inbox", "grp", recordingHandler(j))
		if err := q.PauseTopic(ctx, "seat.inbox", "grp", holdReason); err != nil {
			t.Fatalf("PauseTopic: %v", err)
		}
		if _, err := q.Quiesce(ctx, "seat.inbox", "grp"); err != nil {
			t.Fatalf("Quiesce: %v", err)
		}

		publish(ctx, t, q, "seat.inbox", newEvent("held"))
		if _, err := q.Unquiesce(ctx, "seat.inbox", "grp"); err != nil {
			t.Fatalf("Unquiesce: %v", err)
		}

		if holds := s.caps.PauseHolds; holds != nil {
			if got := holds(q, "seat.inbox", "grp"); !equalStrings(got, []string{holdReason}) {
				t.Fatalf("pause holds after Unquiesce = %v, want [%s]", got, holdReason)
			}
		}
		j.staysAt(t, 0, "Unquiesce lifted a pause hold it does not own")
	})

	t.Run("re_attaching_clears_a_stale_quiesce", func(t *testing.T) {
		t.Parallel()
		// A quiesce that outlives its consumer strands the seat forever.
		// Detach clears the flag — but an in-flight handler abandoned by
		// that detach can still defer afterwards, and applying the
		// deferral puts the key straight back. From there nothing is
		// deliverable on that (topic, group) and there is no consumer
		// left to un-quiesce it, so the seat's next owner attaches to a
		// subscription that never hands it anything.
		backlog := s.needBacklog(t)
		q := s.start(ctx, t)

		entered := make(chan struct{})
		release := make(chan struct{})
		subscribe(ctx, t, q, "topic.q", "grp", func(context.Context, *events.Event) queue.Result {
			close(entered)
			<-release
			return queue.Defer("lease moved while this handler was running")
		})

		published := make(chan struct{})
		go func() {
			defer close(published)
			_ = q.Publish(ctx, "topic.q", newEvent("e0"))
		}()
		awaitSignal(t, entered, "the handler to start", func() {})

		// The fenced release: detach first, then let the abandoned handler
		// land its deferral.
		//
		// Detach must NOT wait for the in-flight handler. The two verbs
		// differ on exactly this point — a quiesce lets a running handler
		// finish, a fenced detach abandons it — and a Detach that joined
		// its dispatcher would block a node that has LOST its lease behind
		// a handler that may run for minutes, which is the one moment it
		// must not wait. It also deadlocks outright here, because the
		// handler is released only once Detach returns.
		detached := make(chan error, 1)
		go func() {
			_, err := q.Detach(ctx, "topic.q", "grp")
			detached <- err
		}()
		select {
		case err := <-detached:
			if err != nil {
				close(release)
				t.Fatalf("Detach: %v", err)
			}
		case <-time.After(settleFor):
			// Release the handler so the process can still exit: a
			// conformance suite that hangs a backend's test binary costs
			// its author ten minutes and a goroutine dump to learn what
			// one line could have told them.
			close(release)
			t.Fatalf("Detach blocked on an in-flight handler for %s.\n"+
				"A fenced detach abandons a running handler; only Quiesce waits for one.", settleFor)
		}
		close(release)
		awaitSignal(t, published, "the deferral to be applied", func() {})
		awaitState(t, "the deferred event to return to the mailbox", func() bool {
			return equalStrings(labelsOf(backlog(q, "topic.q", "grp")), []string{"e0"})
		})

		j := newJournal()
		subscribe(ctx, t, q, "topic.q", "grp", recordingHandler(j))
		publish(ctx, t, q, "topic.q", newEvent("e1"))
		// IN ANY ORDER. The subject here is that the re-attached seat is
		// deliverable at all — both events arrive — and the deferred one
		// is a REDELIVERY, which [Caps.HeadReplayOnNak] says the backends
		// answer differently: the twin replays from the head, JetStream
		// returns it behind never-delivered events. Asserting the
		// sequence made this pass on an idle machine and fail under load
		// on a backend that had already declared the behaviour, which
		// reads as a broker bug and is not one. Head-replay order is
		// certified where it belongs, by
		// nak_returns_the_event_to_the_front_of_the_mailbox, gated on the
		// capability.
		j.awaitLabelsInAnyOrder(t, "a new owner to receive on a re-attached seat",
			"e0", "e1")
	})
}

// runFleet asserts the seam between what a fleet SHARES — subscriptions and
// the mail in them — and what belongs to one node: its attachments, its pause
// holds, its quiesce flags and its drain state. For a single process the
// conflation is invisible; for two it inverts the property seat ownership
// rests on.
func (s *suite) runFleet(t *testing.T) {
	ctx := t.Context()

	t.Run("a_clients_detach_leaves_its_peers_attached", func(t *testing.T) {
		t.Parallel()
		peer := s.needPeer(t)
		if s.caps.Attachments == nil {
			t.Skip("backend cannot report which pairs a client is attached to")
		}
		a := s.start(ctx, t)
		b := startQueue(ctx, t, peer(t, a))

		gotA, gotB := newJournal(), newJournal()
		subscribe(ctx, t, a, "seat.inbox", "grp", recordingHandler(gotA))
		subscribe(ctx, t, b, "seat.inbox", "grp", recordingHandler(gotB))
		assertAttached(t, s.caps.Attachments(a), "seat.inbox", "grp")
		assertAttached(t, s.caps.Attachments(b), "seat.inbox", "grp")

		if _, err := a.Detach(ctx, "seat.inbox", "grp"); err != nil {
			t.Fatalf("Detach: %v", err)
		}
		if got := s.caps.Attachments(a); len(got) != 0 {
			t.Fatalf("the detaching client is still attached to %v", got)
		}
		assertAttached(t, s.caps.Attachments(b), "seat.inbox", "grp")

		publish(ctx, t, a, "seat.inbox", newEvent("after"))
		gotB.awaitLabels(t, "the surviving peer to keep serving the seat", "after")
		gotA.staysAt(t, 0, "the detached client kept receiving")
	})

	t.Run("a_clients_pause_does_not_gate_its_peers", func(t *testing.T) {
		t.Parallel()
		// A hold describes ONE node's attachment. Gating the subscription
		// instead would let one node's pause, or one node's shutdown, stop a
		// peer from serving the seat it owns.
		peer := s.needPeer(t)
		a := s.start(ctx, t)
		b := startQueue(ctx, t, peer(t, a))

		gotB := newJournal()
		subscribe(ctx, t, a, "seat.inbox", "grp", func(context.Context, *events.Event) queue.Result {
			t.Errorf("delivered to a paused attachment")
			return queue.Ack()
		})
		subscribe(ctx, t, b, "seat.inbox", "grp", recordingHandler(gotB))
		if err := a.PauseTopic(ctx, "seat.inbox", "grp", holdReason); err != nil {
			t.Fatalf("PauseTopic: %v", err)
		}

		if holds := s.caps.PauseHolds; holds != nil {
			if got := holds(a, "seat.inbox", "grp"); !equalStrings(got, []string{holdReason}) {
				t.Fatalf("the pausing client's holds = %v, want [%s]", got, holdReason)
			}
			if got := holds(b, "seat.inbox", "grp"); len(got) != 0 {
				t.Fatalf("a peer inherited the hold: %v", got)
			}
		}

		for range 4 {
			publish(ctx, t, b, "seat.inbox", newEvent("work"))
		}
		gotB.awaitLabels(t, "the unpaused peer to take the whole round robin",
			"work", "work", "work", "work")
	})

	t.Run("stopping_one_client_leaves_the_broker_and_its_peers", func(t *testing.T) {
		t.Parallel()
		peer := s.needPeer(t)
		a := s.start(ctx, t)
		b := startQueue(ctx, t, peer(t, a))

		gotB := newJournal()
		subscribe(ctx, t, b, "seat.inbox", "grp", recordingHandler(gotB))
		if err := a.Stop(ctx); err != nil {
			t.Fatalf("Stop: %v", err)
		}

		publish(ctx, t, b, "seat.inbox", newEvent("still-serving"))
		gotB.awaitLabels(t, "a peer to keep serving after another node stopped", "still-serving")
	})

	t.Run("a_stream_subscriber_sees_a_peers_publish", func(t *testing.T) {
		t.Parallel()
		// A broadcast is a BROKER fact, not a client one. A dashboard
		// attached to one node has to see what another node publishes,
		// or half the fleet's traffic is invisible on every screen —
		// and invisible in a way that looks exactly like a quiet company.
		peer := s.needPeer(t)
		a := s.start(ctx, t)
		b := startQueue(ctx, t, peer(t, a))

		watching := newJournal()
		streamTo(ctx, t, a, "crewlet.events.>", watching)
		publish(ctx, t, b, "crewlet.events.agent_phase_started", newEvent("agent_phase_started"))

		watching.awaitLabels(t, "a peer's publish to reach the stream",
			"crewlet.events.agent_phase_started/agent_phase_started")
	})

	t.Run("a_peer_can_delete_a_subscription_it_never_consumed", func(t *testing.T) {
		t.Parallel()
		// Decommissioning a role must not depend on which node happened
		// to run the seat.
		peer := s.needPeer(t)
		backlog := s.needBacklog(t)
		a := s.start(ctx, t)
		b := startQueue(ctx, t, peer(t, a))

		if _, err := a.EnsureSubscription(ctx, "seat.gone", "grp"); err != nil {
			t.Fatalf("EnsureSubscription: %v", err)
		}
		publish(ctx, t, a, "seat.gone", newEvent("e1"))
		awaitState(t, "the event to be retained", func() bool {
			return len(backlog(a, "seat.gone", "grp")) == 1
		})

		deleted, err := b.DeleteSubscription(ctx, "seat.gone", "grp")
		if err != nil || !deleted {
			t.Fatalf("DeleteSubscription from a peer = (%v, %v), want (true, nil)", deleted, err)
		}
		if got := backlog(a, "seat.gone", "grp"); len(got) != 0 {
			t.Fatalf("the subscription survived a peer's delete: %v", labelsOf(got))
		}
	})
}

func assertAttached(t *testing.T, got [][2]string, topic, group string) {
	t.Helper()
	for _, pair := range got {
		if pair[0] == topic && pair[1] == group {
			return
		}
	}
	t.Fatalf("attachments = %v, want to contain (%s, %s)", got, topic, group)
}
