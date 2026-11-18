package worklet_test

import (
	"database/sql"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/inbox"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/worklet"
)

func (r *roundTrip) activity(q worklet.ActivityQuery) worklet.ActivityAnswer {
	r.t.Helper()
	if q.Level == "" {
		q.Level = statelog.ReadStale
	}
	answer, err := r.reader.Activity(r.t.Context(), q, wednesday)
	if err != nil {
		r.t.Fatalf("Activity(%+v): %v", q, err)
	}
	return answer
}

// A QUIET COMMIT IS IN THE FEED. "Quiet" means it woke nobody, not that it did
// not happen — and a feed assembled from the notifications would be an account
// of what was ANNOUNCED rather than of what was done.
func TestTheActivityFeedCarriesQuietCommits(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	filedTask(t, r, "t-1")
	done := worklet.StatusDone
	// NO NOTIFY: this change tells nobody.
	if _, err := r.writer.UpdateTask(t.Context(), "op-quiet", "t-1", "ENG",
		worklet.NoIfMatch, worklet.TaskPatch{Status: &done}, worklet.ChangeStatus, nil); err != nil {
		t.Fatalf("UpdateTask: %v", err)
	}
	r.drain()

	answer := r.activity(worklet.ActivityQuery{Task: "t-1"})
	if len(answer.Records) < 2 {
		t.Fatalf("the feed carries %d records for a task that was created and "+
			"moved, want both — a quiet commit is still something that "+
			"happened", len(answer.Records))
	}
	// NEWEST FIRST, and the order is the LOG's rather than any clock's:
	// two nodes' authored instants can tie and can run backwards, and
	// neither says which commit the broker accepted first.
	if answer.Records[0].Kind != worklet.ChangeStatus {
		t.Fatalf("the newest record is %q, want the status change — the feed "+
			"is newest first", answer.Records[0].Kind)
	}
	first := answer.Records[0]
	if first.Notified {
		t.Error("a commit that carried no notification is reported as notified " +
			"— `notified` is how a reader tells `nothing was announced` from " +
			"`nothing happened`")
	}
	if first.Fields["status"].To != string(worklet.StatusDone) {
		t.Errorf("the record's deltas are %+v, want the status it moved to",
			first.Fields)
	}
	if first.SubjectKey == "" {
		t.Error("the record names no task key — a feed of uuids is a feed " +
			"nobody reads")
	}
	if first.LogStream == "" || first.LogSeq == 0 {
		t.Errorf("the record carries position %s@%d:%d, want the whole triple "+
			"— a bare sequence names no stream and no generation, so a cursor "+
			"built from one cannot survive a reanchor",
			first.LogStream, first.LogGeneration, first.LogSeq)
	}
	// BOTH INSTANTS: the authored one is what a card renders, the effective
	// one what every duration is measured on; carrying only one silently
	// answers another question.
	if first.At.IsZero() || first.EffectiveAt.IsZero() {
		t.Errorf("the record carries at=%v effective_at=%v, want both",
			first.At, first.EffectiveAt)
	}
}

