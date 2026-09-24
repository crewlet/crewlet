package eventfantest_test

import (
	"os"
	"testing"

	"github.com/crewlet/crewlet/internal/eventfan/eventfantest"
	"github.com/crewlet/crewlet/internal/queue"
	js "github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/queue/jetstream/jetstreamtest"
	"github.com/crewlet/crewlet/internal/solo"
)

// This package stands up a THREE-member embedded JetStream cluster
// (jetstreamtest.StartCluster) — a reply crossing a route between members is
// what the suite is certifying here, and one server with three clients crosses
// none — so it cannot share a runner with the rest of the suite. See
// `go doc ./internal/solo`.
func TestMain(m *testing.M) {
	os.Exit(solo.Run(m))
}

// THE SUITE, on a real cluster: each member a client of its own server.
func TestTheScatterOnAThreeMemberCluster(t *testing.T) {
	t.Parallel()
	eventfantest.Run(t, func(t *testing.T, n int) []queue.EventQueue {
		c := jetstreamtest.StartCluster(t, n, js.Config{})
		out := make([]queue.EventQueue, 0, n)
		for i := range n {
			out = append(out, c.Client(t, i))
		}
		return out
	})
}
