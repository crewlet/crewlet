package queries_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/usage"
)

// fireAt is a schedule fire's instant on the history's fixture day.
func fireAt(h int) time.Time { return time.Date(2026, 8, 23, h, 0, 0, 0, time.UTC) }

// scheduleHistory is the replicated history of two schedules that share a
// name, as two data nodes published it: the scheduler's node sent three fires
// of the role's `standup` and one of the unit's, and the seat's node, the next
// day, ran the 09:00 standup in place of the 08:00 one.
func scheduleHistory(t *testing.T) *store.DB {
	t.Helper()
	db := openStore(t)
	standup := func(node, day string) usage.Record {
		return usage.Record{RecordEnvelope: usage.RecordEnvelope{Writer: node,
			Subject: usage.Subject{Kind: usage.KindSchedule, Node: node, Day: day,
				ScopeType: "role", ScopeID: "ceo", Schedule: "standup"}}}
	}
	fires := standup("node-a", "2026-08-23")
	fires.Fires = []usage.Fire{
		{At: fireAt(7), ScheduledAt: fireAt(6), FireLabel: "20260823T0600",
			Outcome: "skipped_catchup"},
		{At: fireAt(8), ScheduledAt: fireAt(8), FireLabel: "20260823T0800", Target: "ceo",
			Outcome: "fired", TraceID: "trace-8"},
		{At: fireAt(9), ScheduledAt: fireAt(9), FireLabel: "20260823T0900", Target: "ceo",
			Outcome: "fired", TraceID: "trace-9"},
	}
	applyUsage(t, db, fires, 1)
	unit := usage.Record{RecordEnvelope: usage.RecordEnvelope{Writer: "node-a",
		Subject: usage.Subject{Kind: usage.KindSchedule, Node: "node-a", Day: "2026-08-23",
			ScopeType: "unit", ScopeID: "platform", Schedule: "standup"}},
		Fires: []usage.Fire{{At: fireAt(9), ScheduledAt: fireAt(9), FireLabel: "20260823T0900",
			Target: "eng", Outcome: "fired", TraceID: "trace-unit"}}}
	applyUsage(t, db, unit, 2)
	setAside := standup("node-b", "2026-08-24")
	setAside.Superseded = []usage.Superseded{{TraceID: "trace-8", At: fireAt(9).AddDate(0, 0, 1)}}
	applyUsage(t, db, setAside, 3)
	return db
}

// outcomesOf maps each fire on a page to what the answer says became of it,
// by its trace (or its tick, for a skip that was never sent).
func outcomesOf(t *testing.T, page any) map[string]map[string]any {
	t.Helper()
	out := map[string]map[string]any{}
	for _, run := range rows(t, page) {
		key, _ := run["trace_id"].(string)
		if key == "" {
			key, _ = run["fire_label"].(string)
		}
		out[key] = run
	}
	return out
}

// THE SCHEDULES' RECENT RUNS SAY WHAT BECAME OF EACH FIRE, every data node's,
// and not only what the scheduler did.
//
// The scheduler's node sent the 08:00 standup and its ledger says `fired`; the
// seat's node ran the 09:00 one in its place, the next day. A history read off
// the scheduler's own ledger said the standup ran twice, which is the thing
// that stopped happening.
//
// Mutation: drop the EXISTS from usage's fireOutcomeSQL, or read the history
// off anything but usage.ScheduleRuns, and the 08:00 fire reads fired.
func TestTheSchedulesRecentRunsSayWhatBecameOfEachFire(t *testing.T) {
	t.Parallel()
	db := scheduleHistory(t)
	body := asMap(t, answer(t, queries.Sources{
		Company: companySource(t, company(t)),
		Usage:   usage.Reader{Estate: db.Replicated()},
	}, "schedules", nil))

	runs := outcomesOf(t, body["recent_runs"])
	if len(runs) != 4 {
		t.Fatalf("%d recent runs, want every schedule's four fires: %v", len(runs), runs)
	}
	for key, want := range map[string]string{
		"trace-8":       "skipped_superseded",
		"trace-9":       "fired",
		"trace-unit":    "fired",
		"20260823T0600": "skipped_catchup",
	} {
		if got := runs[key]["outcome"]; got != want {
			t.Errorf("the fire %s reads %v, want %s", key, got, want)
		}
	}
	// AND THE TICK EACH STANDS FOR, which on the catch-up skip is not when
	// it was recorded.
	skip := runs["20260823T0600"]
	if skip["scheduled_at"] != fireAt(6).Format(time.RFC3339Nano) ||
		skip["fired_at"] != fireAt(7).Format(time.RFC3339Nano) {
		t.Errorf("the catch-up skip reads due %v, recorded %v; want %s and %s",
			skip["scheduled_at"], skip["fired_at"], fireAt(6).Format(time.RFC3339Nano),
			fireAt(7).Format(time.RFC3339Nano))
	}
}

