package chart_test

import (
	"database/sql"
	"encoding/json"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/chart"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// THE APPLIER'S OWN SUITE: one record in, one set of rows out.
//
// The framework's shared suite certifies the CONTRACT — determinism,
// idempotency, the deferral grammar — over every domain in the same cases, and
// it is what catches this domain breaking a rule the framework depends on.
// What it cannot say is which VALUE went wrong, because it knows nothing about
// units, seats or leads. These cases do: each one names an invariant that is
// particular to an org chart and would otherwise be visible only as a
// checksum mismatch between two estates.

var brokerAt = time.Date(2031, 4, 2, 3, 14, 0, 0, time.UTC)

type harness struct {
	t            *testing.T
	db           *store.DB
	applier      *chart.Applier
	maxVariables int
	seq          uint64
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "node.db"),
		store.Options{PinnedWriters: 1})
	if err != nil {
		t.Fatalf("open a store: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close the store: %v", err)
		}
	})
	return &harness{
		t: t, db: db, applier: chart.NewApplier("node-a", nil),
		maxVariables: db.Replicated().Caps().MaxVariables,
	}
}

// apply runs one record at the next position, returning the gate that dropped
// it when one did.
func (h *harness) apply(rec chart.MutationRecord) (rows int, gate statelog.Reason, err error) {
	h.t.Helper()
	h.seq++
	return h.applyAt(rec, h.seq)
}

func (h *harness) applyAt(rec chart.MutationRecord, seq uint64) (
	rows int, gate statelog.Reason, err error) {

	h.t.Helper()
	body, encodeErr := chart.Encode(rec)
	if encodeErr != nil {
		h.t.Fatalf("encode the record: %v", encodeErr)
	}
	record := statelog.Record{
		Envelope: statelog.Envelope{
			V: rec.V, Kind: string(rec.Subject.Kind),
			Subject: statelog.Subject{
				Kind: string(rec.Subject.Kind), ID: rec.Subject.ID,
			},
			Op: string(rec.Op), OpID: rec.OpID, Gen: rec.Gen,
			Writer: rec.Writer, Scope: rec.Scope.Resolve(rec.Subject),
		},
		Position: statelog.Position{
			Stream: "CREWLET_CHART_LOG", Generation: rec.Gen, Seq: seq,
		},
		Payload:  body,
		StoredAt: brokerAt,
	}
	err = h.db.Replicated().Tx(h.t.Context(), func(tx *sql.Tx) error {
		reason, gated, gErr := h.applier.Gated(h.t.Context(), tx, record)
		if gErr != nil {
			return gErr
		}
		if gated {
			gate = reason
			return nil
		}
		n, aErr := h.applier.Apply(h.t.Context(), tx, record,
			statelog.ApplyOptions{
				Now: brokerAt, StoredAt: brokerAt,
				MaxVariables: h.maxVariables,
			})
		rows = n
		return aErr
	})
	return rows, gate, err
}

// must applies one record and fails the test on anything but a clean apply.
func (h *harness) must(rec chart.MutationRecord) int {
	h.t.Helper()
	rows, gate, err := h.apply(rec)
	if err != nil {
		h.t.Fatalf("apply the %s record on %s: %v", rec.Op, rec.Subject, err)
	}
	if gate != "" {
		h.t.Fatalf("the %s record on %s was gated: %s", rec.Op, rec.Subject, gate)
	}
	return rows
}

func (h *harness) count(table string) int {
	h.t.Helper()
	var n int
	if err := h.db.Replicated().Read(h.t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(h.t.Context(),
			`SELECT COUNT(*) FROM `+table).Scan(&n)
	}); err != nil {
		h.t.Fatalf("count %s: %v", table, err)
	}
	return n
}

// column reads one column of every matching row, in the query's own order.
func (h *harness) column(query string, args ...any) []string {
	h.t.Helper()
	var out []string
	if err := h.db.Replicated().Read(h.t.Context(), func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(h.t.Context(), query, args...)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var value string
			if err := rows.Scan(&value); err != nil {
				return err
			}
			out = append(out, value)
		}
		return rows.Err()
	}); err != nil {
		h.t.Fatalf("read %q: %v", query, err)
	}
	return out
}

// one reads one string out of the estate.
func (h *harness) one(query string, args ...any) string {
	h.t.Helper()
	got := h.column(query, args...)
	if len(got) != 1 {
		h.t.Fatalf("%q returned %d rows, want exactly one: %v", query, len(got), got)
	}
	return got[0]
}

