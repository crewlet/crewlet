package jetstream

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/queuetest"
)

// TestConformance certifies this backend against the one suite every backend
// runs. A backend the suite has not certified does not exist as far as the
// engine is concerned — which is the whole reason the suite is separate from
// any backend that passes it.
func TestConformance(t *testing.T) {
	t.Parallel()
	queuetest.RunWith(t, newConformanceQueue, capabilities())
}

// TestInspectionClientsEndWithTheirTest pins that the inspection registry, and
// the BrokerDown registry beside it, hold a queue only while the test that
// opened it runs. Each entry holds a
// whole broker, so one that outlives its test is a broker the binary keeps
// until it exits — which is how a -count=20 run of this package reached
// 12 GB and was killed.
func TestInspectionClientsEndWithTheirTest(t *testing.T) {
	t.Parallel()
	var q *Queue
	t.Run("open", func(t *testing.T) {
		q = openForTest(t, Config{})
		if inspector(q) == q {
			t.Fatal("no inspection client was registered for the queue")
		}
	})

	adminMu.Lock()
	_, held := admins[q]
	_, stoppable := servers[q]
	adminMu.Unlock()
	if held {
		t.Fatal("the inspection registry still holds a queue whose test has ended, and through it the queue's whole broker")
	}
	if stoppable {
		t.Fatal("the BrokerDown registry still holds a queue whose test has ended, and through it the queue's whole broker")
	}
}

// newConformanceQueue returns a fresh queue on its own embedded broker.
//
// Own broker per queue, not per test binary: the suite asserts things like
// "a subscription nobody created retains nothing", which a shared broker
// carrying another subtest's streams could satisfy accidentally.
func newConformanceQueue(t *testing.T, opts ...queue.Option) queue.EventQueue {
	t.Helper()
	return openForTest(t, Config{}, opts...)
}

// openForTest starts a broker owned by the TEST, not by the queue, and
// returns a client of it.
//
// That split is what lets the suite inspect a subscription after stopping
// the client that used it — "the mail survived a node leaving" is precisely
// the property seat ownership rests on, and it is unobservable if stopping
// the node also took the broker down.
func openForTest(t *testing.T, cfg Config, opts ...queue.Option) *Queue {
	t.Helper()
	srv := startTestServer(t, testTimings(cfg))
	return clientUnderTest(t, srv, srv.Server, opts...)
}

// testServer is a broker a TEST owns, beside the one stop its cleanup and the
// BrokerDown capability both take.
type testServer struct {
	*Server
	// stop shuts the server down ONCE, because the BrokerDown capability
	// may stop it before the cleanup does, and [Server.Shutdown] is
	// idempotent only when the two calls cannot overlap.
	stop func()
}

// startTestServer starts a broker the test owns and stops it when the test
// ends.
func startTestServer(t *testing.T, cfg Config) testServer {
	t.Helper()
	srv, err := StartServer(t.Context(), cfg)
	if err != nil {
		t.Fatalf("StartServer: %v", err)
	}
	var shutdown sync.Once
	stop := func() { shutdown.Do(srv.Shutdown) }
	t.Cleanup(stop)
	return testServer{Server: srv, stop: stop}
}

// testTimings shortens the timings a suite would otherwise wait out.
//
// Production timings would make this suite take hours: a 30-minute ack
// window, a one-second poll, a one-second redelivery delay. The behaviours
// under test are the same at any scale, and the numbers themselves are pinned
// separately by the measurement harness.
func testTimings(cfg Config) Config {
	if cfg.AckWait == 0 {
		cfg.AckWait = 2 * time.Second
	}
	if cfg.FetchWait == 0 {
		cfg.FetchWait = 25 * time.Millisecond
	}
	// 25 ms, and it is what queuetest's return-order case stands on: a
	// failure is returned with NakWithDelay and the consumer serves the
	// never-delivered mail while it waits, so it comes back BEHIND it, while
	// a hand-back is a plain Nak whose redelivery the broker serves before
	// new mail. Measured 100 of 100 each way, 60 of them under CPU pressure,
	// at this value (76ad6f656).
	if cfg.NakDelay == 0 {
		cfg.NakDelay = 25 * time.Millisecond
	}
	// AND ITS CEILING, or the doubling puts a suite's later redeliveries
	// seconds apart and the redelivery cases wait for real minutes.
	if cfg.NakCeiling == 0 {
		cfg.NakCeiling = 50 * time.Millisecond
	}
	return cfg
}

