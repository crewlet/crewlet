package advisor_test

import (
	"encoding/json"
	"testing"

	"github.com/crewlet/crewlet/internal/advisor"
	"github.com/crewlet/crewlet/internal/org"
)

// readBack is s as the duty reads it from the log: its observation decoded
// from the body the log keeps.
func readBack(t *testing.T, s advisor.Stored) advisor.Stored {
	t.Helper()
	var o advisor.Observation
	if err := json.Unmarshal(s.Body, &o); err != nil {
		t.Fatal(err)
	}
	s.Observation = o
	return s
}

// scheduled is an unproductive schedule's finding whose fix carries the
// schedules as the document types them, with fires facts.
func scheduled(fires int64) advisor.Finding {
	schedules := []org.Schedule{{Name: "poll", Cron: "0 9 * * *", Task: "Look."}}
	return advisor.Finding{Class: advisor.ClassScheduleUnproductive, Severity: advisor.SeverityCosts,
		Actor: advisor.ActorOwner, Source: advisor.SourceEvidence, Area: advisor.AreaSchedules,
		Subject:  advisor.Subject{Kind: advisor.SubjectSchedule, Seat: "dev", Schedule: "poll", Name: "poll"},
		Evidence: []advisor.Fact{{Key: "fires", Value: fires}, {Key: "moved", Value: 0}},
		Fix: advisor.Fix{Kind: advisor.FixConfigChange, Sentence: "Run poll less often.", Options: []advisor.Option{{
			Label: "Run it once a day", Change: advisor.Change{Entity: advisor.EntityRoles, ID: "dev",
				Set: map[string]any{"schedules": schedules}, Summary: "Run schedule poll once a day"}}}}}
}

// A FINDING READ BACK FROM THE LOG DECIDES AS THE ONE JUST OBSERVED: a second
// pass that moves only its facts is a refresh, never a revision, whatever
// typed values its fix carries (a schedule, whose fields a struct encodes in
// its own order and a decoded copy in sorted order). Mutation: digest a
// change's values as built, and the schedule's fix read back is a revision on
// every pass, which the log refuses as moving neither verdict nor fix.
func TestAFindingReadBackFromTheLogDecidesAsTheOneJustObserved(t *testing.T) {
	t.Parallel()
	first := observed(scheduled(7))
	stored := readBack(t, storedAs(t, first, advisor.StateOpen))
	if advisor.FixDigest(stored.Observation.Finding.Fix) != advisor.FixDigest(first.Finding.Fix) ||
		advisor.OptionDigest(stored.Observation.Finding.Fix.Options[0]) != advisor.OptionDigest(first.Finding.Fix.Options[0]) {
		t.Error("the fix read back from the log digests otherwise than the fix it was written from")
	}
	steps := advisor.Reconcile([]advisor.Stored{stored}, []advisor.Observation{observed(scheduled(9))}, evidenceRun())
	if len(steps) != 1 || steps[0].Step != advisor.StepRefresh {
		t.Fatalf("steps %v, want one refresh: only its facts moved", stepsOf(steps))
	}
}
