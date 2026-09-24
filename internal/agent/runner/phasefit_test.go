package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/agent/extension"
	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/subagent"
	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/observe"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/textcut"
)

// publishedPhases runs rec through an emitter over the in-memory queue — which
// refuses what the real broker refuses, at the same ceiling — and returns the
// phase records and the record parts that were accepted.
func publishedPhases(t *testing.T, rec phaseRecord) ([]*types.AgentPhaseCompleted, []*types.AgentPhaseRecordPart) {
	t.Helper()
	return publishedBy(t, func(ctx context.Context, e emitter) { e.completed(ctx, rec) })
}

// publishedBy is publishedPhases for any way an emitter publishes a phase
// record: drive publishes through an emitter over the in-memory queue.
func publishedBy(t *testing.T, drive func(ctx context.Context, e emitter)) ([]*types.AgentPhaseCompleted, []*types.AgentPhaseRecordPart) {
	t.Helper()
	q := memory.New()
	if err := q.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = q.Stop(context.Background()) })
	var mu sync.Mutex
	var records []*types.AgentPhaseCompleted
	var parts []*types.AgentPhaseRecordPart
	q.AddPublishListener(func(_ context.Context, _ string, ev *events.Event) {
		mu.Lock()
		defer mu.Unlock()
		switch data := ev.Data.(type) {
		case *types.AgentPhaseCompleted:
			records = append(records, data)
		case *types.AgentPhaseRecordPart:
			parts = append(parts, data)
		}
	})
	var emu sync.Mutex
	e := emitter{pub: q, turn: Turn{RunID: "tn-1", AgentID: "agent-1"}, role: "Lead",
		tally: &Spend{}, mu: &emu}
	drive(t.Context(), e)
	return records, parts
}

// heavyResult is a phase whose tool results total past one event: three-byte
// characters, so a cut through one would show.
func heavyResult(calls, each int) toolloop.Result {
	big := strings.Repeat("あ", each/3)
	res := toolloop.Result{RoundsUsed: 1, Model: "claude-sonnet-5", InputTokens: 9100, OutputTokens: 420}
	for range calls {
		res.Executions = append(res.Executions, toolloop.Execution{
			Round: 1, Name: "read_page", Args: map[string]any{"id": 1}, Output: big,
		})
	}
	res.Executions = append(res.Executions, toolloop.Execution{
		Round: 1, Name: "slack_post", Args: map[string]any{"channel": "C1"}, Output: "posted",
	})
	return res
}

// reassemble puts parts back together in index order, failing the test on a
// part that does not continue the whole — the same checks a reader of them
// makes.
func reassemble(t *testing.T, parts []*types.AgentPhaseRecordPart) []byte {
	t.Helper()
	sorted := slices.Clone(parts)
	slices.SortFunc(sorted, func(a, b *types.AgentPhaseRecordPart) int { return a.Index - b.Index })
	var whole []byte
	for i, part := range sorted {
		if part.Index != i || part.Offset != len(whole) || len(part.Data) == 0 {
			t.Fatalf("part %d (index %d) starts at %d with %d bytes where the whole continues "+
				"at %d: the parts are not contiguous", i, part.Index, part.Offset, len(part.Data), len(whole))
		}
		whole = append(whole, part.Data...)
	}
	if len(sorted) > 0 && len(whole) != sorted[0].WholeBytes {
		t.Fatalf("the parts reassemble to %d bytes and state a whole of %d", len(whole), sorted[0].WholeBytes)
	}
	return whole
}

// A PHASE RECORD TOO LARGE FOR ONE EVENT IS FIT, NOT DROPPED — AND ITS WHOLE
// IS KEPT.
//
// Past the transport's ceiling an event is refused whole, so a phase whose
// tool calls returned more than the ceiling in total had no record at all —
// the event store and every screen lost exactly the phases that did the most.
// Fit, it keeps every call: the small results whole, the large ones cut to a
// common level, each cut marked and its whole length on its row. And the
// whole of every one of them is published first, as parts the record names.
func TestAPhaseRecordTooLargeForOneEventIsFitAndItsWholeKept(t *testing.T) {
	t.Parallel()
	const heavy = 300
	res := heavyResult(heavy, 42_000)
	// The premise: whole, this record is past the ceiling.
	if heavy*42_000 <= queue.MaxPayloadBytes {
		t.Fatalf("the fixture is %d bytes of results, not past the %d-byte ceiling",
			heavy*42_000, queue.MaxPayloadBytes)
	}

	records, parts := publishedPhases(t, phaseRecord{Phase: phase.Execute, Iteration: 1, Result: res})
	if len(records) != 1 {
		t.Fatalf("%d phase records were published, want the one — fit rather than dropped", len(records))
	}
	rec := records[0]
	if len(rec.ToolExecutions) != heavy+1 {
		t.Fatalf("the record carries %d tool calls, want every one of the %d", len(rec.ToolExecutions), heavy+1)
	}
	small := rec.ToolExecutions[heavy]
	if small["result"] != "posted" {
		t.Errorf("a result far under the level was cut: %q", small["result"])
	}
	if _, marked := small["result_bytes"]; marked {
		t.Error("a whole result is marked as cut")
	}
	big := res.Executions[0].Output
	for i, row := range rec.ToolExecutions[:heavy] {
		result, _ := row["result"].(string)
		if !strings.HasSuffix(result, "…") || !utf8.ValidString(result) {
			t.Fatalf("call %d's cut result is unmarked or broken: …%q", i, result[max(0, len(result)-9):])
		}
		if !strings.HasPrefix(big, strings.TrimSuffix(result, "…")) {
			t.Fatalf("call %d's cut result is not a head of the whole", i)
		}
		if row["result_bytes"] != len(big) {
			t.Fatalf("call %d: result_bytes = %v, want the whole result's %d", i, row["result_bytes"], len(big))
		}
		if row["arguments"] != `{"id":1}` {
			t.Fatalf("call %d's arguments were cut while its results could make the room: %v", i, row["arguments"])
		}
	}
	if !strings.Contains(rec.Notes, "record cut to fit one event") {
		t.Errorf("notes = %q, want the record to say it was fit", rec.Notes)
	}
	if rec.InputTokens != 9100 || rec.OutputTokens != 420 || rec.TotalTokens != 9520 || rec.Model != "claude-sonnet-5" {
		t.Errorf("the cut record's spend = %d/%d/%d on %q, want the phase's own 9100/420/9520 on "+
			"claude-sonnet-5", rec.InputTokens, rec.OutputTokens, rec.TotalTokens, rec.Model)
	}
	raw, err := json.Marshal(events.New(*rec, events.TraceContext{}))
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) > queue.MaxPayloadBytes {
		t.Errorf("the fitted record is %d bytes, past the %d-byte ceiling", len(raw), queue.MaxPayloadBytes)
	}

	// THE WHOLE, IN PARTS, NAMED BY THE RECORD.
	if len(parts) < 2 || rec.WholeParts != len(parts) {
		t.Fatalf("%d parts were published and the record names %d; want the whole of a record past "+
			"one event in two or more, every one of them named", len(parts), rec.WholeParts)
	}
	whole := reassemble(t, parts)
	if rec.WholeBytes != len(whole) {
		t.Errorf("whole_bytes = %d, and the parts reassemble to %d", rec.WholeBytes, len(whole))
	}
	var back events.Event
	if err := json.Unmarshal(whole, &back); err != nil {
		t.Fatalf("the reassembled whole is not an event: %v", err)
	}
	full, ok := back.Data.(*types.AgentPhaseCompleted)
	if !ok || back.ID.String() != parts[0].RecordID {
		t.Fatalf("the whole is a %T with id %s; want the phase record %s", back.Data, back.ID, parts[0].RecordID)
	}
	for i, row := range full.ToolExecutions[:heavy] {
		if row["result"] != big {
			t.Fatalf("call %d in the whole is %d bytes, want its result whole (%d)", i,
				len(fmt.Sprint(row["result"])), len(big))
		}
	}
	if full.WholeParts != 0 || full.WholeBytes != 0 {
		t.Errorf("the whole names a whole of its own (%d parts, %d bytes); it IS the whole",
			full.WholeParts, full.WholeBytes)
	}
}