// A TEXT SEARCH IS GATED ON WHAT IT WOULD SCAN, never on which keys were
// named: `container=workspace&q=` names a container and an unbounded `since:`
// a bound, and both run the full scan the gate exists to stop.
func TestAnActivitySearchIsRefusedByWhatItWouldScan(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)

	// (a) COMPANY SCOPE with a text search: refused naming BOTH keys.
	_, err := r.reader.Activity(t.Context(), worklet.ActivityQuery{
		Q: "deploy", Workspace: true, Level: statelog.ReadStale,
	}, wednesday)
	if err == nil {
		t.Fatal("a company-wide text search over the feed answered — it reads " +
			"every commit this company has ever made")
	}
	for _, want := range []string{"task", "container", "since"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal is %q and does not name %q — a gate that "+
				"names one key is a gate a caller satisfies without narrowing "+
				"anything", err, want)
		}
	}

	// (b) A PROJECT WITH NO WINDOW is refused too.
	if _, err := r.reader.Activity(t.Context(), worklet.ActivityQuery{
		Q: "deploy", Project: "ENG", Level: statelog.ReadStale,
	}, wednesday); err == nil {
		t.Error("a project-wide text search with no window answered — it " +
			"reads the project's whole history")
	}

	// (c) AND A WINDOW WIDER THAN THE SPAN: a five-year `since:` narrows
	// nothing.
	_, err = r.reader.Activity(t.Context(), worklet.ActivityQuery{
		Q: "deploy", Project: "ENG", Level: statelog.ReadStale,
		SinceAt: wednesday.AddDate(-5, 0, 0),
	}, wednesday)
	if err == nil {
		t.Fatal("a five-year text search over a project answered")
	}
	if !strings.Contains(err.Error(), "90") {
		t.Errorf("the refusal is %q and does not name the span", err)
	}

	// (d) INSIDE THE SPAN IT ANSWERS, and so does a search scoped to one
	// task at any age — one subject is an index range however old.
	r.activity(worklet.ActivityQuery{
		Q: "deploy", Project: "ENG", SinceAt: wednesday.AddDate(0, 0, -30),
	})
	filedTask(t, r, "t-1")
	r.activity(worklet.ActivityQuery{Q: "deploy", Task: "t-1"})

	// AND A QUERY WITH NO `q` IS NEVER GATED: `kinds`, `actor` and the
	// position cursor are all indexed, so a company-wide feed is the
	// ordinary case rather than the dangerous one.
	r.activity(worklet.ActivityQuery{Workspace: true})
}

// A FEED'S POSITIONS ARE THE CALLER'S TO GET RIGHT, AND A MISTAKE IN ONE IS
// REFUSED AS THE REQUEST'S.
//
// Both feeds compare their cursor and `since` against `log_seq`, a packed
// column carrying no stream, so a position from another domain's log would
// silently pick a page, and an unparsable cursor would fail inside the read's
// transaction like a node fault. Each is refused before a row is read with
// [worklet.ErrBadQuery], the sentinel a surface answers as a bad request (a
// foreign one also with [statelog.ErrForeignPosition]), never as a state-log
// refusal; so are the activity gate and an unknown inbox reason. The control
// is the feed's own cursor, which pages.
func TestAFeedRefusesAPositionItCannotUseAsTheRequestsMistake(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	filedTask(t, r, "t-1")
	filedTask(t, r, "t-2")

	pages := statelog.Position{Stream: "CREWLET_PAGES_LOG", Generation: 1, Seq: 5}
	type ask struct {
		name    string
		run     func() error
		foreign bool
	}
	activity := func(q worklet.ActivityQuery) func() error {
		return func() error {
			q.Level = statelog.ReadStale
			_, err := r.reader.Activity(t.Context(), q, wednesday)
			return err
		}
	}
	inboxRead := func(q worklet.InboxQuery) func() error {
		return func() error {
			q.Handle, q.Level = "bob", statelog.ReadStale
			// THE SCOPE EVERY SURFACE DEFAULTS TO, so the case reaches
			// the position it is about rather than the absent scope.
			q.Snoozed = inbox.SnoozeExclude
			_, err := r.reader.Inbox(t.Context(), q, wednesday)
			return err
		}
	}
	for _, c := range []ask{
		{"an activity cursor from another log",
			activity(worklet.ActivityQuery{Workspace: true, Cursor: pages.String()}), true},
		{"an activity since from another log",
			activity(worklet.ActivityQuery{Workspace: true, Since: pages}), true},
		{"an activity cursor that is not a position",
			activity(worklet.ActivityQuery{Workspace: true, Cursor: "42"}), false},
		{"an activity search wider than the feed scans",
			activity(worklet.ActivityQuery{Workspace: true, Q: "deploy"}), false},
		{"an inbox cursor from another log",
			inboxRead(worklet.InboxQuery{Cursor: pages.String()}), true},
		{"an inbox since from another log",
			inboxRead(worklet.InboxQuery{Since: pages}), true},
		{"an inbox cursor that is not a position",
			inboxRead(worklet.InboxQuery{Cursor: "42"}), false},
		{"an unknown wake reason",
			inboxRead(worklet.InboxQuery{Reasons: []worklet.Reason{"because-i-said-so"}}), false},
	} {
		err := c.run()
		if !errors.Is(err, worklet.ErrBadQuery) {
			t.Errorf("%s: %v, want %v", c.name, err, worklet.ErrBadQuery)
			continue
		}
		if c.foreign && !errors.Is(err, statelog.ErrForeignPosition) {
			t.Errorf("%s: %v, want it to say the position is on another log", c.name, err)
		}
		if errors.Is(err, statelog.ErrUnavailable) {
			t.Errorf("%s: %v is a state-log refusal — the request is what is "+
				"wrong, on every node alike", c.name, err)
		}
	}

	// THE CONTROL: the feed's own cursor is on this log and pages.
	first := r.activity(worklet.ActivityQuery{Workspace: true, Limit: 1})
	if first.NextCursor == "" {
		t.Fatal("a one-row page over two commits carries no cursor")
	}
	if next := r.activity(worklet.ActivityQuery{
		Workspace: true, Limit: 1, Cursor: first.NextCursor,
	}); len(next.Records) != 1 {
		t.Errorf("the feed's own cursor paged to %d records, want 1", len(next.Records))
	}
}

