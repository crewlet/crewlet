package runner

import (
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/phase"
)

// The submission schema's enum and the decoder's validation are ONE list, and
// this is what says so.
//
// Spelled separately they were one edit apart from disagreeing in the
// direction nothing catches. A schema that OFFERS a value the decoder REFUSES
// bounces a model that did exactly what it was told, and the model cannot see
// that the two lists differ — it only sees its submission rejected for naming
// a decision the tool it was handed said was available.
//
// Asserted in BOTH directions, because each catches a different edit: an
// addition that reached the schema and not the decoder, and one that reached
// the decoder and not the schema. The second is quieter — a reviewer is simply
// never offered a decision the engine would have honoured — which is why a
// one-directional check would have been the half that stayed green.
func TestTheReviewSchemaOffersExactlyWhatTheDecoderAccepts(t *testing.T) {
	t.Parallel()
	props, ok := reviewSchema["properties"].(map[string]any)
	if !ok {
		t.Fatal("the review schema has no properties")
	}
	decision, ok := props["decision"].(map[string]any)
	if !ok {
		t.Fatal("the review schema has no decision property")
	}
	offered, ok := decision["enum"].([]any)
	if !ok || len(offered) == 0 {
		t.Fatal("the review schema offers no decision at all")
	}

	for _, v := range offered {
		name, ok := v.(string)
		if !ok {
			t.Errorf("the schema offers a non-string decision %#v", v)
			continue
		}
		// Notes are supplied because self_iterate demands them; this is a
		// question about the decision vocabulary, not about the fields a
		// particular decision requires.
		got, err := decodeReview(map[string]any{"decision": name, "notes": "n"})
		if err != nil {
			t.Errorf("the schema offers %q but the decoder refuses it: %v", name, err)
			continue
		}
		if got.Decision.String() != name {
			t.Errorf("the decoder turned the offered %q into %q", name, got.Decision)
		}
	}

	for _, d := range phase.ReviewDecisions() {
		if !slices.Contains(offered, any(d.String())) {
			t.Errorf("the decoder accepts %q but the schema never offers it", d)
		}
	}
}

// A reviewer may not reach phase.Skipped, so the schema must not offer it.
//
// It is the ENGINE's own reading of a round nobody was waiting on, taken
// before the reviewer is called at all — see turn.Check — so a reviewer
// submitting it would mean the turn ran after deciding not to.
func TestTheReviewSchemaNeverOffersSkipped(t *testing.T) {
	t.Parallel()
	if slices.Contains(phase.ReviewDecisions(), phase.Skipped) {
		t.Error("phase.ReviewDecisions offers skipped, which is the engine's own verdict")
	}
	if _, err := decodeReview(map[string]any{"decision": "skipped"}); err == nil {
		t.Error("the decoder accepted skipped from a reviewer")
	}
}
