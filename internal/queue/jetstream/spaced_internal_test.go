package jetstream

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/queue"
)

// spacedOutcomes are the returns that go back spaced: a pass, and a failure
// (the node's own, so no failure mark asks a broker).
var spacedOutcomes = map[string]queue.Result{
	"pass":    queue.Pass("newer_record", errors.New("a peer on a newer build reads it")),
	"failure": queue.NakNode(errors.New("this node failed it")),
}

// A SPACED RETURN GOES BACK ONLY ONCE THE LOOP HOLDS ITS NEXT DELIVERY, so it
// comes back behind the mail that was waiting whatever its backoff. Sent at
// once, the backoff raced the next pull, which a loaded runner or a leaf's link
// lost, and the broker served the due redelivery first (crewlet/crewlet#2108).
// Read at the broker's side of each pull: how many returns were sent by then.
//
// Owed, the delivery is kept in progress, so the hold costs no redelivery.
//
// Mutation: send a spaced return at once (returnMsg), and the second pull finds
// it sent; send what is owed after every fetch, the one that took the returned
// delivery included, and so does it; drop the in-progress acknowledgement, and
// the owed delivery has none.
func TestASpacedReturnGoesBackOnlyOnceTheLoopHoldsItsNextDelivery(t *testing.T) {
	t.Parallel()
	for name, outcome := range spacedOutcomes {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			returned, next := deliveredEvent("test.returned", 7), deliveredEvent("test.next", 8)
			cons := &pullWitness{returned: returned, scriptedConsumer: &scriptedConsumer{t: t,
				answers: []pullAnswer{{delivered: []jetstream.Msg{returned}}, {delivered: []jetstream.Msg{next}}}}}
			a := spacingAttachment(t, cons)
			sentAtNext := -1
			h := func(_ context.Context, ev *events.Event) queue.Result {
				if ev.Type == "test.next" {
					sentAtNext = len(returned.delays)
					return queue.Ack()
				}
				return outcome
			}

			a.takeOne(t.Context(), h)
			a.takeOne(t.Context(), h)

			if !slices.Equal(cons.sent, []int{0, 0}) {
				t.Errorf("the returns sent at each pull were %v, want [0 0]: a return sent before the "+
					"pull for the waiting mail races its backoff against that pull", cons.sent)
			}
			if sentAtNext != 1 {
				t.Errorf("%d returns were sent when the next delivery's handler ran, want 1: the "+
					"waiting mail in hand is what the return goes back behind", sentAtNext)
			}
			if returned.progressed != 1 {
				t.Errorf("the owed delivery was kept in progress %d times, want once: held "+
					"without it, its ack window can lapse before the return is sent", returned.progressed)
			}
		})
	}
}

// A SPACED RETURN IS NEVER HELD PAST WHAT THE LOOP WILL TAKE: a cycle that
// found nothing waiting, a loop whose gate is shut, a loop that returns and one
// whose context a stop ended mid-fetch each send what is owed, or it would sit
// out polls, a hold or the ack window.
//
// Mutation: drop any of the three sends (takeOne's when it took nothing,
// reachedTop's when blocked, run's deferred one), and its case keeps the
// return unsent.
func TestASpacedReturnIsSentOnceTheLoopTakesNothingMore(t *testing.T) {
	t.Parallel()
	pass := spacedOutcomes["pass"]
	h := func(context.Context, *events.Event) queue.Result { return pass }
	cases := map[string]func(t *testing.T, a *attachment){
		"nothing_waited": func(t *testing.T, a *attachment) {
			a.takeOne(t.Context(), h)
			a.takeOne(t.Context(), h)
		},
		"the_gate_shut": func(t *testing.T, a *attachment) {
			a.takeOne(t.Context(), h)
			a.reachedTop(true)
		},
		"the_loop_ended": func(t *testing.T, a *attachment) {
			a.done, a.abandon = make(chan struct{}), func() {}
			a.run(t.Context(), func(ctx context.Context, a *attachment) { a.takeOne(ctx, h) })
		},
		"its_context_ended": func(t *testing.T, a *attachment) {
			ctx, stop := context.WithCancel(t.Context())
			a.done, a.abandon, a.fetching = make(chan struct{}), stop, ctx
			a.run(ctx, func(ctx context.Context, a *attachment) {
				a.takeOne(ctx, h)
				stop()
				// Its fetch ends unanswered, so the cycle keeps the return owed.
				a.takeOne(ctx, h)
			})
		},
	}
	for name, then := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			returned := deliveredEvent("test.returned", 7)
			a := spacingAttachment(t, &scriptedConsumer{t: t,
				answers: []pullAnswer{{delivered: []jetstream.Msg{returned}}, {}}})
			then(t, a)
			if len(returned.delays) != 1 {
				t.Errorf("the pass was sent %d times once the loop took nothing more, want once",
					len(returned.delays))
			}
		})
	}
}

