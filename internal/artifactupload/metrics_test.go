package artifactupload_test

import (
	"fmt"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/artifactupload"
)

// A COLLISION AND A FULL SET ARE COUNTED APART, each under the answer the
// artifacts API gives it, because the refusals counter exists so a dashboard
// can tell one cause from another: a label another version holds is
// `label_taken`, every kept label in use is `labels_full`, and a source address
// another artifact holds is `source_taken` — it was counted `invalid`, beside
// the requests a client sent wrong. Each reason is one the closed set declares.
//
// Mutation: count ErrLabelsFull as `label_taken`, or ErrSourceTaken as
// `invalid` again, and this goes red.
func TestACollisionIsCountedUnderItsOwnReason(t *testing.T) {
	t.Parallel()
	for err, want := range map[error]string{
		fmt.Errorf("%w: version 3 is labelled \"v1\"", artifactsdomain.ErrLabelTaken): "label_taken",
		fmt.Errorf("%w: it keeps 100", artifactsdomain.ErrLabelsFull):                 "labels_full",
		artifactsdomain.ErrSourceTaken:                                                "source_taken",
	} {
		got := artifactupload.ReasonOf(err)
		if got != want {
			t.Errorf("%v is counted %q, want %q", err, got, want)
		}
		if !slices.Contains(artifactupload.Refusals, got) {
			t.Errorf("%q is a reason the closed set does not declare", got)
		}
	}
}
