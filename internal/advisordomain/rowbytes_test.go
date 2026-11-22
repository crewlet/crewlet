package advisordomain_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/advisordomain"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A PAGE OF FINDINGS HOLDS ITS ROWS TO THE BYTES ITS READ CARRIES, short of its
// limit where they run out, and says so with a cursor that resumes at the first
// row it left out: paging to the end reads every finding exactly once.
//
// Mutations, each red: drop the Take (a page holds all five); answer no cursor
// on a cut page (paging stops after the first); take the cursor from the
// refused row (a finding is skipped).
func TestAPageOfFindingsStopsAtItsBytesAndResumesWhereItStopped(t *testing.T) {
	t.Parallel()
	n := newWriteNode(t, openAdvisorLog(t), "data-1")
	why := strings.Repeat("x", 10<<10)
	unread := map[string]bool{}
	for i := range 5 {
		o := opening(fmt.Sprintf("seats.idle_with_queue:seat:s%d", i), advisordomain.SeverityCosts,
			writerClock.Add(time.Duration(i)*time.Minute))
		o.Body = json.RawMessage(`{"title":"An idle seat with work waiting","why":"` + why + `"}`)
		n.open(o)
		unread[o.Episode] = true
	}
	// Room for two rows of ten kibibytes each, never three.
	ctx := statelog.WithAnswerBytes(t.Context(), 25<<10)
	cursor, pages := "", 0
	for {
		rows, next, err := n.reader().Findings(ctx, advisordomain.Filter{Cursor: cursor, Limit: 10}, stale)
		if err != nil {
			t.Fatalf("read page %d of findings: %v", pages+1, err)
		}
		pages++
		if len(rows) > 2 {
			t.Fatalf("page %d held %d findings of ten kibibytes within 25, want at most two", pages, len(rows))
		}
		for _, row := range rows {
			if !unread[row.ID] {
				t.Fatalf("page %d read finding %s twice, or one never opened", pages, row.ID)
			}
			delete(unread, row.ID)
		}
		if next == "" {
			break
		}
		if pages == 5 {
			t.Fatal("paging five findings did not end within five pages")
		}
		cursor = next
	}
	if len(unread) != 0 {
		t.Errorf("paging to the end left %d findings unread", len(unread))
	}
	if pages != 3 {
		t.Errorf("read %d pages, want three: two findings, two, then one", pages)
	}
}

// THE LISTS A NODE HOLDING NO DATA READS A PAGE AT A TIME PAGE AT THEIR BYTES,
// and the duty's one-snapshot read is refused past them rather than cut: under
// a bound one row fits, the classes' last runs, a key's episodes and the alarm
// episodes come back a row a page, every row once, and the open episodes the
// duty reconciles are refused.
//
// Mutations, each red: drop the Take check in ClassRunsPage, EpisodesPage or
// AlarmEpisodesPage (one page holds every row); answer a cut page no cursor
// (paging stops after one); drop the refusal in NonClosed (it answers a slice).
func TestTheAdvisorsListsPageAtTheirBytes(t *testing.T) {
	t.Parallel()
	n := newWriteNode(t, openAdvisorLog(t), "data-1")
	if _, err := n.writer.RecordRun(t.Context(), engineCall(), advisordomain.RunRecord{
		Run: "4c7d2e91-3b5a-4f60-8e1d-9a2b7c6d5e4f", Trigger: advisordomain.TriggerSchedule,
		StartedAt: writerClock, EndedAt: writerClock.Add(time.Minute), Status: advisordomain.RunDone,
		Classes: json.RawMessage(`[{"class":"seats.idle_with_queue","status":"done"},` +
			`{"class":"capacity.add_seat","status":"done"},{"class":"schedule.frequent","status":"done"}]`),
	}); err != nil {
		t.Fatalf("record a run: %v", err)
	}
	first := opening(idleKey, advisordomain.SeverityCosts, writerClock)
	n.open(first)
	n.move(engineCall(), idleKey, closing(first.Episode))
	n.open(opening(idleKey, advisordomain.SeverityCosts, writerClock.Add(time.Hour)))
	for i, instance := range []string{"replicated", "node", "events"} {
		if _, err := n.writer.RaiseAlarm(t.Context(), engineCall(), advisordomain.AlarmRaise{Node: "data-1",
			Kind: "pool_starved", Instance: instance,
			RaisedAt: writerClock.Add(time.Duration(i) * time.Minute)}); err != nil {
			t.Fatalf("raise on %s: %v", instance, err)
		}
	}
	one := statelog.WithAnswerBytes(t.Context(), 1)
	walk := func(name string, want int, page func(cursor string) (int, string, error)) {
		t.Helper()
		got, pages := 0, 0
		for cursor := ""; pages < 10; {
			rows, next, err := page(cursor)
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			got, pages = got+rows, pages+1
			if rows != 1 {
				t.Errorf("%s: a page under one row's bytes held %d rows", name, rows)
			}
			if cursor = next; cursor == "" {
				break
			}
		}
		if got != want || pages != want {
			t.Errorf("%s: paged %d rows over %d pages, want %d of each", name, got, pages, want)
		}
	}
	walk("the classes' last runs", 3, func(cursor string) (int, string, error) {
		runs, next, err := n.reader().ClassRunsPage(one, cursor, stale)
		return len(runs), next, err
	})
	walk("a key's episodes", 2, func(cursor string) (int, string, error) {
		rows, next, err := n.reader().EpisodesPage(one, idleKey, cursor, stale)
		return len(rows), next, err
	})
	walk("the alarm episodes", 3, func(cursor string) (int, string, error) {
		rows, next, err := n.reader().AlarmEpisodesPage(one, writerClock, cursor, stale)
		return len(rows), next, err
	})
	n.open(opening("seats.idle_with_queue:seat:ops", advisordomain.SeverityCosts, writerClock))
	if _, err := n.reader().NonClosed(one, stale); err == nil {
		t.Error("the open episodes past one row's bytes were answered, not refused")
	}
	if rows, err := n.reader().NonClosed(t.Context(), stale); err != nil || len(rows) != 2 {
		t.Errorf("the open episodes within the bound read %d rows, %v; want two", len(rows), err)
	}
}
