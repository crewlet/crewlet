package statelogtest

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// evictionLister is what the trim asks an identity-claiming domain: every
// eviction its own applied rows hold, on its own log. Declared here as well as
// in the engine: the suite consumes it too, so a domain answers it before the
// trim ever runs against its log.
type evictionLister interface {
	Evictions(ctx context.Context, db store.ReplicatedReader) ([]statelog.EvictionRow, error)
}

// runEvictions reports what [Evictions] found.
func runEvictions(t *testing.T, new Factory) {
	t.Helper()
	t.Run("an identity-claiming domain lists the evictions on its own log",
		func(t *testing.T) {
			if err := Evictions(t, new); err != nil {
				t.Fatal(err)
			}
		})
}

// Evictions certifies that a domain which claims identity answers, from its
// own applied rows, which nodes are evicted on its own log, and that one which
// claims none does not pretend to.
//
// The trim stops counting a node on a log only once its tombstone there is
// older than the fence window, and the floor theorem binds every identity
// log, so each needs its own answer: without one, an evicted node is counted
// for ever and the log grows to its ceiling. Asserted is the whole cycle the
// trim and node gate rely on: the eviction's own position (the gate's
// comparand) and the broker's instant (the fence window's start), a
// readmission kept as a row, and a re-eviction that clears it. It returns the
// verdict for [Declaration]'s reason.
func Evictions(t *testing.T, new Factory) error {
	t.Helper()
	c := new(t)
	name := c.Domain.Name()
	lister, lists := c.Domain.(evictionLister)
	if !c.Domain.ClaimsIdentity() {
		if lists {
			return fmt.Errorf("%s claims no identity and lists evictions — the "+
				"trim never asks a domain that does not count nodes, so the "+
				"answer is a second opinion nothing reads", name)
		}
		return nil
	}
	switch {
	case !lists:
		return fmt.Errorf("%s claims identity and lists no evictions, so the "+
			"trim can never stop counting an evicted node on its log — the log "+
			"grows behind a node that is not coming back until its ceiling "+
			"refuses writes", name)
	case c.EncodeGate == nil:
		return fmt.Errorf("%s claims identity and supplies no eviction record, "+
			"so nothing can show its rows answer for one", name)
	}

	db := openEstate(t, c)
	w, pinErr := db.Writer(t.Context())
	if pinErr != nil {
		t.Fatalf("pin a writer: %v", pinErr)
	}
	defer func() { _ = w.Close() }()

	const node = "suite-node-away"
	base := time.Unix(1_700_000_000, 0).UTC()
	var seq uint64
	apply := func(readmit bool) (statelog.Position, time.Time) {
		t.Helper()
		seq++
		body, err := c.EncodeGate(node, readmit)
		if err != nil {
			t.Fatalf("encode %s's gate record: %v", name, err)
		}
		env, err := c.Domain.Envelope(body)
		if err != nil {
			t.Fatalf("envelope of %s's gate record: %v", name, err)
		}
		if !c.Domain.InstallsGate(env) {
			t.Fatalf("%s's gate record is not declared as installing a gate, "+
				"so a build that cannot decode it would defer it", name)
		}
		at := statelog.Position{Stream: c.spec().Name, Generation: 1, Seq: seq}
		stored := base.Add(time.Duration(seq) * time.Minute)
		rec := statelog.Record{Envelope: env, Position: at, Payload: body, StoredAt: stored}
		opts := statelog.ApplyOptions{
			Now: stored, StoredAt: stored,
			ArbitratedKinds: c.spec().ArbitratedKinds,
			MaxVariables:    db.Caps().MaxVariables,
		}
		if err := w.Tx(t.Context(), func(tx *sql.Tx) error {
			return applyOne(t.Context(), c, tx, rec, opts)
		}); err != nil {
			t.Fatalf("apply %s's gate record at %s: %v", name, at, err)
		}
		return at, stored
	}
	standing := func() (statelog.EvictionRow, error) {
		rows, err := lister.Evictions(t.Context(), db.Reader())
		if err != nil {
			return statelog.EvictionRow{}, fmt.Errorf("%s could not list its "+
				"evictions: %w", name, err)
		}
		var found []statelog.EvictionRow
		for _, row := range rows {
			if row.NodeID == node {
				found = append(found, row)
			}
		}
		if len(found) != 1 {
			return statelog.EvictionRow{}, fmt.Errorf("%s lists %d eviction "+
				"row(s) for %s, want exactly one — a node's standing on a log "+
				"is one fact", name, len(found), node)
		}
		return found[0], nil
	}

	evicted, at := apply(false)
	row, err := standing()
	switch {
	case err != nil:
		return err
	case row.Back:
		return fmt.Errorf("%s lists %s as back straight after its eviction", name, node)
	case row.From != uint64(evicted.Packed()):
		return fmt.Errorf("%s lists %s's eviction above %d, want its own "+
			"position %d — the gate drops records by comparing against exactly "+
			"this", name, node, row.From, evicted.Packed())
	case !row.At.Equal(at):
		return fmt.Errorf("%s lists %s's eviction at %s, want the broker's "+
			"own instant %s — the fence window is measured from it, and a "+
			"clock any one node read would give every node a different "+
			"window", name, node, row.At, at)
	}

	readmitted, _ := apply(true)
	if row, err = standing(); err != nil {
		return err
	}
	if !row.Back || row.Readmitted != uint64(readmitted.Packed()) ||
		row.From != uint64(evicted.Packed()) {
		return fmt.Errorf("%s lists %s after its readmission as %+v, want its "+
			"row kept, the eviction at %d and the readmission at %d — a "+
			"readmission is an inverse commit, not a delete",
			name, node, row, evicted.Packed(), readmitted.Packed())
	}

	again, _ := apply(false)
	if row, err = standing(); err != nil {
		return err
	}
	if row.Back || row.From != uint64(again.Packed()) {
		return fmt.Errorf("%s lists %s after a second eviction as %+v, want "+
			"evicted again above %d — a re-eviction clears the readmission",
			name, node, row, again.Packed())
	}
	return nil
}
