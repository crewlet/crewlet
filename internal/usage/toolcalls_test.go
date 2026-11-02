package usage_test

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/usage"
)

// seatWithTools is a seat-day carrying the given tool calls.
func seatWithTools(node, day, seat string, tools ...usage.ToolCalls) usage.Record {
	r := seatRecord(node, day, seat, "m-1")
	r.Tools = tools
	return r
}

// A SEAT-DAY'S CALLS PER TOOL ARE REPLACED WITH ITS RECORD, AND EVERY NODE'S
// ARE SUMMED. A record is the seat-day's whole value, so the afternoon's
// record leaves only its own entries; two nodes that both ran the seat publish
// two records for the day, and a reader asking for the server sums them per
// (tool, seat), naming the seat by its head row.
//
// Mutation: leave the tool rows out of the seat apply's clear, and the
// morning's github_close_issue survives; drop the server from the read's
// filter, and linear's calls are counted as github's.
func TestASeatDaysToolCallsAreReplacedAndSummedAcrossNodes(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	const day = "2026-09-23"
	at := func(h int) time.Time { return time.Date(2026, 9, 23, h, 0, 0, 0, time.UTC) }
	morning := seatWithTools("node-a", day, "seat-1",
		usage.ToolCalls{Server: "github", Tool: "github_close_issue", Calls: 1, Refused: 1},
		usage.ToolCalls{Server: "github", Tool: "github_get_issue", Calls: 1, TotalMS: 100, LastAt: at(9)})
	afternoon := seatWithTools("node-a", day, "seat-1",
		usage.ToolCalls{Server: "github", Tool: "github_get_issue", Calls: 3, Failed: 1,
			Unavailable: 1, TotalMS: 400, LastAt: at(15)},
		usage.ToolCalls{Server: "linear", Tool: "linear_list_issues", Calls: 2, TotalMS: 50, LastAt: at(14)})
	other := seatWithTools("node-b", day, "seat-1",
		usage.ToolCalls{Server: "github", Tool: "github_get_issue", Calls: 2, TotalMS: 60, LastAt: at(16)})
	for i, r := range []usage.Record{morning, afternoon, other} {
		if err := applyRecord(t.Context(), db, recordAt(t, r, uint64(i+1))); err != nil {
			t.Fatalf("apply record %d: %v", i, err)
		}
	}

	rows, err := usage.ServerToolCalls(t.Context(), db.Replicated(),
		usage.ServerToolCallsQuery{From: day, To: day, Server: "github"})
	if err != nil {
		t.Fatalf("read github's calls: %v", err)
	}
	want := []usage.ServerToolCallsRow{{Tool: "github_get_issue", AgentID: "seat-1", Handle: "dev",
		Calls: 5, Failed: 1, Unavailable: 1, TotalMS: 460, LastAt: at(16)}}
	if !reflect.DeepEqual(rows, want) {
		t.Errorf("github's calls read\n%+v\nwant node-a's afternoon and node-b's day summed, "+
			"and nothing of the morning the afternoon replaced:\n%+v", rows, want)
	}
	if n := len(dump(t, db)["usage_tool_calls"]); n != 3 {
		t.Errorf("the estate holds %d tool rows, want node-a's two and node-b's one", n)
	}
}

// WHAT THE FLEET HOLDS OF A SEAT-DAY'S TOOL CALLS IS READ BACK WHOLE, and the
// join raises each figure to the larger, keeps an entry only the fleet holds,
// and counts what the cap dropped on the larger side.
//
// Mutation: read the head row without `tools_elided`, or the entries without
// `last_at`, and the read-back differs from the record; join a figure by the
// derivation alone, and the fleet's larger count is lost.
func TestWhatTheFleetHoldsOfASeatsToolCallsJoinsFigureByFigure(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	const day = "2026-09-23"
	at := func(h int) time.Time { return time.Date(2026, 9, 23, h, 0, 0, 0, time.UTC) }
	held := seatWithTools("node-a", day, "seat-1",
		usage.ToolCalls{Server: "github", Tool: "github_get_issue", Calls: 4, Failed: 2,
			TotalMS: 800, LastAt: at(11)},
		usage.ToolCalls{Server: "linear", Tool: "linear_list_issues", Calls: 1, LastAt: at(8)})
	held.ToolsElided = 3
	if err := applyRecord(t.Context(), db, recordAt(t, held, 1)); err != nil {
		t.Fatalf("apply: %v", err)
	}
	applied, ok, err := usage.Applied(t.Context(), db.Replicated(), held.Subject)
	if err != nil || !ok {
		t.Fatalf("read what the fleet holds: %v, %v", ok, err)
	}
	if !reflect.DeepEqual(applied.Tools, held.Tools) || applied.ToolsElided != 3 {
		t.Fatalf("the fleet holds %+v (elided %d), want the record's %+v (elided 3)",
			applied.Tools, applied.ToolsElided, held.Tools)
	}

	derived := seatWithTools("node-a", day, "seat-1",
		usage.ToolCalls{Server: "github", Tool: "github_get_issue", Calls: 5, Failed: 1,
			Refused: 1, TotalMS: 700, LastAt: at(12)})
	got := usage.Join(derived, applied)
	want := []usage.ToolCalls{
		{Server: "github", Tool: "github_get_issue", Calls: 5, Failed: 2, Refused: 1,
			TotalMS: 800, LastAt: at(12)},
		{Server: "linear", Tool: "linear_list_issues", Calls: 1, LastAt: at(8)},
	}
	if !reflect.DeepEqual(got.Tools, want) {
		t.Errorf("the join's tool calls are\n%+v\nwant\n%+v", got.Tools, want)
	}
	if got.ToolsElided != 3 {
		t.Errorf("the join elides %d entries, want the fleet's 3: it held five", got.ToolsElided)
	}
}

