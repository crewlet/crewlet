package tracker_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// A goal fixture the cases vary.
func aGoal(id string, mutate func(*tracker.Goal)) tracker.Goal {
	goal := tracker.Goal{
		ID:     id,
		Name:   "Ship the thing",
		Owners: []string{"ana"},
		Health: string(tracker.HealthOnTrack),
		Targets: []tracker.GoalTarget{{
			ID: "t-1", Name: "The work", Type: tracker.TargetTasks,
			Projects: []string{"ENG"},
		}},
	}
	if mutate != nil {
		mutate(&goal)
	}
	return goal
}

func (r *roundTrip) goals(q tracker.GoalQuery) tracker.GoalListing {
	r.t.Helper()
	if q.Level == "" {
		q.Level = statelog.ReadStale
	}
	listing, err := r.reader.Goals(r.t.Context(), q)
	if err != nil {
		r.t.Fatalf("Goals(%+v): %v", q, err)
	}
	return listing
}

// A GOAL THAT COULD NOT BE COUNTED IS REFUSED AT THE SAVE.
//
// Every one of these produces a goal whose progress is a lie rather than a
// number, and the lie is discovered by whoever quotes it in a review.
func TestAGoalThatCouldNotBeCountedIsRefused(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)

	for _, tc := range []struct {
		name   string
		mutate func(*tracker.Goal)
		want   string
	}{
		{"no id", func(g *tracker.Goal) { g.ID = "" }, "names no goal id"},
		{"no name", func(g *tracker.Goal) { g.Name = "  " }, "has no name"},
		{"no owner", func(g *tracker.Goal) { g.Owners = nil }, "names no owner"},
		{"owners nobody could be", func(g *tracker.Goal) {
			g.Owners = make([]string, 0, tracker.MaxGoalOwners+1)
			for i := range tracker.MaxGoalOwners + 1 {
				g.Owners = append(g.Owners, string(rune('a'+i)))
			}
		}, "owners and the maximum"},
		{"a health nobody set", func(g *tracker.Goal) { g.Health = "amber" },
			"not a health value"},
		{"a window that ends before it begins", func(g *tracker.Goal) {
			start := wednesday
			due := wednesday.Add(-time.Hour)
			g.StartAt, g.DueAt = &start, &due
		}, "ends before it begins"},
		{"a target with no id", func(g *tracker.Goal) { g.Targets[0].ID = "" },
			"names no target id"},
		{"two targets under one id", func(g *tracker.Goal) {
			g.Targets = append(g.Targets, g.Targets[0])
		}, "twice"},
		{"a target type that is not one", func(g *tracker.Goal) {
			g.Targets[0].Type = "burndown"
		}, "not a target type"},
		{"a task target naming nothing", func(g *tracker.Goal) {
			g.Targets[0].Projects = nil
		}, "names none"},
		{"a number that cannot move", func(g *tracker.Goal) {
			g.Targets[0] = tracker.GoalTarget{
				ID: "t-1", Name: "Revenue", Type: tracker.TargetNumber,
				Start: 10, Goal: 10,
			}
		}, "no movement could ever register"},
		{"more tasks than a target counts", func(g *tracker.Goal) {
			g.Targets[0].Projects = nil
			g.Targets[0].Tasks = make([]string, 0, tracker.MaxTasksPerTarget+1)
			for i := range tracker.MaxTasksPerTarget + 1 {
				g.Targets[0].Tasks = append(g.Targets[0].Tasks, "t-"+itoa(i))
			}
		}, "tasks and the maximum"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := r.writer.WriteGoal(t.Context(), "op-"+tc.name,
				aGoal("g-bad", tc.mutate)); err == nil {
				t.Fatal("a goal that cannot be counted was saved")
			} else if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("the refusal %q does not say %q", err, tc.want)
			}
		})
	}

	// THE CONTROL. Without it every case above would pass against a verb
	// that refused everything.
	if _, err := r.writer.WriteGoal(t.Context(), "op-good", aGoal("g-good", nil)); err != nil {
		t.Fatalf("the control goal was refused: %v", err)
	}
}

