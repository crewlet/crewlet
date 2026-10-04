package engine

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"

	"github.com/crewlet/crewlet/internal/config"
)

// A SEAT'S MEMORY TRAVELS ON AN EXTERNAL BROKER TOO.
//
// A seat's memory follows it between nodes over the changelog (adr/0014), and
// the topology whose seats move between machines most is the one dialling an
// external cluster. The syncer was built over the embedded broker's second
// connection, which is nil by design when the node dialled out — so on
// exactly that topology it came up nil, and every seat forgot what it had
// learned each time placement moved it, with nothing logged.
func TestSeatMemoryTravelsOnAnExternalBroker(t *testing.T) {
	t.Parallel()
	broker, err := server.NewServer(&server.Options{
		ServerName: "external", JetStream: true, Port: -1, Host: "127.0.0.1",
		StoreDir: filepath.Join(t.TempDir(), "external"), NoSigs: true,
		MaxPayload: 8 << 20,
	})
	if err != nil {
		t.Fatalf("start an external broker: %v", err)
	}
	go broker.Start()
	t.Cleanup(broker.Shutdown)
	if !broker.ReadyForConnections(30 * time.Second) {
		t.Fatal("the external broker never became ready")
	}

	b := config.DefaultBootstrap()
	b.Store.Path = filepath.Join(t.TempDir(), "crewlet.db")
	b.Stream.Type = config.StreamNATS
	b.Stream.URL = broker.ClientURL()
	b.Coordination.Type = config.CoordinationEmbeddedKV
	cfg, err := config.ParseCompany([]byte(nativeCleanupCompany))
	if err != nil {
		t.Fatalf("parse the company: %v", err)
	}
	e, err := New(t.Context(), Options{Bootstrap: &b, Company: cfg})
	if err != nil {
		t.Fatalf("New on an external broker: %v", err)
	}
	t.Cleanup(func() { e.Stop(context.Background()) })

	if e.memory == nil {
		t.Fatal("a node on an external broker built no memory syncer, so a " +
			"seat it acquires hydrates nothing and one it releases carries " +
			"nothing — every move is a seat that forgot")
	}
}
