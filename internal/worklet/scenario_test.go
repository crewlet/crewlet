package worklet_test

import (
	"database/sql"
	"fmt"
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/advisor"
	"github.com/crewlet/crewlet/internal/decision"
	"github.com/crewlet/crewlet/internal/scenario"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/worklet"
)

// THE HEALTHY COMPANY'S HAND-OFFS, ASKS AND ANSWERS STILL WAKE WHOM THEY
// CONCERN, step by step, through the real writer: the control any routing
// change must keep passing while it quiets what asks nothing.
func TestTheHealthyCompanyWakesEverySeatItsWorkNeeds(t *testing.T) {
	t.Parallel()
	tl, err := scenario.Named("healthy_company")
	if err != nil {
		t.Fatal(err)
	}
	playTimeline(t, tl)
}

// EVERY ENGINE DETECTOR FIRES ON ITS OWN PATTERN'S TIMELINE AND ON NO OTHER,
// read from the records the timeline wrote; the healthy company raises none.
func TestEveryEngineDetectorFiresOnItsOwnTimelineAlone(t *testing.T) {
	t.Parallel()
	want := map[scenario.Pattern]advisor.Class{
		scenario.PatternHealthyCompany:           "",
		scenario.PatternAgentHandoffs:            "",
		scenario.PatternAgentMentionsWithoutAsk:  "",
		scenario.PatternPersonRequestsWithoutAsk: advisor.ClassPersonRequestsWithoutAsk,
		scenario.PatternAskOvertaken:             advisor.ClassAskOvertaken,
		scenario.PatternReviewIdle:               advisor.ClassReviewIdle,
		scenario.PatternApprovalRubberStamp:      advisor.ClassApprovalRubberStamp,
	}
	// ROUTING PREVENTS THIS ONE (ADR-0113), so no timeline makes it fire; its
	// read is held by TestAStatedReferenceThatWokeIsCountedAndAnUnknownOneIsNot.
	guarded := []advisor.Class{advisor.ClassAgentMentionsWithoutAsk}
	// A LOAD IS HUNDREDS OF NOTICES, which no timeline spells out step by step;
	// its read is held by TestAPersonsNoticeLoadIsEveryNoticeSentThemInTheWindow.
	counted := []advisor.Class{advisor.ClassNoticeLoad}
	for _, class := range advisor.WorkClasses() {
		if !slices.Contains(slices.Collect(maps.Values(want)), class) && !slices.Contains(guarded, class) &&
			!slices.Contains(counted, class) {
			t.Errorf("detector %s has no timeline it must fire on", class)
		}
	}
	all, err := scenario.All()
	if err != nil {
		t.Fatal(err)
	}
	for _, tl := range all {
		expected, ok := want[tl.Pattern]
		if !ok {
			t.Errorf("timeline %s's pattern %s names no detector, nor none", tl.Name, tl.Pattern)
			continue
		}
		t.Run(tl.Name, func(t *testing.T) {
			t.Parallel()
			d := playTimeline(t, tl)
			end := time.Now().UTC().Add(time.Microsecond)
			var agents []string
			for _, s := range tl.Seats {
				if s.Kind == scenario.SeatAgent {
					agents = append(agents, s.Handle)
				}
			}
			work, err := d.r.reader.WorkPatterns(t.Context(), worklet.PatternsQuery{
				Since: d.start, Until: end, Level: statelog.ReadStale,
				Reviewers: agents, QuietSince: d.quietSince()})
			if err != nil {
				t.Fatalf("WorkPatterns: %v", err)
			}
			var raised []advisor.Class
			for _, f := range advisor.Detect(advisor.Evidence{Until: end, Work: work}, d.kind) {
				if !slices.Contains(raised, f.Class) {
					raised = append(raised, f.Class)
				}
			}
			if expected == "" && len(raised) != 0 || expected != "" && !slices.Equal(raised, []advisor.Class{expected}) {
				t.Errorf("raised %v, want only %q; read %+v", raised, expected, work)
			}
		})
	}
}

