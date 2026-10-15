package queuetest

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/queue"
)

// reachWithin is the deadline every verb is sent with once its broker has gone.
//
// A CALLER'S deadline, deliberately, and short: on the backend that ships, a
// verb sent into a connection that is down waits for an answer that cannot
// come and ends on whichever deadline is nearer, so without one each would sit
// out its client's own default and the case would take a minute to say what
// it says in a second. The contract marks a deadline that ends while the
// connection is down ([queue.ErrUnavailable] says why), so this bounds the
// case without changing what it certifies.
const reachWithin = 500 * time.Millisecond

// runReachability certifies what a verb answers once the broker it would have
// to reach has gone away under a queue that is still live.
//
// # Why a group of its own
//
// Because it is the one point in a queue's life no other case visits — every
// other case runs against a healthy broker — and its answer is read by callers
// who decide opposite things on it. Marked [queue.ErrUnavailable], a tool tells
// its model to try again shortly and a person's surface answers 503 with a
// Retry-After. Unmarked, the same blip read above the queue as this node's own
// fault: `internal_error`, "trying again does not fix it", the broker's
// message only in the log. And marked [queue.ErrNotLive] it would be worse
// than either, because a seat release reads that as proof the mailbox is torn
// down and hands the seat to a peer while this node's consumers are still
// attached and resume when the connection does.
//
// THE WHOLE MATRIX, for the reason lifecycleVerbs gives: a case that sent the
// two verbs somebody thought of — Publish and Ask, the two a tool sends — would
// certify those two and leave the other twelve to whatever their backend
// happened to do.
func (s *suite) runReachability(t *testing.T) {
	ctx := t.Context()

	t.Run("a_verb_that_cannot_reach_the_broker_says_it_is_unavailable", func(t *testing.T) {
		t.Parallel()
		if s.caps.BrokerDown == nil {
			t.Skip("backend declares no BrokerDown: it cannot take its broker away " +
				"under a live client, so what its verbs answer then is uncertified")
		}
		q := s.start(ctx, t)
		// ATTACHED WHILE THE BROKER WAS UP, and holding mail it was told
		// not to take yet, so the verbs about this node's own attachments
		// below act on one that exists — and the resume among them has
		// something it could wrongly hand out.
		j := newJournal()
		subscribe(ctx, t, q, "reach.t", "g", recordingHandler(j))
		if _, err := q.Quiesce(ctx, "reach.t", "g"); err != nil {
			t.Fatalf("Quiesce with the broker up: %v", err)
		}
		publish(ctx, t, q, "reach.t", newEvent("held"))
		j.staysAt(t, 0, "a quiesced attachment took new work with its broker up")
		// AND A SECOND MAILBOX THIS NODE CONSUMES, for the delete below to
		// be asked about one it is attached to.
		subscribe(ctx, t, q, "reach.d", "g", recordingHandler(newJournal()))
		s.caps.BrokerDown(t, q)

		// TOGETHER, under one deadline, because on the backend that ships
		// most of them end on it: sent one after another the case pays
		// the deadline nine times for the same fact.
		reached, cancel := context.WithTimeout(ctx, reachWithin)
		defer cancel()
		for _, verb := range brokerVerbs(reached, q, "reach") {
			// Logged whatever it answered: the mark is what is
			// asserted, and the words around it are what a reader
			// diagnosing a failure here needs.
			t.Logf("%s: %v", verb.name, verb.err)
			switch {
			case verb.err == nil:
				t.Errorf("%s succeeded with its broker gone; whatever it reported "+
					"was not the broker's answer", verb.name)
			case errors.Is(verb.err, queue.ErrNotLive):
				t.Errorf("%s answered %v, which is queue.ErrNotLive; the queue is "+
					"still live and its consumers still attached, and a seat "+
					"release reading this as teardown hands the seat to a peer "+
					"this node may still be consuming it for", verb.name, verb.err)
			case !errors.Is(verb.err, queue.ErrUnavailable):
				t.Errorf("%s answered %v, which is not queue.ErrUnavailable; a "+
					"caller above the queue reads an unmarked failure as this "+
					"node's own fault, and tells whoever asked that trying "+
					"again will not help", verb.name, verb.err)
			}
		}

		// AN ASK IS REFUSED, NOT SENT — and the only thing a caller can see
		// of that is WHEN it is refused. A backend that wrote the request
		// into a reconnect buffer answers the same mark once the deadline
		// ends, having queued a request that goes out to its servers after
		// the asker was told it failed; one that refuses answers at once.
		// Half the deadline is a margin no refusal comes near and no wait
		// for the deadline gets under.
		asked, cancelAsk := context.WithTimeout(ctx, reachWithin)
		begun := time.Now()
		_, err := q.Ask(asked, "reach.refused", []byte("reach"), 1)
		took := time.Since(begun)
		cancelAsk()
		if !errors.Is(err, queue.ErrUnavailable) || took >= reachWithin/2 {
			t.Errorf("an Ask with its broker gone answered %v after %s, want "+
				"queue.ErrUnavailable well inside its %s deadline: a request "+
				"held for the reconnect is a retained one, delivered after its "+
				"asker has gone", err, took, reachWithin)
		}

		// A DELETE THE BROKER CANNOT HEAR STILL DETACHES, on both backends
		// alike: the detach is this node's own state and needs no broker,
		// and only the delete after it has to reach one. What that leaves
		// is a mailbox standing with nothing attached — the state every
		// mailbox is in while no node holds its seat, which a retry
		// finishes — and never a node still consuming a mailbox its
		// caller asked to destroy. A twin that refused before detaching
		// certified that second state against a backend that never
		// reaches it.
		deleting, cancelDelete := context.WithTimeout(ctx, reachWithin)
		_, err = q.DeleteSubscription(deleting, "reach.d", "g")
		cancelDelete()
		if !errors.Is(err, queue.ErrUnavailable) {
			t.Errorf("a DeleteSubscription of a mailbox this node consumes, with its "+
				"broker gone, answered %v, want queue.ErrUnavailable", err)
		}
		if s.caps.Attachments != nil {
			for _, pair := range s.caps.Attachments(q) {
				if pair == [2]string{"reach.d", "g"} {
					t.Errorf("a DeleteSubscription its broker never heard left this " +
						"node attached to the mailbox it was asked to destroy")
				}
			}
		}

		// AND THE VERBS ABOUT THIS NODE'S OWN ATTACHMENTS STILL ANSWER:
		// they are the client's own state and reach no broker, and a
		// node that can no longer prove it owns a seat — on exactly
		// this outage, when the coordination store rides the same
		// broker — quiesces that seat's mailbox, which must not fail
		// because the broker that cost it the proof is gone.
		local := func(verbs ...verbResult) {
			for _, verb := range verbs {
				if verb.err != nil {
					t.Errorf("%s refused with its broker gone (%v); it touches "+
						"nothing but this node's own attachment, so a node that "+
						"lost the proof it owns a seat could not stop taking its "+
						"work", verb.name, verb.err)
				}
			}
		}
		local(
			verbResult{"Quiesce", second(q.Quiesce(ctx, "reach.t", "g"))},
			verbResult{"PauseTopic", q.PauseTopic(ctx, "reach.t", "g", holdReason)},
			verbResult{"ResumeTopic", q.ResumeTopic(ctx, "reach.t", "g", holdReason)},
			verbResult{"Unquiesce", second(q.Unquiesce(ctx, "reach.t", "g"))},
		)
		// AND A RESUME HANDS OUT NOTHING the broker cannot deliver: the
		// held mail is in a broker nobody can reach, and a backend that
		// delivered it anyway would be certifying a fetch that cannot
		// happen.
		j.staysAt(t, 0, "an attachment resumed with its broker gone delivered mail "+
			"that broker could not have handed out")
		local(verbResult{"Detach", second(q.Detach(ctx, "reach.t", "g"))})
	})

	// A QUEUE STOPPED DURING THE OUTAGE IS NOT LIVE, and says so rather than
	// that its broker is unavailable. The order matters for the same seat
	// release: a node shutting down while its broker is gone detaches every
	// seat, and an answer of "unavailable" there reads as a detach that could
	// not be proven — which KEEPS THE LEASE and strands the seat for a full
	// TTL on the one path where the node is handing it back.
	t.Run("a_queue_stopped_while_its_broker_is_gone_is_not_live", func(t *testing.T) {
		t.Parallel()
		if s.caps.BrokerDown == nil {
			t.Skip("backend declares no BrokerDown; see the case above")
		}
		q := s.start(ctx, t)
		s.caps.BrokerDown(t, q)
		if err := q.Stop(ctx); err != nil {
			t.Fatalf("Stop with the broker gone: %v", err)
		}
		for _, verb := range lifecycleVerbs(ctx, q, "reach-stopped") {
			if !errors.Is(verb.err, queue.ErrNotLive) {
				t.Errorf("%s on a stopped queue whose broker is gone answered %v, "+
					"want queue.ErrNotLive: the stop is what a caller has to read, "+
					"and the outage behind it changes nothing it may do",
					verb.name, verb.err)
			}
		}
	})
}