// unit reads one unit's stored document back.
func (h *harness) unit(key string) chart.Unit {
	h.t.Helper()
	var document []byte
	if err := h.db.Replicated().Read(h.t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(h.t.Context(),
			`SELECT document FROM chart_units WHERE key = ?`, key).Scan(&document)
	}); err != nil {
		h.t.Fatalf("read unit %s: %v", key, err)
	}
	unit, err := chart.DecodeUnit(document)
	if err != nil {
		h.t.Fatalf("decode unit %s: %v", key, err)
	}
	return unit
}

// --- the records ------------------------------------------------------------ //

func record(subject chart.Subject, op chart.OpKind, opID string, payload any,
	scope chart.ScopeSet) chart.MutationRecord {

	rec := chart.MutationRecord{
		RecordEnvelope: chart.RecordEnvelope{
			V: chart.RecordVersion, OpID: opID, Subject: subject, Op: op,
			CreatedAt: brokerAt, Gen: 1, Writer: "node-a", Scope: scope,
		},
		Actor: "ana", ActorKind: chart.AuthorHuman,
	}
	if payload != nil {
		body, err := json.Marshal(payload)
		if err != nil {
			panic(err)
		}
		rec.Mutation = body
	}
	return rec
}

// place is a structural record over one set of edges.
func place(opID string, edges ...chart.Edge) chart.MutationRecord {
	terms := make([]chart.ScopeTerm, 0, len(edges))
	for _, edge := range edges {
		switch edge.Object.Kind {
		case chart.KindUnit:
			terms = append(terms, chart.ScopeTerm{
				Kind: chart.TermUnit, ID: edge.Object.ID})
		case chart.KindSeat:
			terms = append(terms, chart.ScopeTerm{
				Kind: chart.TermSeat, Unit: edge.Parent, ID: edge.Object.ID})
		}
	}
	return record(chart.TreeSubject(), chart.OpPlace, opID,
		chart.PlacementPayload{V: chart.DocumentVersion, Edges: edges},
		chart.BatchScope(terms))
}

// unitRecord is one unit's content.
func unitRecord(opID, key string, edit func(*chart.UnitPayload)) chart.MutationRecord {
	payload := chart.UnitPayload{V: chart.DocumentVersion, Key: key,
		Name: "The " + key + " team"}
	if edit != nil {
		edit(&payload)
	}
	return record(chart.UnitSubject(key), chart.OpUpsert, opID, payload,
		chart.ScopeSet{Subject: true})
}

// seatRecord is one seat's content.
func seatRecord(opID, handle, unit string, edit func(*chart.SeatPayload)) chart.MutationRecord {
	payload := chart.SeatPayload{V: chart.DocumentVersion, Handle: handle,
		Kind: chart.SeatAgent, Name: "Seat " + handle}
	if edit != nil {
		edit(&payload)
	}
	return record(chart.SeatSubject(handle), chart.OpUpsert, opID, payload,
		chart.ScopeSet{Subject: true, Unit: unit})
}

// --- the cases -------------------------------------------------------------- //

// A CONTENT RECORD AND A STRUCTURAL ONE MEET ON ONE ROW AND NEITHER REVERTS THE
// OTHER.
//
// This is the property that is particular to this domain and to no other: a
// unit has TWO writers on two different subjects, arbitrating separately and
// landing in either order. If the content apply wrote `parent_key` it would
// reparent a unit from a record that never mentioned the tree; if the
// structural apply wrote `name` it would revert a rename from a record that
// never carried one. Either way the estate would be a function of ARRIVAL ORDER
// rather than of the log, and two nodes would hold different charts.
func TestContentAndStructureMeetOnOneRowWithoutRevertingEachOther(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	h.must(place("op-place", chart.Edge{
		Object: chart.ObjectRef{Kind: chart.KindUnit, ID: "platform"},
		Parent: "engineering", Lead: "sarah-chen",
	}))
	h.must(unitRecord("op-content", "platform", func(p *chart.UnitPayload) {
		p.Name = "Platform"
		p.Purpose = "keep the lights on"
	}))

	unit := h.unit("platform")
	if unit.Name != "Platform" || unit.Purpose != "keep the lights on" {
		t.Errorf("the content record did not land: %+v", unit)
	}
	if unit.ParentKey != "engineering" || unit.Lead != "sarah-chen" {
		t.Errorf("the content record reverted the structure: parent %q lead %q, "+
			"want engineering and sarah-chen — a unit's content and its "+
			"placement arbitrate on two subjects and meet on one row",
			unit.ParentKey, unit.Lead)
	}

	// AND THE OTHER ORDER. A second placement must not revert the content
	// that landed between them.
	h.must(place("op-move", chart.Edge{
		Object: chart.ObjectRef{Kind: chart.KindUnit, ID: "platform"},
		Parent: "infrastructure", Lead: "sarah-chen",
	}))
	unit = h.unit("platform")
	if unit.ParentKey != "infrastructure" {
		t.Errorf("the move did not land: parent %q", unit.ParentKey)
	}
	if unit.Name != "Platform" || unit.Purpose != "keep the lights on" {
		t.Errorf("the structural record reverted the content: %+v — a "+
			"placement carries no name and no purpose, so it can only have "+
			"written the zero values it never read", unit)
	}
}

