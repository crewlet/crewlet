package engine

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/tracker"
)

// A NODE BELOW THE FLEET'S GENERATION TAKES THE FLEET'S GENERATION, AND ADOPTS
// — WHETHER IT HOLDS NO CHECKPOINT OR ONE AT THE GENERATION THE FLEET LEFT.
//
// In a fleet that has re-anchored, the records before the re-anchor are not on
// the log. A node that replayed from the new stream's sequence 1 would stamp
// every row at its own generation while its peers hold the same records at N,
// report itself caught up over a database holding only what was published
// since, and block the fleet's trim, whose applied term reads a counted node
// at a lower generation as unknown. And the floor comparison made at its own
// generation against a floor published at N fails the boot on the call that
// exists to send it to adopt — so the decision below is either an adoption or
// an error, and an error here is a node that can never come up.
//
// Mutation: compare a found checkpoint's own generation against the floor and
// the second case fails its boot; read an absent checkpoint as generation zero
// and the first asks for an artefact no peer holds.
func TestANodeBelowTheFleetsGenerationTakesItAndAdopts(t *testing.T) {
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

	// THE TRIM IS QUIESCED FIRST, because it is the other writer of the
	// floor this case forges. `PutFloor` replaces a domain's floor
	// wholesale, and this node's own retention loop publishes one per
	// domain per tick at the generation its rows are on — zero — so a tick
	// landing after the write below erases the fleet's generation 3 and the
	// case asserts against a floor it did not publish. `stopRetention`
	// waits for an in-flight tick, so after it returns nothing else writes.
	e.stopRetention()

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

	logs := map[string]*jetstream.DomainLog{}
	for _, domain := range registeredDomains() {
		running := s.Domain(domain.Name())
		if running == nil {
			t.Fatalf("%s is not running", domain.Name())
		}
		logs[domain.Name()] = running.log
	}
	decides := func(t *testing.T, what string) {
		t.Helper()
		behind, want, err := s.replayable(t.Context(), logs)
		if err != nil {
			t.Fatalf("replayable with %s: %v — the node compared its own "+
				"generation against the fleet's floor and failed its boot on the "+
				"call that would have sent it to adopt", what, err)
		}
		if got := want.Generations[name]; got != 3 {
			t.Errorf("with %s the node would ask for a generation-%d artefact, "+
				"want 3 — every donor refuses an artefact from another "+
				"generation, so a request below it is one no peer can satisfy",
				what, got)
		}
		if !slices.Contains(behind, name) {
			t.Errorf("with %s the node judged itself able to replay %s in a "+
				"fleet at generation 3 — the records before the re-anchor are "+
				"not on the log", what, name)
		}
	}

	// THIS NODE HOLDS A CHECKPOINT AT THE GENERATION THE FLEET LEFT — a
	// peer of the node that re-anchored, restarted to follow it. Written
	// here, because a company that has published nothing to this log has
	// no checkpoint on it yet.
	const left = 1
	running := s.Domain(name)
	if err := s.db.Replicated().Tx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `
			INSERT INTO statelog_cursor (stream, generation, seq, stream_created_at, updated_at)
			VALUES (?, ?, ?, ?, ?)
			ON CONFLICT (stream) DO UPDATE SET
				generation = excluded.generation, seq = excluded.seq`,
			stream, left, 0, store.EncodeTime(running.createdAt),
			store.EncodeTime(time.Now()))
		return err
	}); err != nil {
		t.Fatalf("write a checkpoint at generation %d: %v", left, err)
	}
	decides(t, fmt.Sprintf("a checkpoint at generation %d", left))

	// AND THIS NODE HOLDS NO CHECKPOINT AT ALL — a machine added to the
	// company, or one whose replicated estate was lost and rebuilt.
	if err := s.db.Replicated().Tx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(),
			`DELETE FROM statelog_cursor WHERE stream = ?`, stream)
		return err
	}); err != nil {
		t.Fatalf("clear the checkpoint: %v", err)
	}
	decides(t, "no checkpoint")
}
