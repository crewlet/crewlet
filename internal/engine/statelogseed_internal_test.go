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
	"github.com/crewlet/crewlet/internal/tracker"
)

// A NODE WITH NO CHECKPOINT TAKES THE FLEET'S GENERATION, AND ADOPTS.
//
// An absent checkpoint row used to be read as `{generation 0, sequence 0}`,
// which in a fleet that has never re-anchored is exactly right: a fresh node
// has the whole log ahead of it. In one that HAS re-anchored it is a lie in
// the direction nothing recovers from.
//
// The stream still begins at sequence 1, so the behind test passed and the
// node replayed — stamping every row it wrote at generation 0 while its peers
// held the same records at N, reporting itself caught up over a database
// holding only what was published since the re-anchor, and blocking the
// fleet's trim, whose applied term reads a counted node at a lower generation
// as unknown. Then the first floor published at N failed the node's own boot
// on the floor comparison, which is the call that would have sent it to adopt:
// the state is terminal, and this test would have reported it as the error
// below rather than as a decision.
func TestANodeWithNoCheckpointTakesTheFleetsGeneration(t *testing.T) {
	t.Parallel()
	b := config.DefaultBootstrap()
	b.Store.Path = filepath.Join(t.TempDir(), "crewlet.db")
	b.Stream.StoreDir = filepath.Join(t.TempDir(), "stream")
	cfg, err := config.ParseCompany([]byte(nativeCleanupCompany))
	if err != nil {
		t.Fatalf("parse the company: %v", err)
	}
	back, err := OpenBackends(t.Context(), &b, cfg)
	if err != nil {
		t.Fatalf("OpenBackends: %v", err)
	}
	t.Cleanup(func() { back.Close(context.Background()) })
	e, err := New(t.Context(), Options{Bootstrap: &b, Company: cfg, Backends: back})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { e.Stop(context.Background()) })
	s := e.native.log
	waitUntil(t, 20*time.Second, "the node to admit seats", e.NativeHydrated)

	name := tracker.Domain{}.Name()
	stream := tracker.Domain{}.Stream().Name

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

	behind, want, err := s.replayable(t.Context(), logs)
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