// A GOAL IS AT WHAT ITS TARGETS SAY, computed on every read.
//
// A stored percentage is a second answer to a question that already has one,
// and the two drift the moment a task closes without anybody editing the goal
// — which is the ordinary case, not the exotic one.
func TestAGoalsProgressIsItsTargets(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)

	// Four tasks in ENG, two of them FINISHED and only one of them
	// DELIVERED — one `done` and one `cancelled`, because those are two
	// different facts and a fixture where every finished task is `done`
	// cannot tell them apart. Cancelling work is how this design spells
	// "abandoned" ([Delivered]), so a goal that counted it would be driven
	// to a hundred per cent by a team giving up.
	for i, status := range []tracker.Status{
		tracker.StatusDone, tracker.StatusCancelled, "", "",
	} {
		task := newTask("gt-" + itoa(i))
		task.Key = "ENG-" + itoa(i)
		if status != "" {
			task.Status = status
			task.StatusGroup = status.Group()
		}
		if _, err := r.writer.CreateTask(t.Context(), "op-task-"+itoa(i), task, nil); err != nil {
			t.Fatalf("seed a task: %v", err)
		}
		r.drain()
	}

	if _, err := r.writer.WriteGoal(t.Context(), "op-goal", aGoal("g-1", nil)); err != nil {
		t.Fatalf("save the goal: %v", err)
	}
	r.drain()

	listing := r.goals(tracker.GoalQuery{ID: "g-1"})
	if len(listing.Goals) != 1 {
		t.Fatalf("the listing has %d goals, want 1", len(listing.Goals))
	}
	goal := listing.Goals[0]
	if len(goal.Targets) != 1 {
		t.Fatalf("the goal has %d targets, want 1", len(goal.Targets))
	}
	target := goal.Targets[0]
	if target.Finished != 1 || target.Total != 4 {
		t.Fatalf("the target is at %d of %d, want 1 of 4: two tasks are "+
			"finished and one of those is cancelled, which is abandoned work "+
			"rather than delivered work", target.Finished, target.Total)
	}
	if goal.Progress == nil || *goal.Progress != 0.25 {
		t.Fatalf("the goal is at %v, want 0.25", goal.Progress)
	}

	// AND IT MOVES WHEN THE WORK DOES, with nobody editing the goal —
	// which is the whole reason the number is not stored.
	done := tracker.StatusDone
	if _, err := r.writer.UpdateTask(t.Context(), "op-finish", "gt-2", "ENG", 0,
		tracker.TaskPatch{Status: &done}, tracker.ChangeStatus, nil); err != nil {
		t.Fatalf("finish a task: %v", err)
	}
	r.drain()
	after := r.goals(tracker.GoalQuery{ID: "g-1"}).Goals[0]
	if after.Progress == nil || *after.Progress != 0.5 {
		t.Fatalf("the goal is at %v after a second task was DELIVERED, want 0.5",
			after.Progress)
	}

	// AND CANCELLING THE REST DOES NOT FINISH THE GOAL. This is the shape
	// the count was wrong in: `cancelled` is a FINISHED group, so a target
	// counting finished groups reported a team that gave up as having met
	// its goal.
	cancelled := tracker.StatusCancelled
	if _, err := r.writer.UpdateTask(t.Context(), "op-abandon", "gt-3", "ENG", 0,
		tracker.TaskPatch{Status: &cancelled}, tracker.ChangeStatus, nil); err != nil {
		t.Fatalf("cancel the last open task: %v", err)
	}
	r.drain()
	abandoned := r.goals(tracker.GoalQuery{ID: "g-1"}).Goals[0]
	if abandoned.Progress == nil || *abandoned.Progress != 0.5 {
		t.Fatalf("the goal is at %v after the remaining work was CANCELLED, "+
			"want 0.5 — cancelling is how this design spells abandoned, and a "+
			"goal a team can complete by giving up measures nothing",
			abandoned.Progress)
	}
	if after.Version != goal.Version {
		t.Fatalf("the goal's own version moved from %d to %d for a task's "+
			"change, so the progress is stored after all",
			goal.Version, after.Version)
	}
}

