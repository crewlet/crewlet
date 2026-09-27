package chart_test

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/chart"
)

// A SEAT'S KIND IS STRUCTURE: a create_seat states it, a set_kind changes it,
// and a content write neither carries nor touches it.
//
// It was a field of the content record, which put the one fact that decides
// whether anything RUNS a seat in the hands of whoever may edit its backstory,
// and left a hire's seat with no kind at all between its create and its
// content — an agent by omission, which a node would claim.

// A CREATE_SEAT STATES ITS KIND, AND THE SEAT IS THAT KIND FROM THE CREATE.
func TestACreateSeatStatesItsKindAndTheSeatIsItFromTheStart(t *testing.T) {
	t.Parallel()
	r := newWriteRig(t)

	_, err := r.writer.WriteBatch(t.Context(), "op-kindless", chart.Batch{
		Operations: []chart.Operation{{Kind: chart.OpCreateSeat,
			Object: chart.ObjectRef{Kind: chart.KindSeat, ID: "sarah-chen"}}}})
	if ref := refusal(t, err); ref.Rule != chart.RuleSeatKind {
		t.Errorf("a create_seat with no kind: rule = %q, want %q — a default "+
			"would be `agent`, the one kind that runs", ref.Rule, chart.RuleSeatKind)
	}

	r.batch("op-hire", seatOp(chart.SeatHuman, "sarah-chen", ""))
	if got := r.column(`SELECT kind FROM chart_seats WHERE handle = 'sarah-chen'`); !slices.Equal(
		got, []string{"human"}) {
		t.Errorf("the created seat's column says %v, want human", got)
	}
	seat := r.mustSeat("sarah-chen")
	if seat.Kind != chart.SeatHuman {
		t.Errorf("the created seat's document says %q, want human", seat.Kind)
	}
	if seat.HasContent {
		t.Error("a seat nothing has written content to reads as filled")
	}

	// AND ITS CONTENT DOES NOT MAKE IT AN AGENT: the write carries no kind,
	// and the apply keeps the row's.
	if _, err := r.seat("op-content", chart.SeatContent{
		Handle: "sarah-chen", Name: "Sarah Chen"}); err != nil {
		t.Fatalf("write the seat's content: %v", err)
	}
	r.drain()
	seat = r.mustSeat("sarah-chen")
	if seat.Kind != chart.SeatHuman || !seat.HasContent {
		t.Errorf("after its content the seat is %q (filled %v), want a filled "+
			"human seat", seat.Kind, seat.HasContent)
	}
}

// A SET_KIND CHANGES WHAT HOLDS A SEAT, AND IS REFUSED WHERE A PERSON WOULD
// LOSE IT.
//
// A person's seat made an agent's leaves whoever is bound to it signed in as a
// seat a turn loop runs — the same ordinary mistake a removal is refused for,
// and refused the same way, naming them.
func TestASetKindChangesWhatHoldsASeat(t *testing.T) {
	t.Parallel()
	r := newWriteRig(t)
	r.batch("op-hire", seatOp(chart.SeatHuman, "sarah-chen", ""),
		seatOp(chart.SeatAgent, "scribe", ""))

	toAgent := chart.Batch{Operations: []chart.Operation{{Kind: chart.OpSetKind,
		Object:   chart.ObjectRef{Kind: chart.KindSeat, ID: "sarah-chen"},
		SeatKind: chart.SeatAgent}}}
	_, err := r.writer.WithHolders(heldBy{handle: "sarah-chen", login: "sarah.chen"}).
		WriteBatch(t.Context(), "op-held", toAgent)
	if ref := refusal(t, err); ref.Rule != chart.RuleSeatHeld {
		t.Errorf("a held person's seat made an agent's: rule = %q, want %q",
			ref.Rule, chart.RuleSeatHeld)
	}
	_, err = r.writer.WithHolders(nil).WriteBatch(t.Context(), "op-blind", toAgent)
	if ref := refusal(t, err); ref.Rule != chart.RuleDirectoryUnreadable {
		t.Errorf("a node that cannot read the directory: rule = %q, want %q",
			ref.Rule, chart.RuleDirectoryUnreadable)
	}

	// THE OTHER DIRECTION TAKES NOTHING FROM ANYBODY, and asks nobody.
	if _, err := r.writer.WithHolders(nil).WriteBatch(t.Context(), "op-to-human",
		chart.Batch{Operations: []chart.Operation{{Kind: chart.OpSetKind,
			Object:   chart.ObjectRef{Kind: chart.KindSeat, ID: "scribe"},
			SeatKind: chart.SeatHuman}}}); err != nil {
		t.Fatalf("an agent's seat given to a person: %v", err)
	}
	r.drain()
	// AND A SEAT NOBODY HOLDS IS CHANGED.
	if _, err := r.writer.WithHolders(noHolders{}).WriteBatch(t.Context(),
		"op-unheld", toAgent); err != nil {
		t.Fatalf("an unheld person's seat made an agent's: %v", err)
	}
	r.drain()
	if got := r.column(`SELECT handle || '/' || kind FROM chart_seats ORDER BY handle`); !slices.Equal(
		got, []string{"sarah-chen/agent", "scribe/human"}) {
		t.Errorf("the seats are %v, want sarah-chen an agent's and scribe a person's", got)
	}
	if got := r.mustSeat("scribe").Kind; got != chart.SeatHuman {
		t.Errorf("scribe's document says %q, want human", got)
	}
	if got := r.column(`SELECT kind FROM chart_history WHERE id = 'op-to-human'`); !slices.Equal(
		got, []string{string(chart.ChangeKindSet)}) {
		t.Errorf("the change is recorded as %v, want %s", got, chart.ChangeKindSet)
	}
}