// ONE TEXT PAST THE CEILING IS FIT TOO, AND THE NOTE SAYING SO FITS WITH IT.
//
// A single tool result larger than one event — one file read, one page body —
// is the plainest way a record gets too large, and the one where the fit lands
// closest to the ceiling: one text cut to the highest level that sheds the
// excess leaves the record a few dozen bytes under it. The note that says the
// record was cut is part of the record, so a note written after the level was
// chosen would push it back over, and the record the fit had saved would be
// refused.
func TestOneTextPastTheCeilingIsFitWithTheNoteSayingSo(t *testing.T) {
	t.Parallel()
	whole := strings.Repeat("x", queue.MaxPayloadBytes+1<<20)
	res := toolloop.Result{RoundsUsed: 1, Executions: []toolloop.Execution{
		{Round: 1, Name: "read_file", Args: map[string]any{"path": "dump.log"}, Output: whole},
	}}
	records, _ := publishedPhases(t, phaseRecord{Phase: phase.Execute, Iteration: 1, Result: res})
	if len(records) != 1 {
		t.Fatalf("%d phase records were published, want the one, fit", len(records))
	}
	row := records[0].ToolExecutions[0]
	result, _ := row["result"].(string)
	if !strings.HasSuffix(result, "…") || row["result_bytes"] != len(whole) {
		t.Errorf("the result is not marked as cut: …%q, result_bytes = %v",
			result[max(0, len(result)-9):], row["result_bytes"])
	}
	if !strings.Contains(records[0].Notes, "record cut to fit one event") {
		t.Errorf("notes = %q, want the record to say it was fit", records[0].Notes)
	}
}

// A PHASE'S ERROR PAST ONE EVENT RIDES WHOLE IN THE PARTS, AND THE RECORD
// CARRIES ITS HEAD.
//
// An error is as long as whatever failed made it. Cut to a fixed bound before
// the record existed, the whole the parts kept would be the cut text and the
// rest of the error would be kept nowhere. Built whole, it is the last text any
// form cuts — every other text goes to its mark and every row is given up
// first — and the parts hold it exactly as the phase returned it. For the
// turn's own phase and for a delegated worker alike, which build their records
// separately.
func TestAPhaseErrorPastOneEventRidesWholeInTheParts(t *testing.T) {
	t.Parallel()
	// Past the ceiling on its own, in a three-byte script, so a cut through a
	// character would show.
	failure := "provider: " + strings.Repeat("失敗した ", queue.MaxPayloadBytes/13+100_000)
	calls := []toolloop.Execution{
		{Round: 1, Name: "read_page", Args: map[string]any{"id": 1}, Output: "the page body"},
		{Round: 1, Name: "slack_post", Args: map[string]any{"channel": "C1"}, Output: "posted"},
	}
	for _, tc := range []struct {
		name  string
		drive func(ctx context.Context, e emitter)
	}{
		{"the turn's own phase", func(ctx context.Context, e emitter) {
			e.completed(ctx, phaseRecord{Phase: phase.Execute, Iteration: 1, Failed: true,
				Err:    errors.New(failure),
				Result: toolloop.Result{RoundsUsed: 1, Text: "partial answer", Executions: calls}})
		}},
		{"a delegated worker", func(ctx context.Context, e emitter) {
			e.subagentCompleted(ctx, subagent.Result{ID: "t1", Status: subagent.StatusFailed,
				Error: failure, Text: "partial answer", Rounds: 1, Executions: calls})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			records, parts := publishedBy(t, tc.drive)
			if len(records) != 1 {
				t.Fatalf("%d phase records were published, want the one, cut", len(records))
			}
			rec := records[0]
			if !rec.Failed {
				t.Error("the record of a failed phase does not say it failed")
			}
			if !strings.HasSuffix(rec.Error, "…") || !utf8.ValidString(rec.Error) ||
				!strings.HasPrefix(failure, strings.TrimSuffix(rec.Error, "…")) || len(rec.Error) >= len(failure) {
				t.Fatalf("the record's error is not a marked head of the failure: %d of %d bytes, ending …%q",
					len(rec.Error), len(failure), rec.Error[max(0, len(rec.Error)-12):])
			}
			// THE ERROR WAS REACHED LAST: every other text went to its mark
			// and every row was given up, counted, before it was touched.
			if rec.Response != "…" {
				t.Errorf("the response is %q; want its mark, since the error was cut", rec.Response)
			}
			if len(rec.ToolExecutions) != 0 || rec.ToolExecutionsOmitted != len(calls) {
				t.Errorf("the record carries %d calls and counts %d omitted; want every one of the %d "+
					"given up, since the error was cut", len(rec.ToolExecutions), rec.ToolExecutionsOmitted,
					len(calls))
			}
			raw, err := json.Marshal(events.New(*rec, events.TraceContext{}))
			if err != nil {
				t.Fatal(err)
			}
			if len(raw) > queue.MaxPayloadBytes {
				t.Errorf("the cut record is %d bytes, past the %d-byte ceiling", len(raw), queue.MaxPayloadBytes)
			}
			// THE WHOLE: the error exactly as the phase returned it.
			if len(parts) < 2 || rec.WholeParts != len(parts) {
				t.Fatalf("%d parts published and the record names %d", len(parts), rec.WholeParts)
			}
			var back events.Event
			if err := json.Unmarshal(reassemble(t, parts), &back); err != nil {
				t.Fatalf("the reassembled whole is not an event: %v", err)
			}
			full, ok := back.Data.(*types.AgentPhaseCompleted)
			if !ok {
				t.Fatalf("the whole is a %T", back.Data)
			}
			if full.Error != failure {
				t.Errorf("the whole's error is %d bytes; want the phase's own %d, whole", len(full.Error), len(failure))
			}
		})
	}
}

// THE FIT LEAVES THE ERROR WHOLE.
//
// It is what says why a failed phase failed, so while cutting the other texts
// can make the room — a tool result, or the prose, each as long as the error —
// the error goes out whole beside them. Were it among the texts a common
// level is taken over, it would have been cut with them.
func TestTheFitLeavesTheErrorWhole(t *testing.T) {
	t.Parallel()
	failure := strings.Repeat("the provider refused: ", (5<<20)/22)
	long := strings.Repeat("x", 5<<20)
	for _, tc := range []struct {
		name string
		res  toolloop.Result
		// other reads the long text back off the published record.
		other func(*types.AgentPhaseCompleted) string
	}{
		{"beside a long tool result",
			toolloop.Result{RoundsUsed: 1, Executions: []toolloop.Execution{
				{Round: 1, Name: "read_file", Args: map[string]any{"path": "dump.log"}, Output: long}}},
			func(rec *types.AgentPhaseCompleted) string {
				s, _ := rec.ToolExecutions[0]["result"].(string)
				return s
			}},
		{"beside a long response",
			toolloop.Result{RoundsUsed: 1, Text: long},
			func(rec *types.AgentPhaseCompleted) string { return rec.Response }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			records, _ := publishedPhases(t, phaseRecord{Phase: phase.Execute, Iteration: 1,
				Failed: true, Err: errors.New(failure), Result: tc.res})
			if len(records) != 1 {
				t.Fatalf("%d phase records were published, want the one, cut", len(records))
			}
			if got := records[0].Error; got != failure {
				t.Errorf("the error went out as %d of its %d bytes while the other long text could "+
					"make the room", len(got), len(failure))
			}
			if other := tc.other(records[0]); !strings.HasSuffix(other, "…") || len(other) >= len(long) {
				t.Errorf("the other long text went out as %d of its %d bytes; want it cut to make the room",
					len(other), len(long))
			}
		})
	}
}