// THE CURSOR IS A LOG POSITION, and it pages with no gap and no repeat.
func TestTheActivityCursorPagesByPosition(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	for i := range 5 {
		filedTask(t, r, "t-"+itoa(i))
	}

	first := r.activity(worklet.ActivityQuery{Workspace: true, Limit: 2})
	if len(first.Records) != 2 || first.NextCursor == "" {
		t.Fatalf("the first page has %d records and cursor %q, want 2 and a "+
			"cursor", len(first.Records), first.NextCursor)
	}
	second := r.activity(worklet.ActivityQuery{
		Workspace: true, Limit: 2, Cursor: first.NextCursor,
	})
	if len(second.Records) != 2 {
		t.Fatalf("the second page has %d records, want 2", len(second.Records))
	}
	// NO REPEAT, which a cursor comparing anything but the total order
	// cannot promise: two commits share an authored instant routinely.
	seen := map[string]bool{}
	for _, record := range append(first.Records, second.Records...) {
		if seen[record.ID] {
			t.Fatalf("record %s is on both pages — the cursor is not strictly "+
				"after the last row", record.ID)
		}
		seen[record.ID] = true
	}
	// AND STRICTLY DESCENDING: the page after is older, always.
	if second.Records[0].LogSeq >= first.Records[1].LogSeq {
		t.Errorf("the second page starts at %d and the first ended at %d, want "+
			"strictly older", second.Records[0].LogSeq, first.Records[1].LogSeq)
	}

	// A CURSOR THAT IS NOT A POSITION IS REFUSED naming the shape, never
	// silently ignored — a page that quietly restarted from the top would
	// loop for ever.
	if _, err := r.reader.Activity(t.Context(), worklet.ActivityQuery{
		Workspace: true, Cursor: "42", Level: statelog.ReadStale,
	}, wednesday); err == nil {
		t.Error("a bare sequence was accepted as a cursor — it names no " +
			"stream and no generation")
	}
}

