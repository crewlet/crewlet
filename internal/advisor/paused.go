package advisor

import (
	"cmp"
	"fmt"
	"maps"
	"slices"
)

// The classes of an agent the engine paused for a failure a retry cannot cure
// (ADR-0117): one per failure in the engine's table, each the owner's to fix.
const (
	ClassSandboxUnstartable Class = "sandbox.image_unstartable"
	ClassModelsCredential   Class = "models.credential_refused"
)

// PausedSeat is an agent seat the engine paused, with the failure's kind as
// the pause record names it.
type PausedSeat struct {
	Handle, Name, Failure string
}

// pausedFailure is one failure kind's finding: what paused the agents, and
// the fix. because finishes "Paused because ...", after the agents' names.
type pausedFailure struct {
	class   Class
	because string
	fix     Fix
}

// pausedFailures is keyed by the engine's failure kinds, which
// internal/engine.TestEveryFailurePauseHasTheAdvisorsFix holds it to.
var pausedFailures = map[string]pausedFailure{
	"sandbox_configuration": {
		class: ClassSandboxUnstartable, because: "the sandbox could not start as configured",
		fix: Fix{Kind: FixOpenSetting, Setting: "providers.sandbox",
			Sentence: "Correct the sandbox settings, such as its image. Saving the change resumes " +
				"the paused agents; if the fix was made on the host or in the image's registry " +
				"instead, resume each paused agent from its page."},
	},
	"models_refused": {
		class: ClassModelsCredential, because: "no model accepted the credential",
		fix: Fix{Kind: FixExternalStep, Setting: "providers.llm",
			Sentence: "Replace the models' credentials under Settings, Models, then resume each " +
				"paused agent from its page. Changing the models they run on resumes them as well."},
	},
}

// PausedFailureClass is the class of a seat the engine paused for kind.
func PausedFailureClass(kind string) (Class, bool) {
	f, ok := pausedFailures[kind]
	return f.class, ok
}

// PausedClasses is every class a paused seat raises, sorted. The dashboard
// reads it ([TestTheDashboardKnowsEveryClassAPausedSeatRaises]): a seat the
// engine paused is said by its failure's finding, which names it and the fix,
// and not by a second row of its own.
func PausedClasses() []Class {
	out := make([]Class, 0, len(pausedFailures))
	for _, f := range pausedFailures {
		out = append(out, f.class)
	}
	slices.Sort(out)
	return out
}

// pausedSeats is one blocking finding per failure kind that holds an agent,
// in that kind's words whoever it holds, naming every agent it holds in Names.
// A kind this build does not know raises nothing.
func pausedSeats(ev Evidence) []Finding {
	byKind := map[string][]PausedSeat{}
	for _, p := range ev.Paused {
		if _, ok := pausedFailures[p.Failure]; ok {
			byKind[p.Failure] = append(byKind[p.Failure], p)
		}
	}
	var out []Finding
	for _, kind := range slices.Sorted(maps.Keys(byKind)) {
		seats, f := byKind[kind], pausedFailures[kind]
		slices.SortFunc(seats, func(a, b PausedSeat) int { return cmp.Compare(a.Handle, b.Handle) })
		names := make([]string, len(seats))
		for i, s := range seats {
			names[i] = s.Name
		}
		subject := Subject{Kind: SubjectCompany}
		if len(seats) == 1 {
			subject = Subject{Kind: SubjectSeat, Seat: seats[0].Handle}
		}
		fix := f.fix
		fix.Options = []Option{}
		out = append(out, Finding{
			Class: f.class, Severity: SeverityBlocks, Actor: ActorOwner, Source: SourceEvidence,
			Area: AreaSetup, Subject: subject,
			Title: fmt.Sprintf("Paused because %s.", f.because),
			Why: "Work sent to a paused agent waits until it resumes; nothing is dropped. The engine " +
				"pauses an agent after the same failure twice in a row, since another try would fail " +
				"the same way.",
			Names:    names,
			Evidence: []Fact{{Key: "agents", Value: int64(len(seats))}},
			Fix:      fix,
		})
	}
	return out
}