// A STRUCTURAL WRITE STAMPS `scoped_through` AND NEVER `version`.
//
// The record arbitrated on the TREE's subject rather than on the object's own,
// so writing the object's version from here would set an expectation no writer
// on that object's subject could ever satisfy — every later content record
// would be refused, for ever, and the object would be permanently unwritable.
func TestAStructuralWriteLeavesTheObjectsOwnExpectationAlone(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	h.must(unitRecord("op-content", "platform", nil))
	before := h.one(`SELECT version FROM chart_units WHERE key = 'platform'`)

	h.must(place("op-place", chart.Edge{
		Object: chart.ObjectRef{Kind: chart.KindUnit, ID: "platform"},
		Parent: "engineering",
	}))
	after := h.one(`SELECT version FROM chart_units WHERE key = 'platform'`)
	if after != before {
		t.Errorf("a structural record moved the unit's own version from %s to "+
			"%s — the next content write compares against that column, so the "+
			"unit would be unwritable from its own subject for ever",
			before, after)
	}
	scoped := h.one(`SELECT scoped_through FROM chart_units WHERE key = 'platform'`)
	if scoped == "0" {
		t.Error("a structural record left scoped_through at zero, so a read " +
			"barrier cannot tell that this row was written after the position " +
			"it is comparing against")
	}
}

// AN AUTHORED EDGE SET IS FOLDED AND DE-DUPLICATED ON THE WAY IN.
//
// An entry is what a founder TYPED and a row is an ADDRESS: `Platform` and
// `platform` name one unit, so storing both would make the inverse index —
// "who manages this" — answer one seat twice for one edge, and a caller
// counting managers would find two.
//
// WHAT THIS CASE DOES NOT CHECK is the sort that goes with the fold. The
// table's primary key is (manager, target), so SQLite returns this scan in key
// order whatever order the rows went in, and an assertion on the order read
// back would pass with the sort deleted — a test that cannot fail. The sort is
// there for the layer underneath: a b-tree built by inserting one key set in
// two orders splits its pages at different points, so two nodes that reached
// the same rows by different routes would hold different BYTES in a table this
// domain claims is byte-identical. Nothing in Go observes that cheaply, so it
// is stated at [sortedKeys] rather than asserted here.
func TestAnAuthoredEdgeSetIsFoldedAndDeduplicated(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	h.must(seatRecord("op-seat", "sarah-chen", "engineering", func(p *chart.SeatPayload) {
		p.Manages = []string{"zoe", "Platform", "adam", "platform", "", "bob"}
	}))

	got := h.column(
		`SELECT target FROM chart_manages WHERE manager = 'sarah-chen' ORDER BY target`)
	want := []string{"adam", "bob", "platform", "zoe"}
	if !slices.Equal(got, want) {
		t.Errorf("the authored edge set is %v, want %v — one address per edge, "+
			"folded, with the empty entry dropped rather than stored as an "+
			"edge to nothing", got, want)
	}
}

// AND A REWRITE REPLACES THE WHOLE SET RATHER THAN ADDING TO IT.
//
// The record carries the COMPLETE post-state, so an entry absent from it was
// removed — and a write that only inserted would leave a seat managing
// somebody it was explicitly taken off.
func TestRewritingAnEdgeSetRemovesWhatTheRecordNoLongerNames(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	h.must(seatRecord("op-one", "sarah-chen", "engineering", func(p *chart.SeatPayload) {
		p.Manages = []string{"adam", "bob"}
	}))
	h.must(seatRecord("op-two", "sarah-chen", "engineering", func(p *chart.SeatPayload) {
		p.Manages = []string{"bob"}
	}))

	got := h.column(
		`SELECT target FROM chart_manages WHERE manager = 'sarah-chen' ORDER BY target`)
	if !slices.Equal(got, []string{"bob"}) {
		t.Errorf("the edge set is %v, want [bob] — the record is full "+
			"post-state, so an entry it no longer names is one somebody "+
			"deliberately removed", got)
	}
}