// WHAT A SET_KIND MAY NOT BE, each on its own rule.
func TestASetKindIsRefusedWhereItMeansNothing(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.must(create("op-eng", chart.KindUnit, "engineering", ""))
	h.must(create("op-sre", chart.KindSeat, "sre", ""))

	for _, c := range []struct {
		name string
		op   chart.Operation
		rule string
	}{
		{"on a unit", chart.Operation{Kind: chart.OpSetKind,
			Object:   chart.ObjectRef{Kind: chart.KindUnit, ID: "engineering"},
			SeatKind: chart.SeatHuman}, chart.RuleUnknownKind},
		{"with no kind", chart.Operation{Kind: chart.OpSetKind,
			Object: chart.ObjectRef{Kind: chart.KindSeat, ID: "sre"}}, chart.RuleSeatKind},
		{"with a kind this build does not serve", chart.Operation{Kind: chart.OpSetKind,
			Object:   chart.ObjectRef{Kind: chart.KindSeat, ID: "sre"},
			SeatKind: "robot"}, chart.RuleSeatKind},
		{"with a parent", chart.Operation{Kind: chart.OpSetKind,
			Object:   chart.ObjectRef{Kind: chart.KindSeat, ID: "sre"},
			SeatKind: chart.SeatHuman, Parent: "engineering"}, chart.RuleUnusedField},
		{"a seat kind on a move", chart.Operation{Kind: chart.OpMove,
			Object:   chart.ObjectRef{Kind: chart.KindSeat, ID: "sre"},
			SeatKind: chart.SeatHuman, Parent: "engineering"}, chart.RuleUnusedField},
	} {
		_, _, err := h.validate(chart.Batch{Operations: []chart.Operation{c.op}})
		if ref := refusal(t, err); ref.Rule != c.rule {
			t.Errorf("%s: rule = %q, want %q", c.name, ref.Rule, c.rule)
		}
	}
}

// A SEAT'S EDGE CARRIES ITS KIND, whatever else the batch did to it — the edge
// is the seat's whole structural post-state.
func TestASeatsEdgeCarriesItsKind(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.must(create("op-eng", chart.KindUnit, "engineering", ""))
	h.must(place("op-ana", chart.Edge{
		Object: chart.ObjectRef{Kind: chart.KindSeat, ID: "ana"},
		Op:     chart.OpCreateSeat, Kind: chart.SeatHuman}))

	edges, _, err := h.validate(chart.Batch{Operations: []chart.Operation{
		op(chart.OpMove, chart.KindSeat, "ana", "engineering"),
	}})
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if len(edges) != 1 || edges[0].Kind != chart.SeatHuman {
		t.Fatalf("the move published %+v, want ana's edge to say she is a person's "+
			"seat — an edge is full post-state, and a kind read from the "+
			"operation alone would be empty", edges)
	}
	h.must(place("op-move", edges...))
	if got := h.one(`SELECT kind FROM chart_seats WHERE handle = 'ana'`); got != "human" {
		t.Errorf("the moved seat is %q, want human", got)
	}
}