// A RECORD THAT FITS IS PUBLISHED EXACTLY AS IT WAS: nothing is cut and no
// part is published unless the transport refuses the whole.
func TestAPhaseRecordThatFitsIsNotTouched(t *testing.T) {
	t.Parallel()
	res := toolloop.Result{RoundsUsed: 1, Executions: []toolloop.Execution{
		{Round: 1, Name: "read_page", Output: strings.Repeat("x", 100_000)},
	}}
	records, parts := publishedPhases(t, phaseRecord{Phase: phase.Execute, Iteration: 1, Result: res,
		Notes: "missing: jira"})
	if len(records) != 1 {
		t.Fatalf("%d records published", len(records))
	}
	row := records[0].ToolExecutions[0]
	if row["result"] != strings.Repeat("x", 100_000) {
		t.Error("a record that fits had a result cut")
	}
	if _, marked := row["result_bytes"]; marked {
		t.Error("a record that fits carries a cut mark")
	}
	if records[0].Notes != "missing: jira" {
		t.Errorf("notes = %q, want the phase's own", records[0].Notes)
	}
	if len(parts) != 0 || records[0].WholeBytes != 0 || records[0].WholeParts != 0 {
		t.Errorf("a record that went whole has %d parts, whole_bytes %d, whole_parts %d; want none",
			len(parts), records[0].WholeBytes, records[0].WholeParts)
	}
}

// THE LEVEL LEAVES THE MOST OF EVERY TEXT: the longest are cut to one level
// and every text at or under it is left whole.
func TestTheWaterLevelCutsOnlyTheLongest(t *testing.T) {
	t.Parallel()
	const mark = 20
	slots := []*textSlot{
		{text: strings.Repeat("a", 1000), mark: mark},
		{text: strings.Repeat("b", 600), mark: mark},
		{text: strings.Repeat("c", 100), mark: mark},
	}
	// Shedding 500 bytes, each first cut writing its mark beside what it
	// leaves: the two longest go to one level and the short one is untouched.
	level, ok := waterLevel(slots, 500)
	if !ok {
		t.Fatal("a tier with room to shed reported none")
	}
	shed := 0
	for _, s := range slots {
		if short := textcut.Within(s.text, level); s.shortens(short) {
			shed += len(s.text) - len(short) - mark
		}
	}
	if shed < 500 {
		t.Errorf("level %d sheds %d bytes, want at least 500", level, shed)
	}
	if level <= 100 {
		t.Errorf("level %d cuts the 100-byte text too, where the two longest could shed it", level)
	}
	if level >= 600-mark {
		t.Errorf("level %d leaves the 600-byte text whole; it cannot shed 500 from one text", level)
	}
	if _, ok := waterLevel([]*textSlot{{text: "…"}}, 10); ok {
		t.Error("a tier holding nothing but marks reported room to shed")
	}
}

// A TEXT NO LONGER THAN ITS MARK IS NEVER CUT.
//
// On a tool call or a round a cut writes its whole length beside the text it
// leaves, so cutting a short result to "…" puts more bytes on the record than
// it takes off, and says the result was shortened while the record grew. The
// water level offers no such text, and the least form leaves it whole, with
// no `<field>_bytes` beside it.
func TestATextNoLongerThanItsMarkIsNeverCut(t *testing.T) {
	t.Parallel()
	// "the page body" is thirteen bytes; its mark is "…" and a
	// `,"result_bytes":13` entry, twenty-one.
	row := types.ToolExecution{"name": "read_page", "result": "the page body"}
	slot := appendRowSlot(nil, row, "result")[0]
	if want := len(`,"result_bytes":13`); slot.mark != want {
		t.Fatalf("the slot's mark weighs %d bytes; the entry its cut writes is %d", slot.mark, want)
	}
	if _, ok := waterLevel([]*textSlot{slot}, 1); ok {
		t.Error("the water level offered a text its own mark outweighs")
	}

	rec := phaseEvent(toolloop.Result{RoundsUsed: 1, Executions: []toolloop.Execution{
		{Round: 1, Name: "read_page", Args: map[string]any{"id": 1}, Output: "the page body"},
		{Round: 1, Name: "read_file", Args: map[string]any{"path": "a.log"}, Output: strings.Repeat("x", 4000)},
	}})
	env := events.New(rec, events.TraceContext{})
	least, err := (&phaseCutter{env: env, original: env.Data.(*types.AgentPhaseCompleted), whole: 1 << 20,
		kept: wholeKept{parts: 1}}).leastForm()
	if err != nil {
		t.Fatal(err)
	}
	short, long := least.rec.ToolExecutions[0], least.rec.ToolExecutions[1]
	if short["result"] != "the page body" || short["result_bytes"] != nil {
		t.Errorf("the short result went into the least form as %q (result_bytes %v); want it whole and unmarked",
			short["result"], short["result_bytes"])
	}
	if long["result"] != "…" || long["result_bytes"] != 4000 {
		t.Errorf("the long result went into the least form as %.20q (result_bytes %v); want its mark",
			long["result"], long["result_bytes"])
	}
	if least.texts != 1 {
		t.Errorf("the least form counts %d texts shortened; want the one it cut", least.texts)
	}
}

// transport is a publisher that refuses what refuse says to and keeps every
// attempt: its type, its encoded size, the event decoded back as a consumer
// would read it, and the bytes of the first — the whole a record went out as.
type transport struct {
	refuse func(ev *events.Event, raw []byte) error

	mu       sync.Mutex
	attempts []sent
	first    []byte
}

// sent is one attempt a transport saw.
type sent struct {
	typ   string
	bytes int
	err   error
	ev    *events.Event
	// data is how much of the whole a part attempt carried, refused or not;
	// zero on anything but a part.
	data int
}

func (p *transport) Publish(_ context.Context, _ string, ev *events.Event) error {
	raw, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	var refused error
	if p.refuse != nil {
		refused = p.refuse(ev, raw)
	}
	// Decoded as a consumer would read it — every accepted event, and every
	// attempt at a phase record, so a test can say what form each one was.
	var back *events.Event
	if refused == nil || ev.Type == (types.AgentPhaseCompleted{}).EventType() {
		back = new(events.Event)
		if err := json.Unmarshal(raw, back); err != nil {
			return err
		}
	}
	data := 0
	if part, ok := ev.Data.(*types.AgentPhaseRecordPart); ok {
		data = len(part.Data)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.first == nil {
		p.first = raw
	}
	p.attempts = append(p.attempts, sent{typ: ev.Type, bytes: len(raw), err: refused, ev: back, data: data})
	return refused
}

// recordAttempts is every attempt at the phase record, the whole first.
func (p *transport) recordAttempts() []sent {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []sent
	for _, a := range p.attempts {
		if a.typ == (types.AgentPhaseCompleted{}).EventType() {
			out = append(out, a)
		}
	}
	return out
}

// partAttempts is every attempt at a part, refused or not.
func (p *transport) partAttempts() []sent {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []sent
	for _, a := range p.attempts {
		if a.typ == (types.AgentPhaseRecordPart{}).EventType() {
			out = append(out, a)
		}
	}
	return out
}

// encodedPart is what a part carrying n bytes of data weighs as the publisher
// encodes it, with the envelope a part of this package's emitter has.
func encodedPart(t *testing.T, n int) int {
	t.Helper()
	record := uuid.New()
	part := events.New(types.AgentPhaseRecordPart{
		RecordID: record.String(), Index: 1, Offset: n, WholeBytes: 4 * n, Data: make([]byte, n),
	}, events.TraceContext{})
	part.ID = types.PhaseRecordPartID(record, 1)
	raw, err := json.Marshal(part)
	if err != nil {
		t.Fatal(err)
	}
	return len(raw)
}

// accepted is what the transport took: the phase records and the parts.
func (p *transport) accepted() ([]*types.AgentPhaseCompleted, []*types.AgentPhaseRecordPart) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var records []*types.AgentPhaseCompleted
	var parts []*types.AgentPhaseRecordPart
	for _, a := range p.attempts {
		if a.err != nil {
			continue
		}
		switch data := a.ev.Data.(type) {
		case *types.AgentPhaseCompleted:
			records = append(records, data)
		case *types.AgentPhaseRecordPart:
			parts = append(parts, data)
		}
	}
	return records, parts
}

// tooLargeAbove refuses as too large every event encoded past limit — what a
// NATS server whose max_payload is below the contract does to one within it.
func tooLargeAbove(limit int) func(*events.Event, []byte) error {
	return func(_ *events.Event, raw []byte) error {
		if len(raw) > limit {
			return fmt.Errorf("publish: %d bytes, and the server accepts %d: %w", len(raw), limit, queue.ErrTooLarge)
		}
		return nil
	}
}