// A SPACED RETURN STAYS OWED UNTIL THE BROKER ANSWERS: a fetch it refused, or
// one never sent, handed over none of the mail that waits, so a return sent
// then would race its backoff against the next pull as before. The next
// answered fetch sends it. In a bubble: a failed fetch waits out a poll.
//
// Mutation: send what is owed after any fetch that took nothing, answered or
// not, and the refused cycle sends it, on either path.
func TestASpacedReturnStaysOwedUntilTheBrokerAnswers(t *testing.T) {
	t.Parallel()
	pass := spacedOutcomes["pass"]
	paths := map[string]func(t *testing.T, a *attachment){
		"subscribe": func(t *testing.T, a *attachment) {
			a.takeOne(t.Context(), func(context.Context, *events.Event) queue.Result { return pass })
		},
		"drain": func(t *testing.T, a *attachment) {
			a.drain(t.Context(), queue.DefaultBatchOptions())
		},
	}
	for name, cycle := range paths {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				returned := deliveredEvent("test.returned", 7)
				a := spacingAttachment(t, &scriptedConsumer{t: t,
					answers: []pullAnswer{{refusal: nats.ErrNoResponders}, {}}})
				a.spaced = []spacedReturn{{msg: returned, delay: time.Second}}

				cycle(t, a)
				if len(returned.delays) != 0 {
					t.Fatalf("a return was sent %d times after a refused fetch, want none: "+
						"the mail it goes back behind still waits", len(returned.delays))
				}
				cycle(t, a)
				if len(returned.delays) != 1 {
					t.Errorf("a return was sent %d times once the broker answered, want once",
						len(returned.delays))
				}
			})
		})
	}
}

// A PASSED PARTITION GOES BACK ONLY ONCE THE NEXT DRAIN IS IN HAND, the batch
// path's form of the same order: its return is owed across the drain's later
// handlers and sent behind the next drain, never before that drain's pull.
//
// Mutation: send a spaced return at once, and it is sent before the next drain;
// send what is owed at the drain's start, and the drain's pull finds it sent.
func TestAPassedPartitionGoesBackOnlyOnceTheNextDrainIsInHand(t *testing.T) {
	t.Parallel()
	returned, waiting := deliveredEvent("test.returned", 7), deliveredEvent("test.waiting", 8)
	cons := &pullWitness{returned: returned, scriptedConsumer: &scriptedConsumer{t: t,
		answers: []pullAnswer{{delivered: []jetstream.Msg{waiting}}, {}}}}
	a := spacingAttachment(t, cons)
	opts := queue.DefaultBatchOptions()
	ev := &events.Event{Type: "test.returned"}

	a.dispatchBatch(t.Context(), []delivery{{msg: returned, ev: ev, seq: 7}},
		func(context.Context, []*events.Event) queue.Result { return spacedOutcomes["pass"] },
		func(*events.Event) string { return "conversation-1" }, opts)
	if len(returned.delays) != 0 {
		t.Fatalf("the passed partition was sent %d times before the next drain, want none",
			len(returned.delays))
	}
	if got := a.drain(t.Context(), opts); len(got) != 1 {
		t.Fatalf("the next drain took %d deliveries, want the 1 waiting", len(got))
	}
	if len(cons.sent) == 0 || cons.sent[0] != 0 {
		t.Errorf("the returns sent at each of the drain's pulls were %v, want none at its first: "+
			"a return sent before it races its backoff against that pull", cons.sent)
	}
	if len(returned.delays) != 1 {
		t.Errorf("the passed partition was sent %d times once the next drain was in hand, want once",
			len(returned.delays))
	}
}