// A VERSION-1 RECORD'S KIND IS APPLIED AS IT WAS MEANT: a version-1 content
// record carries the seat's kind and sets it, and a version-1 placement states
// none and leaves the row's alone.
//
// Nothing rewrites a record on the log, so a replay of one written before the
// kind became structure must still produce the chart it produced then.
func TestAVersionOneRecordsKindIsReadAsItWasMeant(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.must(place("op-hire", chart.Edge{
		Object: chart.ObjectRef{Kind: chart.KindSeat, ID: "ana"},
		Op:     chart.OpCreateSeat, Kind: chart.SeatAgent}))

	h.must(v1(seatRecord("op-old-content", "ana", "", func(p *chart.SeatPayload) {
		p.Kind = chart.SeatHuman
	})))
	if got := h.one(`SELECT kind FROM chart_seats WHERE handle = 'ana'`); got != "human" {
		t.Errorf("a version-1 content record's kind was not applied: %q", got)
	}
	h.must(v1(place("op-old-place", chart.Edge{
		Object: chart.ObjectRef{Kind: chart.KindSeat, ID: "ana"}, Parent: ""})))
	if got := h.one(`SELECT kind FROM chart_seats WHERE handle = 'ana'`); got != "human" {
		t.Errorf("a version-1 placement changed the seat's kind to %q", got)
	}

	// AND A VERSION-2 CONTENT RECORD KEEPS THE ROW'S, whatever it carries:
	// the kind is not its to say.
	h.must(seatRecord("op-new-content", "ana", "", func(p *chart.SeatPayload) {
		p.Kind = chart.SeatAgent
	}))
	if got := h.one(`SELECT kind FROM chart_seats WHERE handle = 'ana'`); got != "human" {
		t.Errorf("a version-2 content record changed the seat's kind to %q", got)
	}
}

// AN IMPORT STATES EVERY SEAT'S KIND, AND NO OPERATION.
//
// The content writes that follow an import carry no kind, so a seat edge
// without one would make every new seat in a revision an agent's whatever the
// document said; and an import decides nothing about which objects exist, so
// an edge carrying a batch's verb is one its author meant something else by.
func TestAnImportStatesEverySeatsKindAndNoOperation(t *testing.T) {
	t.Parallel()
	r := newWriteRig(t)
	seat := chart.ObjectRef{Kind: chart.KindSeat, ID: "ana"}
	for _, c := range []struct {
		name string
		edge chart.Edge
	}{
		{"a seat with no kind", chart.Edge{Object: seat}},
		{"a unit with a kind", chart.Edge{
			Object: chart.ObjectRef{Kind: chart.KindUnit, ID: "eng"},
			Kind:   chart.SeatHuman}},
		{"an edge with a verb", chart.Edge{Object: seat, Kind: chart.SeatHuman,
			Op: chart.OpCreateSeat}},
		{"an edge with a former address", chart.Edge{Object: seat,
			Kind: chart.SeatHuman, From: "anna"}},
	} {
		if _, err := r.writer.WriteImport(t.Context(), "op-"+c.name, "rev-1",
			[]chart.Edge{c.edge}); !errors.Is(err, chart.ErrRefused) {
			t.Errorf("%s: err = %v, want a refusal", c.name, err)
		}
	}

	// THE CONTROL: a seat edge stating its kind lands as that kind.
	r.mustImport("op-import", "rev-2", chart.Edge{Object: seat, Kind: chart.SeatHuman})
	if got := r.mustSeat("ana").Kind; got != chart.SeatHuman {
		t.Errorf("the imported seat is %q, want human", got)
	}
}

// A STUB AN EARLIER BUILD PLACED KEEPS ITS COLUMN'S KIND.
//
// Such a stub carried an empty kind in its document and `agent` in its column.
// A structural write now writes the column from the document, so reading the
// empty kind through would blank the column of every seat an older build
// placed and nothing has filled since.
func TestAStubAnEarlierBuildPlacedKeepsItsKind(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.must(v1(place("op-stub", chart.Edge{
		Object: chart.ObjectRef{Kind: chart.KindSeat, ID: "ana"}})))
	// THE DOCUMENT AN EARLIER BUILD WROTE, with no kind in it.
	if err := h.db.Replicated().Tx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(),
			`UPDATE chart_seats SET document = json_remove(document, '$.kind')
			 WHERE handle = 'ana'`)
		return err
	}); err != nil {
		t.Fatalf("write the earlier build's document: %v", err)
	}

	h.must(v1(place("op-move", chart.Edge{
		Object: chart.ObjectRef{Kind: chart.KindSeat, ID: "ana"}, Parent: ""})))
	if got := h.one(`SELECT kind FROM chart_seats WHERE handle = 'ana'`); got != "agent" {
		t.Errorf("the stub's column says %q after a placement, want agent", got)
	}
	if got := h.seat("ana").Kind; got != chart.SeatAgent {
		t.Errorf("the stub's document says %q, want agent", got)
	}
}