// assertTheWalkHalves holds the attempts at a record to the walk's rule: after
// a refusal within the ceiling, each form tried is at most half the size
// refused, and after one past it, at most the ceiling — with two exceptions
// that are each still smaller than what was refused: the least form, where no
// form that small carries every row, and the form carrying no row at all,
// which is as small as the record gets.
func assertTheWalkHalves(t *testing.T, pub *transport) {
	t.Helper()
	pub.mu.Lock()
	defer pub.mu.Unlock()
	var records []sent
	for _, a := range pub.attempts {
		if a.typ == (types.AgentPhaseCompleted{}).EventType() {
			records = append(records, a)
		}
	}
	for i := 1; i < len(records); i++ {
		prev, next := records[i-1], records[i]
		target := prev.bytes / 2
		if prev.bytes > queue.MaxPayloadBytes {
			target = queue.MaxPayloadBytes
		}
		if next.bytes <= target {
			continue
		}
		rec, _ := next.ev.Data.(*types.AgentPhaseCompleted)
		least := rec != nil && rec.ToolExecutionsOmitted == 0 && rec.RoundNarrationOmitted == 0 &&
			rec.AbandonedAttemptsOmitted == 0
		for _, row := range rec.ToolExecutions {
			// At its least a result is its mark, or whole and unmarked
			// because it is no longer than its mark: never a head.
			if _, cut := row["result_bytes"]; cut && row["result"] != "…" {
				least = false
			}
		}
		rowless := rec != nil && len(rec.ToolExecutions) == 0 && len(rec.RoundNarration) == 0 &&
			len(rec.AbandonedAttempts) == 0
		if (!least && !rowless) || next.bytes >= prev.bytes {
			t.Fatalf("attempt %d weighs %d bytes after one of %d was refused: past the %d-byte "+
				"target, and neither the least form nor the smallest", i, next.bytes, prev.bytes, target)
		}
	}
}

// emitterOver is an emitter publishing through pub.
func emitterOver(pub queue.Publisher) emitter {
	return emitter{pub: pub, turn: Turn{RunID: "tn-1", AgentID: "agent-1"}, role: "Lead",
		tally: &Spend{}, mu: &sync.Mutex{}}
}

// phaseEvent is the wire record completed() would publish for res.
func phaseEvent(res toolloop.Result) types.AgentPhaseCompleted {
	return types.AgentPhaseCompleted{
		Phase: types.PhaseExecute, Iteration: 1, Model: res.Model,
		ToolExecutions: toolExecutions(res.Executions),
		InputTokens:    res.InputTokens, OutputTokens: res.OutputTokens,
		TotalTokens: res.InputTokens + res.OutputTokens,
		CostUSD:     0.25, Decision: "delivered", Backend: types.BackendNative,
	}
}

// A PART REFUSED BELOW THE CONTRACT IS SPLIT, AND THE WHOLE STILL REASSEMBLES
// BYTE FOR BYTE.
//
// The server a node reaches after a failover can accept less than the
// contract's ceiling. A part it refuses as too large is split in two and each
// half published — and the parts after it go at the size it took — so one
// whole ends up in parts of more than one size, which reassemble because each
// carries its offset. Byte for byte: the whole is the record exactly as the
// transport refused it, not a re-encoding of it.
func TestAPartRefusedBelowTheContractIsSplitAndTheWholeStillReassembles(t *testing.T) {
	t.Parallel()
	// The server shrinks after the first part lands, as after a failover.
	pub := &transport{}
	landed := 0
	pub.refuse = func(ev *events.Event, raw []byte) error {
		if err := tooLargeAbove(queue.MaxPayloadBytes)(ev, raw); err != nil {
			return err
		}
		if landed >= 1 {
			return tooLargeAbove(3<<20)(ev, raw)
		}
		landed++
		return nil
	}
	account := emitterOver(pub).publishPhase(t.Context(), phaseEvent(heavyResult(400, 42_000)))

	records, parts := pub.accepted()
	if len(records) != 1 || account.Form == "" {
		t.Fatalf("%d records published (account %+v); want the record, cut", len(records), account)
	}
	sizes := map[int]bool{}
	for _, part := range parts {
		sizes[len(part.Data)] = true
	}
	if len(sizes) < 2 {
		t.Errorf("the parts are all one size (%v): the refused part was not split", sizes)
	}
	whole := reassemble(t, parts)
	if !bytes.Equal(whole, pub.first) {
		t.Fatalf("the parts reassemble to %d bytes that differ from the %d the transport refused",
			len(whole), len(pub.first))
	}
	if records[0].WholeParts != len(parts) || account.Parts != len(parts) {
		t.Errorf("the record names %d parts and the account %d, of the %d published",
			records[0].WholeParts, account.Parts, len(parts))
	}
}

// A REFUSED PART GOES OUT AS TWO PARTS, WHATEVER ITS LENGTH.
//
// The last part of a whole is whatever is left, so its length can be odd, and
// halving that rounding down leaves a byte over — a third publish, broadcast
// to every node serving the API, to carry one byte.
func TestARefusedPartOfOddLengthGoesOutAsTwoParts(t *testing.T) {
	t.Parallel()
	whole := bytes.Repeat([]byte("x"), 2*phasePartFloor+1)
	half := (len(whole) + 1) / 2
	pub := &transport{refuse: func(ev *events.Event, _ []byte) error {
		if part, ok := ev.Data.(*types.AgentPhaseRecordPart); ok && len(part.Data) > half {
			return fmt.Errorf("publish: a part of %d bytes: %w", len(part.Data), queue.ErrTooLarge)
		}
		return nil
	}}
	env := events.New(types.AgentPhaseCompleted{}, events.TraceContext{})
	kept := emitterOver(pub).publishParts(t.Context(), env, whole)

	_, parts := pub.accepted()
	if kept.parts != 2 || len(parts) != 2 {
		t.Fatalf("a refused part of %d bytes went out as %d parts (%d kept); want its two halves",
			len(whole), len(parts), kept.parts)
	}
	if !bytes.Equal(reassemble(t, parts), whole) {
		t.Error("the two halves do not reassemble to the whole")
	}
}

// A REFUSED PART LARGER THAN THE FLOOR IS ASKED FOR AT THE FLOOR BEFORE THE
// WHOLE IS GIVEN UP.
//
// Halving alone steps over the floor: a part of between one floor and two
// halves to less than one. A server that takes a part AT the floor and refuses
// the part twice its size would then never be asked for one, and the record
// would go out with no whole behind it — so a half below the floor is raised
// to it.
func TestARefusedPartIsTriedAtTheFloorBeforeTheWholeIsGivenUp(t *testing.T) {
	t.Parallel()
	// THE SERVER, between the floor and twice it: it takes a part of exactly
	// the floor, with room for the digits one part's envelope has over
	// another's, and nothing larger.
	pub := &transport{refuse: tooLargeAbove(encodedPart(t, phasePartFloor) + 512)}
	// A WHOLE BETWEEN ONE FLOOR AND TWO, so its first part is refused and
	// halves to below the floor.
	rec := phaseEvent(toolloop.Result{RoundsUsed: 1, Executions: []toolloop.Execution{
		{Round: 1, Name: "read_file", Args: map[string]any{"path": "a.log"}, Output: strings.Repeat("x", 100_000)},
	}})
	account := emitterOver(pub).publishPhase(t.Context(), rec)

	records, parts := pub.accepted()
	whole := len(pub.first)
	if whole <= phasePartFloor || whole >= 2*phasePartFloor {
		t.Fatalf("the whole is %d bytes; the case needs one between the floor (%d) and twice it",
			whole, phasePartFloor)
	}
	if len(records) != 1 {
		t.Fatalf("%d records published (account %+v); want the record, cut", len(records), account)
	}
	if records[0].WholeParts == 0 || records[0].WholeParts != len(parts) || account.Parts != len(parts) {
		t.Fatalf("the record names %d parts, the account %d, and %d landed: a server that takes a "+
			"part at the floor lost the whole (notes: %q)", records[0].WholeParts, account.Parts,
			len(parts), records[0].Notes)
	}
	if !bytes.Equal(reassemble(t, parts), pub.first) {
		t.Error("the parts do not reassemble to the whole the transport refused")
	}
	if len(parts[0].Data) != phasePartFloor {
		t.Errorf("the first part that landed carries %d bytes; want exactly the floor, %d",
			len(parts[0].Data), phasePartFloor)
	}
}

