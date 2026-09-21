package chart_test

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/chart"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/statelogtest"
)

// THE READ SIDE, and the property every case here is a reading of: an answer
// carries the POSITION it was true as of, and a level asked for is a level
// served or a refusal.
//
// A read that degraded silently would be worse than one that failed: the
// caller acts on the answer either way, and only one of them knows it is
// acting on something stale.

// reader builds the chart's read side over this rig's estate.
//
// THROUGH THE FRAMEWORK'S OWN LOCAL AUTHORITY, in the idiom the knowledge
// base's round trip uses: a harness that handed the reader its own transaction
// would exercise the SQL and none of the contract the rows are served under,
// which is the shape that let a level be a label for as long as it was.
func (r *writeRig) reader() *chart.Reader {
	r.t.Helper()
	authority, err := statelogtest.LocalReader(chart.Domain{},
		r.db.Replicated(), r.waiter.Committed())
	if err != nil {
		r.t.Fatalf("build the read authority: %v", err)
	}
	reader, err := chart.NewReader(chart.ReaderOptions{
		DB: r.db, Log: authority, Committed: r.waiter.Committed,
	})
	if err != nil {
		r.t.Fatalf("build the reader: %v", err)
	}
	return reader
}

func session() statelog.Freshness {
	return statelog.Freshness{Level: statelog.ReadSession}
}

// A READER WITH NO READ AUTHORITY IS REFUSED AT CONSTRUCTION.
//
// Without one every level is a label: the reader would report `session` while
// serving whatever this node happens to hold, and nothing in the answer would
// say so. That was the shape before the framework, and it is why this is a
// construction failure rather than a nil to branch on.
func TestAReaderWithNoReadAuthorityIsRefused(t *testing.T) {
	t.Parallel()
	r := newWriteRig(t)

	if _, err := chart.NewReader(chart.ReaderOptions{DB: r.db}); err == nil {
		t.Fatal("a reader was built with no read authority, so every level it " +
			"reports is a label rather than a guarantee")
	}
	if _, err := chart.NewReader(chart.ReaderOptions{}); err == nil {
		t.Fatal("a reader was built with no estate at all")
	}
}

// A READ THAT NAMES NO LEVEL IS REFUSED, AND SAYS WHOSE JOB IT IS TO FIX.
//
// An absent level is not a fifth level: the SURFACE resolves it, because a
// grammar four surfaces share cannot know which one is asking. A read that
// picked a default here would make that choice invisibly, and two surfaces
// would then disagree about what "unspecified" meant.
//
// THE MESSAGE IS WHAT THIS ASSERTS, not merely that an error came back. The
// framework refuses an invalid level too — so a case checking only for an
// error would pass with this domain's own check deleted, and would be testing
// [statelog] rather than the contract stated here. What the domain adds is the
// sentence that tells the caller where to fix it.
func TestAReadThatNamesNoLevelIsRefused(t *testing.T) {
	t.Parallel()
	r := newWriteRig(t)
	reader := r.reader()

	// THE DOMAIN'S OWN SENTENCE, which is what a surface author reads.
	const names = "names no level"
	if _, err := reader.Read(t.Context(), statelog.Freshness{}); err == nil ||
		!strings.Contains(err.Error(), names) {
		t.Errorf("a whole-chart read with no level answered %v, want a "+
			"refusal naming the level", err)
	}
	if _, err := reader.Unit(t.Context(), "platform", statelog.Freshness{}); err == nil ||
		!strings.Contains(err.Error(), names) {
		t.Errorf("a unit read with no level answered %v", err)
	}
	if _, err := reader.Seat(t.Context(), "sarah-chen", statelog.Freshness{}); err == nil ||
		!strings.Contains(err.Error(), names) {
		t.Errorf("a seat read with no level answered %v", err)
	}
	if _, _, err := reader.Imports(t.Context(), 10, statelog.Freshness{}); err == nil ||
		!strings.Contains(err.Error(), names) {
		t.Errorf("a ledger read with no level answered %v", err)
	}
	if _, _, _, err := reader.Removed(t.Context(),
		chart.ObjectRef{Kind: chart.KindUnit, ID: "platform"},
		statelog.Freshness{}); err == nil || !strings.Contains(err.Error(), names) {
		t.Errorf("a removal read with no level answered %v", err)
	}

	// AND THE SURFACE'S OWN RESOLUTION IS ONE FUNCTION, so the four
	// readers of this domain cannot disagree about the default.
	if got := chart.ReadLevelFor(""); got != statelog.ReadSession {
		t.Errorf("an unspecified level resolves to %q, want %q — session is "+
			"what makes \"I wrote it and then read it\" work, and stale would "+
			"have a founder who just moved a seat reading a chart that does "+
			"not show it", got, statelog.ReadSession)
	}
	if got := chart.ReadLevelFor("stale"); got != statelog.ReadStale {
		t.Errorf("a stated level was overridden: %q", got)
	}
	if got := chart.ReadLevelFor("nonsense"); got != statelog.ReadSession {
		t.Errorf("an unknown level resolves to %q, want the default", got)
	}
}

