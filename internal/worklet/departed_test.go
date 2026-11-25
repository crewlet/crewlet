package worklet_test

import (
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/worklet"
)

// departureLeads is a chart whose project and unit leads a case states.
type departureLeads struct{ projects, units map[string]string }

func (l departureLeads) ProjectLead(project string) string { return l.projects[project] }
func (l departureLeads) UnitLead(unit string) string       { return l.units[unit] }

// historyNote is the newest history row of an item: its kind, who wrote it and
// its excerpt, the note a re-homing leaves.
type historyNote struct{ kind, actorKind, excerpt string }

func (r *roundTrip) newestHistory(id string) historyNote {
	r.t.Helper()
	var note historyNote
	if err := r.db.Read(r.t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(r.t.Context(), `SELECT kind, actor_kind, excerpt
			FROM tracker_history WHERE subject_id = ? ORDER BY log_seq DESC LIMIT 1`, id).
			Scan(&note.kind, &note.actorKind, &note.excerpt)
	}); err != nil {
		r.t.Fatalf("read the newest history of %s: %v", id, err)
	}
	return note
}

// A REMOVED SEAT'S WORK IS RETURNED OR HANDED TO ITS LEAD (ADR-0162, situation
// 7): each open item it holds is written once, by the engine, with a history
// note naming the seat, by [worklet.Writer.RehomeDeparted]'s rule, one item per
// rule and per live side effect. The lead is the project's, else the unit's,
// never the departed seat itself. Finished work and other seats' work are not
// touched, and a second pass finds nothing left.
//
// Mutations: read every item in progress as one with a side effect (or none),
// drop any one side effect, hand an item in review back to todo, take a lead
// the chart names for the departed seat, or skip the history note, and this
// fails on the item that rule decides.
func TestARemovedSeatsWorkIsReturnedOrHandedToItsLead(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	// THE REAL CLOCK, so the turn a seat ended on an item (stamped with the
	// broker's own instant) is after a start an hour ago, as in a company.
	r.at = time.Now().UTC().Truncate(time.Second)
	r.ledBy(departureLeads{
		projects: map[string]string{"ENG": "lin"},
		units:    map[string]string{"core": "kai"},
	})
	ahead, passed := r.at.Add(48*time.Hour), r.at.Add(-time.Hour)
	file := func(id, holder string, status worklet.Status, edit func(*worklet.Task)) {
		t.Helper()
		task := newTask(id)
		task.Key = "ENG-" + id
		task.Assignee, task.Status, task.StatusGroup = holder, status, status.Group()
		if edit != nil {
			edit(&task)
		}
		pm := r.writer.As("pm", worklet.AuthorHuman, worklet.Provenance{})
		if _, err := pm.CreateTask(t.Context(), "op-"+id, task, nil); err != nil {
			t.Fatalf("CreateTask %s: %v", id, err)
		}
		r.drain()
	}
	starting := func(at time.Time) func(*worklet.Task) {
		return func(task *worklet.Task) { task.StartAt = &at }
	}

	file("waiting", "ada", worklet.StatusInProgress, starting(ahead))
	file("due", "ada", worklet.StatusInProgress, starting(passed))
	file("worked", "ada", worklet.StatusInProgress, starting(passed))
	file("asking", "ada", worklet.StatusInProgress, nil)
	file("pull", "ada", worklet.StatusInProgress, nil)
	file("merged", "ada", worklet.StatusInProgress, nil)
	file("running", "ada", worklet.StatusInProgress, nil)
	file("plain", "ada", worklet.StatusInProgress, nil)
	file("blocked", "ada", worklet.StatusInProgress, nil)
	file("blocker", "bob", worklet.StatusTodo, nil)
	file("review", "ada", worklet.StatusInReview, nil)
	file("unit-review", "ada", worklet.StatusInReview, func(task *worklet.Task) {
		task.RoutingUnit = "core"
	})
	file("queued", "ada", worklet.StatusTodo, starting(ahead))
	file("finished", "ada", worklet.StatusDone, nil)
	file("bobs", "bob", worklet.StatusInProgress, nil)

	ada := r.writer.As("ada", worklet.AuthorAgent, aTurnNobodyWoke())
	if _, err := ada.RecordTurn(t.Context(), "turn/ada-1/dispatch", worklet.TurnRecord{
		Task: "worked", Seat: "ada", TurnID: "ada-1", Trigger: "work_item", Outcome: "done",
		Spend: worklet.TurnSpend{Turns: 1},
	}); err != nil {
		t.Fatalf("RecordTurn: %v", err)
	}
	question := worklet.Comment{ID: "q-1", Task: "asking", Author: "ada",
		AuthorKind: worklet.AuthorAgent, Body: "which environment?", Ask: "pm", CreatedAt: r.at}
	if _, err := ada.UpdateTask(t.Context(), "op-ask", "asking", "ENG", worklet.NoIfMatch,
		worklet.TaskPatch{Comment: &question}, worklet.ChangeComment, nil); err != nil {
		t.Fatalf("ask on asking: %v", err)
	}
	r.drain()
	waitOn(t, r, "pull", deadPull)
	waitOn(t, r, "merged", deadPull)
	waitOn(t, r, "blocked", "blocker")
	if _, err := r.writer.SettleChange(t.Context(), "op-merged", r.task(t, "merged").Task,
		worklet.ChangeSettlement{Ref: deadPull, State: worklet.ChangeMerged, At: r.at,
			Source: worklet.SettledByWebhook}); err != nil {
		t.Fatalf("settle the pull request merged's waits on: %v", err)
	}
	r.drain()

	engine := r.writer.As("node-a", worklet.AuthorSystem, worklet.Provenance{})
	// THE DEPARTED HOLDERS ARE THE FLOW'S: none before a chart is applied,
	// and then every holder of open work the chart does not place.
	departedHolders := func() []string {
		t.Helper()
		holders, err := engine.DepartedHolders(t.Context())
		if err != nil {
			t.Fatalf("DepartedHolders: %v", err)
		}
		return holders
	}
	if got := departedHolders(); len(got) != 0 {
		t.Errorf("before any chart was applied the departed holders are %v, want none", got)
	}
	if _, err := engine.ApplyRoster(t.Context(), activatedAt(100), map[string]string{
		"bob": "Bob", "lin": "Lin", "kai": "Kai", "pm": "PM"}); err != nil {
		t.Fatalf("ApplyRoster: %v", err)
	}
	r.drain()
	if got := departedHolders(); !slices.Equal(got, []string{"ada"}) {
		t.Errorf("the departed holders are %v, want ada alone", got)
	}

	departure := worklet.Departure{Handle: "ada", Runs: []string{"running"}, At: r.at}
	if _, err := r.writer.RehomeDeparted(t.Context(), departure); !errors.Is(err, worklet.ErrInvalid) {
		t.Fatalf("a person's writer re-homed a departed seat's work (%v), want it refused", err)
	}
	moved, err := engine.RehomeDeparted(t.Context(), departure)
	if err != nil {
		t.Fatalf("RehomeDeparted: %v", err)
	}
	r.drain()

	type want struct {
		to      string
		status  worklet.Status
		effects []worklet.SideEffect
		note    []string
	}
	todo, progress, review := worklet.StatusTodo, worklet.StatusInProgress, worklet.StatusInReview
	cases := map[string]want{
		"waiting": {"lin", progress, []worklet.SideEffect{worklet.SideEffectDate},
			[]string{"lin holds it now, still in progress", "it waits until"}},
		"due": {"lin", progress, []worklet.SideEffect{worklet.SideEffectDate},
			[]string{"passed with nothing done since"}},
		"asking": {"lin", progress, []worklet.SideEffect{worklet.SideEffectQuestion},
			[]string{"ada's question on it is still open"}},
		"pull": {"lin", progress, []worklet.SideEffect{worklet.SideEffectChange},
			[]string{"it waits on an open pull request"}},
		"running": {"lin", progress, []worklet.SideEffect{worklet.SideEffectRun},
			[]string{"ada's run on it was ended"}},
		"worked":      {"", todo, nil, []string{"back in todo, unassigned"}},
		"merged":      {"", todo, nil, []string{"back in todo, unassigned"}},
		"plain":       {"", todo, nil, []string{"back in todo, unassigned"}},
		"blocked":     {"", todo, nil, []string{"back in todo, unassigned"}},
		"review":      {"lin", review, nil, []string{"lin holds it now, to finish its review"}},
		"unit-review": {"lin", review, nil, []string{"lin holds it now"}},
		"queued":      {"", todo, nil, []string{"It is unassigned."}},
	}
	byID := map[string]worklet.Rehomed{}
	for _, m := range moved {
		byID[m.ID] = m
	}
	if len(moved) != len(cases) {
		t.Errorf("re-homed %d items, want the %d open items ada held: %+v", len(moved), len(cases), moved)
	}
	for id, w := range cases {
		got := r.task(t, id).Task
		if got.Assignee != w.to || got.Status != w.status {
			t.Errorf("%s is held by %q in %s, want %q in %s", id, got.Assignee, got.Status, w.to, w.status)
		}
		if m := byID[id]; m.To != w.to || m.Status != w.status || !slices.Equal(m.Effects, w.effects) {
			t.Errorf("%s was answered as %+v, want to %q in %s for %v", id, m, w.to, w.status, w.effects)
		}
		note := r.newestHistory(id)
		if note.kind != string(worklet.ChangeAssignee) || note.actorKind != string(worklet.AuthorSystem) ||
			!strings.HasPrefix(note.excerpt, "ada left the chart") {
			t.Errorf("%s's newest history is %+v, want the engine's reassignment noting ada left", id, note)
		}
		for _, words := range w.note {
			if !strings.Contains(note.excerpt, words) {
				t.Errorf("%s's history note %q does not say %q", id, note.excerpt, words)
			}
		}
	}
	for id, holder := range map[string]string{"finished": "ada", "bobs": "bob", "blocker": "bob"} {
		if got := r.task(t, id).Task.Assignee; got != holder {
			t.Errorf("%s, which is not ada's open work, is held by %q, want %q", id, got, holder)
		}
	}
	// THE LEAD IS TOLD, as anybody handed work is.
	var told bool
	for _, n := range r.wakes(t) {
		if n.Kind == worklet.ChangeAssignee && n.Snapshot.Assignee == "lin" &&
			strings.HasPrefix(n.Excerpt, "ada left the chart") {
			told = true
		}
	}
	if !told {
		t.Error("no re-homing's wake names lin as the item's new assignee")
	}

	// A SECOND PASS, as a retried retirement makes, finds nothing left.
	history := r.historyOf("waiting")
	again, err := engine.RehomeDeparted(t.Context(), worklet.Departure{Handle: "ada", At: r.at.Add(time.Hour)})
	if err != nil || len(again) != 0 {
		t.Fatalf("a second pass re-homed %+v (%v), want nothing", again, err)
	}
	r.drain()
	if got := r.historyOf("waiting"); got != history {
		t.Errorf("a second pass wrote %d history rows on an item already re-homed", got-history)
	}

	// THE UNIT'S LEAD WHERE THE PROJECT HAS NONE; NEITHER, AND NOBODY; AND
	// NEVER THE DEPARTED SEAT, which a chart read before its removal names.
	for _, c := range []struct {
		name   string
		leads  departureLeads
		holder string
		want   string
	}{
		{"the unit's lead", departureLeads{units: map[string]string{"core": "kai"}}, "eve", "kai"},
		{"no lead", departureLeads{}, "fay", ""},
		{"the seat itself", departureLeads{projects: map[string]string{"ENG": "gus"},
			units: map[string]string{"core": "gus"}}, "gus", ""},
	} {
		r.ledBy(c.leads)
		file(c.holder+"-review", c.holder, worklet.StatusInReview, func(task *worklet.Task) {
			task.RoutingUnit = "core"
		})
		engine = r.writer.As("node-a", worklet.AuthorSystem, worklet.Provenance{})
		moved, err := engine.RehomeDeparted(t.Context(), worklet.Departure{Handle: c.holder, At: r.at})
		if err != nil {
			t.Fatalf("%s: RehomeDeparted: %v", c.name, err)
		}
		r.drain()
		got := r.task(t, c.holder+"-review").Task
		wantStatus := worklet.StatusInReview
		if c.want == "" {
			wantStatus = worklet.StatusTodo
		}
		if got.Assignee != c.want || got.Status != wantStatus {
			t.Errorf("%s: the review is held by %q in %s, want %q in %s", c.name, got.Assignee,
				got.Status, c.want, wantStatus)
		}
		if leaderless := len(moved) == 1 && moved[0].Leaderless; leaderless != (c.want == "") {
			t.Errorf("%s: answered %+v, want leaderless %t", c.name, moved, c.want == "")
		}
	}
}

