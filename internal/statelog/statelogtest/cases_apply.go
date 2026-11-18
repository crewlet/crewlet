package statelogtest

import (
	"context"
	"database/sql"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
)

// runEnvelope certifies the half of a record every build must be able to read:
// without an envelope, a record a build cannot decode has no kind, subject or
// scope to index, probe or report, so it can only be dropped, which turns a
// rolling upgrade into an outage.
func runEnvelope(t *testing.T, new Factory) {
	t.Helper()

	// An unknown version must still decode: the caller is the arm that cannot
	// read the payload, and has nothing to retain without an envelope.
	t.Run("a version above this build still yields an envelope", func(t *testing.T) {
		c := new(t)
		requireKinds(t, c)
		future := c.Domain.RecordVersion() + 7
		body, err := c.Encode(c.Kinds[0], "suite-object", "suite-op", future)
		if err != nil {
			t.Fatalf("a record at version %d could not even be encoded: %v — a "+
				"later build publishes exactly this, and this build has to "+
				"retain it", future, err)
		}
		env, err := c.Domain.Envelope(body)
		if err != nil {
			t.Fatalf("Envelope refused a record at version %d: %v — every field "+
				"the deferral index needs is inside it, so a build that cannot "+
				"read it can only drop the record", future, err)
		}
		if env.V != future {
			t.Errorf("the envelope reports version %d, want %d — an operator "+
				"reads this number to decide which build to run", env.V, future)
		}
		if env.Scope.Empty() {
			t.Error("the envelope declares no scope — an empty scope claims the " +
				"record makes nothing stale, which is the one claim a record no " +
				"build may be able to read cannot make")
		}
		if env.Subject.Kind == "" {
			t.Error("the envelope names no subject kind — the deferral index and " +
				"the anchor both key on it")
		}
	})

	// A record this build knows decodes to what it was encoded as.
	t.Run("a record at this build's version round-trips", func(t *testing.T) {
		c := new(t)
		requireKinds(t, c)
		body, err := c.Encode(c.Kinds[0], "suite-object", "suite-op", c.Domain.RecordVersion())
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		env, err := c.Domain.Envelope(body)
		if err != nil {
			t.Fatalf("Envelope: %v", err)
		}
		if env.OpID != "suite-op" {
			t.Errorf("the envelope carries op id %q, want %q — it is what an "+
				"ambiguous publish is resolved by", env.OpID, "suite-op")
		}
	})
}

// runApply certifies the two properties every data node's copy rests on: the
// same records produce the same rows, and applying one twice is applying it
// once.
func runApply(t *testing.T, new Factory) {
	t.Helper()

	// Apply is a pure function of the transaction's rows, the record and the
	// options, so two nodes applying one record produce the same rows.
	t.Run("the same records produce the same rows", func(t *testing.T) {
		if err := Determinism(t, new); err != nil {
			t.Fatal(err)
		}
	})

	// Apply is idempotent at a position: a domain that is not, beneath the
	// checkpoint, diverges from its peers after any redelivery, which every
	// broker makes eventually.
	t.Run("applying a record twice is applying it once", func(t *testing.T) {
		if err := Idempotency(t, new); err != nil {
			t.Fatal(err)
		}
	})
}

// Determinism runs one set of records into two fresh estates and reports what
// differs.
//
// It returns the verdict for [Declaration]'s reason; t is only for
// infrastructure failures, which still stop the test.
func Determinism(t *testing.T, new Factory) error {
	t.Helper()
	records := suiteRecords(t, new)
	first := applyInto(t, new, records)
	second := applyInto(t, new, records)
	if maps.Equal(first, second) {
		return nil
	}
	return fmt.Errorf("two estates at one checkpoint hold different rows:\n%s\n"+
		"An apply that reads a clock, a config, a node identity or an unsorted "+
		"map is a fleet whose nodes disagree about the same log",
		diffTables(first, second))
}

// Idempotency applies one set of records twice and reports what the second
// pass changed.
func Idempotency(t *testing.T, new Factory) error {
	t.Helper()
	records := suiteRecords(t, new)
	once := applyInto(t, new, records)
	twice := applyInto(t, new, append(append([]suiteRecord{}, records...), records...))
	if maps.Equal(once, twice) {
		return nil
	}
	return fmt.Errorf("applying the same records twice produced different rows:\n%s\n"+
		"A redelivery is not an exceptional case — the checkpoint drops one that "+
		"arrives below it, and the applier's own guards have to drop one that "+
		"does not", diffTables(once, twice))
}

// suiteRecord is one record the suite publishes, at a stated position.
//
// The position is part of the fixture, not the loop's index: the idempotency
// case's replay is a redelivery, at the sequence the broker already issued.
// A fresh sequence would make every domain that records its applied position
// look non-idempotent. Re-publication under a new sequence is the operation
// ledger's case, certified in internal/statelog.
type suiteRecord struct {
	kind string
	id   string
	op   string
	seq  uint64
}

