package engine_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/learning/memsync"
	"github.com/crewlet/crewlet/internal/objstore/natsobj"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/jetstream/externaltest"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
)

// A NODE'S UPLOADS SHARE THE WINDOW ITS BROKER KIND CALLS FOR: the small one
// where its connection crosses a link to the stream — a dial to an external
// cluster here, a leaf's link alike — because every request sent after a put
// on that link waits behind whatever the puts have out, and the large one on
// a member's own client, where nothing lies between. natsobj's own cases open
// the bucket with the choice made for them; this holds the engine to making
// it, since a node opened across a link with the member's window holds a
// request sent behind it a minute at the floor, past its five-second term.
func TestANodesUploadsShareTheWindowItsBrokerKindCallsFor(t *testing.T) {
	t.Parallel()
	// The windows natsobj gives, read from backends opened with the choice
	// made explicitly, so the case names no figure of its own.
	reference := func(t *testing.T, url string, cfg natsobj.Config) int {
		t.Helper()
		conn, err := nats.Connect(url, nats.Token(externaltest.Token))
		if err != nil {
			t.Fatalf("dial the external broker: %v", err)
		}
		defer conn.Close()
		js, err := jetstream.New(conn)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()
		// ANY RESERVATION: the window is no function of it, and the node
		// opened after it keeps the ceiling the bucket was created at.
		cfg.MaxBytes = 64 << 20
		b, err := natsobj.Open(ctx, js, cfg)
		if err != nil {
			t.Fatalf("open the bucket with %+v: %v", cfg, err)
		}
		return b.Window()
	}
	window := func(t *testing.T, b *config.Bootstrap) int {
		t.Helper()
		back, err := openBackends(t, b)
		if err != nil {
			t.Fatalf("OpenBackends: %v", err)
		}
		t.Cleanup(func() { back.Close(context.Background()) })
		objects, ok := back.Objects.(*natsobj.Backend)
		if !ok {
			t.Fatalf("the node's object store is a %T, want the broker's bucket", back.Objects)
		}
		return objects.Window()
	}
	client := func(url string) func(*config.Bootstrap) {
		return func(b *config.Bootstrap) {
			b.Stream.Type = config.StreamNATS
			b.Stream.URL = url
			b.Stream.Token = externaltest.Token
			b.Coordination.Type = config.CoordinationEmbeddedKV
		}
	}

	t.Run("a member", func(t *testing.T) {
		t.Parallel()
		url := externalBroker(t)
		want := reference(t, url, natsobj.Config{Replicas: 1})
		if got := window(t, bootstrap(t, nil)); got != want {
			t.Fatalf("a member's uploads share a window of %d messages, want the member's own %d", got, want)
		}
	})
	t.Run("a client of an external cluster", func(t *testing.T) {
		t.Parallel()
		url := externalBroker(t)
		want := reference(t, url, natsobj.Config{Replicas: 1, AcrossLink: true})
		member := reference(t, url, natsobj.Config{Replicas: 1})
		if want >= member {
			t.Fatalf("natsobj's window across a link is %d and on a member's client %d; the case needs "+
				"the first smaller to tell them apart", want, member)
		}
		if got := window(t, bootstrap(t, client(url))); got != want {
			t.Fatalf("a node dialling an external cluster shares a window of %d messages, want the %d "+
				"a request queued behind it on the link is sized for", got, want)
		}
	})
}

// A NODE'S MEMORY REPLAYS PULL THE WINDOW ITS BROKER KIND CALLS FOR: across a
// link — a dial to an external cluster here, a leaf's link alike — a replay's
// pull carries the window a request whose answer queues behind it on the link
// is sized for, and in a member's own process the connection's whole bound.
// memsync's own cases build the syncer with the choice made for them; this
// holds the engine to making it, since a node across a link whose syncer was
// built as a member's pulls up to 8 MiB at a time, which holds the answer to a
// request sent behind it four minutes at the floor, past its five-second
// term.
//
// Mutation: build the engine's syncer as a member's (memsync.Config's
// AcrossLink false), and the client's half goes red.
func TestANodesReplaysPullTheWindowItsBrokerKindCallsFor(t *testing.T) {
	t.Parallel()
	// The window memsync gives a syncer, read from one built with the choice
	// made explicitly, so the case names no figure of its own.
	reference := func(t *testing.T, url string, cfg memsync.Config) int {
		t.Helper()
		conn, err := nats.Connect(url, nats.Token(externaltest.Token))
		if err != nil {
			t.Fatalf("dial the external broker: %v", err)
		}
		t.Cleanup(conn.Close)
		js, err := jetstream.New(conn)
		if err != nil {
			t.Fatal(err)
		}
		db := storetest.OpenNode(t, filepath.Join(t.TempDir(), "node.db"), store.Options{})
		return memsync.New(db, js, func(handle string) string { return handle }, cfg).Window()
	}
	url := externalBroker(t)
	across := reference(t, url, memsync.Config{AcrossLink: true})
	own := reference(t, url, memsync.Config{})
	if across <= 0 || own != 0 {
		t.Fatalf("memsync windows a pull across a link at %d bytes and in a member's own process at %d; "+
			"the case needs a window across a link and none in a member's own process to tell the "+
			"two apart", across, own)
	}

	t.Run("a member", func(t *testing.T) {
		t.Parallel()
		if got := engine.MemoryWindow(newEngine(t, engine.Options{})); got != own {
			t.Fatalf("a member's replays pull a window of %d bytes, want none (%d): its own process "+
				"crosses no link", got, own)
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
		if got := engine.MemoryWindow(newEngine(t, engine.Options{Bootstrap: b})); got != across {
			t.Fatalf("a node dialling an external cluster pulls a window of %d bytes, want the %d a "+
				"request whose answer queues behind it on the link is sized for", got, across)
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