// ONLY A PART REFUSED AT THE FLOOR GIVES THE WHOLE UP, AND NOTHING SMALLER IS
// EVER ASKED FOR.
//
// A server that refuses a part of the floor is not one a smaller part can
// serve at a reasonable cost, so the parts end there: the record goes out
// with no whole behind it and says so — after a part at the floor was asked
// for, and never one below it.
func TestAPartRefusedAtTheFloorGivesTheWholeUp(t *testing.T) {
	t.Parallel()
	pub := &transport{refuse: tooLargeAbove(encodedPart(t, phasePartFloor) - 512)}
	account := emitterOver(pub).publishPhase(t.Context(), phaseEvent(heavyResult(300, 42_000)))

	records, parts := pub.accepted()
	if len(records) != 1 || len(parts) != 0 {
		t.Fatalf("%d records and %d parts published; want the record alone", len(records), len(parts))
	}
	if records[0].WholeParts != 0 || account.Parts != 0 {
		t.Errorf("the record names %d parts (account %d) of a whole that was not kept",
			records[0].WholeParts, account.Parts)
	}
	if !strings.Contains(records[0].Notes, "was not kept") {
		t.Errorf("notes = %q, want them to say the whole was not kept", records[0].Notes)
	}
	smallest := 0
	for _, a := range pub.partAttempts() {
		if smallest == 0 || a.data < smallest {
			smallest = a.data
		}
	}
	if smallest != phasePartFloor {
		t.Errorf("the smallest part asked for carried %d bytes; want exactly the floor, %d — "+
			"asked for no smaller, and not given up before it", smallest, phasePartFloor)
	}
}

// THE RECORD'S WALK STARTS BELOW THE SMALLEST PART THE SERVER REFUSED.
//
// The parts go first, and a part refused as too large is a message the server
// has already said it refuses at that size. The record is measured the same
// way, so asking for it at that size again is a refusal already answered: the
// walk starts at half the smallest part refused, and every form it tries is
// smaller than that part.
func TestTheRecordsWalkStartsBelowTheSmallestPartRefused(t *testing.T) {
	t.Parallel()
	pub := &transport{refuse: tooLargeAbove(3 << 20)}
	account := emitterOver(pub).publishPhase(t.Context(), phaseEvent(heavyResult(300, 42_000)))

	records, _ := pub.accepted()
	if len(records) != 1 || account.Form == "" {
		t.Fatalf("%d records published (account %+v); want the record, cut", len(records), account)
	}
	smallest := 0
	for _, a := range pub.partAttempts() {
		if a.err != nil && (smallest == 0 || a.bytes < smallest) {
			smallest = a.bytes
		}
	}
	if smallest == 0 {
		t.Fatal("the server refused no part; the case needs one it did")
	}
	attempts := pub.recordAttempts()
	if len(attempts) < 2 {
		t.Fatalf("%d attempts at the record; want the whole and at least one form", len(attempts))
	}
	if first := attempts[1]; first.bytes > smallest/2 {
		t.Errorf("the first form tried weighs %d bytes; want at most half the %d-byte part the "+
			"server refused", first.bytes, smallest)
	}
	for i, a := range attempts[1:] {
		if a.bytes >= smallest {
			t.Errorf("form %d weighs %d bytes, at least the %d-byte part the server had already refused",
				i+1, a.bytes, smallest)
		}
	}
	// And the form the parts pointed at is one the server takes: it landed a
	// part at least that large.
	largest := 0
	for _, a := range pub.partAttempts() {
		if a.err == nil {
			largest = max(largest, a.bytes)
		}
	}
	if first := attempts[1]; first.bytes <= largest && first.err != nil {
		t.Errorf("the first form, %d bytes, was refused though the server took a %d-byte part",
			first.bytes, largest)
	}
}

// A FULL PART FITS ONE EVENT, WITH ITS RESERVE TO SPARE.
//
// A part's data is base64 on the wire, so what it costs is a function of its
// length: four bytes for every three, none of them escaped. What is left is
// the envelope — the ids, the trace, three numbers — and the reserve is sized
// against it, so this measures it with every field at its widest.
func TestAFullPartFitsOneEventWithItsReserveToSpare(t *testing.T) {
	t.Parallel()
	record := uuid.New()
	data := bytes.Repeat([]byte{0xff, '<', '"'}, phasePartBytes/3)
	part := events.New(types.AgentPhaseRecordPart{
		RecordID: record.String(), Index: 999_999, Offset: 1 << 40, WholeBytes: 1 << 41, Data: data,
	}, events.NewTrace())
	part.ID = types.PhaseRecordPartID(record, 999_999)
	part.Source = strings.Repeat("r", 200)
	raw, err := json.Marshal(part)
	if err != nil {
		t.Fatal(err)
	}
	envelope := len(raw) - 4*phasePartBytes/3
	if envelope >= 1<<10 {
		t.Errorf("a part's envelope measures %d bytes; the reserve's reason says well under a kilobyte",
			envelope)
	}
	if spare := queue.MaxPayloadBytes - len(raw); spare < phasePartReserve-1<<10 {
		t.Errorf("a full part is %d bytes, leaving %d of the %d-byte ceiling: the reserve is gone",
			len(raw), spare, queue.MaxPayloadBytes)
	}
}

// A RECORD REFUSED WITHIN THE CEILING GOES OUT IN ITS LEAST FORM, WITH ITS
// WHOLE AND ITS SPEND.
//
// Refused although it is within the contract's ceiling, the record was
// refused by a server set below it, and no cut to the contract's number
// answers that. The target is halved on each refusal instead, down through the
// fitted forms to the least one — every text longer than its mark reduced to
// it, every call still carried — and the record goes out carrying its tokens,
// its cost and a reference to its whole, which went first as parts the smaller
// server takes.
func TestARecordRefusedWithinTheCeilingGoesOutInItsLeastFormWithItsWhole(t *testing.T) {
	t.Parallel()
	const calls = 1500
	rec := phaseEvent(heavyResult(calls, 2_000))
	// THE SERVER TAKES THE LEAST FORM AND NOTHING LARGER: its limit is the
	// least form's own size, measured on a cutter built as publishPhase
	// builds one — the same source, the same whole, a part count of the same
	// digits. What still varies is the envelope's timestamp, a few bytes, so
	// the limit allows a few more; every fitted form the walk tries weighs
	// tens of kilobytes above it.
	env := events.New(rec, events.TraceContext{})
	env.Source = "Lead"
	whole, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	least, err := (&phaseCutter{env: env, original: env.Data.(*types.AgentPhaseCompleted),
		whole: len(whole), kept: wholeKept{parts: 10}}).leastForm()
	if err != nil {
		t.Fatal(err)
	}
	if least.bytes < 140<<10 || len(whole) > queue.MaxPayloadBytes {
		t.Fatalf("the fixture's least form is %d bytes and its whole %d: it must be within the "+
			"ceiling, with a least form a part at the floor fits under", least.bytes, len(whole))
	}
	pub := &transport{refuse: tooLargeAbove(least.bytes + 32)}
	account := emitterOver(pub).publishPhase(t.Context(), rec)

	records, parts := pub.accepted()
	if len(records) != 1 || account.Form != phaseLeast {
		t.Fatalf("%d records published in form %q (account %+v); want the least form",
			len(records), account.Form, account)
	}
	got := records[0]
	if len(got.ToolExecutions) != calls+1 || got.ToolExecutionsOmitted != 0 {
		t.Errorf("the least form carries %d calls and counts %d omitted; want every one of the %d carried",
			len(got.ToolExecutions), got.ToolExecutionsOmitted, calls+1)
	}
	for i, row := range got.ToolExecutions[:calls] {
		// A number read back off the wire into a map is a float64.
		if row["result"] != "…" || row["result_bytes"] != float64(2_000/3*3) {
			t.Fatalf("call %d's result is %q (result_bytes %v), want its mark and its whole length",
				i, row["result"], row["result_bytes"])
		}
	}
	if got.InputTokens != 9100 || got.TotalTokens != 9520 || got.CostUSD != 0.25 || got.Decision != "delivered" {
		t.Errorf("the least form's scalars are %d/%d, $%v, %q; want the phase's own",
			got.InputTokens, got.TotalTokens, got.CostUSD, got.Decision)
	}
	if got.WholeParts == 0 || got.WholeParts != len(parts) || account.Parts != len(parts) {
		t.Fatalf("the record names %d parts, the account %d, and %d were published: the whole "+
			"of a record refused within the ceiling must be kept", got.WholeParts, account.Parts, len(parts))
	}
	if whole := reassemble(t, parts); !bytes.Equal(whole, pub.first) {
		t.Errorf("the parts reassemble to %d bytes that differ from the %d refused", len(whole), len(pub.first))
	}
	if !strings.Contains(got.Notes, "is kept in") {
		t.Errorf("notes = %q, want them to say where the whole is", got.Notes)
	}
	assertTheWalkHalves(t, pub)
}