// playTimeline runs every step of tl and holds each to the seats it declares woken.
func playTimeline(t *testing.T, tl scenario.Timeline) *timelineDriver {
	t.Helper()
	d := newTimelineDriver(t, tl)
	for i, st := range tl.Steps {
		got := d.run(i, st)
		if !slices.Equal(got, st.Expect.Woken) {
			t.Errorf("step %d (%s %s %s) woke %v, want %v", i, st.Actor, st.Verb, st.Item,
				got, st.Expect.Woken)
		}
	}
	return d
}

func (d *timelineDriver) agent(handle string) bool { return d.tl.Kind(handle) == scenario.SeatAgent }

// kind is what a handle names in the timeline, in the advisor's terms.
func (d *timelineDriver) kind(handle string) advisor.SeatKind {
	switch d.tl.Kind(handle) {
	case scenario.SeatAgent:
		return advisor.SeatAgent
	case scenario.SeatPerson:
		return advisor.SeatPerson
	}
	return advisor.SeatUnknown
}

// quietSince is just after the last item still in review entered it. The
// tracker stamps a status and a turn by the broker's clock, which a timeline's
// offsets do not move, so the quiet instant is read off the records themselves.
func (d *timelineDriver) quietSince() time.Time {
	d.t.Helper()
	var last time.Time
	for _, id := range d.ids {
		task := d.r.task(d.t, id).Task
		if task.Status == worklet.StatusInReview && task.StatusEnteredAt.After(last) {
			last = task.StatusEnteredAt
		}
	}
	return last.Add(time.Microsecond)
}

// timelineDriver plays a timeline's steps as the tools do: the wake built from
// the task before and after, the thread resolved by the reader.
type timelineDriver struct {
	t *testing.T
	// start is the instant before the first step; every step is stamped with
	// the clock the broker stamps its records by, its offset only ordering it.
	start    time.Time
	r        *roundTrip
	tl       scenario.Timeline
	ids      map[string]string
	comments map[int]string
	said     int
}

func newTimelineDriver(t *testing.T, tl scenario.Timeline) *timelineDriver {
	return &timelineDriver{t: t, r: newRoundTrip(t), tl: tl, start: time.Now().UTC(),
		ids: map[string]string{}, comments: map[int]string{}}
}

// run applies step i and answers which agent seats its notice starts a turn for.
func (d *timelineDriver) run(i int, st scenario.Step) []string {
	d.t.Helper()
	at := time.Now().UTC()
	d.r.at = at
	kind := worklet.AuthorAgent
	if d.tl.Kind(st.Actor) == scenario.SeatPerson {
		kind = worklet.AuthorHuman
	}
	writer := d.r.writer.As(st.Actor, kind, worklet.Provenance{})
	before := len(d.r.wakes(d.t))
	opID := fmt.Sprintf("op-step-%d", i)
	if st.Verb == scenario.VerbFile {
		task := newTask("t-" + st.Item)
		task.Title, task.Reporter, task.Assignee = st.Item, st.Actor, st.Assignee
		task.Watchers = []string{st.Actor}
		task.CreatedAt, task.UpdatedAt = at, at
		d.ids[st.Item] = task.ID
		notify := worklet.Wake{Kind: worklet.ChangeCreated, After: task}.Notify(d.r.leads)
		if _, err := writer.CreateTask(d.t.Context(), opID, task, notify); err != nil {
			d.t.Fatalf("step %d: file %s: %v", i, st.Item, err)
		}
	} else {
		d.update(i, st, writer, kind, opID, at)
	}
	d.r.drain()
	return d.woken(before, st.Actor)
}