// A SEAT-DAY CARRIES AT MOST [usage.ToolsPerSeatDay] TOOL ENTRIES, the most
// called kept and the rest counted; a record past the cap, or naming one tool
// twice, is a writer fault no apply takes.
//
// Mutation: keep the least-called instead, and the busiest tool is the one
// dropped; let a record past the cap encode, and the refusal goes missing.
func TestASeatDaysToolEntriesAreCappedWithTheRestCounted(t *testing.T) {
	t.Parallel()
	var many []usage.ToolCalls
	for i := range usage.ToolsPerSeatDay + 2 {
		many = append(many, usage.ToolCalls{Server: "wide", Tool: fmt.Sprintf("tool_%04d", i),
			Calls: int64(i + 1)})
	}
	derived := seatWithTools("node-a", "2026-09-23", "seat-1", many...)
	joined := usage.Join(derived, usage.Record{RecordEnvelope: derived.RecordEnvelope,
		ToolsElided: 0, Tools: []usage.ToolCalls{{Server: "wide", Tool: "tool_0000", Calls: 1}}})
	if len(joined.Tools) != usage.ToolsPerSeatDay || joined.ToolsElided != 2 {
		t.Fatalf("the capped day carries %d entries eliding %d, want %d eliding 2",
			len(joined.Tools), joined.ToolsElided, usage.ToolsPerSeatDay)
	}
	for _, t2 := range joined.Tools {
		if t2.Tool == "tool_0000" || t2.Tool == "tool_0001" {
			t.Errorf("the cap kept %s, one of the two least-called tools", t2.Tool)
		}
	}
	if _, err := derived.Encode(); err == nil || !strings.Contains(err.Error(), "at most") {
		t.Errorf("a record past the cap encoded (%v); the writer elides past it", err)
	}
	twice := seatWithTools("node-a", "2026-09-23", "seat-1",
		usage.ToolCalls{Server: "github", Tool: "github_get_issue", Calls: 1},
		usage.ToolCalls{Server: "github", Tool: "github_get_issue", Calls: 2})
	if _, err := twice.Encode(); err == nil || !strings.Contains(err.Error(), "twice") {
		t.Errorf("a record naming one tool twice encoded (%v)", err)
	}
}

// A SEAT'S CALLS PER TOOL ARE PUBLISHED FROM ITS TURNS' ENDINGS: the node
// derives its day from the tally each ending states, publishes it on the
// seat's record, and every data node's applier writes it where a server's
// reader finds it, a seat-day without calls carrying none.
//
// Mutation: leave the tools off the seat's record in Records, and the server
// reads nothing.
func TestASeatsToolCallsArePublishedFromItsTurnsEndings(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	local := func(h, m int) time.Time { return time.Date(2026, 9, 23, h, m, 0, 0, santiago) }
	ev := &events{t: t, db: db}
	ev.turnCalling(local(9, 0), "seat-1", "dev", "t1",
		map[string]any{"server": "github", "tool": "github_get_issue", "calls": 3, "failed": 1,
			"total_ms": 300, "last_at": local(8, 59).UTC()},
		map[string]any{"server": "linear", "tool": "linear_list_issues", "calls": 1, "refused": 1})
	ev.turnCalling(local(10, 0), "seat-1", "dev", "t2",
		map[string]any{"server": "github", "tool": "github_get_issue", "calls": 2,
			"total_ms": 100, "last_at": local(9, 58).UTC()})
	ev.turn(local(11, 0), "seat-2", "ops", "t3", false, time.Minute)

	p, _ := newPublisher(t, "node-a", db, db, local(23, 50))
	if err := p.Flush(t.Context()); err != nil {
		t.Fatalf("flush: %v", err)
	}
	rows, err := usage.ServerToolCalls(t.Context(), db.Replicated(),
		usage.ServerToolCallsQuery{From: "2026-09-23", To: "2026-09-23", Server: "github"})
	if err != nil {
		t.Fatal(err)
	}
	want := []usage.ServerToolCallsRow{{Tool: "github_get_issue", AgentID: "seat-1",
		Handle: "dev", Calls: 5, Failed: 1, TotalMS: 400, LastAt: local(9, 58).UTC()}}
	if !reflect.DeepEqual(rows, want) {
		t.Errorf("github's calls are\n%+v\nwant both turns' added up\n%+v", rows, want)
	}
	if got := len(dump(t, db)["usage_tool_calls"]); got != 2 {
		t.Errorf("the estate holds %d tool rows, want seat-1's two and seat-2's none", got)
	}
}