// A GOAL WITH NOTHING TO MEASURE HAS NO PROGRESS, and that is not zero.
//
// "Nothing has happened" and "there is nothing to measure" are different
// facts. A goal rendered at 0% because nobody set a target is one somebody
// escalates.
func TestAGoalWithNoTargetsHasNoProgress(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)

	if _, err := r.writer.WriteGoal(t.Context(), "op-bare",
		aGoal("g-bare", func(g *tracker.Goal) { g.Targets = nil })); err != nil {
		t.Fatalf("save: %v", err)
	}
	r.drain()

	goal := r.goals(tracker.GoalQuery{ID: "g-bare"}).Goals[0]
	if goal.Progress != nil {
		t.Fatalf("a goal with no targets is at %v, want no answer at all",
			*goal.Progress)
	}
}

// EVERY TARGET TYPE HAS ITS OWN ARITHMETIC, and a numeric one that overshoots
// is CLAMPED rather than reported above one.
//
// A progress bar at 130% is a rendering bug in every screen that draws one,
// and the raw number is on the target itself, so nothing is lost.
func TestEachTargetTypeCountsItsOwnWay(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)

	goal := aGoal("g-mixed", func(g *tracker.Goal) {
		g.Targets = []tracker.GoalTarget{
			{ID: "half", Name: "Halfway", Type: tracker.TargetNumber,
				Start: 0, Goal: 100, Current: 50},
			{ID: "over", Name: "Overshot", Type: tracker.TargetNumber,
				Start: 0, Goal: 100, Current: 130},
			{ID: "back", Name: "Went backwards", Type: tracker.TargetNumber,
				Start: 0, Goal: 100, Current: -20},
			{ID: "yes", Name: "Done", Type: tracker.TargetBinary, Done: true},
			{ID: "no", Name: "Not done", Type: tracker.TargetBinary},
		}
	})
	if _, err := r.writer.WriteGoal(t.Context(), "op-mixed", goal); err != nil {
		t.Fatalf("save: %v", err)
	}
	r.drain()

	want := map[string]float64{
		"half": 0.5, "over": 1, "back": 0, "yes": 1, "no": 0,
	}
	got := r.goals(tracker.GoalQuery{ID: "g-mixed"}).Goals[0]
	for _, target := range got.Targets {
		if target.Progress == nil {
			t.Fatalf("target %s measures nothing", target.ID)
		}
		if *target.Progress != want[target.ID] {
			t.Fatalf("target %s is at %v, want %v",
				target.ID, *target.Progress, want[target.ID])
		}
	}
	// THE MEAN, UNWEIGHTED: (0.5 + 1 + 0 + 1 + 0) / 5.
	if got.Progress == nil || *got.Progress != 0.5 {
		t.Fatalf("the goal is at %v, want the unweighted mean 0.5", got.Progress)
	}
}

// A GOAL'S SCOPE COVERS THE PROJECTS ITS TARGETS NAME.
//
// A task filtered by `goal=<id>` reads the target references, so a read scoped
// to a project has to wait for a deferred goal record that would put one of
// its tasks in the answer — and a scope naming only the goal would let such a
// read report itself complete while missing it.
func TestAGoalsScopeReachesTheProjectsItCounts(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)

	if _, err := r.writer.WriteGoal(t.Context(), "op-scoped", aGoal("g-1", nil)); err != nil {
		t.Fatalf("save: %v", err)
	}
	r.drain()

	scope := r.scopeOfLastRecord()
	engs := tracker.ScopeTerm{Kind: tracker.TermContainer, ID: "ENG"}.Path()
	if !statelog.Covers(engs, engs) {
		t.Fatal("the containment helper does not agree with itself")
	}
	var found bool
	for _, path := range scope.Paths {
		if statelog.Covers(path, engs) {
			found = true
		}
	}
	if !found {
		t.Fatalf("the goal's scope is %v and none of it covers %s — a project "+
			"read would never wait for this record", scope.Paths, engs)
	}
}