func (d *timelineDriver) update(i int, st scenario.Step, writer *worklet.Writer,
	kind worklet.AuthorKind, opID string, at time.Time) {
	d.t.Helper()
	id := d.ids[st.Item]
	current := d.r.task(d.t, id).Task
	var patch worklet.TaskPatch
	var change worklet.ChangeKind
	wake := worklet.Wake{Before: current}
	switch st.Verb {
	case scenario.VerbAssign:
		patch.Assignee, change = &st.Assignee, worklet.ChangeAssignee
	case scenario.VerbStatus:
		status := worklet.Status(st.Status)
		patch.Status, change = &status, worklet.ChangeStatus
		if st.Assignee != "" {
			// ONE CHANGE THAT ALSO HANDS IT OVER, recorded under its status.
			patch.Assignee = &st.Assignee
		}
	case scenario.VerbWatch:
		patch.Watch, change = &worklet.WatchIntent{Handle: st.Watcher, Watch: true}, worklet.ChangeWatchers
	case scenario.VerbComment:
		change = worklet.ChangeComment
		patch, wake = d.comment(i, st, kind, id, current, at)
	case scenario.VerbTurn:
		if _, err := writer.RecordTurn(d.t.Context(), opID, worklet.TurnRecord{
			Task: id, Seat: st.Actor, TurnID: fmt.Sprintf("run-%d", i), Trigger: "work_pickup",
		}); err != nil {
			d.t.Fatalf("step %d: turn on %s: %v", i, st.Item, err)
		}
		return
	default:
		d.t.Fatalf("step %d: verb %q has no driver here", i, st.Verb)
	}
	wake.Kind, wake.After = change, worklet.Patched(current, patch)
	if _, err := writer.UpdateTask(d.t.Context(), opID, id, current.Project, worklet.NoIfMatch,
		patch, change, wake.Notify(d.r.leads)); err != nil {
		d.t.Fatalf("step %d: %s %s: %v", i, st.Verb, st.Item, err)
	}
}

// comment forms a comment's patch and wake as the comment tool does: the thread
// read first, and the author's automatic watch as a gesture.
func (d *timelineDriver) comment(i int, st scenario.Step, kind worklet.AuthorKind, id string,
	current worklet.Task, at time.Time) (worklet.TaskPatch, worklet.Wake) {
	d.t.Helper()
	c := &worklet.Comment{ID: fmt.Sprintf("c-%d", i), Task: id, Author: st.Actor, AuthorKind: kind,
		Body: scenario.Phrase(st.Say, d.said), Mentions: st.Mentions, CreatedAt: at}
	d.said++
	query := worklet.ThreadQuery{Task: id, Ask: st.Ask, Author: st.Actor, Comment: c.ID}
	if st.Answers != nil {
		query.Answers = d.comments[*st.Answers]
	}
	thread, err := d.r.reader.Thread(d.t.Context(), query, statelog.Freshness{Level: statelog.ReadStale})
	if err != nil {
		d.t.Fatalf("step %d: resolve the thread: %v", i, err)
	}
	c.Ask = thread.Asked
	if thread.Answers != "" {
		c.Answers = &thread.Answers
	}
	if st.Recommend != "" {
		c.Decision = scenarioDecision(st.Recommend)
	}
	c.Choice = st.Choose
	d.comments[i] = c.ID
	patch := worklet.TaskPatch{Comment: c,
		Watch: &worklet.WatchIntent{Handle: st.Actor, Watch: true, Auto: true}}
	wake := worklet.Wake{Before: current, Comment: c, Mentions: c.Mentions,
		Thread: thread.ThreadParties, AnswersDecision: thread.AnswersDecision}
	if st.Asks != nil {
		// AS THE COMMENT TOOL STATES IT: an ask asks by itself, and only the
		// agents it names are told apart from the people.
		asks := *st.Asks || c.Ask != ""
		wake.Asks = &asks
		for _, m := range c.Mentions {
			if d.agent(m) {
				wake.MentionedAgents = append(wake.MentionedAgents, m)
			}
		}
	}
	return patch, wake
}

// woken is every agent seat the notices after the first before start a turn
// for, sorted, read off the log's records.
func (d *timelineDriver) woken(before int, actor string) []string {
	d.t.Helper()
	out := []string{}
	for _, n := range d.r.wakes(d.t)[before:] {
		for _, c := range worklet.Route(worklet.Candidates(n), everyone, actor) {
			if d.agent(c.Handle) && !slices.Contains(out, c.Handle) {
				out = append(out, c.Handle)
			}
		}
	}
	slices.Sort(out)
	return out
}