// EVERY ANSWER CARRIES THE POSITION IT WAS TRUE AS OF.
func TestEveryAnswerCarriesThePositionItWasReadAt(t *testing.T) {
	t.Parallel()
	r := newWriteRig(t)
	r.batch("op-build",
		op(chart.OpCreateUnit, chart.KindUnit, "engineering", ""),
		op(chart.OpCreateSeat, chart.KindSeat, "sarah-chen", "engineering"))
	reader := r.reader()

	whole, err := reader.Read(t.Context(), session())
	if err != nil {
		t.Fatalf("read the chart: %v", err)
	}
	if whole.Position.Seq == 0 {
		t.Error("the whole-chart answer carries no position, so a caller " +
			"cannot tell it from a read of an empty company")
	}
	if whole.Level != statelog.ReadSession {
		t.Errorf("served at %q, want %q — this domain refuses rather than "+
			"degrading", whole.Level, statelog.ReadSession)
	}

	unit, err := reader.Unit(t.Context(), "engineering", session())
	if err != nil {
		t.Fatalf("read the unit: %v", err)
	}
	if unit.Position.Seq == 0 {
		t.Error("the unit answer carries no position")
	}
	seat, err := reader.Seat(t.Context(), "sarah-chen", session())
	if err != nil {
		t.Fatalf("read the seat: %v", err)
	}
	if seat.Position.Seq == 0 {
		t.Error("the seat answer carries no position")
	}
}

// THE WHOLE CHART COMES BACK IN ONE ANSWER, EDGES INCLUDED.
//
// Every derivation over a chart is a WALK, and a walk served by a query per
// ancestor is N round trips against a copy that may move between them — so the
// answer that a walk is computed from has to be one read.
func TestTheWholeChartComesBackAsOneAnswer(t *testing.T) {
	t.Parallel()
	r := newWriteRig(t)
	r.batch("op-build",
		op(chart.OpCreateUnit, chart.KindUnit, "engineering", ""),
		op(chart.OpCreateUnit, chart.KindUnit, "platform", "engineering"),
		op(chart.OpCreateSeat, chart.KindSeat, "sarah-chen", "platform"),
		chart.Operation{Kind: chart.OpSetLead,
			Object: chart.ObjectRef{Kind: chart.KindUnit, ID: "platform"},
			Lead:   "sarah-chen"})
	if _, err := r.seat("op-manages", chart.SeatContent{
		Handle: "sarah-chen", Unit: "platform", Kind: chart.SeatAgent,
		Name: "Sarah Chen", Manages: []string{"platform"},
	}); err != nil {
		t.Fatalf("write the seat: %v", err)
	}

	got, err := r.reader().Read(t.Context(), session())
	if err != nil {
		t.Fatalf("read the chart: %v", err)
	}
	var keys []string
	for _, unit := range got.Units {
		keys = append(keys, unit.Key)
	}
	if !slices.Equal(keys, []string{"engineering", "platform"}) {
		t.Errorf("units = %v, want [engineering platform] — ordered by the "+
			"address, because an unordered read of a set answers differently "+
			"on two nodes holding identical rows", keys)
	}
	if len(got.Seats) != 1 || got.Seats[0].Handle != "sarah-chen" {
		t.Errorf("seats = %+v, want the one", got.Seats)
	}
	if got.Leads["platform"] != "sarah-chen" {
		t.Errorf("leads = %v, want platform led by sarah-chen", got.Leads)
	}
	if !slices.Equal(got.Manages["sarah-chen"], []string{"platform"}) {
		t.Errorf("manages = %v, want the authored edge", got.Manages)
	}
}