// AN UNKNOWN FIELD FROM A NEWER BUILD SURVIVES IN THE DOCUMENT.
//
// A row is read, modified and written back by whichever node applied the
// record, so an older build's apply would strip a newer build's field out of
// the object — permanently, because nothing ever writes it again. The typed
// columns are a projection the apply rewrites; the DOCUMENT is what carries
// what this build does not know.
func TestAnUnknownFieldFromANewerBuildSurvivesInTheDocument(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	h.must(unitRecord("op-one", "platform", nil))

	// A newer build's field, injected the way one would actually arrive:
	// in the stored document, which the next apply reads and writes back.
	if err := h.db.Replicated().Tx(t.Context(), func(tx *sql.Tx) error {
		var document []byte
		if err := tx.QueryRowContext(t.Context(),
			`SELECT document FROM chart_units WHERE key = 'platform'`).
			Scan(&document); err != nil {
			return err
		}
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(document, &raw); err != nil {
			return err
		}
		raw["cost_centre"] = json.RawMessage(`"CC-4471"`)
		grown, err := json.Marshal(raw)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(t.Context(),
			`UPDATE chart_units SET document = ? WHERE key = 'platform'`, grown)
		return err
	}); err != nil {
		t.Fatalf("inject a newer build's field: %v", err)
	}

	h.must(unitRecord("op-two", "platform", func(p *chart.UnitPayload) {
		p.Name = "Platform, renamed"
	}))

	unit := h.unit("platform")
	if unit.Name != "Platform, renamed" {
		t.Fatalf("the second record did not land: %+v", unit)
	}
	got, held := unit.Extra["cost_centre"]
	if !held {
		t.Fatalf("a field this build does not know was dropped by the apply "+
			"that rewrote the row — permanently, because nothing writes it "+
			"again. Carried fields: %v", unit.Extra)
	}
	if string(got) != `"CC-4471"` {
		t.Errorf("the carried field is %s, want \"CC-4471\"", got)
	}
}

// A TOMBSTONE HOLDS THE OBJECT, THE INSTANT, THE RECORD AND THE REASON.
//
// A removal is the one operation here with no inverse: every other record is a
// full post-state under a monotone guard, so a node that applied a stale one is
// repaired by the next record on that object, and nothing ever names a removed
// object again. The row is also what OUTLIVES the record — a removal below the
// trim floor has nothing on the log left to prove it happened.
func TestATombstoneHoldsTheObjectTheRecordAndTheReason(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	h.must(unitRecord("op-content", "platform", nil))
	h.must(record(chart.TreeSubject(), chart.OpRemove, "op-remove",
		chart.RemovePayload{
			V: chart.GateRecordVersion, Reason: "merged into infrastructure",
			Objects: []chart.ObjectRef{{Kind: chart.KindUnit, ID: "platform"}},
		},
		chart.BatchScope([]chart.ScopeTerm{
			{Kind: chart.TermUnit, ID: "platform"},
		})))

	if got := h.count("chart_units"); got != 0 {
		t.Errorf("%d unit rows after a removal, want none", got)
	}
	got := h.column(`
		SELECT object_kind || '/' || object_id || '/' || record_id || '/' ||
		       reason || '/' || (at > 0)
		FROM chart_removed`)
	want := []string{"unit/platform/op-remove/merged into infrastructure/1"}
	if !slices.Equal(got, want) {
		t.Errorf("the tombstone is %v, want %v — it is the one row an operator "+
			"asking why next month has to read", got, want)
	}
}