// THE FILTERS NARROW, and each one is a different question.
func TestTheActivityFiltersNarrow(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	filedTask(t, r, "t-1")
	filedTask(t, r, "t-2")
	done := worklet.StatusDone
	if _, err := r.writer.UpdateTask(t.Context(), "op-done", "t-1", "ENG",
		worklet.NoIfMatch, worklet.TaskPatch{Status: &done}, worklet.ChangeStatus, nil); err != nil {
		t.Fatalf("UpdateTask: %v", err)
	}
	r.drain()

	byKind := r.activity(worklet.ActivityQuery{
		Workspace: true, Kinds: []worklet.ChangeKind{worklet.ChangeStatus},
	})
	if len(byKind.Records) != 1 || byKind.Records[0].SubjectID != "t-1" {
		t.Fatalf("kinds=status answers %d records, want the one status change",
			len(byKind.Records))
	}
	byTask := r.activity(worklet.ActivityQuery{Task: "t-2"})
	for _, record := range byTask.Records {
		if record.SubjectID != "t-2" {
			t.Fatalf("task=t-2 answers a record about %s", record.SubjectID)
		}
	}
	// A KEY RESOLVES, IN ANY CASE: a key, not a uuid, is what somebody
	// pastes out of a chat message.
	key := byTask.Records[0].SubjectKey
	if key == "" {
		t.Fatal("the feed names no key for a task, so this case tests nothing")
	}
	if got := r.activity(worklet.ActivityQuery{
		Task: strings.ToLower(key),
	}); len(got.Records) == 0 {
		t.Errorf("the feed answered nothing for %q — a key is uppercased "+
			"before it is resolved, because `eng-9` is the same task as "+
			"`ENG-9`", strings.ToLower(key))
	}
	if _, err := r.reader.Activity(t.Context(), worklet.ActivityQuery{
		Task: "nope", Level: statelog.ReadStale,
	}, wednesday); err == nil {
		t.Error("the feed answered for an unknown task — an empty history and " +
			"a typo are different facts")
	}

	// AND `actor` IS NOT `assignee`. One is who did it, the other whose
	// work it is, and they are routinely different people.
	byActor := r.activity(worklet.ActivityQuery{Workspace: true, Actor: "ana"})
	if len(byActor.Records) == 0 {
		t.Error("actor=ana answers nothing, and every commit here is hers")
	}
	if got := r.activity(worklet.ActivityQuery{
		Workspace: true, Actor: "nobody",
	}); len(got.Records) != 0 {
		t.Errorf("actor=nobody answers %d records", len(got.Records))
	}
}

// A KIND THAT NAMES A FIELD KEEPS EVERY CHANGE THAT MOVED THE FIELD, whatever
// kind its record was filed under: a record carries one kind, so a write moving
// two fields is filed under one of them. Each case moves its field under
// another kind; a change that moved none of the field is not kept.
//
// Mutation: filter on `h.kind IN (…)` alone, and no case keeps its change.
func TestAKindThatNamesAFieldKeepsEveryChangeThatMovedIt(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	ana, bo, unit := "ana", "bo", "platform"
	started := worklet.StatusInProgress
	urgent := []string{"urgent"}
	cases := []struct {
		id    string
		asked worklet.ChangeKind
		patch worklet.TaskPatch
		filed worklet.ChangeKind
	}{
		{"t-assignee", worklet.ChangeAssignee,
			worklet.TaskPatch{Assignee: &ana, Status: &started}, worklet.ChangeStatus},
		{"t-status", worklet.ChangeStatus,
			worklet.TaskPatch{Status: &started, Tags: &urgent}, worklet.ChangeTags},
		{"t-tags", worklet.ChangeTags,
			worklet.TaskPatch{Assignee: &bo, Tags: &urgent}, worklet.ChangeAssignee},
		{"t-watchers", worklet.ChangeWatchers,
			worklet.TaskPatch{Status: &started, Watchers: &[]string{"ana"}}, worklet.ChangeStatus},
		{"t-collaborators", worklet.ChangeCollaborators,
			worklet.TaskPatch{Assignee: &ana, Collaborators: &[]string{"bo"}}, worklet.ChangeAssignee},
		{"t-routed", worklet.ChangeRouted,
			worklet.TaskPatch{Status: &started, RoutingUnit: &unit}, worklet.ChangeStatus},
		{"t-relations", worklet.ChangeRelations, worklet.TaskPatch{Assignee: &ana,
			Relate: &worklet.RelationIntent{Add: []worklet.Relation{{
				Kind: worklet.RelationLinked, Other: "t-other",
			}}}}, worklet.ChangeAssignee},
	}
	if _, err := r.writer.WriteTags(t.Context(), "op-tags-urgent", "ENG",
		worklet.TagEdit{Add: []worklet.Tag{{Slug: "urgent", Label: "Urgent"}}}, worklet.TagAuthority{}); err != nil {
		t.Fatalf("declare ENG's tag: %v", err)
	}
	r.drain()
	filedTask(t, r, "t-other")
	for _, c := range cases {
		filedTask(t, r, c.id)
		if _, err := r.writer.UpdateTask(t.Context(), "op-"+c.id+"-moved", c.id, "ENG",
			worklet.NoIfMatch, c.patch, c.filed, nil); err != nil {
			t.Fatalf("move %s's %s under %s: %v", c.id, c.asked, c.filed, err)
		}
		r.drain()
	}
	for _, c := range cases {
		got := r.activity(worklet.ActivityQuery{Task: c.id, Kinds: []worklet.ChangeKind{c.asked}})
		if !slices.ContainsFunc(got.Records, func(record worklet.ActivityRecord) bool {
			return record.Kind == c.filed
		}) {
			t.Errorf("kinds=%s on %s keeps no %s change, and that change moved its %s",
				c.asked, c.id, c.filed, c.asked)
		}
	}
	// A CHANGE THAT MOVED NONE OF THE FIELD IS NOT KEPT: t-status's moved its
	// status and its tags, and nobody ever assigned it.
	if got := r.activity(worklet.ActivityQuery{Task: "t-status",
		Kinds: []worklet.ChangeKind{worklet.ChangeAssignee}}); len(got.Records) != 0 {
		t.Errorf("kinds=assignee on t-status keeps %d changes, and none of them moved its assignee",
			len(got.Records))
	}
}

