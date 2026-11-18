package engine

import (
	"context"
	"database/sql"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/queue/jetstream/brokertest"
	"github.com/crewlet/crewlet/internal/worklet"
)

// A NODE WITH NO CHECKPOINT TAKES THE FLEET'S GENERATION, AND ADOPTS.
//
// An absent checkpoint is `{generation 0, sequence 0}` only in a fleet never
// re-anchored. In one that HAS, the stream still begins at 1, so such a node
// would replay: stamping its rows at generation 0 while peers hold them at N,
// reporting itself caught up over only what followed the re-anchor, blocking
// the trim (a counted node at a lower generation reads as unknown), then
// failing its own boot on the first floor at N, the very comparison that
// should send it to adopt.
func TestANodeWithNoCheckpointTakesTheFleetsGeneration(t *testing.T) {
	t.Parallel()
	b := testBootstrap(t)
	b.Store.Path = filepath.Join(t.TempDir(), "crewlet.db")
	brokertest.StoreIn(&b.Stream, filepath.Join(brokertest.TempDir(t), "stream"))
	SeedStore(t, &b)
	cfg, err := config.ParseCompany([]byte(nativeCleanupCompany))
	if err != nil {
		t.Fatalf("parse the company: %v", err)
	}
	back, err := OpenBackends(t.Context(), &b, cfg, nil)
	if err != nil {
		t.Fatalf("OpenBackends: %v", err)
	}
	t.Cleanup(func() { back.Close(context.Background()) })
	e, err := New(t.Context(), Options{Bootstrap: &b, Company: cfg, Backends: back})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { e.Stop(context.Background()) })
	s := e.core.Load().log
	waitUntil(t, 20*time.Second, "the node to admit seats", hydrated(t, e))

	// THE TRIM IS QUIESCED FIRST, as the other writer of the floor forged
	// here: `PutFloor` replaces a domain's floor wholesale, and this node's
	// retention publishes one per tick at its rows' generation, zero,
	// erasing the fleet's 3. `stopRetention` waits for an in-flight tick.
	e.stopRetention()

	// AND THE APPLIERS ARE HALTED, as the other writer of the checkpoint
	// deleted here: the node writes its own logs once it admits seats (a
	// read barrier at boot), and a batch committed after the DELETE writes
	// the row back (`setCursor` upserts). `haltAppliers` waits for every
	// loop.
	s.haltAppliers()

	name := worklet.Domain{}.Name()
	stream := worklet.Domain{}.Stream().Name

	// THE FLEET HAS RE-ANCHORED: an operator moved it to generation 3, and
	// the trim has published a floor there.
	if err := back.Fleet.PutFloor(t.Context(), coord.TrimFloor{
		Domain: name, Generation: 3, TrimTo: 0,
		BlockedBy: "applied", At: time.Now().UTC(), By: "peer",
	}); err != nil {
		t.Fatalf("publish a floor at the fleet's generation: %v", err)
	}

	// AND THIS NODE HOLDS NO CHECKPOINT AT ALL — a machine added to the
	// company, or one whose replicated estate was lost and rebuilt.
	if err := s.db.Replicated().Tx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(),
			`DELETE FROM statelog_cursor WHERE stream = ?`, stream)
		return err
	}); err != nil {
		t.Fatalf("clear the checkpoint: %v", err)
	}

	logs := map[string]*jetstream.DomainLog{}
	for _, domain := range registeredDomains() {
		running := s.Domain(domain.Name())
		if running == nil {
			t.Fatalf("%s is not running", domain.Name())
		}
		logs[domain.Name()] = running.log
	}

	behind, _, want, err := s.replayable(t.Context(), logs)
	if err != nil {
		t.Fatalf("replayable: %v — a node with no checkpoint compared its "+
			"absent generation against the fleet's and failed its own boot on "+
			"the call that would have sent it to adopt", err)
	}
	if got := want.Generations[name]; got != 3 {
		t.Errorf("the node would ask for a generation-%d artefact, want 3 — "+
			"every donor refuses an artefact from another generation, so a "+
			"request at 0 is one no peer can ever satisfy", got)
	}
	if !slices.Contains(behind, name) {
		t.Errorf("the node judged itself able to replay %s from nothing in a "+
			"fleet at generation 3 — the records before the re-anchor are not "+
			"on the log, so what it would build is a database holding only "+
			"what came after, reporting itself caught up", name)
	}
}
