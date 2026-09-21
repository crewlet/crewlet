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

	// A RECORD THIS BUILD DOES KNOW decodes to what it was encoded as.
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
// THE POSITION IS PART OF THE FIXTURE rather than the loop's index, because
// the idempotency case replays records and a replay is a REDELIVERY: the
// broker hands back the message it already handed over, at the sequence it
// already had. Numbered by the loop instead, a replayed record arrives at a
// sequence the broker never issued, and every domain that records the position
// it applied at then looks non-idempotent for a case the suite invented.
//
// Re-publication under a NEW sequence — the same operation id appended again
// by a publisher that never learned the first one landed — is a real case, and
// it is the FRAMEWORK's: the operation ledger drops it, which this harness does
// not run. It is certified in internal/statelog against the loop that owns it.
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

		// THE PROBED LIMIT, not a fixed one: an applier sizes its
		// multi-row inserts by this, so a suite that left it zero would
		// certify every domain writing one row per statement — the one
		// shape the chunker exists to replace — and agree with itself.
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
				Stream:     c.Domain.Stream().Name,
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
// FATAL, NOT A SKIP. Kinds is what this suite publishes records of, so a
// candidate declaring none is one it cannot certify at all — and certifying
// nothing while reporting a pass is the outcome a conformance suite exists to
// make impossible. It was three copies of a t.Skip, which never fired (every
// shipped domain derives Kinds from an enum, and the meta-test hardcodes one)
// and would have silently hollowed out the apply cases the day one did not.
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
// NAMED RATHER THAN DUMPED. Two whole estates rendered side by side is
// unreadable at any real corpus, and the failure it reports is usually one
// column of one table — a clock, a node id, an unsorted map. So the message
// carries only the tables that actually differ.
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
		// Only reachable when one side holds a table the other does
		// not, which a shared migration makes impossible — so it is
		// reported rather than silently rendering nothing.
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