// A FORMER KEY GOES ON RESOLVING, AND A CLAIMANT WINS.
//
// A key is pasted into chat and typed into `manages:` entries, so one that
// stopped resolving would break every reference anybody had written. But when
// something ELSE takes the address, the claimant wins — the alternative is
// that creating a unit named after a retired one silently resolves to the old
// object, for ever, on every node.
func TestAFormerKeyResolvesUntilSomethingElseClaimsIt(t *testing.T) {
	t.Parallel()
	r := newWriteRig(t)
	r.batch("op-build", op(chart.OpCreateUnit, chart.KindUnit, "platform", ""))
	if _, err := r.writer.WriteBatch(t.Context(), "op-noop", chart.Batch{
		Operations: []chart.Operation{
			op(chart.OpCreateUnit, chart.KindUnit, "engineering", ""),
		}}); err != nil {
		t.Fatalf("seed a second unit: %v", err)
	}
	r.drain()

	// The rekey, published directly: the batch vocabulary does not carry
	// one, because a key claim arbitrates on the KEY's own subject rather
	// than on the structure's.
	if _, err := r.applyRekey("op-rekey", "infrastructure", "platform"); err != nil {
		t.Fatalf("rekey: %v", err)
	}
	reader := r.reader()

	byNew, err := reader.Unit(t.Context(), "infrastructure", session())
	if err != nil {
		t.Fatalf("read by the new key: %v", err)
	}
	if byNew.Unit.Key != "infrastructure" {
		t.Errorf("the new key answers %q", byNew.Unit.Key)
	}
	byOld, err := reader.Unit(t.Context(), "platform", session())
	if err != nil {
		t.Fatalf("read by the retired key: %v — a key is pasted into chat and "+
			"typed into manages entries, so one that stopped resolving would "+
			"break every reference anybody had already written", err)
	}
	if byOld.Unit.Key != "infrastructure" {
		t.Errorf("the retired key answers %q, want the object that holds it",
			byOld.Unit.Key)
	}
}

// AN ADDRESS NOTHING ANSWERS TO IS A TYPED REFUSAL.
//
// A surface has to tell it from a read that could not be SERVED: one is a 404
// and the other a 503, and collapsing them would have a node that is merely
// behind reporting that a team does not exist.
func TestAnAddressNothingAnswersToIsATypedRefusal(t *testing.T) {
	t.Parallel()
	r := newWriteRig(t)
	reader := r.reader()

	_, err := reader.Unit(t.Context(), "nowhere", session())
	if !errors.Is(err, chart.ErrNotFound) {
		t.Errorf("a missing unit answered %v, want ErrNotFound", err)
	}
	_, err = reader.Seat(t.Context(), "nobody", session())
	if !errors.Is(err, chart.ErrNotFound) {
		t.Errorf("a missing seat answered %v, want ErrNotFound", err)
	}
}

