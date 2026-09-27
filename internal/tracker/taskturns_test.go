package tracker_test

import (
	"fmt"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// segment is one completed segment of a run, as the engine charges it.
func segment(run string, turns int, outcome string, phases ...string) tracker.TurnRecord {
	return tracker.TurnRecord{
		Task: "t-1", Seat: "dev", TurnID: run, Trigger: "work_item",
		Outcome: outcome, Phases: phases,
		Spend: tracker.TurnSpend{Turns: turns, Rounds: 2, Input: 100, Output: 20, WallMs: 1000},
	}
}

func (r *roundTrip) turnsOf(task, cursor string, limit int) tracker.TaskTurns {
	r.t.Helper()
	page, err := r.reader.TurnsOf(r.t.Context(), task, cursor, limit,
		statelog.Freshness{Level: statelog.ReadStale})
	if err != nil {
		r.t.Fatalf("TurnsOf(%q, %q, %d): %v", task, cursor, limit, err)
	}
	return page
}

// A TASK'S TURN LIST NUMBERS TURNS, NOT SEGMENTS.
//
// A turn that parks on a coding run charges its task once per segment, and
// only one of those segments counts a turn. Listed as rows, one turn would be
// three cards — the first headed "suspended", which is not how it ended — and
// numbered by row, "Turn 3" would sit beside a cost panel reading 2 turns. So
// the segments fold into one entry per run, the outcome and summary are the
// newest segment's, and the number is the task's own count of turns.
func TestTurnsOfNumbersTurnsNotSegments(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	if _, err := r.writer.CreateTask(t.Context(), "op-1", newTask("t-1"), nil); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	r.drain()

	parked := segment("run-a", 1, "suspended", "execute")
	parked.Summary = "started the branch"
	parked.Tools = []tracker.TurnTool{{Name: "run_sandbox", Calls: 1}}
	resumed := segment("run-a", 0, "done", "execute", "review")
	resumed.Summary = "added the retry"
	resumed.Review = "the timeout path has no test"
	resumed.Spend.SentBack = 1
	resumed.Tools = []tracker.TurnTool{{Name: "run_sandbox", Calls: 2}, {Name: "comment", Calls: 1}}
	// MORE OF A TURN CHARGED ELSEWHERE: a segment carrying no count.
	elsewhere := segment("run-c", 0, "done", "execute")
	// A TURN THAT BROKE IN REVIEW, which the card marks on that chip.
	broke := segment("run-b", 1, "failed", "execute", "review")
	broke.FailedIn = "review"
	for i, rec := range []struct {
		op   string
		turn tracker.TurnRecord
	}{
		{"turn/run-a/dispatch", parked},
		{"turn/run-a/resume/l-1", resumed},
		{"turn/run-b/dispatch", broke},
		{"turn/run-c/resume/l-9", elsewhere},
	} {
		if _, err := r.writer.RecordTurn(t.Context(), rec.op, rec.turn); err != nil {
			t.Fatalf("RecordTurn %d: %v", i, err)
		}
		r.drain()
	}

	page := r.turnsOf("ENG-1", "", 0)
	if len(page.Turns) != 3 {
		t.Fatalf("four segments of three runs listed as %d entries, want 3: %+v",
			len(page.Turns), page.Turns)
	}
	byRun := map[string]tracker.TaskTurn{}
	for _, turn := range page.Turns {
		byRun[turn.TurnID] = turn
	}
	a := byRun["run-a"]
	if a.Ordinal != 1 || byRun["run-b"].Ordinal != 2 || byRun["run-c"].Ordinal != 0 {
		t.Errorf("ordinals a=%d b=%d c=%d, want 1, 2 and none — the number is the "+
			"task's count of turns, and a turn counted elsewhere has none here",
			a.Ordinal, byRun["run-b"].Ordinal, byRun["run-c"].Ordinal)
	}
	if a.Segments != 2 || a.Tokens != 240 || a.Rounds != 4 || a.WallMs != 2000 {
		t.Errorf("run-a folded to segments=%d tokens=%d rounds=%d wall=%d, want "+
			"2, 240, 4 and 2000 — both segments summed", a.Segments, a.Tokens, a.Rounds,
			a.WallMs)
	}
	if a.Outcome != "done" || a.Summary != "added the retry" ||
		a.Review != "the timeout path has no test" || a.SentBack != 1 {
		t.Errorf("run-a reads outcome=%q summary=%q review=%q sent_back=%d, want the "+
			"newest segment's account", a.Outcome, a.Summary, a.Review, a.SentBack)
	}
	if b := byRun["run-b"]; b.Outcome != "failed" || b.FailedIn != "review" {
		t.Errorf("run-b reads outcome=%q failed_in=%q, want failed in review", b.Outcome, b.FailedIn)
	}
	if a.FailedIn != "" {
		t.Errorf("run-a ended done and names a failed phase %q", a.FailedIn)
	}
	if !slices.Equal(a.Phases, []string{"execute", "review"}) {
		t.Errorf("run-a phases %v, want each once in first-run order", a.Phases)
	}
	wantTools := []tracker.TurnTool{{Name: "run_sandbox", Calls: 3}, {Name: "comment", Calls: 1}}
	if !slices.Equal(a.Tools, wantTools) {
		t.Errorf("run-a tools %+v, want %+v", a.Tools, wantTools)
	}
	if got := r.taskSpend("t-1")["spend_turns"]; got != 2 {
		t.Errorf("spend_turns = %d, want 2 — the newest ordinal and the count agree", got)
	}
	if page.Turns[0].TurnID != "run-c" || page.Turns[2].TurnID != "run-a" {
		t.Errorf("order %s, %s, %s — want newest first by each turn's first segment",
			page.Turns[0].TurnID, page.Turns[1].TurnID, page.Turns[2].TurnID)
	}
	if page.Key != "ENG-1" || page.Task != "t-1" || !page.Complete {
		t.Errorf("the page names %q/%q complete=%v, want the task both ways and complete",
			page.Task, page.Key, page.Complete)
	}
}

// A TASK'S TURNS PAGE WITHOUT LOSING ONE.
//
// A cursor is the first segment's position of the oldest turn a page returned,
// and a turn's later segments arrive as its runs are collected — so the walk
// must reach every turn exactly once even while a turn on an earlier page
// gains a segment, and only a page with more past it hands out a cursor.
func TestWorkItemTurnsPages(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	if _, err := r.writer.CreateTask(t.Context(), "op-1", newTask("t-1"), nil); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	r.drain()
	const total = 23
	for i := range total {
		run := fmt.Sprintf("run-%02d", i)
		if _, err := r.writer.RecordTurn(t.Context(), "turn/"+run+"/dispatch",
			segment(run, 1, "done", "execute")); err != nil {
			t.Fatalf("RecordTurn %s: %v", run, err)
		}
	}
	r.drain()

	seen := map[string]int{}
	cursor := ""
	for pages := 0; ; pages++ {
		if pages > total {
			t.Fatal("the walk did not end")
		}
		page := r.turnsOf("t-1", cursor, 10)
		for _, turn := range page.Turns {
			seen[turn.TurnID]++
		}
		if pages == 0 {
			// A LATE SEGMENT OF A TURN ALREADY LISTED lands mid-walk.
			if _, err := r.writer.RecordTurn(t.Context(), "turn/run-22/resume/l-1",
				segment("run-22", 0, "done", "review")); err != nil {
				t.Fatalf("RecordTurn late: %v", err)
			}
			r.drain()
		}
		if page.Next == "" {
			if len(page.Turns) == 0 && pages > 0 {
				t.Error("the last page came back empty — a cursor was handed out " +
					"with nothing past it")
			}
			break
		}
		cursor = page.Next
	}
	if len(seen) != total {
		t.Fatalf("the walk reached %d of %d turns", len(seen), total)
	}
	for run, n := range seen {
		if n != 1 {
			t.Errorf("%s was listed %d times", run, n)
		}
	}
	if first := r.turnsOf("t-1", "", 1).Turns[0]; first.Ordinal != total || first.Segments != 2 {
		t.Errorf("the newest turn reads ordinal=%d segments=%d, want %d and 2",
			first.Ordinal, first.Segments, total)
	}
	if _, err := r.reader.TurnsOf(t.Context(), "t-1", "not-a-cursor", 10,
		statelog.Freshness{Level: statelog.ReadStale}); err == nil {
		t.Error("a malformed cursor was accepted rather than refused")
	}
}
