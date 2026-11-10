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
		name := surfaceName(state.Kind)
		for _, f := range state.Findings {
			out = append(out, fromIntegration(state.Kind, name, f))
		}
	}
	return out
}

// fromIntegration is one reconcile finding as the advisor lists it.
func fromIntegration(kind integration.Kind, name string, f integration.Finding) Finding {
	phase, actor := f.Kind.Verdict()
	out := Finding{
		Class: Class("integration." + string(f.Kind)), Severity: severityOf(phase),
		Actor: ActorEngine, Source: SourceReconcile, Area: AreaIntegrations,
		Subject: Subject{Kind: SubjectIntegration, Name: name},
		Title:   f.Title, Why: f.Detail, Path: "integrations." + string(kind),
		Evidence: []Fact{},
		Fix: Fix{Kind: FixNone, Sentence: "The engine is carrying this forward; nothing to do.",
			Options: []Option{}},
	}
	if out.Title == "" {
		// The reconcile's subject names which one, so two findings of a kind
		// on one surface (two agents' refusals, say) do not read alike.
		out.Title = name + ": " + strings.ReplaceAll(string(f.Kind), "_", " ")
		if f.Subject != "" {
			out.Title += " (" + f.Subject + ")"
		}
		out.Title += "."
	}
	if actor == integration.ActorAdmin || actor == integration.ActorOperator {
		out.Actor = ActorOwner
		out.Fix = Fix{Kind: FixExternalStep, Sentence: f.Remedy, Options: []Option{},
			Setting: string(kind), URL: f.ActionURL}
		if out.Fix.Sentence == "" {
			out.Fix.Sentence = "Open " + name + "'s settings and follow its steps."
		}
	}
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

// surfaceName is how a sentence names a surface, from the catalogue of tools.
func surfaceName(kind integration.Kind) string {
	for _, tool := range integration.Tools {
		for _, s := range tool.Surfaces {
			if s.Key == string(kind) {
				return s.Name
			}
		}
	}
	return string(kind)
}
