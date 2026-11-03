package engine

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/jetstream/externaltest"
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
	// THROUGH externaltest, whose server's account states a limit: a server
	// started here with none made the node stand this machine's free disk in
	// for its broker's limit, and a fresh node's reservations refused the boot
	// on a runner short of them.
	broker := externaltest.Start(t, queue.MaxPayloadBytes)

	b := testBootstrap(t)
	b.Store.Path = filepath.Join(t.TempDir(), "crewlet.db")
	b.Stream.Type = config.StreamNATS
	b.Stream.URL = broker.URL()
	b.Stream.Token = externaltest.Token
	b.Coordination.Type = config.CoordinationEmbeddedKV
	SeedStore(t, &b)
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
