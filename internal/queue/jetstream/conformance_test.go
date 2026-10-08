package jetstream

import (
	"context"
	"sync"
	"testing"
	"time"

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

// TestInspectionClientsEndWithTheirTest pins that the inspection registry
// holds a queue only while the test that opened it runs. Each entry holds a
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
	adminMu.Unlock()
	if held {
		t.Fatal("the inspection registry still holds a queue whose test has ended, and through it the queue's whole broker")
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
	cfg = testTimings(cfg)
	srv, err := StartServer(t.Context(), cfg)
	if err != nil {
		t.Fatalf("StartServer: %v", err)
	}
	t.Cleanup(srv.Shutdown)
	return clientUnderTest(t, srv, srv, opts...)
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
// client of inspect registered beside it.
func clientUnderTest(t *testing.T, srv, inspect *Server, opts ...queue.Option) *Queue {
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
	adminMu.Unlock()
	// Registered after both Stops so it runs before them, and after every
	// cleanup the case itself registers — those may still read through the
	// inspector.
	t.Cleanup(func() {
		adminMu.Lock()
		delete(admins, q)
		adminMu.Unlock()
	})

	return q
}

// admins maps a queue under test to the inspection client for its broker,
// for as long as the test that opened the queue runs.
//
// THE ENTRY GOES WITH THE TEST, because both keys hold the whole broker: a
// Queue keeps its embeddedServer, and with it the nats-server and every
// stream, buffer and subscription list it allocated. A package-level map that
// is never pruned keeps every broker the package ever started — about 50 MB
// of heap per conformance run, 240 MB of resident memory under -race — and a
// run at -count=20 was OOM-killed at 12 GB.
var (
	adminMu sync.Mutex
	admins  = map[*Queue]*Queue{}
)

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

func capabilities() queuetest.Capabilities {
	return capabilitiesFor(func(t *testing.T, cfg Config) *Queue { return openForTest(t, cfg) })
}

// capabilitiesFor is the suite's capabilities over queues open builds, so a
// topology that starts its brokers differently is certified on the same
// cases.
func capabilitiesFor(open func(*testing.T, Config) *Queue) queuetest.Capabilities {
	return queuetest.Capabilities{
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