// LISTING NARROWS BY OWNER, BY GROUP AND BY ARCHIVE.
//
// "My goals" means owned OR contributed to: a person working towards an
// outcome wants it on their page, and the member flag is what tells a report
// which they are.
func TestAGoalListingNarrowsTheWayAPersonAsks(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)

	for _, spec := range []struct {
		id, group string
		owners    []string
		members   []string
		archived  bool
	}{
		{"g-ana", "H1", []string{"ana"}, nil, false},
		{"g-bob", "H1", []string{"bob"}, []string{"ana"}, false},
		{"g-else", "H2", []string{"cleo"}, nil, false},
		{"g-old", "H1", []string{"ana"}, nil, true},
	} {
		if _, err := r.writer.WriteGoal(t.Context(), "op-"+spec.id,
			aGoal(spec.id, func(g *tracker.Goal) {
				g.Owners, g.Members = spec.owners, spec.members
				g.Group, g.Archived = spec.group, spec.archived
			})); err != nil {
			t.Fatalf("save %s: %v", spec.id, err)
		}
		r.drain()
	}

	for name, tc := range map[string]struct {
		query tracker.GoalQuery
		want  []string
	}{
		"everything open":  {tracker.GoalQuery{}, []string{"g-ana", "g-bob", "g-else"}},
		"ana's":            {tracker.GoalQuery{Owner: "ana"}, []string{"g-ana", "g-bob"}},
		"one group":        {tracker.GoalQuery{Group: "H1"}, []string{"g-ana", "g-bob"}},
		"the archived too": {tracker.GoalQuery{Archived: true}, []string{"g-ana", "g-bob", "g-else", "g-old"}},
		"ana's, archived":  {tracker.GoalQuery{Owner: "ana", Archived: true}, []string{"g-ana", "g-bob", "g-old"}},
	} {
		t.Run(name, func(t *testing.T) {
			var got []string
			for _, goal := range r.goals(tc.query).Goals {
				got = append(got, goal.ID)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("%v, want %v", got, tc.want)
			}
			for _, id := range tc.want {
				if !containsString(got, id) {
					t.Fatalf("%v, want %v", got, tc.want)
				}
			}
		})
	}

	// AND A READ WITH NO LEVEL IS REFUSED, like every other read here.
	if _, err := r.reader.Goals(t.Context(), tracker.GoalQuery{}); err == nil {
		t.Fatal("a goal read with no read level was answered")
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var out []byte
	for n > 0 {
		out = append([]byte{byte('0' + n%10)}, out...)
		n /= 10
	}
	return string(out)
}

// A GOAL'S FILTER AND ITS PROGRESS READ THE SAME ROWS.
//
// A target names TASKS AND PROJECTS, and the goal's own progress counts a task
// reached either way. `goal=<id>` matched task refs alone, so a goal whose
// target is a project scored over every task in it and listed none of them —
// two readings of one row set, disagreeing with nobody to notice.
func TestTheGoalFilterReachesEverythingTheGoalCounts(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)

	for i := range 2 {
		task := newTask("gf-" + itoa(i))
		task.Key = "ENG-" + itoa(i)
		if _, err := r.writer.CreateTask(t.Context(), "op-gf-"+itoa(i), task, nil); err != nil {
			t.Fatalf("seed a task: %v", err)
		}
		r.drain()
	}
	// THE FIXTURE'S TARGET IS A PROJECT — `Projects: ["ENG"]` — which is
	// exactly the half the filter could not see.
	if _, err := r.writer.WriteGoal(t.Context(), "op-goal", aGoal("g-f", nil)); err != nil {
		t.Fatalf("save the goal: %v", err)
	}
	r.drain()

	counted := r.goals(tracker.GoalQuery{ID: "g-f"}).Goals[0].Targets[0].Total
	if counted != 2 {
		t.Fatalf("the target counts %d tasks, want the 2 in ENG", counted)
	}
	answer := r.ask(map[string]any{"container": "project:ENG", "goal": "g-f"})
	if len(answer.Rows) != counted {
		t.Fatalf("goal=g-f lists %d tasks while the same goal's target counts "+
			"%d of them — the filter and the progress read one row set two "+
			"different ways", len(answer.Rows), counted)
	}
}