// AND A RECORD ABOUT A REMOVED OBJECT WRITES NOTHING, FOR EVER.
//
// Without the gate a redelivery months later would write the object straight
// back — on the one node that saw it, in a table that claims identity.
func TestARecordAboutARemovedObjectIsGated(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	h.must(unitRecord("op-content", "platform", nil))
	h.must(record(chart.TreeSubject(), chart.OpRemove, "op-remove",
		chart.RemovePayload{V: chart.GateRecordVersion,
			Objects: []chart.ObjectRef{{Kind: chart.KindUnit, ID: "platform"}}},
		chart.BatchScope([]chart.ScopeTerm{
			{Kind: chart.TermUnit, ID: "platform"},
		})))

	_, gate, err := h.apply(unitRecord("op-late", "platform", nil))
	if err != nil {
		t.Fatalf("apply a record about a removed unit: %v", err)
	}
	if gate != statelog.ReasonDeleted {
		t.Errorf("a record about a removed unit was gated %q, want %q — "+
			"without it, a redelivery writes a unit the company dissolved "+
			"back onto one node", gate, statelog.ReasonDeleted)
	}
	if got := h.count("chart_units"); got != 0 {
		t.Errorf("%d unit rows after a gated record, want none", got)
	}

	// AND A PLACEMENT NAMES ITS OBJECTS IN A PAYLOAD, so the envelope gate
	// cannot answer for it and the refusal is per object inside the apply.
	// This is the half a gate keyed on the subject alone would miss.
	h.must(place("op-late-place", chart.Edge{
		Object: chart.ObjectRef{Kind: chart.KindUnit, ID: "platform"},
		Parent: "engineering",
	}))
	if got := h.count("chart_units"); got != 0 {
		t.Errorf("%d unit rows after a placement naming a removed unit, want "+
			"none — a structural record names its objects inside a payload "+
			"the envelope gate cannot read, so the apply refuses per object", got)
	}
}

// A REDELIVERED RECORD WRITES ONE HISTORY ROW.
//
// The row is keyed on the operation id with ON CONFLICT DO NOTHING, which is
// what makes a redelivery free: written once, and a second delivery of the same
// record writes nothing on every node without any of them comparing positions.
func TestARedeliveredRecordWritesOneHistoryRow(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	h.must(unitRecord("op-one", "platform", nil))
	if got := h.count("chart_history"); got != 1 {
		t.Fatalf("%d history rows after one record, want 1", got)
	}

	// THE SAME RECORD AT THE SAME POSITION, which is what a redelivery is.
	rows, gate, err := h.applyAt(unitRecord("op-one", "platform", nil), h.seq)
	if err != nil {
		t.Fatalf("redelivery: %v", err)
	}
	if gate != "" {
		t.Fatalf("a redelivery was gated: %s", gate)
	}
	if rows != 0 {
		t.Errorf("a redelivered record wrote %d rows, want none", rows)
	}
	if got := h.count("chart_history"); got != 1 {
		t.Errorf("%d history rows after the same record arrived twice, want 1 — "+
			"the entry is keyed on the operation id precisely so a redelivery "+
			"is free", got)
	}
}

// AN IMPORT OF A REVISION THE CHART ALREADY HOLDS IS A NO-OP.
//
// Re-activating an UNCHANGED revision is the credential-rotation gesture and is
// therefore routine. Without the ledger it would rewrite every object in the
// chart and wake everybody a second time; with it, every node reaches the same
// no-op the same way — from a row, rather than from a comparison each node makes
// for itself.
func TestReimportingOneRevisionWritesNothingTheSecondTime(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	importRecord := func(opID string) chart.MutationRecord {
		return record(chart.TreeSubject(), chart.OpImport, opID,
			chart.ImportPayload{V: chart.DocumentVersion, Revision: "rev-7",
				Edges: []chart.Edge{
					{Object: chart.ObjectRef{Kind: chart.KindUnit, ID: "platform"},
						Parent: "engineering"},
					{Object: chart.ObjectRef{Kind: chart.KindSeat, ID: "sarah-chen"},
						Parent: "platform"},
				}},
			chart.BatchScope([]chart.ScopeTerm{
				{Kind: chart.TermUnit, ID: "platform"},
				{Kind: chart.TermSeat, Unit: "platform", ID: "sarah-chen"},
			}))
	}

	if rows := h.must(importRecord("op-import")); rows == 0 {
		t.Fatal("the first import wrote nothing at all")
	}
	if got := h.count("chart_import_ledger"); got != 1 {
		t.Fatalf("%d ledger rows after one import, want 1", got)
	}

	// A SECOND, DISTINCT record naming the same revision — which is what a
	// re-activation publishes: a new operation id, the same structure.
	rows := h.must(importRecord("op-rotate"))
	if rows != 0 {
		t.Errorf("re-importing revision rev-7 wrote %d rows, want none — the "+
			"rotation gesture would otherwise rewrite the whole chart and wake "+
			"every seat in it a second time", rows)
	}
	if got := h.count("chart_history"); got != 1 {
		t.Errorf("%d history rows after a re-import, want 1 — a no-op is not "+
			"an event", got)
	}
}