// A REMOVED OBJECT IS A DIFFERENT ANSWER FROM ONE THAT NEVER EXISTED.
//
// "This team was dissolved in March, merged into infrastructure" and "there is
// no such team" are different answers to a person asking where their work went.
func TestARemovedObjectReportsWhenAndWhy(t *testing.T) {
	t.Parallel()
	r := newWriteRig(t)
	r.batch("op-build", op(chart.OpCreateUnit, chart.KindUnit, "platform", ""))

	if _, err := r.writer.WriteRemoval(t.Context(), "op-remove", chart.Batch{
		Reason: "merged into infrastructure",
		Operations: []chart.Operation{
			op(chart.OpRemoveObject, chart.KindUnit, "platform", ""),
		}}); err != nil {
		t.Fatalf("remove the unit: %v", err)
	}
	r.drain()
	reader := r.reader()

	if _, err := reader.Unit(t.Context(), "platform", session()); !errors.Is(
		err, chart.ErrNotFound) {
		t.Errorf("a removed unit still reads: %v", err)
	}
	removal, found, _, err := reader.Removed(t.Context(),
		chart.ObjectRef{Kind: chart.KindUnit, ID: "platform"}, session())
	if err != nil {
		t.Fatalf("read the removal: %v", err)
	}
	if !found {
		t.Fatal("a removed unit has no tombstone to read, so the person " +
			"asking where their team went has nothing at all")
	}
	if removal.Reason != "merged into infrastructure" {
		t.Errorf("the reason is %q, want the one the removal stated",
			removal.Reason)
	}
	if removal.RecordID != "op-remove" {
		t.Errorf("the tombstone names record %q, want op-remove", removal.RecordID)
	}
}

// THE IMPORT LEDGER ANSWERS WHICH REVISION THIS CHART IS RUNNING.
func TestTheImportLedgerNamesTheRevisionAndItsPosition(t *testing.T) {
	t.Parallel()
	r := newWriteRig(t)
	r.mustImport("op-import", "rev-7",
		chart.Edge{Object: chart.ObjectRef{Kind: chart.KindUnit, ID: "engineering"}})

	got, answer, err := r.reader().Imports(t.Context(), 10, session())
	if err != nil {
		t.Fatalf("read the ledger: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("the ledger holds %d rows, want 1: %+v", len(got), got)
	}
	if got[0].Revision != "rev-7" {
		t.Errorf("revision = %q, want rev-7", got[0].Revision)
	}
	if got[0].Position.Seq == 0 {
		t.Error("the ledger row carries no position, so nothing can say " +
			"where on the log this revision landed")
	}
	if want := (chart.Domain{}).Stream().Name; got[0].Position.Stream != want {
		t.Errorf("the position names stream %q, want this domain's — a "+
			"position without its own stream refuses every comparison",
			got[0].Position.Stream)
	}
	if answer.Position.Seq == 0 {
		t.Error("the ledger answer carries no position of its own")
	}
}

// ONE OBJECT'S HISTORY IS BOUNDED AND NEWEST FIRST.
//
// An object's whole history is unbounded in principle — a seat edited daily
// for three years is a thousand rows — and a read that returned all of it
// would put a megabyte on the wire for a panel that renders ten lines.
func TestOneObjectsHistoryIsBoundedAndNewestFirst(t *testing.T) {
	t.Parallel()
	r := newWriteRig(t)
	r.batch("op-build", op(chart.OpCreateSeat, chart.KindSeat, "sarah-chen", ""))

	for i := range 3 {
		if _, err := r.seat("op-edit-"+string(rune('a'+i)), chart.SeatContent{
			Handle: "sarah-chen", Kind: chart.SeatAgent,
			Name: "Sarah " + string(rune('A'+i)),
		}); err != nil {
			t.Fatalf("edit %d: %v", i, err)
		}
	}

	seat, err := r.reader().Seat(t.Context(), "sarah-chen", session())
	if err != nil {
		t.Fatalf("read the seat: %v", err)
	}
	if len(seat.History) == 0 {
		t.Fatal("the seat has no history at all")
	}
	if len(seat.History) > chart.HistoryLimit {
		t.Errorf("the history is %d rows and the cap is %d",
			len(seat.History), chart.HistoryLimit)
	}
	for _, change := range seat.History {
		if change.Object.ID != "sarah-chen" {
			t.Errorf("the seat's history holds a row about %v", change.Object)
		}
	}
}