// AND A SAVE THAT DROPS A PROJECT STILL NAMES IT.
//
// The apply rewrites every target reference the goal has, so a project it is
// LEAVING has its rows written out by this same record. A scope derived from
// the NEW targets alone named only what remained — the under-declaration a
// project move makes, from the other side — and a read scoped to the dropped
// project never waited for the record that emptied it.
func TestAGoalSaveNamesTheProjectItStopsCounting(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)

	if _, err := r.writer.WriteGoal(t.Context(), "op-both", aGoal("g-move",
		func(g *tracker.Goal) {
			g.Targets[0].Projects = []string{"ENG", "OPS"}
		})); err != nil {
		t.Fatalf("save: %v", err)
	}
	r.drain()

	if _, err := r.writer.WriteGoal(t.Context(), "op-drop", aGoal("g-move",
		func(g *tracker.Goal) {
			g.Targets[0].Projects = []string{"ENG"}
		})); err != nil {
		t.Fatalf("drop OPS: %v", err)
	}
	r.drain()

	scope := r.scopeOfLastRecord()
	ops := tracker.ScopeTerm{Kind: tracker.TermContainer, ID: "OPS"}.Path()
	var found bool
	for _, path := range scope.Paths {
		if statelog.Covers(path, ops) {
			found = true
		}
	}
	if !found {
		t.Fatalf("the save that dropped OPS carries the scope %v, none of "+
			"which covers %s — the record wrote OPS's target rows out and "+
			"named every project but that one", scope.Paths, ops)
	}
}

// A GOAL THAT HOLDS ITS CAP OF UPDATES REFUSES THE NEXT ONE, AND DROPS NONE.
//
// A goal kept a rolling window: the oldest update went to make room for the
// newest, with a warning saying it was readable nowhere. An update is the
// stored value rather than a preview of one, so that is a blind cut of what
// somebody wrote. The save is refused instead, naming the cap — and the health
// still moves, because it is the goal's own field rather than an update.
func TestAFullGoalRefusesAnUpdateAndDropsNone(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)

	// FILL IT EXACTLY, and that is accepted.
	fill := aGoal("g-1", func(g *tracker.Goal) {
		for i := range tracker.MaxGoalUpdates {
			g.Updates = append(g.Updates, tracker.GoalUpdate{
				Health: string(tracker.HealthOnTrack),
				Text:   fmt.Sprintf("week %d", i),
			})
		}
	})
	if _, err := r.writer.WriteGoal(t.Context(), "op-fill", fill); err != nil {
		t.Fatalf("a goal filled exactly to its cap was refused: %v", err)
	}
	r.drain()

	// ONE MORE IS REFUSED, naming the cap and what to do instead.
	over := aGoal("g-1", func(g *tracker.Goal) {
		g.Health = string(tracker.HealthAtRisk)
		g.Updates = []tracker.GoalUpdate{{
			Health: string(tracker.HealthAtRisk), Text: "the quarter slipped",
		}}
	})
	_, err := r.writer.WriteGoal(t.Context(), "op-over", over)
	if !errors.Is(err, tracker.ErrGoalUpdatesFull) {
		t.Fatalf("an update past the cap answered %v, want ErrGoalUpdatesFull "+
			"— the alternative is an update dropped where nobody can read it", err)
	}
	for _, want := range []string{fmt.Sprint(tracker.MaxGoalUpdates), "health",
		"nothing was saved"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not say %q: %v", want, err)
		}
	}
	r.drain()
	updates := r.goals(tracker.GoalQuery{ID: "g-1"}).Goals[0].Updates
	if len(updates) != tracker.MaxGoalUpdates || updates[0].Text != "week 0" {
		t.Fatalf("after the refusal the goal holds %d updates starting %q — "+
			"every one it had, oldest first, is what a refusal leaves",
			len(updates), updates[0].Text)
	}

	// AND THE HEALTH STILL MOVES, with no update beside it.
	health := aGoal("g-1", func(g *tracker.Goal) {
		g.Health = string(tracker.HealthAtRisk)
	})
	if _, err := r.writer.WriteGoal(t.Context(), "op-health", health); err != nil {
		t.Fatalf("a health change with no update was refused at the cap: %v", err)
	}
	r.drain()
	if got := r.goals(tracker.GoalQuery{ID: "g-1"}).Goals[0]; got.Health != tracker.HealthAtRisk ||
		len(got.Updates) != tracker.MaxGoalUpdates {
		t.Fatalf("the goal reads health %q with %d updates, want at_risk and "+
			"all %d", got.Health, len(got.Updates), tracker.MaxGoalUpdates)
	}
}

