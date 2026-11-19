package advisor_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/advisor"
)

// overdueShape is the stall shape with Dev running a weekday standup.
func overdueShape(t *testing.T) string {
	t.Helper()
	return replace(t, stallShape, "  - name: Dev\n    handle: dev\n    llm: p\n",
		"  - name: Dev\n    handle: dev\n    llm: p\n    schedules:\n      - name: standup\n        cron: \"0 9 * * 1-5\"\n        task: Stand up.\n")
}

// A SCHEDULE WHOSE RUNS WERE SKIPPED AGAIN AND AGAIN IS THE OWNER'S: two in a
// week, said by why; while its agent was paused the fix opens the agent's page,
// and runs missed while no server ran the schedules open the catch-up window.
// One skip, or a schedule the document no longer has, is not raised.
// Mutation: raise at one skip, or open the setting for a paused agent, and a
// check fails.
func TestAScheduleWhoseRunsWereSkippedIsTheOwners(t *testing.T) {
	t.Parallel()
	doc := overdueShape(t)
	paused := advisor.ScheduleSkip{Seat: "dev", Name: "standup", Target: "dev", Paused: true}
	missed := advisor.ScheduleSkip{Seat: "dev", Name: "standup", Target: "dev"}
	found := stalls(t, doc, advisor.Evidence{Skips: []advisor.ScheduleSkip{paused, paused, missed}}, advisor.ClassScheduleOverdue)
	if len(found) != 1 || found[0].Subject.Schedule != "standup" || found[0].Subject.Seat != "dev" {
		t.Fatalf("raised %+v, want Dev's standup", found)
	}
	if f := found[0]; f.Item != "standup (3 runs skipped: 2 while paused, 1 missed)" || f.Fix.Kind != advisor.FixOpenPage ||
		f.Fix.Page != advisor.PageSeat || f.Fix.Target != "dev" {
		t.Errorf("paused = %+v", f)
	}
	found = stalls(t, doc, advisor.Evidence{Skips: []advisor.ScheduleSkip{missed, missed}}, advisor.ClassScheduleOverdue)
	if len(found) != 1 || found[0].Fix.Kind != advisor.FixOpenSetting || found[0].Fix.Setting != "scheduling.catchup_max_seconds" {
		t.Errorf("missed = %+v, want the catch-up window opened", found)
	}
	for name, skips := range map[string][]advisor.ScheduleSkip{
		"one skip":            {paused},
		"a schedule now gone": {{Seat: "dev", Name: "retro", Paused: true}, {Seat: "dev", Name: "retro", Paused: true}},
	} {
		if got := stalls(t, doc, advisor.Evidence{Skips: skips}, advisor.ClassScheduleOverdue); len(got) != 0 {
			t.Errorf("%s raised %+v", name, got)
		}
	}
	if got := stalls(t, doc, advisor.Evidence{Skips: []advisor.ScheduleSkip{paused, paused}, SkipsUnread: true},
		advisor.ClassScheduleOverdue); len(got) != 0 {
		t.Errorf("unread skips raised %+v", got)
	}
}