// suiteRecords is the fixed set every apply case runs, which is what makes
// two runs comparable at all.
func suiteRecords(t *testing.T, new Factory) []suiteRecord {
	t.Helper()
	c := new(t)
	requireKinds(t, c)
	var out []suiteRecord
	for i, kind := range c.Kinds {
		out = append(out,
			suiteRecord{kind: kind, id: "suite-a", op: "suite-op-a-" + kind,
				seq: uint64(len(out) + 1)},
			suiteRecord{kind: kind, id: "suite-b", op: "suite-op-b-" + kind,
				seq: uint64(len(out) + 2)},
		)
		if i >= 2 {
			break
		}
	}
	return out
}

// applyInto runs records into a fresh estate and returns what every declared
// table holds.
func applyInto(t *testing.T, new Factory, records []suiteRecord) map[string]string {
	t.Helper()
	c := new(t)
	db := openEstate(t, c)

	w, err := db.Writer(t.Context())
	if err != nil {
		t.Fatalf("pin a writer: %v", err)
	}
	defer func() { _ = w.Close() }()

	// Fixed options: every value an applier would read from the world
	// arrives here, so two runs differ only in what the records say.
	opts := statelog.ApplyOptions{
		Now:             time.Unix(1_700_000_000, 0).UTC(),
		StoredAt:        time.Unix(1_700_000_000, 0).UTC(),
		ArbitratedKinds: c.spec().ArbitratedKinds,

		// The probed limit: appliers size multi-row inserts by it, and
		// zero would certify one row per statement, the shape the chunker
		// replaces.
		MaxVariables: db.Caps().MaxVariables,
	}

	for _, r := range records {
		body, err := c.Encode(r.kind, r.id, r.op, c.Domain.RecordVersion())
		if err != nil {
			t.Fatalf("encode %s/%s: %v", r.kind, r.id, err)
		}
		env, err := c.Domain.Envelope(body)
		if err != nil {
			t.Fatalf("envelope %s/%s: %v", r.kind, r.id, err)
		}
		rec := statelog.Record{
			Envelope: env,
			Position: statelog.Position{
				Stream:     c.spec().Name,
				Generation: 1,
				Seq:        r.seq,
			},
			Payload:  body,
			StoredAt: opts.StoredAt,
		}
		if err := w.Tx(t.Context(), func(tx *sql.Tx) error {
			return applyOne(t.Context(), c, tx, rec, opts)
		}); err != nil {
			t.Fatalf("apply %s/%s at %s: %v", r.kind, r.id, rec.Position, err)
		}
	}
	return tableContents(t, db, c.Domain.Tables())
}

// applyOne runs the candidate's own gate and state machine, which is what the
// framework's loop does for one record.
func applyOne(ctx context.Context, c Candidate, tx *sql.Tx, rec statelog.Record, opts statelog.ApplyOptions) error {
	if _, gated, err := c.Applier.Gated(ctx, tx, rec); err != nil || gated {
		return err
	}
	_, err := c.Applier.Apply(ctx, tx, rec, opts)
	return err
}

// requireKinds refuses a candidate the apply cases cannot exercise.
//
// Fatal, not a skip: Kinds is what the suite publishes, and certifying
// nothing while reporting a pass is what a conformance suite exists to
// prevent.
func requireKinds(t *testing.T, c Candidate) {
	t.Helper()
	if len(c.Kinds) == 0 {
		t.Fatalf("the candidate declares no kinds, so this suite has nothing " +
			"to publish and cannot certify its apply path; Kinds is what a " +
			"domain states it writes")
	}
}

// diffTables names the tables that differ and shows what each side holds.
//
// Only differing tables are shown: two whole estates are unreadable, and the
// failure is usually one column of one table.
func diffTables(a, b map[string]string) string {
	names := make([]string, 0, len(a))
	for name := range a {
		names = append(names, name)
	}
	slices.Sort(names)
	var out strings.Builder
	for _, name := range names {
		if a[name] == b[name] {
			continue
		}
		fmt.Fprintf(&out, "  %s:\n    one:   %s\n    other: %s\n",
			name, oneLine(a[name]), oneLine(b[name]))
	}
	if out.Len() == 0 {
		// One side holds a table the other does not, which a shared
		// migration rules out; reported rather than rendering nothing.
		return "  the two sides declare different tables"
	}
	return strings.TrimRight(out.String(), "\n")
}

// oneLine keeps a difference readable when a table holds many rows.
func oneLine(rendered string) string {
	const cap = 400
	flat := strings.ReplaceAll(rendered, "\n", " | ")
	if len(flat) <= cap {
		return flat
	}
	return flat[:cap] + "… (truncated)"
}
