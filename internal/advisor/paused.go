package advisor

import (
	"cmp"
	"fmt"
	"maps"
	"slices"

	"github.com/crewlet/crewlet/internal/words"
)

// The classes of an agent the engine paused for a failure a retry cannot cure
// (ADR-0117): one per failure in the engine's table, each the owner's to fix.
const (
	ClassSandboxUnstartable Class = "sandbox.image_unstartable"
	ClassSandboxCredential  Class = "sandbox.credential_refused"
	ClassSandboxBilling     Class = "sandbox.billing_refused"
	ClassModelsCredential   Class = "models.credential_refused"
)

// PausedSeat is an agent seat the engine paused, with the failure's kind as
// the pause record names it.
type PausedSeat struct {
	Handle, Name, Failure string
}

// pausedFailure is one failure kind's finding: what paused the agents, and
// the fix. because finishes "<agents> are paused because ...".
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
			Sentence: "Correct the sandbox settings, such as its image or template. Saving the " +
				"change resumes the paused agents; if the fix was made at the provider instead, " +
				"resume each paused agent from its page."},
	},
	"sandbox_refused": {
		class: ClassSandboxCredential, because: "the sandbox provider refused the engine's credential",
		fix: Fix{Kind: FixExternalStep, Setting: "providers.sandbox",
			Sentence: "Replace the sandbox provider's key, then resume each paused agent from its page."},
	},
	"sandbox_billing": {
		class: ClassSandboxBilling, because: "the sandbox provider refused for billing",
		fix: Fix{Kind: FixExternalStep, Setting: "providers.sandbox",
			Sentence: "Settle the account's billing with the sandbox provider, then resume each " +
				"paused agent from its page."},
	},
	"models_refused": {
		class: ClassModelsCredential, because: "no model would accept its credential",
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

// pausedClasses is every class a paused seat raises.
func pausedClasses() []Class {
	out := make([]Class, 0, len(pausedFailures))
	for _, f := range pausedFailures {
		out = append(out, f.class)
	}
	slices.Sort(out)
	return out
}

// pausedSeats is one blocking finding per failure kind that holds an agent,
// naming every agent it holds. A kind this build does not know raises nothing.
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
		names := make([]words.Said, len(seats))
		for i, s := range seats {
			names[i] = words.Said(s.Name)
		}
		subject, verb, them := Subject{Kind: SubjectCompany}, "are", "them"
		if len(seats) == 1 {
			subject, verb, them = Subject{Kind: SubjectSeat, Seat: seats[0].Handle}, "is", "it"
		}
		fix := f.fix
		fix.Options = []Option{}
		out = append(out, Finding{
			Class: f.class, Severity: SeverityBlocks, Actor: ActorOwner, Source: SourceEvidence,
			Area: AreaSetup, Subject: subject,
			Title: fmt.Sprintf("%s %s paused because %s.", words.And(names), verb, f.because),
			Why: fmt.Sprintf("The engine paused %s after the same failure twice in a row, since "+
				"another try would fail the same way. Work sent to %s waits and nothing is dropped.",
				them, them),
			Evidence: []Fact{{Key: "agents", Value: int64(len(seats))}},
			Fix:      fix,
		})
	}
	return out
}