// clientUnderTest is a client of srv, built with opts, with an inspection
// client of inspect registered beside it — and srv's stop registered as the
// way the BrokerDown capability takes the client's broker away.
func clientUnderTest(t *testing.T, srv testServer, inspect *Server, opts ...queue.Option) *Queue {
	t.Helper()
	q, err := srv.Client(t.Context(), opts...)
	if err != nil {
		t.Fatalf("Client: %v", err)
	}
	t.Cleanup(func() {
		if stopErr := q.Stop(context.WithoutCancel(t.Context())); stopErr != nil {
			t.Errorf("Stop: %v", stopErr)
		}
	})

	// An inspection client the suite's capabilities read through. It
	// outlives the queue under test on purpose: the backlog left behind
	// by a stopped node is exactly what several cases assert about.
	admin, err := inspect.Client(t.Context())
	if err != nil {
		t.Fatalf("admin Client: %v", err)
	}
	t.Cleanup(func() { _ = admin.Stop(context.WithoutCancel(t.Context())) })
	adminMu.Lock()
	admins[q] = admin
	servers[q] = srv.stop
	adminMu.Unlock()
	// Registered after both Stops so it runs before them, and after every
	// cleanup the case itself registers — those may still read through the
	// inspector.
	t.Cleanup(func() {
		adminMu.Lock()
		delete(admins, q)
		delete(servers, q)
		adminMu.Unlock()
	})

	return q
}

// admins maps a queue under test to the inspection client for its broker, and
// servers to what stops the broker it is a client of — the BrokerDown
// capability's one way of taking a broker away — each for as long as the test
// that opened the queue runs.
//
// THE ENTRIES GO WITH THE TEST, because both keys hold the whole broker: a
// Queue keeps its embeddedServer, and with it the nats-server and every
// stream, buffer and subscription list it allocated. A package-level map that
// is never pruned keeps every broker the package ever started — about 50 MB
// of heap per conformance run, 240 MB of resident memory under -race — and a
// run at -count=20 was OOM-killed at 12 GB.
var (
	adminMu sync.Mutex
	admins  = map[*Queue]*Queue{}
	servers = map[*Queue]func(){}
)

// takeBrokerDown stops the server q is a client of and returns once q's
// connection has noticed, so the verb sent next meets a connection that is
// down rather than racing the client's discovery of it.
//
// STOPPED, NOT PARTITIONED, because stopping is the one outage the harness can
// produce exactly: the connection goes into the reconnect loop every
// connection to a broker that went away goes into, and stays there for the
// reconnect budget — far longer than any case takes to send its verbs.
func takeBrokerDown(t *testing.T, q queue.EventQueue) {
	t.Helper()
	jq, ok := q.(*Queue)
	if !ok {
		t.Fatalf("BrokerDown called with a %T", q)
	}
	adminMu.Lock()
	stop := servers[jq]
	adminMu.Unlock()
	if stop == nil {
		t.Fatalf("BrokerDown called with a queue clientUnderTest did not open")
	}
	stop()
	deadline := time.Now().Add(10 * time.Second)
	for jq.nc.Status() == nats.CONNECTED {
		if time.Now().After(deadline) {
			t.Fatalf("the connection still reads CONNECTED 10s after its server stopped")
		}
		time.Sleep(5 * time.Millisecond) //fixedwait:liveness
	}
}

// inspector returns the client to run a capability read through: the
// long-lived admin client when there is one, else the queue itself.
func inspector(q queue.EventQueue) *Queue {
	jq, ok := q.(*Queue)
	if !ok {
		return nil
	}
	adminMu.Lock()
	defer adminMu.Unlock()
	if a, ok := admins[jq]; ok {
		return a
	}
	return jq
}

// settlePoll is how often settleQueue reads the loops again while one has not
// settled: a fifth of the harness's poll (testTimings' FetchWait), so a loop
// that has come round is seen within a fifth of its cycle. It decides nothing:
// each read does, and the suite's settleFor bounds them all.
const settlePoll = 5 * time.Millisecond

