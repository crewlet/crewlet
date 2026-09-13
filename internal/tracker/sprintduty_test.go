package tracker_test

import (
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/maintenance"
	"github.com/crewlet/crewlet/internal/tracker"
)

// sprintJob runs the duty's sprint job once and reports what it wrote.
func sprintJob(t *testing.T, r *roundTrip, now time.Time) int64 {
	t.Helper()
	var job maintenance.Job
	for _, candidate := range tracker.Jobs(tracker.DutyDeps{
		DB: r.db, Writer: r.writer, NodeID: "node-a",
		Logger: slog.New(slog.DiscardHandler),
	}) {
		if candidate.Name == "tracker_sprints" {
			job = candidate
		}
	}
	if job.Run == nil {
		t.Fatal("the duty has no sprint job")
	}
	if job.Gate != nil {
		on, err := job.Gate(t.Context())
		if err != nil {
			t.Fatalf("the sprint gate: %v", err)
		}
		if !on {
			return 0
		}
	}
	wrote, err := job.Run(t.Context(), now, now)
	if err != nil {
		t.Fatalf("the sprint job: %v", err)
	}
	r.drain()
	return wrote
}

// sprintingProject gives ENG a sprint policy.
func sprintingProject(t *testing.T, r *roundTrip, policy tracker.SprintPolicy) {
	t.Helper()
	seedProject(t, r, tracker.Project{
		Key: "ENG", Name: "Engineering", Sprints: &policy,
	})
}

// THE GATE IS ONE INDEXED READ, and a company with no sprints pays that and
// nothing else — which is the shape every duty here has.
func TestTheSprintDutyDoesNothingWhereThereAreNoSprints(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	if wrote := sprintJob(t, r, wednesday); wrote != 0 {
		t.Fatalf("the sprint job wrote %d on a company with no sprints", wrote)
	}
}

// SPRINTS ARE MINTED AHEAD, as create-only appends at expectation zero — so
// two nodes minting the same number contend at the broker and the loser's
// refusal means the sprint it wanted exists.
func TestTheDutyMintsSprintsAhead(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	sprintingProject(t, r, tracker.SprintPolicy{
		LengthDays: 14, Ahead: 3, Next: 1, StartWeekday: time.Monday,
	})

	sprintJob(t, r, wednesday)
	listing := r.sprints(tracker.SprintQuery{Project: "ENG"}, wednesday)
	if len(listing.Sprints) != 3 {
		t.Fatalf("the duty minted %d sprints, want the policy's three",
			len(listing.Sprints))
	}
	// EACH ONE STARTS WHERE THE LAST ENDED, so a cadence stays a cadence:
	// computed from `now` alone every sprint minted in one tick would
	// start on the same day.
	for i := 1; i < len(listing.Sprints); i++ {
		prev, next := listing.Sprints[i-1], listing.Sprints[i]
		if !next.StartAt.Equal(prev.EndAt) {
			t.Errorf("sprint %d starts at %v and sprint %d ended at %v — a "+
				"cadence is contiguous", next.Number, next.StartAt,
				prev.Number, prev.EndAt)
		}
		if next.Number != prev.Number+1 {
			t.Errorf("the numbers are %d then %d", prev.Number, next.Number)
		}
	}
	// AND THE WINDOW IS A CALENDAR BOUNDARY: the policy names a weekday,
	// not an offset from whenever the duty happened to run.
	if got := listing.Sprints[0].StartAt.Weekday(); got != time.Monday {
		t.Errorf("the first sprint starts on a %v, want the policy's Monday", got)
	}

	// A SECOND TICK MINTS NOTHING, because the project is already at its
	// `ahead` count — which is what makes this free on a healthy company.
	if wrote := sprintJob(t, r, wednesday); wrote != 0 {
		t.Errorf("a second tick wrote %d, want nothing to do", wrote)
	}
}

// A SPRINT ALWAYS CLOSES AT ITS END, and that is not a setting: a sprint that
// ran past its own window is a number nobody can report on, because every
// figure in a sprint report is a predicate over that window.
func TestTheDutyClosesASprintAtItsEnd(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	base := time.Now().UTC().Truncate(time.Microsecond)
	sprintingProject(t, r, tracker.SprintPolicy{LengthDays: 14, Next: 1})
	seedSprintWindow(t, r, 1, tracker.SprintActive,
		base.Add(-15*24*time.Hour), base.Add(-time.Hour), nil)
	one := 1
	pointedTask(t, r, "left-over", 5, &one, "ada")

	sprintJob(t, r, base)
	got := r.sprints(tracker.SprintQuery{Project: "ENG"}, base).Sprints[0]
	if got.State != tracker.SprintClosed {
		t.Fatalf("the sprint is %s past its end, want closed — a close is not "+
			"a setting", got.State)
	}
	if got.ClosedAt == nil {
		t.Error("the close stamped no instant")
	}
	// AND IT CHANGES NO TASK'S STATUS. The close is about the SPRINT; what
	// happens to the unfinished work is the rollover's.
	if rows := ids(r.ask(map[string]any{
		"container": "project:ENG", "status": "todo",
	})); len(rows) != 1 {
		t.Errorf("the close moved a task: %v", rows)
	}
}

