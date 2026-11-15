package statelogtest

import (
	"errors"
	"fmt"
	"maps"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/recordseal"
	"github.com/crewlet/crewlet/internal/statelog"
)

// runInstant reports what [BrokerInstant] found.
func runInstant(t *testing.T, new Factory) {
	t.Helper()
	t.Run("a record's broker instant writes one value on every path", func(t *testing.T) {
		if err := BrokerInstant(t, new); err != nil {
			t.Fatal(err)
		}
	})
}

// BrokerInstant certifies that the domain's Replicated tables hold the same
// bytes whichever way a record reached its applier: applied as it arrived on a
// node east of UTC, applied as it arrived on a node west of it, and held back
// on a third node and applied later. Each node runs the framework's own runner
// over the domain's own applier, on a log whose broker instants carry a
// sub-microsecond remainder.
//
// # Why that is a contract a domain must keep
//
// Because the broker's instant is the one instant an applier may write of its
// own, and it reaches the framework in two shapes: as the broker client reports
// it — nanoseconds, in the node's own zone — on a record applied as it arrives,
// and in the store's encoding — UTC at a microsecond — on one the deferred
// table kept until a later build or an arriving key could read it. A document
// encodes the precision and the zone it is handed, so a framework that handed
// each applier the shape its path produced had every domain that writes the
// instant into a document hold different bytes for one record on two nodes —
// chat's messages and the knowledge base's pages among them — under a claim
// that two nodes at one checkpoint hold the same. The framework hands every
// applier one value, and statelog's own test is what holds the framework to
// it. This case holds the domain to it as far as the suite's records reach: it
// goes red where they reach a document the instant is written into, and it
// cannot see a domain that reads the instant in the process's own zone, since
// the three nodes share one.
//
// # How a record is held back here
//
// SIGNED UNDER A KEY THE NODE DOES NOT HOLD YET, which every domain retains
// alike and every domain applies once the key arrives — where a record from a
// newer build would need a build this one is not. A record that installs an
// apply gate is left out on every node: one this node cannot authenticate is a
// stop, never a retention, and it is the other records that write documents.
//
// EXPORTED AND RETURNING THE VERDICT, for [Determinism]'s reason.
func BrokerInstant(t *testing.T, new Factory) error {
	t.Helper()
	c := new(t)
	requireKinds(t, c)
	var records []suiteRecord
	for _, r := range suiteRecords(t, new) {
		body, err := c.Encode(r.kind, r.id, r.op, c.Domain.RecordVersion())
		if err != nil {
			t.Fatalf("encode %s/%s: %v", r.kind, r.id, err)
		}
		env, err := c.Domain.Envelope(body)
		if err != nil {
			t.Fatalf("envelope %s/%s: %v", r.kind, r.id, err)
		}
		if !c.Domain.InstallsGate(env) {
			records = append(records, r)
		}
	}
	if len(records) == 0 {
		t.Fatalf("every record the suite publishes for %s installs an apply gate, so "+
			"none can be held back to compare", c.Domain.Name())
	}

	east := time.FixedZone("east", 5*60*60+30*60)
	west := time.FixedZone("west", -7*60*60)
	live := instantNode(t, new, records, east, false)
	elsewhere := instantNode(t, new, records, west, false)
	held := instantNode(t, new, records, east, true)
	// BOTH COMPARISONS, reported together: a framework that hands on the
	// broker's instant as it arrived fails each for its own reason, and a
	// verdict that stopped at the first would hide the second.
	var differ []string
	if !maps.Equal(live, elsewhere) {
		differ = append(differ, fmt.Sprintf("two nodes that applied one log as it "+
			"arrived, in two time zones, hold different rows:\n%s\nThe broker's "+
			"instant arrives in the node's own zone; an applier handed it as it "+
			"arrived writes the zone into every document it dates",
			diffTables(live, elsewhere)))
	}
	if !maps.Equal(live, held) {
		differ = append(differ, fmt.Sprintf("a node that applied the log as it "+
			"arrived and a node that held it back and applied it later hold "+
			"different rows:\n%s\nThe broker's instant arrives at nanoseconds and "+
			"the deferred table keeps a microsecond in UTC; an applier handed each "+
			"path's own writes two documents for one record", diffTables(live, held)))
	}
	if len(differ) == 0 {
		return nil
	}
	return errors.New(strings.Join(differ, "\n"))
}