// A REKEY MOVES EVERY STRUCTURAL REFERENCE AND LEAVES THE AUTHORED ONES ALONE.
//
// The two halves are different in kind. A child's `parent_key` and a seat's
// `unit_key` are STRUCTURE nobody typed, so they follow the key or a read sees
// a child pointing at a unit that no longer answers. A `manages:` entry is what
// somebody WROTE, and the former key goes on resolving — so rewriting it would
// edit a document nobody edited, and the next config apply would write the old
// spelling straight back.
func TestARekeyMovesTheStructureAndNotTheAuthoredText(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	h.must(unitRecord("op-unit", "platform", nil))
	h.must(place("op-place",
		chart.Edge{Object: chart.ObjectRef{Kind: chart.KindUnit, ID: "core"},
			Parent: "platform"},
		chart.Edge{Object: chart.ObjectRef{Kind: chart.KindSeat, ID: "sarah-chen"},
			Parent: "platform"}))
	h.must(seatRecord("op-seat", "ana", "engineering", func(p *chart.SeatPayload) {
		p.Manages = []string{"platform"}
	}))

	h.must(record(chart.RekeySubject("infrastructure"), chart.OpRekey, "op-rekey",
		chart.RekeyPayload{V: chart.DocumentVersion, Key: "infrastructure",
			FormerKey: "platform",
			Object:    chart.ObjectRef{Kind: chart.KindUnit, ID: "infrastructure"}},
		chart.BatchScope([]chart.ScopeTerm{
			{Kind: chart.TermUnit, ID: "infrastructure"},
		})))

	if got := h.column(`SELECT key FROM chart_units ORDER BY key`); !slices.Equal(
		got, []string{"core", "infrastructure"}) {
		t.Errorf("the unit keys are %v, want [core infrastructure]", got)
	}
	unit := h.unit("infrastructure")
	if !slices.Contains(unit.FormerKeys, "platform") {
		t.Errorf("the retired key is not held: %v — a key is pasted into chat "+
			"and typed into manages entries, so one that stopped resolving "+
			"would break every reference anybody had written", unit.FormerKeys)
	}

	if got := h.one(`SELECT parent_key FROM chart_units WHERE key = 'core'`); got != "infrastructure" {
		t.Errorf("the child's parent is %q, want infrastructure — a structural "+
			"reference that did not follow leaves a read pointing at a key "+
			"nothing answers to", got)
	}
	if got := h.one(`SELECT unit_key FROM chart_seats WHERE handle = 'sarah-chen'`); got != "infrastructure" {
		t.Errorf("the seat's unit is %q, want infrastructure", got)
	}
	if got := h.column(`SELECT target FROM chart_manages WHERE manager = 'ana'`); !slices.Equal(
		got, []string{"platform"}) {
		t.Errorf("the authored manages entry is %v, want [platform] — it is "+
			"what somebody wrote and the retired key still resolves, so "+
			"rewriting it would edit a document nobody edited", got)
	}
}

// AN EVICTED NODE'S RECORDS ARE DROPPED, AND A READMISSION TAKES IT BACK.
//
// The gate is "records this node wrote ABOVE this position", and the position
// is the log's own — so every node reaches the same verdict about every record
// with no clock and no coordination read, which is what makes it the fence that
// still holds when coordination cannot be reached at all.
func TestAnEvictedNodesRecordsAreDroppedUntilItIsReadmitted(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	h.must(record(chart.EvictionSubject("node-b"), chart.OpEviction, "op-evict",
		chart.Eviction{V: chart.GateRecordVersion, NodeID: "node-b", By: "ana"},
		chart.ScopeSet{Subject: true}))

	fromNodeB := unitRecord("op-b", "platform", nil)
	fromNodeB.Writer = "node-b"
	_, gate, err := h.apply(fromNodeB)
	if err != nil {
		t.Fatalf("apply an evicted node's record: %v", err)
	}
	if gate != statelog.ReasonEvicted {
		t.Errorf("an evicted node's record was gated %q, want %q",
			gate, statelog.ReasonEvicted)
	}
	if got := h.count("chart_units"); got != 0 {
		t.Errorf("%d unit rows from an evicted node, want none", got)
	}

	h.must(record(chart.EvictionSubject("node-b"), chart.OpEviction, "op-readmit",
		chart.Eviction{V: chart.GateRecordVersion, NodeID: "node-b", Readmit: true},
		chart.ScopeSet{Subject: true}))

	// THE INVERSE COMMIT, not a delete: the eviction's own history
	// survives, so a node evicted, readmitted and evicted again reads
	// correctly rather than as one long absence.
	if got := h.count("chart_evictions"); got != 1 {
		t.Errorf("%d eviction rows after a readmission, want 1 — a readmission "+
			"that deleted the row would lose the fact that it ever happened", got)
	}
	back := unitRecord("op-b2", "platform", nil)
	back.Writer = "node-b"
	h.must(back)
	if got := h.count("chart_units"); got != 1 {
		t.Errorf("%d unit rows after readmission, want 1", got)
	}
}

