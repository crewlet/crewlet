package queries_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/period"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
	"github.com/crewlet/crewlet/internal/tracker"
)

// fixedRuns is a pending-run store holding exactly these runs.
type fixedRuns []sandbox.PendingRun

func (f fixedRuns) ListActive(context.Context) ([]sandbox.PendingRun, error) {
	return f, nil
}

// WHAT WAITS ON A PERSON IS THEIR OPEN ASKS AND THE RUNS PARKED ON A QUESTION
// TO THEM — under either of their names, and only runs that are actually
// waiting.
func TestDecisionsCountsAsksAndRunsForTheParty(t *testing.T) {
	t.Parallel()
	askedAt := time.Date(2031, 4, 16, 10, 0, 0, 0, time.UTC)
	oldest := askedAt.Add(-5 * time.Hour)
	parkedAt := askedAt.Add(-time.Hour)
	work := &stubWork{decisions: tracker.DecisionsAnswer{
		Asks: []tracker.AskRow{
			{Comment: "c-1", AskedAt: askedAt},
			{Comment: "c-2", AskedAt: oldest},
		},
		Total:    tracker.ClaimTotal{Total: 7},
		OldestAt: &oldest,
	}}
	sources := partySources(t, work)
	sources.Sandbox = fixedRuns{
		{TurnID: "mine", AgentHandle: "swe", Status: sandbox.StatusAwaiting,
			AudienceHandles: []string{"ops-1"}, UpdatedAt: parkedAt},
		{TurnID: "reseeded", AgentHandle: "swe", Status: sandbox.StatusReseed,
			AudienceHandles: []string{"ana"}, UpdatedAt: parkedAt.Add(-time.Minute)},
		{TurnID: "running", AgentHandle: "swe", Status: sandbox.StatusRunning,
			AudienceHandles: []string{"ana"}, UpdatedAt: askedAt},
		{TurnID: "theirs", AgentHandle: "swe", Status: sandbox.StatusAwaiting,
			AudienceHandles: []string{"cy"}, UpdatedAt: askedAt},
	}
	got, err := askAsOperator(t, sources, "decisions", nil)
	if err != nil {
		t.Fatalf("decisions: %v", err)
	}
	answer := got.(map[string]any)
	wantParty(t, work.decisionsQuery.Who, "ana", "ops-1")
	if answer["total"] != 9 {
		t.Errorf("total = %v, want the tracker's 7 open asks and ana's 2 parked runs",
			answer["total"])
	}
	items := answer["items"].([]queries.DecisionItem)
	var order []string
	for _, item := range items {
		if item.Ask != nil {
			order = append(order, item.Ask.Comment)
		} else {
			order = append(order, item.Run["turn_id"].(string))
		}
	}
	want := []string{"c-1", "mine", "reseeded", "c-2"}
	if len(order) != len(want) {
		t.Fatalf("items are %v, want %v newest first", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("items are %v, want %v newest first", order, want)
		}
	}
	if answer["oldest_at"] != oldest {
		t.Errorf("oldest_at = %v, want the oldest ask's %v", answer["oldest_at"], oldest)
	}
}

// NOBODY TO ANSWER ABOUT IS NOT AN EMPTY LIST: a credential bound to no seat
// is told so, the refusal every personal question gives.
func TestDecisionsNeedsAPerson(t *testing.T) {
	t.Parallel()
	sources := partySources(t, &stubWork{})
	if _, err := askNative(t, sources, "decisions", nil); err == nil {
		t.Error("an anonymous caller naming nobody got an answer")
	}
}

// THE SERIES TAKES A DAY OR A WEEK AND AT MOST NINETY POINTS, and a fortnight
// of days when it names neither.
func TestWorkFlowParams(t *testing.T) {
	t.Parallel()
	work := &stubWork{}
	sources := queries.Sources{Work: work}
	if _, err := askNative(t, sources, "work_flow", nil); err != nil {
		t.Fatalf("work_flow: %v", err)
	}
	if work.flowQuery.Bucket != period.Day || work.flowQuery.Points != queries.DefaultFlowPoints {
		t.Errorf("the default series is %s×%d, want day×%d", work.flowQuery.Bucket,
			work.flowQuery.Points, queries.DefaultFlowPoints)
	}
	for _, params := range []map[string]any{
		{"points": tracker.MaxFlowPoints + 1},
		{"points": 0},
		{"bucket": "month"},
	} {
		if _, err := askNative(t, sources, "work_flow", params); !errors.Is(err, queries.ErrBadParams) {
			t.Errorf("work_flow %v = %v, want bad params", params, err)
		}
	}
}

