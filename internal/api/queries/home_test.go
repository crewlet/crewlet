package queries_test

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/doclet"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/period"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/worklet"
)

// fixedRuns is a pending-run store holding exactly these runs.
type fixedRuns []sandbox.PendingRun

func (f fixedRuns) ListActive(context.Context) ([]sandbox.PendingRun, error) {
	return f, nil
}

func (f fixedRuns) Get(_ context.Context, turnID string) (sandbox.PendingRun, bool, error) {
	for _, run := range f {
		if run.TurnID == turnID {
			return run, true, nil
		}
	}
	return sandbox.PendingRun{}, false, nil
}

// WHAT WAITS ON A PERSON IS THEIR OPEN ASKS AND THE RUNS PARKED ON A QUESTION
// TO THEM — their own record when they name nobody, and only runs that are
// actually waiting.
func TestDecisionsCountsAsksAndRunsForThePerson(t *testing.T) {
	t.Parallel()
	askedAt := time.Date(2031, 4, 16, 10, 0, 0, 0, time.UTC)
	oldest := askedAt.Add(-5 * time.Hour)
	parkedAt := askedAt.Add(-time.Hour)
	work := &stubWork{decisions: worklet.DecisionsAnswer{
		Asks: []worklet.AskRow{
			{Comment: "c-1", AskedAt: askedAt},
			{Comment: "c-2", AskedAt: oldest},
		},
		Total:    worklet.ClaimTotal{Total: 7},
		OldestAt: &oldest,
	}}
	sources := viewerSources(t, work)
	sources.Sandbox = fixedRuns{
		{TurnID: "mine", AgentHandle: "swe", Status: sandbox.StatusAwaiting,
			AudienceHandles: []string{"ana"}, UpdatedAt: parkedAt},
		{TurnID: "reseeded", AgentHandle: "swe", Status: sandbox.StatusReseed,
			AudienceHandles: []string{"ana"}, UpdatedAt: parkedAt.Add(-time.Minute)},
		{TurnID: "running", AgentHandle: "swe", Status: sandbox.StatusRunning,
			AudienceHandles: []string{"ana"}, UpdatedAt: askedAt},
		{TurnID: "theirs", AgentHandle: "swe", Status: sandbox.StatusAwaiting,
			AudienceHandles: []string{"bo"}, UpdatedAt: askedAt},
	}
	got, err := askAsSeat(t, sources, "ana", "decisions", nil)
	if err != nil {
		t.Fatalf("decisions: %v", err)
	}
	answer := got.(map[string]any)
	if work.decisionsQuery.Handle != "ana" {
		t.Errorf("the asks were read for %q, want the caller's own record",
			work.decisionsQuery.Handle)
	}
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

// NOBODY TO ANSWER ABOUT IS NOT AN EMPTY LIST: a credential the engine can
// name no record for, naming nobody, is told so — the refusal every personal
// question gives — rather than answered "nothing waits on you".
func TestDecisionsNeedsARecord(t *testing.T) {
	t.Parallel()
	work := &stubWork{}
	r := queries.NewRegistry()
	queries.Register(r, viewerSources(t, work))
	nobody := iam.Principal{ID: uuid.New(), Kind: iam.KindPerson,
		Stage: iam.StageActive, Grants: []iam.Grant{iam.GrantWorklet}}
	_, err := r.Answer(iam.WithPrincipal(t.Context(), nobody), "decisions", nil)
	if !errors.Is(err, queries.ErrBadParams) {
		t.Errorf("a caller with no record naming nobody = %v, want bad params", err)
	}
	if work.decisionsQuery.Handle != "" {
		t.Errorf("the asks were read for %q anyway", work.decisionsQuery.Handle)
	}
}

// AND SOMEBODY ELSE'S DECISIONS TAKE THE PERSON-READ RULE: theirs, whoever
// leads them, or an admin, each with the tracker — the rule `work_my_work`
// takes, because what waits on somebody is their queue.
func TestSomebodyElsesDecisionsTakeTheLeadRelation(t *testing.T) {
	t.Parallel()
	work := &stubWork{}
	if _, err := askHolding(t, viewerSources(t, work), "ana", "decisions",
		map[string]any{"handle": "bo"}, iam.GrantWorklet); !errors.Is(err,
		queries.ErrUnauthorized) {
		t.Errorf("a colleague read bo's decisions = %v, want unauthorized", err)
	}
	if work.decisionsQuery.Handle != "" {
		t.Errorf("a refused read still asked the tracker about %q",
			work.decisionsQuery.Handle)
	}
	s := viewerSources(t, work)
	s.Chart = leadsChart{lead: "ana", report: "bo"}
	if _, err := askHolding(t, s, "ana", "decisions",
		map[string]any{"handle": "bo"}, iam.GrantWorklet); err != nil {
		t.Fatalf("bo's lead was refused his decisions: %v", err)
	}
	if work.decisionsQuery.Handle != "bo" {
		t.Errorf("the lead's read reached %q, want bo", work.decisionsQuery.Handle)
	}
}

// WHAT A PERSON DECIDED LATELY IS READ FOR THEIR OWN RECORD, on the scope rule
// `decisions` takes: a colleague is refused somebody else's, and their lead is
// answered.
func TestAnsweredDecisionsTakeTheDecisionsScope(t *testing.T) {
	t.Parallel()
	work := &stubWork{}
	if _, err := askHolding(t, viewerSources(t, work), "ana", "answered_decisions", nil,
		iam.GrantWorklet); err != nil {
		t.Fatalf("ana's own answered decisions: %v", err)
	}
	if work.answeredQuery.Handle != "ana" {
		t.Errorf("the answered asks were read for %q, want the caller's own record",
			work.answeredQuery.Handle)
	}
	other := &stubWork{}
	if _, err := askHolding(t, viewerSources(t, other), "ana", "answered_decisions",
		map[string]any{"handle": "bo"}, iam.GrantWorklet); !errors.Is(err,
		queries.ErrUnauthorized) {
		t.Errorf("a colleague read bo's answered decisions = %v, want unauthorized", err)
	}
	if other.answeredQuery.Handle != "" {
		t.Errorf("a refused read still asked the tracker about %q",
			other.answeredQuery.Handle)
	}
	s := viewerSources(t, other)
	s.Chart = leadsChart{lead: "ana", report: "bo"}
	if _, err := askHolding(t, s, "ana", "answered_decisions",
		map[string]any{"handle": "bo"}, iam.GrantWorklet); err != nil {
		t.Fatalf("bo's lead was refused his answered decisions: %v", err)
	}
	if other.answeredQuery.Handle != "bo" {
		t.Errorf("the lead's read reached %q, want bo", other.answeredQuery.Handle)
	}
}

// WHAT THE ASKER DID NEXT IS THE TASK'S: a reader the tracker shows no task to
// is answered every ask put to them and who answered it, and none of the task's
// records beyond the question.
func TestAnsweredDecisionsShowTheFollowUpOnlyToATrackerReader(t *testing.T) {
	t.Parallel()
	answered := worklet.AnsweredAnswer{Asks: []worklet.AnsweredAsk{{
		Ask:        worklet.AskRow{Comment: "c-1"},
		Answer:     worklet.AskAnswer{By: "ana", Body: "Ship it"},
		FollowedUp: &worklet.AskFollowUp{Kind: worklet.ChangeStatus},
	}}}
	for _, c := range []struct {
		name   string
		grants []iam.Grant
		shown  bool
	}{
		{"with the tracker", []iam.Grant{iam.GrantWorklet}, true},
		{"without it", nil, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			work := &stubWork{answered: answered}
			got, err := askHolding(t, viewerSources(t, work), "ana", "answered_decisions",
				nil, c.grants...)
			if err != nil {
				t.Fatalf("answered_decisions: %v", err)
			}
			items := got.(map[string]any)["items"].([]any)
			if len(items) != 1 {
				t.Fatalf("items are %+v, want the one answered ask", items)
			}
			item, ok := items[0].(worklet.AnsweredAsk)
			if !ok || item.Answer.Body != "Ship it" {
				t.Fatalf("items are %+v, want the one answered ask", items)
			}
			if (item.FollowedUp != nil) != c.shown {
				t.Errorf("the follow-up is %+v, want shown=%v", item.FollowedUp, c.shown)
			}
		})
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
		{"points": worklet.MaxFlowPoints + 1},
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
	work := &stubWork{feed: worklet.FeedPage{Rows: []worklet.FeedRow{
		{Task: "a", Kind: worklet.FeedCompleted, At: base,
			Cursor: worklet.FeedCursor{At: base, Seq: 9}.String()},
		{Task: "b", Kind: worklet.FeedCreated, At: base.Add(-2 * time.Hour),
			Cursor: worklet.FeedCursor{At: base.Add(-2 * time.Hour), Seq: 5}.String()},
	}, Complete: true}}
	pageStore := &stubPages{changes: []doclet.PageChange{
		{ID: "h-1", PageID: "p", Kind: doclet.ChangeSaved, At: base.Add(-time.Hour), LogSeq: 4},
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
	if err := db.Replicated().Tx(t.Context(), func(tx *sql.Tx) error {
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
	work := &stubWork{feed: worklet.FeedPage{Rows: []worklet.FeedRow{
		{Task: "a", Kind: worklet.FeedCompleted, At: base,
			Cursor: worklet.FeedCursor{At: base, Seq: 9}.String()},
	}, Complete: true}}
	sources := queries.Sources{Work: work, Usage: db.Replicated()}
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

// EACH FEED ROW IS SHOWN BY ITS APP (ADR-0033): the work the tracker records
// to whoever holds Worklet, the pages to whoever holds Doclet,
// and the schedules' runs — the company's own machinery — to everybody, an
// admin holding all three. A kind the caller may not see is DROPPED rather than
// refused, named in `kinds=` or not, and its source is never read: a refusal of
// `kinds=page` would confirm the pages exist to somebody who may not see them.
//
// Mutation: skip the kind filter in [queries.Sources.companyFeed] and the
// overview-only caller is shown a completion and a page save.
func TestTheFeedShowsOnlyTheAppsTheCallerHolds(t *testing.T) {
	t.Parallel()
	base := time.Date(2031, 4, 16, 10, 0, 0, 0, time.UTC)
	db := openStore(t)
	if err := db.Replicated().Tx(t.Context(), func(tx *sql.Tx) error {
		at := base.Add(-30 * time.Minute)
		_, err := tx.ExecContext(t.Context(), `
			INSERT INTO usage_schedule_runs
				(day, node, scope_type, scope_id, name, fired_at, target,
				 outcome, trace_id, turn_id, version)
			VALUES (?, 'n1', 'role', 'pm', 'standup', ?, 'pm', 'fired', '', '', 1)`,
			at.Format("2006-01-02"), store.EncodeTime(at))
		return err
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	for _, c := range []struct {
		name    string
		holding []iam.Grant
		params  map[string]any
		want    []queries.FeedKind
	}{
		// NEWEST FIRST: the completion, the schedule's run half an hour
		// before it, the page save an hour before.
		{"an admin", []iam.Grant{iam.GrantAdmin}, nil,
			[]queries.FeedKind{queries.FeedCompleted, queries.FeedSchedule, queries.FeedPage}},
		{"the tracker", []iam.Grant{iam.GrantWorklet}, nil,
			[]queries.FeedKind{queries.FeedCompleted, queries.FeedSchedule}},
		{"the knowledge base", []iam.Grant{iam.GrantDoclet}, nil,
			[]queries.FeedKind{queries.FeedSchedule, queries.FeedPage}},
		{"nothing", nil, nil, []queries.FeedKind{queries.FeedSchedule}},
		{"the tracker, naming the pages", []iam.Grant{iam.GrantWorklet},
			map[string]any{"kinds": "page"}, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			work := &stubWork{feed: worklet.FeedPage{Rows: []worklet.FeedRow{
				{Task: "a", Kind: worklet.FeedCompleted, At: base,
					Cursor: worklet.FeedCursor{At: base, Seq: 9}.String()},
			}, Complete: true}}
			pageStore := &stubPages{changes: []doclet.PageChange{
				{ID: "h-1", PageID: "p", Kind: doclet.ChangeSaved,
					At: base.Add(-time.Hour), LogSeq: 4},
			}}
			r := queries.NewRegistry()
			queries.Register(r, queries.Sources{Work: work, Pages: pageStore,
				Usage: db.Replicated()})
			got, err := r.Answer(asGrants(t, "", c.holding...), "company_feed", c.params)
			if err != nil {
				t.Fatalf("company_feed: %v", err)
			}
			var kinds []queries.FeedKind
			for _, row := range got.(map[string]any)["rows"].([]queries.FeedEntry) {
				kinds = append(kinds, row.Kind)
			}
			if !slices.Equal(kinds, c.want) {
				t.Errorf("the feed showed %v, want %v", kinds, c.want)
			}
			if !slices.Contains(c.want, queries.FeedCompleted) && len(work.feedQuery.Kinds) != 0 {
				t.Errorf("the tracker was read for %v, a caller it is not shown to",
					work.feedQuery.Kinds)
			}
			if !slices.Contains(c.want, queries.FeedPage) && len(pageStore.activity.Kinds) != 0 {
				t.Errorf("the pages were read for %v, a caller they are not shown to",
					pageStore.activity.Kinds)
			}
		})
	}
}

// YOUR OWN ASKS NEED NO TRACKER (ADR-0033): a person kept off the tracker is
// still asked things, may read the item an open ask names them on and post the
// answer without it — so the list of what waits on them is theirs to read
// whatever they hold. A parked executor's question put to them is shown, and
// the task behind it, its seat's trail, is not, nor the entry its job was
// launched on, which is an admin's.
//
// Somebody else's list is still the tracker's: a lead without it reads no
// report's, and is told the tracker would admit them.
//
// Mutation: register `decisions` as the tracker's, or decide the caller's own
// record by the person-read rule, and the first half goes red.
func TestYourOwnAsksNeedNoTracker(t *testing.T) {
	t.Parallel()
	work := &stubWork{decisions: worklet.DecisionsAnswer{Asks: []worklet.AskRow{
		{Comment: "c-1", AskedAt: time.Date(2031, 4, 16, 10, 0, 0, 0, time.UTC)},
	}, Total: worklet.ClaimTotal{Total: 1}}}
	s := viewerSources(t, work)
	s.Sandbox = fixedRuns{parkedRun()}
	got, err := askHolding(t, s, "ana", "decisions", nil)
	if err != nil {
		t.Fatalf("ana's own decisions, holding nothing: %v", err)
	}
	if work.decisionsQuery.Handle != "ana" {
		t.Errorf("the asks were read for %q, want ana", work.decisionsQuery.Handle)
	}
	items := got.(map[string]any)["items"].([]queries.DecisionItem)
	var ask, run *queries.DecisionItem
	for i := range items {
		switch items[i].Kind {
		case "ask":
			ask = &items[i]
		case "run":
			run = &items[i]
		}
	}
	if ask == nil || ask.Ask.Comment != "c-1" {
		t.Errorf("the ask put to ana is not listed: %+v", items)
	}
	if run == nil || run.Run["question"] != "which branch?" {
		t.Fatalf("the run's question put to ana is not listed: %+v", items)
	}
	if _, shown := run.Run["task_description"]; shown {
		t.Error("ana was shown the run's task, which is its seat's trail")
	}
	if _, shown := run.Run["work_item"]; shown {
		t.Error("ana, holding no tracker, was shown the run's work item")
	}
	// THE ENTRY THE RUN'S JOB WAS LAUNCHED ON IS THE COMPANY'S CONFIGURATION,
	// as the board withholds it: the decisions answer serialises the run the
	// board's way, so ana, no admin, is told the model and not the entry.
	if _, shown := run.Run["provider_key"]; shown || run.Run["model"] != "opus" {
		t.Errorf("ana was shown the run's entry %v, model %v: want the model alone",
			run.Run["provider_key"], run.Run["model"])
	}

	lead := viewerSources(t, &stubWork{})
	lead.Chart = leadsChart{lead: "ana", report: "bo"}
	_, err = askHolding(t, lead, "ana", "decisions", map[string]any{"handle": "bo"})
	var refusal *queries.Refusal
	if !errors.As(err, &refusal) || !slices.Equal(refusal.Grants, []iam.Grant{iam.GrantWorklet}) {
		t.Errorf("a lead without Worklet reading a report's decisions = %v, "+
			"want a refusal naming [worklet]", err)
	}
}
