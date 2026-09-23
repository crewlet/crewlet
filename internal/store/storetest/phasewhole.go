package storetest

import (
	"bytes"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/store"
)

// A phase record published cut keeps its whole in parts, under ids derived
// from its own, and these cases are the store's half of that contract: the
// parts read back whole by the record's id, a whole that is not all here is
// named with how much is, and no read that lists, counts or folds events ever
// returns a part.

// cutRecord is a phase record's whole as the engine's writer stores it, cut
// into parts of the given sizes, beside the record's own row saying so.
type cutRecord struct {
	id    uuid.UUID
	whole []byte
	parts []store.EventRecord
	row   store.EventRecord
}

// newCutRecord builds one: a phase record carrying a long result, encoded
// whole, and its parts and cut row built by the store's own row builder.
func newCutRecord(t *testing.T, at time.Time, sizes ...int) cutRecord {
	t.Helper()
	full := types.AgentPhaseCompleted{
		RoleName: "Lead", Agent: "agent-lead", TurnID: "turn-cut", Phase: types.PhaseExecute,
		Model: "claude-sonnet-5", InputTokens: 900, OutputTokens: 100, TotalTokens: 1000,
		ToolExecutions: []types.ToolExecution{{"name": "read_file",
			"result": strings.Repeat("the whole of it ", 400)}},
	}
	env := events.New(full, events.TraceContext{TraceID: "trace-cut", SpanID: "span-cut"})
	env.Timestamp = at
	env.Source = "Lead"
	whole, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	c := cutRecord{id: env.ID, whole: whole}
	offset := 0
	for index, size := range sizes {
		if index == len(sizes)-1 {
			size = len(whole) - offset
		}
		part := events.New(types.AgentPhaseRecordPart{
			RecordID: env.ID.String(), Index: index, Offset: offset,
			WholeBytes: len(whole), Data: whole[offset : offset+size],
		}, events.TraceContext{TraceID: "trace-cut", SpanID: "span-cut"})
		part.ID = types.PhaseRecordPartID(env.ID, index)
		part.Timestamp = at.Add(-time.Millisecond)
		c.parts = append(c.parts, rowFor(t, part))
		offset += size
	}
	cut := full
	cut.ToolExecutions = []types.ToolExecution{{"name": "read_file", "result": "…",
		"result_bytes": len(strings.Repeat("the whole of it ", 400))}}
	cut.WholeBytes, cut.WholeParts = len(whole), len(sizes)
	cutEnv := *env
	cutEnv.Data = &cut
	c.row = rowFor(t, &cutEnv)
	return c
}

// rowFor is the row the engine's writer stores for ev.
func rowFor(t *testing.T, ev *events.Event) store.EventRecord {
	t.Helper()
	rec, stored, err := store.RecordFor(ev)
	if err != nil || !stored {
		t.Fatalf("RecordFor(%s) = stored %v, %v", ev.Type, stored, err)
	}
	return rec
}

// testPhaseRecordWholeFromItsParts: parts of mixed sizes — a part the
// transport refused is published again as two halves — reassemble to the
// whole BYTE FOR BYTE, and a record published whole is its own whole.
func testPhaseRecordWholeFromItsParts(t *testing.T, db *store.DB) {
	log := db.Events()
	c := newCutRecord(t, base, 2000, 1000, 1000, 0)
	for _, rec := range append(c.parts, c.row) {
		write(t, log, rec)
	}
	got, err := log.PhaseRecordWhole(t.Context(), c.id.String())
	if err != nil {
		t.Fatalf("PhaseRecordWhole: %v", err)
	}
	if !bytes.Equal(got.Payload, c.whole) || got.Parts != len(c.parts) {
		t.Fatalf("the whole reads back as %d bytes from %d parts; want the %d bytes the record was, "+
			"from its %d parts", len(got.Payload), got.Parts, len(c.whole), len(c.parts))
	}

	whole := store.EventRecord{
		ID: uuid.NewString(), Type: "agent_phase_completed", Time: base, Category: "system",
		Payload: json.RawMessage(`{"phase":"execute","tool_executions":[{"result":"all of it"}]}`),
	}
	write(t, log, whole)
	own, err := log.PhaseRecordWhole(t.Context(), whole.ID)
	if err != nil || !bytes.Equal(own.Payload, whole.Payload) || own.Parts != 0 {
		t.Fatalf("a record published whole reads back as %q from %d parts (%v); want its own row",
			own.Payload, own.Parts, err)
	}
}