// THE FEED MERGES ITS SOURCES BY INSTANT, and a kind this node keeps no source
// for is refused by name rather than answered empty.
func TestTheCompanyFeedMergesItsSources(t *testing.T) {
	t.Parallel()
	base := time.Date(2031, 4, 16, 10, 0, 0, 0, time.UTC)
	work := &stubWork{feed: tracker.FeedPage{Rows: []tracker.FeedRow{
		{Task: "a", Kind: tracker.FeedCompleted, At: base,
			Cursor: tracker.FeedCursor{At: base, Seq: 9}.String()},
		{Task: "b", Kind: tracker.FeedCreated, At: base.Add(-2 * time.Hour),
			Cursor: tracker.FeedCursor{At: base.Add(-2 * time.Hour), Seq: 5}.String()},
	}, Complete: true}}
	pageStore := &stubPages{changes: []pages.PageChange{
		{ID: "h-1", PageID: "p", Kind: pages.ChangeSaved, At: base.Add(-time.Hour), LogSeq: 4},
	}}
	sources := queries.Sources{Work: work, Pages: pageStore}
	got, err := askNative(t, sources, "company_feed", map[string]any{"limit": 2})
	if err != nil {
		t.Fatalf("company_feed: %v", err)
	}
	answer := got.(map[string]any)
	rows := answer["rows"].([]queries.FeedEntry)
	if len(rows) != 2 || rows[0].Work == nil || rows[0].Work.Task != "a" ||
		rows[1].Page == nil {
		t.Fatalf("the merged page is %+v, want the completion then the page save", rows)
	}
	if _, more := answer["next_cursor"]; !more {
		t.Fatal("the tracker's older row was not consumed and no cursor resumes it")
	}
	if len(pageStore.activity.Kinds) != 2 {
		t.Errorf("the pages were asked for %v, want created and saved", pageStore.activity.Kinds)
	}

	// A FLOOR NAMES THE TRACKER'S LOG, and the pages half never gets it: a
	// position in one domain's log is no bound on another's.
	if _, err := askNative(t, sources, "company_feed", map[string]any{
		"read_level": "session", "min_position": "CREWLET_TRACKER_LOG@1:4711",
	}); err != nil {
		t.Fatalf("company_feed at a floor: %v", err)
	}
	if work.feedQuery.MinPosition.Seq != 4711 {
		t.Errorf("the tracker half read at %+v, want the caller's floor", work.feedQuery.MinPosition)
	}
	if pageStore.fresh.MinPosition.Seq != 0 || pageStore.fresh.Level == statelog.ReadSession {
		t.Errorf("the pages half was handed the tracker's floor: %+v", pageStore.fresh)
	}

	if _, err := askNative(t, queries.Sources{Work: work}, "company_feed",
		map[string]any{"kinds": "page"}); !errors.Is(err, queries.ErrBadParams) {
		t.Errorf("asking for pages where none are kept = %v, want bad params", err)
	}
	if _, err := askNative(t, sources, "company_feed",
		map[string]any{"kinds": "gossip"}); !errors.Is(err, queries.ErrBadParams) {
		t.Errorf("an unknown kind = %v, want bad params", err)
	}
}

// A TICK NOT RUN IS NOT SOMETHING THE COMPANY DID, AND A SCHEDULE'S HEARTBEAT
// IS ONE ROW. Over the usage domain's real rows: a sweep that fired six times
// with two catch-up skips and a paused tick among the fires reads as ONE row
// of six runs, and no row of the feed — under Everything or under Schedules —
// is a skipped tick.
func TestTheFeedFoldsASchedulesRunsAndLeavesItsSkippedTicksOut(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	base := time.Date(2031, 4, 16, 10, 0, 0, 0, time.UTC)
	outcomes := []string{"fired", "skipped_catchup", "fired", "fired", "skipped_paused",
		"fired", "skipped_catchup", "fired", "fired"}
	if err := storetest.EstateOf(db).Tx(t.Context(), func(tx *sql.Tx) error {
		for i, outcome := range outcomes {
			at := base.Add(-time.Duration(i+1) * 10 * time.Minute)
			target := "pm"
			if outcome == "skipped_catchup" {
				target = "" // a catch-up skip resolved no runner
			}
			if _, err := tx.ExecContext(t.Context(), `
				INSERT INTO usage_schedule_runs
					(day, node, scope_type, scope_id, name, fired_at, target,
					 outcome, trace_id, turn_id, version)
				VALUES (?, 'n1', 'role', 'pm', 'backlog-sweep', ?, ?, ?, '', '', 1)`,
				at.Format("2006-01-02"), store.EncodeTime(at), target, outcome); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	work := &stubWork{feed: tracker.FeedPage{Rows: []tracker.FeedRow{
		{Task: "a", Kind: tracker.FeedCompleted, At: base,
			Cursor: tracker.FeedCursor{At: base, Seq: 9}.String()},
	}, Complete: true}}
	sources := queries.Sources{Work: work, Usage: storetest.EstateOf(db)}
	for _, kinds := range []string{"", "schedule"} {
		got, err := askNative(t, sources, "company_feed", map[string]any{"kinds": kinds})
		if err != nil {
			t.Fatalf("company_feed kinds=%q: %v", kinds, err)
		}
		var runs []*queries.FeedScheduleRun
		for _, row := range got.(map[string]any)["rows"].([]queries.FeedEntry) {
			if row.Schedule != nil {
				runs = append(runs, row.Schedule)
			}
		}
		if len(runs) != 1 || runs[0].Runs != 6 || runs[0].Outcome != "fired" ||
			runs[0].Since == nil || !runs[0].Since.Equal(base.Add(-90*time.Minute)) {
			t.Errorf("kinds=%q read the schedule as %+v, want one row of six runs since "+
				"the oldest fire, and no skipped tick", kinds, runs)
		}
	}
}