// A RECORD THAT TOOK ONE ADDRESS AND CLAIMS ANOTHER IS REFUSED.
//
// The broker arbitrated this record on its subject, so a payload naming a
// different object would have contended with the wrong writers — two writers on
// what they each believe is one object, neither of them told. It is the one
// error here worth failing the batch for: every node reaches it identically, and
// the record is malformed rather than merely unreadable.
func TestARecordThatClaimsADifferentAddressIsRefused(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	rec := record(chart.UnitSubject("platform"), chart.OpUpsert, "op-one",
		chart.UnitPayload{V: chart.DocumentVersion, Key: "infrastructure"},
		chart.ScopeSet{Subject: true})
	_, _, err := h.apply(rec)
	if err == nil {
		t.Fatal("a record arbitrated on one unit and naming another applied " +
			"cleanly — it contended with the writers of a different object, " +
			"and neither side was told")
	}
	if got := h.count("chart_units"); got != 0 {
		t.Errorf("%d unit rows from a refused record, want none", got)
	}
}

// A PLACEMENT FOR AN OBJECT THAT DOES NOT EXIST YET CREATES A STUB THE CONTENT
// RECORD THEN FILLS.
//
// An import publishes the structure FIRST and the content follows on each
// object's own subject, so a placement landing before the row it places is the
// ORDINARY case rather than the broken one. The stub's own version is ZERO,
// which is what lets the first content record win its guard.
func TestAPlacementBeforeItsContentLeavesAStubTheContentFills(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	h.must(place("op-place", chart.Edge{
		Object: chart.ObjectRef{Kind: chart.KindUnit, ID: "platform"},
		Parent: "engineering", Lead: "sarah-chen",
	}))
	stub := h.unit("platform")
	if stub.ParentKey != "engineering" || stub.Name != "" {
		t.Fatalf("the stub is %+v, want the placement and no content", stub)
	}
	if got := h.one(`SELECT version FROM chart_units WHERE key = 'platform'`); got != "0" {
		t.Errorf("the stub's own version is %s, want 0 — a stub written at the "+
			"structural record's position would refuse every content record "+
			"published before it", got)
	}

	h.must(unitRecord("op-content", "platform", func(p *chart.UnitPayload) {
		p.Name = "Platform"
	}))
	filled := h.unit("platform")
	if filled.Name != "Platform" {
		t.Errorf("the content did not reach the stub: %+v", filled)
	}
	if filled.ParentKey != "engineering" || filled.Lead != "sarah-chen" {
		t.Errorf("the content overwrote the placement it was filling: %+v", filled)
	}
}

// A UNIT WITH NO LEAD OF ITS OWN HOLDS NO ROW IN THE INVERSE INDEX.
//
// That index is read as "which units does this seat lead", and a row naming
// nobody would answer it for a seat whose handle is the empty string. A unit
// with no authored lead is the ORDINARY case, because inheritance is what fills
// it in — see the organisation model.
func TestAUnitWithNoAuthoredLeadHoldsNoLeadRow(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	h.must(place("op-led", chart.Edge{
		Object: chart.ObjectRef{Kind: chart.KindUnit, ID: "platform"},
		Lead:   "sarah-chen",
	}))
	if got := h.column(`SELECT handle FROM chart_leads WHERE unit_key = 'platform'`); !slices.Equal(
		got, []string{"sarah-chen"}) {
		t.Fatalf("the lead edge is %v, want [sarah-chen]", got)
	}

	h.must(place("op-unled", chart.Edge{
		Object: chart.ObjectRef{Kind: chart.KindUnit, ID: "platform"},
	}))
	if got := h.count("chart_leads"); got != 0 {
		t.Errorf("%d lead rows after the lead was cleared, want none — an "+
			"empty handle in the inverse index answers \"which units does "+
			"nobody lead\", which is a question no caller asks", got)
	}
}