// A DEPARTURE TELLS EACH LEAD ONCE OF THE WORK IT LEFT WITHOUT AN OWNER
// (ADR-0162's amendment): a seat leaving with many items wakes the lead of
// each project once, on one record listing that lead's items, never once an
// item, whether the items are watched (OPS) or not (ENG), and the applier's
// rows and the wake path agree.
//
// Mutation: build each item's record with its leads, or name the departure's
// lead on every item, and a lead is woken once an item; offer that lead as a
// fallback, and a watched item's watcher keeps it quiet.
func TestADepartureTellsEachLeadOnceOfItsUnownedWork(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	r.applyWhileWriting()
	r.ledBy(departureLeads{projects: map[string]string{"ENG": "lin", "OPS": "olu"}})
	if _, err := r.writer.WriteDocument(t.Context(), "op-ops", worklet.ProjectSubject("OPS"), "",
		worklet.Project{V: 1, Key: "OPS", Name: "Operations", CreatedAt: wednesday, UpdatedAt: wednesday},
		worklet.ChangeProjectCreated, nil); err != nil {
		t.Fatalf("seed OPS: %v", err)
	}
	pm := r.writer.As("pm", worklet.AuthorHuman, worklet.Provenance{})
	want := map[string][]string{}
	file := func(project, id string, status worklet.Status) {
		t.Helper()
		task := newTask(id)
		task.Project, task.Key = project, project+"-"+id
		task.Assignee, task.Status, task.StatusGroup = "ada", status, status.Group()
		if project == "OPS" {
			task.Watchers = []string{"pm"}
		}
		if _, err := pm.CreateTask(t.Context(), "op-"+id, task, nil); err != nil {
			t.Fatalf("CreateTask %s: %v", id, err)
		}
		r.drain()
		lead := map[string]string{"ENG": "lin", "OPS": "olu"}[project]
		want[lead] = append(want[lead], r.task(t, id).Task.Key)
	}
	for _, id := range []string{"a", "b", "c", "d", "e", "f"} {
		file("ENG", id, worklet.StatusTodo)
	}
	file("ENG", "g", worklet.StatusInProgress)
	file("ENG", "h", worklet.StatusInProgress)
	for _, id := range []string{"x", "y", "z"} {
		file("OPS", id, worklet.StatusTodo)
	}

	from := r.end()
	engine := r.writer.As("node-a", worklet.AuthorSystem, worklet.Provenance{})
	if _, err := engine.RehomeDeparted(t.Context(), worklet.Departure{Handle: "ada", At: r.at}); err != nil {
		t.Fatalf("RehomeDeparted: %v", err)
	}
	r.drain()

	woken := map[string]int{}
	notes := map[string]string{}
	parser := worklet.NewParser(worklet.ParserOptions{})
	staff := func(h string) bool { return h != "ada" }
	for seq := from + 1; seq <= r.end(); seq++ {
		_, payload, _, ok, err := r.log.At(t.Context(), seq)
		if err != nil || !ok {
			t.Fatalf("read record %d: %v", seq, err)
		}
		rec, err := worklet.Decode(r.open(seq, payload))
		if err != nil || rec.Notify == nil {
			continue
		}
		routed := worklet.Route(worklet.Candidates(rec.Notify), staff, rec.Actor)
		parsed, err := parser.Parse(t.Context(), delivery(t, rec), registry(t, "lin", "olu", "pm"))
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		var byParser, byRows []string
		for _, p := range parsed {
			if !p.Held {
				byParser = append(byParser, p.To.Handle)
			}
		}
		byRows = r.startedTurns(rec)
		var byRoute []string
		for _, c := range routed {
			byRoute = append(byRoute, c.Handle)
			woken[c.Handle]++
			notes[c.Handle] = rec.Notify.Excerpt
		}
		slices.Sort(byParser)
		slices.Sort(byRoute)
		if !slices.Equal(byRoute, byParser) || !slices.Equal(byRoute, byRows) {
			t.Errorf("%s: Route wakes %v, the parser %v and the notice rows %v; they must agree",
				rec.Subject.ID, byRoute, byParser, byRows)
		}
	}
	if len(woken) != 2 || woken["lin"] != 1 || woken["olu"] != 1 {
		t.Fatalf("the departure woke %v, want lin and olu once each", woken)
	}
	for lead, keys := range want {
		note := notes[lead]
		if !strings.Contains(note, fmt.Sprintf("%d items ada held have no owner now", len(keys))) {
			t.Errorf("%s's one notice says %q, want it to count the %d items", lead, note, len(keys))
		}
		for _, key := range keys {
			if !strings.Contains(note, " "+key+",") && !strings.Contains(note, " "+key+" ") &&
				!strings.Contains(note, " "+key+".") {
				t.Errorf("%s's one notice %q does not name %s", lead, note, key)
			}
		}
	}
}