// A COMMENT WHOSE AUTHOR STATED ASKS IS STAMPED AT THE VERSION THAT KEEPS IT, so
// a node on a build before the rule retains it rather than applying it with no
// statement, and two nodes never keep different notice rows for it.
func TestAStatedCommentIsStampedSoAnOlderNodeRetainsIt(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"agent_handoffs", "agent_mentions_without_ask"} {
		tl, err := scenario.Named(name)
		if err != nil {
			t.Fatal(err)
		}
		d := playTimeline(t, tl)
		end, err := d.r.log.End(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		stated, unstated := 0, 0
		for seq := uint64(1); seq <= end; seq++ {
			_, payload, _, ok, err := d.r.log.At(t.Context(), seq)
			if err != nil || !ok {
				t.Fatalf("read record %d: %v", seq, err)
			}
			rec, err := worklet.Decode(d.r.open(seq, payload))
			if err != nil || rec.Notify == nil || rec.Notify.Kind != worklet.ChangeComment {
				continue
			}
			// A RECORD THAT TELLS SOMEBODY IT DOES NOT WAKE is stamped at the
			// version that keeps that decision, whatever its statement.
			woke := worklet.NoticeAsksVersion
			if rec.NoticeWokeKept {
				woke = worklet.NoticeWokeVersion
			}
			switch rec.Notify.Snapshot.CommentAsks {
			case nil:
				unstated++
				if rec.NoticeAsksKept || (!rec.NoticeWokeKept && rec.V >= worklet.NoticeAsksVersion) {
					t.Errorf("%s: a comment that states nothing is version %d, kept %v; want below %d",
						name, rec.V, rec.NoticeAsksKept, worklet.NoticeAsksVersion)
				}
			default:
				stated++
				if !rec.NoticeAsksKept || rec.V != woke {
					t.Errorf("%s: a stated comment is version %d, kept %v; want %d, kept",
						name, rec.V, rec.NoticeAsksKept, woke)
				}
			}
		}
		if stated == 0 || name == "agent_mentions_without_ask" && unstated == 0 {
			t.Errorf("%s: read %d stated and %d unstated comments, want both kinds the timeline writes",
				name, stated, unstated)
		}
	}
}

// A STATED REFERENCE THAT WOKE ITS SEAT IS COUNTED, AND AN UNKNOWN ONE IS NOT:
// routing wakes no stated reference today, so the case writes the regression
// by hand; a notice an older build kept no statement on is never counted.
func TestAStatedReferenceThatWokeIsCountedAndAnUnknownOneIsNot(t *testing.T) {
	t.Parallel()
	tl, err := scenario.Named("agent_mentions_without_ask")
	if err != nil {
		t.Fatal(err)
	}
	d := playTimeline(t, tl)
	w, err := d.r.db.Writer(t.Context())
	if err != nil {
		t.Fatalf("take the writer: %v", err)
	}
	if err := w.Tx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `UPDATE tracker_notifications SET addressed = 1
			WHERE reason = 'mention' AND asks = 0`)
		return err
	}); err != nil {
		t.Fatalf("wake the stated references by hand: %v", err)
	}
	_ = w.Close()
	// EVERY INSTANT EITHER CLOCK STAMPS: the read is about the rows, not the window.
	work, err := d.r.reader.WorkPatterns(t.Context(), worklet.PatternsQuery{
		Since: time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC), Until: time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC),
		Level: statelog.ReadStale})
	if err != nil {
		t.Fatalf("WorkPatterns: %v", err)
	}
	woken, told, asked := 0, 0, 0
	for _, n := range work.Named {
		woken, told, asked = woken+n.Woken, told+n.Told, asked+n.Asked
	}
	if woken != 2 || asked != 0 || told != 8 {
		t.Fatalf("read %d woken, %d asked of %d told; want the 2 stated references woken, "+
			"and the 6 an older build wrote counted as neither", woken, asked, told)
	}
}

