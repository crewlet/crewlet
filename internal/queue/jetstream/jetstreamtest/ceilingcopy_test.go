package jetstreamtest

import (
	"context"
	"fmt"
	"testing"

	"github.com/crewlet/crewlet/internal/queue"
	js "github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/queue/jetstream/brokertest"
	"github.com/crewlet/crewlet/internal/queue/queuetest"
)

// A COPY A PASS RETIRED AT THE TRANSPORT'S CEILING IS KEPT WHOLE WHERE ITS
// GROUP FINDS IT, on JetStream, on a broker of its own and through a leaf:
// queuetest's ceiling case (queuetest.RunCeilingCopy).
//
// It runs HERE, in a package of the solo roster, and not in the parallel
// conformance suite. Moving its megabytes four times beside the suite's other
// cases, on a loaded runner under the race detector, starved it
// (crewlet/crewlet#1132's CI). Its queues keep production's ack window, which
// outlasts the keep.
//
// Mutation: count a pass as a failure of its message, and its copy is kept
// with the pair's poison while the unhandled subject holds nothing.
func TestAPassedCopyAtTheTransportsCeilingIsKeptWhole(t *testing.T) {
	t.Run("on_its_own_broker", func(t *testing.T) {
		queuetest.RunCeilingCopy(t, func(t *testing.T, attempts int) queue.EventQueue {
			t.Helper()
			srv, err := js.StartServer(t.Context(), js.Config{MaxDeliver: attempts,
				StoreMaxBytes: brokertest.StoreMaxBytes})
			if err != nil {
				t.Fatalf("start a broker: %v", err)
			}
			t.Cleanup(srv.Shutdown)
			return clientOf(t, srv)
		})
	})
	t.Run("through_a_leaf", func(t *testing.T) {
		queuetest.RunCeilingCopy(t, func(t *testing.T, attempts int) queue.EventQueue {
			t.Helper()
			// A MEMBER THAT SERVES LEAVES PERSISTS, and the leaf is the
			// broker the case's queue is a client of.
			member, err := js.StartServer(t.Context(), js.Config{ServerName: "member",
				LeafHost: "127.0.0.1", LeafPort: js.AnyPort, StoreDir: t.TempDir(),
				StoreMaxBytes: brokertest.StoreMaxBytes, MaxDeliver: attempts, FleetAuth: fleetAuth()})
			if err != nil {
				t.Fatalf("start a member that serves leaves: %v", err)
			}
			t.Cleanup(member.Shutdown)
			leaf, err := js.StartServer(t.Context(), js.Config{ServerName: "leaf",
				LeafURLs:   []string{fmt.Sprintf("nats-leaf://127.0.0.1:%d", member.LeafPort())},
				MaxDeliver: attempts, FleetAuth: fleetAuth()})
			if err != nil {
				t.Fatalf("start a leaf of the member: %v", err)
			}
			t.Cleanup(leaf.Shutdown)
			return clientOf(t, leaf)
		})
	})
}

// clientOf is a new client of srv, stopped at the case's end.
func clientOf(t *testing.T, srv *js.Server) queue.EventQueue {
	t.Helper()
	q, err := srv.Client(t.Context())
	if err != nil {
		t.Fatalf("a client of the broker: %v", err)
	}
	t.Cleanup(func() { _ = q.Stop(context.WithoutCancel(t.Context())) })
	return q
}