// ONE SCHEDULE'S HISTORY IS ITS OWN FIRES, under the same outcomes.
//
// `schedules.recent_runs` is fifty rows across EVERY schedule, so a company
// with twenty hourly ones fills it in two and a half hours: "did the standup
// fire this week" was unanswerable while every row of the answer sat in the
// table. And a schedule's identity is all three parts: a second unit declaring
// the same name is a different schedule.
//
// Mutation: drop the schedule's identity from the read, and the unit's fire is
// on the role's page; drop the EXISTS from usage's fireOutcomeSQL, and the
// 08:00 fire reads fired.
func TestOneSchedulesRunsSayWhatBecameOfEachFire(t *testing.T) {
	t.Parallel()
	db := scheduleHistory(t)
	body := asMap(t, answer(t, queries.Sources{Usage: usage.Reader{Estate: db.Replicated()}}, "schedule_runs",
		map[string]any{"scope_type": "role", "scope_id": "ceo", "name": "standup"}))

	runs := outcomesOf(t, body["runs"])
	if len(runs) != 3 {
		t.Fatalf("%d runs, want the role's three alone: %v", len(runs), runs)
	}
	if got := runs["trace-8"]["outcome"]; got != "skipped_superseded" {
		t.Errorf("the fire a later one replaced reads %v, want skipped_superseded", got)
	}
	if got := runs["trace-9"]["outcome"]; got != "fired" {
		t.Errorf("the fire that ran reads %v, want fired", got)
	}
	if got := runs["trace-9"]["fire_label"]; got != "20260823T0900" {
		t.Errorf("the fire that ran is labelled %v, want its tick 20260823T0900", got)
	}
	if body["truncated"] != false {
		t.Errorf("three fires read as a cut page: %v", body["truncated"])
	}
}

// A PAGE SAYS IT WAS CUT ONLY WHEN IT WAS: a schedule that has fired exactly a
// page's worth of times is whole, and one fire more is not.
//
// Mutation: report the cut as a full page (`len(runs) == limit`), and the
// exact page reads as cut.
func TestOneSchedulesHistorySaysWhenItWasCut(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		fires int
		cut   bool
	}{{queries.MaxScheduleRuns, false}, {queries.MaxScheduleRuns + 1, true}} {
		t.Run(fmt.Sprint(c.fires), func(t *testing.T) {
			t.Parallel()
			db := openStore(t)
			r := usage.Record{RecordEnvelope: usage.RecordEnvelope{Writer: "node-a",
				Subject: usage.Subject{Kind: usage.KindSchedule, Node: "node-a",
					Day: "2026-08-23", ScopeType: "role", ScopeID: "ceo", Schedule: "heartbeat"}}}
			for i := range c.fires {
				at := fireAt(0).Add(time.Duration(i) * time.Minute)
				r.Fires = append(r.Fires, usage.Fire{At: at, ScheduledAt: at,
					FireLabel: at.Format("20060102T1504"), Target: "ceo", Outcome: "fired",
					TraceID: fmt.Sprintf("trace-%d", i)})
			}
			applyUsage(t, db, r, 1)
			body := asMap(t, answer(t, queries.Sources{Usage: usage.Reader{Estate: db.Replicated()}}, "schedule_runs",
				map[string]any{"scope_type": "role", "scope_id": "ceo", "name": "heartbeat"}))
			if got := len(rows(t, body["runs"])); got != queries.MaxScheduleRuns {
				t.Errorf("%d runs, want a page of %d", got, queries.MaxScheduleRuns)
			}
			if body["truncated"] != c.cut {
				t.Errorf("%d fires read truncated=%v, want %v", c.fires, body["truncated"], c.cut)
			}
		})
	}
}

// unreadable is a replicated estate that cannot be read.
type unreadable struct{}

func (unreadable) Read(context.Context, func(*sql.Tx) error) error {
	return errors.New("the estate is catching up")
}

// AN UNREADABLE HISTORY DOES NOT BLANK THE SCHEDULES. The configured schedules
// are the half an operator opens this screen for, and refusing to show them
// because the footnote is unreadable would blank the page over its least
// important part.
func TestAnUnreadableHistoryDoesNotBlankTheSchedules(t *testing.T) {
	t.Parallel()
	body := asMap(t, answer(t, queries.Sources{
		Company: companySource(t, company(t)),
		Usage:   usage.Reader{Estate: unreadable{}},
	}, "schedules", nil))

	if rows, _ := body["schedules"].([]any); len(rows) != 1 {
		t.Errorf("the schedules were lost with the history: %v", body)
	}
	if runs, _ := body["recent_runs"].([]any); len(runs) != 0 {
		t.Errorf("recent_runs = %v, want an empty list", runs)
	}
}

// A SCHEDULE'S IDENTITY IS ALL THREE PARTS, and every one of them is required:
// a name alone would merge two teams' histories, and a scope this build does
// not know is refused naming the two rather than read as a filter that matches
// nothing.
func TestAScheduleRunsReadStatesWhatItIsMissing(t *testing.T) {
	t.Parallel()
	src := queries.Sources{Usage: usage.Reader{Estate: openStore(t).Replicated()}}
	for _, params := range []map[string]any{
		{"scope_id": "ceo", "name": "standup"},
		{"scope_type": "role", "name": "standup"},
		{"scope_type": "role", "scope_id": "ceo"},
		{"scope_type": "team", "scope_id": "ceo", "name": "standup"},
	} {
		if _, err := askNative(t, src, "schedule_runs", params); !errors.Is(
			err, queries.ErrBadParams) {

			t.Errorf("%v answered %v, want bad params", params, err)
		}
	}
}