// settleQueue is this backend's Settle: it returns once q will dispatch nothing
// more until the next verb is sent to it.
//
// A consume loop is the one goroutine that dispatches for its attachment, and
// it dispatches only between a fetch and its next loop top ([Queue.Loops]). So
// once, after a snapshot of every loop, each has
//
//   - ended (it is gone from Loops);
//   - stood at a loop top after the snapshot with its gate shut, so that it
//     fetches nothing until a verb opens it;
//   - stood at a loop top after the snapshot with its connection down, when it
//     was down at the snapshot already: what it fetched before is dispatched,
//     and every fetch after it is asked of a broker that cannot answer, since
//     the outage BrokerDown takes is one-way;
//   - or a mailbox that reads empty ([Queue.Backlog]), sized from the stream,
//     whose ack floor can only lag backwards — so an empty read is never a
//     message still owed, and a message not yet acked is never read as gone —
//
// nothing is left that it could hand a handler. The broker's own counts cannot
// say this: they are snapshots, which cannot tell a loop parked at its gate
// from one between two pulls, and a consumer's pending counts lag a publish.
// The mailbox is not read with the connection down, where the broker that holds
// it may be the one that stopped.
func settleQueue(ctx context.Context, t *testing.T, eq queue.EventQueue) error {
	t.Helper()
	q, ok := eq.(*Queue)
	if !ok {
		return fmt.Errorf("the Settle capability was handed a %T", eq)
	}
	before := q.Loops()
	down := q.nc.Status() != nats.CONNECTED
	// A PAIR THIS CLIENT IS ATTACHED TO WITH NO LOOP IN THE SNAPSHOT is a
	// Loops that stopped reporting — a field renamed, a map it no longer
	// reads — and settling over it would check nothing. The pairs are read
	// after the loops, so a pair let go of between the two reads drops out
	// of the second rather than reading as missing from the first.
	for _, pair := range q.Attachments() {
		if !slices.ContainsFunc(before, func(l Loop) bool { return l.Topic == pair[0] && l.Group == pair[1] }) {
			return fmt.Errorf("no consume loop in Loops for the attached pair (%s, %s), "+
				"so settling would check nothing", pair[0], pair[1])
		}
	}
	since := make(map[uint64]uint64, len(before))
	for _, l := range before {
		since[l.ID] = l.Cycles
	}
	return untilLoops(ctx, "consume loops still able to dispatch", func() ([]string, error) {
		return unsettledLoops(ctx, q, since, down)
	})
}

// untilLoops reads q's consume loops through waiting every settlePoll until it
// names none still to wait for, failing with what it named last, as what,
// when ctx ends first.
func untilLoops(ctx context.Context, what string, waiting func() ([]string, error)) error {
	poll := time.NewTicker(settlePoll) //fixedwait:liveness
	defer poll.Stop()
	for {
		names, err := waiting()
		if err != nil {
			return err
		}
		if len(names) == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%s: %s: %w", what, strings.Join(names, "; "), ctx.Err())
		case <-poll.C:
		}
	}
}

// parkedLoops is this backend's Parked: it returns once every consume loop of
// (topic, group) has come round to the top of its cycle since the call and
// found its gate shut there ([Queue.Loops]). A loop is the one goroutine that
// dispatches for its attachment, and it reaches its top only once what it
// fetched before is dispatched or handed back; with the gate shut there, it
// fetches nothing more until a verb opens it. A loop that returns instead
// never comes round again, so it is named when ctx ends — or at once, when it
// was let go of and has left Loops.
func parkedLoops(ctx context.Context, _ *testing.T, eq queue.EventQueue, topic, group string) error {
	q, ok := eq.(*Queue)
	if !ok {
		return fmt.Errorf("the Parked capability was handed a %T", eq)
	}
	since := map[uint64]uint64{}
	for _, l := range q.Loops() {
		if l.Topic == topic && l.Group == group {
			since[l.ID] = l.Cycles
		}
	}
	if len(since) == 0 {
		return fmt.Errorf("no consume loop of (%s, %s) in Loops, so there is nothing to park",
			topic, group)
	}
	return untilLoops(ctx, "consume loops not yet parked", func() ([]string, error) {
		now := q.Loops()
		var unparked []string
		for id, cycles := range since {
			i := slices.IndexFunc(now, func(l Loop) bool { return l.ID == id })
			switch {
			case i < 0:
				return nil, fmt.Errorf("the loop of (%s, %s) at cycle %d ended rather than "+
					"parking at its gate", topic, group, cycles)
			case now[i].Cycles > cycles && now[i].Parked:
			default:
				unparked = append(unparked, fmt.Sprintf("the loop of (%s, %s) at cycle %d, "+
					"not yet round to a shut gate", topic, group, now[i].Cycles))
			}
		}
		return unparked, nil
	})
}