// `auto_roll` CARRIES THE WORK FORWARD; without it the spillover is PENDING,
// which is a state a lead settles rather than an absence to interpret.
func TestTheDutyRollsOrLeavesTheSpilloverPending(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	base := time.Now().UTC().Truncate(time.Microsecond)
	sprintingProject(t, r, tracker.SprintPolicy{
		LengthDays: 14, Next: 1, Ahead: 2, AutoRoll: false,
	})
	seedSprintWindow(t, r, 1, tracker.SprintActive,
		base.Add(-15*24*time.Hour), base.Add(-time.Hour), nil)
	one := 1
	pointedTask(t, r, "unfinished", 5, &one, "ada")

	sprintJob(t, r, base)
	pending := r.sprints(tracker.SprintQuery{Project: "ENG", Number: 1}, base).Sprints[0]
	if !pending.RolloverPending {
		t.Fatal("the sprint closed without auto_roll and is not reported " +
			"pending — a lead has to be able to see there is a decision")
	}
	// THE WORK HAS NOT MOVED, which is the whole of that setting: it stays
	// where everybody left it until somebody says where it goes.
	if got := ids(r.ask(map[string]any{
		"container": "project:ENG", "sprint": "1",
	})); len(got) != 1 {
		t.Errorf("sprint 1 holds %v, want the unfinished task still in it", got)
	}

	// AND A LEAD SETTLES IT, which is what `manage_sprint(rollover)` does
	// through this same verb.
	moved, done, err := r.writer.RolloverSprint(t.Context(), "op-roll", "ENG", 1,
		tracker.RolloverNext)
	if err != nil {
		t.Fatalf("RolloverSprint: %v", err)
	}
	r.drain()
	if moved != 1 || !done {
		t.Fatalf("the rollover moved %d and reported done=%v, want 1 and true",
			moved, done)
	}
	settled := r.sprints(tracker.SprintQuery{Project: "ENG", Number: 1}, base).Sprints[0]
	if settled.RolloverPending {
		t.Error("the sprint is still pending after its spillover was settled")
	}
	// THE TASK IS IN THE NEXT SPRINT, and its stay says so — which is what
	// makes a carry-over visible in both sprints' figures.
	if got := ids(r.ask(map[string]any{
		"container": "project:ENG", "sprint": "2",
	})); len(got) != 1 || got[0] != "unfinished" {
		t.Fatalf("sprint 2 holds %v, want the carried-over task", got)
	}
}

// `backlog` CLEARS THE POINTER and leaves the task in its project, which is
// what the Backlog IS here: not a container, but the absence of a sprint.
func TestARolloverToBacklogAndToClose(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	base := time.Now().UTC().Truncate(time.Microsecond)
	closed := base.Add(-time.Hour)
	sprintingProject(t, r, tracker.SprintPolicy{LengthDays: 14, Next: 1})
	seedSprintWindow(t, r, 1, tracker.SprintClosed,
		base.Add(-15*24*time.Hour), closed, &closed)
	one := 1
	pointedTask(t, r, "dropped", 3, &one, "ada")

	if _, _, err := r.writer.RolloverSprint(t.Context(), "op-b", "ENG", 1,
		tracker.RolloverBacklog); err != nil {
		t.Fatalf("RolloverSprint: %v", err)
	}
	r.drain()
	if got := ids(r.ask(map[string]any{
		"container": "project:ENG", "sprint": "none",
	})); len(got) != 1 || got[0] != "dropped" {
		t.Fatalf("the backlog holds %v, want the task whose sprint was cleared",
			got)
	}

	// AND `close` CANCELS what is left — abandoned, not delivered, which
	// is exactly why `cancelled` is a finished group Delivered excludes.
	seedSprintWindow(t, r, 2, tracker.SprintClosed,
		base.Add(-15*24*time.Hour), closed, &closed)
	two := 2
	pointedTask(t, r, "abandoned", 3, &two, "ada")
	if _, _, err := r.writer.RolloverSprint(t.Context(), "op-c", "ENG", 2,
		tracker.RolloverClose); err != nil {
		t.Fatalf("RolloverSprint: %v", err)
	}
	r.drain()
	// `show_closed` IS NEEDED HERE, because a cancelled task is in a
	// FINISHED group and every board hides those by default — which is
	// also what makes "the close cancelled it" invisible without asking.
	detail := r.ask(map[string]any{
		"container": "project:ENG", "status": "cancelled", "show_closed": "true",
	})
	if got := ids(detail); len(got) != 1 || got[0] != "abandoned" {
		t.Fatalf("close cancelled %v, want the one open task", got)
	}
}