// earlierBuildStub drops the kind from a seat's document, which is the row an
// earlier build placed: an empty kind in the document and `agent` in the
// column beside it.
func earlierBuildStub(t *testing.T, db interface {
	Tx(ctx context.Context, fn func(*sql.Tx) error) error
}, handle string) {
	t.Helper()
	if err := db.Tx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(),
			`UPDATE chart_seats SET document = json_remove(document, '$.kind')
			 WHERE handle = ?`, handle)
		return err
	}); err != nil {
		t.Fatalf("write the earlier build's document: %v", err)
	}
}

// A STUB AN EARLIER BUILD PLACED IS FILLED BY A CONTENT RECORD, AS AN AGENT'S.
//
// Every reader of a seat row takes the `kind` column where the document names
// none. A content apply read the document alone, so a version-2 content record
// on such a stub wrote its empty kind back into the column — and a node that had
// replayed the placement on a later build held `agent` in both, so the two
// estates parted. The content write's own decide refused the seat outright,
// validating the empty kind as "not a seat kind this build serves", so a seat an
// earlier build placed could never be filled on a node that held it that way.
func TestAStubAnEarlierBuildPlacedIsFilledAsAnAgents(t *testing.T) {
	t.Parallel()

	t.Run("the apply", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		h.must(v1(place("op-stub", chart.Edge{
			Object: chart.ObjectRef{Kind: chart.KindSeat, ID: "ana"}})))
		earlierBuildStub(t, h.db.Replicated(), "ana")

		h.must(seatRecord("op-content", "ana", "", nil))
		if got := h.one(`SELECT kind FROM chart_seats WHERE handle = 'ana'`); got != "agent" {
			t.Errorf("the column says %q after a content record, want agent", got)
		}
		if got := h.seat("ana").Kind; got != chart.SeatAgent {
			t.Errorf("the document says %q after a content record, want agent", got)
		}
	})

	t.Run("the write", func(t *testing.T) {
		t.Parallel()
		r := newWriteRig(t)
		r.batch("op-hire", op(chart.OpCreateSeat, chart.KindSeat, "ana", ""))
		earlierBuildStub(t, r.db.Replicated(), "ana")

		if _, err := r.seat("op-content", chart.SeatContent{
			Handle: "ana", Name: "Ana Lopez"}); err != nil {
			t.Fatalf("a content write on a seat an earlier build placed was "+
				"refused: %v", err)
		}
		if got := r.column(`SELECT kind FROM chart_seats WHERE handle = 'ana'`); !slices.Equal(
			got, []string{"agent"}) {
			t.Errorf("the column says %v after the write, want [agent]", got)
		}
		if got := r.mustSeat("ana"); got.Kind != chart.SeatAgent || got.Name != "Ana Lopez" {
			t.Errorf("the seat reads kind %q named %q, want an agent's named "+
				"Ana Lopez", got.Kind, got.Name)
		}
	})

	// AND BY A RETIRED ADDRESS, which reads the row through the scan for one
	// rather than the direct lookup: the seat the identity directory resolves
	// a binding through is the same seat the chart's own read answers.
	t.Run("a retired address", func(t *testing.T) {
		t.Parallel()
		r := newWriteRig(t)
		r.batch("op-hire", op(chart.OpCreateSeat, chart.KindSeat, "ana", ""))
		r.applySeatRekey("op-rename", "ana-lopez", "ana")
		earlierBuildStub(t, r.db.Replicated(), "ana-lopez")

		for name, read := range map[string]func() (chart.Seat, error){
			"by its identity": func() (chart.Seat, error) {
				return r.reader().SeatByIdentity(t.Context(), "ana", session())
			},
			"by its former handle": func() (chart.Seat, error) {
				got, err := r.reader().Seat(t.Context(), "ana", session())
				return got.Seat, err
			},
		} {
			got, err := read()
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			if got.Kind != chart.SeatAgent {
				t.Errorf("%s the seat reads kind %q, want agent — the column's",
					name, got.Kind)
			}
		}
	})
}
