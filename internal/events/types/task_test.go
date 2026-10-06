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