// A GOAL AT EVERY CAP FITS ONE RECORD.
//
// A goal is saved whole, updates and all, so [tracker.MaxGoalUpdates] is not a
// number of its own: it is what is left of [tracker.MaxCommitBytes] once
// everything else a goal may carry is at its own cap, at the six-fold JSON
// escaping the task patch's maximum is measured at. A cap past that is a save
// the writer refuses as oversized with nothing in the refusal about updates.
// The case runs the maximal SECOND save — the one whose notification carries
// every delta — through the real writer.
func TestAGoalAtEveryCapFitsOneRecord(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)

	escaping := func(n int) string { return strings.Repeat("\x01", n) }
	targets := make([]tracker.GoalTarget, 0, tracker.MaxGoalTargets)
	for i := range tracker.MaxGoalTargets {
		tasks := make([]string, 0, tracker.MaxTasksPerTarget)
		projects := make([]string, 0, tracker.MaxTasksPerTarget)
		for j := range tracker.MaxTasksPerTarget {
			tasks = append(tasks, fmt.Sprintf("%08d-0000-4000-8000-%012d", i, j))
			projects = append(projects, fmt.Sprintf("P%02dX%04d", i, j))
		}
		targets = append(targets, tracker.GoalTarget{
			ID: fmt.Sprintf("target-%02d", i), Name: escaping(tracker.MaxGoalName),
			Type: tracker.TargetTasks, Tasks: tasks, Projects: projects,
		})
	}
	update := func(label string) tracker.GoalUpdate {
		return tracker.GoalUpdate{
			Health: string(tracker.HealthOffTrack),
			Text:   label + escaping(tracker.MaxGoalUpdateText-len(label)),
		}
	}
	due := wednesday.Add(90 * 24 * time.Hour)
	first := aGoal("g-max", func(g *tracker.Goal) {
		g.Name = escaping(tracker.MaxGoalName)
		g.Description = escaping(tracker.MaxGoalDescription)
		g.Group = escaping(tracker.MaxGoalGroup)
		g.Owners = longHandles(tracker.MaxGoalOwners, 64)
		g.Members = longHandles(tracker.MaxGoalMembers, 64)
		g.Targets = targets
		for i := range tracker.MaxGoalUpdates - 1 {
			g.Updates = append(g.Updates, update(fmt.Sprintf("%d ", i)))
		}
	})
	if _, err := r.writer.WriteGoal(t.Context(), "op-max-1", first); err != nil {
		t.Fatalf("a goal one update short of every cap was refused: %v", err)
	}
	r.drain()

	// EVERY DELTA MOVES, so the notification is as large as a goal's gets.
	second := first
	second.Name = "x" + escaping(tracker.MaxGoalName-1)
	second.Owners = longHandles(tracker.MaxGoalOwners, 63)
	second.Members = longHandles(tracker.MaxGoalMembers, 63)
	second.Health = string(tracker.HealthOffTrack)
	second.StartAt, second.DueAt = &wednesday, &due
	second.Archived = true
	second.Updates = []tracker.GoalUpdate{update("last ")}
	if _, err := r.writer.WriteGoal(t.Context(), "op-max-2", second); err != nil {
		t.Fatalf("a goal at every cap was refused: %v — MaxGoalUpdates is sized "+
			"from what MaxCommitBytes leaves, and this is a save the caps "+
			"promise to accept", err)
	}
	r.drain()
	bytes := r.strings(`SELECT CAST(length(document) AS TEXT) FROM tracker_history
		WHERE subject_kind = 'goal' AND subject_id = 'g-max'
		ORDER BY log_seq DESC LIMIT 1`)
	t.Logf("the maximal goal's document is %s bytes against a %d-byte record",
		bytes, tracker.MaxCommitBytes)
}