// unsettledLoops names each loop of q that has not settled since the snapshot
// since, which holds each loop's cycles then. A loop the snapshot did not hold
// was attached after it, so every top it has reached is after it.
func unsettledLoops(ctx context.Context, q *Queue, since map[uint64]uint64, down bool) ([]string, error) {
	var busy []string
	empty := map[[2]string]bool{}
	for _, l := range q.Loops() {
		if l.Cycles > since[l.ID] && (l.Parked || down) {
			continue
		}
		name := fmt.Sprintf("the loop of (%s, %s) at cycle %d", l.Topic, l.Group, l.Cycles)
		if down {
			busy = append(busy, name+", not yet round with its connection down")
			continue
		}
		pair := [2]string{l.Topic, l.Group}
		isEmpty, read := empty[pair]
		if !read {
			held, err := inspector(q).Backlog(ctx, l.Topic, l.Group)
			switch {
			case err != nil && ctx.Err() != nil:
				// The bound ended inside the read: the loop is still
				// unsettled, and the caller names it with the rest.
				busy = append(busy, name+", its mailbox still being read when the bound ended")
				continue
			case err != nil:
				return nil, fmt.Errorf("read the mailbox of (%s, %s): %w", l.Topic, l.Group, err)
			}
			isEmpty = len(held) == 0
			empty[pair] = isEmpty
		}
		if !isEmpty {
			busy = append(busy, name+", neither parked nor with an empty mailbox")
		}
	}
	return busy, nil
}