// testPhaseRecordWholeNamesWhatIsMissing: a whole that is not all here is
// answered with a typed error saying how much of it is, and why — never with
// the bytes that are.
func testPhaseRecordWholeNamesWhatIsMissing(t *testing.T, db *store.DB) {
	log := db.Events()
	ctx := t.Context()

	// Its record says every part was published; the middle one is gone.
	kept := newCutRecord(t, base, 2000, 2000, 0)
	for _, rec := range []store.EventRecord{kept.parts[0], kept.parts[2], kept.row} {
		write(t, log, rec)
	}
	_, err := log.PhaseRecordWhole(ctx, kept.id.String())
	var missing *store.MissingWholeError
	if !errors.As(err, &missing) {
		t.Fatalf("a whole missing a part answered %v, want a MissingWholeError", err)
	}
	if missing.FoundBytes != 2000 || missing.Parts != 1 || missing.WholeBytes != len(kept.whole) ||
		!missing.Kept || !missing.Recorded {
		t.Errorf("missing = %+v; want 2000 of %d bytes in 1 part, the record saying all were kept",
			*missing, len(kept.whole))
	}

	// Its record says the whole was never kept.
	never := newCutRecord(t, base.Add(time.Minute), 2000, 0)
	never.row = func() store.EventRecord {
		var body map[string]any
		if err := json.Unmarshal(never.row.Payload, &body); err != nil {
			t.Fatal(err)
		}
		body["whole_parts"] = 0
		raw, _ := json.Marshal(body)
		row := never.row
		row.Payload = raw
		return row
	}()
	write(t, log, never.row)
	if _, err := log.PhaseRecordWhole(ctx, never.id.String()); !errors.As(err, &missing) ||
		missing.Kept || !missing.Recorded || missing.FoundBytes != 0 {
		t.Errorf("a whole never kept answered %v; want a MissingWholeError saying it was never kept", err)
	}

	// No record and no part: nothing by that id.
	if _, err := log.PhaseRecordWhole(ctx, uuid.NewString()); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("an id with nothing behind it answered %v, want ErrNotFound", err)
	}

	// No record row, every part: whatever left the row out — every cut form
	// refused, or the record's own publish or write failing — the whole
	// reads back from its parts alone.
	orphan := newCutRecord(t, base.Add(2*time.Minute), 3000, 0)
	for _, rec := range orphan.parts {
		write(t, log, rec)
	}
	if got, err := log.PhaseRecordWhole(ctx, orphan.id.String()); err != nil || !bytes.Equal(got.Payload, orphan.whole) {
		t.Errorf("parts with no record row read back as %d bytes (%v); want the whole", len(got.Payload), err)
	}

	// No record row, and the parts stop short: nothing here says whether
	// every part was published, and the error names how much is here with
	// neither flag set rather than guessing which.
	partial := newCutRecord(t, base.Add(3*time.Minute), 2000, 0)
	write(t, log, partial.parts[0])
	if _, err := log.PhaseRecordWhole(ctx, partial.id.String()); !errors.As(err, &missing) ||
		missing.Kept || missing.Recorded || missing.FoundBytes != 2000 || missing.Parts != 1 ||
		missing.WholeBytes != len(partial.whole) {
		t.Errorf("parts short of the whole with no record row answered %v; want 2000 of %d "+
			"bytes in 1 part, with no row and no claim that every part was published",
			err, len(partial.whole))
	}
}

