package runner

import (
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/kept"
)

// The submission schema's enum and the decoder's validation are ONE list. A
// schema offering a value the decoder refuses bounces a model that did as told,
// unable to see why. Asserted in BOTH directions: an addition reaching only the
// decoder is the quieter one (a reviewer is never offered a decision the
// engine would honour), the half a one-way check leaves green.
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
		// Notes because self_iterate demands them: this tests the vocabulary,
		// not a decision's required fields.
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

// A reviewer may not reach phase.Skipped, so the schema must not offer it: it
// is the engine's own reading, taken before any reviewer runs (turn.Check).
func TestTheReviewSchemaNeverOffersSkipped(t *testing.T) {
	t.Parallel()
	if slices.Contains(phase.ReviewDecisions(), phase.Skipped) {
		t.Error("phase.ReviewDecisions offers skipped, which is the engine's own verdict")
	}
	if _, err := decodeReview(map[string]any{"decision": "skipped"}); err == nil {
		t.Error("the decoder accepted skipped from a reviewer")
	}
}

// A REVIEW'S WORDS PAST THEIR BOUNDS ARE REFUSED WHERE THEY ARE MADE, naming
// the field, its size and its bound, while the reviewer can still shorten
// them. The final artifact is the turn's answer, held to a run's own report's
// bound; the notes and the account of what landed are the next brief, held to
// an executor's whole submission's. Words within bounds are taken as they are.
//
// Mutation: take the words whatever their size, and every reader of the
// answer holds a different cut of it, or none.
func TestAReviewsWordsPastTheirBoundsAreRefusedWhereTheyAreMade(t *testing.T) {
	t.Parallel()
	for field, bound := range map[string]int{"final_artifact": finalArtifactBytes, "notes": reviewProseBytes,
		"completed_work": reviewProseBytes} {
		args := map[string]any{"decision": "self_iterate", "notes": "fix the title", field: strings.Repeat("a",
			bound+1)}
		if _, err := decodeReview(args); err == nil || !strings.Contains(err.Error(), field+" is ") ||
			!strings.Contains(err.Error(), "kept up to "+kept.Size(bound)) {
			t.Errorf("a review whose %s is past its %d bytes was answered %v, want it refused by name and bound",
				field, bound, err)
		}
		args[field] = strings.Repeat("a", bound)
		if _, err := decodeReview(args); err != nil {
			t.Errorf("a review whose %s is at its bound was refused: %v", field, err)
		}
	}
}
