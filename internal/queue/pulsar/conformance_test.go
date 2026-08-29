package pulsar

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/queuetest"
)

// Environment this suite reads. Only the first is required; the rest have
// defaults that match a `pulsar standalone`.
const (
	envURL      = "CREWLET_TEST_PULSAR_URL"
	envAdminURL = "CREWLET_TEST_PULSAR_ADMIN_URL"
	envTenant   = "CREWLET_TEST_PULSAR_TENANT"
	envToken    = "CREWLET_TEST_PULSAR_TOKEN"
)

// TestConformance certifies this backend against the one suite every backend
// runs. A backend the suite has not certified does not exist as far as the
// engine is concerned — which is the whole reason the suite is separate from
// any backend that passes it.
//
// It needs a REAL broker and skips without one. There is deliberately no
// in-process fake: a twin that is not the broker does not merely fail to
// catch bugs, it certifies them, and this repo has the scars to prove it
// (decisions/103-payload-pointer-invariant.md). Skipping is not
// passing — the CI job in .github/workflows/ci.yml is where this actually
// runs.
func TestConformance(t *testing.T) {
	requireBroker(t)
	queuetest.RunWith(t, newConformanceQueue, capabilities())
}

func requireBroker(t *testing.T) {
	t.Helper()
	if os.Getenv(envURL) == "" {
		t.Skipf("no Pulsar broker: set %s (e.g. pulsar://localhost:6650) to run the "+
			"conformance suite. Optional: %s, %s (default \"public\"), %s.",
			envURL, envAdminURL, envTenant, envToken)
	}
}

// newConformanceQueue returns a fresh queue in its OWN namespace.
//
// A namespace per queue, not per test binary. The suite's subtests run in
// parallel and share subject names — several of them use topic "t" group "g"
// — so on one shared broker they would consume each other's mail and the
// failures would read as ordering bugs in this backend. The other backends
// get the same isolation by starting a whole embedded broker per queue; here
// the namespace is the cheap equivalent, and it exercises the tenant-scoped
// naming that is the reason this backend exists.
func newConformanceQueue(t *testing.T) queue.EventQueue {
	t.Helper()
	return openForTest(t, Config{})
}

func openForTest(t *testing.T, cfg Config) *Queue {
	t.Helper()
	requireBroker(t)

	cfg.URL = os.Getenv(envURL)
	cfg.AdminURL = os.Getenv(envAdminURL)
	cfg.Token = os.Getenv(envToken)
	cfg.Tenant = cmpOr(os.Getenv(envTenant), "public")
	cfg.Namespace = "crewlet-test-" + strings.ReplaceAll(uuid.NewString(), "-", "")[:16]

	// Production timings would make this suite take minutes of pure
	// waiting: a one-second poll and a one-second redelivery delay. The
	// behaviours under test are the same at any scale, and the numbers
	// themselves are pinned separately (the constants in pulsar.go, with
	// their measurements in decisions/104-pulsar-redelivery-economics.md).
	if cfg.ReceiveWait == 0 {
		cfg.ReceiveWait = 25 * time.Millisecond
	}
	if cfg.NackRedeliveryDelay == 0 {
		// Well under the suite's 50 ms linger window, because one case
		// deliberately makes a redelivered event race a freshly published
		// sibling into the same batch. The client's nack tracker polls at
		// delay/3, so this is what decides whether the redelivery lands
		// inside the drain rather than one batch later.
		cfg.NackRedeliveryDelay = 10 * time.Millisecond
	}
	if cfg.ReceiverQueueSize == 0 {
		// A SMALL prefetch, deliberately, and this one shrinks a
		// production value for a reason worth stating rather than for
		// speed.
		//
		// Pulsar dispatches to a Shared subscription by available
		// PERMITS: it hands one consumer as many entries as it has room
		// for before moving to the next. At the production prefetch (64)
		// a burst of four messages is legitimately taken whole by
		// whichever member the dispatcher reaches first — each event
		// still goes to exactly one member, which is all competing
		// consumers owe. But queuetest's members_of_a_group_compete asks
		// the stronger question, that the load is SHARED, and that is
		// only observable when the prefetch is smaller than the burst.
		//
		// Two makes it deterministic. The production value is chosen for
		// an unrelated reason — bounding how much of a seat's mail one
		// node can hold hostage (see receiverQueueSize) — and the batch
		// paths still get max(2, 2*max_batch) from prefetchFor, so
		// coalescing is unaffected.
		cfg.ReceiverQueueSize = 2
	}
	if cfg.AutoDiscoveryPeriod == 0 {
		// A broadcast subscription only sees a topic once the client's
		// pattern scan has found it, and the suite publishes to a
		// brand-new topic immediately after subscribing. In production
		// this window is a feed-latency tradeoff (see autoDiscoveryPeriod);
		// here it has to fit MANY times inside the suite's settle budget,
		// because one scan landing a moment before the topic exists costs
		// a whole period and the case has 3 s in total.
		cfg.AutoDiscoveryPeriod = 100 * time.Millisecond
	}

	createNamespace(t, cfg)
	q, err := Open(t.Context(), cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if err := q.Stop(context.WithoutCancel(t.Context())); err != nil {
			t.Errorf("Stop: %v", err)
		}
	})
	return q
}