// THE LEAST FORM CARRIES THE PHASE'S ERROR WHOLE.
//
// A failed phase's record in its least form still says why it failed: its
// other texts are at their marks, and the error, a few dozen bytes, goes out
// whole beside every row. Reduced to its mark with them, the record the
// server took would have said the phase failed and not why, for the sake of
// bytes the server had room for.
func TestTheLeastFormCarriesTheErrorWhole(t *testing.T) {
	t.Parallel()
	const failure = "provider: 429 rate limited"
	rec := phaseEvent(heavyResult(1500, 2_000))
	rec.Failed, rec.Error = true, failure
	env := events.New(rec, events.TraceContext{})
	env.Source = "Lead"
	whole, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	least, err := (&phaseCutter{env: env, original: env.Data.(*types.AgentPhaseCompleted),
		whole: len(whole), kept: wholeKept{parts: 99}}).leastForm()
	if err != nil {
		t.Fatal(err)
	}
	const server = 200 << 10
	if least.bytes >= server || len(whole) <= server {
		t.Fatalf("the fixture's least form is %d bytes and its whole %d; the case needs a whole the "+
			"%d-byte server refuses and a least form it takes", least.bytes, len(whole), server)
	}
	pub := &transport{refuse: tooLargeAbove(server)}
	account := emitterOver(pub).publishPhase(t.Context(), rec)

	records, _ := pub.accepted()
	if len(records) != 1 || account.Form != phaseLeast {
		t.Fatalf("%d records published in form %q (account %+v); want the least form",
			len(records), account.Form, account)
	}
	if got := records[0]; got.Error != failure || len(got.ToolExecutions) != 1501 {
		t.Errorf("the least form carries the error %q and %d calls; want %q whole beside all 1501",
			got.Error, len(got.ToolExecutions), failure)
	}
}

// EVERY FORM TRIED IS SMALLER THAN EVERY FORM REFUSED, AND THE LARGEST SUCH.
//
// The walk's one rule, asked of the cutter directly: a target the least form
// fits is answered by a fit to it; one it does not is answered by the least
// form while that is smaller than what was refused, and by the rows that fit
// once it is not; and when even no row at all is smaller than what was
// refused, there is nothing left to try.
func TestAFormIsOnlyEverSmallerThanWhatWasRefused(t *testing.T) {
	t.Parallel()
	rec := phaseEvent(heavyResult(400, 2_000))
	env := events.New(rec, events.TraceContext{})
	cut := &phaseCutter{env: env, original: env.Data.(*types.AgentPhaseCompleted), whole: 1 << 20,
		kept: wholeKept{parts: 1}}
	least, err := cut.leastForm()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name            string
		target, refused int
		want            phaseForm
	}{
		{"a target the least form fits", least.bytes + 20_000, 1 << 30, phaseFitted},
		{"a target under it, the least form not yet refused", least.bytes - 1, least.bytes + 1, phaseLeast},
		{"the least form refused", least.bytes / 2, least.bytes, phaseRowsOmitted},
	} {
		got, ok, err := cut.shapeBelow(tc.target, tc.refused)
		if err != nil || !ok {
			t.Fatalf("%s: no form (%v)", tc.name, err)
		}
		if got.form != tc.want || got.bytes >= tc.refused {
			t.Errorf("%s: a %s form of %d bytes, want a %s form under the %d refused",
				tc.name, got.form, got.bytes, tc.want, tc.refused)
		}
		if tc.want != phaseLeast && got.bytes > tc.target {
			t.Errorf("%s: %d bytes, past the %d-byte target", tc.name, got.bytes, tc.target)
		}
	}
	// NOTHING SMALLER: every row given up, and still no smaller than a form
	// already refused.
	none, err := cut.rowsFitting(0)
	if err != nil || none.calls != len(rec.ToolExecutions) {
		t.Fatalf("the smallest form carries %d of %d calls (%v)", len(rec.ToolExecutions)-none.calls,
			len(rec.ToolExecutions), err)
	}
	if _, ok, err := cut.shapeBelow(0, none.bytes); ok || err != nil {
		t.Errorf("a form was offered below the smallest one this record has (%v)", err)
	}
}

// PAST THE LEAST FORM, THE FIRST ROWS GO OUT AND THE REST ARE COUNTED.
//
// A record whose every text is already its mark can still be too large — an
// agent-mode run's bridge log holds every call the run made — and then its
// later rows are left off, counted rather than dropped, because the whole
// holds them. The rows it keeps are its FIRST, in order.
func TestPastTheLeastFormTheFirstRowsGoOutAndTheRestAreCounted(t *testing.T) {
	t.Parallel()
	const calls = 3000
	rec := phaseEvent(heavyResult(calls, 2_000))
	for i := range rec.ToolExecutions {
		rec.ToolExecutions[i]["name"] = fmt.Sprintf("call_%04d", i)
	}
	pub := &transport{refuse: tooLargeAbove(200 << 10)}
	account := emitterOver(pub).publishPhase(t.Context(), rec)

	records, parts := pub.accepted()
	if len(records) != 1 || account.Form != phaseRowsOmitted {
		t.Fatalf("%d records published in form %q; want its first rows", len(records), account.Form)
	}
	got := records[0]
	kept := len(got.ToolExecutions)
	if kept == 0 || kept+got.ToolExecutionsOmitted != calls+1 {
		t.Fatalf("the record carries %d calls and counts %d omitted, of %d", kept,
			got.ToolExecutionsOmitted, calls+1)
	}
	for i, row := range got.ToolExecutions {
		if row["name"] != fmt.Sprintf("call_%04d", i) {
			t.Fatalf("carried call %d is %v: the rows kept are not the first ones", i, row["name"])
		}
	}
	if !strings.Contains(got.Notes, fmt.Sprintf("%d tool calls", got.ToolExecutionsOmitted)) {
		t.Errorf("notes = %q, want them to count the calls not carried", got.Notes)
	}
	if got.WholeParts == 0 || got.WholeParts != len(parts) {
		t.Errorf("the record names %d parts of the %d published", got.WholeParts, len(parts))
	}
	if got.InputTokens != 9100 {
		t.Errorf("input tokens = %d, want the phase's own 9100", got.InputTokens)
	}
}

// A LEAST FORM PAST THE CEILING IS CUT TO THE CEILING, NOT TO HALF OF ITSELF —
// AND ITS ROWS GO BEFORE ITS ERROR DOES.
//
// A record of enough rows is past one event even with every text at its mark.
// The transport refusing that is the contract working, not a server below it,
// so the rows are cut to what every connection carries — halving instead would
// throw away rows the transport takes. And the rows are what is given up: the
// phase's error, which says why it failed, goes out whole beside the rows that
// fit.
func TestALeastFormPastTheCeilingKeepsTheRowsTheCeilingTakes(t *testing.T) {
	t.Parallel()
	const calls = 90_000
	const failure = "provider: 429 rate limited"
	rec := phaseEvent(heavyResult(calls, 300))
	rec.Failed, rec.Error = true, failure
	pub := &transport{refuse: tooLargeAbove(queue.MaxPayloadBytes)}
	account := emitterOver(pub).publishPhase(t.Context(), rec)

	records, parts := pub.accepted()
	if len(records) != 1 || account.Form != phaseRowsOmitted || len(parts) == 0 {
		t.Fatalf("%d records in form %q with %d parts; want the first rows, and the whole in parts",
			len(records), account.Form, len(parts))
	}
	if got := records[0]; got.Error != failure || got.ToolExecutionsOmitted == 0 {
		t.Errorf("the record's error went out as %q with %d calls omitted; want %q whole beside "+
			"the rows that fit", got.Error, got.ToolExecutionsOmitted, failure)
	}
	var published int
	for _, a := range pub.attempts {
		if a.err == nil && a.typ == (types.AgentPhaseCompleted{}).EventType() {
			published = a.bytes
		}
	}
	if published > queue.MaxPayloadBytes || published <= queue.MaxPayloadBytes*3/4 {
		t.Errorf("the record went out at %d bytes; want the rows the %d-byte ceiling takes, not a "+
			"target halved below it", published, queue.MaxPayloadBytes)
	}
	assertTheWalkHalves(t, pub)
}

