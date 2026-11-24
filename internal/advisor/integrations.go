package advisor

import (
	"strings"

	"github.com/crewlet/crewlet/internal/integration"
)

// FromIntegrations is every finding the integrations' last reconcile reported,
// in the integrations' own words. Who acts and how costly it is come from the
// kind's own verdict ([integration.FindingKind.Verdict]), never from a second
// table here: an admin's or an operator's finding is the owner's, the engine's
// or the vendor's is listed as theirs. A surface being disconnected is skipped.
func FromIntegrations(states []integration.State) []Finding {
	out := []Finding{}
	for _, state := range states {
		if state.Disconnecting {
			continue
		}
		name := SurfaceName(state.Kind)
		cut := state.FindingsRecord.WordsOmitted+state.FindingsRecord.SeatOmitted > 0
		for _, f := range state.Findings {
			out = append(out, fromIntegration(state.Kind, name, f, cut))
		}
	}
	return out
}

// IntegrationPath is the path every finding about a surface names.
func IntegrationPath(kind integration.Kind) string { return "integrations." + string(kind) }

// fromIntegration is one reconcile finding as the advisor lists it, marked
// [Finding.WordsCut] where its record cut words and it has none.
func fromIntegration(kind integration.Kind, name string, f integration.Finding, cut bool) Finding {
	phase, actor := f.Kind.Verdict()
	out := Finding{
		Class: Class("integration." + string(f.Kind)), Severity: severityOf(phase),
		Actor: ActorEngine, Source: SourceReconcile, Area: AreaIntegrations,
		Subject: Subject{Kind: SubjectIntegration, Name: name},
		Title:   f.Label(), Why: f.Detail, Path: IntegrationPath(kind), Focus: f.Subject,
		Evidence: []Fact{},
		Fix: Fix{Kind: FixNone, Sentence: "The engine is carrying this forward; nothing to do.",
			Options: []Option{}},
	}
	if strings.TrimSpace(f.Title) == "" {
		// THE KIND'S OWN WORDS say "this integration", and this list holds
		// every surface's findings, so it names the surface; the subject in
		// the label tells two findings of a kind on one surface apart.
		out.Title = name + ": " + out.Title + "."
	}
	if actor == integration.ActorAdmin || actor == integration.ActorOperator {
		out.Actor = ActorOwner
		out.Fix = Fix{Kind: FixExternalStep, Sentence: f.Remedy, Options: []Option{},
			Setting: string(kind), URL: f.ActionURL}
		if out.Fix.Sentence == "" {
			out.Fix.Sentence = "Open " + name + "'s settings and follow its steps."
		}
	}
	out.WordsCut = cut && f.Title == "" && f.Detail == "" && f.Remedy == "" && f.ActionURL == ""
	return out
}

// severityOf is how costly a reconcile phase is: a surface the engine cannot
// talk to, or one waiting on a person to approve it, blocks; one that works
// worse than it should costs; one that works is a note.
func severityOf(phase integration.Phase) Severity {
	switch phase {
	case integration.PhaseUnconfigured, integration.PhaseAwaitingAdmin:
		return SeverityBlocks
	case integration.PhaseDegraded:
		return SeverityCosts
	default:
		return SeverityNote
	}
}

// SurfaceName is how a sentence names a surface, from the catalogue of tools.
func SurfaceName(kind integration.Kind) string {
	for _, tool := range integration.Tools {
		for _, s := range tool.Surfaces {
			if s.Key == string(kind) {
				return s.Name
			}
		}
	}
	return string(kind)
}