// A ROLLOVER INTO A CLOSED SPRINT IS REFUSED, and so is one into itself:
// either would put the work back where the rollover was called to take it out
// of, and the duty would find it again on every tick.
func TestARolloverRefusesAnImpossibleTarget(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	base := time.Now().UTC().Truncate(time.Microsecond)
	closed := base.Add(-time.Hour)
	sprintingProject(t, r, tracker.SprintPolicy{LengthDays: 14, Next: 1})
	seedSprintWindow(t, r, 1, tracker.SprintClosed,
		base.Add(-15*24*time.Hour), closed, &closed)
	seedSprintWindow(t, r, 2, tracker.SprintClosed,
		base.Add(-30*24*time.Hour), closed, &closed)

	if _, _, err := r.writer.RolloverSprint(t.Context(), "op-self", "ENG", 1,
		tracker.RolloverTarget("1")); err == nil {
		t.Error("a sprint was rolled into itself")
	}
	_, _, err := r.writer.RolloverSprint(t.Context(), "op-closed", "ENG", 1,
		tracker.RolloverTarget("2"))
	if err == nil {
		t.Fatal("work was rolled into a closed sprint")
	}
	if !strings.Contains(err.Error(), "backlog") {
		t.Errorf("the refusal is %q and does not name what to do instead", err)
	}

	// AND AN OPEN SPRINT CANNOT BE ROLLED OVER, because the spillover is
	// what a CLOSE leaves behind.
	seedSprintWindow(t, r, 3, tracker.SprintActive, base, base.AddDate(0, 0, 14), nil)
	if _, _, err := r.writer.RolloverSprint(t.Context(), "op-open", "ENG", 3,
		tracker.RolloverBacklog); err == nil {
		t.Error("an active sprint's spillover was settled")
	}
}

// `archive_after` HIDES A SETTLED SPRINT from the sprint screens and TOUCHES
// NO TASK — `sprint=<an archived number>` still answers from the stays.
func TestTheDutyArchivesSettledSprints(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	base := time.Now().UTC().Truncate(time.Microsecond)
	closed := base.Add(-time.Hour)
	sprintingProject(t, r, tracker.SprintPolicy{
		LengthDays: 14, Next: 4, ArchiveAfter: 2,
	})
	for n := 1; n <= 3; n++ {
		seedSprintWindow(t, r, n, tracker.SprintClosed,
			base.Add(-30*24*time.Hour), closed, &closed)
		if _, _, err := r.writer.RolloverSprint(t.Context(),
			"op-settle-"+itoa(n), "ENG", n, tracker.RolloverBacklog); err != nil {
			t.Fatalf("settle %d: %v", n, err)
		}
		r.drain()
	}
	one := 1
	pointedTask(t, r, "old", 3, &one, "ada")

	sprintJob(t, r, base)
	live := r.sprints(tracker.SprintQuery{Project: "ENG"}, base)
	for _, s := range live.Sprints {
		if s.Number == 1 {
			t.Errorf("sprint 1 is still listed with two newer sprints closed " +
				"and archive_after=2")
		}
	}
	// AND ITS STAYS STILL ANSWER, which is what "touches no task" means.
	if got := ids(r.ask(map[string]any{
		"container": "project:ENG", "sprint": "1",
	})); len(got) != 1 {
		t.Errorf("sprint=1 answers %v after the sprint was archived — an "+
			"archived sprint is hidden from a picker, not erased", got)
	}
	// AND A PROJECT WITH `archive_after` OFF ARCHIVES NOTHING, which is
	// the documented meaning of zero and the right default.
	if archived := r.sprints(tracker.SprintQuery{
		Project: "ENG", Archived: true,
	}, base); len(archived.Sprints) != 3 {
		t.Errorf("archived=true answers %d sprints, want all three",
			len(archived.Sprints))
	}
}
