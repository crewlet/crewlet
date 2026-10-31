package engine_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/objstore/natsobj"
	"github.com/crewlet/crewlet/internal/queue/jetstream/externaltest"
	"github.com/crewlet/crewlet/internal/seat"
)

// A NODE'S UPLOADS SHARE THE WINDOW ITS BROKER KIND CALLS FOR, AT THE LEASE
// IT HOLDS: the small one where its connection crosses a link to the stream —
// a dial to an external cluster here, a leaf's link alike — because a lease
// renewal queues behind whatever the puts have out there, sized by the
// heartbeat of the lease IN FORCE; and the large one on a member's own
// client, where nothing lies between. natsobj's own cases open the bucket
// with the choice made for them; this holds the engine to making it, since a
// node opened across a link with the member's window holds a renewal four
// times as long at the floor, past its lease — and one opened with a window
// sized for the shipped lease while it holds a shorter one does the same.
//
// The lease in force is the fleet's bucket's, which the first node to make it
// set: a node configured for another TTL renews at the bucket's, and its
// window follows that, not its own file.
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
	linkAt := func(ttl time.Duration) natsobj.Config {
		return natsobj.Config{Replicas: 1, AcrossLink: true, LeaseHeartbeat: ttl / seat.HeartbeatRatio}
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
	client := func(url string, ttl time.Duration) func(*config.Bootstrap) {
		return func(b *config.Bootstrap) {
			b.Stream.Type = config.StreamNATS
			b.Stream.URL = url
			b.Stream.Token = externaltest.Token
			b.Coordination.Type = config.CoordinationEmbeddedKV
			b.Coordination.LeaseTTLSeconds = ttl.Seconds()
		}
	}
	const shortLease = 15 * time.Second

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
		want, member := reference(t, url, linkAt(seat.SeatLeaseTTL)), reference(t, url, natsobj.Config{Replicas: 1})
		if want >= member {
			t.Fatalf("natsobj's window across a link is %d and on a member's client %d; the case needs "+
				"the first smaller to tell them apart", want, member)
		}
		if got := window(t, bootstrap(t, client(url, 0))); got != want {
			t.Fatalf("a node dialling an external cluster at the shipped lease shares a window of %d "+
				"messages, want the %d its lease renewals are sized for", got, want)
		}
	})
	t.Run("a client whose lease is shorter", func(t *testing.T) {
		t.Parallel()
		url := externalBroker(t)
		want := reference(t, url, linkAt(shortLease))
		if shipped := reference(t, url, linkAt(seat.SeatLeaseTTL)); want >= shipped {
			t.Fatalf("natsobj's window on a %v lease is %d and on the shipped one %d; the case needs the "+
				"first smaller to tell them apart", shortLease, want, shipped)
		}
		if got := window(t, bootstrap(t, client(url, shortLease))); got != want {
			t.Fatalf("a node dialling an external cluster with coordination.lease_ttl_seconds %v shares "+
				"a window of %d messages, want the %d its own lease's renewals are sized for", shortLease,
				got, want)
		}
	})
	t.Run("a client adopting the fleet's lease", func(t *testing.T) {
		t.Parallel()
		url := externalBroker(t)
		// The fleet's first node makes the lease bucket at the shipped TTL;
		// this one, configured shorter, renews at the bucket's.
		window(t, bootstrap(t, client(url, 0)))
		want := reference(t, url, linkAt(seat.SeatLeaseTTL))
		if short := reference(t, url, linkAt(shortLease)); short >= want {
			t.Fatalf("natsobj's window on a %v lease is %d and on the shipped one %d; the case needs the "+
				"first smaller to tell them apart", shortLease, short, want)
		}
		if got := window(t, bootstrap(t, client(url, shortLease))); got != want {
			t.Fatalf("a node configured for a %v lease on a fleet whose lease bucket holds %v shares a "+
				"window of %d messages, want the %d the lease in force is renewed by", shortLease,
				seat.SeatLeaseTTL, got, want)
		}
	})
}

// externalBroker starts a JetStream broker of the case's own, as an external
// cluster an operator runs, and answers the URL its clients dial — one a case,
// because the lease bucket the first node makes on it fixes the lease every
// later node holds.
func externalBroker(t *testing.T) string {
	t.Helper()
	external, err := server.NewServer(&server.Options{
		ServerName: "external", JetStream: true, Port: -1, Host: "127.0.0.1",
		StoreDir: filepath.Join(t.TempDir(), "external"), NoSigs: true,
		MaxPayload: 8 << 20,
		// An external server authenticates somebody, or the dial refuses it.
		Authorization: externaltest.Token,
	})
	if err != nil {
		t.Fatalf("start an external broker: %v", err)
	}
	go external.Start()
	t.Cleanup(external.Shutdown)
	if !external.ReadyForConnections(30 * time.Second) {
		t.Fatal("the external broker never became ready")
	}
	return external.ClientURL()
}
