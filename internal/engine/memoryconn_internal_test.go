package engine

import (
	"path/filepath"
	"testing"

	"github.com/nats-io/nats.go"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/jetstream/brokertest"
	"github.com/crewlet/crewlet/internal/queue/jetstream/externaltest"
)

// A NODE CARRIES ITS SEATS' MEMORY WHOEVER RUNS THE BROKER.
//
// Placement moves seats, so a seat's memory rides a changelog the taker
// replays. The syncer rides the coordination store's connection (the queue's
// own on an external broker, Backends' second one on an embedded broker),
// never Backends.Conn: that is nil on an external broker, the backup's
// decision, and a nil connection tells memsync this node carries no memory,
// so on `stream.type: nats` a moved seat would silently forget everything.
//
// Mutation: hand memsync Backends.Conn and the external row goes red.
func TestANodeCarriesSeatMemoryWhoeverRunsTheBroker(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		configure func(t *testing.T, b *config.Bootstrap)
		rides     func(back *Backends) *nats.Conn
	}{
		{"an embedded broker", func(t *testing.T, b *config.Bootstrap) {
			brokertest.StoreIn(&b.Stream, filepath.Join(brokertest.TempDir(t), "stream"))
		}, func(back *Backends) *nats.Conn { return back.Conn() }},
		{"an operator's NATS server", func(t *testing.T, b *config.Bootstrap) {
			b.Stream.Type = config.StreamNATS
			b.Stream.URL = externaltest.Start(t, queue.MaxPayloadBytes).URL()
			b.Stream.Token = externaltest.Token
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
