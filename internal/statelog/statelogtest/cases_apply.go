package statelogtest

import (
	"context"
	"database/sql"
	"fmt"
	"maps"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
)

// runEnvelope certifies the half of a record every build must be able to read.
//
// It is the one part of the deferral contract a domain can break on its own:
// without an envelope there is no position, no kind, no subject and no scope,
// so a record a build cannot decode could not be indexed, probed for or
// reported on — only dropped, which is what turns a rolling upgrade into an
// outage.
func runEnvelope(t *testing.T, new Factory) {
	t.Helper()

	// AN UNKNOWN VERSION MUST STILL DECODE. The caller is the arm that by
	// definition cannot read the payload, so an Envelope that failed here
	// would leave a build with nothing to retain and nothing to say about
	// what it is missing.
	t.Run("a version above this build still yields an envelope", func(t *testing.T) {
		c := new(t)
		if len(c.Kinds) == 0 {
			t.Skip("the candidate declares no kinds for the suite to publish")
		}
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

	// A RECORD THIS BUILD DOES KNOW decodes to what it was encoded as.
	t.Run("a record at this build's version round-trips", func(t *testing.T) {
		c := new(t)
		if len(c.Kinds) == 0 {
			t.Skip("the candidate declares no kinds for the suite to publish")
		}
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

// runApply certifies the two properties every node's copy rests on: the same
// records produce the same rows, and applying one twice is applying it once.
func runApply(t *testing.T, new Factory) {
	t.Helper()

	// APPLY IS A PURE FUNCTION of the rows in this transaction, the record
	// and the options — so two nodes applying one record produce the same
	// rows. This case runs the same records into two fresh estates and
	// compares what they hold, which is the strongest statement that can
	// be made from outside without reading the applier's own source.
	t.Run("the same records produce the same rows", func(t *testing.T) {
		if err := Determinism(t, new); err != nil {
			t.Fatal(err)
		}
	})

	// APPLY IS IDEMPOTENT AT A POSITION. The checkpoint is what makes it
	// so on the ordinary path, and a domain whose apply is not idempotent
	// underneath it produces a node that differs from its peers after any
	// redelivery — which every broker makes eventually.
	t.Run("applying a record twice is applying it once", func(t *testing.T) {
		if err := Idempotency(t, new); err != nil {
			t.Fatal(err)
		}
	})
}

// Determinism runs one set of records into two fresh estates and reports what
// differs.
//
// EXPORTED AND RETURNING THE VERDICT, for the reason [Declaration] is: a case
// that cannot be shown to fail is a claim rather than a check. The *testing.T
// is for the INFRASTRUCTURE — a temporary directory, a context, a store that
// would not open — and those still stop the test; what comes back is the
// contract's own answer.
func Determinism(t *testing.T, new Factory) error {
	t.Helper()
	records := suiteRecords(t, new)
	first := applyInto(t, new, records)
	second := applyInto(t, new, records)
	if maps.Equal(first, second) {
		return nil
	}
	return fmt.Errorf("two estates at one checkpoint hold different rows:\n"+
		"  %v\n  %v\n"+
		"An apply that reads a clock, a config, a node identity or an unsorted "+
		"map is a fleet whose nodes disagree about the same log", first, second)
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
	return fmt.Errorf("applying the same records twice produced different rows:\n"+
		"  once:  %v\n  twice: %v\n"+
		"A redelivery is not an exceptional case — the checkpoint drops one that "+
		"arrives below it, and the applier's own guards have to drop one that "+
		"does not", once, twice)
}

// suiteRecord is one record the suite publishes.
type suiteRecord struct {
	kind string
	id   string
	op   string
}

// suiteRecords is the fixed set every apply case runs, which is what makes
// two runs comparable at all.
func suiteRecords(t *testing.T, new Factory) []suiteRecord {
	t.Helper()
	c := new(t)
	if len(c.Kinds) == 0 {
		t.Skip("the candidate declares no kinds for the suite to publish")
	}
	var out []suiteRecord
	for i, kind := range c.Kinds {
		out = append(out,
			suiteRecord{kind: kind, id: "suite-a", op: "suite-op-a-" + kind},
			suiteRecord{kind: kind, id: "suite-b", op: "suite-op-b-" + kind},
		)
		if i >= 2 {
			break
		}
	}
	return out
}

// applyInto runs records into a fresh estate and returns what every declared
// table holds.
func applyInto(t *testing.T, new Factory, records []suiteRecord) map[string]int {
	t.Helper()
	c := new(t)
	db := openEstate(t, c)

	w, err := db.Replicated().Writer(t.Context())
	if err != nil {
		t.Fatalf("pin a writer: %v", err)
	}
	defer func() { _ = w.Close() }()

	// THE OPTIONS ARE FIXED, which is the point: every value an applier
	// would otherwise read from the world arrives here, so two runs differ only
	// in what the records say.
	opts := statelog.ApplyOptions{
		Now:             time.Unix(1_700_000_000, 0).UTC(),
		StoredAt:        time.Unix(1_700_000_000, 0).UTC(),
		ArbitratedKinds: c.Domain.Stream().ArbitratedKinds,
	}

	for i, r := range records {
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
				Stream:     c.Domain.Stream().Name,
				Generation: 1,
				Seq:        uint64(i + 1),
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
	return countRows(t, db, c.Domain.Tables())
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