// AN IDLE REVIEW IS ONE ITS HOLDER TOOK NO TURN ON SINCE: of two reviews held
// by one agent, the one it turned to is not idle and the other is.
func TestAnIdleReviewIsOneItsHolderLeftAlone(t *testing.T) {
	t.Parallel()
	tl, err := scenario.Named("review_idle")
	if err != nil {
		t.Fatal(err)
	}
	d := playTimeline(t, tl)
	work, err := d.r.reader.WorkPatterns(t.Context(), worklet.PatternsQuery{
		Since: d.start, Until: time.Now().UTC().Add(time.Microsecond), Level: statelog.ReadStale,
		Reviewers: []string{"app", "qa"}, QuietSince: d.quietSince()})
	if err != nil {
		t.Fatalf("WorkPatterns: %v", err)
	}
	if len(work.IdleReviews) != 1 || work.IdleReviews[0].Task != d.ids["W-1"] ||
		work.IdleReviews[0].Holder != "qa" {
		t.Fatalf("idle reviews %+v, want only W-1, held by qa", work.IdleReviews)
	}
	// A QUIET INSTANT BEFORE ANY REVIEW BEGAN finds none idle: a review that
	// has not waited the whole quiet period is not idle, however untouched.
	early, err := d.r.reader.WorkPatterns(t.Context(), worklet.PatternsQuery{
		Since: d.start, Until: time.Now().UTC().Add(time.Microsecond), Level: statelog.ReadStale,
		Reviewers: []string{"app", "qa"}, QuietSince: d.start})
	if err != nil || len(early.IdleReviews) != 0 {
		t.Fatalf("with the quiet instant before every review, idle reviews %+v (%v), want none",
			early.IdleReviews, err)
	}
}

// scenarioDecision is a decision step's question: the fixed choices, one
// recommended, in the timeline's own words.
func scenarioDecision(recommend string) *decision.Decision {
	d := &decision.Decision{Question: scenario.Phrase(scenario.SayQuestion, 0),
		Role: decision.RoleApprover, Recommended: recommend, Rationale: "It keeps the work moving."}
	for _, id := range scenario.Choices {
		d.Options = append(d.Options, decision.Option{ID: id, Label: scenario.ChoiceLabels[id]})
	}
	return d
}

// A PERSON'S NOTICE LOAD IS EVERY NOTICE SENT THEM IN THE WINDOW, and how many
// of those were put to them, read beside the rows themselves; a person sent
// nothing reads zero rather than going missing.
func TestAPersonsNoticeLoadIsEveryNoticeSentThemInTheWindow(t *testing.T) {
	t.Parallel()
	tl, err := scenario.Named("ask_overtaken")
	if err != nil {
		t.Fatal(err)
	}
	d := playTimeline(t, tl)
	want := map[string]worklet.NoticeLoad{}
	if err := d.r.db.Read(t.Context(), func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(t.Context(), `SELECT recipient, addressed FROM tracker_notifications`)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var handle string
			var addressed bool
			if err := rows.Scan(&handle, &addressed); err != nil {
				return err
			}
			load := want[handle]
			load.Handle = handle
			load.Notices++
			if addressed {
				load.Addressed++
			}
			want[handle] = load
		}
		return rows.Err()
	}); err != nil {
		t.Fatalf("read the notices: %v", err)
	}
	if owner := want["owner"]; owner.Addressed == 0 || owner.Addressed == owner.Notices {
		t.Fatalf("the timeline sent the owner %+v, want notices asking them something among others", want["owner"])
	}
	people := []string{"owner", "nobody"}
	for _, c := range []struct {
		name         string
		since, until time.Time
		want         []worklet.NoticeLoad
	}{
		{"the timeline's window", d.start, time.Now().UTC().Add(time.Microsecond),
			[]worklet.NoticeLoad{want["owner"], {Handle: "nobody"}}},
		{"a window that ended before it", d.start.Add(-time.Hour), d.start,
			[]worklet.NoticeLoad{{Handle: "owner"}, {Handle: "nobody"}}},
	} {
		work, err := d.r.reader.WorkPatterns(t.Context(), worklet.PatternsQuery{
			Since: c.since, Until: c.until, Level: statelog.ReadStale, People: people})
		if err != nil {
			t.Fatalf("WorkPatterns: %v", err)
		}
		if !slices.Equal(work.Notices, c.want) {
			t.Errorf("%s: read %+v, want %+v", c.name, work.Notices, c.want)
		}
	}
}