// THE FEED NARROWS TO WHO WAS WRITING, which is a different question from
// which handle. Actor names are disjoint per kind (an operator commit carries
// the token's label, an agent one a seat handle), so the audit screen's "what
// did the operators do" cannot be a set of handles: that is the roster, which
// changes and would drop a commit by somebody who has left. Both directions,
// because narrowing to nothing reads as a quiet company and narrowing to
// everything as one with no seats.
func TestTheFeedNarrowsToWhoWasWriting(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	filedTask(t, r, "t-1")

	// The harness's own writer is a HUMAN; this one is the operator token,
	// which is what an audit is about.
	operator := r.writer.As("ops-1", worklet.AuthorOperator, worklet.Provenance{})
	done := worklet.StatusDone
	if _, err := operator.UpdateTask(t.Context(), "op-by-token", "t-1", "ENG",
		worklet.NoIfMatch, worklet.TaskPatch{Status: &done}, worklet.ChangeStatus,
		nil); err != nil {
		t.Fatalf("UpdateTask as the operator: %v", err)
	}
	r.drain()

	all := r.activity(worklet.ActivityQuery{Workspace: true})
	if len(all.Records) < 2 {
		t.Fatalf("the feed carries %d records, want the human's create and the "+
			"operator's change — this case tests nothing without both",
			len(all.Records))
	}

	byOperator := r.activity(worklet.ActivityQuery{
		Workspace: true, ActorKinds: []worklet.AuthorKind{worklet.AuthorOperator},
	})
	if len(byOperator.Records) != 1 {
		t.Fatalf("actor_kinds=operator answers %d records, want the one commit "+
			"a token made", len(byOperator.Records))
	}
	if got := byOperator.Records[0]; got.ActorKind != worklet.AuthorOperator {
		t.Errorf("the record it answered was written by a %s", got.ActorKind)
	}

	// A SET IS A UNION, not an intersection: "everything a person or a
	// token did" is one question and it names two kinds.
	both := r.activity(worklet.ActivityQuery{
		Workspace: true,
		ActorKinds: []worklet.AuthorKind{
			worklet.AuthorOperator, worklet.AuthorHuman,
		},
	})
	if len(both.Records) != len(all.Records) {
		t.Errorf("actor_kinds=operator,human answers %d of %d records, and every "+
			"commit here was written by one or the other",
			len(both.Records), len(all.Records))
	}

	// AND A KIND NOBODY WROTE UNDER ANSWERS NOTHING, not every commit: on an
	// audit surface an ignored filter is a wider answer shaped like the
	// narrow one.
	if got := r.activity(worklet.ActivityQuery{
		Workspace: true, ActorKinds: []worklet.AuthorKind{worklet.AuthorSystem},
	}); len(got.Records) != 0 {
		t.Errorf("actor_kinds=system answers %d records, and the engine wrote "+
			"none of them", len(got.Records))
	}
}

