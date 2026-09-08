package integration_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/integration"
)

// EVERY SURFACE SAYS WHO KEEPS ITS ADDRESS CURRENT.
//
// The value decides whether a moved public base URL repairs itself on the
// next tick or sits there as the only warning an operator gets, so a new
// surface that falls through to a default nobody chose is the failure this
// pins: an operator-owned address classified as engine-owned reports a
// healthy integration over deliveries that go nowhere.
func TestEverySurfaceDeclaresWhoOwnsItsAddress(t *testing.T) {
	want := map[integration.Kind]integration.Ingress{
		integration.KindGitHub:     integration.IngressEngine,
		integration.KindGitLab:     integration.IngressEngine,
		integration.KindJira:       integration.IngressEngine,
		integration.KindConfluence: integration.IngressEngine,
		integration.KindSlack:      integration.IngressOperator,
		integration.KindDatadog:    integration.IngressOperator,
		integration.KindMattermost: integration.IngressNone,
		integration.KindAtlassian:  integration.IngressNone,
	}
	for _, kind := range integration.Kinds {
		expect, listed := want[kind]
		if !listed {
			t.Errorf("%s: a new surface has no declared ingress owner; decide "+
				"whether a pass registers its address, a person does, or it "+
				"has none, and say so here", kind)
			continue
		}
		if got := kind.Ingress(); got != expect {
			t.Errorf("%s: ingress is %q, want %q", kind, got, expect)
		}
	}
}

// A ROW WITHOUT A PHASE IS NOT A REPORT.
//
// The setup write stamps an address on a surface no pass converges, which
// leaves a row carrying that one fact. Read as a report it renders an empty
// phase as the integration's status, which is a dashboard card with no state
// on it at all.
func TestAnAddressOnlyRowHasNotBeenObserved(t *testing.T) {
	stamped := integration.State{
		Kind:     integration.KindSlack,
		Endpoint: "https://example.com",
	}
	if stamped.Observed() {
		t.Error("a row carrying only an address reported itself as observed")
	}
	reported := stamped
	reported.Report = integration.Report{Phase: integration.PhaseReady}
	if !reported.Observed() {
		t.Error("a row carrying a phase reported itself as unobserved")
	}
	// A DISCONNECT IS AN OBSERVATION TOO: the phase is derived rather than
	// stored for the first pass, and a card must not go blank between the
	// press and that pass.
	going := stamped
	going.Disconnecting = true
	if !going.Observed() {
		t.Error("a surface being torn down reported itself as unobserved")
	}
}