// testPhaseRecordWholeRefusesPartsThatDoNotContinueIt: assembled bytes that
// are not the whole are worse than none, since nothing downstream could tell.
func testPhaseRecordWholeRefusesPartsThatDoNotContinueIt(t *testing.T, db *store.DB) {
	log := db.Events()
	c := newCutRecord(t, base, 2000, 0)
	// The second part claims to start a byte early: an overlap.
	var body map[string]any
	if err := json.Unmarshal(c.parts[1].Payload, &body); err != nil {
		t.Fatal(err)
	}
	body["offset"] = 1999
	raw, _ := json.Marshal(body)
	c.parts[1].Payload = raw
	for _, rec := range append(c.parts, c.row) {
		write(t, log, rec)
	}
	_, err := log.PhaseRecordWhole(t.Context(), c.id.String())
	var missing *store.MissingWholeError
	if err == nil || errors.As(err, &missing) || errors.Is(err, store.ErrNotFound) {
		t.Fatalf("overlapping parts answered %v; want the read refused as not contiguous", err)
	}
	if !strings.Contains(err.Error(), "not contiguous") {
		t.Errorf("error = %v, want it to say the parts are not contiguous", err)
	}
}

// testPhaseRecordWholeReadsTheNewestRow: an id names more than one row only
// when it is shared — the key is (event_time, event_id) — and then the read
// answers the newest, as ByID does: here a part written twice, and a whole
// record beside a cut one under the same id.
//
// THE NEWER ROW IS WRITTEN FIRST in both. The read walks an id's rows in the
// order they were written, so a read that kept the last row it walked would
// answer the newest of rows written oldest first — and pass a case that
// wrote them that way.
func testPhaseRecordWholeReadsTheNewestRow(t *testing.T, db *store.DB) {
	log := db.Events()
	ctx := t.Context()
	c := newCutRecord(t, base, 2000, 0)
	// The first part twice, the newer row carrying other bytes of the same
	// length: the whole reads back with the newer row's.
	newerPart := c.parts[0]
	newerPart.Time = base.Add(time.Second)
	var body map[string]any
	if err := json.Unmarshal(newerPart.Payload, &body); err != nil {
		t.Fatal(err)
	}
	newerBytes := bytes.Repeat([]byte("#"), 2000)
	body["data"] = newerBytes
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	newerPart.Payload = raw
	for _, rec := range append([]store.EventRecord{newerPart}, append(c.parts, c.row)...) {
		write(t, log, rec)
	}
	want := append(slices.Clone(newerBytes), c.whole[2000:]...)
	got, err := log.PhaseRecordWhole(ctx, c.id.String())
	if err != nil || !bytes.Equal(got.Payload, want) {
		t.Fatalf("a part written twice reads back as %d bytes (%v); want the whole with the newer "+
			"row's part", len(got.Payload), err)
	}

	// A NEWER ROW UNDER A CUT RECORD'S ID, this one whole: it is the answer,
	// though every part the cut row names is here too.
	cut := newCutRecord(t, base.Add(time.Hour), 2000, 0)
	newer := store.EventRecord{
		ID: cut.id.String(), Type: "agent_phase_completed", Time: base.Add(2 * time.Hour),
		Category: "llm", Payload: json.RawMessage(`{"phase":"execute","response":"the newer row"}`),
	}
	for _, rec := range append([]store.EventRecord{newer}, append(cut.parts, cut.row)...) {
		write(t, log, rec)
	}
	got, err = log.PhaseRecordWhole(ctx, cut.id.String())
	if err != nil || !bytes.Equal(got.Payload, newer.Payload) || got.Parts != 0 {
		t.Fatalf("the record reads back as %d bytes from %d parts (%v); want its newest row, %q",
			len(got.Payload), got.Parts, err, newer.Payload)
	}
}