// A PART THAT FAILS FOR ANOTHER REASON ENDS THE PARTS, AND THE RECORD SAYS SO.
//
// Only a size refusal is answered by splitting. Any other failure means this
// whole will not be kept, so the record must name no whole — one it named
// would be a reference to parts that are not there — and its notes say why.
func TestAPartThatFailsForAnotherReasonLeavesTheRecordWithNoWhole(t *testing.T) {
	t.Parallel()
	pub := &transport{}
	// The first part lands; the connection goes before the second.
	partsSeen := 0
	pub.refuse = func(ev *events.Event, raw []byte) error {
		if ev.Type == (types.AgentPhaseRecordPart{}).EventType() {
			if partsSeen++; partsSeen > 1 {
				return errors.New("nats: connection closed")
			}
			return nil
		}
		return tooLargeAbove(queue.MaxPayloadBytes)(ev, raw)
	}
	account := emitterOver(pub).publishPhase(t.Context(), phaseEvent(heavyResult(300, 42_000)))

	records, parts := pub.accepted()
	if len(records) != 1 || len(parts) != 1 {
		t.Fatalf("%d records and %d parts published; want the record and the one part that landed",
			len(records), len(parts))
	}
	got := records[0]
	if got.WholeParts != 0 || account.Parts != 0 {
		t.Errorf("the record names %d parts (account %d) of a whole that was not all published: a "+
			"reference to parts that are not all there", got.WholeParts, account.Parts)
	}
	if got.WholeBytes == 0 {
		t.Error("whole_bytes is zero, so the record reads as whole though it was cut")
	}
	if !strings.Contains(got.Notes, "was not kept") || !strings.Contains(got.Notes, "connection closed") {
		t.Errorf("notes = %q, want them to say the whole was not kept and why", got.Notes)
	}
}

// THE ONE FLOOR: A LEAST FORM THE TRANSPORT REFUSES LEAVES NO RECORD — AND ITS
// WHOLE STILL READS BACK.
//
// When even the smallest form of a record is refused there is nothing smaller
// to send, and the account says no record was published. The parts went
// first, so the whole is kept all the same.
func TestTheSmallestFormRefusedLeavesNoRecordAndTheWholeInParts(t *testing.T) {
	t.Parallel()
	pub := &transport{}
	pub.refuse = func(ev *events.Event, raw []byte) error {
		if ev.Type == (types.AgentPhaseCompleted{}).EventType() {
			return tooLargeAbove(100)(ev, raw)
		}
		return tooLargeAbove(queue.MaxPayloadBytes)(ev, raw)
	}
	account := emitterOver(pub).publishPhase(t.Context(), phaseEvent(heavyResult(300, 42_000)))

	records, parts := pub.accepted()
	if len(records) != 0 || account.Form != "" || !errors.Is(account.Err, queue.ErrTooLarge) {
		t.Fatalf("%d records published, account %+v; want none, refused as too large", len(records), account)
	}
	if whole := reassemble(t, parts); !bytes.Equal(whole, pub.first) {
		t.Errorf("the parts reassemble to %d bytes that differ from the %d refused", len(whole), len(pub.first))
	}
	assertTheWalkHalves(t, pub)
	// Every form tried was smaller than the one refused before it, which is
	// what ends the walk.
	previous := len(pub.first) + 1
	for _, a := range pub.attempts {
		if a.typ != (types.AgentPhaseCompleted{}).EventType() {
			continue
		}
		if a.bytes >= previous {
			t.Fatalf("a form of %d bytes was tried after one of %d was refused", a.bytes, previous)
		}
		previous = a.bytes
	}
}

// THE JUDGE'S RECORD GOES THROUGH THE SAME PATH.
//
// Every agent_phase_completed publisher shares the one that keeps a refused
// record's whole. A judge record refused as too large is published as a part
// before anything else — which a plain publish, giving up on the refusal,
// never does.
func TestTheJudgesRecordKeepsItsWholeWhenRefused(t *testing.T) {
	t.Parallel()
	pub := &transport{}
	refusedOnce := false
	pub.refuse = func(ev *events.Event, raw []byte) error {
		if ev.Type == (types.AgentPhaseCompleted{}).EventType() && !refusedOnce {
			refusedOnce = true
			return fmt.Errorf("publish: %d bytes: %w", len(raw), queue.ErrTooLarge)
		}
		return nil
	}
	e := emitterOver(pub)
	e.judged(t.Context(), phase.Execute, 1, 4, extension.Decision{
		Extend: true, Reason: "still making progress", Asked: true,
		Model: "claude-haiku-5", InputTokens: 300, OutputTokens: 20,
	}, 0)

	_, parts := pub.accepted()
	if len(parts) != 1 {
		t.Fatalf("%d parts published after the judge's record was refused; want its whole, as one part",
			len(parts))
	}
	var back events.Event
	if err := json.Unmarshal(reassemble(t, parts), &back); err != nil {
		t.Fatal(err)
	}
	if judged, ok := back.Data.(*types.AgentPhaseCompleted); !ok || judged.Phase != types.PhaseJudge {
		t.Errorf("the part holds %T, want the judge's phase record", back.Data)
	}
}

// A WHOLE TOO LARGE FOR ONE EVENT READS BACK WHOLE FROM THE EVENT STORE.
//
// End to end: the engine's own writer persists what this node publishes, the
// parts included, and the store reassembles them by the record's id — while
// no listing of the log ever shows one.
func TestAWholeTooLargeForOneEventReadsBackWholeFromTheStore(t *testing.T) {
	t.Parallel()
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "store.db"), store.Options{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	q := memory.New()
	q.AddPublishListener(observe.NewWriter(db.Events()).Listen())
	if err := q.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = q.Stop(context.Background()) })

	res := heavyResult(300, 42_000)
	emitterOver(q).completed(t.Context(), phaseRecord{Phase: phase.Execute, Iteration: 1, Result: res})

	phases, _, err := db.Events().Phases(t.Context(), "", 0, nil)
	if err != nil || len(phases) != 1 {
		t.Fatalf("Phases = %d rows, %v; want the one phase record and no part", len(phases), err)
	}
	listed, err := db.Events().List(t.Context(), store.ListQuery{})
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range listed {
		if row.Type == (types.AgentPhaseRecordPart{}).EventType() {
			t.Fatalf("the event listing returned a part: %s", row.ID)
		}
	}
	got, err := db.Events().PhaseRecordWhole(t.Context(), phases[0].ID)
	if err != nil {
		t.Fatalf("PhaseRecordWhole: %v", err)
	}
	var back events.Event
	if err := json.Unmarshal(got.Payload, &back); err != nil {
		t.Fatal(err)
	}
	full, ok := back.Data.(*types.AgentPhaseCompleted)
	if !ok || back.ID.String() != phases[0].ID || got.Parts < 2 {
		t.Fatalf("the whole is a %T with id %s from %d parts; want the record %s from its parts",
			back.Data, back.ID, got.Parts, phases[0].ID)
	}
	if full.ToolExecutions[0]["result"] != res.Executions[0].Output {
		t.Error("the whole read back from the store carries a cut result")
	}
}