// SETTLE WAITS OUT A DELIVERY THE BROKER STILL OWES, which is what makes a read
// behind it hold everything the backend was going to hand over: here a failure
// whose redelivery is spaced well past the loop's poll, so the loop comes round
// many times, its gate open, before the message it owes comes back.
//
// Mutation: report every loop top as parked, or every mailbox as empty, and
// Settle returns before the redelivery.
func TestSettleWaitsOutARedeliveryTheBrokerStillOwes(t *testing.T) {
	t.Parallel()
	// The spacing is what puts loop tops between the failure and its
	// redelivery: twenty of the harness's polls.
	const spacing = 500 * time.Millisecond
	q := openForTest(t, Config{NakDelay: spacing, NakCeiling: spacing})
	failed := make(chan struct{})
	var mu sync.Mutex
	var deliveries int
	if err := q.Subscribe(t.Context(), "settle.owed", "grp",
		func(context.Context, *events.Event) queue.Result {
			mu.Lock()
			defer mu.Unlock()
			deliveries++
			if deliveries == 1 {
				close(failed)
				return queue.Nak(errors.New("the first delivery fails"))
			}
			return queue.Ack()
		}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if err := q.Publish(t.Context(), "settle.owed", &events.Event{Type: "owed"}); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	select {
	case <-failed:
	case <-time.After(10 * time.Second): //fixedwait:liveness
		t.Fatal("the published event was never delivered")
	}

	// LIVENESS: a Settle that never returns fails here, and the redelivery
	// it waits for comes after the spacing.
	settling, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if err := settleQueue(settling, t, q); err != nil {
		t.Fatalf("Settle: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if deliveries != 2 {
		t.Fatalf("Settle returned with %d deliveries made, before the redelivery the broker "+
			"still owed", deliveries)
	}
}

func capabilities() queuetest.Capabilities {
	return capabilitiesFor(func(t *testing.T, cfg Config) *Queue { return openForTest(t, cfg) })
}

// capabilitiesFor is the suite's capabilities over queues open builds, so a
// topology that starts its brokers differently is certified on the same
// cases.
func capabilitiesFor(open func(*testing.T, Config) *Queue) queuetest.Capabilities {
	return queuetest.Capabilities{
		Settle: settleQueue,

		Peer: func(t *testing.T, q queue.EventQueue) queue.EventQueue {
			t.Helper()
			owner, ok := q.(*Queue)
			if !ok {
				t.Fatalf("Peer called with a %T", q)
			}
			peer, err := owner.Peer(t.Context())
			if err != nil {
				t.Fatalf("Peer: %v", err)
			}
			t.Cleanup(func() {
				_ = peer.Stop(context.WithoutCancel(t.Context()))
			})
			return peer
		},

		// MaxDeliver already counts total deliveries, which is the
		// observable the suite asks for, so the translation is the identity.
		WithDeliveryAttempts: func(t *testing.T, attempts int) queue.EventQueue {
			t.Helper()
			return open(t, Config{MaxDeliver: attempts})
		},

		// Both reads report a failure AS a failure. Returning the error as
		// an empty result would tell the suite the seat is holding no mail
		// whenever the broker did not answer, inside the group that asserts
		// absences.
		Backlog: func(t *testing.T, q queue.EventQueue, topic, group string) []*events.Event {
			t.Helper()
			evs, err := inspector(q).Backlog(context.Background(), topic, group)
			if err != nil {
				t.Fatalf("Backlog(%s, %s): %v", topic, group, err)
			}
			return evs
		},

		DeadLetters: func(t *testing.T, q queue.EventQueue, topic, group string) []*events.Event {
			t.Helper()
			evs, err := inspector(q).DeadLetters(context.Background(), topic, group)
			if err != nil {
				t.Fatalf("DeadLetters(%s, %s): %v", topic, group, err)
			}
			return evs
		},

		Attachments: func(q queue.EventQueue) [][2]string {
			return q.(*Queue).Attachments()
		},

		PauseHolds: func(q queue.EventQueue, topic, group string) []string {
			return q.(*Queue).PauseHolds(topic, group)
		},

		Quiescing: func(q queue.EventQueue, topic, group string) bool {
			return q.(*Queue).Quiescing(topic, group)
		},

		Lingering: func(q queue.EventQueue, topic, group string) bool {
			return q.(*Queue).Lingering(topic, group)
		},

		InWindow: func(q queue.EventQueue, topic, group string) int {
			return q.(*Queue).InWindow(topic, group)
		},

		// The drain's own reading of where its window ends and whether it
		// has; each fetch inside the window keeps its expiry on the broker's
		// clock, so the drain reads this again within the harness's poll.
		LingerClock: func(t *testing.T, clock *queuetest.Clock) queue.EventQueue {
			t.Helper()
			return open(t, Config{clock: clock})
		},

		Parked: parkedLoops,

		StreamSubscriptions: func(q queue.EventQueue) int {
			return q.(*Queue).StreamSubscriptions()
		},

		// THE CLIENT'S OWN BARRIER, BEHIND A FLUSH. A broadcast this
		// client sent reaches its own listeners from its own server, which
		// delivers it to the connection ahead of the PONG the flush waits
		// for, so once the flush returns the client has queued it to every
		// subscription it matched; nats.Conn.Barrier then runs once each of
		// the connection's callback subscriptions has delivered what it held
		// — the listeners' handlers called and returned.
		ListenBarrier: func(ctx context.Context, t *testing.T, q queue.EventQueue) {
			t.Helper()
			nc := q.(*Queue).Conn()
			if err := nc.FlushWithContext(ctx); err != nil {
				t.Fatalf("flush ahead of the listen barrier: %v", err)
			}
			passed := make(chan struct{})
			if err := nc.Barrier(func() { close(passed) }); err != nil {
				t.Fatalf("Barrier: %v", err)
			}
			select {
			case <-passed:
			case <-ctx.Done():
				t.Fatalf("the client's barrier had not passed its listeners when the case's "+
					"bound ended: %v", ctx.Err())
			}
		},

		Answering: func(q queue.EventQueue, subject string) (running, waiting int) {
			return q.(*Queue).Answering(subject)
		},

		// THE QUEUE UNDER TEST, never the inspector: this counts what the
		// client being certified asked the broker for, and the admin
		// connection asked for none of it. Reading it through inspector()
		// the way the two mail reads do would report zero for ever and
		// pass the case vacuously.
		ConsumerProposals: func(q queue.EventQueue) int {
			return q.(*Queue).ConsumerProposals()
		},

		BrokerDown: takeBrokerDown,

		// Deliberately NOT declared, each for a measured reason:
		//
		// InlineDispatch — pull consumers fetch on their own schedule, so
		// a publish returns before anything has been dispatched.
		//
		// StrictRoundRobin — JetStream serves whichever member asks
		// first. Each event still reaches exactly one member, which is
		// the part every broker owes.
		//
		// History — this backend has no ledger of everything ever
		// published; interest retention deliberately drops what no
		// subscription covers, which is the mailbox semantic itself.
		//
		// RequiresStart — Open establishes the connection and
		// streams, so a publish before Start is legitimate here.
	}
}
