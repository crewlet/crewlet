package types

import (
	"strings"
	"testing"
)

// A SCHEDULE'S BRIEF IS ITS TASK, NOT ITS FIRE'S ID. The brief led with "Task
// <id>", and the id is the scheduler's name for one fire — scope, schedule,
// instant, runner — which gates nothing and reads as a tracker key that names
// no item, so the seat was handed something to look up that is not there.
func TestAScheduledBriefIsTheTaskWithoutTheFiresID(t *testing.T) {
	t.Parallel()
	brief := TaskAssigned{
		TaskID:   "unit:Engineering:weekly-report:2026-09-28T09:00:00Z:swe",
		Schedule: "weekly-report", Description: "Summarise the week's merged PRs.",
	}.Brief()
	if want := "Scheduled work: weekly-report\n\nSummarise the week's merged PRs."; brief != want {
		t.Fatalf("brief = %q, want %q", brief, want)
	}
	if strings.Contains(brief, "2026-09-28") {
		t.Fatalf("the brief carries the fire's id: %q", brief)
	}
}

// A FIRE'S LABEL NAMES ITS SCHEDULE, NOT ITS ID, for the brief's reason and
// one more: the label is the trigger's summary, so it is the recalled
// episode's "woken by" line and the first paragraph its vector is made of.
// Led by the id, every fire of one schedule was unalike to a similarity
// search, and every recall handed the seat a key that names no item.
func TestAFiresLabelNamesItsScheduleNotItsID(t *testing.T) {
	t.Parallel()
	fire := func(instant string) TaskAssigned {
		return TaskAssigned{
			TaskID:   "unit:Engineering:weekly-report:" + instant + ":swe",
			Schedule: "weekly-report", Description: "Summarise the week's merged PRs.",
		}
	}
	first, second := fire("2026-09-21T09:00:00Z").SummaryFor("swe"), fire("2026-09-28T09:00:00Z").SummaryFor("swe")
	if want := "swe was assigned scheduled work weekly-report"; first != want || second != want {
		t.Fatalf("labels = %q, %q; want both %q", first, second, want)
	}
	// With no schedule there is nothing a seat could look the id up as.
	if got := (TaskAssigned{TaskID: "T-42"}).SummaryFor("swe"); got != "swe was assigned a task" {
		t.Fatalf("a fire with no schedule is labelled %q", got)
	}
}
