package engine_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/jsapi"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/jetstream/externaltest"
)

// A NODE'S TRANSFERS SHARE ONE WINDOW ACROSS ITS LINK.
//
// Across a link (a dial to an external cluster here, a leaf's alike) the
// object store's transfers and the memory sync's replay pulls take from the
// ONE window the link's queue owns (ADR-0160). natsobj's and memsync's cases
// hand their stores a window; this holds the engine to handing both the
// queue's. A member crosses no link: its puts share a window of their own, and
// a replay pulls the connection's whole bound.
//
// Mutation: hand the object store or the memory sync a window of its own,
// or none, and the client's half goes red.
func TestANodesTransfersShareOneWindowAcrossItsLink(t *testing.T) {
	t.Parallel()
	t.Run("a member", func(t *testing.T) {
		t.Parallel()
		link, objects, memory := engine.TransferWindows(newEngine(t, engine.Options{}))
		if link != nil || memory != nil {
			t.Fatalf("a member's queue has a link window %v and its memory sync %v; want none: its "+
				"own process crosses no link", link, memory)
		}
		if objects == nil || objects.Size() == jsapi.LinkWindow {
			t.Fatalf("a member's object store transfers inside %v, want its puts' own window", objects)
		}
	})
	t.Run("a client of an external cluster", func(t *testing.T) {
		t.Parallel()
		b := bootstrap(t, func(b *config.Bootstrap) {
			b.Stream.Type = config.StreamNATS
			b.Stream.URL = externalBroker(t)
			b.Stream.Token = externaltest.Token
			b.Coordination.Type = config.CoordinationEmbeddedKV
		})
		link, objects, memory := engine.TransferWindows(newEngine(t, engine.Options{Bootstrap: b}))
		if link == nil || link.Size() != jsapi.LinkWindow {
			t.Fatalf("a node dialling an external cluster has a link window %v, want one of %d bytes",
				link, jsapi.LinkWindow)
		}
		if objects != link || memory != link {
			t.Fatalf("a node dialling an external cluster transfers objects inside %p and replays "+
				"inside %p, want both inside its link's one window %p", objects, memory, link)
		}
	})
}

// externalBroker starts a JetStream broker of the case's own, as an external
// cluster an operator runs, and answers the URL its clients dial — one a case,
// because the lease bucket the first node makes on it fixes the lease every
// later node holds.
//
// EXTERNALTEST'S, WHOSE ACCOUNT STATES ITS LIMIT. A node on an external broker
// reads the account's limit and nothing else; where the account states none,
// it stands three quarters of its own volume's free space in for the limit and
// reserves the bucket a quarter of that. A server built here capped its store
// and left its account unlimited, so the case passed or failed on the runner's
// disk: past about 92 GB free the bucket asked for more than the cap, and the
// broker refused it.
func externalBroker(t *testing.T) string {
	t.Helper()
	return externaltest.Start(t, queue.MaxPayloadBytes).URL()
}
