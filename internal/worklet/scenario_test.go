package worklet_test

import (
	"fmt"
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/advisor"
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
		scenario.PatternAgentMentionsWithoutAsk:  advisor.ClassAgentMentionsWithoutAsk,
		scenario.PatternPersonRequestsWithoutAsk: advisor.ClassPersonRequestsWithoutAsk,
		scenario.PatternAskOvertaken:             advisor.ClassAskOvertaken,
	}
	for _, class := range advisor.WorkClasses() {
		if !slices.Contains(slices.Collect(maps.Values(want)), class) {
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
			end := wednesday.Add(time.Duration(tl.Steps[len(tl.Steps)-1].At+1) * time.Second)
			work, err := d.r.reader.WorkPatterns(t.Context(), worklet.PatternsQuery{
				Since: wednesday, Until: end, Level: statelog.ReadStale})
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

// timelineDriver plays a timeline's steps as the tools do: the wake built from
// the task before and after, the thread resolved by the reader.
type timelineDriver struct {
	t        *testing.T
	r        *roundTrip
	tl       scenario.Timeline
	ids      map[string]string
	comments map[int]string
	said     int
}

func newTimelineDriver(t *testing.T, tl scenario.Timeline) *timelineDriver {
	return &timelineDriver{t: t, r: newRoundTrip(t), tl: tl,
		ids: map[string]string{}, comments: map[int]string{}}
}

// run applies step i and answers which agent seats its notice starts a turn for.
func (d *timelineDriver) run(i int, st scenario.Step) []string {
	d.t.Helper()
	at := wednesday.Add(time.Duration(st.At) * time.Second)
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
	case scenario.VerbWatch:
		patch.Watch, change = &worklet.WatchIntent{Handle: st.Watcher, Watch: true}, worklet.ChangeWatchers
	case scenario.VerbComment:
		change = worklet.ChangeComment
		patch, wake = d.comment(i, st, kind, id, current, at)
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