// A RETURN OWED WHEN ITS PROCESS DIES IS DELIVERED AGAIN once the ack window
// its in-progress acknowledgement restarted lapses, as any delivery a dead node
// held: nothing else sends it, and delivery stays at least once.
//
// Mutation: settle the delivery when its return is owed (Ack or Term instead of
// InProgress), and nothing delivers it again.
func TestAReturnOwedWhenItsProcessDiesIsDeliveredAgain(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	srv := startTestServer(t, testTimings(Config{}))
	q := clientUnderTest(t, srv, srv.Server)
	topic, group := seatInbox("owed"), seatGroup("owed")
	if _, err := q.EnsureSubscription(ctx, topic, group); err != nil {
		t.Fatalf("EnsureSubscription: %v", err)
	}
	if err := q.Publish(ctx, topic, ev(1)); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	cons, err := q.js.Consumer(ctx, q.mustStream(t, topic), consumerName(topic, group))
	if err != nil {
		t.Fatalf("Consumer: %v", err)
	}
	dying := &attachment{q: q, cons: cons, log: slog.New(slog.DiscardHandler), fetching: ctx}
	dying.takeOne(ctx, func(context.Context, *events.Event) queue.Result { return spacedOutcomes["pass"] })
	if len(dying.spaced) != 1 {
		t.Fatalf("the loop owes %d returns, want the pass's one", len(dying.spaced))
	}

	// The process dies here: nothing sends what it owed. Each fetch waits for
	// the broker to hand the message over; the bound is a few ack windows.
	var md *jetstream.MsgMetadata
	taker := &attachment{cons: cons, fetching: ctx}
	for range 4 {
		if _, err := taker.fetch(1, 5*time.Second, func(msg jetstream.Msg) {
			md, _ = msg.Metadata()
			_ = msg.Ack()
		}); err != nil {
			t.Fatalf("fetch: %v", err)
		}
		if md != nil {
			break
		}
	}
	if md == nil || md.NumDelivered != 2 {
		t.Fatalf("the message whose return was owed came back as %+v, want its second delivery", md)
	}
}

// A RETURN OWED PAST ITS ACK WINDOW IS KEPT IN PROGRESS until it is sent: the
// next pull can stay open longer than the window, and a drain's later handlers
// run as long as they take. Lapsed, the broker delivers the message again, and
// the owed return, which it matches by stream sequence alone, lands on that
// newer delivery and has it delivered once more before its handler settles it:
// a handler past its budget, as the hold-over-pull cases saw. Read in a bubble,
// as the keeper's loop runs it ([attachment.run]): no gap in the owed
// delivery's renewals, through its return, exceeds a third of the window.
//
// Mutation: renew no owed return ([attachment.renewSpaced]), or start no keeper
// in run, and the gap is the whole wait.
func TestAReturnOwedPastItsAckWindowIsKeptInProgress(t *testing.T) {
	t.Parallel()
	const window = 3 * time.Second
	pass := spacedOutcomes["pass"]
	cases := map[string]func(t *testing.T, a *attachment, returned *clockedMsg, wait func()){
		"a pull open past the window": func(t *testing.T, a *attachment, returned *clockedMsg, wait func()) {
			a.cons = &slowPull{wait: wait, scriptedConsumer: &scriptedConsumer{t: t,
				answers: []pullAnswer{{delivered: []jetstream.Msg{returned}}, {}}}}
			h := func(context.Context, *events.Event) queue.Result { return pass }
			a.takeOne(t.Context(), h)
			a.takeOne(t.Context(), h)
		},
		"a drain's later handler running past it": func(t *testing.T, a *attachment, returned *clockedMsg, wait func()) {
			a.cons = &scriptedConsumer{t: t, answers: []pullAnswer{
				{delivered: []jetstream.Msg{deliveredEvent("test.waiting", 9)}}, {}}}
			at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			first := &events.Event{Type: "test.returned", Timestamp: at}
			later := &events.Event{Type: "test.later", Timestamp: at.Add(time.Second)}
			opts := queue.DefaultBatchOptions()
			a.dispatchBatch(t.Context(), []delivery{{msg: returned, ev: first, seq: 7},
				{msg: deliveredEvent("test.later", 8), ev: later, seq: 8}},
				func(_ context.Context, evs []*events.Event) queue.Result {
					if evs[0].Type == "test.returned" {
						return pass
					}
					wait()
					return queue.Ack()
				}, func(ev *events.Event) string { return ev.Type }, opts)
			if got := a.drain(t.Context(), opts); len(got) != 1 {
				t.Fatalf("the next drain took %d deliveries, want the 1 waiting", len(got))
			}
		},
	}
	for name, owe := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				returned := &clockedMsg{answeredMsg: deliveredEvent("test.returned", 7)}
				a := spacingAttachment(t, nil)
				a.q.cfg.AckWait = window
				a.done, a.abandon = make(chan struct{}), func() {}
				a.run(t.Context(), func(context.Context, *attachment) {
					owe(t, a, returned, func() { time.Sleep(4 * window) })
				})
				synctest.Wait()

				signals := returned.stamped()
				if n := len(signals); n == 0 || !signals[n-1].returned ||
					slices.ContainsFunc(signals[:n-1], func(s ackStamp) bool { return s.returned }) {
					t.Fatalf("the owed delivery's signals were %v, want renewals and then its one return",
						signals)
				}
				for i := 1; i < len(signals); i++ {
					if gap := signals[i].at.Sub(signals[i-1].at); gap > window/3 {
						t.Errorf("the owed delivery went %v unrenewed before signal %d of %d, past a "+
							"third of its %v window: the broker would deliver it again before its "+
							"return", gap, i+1, len(signals), window)
					}
				}
			})
		})
	}
}