// brokerVerbs sends every verb that has to REACH the broker, concurrently and
// under ctx, and reports what each answered. The complement of the attachment
// verbs runReachability sends itself, and together with them the fourteen
// lifecycleVerbs sends.
func brokerVerbs(ctx context.Context, q queue.EventQueue, ns string) []verbResult {
	topic := ns + ".broker"
	ack := func(context.Context, *events.Event) queue.Result { return queue.Ack() }
	sends := []struct {
		name string
		send func() error
	}{
		{"Publish", func() error { return q.Publish(ctx, topic, newEvent(topic)) }},
		{"Subscribe", func() error { return q.Subscribe(ctx, topic, "g", ack) }},
		{"SubscribeBatch", func() error {
			return q.SubscribeBatch(ctx, topic, "batch", func(context.Context, []*events.Event) queue.Result {
				return queue.Ack()
			}, nil, nil)
		}},
		{"EnsureSubscription", func() error { return second(q.EnsureSubscription(ctx, topic, "ensure")) }},
		{"DeleteSubscription", func() error { return second(q.DeleteSubscription(ctx, topic, "delete")) }},
		{"ListSubscriptions", func() error { return listErr(q.ListSubscriptions(ctx, ns+".>")) }},
		{"SubscribeStream", func() error {
			unsub, err := q.SubscribeStream(ctx, ns+".>", func(context.Context, string, *events.Event) {})
			if unsub != nil {
				_ = unsub(context.WithoutCancel(ctx))
			}
			return err
		}},
		{"Serve", func() error {
			stop, err := q.Serve(ctx, topic, func(context.Context, []byte) ([]byte, error) {
				return nil, nil
			})
			if stop != nil {
				_ = stop(context.WithoutCancel(ctx))
			}
			return err
		}},
		{"Ask", func() error {
			_, err := q.Ask(ctx, topic, []byte("reach"), 1)
			return err
		}},
	}
	out := make([]verbResult, len(sends))
	var wg sync.WaitGroup
	for i, v := range sends {
		wg.Go(func() { out[i] = verbResult{name: v.name, err: v.send()} })
	}
	wg.Wait()
	return out
}