// AN AUDIT OF SEVERAL KINDS IS ONE FEED, PAGED IN THE LOG'S ORDER. Each author
// kind is its own arm and the arms are merged, so across pages every commit of
// every named kind comes back once, newest first, interleaved as the log made
// them (a merge dropping its ORDER BY or LIMIT answers one kind's page, then
// the other's), and a kind named twice is read once.
func TestAnAuditOfSeveralKindsIsOneFeedInTheLogsOrder(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	filedTask(t, r, "t-1")
	operator := r.writer.As("ops-1", worklet.AuthorOperator, worklet.Provenance{})
	agent := r.writer.As("swe", worklet.AuthorAgent, worklet.Provenance{WrittenFor: types.ForTurn(nil)})
	for i, writer := range []*worklet.Writer{operator, r.writer, agent, operator,
		r.writer, operator} {
		title := "title " + itoa(i)
		if _, err := writer.UpdateTask(t.Context(), "op-edit-"+itoa(i), "t-1", "ENG",
			worklet.NoIfMatch, worklet.TaskPatch{Title: &title}, worklet.ChangeFields,
			nil); err != nil {
			t.Fatalf("edit %d: %v", i, err)
		}
		r.drain()
	}
	audit := worklet.ActivityQuery{Workspace: true, ActorKinds: []worklet.AuthorKind{
		worklet.AuthorOperator, worklet.AuthorHuman, worklet.AuthorOperator}}

	var want []uint64
	for _, record := range r.activity(worklet.ActivityQuery{Workspace: true}).Records {
		if record.ActorKind == worklet.AuthorOperator || record.ActorKind == worklet.AuthorHuman {
			want = append(want, record.LogSeq)
		}
	}
	if len(want) < 6 {
		t.Fatalf("the feed carries %d operator and human commits, want at least "+
			"the create and five edits", len(want))
	}
	var got []uint64
	for page := 0; page < len(want); page++ {
		audit.Limit = 2
		answer := r.activity(audit)
		for _, record := range answer.Records {
			got = append(got, record.LogSeq)
		}
		if answer.NextCursor == "" {
			break
		}
		audit.Cursor = answer.NextCursor
	}
	if !slices.Equal(got, want) {
		t.Errorf("the audit paged as %v, want %v — every operator and human "+
			"commit once, newest first, as the log made them", got, want)
	}
}

// A TASK'S ACTIVITY IS ITS WHOLE HISTORY, AND THE CONTAINER BESIDE IT A CHECK.
//
// A history row carries the project it was WRITTEN in, so a container used as
// a row filter would hide every change before the task moved, its creation
// among them. A tracker holder reads the whole tracker, so the container only
// checks the task is filed there; a wrong one is refused naming the right one,
// never answered with nothing, which reads as a task nothing happened to.
func TestATasksActivityIsItsWholeHistoryInItsContainer(t *testing.T) {
	t.Parallel()
	r := moveFixture(t)
	title := "before the move"
	if _, err := r.writer.UpdateTask(t.Context(), "op-before", "m-root", "ENG",
		worklet.NoIfMatch, worklet.TaskPatch{Title: &title}, worklet.ChangeFields,
		nil); err != nil {
		t.Fatalf("edit before the move: %v", err)
	}
	r.drain()
	if _, err := r.writer.MoveTaskToProject(t.Context(),
		statelog.NewOpID(time.Now(), "move"), "m-root", "OPS", nil); err != nil {
		t.Fatalf("move: %v", err)
	}
	r.drain()

	whole := r.activity(worklet.ActivityQuery{Task: "m-root"})
	inOPS := r.activity(worklet.ActivityQuery{Task: "m-root", Project: "OPS"})
	var before bool
	for _, record := range whole.Records {
		before = before || record.Project == "ENG"
	}
	if !before {
		t.Fatalf("the task's own feed holds no row written in ENG, so this case "+
			"tests nothing: %+v", whole.Records)
	}
	if len(inOPS.Records) != len(whole.Records) {
		t.Errorf("the task's activity in OPS answers %d of its %d records — the "+
			"ones written before it moved are missing", len(inOPS.Records),
			len(whole.Records))
	}

	_, err := r.reader.Activity(t.Context(), worklet.ActivityQuery{
		Task: "m-root", Project: "ENG", Level: statelog.ReadStale,
	}, wednesday)
	if !errors.Is(err, worklet.ErrBadQuery) || !strings.Contains(err.Error(), "OPS") {
		t.Errorf("the task's activity in ENG, which it left, = %v, want a bad "+
			"query naming OPS, the project it is filed in", err)
	}
}

