package builtin_test

import (
	"errors"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/caveat"
	"github.com/crewlet/crewlet/internal/provenance"
)

// A RECEIPT THAT ECHOES AN ASK STILL CARRIES THE WRITE'S CAVEATS. A
// create_work_item receipt echoing the caller's ask is fenced as the caller's
// own words (ADR-0040), and its caveats are answered for a model and a person
// from the same values (ADR-0060); one create can be both, and the fenced path
// must not drop the `warnings`, `dependencies_failed` or a person's caveats.
//
// Mutation: answer ownReceipt's fenced path with the foreign read alone, and
// the model's warnings and both of a person's caveats go red.
func TestAReceiptThatEchoesAnAskCarriesItsCaveats(t *testing.T) {
	t.Parallel()
	turn, b := readerTurn(t)
	trk := newFakeTracker()
	near := caveat.TagNearDuplicate("ENG", "regression", []string{"regressions"})
	trk.tagWarnings = []caveat.Caveat{near}
	trk.dependErr = errors.New("the dependency's record could not be appended")
	got := callIn(t, labelSurface(t, trk, false), turn, builtin.CreateWorkItemTool,
		map[string]any{
			"title": "Which vendor do we take?", "project": "ENG", "ask": "agent-ceo",
			"decision": map[string]any{"question": "Which vendor?", "role": "approver",
				"options": []any{
					map[string]any{"id": "acme", "label": "Acme"},
					map[string]any{"id": "bolt", "label": "Bolt"},
				},
				"recommended": "acme", "rationale": "Acme already holds our data."},
			"labels": []any{"regression"}, "labels_create_missing": true,
			"waiting_on": []any{"ENG-2"},
		})
	_, answer := fencesIn(t, b, got.Output)
	if _, echoed := answer["decision"]; !echoed {
		t.Fatalf("the receipt echoes no decision, so it never took the fenced path: %v",
			answer)
	}
	// THE ECHOED ASK IS THE CALLER'S OWN.
	assertEnvelope(t, answer, provenance.TrustOwn, 1)

	// THE MODEL'S WORDS, under the keys it reads them from.
	warnings, _ := answer[builtin.WarningsKey].([]any)
	if !slices.Contains(warnings, any(near.Agent())) {
		t.Errorf("the model is told %v, want the near-duplicate's own words", warnings)
	}
	if said, _ := answer[builtin.DependenciesFailedKey].(string); said == "" {
		t.Errorf("the model is not told the dependencies did not land: %v", answer)
	}

	// AND A PERSON'S, beside them.
	if !slices.Contains(got.Caveats, near) {
		t.Errorf("the result carries %v, want the near-duplicate for a person's surface",
			got.Caveats)
	}
	if !slices.ContainsFunc(got.Caveats, func(c caveat.Caveat) bool {
		return c.Code() == caveat.CodeDependenciesNotRecorded
	}) {
		t.Errorf("the result carries %v, want the dependencies not recorded", got.Caveats)
	}
}