func cmpOr(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

// createNamespace provisions the queue's namespace and removes it afterwards.
//
// Namespaces are never auto-created — Pulsar auto-creates topics, never
// tenants or namespaces — which is exactly the operational note this backend
// carries, so the suite has to do what an operator does.
func createNamespace(t *testing.T, cfg Config) {
	t.Helper()
	base := cfg.AdminURL
	if base == "" {
		derived, err := DeriveAdminURL(cfg.URL)
		if err != nil {
			t.Fatalf("derive admin url: %v", err)
		}
		base = derived
	}
	endpoint := fmt.Sprintf("%s/admin/v2/namespaces/%s/%s",
		strings.TrimRight(base, "/"), cfg.Tenant, cfg.Namespace)

	if status, body := adminCall(t, http.MethodPut, endpoint, cfg.Token); status != http.StatusNoContent && status != http.StatusOK {
		t.Fatalf("create namespace %s/%s: %d %s", cfg.Tenant, cfg.Namespace, status, body)
	}
	t.Cleanup(func() {
		// Forced, because the suite deliberately leaves subscriptions and
		// backlogs behind — that is what several cases assert about. A
		// failure here leaks a namespace on a test broker and is not
		// worth failing a green run over.
		status, body := adminCall(t, http.MethodDelete, endpoint+"?force=true", cfg.Token)
		if status != http.StatusNoContent && status != http.StatusOK && status != http.StatusNotFound {
			t.Logf("could not remove namespace %s/%s: %d %s", cfg.Tenant, cfg.Namespace, status, body)
		}
	})
}

func adminCall(t *testing.T, method, endpoint, token string) (int, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), adminTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, endpoint, nil)
	if err != nil {
		t.Fatalf("build %s %s: %v", method, endpoint, err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, endpoint, err)
	}
	defer resp.Body.Close()
	body := make([]byte, 512)
	n, _ := resp.Body.Read(body)
	return resp.StatusCode, string(body[:n])
}

func capabilities() queuetest.Capabilities {
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
			t.Cleanup(func() { _ = peer.Stop(context.WithoutCancel(t.Context())) })
			return peer
		},

		// Pulsar's DLQPolicy.MaxDeliveries counts TOTAL deliveries — the
		// client's own router says so ("the user specifies that wants to
		// process a message up to 10 times") — so the observable the
		// suite asks for maps across with no arithmetic. Which is the
		// reason the suite asks for attempts rather than a budget: NATS
		// counts the same way and the in-memory twin does not.
		WithDeliveryAttempts: func(t *testing.T, attempts int) queue.EventQueue {
			t.Helper()
			return openForTest(t, Config{MaxDeliveries: attempts})
		},

		Backlog: func(q queue.EventQueue, topic, group string) []*events.Event {
			evs, _ := q.(*Queue).Backlog(context.Background(), topic, group)
			return evs
		},

		DeadLetters: func(q queue.EventQueue, topic, group string) []*events.Event {
			evs, _ := q.(*Queue).DeadLetters(context.Background(), topic, group)
			return evs
		},

		Attachments: func(q queue.EventQueue) [][2]string { return q.(*Queue).Attachments() },

		PauseHolds: func(q queue.EventQueue, topic, group string) []string {
			return q.(*Queue).PauseHolds(topic, group)
		},

		Quiescing: func(q queue.EventQueue, topic, group string) bool {
			return q.(*Queue).Quiescing(topic, group)
		},

		// FREE DEFERRAL — the property this backend has and JetStream does
		// not. Measured on Pulsar 4.2.4: a graceful consumer close returns
		// unacked messages at redeliveryCount 0 where an ack
		// timeout costs one (the table at the head of
		// tests/test_queue/test_broker_behavior.py, and
		// decisions/102-jetstream-redelivery.md). So a deferral
		// here leaves the message unacked and the consumer's close — or
		// Unquiesce's recycle — hands it back whole.
		FreeDeferral: true,

		// Deliberately NOT declared, each for a reason:
		//
		// HeadReplayOnNak — the BROKER replays from the head, but this
		// client PREFETCHES, so by the time a handler naks a message its
		// never-delivered siblings are already sitting in the local
		// receiver queue and are served first. The property the suite
		// asserts is what the handler observes, and that is not it.
		// Nothing depends on it: within-conversation order comes from
		// event timestamps, which
		// within_a_partition_events_are_ordered_by_timestamp certifies
		// for every backend (d-102 decision 3).
		//
		// InlineDispatch — a consumer receives on its own schedule, so a
		// publish returns before anything has been dispatched.
		//
		// StrictRoundRobin — a Shared subscription dispatches by
		// available permits, so a member with an empty prefetch takes
		// more. Each event still reaches exactly one member, which is the
		// part every broker owes.
		//
		// History — this backend has no ledger of everything ever
		// published; a topic no subscription covers drops its messages,
		// which is the mailbox semantic itself.
		//
		// RequiresStart — Open connects, so a publish before
		// Start is legitimate here.
		//
		// Restartable — Stop closes the client and Start does not
		// re-establish it, and the contract does not say whether it
		// should; the flag's own doc carries what is unsettled.
	}
}