// clockedMsg is a delivered message that stamps, in order and on the clock,
// each renewal of its ack window and each return sent of it.
type clockedMsg struct {
	*answeredMsg
	mu      sync.Mutex
	signals []ackStamp
}

// ackStamp is one acknowledgement of a clockedMsg: a renewal, or its return.
type ackStamp struct {
	at       time.Time
	returned bool
}

func (m *clockedMsg) InProgress() error { return m.stamp(false) }

func (m *clockedMsg) NakWithDelay(time.Duration) error { return m.stamp(true) }

func (m *clockedMsg) stamp(returned bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.signals = append(m.signals, ackStamp{at: time.Now(), returned: returned})
	return nil
}

// stamped is what m stamped so far.
func (m *clockedMsg) stamped() []ackStamp {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.signals)
}

// slowPull is a scripted broker that answers every pull after the first only
// once wait returns: a pull it holds open as long as wait takes.
type slowPull struct {
	*scriptedConsumer
	wait func()
}

func (c *slowPull) Fetch(n int, opts ...jetstream.FetchOpt) (jetstream.MessageBatch, error) {
	if len(c.fetches) > 0 {
		c.wait()
	}
	return c.scriptedConsumer.Fetch(n, opts...)
}

// pullWitness is a scripted broker that reads, at each pull, how many spaced
// returns of returned the loop had sent by then.
type pullWitness struct {
	*scriptedConsumer
	returned *answeredMsg
	sent     []int
}

func (c *pullWitness) Fetch(n int, opts ...jetstream.FetchOpt) (jetstream.MessageBatch, error) {
	c.sent = append(c.sent, len(c.returned.delays))
	return c.scriptedConsumer.Fetch(n, opts...)
}

// deliveredEvent is the first delivery of an event of type typ, stored at seq.
func deliveredEvent(typ string, seq uint64) *answeredMsg {
	return &answeredMsg{data: []byte(`{"type":"` + typ + `"}`),
		md: &jetstream.MsgMetadata{NumDelivered: 1, Sequence: jetstream.SequencePair{Stream: seq}}}
}

// spacingAttachment is an attachment reading cons at the shipped spacing, with
// no broker behind it: nothing here spends a budget or writes a mark.
func spacingAttachment(t *testing.T, cons jetstream.Consumer) *attachment {
	t.Helper()
	return &attachment{
		q:        &Queue{cfg: Config{NakDelay: time.Second, NakCeiling: 30 * time.Second}},
		cons:     cons,
		log:      slog.New(slog.DiscardHandler),
		fetching: t.Context(),
	}
}
