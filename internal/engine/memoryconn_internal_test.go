package engine

import (
	"path/filepath"
	"testing"

	"github.com/nats-io/nats.go"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/jetstream/externaltest"
)

// A NODE CARRIES ITS SEATS' MEMORY WHOEVER RUNS THE BROKER.
//
// A seat's memory is written to the node's own store and placement moves
// seats, so every row rides a changelog on the stream and the node that takes
// a seat replays it first. The syncer was handed Backends.Conn, whose nil on
// an external broker is the BACKUP's decision — an operator's cluster backs up
// its own streams — and a nil connection is how memsync says "this node cannot
// carry memory". So every fleet on `stream.type: nats` built no syncer, and a
// seat that moved forgot everything it had learned, with nothing saying so.
//
// Both topologies, and the connection each one's syncer rides: the
// coordination store's, which is the queue's own on an external broker and the
// second connection Backends owns on an embedded one.
//
// Mutation: hand memsync Backends.Conn again and the external row goes red.
func TestANodeCarriesSeatMemoryWhoeverRunsTheBroker(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		configure func(t *testing.T, b *config.Bootstrap)
		rides     func(back *Backends) *nats.Conn
	}{
		{"an embedded broker", func(t *testing.T, b *config.Bootstrap) {
			b.Stream.StoreDir = filepath.Join(t.TempDir(), "stream")
		}, func(back *Backends) *nats.Conn { return back.Conn() }},
		{"an operator's NATS server", func(t *testing.T, b *config.Bootstrap) {
			b.Stream.Type = config.StreamNATS
			b.Stream.URL = externaltest.Start(t, queue.MaxPayloadBytes).URL()
		}, func(back *Backends) *nats.Conn {
			return back.Queue.(interface{ Conn() *nats.Conn }).Conn()
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			b := testBootstrap(t)
			b.Store.Path = filepath.Join(t.TempDir(), "crewlet.db")
			b.Coordination.Type = config.CoordinationEmbeddedKV
			tc.configure(t, &b)
			e, back := bootNode(t, &b, nil)

			want := tc.rides(back)
			if got := back.CoordinationConn(); got == nil || got != want {
				t.Errorf("the coordination store's connection is %p, want %p", got, want)
			}
			if e.memory == nil {
				t.Error("this node built no memory syncer, so a seat placement " +
					"moves here forgets everything it learned elsewhere")
			}
		})
	}
}