// instantNode applies records on a fresh node whose broker reports every
// instant in zone, with a sub-microsecond remainder, and answers its Replicated
// tables. A held node first runs a build without the key the records are signed
// under, which retains all of them, then restarts with the key, which applies
// them through the deferred table.
func instantNode(t *testing.T, new Factory, records []suiteRecord, zone *time.Location,
	held bool) map[string]string {

	t.Helper()
	c := new(t)
	name, spec := c.Domain.Name(), c.spec()
	node, db := openNodeEstate(t, c)
	log := newSuiteLog()
	log.stamp = func(seq uint64) time.Time {
		return time.Unix(1_700_000_000, 123_456_789).Add(time.Duration(seq) * time.Second).In(zone)
	}

	// THE KEY THE RECORDS ARE SIGNED UNDER: the suite's own on a live node,
	// and on a held one a key its first build does not hold.
	known := suiteRing()
	later := recordseal.Keyring{ActiveID: "later", Keys: []recordseal.Key{
		{ID: "later", Material: "statelogtest-material-later"},
	}}
	signing := known
	if held {
		signing = later
	}
	signer, err := recordseal.NewSigner(name, signing)
	if err != nil {
		t.Fatal(err)
	}
	appendRecords(t, c, spec, log, signer, records)

	probe, err := c.Encode(c.Kinds[0], "suite-probe", "suite-probe", 0)
	if err != nil {
		t.Fatalf("encode a %s %s record: %v", name, c.Kinds[0], err)
	}
	stamp, err := c.Domain.Envelope(probe)
	if err != nil {
		t.Fatalf("read the envelope of a %s %s record: %v", name, c.Kinds[0], err)
	}
	// ONE CLOCK ON EVERY NODE, so the broker's instant is the only value the
	// three differ in.
	clock := func() time.Time { return time.Unix(1_700_000_000, 0).UTC() }
	run := func(applier statelog.Applier, ring recordseal.Keyring, at statelog.Checkpoint,
		drained func()) *loop {

		verifier, verifyErr := recordseal.NewVerifier(name, ring)
		if verifyErr != nil {
			t.Fatal(verifyErr)
		}
		runner, buildErr := statelog.NewRunner(statelog.RunnerDeps{
			Domain: c.Domain, Spec: spec, Applier: applier, Verifier: verifier,
			Fetch: log, Log: log, Node: node, DB: db, Now: clock,
			Checkpoint: at.At, CheckpointStoredAt: at.StoredAt, OnDrained: drained,
		})
		if buildErr != nil {
			t.Fatalf("build %s's applier: %v", name, buildErr)
		}
		return startLoop(t, runner)
	}

	first := run(c.Applier, known, statelog.Checkpoint{
		At: statelog.Position{Stream: spec.Name, Generation: stamp.Gen},
	}, nil)
	if err = first.waitPast(log.last()); err != nil {
		t.Fatalf("%s's applier did not consume the suite's records: %v", name, err)
	}
	first.stop()
	if !held {
		return tableContents(t, db, replicatedTables(c.Domain))
	}
	if _, retained := first.runner.Deferred(); !retained {
		t.Fatalf("%s's applier retained nothing signed under a key it lacks, so "+
			"there is no record held back to compare", name)
	}

	// THE KEY ARRIVES: the same database under a restarted build that holds
	// it, which applies what the first one retained before its first fetch.
	at, found, err := statelog.CheckpointOf(t.Context(), db, spec.Name)
	if err != nil || !found {
		t.Fatalf("read %s's checkpoint: found %v, %v", name, found, err)
	}
	level := make(chan struct{})
	restarted := run(new(t).Applier, recordseal.Keyring{ActiveID: "suite",
		Keys: append(append([]recordseal.Key(nil), known.Keys...), later.Keys...)}, at,
		func() { close(level) })
	if err = restarted.waitDrained(level); err != nil {
		t.Fatalf("%s's restarted applier: %v", name, err)
	}
	restarted.stop()
	if _, retained := restarted.runner.Deferred(); retained {
		t.Fatalf("%s's applier still holds a record back with the key in hand", name)
	}
	return tableContents(t, db, replicatedTables(c.Domain))
}

// replicatedTables is the domain's tables of the class the identity claim is
// made over: the framework's own ledger and deferral tables, which record this
// node's clock and its path, are of another.
func replicatedTables(d statelog.Domain) map[string]statelog.TableClass {
	out := map[string]statelog.TableClass{}
	for name, class := range d.Tables() {
		if class == statelog.Replicated {
			out[name] = class
		}
	}
	return out
}