// A KIND THE FEED HAS NO ROWS OF IS REFUSED NAMING THE SET, never answered with
// an empty page, which reads exactly like a quiet project. The read itself
// refuses it, as [worklet.ErrBadQuery], so every surface (a seat's
// `task_activity` too) answers alike; the control, a kind the feed has, still
// pages.
//
// Mutation: drop activityFilters from Activity and both refusals read an empty
// page instead.
func TestAnActivityFilterOnAKindThisBuildDoesNotHaveIsRefused(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	filedTask(t, r, "t-1")
	for _, c := range []struct {
		name string
		q    worklet.ActivityQuery
		want string
	}{
		{"a change kind", worklet.ActivityQuery{Workspace: true,
			Kinds: []worklet.ChangeKind{"status_changed"}}, "status_changed"},
		{"an author kind", worklet.ActivityQuery{Workspace: true,
			ActorKinds: []worklet.AuthorKind{"robot"}}, "robot"},
	} {
		c.q.Level = statelog.ReadStale
		_, err := r.reader.Activity(t.Context(), c.q, wednesday)
		if !errors.Is(err, worklet.ErrBadQuery) {
			t.Errorf("%s nobody has: %v, want %v", c.name, err, worklet.ErrBadQuery)
			continue
		}
		if !strings.Contains(err.Error(), c.want) || !strings.Contains(err.Error(), "created") &&
			!strings.Contains(err.Error(), "agent") {
			t.Errorf("%s: %q does not name what was asked and what is accepted",
				c.name, err)
		}
	}
	// THE CONTROL.
	if got := r.activity(worklet.ActivityQuery{Workspace: true,
		Kinds: []worklet.ChangeKind{worklet.ChangeCreated}}); len(got.Records) != 1 {
		t.Errorf("a kind the feed has answered %d records, want the one create",
			len(got.Records))
	}
}

// THE FEED SAYS WHERE ITS NODE IS ON THE LOG, and how much of it produced its
// rows: the audit tells a node is behind by these two numbers alone. A node
// retaining a record it cannot decode moves its checkpoint past it, so only the
// applied prefix stopping below the record shows it; a record retained ABOVE
// the checkpoint holds nothing back, so the prefix is the checkpoint, never
// beyond it. The rule is [statelog.PrefixIn]'s.
func TestTheFeedReportsItsPositionAndAppliedPrefix(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	filedTask(t, r, "t-1")
	filedTask(t, r, "t-2")

	caught := r.activity(worklet.ActivityQuery{})
	if len(caught.Records) < 2 {
		t.Fatalf("the feed carries %d records, want both tasks' — this case "+
			"tests nothing without them", len(caught.Records))
	}
	at := func(record worklet.ActivityRecord) uint64 {
		return uint64(statelog.Position{
			Generation: record.LogGeneration, Seq: record.LogSeq,
		}.Packed())
	}
	newest, oldest := at(caught.Records[0]), at(caught.Records[len(caught.Records)-1])
	if caught.LogSeq < newest {
		t.Errorf("the feed is at %d, below its own newest record at %d",
			caught.LogSeq, newest)
	}
	if caught.AppliedThrough != caught.LogSeq {
		t.Errorf("a node that retains nothing applied through %d of %d",
			caught.AppliedThrough, caught.LogSeq)
	}

	// A RECORD RETAINED AT THE OLDEST RECORD'S POSITION: the checkpoint is
	// past it, and the applied prefix stops just below it.
	r.retainAt(oldest)
	behind := r.activity(worklet.ActivityQuery{})
	if behind.LogSeq != caught.LogSeq {
		t.Errorf("retaining a record moved the position from %d to %d",
			caught.LogSeq, behind.LogSeq)
	}
	if behind.AppliedThrough != oldest-1 {
		t.Errorf("a node retaining the record at %d applied through %d, want %d",
			oldest, behind.AppliedThrough, oldest-1)
	}

	// AND ONE ABOVE THE CHECKPOINT holds back nothing the checkpoint passed.
	r.clearRetained()
	r.retainAt(caught.LogSeq + 1_000)
	ahead := r.activity(worklet.ActivityQuery{})
	if ahead.AppliedThrough != ahead.LogSeq {
		t.Errorf("a record retained above the checkpoint put the applied "+
			"prefix at %d against a checkpoint of %d — an applied prefix "+
			"past the checkpoint is a position no record reached",
			ahead.AppliedThrough, ahead.LogSeq)
	}
}