// abandonedRecord is a phase record whose bulk is attempts a provider gave up
// on: n of them, each with a short reasoning of its own and content of about
// `each` bytes of three-byte characters, so a cut through one would show.
func abandonedRecord(n, each int) types.AgentPhaseCompleted {
	rec := phaseEvent(toolloop.Result{RoundsUsed: 1, Model: "claude-sonnet-5",
		InputTokens: 9100, OutputTokens: 420, Executions: []toolloop.Execution{
			{Round: 1, Name: "slack_post", Args: map[string]any{"channel": "C1"}, Output: "posted"},
		}})
	for i := range n {
		rec.AbandonedAttempts = append(rec.AbandonedAttempts, types.RoundNarration{
			"round": 1, "reasoning": fmt.Sprintf("attempt %d", i),
			"content": strings.Repeat("あ", each/3),
		})
	}
	return rec
}

// A RECORD PAST ONE EVENT CUTS ITS ABANDONED ATTEMPTS LIKE ITS ROUNDS.
//
// What the model wrote and no round kept is prose on the record, and a phase
// whose provider kept failing late in its answers holds enough of it to put
// the record past what one event carries. The fit cuts those texts with the
// rest of the prose — each ending in "…", with its whole length beside it —
// rather than giving up a row or the record, and the whole in parts holds
// every one of them whole.
func TestARecordPastOneEventCutsItsAbandonedAttemptsLikeItsRounds(t *testing.T) {
	t.Parallel()
	const attempts = 250
	rec := abandonedRecord(attempts, 42_000)
	whole, _ := rec.AbandonedAttempts[0]["content"].(string)
	// The premise: whole, this record is past the ceiling.
	if attempts*len(whole) <= queue.MaxPayloadBytes {
		t.Fatalf("the fixture's attempts total %d bytes, not past the %d-byte ceiling",
			attempts*len(whole), queue.MaxPayloadBytes)
	}
	pub := &transport{refuse: tooLargeAbove(queue.MaxPayloadBytes)}
	account := emitterOver(pub).publishPhase(t.Context(), rec)

	records, parts := pub.accepted()
	if len(records) != 1 || account.Form != phaseFitted {
		t.Fatalf("%d records published in form %q; want the record fitted", len(records), account.Form)
	}
	got := records[0]
	if len(got.AbandonedAttempts) != attempts || got.AbandonedAttemptsOmitted != 0 {
		t.Fatalf("the record carries %d abandoned attempts and counts %d omitted; want every one "+
			"of the %d", len(got.AbandonedAttempts), got.AbandonedAttemptsOmitted, attempts)
	}
	for i, row := range got.AbandonedAttempts {
		content, _ := row["content"].(string)
		if !strings.HasSuffix(content, "…") || !utf8.ValidString(content) ||
			!strings.HasPrefix(whole, strings.TrimSuffix(content, "…")) {
			t.Fatalf("attempt %d's cut content is unmarked, broken or not a head of the whole", i)
		}
		// A number read back off the wire into a map is a float64.
		if row["content_bytes"] != float64(len(whole)) {
			t.Fatalf("attempt %d: content_bytes = %v, want the whole content's %d",
				i, row["content_bytes"], len(whole))
		}
		if row["reasoning"] != fmt.Sprintf("attempt %d", i) {
			t.Fatalf("attempt %d's reasoning, far under the level, was cut: %v", i, row["reasoning"])
		}
	}
	if !strings.Contains(got.Notes, "abandoned attempt") {
		t.Errorf("notes = %q, want them to say a cut text on an abandoned attempt carries its "+
			"whole length", got.Notes)
	}
	if got.WholeParts == 0 || got.WholeParts != len(parts) {
		t.Fatalf("the record names %d parts of the %d published", got.WholeParts, len(parts))
	}
	var back events.Event
	if err := json.Unmarshal(reassemble(t, parts), &back); err != nil {
		t.Fatalf("the reassembled whole is not an event: %v", err)
	}
	full, _ := back.Data.(*types.AgentPhaseCompleted)
	if full == nil || len(full.AbandonedAttempts) != attempts {
		t.Fatalf("the whole is a %T; want the record with all %d abandoned attempts", back.Data, attempts)
	}
	for i, row := range full.AbandonedAttempts {
		if row["content"] != whole {
			t.Fatalf("attempt %d in the whole is %d bytes; want its content whole (%d)",
				i, len(fmt.Sprint(row["content"])), len(whole))
		}
	}
}

// CUTTING A RECORD LEAVES THE CALLER'S ATTEMPTS AS THEY WERE.
//
// A row is a map, and the record a caller hands in shares its rows with
// whoever built it. Every form is cut from a copy with rows of its own: a cut
// written into a shared row would change the caller's record under it, and
// every form after the first would be cut from texts already cut.
func TestCuttingARecordLeavesTheCallersAttemptsAsTheyWere(t *testing.T) {
	t.Parallel()
	rec := abandonedRecord(20, 42_000)
	whole, _ := rec.AbandonedAttempts[0]["content"].(string)
	// A server far below the contract, so the record goes out cut.
	pub := &transport{refuse: tooLargeAbove(200 << 10)}
	if account := emitterOver(pub).publishPhase(t.Context(), rec); account.Form == phaseWhole ||
		account.Form == "" {
		t.Fatalf("the record went out in form %q (%v); the case needs it cut", account.Form, account.Err)
	}
	for i, row := range rec.AbandonedAttempts {
		if row["content"] != whole || row["content_bytes"] != nil {
			t.Fatalf("the caller's attempt %d holds %d bytes of content and content_bytes %v: "+
				"the cut wrote into it", i, len(fmt.Sprint(row["content"])), row["content_bytes"])
		}
	}
}

// PAST THE LEAST FORM, THE ABANDONED ATTEMPTS GO FIRST.
//
// When a record at its least — every text at its mark — is still too large,
// its later rows are counted rather than carried, one list at a time. The
// attempts no round kept go before the tool calls and the rounds, which are
// what the phase did: the form keeps every call and every round, and the
// first attempts that still fit.
func TestPastTheLeastFormTheAbandonedAttemptsGoFirst(t *testing.T) {
	t.Parallel()
	const attempts = 400
	rec := phaseEvent(heavyResult(400, 2_000))
	rec.RoundNarration = []types.RoundNarration{
		{"round": 1, "reasoning": strings.Repeat("r", 500), "content": "posted it"},
	}
	for i := range attempts {
		rec.AbandonedAttempts = append(rec.AbandonedAttempts, types.RoundNarration{
			"round": i + 1, "reasoning": strings.Repeat("t", 500), "content": strings.Repeat("c", 500),
		})
	}
	env := events.New(rec, events.TraceContext{})
	cut := &phaseCutter{env: env, original: env.Data.(*types.AgentPhaseCompleted), whole: 1 << 20,
		kept: wholeKept{parts: 1}}
	least, err := cut.leastForm()
	if err != nil {
		t.Fatal(err)
	}
	// A target half way between the least form and the least form with no
	// attempt at all: some attempts fit, and nothing else has to go.
	bare := *least.rec
	bare.AbandonedAttempts = nil
	bareEnv := *least.env
	bareEnv.Data = &bare
	without, err := measure(&bareEnv)
	if err != nil {
		t.Fatal(err)
	}
	target := (least.bytes + without) / 2

	shape, err := cut.rowsFitting(target)
	if err != nil {
		t.Fatal(err)
	}
	if shape.bytes > target || shape.calls != 0 || shape.rounds != 0 {
		t.Fatalf("the form weighs %d of a %d-byte target and gives up %d calls and %d rounds; want "+
			"every call and round carried", shape.bytes, target, shape.calls, shape.rounds)
	}
	carried := shape.rec.AbandonedAttempts
	if shape.attempts == 0 || len(carried) == 0 || shape.attempts+len(carried) != attempts ||
		shape.rec.AbandonedAttemptsOmitted != shape.attempts {
		t.Fatalf("the form carries %d attempts and counts %d omitted (%d on the record), of %d; "+
			"want the first that fit carried and the rest counted", len(carried), shape.attempts,
			shape.rec.AbandonedAttemptsOmitted, attempts)
	}
	for i, row := range carried {
		if row["round"] != i+1 {
			t.Fatalf("carried attempt %d is round %v: the attempts kept are not the first ones",
				i, row["round"])
		}
	}
	if !strings.Contains(shape.rec.Notes, fmt.Sprintf("%d abandoned attempts", shape.attempts)) {
		t.Errorf("notes = %q, want them to count the attempts not carried", shape.rec.Notes)
	}
}