// seatRekey is a rekey record moving one seat onto a new handle.
func seatRekey(opID, handle, former string) chart.MutationRecord {
	return record(chart.RekeySubject(handle), chart.OpRekey, opID,
		chart.RekeyPayload{V: chart.DocumentVersion, Key: handle,
			FormerKey: former,
			Object:    chart.ObjectRef{Kind: chart.KindSeat, ID: handle}},
		chart.BatchScope([]chart.ScopeTerm{{Kind: chart.TermSeat, ID: handle}}))
}

// A CREATION THE DECIDE COULD NOT SEE IS DECLINED BY THE APPLY, and so is a
// rename onto a removed address.
//
// A content record can create a seat, and a rename arbitrates on the address's
// own subject rather than the structure's — so a node whose view lagged the log
// can decide either against a snapshot in which a renamed seat's identity, or a
// removed seat's address, looked free. Applied, the first is a second seat with
// the first one's identity; the second is a seat whose every later record the
// removal gate drops. Declined — not raised, because every node reaches the
// same verdict and an apply error would stall the domain on all of them.
func TestTheApplyDeclinesAnIdentityOrARemovedAddressItIsHandedAnyway(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.must(seatRecord("op-founder", "founder", "", nil))
	h.must(seatRecord("op-lena", "lena", "", nil))
	h.must(seatRekey("op-rename", "dana", "founder"))

	// ONTO THE RENAMED SEAT'S IDENTITY: nothing is created.
	h.must(seatRecord("op-stranger", "founder", "", nil))
	if got := h.column(`SELECT handle FROM chart_seats ORDER BY handle`); !slices.Equal(
		got, []string{"dana", "lena"}) {
		t.Errorf("the seats are %v, want [dana lena] — a content record created "+
			"a second seat with dana's identity", got)
	}

	// ONTO A REMOVED ADDRESS: the rename is dropped.
	h.must(record(chart.TreeSubject(), chart.OpRemove, "op-remove",
		chart.RemovePayload{V: chart.GateRecordVersion, Reason: "left",
			Objects: []chart.ObjectRef{{Kind: chart.KindSeat, ID: "dana"}}},
		chart.BatchScope([]chart.ScopeTerm{{Kind: chart.TermSeat, ID: "dana"}})))
	h.must(seatRekey("op-take", "dana", "lena"))
	if got := h.column(`SELECT handle FROM chart_seats ORDER BY handle`); !slices.Equal(
		got, []string{"lena"}) {
		t.Errorf("the seats are %v, want [lena] — a rename landed on a removed "+
			"address, where the removal gate drops every record about it", got)
	}

	// THE CONTROL: a content record on a free handle still creates, and a
	// rename onto a free address still lands.
	h.must(seatRecord("op-new", "omar", "", nil))
	h.must(seatRekey("op-free", "lena-ops", "lena"))
	if got := h.column(`SELECT handle FROM chart_seats ORDER BY handle`); !slices.Equal(
		got, []string{"lena-ops", "omar"}) {
		t.Errorf("the seats are %v, want [lena-ops omar]", got)
	}
}

// A REMOVAL TOMBSTONES THE ADDRESS THE SEAT WAS CREATED UNDER, beside the one it
// held — under the same record, so the gate's one exception covers both.
func TestARemovalTombstonesTheIdentityBesideTheAddress(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.must(seatRecord("op-omar", "omar", "", nil))
	h.must(seatRekey("op-rename", "ops-head", "omar"))
	h.must(record(chart.TreeSubject(), chart.OpRemove, "op-remove",
		chart.RemovePayload{V: chart.GateRecordVersion, Reason: "left",
			Objects: []chart.ObjectRef{{Kind: chart.KindSeat, ID: "ops-head"}}},
		chart.BatchScope([]chart.ScopeTerm{{Kind: chart.TermSeat, ID: "ops-head"}})))

	got := h.column(`SELECT object_id || '/' || record_id FROM chart_removed
		ORDER BY object_id`)
	if want := []string{"omar/op-remove", "ops-head/op-remove"}; !slices.Equal(got, want) {
		t.Errorf("the tombstones are %v, want %v", got, want)
	}
}