// retainAt files a record this node "could not decode" at a packed position,
// the rows the framework's own loop writes when it retains one.
func (r *roundTrip) retainAt(position uint64) {
	r.t.Helper()
	if err := r.db.Tx(r.t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(r.t.Context(), `
			INSERT INTO tracker_log_deferred
				(position, subject, subject_kind, subject_id, version, payload, stored_at)
			VALUES (?, 'task.x', 'task', 'x', ?, x'00', 0)`,
			int64(position), worklet.RecordVersion+1)
		return err
	}); err != nil {
		r.t.Fatalf("retain a record: %v", err)
	}
}

// clearRetained forgets every record [roundTrip.retainAt] filed.
func (r *roundTrip) clearRetained() {
	r.t.Helper()
	if err := r.db.Tx(r.t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(r.t.Context(), `DELETE FROM tracker_log_deferred`)
		return err
	}); err != nil {
		r.t.Fatalf("clear the retained records: %v", err)
	}
}

// A PERSON'S OWN BOOKKEEPING IS NOT IN THE COMPANY'S HISTORY: every pin, inbox
// mark and snooze writes a `person_updated` row on that person's record, which
// in the feed says nothing about the work and tells every reader when a
// colleague marked their inbox read. The rows are still recorded, and naming
// the kind still reads them.
func TestAPersonsOwnBookkeepingIsNotTheCompanysHistory(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	filedTask(t, r, "t-1")
	if _, err := asBob(r).MarkInbox(t.Context(), "op-prefs", "bob", statelog.Position{},
		inbox.Gesture[worklet.Reason]{PrimaryReasons: &[]worklet.Reason{worklet.ReasonMention}},
		worklet.PersonAuthority{}); err != nil {
		t.Fatalf("MarkInbox: %v", err)
	}
	r.drain()

	kinds := func(q worklet.ActivityQuery) []worklet.ChangeKind {
		var out []worklet.ChangeKind
		for _, record := range r.activity(q).Records {
			out = append(out, record.Kind)
		}
		return out
	}
	if got := kinds(worklet.ActivityQuery{Workspace: true}); slices.Contains(got, worklet.ChangePersonUpdated) {
		t.Fatalf("the company's history lists a person's own bookkeeping: %v", got)
	}
	if got := kinds(worklet.ActivityQuery{Workspace: true}); !slices.Contains(got, worklet.ChangeCreated) {
		t.Fatalf("the company's history lost the task filed beside it: %v", got)
	}
	asked := kinds(worklet.ActivityQuery{
		Workspace: true, Kinds: []worklet.ChangeKind{worklet.ChangePersonUpdated},
	})
	if len(asked) != 1 || asked[0] != worklet.ChangePersonUpdated {
		t.Fatalf("asked for by its kind, the bookkeeping answered %v", asked)
	}
}
