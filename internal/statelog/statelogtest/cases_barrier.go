package statelogtest

import (
	"fmt"
	"maps"
	"testing"

	"github.com/crewlet/crewlet/internal/recordseal"
	"github.com/crewlet/crewlet/internal/statelog"
)

// runBarrier reports what [ABarrierWritesNoRow] found.
func runBarrier(t *testing.T, new Factory) {
	t.Helper()
	t.Run("a barrier writes no row", func(t *testing.T) {
		if err := ABarrierWritesNoRow(t, new); err != nil {
			t.Fatal(err)
		}
	})
}

// ABarrierWritesNoRow certifies that the domain's own barrier (its read
// index's append, by its production encoder), applied through the framework's
// runner and the domain's applier, changes no row of any declared table.
//
// A linearizable read is released on a barrier from its envelope alone
// ([statelog.Runner.Reached]), before the domain's Apply runs, so a row
// written for it is one those reads miss or one that rolls back under them.
// The tables are compared whole: a row count of zero is only a count. A
// domain with no read index states [Candidate.NoBarrier]; one stating neither
// is refused, since nothing else certifies its barrier. It returns the verdict
// for [Declaration]'s reason.
func ABarrierWritesNoRow(t *testing.T, new Factory) error {
	t.Helper()
	c := new(t)
	name, spec := c.Domain.Name(), c.spec()
	switch {
	case c.EncodeBarrier != nil && c.NoBarrier:
		return fmt.Errorf("%s's candidate supplies a barrier encoder and says its "+
			"log has no barrier — state one", name)
	case c.EncodeBarrier == nil && !c.NoBarrier:
		return fmt.Errorf("%s's candidate supplies no barrier encoder and does not "+
			"say its log has no barrier — a read index's barrier is released on "+
			"before it commits, so what its apply writes is certified here or "+
			"nowhere", name)
	case c.NoBarrier:
		return nil
	}
	if len(c.Kinds) == 0 {
		return fmt.Errorf("%s's candidate names no kind, so the case has no record "+
			"to stamp its barrier like", name)
	}

	// The generation the candidate's records are stamped in, which the read
	// index appends its barrier in: the runner's.
	probe, err := c.Encode(c.Kinds[0], "suite-probe", "suite-probe", 0)
	if err != nil {
		t.Fatalf("encode a %s %s record: %v", name, c.Kinds[0], err)
	}
	stamp, err := c.Domain.Envelope(probe)
	if err != nil {
		t.Fatalf("read the envelope of a %s %s record: %v", name, c.Kinds[0], err)
	}

	node, db := openNodeEstate(t, c)
	log := newSuiteLog()
	signer, err := recordseal.NewSigner(name, suiteRing())
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := recordseal.NewVerifier(name, suiteRing())
	if err != nil {
		t.Fatal(err)
	}
	runner, err := statelog.NewRunner(statelog.RunnerDeps{
		Domain: c.Domain, Spec: spec, Applier: c.Applier, Verifier: verifier,
		Fetch: log, Log: log, Node: node, DB: db,
		Checkpoint: statelog.Position{Stream: spec.Name, Generation: stamp.Gen},
	})
	if err != nil {
		t.Fatalf("build %s's applier: %v", name, err)
	}
	loop := startLoop(t, runner)
	defer loop.stop()

	// The candidate's own records first, so a barrier that updates existing
	// rows, rather than inserting one, is caught too.
	appendRecords(t, c, spec, log, signer, suiteRecords(t, new))
	if err = loop.waitPast(log.last()); err != nil {
		return fmt.Errorf("%s's applier did not apply its own records: %w", name, err)
	}
	before := tableContents(t, db, c.Domain.Tables())

	env := statelog.Envelope{
		V: statelog.BarrierVersion, Kind: statelog.BarrierKind,
		Subject: statelog.Subject{Kind: statelog.BarrierKind}, Gen: stamp.Gen,
		Scope: statelog.ScopeSet{Paths: []string{statelog.FrameworkScope}},
	}
	body, err := c.EncodeBarrier(env)
	if err != nil {
		return fmt.Errorf("%s's barrier encoder refused the read index's envelope: %w",
			name, err)
	}
	seq, _, err := log.Append(t.Context(), subjectOf(spec, env.Subject), "", nil, signer.Seal(body))
	if err != nil {
		t.Fatalf("append %s's barrier: %v", name, err)
	}
	if err := loop.waitPast(seq); err != nil {
		return fmt.Errorf("%s's applier did not apply its barrier: %w — an applier "+
			"with no case for the kind faults every node's copy at the first "+
			"linearizable read", name, err)
	}
	if after := tableContents(t, db, c.Domain.Tables()); !maps.Equal(before, after) {
		return fmt.Errorf("%s's applier changed rows for a barrier:\n%s\na barrier "+
			"writes no row on any node: the reads waiting on it are served the "+
			"moment it is decoded, before its transaction commits, so a row written "+
			"there is one they are served without — or one that rolls back under "+
			"them", name, diffTables(before, after))
	}
	return nil
}