// testPhaseRecordWholeUnderAnIDNoPartDerivesFrom: a part's id is derived from
// its record's UUID, so a row stating a whole in parts under an id that is not
// one names parts nothing can find. No build of the engine writes one; a row
// that says it anyway is answered as what it is, a record whose parts cannot
// be named — neither a whole not all here, which would blame the parts, nor
// nothing at all, which the row plainly is not.
func testPhaseRecordWholeUnderAnIDNoPartDerivesFrom(t *testing.T, db *store.DB) {
	log := db.Events()
	write(t, log, store.EventRecord{
		ID: "not-a-uuid", Type: "agent_phase_completed", Time: base, Category: "llm",
		Payload: json.RawMessage(`{"phase":"execute","whole_bytes":90000,"whole_parts":2}`),
	})
	_, err := log.PhaseRecordWhole(t.Context(), "not-a-uuid")
	var missing *store.MissingWholeError
	switch {
	case err == nil:
		t.Fatal("a record whose parts cannot be named read back as a whole")
	case errors.As(err, &missing):
		t.Fatalf("answered %v: a whole not all here blames parts for an id no part derives from", err)
	case errors.Is(err, store.ErrNotFound):
		t.Fatalf("answered %v: the record's row is here", err)
	case !strings.Contains(err.Error(), "not-a-uuid") || !strings.Contains(err.Error(), "UUID"):
		t.Errorf("error = %v, want it to name the id and why no part derives from it", err)
	}
	// With no row, an id that is not a UUID names nothing here.
	if _, err := log.PhaseRecordWhole(t.Context(), "nothing-by-this-name"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("an id with no row and no parts answered %v, want ErrNotFound", err)
	}
}

// testPartsAreNeverListed: a part is storage for another row, up to nearly as
// large as one event may be — so no read that lists, counts or folds events
// returns one, whatever it is filtered by, while the record it belongs to reads
// as it always does.
func testPartsAreNeverListed(t *testing.T, db *store.DB) {
	log := db.Events()
	ctx := t.Context()
	c := newCutRecord(t, base, 2000, 0)
	for _, rec := range append(c.parts, c.row) {
		write(t, log, rec)
	}
	partType := types.AgentPhaseRecordPart{}.EventType()
	partIDs := map[string]bool{}
	for _, p := range c.parts {
		partIDs[p.ID] = true
	}
	check := func(read string, rows []store.EventRecord) {
		t.Helper()
		for _, row := range rows {
			if partIDs[row.ID] || row.Type == partType {
				t.Errorf("%s returned a part (%s)", read, row.ID)
			}
		}
	}
	// Unfiltered, and filtered by the one dimension a part's row DOES carry.
	for _, q := range []store.ListQuery{{}, {Type: partType}} {
		rows, err := log.List(ctx, q)
		if err != nil {
			t.Fatal(err)
		}
		check("List", rows)
	}
	if rows, err := log.List(ctx, store.ListQuery{}); err != nil || len(rows) != 1 || rows[0].ID != c.id.String() {
		t.Errorf("the listing holds %v (%v); want the one phase record", ids(rows), err)
	}
	phases, _, err := log.Phases(ctx, "", 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	check("Phases", phases)
	seat, _, err := log.AgentPhases(ctx, "agent-lead", "Lead", nil)
	if err != nil {
		t.Fatal(err)
	}
	check("AgentPhases", seat)
	turn, err := log.Turn(ctx, "turn-cut")
	if err != nil {
		t.Fatal(err)
	}
	check("Turn", turn)
	trace, err := log.Trace(ctx, "trace-cut")
	if err != nil {
		t.Fatal(err)
	}
	check("Trace", trace)

	histogram, err := log.Histogram(ctx, store.HistogramQuery{Bucket: store.BucketDay})
	if err != nil {
		t.Fatal(err)
	}
	if histogram.Total != 1 || histogram.ByCategory[""] != 0 {
		t.Errorf("the histogram counts %d rows (%v by category); want the one phase record",
			histogram.Total, histogram.ByCategory)
	}
	tally, err := log.Tally(ctx, store.TallyQuery{})
	if err != nil {
		t.Fatal(err)
	}
	for _, group := range tally {
		if group.Type == partType {
			t.Errorf("the tally counts %d parts", group.Count)
		}
	}
	turns, _, err := log.Turns(ctx, store.TurnQuery{})
	if err != nil || len(turns) != 1 || turns[0].TurnID != "turn-cut" || turns[0].Phases != 1 ||
		turns[0].TotalTokens != 1000 {
		t.Errorf("turns = %+v (%v); want the one turn, its one phase and its 1000 tokens", turns, err)
	}
	spend, err := log.PhaseTokens(ctx, store.PhaseTokenQuery{})
	if err != nil || len(spend) != 1 || spend[0].TotalTokens != 1000 {
		t.Errorf("spend = %+v (%v); want the phase record's 1000 tokens and nothing for its parts", spend, err)
	}
}
